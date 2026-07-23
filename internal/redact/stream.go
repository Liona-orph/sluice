package redact

import (
	"iter"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// StreamRedactor redacts text arriving in arbitrary chunks.
//
// The problem it solves: an email address split as "ali" + "ce@example.com"
// contains no email address in either chunk. Redacting chunk by chunk therefore
// emits the first half in the clear and then redacts nothing, which is worse
// than not redacting at all because the operator believes it is working.
//
// The solution is a bounded look-behind. The redactor holds back the last
// lookAhead bytes of everything it has seen, where lookAhead is the longest
// match any configured detector can produce (Detector.MaxLen). Detection always
// runs over the whole retained buffer, so it sees the maximum available
// context, and only matches ending more than lookAhead bytes from the end of
// the buffer are emitted. A match that could still grow with the next chunk is,
// by construction, one that ends within lookAhead of the end, so it is never
// emitted early.
//
// The cost is latency, memory and CPU, all bounded and all worth stating.
// Output lags input by at most lookAhead bytes plus the length of one
// straddling match, and the buffer never exceeds roughly twice lookAhead; with
// the default detectors lookAhead is 254 bytes, set by the maximum length of an
// email address.
//
// The CPU cost is the one that would surprise someone: every Write re-runs
// every detector over the whole retained buffer, so streaming a response in
// token-sized chunks costs on the order of one full detection pass per chunk
// rather than one per response. The two benchmarks differ only in how the same
// 1,085 bytes arrive: BenchmarkRedactOneShot takes 223us in a single call and
// BenchmarkStreamRedact takes 3.00ms in 16-byte chunks, a factor of 13. That
// is the correct thing to fix first if streaming redaction ever shows up in a
// profile. The fix is incremental matching: only rescan the region a new chunk
// can have changed. It is not done here because it is a meaningful amount of
// subtle code, and 3ms spread across the seconds a streamed completion takes is
// not yet worth the risk of getting it wrong.
//
// A StreamRedactor is not safe for concurrent use; it belongs to one stream.
// The Vault it writes into is safe for concurrent use, so the same vault can
// serve the request path and the response path at once.
type StreamRedactor struct {
	r     *Redactor
	vault *Vault
	// pending is the retained tail. A plain string rather than a ring buffer:
	// the copying is bounded by lookAhead on every Write, which for a 254-byte
	// window is not worth the complexity of avoiding.
	pending string
	done    bool
}

// NewStreamRedactor starts a stream backed by vault. Passing a vault shared
// with the request path is what makes a value redacted in the prompt restorable
// in the response.
func (r *Redactor) NewStreamRedactor(vault *Vault) *StreamRedactor {
	if vault == nil {
		vault = NewVault(r.policy.HashSalt)
	}
	return &StreamRedactor{r: r, vault: vault}
}

// Vault returns the vault this stream writes into.
func (s *StreamRedactor) Vault() *Vault { return s.vault }

// Write accepts the next chunk and returns whatever is now safe to emit, which
// is frequently the empty string.
func (s *StreamRedactor) Write(chunk string) string {
	s.pending += chunk
	s.vault.reserve(s.pending)

	lookAhead := s.r.lookAhead
	if len(s.pending) <= lookAhead {
		return ""
	}

	matches := s.r.Detect(s.pending)
	cut := len(s.pending) - lookAhead

	// Back the cut up out of any match that spans it. Such a match is complete
	// as far as this buffer is concerned but may still be extended by the next
	// chunk, so it has to wait.
	for _, m := range matches {
		if m.Start < cut && m.End > cut {
			cut = m.Start
		}
	}
	cut = alignToRuneStart(s.pending, cut)
	if cut <= 0 {
		return ""
	}

	emit := s.r.apply(s.pending[:cut], matchesBefore(matches, cut), s.vault)
	s.pending = s.pending[cut:]
	return emit
}

// Flush redacts and returns whatever is left. It must be called exactly once,
// when the stream ends; the entity at the end of a response is the one the
// look-behind is still holding.
func (s *StreamRedactor) Flush() string {
	if s.done {
		return ""
	}
	s.done = true
	out := s.r.RedactWith(s.pending, s.vault)
	s.pending = ""
	return out
}

// matchesBefore returns the matches lying entirely before cut.
//
// Detection deliberately runs on the whole buffer and is filtered here rather
// than being run on the prefix. Running it on the prefix would let the end of
// the prefix act as context -- a word boundary that is not really there -- and
// invent matches the full text does not contain.
func matchesBefore(matches []Match, cut int) []Match {
	out := matches[:0:0]
	for _, m := range matches {
		if m.End <= cut {
			out = append(out, m)
		}
	}
	return out
}

// RedactStream wraps a chunk sequence, redacting the text deltas as they pass.
//
// Chunks are re-emitted with rewritten content, which means the chunk
// boundaries a client observes are not the ones the provider produced: a chunk
// may come out empty while the look-behind fills, and the final flush attaches
// its text to the terminating chunk. That is unavoidable -- the alternative is
// emitting unredacted text -- and it is why the finish-reason chunk is held
// until the flush has somewhere to go.
//
// The sequence is lazy and runs entirely on the consumer's goroutine, so
// abandoning it leaks nothing.
func (r *Redactor) RedactStream(seq iter.Seq2[llm.Chunk, error], vault *Vault) iter.Seq2[llm.Chunk, error] {
	return func(yield func(llm.Chunk, error) bool) {
		sr := r.NewStreamRedactor(vault)
		// terminator is the chunk carrying the finish reason and usage. It is
		// held back so that the flushed tail can be attached to it rather than
		// arriving after a client has already seen the stream end.
		var terminator llm.Chunk

		for chunk, err := range seq {
			if err != nil {
				// Whatever the look-behind is still holding was already going
				// to be delivered; emitting it before the error is the
				// difference between a truncated answer and a lost one.
				if tail := sr.Flush(); tail != "" {
					out := terminator
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
				if chunk.Delta.Content == "" && len(chunk.Delta.ToolCalls) == 0 {
					continue
				}
			}

			out := chunk
			out.FinishReason, out.Usage = "", nil
			out.Delta.Content = sr.Write(chunk.Delta.Content)
			if terminator.ID == "" {
				terminator.ID, terminator.Model, terminator.Provider = chunk.ID, chunk.Model, chunk.Provider
			}
			if out.Delta.Content == "" && len(out.Delta.ToolCalls) == 0 {
				// Nothing safe to say yet. Suppressing the chunk rather than
				// sending an empty one keeps the stream free of frames a client
				// would have to learn to ignore.
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
