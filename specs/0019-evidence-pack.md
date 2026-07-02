# 0019 — Attestation evidence pack

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

A single attestation ([[spec 0012]]) proves one restore-test happened and was
independently counter-signed. But an auditor (SOC 2, ISO 27001) or a cyber-insurer
asks a different question: *"show me you test restores on a cadence."* They want an
**aggregated, verifiable record** of the account's whole history — which targets, how
often, pass rate, over what period — in a form they can file and check. That is the
evidence pack: the artifact that turns "we have attestations" into "here is the
document you hand your insurer."

It is a **horizontal platform** feature ([[spec 0017]]): it reads the ledger and knows
nothing about the backup type, so a Postgres attestation and a restic attestation
appear side by side.

## Goals

- Aggregate an account's attestation history into an auditor/insurer-ready document.
- Two formats: a human-readable, print-to-PDF page and a machine-verifiable JSON.
- Independently verifiable **offline** — the reader trusts the cryptography, not the page.
- Carry the honest-scope notice so the pack cannot be misrepresented.

## Non-goals

- A literal one-click `.pdf` download (print-to-PDF suffices for v1; a PDF-lib render is
  a possible follow-up).
- Public share-token access (an auditor receiving the file can verify it offline; a
  shareable public evidence URL ties to the Team-tier private-ledger work).
- Any new claim beyond what an attestation already proves (still notary, not executor).

## Requirements

**R1 — Aggregate per target.** For the authenticated account, group attestations by
`target` and report per target: count, pass/fail split, first/last timestamp, and an
inferred **cadence** (median interval between attestations, in days; null for a single
one). Plus an account-level summary (total attestations, target count, span).

**R2 — Two formats.** `GET /portal/evidence` renders a human-readable, **print-optimized**
HTML document (site chrome dropped on print → a clean, filable PDF via the browser).
`GET /portal/evidence.json` returns the machine-readable pack as a download.

**R3 — Self-verifiable offline.** The JSON pack MUST carry, per attestation, everything a
third party needs to verify without contacting the notary: `id, seq, created_at, verdict,
report_sha256, prev_hash, entry_hash, signature, key_id`, plus the Firerok public key and
a `verify_hint` describing the canonical hash. Each entry also carries its public
`verify_url`. Verification matches `salvage verify` and the `/a/:id` page.

**R4 — Honest scope.** Both formats MUST include the notary honest-scope notice verbatim
(received + counter-signed; does NOT assert the restore ran on Firerok hardware), so the
pack cannot be presented as proof of independent execution.

**R5 — Owner-authed.** The pack is generated for the signed-in account (session).
Delivery is owner-download → hand-off; the JSON's self-verifiability means the recipient
needs no account and no notary access.

**R6 — Backup-type-agnostic.** The pack reads only the ledger; it MUST NOT assume Postgres
or any engine. A mixed account (e.g. Postgres + restic targets) produces one unified pack.

## Design

`internal` to the notary (`salvage-attest`): `accountAttestations(account)` reads the
ledger; `buildEvidence(...)` aggregates (per-target + `medianCadenceDays`); `evidencePage`
renders branded print-friendly HTML; `evidenceJSON` returns the download. Surfaced from
the portal once the account has ≥1 attestation. The append-only hash chain is what makes
the *cadence* itself trustworthy — it cannot be backdated or reordered (see [[spec 0012]]).

## Acceptance criteria

1. An account with attestations across multiple targets gets a pack showing per-target
   count, pass/fail, span, and cadence, plus the honest-scope notice.
2. The JSON pack verifies offline: recomputing each `entry_hash` and checking the Firerok
   signature against the embedded public key succeeds for genuine entries.
3. The HTML page prints to a clean PDF (no nav/footer, ink-on-white).
4. A mixed Postgres + restic account produces a single unified pack (R6).
