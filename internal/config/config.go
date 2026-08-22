// Package config is the declarative description of a running gateway.
//
// Everything the decision layer does -- which upstream serves a model alias,
// what a key is allowed to spend, how fast it may ask, what gets redacted --
// is data here rather than code somewhere else. That is not tidiness for its
// own sake: routing and budget rules change on the timescale of a pricing
// negotiation, and a change that requires a rebuild is a change that gets made
// in a hurry at the wrong time.
//
// The package holds data and validation and nothing else. It does not build
// providers, routers or caches; internal/gateway does that. The direction is
// deliberate -- config imports the packages whose vocabulary it borrows
// (pkg/llm, internal/redact) and none of the packages that consume it -- so
// that adding a knob never risks an import cycle and so that a test can build
// a Config in Go without going through YAML.
//
// Validation is exhaustive rather than fail-fast. Load reports every problem it
// finds in one pass, each with the path of the field that caused it, because an
// operator fixing a config file one error per restart is an operator who will
// eventually stop reading them.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Liona-orph/sluice/internal/redact"
	"github.com/Liona-orph/sluice/pkg/llm"
)

// Config is a whole gateway.
type Config struct {
	Server    Server           `yaml:"server" json:"server"`
	Telemetry Telemetry        `yaml:"telemetry" json:"telemetry"`
	Audit     Audit            `yaml:"audit" json:"audit"`
	Redaction Redaction        `yaml:"redaction" json:"redaction"`
	Cache     Cache            `yaml:"cache" json:"cache"`
	Providers []Provider       `yaml:"providers" json:"providers"`
	Routes    []Route          `yaml:"routes" json:"routes"`
	Keys      []Key            `yaml:"keys" json:"keys"`
	Teams     []Team           `yaml:"teams" json:"teams"`
	Pricing   map[string]Price `yaml:"pricing" json:"pricing"`
}

// Server is the HTTP surface.
type Server struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string `yaml:"addr" json:"addr"`
	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers. It is set rather than left at zero because the zero value is
	// "no limit", which is a slowloris invitation on any public listener.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout" json:"read_header_timeout"`
	// RequestTimeout bounds a whole completion, streaming included. Generous,
	// because a long completion is a legitimate request, not an attack.
	RequestTimeout Duration `yaml:"request_timeout" json:"request_timeout"`
	// ShutdownGrace is how long a graceful shutdown waits for in-flight
	// requests before it stops being graceful.
	ShutdownGrace Duration `yaml:"shutdown_grace" json:"shutdown_grace"`
	// MaxRequestBytes caps the decoded request body.
	MaxRequestBytes int64 `yaml:"max_request_bytes" json:"max_request_bytes"`
	// Dashboard serves the embedded operator dashboard at /.
	Dashboard bool `yaml:"dashboard" json:"dashboard"`
}

// Telemetry configures logging and metrics.
type Telemetry struct {
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log_level" json:"log_level"`
	// LogFormat is json or text.
	LogFormat string `yaml:"log_format" json:"log_format"`
	// MetricsPath is where the Prometheus exposition lives. Empty disables it.
	MetricsPath string `yaml:"metrics_path" json:"metrics_path"`
}

// Audit configures the audit log.
type Audit struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Path is the file to append to. "-" means stdout, which is what a
	// container wants when a log shipper owns the file system.
	Path string `yaml:"path" json:"path"`
	// Sync fsyncs after every record. It costs roughly an order of magnitude in
	// throughput and buys durability across a machine failure rather than a
	// process failure; a gateway whose audit log is a compliance artefact wants
	// it, one whose audit log is a cost report does not.
	Sync bool `yaml:"sync" json:"sync"`
}

// Redaction configures the redactor.
type Redaction struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// DefaultAction applies to entity types not named in Actions.
	DefaultAction string `yaml:"default_action" json:"default_action"`
	// Actions maps entity type to one of tokenize, mask, hash, allow.
	Actions map[string]string `yaml:"actions" json:"actions"`
	// MaskKeepLast is how many trailing characters a mask leaves visible.
	MaskKeepLast int `yaml:"mask_keep_last" json:"mask_keep_last"`
	// MinConfidence drops matches below this score. 0.6 removes the
	// gazetteer-only person-name matches; see internal/redact for what that
	// costs in recall.
	MinConfidence float64 `yaml:"min_confidence" json:"min_confidence"`
	// HashSalt makes hashed pseudonyms unguessable. An empty salt is accepted
	// but warned about, because a hash of an email address without a salt is an
	// email address to anyone with a word list.
	HashSalt string `yaml:"hash_salt" json:"hash_salt"`
}

// Cache configures the response cache.
type Cache struct {
	Enabled    bool     `yaml:"enabled" json:"enabled"`
	MaxEntries int      `yaml:"max_entries" json:"max_entries"`
	TTL        Duration `yaml:"ttl" json:"ttl"`
	// Semantic enables nearest-neighbour lookup. Off by default: an exact cache
	// cannot return a wrong answer and a semantic one can.
	Semantic bool `yaml:"semantic" json:"semantic"`
	// EmbeddingDimensions sizes the built-in hashing embedder.
	EmbeddingDimensions int `yaml:"embedding_dimensions" json:"embedding_dimensions"`
	// SimilarityThreshold is the cosine a semantic candidate must reach. The
	// default is measured, not guessed; see internal/cache.
	SimilarityThreshold float64 `yaml:"similarity_threshold" json:"similarity_threshold"`
	// SweepInterval runs the expiry sweep. Zero disables it and leaves expiry
	// lazy, which is correct but holds memory for entries nobody asks for.
	SweepInterval Duration `yaml:"sweep_interval" json:"sweep_interval"`
}

// Provider is one configured upstream.
type Provider struct {
	// Name is how routes refer to this upstream. It is also the value that
	// appears in metrics and audit records, so it is bounded-cardinality by
	// construction: it comes from a config file an operator wrote.
	Name string `yaml:"name" json:"name"`
	// Type selects the adapter. Only "local" exists; see docs/adr/0004.
	Type string `yaml:"type" json:"type"`
	// Seed shifts the local provider's deterministic output, so that two
	// configured upstreams give distinguishable answers to the same question.
	Seed int64 `yaml:"seed" json:"seed"`
	// Models restricts what this upstream accepts. Empty accepts anything.
	Models []string `yaml:"models" json:"models"`
	// ContextTokens is the window. Zero is unbounded.
	ContextTokens int `yaml:"context_tokens" json:"context_tokens"`
	// MaxOutputTokens caps generation when a request does not.
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens"`
	// Latency and Failure make the local provider imitate a real one. They are
	// in the config file, not just in tests, because the only honest way to
	// demonstrate that failover and the circuit breaker work is to run the
	// gateway against an upstream that actually fails.
	Latency Latency `yaml:"latency" json:"latency"`
	Failure Failure `yaml:"failure" json:"failure"`
}

// Latency imitates upstream timing.
type Latency struct {
	TimeToFirstToken Duration `yaml:"time_to_first_token" json:"time_to_first_token"`
	PerToken         Duration `yaml:"per_token" json:"per_token"`
	Jitter           float64  `yaml:"jitter" json:"jitter"`
}

// Failure injects errors.
type Failure struct {
	// Code is an llm.ErrorCode. Empty injects nothing.
	Code string `yaml:"code" json:"code"`
	// Rate is the probability in [0,1], evaluated deterministically per
	// request, so a failing request keeps failing and a test stays reproducible.
	Rate        float64  `yaml:"rate" json:"rate"`
	RetryAfter  Duration `yaml:"retry_after" json:"retry_after"`
	Message     string   `yaml:"message" json:"message"`
	AfterChunks int      `yaml:"after_chunks" json:"after_chunks"`
}

// Strategy selects between healthy targets.
type Strategy string

const (
	// StrategyPriority always prefers the first healthy target. Correct when
	// the list is ordered by preference -- cheapest first, or primary then
	// standby -- and it is the default because that is what a failover list
	// usually is.
	StrategyPriority Strategy = "priority"
	// StrategyRoundRobin spreads load evenly over healthy targets. Correct when
	// the targets are interchangeable and the goal is to stay inside several
	// providers' rate limits at once.
	StrategyRoundRobin Strategy = "round_robin"
	// StrategyWeighted spreads load in proportion to Target.Weight, for a
	// deliberate split -- 90% to a committed-spend contract, 10% to a second
	// vendor kept warm so that the failover path is exercised before it is
	// needed rather than during an incident.
	StrategyWeighted Strategy = "weighted"
)

// Route maps a model alias onto an ordered list of targets.
type Route struct {
	// Model is the alias a client asks for.
	Model string `yaml:"model" json:"model"`
	// Strategy is how healthy targets are chosen between.
	Strategy Strategy `yaml:"strategy" json:"strategy"`
	Targets  []Target `yaml:"targets" json:"targets"`
	Retry    Retry    `yaml:"retry" json:"retry"`
	Breaker  Breaker  `yaml:"breaker" json:"breaker"`
}

// Target is one provider/model pair a route may use.
type Target struct {
	Provider string `yaml:"provider" json:"provider"`
	// Model is the physical model name sent upstream. Empty means "the alias",
	// which is the common case for a provider that already uses the public name.
	Model string `yaml:"model" json:"model"`
	// Weight is used by StrategyWeighted. Zero is treated as one.
	Weight int `yaml:"weight" json:"weight"`
}

// Retry configures attempts against a single target.
type Retry struct {
	// MaxAttempts counts the first try. 1 disables retry.
	MaxAttempts int      `yaml:"max_attempts" json:"max_attempts"`
	BaseDelay   Duration `yaml:"base_delay" json:"base_delay"`
	MaxDelay    Duration `yaml:"max_delay" json:"max_delay"`
	Multiplier  float64  `yaml:"multiplier" json:"multiplier"`
	// Jitter is the fraction of each computed delay that is randomised. Without
	// it a thundering herd retries in lockstep and re-creates the overload it
	// is backing off from.
	Jitter float64 `yaml:"jitter" json:"jitter"`
}

// Breaker configures the per-target circuit breaker.
type Breaker struct {
	// FailureThreshold is the number of consecutive failures that opens the
	// circuit. Consecutive rather than a rate over a window: a rate needs a
	// window long enough to be statistically meaningful, which is longer than
	// an operator is willing to keep sending traffic into a dead upstream, and
	// the gateway already has a second target to send it to instead.
	FailureThreshold int `yaml:"failure_threshold" json:"failure_threshold"`
	// SuccessThreshold is the number of consecutive successes in half-open that
	// closes the circuit again. Above one so that a single lucky probe does not
	// restore full traffic to an upstream that is still failing most requests.
	SuccessThreshold int `yaml:"success_threshold" json:"success_threshold"`
	// OpenFor is how long the circuit stays open before a probe is allowed.
	OpenFor Duration `yaml:"open_for" json:"open_for"`
	// HalfOpenProbes bounds concurrent trial requests while half-open.
	HalfOpenProbes int `yaml:"half_open_probes" json:"half_open_probes"`
}

// Key is one API key and everything attached to it.
type Key struct {
	// ID is the stable, non-secret identifier. It appears in audit records,
	// logs and -- because it is bounded by the config file -- is the only
	// principal identifier that may appear in a metric label.
	ID string `yaml:"id" json:"id"`
	// Secret is the bearer token clients present. It is a secret in a config
	// file, which is a real weakness; see SECURITY.md.
	Secret string `yaml:"secret" json:"secret"`
	// Team attributes this key's spend to a shared budget.
	Team string `yaml:"team" json:"team"`
	// Models restricts which route aliases this key may use. Empty allows all.
	Models    []string  `yaml:"models" json:"models"`
	RateLimit RateLimit `yaml:"rate_limit" json:"rate_limit"`
	Budget    Budget    `yaml:"budget" json:"budget"`
}

// Team is a shared budget across several keys.
type Team struct {
	ID     string `yaml:"id" json:"id"`
	Budget Budget `yaml:"budget" json:"budget"`
}

// RateLimit is a token bucket on requests and another on tokens.
//
// Both, because they bound different things. A request limit bounds concurrency
// and protects the upstream; it does not bound spend, since one request with a
// 200k-token context costs more than a thousand short ones. A token limit
// bounds spend rate directly. Running only one of them leaves the other failure
// mode wide open.
type RateLimit struct {
	RequestsPerMinute float64 `yaml:"requests_per_minute" json:"requests_per_minute"`
	TokensPerMinute   float64 `yaml:"tokens_per_minute" json:"tokens_per_minute"`
	// Burst is the bucket depth as a multiple of one minute's refill. 1 means
	// no burst allowance beyond the steady rate.
	Burst float64 `yaml:"burst" json:"burst"`
}

// Zero reports whether no limit is configured.
func (r RateLimit) Zero() bool { return r.RequestsPerMinute <= 0 && r.TokensPerMinute <= 0 }

// OnExceed is what happens when a budget is exhausted.
type OnExceed string

const (
	// OnExceedReject refuses the request with HTTP 402. It is the default
	// because a budget that silently changes the model changes the answers an
	// application gets, and an application that starts getting worse answers
	// with no error to correlate against is a support ticket nobody can close.
	// A 402 is loud, attributable and immediately actionable.
	OnExceedReject OnExceed = "reject"
	// OnExceedDegrade reroutes to a cheaper model alias and keeps serving. It
	// is right where availability outranks fidelity -- a background
	// summarisation job, an internal tool -- and it is only safe because the
	// response says which model actually answered and the audit record says the
	// request was degraded, so the change is observable rather than silent.
	OnExceedDegrade OnExceed = "degrade"
)

// Budget is a spend limit over a rolling window.
type Budget struct {
	// Limit is the cap in US dollars over Window. Zero means no budget.
	Limit Money `yaml:"limit" json:"limit"`
	// Window is the rolling period. Rolling rather than calendar: a calendar
	// month resets at midnight UTC and invites a queue of deferred work to
	// stampede the moment it does, while a rolling window spreads the same
	// budget smoothly and is what an operator actually means by "$50 a day".
	Window Duration `yaml:"window" json:"window"`
	// OnExceed is reject or degrade.
	OnExceed OnExceed `yaml:"on_exceed" json:"on_exceed"`
	// DegradeTo names the cheaper route alias used by OnExceedDegrade.
	DegradeTo string `yaml:"degrade_to" json:"degrade_to"`
}

// Zero reports whether no budget is configured.
func (b Budget) Zero() bool { return b.Limit <= 0 }

// Price overrides an entry in the built-in pricing table.
//
// Values are US dollars per million tokens. The built-in table is a snapshot
// with a date on it (llm.PricingSnapshot) and nothing keeps it current, so a
// deployment that reports cost to a finance team should state its own prices
// here and reconcile against the vendor invoice.
type Price struct {
	InputPerMillion       Money `yaml:"input_per_million" json:"input_per_million"`
	OutputPerMillion      Money `yaml:"output_per_million" json:"output_per_million"`
	CachedInputPerMillion Money `yaml:"cached_input_per_million" json:"cached_input_per_million"`
}

// --- scalar types -----------------------------------------------------------

// Duration is a time.Duration that reads as "30s" in YAML and JSON.
//
// A bare integer would be ambiguous about its unit, and every config format
// that has tried it has ended up with a field named timeout_ms next to one
// named timeout. Naming the unit in the value removes the question.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\": %w", err)
	}
	return d.parse(s)
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalJSON accepts both "30s" and a number of nanoseconds, the latter so
// that a Config round-trips through encoding/json without a custom encoder on
// the other side.
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return fmt.Errorf("duration: %w", err)
		}
		return d.parse(unquoted)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("duration must be a string such as \"30s\" or a nanosecond count: %q", s)
	}
	*d = Duration(n)
	return nil
}

// MarshalJSON writes the string form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(d.String())), nil
}

func (d *Duration) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Money is an amount in US dollars, stored as llm.Cost nanodollars.
//
// It is parsed from a decimal string or a number and stored as an integer for
// the reason llm.Cost exists: a gateway adds up millions of these and float64
// accumulation at 1e-6 loses cents.
type Money llm.Cost

// Cost returns the amount as an llm.Cost.
func (m Money) Cost() llm.Cost { return llm.Cost(m) }

func (m Money) String() string { return llm.Cost(m).String() }

// UnmarshalYAML implements yaml.Unmarshaler, accepting 12.5, "12.50" or "$12.50".
func (m *Money) UnmarshalYAML(unmarshal func(any) error) error {
	var f float64
	if err := unmarshal(&f); err == nil {
		*m = Money(llm.Cost(f * float64(llm.Dollar)))
		return nil
	}
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("money must be a number or a string such as \"12.50\": %w", err)
	}
	return m.parse(s)
}

// MarshalYAML writes a plain decimal so that a rendered config is readable.
func (m Money) MarshalYAML() (any, error) { return llm.Cost(m).Dollars(), nil }

// UnmarshalJSON accepts the same forms as UnmarshalYAML.
func (m *Money) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil
	}
	if len(s) >= 2 && s[0] == '"' {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return fmt.Errorf("money: %w", err)
		}
		return m.parse(unquoted)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("money must be a number or a decimal string: %q", s)
	}
	*m = Money(llm.Cost(f * float64(llm.Dollar)))
	return nil
}

// MarshalJSON writes the dollar amount as a number.
func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(llm.Cost(m).Dollars(), 'f', -1, 64)), nil
}

func (m *Money) parse(s string) error {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	if s == "" {
		*m = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid money %q: %w", s, err)
	}
	*m = Money(llm.Cost(f * float64(llm.Dollar)))
	return nil
}

// --- lookups ----------------------------------------------------------------

// Route returns the route for a model alias.
func (c *Config) Route(model string) (Route, bool) {
	for _, r := range c.Routes {
		if r.Model == model {
			return r, true
		}
	}
	return Route{}, false
}

// Team returns a team by ID.
func (c *Config) Team(id string) (Team, bool) {
	for _, t := range c.Teams {
		if t.ID == id {
			return t, true
		}
	}
	return Team{}, false
}

// KeyByID returns a key by its non-secret identifier.
func (c *Config) KeyByID(id string) (Key, bool) {
	for _, k := range c.Keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

// actionNames maps the config vocabulary onto redact.Action. It is here rather
// than in internal/redact because it is a serialisation concern: the redactor
// has no business knowing what a YAML file calls its constants.
var actionNames = map[string]redact.Action{
	"tokenize": redact.ActionTokenize,
	"mask":     redact.ActionMask,
	"hash":     redact.ActionHash,
	"allow":    redact.ActionAllow,
}

// entityTypes is the set of entity names a config may mention. A typo in an
// entity name would otherwise be a silently ignored redaction rule, which is
// the most dangerous kind of configuration error this file can contain.
var entityTypes = map[string]redact.EntityType{
	"email":       redact.EntityEmail,
	"phone":       redact.EntityPhone,
	"credit_card": redact.EntityCreditCard,
	"iban":        redact.EntityIBAN,
	"ip_address":  redact.EntityIPAddress,
	"us_ssn":      redact.EntityUSSSN,
	"uk_nino":     redact.EntityUKNINO,
	"es_dni":      redact.EntityESDNI,
	"api_key":     redact.EntityAPIKey,
	"person_name": redact.EntityPersonName,
}

// RedactPolicy converts the declarative redaction section into a redact.Policy.
// It assumes the config has been validated.
func (c *Config) RedactPolicy() redact.Policy {
	p := redact.Policy{
		Default:       actionNames[c.Redaction.DefaultAction],
		ByType:        map[redact.EntityType]redact.Action{},
		MaskKeepLast:  c.Redaction.MaskKeepLast,
		MinConfidence: c.Redaction.MinConfidence,
	}
	if c.Redaction.HashSalt != "" {
		p.HashSalt = []byte(c.Redaction.HashSalt)
	}
	for name, action := range c.Redaction.Actions {
		p.ByType[entityTypes[name]] = actionNames[action]
	}
	return p
}

// PricingTable returns the built-in price table with this config's overrides
// applied.
func (c *Config) PricingTable() *llm.Pricing {
	table := llm.DefaultPricing()
	for model, p := range c.Pricing {
		table = table.With(model, llm.ModelPrice{
			InputPerMillion:       p.InputPerMillion.Cost(),
			OutputPerMillion:      p.OutputPerMillion.Cost(),
			CachedInputPerMillion: p.CachedInputPerMillion.Cost(),
		})
	}
	return table
}
