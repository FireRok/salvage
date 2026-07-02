# 0009 — Config scaffolding (deterministic, with optional AI assist)

- **Status:** Implemented
- **Created:** 2026-06-30
- **Owner:** Firerok

## Context

Hand-authoring a YAML target per backup is the #1 adoption barrier — a DBA won't
write one per database across a fleet. The boilerplate (image, stanza, creds) is
deterministic; the *checks* (intent) are the hard part. But Postgres/TimescaleDB
catalogs are rich enough to derive a strong, safe baseline **deterministically**,
with thresholds computed from **observed current state** (not guessed). An
optional LLM layer can later enrich with semantic prioritization, but the core
must be deterministic, offline, reproducible, and zero-egress — properties a
trust/compliance tool needs more than semantic polish.

## Goals

- `salvage scaffold` generates a runnable target YAML with sensible
  auto-generated checks, deterministically.
- Thresholds derived from observed state; heuristic checks emitted **advisory**;
  structural checks **required**.
- No LLM dependency in the core; works air-gapped.

## Non-goals

- LLM-assisted check generation (separate, optional, later spec — a `--smart`
  layer that reads schema + aggregate stats, never raw rows).
- Auto-enumerating *all* pgBackRest stanzas (v1 scaffolds one target given the
  source basics; fleet enumeration is a follow-on).

## Design

```
salvage scaffold
  → [discovery restore]                      (reuse the existing restore engine)
  → discover.Introspect(RowQueryer, db)       → Discovery
  → discover.GenerateChecks(Discovery)        → []config.Check
  → [verify each check runs+passes; drop failures]
  → scaffold.Render(*config.Config)           → YAML (stdout / -o file)
```

Two new packages are independently buildable and testable:
- `internal/discover` — catalog introspection + check generation (pure logic
  against a `RowQueryer` interface; testable with fakes).
- `internal/scaffold` — assemble a `config.Config` and render YAML.

The CLI command, the multi-row query method on the restore environment, and the
glue that ties them to a discovery restore are integration work.

## Requirements

**R1 — `salvage scaffold` command.** Takes the same source/restore inputs as
`run` (flags or a partial config) and emits a complete target YAML (stdout by
default; `-o <file>` to write). It performs a discovery restore to introspect.

**R2 — Multi-row query.** The restore environment MUST expose
`QueryRows(ctx, sql) ([][]string, error)` so introspection can read catalog rows
(the existing scalar `Query` is insufficient). *(Integration: implemented on the
ephemeral restore types.)*

**R3 — Catalog introspection (`internal/discover`).** Given a `RowQueryer` and a
database name, introspect via system catalogs (no LLM): non-template databases;
user tables (schema, name, current row count); timestamp/timestamptz columns per
table; TimescaleDB hypertables and their time column (via
`timescaledb_information.hypertables` when timescaledb is installed); installed
extensions. Return a structured `Discovery`. Table/column discovery MUST exclude
Postgres catalogs **and** TimescaleDB's internal implementation schemas
(`_timescaledb_internal`, `_timescaledb_catalog`, `_timescaledb_config`,
`_timescaledb_cache`) so a hypertable's per-chunk tables do not generate noisy
per-chunk checks — the hypertable itself is covered by its own checks (R4).

**R4 — Deterministic check generation (`internal/discover`).**
`GenerateChecks(*Discovery) []config.Check` produces:
- `server_reachable` — `SELECT 1` `equals "1"` — **required**.
- `has_user_database` — count of non-template databases `expect_min 1` — **required**.
- `schema_present` — count of user tables `expect_min 1` — **required**.
- per non-empty user table (capped, default 50; note if capped) —
  `SELECT count(*) FROM (SELECT 1 FROM <t> LIMIT 1) x` `expect_min 1` — **advisory**.
- per TimescaleDB hypertable, three checks — all **advisory**:
  - `<ht>_is_hypertable` — `count(*) > 0 FROM timescaledb_information.hypertables`
    for that schema/name, `bool true`. Guards the silent-restore failure where
    the data comes back but the TimescaleDB catalog registration is lost, so the
    hypertable degrades to an ordinary table (partitioning/compression/retention
    policies gone).
  - `<ht>_chunks` — `count(*) FROM timescaledb_information.chunks` for that
    schema/name, `expect_min` = observed chunk count. Emitted only when the
    observed count is > 0 (a `>= 0` floor asserts nothing). Guards a hypertable
    that restored with fewer chunks than it had.
  - `<ht>_fresh` — `SELECT max(<time_col>) FROM <ht>` with `max_age` = observed
    age × margin (default 5×, floor +24h).
- per plain table with one obvious timestamptz column (name ~
  `created_at|updated_at|inserted_at|ts|time|event_time`) — same freshness check
  — **advisory**.
- per required-preload extension — a boolean check that it is loaded — **advisory**.
Thresholds MUST be derived from observed values in `Discovery`, never hard-coded.
Identifiers MUST be safely quoted.

**R5 — Verify-by-running.** Each generated check SHOULD be executed against the
discovery cluster; only checks that run and pass on the known-good snapshot are
emitted. (Threshold-from-observed makes them pass by construction; this is the
safety net.)

**R6 — Severity discipline.** Structural/presence checks are `required`; every
heuristic check (non-empty, freshness, counts, extensions) is `advisory` — so a
scaffolded config never false-FAILs out of the box; a human promotes the keepers.

**R7 — YAML emission (`internal/scaffold`).** `Render(*config.Config) ([]byte, error)`
renders YAML with a header comment ("auto-generated by salvage scaffold; review
advisory checks and promote the keepers to required"). Output MUST re-parse via
`config.Load` (round-trips).

**R8 — No LLM in core.** The scaffold path MUST NOT require any network or LLM.
The optional `--smart` AI-assist is a separate future spec.

## Open questions

- Cap strategy for non-empty checks on wide schemas (top-N by size vs all-advisory).
- Freshness column selection when several timestamp columns exist.
- Introspecting the live source (read-only role) as an alternative to a discovery
  restore.

## Acceptance criteria

1. `salvage scaffold` against the local demo produces YAML that `config.Load`
   accepts and `salvage run` passes.
2. Generated structural checks are `required`; freshness/non-empty are `advisory`.
3. A TimescaleDB hypertable yields a freshness check on its actual time column
   (threshold from observed data), an `is_hypertable` registration check, and —
   when it has chunks — a chunk-count floor derived from the observed count.
4. The core scaffold runs with no network/LLM.
