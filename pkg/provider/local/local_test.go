package local_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/internal/leaktest"
	"github.com/sluice-gw/sluice/pkg/llm"
	"github.com/sluice-gw/sluice/pkg/provider/local"
)

// mustNew builds a Provider from a configuration known to be valid. New
// returning an error is a configuration mistake, not a runtime condition, and
// every test here supplies a literal.
func mustNew(t testing.TB, cfg local.Config) *local.Provider {
	t.Helper()
	p, err := local.New(cfg)
	require.NoError(t, err)
	return p
}

func ask(text string) llm.Request {
	return llm.Request{
		Model:    "local-small",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: text}},
	}
}

func TestNewValidatesConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  local.Config
		ok   bool
	}{
		{"zero value", local.Config{}, true},
		{"failure with a rate", local.Config{Failure: local.Failure{Code: llm.CodeTimeout, Rate: 1}}, true},
		{"failure without a rate never fires", local.Config{Failure: local.Failure{Code: llm.CodeTimeout}}, false},
		{"rate above one", local.Config{Failure: local.Failure{Code: llm.CodeTimeout, Rate: 1.5}}, false},
		{"jitter above one", local.Config{Latency: local.Latency{Jitter: 2}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := local.New(tc.cfg)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestCompleteIsDeterministic(t *testing.T) {
	p := mustNew(t, local.Config{})
	ctx := context.Background()

	first, err := p.Complete(ctx, ask("what does this cost"))
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		again, againErr := p.Complete(ctx, ask("what does this cost"))
		require.NoError(t, againErr)
		assert.Equal(t, first.Message.Content, again.Message.Content)
		assert.Equal(t, first.ID, again.ID)
		assert.Equal(t, first.Usage, again.Usage)
	}

	other, err := p.Complete(ctx, ask("something entirely different"))
	require.NoError(t, err)
	assert.NotEqual(t, first.Message.Content, other.Message.Content)
}

// Determinism must survive concurrency: this is the property a provider that
// advanced one shared PRNG per call would fail.
func TestDeterminismUnderConcurrency(t *testing.T) {
	p := mustNew(t, local.Config{})
	want, err := p.Complete(context.Background(), ask("concurrent question"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	results := make([]string, 50)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Interleave a different request so that a shared RNG would be
			// advanced by a varying amount between the calls under test.
			_, _ = p.Complete(context.Background(), ask("noise"))
			resp, err := p.Complete(context.Background(), ask("concurrent question"))
			if err == nil {
				results[i] = resp.Message.Content
			}
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		assert.Equalf(t, want.Message.Content, got, "goroutine %d", i)
	}
}

func TestSeedSeparatesProviders(t *testing.T) {
	a := mustNew(t, local.Config{Name: "a", Seed: 1})
	b := mustNew(t, local.Config{Name: "b", Seed: 2})

	ra, err := a.Complete(context.Background(), ask("same question"))
	require.NoError(t, err)
	rb, err := b.Complete(context.Background(), ask("same question"))
	require.NoError(t, err)

	assert.Equal(t, "a", ra.Provider)
	assert.Equal(t, "b", rb.Provider)
	assert.NotEqual(t, ra.Message.Content, rb.Message.Content,
		"a failover test has to be able to tell which upstream answered")
}

func TestEchoesPromptSoPlaceholdersSurvive(t *testing.T) {
	p := mustNew(t, local.Config{})
	// The scenario redaction depends on: the model writes new text around a
	// token it does not understand, and does not alter the token.
	resp, err := p.Complete(context.Background(), ask("email [SLUICE_EMAIL_0001] about the invoice"))
	require.NoError(t, err)
	assert.Contains(t, resp.Message.Content, "[SLUICE_EMAIL_0001]")
	assert.Greater(t, len(resp.Message.Content), len("email [SLUICE_EMAIL_0001] about the invoice"))
}

func TestStreamMatchesComplete(t *testing.T) {
	defer leaktest.Check(t)()
	p := mustNew(t, local.Config{})
	req := ask("stream and buffer must agree")

	whole, err := p.Complete(context.Background(), req)
	require.NoError(t, err)

	seq, err := p.Stream(context.Background(), req)
	require.NoError(t, err)
	streamed, err := llm.Collect(seq)
	require.NoError(t, err)

	assert.Equal(t, whole.Message.Content, streamed.Message.Content)
	assert.Equal(t, whole.Message.ToolCalls, streamed.Message.ToolCalls)
	assert.Equal(t, whole.FinishReason, streamed.FinishReason)
	assert.Equal(t, whole.Usage, streamed.Usage)
	assert.Equal(t, whole.ID, streamed.ID)
}

func TestStreamChunking(t *testing.T) {
	defer leaktest.Check(t)()
	p := mustNew(t, local.Config{MaxChunkWords: 3})
	seq, err := p.Stream(context.Background(), ask("please produce several chunks of output"))
	require.NoError(t, err)

	var (
		content strings.Builder
		chunks  int
		final   *llm.Chunk
	)
	for c, err := range seq {
		require.NoError(t, err)
		chunks++
		content.WriteString(c.Delta.Content)
		if c.Usage != nil {
			cc := c
			final = &cc
		}
	}
	assert.Greater(t, chunks, 2, "a whole response in one chunk is not streaming")
	require.NotNil(t, final, "usage must arrive on the last chunk")
	assert.Equal(t, llm.FinishStop, final.FinishReason)
	assert.Positive(t, final.Usage.OutputTokens)
	assert.NotEmpty(t, content.String())
}

func TestStreamAbandonedEarly(t *testing.T) {
	defer leaktest.Check(t)()
	p := mustNew(t, local.Config{Latency: local.Latency{PerToken: time.Millisecond}})
	seq, err := p.Stream(context.Background(), ask("abandon this stream after one chunk"))
	require.NoError(t, err)

	for range seq {
		break
	}
	// The assertion is leaktest's: nothing may still be generating.
}

func TestStreamHonoursCancellation(t *testing.T) {
	defer leaktest.Check(t)()
	p := mustNew(t, local.Config{Latency: local.Latency{TimeToFirstToken: time.Hour}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	seq, err := p.Stream(ctx, ask("cancelled before it starts"))
	require.NoError(t, err, "cancellation is discovered while iterating, not before")

	var gotErr error
	for _, err := range seq {
		if err != nil {
			gotErr = err
		}
	}
	require.Error(t, gotErr)
	require.ErrorIs(t, gotErr, context.Canceled)
	assert.False(t, llm.IsRetryable(gotErr), "the caller cancelled; retrying would fight them")
}

func TestCompleteHonoursDeadline(t *testing.T) {
	p := mustNew(t, local.Config{Latency: local.Latency{TimeToFirstToken: time.Hour}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := p.Complete(ctx, ask("too slow"))
	require.Error(t, err)
	assert.Equal(t, llm.CodeTimeout, llm.CodeOf(err))
	assert.True(t, llm.IsRetryable(err))
}

// Every code in the taxonomy must be injectable, because every one of them has
// a distinct downstream policy that needs a test.
func TestFailureInjectionCoversTaxonomy(t *testing.T) {
	codes := []llm.ErrorCode{
		llm.CodeRateLimited, llm.CodeContextLengthExceeded, llm.CodeContentFiltered,
		llm.CodeAuthentication, llm.CodeProviderUnavailable, llm.CodeInvalidRequest,
		llm.CodeTimeout, llm.CodeUnknown,
	}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			p := mustNew(t, local.Config{Failure: local.Failure{
				Code: code, Rate: 1, RetryAfter: 3 * time.Second,
			}})

			_, err := p.Complete(context.Background(), ask("x"))
			require.Error(t, err)
			assert.Equal(t, code, llm.CodeOf(err))

			_, streamErr := p.Stream(context.Background(), ask("x"))
			require.Error(t, streamErr)
			assert.Equal(t, code, llm.CodeOf(streamErr),
				"a pre-stream failure must arrive as the outer error so a caller can still fail over")

			if code == llm.CodeRateLimited {
				d, ok := llm.RetryAfter(err)
				assert.True(t, ok)
				assert.Equal(t, 3*time.Second, d)
			}
		})
	}
}

func TestPartialFailureRateIsDeterministicPerRequest(t *testing.T) {
	p := mustNew(t, local.Config{Failure: local.Failure{Code: llm.CodeProviderUnavailable, Rate: 0.5}})

	failed, succeeded := 0, 0
	for i := 0; i < 200; i++ {
		req := ask(strings.Repeat("q", i%7) + string(rune('a'+i%26)) + "?")
		_, err := p.Complete(context.Background(), req)
		if err != nil {
			failed++
			// The same request must fail the same way every time.
			_, again := p.Complete(context.Background(), req)
			require.Error(t, again)
		} else {
			succeeded++
			_, again := p.Complete(context.Background(), req)
			require.NoError(t, again)
		}
	}
	assert.Positive(t, failed)
	assert.Positive(t, succeeded)
}

func TestMidStreamFailure(t *testing.T) {
	defer leaktest.Check(t)()
	p := mustNew(t, local.Config{
		MaxChunkWords: 1,
		Failure:       local.Failure{Code: llm.CodeProviderUnavailable, Rate: 1, AfterChunks: 2},
	})

	seq, err := p.Stream(context.Background(), ask("this one dies halfway"))
	require.NoError(t, err, "output has already started, so the failure cannot be the outer error")

	delivered := 0
	var streamErr error
	for _, err := range seq {
		if err != nil {
			streamErr = err
			break
		}
		delivered++
	}
	assert.Equal(t, 2, delivered)
	require.Error(t, streamErr)
	assert.Equal(t, llm.CodeProviderUnavailable, llm.CodeOf(streamErr))
}

func TestContextWindowIsEnforced(t *testing.T) {
	p := mustNew(t, local.Config{ContextTokens: 50})
	_, err := p.Complete(context.Background(), ask(strings.Repeat("word ", 200)))
	require.Error(t, err)
	assert.Equal(t, llm.CodeContextLengthExceeded, llm.CodeOf(err))
	assert.False(t, llm.IsRetryable(err))
	assert.False(t, llm.ShouldFailover(err))
}

func TestMaxTokensTruncates(t *testing.T) {
	p := mustNew(t, local.Config{})
	req := ask("give me a long answer")
	req.MaxTokens = 6

	resp, err := p.Complete(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, llm.FinishLength, resp.FinishReason)
	assert.LessOrEqual(t, resp.Usage.OutputTokens, 6)
}

func TestInvalidRequests(t *testing.T) {
	p := mustNew(t, local.Config{Models: []string{"local-small"}})
	for _, tc := range []struct {
		name string
		req  llm.Request
	}{
		{"no messages", llm.Request{Model: "local-small"}},
		{"unknown model", ask2("local-huge", "hi")},
		{"bad role", llm.Request{Model: "local-small", Messages: []llm.Message{{Role: "function", Content: "x"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Complete(context.Background(), tc.req)
			require.Error(t, err)
			assert.Equal(t, llm.CodeInvalidRequest, llm.CodeOf(err))
			assert.False(t, llm.ShouldFailover(err), "every provider will reject this identically")
		})
	}
}

func ask2(model, text string) llm.Request {
	r := ask(text)
	r.Model = model
	return r
}

func TestToolCalls(t *testing.T) {
	p := mustNew(t, local.Config{})
	req := ask("what is the weather in Oslo")
	req.Tools = []llm.Tool{{
		Name:       "get_weather",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"days":{"type":"integer"}}}`),
	}}
	req.ToolChoice = llm.ToolChoiceRequired

	resp, err := p.Complete(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, resp.Message.ToolCalls, 1)
	call := resp.Message.ToolCalls[0]
	assert.Equal(t, "get_weather", call.Name)

	var args struct {
		City string `json:"city"`
		Days int    `json:"days"`
	}
	require.NoError(t, json.Unmarshal(call.Arguments, &args), "arguments must be valid JSON for a well-formed schema")
	assert.NotEmpty(t, args.City)

	req.ToolChoice = llm.ToolChoiceNone
	resp, err = p.Complete(context.Background(), req)
	require.NoError(t, err)
	assert.Empty(t, resp.Message.ToolCalls)
}

func TestToolCallArgumentsSurviveStreaming(t *testing.T) {
	defer leaktest.Check(t)()
	p := mustNew(t, local.Config{})
	req := ask("call the tool")
	req.Tools = []llm.Tool{{Name: "search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}
	req.ToolChoice = llm.ToolChoiceRequired

	seq, err := p.Stream(context.Background(), req)
	require.NoError(t, err)
	collected, err := llm.Collect(seq)
	require.NoError(t, err)

	require.Len(t, collected.Message.ToolCalls, 1)
	assert.True(t, json.Valid(collected.Message.ToolCalls[0].Arguments),
		"arguments split across chunks must reassemble into valid JSON")
}

func TestLatencyIsSimulated(t *testing.T) {
	p := mustNew(t, local.Config{Latency: local.Latency{TimeToFirstToken: 20 * time.Millisecond}})
	start := time.Now()
	_, err := p.Complete(context.Background(), ask("slow down"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 15*time.Millisecond)
}

func TestUsageIsNotMarkedEstimated(t *testing.T) {
	p := mustNew(t, local.Config{})
	resp, err := p.Complete(context.Background(), ask("count my tokens"))
	require.NoError(t, err)
	assert.False(t, resp.Usage.Estimated,
		"the provider generated this text with this tokenizer; the count is exact for it")
	assert.Positive(t, resp.Usage.InputTokens)
	assert.Positive(t, resp.Usage.OutputTokens)
}

func TestCostIsComputable(t *testing.T) {
	p := mustNew(t, local.Config{})
	resp, err := p.Complete(context.Background(), ask("what will this cost"))
	require.NoError(t, err)

	pricing := llm.DefaultPricing().With("local-small",
		llm.ModelPrice{InputPerMillion: 1 * llm.Dollar, OutputPerMillion: 2 * llm.Dollar})
	cost, err := pricing.Cost(resp.Model, resp.Usage)
	require.NoError(t, err)
	assert.Positive(t, int64(cost))
}

func BenchmarkComplete(b *testing.B) {
	p := mustNew(b, local.Config{})
	ctx := context.Background()
	req := ask("benchmark the gateway core")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := p.Complete(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStream(b *testing.B) {
	p := mustNew(b, local.Config{})
	ctx := context.Background()
	req := ask("benchmark the streaming path")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		seq, err := p.Stream(ctx, req)
		if err != nil {
			b.Fatal(err)
		}
		for _, err := range seq {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
