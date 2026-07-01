# Salvage

**Prove your backups actually restore.** Salvage restores a backup into a
throwaway Postgres, asserts the data is really there and usable, and emits a
signed pass/fail verdict. It's the test your backup tool refuses to run.

> Status: **early / work in progress.** The Postgres restore-test loop is
> validated end-to-end across logical dumps and pgBackRest (local and remote
> S3/R2) — including a real **TimescaleDB 17 production restore from Cloudflare
> R2**. Signing and the hosted attestation service are evolving; expect breaking
> changes before v1.

## Why

There are three levels of backup confidence, and most tooling stops one short:

| Level | Proves | Who does it |
|------:|--------|-------------|
| 1 | the backup job ran | your cron job |
| 2 | the bits aren't rotted (integrity) | `restic check`, `borg check`, `kopia verify`, pgBackRest manifests |
| 3 | **it restores and the data works** | **Salvage** |

The level-2 tools deliberately stop short — their own docs note that a real
restore is the gold standard but is too expensive to run by default. Salvage
does exactly that Level-3 test, on a schedule, and gives you a verdict you can
act on (and, later, attest to).

Salvage **augments** your integrity checks; it does not replace them.
"Will it restore?" is a property of *artifact × restore-procedure ×
target-environment*, and the only way to know is to run it. Salvage is **not a
backup tool** and won't ask you to migrate anything — it runs *on top of* the
backups you already have.

**Representative, not universal.** A passing restore-test proves the backup
restores *in the environment Salvage used* — a strong sample, not a mathematical
guarantee. The closer that environment mirrors production, the more the result
generalizes; reports record the environment used so the claim is honestly
scoped. See [`specs/0000`](./specs/0000-product-overview.md).

## How it works

```
salvage run -config salvage.yaml
```

1. Spins up a disposable Postgres container (via Docker — no host Postgres
   client needed), restored with `--rm`.
2. Restores your backup artifact into it.
3. Network-isolates the restored cluster once it reaches a consistent recovery
   point, then runs your assertions: tables present, row counts in range, the
   newest row is recent, a known value matches, an invariant holds.
4. Writes a JSON verdict (optionally ed25519-signed) and exits `0` pass /
   `1` fail.

See [`salvage.example.yaml`](./salvage.example.yaml) for a worked config.

## Sources

Both logical and physical/PITR Postgres restores are validated end-to-end:

| Kind | What it restores | Example |
|------|------------------|---------|
| `pg_dump` | a `pg_dump` archive (custom/dir/tar) | [`salvage.example.yaml`](./salvage.example.yaml) |
| `sql` | a plain `.sql` dump | [`salvage.example.yaml`](./salvage.example.yaml) |
| `pgbackrest` | a pgBackRest repo — **local filesystem** | [`salvage.pgbackrest.example.yaml`](./salvage.pgbackrest.example.yaml) |
| `pgbackrest` | a pgBackRest repo — **remote S3 / Cloudflare R2** | [`salvage.pgbackrest-s3.example.yaml`](./salvage.pgbackrest-s3.example.yaml) |

The physical path delegates to `pgbackrest restore` and waits for the cluster to
reach a consistent recovery point before asserting. A local repo is mounted via
`source.repo_volume`; a remote S3/R2 repo lives in the image's `pgbackrest.conf`
and credentials are forwarded by name via `source.pass_env`. See
[`dev/pgbackrest/`](./dev/pgbackrest/) for a self-contained, reproducible
harness (`make-backup.sh`, `Dockerfile`, `pgbackrest.conf`).

## Commands

```sh
salvage run    -config salvage.yaml   # restore into a throwaway db and assert it works
salvage check  -config salvage.yaml   # validate config + preflight Docker (no restore)
salvage inspect [-json] <pgdata-dir>  # offline pre-flight on an unpacked PGDATA dir
salvage version
salvage help
```

`salvage inspect` reads a PGDATA directory **without starting Postgres** and
reports the PG major version (from `PG_VERSION`), the
`shared_preload_libraries` the cluster requires, and the number of databases —
so you can size the restore image (and `restore.preload_libraries`) before a
full run. Add `-json` for machine-readable output.

**Exit codes** (same across all commands):

| Code | Meaning |
|-----:|---------|
| `0` | **pass** — restore succeeded and every required check passed |
| `1` | **fail** — restore failed or a required check failed (a *result*, not a crash) |
| `2` | **error** — operational problem (bad config, Docker unavailable, missing secret) |

## Checks

Each check runs one SQL statement that returns a single scalar, with exactly one
expectation:

| Expectation | Asserts |
|-------------|---------|
| `expect_min` / `expect_max` | the numeric result is within range (either or both) |
| `equals` | the result equals a given string |
| `max_age` | the result is a timestamp no older than a Go duration (e.g. `36h`) |
| `bool` | the result is a boolean equal to `true`/`false` (e.g. `SELECT count(*) = 0 FROM orphaned_rows`) |

Each check also carries a `severity`:

- `required` (default) — a failure fails the verdict.
- `advisory` — a failure is **recorded but does not fail the verdict**.

The verdict is `pass` iff the restore succeeded **and** every *required* check
passed.

## Key restore config

```yaml
target:
  source:
    kind: pgbackrest
    stanza: prod
    # Forward repo credentials BY NAME — secret values never hit the config,
    # the image, or any command line:
    pass_env:
      - PGBACKREST_REPO1_S3_KEY
      - PGBACKREST_REPO1_S3_KEY_SECRET
  restore:
    image: your-registry/postgres-pgbackrest:17  # must carry postgres + pgbackrest + needed extensions
    database: postgres                           # the database checks connect to
    user: postgres                               # the role checks connect as (default: postgres)
    preload_libraries: [timescaledb]             # synthesizes a minimal postgresql.conf
    timeout: 30m                                  # bounds the whole restore phase
```

- **`restore.image`** — the container image. Logical restores default to
  `postgres:16`; pgBackRest restores have no default and must carry both
  `postgres` and `pgbackrest` (plus any extensions the source uses).
- **`restore.preload_libraries`** — seeds `shared_preload_libraries` in a
  synthesized minimal `postgresql.conf` for clusters that keep their config
  *outside* PGDATA (e.g. Debian-packaged Postgres). Required for extensions like
  `timescaledb`, which must be preloaded or the server won't start.
- **`source.pass_env`** — forwards named environment variables from Salvage's
  own process into the restore container *by name*, so secret values never
  appear in command arguments (e.g. `PGBACKREST_REPO1_S3_KEY[_SECRET]` for an
  S3/R2 repo).

## Security & isolation

- **Ephemeral throwaway.** The restore container runs `--rm` and is destroyed
  when the run ends — nothing persists.
- **Network isolation.** Once the restored cluster reaches consistency it is
  cut off from the network before any check SQL runs.
- **Secrets by reference only.** Credentials are forwarded by *name*
  (`source.pass_env`), never embedded in config or command lines. Use a
  **read-only** repo token.

## Quickstart

```sh
brew install go            # Salvage builds with Go 1.23+
make build                 # produces ./salvage   (one dependency: gopkg.in/yaml.v3)
cp salvage.example.yaml salvage.yaml   # then edit
./salvage check            # validate config + preflight Docker
./salvage run              # restore-test and print the verdict
```

## Roadmap

- **More sources.** Logical dumps and pgBackRest (local + S3/R2) are validated;
  next are object-storage logical artifacts and `restic` / `borg`. See
  [`specs/0005`](./specs/0005-source-interface-and-roadmap.md).
- **Independent attestation (hosted).** A local signature proves integrity, not
  independence. The hosted service issues *independently verifiable* attestation
  reports — the kind an auditor or cyber-insurer will accept. This is the part
  you can't self-host, and it's how the project sustains itself.
- **Fleet view & MSP multi-tenancy.** One dashboard of "every backup, tested,
  green or red" across clients.

## Specs

Design intent lives in [`specs/`](./specs/) — spec-driven development, the source
of truth for what to build. Start with
[`specs/0000`](./specs/0000-product-overview.md) (product model) and
[`specs/0001`](./specs/0001-environment-autodetection.md) (environment
auto-detection and zero-config restore).

## License

Salvage is **Fair Source**, not open source — licensed under the
[Functional Source License](./LICENSE) (FSL-1.1-ALv2). Use, run, self-host, and
modify it freely for any purpose **except** offering it as a competing
commercial service. Each release becomes Apache 2.0 two years after publication.

## Development

Built in private on Forgejo; release branches and tags are mirrored to public
GitHub. The simplest mirror is Forgejo's built-in **push mirror** (repo →
Settings → Mirroring), scoped to release tags. CI runs on Forgejo
([`.forgejo/workflows/ci.yml`](./.forgejo/workflows/ci.yml)); releases build via
GoReleaser on GitHub when a `v*` tag lands
([`.github/workflows/release.yml`](./.github/workflows/release.yml)).

```sh
make tidy   # fetch deps
make vet
make test
make build
```
