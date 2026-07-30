package policy

import (
	"fmt"
	"sync"
	"time"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// Action is what a budget check decided.
type Action string

const (
	// ActionAllow means the request proceeds as asked.
	ActionAllow Action = "allow"
	// ActionDegrade means the request proceeds on a cheaper route.
	ActionDegrade Action = "degrade"
	// ActionReject means the request does not proceed.
	ActionReject Action = "reject"
)

// Budget is one subject's spend limit over a rolling window.
type Budget struct {
	Limit  llm.Cost
	Window time.Duration
	// OnExceed is "reject" or "degrade".
	OnExceed string
	// DegradeTo is the cheaper route alias used when OnExceed is degrade.
	DegradeTo string
}

// Zero reports whether no budget is configured.
func (b Budget) Zero() bool { return b.Limit <= 0 || b.Window <= 0 }

// Decision is the outcome of a budget check, carrying enough context for the
// audit record and for the error message a client sees.
type Decision struct {
	Action Action
	// Subject names which budget bound, key or team, so that a rejected caller
	// is told whether to ask for more or to ask their team for more.
	Subject string
	Spent   llm.Cost
	Limit   llm.Cost
	// Model is the route to use. It is the requested model for ActionAllow and
	// the degrade target for ActionDegrade.
	Model string
	// ResetsIn is when the oldest spend in the window falls out of it, which is
	// the earliest the caller could succeed unchanged.
	ResetsIn time.Duration
}

// ExceededError reports a rejected request.
type ExceededError struct{ Decision Decision }

func (e *ExceededError) Error() string {
	return fmt.Sprintf("budget: %s has spent %s of %s in the window; resets in %s",
		e.Decision.Subject, e.Decision.Spent, e.Decision.Limit, e.Decision.ResetsIn.Round(time.Second))
}

// bucketCount is how many slots a window is divided into.
//
// A rolling window computed exactly would need every individual charge kept
// until it aged out, which is unbounded memory in the one place a gateway
// cannot afford it -- a key doing a thousand requests a second against a
// 24-hour window is 86 million records. Dividing the window into fixed slots
// and expiring whole slots bounds the memory at 120 int64s per subject and
// bounds the error at one slot's width: a 24-hour window is accurate to twelve
// minutes, which is far finer than the granularity at which anyone sets a daily
// spending limit.
const bucketCount = 120

// ledger is one subject's rolling spend.
type ledger struct {
	window time.Duration
	slots  [bucketCount]llm.Cost
	// slotStart is the start time of the slot at index cursor.
	slotStart time.Time
	cursor    int
	total     llm.Cost
	lastSeen  time.Time
}

func (l *ledger) slotWidth() time.Duration { return l.window / bucketCount }

// advance expires slots that have fallen out of the window.
func (l *ledger) advance(now time.Time) {
	width := l.slotWidth()
	if width <= 0 {
		return
	}
	steps := int(now.Sub(l.slotStart) / width)
	if steps <= 0 {
		return
	}
	if steps >= bucketCount {
		// The subject has been idle for longer than the whole window; nothing
		// survives, and looping bucketCount times to discover that is waste.
		for i := range l.slots {
			l.slots[i] = 0
		}
		l.total = 0
		l.cursor = 0
		l.slotStart = now.Truncate(width)
		return
	}
	for i := 0; i < steps; i++ {
		l.cursor = (l.cursor + 1) % bucketCount
		l.total -= l.slots[l.cursor]
		l.slots[l.cursor] = 0
	}
	l.slotStart = l.slotStart.Add(time.Duration(steps) * width)
}

// resetsIn is how long until the oldest non-empty slot expires.
func (l *ledger) resetsIn(now time.Time) time.Duration {
	width := l.slotWidth()
	if width <= 0 {
		return 0
	}
	for i := 1; i <= bucketCount; i++ {
		idx := (l.cursor + i) % bucketCount
		if l.slots[idx] > 0 {
			// Slot idx expires after (i) more steps from the current slot.
			return l.slotStart.Add(time.Duration(i) * width).Sub(now)
		}
	}
	return 0
}

// Budgets tracks rolling spend per subject.
//
// Subjects are opaque strings; the gateway uses "key:<id>" and "team:<id>" so
// that the two namespaces cannot collide and so that a metric or a log line
// says which kind of limit was involved without a second field.
type Budgets struct {
	mu      sync.Mutex
	ledgers map[string]*ledger
	now     func() time.Time
}

// NewBudgets returns a tracker. now may be nil for time.Now.
func NewBudgets(now func() time.Time) *Budgets {
	if now == nil {
		now = time.Now
	}
	return &Budgets{ledgers: map[string]*ledger{}, now: now}
}

// Sweep drops ledgers that have been empty for a whole window, and returns how
// many went. Without it a gateway that has seen a million distinct keys holds a
// million ledgers forever, which is a slow memory leak wearing the costume of a
// cache.
func (b *Budgets) Sweep() int {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for subject, l := range b.ledgers {
		l.advance(now)
		if l.total == 0 && now.Sub(l.lastSeen) > l.window {
			delete(b.ledgers, subject)
			n++
		}
	}
	return n
}

// Spent returns the rolling spend for a subject over window.
func (b *Budgets) Spent(subject string, window time.Duration) llm.Cost {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerLocked(subject, window, now)
	l.advance(now)
	return l.total
}

// Record adds cost to a subject's rolling spend.
//
// Recorded after the response rather than reserved before it. Reserving would
// need an estimate of the output length, which nobody has before generation,
// and the error would be systematic: reserve too much and a key with a
// generous limit is throttled at half of it. The cost of recording afterwards
// is that a burst of concurrent requests can overshoot the limit by roughly one
// burst's worth of spend before any of them is refused. That is a real
// overshoot, it is bounded by concurrency times the per-request cost, and it is
// the correct trade for a limit that is measured in dollars per day rather than
// dollars per second.
func (b *Budgets) Record(subject string, window time.Duration, cost llm.Cost) {
	if cost == 0 || window <= 0 {
		return
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerLocked(subject, window, now)
	l.advance(now)
	l.slots[l.cursor] += cost
	l.total += cost
	l.lastSeen = now
}

// Check decides whether subject may make a request against model.
//
// It does not reserve anything; see Record for why.
func (b *Budgets) Check(subject string, budget Budget, model string) Decision {
	if budget.Zero() {
		return Decision{Action: ActionAllow, Subject: subject, Model: model}
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.ledgerLocked(subject, budget.Window, now)
	l.advance(now)

	d := Decision{
		Subject: subject, Spent: l.total, Limit: budget.Limit,
		Model: model, ResetsIn: l.resetsIn(now),
	}
	if l.total < budget.Limit {
		d.Action = ActionAllow
		return d
	}
	if budget.OnExceed == "degrade" && budget.DegradeTo != "" && budget.DegradeTo != model {
		d.Action = ActionDegrade
		d.Model = budget.DegradeTo
		return d
	}
	// Degrading to the model already in use would be a no-op that silently
	// removed the limit, so it becomes a rejection instead.
	d.Action = ActionReject
	d.Model = model
	return d
}

func (b *Budgets) ledgerLocked(subject string, window time.Duration, now time.Time) *ledger {
	l, ok := b.ledgers[subject]
	if ok && l.window == window {
		return l
	}
	width := window / bucketCount
	start := now
	if width > 0 {
		start = now.Truncate(width)
	}
	l = &ledger{window: window, slotStart: start, lastSeen: now}
	b.ledgers[subject] = l
	return l
}

// Snapshot is a subject's spend, for the dashboard and the stats endpoint.
type Snapshot struct {
	Subject string   `json:"subject"`
	Spent   llm.Cost `json:"spent_nanodollars"`
	Dollars float64  `json:"spent_dollars"`
}

// Snapshots returns every tracked subject's current rolling spend.
func (b *Budgets) Snapshots() []Snapshot {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Snapshot, 0, len(b.ledgers))
	for subject, l := range b.ledgers {
		l.advance(now)
		out = append(out, Snapshot{Subject: subject, Spent: l.total, Dollars: l.total.Dollars()})
	}
	return out
}

// Series is a spend time series for one subject, oldest slot first. It is what
// the dashboard's spend chart draws, and it comes straight out of the ledger
// rather than from a second store, so the chart cannot disagree with the
// enforcement.
type Series struct {
	Subject   string     `json:"subject"`
	SlotWidth Duration   `json:"slot_width_seconds"`
	Points    []llm.Cost `json:"points"`
}

// Duration is a JSON-friendly seconds count.
type Duration float64

// SpendSeries returns up to n trailing slots of a subject's spend.
func (b *Budgets) SpendSeries(subject string, n int) Series {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.ledgers[subject]
	if !ok {
		return Series{Subject: subject}
	}
	l.advance(now)
	if n <= 0 || n > bucketCount {
		n = bucketCount
	}
	points := make([]llm.Cost, 0, n)
	for i := n - 1; i >= 0; i-- {
		idx := ((l.cursor-i)%bucketCount + bucketCount) % bucketCount
		points = append(points, l.slots[idx])
	}
	return Series{Subject: subject, SlotWidth: Duration(l.slotWidth().Seconds()), Points: points}
}
