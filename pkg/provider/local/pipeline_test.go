package local_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/internal/cache"
	"github.com/Liona-orph/sluice/internal/leaktest"
	"github.com/Liona-orph/sluice/internal/redact"
	"github.com/Liona-orph/sluice/pkg/llm"
	"github.com/Liona-orph/sluice/pkg/provider/local"
)

// These tests assemble the four core pieces the way a gateway would -- redact,
// look up, call a provider, store, restore -- and assert the properties that
// only appear when they are composed. Each piece is tested on its own
// elsewhere; what is checked here is that the seams hold.
//
// They run offline, which is the point of the local provider existing at all.

func pipeline(t testing.TB) (*redact.Redactor, *cache.Cache, *local.Provider) {
	t.Helper()
	c, err := cache.New(cache.Options{
		MaxEntries: 100,
		Embedder:   cache.NewHashingEmbedder(256),
	})
	require.NoError(t, err)
	return redact.New(redact.DefaultPolicy()), c, mustNew(t, local.Config{})
}

func TestPipelineRedactCallRestore(t *testing.T) {
	r, c, p := pipeline(t)
	ctx := context.Background()

	req := llm.Request{
		Model: "local-small",
		Messages: []llm.Message{{Role: llm.RoleUser,
			Content: "Draft a reminder to alice.hansen@example.com about invoice 4021."}},
	}

	clean, vault := r.RedactRequest(req)
	require.NotContains(t, clean.Messages[0].Content, "alice.hansen@example.com")

	res, err := c.Get(ctx, clean)
	require.NoError(t, err)
	require.False(t, res.Hit)

	resp, err := p.Complete(ctx, clean)
	require.NoError(t, err)
	require.NoError(t, c.Put(ctx, clean, resp))

	// The provider echoes the prompt, so the placeholder is in the raw
	// response; the caller must never see it.
	require.Contains(t, resp.Message.Content, "SLUICE_EMAIL")
	restored := r.RestoreResponse(resp, vault)
	assert.Contains(t, restored.Message.Content, "alice.hansen@example.com")
	assert.NotContains(t, restored.Message.Content, "SLUICE_EMAIL")

	// A second identical request is served from cache and restores identically.
	clean2, vault2 := r.RedactRequest(req)
	assert.Equal(t, clean.Fingerprint(), clean2.Fingerprint(),
		"redaction must be deterministic, or every request is a cache miss")

	res, err = c.Get(ctx, clean2)
	require.NoError(t, err)
	require.True(t, res.Hit)
	assert.Equal(t, cache.HitExact, res.Kind)
	assert.Equal(t, restored.Message.Content, r.RestoreResponse(res.Response, vault2).Message.Content)
}

// Cost accounting over the whole pipeline: what was billed, and what a cache
// hit saved.
func TestPipelineCostAccounting(t *testing.T) {
	_, c, p := pipeline(t)
	ctx := context.Background()
	pricing := llm.DefaultPricing().With("local-small", llm.ModelPrice{
		InputPerMillion: 3 * llm.Dollar, OutputPerMillion: 15 * llm.Dollar,
	})

	req := ask("summarise the quarterly report")

	resp, err := p.Complete(ctx, req)
	require.NoError(t, err)
	require.NoError(t, c.Put(ctx, req, resp))

	spent, err := pricing.Cost(resp.Model, resp.Usage)
	require.NoError(t, err)
	assert.Positive(t, int64(spent))

	res, err := c.Get(ctx, req)
	require.NoError(t, err)
	require.True(t, res.Hit)

	saved, err := pricing.Cost(res.Response.Model, res.Response.Usage)
	require.NoError(t, err)
	assert.Equal(t, spent, saved, "a cache hit avoids exactly what the miss cost")
	t.Logf("one call cost %s; the cache hit avoided the same again", spent)
}

// Failover: the first provider is down, the second answers, and the error
// taxonomy is what decides to move on.
func TestPipelineFailover(t *testing.T) {
	ctx := context.Background()
	down := mustNew(t, local.Config{
		Name: "primary", Seed: 1,
		Failure: local.Failure{Code: llm.CodeProviderUnavailable, Rate: 1},
	})
	up := mustNew(t, local.Config{Name: "secondary", Seed: 2})

	req := ask("which upstream answered")
	var lastErr error
	var resp llm.Response
	for _, p := range []llm.Provider{down, up} {
		r, err := p.Complete(ctx, req)
		if err == nil {
			resp = r
			break
		}
		lastErr = err
		if !llm.ShouldFailover(err) {
			break
		}
	}
	require.Error(t, lastErr)
	assert.True(t, llm.ShouldFailover(lastErr))
	assert.Equal(t, "secondary", resp.Provider)
}

// A content filter must not fail over: trying the next provider until one
// accepts the prompt is a policy decision, not a default.
func TestPipelineDoesNotFailoverOnContentFilter(t *testing.T) {
	p := mustNew(t, local.Config{
		Failure: local.Failure{Code: llm.CodeContentFiltered, Rate: 1},
	})
	_, err := p.Complete(context.Background(), ask("anything"))
	require.Error(t, err)
	assert.False(t, llm.ShouldFailover(err))
	assert.False(t, llm.IsRetryable(err))
}

// The streaming seam: provider chunks pass through the stream redactor and the
// reassembled response matches what the non-streaming path would have produced.
func TestPipelineStreamingRedaction(t *testing.T) {
	defer leaktest.Check(t)()
	ctx := context.Background()
	r := redact.New(redact.DefaultPolicy())
	p := mustNew(t, local.Config{MaxChunkWords: 2})

	req := llm.Request{
		Model: "local-small",
		Messages: []llm.Message{{Role: llm.RoleUser,
			Content: "Reply to bob@example.org and copy carol@example.org."}},
	}
	clean, vault := r.RedactRequest(req)

	seq, err := p.Stream(ctx, clean)
	require.NoError(t, err)

	collected, err := llm.Collect(r.RedactStream(seq, vault))
	require.NoError(t, err)

	assert.NotContains(t, collected.Message.Content, "@example.org",
		"nothing that reaches the client may carry an address")
	restored := vault.Restore(collected.Message.Content)
	assert.Contains(t, restored, "bob@example.org")
	assert.Contains(t, restored, "carol@example.org")
	assert.Equal(t, llm.FinishStop, collected.FinishReason)
}

// A semantic hit across a redacted prompt: the placeholders are part of the
// text the embedder sees, so two prompts differing only in the value redacted
// must not collide.
func TestPipelineSemanticCacheRespectsPlaceholders(t *testing.T) {
	r, c, p := pipeline(t)
	ctx := context.Background()

	first := llm.Request{Model: "local-small", Messages: []llm.Message{{
		Role: llm.RoleUser, Content: "Send the invoice to alice@example.com right away."}}}
	second := llm.Request{Model: "local-small", Messages: []llm.Message{{
		Role: llm.RoleUser, Content: "Send the invoice to bob@example.com right away."}}}

	cleanFirst, _ := r.RedactRequest(first)
	resp, err := p.Complete(ctx, cleanFirst)
	require.NoError(t, err)
	require.NoError(t, c.Put(ctx, cleanFirst, resp))

	cleanSecond, vault := r.RedactRequest(second)
	// Both redact to the same placeholder, so this is deliberately the worst
	// case: the cache cannot distinguish them, and it does not need to, because
	// the vault restores the right address into whichever answer comes back.
	res, err := c.Get(ctx, cleanSecond)
	require.NoError(t, err)
	require.True(t, res.Hit, "identical redacted prompts are the same question")

	restored := r.RestoreResponse(res.Response, vault)
	assert.Contains(t, restored.Message.Content, "bob@example.com")
	assert.NotContains(t, restored.Message.Content, "alice@example.com",
		"the second caller must not learn the first caller's address")
}

func TestPipelineTokenAccountingMatchesProvider(t *testing.T) {
	ctx := context.Background()
	p := mustNew(t, local.Config{})
	req := ask("how many tokens is this")

	resp, err := p.Complete(ctx, req)
	require.NoError(t, err)

	estimated := llm.EstimateUsage(llm.DefaultTokenizer(), req, resp)
	assert.Equal(t, resp.Usage.OutputTokens, estimated.OutputTokens,
		"the gateway's estimate and the provider's count come from the same tokenizer")
	assert.True(t, estimated.Estimated)
	assert.False(t, resp.Usage.Estimated)
}

func BenchmarkPipeline(b *testing.B) {
	c, _ := cache.New(cache.Options{Embedder: cache.NewHashingEmbedder(256)})
	r := redact.New(redact.DefaultPolicy())
	p := mustNew(b, local.Config{})
	ctx := context.Background()

	prompts := make([]llm.Request, 64)
	for i := range prompts {
		prompts[i] = llm.Request{Model: "local-small", Messages: []llm.Message{{
			Role:    llm.RoleUser,
			Content: "Question " + strings.Repeat("x", i%7) + " for alice@example.com about billing.",
		}}}
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req := prompts[i%len(prompts)]
		clean, vault := r.RedactRequest(req)
		res, err := c.Get(ctx, clean)
		if err != nil {
			b.Fatal(err)
		}
		if !res.Hit {
			resp, err := p.Complete(ctx, clean)
			if err != nil {
				b.Fatal(err)
			}
			if err := c.Put(ctx, clean, resp); err != nil {
				b.Fatal(err)
			}
			res.Response = resp
		}
		_ = r.RestoreResponse(res.Response, vault)
	}
}
