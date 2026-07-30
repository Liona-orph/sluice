package router

import (
	"context"
	"errors"
	"iter"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/internal/leaktest"
	"github.com/sluice-gw/sluice/pkg/llm"
)

// fakeProvider is scripted rather than random: a routing test is about which
// upstream was asked and in what order, and a provider that generates text
// would only add noise to that assertion.
type fakeProvider struct {
	name string
	mu   sync.Mutex
	// results is consumed one per Complete/Stream call; the last entry repeats
	// once exhausted, so a test can say "fails twice then succeeds" without
	// counting how many attempts the policy will make.
	results []error
	calls   atomic.Int64
	// chunks is what a successful stream yields.
	chunks []string
	// streamErrAfter, when >= 0, makes the sequence fail after that many chunks.
	streamErrAfter int
	streamErr      error
}

func newFake(name string, results ...error) *fakeProvider {
	return &fakeProvider{name: name, results: results, chunks: []string{"hello "}, streamErrAfter: -1}
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) next() error {
	n := int(f.calls.Add(1)) - 1
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.results) == 0 {
		return nil
	}
	if n >= len(f.results) {
		return f.results[len(f.results)-1]
	}
	return f.results[n]
}

func (f *fakeProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	if err := f.next(); err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		ID: f.name + "-1", Model: req.Model, Provider: f.name,
		Message: llm.Message{Role: llm.RoleAssistant, Content: "from " + f.name},
		Usage:   llm.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func (f *fakeProvider) Stream(_ context.Context, req llm.Request) (iter.Seq2[llm.Chunk, error], error) {
	if err := f.next(); err != nil {
		return nil, err
	}
	return func(yield func(llm.Chunk, error) bool) {
		for i, c := range f.chunks {
			if f.streamErrAfter >= 0 && i == f.streamErrAfter {
				yield(llm.Chunk{}, f.streamErr)
				return
			}
			if !yield(llm.Chunk{ID: f.name, Model: req.Model, Provider: f.name,
				Delta: llm.Delta{Content: c}}, nil) {
				return
			}
		}
		if f.streamErrAfter >= 0 && f.streamErrAfter >= len(f.chunks) {
			yield(llm.Chunk{}, f.streamErr)
			return
		}
		usage := llm.Usage{InputTokens: 1, OutputTokens: 1}
		yield(llm.Chunk{ID: f.name, Model: req.Model, Provider: f.name,
			FinishReason: llm.FinishStop, Usage: &usage}, nil)
	}, nil
}

func unavailable(provider string) error {
	return &llm.Error{Code: llm.CodeProviderUnavailable, Provider: provider, Message: "down"}
}

func invalid(provider string) error {
	return &llm.Error{Code: llm.CodeInvalidRequest, Provider: provider, Message: "no"}
}

// testRouter builds a router with a deterministic clock, no real sleeping, and
// a fixed jitter source, so that a backoff schedule is assertable.
func testRouter(t *testing.T, spec RouteSpec, opts ...func(*Options)) (*Router, *[]time.Duration) {
	t.Helper()
	var slept []time.Duration
	o := Options{
		Routes: []RouteSpec{spec},
		Now:    time.Now,
		Rand:   func() float64 { return 0.5 },
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	for _, f := range opts {
		f(&o)
	}
	r, err := New(o)
	require.NoError(t, err)
	return r, &slept
}

func chat(model string) llm.Request {
	return llm.Request{Model: model, Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}
}

func TestRoutesToTheFirstTarget(t *testing.T) {
	a, b := newFake("a"), newFake("b")
	r, _ := testRouter(t, RouteSpec{
		Alias: "alias", Strategy: StrategyPriority,
		Targets: []TargetSpec{{Provider: a, Model: "m-a"}, {Provider: b, Model: "m-b"}},
	})

	resp, out, err := r.Complete(context.Background(), chat("alias"))
	require.NoError(t, err)
	assert.Equal(t, "a", resp.Provider)
	assert.Equal(t, "m-a", resp.Model, "the physical model replaces the alias before the provider sees it")
	assert.Equal(t, 1, out.Attempts)
	assert.Zero(t, out.Failovers)
	assert.EqualValues(t, 0, b.calls.Load())
}

func TestUnknownAliasIsAnError(t *testing.T) {
	r, _ := testRouter(t, RouteSpec{Alias: "alias", Targets: []TargetSpec{{Provider: newFake("a")}}})
	_, _, err := r.Complete(context.Background(), chat("nope"))
	assert.ErrorIs(t, err, ErrNoRoute)
}

func TestRetriesThenSucceedsOnTheSameTarget(t *testing.T) {
	a := newFake("a", unavailable("a"), unavailable("a"), nil)
	r, slept := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}},
		Retry:   RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Multiplier: 2},
		Breaker: BreakerConfig{FailureThreshold: 10},
	})

	resp, out, err := r.Complete(context.Background(), chat("alias"))
	require.NoError(t, err)
	assert.Equal(t, "a", resp.Provider)
	assert.Equal(t, 3, out.Attempts)
	// 100ms then 200ms: base * multiplier^(attempt-1), with jitter fixed at the
	// midpoint so the schedule is exact.
	assert.Equal(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, *slept)
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	r, _ := testRouter(t, RouteSpec{Alias: "a", Targets: []TargetSpec{{Provider: newFake("a")}}})
	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: 4 * time.Second, Multiplier: 3, Jitter: 0.5}

	// rnd is fixed at 0.5, i.e. no net jitter, so the cap is visible.
	assert.Equal(t, time.Second, r.backoff(p, 1))
	assert.Equal(t, 3*time.Second, r.backoff(p, 2))
	assert.Equal(t, 4*time.Second, r.backoff(p, 3), "capped at MaxDelay")

	lo, _ := testRouter(t, RouteSpec{Alias: "a", Targets: []TargetSpec{{Provider: newFake("a")}}},
		func(o *Options) { o.Rand = func() float64 { return 0 } })
	hi, _ := testRouter(t, RouteSpec{Alias: "a", Targets: []TargetSpec{{Provider: newFake("a")}}},
		func(o *Options) { o.Rand = func() float64 { return 1 } })
	assert.Equal(t, 500*time.Millisecond, lo.backoff(p, 1), "jitter 0.5 at the low end halves the delay")
	assert.Equal(t, 1500*time.Millisecond, hi.backoff(p, 1), "and at the high end adds half")
}

func TestRetryAfterOverridesTheComputedBackoff(t *testing.T) {
	limited := &llm.Error{Code: llm.CodeRateLimited, Provider: "a", RetryAfter: 7 * time.Second}
	a := newFake("a", limited, nil)
	r, slept := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}},
		Retry:   RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond, Multiplier: 2},
	})
	_, _, err := r.Complete(context.Background(), chat("alias"))
	require.NoError(t, err)
	require.Len(t, *slept, 1)
	assert.Equal(t, 7*time.Second, (*slept)[0],
		"the provider's Retry-After wins even over MaxDelay; retrying sooner than asked is how a rate limit becomes a ban")
}

func TestRetryDoesNotOutlastTheCallerDeadline(t *testing.T) {
	limited := &llm.Error{Code: llm.CodeRateLimited, Provider: "a", RetryAfter: time.Hour}
	a := newFake("a", limited, nil)
	r, slept := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}},
		Retry:   RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Multiplier: 1},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, out, err := r.Complete(ctx, chat("alias"))
	require.Error(t, err)
	assert.Empty(t, *slept, "a wait longer than the deadline is not waited out")
	assert.Equal(t, 1, out.Attempts)
}

func TestFailsOverOnlyOnFailoverWorthyCodes(t *testing.T) {
	t.Run("provider_unavailable fails over", func(t *testing.T) {
		a, b := newFake("a", unavailable("a")), newFake("b")
		r, _ := testRouter(t, RouteSpec{
			Alias:   "alias",
			Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
		})
		resp, out, err := r.Complete(context.Background(), chat("alias"))
		require.NoError(t, err)
		assert.Equal(t, "b", resp.Provider)
		assert.Equal(t, 1, out.Failovers)
	})

	t.Run("invalid_request does not", func(t *testing.T) {
		a, b := newFake("a", invalid("a")), newFake("b")
		r, _ := testRouter(t, RouteSpec{
			Alias:   "alias",
			Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
		})
		_, out, err := r.Complete(context.Background(), chat("alias"))
		require.Error(t, err)
		assert.Equal(t, llm.CodeInvalidRequest, llm.CodeOf(err))
		assert.Zero(t, out.Failovers)
		assert.EqualValues(t, 0, b.calls.Load(), "a malformed request will be malformed at the second provider too")
	})
}

func TestRoundRobinSpreadsAcrossTargets(t *testing.T) {
	a, b, c := newFake("a"), newFake("b"), newFake("c")
	r, _ := testRouter(t, RouteSpec{
		Alias: "alias", Strategy: StrategyRoundRobin,
		Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}, {Provider: c, Model: "m"}},
	})
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		resp, _, err := r.Complete(context.Background(), chat("alias"))
		require.NoError(t, err)
		seen[resp.Provider]++
	}
	assert.Equal(t, map[string]int{"a": 3, "b": 3, "c": 3}, seen)
}

func TestWeightedSelectionFollowsTheWeights(t *testing.T) {
	a, b := newFake("a"), newFake("b")
	// A seeded PRNG rather than the global one: the split is statistical, and a
	// statistical assertion on an unseeded source is a flake waiting for a bad
	// afternoon.
	prng := rand.New(rand.NewPCG(1, 2))
	r, _ := testRouter(t, RouteSpec{
		Alias: "alias", Strategy: StrategyWeighted,
		Targets: []TargetSpec{{Provider: a, Model: "m", Weight: 9}, {Provider: b, Model: "m", Weight: 1}},
	}, func(o *Options) { o.Rand = prng.Float64 })

	const n = 2000
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		resp, _, err := r.Complete(context.Background(), chat("alias"))
		require.NoError(t, err)
		seen[resp.Provider]++
	}
	// Expected 1800/200. The bounds are wide enough that only a broken weighting
	// fails them and narrow enough that an unweighted router does.
	assert.Greater(t, seen["a"], 1700, "the 9:1 weight should send ~90%% to a: %v", seen)
	assert.Less(t, seen["a"], 1900, "but not all of it: %v", seen)
}

func TestBreakerOpensAndShortCircuits(t *testing.T) {
	a, b := newFake("a", unavailable("a")), newFake("b")
	r, _ := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
		Breaker: BreakerConfig{FailureThreshold: 2, OpenFor: time.Hour, SuccessThreshold: 1, HalfOpenProbes: 1},
	})

	for i := 0; i < 2; i++ {
		_, _, err := r.Complete(context.Background(), chat("alias"))
		require.NoError(t, err)
	}
	before := a.calls.Load()

	_, out, err := r.Complete(context.Background(), chat("alias"))
	require.NoError(t, err)
	assert.Equal(t, 1, out.ShortCircuits)
	assert.Equal(t, before, a.calls.Load(), "an open circuit does not call the provider at all")

	status := r.Status()
	require.Len(t, status, 2)
	assert.Equal(t, StateOpen, status[0].Breaker.State)
	assert.EqualValues(t, 1, status[0].Breaker.Trips)
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	a := newFake("a", unavailable("a"), unavailable("a"), nil)
	r, err := New(Options{
		Routes: []RouteSpec{{
			Alias:   "alias",
			Targets: []TargetSpec{{Provider: a, Model: "m"}},
			Breaker: BreakerConfig{FailureThreshold: 2, OpenFor: 30 * time.Second, SuccessThreshold: 1, HalfOpenProbes: 1},
		}},
		Now:   clock,
		Rand:  func() float64 { return 0.5 },
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		_, _, cerr := r.Complete(context.Background(), chat("alias"))
		require.Error(t, cerr)
	}
	assert.Equal(t, StateOpen, r.Status()[0].Breaker.State)

	_, out, cerr := r.Complete(context.Background(), chat("alias"))
	require.Error(t, cerr)
	assert.Equal(t, 1, out.ShortCircuits, "still open before the cooldown elapses")

	now = now.Add(31 * time.Second)
	_, _, cerr = r.Complete(context.Background(), chat("alias"))
	require.NoError(t, cerr, "the probe is allowed once the cooldown has passed")
	assert.Equal(t, StateClosed, r.Status()[0].Breaker.State)
}

func TestBreakerIgnoresClientErrors(t *testing.T) {
	// A malformed request must not be able to remove a healthy provider for
	// everyone else.
	a := newFake("a", invalid("a"))
	r, _ := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}},
		Breaker: BreakerConfig{FailureThreshold: 2, OpenFor: time.Hour, SuccessThreshold: 1, HalfOpenProbes: 1},
	})
	for i := 0; i < 5; i++ {
		_, _, err := r.Complete(context.Background(), chat("alias"))
		require.Error(t, err)
	}
	assert.Equal(t, StateClosed, r.Status()[0].Breaker.State)
}

func TestStreamFailsOverBeforeTheFirstToken(t *testing.T) {
	defer leaktest.Check(t)()

	a := newFake("a")
	a.streamErrAfter, a.streamErr = 0, unavailable("a") // fails on the first element
	b := newFake("b")
	b.chunks = []string{"good ", "answer"}

	r, _ := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
	})

	seq, out, err := r.Stream(context.Background(), chat("alias"))
	require.NoError(t, err)

	var text string
	for chunk, cerr := range seq {
		require.NoError(t, cerr)
		text += chunk.Delta.Content
	}
	assert.Equal(t, "good answer", text)
	assert.Equal(t, 1, out.Failovers)
	assert.Equal(t, "b", out.Provider)
}

// TestStreamNeverFailsOverAfterFirstToken is the enforcement of the rule stated
// on Router.Stream. A prefix of the answer is already on the client's screen;
// splicing a second provider's answer onto it produces a response that
// contradicts itself with no marker of the seam.
func TestStreamNeverFailsOverAfterFirstToken(t *testing.T) {
	defer leaktest.Check(t)()

	a := newFake("a")
	a.chunks = []string{"half an "}
	a.streamErrAfter, a.streamErr = 1, unavailable("a") // fails after one chunk
	b := newFake("b")

	r, _ := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
	})

	seq, out, err := r.Stream(context.Background(), chat("alias"))
	require.NoError(t, err)

	var text string
	var streamErr error
	for chunk, cerr := range seq {
		if cerr != nil {
			streamErr = cerr
			break
		}
		text += chunk.Delta.Content
	}
	require.Error(t, streamErr)
	assert.Equal(t, "half an ", text, "the client keeps what it was given")
	assert.Zero(t, out.Failovers)
	assert.EqualValues(t, 0, b.calls.Load(), "the standby is never asked once output has been emitted")
}

func TestStreamAbandonedEarlyLeaksNothing(t *testing.T) {
	defer leaktest.Check(t)()

	a := newFake("a")
	a.chunks = []string{"one ", "two ", "three ", "four "}
	r, _ := testRouter(t, RouteSpec{Alias: "alias", Targets: []TargetSpec{{Provider: a, Model: "m"}}})

	seq, _, err := r.Stream(context.Background(), chat("alias"))
	require.NoError(t, err)
	for range seq {
		break // walk away after the first chunk, as a disconnecting client does
	}
}

func TestStreamOuterErrorWhenEveryTargetRefuses(t *testing.T) {
	a := newFake("a", unavailable("a"))
	b := newFake("b", unavailable("b"))
	r, _ := testRouter(t, RouteSpec{
		Alias:   "alias",
		Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
	})
	seq, _, err := r.Stream(context.Background(), chat("alias"))
	require.Error(t, err, "nothing has been written to the client, so this is still a clean failure")
	assert.Nil(t, seq)
	assert.Equal(t, llm.CodeProviderUnavailable, llm.CodeOf(err))
}

func TestNewRejectsBadSpecs(t *testing.T) {
	_, err := New(Options{})
	require.Error(t, err)

	_, err = New(Options{Routes: []RouteSpec{{Alias: "a"}}})
	require.ErrorContains(t, err, "no targets")

	_, err = New(Options{Routes: []RouteSpec{
		{Alias: "a", Targets: []TargetSpec{{Provider: newFake("p")}}},
		{Alias: "a", Targets: []TargetSpec{{Provider: newFake("p")}}},
	}})
	require.ErrorContains(t, err, "duplicate")

	_, err = New(Options{Routes: []RouteSpec{{Alias: "a", Targets: []TargetSpec{{}}}}})
	assert.ErrorContains(t, err, "no provider")
}

func TestTargetModelDefaultsToTheAlias(t *testing.T) {
	a := newFake("a")
	r, _ := testRouter(t, RouteSpec{Alias: "gpt-4o", Targets: []TargetSpec{{Provider: a}}})
	resp, _, err := r.Complete(context.Background(), chat("gpt-4o"))
	require.NoError(t, err)
	assert.Equal(t, "gpt-4o", resp.Model)
}

func TestConcurrentRoutingIsRaceFree(t *testing.T) {
	a, b := newFake("a"), newFake("b")
	r, _ := testRouter(t, RouteSpec{
		Alias: "alias", Strategy: StrategyRoundRobin,
		Targets: []TargetSpec{{Provider: a, Model: "m"}, {Provider: b, Model: "m"}},
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := r.Complete(context.Background(), chat("alias")); err != nil {
				t.Error(err)
			}
			seq, _, err := r.Stream(context.Background(), chat("alias"))
			if err != nil {
				t.Error(err)
				return
			}
			for _, cerr := range seq {
				if cerr != nil && !errors.Is(cerr, context.Canceled) {
					t.Error(cerr)
				}
			}
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 64, a.calls.Load()+b.calls.Load())
}
