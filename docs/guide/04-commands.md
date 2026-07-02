# Command reference

Every command shares the same exit-code contract: `0` = pass, `1` = verdict fail,
`2` = operational error. Commands that take a config default to `salvage.yaml`.

| Command | Purpose |
|---------|---------|
| [`run`](#run) | Restore into a throwaway target and assert it works. |
| [`check`](#check) | Validate config + preflight Docker (no restore). |
| [`inspect`](#inspect) | Offline pre-flight on an unpacked PGDATA dir. |
| [`scaffold`](#scaffold) | Restore + introspect (postgres/mysql/restic/borg/exec), emit a starter config with auto-generated checks. |
| [`last-good`](#last-good) | Walk the backup chain newest-first (pgBackRest/restic/borg); report the freshest restorable backup. |
| [`fleet`](#fleet) | Survey a repo's units (pgBackRest stanzas; a restic/borg repo is one unit). |
| [`schedule`](#schedule) | Print a systemd timer + cron line to run `salvage attest` on a cadence. |
| [`login`](#login) | Sign in via your browser and store an API key locally. |
| [`logout`](#logout) | Remove the stored API key. |
| [`attest`](#attest) | Run the test, then submit the signed report to the hosted notary. |
| [`verify`](#verify) | Fetch an attestation and verify it offline. |
| [`mcp`](#mcp) | Serve Salvage as an MCP server over stdio, for agent runtimes. |
| [`version`](#version) | Print the version; `-check` also reports whether a newer release exists. |

## Diagnostics: `-verbose` / `-quiet`

`run`, `check`, `scaffold`, `last-good`, `fleet`, and `attest` accept
**`-verbose`** and **`-quiet`**. Both act on **stderr diagnostics only**:

- `-quiet` suppresses everything but errors.
- `-verbose` adds debug-level detail — and on `run`/`attest`, the raw (still
  secret-scrubbed) command output.
- Neither flag changes stdout output, report JSON bytes, or exit codes; when
  both are set, `-quiet` wins.

---

## `run`

Restore the backup into a throwaway environment and run the configured checks,
printing a verdict summary. Writes the JSON report to `report.out` and, when
`report.sign` is set, a signature sidecar.

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Path to the config file. |
| `-json` | off | Write the **full report JSON to stdout** instead of the human summary (spec [0026](../../specs/0026-machine-readable-output.md)). |
| `-show-output` | off | Print the raw (still secret-scrubbed) restore output to **stderr**; it is never serialized into the report (spec [0027](../../specs/0027-report-redaction-secret-hygiene.md)). Also implied by `-verbose`. |
| `-verbose` / `-quiet` | off | See [Diagnostics](#diagnostics--verbose---quiet). |

The report is **redacted by default** (see
[Configuration → Redaction](./02-configuration.md#redaction-and-keep_literal));
`-show-output` is the stderr-only escape hatch for triage. After the report is
written, a configured [`alerts:` hook](./02-configuration.md#alerts) fires —
best-effort, never changing the exit code.

With `-json`, stdout is a single JSON document — the exact bytes written to
`report.out` — carrying a top-level `schema_version` (currently `1`).
`report.out` is still honored when set (`-json` adds a destination, it does not
replace the file), diagnostics go to stderr, and the exit code is unchanged, so
CI can gate on the exit code and pipe stdout straight into a parser:

```sh
salvage run -config salvage.yaml                          # human summary
salvage run -json -config salvage.yaml > report.json      # machine report
salvage run -json -config salvage.yaml | jq -r .verdict   # "pass" / "fail"
```

## `check`

Validate the config and preflight Docker — **no restore happens**. Reports the
target name and how many checks are defined. (For `target.type: exec`, no
container is used, so the Docker preflight is skipped.)

Config parsing is **strict**: an unknown or misspelled YAML key (`expct_min`,
`snapshto`, …) fails `salvage check` with exit `2` and an error naming the key,
rather than being silently ignored.

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Path to the config file. |
| `-verbose` / `-quiet` | off | See [Diagnostics](#diagnostics--verbose---quiet). |

```sh
salvage check -config salvage.yaml
```

## `inspect`

Read an unpacked PGDATA directory **without starting Postgres** and report the PG
major version (from `PG_VERSION`), the `shared_preload_libraries` the cluster
requires, and the number of databases — so you can size the restore image (and
`restore.preload_libraries`) before a full run.

| Flag | Default | Meaning |
|------|---------|---------|
| `-json` | off | Emit machine-readable JSON. |

```sh
salvage inspect -json ./unpacked-pgdata
```

## `scaffold`

Perform a discovery restore, introspect the restored target, and emit a
complete starter config with **auto-generated checks** (thresholds derived from
observed state; structural checks `required`, heuristic checks `advisory`).
Prints to stdout by default. Supported for **postgres, mysql, restic, borg, and
exec** targets (spec [0028](../../specs/0028-cross-engine-scaffold.md)):

- **postgres / mysql** — introspect the restored catalog
  (`information_schema` for MySQL) and emit `sql`-kind checks.
- **restic / borg** — walk the restored tree and emit
  `file_exists`/`file_count` checks.
- **exec** — same tree walk over `restore.workdir` (which must be declared —
  it names the directory your restore command populates); the restore command
  runs first.

MongoDB has no discovery path yet and reports "not supported".

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Config providing the source + restore to introspect. |
| `-o` | stdout | Write the generated config to this file. |
| `-cap` | `50` | Max tables/directories **per family** to generate checks for — the top N by observed size. Structural checks are never capped; when the cap truncates, the emitted config says so and how to widen. |
| `-verbose` / `-quiet` | off | See [Diagnostics](#diagnostics--verbose---quiet). |

```sh
salvage scaffold -config salvage.yaml -o generated.yaml
salvage scaffold -cap 100 -config salvage.yaml    # widen the per-family cap
```

Every generated check is **verified against the restored snapshot** before it
is emitted — a check that would fail on known-good data is dropped.

## `last-good`

For a chain-backed source — **pgBackRest, restic, or borg** (spec
[0029](../../specs/0029-cross-engine-last-good-fleet.md)) — restore-test the
backup chain **newest→oldest** (pgBackRest backups, restic snapshots, borg
archives), stop at the first that passes, and report it as the freshest
restorable recovery point — plus the newer backups that failed, with reasons.
Exit `0` if one is found, `1` if none restore, `2` on operational error.

**Every candidate is a full restore** into a throwaway environment — there is
no cheaper probe that would honestly answer "does it restore?". On a long
restic/borg snapshot history, bound the search with `-max` or the walk can run
for a very long time.

> Honest scope: this finds the freshest *good* backup. It does not repair a
> corrupt backup, reconstruct missing WAL, or recover data that was never
> captured — Salvage is a verifier, not a data-recovery tool.

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Config with the chain-backed source (pgbackrest, restic, or borg) + restore. |
| `-max` | `0` | Max backups to try (`0` = until the first that restores). Cap this on large histories — every try is a full restore. |
| `-json` | off | Emit JSON. |
| `-verbose` / `-quiet` | off | See [Diagnostics](#diagnostics--verbose---quiet). |

```sh
salvage last-good -config salvage.yaml -max 5
```

## `fleet`

For a **pgBackRest, restic, or borg** source (spec
[0029](../../specs/0029-cross-engine-last-good-fleet.md)), survey the repo's
**units** — every stanza of a pgBackRest repo (reads `pgbackrest info`); a
restic or borg repository is **one unit**, reported with its snapshot/archive
count and newest entry. Metadata-only, **no restore**. Optionally emit a
ready-to-fill skeleton config per unit.

Exit codes give cron/CI a real signal: `0` only when **every** unit is
healthy and has at least one backup; `1` when any surveyed unit is degraded
or empty (or the repo has no units at all) — a *result*, like `last-good` —
and `2` on operational error.

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Config providing the repo (source) + restore image. |
| `-o` | — | Write a per-unit skeleton config into this directory. |
| `-json` | off | Emit JSON. |
| `-verbose` / `-quiet` | off | See [Diagnostics](#diagnostics--verbose---quiet). |

```sh
salvage fleet -config salvage.yaml -o ./fleet-configs
```

## `schedule`

Print a ready-to-install **systemd** service + timer and an equivalent **cron**
line that run `salvage attest` on a cadence. It installs nothing — it prints for
review, and notes that the unattended run needs an API key
(`SALVAGE_ATTEST_KEY` or a stored `salvage login` credential). See
[Scheduling & monitoring](./06-scheduling-and-monitoring.md).

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Config to attest on a schedule. |
| `-every` | `1d` | Interval: `1h`, `12h`, `1d`, `7d`, `1w`. |

```sh
salvage schedule -config salvage.yaml -every 7d
```

## `login`

Sign in via the OAuth device flow: Salvage prints a URL + code, opens your
browser, and — once you approve — stores an API key in `~/.salvage/credentials`
that `salvage attest` uses automatically. See [Attestation](./05-attestation.md).

| Flag | Default | Meaning |
|------|---------|---------|
| `-endpoint` | `https://attest.salvage.sh` | Notary base URL. |

```sh
salvage login
# on a headless/SSH box, set SALVAGE_NO_BROWSER=1 and open the printed URL yourself
```

## `logout`

Remove the stored credentials (`~/.salvage/credentials`).

```sh
salvage logout
```

## `attest`

Run the restore-test (or submit an existing report), then submit the signed
report to the hosted notary, which counter-signs it and appends it to your
append-only ledger. Prints the public attestation URL. See
[Attestation](./05-attestation.md).

| Flag | Default | Meaning |
|------|---------|---------|
| `-config` | `salvage.yaml` | Config to run + submit. |
| `-report` | — | Submit an existing report file instead of running the test. |
| `-sig` | — | Signature sidecar for `-report` (optional). |
| `-endpoint` | — | Notary base URL (overrides `attest.endpoint`). |
| `-key-env` | — | Env var holding the API key (overrides `attest.api_key_env`). |
| `-verbose` / `-quiet` | off | See [Diagnostics](#diagnostics--verbose---quiet). |

Endpoint and key resolve in the order flags → config → stored login credentials.
The API key is read from the named environment variable (default
`SALVAGE_ATTEST_KEY`) or from `~/.salvage/credentials` — never from the config
file.

Before anything leaves the machine, the report bytes pass the
**`attest.secret_scan` credential-pattern gate** (default `refuse`: a match
refuses the submission with exit `2`; see
[Configuration → `attest`](./02-configuration.md#attest)). Because `attest`
runs the same test as `run`, a configured
[`alerts:` hook](./02-configuration.md#alerts) fires here too.

```sh
salvage attest -config salvage.yaml
```

## `verify`

Fetch an attestation by id or URL and verify it **offline** against Firerok's
baked-in public key: it re-checks the Firerok signature, the ledger chain hash,
the report hash, and the tenant signature. Exit `0` if genuine, `1` if not.

```sh
salvage verify att_abc123
salvage verify https://attest.salvage.sh/a/att_abc123
salvage verify -json att_abc123 | jq .valid
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-endpoint` | `https://attest.salvage.sh` | Notary base URL (for bare-id lookups). |
| `-json` | off | Emit a machine verdict object instead of the human text (spec [0026](../../specs/0026-machine-readable-output.md)). |

The `-json` object carries `schema_version` (currently `1`), the attestation
`id`, `target`, `verdict`, `seq`, `key_id`, a boolean `valid`, and the
per-check verification transcript. Exit codes are unchanged: an invalid
attestation still exits `1`.

## `mcp`

Serve Salvage as a **Model Context Protocol (MCP) server over stdio** (spec
[0032](../../specs/0032-mcp-server.md)), so an agent runtime (Claude Code,
etc.) can drive the restore/verify/attest loop as structured tools. It takes no
arguments; stdout is the protocol stream and diagnostics go to stderr.

```sh
salvage mcp
```

Eight tools are exposed, each an adapter over the exact code path the CLI
subcommand uses, and each carrying a machine-readable classification a host can
gate on:

| Tool | Wraps | Classification |
|------|-------|----------------|
| `salvage_inspect` | `inspect` | read-only |
| `salvage_fleet` | `fleet` | read-only (never writes skeleton configs via MCP) |
| `salvage_verify` | `verify` | read-only |
| `salvage_run` | `run` | restore-executing |
| `salvage_check` | `check` | restore-executing (Docker preflight only) |
| `salvage_last_good` | `last-good` | restore-executing |
| `salvage_attest` | `attest` | mutating (appends to the ledger) |
| `salvage_scaffold` | `scaffold` | mutating (returns generated YAML as a string; writes nothing to disk) |

*Restore-executing* tools mutate no Salvage state but do run a real restore in
an isolated throwaway environment (for `target.type exec`, the customer's own
restore command). A **bad backup is a successful tool call** whose payload says
verdict `fail`; a tool error means the operation could not run at all — the
exit-code semantics of the CLI, translated to structured results.

**Credentials stay by reference.** No tool accepts a secret as an argument: the
attest API key comes from the environment or `~/.salvage/credentials` (a human
runs `salvage login` first), and tool output passes through the same
known-secret redaction as reports. Unlike the CLI, `salvage_run` writes no
report file — the report JSON *is* the tool result.

## `version`

```sh
salvage version [-check]
```

Prints the running version. Plain `salvage version` performs **no network
I/O**. With `-check` it additionally queries the GitHub releases API for the
latest published release (10-second timeout; the request carries nothing
beyond the version lookup itself — no identifiers, no telemetry), prints both
versions, and exits:

| Exit | Meaning |
|-----:|---------|
| `0` | up to date |
| `1` | a newer release exists (a *result*, not a crash) |
| `2` | the check could not be performed (network/API error, or a dev/off-tag build whose version cannot be compared) |

Salvage **never updates itself**: `-check` reports, the operator (or their
package manager) acts. When a newer release exists, the output includes the
one-line install command. This makes update drift visible on unattended hosts
(e.g. a `salvage schedule` runner) without any auto-update machinery.
