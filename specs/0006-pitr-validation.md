# 0006 — Recovery-target / PITR validation

- **Status:** Proposed
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

Backups are not just "latest." The real disaster-recovery question is often *"can
I recover to a specific point — just before the bad migration, to 03:00 yesterday,
to a named restore point?"* PostgreSQL + pgBackRest support recovery targets
(`time`, `xid`, `name`, `lsn`, `immediate`, `latest`). Salvage should let an
operator specify a target and **verify the cluster actually reaches it** — a
capability the integrity-only world can't touch, and a strong differentiator.

## Goals

- Restore-test to a specified recovery target, not only latest.
- Verify the target was actually **reached** (not just that the server started).
- Optionally prove the whole PITR window is restorable (coverage).

## Non-goals

- Implementing recovery (delegated to the backup tool, `0005`).
- Choosing the target for the operator (that's intent).

## Requirements

**R1 — Recovery target config.** Support `latest | immediate | time=<ts> |
xid=<id> | name=<restore_point> | lsn=<lsn>`, passed through to the restore tool.
Default: `latest`.

**R2 — Reached-target verification.** Salvage MUST verify recovery reached the
requested target — confirm recovery completed and the achieved recovery point
(last replayed LSN/time) satisfies the target — not merely that Postgres accepted
connections.

**R3 — Target-aware assertions.** Checks MAY assert state appropriate to the target
(e.g. "a row written after T is absent when recovering to T"), proving PITR landed
where intended rather than just somewhere.

**R4 — Recovery coverage (optional).** Test multiple targets sampled across the
retention window to prove the window is restorable, not just the newest backup, and
report coverage.

**R5 — Timeline handling.** A targeted recovery may branch a new timeline; Salvage
MUST handle and record it.

**R6 — Evidence.** The report MUST record the requested target and the achieved
recovery point (LSN/time), feeding attestation (`0002` R6).

## Open questions

- Precisely verifying "reached target" across target types (time vs xid vs lsn).
- Coverage sampling strategy (window endpoints + interior points?) and its cost.
- `promote` vs `pause` at the target, and querying at a paused recovery point
  (hot-standby read-only).

## Acceptance criteria

1. A run with `target=time` recovers to that time and the report records the
   achieved point (R1/R2/R6).
2. A target-aware check correctly distinguishes data present *at* vs *after* the
   target (R3).
3. A coverage run reports pass/fail across N sampled targets (R4).
