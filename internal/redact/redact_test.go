package redact

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/pkg/llm"
)

func tokenizeAll() *Redactor {
	return New(Policy{Default: ActionTokenize})
}

func TestActions(t *testing.T) {
	const text = "write to alice@example.com about it"
	for _, tc := range []struct {
		name   string
		policy Policy
		want   string
	}{
		{"allow", Policy{Default: ActionAllow}, text},
		{"tokenize", Policy{Default: ActionTokenize}, "write to [SLUICE_EMAIL_0001] about it"},
		{"mask", Policy{Default: ActionMask}, "write to ***************** about it"},
		{"mask keeping a suffix", Policy{Default: ActionMask, MaskKeepLast: 4},
			"write to *************.com about it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := New(tc.policy).Redact(text)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("hash is stable and unlike a placeholder", func(t *testing.T) {
		r := New(Policy{Default: ActionHash, HashSalt: []byte("pepper")})
		got, _ := r.Redact("alice@example.com and alice@example.com")
		parts := strings.Fields(got)
		assert.Equal(t, parts[0], parts[2], "the same value must hash the same way")
		assert.True(t, strings.HasPrefix(parts[0], "<EMAIL:"))
		assert.NotContains(t, got, "SLUICE_", "a hash must not look restorable")
	})

	t.Run("salt changes the hash", func(t *testing.T) {
		a, _ := New(Policy{Default: ActionHash, HashSalt: []byte("one")}).Redact("alice@example.com")
		b, _ := New(Policy{Default: ActionHash, HashSalt: []byte("two")}).Redact("alice@example.com")
		assert.NotEqual(t, a, b)
	})
}

func TestRoundTrip(t *testing.T) {
	r := tokenizeAll()
	const original = "Email alice@example.com and call +44 20 7946 0958 about invoice 12."

	redacted, vault := r.Redact(original)
	assert.NotContains(t, redacted, "alice@example.com")
	assert.NotContains(t, redacted, "7946")
	assert.Equal(t, original, vault.Restore(redacted))
}

// The property that makes tokenization usable rather than merely safe: the
// model rewrites the text around the placeholder, and restoration still works.
func TestRoundTripThroughARewrite(t *testing.T) {
	r := tokenizeAll()
	_, vault := r.Redact("Draft a reminder to alice@example.com about the invoice.")

	for _, rewritten := range []string{
		"Certainly. Here is a draft addressed to [SLUICE_EMAIL_0001].",
		"Sure — I have written to [sluice_email_0001] as requested.",
		"Done: [ SLUICE_EMAIL_0001 ] has been notified.",
		"Sent to [SLUICE-EMAIL-0001].",
		"**[SLUICE_EMAIL_0001]** has been contacted.",
		"[SLUICE_EMAIL_1]",
	} {
		t.Run(rewritten, func(t *testing.T) {
			assert.Contains(t, vault.Restore(rewritten), "alice@example.com")
		})
	}
}

func TestRestoreLeavesUnknownPlaceholders(t *testing.T) {
	r := tokenizeAll()
	_, vault := r.Redact("mail alice@example.com")

	// A model that invents a placeholder must not cause an invented value.
	const hallucinated = "I also contacted [SLUICE_EMAIL_0099] and [SLUICE_PHONE_0001]."
	assert.Equal(t, hallucinated, vault.Restore(hallucinated))
}

// A prompt that already contains something placeholder-shaped must not have its
// own text rewritten by restoration.
func TestPlaceholderCollisionAvoided(t *testing.T) {
	r := tokenizeAll()
	const original = "The template literally says [SLUICE_EMAIL_0001]; send it to alice@example.com."

	redacted, vault := r.Redact(original)
	assert.Contains(t, redacted, "[SLUICE_EMAIL_0001]", "the pre-existing token is left alone")
	assert.NotContains(t, redacted, "alice@example.com")
	assert.Equal(t, original, vault.Restore(redacted), "the round trip must still be exact")

	// The allocated placeholder skipped the reserved index.
	assert.Contains(t, redacted, "[SLUICE_EMAIL_0002]")
}

func TestCollisionAvoidedAcrossFormatVariants(t *testing.T) {
	r := tokenizeAll()
	// Reserved via the loose form, allocated via the strict one: if the
	// reservation were case-sensitive, restoration would rewrite the literal.
	const original = "See [ sluice-email-1 ] and mail bob@example.com."
	redacted, vault := r.Redact(original)
	assert.Equal(t, original, vault.Restore(redacted))
}

func TestRepeatedValueGetsOnePlaceholder(t *testing.T) {
	r := tokenizeAll()
	redacted, vault := r.Redact("alice@example.com wrote; reply to alice@example.com")
	assert.Equal(t, 2, strings.Count(redacted, "[SLUICE_EMAIL_0001]"))
	assert.Equal(t, 1, vault.Len())
}

func TestVaultSharedAcrossMessages(t *testing.T) {
	r := tokenizeAll()
	v := NewVault(nil)
	first := r.RedactWith("from alice@example.com", v)
	second := r.RedactWith("cc alice@example.com and bob@example.com", v)

	assert.Contains(t, first, "[SLUICE_EMAIL_0001]")
	assert.Contains(t, second, "[SLUICE_EMAIL_0001]", "a value seen twice keeps its placeholder")
	assert.Contains(t, second, "[SLUICE_EMAIL_0002]")
	assert.Equal(t, 2, vaultTypeCount(v, EntityEmail))
}

func vaultTypeCount(v *Vault, t EntityType) int { return v.Types()[t] }

func TestRedactRequestAndRestoreResponse(t *testing.T) {
	r := New(DefaultPolicy())
	req := llm.Request{
		Model: "local-small",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are a support agent."},
			{Role: llm.RoleUser, Content: "My name is Sarah Chen, email sarah.chen@example.com."},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "c1", Name: "lookup",
				Arguments: json.RawMessage(`{"email":"sarah.chen@example.com"}`),
			}}},
		},
	}

	clean, vault := r.RedactRequest(req)
	require.Len(t, clean.Messages, 3)
	assert.NotContains(t, clean.Messages[1].Content, "sarah.chen@example.com")
	assert.NotContains(t, clean.Messages[1].Content, "Sarah Chen")
	assert.NotContains(t, string(clean.Messages[2].ToolCalls[0].Arguments), "sarah.chen@example.com")
	assert.True(t, json.Valid(clean.Messages[2].ToolCalls[0].Arguments),
		"a placeholder must not break the JSON it sits inside")

	assert.Contains(t, req.Messages[1].Content, "sarah.chen@example.com", "the original must not be mutated")

	resp := llm.Response{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: "I have emailed " + placeholderFor(vault, EntityEmail, 1) + " on behalf of " + placeholderFor(vault, EntityPersonName, 1) + ".",
	}}
	restored := r.RestoreResponse(resp, vault)
	assert.Contains(t, restored.Message.Content, "sarah.chen@example.com")
	assert.Contains(t, restored.Message.Content, "Sarah Chen")
}

func placeholderFor(v *Vault, t EntityType, n int) string {
	return "[SLUICE_" + strings.ToUpper(string(t)) + "_000" + string(rune('0'+n)) + "]"
}

func TestDefaultPolicyChoices(t *testing.T) {
	r := New(DefaultPolicy())
	out, vault := r.Redact(
		"card 4111111111111111, key sk-Ab3xK9mQ2pL7vR4tY8wZ1nHj, mail alice@example.com")

	assert.Contains(t, out, "************1111", "cards keep the last four and are not restorable")
	assert.Contains(t, out, "<API_KEY:", "credentials are hashed")
	assert.Contains(t, out, "[SLUICE_EMAIL_0001]", "contact details are reversible")

	// Only the reversible entity is in the vault; a masked or hashed value is
	// gone for good, which is the point of choosing those actions.
	assert.Equal(t, 1, vault.Len())
	assert.NotContains(t, vault.Restore(out), "4111111111111111")
}

func TestPolicyIsCopied(t *testing.T) {
	p := DefaultPolicy()
	r := New(p)
	p.ByType[EntityEmail] = ActionAllow

	out, _ := r.Redact("alice@example.com")
	assert.NotEqual(t, "alice@example.com", out, "mutating the caller's map must not change the redactor")
	assert.Equal(t, ActionTokenize, r.Policy().For(EntityEmail))
}

func TestPolicyWithHelpers(t *testing.T) {
	base := DefaultPolicy()
	allowEmail := base.With(EntityEmail, ActionAllow)
	assert.Equal(t, ActionTokenize, base.For(EntityEmail))
	assert.Equal(t, ActionAllow, allowEmail.For(EntityEmail))

	all := base.WithAll(ActionMask)
	for _, typ := range []EntityType{EntityEmail, EntityAPIKey, EntityPersonName, "unknown"} {
		assert.Equal(t, ActionMask, all.For(typ), string(typ))
	}
}

func TestEmptyAndNoMatchInputs(t *testing.T) {
	r := tokenizeAll()
	for _, text := range []string{"", "nothing sensitive here", "  ", "\x00\x01"} {
		out, v := r.Redact(text)
		assert.Equal(t, text, out)
		assert.Zero(t, v.Len())
		assert.Equal(t, text, v.Restore(text))
	}
}

// The vault is read by the response path while the request path may still be
// writing to it, so its concurrency is a correctness property, not a nicety.
func TestVaultConcurrentUse(t *testing.T) {
	r := tokenizeAll()
	v := NewVault(nil)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.RedactWith("mail user"+string(rune('a'+i%26))+"@example.com now", v)
			_ = v.Restore("check [SLUICE_EMAIL_0001] please")
			_ = v.Len()
			_ = v.Types()
		}(i)
	}
	wg.Wait()
	assert.Positive(t, v.Len())
}

func TestMask(t *testing.T) {
	for _, tc := range []struct {
		value string
		keep  int
		want  string
	}{
		{"4111111111111111", 4, "************1111"},
		{"abc", 0, "***"},
		{"abc", 10, "***"}, // keeping more than exists would reveal everything
		{"abc", -1, "***"},
		{"héllo", 2, "***lo"}, // runes, not bytes
	} {
		assert.Equalf(t, tc.want, mask(tc.value, tc.keep), "%q keep %d", tc.value, tc.keep)
	}
}

func BenchmarkRedact(b *testing.B) {
	r := New(DefaultPolicy())
	text := "My name is David Larsen, email d.larsen@example.org, phone +47 22 12 34 56."
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Redact(text)
	}
}
