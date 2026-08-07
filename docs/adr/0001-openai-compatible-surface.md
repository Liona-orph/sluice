# 1. An OpenAI-compatible API surface

Date: 2026-05-04

## Status

Accepted.

## Context

Sluice sits between applications and language models. For it to be adopted at all, the cost
of putting it in front of an existing application has to be close to zero — a gateway that
requires every caller to be rewritten is a gateway that gets scheduled for next quarter and
never installed.

Three options were on the table.

**A native API of our own.** Cleanest to design: we would model exactly what Sluice does,
with no vendor's history baked in. It requires every caller to adopt a new client, and
there is no such client for most languages until someone writes one.

**A pass-through proxy** that forwards request bodies verbatim to whichever upstream the
route names. Zero translation work and perfect compatibility with whatever the upstream
accepts — but it makes every downstream feature impossible. The gateway cannot count tokens
it has not parsed, cannot redact a prompt it treats as opaque bytes, cannot compute a cache
key over fields it does not understand, and cannot fail over to a provider whose request
schema differs.

**OpenAI's chat-completions schema as the wire format**, translated into our own types
internally. Every major language has a maintained OpenAI client; most competing vendors
already ship an OpenAI-compatible endpoint, so the schema is a de facto interchange format
rather than one vendor's API.

## Decision

Speak OpenAI's `/v1/chat/completions` dialect on the wire, buffered and SSE, and translate
into `pkg/llm`'s provider-agnostic types at the edge.

Compatibility is honest or it errors. Anything Sluice cannot serve faithfully is a 400 with
a message that says so:

- `n > 1` — a caller asking for five candidates and receiving one gets wrong results rather
  than an error.
- `logprobs` — there is no token-level data in `llm.Response`, and inventing it is worse
  than omitting it.
- `tool_choice` naming a specific function — approximating it as "required" would let the
  model call a different tool than the one the caller demanded.
- Image content parts — `llm.Message.Content` is a string; forwarding the request with the
  image silently dropped answers a different question.
- The pre-2023 `functions`/`function_call` API — rejected with a message pointing at
  `tools`.

Parameters with no cross-provider meaning (`frequency_penalty`, `presence_penalty`,
`logit_bias`, `response_format`, `parallel_tool_calls`, `service_tier`, `store`,
`metadata`) are accepted, dropped, and reported in an `X-Sluice-Ignored-Fields` header.

Unknown JSON fields are rejected. A misspelled `temprature` that is silently ignored
changes the model's behaviour with no signal at all.

Sluice's own metadata — provider, cache result, cost, redaction counts, request ID —
travels in `X-Sluice-*` response headers, never in the response body.

## Consequences

An application already using an OpenAI SDK changes one environment variable. That is the
whole point and it is worth most of what follows.

We inherit OpenAI's schema warts: `stop` that is either a string or an array, `max_tokens`
superseded by `max_completion_tokens`, content that is either a string or an array of typed
parts, an error envelope with a `param` field nothing populates. Each is handled at the
edge, which is a small amount of ugly code in exactly one file.

We are coupled to a schema we do not control. When OpenAI adds a field, Sluice either maps
it, rejects it, or accepts-and-reports it — and the choice has to be made deliberately each
time. `DisallowUnknownFields` guarantees the choice cannot be skipped by accident, at the
cost of failing requests from a client newer than the binary. That failure is loud and
names the field, which is the right kind of failure.

A strict client decoding into a generated struct never meets an unexpected field, because
our metadata is in headers. The cost is that the metadata is invisible to a caller who only
reads the body — acceptable, since anyone who wants it can read a header, and anyone who
does not is unaffected.

We are not OpenAI. Assistants, embeddings, images, audio, files, batches and fine-tuning do
not exist here. A stub returning an empty list from `/v1/assistants` would be a lie that
costs someone an afternoon.
