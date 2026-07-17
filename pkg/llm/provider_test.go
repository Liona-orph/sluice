package llm

import (
	"encoding/json"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seqOf(chunks []Chunk, tail error) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		for _, c := range chunks {
			if !yield(c, nil) {
				return
			}
		}
		if tail != nil {
			yield(Chunk{}, tail)
		}
	}
}

func TestCollectAssemblesContent(t *testing.T) {
	usage := Usage{InputTokens: 9, OutputTokens: 3}
	seq := seqOf([]Chunk{
		{ID: "r1", Model: "local-small", Provider: "local", Delta: Delta{Role: RoleAssistant, Content: "Hello"}},
		{ID: "r1", Delta: Delta{Content: ", "}},
		{ID: "r1", Delta: Delta{Content: "world"}, FinishReason: FinishStop, Usage: &usage},
	}, nil)

	resp, err := Collect(seq)
	require.NoError(t, err)
	assert.Equal(t, "r1", resp.ID)
	assert.Equal(t, "local-small", resp.Model)
	assert.Equal(t, "local", resp.Provider)
	assert.Equal(t, RoleAssistant, resp.Message.Role)
	assert.Equal(t, "Hello, world", resp.Message.Content)
	assert.Equal(t, FinishStop, resp.FinishReason)
	assert.Equal(t, usage, resp.Usage)
}

func TestCollectAssemblesToolCalls(t *testing.T) {
	// Two interleaved calls, arguments split mid-JSON: the case that breaks
	// naive reassembly.
	seq := seqOf([]Chunk{
		{Delta: Delta{Role: RoleAssistant, ToolCalls: []ToolCallDelta{
			{Index: 0, ID: "c0", Name: "search", ArgumentsDelta: `{"q":`},
			{Index: 1, ID: "c1", Name: "weather", ArgumentsDelta: `{"ci`},
		}}},
		{Delta: Delta{ToolCalls: []ToolCallDelta{
			{Index: 1, ArgumentsDelta: `ty":"Oslo"}`},
			{Index: 0, ArgumentsDelta: `"go"}`},
		}}, FinishReason: FinishToolCalls},
	}, nil)

	resp, err := Collect(seq)
	require.NoError(t, err)
	require.Len(t, resp.Message.ToolCalls, 2)
	assert.Equal(t, ToolCall{ID: "c0", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}, resp.Message.ToolCalls[0])
	assert.Equal(t, ToolCall{ID: "c1", Name: "weather", Arguments: json.RawMessage(`{"city":"Oslo"}`)}, resp.Message.ToolCalls[1])
	assert.Equal(t, FinishToolCalls, resp.FinishReason)
}

func TestCollectStopsAtFirstError(t *testing.T) {
	boom := errors.New("upstream closed")
	seq := seqOf([]Chunk{{Delta: Delta{Content: "partial"}}}, boom)
	_, err := Collect(seq)
	assert.ErrorIs(t, err, boom)
}

func TestCollectEmptyStream(t *testing.T) {
	resp, err := Collect(seqOf(nil, nil))
	require.NoError(t, err)
	assert.Equal(t, RoleAssistant, resp.Message.Role, "an empty stream still yields a well-formed message")
	assert.Empty(t, resp.Message.Content)
}

func TestStreamOfRoundTrips(t *testing.T) {
	orig := Response{
		ID: "r", Model: "m", Provider: "p",
		Message: Message{Role: RoleAssistant, Content: "cached answer",
			ToolCalls: []ToolCall{{ID: "t0", Name: "f", Arguments: json.RawMessage(`{}`)}}},
		FinishReason: FinishStop,
		Usage:        Usage{InputTokens: 4, OutputTokens: 2},
	}
	got, err := Collect(StreamOf(orig))
	require.NoError(t, err)
	assert.Equal(t, orig.ID, got.ID)
	assert.Equal(t, orig.Message, got.Message)
	assert.Equal(t, orig.FinishReason, got.FinishReason)
	assert.Equal(t, orig.Usage, got.Usage)
}

// Abandoning a stream part-way is the normal case, not an error case: the
// iterator must simply stop.
func TestStreamEarlyTermination(t *testing.T) {
	produced := 0
	seq := iter.Seq2[Chunk, error](func(yield func(Chunk, error) bool) {
		for i := 0; i < 100; i++ {
			produced++
			if !yield(Chunk{Delta: Delta{Content: "x"}}, nil) {
				return
			}
		}
	})

	seen := 0
	for range seq {
		seen++
		if seen == 3 {
			break
		}
	}
	assert.Equal(t, 3, seen)
	assert.Equal(t, 3, produced, "the producer must stop when the consumer does")
}
