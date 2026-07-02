# 0027 — Report redaction & secret hygiene

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

[[spec 0003]] closed the *input* half of secret discipline: repository and engine
credentials are forwarded **by reference** (`source.pass_env` by name;
`MYSQL_PWD`/`MONGO_PWD` handed to the container via environment, never in config,
images, argv, or a process listing — see [[spec 0024]] R5 and [[spec 0025]]).
0003 R4 states the goal plainly: "Salvage MUST NOT print secret values." That
covers everything Salvage *chooses* to write. It does **not** cover what a
customer's own restore command or check command chooses to *echo*.

There is a residual leak at the **output** layer, and it flows straight into the
signed, attested artifact:

- **Restore combined output.** The exec engine tails the restore command's
  combined stdout+stderr — 4 KiB via `tailString` — into the report on *every*
  path: success (`internal/engine/exec/exec.go:111`), timeout
  (`internal/engine/exec/exec.go:94`), and non-zero exit
  (`internal/engine/exec/exec.go:105`). The source is `cmd.CombinedOutput()`
  (`internal/engine/exec/exec.go:87-88`). That tail lands in
  `RestoreResult.Warnings` (`internal/report/report.go:44`) and is printed by
  `printSummary` (`main.go:615-616`). A restore script that logs a connection
  string, runs `curl -u user:pass`, or echoes `$PGPASSWORD` on error puts that
  string verbatim into `Warnings`.

- **Command/probe `Got`.** A `command`-kind check copies the command's stdout
  into the check's `Got` field when an `equals` assertion is set
  (`internal/probe/probe.go:179`, `res.Got = out`). `evalChecksum`,
  `evalFileExists`, and `evalFileCount` similarly populate `Got`
  (`internal/probe/probe.go:156`, etc.). `Got` is serialized in `CheckResult`
  (`internal/report/report.go:55`).

- **Attestation counter-signs it.** `salvage attest` submits this *exact* report
  object to the hosted notary: `cmdAttest` runs `engine.Run`, serializes with
  `rep.WriteJSON("")` (`main.go:414-415`), and passes those bytes to
  `attest.Submit(...)` (`main.go:462`). So whatever sits in `Restore.Warnings`
  and every check's `Got` is counter-signed by Firerok and appended to a shared,
  tamper-evident ledger — permanently, and visibly to the auditor the ledger is
  meant to reassure.

[[spec 0002]] R7 and [[spec 0003]] R7 already forbid raw production *data* in
reports. This spec is the credential-hygiene sibling: a restore or check that
echoes a **secret** in its output must not be able to smuggle that secret past
the report boundary into the signed and attested bytes. The by-reference input
discipline is necessary but not sufficient; a value forwarded correctly by name
can still be re-emitted by the very program it was forwarded to.

This spec adds a **redaction layer** applied to captured program output *before*
the report is serialized. It changes only *what is captured*, never the
signing/ledger/verify math ([[spec 0012]]): existing attestations still verify
unchanged, and `salvage verify` is untouched.

## Goals

- Redact captured *program* output — restore combined-output and command/probe
  `Got` — before it reaches serialized report JSON, on every path (success,
  timeout, non-zero exit).
- Scrub the **resolved values** of known secrets (`source.pass_env` variables and
  the engines' env-forwarded passwords) from any captured string, so a credential
  that leaks through output is replaced by a fixed marker, not stored.
- Make redaction the **default** for both `attest` and local `report.out`, so the
  safe path is the path a user gets without opting in.
- Preserve a **local-only** way to see raw output for debugging (stderr) that is
  *never* serialized into report JSON.
- Preserve backward compatibility: no change to the canonical signing payload
  shape, the ledger, or `salvage verify` ([[spec 0012]], [[spec 0002]] R2/R3).

## Non-goals

- **Sandboxing the restore/check command.** [[spec 0003]] and the exec engine's
  own design explicitly do not sandbox the customer's restore command; this spec
  does not change that. It governs what we *record*, not what the command may do.
- **A general DLP/PII engine for production row data.** Row-data minimization is
  already owned by [[spec 0002]] R7 / [[spec 0003]] R7 (checks assert scalars;
  the report stores only the asserted scalar). This spec targets **credential**
  material in **captured program output**, not arbitrary sensitive data a check
  intentionally asserts.
- **Changing the signing, ledger, or verification math.** [[spec 0012]] is
  untouched. Redaction happens upstream of canonicalization; the payload schema
  (`schema_version`, field set) is unchanged.
- **Encrypting or vaulting reports at rest.** Out of scope; redaction removes the
  secret rather than protecting a report that still contains it.

## Design

### Where the layer sits

Redaction is a transform over the in-memory report applied **once, immediately
before serialization**, at the two serialization sites that can reach a durable
or counter-signed artifact:

1. the local `report.out` write, and
2. the `attest` submission path (`main.go:414-415` → `main.go:462`).

Because it runs before `rep.WriteJSON` produces the bytes, the redacted form is
what gets canonicalized, locally signed, and submitted — there is no window in
which raw output is signed and then cleaned up afterward. The transform is
idempotent (redacting an already-redacted report is a no-op).

Two independent mechanisms compose, applied in order:

**(A) Captured-output redaction (structural).** The free-text program-output
fields are bounded and de-identified regardless of content:

- `RestoreResult.Warnings` (the 4 KiB restore combined-output tail) is, by
  default, reduced to a bounded, non-secret-bearing form. The recommended default
  is a short **redacted preview** (first line only, itself scrubbed by (B) and
  truncated to a small bound) plus a **SHA-256 hash** of the full captured tail,
  so an operator retains a stable fingerprint and a hint of the failure without
  the report carrying the raw stream. The engine still needs the *unredacted* tail
  transiently to compute the verdict detail (e.g. `firstLine(tail)` at
  `internal/engine/exec/exec.go:101`); redaction applies to what is *stored*, not
  to that in-process decision.
- Check `Got` for output-bearing kinds is treated by kind:
  - For a `command`/`sql` check whose own `equals` assertion is the point of the
    test, the compared value may be stored (the assertion is the intended output),
    but it MUST still pass through secret-value scrubbing (B). To keep even that
    path safe by default, the stored form is a bounded redacted preview; where an
    operator needs the exact literal for a byte-equal assertion, they opt in
    explicitly (see Open questions on granularity).
  - For all other `Got` population (`checksum`, `file_count`, `file_exists`,
    `exit N`), the value is already a bounded scalar and is preserved as-is; these
    do not carry free-text program output.

**(B) Known-secret scrubbing (value-based).** Before storing any captured string,
Salvage resolves the set of **known secret values** for the run and replaces every
occurrence of each with a fixed marker (`[REDACTED:<name>]`). The known set is:

- the resolved value of each `source.pass_env` variable named in the config, and
- the engine-forwarded container passwords used this run (`MYSQL_PWD` per
  [[spec 0024]], `MONGO_PWD` per [[spec 0025]], and any Postgres analogue).

This is a targeted, exact-value scrub — Salvage already *has* these values in
hand (it forwarded them by name), so it can recognize them in output without
guessing. It is deliberately not a general regex hunt; (B) is precise and
low-false-positive. Empty or single-character values are skipped to avoid
degenerate matches.

### Local verbose mode (never serialized)

A local-only flag (e.g. `--show-output`/verbose) MAY write the *raw* captured
restore/check output to **stderr** for debugging. This stream is diagnostic only:
it is never written to `report.out`, never part of the canonical signing payload,
and never submitted by `attest`. `printSummary` (`main.go:615-616`) prints the
**redacted** `Warnings` in normal operation; raw output appears only on stderr
and only under the explicit local flag. This preserves the debugging affordance
without ever letting raw output reach a durable or counter-signed artifact.

### Optional secret-pattern scan (defense in depth)

Independent of (A)/(B), `attest` MAY run a lightweight pattern scan over the
final report bytes for common credential shapes (e.g. `AKIA…` access-key ids,
`-----BEGIN … PRIVATE KEY-----`, bearer-token and URL-embedded `user:pass@`
forms). On a match, `attest` refuses (or, if configured to warn, prints a loud
stderr warning and proceeds). This is a backstop for secrets that are *not* in
the known-value set (B) — e.g. a credential the restore script fetched itself and
never told Salvage about — and is the last gate before Firerok counter-signs.

### Why default-on (the trade-off, made explicit)

Redaction is **on by default** for both local `report.out` and `attest`. The
trade-off is real and named here: default-on means a debugging operator sees a
hash-plus-preview instead of a full 4 KiB restore log in the report file, and
must pass the local verbose flag (stderr) to see raw output. We accept that cost
because the failure mode of default-*off* is unrecoverable: a secret that reaches
the hosted notary is counter-signed and written to a shared, tamper-evident
ledger ([[spec 0002]] R4), where it cannot be unpublished. A safe default that
occasionally costs a debugging round-trip is strictly better than an unsafe
default that can permanently publish a live credential. The raw output remains
one local stderr flag away; the leaked secret, once attested, is forever.

### Compatibility

The report schema (`schema_version`, field set) is unchanged; only the *contents*
of `Warnings` and `Got` differ. Canonicalization ([[spec 0002]] R3), local
signing ([[spec 0002]] R2), the ledger/chain math, and `salvage verify`
([[spec 0012]]) are untouched — a report signs and verifies exactly as before,
whether or not its captured-output fields were redacted. Pre-existing
attestations continue to verify unchanged.

## Requirements

**R1 — Default restore-output redaction.** The restore combined-output tail
stored in `RestoreResult.Warnings` MUST be redacted by default before the report
is serialized for either `report.out` or `attest`, on **every** path — success
(`internal/engine/exec/exec.go:111`), timeout
(`internal/engine/exec/exec.go:94`), and non-zero exit
(`internal/engine/exec/exec.go:105`). The stored form MUST be bounded and MUST NOT
contain the raw captured stream; a hash and/or a scrubbed, truncated preview is
the recommended form. Verdict decisions computed from the raw tail in-process
(e.g. `firstLine(tail)`) MAY use the unredacted value transiently but MUST NOT
store it unredacted.

**R2 — Command/probe `Got` redaction.** For output-bearing check kinds
(`command`, `sql`), the `Got` value stored in `CheckResult`
(`internal/report/report.go:55`) MUST be redacted by default: stored as a hash or
a bounded, scrubbed preview. The exact literal MAY be stored only where the
check's own `equals` assertion requires it, and even then the value MUST first
pass known-secret scrubbing (R3). Scalar `Got` values that are not free-text
program output (`checksum` digests, `file_count`, `file_exists`, `exit N`) MAY be
preserved unchanged.

**R3 — Known-secret scrubbing.** Before any captured string is stored in the
report, Salvage MUST replace every occurrence of each **known secret value** with
a fixed marker. The known set MUST include the resolved value of every
`source.pass_env` variable named in the config and every engine-forwarded
container password used in the run (`MYSQL_PWD`, `MONGO_PWD`, and any Postgres
analogue). Values that are empty or too short to scrub safely MUST be skipped.
This scrubbing MUST apply even to a literal `Got` retained under R2.

**R4 — Local verbose mode never reaches report JSON.** Salvage MAY provide a
local-only flag that writes raw captured restore/check output to **stderr** for
debugging. That raw output MUST NOT appear in `report.out`, in the canonical
signing payload, or in any `attest` submission. Absent the flag, only the
redacted form is printed (`main.go:615-616`).

**R5 — Default-on for durable and attested artifacts.** Redaction MUST be enabled
by default for both the local `report.out` write and the `attest` submission
path. Any mechanism to reduce redaction (e.g. to retain a literal for an
assertion) MUST be an explicit opt-in and MUST still apply R3 known-secret
scrubbing.

**R6 — Signing/verify unchanged.** Redaction MUST NOT alter the report
`schema_version`, the canonicalization ([[spec 0002]] R3), the local signature
model ([[spec 0002]] R2), the ledger/chain math, or `salvage verify`
([[spec 0012]]). Pre-existing attestations MUST continue to verify unchanged.
Redaction changes only *what is captured*, not the signing math.

**R7 — Optional secret-pattern gate (SHOULD).** `attest` SHOULD support a
pattern scan over the final report bytes for common credential formats and, on a
match, refuse to submit by default (configurable to warn-and-proceed). This is a
backstop for secrets outside the known-value set (R3) and MUST run before
submission (`main.go:462`).

**R8 — Idempotence and no-leak on error paths.** The redaction transform MUST be
idempotent and MUST be applied before serialization on **all** report-producing
outcomes, including restore/check failures and timeouts — the paths most likely
to echo diagnostic output containing a credential.

## Open questions

- **Preview vs hash-only default for `Warnings`.** Is a short scrubbed first-line
  preview worth keeping, or is a bare hash the safer default (zero residual text)?
  A preview aids triage but is one more surface (B) must fully scrub.
- **Per-check opt-in granularity.** For a `command`/`sql` check that genuinely
  needs a byte-equal literal in `Got`, what is the cleanest opt-in — a per-check
  config field, or rely on the `equals` assertion itself as the signal? Either way
  R3 scrubbing still applies.
- **Pattern-scan strictness.** Which credential patterns are high-signal enough to
  *refuse* on (vs merely warn), and can a customer add patterns for their own
  formats without weakening the default set?
- **Hash salting.** Should the stored hash of captured output be salted/HMAC'd (so
  it cannot be brute-forced against a small credential space) or is a plain
  SHA-256 fingerprint sufficient given (B) already removes known secrets?

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass; no change to
   canonicalization, signing, ledger, or `salvage verify` behavior or tests.
2. **Planted-secret test (the load-bearing one).** A restore command that echoes a
   known secret (a `pass_env`/engine password value) on its combined output, and a
   `command` check that echoes the same secret into `Got`, both run to a report;
   the attested report **bytes** (the exact bytes passed to `attest.Submit`) MUST
   NOT contain the planted secret. The same holds on the timeout and non-zero-exit
   restore paths.
3. With redaction default-on, `RestoreResult.Warnings` and output-bearing `Got`
   in a serialized report contain a bounded hash/preview, never the raw captured
   stream.
4. The local verbose flag surfaces raw output on stderr, and a byte-for-byte diff
   of the resulting `report.out` and `attest` payload shows the raw output is
   **absent** from both.
5. A report produced with redaction on verifies with `salvage verify` by the
   identical path as before, and a pre-existing (pre-redaction) attestation still
   verifies unchanged — confirming R6.
6. With the R7 gate enabled, an `attest` run whose report matches a known
   credential pattern refuses to submit (or warns, per configuration) before
   reaching `attest.Submit`.
