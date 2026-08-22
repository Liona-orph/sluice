package gateway

import (
	"sort"
	"sync"
	"time"

	"github.com/Liona-orph/sluice/internal/audit"
	"github.com/Liona-orph/sluice/internal/cache"
	"github.com/Liona-orph/sluice/internal/policy"
	"github.com/Liona-orph/sluice/internal/router"
	"github.com/Liona-orph/sluice/pkg/llm"
)

// latencyTracker keeps a bounded reservoir of recent latencies per provider.
//
// Prometheus histograms are the right thing for alerting and the wrong thing
// for the dashboard: a quantile computed from bucket boundaries is only as
// precise as the boundaries, and reading them back in-process means either
// gathering the registry and interpolating or shipping a query engine. A ring
// of the last few hundred observations gives exact quantiles over a recent
// window for the cost of a slice, and "recent" is what an operator watching a
// dashboard means anyway.
//
// The window is a count, not a duration, so a quiet provider's numbers age. The
// timestamps are kept so the dashboard can say how old they are rather than
// implying they are current.
type latencyTracker struct {
	mu   sync.Mutex
	size int
	obs  map[string]*ring
}

type ring struct {
	values []time.Duration
	next   int
	full   bool
	last   time.Time
}

func newLatencyTracker(size int) *latencyTracker {
	if size <= 0 {
		size = 256
	}
	return &latencyTracker{size: size, obs: map[string]*ring{}}
}

func (t *latencyTracker) observe(key string, d time.Duration) {
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.obs[key]
	if !ok {
		r = &ring{values: make([]time.Duration, t.size)}
		t.obs[key] = r
	}
	r.values[r.next] = d
	r.next = (r.next + 1) % len(r.values)
	if r.next == 0 {
		r.full = true
	}
	r.last = time.Now()
}

// LatencyStats is one provider's recent latency distribution.
type LatencyStats struct {
	Provider string  `json:"provider"`
	Count    int     `json:"count"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	MaxMs    float64 `json:"max_ms"`
}

func (t *latencyTracker) stats() []LatencyStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]LatencyStats, 0, len(t.obs))
	for key, r := range t.obs {
		n := r.next
		if r.full {
			n = len(r.values)
		}
		if n == 0 {
			continue
		}
		vals := make([]time.Duration, n)
		copy(vals, r.values[:n])
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		out = append(out, LatencyStats{
			Provider: key, Count: n,
			P50Ms: ms(quantile(vals, 0.50)),
			P95Ms: ms(quantile(vals, 0.95)),
			P99Ms: ms(quantile(vals, 0.99)),
			MaxMs: ms(vals[len(vals)-1]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// quantile uses nearest-rank on the sorted slice. Nearest-rank rather than
// interpolation because an interpolated p99 over 512 samples invents a value
// that was never observed, and a dashboard that shows a latency nobody
// experienced is worse than one that shows the nearest one somebody did.
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(q*float64(len(sorted))+0.5) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Stats is everything the dashboard and the stats endpoint report.
//
// It is assembled from the same structures that enforce the rules -- the
// budgets ledger, the cache's counters, the router's breakers -- rather than
// from a parallel set of counters. A dashboard that can disagree with the
// enforcement is a dashboard that will, at the worst moment.
type Stats struct {
	StartedAt time.Time `json:"started_at"`
	UptimeSec float64   `json:"uptime_seconds"`

	Cache        cache.Stats `json:"cache"`
	CacheHitRate float64     `json:"cache_hit_rate"`

	Redactions []RedactionCount `json:"redactions"`

	Latency []LatencyStats `json:"latency"`

	Targets []router.TargetStatus `json:"targets"`

	Budgets []BudgetStatus `json:"budgets"`

	Spend SpendSeries `json:"spend"`

	Requests RequestCounts `json:"requests"`

	Recent []audit.Record `json:"recent"`
}

// RedactionCount is one entity type's running total.
type RedactionCount struct {
	EntityType string `json:"entity_type"`
	Count      uint64 `json:"count"`
}

// BudgetStatus is one subject's spend against its limit.
type BudgetStatus struct {
	Subject   string  `json:"subject"`
	SpentUSD  float64 `json:"spent_usd"`
	LimitUSD  float64 `json:"limit_usd"`
	Fraction  float64 `json:"fraction"`
	WindowSec float64 `json:"window_seconds"`
	OnExceed  string  `json:"on_exceed"`
}

// SpendSeries is total spend per slot across all subjects, for the chart.
type SpendSeries struct {
	SlotSeconds float64   `json:"slot_seconds"`
	PointsUSD   []float64 `json:"points_usd"`
}

// RequestCounts is a coarse breakdown derived from the retained audit window.
//
// Derived from the audit ring rather than from the Prometheus counters because
// the counters are monotonic since process start and the dashboard wants
// "recently", and because reading them back would mean gathering the registry
// and unpacking protobufs to draw a bar chart.
type RequestCounts struct {
	Window    int            `json:"window"`
	Total     int            `json:"total"`
	Errors    int            `json:"errors"`
	CacheHits int            `json:"cache_hits"`
	Degraded  int            `json:"degraded"`
	ByCode    map[string]int `json:"by_code"`
	ErrorRate float64        `json:"error_rate"`
}

// Stats assembles the current picture.
func (g *Gateway) Stats() Stats {
	now := g.now()
	s := Stats{
		StartedAt: g.started,
		UptimeSec: now.Sub(g.started).Seconds(),
		Latency:   g.latency.stats(),
		Targets:   g.router.Status(),
		Recent:    g.recent.Recent(50),
	}
	if g.cache != nil {
		s.Cache = g.cache.Stats()
		s.CacheHitRate = s.Cache.HitRate()
	}

	g.mu.Lock()
	for t, n := range g.redactionCounts {
		s.Redactions = append(s.Redactions, RedactionCount{EntityType: string(t), Count: n})
	}
	g.mu.Unlock()
	sort.Slice(s.Redactions, func(i, j int) bool { return s.Redactions[i].EntityType < s.Redactions[j].EntityType })

	s.Budgets = g.budgetStatuses()
	s.Spend = g.spendSeries()
	s.Requests = requestCounts(g.recent.Records())
	return s
}

func (g *Gateway) budgetStatuses() []BudgetStatus {
	var out []BudgetStatus
	add := func(subject string, b policy.Budget) {
		if b.Zero() {
			return
		}
		spent := g.budgets.Spent(subject, b.Window)
		frac := 0.0
		if b.Limit > 0 {
			frac = float64(spent) / float64(b.Limit)
		}
		out = append(out, BudgetStatus{
			Subject: subject, SpentUSD: spent.Dollars(), LimitUSD: b.Limit.Dollars(),
			Fraction: frac, WindowSec: b.Window.Seconds(), OnExceed: b.OnExceed,
		})
	}
	for _, k := range g.cfg.Keys {
		add("key:"+k.ID, policy.Budget{
			Limit: k.Budget.Limit.Cost(), Window: k.Budget.Window.D(),
			OnExceed: string(k.Budget.OnExceed), DegradeTo: k.Budget.DegradeTo,
		})
	}
	for _, t := range g.cfg.Teams {
		add("team:"+t.ID, policy.Budget{
			Limit: t.Budget.Limit.Cost(), Window: t.Budget.Window.D(),
			OnExceed: string(t.Budget.OnExceed), DegradeTo: t.Budget.DegradeTo,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// spendSeries sums every tracked subject's per-slot spend.
//
// Key and team ledgers both record the same money, so summing all of them would
// double-count. Only key ledgers are summed; a team total is the sum of its
// keys by construction.
func (g *Gateway) spendSeries() SpendSeries {
	const points = 60
	var (
		out   SpendSeries
		total = make([]float64, points)
	)
	for _, k := range g.cfg.Keys {
		if k.Budget.Zero() {
			continue
		}
		series := g.budgets.SpendSeries("key:"+k.ID, points)
		if len(series.Points) == 0 {
			continue
		}
		out.SlotSeconds = float64(series.SlotWidth)
		for i, p := range series.Points {
			if i < points {
				total[i] += p.Dollars()
			}
		}
	}
	out.PointsUSD = total
	return out
}

func requestCounts(records []audit.Record) RequestCounts {
	rc := RequestCounts{Window: len(records), ByCode: map[string]int{}}
	for _, r := range records {
		rc.Total++
		if r.ErrorCode != "" {
			rc.Errors++
			rc.ByCode[r.ErrorCode]++
		}
		if r.CacheHit != "" {
			rc.CacheHits++
		}
		if r.Degraded {
			rc.Degraded++
		}
	}
	if rc.Total > 0 {
		rc.ErrorRate = float64(rc.Errors) / float64(rc.Total)
	}
	return rc
}

// Pricing exposes the price table, for the replay tool and for tests that need
// to assert a cost.
func (g *Gateway) Pricing() *llm.Pricing { return g.pricing }
