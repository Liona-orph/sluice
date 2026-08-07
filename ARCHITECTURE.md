# Architecture

This document explains how a request moves through Sluice, why the stages are in the order
they are, and what happens when each part fails. It assumes you have read the README.

The short version: Sluice is a single process with four jobs — know the cost, keep personal
data in, stop paying twice, stay available — and every design decision here follows from
doing all four in one place rather than badly in twenty.

---

## Package layout

```
pkg/llm              provider-agnostic vocabulary: Request, Response, Chunk, Usage,
                     Cost, the error taxonomy, the pricing table, the tokenizer
pkg/provider/local   a deterministic offline llm.Provider

internal/redact      10 PII detectors, reversible tokenization, streaming in both
                     directions
internal/cache       exact-match and semantic caching
internal/config      the declarative deployment description, and its validation
internal/policy      token-bucket rate limiting, rolling-window budgets
internal/router      aliases, targets, retry, failover, circuit breakers
internal/audit       append-only JSON Lines record
internal/telemetry   slog and Prometheus, with the cardinality rule
internal/gateway     the pipeline: assembles all of the above
internal/server      HTTP: OpenAI-compatible surface, SSE, the dashboard
cmd/sluice           serve, replay, version
```

The dependency direction is strict and one-way: `pkg/llm` knows about nothing;
`internal/config` knows about `pkg/llm` and `internal/redact` and nothing that consumes it;
`internal/gateway` knows about everything below it; `internal/server` knows only about
`internal/gateway`. Nothing below `internal/gateway` imports anything above it, which is
what makes each piece testable on its own and what stops a new knob from creating an import
cycle.

---

## The pipeline, stage by stage

```
authenticate -> rate limit -> budget -> redact -> cache -> route -> call
             -> un-redact -> account -> audit
```

Each stage is an exported method on `*gateway.Gateway`, so each has a name, a test, and a
place in `Complete` where a reader can see it happen. The streaming path shares stages 1–5
verbatim (`prepare`) so that the two entry points cannot drift on anything to do with
authentication, limits, budgets or redaction.

### 1. Authenticate

A bearer token, SHA-256'd, looked up in a map keyed on the digest. Hashing before the
lookup means the table compares fixed-size arrays rather than strings, so the comparison
time does not depend on how many leading bytes of a guess were right, and a heap dump
contains digests rather than a list of valid credentials.

It goes first because nothing downstream can be attributed without a principal, and because
it is the cheapest possible rejection: one hash and one map lookup against a request that
has not yet cost anything.

**Fails:** 401, and the request is still audited. An audit log that only records successes
cannot answer "who tried".

### 2. Rate limit

Two token buckets per key: requests per minute and tokens per minute. Both, because they
bound different things. A request limit bounds concurrency and protects the upstream; it
does not bound spend, because one request with a 200k-token context costs more than a
thousand short ones. A token limit bounds the spend *rate* directly.

The buckets refill continuously rather than resetting on a fixed window, because a fixed
window lets a caller spend the whole allowance in the last instant of one window and again
in the first instant of the next — twice the configured rate across the boundary, exactly
when an upstream is least able to absorb it.

The token charge is an estimate made *before* the call: prompt tokens from the tokenizer,
plus `max_tokens`, or a 512-token floor when `max_tokens` is unset. It has to be an
estimate, because a limiter that only learns the cost afterwards cannot refuse anything.
The 512 floor is deliberately not zero: assuming zero would let any client evade the token
limit entirely by omitting `max_tokens`, which is the default in every client library.
`Settle` reconciles against the measured usage once the response exists, so the limiter
does not drift in the direction of the tokenizer's known 0.68 % underestimate.

It runs before the budget check because both are rejections and this one is cheaper, and
because a client in a hot loop should be stopped by the mechanism designed for hot loops —
so the error it gets says "slow down" and carries a `Retry-After` rather than "you are out
of money".

**Fails:** 429 with `Retry-After` computed from the deficit and the refill rate. A request
larger than the whole bucket is a permanent rejection with no `Retry-After`, because no
amount of waiting will make it fit.

### 3. Budget

Rolling-window spend, checked against the key's budget and then the team's, with the
stricter outcome winning and the key checked first so a caller who has exhausted their own
allowance is told so rather than being told their team is out of money.

Rolling rather than calendar, because a calendar month resets at midnight UTC and invites a
queue of deferred work to stampede the moment it does. The window is divided into 120 slots
and whole slots expire; an exact rolling window would need every individual charge retained
until it aged out, which is unbounded memory in the one place a gateway cannot afford it.
The approximation bounds memory at 120 int64s per subject and bounds the error at one
slot's width — twelve minutes on a 24-hour window, far finer than the granularity at which
anyone sets a daily limit.

Spend is *recorded after* the response, not reserved before it. Reserving needs an estimate
of the output length, which nobody has before generation, and the error would be
systematic. The cost of recording afterwards is a bounded overshoot: a burst of concurrent
requests can exceed the limit by roughly one burst's worth of spend before any of them is
refused.

**At the limit** there are two configured behaviours and Sluice implements both:

- `reject` — HTTP 402 and the request does not happen. This is the **default**, because a
  budget that silently changes the model changes the answers an application gets, and an
  application that starts getting worse answers with no error to correlate against
  generates a support ticket nobody can close. A 402 is loud, attributable and immediately
  actionable.
- `degrade` — the model alias is rewritten to a cheaper route and the request proceeds. It
  is right where availability outranks fidelity (a background summariser, an internal
  tool), and it is only safe because the change is observable: the response reports the
  model that actually answered, the `X-Sluice-Degraded` header is set, and the audit record
  carries `degraded: true`.

Degrading to the model already in use becomes a rejection, because it would otherwise be a
no-op that silently removed the limit.

Budget must run before the cache, not after, because a degrade decision rewrites the model
alias and the cache namespace *is* the alias.

**Fails:** 402 naming the subject, the spend, the limit and when the window rolls.

### 4. Redact

Ten detectors run over every message. Each detector that can validate its own findings
does; the Luhn check removes about 96 % of what the credit-card pattern matches. Overlaps
resolve longest-first, then by detector priority, so a nine-digit run inside a card number
is the card number and an IBAN is not misclassified as a high-entropy credential.

The policy decides what happens per entity type: tokenize (reversible), mask
(irreversible, shape-preserving), hash (irreversible, stable), or allow.

**Fails:** it does not. Detection is pure computation over a string. A pathological input
costs CPU bounded by the input size; the fuzz corpus exists to keep that true.

### 5. Cache

Two lookups. The exact-match cache is keyed on `Key{Namespace: model, Hash:
request.Fingerprint()}` and is unambiguously safe: identical request, identical answer. The
semantic cache — off unless an embedder is configured — embeds the prompt and returns the
nearest stored entry above a threshold, subject to the namespace and every non-message
request parameter matching exactly.

Responses finishing with `length` or `content_filter` are never stored. A truncated answer
served from cache is a defect that outlives the request that caused it.

#### Why redaction comes first

Three reasons, in increasing order of importance.

1. Cache entries are stored redacted, so a long-lived in-memory store — and a shared one in
   any future version — never holds personal data.
2. Placeholders are allocated deterministically per request: the first email is always
   `[SLUICE_EMAIL_0001]`. Two customers asking the same question about their own different
   addresses therefore produce the *identical* redacted prompt and share one cache entry.
   Redacting after the cache would make every such request a miss.
3. Decisively: a cache hit skips the provider call. If redaction ran after the cache it
   would not run at all on a hit, and the response served would have been built from
   someone else's unredacted text.

The consequence has to be stated because it is real: two callers whose prompts differ only
in the redacted values get the same cached answer, each with their own values substituted
back in. That is correct — it is the entire point of a reversible placeholder — but it
means the cache is shared across tenants by design. A deployment needing per-tenant
isolation must namespace the cache, and this build does not.

**Fails:** a lookup error is logged and treated as a miss. Failing a request because an
optimisation broke would trade availability for nothing.

### 6–7. Route and call

See "Routing and failover" below.

### 8. Un-redact

The cache holds the redacted response; restoration happens per caller, from that caller's
own vault. Restoration is exact rather than nearly exact — see "The redaction round trip".

### 9. Account

Cost is `price(model) × usage`, in integer nanodollars. A gateway sums millions of
per-request costs into a monthly invoice, and float64 accumulation of values around 1e-6
loses cents at that scale.

A cache hit costs **zero**, not the cost of the call that filled it. Reporting the original
cost would double-count money that was spent once; the saving shows up instead as the gap
between the hit rate and the spend curve.

An unpriced model is a warning and a zero, not a failed request — otherwise adding a model
to the routing table becomes a two-step deploy and operators route around it. The audit
record carries the token counts either way, so a price discovered later can be applied
retrospectively with `sluice replay`.

### 10. Audit

One append-only JSON Lines record per request, written after the response — including when
the response ended because the client disconnected, because those tokens were generated and
paid for whether or not anyone read them.

The prompt and completion are stored **redacted**. See
[ADR 0006](docs/adr/0006-redacted-prompts-in-the-audit-log.md).

**Fails:** a write error is logged, counted in `sluice_audit_records_dropped_total`, and
the request still succeeds. A gateway must not fail a paid-for request because its log is
full — but it must not pretend the record was written either, which is why the counter
exists and why any non-zero value is an incident.

---

## The redaction round trip

The feature is not "remove personal data"; it is "remove it and put it back", because a
gateway that turns `alice@example.com` into `[REDACTED]` makes the model's reply useless.

```
prompt      "Email alice@example.com about the invoice"
   |  detect + tokenize, recording value -> placeholder in a Vault
   v
upstream    "Email [SLUICE_EMAIL_0001] about the invoice"
   |  the model writes new text around a token it does not understand
   v
response    "I have drafted a note to [SLUICE_EMAIL_0001]."
   |  restore, from this caller's vault
   v
client      "I have drafted a note to alice@example.com."
```

Three things make the round trip exact rather than approximate.

**Reserved placeholders.** If a prompt already contains `[SLUICE_EMAIL_0001]` — pasted from
a previous redaction, or because someone is probing — and the vault then allocated that
same placeholder for a real address, restoration would replace both occurrences and the
caller would get back text they never sent. Sluice scans the source text first and skips
reserved indices, removing the collision instead of making it unlikely.

**A loose matcher on the way back.** Models lowercase things, insert spaces inside brackets
and swap underscores for hyphens while rewriting text they do not understand. A strict
matcher loses the restoration in exactly those cases, and a lost restoration means the
caller receives a placeholder in their answer. The pattern therefore accepts
`[ sluice-email-1 ]` as well as `[SLUICE_EMAIL_0001]`. This costs nothing, because a string
of that shape that is not in the vault is left alone.

**Unknown placeholders are left alone.** The model may have invented one. Inventing a value
to go with it would be worse than returning the token.

### The streaming boundary problem

This is the case that leaks in production and never in a demo.

**Upward (redaction).** An email address split as `"ali"` + `"ce@example.com"` contains no
email address in *either* chunk. Redacting chunk by chunk emits the first half in the clear
and then redacts nothing — worse than not redacting at all, because the operator believes
it is working.

The fix is a bounded look-behind. `StreamRedactor` holds back the last `lookAhead` bytes of
everything it has seen, where `lookAhead` is the longest match any configured detector can
produce (`Detector.MaxLen`, 254 bytes with the defaults, set by the maximum length of an
email address). Detection always runs over the *whole* retained buffer, so it sees maximum
context, and only matches ending more than `lookAhead` from the end are emitted. A match
that could still grow with the next chunk is, by construction, one that ends within
`lookAhead` of the end.

The cost: output lags input by at most `lookAhead` bytes plus one straddling match; the
buffer never exceeds roughly twice `lookAhead`; and every `Write` re-runs every detector
over the buffer. That last one is the expensive part — 3.00 ms against 223 µs for the same
1,085 bytes delivered in one call. The fix is incremental matching and it is not written;
13× on a millisecond, amortised across the seconds a completion takes, has not yet been
worth the risk of getting subtle rescanning logic wrong.

**Downward (restoration).** The mirror image: a model emits `[SLUICE_EMAIL` in one chunk
and `_0001] is the address` in the next. Restoring chunk by chunk finds no placeholder in
either, and a client displaying the stream shows the token and never corrects it.

The fix is much cheaper, because a placeholder always begins with `[`. `StreamRestorer`
holds back only a trailing run starting at the last unclosed bracket; everything before it
is decidable now. In the overwhelmingly common case there is no `[` in the tail at all and
the chunk passes through untouched, which is why streaming restoration costs 19 µs where
streaming redaction costs 3 ms.

Both wrappers hold the terminating chunk (finish reason and usage) back so that the flushed
tail has somewhere to go, rather than arriving after a client has already seen the stream
end. Both are lazy sequences that run entirely on the consumer's goroutine, so abandoning
one leaks nothing — asserted by `internal/leaktest`, not by inspection.

**What is not solved:** tool-call arguments are restored per fragment, not across
fragments. Doing it properly means buffering a whole argument document, which defeats the
reason arguments are streamed. A placeholder split across two argument fragments is
delivered unrestored. Callers who need exact tool arguments should use the buffered
endpoint, where reassembly happens first.

---

## The semantic cache and its risk

The exact cache saves money on repeated questions. The semantic cache saves money on
*similar* questions, which is where the volume is — and where the danger is.

**A false cache hit is a confidently wrong answer to a question nobody asked, and it is
silent.** No error, no retry, no alert. Just a response that does not correspond to the
prompt. That is a qualitatively different failure from a provider outage, and it is why
four things bound it:

1. **It is off by default.** The safe behaviour is the one you get without deciding.
2. **The threshold is measured, not guessed.** 0.97, against a fixture set where the
   false-hit rate is 0/20 down to 0.90 and 3/20 at 0.85. The closest non-equivalent pair
   scores 0.879, so the default sits nine points above the first wrong answer. The config
   validator refuses anything below 0.90.
3. **Approximation applies to *what* was asked, never to *how*.** A semantic hit requires
   the namespace and every non-message request parameter to match exactly. An answer
   generated with `max_tokens: 50` is not an acceptable answer to a request for 2000; an
   answer generated without tools cannot satisfy a request that declares them.
4. **The default embedder fails in the safe direction.** It is the hashing trick over word
   unigrams, word bigrams and character n-grams — surface similarity, not meaning. "Cancel
   my subscription" and "how do I unsubscribe" score low, so the failure it produces is a
   missed hit, which costs money, rather than a wrong hit, which costs correctness.

Word order is represented explicitly, by bigrams and by n-grams taken across the whole
normalised string rather than within each word. That is not a refinement, it is a
correctness requirement: a pure bag-of-words vector scores "Convert 10 USD to EUR" and
"Convert 10 EUR to USD" at exactly 1.0, and a cache that treats those as the same question
returns a confidently reversed answer.

A real embedding model has a differently shaped similarity distribution and needs its own
threshold measured the same way. That is why the threshold is a knob and not a constant.

---

## Routing and failover

Four mechanisms compose, and the composition order is the design.

**Selection.** A model alias maps to an ordered list of provider/model targets. The
strategy decides the order for one request:

- `priority` — always the first healthy target. Correct when the list is ordered by
  preference (cheapest first, or primary then standby), which is what a failover list
  usually is. The default.
- `round_robin` — even spread. Correct when targets are interchangeable and the goal is to
  stay inside several providers' rate limits at once.
- `weighted` — proportional to weight, for a deliberate split: 90 % to a committed-spend
  contract, 10 % to a second vendor kept warm so the failover path is exercised before it
  is needed rather than during an incident.

**Retry.** Re-attempt the same target while the error is retryable and the caller's
deadline allows. Exponential backoff, `base × multiplier^(attempt-1)`, capped, then
jittered — without jitter a thundering herd retries in lockstep and re-creates the overload
it is backing off from.

The provider's `Retry-After` wins over the computed backoff, and it wins even when it
exceeds `max_delay`: retrying sooner than a provider that is actively throttling you asked
is how a rate limit becomes a ban. Jitter is applied upward only, for the same reason. If
the wait would outlast the caller's deadline the router does not sleep at all — it returns
immediately so the caller can fail over to something that might answer in the time
remaining.

**Failover.** Move to the next target when — and only when — the error code says a
different provider might do better. `llm.ErrorCode` draws that line and the router does not
second-guess it. A malformed request is malformed at every provider; a content-filter
refusal is deliberately *not* failover-worthy, because shopping the same prompt around
until someone accepts it is a policy decision an operator must make explicitly.

**Circuit breaker,** per target, three states:

| State | Behaviour | Leaves when |
|---|---|---|
| closed | pass traffic | 5 consecutive unhealthy failures |
| open | refuse without calling | 30 s elapsed → half-open |
| half-open | at most 1 concurrent probe | 2 consecutive successes → closed; 1 failure → open |

Consecutive failures rather than a failure rate, because a rate needs a window long enough
to be statistically meaningful and any such window is longer than an operator wants to keep
feeding a dead upstream — and the gateway already has somewhere else to send the traffic.
Thirty seconds is long enough for a restarting upstream to have restarted and short enough
that a blip does not remove a provider for a noticeable fraction of an incident. One probe,
because a probe that fails costs one request's latency and a hundred concurrent probes cost
a hundred and arrive as a spike. Two successes to close, because one success is weak
evidence and two consecutive ones through the real path are slightly less weak.

Only errors that say something about the *target's* health count: unavailable, timeout,
rate-limited, authentication. A malformed request is a fact about the request, and letting
it open a circuit would let one bad client remove a healthy provider for everyone else.

### The rule that overrides everything: never fail over a partial stream

A stream that has already emitted a chunk to the client is never retried and never failed
over. The failure is delivered to the caller as-is.

The reason is not that a chat completion has side effects upstream — it usually does not.
The side effect is *downstream*, in the client's buffer. Some prefix of the answer has
already been printed on someone's screen. Restarting on another provider produces a second,
different answer, which the client concatenates onto the first, and the result is a
response that contradicts itself in the middle with no marker of where the seam is. That is
worse than the error: an error is handled, a silently spliced answer is believed.

`Router.Stream` therefore tracks whether anything has been emitted and refuses to recover
once it has. `TestStreamNeverFailsOverAfterFirstToken` is what keeps that true.

Failure *before* the first chunk is a different case entirely: nothing has reached the
client, the request is repeatable, and failover is exactly right. The router opens the
first workable stream eagerly — so an HTTP handler that has not written a status line yet
can still write a JSON error instead of a half-finished event stream — and continues to
fail over inside the iterator for a sequence that errors on its first element.

---

## Telemetry and the cardinality rule

A Prometheus time series exists for every distinct combination of label values, and it
lives in the server's memory until someone deletes it. Labelling by API key looks harmless
with ten keys and takes the monitoring stack down at ten thousand; labelling by prompt,
user ID or request ID does it immediately. There is no runtime guard, because by the time
the cardinality is visible the damage is a heap full of series.

So the rule is structural: every label value comes from a config file an operator wrote
(provider, model, route alias) or from a closed enumeration in the source (error code,
cache result, breaker state, budget action, entity type, pipeline stage). `AllowedLabels`
states that rule as data and `TestNoUnboundedLabelCardinality` gathers the live registry
and fails the build if a metric carries a label outside it — or if the total series count
after a small synthetic workload exceeds a bound, which catches a label that is allowed but
misused.

The identifiers that cannot be labels still belong in the record of what happened. They go
to the structured log and to the audit record, both of which are append-only streams that
cost disk rather than resident memory.

Latency buckets run to two minutes. The Prometheus defaults stop at ten seconds, which puts
every long completion in `+Inf` and makes p99 unanswerable — and the p99 of a model call is
the number anyone actually asks about.

The dashboard does not read Prometheus. It reads `/v1/stats`, which is assembled from the
same structures that *enforce* the rules: the budget ledger, the cache's own counters, the
router's breakers. A dashboard that can disagree with the enforcement will, at the worst
possible moment.

---

## Failure modes

| What fails | What happens |
|---|---|
| One provider returns 5xx or times out | Retry with backoff up to `max_attempts`, then fail over to the next target. Breaker records each attempt. |
| One provider fails 5 times running | Circuit opens for 30 s. Subsequent requests skip it entirely and go to the next target with no upstream call and no timeout. |
| Every target's circuit is open | 502 immediately rather than a slow timeout. `/readyz` returns 503, which is the one condition where removing this instance from a load balancer helps. |
| A provider fails mid-stream | The error reaches the client. No retry, no failover, no splice. Whatever the redaction look-behind was holding is flushed first, so a truncated answer stays a truncated answer rather than becoming a lost one. |
| Client disconnects mid-stream | The range loop ends, the gateway settles: usage accounted, cost recorded, audit written. Nothing leaks — no goroutine was started. |
| Provider rate-limits the gateway | `Retry-After` is honoured exactly. If it outlasts the caller's deadline the router fails over instead of waiting. |
| A key exceeds its rate limit | 429 with a computed `Retry-After`. The token bucket refunds the request token so the two limits stay independent. |
| A key or team exceeds its budget | 402, or a degrade to the configured cheaper alias with `X-Sluice-Degraded` set and `degraded: true` in the audit record. |
| The cache errors | Logged, counted, treated as a miss. |
| The embedder errors | Same. The exact cache still works. |
| A model has no price | Logged, cost recorded as zero, request served. Token counts are still recorded so `sluice replay` can price it later. |
| The audit log cannot be written | Logged, `sluice_audit_records_dropped_total` incremented, request still served. Any non-zero value there is an incident. |
| The config is invalid | The process refuses to start and prints every problem with the path of the field that caused it. |
| A provider invents a placeholder | Left in the response verbatim. Inventing a value to go with it would be worse. |
| SIGTERM during a stream | Graceful shutdown waits up to `shutdown_grace`. An in-flight stream has already been paid for upstream, so cutting it off wastes the money and gives the client nothing. |
| Two Sluice instances behind a load balancer | Each enforces its own half of every limit. This is a real limitation, not a failure mode; see "What this is not" in the README. |
