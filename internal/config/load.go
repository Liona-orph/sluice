package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default returns a configuration that runs offline against the local provider.
//
// It is a working deployment, not a skeleton: two upstreams so that failover
// has somewhere to go, three route aliases so that budget degradation has a
// cheaper model to degrade to, and one demo key. That matters because it is
// what `sluice serve` uses when no config file is given, and the README's
// quickstart runs against exactly this. A default config that could not serve a
// request would make the quickstart a lie.
//
// The demo secret is in the source. That is safe only because this config
// reaches no network and spends no money; a deployment that copies it and adds
// a real provider has published a credential. SECURITY.md says so again.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8080",
			ReadHeaderTimeout: Duration(5_000_000_000),   // 5s
			RequestTimeout:    Duration(120_000_000_000), // 2m
			ShutdownGrace:     Duration(15_000_000_000),  // 15s
			MaxRequestBytes:   4 << 20,
			Dashboard:         true,
		},
		Telemetry: Telemetry{LogLevel: "info", LogFormat: "json", MetricsPath: "/metrics"},
		Audit:     Audit{Enabled: true, Path: "-"},
		Redaction: Redaction{
			Enabled:       true,
			DefaultAction: "tokenize",
			MaskKeepLast:  4,
			HashSalt:      "sluice-default-salt-change-me",
			Actions: map[string]string{
				"email":       "tokenize",
				"phone":       "tokenize",
				"person_name": "tokenize",
				"ip_address":  "tokenize",
				"credit_card": "mask",
				"iban":        "mask",
				"us_ssn":      "mask",
				"uk_nino":     "mask",
				"es_dni":      "mask",
				"api_key":     "hash",
			},
		},
		Cache: Cache{
			Enabled:             true,
			MaxEntries:          10_000,
			TTL:                 Duration(3_600_000_000_000), // 1h
			Semantic:            false,
			EmbeddingDimensions: 256,
			SimilarityThreshold: 0.97,
			SweepInterval:       Duration(60_000_000_000), // 1m
		},
		Providers: []Provider{
			{Name: "local-primary", Type: "local", Seed: 1, MaxOutputTokens: 200},
			{Name: "local-standby", Type: "local", Seed: 2, MaxOutputTokens: 200},
		},
		Routes: []Route{
			{
				Model: "sluice-demo", Strategy: StrategyPriority,
				Targets: []Target{
					{Provider: "local-primary", Model: "local-large"},
					{Provider: "local-standby", Model: "local-large"},
				},
				Retry:   defaultRetry(),
				Breaker: defaultBreaker(),
			},
			{
				Model: "sluice-demo-cheap", Strategy: StrategyPriority,
				Targets: []Target{
					{Provider: "local-primary", Model: "local-small"},
					{Provider: "local-standby", Model: "local-small"},
				},
				Retry:   defaultRetry(),
				Breaker: defaultBreaker(),
			},
			// An alias named after a real model, served by the local provider
			// under that name. The local provider accepts any model name, so
			// the request is answered offline while the cost is computed from
			// the real list price in llm.DefaultPricing. The money is therefore
			// simulated and the token counts are not: it is what this traffic
			// would have cost at gpt-4o-mini's published price. It exists so
			// that the dashboard's spend chart and `sluice replay` have
			// something true to show without a paid API key. Nothing here
			// reaches OpenAI.
			{
				Model: "gpt-4o-mini", Strategy: StrategyRoundRobin,
				Targets: []Target{
					{Provider: "local-primary", Model: "gpt-4o-mini"},
					{Provider: "local-standby", Model: "gpt-4o-mini"},
				},
				Retry:   defaultRetry(),
				Breaker: defaultBreaker(),
			},
		},
		Teams: []Team{
			{ID: "demo", Budget: Budget{Limit: Money(50 * 1_000_000_000), Window: Duration(86_400_000_000_000), OnExceed: OnExceedReject}},
		},
		Keys: []Key{
			{
				ID: "demo-key", Secret: "sk-sluice-local-demo", Team: "demo",
				RateLimit: RateLimit{RequestsPerMinute: 120, TokensPerMinute: 200_000, Burst: 2},
				Budget: Budget{
					Limit: Money(5 * 1_000_000_000), Window: Duration(86_400_000_000_000),
					OnExceed: OnExceedDegrade, DegradeTo: "sluice-demo-cheap",
				},
			},
		},
	}
}

func defaultRetry() Retry {
	return Retry{
		MaxAttempts: 3,
		BaseDelay:   Duration(50_000_000),    // 50ms
		MaxDelay:    Duration(2_000_000_000), // 2s
		Multiplier:  2,
		Jitter:      0.3,
	}
}

func defaultBreaker() Breaker {
	return Breaker{
		FailureThreshold: 5,
		SuccessThreshold: 2,
		OpenFor:          Duration(30_000_000_000), // 30s
		HalfOpenProbes:   1,
	}
}

// Parse decodes YAML over the defaults and validates the result.
//
// Unknown fields are rejected. A silently ignored key in a security product's
// configuration is the worst kind of bug: the operator believes the control is
// on, the file says it is on, and it is not. The cost is that a config written
// for a newer version fails against an older binary, which is a good trade
// because it fails at startup with the field name in the message.
//
// Decoding over the defaults rather than into a zero value means a file only
// has to state what it changes. It also means a boolean cannot be turned off by
// omission, which is why every default that could be a security control
// (redaction, audit) defaults to on.
func Parse(data []byte) (Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse: %w", err)
	}
	// A file that lists providers, routes or keys replaces the defaults for
	// that section wholesale -- yaml.v3 appends to a non-empty slice otherwise,
	// which would leave the demo key enabled in a production deployment.
	if err := stripDefaultLists(&cfg, data); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// stripDefaultLists re-decodes the list-valued sections into a zero Config so
// that a file which mentions them gets exactly what it wrote.
//
// yaml.v3 merges into an existing slice by index rather than replacing it, so
// decoding a one-provider file over a two-provider default leaves the second
// default provider in place. For scalar fields that merge behaviour is what we
// want; for the lists that define the security perimeter it is dangerous, so
// the lists are handled separately.
func stripDefaultLists(cfg *Config, data []byte) error {
	var raw struct {
		Providers []Provider `yaml:"providers"`
		Routes    []Route    `yaml:"routes"`
		Keys      []Key      `yaml:"keys"`
		Teams     []Team     `yaml:"teams"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(false)
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("config: parse lists: %w", err)
	}
	if raw.Providers != nil {
		cfg.Providers = raw.Providers
	}
	if raw.Routes != nil {
		cfg.Routes = raw.Routes
	}
	if raw.Keys != nil {
		cfg.Keys = raw.Keys
	}
	if raw.Teams != nil {
		cfg.Teams = raw.Teams
	}
	return nil
}

// Load reads and validates a config file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is an operator-supplied argument, which is the point
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// EnvPrefix is the prefix for every environment variable Sluice reads.
const EnvPrefix = "SLUICE_"

// ApplyEnv overlays environment variables onto cfg.
//
// Only the settings an operator needs to change per environment are exposed:
// the listen address, log level and format, the audit destination, the hash
// salt, and the on/off switches for redaction and caching. Routes, keys and
// budgets are deliberately not settable from the environment -- they are
// structured data, and flattening a list of routes into environment variables
// produces a syntax nobody can validate and nobody can review.
//
// The salt is the exception that proves the rule: it is the one secret in the
// config file, and a deployment that keeps its config in version control needs
// somewhere else to put it.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) error {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	str := func(name string, dst *string) {
		if v, ok := lookup(EnvPrefix + name); ok {
			*dst = v
		}
	}
	var errs ValidationErrors
	boolean := func(name string, dst *bool) {
		v, ok := lookup(EnvPrefix + name)
		if !ok {
			return
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, Problem{Path: EnvPrefix + name, Msg: fmt.Sprintf("must be a boolean; got %q", v)})
			return
		}
		*dst = b
	}

	str("ADDR", &c.Server.Addr)
	str("LOG_LEVEL", &c.Telemetry.LogLevel)
	str("LOG_FORMAT", &c.Telemetry.LogFormat)
	str("METRICS_PATH", &c.Telemetry.MetricsPath)
	str("AUDIT_PATH", &c.Audit.Path)
	str("REDACTION_HASH_SALT", &c.Redaction.HashSalt)
	boolean("AUDIT_ENABLED", &c.Audit.Enabled)
	boolean("AUDIT_SYNC", &c.Audit.Sync)
	boolean("REDACTION_ENABLED", &c.Redaction.Enabled)
	boolean("CACHE_ENABLED", &c.Cache.Enabled)
	boolean("CACHE_SEMANTIC", &c.Cache.Semantic)
	boolean("DASHBOARD", &c.Server.Dashboard)
	return errs.ErrOrNil()
}

// EnvVars lists the environment variables ApplyEnv reads, for --help output.
func EnvVars() []string {
	return []string{
		EnvPrefix + "ADDR",
		EnvPrefix + "AUDIT_ENABLED",
		EnvPrefix + "AUDIT_PATH",
		EnvPrefix + "AUDIT_SYNC",
		EnvPrefix + "CACHE_ENABLED",
		EnvPrefix + "CACHE_SEMANTIC",
		EnvPrefix + "CONFIG",
		EnvPrefix + "DASHBOARD",
		EnvPrefix + "LOG_FORMAT",
		EnvPrefix + "LOG_LEVEL",
		EnvPrefix + "METRICS_PATH",
		EnvPrefix + "REDACTION_ENABLED",
		EnvPrefix + "REDACTION_HASH_SALT",
	}
}

// Marshal renders the config as YAML, for `sluice serve --print-config`. It is
// the only way to answer "what is this process actually running" without
// guessing at the interaction of file, environment and flags.
func (c *Config) Marshal() ([]byte, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("config: marshal: %w", err)
	}
	return b, nil
}

// Redacted returns a copy with every key secret replaced, so that a config dump
// can be pasted into an issue.
func (c *Config) Redacted() Config {
	out := *c
	out.Keys = append([]Key(nil), c.Keys...)
	for i := range out.Keys {
		out.Keys[i].Secret = "[redacted]"
	}
	if out.Redaction.HashSalt != "" {
		out.Redaction.HashSalt = "[redacted]"
	}
	return out
}
