package cache

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/pkg/llm"
)

type promptPair struct {
	A          string `json:"a"`
	B          string `json:"b"`
	Equivalent bool   `json:"equivalent"`
	Note       string `json:"note"`
}

func loadPairs(t testing.TB) []promptPair {
	t.Helper()
	raw, err := os.ReadFile("testdata/semantic_pairs.json")
	require.NoError(t, err)
	var doc struct {
		Pairs []promptPair `json:"pairs"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Pairs)
	return doc.Pairs
}

// maxFalseHitRate is the assertion the semantic cache's safety rests on.
//
// Zero, not "low". A false hit is a wrong answer returned with no error and no
// signal, and there is no threshold at which some of those are acceptable in a
// default configuration. If a change to the embedder or the threshold admits
// one, that change needs to be argued for, not merged.
const maxFalseHitRate = 0.0

// TestSemanticFalseHitRate is the measurement quoted in the package
// documentation and in DefaultSimilarityThreshold's comment.
func TestSemanticFalseHitRate(t *testing.T) {
	pairs := loadPairs(t)
	emb := NewHashingEmbedder(256)
	ctx := context.Background()

	var (
		falseHits, dangerous int
		trueHits, equivalent int
		worstFalse           promptPair
		worstFalseSim        float64
		worstMissed          promptPair
		worstMissedSim       = 1.0
	)
	for _, p := range pairs {
		va, err := emb.Embed(ctx, p.A)
		require.NoError(t, err)
		vb, err := emb.Embed(ctx, p.B)
		require.NoError(t, err)
		sim := CosineSimilarity(va, vb)

		if p.Equivalent {
			equivalent++
			if sim >= DefaultSimilarityThreshold {
				trueHits++
			} else if sim < worstMissedSim {
				worstMissed, worstMissedSim = p, sim
			}
			continue
		}
		dangerous++
		if sim >= DefaultSimilarityThreshold {
			falseHits++
			t.Errorf("false hit at %.4f: %q vs %q (%s)", sim, p.A, p.B, p.Note)
		}
		if sim > worstFalseSim {
			worstFalse, worstFalseSim = p, sim
		}
	}

	falseRate := float64(falseHits) / float64(dangerous)
	trueRate := float64(trueHits) / float64(equivalent)
	t.Logf("threshold %.2f over %d pairs: false-hit rate %.4f (%d/%d), true-hit rate %.4f (%d/%d)",
		DefaultSimilarityThreshold, len(pairs), falseRate, falseHits, dangerous, trueRate, trueHits, equivalent)
	t.Logf("closest non-equivalent pair scored %.4f: %q vs %q (%s)",
		worstFalseSim, worstFalse.A, worstFalse.B, worstFalse.Note)
	if worstMissedSim < 1 {
		t.Logf("furthest equivalent pair scored %.4f: %q vs %q (%s)",
			worstMissedSim, worstMissed.A, worstMissed.B, worstMissed.Note)
	}

	assert.LessOrEqual(t, falseRate, maxFalseHitRate)
	// A cache that never hits is safe and useless, so the useful half is
	// asserted too.
	assert.GreaterOrEqual(t, trueRate, 0.8)
	// The margin between the two populations is what makes the threshold a
	// choice rather than a coincidence.
	assert.Less(t, worstFalseSim, DefaultSimilarityThreshold)
}

// The threshold is a trade-off, so its shape is worth showing: how the two
// rates move as it is loosened.
func TestSemanticThresholdSweep(t *testing.T) {
	pairs := loadPairs(t)
	emb := NewHashingEmbedder(256)
	ctx := context.Background()

	type scored struct {
		sim        float64
		equivalent bool
	}
	var sims []scored
	for _, p := range pairs {
		va, _ := emb.Embed(ctx, p.A)
		vb, _ := emb.Embed(ctx, p.B)
		sims = append(sims, scored{CosineSimilarity(va, vb), p.Equivalent})
	}
	sort.Slice(sims, func(i, j int) bool { return sims[i].sim > sims[j].sim })

	for _, threshold := range []float64{0.80, 0.85, 0.90, 0.95, 0.97, 0.99} {
		var falseHits, dangerous, trueHits, equivalent int
		for _, s := range sims {
			if s.equivalent {
				equivalent++
				if s.sim >= threshold {
					trueHits++
				}
			} else {
				dangerous++
				if s.sim >= threshold {
					falseHits++
				}
			}
		}
		t.Logf("threshold %.2f: false-hit %.3f (%d/%d), true-hit %.3f (%d/%d)",
			threshold, float64(falseHits)/float64(dangerous), falseHits, dangerous,
			float64(trueHits)/float64(equivalent), trueHits, equivalent)
	}
}

// End to end: a paraphrase must be served from the entry stored for the
// original, and a different question must not be.
func TestSemanticHitAndMiss(t *testing.T) {
	c, err := New(Options{Embedder: NewHashingEmbedder(256)})
	require.NoError(t, err)
	ctx := context.Background()

	stored := ask("What is the capital of France?")
	require.NoError(t, c.Put(ctx, stored, respond("Paris.")))

	res, err := c.Get(ctx, ask("what is the capital of france"))
	require.NoError(t, err)
	require.True(t, res.Hit)
	assert.Equal(t, HitSemantic, res.Kind)
	assert.Equal(t, "Paris.", res.Response.Message.Content)
	assert.GreaterOrEqual(t, res.Similarity, DefaultSimilarityThreshold)

	res, err = c.Get(ctx, ask("What is the capital of Germany?"))
	require.NoError(t, err)
	assert.False(t, res.Hit, "a different country is a different question")

	assert.Equal(t, uint64(1), c.Stats().SemanticHits)
	assert.Positive(t, c.Stats().NearMisses, "the near miss must be visible to an operator")
}

// The guard that keeps approximation confined to the prompt.
func TestSemanticHitRequiresIdenticalParameters(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*llm.Request)
	}{
		{"max tokens", func(r *llm.Request) { r.MaxTokens = 4000 }},
		{"temperature", func(r *llm.Request) { r.Temperature = llm.Ptr(1.5) }},
		{"tools", func(r *llm.Request) { r.Tools = []llm.Tool{{Name: "search"}} }},
		{"stop sequences", func(r *llm.Request) { r.Stop = []string{"\n"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Options{Embedder: NewHashingEmbedder(256)})
			require.NoError(t, err)

			stored := ask("What is the capital of France?")
			stored.MaxTokens = 100
			require.NoError(t, c.Put(ctx, stored, respond("Paris.")))

			probe := ask("what is the capital of france")
			probe.MaxTokens = 100
			tc.mutate(&probe)

			res, err := c.Get(ctx, probe)
			require.NoError(t, err)
			assert.False(t, res.Hit, "differing parameters must not be approximated away")
		})
	}
}

func TestSemanticIsNamespacedByModel(t *testing.T) {
	c, err := New(Options{Embedder: NewHashingEmbedder(256)})
	require.NoError(t, err)
	ctx := context.Background()

	stored := ask("What is the capital of France?")
	require.NoError(t, c.Put(ctx, stored, respond("Paris.")))

	other := ask("what is the capital of france")
	other.Model = "some-other-model"
	res, err := c.Get(ctx, other)
	require.NoError(t, err)
	assert.False(t, res.Hit, "the same prompt to another model is another question")
}

func TestSemanticDisabledWithoutEmbedder(t *testing.T) {
	c, err := New(Options{})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, c.Put(ctx, ask("What is the capital of France?"), respond("Paris.")))

	res, err := c.Get(ctx, ask("what is the capital of france"))
	require.NoError(t, err)
	assert.False(t, res.Hit, "the unsafe lookup must be opt-in")
}

func TestHashingEmbedderProperties(t *testing.T) {
	emb := NewHashingEmbedder(128)
	ctx := context.Background()

	a, err := emb.Embed(ctx, "reset the password")
	require.NoError(t, err)
	assert.Len(t, a, 128)

	again, err := emb.Embed(ctx, "reset the password")
	require.NoError(t, err)
	assert.Equal(t, a, again, "an embedder that is not deterministic makes entries unfindable")

	assert.InDelta(t, 1.0, CosineSimilarity(a, a), 1e-6)

	// Word order matters, which is why character n-grams are in the feature set
	// at all.
	reordered, _ := emb.Embed(ctx, "password the reset")
	assert.Less(t, CosineSimilarity(a, reordered), 1.0)

	empty, err := emb.Embed(ctx, "")
	require.NoError(t, err)
	assert.Len(t, empty, 128)
	assert.InDelta(t, 0.0, CosineSimilarity(a, empty), 0, "a zero vector has no direction to compare")
}

func TestCosineSimilarityEdgeCases(t *testing.T) {
	assert.InDelta(t, 0.0, CosineSimilarity(nil, nil), 0)
	assert.InDelta(t, 0.0, CosineSimilarity([]float32{1, 0}, []float32{1, 0, 0}), 0, "different lengths are not comparable")
	assert.InDelta(t, 1.0, CosineSimilarity([]float32{3, 4}, []float32{6, 8}), 1e-6, "magnitude must not matter")
	assert.InDelta(t, -1.0, CosineSimilarity([]float32{1, 0}, []float32{-1, 0}), 1e-6)
	assert.InDelta(t, 0.0, CosineSimilarity([]float32{1, 0}, []float32{0, 1}), 1e-6)
}

func BenchmarkSemanticLookup(b *testing.B) {
	c, _ := New(Options{Embedder: NewHashingEmbedder(256), MaxEntries: 5000})
	ctx := context.Background()
	for i := 0; i < 2000; i++ {
		req := ask("question number " + itoa(i) + " about something specific")
		_ = c.Put(ctx, req, respond("answer"))
	}
	probe := ask("a question that will not match anything stored here at all")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.Get(ctx, probe); err != nil {
			b.Fatal(err)
		}
	}
}
