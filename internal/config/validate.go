package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sluice-gw/sluice/pkg/llm"
)

// Problem is one validation failure, located by the path of the offending
// field. The path uses the file's own vocabulary -- routes[1].targets[0].provider
// -- so that an operator can find it without knowing the Go type names.
type Problem struct {
	Path string
	Msg  string
}

func (p Problem) String() string { return p.Path + ": " + p.Msg }

// ValidationErrors is the set of failures found in one validation pass.
type ValidationErrors []Problem

func (ps ValidationErrors) Error() string {
	if len(ps) == 0 {
		return "config: valid"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d problem", len(ps))
	if len(ps) != 1 {
		b.WriteByte('s')
	}
	b.WriteByte(':')
	for _, p := range ps {
		b.WriteString("\n  ")
		b.WriteString(p.String())
	}
	return b.String()
}

// ErrOrNil returns a typed nil-safe error, so that callers can write
// `return problems.ErrOrNil()` without the classic nil-interface trap.
func (ps ValidationErrors) ErrOrNil() error {
	if len(ps) == 0 {
		return nil
	}
	return ps
}

type validator struct{ problems ValidationErrors }

func (v *validator) fail(path, format string, args ...any) {
	v.problems = append(v.problems, Problem{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// Validate checks the whole config and returns every problem it finds.
//
// Two classes of check are deliberately included that a schema alone would not
// catch. Referential ones -- a route target naming a provider that does not
// exist, a key naming a team that does not exist, a degrade_to naming a route
// that does not exist -- because those fail at the worst possible moment
// otherwise: the first time a budget is exceeded, in production, months after
// the typo. And semantic ones -- a degrade target that costs more than the
// model it degrades from, a burst below one -- because a config that is
// syntactically fine and operationally nonsense is the failure mode a
// validation pass exists to catch.
func (c *Config) Validate() error {
	v := &validator{}
	c.validateServer(v)
	c.validateTelemetry(v)
	c.validateAudit(v)
	c.validateRedaction(v)
	c.validateCache(v)
	providers := c.validateProviders(v)
	routes := c.validateRoutes(v, providers)
	c.validateTeams(v)
	c.validateKeys(v, routes)
	c.validatePricing(v)
	sort.SliceStable(v.problems, func(i, j int) bool { return v.problems[i].Path < v.problems[j].Path })
	return v.problems.ErrOrNil()
}

func (c *Config) validateServer(v *validator) {
	if c.Server.Addr == "" {
		v.fail("server.addr", "must not be empty")
	}
	if c.Server.ReadHeaderTimeout <= 0 {
		v.fail("server.read_header_timeout", "must be positive; zero means no limit, which is a slowloris invitation")
	}
	if c.Server.RequestTimeout <= 0 {
		v.fail("server.request_timeout", "must be positive")
	}
	if c.Server.ShutdownGrace < 0 {
		v.fail("server.shutdown_grace", "must not be negative")
	}
	if c.Server.MaxRequestBytes <= 0 {
		v.fail("server.max_request_bytes", "must be positive")
	}
}

func (c *Config) validateTelemetry(v *validator) {
	switch c.Telemetry.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		v.fail("telemetry.log_level", "must be one of debug, info, warn, error; got %q", c.Telemetry.LogLevel)
	}
	switch c.Telemetry.LogFormat {
	case "json", "text":
	default:
		v.fail("telemetry.log_format", "must be json or text; got %q", c.Telemetry.LogFormat)
	}
	if p := c.Telemetry.MetricsPath; p != "" && !strings.HasPrefix(p, "/") {
		v.fail("telemetry.metrics_path", "must begin with / or be empty to disable; got %q", p)
	}
}

func (c *Config) validateAudit(v *validator) {
	if c.Audit.Enabled && c.Audit.Path == "" {
		v.fail("audit.path", "must be a file path or \"-\" for stdout when audit is enabled")
	}
}

func (c *Config) validateRedaction(v *validator) {
	if !c.Redaction.Enabled {
		return
	}
	if _, ok := actionNames[c.Redaction.DefaultAction]; !ok {
		v.fail("redaction.default_action", "must be one of %s; got %q", knownActions(), c.Redaction.DefaultAction)
	}
	names := make([]string, 0, len(c.Redaction.Actions))
	for name := range c.Redaction.Actions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := "redaction.actions." + name
		if _, ok := entityTypes[name]; !ok {
			v.fail(path, "unknown entity type; known types are %s", knownEntities())
		}
		if _, ok := actionNames[c.Redaction.Actions[name]]; !ok {
			v.fail(path, "must be one of %s; got %q", knownActions(), c.Redaction.Actions[name])
		}
	}
	if c.Redaction.MinConfidence < 0 || c.Redaction.MinConfidence > 1 {
		v.fail("redaction.min_confidence", "must be in [0,1]; got %v", c.Redaction.MinConfidence)
	}
	if c.Redaction.MaskKeepLast < 0 {
		v.fail("redaction.mask_keep_last", "must not be negative")
	}
	// A hash action with no salt is recoverable by dictionary attack, so it is
	// rejected rather than warned about: a warning in a log nobody reads is not
	// a control.
	usesHash := c.Redaction.DefaultAction == "hash"
	for _, a := range c.Redaction.Actions {
		if a == "hash" {
			usesHash = true
		}
	}
	if usesHash && c.Redaction.HashSalt == "" {
		v.fail("redaction.hash_salt", "must be set when any entity type uses the hash action, "+
			"otherwise the digest is reversible with a word list")
	}
}

func (c *Config) validateCache(v *validator) {
	if !c.Cache.Enabled {
		return
	}
	if c.Cache.MaxEntries <= 0 {
		v.fail("cache.max_entries", "must be positive")
	}
	if c.Cache.TTL <= 0 {
		v.fail("cache.ttl", "must be positive")
	}
	if c.Cache.SweepInterval < 0 {
		v.fail("cache.sweep_interval", "must not be negative")
	}
	if !c.Cache.Semantic {
		return
	}
	if c.Cache.EmbeddingDimensions <= 0 {
		v.fail("cache.embedding_dimensions", "must be positive when semantic caching is on")
	}
	if c.Cache.SimilarityThreshold <= 0 || c.Cache.SimilarityThreshold > 1 {
		v.fail("cache.similarity_threshold", "must be in (0,1]; got %v", c.Cache.SimilarityThreshold)
	}
	if c.Cache.SimilarityThreshold > 0 && c.Cache.SimilarityThreshold < 0.90 {
		// The measured false-hit rate on the fixture pairs is 0/20 down to
		// 0.90 and 3/20 at 0.85. Below 0.90 the cache starts answering
		// questions nobody asked, silently.
		v.fail("cache.similarity_threshold", "%v is below 0.90, where the measured false-hit rate on the "+
			"fixture pairs stops being zero; set it higher or plug in an embedder you have measured yourself",
			c.Cache.SimilarityThreshold)
	}
}

func (c *Config) validateProviders(v *validator) map[string]bool {
	seen := map[string]bool{}
	if len(c.Providers) == 0 {
		v.fail("providers", "at least one provider is required")
	}
	for i, p := range c.Providers {
		path := fmt.Sprintf("providers[%d]", i)
		switch {
		case p.Name == "":
			v.fail(path+".name", "must not be empty")
		case seen[p.Name]:
			v.fail(path+".name", "duplicate provider name %q", p.Name)
		default:
			seen[p.Name] = true
		}
		if p.Type != "local" {
			v.fail(path+".type", "unknown provider type %q; this build implements \"local\" only", p.Type)
		}
		if p.ContextTokens < 0 {
			v.fail(path+".context_tokens", "must not be negative")
		}
		if p.MaxOutputTokens < 0 {
			v.fail(path+".max_output_tokens", "must not be negative")
		}
		if p.Latency.Jitter < 0 || p.Latency.Jitter > 1 {
			v.fail(path+".latency.jitter", "must be a fraction in [0,1]; got %v", p.Latency.Jitter)
		}
		if p.Latency.TimeToFirstToken < 0 || p.Latency.PerToken < 0 {
			v.fail(path+".latency", "must not be negative")
		}
		if p.Failure.Code != "" {
			if !knownErrorCode(p.Failure.Code) {
				v.fail(path+".failure.code", "unknown error code %q; known codes are %s", p.Failure.Code, knownCodes())
			}
			if p.Failure.Rate <= 0 || p.Failure.Rate > 1 {
				v.fail(path+".failure.rate", "must be in (0,1] when a failure code is set; "+
					"a code with rate 0 never fires and is almost certainly a mistake")
			}
		}
	}
	return seen
}

func (c *Config) validateRoutes(v *validator, providers map[string]bool) map[string]bool {
	seen := map[string]bool{}
	if len(c.Routes) == 0 {
		v.fail("routes", "at least one route is required")
	}
	for i, r := range c.Routes {
		path := fmt.Sprintf("routes[%d]", i)
		switch {
		case r.Model == "":
			v.fail(path+".model", "must not be empty")
		case seen[r.Model]:
			v.fail(path+".model", "duplicate route for model alias %q", r.Model)
		default:
			seen[r.Model] = true
		}
		switch r.Strategy {
		case StrategyPriority, StrategyRoundRobin, StrategyWeighted:
		default:
			v.fail(path+".strategy", "must be one of priority, round_robin, weighted; got %q", r.Strategy)
		}
		if len(r.Targets) == 0 {
			v.fail(path+".targets", "must list at least one target")
		}
		for j, t := range r.Targets {
			tp := fmt.Sprintf("%s.targets[%d]", path, j)
			if t.Provider == "" {
				v.fail(tp+".provider", "must not be empty")
			} else if !providers[t.Provider] {
				v.fail(tp+".provider", "no provider named %q is configured", t.Provider)
			}
			if t.Weight < 0 {
				v.fail(tp+".weight", "must not be negative")
			}
			if r.Strategy == StrategyWeighted && t.Weight == 0 {
				v.fail(tp+".weight", "must be positive under the weighted strategy, "+
					"otherwise this target is configured to receive no traffic and should be removed")
			}
		}
		c.validateRetry(v, path+".retry", r.Retry)
		c.validateBreaker(v, path+".breaker", r.Breaker)
	}
	return seen
}

func (c *Config) validateRetry(v *validator, path string, r Retry) {
	if r.MaxAttempts < 1 {
		v.fail(path+".max_attempts", "must be at least 1; 1 disables retry")
	}
	if r.MaxAttempts > 10 {
		v.fail(path+".max_attempts", "%d attempts against one target is load amplification, not resilience", r.MaxAttempts)
	}
	if r.MaxAttempts > 1 {
		if r.BaseDelay <= 0 {
			v.fail(path+".base_delay", "must be positive when retries are enabled")
		}
		if r.MaxDelay < r.BaseDelay {
			v.fail(path+".max_delay", "must be at least base_delay (%s)", r.BaseDelay)
		}
		if r.Multiplier < 1 {
			v.fail(path+".multiplier", "must be at least 1; a shrinking backoff retries faster than it failed")
		}
	}
	if r.Jitter < 0 || r.Jitter > 1 {
		v.fail(path+".jitter", "must be a fraction in [0,1]; got %v", r.Jitter)
	}
}

func (c *Config) validateBreaker(v *validator, path string, b Breaker) {
	if b.FailureThreshold < 1 {
		v.fail(path+".failure_threshold", "must be at least 1")
	}
	if b.SuccessThreshold < 1 {
		v.fail(path+".success_threshold", "must be at least 1")
	}
	if b.OpenFor <= 0 {
		v.fail(path+".open_for", "must be positive; a circuit that reopens instantly is not a circuit breaker")
	}
	if b.HalfOpenProbes < 1 {
		v.fail(path+".half_open_probes", "must be at least 1, otherwise an open circuit never recovers")
	}
}

func (c *Config) validateTeams(v *validator) {
	seen := map[string]bool{}
	for i, t := range c.Teams {
		path := fmt.Sprintf("teams[%d]", i)
		switch {
		case t.ID == "":
			v.fail(path+".id", "must not be empty")
		case seen[t.ID]:
			v.fail(path+".id", "duplicate team %q", t.ID)
		default:
			seen[t.ID] = true
		}
		c.validateBudget(v, path+".budget", t.Budget)
	}
}

func (c *Config) validateKeys(v *validator, routes map[string]bool) {
	ids := map[string]bool{}
	secrets := map[string]bool{}
	if len(c.Keys) == 0 {
		v.fail("keys", "at least one key is required; a gateway with no keys accepts nothing")
	}
	for i, k := range c.Keys {
		path := fmt.Sprintf("keys[%d]", i)
		switch {
		case k.ID == "":
			v.fail(path+".id", "must not be empty")
		case ids[k.ID]:
			v.fail(path+".id", "duplicate key id %q", k.ID)
		default:
			ids[k.ID] = true
		}
		switch {
		case k.Secret == "":
			v.fail(path+".secret", "must not be empty")
		case len(k.Secret) < 16:
			v.fail(path+".secret", "is %d characters; use at least 16 so that it is not guessable", len(k.Secret))
		case secrets[k.Secret]:
			// Two keys sharing a secret makes attribution impossible and
			// revocation ambiguous, and the audit log would name whichever one
			// happened to be first in the file.
			v.fail(path+".secret", "duplicate secret; two keys sharing a secret cannot be told apart in the audit log")
		default:
			secrets[k.Secret] = true
		}
		if k.Team != "" {
			if _, ok := c.Team(k.Team); !ok {
				v.fail(path+".team", "no team named %q is configured", k.Team)
			}
		}
		for j, m := range k.Models {
			if !routes[m] {
				v.fail(fmt.Sprintf("%s.models[%d]", path, j), "no route for model alias %q", m)
			}
		}
		if k.RateLimit.RequestsPerMinute < 0 {
			v.fail(path+".rate_limit.requests_per_minute", "must not be negative")
		}
		if k.RateLimit.TokensPerMinute < 0 {
			v.fail(path+".rate_limit.tokens_per_minute", "must not be negative")
		}
		if !k.RateLimit.Zero() && k.RateLimit.Burst < 1 {
			v.fail(path+".rate_limit.burst", "must be at least 1; a bucket shallower than its refill rate "+
				"rejects traffic that is within the configured rate")
		}
		c.validateBudget(v, path+".budget", k.Budget)
		if k.Budget.OnExceed == OnExceedDegrade && k.Budget.DegradeTo != "" && !routes[k.Budget.DegradeTo] {
			v.fail(path+".budget.degrade_to", "no route for model alias %q", k.Budget.DegradeTo)
		}
	}
}

func (c *Config) validateBudget(v *validator, path string, b Budget) {
	if b.Limit < 0 {
		v.fail(path+".limit", "must not be negative")
	}
	if b.Zero() {
		return
	}
	if b.Window <= 0 {
		v.fail(path+".window", "must be positive when a limit is set")
	}
	switch b.OnExceed {
	case OnExceedReject:
	case OnExceedDegrade:
		if b.DegradeTo == "" {
			v.fail(path+".degrade_to", "must name a cheaper route alias when on_exceed is degrade")
		}
	default:
		v.fail(path+".on_exceed", "must be reject or degrade; got %q", b.OnExceed)
	}
}

func (c *Config) validatePricing(v *validator) {
	names := make([]string, 0, len(c.Pricing))
	for name := range c.Pricing {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := "pricing." + name
		p := c.Pricing[name]
		if p.InputPerMillion < 0 || p.OutputPerMillion < 0 || p.CachedInputPerMillion < 0 {
			v.fail(path, "prices must not be negative")
		}
	}
}

func knownActions() string { return joinKeys(actionNames) }

func knownEntities() string { return joinKeys(entityTypes) }

func joinKeys[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

var errorCodes = []llm.ErrorCode{
	llm.CodeRateLimited, llm.CodeContextLengthExceeded, llm.CodeContentFiltered,
	llm.CodeAuthentication, llm.CodeProviderUnavailable, llm.CodeInvalidRequest,
	llm.CodeTimeout, llm.CodeUnknown,
}

func knownErrorCode(s string) bool {
	for _, c := range errorCodes {
		if string(c) == s {
			return true
		}
	}
	return false
}

func knownCodes() string {
	out := make([]string, len(errorCodes))
	for i, c := range errorCodes {
		out[i] = string(c)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
