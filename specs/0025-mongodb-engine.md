# 0025 — The MongoDB engine (a third way to extend the check-kind seam)

- **Status:** Implemented
- **Created:** 2026-07-02
- **Owner:** Firerok

## Context

[[spec 0016]] built the vertical seam (the engine SPI keyed by `target.type`) and
[[spec 0017]] built the horizontal one (a check `kind` + engine-provided
evaluators). The R5 roadmap order (0017, positioning §8) is restic, borg, MySQL,
MongoDB, object-storage. [[spec 0024]] shipped MySQL, a second **SQL** engine
that reused the existing `sql` check kind with **zero new evaluator code** — the
opposite shape from restic/borg (spec 0018/0022), which each registered four new
non-SQL kinds (`file_exists`/`file_count`/`checksum`/`command`) shared via
`internal/probe` because a filesystem backup has no SQL surface.

MongoDB is the next roadmap item, and it is a **third shape**, not a repeat of
either precedent:

- It is not a SQL engine — there is no `SELECT`-shaped scalar query to reuse the
  `sql` kind for.
- It is not a filesystem engine either — a restored MongoDB database has no
  meaningful file tree to probe (`internal/probe`'s `FileProber` contract is
  about paths, globs, and checksums on disk; a Mongo collection is neither).

So MongoDB registers **its own** check kinds — `collection_count` and
`doc_query` — self-contained to `internal/engine/mongodb`, not shared via
`internal/probe` and not the reused `sql` kind. Spec 0017's design notes
explicitly named these two kinds as MongoDB's future contribution to the seam
(illustrative, not yet built, at the time); this spec is the one that builds
them, and in doing so demonstrates the check-`kind` seam (0017 R3) generalizes a
**third** way: an engine can extend the seam with kinds that belong to it alone,
neither shared infrastructure (`internal/probe`) nor a reused existing kind
(`sql`).

## Goals

- A registered `spi.Engine` for `target.type: "mongodb"` that loads a
  `mongodump --archive` file into a throwaway MongoDB container in Docker (no
  host `mongosh`/`mongodump`/`mongorestore` required to run Salvage) and returns
  a live, Mongo-queryable target.
- Two new check kinds — `collection_count` and `doc_query` — registered as
  evaluators in `internal/engine/mongodb`'s own `init()`, each type-asserting the
  opaque `checks.Target` to a small `MongoQueryer` capability interface the
  restored target implements by shelling to `mongosh`.
- Credentials by reference (spec 0003): the container's root credential is a
  fixed, disposable dev credential scoped to the throwaway container (mirroring
  `ephemeral.mysqlPass`/`ephemeral.pgPass`) — never a customer secret, never
  persisted past `Stop()`.
- Adding this engine touches only new files (`internal/engine/mongodb`,
  `internal/ephemeral/mongodb.go`) plus the config allow-list and one blank
  import — no change to `internal/engine` orchestration or the CLI.

## Non-goals

- **Physical/oplog restore.** MongoDB also supports physical/filesystem-snapshot
  backups and point-in-time recovery via oplog replay — the MongoDB analogue of
  pgBackRest. v1 supports the logical `mongodump`/`mongorestore` path only (the
  common, easy case, and the direct MongoDB analogue of Postgres's `pg_dump`/
  `sql` kinds and MySQL's `.sql` dump kind). A physical/oplog engine is future
  work; see Open questions.
- **`scaffold` (introspection).** MongoDB has no discovery engine yet.
  `internal/discover` is Postgres-catalog-specific and has no MongoDB analogue
  built. The MongoDB `RestoredTarget` does not implement `discover.RowQueryer`,
  so `scaffold` cleanly gates off with "not supported for target.type mongodb" —
  the same gate restic/borg/mysql use, zero orchestrator change.
- **`last-good`/`fleet`.** Both are pgBackRest-scoped chain/fleet concepts with
  no MongoDB equivalent yet (that would require the physical/oplog path above).
  MongoDB implements neither `spi.ChainTester` nor `spi.FleetSurveyor`, so both
  commands cleanly report "not supported for target.type mongodb" — mirroring
  restic/borg/mysql (0018, 0022, 0024).

## Design

### The engine (`internal/engine/mongodb`)

`Engine{}` implements `spi.Engine` for `Type() == "mongodb"` and `Register`s
itself, plus its two check kinds, in `init()`. `Restore`:

1. `ephemeral.Preflight` + `requireEnv(pass_env)` — a missing Docker or a missing
   secret is a `spi.Fault` (operational, exit 2, no verdict). MongoDB v1 has no
   required `pass_env` vars (the container's credential is Salvage's own, not
   the customer's), but `pass_env` is still honored for forward compatibility.
2. `ephemeral.StartMongoDB` stands up a disposable `mongo:7` (configurable via
   `target.restore.image`) container authenticated with a fixed root credential,
   and waits for it to answer a real `mongosh` query
   (`db.runCommand({ping:1}).ok`) — not merely a listening port — for the same
   reason `ephemeral.Postgres`/`ephemeral.MySQL` avoid `pg_isready`/a bare TCP
   probe: the official image's init phase can accept connections before the
   auth database is fully seeded. A container-create failure is a `spi.Fault`.
3. `docker cp` copies the `mongodump --archive` file into the container;
   `mongorestore --archive=<path> --nsInclude <database>.* --drop` loads it
   inside the container via `docker exec` (no shell redirect is needed here —
   unlike MySQL's `mysql < dump.sql`, `mongorestore` takes the archive as a flag
   argument, but the archive's *content* never crosses the docker-cp/exec
   boundary as a raw argv string; only the in-container path does — the same
   discipline MySQL's redirect achieves by a different mechanism). A load
   failure is a **bare error** (a "fail" verdict), not a Fault — the backup
   itself didn't restore. (Verified live: an archive that isn't a real mongodump
   stream fails `mongorestore` with a clear "not a mongodump archive" error,
   surfaced as a FAIL verdict, not an operational exit — see "Validate it for
   real" below.)
4. The returned `*ephemeral.MongoDB` is the live `RestoredTarget`; `Stop()`
   `docker kill`s it and is idempotent.

There is no network-isolation phase (spec 0003 R2) here, for the same reason
MySQL has none: a logical dump load has no outbound-egress step, and MongoDB's
two check kinds (`collection_count`/`doc_query`) only ever run read-only queries
against the container's own database — there is no `command`-style check that
could exfiltrate through a restored dump.

### The capability: `MongoQueryer`

```go
// internal/engine/mongodb/mongodb.go
type MongoQueryer interface {
    CountDocuments(ctx context.Context, collection, filterJSON string) (int64, error)
    FindOneField(ctx context.Context, collection, filterJSON, field string) (string, error)
}
```

`*ephemeral.MongoDB` implements it by shelling to `mongosh --quiet --eval` inside
the container via `docker exec` — **no Go MongoDB driver**, keeping the module's
only dependency `gopkg.in/yaml.v3` (spec 0016 R7). This mirrors `checks.Queryer`
(the `sql` kind's capability, satisfied via `psql`/`mysql`) and
`probe.FileProber` (the file/command kinds' capability, satisfied via file/shell
probes over `docker exec`) — a third instance of the same "type-assert an opaque
`checks.Target` to the capability this kind needs" pattern.

Unlike MySQL's `MYSQL_PWD` or Postgres's `PGPASSWORD`, neither `mongosh` nor
`mongorestore` has a universal password-via-environment-variable flag. The
credential is instead forwarded as `--password "$MONGO_PWD"`, a **shell
expansion** run inside the container: the literal argument the outer `docker
exec` invocation carries is the string `$MONGO_PWD`, not the secret itself: the
substitution happens only inside the container's own shell, from an env var
(`-e MONGO_PWD=...`) scoped to that single `docker exec` call. This achieves the
same "never a literal command-line secret visible via process listing" property
`MYSQL_PWD`/`PGPASSWORD` give the sibling engines, adapted to a client with no
direct env-var password flag.

**Why MongoDB has no `internal/probe` reuse.** `internal/probe`'s `FileProber`
is a *filesystem* contract — `Exists(path)`, `Count(glob)`, `Sha256(path)`,
`RunCommand`. A restored MongoDB database is a set of collections inside
`mongod`'s own storage engine, not a walkable file tree Salvage's container
exposes; there is nothing for `find`/`sha256sum`/`test -e` to probe. MongoDB's
two kinds are therefore genuinely new — the third instance of the check-kind
seam admitting an engine-owned capability, after "reuse `sql`" (MySQL) and
"share `internal/probe`" (restic/borg).

### The check kinds (evaluators, spec 0017 R3 — the third extension of the seam)

Each evaluator type-asserts the opaque `checks.Target` to `MongoQueryer`; a
target that isn't Mongo-queryable (e.g. a SQL or filesystem engine's target
reaching a mongodb check) yields a clear failing result, never a panic — the
same pattern `internal/checks/sql.go` and `internal/probe` already use. A result
is the same generic `{name, ok, severity, got, detail, error}` as every other
kind (spec 0017 R2).

| kind | config fields | evaluates | expectation fields |
|---|---|---|---|
| `collection_count` | `collection` (required), `filter` (optional JSON string; empty/omitted = count all) | `db.<collection>.countDocuments(<filter>)` as a scalar count | `expect_min`/`expect_max`/`equals` |
| `doc_query` | `collection` (required), `filter` (required JSON string), `field` (required, dotted path) | `findOne(filter)`, then extracts `field`'s value as a scalar string | `equals` (the common case), or `expect_min`/`expect_max` for numeric/date-like values |

`doc_query`'s `field` supports a dotted path (e.g. `meta.version`) via a
null-safe optional-chained field access inside the `mongosh --eval` script, so a
missing intermediate object yields a clear "field not present" error rather than
a script crash. A `Date` field value is rendered as an ISO-8601 string (matching
the timestamp shapes the `sql` kind's `max_age`/scalar parsing already expect,
though `doc_query` itself only supports `equals`/`expect_min`/`expect_max` in
v1 — `max_age` is not wired for `doc_query`; see Open questions).

`collection_count` is the MongoDB analogue of restic/borg's `file_count`
(both are a bounded scalar count); `doc_query` is the MongoDB analogue of a SQL
`SELECT field FROM ... WHERE ...` scalar check (both are "read one field from
one located record"). Both fit the existing `CheckResult` shape with no report/
verdict/ledger change (spec 0017 R2).

### Config (`internal/config`)

- `Source` gains `kind: "mongodb"` alongside the existing kinds; it reuses the
  existing `Path` field (a local `mongodump --archive` file) — no new `Source`
  fields, mirroring MySQL.
- `Check` gains `Collection`, `Filter`, `Field` (all `yaml:",omitempty"` so
  non-mongodb configs are byte-identical). The `sql`/file/http kinds ignore
  them; `collection_count` uses `Collection`+`Filter`; `doc_query` uses all
  three. `ExpectMin`/`ExpectMax`/`Equals` are reused per the table above.
- `applyDefaults` defaults the image (`mongo:7`), the database
  (`salvage_restore_test`, matching MySQL's logical default), and the user
  (`root`, mirroring MySQL's default administrative account).
- `Validate` accepts `target.type: "mongodb"` via `validateMongoDBSource`:
  `Path` is required and must exist on disk — the same rule
  `validateMySQLSource` applies. `validateCheck` requires `collection` for
  `collection_count` (plus an expectation field) and `collection`/`filter`/
  `field` for `doc_query` (plus an expectation field); both kinds are rejected
  outside `target.type: mongodb`, mirroring how `file_exists`/`file_count`/
  `checksum`/`command` are rejected outside restic/borg/exec.
  `target.type: "mongodb"` is **not** added to `isFileProbeTarget` — its checks
  use its own kinds, not the restic/borg/exec-scoped file/command kinds.
- A `kind: sql` check on a `target.type: mongodb` config is **not** rejected at
  config-load time (the `sql` kind's validation, like `collection_count`'s
  target-type restriction, is deliberately narrow — see spec 0024's precedent,
  where `sql` has no target-type restriction because Postgres/MySQL both use
  it unrestricted). It instead fails cleanly at **evaluation** time: the `sql`
  evaluator's type-assert of the MongoDB target to `checks.Queryer` fails
  (MongoDB does not implement `Query`), yielding the existing "sql check
  requires a SQL-queryable target" failing result — the same
  type-assert-failure path a `collection_count`/`doc_query` check hitting a
  non-MongoDB target already uses in reverse. No special-casing was added or
  needed; this is the seam working as designed (spec 0017 R3: "An engine asked
  to evaluate a kind it does not support MUST return a clear error").

### Credentials

The credential inside the throwaway container is Salvage's own fixed dev
password (`ephemeral.mongoPass`), scoped to a container destroyed on `Stop()` —
identical to how `ephemeral.MySQL`/`ephemeral.Postgres` pick a fixed dev
credential for their own throwaway containers. This is not a customer secret:
the customer's actual secret, if any, would be needed only to *fetch* the
archive (e.g. from S3) before Salvage sees it, out of scope for the local-path
v1 source (mirroring Postgres/MySQL's local-`Path` v1 sources).
`source.pass_env` is threaded through and honored today for forward
compatibility with a future authenticated-fetch source kind.

## Requirements

**R1 — Registered MongoDB engine.** There MUST be an `spi.Engine` for
`target.type: "mongodb"`, registered in its package `init()` and wired via a
blank import in `internal/engine/engine.go`. It MUST restore in Docker with no
host MongoDB client tools required.

**R2 — A third extension of the check-kind seam.** `internal/engine/mongodb`
MUST register two new evaluators — `collection_count` and `doc_query` — neither
reusing the `sql` kind (unlike MySQL, spec 0024) nor sharing
`internal/probe`'s kinds (unlike restic/borg, spec 0018/0022). Both MUST
type-assert the opaque `checks.Target` to a `MongoQueryer` capability the
MongoDB `RestoredTarget` implements, and MUST return a clear failing
`CheckResult` (never a panic) when the target lacks that capability.

**R3 — Logical restore only.** `Restore` MUST support loading a
`mongodump --archive` file into the throwaway container via `docker cp` +
in-container `mongorestore`. A physical/oplog restore is explicitly out of
scope for v1.

**R4 — Operational-vs-verdict split.** A missing `pass_env` secret or a
Docker/container problem MUST be a `spi.Fault` (exit 2, no verdict); an archive
that fails to load MUST be a bare error (a "fail" verdict). Inherited unchanged
from 0016 R4. (Verified live — see "Validate it for real".)

**R5 — Credentials by reference, no Go driver.** The database client MUST be the
`mongosh`/`mongorestore`/`mongodump` CLIs invoked via `docker exec` — no Go
MongoDB driver dependency. Any credential forwarded into the container MUST
never appear as a literal command-line argument visible via `docker exec`'s
outer invocation (the `--password "$MONGO_PWD"` shell-expansion technique
above), mirroring the by-reference discipline `MYSQL_PWD`/`PGPASSWORD` give the
sibling engines.

**R6 — Config allow-list.** `config.Validate` MUST accept `target.type:
"mongodb"` and validate its source shape (`source.path` required and must exist)
and its two check kinds' required fields, with all Postgres/restic/borg/mysql/
exec validation and messages unchanged (0016 R6). `target.type: "mongodb"` MUST
NOT be added to `isFileProbeTarget`.

**R7 — Engine-specific commands cleanly gated.** `scaffold`, `last-good`, and
`fleet` on a `target.type: "mongodb"` target MUST each return a clear "not
supported for target.type mongodb" error via the existing capability gates — no
orchestrator change. (Verified live.)

**R8 — Inherited platform, no new deps.** The report, verdict, signing/ledger/
verify, and dead-man's-switch MUST be inherited unchanged. No new Go dependency
(stdlib + `gopkg.in/yaml.v3`); the `mongo:7` Docker image is runtime, not a Go
dep.

## Contrast with [[spec 0024]] (MySQL)

|  | MySQL (0024) | MongoDB (this spec) |
|---|---|---|
| Kind of backup | logical SQL dump | logical `mongodump` archive |
| Restored target answers | SQL queries | document count / one field of one document |
| Check kind(s) used | 1 existing: `sql` (unchanged) | 2 new: `collection_count`, `doc_query` |
| New evaluator code | **none** | yes (`internal/engine/mongodb`) |
| Capability the target implements | `checks.Queryer` (same as Postgres) | `mongodb.MongoQueryer` (new, engine-owned) |
| Closest sibling | Postgres | none — a new shape |
| Credential-in-exec technique | `MYSQL_PWD` env var | `--password "$MONGO_PWD"` shell expansion (no native password-env flag) |

Where MySQL proved a second SQL-shaped backup type could plug into the existing
`sql` kind with zero new code, and restic/borg proved validation could move off
SQL entirely onto shared filesystem probes, MongoDB proves the seam supports a
**third** shape: an engine that is neither SQL nor filesystem, contributing
kinds that belong to it alone.

## Open questions

- **`mongosh`/`mongodump`/`mongorestore` CLI availability.** Confirmed live in
  this implementation session: Docker was available, and `docker run mongo:7`
  bundles `mongosh`, `mongodump`, and `mongorestore` (Mongo 6+ images ship
  `mongosh`, replacing the legacy `mongo` shell removed in Mongo 6). The dev
  harness (`dev/mongodb/make-backup.sh`) and the worked example
  (`salvage.mongodb.example.yaml`) were both run end-to-end against a real
  `mongo:7` container as part of this spec's implementation, producing PASS and
  FAIL verdicts as expected (see "Validate it for real"). This is a stronger
  verification than restic/borg/MySQL received (spec 0018/0022/0024 were all
  written without live Docker access and explicitly deferred verification) —
  noted here so the difference in confidence is honest and visible. What remains
  unverified: behavior against a `mongo:7`-produced archive restored into a
  *different* minor version, and any auth/TLS-hardened production MongoDB
  topology the archive might have come from (replica sets, sharded clusters) —
  v1's throwaway container is always a single unauthenticated-beyond-root
  standalone `mongod`.
- **Physical/oplog restore.** A filesystem-snapshot-based physical restore (with
  PITR via oplog replay, MongoDB's analogue of pgBackRest/`xtrabackup`) is real
  future work — it would likely warrant its own `source.kind` (e.g.
  `mongodump-fs`/`oplog`) and would be the natural home for a
  `spi.ChainTester`/`spi.FleetSurveyor` implementation (backup chains and
  multi-database "fleets" are meaningful there in a way they are not for a
  single logical archive). Deferred; not started.
- **`scaffold` for MongoDB.** Once there's appetite, a MongoDB discovery path
  (enumerating collections/indexes via `listCollections`/`listIndexes` and
  proposing `collection_count` checks, the document-store analogue of
  `internal/discover`'s catalog introspection) could implement a
  `discover`-equivalent capability and light up `scaffold`. Deferred, mirroring
  restic/borg/mysql (0018, 0022, 0024 Open questions).
- **`doc_query` and `max_age`.** The `sql` kind supports a `max_age` expectation
  for timestamp columns; `doc_query` does not wire `max_age` in v1 even though
  `FindOneField` renders `Date` fields as ISO-8601 strings (so the plumbing is
  present). Deferred rather than half-built without a concrete need driving the
  parsing-edge-case work.
- **Remote/authenticated archive sources.** Like Postgres's `pg_dump`/`sql`
  kinds and MySQL's `.sql` dump kind, v1 takes a local file `Path` only. A
  future `pass_env`-authenticated fetch (e.g. from S3) is straightforward to add
  without touching the SPI.

## Validate it for real

`dev/mongodb/make-backup.sh` (mirroring `dev/mysql/make-backup.sh`) seeds a
throwaway `mongo:7` container with a small `orders` collection (six documents,
including one with a stable `_id: "o1"`) and a `meta` collection holding a
schema-version document, then `mongodump`s it to
`dev/mongodb/seed-dump.archive`. `salvage.mongodb.example.yaml` runs two
`collection_count` checks and two `doc_query` checks against it — this was run
against a live Docker daemon during this spec's implementation:

```
$ ./dev/mongodb/make-backup.sh
$ salvage run -config salvage.mongodb.example.yaml
salvage: target "demo-mongodb"
  restore   ok    (3555ms)
  check     ok    orders_not_empty           6 within bounds
  check     ok    shipped_orders_present     4 within bounds
  check     ok    order_o1_status
  check     ok    schema_version_current     3 within bounds
  verdict   PASS
```

Changing the `order_o1_status` check's `equals` to a wrong value flips exactly
that check and the verdict to FAIL (exit 1) with `got`/`want` shown:

```
  check     FAIL  order_o1_status            got=shipped want "delivered"
  verdict   FAIL
```

Pointing `source.path` at a file that is not a real `mongodump` archive
confirms the operational-vs-verdict split (R4): `mongorestore` fails with
`"stream or file does not appear to be a mongodump archive"`, surfaced as a
restore FAIL and a FAIL verdict (exit 1) — not a `spi.Fault`/exit 2, since the
container itself came up fine and only the backup content was bad.
`scaffold`/`last-good`/`fleet` against the same config each return "not
supported for target.type mongodb" (exit 2), confirming R7.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass; no Postgres/
   restic/borg/mysql/exec behaviour or test changes.
2. A mongodb config with `target.type: mongodb` and `collection_count`/
   `doc_query` checks parses, validates, and (against a real archive) restores
   in Docker and produces a PASS verdict; a mismatched check yields a FAIL
   verdict. Verified live against a real `mongo:7` container (see "Validate it
   for real").
3. `internal/engine/mongodb` registers exactly two new evaluators
   (`grep -rn RegisterEvaluator internal/engine/mongodb` shows
   `collection_count` and `doc_query`) — confirming R2's "two new kinds, neither
   reused nor shared via `internal/probe`" claim.
4. `scaffold`, `last-good`, and `fleet` on a mongodb target each return a clear
   "not supported for target.type mongodb" error. Verified live.
5. The report, verdict, and attestation surface carry no MongoDB-specific
   structure — the mongodb verdict is signed and attested by the identical path
   as Postgres, restic, borg, and MySQL.
