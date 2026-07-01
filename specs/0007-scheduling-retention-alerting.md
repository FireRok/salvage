# 0007 — Scheduling, retention & alerting

- **Status:** Proposed
- **Created:** 2026-06-29
- **Owner:** Firerok

## Context

A restore-test is only valuable if it runs **regularly** and someone **hears**
when it fails. Salvage is a one-shot CLI with a meaningful exit code. Per the Unix
philosophy (`0005`), it should not reimplement cron — it should compose with
existing schedulers and emit clean signals. This spec covers cadence, report
retention, and failure (and missed-run) alerting.

## Goals

- Run on a cadence via existing schedulers (cron, systemd timers, CI).
- Retain a history of reports/attestations.
- Alert on failure — and on the **absence** of a run (silent stop).

## Non-goals

- Building a scheduler daemon.
- The hosted dashboard / fleet view (`0008`).

## Requirements

**R1 — Scheduler-agnostic.** Salvage MUST remain a one-shot command driven by
cron / systemd-timer / CI. No built-in daemon or scheduler.

**R2 — Machine-readable output.** A stable JSON report (`0002`) plus the exit-code
contract (`0000` R4) MUST let schedulers and CI act programmatically.

**R3 — Report retention.** Salvage MUST write timestamped reports to a configured
local location, with simple rotation (keep N / keep days). Remote retention is the
operator's pipe or the hosted plane's ingestion (`0008`) — Salvage ships no
transport (`0005`).

**R4 — Failure alerting hooks.** On `fail` or operational error, alerting MUST be
easy: a non-zero exit (for cron `MAILTO` / CI), and optionally an `on_fail`
webhook/command hook. Keep it composable — exit code first, hook second.

**R5 — Dead-man's switch.** A backup-test that silently *stops running* is as
dangerous as a failing one. Salvage SHOULD emit a success heartbeat to a
dead-man's-switch URL the operator provides (healthchecks.io-style). (This ping is
from the Salvage process, not the restored cluster, so it does not conflict with
the cluster network isolation in `0003`.)

**R6 — Concurrency & cleanup.** Scheduled runs MUST clean up after themselves
(`0003` R1) and not collide with concurrent runs (unique container names/labels).

**R7 — CI integration.** Provide documented patterns for GitHub / Forgejo Actions
and GitLab CI.

## Open questions

- `on_fail`/`on_success` hook in config vs pure exit-code composition — how much to
  ship.
- Webhook payload shape (reuse the report JSON?).
- Whether minimal remote retention (push the report somewhere) is worth a small
  exception to "no transport," or strictly deferred to `0008`.

## Acceptance criteria

1. A cron example runs on a schedule and mails on failure via non-zero exit (R1/R4).
2. A success heartbeat fires to a configured dead-man URL (R5).
3. Reports are timestamped and rotated per the configured policy (R3).
4. Two concurrent runs do not collide and both clean up (R6).
