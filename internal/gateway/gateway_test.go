package gateway

import (
	"context"
	"io"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/internal/audit"
	"github.com/sluice-gw/sluice/internal/cache"
	"github.com/sluice-gw/sluice/internal/config"
	"github.com/sluice-gw/sluice/internal/leaktest"
	"github.com/sluice-gw/sluice/internal/redact"
	"github.com/sluice-gw/sluice/internal/telemetry"
	"github.com/sluice-gw/sluice/pkg/llm"
	"github.com/sluice-gw/sluice/pkg/provider/local"
)

const demoSecret = "sk-sluice-local-demo"

type harness struct {
	*Gateway
	audit *audit.Memory
	clock *fakeClock
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Every read advances by a microsecond so that latency is non-zero and
	// monotonic without the test having to sleep.
	c.t = c.t.Add(time.Microsecond)
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newHarness(t *testing.T, mutate ...func(*config.Config)) *harness {
	t.Helper()
	cfg := config.Default()
	for _, m := range mutate {
		m(&cfg)
	}
	mem := audit.NewMemory(100)
	clk := &fakeClock{t: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	metrics, err := telemetry.NewMetrics(prometheus.NewPedanticRegistry())
	require.NoError(t, err)

	g, err := New(Options{
		Config:  cfg,
		Metrics: metrics,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auditor: mem,
		Now:     clk.now,
	})
	require.NoError(t, err)
	return &harness{Gateway: g, audit: mem, clock: clk}
}

func ask(secret, model, text string) Request {
	return Request{
		Secret: secret,
		LLM: llm.Request{
			Model:    model,
			Messages: []llm.Message{{Role: llm.RoleUser, Content: text}},
		},
	}
}

func TestCompleteHappyPath(t *testing.T) {
	h := newHarness(t)
	res, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hello there"))
	require.NoError(t, err)

	assert.NotEmpty(t, res.Response.Message.Content)
	assert.Equal(t, "local-primary", res.Response.Provider)
	assert.Equal(t, "demo-key", res.Principal.KeyID)
	assert.Positive(t, res.Response.Usage.InputTokens)
	assert.NotEmpty(t, res.AuditID)

	records := h.audit.Records()
	require.Len(t, records, 1)
	assert.Equal(t, "sluice-demo", records[0].RequestedModel)
	assert.Equal(t, "local-large", records[0].ServedModel)
	assert.Equal(t, res.AuditID, records[0].ID)
}

// --- stage 1: authenticate --------------------------------------------------

func TestAuthenticate(t *testing.T) {
	h := newHarness(t)

	p, err := h.Authenticate(demoSecret)
	require.NoError(t, err)
	assert.Equal(t, "demo-key", p.KeyID)

	for _, bad := range []string{"wrong", demoSecret + "x", strings.ToUpper(demoSecret)} {
		_, badErr := h.Authenticate(bad)
		var gerr *Error
		require.ErrorAs(t, badErr, &gerr, "secret %q", bad)
		assert.Equal(t, 401, gerr.Status)
		assert.Equal(t, KindAuthentication, gerr.Kind)
		assert.NotContains(t, gerr.Message, bad, "an error must not echo the credential back")
	}

	_, err = h.Authenticate("")
	var gerr *Error
	require.ErrorAs(t, err, &gerr)
	assert.Equal(t, 401, gerr.Status)
	assert.Contains(t, gerr.Message, "missing API key")
}

func TestUnauthenticatedRequestIsStillAudited(t *testing.T) {
	// A rejected request is a fact about who tried what, and an audit log that
	// only records successes cannot answer the question it exists for.
	h := newHarness(t)
	_, err := h.Complete(context.Background(), ask("nope", "sluice-demo", "hi"))
	require.Error(t, err)
	records := h.audit.Records()
	require.Len(t, records, 1)
	assert.Equal(t, "authentication_error", records[0].ErrorCode)
	assert.Empty(t, records[0].KeyID)
}

func TestKeyModelAllowList(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Keys[0].Models = []string{"sluice-demo-cheap"}
	})
	_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hi"))
	var gerr *Error
	require.ErrorAs(t, err, &gerr)
	assert.Equal(t, 403, gerr.Status)
	assert.Equal(t, KindPermission, gerr.Kind)
}

func TestUnknownModelIs404(t *testing.T) {
	h := newHarness(t)
	_, err := h.Complete(context.Background(), ask(demoSecret, "gpt-9", "hi"))
	var gerr *Error
	require.ErrorAs(t, err, &gerr)
	assert.Equal(t, 404, gerr.Status)
}

// --- stage 2: rate limit ----------------------------------------------------

func TestRateLimitRejectsWithARetryAfter(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Keys[0].RateLimit = config.RateLimit{RequestsPerMinute: 2, Burst: 1}
	})
	for i := 0; i < 2; i++ {
		_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hi"))
		require.NoError(t, err)
	}
	_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hi"))
	var gerr *Error
	require.ErrorAs(t, err, &gerr)
	assert.Equal(t, 429, gerr.Status)
	assert.Positive(t, gerr.RetryAfter, "a 429 without a Retry-After is not actionable")
}

func TestTokenLimitBoundsSpendWhereARequestLimitCannot(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Keys[0].RateLimit = config.RateLimit{RequestsPerMinute: 1000, TokensPerMinute: 1000, Burst: 1}
	})
	// Large requests are all comfortably inside the 1000/min request limit and
	// exhaust the token allowance within a handful of calls. The loop rather
	// than a fixed count because Settle refunds the difference between the
	// pre-call estimate and the measured usage, so the exact request at which
	// the bucket empties depends on how long the answers happen to be.
	big := strings.Repeat("word ", 200)
	var gerr *Error
	for i := 0; i < 5; i++ {
		_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", big))
		if err != nil {
			require.ErrorAs(t, err, &gerr)
			break
		}
	}
	require.NotNil(t, gerr, "five 700-token requests must not fit in a 1000-token minute")
	assert.Equal(t, 429, gerr.Status)
	assert.Contains(t, gerr.Message, "tokens", "the rejection names which bucket ran out")
}

// --- stage 3: budget --------------------------------------------------------

func TestBudgetRejectsWhenExhausted(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Keys[0].Budget = config.Budget{
			Limit: config.Money(llm.Microdollar), Window: config.Duration(time.Hour),
			OnExceed: config.OnExceedReject,
		}
	})
	// gpt-4o-mini is priced, so one request spends more than a microdollar.
	_, err := h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "hello"))
	require.NoError(t, err)

	_, err = h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "hello again"))
	var gerr *Error
	require.ErrorAs(t, err, &gerr)
	assert.Equal(t, 402, gerr.Status)
	assert.Equal(t, KindBudget, gerr.Kind)
}

func TestBudgetDegradesToACheaperRoute(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Keys[0].Budget = config.Budget{
			Limit: config.Money(llm.Microdollar), Window: config.Duration(time.Hour),
			OnExceed: config.OnExceedDegrade, DegradeTo: "sluice-demo-cheap",
		}
	})
	_, err := h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "hello"))
	require.NoError(t, err)

	res, err := h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "hello again"))
	require.NoError(t, err, "degradation keeps serving rather than refusing")
	assert.True(t, res.Degraded)
	assert.Equal(t, "local-small", res.Response.Model,
		"the answer comes from the cheaper model, and the response says which")

	records := h.audit.Recent(1)
	require.Len(t, records, 1)
	assert.True(t, records[0].Degraded, "a degradation must be observable after the fact, not silent")
}

func TestTeamBudgetBindsAcrossKeys(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Teams[0].Budget = config.Budget{
			Limit: config.Money(llm.Microdollar), Window: config.Duration(time.Hour),
			OnExceed: config.OnExceedReject,
		}
		c.Keys[0].Budget = config.Budget{}
		c.Keys = append(c.Keys, config.Key{
			ID: "second-key", Secret: "sk-sluice-local-second", Team: "demo",
		})
	})
	_, err := h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "hello"))
	require.NoError(t, err)

	_, err = h.Complete(context.Background(), ask("sk-sluice-local-second", "gpt-4o-mini", "hello"))
	var gerr *Error
	require.ErrorAs(t, err, &gerr)
	assert.Equal(t, 402, gerr.Status, "a second key on the same team spends the same money")
}

// --- stage 4/5: redaction before caching ------------------------------------

func TestRedactionHappensBeforeTheProviderSeesAnything(t *testing.T) {
	seen := &spyProvider{}
	cfg := config.Default()
	g, err := New(Options{
		Config:    cfg,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Providers: map[string]llm.Provider{"local-primary": seen, "local-standby": seen},
	})
	require.NoError(t, err)

	res, err := g.Complete(context.Background(),
		ask(demoSecret, "sluice-demo", "mail alice@example.com about card 4111 1111 1111 1111"))
	require.NoError(t, err)

	sent := seen.last()
	assert.NotContains(t, sent, "alice@example.com")
	assert.NotContains(t, sent, "4111 1111 1111 1111")
	assert.Contains(t, sent, "[SLUICE_EMAIL_0001]")
	assert.Equal(t, 1, res.Redactions[redact.EntityEmail])
	assert.Equal(t, 1, res.Redactions[redact.EntityCreditCard])
}

// TestCacheStoresRedactedText is the stage-order argument made executable. If
// caching ran before redaction, the entry below would hold the address.
func TestCacheStoresRedactedText(t *testing.T) {
	h := newHarness(t)
	prompt := "please email alice@example.com the summary"

	first, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", prompt))
	require.NoError(t, err)
	assert.False(t, first.Cache.Hit)

	second, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", prompt))
	require.NoError(t, err)
	require.True(t, second.Cache.Hit)
	assert.Equal(t, cache.HitExact, second.Cache.Kind)
	assert.Equal(t, first.Response.Message.Content, second.Response.Message.Content,
		"the restored answer is identical whether it came from the provider or the cache")
	assert.Contains(t, second.Response.Message.Content, "alice@example.com",
		"un-redaction still runs on a cache hit, because it happens after the lookup")
	assert.Zero(t, second.Cost, "a cache hit costs nothing; charging it again would double-count")
}

func TestTwoCallersWithDifferentValuesShareOneCacheEntry(t *testing.T) {
	// The consequence of redacting before caching, stated in the package
	// comment: placeholders are positional, so the two prompts are identical
	// once redacted and each caller gets their own value back.
	h := newHarness(t)
	_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "write to alice@example.com"))
	require.NoError(t, err)

	res, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "write to bob@example.org"))
	require.NoError(t, err)
	require.True(t, res.Cache.Hit, "the redacted prompts are identical, so this is a hit")
	assert.Contains(t, res.Response.Message.Content, "bob@example.org")
	assert.NotContains(t, res.Response.Message.Content, "alice@example.com",
		"each caller's own vault decides what the placeholder means")
}

func TestAuditStoresTheRedactedPromptAndCompletion(t *testing.T) {
	h := newHarness(t)
	_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "mail alice@example.com now"))
	require.NoError(t, err)

	records := h.audit.Records()
	require.Len(t, records, 1)
	require.NotEmpty(t, records[0].Prompt)
	assert.NotContains(t, records[0].Prompt[0].Content, "alice@example.com")
	assert.Contains(t, records[0].Prompt[0].Content, "[SLUICE_EMAIL_0001]")
	assert.NotContains(t, records[0].Completion, "alice@example.com",
		"the completion echoes the prompt, and restoring it for the log would put the value back in the file that outlives everything")
	assert.Equal(t, map[string]int{"email": 1}, records[0].RedactionCounts)
}

func TestRedactionCanBeDisabled(t *testing.T) {
	seen := &spyProvider{}
	cfg := config.Default()
	cfg.Redaction.Enabled = false
	g, err := New(Options{
		Config: cfg, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Providers: map[string]llm.Provider{"local-primary": seen, "local-standby": seen},
	})
	require.NoError(t, err)
	_, err = g.Complete(context.Background(), ask(demoSecret, "sluice-demo", "mail alice@example.com"))
	require.NoError(t, err)
	assert.Contains(t, seen.last(), "alice@example.com")
}

// --- streaming --------------------------------------------------------------

func TestStreamRoundTripsRedactedValues(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t)
	sr, err := h.Stream(context.Background(), ask(demoSecret, "sluice-demo", "call bob@corp.io on +1 415 555 0142"))
	require.NoError(t, err)

	var text strings.Builder
	for chunk, cerr := range sr.Chunks {
		require.NoError(t, cerr)
		text.WriteString(chunk.Delta.Content)
	}
	assert.Contains(t, text.String(), "bob@corp.io",
		"the placeholder is restored even though it straddles the provider's chunk boundaries")

	records := h.audit.Records()
	require.Len(t, records, 1)
	assert.True(t, records[0].Stream)
	assert.NotContains(t, records[0].Prompt[0].Content, "bob@corp.io")
	assert.Positive(t, records[0].Usage.OutputTokens, "a stream is billed for what it generated")
}

func TestStreamAbandonedEarlyIsStillAccountedFor(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t)
	sr, err := h.Stream(context.Background(), ask(demoSecret, "sluice-demo", "a long question"))
	require.NoError(t, err)
	for range sr.Chunks {
		break // the client hangs up after one chunk
	}

	records := h.audit.Records()
	require.Len(t, records, 1, "the tokens were generated and paid for whether or not anyone read them")
	assert.True(t, records[0].Stream)
}

func TestStreamNoGoroutineLeakUnderCancellation(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t)
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		sr, err := h.Stream(ctx, ask(demoSecret, "sluice-demo", "question"))
		require.NoError(t, err)
		n := 0
		for range sr.Chunks {
			n++
			if n == 2 {
				cancel()
				break
			}
		}
		cancel()
	}
}

// TestCancelledStreamIsNotCountedAsAnError pins the distinction that keeps an
// error-rate dashboard useful: a client closing a tab is not a provider failure.
func TestCancelledStreamIsNotCountedAsAnError(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t, func(c *config.Config) {
		// Slow enough that cancellation lands mid-generation rather than after
		// the provider has already finished.
		c.Providers[0].Latency = config.Latency{PerToken: config.Duration(2 * time.Millisecond)}
		c.Providers[1].Latency = config.Latency{PerToken: config.Duration(2 * time.Millisecond)}
	})

	ctx, cancel := context.WithCancel(context.Background())
	sr, err := h.Stream(ctx, ask(demoSecret, "sluice-demo", "a long answer please"))
	require.NoError(t, err)

	n := 0
	for chunk, cerr := range sr.Chunks {
		_ = chunk
		if cerr != nil {
			break
		}
		n++
		if n == 1 {
			cancel()
		}
	}
	cancel()

	records := h.audit.Records()
	require.Len(t, records, 1)
	assert.Equal(t, OutcomeClientDisconnected, records[0].ErrorCode,
		"a disconnect is recorded, with its own code, and is not an api_error")
	assert.Empty(t, records[0].Error)
	assert.Positive(t, records[0].Usage.OutputTokens, "what was generated was still paid for")
}

func TestStreamRejectionHappensBeforeAnyChunk(t *testing.T) {
	// The HTTP layer depends on this: a rejection must be a JSON error, not a
	// 200 containing an error event.
	h := newHarness(t)
	sr, err := h.Stream(context.Background(), ask("bad-secret", "sluice-demo", "hi"))
	require.Error(t, err)
	assert.Nil(t, sr.Chunks)
}

func TestStreamServedFromCache(t *testing.T) {
	defer leaktest.Check(t)()

	h := newHarness(t)
	_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "cached question"))
	require.NoError(t, err)

	sr, err := h.Stream(context.Background(), ask(demoSecret, "sluice-demo", "cached question"))
	require.NoError(t, err)
	assert.True(t, sr.Cache.Hit, "streaming and buffered requests share cache entries; the content is the same either way")
	var text strings.Builder
	for chunk, cerr := range sr.Chunks {
		require.NoError(t, cerr)
		text.WriteString(chunk.Delta.Content)
	}
	assert.NotEmpty(t, text.String())
}

// --- failover and accounting ------------------------------------------------

func TestFailoverBetweenProviders(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Providers[0].Failure = config.Failure{Code: "provider_unavailable", Rate: 1}
	})
	res, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hello"))
	require.NoError(t, err)
	assert.Equal(t, "local-standby", res.Response.Provider)
	assert.Equal(t, 1, res.Outcome.Failovers)

	records := h.audit.Recent(1)
	assert.Equal(t, 1, records[0].Failovers)
	assert.Equal(t, "local-standby", records[0].Provider)
}

func TestCostIsComputedFromMeasuredTokens(t *testing.T) {
	h := newHarness(t)
	res, err := h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "hello"))
	require.NoError(t, err)

	price, ok := h.Pricing().Price("gpt-4o-mini")
	require.True(t, ok)
	assert.Equal(t, price.Cost(res.Response.Usage), res.Cost)
	assert.Positive(t, res.Cost)

	records := h.audit.Recent(1)
	assert.Equal(t, res.Cost, records[0].Cost)
}

func TestUnpricedModelRecordsZeroRatherThanFailing(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.Routes[0].Targets[0].Model = "some-model-nobody-priced"
		c.Routes[0].Targets = c.Routes[0].Targets[:1]
	})
	res, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hi"))
	require.NoError(t, err, "an unpriced model must not make the gateway refuse to serve")
	assert.Zero(t, res.Cost)
	assert.Positive(t, res.Response.Usage.TotalTokens(),
		"the token counts are still recorded, so the price can be applied retrospectively with replay")
}

// --- stats and sweep --------------------------------------------------------

func TestStatsReflectRealActivity(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		_, err := h.Complete(context.Background(), ask(demoSecret, "gpt-4o-mini", "mail alice@example.com"))
		require.NoError(t, err)
	}
	s := h.Stats()
	assert.Equal(t, 3, s.Requests.Total)
	assert.Equal(t, 2, s.Requests.CacheHits, "the second and third are exact hits")
	assert.NotEmpty(t, s.Latency)
	assert.NotEmpty(t, s.Targets)
	require.NotEmpty(t, s.Redactions)
	assert.Equal(t, "email", s.Redactions[0].EntityType)

	var spent float64
	for _, b := range s.Budgets {
		if b.Subject == "key:demo-key" {
			spent = b.SpentUSD
		}
	}
	assert.Positive(t, spent)
	var series float64
	for _, p := range s.Spend.PointsUSD {
		series += p
	}
	assert.InDelta(t, spent, series, 1e-9, "the chart is drawn from the ledger that enforces the budget")
}

func TestSweepIsSafeToCall(t *testing.T) {
	h := newHarness(t)
	_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "hi"))
	require.NoError(t, err)
	h.clock.advance(24 * time.Hour)
	h.Sweep()
}

func TestConcurrentRequestsAreRaceFree(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.Complete(context.Background(), ask(demoSecret, "sluice-demo", "shared question"))
			if err != nil {
				t.Error(err)
			}
			sr, err := h.Stream(context.Background(), ask(demoSecret, "sluice-demo", "another question"))
			if err != nil {
				t.Error(err)
				return
			}
			for _, cerr := range sr.Chunks {
				if cerr != nil {
					t.Error(cerr)
				}
			}
		}(i)
	}
	wg.Wait()
	assert.Len(t, h.audit.Records(), 32)
}

func TestNewRejectsAnInvalidConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Server.Addr = ""
	_, err := New(Options{Config: cfg})
	assert.Error(t, err, "a gateway must not start on a configuration it would reject at validation")
}

// --- helpers ----------------------------------------------------------------

// spyProvider records what actually reached the upstream, which is the only
// way to assert that redaction happened before the network boundary rather
// than merely happening.
type spyProvider struct {
	mu     sync.Mutex
	prompt string
	inner  *local.Provider
}

func (s *spyProvider) provider() *local.Provider {
	if s.inner == nil {
		p, err := local.New(local.Config{Name: "spy"})
		if err != nil {
			panic(err) // test helper; a misconfigured fixture is a test bug
		}
		s.inner = p
	}
	return s.inner
}

func (s *spyProvider) Name() string { return "spy" }

func (s *spyProvider) record(req llm.Request) {
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	s.mu.Lock()
	s.prompt = b.String()
	s.mu.Unlock()
}

func (s *spyProvider) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt
}

func (s *spyProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	s.record(req)
	return s.provider().Complete(ctx, req)
}

func (s *spyProvider) Stream(ctx context.Context, req llm.Request) (iter.Seq2[llm.Chunk, error], error) {
	s.record(req)
	return s.provider().Stream(ctx, req)
}

var _ llm.Provider = (*spyProvider)(nil)
