package audit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/pkg/llm"
)

func sample() Record {
	return Record{
		ID: "req_1", Time: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		KeyID: "team-a-dev", Team: "team-a",
		RequestedModel: "fast", ServedModel: "gpt-4o-mini", Provider: "openai",
		Usage: llm.Usage{InputTokens: 120, OutputTokens: 80},
		Cost:  42 * llm.Microdollar,
		Prompt: []Message{
			{Role: "user", Content: "email [SLUICE_EMAIL_0001] about the invoice"},
		},
		Completion:      "I will write to [SLUICE_EMAIL_0001].",
		RedactionCounts: map[string]int{"email": 1},
		Attempts:        1,
		LatencyMS:       812.5,
	}
}

func TestWriteAndReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	require.NoError(t, w.Record(sample()))
	require.NoError(t, w.Record(Record{ID: "req_2", KeyID: "k", ErrorCode: "provider_unavailable"}))

	records, err := Read(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, "req_1", records[0].ID)
	assert.Equal(t, SchemaVersion, records[0].Schema)
	assert.Equal(t, 42*llm.Microdollar, records[0].Cost)
	assert.InDelta(t, 0.000042, records[0].CostDollars, 1e-12,
		"the dollar figure is derived from the integer, which stays authoritative")
	assert.Equal(t, "provider_unavailable", records[1].ErrorCode)
}

func TestOneRecordPerLine(t *testing.T) {
	// JSON Lines is the format because grep and jq are what is available during
	// an incident. That only holds if a record really is one line.
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	for i := 0; i < 5; i++ {
		require.NoError(t, w.Record(sample()))
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	assert.Len(t, lines, 5)
	for _, l := range lines {
		assert.True(t, strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}"))
	}
}

// TestPromptIsStoredRedacted is the check that the package's central promise
// holds at the type level: a caller building a record from a redacted request
// cannot accidentally end up with the original values, because the only place
// the raw prompt could come from is the caller, and the gateway passes the
// redacted one. This asserts the shape of what gets written.
func TestPromptIsStoredRedacted(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	require.NoError(t, w.Record(sample()))

	out := buf.String()
	assert.Contains(t, out, "SLUICE_EMAIL_0001")
	assert.NotContains(t, out, "@", "a redacted prompt contains no address")
	assert.Contains(t, out, `"redaction_counts":{"email":1}`,
		"the record still proves redaction happened and on what")
}

func TestAppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"id":"old"}`+"\n"), 0o600))

	w, err := Open(path, false)
	require.NoError(t, err)
	require.NoError(t, w.Record(sample()))
	require.NoError(t, w.Close())

	records, err := ReadFile(path)
	require.NoError(t, err)
	require.Len(t, records, 2)
	assert.Equal(t, "old", records[0].ID, "history is appended to, never rewritten")
	assert.Equal(t, "req_1", records[1].ID)
}

func TestFlushesEveryRecord(t *testing.T) {
	// A buffered audit log loses exactly the requests an investigation is
	// about, because a process that is killed is a process whose last few
	// seconds matter most.
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(path, true)
	require.NoError(t, err)
	require.NoError(t, w.Record(sample()))

	// Read it back without closing the writer.
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(b), "req_1")
	require.NoError(t, w.Close())
}

func TestReadReportsAMalformedLineRatherThanSkippingIt(t *testing.T) {
	// A replay that silently dropped a third of the log would report a cost
	// that is wrong by a third and look entirely plausible.
	in := `{"id":"a"}` + "\n" + `{"id":` + "\n" + `{"id":"c"}` + "\n"
	records, err := Read(strings.NewReader(in))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2")
	assert.Len(t, records, 1, "what was read before the failure is still returned")
}

func TestReadHandlesLongLines(t *testing.T) {
	long := strings.Repeat("x", 300<<10)
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	require.NoError(t, w.Record(Record{ID: "big", Completion: long}))
	records, err := Read(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Len(t, records[0].Completion, len(long))
}

func TestMemoryKeepsABoundedWindow(t *testing.T) {
	m := NewMemory(3)
	for i := 0; i < 10; i++ {
		require.NoError(t, m.Record(Record{ID: string(rune('a' + i))}))
	}
	all := m.Records()
	require.Len(t, all, 3)
	assert.Equal(t, "h", all[0].ID)
	assert.Equal(t, "j", all[2].ID)

	recent := m.Recent(2)
	require.Len(t, recent, 2)
	assert.Equal(t, "j", recent[0].ID, "newest first")
}

func TestTeeDeliversToAllEvenWhenOneFails(t *testing.T) {
	mem := NewMemory(10)
	tee := Tee{failing{}, mem}
	err := tee.Record(sample())
	require.Error(t, err, "the failure is reported")
	assert.Len(t, mem.Records(), 1, "and the other sink still got it")
}

type failing struct{}

func (failing) Record(Record) error { return errors.New("disk full") }

func TestNopDiscards(t *testing.T) {
	assert.NoError(t, Nop{}.Record(sample()))
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := w.Record(sample()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	records, err := Read(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err, "a torn line would fail to parse")
	assert.Len(t, records, 32*20)
	written, dropped := w.Stats()
	assert.EqualValues(t, 32*20, written)
	assert.Zero(t, dropped)
}

func TestTimeIsFilledWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, false)
	require.NoError(t, w.Record(Record{ID: "x"}))
	records, err := Read(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.False(t, records[0].Time.IsZero(), "a record with no timestamp is not an audit record")
}
