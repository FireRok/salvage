# Salvage user guide

Salvage proves your backups actually restore: it restores a backup into a
throwaway environment, asserts the data is really there and usable, and emits a
signed pass/fail verdict — then attests to it independently. It **augments** your
integrity checks; it is not a backup tool.

## Contents

1. [Getting started](./01-getting-started.md) — what Salvage is, prerequisites
   (Docker required for the container engines; Go 1.23+ only to build),
   install/build, and a first `salvage run` → verdict walkthrough.
2. [Configuration reference](./02-configuration.md) — the config file: `target`,
   `source`, `restore`, `checks`, severity, expectations, the check kinds
   (`sql`, `collection_count`, `doc_query`, `file_exists`, `file_count`,
   `checksum`, `command`, `http`), report redaction (`keep_literal`,
   `attest.secret_scan`), and the `alerts:` hook block. Parsing is strict —
   misspelled keys fail `salvage check`.
3. [Engines](./03-engines.md) — the vertical-engine / horizontal-platform
   model and the six shipped engines: Postgres (logical + pgBackRest/PITR,
   incl. remote S3/R2), MySQL, MongoDB, restic and borg (filesystem
   snapshots/archives), and exec (bring-your-own-restore) — each with a
   first-run block — plus the roadmap.
4. [Command reference](./04-commands.md) — every CLI command, its flags
   (including `run -json` / `verify -json` machine output and the shared
   `-verbose`/`-quiet` diagnostics), and the `salvage mcp` agent-tool server.
5. [Attestation](./05-attestation.md) — local signing vs. the hosted independent
   notary, the append-only ledger, `attest`/`verify`, login, API keys, and
   orgs/teams/share tokens.
6. [Scheduling & monitoring](./06-scheduling-and-monitoring.md) —
   `salvage schedule`, unattended keys, the dead-man's-switch cadence monitor,
   and hosted alert destinations (webhook/Slack/PagerDuty).
7. [Evidence pack](./07-evidence-pack.md) — the auditor/insurer-ready,
   self-verifiable record of your whole attestation history.
8. [CI integration](./08-ci-integration.md) — running Salvage as a scheduled CI
   job: exit-code gating, `-json` report capture, secrets via `pass_env`, the
   Docker-daemon requirement, and a worked GitHub Actions example.

## Reference material

- [`README.md`](../../README.md) — project overview.
- Worked example configs, one per engine path:
  [`salvage.example.yaml`](../../salvage.example.yaml) (Postgres logical),
  [`salvage.pgbackrest.example.yaml`](../../salvage.pgbackrest.example.yaml)
  (pgBackRest, local repo),
  [`salvage.pgbackrest-s3.example.yaml`](../../salvage.pgbackrest-s3.example.yaml)
  (pgBackRest, remote S3/R2),
  [`salvage.mysql.example.yaml`](../../salvage.mysql.example.yaml) (MySQL),
  [`salvage.mongodb.example.yaml`](../../salvage.mongodb.example.yaml)
  (MongoDB),
  [`salvage.restic.example.yaml`](../../salvage.restic.example.yaml) (restic),
  [`salvage.borg.example.yaml`](../../salvage.borg.example.yaml) (borg),
  [`salvage.exec.example.yaml`](../../salvage.exec.example.yaml) (exec).
- [`CHANGELOG.md`](../../CHANGELOG.md) — user-visible changes per release.
- [`specs/`](../../specs/) — the design specs (source of truth for intent).

## License

Salvage is **Fair Source**, licensed under the Functional Source License
(FSL-1.1-ALv2): free to run, self-host, and modify for any purpose **except**
offering it as a competing commercial service. Each release becomes Apache 2.0
two years after publication.
