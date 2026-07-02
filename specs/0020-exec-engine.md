# 0020 — The exec engine (bring-your-own-restore)

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage's engines ([[spec 0016]]) each *restore* a backup into a throwaway
environment and *validate* it. That covers backup types Salvage knows how to
restore (Postgres today, restic next — [[spec 0018]]). But many customers have a
**restore procedure Salvage does not — and may never — natively support**: a
bespoke script, a proprietary database, a cloud-managed snapshot spun into an
instance, an appliance, or simply a restore too large for the container model
([[spec 0018]] notes the scratch-disk ceiling). Today those customers have no
first-class path.

The tempting shortcut — "let them `salvage attest` a hand-authored report JSON" —
is a **weak product path** and we reject it. The notary signs any report ([[spec
0017]] R1), so it is *possible*, but a human-typed verdict is forgeable at the
source: it earns trust only from the ledger and cadence, not from any evidence
that a restore actually happened. Attestation is only as strong as the
verification behind it.

The **exec engine** closes this properly. The customer brings the *restore*
(their own command); Salvage **runs it**, then runs the customer's **validation
logic — expressed in the Salvage config format** (HTTP calls, shell assertions,
file checks, and SQL via a client) — against the restored target, and produces
the verdict/report **itself**. Salvage orchestrates, validates, and attests; the
restore mechanics stay the customer's. This is a real verification event that
inherits the entire horizontal platform ([[spec 0017]]) — report, ledger,
verify, dead-man's-switch, evidence pack — for backup types we never wrote an
engine for.

## Where this sits in the trust model

Honest scoping ([[spec 0012]], [[spec 0017]] R6): the exec engine runs the
**customer's** restore command on the **customer's** host. Salvage does not
independently reconstruct the backup, so this is squarely the **notary tier** —
"Salvage ran this restore procedure and these checks passed, at this time,
chained into an append-only ledger." That is *far* stronger than a hand-authored
JSON (Salvage executed the steps and observed the results) and still weaker than
the future hosted-execution tier (Salvage supplies the environment). The report
MUST record that the restore was operator-supplied so the claim is not overstated
(R7).

## Goals

- A `target.type: exec` engine: **restore = run a customer command**;
  **validate = run customer-authored checks in the Salvage format** against the
  restored target.
- Validation kinds usable without embedding database drivers or new
  dependencies: `http` (stdlib), `command` / `file_exists` / `file_count` /
  `checksum` (reused from the restic prober contract), and SQL via a
  client-shelled `command` (honest, zero-dep).
- Inherit the horizontal platform unchanged (report, attestation, cadence,
  evidence) — [[spec 0017]] R1–R2, R5.
- Solve the "backup larger than container scratch" case: the restore lands
  wherever the customer's command puts it, not inside a Salvage-managed
  container.

## Non-goals (v1)

- Salvage supplying/hardening the restore environment (that is the deferred
  hosted-execution tier). The customer owns the host and the restore command.
- Sandboxing the customer's command. It runs with the privileges of the Salvage
  process; the config is trusted input, exactly like a shell script the operator
  wrote. (Security note in Design.)
- Embedding SQL/HTTP client *drivers*. SQL assertions shell to a client the
  customer already has (`psql`, `mysql`, `mongosh`) via the `command` kind or the
  optional `query` kind.
- Discovery/scaffold for exec — recommending checks for a BYO restore is its own
  feature ([[spec 0021]]).

## The model

```
target:
  type: exec
  name: billing-db-nightly
  restore:
    command: ["/opt/backups/restore-into-scratch.sh"]   # customer's script
    env: [RESTORE_TARGET, PGHOST]                        # host env passed through
    timeout: 30m
    workdir: /opt/backups
  checks:
    - name: api-healthz
      kind: http
      url: http://127.0.0.1:8080/healthz
      expect_status: 200
      expect_body_contains: '"db":"ok"'
    - name: row-count
      kind: command
      command: ["bash","-lc","psql \"$DSN\" -tAc 'select count(*) from invoices'"]
      expect_min: 1000
    - name: manifest-present
      kind: file_exists
      path: /scratch/restore/MANIFEST
    - name: dump-size
      kind: file_count
      path: /scratch/restore/tables
      expect_min: 42
```

- **Restore** = run `restore.command`. Exit `0` → restore succeeded; non-zero →
  a normal `"fail"` verdict (not an operational error). The command's job is to
  leave a restored system reachable from the Salvage host (a local DB, a running
  service, a directory of files).
- **RestoredTarget** = a **host prober**: it runs check commands, file probes,
  and HTTP requests *from the Salvage host* (where the restore just happened).
  `Stop()` optionally runs an operator-supplied `restore.cleanup` command; it is
  idempotent and its failure is a warning, never a verdict change.
- **Checks** = the customer's assertions in the Salvage format, dispatched by
  `kind` through the existing evaluator registry ([[spec 0017]] R3).

## Requirements

**R1 — `target.type: exec` engine.** Implement `spi.Engine` for `type() ==
"exec"`. `Restore` runs `restore.command` with the declared `env`/`workdir`/
`timeout` and returns a live `RestoredTarget`. A missing/empty `restore.command`,
an unresolvable working directory, or an inability to *launch* the process is a
`*spi.Fault` (operational, exit 2, no verdict). A command that **runs but exits
non-zero** is a normal `"fail"` verdict with a nil operational error — preserving
the operational-vs-verdict split ([[spec 0016]] R4). The restore's stdout/stderr
tail is captured into the report's restore detail (bounded).

**R2 — Host prober `RestoredTarget`.** The returned target runs checks against
the host the restore ran on. It MUST implement the same command/file prober
capability the restic engine's target exposes (so the existing `command`,
`file_exists`, `file_count`, `checksum` evaluators work against it unchanged),
plus an HTTP capability for R3. `Stop()` runs the optional `cleanup` command and
is idempotent.

**R3 — `http` check kind (native, stdlib).** Register an `http` evaluator
(`checks.RegisterEvaluator("http", …)`) that type-asserts the target to an
`HTTPProber`. A `kind: http` check supports: `url`, `method` (default `GET`),
optional `headers`, optional `body`, `expect_status` (default 200),
`expect_body_contains` (substring), and `expect_json` (a `path=value` assertion
over a JSON body using a minimal dotted-path lookup — no new dependency). The
result is the standard `{name, ok, severity, got, detail}` so the report/ledger/
verify need no change. Uses `net/http` only.

**R4 — SQL against an external restored DB, zero-dep.** SQL assertions are
supported **without embedding a driver**, two ways:
- via `kind: command` shelling to the customer's client (`psql "$DSN" -tAc …`),
  with the same `expect_min`/`expect_max`/`equals` expectations applied to the
  command's stdout scalar; and
- an optional convenience `kind: query` that takes `client` (`psql`|`mysql`|
  `mongosh`|a path), a `dsn` (from the check or a target-level default), and a
  `sql`/`query` string, builds the client invocation, and applies the scalar
  expectations. `query` is sugar over `command`; it introduces no driver
  dependency. (v1 MAY ship only `command`-based SQL and add `query` as a
  fast-follow — the `command` path alone satisfies the requirement.)

**R5 — Inherit the horizontal platform unchanged.** The exec engine provides only
*restore* + *check evaluation*; it MUST inherit the verdict rule, `report.Report`,
the notary submit/ledger/verify path, the dead-man's-switch, and the evidence
pack with **zero** changes to those layers ([[spec 0017]] R1–R2, R5). An
exec-produced report attests and verifies exactly like a Postgres one.

**R6 — Capabilities gated, not faked.** The exec engine does **not** implement
`ChainTester`/`FleetSurveyor`; `last-good` and `fleet` MUST return the clear "not
supported for target.type exec" error ([[spec 0016]] R5). `scaffold` for exec is
[[spec 0021]]; until then it returns "not supported" like any engine without
discovery.

**R7 — Honest provenance in the report.** The report MUST mark the restore as
operator-supplied (e.g. `restore.method: "exec"` / a boolean the report and
evidence pack surface), so no downstream artifact reads as "Salvage
independently restored this." The verify page and evidence pack state, for exec
attestations, that the restore procedure was customer-provided and Salvage
executed and validated it. This keeps the claim precisely scoped ([[spec 0012]],
[[spec 0017]] R6).

**R8 — Config validation.** `config.Validate` accepts `target.type: exec` and
validates its shape: `restore.command` non-empty; each check's `kind` in the set
the exec engine registers (`http`, `command`, `file_exists`, `file_count`,
`checksum`, and `query` if shipped); `http` checks require `url`; `file_*` checks
require `path`; a `query`/command-sql check names its client or command. Unknown
kinds fail config load with a clear message, not at runtime.

**R9 — No new dependencies.** `http` uses `net/http`; the prober uses `os/exec`;
JSON assertions use `encoding/json`. The module keeps its single
`gopkg.in/yaml.v3` dependency ([[spec 0016]] R7).

## Design notes

### Reusing the restic prober contract
[[spec 0018]] introduced a `FileProber`-style capability that the `command`,
`file_exists`, `file_count`, and `checksum` evaluators type-assert. The exec
target implements the **same interface**, but its methods run **on the host**
(`os/exec`, `os.Stat`, `filepath.Walk`, `crypto/sha256`) instead of via
`docker exec` inside a restic container. Because evaluators are keyed by `kind`
and dispatch by type-asserting the target ([[spec 0017]] seam), those four kinds
work for exec with **no new evaluator code** — the exec engine only adds `http`
(and optional `query`). This is the seam paying off exactly as designed.

### Where checks run
Checks run **from the Salvage host**, the same box the restore command ran on, so
the restored system is reachable as the customer's command left it (a DB on
localhost, a service on a port, files on disk). This is the simplest honest model
and needs no network plumbing. (A future option: run checks inside a
customer-named container via `docker exec`, or over SSH — deferred; the host
prober covers the common case.)

### Security posture
The restore and `command` checks execute operator-authored commands with the
Salvage process's privileges. This is **trusted input** — identical to the shell
script the operator would otherwise run by hand — and MUST be documented as such
(README + `salvage schedule` note). Salvage does not sandbox it. Unattended runs
([[spec 0015]]) therefore inherit the trust of whatever box holds the credential.
No check kind escalates privilege beyond what the config author already has.

### Net isolation
The Postgres/restic engines network-isolate the restored container before running
checks so assertions can't accidentally hit production ([[spec 0003]]). The exec
engine cannot isolate a host it does not own; it MUST document that the customer's
restore command is responsible for restoring into an **isolated** target (a
scratch DB, a throwaway instance) and that checks run against whatever the command
produced. The report records this is an operator-managed environment (R7).

## Open questions

- **`query` in v1 or fast-follow?** The `command` path already satisfies SQL
  assertions; `query` is ergonomics. Ship `command`-only first if it shortens v1.
- **Remote check execution** (`docker exec <name>` / SSH) — deferred; the host
  prober is the v1 surface.
- **Structured HTTP assertions** beyond substring + single json-path (schema
  match, multiple paths) — start minimal, grow with demand.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` pass with the exec engine
   registered and blank-imported; the module still has one dependency.
2. A config with `target.type: exec`, a `restore.command` that succeeds, and one
   each of `http`, `command`, and `file_exists` checks produces a `pass` verdict;
   flipping any required check to fail (or the restore command to exit non-zero)
   produces a `fail` verdict — not an operational error.
3. Removing/renaming the restore command binary (cannot launch) yields exit 2
   with an operational message and **no** verdict.
4. An exec-produced report attests through the live notary and verifies genuine,
   with **no** change to the notary, ledger, verify page, dead-man's-switch, or
   evidence pack; the verify page/evidence note the restore was
   operator-supplied (R7).
5. `last-good`/`fleet` on an exec target return "not supported for target.type
   exec"; `config.Validate` rejects an exec config missing `restore.command` or
   using an unknown check kind, at load time.
6. README gains a "Requirements" section naming Docker (for the container
   engines) and clarifying the exec engine needs only the customer's own restore
   tooling.
