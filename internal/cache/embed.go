package cache

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Embedder turns text into a vector for similarity search.
//
// The interface exists so that a deployment with a real embedding model can use
// it, and so that the semantic cache can be tested without one. An
// implementation must be deterministic: two calls with the same text must
// return the same vector, or a cache entry stops being findable the moment it
// is written.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Dimensions is the length of every vector this embedder produces.
	Dimensions() int
	// ID names the embedder and its configuration. It is part of the cache
	// namespace: vectors from two different embedders are not comparable, and
	// mixing them silently produces similarity scores that mean nothing.
	ID() string
}

// HashingEmbedder is a local, deterministic embedder built from hashed word
// unigrams, word bigrams and character n-grams.
//
// What it is: the hashing trick. Each feature -- a word, or a character n-gram
// -- is hashed to a dimension and to a sign, and its weight is added there.
// Collisions are tolerated rather than avoided; with a few hundred dimensions
// and a signed hash they cancel rather than accumulate. The vector is L2
// normalised so that cosine similarity is a dot product and length does not
// dominate.
//
// What it is not: a semantic model. It measures surface similarity, so
// "cancel my subscription" and "how do I unsubscribe" score low despite meaning
// the same thing, while "delete the user" and "delete the users" score very
// high. That asymmetry is the right one for a cache: the failure it produces is
// a missed hit, which costs money, rather than a wrong hit, which costs
// correctness. A deployment that wants the missed hits back should plug in a
// real embedder, which is the whole reason Embedder is an interface.
//
// Word order is represented explicitly, by word bigrams and by character
// n-grams taken across the whole normalised string rather than within each
// word. That is not a refinement, it is a correctness requirement: a pure
// bag-of-words vector scores "Convert 10 USD to EUR" and "Convert 10 EUR to
// USD" at exactly 1.0, and a cache that treats those as the same question
// returns a confidently reversed answer. The fixture pairs in
// testdata/semantic_pairs.json exist to keep that from regressing.
type HashingEmbedder struct {
	dims    int
	minGram int
	maxGram int
}

// NewHashingEmbedder returns an embedder with the given dimensionality.
//
// 256 dimensions is the default: enough that collisions between the few hundred
// features of a typical prompt are rare, small enough that a linear scan over
// ten thousand entries is a few milliseconds.
func NewHashingEmbedder(dims int) *HashingEmbedder {
	if dims <= 0 {
		dims = 256
	}
	return &HashingEmbedder{dims: dims, minGram: 3, maxGram: 4}
}

// Dimensions implements Embedder.
func (e *HashingEmbedder) Dimensions() int { return e.dims }

// ID implements Embedder.
func (e *HashingEmbedder) ID() string {
	return "hashing-v2-" + itoa(e.dims) + "-" + itoa(e.minGram) + itoa(e.maxGram)
}

// Embed implements Embedder. It never returns an error; the signature carries
// one for implementations that call out to a model.
func (e *HashingEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, e.dims)
	normalised := normaliseText(text)
	words := strings.Fields(normalised)

	for i, word := range words {
		// Words and adjacent word pairs carry more signal than character
		// n-grams, so they are weighted higher. The weights are not tuned
		// beyond "words matter more than fragments"; the false-hit measurement
		// in semantic_test.go is what constrains them.
		addFeature(vec, "w:"+word, 2)
		if i > 0 {
			addFeature(vec, "b:"+words[i-1]+" "+word, 2)
		}
	}

	// Character n-grams over the whole string, including the spaces between
	// words, so that they carry order rather than repeating what the unigrams
	// already said.
	padded := " " + strings.Join(words, " ") + " "
	for n := e.minGram; n <= e.maxGram; n++ {
		for i := 0; i+n <= len(padded); i++ {
			addFeature(vec, "g:"+padded[i:i+n], 1)
		}
	}

	normalise(vec)
	return vec, nil
}

// normaliseText lowercases and reduces punctuation to spaces, so that
// "Reset password!" and "reset password" are the same query.
func normaliseText(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// addFeature hashes a feature to a dimension and a sign.
//
// The sign is what makes collisions tolerable: two features landing in the same
// dimension are as likely to cancel as to reinforce, so the expected error from
// a collision is zero rather than a systematic inflation.
func addFeature(vec []float32, feature string, weight float32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feature))
	sum := h.Sum64()
	// The modulus bounds the value below len(vec), so the conversion cannot
	// overflow whatever the hash produced.
	idx := int(sum % uint64(len(vec))) //nolint:gosec // bounded by the modulus
	if sum&(1<<63) != 0 {
		weight = -weight
	}
	vec[idx] += weight
}

func normalise(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range vec {
		vec[i] *= inv
	}
}

// CosineSimilarity returns the cosine of the angle between two vectors.
//
// Both are expected to be normalised, in which case this is a dot product; the
// magnitudes are divided out anyway so that a caller supplying an unnormalised
// vector gets a correct answer rather than a nonsensical one above 1.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
