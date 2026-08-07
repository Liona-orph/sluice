# 4. A local deterministic provider as a first-class implementation

Date: 2026-05-05

## Status

Accepted.

## Context

Everything interesting about a gateway happens *around* a provider call: retry, failover,
circuit breaking, caching, redaction round trips, cost accounting, streaming. All of it
needs a provider to exercise it, and a real provider is the worst possible test fixture —
it costs money per assertion, needs a credential in CI, is rate-limited, is non-deterministic,
and is occasionally down for reasons unrelated to the change under test.

The usual answer is a mock: a struct with a `Complete` that returns a canned response. It
works for a unit test of one function and falls apart everywhere else. A mock does not
stream in realistic chunks, does not produce token counts that a cost test can assert on,
does not fail intermittently, does not fail *after* the third chunk, and does not exist at
all when someone wants to run the binary and see it work.

The third option is a provider that is not a mock: a real `llm.Provider` implementation
that happens to generate its text locally.

## Decision

`pkg/provider/local` is a first-class `llm.Provider`, not a test helper. It lives in `pkg/`,
it is documented, and it is the provider the default configuration uses.

It generates real text from a seeded PRNG and a corpus, streams it in variable-sized
chunks, simulates time-to-first-token and per-token latency with jitter, counts tokens with
the same tokenizer the gateway uses, produces tool calls against a declared schema, and
fails in every mode of the error taxonomy on request — including *after* a configurable
number of chunks, which is the case `Provider.Stream`'s signature is built around.

**Determinism comes from the request, not from call order.** The PRNG is seeded with a hash
of the request fingerprint XOR a configured seed. The same request produces the same
response no matter how many other requests ran first or on how many goroutines. A provider
whose output depended on a shared RNG would be reproducible only single-threaded, which is
exactly when it stops being useful.

The same property applies to injected failures: `Failure.Rate` is evaluated against the
request's own seed, so a given request always fails or always succeeds. A retry test that
flaked would be worse than no test.

Failure injection is configurable **from the config file**, not only from Go, because the
only honest way to demonstrate that failover and the circuit breaker work is to run the
gateway against an upstream that actually fails.

Two instances with different seeds give different answers to the same question, which is
how a failover test can tell which upstream served it.

## Consequences

The entire repository builds, tests, benchmarks and fuzzes with nothing installed and no
network. `make ci` needs Go and nothing else. Contributors do not need a credential to run
the suite, and CI does not need a secret.

The README's quickstart is real rather than aspirational: `sluice serve` with no arguments
is a working deployment.

Cost figures in the demo are simulated. The default config routes an alias named
`gpt-4o-mini` to the local provider under that model name, so the built-in pricing table
applies real list prices to token counts that were really measured. The tokens are real,
the money is not, and both the config comment and the README say so.

**This build ships no adapter for a real provider.** That is the sharp consequence and it
is deliberate: the local provider exercises the interface hard enough that writing an
OpenAI or Anthropic adapter is a contained piece of work against a settled contract, rather
than the thing that shapes the contract. It also means Sluice is not usable against a paid
model without writing one.

The local provider's determinism is a property tests depend on. Changing the corpus, the
chunking or the seeding changes every fixture that asserts on generated text. It is
versioned by nothing, so that change is a breaking change for the test suite and should be
made deliberately.
