package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Liona-orph/sluice/internal/audit"
	"github.com/Liona-orph/sluice/pkg/llm"
)

func records() []audit.Record {
	return []audit.Record{
		{
			KeyID: "team-a", RequestedModel: "fast", ServedModel: "gpt-4o-mini",
			Usage: llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			Cost:  750 * llm.Millidollar, // 0.15 + 0.60 per million
		},
		{
			KeyID: "team-b", RequestedModel: "fast", ServedModel: "gpt-4o-mini",
			Usage: llm.Usage{InputTokens: 1_000_000, OutputTokens: 0},
			Cost:  150 * llm.Millidollar,
			// A cache hit costs nothing now and saves what it would have cost.
			CacheHit: "exact",
		},
		{KeyID: "team-a", RequestedModel: "fast", ErrorCode: "provider_unavailable"},
	}
}

func TestReplayRePricesAgainstAnotherModel(t *testing.T) {
	rep := buildReport(records(), llm.DefaultPricing(), "gpt-4o", false)

	require.Len(t, rep.Groups, 1)
	g := rep.Groups[0]
	assert.Equal(t, "fast", g.Name)
	assert.Equal(t, 3, g.Requests)
	assert.Equal(t, 1, g.Served, "the cache hit and the error did not cost money")
	assert.Equal(t, 1, g.CacheHits)
	assert.Equal(t, 1, g.Errors)

	// gpt-4o is $2.50 in and $10.00 out per million: one million of each.
	assert.Equal(t, llm.Cost(12.5*float64(llm.Dollar)), g.ReplayCost)
	assert.Equal(t, 750*llm.Millidollar, g.RecordedCost)
	assert.Equal(t, llm.Cost(2.5*float64(llm.Dollar)), g.CacheSaved,
		"the hit is priced at what it would have cost upstream")
}

func TestReplayWithoutAnAsModelUsesWhatActuallyServed(t *testing.T) {
	rep := buildReport(records(), llm.DefaultPricing(), "", false)
	assert.Equal(t, rep.Groups[0].RecordedCost, rep.Groups[0].ReplayCost,
		"re-pricing the same model against the same table must reproduce the recorded cost")
}

func TestReplayGroupsByKey(t *testing.T) {
	rep := buildReport(records(), llm.DefaultPricing(), "", true)
	names := map[string]bool{}
	for _, g := range rep.Groups {
		names[g.Name] = true
	}
	assert.Equal(t, map[string]bool{"team-a": true, "team-b": true}, names)
	assert.Equal(t, "key", rep.GroupBy)
}

func TestReplayCountsUnpricedRatherThanCallingThemFree(t *testing.T) {
	// A zero in a cost column must never be mistaken for a free request.
	recs := []audit.Record{{
		RequestedModel: "fast", ServedModel: "a-model-nobody-priced",
		Usage: llm.Usage{InputTokens: 100, OutputTokens: 100},
	}}
	rep := buildReport(recs, llm.DefaultPricing(), "", false)
	assert.Equal(t, 1, rep.Total.Unpriced)

	var buf bytes.Buffer
	require.NoError(t, rep.write(&buf))
	assert.Contains(t, buf.String(), "not a free request")
}

func TestReplayFlagsEstimatedCounts(t *testing.T) {
	recs := []audit.Record{{
		RequestedModel: "fast", ServedModel: "gpt-4o-mini",
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 10, Estimated: true},
	}}
	rep := buildReport(recs, llm.DefaultPricing(), "", false)
	assert.Equal(t, 1, rep.Total.Estimated)

	var buf bytes.Buffer
	require.NoError(t, rep.write(&buf))
	assert.Contains(t, buf.String(), "0.68%", "the tokenizer's measured error is stated, not implied")
}

func TestReplayOutputIsATable(t *testing.T) {
	rep := buildReport(records(), llm.DefaultPricing(), "gpt-4o", false)
	var buf bytes.Buffer
	require.NoError(t, rep.write(&buf))
	out := buf.String()
	assert.Contains(t, out, "MODEL")
	assert.Contains(t, out, "TOTAL")
	assert.Contains(t, out, "re-priced as \"gpt-4o\"")
	assert.Contains(t, out, "does not predict how many tokens another model would have produced",
		"the one thing this tool cannot do has to be said in its own output")
}

func TestRunReplayEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	f, err := os.Create(path) //nolint:gosec // a temp file in a test
	require.NoError(t, err)
	w := audit.NewWriter(f, false)
	for _, r := range records() {
		require.NoError(t, w.Record(r))
	}
	require.NoError(t, w.Close())

	require.NoError(t, runReplay([]string{"--audit", path, "--as", "gpt-4o", "--json"}))
	require.Error(t, runReplay([]string{"--audit", path, "--as", "no-such-model"}))
	assert.Error(t, runReplay(nil), "--audit is required")
}

func TestVersionStringSaysSomethingTrue(t *testing.T) {
	v := versionString()
	assert.True(t, strings.HasPrefix(v, "sluice "))
	assert.Contains(t, v, "go1.")
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	require.ErrorContains(t, run([]string{"frobnicate"}), "unknown command")
	require.Error(t, run(nil))
	assert.NoError(t, run([]string{"version"}))
}

func TestServeCheckValidatesWithoutListening(t *testing.T) {
	// --check is what a deploy pipeline runs; it must not bind a port.
	assert.NoError(t, runServe([]string{"--check"}))
	assert.NoError(t, runServe([]string{"--print-config"}))

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("server:\n  addr: \"\"\n"), 0o600))
	assert.Error(t, runServe([]string{"--config", bad, "--check"}))
}
