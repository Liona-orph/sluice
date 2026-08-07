# 2. Reversible tokenization over masking

Date: 2026-05-06

## Status

Accepted.

## Context

A gateway that finds personal data in a prompt has to do something with it. The options
form a spectrum from most to least destructive:

**Block the request.** Safest and unusable. Prompts contain customer names constantly, and
a gateway that refuses them is switched off within a week.

**Delete or mask the value.** `alice@example.com` becomes `[REDACTED]` or `****@****.com`.
Safe, irreversible, and it breaks the answer. Ask a model to "draft a reply to
[REDACTED]" and you get a reply addressed to nobody, which the application then has to
repair by string substitution it is not equipped to do.

**Replace with a stable placeholder and restore on the way out.** `alice@example.com`
becomes `[SLUICE_EMAIL_0001]` upstream and `alice@example.com` again in the response. The
model never sees the value; the caller never sees the placeholder.

**Pseudonymise with a keyed hash.** Irreversible but stable, so a model can reason about
"the same customer" across a conversation without learning who.

The failure mode that matters is not a false positive in detection — it is an operator
switching redaction off because it made the product worse. Any design that degrades answer
quality loses to the design that does not, regardless of which is theoretically safer.

## Decision

Reversible tokenization is the **default** action, and the zero value of `redact.Action`,
so an operator who has not thought about a newly added entity type gets the reversible
behaviour rather than one that silently ships the value upstream.

The default policy assigns actions by what the answer needs and what the value costs if it
leaks again:

- **Tokenize** contact details — email, phone, person name, IP address. The reply usually
  needs them back, and seeing one again is not a breach.
- **Mask, keeping the last four** — card numbers, IBANs, national identifiers. Restoring a
  card number into a model's output is a way to leak it into a log or a transcript, and the
  last four are what a human actually needs to identify the instrument.
- **Salted hash, never restored** — credentials. A key that came back out of the gateway
  would defeat the point of stopping it going in. The salt is mandatory: an unsalted digest
  of an email address is an email address to anyone with a word list, and the config
  validator refuses a hash action without one.

Three properties make the round trip exact rather than nearly exact:

1. **Reserved placeholders.** Placeholder-shaped substrings already present in the source
   text are recorded, and those indices are skipped when allocating. Without this, a prompt
   containing `[SLUICE_EMAIL_0001]` — pasted from an earlier redaction, or probing —
   collides with a real allocation and the caller gets back text they never sent.
2. **A loose matcher on the way back.** Models lowercase, insert spaces inside brackets and
   swap underscores for hyphens. A strict matcher loses the restoration in exactly those
   cases. Looseness is free, because a string of that shape that is not in the vault is
   left alone.
3. **Unknown placeholders are left alone.** The model may have invented one; inventing a
   value to go with it would be worse than returning the token.

## Consequences

The model's answer stays useful, which is the reason anyone leaves redaction on.

Placeholders are allocated deterministically per request — the first email is always
`[SLUICE_EMAIL_0001]` — which means two callers asking the same question about their own
different addresses produce the identical redacted prompt and share one cache entry. That
is a large cache-hit-rate win and it is also the reason the cache is shared across tenants
by design. A deployment needing per-tenant isolation must namespace the cache; this build
does not.

The vault is per-request state that must be kept alive for the whole response, including a
stream. It is safe for concurrent use because the request path may still be redacting while
the response path is restoring.

Reversibility is a real risk that masking does not have: a bug in restoration puts a value
back into a place it should not be. That is why the completion stored in the audit log is
the *pre-restoration* text, and why the property has a fuzz corpus rather than a handful of
examples.

Streaming makes the round trip harder in both directions: an entity split across chunk
boundaries must not be emitted half-redacted, and a placeholder split across chunk
boundaries must not be delivered unrestored. Both need a bounded look-behind; the upward
one costs 13× the one-shot path. See ARCHITECTURE.md.

Masked values are irrecoverable by construction, so a caller who genuinely needed the card
number back cannot have it. That is the intended answer.
