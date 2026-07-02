# Evidence pack

A single attestation proves one restore-test happened and was independently
counter-signed. But an auditor (SOC 2, ISO 27001) or a cyber-insurer asks a
different question: *"show me you test restores on a cadence."* The **evidence
pack** (spec [0019](../../specs/0019-evidence-pack.md)) is the artifact that
answers it — the document that turns "we have attestations" into "here is the
record you hand your insurer."

It is a **horizontal platform** feature: it reads only the ledger and knows
nothing about the backup type, so a Postgres attestation and a restic attestation
appear side by side in one unified pack.

## What it aggregates

For your signed-in account, the pack groups every attestation **by target** and
reports, per target:

- attestation **count**,
- the **pass/fail** split,
- the **first** and **last** timestamp (the span it covers),
- an inferred **cadence** — the median interval between attestations, in days
  (null for a target with a single attestation).

Plus an account-level summary: total attestations, number of targets, and the
overall span.

## Two formats

The pack is generated for the signed-in account from the portal, and delivered
as an **owner download → hand-off** — or, with an `evidence`-scoped
[share token](./05-attestation.md#orgs-teams-and-sharing), as a **shareable
URL** an auditor opens directly:

- **`GET /portal/evidence`** — a human-readable, **print-optimized** HTML page.
  Site chrome drops on print, so the browser's print-to-PDF produces a clean,
  filable document.
- **`GET /portal/evidence.json`** — the machine-verifiable JSON pack, as a
  download.

Both formats carry the notary **honest-scope notice** verbatim (received +
counter-signed; does **not** assert the restore ran on Firerok hardware), so the
pack cannot be presented as proof of independent execution.

## Self-verifiable offline

The JSON pack is verifiable by a third party **without contacting the notary and
without an account**. Per attestation it carries everything needed to re-check the
cryptography: `id`, `seq`, `created_at`, `verdict`, `report_sha256`, `prev_hash`,
`entry_hash`, `signature`, and `key_id`, plus each entry's public `verify_url`.
The pack also embeds Firerok's public key and a `verify_hint` describing the
canonical hash.

A recipient recomputes each `entry_hash` and checks the Firerok signature against
the embedded public key — the same verification `salvage verify` and the public
`/a/:id` page perform. Because the ledger is an append-only hash chain, the
**cadence itself** cannot be backdated or reordered, which is precisely what makes
the pack trustworthy to an underwriter.

The reader trusts the cryptography, not the page.
