package redact

import (
	"iter"
	"strings"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// MaxPlaceholderLen bounds how much text a StreamRestorer holds back.
//
// The longest placeholder tokenize can emit is "[SLUICE_CREDIT_CARD_0001]", 25
// bytes. placeholderPattern is deliberately looser than that -- it tolerates
// spaces, hyphens and up to six digits, because models rewrite tokens they do
// not understand -- so the bound has to cover the loose form: brackets, spaces
// inside them, the longest entity name, separators and six digits. 64 bytes is
// comfortably above that and small enough that holding it back is invisible.
//
// A restorer that held back less would split a placeholder across two chunks
// and emit the first half in the clear, which is the streaming version of the
// bug StreamRedactor exists to prevent, running in the opposite direction.
const MaxPlaceholderLen = 64

// StreamRestorer reverses tokenization over text arriving in arbitrary chunks.
//
// The problem is the mirror image of the one StreamRedactor solves. A model
// emits "[SLUICE_EMAIL" in one chunk and "_0001] is the address" in the next;
// restoring chunk by chunk finds no placeholder in either and the caller
// receives a token instead of the address they sent. Worse, a client displaying
// the stream shows the token for a moment and then never corrects it.
//
// The fix is a look-behind, but a much cheaper one than the redactor's. A
// placeholder always begins with '[', so the only text that has to be held back
// is a trailing run starting at the last unclosed '['. Everything before it is
// decidable now. In the overwhelmingly common case there is no '[' at all in
// the tail and the chunk passes through untouched, which is why streaming
// restoration costs a scan for one byte per chunk rather than the full detector
// pass streaming redaction costs.
//
// A StreamRestorer is not safe for concurrent use; it belongs to one stream.
type StreamRestorer struct {
	vault   *Vault
	pending string
	done    bool
}

// NewStreamRestorer returns a restorer backed by vault.
func NewStreamRestorer(vault *Vault) *StreamRestorer {
	return &StreamRestorer{vault: vault}
}

// Write accepts the next chunk and returns whatever can now be safely emitted.
func (s *StreamRestorer) Write(chunk string) string {
	if s.vault == nil {
		return chunk
	}
	s.pending += chunk
	cut := s.safeCut(s.pending)
	if cut <= 0 {
		return ""
	}
	out := s.vault.Restore(s.pending[:cut])
	s.pending = s.pending[cut:]
	return out
}

// Flush returns the remaining text, restored. It must be called once when the
// stream ends: an answer that ends on a placeholder leaves it in the buffer.
func (s *StreamRestorer) Flush() string {
	if s.done || s.vault == nil {
		return ""
	}
	s.done = true
	out := s.vault.Restore(s.pending)
	s.pending = ""
	return out
}

// Pending is how many bytes are currently held back, for tests that assert the
// look-behind stays bounded.
func (s *StreamRestorer) Pending() int { return len(s.pending) }

// safeCut returns the length of the prefix of text that cannot be extended into
// a placeholder by a future chunk.
func (s *StreamRestorer) safeCut(text string) int {
	open := strings.LastIndexByte(text, '[')
	if open < 0 {
		return len(text)
	}
	tail := text[open:]
	// A closed bracket is decidable now: either it is a placeholder the vault
	// knows, or it is not, and no future chunk changes that.
	if strings.IndexByte(tail, ']') >= 0 {
		return len(text)
	}
	// An unclosed run already longer than any placeholder cannot become one.
	if len(tail) >= MaxPlaceholderLen {
		return len(text)
	}
	return open
}

// RestoreStream wraps a chunk sequence, restoring tokenized values in the text
// deltas as they pass.
//
// Like RedactStream it holds the terminating chunk back so that the flushed
// tail has somewhere to go, and it runs entirely on the consumer's goroutine so
// that abandoning the sequence leaks nothing.
//
// Tool call arguments are restored per fragment rather than across fragments.
// Arguments arrive as a byte stream split at arbitrary points, so the same
// straddling problem exists there in principle; it is not solved here because
// doing it properly means buffering a whole argument document, which defeats
// the reason arguments are streamed at all. The consequence is stated rather
// than hidden: a placeholder split across two argument fragments is delivered
// unrestored. Callers that need exact tool arguments should use the buffered
// endpoint, where llm.Collect reassembles first and RestoreResponse runs on the
// whole document.
func RestoreStream(seq iter.Seq2[llm.Chunk, error], vault *Vault) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		sr := NewStreamRestorer(vault)
		var terminator llm.Chunk
		seenTerminator := false

		for chunk, err := range seq {
			if err != nil {
				if tail := sr.Flush(); tail != "" {
					out := chunk
					out.Delta = llm.Delta{Content: tail}
					out.FinishReason, out.Usage = "", nil
					if !yield(out, nil) {
						return
					}
				}
				yield(llm.Chunk{}, err)
				return
			}

			if chunk.FinishReason != "" || chunk.Usage != nil {
				terminator = chunk
				seenTerminator = true
				if chunk.Delta.Content == "" && len(chunk.Delta.ToolCalls) == 0 {
					continue
				}
			}

			out := chunk
			out.FinishReason, out.Usage = "", nil
			out.Delta.Content = sr.Write(chunk.Delta.Content)
			if len(chunk.Delta.ToolCalls) > 0 {
				// Copied rather than rewritten in place: the slice header is
				// shared with the provider's chunk, and a provider that reuses
				// its buffer would otherwise see our edits.
				calls := make([]llm.ToolCallDelta, len(chunk.Delta.ToolCalls))
				copy(calls, chunk.Delta.ToolCalls)
				for i := range calls {
					calls[i].ArgumentsDelta = vault.Restore(calls[i].ArgumentsDelta)
				}
				out.Delta.ToolCalls = calls
			}
			if !seenTerminator && terminator.ID == "" {
				terminator.ID, terminator.Model, terminator.Provider = chunk.ID, chunk.Model, chunk.Provider
			}
			if out.Delta.Content == "" && len(out.Delta.ToolCalls) == 0 {
				continue
			}
			if !yield(out, nil) {
				return
			}
		}

		final := terminator
		final.Delta = llm.Delta{Content: sr.Flush()}
		yield(final, nil)
	}
}
