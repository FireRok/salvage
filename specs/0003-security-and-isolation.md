# 0003 — Security & isolation

- **Status:** Implemented
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

Salvage restores **production** backups and handles repository credentials. The
restored cluster *is* real production data and may carry triggers, scheduled jobs
(pg_cron), foreign-data wrappers, `dblink`, an `archive_command`, or replication
settings that could act on startup. The tool must be safe by construction:
isolate the throwaway, never let it act on the outside world, never persist
sensitive data, and never leak secrets.

This is both a correctness requirement and a product talking point — a verifier
that touches production data must be visibly trustworthy.

## Goals

- Ephemeral, isolated restore: data and cluster destroyed on teardown.
- The restored cluster cannot reach the network or accept external connections.
- Secrets injected by reference, never baked, logged, or placed in arguments.
- Production data never persisted beyond the run.
- Least-privilege repo access.

## Non-goals

- Hardening the host Docker daemon / OS.
- Hosted control-plane authn/authz (0008).

## Requirements

**R1 — Ephemerality.** The restore container MUST be `--rm`; restored PGDATA and
all data MUST be destroyed on teardown; nothing restored is written outside the
container; teardown MUST run even on failure or timeout.

**R2 — Network isolation of the *started* cluster.** Once Postgres is started on
restored data, it MUST NOT reach the network or accept external connections.
`archive_mode` is forced off (implemented). **Timing (validated against the Divina
R2 backup):** isolation happens AFTER the cluster reaches a consistent state, not
before `pg_ctl start` — recovery itself (`archive-get`) needs the repo, so both the
restore *fetch* and recovery require egress. See Design.

**R3 — Neutralize source-side automation on the throwaway.** A restored prod
cluster may contain pg_cron jobs, FDW/dblink targets, or LISTEN/NOTIFY consumers.
These MUST be contained — at minimum by R2 (no network), and where feasible by not
launching the workers that would run them.

**R4 — Secret handling.** Repo credentials MUST be forwarded by reference
(`pass_env`, by name) and MUST NOT appear in config files, images, logs, command
arguments, or reports. Salvage MUST NOT print secret values. *(Forwarding by name
implemented.)*

**R5 — Least-privilege repo credentials.** Salvage needs only **read** access to
the repo. Docs MUST recommend a **read-only** repo token (e.g. a Cloudflare R2
token scoped read-only to the backup bucket) over a read-write key. This is both
correct and a product talking point.

**R6 — Scope of pg_hba relaxation.** The `trust` relaxation that lets the verifier
read the throwaway MUST apply only to local connections inside the isolated,
ephemeral container — never to anything externally reachable (guaranteed by R2).
*(Local-only relaxation implemented.)*

**R7 — Data minimization in output.** No raw production data in reports (see
0002 R7).

**R8 — Supply chain.** Keep dependencies minimal (currently one: `yaml.v3`); pin
base images by digest where practical; keep the tool auditable.

## Design

**The restore-vs-run network nuance (key insight, corrected by the Divina run).** A
remote-repo restore *must* reach the network — and so does recovery — so the
container cannot be `--network none` for its whole lifetime. Resolution: **isolate
late** —
1. *Restore phase:* allow egress so `pgbackrest restore` can fetch the backup.
2. *Recovery phase:* keep egress through `pg_ctl start` — recovery's `archive-get`
   pulls WAL from the repo to reach consistency.
3. *Query phase:* once the cluster is consistent (pg_ctl returns), drop the
   container off the network (`docker network disconnect`) BEFORE any check queries
   it, isolating the live cluster for its whole queryable life.

The original "disconnect before start" was wrong — it cut the network recovery
needs. Isolating after consistency keeps the download AND recovery working while
ensuring the live production data, once queryable, cannot phone home or be reached.

Combined with `--rm` (R1), `archive_mode=off` (R2), local-only `trust` (R6), and
by-name secret forwarding (R4), the restored production data exists only inside an
isolated, ephemeral sandbox.

## Open questions

- Cleanest mechanism to isolate the run phase: `docker network disconnect` after
  restore, a second container, or firewall rules — which is most robust across
  Docker setups?
- Can we restrict restore-phase egress to *only* the repo endpoint (not arbitrary
  network)?
- Detect and refuse to "promote" in a way that could ever connect back to a real
  primary/standby topology.

## Acceptance criteria

1. After a run, no restored data remains on the host or in any volume (R1).
2. The started cluster cannot reach the network (e.g. an outbound connection from
   inside the restored cluster fails) (R2).
3. Secret values never appear in logs, command arguments, images, or reports (R4).
4. Documentation recommends and shows a read-only repo token (R5).
5. Teardown occurs even when the restore or a check fails (R1).
