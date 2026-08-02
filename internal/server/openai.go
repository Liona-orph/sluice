package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sluice-gw/sluice/internal/gateway"
	"github.com/sluice-gw/sluice/pkg/llm"
)

// The OpenAI-compatible surface: what is implemented and what is ignored.
//
// The point of speaking OpenAI's dialect is that an application already using
// an OpenAI SDK can point its base URL at Sluice and keep working. That only
// holds if the compatibility is honest about its edges, so they are listed
// here rather than discovered in production.
//
// Implemented on POST /v1/chat/completions:
//
//	model, messages (system/user/assistant/tool, with name and tool_call_id),
//	max_tokens, max_completion_tokens, temperature, top_p, stop (string or
//	array), seed, stream, stream_options.include_usage, tools (type=function),
//	tool_choice (none/auto/required), user.
//
// Accepted and deliberately ignored, with the reason:
//
//	n                     Only n=1 is served. A value above 1 is rejected
//	                      rather than silently downgraded, because a caller
//	                      asking for five candidates and receiving one gets
//	                      wrong results rather than an error.
//	logprobs, top_logprobs
//	                      Sluice's Response carries no token-level data and
//	                      inventing it would be worse than omitting it. Rejected.
//	frequency_penalty, presence_penalty, logit_bias
//	                      Not in llm.Request. They are OpenAI-shaped sampling
//	                      knobs with no cross-provider meaning; a request that
//	                      sets them is accepted and they are dropped, which is
//	                      stated here and in the response's sluice_ignored field.
//	response_format, parallel_tool_calls, service_tier, store, metadata
//	                      Same: accepted, dropped, reported.
//	function_call, functions
//	                      The pre-2023 tool API. Rejected with a message
//	                      pointing at tools/tool_choice.
//
// Not implemented at all: the assistants, embeddings, images, audio, files,
// batches and fine-tuning surfaces. Sluice is a chat-completions gateway; a
// stub that returned an empty list from /v1/assistants would be a lie that
// costs someone an afternoon.
//
// Response shape: id, object, created, model, choices[0], usage,
// system_fingerprint. Sluice's own metadata -- which provider answered, whether
// it was a cache hit, what it cost -- goes in X-Sluice-* response headers
// rather than in the body, so that a strict client parsing the OpenAI schema
// does not choke on an unexpected field.

// chatRequest is the OpenAI chat completion request.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`

	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Temperature         *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	Stop                stopList `json:"stop"`
	Seed                *int64   `json:"seed"`
	N                   *int     `json:"n"`

	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options"`

	Tools      []chatTool      `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`

	// User is OpenAI's end-user identifier. It is carried into Request.Metadata
	// for the audit record and never forwarded upstream.
	User string `json:"user"`

	// The pre-tools function API, accepted only so that the error can name it.
	Functions    json.RawMessage `json:"functions"`
	FunctionCall json.RawMessage `json:"function_call"`

	// Knobs that are accepted and dropped. They are declared so that
	// DisallowUnknownFields can stay on: an unknown field is far more likely to
	// be a typo in a field that matters than a new OpenAI feature, and a typo
	// in "temperature" that is silently ignored is a bug nobody finds.
	LogProbs          *bool           `json:"logprobs"`
	TopLogProbs       *int            `json:"top_logprobs"`
	FrequencyPenalty  *float64        `json:"frequency_penalty"`
	PresencePenalty   *float64        `json:"presence_penalty"`
	LogitBias         json.RawMessage `json:"logit_bias"`
	ResponseFormat    json.RawMessage `json:"response_format"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	ServiceTier       *string         `json:"service_tier"`
	Store             *bool           `json:"store"`
	Metadata          json.RawMessage `json:"metadata"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// stopList accepts either a string or an array of strings, which is what the
// OpenAI schema permits and what every SDK therefore emits.
type stopList []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *stopList) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		*s = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("stop must be a string or an array of strings: %w", err)
	}
	*s = many
	return nil
}

// ignoredFields lists the fields present in a request that Sluice dropped, so
// that the handler can report them in a header. Silently ignoring a sampling
// parameter changes the output without telling anyone.
func (r *chatRequest) ignoredFields() []string {
	var out []string
	add := func(present bool, name string) {
		if present {
			out = append(out, name)
		}
	}
	add(r.FrequencyPenalty != nil, "frequency_penalty")
	add(r.PresencePenalty != nil, "presence_penalty")
	add(len(r.LogitBias) > 0, "logit_bias")
	add(len(r.ResponseFormat) > 0, "response_format")
	add(r.ParallelToolCalls != nil, "parallel_tool_calls")
	add(r.ServiceTier != nil, "service_tier")
	add(r.Store != nil, "store")
	add(len(r.Metadata) > 0, "metadata")
	return out
}

// toLLM converts an OpenAI request into Sluice's own vocabulary, rejecting the
// forms that cannot be served correctly.
func (r *chatRequest) toLLM() (llm.Request, error) {
	if r.Model == "" {
		return llm.Request{}, gateway.Invalid("'model' is a required property")
	}
	if len(r.Messages) == 0 {
		return llm.Request{}, gateway.Invalid("'messages' must contain at least one message")
	}
	if r.N != nil && *r.N != 1 {
		return llm.Request{}, gateway.Invalid("'n' must be 1; this gateway serves a single completion per request")
	}
	if r.LogProbs != nil && *r.LogProbs {
		return llm.Request{}, gateway.Invalid("'logprobs' is not supported; this gateway does not carry token-level data")
	}
	if len(r.Functions) > 0 || len(r.FunctionCall) > 0 {
		return llm.Request{}, gateway.Invalid("'functions' and 'function_call' are the superseded API; use 'tools' and 'tool_choice'")
	}

	out := llm.Request{
		Model:       r.Model,
		Temperature: r.Temperature,
		TopP:        r.TopP,
		Stop:        r.Stop,
		Seed:        r.Seed,
	}
	switch {
	case r.MaxCompletionTokens != nil:
		out.MaxTokens = *r.MaxCompletionTokens
	case r.MaxTokens != nil:
		out.MaxTokens = *r.MaxTokens
	}
	if r.User != "" {
		out.Metadata = map[string]string{"end_user": r.User}
	}

	for i, m := range r.Messages {
		msg, err := m.toLLM(i)
		if err != nil {
			return llm.Request{}, err
		}
		out.Messages = append(out.Messages, msg)
	}

	for i, t := range r.Tools {
		if t.Type != "" && t.Type != "function" {
			return llm.Request{}, gateway.Invalid("tools[%d].type %q is not supported; only \"function\" is", i, t.Type)
		}
		if t.Function.Name == "" {
			return llm.Request{}, gateway.Invalid("tools[%d].function.name is required", i)
		}
		out.Tools = append(out.Tools, llm.Tool{
			Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}

	choice, err := parseToolChoice(r.ToolChoice)
	if err != nil {
		return llm.Request{}, err
	}
	out.ToolChoice = choice
	return out, nil
}

// parseToolChoice maps OpenAI's tool_choice onto llm.ToolChoice.
//
// The object form -- {"type":"function","function":{"name":"x"}} -- is
// rejected rather than approximated as "required". Approximating it would let
// the model call a different tool than the one the caller demanded, which is a
// correctness failure disguised as compatibility.
func parseToolChoice(raw json.RawMessage) (llm.ToolChoice, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return llm.ToolChoiceAuto, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none":
			return llm.ToolChoiceNone, nil
		case "auto":
			return llm.ToolChoiceAuto, nil
		case "required":
			return llm.ToolChoiceRequired, nil
		default:
			return "", gateway.Invalid("'tool_choice' must be none, auto or required; got %q", s)
		}
	}
	return "", gateway.Invalid("'tool_choice' naming a specific function is not supported by this gateway")
}

// toLLM converts one message, accepting both the plain-string content form and
// the array-of-parts form that newer SDKs emit.
func (m chatMessage) toLLM(index int) (llm.Message, error) {
	role := llm.Role(m.Role)
	if !role.Valid() {
		return llm.Message{}, gateway.Invalid("messages[%d].role %q is not one of system, user, assistant, tool", index, m.Role)
	}
	content, err := decodeContent(m.Content, index)
	if err != nil {
		return llm.Message{}, err
	}
	out := llm.Message{Role: role, Content: content, Name: m.Name, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, llm.ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	if role == llm.RoleTool && out.ToolCallID == "" {
		return llm.Message{}, gateway.Invalid("messages[%d] has role tool but no tool_call_id", index)
	}
	return out, nil
}

// decodeContent handles the two shapes OpenAI allows for message content.
//
// The array form exists for multimodal input. Sluice's Message is text-only
// (see pkg/llm), so text parts are concatenated and any other part type is a
// hard error: forwarding a request with the image silently dropped would
// produce an answer to a different question.
func decodeContent(raw json.RawMessage, index int) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", gateway.Invalid("messages[%d].content is not valid JSON: %v", index, err)
		}
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", gateway.Invalid("messages[%d].content must be a string or an array of content parts", index)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" && p.Type != "" {
			return "", gateway.Invalid("messages[%d].content contains a %q part; this gateway is text-only", index, p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// --- responses --------------------------------------------------------------

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
	// SystemFingerprint identifies the serving configuration. Sluice reports
	// the provider that answered, which is the closest true statement it can
	// make and is more useful than an opaque hash.
	SystemFingerprint string `json:"system_fingerprint,omitempty"`
}

type chatChoice struct {
	Index        int              `json:"index"`
	Message      *chatOutMessage  `json:"message,omitempty"`
	Delta        *chatOutMessage  `json:"delta,omitempty"`
	FinishReason *string          `json:"finish_reason"`
	LogProbs     *json.RawMessage `json:"logprobs,omitempty"`
}

type chatOutMessage struct {
	Role      string            `json:"role,omitempty"`
	Content   *string           `json:"content"`
	ToolCalls []chatOutToolCall `json:"tool_calls,omitempty"`
}

type chatOutToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails carries the provider-cached subset, matching the
	// field OpenAI added for prompt caching.
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func usageOf(u llm.Usage) *chatUsage {
	out := &chatUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens(),
	}
	if u.CachedInputTokens > 0 {
		out.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: u.CachedInputTokens}
	}
	return out
}

// finishReasonOf maps Sluice's finish reasons onto OpenAI's strings. They
// coincide, but the mapping is written out so that a future divergence is a
// change here rather than a silent pass-through of an unknown value.
func finishReasonOf(r llm.FinishReason) *string {
	var s string
	switch r {
	case llm.FinishStop:
		s = "stop"
	case llm.FinishLength:
		s = "length"
	case llm.FinishToolCalls:
		s = "tool_calls"
	case llm.FinishContentFilter:
		s = "content_filter"
	default:
		return nil
	}
	return &s
}

func toolCallsOf(calls []llm.ToolCall, withIndex bool) []chatOutToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]chatOutToolCall, 0, len(calls))
	for i, tc := range calls {
		var c chatOutToolCall
		if withIndex {
			idx := i
			c.Index = &idx
		}
		c.ID = tc.ID
		c.Type = "function"
		c.Function.Name = tc.Name
		c.Function.Arguments = string(tc.Arguments)
		out = append(out, c)
	}
	return out
}

// errorBody is OpenAI's error envelope.
type errorBody struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func errorBodyOf(e *gateway.Error) errorBody {
	var b errorBody
	b.Error.Message = e.Message
	b.Error.Type = string(e.Kind)
	if e.Code != "" {
		code := e.Code
		b.Error.Code = &code
	}
	return b
}

// modelList is the /v1/models response.
type modelList struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}
