package redact

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type labelledEntity struct {
	Type  EntityType `json:"type"`
	Value string     `json:"value"`
}

type document struct {
	ID       string           `json:"id"`
	Text     string           `json:"text"`
	Entities []labelledEntity `json:"entities"`
}

func loadCorpus(t testing.TB) []document {
	t.Helper()
	raw, err := os.ReadFile("testdata/pii_corpus.json")
	require.NoError(t, err)
	var doc struct {
		Documents []document `json:"documents"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Documents)
	return doc.Documents
}

// Accuracy floors, per detector.
//
// They are set below the measured values, not at them, so that ordinary
// variation in the fixtures does not turn every corpus edit into a test
// failure -- but close enough that a real regression trips them. Where a floor
// is well under 1.0 it is because the detector genuinely cannot do better, and
// the comment says why.
var floors = map[EntityType]struct{ precision, recall float64 }{
	EntityEmail:      {0.95, 0.95},
	EntityPhone:      {0.90, 0.90},
	EntityCreditCard: {1.00, 1.00}, // Luhn plus the issuer digit leaves no false positives in the corpus.
	EntityIBAN:       {1.00, 1.00}, // mod-97 likewise.
	EntityIPAddress:  {1.00, 0.95},
	EntityUSSSN:      {1.00, 0.95},
	EntityUKNINO:     {1.00, 0.95},
	EntityESDNI:      {1.00, 0.95},
	// A high-entropy detector cannot tell a credential from a content digest;
	// the corpus contains one digest and it is counted as a false positive.
	EntityAPIKey: {0.75, 0.95},
	// Names have no checksum and no structure. Recall is limited by the
	// gazetteer and by surnames appearing alone; precision by ordinary words
	// that are also names. This is the floor a gazetteer-plus-context approach
	// can hold, and beating it needs a model.
	EntityPersonName: {0.70, 0.55},
}

// counts is a per-type confusion count over (type, value) pairs.
type counts struct{ truePos, falsePos, falseNeg int }

func (c counts) precision() float64 {
	if c.truePos+c.falsePos == 0 {
		return 1
	}
	return float64(c.truePos) / float64(c.truePos+c.falsePos)
}

func (c counts) recall() float64 {
	if c.truePos+c.falseNeg == 0 {
		return 1
	}
	return float64(c.truePos) / float64(c.truePos+c.falseNeg)
}

// TestDetectorAccuracy is the measurement the rest of this package's claims
// rest on. It compares detector output against the labelled corpus by (type,
// value) pair rather than by byte offset: offsets in a hand-written fixture are
// unmaintainable, and a detector that finds the right value at the wrong offset
// would fail the round-trip tests anyway.
func TestDetectorAccuracy(t *testing.T) {
	corpus := loadCorpus(t)
	r := New(Policy{})

	per := map[EntityType]*counts{}
	get := func(typ EntityType) *counts {
		if c, ok := per[typ]; ok {
			return c
		}
		c := &counts{}
		per[typ] = c
		return c
	}

	for _, doc := range corpus {
		want := map[labelledEntity]int{}
		for _, e := range doc.Entities {
			want[e]++
			get(e.Type)
		}
		got := map[labelledEntity]int{}
		for _, m := range r.Detect(doc.Text) {
			got[labelledEntity{Type: m.Type, Value: m.Value}]++
			get(m.Type)
		}

		for e, n := range got {
			matched := min(n, want[e])
			get(e.Type).truePos += matched
			if n > matched {
				get(e.Type).falsePos += n - matched
				t.Logf("false positive in %s: %s %q", doc.ID, e.Type, e.Value)
			}
		}
		for e, n := range want {
			if missing := n - got[e]; missing > 0 {
				get(e.Type).falseNeg += missing
				t.Logf("missed in %s: %s %q", doc.ID, e.Type, e.Value)
			}
		}
	}

	types := make([]EntityType, 0, len(per))
	for typ := range per {
		types = append(types, typ)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })

	t.Log("detector accuracy over " + fmt.Sprint(len(corpus)) + " labelled documents:")
	for _, typ := range types {
		c := per[typ]
		t.Logf("  %-14s precision %.3f  recall %.3f  (tp=%d fp=%d fn=%d)",
			typ, c.precision(), c.recall(), c.truePos, c.falsePos, c.falseNeg)

		floor, ok := floors[typ]
		require.Truef(t, ok, "detector %s has no stated accuracy floor", typ)
		assert.GreaterOrEqualf(t, c.precision(), floor.precision, "%s precision", typ)
		assert.GreaterOrEqualf(t, c.recall(), floor.recall, "%s recall", typ)
	}

	for typ := range floors {
		assert.Containsf(t, per, typ, "no fixture exercises %s", typ)
	}
}

// MinConfidence is a filter, so raising it can only remove matches: recall must
// be non-increasing. What it does to precision is a property of the corpus, not
// a guarantee, and this corpus makes the point -- the one residual false
// positive on names ("contact Support") is a high-confidence context match, so
// raising the threshold discards correct low-confidence matches and leaves the
// error behind. The numbers are logged rather than asserted because the useful
// information is where the remaining errors live, not that a knob turns.
func TestMinConfidenceOnlyRemovesMatches(t *testing.T) {
	corpus := loadCorpus(t)
	measure := func(minConf float64) counts {
		r := New(Policy{MinConfidence: minConf})
		var c counts
		for _, doc := range corpus {
			want := map[string]int{}
			for _, e := range doc.Entities {
				if e.Type == EntityPersonName {
					want[e.Value]++
				}
			}
			for _, m := range r.Detect(doc.Text) {
				if m.Type != EntityPersonName {
					continue
				}
				if want[m.Value] > 0 {
					want[m.Value]--
					c.truePos++
				} else {
					c.falsePos++
				}
			}
			for _, n := range want {
				c.falseNeg += n
			}
		}
		return c
	}

	prev := measure(0)
	t.Logf("names at MinConfidence 0.00: precision %.3f recall %.3f (tp=%d fp=%d fn=%d)",
		prev.precision(), prev.recall(), prev.truePos, prev.falsePos, prev.falseNeg)
	for _, threshold := range []float64{0.55, 0.65, 0.9, 1.0} {
		got := measure(threshold)
		t.Logf("names at MinConfidence %.2f: precision %.3f recall %.3f (tp=%d fp=%d fn=%d)",
			threshold, got.precision(), got.recall(), got.truePos, got.falsePos, got.falseNeg)
		assert.LessOrEqual(t, got.recall(), prev.recall(), "recall must not rise as the filter tightens")
		assert.LessOrEqual(t, got.truePos+got.falsePos, prev.truePos+prev.falsePos)
		prev = got
	}
	assert.Zero(t, prev.truePos+prev.falsePos, "a threshold above every confidence must detect nothing")
}
