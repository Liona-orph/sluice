package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/pkg/llm"
)

func ask(text string) llm.Request {
	return llm.Request{
		Model:    "gpt-4o",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: text}},
	}
}

func respond(text string) llm.Response {
	return llm.Response{
		ID: "r", Model: "gpt-4o", Provider: "test",
		Message:      llm.Message{Role: llm.RoleAssistant, Content: text},
		FinishReason: llm.FinishStop,
		Usage:        llm.Usage{InputTokens: 10, OutputTokens: 5},
	}
}

// clock is a manual clock, so that TTL behaviour is tested by advancing time
// rather than by sleeping.
type clock struct {
	mu time.Time
	sync.Mutex
}

func newClock() *clock { return &clock{mu: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.Lock()
	defer c.Unlock()
	return c.mu
}

func (c *clock) Advance(d time.Duration) {
	c.Lock()
	defer c.Unlock()
	c.mu = c.mu.Add(d)
}

func TestNewValidatesOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		ok   bool
	}{
		{"zero value", Options{}, true},
		{"negative entries", Options{MaxEntries: -1}, false},
		{"negative ttl", Options{TTL: -time.Second}, false},
		{"threshold above one", Options{SimilarityThreshold: 1.5}, false},
		{"threshold below zero", Options{SimilarityThreshold: -0.1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.opts)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestExactHitAndMiss(t *testing.T) {
	c, err := New(Options{})
	require.NoError(t, err)
	ctx := context.Background()

	res, err := c.Get(ctx, ask("hello"))
	require.NoError(t, err)
	assert.False(t, res.Hit)

	require.NoError(t, c.Put(ctx, ask("hello"), respond("hi there")))

	res, err = c.Get(ctx, ask("hello"))
	require.NoError(t, err)
	require.True(t, res.Hit)
	assert.Equal(t, HitExact, res.Kind)
	assert.InDelta(t, 1.0, res.Similarity, 0)
	assert.Equal(t, "hi there", res.Response.Message.Content)

	res, err = c.Get(ctx, ask("goodbye"))
	require.NoError(t, err)
	assert.False(t, res.Hit)

	s := c.Stats()
	assert.Equal(t, uint64(1), s.ExactHits)
	assert.Equal(t, uint64(2), s.Misses)
	assert.InDelta(t, 1.0/3.0, s.HitRate(), 1e-9)
}

// The key includes everything that changes the output and excludes what does
// not; these are the two halves of that claim at the cache level.
func TestKeySensitivity(t *testing.T) {
	ctx := context.Background()
	c, err := New(Options{})
	require.NoError(t, err)
	require.NoError(t, c.Put(ctx, ask("hello"), respond("hi")))

	differing := []struct {
		name string
		req  llm.Request
	}{
		{"model", func() llm.Request { r := ask("hello"); r.Model = "gpt-4o-mini"; return r }()},
		{"temperature", func() llm.Request { r := ask("hello"); r.Temperature = llm.Ptr(0.9); return r }()},
		{"max tokens", func() llm.Request { r := ask("hello"); r.MaxTokens = 10; return r }()},
		{"extra message", func() llm.Request {
			r := ask("hello")
			r.Messages = append(r.Messages, llm.Message{Role: llm.RoleAssistant, Content: "hi"})
			return r
		}()},
	}
	for _, tc := range differing {
		t.Run("miss on "+tc.name, func(t *testing.T) {
			res, err := c.Get(ctx, tc.req)
			require.NoError(t, err)
			assert.False(t, res.Hit)
		})
	}

	t.Run("hit despite differing metadata", func(t *testing.T) {
		r := ask("hello")
		r.Metadata = map[string]string{"tenant": "acme", "user": "u-17"}
		res, err := c.Get(ctx, r)
		require.NoError(t, err)
		assert.True(t, res.Hit, "attribution is not part of the question")
	})
}

func TestTTL(t *testing.T) {
	clk := newClock()
	c, err := New(Options{TTL: 10 * time.Minute, Now: clk.Now})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, c.Put(ctx, ask("hello"), respond("hi")))

	clk.Advance(9 * time.Minute)
	res, err := c.Get(ctx, ask("hello"))
	require.NoError(t, err)
	require.True(t, res.Hit)
	assert.Equal(t, 9*time.Minute, res.Age)

	clk.Advance(2 * time.Minute)
	res, err = c.Get(ctx, ask("hello"))
	require.NoError(t, err)
	assert.False(t, res.Hit)
	assert.Equal(t, uint64(1), c.Stats().Expirations)
	assert.Zero(t, c.Len(), "an expired entry is dropped on the lookup that finds it")
}

func TestSweep(t *testing.T) {
	clk := newClock()
	c, err := New(Options{TTL: time.Minute, Now: clk.Now})
	require.NoError(t, err)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, c.Put(ctx, ask("q"+itoa(i)), respond("a")))
	}
	assert.Equal(t, 0, c.Sweep())
	clk.Advance(2 * time.Minute)
	assert.Equal(t, 5, c.Sweep())
	assert.Zero(t, c.Len())
}

func TestLRUEviction(t *testing.T) {
	c, err := New(Options{MaxEntries: 3})
	require.NoError(t, err)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, c.Put(ctx, ask("q"+itoa(i)), respond("a"+itoa(i))))
	}
	// Touch the oldest so that it is no longer the eviction candidate.
	res, err := c.Get(ctx, ask("q0"))
	require.NoError(t, err)
	require.True(t, res.Hit)

	require.NoError(t, c.Put(ctx, ask("q3"), respond("a3")))
	assert.Equal(t, 3, c.Len())

	for _, tc := range []struct {
		prompt string
		want   bool
	}{
		{"q0", true},  // recently used
		{"q1", false}, // least recently used, evicted
		{"q2", true},
		{"q3", true},
	} {
		res, err := c.Get(ctx, ask(tc.prompt))
		require.NoError(t, err)
		assert.Equalf(t, tc.want, res.Hit, "%s", tc.prompt)
	}
	assert.Equal(t, uint64(1), c.Stats().Evictions)
}

func TestPutOverwrites(t *testing.T) {
	c, err := New(Options{})
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, c.Put(ctx, ask("hello"), respond("first")))
	require.NoError(t, c.Put(ctx, ask("hello"), respond("second")))

	res, err := c.Get(ctx, ask("hello"))
	require.NoError(t, err)
	assert.Equal(t, "second", res.Response.Message.Content)
	assert.Equal(t, 1, c.Len())
}

// A truncated or filtered response is not an answer, and storing one would make
// a transient limit permanent.
func TestIncompleteResponsesAreNotStored(t *testing.T) {
	c, err := New(Options{})
	require.NoError(t, err)
	ctx := context.Background()

	for _, reason := range []llm.FinishReason{llm.FinishLength, llm.FinishContentFilter} {
		resp := respond("half an ans")
		resp.FinishReason = reason
		require.NoError(t, c.Put(ctx, ask("hello"), resp))
		res, err := c.Get(ctx, ask("hello"))
		require.NoError(t, err)
		assert.Falsef(t, res.Hit, "%s must not be cached", reason)
	}
}

func TestBypass(t *testing.T) {
	c, err := New(Options{})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, c.Put(ctx, ask("hello"), respond("stale")))

	res, err := c.Get(WithBypass(ctx), ask("hello"))
	require.NoError(t, err)
	assert.False(t, res.Hit)
	assert.False(t, Bypassed(ctx))
	assert.True(t, Bypassed(WithBypass(ctx)))

	// A bypass does not count as a miss: it never looked.
	assert.Zero(t, c.Stats().Misses)

	// Writing through a bypassed context refreshes the entry for everyone else.
	require.NoError(t, c.Put(WithBypass(ctx), ask("hello"), respond("fresh")))
	res, err = c.Get(ctx, ask("hello"))
	require.NoError(t, err)
	assert.Equal(t, "fresh", res.Response.Message.Content)
}

func TestInvalidation(t *testing.T) {
	c, err := New(Options{})
	require.NoError(t, err)
	ctx := context.Background()

	a := ask("hello")
	b := ask("other")
	b.Model = "claude-3-5-sonnet"
	require.NoError(t, c.Put(ctx, a, respond("1")))
	require.NoError(t, c.Put(ctx, b, respond("2")))

	assert.True(t, c.Invalidate(KeyFor(a)))
	assert.False(t, c.Invalidate(KeyFor(a)), "invalidating twice is not an error but is not a hit either")
	assert.Equal(t, 1, c.Len())

	require.NoError(t, c.Put(ctx, a, respond("1")))
	assert.Equal(t, 1, c.InvalidateNamespace("gpt-4o"))
	assert.Equal(t, 1, c.Len())

	c.Purge()
	assert.Zero(t, c.Len())
	res, err := c.Get(ctx, b)
	require.NoError(t, err)
	assert.False(t, res.Hit)
}

func TestKeyForAndString(t *testing.T) {
	k := KeyFor(ask("hello"))
	assert.Equal(t, "gpt-4o", k.Namespace)
	assert.Len(t, k.Hash, 64)
	assert.Equal(t, "gpt-4o/"+k.Hash, k.String())
}

type failingEmbedder struct{ calls atomic.Int64 }

func (f *failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	f.calls.Add(1)
	return nil, errors.New("model unavailable")
}
func (f *failingEmbedder) Dimensions() int { return 8 }
func (f *failingEmbedder) ID() string      { return "failing" }

// An embedder outage must degrade the cache, not the gateway.
func TestEmbedderFailureDegradesToMiss(t *testing.T) {
	emb := &failingEmbedder{}
	c, err := New(Options{Embedder: emb})
	require.NoError(t, err)
	ctx := context.Background()

	err = c.Put(ctx, ask("hello"), respond("hi"))
	require.Error(t, err, "a store that cannot be indexed is reported")

	res, getErr := c.Get(ctx, ask("hello"))
	require.Error(t, getErr)
	assert.False(t, res.Hit, "the result is still usable as a miss")
	assert.Equal(t, uint64(1), c.Stats().Misses)
}

// Concurrency is a correctness property here: the cache is shared by every
// request goroutine, and the LRU list is mutated by readers.
func TestConcurrentReadersAndWriters(t *testing.T) {
	c, err := New(Options{MaxEntries: 64, Embedder: NewHashingEmbedder(64)})
	require.NoError(t, err)
	ctx := context.Background()

	const goroutines = 16
	const iterations = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				req := ask("prompt " + itoa((g*iterations+i)%128))
				switch i % 5 {
				case 0, 1:
					_ = c.Put(ctx, req, respond("answer "+itoa(i)))
				case 2, 3:
					if _, err := c.Get(ctx, req); err != nil {
						t.Error(err)
						return
					}
				case 4:
					switch i % 15 {
					case 4:
						c.Invalidate(KeyFor(req))
					case 9:
						c.Sweep()
					default:
						_ = c.Stats()
						_ = c.Len()
					}
				}
			}
		}(g)
	}
	wg.Wait()

	assert.LessOrEqual(t, c.Len(), 64, "the bound must hold under contention")
	s := c.Stats()
	assert.Positive(t, s.Stores)
	assert.Equal(t, c.Len(), s.Entries)
}

// The LRU bookkeeping is the part most likely to corrupt under concurrency: a
// list element removed twice, or a map entry left behind, shows up as a length
// mismatch rather than as a crash.
func TestConcurrentEvictionKeepsMapAndListInStep(t *testing.T) {
	c, err := New(Options{MaxEntries: 8})
	require.NoError(t, err)
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = c.Put(ctx, ask("k"+itoa(g*500+i)), respond("v"))
			}
		}(g)
	}
	wg.Wait()

	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Len(t, c.entries, c.lru.Len(),
		"every list element must have exactly one map entry")
	assert.LessOrEqual(t, c.lru.Len(), 8)
}

func BenchmarkExactGet(b *testing.B) {
	c, _ := New(Options{})
	ctx := context.Background()
	req := ask("what does this cost")
	_ = c.Put(ctx, req, respond("not much"))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := c.Get(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
