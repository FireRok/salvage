# 0035 — Upgrade & compatibility policy

- **Status:** Proposed
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage's whole deployment model assumes artifacts that **outlive the binary
that made them**. Five things routinely survive an upgrade:

1. **Config files.** `salvage schedule` wires `salvage attest -config …` into
   cron/systemd ([[spec 0015]]) precisely so it runs unattended for months or
   years. The config a team wrote against `v0.Y` will be parsed by every
   binary they upgrade to.
2. **Reports.** [[spec 0026]] gives reports a `schema_version`; downstream
   consumers (CI gates, the MCP server of [[spec 0032]]) parse reports written
   by whatever version happens to be installed.
3. **Attestations in the ledger.** The notary is an *append-only* ledger
   ([[spec 0012]]); its entire value is that an attestation submitted today is
   verifiable indefinitely. `salvage verify` next year must verify what
   `salvage attest` produced this year.
4. **Evidence packs.** The auditor/insurer artifact ([[spec 0019]]) is
   explicitly designed to be handed to a third party and checked later —
   possibly much later, by a much newer verifier.
5. **The hosted API.** The service deploys continuously; the CLI is pinned by
   operators. Old CLIs talk to a new server every day.

None of these has a stated compatibility promise today. Two pending changes
make the gap acute:

- **Strict config parsing** (BACKLOG S1, `specs/BACKLOG.md:15-23`) will make
  unknown config keys a hard error — correct for typo safety, but it converts
  "newer config key meets older binary" from silently-ignored into
  exits-2-in-cron. Without a stated policy for how config evolves, S1 turns
  every additive config feature into a potential fleet-wide breakage for
  mixed-version fleets.
- **`schema_version`** ([[spec 0026]]) defines how reports are *versioned* but
  deliberately not *when* the version may change or what consumers may assume
  between bumps.

[[spec 0034]] defines what a version *number* communicates; this spec defines
what actually *keeps working* when the number changes — the behavioral
guarantees an operator relies on when they replace the binary and touch
nothing else.

## Goals

- **Drop-in upgrades within a compatibility line.** Replacing the binary with
  a newer non-breaking release, with the same config and environment, keeps
  working: same config accepted, same verdict semantics, same exit codes.
- **A written config-evolution rule** so strict parsing (S1) and additive
  features coexist: what may be added when, how keys are deprecated, and how
  long deprecated keys keep working.
- **Report-consumer guarantees**: exactly what a consumer may assume from
  `schema_version` staying the same, and what a bump obliges Salvage to do.
- **Verification permanence**: every attestation and evidence pack ever
  produced remains verifiable by every future `salvage verify` — the
  append-only ledger's promise extended to the toolchain.
- **A hosted-API compatibility floor** with honest failure: when the service
  drops support for an old CLI, that CLI gets a clear operational error
  naming the remedy — never a wrong verdict, never a silent degradation.

## Non-goals

- **Version-number semantics, changelog, support statement** — [[spec 0034]].
- **Config auto-migration tooling** (`salvage config migrate`). Worth
  building if a breaking config change ever ships; not promised here (Open
  question).
- **Compatibility across major versions.** Post-1.0 majors may break the
  surfaces below; the promise of this spec is that breakage is *confined* to
  majors (and, pre-1.0, to flagged minors per [[spec 0034]] R2).
- **Hosted-service internal migrations** (its own storage, schemas,
  deployment). Only its externally observable contract with released CLIs is
  in scope.
- **Guaranteeing third-party engine tool behavior** (pgBackRest, restic,
  borg, server images). What Salvage supports *of the outside world* is
  [[spec 0036]]; this spec is about Salvage's own surfaces over time.

## Design

### Config: additive within a line, deprecate-then-remove

Within a compatibility line (pre-1.0: a minor series; post-1.0: a major), the
config schema only grows: new **optional** keys with defaults preserving
prior behavior. Under strict parsing this yields an asymmetric but workable
rule: an old config always works on a newer binary (forward upgrade is safe);
a new config using new keys fails *loudly* on an older binary (exit 2 naming
the unknown key — the correct failure, since the old binary can't honor the
intent).

Removing or re-meaning a key is a breaking change and requires the full
cycle: (a) a release that accepts the old key but warns on stderr, with the
changelog naming the removal release ([[spec 0034]] R3); (b) removal only at
a breaking-bump boundary. Warnings go to stderr only — never into the report,
never affecting exit codes — consistent with the stderr/report separation of
[[spec 0027]].

### Reports: `schema_version` as the consumer contract

Between bumps of `schema_version`, changes are **additive only**: new fields
may appear; existing fields keep their names, types, and meanings; no field
is removed. A consumer written against version `N` may therefore parse any
report with `schema_version == N` ignoring unknown fields, and may use
`>= N` checks per [[spec 0026]]. Anything else — rename, retype, re-meaning,
removal — bumps `schema_version` and publishes a new schema document
([[spec 0026]] R7). The same discipline applies to the machine verdict object
of `verify -json`.

### Attestations & evidence packs: verify forever

Envelope formats, hash constructions, and signature schemes are never
retired: `verify` accumulates format support monotonically. Key rotation is
already representable (`key_id` in the envelope, [[spec 0012]]); a rotation
adds a key, it never invalidates history. Evidence packs are self-verifiable
by construction ([[spec 0019]]); newer Salvage versions must keep verifying
packs produced by older ones. Practically this means the verification code
paths for old formats are frozen-and-tested, not refactored away.

### Hosted API: a published floor, an honest wall

The service publishes a **minimum supported CLI version**. At or above the
floor, all documented endpoints behave per their contracts. Below it, the
server responds with an explicit "client version unsupported" error that
names the floor and the upgrade path; the CLI surfaces it as an operational
error (exit 2). Two hard rules: the floor never moves without notice in the
changelog and a reasonable lead time, and **degradation is never silent** —
an old CLI gets a refusal it can't mistake for a verdict, not a
subtly-different answer. (How the CLI identifies its version to the server —
User-Agent vs an explicit field — is an implementation detail; that it does
is required.)

### Local state

The CLI's persisted local state (the stored API key from `salvage login`,
[[spec 0014]]) is versioned or format-stable such that upgrade preserves it
and a subsequent downgrade within the same line does not corrupt it — at
worst, a re-`login`.

## Requirements

**R1 — Config forward-compatibility.** Within a compatibility line, a config
file accepted by version `X` MUST be accepted by every later version in the
line, with unchanged semantics. New config keys MUST be optional with
behavior-preserving defaults.

**R2 — Config deprecation cycle.** A config key or CLI flag MUST NOT be
removed or re-meaned within a line. Removal requires: at least one prior
release that accepts the old form and emits a stderr deprecation warning
naming the replacement and the removal release; the changelog entries per
[[spec 0034]] R3; and removal only at a breaking-bump boundary. Deprecation
warnings MUST NOT alter exit codes, report bytes, or verdicts.

**R3 — Additive-only reports between schema bumps.** While `schema_version`
is unchanged, report changes MUST be strictly additive (no removal, rename,
retype, or re-meaning of any field). Any non-additive change MUST bump
`schema_version` and publish the new schema per [[spec 0026]] R7.

**R4 — Verification permanence.** `salvage verify` (and evidence-pack
verification) MUST successfully verify every attestation and evidence pack
produced by any prior released version. Envelope/signature formats MUST never
be dropped from the verifier; key rotation MUST NOT invalidate previously
issued attestations.

**R5 — Hosted-API floor with explicit refusal.** The hosted service MUST
publish a minimum supported CLI version and MUST serve all documented
endpoints correctly for clients at or above it. For clients below the floor
it MUST return an explicit unsupported-version error (which the CLI reports
as an operational error, exit 2) rather than altered or degraded behavior.
Raising the floor MUST be announced in the changelog before it takes effect.

**R6 — Exit codes frozen forever.** The exit-code contract ([[spec 0000]]
R4: `0` pass / `1` verdict fail / `2` operational error) MUST NOT change at
any version bump, including majors.

**R7 — Verdict stability across upgrade.** For the same backup artifact,
config, and environment, upgrading within a line MUST NOT change the verdict
produced by unchanged checks. New releases may add warnings and additive
report fields; they may not silently re-judge.

**R8 — Local-state survival.** Upgrading MUST preserve stored credentials and
any other CLI-persisted state; a downgrade within the same line MUST NOT be
corrupted by state a newer version wrote (worst case: a clean re-`login`,
never undefined behavior).

## Open questions

- **Line length pre-1.0.** Is a pre-1.0 "compatibility line" a single minor
  series (strict reading of [[spec 0034]] R2) — meaning R1 only binds within
  `0.Y.*` — or should config compatibility be promised across pre-1.0 minors
  except where the changelog flags otherwise? The latter is friendlier and
  recommended; decide before publishing the policy.
- **`salvage config migrate`.** If a breaking config change ever ships,
  should the binary rewrite old configs mechanically? Deferred until a
  concrete breaking change exists to design against.
- **Hosted floor width.** How much lead time and how wide a supported window
  (measured in releases or months) the floor promise commits to — this
  interacts with [[spec 0034]]'s support statement and real operational cost;
  needs a concrete number before the docs publish it.
- **Compatibility test harness.** A pinned corpus of old configs, reports,
  attestations, and evidence packs replayed against each release candidate
  would turn R1/R3/R4 from policy into regression tests (`dev/corpus/`
  already exists as a seed of this shape). Scope and location deferred.

## Acceptance criteria

1. The public docs state the config-evolution rule (R1/R2), the report
   guarantee (R3), the permanence guarantee (R4), and the hosted floor (R5),
   reachable from the docs guide index.
2. A config valid under release `X` parses and runs identically under the
   next release in the same line; a config using a newer release's key fails
   on `X` with exit 2 naming the unknown key (R1, with S1 landed).
3. A deprecated key produces a stderr warning naming its replacement and
   removal release, with byte-identical report output and unchanged exit code
   versus the non-deprecated form (R2).
4. Attestations and an evidence pack produced by an older released binary
   verify successfully with the newest `salvage verify` (R4).
5. A below-floor client against the hosted service receives the explicit
   unsupported-version error and exits 2; an at-floor client behaves normally
   (R5).
6. A repeated run of the same artifact + config across an in-line upgrade
   yields the same verdict and check results (R7).
