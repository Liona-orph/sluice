package llm

import (
	"strings"
	"sync"
)

// Tokenizer estimates how many tokens a piece of text occupies.
//
// It is an interface so that a deployment that can afford a real vocabulary --
// tiktoken, a SentencePiece model, whatever the upstream actually uses -- can
// drop it in and get exact counts. Approx is the default because Sluice must
// build and test with no model files and no network.
type Tokenizer interface {
	CountTokens(text string) int
}

// Chat framing overhead, from OpenAI's documented chat-completion accounting:
// each message costs a few tokens of role and delimiter framing, and the reply
// is primed with a few more. The numbers are small but they are not noise -- a
// conversation of forty short messages carries over a hundred tokens of framing
// -- and they are the same order of magnitude across providers.
const (
	tokensPerMessage = 3
	tokensPerName    = 1
	tokensReplyPrime = 3
)

// Approx is a vocabulary-free approximate tokenizer.
//
// It is a WordPiece-style greedy longest-match segmenter over an embedded list
// of common English words and fragments, wrapped in a pre-tokenizer that
// imitates the one cl100k_base uses: a word absorbs a single leading space,
// digit runs split into groups of at most three, whitespace and punctuation
// runs are their own pieces. The digit rule is not an approximation -- it is
// exactly what cl100k does -- which is why numeric text, the case where a
// naive characters-over-four estimate is worst, comes out right.
//
// Measured against testdata/token_corpus.json, whose reference counts are
// cl100k_base: mean absolute percentage error 0.68%, and no fixture off by more
// than one token. Read that number with its corpus in mind -- the corpus holds
// only constructions whose cl100k segmentation can be derived by hand, so it is
// English prose, punctuation and numbers, and it is not source code. A second
// check uses a bound from outside this repository -- OpenAI's documented 100
// tokens per 75 words -- and measures 1.21 tokens per word on a prose sample
// against that 1.33, i.e. a roughly 9% underestimate on formal English. The
// underestimate is the price of the short-word rule below, and it is the
// direction that matters: an under-counted prompt is a cost report that reads
// low, so treat these figures as a floor on spend, not a guarantee.
//
// Two known biases follow from the design and are not bugs to be fixed cheaply:
//
//   - Case is folded before lookup, so SHOUTED TEXT is underestimated; real BPE
//     vocabularies carry lowercase and capitalised forms but rarely upper.
//   - Non-ASCII runs are estimated at 2.5 bytes per token, calibrated for
//     Latin-script and CJK text but not measured. Cost accounting for a
//     predominantly non-Latin workload should use a real tokenizer.
//
// The zero value is ready to use.
type Approx struct{}

// DefaultTokenizer returns the tokenizer the gateway uses when none is
// configured.
func DefaultTokenizer() Tokenizer { return Approx{} }

// CountTokens estimates the token count of text.
func (a Approx) CountTokens(text string) int {
	total := 0
	for i := 0; i < len(text); {
		c := text[i]

		// A single leading space belongs to the following word or number; it is
		// why " the" and "the" are both one token.
		if c == ' ' && i+1 < len(text) && (isWordByte(text[i+1]) || isDigit(text[i+1])) {
			i++
			c = text[i]
		}

		switch {
		case isDigit(c):
			j := i
			for j < len(text) && isDigit(text[j]) {
				j++
			}
			total += ceilDiv(j-i, 3)
			i = j

		case isWordByte(c):
			j := i
			for j < len(text) && isWordByte(text[j]) {
				j++
			}
			total += countWord(text[i:j])
			i = j

		case c >= 0x80:
			j := i
			for j < len(text) && text[j] >= 0x80 {
				j++
			}
			// 2.5 bytes per token: a 3-byte CJK character is a little over one
			// token, a 2-byte accented Latin character a little under.
			total += ceilDiv(2*(j-i), 5)
			i = j

		case c == '\'' && i+1 < len(text) && isWordByte(text[i+1]):
			// Contractions: "'s", "'t", "'re", "'ve", "'ll" are single tokens,
			// and the encodings all special-case them ahead of the word rule.
			j := i + 1
			for j < len(text) && j-i <= 3 && isWordByte(text[j]) {
				j++
			}
			total++
			i = j

		case isSpace(c):
			j := i
			for j < len(text) && isSpace(text[j]) {
				j++
			}
			// Whitespace runs collapse hard: vocabularies carry dedicated
			// tokens for runs of indentation up to about sixteen characters.
			total += ceilDiv(j-i, 16)
			i = j

		default:
			// A run of the same punctuation character ("...", "---", "###")
			// merges; a run of different ones does not.
			j := i
			for j < len(text) && text[j] == c {
				j++
			}
			total += ceilDiv(j-i, 3)
			i = j
		}
	}
	return total
}

// CountMessages estimates the tokens a conversation occupies once framed for a
// chat endpoint, including the priming of the reply.
func (a Approx) CountMessages(msgs []Message) int {
	total := tokensReplyPrime
	for _, m := range msgs {
		total += tokensPerMessage
		total += a.CountTokens(string(m.Role))
		total += a.CountTokens(m.Content)
		if m.Name != "" {
			total += tokensPerName + a.CountTokens(m.Name)
		}
		if m.ToolCallID != "" {
			total += a.CountTokens(m.ToolCallID)
		}
		for _, tc := range m.ToolCalls {
			total += a.CountTokens(tc.Name) + a.CountTokens(string(tc.Arguments))
		}
	}
	return total
}

// CountRequest estimates the input tokens a request will be billed for,
// including tool schemas.
//
// Tool schemas are counted as their raw JSON. Providers reformat them into a
// prompt preamble of their own, so this is an estimate of an estimate; it is
// still far closer than ignoring them, and a request with a dozen tools carries
// more schema than conversation.
func (a Approx) CountRequest(r Request) int {
	total := a.CountMessages(r.Messages)
	for _, t := range r.Tools {
		total += a.CountTokens(t.Name) + a.CountTokens(t.Description) + a.CountTokens(string(t.Parameters))
	}
	return total
}

// EstimateUsage fills in a Usage for a provider that does not report one.
//
// It marks the result estimated so that cost reports can distinguish measured
// from inferred spend rather than blending them.
func EstimateUsage(tok Tokenizer, req Request, resp Response) Usage {
	in := 0
	if a, ok := tok.(Approx); ok {
		in = a.CountRequest(req)
	} else {
		for _, m := range req.Messages {
			in += tokensPerMessage + tok.CountTokens(m.Content)
		}
		in += tokensReplyPrime
	}
	out := tok.CountTokens(resp.Message.Content)
	for _, tc := range resp.Message.ToolCalls {
		out += tok.CountTokens(tc.Name) + tok.CountTokens(string(tc.Arguments))
	}
	return Usage{InputTokens: in, OutputTokens: out, Estimated: true}
}

// countWord segments a run of letters.
func countWord(word string) int {
	lower := strings.ToLower(word)
	v := vocabulary()

	// The overwhelmingly common case: the whole word is a vocabulary entry, and
	// a BPE vocabulary would carry it as a single token.
	if v.has(lower) {
		return 1
	}
	// Regular inflections of a known stem are also single tokens in practice:
	// " jumping" and " jumped" are one token each wherever " jump" is.
	if stem, ok := stripInflection(lower); ok && (v.has(stem) || v.has(stem+"e")) {
		return 1
	}
	// A 100k-entry vocabulary covers essentially every short English word, so a
	// letter run this short is a single token whether or not the list happens
	// to carry it. The rule costs accuracy on short random letter strings,
	// which are rare in prose and, where they do occur (identifiers, keys),
	// almost always contain digits and are therefore split by the
	// pre-tokenizer before they reach here.
	if len(lower) <= shortWordTokens {
		return 1
	}

	tokens := 0
	for pos := 0; pos < len(lower); {
		n := v.longestPrefix(lower[pos:])
		// Matches shorter than three characters are rejected: allowing them
		// lets a one-letter entry like "a" chop an unknown word into fragments
		// and inflate the count well past what BPE would do.
		if n < 3 {
			n = 4
			if pos+n > len(lower) {
				n = len(lower) - pos
			}
		}
		pos += n
		tokens++
	}
	if tokens == 0 {
		return 1
	}
	return tokens
}

// shortWordTokens is the letter-run length below which a word is assumed to be
// a single token.
const shortWordTokens = 6

// inflections are ordered longest-first so that "ies" is tried before "es".
var inflections = []string{"ing", "ies", "ed", "es", "ly", "s"}

func stripInflection(word string) (string, bool) {
	for _, suf := range inflections {
		if len(word) > len(suf)+2 && strings.HasSuffix(word, suf) {
			stem := word[:len(word)-len(suf)]
			if suf == "ies" {
				stem += "y"
			}
			return stem, true
		}
	}
	return word, false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// vocab is the compiled word list. It is built once and never mutated, so the
// OnceValue is shared state without being mutable state.
type vocab struct {
	words  map[string]struct{}
	maxLen int
}

func (v *vocab) has(s string) bool {
	_, ok := v.words[s]
	return ok
}

// longestPrefix returns the length of the longest vocabulary entry that starts
// s, or 0. It walks down from maxLen because the map has no prefix structure; a
// trie would be faster but the words are short and this is not the hot path
// that a real tokenizer would be.
func (v *vocab) longestPrefix(s string) int {
	n := v.maxLen
	if n > len(s) {
		n = len(s)
	}
	for ; n >= 1; n-- {
		if v.has(s[:n]) {
			return n
		}
	}
	return 0
}

var vocabulary = sync.OnceValue(func() *vocab {
	v := &vocab{words: make(map[string]struct{}, 1024)}
	for _, w := range strings.Fields(commonWords + " " + commonProper + " " + commonFragments) {
		if w == "" {
			continue
		}
		v.words[w] = struct{}{}
		if len(w) > v.maxLen {
			v.maxLen = len(w)
		}
	}
	return v
})
