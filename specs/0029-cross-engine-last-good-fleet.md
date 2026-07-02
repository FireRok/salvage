# 0029 — Cross-engine last-good & fleet

- **Status:** Implemented — R8 (parallel search) and R9 (dumpdir pseudo-chain) deferred per the spec
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Two of Salvage's most valuable commands — `last-good` ([[spec 0010]]) and `fleet`
([[spec 0011]]) — are still nailed to pgBackRest. Each was born as a
pgBackRest-specific capability; [[spec 0016]] then lifted them onto optional SPI
capabilities (`spi.ChainTester` backs `last-good`, `spi.FleetSurveyor` backs
`fleet`) so any engine *could* implement them. But the orchestrator never
finished the job: it gates each command on **both** the capability assertion
**and** a hard `source.kind == "pgbackrest"` string check.

Concretely, in `internal/engine/engine.go`:

- `LastGood` type-asserts `spi.ChainTester`, and then immediately re-gates on
  `cfg.Target.Source.Kind != "pgbackrest"` (`engine.go:174`), returning
  "last-good supports pgbackrest sources only".
- `Fleet` type-asserts `spi.FleetSurveyor`, and then re-gates on the same string
  (`engine.go:220`), returning "fleet supports pgbackrest sources only".

Only the Postgres engine implements either capability, so the string check is
redundant *today* — but it is also a wall. The restic ([[spec 0018]]) and borg
([[spec 0022]]) engines each restore from a repository that holds a real
**history** of snapshots/archives — exactly the shape `last-good` walks and
`fleet` surveys — yet both specs' Open questions defer the capability with the
same sentence: it is "a small, safe follow-up once the gate is generalized." This
spec generalizes the gate and lights up the follow-up.

Alongside the coupling, there is a latent correctness bug in `fleet`'s exit
contract. `last-good` has a real failure signal: `cmdLastGood` exits `1` when no
backup restored (`cmd/salvage/main.go:140-142`, `if lg.RecoveryPoint == nil`).
`cmdFleet` (`cmd/salvage/main.go:165-188`) has **none** — its only non-zero exits
are `os.Exit(2)` for config/operational errors (config load and the `engine.Fleet`
error path); a successful survey **always** exits `0`, even when the repo is empty
or every stanza is degraded. For cron/CI that watches fleet health, "all my
stanzas are broken" is indistinguishable from "all healthy." This spec gives
`fleet` a failure signal consistent with `last-good` and the 0/1/2 contract.

## Goals

- Decouple `last-good` and `fleet` from `source.kind == "pgbackrest"`: dispatch
  **purely** on the `spi.ChainTester` / `spi.FleetSurveyor` capabilities, so any
  engine that implements them lights up the command with **zero further
  orchestrator change** (finishing [[spec 0016]] R5).
- A restic `spi.ChainTester` and a borg `spi.ChainTester`: walk the repository's
  snapshots/archives newest→oldest, restore-test each until one passes, and report
  it as the recovery point — the [[spec 0010]] capability, on a filesystem engine.
- A restic and borg `spi.FleetSurveyor`: a cheap, metadata-only enumeration of the
  repository's snapshots/archives — the [[spec 0011]] survey shape, no restore.
- Fix `fleet`'s exit-code contract: a degraded or empty survey MUST return
  non-zero, consistent with `last-good` and the 0/1/2 contract.

## Non-goals

- **Changing what `last-good`/`fleet` mean.** The report types (`report.LastGood`,
  `report.Fleet`, `report.StanzaSummary`, `report.BackupVerdict`), their JSON, and
  the human rendering are unchanged. This spec adds *engines that satisfy the
  existing capabilities* and *fixes one exit code* — it does not reshape the
  commands.
- **A hosted, cross-repo, always-on fleet dashboard.** That remains the hosted
  plane ([[spec 0008]], referenced from [[spec 0011]]). This is the local,
  one-repo, one-shot enumeration only.
- **MySQL/MongoDB last-good/fleet.** A single logical dump has no chain and no
  multi-unit repo; MySQL ([[spec 0024]]) and MongoDB (0025) implement neither
  capability and continue to report "not supported for target.type X" via the
  (now capability-only) gate. A physical/binlog MySQL path would be the natural
  future home ([[spec 0024]] Open questions), not this spec.
- **Repair or extraction.** Unchanged from [[spec 0010]] R7: `last-good` reports
  the freshest *restorable* point; it never repairs a corrupt backup, reconstructs
  missing data, or implies "what survived." Salvage is a verifier, not a
  data-recovery tool, and must never be sold as one.
- **`scaffold` for restic/borg.** Still gated off (no `discover.RowQueryer`);
  orthogonal to this spec.

## Design

### The coupling removal (`internal/engine`)

The two hard `source.kind` checks are deleted:

- In `LastGood`, drop the `if cfg.Target.Source.Kind != "pgbackrest"` block
  (`engine.go:174`). Dispatch is the `spi.ChainTester` assertion and nothing
  more; an engine that lacks the capability still returns the existing
  "last-good is not supported for target.type X" error.
- In `Fleet`, drop the equivalent block (`engine.go:220`). Dispatch is the
  `spi.FleetSurveyor` assertion and nothing more.

This is the whole coupling fix: the capability *is* the gate. It is exactly the
generalization [[spec 0016]] R5 intended (`last-good`/`fleet` "exposed via
optional capability interfaces") and that [[spec 0018]]/[[spec 0022]] deferred.
Postgres behaviour is unchanged — the pgBackRest engine still implements both
capabilities and the `stanza`/`--set=<label>` mechanics behind them exactly as
before; only the redundant outer string check disappears.

Because the capability is now the sole gate, the `report.LastGood.Stanza` field
(populated from `cfg.Target.Source.Stanza`, a pgBackRest-only field) becomes a
pgBackRest-only detail. For filesystem engines it is simply empty — the
`ChainTester` reports its own unit identity (repository/snapshot labels) in the
per-backup verdicts, and the report renders a blank stanza line without special
casing.

### restic `ChainTester` (`internal/engine/restic`)

restic already restores a *single* snapshot (default `latest`) into a throwaway
container. A `ChainTester` generalizes that to the repository's history:

```go
func (e Engine) Chain(ctx, cfg) ([]spi.Backup, error)
func (e Engine) TestBackup(ctx, cfg, label string) string   // "" = pass
```

- `Chain` runs `restic snapshots --json` inside the container (the same idle
  container the engine already stands up, with credentials forwarded by name per
  [[spec 0018]] R5), parses it into `[]spi.Backup{Label, Type, Timestamp}` ordered
  **newest first** by snapshot time. `Label` is the snapshot's short ID; `Type` is
  fixed (restic has no full/incr/diff distinction — a benign, display-only
  difference from pgBackRest). Parsing is a pure, testable function
  (`internal/resticinfo`, mirroring `internal/pgbrinfo`).
- `TestBackup(label)` is exactly the restic restore path pinned to that snapshot:
  restore `--target /restore` for `<label>` instead of `latest`, apply the same
  two-phase network isolation ([[spec 0018]] R5 / spec 0003 R2 — connected for the
  fetch, disconnected before any check), run the configured file/command checks,
  and return `""` on PASS or the failure reason otherwise. This is "reuse, don't
  fork" ([[spec 0010]] R5): a per-snapshot test is `run` pinned to a snapshot.

The orchestrator's existing `LastGood` loop (walk newest→oldest, stop at first
PASS, record newer failures, honor `maxTry`) then works verbatim.

### borg `ChainTester` (`internal/engine/borg`)

Identical shape, borg lifecycle swapped in (the same near-exact sibling
relationship [[spec 0022]] established):

- `Chain` runs `borg list --json` (repository archive list) inside the container,
  parses to `[]spi.Backup` newest first by archive time; `Label` is the archive
  name. borg has no `latest` alias ([[spec 0022]]), so every candidate is an
  explicit archive name — which is exactly what a chain walk needs.
- `TestBackup(name)` extracts `::<name>` into `/restore` (borg *extracts* rather
  than *restores* — `cd /restore && borg extract`, no `--target`; [[spec 0022]]),
  same two-phase isolation, runs the checks, returns `""` or the reason.

### restic/borg `FleetSurveyor`

`fleet` is cheaper than `last-good` — metadata only, no restore ([[spec 0011]]
R3). For a filesystem engine, the "repo of many units" that pgBackRest expresses
as *stanzas* maps to the repository's own history:

```go
func (e Engine) Survey(ctx, cfg) ([]spi.FleetUnit, error)
func (e Engine) SkeletonSource(cfg, unit) config.Source
```

`Survey` runs the same one metadata call (`restic snapshots --json` /
`borg list --json`) the `ChainTester` uses and folds it into a single
`spi.FleetUnit` for the repository: `Name` = the repository identity,
`BackupCount` = number of snapshots/archives, `NewestLabel` + `NewestBackup` =
the freshest one, and `Status` = a healthy/degraded string derived from whether
the repository is reachable and non-empty. Cost is one metadata call regardless
of history size ([[spec 0011]] R3), so pointing `fleet` at a large repo still
costs seconds. `SkeletonSource` returns the base source with the repository
carried over (there is no per-stanza swap for a single-repo engine); the emitted
skeleton is a directly-usable `run`/`scaffold` input, per [[spec 0011]] R5.

> A filesystem repository surveys as **one unit**, not N — a restic/borg repo is
> the analogue of a single pgBackRest stanza, not of the whole multi-stanza repo.
> That is the honest mapping and it is enough to satisfy the capability and the
> exit contract; a future "many repos, one survey" mode is Open questions.

### The fleet exit-code fix (`cmd/salvage/main.go`)

`cmdFleet` gains a failure signal mirroring `cmdLastGood`. After a successful
survey renders, it inspects the result:

- Exit **1** (a "verdict"-style failure) when the survey is unhealthy:
  **zero units**, **zero restorable backups across all units** (every unit has
  `BackupCount == 0`), or **any unit degraded** (a non-healthy `Status`).
- Exit **2** unchanged — config/operational errors (the existing `engine.Fleet`
  and config-load paths).
- Exit **0** only when the survey is non-empty and every unit is healthy with at
  least one backup.

`engine.Fleet` continues to return a populated `*report.Fleet` for a degraded
repo (a degraded stanza is a *finding*, not an operational error — 0011 R1 keeps
enumeration itself verdict-free); the CLI derives the exit from the report, the
same division of labor `last-good` already uses (`engine.LastGood` returns the
report; `cmdLastGood` derives exit `1` from `RecoveryPoint == nil`). This keeps
the 1-vs-2 split honest: `1` = "I surveyed and it is unhealthy," `2` = "I could
not survey." Both JSON and human output are emitted **before** the exit code is
chosen, so a monitoring pipeline gets the full report *and* the signal.

### Bounded search cost (Open question, [[spec 0010]])

Each restic/borg `last-good` candidate is a **real restore** into a throwaway
container — the same cost model as pgBackRest ([[spec 0010]] R3), and the same
reason the search stops at the first PASS. The existing `-max N` cap
([[spec 0010]] R6) applies unchanged and is the primary bound. Because a
filesystem history can be long, the docs for restic/borg `last-good` MUST state
the per-candidate restore cost explicitly and recommend `-max` for large repos.

### Optional: bounded parallel search ([[spec 0010]] Open question)

The newest→oldest search is serial today. An optional bounded-parallel mode
(`-parallel K`, default `1` = today's behaviour) would restore up to `K`
candidates concurrently into separate throwaway containers, still reporting the
**newest** PASS as the recovery point (not merely the first to finish) so the
result is deterministic and identical to the serial answer. This is a
worst-case-latency optimization only; it changes no verdict. Deferred behind a
flag; engine-agnostic (it lives in the orchestrator's `LastGood` loop, so it
benefits pgBackRest, restic, and borg alike).

### Optional: a logical pseudo-chain source ([[spec 0010]]/[[spec 0011]] Open q.)

A directory of dated dumps (e.g. `db-2026-06-30.sql.gz`) is a *pseudo-chain*: no
backup tool, but a real newest→oldest history. An optional `dumpdir` source kind
could implement `spi.ChainTester` (sort files by embedded/mtime date newest
first; `TestBackup` = restore that one dump via the matching SQL engine and run
checks) and a trivial `spi.FleetSurveyor` (survey the directory's newest file).
Because dispatch is now capability-only, such a source needs **no** orchestrator
change — exactly the payoff of removing the string gate. Deferred; sketched here
to show the generalized gate admits non-tool histories too.

## Requirements

**R1 — Capability-only dispatch (no `source.kind` string gate).** `engine.LastGood`
MUST dispatch on the `spi.ChainTester` assertion alone and `engine.Fleet` on the
`spi.FleetSurveyor` assertion alone. The `cfg.Target.Source.Kind == "pgbackrest"`
checks (`engine.go:174`, `engine.go:220`) MUST be removed. An engine lacking the
relevant capability MUST still return the existing "not supported for target.type
X" error.

**R2 — restic `ChainTester`.** The restic engine MUST implement `spi.ChainTester`:
`Chain` enumerates the repository's snapshots newest-first (`restic snapshots
--json`, parsed by a pure testable function) as `[]spi.Backup`; `TestBackup(label)`
restores that specific snapshot with the same two-phase network isolation and
configured checks as a `run`, returning `""` on PASS or the failure reason. It MUST
reuse the existing restore + checks machinery, not fork it.

**R3 — borg `ChainTester`.** The borg engine MUST implement `spi.ChainTester` with
the identical contract, enumerating archives newest-first (`borg list --json`) and
testing each by extracting the named archive (no `latest` alias; explicit archive
name required).

**R4 — restic/borg `FleetSurveyor`.** Each MUST implement `spi.FleetSurveyor`:
`Survey` returns the repository as a metadata-only `[]spi.FleetUnit` (one unit per
repository) with `Name`, `BackupCount`, newest label/timestamp, and a
healthy/degraded `Status`, from **one** metadata call and **no restore**
([[spec 0011]] R3). `SkeletonSource` MUST return a base source (repository carried
over) whose emitted skeleton re-parses via `config.Load` and is a usable
`scaffold`/`run` input ([[spec 0011]] R5).

**R5 — Fleet exit-code contract.** `cmdFleet` MUST derive its exit code from the
survey: **2** on config/operational error (unchanged); **1** when the survey has
zero units, zero backups across all units, or any degraded unit; **0** only when
the survey is non-empty and every unit is healthy with at least one backup. The
report (JSON and human) MUST be emitted before the exit code is chosen. This
mirrors `cmdLastGood`'s `RecoveryPoint == nil → exit 1` and the 0/1/2 contract.

**R6 — Bounded, honest search.** Each restic/borg `last-good` candidate is a real
restore. The search MUST stop at the first PASS ([[spec 0010]] R3) and MUST honor
`-max N` ([[spec 0010]] R6). Output MUST NOT imply repair or extraction
([[spec 0010]] R7). The command MUST always report what was tried — no silent caps.

**R7 — Postgres/pgBackRest behaviour unchanged.** Removing the string gates MUST
NOT change any pgBackRest `last-good`/`fleet` behaviour, report field, ordering,
or message: the pgBackRest engine still implements both capabilities and its
`stanza`/`--set=<label>` mechanics behind them. Existing pgBackRest tests MUST pass
untouched.

**R8 — Optional: bounded parallel search.** If implemented, a `-parallel K` flag
(default `1` = today's serial behaviour) MUST restore at most `K` candidates
concurrently and MUST still report the **newest** PASS as the recovery point, so
the result is identical to the serial search. It MUST live in the orchestrator's
`LastGood` loop and be engine-agnostic. Deferred; not required for acceptance.

**R9 — Optional: logical pseudo-chain source.** If implemented, a `dumpdir`
(directory-of-dated-dumps) source MUST light up `last-good`/`fleet` purely by
implementing `spi.ChainTester`/`spi.FleetSurveyor`, with **no** orchestrator
change beyond R1. Deferred; not required for acceptance.

**R10 — No new dependencies.** All of the above MUST use the standard library plus
the module's single `gopkg.in/yaml.v3` dependency; `restic`/`borg` remain runtime
Docker images, not Go deps ([[spec 0018]] R7, [[spec 0022]] R7).

## Open questions

- **Repository = one unit vs. many.** A restic/borg repo surveys as a single fleet
  unit (the analogue of one pgBackRest stanza). A future mode could survey *many*
  repositories in one `fleet` run (a directory of repos, or a config listing
  several) — but credentials and repository identity get thornier, and it overlaps
  the hosted cross-repo plane ([[spec 0008]]). Deferred.
- **Per-unit `last-good` inside `fleet`** ([[spec 0011]] Open question). `fleet`
  could optionally chase each unit with a bounded `last-good` for a "freshest
  restorable point per unit" health report — but that is N restores and belongs
  behind an explicit flag (and probably the scheduler). Deferred.
- **restic incremental semantics.** A restic snapshot is always independently
  restorable, so `Type` is display-only; if a future engine has true
  full/incr/diff dependencies, `TestBackup` may need to restore the chain up to the
  candidate. Not a concern for restic/borg today.
- **Parallel search resource ceiling.** R8's `-parallel K` multiplies concurrent
  throwaway containers; a sensible default cap and Docker-resource backpressure
  need thought before it ships. Deferred with R8.
- **Deterministic dump dating** (R9). A `dumpdir` pseudo-chain needs a rule for
  ordering (filename date vs. mtime) and for mapping each dump to a SQL engine.
  Deferred with R9.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass; no Postgres/
   pgBackRest behaviour or test changes (R7).
2. Against a restic (or borg) repository with **multiple** snapshots/archives
   where the newest fails to restore and an older one is good, `salvage last-good`
   skips the newer one and reports the older as the recovery point, including the
   newer candidate's failure reason — the [[spec 0010]] acceptance criterion, now
   on a filesystem engine. Exit `0` with a recovery point; `1` when none restore.
3. `salvage fleet` against that repository enumerates it with a correct backup
   count and newest label, performing **no restore**, and its skeleton output (with
   `-o`) re-parses via `config.Load`.
4. `salvage fleet` returns a **non-zero** exit against a degraded or empty
   repository, and exit `0` against a healthy non-empty one (R5).
5. `engine.LastGood`/`engine.Fleet` contain no `source.kind == "pgbackrest"`
   string check (`grep` returns nothing); dispatch is capability-only (R1).
6. `last-good`/`fleet` on a MySQL/MongoDB target still report "not supported for
   target.type X" — the capability gate, not a `source.kind` gate, produces the
   message ([[spec 0024]] R7).
