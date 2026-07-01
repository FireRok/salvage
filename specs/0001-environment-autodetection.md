# 0001 — Environment auto-detection & zero-config restore

- **Status:** Proposed
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

Restoring a backup faithfully requires two kinds of knowledge about the source
cluster:

1. **Environment** — what it takes to *start* the restored cluster: the
   PostgreSQL major version and any startup-critical extensions (e.g.
   `shared_preload_libraries = 'timescaledb'`).
2. **Topology** — what's *inside*: which databases exist, which roles, and which
   extension versions are installed.

Today an operator must supply these by hand (`restore.image`,
`restore.database`, `restore.user`). That is brittle and does not scale to many
databases.

**Motivating finding (divina-db, R2 dogfood, 2026-06-29):** the production
`divina-db` repo restored fine at the file level, but the cluster runs
**TimescaleDB**, so a stock `postgres:17` image cannot start it
(`could not access file "timescaledb"`). Integrity checks were green; the restore
was unusable without a matching runtime. This is the *environment-dependency*
failure class Salvage exists to catch — and it should be **detected and reported
automatically**, not discovered by a cryptic startup crash.

## Goals

- Auto-detect the **environment** a backup needs before/at restore time.
- Auto-discover the **topology** from the restored cluster.
- Reduce required configuration to **intent only** (assertions, repo credentials,
  and an optional recovery target).
- Turn environment mismatches into **actionable guidance**, not crashes.

## Non-goals

- Deciding what "healthy" means — that is intent (the assertions) and is always
  operator-supplied.
- Managing or creating backups (Salvage validates; it does not back up).
- Auto-provisioning arbitrary third-party extension binaries at scale (tracked as
  an open question, not a v1 requirement).

## Detectability matrix

| Idiosyncrasy | Auto-detectable? | Stage | Mechanism |
|---|---|---|---|
| PG major version | Yes | Offline (pre-start) | Read `PG_VERSION` in restored PGDATA |
| Required preload extensions | Yes | Offline (pre-start) | Parse `shared_preload_libraries` in `postgresql.conf` / `postgresql.auto.conf` |
| Database names | Yes | Post-start | `SELECT datname FROM pg_database` |
| Roles / superuser | Yes (common case) | Post-start | Relaxed `pg_hba` trust + bootstrap-superuser (OID 10) lookup / `postgres --single` fallback |
| Exact extension version | Hard | Post-start / log | Catalog needs a running cluster; mitigate via backward-compat image, log parsing, or probe-then-rebuild |
| What "healthy" means | No (by nature) | n/a | Operator intent — the assertions |

## Requirements

**R1 — Offline pre-flight inspection.** After the backup's files are materialized
but **before** starting Postgres, Salvage MUST read:
- `PG_VERSION` → the PostgreSQL major version;
- `postgresql.conf` and `postgresql.auto.conf` → `shared_preload_libraries` (the
  startup-blocking extension set) and other startup-critical settings.
These are plain-text files in PGDATA; no running server is required.

**R2 — Environment requirement surfacing & image validation.** From R1, Salvage
MUST report the required environment (e.g. "PG 17 + `shared_preload_libraries=timescaledb`").
If the configured `restore.image` cannot satisfy it (wrong major version, missing
preload library), Salvage SHOULD fail fast with a precise, actionable message
rather than letting Postgres crash on start.

**R3 — Post-start topology discovery.** Once the cluster reaches a consistent
state, Salvage MUST be able to enumerate databases (`pg_database`) and, per
database, installed extensions and versions (`pg_extension`). Checks SHOULD be
able to target *all* discovered user databases without per-database name config.

**R4 — Superuser/role auto-discovery.** Salvage MUST connect and run checks
without the operator specifying a role in the common case. Approach: the restored
`pg_hba.conf` is relaxed to trust local connections (already implemented), then
the connecting role is resolved by trying the conventional superuser and, failing
that, discovering the bootstrap superuser (catalog OID 10) — `postgres --single`
is an acceptable no-auth fallback to learn it. `restore.user` remains an optional
override.

**R5 — Exact extension version handling.** When a preload extension's library
version does not match the catalog, Salvage MUST detect it (the startup log emits
a self-describing message, e.g. "extension has version X, library is version Y")
and report the required version precisely. Strategies, in preference order:
(a) use a recent, backward-compatible image (a newer library generally loads an
older catalog); (b) parse the required version from the log and report/retry;
(c) probe-then-rebuild (a throwaway start to read the version, then rebuild the
image to match).

**R6 — `salvage inspect` command.** A pre-flight command that reports the
environment (PG version, required extensions) and, where feasible, topology
(databases, roles) for a backup **without** requiring a full successful start.
Output MUST be available as both human-readable text and JSON. `salvage inspect`
is useful on its own (before committing to a full restore) and is the natural
front-end for R1–R3.

**R7 — Minimal config surface.** After R1–R6, the only configuration an operator
must provide is: the **checks** (intent), the **repo credentials**
(`source.pass_env`), and an optional **recovery target** (default: latest).
`restore.image`, `restore.database`, and `restore.user` SHOULD become optional
(auto-detected / auto-discovered) with explicit overrides retained.

**R8 — Reconstruct config that lives outside PGDATA.** Some clusters
(Debian-packaged Postgres) keep `postgresql.conf` / `pg_hba.conf` / `pg_ident.conf`
in `/etc/postgresql/<ver>/<cluster>/`, OUTSIDE PGDATA — so a pgBackRest backup of
the data dir lacks them, offline inspection (R1) can't read them, and the restored
cluster won't start. When `postgresql.conf` is missing, Salvage MUST synthesize a
minimal one: the required preload extensions (operator-declared via
`restore.preload_libraries` until auto-detected) plus the recovery-critical `max_*`
settings — for which `pg_controldata` is the authoritative OFFLINE source — so
Postgres's "value must be >= primary" check passes, with `hot_standby = on` so
read-only checks work even at a paused recovery target. (Validated against the
Divina R2 backup, which is Debian-packaged.)

## Design notes

Pipeline placement (pgBackRest path):

```
StartRestoreEnv ─▶ pgbackrest restore ─▶ [R1 offline inspect] ─▶ start postgres ─▶ [R3/R4 discovery] ─▶ checks
                                              │                       │
                                          PG_VERSION,           pg_database,
                                          shared_preload_libs   pg_extension, roles
```

- R1/R2 gate the start: if the environment can't be satisfied, fail with guidance
  before the (expensive) start attempt.
- R3/R4 feed the checks: discovered databases/roles parameterize assertion runs.
- The chicken-and-egg (R5) is isolated to *exact extension version*; everything
  else is cleanly detectable offline or post-start.

This auto-detection is also a product differentiator: "point Salvage at a repo and
it tells you what it takes to bring this back." `salvage inspect` demos that value
independently of a full restore.

## Open questions

- **Image strategy:** require the operator to supply a matching image, maintain a
  curated registry of extension images, or auto-build on demand? (v1 likely:
  detect + require/guide; auto-build later.)
- **Non-preload extensions** (e.g. PostGIS) don't block startup but may be needed
  by specific checks — detect and warn, or ignore until a check fails?
- **`postgres --single` reliability** across versions for superuser discovery.
- **Multiple major TimescaleDB versions** and other extensions with strict
  load-time version gates — how far does backward-compat carry us in practice?

## Acceptance criteria

1. Given the `divina-db` repo, `salvage inspect` reports **PG 17** and
   **requires `timescaledb`** without any manual environment config, and without a
   successful cluster start.
2. Given a restore where the configured image lacks a required preload extension,
   Salvage fails with a message naming the missing extension (R2) instead of a raw
   Postgres crash.
3. Given a successful restore, checks run against **all** user databases with no
   per-database name in the config (R3), connecting with an **auto-discovered**
   superuser (R4) for a standard cluster.
4. Given an extension version mismatch, the report names the **required version**
   (R5).
5. A config containing only `source` (kind/stanza/pass_env) + `checks` performs a
   full restore-test, with image/database/user auto-resolved (R7).
