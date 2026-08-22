package router

import (
	"sync"
	"time"

	"github.com/Liona-orph/sluice/pkg/llm"
)

// State is a circuit breaker's state.
type State string

const (
	// StateClosed passes traffic. The normal state.
	StateClosed State = "closed"
	// StateOpen refuses traffic without calling the target at all. The point is
	// not to protect the target -- it is already failing -- but to stop the
	// gateway paying a full timeout per request to discover something it
	// already knows, and to fail over to a healthy target immediately.
	StateOpen State = "open"
	// StateHalfOpen allows a bounded number of trial requests through. Bounded,
	// because the alternative -- reopening the floodgates on a timer -- sends
	// the entire backlog at an upstream that has had a few seconds to breathe
	// and knocks it straight back over.
	StateHalfOpen State = "half_open"
)

// BreakerConfig is the tuning for one breaker.
//
// The defaults elsewhere in the repository are 5 consecutive failures to open,
// 30 seconds open, 1 concurrent probe, 2 consecutive successes to close. The
// reasoning:
//
//   - Five consecutive failures, not a failure rate. A rate needs a window long
//     enough to be significant, and any window long enough is longer than an
//     operator wants to keep feeding a dead upstream. Five is short enough to
//     react within a second at any real request rate and long enough that a
//     single unlucky timeout does not trip it.
//   - Thirty seconds open. Long enough that a restarting upstream has actually
//     restarted, short enough that a transient blip does not remove a provider
//     from rotation for a noticeable fraction of an incident.
//   - One probe. A probe that fails costs one request's latency; a hundred
//     concurrent probes that fail cost a hundred, and arrive as a spike.
//   - Two successes to close. One success can be a health-check endpoint or a
//     cached response; two consecutive ones through the real path is weak
//     evidence, but it is evidence.
type BreakerConfig struct {
	FailureThreshold int
	SuccessThreshold int
	OpenFor          time.Duration
	HalfOpenProbes   int
}

// withDefaults fills unset fields, so that a zero BreakerConfig is a working
// breaker rather than one that opens on the first failure and never closes.
func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = 2
	}
	if c.OpenFor <= 0 {
		c.OpenFor = 30 * time.Second
	}
	if c.HalfOpenProbes <= 0 {
		c.HalfOpenProbes = 1
	}
	return c
}

// Breaker is a per-target circuit breaker. It is safe for concurrent use.
type Breaker struct {
	cfg BreakerConfig
	now func() time.Time

	mu             sync.Mutex
	state          State
	failures       int
	successes      int
	openedAt       time.Time
	inFlight       int
	changedAt      time.Time
	trips          uint64
	shortCircuited uint64
}

// NewBreaker returns a closed breaker.
func NewBreaker(cfg BreakerConfig, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{cfg: cfg.withDefaults(), now: now, state: StateClosed, changedAt: now()}
}

// Allow reports whether a request may be attempted. Every true must be paired
// with exactly one Record, or a half-open breaker leaks probe slots and never
// recovers.
func (b *Breaker) Allow() bool {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if now.Sub(b.openedAt) < b.cfg.OpenFor {
			b.shortCircuited++
			return false
		}
		b.transition(StateHalfOpen, now)
		b.inFlight = 1
		return true
	case StateHalfOpen:
		if b.inFlight >= b.cfg.HalfOpenProbes {
			b.shortCircuited++
			return false
		}
		b.inFlight++
		return true
	}
	return true
}

// Record reports the outcome of an attempt that Allow permitted.
//
// Only errors that say something about the target's health count. A malformed
// request, an oversized context or a content-filter refusal are facts about the
// request, and letting them open a circuit would let one bad client remove a
// healthy provider for everyone else. llm.ErrorCode already draws this line for
// failover; a breaker wants the same line for the same reason.
func (b *Breaker) Record(err error) {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateHalfOpen && b.inFlight > 0 {
		b.inFlight--
	}

	if err == nil || !unhealthy(llm.CodeOf(err)) {
		b.onSuccess(now)
		return
	}
	b.onFailure(now)
}

// unhealthy reports whether an error code is evidence about the target rather
// than about the request.
func unhealthy(code llm.ErrorCode) bool {
	switch code {
	case llm.CodeProviderUnavailable, llm.CodeTimeout, llm.CodeRateLimited, llm.CodeAuthentication:
		return true
	default:
		// Everything else is a fact about the request. Letting one bad client
		// open a circuit would remove a healthy provider for everyone else.
		return false
	}
}

func (b *Breaker) onSuccess(now time.Time) {
	switch b.state {
	case StateClosed:
		b.failures = 0
	case StateHalfOpen:
		b.successes++
		if b.successes >= b.cfg.SuccessThreshold {
			b.transition(StateClosed, now)
		}
	case StateOpen:
		// A success recorded against an open breaker means a probe raced the
		// transition. Harmless; the half-open path will decide.
	}
}

func (b *Breaker) onFailure(now time.Time) {
	switch b.state {
	case StateHalfOpen:
		// One failed probe is enough. The upstream said it is still broken and
		// there is no reason to ask again before the next cooldown.
		b.openedAt = now
		b.transition(StateOpen, now)
	case StateClosed:
		b.failures++
		if b.failures >= b.cfg.FailureThreshold {
			b.openedAt = now
			b.transition(StateOpen, now)
		}
	case StateOpen:
	}
}

// transition moves state and resets the counters that belong to the state being
// left. The caller holds the lock.
func (b *Breaker) transition(to State, now time.Time) {
	if b.state == to {
		return
	}
	if to == StateOpen {
		b.trips++
	}
	b.state = to
	b.changedAt = now
	b.failures = 0
	b.successes = 0
	b.inFlight = 0
}

// State returns the current state, advancing an expired open circuit to
// half-open so that a status read is not stale.
func (b *Breaker) State() State {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && now.Sub(b.openedAt) >= b.cfg.OpenFor {
		return StateHalfOpen
	}
	return b.state
}

// BreakerStats is a snapshot for the dashboard and the stats endpoint.
type BreakerStats struct {
	State State `json:"state"`
	// ConsecutiveFailures is the current run, reset by any success.
	ConsecutiveFailures int `json:"consecutive_failures"`
	// Trips counts how many times the circuit has opened since startup. A
	// target that trips repeatedly and recovers is a different problem from one
	// that trips once and stays open, and the state alone does not distinguish
	// them.
	Trips uint64 `json:"trips"`
	// ShortCircuited counts requests refused without an upstream call.
	ShortCircuited uint64     `json:"short_circuited"`
	ChangedAt      time.Time  `json:"changed_at"`
	OpenUntil      *time.Time `json:"open_until,omitempty"`
}

// Stats returns a snapshot.
func (b *Breaker) Stats() BreakerStats {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	s := BreakerStats{
		State:               b.state,
		ConsecutiveFailures: b.failures,
		Trips:               b.trips,
		ShortCircuited:      b.shortCircuited,
		ChangedAt:           b.changedAt,
	}
	if b.state == StateOpen {
		until := b.openedAt.Add(b.cfg.OpenFor)
		if now.Before(until) {
			s.OpenUntil = &until
		} else {
			s.State = StateHalfOpen
		}
	}
	return s
}
