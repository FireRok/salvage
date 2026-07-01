# 0000 — Product overview & restore-test model

- **Status:** Accepted
- **Created:** 2026-06-29
- **Owner:** Firerok

## What Salvage is

Salvage proves a backup actually **restores into a working database** — and
attests to it. It is the restore-test + attestation runner, **not** a backup tool.

## The three levels of backup confidence

| Level | Proves | Who does it |
|------:|--------|-------------|
| 1 | the backup job ran | your cron job |
| 2 | the bits aren't rotted (integrity) | `restic check`, `borg check`, `kopia verify`, pgBackRest manifests |
| 3 | **it restores and the data works** | **Salvage** |

The level-2 tools stop one short — by their own admission a real restore is the
gold standard but too expensive to run by default. Salvage does exactly the
level-3 test, on a schedule, and produces a verdict (and an attestation) you can
act on.

## Core principle: augmentation, not replacement

Integrity (level 2) is **necessary but not sufficient**. "Will it restore?" is a
property of *artifact × restore-procedure × target-environment*, composed and
executed — an empirical, runtime property. Salvage performs the restore to prove
the *sufficient* condition. It **augments** the deterministic checks; it does not
replace them.

## Honesty: representative, not universal

A successful restore-test proves the backup restores **in the environment Salvage
used** — a strong sample, not a mathematical guarantee. The closer the restore
environment mirrors production, the more the result generalizes. Reports record
the environment used so the claim is honestly scoped.

## Requirements

**R1 — Three-level model.** Salvage's positioning and docs MUST frame it as the
level-3 layer atop existing integrity checks (not a replacement).

**R2 — Run loop.** A run MUST: restore the backup into an isolated, ephemeral
environment (`0003`), run assertions (`0004`), and emit a verdict + report
(`0002`).

**R3 — Verdict.** The verdict is `pass` iff the restore succeeded **and** every
required check passed; otherwise `fail`.

**R4 — Exit codes.** `0` = pass; `1` = verdict fail (restore failed or a check
failed — a *result*, not a crash); `2` = operational error (bad config, Docker
unavailable, missing secret). This distinction MUST be preserved.

**R5 — Augmentation.** Salvage MUST NOT position itself as a substitute for
integrity verification; the two compose.

**R6 — Representativeness.** Reports MUST record the restore environment (image,
PG/extension versions); claims are scoped to it.

**R7 — Boundaries.** Salvage is not a backup tool; transport is delegated
(`0005`); managed-provider snapshots and non-PostgreSQL engines are out of scope.

## Relationship to other specs

`0001` environment auto-detection · `0002` reporting & attestation · `0003`
security & isolation · `0004` check framework · `0005` source interface & scope ·
`0006` PITR validation.

## Acceptance criteria

1. A run against a good backup yields verdict `pass`, exit `0`.
2. A failing check yields verdict `fail`, exit `1`; a missing required secret
   yields exit `2`.
3. The report records the restore image and detected PG/extension versions.
