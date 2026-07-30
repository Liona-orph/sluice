package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sluice-gw/sluice/internal/redact"
	"github.com/sluice-gw/sluice/pkg/llm"
)

func TestDefaultConfigIsValidAndUsable(t *testing.T) {
	// The default is what `sluice serve` runs with no file and what the
	// README's quickstart exercises. If it were not a working deployment the
	// quickstart would be fiction.
	cfg := Default()
	require.NoError(t, cfg.Validate())
	assert.NotEmpty(t, cfg.Providers)
	assert.NotEmpty(t, cfg.Routes)
	assert.NotEmpty(t, cfg.Keys)

	route, ok := cfg.Route("sluice-demo")
	require.True(t, ok)
	assert.GreaterOrEqual(t, len(route.Targets), 2, "failover needs somewhere to fail over to")

	for _, k := range cfg.Keys {
		if k.Budget.OnExceed == OnExceedDegrade {
			_, ok := cfg.Route(k.Budget.DegradeTo)
			assert.True(t, ok, "the degrade target must be routable")
		}
	}
}

func TestParseAppliesOverTheDefaults(t *testing.T) {
	cfg, err := Parse([]byte("server:\n  addr: \":9999\"\n"))
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.Server.Addr)
	assert.Equal(t, 5*time.Second, cfg.Server.ReadHeaderTimeout.D(), "unstated fields keep their default")
	assert.True(t, cfg.Redaction.Enabled, "a security control defaults on, so omission cannot switch it off")
}

func TestParseReplacesListsRatherThanMergingThem(t *testing.T) {
	// yaml.v3 merges into a non-empty slice by index. If that behaviour leaked
	// through, a production file listing one key would silently keep the demo
	// key from the defaults, which is a live credential nobody chose.
	cfg, err := Parse([]byte(`
providers:
  - name: only
    type: local
routes:
  - model: m
    strategy: priority
    targets: [{provider: only}]
    retry: {max_attempts: 1}
    breaker: {failure_threshold: 1, success_threshold: 1, open_for: 1s, half_open_probes: 1}
keys:
  - id: prod
    secret: a-long-enough-secret
teams: []
`))
	require.NoError(t, err)
	require.Len(t, cfg.Providers, 1)
	require.Len(t, cfg.Keys, 1)
	assert.Equal(t, "prod", cfg.Keys[0].ID)
	for _, k := range cfg.Keys {
		assert.NotEqual(t, "sk-sluice-local-demo", k.Secret)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	_, err := Parse([]byte("server:\n  adr: \":9999\"\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adr", "the error names the field, which is the whole point of rejecting it")
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	cfg := Default()
	cfg.Server.Addr = ""
	cfg.Telemetry.LogLevel = "loud"
	cfg.Routes[0].Targets[0].Provider = "nonexistent"
	cfg.Keys[0].Secret = "short"
	cfg.Keys[0].Team = "no-such-team"

	err := cfg.Validate()
	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{
		"server.addr", "telemetry.log_level",
		"routes[0].targets[0].provider", "keys[0].secret", "keys[0].team",
	} {
		assert.Contains(t, msg, want, "an operator should fix every problem in one pass, not one per restart")
	}

	var problems ValidationErrors
	require.ErrorAs(t, err, &problems)
	assert.GreaterOrEqual(t, len(problems), 5)
}

func TestValidationCatchesReferentialMistakes(t *testing.T) {
	cases := map[string]func(*Config){
		"routes[0].targets[0].provider": func(c *Config) { c.Routes[0].Targets[0].Provider = "ghost" },
		"keys[0].models[0]":             func(c *Config) { c.Keys[0].Models = []string{"ghost"} },
		"keys[0].budget.degrade_to":     func(c *Config) { c.Keys[0].Budget.DegradeTo = "ghost" },
	}
	for path, mutate := range cases {
		t.Run(path, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), path)
		})
	}
}

func TestHashActionRequiresASalt(t *testing.T) {
	cfg := Default()
	cfg.Redaction.HashSalt = ""
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redaction.hash_salt")
	assert.Contains(t, err.Error(), "word list")
}

func TestSemanticThresholdBelowTheMeasuredFloorIsRejected(t *testing.T) {
	cfg := Default()
	cfg.Cache.Semantic = true
	cfg.Cache.SimilarityThreshold = 0.85
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0.90", "0.85 is where the measured false-hit rate stops being zero")
}

func TestUnknownEntityTypeIsRejected(t *testing.T) {
	cfg := Default()
	cfg.Redaction.Actions["emial"] = "mask"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redaction.actions.emial")
	assert.Contains(t, err.Error(), "unknown entity type")
}

func TestDurationAndMoneyParsing(t *testing.T) {
	cfg, err := Parse([]byte(`
cache:
  ttl: 90m
keys:
  - id: k
    secret: a-long-enough-secret
    budget: {limit: "$12.50", window: 24h, on_exceed: reject}
teams: []
`))
	require.NoError(t, err)
	assert.Equal(t, 90*time.Minute, cfg.Cache.TTL.D())
	assert.Equal(t, llm.Cost(12.5*float64(llm.Dollar)), cfg.Keys[0].Budget.Limit.Cost())
	assert.Equal(t, 24*time.Hour, cfg.Keys[0].Budget.Window.D())
}

func TestDurationRejectsABareNumberInYAML(t *testing.T) {
	// A bare number would be ambiguous about its unit, which is how every
	// config format ends up with timeout_ms next to timeout.
	_, err := Parse([]byte("cache:\n  ttl: 30\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duration")
}

func TestApplyEnvOverlaysTheFile(t *testing.T) {
	env := map[string]string{
		"SLUICE_ADDR":              ":7000",
		"SLUICE_LOG_LEVEL":         "debug",
		"SLUICE_CACHE_ENABLED":     "false",
		"SLUICE_REDACTION_ENABLED": "true",
	}
	cfg := Default()
	require.NoError(t, cfg.ApplyEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }))
	assert.Equal(t, ":7000", cfg.Server.Addr)
	assert.Equal(t, "debug", cfg.Telemetry.LogLevel)
	assert.False(t, cfg.Cache.Enabled)
	assert.True(t, cfg.Redaction.Enabled)
}

func TestApplyEnvRejectsANonBoolean(t *testing.T) {
	cfg := Default()
	err := cfg.ApplyEnv(func(k string) (string, bool) {
		if k == "SLUICE_CACHE_ENABLED" {
			return "sometimes", true
		}
		return "", false
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SLUICE_CACHE_ENABLED")
}

func TestRedactPolicyMapsTheConfigVocabulary(t *testing.T) {
	cfg := Default()
	p := cfg.RedactPolicy()
	assert.Equal(t, redact.ActionTokenize, p.For(redact.EntityEmail))
	assert.Equal(t, redact.ActionMask, p.For(redact.EntityCreditCard))
	assert.Equal(t, redact.ActionHash, p.For(redact.EntityAPIKey))
	assert.NotEmpty(t, p.HashSalt)
}

func TestPricingOverridesApplyOverTheSnapshot(t *testing.T) {
	cfg := Default()
	cfg.Pricing = map[string]Price{
		"gpt-4o": {InputPerMillion: Money(llm.Dollar), OutputPerMillion: Money(2 * llm.Dollar)},
	}
	table := cfg.PricingTable()
	price, ok := table.Exact("gpt-4o")
	require.True(t, ok)
	assert.Equal(t, llm.Dollar, price.InputPerMillion)
	_, ok = table.Exact("claude-3-opus")
	assert.True(t, ok, "the rest of the snapshot survives an override")
}

func TestRedactedHidesSecrets(t *testing.T) {
	cfg := Default()
	out := cfg.Redacted()
	for _, k := range out.Keys {
		assert.Equal(t, "[redacted]", k.Secret)
	}
	assert.Equal(t, "[redacted]", out.Redaction.HashSalt)
	assert.NotEqual(t, "[redacted]", cfg.Keys[0].Secret, "the original is untouched")

	b, err := out.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, string(b), "sk-sluice-local-demo")
}

func TestMarshalRoundTrips(t *testing.T) {
	cfg := Default()
	b, err := cfg.Marshal()
	require.NoError(t, err)
	back, err := Parse(b)
	require.NoError(t, err)
	assert.Equal(t, cfg.Server.Addr, back.Server.Addr)
	assert.Equal(t, cfg.Cache.TTL, back.Cache.TTL)
	assert.Equal(t, len(cfg.Routes), len(back.Routes))
	assert.Equal(t, cfg.Keys[0].Budget.Limit, back.Keys[0].Budget.Limit)
}

func TestValidationErrorsErrorFormatsReadably(t *testing.T) {
	ps := ValidationErrors{{Path: "a.b", Msg: "bad"}, {Path: "c", Msg: "worse"}}
	msg := ps.Error()
	assert.True(t, strings.HasPrefix(msg, "config: 2 problems:"))
	assert.Contains(t, msg, "a.b: bad")
	assert.NoError(t, ValidationErrors{}.ErrOrNil())
}
