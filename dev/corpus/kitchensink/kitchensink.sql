-- =====================================================================
-- Salvage test corpus: "kitchen-sink" Postgres schema
-- =====================================================================
-- Purpose: deliberately exercise the *restore* edge cases Salvage must
-- survive when a backup is dumped (pg_dump) and restored (pg_restore /
-- psql) into a fresh cluster, then inspected/scaffolded/checked.
--
-- Targets stock `postgres:16` only -- stock contrib extensions, no
-- timescaledb / postgis. Load with:
--   psql -v ON_ERROR_STOP=1 -f kitchensink.sql
-- =====================================================================

\set ON_ERROR_STOP on

-- ---------------------------------------------------------------------
-- Extensions (all ship in the stock postgres:16 image)
-- ---------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS hstore;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ---------------------------------------------------------------------
-- Schemas
-- ---------------------------------------------------------------------
CREATE SCHEMA IF NOT EXISTS inventory;
CREATE SCHEMA IF NOT EXISTS analytics;

-- =====================================================================
-- User-defined types (enum, composite, domain)
-- =====================================================================
CREATE TYPE public.order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');

CREATE TYPE public.us_address AS (
    line1   text,
    city    text,
    state   char(2),
    zip     text
);

-- Domain with a CHECK constraint (must round-trip through restore)
CREATE DOMAIN public.positive_money AS numeric(12,2)
    CHECK (VALUE >= 0);

CREATE DOMAIN public.email_addr AS citext
    CHECK (VALUE ~ '^[^@\s]+@[^@\s]+\.[^@\s]+$');

-- =====================================================================
-- Sequences (one explicitly used outside SERIAL machinery)
-- =====================================================================
CREATE SEQUENCE public.invoice_number_seq
    START WITH 1000
    INCREMENT BY 1
    NO MAXVALUE
    CACHE 1;

-- =====================================================================
-- PL/pgSQL functions
-- =====================================================================

-- updated_at touch trigger function
CREATE OR REPLACE FUNCTION public.touch_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- assigns a human-readable invoice number from the explicit sequence
CREATE OR REPLACE FUNCTION public.assign_invoice_number()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.invoice_no IS NULL THEN
        NEW.invoice_no := 'INV-' || nextval('public.invoice_number_seq')::text;
    END IF;
    RETURN NEW;
END;
$$;

-- plain helper used by a view
CREATE OR REPLACE FUNCTION public.full_name(first text, last text)
RETURNS text
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT btrim(coalesce(first,'') || ' ' || coalesce(last,''));
$$;

-- =====================================================================
-- public schema core tables
-- =====================================================================

-- Wide "everything" table: a broad spread of types.
CREATE TABLE public.wide_types (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- integers
    c_smallint      smallint,
    c_int           integer NOT NULL DEFAULT 0,
    c_bigint        bigint,
    -- exact / approximate numerics
    c_numeric       numeric(20,6),
    c_real          real,
    c_double        double precision,
    c_money         money,
    -- text family
    c_text          text,
    c_varchar       varchar(64),
    c_char          char(8),
    c_citext        citext,
    -- boolean / binary
    c_bool          boolean,
    c_bytea         bytea,
    -- json
    c_json          json,
    c_jsonb         jsonb,
    -- arrays (incl. multi-dimensional)
    c_int_array     integer[],
    c_text_array    text[],
    c_matrix        integer[][],
    -- uuid with stock default
    c_uuid          uuid NOT NULL DEFAULT gen_random_uuid(),
    -- date/time family
    c_date          date,
    c_time          time,
    c_timestamp     timestamp,
    c_timestamptz   timestamptz,
    c_interval      interval,
    -- ranges
    c_int4range     int4range,
    c_tstzrange     tstzrange,
    -- network
    c_inet          inet,
    c_cidr          cidr,
    c_macaddr       macaddr,
    -- bit strings
    c_bit           bit(8),
    c_varbit        bit varying(16),
    -- full text
    c_tsvector      tsvector,
    -- key/value + composite + domain + enum
    c_hstore        hstore,
    c_address       public.us_address,
    c_price         public.positive_money,
    c_status        public.order_status,
    -- generated column (STORED)
    c_int_doubled   integer GENERATED ALWAYS AS (c_int * 2) STORED,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER trg_wide_types_touch
    BEFORE UPDATE ON public.wide_types
    FOR EACH ROW EXECUTE FUNCTION public.touch_updated_at();

-- Trigram index for fuzzy text search (pg_trgm) + a tsvector GIN index.
CREATE INDEX idx_wide_text_trgm ON public.wide_types USING gin (c_text gin_trgm_ops);
CREATE INDEX idx_wide_tsv       ON public.wide_types USING gin (c_tsvector);

-- Customers: citext + hstore + domain email + array, has updated_at trigger.
CREATE TABLE public.customers (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    username        citext NOT NULL UNIQUE,
    email           public.email_addr NOT NULL,
    display_name    text,
    tags            text[] DEFAULT '{}',
    attributes      hstore,
    password_hash   text NOT NULL DEFAULT crypt('changeit', gen_salt('bf')),
    balance         public.positive_money NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT customers_username_len CHECK (length(username::text) >= 2)
);

CREATE TRIGGER trg_customers_touch
    BEFORE UPDATE ON public.customers
    FOR EACH ROW EXECUTE FUNCTION public.touch_updated_at();

-- Invoices: explicit sequence via trigger, FK to customers, enum status,
-- and a fresh timestamptz for freshness checks (seeded at load time).
CREATE TABLE public.invoices (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invoice_no      text UNIQUE,
    customer_id     uuid NOT NULL REFERENCES public.customers(id) ON DELETE CASCADE,
    status          public.order_status NOT NULL DEFAULT 'pending',
    amount          public.positive_money NOT NULL,
    issued_at       timestamptz NOT NULL DEFAULT now(),
    valid_during    tstzrange,
    notes           text
);

CREATE TRIGGER trg_invoices_number
    BEFORE INSERT ON public.invoices
    FOR EACH ROW EXECUTE FUNCTION public.assign_invoice_number();

-- ---------------------------------------------------------------------
-- Circular FK pair, DEFERRABLE INITIALLY DEFERRED
-- ---------------------------------------------------------------------
CREATE TABLE public.departments (
    id          integer PRIMARY KEY,
    name        text NOT NULL,
    lead_id     integer  -- FK to employees added below (circular)
);

CREATE TABLE public.employees (
    id          integer PRIMARY KEY,
    name        text NOT NULL,
    dept_id     integer NOT NULL
);

ALTER TABLE public.departments
    ADD CONSTRAINT departments_lead_fk
    FOREIGN KEY (lead_id) REFERENCES public.employees(id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE public.employees
    ADD CONSTRAINT employees_dept_fk
    FOREIGN KEY (dept_id) REFERENCES public.departments(id)
    DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------
-- Exclusion constraint (no-overlap bookings) via btree_gist
-- ---------------------------------------------------------------------
CREATE TABLE public.room_bookings (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_id     integer NOT NULL,
    during      tstzrange NOT NULL,
    EXCLUDE USING gist (room_id WITH =, during WITH &&)
);

-- =====================================================================
-- inventory schema: partitioning + inheritance
-- =====================================================================

-- RANGE-partitioned table by month, with a few partitions + data.
CREATE TABLE inventory.events (
    id          bigint GENERATED ALWAYS AS IDENTITY,
    occurred_at timestamptz NOT NULL,
    kind        text NOT NULL,
    payload     jsonb,
    PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);

CREATE TABLE inventory.events_2026_05 PARTITION OF inventory.events
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');
CREATE TABLE inventory.events_2026_06 PARTITION OF inventory.events
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');
CREATE TABLE inventory.events_default PARTITION OF inventory.events DEFAULT;

-- HASH-partitioned table by id.
CREATE TABLE inventory.shards (
    id      integer NOT NULL,
    label   text NOT NULL,
    PRIMARY KEY (id)
) PARTITION BY HASH (id);

CREATE TABLE inventory.shards_h0 PARTITION OF inventory.shards
    FOR VALUES WITH (MODULUS 3, REMAINDER 0);
CREATE TABLE inventory.shards_h1 PARTITION OF inventory.shards
    FOR VALUES WITH (MODULUS 3, REMAINDER 1);
CREATE TABLE inventory.shards_h2 PARTITION OF inventory.shards
    FOR VALUES WITH (MODULUS 3, REMAINDER 2);

-- LIST-partitioned table by region.
CREATE TABLE inventory.regional_stock (
    sku         text NOT NULL,
    region      text NOT NULL,
    qty         integer NOT NULL DEFAULT 0,
    PRIMARY KEY (sku, region)
) PARTITION BY LIST (region);

CREATE TABLE inventory.regional_stock_us PARTITION OF inventory.regional_stock
    FOR VALUES IN ('us-east', 'us-west');
CREATE TABLE inventory.regional_stock_eu PARTITION OF inventory.regional_stock
    FOR VALUES IN ('eu-central', 'eu-west');
CREATE TABLE inventory.regional_stock_other PARTITION OF inventory.regional_stock DEFAULT;

-- ---------------------------------------------------------------------
-- Inheritance: parent + inheriting child (legacy, non-partition form)
-- ---------------------------------------------------------------------
CREATE TABLE inventory.assets (
    id          integer PRIMARY KEY,
    name        text NOT NULL,
    acquired_on date
);

CREATE TABLE inventory.vehicles (
    vin             text,
    wheels          smallint NOT NULL DEFAULT 4
) INHERITS (inventory.assets);

-- =====================================================================
-- analytics schema: views + materialized view
-- =====================================================================

-- Plain view joining customers + invoices, exercising the helper fn.
CREATE VIEW analytics.customer_invoices AS
    SELECT  c.id              AS customer_id,
            c.username,
            c.email::text     AS email,
            i.invoice_no,
            i.status,
            i.amount,
            i.issued_at
    FROM public.customers c
    JOIN public.invoices  i ON i.customer_id = c.id;

-- View over the wide table (also exercises generated column).
CREATE VIEW analytics.wide_summary AS
    SELECT  c_status,
            count(*)              AS n,
            sum(c_int)            AS total_int,
            sum(c_int_doubled)    AS total_doubled
    FROM public.wide_types
    GROUP BY c_status;

-- Materialized view, populated, with a unique index on it.
CREATE MATERIALIZED VIEW analytics.revenue_by_status AS
    SELECT  status,
            count(*)         AS invoice_count,
            sum(amount)::numeric(14,2) AS total_amount
    FROM public.invoices
    GROUP BY status
    WITH DATA;

CREATE UNIQUE INDEX idx_revenue_by_status ON analytics.revenue_by_status (status);

-- =====================================================================
-- Edge-case tables
-- =====================================================================

-- Empty table (created, never populated).
CREATE TABLE public.empty_table (
    id      integer PRIMARY KEY,
    note    text
);

-- Table with NO primary key.
CREATE TABLE public.no_pk_log (
    at      timestamptz NOT NULL DEFAULT now(),
    level   text,
    message text
);

-- Weird quoted identifiers: table name with a space + reserved-word column.
CREATE TABLE public."Weird Name" (
    id              integer PRIMARY KEY,
    "select"        text,
    "Mixed Case"    integer,
    "from"          text
);

-- Table that intentionally carries NULLs in several columns.
CREATE TABLE public.nullable_bits (
    id          integer PRIMARY KEY,
    maybe_text  text,
    maybe_num   numeric,
    maybe_ts    timestamptz
);

-- Table with a recent timestamptz target for freshness checks.
CREATE TABLE public.heartbeats (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source      text NOT NULL,
    beat_at     timestamptz NOT NULL DEFAULT now()
);

-- =====================================================================
-- Seed data
-- =====================================================================

-- wide_types: one fully-populated row + one sparse row (NULLs).
INSERT INTO public.wide_types (
    c_smallint, c_int, c_bigint, c_numeric, c_real, c_double, c_money,
    c_text, c_varchar, c_char, c_citext, c_bool, c_bytea,
    c_json, c_jsonb, c_int_array, c_text_array, c_matrix,
    c_date, c_time, c_timestamp, c_timestamptz, c_interval,
    c_int4range, c_tstzrange, c_inet, c_cidr, c_macaddr,
    c_bit, c_varbit, c_tsvector, c_hstore, c_address, c_price, c_status
) VALUES (
    32000, 21, 9000000000, 12345.678901, 1.5, 2.718281828, 19.99::money,
    'the quick brown fox jumps', 'varchar value', 'fixedchr', 'CaseInsensitive', true, '\xdeadbeef'::bytea,
    '{"k":"v","n":1}'::json, '{"nested":{"a":[1,2,3]}}'::jsonb,
    ARRAY[1,2,3], ARRAY['a','b','c'], ARRAY[ARRAY[1,2],ARRAY[3,4]],
    DATE '2026-06-15', TIME '13:45:00', TIMESTAMP '2026-06-15 13:45:00',
    now(), INTERVAL '3 days 4 hours',
    int4range(1,10), tstzrange(now() - interval '1 day', now()),
    '192.168.1.10'::inet, '10.0.0.0/24'::cidr, '08:00:2b:01:02:03'::macaddr,
    B'10101010', B'1100', to_tsvector('english','the quick brown fox'),
    'color=>blue, size=>large'::hstore,
    ROW('123 Main St','Portland','OR','97201')::public.us_address,
    49.95, 'paid'
);

INSERT INTO public.wide_types (c_int, c_text, c_status)
VALUES (7, 'sparse row, mostly nulls', 'pending');

-- customers
INSERT INTO public.customers (username, email, display_name, tags, attributes) VALUES
    ('alice',  'Alice@Example.com', 'Alice Anderson', ARRAY['vip','beta'], 'tier=>gold'::hstore),
    ('bob',    'bob@example.org',   'Bob Brown',      ARRAY['beta'],       'tier=>silver'::hstore),
    ('carol',  'carol@example.net', 'Carol Clark',    '{}',                NULL);

-- invoices (invoice_no assigned by trigger from the explicit sequence)
INSERT INTO public.invoices (customer_id, status, amount, valid_during, notes)
SELECT id, 'paid', 120.00, tstzrange(now() - interval '7 days', now() + interval '23 days'), 'first invoice'
FROM public.customers WHERE username = 'alice';
INSERT INTO public.invoices (customer_id, status, amount, notes)
SELECT id, 'pending', 75.50, 'open balance'
FROM public.customers WHERE username = 'bob';
INSERT INTO public.invoices (customer_id, status, amount, notes)
SELECT id, 'shipped', 333.33, NULL
FROM public.customers WHERE username = 'alice';

-- departments + employees (circular FK, deferred within one txn)
BEGIN;
INSERT INTO public.departments (id, name, lead_id) VALUES (1, 'Engineering', NULL), (2, 'Sales', NULL);
INSERT INTO public.employees (id, name, dept_id) VALUES
    (10, 'Dana', 1), (11, 'Evan', 1), (12, 'Faye', 2);
UPDATE public.departments SET lead_id = 10 WHERE id = 1;
UPDATE public.departments SET lead_id = 12 WHERE id = 2;
COMMIT;

-- room_bookings (non-overlapping, satisfy exclusion constraint)
INSERT INTO public.room_bookings (room_id, during) VALUES
    (1, tstzrange('2026-07-01 09:00+00','2026-07-01 10:00+00')),
    (1, tstzrange('2026-07-01 10:00+00','2026-07-01 11:00+00')),
    (2, tstzrange('2026-07-01 09:00+00','2026-07-01 12:00+00'));

-- inventory.events: rows landing in each range partition + default
INSERT INTO inventory.events (occurred_at, kind, payload) VALUES
    ('2026-05-10 12:00+00', 'login',  '{"ip":"1.2.3.4"}'),
    ('2026-06-10 12:00+00', 'logout', '{"ip":"1.2.3.4"}'),
    (now(),                 'beat',   '{"ok":true}'),
    ('2030-01-01 00:00+00', 'future', NULL);

-- inventory.shards: spread across hash partitions
INSERT INTO inventory.shards (id, label)
SELECT g, 'shard-' || g FROM generate_series(1, 9) g;

-- inventory.regional_stock: hits each list partition + default
INSERT INTO inventory.regional_stock (sku, region, qty) VALUES
    ('SKU1', 'us-east', 100),
    ('SKU1', 'eu-west', 50),
    ('SKU2', 'ap-south', 25);

-- inventory.assets + vehicles (inheritance)
INSERT INTO inventory.assets (id, name, acquired_on) VALUES (1, 'Office printer', '2025-01-15');
INSERT INTO inventory.vehicles (id, name, acquired_on, vin, wheels)
    VALUES (2, 'Delivery van', '2024-09-01', '1HGCM82633A004352', 4);

-- nullable_bits: rows with NULLs
INSERT INTO public.nullable_bits (id, maybe_text, maybe_num, maybe_ts) VALUES
    (1, 'has text', 3.14, now()),
    (2, NULL, NULL, NULL),
    (3, 'partial', NULL, now());

-- weird quoted-identifier table
INSERT INTO public."Weird Name" (id, "select", "Mixed Case", "from") VALUES
    (1, 'reserved-word column value', 42, 'origin'),
    (2, NULL, NULL, NULL);

-- no-PK log
INSERT INTO public.no_pk_log (level, message) VALUES
    ('info', 'started'),
    ('warn', 'something odd'),
    ('info', 'still running');

-- heartbeats: recent timestamptz for freshness checks
INSERT INTO public.heartbeats (source, beat_at) VALUES
    ('cron',   now()),
    ('worker', now() - interval '30 seconds'),
    ('api',    now() - interval '5 minutes');

-- Refresh the materialized view now that invoices exist.
REFRESH MATERIALIZED VIEW analytics.revenue_by_status;

-- Bump the explicit sequence once more so its restored value is non-trivial.
SELECT setval('public.invoice_number_seq', nextval('public.invoice_number_seq'));

-- =====================================================================
-- Sanity: leave a comment so `salvage inspect` sees object comments too.
-- =====================================================================
COMMENT ON SCHEMA inventory IS 'Partitioning + inheritance edge cases';
COMMENT ON TABLE public.wide_types IS 'Broad spread of Postgres types';
COMMENT ON COLUMN public.wide_types.c_int_doubled IS 'GENERATED ALWAYS AS STORED';

ANALYZE;
