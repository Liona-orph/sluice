# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Before 1.0,
minor versions may contain breaking changes; they will be listed here under **Changed** with
the word "breaking".

## [Unreleased]

Nothing yet.

## [0.1.0] - 2026-08-09

First release. Everything below is new, so the sections are organised by area rather than
by change type.

### Core

- `pkg/llm`: provider-agnostic request, response, chunk, usage and cost types; an error
  taxonomy that separates "retry the same provider" from "try a different one"; integer
  nanodollar money; a dated snapshot of published list prices; a vocabulary-free approximate
  tokenizer measured at 0.68 % mean absolute percentage error against cl100k_base over 32
  fixtures.
- `pkg/provider/local`: a deterministic offline `llm.Provider` that generates real text,
  streams it, simulates latency, and injects every error mode in the taxonomy — including
  failures after a configurable number of chunks.

### Redaction

- Ten detectors: email, phone, credit card, IBAN, IP address, US SSN, UK NINO, ES DNI, API
  key, person name. Checksum or format validation wherever one exists; the card pattern
  alone accepts 100 % of random 13–19 digit strings and the validated detector accepts
  4.21 %.
- Reversible tokenization with exact round-tripping, including reserved-placeholder
  collision avoidance and a deliberately loose matcher on the way back.
- Streaming-safe in both directions: a bounded look-behind on redaction (254 bytes with the
  default detectors) and a cheaper one on restoration.
- Measured precision and recall per entity type over 57 labelled documents.

### Caching

- Exact-match cache keyed on a canonical request fingerprint, with LRU eviction and TTL.
- Optional semantic cache, off by default, with a threshold measured at a 0/20 false-hit
  rate on the fixture pairs.
- Truncated and content-filtered responses are never stored.

### Decision layer

- `internal/router`: model aliases to ordered targets, failover on failover-worthy error
  codes only, per-target circuit breaker, exponential backoff with jitter honouring
  `Retry-After`, and priority, round-robin and weighted load balancing. A stream that has
  emitted a token is never failed over.
- `internal/policy`: token buckets on requests and tokens per minute; rolling-window
  budgets per key and per team, with reject or degrade at the limit.
- `internal/config`: a declarative deployment description with exhaustive validation,
  referential checks, and unknown-field rejection.

### Request path

- `internal/gateway`: the ten-stage pipeline, each stage a named and individually testable
  method.
- `internal/server`: an OpenAI-compatible `/v1/chat/completions` (buffered and SSE), Sluice's
  own `/v1/stats` and `/v1/targets`, health and readiness probes, Prometheus exposition, and
  the embedded dashboard.
- `internal/audit`: append-only JSON Lines, with prompts and completions stored redacted.
- `internal/telemetry`: `log/slog` and Prometheus metrics with a build-failing test against
  unbounded label cardinality.

### Tooling

- `sluice serve` with flag, environment and file configuration, documented precedence,
  `--check`, `--print-config`, graceful shutdown and `--version` via ldflags.
- `sluice replay` to re-price a recorded audit log against a different model.
- A single-file embedded dashboard with no build step and no external requests.
- Makefile, pinned `golangci-lint`, multi-stage distroless Dockerfile, docker-compose, and
  GitHub Actions for CI across a Go version matrix, CodeQL, scheduled fuzzing and
  dependency review.

[Unreleased]: https://github.com/sluice-gw/sluice/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/sluice-gw/sluice/releases/tag/v0.1.0
