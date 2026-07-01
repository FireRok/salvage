# Salvage corpus

A small set of **real, downloadable Postgres sample databases** that the Salvage
corpus runner loads, backs up, and restore-verifies. The goal is breadth: varied
schemas, varied sizes, and varied **extension** needs (vanilla Postgres,
TimescaleDB, PostGIS), so the restore-verifier is exercised against the kinds of
databases people actually run.

## Layout

```
dev/corpus/
  manifest.yaml          # the list of entries the runner consumes
  sources/<name>/fetch.sh# idempotent downloader -> .cache/<name>/
  kitchensink/           # hand-built schema (owned by a separate task)
  .cache/                # downloaded artifacts (gitignored, fetch-on-demand)
  README.md              # this file
```

## How the runner uses the manifest

`manifest.yaml` has a top-level `entries:` list. For each entry the runner:

1. Runs `fetch` (if present) to populate `dev/corpus/.cache/<name>/`. Scripts are
   idempotent — re-running skips already-downloaded files.
2. Starts the docker `image` and creates the target `database`.
3. Applies `load.paths` **in order** according to `load.kind`:
   - `sql_files` — `psql -f` each file.
   - `pg_dump_custom` — `pg_restore` each custom-format archive.
   - `shapefile` — `shp2pgsql` the `.shp` into the db (postgis image bundles it).
   - `csv_copy` — `\copy` each CSV into its table after the schema SQL.
4. Backs the database up and runs Salvage's restore-verification pass.

Some entries need an extension created first or a special dump/restore dance;
those are spelled out in each entry's `runner_notes` in the manifest. The most
important ones are summarized in **Per-source notes** below.

## Sources

| Source | Image | License | Approx size | Exercises |
|---|---|---|---|---|
| **pagila** | `postgres:16` | Pagila license (BSD/PostgreSQL-style) | ~3.4 MB SQL | views, sequences, triggers, functions, partitioned tables, tsvector FTS, domains, enums |
| **chinook** | `postgres:16` | MIT | ~560 KB SQL | SERIAL/identity, FK graph, self-referencing FK; script picks its own db name |
| **northwind** | `postgres:16` | community port (MS sample) | ~350 KB SQL | composite PKs, M:N join tables, employee hierarchy, FK actions |
| **timescale** | `timescale/timescaledb:latest-pg16` | Timescale free sample | ~8.4 MB archive (~70 MB CSV) | `timescaledb` extension, hypertable + chunk partitioning |
| **postgis** | `postgis/postgis:16-3.4` | public domain (Natural Earth) | ~215 KB zip | `postgis` extension, geometry column, GiST spatial index |
| **kitchensink** | `postgres:16` | in-repo | small | broad type/feature coverage (built by sibling task) |

### Redistribution / commit policy

All downloaded data lands in `.cache/`, which is **gitignored** — nothing fetched
is committed. Datasets are fetched on demand at corpus-run time. Specific notes:

- **timescale** — the conditions CSV is ~70 MB; fetch-on-demand only, never commit.
- **pagila / chinook / northwind / postgis** — small and redistributable, but
  still fetched on demand and not committed (keeps the repo lean and the licenses
  cleanly upstream). Pagila and Natural Earth are freely redistributable; Chinook
  is MIT; Northwind is a community port of a Microsoft sample.
- Only `kitchensink/kitchensink.sql` (hand-authored in this repo) is committed.

## Per-source notes (for the runner)

**pagila** — apply `pagila-schema.sql` then `pagila-data.sql` into `corpus`. No
extension needed.

**chinook** — the upstream SQL runs `CREATE DATABASE chinook_serial` and
`\c chinook_serial`, so the data lands in **chinook_serial**, not `corpus`. Apply
it against a maintenance db (e.g. `postgres`) and back up `chinook_serial`. The
manifest's `database:` for this entry is set to `chinook_serial` accordingly.

**northwind** — single self-contained file, applied into `corpus`. No extension.

**timescale** — needs `CREATE EXTENSION timescaledb;` before loading. `weather.sql`
creates `locations` + `conditions` and calls
`create_hypertable('conditions','time')` but does **not** load data, so after
applying it the runner must copy the two CSVs:

```sql
\copy locations  FROM '.cache/timescale/weather_small_locations.csv'  CSV
\copy conditions FROM '.cache/timescale/weather_small_conditions.csv' CSV
```

For restore verification, use Timescale's documented dump/restore flow
(`SELECT timescaledb_pre_restore();` before reload, `timescaledb_post_restore();`
after) or `timescaledb-backup` — a naive `pg_dump`/`pg_restore` of a hypertable
won't round-trip cleanly.

**postgis** — needs `CREATE EXTENSION postgis;` first. Load the shapefile with
`shp2pgsql` (bundled in the `postgis/postgis` image):

```sh
shp2pgsql -s 4326 -I .cache/postgis/ne_110m_admin_0_countries.shp \
  ne_110m_admin_0_countries | psql -d corpus
```

(`-s 4326` = WGS84 SRID, `-I` = build the GiST index.)

## Verification status

All five fetch scripts were run and confirmed reachable/working:

| Source | Status | Fetched size |
|---|---|---|
| pagila | OK | schema 52 KB + data 3.3 MB |
| chinook | OK | 562 KB |
| northwind | OK | 350 KB |
| timescale | OK | 8.4 MB archive -> weather.sql + locations CSV (40 KB) + conditions CSV (70 MB) |
| postgis | OK | 215 KB zip -> shp/dbf/prj/shx |
