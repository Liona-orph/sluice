# 6. Storing redacted prompts in the audit log

Date: 2026-05-11

## Status

Accepted.

## Context

The audit log exists to make three questions answerable after the fact: why is the bill this
size, did anything sensitive leave the building, and what would this traffic have cost on a
different model. Answering them needs a record per request with the principal, the model,
the provider, the token counts, the cost, the cache result and the redaction counts.

The open question is what to do with the prompt itself. Three positions:

**Store it raw.** Maximum forensic value. An investigator can see exactly what was sent and
reproduce it.

**Store nothing.** Maximum safety, minimum use. A cost report with no prompts cannot answer
"which feature is generating these enormous requests".

**Store the redacted form** — the text as it was actually sent upstream, with placeholders
where the personal data was.

The argument for raw is genuinely strong during an incident, which is why it needs
answering rather than dismissing.

## Decision

The audit record stores the **redacted** prompt and the **redacted** completion. Never the
original values, and never the restored response.

Four reasons, and they compound.

**The audit log is the least-protected copy of the data.** It is tailed, shipped to a log
aggregator, indexed, and retained for years by policy. A card number that was correctly
stripped before it reached a provider, and then written verbatim to a file that gets
shipped to three SaaS vendors, has not been protected — it has been copied.

**It would make the redactor pointless in exactly the case that matters.** The product's
entire claim is that sensitive values do not leave the process boundary in the clear. A raw
audit log breaks that claim on the gateway's own disk.

**Retention rules differ and cannot both be honoured in one file.** Audit records are kept
for years; the personal data inside a prompt is usually subject to a deletion request
measured in days. A log that mixes them cannot satisfy either policy without rewriting an
append-only file, which is a contradiction in terms.

**The redacted prompt is nearly as useful.** Placeholders are stable within a request, so
the *structure* of what was asked is fully visible: `summarise the account for
[SLUICE_PERSON_0001] at [SLUICE_EMAIL_0001]` tells an investigator everything except the
identity — and the identity is the one thing they should have to obtain through a separate,
authorised path.

The stored completion is the pre-restoration text for the same reason. Restoring it for the
log would put the original values back into the one file that outlives everything.

`redaction_counts` records how many values of each entity type were replaced, so the record
still *proves* redaction ran and on what, without recording what it found.

## Consequences

The audit log can be shipped, indexed and retained under ordinary log-handling rules
without becoming a personal-data store in its own right. That is the point.

An investigator cannot reconstruct the exact prompt from the audit log alone. If the
original values are genuinely required, they have to come from the application that sent
them, under whatever access controls that application has — which is the correct place for
that control to live. This is the real cost of the decision and it is not hypothetical:
some investigations will be harder.

The counts remain a small signal. `redaction_counts: {"credit_card": 4}` on a request from
a key that should never see card numbers is actionable on its own.

Counting differs by action, and a report built on it should know: tokenized values are
counted once per *distinct* value, because that is what a placeholder identifies; masked
and hashed values are counted once per *occurrence*, because neither leaves anything behind
that could tell two identical values apart.

Redaction has to run before the audit record is built, which it does — it is stage 4 of the
pipeline and the record is written at stage 10. A future refactor that moved redaction
later would silently break this decision, which is why `TestAuditStoresTheRedactedPromptAndCompletion`
asserts the absence of the raw value rather than the presence of the placeholder alone.

With redaction disabled the prompt is stored as sent, which is to say raw. That follows
from the same principle — the log records what actually crossed the boundary — and it means
`redaction.enabled: false` is a decision about the log as well as about the upstream.
