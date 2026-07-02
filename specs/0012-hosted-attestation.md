# 0012 — Hosted attestation (independent notary ledger)

- **Status:** Implemented
- **Created:** 2026-06-30
- **Owner:** Firerok

## Context

A local ed25519 signature ([[spec 0002]]) proves a report was not altered after
signing — but the customer holds the key, so it only proves *self-consistency*.
It does not prove an **independent** party saw the verdict. Auditors, cyber-insurers,
and a customer's own customers want the stronger claim: *"a third party attests this
restore-test happened, at this time, and the history is unbroken."*

That independent countersignature is the **paid moat**: you cannot self-host someone
else vouching for you ([[spec 0008]] is the broader control-plane vision; this spec is
the minimal, cheap first cut).

**Model (decided): notary ledger now, execution later.** v1 is a *notary* — the
customer runs `salvage run` locally and submits the signed report; the service
verifies it, timestamps it, counter-signs with a key the customer cannot access, and
appends it to a tamper-evident per-tenant ledger. It does **not** re-run the restore
in v1 (that is "independent execution", a later premium tier — it needs credential
custody + compute per test, which breaks [[spec 0005]]'s no-credential-custody line and
the cost target). The schema/API leave room for it (a `method` field: `notary` now,
`hosted-exec` later).

**Honest claim.** The notary proves: *Firerok independently received report R from
tenant T at server-time S, R carried a valid tenant signature, and this entry extends
tenant T's append-only history at sequence N.* It does **not** prove the restore
physically happened on Firerok's own hardware — that is the execution tier. Marketing
MUST NOT conflate the two.

## Cost posture

Serverless, no per-customer compute: Cloudflare **Worker + D1**. Notarizing is
signature verification + a counter-signature + a small-JSON append, so the service
has no always-on process and scales to zero when idle — which is what makes a
**limited free tier** (N attestations/month, public ledger) sustainable by design.

## Goals

- A hosted endpoint that accepts a signed Salvage report, counter-signs it under
  Firerok's key, and appends it to a tamper-evident per-tenant ledger.
- A public, verifiable URL per attestation, checkable by anyone (auditor/insurer).
- CLI: `salvage attest` (run + submit) and `salvage verify` (fetch + check offline
  against Firerok's published public key).
- Free tier by monthly cap; paid tier removes the cap (+ private ledger/SSO later).

## Non-goals (v1)

- Re-running the restore server-side (execution tier — later, behind `method`).
- Custody of customer backup credentials (never, for the notary).
- A web dashboard/billing UI — API + JSON first; a landing page is cosmetic.
- Private ledgers / SSO — paid-tier features, later.

## Architecture

```
customer:  salvage run → report.json (+ .sig, tenant ed25519)
           salvage attest → POST /v1/attestations  (Bearer <api-key>, body = report)
service (Worker):
           auth tenant (api-key hash) → enforce free-tier monthly cap
           verify tenant signature over report bytes (if provided)
           seq = last+1; prev = last.entry_hash
           entry_hash = SHA256( "v1\n"+prev+"\n"+id+"\n"+tenant+"\n"+seq+"\n"+created_at+"\n"+report_sha256+"\n"+verdict )
           sig = Ed25519_sign(FIREROK_KEY, entry_hash_bytes)
           INSERT into D1; return {id, url, seq, created_at, report_sha256, entry_hash, signature, key_id}
verifier:  salvage verify <id|url> → GET /v1/attestations/:id
           recompute entry_hash from fields; check == returned
           Ed25519_verify(FIREROK_PUBKEY[key_id], entry_hash_bytes, sig)
           if report present: SHA256(report)==report_sha256; tenant sig (if present) valid
```

Per-tenant **hash chain**: each entry's `prev_hash` is the previous entry's
`entry_hash`, and Firerok signs the `entry_hash`. An entry cannot be altered,
backdated, reordered, or deleted without breaking the chain — so the *cadence itself*
(e.g. "a passing restore every week for a year") is tamper-evident, which is what an
insurer underwrites against.

## Requirements

**R1 — Independent key.** Attestations are signed by a Firerok ed25519 key the customer
never holds (Worker secret). The public key(s) are published (`GET /v1/pubkey`, keyed by
`key_id` for rotation) and baked into the CLI for offline verify.

**R2 — Tenant auth.** `Authorization: Bearer <api-key>`; only the SHA-256 hash of the key
is stored. A missing/invalid key is 401.

**R3 — Submit.** `POST /v1/attestations` accepts a Salvage report JSON (optionally the
tenant signature + pubkey). The service records server-authoritative `created_at`,
`report_sha256`, `target`, `verdict`, the tenant pubkey and whether its signature
verified, assigns the next per-tenant `seq`, chains `prev_hash`, computes+signs
`entry_hash`, and stores the report. Returns the attestation record incl. public `url`.

**R4 — Append-only + chained.** The ledger is insert-only; no update/delete endpoints.
`entry_hash` chains to the prior entry per tenant (R-design above).

**R5 — Public verify.** `GET /v1/attestations/:id` returns the record + Firerok signature
(and report) with no auth, so a third party can verify. `GET /v1/tenants/:id/ledger`
returns the chained list for whole-history/cadence verification.

**R6 — Free-tier cap.** Free tenants are limited to N attestations per calendar month
(deterministic count in D1); over the cap → 429 with a clear message. Paid tenants
uncapped. The cap is config, not code.

**R7 — CLI.** `salvage attest [-config … | -report …]` submits and prints the URL;
`salvage verify <id|url>` fetches and verifies **offline** against the baked-in Firerok
pubkey, reporting each check (Firerok sig, chain hash, report hash, tenant sig). Exit 0
if the attestation is genuine, 1 if not, 2 on operational error.

**R8 — Honest scope in output.** Attestation responses and `salvage verify` MUST state
that this is an independent *notary* record (received + counter-signed), not proof the
restore ran on Firerok hardware (R-claim above).

**R9 — Proprietary/OSS split.** The Worker is proprietary (separate private repo). The
wire protocol, the CLI client, and Firerok's public key are open (this repo). Nothing in
the OSS repo depends on the server's source.

## Data model (D1)

```sql
CREATE TABLE tenants (
  id           TEXT PRIMARY KEY,      -- t_<rand>
  name         TEXT NOT NULL,
  api_key_hash TEXT NOT NULL UNIQUE,  -- sha256(api key)
  plan         TEXT NOT NULL DEFAULT 'free',  -- free | paid
  created_at   INTEGER NOT NULL
);
CREATE TABLE attestations (
  id             TEXT PRIMARY KEY,    -- att_<rand>, also the URL slug
  tenant_id      TEXT NOT NULL REFERENCES tenants(id),
  seq            INTEGER NOT NULL,    -- per-tenant, monotonic from 1
  created_at     INTEGER NOT NULL,    -- server unix seconds (authoritative)
  method         TEXT NOT NULL DEFAULT 'notary',  -- notary | hosted-exec (future)
  target         TEXT,
  verdict        TEXT NOT NULL,       -- pass | fail
  report_sha256  TEXT NOT NULL,
  tenant_pubkey  TEXT,                -- customer local signing pubkey (base64), if sent
  tenant_sig_ok  INTEGER NOT NULL DEFAULT 0,
  prev_hash      TEXT NOT NULL,       -- previous entry_hash, or 64 zeros at genesis
  entry_hash     TEXT NOT NULL,       -- sha256 hex over the canonical string
  signature      TEXT NOT NULL,       -- Firerok ed25519 over entry_hash bytes (base64)
  key_id         TEXT NOT NULL,       -- which Firerok key signed
  report_json    TEXT NOT NULL,
  UNIQUE (tenant_id, seq)
);
CREATE INDEX idx_att_tenant ON attestations(tenant_id, seq);
```

## Open questions

- Public transparency log (cross-tenant Merkle root, published periodically) so even
  Firerok cannot rewrite history — a stronger claim, later.
- Billing integration (Stripe) — deferred until a paying customer exists.
- Execution tier: run the restore in a Cloudflare Container / a Firerok runner and sign
  that — the premium upsell, schema-ready via `method`.

## Acceptance criteria

1. `salvage attest` against a passing report returns a public URL; fetching it shows the
   verdict, server timestamp, and a Firerok signature.
2. `salvage verify <url>` verifies the Firerok signature and chain hash **offline**
   against the baked-in public key, and re-checks the report hash; exit 0.
3. Tampering with any stored field (verdict, timestamp, report) makes `salvage verify`
   fail (signature/chain mismatch).
4. A second attestation from the same tenant chains to the first (`prev_hash` = first
   `entry_hash`, `seq` = 2).
5. A free tenant over the monthly cap gets 429; a paid tenant does not.
6. Output never claims the restore ran on Firerok hardware (R8).
