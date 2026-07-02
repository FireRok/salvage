# 0023 — Billing & subscriptions (Hobby tier, self-serve)

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

The notary ([[spec 0012]]) has accounts, per-account usage, and a free monthly
attestation cap ([[spec 0014]]). To let **organic signups convert without any
manual step**, we add a single cheap paid tier — **Hobby, $5/mo** — as a
self-serve subscription. The pricing page's Team/Business/MSP tiers come later as
*additional Prices* against the same machinery; this spec builds the rails with
one price.

Billing on Cloudflare Workers is serverless and effectively free to run, so this
does **not** breach the standing "keep the environment small until a paying
customer / don't build the expensive hosted-exec tier" constraint — it is the
rail that lets the *first* payment happen, not a compute build-out.

Model decided via Stripe's implementation planner (`optimize_subscriptions`):
**Stripe-hosted Checkout (`mode: subscription`)** for signup + **hosted Customer
Portal** for self-serve manage/cancel/update-card + **freemium** (the free tier
stays; upgrade is manual) + **Smart Retries** for dunning. Flat-rate price, no
trial.

## Where it lives

Entirely in the proprietary notary Worker (`salvage-attest`, Cloudflare Worker +
D1) — like [[spec 0012]]/[[spec 0013]]/[[spec 0014]]/[[spec 0015]], the OSS
`salvage` repo carries none of it. The CLI/attestation wire protocol is
unchanged.

## Non-goals (v1)

- Team/Business/MSP tiers, seat/quantity pricing, annual plans — later Prices,
  same flow.
- Trials, coupons, proration UX, tax (Stripe Tax) — deferred; Smart Retries +
  hosted Portal cover dunning/management with zero code.
- Bundling the Stripe Node SDK. The Worker calls the Stripe REST API with `fetch`
  (form-encoded) and verifies webhooks with **WebCrypto** — consistent with the
  existing minimal-dependency, WebCrypto-Ed25519 style.

## Requirements

**R1 — Stripe objects.** A Product "Salvage Hobby" with a **recurring monthly
Price of $5.00 USD**, created in **test mode first**, then an equivalent live
Price at go-live. The active price id is a Worker **var** `STRIPE_PRICE_HOBBY`
(not hard-coded), so test→live is a config swap. No `payment_method_types` is
ever passed (dynamic payment methods).

**R2 — Identity schema (migration 0006, additive).** `accounts` gains
`stripe_customer_id`, `stripe_subscription_id`, `subscription_status` (Stripe's
status string, nullable). The existing `plan` column holds `free` | `hobby`.
Nothing is dropped.

**R3 — Upgrade flow.** `POST /portal/upgrade` (session-authed, CSRF-checked):
resolve-or-create the account's Stripe **Customer** (store `stripe_customer_id`,
set customer `metadata.account_id`), create a **Checkout Session**
(`mode=subscription`, `line_items:[{price: STRIPE_PRICE_HOBBY, quantity:1}]`,
`client_reference_id=account_id`, `customer=<id>`, success/cancel URLs back to the
portal), and 303-redirect to the session URL. No card is collected until the user
chooses to upgrade (freemium).

**R4 — Self-serve management.** `POST /portal/billing` (session + CSRF) creates a
**Billing Portal Session** for the account's customer and redirects to it — the
hosted page handles cancel, update card, and invoice history. No custom
subscription-mutation code.

**R5 — Webhook sync.** `POST /stripe/webhook` is the **only** source of truth for
plan state (never trust the Checkout redirect). It MUST:
- **Verify the Stripe signature** (`Stripe-Signature` header) via WebCrypto
  HMAC-SHA256 over `"{t}.{payload}"` against `STRIPE_WEBHOOK_SECRET`, with a
  timestamp tolerance (≤5 min); reject (400) otherwise. Read the **raw** body for
  verification before parsing.
- Be **idempotent** — record handled Stripe `event.id` (a `stripe_events` table
  or an upsert guard) and no-op on replay.
- Handle: `checkout.session.completed` (link customer+subscription, set
  `plan=hobby`, status active); `customer.subscription.updated` (map status →
  `active`/`trialing` ⇒ hobby, `past_due` ⇒ keep hobby but record status,
  `canceled`/`unpaid` ⇒ revert `free`); `customer.subscription.deleted` (revert
  `free`); `invoice.payment_failed` (record status; Smart Retries drives the
  rest). Unhandled event types return 200 (acknowledged, ignored).

**R6 — Plan → entitlement.** Effective cap resolution ([[spec 0014]] R3) gains a
plan mapping: `free` ⇒ the global free cap (30/mo today), `hobby` ⇒ uncapped
(attestations are cheap D1 writes; a numeric cap can be added later without
schema change). `cap_override` still wins when set.

**R7 — Portal surface.** `/portal` shows the current plan + subscription status.
Free accounts see an **"Upgrade to Hobby — $5/mo"** button (→ R3). Paid accounts
see **"Manage billing"** (→ R4) and their status. Consistent with the marketing
theme.

**R8 — Security.** The Worker uses a **restricted API key** (`rk_`, least
privilege: write Checkout Sessions + Billing Portal Sessions + Customers, read
Subscriptions/Prices), stored as Worker secret `STRIPE_SECRET_KEY` — never in
code, never logged. `STRIPE_WEBHOOK_SECRET` likewise. Every webhook is
signature-verified (R5). Separate test and live keys. Build and validate in
**test mode** before any live key is installed.

**R9 — No heavy dependencies.** Stripe REST via `fetch` (form-encoded bodies),
WebCrypto for signatures. No Stripe SDK; the Worker's dependency posture is
unchanged.

## Design notes

- **Why webhook-authoritative:** the Checkout success redirect can be dropped,
  refreshed, or spoofed; only the signed webhook reliably flips entitlement.
- **Customer reuse:** one Stripe Customer per account (metadata `account_id`),
  created lazily on first upgrade, so Portal + future invoices are coherent.
- **Form-encoding:** Stripe's API takes `application/x-www-form-urlencoded`;
  nested params use `a[b]=c` and array items `line_items[0][price]=...`.

## Open questions

- **Hobby cap value** — uncapped (chosen default, revisit if abused) vs a generous
  numeric cap (e.g. 1000/mo). No schema change either way.
- **Turnstile on `/portal/upgrade`** — probably unnecessary (session-authed), note
  and skip.
- **Tax** — Stripe Tax deferred until revenue warrants registration.

## Acceptance criteria

1. **Test mode, end-to-end:** a free account clicks Upgrade → Stripe Checkout →
   test card `4242 4242 4242 4242` → `checkout.session.completed` webhook flips
   the account to `hobby` and lifts the cap; the portal shows "Manage billing".
2. **Cancel:** via the Billing Portal → `customer.subscription.deleted` webhook
   reverts the account to `free` and restores the cap.
3. **Security:** a webhook with a bad/missing signature is rejected 400 and
   changes nothing; a replayed event id is a no-op.
4. **Config swap:** switching `STRIPE_PRICE_HOBBY` + the keys from test to live is
   the only change needed to go live; no code edit.
