# 0004 — Check / assertion framework

- **Status:** Proposed
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

The checks are Salvage's verification surface — they encode what "healthy" means.
This is the part that carries the value (`0000`) and the only irreducible
configuration (`0001`: config-for-intent). Today a check is a scalar SQL query
plus one expectation (`expect_min`/`expect_max`, `equals`, `max_age`). This spec
defines how the framework grows **without losing simplicity** — SQL stays the
language.

## Goals

- Express common "healthy" assertions simply.
- Built-in structural checks that need **zero** configuration.
- Domain-aware checks (e.g. TimescaleDB) where they add real value.
- Read-only and safe by construction.

## Non-goals

- A general-purpose expression language (SQL is the language).
- The report schema (`0002`).

## Requirements

**R1 — Scalar expectations.** A check is a single-scalar SQL query with one
expectation: numeric bounds (`expect_min`/`expect_max`), `equals`, or freshness
(`max_age` over a timestamp). *(Implemented.)*

**R2 — Boolean predicates.** Support a check whose SQL returns a boolean that must
be true (e.g. `SELECT count(*) = 0 FROM orphaned_rows`).

**R3 — Read-only enforcement.** Checks MUST run read-only (a read-only transaction
or role); a check MUST NOT be able to mutate the restored cluster.

**R4 — Scoping.** A check targets a database. By default checks run against the
discovered user databases (`0001` R3); a check MAY pin a specific database.

**R5 — Severity.** Checks are `required` (fail the verdict) or `advisory` (warn
only). Default `required`.

**R6 — Built-in baseline checks (zero-config).** Structural sanity that needs no
operator input — server reachable, cluster has ≥1 user database, target schema
present — MUST run automatically and cheaply, catching gross failures even before
any custom check.

**R7 — Domain-aware checks.** Provide an opt-in library for common stacks — e.g.
TimescaleDB hypertable freshness ("latest chunk newer than X"), continuous
aggregate up-to-date. (Divina is TimescaleDB, so this carries immediate weight.)

**R8 — Result shape.** Each check yields `{name, ok, got, detail, severity}`,
feeding the report (`0002`) and never including raw data beyond the intentionally
asserted scalar (`0003` R7).

**R9 — Determinism.** A check's pass/fail MUST be a pure function of the restored
data and the expectation — except intentionally time-relative checks (`max_age`),
whose tolerance MUST be documented.

## Open questions

- How large a built-in check library to ship vs leaving everything custom.
- Auto-generating checks from schema is tempting but treacherous ("every table
  non-empty" is often wrong); "tables non-empty at backup time stay non-empty"
  needs source-side metadata — worth it?
- Templating a check across many tables/hypertables.

## Acceptance criteria

1. The four scalar expectation types validate and evaluate correctly (R1).
2. A boolean-predicate check passes/fails correctly (R2).
3. A check attempting a write is prevented or fails (R3).
4. Baseline checks run when no checks are configured (R6).
5. Results carry severity and never leak raw data (R5/R8).
