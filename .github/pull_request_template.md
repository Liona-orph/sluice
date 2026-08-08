## What

<!-- One or two sentences. The diff says what changed; this says what it is. -->

## Why

<!-- The reason the change exists, and the alternative you rejected. This is the
     section reviewers read first and the one that survives into the commit log,
     so it is worth more than the rest of this template put together. -->

## How it was verified

<!-- Not "tests pass" -- which tests, and what property do they protect? If the
     change touches a measured number (detector accuracy, tokenizer error, cache
     false-hit rate, a benchmark quoted in the README), paste the new output. -->

## Checklist

- [ ] `make ci` is green (fmt, vet, lint, `-race`, build)
- [ ] Comments explain **why**, and state the trade-off where there is one
- [ ] Any number added to a comment or document is produced by a test in this repo
- [ ] No test was weakened to make this pass
- [ ] `CHANGELOG.md` updated under Unreleased, if this is user-visible

## Security and correctness

<!-- Delete the lines that do not apply, and answer the ones that do. -->

- [ ] Touches `internal/redact`: does the round trip still hold on the fuzz corpus, and is `MaxLen` still honest for any detector changed?
- [ ] Touches `internal/audit`: does anything raw reach the record?
- [ ] Adds or changes a metric: is every label value bounded by config or a closed enumeration?
- [ ] Changes the error taxonomy, retry or failover: what does it do to a partial stream?
- [ ] Changes pricing or budget arithmetic: is it still integer nanodollars end to end?
- [ ] Changes the OpenAI-compatible surface: is the new behaviour faithful, or does it error?
