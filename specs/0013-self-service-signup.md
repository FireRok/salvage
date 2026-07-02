# 0013 — Self-service signup (attestation free tier)

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

The notary ([[spec 0012]]) launched with concierge-only onboarding (`newtenant.sh`).
That is right for the first few design partners but does not scale to inbound. This
spec adds a self-service free-tier signup: a public form that mints a free tenant
keyed by a verified email, protected against abuse, delivering the API key by email.

Billing stays out of scope — the free tier is the credibility on-ramp; paid
conversion is still a conversation, and the attestation count is already the meter.

## Goals

- A public `GET /signup` form and `POST /v1/signup` that creates a **free** tenant and
  delivers its API key to the submitted email.
- Abuse resistance: Cloudflare **Turnstile** + per-IP rate limiting + one-tenant-per-email.
- Operable customer records: `email`, `status`, per-tenant `cap_override`, `created_via`.
- Ship dormant and safe: signup is **gated off** until Turnstile + email are provisioned.

## Non-goals

- Billing / Stripe / paid signup (concierge upgrades a tenant's `plan` to `paid`).
- Email verification beyond "the key is only delivered to the address" (possession = proof).
- Accounts/login/dashboards — the API key is the credential.

## Requirements

**R1 — Schema (migration 0002).** `tenants` gains `email` (unique when non-null, so
concierge rows are exempt), `status` (`active|suspended`, default active),
`cap_override` (nullable per-tenant monthly cap), `created_via` (`concierge|signup`).
A `signup_attempts(id, ip, email, outcome, created_at)` table logs every attempt for
rate limiting + abuse review.

**R2 — Effective cap + status.** Attestation submit MUST reject a `suspended` tenant
(403) and, for free tenants, use `cap_override` when set, else the global
`FREE_MONTHLY_CAP`.

**R3 — `POST /v1/signup`.** Body `{email, turnstile_token}`. Order: gate check → per-IP
rate limit → email format → Turnstile (when configured) → one-per-email → create free
tenant (`created_via=signup`) → deliver key → log outcome. Every branch records a
`signup_attempts` row (`created|duplicate|turnstile_fail|rate_limited|error`).

**R4 — Anti-abuse.** Per-IP limit (default 5/hour via `signup_attempts`), Turnstile
enforced whenever `TURNSTILE_SECRET` is set (siteverify), one free tenant per email
(409 on duplicate). The API key is delivered out-of-band (email), never guessable.

**R5 — Key delivery.** Prefer email via the Cloudflare Email `send_email` binding
(`EMAIL`); the raw key is emailed, never stored (only its hash). A `SIGNUP_SHOW_KEY`
escape hatch returns the key in the HTTP response so signup works *before* email is
onboarded — it MUST be turned off once email is live. On delivery failure the tenant
row is rolled back so the address can retry.

**R6 — Safe rollout gate.** Signup is inert unless `SIGNUP_ENABLED="true"`; otherwise
`POST /v1/signup` returns 503. This keeps an unprotected key-minting endpoint from
being live before Turnstile + email exist. `GET /signup` may still render.

**R7 — Concierge preserved.** `scripts/newtenant.sh` still works; existing NULL-email
tenants are unaffected.

## Deploy steps (external — need dashboard or a broader CF token)

The Worker code + schema deploy with the standard token, but two provisions need
Turnstile:Edit + DNS:Edit (or dashboard), which the default wrangler OAuth lacks:

1. **Email sending:** onboard `salvage.sh` (Dashboard → Email Service → Email Sending →
   Onboard domain, adds SPF/DKIM) or `wrangler email sending enable salvage.sh` on
   wrangler ≥4.106. Add `"send_email": [{ "name": "EMAIL" }]` to wrangler.jsonc, set
   `SIGNUP_FROM` (e.g. `welcome@salvage.sh`), then `SIGNUP_SHOW_KEY="false"`.
2. **Turnstile:** create a widget (Dashboard → Turnstile) for `salvage.sh`; set
   `TURNSTILE_SITEKEY` var (public) + `wrangler secret put TURNSTILE_SECRET`.
3. Flip `SIGNUP_ENABLED="true"` and redeploy.

## Acceptance criteria

1. With signup enabled + `SHOW_KEY`, `POST /v1/signup` creates a free tenant and returns
   a working key; that key can immediately `POST /v1/attestations`.
2. A duplicate email → 409; an invalid email → 400.
3. Over the per-IP limit → 429; every attempt is logged in `signup_attempts`.
4. When `TURNSTILE_SECRET` is set, a missing/invalid token → 400.
5. With `SIGNUP_ENABLED` unset, `POST /v1/signup` → 503 (dormant).
6. A `suspended` tenant cannot attest (403); a `cap_override` changes that tenant's cap.
