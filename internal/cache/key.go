package cache

import (
	"strings"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// Key identifies a cache entry.
//
// It is a pair rather than a single string so that namespace operations --
// invalidate everything for one model, scan one model's entries for a semantic
// match -- are possible without parsing.
type Key struct {
	// Namespace isolates entries that must never be confused with each other.
	// It is the model name: the same prompt to two models is two different
	// questions, and serving one from the other's answer would be a bug that
	// looks like a routing failure.
	Namespace string
	// Hash is llm.Request.Fingerprint, which covers everything about the
	// request that can change the output. See that method for what is excluded
	// and why.
	Hash string
}

func (k Key) String() string { return k.Namespace + "/" + k.Hash }

// KeyFor builds the exact-match key for a request.
func KeyFor(req llm.Request) Key {
	return Key{Namespace: req.Model, Hash: req.Fingerprint()}
}

// paramsHash fingerprints everything about a request except its messages.
//
// A semantic hit is only allowed between requests with identical parameters.
// This is not fussiness: a cached answer generated with MaxTokens 50 is not an
// acceptable answer to a request for 2000, an answer generated without tools
// cannot satisfy a request that declares them, and a request with a different
// system prompt is a different question however similar the user turn looks.
// Similarity is allowed to be approximate about what was asked; it is not
// allowed to be approximate about how it was asked for.
func paramsHash(req llm.Request) string {
	stripped := req
	stripped.Messages = nil
	return stripped.Fingerprint()
}

// embedText renders the messages into the string the embedder sees.
//
// Roles are included because "you are a pirate" as a system prompt and as a
// user turn are different requests, and a bare concatenation would make them
// identical.
func embedText(req llm.Request) string {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(string(m.Role))
		b.WriteByte(':')
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}
