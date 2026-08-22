package redact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/internal/leaktest"
	"github.com/Liona-orph/sluice/pkg/llm"
)

// vaultWith redacts text so that the test has a vault with real mappings in it,
// rather than one hand-built out of internals the redactor would never produce.
func vaultWith(t *testing.T, text string) (*Redactor, *Vault, string) {
	t.Helper()
	r := New(DefaultPolicy())
	redacted, v := r.Redact(text)
	require.NotEqual(t, text, redacted, "the fixture must actually contain something to redact")
	return r, v, redacted
}

func TestStreamRestorerHandlesASplitPlaceholder(t *testing.T) {
	// The case that matters. A model emits a placeholder across two chunks;
	// restoring chunk by chunk finds nothing in either and the caller gets a
	// token instead of the address they sent.
	_, v, redacted := vaultWith(t, "write to alice@example.com today")
	require.Contains(t, redacted, "[SLUICE_EMAIL_0001]")

	for _, split := range []int{1, 5, 9, 14, 18} {
		sr := NewStreamRestorer(v)
		var out strings.Builder
		out.WriteString(sr.Write(redacted[:split]))
		out.WriteString(sr.Write(redacted[split:]))
		out.WriteString(sr.Flush())
		assert.Equal(t, "write to alice@example.com today", out.String(), "split at byte %d", split)
	}
}

func TestStreamRestorerHandlesOneByteAtATime(t *testing.T) {
	_, v, redacted := vaultWith(t, "mail alice@example.com and bob@example.org now")

	sr := NewStreamRestorer(v)
	var out strings.Builder
	for i := 0; i < len(redacted); i++ {
		out.WriteString(sr.Write(redacted[i : i+1]))
		assert.LessOrEqual(t, sr.Pending(), MaxPlaceholderLen+1,
			"the look-behind must stay bounded, or a long answer buffers in memory")
	}
	out.WriteString(sr.Flush())
	assert.Equal(t, "mail alice@example.com and bob@example.org now", out.String())
}

func TestStreamRestorerPassesOrdinaryTextStraightThrough(t *testing.T) {
	// The common case has no '[' at all, and paying a buffer for it would make
	// every streamed token wait for nothing.
	_, v, _ := vaultWith(t, "alice@example.com")
	sr := NewStreamRestorer(v)
	assert.Equal(t, "hello ", sr.Write("hello "))
	assert.Equal(t, "world", sr.Write("world"))
	assert.Zero(t, sr.Pending())
}

func TestStreamRestorerReleasesAnUnclosedBracket(t *testing.T) {
	// A '[' that never closes must not hold the rest of the answer hostage.
	_, v, _ := vaultWith(t, "alice@example.com")
	sr := NewStreamRestorer(v)
	sr.Write("see [")
	long := strings.Repeat("x", MaxPlaceholderLen)
	out := sr.Write(long)
	assert.Contains(t, out, "[", "past the maximum placeholder length it cannot become one, so it is emitted")
	assert.Less(t, sr.Pending(), MaxPlaceholderLen)
}

func TestStreamRestorerLeavesUnknownPlaceholdersAlone(t *testing.T) {
	_, v, _ := vaultWith(t, "alice@example.com")
	sr := NewStreamRestorer(v)
	out := sr.Write("the value is [SLUICE_EMAIL_0099] apparently") + sr.Flush()
	assert.Contains(t, out, "[SLUICE_EMAIL_0099]",
		"the model may have invented one; inventing a value to go with it would be worse")
}

func TestStreamRestorerWithNoVaultIsATransparentPipe(t *testing.T) {
	sr := NewStreamRestorer(nil)
	assert.Equal(t, "abc", sr.Write("abc"))
	assert.Empty(t, sr.Flush())
}

func TestFlushIsIdempotent(t *testing.T) {
	_, v, redacted := vaultWith(t, "alice@example.com")
	sr := NewStreamRestorer(v)
	// A partial placeholder at the end of the stream is exactly what Flush is
	// for: it is still in the look-behind when the provider stops talking.
	sr.Write("mail " + redacted[:8])
	first := sr.Flush()
	assert.NotEmpty(t, first)
	assert.Empty(t, sr.Flush(), "a second flush must not repeat the tail")
}

func TestRestoreStreamReassemblesAcrossChunks(t *testing.T) {
	defer leaktest.Check(t)()

	_, v, redacted := vaultWith(t, "ping alice@example.com please")
	// Split the redacted text at every byte boundary inside the placeholder.
	idx := strings.Index(redacted, "[SLUICE")
	require.Positive(t, idx)
	parts := []string{redacted[:idx+4], redacted[idx+4 : idx+10], redacted[idx+10:]}

	var text string
	var final llm.Chunk
	for chunk, err := range RestoreStream(chunkSeq(parts, nil), v) {
		require.NoError(t, err)
		text += chunk.Delta.Content
		if chunk.FinishReason != "" {
			final = chunk
		}
	}
	assert.Equal(t, "ping alice@example.com please", text)
	assert.Equal(t, llm.FinishStop, final.FinishReason)
	require.NotNil(t, final.Usage, "the terminating chunk keeps its usage after being held back for the flush")
	assert.Equal(t, 20, final.Usage.OutputTokens)
}

func TestRestoreStreamEmitsTheTailBeforeAnError(t *testing.T) {
	defer leaktest.Check(t)()

	_, v, redacted := vaultWith(t, "alice@example.com")
	failing := func(yield func(llm.Chunk, error) bool) {
		if !yield(llm.Chunk{ID: "id", Delta: llm.Delta{Content: redacted}}, nil) {
			return
		}
		yield(llm.Chunk{}, llm.ErrProviderUnavailable)
	}

	var text string
	var gotErr error
	for chunk, err := range RestoreStream(failing, v) {
		if err != nil {
			gotErr = err
			break
		}
		text += chunk.Delta.Content
	}
	require.Error(t, gotErr)
	assert.Contains(t, text, "alice@example.com",
		"whatever the look-behind held was already going to be delivered; losing it turns a truncated answer into no answer")
}

func TestRestoreStreamAbandonedEarlyLeaksNothing(t *testing.T) {
	defer leaktest.Check(t)()

	_, v, _ := vaultWith(t, "alice@example.com")
	for range RestoreStream(chunkSeq([]string{"a", "b", "c", "d"}, nil), v) {
		break
	}
}

func TestRestoreStreamDoesNotMutateTheProviderChunk(t *testing.T) {
	_, v, redacted := vaultWith(t, "alice@example.com")
	shared := []llm.ToolCallDelta{{Index: 0, ID: "call_1", Name: "send", ArgumentsDelta: `{"to":"` + redacted + `"}`}}
	seq := func(yield func(llm.Chunk, error) bool) {
		yield(llm.Chunk{ID: "id", Delta: llm.Delta{ToolCalls: shared}}, nil)
	}
	for chunk, err := range RestoreStream(seq, v) {
		require.NoError(t, err)
		if len(chunk.Delta.ToolCalls) > 0 {
			assert.Contains(t, chunk.Delta.ToolCalls[0].ArgumentsDelta, "alice@example.com")
		}
	}
	assert.Contains(t, shared[0].ArgumentsDelta, "SLUICE_EMAIL",
		"the provider's own slice must not be rewritten under it")
}

// TestRoundTripThroughAModelThatRewritesTheText is the property the whole
// design exists for: the value goes up redacted, the model writes new text
// around a token it does not understand, and the caller gets their own value
// back.
func TestRoundTripThroughAModelThatRewritesTheText(t *testing.T) {
	r, v, redacted := vaultWith(t, "Contact alice@example.com about card 4111 1111 1111 1111.")
	assert.NotContains(t, redacted, "alice@example.com")
	assert.NotContains(t, redacted, "4111 1111 1111 1111")

	// A model would answer around the placeholder, so imitate that.
	modelOutput := "Certainly. I have drafted a note to " + placeholderIn(redacted) + " and left the card alone."
	restored := r.RestoreResponse(llm.Response{
		Message: llm.Message{Role: llm.RoleAssistant, Content: modelOutput},
	}, v)
	assert.Contains(t, restored.Message.Content, "alice@example.com")
	assert.NotContains(t, restored.Message.Content, "4111",
		"a masked card is not restorable, which is the point of masking it")
}

// placeholderIn extracts the first placeholder from redacted text.
func placeholderIn(s string) string {
	i := strings.Index(s, "[SLUICE_")
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], "]")
	if j < 0 {
		return ""
	}
	return s[i : i+j+1]
}

func BenchmarkStreamRestore(b *testing.B) {
	r := New(DefaultPolicy())
	text := strings.Repeat("Please write to alice@example.com about the account. ", 20)
	redacted, v := r.Redact(text)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sr := NewStreamRestorer(v)
		for j := 0; j < len(redacted); j += 16 {
			end := j + 16
			if end > len(redacted) {
				end = len(redacted)
			}
			sr.Write(redacted[j:end])
		}
		sr.Flush()
	}
}

// BenchmarkRedactOneShot redacts, in a single call, exactly the text that
// BenchmarkStreamRedact pushes through the stream redactor in 16-byte chunks.
// The pair is what makes the streaming cost quotable rather than approximate:
// the two numbers differ only in how the same bytes arrive.
func BenchmarkRedactOneShot(b *testing.B) {
	r := New(DefaultPolicy())
	text := strings.Repeat("ordinary assistant prose about nothing in particular. ", 20) +
		"contact alice@example.com"
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Redact(text)
	}
}
