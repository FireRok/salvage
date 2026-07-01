# 0016 — Modular engines (a registry-driven engine SPI)

- **Status:** Proposed
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage's orchestration (`internal/engine`) used to hardcode Postgres: it
switched on `source.kind` and called `ephemeral.StartPostgres` /
`ephemeral.StartRestoreEnv` directly, then ran `checks`. `target.type` was
validated to be `"postgres"` and never consulted again. The source roadmap
(`0005`) already anticipates more source kinds and explicitly leaves the door
open for other engines later; the config already carries a `target.type` field
for exactly this.

Adding a second engine (MySQL, or any future DB/backup type) should not mean
editing the core orchestrator or the CLI. This spec introduces a small
**service-provider interface (SPI)** keyed by `target.type`, plus a registry, so
that "add MySQL" becomes: implement one interface, register it — with zero
changes to `internal/engine` or `cmd/salvage`.

This is a behaviour-preserving refactor. Postgres becomes the first registered
engine, wrapping the exact logic that was inline before. The config format is
unchanged (`target.type: postgres`).

## Goals

- One stable seam the orchestrator drives, independent of the concrete engine.
- Postgres registered as the first engine, with identical behaviour.
- Engine-specific commands (`last-good`, `fleet`) gated behind optional
  capabilities, so a non-Postgres engine that lacks them fails with a clear
  "not supported for target type X" rather than misbehaving.
- Adding an engine touches only new files.

## Non-goals

- Implementing a second engine (MySQL et al.) — this only builds the seam.
- Changing the config surface, source kinds, or any command's behaviour.
- Making `ephemeral`/`discover`/`scaffold` engine-generic — they stay
  Postgres-shaped and are simply *called from behind* the Postgres engine.

## Design

### The SPI (`internal/engine/spi`)

```go
type Engine interface {
    Type() string
    Restore(ctx, cfg) (RestoredTarget, warnings string, err error)
}

type RestoredTarget interface {
    checks.Queryer       // Query(ctx, sql) (string, error)   — for checks
    discover.RowQueryer  // QueryRows(ctx, sql) ([][]string, error) — for scaffold
    Stop() error         // teardown; idempotent
}
```

`Restore` stands up a throwaway environment, restores the backup into it, and
returns a live `RestoredTarget`. Its error contract preserves the existing
verdict-vs-operational split:

- **nil error + live target** → restore succeeded.
- **`*spi.Fault`** → an *operational* failure (Docker down, a missing `pass_env`
  secret, couldn't create the environment). The CLI exits non-zero without a
  verdict.
- **any other non-nil error** → the backup did not restore/recover — a normal
  `"fail"` verdict, not an operational error.

`Fault` wraps the operational cause; the orchestrator distinguishes the two cases
with `errors.As(err, &*spi.Fault)`.

`warnings` carries the benign-noise note (e.g. pg_restore ignored "already
exists" errors); it is surfaced in the report, never treated as failure.

### Optional capabilities

Two commands are inherently source-specific (pgBackRest-only today). They are
optional interfaces an engine may implement; the orchestrator type-asserts for
them and returns a clear error otherwise.

```go
type ChainTester interface {  // backs `last-good`
    Chain(ctx, cfg) ([]Backup, error)              // backups newest-first
    TestBackup(ctx, cfg, label string) string      // "" = pass, else reason
}

type FleetSurveyor interface { // backs `fleet`
    Survey(ctx, cfg) ([]FleetUnit, error)          // one entry per stanza
    SkeletonSource(cfg, unit string) config.Source // per-unit skeleton source
}
```

### The registry (`internal/engine/spi`)

`Register(Engine)` indexes an engine by `Type()`; `Lookup(type)` returns it or a
clear error naming the registered types. Registration happens at import time (in
each engine package's `init()`), so no locking is needed. Duplicate or empty
types panic — programmer errors surfaced at startup.

### The Postgres engine (`internal/engine/postgres`)

Implements `Engine`, `ChainTester`, and `FleetSurveyor`, wrapping the previously
inline logic: the `source.kind` switch (`pg_dump`/`sql` → `StartPostgres`,
`pgbackrest` → `StartRestoreEnv`), the `pass_env` precondition, the pgBackRest
`info`→`parse` chain read, per-label restore-tests, and the stanza survey. It
`Register`s itself in `init()`. `internal/ephemeral`, `internal/pgbrinfo`, and
`internal/discover` are unchanged and used from behind this engine.

### The orchestrator (`internal/engine`)

`Run`, `Scaffold`, `LastGood`, `Fleet` keep their exact signatures but become
thin: resolve the engine by `cfg.Target.Type` via `spi.Lookup`, then drive it
through the interface. `engine` blank-imports `engine/postgres` so the built-in
engine is wired in; that blank import is the single place engines are enabled.
`last-good`/`fleet` type-assert the optional capability and return "not supported
for target.type X" when absent.

## Requirements

**R1 — Engine SPI.** There MUST be an `Engine` interface keyed by `Type()
string` (the `target.type` it handles) whose `Restore` returns a live
`RestoredTarget` satisfying `checks.Queryer` and `discover.RowQueryer` plus a
`Stop()`. The orchestrator MUST depend only on this interface, not on any
concrete engine.

**R2 — Registry & dispatch.** A `Register(Engine)` / `Lookup(type)` registry MUST
exist. `engine.Run` MUST dispatch by `cfg.Target.Type` through the registry. An
unknown `target.type` MUST produce a clear operational error (exit non-zero, no
verdict).

**R3 — Postgres is the first engine.** Postgres MUST be a registered engine that
reproduces the prior behaviour exactly for `pg_dump`, `sql`, and `pgbackrest`
sources — same restore mechanics, same verdict-vs-operational split, same
warnings, same report fields.

**R4 — Operational-vs-verdict split preserved.** Environment/secret/Docker
failures MUST remain operational (exit 2, `restore.error` set, no verdict); a
backup that merely fails to restore MUST remain a `"fail"` verdict with a nil
operational error. `spi.Fault` carries this distinction.

**R5 — Engine-specific commands gated.** `last-good` and `fleet` MUST be exposed
via optional capability interfaces (`ChainTester`, `FleetSurveyor`). For an
engine that does not implement the relevant capability, the command MUST return a
clear "not supported for target.type X" error instead of failing obscurely.

**R6 — Additive extension.** Adding a new engine MUST require only: a new package
implementing `Engine` (+ optional capabilities), its `init()` registration, and a
blank import — with NO edits to `internal/engine`'s orchestration or the CLI
command wiring. (Config validation currently allow-lists `target.type`; see Open
questions.)

**R7 — No new dependencies.** The SPI and registry MUST use the standard library
only (the module keeps its single `gopkg.in/yaml.v3` dependency).

## How to add a new engine (e.g. `mysql`)

1. **New package** `internal/engine/mysql` with a type implementing
   `spi.Engine`:
   - `Type() string` → `"mysql"`.
   - `Restore(ctx, cfg)` → stand up a throwaway MySQL (a new `ephemeral`-style
     helper), restore the dump/backup, return a `RestoredTarget`. Wrap
     environment/Docker/secret problems in `spi.Faultf(...)`; return a bare error
     if the backup itself fails to restore.
   - The returned target implements `Query` (scalar, for checks) and `QueryRows`
     (rows, for scaffold introspection) over the MySQL client, and an idempotent
     `Stop()`.
2. **Register** it: `func init() { spi.Register(Engine{}) }`.
3. **Wire it in**: add `_ "salvage.sh/internal/engine/mysql"` to the blank-import
   block in `internal/engine/engine.go` (or a central `engines` file).
4. **Optional capabilities**: if the source has a testable backup chain,
   implement `spi.ChainTester` to light up `last-good`; if many units live under
   one repo, implement `spi.FleetSurveyor` to light up `fleet`. Skip either and
   the corresponding command cleanly reports "not supported".
5. **Config**: extend `config.Validate` to accept `target.type: mysql` and any
   MySQL-specific source kinds/defaults. (See Open questions — this allow-list is
   the one core touch-point remaining.)
6. **Introspection for scaffold**: `internal/discover` is Postgres-catalog
   specific. A MySQL engine wanting `scaffold` would add a MySQL discovery path;
   `run`/`check`/`last-good`/`fleet` need none of it.

Nothing in `internal/engine`'s orchestration changes for steps 1–4.

## Open questions

- **Config validation is still centralized.** `config.Validate` allow-lists
  `target.type == "postgres"` and knows the Postgres source kinds. Fully honoring
  R6 for validation would mean letting engines contribute validation (e.g. a
  `Validate(cfg)` on the SPI, or a registry-driven type check). Deferred: the
  orchestration seam is the valuable part; validation is a small, safe follow-up
  and keeping it as-is preserves today's exact error messages.
- **`ephemeral`/`discover` naming.** Left Postgres-shaped and unmoved by design;
  a future engine adds a sibling rather than generalizing these in place.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass.
2. Every command (`run`, `check`, `inspect`, `scaffold`, `fleet`, `last-good`,
   `attest`, `verify`, `schedule`, `login`, …) behaves exactly as before, with
   an unchanged config format (`target.type: postgres`).
3. `internal/engine` contains no `ephemeral.StartPostgres`/`StartRestoreEnv` call
   and no `source.kind` switch — it drives the SPI only. The Postgres mechanics
   live in `internal/engine/postgres`.
4. Adding a hypothetical engine requires only a new package + `init()`
   registration + blank import (plus the config allow-list per Open questions);
   no edit to the orchestration in `internal/engine`.
5. `last-good`/`fleet` on an engine lacking the capability return a clear "not
   supported for target.type X" error.
