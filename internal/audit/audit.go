// Package audit is the append-only record of what the gateway did.
//
// One record per request, written after the response, answering: who asked,
// what they asked (redacted), which provider answered, what it cost, what was
// redacted, and whether it came from cache. It exists to make three questions
// answerable after the fact -- "why is the bill this size", "did anything
// sensitive leave the building", and "what would this have cost on the other
// model" -- and the third is why the record carries the full token counts and
// the physical model rather than just a dollar figure. See cmd/sluice replay.
//
// # Why the prompt is stored redacted and never raw
//
// The tempting design stores the original prompt so that an investigator can
// see exactly what happened. It is the wrong one, for four reasons that
// compound.
//
// The audit log is the least-protected copy of the data. It is tailed, shipped
// to a log aggregator, indexed, and retained for years by policy. A prompt
// containing a customer's card number that was correctly stripped before it
// reached a provider, and then written verbatim to a file that gets shipped to
// three SaaS vendors, has not been protected: it has been copied.
//
// It would also make the redactor pointless in exactly the case it matters. The
// entire claim of this product is that sensitive values do not leave the
// process boundary in the clear. A raw audit log breaks that claim on the
// gateway's own disk.
//
// Retention rules differ. Audit records are kept for years; the personal data
// inside a prompt is usually subject to a deletion request measured in days. A
// log that mixes them cannot honour either policy without rewriting an
// append-only file, which is a contradiction in terms.
//
// And the redacted prompt is nearly as useful. The placeholders are stable
// within a request, so the structure of what was asked is fully visible --
// "summarise the account for [SLUICE_PERSON_0001] at [SLUICE_EMAIL_0001]" tells
// an investigator everything except the identity, and the identity is the one
// thing they should have to obtain through a separate, authorised path.
//
// What is lost: an investigator cannot reconstruct the exact prompt from the
// audit log alone. That is the intended trade, and RedactionCounts is there so
// that the record still proves redaction happened and on what.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// SchemaVersion is written into every record.
//
// A replay tool reading a year of logs will meet records written by several
// builds, and a field that changed meaning silently is worse than one that was
// added. Bump this when the meaning of an existing field changes, not when a
// field is added.
const SchemaVersion = 1

// Message is one turn as stored: role plus already-redacted content.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Record is one request's audit entry.
type Record struct {
	Schema int       `json:"schema"`
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`

	// KeyID is the non-secret key identifier. The secret itself never appears
	// here, in a log line, or in a metric label.
	KeyID string `json:"key_id"`
	Team  string `json:"team,omitempty"`

	// RequestedModel is the alias the client asked for; ServedModel and
	// Provider are what actually answered. All three, because a cost report
	// needs the physical model, a product owner needs the alias, and an
	// incident needs the provider.
	RequestedModel string `json:"requested_model"`
	ServedModel    string `json:"served_model,omitempty"`
	Provider       string `json:"provider,omitempty"`

	Stream   bool `json:"stream"`
	Degraded bool `json:"degraded,omitempty"`
	// Attempts and Failovers say how much work the answer took, which is the
	// difference between a healthy p99 and a lucky one.
	Attempts  int `json:"attempts"`
	Failovers int `json:"failovers,omitempty"`

	CacheHit        string  `json:"cache_hit,omitempty"`
	CacheSimilarity float64 `json:"cache_similarity,omitempty"`

	Usage llm.Usage `json:"usage"`
	// Cost is in nanodollars, the unit the gateway computes in. CostDollars is
	// the same number rendered for humans and for tools that will not parse an
	// integer scale; it is derived, and the integer is authoritative.
	Cost        llm.Cost `json:"cost_nanodollars"`
	CostDollars float64  `json:"cost_dollars"`
	// Estimated mirrors Usage.Estimated at the top level so that a cost report
	// can filter on it without unpacking the usage object.
	Estimated bool `json:"estimated,omitempty"`

	// RedactionCounts is entity type to number of distinct values replaced. It
	// is the proof that redaction ran and what it found, without recording what
	// it found.
	RedactionCounts map[string]int `json:"redaction_counts,omitempty"`

	LatencyMS float64 `json:"latency_ms"`

	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`

	// Prompt and Completion are redacted. See the package comment.
	Prompt     []Message `json:"prompt,omitempty"`
	Completion string    `json:"completion,omitempty"`
}

// Recorder accepts audit records.
//
// An interface so that the gateway can be tested against an in-memory
// implementation, and so that a deployment can send records somewhere other
// than a file without the gateway learning about it.
type Recorder interface {
	Record(Record) error
}

// Nop discards records. Used when auditing is switched off, so that the
// gateway has no nil check on its hot path.
type Nop struct{}

// Record implements Recorder.
func (Nop) Record(Record) error { return nil }

// Writer appends records to a stream as JSON Lines.
//
// JSON Lines rather than a framed binary format or a database: an audit log's
// most important property is that it can be read by whatever is available
// during an incident, which is grep and jq. A partially written final line is
// recoverable by dropping it; a partially written binary frame is not.
//
// Append-only is enforced by how the file is opened (O_APPEND) rather than by
// permissions, which the process cannot set on itself meaningfully. That stops
// this code from rewriting history; it does not stop anything else on the host
// from doing so. A deployment that needs tamper evidence ships the lines
// somewhere the gateway cannot reach; see SECURITY.md.
type Writer struct {
	mu  sync.Mutex
	w   *bufio.Writer
	f   *os.File
	enc *json.Encoder
	// sync fsyncs after every record.
	sync bool
	// dropped counts records lost to write errors. A gateway must not fail a
	// request because its audit log is full, but it must not pretend the record
	// was written either.
	dropped uint64
	written uint64
}

// NewWriter appends to w. If w is an *os.File and fsync is set, every record
// is flushed all the way to the device.
func NewWriter(w io.Writer, fsync bool) *Writer {
	aw := &Writer{w: bufio.NewWriterSize(w, 32<<10), sync: fsync}
	if f, ok := w.(*os.File); ok {
		aw.f = f
	}
	aw.enc = json.NewEncoder(aw.w)
	return aw
}

// Open opens path for append, creating it with owner-only permissions. "-"
// means stdout.
func Open(path string, fsync bool) (*Writer, error) {
	if path == "-" {
		return NewWriter(os.Stdout, false), nil
	}
	// The path comes from the operator's own configuration, which is the point
	// of a configurable audit destination.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return NewWriter(f, fsync), nil
}

// Record appends r.
func (w *Writer) Record(r Record) error {
	r.Schema = SchemaVersion
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	r.CostDollars = r.Cost.Dollars()

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(r); err != nil {
		w.dropped++
		return fmt.Errorf("audit: encode: %w", err)
	}
	// Flushed per record rather than per buffer. An audit log that loses the
	// last few seconds of records when the process is killed is an audit log
	// that is missing exactly the requests an investigation is about.
	if err := w.w.Flush(); err != nil {
		w.dropped++
		return fmt.Errorf("audit: flush: %w", err)
	}
	if w.sync && w.f != nil {
		if err := w.f.Sync(); err != nil {
			w.dropped++
			return fmt.Errorf("audit: sync: %w", err)
		}
	}
	w.written++
	return nil
}

// Stats reports records written and dropped.
func (w *Writer) Stats() (written, dropped uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written, w.dropped
}

// Close flushes and closes the underlying file, if there is one.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.w.Flush()
	if w.f != nil && w.f != os.Stdout {
		if cerr := w.f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return fmt.Errorf("audit: close: %w", err)
	}
	return nil
}

// Memory is an in-memory Recorder for tests and for the dashboard's recent
// activity list. It keeps the last max records.
type Memory struct {
	mu      sync.RWMutex
	records []Record
	limit   int
}

// NewMemory returns a ring holding at most limit records. A limit of zero or
// less means 1000.
func NewMemory(limit int) *Memory {
	if limit <= 0 {
		limit = 1000
	}
	return &Memory{limit: limit}
}

// Record implements Recorder.
func (m *Memory) Record(r Record) error {
	r.Schema = SchemaVersion
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	r.CostDollars = r.Cost.Dollars()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, r)
	if len(m.records) > m.limit {
		m.records = append(m.records[:0:0], m.records[len(m.records)-m.limit:]...)
	}
	return nil
}

// Records returns a copy of the retained records, oldest first.
func (m *Memory) Records() []Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Record(nil), m.records...)
}

// Recent returns up to n most recent records, newest first.
func (m *Memory) Recent(n int) []Record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if n <= 0 || n > len(m.records) {
		n = len(m.records)
	}
	out := make([]Record, 0, n)
	for i := len(m.records) - 1; i >= len(m.records)-n; i-- {
		out = append(out, m.records[i])
	}
	return out
}

// Tee sends each record to several recorders, returning the first error but
// still delivering to the rest.
//
// The gateway uses it to keep a bounded in-memory window for the dashboard
// while the durable log goes to a file. Failing the file write must not stop
// the dashboard from seeing the request, and vice versa.
type Tee []Recorder

// Record implements Recorder.
func (t Tee) Record(r Record) error {
	var first error
	for _, rec := range t {
		if err := rec.Record(r); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Read decodes every record from r.
//
// A malformed line is an error rather than a skip: a replay that silently
// dropped a third of the log would report a cost that is wrong by a third and
// look entirely plausible. The error names the line so it can be found.
func Read(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	// Prompts are large. The default 64KB token limit would turn a long
	// conversation into a parse error a long way from where it was caused.
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(b, &rec); err != nil {
			return out, fmt.Errorf("audit: line %d: %w", line, err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("audit: read: %w", err)
	}
	return out, nil
}

// ReadFile reads an audit log from disk. "-" reads stdin.
func ReadFile(path string) ([]Record, error) {
	if path == "-" {
		return Read(os.Stdin)
	}
	f, err := os.Open(path) //nolint:gosec // the path is an operator-supplied argument
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; a close error here says nothing new
	return Read(f)
}

// ErrNoRecords is returned by tools that need at least one record.
var ErrNoRecords = errors.New("audit: log contains no records")
