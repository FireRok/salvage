# Configuration reference

A Salvage config is a YAML file describing **one restore-test target**: the
backup to test (`target.source`), the throwaway environment to restore it into
(`target.restore`), and the assertions that prove it works (`target.checks`).
Point any command at it with `-config` (default: `salvage.yaml`).

The top level has four blocks:

```yaml
target:   # what to restore + how + what to assert
report:   # verdict output (JSON, optional local signature)
attest:   # optional — hosted notary submission (see 05-attestation.md)
alerts:   # optional — client-side on_fail/on_success alert hooks (see below)
```

**Parsing is strict.** An unknown or misspelled key (`expct_min`, `snapshto`,
`pass_evn`, …) fails config load with an error naming the key — `salvage check`
exits `2` — rather than being silently dropped. A typo can therefore never
quietly disable a check's expectation or a source field.

## `target`

| Field | Meaning |
|-------|---------|
| `name` | Human label used in reports, e.g. `prod-orders-db`. |
| `type` | The engine: `postgres` (default), `mysql`, `mongodb`, `restic`, `borg`, or `exec`. Selects which source kinds and check kinds are valid. See [Engines](./03-engines.md). |
| `source` | The backup artifact to restore (see below). |
| `restore` | The throwaway environment (see below). |
| `checks` | The assertions that prove the restore is usable (see below). |

If `type` is omitted it defaults to `postgres`.

### `target.source`

The source `kind` depends on the engine:

| `target.type` | `source.kind` | Key fields |
|---------------|---------------|------------|
| `postgres` | `pg_dump` | `path` — a `pg_dump` archive (custom/dir/tar) |
| `postgres` | `sql` | `path` — a plain `.sql` dump |
| `postgres` | `pgbackrest` | `stanza`, plus `repo_volume`/`repo_path` (local) or `pass_env` (remote S3/R2) |
| `mysql` | `mysql` | `path` — a logical `mysqldump` `.sql` dump |
| `mongodb` | `mongodb` | `path` — a `mongodump --archive` file |
| `restic` | `restic` | `snapshot` (default `latest`), `repository` or `RESTIC_REPOSITORY` in `pass_env`; optional `repo_volume`/`repo_path` |
| `borg` | `borg` | `archive` (**required** — borg has no `latest` alias), `repository` or `BORG_REPO` in `pass_env`; `BORG_PASSPHRASE` in `pass_env`; optional `repo_volume`/`repo_path` |

The **`exec`** engine has **no `source`** — the customer's own `restore.command`
performs the restore, so there is nothing for Salvage to point a source kind at.
See [`target.restore` (exec)](#targetrestore-exec).

Common source fields:

- **`path`** — the local dump/archive file (logical Postgres, MySQL, and
  MongoDB sources).
- **`stanza`** — the pgBackRest stanza to restore.
- **`snapshot`** — the restic snapshot to restore (defaults to `latest`).
- **`archive`** — the borg archive to restore (**required**; borg has no
  `latest` alias).
- **`repository`** — a **non-secret** restic/borg repo path/URL, set inline.
- **`repo_volume` / `repo_path`** — mount a local-filesystem repo (pgBackRest,
  restic, or borg) at `repo_path` from a Docker volume `repo_volume`.
- **`pass_env`** — forwards named environment variables from Salvage's own
  process into the restore container **by name**, so secret *values* never appear
  in the config, the image, or any command line. Use it for S3/R2 keys
  (`PGBACKREST_REPO1_S3_KEY[_SECRET]`), restic secrets
  (`RESTIC_PASSWORD`, `RESTIC_REPOSITORY`, `AWS_*`/`B2_*`/`AZURE_*`), and borg
  secrets (`BORG_PASSPHRASE`, `BORG_REPO`).

### `target.restore`

| Field | Meaning |
|-------|---------|
| `image` | The container image. Defaults are **pinned to versions verified end-to-end** (see `dev/<engine>/VERIFIED.md`), so an upstream retag never changes a pinned Salvage release's behavior: logical Postgres `postgres:16`; MySQL `mysql:8.4.10`; MongoDB `mongo:7.0.37`; restic `restic/restic:0.19.0`; borg `ghcr.io/borgmatic-collective/borgmatic:2.1.6` (ships borg 1.4.4 on `PATH`; borg publishes no official image). Override any of them with `restore.image`. **pgBackRest has no default** and must carry both `postgres` and `pgbackrest` (plus any extensions the source uses). |
| `database` | The database checks connect to. Defaults to `salvage_restore_test` (logical Postgres, MySQL, MongoDB) or `postgres` (pgBackRest). |
| `user` | The role checks connect as. Defaults to `postgres` (Postgres) or `root` (MySQL, MongoDB). |
| `preload_libraries` | Seeds `shared_preload_libraries` in a synthesized minimal `postgresql.conf` for clusters that keep config outside PGDATA — required for extensions like `timescaledb`. |
| `timeout` | Bounds the whole restore phase (Go duration, e.g. `30m`). Defaults to `10m`. |

### `target.restore` (exec)

For `target.type: exec` the `restore` block describes the **customer's own
restore command** instead of a container. No `image` is used.

| Field | Meaning |
|-------|---------|
| `command` | **Required.** The argv array Salvage runs to perform the restore (e.g. `["/opt/restore.sh", "--latest"]`). Exit `0` = restore succeeded; a non-zero exit is a **fail** verdict (a missing binary or unusable `workdir` is instead an operational error, exit 2). |
| `env` | Names of host environment variables the command depends on. The command inherits the Salvage process's full environment (so `PATH`/`HOME` resolve); listing the names documents the contract — the *values* come from the host, never the config. |
| `workdir` | The working directory the command (and, later, `command`/file checks) run in. Optional; defaults to the Salvage process's cwd. Relative check paths resolve against it. |
| `timeout` | Bounds the restore command (Go duration). Defaults to `10m`. A **timed-out restore is a fail verdict** (the command launched but did not demonstrably succeed in time), not an operational error. |
| `cleanup` | Optional argv run once on teardown (`Stop`). Its failure is a warning, never a verdict change; it runs even if the restore timed out. |

```yaml
target:
  name: legacy-restore
  type: exec
  restore:
    command: ["/opt/restore.sh", "--latest"]  # required; exit 0 = restored
    env:                                       # host var names the command needs
      - RESTORE_TOKEN
    workdir: /var/lib/legacy-restore
    timeout: 30m
    cleanup: ["/opt/restore.sh", "--teardown"]
  checks:
    - name: service_healthy
      kind: http
      url: http://127.0.0.1:8080/healthz
      expect_status: 200
      expect_body_contains: "ok"
    - name: data_dir_present
      kind: file_exists
      path: data/current
```

## `checks`

Each check is one assertion against the restored target, with a `name`, an
optional `severity`, and — depending on the **kind** — a subject and exactly one
expectation.

### `severity`

- **`required`** (default) — a failure fails the verdict.
- **`advisory`** — a failure is **recorded but does not fail the verdict**.

The verdict is `pass` iff the restore succeeded **and** every *required* check
passed.

### Redaction and `keep_literal`

Reports are **redacted by default** (spec 0027): free-text captured output — the
restore warnings tail and any long or multi-line check `got` value — is stored
as a bounded, scrubbed first-line preview plus a SHA-256 fingerprint, and every
occurrence of a known secret value (anything forwarded via `source.pass_env`,
`restore.env`, or an alert hook's `token_ref` env var) is replaced with a
`[REDACTED:<name>]` marker. Short scalar `got` values — counts, booleans,
digests — are kept as-is. There is no opt-out; to see the raw (still
secret-scrubbed) output on stderr use `run -show-output` or `-verbose`
(see [Commands](./04-commands.md#diagnostics--verbose---quiet)).

A check may set **`keep_literal: true`** to store its exact `got` value in the
report instead of the bounded preview — the explicit opt-in for a `command`/`sql`
check whose byte-equal `equals` assertion needs the full literal recorded. It
requires an `equals` expectation (rejected at load otherwise), and known-secret
scrubbing still applies to the stored value.

### Expectation fields

| Expectation | Asserts |
|-------------|---------|
| `expect_min` / `expect_max` | the numeric result is within range (either or both) |
| `equals` | the result equals a given string |
| `max_age` | the result is a timestamp no older than a Go duration (e.g. `36h`) — available on the `sql` and `doc_query` kinds |
| `bool` | the result is a boolean equal to `true`/`false` |

## Check kinds

A check has a **`kind`** that selects how it is evaluated. The kinds available
today:

| `kind` | Valid for | Subject | Expectation |
|--------|-----------|---------|-------------|
| `sql` (default) | `postgres`, `mysql` | `sql` — one statement returning a single scalar | exactly one of `expect_min`/`expect_max`, `equals`, `max_age`, `bool` |
| `collection_count` | `mongodb` | `collection` + optional `filter` — `countDocuments(filter)` as a scalar | `expect_min`/`expect_max` or `equals` |
| `doc_query` | `mongodb` | `collection` + `filter` + `field` — one field of `findOne(filter)` as a scalar | `equals`, `expect_min`/`expect_max`, or `max_age` |
| `file_exists` | `restic`, `borg`, `exec` | `path` | `bool` (default `true`) — presence matches |
| `file_count` | `restic`, `borg`, `exec` | `path` (a glob) | `expect_min` and/or `expect_max` |
| `checksum` | `restic`, `borg`, `exec` | `path` | `equals` — expected hex sha256 |
| `command` | `restic`, `borg`, `exec` | `command` | passes on exit 0; `equals` (optional) matches stdout |
| `http` | `restic`, `borg`, `exec` | `url` | `expect_status` (default `200`), `expect_body_contains`, `expect_json` |

If `kind` is omitted it defaults to `sql`, so existing Postgres configs are
unchanged. A `sql` check must set exactly one expectation; the other kinds each
require the fields they consume (a `checksum` without `equals`, a `file_count`
without bounds, a `doc_query` without a `filter`, or an `http` without `url`,
fails at load time rather than at runtime).

The `file_exists`, `file_count`, `checksum`, `command`, and `http` kinds are
shared between the `restic`, `borg`, and `exec` engines. Under `restic` and
`borg` the file/command kinds probe the restored tree inside the container;
under `exec` they run **on the host** (relative `path`s and the `command`
working directory resolve against `restore.workdir`). The `http` kind always
probes from the Salvage host.

### MongoDB check kinds

`kind: collection_count` and `kind: doc_query` (MongoDB-only) are the
document-store analogues of a SQL scalar check:

| Field | Used by | Meaning |
|-------|---------|---------|
| `collection` | both | **Required.** The collection to query. |
| `filter` | both | A JSON filter document, e.g. `'{"status":"shipped"}'`. Optional for `collection_count` (empty counts every document); **required** for `doc_query`. |
| `field` | `doc_query` | **Required.** The dotted field path read from the matched document as a scalar. |

`collection_count` asserts the count with `expect_min`/`expect_max` or
`equals`. `doc_query` asserts the field's value with `equals`,
`expect_min`/`expect_max`, or **`max_age`** — freshness against a timestamp
field (BSON dates are compared in ISO-8601 form), so "is the newest data
recent?" is expressible for MongoDB just as it is for SQL engines. `doc_query`
fails if no document matches the filter or the field is absent on the matched
document.

```yaml
- name: shipped_orders_present
  kind: collection_count
  collection: orders
  filter: '{"status":"shipped"}'
  expect_min: 1

- name: latest_order_is_recent
  kind: doc_query
  collection: orders
  filter: '{"_id":"latest"}'
  field: created_at
  max_age: 36h
```

See [`salvage.mongodb.example.yaml`](../../salvage.mongodb.example.yaml) for a
complete worked config.

### The `http` check kind

`kind: http` (valid for `restic`, `borg`, and `exec` targets) probes a restored
service over HTTP from the Salvage host:

| Field | Meaning |
|-------|---------|
| `url` | **Required.** The request target. |
| `method` | HTTP method. Defaults to `GET`. |
| `headers` | Optional request headers (a map of name → value). |
| `body` | Optional request body. |
| `expect_status` | Expected HTTP status code. Defaults to `200`. |
| `expect_body_contains` | Asserts the response body contains this substring. |
| `expect_json` | A single `dotted.path=value` assertion over a JSON response body. |

```yaml
- name: api_up
  kind: http
  url: http://127.0.0.1:8080/api/health
  expect_status: 200
  expect_json: "status=ready"
```

### Worked example — Postgres (`sql` kind)

From [`salvage.example.yaml`](../../salvage.example.yaml):

```yaml
target:
  name: prod-orders-db
  type: postgres
  source:
    kind: pg_dump            # pg_dump (custom/dir/tar) | sql (plain)
    path: ./backups/latest.dump
  restore:
    image: postgres:16
    database: salvage_restore_test
    timeout: 10m
  checks:
    - name: schema_present
      sql: "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'"
      expect_min: 1

    - name: orders_not_empty
      sql: "SELECT count(*) FROM orders"
      expect_min: 1

    - name: latest_order_is_recent
      sql: "SELECT max(created_at) FROM orders"
      max_age: 36h           # stale/broken if the newest row is too old

    - name: schema_version
      sql: "SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1"
      equals: "20260615000000"

report:
  format: json
  out: ./salvage-report.json
  sign: true                 # local ed25519 signature (integrity, not independence)
  key_path: ./keys/salvage.key
```

### Worked example — restic (filesystem kinds)

From [`salvage.restic.example.yaml`](../../salvage.restic.example.yaml):

```yaml
target:
  name: demo-restic
  type: restic
  source:
    kind: restic
    snapshot: latest
    repository: /repo                 # non-secret path; mounted from repo_volume
    repo_volume: salvage-restic-repo  # docker volume holding the restic repo
    pass_env:
      - RESTIC_PASSWORD               # forwarded by name — value never in this file
  restore:
    image: restic/restic:0.19.0
    timeout: 10m
  checks:
    - name: config_present
      kind: file_exists
      path: seed/etc/app.conf
    - name: data_files_present
      kind: file_count
      path: "seed/data/*.csv"
      expect_min: 3
      expect_max: 3
    - name: seed_checksum
      kind: checksum
      path: seed/data/seed.csv
      equals: "17f559a3c5d175f059adc34bd95ae76c63dc328ab3a15542ccf46b8c5c765cf9"
    - name: config_readable
      kind: command
      command: "grep -q salvage-demo seed/etc/app.conf"

report:
  format: json
  out: ./salvage-report-restic.json
  sign: false
```

The borg equivalent —
[`salvage.borg.example.yaml`](../../salvage.borg.example.yaml) — uses the same
four check kinds; the MySQL config
([`salvage.mysql.example.yaml`](../../salvage.mysql.example.yaml)) reuses the
Postgres `sql` shape unchanged; and
[`salvage.mongodb.example.yaml`](../../salvage.mongodb.example.yaml) shows the
MongoDB kinds.

## `report`

| Field | Meaning |
|-------|---------|
| `format` | Only `json` is supported today. |
| `out` | Where to write the JSON verdict. Every report carries a top-level `schema_version` (currently `1`); `salvage run -json` emits the same bytes to stdout (see [Commands → `run`](./04-commands.md#run)). |
| `sign` | When `true`, writes a local ed25519 signature sidecar (`<out>.sig`) — proves integrity, not independence. |
| `key_path` | The ed25519 signing key used when `sign` is `true`. |

Report bytes are redacted by default on every output path — the `report.out`
file, `run -json` stdout, and the bytes submitted for attestation. See
[Redaction and `keep_literal`](#redaction-and-keep_literal).

## `attest`

Optional; configures submission to the hosted notary. See
[Attestation](./05-attestation.md).

```yaml
attest:
  endpoint: https://attest.salvage.sh
  api_key_env: SALVAGE_ATTEST_KEY   # names the env var holding the key (never the value)
  secret_scan: refuse               # refuse (default) | warn | off
```

**`secret_scan`** controls the pre-submission credential-pattern gate (spec
0027): before anything reaches the notary, the final report bytes are scanned
for common credential shapes (AWS access-key ids, PEM private keys, bearer
tokens, URL-embedded `user:pass@`).

- `refuse` (the default, also when the key is absent) — a match refuses the
  submission (exit `2`).
- `warn` — a match prints a loud stderr warning and proceeds.
- `off` — the gate is disabled.

This is a backstop for secrets outside the known-value scrub (e.g. a credential
the restore script fetched itself); the default is the safe one because a
secret counter-signed into the ledger cannot be unpublished.

## `alerts`

Optional client-side alert hooks (spec 0030): after a run's verdict is
finalized and its report is written, `salvage run` and `salvage attest` invoke
the configured hook with the run's report JSON — `on_fail` on a **fail verdict
or an operational error**, `on_success` on a pass. At least one of the two is
required when the block is present.

```yaml
alerts:
  on_fail: 'jq -r .verdict | mail -s "salvage FAIL" oncall@example.com'
  on_success: "https://alerts.example.com/salvage?token_ref=env:SALVAGE_HOOK_TOKEN"
  timeout: 30s        # bounds each hook invocation (default 30s)
```

A hook value is either:

- **a command line** — run via `sh -c` with the exact redacted report JSON on
  **stdin** and, when a report file was written, its path in
  **`$SALVAGE_REPORT`**; or
- **an `http(s)://` URL** — the report JSON is **POSTed** as
  `application/json`.

Hooks are **best-effort and secondary**: a hook that errors or times out is
logged to stderr and never changes the run's exit code, and no daemon is
involved — the hook runs inline in the one-shot process. The exit code remains
the primary signal.

**Tokens are passed by reference, never embedded.** A URL hook must not embed
credentials (`user:pass@` is rejected at load); instead a `*_ref=env:NAME`
query parameter (e.g. `token_ref=env:SALVAGE_HOOK_TOKEN`) is resolved from the
environment only at delivery time into `token=<value>`, so no secret is ever
written into the config — the same by-reference posture as `source.pass_env`.
The referenced env vars are also folded into the report's known-secret scrub
set, so a hook token can never surface in a report.

For the **hosted** counterpart — webhook/Slack/PagerDuty destinations on the
dead-man's-switch — see
[Scheduling & monitoring](./06-scheduling-and-monitoring.md#hosted-alert-destinations).
