# 0031 — Organizations, teams & RBAC

- **Status:** Implemented — R6 partial: SP-initiated SAML redirect + SSO config CRUD ship; assertion (XML-DSig) verification at the ACS is deferred (returns 501) pending a vetted verifier
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

[[spec 0008]] framed the hosted control plane as the capture surface and named four
pillars: report ingestion, independent attestation issuance, a fleet view, and
**MSP multi-tenancy with per-tenant RBAC, alerting, and SLA reporting**. Most of that
umbrella has since shipped in narrow, single-human slices — the notary ledger
([[spec 0012]]), signup ([[spec 0013]]), accounts + CLI auth ([[spec 0014]]), and
self-serve billing ([[spec 0023]]). One pillar is deliberately still unbuilt: the
**multi-user layer** — organizations, members, roles, and tenant isolation for an
MSP. This spec fills exactly that gap.

The current state is single-human by design, and each of the shipped specs drew the
line explicitly:

- [[spec 0014]] scoped identity to "**one account = one human (or one project)**" and
  listed *orgs/teams/RBAC* (and password auth) as explicit Non-goals. An account holds
  many API keys, but no notion of a *second person* who can also hold keys against the
  same ledger.
- [[spec 0023]] shipped a **single self-serve tier (Hobby)** on flat-rate machinery and
  named Team/Business/MSP tiers, **seat/quantity pricing, and annual plans** as Non-goals
  ("later Prices, same flow"). The billing rails exist; the multi-seat shape on top of
  them does not.
- [[spec 0012]] gives each account a **per-account, tamper-evident hash-chained ledger**
  but names **private ledgers and SSO** as paid-tier Non-goals. Ledgers today are public
  by id.
- [[spec 0019]] produces an auditor/insurer-ready **evidence pack**, but has **no
  shareable public URL** — an auditor must *receive the file* and verify it offline. A
  shareable evidence URL is explicitly deferred to "the Team-tier private-ledger work",
  i.e. this spec.

So the unbuilt work is a coherent cluster: an **organization** that many humans belong
to under **roles**, tenant isolation strong enough for an **MSP** managing many client
sub-tenants, **private ledgers** that gate read access (which in turn unblocks a
shareable evidence-pack URL), **SSO/SAML** for the larger tiers, and a **mechanism** for
turning capabilities on and off per named tier. This spec specifies that layer.

**Publication boundary.** This spec ships in the public `specs/` tree. It therefore
names tiers **only** by name — Community, Hobby, Team, Business, MSP, Enterprise — and
never states a price, packaging bundle, discount, or commercial framing. Concrete prices,
seat minimums, and which capability lands in which tier live in `marketing/pricing.md` and
are **out of scope** for this spec. Where a requirement gates a capability "to a higher
tier", the *set* of tiers that unlock it is a config/marketing decision, not a spec
constant.

## Goals

- An **Organization** entity that owns targets, ledger, attestations, billing, and
  settings, with **many Members** joined under **Roles**.
- **RBAC** — a small fixed role set (`owner`, `admin`, `member`, `viewer`) enforced at
  every mutation and read boundary across ledger, attestations, billing, and settings.
- An **MSP tenancy model**: a managing org that oversees many **client sub-tenants**,
  each with its own isolated ledger, building directly on [[spec 0012]]'s per-account
  ledger isolation — a client sub-tenant's members MUST NOT be able to read another
  client's ledger.
- **Private ledgers** — a ledger whose read surface is gated to org members, plus
  **share-token URLs** that grant scoped, revocable read access to an outside party
  (auditor/insurer) without an account. This also unblocks [[spec 0019]]'s deferred
  **shareable evidence-pack URL**.
- **SSO/SAML** sign-in, **tier-gated** to the larger tiers.
- A **tier-capability gating mechanism**: capabilities are enabled per named tier by
  config; the code asks "does this org's tier grant capability X?" and never hard-codes a
  tier name at a call site. The capability list is in this spec; the numbers and bundling
  are not.
- **Backward compatibility**: every existing single-human [[spec 0014]] account
  transparently becomes a **one-member organization** owned by that human, with no wire,
  CLI, or credential change.
- A **conceptual extension** of [[spec 0023]] billing to **seats/quantities and annual**
  intervals — the mechanism only, with prices deferred to marketing.

## Non-goals

- **Prices, packaging, seat minimums, discounts, and tier bundling.** Named tiers only;
  the numbers live in `marketing/pricing.md`. No dollar figures appear in this spec.
- **The independent-execution / hosted-runner tier** ([[spec 0008]] R2 hosted-runner,
  [[spec 0012]] `method: hosted-exec`). Orgs/RBAC govern *who may see and act on* the
  notary ledger; they do not re-run restores. That tier remains future work and is
  unaffected by this one.
- **Custody of customer backup credentials or raw row data** ([[spec 0008]] R7,
  [[spec 0012]] Non-goals). The plane still stores only reports/evidence/metadata; RBAC
  changes who can *read* those, never *what* is stored.
- **Fine-grained / per-target ACLs and custom roles.** v1 ships four fixed org-wide roles.
  Per-target scoping and user-defined roles are Open questions.
- **SCIM / directory auto-provisioning.** SSO/SAML sign-in is in scope; automated
  user lifecycle sync (SCIM) is deferred.
- **Password auth.** Still a Non-goal (inherited from [[spec 0014]]); sign-in remains
  GitHub OAuth, email magic-link, and — new here — SSO/SAML for gated tiers.
- **A separate MSP fleet-dashboard UI spec.** [[spec 0008]] R3's fleet view is its own
  surface; this spec provides the *tenancy and RBAC substrate* it reads through, not the
  dashboard layout.

## Design

Everything here lives in the **proprietary notary Worker** (`salvage-attest`,
Cloudflare Worker + D1), exactly like [[spec 0012]]/[[spec 0013]]/[[spec 0014]]/
[[spec 0023]]. The public `salvage` repo carries **none** of it, and the
CLI/attestation wire protocol is **unchanged** — a member's API key still attests with
`Bearer <key>` and resolves through `api_keys` to an *account*, which is now a member of
exactly one org (see backward-compat below).

### The organization is the tenancy unit

Today [[spec 0014]]'s `account` doubles as both "a human" and "a billing/ledger tenant".
This spec **splits those roles**:

- An **`org`** becomes the tenant that owns the ledger, attestations, billing state, and
  settings. Billing columns added by [[spec 0023]] R2 (`stripe_customer_id`,
  `stripe_subscription_id`, `subscription_status`, `plan`) move to the org — the org is
  what carries a subscription and a tier.
- An **`account`** ([[spec 0014]]) stays "a human/identity" (email, `github_id`,
  sessions, magic tokens). It no longer carries a plan directly; its entitlement is its
  org's tier.
- A **`membership`** joins an account to an org under a **role**.

```sql
-- migration 0007, additive (new tables + a nullable back-reference)
CREATE TABLE orgs (
  id                    TEXT PRIMARY KEY,     -- org_<rand>
  name                  TEXT NOT NULL,
  parent_org_id         TEXT REFERENCES orgs(id),  -- NULL except for MSP client sub-tenants
  tier                  TEXT NOT NULL DEFAULT 'community', -- named tier; drives capabilities
  plan                  TEXT,                 -- migrated from accounts (0023): free | hobby | ...
  stripe_customer_id    TEXT,                 -- moved from accounts (0023 R2)
  stripe_subscription_id TEXT,
  subscription_status   TEXT,
  ledger_visibility     TEXT NOT NULL DEFAULT 'public', -- public | private
  created_at            INTEGER NOT NULL
);
CREATE TABLE memberships (
  id           TEXT PRIMARY KEY,     -- mem_<rand>
  org_id       TEXT NOT NULL REFERENCES orgs(id),
  account_id   TEXT NOT NULL REFERENCES accounts(id),
  role         TEXT NOT NULL,        -- owner | admin | member | viewer
  created_at   INTEGER NOT NULL,
  UNIQUE (org_id, account_id)
);
CREATE TABLE invitations (
  id           TEXT PRIMARY KEY,     -- inv_<rand>, also the accept-link slug
  org_id       TEXT NOT NULL REFERENCES orgs(id),
  email        TEXT NOT NULL,
  role         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  accepted_at  INTEGER,
  UNIQUE (org_id, email)
);
CREATE TABLE share_tokens (
  id           TEXT PRIMARY KEY,     -- sha256(token); the token itself is never stored
  org_id       TEXT NOT NULL REFERENCES orgs(id),
  scope        TEXT NOT NULL,        -- ledger | evidence  (read-only)
  created_by   TEXT NOT NULL REFERENCES accounts(id),
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,              -- NULL = no expiry
  revoked_at   INTEGER
);
```

The attestation ledger from [[spec 0012]] is unchanged in shape; its ownership column
(`tenant_id`, which since [[spec 0014]] R2 already holds an account id) is
**re-interpreted as an org id** by the migration below — chains, `seq`, and signatures are
untouched, so no attestation is re-hashed or re-signed.

### RBAC: four roles, enforced at the boundary

A small, fixed role lattice — each role a strict superset of the next:

| Role | Ledger / attestations | Members & invites | Billing | Org settings & SSO | Delete org |
|---|---|---|---|---|---|
| `owner` | read + attest | manage | manage | manage | yes |
| `admin` | read + attest | manage (not owners) | read | manage | no |
| `member` | read + attest | read | — | — | no |
| `viewer` | **read only** | read | — | — | no |

Enforcement is a **single choke point**, not scattered checks: every authenticated
request resolves `(account → membership → org, role)` once, and a `require(role ≥ X)` /
`require(capability C)` guard sits in front of each handler. A `viewer` calling any
mutating route (attest submit on behalf of the org, invite, billing, settings) gets `403`;
a non-member of the org gets `403` (never `404`-leak beyond what public visibility already
allows). Attestation *submission* is a write, so `viewer` cannot attest; `member` and up
can. There is exactly **one** `owner`-only destructive action set (delete org, transfer
ownership).

The CLI path is unaffected: an API key still belongs to an account, the account has one
membership, and the key inherits that membership's role for org-scoped writes. A key
minted by a `viewer` cannot attest into the org ledger.

### MSP tenancy: org-of-orgs over 0012's isolation

An **MSP** is modelled as a managing org whose `orgs.parent_org_id` points at it from each
**client sub-tenant** org. Each client sub-tenant is a *full org* with its **own isolated
ledger** — reusing [[spec 0012]]'s per-tenant hash chain verbatim, now keyed by the
sub-tenant org id. The isolation guarantee is the load-bearing property: **a member of
client sub-tenant A cannot read client sub-tenant B's ledger**, even though both are
managed by the same MSP. The MSP's own staff read *across* their sub-tenants only through
memberships the MSP holds (a managing membership at the parent), never by ambient
authority.

Whether MSP tenancy should be **org-of-orgs** (chosen here — each client is a real,
independently-billable, independently-attesting org, which composes cleanly with
[[spec 0023]] and lets a client "graduate" to a direct customer by re-parenting) versus a
**flat single org with tenant-labelled ledgers** (fewer rows, but every isolation check
becomes a `WHERE tenant_label = ?` that is easy to forget) is called out in Open
questions. The design proceeds on org-of-orgs because it makes isolation a *foreign-key
and membership* fact rather than a filter every query must remember.

### Private ledgers + share tokens (unblocks 0019's URL)

`orgs.ledger_visibility` gates the public read routes from [[spec 0012]] R5
(`GET /v1/attestations/:id`, `GET /v1/tenants/:id/ledger`):

- `public` (default, preserves today's behaviour): anyone with the id can read — the
  Community/Hobby experience is unchanged.
- `private` (a gated capability): those routes require either an org membership *or* a
  valid **share token**.

A **share token** is a high-entropy secret handed out-of-band; only its `sha256` is stored
(mirroring API-key storage, [[spec 0012]] R2 / [[spec 0014]] R1). It grants **read-only**,
**scoped** (`ledger` or `evidence`), optionally-expiring, **revocable** access to exactly
one org's ledger or evidence pack — and nothing mutating, ever. Presenting it as
`?token=` (or an `Authorization` header) on a private org's read route returns that org's
data; presenting it against any other org returns `403`.

This directly **unblocks [[spec 0019]]'s deferred shareable evidence-pack URL**: the
evidence pack ([[spec 0019]] R2/R3) gains a public route reachable with an `evidence`-scoped
share token, so an auditor can open a link instead of being emailed a file — while the
pack's **offline self-verifiability is unchanged** (the reader still trusts the
cryptography, not the URL; [[spec 0019]] R3/R4 notices ride along verbatim). Revoking the
token (or its expiry) closes the link; the underlying attestations are untouched
(append-only, [[spec 0012]] R4).

### SSO/SAML — tier-gated sign-in

Sign-in ([[spec 0014]] R4) gains a third provider: **SAML 2.0 SSO** against a customer IdP,
configured per org (IdP metadata / entity id / signing cert in an `org_sso` settings blob,
managed by `owner`/`admin`). It is **only available when the org's tier grants the `sso`
capability** — attempting to configure or use it on a tier without that capability is
refused with a clear "not available on the <tier> tier" message. When enabled, members of
that org may sign in via the IdP; GitHub/magic-link remain available unless the org
restricts to SSO-only. No SCIM (Non-goal). Session issuance, CSRF, and cookie flags are
inherited unchanged from [[spec 0014]] R4.

### Tier-capability gating mechanism (no numbers)

Capabilities are **not** keyed off tier names at call sites. Instead a single
**capability map** — tier name → set of enabled capability flags — is Worker **config**
(a `var`, editable without a code change, exactly as [[spec 0023]] R1 keeps the price id in
a `var`). Handlers ask `orgHasCapability(org, "private_ledger")`; they never test
`org.tier == "team"`. The capability keys defined by this spec are:

`multi_member` · `rbac` · `private_ledger` · `share_tokens` · `shareable_evidence` ·
`sso` · `msp_subtenants` · `seat_billing` · `annual_billing`.

Which tiers switch which capabilities on — and any per-tier seat counts — is a
`marketing/pricing.md` decision expressed as config values, deliberately **absent from this
spec**. This keeps the public spec free of packaging while making the mechanism testable:
flipping a capability for a tier is a config edit, not a code edit.

### Billing extension to seats/quantities/annual (mechanism only)

[[spec 0023]] built subscription rails with a single flat-rate monthly Price and one
`quantity: 1` line item. This spec extends that **conceptually** to multi-seat and annual,
**reusing the same machinery** ([[spec 0023]] R3–R5): the org's subscription carries a
**quantity = billable seat count** (derived from active memberships in seat-billed roles),
and a tier may have **monthly and annual** Prices selected by the same `var`-indirection
[[spec 0023]] R1 already uses. Seat changes update the subscription quantity; the **webhook
remains the single source of truth** for entitlement ([[spec 0023]] R5), now also
reconciling seat count and interval. **No prices, seat minimums, or intervals-with-numbers
appear here** — only that the mechanism carries a quantity and an interval, and that the
active Price ids stay config `var`s so test→live and tier changes are config swaps.

### Backward compatibility — every account becomes a one-member org

The migration is **additive and transparent** (in the spirit of [[spec 0014]] R1's legacy
copy-across):

1. For each existing `account`, create an `org` (name derived from the account),
   **migrate the billing/plan/tier columns** from the account onto the org, and set the
   ledger `visibility = public` (today's behaviour).
2. Create one `membership` joining that account to its new org as **`owner`**.
3. Re-interpret the ledger's ownership column as the org id (the account id and the new
   org id are wired 1:1 by the migration, so no attestation row is rewritten, re-hashed,
   or re-signed — chains and [[spec 0012]] verification stay valid).

After migration a lone user is an `owner` of a one-member org and notices **nothing**: the
portal, `salvage login`, existing API keys, `salvage attest`/`salvage verify`, the public
ledger URL, and the evidence pack all behave exactly as before. Multi-member, private
ledgers, SSO, and MSP sub-tenants are strictly *additive* capabilities that a lone
Community/Hobby org simply never exercises.

## Requirements

**R1 — Org + membership + roles (migration 0007, additive).** There MUST be an `orgs`
table (owning ledger, attestations, billing, settings, tier), a `memberships` table
joining `accounts` to `orgs` under a `role` in `{owner, admin, member, viewer}` (unique per
`(org_id, account_id)`), and an `invitations` mechanism to add a person by email under a
role. The migration MUST be additive: no existing table is dropped, and the billing columns
from [[spec 0023]] R2 move onto the org.

**R2 — RBAC enforcement at every boundary.** Every org-scoped request MUST resolve
`(account → membership → org, role)` and pass a role/capability guard **before** any read
or mutation of ledger, attestations, members/invitations, billing, or settings. A `viewer`
MUST be refused (`403`) on **every** mutating route, including attesting into the org
ledger; a non-member MUST be refused (`403`) on private-org reads and on all org mutations.
Exactly one destructive action set (delete org / transfer ownership) MUST be `owner`-only.
The role lattice MUST be a strict superset chain `owner ⊇ admin ⊇ member ⊇ viewer`.

**R3 — MSP sub-tenant isolation (builds on [[spec 0012]]).** An org MAY manage client
sub-tenant orgs via `parent_org_id`. Each sub-tenant MUST have its **own** [[spec 0012]]
per-tenant hash-chained ledger. A member of one sub-tenant MUST NOT be able to read or
mutate another sub-tenant's ledger, attestations, evidence, or settings — even under the
same MSP. MSP staff MUST reach a sub-tenant only through an explicit membership (direct or
managing), never by ambient parent authority. Managing an MSP requires the `msp_subtenants`
capability (R7).

**R4 — Private ledger + share-token URLs.** An org MUST support `ledger_visibility ∈
{public, private}`. When `private`, the [[spec 0012]] R5 public read routes MUST require an
org membership **or** a valid **share token**. A share token MUST be stored only as its
`sha256`, MUST be **read-only** and **scoped** (`ledger` | `evidence`), MAY expire, MUST be
revocable, and MUST grant access to **exactly one org** (presenting it against any other org
is `403`). No share token may authorize any mutation. `private` requires the
`private_ledger` capability; share tokens require `share_tokens` (R7).

**R5 — Shareable evidence-pack URL (unblocks [[spec 0019]]).** The evidence pack
([[spec 0019]] R2/R3) MUST be reachable via a public route authorized by an
`evidence`-scoped share token (R4), so an auditor can open a link with no account. The
pack's **offline self-verifiability and honest-scope notice** ([[spec 0019]] R3/R4) MUST be
carried unchanged. Revoking or expiring the token MUST close the URL without altering any
attestation. This route is gated by the `shareable_evidence` capability (R7).

**R6 — SSO/SAML, tier-gated.** An org whose tier grants the `sso` capability MUST be able
to configure a SAML 2.0 IdP (managed by `owner`/`admin`) and let its members sign in through
it, alongside the existing GitHub/magic-link providers ([[spec 0014]] R4). Configuring or
using SSO on a tier **without** the `sso` capability MUST be refused with a clear
tier-availability message. Session/CSRF/cookie handling MUST be inherited unchanged from
[[spec 0014]] R4. SCIM/directory sync is out of scope.

**R7 — Tier-capability gating mechanism (no numbers).** Capabilities MUST be enabled per
named tier via a **config capability map** (a Worker `var`), not by hard-coded tier-name
comparisons at call sites. Handlers MUST test capability flags
(`multi_member`, `rbac`, `private_ledger`, `share_tokens`, `shareable_evidence`, `sso`,
`msp_subtenants`, `seat_billing`, `annual_billing`), never a literal tier name. The mapping
of tiers→capabilities and any seat counts are **config/marketing values and MUST NOT appear
in this spec or in code as constants**. Changing what a tier unlocks MUST be a config edit,
not a code edit.

**R8 — Billing extension to seats/quantities (mechanism only).** The [[spec 0023]]
subscription machinery MUST be extended so an org's subscription carries a **quantity =
billable seat count** derived from active seat-billed memberships, and MAY select a
**monthly or annual** Price via the existing `var`-indirected price ids ([[spec 0023]] R1).
Seat/interval changes MUST update the subscription quantity/price, and the **signed webhook
MUST remain the single source of truth** for entitlement and seat/interval reconciliation
([[spec 0023]] R5). **No prices, seat minimums, or numeric intervals** may appear; only the
mechanism (a quantity and an interval, selected by config).

**R9 — Single-user backward-compat migration.** The migration MUST turn each existing
[[spec 0014]] account into a **one-member org** with that account as `owner`, moving the
billing/plan/tier columns onto the org and re-interpreting the ledger ownership column as the
org id **without rewriting, re-hashing, or re-signing any attestation** ([[spec 0012]]
chains stay valid). After migration, a lone user's portal, CLI auth, existing API keys,
`attest`/`verify`, public ledger URL, and evidence pack MUST behave exactly as before, with
the wire protocol unchanged.

## Open questions

- **Org-of-orgs vs flat tenants for MSP.** This spec chooses org-of-orgs (isolation is a
  foreign-key + membership fact; clients are independently billable and can be re-parented).
  A flat "one org, tenant-labelled ledgers" model has fewer rows but makes every isolation
  check a filter that is easy to forget. Revisit if row/query overhead of org-of-orgs proves
  material at MSP scale.
- **Per-target / custom roles.** v1 ships four fixed org-wide roles. Do larger tiers need
  per-target scoping (e.g. a viewer limited to a subset of targets) or user-defined roles?
  Deferred; would extend `memberships`, not replace it.
- **Seat definition for billing.** Which roles count as billable seats (e.g. do `viewer`s
  consume a seat?) is a `marketing/pricing.md` decision; R8 only requires the quantity be
  *derivable from memberships*.
- **Share-token granularity.** One token per org+scope (chosen) vs per-target or per-time-
  window tokens. Start coarse; narrow if auditors ask for scoped links.
- **SSO-only enforcement & JIT membership.** Should an SSO org be able to force SSO-only
  sign-in and auto-create a `member` on first IdP login (JIT), short of full SCIM? Likely
  yes for the larger tiers; specify with the SSO capability rollout.
- **Ownership transfer & last-owner safety.** Exact UX for transferring `owner` and
  preventing an org from being left with zero owners (or an MSP sub-tenant orphaned when a
  managing membership is removed).

## Acceptance criteria

Behaviour/capability based — never revenue or price based.

1. **Backward compat.** An existing single-user [[spec 0014]] account, post-migration,
   keeps working unchanged: it is the `owner` of a one-member org, its existing API keys
   still attest, its public ledger URL and evidence pack are unchanged, and no attestation
   fails [[spec 0012]] verification.
2. **RBAC read/write split.** A `viewer` role **cannot mutate the ledger** (attest,
   invite, change billing, or edit settings) — every such attempt is `403` — while it can
   read the org's ledger and members. A `member` can attest but cannot manage members or
   billing. Only an `owner` can delete the org / transfer ownership.
3. **MSP isolation.** Under one MSP, a member of client sub-tenant A **cannot read**
   sub-tenant B's ledger, attestations, or evidence; MSP staff reach a sub-tenant only via an
   explicit membership. Each sub-tenant's [[spec 0012]] chain verifies independently.
4. **Private ledger + share token.** Setting an org `private` makes its [[spec 0012]] read
   routes `403` for non-members and non-token requests; a valid, in-scope, unexpired share
   token reads that org's ledger, and the **same token against another org is `403`**.
   Revoking/expiring the token closes access with no change to stored attestations.
5. **Shareable evidence URL.** An `evidence`-scoped share token opens the [[spec 0019]]
   evidence pack with no account; the pack still verifies **offline** and carries the
   honest-scope notice; revoking the token closes the URL.
6. **SSO tier gate.** An org on a tier that grants `sso` can configure and sign in via
   SAML; an org on a tier without `sso` is refused configuration/use with a clear
   tier-availability message — with **no price shown**.
7. **Capability mechanism.** Flipping a capability for a tier is a **config change only**
   (no code edit): toggling `private_ledger` off for a tier immediately makes that tier's
   orgs unable to go private, and no handler references a literal tier name.
8. **Seat/interval mechanism.** Adding a seat-billed member updates the subscription
   **quantity** and the signed webhook reconciles entitlement; switching a tier between
   monthly and annual is a config `var` swap — all verified in Stripe **test mode**, with
   **no price asserted** by the test.
