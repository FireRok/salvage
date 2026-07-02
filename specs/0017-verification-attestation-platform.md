# 0017 — The verification & attestation platform (backup-type-agnostic)

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

GTM positioning crystallized the architecture thesis (positioning §8, "the moat is
horizontal, the engine is vertical"): Salvage is Postgres-only *for the restore-test
engine* today, but that is a limitation of **one layer**, not the product. [[spec 0016]]
built the vertical seam — the engine SPI keyed by `target.type`. This spec defines the
**horizontal platform** every engine inherits — validation orchestration, the verdict/
report, independent attestation, and cadence monitoring — and specifies how **validation
generalizes to non-SQL backup types** so the platform can grow toward "prove and attest
*any* backup," one engine at a time.

## The model: vertical engines, horizontal platform

- **Vertical (per backup type — [[spec 0016]]):** *restore* and *discovery*. Restoring
  and asserting a Postgres dump is Postgres-specific work; MySQL, restic/borg filesystem
  snapshots, MongoDB, and object-storage artifacts each need their own engine.
- **Horizontal (across all types):** the validation framework's *orchestration*, the
  *verdict/report*, the *signing / ledger / verify / attestation* surface
  ([[spec 0012]]), and the *dead-man's-switch* cadence monitor ([[spec 0015]]). None of
  these care what produced the report.

**Growth thesis:** every new engine multiplies the reach of a moat already built, without
rebuilding it. Salvage is a modular verification-and-attestation platform; backup types
are pluggable engines that each inherit the same independent-attestation surface.

## The lifecycle — which layer owns each step

| Step | Layer | Owner |
|---|---|---|
| 1. Restore into a throwaway environment | **vertical** | the engine ([[spec 0016]]) |
| 2. Validate (run checks) | **mixed** | *check evaluation* is engine-shaped; *orchestration* is horizontal |
| 3. Verdict + report | **horizontal** | `internal/report` — backup-type-agnostic |
| 4. Attest (counter-sign into the ledger) | **horizontal** | the notary ([[spec 0012]]) |
| 5. Monitor (alert on overdue cadence) | **horizontal** | the dead-man's-switch ([[spec 0015]]) |

## Requirements

**R1 — Attestation is backup-type-agnostic.** The notary MUST counter-sign any Salvage
report regardless of the engine that produced it. Nothing in the submit / ledger / verify
/ pubkey / cadence path may assume Postgres. (This is already true — the notary receives a
report and signs it; lock it in and keep it so as engines are added.)

**R2 — The report is backup-type-agnostic.** `report.Report` MUST NOT carry
Postgres-specific structure. A check result is generic — `{name, ok, severity, got,
detail, error}` — regardless of what was checked. The verdict rule (pass iff restore
succeeded and every *required* check passed) is engine-independent. (Already true; this
spec locks it as an invariant.)

**R3 — Validation: horizontal orchestration, engine-provided evaluation.** Today a `Check`
is one SQL statement returning a scalar, evaluated via `checks.Queryer` — correct for
database engines. The framework MUST evolve so that:
- a check has a **kind** (default `sql` — today's behaviour, unchanged), and
- the **engine** knows how to *evaluate* the kinds it supports against its
  `RestoredTarget`, while
- the **orchestration** ([[spec 0004]]: iterate checks, apply `severity`, aggregate to a
  pass/fail verdict) stays shared and engine-independent.

An engine asked to evaluate a kind it does not support MUST return a clear error. This
seam is what lets a non-SQL engine (e.g. restic/borg filesystem) validate via
file-presence / checksum / command probes instead of SQL — with the verdict, report, and
attestation path completely unchanged.

**R4 — Discovery/scaffold is per-engine (vertical).** `scaffold`'s introspection is
engine-specific (Postgres system catalogs today, [[spec 0009]]). Each engine provides its
own discovery or simply omits `scaffold`. The *emission* of generated checks and the
*verify-by-running* safety net remain shared for any check kind the engine supports.

**R5 — Engines grow one at a time, each inheriting the horizontal layer for free.** A new
engine provides only its *restore* + *check evaluation* (+ optional discovery, chain, and
fleet capabilities per [[spec 0016]]); it inherits R1–R2 (report + attestation +
monitoring) at no cost. Priority order (positioning §8): **restic filesystem
snapshots (in progress — [[spec 0018]], the first non-SQL engine)**, then **borg**,
**MySQL**, **MongoDB**, and **object-storage artifacts**. Orthogonal to those
type-specific engines is the **exec engine ([[spec 0020]])** — a
bring-your-own-restore engine that runs the customer's own restore command and
validates it with Salvage-format checks (`http`/`command`/`file_*`/client-shelled
SQL), extending the platform to *any* restore procedure without a dedicated
engine, with recommend-my-checks onboarding via [[spec 0021]].

**R6 — Honest scoping (positioning §9).** Present tense is "Postgres-first" /
"database-first, starting with Postgres" — claim only what runs. The north star is "prove
and attest any backup." Roadmap language MUST never read as a present-tense capability: a
backup type Salvage does not yet verify is a roadmap item, not a feature. This applies to
the CLI help, the site, and the specs alike.

## Design notes — the validation-generalization seam (R3)

- A `config.Check` today: one `sql` statement + one expectation (`expect_min`/`expect_max`
  /`equals`/`max_age`/`bool`), evaluated by `checks.Run` via `Queryer`. Rename this the
  **`sql` check kind** (the implicit default) rather than the only possibility.
- Add an optional `kind` to `config.Check` (default `sql`). `checks.Run` dispatches by
  kind to an evaluator the engine supplies; severity handling, aggregation, and result
  shape are unchanged. Postgres registers the `sql` evaluator; behaviour is identical.
- Illustrative future kinds (each an engine's concern, not the core's):
  - restic/borg filesystem: `file_exists`, `file_count`, `checksum`, `command`.
  - MongoDB: `collection_count`, `doc_query`.
  - object-storage: `object_present`, `object_bytes`, `manifest_matches`.
- A check *result* is `{name, ok, severity, got, detail}` for every kind — so the report,
  verdict, ledger, verify page, and dead-man's-switch need no change to add a kind.

Import-cycle note: `config` is a leaf package, so the kind evaluators live with the
engines (which already import `config`), not in `config`. `checks.Run` gains a small
kind→evaluator lookup, keeping the orchestration engine-agnostic.

### The seam as built (implementation note)

The seam is implemented as:

- `config.Check.Kind` (`yaml:"kind,omitempty"`); an empty kind means `"sql"`.
- `internal/checks` owns the horizontal orchestration:
  - `type Target = any` — the restored thing, passed opaquely. Each evaluator
    *type-asserts* it to the capability that kind needs (the `sql` evaluator to
    `checks.Queryer`), which is what decouples orchestration from SQL.
  - `type Evaluator func(ctx, Target, config.Check) report.CheckResult` and a
    `RegisterEvaluator(kind, Evaluator)` registry (populated at `init()`).
  - `checks.Run(ctx, Target, []config.Check)` dispatches each check by kind
    (empty → `"sql"`), applies severity, and aggregates exactly as before. An
    unknown kind yields a failing `CheckResult` (`unknown check kind "X"`), not a
    panic.
  - The built-in `sql` evaluator (`internal/checks/sql.go`) registers itself and
    carries the *verbatim* pre-seam expectation logic.
- `spi.RestoredTarget` is now minimal (`Stop() error`). The Postgres target keeps
  `Query`/`QueryRows`, so the `sql` evaluator's `Queryer` assert succeeds and
  `scaffold` still introspects via a `discover.RowQueryer` assert (gated with a
  "scaffold not supported for target.type X" error when absent, mirroring
  last-good/fleet).

**For the restic/borg engine that comes next** (now built for restic — [[spec 0018]]):
implement `spi.Engine` for
`target.type: "restic"` (or `"filesystem"`) whose `Restore` returns a
`RestoredTarget` exposing a filesystem/command handle (e.g. a `FileProber`
interface) instead of `Query`. In that package's `init()`, call
`checks.RegisterEvaluator("file_exists", …)`, `"file_count"`, `"checksum"`,
`"command"`, each type-asserting the `Target` to that handle. A config then uses
`kind: file_exists` checks; `checks.Run`, the verdict, the report, the ledger, the
verify page, and the dead-man's-switch are all inherited unchanged. The engine
omits `discover.RowQueryer`, so `scaffold` returns "not supported" for it until it
grows its own discovery — no core change required.

## Non-goals (this spec)

- Implementing a second engine or a non-`sql` check kind — this spec defines the platform
  contract and the seam; the first non-Postgres engine (restic/borg) is the follow-on
  feature that exercises it.
- Changing today's Postgres behaviour, config format, or the `sql` check semantics.

## Acceptance criteria

1. The specs describe one **horizontal platform** (validation orchestration + report +
   attestation + monitoring) that every engine inherits, and a per-engine **vertical**
   layer (restore + check evaluation + discovery).
2. The attestation / ledger / verify / cadence path is confirmed to carry no
   Postgres-specific assumptions (R1–R2), and this is stated as an invariant for future
   engines.
3. There is a concrete, import-safe plan (R3 seam: check `kind` + engine-provided
   evaluators) that lets a non-SQL engine validate without SQL — ready to be exercised by
   the restic/borg engine next.
4. The engine roadmap is named with honest present-tense scoping (R5–R6).
