// Package router turns a model alias into an actual call to an actual
// upstream, and keeps doing so when the upstream stops working.
//
// Four mechanisms compose, and the order they compose in is the design:
//
//  1. Selection picks an order over the route's targets, from the configured
//     strategy, skipping targets whose circuit breaker is open.
//  2. Retry re-attempts the chosen target while the error is retryable and the
//     caller's deadline allows, with exponential backoff, jitter, and the
//     provider's own Retry-After honoured over our arithmetic.
//  3. Failover moves to the next target when the error says a different
//     provider might do better -- and only then. llm.ErrorCode draws that line;
//     the router does not second-guess it.
//  4. The circuit breaker records each attempt so that step 1 stops choosing a
//     target that has been failing.
//
// The one rule that overrides all four: a stream that has already emitted a
// token to the client is never retried or failed over. See Stream.
package router

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"

	"sync/atomic"
	"time"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// Strategy selects between healthy targets. The values match the config file's.
type Strategy string

const (
	// StrategyPriority always prefers the first healthy target.
	StrategyPriority Strategy = "priority"
	// StrategyRoundRobin spreads load evenly.
	StrategyRoundRobin Strategy = "round_robin"
	// StrategyWeighted spreads load in proportion to Weight.
	StrategyWeighted Strategy = "weighted"
)

// RetryPolicy configures attempts against one target.
type RetryPolicy struct {
	// MaxAttempts counts the first try; 1 disables retry.
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Multiplier  float64
	// Jitter is the fraction of a computed delay that is randomised.
	Jitter float64
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = 50 * time.Millisecond
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 5 * time.Second
	}
	if p.Multiplier < 1 {
		p.Multiplier = 2
	}
	return p
}

// TargetSpec is one provider/model pair a route may use.
type TargetSpec struct {
	Provider llm.Provider
	// Model is the physical model name sent upstream.
	Model string
	// Weight is used by StrategyWeighted; zero is treated as one.
	Weight int
}

// RouteSpec declares one model alias.
type RouteSpec struct {
	Alias    string
	Strategy Strategy
	Targets  []TargetSpec
	Retry    RetryPolicy
	Breaker  BreakerConfig
}

// Attempt describes one call to one target, delivered to Options.OnAttempt.
//
// It carries the provider and model but never the prompt or the key: this is
// the struct metrics are derived from, and a metric labelled by anything
// unbounded is an outage waiting for a busy afternoon.
type Attempt struct {
	Alias    string
	Provider string
	Model    string
	// Number is 1-based across the whole request, counting retries and
	// failovers together, so that "attempt 4" is the fourth upstream call
	// whether it was the same target again or a different one.
	Number   int
	Err      error
	Code     llm.ErrorCode
	Latency  time.Duration
	Streamed bool
	// ShortCircuited is true when the breaker refused before any call was made.
	ShortCircuited bool
}

// Options configures a Router.
type Options struct {
	Routes []RouteSpec
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Rand returns a float in [0,1). Nil means the global source. Injectable so
	// that jitter and weighted selection are deterministic in tests, which is
	// the only way to assert on a backoff schedule.
	Rand func() float64
	// Sleep waits or returns the context's error. Nil means a real timer.
	// Injectable so that a retry test does not spend its budget sleeping.
	Sleep func(context.Context, time.Duration) error
	// OnAttempt observes every upstream call. Nil disables observation.
	OnAttempt func(Attempt)
}

// Router routes requests. It is safe for concurrent use and immutable after
// New; changing routes means building a new Router and swapping it.
type Router struct {
	routes    map[string]*route
	aliases   []string
	now       func() time.Time
	rnd       func() float64
	sleep     func(context.Context, time.Duration) error
	onAttempt func(Attempt)
}

type route struct {
	alias    string
	strategy Strategy
	targets  []*target
	retry    RetryPolicy
	cursor   atomic.Uint64
}

type target struct {
	provider llm.Provider
	model    string
	weight   int
	breaker  *Breaker
}

// Errors the router itself produces. They are llm.Errors so that a caller can
// classify them with the same taxonomy as a provider failure.
var (
	// ErrNoRoute means the alias is not configured.
	ErrNoRoute = errors.New("router: no route for model")
	// ErrNoTarget means every target for the route is short-circuited.
	ErrNoTarget = errors.New("router: no target available")
)

// New validates the specs and builds a Router.
func New(opts Options) (*Router, error) {
	r := &Router{
		routes:    make(map[string]*route, len(opts.Routes)),
		now:       opts.Now,
		rnd:       opts.Rand,
		sleep:     opts.Sleep,
		onAttempt: opts.OnAttempt,
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.rnd == nil {
		r.rnd = rand.Float64
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	if len(opts.Routes) == 0 {
		return nil, errors.New("router: no routes configured")
	}
	for _, spec := range opts.Routes {
		if spec.Alias == "" {
			return nil, errors.New("router: a route has an empty alias")
		}
		if _, dup := r.routes[spec.Alias]; dup {
			return nil, fmt.Errorf("router: duplicate route %q", spec.Alias)
		}
		if len(spec.Targets) == 0 {
			return nil, fmt.Errorf("router: route %q has no targets", spec.Alias)
		}
		rt := &route{alias: spec.Alias, strategy: spec.Strategy, retry: spec.Retry.withDefaults()}
		if rt.strategy == "" {
			rt.strategy = StrategyPriority
		}
		for i, ts := range spec.Targets {
			if ts.Provider == nil {
				return nil, fmt.Errorf("router: route %q target %d has no provider", spec.Alias, i)
			}
			w := ts.Weight
			if w <= 0 {
				w = 1
			}
			model := ts.Model
			if model == "" {
				model = spec.Alias
			}
			rt.targets = append(rt.targets, &target{
				provider: ts.Provider, model: model, weight: w,
				breaker: NewBreaker(spec.Breaker, r.now),
			})
		}
		r.routes[spec.Alias] = rt
		r.aliases = append(r.aliases, spec.Alias)
	}
	return r, nil
}

// Aliases lists the configured model aliases, in declaration order.
func (r *Router) Aliases() []string { return append([]string(nil), r.aliases...) }

// Has reports whether an alias is routable.
func (r *Router) Has(alias string) bool {
	_, ok := r.routes[alias]
	return ok
}

// Outcome summarises how a request was served.
//
// For Complete it is final on return. For Stream it is filled as the stream
// progresses and is only final once iteration has ended; it is written on the
// consumer's goroutine, so a consumer that reads it after the range loop needs
// no synchronisation and one that reads it during needs a mutex it does not
// have. Read it afterwards.
type Outcome struct {
	Alias string
	// Provider and Model are the target that served, empty if none did.
	Provider string
	Model    string
	// Attempts counts upstream calls actually made.
	Attempts int
	// Failovers counts moves to a different target.
	Failovers int
	// ShortCircuits counts targets skipped because their breaker was open.
	ShortCircuits int
	// Degraded is not set by the router; the gateway sets it when a budget
	// rewrote the alias. It lives here so that one struct describes the routing
	// decision in the audit record.
	Degraded bool
}

// Complete routes and performs a buffered completion.
//
// req.Model is a route alias; the physical model is substituted before the
// request reaches a provider, and Response.Model reports what actually served
// it. That substitution is why the alias has to be recorded separately: a cost
// report that only knows the physical model cannot answer "what did the
// 'summarise' feature cost".
func (r *Router) Complete(ctx context.Context, req llm.Request) (llm.Response, Outcome, error) {
	rt, ok := r.routes[req.Model]
	if !ok {
		return llm.Response{}, Outcome{Alias: req.Model}, fmt.Errorf("%w: %q", ErrNoRoute, req.Model)
	}
	out := Outcome{Alias: rt.alias}
	var lastErr error

	for _, t := range r.order(rt) {
		if !t.breaker.Allow() {
			out.ShortCircuits++
			r.observe(Attempt{Alias: rt.alias, Provider: t.provider.Name(), Model: t.model,
				Number: out.Attempts + 1, ShortCircuited: true, Code: llm.CodeProviderUnavailable})
			lastErr = shortCircuitError(t)
			continue
		}
		resp, err := r.attemptComplete(ctx, rt, t, req, &out)
		if err == nil {
			out.Provider, out.Model = t.provider.Name(), t.model
			return resp, out, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			// The caller gave up. Trying another provider would spend money on
			// a response nobody is waiting for.
			return llm.Response{}, out, err
		}
		if !llm.ShouldFailover(err) {
			return llm.Response{}, out, err
		}
		out.Failovers++
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w for %q", ErrNoTarget, rt.alias)
	}
	return llm.Response{}, out, lastErr
}

// attemptComplete runs the retry loop against one target. The breaker is
// recorded once per attempt, not once per target, because a target that fails
// three times in one request has produced three pieces of evidence.
func (r *Router) attemptComplete(ctx context.Context, rt *route, t *target, req llm.Request, out *Outcome) (llm.Response, error) {
	physical := req
	physical.Model = t.model

	var lastErr error
	for attempt := 1; attempt <= rt.retry.MaxAttempts; attempt++ {
		if attempt > 1 {
			if !t.breaker.Allow() {
				out.ShortCircuits++
				return llm.Response{}, shortCircuitError(t)
			}
		}
		start := r.now()
		resp, err := t.provider.Complete(ctx, physical)
		latency := r.now().Sub(start)
		out.Attempts++
		t.breaker.Record(err)
		r.observe(Attempt{Alias: rt.alias, Provider: t.provider.Name(), Model: t.model,
			Number: out.Attempts, Err: err, Code: llm.CodeOf(err), Latency: latency})

		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !llm.IsRetryable(err) || attempt == rt.retry.MaxAttempts {
			return llm.Response{}, err
		}
		if werr := r.wait(ctx, rt.retry, attempt, err); werr != nil {
			return llm.Response{}, werr
		}
	}
	return llm.Response{}, lastErr
}

// Stream routes and performs a streaming completion.
//
// The outer error covers everything that goes wrong before the client has been
// committed to a response: no route, every target short-circuited, or every
// target refusing the request outright. Failover across those is eager, so a
// caller that has not yet written a response header can still write a JSON
// error instead of a half-finished event stream.
//
// Once iteration starts, one rule applies without exception: a failure that
// arrives after a chunk has been yielded is delivered to the caller as-is. It
// is not retried and it is not failed over.
//
// The reason is not that a chat completion has side effects upstream -- it
// usually does not -- but that the side effect is downstream, in the client's
// buffer. Some prefix of the answer has already been printed on someone's
// screen. Restarting on another provider produces a second, different answer
// which the client will concatenate onto the first, and the result is a
// response that contradicts itself in the middle with no marker of where the
// seam is. That is worse than the error: an error is handled, a silently
// spliced answer is believed. So the router tracks whether anything has been
// emitted and refuses to recover once it has, and TestStreamNeverFailsOverAfterFirstToken
// is what keeps that true.
//
// Failure before the first chunk is a different case. Nothing has reached the
// client, the request is repeatable, and failover is exactly right; the loop
// below does it inside the iterator so that a sequence which errors on its
// first element is still recoverable.
func (r *Router) Stream(ctx context.Context, req llm.Request) (iter.Seq2[llm.Chunk, error], *Outcome, error) {
	rt, ok := r.routes[req.Model]
	if !ok {
		return nil, &Outcome{Alias: req.Model}, fmt.Errorf("%w: %q", ErrNoRoute, req.Model)
	}
	out := &Outcome{Alias: rt.alias}
	candidates := r.order(rt)

	// Open the first workable stream eagerly. Everything that fails here has
	// produced no output, so failover is free and the caller learns about the
	// failure before it has committed to an SSE response.
	first, opened, err := r.openStream(ctx, rt, candidates, req, out)
	if err != nil {
		return nil, out, err
	}
	remaining := candidates[opened+1:]
	t := candidates[opened]
	out.Provider, out.Model = t.provider.Name(), t.model

	seq := func(yield func(llm.Chunk, error) bool) {
		emitted := false
		current := first
		currentTarget := t
		rest := remaining

		for {
			var streamErr error
			for chunk, err := range current {
				if err != nil {
					streamErr = err
					break
				}
				emitted = true
				if !yield(chunk, nil) {
					// The consumer walked away -- a client disconnect, a
					// guardrail, a cancelled context. Recording a success would
					// be wrong and recording a failure would blame the provider
					// for the client's decision, so the breaker is told nothing
					// and the outcome keeps whatever it had.
					currentTarget.breaker.Record(nil)
					return
				}
			}
			currentTarget.breaker.Record(streamErr)
			r.observe(Attempt{Alias: rt.alias, Provider: currentTarget.provider.Name(),
				Model: currentTarget.model, Number: out.Attempts, Err: streamErr,
				Code: llm.CodeOf(streamErr), Streamed: true})

			if streamErr == nil {
				return
			}
			if emitted || !llm.ShouldFailover(streamErr) || ctx.Err() != nil {
				yield(llm.Chunk{}, streamErr)
				return
			}
			next, opened, err := r.openStream(ctx, rt, rest, req, out)
			if err != nil {
				// Nothing left to fail over to; report the failure that got us
				// here rather than the bookkeeping error about running out of
				// targets, because the former is the one an operator can act on.
				yield(llm.Chunk{}, streamErr)
				return
			}
			out.Failovers++
			currentTarget = rest[opened]
			out.Provider, out.Model = currentTarget.provider.Name(), currentTarget.model
			rest = rest[opened+1:]
			current = next
		}
	}
	return seq, out, nil
}

// openStream walks candidates until one hands back a sequence, returning it and
// the index of the target that produced it.
func (r *Router) openStream(ctx context.Context, rt *route, candidates []*target, original llm.Request, out *Outcome) (iter.Seq2[llm.Chunk, error], int, error) {
	var lastErr error
	for i, t := range candidates {
		if !t.breaker.Allow() {
			out.ShortCircuits++
			r.observe(Attempt{Alias: rt.alias, Provider: t.provider.Name(), Model: t.model,
				Number: out.Attempts + 1, ShortCircuited: true, Streamed: true,
				Code: llm.CodeProviderUnavailable})
			lastErr = shortCircuitError(t)
			continue
		}
		physical := original
		physical.Model = t.model
		seq, err := r.openWithRetry(ctx, rt, t, physical, out)
		if err == nil {
			return seq, i, nil
		}
		lastErr = err
		if ctx.Err() != nil || !llm.ShouldFailover(err) {
			return nil, 0, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w for %q", ErrNoTarget, rt.alias)
	}
	return nil, 0, lastErr
}

// openWithRetry retries the pre-stream open against one target. Only the open
// is retried: by the time the sequence exists, the next failure may be a
// mid-stream one, and that path is not retryable at all.
func (r *Router) openWithRetry(ctx context.Context, rt *route, t *target, physical llm.Request, out *Outcome) (iter.Seq2[llm.Chunk, error], error) {
	var lastErr error
	for attempt := 1; attempt <= rt.retry.MaxAttempts; attempt++ {
		if attempt > 1 && !t.breaker.Allow() {
			out.ShortCircuits++
			return nil, shortCircuitError(t)
		}
		start := r.now()
		seq, err := t.provider.Stream(ctx, physical)
		out.Attempts++
		if err == nil {
			// The breaker is not recorded here. Opening a stream succeeded, but
			// the request has not; the outcome is recorded once the sequence
			// ends, in Stream.
			return seq, nil
		}
		t.breaker.Record(err)
		r.observe(Attempt{Alias: rt.alias, Provider: t.provider.Name(), Model: t.model,
			Number: out.Attempts, Err: err, Code: llm.CodeOf(err),
			Latency: r.now().Sub(start), Streamed: true})
		lastErr = err
		if !llm.IsRetryable(err) || attempt == rt.retry.MaxAttempts {
			return nil, err
		}
		if werr := r.wait(ctx, rt.retry, attempt, err); werr != nil {
			return nil, werr
		}
	}
	return nil, lastErr
}

// wait sleeps between attempts.
//
// The provider's Retry-After wins over the computed backoff, and it wins even
// when it is longer than MaxDelay. Capping it would mean retrying sooner than a
// provider that is actively throttling us asked, which is how a rate limit
// becomes a ban. Jitter is applied upward only for the same reason: never
// earlier than asked.
//
// If the wait would outlast the caller's deadline the router does not sleep at
// all -- it returns the error immediately so that the caller can fail over to a
// provider that might answer inside the time remaining, rather than spending
// what is left of the deadline waiting.
func (r *Router) wait(ctx context.Context, p RetryPolicy, attempt int, err error) error {
	delay := r.backoff(p, attempt)
	if after, ok := llm.RetryAfter(err); ok {
		delay = after
		if p.Jitter > 0 {
			delay += time.Duration(float64(delay) * p.Jitter * r.rnd())
		}
	}
	if deadline, ok := ctx.Deadline(); ok && r.now().Add(delay).After(deadline) {
		return err
	}
	if serr := r.sleep(ctx, delay); serr != nil {
		return serr
	}
	return nil
}

// backoff is base * multiplier^(attempt-1), capped, then jittered by +/-Jitter.
func (r *Router) backoff(p RetryPolicy, attempt int) time.Duration {
	d := float64(p.BaseDelay) * math.Pow(p.Multiplier, float64(attempt-1))
	if d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	if p.Jitter > 0 {
		d *= 1 + p.Jitter*(2*r.rnd()-1)
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// order returns the targets in the order this request should try them.
//
// Every target appears, including ones whose breaker is open: the caller skips
// those, but leaving them in the list means a route whose breakers have all
// tripped still reports a specific failure per target rather than a bare "no
// targets", and it means a target that half-opens between selection and use is
// still tried.
func (r *Router) order(rt *route) []*target {
	n := len(rt.targets)
	if n == 1 {
		return rt.targets
	}
	switch rt.strategy {
	case StrategyRoundRobin:
		// The counter wraps after 2^64 requests; the modulus makes the
		// wraparound invisible, which is why the conversion is safe.
		start := int(rt.cursor.Add(1)-1) % n //nolint:gosec // modular arithmetic on a rotating cursor
		out := make([]*target, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, rt.targets[(start+i)%n])
		}
		return out
	case StrategyWeighted:
		return r.weightedOrder(rt.targets)
	default:
		return rt.targets
	}
}

// weightedOrder shuffles by weight without replacement, so that the first
// element is chosen with probability proportional to its weight and the rest
// remain a valid failover order.
func (r *Router) weightedOrder(targets []*target) []*target {
	pool := append([]*target(nil), targets...)
	out := make([]*target, 0, len(pool))
	total := 0
	for _, t := range pool {
		total += t.weight
	}
	for len(pool) > 0 {
		pick := r.rnd() * float64(total)
		idx := len(pool) - 1
		acc := 0.0
		for i, t := range pool {
			acc += float64(t.weight)
			if pick < acc {
				idx = i
				break
			}
		}
		out = append(out, pool[idx])
		total -= pool[idx].weight
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}

func (r *Router) observe(a Attempt) {
	if r.onAttempt != nil {
		r.onAttempt(a)
	}
}

func shortCircuitError(t *target) error {
	return &llm.Error{
		Code: llm.CodeProviderUnavailable, Provider: t.provider.Name(), Model: t.model,
		Message: "circuit breaker is open", Err: ErrNoTarget,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TargetStatus is one target's health, for the dashboard.
type TargetStatus struct {
	Alias    string       `json:"alias"`
	Provider string       `json:"provider"`
	Model    string       `json:"model"`
	Weight   int          `json:"weight"`
	Breaker  BreakerStats `json:"breaker"`
}

// Status returns every target's breaker state, in route declaration order.
func (r *Router) Status() []TargetStatus {
	out := make([]TargetStatus, 0, len(r.routes))
	for _, alias := range r.aliases {
		rt := r.routes[alias]
		for _, t := range rt.targets {
			out = append(out, TargetStatus{
				Alias: alias, Provider: t.provider.Name(), Model: t.model,
				Weight: t.weight, Breaker: t.breaker.Stats(),
			})
		}
	}
	return out
}
