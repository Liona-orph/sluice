// Package server is the HTTP surface: an OpenAI-compatible chat completions
// endpoint, Sluice's own operational endpoints, and the embedded dashboard.
//
// Two rules shape it.
//
// The compatible endpoint is compatible or it errors. Anything Sluice cannot
// serve faithfully -- n>1, logprobs, a tool_choice naming a function -- is a
// 400 with a message saying so, never a quiet approximation. A gateway that
// silently changes the meaning of a request is worse than one that refuses it,
// because the refusal is noticed in five minutes and the change is noticed in a
// quarterly review.
//
// Sluice's own information goes in headers and in its own endpoints, never
// mixed into the OpenAI response body. A strict client decoding into a
// generated struct must not meet a field its schema does not have.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sluice-gw/sluice/internal/gateway"
	"github.com/sluice-gw/sluice/pkg/llm"
)

// Options configures a Server.
type Options struct {
	Gateway *gateway.Gateway
	Logger  *slog.Logger
	// Registry is gathered by the metrics endpoint. Nil disables it.
	Registry *prometheus.Registry
	// Addr, timeouts and limits come from the config's server section.
	Addr              string
	ReadHeaderTimeout time.Duration
	RequestTimeout    time.Duration
	ShutdownGrace     time.Duration
	MaxRequestBytes   int64
	MetricsPath       string
	Dashboard         bool
	// Version is reported by /healthz and shown on the dashboard.
	Version string
	// Now is the clock, for deterministic timestamps in tests.
	Now func() time.Time
}

// Server wraps an http.Server with Sluice's handlers.
type Server struct {
	opts Options
	log  *slog.Logger
	http *http.Server
	mux  *http.ServeMux
	now  func() time.Time
}

// New builds the server and its routes.
func New(opts Options) (*Server, error) {
	if opts.Gateway == nil {
		return nil, errors.New("server: no gateway")
	}
	s := &Server{opts: opts, log: opts.Logger, now: opts.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.opts.MaxRequestBytes <= 0 {
		s.opts.MaxRequestBytes = 4 << 20
	}

	s.mux = http.NewServeMux()
	s.routes()

	s.http = &http.Server{
		Addr:    opts.Addr,
		Handler: s.mux,
		// ReadHeaderTimeout is the one timeout that must be set on any public
		// listener; without it a client can hold a connection open forever by
		// sending headers one byte at a time.
		ReadHeaderTimeout: opts.ReadHeaderTimeout,
		// WriteTimeout is deliberately not set. It is a deadline on the whole
		// response, and a streaming completion legitimately takes minutes; a
		// WriteTimeout would sever long streams at an arbitrary point with no
		// error the client could distinguish from a network failure. The
		// per-request timeout below bounds the work instead, and it can do so
		// with a context the handler can observe.
		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("GET /v1/stats", s.handleStats)
	s.mux.HandleFunc("GET /v1/targets", s.handleTargets)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	if s.opts.MetricsPath != "" && s.opts.Registry != nil {
		s.mux.Handle("GET "+s.opts.MetricsPath, promhttp.HandlerFor(s.opts.Registry, promhttp.HandlerOpts{}))
	}
	if s.opts.Dashboard {
		s.mux.HandleFunc("GET /", s.handleDashboard)
	}
}

// Handler exposes the mux, so that a test can drive the server through
// httptest.NewServer without binding a port in production code.
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe starts the listener.
func (s *Server) ListenAndServe() error {
	s.log.Info("listening", "addr", s.opts.Addr, "version", s.opts.Version)
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// Serve serves on an existing listener, for tests and for socket activation.
func (s *Server) Serve(l net.Listener) error {
	if err := s.http.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

// Shutdown stops accepting and waits for in-flight requests, up to the grace
// period.
//
// A streaming completion in flight is a request that has already been paid for
// upstream, so cutting it off wastes the money and gives the client nothing.
// The grace period is how long that consideration outranks the deploy.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.opts.ShutdownGrace > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.ShutdownGrace)
		defer cancel()
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}
	return nil
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.opts.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.opts.RequestTimeout)
		defer cancel()
	}

	var req chatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.opts.MaxRequestBytes))
	// Unknown fields are rejected. See openai.go for why: a misspelled
	// parameter that is silently dropped changes the model's behaviour with no
	// signal at all.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, decodeError(err))
		return
	}

	llmReq, err := req.toLLM()
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	in := gateway.Request{
		Secret:     bearerToken(r),
		LLM:        llmReq,
		Stream:     req.Stream,
		ClientAddr: clientAddr(r),
	}
	if ignored := req.ignoredFields(); len(ignored) > 0 {
		w.Header().Set("X-Sluice-Ignored-Fields", strings.Join(ignored, ","))
	}

	if req.Stream {
		s.streamCompletion(ctx, w, r, in, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
		return
	}
	s.bufferedCompletion(ctx, w, r, in)
}

func (s *Server) bufferedCompletion(ctx context.Context, w http.ResponseWriter, r *http.Request, in gateway.Request) {
	res, err := s.opts.Gateway.Complete(ctx, in)
	if err != nil {
		s.setResultHeaders(w, res)
		s.writeError(w, r, err)
		return
	}
	s.setResultHeaders(w, res)

	content := res.Response.Message.Content
	out := chatResponse{
		ID:      res.Response.ID,
		Object:  "chat.completion",
		Created: createdOf(res.Response, s.now()),
		Model:   res.Response.Model,
		Choices: []chatChoice{{
			Index: 0,
			Message: &chatOutMessage{
				Role:      string(llm.RoleAssistant),
				Content:   &content,
				ToolCalls: toolCallsOf(res.Response.Message.ToolCalls, false),
			},
			FinishReason: finishReasonOf(res.Response.FinishReason),
		}},
		Usage:             usageOf(res.Response.Usage),
		SystemFingerprint: res.Response.Provider,
	}
	s.writeJSON(w, r, http.StatusOK, out)
}

// streamCompletion writes an SSE response.
//
// The contract, in the order it matters:
//
//   - Nothing is written until the gateway has committed to a stream, so a
//     rejection is still a JSON error with the right status rather than a 200
//     containing an error event. That is why gateway.Stream does its
//     pre-provider work eagerly.
//   - Every event is flushed immediately. An SSE response that is buffered is
//     not a stream; it is a slow buffered response with extra syntax.
//   - The client's disconnect cancels the request context, the range loop
//     stops, and the gateway's settle path still records the usage and the
//     audit entry -- the tokens were generated and paid for whether or not
//     anyone read them.
//   - The terminating "data: [DONE]" is written even after an error event,
//     because that is what OpenAI does and clients wait for it.
func (s *Server) streamCompletion(ctx context.Context, w http.ResponseWriter, r *http.Request, in gateway.Request, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, r, &gateway.Error{
			Status: http.StatusInternalServerError, Kind: gateway.KindInternal,
			Message: "the HTTP server does not support streaming",
		})
		return
	}

	res, err := s.opts.Gateway.Stream(ctx, in)
	if err != nil {
		if res.Result != nil {
			s.setResultHeaders(w, *res.Result)
		}
		s.writeError(w, r, err)
		return
	}
	s.setResultHeaders(w, *res.Result)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Tells nginx and friends not to buffer the response into uselessness.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	created := createdOf(res.Result.Response, s.now())
	first := true
	var streamErr error

	for chunk, cerr := range res.Chunks {
		if cerr != nil {
			streamErr = cerr
			break
		}
		ev := chatResponse{
			ID:                chunk.ID,
			Object:            "chat.completion.chunk",
			Created:           created,
			Model:             chunk.Model,
			Choices:           []chatChoice{{Index: 0, Delta: &chatOutMessage{}}},
			SystemFingerprint: chunk.Provider,
		}
		if first {
			ev.Choices[0].Delta.Role = string(llm.RoleAssistant)
			first = false
		}
		content := chunk.Delta.Content
		ev.Choices[0].Delta.Content = &content
		if len(chunk.Delta.ToolCalls) > 0 {
			ev.Choices[0].Delta.ToolCalls = streamToolCalls(chunk.Delta.ToolCalls)
		}
		ev.Choices[0].FinishReason = finishReasonOf(chunk.FinishReason)
		if chunk.Usage != nil && includeUsage {
			ev.Usage = usageOf(*chunk.Usage)
		}
		if !s.writeEvent(w, flusher, ev) {
			// The client is gone. Abandoning the range loop is what tells the
			// gateway to settle; there is no goroutine to stop because the
			// sequence has been running on this one.
			return
		}
	}

	if streamErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			// The client hung up. There is nobody to send an error event to and
			// nothing went wrong, so this is a debug line rather than a warning.
			s.log.DebugContext(ctx, "client disconnected mid-stream", "request_id", res.Result.AuditID)
			return
		}
		gerr := gateway.FromProvider(streamErr)
		s.log.WarnContext(ctx, "stream failed after the response began",
			"error", gerr.Message, "code", gerr.Code, "request_id", res.Result.AuditID)
		// The status line is long gone, so the only way to tell the client is
		// an event. Clients that decode the OpenAI schema see an object with an
		// "error" member, which is what OpenAI itself emits in this case.
		if !s.writeRaw(w, flusher, errorBodyOf(gerr)) {
			return
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func streamToolCalls(deltas []llm.ToolCallDelta) []chatOutToolCall {
	out := make([]chatOutToolCall, 0, len(deltas))
	for _, d := range deltas {
		idx := d.Index
		c := chatOutToolCall{Index: &idx, ID: d.ID}
		if d.Name != "" {
			c.Type = "function"
		}
		c.Function.Name = d.Name
		c.Function.Arguments = d.ArgumentsDelta
		out = append(out, c)
	}
	return out
}

// writeEvent writes one SSE frame and flushes. It reports whether the write
// succeeded; a failure means the client hung up.
func (s *Server) writeEvent(w http.ResponseWriter, f http.Flusher, ev chatResponse) bool {
	return s.writeRaw(w, f, ev)
}

func (s *Server) writeRaw(w http.ResponseWriter, f http.Flusher, v any) bool {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Error("failed to encode a stream event", "error", err)
		return false
	}
	if _, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n')); err != nil {
		return false
	}
	f.Flush()
	return true
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, err := s.opts.Gateway.Authenticate(bearerToken(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	aliases := s.opts.Gateway.Router().Aliases()
	out := modelList{Object: "list", Data: make([]modelInfo, 0, len(aliases))}
	created := s.now().Unix()
	for _, a := range aliases {
		out.Data = append(out.Data, modelInfo{ID: a, Object: "model", Created: created, OwnedBy: "sluice"})
	}
	s.writeJSON(w, r, http.StatusOK, out)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if _, err := s.opts.Gateway.Authenticate(bearerToken(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, s.opts.Gateway.Stats())
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	if _, err := s.opts.Gateway.Authenticate(bearerToken(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, s.opts.Gateway.Router().Status())
}

// handleHealth reports that the process is alive. It is unauthenticated,
// because a liveness probe that needs a credential is a liveness probe that
// fails when the credential rotates.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"status": "ok", "version": s.opts.Version,
	})
}

// handleReady reports that the process can serve. It reports 503 when every
// target of every route has an open circuit, which is the one condition under
// which taking this instance out of a load balancer's rotation would help.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	targets := s.opts.Gateway.Router().Status()
	healthy := 0
	for _, t := range targets {
		if t.Breaker.State != "open" {
			healthy++
		}
	}
	status := http.StatusOK
	if len(targets) > 0 && healthy == 0 {
		status = http.StatusServiceUnavailable
	}
	s.writeJSON(w, r, status, map[string]any{
		"status":          map[bool]string{true: "ready", false: "unavailable"}[status == http.StatusOK],
		"targets_total":   len(targets),
		"targets_healthy": healthy,
	})
}

// --- helpers ----------------------------------------------------------------

// setResultHeaders publishes Sluice's own metadata about how a request was
// served. Headers rather than body fields, so that the OpenAI response schema
// stays exactly what an OpenAI client expects.
func (s *Server) setResultHeaders(w http.ResponseWriter, res gateway.Result) {
	h := w.Header()
	if res.AuditID != "" {
		h.Set("X-Sluice-Request-Id", res.AuditID)
	}
	if res.Response.Provider != "" {
		h.Set("X-Sluice-Provider", res.Response.Provider)
	}
	if res.Response.Model != "" {
		h.Set("X-Sluice-Model", res.Response.Model)
	}
	if res.Cache.Hit {
		h.Set("X-Sluice-Cache", string(res.Cache.Kind))
		if res.Cache.Kind == "semantic" {
			h.Set("X-Sluice-Cache-Similarity", strconv.FormatFloat(res.Cache.Similarity, 'f', 4, 64))
		}
	} else {
		h.Set("X-Sluice-Cache", "miss")
	}
	// Cost as a decimal string rather than a float: it is money, and a client
	// parsing it should decide its own precision.
	h.Set("X-Sluice-Cost-Usd", strconv.FormatFloat(res.Cost.Dollars(), 'f', 9, 64))
	if res.Degraded {
		h.Set("X-Sluice-Degraded", "true")
	}
	if n := len(res.Redactions); n > 0 {
		h.Set("X-Sluice-Redactions", redactionHeader(res.Redactions))
	}
	if res.Outcome.Failovers > 0 {
		h.Set("X-Sluice-Failovers", strconv.Itoa(res.Outcome.Failovers))
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already written, so there is nothing to tell the
		// client. Logging it is all that is left.
		s.log.WarnContext(r.Context(), "failed to write response body", "error", err, "path", r.URL.Path)
	}
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	gerr := gateway.FromProvider(err)
	if gerr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(gerr.RetryAfter.Round(time.Second)/time.Second)))
	}
	if gerr.Status >= 500 {
		s.log.ErrorContext(r.Context(), "request failed",
			"status", gerr.Status, "kind", gerr.Kind, "error", gerr.Message, "path", r.URL.Path)
	}
	s.writeJSON(w, r, gerr.Status, errorBodyOf(gerr))
}

// decodeError turns a JSON decoding failure into a client-facing 400.
//
// The decoder's own messages are surfaced because they name the offending
// field, which is the only thing that makes a schema error fixable. The
// exception is the body-size limit, which gets a message of its own since the
// decoder's is about an unexpected EOF and points nowhere useful.
func decodeError(err error) error {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return &gateway.Error{
			Status: http.StatusRequestEntityTooLarge, Kind: gateway.KindInvalidRequest,
			Message: fmt.Sprintf("request body exceeds the %d byte limit", maxErr.Limit),
		}
	}
	return gateway.Invalid("could not decode the request body: %v", err)
}

// bearerToken extracts the credential from the Authorization header, accepting
// the api-key header some Azure-flavoured clients send.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, found := strings.CutPrefix(h, "Bearer "); found {
			return strings.TrimSpace(after)
		}
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(r.Header.Get("api-key"))
}

func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func createdOf(resp llm.Response, fallback time.Time) int64 {
	if !resp.Created.IsZero() {
		return resp.Created.Unix()
	}
	return fallback.Unix()
}
