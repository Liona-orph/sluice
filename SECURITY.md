# Security policy

Sluice is a security control. Its job is to stop personal data reaching a third-party model
provider, to bound what a compromised or runaway key can spend, and to leave a record of
what happened. This document says what it actually defends against, what it does not, and
how to tell us when it fails.

Read the "What this is not" section of the README alongside this. Several of the
limitations there are security-relevant.

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private vulnerability reporting on this repository
(**Security → Report a vulnerability**), or email **security@sluice-gw.dev**. If you want
to encrypt, ask for a key in a first message containing nothing sensitive.

Please include: a description, the version or commit, a reproduction, the impact you
believe it has, and whether any third party is already aware.

What to expect:

| Stage | Target |
|---|---|
| Acknowledgement that a human has read it | 3 working days |
| Initial assessment and severity | 10 working days |
| Fix or documented mitigation for high/critical | 90 days |
| Public advisory | after a fix ships, or by agreement |

We will credit you in the advisory unless you ask us not to. There is no bug bounty.

## Supported versions

Sluice is pre-1.0. Only `main` receives fixes. There are no maintained release branches,
and there will not be until 1.0.

## What Sluice defends against

**Personal data reaching a model provider.** Ten detectors run over every message before it
leaves the process. Detectors that can validate their own findings do — Luhn for cards,
mod-97 for IBANs, format rules for national identifiers — and the measured precision and
recall are published in the README rather than claimed.

**Personal data reaching the audit log.** Prompts and completions are stored redacted. See
[ADR 0006](docs/adr/0006-redacted-prompts-in-the-audit-log.md).

**Personal data reaching the metrics endpoint.** No metric may carry a label derived from a
request. `TestNoUnboundedLabelCardinality` gathers the live registry and fails the build if
one does. `/metrics` is unauthenticated by design, so it must contain nothing sensitive —
and the test is what makes that a property rather than an intention.

**Unbounded spend.** Token buckets on both requests and tokens per minute, and rolling-window
budgets per key and per team. A compromised key is bounded in what it can cost before
anyone notices.

**Credential recovery from process memory.** Keys are indexed by SHA-256 of the secret, so
a heap dump contains digests rather than a table of valid credentials. The lookup compares
fixed-size arrays, so it does not leak how many leading bytes of a guess were correct.

**Amplification during an incident.** Retries are bounded, jittered and honour
`Retry-After`; circuit breakers stop the gateway hammering a failing upstream. A malformed
request cannot open a circuit, so one bad client cannot remove a provider for everyone
else.

**Trojan Source.** `bidichk` runs in CI. Bidirectional control characters that make source
read differently to a human than to the compiler would be used, if anywhere, in a redaction
policy or an auth check.

## What Sluice does not defend against

Be precise about these before deploying it.

**Detection is not complete, and cannot be.** Person-name recall is 0.750 on the labelled
corpus. Every entity type not in the detector list — passport numbers, medical record
numbers, most non-US/UK/ES national identifiers, internal customer IDs — passes through
untouched. Redaction reduces exposure; it does not eliminate it, and a deployment that
treats it as a guarantee has mis-modelled its risk.

**A determined caller can defeat redaction.** Base64 the card number, spell it out in
words, split it across two messages, or describe it. The detectors look at text, not intent.
Sluice is a control against accidental disclosure, not against a user who wants the value
to reach the model.

**Prompt injection, jailbreaks and model output safety.** Entirely out of scope. Sluice
does not inspect what the model says for anything except placeholders to restore.

**Keys in a config file.** Secrets live in the YAML. SHA-256 is not a password hash — it is
fast by design, so a low-entropy key is brute-forceable offline by anyone who obtains the
file. The validator enforces a 16-character minimum; that is a floor, not a policy.
Generate keys from a CSPRNG, keep the file out of version control, mount it read-only, and
use a secret manager if you have one. `SLUICE_REDACTION_HASH_SALT` exists so that at least
the salt can come from the environment.

**The demo key is public.** `sk-sluice-local-demo` is in the source. It is harmless only
because the default configuration reaches no network and spends no money. A deployment that
copies the default and adds a real provider has published a credential.

**Multi-tenant cache isolation.** Redacted prompts are shared across callers by design —
that is what makes the placeholder scheme save money. Two callers whose prompts differ only
in the redacted values share a cache entry. If your tenants must not share cached answers,
this build cannot give you that.

**Audit log tamper-evidence.** The file is opened `O_APPEND`, which stops *this code* from
rewriting history. It stops nothing else on the host. Ship the lines somewhere the gateway
cannot reach if you need integrity.

**Distributed limit enforcement.** All state is per-process. Two instances behind a load
balancer enforce two independent halves of every limit. See the README.

**Denial of service.** There is a body-size cap, a header-read timeout and a per-request
timeout. There is no connection limit, no per-IP throttle and no protection against a
caller who authenticates and then opens ten thousand streams. Put it behind something that
does that.

**Transport security.** Sluice serves plain HTTP. Terminate TLS in front of it. There is no
certificate handling here at all.

**Supply chain beyond our direct dependencies.** Dependabot and `dependency-review` run in
CI, CodeQL scans the code, and the dependency list is deliberately tiny (testify,
yaml.v3, prometheus/client_golang). That reduces the surface; it does not eliminate it.

## Hardening a deployment

- Terminate TLS in front; do not expose the listener directly.
- Generate key secrets from a CSPRNG; 32 bytes of base64 or better.
- Set `redaction.hash_salt` from `SLUICE_REDACTION_HASH_SALT`, not from the file.
- Put `/metrics` and the dashboard behind your own network policy. `/metrics` is
  unauthenticated; the dashboard requires a key but that key is a full API key.
- Set `audit.sync: true` if the log is a compliance artefact rather than a cost report, and
  ship it off the host.
- Set budgets and rate limits on every key. The defaults are permissive for a demo.
- Run `sluice serve --check` in the deploy pipeline. Configuration errors are the most
  likely way a control ends up switched off.
- Run the container as the non-root user it already defaults to, read-only root filesystem,
  no new privileges.
