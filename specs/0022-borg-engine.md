# 0022 — The borg engine (the second filesystem engine)

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

[[spec 0018]] built the restic engine — Salvage's first non-SQL engine — and in
doing so proved out both seams from [[spec 0017]] at once: a filesystem restore
into a throwaway container, validated with file/command probes instead of SQL,
inheriting the report + verdict + attestation + dead-man's-switch layers
unchanged. Both 0017 (R5) and 0018 named **borg as the sibling that comes next**:
the same seams, a different filesystem backup tool.

This spec is that engine. borg is the **second filesystem engine** and a
near-exact sibling of restic. Since 0018 the shared file/command check kinds and
the `FileProber` capability were lifted into `internal/probe` (spec 0020), so
borg registers **nothing new**: its restored target satisfies `probe.FileProber`
via `docker exec`, and it inherits `file_exists`/`file_count`/`checksum`/`command`
for free. The horizontal platform (report, verdict, signing/ledger/verify,
attestation, dead-man's-switch) is unchanged.

## Goals

- A registered `spi.Engine` for `target.type: "borg"` that extracts a BorgBackup
  archive in Docker (no host `borg` required) and returns a filesystem-probeable
  target satisfying the shared `probe.FileProber`.
- The same non-SQL check kinds restic uses (`file_exists`, `file_count`,
  `checksum`, `command`), inherited from `internal/probe` with no new
  registration, and the verdict/report/ledger path completely unchanged.
- Credentials strictly by reference (spec 0003): `BORG_PASSPHRASE` and, when the
  repo itself is a secret, `BORG_REPO`, forwarded by name via `pass_env`.
- Two-phase network isolation (spec 0003 R2), identical to the restic path.
- Adding this engine touches only new files (`internal/engine/borg`,
  `internal/ephemeral/borg.go`) plus the config allow-list and one blank import —
  no change to `internal/engine` orchestration or the CLI.

## Non-goals

- `scaffold`, `last-good`, `fleet` for borg — like restic, borg does not
  implement `discover.RowQueryer`, `spi.ChainTester`, or `spi.FleetSurveyor`, so
  all three cleanly report "not supported for target.type borg" via the existing
  gates, with zero orchestrator change (see 0018 Open questions).

## Design

### The engine (`internal/engine/borg`)

`Engine{}` implements `spi.Engine` for `Type() == "borg"` and `Register`s itself
in `init()`; a blank import of `internal/probe` pulls in the shared kinds. `Restore`
is line-for-line the restic engine with the borg lifecycle swapped in:

1. `ephemeral.Preflight` + `requireEnv(pass_env)` — a missing Docker or a missing
   secret is a `spi.Fault` (operational, exit 2, no verdict).
2. `ephemeral.StartBorg` stands up an idle `--entrypoint sh … sleep infinity`
   container from a maintained borgbackup image (configurable; default
   `ghcr.io/borgmatic-collective/borgmatic:latest` — borg publishes no official
   image; the borgmatic image ships borg on PATH), mounting a local repo volume where the
   repository points and forwarding `BORG_REPO`/`BORG_PASSPHRASE` by name. A
   container-create failure is a `spi.Fault`.
3. `env.Restore(archive)` runs `borg extract ::<archive>` from the restore dir. A
   failure is a **bare error** (a "fail" verdict), not a Fault.
4. **Two-phase network isolation** (spec 0003 R2): connected through the extract
   (a remote `ssh://` repo may need egress), then `docker network disconnect` off
   every network *before any check runs*.

The returned `*ephemeral.Borg` is the live `RestoredTarget`; `Stop()` `docker
kill`s it and is idempotent.

### The prober

`*ephemeral.Borg` satisfies `probe.FileProber` (Exists/Count/Sha256/RunCommand)
via `docker exec` against `/restore`, identical to `*ephemeral.Restic`. The only
tool-level differences from restic are:

- **Extract, not restore.** `borg extract` writes into the current working
  directory (there is no `--target` flag), so the container `mkdir -p /restore`s
  up front and the extract runs `cd /restore && borg extract ::<archive>`.
- **No "latest" alias.** borg has no `latest` archive, so `source.archive` is
  required (restic defaults `snapshot` to `latest`).
- **`borgError`** extracts borg's own error line (wrong passphrase, missing
  archive, not-a-repository) for the verdict reason, as `resticError` does.

It does **not** implement `discover.RowQueryer`, so `scaffold` gates off cleanly.

### Config (`internal/config`)

- `Source` gains `Archive` (required; `yaml:",omitempty"` so other configs are
  byte-identical). `Repository`, `RepoVolume`, `RepoPath`, and `PassEnv` are
  reused exactly as for restic.
- `applyDefaults` defaults the borg image (`ghcr.io/borgmatic-collective/borgmatic:latest`) and
  the local-repo mount path; there is no archive default.
- `Validate` accepts `target.type: "borg"` via `validateBorgSource`: a repository
  inline (`repository`) or by reference (`BORG_REPO` in `pass_env`), plus a
  required `archive`. The file/command check kinds are accepted on borg targets
  through the shared `isFileProbeTarget` helper (restic/borg/exec); unknown kinds
  still fail at load, and a file kind on a postgres target still errors. Postgres
  and restic validation and messages are unchanged.

### Credentials & isolation

- **By reference only (spec 0003):** `BORG_PASSPHRASE` (and `BORG_REPO` when the
  repo is a secret) are named in `source.pass_env` and forwarded with `docker run
  -e NAME` — the value never appears in a command argument or in the config file.
  A plain local repo path may be set inline via `source.repository` (→ `BORG_REPO`).
- **Isolation (spec 0003 R2):** connected for the extract, disconnected from every
  network before any check.

## Requirements

**R1 — Registered borg engine.** There MUST be an `spi.Engine` for
`target.type: "borg"`, registered in its package `init()` and wired via a blank
import in `internal/engine/engine.go`. It MUST extract in Docker with no host
`borg` binary.

**R2 — Filesystem-probeable target.** `Restore` MUST return a `RestoredTarget`
satisfying `probe.FileProber` (exists/count/sha256/command over `docker exec`)
with an idempotent `Stop()`. It MUST NOT implement `discover.RowQueryer` (so
`scaffold` gates off cleanly).

**R3 — Inherited non-SQL check kinds.** `file_exists`, `file_count`, `checksum`,
and `command` MUST evaluate against a borg target via the shared `internal/probe`
evaluators. borg MUST register no new evaluators.

**R4 — Operational-vs-verdict split.** A missing `pass_env` secret or a
Docker/container problem MUST be a `spi.Fault` (exit 2, no verdict); an archive
that fails to extract MUST be a bare error (a "fail" verdict). Inherited unchanged
from 0016 R4.

**R5 — Credentials by reference + isolation.** Secrets MUST be forwarded by name
only (spec 0003); the container MUST be disconnected from every Docker network
after the extract and before any check (spec 0003 R2).

**R6 — Config allow-list.** `config.Validate` MUST accept `target.type: "borg"`
and validate its source shape (a repository inline or by reference, plus a
required archive) and its check kinds, with all Postgres and restic validation
and messages unchanged (0016 R6).

**R7 — Inherited platform, no new deps.** The report, verdict, signing/ledger/
verify, and dead-man's-switch MUST be inherited unchanged. No new Go dependency
(stdlib + `gopkg.in/yaml.v3`); the borgbackup Docker image is runtime, not a Go
dep.

## Open questions

Same as restic ([[spec 0018]] Open questions): borg archives form a history, so a
`ChainTester`/`FleetSurveyor` is conceivable, but the orchestrator's
`last-good`/`fleet` gates are `pgbackrest`-scoped today. borg omits both
optional capabilities, so both commands cleanly report "not supported for
target.type borg" with zero orchestrator change — a small, safe follow-up once
the gate is generalized. `scaffold` is gated the same way.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass; no Postgres or
   restic behaviour or test changes.
2. A borg config with `target.type: borg` and the four check kinds parses,
   validates, and (against a real repo) extracts in Docker and produces a PASS
   verdict; a mismatched check yields a FAIL verdict.
3. Secrets are forwarded by name only; the container is network-isolated after
   extract and before checks.
4. `scaffold`, `last-good`, and `fleet` on a borg target each return a clear
   "not supported for target.type borg" error.
5. The report, verdict, and attestation surface carry no borg-specific structure —
   the borg verdict is signed and attested by the identical path as restic and
   Postgres.
