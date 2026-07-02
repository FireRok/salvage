# 0034 — Release process, versioning & changelog

- **Status:** Proposed
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

Salvage can *cut* a release — a `v*` tag on the public mirror triggers
GoReleaser (`.github/workflows/release.yml`) — but nothing defines what a
release *means*. Concretely, today:

- **Version numbers carry no promise.** The README warns "expect breaking
  changes before v1" (`README.md:7-11`) and stops there. Nothing says what a
  minor vs patch bump signals, before or after 1.0, or which surfaces a
  version number is even *about*. Operators pin Salvage in cron jobs and CI
  (`salvage schedule` exists precisely to wire it into cron/systemd,
  [[spec 0015]]); a pinned tool with undefined version semantics can't be
  upgraded confidently.
- **There is no changelog.** No `CHANGELOG.md` exists in the repo, and the
  release notes are configured to be derived from the commit log
  (`.goreleaser.yaml`: `changelog: use: github`). Development is
  Forgejo-first with a curated snapshot released to public GitHub
  (`README.md:237-244`), so the public commit history is a release-shaped
  digest, not a narrative of user-visible change — release notes derived from
  it will be sparse at best and misleading at worst.
- **"Supported" is undefined.** When a fix lands, does it go only into the
  next release, or are older minors patched? An operator on `vX.(Y-1)` has no
  documented answer.

The three release-adjacent specs divide as follows, so they compose without
overlap:

| Spec | Owns |
|---|---|
| [[spec 0033]] | The **artifacts**: what a release ships (binaries, checksums, signature, install paths). |
| **0034 (this spec)** | The **communication**: what version numbers mean, where changes are written down, what a release *is*, what "supported" means. |
| [[spec 0035]] | The **behavioral guarantees**: what actually keeps working across an upgrade (config, reports, attestations, hosted API). |

This spec defines only the *public* contract. The private Forgejo-side
workflow, branch strategy, and mirror mechanics are internal tooling and out
of scope — the contract is expressed entirely in terms of what appears on the
public repository and its releases.

## Goals

- **Semantic versioning with an enumerated surface.** A version number is a
  claim about specific, listed public surfaces — not a vibe. Pre-1.0 and
  post-1.0 semantics are both written down.
- **A human-curated `CHANGELOG.md`** in the repository: the single narrative
  of user-visible change, and the source from which GitHub Release notes are
  produced (replacing the derived commit log).
- **A crisp definition of "a release"**: an annotated `vX.Y.Z` tag on the
  public repository, its [[spec 0033]] artifact set, and a changelog entry —
  all three, always.
- **A published support statement** so operators know which releases receive
  fixes.
- **One source of truth for the version string** — the tag — flowing into the
  binary (`internal/version`), the artifacts, and the changelog heading
  identically.

## Non-goals

- **Internal workflow.** Branching, review, the private-to-public release
  procedure, and mirror tooling are not specified here; only their observable
  output is.
- **A fixed calendar cadence.** Salvage releases when there is something to
  release. What this spec fixes is that *every* release is complete (tag +
  artifacts + changelog), not *when* releases happen.
- **Long-term-support branches.** The support statement below is deliberately
  minimal; LTS commitments can be introduced later without breaking it.
- **Hosted-service release management.** The notary/control-plane deploys
  continuously; its compatibility obligations to released CLIs are
  [[spec 0035]] R5's concern, not a versioning scheme here.
- **Behavioral compatibility rules** — what may change at which bump lives in
  [[spec 0035]]; this spec only defines what the bump *signals*.

## Design

### The versioned surface

A Salvage version number makes claims about exactly these public surfaces:

1. **CLI**: commands, flags, and argument semantics (`cmd/salvage/main.go`
   usage surface).
2. **Exit codes**: the `0`/`1`/`2` contract ([[spec 0000]] R4) — frozen at
   every bump, forever ([[spec 0035]] R6).
3. **Config schema**: the YAML surface documented in
   `docs/guide/02-configuration.md`.
4. **Report JSON**: versioned independently by `schema_version`
   ([[spec 0026]]); a report-schema bump is a breaking change for the binary
   that emits it.
5. **Attestation formats & hosted API**: the envelope `verify` checks and the
   endpoints the CLI calls ([[spec 0012]], [[spec 0014]]).
6. **Evidence pack format** ([[spec 0019]]).

Anything not on this list (internal packages, log/stderr text, human-readable
summaries, dev harnesses under `dev/`) may change at any bump without notice.

### Version semantics

**Pre-1.0 (now):** `0.MINOR.PATCH`. A **minor** bump may include breaking
changes to any versioned surface, each explicitly flagged in the changelog. A
**patch** bump is fixes and strictly additive changes only — safe to take
blind. This makes the README's "expect breaking changes before v1" precise:
breakage is *allowed* pre-1.0, but only at minor bumps and only announced.

**Post-1.0:** standard semver over the enumerated surface. **Major** =
breaking change to any listed surface; **minor** = additive; **patch** =
fixes. The rules for *what counts* as breaking for config and reports are
[[spec 0035]]'s R1/R3.

### CHANGELOG.md

A `CHANGELOG.md` at the repo root, newest release first, in the
Keep-a-Changelog shape: each release heading is `## [X.Y.Z] — YYYY-MM-DD`
with entries grouped under `Added` / `Changed` / `Deprecated` / `Removed` /
`Fixed` / `Security`. Breaking changes are prefixed `**Breaking:**` and
deprecations name the release at which removal is planned (feeding
[[spec 0035]] R2's deprecation cycle). An `## [Unreleased]` section
accumulates entries between releases so changelog-writing is incremental, not
archaeological.

Because specs ship publicly, the changelog follows the same content rules as
`specs/` — user-visible behavior only.

### Release notes come from the changelog

GoReleaser's `changelog.use: github` (derived commit log) is replaced: the
GitHub Release body for `vX.Y.Z` is that release's `CHANGELOG.md` section
(GoReleaser supports supplying release notes from a file). The commit log of
a curated mirror stops masquerading as release notes.

### Support statement

Published in the docs: **fixes land in the next release from the latest
version; released artifacts are never mutated.** If a fix must reach users
urgently, it ships as a new patch release of the latest minor. No backports
to older minors are promised (they may happen; they are not owed). This is
the honest minimal statement for a small team, and it is compatible with
adding LTS later.

## Requirements

**R1 — Enumerated versioned surface.** The public docs MUST enumerate the
surfaces a version number covers (the six listed in Design). Changes outside
that list MUST NOT be treated as breaking for versioning purposes.

**R2 — Version semantics.** Pre-1.0: breaking changes to any versioned
surface MUST occur only at a minor bump and MUST be flagged in the changelog;
patch releases MUST be non-breaking and additive-only. Post-1.0: breaking
changes MUST occur only at a major bump. The semantics MUST be stated in the
public docs, not merely in this spec.

**R3 — Changelog exists and is curated.** The repository MUST contain a
root-level `CHANGELOG.md` with one section per release (`X.Y.Z` + date,
categorized entries) and an `Unreleased` section. Every release MUST add a
section describing its user-visible changes; a release with an empty or
missing changelog section is not a valid release. Breaking changes MUST be
explicitly marked; deprecations MUST name the planned removal release.

**R4 — Release notes derive from the changelog.** The published GitHub
Release body for `vX.Y.Z` MUST be that release's changelog section (verbatim
or trivially reformatted), not a derived commit log. The
`changelog.use: github` configuration in `.goreleaser.yaml` MUST be replaced
accordingly.

**R5 — A release is tag + artifacts + changelog.** Every release MUST consist
of: an annotated `vX.Y.Z` tag on the public repository, the [[spec 0033]] R1
artifact set attached to its GitHub Release, and the R3 changelog section.
Any one without the others is an incomplete release and MUST be completed or
withdrawn.

**R6 — Single version source.** The tag MUST be the sole source of the
version string: the value embedded in the binary
(`internal/version.Version`), the artifact filenames, and the changelog
heading MUST all equal the tag (less the `v` prefix) with no independently
maintained version constant anywhere in the tree.

**R7 — Immutable releases.** A published tag and its artifacts MUST never be
re-pointed, re-uploaded, or deleted. Fixes ship as a new version. (This is
the binary-artifact analog of the attestation ledger's append-only posture,
[[spec 0012]].)

**R8 — Support statement.** The public docs MUST state where fixes land (next
release from latest, no promised backports) so an operator can plan upgrade
expectations. Changing that statement is itself a changelog-worthy event.

## Open questions

- **When to declare 1.0.** The README's own bar ("signing and the hosted
  attestation service are evolving") suggests 1.0 waits for [[spec 0026]]
  (versioned reports) and [[spec 0035]] (compat policy) to be Implemented —
  1.0 is a promise-keeping capability, not a feature count. Decide the gate
  explicitly.
- **Changelog enforcement.** Should CI on the public repo fail a tag whose
  changelog lacks a matching section (mechanical guard for R3/R5), or is this
  process-only? A tiny check script is cheap and recommended.
- **Retroactive changelog.** Whether to backfill `CHANGELOG.md` for tags that
  predate this spec, or start it at the first post-spec release with a "prior
  history: see release tags" note.
- **Pre-release channels.** Whether `-rc.N` pre-release tags (which GoReleaser
  handles natively) are worth the ceremony before 1.0, or whether pre-1.0
  minors already serve that role.

## Acceptance criteria

1. `CHANGELOG.md` exists at the repo root with an `Unreleased` section and a
   section for the most recent release, categorized per R3.
2. The docs state the versioned surface (R1), the pre/post-1.0 semantics
   (R2), and the support statement (R8), each findable from the docs guide
   index.
3. The next release after this spec lands satisfies R5: tag, full artifact
   set, and changelog section all present; its GitHub Release body matches
   the changelog section (R4).
4. `salvage version` on that release's binaries prints exactly the tag
   version; the artifact filenames embed the same string (R6).
5. `.goreleaser.yaml` no longer contains `use: github` under `changelog:`
   (R4).
6. A dry-run "breaking change in a patch release" review scenario is
   rejectable by pointing at R2 as published in the docs — i.e., the rule is
   citable, not tribal.
