# 0018 — The restic engine (the first non-SQL engine)

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

[[spec 0016]] built the vertical seam — the engine SPI keyed by `target.type` —
and [[spec 0017]] built the horizontal one: a check `kind` + engine-provided
evaluators, so validation can generalize past SQL while the report, verdict,
attestation, and dead-man's-switch stay untouched. Both specs named the
**restic/borg filesystem engine as the one that comes next** (0017 R5, positioning
§8). This spec is that engine.

restic is the first *non-SQL* engine, and the first to exercise both seams end to
end at once: it restores a restic filesystem snapshot into a throwaway container
and validates it with **file/command probes instead of SQL**, inheriting the
report + attestation + monitoring layers unchanged. It is the proof that Salvage
is a modular verification-and-attestation platform, not a Postgres tool — "prove
and attest *any* backup," one engine at a time.

## Goals

- A registered `spi.Engine` for `target.type: "restic"` that restores a snapshot
  in Docker (no host `restic` required) and returns a filesystem-probeable target.
- Four non-SQL check kinds — `file_exists`, `file_count`, `checksum`, `command` —
  registered as evaluators against that target, with the verdict/report/ledger
  path completely unchanged.
- Credentials strictly by reference (spec 0003) and two-phase network isolation
  (spec 0003 R2), mirroring the pgBackRest path.
- Adding this engine touches only new files plus the config allow-list (0016 R6)
  and one blank import — no change to `internal/engine` orchestration or the CLI.

## Non-goals

- borg (a sibling engine later; the same seams apply).
- `scaffold` for restic — it needs a restic-specific discovery path (0017 R4);
  until then `scaffold` cleanly reports "not supported for target.type restic".
- A restic `last-good`/`fleet` — the orchestrator's chain/fleet commands are
  pgBackRest-scoped today; restic simply does not implement the optional
  capabilities, so both commands cleanly report "not supported" (see Open
  questions).

## Design

### The engine (`internal/engine/restic`)

`Engine{}` implements `spi.Engine` for `Type() == "restic"` and `Register`s itself
plus its four check kinds in `init()`. `Restore`:

1. Enforces the `pass_env` precondition (every named var must be set) — a missing
   secret is a `spi.Fault` (operational, exit 2, no verdict), exactly like the
   pgBackRest path.
2. `docker run -d --rm … --entrypoint sh restic/restic:<tag> -c "sleep infinity"`
   — an idle container we `docker exec` into. The repo volume (local repo) mounts
   where the repository points; a remote repo relies on forwarded backend vars.
3. `restic restore <snapshot|latest> --target /restore` inside the container. A
   restore failure is a **bare error** (a "fail" verdict), not a Fault.
4. **Two-phase network isolation** (spec 0003 R2): the restore fetch needs egress
   (a remote repo is downloaded), so the container stays connected through the
   restore; immediately after, `docker network disconnect` drops it off every
   network *before any check runs*, so a restored `command` check cannot reach
   out. Isolation failure aborts rather than checking a connected container.

The returned `*ephemeral.Restic` is the live `RestoredTarget`; `Stop()`
`docker kill`s it and is idempotent.

### The prober

The restic target exposes a `restic.FileProber` instead of a SQL `Queryer`:

```go
type FileProber interface {
    Exists(ctx, path string) (bool, error)
    Count(ctx, pattern string) (int, error)   // find <root> -path pattern | wc -l
    Sha256(ctx, path string) (string, error)
    RunCommand(ctx, cmd string) (out string, exit int, err error)
}
```

`*ephemeral.Restic` implements it via `docker exec` against `/restore`
(`test -e`, `find … | wc -l`, `sha256sum`, `sh -c '<cmd>'`). Paths in a config are
relative to the restored tree. `RunCommand` returns a non-zero exit in `exit`
(not `err`) so a check distinguishes "command ran and failed" (a verdict) from
"could not run it" (operational). It does **not** implement `discover.RowQueryer`,
so `scaffold` is gated off via the existing "not supported" path — no core change.

### The check kinds (evaluators, spec 0017 R3)

Each evaluator type-asserts the opaque `checks.Target` to `FileProber`; a target
that cannot probe files (e.g. a SQL engine's target reaching a restic check)
yields a clear failing result, never a panic. A result is the same generic
`{name, ok, severity, got, detail, error}` as every other kind:

| kind | subject | expectation | passes iff |
|---|---|---|---|
| `file_exists` | `path` | `bool` (default true) | presence matches the bool |
| `file_count` | `path` (glob) | `expect_min`/`expect_max` | count within bounds |
| `checksum` | `path` | `equals` (hex sha256) | sha256(path) == equals |
| `command` | `command` | `equals` (optional) | exit 0, or stdout == equals |

### Config (`internal/config`)

- `Source` gains `Snapshot` (default `"latest"`) and `Repository` (a non-secret
  path/URL set as `RESTIC_REPOSITORY`; a *secret* repo is instead forwarded by
  name via `pass_env`). `RepoVolume`/`RepoPath` (already present) mount a local
  repo; `RepoPath` defaults to `Repository` for the local case.
- `Check` gains `Path` and `Command` (both `yaml:",omitempty"` so SQL configs are
  byte-identical). The sql kind ignores them; file kinds use `Path`, the command
  kind uses `Command`; `ExpectMin/Max`, `Equals`, and `Bool` are reused per the
  table above.
- `applyDefaults` defaults the restic image (`restic/restic:latest`), snapshot,
  and local-repo mount path.
- `Validate` is the **allow-list step (0016 R6)**: `target.type` now accepts
  `postgres` **or** `restic`, each validating its own source shape and check
  kinds. restic requires a repository (inline or `RESTIC_REPOSITORY` by
  reference) and validates each non-SQL kind's required fields. Postgres
  validation and every Postgres error message are unchanged; restic check kinds
  are rejected on a postgres target and vice-versa.

### Credentials & isolation

- **By reference only (spec 0003):** `RESTIC_PASSWORD` (or
  `RESTIC_PASSWORD_FILE`/`RESTIC_PASSWORD_COMMAND`) and any backend vars
  (`AWS_*`, `B2_*`, `AZURE_*`, …) are named in `source.pass_env` and forwarded
  with `docker run -e NAME` — the value never appears in a command argument or in
  the config file. The repository, when it is a plain path/URL, may be set inline
  (`source.repository`); when it is itself a secret, forward `RESTIC_REPOSITORY`
  by name instead.
- **Isolation (spec 0003 R2):** as above — connected for the fetch, disconnected
  from every network before any check.

## Requirements

**R1 — Registered restic engine.** There MUST be an `spi.Engine` for
`target.type: "restic"`, registered in its package `init()` and wired via a blank
import in `internal/engine/engine.go`. It MUST restore in Docker with no host
`restic` binary.

**R2 — Filesystem-probeable target.** `Restore` MUST return a `RestoredTarget`
exposing a `FileProber` (exists/count/sha256/command over `docker exec`) with an
idempotent `Stop()`. It MUST NOT implement `discover.RowQueryer` (so `scaffold`
gates off cleanly).

**R3 — Non-SQL check kinds.** `file_exists`, `file_count`, `checksum`, and
`command` MUST be registered as evaluators, each type-asserting the target to
`FileProber` and returning a clear failing `CheckResult` (never a panic) on a bad
target or a lacking capability.

**R4 — Operational-vs-verdict split.** A missing `pass_env` secret or a
Docker/container problem MUST be a `spi.Fault` (exit 2, no verdict); a snapshot
that fails to restore MUST be a bare error (a "fail" verdict). Inherited unchanged
from 0016 R4.

**R5 — Credentials by reference + isolation.** Secrets MUST be forwarded by name
only (spec 0003); the container MUST be disconnected from every Docker network
after the restore and before any check (spec 0003 R2).

**R6 — Config allow-list.** `config.Validate` MUST accept `target.type: "restic"`
and validate its source shape (a repository, inline or by reference) and its
check kinds' required fields, with all Postgres validation and messages unchanged
(0016 R6).

**R7 — Inherited platform, no new deps.** The report, verdict, signing/ledger/
verify, and dead-man's-switch MUST be inherited unchanged (0017 R1–R2, R5). No new
Go dependency (stdlib + `gopkg.in/yaml.v3`); the `restic/restic` Docker image is
runtime, not a Go dep.

## Open questions

- **restic `last-good`/`fleet`.** restic snapshots form a history, so a
  `ChainTester` is conceivable. But `internal/engine`'s `LastGood`/`Fleet` gate on
  `source.kind == "pgbackrest"` after the capability assert — a restic
  implementation would require loosening that orchestrator gate, which 0016 keeps
  deliberately thin. Deferred: restic omits both capabilities, so both commands
  cleanly report "not supported for target.type restic" (the correct behaviour per
  0016 R5) with zero orchestrator change. Lighting them up is a small, safe
  follow-up once the gate is generalized.
- **`scaffold` discovery for restic.** Needs a restic-specific "walk the restored
  tree and propose file checks" path (0017 R4). Until then `scaffold` reports "not
  supported", mirroring last-good/fleet.

## Validate it for real

`dev/restic/make-backup.sh` (mirroring `dev/pgbackrest/`) stages known test files
in a Docker volume (a config file with a deterministic sha256, three CSV data
files → a known count), inits a local restic repo in a volume, and backs them up.
`salvage.restic.example.yaml` runs `file_exists` + `file_count` + `checksum` +
`command` checks against it.

```
$ export RESTIC_PASSWORD=salvage-dev
$ salvage run -config salvage.restic.example.yaml
salvage: target "demo-restic"
  restore   ok    (948ms)
  check     ok    config_present
  check     ok    data_files_present         3 within bounds
  check     ok    seed_checksum
  check     ok    config_readable
  verdict   PASS
```

Corrupting the checksum expectation flips exactly that check and the verdict to
FAIL (exit 1) with `got`/`want` shown, confirming the verdict path.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass; no Postgres
   behaviour or test changes.
2. A restic config with `target.type: restic` and the four check kinds parses,
   validates, restores in Docker, and produces a PASS verdict against the dev
   harness; a mismatched check yields a FAIL verdict.
3. Secrets are forwarded by name only; the container is network-isolated after
   restore and before checks.
4. `scaffold`, `last-good`, and `fleet` on a restic target each return a clear
   "not supported for target.type restic" error.
5. The report, verdict, and attestation surface carry no restic-specific
   structure — the restic verdict is signed and attested by the identical path as
   Postgres.
