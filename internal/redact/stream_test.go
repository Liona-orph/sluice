package redact

import (
	"errors"
	"iter"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/internal/leaktest"
	"github.com/Liona-orph/sluice/pkg/llm"
)

// feed pushes text through a stream redactor in the given pieces.
func feed(r *Redactor, pieces []string) (string, *Vault) {
	v := NewVault(nil)
	sr := r.NewStreamRedactor(v)
	var out strings.Builder
	for _, p := range pieces {
		out.WriteString(sr.Write(p))
	}
	out.WriteString(sr.Flush())
	return out.String(), v
}

// splitEverywhere is the test the whole look-behind design exists to pass: for
// every possible two-way split of the input, the streamed output must equal the
// output of redacting the text whole.
func splitEverywhere(t *testing.T, r *Redactor, text string) {
	t.Helper()
	want, wantVault := r.Redact(text)

	for i := 0; i <= len(text); i++ {
		got, gotVault := feed(r, []string{text[:i], text[i:]})
		require.Equalf(t, want, got, "split at byte %d of %d", i, len(text))
		require.Equalf(t, wantVault.Len(), gotVault.Len(), "split at byte %d", i)
		require.Equalf(t, text, gotVault.Restore(got), "restore after split at byte %d", i)
	}
}

// A short look-behind makes the cut logic reachable with short fixtures; the
// SSN detector's MaxLen is 11 bytes.
func shortWindowRedactor() *Redactor {
	return New(Policy{Default: ActionTokenize}, NewUSSSNDetector())
}

func TestStreamSplitAtEveryPoint(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redactor *Redactor
		text     string
	}{
		{
			"short window, entity in the middle",
			shortWindowRedactor(),
			"the applicant gave 123-45-6789 on the intake form today",
		},
		{
			"short window, entity at the very end",
			shortWindowRedactor(),
			"the number on file is 078 05 1120",
		},
		{
			"short window, entity at the very start",
			shortWindowRedactor(),
			"123-45-6789 was the number quoted in the application",
		},
		{
			"short window, two adjacent entities",
			shortWindowRedactor(),
			"records 123-45-6789 078-05-1120 both appear",
		},
		{
			"short window, near-misses either side",
			shortWindowRedactor(),
			"ids 000-12-3456 and 123-45-6789 and 666-45-6789 filed",
		},
		{
			"full detector set, long enough to force cuts",
			New(Policy{Default: ActionTokenize}),
			strings.Repeat("padding text to push past the look-behind window. ", 8) +
				"Contact alice@example.com or call +44 20 7946 0958. " +
				strings.Repeat("more trailing prose that has to be emitted too. ", 8),
		},
		{
			"multi-byte runes around the entity",
			shortWindowRedactor(),
			"identité 123-45-6789 — enregistrée ✓ dans le système",
		},
		{
			"no entities at all",
			shortWindowRedactor(),
			strings.Repeat("nothing sensitive in this sentence at all. ", 10),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			splitEverywhere(t, tc.redactor, tc.text)
		})
	}
}

func TestStreamByteAtATime(t *testing.T) {
	r := New(Policy{Default: ActionTokenize})
	text := strings.Repeat("lorem ipsum dolor sit amet, consectetur. ", 10) +
		"reach me at alice@example.com or 415-555-0132."
	want, _ := r.Redact(text)

	pieces := make([]string, 0, len(text))
	for i := 0; i < len(text); i++ {
		pieces = append(pieces, text[i:i+1])
	}
	got, v := feed(r, pieces)
	assert.Equal(t, want, got)
	assert.Equal(t, text, v.Restore(got))
}

func TestStreamThreeWaySplits(t *testing.T) {
	r := shortWindowRedactor()
	text := "ref 123-45-6789 and 078 05 1120 filed on the intake form"
	want, _ := r.Redact(text)

	for i := 0; i <= len(text); i++ {
		for j := i; j <= len(text); j++ {
			got, _ := feed(r, []string{text[:i], text[i:j], text[j:]})
			require.Equalf(t, want, got, "splits at %d and %d", i, j)
		}
	}
}

func TestStreamHoldsBackBoundedAmount(t *testing.T) {
	r := shortWindowRedactor()
	sr := r.NewStreamRedactor(nil)

	// Nothing is emitted until the buffer exceeds the look-behind, and after
	// that the lag stays bounded rather than growing with the input.
	assert.Empty(t, sr.Write("short"))

	var emitted int
	const chunk = "some ordinary text without identifiers "
	for i := 0; i < 20; i++ {
		emitted += len(sr.Write(chunk))
	}
	total := 5 + 20*len(chunk)
	assert.Positive(t, emitted)
	assert.LessOrEqual(t, total-emitted, 2*11+len(chunk),
		"the retained tail must stay within the look-behind plus one chunk")
}

func TestStreamFlushIsIdempotent(t *testing.T) {
	r := shortWindowRedactor()
	sr := r.NewStreamRedactor(nil)
	sr.Write("id 123-45-6789")
	assert.NotEmpty(t, sr.Flush())
	assert.Empty(t, sr.Flush(), "a second flush must not repeat the tail")
}

// --- chunk sequence wrapper -------------------------------------------------

func chunkSeq(contents []string, tail error) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		for _, c := range contents {
			if !yield(llm.Chunk{ID: "r1", Model: "m", Provider: "p", Delta: llm.Delta{Content: c}}, nil) {
				return
			}
		}
		if tail != nil {
			yield(llm.Chunk{}, tail)
			return
		}
		usage := llm.Usage{InputTokens: 10, OutputTokens: 20}
		yield(llm.Chunk{ID: "r1", Model: "m", Provider: "p",
			FinishReason: llm.FinishStop, Usage: &usage}, nil)
	}
}

func TestRedactStream(t *testing.T) {
	defer leaktest.Check(t)()
	r := New(Policy{Default: ActionTokenize})
	text := strings.Repeat("assistant output that is long enough to be cut. ", 8) +
		"You can reach her at alice@example.com."

	var pieces []string
	for i := 0; i < len(text); i += 7 {
		end := min(i+7, len(text))
		pieces = append(pieces, text[i:end])
	}

	vault := NewVault(nil)
	collected, err := llm.Collect(r.RedactStream(chunkSeq(pieces, nil), vault))
	require.NoError(t, err)

	want, _ := New(Policy{Default: ActionTokenize}).Redact(text)
	assert.Equal(t, want, collected.Message.Content)
	assert.NotContains(t, collected.Message.Content, "alice@example.com")
	assert.Equal(t, llm.FinishStop, collected.FinishReason, "the terminator must survive rewriting")
	assert.Equal(t, 20, collected.Usage.OutputTokens)
	assert.Equal(t, "r1", collected.ID)
}

func TestRedactStreamPropagatesErrorAfterFlushing(t *testing.T) {
	defer leaktest.Check(t)()
	r := New(Policy{Default: ActionTokenize})
	boom := errors.New("upstream closed")

	seq := r.RedactStream(chunkSeq([]string{"partial answer with alice@example.com in it"}, boom), nil)

	var content strings.Builder
	var got error
	for c, err := range seq {
		if err != nil {
			got = err
			break
		}
		content.WriteString(c.Delta.Content)
	}
	require.ErrorIs(t, got, boom)
	assert.Contains(t, content.String(), "[SLUICE_EMAIL_0001]",
		"text held by the look-behind must still be delivered")
	assert.NotContains(t, content.String(), "alice@example.com")
}

func TestRedactStreamAbandonedEarly(t *testing.T) {
	defer leaktest.Check(t)()
	r := New(Policy{Default: ActionTokenize})
	pieces := make([]string, 200)
	for i := range pieces {
		pieces[i] = "some words here "
	}
	for range r.RedactStream(chunkSeq(pieces, nil), nil) {
		break
	}
}

func TestStreamAndRequestShareAVault(t *testing.T) {
	// The end-to-end shape: redact the request, stream the response through the
	// same vault, restore. A value tokenized on the way in comes back on the
	// way out.
	r := New(Policy{Default: ActionTokenize})
	req := llm.Request{Messages: []llm.Message{{Role: llm.RoleUser,
		Content: "Write to alice@example.com about the invoice."}}}

	clean, vault := r.RedactRequest(req)
	require.NotContains(t, clean.Messages[0].Content, "alice@example.com")

	// The model echoes the placeholder back, chunked awkwardly.
	reply := "I have drafted a note to [SLUICE_EM" + "AIL_0001] as requested."
	collected, err := llm.Collect(r.RedactStream(chunkSeq([]string{
		"I have drafted a note to [SLUICE_EM", "AIL_0001] as requested.",
	}, nil), vault))
	require.NoError(t, err)
	assert.Equal(t, reply, collected.Message.Content)
	assert.Contains(t, vault.Restore(collected.Message.Content), "alice@example.com")
}

func BenchmarkStreamRedact(b *testing.B) {
	r := New(DefaultPolicy())
	text := strings.Repeat("ordinary assistant prose about nothing in particular. ", 20) +
		"contact alice@example.com"
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sr := r.NewStreamRedactor(nil)
		for j := 0; j < len(text); j += 16 {
			sr.Write(text[j:min(j+16, len(text))])
		}
		sr.Flush()
	}
}
