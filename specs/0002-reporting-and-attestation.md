# 0002 — Reporting & attestation

- **Status:** Implemented
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

A restore-test's value is only realized if its result is **trustworthy** and —
for compliance and cyber-insurance — **independently verifiable**. Today Salvage
emits a JSON report plus an optional local ed25519 signature. A local signature
proves *integrity* (the report wasn't altered after signing); it does **not**
prove *independence* (that the test really ran, observed by a party other than
the operator). Independent attestation is the un-self-hostable paid surface and
the core of the business model, so its contract must be specified, not improvised.

This spec defines the report schema, the local-signature model, and the
independent-attestation contract. It does **not** define checks (see 0004) or the
hosted control-plane UI (see 0008).

## Goals

- A stable, versioned, machine-readable report schema.
- Local integrity signing that is honestly scoped (integrity, not independence).
- An independent-attestation contract an auditor or insurer will accept.
- Offline-verifiable signatures; resolvable/revocable hosted attestations.
- No sensitive production data in reports.

## Non-goals

- The assertion/check language (0004).
- Hosted control-plane auth, billing, dashboards (0008).

## Requirements

**R1 — Versioned report schema.** Reports MUST be JSON with a `schema_version`.
Fields: tool, version, target, timings, restore result, per-check results,
verdict, and the detected environment (PG version, required extensions — see
0001). The schema MUST be published so third parties can validate.

**R2 — Local signature (integrity).** Salvage MUST be able to ed25519-sign the
canonical report bytes and emit a sidecar with algorithm, public key, signature,
timestamp, and a note that this proves integrity, **not** independence.
*(Implemented.)*

**R3 — Canonicalization.** The signing payload MUST be a deterministic
serialization so signatures verify reliably across tools/versions.

**R4 — Independent attestation (hosted).** A hosted attestor MUST be able to
**countersign** a report into an attestation that a third party will trust.
Independence requires more than a self-asserted JSON: the attestation MUST bind to
**evidence the test actually ran** (R6), produced by the attestor running the
test or by a tamper-evident execution record the attestor verifies. Output: a
countersigned envelope verifiable against the attestor's public key, with a
resolvable verification URL.

**R5 — Offline + online verification.** Local signatures MUST be verifiable
offline with only the public key. Hosted attestations MUST additionally be
resolvable (and revocable) via the attestor service.

**R6 — Evidence binding.** An attestation MUST carry enough evidence to be
meaningful, not "trust me": restore duration, backup id/label, WAL range
restored, restore image digest, the environment detected, and the check
transcript (names, expectations, observed scalars, verdict).

**R7 — Data minimization.** Reports MUST NOT embed raw production data. Checks
return scalars/counts/timestamps; the report stores those (the asserted values)
and nothing more. Anything a check does not intentionally assert MUST NOT appear.

## Design

- **Now:** local ed25519 signature over the canonical report (integrity).
- **Hosted:** `POST report + evidence` → attestor countersigns → returns an
  attestation envelope + verification URL. Independence comes from the attestor
  being a trusted third party that either ran the test or verified a tamper-evident
  record of it.
- **Monetization tie-in:** attestation is the paid, un-self-hostable surface —
  "you can't self-host someone else vouching for you." The free CLI proves a
  restore to *you*; the hosted attestation proves it to *your auditor/insurer*.

## Open questions

- How strong must "evidence the test ran" be to satisfy insurers — attestor-run
  vs verified tamper-evident record?
- Adopt a standard envelope (in-toto / DSSE, SLSA-style provenance) for the
  attestation rather than a bespoke format?
- Attestation expiry and revocation semantics.

## Acceptance criteria

1. A report validates against the published JSON schema and carries
   `schema_version`.
2. A local signature verifies offline with only the public key (R2/R3/R5).
3. A stub attestation flow produces a countersigned envelope, bound to execution
   evidence (R6), verifiable against the attestor's public key (R4/R5).
4. Reports contain no raw row data beyond intentionally asserted scalars (R7).
