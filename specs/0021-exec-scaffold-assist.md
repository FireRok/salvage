# 0021 — Scaffold assist for exec restores (observe & recommend checks)

- **Status:** Proposed
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

For Postgres, `scaffold` ([[spec 0009]]) restores the backup, **introspects the
system catalogs**, and emits a starter config whose checks assert the cluster's
real shape (tables present, row-count floors, extensions, freshness) — then
verifies them by running. That "inspect the backup, recommend the validation
logic" step is one of Salvage's best onboarding moments: the user starts from a
meaningful, passing config instead of a blank file.

The exec engine ([[spec 0020]]) has **no catalog to introspect** — the restored
target is whatever the customer's command produced (a DB, a service, a directory
of files) and Salvage doesn't know its schema. So the *mechanism* of 0009 can't
apply. But the *value* — "run my known-good restore once, hand me a starter set
of checks that lock in its current shape" — absolutely can, via a different
mechanism: **observe the restored environment and propose assertions that pin
what's there now.**

This spec defines **exec scaffold assist**: an observe-then-recommend flow that
turns a single known-good exec restore into a reviewed starter config of
[[spec 0020]] checks (`http` / `file_*` / `command`), so BYO-restore customers
get the same onboarding lift.

## The principle: snapshot-now, assert-later

A known-good restore *is* the specification of "correct." Observe it once, and
each observation becomes a candidate assertion that the **next** restore must
still satisfy:

| Observation of the restored target | Proposed check ([[spec 0020]]) |
|---|---|
| A file tree under a restore path | `file_exists` on stable anchor files; `file_count -min` on populated dirs; `checksum` on files that should be byte-identical |
| An HTTP endpoint returning 2xx | `http` check: `expect_status`, `expect_body_contains` on a stable token, `expect_json` on a health field |
| A reachable DB (a client is available) | client-shelled `command`/`query` checks: table/row-count floors from `count(*)` probes |
| The restore command's own output | a `command` re-assertion or a note the operator can adapt |

Each proposal is a **floor**, not an exact pin (row counts as `-min`, file
counts as `-min`), so normal growth doesn't cause false failures — the same
philosophy as 0009's generated checks.

## Goals

- `salvage scaffold` (or `salvage scaffold -type exec`) that, given an exec
  config with a `restore.command` plus one or more **observation hints**
  (a `path` to walk, a `base_url` to probe, an optional DB `dsn` + `client`),
  runs the restore, **observes** the target, and emits a starter config of
  reviewed checks.
- Every generated check is **verified by running** before it's emitted — never
  propose an assertion that doesn't currently pass (0009's safety net, R-verify).
- A small **profile library** so common targets (a Postgres/MySQL DSN, a web
  service healthcheck, a restored file tree) start from good defaults.
- Optional AI assist (consistent with [[spec 0009]]'s "deterministic core,
  optional AI") to turn observations into *meaningful* named checks — off by
  default, deterministic output without it.

## Non-goals (v1)

- Introspecting arbitrary proprietary systems Salvage has no probe for — v1
  covers files, HTTP, and client-shelled SQL. Others: the operator writes checks
  by hand ([[spec 0020]] already lets them).
- Guaranteeing completeness. Scaffold assist proposes a **starting point**; the
  operator curates. It MUST say so (honest scoping).
- Auto-applying/overwriting a hand-edited config. It prints/writes a starter for
  review, like 0009.

## Requirements

**R1 — Observe-then-recommend flow.** `scaffold` for `target.type: exec` MUST:
(1) run `restore.command` via the exec engine ([[spec 0020]]); (2) **observe**
the target using whichever hints are present (`observe.path`, `observe.base_url`,
`observe.dsn`+`observe.client`); (3) synthesize candidate checks from the
observations; (4) **run each candidate** against the live target and **keep only
those that pass**; (5) emit a complete exec config (target + restore + the
verified checks) to stdout or `-out`. Steps 3–4 reuse the exec evaluators; no new
verdict path.

**R2 — Filesystem observation.** Given `observe.path`, walk it (bounded depth/
count) and propose: `file_exists` on a handful of stable anchor files
(deterministic selection — e.g. top-level, lexically-first, non-temp), a
`file_count -min` on the most-populated directories (floor = observed count), and
`checksum` on files under a caller-opt-in `observe.checksum_globs` (only where a
byte-stable file is expected). Selections MUST be deterministic (no randomness)
so re-running scaffold is reproducible.

**R3 — HTTP observation.** Given `observe.base_url`, probe a small,
**configurable** list of candidate paths (default includes `/`, `/health`,
`/healthz`, `/readyz` — overridable via `observe.http_paths`). For each that
returns 2xx, propose an `http` check with `expect_status` = the observed status
and, when the body is small/JSON, an `expect_json` on an obvious health field or
an `expect_body_contains` on a stable substring. Salvage MUST NOT invent
endpoints that didn't respond.

**R4 — SQL observation (client-shelled, zero-dep).** Given `observe.dsn` +
`observe.client` (`psql`/`mysql`/`mongosh`), enumerate tables/collections via the
client (a fixed catalog query per client) and propose a `count(*)`-based
`command`/`query` check per table with `expect_min` = observed count (a floor).
This needs the customer's client on the box; absent a client, SQL observation is
skipped with a note — never a hard failure. No DB **driver** is added
([[spec 0020]] R4).

**R5 — Verify-before-emit (safety net).** Identical to [[spec 0009]]: every
generated check MUST be executed against the just-restored target and only
emitted if it passes. A candidate that cannot be made to pass is dropped (with a
commented note in the output when useful), so the emitted config is
green-on-first-run.

**R6 — Profiles.** `scaffold -profile <name>` seeds sensible observation hints and
check templates for common targets: `postgres-dsn`, `mysql-dsn`, `web-service`,
`file-tree`. A profile only sets defaults; explicit `observe.*` config overrides
it. Profiles are data, not code branches in the engine.

**R7 — Honest output.** The emitted config MUST carry a header comment stating it
is a **reviewed starting point** generated from one observed restore — floors,
not exact pins — and that the operator should tighten/extend it. Consistent with
[[spec 0017]] R6, it MUST NOT imply completeness or that Salvage understands the
system's semantics.

**R8 — Optional AI assist.** With an explicit opt-in flag/env (mirroring
[[spec 0009]]), Salvage MAY pass the *observations* (file tree summary, endpoint
list, table list — never secrets/credentials) to an LLM to produce better check
**names** and select the most meaningful assertions. Default is deterministic and
fully offline; AI never invents an assertion that isn't then verified by running
(R5 still applies to AI-suggested checks).

**R9 — No new dependencies.** Observation uses `os`/`filepath`/`net/http`/
`os/exec`/`encoding/json`/`crypto/sha256`. The module keeps its single
`gopkg.in/yaml.v3` dependency.

## Design notes

- **Engine capability, gated.** Exec scaffold is an **optional engine
  capability** (like `ChainTester`/`FleetSurveyor` in [[spec 0016]]): the exec
  engine implements a `ScaffoldObserver` interface the `scaffold` command
  type-asserts. Engines without it (or an exec target with no `observe.*` hints)
  return the existing "not supported / nothing to observe" message. No core
  orchestration change.
- **Reuses the exec evaluators.** Candidate checks are just [[spec 0020]] checks;
  "verify by running" is a normal `checks.Run` against the live target. Scaffold
  assist is orchestration around the exec engine, not a parallel validator.
- **Deterministic selection** everywhere (anchor files, dir choice, endpoint
  order) so `scaffold` is reproducible and reviewable in a diff.

## Open questions

- **How many anchors/tables to propose by default** before it's noise — start
  small (e.g. ≤10 file anchors, all tables but capped), let flags widen.
- **Checksum proposals** are risky (many files legitimately change) — default
  **off**, opt-in per glob (R2).
- **Profiles as shipped data vs. docs-only recipes** — start with a couple of
  built-in profiles; expand as real BYO configs teach us the common shapes.

## Acceptance criteria

1. `salvage scaffold` on an exec target with `restore.command` + `observe.path`
   emits a config containing `file_exists`/`file_count` checks that **pass** on a
   fresh `salvage run` of the emitted config (green-on-first-run).
2. With `observe.base_url` pointing at a service returning 200 on `/healthz`, the
   emitted config contains a passing `http` check; no check is emitted for an
   endpoint that didn't respond.
3. With `observe.dsn`+`observe.client` and the client present, the emitted config
   contains per-table `count(*)` floor checks that pass; with no client present,
   SQL observation is skipped with a note and the rest still emits.
4. Every emitted check passes when the emitted config is run immediately (R5); a
   candidate that can't pass is not emitted.
5. The output header states it's a reviewed starting point (floors, not pins);
   no generated check overstates semantics (R7).
6. Deterministic: running `scaffold` twice against the same restored target
   produces identical output.
