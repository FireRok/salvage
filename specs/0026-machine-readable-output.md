# 0026 — Machine-readable output contract

- **Status:** Implemented
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage's verdict is meant to be consumed by machines — CI pipelines gating a
deploy, schedulers recording a run, and (soon) an agent driving Salvage over a
tool interface. [[spec 0002]] R1 committed to exactly this
(`specs/0002-reporting-and-attestation.md:36`): *"Reports MUST be JSON with a
`schema_version`,"* and its acceptance criterion restates it (*"A report
validates against the published JSON schema and carries `schema_version`"*,
`specs/0002-reporting-and-attestation.md:91`). The shipped code does **not**
honor this. The `Report` struct (`internal/report/report.go:17-27`) carries
`Tool, Version, Target, StartedAt, FinishedAt, DurationMS, Restore, Checks,
Verdict` — and no `schema_version` at all (a `grep -r schema_version` across the
Go tree returns zero matches). So the flagship artifact violates its own shipped
spec, and any agent that keys off the promised field finds nothing.

The output *surface* is also inverted. The commands an agent or CI is most likely
to script — `run` and `verify` — are the two that cannot emit machine output:

- `salvage run` (`cmd/salvage/main.go:511-547`) takes only a `-config` flag. It
  writes the report JSON to a file **only** when `report.out` is set in config
  (`main.go:524`, via `report.WriteJSON`, `internal/report/report.go:90-104`);
  with no `report.out` it prints a human summary (`printSummary`,
  `main.go:611-632`) and the verdict JSON is unreachable. There is no way to get
  the verdict object on stdout.
- `salvage verify` (`main.go:477-509`) takes only `-endpoint` and prints human
  text; there is no machine verdict object at all.

Meanwhile the *secondary* commands already speak JSON: `inspect`
(`main.go:577`), `last-good` (`main.go:121`), and `fleet` (`main.go:169`) each
define a `-json` flag. The commands that most need scripting lack the flag the
diagnostic commands already have.

External copy compounds the gap: `marketing/llms.txt` tells agents *"Verdicts are
JSON with a `schema_version`"* — a claim the binary does not currently satisfy on
either count (no field, and no way to get the verdict as JSON from `run`).

This spec closes that gap: it adds the missing `schema_version` and gives `run`
and `verify` a first-class machine-output mode, while leaving the exit-code
contract and the human output untouched. It **completes [[spec 0002]] R1** rather
than superseding it (the requirement was always right; the implementation lagged),
and it is a prerequisite for [[spec 0032]] (the MCP server), which will drive
Salvage programmatically and depends on a stable, versioned verdict object.

## Goals

- Make [[spec 0002]] R1 true: every report Salvage emits carries an explicit,
  versioned `schema_version`, on every path that produces report bytes (file
  output, stdout, and attestation submission).
- Give `run` a `-json` mode that emits the full report object to stdout, so an
  agent or CI step can capture the verdict without configuring `report.out` to a
  temp file and reading it back.
- Give `verify` a `-json` mode that emits a machine verdict object, so
  attestation verification is scriptable with the same ergonomics.
- Keep the exit-code contract (`main.go:83-87`: 0 pass / 1 verdict fail / 2
  operational error) **byte-for-byte unchanged** — the primary machine signal is
  the exit code; JSON is the structured detail beside it.
- Keep the attestation submission carrying the same versioned bytes, so the
  countersigned envelope and the local report never disagree about schema.
- Publish the report JSON schema at a stable public URL so third parties (and
  [[spec 0002]] R1's *"published so third parties can validate"*) can validate
  without reading Salvage's source.

## Non-goals

- **A new report shape.** The field set stays exactly as it is
  (`internal/report/report.go:17-58`); this spec adds a version marker and output
  plumbing, not new report content. Evidence-pack structure ([[spec 0019]]) is
  untouched.
- **Changing verdict semantics or exit codes.** The mapping of restore/check
  results to a `pass`/`fail` verdict (`Report.Finalize`,
  `internal/report/report.go:66-83`) and the exit codes are frozen. An agent that
  today reads only the exit code keeps working unchanged.
- **The MCP server itself.** Driving Salvage as agent tools is [[spec 0032]];
  this spec only guarantees the versioned, stdout-available contract that server
  will build on.
- **Data content of reports.** Data minimization ([[spec 0002]] R7) is inherited
  unchanged — `-json` emits the same asserted scalars the file already does, and
  no more.

## Design

### The version marker (`internal/report`)

`Report` (`internal/report/report.go:17-27`) gains one field, first in the struct
so it leads the JSON object:

```go
type Report struct {
    SchemaVersion int    `json:"schema_version"`
    Tool          string `json:"tool"`
    // ... existing fields unchanged ...
}
```

`report.New` (`internal/report/report.go:61-63`) stamps it from a package
constant (`report.SchemaVersion`) so every report — regardless of which command
or engine produced it — carries the same value from birth. Because every report
originates through `New`, no call site needs to set it explicitly, and the field
cannot be silently omitted on a new code path.

The recommended encoding is a **single monotonic integer** starting at `1`
(shown above), not a semver string: consumers overwhelmingly need "is this a
shape I understand?", which is a `>=` integer comparison, and an integer removes
any ambiguity about which component of a semver a breaking change bumps. The
final choice is left to an Open question, but the requirements below are written
against the recommended integer form.

The field is set once, in one place, and serialized by the existing
`json.MarshalIndent` in `WriteJSON` (`internal/report/report.go:90-104`) — so it
appears identically in the file at `report.out`, in the `-json` stdout bytes, and
in the attestation payload, because all three render the same `*Report`.

### `run -json` (`cmd/salvage/main.go`)

`cmdRun` (`main.go:511-547`) gains a `-json` bool flag. When set, after
`engine.Run` returns and the existing `report.out`/signing side effects run
(unchanged), Salvage writes the report bytes to **stdout** instead of calling
`printSummary`:

- The stdout bytes are the exact bytes `WriteJSON` already produces (the same
  bytes written to `report.out` and, on the attest path, submitted for
  countersignature) — one serialization, three destinations, no divergence.
- `report.out` is still honored when set: `-json` adds a stdout destination, it
  does not disable the file. `-json` and `report.out` are independent.
- Human diagnostics that are not the report (warnings such as `"write report:"`
  or `"operational error:"`) continue to go to **stderr**, so `-json` stdout is a
  single clean JSON document an agent can pipe straight into a parser.
- The exit code is computed exactly as today (`main.go:540-546`): operational
  error → 2, failing verdict → 1, else 0. `-json` never changes the exit code.

### `verify -json` (`cmd/salvage/main.go`)

`cmdVerify` (`main.go:477-509`) gains a `-json` bool flag. When set, instead of
the human block (`main.go:491-508`) Salvage emits a single machine verdict object
to stdout describing the fetched attestation: at minimum the attestation `id`,
`target`, `verdict`, `seq`, `key_id`, the per-check transcript already available
in `checks` (`main.go:490`), a boolean `valid` (the `ok` from `attest.Verify`,
`main.go:490`), and a `schema_version` so the verify object versions alongside
the report. The exit code is unchanged: an invalid attestation still exits 1
(`main.go:504-506`), a genuine one exits 0.

### Attestation submission carries the version

The attest path already reuses the report bytes: `cmdAttest` renders the report
with `rep.WriteJSON("")` and submits those exact bytes
(`main.go:414-416`, `main.go:462`). Because `schema_version` is stamped in
`report.New` and serialized by `WriteJSON`, the submitted bytes carry it with no
change to the attest code — the countersigned envelope and any local report copy
are guaranteed to agree on schema. This spec only *requires and tests* that
property; it needs no new attest logic.

### Published schema

The report JSON schema is published at a stable public URL (e.g. a
`schema.salvage.sh/report/v1.json`-style path), fulfilling [[spec 0002]] R1's
"published so third parties can validate" and supporting the operability goal
that a machine consumer can validate a verdict without reading Salvage's source.
The `schema_version` integer corresponds to a published schema document; bumping
the integer means publishing a new document, never mutating an existing one.

### Secondary commands

`inspect`, `last-good`, and `fleet` already have `-json` (`main.go:577`, `:121`,
`:169`) and are out of scope except that their JSON is inventoried alongside the
new flags. Whether `check`, `attest`, and `schedule` should also gain `-json` is
deferred to an Open question — `run` and `verify` are the load-bearing pair for
CI and agents and are the whole scope of this spec's new flags.

## Requirements

**R1 — Versioned report, everywhere.** Every `Report` Salvage emits MUST carry a
`schema_version` field. It MUST be stamped in `report.New`
(`internal/report/report.go:61-63`) from a single package constant, so it is
present and identical on every path that produces report bytes — the file at
`report.out`, `run -json` stdout, and the attestation submission — with no call
site able to omit it. This completes [[spec 0002]] R1.

**R2 — `run -json` emits the full report to stdout.** `salvage run` MUST accept a
`-json` flag; when set it MUST write the full report JSON (the exact bytes
`report.WriteJSON` produces) to stdout, and MUST NOT emit the human summary to
stdout. Non-report diagnostics MUST go to stderr, so `-json` stdout is a single
valid JSON document.

**R3 — `-json` does not disable `report.out`.** When both `-json` and
`report.out` are set, Salvage MUST still write the file at `report.out` (and its
`.sig` sidecar if signing is configured) *and* emit to stdout. The two
destinations are independent; the bytes MUST be identical.

**R4 — `verify -json` emits a machine verdict object.** `salvage verify` MUST
accept a `-json` flag; when set it MUST emit a single JSON object to stdout
carrying at least `id`, `target`, `verdict`, `seq`, `key_id`, the per-check
transcript, a boolean validity result, and a `schema_version`, in place of the
human text.

**R5 — Exit codes unchanged.** `-json` MUST NOT change any exit code. `run` MUST
still exit 2 on operational error, 1 on a failing verdict, and 0 on pass
(`main.go:540-546`); `verify` MUST still exit 1 on an invalid attestation and 0
on a genuine one (`main.go:504-506`). The exit-code contract at `main.go:83-87`
MUST remain byte-for-byte unchanged.

**R6 — Attestation submission carries `schema_version`.** The bytes submitted to
the notary (`main.go:414-416`, `main.go:462`) MUST carry `schema_version`, with
no change to the attest submission logic beyond what R1 provides — the submitted
report and any local report copy MUST agree on schema.

**R7 — Published schema.** The report JSON schema MUST be published at a stable
public URL, and the emitted `schema_version` value MUST correspond to a published
schema document. Bumping the version MUST mean publishing a new document, never
mutating an existing one.

**R8 — No shape change, no new dependency.** The report field set MUST be
unchanged except for the added `schema_version` field; verdict semantics
(`Report.Finalize`, `internal/report/report.go:66-83`), data minimization
([[spec 0002]] R7), signing, and the evidence pack ([[spec 0019]]) MUST be
inherited unchanged. No new Go dependency (stdlib + `gopkg.in/yaml.v3`).

## Open questions

- **Integer vs semver `schema_version`.** Recommended: a single monotonic
  integer starting at `1` (consumers need a `>=` "can I read this?" check, and an
  integer has no component-ambiguity). A short semver string (`"1.0"`) is the
  alternative if we anticipate additive-vs-breaking distinctions consumers should
  branch on. Pick one before implementation; the requirements assume the integer.
- **Do `check`, `attest`, and `schedule` also get `-json`?** `run` and `verify`
  are the load-bearing pair and are this spec's whole flag scope. `attest` in
  particular already prints a share URL and could emit a machine object
  (`main.go:467-474`); deciding whether to round out `-json` coverage across all
  commands, and whether an eventual global `--output=json` is cleaner than
  per-command flags, is deferred.
- **Schema hosting.** The exact stable URL and whether the schema is generated
  from the Go struct or hand-maintained is deferred to implementation, so long as
  R7 (stable, versioned, published, never mutated) holds.
- **`verify -json` object versioning.** Whether the verify object shares the
  report's `schema_version` counter or carries its own is open; R4 requires only
  that it carries *a* `schema_version`.

## Acceptance criteria

1. `go build ./... && go vet ./... && go test ./...` all pass.
2. `grep -rn '"schema_version"' internal/report` returns the added struct tag
   (today it returns nothing across the whole tree) — confirming R1's field
   exists and is serialized.
3. `salvage run -json -config salvage.example.yaml` prints a single JSON document
   to stdout whose top-level object contains `schema_version` and the existing
   report fields; the process exit code matches the verdict (0 on PASS, 1 on
   FAIL, 2 on operational error) exactly as without `-json`.
4. With both `-json` set and `report.out` configured, the file at `report.out`
   and the stdout bytes are byte-identical and both carry `schema_version` (R3).
5. `salvage verify -json <id>` prints a single JSON object carrying `id`,
   `target`, `verdict`, `seq`, `key_id`, the per-check transcript, a validity
   boolean, and `schema_version`; an invalid attestation still exits 1 (R4/R5).
6. A report submitted for attestation carries `schema_version` in the submitted
   bytes, matching the local report copy (R6).
7. The report JSON schema resolves at its published URL, and a report emitted by
   `run -json` validates against it (R7).
