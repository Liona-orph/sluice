# Architecture decision records

One file per decision that was worth arguing about, in
[Michael Nygard's format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions):
context, decision, consequences. They are immutable once accepted — a decision that changes
gets a new record that supersedes the old one, because the value of an ADR is that it
records what was believed *at the time*, and a rewritten one records nothing.

| # | Decision | Status |
|---|---|---|
| [0001](0001-openai-compatible-surface.md) | An OpenAI-compatible API surface | Accepted |
| [0002](0002-reversible-tokenization-over-masking.md) | Reversible tokenization over masking | Accepted |
| [0003](0003-semantic-caching-with-a-conservative-threshold.md) | Semantic caching, off by default, with a measured threshold | Accepted |
| [0004](0004-local-provider-as-a-first-class-implementation.md) | A local deterministic provider as a first-class implementation | Accepted |
| [0005](0005-iter-seq2-for-streaming.md) | `iter.Seq2` for streaming rather than channels | Accepted |
| [0006](0006-redacted-prompts-in-the-audit-log.md) | Storing redacted prompts in the audit log | Accepted |
