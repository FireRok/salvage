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
| [0002](./0002-reporting-and-attestation.md) | Reporting & attestation | Implemented |
| [0003](./0003-security-and-isolation.md) | Security & isolation | Implemented |
| [0004](./0004-check-framework.md) | Check / assertion framework | Implemented |
| [0005](./0005-source-interface-and-roadmap.md) | Source interface, transport scope & roadmap | Proposed |
| [0006](./0006-pitr-validation.md) | Recovery-target / PITR validation | Proposed |
| [0007](./0007-scheduling-retention-alerting.md) | Scheduling, retention & alerting | Proposed |
| [0008](./0008-hosted-control-plane.md) | Hosted control plane & MSP multi-tenancy | Proposed |
| [0009](./0009-config-scaffold.md) | Config scaffolding (deterministic, optional AI assist) | Implemented |
| [0010](./0010-last-known-good.md) | Last-known-good recovery-point discovery | Implemented |
| [0011](./0011-fleet-discovery.md) | Fleet discovery (enumerate a whole pgBackRest repo) | Implemented |
| [0012](./0012-hosted-attestation.md) | Hosted attestation (independent notary ledger) | Implemented |
| [0013](./0013-self-service-signup.md) | Self-service signup (attestation free tier) | Implemented |
| [0014](./0014-accounts-and-cli-auth.md) | Accounts, API keys & CLI auth (device flow) | Implemented |
| [0015](./0015-scheduled-attestation.md) | Scheduled attestation + dead-man's-switch | Implemented |
| [0016](./0016-modular-engines.md) | Modular engines — the registry-driven engine SPI (vertical seam) | Implemented |
| [0017](./0017-verification-attestation-platform.md) | Verification & attestation platform (horizontal layer) | Implemented |
| [0018](./0018-restic-engine.md) | The restic engine (first non-SQL engine — filesystem snapshots) | Implemented |
| [0019](./0019-evidence-pack.md) | Attestation evidence pack (auditor/insurer artifact) | Implemented |
| [0020](./0020-exec-engine.md) | The exec engine (bring-your-own-restore + Salvage-format validation) | Implemented |
| [0021](./0021-exec-scaffold-assist.md) | Scaffold assist for exec restores (observe & recommend checks) | Proposed |
| [0022](./0022-borg-engine.md) | The borg engine (second filesystem engine — BorgBackup archives) | Implemented |
| [0023](./0023-billing-subscriptions.md) | Billing & subscriptions (Hobby tier, self-serve Stripe) | Implemented |
| [0024](./0024-mysql-engine.md) | The MySQL engine (a second SQL engine — reuses the `sql` check kind) | Implemented |
| [0025](./0025-mongodb-engine.md) | The MongoDB engine (a third way to extend the check-kind seam — `collection_count`/`doc_query`) | Implemented |
| [0026](./0026-machine-readable-output.md) | Machine-readable output contract (`-json` on `run`/`verify`, `schema_version`) | Implemented |
| [0027](./0027-report-redaction-secret-hygiene.md) | Report redaction & secret hygiene (no secrets in the attested report) | Implemented |
| [0028](./0028-cross-engine-scaffold.md) | Cross-engine scaffold (discovery beyond Postgres) | Implemented |
| [0029](./0029-cross-engine-last-good-fleet.md) | Cross-engine last-known-good & fleet (decouple from pgBackRest) | Implemented |
| [0030](./0030-alerting-integrations.md) | Alerting integrations (webhook / Slack / PagerDuty) | Implemented |
| [0031](./0031-orgs-teams-rbac.md) | Organizations, teams & RBAC (the MSP multi-tenancy gap) | Implemented |
| [0032](./0032-mcp-server.md) | The `salvage` MCP server (agent-operable Salvage) | Implemented |
| [0033](./0033-distribution-packaging.md) | Distribution & packaging (release artifacts, signing, install paths) | Implemented |
| [0034](./0034-release-versioning-changelog.md) | Release process, versioning & changelog | Proposed |
| [0035](./0035-upgrade-compatibility-policy.md) | Upgrade & compatibility policy (what keeps working across versions) | Proposed |
| [0036](./0036-supported-platforms-matrix.md) | Supported-platforms & engine-version matrix | Proposed |
| [0037](./0037-docs-coverage-parity.md) | Documentation coverage parity (guide keeps up with shipped features) | Proposed |

See [BACKLOG.md](./BACKLOG.md) for smaller stories and deferred items not yet
promoted to a numbered spec.

## Guiding principle

**Zero-config restore, config-for-intent.** Salvage should auto-detect the
*environment* a backup needs (PG version, required extensions) and auto-discover
the cluster's *topology* (databases, roles, extension versions). The only things
an operator must write are what *correct* looks like (the assertions), the repo
credentials, and — if not "latest" — the recovery target.
