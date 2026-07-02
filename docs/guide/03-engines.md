# Engines

## Vertical engines, horizontal platform

Salvage is built on two seams (specs
[0016](../../specs/0016-modular-engines.md) /
[0017](../../specs/0017-verification-attestation-platform.md)):

- **Vertical — the engine (per backup type).** *Restoring* a backup and
  *discovering* its structure is backup-type-specific work. Each `target.type`
  is a pluggable engine keyed on that type. This is where "how do I restore a
  Postgres dump?" versus "how do I restore a restic snapshot?" lives.
- **Horizontal — the platform (across all types).** The validation
  *orchestration*, the *verdict/report*, independent *attestation*, and the
  *cadence monitor* care nothing about what produced the report. Every engine
  inherits this surface unchanged.

The moat is horizontal; the engine is vertical. Adding an engine multiplies the
reach of a platform already built, without rebuilding it.

Six engines ship today: **postgres**, **mysql**, **mongodb**, **restic**,
**borg**, and **exec**. Each section below ends with a **first run** block —
the shortest copy-paste route from that engine's example config to a first
verdict.

## Postgres

`target.type: postgres` restores into a disposable Postgres container and
validates with `sql` checks. Three source kinds:

### Logical — `pg_dump` and `sql`

A local dump file, given by `source.path`:

- `kind: pg_dump` — a `pg_dump` archive (custom/dir/tar).
- `kind: sql` — a plain `.sql` dump.

The restore image defaults to `postgres:16`. See
[`salvage.example.yaml`](../../salvage.example.yaml).

### Physical / PITR — `pgbackrest`

`kind: pgbackrest` restores a pgBackRest repo by delegating to
`pgbackrest restore`, then waits for the cluster to reach a **consistent recovery
point** before asserting — so it validates the physical/PITR restore path, not
just a logical dump.

The restore `image` has **no default** here: it must carry both `postgres` and
`pgbackrest` (plus any extensions the source cluster loads, e.g. `timescaledb`,
declared in `restore.preload_libraries`).

- **Local repo** — mount it with `source.repo_volume` at `source.repo_path`
  (must match the image's `pgbackrest.conf` `repo1-path`). See
  [`salvage.pgbackrest.example.yaml`](../../salvage.pgbackrest.example.yaml).
- **Remote S3 / Cloudflare R2** — the repo lives in the image's
  `pgbackrest.conf`; forward the credentials **by name** via `source.pass_env`
  (e.g. `PGBACKREST_REPO1_S3_KEY`, `PGBACKREST_REPO1_S3_KEY_SECRET`) so secret
  values never touch the config or command line. See
  [`salvage.pgbackrest-s3.example.yaml`](../../salvage.pgbackrest-s3.example.yaml).
  This path has been validated end-to-end against a real TimescaleDB 17
  production restore from Cloudflare R2.

A pgBackRest repo also lights up the chain commands:
[`last-good`](./04-commands.md#last-good) (freshest restorable backup) and
[`fleet`](./04-commands.md#fleet) (enumerate every stanza) — capabilities the
restic and borg engines now share. [`scaffold`](./04-commands.md#scaffold)
generates a starter config from any Postgres source.

**First run (Postgres):**

```sh
cp salvage.example.yaml salvage.yaml
# edit: source.path (your pg_dump/.sql file), the checks (your tables)
salvage check -config salvage.yaml    # validate config + preflight Docker
salvage run   -config salvage.yaml    # restore-test and print the verdict
```

For pgBackRest, start from
[`salvage.pgbackrest.example.yaml`](../../salvage.pgbackrest.example.yaml)
(local repo: edit `source.stanza`, `source.repo_volume`, and `restore.image`) or
[`salvage.pgbackrest-s3.example.yaml`](../../salvage.pgbackrest-s3.example.yaml)
(remote repo: edit the stanza, the image, and export the `pass_env` credentials).

## MySQL

`target.type: mysql` (spec [0024](../../specs/0024-mysql-engine.md)) is the
second SQL engine — Postgres's closest sibling. It restores a **logical dump**
(a `mysqldump` `.sql` file, given by `source.path` with `kind: mysql`) into a
disposable MySQL container and validates it with the **same `sql` check kind**
Postgres uses — `kind` defaults to `sql`, so the checks look identical to a
Postgres config, with every expectation (`expect_min`/`expect_max`, `equals`,
`max_age`, `bool`) behaving the same.

- The restore image defaults to `mysql:8.4.10` (pinned to the version verified
  end-to-end; override with `restore.image`); the database defaults to
  `salvage_restore_test` and the role to `root` (both overridable under
  `restore`).
- v1 restores logical dumps only. Physical/binlog restore (xtrabackup) and
  MySQL PITR are **roadmap**, not shipped (spec 0024 Open questions).
- [`scaffold`](./04-commands.md#scaffold) works for MySQL targets: it
  introspects MySQL's own `information_schema` and emits `sql`-kind checks with
  thresholds from observed state. `last-good` and `fleet` report "not
  supported" — a logical dump file has no backup chain to walk.

**First run (MySQL):**

```sh
cp salvage.mysql.example.yaml salvage.yaml
# edit: source.path (your mysqldump .sql file), restore.database, the checks
salvage check -config salvage.yaml
salvage run   -config salvage.yaml
```

See [`salvage.mysql.example.yaml`](../../salvage.mysql.example.yaml) and
`dev/mysql/` for a reproducible harness (`make-backup.sh`).

## MongoDB

`target.type: mongodb` (spec [0025](../../specs/0025-mongodb-engine.md)) is
neither a SQL engine nor a filesystem engine: it restores a **logical
`mongodump --archive` file** (given by `source.path` with `kind: mongodb`) into
a disposable MongoDB container and validates it with **two check kinds of its
own**:

- **`collection_count`** — `countDocuments(filter)` on a collection as a
  scalar, asserted with `expect_min`/`expect_max`/`equals`.
- **`doc_query`** — `findOne(filter)` on a collection, reading one dotted
  `field` path from the matched document as a scalar, asserted with `equals`,
  `expect_min`/`expect_max`, or `max_age` (freshness against a timestamp
  field) — the MongoDB analogue of a SQL `SELECT field FROM … WHERE …` check.

See [Configuration → MongoDB check kinds](./02-configuration.md#mongodb-check-kinds)
for the full field reference.

- The restore image defaults to `mongo:7.0.37` (pinned to the version verified
  end-to-end; override with `restore.image`); the database defaults to
  `salvage_restore_test`.
- v1 restores logical archives only. Physical/filesystem-snapshot restore and
  oplog PITR are **roadmap**, not shipped (spec 0025 Non-goals); hardened
  topologies (replica sets, sharding, auth/TLS) are unverified.
- `scaffold`, `last-good`, and `fleet` report "not supported" for MongoDB
  targets.

**First run (MongoDB):**

```sh
cp salvage.mongodb.example.yaml salvage.yaml
# edit: source.path (your mongodump --archive file), the collections in checks
salvage check -config salvage.yaml
salvage run   -config salvage.yaml
```

See [`salvage.mongodb.example.yaml`](../../salvage.mongodb.example.yaml) and
`dev/mongodb/` for a reproducible harness (`make-backup.sh`).

## restic

`target.type: restic` (spec [0018](../../specs/0018-restic-engine.md)) is the
first **non-SQL** engine. It restores a restic filesystem snapshot into a
throwaway `restic/restic` container and validates it with **file/command probes
instead of SQL** — the `file_exists`, `file_count`, `checksum`, and `command`
check kinds, plus `http` for probing a restored service from the host (see
[Configuration](./02-configuration.md#check-kinds)).

- The restore image defaults to `restic/restic:0.19.0` (pinned to the version
  verified end-to-end; override with `restore.image`).
- The snapshot to restore comes from `source.snapshot` (default `latest`).
- A non-secret repo path/URL goes inline in `source.repository`; a **secret**
  repo, its password, or backend keys are forwarded by name via `source.pass_env`
  (`RESTIC_PASSWORD`, `RESTIC_REPOSITORY`, `AWS_*`/`B2_*`/`AZURE_*`) — never in
  the config.
- A local repo mounts via `source.repo_volume`.
- The restore fetch is allowed egress (a remote repo is downloaded), then the
  container is **dropped off every network before any check runs**, so a restored
  `command` check cannot reach out.

Because restic inherits the horizontal platform unchanged, a restic attestation
and a Postgres attestation sit side by side in the same ledger and evidence pack.

restic implements the full command surface (specs
[0028](../../specs/0028-cross-engine-scaffold.md) /
[0029](../../specs/0029-cross-engine-last-good-fleet.md)):

- [`scaffold`](./04-commands.md#scaffold) walks the restored tree and emits
  `file_exists`/`file_count` starter checks.
- [`last-good`](./04-commands.md#last-good) walks the repo's snapshots
  newest-first to find the freshest restorable one. **Each candidate is a full
  restore** — on a long snapshot history, bound the search with `-max`.
- [`fleet`](./04-commands.md#fleet) surveys the repository (metadata-only, one
  `restic snapshots --json` call): a restic repo is **one unit**, reported with
  its snapshot count and newest snapshot.

**First run (restic):**

```sh
cp salvage.restic.example.yaml salvage.yaml
# edit: source.repository + source.repo_volume (or a remote repo via pass_env),
#       source.snapshot (defaults to latest), the file paths in checks
export RESTIC_PASSWORD=…              # forwarded by name via pass_env
salvage check -config salvage.yaml
salvage run   -config salvage.yaml
```

See [`salvage.restic.example.yaml`](../../salvage.restic.example.yaml) and
`dev/restic/` for a reproducible harness.

## borg

`target.type: borg` (spec [0022](../../specs/0022-borg-engine.md)) is the
second filesystem engine — a near-exact sibling of restic. It extracts a
**BorgBackup archive** into a throwaway container and validates it with the
same probes: `file_exists`, `file_count`, `checksum`, `command`, and `http`
(see [Configuration](./02-configuration.md#check-kinds)).

The differences from restic are borg's own semantics:

- **The archive is required** (`source.archive`) — borg has no `latest` alias,
  so you name the archive to restore.
- The repository goes inline in `source.repository` (non-secret path/URL) or by
  name as `BORG_REPO` via `source.pass_env`; the passphrase is always forwarded
  by name (`BORG_PASSPHRASE` in `pass_env`) — never in the config. A local repo
  mounts via `source.repo_volume`.
- The restore image defaults to
  `ghcr.io/borgmatic-collective/borgmatic:2.1.6` (pinned; ships the
  end-to-end-verified borg 1.4.4 on `PATH` — borg publishes no official image).
  Override with `restore.image` if you prefer your own.
- The same two-phase isolation applies: the extract is allowed egress (a remote
  `ssh://` repo is downloaded), then the container is **dropped off every
  network before any check runs**.

borg lights up the same cross-engine commands as restic: `scaffold` (tree-walk
starter checks), `last-good` (walks the repo's archives newest-first — each
candidate is a **full extract**, so bound long histories with `-max`), and
`fleet` (metadata-only survey; a borg repo is one unit).

**First run (borg):**

```sh
cp salvage.borg.example.yaml salvage.yaml
# edit: source.archive (required — no "latest"), source.repository +
#       source.repo_volume (or BORG_REPO via pass_env), the file paths in checks
export BORG_PASSPHRASE=…              # forwarded by name via pass_env
salvage check -config salvage.yaml
salvage run   -config salvage.yaml
```

See [`salvage.borg.example.yaml`](../../salvage.borg.example.yaml) and
`dev/borg/` for a reproducible harness (`make-backup.sh`).

## exec (bring-your-own-restore)

`target.type: exec` (spec [0020](../../specs/0020-exec-engine.md)) covers backup
types Salvage does not natively restore — a bespoke script, a proprietary
database, a cloud-managed snapshot spun into an instance, or a restore too large
for the container model. The customer brings the **restore** (their own command);
Salvage **runs it**, then runs the customer's checks — expressed in the Salvage
config format — against whatever the command left behind, and produces the
verdict/report itself.

It is **Docker-free**: unlike the Postgres and restic engines, the exec engine
does not stand up a container. Salvage runs `restore.command` directly on the
host with the Salvage process's own privileges and environment, and the checks
then run from the same host — so the restore can land wherever the command puts
it (a local DB, a running service, a directory).

- **The restore is `restore.command`** — an argv array Salvage runs. Exit `0`
  means the restore succeeded; a non-zero exit is a **fail** verdict (not an
  operational error). A missing binary or an unusable `workdir`, by contrast, is
  an operational error (exit 2). See
  [Configuration → `target.restore` (exec)](./02-configuration.md#targetrestore-exec).
- **Checks run on the host.** The `command`, `file_exists`, `file_count`, and
  `checksum` kinds work against exec targets exactly as they do for restic, but
  resolve on the host filesystem (relative paths against `restore.workdir`). The
  **`http`** check kind — probe a restored service over HTTP — works here too
  (and for restic/borg). See
  [Configuration → Check kinds](./02-configuration.md#check-kinds).
- **`salvage check`** on an exec target skips the Docker preflight (no container
  is used) and simply validates the config.
- **`salvage scaffold`** works when `restore.workdir` is declared: it runs your
  restore command, walks the tree the command populated (the same bounded
  discovery restic/borg use), and emits `file_exists`/`file_count` starter
  checks. Without a `workdir` there is nothing observable, and scaffold says so
  instead of guessing. `last-good` and `fleet` report "not supported" — Salvage
  cannot enumerate a backup chain it does not manage.

**Honest scope:** the exec engine runs the *customer's* restore command on the
*customer's* host — Salvage does not independently reconstruct the backup — so
this sits squarely in the notary tier: "Salvage ran this restore procedure and
these checks passed, at this time." It still inherits the horizontal platform
unchanged: report, ledger, verify, dead-man's-switch, and evidence pack.

**First run (exec):**

```sh
cp salvage.exec.example.yaml salvage.yaml
# edit: restore.command (your restore script), restore.workdir, the checks
salvage check -config salvage.yaml    # validates config; skips the Docker preflight
salvage run   -config salvage.yaml
```

See [`salvage.exec.example.yaml`](../../salvage.exec.example.yaml) and the
worked config in
[Configuration → `target.restore` (exec)](./02-configuration.md#targetrestore-exec).

## Roadmap

The following are **roadmap**, not shipped capabilities — do not treat them as
present-tense:

- **More engines** — object-storage logical artifacts and others, each a
  sibling engine inheriting the same attestation surface (spec
  [0005](../../specs/0005-source-interface-and-roadmap.md)).
- **Deeper restores for the shipped engines** — MySQL physical/binlog
  (xtrabackup) restore and PITR (spec 0024 Open questions); MongoDB
  physical/oplog restore (spec 0025 Non-goals).

When these land they will document their own source kinds and check kinds; until
then, the shipped engines are **postgres**, **mysql**, **mongodb**, **restic**,
**borg**, and **exec**.
