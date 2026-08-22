package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/internal/audit"
	"github.com/Liona-orph/sluice/internal/config"
	"github.com/Liona-orph/sluice/internal/gateway"
	"github.com/Liona-orph/sluice/internal/leaktest"
	"github.com/Liona-orph/sluice/internal/telemetry"
)

const demoSecret = "sk-sluice-local-demo"

type harness struct {
	*Server
	http  *httptest.Server
	audit *audit.Memory
	reg   *prometheus.Registry
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	for _, m := range mutate {
		m(&cfg)
	}
	mem := audit.NewMemory(100)
	reg := prometheus.NewPedanticRegistry()
	metrics, err := telemetry.NewMetrics(reg)
	require.NoError(t, err)

	gw, err := gateway.New(gateway.Options{
		Config: cfg, Metrics: metrics, Auditor: mem,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	srv, err := New(Options{
		Gateway: gw, Registry: reg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MetricsPath: "/metrics", Dashboard: true, Version: "test",
		MaxRequestBytes: 1 << 20, RequestTimeout: 10 * time.Second,
	})
	require.NoError(t, err)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{Server: srv, http: ts, audit: mem, reg: reg}
}

func (h *harness) post(t *testing.T, body string, opts ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		h.http.URL+"/v1/chat/completions", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+demoSecret)
	for _, o := range opts {
		o(req)
	}
	resp, err := h.http.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) get(t *testing.T, path string, opts ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+path, http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+demoSecret)
	for _, o := range opts {
		o(req)
	}
	resp, err := h.http.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func noAuth(r *http.Request) { r.Header.Del("Authorization") }

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestChatCompletionsIsOpenAIShaped(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","messages":[{"role":"user","content":"hello"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	out := decode[chatResponse](t, resp)
	assert.Equal(t, "chat.completion", out.Object)
	require.Len(t, out.Choices, 1)
	require.NotNil(t, out.Choices[0].Message)
	assert.Equal(t, "assistant", out.Choices[0].Message.Role)
	require.NotNil(t, out.Choices[0].Message.Content)
	assert.NotEmpty(t, *out.Choices[0].Message.Content)
	require.NotNil(t, out.Choices[0].FinishReason)
	assert.Equal(t, "stop", *out.Choices[0].FinishReason)
	require.NotNil(t, out.Usage)
	assert.Equal(t, out.Usage.PromptTokens+out.Usage.CompletionTokens, out.Usage.TotalTokens)
	assert.Equal(t, "local-primary", out.SystemFingerprint)
}

func TestSluiceMetadataIsInHeadersNotTheBody(t *testing.T) {
	// A strict client decoding the OpenAI schema must not meet a field its
	// generated struct does not have.
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","messages":[{"role":"user","content":"mail alice@example.com"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.NotEmpty(t, resp.Header.Get("X-Sluice-Request-Id"))
	assert.Equal(t, "miss", resp.Header.Get("X-Sluice-Cache"))
	assert.Equal(t, "local-primary", resp.Header.Get("X-Sluice-Provider"))
	assert.Equal(t, "email=1", resp.Header.Get("X-Sluice-Redactions"))
	assert.NotEmpty(t, resp.Header.Get("X-Sluice-Cost-Usd"))

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	for key := range body {
		assert.NotContains(t, key, "sluice", "no Sluice-specific fields leak into the OpenAI body")
	}
}

func TestCacheHitIsReportedInAHeader(t *testing.T) {
	h := newHarness(t)
	body := `{"model":"sluice-demo","messages":[{"role":"user","content":"same question"}]}`
	require.Equal(t, http.StatusOK, h.post(t, body).StatusCode)
	resp := h.post(t, body)
	assert.Equal(t, "exact", resp.Header.Get("X-Sluice-Cache"))
}

func TestAuthenticationErrors(t *testing.T) {
	h := newHarness(t)
	body := `{"model":"sluice-demo","messages":[{"role":"user","content":"hi"}]}`

	noKey := h.post(t, body, noAuth)
	assert.Equal(t, http.StatusUnauthorized, noKey.StatusCode)
	e := decode[errorBody](t, noKey)
	assert.Equal(t, "authentication_error", e.Error.Type)

	badKey := h.post(t, body, func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") })
	assert.Equal(t, http.StatusUnauthorized, badKey.StatusCode)
}

func TestAzureStyleApiKeyHeaderIsAccepted(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","messages":[{"role":"user","content":"hi"}]}`,
		func(r *http.Request) {
			noAuth(r)
			r.Header.Set("api-key", demoSecret)
		})
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestUnsupportedRequestsAreRefusedRatherThanApproximated(t *testing.T) {
	h := newHarness(t)
	cases := map[string]string{
		"n above one":          `{"model":"sluice-demo","n":3,"messages":[{"role":"user","content":"hi"}]}`,
		"logprobs":             `{"model":"sluice-demo","logprobs":true,"messages":[{"role":"user","content":"hi"}]}`,
		"named tool choice":    `{"model":"sluice-demo","tool_choice":{"type":"function","function":{"name":"f"}},"messages":[{"role":"user","content":"hi"}]}`,
		"legacy functions":     `{"model":"sluice-demo","functions":[],"messages":[{"role":"user","content":"hi"}]}`,
		"image content part":   `{"model":"sluice-demo","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
		"unknown field":        `{"model":"sluice-demo","temprature":0.5,"messages":[{"role":"user","content":"hi"}]}`,
		"no messages":          `{"model":"sluice-demo","messages":[]}`,
		"bad role":             `{"model":"sluice-demo","messages":[{"role":"wizard","content":"hi"}]}`,
		"tool without call id": `{"model":"sluice-demo","messages":[{"role":"tool","content":"result"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := h.post(t, body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			e := decode[errorBody](t, resp)
			assert.Equal(t, "invalid_request_error", e.Error.Type)
			assert.NotEmpty(t, e.Error.Message)
		})
	}
}

func TestIgnoredFieldsAreReported(t *testing.T) {
	// Silently dropping a sampling parameter changes the output with no signal.
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","frequency_penalty":0.5,"presence_penalty":0.2,
		"messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "frequency_penalty,presence_penalty", resp.Header.Get("X-Sluice-Ignored-Fields"))
}

func TestStopAcceptsAStringOrAnArray(t *testing.T) {
	h := newHarness(t)
	for _, body := range []string{
		`{"model":"sluice-demo","stop":"END","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"sluice-demo","stop":["END","STOP"],"messages":[{"role":"user","content":"hi"}]}`,
	} {
		assert.Equal(t, http.StatusOK, h.post(t, body).StatusCode)
	}
}

func TestContentPartsArrayIsConcatenated(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","messages":[{"role":"user",
		"content":[{"type":"text","text":"one "},{"type":"text","text":"two"}]}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	records := h.audit.Recent(1)
	require.Len(t, records, 1)
	assert.Equal(t, "one two", records[0].Prompt[0].Content)
}

func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Server.MaxRequestBytes = 256 })
	h.Server.opts.MaxRequestBytes = 256
	big := strings.Repeat("x", 4096)
	resp := h.post(t, `{"model":"sluice-demo","messages":[{"role":"user","content":"`+big+`"}]}`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestRateLimitSetsRetryAfter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Keys[0].RateLimit = config.RateLimit{RequestsPerMinute: 1, Burst: 1}
	})
	body := `{"model":"sluice-demo","messages":[{"role":"user","content":"hi"}]}`
	require.Equal(t, http.StatusOK, h.post(t, body).StatusCode)
	resp := h.post(t, body)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	e := decode[errorBody](t, resp)
	assert.Equal(t, "rate_limit_error", e.Error.Type)
}

func TestUnknownModelIs404WithTheOpenAIMessage(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, `{"model":"gpt-9","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	e := decode[errorBody](t, resp)
	assert.Contains(t, e.Error.Message, "does not exist")
}

// --- streaming --------------------------------------------------------------

// sseEvents reads an SSE body into its data payloads.
func sseEvents(t *testing.T, r io.Reader) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			out = append(out, after)
		}
	}
	require.NoError(t, sc.Err())
	return out
}

func TestStreamingProducesSSE(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"call bob@corp.io about it"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	events := sseEvents(t, resp.Body)
	require.NotEmpty(t, events)
	assert.Equal(t, "[DONE]", events[len(events)-1], "clients wait for the terminator")

	var text strings.Builder
	var sawUsage, sawFinish bool
	for _, ev := range events[:len(events)-1] {
		var chunk chatResponse
		require.NoError(t, json.Unmarshal([]byte(ev), &chunk))
		assert.Equal(t, "chat.completion.chunk", chunk.Object)
		require.Len(t, chunk.Choices, 1)
		if c := chunk.Choices[0].Delta.Content; c != nil {
			text.WriteString(*c)
		}
		if chunk.Choices[0].FinishReason != nil {
			sawFinish = true
		}
		if chunk.Usage != nil {
			sawUsage = true
		}
	}
	assert.True(t, sawFinish)
	assert.True(t, sawUsage, "stream_options.include_usage was requested")
	assert.Contains(t, text.String(), "bob@corp.io",
		"un-redaction spans the provider's chunk boundaries end to end")
}

func TestStreamingFirstChunkCarriesTheRole(t *testing.T) {
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	events := sseEvents(t, resp.Body)
	require.NotEmpty(t, events)
	var first chatResponse
	require.NoError(t, json.Unmarshal([]byte(events[0]), &first))
	assert.Equal(t, "assistant", first.Choices[0].Delta.Role)
}

func TestStreamingRejectionIsAJSONErrorNotAnEventStream(t *testing.T) {
	// The reason gateway.Stream does its pre-provider work eagerly.
	h := newHarness(t)
	resp := h.post(t, `{"model":"sluice-demo","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") })
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	e := decode[errorBody](t, resp)
	assert.Equal(t, "authentication_error", e.Error.Type)
}

func TestClientDisconnectMidStreamIsStillAudited(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t, func(c *config.Config) {
		// Slow enough that the client can disconnect mid-stream deterministically.
		c.Providers[0].Latency = config.Latency{PerToken: config.Duration(2 * time.Millisecond)}
		c.Providers[1].Latency = config.Latency{PerToken: config.Duration(2 * time.Millisecond)}
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.http.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"sluice-demo","stream":true,"messages":[{"role":"user","content":"a long answer please"}]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+demoSecret)

	resp, err := h.http.Client().Do(req)
	require.NoError(t, err)
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()

	require.Eventually(t, func() bool { return len(h.audit.Records()) == 1 }, 2*time.Second, 5*time.Millisecond,
		"a disconnected stream still generated tokens, and they were still paid for")
}

// --- other endpoints --------------------------------------------------------

func TestModelsListsTheRouteAliases(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/v1/models")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	out := decode[modelList](t, resp)
	assert.Equal(t, "list", out.Object)
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	assert.Contains(t, ids, "sluice-demo")
}

func TestStatsAndTargetsRequireAKey(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/v1/stats", "/v1/targets"} {
		assert.Equal(t, http.StatusUnauthorized, h.get(t, path, noAuth).StatusCode, path)
		assert.Equal(t, http.StatusOK, h.get(t, path).StatusCode, path)
	}
}

func TestHealthAndReady(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, http.StatusOK, h.get(t, "/healthz", noAuth).StatusCode,
		"a liveness probe must not need a credential")
	assert.Equal(t, http.StatusOK, h.get(t, "/readyz", noAuth).StatusCode)
}

func TestDashboardIsSelfContained(t *testing.T) {
	h := newHarness(t)
	resp := h.get(t, "/", noAuth)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	page := string(body)
	assert.Contains(t, page, "<!doctype html>")
	assert.NotContains(t, page, "https://", "no CDN: a security product must not fetch code from a third party")
	assert.NotContains(t, page, "<script src", "no external script tags")
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'none'")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
}

func TestUnknownPathIs404(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, http.StatusNotFound, h.get(t, "/nope").StatusCode)
}

func TestMetricsEndpointExposesSluiceMetrics(t *testing.T) {
	h := newHarness(t)
	require.Equal(t, http.StatusOK,
		h.post(t, `{"model":"sluice-demo","messages":[{"role":"user","content":"hi"}]}`).StatusCode)

	resp := h.get(t, "/metrics", noAuth)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "sluice_requests_total")
	assert.NotContains(t, string(body), demoSecret, "a credential must never reach the metrics endpoint")
}

func TestGracefulShutdown(t *testing.T) {
	h := newHarness(t)
	h.Server.opts.ShutdownGrace = time.Second
	require.NoError(t, h.Server.Shutdown(context.Background()))
}

func TestBearerTokenParsing(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"Bearer  abc": "abc",
		"abc":         "abc",
		"":            "",
	}
	for header, want := range cases {
		r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://x", http.NoBody)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		assert.Equal(t, want, bearerToken(r), "header %q", header)
	}
}
