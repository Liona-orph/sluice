// Package cache stores completions so that the same question is not paid for
// twice.
//
// Two lookups happen, in order. The exact-match cache is keyed on a canonical
// hash of the request and is unambiguously safe: the same request gets the same
// answer. The semantic cache embeds the prompt and returns the nearest stored
// entry when the similarity clears a threshold, which is where the savings are
// and where the risk is.
//
// The risk deserves naming. A false cache hit returns a confidently wrong
// answer to a question nobody asked, and unlike a provider outage it is silent:
// no error, no retry, no alert, just a response that does not correspond to the
// prompt. Three things bound it here. The default threshold is set from a
// measurement rather than a guess (see semantic_test.go, which reports the
// false-hit rate on a fixture set of similar-but-different prompts). A
// semantic hit is only permitted between requests whose parameters are
// identical, so approximation applies to what was asked and never to how. And
// the semantic cache is off unless an Embedder is configured, so the safe
// behaviour is the one you get by default.
package cache

import (
	"container/list"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// DefaultSimilarityThreshold is the cosine similarity a semantic candidate must
// reach.
//
// 0.97 is deliberately conservative, and the margin is measured rather than
// assumed. On the fixture pairs the false-hit rate at this threshold is 0 out
// of 20 adversarial pairs, the true-hit rate is 5 out of 5, and the closest
// non-equivalent pair -- "What did revenue do in Q1?" against the same question
// about Q4 -- scores 0.879. False hits only begin to appear below 0.90, so
// there is roughly nine points of headroom between the setting and the first
// wrong answer.
//
// A real embedding model has a differently shaped similarity distribution and
// needs its own threshold measured the same way, which is why this is a knob
// and not a constant in the lookup path.
const DefaultSimilarityThreshold = 0.97

// DefaultTTL bounds how long an answer stays servable. Model behaviour and the
// world both change; an hour is short enough that a stale answer is a nuisance
// rather than an incident, and long enough to absorb the bursts of identical
// requests that make caching worth doing.
const DefaultTTL = time.Hour

// DefaultMaxEntries bounds memory. Entries are small -- a response and a few
// hundred floats -- so this is a few tens of megabytes.
const DefaultMaxEntries = 10_000

// HitKind distinguishes how an entry was found, because the two have very
// different trust levels and a caller may want to log or meter them apart.
type HitKind string

// The ways a lookup can resolve.
const (
	HitNone     HitKind = ""
	HitExact    HitKind = "exact"
	HitSemantic HitKind = "semantic"
)

// Result is the outcome of a lookup.
type Result struct {
	// Hit reports whether Response is populated.
	Hit  bool
	Kind HitKind
	// Similarity is 1 for an exact hit and the measured cosine for a semantic
	// one, so that a caller can log how close a call it was.
	Similarity float64
	Key        Key
	Response   llm.Response
	// Age is how long ago the entry was stored.
	Age time.Duration
}

// Options configures a Cache. The zero value is usable and gives an
// exact-match-only cache with the defaults above.
type Options struct {
	MaxEntries int
	TTL        time.Duration
	// Embedder enables the semantic cache. Nil disables it.
	Embedder Embedder
	// SimilarityThreshold overrides DefaultSimilarityThreshold.
	SimilarityThreshold float64
	// Now is the clock, injectable so that TTL behaviour is testable without
	// sleeping.
	Now func() time.Time
}

// Stats are cumulative counters for one Cache.
type Stats struct {
	Entries      int
	ExactHits    uint64
	SemanticHits uint64
	Misses       uint64
	Evictions    uint64
	Expirations  uint64
	Stores       uint64
	// NearMisses counts semantic candidates that were the best available but
	// fell below the threshold. A high count next to a low SemanticHits is the
	// signal that the threshold is set too tight for the embedder in use.
	NearMisses uint64
}

// HitRate is hits over lookups, or 0 if there have been none.
func (s Stats) HitRate() float64 {
	total := s.ExactHits + s.SemanticHits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.ExactHits+s.SemanticHits) / float64(total)
}

// entryOf reads the value out of an LRU element.
//
// The type assertion has no comma-ok because the list is private to this file
// and only ever holds *entry; a failure would be a bug in this package, and a
// nil return would turn that bug into a nil dereference three frames away. It
// is a helper rather than an inline assertion at six call sites so that the
// reasoning is written once.
func entryOf(el *list.Element) *entry {
	return el.Value.(*entry) //nolint:errcheck,forcetypeassert // see above
}

type entry struct {
	key        Key
	paramsHash string
	embedding  []float32
	response   llm.Response
	storedAt   time.Time
	expiresAt  time.Time
}

// Cache is a bounded, TTL'd store of completions with optional semantic lookup.
//
// It is safe for concurrent use. A single mutex protects everything: the LRU
// list makes reads mutating anyway, so a read-write lock would only add the
// illusion of concurrency. The semantic scan is the one operation long enough
// to care about, and it is bounded by MaxEntries.
type Cache struct {
	mu      sync.Mutex
	entries map[Key]*list.Element
	lru     *list.List // front is most recently used

	maxEntries int
	ttl        time.Duration
	embedder   Embedder
	threshold  float64
	now        func() time.Time

	stats Stats
}

// New builds a Cache.
func New(opts Options) (*Cache, error) {
	if opts.MaxEntries < 0 {
		return nil, fmt.Errorf("cache: MaxEntries %d is negative", opts.MaxEntries)
	}
	if opts.TTL < 0 {
		return nil, fmt.Errorf("cache: TTL %v is negative", opts.TTL)
	}
	if opts.SimilarityThreshold < 0 || opts.SimilarityThreshold > 1 {
		return nil, fmt.Errorf("cache: SimilarityThreshold %v is not in [0,1]", opts.SimilarityThreshold)
	}
	c := &Cache{
		entries:    map[Key]*list.Element{},
		lru:        list.New(),
		maxEntries: opts.MaxEntries,
		ttl:        opts.TTL,
		embedder:   opts.Embedder,
		threshold:  opts.SimilarityThreshold,
		now:        opts.Now,
	}
	if c.maxEntries == 0 {
		c.maxEntries = DefaultMaxEntries
	}
	if c.ttl == 0 {
		c.ttl = DefaultTTL
	}
	if c.threshold == 0 {
		c.threshold = DefaultSimilarityThreshold
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// bypassKey is the context key for cache bypass. An unexported struct type
// keeps it from colliding with any other package's context values.
type bypassKey struct{}

// WithBypass marks a context as skipping cache reads.
//
// Writes still happen, which is the useful semantics: a caller who bypasses
// because they suspect the entry is stale wants the fresh answer stored in its
// place, not a bypass that leaves the stale entry to serve everyone else.
func WithBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, bypassKey{}, true)
}

// Bypassed reports whether ctx carries a bypass.
func Bypassed(ctx context.Context) bool {
	v, _ := ctx.Value(bypassKey{}).(bool)
	return v
}

// Get looks up a response for req.
//
// A returned error means the lookup could not be completed -- an embedder
// failure, essentially -- and a caller may reasonably treat it as a miss and
// carry on; Result.Hit is false in that case. Failing the whole request because
// an optimisation broke would be the wrong trade.
func (c *Cache) Get(ctx context.Context, req llm.Request) (Result, error) {
	if Bypassed(ctx) {
		return Result{}, nil
	}
	key := KeyFor(req)
	now := c.now()

	c.mu.Lock()
	if el, ok := c.entries[key]; ok {
		e := entryOf(el)
		if now.After(e.expiresAt) {
			c.removeElement(el)
			c.stats.Expirations++
		} else {
			c.lru.MoveToFront(el)
			c.stats.ExactHits++
			res := Result{
				Hit: true, Kind: HitExact, Similarity: 1,
				Key: key, Response: e.response, Age: now.Sub(e.storedAt),
			}
			c.mu.Unlock()
			return res, nil
		}
	}
	c.mu.Unlock()

	if c.embedder == nil {
		c.countMiss()
		return Result{}, nil
	}

	// Embedding happens outside the lock: it is the expensive part, and with a
	// real model it involves a network call.
	vec, err := c.embedder.Embed(ctx, embedText(req))
	if err != nil {
		c.countMiss()
		return Result{}, fmt.Errorf("cache: embed: %w", err)
	}

	params := paramsHash(req)

	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		best    *list.Element
		bestSim float64
		expired []*list.Element
	)
	for el := c.lru.Front(); el != nil; el = el.Next() {
		e := entryOf(el)
		if now.After(e.expiresAt) {
			expired = append(expired, el)
			continue
		}
		// Namespace and parameters must match exactly; only the prompt is
		// allowed to differ.
		if e.key.Namespace != key.Namespace || e.paramsHash != params {
			continue
		}
		// A different length means the entry was written by a different
		// embedder. Its vector is not comparable with this one, and comparing
		// them anyway would produce a number that looks like a similarity.
		if len(e.embedding) != len(vec) {
			continue
		}
		if sim := CosineSimilarity(vec, e.embedding); sim > bestSim {
			best, bestSim = el, sim
		}
	}
	for _, el := range expired {
		c.removeElement(el)
		c.stats.Expirations++
	}

	if best == nil || bestSim < c.threshold {
		if best != nil {
			c.stats.NearMisses++
		}
		c.stats.Misses++
		return Result{}, nil
	}

	e := entryOf(best)
	c.lru.MoveToFront(best)
	c.stats.SemanticHits++
	return Result{
		Hit: true, Kind: HitSemantic, Similarity: bestSim,
		Key: e.key, Response: e.response, Age: now.Sub(e.storedAt),
	}, nil
}

func (c *Cache) countMiss() {
	c.mu.Lock()
	c.stats.Misses++
	c.mu.Unlock()
}

// Put stores a response.
//
// Responses that were truncated by a token limit are not stored: FinishLength
// means the answer is incomplete, and an incomplete answer served from cache is
// a defect that outlives the request that caused it.
func (c *Cache) Put(ctx context.Context, req llm.Request, resp llm.Response) error {
	if resp.FinishReason == llm.FinishLength || resp.FinishReason == llm.FinishContentFilter {
		return nil
	}

	var vec []float32
	if c.embedder != nil {
		v, err := c.embedder.Embed(ctx, embedText(req))
		if err != nil {
			return fmt.Errorf("cache: embed: %w", err)
		}
		vec = v
	}

	key := KeyFor(req)
	now := c.now()
	e := &entry{
		key:        key,
		paramsHash: paramsHash(req),
		embedding:  vec,
		response:   resp,
		storedAt:   now,
		expiresAt:  now.Add(c.ttl),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value = e
		c.lru.MoveToFront(el)
		c.stats.Stores++
		return nil
	}
	c.entries[key] = c.lru.PushFront(e)
	c.stats.Stores++
	for c.lru.Len() > c.maxEntries {
		c.removeElement(c.lru.Back())
		c.stats.Evictions++
	}
	return nil
}

// Invalidate removes one entry, reporting whether it was there.
func (c *Cache) Invalidate(key Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return false
	}
	c.removeElement(el)
	return true
}

// InvalidateNamespace removes every entry for a model, returning the count.
// It is what an operator reaches for when a model is updated behind a stable
// name and yesterday's answers stop being right.
func (c *Cache) InvalidateNamespace(namespace string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var doomed []*list.Element
	for el := c.lru.Front(); el != nil; el = el.Next() {
		if entryOf(el).key.Namespace == namespace {
			doomed = append(doomed, el)
		}
	}
	for _, el := range doomed {
		c.removeElement(el)
	}
	return len(doomed)
}

// Purge empties the cache.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[Key]*list.Element{}
	c.lru.Init()
}

// Sweep removes expired entries and returns how many went.
//
// Expiry is otherwise lazy, which means an entry nobody looks for occupies a
// slot until it is evicted. A caller that cares about memory more than about
// doing nothing calls this on a timer.
func (c *Cache) Sweep() int {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var doomed []*list.Element
	for el := c.lru.Front(); el != nil; el = el.Next() {
		if now.After(entryOf(el).expiresAt) {
			doomed = append(doomed, el)
		}
	}
	for _, el := range doomed {
		c.removeElement(el)
		c.stats.Expirations++
	}
	return len(doomed)
}

// Len is the number of stored entries, including any that have expired but not
// yet been swept.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// Stats returns a snapshot of the counters.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = c.lru.Len()
	return s
}

// removeElement drops an element. The caller holds the lock.
func (c *Cache) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	delete(c.entries, entryOf(el).key)
	c.lru.Remove(el)
}
