# 0024 — The MySQL engine (a second SQL engine)

- **Status:** Implemented
- **Created:** 2026-07-02
- **Owner:** Firerok

## Context

[[spec 0016]] built the vertical seam (the engine SPI keyed by `target.type`) and
[[spec 0017]] built the horizontal one (a check `kind` + engine-provided
evaluators). Both specs, plus [[spec 0022]] (borg), named the R5 roadmap order:
**restic, borg, MySQL, MongoDB, object-storage**. borg shipped as the second
*filesystem* engine, a near-exact sibling of restic. This spec is the next item —
MySQL — and it is structurally a **different animal**: MySQL is not a filesystem
engine at all. It is a **SQL engine**, Postgres's closest sibling, not restic/borg's.

Concretely: a MySQL logical dump restores into a throwaway container the same way
a `pg_dump`/`sql`-kind Postgres source does, and the restored target is asked
questions with SQL, not file probes. The existing `sql` check kind — registered
once, in `internal/checks/sql.go`, against the `checks.Queryer` capability
(`Query(ctx, sql) (string, error)`) — already expresses exactly that. So MySQL's
entire contribution to the check-evaluation seam is: **implement `Query` against
a `mysql` CLI instead of `psql`.** No new evaluator, no new check kind, no new
`config.Check` fields. This is the opposite shape from restic/borg, which each
needed four new non-SQL kinds (`file_exists`/`file_count`/`checksum`/`command`)
because a filesystem backup has no SQL to query.

## Goals

- A registered `spi.Engine` for `target.type: "mysql"` that loads a logical
  (`.sql` dump) restore into a throwaway MySQL container in Docker (no host
  `mysql` client required to run Salvage) and returns a live, SQL-queryable
  target.
- The target's `Query(ctx, sql) (string, error)` satisfies `checks.Queryer`, so
  the existing `sql` check kind evaluates against it with **zero new evaluator
  code** — the key correctness property of this spec.
- Credentials by reference (spec 0003): the container's root password is a fixed,
  disposable dev credential scoped to the throwaway container (mirroring
  Postgres's `ephemeral.Postgres`, which does the same) — never the customer's
  production credential, and never persisted past `Stop()`.
- Adding this engine touches only new files (`internal/engine/mysql`,
  `internal/ephemeral/mysql.go`) plus the config allow-list and one blank
  import — no change to `internal/engine` orchestration or the CLI.

## Non-goals

- **Physical/binlog restore.** MySQL also supports physical backups (Percona
  XtraBackup, MySQL Enterprise Backup) and point-in-time recovery via binlog
  replay — the MySQL analogue of pgBackRest. v1 supports the logical dump path
  only (the common, easy case, and the direct MySQL analogue of Postgres's
  `pg_dump`/`sql` kinds). A physical/binlog engine is future work; see Open
  questions.
- **`scaffold` (introspection).** MySQL has no discovery engine yet — MySQL's
  `information_schema` differs enough from Postgres's catalog that
  `internal/discover` (Postgres-catalog-specific, per 0016's Open questions)
  cannot simply be reused. The MySQL `RestoredTarget` does not implement
  `discover.RowQueryer`, so `scaffold` cleanly gates off with "not supported for
  target.type mysql" — the same gate restic/borg use, zero orchestrator change.
- **`last-good`/`fleet`.** Both are pgBackRest-scoped chain/fleet concepts with no
  MySQL equivalent yet (that would require the physical/binlog path above). MySQL
  implements neither `spi.ChainTester` nor `spi.FleetSurveyor`, so both commands
  cleanly report "not supported for target.type mysql" — mirroring restic/borg
  (0018, 0022).

## Design

### The engine (`internal/engine/mysql`)

`Engine{}` implements `spi.Engine` for `Type() == "mysql"` and `Register`s itself
in `init()`. `Restore` is line-for-line the Postgres logical-restore path (the
`default:` branch of `postgres.Engine.Restore`) with the MySQL lifecycle swapped
in:

1. `ephemeral.Preflight` + `requireEnv(pass_env)` — a missing Docker or a missing
   secret is a `spi.Fault` (operational, exit 2, no verdict). MySQL v1 has no
   required `pass_env` vars (the container's credential is Salvage's own, not the
   customer's), but `pass_env` is still honored for forward compatibility.
2. `ephemeral.StartMySQL` stands up a disposable `mysql:8` (configurable via
   `target.restore.image`) container with a fixed root password and the
   configured database pre-created, and waits for it to accept real queries — not
   `mysqladmin ping`, for the same reason `ephemeral.Postgres` avoids
   `pg_isready`: the official image's init phase can report ready before the
   target database exists. A container-create failure is a `spi.Fault`.
3. `docker cp` copies the `.sql` dump into the container; `docker exec … sh -c
   "mysql -h 127.0.0.1 -u root <db> < /tmp/salvage-dump.sql"` loads it (the
   redirect runs inside the container, so dump content never crosses the
   docker-cp/exec boundary as a command argument). A load failure is a **bare
   error** (a "fail" verdict), not a Fault — the backup itself didn't restore.
4. The returned `*ephemeral.MySQL` is the live `RestoredTarget`; `Stop()` `docker
   kill`s it and is idempotent.

There is no network-isolation phase (spec 0003 R2) here the way restic/borg need
one: a logical SQL dump has no way to make outbound connections during load, and
the `sql` check kind only ever runs `SELECT`-shaped read queries against the
container's own database — there is no `command`-style check that could exfiltrate
through a restored dump the way a restored filesystem's `command` check could.

### The target: `Query` satisfies `checks.Queryer`

```go
func (m *MySQL) Query(ctx context.Context, sql string) (string, error) {
    out, err := m.mysqlExec(ctx, "-N", "-B", "-e", sql, m.Database)
    ...
    return firstField(out), nil
}
```

`mysqlExec` shells to the `mysql` CLI inside the container via `docker exec`
(exactly how `ephemeral.Postgres.psql` shells to `psql`) — **no Go MySQL driver**,
keeping the module's only dependency `gopkg.in/yaml.v3`. `-N -B` (skip-column-
names, tab-separated batch mode) is MySQL's analogue of `psql -tAqc`: a bare
scalar for a single-row, single-column `SELECT`. The root password is forwarded
via the `MYSQL_PWD` environment variable on the `docker exec` (never a `-p`
command-line argument, which would be visible in a process listing) — the same
by-reference discipline spec 0003 requires for real secrets, applied here even
though this credential is Salvage's own throwaway one.

Because `Query` has this exact signature, `*ephemeral.MySQL` satisfies
`checks.Queryer` (`internal/checks/sql.go`) automatically. `evaluateSQL` — the
one and only `sql`-kind evaluator, registered once in `internal/checks/sql.go`'s
`init()` — type-asserts any `checks.Target` to `Queryer` and runs unchanged
against a MySQL target. **The MySQL engine registers no evaluator of its own.**
This is the structural inverse of restic/borg (spec 0018 R3, spec 0022 R3), which
each had to register four *new* kinds because there was no existing SQL surface
to reuse; MySQL has one, so it reuses it.

### Config (`internal/config`)

- `Source` gains `kind: "mysql"` alongside the existing `pg_dump`/`sql`/
  `pgbackrest`/`restic`/`borg` kinds; it reuses the existing `Path` field (a local
  `.sql` dump) — no new `Source` fields.
- `applyDefaults` defaults the image (`mysql:8`), the database
  (`salvage_restore_test`, matching Postgres's logical default), and the user
  (`root`, MySQL's default administrative account, mirroring Postgres defaulting
  to `postgres`).
- `Validate` accepts `target.type: "mysql"` via `validateMySQLSource`: `Path` is
  required and must exist on disk — the same rule `validatePostgresSource` applies
  to its `pg_dump`/`sql` kinds. `target.type: "mysql"` is **not** added to
  `isFileProbeTarget` (internal/config/config.go): its checks use the `sql` kind,
  which has no target-type restriction (unlike the file/command/http kinds, which
  are restricted to restic/borg/exec), so a mysql config's checks validate through
  the same unrestricted path Postgres configs already use — zero change to
  `validateCheck`'s `sql` branch.

### Credentials

The database credential inside the throwaway container is Salvage's own fixed
dev password (`ephemeral.mysqlPass`), scoped to a container that is destroyed on
`Stop()` — identical to how `ephemeral.Postgres` picks a fixed `postgres`/
`salvage` credential for its own throwaway container. This is not a customer
secret: the customer's actual secret, if any, would be needed only to *fetch* the
dump (e.g. from S3) before Salvage sees it, which is out of scope for the local-
path v1 source (mirroring Postgres's `pg_dump`/`sql` kinds, which take a local
`Path` too). `source.pass_env` is threaded through and honored today for forward
compatibility with a future authenticated-fetch source kind.

## Requirements

**R1 — Registered MySQL engine.** There MUST be an `spi.Engine` for
`target.type: "mysql"`, registered in its package `init()` and wired via a blank
import in `internal/engine/engine.go`. It MUST restore in Docker with no host
`mysql` client required to run Salvage.

**R2 — SQL-queryable target, reusing the existing `sql` check kind.** `Restore`
MUST return a `RestoredTarget` whose `Query(ctx, sql) (string, error)` satisfies
`checks.Queryer`, with an idempotent `Stop()`. The MySQL engine MUST register **no
new check evaluator** — the existing `sql` evaluator (`internal/checks/sql.go`,
registered once) MUST evaluate unchanged against a MySQL target. It MUST NOT
implement `discover.RowQueryer` (so `scaffold` gates off cleanly).

**R3 — Logical restore only.** `Restore` MUST support loading a `.sql` dump file
into the throwaway container via `docker cp` + an in-container `mysql < dump.sql`
(or equivalent). A physical/binlog restore is explicitly out of scope for v1.

**R4 — Operational-vs-verdict split.** A missing `pass_env` secret or a
Docker/container problem MUST be a `spi.Fault` (exit 2, no verdict); a dump that
fails to load MUST be a bare error (a "fail" verdict). Inherited unchanged from
0016 R4.

**R5 — Credentials by reference, no Go driver.** The database client MUST be the
`mysql` CLI invoked via `docker exec` — no Go MySQL driver dependency. Any
credential forwarded into the container (root password, and any future
`pass_env` secret) MUST be forwarded via environment (`MYSQL_PWD`/`docker run -e
NAME`), never as a command-line argument.

**R6 — Config allow-list.** `config.Validate` MUST accept `target.type: "mysql"`
and validate its source shape (`source.path` required and must exist), with all
Postgres/restic/borg/exec validation and messages unchanged (0016 R6).
`target.type: "mysql"` MUST NOT be added to `isFileProbeTarget` — its checks use
the unrestricted `sql` kind, not the restic/borg/exec-scoped file/command kinds.

**R7 — Engine-specific commands cleanly gated.** `scaffold`, `last-good`, and
`fleet` on a `target.type: "mysql"` target MUST each return a clear "not
supported for target.type mysql" error via the existing capability gates — no
orchestrator change.

**R8 — Inherited platform, no new deps.** The report, verdict, signing/ledger/
verify, and dead-man's-switch MUST be inherited unchanged. No new Go dependency
(stdlib + `gopkg.in/yaml.v3`); the `mysql:8` Docker image is runtime, not a Go dep.

## Contrast with [[spec 0022]] (borg)

|  | borg (0022) | MySQL (this spec) |
|---|---|---|
| Kind of backup | filesystem archive | logical SQL dump |
| Restored target answers | file probes | SQL queries |
| Check kind(s) used | 4 new: `file_exists`/`file_count`/`checksum`/`command` | 1 existing: `sql` (unchanged) |
| New evaluator code | yes (`internal/probe`) | **none** |
| Capability the target implements | `probe.FileProber` | `checks.Queryer` (same as Postgres) |
| Closest sibling | restic | Postgres |
| Network isolation phase | yes (spec 0003 R2) | not needed (no outbound-capable restore step, no `command`-style check) |

The two engines prove opposite halves of spec 0017's generalization: borg proved
validation could move *off* SQL; MySQL proves a second SQL-shaped backup type can
plug into the *same* SQL check kind with no new code at all.

## Open questions

- **Physical/binlog restore.** A `xtrabackup`-based physical restore (with PITR
  via binlog replay, MySQL's analogue of pgBackRest) is real future work — it
  would likely warrant its own `source.kind` (e.g. `xtrabackup`) and would be the
  natural home for a `spi.ChainTester`/`spi.FleetSurveyor` implementation (backup
  chains and multi-schema "fleets" are meaningful there in a way they are not for
  a single logical dump). Deferred; not started.
- **`scaffold` for MySQL.** Once there's appetite, a MySQL-catalog discovery path
  (querying `information_schema` instead of Postgres's `pg_catalog`/
  `information_schema` mix) could implement `discover.RowQueryer`-equivalent
  introspection and light up `scaffold`. Deferred, mirroring restic/borg (0018,
  0022 Open questions).
- **Remote/authenticated dump sources.** Like Postgres's `pg_dump`/`sql` kinds,
  v1 takes a local file `Path` only. A future `pass_env`-authenticated fetch (e.g.
  from S3) is straightforward to add without touching the SPI.

## Validate it for real

`dev/mysql/make-backup.sh` (mirroring `dev/pgbackrest/`) seeds a throwaway
`mysql:8` container with a small `orders` table (a known row count) and
`mysqldump`s it to `dev/mysql/seed-dump.sql`. `salvage.mysql.example.yaml` runs
two `kind: sql` checks against it — a row-count floor and a freshness bound —
the identical check shape Postgres configs already use.

```
$ ./dev/mysql/make-backup.sh
$ salvage run -config salvage.mysql.example.yaml
salvage: target "demo-mysql"
  restore   ok
  check     ok    orders_not_empty
  check     ok    latest_order_recent
  verdict   PASS
```

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass; no Postgres/
   restic/borg/exec behaviour or test changes.
2. A mysql config with `target.type: mysql` and `kind: sql` checks parses,
   validates, and (against a real dump) restores in Docker and produces a PASS
   verdict; a mismatched check yields a FAIL verdict.
3. `internal/checks/sql.go` gains no new code, and `internal/engine/mysql`
   registers no evaluator (`grep -r RegisterEvaluator internal/engine/mysql`
   returns nothing) — confirming R2's zero-new-evaluator claim.
4. `scaffold`, `last-good`, and `fleet` on a mysql target each return a clear
   "not supported for target.type mysql" error.
5. The report, verdict, and attestation surface carry no MySQL-specific
   structure — the mysql verdict is signed and attested by the identical path as
   Postgres, restic, and borg.
