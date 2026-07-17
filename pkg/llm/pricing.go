package llm

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Cost is an amount of money in nanodollars (1e-9 USD).
//
// Integer nanodollars rather than float64 dollars: a gateway sums millions of
// per-request costs into a monthly invoice, and float64 accumulation of values
// around 1e-6 loses cents at that scale. Nanodollars keep every arithmetic
// operation exact and still hold roughly 9.2e9 dollars in an int64, which is
// several orders of magnitude past any plausible LLM bill.
type Cost int64

// Cost units.
const (
	Nanodollar  Cost = 1
	Microdollar      = 1000 * Nanodollar
	Millidollar      = 1000 * Microdollar
	Dollar           = 1000 * Millidollar
)

// Dollars returns the cost as a float, for display and for callers that must
// hand it to something float-shaped. Do not accumulate the result.
func (c Cost) Dollars() float64 { return float64(c) / float64(Dollar) }

// String renders the cost with six decimal places, enough to show the price of
// a single short completion without scientific notation.
func (c Cost) String() string {
	neg := c < 0
	if neg {
		c = -c
	}
	whole := int64(c / Dollar)
	frac := int64(c % Dollar / 1000) // nanodollars -> microdollars
	s := fmt.Sprintf("$%d.%06d", whole, frac)
	if neg {
		return "-" + s
	}
	return s
}

// ModelPrice is the per-million-token price of one model.
type ModelPrice struct {
	InputPerMillion  Cost
	OutputPerMillion Cost
	// CachedInputPerMillion prices tokens the provider served from its own
	// prompt cache. Zero means "same as input", which is the correct default
	// for providers that offer no such discount.
	CachedInputPerMillion Cost
}

// PricingSnapshot names the date the built-in table was taken.
//
// The table is a snapshot of published list prices and nothing keeps it
// current. Vendors change prices, rename models and add tiers without warning,
// and there is no API to fetch this from. Treat the built-in table as a
// starting point: a deployment that cares about the accuracy of its cost
// reports should load prices from its own configuration at startup with
// NewPricing, and reconcile against the vendor invoice monthly. Sluice's job is
// to make cost computed rather than guessed; keeping the inputs true is an
// operational task it cannot do for you.
const PricingSnapshot = "2026-05"

// ErrUnknownModel is returned when a model has no price.
//
// It is an error rather than a zero cost on purpose. A model that silently
// costs nothing is worse than one that costs an unknown amount: it makes a cost
// dashboard confidently wrong, and the first anyone hears of it is the invoice.
var ErrUnknownModel = errors.New("llm: no price for model")

// Pricing is an immutable price table.
//
// Immutable so that it can be shared by every request goroutine without a lock
// and without the global mutable table that price lists usually become. With
// returns a modified copy; a running gateway swaps the whole table.
type Pricing struct {
	models map[string]ModelPrice
	// prefixes is models' keys sorted longest-first, so Price can resolve a
	// dated snapshot name without allocating on every lookup.
	prefixes []string
}

// NewPricing builds a table from an explicit map, copying it.
func NewPricing(models map[string]ModelPrice) *Pricing {
	p := &Pricing{models: make(map[string]ModelPrice, len(models))}
	for k, v := range models {
		p.models[k] = v
	}
	p.prefixes = make([]string, 0, len(p.models))
	for k := range p.models {
		p.prefixes = append(p.prefixes, k)
	}
	sort.Slice(p.prefixes, func(i, j int) bool {
		if len(p.prefixes[i]) != len(p.prefixes[j]) {
			return len(p.prefixes[i]) > len(p.prefixes[j])
		}
		return p.prefixes[i] < p.prefixes[j]
	})
	return p
}

// With returns a copy of p with model priced at price.
func (p *Pricing) With(model string, price ModelPrice) *Pricing {
	models := make(map[string]ModelPrice, len(p.models)+1)
	for k, v := range p.models {
		models[k] = v
	}
	models[model] = price
	return NewPricing(models)
}

// Models lists the priced model names, sorted.
func (p *Pricing) Models() []string {
	out := make([]string, 0, len(p.models))
	for k := range p.models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Price looks up a model, falling back to the longest table entry that is a
// prefix of the name.
//
// The fallback exists because vendors ship dated snapshots -- "gpt-4o" becomes
// "gpt-4o-2024-11-20" -- at the same price, and a table that had to list every
// snapshot would be stale within a week. The risk it accepts is real: a future
// model named as an extension of an existing one ("gpt-4o-ultra") would inherit
// the wrong price silently. Prefix matching is longest-first to limit the blast
// radius, and Exact is available where a caller would rather be told nothing
// than told something plausible.
func (p *Pricing) Price(model string) (ModelPrice, bool) {
	if mp, ok := p.models[model]; ok {
		return mp, true
	}
	for _, prefix := range p.prefixes {
		if strings.HasPrefix(model, prefix) {
			return p.models[prefix], true
		}
	}
	return ModelPrice{}, false
}

// Exact looks up a model without prefix fallback.
func (p *Pricing) Exact(model string) (ModelPrice, bool) {
	mp, ok := p.models[model]
	return mp, ok
}

// Cost computes what a usage record costs on a model.
func (p *Pricing) Cost(model string, u Usage) (Cost, error) {
	mp, ok := p.Price(model)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	return mp.Cost(u), nil
}

// Cost computes the price of a usage record.
//
// Cached input tokens are a subset of InputTokens, so they are billed at the
// cached rate and subtracted from the full-price count rather than added.
func (mp ModelPrice) Cost(u Usage) Cost {
	cachedRate := mp.CachedInputPerMillion
	if cachedRate == 0 {
		cachedRate = mp.InputPerMillion
	}
	cached := u.CachedInputTokens
	if cached > u.InputTokens {
		// A provider reporting more cached than total input tokens is
		// nonsensical; clamping keeps a bad upstream from producing a negative
		// invoice line.
		cached = u.InputTokens
	}
	full := u.InputTokens - cached
	return perMillion(mp.InputPerMillion, full) +
		perMillion(cachedRate, cached) +
		perMillion(mp.OutputPerMillion, u.OutputTokens)
}

// perMillion multiplies before dividing so that the division is the only place
// precision is lost, and that loss is bounded by one nanodollar.
func perMillion(rate Cost, tokens int) Cost {
	if tokens <= 0 || rate == 0 {
		return 0
	}
	return Cost(int64(rate) * int64(tokens) / 1_000_000)
}

// DefaultPricing returns the built-in snapshot of published list prices.
//
// Prices are USD per million tokens as of PricingSnapshot. Every call builds a
// fresh table, so a caller may hold onto and modify one without affecting
// anyone else.
func DefaultPricing() *Pricing {
	usd := func(perMillionDollars float64) Cost {
		return Cost(perMillionDollars * float64(Dollar))
	}
	return NewPricing(map[string]ModelPrice{
		// OpenAI
		"gpt-4o":        {InputPerMillion: usd(2.50), OutputPerMillion: usd(10.00), CachedInputPerMillion: usd(1.25)},
		"gpt-4o-mini":   {InputPerMillion: usd(0.15), OutputPerMillion: usd(0.60), CachedInputPerMillion: usd(0.075)},
		"gpt-4-turbo":   {InputPerMillion: usd(10.00), OutputPerMillion: usd(30.00)},
		"gpt-3.5-turbo": {InputPerMillion: usd(0.50), OutputPerMillion: usd(1.50)},
		"o1":            {InputPerMillion: usd(15.00), OutputPerMillion: usd(60.00), CachedInputPerMillion: usd(7.50)},
		"o1-mini":       {InputPerMillion: usd(3.00), OutputPerMillion: usd(12.00), CachedInputPerMillion: usd(1.50)},

		// Anthropic
		"claude-3-5-sonnet": {InputPerMillion: usd(3.00), OutputPerMillion: usd(15.00), CachedInputPerMillion: usd(0.30)},
		"claude-3-5-haiku":  {InputPerMillion: usd(0.80), OutputPerMillion: usd(4.00), CachedInputPerMillion: usd(0.08)},
		"claude-3-opus":     {InputPerMillion: usd(15.00), OutputPerMillion: usd(75.00), CachedInputPerMillion: usd(1.50)},
		"claude-3-haiku":    {InputPerMillion: usd(0.25), OutputPerMillion: usd(1.25)},

		// Google
		"gemini-1.5-pro":   {InputPerMillion: usd(1.25), OutputPerMillion: usd(5.00)},
		"gemini-1.5-flash": {InputPerMillion: usd(0.075), OutputPerMillion: usd(0.30)},

		// Meta, as served by the common hosted inference providers.
		"llama-3.1-70b": {InputPerMillion: usd(0.35), OutputPerMillion: usd(0.40)},
		"llama-3.1-8b":  {InputPerMillion: usd(0.05), OutputPerMillion: usd(0.08)},

		// The local provider is free, and saying so explicitly keeps offline
		// runs out of the ErrUnknownModel path.
		"local-small": {},
		"local-large": {},
	})
}
