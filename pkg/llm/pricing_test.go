package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostArithmeticIsExact(t *testing.T) {
	// The reason for integer nanodollars: a million small charges must sum to
	// the same value however they are grouped.
	price := ModelPrice{InputPerMillion: 3 * Dollar, OutputPerMillion: 15 * Dollar}
	var total Cost
	for i := 0; i < 1_000_000; i++ {
		total += price.Cost(Usage{InputTokens: 1, OutputTokens: 1})
	}
	// A million input and a million output tokens at $3 and $15 per million.
	assert.Equal(t, 18*Dollar, total)
	assert.Equal(t, "$18.000000", total.String())
}

func TestModelPriceCost(t *testing.T) {
	p := ModelPrice{
		InputPerMillion:       Cost(2.50 * float64(Dollar)),
		OutputPerMillion:      Cost(10.00 * float64(Dollar)),
		CachedInputPerMillion: Cost(1.25 * float64(Dollar)),
	}
	for _, tc := range []struct {
		name  string
		usage Usage
		want  Cost
	}{
		{"empty", Usage{}, 0},
		{"input only", Usage{InputTokens: 1_000_000}, Cost(2.5 * float64(Dollar))},
		{"output only", Usage{OutputTokens: 1_000_000}, 10 * Dollar},
		{
			"cached tokens are a subset of input, not an addition",
			Usage{InputTokens: 1_000_000, CachedInputTokens: 1_000_000},
			Cost(1.25 * float64(Dollar)),
		},
		{
			"half cached",
			Usage{InputTokens: 1_000_000, CachedInputTokens: 500_000},
			Cost(1.875 * float64(Dollar)),
		},
		{
			"a provider over-reporting cached tokens cannot produce a credit",
			Usage{InputTokens: 100, CachedInputTokens: 1_000_000},
			Cost(1.25 * float64(Dollar) * 100 / 1_000_000),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, p.Cost(tc.usage))
		})
	}
}

func TestCachedRateDefaultsToInputRate(t *testing.T) {
	p := ModelPrice{InputPerMillion: 4 * Dollar, OutputPerMillion: 8 * Dollar}
	withCache := p.Cost(Usage{InputTokens: 1_000_000, CachedInputTokens: 1_000_000})
	assert.Equal(t, 4*Dollar, withCache, "no discount configured means no discount applied")
}

func TestPricingPrefixFallback(t *testing.T) {
	p := DefaultPricing()

	exact, ok := p.Price("gpt-4o")
	require.True(t, ok)
	dated, ok := p.Price("gpt-4o-2024-11-20")
	require.True(t, ok)
	assert.Equal(t, exact, dated, "a dated snapshot inherits its base model's price")

	// Longest-prefix, not first-match: gpt-4o-mini must not be priced as gpt-4o.
	mini, ok := p.Price("gpt-4o-mini-2024-07-18")
	require.True(t, ok)
	assert.NotEqual(t, exact, mini)

	_, ok = p.Exact("gpt-4o-2024-11-20")
	assert.False(t, ok, "Exact must not fall back")
}

func TestPricingUnknownModelIsAnError(t *testing.T) {
	p := DefaultPricing()
	_, err := p.Cost("some-model-nobody-priced", Usage{InputTokens: 1000})
	assert.ErrorIs(t, err, ErrUnknownModel)
}

func TestPricingIsImmutable(t *testing.T) {
	base := DefaultPricing()
	extended := base.With("acme-1", ModelPrice{InputPerMillion: Dollar})

	_, ok := base.Exact("acme-1")
	assert.False(t, ok, "With must not mutate the receiver")
	_, ok = extended.Exact("acme-1")
	assert.True(t, ok)

	// Two calls to DefaultPricing must not share a map either.
	a, b := DefaultPricing(), DefaultPricing()
	a2 := a.With("acme-2", ModelPrice{})
	_, ok = b.Exact("acme-2")
	assert.False(t, ok)
	_, ok = a2.Exact("acme-2")
	assert.True(t, ok)
}

func TestCostString(t *testing.T) {
	for _, tc := range []struct {
		c    Cost
		want string
	}{
		{0, "$0.000000"},
		{Dollar, "$1.000000"},
		{Microdollar, "$0.000001"},
		{Nanodollar, "$0.000000"}, // below display resolution, deliberately
		{-2 * Dollar, "-$2.000000"},
		{Cost(1234) * Millidollar, "$1.234000"},
	} {
		assert.Equal(t, tc.want, tc.c.String())
	}
	assert.InDelta(t, 2.5, (Cost(2.5 * float64(Dollar))).Dollars(), 1e-9)
}

func TestDefaultPricingCoversLocalProvider(t *testing.T) {
	p := DefaultPricing()
	c, err := p.Cost("local-small", Usage{InputTokens: 1e6, OutputTokens: 1e6})
	require.NoError(t, err)
	assert.Equal(t, Cost(0), c, "offline runs must not fall into the unknown-model path")
	assert.NotEmpty(t, p.Models())
}
