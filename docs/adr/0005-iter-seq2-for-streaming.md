# 5. `iter.Seq2` for streaming rather than channels

Date: 2026-05-05

## Status

Accepted.

## Context

`Provider.Stream` has to deliver an unbounded sequence of chunks that can end in an error.
Go offers three shapes.

**A channel** — `<-chan Chunk` plus an error channel or a wrapper struct. Idiomatic, and it
leaks a goroutine unless every consumer drains it to completion. Consumers of a token
stream abandon it constantly: the client disconnects, a budget is exhausted mid-generation,
a guardrail trips, a context is cancelled. Every one of those paths has to remember to
drain or to cancel, and the one that forgets leaks a goroutine per request, which is
invisible until the process is out of memory. A `context.Context` mitigates it, but only if
the producer selects on it correctly at every send.

**A callback** — `Stream(ctx, req, func(Chunk) error) error`. No goroutine, no leak, and
control inversion: the consumer cannot easily buffer, cannot easily hand the stream to
another layer, and cannot `break` without inventing a sentinel error.

**A range-over-func iterator** — `iter.Seq2[Chunk, error]`, Go 1.23. The producer runs on
the consumer's goroutine, early termination is a normal `return` from `yield`, and there is
nothing to leak.

## Decision

`Provider.Stream` returns `(iter.Seq2[llm.Chunk, error], error)`.

The two error channels are deliberate and they mean different things:

- **The outer error** reports failures that happen *before any token is produced* — a
  rejected request, a refused connection, an open circuit. Nothing has reached the client,
  so the caller can still fail over, and an HTTP handler that has not written a status line
  can still write a JSON error instead of a half-finished event stream.
- **An error yielded by the sequence** reports a failure *after* output has begun. By then
  some of the answer has reached the client and a retry would duplicate it.

The sequence yields at most one error and stops immediately after. Implementations must
release their resources when iteration ends, which for range-over-func means a `defer` in
the iterator body. Consumers may abandon the sequence at any point.

The whole streaming stack is built on this shape and every layer is a lazy wrapper:
`redact.RedactStream`, `redact.RestoreStream`, the gateway's accounting tap, and the
router's failover loop. None of them starts a goroutine.

## Consequences

Abandoning a stream leaks nothing, and that is asserted rather than asserted-to-be-true:
`internal/leaktest` samples the goroutine count before and after tests that walk away from
a stream mid-flight, at the router, redactor and gateway layers.

The router can implement "fail over before the first token, never after" inside the
returned iterator without `iter.Pull2`, a coroutine, or any goroutine at all. The rule that
matters most is enforced by ordinary control flow.

The gateway's streaming path can do its accounting and write its audit record in the same
iterator that delivers the last chunk, including when delivery stopped because the client
disconnected — so a stream that nobody read is still billed and still recorded.

**The cost:** a consumer cannot `select` over a stream alongside other events. Something
that needs to interleave a token stream with a timer or another channel has to convert,
with a goroutine it owns and can therefore reason about. Nothing in Sluice needs to; a
future component might, and it will own that goroutine explicitly, which is the point.

Producer errors surface at the consumer's stack rather than the producer's, which makes
panics and profiles read slightly unusually — the provider's work appears under the HTTP
handler.

It requires Go 1.23. That is stated in `go.mod` and in CI's version matrix.
