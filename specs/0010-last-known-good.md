# 0010 — Last-known-good recovery-point discovery

- **Status:** Implemented
- **Created:** 2026-06-30
- **Owner:** Firerok

## Context

When the latest backup fails to restore, the only question that matters in an
incident is: *"what is my freshest **restorable** point, right now?"* Salvage can
answer it directly — restore-test the backup chain newest→oldest and report the
first one that passes. This is the capability that makes the name honest: it turns
*"your backup is bad"* into *"…and here's the one that isn't."*

**Honest scope:** this finds the freshest *good* backup. It does **not** repair a
corrupt backup, reconstruct missing WAL, or recover data that was never captured.
Salvage is a verifier, not a data-recovery tool — and must never be sold as one.

## Goals

- `salvage last-good` enumerates the pgBackRest backup chain, restore-tests it
  newest→oldest, stops at the first PASS, and reports that backup as the recovery
  point — plus the newer backups that failed, with reasons.

## Non-goals

- Repairing, extracting from, or partially recovering a bad backup (a "what
  survived" report is a possible later feature, noted in Open questions).
- Logical (non-chain) sources in v1 — pgBackRest first (it has a real backup
  history; a directory of dated dumps could come later).
- Exhaustively testing every backup — stop at the first good one.

## Requirements

**R1 — `salvage last-good` command.** For a pgBackRest source, find and report the
freshest restorable backup. Exit `0` if one is found, `1` if **none** restore, `2`
on operational error.

**R2 — Enumerate the chain.** Read `pgbackrest --stanza=<s> info --output=json` and
produce the stanza's backups ordered **newest first** — `{label, type, timestamp}`.
Parsing lives in `internal/pgbrinfo` as a pure, testable function.

**R3 — Restore-test newest→oldest.** For each backup, stand up a fresh isolated
restore environment, restore **that specific backup** (`pgbackrest --set=<label>`),
and run the configured checks — the same isolation/config-synthesis/network rules
as `salvage run`. Record the verdict and reason per backup. **Stop at the first
PASS.**

**R4 — Report.** Emit (JSON + human): the **last-known-good** backup (label,
timestamp, age) as the recovery point, and each *newer* backup that failed with its
reason. If none restore, say so clearly.

**R5 — Reuse, don't fork.** Build on the existing restore + checks machinery; a
per-backup test is exactly `run` pinned to a backup label.

**R6 — Bound + log the search.** Support an optional cap (`-max N` backups to try;
default: until the first PASS). Always report what was tried — no silent caps.

**R7 — Honest scope in output.** The report identifies the freshest restorable
point; it MUST NOT imply repair or extraction.

## Design

```
salvage last-good
  → StartRestoreEnv → `pgbackrest --stanza=s info --output=json`
  → pgbrinfo.Parse(json, stanza)  → []Backup (newest first)
  → for b in backups (newest→oldest):
       fresh restore env → Restore(stanza, set=b.Label) → run checks → record
       if PASS: recovery point = b; stop
  → render report (recovery point + newer failures)
```

`internal/pgbrinfo` contract:

```go
type Backup struct {
    Label     string
    Type      string    // full | incr | diff
    Timestamp time.Time // backup stop time
}
// Parse parses `pgbackrest info --output=json` and returns the named stanza's
// backups ordered NEWEST FIRST (by stop time).
func Parse(infoJSON []byte, stanza string) ([]Backup, error)
```

Integration adds `PgBackRest.Info(stanza)` and a backup-set argument to the
pgBackRest restore (`--set=<label>`, empty = latest), an `engine.LastGood`, a
report type, and the CLI command.

## Open questions

- "What survived" — partial-restore reporting from a damaged backup (a real
  salvage-the-data capability, but a different mechanism).
- Cost: each tested backup is a full restore. Search is serial in v1; a bounded
  parallel search could speed worst cases.
- Logical sources: a directory of dated dumps as a pseudo-chain.

## Acceptance criteria

1. Given a chain whose newest backup is corrupt and an older one is good,
   `salvage last-good` skips the corrupt one and reports the older as the recovery
   point, including the newer backup's failure reason.
2. Exit `0` with a recovery point when one restores; `1` when none do.
3. The report never implies it repaired or extracted anything (R7).
