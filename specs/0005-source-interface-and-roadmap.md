# 0005 — Source interface, transport scope & roadmap

- **Status:** Proposed
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

Backups live in many places — object storage (R2/S3/GCS/Azure), SFTP, SMB/NFS
shares, local disk. A tempting scope creep is to make Salvage able to *fetch*
from all of them. This spec records the boundary decision: it does not.

## Decision

**Salvage ships no transport clients. It drives backup tools and the OS for
transport, and owns the isolated, verified, attested restore-test.**

This is deliberately Unix-y — do one thing well — and here it is *not* a tradeoff:
delegation yields broad location support for free, because transport already lives
elsewhere.

- **Object storage / SFTP:** pgBackRest, restic, WAL-G, kopia speak these backends
  natively. Because Salvage delegates the restore to the tool's own command, it
  inherits every backend that tool supports. (Proven: the divina restore streams
  from R2 with zero transport code in Salvage — pgBackRest is block-aware and pulls
  only what it needs.)
- **SMB/NFS:** filesystem transports — i.e. *mounts*. The OS/container mounts them
  and the tool sees a local path. Supported via existing path/volume inputs; no
  special code.
- **A plain dump with no tool:** the operator stages it (mount, `rclone mount`,
  `aws s3 cp`) to a local path. The Unix way — stage, then point Salvage at it.

| Concern | Owner |
|---|---|
| Fetch bytes + reconstruct the cluster | The backup tool (pgBackRest/restic/WAL-G/pg_restore) |
| Network transport (S3/SFTP/SMB/NFS) | The tool's backends + OS mounts |
| Isolated sandbox, verification, attestation, env auto-detect | **Salvage** |

Salvage **orchestrates** the restore (drives the tool, isolates the sandbox) but
**delegates** the byte-level fetch/reconstruction. Attestation is the *output*;
the core is performing the restore-test in isolation — which is what produces the
evidence the attestation binds to (`0002` R6).

## Goals

- A small, stable interface for adding new source kinds.
- Broad location support via delegation, not transport code.
- Keep the credential/attack surface minimal.

## Non-goals

- Implementing object-storage, SFTP, SMB, or NFS clients.
- A Salvage-native backup format.

## Requirements

**R1 — No transport clients.** Salvage MUST NOT implement S3/object-storage, SFTP,
SMB, or NFS clients. Network transport is the backup tool's backend or an OS mount.

**R2 — Delegated restore.** Physical/tool-backed sources MUST restore by driving
the tool's own restore command (e.g. `pgbackrest restore`), inheriting its
backends. Salvage configures the tool (via the restore image's config) and
forwards credentials by reference (`pass_env`, per `0003`).

**R3 — Filesystem transports as mounts.** SMB/NFS/local MUST be supported by
accepting a path or volume that may be a network mount — handled by existing
inputs (`source.path`, `source.repo_volume`), with no transport-specific code.

**R4 — Operator-staged logical dumps.** For logical sources (`pg_dump`/`sql`)
located remotely, the operator stages them to a local path; Salvage reads local
paths only. Salvage does not fetch them.

**R5 — Credential handling.** Transport credentials are the tool's concern,
forwarded by name and never stored, logged, baked, or placed in arguments
(`0003` R4). Salvage holds no transport secrets of its own.

**R6 — Source-kind interface.** A source kind MUST provide: (a) a way to declare
or detect the **required environment** (feeds `0001`), (b) a routine to **drive
the restore** into the isolated sandbox, and (c) a **Queryer** for the started
cluster. The interface MUST stay small enough that a new source is a self-contained
addition.

**R7 — Explicit out-of-scope.** Managed-provider snapshots (RDS/Cloud SQL/Azure)
are out of scope — managed Postgres exposes no `PGDATA`, so there is nothing to
restore-and-start. Non-PostgreSQL engines are out of scope for now.

## Source roadmap

- **Supported:** `pg_dump` (custom/dir/tar), `sql` (plain), `pgbackrest`
  (physical/PITR, local or S3/R2 repo).
- **Next:** `restic`/`borg` repositories (of dumps or base backups), `wal-g`,
  `barman`, plain `pg_basebackup`.
- **Out of scope:** cloud-provider managed snapshots; non-PostgreSQL engines (for now).

The *engine* seam that lets a non-PostgreSQL engine be added later without
touching the core orchestrator is specified in `0016` (modular engines): an
`Engine` SPI keyed by `target.type` plus a registry, with Postgres as the first
registered engine. R6's small source-kind interface lives *inside* an engine;
`0016` is the layer above it that selects which engine handles a target.

## How this sharpens other specs

- **`0001` (config-for-intent):** transport config belongs to the backup tool's
  native config, not a second place inside Salvage — reinforcing the minimal,
  intent-only config surface.
- **`0003` (security):** shipping no transport clients keeps the credential path
  and attack surface small, in the part of the system that touches production data.

## Open questions

- The exact Go interface shape for a source kind (and how much the physical vs
  logical paths can share).
- Tools without a clean "restore to a path" command — how to drive them.
- Whether to offer *any* convenience for staging (e.g. documenting `rclone mount`
  recipes) without crossing into shipping transport. Lean: docs only.

## Acceptance criteria

1. Adding a new tool-backed source (e.g. WAL-G) requires implementing only the
   small source-kind interface (R6) and no transport code (R1).
2. A backup on an SMB/NFS mount is testable by pointing Salvage at the mounted path
   (R3), with no Salvage code aware of SMB/NFS.
3. An R2/S3-backed repo is testable purely via the tool's backend + `pass_env`
   (R2/R5), as already demonstrated for pgBackRest.
4. The codebase contains no S3/SMB/NFS/SFTP client implementation.
