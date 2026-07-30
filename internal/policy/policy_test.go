package policy

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// clock is a manual clock. Rate limits and rolling windows are entirely about
// the passage of time, and a test that asserts on them by sleeping is a test
// that is slow when it passes and flaky when it fails.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRequestLimitRefillsContinuously(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	limits := Limits{RequestsPerMinute: 60, Burst: 1}

	// The bucket starts full: 60 requests, then the 61st is refused.
	for i := 0; i < 60; i++ {
		require.NoError(t, rl.Allow("k", limits, 0), "request %d", i)
	}
	err := rl.Allow("k", limits, 0)
	require.Error(t, err)
	var le *LimitError
	require.ErrorAs(t, err, &le)
	assert.Equal(t, KindRequests, le.Kind)
	assert.InDelta(t, float64(time.Second), float64(le.RetryAfter), float64(50*time.Millisecond),
		"one token refills in one second at 60/min")

	c.advance(time.Second)
	require.NoError(t, rl.Allow("k", limits, 0), "a second's refill buys exactly one request")
	assert.Error(t, rl.Allow("k", limits, 0))
}

func TestTokenLimitIsSeparateFromTheRequestLimit(t *testing.T) {
	// The reason both exist: one enormous request is within any request limit
	// and can still exhaust a token budget.
	c := newClock()
	rl := NewRateLimiter(c.now)
	limits := Limits{RequestsPerMinute: 1000, TokensPerMinute: 1000, Burst: 1}

	require.NoError(t, rl.Allow("k", limits, 900))
	err := rl.Allow("k", limits, 200)
	require.Error(t, err)
	var le *LimitError
	require.ErrorAs(t, err, &le)
	assert.Equal(t, KindTokens, le.Kind)
}

func TestRejectedRequestDoesNotConsumeTheRequestToken(t *testing.T) {
	// A client that trips the token limit must not also be charged a request,
	// or the request limit becomes stricter than the one configured.
	c := newClock()
	rl := NewRateLimiter(c.now)
	limits := Limits{RequestsPerMinute: 2, TokensPerMinute: 100, Burst: 1}

	require.Error(t, rl.Allow("k", limits, 1000), "larger than the whole token bucket")
	require.NoError(t, rl.Allow("k", limits, 10))
	require.NoError(t, rl.Allow("k", limits, 10))
	assert.Error(t, rl.Allow("k", limits, 10), "exactly two requests were allowed, not one")
}

func TestRequestLargerThanTheBucketIsPermanentlyRefused(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	err := rl.Allow("k", Limits{TokensPerMinute: 100, Burst: 1}, 500)
	var le *LimitError
	require.ErrorAs(t, err, &le)
	assert.Zero(t, le.RetryAfter, "no amount of waiting makes this request fit; the client must change it")
	assert.Contains(t, le.Error(), "entire")
}

func TestBurstDeepensTheBucketWithoutChangingTheRate(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	limits := Limits{RequestsPerMinute: 10, Burst: 3}
	for i := 0; i < 30; i++ {
		require.NoError(t, rl.Allow("k", limits, 0), "burst 3 allows 3 minutes' worth up front, request %d", i)
	}
	assert.Error(t, rl.Allow("k", limits, 0))
}

func TestSettleReconcilesTheEstimate(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	limits := Limits{TokensPerMinute: 1000, Burst: 1}

	require.NoError(t, rl.Allow("k", limits, 100))
	// The response actually used 600, so 500 more must be charged.
	rl.Settle("k", 100, 600)
	require.NoError(t, rl.Allow("k", limits, 300))
	require.Error(t, rl.Allow("k", limits, 300), "900 of the 1000 are gone")

	// And an over-estimate is given back.
	c.advance(time.Hour)
	require.NoError(t, rl.Allow("k", limits, 900))
	rl.Settle("k", 900, 100)
	assert.NoError(t, rl.Allow("k", limits, 800))
}

func TestChangingLimitsTakesEffectImmediately(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	require.NoError(t, rl.Allow("k", Limits{RequestsPerMinute: 100, Burst: 1}, 0))
	// Reconfigured downward: the new, smaller bucket applies now rather than
	// once the old one has drained.
	tight := Limits{RequestsPerMinute: 1, Burst: 1}
	require.NoError(t, rl.Allow("k", tight, 0))
	assert.Error(t, rl.Allow("k", tight, 0))
}

func TestUnlimitedSubjectsAreNeverRefused(t *testing.T) {
	rl := NewRateLimiter(newClock().now)
	for i := 0; i < 1000; i++ {
		require.NoError(t, rl.Allow("k", Limits{}, 1_000_000))
	}
	assert.Zero(t, rl.Subjects(), "no buckets are allocated for a subject with no limits")
}

func TestSweepDropsIdleSubjects(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	require.NoError(t, rl.Allow("k", Limits{RequestsPerMinute: 10, Burst: 1}, 0))
	assert.Equal(t, 1, rl.Subjects())
	assert.Zero(t, rl.Sweep(), "not idle yet")
	c.advance(time.Hour)
	assert.Equal(t, 1, rl.Sweep())
	assert.Zero(t, rl.Subjects())
}

// --- budgets ----------------------------------------------------------------

func budget(limitUSD float64, window time.Duration, onExceed, degradeTo string) Budget {
	return Budget{
		Limit:  llm.Cost(limitUSD * float64(llm.Dollar)),
		Window: window, OnExceed: onExceed, DegradeTo: degradeTo,
	}
}

func TestBudgetAllowsUntilTheLimitIsReached(t *testing.T) {
	c := newClock()
	b := NewBudgets(c.now)
	bud := budget(1.00, time.Hour, "reject", "")

	assert.Equal(t, ActionAllow, b.Check("key:a", bud, "big").Action)
	b.Record("key:a", bud.Window, 90*llm.Millidollar)
	assert.Equal(t, ActionAllow, b.Check("key:a", bud, "big").Action, "90c of a dollar is still under")
	b.Record("key:a", bud.Window, 950*llm.Millidollar)

	d := b.Check("key:a", bud, "big")
	assert.Equal(t, ActionReject, d.Action)
	assert.Equal(t, "key:a", d.Subject)
	assert.Greater(t, d.ResetsIn, time.Duration(0), "a rejection says when it would stop being one")
	assert.Contains(t, (&ExceededError{Decision: d}).Error(), "key:a")
}

func TestBudgetDegradesToTheCheaperRoute(t *testing.T) {
	c := newClock()
	b := NewBudgets(c.now)
	bud := budget(0.01, time.Hour, "degrade", "cheap")
	b.Record("key:a", bud.Window, 20*llm.Millidollar)

	d := b.Check("key:a", bud, "expensive")
	assert.Equal(t, ActionDegrade, d.Action)
	assert.Equal(t, "cheap", d.Model, "the request proceeds, on a cheaper model, and says so")
}

func TestDegradingToTheSameModelBecomesARejection(t *testing.T) {
	// Otherwise the limit would silently stop existing for anyone who happened
	// to request the degrade target directly.
	c := newClock()
	b := NewBudgets(c.now)
	bud := budget(0.01, time.Hour, "degrade", "cheap")
	b.Record("key:a", bud.Window, 20*llm.Millidollar)
	assert.Equal(t, ActionReject, b.Check("key:a", bud, "cheap").Action)
}

func TestRollingWindowExpiresOldSpend(t *testing.T) {
	c := newClock()
	b := NewBudgets(c.now)
	window := 2 * time.Hour
	bud := budget(1.00, window, "reject", "")

	b.Record("key:a", window, 900*llm.Millidollar)
	assert.Equal(t, 900*llm.Millidollar, b.Spent("key:a", window))

	// Half a window later the old spend is still inside it.
	c.advance(time.Hour)
	assert.Equal(t, 900*llm.Millidollar, b.Spent("key:a", window))
	b.Record("key:a", window, 200*llm.Millidollar)
	assert.Equal(t, ActionReject, b.Check("key:a", bud, "m").Action)

	// Past the full window from the first charge, only the second survives.
	c.advance(time.Hour + time.Minute)
	assert.Equal(t, 200*llm.Millidollar, b.Spent("key:a", window))
	assert.Equal(t, ActionAllow, b.Check("key:a", bud, "m").Action)

	c.advance(2 * time.Hour)
	assert.Zero(t, b.Spent("key:a", window), "everything has aged out")
}

func TestRollingWindowGranularityIsBounded(t *testing.T) {
	// The slot approximation is the documented trade: memory is bounded at
	// bucketCount slots, and the error is at most one slot's width. This pins
	// that down rather than leaving it as a claim in a comment.
	c := newClock()
	b := NewBudgets(c.now)
	window := time.Duration(bucketCount) * time.Minute // one slot per minute

	b.Record("key:a", window, llm.Dollar)
	c.advance(window - time.Minute)
	assert.Equal(t, llm.Dollar, b.Spent("key:a", window), "still inside the window")
	c.advance(2 * time.Minute)
	assert.Zero(t, b.Spent("key:a", window), "and out of it within one slot of the exact boundary")
}

func TestSpendSeriesMatchesTheLedger(t *testing.T) {
	c := newClock()
	b := NewBudgets(c.now)
	window := time.Duration(bucketCount) * time.Minute

	b.Record("key:a", window, 10*llm.Millidollar)
	c.advance(time.Minute)
	b.Record("key:a", window, 20*llm.Millidollar)

	s := b.SpendSeries("key:a", 5)
	require.Len(t, s.Points, 5)
	assert.Equal(t, 20*llm.Millidollar, s.Points[4], "newest slot last")
	assert.Equal(t, 10*llm.Millidollar, s.Points[3])

	var total llm.Cost
	for _, p := range s.Points {
		total += p
	}
	assert.Equal(t, b.Spent("key:a", window), total, "the chart cannot disagree with the enforcement")
}

func TestBudgetSweepDropsEmptyLedgers(t *testing.T) {
	c := newClock()
	b := NewBudgets(c.now)
	window := time.Hour
	b.Record("key:a", window, llm.Dollar)
	assert.Zero(t, b.Sweep(), "not empty yet")
	c.advance(3 * time.Hour)
	assert.Equal(t, 1, b.Sweep())
	assert.Empty(t, b.Snapshots())
}

func TestZeroBudgetIsNoBudget(t *testing.T) {
	b := NewBudgets(newClock().now)
	assert.Equal(t, ActionAllow, b.Check("key:a", Budget{}, "m").Action)
	assert.True(t, Budget{Window: time.Hour}.Zero())
	assert.True(t, Budget{Limit: llm.Dollar}.Zero(), "a limit without a window bounds nothing")
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	c := newClock()
	rl := NewRateLimiter(c.now)
	b := NewBudgets(c.now)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = rl.Allow("k", Limits{RequestsPerMinute: 10000, TokensPerMinute: 1e6, Burst: 2}, 10)
				rl.Settle("k", 10, 12)
				b.Record("key:k", time.Hour, llm.Microdollar)
				b.Check("key:k", budget(1000, time.Hour, "reject", ""), "m")
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, llm.Cost(16*50)*llm.Microdollar, b.Spent("key:k", time.Hour))
}
