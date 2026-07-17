package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseRequest() Request {
	return Request{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: RoleSystem, Content: "You are terse."},
			{Role: RoleUser, Content: "hello"},
		},
		Temperature: Ptr(0.2),
		MaxTokens:   256,
	}
}

func TestFingerprintStability(t *testing.T) {
	a, b := baseRequest(), baseRequest()
	assert.Equal(t, a.Fingerprint(), b.Fingerprint())
	assert.Len(t, a.Fingerprint(), 64)
}

func TestFingerprintSensitivity(t *testing.T) {
	base := baseRequest()
	for _, tc := range []struct {
		name    string
		mutate  func(*Request)
		differs bool
	}{
		{"model", func(r *Request) { r.Model = "gpt-4o-mini" }, true},
		{"message content", func(r *Request) { r.Messages[1].Content = "hi" }, true},
		{"message role", func(r *Request) { r.Messages[1].Role = RoleAssistant }, true},
		{"message order", func(r *Request) { r.Messages[0], r.Messages[1] = r.Messages[1], r.Messages[0] }, true},
		{"max tokens", func(r *Request) { r.MaxTokens = 512 }, true},
		{"temperature value", func(r *Request) { r.Temperature = Ptr(0.7) }, true},
		{"temperature unset", func(r *Request) { r.Temperature = nil }, true},
		{"stop sequences", func(r *Request) { r.Stop = []string{"\n\n"} }, true},
		{"seed", func(r *Request) { r.Seed = Ptr(int64(7)) }, true},
		{"tools", func(r *Request) { r.Tools = []Tool{{Name: "search"}} }, true},
		{"tool choice", func(r *Request) { r.ToolChoice = ToolChoiceRequired }, true},
		{"provider options", func(r *Request) {
			r.ProviderOptions = map[string]json.RawMessage{"openai": json.RawMessage(`{"logprobs":true}`)}
		}, true},

		// Excluded on purpose: two tenants asking the same question share an
		// answer, and attribution is not part of the question.
		{"metadata", func(r *Request) { r.Metadata = map[string]string{"tenant": "acme"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := baseRequest()
			tc.mutate(&mutated)
			if tc.differs {
				assert.NotEqual(t, base.Fingerprint(), mutated.Fingerprint())
			} else {
				assert.Equal(t, base.Fingerprint(), mutated.Fingerprint())
			}
		})
	}
}

// Length prefixing is what stops adjacent fields from running together; without
// it these two requests would hash identically.
func TestFingerprintIsNotConcatenation(t *testing.T) {
	a := Request{Model: "ab", Messages: []Message{{Role: RoleUser, Content: "c"}}}
	b := Request{Model: "a", Messages: []Message{{Role: RoleUser, Content: "bc"}}}
	assert.NotEqual(t, a.Fingerprint(), b.Fingerprint())

	c := Request{Messages: []Message{{Content: "ab"}, {Content: "c"}}}
	d := Request{Messages: []Message{{Content: "a"}, {Content: "bc"}}}
	assert.NotEqual(t, c.Fingerprint(), d.Fingerprint())
}

func TestFingerprintDistinguishesZeroFromUnset(t *testing.T) {
	unset := Request{Model: "m"}
	zero := Request{Model: "m", Temperature: Ptr(0.0)}
	assert.NotEqual(t, unset.Fingerprint(), zero.Fingerprint())
}

func TestUsageAdd(t *testing.T) {
	a := Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 2}
	b := Usage{InputTokens: 1, OutputTokens: 1, Estimated: true}
	sum := a.Add(b)
	assert.Equal(t, Usage{InputTokens: 11, OutputTokens: 6, CachedInputTokens: 2, Estimated: true}, sum)
	assert.Equal(t, 17, sum.TotalTokens())
}

func TestCloneMessagesIsDeep(t *testing.T) {
	orig := []Message{{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: "1", Name: "search", Arguments: json.RawMessage(`{"q":"x"}`)}},
	}}
	clone := CloneMessages(orig)
	require.Len(t, clone, 1)

	clone[0].ToolCalls[0].Arguments[2] = 'X'
	clone[0].ToolCalls[0].Name = "other"
	assert.Equal(t, "search", orig[0].ToolCalls[0].Name)
	assert.JSONEq(t, `{"q":"x"}`, string(orig[0].ToolCalls[0].Arguments))

	assert.Nil(t, CloneMessages(nil))
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool} {
		assert.True(t, r.Valid(), string(r))
	}
	assert.False(t, Role("function").Valid())
	assert.False(t, Role("").Valid())
}
