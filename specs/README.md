# Salvage specs

Spec-driven development: these documents are the source of truth for *what*
Salvage should do and *why*. Code follows the specs, not the other way around —
a change in intent starts as a spec edit, then the implementation follows.

## Conventions

- Files are numbered `NNNN-short-title.md`.
- Each spec carries a **Status**: `Backlog` → `Proposed` → `Accepted` → `Implemented` → `Superseded`.
- Each spec has: Context, Goals / Non-goals, Requirements (numbered + testable),
  Design, Open questions, Acceptance criteria.
- Requirements are written so an implementer (human or agent) can build each one
  and a test can verify it. Reference them by id (e.g. `R3`) in commits and tests.

## Index

| # | Spec | Status |
|---|------|--------|
| [0000](./0000-product-overview.md) | Product overview & restore-test model | Accepted |
| [0001](./0001-environment-autodetection.md) | Environment auto-detection & zero-config restore | Proposed |
| [0002](./0002-reporting-and-attestation.md) | Reporting & attestation | Proposed |
| [0003](./0003-security-and-isolation.md) | Security & isolation | Proposed |
| [0004](./0004-check-framework.md) | Check / assertion framework | Proposed |
| [0005](./0005-source-interface-and-roadmap.md) | Source interface, transport scope & roadmap | Proposed |
| [0006](./0006-pitr-validation.md) | Recovery-target / PITR validation | Proposed |
| [0007](./0007-scheduling-retention-alerting.md) | Scheduling, retention & alerting | Proposed |
| [0008](./0008-hosted-control-plane.md) | Hosted control plane & MSP multi-tenancy | Proposed |
| [0009](./0009-config-scaffold.md) | Config scaffolding (deterministic, optional AI assist) | Proposed |
| [0010](./0010-last-known-good.md) | Last-known-good recovery-point discovery | Proposed |

## Guiding principle

**Zero-config restore, config-for-intent.** Salvage should auto-detect the
*environment* a backup needs (PG version, required extensions) and auto-discover
the cluster's *topology* (databases, roles, extension versions). The only things
an operator must write are what *correct* looks like (the assertions), the repo
credentials, and — if not "latest" — the recovery target.
