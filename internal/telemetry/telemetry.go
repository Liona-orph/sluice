// Package telemetry is the gateway's logging and metrics.
//
// The organising constraint is label cardinality. A Prometheus time series
// exists for every distinct combination of label values, and it lives in the
// server's memory until someone deletes it. Labelling by API key looks harmless
// with ten keys and takes the monitoring stack down at ten thousand; labelling
// by prompt, user ID or request ID does it immediately. There is no runtime
// guard that can save you, because by the time the cardinality is visible the
// damage is a heap full of series.
//
// So the rule here is structural: every label value comes from a config file an
// operator wrote -- provider name, model name, route alias -- or from a closed
// enumeration in the source -- error code, cache result, breaker state. Nothing
// derived from a request may be a label. AllowedLabels states that rule as
// data, and TestNoUnboundedLabelCardinality gathers the live registry and fails
// if a metric carries a label outside it. That test is the enforcement; the
// comment is just the reason.
//
// The identifiers that cannot be labels still belong in the record of what
// happened. They go to the structured log and to the audit record, both of
// which are append-only streams that cost disk rather than resident memory.
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// AllowedLabels is every label name any Sluice metric may use.
//
// Each is bounded by something that is not a request:
//
//	route        route aliases, from the config file
//	provider     provider names, from the config file
//	model        physical model names, from the config file
//	code         llm.ErrorCode, a closed enumeration
//	result       cache lookup outcome, a closed enumeration
//	kind         which rate limit, a closed enumeration
//	action       budget decision, a closed enumeration
//	entity_type  redact.EntityType, a closed enumeration
//	stage        pipeline stage name, a closed enumeration
//	state        circuit breaker state, a closed enumeration
//	outcome      request outcome class, a closed enumeration
var AllowedLabels = map[string]string{
	"route":       "route aliases from the config file",
	"provider":    "provider names from the config file",
	"model":       "physical model names from the config file",
	"code":        "llm.ErrorCode, a closed enumeration",
	"result":      "cache lookup outcome, a closed enumeration",
	"kind":        "rate limit kind, a closed enumeration",
	"action":      "budget decision, a closed enumeration",
	"entity_type": "redact.EntityType, a closed enumeration",
	"stage":       "pipeline stage, a closed enumeration",
	"state":       "circuit breaker state, a closed enumeration",
	"outcome":     "request outcome class, a closed enumeration",
}

// ForbiddenLabels names things that must never become labels, with the reason.
// It exists so that the test can fail with an explanation rather than a diff.
var ForbiddenLabels = map[string]string{
	"key":        "one series per API key; unbounded as soon as keys are provisioned by an API",
	"key_id":     "same as key",
	"api_key":    "same as key, and it is a credential",
	"team":       "bounded today, unbounded the moment teams are self-service",
	"user":       "unbounded by definition",
	"user_id":    "unbounded by definition",
	"prompt":     "unbounded and sensitive",
	"request_id": "one series per request; the worst case",
	"id":         "almost certainly a request or key identifier",
	"path":       "unbounded on any server that 404s",
	"subject":    "a key or team identifier by another name",
}

// latencyBuckets are the histogram boundaries for anything measuring a model
// call.
//
// The Prometheus defaults stop at 10 seconds, which puts every long completion
// in +Inf and makes a p99 unanswerable -- and the p99 of a language model call
// is the number anyone actually asks about. These run to two minutes, densely
// below one second where a cache hit or a rejection lands, and coarsely above
// ten where the only question is "how bad".
var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2, 3, 5, 8, 13, 21, 34, 60, 120,
}

// tokenBuckets size a single request's token count. Powers of four from 16 to
// 262144: a request is either a one-line question, a paragraph, a document, or
// a whole context window, and those are four orders of magnitude apart.
var tokenBuckets = []float64{16, 64, 256, 1024, 4096, 16384, 65536, 262144}

// Metrics is the gateway's metric set, bound to one registry.
type Metrics struct {
	Requests        *prometheus.CounterVec
	RequestDuration *prometheus.HistogramVec
	TimeToFirstByte *prometheus.HistogramVec

	UpstreamAttempts *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec
	Failovers        *prometheus.CounterVec
	ShortCircuits    *prometheus.CounterVec
	BreakerState     *prometheus.GaugeVec

	Tokens        *prometheus.CounterVec
	RequestTokens *prometheus.HistogramVec
	// CostNanodollars is a counter in nanodollars rather than a float in
	// dollars. Prometheus counters are float64, and summing a million values of
	// around 1e-6 dollars loses the low digits; counting integers and dividing
	// once at query time does not.
	CostNanodollars *prometheus.CounterVec

	CacheLookups *prometheus.CounterVec
	CacheEntries prometheus.Gauge

	Redactions *prometheus.CounterVec

	RateLimited     *prometheus.CounterVec
	BudgetDecisions *prometheus.CounterVec

	StageDuration *prometheus.HistogramVec

	AuditWritten prometheus.Counter
	AuditDropped prometheus.Counter
}

// NewMetrics registers the metric set on reg.
//
// It takes a registry rather than using the default one so that a test can
// build an isolated set, and so that the process has no package-level mutable
// state. prometheus.MustRegister on the default registry would be both.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_requests_total",
			Help: "Completion requests by route and outcome class.",
		}, []string{"route", "outcome"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sluice_request_duration_seconds",
			Help:    "End-to-end request latency, gateway ingress to last byte.",
			Buckets: latencyBuckets,
		}, []string{"route", "outcome"}),
		TimeToFirstByte: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sluice_time_to_first_byte_seconds",
			Help:    "Latency to the first streamed byte reaching the client.",
			Buckets: latencyBuckets,
		}, []string{"route"}),

		UpstreamAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_upstream_attempts_total",
			Help: "Calls made to a provider, including retries, by result code.",
		}, []string{"provider", "model", "code"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sluice_upstream_duration_seconds",
			Help:    "Latency of one call to one provider.",
			Buckets: latencyBuckets,
		}, []string{"provider", "model"}),
		Failovers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_failovers_total",
			Help: "Moves to a different target after a failure.",
		}, []string{"route"}),
		ShortCircuits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_short_circuits_total",
			Help: "Attempts refused by an open circuit breaker without an upstream call.",
		}, []string{"provider", "model"}),
		BreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sluice_breaker_state",
			Help: "Circuit breaker state, 1 for the current state and 0 for the others.",
		}, []string{"provider", "model", "state"}),

		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_tokens_total",
			Help: "Tokens billed, by provider, model and direction.",
		}, []string{"provider", "model", "kind"}),
		RequestTokens: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sluice_request_tokens",
			Help:    "Tokens in a single request, by direction.",
			Buckets: tokenBuckets,
		}, []string{"kind"}),
		CostNanodollars: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_cost_nanodollars_total",
			Help: "Spend in nanodollars (1e-9 USD). Divide by 1e9 for dollars.",
		}, []string{"provider", "model"}),

		CacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_cache_lookups_total",
			Help: "Cache lookups by result: exact, semantic, miss, bypass or error.",
		}, []string{"result"}),
		CacheEntries: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sluice_cache_entries",
			Help: "Entries currently held by the response cache.",
		}),

		Redactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_redactions_total",
			Help: "Distinct values replaced, by entity type.",
		}, []string{"entity_type"}),

		RateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_rate_limited_total",
			Help: "Requests refused by a rate limit, by which bucket ran out.",
		}, []string{"kind"}),
		BudgetDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sluice_budget_decisions_total",
			Help: "Budget check outcomes: allow, degrade or reject.",
		}, []string{"action"}),

		StageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sluice_stage_duration_seconds",
			Help:    "Time spent in one pipeline stage.",
			Buckets: latencyBuckets,
		}, []string{"stage"}),

		AuditWritten: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sluice_audit_records_written_total",
			Help: "Audit records successfully appended.",
		}),
		AuditDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sluice_audit_records_dropped_total",
			Help: "Audit records lost to a write error. Any value above zero is an incident.",
		}),
	}

	for _, c := range m.collectors() {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("telemetry: register: %w", err)
		}
	}
	return m, nil
}

func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.Requests, m.RequestDuration, m.TimeToFirstByte,
		m.UpstreamAttempts, m.UpstreamDuration, m.Failovers, m.ShortCircuits, m.BreakerState,
		m.Tokens, m.RequestTokens, m.CostNanodollars,
		m.CacheLookups, m.CacheEntries,
		m.Redactions,
		m.RateLimited, m.BudgetDecisions,
		m.StageDuration,
		m.AuditWritten, m.AuditDropped,
	}
}

// SetBreakerState records a breaker's state as a one-hot set of gauges.
//
// One-hot rather than a numeric encoding because "state 2" in an alert is
// unreadable, and because a range query over an enum encoded as a number
// produces averages of state values, which mean nothing.
func (m *Metrics) SetBreakerState(provider, model, state string) {
	for _, s := range []string{"closed", "open", "half_open"} {
		v := 0.0
		if s == state {
			v = 1
		}
		m.BreakerState.WithLabelValues(provider, model, s).Set(v)
	}
}

// ObserveUsage records tokens and cost for one served request.
func (m *Metrics) ObserveUsage(provider, model string, u llm.Usage, cost llm.Cost) {
	m.Tokens.WithLabelValues(provider, model, "input").Add(float64(u.InputTokens))
	m.Tokens.WithLabelValues(provider, model, "output").Add(float64(u.OutputTokens))
	m.RequestTokens.WithLabelValues("input").Observe(float64(u.InputTokens))
	m.RequestTokens.WithLabelValues("output").Observe(float64(u.OutputTokens))
	if cost > 0 {
		m.CostNanodollars.WithLabelValues(provider, model).Add(float64(cost))
	}
}

// NewLogger builds the process logger.
//
// JSON by default. A gateway's logs are read by a machine first and a human
// second, and the human reading them is usually doing so through something that
// wants structure.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("telemetry: unknown log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json", "":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("telemetry: unknown log format %q", format)
	}
}
