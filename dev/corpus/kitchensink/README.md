# Kitchen-sink corpus

A deliberately-diverse, single-file Postgres schema for Salvage's test
corpus. Its job is to exercise the **restore** edge cases Salvage must
survive: when this database is dumped (`pg_dump`) and restored
(`pg_restore` / `psql`) into a fresh cluster, it stresses cluster
startup, `salvage inspect`, `salvage scaffold` (check generation), and
check execution.

Targets the **stock `postgres:16` image only** — stock contrib
extensions, no `timescaledb` / `postgis`.

## Load

```sh
psql -v ON_ERROR_STOP=1 -f kitchensink.sql
```

Verified end-to-end against a throwaway container:

```sh
CID=$(docker run -d --rm -e POSTGRES_PASSWORD=x -e POSTGRES_DB=corpus postgres:16)
# poll until ready:
#   docker exec -e PGPASSWORD=x $CID psql -h 127.0.0.1 -U postgres -d corpus -tAc 'select 1'
docker cp kitchensink.sql $CID:/tmp/ks.sql
docker exec -e PGPASSWORD=x $CID psql -h 127.0.0.1 -U postgres -d corpus -v ON_ERROR_STOP=1 -f /tmp/ks.sql
docker kill $CID
```

The script also survives a full `pg_dump -Fc` →
`pg_restore --exit-on-error` round-trip (the actual Salvage path),
including the populated materialized view and partitioned tables.

## What it covers

### Extensions (all in stock `postgres:16`)
`citext`, `hstore`, `pgcrypto`, `"uuid-ossp"`, `pg_trgm`, `btree_gist`
— each used in real columns/defaults (e.g. `gen_random_uuid()`,
`uuid_generate_v4()`, `crypt()/gen_salt()`, citext usernames, hstore
attributes, trgm + tsvector GIN indexes, btree_gist exclusion).

### Schemas
`public`, `inventory`, `analytics`.

### Type spread (mostly on `public.wide_types`)
smallint / int / bigint, numeric, real / double precision, money,
text / varchar / char, citext, boolean, bytea, json, jsonb, 1-D and
multi-dimensional arrays, uuid (default `gen_random_uuid()`),
date / time / timestamp / timestamptz / interval, int4range / tstzrange,
inet / cidr / macaddr, bit / varbit, tsvector, hstore, an **enum**
(`order_status`), a **composite** (`us_address`), two **domains**
(`positive_money`, `email_addr`), and a **generated column**
(`c_int_doubled GENERATED ALWAYS AS (...) STORED`).

### Constraints
PKs, FKs, UNIQUE, CHECK, a **circular FK pair** on
`departments`/`employees` (`DEFERRABLE INITIALLY DEFERRED`, seeded in one
deferred transaction), and an **exclusion constraint** on
`room_bookings` (`EXCLUDE USING gist (room_id WITH =, during WITH &&)`
via btree_gist).

### Partitioning
- `inventory.events` — **RANGE** by month (`2026_05`, `2026_06`,
  `DEFAULT`), with data in each.
- `inventory.shards` — **HASH** (modulus 3).
- `inventory.regional_stock` — **LIST** by region (us / eu / default).

### Inheritance
`inventory.vehicles` **INHERITS** `inventory.assets` (legacy
table-inheritance form, distinct from partitioning).

### Views
- `analytics.customer_invoices` — join view using a helper function.
- `analytics.wide_summary` — aggregate view over the generated column.
- `analytics.revenue_by_status` — **materialized view**, populated
  (`WITH DATA` + `REFRESH`), with a UNIQUE index.

### Functions, triggers, sequence
- `touch_updated_at()` — `updated_at` touch trigger on `wide_types`
  and `customers`.
- `assign_invoice_number()` — insert trigger pulling from the explicit
  `invoice_number_seq` **sequence**.
- `full_name()` — immutable SQL helper used by a view.

### Edge cases
- `public.empty_table` — created, never populated (empty).
- `public.no_pk_log` — **no primary key**.
- `public."Weird Name"` — **weird quoted identifier**, with reserved-word
  columns `"select"` / `"from"` and a `"Mixed Case"` column.
- `public.nullable_bits` — rows carrying **NULLs**.
- `public.heartbeats` — **recent `timestamptz`** data (`now()`-based) so
  freshness checks have a live target.

Most tables carry a few seed rows so non-empty / freshness checks pass;
object comments are attached so `salvage inspect` sees them too.

## Verified object counts (clean load)

| Object | Count |
|---|---|
| Base tables (non-system, incl. partitions) | 25 |
| Views | 2 |
| Materialized views | 1 |
| Extensions | 6 |
| Schemas (public/inventory/analytics) | 3 |
| Enum types | 1 |
| Domains | 2 |
| Composite types | 1 |
| User PL/pgSQL + SQL functions | 3 |
| User triggers | 3 |
| Partitioned parents | 3 |
| Exclusion constraints | 1 |
| Deferrable FKs (circular pair) | 2 |
| Generated columns | 1 |
