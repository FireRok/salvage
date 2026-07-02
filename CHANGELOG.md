# Changelog

All notable, user-visible changes to Salvage are documented in this file,
newest first. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning follows
[spec 0034](./specs/0034-release-versioning-changelog.md) (pre-1.0: breaking
changes land only at minor bumps and are flagged `**Breaking:**` here; patch
releases are fixes and strictly additive changes only). GitHub Release notes
for a tag are that release's section of this file.

## [Unreleased]

Nothing yet.

## [0.2.1] — 2026-07-02

### Added

- One-command install: `curl -fsSL https://salvage.sh/install.sh | sh`
  (`scripts/install.sh`). Detects OS/arch (macOS/Linux, amd64/arm64), verifies
  the artifact's SHA-256 against the release's `checksums.txt` **before**
  unpacking (a mismatch aborts with nothing installed), additionally verifies
  the manifest's cosign signature when `cosign` is present, honors
  `SALVAGE_VERSION` and `SALVAGE_INSTALL_DIR`, and never invokes `sudo`.
  ([spec 0033](./specs/0033-distribution-packaging.md))
- Signed releases: every release now ships `checksums.txt.sigstore.json` — a
  keyless cosign signature bundle over the checksum manifest, bound to the
  public release workflow's identity and verifiable by third parties using
  only public information (cosign ≥ 2.4). The verification procedure is
  documented in the README under "Verify the artifacts".
  ([spec 0033](./specs/0033-distribution-packaging.md))
- Homebrew tap: `brew install firerok/salvage/salvage`; the release pipeline
  regenerates and pushes the formula automatically on each release.
  ([spec 0033](./specs/0033-distribution-packaging.md))
- `salvage version -check` — opt-in update check against the GitHub releases
  API (10s timeout): prints the running and latest versions and exits `0`
  up to date / `1` newer release available / `2` check failed. Plain
  `salvage version` still performs no network I/O, the check transmits nothing
  beyond the version lookup itself, and Salvage never modifies its own binary.
  ([spec 0033](./specs/0033-distribution-packaging.md))

### Changed

- README and getting-started guide now lead with the prebuilt install paths
  (install script, Homebrew) before `go install`, with build-from-source last;
  the Quickstart no longer requires Go.
  ([spec 0033](./specs/0033-distribution-packaging.md))

## [0.2.0] — 2026-07-02

### Added

- `salvage login` now tells you which org's ledger attestations from the
  machine will land in, and the hosted approval page lets team members choose
  the org when authorizing a device (keys are pinned to the choice).

- `salvage run -json` — emits the full report JSON to stdout (the exact bytes
  written to `report.out`, which is still honored when set), so CI and agents
  can capture the verdict without a temp file. Exit codes are unchanged.
  ([spec 0026](./specs/0026-machine-readable-output.md))
- `salvage verify -json` — emits a machine verdict object (attestation `id`,
  `target`, `verdict`, `seq`, `key_id`, a `valid` boolean, and the per-check
  verification transcript) instead of the human text. Exit codes are unchanged.
  ([spec 0026](./specs/0026-machine-readable-output.md))
- Every report now carries a top-level `schema_version` field (currently `1`)
  on every output path — the `report.out` file, `run -json` stdout, and the
  bytes submitted for attestation.
  ([spec 0026](./specs/0026-machine-readable-output.md))
- `doc_query` checks (MongoDB) accept a `max_age` expectation — assert a
  timestamp field is no older than a configured window, the same freshness
  check the `sql` kind already had.
- Live-Docker end-to-end verification of the restic, borg, and MySQL engines,
  recorded in `dev/<engine>/VERIFIED.md` (MongoDB was verified live at
  release of its engine).
- User guide: engine chapters and first-run blocks for all six shipped engines
  (borg, MySQL, and MongoDB were previously undocumented), the
  `collection_count`/`doc_query` check-kind reference, and a new
  [CI integration](./docs/guide/08-ci-integration.md) chapter. This changelog.
- Client-side alert hooks: an `alerts:` config block with `on_fail`/`on_success`
  hooks fired by `run` and `attest` after the report is written — a command
  (run via `sh -c` with the redacted report JSON on stdin and the report path
  in `$SALVAGE_REPORT`) or an `http(s)://` URL (the report JSON POSTed as
  `application/json`). Hooks are best-effort (a failure is logged and never
  changes the exit code), bounded by `alerts.timeout` (default 30s), and URL
  tokens are passed by reference (`token_ref=env:NAME`), never embedded.
  ([spec 0030](./specs/0030-alerting-integrations.md))
- Hosted notary: per-monitor **alert destinations** — generic webhook, Slack,
  PagerDuty, and extra email — fired on dead-man's-switch transitions, managed
  via `/v1/orgs/:id/monitors/:mid/destinations`; destination secrets are stored
  by reference. ([spec 0030](./specs/0030-alerting-integrations.md))
- Hosted notary: **orgs/teams with RBAC** (`owner`/`admin`/`member`/`viewer`),
  private ledgers, scoped revocable **share tokens**, and a shareable
  evidence-pack URL. ([spec 0031](./specs/0031-orgs-teams-rbac.md))
- `salvage last-good` and `salvage fleet` now support **restic and borg**
  targets in addition to pgBackRest: `last-good` walks snapshots/archives
  newest-first (each candidate is a full restore — bound long histories with
  `-max`); `fleet` surveys the repository as one unit, metadata-only.
  ([spec 0029](./specs/0029-cross-engine-last-good-fleet.md))
- `salvage scaffold` is now cross-engine — **postgres, mysql, restic, borg,
  and exec** (exec requires `restore.workdir`; MongoDB reports "not
  supported") — and gained a `-cap N` flag (default 50) bounding generated
  checks to the top-N tables/directories per family by observed size.
  Generated checks are verified against the restored snapshot before emit.
  ([spec 0028](./specs/0028-cross-engine-scaffold.md))
- `salvage mcp` — serve Salvage as a Model Context Protocol server over stdio:
  eight tools (`salvage_run`/`check`/`inspect`/`last_good`/`fleet`/`verify`/
  `attest`/`scaffold`), each carrying a read-only / restore-executing /
  mutating classification hosts can gate on. Credentials stay by reference
  (never tool arguments); `salvage_run` returns the report as the tool result
  and writes no file. ([spec 0032](./specs/0032-mcp-server.md))
- Per-check `keep_literal: true` — opt-in to store a check's exact `got` value
  (requires an `equals` expectation; known-secret scrubbing still applies), and
  `salvage run -show-output` — print the raw (still secret-scrubbed) restore
  output to stderr, never into the report.
  ([spec 0027](./specs/0027-report-redaction-secret-hygiene.md))
- `attest.secret_scan: refuse|warn|off` — a pre-submission credential-pattern
  gate over the report bytes (AWS key ids, PEM keys, bearer tokens,
  URL-embedded `user:pass@`). Default `refuse`: a match refuses the submission
  with exit `2`. ([spec 0027](./specs/0027-report-redaction-secret-hygiene.md))
- `-verbose` / `-quiet` on `run`, `check`, `scaffold`, `last-good`, `fleet`,
  and `attest` — leveled stderr diagnostics only; stdout output, report JSON
  bytes, and exit codes never change. `-verbose` on `run`/`attest` also prints
  the raw secret-scrubbed command output.
- `http` checks are now valid for **restic and borg** targets too (previously
  exec-only) — probe a restored service over HTTP from the Salvage host.
- [`salvage.exec.example.yaml`](./salvage.exec.example.yaml) — a worked example
  config for the exec (bring-your-own-restore) engine.

### Changed

- **Breaking:** config parsing is now strict — an unknown or misspelled YAML
  key (e.g. `expct_min`, `snapshto`, `pass_evn`) fails config load with an
  error naming the key (`salvage check` exits `2`), instead of being silently
  dropped. Fix the key; no valid config is affected.
- **Breaking:** `salvage fleet` now exits `1` when any surveyed stanza is
  degraded or has zero backups (or the repo has no stanzas), so cron/CI get a
  real failure signal. It previously exited `0` regardless; exit `0` now means
  every stanza is healthy and non-empty.
- **Breaking:** report redaction & secret hygiene is now **default-on** on
  every output path (`report.out`, `run -json` stdout, and the attested bytes):
  captured restore output and long/multi-line check `got` values are stored as
  a bounded, scrubbed preview plus a SHA-256 fingerprint, and every occurrence
  of a known secret value (`source.pass_env`, `restore.env`, alert-hook
  `token_ref` env vars) is replaced with a `[REDACTED:<name>]` marker. Short
  scalar `got` values (counts, booleans, digests) are unchanged; use
  `keep_literal` per check or `run -show-output` (stderr-only) where the
  literal is needed. There is no opt-out.
  ([spec 0027](./specs/0027-report-redaction-secret-hygiene.md))
- Default restore images are now **pinned to the versions verified
  end-to-end** (`dev/<engine>/VERIFIED.md`) instead of floating tags, so an
  upstream retag never changes a pinned Salvage release's behavior — a
  default-behavior change; override with `restore.image` as before:
  - restic: `restic/restic:0.19.0` (was `restic/restic:latest`)
  - borg: `ghcr.io/borgmatic-collective/borgmatic:2.1.6`, which ships borg
    1.4.4 (was `ghcr.io/borgbackup/borg:stable`; borg publishes no official
    image and the borgmatic image ships `borg` on `PATH`)
  - MySQL: `mysql:8.4.10` (was `mysql:8`)
  - MongoDB: `mongo:7.0.37` (was `mongo:7`)

## [0.1.1] — 2026-07-01

### Fixed

- `salvage version` reports its real version for `go install …@vX` builds
  (falls back to Go module build info when release ldflags are absent).

## [0.1.0] — 2026-07-01

Initial public release: the Postgres and restic engines, the check framework,
signed reports, scaffold/fleet/last-good, and hosted attestation
(`salvage attest` / `salvage verify` against the independent notary).
