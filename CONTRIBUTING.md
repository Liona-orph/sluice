# Contributing

Thanks for considering it. This is a small codebase with strong opinions; the fastest way
to get a change merged is to match them.

## Before you start

For anything beyond a typo, open an issue first. A design discussion in an issue costs an
hour; the same discussion on a finished branch costs an afternoon and someone's goodwill.

## The bar

Every change must leave the repository green on all four:

```sh
gofmt -l .                          # must print nothing
go vet ./...
golangci-lint run                   # v1.62.2, pinned; make lint installs it
CGO_ENABLED=1 go test ./... -race
```

`make ci` runs the lot. CI runs the same commands, so a green `make ci` locally means a
green pull request.

## House style

**Comments explain why.** The code already says what it does. A comment that restates the
line above it is noise; a comment that says which alternative was rejected and what it
would have cost is the reason anyone can change this code six months from now. State the
trade-off and what it cost.

Concretely, prefer:

```go
// Recorded after the response rather than reserved before it. Reserving would
// need an estimate of the output length, which nobody has before generation.
// The cost is that a burst of concurrent requests can overshoot the limit by
// roughly one burst's worth of spend before any of them is refused.
```

over:

```go
// Record adds the cost to the ledger.
```

No marketing, no exclamation marks, no emoji, in code or in commit messages.

**Numbers are measured.** If a comment or a document states a figure — an error rate, a
precision, a latency — there must be a test or a benchmark in this repository that produces
it, and the comment should name it. "Roughly" is fine when it is a rounded measurement and
not when it is a guess.

**No global mutable state.** Immutable lookup tables are fine. A package-level variable
that anything writes to after initialisation is not. `init()` is banned outright and
`gochecknoinits` enforces it.

**No `panic` in library code.** Return an error. `cmd/` may exit; a test helper may panic on
a misconfigured fixture, because that is a test bug failing loudly.

**Errors carry classification.** Provider failures are `*llm.Error` with a code from the
taxonomy. Returning a bare error is not a bug, but an unclassified error disables retry and
failover, so an adapter that cannot be bothered to classify silently gives up the
availability the gateway exists to provide.

**Tests assert on behaviour, with a reason.** A test name should say what property is being
protected — `TestStreamNeverFailsOverAfterFirstToken`, not `TestStream2`. Where the
assertion is subtle, the message says why it matters.

**Do not weaken a test to make a change pass.** If a test is wrong, fix the test in its own
commit with an explanation.

## Adding a provider

`llm.Provider` is three methods. The contract that matters is in the doc comment on
`Stream`: the outer error is for failures before any token, the sequence's error is for
failures after. Getting that wrong breaks failover in a way no test of the adapter alone
will catch.

Classify every error into `llm.ErrorCode`. Populate `Provider` and `Model` on the error —
by the time it reaches a retry loop, the caller has forgotten which of three failover
candidates produced it. Populate `RetryAfter` when the provider states one.

`pkg/provider/local` is the reference implementation and the thing to read first.

## Adding a detector

Implement `redact.Detector`. Two things are load-bearing:

- **Validate.** A pattern that matches the shape of a thing matches a great many things
  that are not it. If there is a checksum, use it; if there is a format rule, apply it. Add
  a test that measures the false-positive rate on random input, as
  `TestCreditCardValidationFalsePositiveRate` does.
- **`MaxLen` must be honest.** It sets the streaming redactor's look-behind window. A
  detector that under-reports it causes entities to be emitted half-redacted across chunk
  boundaries — a silent leak, in production only.

Add fixtures to `internal/redact/testdata/pii_corpus.json` and a precision/recall floor in
`eval_test.go`. A detector with no stated floor fails the accuracy test by design.

## Adding a metric

Read the package comment on `internal/telemetry` first. Every label value must come from a
config file or a closed enumeration. If you need a request-derived identifier, it goes in
the log or the audit record, not in a label.

## Commits and pull requests

Conventional-ish subjects (`router: honour Retry-After over the computed backoff`) in the
imperative mood, under 72 characters. The body explains why, not what — the diff is what.

Fill in the pull request template. "Why" and "how it was verified" are the two sections
reviewers actually read.

Keep changes focused. A pull request that fixes a bug and reformats two files is two pull
requests.

## Conduct

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Licence

Contributions are licensed under Apache 2.0, the same as the project. By submitting a pull
request you certify you have the right to do so.
