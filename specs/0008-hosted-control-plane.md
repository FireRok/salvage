# 0008 — Hosted control plane & MSP multi-tenancy

- **Status:** Proposed
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

The free, self-hosted CLI is the wedge; the **hosted control plane is the capture
surface**. It aggregates reports across many targets and tenants, issues
**independent attestations** (`0002`), provides a fleet view ("every backup,
tested, green or red"), and serves MSPs managing many clients. This is the
monetization architecture and the open-core line.

## Goals

- Central fleet view across all targets.
- Independent attestation issuance (the un-self-hostable value).
- Multi-tenant isolation for MSPs.
- Per-tenant alerting / SLA reporting.

## Non-goals

- CLI internals (other specs).
- Billing mechanics (acknowledged, not specced here).

## Requirements

**R1 — Report ingestion.** The CLI MUST be able to submit reports + execution
evidence (`0002` R6) to the plane via an authenticated, opt-in channel.

**R2 — Independent attestation issuance.** The plane MUST countersign per `0002`
R4. Two independence tiers:
- *Hosted-runner:* the plane runs the restore-test itself (strongest independence).
- *Verify-only:* the plane attests a tamper-evident execution record from the
  self-hosted CLI.
Define both; the strength of "verify-only" evidence is an open question (`0002`).

**R3 — Fleet view.** A dashboard of all targets: current status, history,
last-green, and trends — "every backup, tested."

**R4 — Multi-tenancy / MSP.** Tenant isolation; an MSP manages many client orgs;
per-tenant RBAC, scoping, and reporting.

**R5 — Alerting & SLA.** Per-tenant notification on failure / missed run
(dead-man, ties to `0007` R5) across channels; SLA reporting for clients.

**R6 — Open-core boundary.** The OSS CLI performs the restore-test; the plane adds
attestation, fleet view, multi-tenancy, retention, and alerting — the paid product.
This MUST align with the FSL licensing decision (individual/single-node free;
multi-tenant/compliance paid).

**R7 — No customer data.** The plane MUST receive only reports / evidence /
metadata (`0002` R7, `0003` R7) — never raw customer row data.

**R8 — Hosted-runner trust.** If the plane runs restores (R2 hosted-runner tier),
it needs only **read-only** repo access (`0003` R5) and MUST apply the same
isolation and ephemerality (`0003`) hosted-side.

## Open questions

- Ship hosted-runner or verify-only first? (Hosted-runner is more convincing but
  carries customer-backup data-residency weight.)
- Data residency for hosted runners (customer backups are sensitive).
- Billing model: per target / per attestation / per seat.
- How strong must verify-only evidence be to satisfy insurers (`0002`)?

## Acceptance criteria

1. A tenant submits reports and sees a fleet view of its targets (R1/R3).
2. An attestation is issued and is externally verifiable (R2, `0002` R5).
3. Tenant isolation holds — one tenant cannot see another's targets (R4).
4. No raw customer row data is stored by the plane (R7).
