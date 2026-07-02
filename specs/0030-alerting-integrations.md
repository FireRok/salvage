# 0030 — Alerting integrations

- **Status:** Implemented — client hooks in the CLI (R1/R2/R7-client); hosted destinations in the notary service (R3-R6, R8, R7-hosted); signed webhook payloads and summary-only mode remain open questions
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage already knows *how to notice* a problem on both sides of the product, but
it can only tell you about one of them, and only over email.

- **Client side.** [[spec 0007]] (Proposed) fixes the local contract: Salvage is a
  one-shot CLI with a meaningful exit code and **no daemon** (0007 R1), plus an
  optional `on_fail`/`on_success` hook (0007 R4). That spec deliberately leaves the
  *concrete* channel open ("`on_fail` webhook/command hook", payload shape an Open
  question — "reuse the report JSON?"). It defines the contract; it does not
  realize a channel.
- **Hosted side.** [[spec 0015]] shipped the dead-man's-switch: the notary knows
  each monitored target's expected interval and alerts when an attestation is
  overdue — the "your verification silently stopped" signal that a client-side cron
  can never self-report (a dead box sends nothing). But 0015 delivery is
  **email-only** (0015 R6, and 0015 Non-goals: "Paging/SMS/Slack — email only in
  v1 … Webhooks later"). A team with an existing incident pipeline — an on-call
  rotation, a Slack channel, a pager — cannot route that signal into it. The one
  alert that most warrants paging someone lands in an inbox.

This spec realizes the concrete outbound channels for **both** sides. It does not
re-open 0007's no-daemon posture, nor 0015's cron-trigger architecture; it plugs
delivery adapters onto contracts those specs already fixed.

**Division of labor (stated so the three specs do not overlap confusingly):**

| Spec | Owns |
|---|---|
| [[spec 0007]] | The **local** contract: exit codes, retention, the `on_fail`/`on_success` hook *interface*. One-shot CLI, no daemon. |
| [[spec 0015]] | The **hosted heartbeat**: monitor model, overdue evaluation, alert-once-per-transition, the cron trigger. |
| **0030 (this spec)** | The **concrete channels** for both: what an `on_fail` hook is invoked with, and how the hosted dead-man's-switch reaches a webhook / Slack / PagerDuty in addition to email. |

Nothing here changes the *decision* to alert (that stays in 0007/0015); it changes
only *where the alert goes* and *what shape it arrives in*.

## Goals

- **Client-side realization of the 0007 hook.** On a `fail` (or operational error)
  the CLI invokes the operator-configured `on_fail` command/URL with the run's
  report JSON as payload; symmetrically `on_success`. No daemon, no new transport
  beyond invoking the hook the operator named (consistent with 0007 R1/R4 and the
  Unix-composition posture of `[[spec 0005]]`-lineage design).
- **Hosted-side realization of the 0015 switch.** The dead-man's-switch delivers an
  overdue (and recovery) alert to a **per-target / per-tenant** configured generic
  webhook, Slack, and PagerDuty destination **in addition to** the existing email
  path — which stays exactly as 0015 R6 specified.
- **One canonical payload.** The generic webhook payload **is** the versioned
  report / attestation JSON — no bespoke alert envelope — so a receiver written
  once keeps working. Slack and PagerDuty are *formatting adapters* layered over
  that same generic delivery, not independent integrations.
- **Reliable hosted delivery.** Retry with backoff, and de-duplication / flap
  control so a target oscillating around its deadline does not page an on-call
  rotation repeatedly.
- **Secrets by reference.** Every destination secret (webhook URL with an embedded
  token, Slack/PagerDuty routing key) is stored and handled **by reference**, never
  embedded in a report or attestation body — consistent with [[spec 0003]].

## Non-goals

- **Re-litigating architecture.** No client daemon (0007 R1); no change to 0015's
  Cloudflare Cron-Trigger model or its monitor schema beyond adding destination
  rows. This spec adds channels, not a runtime.
- **Cadence inference / new monitor semantics.** Overdue evaluation and
  alert-once-per-transition are inherited unchanged from 0015 R3/R4. This spec
  reacts to the *transition* 0015 already computes.
- **SMS / phone / email-provider fan-out.** PagerDuty (and its own downstream
  paging) covers the "wake someone up" need; native SMS is deferred.
- **A bespoke alert schema.** We explicitly do **not** invent an alert-specific JSON
  envelope. The payload is the versioned report/attestation ([[spec 0002]],
  [[spec 0026]]); adapters reshape it for a vendor, they do not replace it.
- **Inbound / bidirectional integrations** (e.g. acking a PagerDuty incident back
  into Salvage). One-way outbound only in v1.

## Design

### A. Client side — the `on_fail` / `on_success` hook (realizing 0007 R4)

The CLI gains an optional `alerts` block:

```yaml
alerts:
  on_fail: "./notify.sh"              # command, or an https:// URL
  on_success: "https://hooks.example/salvage?token_ref=env:SALVAGE_HOOK_TOKEN"
```

- **When.** After the run's verdict is finalized and the report is written
  ([[spec 0007]] R3), the CLI fires `on_fail` on a `fail` verdict **or** an
  operational error (exit 2), and `on_success` on a `pass`. Exit-code composition
  stays primary (0007 R4: "exit code first, hook second"); the hook is best-effort
  and its own failure does not change the run's exit code (it is logged).
- **Payload.** The invoked hook receives the **run report JSON** — the same bytes
  Salvage wrote to disk. For a **command** hook, the JSON is delivered on stdin
  (and the report path in `$SALVAGE_REPORT`). For a **URL** hook, the JSON is
  `POST`ed with `Content-Type: application/json`.
- **Schema recommendation.** The report SHOULD carry the **versioned** schema of
  [[spec 0026]] (a top-level `schema_version`) so a hook receiver — often the same
  code that consumes the hosted webhook below — binds to a stable contract rather
  than an unversioned snapshot. This is a recommendation, not a hard gate: an
  operator on an older report shape still gets a hook fire.
- **No daemon.** The hook runs inline within the one-shot process and the process
  still exits; there is no resident agent (0007 R1).

### B. Hosted side — dead-man's-switch destinations (extending 0015)

[[spec 0015]] R4 computes the `ok → overdue` and `overdue → ok` transitions on the
hourly cron tick. This spec adds, per monitor, a set of **destinations** that a
transition fans out to. Email remains the default and is unchanged.

A `monitor_destinations` table (one-to-many with 0015's `monitors`):

```
monitor_destinations(
  id, monitor_id  -> monitors.id,
  kind            in ('email','webhook','slack','pagerduty'),
  target_ref,          -- BY REFERENCE: secret-store handle, not the secret itself
  enabled,
  created_at
)
```

- A monitor with **no** destination rows behaves exactly as 0015 today: it emails
  `contact_email`. So the email path is strictly additive/back-compatible.
- Destinations are per-target (per `monitors` row) and therefore per-tenant, created
  and deleted from the portal ([[spec 0015]] R7), CSRF-checked.
- On a transition, the switch renders the payload once (below) and delivers to every
  `enabled` destination. Per-destination outcome is recorded; one destination
  failing does not block the others or the email.

### C. The generic webhook — payload is the versioned JSON

The `webhook` destination `POST`s, to the referenced URL:

- **Body:** the versioned **attestation** JSON for the freshest attestation of the
  target ([[spec 0002]] / [[spec 0012]]), i.e. the same object shape a client hook
  would carry, plus the monitor's transition context (`state: overdue|recovered`,
  `interval_seconds`, `grace_seconds`, `freshest_attestation_at`). `schema_version`
  ([[spec 0026]]) is present so a receiver validates against a stable contract.
- **Honest scope.** The payload states plainly that Salvage has **not received** a
  passing attestation within interval + grace — it does **not** assert the backup
  itself failed (inherited verbatim from [[spec 0015]] R6 / [[spec 0012]]).
- **Headers:** `Content-Type: application/json`, an idempotency key (below), and (Open
  question) a signature header so the receiver can verify authenticity.

This generic webhook is the substrate; Slack and PagerDuty are adapters over it.

### D. Slack & PagerDuty adapters

Both are **formatting adapters** that consume the same rendered transition + the
same versioned JSON and reshape it for the vendor endpoint — they are not separate
delivery engines:

- **Slack** (`kind: 'slack'`): `POST`s a Slack-shaped message (a `blocks` summary —
  target, state, how overdue, link to the portal monitor) to a Slack **incoming
  webhook URL** held by reference. The raw versioned JSON is not required by Slack;
  the human-readable summary is derived from it.
- **PagerDuty** (`kind: 'pagerduty'`): `POST`s a PagerDuty Events API v2 payload —
  `trigger` on `ok → overdue`, `resolve` on `overdue → ok` — keyed by a
  **`dedup_key`** derived from `(monitor_id)` so PagerDuty correlates the trigger
  and its later resolve into a single incident. The **routing key** is held by
  reference.

Both adapters reuse the generic webhook's delivery machinery (Section E) — retry,
backoff, idempotency — so reliability is implemented once.

### E. Delivery reliability (hosted only)

Client hooks are best-effort/local (Section A). **Hosted** delivery is where
reliability matters, because the switch fires from a serverless cron tick with no
operator watching:

- **Retry + backoff.** A destination `POST` that fails (non-2xx, timeout) is retried
  with exponential backoff and a bounded ceiling. Because the switch runs on the
  hourly cron ([[spec 0015]] R5), a delivery that exhausts in-tick retries is
  re-attempted on the next tick while the monitor remains `alerting` — the durable
  fallback is the cron cadence itself, not an in-process queue.
- **Idempotency / de-dupe.** Every delivery carries a stable **idempotency key** =
  `hash(monitor_id, transition_state, freshest_attestation_at)`. A receiver (and the
  PagerDuty `dedup_key`) can collapse duplicates from a retry or an overlapping tick
  into one incident. This composes with 0015 R4's alert-once-per-transition, which
  already suppresses repeat *alerts* while continuously overdue; the idempotency key
  additionally suppresses repeat *deliveries* of the same alert.
- **Flap control.** A monitor oscillating around its deadline (fresh, stale, fresh)
  is damped: a minimum re-notify interval per destination prevents a
  quick `overdue → ok → overdue` from paging twice in close succession. The grace
  window of 0015 R3 is the first line of flap defense; this is the second.
- **Rate limiting.** Per-tenant and per-destination delivery is rate-limited so a
  large fleet turning over at once (or a misconfigured monitor) cannot burst a
  tenant's Slack/PagerDuty endpoint.

### F. Secret handling (by reference — [[spec 0003]])

- Destination secrets — a webhook URL containing a token, a Slack incoming-webhook
  URL, a PagerDuty routing key — are stored in the secret store and referenced by a
  **handle** (`target_ref`); the plaintext is resolved only at delivery time.
- These secrets **never** appear in a report, an attestation, the ledger, or a
  webhook *payload* body. A payload that echoes destination config carries the
  reference, never the value — the same discipline [[spec 0003]] mandates and
  [[spec 0024]] R5 applies to engine credentials (`MYSQL_PWD` by env, never on a
  command line).
- Client-side URL hooks reference their token the same way (`token_ref=env:…` in the
  example above), so no secret is written into `salvage.yaml`.

## Requirements

**R1 — Client `on_fail` / `on_success` invocation.** The CLI MUST support an
optional `alerts.on_fail` and `alerts.on_success` hook (command or `https://` URL).
`on_fail` MUST fire on a `fail` verdict or an operational error; `on_success` on a
`pass`. The hook MUST receive the run's **report JSON** — on stdin for a command
hook (with the report path in `$SALVAGE_REPORT`), as a `POST` body with
`Content-Type: application/json` for a URL hook. Realizes [[spec 0007]] R4; MUST NOT
introduce a daemon ([[spec 0007]] R1).

**R2 — Hook is non-fatal and secondary.** A hook that errors, hangs past a bounded
timeout, or is unreachable MUST NOT change the run's exit code (exit-code
composition stays primary per [[spec 0007]] R4); the failure MUST be logged. The
report MUST already be written before the hook fires ([[spec 0007]] R3).

**R3 — Hosted webhook delivery for the dead-man's-switch.** On a [[spec 0015]] R4
transition (`ok → overdue` and `overdue → ok`), the notary MUST deliver to every
`enabled` destination configured for that monitor, **in addition to** the existing
email path. A monitor with zero destination rows MUST behave exactly as 0015 today
(email only) — the email path is unchanged.

**R4 — Generic webhook payload = versioned report/attestation JSON.** A `webhook`
destination MUST `POST` the versioned attestation JSON ([[spec 0002]] /
[[spec 0012]]) plus the transition context (`state`, `interval_seconds`,
`grace_seconds`, `freshest_attestation_at`), with `schema_version` present
([[spec 0026]]). The payload MUST NOT invent a bespoke alert envelope, and MUST
state honest scope — "no passing attestation received," not "the backup failed"
([[spec 0015]] R6).

**R5 — Slack + PagerDuty adapters over the generic webhook.** `slack` and
`pagerduty` destinations MUST be delivered by reshaping the same transition/payload
for the vendor endpoint, reusing the generic webhook's delivery machinery (R6). The
PagerDuty adapter MUST emit `trigger` on `ok → overdue` and `resolve` on
`overdue → ok`, correlated by a stable `dedup_key` derived from the monitor.

**R6 — Retry/backoff + de-duplication for hosted delivery.** Hosted delivery MUST
retry failed `POST`s with exponential backoff and a bounded ceiling, falling back to
re-attempt on the next cron tick while the monitor remains `alerting`. Every delivery
MUST carry a stable idempotency key = `hash(monitor_id, state,
freshest_attestation_at)` so retries and overlapping ticks collapse to one incident.
Delivery MUST be rate-limited per tenant and per destination, and MUST damp flapping
(a minimum re-notify interval). This composes with — does not replace —
[[spec 0015]] R4's alert-once-per-transition.

**R7 — Secrets by reference for destinations.** Every destination secret (webhook
URL/token, Slack webhook URL, PagerDuty routing key) MUST be stored and passed **by
reference** (a secret-store handle), resolved only at delivery time, and MUST NEVER
appear in a report, attestation, ledger entry, or webhook payload body. Client URL
hooks MUST likewise reference their token rather than embed it in `salvage.yaml`.
Consistent with [[spec 0003]].

**R8 — Inherited platform, additive schema.** The monitor model, overdue
evaluation, and cron trigger of [[spec 0015]] MUST be inherited unchanged; this spec
adds only a `monitor_destinations` table and delivery adapters. No change to the
verdict, signing/ledger, or attestation surface ([[spec 0002]], [[spec 0012]]).

## Open questions

- **Signed webhook payloads.** Should the generic webhook carry a signature header
  (an HMAC over the body keyed by a per-destination secret, or an attestation-key
  signature per [[spec 0002]]) so a receiver can verify the alert genuinely came
  from Salvage and was not tampered with in transit? Leaning yes for the generic
  webhook (Slack/PagerDuty verify via their own routing secrets); the choice of
  HMAC-vs-attestation-signature and key rotation is the open part.
- **Destination-level verbosity.** Should the generic webhook always carry the full
  versioned attestation, or offer a "summary-only" mode for receivers that just want
  the transition? (Default: full JSON, since that is the stable contract R4 promises.)
- **Client-side hosted-webhook parity.** Should the *client* `on_fail` hook and the
  *hosted* webhook share one documented receiver contract (same versioned JSON, same
  optional signature) so an operator writes one endpoint for both? (Leaning yes;
  [[spec 0026]] makes this practical.)
- **PagerDuty severity mapping.** Whether overdue always maps to a single severity,
  or the monitor's grace/interval informs it. Deferred.
- **Retry ceiling vs. cron cadence.** How many in-tick retries before deferring to
  the next hourly tick — a tuning question, not a design one.

## Acceptance criteria

1. A local run that produces a `fail` verdict invokes the configured
   `alerts.on_fail` hook with the run's report JSON (stdin for a command hook, `POST`
   body for a URL hook); a `pass` invokes `on_success`; a hook that errors does not
   change the run's exit code.
2. A simulated overdue target (a monitor past interval + grace) delivers, on the next
   cron tick, the versioned attestation JSON to a **test webhook endpoint**, and a
   subsequent fresh attestation delivers a `recovered` payload — both carrying a
   `schema_version` and honest "not received" scope.
3. The **email path is unchanged**: a monitor with no destination rows emails
   `contact_email` exactly as [[spec 0015]] R6, with no regression.
4. A Slack destination receives a human-readable summary and a PagerDuty destination
   receives a `trigger` then, on recovery, a `resolve` correlated by one `dedup_key`.
5. A destination `POST` that fails is retried with backoff and re-attempted on the
   next tick; duplicate deliveries for the same transition share one idempotency key
   and collapse to a single incident; no destination secret appears in any report,
   attestation, ledger entry, or payload body (`grep` for the secret value returns
   nothing).
