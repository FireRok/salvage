# 0014 — Accounts, API keys, and CLI auth

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Signup ([[spec 0013]]) gave each tenant a single mailed key. Real use needs an
*account* that holds **multiple named, revocable keys**, a **portal** to manage them,
an interactive **`salvage login`** (browser device flow) for humans, and unattended
**portal-generated keys** for CI/servers. This spec introduces the identity model and
both auth paths. Auth providers: **GitHub OAuth** (no email/Turnstile dependency) and
**email magic-link** (reuses the email infra); either alone is sufficient to sign in.

## Goals

- `accounts` + `api_keys` (many per account, hashed, prefix-for-display, revocable,
  last-used) + `sessions` + `device_auth`, replacing one-key `tenants`.
- `salvage login` — OAuth 2.0 Device Authorization Grant (RFC 8628): CLI gets a code,
  the browser approves, the CLI stores a key in `~/.salvage/credentials`.
- A portal: sign in (GitHub or magic-link), generate/list/revoke keys, see usage vs cap.
- Attestation auth unchanged on the wire (`Bearer <key>`), resolving `api_keys → account`.

## Non-goals

- Orgs/teams/RBAC, billing, password auth. One account = one human (or one project).
- Replacing concierge (`newtenant.sh`) — it still works.

## Requirements

**R1 — Identity schema (migration 0003, additive).** `accounts(id, email unique,
github_id unique, github_login, plan, status, cap_override, created_at, created_via)`;
`api_keys(id, account_id, name, key_hash unique, key_prefix, created_at, last_used_at,
revoked_at)`; `sessions(id=sha256(token), account_id, csrf, created_at, expires_at)`;
`device_auth(id=sha256(device_code), user_code, account_id, status, created_at,
expires_at)`; `magic_tokens(id=sha256(token), email, created_at, expires_at, used_at)`.
Existing tenants are copied across, **reusing tenant ids as account ids**, and each
tenant's key becomes one `api_keys` row (`legacy`).

**R2 — Drop the stale ledger FK (migration 0004).** `attestations.tenant_id` was a FK
to `tenants(id)`; it now holds an account id. D1 enforces FKs, so the table is rebuilt
(data preserved) without the constraint. The column keeps its name (holds an account id).

**R3 — Attestation auth via keys.** `Bearer <key>` resolves through `api_keys` (not
revoked) → `accounts`; `last_used_at` is stamped. Suspended account → 403; effective cap
= `cap_override` else global. A revoked key → 401.

**R4 — Sign-in.** `GET /login` offers GitHub (`/auth/github` → callback) and email
magic-link (`POST /auth/magic` → `GET /auth/magic/callback`). Either creates/looks up an
account and starts a session (HttpOnly, SameSite=Lax, Secure in prod; per-session CSRF).
Providers are optional: GitHub is active only when `GITHUB_CLIENT_ID/SECRET` are set;
magic-link needs the email binding (or `MAGIC_DEV` returns the link locally).

**R5 — Portal.** `GET /portal` (session) shows account + usage vs cap + keys (name,
prefix, last-used, revoke). `POST /portal/keys` mints a key shown **once**; revoke marks
`revoked_at`. All mutating POSTs are CSRF-checked.

**R6 — Device flow.** `POST /device/code` → `{device_code, user_code, verification_uri,
verification_uri_complete, interval, expires_in}`. `POST /device/token` polls:
`authorization_pending` until the user approves at `GET /activate?user_code=` (session
required) via `POST /device/approve`; then it mints a key once (`cli-login <date>`) and
returns it, marking the request `claimed`. Codes expire (~10 min).

**R7 — CLI.** `salvage login [-endpoint]` runs the device flow, opens the browser
(skippable with `SALVAGE_NO_BROWSER=1`), polls, and stores `{endpoint, api_key}` in
`~/.salvage/credentials` (0600). `salvage logout` removes it. `salvage attest` resolves
endpoint+key as flags → config → stored credentials, so a logged-in user needs neither.

## Acceptance criteria

1. A migrated legacy key still attests; a revoked key → 401; a suspended account → 403.
2. Magic-link sign-in → portal → generate key → that key attests.
3. `salvage login` (approved in the browser) stores a key; `salvage attest` then submits
   with no `-endpoint`/env key; `salvage logout` removes it.
4. A device token cannot be claimed twice (`already_claimed`).
5. GitHub OAuth sign-in creates/looks up an account and reaches the portal (needs the
   registered OAuth app).
