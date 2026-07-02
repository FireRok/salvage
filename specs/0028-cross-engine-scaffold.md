# 0028 — Cross-engine scaffold (discovery beyond Postgres)

- **Status:** Implemented — R7 shipped for filesystem observation only (observe.* hints need a config surface); R8 (MongoDB) deferred as written; per-glob checksum opt-in needs a flag surface
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

`scaffold` is one of Salvage's best onboarding moments. [[spec 0009]] named the
reason plainly: hand-authoring a YAML target per backup is the #1 adoption
barrier, and the *checks* (intent) are the hard part. So `scaffold` restores a
known-good backup, introspects it, derives a strong baseline of checks with
thresholds computed from **observed current state**, verifies each by running,
and hands the operator a green-on-first-run config to curate — a meaningful,
passing starting point instead of a blank file.

Today that gift is **Postgres-only**, at two layers:

- **Dispatch.** `engine.Scaffold` type-asserts the restored target to
  `discover.RowQueryer` and gates off when the assert fails
  (`internal/engine/engine.go:120`, "scaffold is not supported for target.type
  %q"). Only the two Postgres restored targets implement `QueryRows`
  (`internal/ephemeral/postgres.go:125`, `internal/ephemeral/pgbackrest.go:211`).
  MySQL, MongoDB, restic, borg, and exec targets do not, so `scaffold` errors off
  for every one of them.
- **Introspection.** `internal/discover` is hard-wired to Postgres catalogs:
  `pg_database` (`discover.go:162`), `information_schema.tables`
  (`discover.go:174`), `information_schema.columns` (`discover.go:187`),
  `pg_extension` (`discover.go:216`), plus `timescaledb_information.*`. The row
  shapes, the check kinds it emits (`sql`), and the threshold heuristics are all
  Postgres-specific.

[[spec 0016]] and [[spec 0017]] deliberately left this Postgres-shaped:
`discover`/`scaffold` "stay Postgres-shaped and are simply *called from behind*
the Postgres engine" (0016 Non-goals), and 0017 R4 made discovery an explicitly
**per-engine (vertical)** concern — "each engine provides its own discovery or
simply omits `scaffold`," while the *emission* and *verify-by-running* safety net
"remain shared for any check kind the engine supports." Every subsequent engine
took the deferral: MySQL noted it is "one `RowQueryer` away" but that its
`information_schema` differs from Postgres's catalog (0024 Non-goals, Open
questions); restic/borg deferred a filesystem-walk discovery ([[spec 0018]],
[[spec 0022]]); and the exec engine, which has no catalog at all, got its own
observe-and-recommend proposal ([[spec 0021]]).

This spec cashes in 0017 R4. It **generalizes the discovery seam** so an engine
can opt into `scaffold` by implementing a small capability — exactly as
`spi.ChainTester`/`spi.FleetSurveyor` let an engine opt into `last-good`/`fleet`
(0016) — and it reconciles that generalization with [[spec 0021]]. The two are the
same idea (observe a restored artifact → propose checks that pin its current
shape → verify by running before emit); this spec makes them **one capability**,
so exec scaffold-assist and catalog scaffolding are two implementations of a
single seam rather than parallel code paths.

## Goals

- A per-engine **discovery capability** the `scaffold` command dispatches
  through, with **no Postgres assumption** baked into `internal/engine` — mirroring
  the optional-capability pattern already used for `last-good`/`fleet`.
- Two new discovery implementations that exercise the seam across the two
  fundamentally different engine shapes:
  - **MySQL** discovery via `information_schema`, emitting `sql`-kind checks
    (row-count floors, freshness) — a SQL engine, Postgres's sibling.
  - **restic/borg** discovery via a walk of the restored tree, emitting
    `file_exists` / `file_count` checks — a filesystem engine.
- The deterministic, offline, **verify-by-running** property preserved intact:
  candidates are run against the known-good snapshot and only emitted if they pass
  (0009 R5, 0021 R5).
- Wide-schema caps (0009 Open question) resolved into a shared, deterministic
  policy so a large database or a deep tree scaffolds to a reviewable config, not
  noise.
- **[[spec 0021]] folded in**: exec observe-and-recommend becomes one
  implementation of this capability, not a separate seam.
- No core-orchestrator change beyond generalizing the dispatch — adding discovery
  to an engine touches only that engine's package (0016 R6, 0017 R5).

## Non-goals

- **A new check kind.** MySQL reuses the existing `sql` kind (0024 R2);
  restic/borg reuse the existing `file_exists`/`file_count` kinds (0018, 0022);
  exec reuses its `http`/`file_*`/`command` kinds (0020, 0021). This spec adds a
  **discovery** seam, not an **evaluation** seam — the check-`kind` seam
  ([[spec 0017]] R3) is untouched, and every generated check is a check that
  already runs today.
- **Changing Postgres scaffold behaviour.** The Postgres discovery path
  (`internal/discover`, the TimescaleDB heuristics, the thresholds) is refactored
  *behind* the new capability with byte-identical output — Postgres becomes the
  first implementer, not a rewrite. Golden scaffold output for Postgres MUST NOT
  change.
- **LLM-assisted generation in the core.** As in 0009 R8 and 0021 R8, the core is
  deterministic, offline, and zero-egress; any AI assist is an opt-in layer that
  never invents an unverified assertion.
- **A discovery-only restore mode change.** `scaffold` still performs an ordinary
  restore through the engine SPI ([[spec 0016]]) and introspects the live target;
  it does not add a second restore path.
- **MongoDB is a later requirement, not v1.** The seam is designed to admit it
  (R8), but the shipping surface this spec commits to is MySQL + one filesystem
  engine.

## Design

### The discovery capability (`spi.Scaffolder`)

Today `engine.Scaffold` reaches past the SPI and type-asserts the *restored
target* to `discover.RowQueryer` (`engine.go:120`) — a Postgres-catalog interface
leaking into the orchestrator. We replace that with an **optional engine
capability**, dispatched exactly like `spi.ChainTester`/`spi.FleetSurveyor`:

```go
// spi.Scaffolder is an optional capability an Engine implements to light up
// `salvage scaffold`. An engine that omits it gates off with the existing
// "not supported for target.type X" message — no core change.
type Scaffolder interface {
    // Discover observes the just-restored target and proposes candidate checks
    // that pin its current shape. It MUST NOT run them — verification is the
    // orchestrator's shared job (Verify-by-running, below).
    Discover(ctx context.Context, rt RestoredTarget, cfg config.Config) ([]config.Check, error)
}
```

`engine.Scaffold` becomes: `spi.Lookup(cfg.Target.Type)` → restore → type-assert
the *engine* (not the target) to `spi.Scaffolder` → `Discover` → the **shared**
`verifyChecks` (already present at `engine.go:145`) → `scaffold.Render`. The
Postgres-only `rt.(discover.RowQueryer)` assert is deleted; the gate now keys on
the engine capability. `verifyChecks`, `scaffold.Build`, and `scaffold.Render`
are engine-agnostic already and stay put — the emission + verify-by-running layer
is horizontal (0017 R4).

Each engine's `Discover` is free to use whatever handle its target exposes: the
Postgres/MySQL SQL engines assert their target to a row-queryer; the restic/borg
filesystem engines assert to their `FileProber`/tree handle; the exec engine
reads its `observe.*` hints. The orchestrator knows none of this — it only knows
`spi.Scaffolder`.

### Postgres: the first implementer (behaviour-preserving)

The existing `internal/discover` logic moves behind
`postgres.Engine.Discover`, which asserts the restored target to
`discover.RowQueryer` and calls `discover.Introspect` +
`discover.GenerateChecks` verbatim. Output is byte-identical; this is the same
behaviour-preserving move 0016 made when Postgres became the first *engine*.

### MySQL discovery (`sql`-kind checks from `information_schema`)

MySQL is "one `RowQueryer` away" (0024 Open questions): its `*ephemeral.MySQL`
target already answers scalar `Query` for the `sql` evaluator (0024 R2). Adding a
`QueryRows(ctx, sql) ([][]string, error)` (the multi-row analogue, via
`mysql -N -B`, the same in-container `docker exec` discipline as its scalar
`Query`, 0024 R5) makes it introspectable. `mysql.Engine.Discover` then queries
MySQL's own `information_schema` — **not** Postgres's catalog — for the
MySQL-shaped facts:

- **User tables** from `information_schema.tables` where `table_schema` = the
  restored database (excluding MySQL's system schemas `mysql`,
  `information_schema`, `performance_schema`, `sys`).
- **Row-count floor** per non-empty table — a `kind: sql` check
  `SELECT count(*) FROM <t>` with `expect_min` = observed count (a floor, so
  growth doesn't false-FAIL; the same philosophy as 0009 R4 and 0021's floors),
  **advisory**.
- **Freshness** per table with an obvious datetime/timestamp column (from
  `information_schema.columns`, `data_type IN ('timestamp','datetime')`, name
  matching the 0009 pattern `created_at|updated_at|...`) — a `kind: sql` check
  `SELECT max(<col>) FROM <t>` with `max_age` = observed age × margin,
  **advisory**.
- **Structural presence** — `SELECT 1` reachability (**required**) and a
  user-table-count floor (**required**) — the MySQL analogues of Postgres's
  `server_reachable`/`schema_present` (0009 R4), so a scaffolded config is
  structurally guarded but never false-FAILs out of the box (0009 R6).

No TimescaleDB analogue and no new check kind: MySQL contributes **zero** new
evaluator code, reusing the `sql` kind exactly as 0024 R2 did for `run`.
Identifiers MUST be safely quoted (backtick-quoted for MySQL), the MySQL analogue
of 0009 R4's quoting rule.

### restic/borg discovery (file checks from a restored tree)

restic and borg restore a filesystem archive to a directory; their target
exposes a file/tree handle, and their evaluators (`file_exists`, `file_count`,
`checksum`, `command`) already exist (0018 R3, 0022 R3). `Discover` walks the
restored tree (bounded depth/count, deterministic order — no randomness, so
re-running is reproducible, per 0021 R2) and proposes:

- `file_exists` on a handful of **stable anchor files** (deterministic selection —
  top-level, lexically first, non-temp), **advisory**.
- `file_count -min` on the most-populated directories, floor = observed count,
  **advisory**.
- A structural presence check that the restore root is non-empty — **required**.

`checksum` proposals are **off by default** (0021 Open questions: many files
legitimately change), opt-in per glob. This is exactly the filesystem row of
0021's snapshot-now/assert-later table, now shared between the two dedicated
filesystem engines and the exec engine.

### Reconciling with [[spec 0021]] (unify, don't parallel)

[[spec 0021]] proposed a `ScaffoldObserver` capability on the exec engine
(0021 Design notes) with an observe-then-recommend flow (path walk / HTTP probe /
client-shelled SQL). That is **the same capability this spec generalizes** —
"observe a restored artifact, propose checks that pin its current shape, verify
by running before emit." Rather than ship two capabilities, this spec makes
`spi.Scaffolder` the **single** discovery seam and folds 0021 into it:

- 0021's `ScaffoldObserver` is `spi.Scaffolder` implemented by the exec engine.
  Its `Discover` reads the `observe.*` hints (`observe.path`, `observe.base_url`,
  `observe.dsn`+`observe.client`) and proposes `http`/`file_*`/`command` checks
  (0021 R1–R4).
- Catalog scaffolding (Postgres, MySQL) and tree-walk scaffolding (restic/borg)
  are the *other* implementations of the same interface. The difference is only
  *what the engine observes* (a SQL catalog vs. a file tree vs. caller hints); the
  emission, floors-not-pins philosophy, verify-by-running, honest-output header,
  and optional-AI layer are shared.
- 0021's profile library and AI-assist (0021 R6, R8) remain exec-specific
  refinements layered on its `Discover`; they are not core seam concerns.

Net: [[spec 0021]] is **not superseded** — its exec-specific observations,
profiles, and honest-scoping requirements still stand — but its *capability
plumbing* is unified with this spec's. Where 0021 says "an optional engine
capability the `scaffold` command type-asserts," that capability **is**
`spi.Scaffolder`.

### Wide-schema / deep-tree caps (resolving 0009's Open question)

0009 left "cap strategy for non-empty checks on wide schemas (top-N by size vs
all-advisory)" open, and 0021 flagged the same ("how many anchors/tables to
propose before it's noise"). This spec resolves both with one shared,
deterministic policy applied in the horizontal emission layer:

- Per-table / per-anchor generated checks are capped at a default **top-N by size**
  (tables by observed row count; directories by observed file count), N default
  50 (matching 0009 R4's table cap), overridable by flag.
- When the cap truncates, the emitted config carries a header note stating it was
  capped and how to widen — honest output (0021 R7, 0009 R7).
- Structural/presence checks are never capped (there are O(1) of them).

Because the cap lives in the shared emission path, every engine inherits it — the
policy is written once, not per engine.

### Verify-by-running (unchanged, shared)

The existing `verifyChecks` (`engine.go:145`) runs each candidate against the
just-restored target via `checks.Run` and keeps only those that pass — the 0009
R5 / 0021 R5 safety net. Because thresholds are derived from observed state, the
generated checks pass by construction; verification is the net that drops any
that don't. This layer does not change: it already dispatches by check `kind`, so
it verifies `sql`, `file_*`, `http`, and `command` candidates identically.

## Requirements

**R1 — Generalized discovery capability + dispatch.** There MUST be an optional
engine capability (`spi.Scaffolder`, or equivalently named) with a `Discover`
method that proposes candidate checks from a restored target. `engine.Scaffold`
MUST dispatch through it — resolving the engine via `spi.Lookup`, type-asserting
the **engine** to the capability — and MUST NOT type-assert the restored target
to `discover.RowQueryer` (the Postgres-only assumption at `engine.go:120` is
removed). An engine that does not implement the capability MUST gate off with the
existing "scaffold is not supported for target.type X" message. No change to
`internal/engine` beyond generalizing this dispatch, and none to the CLI (0016
R6).

**R2 — Postgres behaviour preserved.** Postgres MUST become the first implementer
of the capability, wrapping `internal/discover` unchanged. Scaffold output for a
Postgres/TimescaleDB target (structural checks, table floors, hypertable
`is_hypertable`/`chunks`/`fresh` checks, extension checks, thresholds, header,
ordering) MUST be byte-identical to today's. No `internal/discover` behaviour or
threshold changes.

**R3 — MySQL discovery producing verified `sql` checks.** The MySQL engine MUST
implement the discovery capability, introspecting MySQL's own
`information_schema` (never Postgres catalogs) via a multi-row query on its
existing in-container `mysql` client (no Go MySQL driver, per 0024 R5). It MUST
emit only `kind: sql` checks — reachability and user-table-count (**required**),
per-non-empty-table row-count floors and per-timestamp-column freshness bounds
(**advisory**) — with thresholds derived from observed state and identifiers
safely quoted. It MUST register **no new check evaluator** (reusing the `sql`
kind, 0024 R2).

**R4 — restic/borg tree-walk discovery producing verified file checks.** The
restic and borg engines MUST implement the discovery capability, walking the
restored tree (bounded, deterministic order) and emitting only existing
filesystem check kinds: a non-empty-root presence check (**required**),
`file_exists` on deterministically-selected stable anchor files and `file_count
-min` on the most-populated directories (**advisory**). `checksum` proposals MUST
default off and be opt-in per glob. No new check kind.

**R5 — Verify-by-running before emit (shared).** Every generated candidate, for
every engine, MUST be executed against the just-restored target via the shared
verify path (`verifyChecks`/`checks.Run`) and emitted only if it runs and passes;
a candidate that cannot pass MUST be dropped. The emitted config MUST be
green-on-first-run (0009 R5, 0021 R5). Severity discipline MUST hold: structural
checks `required`, heuristic checks `advisory` (0009 R6), so a scaffolded config
never false-FAILs out of the box.

**R6 — Wide-schema / deep-tree caps.** Per-table and per-anchor generated checks
MUST be capped by a shared, deterministic top-N-by-size policy (default 50,
overridable), applied in the horizontal emission layer so every engine inherits
it. When truncation occurs, the emitted config MUST carry a header note saying it
was capped and how to widen. Selection MUST be deterministic so re-running
`scaffold` is reproducible (0021 R2/AC6).

**R7 — Relationship to [[spec 0021]] (unified).** The exec engine's
observe-and-recommend flow ([[spec 0021]]) MUST be an implementation of this
spec's discovery capability, not a separate seam: 0021's `ScaffoldObserver`
becomes the exec engine's `spi.Scaffolder.Discover`. 0021's exec-specific
observations, profiles, AI-assist, and honest-output requirements still apply;
only the capability plumbing is unified. This spec MUST NOT introduce a second,
parallel discovery interface.

**R8 — Optional MongoDB discovery.** A later requirement, not v1: the MongoDB
engine ([[spec 0025]]) MAY implement the discovery capability via
`listCollections`/`listIndexes`, emitting its existing `collection_count` /
`doc_query` kinds (collection-count floors, presence). The seam MUST admit this
with no core change; shipping it is out of scope here.

**R9 — Deterministic, offline core; no new dependencies.** The core discovery +
emission path MUST require no network or LLM and be reproducible (0009 R8, 0021
R8). Any AI assist is an opt-in layer that never emits an unverified assertion (R5
still applies). No new Go dependency (stdlib + `gopkg.in/yaml.v3`); MySQL uses its
in-container `mysql` client, filesystem engines use `os`/`filepath`
(0021 R9, 0024 R8).

## Open questions

- **Where the discovery capability lives.** `spi.Scaffolder` referencing
  `config.Check` is clean (`config` is a leaf package, imported by engines
  already — 0017's import-cycle note). But `discover.RowQueryer` currently lives
  in `internal/discover`; whether MySQL grows a sibling `discover`-style package or
  keeps its introspection in `internal/engine/mysql` is an engine-local choice
  (0016 Open questions preferred "add a sibling rather than generalizing in
  place"). Recommend: engine-local discovery packages, shared only via
  `spi.Scaffolder` + the emission layer.
- **Freshness column selection when several timestamp columns exist** — inherited
  unresolved from 0009. A deterministic pick (first by ordinal, preferring
  `updated_at` over `created_at`) is the likely default.
- **Config surface for `observe.*` vs. catalog engines.** Exec needs `observe.*`
  hints (0021); catalog/tree engines need none. Whether the cap/anchor flags are
  shared CLI flags or per-engine config is a small follow-up.
- **restic/borg anchor heuristics.** "Stable anchor files" is easy to state, hard
  to make universally good; start with a conservative deterministic rule and widen
  as real archives teach us (mirrors 0021 Open questions).

## Acceptance criteria

1. `salvage scaffold` against a MySQL target (`target.type: mysql`, a real dump)
   emits a config of `kind: sql` checks that `config.Load` accepts and a fresh
   `salvage run` **passes** (green-on-first-run); the checks introspect MySQL's
   `information_schema`, not Postgres catalogs.
2. `salvage scaffold` against one filesystem engine (`restic` or `borg`, a real
   restored tree) emits a config of `file_exists`/`file_count` checks that pass on
   a fresh `salvage run`; no check is emitted for a path that isn't present.
3. Postgres scaffold output is unchanged — the golden Postgres/TimescaleDB
   scaffold config is byte-identical to before this spec, and
   `internal/discover`'s behaviour/thresholds are untouched.
4. `engine.Scaffold` no longer type-asserts the restored target to
   `discover.RowQueryer`; it dispatches through the engine capability, and an
   engine lacking it returns "scaffold is not supported for target.type X"
   (e.g. MongoDB until R8 lands).
5. Every emitted check, for every engine, passes when the emitted config is run
   immediately; a candidate that can't pass is dropped (R5). Structural checks are
   `required`, heuristic checks `advisory`.
6. Running `scaffold` twice against the same restored target produces identical
   output (deterministic); a wide schema / deep tree is capped with a header note.
7. The exec engine's scaffold-assist ([[spec 0021]]) is wired through the same
   `spi.Scaffolder` capability — there is exactly one discovery seam, not two.
8. `go build ./... && go vet ./... && go test ./...` pass; the report, verdict,
   signing/ledger/verify, and attestation surface carry no scaffold- or
   engine-specific structure (inherited unchanged, 0017 R1–R2).
