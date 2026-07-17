package llm

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
)

// Provider is one upstream that can serve completions.
//
// Implementations must be safe for concurrent use: the gateway holds one
// instance per configured upstream and calls it from every request goroutine.
//
// Every error an implementation returns should be an *Error. Returning a bare
// error is not a bug -- CodeOf degrades it to CodeUnknown -- but an unknown
// code disables retry and failover, so an adapter that cannot be bothered to
// classify silently gives up the availability the gateway exists to provide.
type Provider interface {
	// Name identifies the upstream in metrics, logs and Response.Provider.
	Name() string

	// Complete performs a single non-streaming completion.
	Complete(ctx context.Context, req Request) (Response, error)

	// Stream performs a streaming completion.
	//
	// The outer error reports failures that happen before any token is
	// produced -- a rejected request, a refused connection -- so that callers
	// can fail over without having committed to a response. Failures after the
	// first token arrive through the sequence, because by then some output has
	// already reached the client and a retry would duplicate it.
	//
	// The sequence yields at most one error, and stops immediately after doing
	// so. Consumers may abandon it early (break out of the range); an
	// implementation must release its resources when the iteration ends, which
	// for range-over-func means a defer in the iterator body.
	//
	// Why iter.Seq2 rather than a channel: a channel-returning API leaks a
	// goroutine unless every consumer drains it to completion, and consumers of
	// a token stream abandon it constantly -- client disconnects, budget
	// exhaustion, a guardrail tripping mid-generation. With range-over-func the
	// producer runs on the consumer's goroutine, early termination is a normal
	// return from yield, and there is nothing to leak. The price is that a
	// consumer cannot select over a stream alongside other events; a consumer
	// that needs to can convert with a goroutine it owns and can therefore
	// reason about.
	Stream(ctx context.Context, req Request) (iter.Seq2[Chunk, error], error)
}

// Collect drains a chunk sequence into the Response it describes.
//
// It exists so that "stream upstream, buffer for a non-streaming client" is one
// call rather than a delta-reassembly loop copied into every adapter, and so
// that the reassembly is tested once.
func Collect(seq iter.Seq2[Chunk, error]) (Response, error) {
	var (
		resp     Response
		content  strings.Builder
		toolArgs = map[int]*strings.Builder{}
		toolMeta = map[int]ToolCall{}
		maxIndex = -1
	)
	for chunk, err := range seq {
		if err != nil {
			return resp, err
		}
		if resp.ID == "" {
			resp.ID = chunk.ID
		}
		if resp.Model == "" {
			resp.Model = chunk.Model
		}
		if resp.Provider == "" {
			resp.Provider = chunk.Provider
		}
		if chunk.Delta.Role != "" {
			resp.Message.Role = chunk.Delta.Role
		}
		content.WriteString(chunk.Delta.Content)
		for _, tc := range chunk.Delta.ToolCalls {
			if tc.Index > maxIndex {
				maxIndex = tc.Index
			}
			b, ok := toolArgs[tc.Index]
			if !ok {
				b = &strings.Builder{}
				toolArgs[tc.Index] = b
			}
			b.WriteString(tc.ArgumentsDelta)
			meta := toolMeta[tc.Index]
			if tc.ID != "" {
				meta.ID = tc.ID
			}
			if tc.Name != "" {
				meta.Name = tc.Name
			}
			toolMeta[tc.Index] = meta
		}
		if chunk.FinishReason != "" {
			resp.FinishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			resp.Usage = *chunk.Usage
		}
	}
	if resp.Message.Role == "" {
		resp.Message.Role = RoleAssistant
	}
	resp.Message.Content = content.String()
	for i := 0; i <= maxIndex; i++ {
		call := toolMeta[i]
		if b, ok := toolArgs[i]; ok {
			call.Arguments = json.RawMessage(b.String())
		}
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, call)
	}
	return resp, nil
}

// StreamOf turns a completed response into a single-chunk sequence.
//
// The cache uses it to serve a streaming request from a stored response. The
// result is one large chunk rather than a re-chunked imitation of the original
// stream: pretending a cache hit arrived token by token would fabricate timing
// data that downstream latency metrics would then report as real.
func StreamOf(resp Response) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		usage := resp.Usage
		chunk := Chunk{
			ID:       resp.ID,
			Model:    resp.Model,
			Provider: resp.Provider,
			Delta: Delta{
				Role:    resp.Message.Role,
				Content: resp.Message.Content,
			},
			FinishReason: resp.FinishReason,
			Usage:        &usage,
		}
		for i, tc := range resp.Message.ToolCalls {
			chunk.Delta.ToolCalls = append(chunk.Delta.ToolCalls, ToolCallDelta{
				Index:          i,
				ID:             tc.ID,
				Name:           tc.Name,
				ArgumentsDelta: string(tc.Arguments),
			})
		}
		yield(chunk, nil)
	}
}
