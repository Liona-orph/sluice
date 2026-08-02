package telemetry

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// TestNoUnboundedLabelCardinality is the enforcement of the rule the package
// comment states. It gathers the live registry rather than reading the source,
// so a metric added in a hurry with a key_id label fails the build.
//
// It checks three things: that every label name is on the allow list, that
// nothing on the forbidden list appears under any spelling, and that the total
// number of series stays small for a workload that exercised every metric --
// because a label can be allowed and still be misused, and a series count is
// the only signal that catches that.
func TestNoUnboundedLabelCardinality(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)

	exercise(m)

	families, err := reg.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, families)

	series := 0
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			series++
			for _, pair := range metric.GetLabel() {
				name := pair.GetName()
				if reason, forbidden := ForbiddenLabels[name]; forbidden {
					t.Errorf("metric %s carries the forbidden label %q: %s", f.GetName(), name, reason)
				}
				assert.Containsf(t, AllowedLabels, name,
					"metric %s carries the label %q, which is not on the allow list. "+
						"If it is genuinely bounded by a config file or a closed enumeration, add it to "+
						"AllowedLabels with the reason; if it comes from a request, it must not be a label.",
					f.GetName(), name)
			}
		}
	}
	// The exercise below covers every metric with a handful of providers,
	// models and routes. A bound well above that and well below anything
	// unbounded catches a label that is quietly per-request.
	assert.Lessf(t, series, 200, "%d series after a small workload; something is labelled by a request", series)
}

// exercise touches every metric with the kind of values a real gateway would
// produce, so that Gather returns a populated registry.
func exercise(m *Metrics) {
	for _, route := range []string{"fast", "smart"} {
		for _, outcome := range []string{"ok", "rate_limit_error", "cache_exact"} {
			m.Requests.WithLabelValues(route, outcome).Inc()
			m.RequestDuration.WithLabelValues(route, outcome).Observe(0.42)
		}
		m.TimeToFirstByte.WithLabelValues(route).Observe(0.1)
		m.Failovers.WithLabelValues(route).Inc()
	}
	for _, provider := range []string{"openai", "anthropic"} {
		for _, model := range []string{"gpt-4o", "claude-3-5-sonnet"} {
			m.UpstreamAttempts.WithLabelValues(provider, model, "ok").Inc()
			m.UpstreamAttempts.WithLabelValues(provider, model, string(llm.CodeRateLimited)).Inc()
			m.UpstreamDuration.WithLabelValues(provider, model).Observe(1.5)
			m.ShortCircuits.WithLabelValues(provider, model).Inc()
			m.SetBreakerState(provider, model, "closed")
			m.ObserveUsage(provider, model, llm.Usage{InputTokens: 100, OutputTokens: 50}, 1234)
		}
	}
	for _, result := range []string{"exact", "semantic", "miss", "bypass", "error"} {
		m.CacheLookups.WithLabelValues(result).Inc()
	}
	m.CacheEntries.Set(17)
	for _, entity := range []string{"email", "phone", "credit_card", "api_key", "person_name"} {
		m.Redactions.WithLabelValues(entity).Inc()
	}
	for _, kind := range []string{"requests", "tokens"} {
		m.RateLimited.WithLabelValues(kind).Inc()
	}
	for _, action := range []string{"allow", "degrade", "reject"} {
		m.BudgetDecisions.WithLabelValues(action).Inc()
	}
	for _, stage := range []string{"authenticate", "rate_limit", "budget", "redact"} {
		m.StageDuration.WithLabelValues(stage).Observe(0.0001)
	}
	m.AuditWritten.Inc()
	m.AuditDropped.Add(0)
}

func TestAllowedAndForbiddenLabelsDoNotOverlap(t *testing.T) {
	for name := range ForbiddenLabels {
		assert.NotContains(t, AllowedLabels, name, "%q cannot be both allowed and forbidden", name)
	}
}

func TestLatencyBucketsCoverALanguageModelCall(t *testing.T) {
	// The Prometheus defaults stop at 10 seconds, which puts every long
	// completion in +Inf and makes p99 unanswerable. This pins the property
	// rather than the exact list.
	assert.GreaterOrEqual(t, latencyBuckets[len(latencyBuckets)-1], 120.0,
		"the top bucket must cover a slow completion")
	assert.LessOrEqual(t, latencyBuckets[0], 0.001,
		"the bottom bucket must resolve a cache hit")
	for i := 1; i < len(latencyBuckets); i++ {
		assert.Greater(t, latencyBuckets[i], latencyBuckets[i-1], "buckets must be strictly increasing")
	}
}

func TestBreakerStateIsOneHot(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	m.SetBreakerState("p", "m", "open")

	families, err := reg.Gather()
	require.NoError(t, err)
	sum := 0.0
	found := 0
	for _, f := range families {
		if f.GetName() != "sluice_breaker_state" {
			continue
		}
		for _, metric := range f.GetMetric() {
			found++
			sum += metric.GetGauge().GetValue()
		}
	}
	assert.Equal(t, 3, found, "one series per state")
	assert.InDelta(t, 1.0, sum, 0, "exactly one is set")
}

func TestRegisteringTwiceOnOneRegistryFails(t *testing.T) {
	reg := prometheus.NewRegistry()
	_, err := NewMetrics(reg)
	require.NoError(t, err)
	_, err = NewMetrics(reg)
	assert.Error(t, err, "a duplicate registration is a bug, and returning an error beats the library's panic")
}

func TestCostIsCountedInIntegerNanodollars(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m, err := NewMetrics(reg)
	require.NoError(t, err)
	// A million requests at a thousandth of a cent each. Accumulated as dollars
	// in a float64 this would lose the low digits; as nanodollars it does not.
	for i := 0; i < 1000; i++ {
		m.ObserveUsage("p", "m", llm.Usage{}, 10*llm.Microdollar)
	}
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "sluice_cost_nanodollars_total" {
			continue
		}
		require.Len(t, f.GetMetric(), 1)
		assert.InDelta(t, float64(1000*10*llm.Microdollar), f.GetMetric()[0].GetCounter().GetValue(), 0)
	}
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, "info", "json")
	require.NoError(t, err)
	log.Info("hello", "key_id", "abc")
	var out map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	assert.Equal(t, "hello", out["msg"])
	assert.Equal(t, "abc", out["key_id"], "identifiers that must not be metric labels still belong in the log")

	buf.Reset()
	log, err = NewLogger(&buf, "warn", "text")
	require.NoError(t, err)
	log.Info("suppressed")
	log.Warn("kept")
	assert.NotContains(t, buf.String(), "suppressed")
	assert.Contains(t, buf.String(), "kept")

	_, err = NewLogger(&buf, "chatty", "json")
	require.Error(t, err)
	_, err = NewLogger(&buf, "info", "xml")
	assert.Error(t, err)
}
