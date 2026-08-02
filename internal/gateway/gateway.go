// Package gateway is the request pipeline: everything that happens between a
// client's call arriving and its answer leaving.
//
// # The pipeline, and why the stages are in this order
//
//	authenticate -> rate limit -> budget -> redact -> cache -> route -> call
//	             -> un-redact -> account -> audit
//
// Each stage is a method on Gateway with a name and a test. The order is not
// arbitrary and most of it is forced:
//
//   - Authenticate first. Nothing downstream can be attributed without a
//     principal, and it is the cheapest possible rejection: a map lookup
//     against a request that has not yet cost anything.
//
//   - Rate limit before budget. Both are rejections, so the cheaper one goes
//     first; more importantly, a client in a hot loop should be stopped by the
//     mechanism designed for hot loops rather than by the one designed for
//     monthly spend, so that the error it gets says "slow down" and carries a
//     Retry-After rather than "you are out of money".
//
//   - Budget before redaction. Redaction is the most expensive stage that runs
//     on every request -- ten detectors over every message -- and a request
//     that is about to be refused should not pay for it. The budget stage may
//     also rewrite the model alias (degradation), and the cache namespace is
//     the alias, so it has to be settled before a cache key exists.
//
//   - Redaction before caching. This is the one that looks like it could go
//     either way and cannot. Three reasons, in increasing order of importance.
//     Cache entries are stored redacted, so the cache -- which is a long-lived
//     in-memory store, and in a future version a shared one -- never holds
//     personal data. Placeholders are allocated deterministically per request
//     (first email is always [SLUICE_EMAIL_0001]), so two customers asking the
//     same question about their own different addresses produce the identical
//     redacted prompt and share one cache entry; redacting after the cache
//     would make every such request a miss. And decisively: a cache hit skips
//     the provider call, so if redaction ran after the cache it would not run
//     at all on a hit, and the response served would be one built from
//     someone else's unredacted text.
//
//     The consequence has to be stated because it is a real one. Two callers
//     whose prompts differ only in the redacted values get the same cached
//     answer, each with their own values substituted back in. That is correct
//     -- it is the whole point of a reversible placeholder -- but it means the
//     cache is shared across tenants by design. A deployment that needs
//     per-tenant isolation must namespace the cache, and this build does not.
//
//   - Cache before routing, obviously: the cheapest provider call is the one
//     that does not happen.
//
//   - Un-redaction after the provider and after the cache write. The response
//     is stored in the cache in its redacted form and restored per caller, so
//     that the caller's own vault decides what each placeholder means.
//
//   - Accounting before auditing, because the audit record contains the cost.
//
// Every stage is exported as a method so that it can be tested in isolation and
// so that the order above is visible in Complete rather than buried in a chain
// of middleware.
package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sluice-gw/sluice/internal/audit"
	"github.com/sluice-gw/sluice/internal/cache"
	"github.com/sluice-gw/sluice/internal/config"
	"github.com/sluice-gw/sluice/internal/policy"
	"github.com/sluice-gw/sluice/internal/redact"
	"github.com/sluice-gw/sluice/internal/router"
	"github.com/sluice-gw/sluice/internal/telemetry"
	"github.com/sluice-gw/sluice/pkg/llm"
	"github.com/sluice-gw/sluice/pkg/provider/local"
)

// Principal is an authenticated caller.
type Principal struct {
	KeyID string
	Team  string
	// Models is the set of route aliases this key may use; empty means all.
	Models     []string
	Limits     policy.Limits
	Budget     policy.Budget
	TeamBudget policy.Budget
}

// Allowed reports whether the principal may use a route alias.
func (p Principal) Allowed(model string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if m == model {
			return true
		}
	}
	return false
}

// Request is one call into the gateway.
type Request struct {
	// Secret is the bearer token as presented.
	Secret string
	// LLM is the request in Sluice's own vocabulary. Model is a route alias.
	LLM llm.Request
	// Stream selects the streaming path.
	Stream bool
	// ClientAddr is recorded in logs. It is never a metric label.
	ClientAddr string
}

// Result describes how a request was served. It is what the audit record and
// the response headers are built from.
type Result struct {
	Response  llm.Response
	Principal Principal
	// Alias is what the client asked for; Response.Model is what served it.
	Alias   string
	Cache   cache.Result
	Outcome router.Outcome
	Cost    llm.Cost
	// Redactions is entity type to count of distinct values replaced.
	Redactions map[redact.EntityType]int
	Degraded   bool
	Latency    time.Duration
	AuditID    string
}

// Options configures a Gateway. Everything is injectable because everything
// here is a thing a test needs to control: the clock, the audit sink, the
// provider set.
type Options struct {
	Config  config.Config
	Metrics *telemetry.Metrics
	Logger  *slog.Logger
	// Auditor receives one record per request. Nil means audit.Nop.
	Auditor audit.Recorder
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Providers overrides the providers built from Config. A test that needs a
	// provider which fails in a particular way supplies it here rather than
	// describing it in YAML.
	Providers map[string]llm.Provider
	// Router overrides the router built from Config, for the same reason.
	Router *router.Router
}

// Gateway is the assembled pipeline. It is safe for concurrent use.
type Gateway struct {
	cfg     config.Config
	log     *slog.Logger
	metrics *telemetry.Metrics
	now     func() time.Time

	keys      map[[32]byte]Principal
	router    *router.Router
	redactor  *redact.Redactor
	cache     *cache.Cache
	limiter   *policy.RateLimiter
	budgets   *policy.Budgets
	pricing   *llm.Pricing
	tokenizer llm.Approx
	auditor   audit.Recorder
	recent    *audit.Memory
	latency   *latencyTracker

	// redactionCounts accumulates per-entity totals for the dashboard. The
	// Prometheus counter is authoritative for alerting; this is here so the
	// dashboard does not have to query Prometheus to draw one chart.
	mu              sync.Mutex
	redactionCounts map[redact.EntityType]uint64
	started         time.Time
}

// New builds a Gateway from a validated config.
func New(opts Options) (*Gateway, error) {
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &Gateway{
		cfg:             cfg,
		log:             opts.Logger,
		metrics:         opts.Metrics,
		now:             opts.Now,
		keys:            make(map[[32]byte]Principal, len(cfg.Keys)),
		pricing:         cfg.PricingTable(),
		auditor:         opts.Auditor,
		recent:          audit.NewMemory(200),
		latency:         newLatencyTracker(512),
		redactionCounts: map[redact.EntityType]uint64{},
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.auditor == nil {
		g.auditor = audit.Nop{}
	}
	g.started = g.now()
	// The in-memory ring always receives a copy, so the dashboard works even
	// when the durable log is off. Tee returns the first error but delivers to
	// both, so a full disk does not blind the dashboard.
	g.auditor = audit.Tee{g.auditor, g.recent}

	for _, k := range cfg.Keys {
		p := Principal{
			KeyID:  k.ID,
			Team:   k.Team,
			Models: append([]string(nil), k.Models...),
			Limits: policy.Limits{
				RequestsPerMinute: k.RateLimit.RequestsPerMinute,
				TokensPerMinute:   k.RateLimit.TokensPerMinute,
				Burst:             k.RateLimit.Burst,
			},
			Budget: policy.Budget{
				Limit: k.Budget.Limit.Cost(), Window: k.Budget.Window.D(),
				OnExceed: string(k.Budget.OnExceed), DegradeTo: k.Budget.DegradeTo,
			},
		}
		if t, ok := cfg.Team(k.Team); ok {
			p.TeamBudget = policy.Budget{
				Limit: t.Budget.Limit.Cost(), Window: t.Budget.Window.D(),
				OnExceed: string(t.Budget.OnExceed), DegradeTo: t.Budget.DegradeTo,
			}
		}
		g.keys[sha256.Sum256([]byte(k.Secret))] = p
	}

	if cfg.Redaction.Enabled {
		g.redactor = redact.New(cfg.RedactPolicy())
	}
	if cfg.Cache.Enabled {
		copts := cache.Options{
			MaxEntries:          cfg.Cache.MaxEntries,
			TTL:                 cfg.Cache.TTL.D(),
			SimilarityThreshold: cfg.Cache.SimilarityThreshold,
			Now:                 g.now,
		}
		if cfg.Cache.Semantic {
			copts.Embedder = cache.NewHashingEmbedder(cfg.Cache.EmbeddingDimensions)
		}
		c, err := cache.New(copts)
		if err != nil {
			return nil, err
		}
		g.cache = c
	}
	g.limiter = policy.NewRateLimiter(g.now)
	g.budgets = policy.NewBudgets(g.now)

	if opts.Router != nil {
		g.router = opts.Router
	} else {
		r, err := buildRouter(cfg, opts.Providers, g.now, g.observeAttempt)
		if err != nil {
			return nil, err
		}
		g.router = r
	}
	return g, nil
}

// buildRouter turns the declarative routes into a live router.
func buildRouter(cfg config.Config, override map[string]llm.Provider, now func() time.Time, onAttempt func(router.Attempt)) (*router.Router, error) {
	providers := make(map[string]llm.Provider, len(cfg.Providers))
	for _, pc := range cfg.Providers {
		if p, ok := override[pc.Name]; ok {
			providers[pc.Name] = p
			continue
		}
		p, err := local.New(local.Config{
			Name:            pc.Name,
			Seed:            pc.Seed,
			Models:          pc.Models,
			ContextTokens:   pc.ContextTokens,
			MaxOutputTokens: pc.MaxOutputTokens,
			Latency: local.Latency{
				TimeToFirstToken: pc.Latency.TimeToFirstToken.D(),
				PerToken:         pc.Latency.PerToken.D(),
				Jitter:           pc.Latency.Jitter,
			},
			Failure: local.Failure{
				Code:        llm.ErrorCode(pc.Failure.Code),
				Rate:        pc.Failure.Rate,
				RetryAfter:  pc.Failure.RetryAfter.D(),
				Message:     pc.Failure.Message,
				AfterChunks: pc.Failure.AfterChunks,
			},
			Now: now,
		})
		if err != nil {
			return nil, fmt.Errorf("gateway: provider %q: %w", pc.Name, err)
		}
		providers[pc.Name] = p
	}

	specs := make([]router.RouteSpec, 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		spec := router.RouteSpec{
			Alias:    rc.Model,
			Strategy: router.Strategy(rc.Strategy),
			Retry: router.RetryPolicy{
				MaxAttempts: rc.Retry.MaxAttempts,
				BaseDelay:   rc.Retry.BaseDelay.D(),
				MaxDelay:    rc.Retry.MaxDelay.D(),
				Multiplier:  rc.Retry.Multiplier,
				Jitter:      rc.Retry.Jitter,
			},
			Breaker: router.BreakerConfig{
				FailureThreshold: rc.Breaker.FailureThreshold,
				SuccessThreshold: rc.Breaker.SuccessThreshold,
				OpenFor:          rc.Breaker.OpenFor.D(),
				HalfOpenProbes:   rc.Breaker.HalfOpenProbes,
			},
		}
		for _, tc := range rc.Targets {
			spec.Targets = append(spec.Targets, router.TargetSpec{
				Provider: providers[tc.Provider], Model: tc.Model, Weight: tc.Weight,
			})
		}
		specs = append(specs, spec)
	}
	return router.New(router.Options{Routes: specs, Now: now, OnAttempt: onAttempt})
}

// --- stage 1: authenticate --------------------------------------------------

// Authenticate resolves a bearer secret to a principal.
//
// Keys are indexed by the SHA-256 of the secret and the presented secret is
// hashed before the lookup, so the table is keyed on a fixed-size array whose
// comparison time does not depend on how many leading bytes of a guess were
// correct. A map keyed on the plaintext would compare strings, which stops at
// the first differing byte and turns the lookup into an oracle for anyone
// willing to measure. It also means a heap dump of a running gateway contains
// digests rather than a list of valid credentials.
//
// What this does not do: it is not a password hash. SHA-256 over a
// high-entropy random key is fine; SHA-256 over a key someone chose is not, and
// the config validator's 16-character minimum is the only thing standing
// between those two cases. SECURITY.md says so in more detail.
func (g *Gateway) Authenticate(secret string) (Principal, error) {
	if secret == "" {
		return Principal{}, &Error{
			Status: 401, Kind: KindAuthentication,
			Message: "missing API key; send it as \"Authorization: Bearer <key>\"",
		}
	}
	sum := sha256.Sum256([]byte(secret))
	p, ok := g.keys[sum]
	if !ok {
		return Principal{}, &Error{Status: 401, Kind: KindAuthentication, Message: "invalid API key"}
	}
	return p, nil
}

// --- stage 2: rate limit ----------------------------------------------------

// RateLimit charges the request against the key's token buckets.
//
// The token charge is an estimate made before the call: prompt tokens from the
// tokenizer plus MaxTokens, or a floor when MaxTokens is unset. It has to be an
// estimate, because the only alternative is charging after the fact, and a
// limiter that learns the cost after the request has been made cannot refuse
// anything. Settle reconciles once the real usage is known.
func (g *Gateway) RateLimit(p Principal, req llm.Request) (int, error) {
	estimate := g.estimateTokens(req)
	if err := g.limiter.Allow("key:"+p.KeyID, p.Limits, estimate); err != nil {
		var le *policy.LimitError
		if errors.As(err, &le) {
			if g.metrics != nil {
				g.metrics.RateLimited.WithLabelValues(string(le.Kind)).Inc()
			}
			return estimate, &Error{
				Status: 429, Kind: KindRateLimit, Message: le.Error(),
				RetryAfter: le.RetryAfter, Err: err,
			}
		}
		return estimate, &Error{Status: 429, Kind: KindRateLimit, Message: err.Error(), Err: err}
	}
	return estimate, nil
}

// estimateTokens is the pre-call token estimate: the prompt as counted by the
// tokenizer, plus the requested completion length.
//
// When MaxTokens is unset the completion is assumed to be 512 tokens. That is a
// guess, and it is deliberately not a small one: assuming zero would let a
// caller evade the token limit entirely by omitting max_tokens, which is the
// default in every client library.
func (g *Gateway) estimateTokens(req llm.Request) int {
	in := g.tokenizer.CountRequest(req)
	out := req.MaxTokens
	if out <= 0 {
		out = 512
	}
	return in + out
}

// --- stage 3: budget --------------------------------------------------------

// CheckBudget evaluates the key budget and then the team budget.
//
// Both are checked and the stricter outcome wins, with the key checked first so
// that a caller who has exhausted their own allowance is told so rather than
// being told their team is out of money. A team-level rejection is a different
// conversation from a key-level one.
func (g *Gateway) CheckBudget(p Principal, alias string) (policy.Decision, error) {
	decision := policy.Decision{Action: policy.ActionAllow, Model: alias}
	checks := []struct {
		subject string
		budget  policy.Budget
	}{{"key:" + p.KeyID, p.Budget}}
	if p.Team != "" {
		checks = append(checks, struct {
			subject string
			budget  policy.Budget
		}{"team:" + p.Team, p.TeamBudget})
	}
	for _, check := range checks {
		if check.budget.Zero() {
			continue
		}
		d := g.budgets.Check(check.subject, check.budget, decision.Model)
		switch d.Action {
		case policy.ActionReject:
			if g.metrics != nil {
				g.metrics.BudgetDecisions.WithLabelValues(string(policy.ActionReject)).Inc()
			}
			return d, &Error{
				Status: 402, Kind: KindBudget,
				Message: (&policy.ExceededError{Decision: d}).Error(),
			}
		case policy.ActionDegrade:
			decision = d
		case policy.ActionAllow:
		}
	}
	if g.metrics != nil {
		g.metrics.BudgetDecisions.WithLabelValues(string(decision.Action)).Inc()
	}
	return decision, nil
}

// --- stage 4: redact --------------------------------------------------------

// Redact rewrites the request and returns the vault needed to reverse it.
//
// With redaction disabled it returns the request unchanged and a nil vault;
// every downstream stage treats a nil vault as "nothing to restore" so that the
// disabled path is the same code path rather than a parallel one.
func (g *Gateway) Redact(req llm.Request) (llm.Request, *redact.Vault) {
	if g.redactor == nil {
		return req, nil
	}
	return g.redactor.RedactRequest(req)
}

// --- stage 5: cache ---------------------------------------------------------

// CacheLookup checks the cache for a redacted request.
//
// A lookup error is logged and reported as a miss. Failing a request because an
// optimisation broke would trade availability for nothing.
func (g *Gateway) CacheLookup(ctx context.Context, req llm.Request) cache.Result {
	if g.cache == nil {
		return cache.Result{}
	}
	if cache.Bypassed(ctx) {
		g.countCache("bypass")
		return cache.Result{}
	}
	res, err := g.cache.Get(ctx, req)
	switch {
	case err != nil:
		g.log.WarnContext(ctx, "cache lookup failed", "error", err)
		g.countCache("error")
		return cache.Result{}
	case res.Hit:
		g.countCache(string(res.Kind))
	default:
		g.countCache("miss")
	}
	return res
}

// CacheStore writes a redacted response under a redacted request.
func (g *Gateway) CacheStore(ctx context.Context, req llm.Request, resp llm.Response) {
	if g.cache == nil {
		return
	}
	if err := g.cache.Put(ctx, req, resp); err != nil {
		g.log.WarnContext(ctx, "cache store failed", "error", err)
	}
	if g.metrics != nil {
		g.metrics.CacheEntries.Set(float64(g.cache.Len()))
	}
}

func (g *Gateway) countCache(result string) {
	if g.metrics != nil {
		g.metrics.CacheLookups.WithLabelValues(result).Inc()
	}
}

// --- stage 8: account -------------------------------------------------------

// Cost prices a usage record against the model that produced it.
//
// An unpriced model is a warning and a zero cost, not a failed request. The
// alternative -- refusing to serve a model whose price is unknown -- would make
// adding a model to the routing table a two-step deploy, and operators would
// route around it. The audit record carries the token counts either way, so a
// price discovered later can be applied retrospectively with `sluice replay`.
func (g *Gateway) Cost(model string, u llm.Usage) llm.Cost {
	c, err := g.pricing.Cost(model, u)
	if err != nil {
		g.log.Warn("no price for model; recording zero cost",
			"model", model, "input_tokens", u.InputTokens, "output_tokens", u.OutputTokens)
		return 0
	}
	return c
}

// Account records spend against the budgets and settles the rate limiter.
func (g *Gateway) Account(p Principal, estimated int, u llm.Usage, cost llm.Cost, provider, model string) {
	g.limiter.Settle("key:"+p.KeyID, estimated, u.TotalTokens())
	if !p.Budget.Zero() {
		g.budgets.Record("key:"+p.KeyID, p.Budget.Window, cost)
	}
	if p.Team != "" && !p.TeamBudget.Zero() {
		g.budgets.Record("team:"+p.Team, p.TeamBudget.Window, cost)
	}
	if g.metrics != nil && provider != "" {
		g.metrics.ObserveUsage(provider, model, u, cost)
	}
}

// --- stage 9: audit ---------------------------------------------------------

// Audit writes the record for a completed request.
//
// The prompt is written from the redacted request, never the original; see the
// package comment on internal/audit for why that is not negotiable.
func (g *Gateway) Audit(ctx context.Context, rec audit.Record) {
	if err := g.auditor.Record(rec); err != nil {
		g.log.ErrorContext(ctx, "audit record dropped", "error", err, "id", rec.ID)
		if g.metrics != nil {
			g.metrics.AuditDropped.Inc()
		}
		return
	}
	if g.metrics != nil {
		g.metrics.AuditWritten.Inc()
	}
}

// observeAttempt bridges the router's per-attempt callback into metrics.
func (g *Gateway) observeAttempt(a router.Attempt) {
	if g.metrics == nil {
		return
	}
	if a.ShortCircuited {
		g.metrics.ShortCircuits.WithLabelValues(a.Provider, a.Model).Inc()
		return
	}
	code := "ok"
	if a.Err != nil {
		code = string(a.Code)
	}
	g.metrics.UpstreamAttempts.WithLabelValues(a.Provider, a.Model, code).Inc()
	if a.Latency > 0 {
		g.metrics.UpstreamDuration.WithLabelValues(a.Provider, a.Model).Observe(a.Latency.Seconds())
	}
}

// Router exposes the router, for the status endpoint.
func (g *Gateway) Router() *router.Router { return g.router }

// Config returns the configuration in force.
func (g *Gateway) Config() config.Config { return g.cfg }

// Sweep runs the periodic maintenance every bounded-memory structure needs.
// The server calls it on a ticker; it is exported so a test can call it
// directly instead of waiting.
func (g *Gateway) Sweep() {
	if g.cache != nil {
		g.cache.Sweep()
		if g.metrics != nil {
			g.metrics.CacheEntries.Set(float64(g.cache.Len()))
		}
	}
	g.limiter.Sweep()
	g.budgets.Sweep()
}
