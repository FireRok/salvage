# Scheduling & monitoring

A single attestation proves one restore worked once. What an auditor or insurer
actually underwrites is a **continuous, unbroken cadence** — "a passing restore
every week for a year." Two pieces make that real (spec
[0015](../../specs/0015-scheduled-attestation.md)):

1. **Client cadence** — you run `salvage attest` on a schedule. Each run appends
   to the tamper-evident ledger, whose hash chain makes the cadence un-forgeable
   after the fact.
2. **The dead-man's-switch** — the notary knows each monitored target's expected
   interval and **alerts when an attestation is overdue** — catching the exact
   failure a client-side cron cannot self-report: a dead box sends nothing.

## `salvage schedule`

`salvage schedule` emits a ready-to-install **systemd** service + timer and an
equivalent **cron** line that run `salvage attest` on your interval. It
**installs nothing** — it prints for review.

```sh
salvage schedule -config salvage.yaml -every 7d
```

- `-every` accepts `1h`, `12h`, `1d`, `7d`, `1w`. The systemd timer uses
  `OnUnitActiveSec`, so it can express any interval; the cron line is emitted for
  the intervals cron can represent cleanly (otherwise it points you at the
  systemd timer).
- The output embeds the absolute path to the `salvage` binary and your config, so
  the printed units are ready to paste.

Example (abridged) output:

```ini
### systemd (recommended) — /etc/systemd/system/salvage-attest.service
[Service]
Type=oneshot
# Environment=SALVAGE_ATTEST_KEY=sk_...     # or rely on ~/.salvage/credentials
ExecStart=/usr/local/bin/salvage attest -config /etc/salvage/prod.yaml

### /etc/systemd/system/salvage-attest.timer
[Timer]
OnUnitActiveSec=7d
OnBootSec=5min
Persistent=true
[Install]
WantedBy=timers.target

# enable:  sudo systemctl daemon-reload && sudo systemctl enable --now salvage-attest.timer
```

## Unattended keys

An unattended `salvage attest` needs an API key, supplied one of two ways (see
[Attestation](./05-attestation.md#accounts-login-and-api-keys)):

- **`SALVAGE_ATTEST_KEY`** in the environment / the systemd unit — a
  portal-generated key is the right fit for a server or CI runner.
- **`~/.salvage/credentials`** — left by running `salvage login` as the user the
  timer/cron job runs as.

## The dead-man's-switch

The notary runs a per-account **monitor** for each target: an expected
`interval` plus a `grace` window, created and deleted from the `/portal`. On each
hourly cron tick it finds the freshest attestation for that target and evaluates
whether it is overdue:

> overdue when `now - freshest > interval + grace`
> (a monitor with no attestations yet becomes overdue once `interval + grace`
> elapses since it was created)

It alerts **once per transition**, so a continuously-overdue target does not
spam you:

- **ok → overdue** — one alert; the monitor's state becomes `alerting`.
- **overdue → ok** — a fresh attestation lands; a one-line recovery note, state
  back to `ok`.

**Honest scope:** the alert states plainly that Salvage has not *received* a
passing attestation for the target within the expected window. It does **not**
assert the backup itself failed — only that verification is overdue.

The `/portal` lists every monitor with live status (`ok` / `overdue` / `never`),
a form to add one (target + interval days + optional grace), and delete.

## Hosted alert destinations

Beyond the built-in email path, each monitor can carry **alert destinations**
(spec [0030](../../specs/0030-alerting-integrations.md)) that fire on the same
dead-man's-switch transitions:

- **`webhook`** — the payload is the versioned alert JSON, POSTed to your URL.
- **`slack`** — a human-readable summary via a Slack incoming-webhook URL.
- **`pagerduty`** — a PagerDuty Events API v2 event (trigger on overdue,
  resolve on recovery) via a routing key.
- **`email`** — additional recipients beyond the account address.

Destinations are managed via the org API
(`GET/POST /v1/orgs/:id/monitors/:mid/destinations`,
`PATCH/DELETE …/destinations/:did`). Destination secrets — webhook and Slack
URLs, PagerDuty routing keys — are stored by reference and previewed masked;
delivery is retried, and the alert carries the same honest scope as the email:
verification is overdue, not "the backup failed".

For **per-run** alerting from the client instead — an `on_fail`/`on_success`
hook fired by `salvage run`/`attest` itself — see
[Configuration → `alerts`](./02-configuration.md#alerts). The two compose: the
client hook tells you a run failed; the dead-man's-switch tells you runs
stopped arriving at all.

## Running the cadence from CI instead

If a CI system is your scheduler of choice, the same unattended pattern —
scheduled trigger, exit-code gating, secrets by name — has its own chapter:
[CI integration](./08-ci-integration.md). The dead-man's-switch composes with
either scheduler: it watches for *received attestations*, however they were
produced.
