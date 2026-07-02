# Attestation

There are two levels of proof for a Salvage verdict: a **local signature** you
hold, and an **independent notary** attestation you cannot forge.

## Local signing (integrity, not independence)

When `report.sign: true`, `salvage run` writes an ed25519 signature sidecar
(`<report.out>.sig`) using `report.key_path`. This proves the report was **not
altered after signing** — but *you* hold the key, so it only proves
self-consistency. It does not prove an **independent** party ever saw the
verdict.

```yaml
report:
  format: json
  out: ./salvage-report.json
  sign: true
  key_path: ./keys/salvage.key
```

## The hosted independent notary

The paid moat is a hosted notary at **`attest.salvage.sh`** (spec
[0012](../../specs/0012-hosted-attestation.md)). You run the restore-test
locally and submit the signed report; the service verifies it, records a
server-authoritative timestamp, **counter-signs it under a Firerok key you never
hold**, and appends it to a tamper-evident, per-account **append-only ledger**.

**What the notary honestly proves:** Firerok independently received report *R*
from your account at server-time *S*, *R* carried a valid signature, and this
entry extends your history at sequence *N*. It does **not** re-run the restore or
claim the restore ran on Firerok hardware — that is a later execution tier.
`salvage verify` and the notary's output state this scope plainly.

### The append-only ledger

Each attestation is one insert-only entry. Every entry's `prev_hash` is the
previous entry's `entry_hash`, and Firerok signs the `entry_hash`. An entry
cannot be altered, backdated, reordered, or deleted without breaking the chain —
so the **cadence itself** ("a passing restore every week for a year") is
tamper-evident. That un-forgeable cadence is what an auditor or insurer
underwrites (see [Scheduling & monitoring](./06-scheduling-and-monitoring.md) and
the [Evidence pack](./07-evidence-pack.md)).

## `salvage attest` — run and submit

```sh
salvage attest -config salvage.yaml
```

This runs the restore-test, submits the report, and prints:

```
attested: https://attest.salvage.sh/a/att_abc123
  verdict PASS   seq 7
  <honest-scope notice>
  verify with: salvage verify att_abc123
```

You can also submit a report you already have with `-report` (and `-sig`).

Reports are redacted by default, and before submission the bytes pass a
credential-pattern **secret scan** that refuses to submit on a match (an
attested secret cannot be unpublished) — see
[Configuration → `attest`](./02-configuration.md#attest) for the
`secret_scan: refuse|warn|off` setting.

**The API key is never stored in the config.** It is read from an environment
variable (default `SALVAGE_ATTEST_KEY`, or `attest.api_key_env`) or from
`~/.salvage/credentials` left by `salvage login`. Endpoint + key resolve in the
order flags → config → stored login credentials.

## `salvage verify` — check it offline

Anyone — an auditor, an insurer, you — can verify an attestation **offline**
against Firerok's baked-in public key, without an account:

```sh
salvage verify att_abc123
salvage verify https://attest.salvage.sh/a/att_abc123
```

It re-checks each of: the Firerok signature, the ledger chain hash, the report
hash, and the tenant signature, and prints whether the attestation is genuine
(exit `0`) or invalid (exit `1`). The same record is viewable on the notary's
public **verify page** at `/a/<id>`.

## Accounts, login, and API keys

An account holds **multiple named, revocable API keys** managed from the portal
(spec [0014](../../specs/0014-accounts-and-cli-auth.md)).

- **`salvage login`** — the interactive path for a human. It runs the OAuth 2.0
  device flow: the CLI prints a URL + code, opens your browser (set
  `SALVAGE_NO_BROWSER=1` on a headless box and open the URL yourself), and once
  you approve, stores an API key in `~/.salvage/credentials`. `salvage attest`
  then uses it automatically.
- **`salvage logout`** — removes those credentials.
- **The portal** — sign in with GitHub OAuth or an email magic-link, then
  generate/list/revoke keys and see usage vs. your plan cap. **Portal-generated
  keys** are the unattended path: drop one in `SALVAGE_ATTEST_KEY` on a
  server/CI runner for scheduled attestation.

A free tier caps attestations per calendar month; a paid tier removes the cap.

## Orgs, teams, and sharing

Accounts are org-based (spec
[0031](../../specs/0031-orgs-teams-rbac.md)): an org holds the ledger, keys,
and monitors, and can have **multiple members** with roles (`owner`, `admin`,
`member`, `viewer`) enforced on every org-scoped request. A single-member org
behaves exactly like the individual account it grew from.

Two sharing mechanisms come with it: a ledger can be made **private**, and
scoped, revocable **share tokens** grant an outside party (an auditor, an
insurer) read-only access to exactly the ledger or the evidence pack — which
gives the [evidence pack](./07-evidence-pack.md) a **shareable URL** instead of
an emailed file. Members, roles, and share tokens are managed from the portal;
nothing a share token grants is ever mutating.

### Which ledger do my attestations land in?

Every API key is **pinned to one org** at the moment it is minted, and
`salvage attest` writes to that org's ledger using your role there *at use
time* (a viewer's key cannot attest). Two ways to mint:

- **`salvage login`** — the browser approval page asks where attestations from
  this device should go (your personal ledger, or any org you're a member of)
  and pins the key to your choice. The CLI confirms it:
  `Attestations from this machine will land in the "Acme Team" org's ledger.`
  If you only belong to your personal org, there's no picker and nothing
  changes.
- **The portal** — keys are pinned to whichever org you're viewing when you
  generate them. Feed one to the CLI via `SALVAGE_ATTEST_KEY` or
  `attest.api_key_env`.

**Moving to a team.** Ledgers are per-org and append-only — history never
moves between orgs (rewriting a chain would forge the cadence the attestation
exists to prove). When individual accounts grow into a team, either upgrade
one member's personal org to the team tier (its history continues unbroken)
and have everyone else point their keys at it, or start a fresh team org.
Earlier attestations remain verifiable in the personal ledgers they were made
in — for an audit, present those alongside the team ledger; every entry stays
independently checkable with `salvage verify`.
