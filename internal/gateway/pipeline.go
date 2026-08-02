package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"iter"
	"time"

	"github.com/sluice-gw/sluice/internal/audit"
	"github.com/sluice-gw/sluice/internal/cache"
	"github.com/sluice-gw/sluice/internal/redact"
	"github.com/sluice-gw/sluice/internal/router"
	"github.com/sluice-gw/sluice/pkg/llm"
)

// Complete runs the buffered pipeline.
//
// The stage order is the one documented on the package, and it is written out
// linearly on purpose: a reader should be able to see every decision made about
// a request without following a middleware chain.
func (g *Gateway) Complete(ctx context.Context, in Request) (Result, error) {
	start := g.now()
	res := Result{Alias: in.LLM.Model}
	rec := audit.Record{ID: newID(), Time: start.UTC(), RequestedModel: in.LLM.Model}

	principal, prepared, err := g.prepare(ctx, &in, &res, &rec)
	if err != nil {
		g.finish(ctx, &res, &rec, start, err)
		return res, err
	}

	// Stage 5: cache. The lookup uses the redacted request, so the key space is
	// the redacted one and nothing personal is ever a cache key.
	hit := g.CacheLookup(ctx, prepared.request)
	res.Cache = hit

	var (
		resp    llm.Response
		outcome router.Outcome
	)
	if hit.Hit {
		resp = hit.Response
	} else {
		// Stage 6/7: route and call.
		resp, outcome, err = g.router.Complete(ctx, prepared.request)
		if err != nil {
			gerr := FromProvider(err)
			res.Outcome = outcome
			g.finish(ctx, &res, &rec, start, gerr)
			return res, gerr
		}
		g.CacheStore(ctx, prepared.request, resp)
	}
	res.Outcome = outcome

	// Stage 8: un-redact. The cache holds the redacted response; the caller's
	// own vault decides what each placeholder means. The pre-restore response
	// is kept because that is the one the audit record stores: restoring for
	// the log would put the original values back into the file that outlives
	// everything.
	redacted := resp
	if prepared.vault != nil && g.redactor != nil {
		resp = g.redactor.RestoreResponse(resp, prepared.vault)
	}
	res.Response = resp
	res.Cost = g.costOf(hit, resp)

	// Stage 9: account.
	g.Account(principal, prepared.estimate, resp.Usage, res.Cost, resp.Provider, resp.Model)
	g.latency.observe(providerKey(resp, hit), g.now().Sub(start))

	g.fillRecord(&rec, res, redacted)
	g.finish(ctx, &res, &rec, start, nil)
	return res, nil
}

// StreamResult is the result of a streaming request. The response fields are
// only complete once the sequence has been fully consumed.
type StreamResult struct {
	*Result
	// Chunks is the sequence to forward to the client.
	Chunks iter.Seq2[llm.Chunk, error]
}

// Stream runs the streaming pipeline.
//
// The stages before the provider call are identical to Complete's and run
// eagerly, so a rejection still produces a JSON error rather than an event
// stream containing an error. Everything after the provider call happens as the
// stream is consumed: un-redaction across chunk boundaries, usage accounting
// when the terminating chunk arrives, and the audit record when the sequence
// ends -- including when it ends because the client disconnected, which is the
// case that would otherwise leave no record of a request that was paid for.
//
// The sequence runs entirely on the consumer's goroutine. Nothing here starts a
// goroutine, so abandoning the stream leaks nothing; TestStreamNoGoroutineLeak
// abandons one mid-flight and checks.
func (g *Gateway) Stream(ctx context.Context, in Request) (StreamResult, error) {
	start := g.now()
	res := &Result{Alias: in.LLM.Model}
	rec := audit.Record{ID: newID(), Time: start.UTC(), RequestedModel: in.LLM.Model, Stream: true}

	principal, prepared, err := g.prepare(ctx, &in, res, &rec)
	if err != nil {
		g.finish(ctx, res, &rec, start, err)
		return StreamResult{Result: res}, err
	}

	hit := g.CacheLookup(ctx, prepared.request)
	res.Cache = hit

	var (
		upstream iter.Seq2[llm.Chunk, error]
		outcome  *router.Outcome
	)
	if hit.Hit {
		// A cache hit is delivered as one chunk rather than re-chunked into an
		// imitation of the original stream; see llm.StreamOf for why fabricated
		// timing would be worse than a single frame.
		upstream = llm.StreamOf(hit.Response)
		outcome = &router.Outcome{Alias: res.Alias, Provider: hit.Response.Provider, Model: hit.Response.Model}
	} else {
		seq, oc, serr := g.router.Stream(ctx, prepared.request)
		if serr != nil {
			gerr := FromProvider(serr)
			if oc != nil {
				res.Outcome = *oc
			}
			g.finish(ctx, res, &rec, start, gerr)
			return StreamResult{Result: res}, gerr
		}
		upstream, outcome = seq, oc
	}

	// Collect the redacted chunks as they pass so that the cache and the audit
	// record see the redacted text, then restore for the client. The two wrap
	// in this order because the cache must never store a restored value.
	var (
		collected llm.Response
		accounted bool
	)
	tapped := func(yield func(llm.Chunk, error) bool) {
		for chunk, err := range upstream {
			if err != nil {
				yield(chunk, err)
				return
			}
			accumulate(&collected, chunk)
			if !yield(chunk, nil) {
				return
			}
		}
	}

	restored := tapped
	if prepared.vault != nil {
		restored = redact.RestoreStream(tapped, prepared.vault)
	}

	final := func(yield func(llm.Chunk, error) bool) {
		var streamErr error
		for chunk, err := range restored {
			if err != nil {
				streamErr = err
				yield(chunk, err)
				break
			}
			if !yield(chunk, nil) {
				// Client gone. Everything generated so far was still paid for,
				// so the accounting and the audit record happen anyway.
				break
			}
		}
		if accounted {
			return
		}
		accounted = true
		g.settleStream(ctx, principal, prepared, hit, outcome, collected, res, &rec, start, streamErr)
	}

	res.Outcome = *outcome
	return StreamResult{Result: res, Chunks: final}, nil
}

// settleStream does everything Complete does after the provider call, once the
// stream has ended for any reason.
func (g *Gateway) settleStream(ctx context.Context, p Principal, prepared prepared, hit cache.Result,
	outcome *router.Outcome, collected llm.Response, res *Result, rec *audit.Record,
	start time.Time, streamErr error,
) {
	if collected.Provider == "" {
		collected.Provider, collected.Model = outcome.Provider, outcome.Model
	}
	if collected.Usage.TotalTokens() == 0 && collected.Message.Content != "" {
		// A provider that streamed without a usage frame still has to be
		// billed. Estimating is marked as such so a cost report can say which
		// figures are measured.
		collected.Usage = llm.EstimateUsage(g.tokenizer, prepared.request, collected)
	}
	if streamErr == nil && !hit.Hit && collected.Message.Content != "" {
		g.CacheStore(ctx, prepared.request, collected)
	}

	res.Outcome = *outcome
	res.Response = collected
	if prepared.vault != nil && g.redactor != nil {
		res.Response = g.redactor.RestoreResponse(collected, prepared.vault)
	}
	res.Cost = g.costOf(hit, collected)
	g.Account(p, prepared.estimate, collected.Usage, res.Cost, collected.Provider, collected.Model)
	g.latency.observe(providerKey(collected, hit), g.now().Sub(start))

	g.fillRecord(rec, *res, collected)

	var gerr error
	switch {
	case streamErr == nil:
	case errors.Is(ctx.Err(), context.Canceled):
		// The client went away mid-stream. That is not a provider failure and
		// must not be counted as one: a user closing a tab would otherwise show
		// up as an error-rate spike and hide the failures that matter. It is
		// still recorded, with its own code, because the tokens were generated
		// and paid for.
		rec.ErrorCode = OutcomeClientDisconnected
	default:
		gerr = FromProvider(streamErr)
	}
	g.finish(ctx, res, rec, start, gerr)
}

// prepared is what the pre-provider stages produce.
type prepared struct {
	request  llm.Request
	vault    *redact.Vault
	estimate int
}

// prepare runs stages 1 to 4 and is shared by both entry points, so that the
// streaming path cannot drift from the buffered one on anything to do with
// authentication, limits, budgets or redaction.
func (g *Gateway) prepare(ctx context.Context, in *Request, res *Result, rec *audit.Record) (Principal, prepared, error) {
	_ = ctx

	// Stage 1: authenticate.
	stage := g.now()
	principal, err := g.Authenticate(in.Secret)
	g.observeStage("authenticate", stage)
	if err != nil {
		return principal, prepared{}, err
	}
	res.Principal = principal
	rec.KeyID, rec.Team = principal.KeyID, principal.Team

	if in.LLM.Model == "" {
		return principal, prepared{}, Invalid("the request must name a model")
	}
	if !principal.Allowed(in.LLM.Model) {
		return principal, prepared{}, Forbidden("key %q may not use model %q", principal.KeyID, in.LLM.Model)
	}
	if !g.router.Has(in.LLM.Model) {
		return principal, prepared{}, &Error{
			Status: 404, Kind: KindInvalidRequest,
			Message: "the model does not exist or you do not have access to it",
		}
	}

	// Stage 2: rate limit.
	stage = g.now()
	estimate, err := g.RateLimit(principal, in.LLM)
	g.observeStage("rate_limit", stage)
	if err != nil {
		return principal, prepared{}, err
	}

	// Stage 3: budget. A degrade decision rewrites the alias, which is why this
	// must run before the cache key exists.
	stage = g.now()
	decision, err := g.CheckBudget(principal, in.LLM.Model)
	g.observeStage("budget", stage)
	if err != nil {
		return principal, prepared{}, err
	}
	req := in.LLM
	if decision.Model != "" && decision.Model != req.Model {
		if !g.router.Has(decision.Model) {
			return principal, prepared{}, &Error{
				Status: 500, Kind: KindInternal,
				Message: "budget degradation target is not routable",
			}
		}
		req.Model = decision.Model
		res.Degraded = true
		res.Alias = decision.Model
		rec.Degraded = true
		g.log.Info("degraded to a cheaper model on budget exhaustion",
			"key_id", principal.KeyID, "from", in.LLM.Model, "to", decision.Model,
			"spent", decision.Spent.String(), "limit", decision.Limit.String())
	}

	// Stage 4: redact.
	stage = g.now()
	redacted, vault := g.Redact(req)
	g.observeStage("redact", stage)
	if vault != nil {
		res.Redactions = vault.Types()
		g.countRedactions(res.Redactions)
	}
	rec.Prompt = promptOf(redacted)
	return principal, prepared{request: redacted, vault: vault, estimate: estimate}, nil
}

// finish writes the audit record and the request-level metrics exactly once.
func (g *Gateway) finish(ctx context.Context, res *Result, rec *audit.Record, start time.Time, err error) {
	res.Latency = g.now().Sub(start)
	rec.LatencyMS = float64(res.Latency) / float64(time.Millisecond)
	rec.ID = orNewID(rec.ID)
	res.AuditID = rec.ID

	outcome := "ok"
	switch {
	case err == nil && rec.ErrorCode != "":
		// A non-error outcome that is still not a plain success, currently only
		// a client disconnect. It is a fixed string, so it is safe as a label.
		outcome = rec.ErrorCode
	case err != nil:
		gerr := FromProvider(err)
		outcome = string(gerr.Kind)
		rec.Error = gerr.Message
		rec.ErrorCode = gerr.Code
		if rec.ErrorCode == "" {
			rec.ErrorCode = string(gerr.Kind)
		}
	case res.Cache.Hit:
		outcome = "cache_" + string(res.Cache.Kind)
	}
	if g.metrics != nil {
		route := res.Alias
		g.metrics.Requests.WithLabelValues(route, outcome).Inc()
		g.metrics.RequestDuration.WithLabelValues(route, outcome).Observe(res.Latency.Seconds())
	}
	g.Audit(ctx, *rec)
}

// fillRecord copies the result into the audit record. resp must be the
// redacted response, not the restored one.
func (g *Gateway) fillRecord(rec *audit.Record, res Result, resp llm.Response) {
	rec.ServedModel = resp.Model
	rec.Provider = resp.Provider
	rec.Usage = resp.Usage
	rec.Estimated = resp.Usage.Estimated
	rec.Cost = res.Cost
	rec.Attempts = res.Outcome.Attempts
	rec.Failovers = res.Outcome.Failovers
	if res.Cache.Hit {
		rec.CacheHit = string(res.Cache.Kind)
		rec.CacheSimilarity = res.Cache.Similarity
	}
	if len(res.Redactions) > 0 {
		rec.RedactionCounts = make(map[string]int, len(res.Redactions))
		for t, n := range res.Redactions {
			rec.RedactionCounts[string(t)] = n
		}
	}
	// The completion is stored as the provider produced it, which is to say
	// still tokenized. Restoring it for the log would put the original values
	// back into the one file that outlives everything.
	rec.Completion = resp.Message.Content
}

// costOf prices a response, charging nothing for a cache hit.
//
// A cache hit that reported the cost of the call that originally filled it
// would double-count: the money was spent once and the report would show it
// twice. The saving is visible instead as the difference between the cache hit
// rate and the spend curve.
func (g *Gateway) costOf(hit cache.Result, resp llm.Response) llm.Cost {
	if hit.Hit {
		return 0
	}
	return g.Cost(resp.Model, resp.Usage)
}

func (g *Gateway) observeStage(name string, start time.Time) {
	if g.metrics != nil {
		g.metrics.StageDuration.WithLabelValues(name).Observe(g.now().Sub(start).Seconds())
	}
}

func (g *Gateway) countRedactions(counts map[redact.EntityType]int) {
	if len(counts) == 0 {
		return
	}
	g.mu.Lock()
	for t, n := range counts {
		g.redactionCounts[t] += uint64(n) //nolint:gosec // n is a match count, never negative
	}
	g.mu.Unlock()
	if g.metrics != nil {
		for t, n := range counts {
			g.metrics.Redactions.WithLabelValues(string(t)).Add(float64(n))
		}
	}
}

// accumulate folds a chunk into a response under construction. It is
// llm.Collect's body without the sequence, because the streaming path has to
// observe chunks as they pass rather than after they have all arrived.
func accumulate(resp *llm.Response, chunk llm.Chunk) {
	if resp.ID == "" {
		resp.ID = chunk.ID
	}
	if resp.Model == "" {
		resp.Model = chunk.Model
	}
	if resp.Provider == "" {
		resp.Provider = chunk.Provider
	}
	if chunk.Delta.Role != "" {
		resp.Message.Role = chunk.Delta.Role
	}
	resp.Message.Content += chunk.Delta.Content
	for _, tc := range chunk.Delta.ToolCalls {
		for len(resp.Message.ToolCalls) <= tc.Index {
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, llm.ToolCall{})
		}
		call := &resp.Message.ToolCalls[tc.Index]
		if tc.ID != "" {
			call.ID = tc.ID
		}
		if tc.Name != "" {
			call.Name = tc.Name
		}
		call.Arguments = append(call.Arguments, tc.ArgumentsDelta...)
	}
	if chunk.FinishReason != "" {
		resp.FinishReason = chunk.FinishReason
	}
	if chunk.Usage != nil {
		resp.Usage = *chunk.Usage
	}
}

func promptOf(req llm.Request) []audit.Message {
	out := make([]audit.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		out = append(out, audit.Message{Role: string(m.Role), Content: m.Content})
	}
	return out
}

func providerKey(resp llm.Response, hit cache.Result) string {
	if hit.Hit {
		return "cache"
	}
	if resp.Provider == "" {
		return "unknown"
	}
	return resp.Provider
}

// newID returns a request identifier.
//
// Random rather than sequential: a sequential identifier in a response header
// tells anyone who receives one how many requests the gateway has served, which
// is commercially interesting information to give away for free.
func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the process is in no state to serve
		// requests; a timestamp keeps the record identifiable rather than
		// empty, and the caller will find out about the real problem shortly.
		return "req_" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return "req_" + hex.EncodeToString(b[:])
}

func orNewID(id string) string {
	if id == "" {
		return newID()
	}
	return id
}
