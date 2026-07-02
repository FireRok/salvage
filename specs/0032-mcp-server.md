# 0032 — The salvage MCP server

- **Status:** Implemented — stdlib hand-roll; subcommand entrypoint; path-only config input
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage is a non-interactive CLI with a stable exit-code contract (0 = pass,
1 = verdict fail, 2 = operational error) and a fixed set of subcommands —
`run`, `check`, `inspect`, `scaffold`, `last-good`, `fleet`, `schedule`,
`login`, `logout`, `attest`, `verify`, `version` (confirmed in
`cmd/salvage/main.go`). Everything it does is scriptable: a config goes in, a
verdict and a report come out, and (for hosted calls) an attestation lands in
an append-only ledger. That shape — deterministic, exit-code-honest, one binary
— is precisely the shape a coding agent can drive, the same way agents already
drive `uv`, `curl`, and `wrangler`.

The Model Context Protocol (MCP) is a standard agent-tool interface: a host
(an agent runtime) connects to an MCP server, lists the tools it exposes, and
calls them with structured JSON arguments, receiving structured JSON back. This
spec proposes an **MCP server for Salvage** that exposes its commands as MCP
tools with structured JSON input and output, so an AI agent can operate
Salvage — drive its restore/verify/attest loop — directly, without screen-
scraping human-readable CLI text.

This is the "make agents effective" bet. Restore verification is a task agents
should be able to run unattended: point an agent at a backup config, have it
restore into a throwaway environment, assert the data is intact, and attest the
result into the ledger — then reason over the returned report. No widely-used
backup platform today is agent-operable; an MCP server makes Salvage's
verify/attest loop directly drivable by an agent, which is a real differentiator
rather than a cosmetic one.

**This spec depends on [[spec 0026]] (the machine-readable output contract) and
must land after it.** Today `run` and `verify` do not accept `-json`, and the
report carries no `schema_version` field. An MCP tool that returns
human-formatted CLI text, or an unversioned JSON blob whose shape can drift, is
not something an agent can reliably parse across releases. 0026 gives every
exposed command a stable, versioned structured payload; the MCP tools are thin
adapters over that payload. Without 0026 the tool outputs have nothing stable to
return, so 0026 is a hard prerequisite, not a nicety.

The server must also respect three existing platform contracts:
- **[[spec 0027]] (report redaction)** — tool output is handed straight to an
  agent/LLM, the worst possible place to leak a DSN, password, or `pass_env`
  value. Everything the MCP server returns MUST go through the same redaction
  0027 applies to reports and evidence.
- **[[spec 0003]] (isolation)** — restores still run in throwaway, network-
  isolated environments; being invoked by an agent changes nothing about the
  restore's containment.
- **[[spec 0014]] (accounts and CLI auth)** — hosted `attest`/`verify` calls
  authenticate with an API key **by reference** (from the environment or
  `~/.salvage/credentials`), exactly as the CLI does. The MCP server never takes
  a key as a tool argument.

## Goals

- An MCP server entrypoint that speaks MCP over stdio and exposes a curated set
  of Salvage commands as MCP tools.
- Each tool returns the **versioned, structured JSON** defined by [[spec 0026]]
  (report with `schema_version`, verify result, chain/fleet listings) — never
  scraped human text.
- Well-specified JSON input and output schemas per tool, advertised to the host
  so an agent can call them correctly and validate what comes back.
- A **read-only vs. mutating** classification on every tool, surfaced in the
  tool metadata, so a host can gate or require confirmation for state-changing
  calls (ledger writes, config emission).
- Auth **by reference** for hosted calls ([[spec 0014]]): the API key is read
  from the environment / `~/.salvage/credentials`, identically to `salvage
  attest`, and never appears in tool arguments or tool output.
- Secret-safety alignment with [[spec 0027]]: no tool result contains secret
  material.
- Keep dependencies minimal in the spirit of the repo (currently stdlib +
  `gopkg.in/yaml.v3`).

## Non-goals

- **Re-implementing engine logic.** The MCP server is an adapter. It invokes the
  same code paths (or the same binary) the CLI does; it does not restore,
  validate, sign, or attest by any new path. Every verdict still comes from the
  existing orchestrator ([[spec 0016]]).
- **A new auth mechanism.** Hosted calls reuse [[spec 0014]] API-key-by-reference
  auth verbatim. The MCP server does not introduce login flows, token exchange,
  or key storage of its own.
- **Exposing `login`/`logout`.** Interactive browser device-flow login
  ([[spec 0014]] R6–R7) is a human action, not an agent tool. The agent inherits
  whatever credential the environment already holds. `version` is trivial and
  need not be a distinct tool (it MAY be folded into server metadata).
- **A hosted/multi-tenant MCP endpoint.** v1 is a local server the agent runtime
  spawns alongside itself (stdio). A shared network-hosted MCP service is future
  work (see Open questions).
- **Sandboxing the agent's judgment.** The server classifies and gates tools and
  redacts output; it does not attempt to reason about whether a given restore is
  safe to run. Guardrails for agent-invoked restores are bounded by [[spec 0003]]
  isolation plus host-side gating (see Design and Open questions).

## Design

### Entrypoint

The server is exposed as a **`salvage mcp` subcommand** (v1 preference): it keeps
the single-binary distribution story intact (an agent runtime configures
`salvage mcp` as its MCP command, the same way it would configure any CLI-shaped
server), shares the config loader, engine registry, redaction, and credential
resolution already compiled into the binary, and adds no second artifact to
build, sign, or ship. Whether the server should instead be a separate
binary/module (`salvage-mcp`) — to isolate any MCP dependency from the core CLI —
is an Open question.

`salvage mcp` starts an MCP server on stdio, advertises its tool set, and serves
requests until the transport closes. It is itself non-interactive and inherits
the process's environment and `~/.salvage/credentials`.

### Exposed tools → commands

The server exposes a curated subset of commands as tools. Each tool wraps the
corresponding command's [[spec 0026]] structured output and returns it verbatim
(post-redaction):

| Tool | Wraps | Returns (0026) | Class |
|---|---|---|---|
| `salvage_run` | `run` | report JSON (`schema_version`, verdict, per-check results) | read-only-ish |
| `salvage_check` | `check` | report/check JSON | read-only-ish |
| `salvage_inspect` | `inspect` | resolved-config / plan JSON | read-only |
| `salvage_last_good` | `last-good` | backup-chain listing JSON | read-only |
| `salvage_fleet` | `fleet` | per-stanza fleet survey JSON | read-only |
| `salvage_verify` | `verify` | attestation verify result JSON | read-only |
| `salvage_attest` | `attest` | submitted attestation receipt JSON | **mutating** (ledger write) |
| `salvage_scaffold` | `scaffold` | suggested-config / checks JSON | **mutating** (emits config) |

Notes on classification:
- `run` and `check` are marked **read-only-ish**, not purely read-only: they do
  not mutate Salvage's own durable state (no ledger write), but they *do* execute
  a restore, which runs a Docker container and — for `target.type: exec`
  ([[spec 0020]]) — the customer's own restore command. They are read-only with
  respect to Salvage's records and to the production system (the restore lands in
  an isolated throwaway per [[spec 0003]]), but they are not side-effect-free at
  the OS level. The classification MUST make this distinction explicit so a host
  can decide whether to gate them (see "Sandboxing").
- `inspect`, `last-good`, `fleet`, `verify` are genuinely **read-only**: they
  read config, a backup repository, or the hosted ledger and mutate nothing.
- `attest` is **mutating**: it writes an attestation into the append-only ledger
  ([[spec 0014]] R3). A host SHOULD be able to require confirmation before it
  fires.
- `scaffold` is **mutating** in the weak sense that it *emits config* an agent
  may then persist; it writes nothing hosted.

`schedule` is not exposed as a call-and-forget tool in v1: it emits scheduling
config (cron / launchd / systemd fragments) for a human to install, and letting
an agent silently install unattended runs is exactly the kind of side effect that
wants a human in the loop. It MAY be exposed later as an explicitly-mutating,
gated tool that only *emits* the fragment as JSON (never installs it).

### Tool input/output schemas

Every tool advertises a JSON Schema for its arguments and documents its result
shape:

- **Inputs** are the command's real knobs: `salvage_run` takes `{ "config":
  "<path>" }` (or an inline config document — Open question), optional selectors,
  and nothing else. Credentials are **never** an input (see Auth). Unknown or
  malformed arguments produce a structured tool error, not a panic or a scraped
  usage string.
- **Outputs** are the [[spec 0026]] payloads verbatim: a `salvage_run` result is
  the versioned report object, carrying `schema_version` so the agent can branch
  on shape across releases. A `salvage_verify` result is the verify object
  (genuine / not-found / tampered + attestation metadata). Errors are returned as
  MCP tool errors carrying the operational-vs-verdict distinction: a **verdict
  fail** (exit 1) is a *successful* tool call whose report says `verdict: fail`;
  an **operational error** (exit 2 — Docker down, missing secret, unreachable
  endpoint) is an MCP tool error with a structured reason. This preserves the
  CLI's exit-code contract at the tool boundary so an agent can tell "the backup
  is bad" from "I couldn't run the check."

### Auth passthrough (by reference)

Hosted calls — `salvage_attest` and `salvage_verify` — resolve their API key
exactly as `salvage attest` does ([[spec 0014]] R7): flag/config is not used in
the MCP path, so resolution is **environment key → `~/.salvage/credentials`**.
The key is **never** a tool argument (an agent must not be able to read, pass, or
log it) and **never** appears in any tool output. If no credential is resolvable,
the tool returns a structured "not authenticated" operational error naming the
remedy (a human runs `salvage login`), not a stack trace. The endpoint resolves
the same way (`credentials` → default).

### Secret-safety ([[spec 0027]])

Every byte the server returns to the host passes through the [[spec 0027]]
redaction path already applied to reports and evidence packs. Concretely: DSNs,
passwords, `pass_env` values, `Authorization: Bearer <key>` material, and restore
stdout/stderr tails are redacted before they enter a tool result. Because MCP
output feeds an LLM context window, the server MUST treat *every* result field as
attacker-reachable and MUST NOT add any new unredacted surface (e.g. echoing raw
config back with secrets inlined). Tool-error messages are held to the same bar.

### Transport

**stdio first.** The agent runtime spawns `salvage mcp` as a child process and
speaks MCP over its stdin/stdout — the standard, lowest-friction local transport,
and the one that composes with the by-reference credential model (the child
inherits the parent's environment and home). An **HTTP transport** (for a
network-reachable server) is deferred and is an Open question, because it drags
in listener lifecycle, its own authn/authz for the MCP endpoint itself
(distinct from [[spec 0014]] attestation auth), and a larger attack surface.

### Sandboxing agent-invoked restores

`salvage_run`/`salvage_check` execute real restores. Two containment layers
already exist and are inherited unchanged: [[spec 0003]] isolates the restored
target (throwaway container, network-isolated) so a `run` cannot reach
production. For `target.type: exec` ([[spec 0020]]), the restore runs the
**customer's own command** with the Salvage process's privileges — trusted config
input, but an agent choosing *which* config to run is a new actor in that trust
chain. The MCP server's contribution to guardrails is the read-only-vs-mutating
classification (so a host can gate `run`/`check`/`attest`) plus honest tool
descriptions stating that `run` executes a restore. It does **not** add a new
sandbox around exec commands (that remains [[spec 0020]]'s posture). How much
further to go — an allow-list of configs an agent may run, a dry-run/`inspect`-
only default, per-tool host confirmation — is an Open question.

### Dependencies

The repo's entire dependency surface today is stdlib + `gopkg.in/yaml.v3`. MCP is
a JSON-RPC-shaped protocol over a byte stream; a minimal server (framing +
`encoding/json` + a small tool dispatch table) is implementable with the standard
library alone, preserving the zero-marginal-dependency ethos. Whether to instead
adopt a maintained MCP library — accepting the **first new dependency** in
exchange for protocol conformance and forward compatibility as MCP evolves — is
an explicit Open question weighed below.

## Requirements

**R1 — MCP server entrypoint.** There MUST be an MCP server entrypoint exposed as
`salvage mcp` (subject to the entrypoint Open question). It MUST speak MCP over
stdio, respond to tool-list requests by advertising its tool set, and serve tool
calls until the transport closes. It MUST be non-interactive and MUST inherit the
process environment and `~/.salvage/credentials`.

**R2 — Tool set over versioned JSON (depends on [[spec 0026]]).** The server MUST
expose, at minimum, the tools `salvage_run`, `salvage_check`, `salvage_inspect`,
`salvage_last_good`, `salvage_fleet`, `salvage_verify`, and `salvage_attest`,
each wrapping the corresponding command. Every tool result that carries a report
or verify payload MUST be the [[spec 0026]] structured JSON **including
`schema_version`** — never scraped human-readable CLI text. This requirement is
untestable until 0026 has landed; 0026 is a hard prerequisite.

**R3 — Advertised input/output schemas.** Each tool MUST advertise a JSON Schema
for its arguments to the host. Malformed or unknown arguments MUST produce a
structured tool error, not a crash or a raw usage dump. Each tool's result MUST
conform to the documented [[spec 0026]] payload for the command it wraps.

**R4 — Read-only vs. mutating classification.** Every exposed tool MUST carry a
machine-readable classification distinguishing **read-only** (`inspect`,
`last-good`, `fleet`, `verify`), **read-only-ish / restore-executing**
(`run`, `check` — no Salvage-state mutation but they run a container and possibly
a customer command), and **mutating** (`attest` writes the ledger; `scaffold`
emits config). The classification MUST be exposed in tool metadata so a host can
gate or require confirmation for non-read-only calls. `login`/`logout` MUST NOT
be exposed as tools; `schedule` MUST NOT auto-install anything.

**R5 — Auth by reference ([[spec 0014]]).** Hosted calls (`salvage_attest`,
`salvage_verify`) MUST resolve the API key and endpoint by reference —
environment key → `~/.salvage/credentials` — exactly as `salvage attest` does.
The key MUST NOT be accepted as a tool argument and MUST NOT appear in any tool
output or error. Absence of a resolvable credential MUST yield a structured
"not authenticated" operational error naming the remedy.

**R6 — Secret-safety ([[spec 0027]]).** Every tool result and every tool-error
message MUST pass through the [[spec 0027]] redaction path before leaving the
server. No DSN, password, `pass_env` value, bearer token, or raw restore
stdout/stderr containing secrets may appear in any output handed to the host. The
MCP path MUST add no new unredacted surface relative to the report/evidence path.

**R7 — Operational-vs-verdict fidelity.** The CLI exit-code contract MUST survive
the tool boundary: a **verdict fail** (exit 1) MUST be a successful tool call
whose payload states `verdict: fail`; an **operational error** (exit 2) MUST be
an MCP tool error carrying a structured reason. A host MUST be able to
distinguish "the backup is bad" from "the check could not be run."

**R8 — Isolation preserved ([[spec 0003]]).** A `salvage_run`/`salvage_check`
invoked via MCP MUST perform the identical isolated restore the CLI performs — no
weakening of containment, network isolation, or teardown because the caller is an
agent.

**R9 — Transport.** The server MUST support **stdio** transport in v1. Any HTTP
transport is out of scope for v1 (Open question); if later added it MUST NOT
change the by-reference credential model for attestation auth and MUST carry its
own endpoint authn/authz.

**R10 — Dependency discipline.** The server SHOULD be implementable within the
module's existing dependency surface (stdlib + `gopkg.in/yaml.v3`). Introducing
an MCP library — the first new dependency — MUST be a deliberate, documented
decision (see Open questions), not incidental.

## Open questions

- **Entrypoint: `salvage mcp` subcommand vs. a separate binary/module.** The
  subcommand keeps one artifact and shares config/redaction/credentials; a
  separate `salvage-mcp` would isolate any MCP dependency from the core CLI and
  let the server version independently. v1 leans subcommand.
- **stdlib MCP vs. a library (the first new dependency).** A hand-rolled JSON-RPC
  server keeps the zero-marginal-dependency ethos but tracks the protocol
  manually as MCP evolves; a maintained library buys conformance at the cost of
  the module's first third-party dependency beyond `yaml.v3`. If a separate
  binary is chosen, the dependency need not touch the core CLI.
- **HTTP transport.** When (if) to add a network-reachable server, and how to
  authenticate the MCP endpoint itself (distinct from [[spec 0014]] attestation
  auth) without a shared-secret footgun.
- **Config input shape.** Whether `salvage_run` takes only a config **path** or
  may accept an **inline config document** as an argument. Inline is more
  agent-friendly (no filesystem round-trip) but widens the surface an agent can
  hand to a restore engine — interacts directly with the sandboxing question.
- **Guardrails for agent-invoked restores.** Beyond [[spec 0003]] isolation and
  the read-only/mutating gate: an allow-list of runnable configs, an
  `inspect`-only default posture, per-tool host confirmation, or a dry-run mode.
  How much belongs in the server vs. the host runtime.
- **`schedule` as a gated emit-only tool.** Whether to expose `schedule` at all,
  and if so strictly as a JSON *emitter* an agent can read but that installs
  nothing.
- **Resource/prompt surfaces.** Whether to also expose MCP *resources* (e.g. a
  read-only view of recent reports or the ledger) and *prompts*, or keep v1 to
  tools only.

## Acceptance criteria

1. An MCP client connecting to `salvage mcp` over stdio can **list the tools**
   and sees at least `salvage_run`, `salvage_check`, `salvage_inspect`,
   `salvage_last_good`, `salvage_fleet`, `salvage_verify`, and `salvage_attest`,
   each with an advertised argument schema and a read-only/mutating
   classification.
2. Calling `salvage_run` against a sample config returns a **versioned report
   JSON** (carrying `schema_version` per [[spec 0026]]) with a verdict and
   per-check results — not scraped CLI text.
3. Calling `salvage_verify` on an attestation id returns a structured verify
   result (genuine / not-found / tampered + metadata), authenticating by
   reference ([[spec 0014]]) with the key never appearing in the arguments or the
   result.
4. **No secret material** (DSN, password, `pass_env` value, bearer token, raw
   restore stderr containing secrets) appears in any tool output or tool-error
   message, confirming [[spec 0027]] redaction on the MCP path.
5. A verdict-fail config yields a *successful* tool call whose payload says
   `verdict: fail`; an operational failure (e.g. Docker unavailable) yields an
   MCP tool error with a structured reason — the exit-code contract preserved at
   the tool boundary (R7).
6. A `salvage_run` invoked via MCP performs the same isolated restore as the CLI
   ([[spec 0003]]); no containment is weakened because the caller is an agent.
7. The build carries no new Go dependency unless the library decision (Open
   questions) is explicitly taken and recorded; the module otherwise remains
   stdlib + `gopkg.in/yaml.v3`.
