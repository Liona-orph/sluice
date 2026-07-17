package llm

import "encoding/json"

// Role identifies who produced a message.
//
// The set is deliberately closed. Providers disagree about names ("model" vs
// "assistant", "function" vs "tool") and about whether a system prompt is a
// message or a separate field; both disagreements are the adapter's problem,
// not the gateway's.
type Role string

const (
	// RoleSystem carries instructions that frame the whole conversation.
	RoleSystem Role = "system"
	// RoleUser carries input from the calling application's end user.
	RoleUser Role = "user"
	// RoleAssistant carries model output, including requests to call tools.
	RoleAssistant Role = "assistant"
	// RoleTool carries the result of a tool call back to the model. It must
	// name the call it answers via Message.ToolCallID.
	RoleTool Role = "tool"
)

// Valid reports whether r is one of the defined roles.
func (r Role) Valid() bool {
	switch r {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	}
	return false
}

// Message is one turn of a conversation.
//
// Content is a plain string rather than a list of typed parts. That rules out
// images and audio, which is a real limitation and a deliberate one: multimodal
// content would push every component in the gateway -- redaction, token
// counting, cache keying -- into handling opaque blobs it cannot inspect, and
// none of Sluice's four jobs applies to a JPEG. Adding parts later is a
// backwards-compatible change (a Parts field beside Content); pretending to
// support them now is not.
type Message struct {
	Role Role `json:"role"`
	// Content is the text of the turn. It may be empty on an assistant message
	// that only requests tool calls.
	Content string `json:"content,omitempty"`
	// Name optionally distinguishes participants sharing a role, e.g. several
	// users in a group chat. Most providers ignore it.
	Name string `json:"name,omitempty"`
	// ToolCalls is set on assistant messages that ask the caller to run tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on RoleTool messages and names the ToolCall.ID being
	// answered. A tool result that does not name its call is ambiguous as soon
	// as a model requests two calls in one turn, so it is required.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	// ID is provider-assigned and only needs to be unique within a response.
	ID string `json:"id"`
	// Name is the tool being called.
	Name string `json:"name"`
	// Arguments is the raw JSON object the model produced. It is kept raw
	// because it frequently is not valid JSON -- models truncate and hallucinate
	// -- and the gateway must pass through what was actually generated rather
	// than fail a request it could have delivered.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Tool declares a callable function to the model.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is a JSON Schema object. It is raw for the same reason
	// ToolCall.Arguments is: the gateway has no business validating a schema it
	// will only forward.
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// ToolChoice constrains whether and which tool the model may call.
type ToolChoice string

const (
	// ToolChoiceAuto lets the model decide. It is the zero value, so a request
	// that declares tools without an opinion behaves the way callers expect.
	ToolChoiceAuto ToolChoice = ""
	// ToolChoiceNone forbids tool calls for this turn.
	ToolChoiceNone ToolChoice = "none"
	// ToolChoiceRequired demands at least one tool call.
	ToolChoiceRequired ToolChoice = "required"
)

// CloneMessages returns a deep copy of msgs.
//
// Used wherever a request crosses a boundary that may retain it -- the cache
// stores responses, the redactor rewrites content in place -- so that a caller
// reusing its slice cannot observe someone else's edits.
func CloneMessages(msgs []Message) []Message {
	if msgs == nil {
		return nil
	}
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if m.ToolCalls != nil {
			out[i].ToolCalls = make([]ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				out[i].ToolCalls[j] = tc
				if tc.Arguments != nil {
					out[i].ToolCalls[j].Arguments = append(json.RawMessage(nil), tc.Arguments...)
				}
			}
		}
	}
	return out
}
