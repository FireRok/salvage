# 0015 — Scheduled attestation + dead-man's-switch

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

A single attestation proves one restore worked once. The value an auditor or insurer
underwrites is a **continuous, unbroken cadence** — "a passing restore every week for a
year." Two pieces make that real:

1. **Client cadence** — the customer runs `salvage attest` on a schedule (their own
   cron/systemd/k8s CronJob). Each run appends to the tamper-evident per-account ledger
   ([[spec 0012]]). The ledger's hash chain already makes the cadence un-forgeable after
   the fact.
2. **Dead-man's-switch** — the notary knows each monitored target's *expected* interval
   and **alerts when an attestation is overdue**. This is the moat-y half: it catches the
   exact failure that matters — *your backups silently stopped being verified* — which a
   client-side cron cannot self-report (a dead box sends nothing).

## Goals

- Make unattended `salvage attest` easy (guidance + an optional `salvage schedule` helper
  that emits a systemd timer/service or cron line).
- A per-account **monitor**: `(target, expected interval, grace)`. The notary emails the
  account when the freshest attestation for that target is older than interval + grace,
  and again notes recovery when a fresh one arrives.
- Runs on a Cloudflare **Cron Trigger** — serverless, no always-on process (cost posture
  unchanged).

## Non-goals (v1)

- Inferring cadence automatically (explicit config is predictable; inference later).
- Paging/SMS/Slack — email only in v1 (reuses the Email binding). Webhooks later.
- Client daemon — the customer's own scheduler runs the CLI; Salvage does not daemonize.

## Requirements

**R1 — `salvage schedule`.** `salvage schedule -config salvage.yaml -every <interval>`
prints a ready-to-install **systemd** service+timer and an equivalent **cron** line that
run `salvage attest` on that interval. It installs nothing (prints for review); it notes
that the unattended run needs a key (env `SALVAGE_ATTEST_KEY` or a `salvage login`
credential on the box).

**R2 — Monitor model.** A `monitors` row: `account_id, target, interval_seconds,
grace_seconds, contact_email, alert_state (ok|alerting), last_alert_at`. Unique per
`(account_id, target)`. Created/deleted from the portal.

**R3 — Overdue evaluation.** For each monitor, the freshest attestation is
`max(created_at)` over `attestations` where `tenant_id = account_id AND target = target`.
Overdue when `now - freshest > interval + grace` (a monitor with no attestations yet is
overdue once `interval + grace` elapses since it was created).

**R4 — Alert once per transition.** On `ok → overdue`: email the contact and set
`alert_state = alerting`, `last_alert_at = now`. On `overdue → ok` (a fresh attestation
lands): set `alert_state = ok` and send a one-line recovery note. No repeated spam while
continuously overdue.

**R5 — Cron trigger.** A scheduled Worker handler (`scheduled()`) runs on a Cron Trigger
(hourly) and evaluates every monitor. Serverless; no persistent process.

**R6 — Alert channel.** Email via the Cloudflare Email binding to `contact_email`
(default: the account email). Alerts state plainly that Salvage has not *received* a
passing attestation — it does not assert the backup itself failed, only that verification
is overdue (honest-scope, consistent with [[spec 0012]]).

**R7 — Portal.** `/portal` lists monitors with live status (ok / overdue / never), a form
to add one (target + interval days + optional grace), and delete. CSRF-checked.

## Acceptance criteria

1. `salvage schedule -config X -every 7d` prints a valid systemd timer/service + cron line
   invoking `salvage attest`.
2. A monitor whose target has no fresh attestation within interval+grace flips to
   `alerting` and sends one alert on the next cron tick; a subsequent fresh attestation
   flips it back to `ok` with a recovery note.
3. A monitor that stays healthy never alerts.
4. The cron handler evaluates all monitors in one scheduled run with no client involvement.
