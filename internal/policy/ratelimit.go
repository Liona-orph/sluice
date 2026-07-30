// Package policy decides what a caller is allowed to do before anything is
// spent on their behalf: how fast they may ask, and how much they may spend.
//
// The two are separate mechanisms because they answer different questions and
// fail differently. A rate limit protects the upstream and the gateway from a
// caller in a loop; it is cheap, it is per-instant, and exceeding it is a
// normal condition a client should retry. A budget protects the invoice; it is
// per-window, it is the slow variable, and exceeding it is a business event
// rather than a transient one. Conflating them produces a limiter that either
// lets a single enormous request blow the month's budget or rejects a burst of
// trivial ones on cost grounds.
//
// Both are in-memory and per-process. See "What this is not" in the README: two
// Sluice instances behind a load balancer enforce two independent halves of a
// limit, and neither knows about the other.
package policy

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// bucket is a token bucket: a continuously refilling allowance with a cap.
//
// Continuous refill rather than a fixed window, because a fixed window lets a
// caller spend the whole allowance in the last instant of one window and again
// in the first instant of the next -- twice the configured rate across the
// boundary, which is exactly when an upstream is least able to absorb it.
type bucket struct {
	capacity float64
	// refillPerSecond is the steady-state rate.
	refillPerSecond float64
	tokens          float64
	last            time.Time
}

func newBucket(perMinute, burst float64, now time.Time) *bucket {
	if burst < 1 {
		burst = 1
	}
	capacity := perMinute * burst
	return &bucket{
		capacity:        capacity,
		refillPerSecond: perMinute / 60,
		tokens:          capacity,
		last:            now,
	}
}

// take removes n tokens if they are available, reporting success and, on
// failure, how long the caller should wait for them to exist.
//
// The wait is computed rather than left to the caller to guess, because it is
// the only number that makes a 429 actionable. It is also the number that goes
// into Retry-After.
func (b *bucket) take(n float64, now time.Time) (bool, time.Duration) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+elapsed.Seconds()*b.refillPerSecond)
		b.last = now
	}
	if n > b.capacity {
		// A single request larger than the whole bucket can never succeed. It
		// is reported as a permanent rejection rather than a retry-after of
		// infinity, so that a client is told to change the request rather than
		// to wait forever.
		return false, 0
	}
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	deficit := n - b.tokens
	return false, time.Duration(deficit / b.refillPerSecond * float64(time.Second))
}

// Limits is one subject's configured allowance.
type Limits struct {
	RequestsPerMinute float64
	TokensPerMinute   float64
	// Burst is the bucket depth as a multiple of one minute's refill.
	Burst float64
}

// Zero reports whether nothing is limited.
func (l Limits) Zero() bool { return l.RequestsPerMinute <= 0 && l.TokensPerMinute <= 0 }

// Kind names which limit was hit, so that a rejection can say which knob to
// turn rather than just "rate limited".
type Kind string

const (
	// KindRequests is the request-per-minute bucket.
	KindRequests Kind = "requests"
	// KindTokens is the token-per-minute bucket.
	KindTokens Kind = "tokens"
)

// LimitError reports a rejected request.
type LimitError struct {
	Subject string
	Kind    Kind
	// RetryAfter is how long until the request would succeed, or zero if it
	// never would.
	RetryAfter time.Duration
	// Limit is the configured allowance that was exceeded, for the message.
	Limit float64
}

func (e *LimitError) Error() string {
	if e.RetryAfter == 0 {
		return fmt.Sprintf("rate limit: %s exceeds the entire %s allowance of %.0f/min", e.Subject, e.Kind, e.Limit)
	}
	return fmt.Sprintf("rate limit: %s exceeded %s limit of %.0f/min; retry after %s",
		e.Subject, e.Kind, e.Limit, e.RetryAfter.Round(time.Millisecond))
}

// RateLimiter holds one pair of buckets per subject.
//
// It is safe for concurrent use behind a single mutex. A sharded map would
// scale further; the critical section here is a few floating-point operations
// and a map lookup, and a gateway that is contended on this lock is a gateway
// doing hundreds of thousands of requests per second, which is not the shape
// this is built for.
type RateLimiter struct {
	mu       sync.Mutex
	subjects map[string]*subjectBuckets
	now      func() time.Time
	// idleTTL bounds memory. A subject that has not been seen for this long is
	// dropped, which resets its bucket to full -- harmless, because a subject
	// idle for that long has refilled anyway.
	idleTTL time.Duration
}

type subjectBuckets struct {
	limits   Limits
	requests *bucket
	tokens   *bucket
	lastSeen time.Time
}

// NewRateLimiter returns a limiter. now may be nil for time.Now.
func NewRateLimiter(now func() time.Time) *RateLimiter {
	if now == nil {
		now = time.Now
	}
	return &RateLimiter{
		subjects: map[string]*subjectBuckets{},
		now:      now,
		idleTTL:  10 * time.Minute,
	}
}

// Allow charges one request and estimatedTokens tokens against subject.
//
// Tokens are charged on the estimate available before the call, not on the
// measured usage afterwards, because a limiter that only learns the cost after
// the request has been made cannot prevent anything. The estimate comes from
// the tokenizer over the prompt plus MaxTokens; it is wrong in the direction of
// the tokenizer's known 0.68% underestimate, which is why Settle exists to
// reconcile afterwards.
func (r *RateLimiter) Allow(subject string, limits Limits, estimatedTokens int) error {
	if limits.Zero() {
		return nil
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	sb := r.subjectLocked(subject, limits, now)

	// Requests first: it is the cheaper bucket and the one a runaway client
	// hits first. Order matters because a rejection must not consume the other
	// bucket's tokens -- charging a request that was refused would let a
	// rejected client still exhaust the token budget.
	if sb.requests != nil {
		ok, wait := sb.requests.take(1, now)
		if !ok {
			return &LimitError{Subject: subject, Kind: KindRequests, RetryAfter: wait, Limit: limits.RequestsPerMinute}
		}
	}
	if sb.tokens != nil && estimatedTokens > 0 {
		ok, wait := sb.tokens.take(float64(estimatedTokens), now)
		if !ok {
			// Refund the request token: this request is not happening, and
			// leaving it spent would make the request limit stricter than
			// configured for any client that trips the token limit.
			if sb.requests != nil {
				sb.requests.tokens = math.Min(sb.requests.capacity, sb.requests.tokens+1)
			}
			return &LimitError{Subject: subject, Kind: KindTokens, RetryAfter: wait, Limit: limits.TokensPerMinute}
		}
	}
	return nil
}

// Settle reconciles the estimate with the measured usage.
//
// Called after a response, with the difference between what was charged and
// what was actually used. A positive delta takes more tokens (without being
// able to refuse -- the request already happened); a negative delta gives them
// back. Without this the limiter drifts away from reality in whichever
// direction the estimator is biased, and the tokenizer's bias is known to be
// downward.
func (r *RateLimiter) Settle(subject string, estimated, actual int) {
	delta := actual - estimated
	if delta == 0 {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.subjects[subject]
	if !ok || sb.tokens == nil {
		return
	}
	sb.lastSeen = now
	if delta > 0 {
		// The outcome is ignored on purpose: the request has already been
		// served, so there is nothing left to refuse. The charge lands and the
		// caller pays for it out of the next minute's allowance.
		_, _ = sb.tokens.take(float64(delta), now)
		return
	}
	sb.tokens.tokens = math.Min(sb.tokens.capacity, sb.tokens.tokens-float64(delta))
}

// subjectLocked fetches or creates a subject's buckets. The caller holds the
// lock.
func (r *RateLimiter) subjectLocked(subject string, limits Limits, now time.Time) *subjectBuckets {
	sb, ok := r.subjects[subject]
	if ok && sb.limits == limits {
		sb.lastSeen = now
		return sb
	}
	// A changed limit rebuilds the buckets. Reconfiguring downward should take
	// effect now, not once the old, larger bucket has drained.
	sb = &subjectBuckets{limits: limits, lastSeen: now}
	if limits.RequestsPerMinute > 0 {
		sb.requests = newBucket(limits.RequestsPerMinute, limits.Burst, now)
	}
	if limits.TokensPerMinute > 0 {
		sb.tokens = newBucket(limits.TokensPerMinute, limits.Burst, now)
	}
	r.subjects[subject] = sb
	return sb
}

// Sweep drops subjects idle for longer than the TTL and returns how many went.
func (r *RateLimiter) Sweep() int {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, sb := range r.subjects {
		if now.Sub(sb.lastSeen) > r.idleTTL {
			delete(r.subjects, k)
			n++
		}
	}
	return n
}

// Subjects is the number of tracked subjects, for the memory-bound metric.
func (r *RateLimiter) Subjects() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subjects)
}
