# Sluice

An LLM gateway in Go that measures what you spend, strips personal data out of prompts before they leave the building, stops paying twice for the same answer, and keeps working when a provider does not.

[![CI](https://github.com/Liona-orph/sluice/actions/workflows/ci.yml/badge.svg)](https://github.com/Liona-orph/sluice/actions/workflows/ci.yml)
[![CodeQL](https://github.com/Liona-orph/sluice/actions/workflows/codeql.yml/badge.svg)](https://github.com/Liona-orph/sluice/actions/workflows/codeql.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Liona-orph/sluice.svg)](https://pkg.go.dev/github.com/Liona-orph/sluice)
[![Go 1.23](https://img.shields.io/badge/go-1.23-00ADD8?logo=go&logoColor=white)](https://go.dev/dl/)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Point an existing OpenAI client at it and nothing in your application changes.

---

## Sixty-second quickstart, entirely offline

No API key, no network, no Docker. `sluice serve` with no config file runs a complete
deployment against the built-in [local provider](pkg/provider/local): two upstreams, three
route aliases, one demo key.

```sh
git clone https://github.com/Liona-orph/sluice && cd sluice
go build -o sluice ./cmd/sluice
# --audit writes the log to a file; the default is stdout, which is what a
# container wants and not what the replay step below wants.
./sluice serve --audit audit.jsonl &
```

Send it something with personal data in it. The output below is real: it was produced by
running exactly these commands against this commit.

```sh
curl -s localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-sluice-local-demo' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user",
       "content":"Email alice@example.com about card 4111 1111 1111 1111."}]}'
```

```json
{"id":"local-primary-79f8895d44990106","object":"chat.completion","created":1786317969,
 "model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant",
 "content":"Regarding \"Email alice@example.com about card ***************1111.\": Observability is the difference between a bill and an explanation."},
 "finish_reason":"stop"}],
 "usage":{"prompt_tokens":28,"completion_tokens":44,"total_tokens":72},
 "system_fingerprint":"local-primary"}
```

The address came back. The card number did not, because the default policy tokenizes
contact details reversibly and masks payment instruments irreversibly. Sluice's own
metadata rides in headers, so the JSON body stays exactly the shape an OpenAI client
expects:

```
X-Sluice-Cache: miss
X-Sluice-Cost-Usd: 0.000030600
X-Sluice-Model: gpt-4o-mini
X-Sluice-Provider: local-primary
X-Sluice-Redactions: credit_card=1,email=1
X-Sluice-Request-Id: req_79eb38c74f951d7f6d7b6583
```

Send the identical request again:

```
X-Sluice-Cache: exact
X-Sluice-Cost-Usd: 0.000000000
```

Streaming works the same way, and the un-redaction spans the provider's chunk boundaries —
`Sarah Chen` arrives in a different chunk from the placeholder that stood in for her:

```sh
curl -sN localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer sk-sluice-local-demo' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user",
       "content":"Text Sarah Chen on +1 415 555 0142."}]}'
```

```
data: {"id":"local-standby-45a6e46f51ee389d","object":"chat.completion.chunk",...,"delta":{"role":"assistant","content":"Regarding \"Text "},...}

data: {"id":"local-standby-45a6e46f51ee389d","object":"chat.completion.chunk",...,"delta":{"content":"Sarah Chen on "},...}
```

Now look at what was written to the audit log for that request. The prompt is stored
**redacted**, and so is the completion — see [ADR 0006](docs/adr/0006-redacted-prompts-in-the-audit-log.md):

```json
{"schema":1,"id":"req_4e114c0ec2bb3f5f58a8f329","time":"2026-08-09T23:56:30.372248531Z",
 "key_id":"demo-key","team":"demo","requested_model":"gpt-4o-mini","served_model":"gpt-4o-mini",
 "provider":"local-standby","stream":true,"attempts":1,
 "usage":{"input_tokens":30,"output_tokens":57},
 "cost_nanodollars":38700,"cost_dollars":0.0000387,
 "redaction_counts":{"person_name":1,"phone":1},"latency_ms":0.384083,
 "prompt":[{"role":"user","content":"Text [SLUICE_PERSON_NAME_0001] on [SLUICE_PHONE_0001]."}],
 "completion":"Regarding \"Text [SLUICE_PERSON_NAME_0001] on [SLUICE_PHONE_0001].\": The answer depends on which model served the request. ..."}
```

And the question that log exists to answer — *what would this have cost on the other model?*

```sh
./sluice replay --audit audit.jsonl --as gpt-4o
```

```
MODEL        REQS  SERVED  HITS  ERRS  IN  OUT  RECORDED   REPLAY     DELTA       SAVED BY CACHE
gpt-4o-mini  3     2       1     0     86  145  $0.000069  $0.001155  +$0.001085  $0.000510

TOTAL        3     2       1     0     86  145  $0.000069  $0.001155  +$0.001085  $0.000510

3 records, re-priced as "gpt-4o".
Token counts are held at their recorded values: this prices the same tokens differently,
it does not predict how many tokens another model would have produced.
```

Open <http://localhost:8080> and paste `sk-sluice-local-demo` for the dashboard.

> The `gpt-4o-mini` alias in the default config is answered by the local provider under that
> name, so the cost figures are OpenAI's published list prices applied to token counts that
> were really measured. The tokens are real; the money is simulated. Nothing reaches OpenAI.

---

## The request pipeline

```mermaid
flowchart LR
    C([Client]) -->|POST /v1/chat/completions| A

    subgraph GW["Sluice"]
        direction LR
        A[authenticate] --> RL[rate limit<br/><i>requests + tokens</i>]
        RL --> B[budget<br/><i>key then team</i>]
        B --> R[redact<br/><i>10 detectors</i>]
        R --> K{cache}
        K -->|hit| U[un-redact]
        K -->|miss| RT[route]
        RT --> P[provider call<br/><i>retry, failover, breaker</i>]
        P --> ST[(store redacted)]
        ST --> U
        U --> AC[account<br/><i>usage + cost</i>]
        AC --> AU[audit<br/><i>append-only</i>]
    end

    P -.-> X1[[upstream A]]
    P -.-> X2[[upstream B]]
    AU --> C

    classDef stage fill:#161b22,stroke:#253040,color:#e6edf3
    classDef term fill:#0e1116,stroke:#4cc2ff,color:#4cc2ff
    class A,RL,B,R,RT,P,U,AC,AU,ST stage
    class C,X1,X2 term
```

Each box is a named, individually testable method on `*gateway.Gateway`. The order is not
arbitrary and the reasons are in [ARCHITECTURE.md](ARCHITECTURE.md) and in the package
comment on [`internal/gateway`](internal/gateway/gateway.go). The one that looks like it
could go either way and cannot is **redaction before caching**: a cache hit skips the
provider call, so a redactor sitting after the cache would not run at all on a hit.

---

## What it does

| Capability | What you get | Where |
|---|---|---|
| **OpenAI-compatible API** | `/v1/chat/completions`, buffered and SSE. Existing SDKs work unchanged. Unsupported requests are refused, never approximated. | [`internal/server`](internal/server/openai.go) |
| **Cost accounting** | Integer nanodollars, per request, per key, per team. Provider-reported tokens where available, tokenizer estimate where not, and the record says which. | [`pkg/llm/pricing.go`](pkg/llm/pricing.go) |
| **PII redaction** | 10 detectors, checksum-validated where a checksum exists. Reversible tokenization by default, masking for payment instruments, salted hashing for credentials. | [`internal/redact`](internal/redact) |
| **Streaming-safe redaction** | A bounded look-behind in both directions, so an entity split across chunk boundaries is neither leaked on the way up nor left as a placeholder on the way down. | [`stream.go`](internal/redact/stream.go), [`restore.go`](internal/redact/restore.go) |
| **Caching** | Exact-match on a canonical request fingerprint, plus optional semantic lookup with a measured threshold. Truncated and filtered responses are never stored. | [`internal/cache`](internal/cache) |
| **Routing and failover** | Model aliases to ordered targets, failover only on failover-worthy error codes, per-target circuit breaker, exponential backoff with jitter honouring `Retry-After`, three load-balancing strategies. | [`internal/router`](internal/router) |
| **Budgets and quotas** | Per-key and per-team spend over a rolling window; at the limit, reject (default) or degrade to a cheaper model, and the audit record says which happened. | [`internal/policy`](internal/policy/budget.go) |
| **Rate limiting** | Token buckets on both requests per minute and tokens per minute, because a request limit alone does not bound spend. | [`internal/policy`](internal/policy/ratelimit.go) |
| **Audit log** | Append-only JSON Lines: who asked, what (redacted), which provider answered, what it cost, what was redacted, whether it was a cache hit. | [`internal/audit`](internal/audit) |
| **Telemetry** | `log/slog` plus Prometheus metrics with buckets sized for model latency and a test that fails the build if a metric grows an unbounded label. | [`internal/telemetry`](internal/telemetry) |
| **Dashboard** | One self-contained HTML page served from the binary. No build step, no npm, no CDN. | [`dashboard.html`](internal/server/dashboard.html) |
| **Replay** | Re-price a recorded audit log against a different model or a negotiated rate. | [`cmd/sluice/replay.go`](cmd/sluice/replay.go) |
| **Offline provider** | A deterministic provider that generates real text, streams it, simulates latency and fails in every error mode on request. The whole repository is testable with nothing installed. | [`pkg/provider/local`](pkg/provider/local) |

---

## The measured numbers

Every figure here is printed by a test in this repository. Run them yourself with
`make test` and read the `t.Log` output; `make bench` reproduces the timings.

**Tokenizer** — `pkg/llm`, `TestApproxAccuracy`, `TestApproxTokensPerWord`

| Measurement | Value |
|---|---|
| Mean absolute percentage error vs cl100k_base, 32 hand-derived fixtures | **0.68 %** |
| Worst single fixture | +1 token |
| Tokens per word on English prose | 1.213, against OpenAI's documented 1.333 — a ~9 % underestimate |

The underestimate is one-directional, so treat a Sluice cost report as a floor on spend.

**Redactor** — `internal/redact`, `TestDetectorAccuracy` over 57 labelled documents

| Entity | Precision | Recall |
|---|---|---|
| credit_card | 1.000 | 1.000 |
| iban | 1.000 | 1.000 |
| email | 1.000 | 1.000 |
| phone | 1.000 | 1.000 |
| ip_address | 1.000 | 1.000 |
| us_ssn | 1.000 | 1.000 |
| uk_nino | 1.000 | 1.000 |
| es_dni | 1.000 | 1.000 |
| api_key | 0.800 | 1.000 |
| **person_name** | **0.857** | **0.750** |

Validation is what makes those numbers possible. On 20,000 random 13–19 digit strings the
credit-card *pattern* accepts 100 %; the pattern plus Luhn plus an issuer-prefix check
accepts 4.21 % (`TestCreditCardValidationFalsePositiveRate`).

Person names are the honest weak spot — see "What this is not".

**Semantic cache** — `internal/cache`, `TestSemanticFalseHitRate`, `TestSemanticThresholdSweep`

| Threshold | False-hit rate | True-hit rate |
|---|---|---|
| 0.97 (default) | **0/20** | 5/5 |
| 0.90 | 0/20 | 5/5 |
| 0.85 | 3/20 | 5/5 |
| 0.80 | 4/20 | 5/5 |

The closest non-equivalent pair — "What did revenue do in Q1?" against the same question
about Q4 — scores 0.879, so the default sits about nine points above the first wrong
answer. The config validator refuses a threshold below 0.90 for that reason.

**Streaming redaction cost** — `internal/redact`, `BenchmarkRedactOneShot` vs
`BenchmarkStreamRedact`, same 1,085-byte input, `linux/arm64`

| Path | Time |
|---|---|
| Redact in one call | 223 µs |
| Redact arriving in 16-byte chunks | 3.00 ms |
| Un-redact arriving in 16-byte chunks | 19 µs |

Streaming redaction re-runs every detector over the retained buffer on every write, so it
costs about 13× the one-shot path. Un-redaction is cheap because a placeholder always
starts with `[`, so the look-behind is a scan for one byte.

---

## Configuration

Precedence, highest first: **flags → environment → config file → built-in defaults.**
`sluice serve --print-config` prints what the process actually resolved, with secrets
redacted; `sluice serve --check` validates and exits, which is what a deploy pipeline runs.

Validation is exhaustive rather than fail-fast — every problem, each with the path of the
field that caused it:

```
config: 3 problems:
  keys[0].secret: is 5 characters; use at least 16 so that it is not guessable
  routes[0].targets[0].provider: no provider named "openai" is configured
  telemetry.log_level: must be one of debug, info, warn, error; got "loud"
```

See [`sluice.example.yaml`](sluice.example.yaml) for a fully commented file.

### Endpoints

| Path | Auth | Purpose |
|---|---|---|
| `POST /v1/chat/completions` | key | OpenAI-compatible, buffered or SSE |
| `GET /v1/models` | key | Route aliases, OpenAI-shaped |
| `GET /v1/stats` | key | Everything the dashboard draws |
| `GET /v1/targets` | key | Per-target circuit breaker state |
| `GET /metrics` | none | Prometheus exposition |
| `GET /healthz` | none | Liveness. A probe that needs a credential fails when the credential rotates. |
| `GET /readyz` | none | 503 only when every target of every route has an open circuit |
| `GET /` | key (in-page) | Dashboard |

---

## What this is not

Read this section before deploying it. Each item is a real limitation, not a roadmap entry
in disguise.

- **It is single-node, and all its state is in memory.** The cache, the rate-limit buckets,
  the budget ledgers and the circuit breakers live in one process. Run two instances behind
  a load balancer and you have two independent halves of every limit: a key configured for
  60 requests a minute gets 120, and a $50 daily budget becomes $100. There is no Redis, no
  shared store, no gossip. Fixing that properly is a different program.

- **Nothing survives a restart.** Spend since the process started is gone; the audit log on
  disk is the only durable record, which is why `sluice replay` exists.

- **There is no provider SDK dependency, so new provider fields need code.** Every adapter
  translates into `pkg/llm`'s own types. That is what keeps the router, the cache and the
  cost accountant free of vendor switches, and the price is that a field a vendor ships on
  Tuesday is not available until someone maps it — through `ProviderOptions` if it is
  vendor-specific, through a change to the common types if it is not.

- **Only the local provider is implemented.** OpenAI, Anthropic and the rest are priced in
  the built-in table but there is no adapter for them in this build. See
  [ADR 0004](docs/adr/0004-local-provider-as-a-first-class-implementation.md) for why the
  offline provider came first.

- **Streaming redaction is expensive.** 3.00 ms for a 1 KB response arriving in 16-byte
  chunks, against 223 µs for the same bytes in one call. It is bounded and it is
  amortised across the seconds a completion takes, but it is a real 13× and the fix
  (incremental matching) is not written.

- **Person-name recall is 0.750 and will not get much better without a model.** The
  detector is a gazetteer of common given names plus capitalisation and title heuristics.
  It misses surnames standing alone ("Kowalski"), unusual hyphenated forms
  ("Jean-Pierre"), and most names outside its word list; it occasionally claims a
  capitalised department name. Every other detector validates against a checksum or a
  format rule and scores 1.000 precision. If names matter to your threat model, raising
  `min_confidence` trades recall for precision (0.750 → 0.250 recall at 0.90) and a real
  NER model is the actual answer.

- **The pricing table is a snapshot**, dated `2026-05` in `llm.PricingSnapshot`. Vendors
  change prices, rename models and add tiers without warning, and there is no API to fetch
  this from. A prefix fallback means `gpt-4o-2024-11-20` inherits `gpt-4o`'s price, which
  is right for dated snapshots and wrong for a future `gpt-4o-ultra`. State your own prices
  in config and reconcile against the invoice.

- **The semantic cache can be wrong.** It is off by default. A false hit is silent: no
  error, no retry, just a confident answer to a question nobody asked. The threshold is
  measured against a fixture set of 25 pairs, which is a small fixture set. A different
  embedder needs its own threshold measured the same way.

- **The default embedder measures surface similarity, not meaning.** "Cancel my
  subscription" and "how do I unsubscribe" score low. That failure direction is deliberate
  — a missed hit costs money, a wrong hit costs correctness.

- **The compatible surface is a subset.** `n > 1`, `logprobs`, a `tool_choice` naming a
  specific function, and image content parts are refused rather than approximated.
  `frequency_penalty`, `presence_penalty`, `logit_bias`, `response_format` and friends are
  accepted, dropped, and reported in an `X-Sluice-Ignored-Fields` header. There are no
  assistants, embeddings, images or batch endpoints.

- **Keys are secrets in a config file.** They are indexed by SHA-256 so a heap dump does
  not contain a credential table, but SHA-256 is not a password hash and a config file is
  not a secret manager. See [SECURITY.md](SECURITY.md).

- **The audit log is append-only by how the file is opened, not tamper-evident.** Nothing
  stops anything else on the host from rewriting it. Ship the lines somewhere the gateway
  cannot reach.

---

## Development

```sh
make help          # every target, with what it does
make test          # go test ./...
make test-race     # CGO_ENABLED=1 go test ./... -race
make lint          # golangci-lint v1.62.2, pinned
make ci            # fmt, vet, lint, race, build -- what CI runs
make run           # sluice serve with the built-in defaults
make bench         # the benchmarks quoted above
make fuzz          # 60s of the redactor fuzz corpus
make docker        # multi-stage distroless image, non-root
```

The repository must end every change green on `gofmt -l .`, `go vet ./...`,
`golangci-lint run`, and `go test ./... -race`. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Further reading

- [ARCHITECTURE.md](ARCHITECTURE.md) — the pipeline stage by stage, the redaction round
  trip, the semantic cache's risk, routing and failover, and the failure modes.
- [docs/adr/](docs/adr) — the six decisions worth arguing about, in Nygard format.
- [SECURITY.md](SECURITY.md) — threat model, what Sluice does and does not defend against,
  and how to report a vulnerability.

## License

Apache 2.0. See [LICENSE](LICENSE).
