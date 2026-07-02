# Getting started

Salvage proves your backups actually restore. It restores a backup into a
throwaway environment, asserts the data is really there and usable, and emits a
signed pass/fail verdict — the level-3 test (*"it restores and the data works"*)
that your backup and integrity tools stop one step short of. Salvage **augments**
your existing integrity checks; it is not a backup tool and won't ask you to
migrate anything.

## Prerequisites

- **Docker** — **required** for the container engines (Postgres, MySQL,
  MongoDB, restic, borg), which restore each backup into a disposable `--rm`
  container, so no host database or backup client is needed — but Docker itself
  must be installed and reachable. `salvage check` preflights it for you. The
  **`exec` engine is the exception**: it runs your own restore command on the
  host and needs no Docker.
- **Go 1.23+** — only needed to **build Salvage from source** (see below). If you
  install a prebuilt binary, you do not need Go.
- A backup artifact to test — a `pg_dump`/plain-SQL file, a pgBackRest repo
  (local or remote S3/R2), a `mysqldump` file, a `mongodump --archive` file, a
  restic snapshot, or a borg archive. See [Engines](./03-engines.md).

## Install / build

Salvage builds into a single binary with one dependency (`gopkg.in/yaml.v3`):

```sh
brew install go            # Go 1.23+; skip if you have a prebuilt binary
make build                 # produces ./salvage
```

Prebuilt binaries are published on GitHub releases (built via GoReleaser when a
`v*` tag lands).

## Your first run

1. **Copy an example config** and edit it for your backup:

   ```sh
   cp salvage.example.yaml salvage.yaml
   ```

   The example targets a local `pg_dump`/SQL file. **Testing something other
   than Postgres?** Each engine section in [Engines](./03-engines.md) — MySQL,
   MongoDB, restic, borg, exec — has its own first-run block and example
   config. See [Configuration](./02-configuration.md) for the full reference.

2. **Validate the config and preflight Docker** (no restore happens yet):

   ```sh
   ./salvage check -config salvage.yaml
   # ok — target "prod-orders-db" valid, docker reachable, 4 check(s) defined
   ```

3. **Run the restore-test** and read the verdict:

   ```sh
   ./salvage run -config salvage.yaml
   ```

   Salvage spins up a disposable Postgres container, restores your artifact,
   network-isolates the restored cluster once it reaches a consistent recovery
   point, then runs your assertions and prints a summary:

   ```
   salvage: target "prod-orders-db"
     restore   ok    (4213ms)
     check     ok    schema_present             ...
     check     ok    orders_not_empty           ...
     check     ok    latest_order_is_recent     ...
     verdict   PASS
   ```

   A JSON verdict is written to `report.out` (optionally ed25519-signed).

## The verdict and exit codes

The verdict is **`pass`** if and only if the restore succeeded **and** every
*required* check passed. Exit codes are identical across every command:

| Code | Meaning |
|-----:|---------|
| `0` | **pass** — restore succeeded and every required check passed |
| `1` | **fail** — restore failed or a required check failed (a *result*, not a crash) |
| `2` | **error** — operational problem (bad config, Docker unavailable, missing secret) |

## Where to go next

- [Configuration reference](./02-configuration.md) — the config file, check
  kinds, and expectations.
- [Engines](./03-engines.md) — all six engines (Postgres, MySQL, MongoDB,
  restic, borg, exec), each with a first-run block.
- [Commands](./04-commands.md) — every CLI command and its flags.
- [Attestation](./05-attestation.md) — local signing vs the hosted independent
  notary.
- [Scheduling & monitoring](./06-scheduling-and-monitoring.md) — unattended runs
  and the dead-man's-switch.
- [Evidence pack](./07-evidence-pack.md) — the auditor/insurer-ready record.
- [CI integration](./08-ci-integration.md) — scheduled restore-tests in CI with
  exit-code gating and `-json` report capture.
