# 3. Semantic caching, off by default, with a measured threshold

Date: 2026-05-08

## Status

Accepted.

## Context

An exact-match cache keyed on a canonical request fingerprint is unambiguously safe:
identical request, identical answer. It is also narrow. Real traffic is full of prompts
that differ by a word, a comma, or a timestamp in a system message, and every one of those
is a miss and a bill.

A semantic cache embeds the prompt and returns the nearest stored entry when the similarity
clears a threshold. That is where the savings are and where the danger is.

The danger deserves naming precisely, because it is unlike every other failure a gateway
has. **A false cache hit is a confidently wrong answer to a question nobody asked, and it
is silent.** No error, no retry, no alert, no elevated latency — just a response that does
not correspond to the prompt. An outage is noticed in minutes; a false hit is noticed when
a customer complains about advice they were given three weeks ago.

The threshold is the whole decision, and the temptation is to pick a number that sounds
conservative (0.9? 0.95?) and ship it. A number chosen that way is a guess wearing the
costume of a measurement.

## Decision

Ship semantic caching, **off by default**, with a threshold derived from a measurement, and
with approximation confined to *what* was asked and never to *how*.

**Off by default.** An exact cache cannot return a wrong answer and a semantic one can, so
the behaviour you get without making a decision is the safe one. Turning it on is an
explicit act.

**The threshold is measured.** `internal/cache/testdata/semantic_pairs.json` holds 25
labelled pairs — 5 equivalent, 20 adversarial near-misses. `TestSemanticThresholdSweep`
reports:

| Threshold | False-hit rate | True-hit rate |
|---|---|---|
| 0.97 (default) | 0/20 | 5/5 |
| 0.95 | 0/20 | 5/5 |
| 0.90 | 0/20 | 5/5 |
| 0.85 | 3/20 | 5/5 |
| 0.80 | 4/20 | 5/5 |

The closest non-equivalent pair — "What did revenue do in Q1?" against the same question
about Q4 — scores 0.879. The default therefore sits about nine points above the first wrong
answer, and the config validator **refuses** a configured threshold below 0.90 with a
message saying why.

**Parameters must match exactly.** A semantic hit requires the same namespace and an
identical fingerprint of every non-message field. An answer generated with `max_tokens: 50`
is not an acceptable answer to a request for 2000; an answer generated without tools cannot
satisfy a request that declares them; a different system prompt is a different question
however similar the user turn looks.

**The default embedder fails in the safe direction.** It is the hashing trick over word
unigrams, word bigrams and character n-grams taken across the whole normalised string. It
measures surface similarity, not meaning: "cancel my subscription" and "how do I
unsubscribe" score low, while "delete the user" and "delete the users" score very high.
That asymmetry is correct for a cache — a missed hit costs money, a wrong hit costs
correctness.

Word order is carried explicitly by the bigrams and cross-word n-grams. This is not a
refinement: a pure bag-of-words vector scores "Convert 10 USD to EUR" and "Convert 10 EUR
to USD" at exactly 1.0, and a cache that treats those as the same question returns a
confidently reversed answer. Fixture pairs exist to keep that from regressing.

Vectors of differing length are never compared, because a length mismatch means a different
embedder and comparing them anyway produces a number that merely looks like a similarity.

## Consequences

Most deployments run exact-match only and get a correctness guarantee. Those that turn on
the semantic cache accept a measured, bounded risk rather than an unmeasured one.

The measurement is only as good as its fixture set, and 25 pairs is a small fixture set. It
is enough to catch a threshold that is obviously too low and not enough to certify one that
is marginally too low. That is stated in the README rather than hidden.

A real embedding model has a differently shaped similarity distribution — typically much
higher baseline similarity between unrelated texts — so 0.97 is meaningless for it. The
threshold is a knob and not a constant precisely so that a deployment can measure its own
and set it. If it does not, the validator's 0.90 floor is a guard rail, not a guarantee.

Because the default embedder is lexical, the semantic cache buys much less than a real one
would. `Stats.NearMisses` counts candidates that were the best available and fell below the
threshold, so a high near-miss count next to a low hit count is the signal that the
threshold is too tight for the embedder in use.

The scan is linear over the entries in the namespace, bounded by `max_entries`. At the
10,000-entry default with 256 dimensions that is a few milliseconds under one mutex. A
deployment that needs more entries needs an index, and that is a different design.
