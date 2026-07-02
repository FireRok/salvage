# Salvage

**Prove your backups actually restore.** Salvage restores a backup into a
throwaway environment (Postgres, MySQL, MongoDB, restic, borg, or your own
restore command), asserts the data is really there and usable, and emits a
signed pass/fail verdict. It's the test your backup tool refuses to run.

> Status: **early / work in progress.** The Postgres restore-test loop is
> validated end-to-end across logical dumps and pgBackRest (local and remote
> S3/R2) — including a real **TimescaleDB 17 production restore from Cloudflare
> R2** — and the restic, borg, MySQL, and MongoDB engines are verified against
> live Docker (see `dev/<engine>/`). Signing and the hosted attestation service
> are evolving; expect breaking changes before v1.

**Documentation:** the [user guide](./docs/guide/README.md) covers every
engine, the config format, all commands, attestation, scheduling, and CI
integration. User-visible changes are tracked in [`CHANGELOG.md`](./CHANGELOG.md).

## Requirements

- **Docker** — required for the container engines (`postgres`, `mysql`,
  `mongodb`, `restic`, `borg`). Salvage spins up a disposable container to
  restore into, so no host database or backup client is needed — just a
  reachable Docker daemon.
- **Go 1.23+** — only to build from source; a prebuilt binary needs nothing.
- **The `exec` engine needs no Docker.** For a bring-your-own-restore target
  (`target.type: exec`), Salvage runs *your* restore command on the host and runs
  the checks against whatever it produced — so it depends only on your own restore
  tooling (a script, `psql`/`mysql`, etc.), not on Docker. Note that an `exec`
  restore and its `command` checks run with the Salvage process's privileges: the
  config is trusted input, exactly like a shell script you'd run by hand.

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
restores *in the environment Salvage used* — a strong sample, not a universal
claim. The closer that environment mirrors production, the more the result
generalizes; reports record the environment used so the claim is honestly
scoped. See [`specs/0000`](./specs/0000-product-overview.md).

## How it works

```
salvage run -config salvage.yaml
```

1. Spins up a disposable container for the target engine (via Docker — no host
   database client needed), run with `--rm`. Postgres shown here; MySQL,
   MongoDB, restic, and borg work the same way, and `exec` skips the container
   entirely.
2. Restores your backup artifact into it.
3. Network-isolates the restored cluster once it reaches a consistent recovery
   point, then runs your assertions: tables present, row counts in range, the
   newest row is recent, a known value matches, an invariant holds.
4. Writes a JSON verdict (optionally ed25519-signed) and exits `0` pass /
   `1` fail.

See [`salvage.example.yaml`](./salvage.example.yaml) for a worked config.

## Sources

Salvage is database-first (Postgres), expanding the verification universe one
engine at a time (spec [0016]/[0017]). Six engines ship today — Postgres
(logical + physical/PITR), MySQL, MongoDB, the restic and borg filesystem
engines, and a bring-your-own-restore `exec` engine:

| `target.type` | Kind | What it restores | Example |
|------|------|------------------|---------|
| `postgres` | `pg_dump` | a `pg_dump` archive (custom/dir/tar) | [`salvage.example.yaml`](./salvage.example.yaml) |
| `postgres` | `sql` | a plain `.sql` dump | [`salvage.example.yaml`](./salvage.example.yaml) |
| `postgres` | `pgbackrest` | a pgBackRest repo — **local filesystem** | [`salvage.pgbackrest.example.yaml`](./salvage.pgbackrest.example.yaml) |
| `postgres` | `pgbackrest` | a pgBackRest repo — **remote S3 / Cloudflare R2** | [`salvage.pgbackrest-s3.example.yaml`](./salvage.pgbackrest-s3.example.yaml) |
| `mysql` | `mysql` | a logical `mysqldump` `.sql` dump | [`salvage.mysql.example.yaml`](./salvage.mysql.example.yaml) |
| `mongodb` | `mongodb` | a `mongodump --archive` file | [`salvage.mongodb.example.yaml`](./salvage.mongodb.example.yaml) |
| `restic` | `restic` | a restic filesystem snapshot (local or remote repo) | [`salvage.restic.example.yaml`](./salvage.restic.example.yaml) |
| `borg` | `borg` | a BorgBackup archive (local or remote repo) | [`salvage.borg.example.yaml`](./salvage.borg.example.yaml) |
| `exec` | — | whatever **your** restore command produces (no Docker) | [`salvage.exec.example.yaml`](./salvage.exec.example.yaml) |

The physical Postgres path delegates to `pgbackrest restore` and waits for the
cluster to reach a consistent recovery point before asserting. A local repo is
mounted via `source.repo_volume`; a remote S3/R2 repo lives in the image's
`pgbackrest.conf` and credentials are forwarded by name via `source.pass_env`. See
[`dev/pgbackrest/`](./dev/pgbackrest/) for a self-contained, reproducible
harness (`make-backup.sh`, `Dockerfile`, `pgbackrest.conf`).

The restic and borg paths restore a snapshot/archive into a throwaway container
and validate it with non-SQL check kinds — `file_exists`, `file_count`,
`checksum`, and `command` — inheriting the report + attestation + monitoring
layers unchanged. Passwords/passphrases and backend keys are forwarded by name
via `source.pass_env` (never in the config); the container is dropped off every
network after restore and before checks. MySQL reuses the Postgres `sql` check
kind; MongoDB brings its own `collection_count` and `doc_query` kinds. See
[`dev/`](./dev/) for reproducible per-engine harnesses (`make-backup.sh`) and
the [engines chapter](./docs/guide/03-engines.md) for a first-run block per
engine.

[0016]: ./specs/0016-modular-engines.md
[0017]: ./specs/0017-verification-attestation-platform.md

## Commands

```sh
salvage run   [-json] -config salvage.yaml   # restore into a throwaway db and assert it works (-json: report to stdout)
salvage check -config salvage.yaml           # validate config + preflight Docker (no restore)
salvage inspect [-json] <pgdata-dir>         # offline pre-flight on an unpacked PGDATA dir
salvage version [-check]                     # -check: also report whether a newer release exists (never auto-updates)
salvage help
```

The full set — including `scaffold [-cap N]` (postgres/mysql/restic/borg/exec),
`last-good` and `fleet` (pgBackRest/restic/borg), `schedule`, `attest`,
`verify [-json]`, and `mcp` — is in the
[command reference](./docs/guide/04-commands.md). `run -json` and
`verify -json` emit machine-readable JSON (with a `schema_version`) for CI and
agents; `salvage mcp` serves the whole loop as MCP tools over stdio for agent
runtimes; see the [CI integration chapter](./docs/guide/08-ci-integration.md).
`run`, `check`, `scaffold`, `last-good`, `fleet`, and `attest` accept
`-verbose`/`-quiet` for stderr diagnostics (stdout, report bytes, and exit
codes never change).

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

A check asserts one fact about the restored data. The check *kind* varies by
engine — `sql` for Postgres/MySQL, `collection_count`/`doc_query` for MongoDB,
file/command probes for restic/borg/exec (see the
[configuration reference](./docs/guide/02-configuration.md#check-kinds)) — but
the shape is the same: a subject that yields a scalar, with an expectation:

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

## Install

Prebuilt binaries ship for macOS and Linux (amd64/arm64) — no Go needed.

**Install script** — detects your platform, verifies the artifact's SHA-256
against the release manifest *before* unpacking (plus the manifest's cosign
signature when `cosign` is installed), and installs the binary. It never
invokes `sudo`; `SALVAGE_VERSION=vX.Y.Z` pins a release and
`SALVAGE_INSTALL_DIR` overrides the destination (default: `/usr/local/bin`
when writable, else `~/.local/bin`):

```sh
curl -fsSL https://salvage.sh/install.sh | sh
```

**Homebrew** (macOS and Linuxbrew) — the tap formula is updated automatically
by the release pipeline:

```sh
brew install firerok/salvage/salvage
```

**`go install`** (Go 1.23+):

```sh
go install salvage.sh/cmd/salvage@latest
```

**Build from source** (Go 1.23+; one dependency: `gopkg.in/yaml.v3`):

```sh
git clone https://github.com/firerok/salvage && cd salvage
make build                 # produces ./salvage
```

On every path, `salvage version` reports the release version.

### Verify the artifacts

Every release publishes `checksums.txt` (SHA-256 of each archive) plus a
keyless [cosign](https://docs.sigstore.dev/) signature over it
(`checksums.txt.sigstore.json` — a sigstore bundle carrying the signature,
the ephemeral certificate, and the transparency-log proof), bound to this
repository's release-workflow identity and recorded in the Rekor transparency
log — so anyone can verify a download end-to-end using only public
information (cosign ≥ 2.4):

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp \
    '^https://github\.com/firerok/salvage/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

sha256sum --check --ignore-missing checksums.txt   # macOS: shasum -a 256 -c
```

A release whose signature does not verify must be treated as invalid. The
install script always enforces the checksum verification and adds the
signature verification when `cosign` is present. (Releases up to v0.2.0
predate signing and carry no signature bundle.)

## Quickstart

```sh
curl -fsSL https://salvage.sh/install.sh | sh    # or: brew install firerok/salvage/salvage
cp salvage.example.yaml salvage.yaml   # then edit
salvage check              # validate config + preflight Docker
salvage run                # restore-test and print the verdict
```

## Roadmap

**Shipped today:** six engines — Postgres (logical + pgBackRest, local and
S3/R2, incl. PITR), MySQL and MongoDB (logical dumps), `restic` and `borg`
filesystem engines, and a bring-your-own-restore `exec` engine; machine-readable
`run -json`/`verify -json` output; cross-engine `scaffold`, `last-good`, and
`fleet`; default-on report redaction with a pre-attest secret scan; client-side
`alerts:` hooks on run/attest; a `salvage mcp` agent-tool server; the hosted
independent-attestation notary (append-only ledger + public verify page) with
orgs/teams, share tokens, and a shareable evidence URL; scheduled attestation
with a dead-man's-switch and webhook/Slack/PagerDuty alert destinations; and
the auditor/insurer evidence pack. See [`specs/`](./specs/README.md) for status
per feature.

Next:

- **More engines and deeper restores.** Object-storage artifacts, MySQL
  physical/binlog restore, MongoDB oplog PITR — each engine inherits the same
  validation, report, and attestation surface
  ([`specs/0017`](./specs/0017-verification-attestation-platform.md)). The
  `exec` engine already covers arbitrary/proprietary restore procedures today.
- **Hosted execution tier.** Beyond the notary (which counter-signs a restore
  *you* ran), an optional tier where Salvage supplies the restore environment
  itself — closing the last gap between "independently attested" and
  "independently executed."
- **Fleet view & MSP multi-tenancy.** One dashboard of "every backup, tested,
  green or red" across clients
  ([`specs/0008`](./specs/0008-hosted-control-plane.md)).

## Specs

Design intent lives in [`specs/`](./specs/) — spec-driven development, the source
of truth for what to build. Start with
[`specs/0000`](./specs/0000-product-overview.md) (product model) and
[`specs/0001`](./specs/0001-environment-autodetection.md) (environment
auto-detection and zero-config restore).

## License

Salvage is **Fair Source** — free to run and self-host, not free to resell —
licensed under the [Functional Source License](./LICENSE) (FSL-1.1-ALv2). Use,
run, self-host, and modify it freely for any purpose **except** offering it as
a competing commercial service. Each release becomes Apache 2.0 two years after
publication.

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
