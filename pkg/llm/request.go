package llm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"math"
	"sort"
	"time"
)

// Request is a chat completion request in the gateway's own terms.
type Request struct {
	// Model is the logical model name, e.g. "gpt-4o" or "local-small". The
	// router may map it to a different physical model on a different provider;
	// Response.Model reports what actually served the request.
	Model string `json:"model"`

	Messages []Message `json:"messages"`

	Tools      []Tool     `json:"tools,omitempty"`
	ToolChoice ToolChoice `json:"tool_choice,omitempty"`

	// MaxTokens bounds the completion. Zero means the provider's default.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature, TopP: nil means "do not send", which is not the same as
	// sending zero. See Ptr.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	// Stop are sequences that end generation.
	Stop []string `json:"stop,omitempty"`
	// Seed requests reproducible sampling where the provider supports it.
	Seed *int64 `json:"seed,omitempty"`

	// Metadata is gateway-side context: tenant, application, end-user ID, cost
	// centre. It is never forwarded to a provider and never affects the output,
	// which is why Fingerprint ignores it. Putting billing attribution here
	// rather than in a parallel argument keeps it attached to the request as it
	// moves through the pipeline.
	Metadata map[string]string `json:"metadata,omitempty"`

	// ProviderOptions carries vendor extensions, keyed by provider name. An
	// adapter reads only its own key. It participates in Fingerprint because an
	// option that does not change the output does not belong here.
	ProviderOptions map[string]json.RawMessage `json:"provider_options,omitempty"`
}

// FinishReason says why generation stopped.
type FinishReason string

const (
	// FinishStop is a natural end or a stop sequence.
	FinishStop FinishReason = "stop"
	// FinishLength means MaxTokens or the context window truncated the output.
	// It is worth distinguishing from FinishStop because a truncated answer is
	// usually wrong to cache and always wrong to bill as a success.
	FinishLength FinishReason = "length"
	// FinishToolCalls means the model stopped to request tool calls.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishContentFilter means the provider suppressed the output.
	FinishContentFilter FinishReason = "content_filter"
)

// Usage is the token accounting for one request/response pair.
//
// Counts come from the provider when it reports them and from the tokenizer
// when it does not; Estimated records which, because a cost report built partly
// from estimates should say so rather than imply a precision it lacks.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CachedInputTokens is the subset of InputTokens the provider served from
	// its own prompt cache, usually at a discount. It is a subset, not an
	// addition, so InputTokens already includes it.
	CachedInputTokens int  `json:"cached_input_tokens,omitempty"`
	Estimated         bool `json:"estimated,omitempty"`
}

// TotalTokens is the billable token count in the usual sense.
func (u Usage) TotalTokens() int { return u.InputTokens + u.OutputTokens }

// Add sums two usage records, used to aggregate a retried or failed-over
// request whose earlier attempts still cost money.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		InputTokens:       u.InputTokens + v.InputTokens,
		OutputTokens:      u.OutputTokens + v.OutputTokens,
		CachedInputTokens: u.CachedInputTokens + v.CachedInputTokens,
		Estimated:         u.Estimated || v.Estimated,
	}
}

// Response is a completed chat completion.
type Response struct {
	// ID is the provider's identifier where it supplies one, otherwise a
	// deterministic identifier derived by the adapter.
	ID string `json:"id"`
	// Model is the model that actually produced the response, which may differ
	// from Request.Model after routing or failover.
	Model string `json:"model"`
	// Provider names the adapter that served the request.
	Provider string `json:"provider"`

	Message      Message      `json:"message"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage"`

	// Created is when the provider produced the response.
	Created time.Time `json:"created"`
	// Latency is the wall-clock time the adapter observed. It is recorded here
	// rather than in a side channel so that a cached response can report the
	// latency of the call that originally produced it.
	Latency time.Duration `json:"latency,omitempty"`
}

// Delta is the incremental part of a streamed response.
type Delta struct {
	// Role is set on the first chunk only.
	Role Role `json:"role,omitempty"`
	// Content is the text appended by this chunk.
	Content string `json:"content,omitempty"`
	// ToolCalls are partial tool calls, addressed by index because arguments
	// arrive as a byte stream spread over many chunks.
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is a fragment of a tool call.
type ToolCallDelta struct {
	// Index positions this fragment within the response's tool call list.
	Index int `json:"index"`
	// ID and Name are sent once, on the fragment that opens the call.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	// ArgumentsDelta is appended to the call's argument buffer.
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
}

// Chunk is one element of a streamed response.
type Chunk struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Delta    Delta  `json:"delta"`
	// FinishReason is set on the final content chunk.
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	// Usage is set on the last chunk, if the provider reports it at all. It is
	// a pointer because "no usage yet" and "zero tokens" are different claims.
	Usage *Usage `json:"usage,omitempty"`
}

// Fingerprint is a stable hash of everything about the request that can change
// the output. It is the basis of the exact-match cache key and of the local
// provider's deterministic seed.
//
// Included: model, every message field, tools and tool choice, max tokens,
// temperature, top_p, stop sequences, seed, and provider options.
//
// Excluded, and why:
//
//   - Metadata. Tenant and user IDs are billing attribution; two tenants asking
//     the identical question should share a cache entry, and the cache's own
//     namespacing (not the key) is where isolation belongs if a deployment
//     needs it.
//   - Whether the call is streaming. The content is the same either way, so a
//     streamed request may be served from a cache entry filled by a
//     non-streamed one.
//
// Two requests that differ only in field ordering inside ProviderOptions JSON
// hash differently. Canonicalising arbitrary JSON is more machinery than the
// case deserves; adapters that build options programmatically produce stable
// ordering anyway.
func (r Request) Fingerprint() string {
	h := sha256.New()
	hashString(h, "sluice/v1")
	hashString(h, r.Model)

	hashInt(h, len(r.Messages))
	for _, m := range r.Messages {
		hashString(h, string(m.Role))
		hashString(h, m.Content)
		hashString(h, m.Name)
		hashString(h, m.ToolCallID)
		hashInt(h, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			hashString(h, tc.ID)
			hashString(h, tc.Name)
			hashBytes(h, tc.Arguments)
		}
	}

	hashInt(h, len(r.Tools))
	for _, t := range r.Tools {
		hashString(h, t.Name)
		hashString(h, t.Description)
		hashBytes(h, t.Parameters)
	}
	hashString(h, string(r.ToolChoice))

	hashInt(h, r.MaxTokens)
	hashOptFloat(h, r.Temperature)
	hashOptFloat(h, r.TopP)
	hashInt(h, len(r.Stop))
	for _, s := range r.Stop {
		hashString(h, s)
	}
	if r.Seed != nil {
		hashString(h, "seed")
		hashInt(h, int(*r.Seed))
	}

	keys := make([]string, 0, len(r.ProviderOptions))
	for k := range r.ProviderOptions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	hashInt(h, len(keys))
	for _, k := range keys {
		hashString(h, k)
		hashBytes(h, r.ProviderOptions[k])
	}

	return hex.EncodeToString(h.Sum(nil))
}

// hashString writes a length-prefixed value. The prefix is what stops
// {"ab", "c"} from hashing the same as {"a", "bc"}.
func hashString(h hash.Hash, s string) {
	hashInt(h, len(s))
	_, _ = h.Write([]byte(s))
}

func hashBytes(h hash.Hash, b []byte) {
	hashInt(h, len(b))
	_, _ = h.Write(b)
}

func hashInt(h hash.Hash, n int) {
	var buf [8]byte
	// The conversion is deliberate: this is a hash, so a wrapped value is
	// still a stable one, and every n reaching here is a length or a seed.
	binary.LittleEndian.PutUint64(buf[:], uint64(n)) //nolint:gosec // hashing, not arithmetic
	_, _ = h.Write(buf[:])
}

func hashOptFloat(h hash.Hash, f *float64) {
	if f == nil {
		_, _ = h.Write([]byte{0})
		return
	}
	_, _ = h.Write([]byte{1})
	var buf [8]byte
	// The bit pattern rather than the decimal rendering: 0.1 must hash the same
	// on every platform, and its shortest decimal form is not unique.
	binary.LittleEndian.PutUint64(buf[:], math.Float64bits(*f))
	_, _ = h.Write(buf[:])
}
