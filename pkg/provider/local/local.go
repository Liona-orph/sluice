// Package local implements llm.Provider without a network or an API key.
//
// It is not a mock. It generates real text from a seeded PRNG and a corpus,
// streams it in realistic chunks, simulates latency, counts tokens with the
// same tokenizer the gateway uses, and fails in each of the taxonomy's error
// modes on request. Everything downstream -- routing, retry, failover, caching,
// redaction, cost accounting -- can therefore be developed, tested and
// benchmarked end to end with nothing installed, which is the property that
// makes the rest of the repository testable at all.
//
// Determinism comes from the request, not from call order: the PRNG is seeded
// with a hash of the request fingerprint and the configured seed, so the same
// request produces the same response no matter how many other requests ran
// first or on how many goroutines. A provider whose output depended on a shared
// RNG would be reproducible only single-threaded, which is exactly when it
// would stop being useful.
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"iter"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Liona-orph/sluice/pkg/llm"
)

// Latency describes the timing the provider imitates.
//
// Zero disables the wait entirely, which is what unit tests want; the benchmark
// suite and any demonstration of streaming want it set.
type Latency struct {
	// TimeToFirstToken is the delay before the first chunk.
	TimeToFirstToken time.Duration
	// PerToken is added per generated token, spread across the chunks.
	PerToken time.Duration
	// Jitter is the fraction of the total by which a request's latency varies,
	// drawn deterministically from the request's own seed. 0.2 means +/-20%.
	Jitter float64
}

// Failure configures injected errors.
//
// Rate is a probability rather than a switch because the interesting failures
// are intermittent ones: a retry loop that works against a provider failing
// 100% of the time is not evidence that it works at all.
type Failure struct {
	// Code is the error to inject. Empty means no injection.
	Code llm.ErrorCode
	// Rate is the probability in [0,1] that a given request fails, evaluated
	// against the request's own deterministic seed. The same request therefore
	// always fails or always succeeds, which is what makes a failing test
	// reproducible.
	Rate float64
	// RetryAfter is reported on the injected error, for rate limits.
	RetryAfter time.Duration
	// Message overrides the generated description.
	Message string
	// AfterChunks, when positive, delays a streaming failure until this many
	// chunks have been delivered. It exists to exercise the case Stream's
	// signature is built around: a failure after output has already reached the
	// client, which cannot be retried transparently.
	AfterChunks int
}

// Config configures a Provider. The zero value is valid and yields a fast,
// deterministic, always-succeeding provider.
type Config struct {
	// Name is reported as Response.Provider. Defaults to "local".
	Name string
	// Seed shifts every request's derived seed, so two Providers configured
	// differently give different answers to the same question. That is how a
	// failover test tells which upstream actually served it.
	Seed int64
	// Models restricts the accepted model names. Empty accepts any name, which
	// keeps the provider usable as a stand-in for a model it has never heard of.
	Models []string
	// ContextTokens is the window size. Requests larger than it fail with
	// llm.CodeContextLengthExceeded, which is a real behaviour worth testing
	// against rather than an injected one. Zero means unbounded.
	ContextTokens int
	// MaxOutputTokens caps generation when a request does not.
	MaxOutputTokens int
	// Corpus replaces the built-in sentence pool.
	Corpus []string
	// EchoPrompt makes the response quote the last user message.
	//
	// On by default (see New) because it is what lets a redaction test observe
	// a placeholder surviving a round trip through a model: the provider writes
	// new text around a value it does not understand, which is precisely what a
	// real model does to a tokenised prompt.
	EchoPrompt *bool
	// MaxChunkWords bounds the size of a streamed chunk. Defaults to 4.
	MaxChunkWords int
	Latency       Latency
	Failure       Failure
	// Now is the clock, injectable so that Response.Created is assertable.
	Now func() time.Time
}

// Provider is a deterministic llm.Provider. It is immutable after New and safe
// for concurrent use.
type Provider struct {
	name          string
	seed          int64
	models        map[string]struct{}
	contextTokens int
	maxOutput     int
	corpus        []string
	echo          bool
	maxChunkWords int
	latency       Latency
	failure       Failure
	now           func() time.Time
	tok           llm.Approx
}

// New validates cfg and returns a Provider.
func New(cfg Config) (*Provider, error) {
	if cfg.Failure.Code != "" && cfg.Failure.Rate <= 0 {
		// Silently never failing is the worst outcome for a test that meant to
		// inject an error, so it is rejected rather than defaulted.
		return nil, fmt.Errorf("local: failure code %q configured with rate %v; set Rate to a positive probability",
			cfg.Failure.Code, cfg.Failure.Rate)
	}
	if cfg.Failure.Rate > 1 {
		return nil, fmt.Errorf("local: failure rate %v exceeds 1", cfg.Failure.Rate)
	}
	if cfg.Latency.Jitter < 0 || cfg.Latency.Jitter > 1 {
		return nil, fmt.Errorf("local: latency jitter %v is not a fraction in [0,1]", cfg.Latency.Jitter)
	}

	p := &Provider{
		name:          cfg.Name,
		seed:          cfg.Seed,
		contextTokens: cfg.ContextTokens,
		maxOutput:     cfg.MaxOutputTokens,
		corpus:        cfg.Corpus,
		echo:          cfg.EchoPrompt == nil || *cfg.EchoPrompt,
		maxChunkWords: cfg.MaxChunkWords,
		latency:       cfg.Latency,
		failure:       cfg.Failure,
		now:           cfg.Now,
	}
	if p.name == "" {
		p.name = "local"
	}
	if len(p.corpus) == 0 {
		p.corpus = defaultCorpus
	} else {
		p.corpus = append([]string(nil), p.corpus...)
	}
	if p.maxChunkWords <= 0 {
		p.maxChunkWords = 4
	}
	if p.maxOutput <= 0 {
		p.maxOutput = 400
	}
	if p.now == nil {
		p.now = time.Now
	}
	if len(cfg.Models) > 0 {
		p.models = make(map[string]struct{}, len(cfg.Models))
		for _, m := range cfg.Models {
			p.models[m] = struct{}{}
		}
	}
	return p, nil
}

// Name implements llm.Provider.
func (p *Provider) Name() string { return p.name }

// Complete implements llm.Provider.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	start := p.now()
	gen, err := p.plan(req)
	if err != nil {
		return llm.Response{}, err
	}
	if err := sleep(ctx, gen.totalLatency, p.name, req.Model); err != nil {
		return llm.Response{}, err
	}
	// AfterChunks is meaningless without chunks, so a failure configured to
	// arrive mid-stream simply fails the whole call here. That matches what a
	// non-streaming client sees from a provider that drops a connection late.
	if gen.failure != nil {
		return llm.Response{}, gen.failure.err
	}

	resp := llm.Response{
		ID:           gen.id,
		Model:        req.Model,
		Provider:     p.name,
		Message:      gen.message(),
		FinishReason: gen.finishReason,
		Usage:        gen.usage,
		Created:      start,
		Latency:      p.now().Sub(start),
	}
	return resp, nil
}

// Stream implements llm.Provider.
func (p *Provider) Stream(ctx context.Context, req llm.Request) (iter.Seq2[llm.Chunk, error], error) {
	gen, err := p.plan(req)
	if err != nil {
		return nil, err
	}
	if gen.failure != nil && gen.failure.AfterChunks <= 0 {
		// Nothing has been emitted, so this is a pre-stream failure and belongs
		// in the outer error where a caller can still fail over.
		return nil, gen.failure.err
	}

	return func(yield func(llm.Chunk, error) bool) {
		if err := sleep(ctx, gen.firstTokenLatency, p.name, req.Model); err != nil {
			yield(llm.Chunk{}, err)
			return
		}

		chunks := gen.chunks(p.maxChunkWords)
		perChunk := time.Duration(0)
		if n := len(chunks); n > 0 {
			perChunk = gen.streamLatency / time.Duration(n)
		}

		for i, text := range chunks {
			if i > 0 {
				if err := sleep(ctx, perChunk, p.name, req.Model); err != nil {
					yield(llm.Chunk{}, err)
					return
				}
			}
			if gen.failure != nil && i == gen.failure.AfterChunks {
				yield(llm.Chunk{}, gen.failure.err)
				return
			}
			c := llm.Chunk{ID: gen.id, Model: req.Model, Provider: p.name,
				Delta: llm.Delta{Content: text}}
			if i == 0 {
				c.Delta.Role = llm.RoleAssistant
			}
			if !yield(c, nil) {
				return
			}
		}

		// Tool calls arrive after the text, with arguments split across two
		// fragments so that consumers which assume a whole JSON document per
		// chunk break here rather than in production.
		for i, tc := range gen.toolCalls {
			args := string(tc.Arguments)
			half := len(args) / 2
			opening := llm.Chunk{ID: gen.id, Model: req.Model, Provider: p.name,
				Delta: llm.Delta{ToolCalls: []llm.ToolCallDelta{
					{Index: i, ID: tc.ID, Name: tc.Name, ArgumentsDelta: args[:half]},
				}}}
			if !yield(opening, nil) {
				return
			}
			rest := llm.Chunk{ID: gen.id, Model: req.Model, Provider: p.name,
				Delta: llm.Delta{ToolCalls: []llm.ToolCallDelta{
					{Index: i, ArgumentsDelta: args[half:]},
				}}}
			if !yield(rest, nil) {
				return
			}
		}

		usage := gen.usage
		yield(llm.Chunk{
			ID: gen.id, Model: req.Model, Provider: p.name,
			FinishReason: gen.finishReason,
			Usage:        &usage,
		}, nil)
	}, nil
}

// generation is everything decided about a response before any of it is
// delivered. Complete and Stream share it so that the two paths cannot drift:
// the same request must produce the same text whichever way it is asked for.
type generation struct {
	id                string
	text              string
	toolCalls         []llm.ToolCall
	finishReason      llm.FinishReason
	usage             llm.Usage
	firstTokenLatency time.Duration
	streamLatency     time.Duration
	totalLatency      time.Duration
	failure           *injectedFailure
}

type injectedFailure struct {
	err         error
	AfterChunks int
}

func (g *generation) message() llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: g.text, ToolCalls: g.toolCalls}
}

// chunks splits the generated text on word boundaries into groups of varying
// size, keeping the trailing space with the preceding chunk so that
// concatenation reproduces the text exactly.
func (g *generation) chunks(maxWords int) []string {
	if g.text == "" {
		return nil
	}
	fields := splitKeepingSpaces(g.text)
	// The group sizes are derived from the id so that they, too, are a
	// deterministic function of the request.
	// math/rand, not crypto/rand: determinism is the entire point, and this
	// generator decides chunk sizes in a test fixture, never anything a
	// security property depends on.
	r := rand.New(rand.NewPCG(hash64(g.id), 0x5EED)) //nolint:gosec // deterministic by design
	var out []string
	for i := 0; i < len(fields); {
		n := 1 + r.IntN(maxWords)
		if i+n > len(fields) {
			n = len(fields) - i
		}
		out = append(out, strings.Join(fields[i:i+n], ""))
		i += n
	}
	return out
}

// splitKeepingSpaces splits into words that carry their trailing whitespace.
func splitKeepingSpaces(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			continue
		}
		for i+1 < len(s) && s[i+1] == ' ' {
			i++
		}
		out = append(out, s[start:i+1])
		start = i + 1
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// plan does all the deciding: validation, failure injection, text generation,
// token accounting and latency. It performs no I/O and does not sleep, so both
// entry points can call it before committing to a delivery strategy.
func (p *Provider) plan(req llm.Request) (*generation, error) {
	if err := p.validate(req); err != nil {
		return nil, err
	}

	seed := hash64(req.Fingerprint()) ^ uint64(p.seed) //nolint:gosec // a seed, where wraparound is harmless
	r := rand.New(rand.NewPCG(seed, seed>>32|1))       //nolint:gosec // deterministic by design; see above

	g := &generation{
		id:           fmt.Sprintf("%s-%016x", p.name, seed),
		finishReason: llm.FinishStop,
	}

	inputTokens := p.tok.CountRequest(req)
	if p.contextTokens > 0 && inputTokens > p.contextTokens {
		return nil, &llm.Error{
			Code: llm.CodeContextLengthExceeded, Provider: p.name, Model: req.Model,
			Message: fmt.Sprintf("request is %d tokens, context window is %d", inputTokens, p.contextTokens),
		}
	}

	if p.failure.Code != "" && r.Float64() < p.failure.Rate {
		g.failure = &injectedFailure{err: p.injectedError(req), AfterChunks: p.failure.AfterChunks}
	}

	g.text = p.generate(r, req)
	g.toolCalls = p.generateToolCalls(r, req)
	if len(g.toolCalls) > 0 {
		g.finishReason = llm.FinishToolCalls
	}

	// Truncation is applied on a token budget, not a character count, so that a
	// MaxTokens test measures the thing it names.
	budget := req.MaxTokens
	if budget <= 0 || budget > p.maxOutput {
		budget = p.maxOutput
	}
	if truncated, cut := truncateToTokens(p.tok, g.text, budget); cut {
		g.text = truncated
		g.finishReason = llm.FinishLength
		g.toolCalls = nil
	}

	outputTokens := p.tok.CountTokens(g.text)
	for _, tc := range g.toolCalls {
		outputTokens += p.tok.CountTokens(tc.Name) + p.tok.CountTokens(string(tc.Arguments))
	}
	// Estimated is false: this provider generated the text with this tokenizer,
	// so for it the count is not an estimate of anything.
	g.usage = llm.Usage{InputTokens: inputTokens, OutputTokens: outputTokens}

	jitter := 1.0
	if p.latency.Jitter > 0 {
		jitter = 1 + p.latency.Jitter*(2*r.Float64()-1)
	}
	g.firstTokenLatency = scale(p.latency.TimeToFirstToken, jitter)
	g.streamLatency = scale(time.Duration(outputTokens)*p.latency.PerToken, jitter)
	g.totalLatency = g.firstTokenLatency + g.streamLatency
	return g, nil
}

func (p *Provider) validate(req llm.Request) error {
	if len(req.Messages) == 0 {
		return llm.Errorf(llm.CodeInvalidRequest, p.name, req.Model, "request has no messages")
	}
	for i, m := range req.Messages {
		if !m.Role.Valid() {
			return llm.Errorf(llm.CodeInvalidRequest, p.name, req.Model, "message %d has role %q", i, m.Role)
		}
	}
	if p.models != nil {
		if _, ok := p.models[req.Model]; !ok {
			return llm.Errorf(llm.CodeInvalidRequest, p.name, req.Model, "unknown model %q", req.Model)
		}
	}
	return nil
}

func (p *Provider) injectedError(req llm.Request) error {
	msg := p.failure.Message
	if msg == "" {
		msg = "injected " + string(p.failure.Code)
	}
	return &llm.Error{
		Code:       p.failure.Code,
		Provider:   p.name,
		Model:      req.Model,
		Message:    msg,
		RetryAfter: p.failure.RetryAfter,
	}
}

// generate assembles the response text.
func (p *Provider) generate(r *rand.Rand, req llm.Request) string {
	var b strings.Builder
	if p.echo {
		if q := lastUserContent(req); q != "" {
			b.WriteString("Regarding ")
			b.WriteString(strconv.Quote(truncateRunes(q, 120)))
			b.WriteString(": ")
		}
	}
	n := 1 + r.IntN(3)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p.corpus[r.IntN(len(p.corpus))])
	}
	return b.String()
}

// generateToolCalls emits a call when the request declares tools and either
// demands one or the PRNG chooses one.
func (p *Provider) generateToolCalls(r *rand.Rand, req llm.Request) []llm.ToolCall {
	if len(req.Tools) == 0 || req.ToolChoice == llm.ToolChoiceNone {
		return nil
	}
	if req.ToolChoice != llm.ToolChoiceRequired && r.Float64() < 0.5 {
		return nil
	}
	tool := req.Tools[r.IntN(len(req.Tools))]
	return []llm.ToolCall{{
		ID:        fmt.Sprintf("call_%s_%d", tool.Name, r.IntN(1000)),
		Name:      tool.Name,
		Arguments: argumentsFor(r, tool),
	}}
}

// argumentsFor fills the tool's top-level schema properties with plausible
// values. It reads only "properties" and "type"; anything richer is the
// caller's business and a real model would get it wrong too.
func argumentsFor(r *rand.Rand, tool llm.Tool) json.RawMessage {
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if len(tool.Parameters) == 0 || json.Unmarshal(tool.Parameters, &schema) != nil {
		return json.RawMessage(`{}`)
	}
	names := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		names = append(names, k)
	}
	sort.Strings(names) // map order must not leak into a deterministic provider
	out := make(map[string]any, len(names))
	for _, k := range names {
		switch schema.Properties[k].Type {
		case "number", "integer":
			out[k] = r.IntN(100)
		case "boolean":
			out[k] = r.IntN(2) == 1
		default:
			out[k] = fillerWords[r.IntN(len(fillerWords))]
		}
	}
	// json.Marshal sorts map keys, so the encoding is stable.
	b, err := json.Marshal(out)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func lastUserContent(req llm.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == llm.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

// truncateToTokens cuts text at a word boundary so that it fits the budget.
func truncateToTokens(tok llm.Approx, text string, budget int) (string, bool) {
	if tok.CountTokens(text) <= budget {
		return text, false
	}
	words := splitKeepingSpaces(text)
	var b strings.Builder
	for _, w := range words {
		if tok.CountTokens(b.String()+w) > budget {
			break
		}
		b.WriteString(w)
	}
	return strings.TrimRight(b.String(), " "), true
}

func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

func scale(d time.Duration, factor float64) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(float64(d) * factor)
}

// sleep waits, honouring cancellation. A cancelled context becomes a typed
// timeout so that the retry policy sees a classification rather than a bare
// context error.
func sleep(ctx context.Context, d time.Duration, provider, model string) error {
	if err := ctx.Err(); err != nil {
		return contextError(err, provider, model)
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return contextError(ctx.Err(), provider, model)
	}
}

func contextError(err error, provider, model string) error {
	code := llm.CodeTimeout
	if errors.Is(err, context.Canceled) {
		// A cancellation is the caller's own decision, not a provider failure;
		// classifying it as retryable would have the retry loop fight the
		// caller who just gave up.
		code = llm.CodeUnknown
	}
	return &llm.Error{Code: code, Provider: provider, Model: model, Message: err.Error(), Err: err}
}

func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

var _ llm.Provider = (*Provider)(nil)
