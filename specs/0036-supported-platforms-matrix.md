# 0036 — Supported-platforms & engine-version matrix

- **Status:** Proposed
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

"Does Salvage support my stack?" has no citable answer today. The facts exist,
but scattered and mostly as *code*, not *statement*:

- **Host platforms** are whatever `.goreleaser.yaml` builds
  (`linux`/`darwin` × `amd64`/`arm64`) — an artifact list, not a support
  claim.
- **Docker** is "required" (`README.md:14-17`) with no minimum version, and
  the `exec` engine is Docker-free (`README.md:19-24`) — the one nuance that
  *is* documented.
- **Engine-world versions** live as hardcoded default images in
  `internal/config/config.go:280-330`: `postgres:16`, `mysql:8`, `mongo:7`,
  `restic/restic:latest`, `ghcr.io/borgbackup/borg:stable`. Two of those five
  defaults are **floating tags** — `latest` and `stable` — which means the
  default behavior of a pinned Salvage release changes when an upstream image
  moves, and no honest version claim can even be made about it.
- **What was actually verified** is recorded unevenly: the README attests a
  TimescaleDB 17 production restore from R2 (`README.md:7-11`); MongoDB was
  confirmed live, while restic/borg/MySQL end-to-end verification was
  explicitly deferred (`specs/BACKLOG.md` section C). So the honest current
  answer to "is engine X supported at version Y?" ranges from "yes, verified"
  to "it was written to spec but never run against a real daemon."

The absence hurts in both directions. Operators evaluating Salvage cannot
determine coverage of their stack without reading Go source. And Firerok has
no bounded promise: without a matrix, *every* combination is implicitly
claimed, and every "doesn't work on my ancient PG" report is arguably a bug.
A published matrix is simultaneously an adoption document and a scope fence.

This spec creates the matrix as a **contract with tiers**, ties every
"Supported" claim to a reproducible verification, and fixes the floating
defaults that make claims impossible. It complements [[spec 0035]] (Salvage's
own surfaces over time); this spec is about the *outside world* — OSes,
Docker, and engine-ecosystem versions — at a point in time.

## Goals

- **One published matrix document** covering: host OS/arch for the binary;
  minimum Docker version (and the Docker-free `exec` carve-out); and, per
  engine, the supported artifact-format and server versions (PG majors,
  MySQL majors, MongoDB majors, restic repo versions, borg repo versions,
  pgBackRest versions).
- **Two honest tiers.** *Supported* — verified by a recorded, reproducible
  run; issues are treated as bugs. *Expected* — believed to work by design;
  best-effort. Nothing is listed without a tier, and nothing is "Supported"
  without evidence.
- **Verification-backed claims.** Every Supported cell maps to a reproducible
  harness (the `dev/<engine>/make-backup.sh` pattern that pgBackRest, MySQL,
  and MongoDB already have) and a recorded passing run — turning BACKLOG
  section C's verification debt into a standing gate rather than a one-time
  cleanup.
- **Deterministic defaults.** Default restore images are pinned to specific
  major-version tags so a Salvage release's default behavior is stable and
  matrix-describable.
- **Defined out-of-matrix behavior.** Where a version mismatch is detectable
  (e.g. `PG_VERSION` in a data dir — `salvage inspect` already reads it),
  Salvage says so plainly instead of failing mysteriously.

## Non-goals

- **Growing the matrix itself.** Which new versions/platforms to support is
  ongoing product work; this spec creates the structure, the evidence bar,
  and the initial honest snapshot — not an expansion commitment.
- **Windows.** Out of the matrix entirely until [[spec 0033]]'s Open question
  resolves; the matrix's job is to say so explicitly.
- **Managed-provider snapshots** — out of scope per [[spec 0000]] R7; the
  matrix restates the boundary.
- **Guaranteeing upstream tools' own bugs.** A Supported claim means *Salvage
  correctly drives* that version; it does not indemnify pgBackRest/restic/
  borg defects.
- **Compatibility of Salvage's own surfaces across Salvage versions** —
  [[spec 0035]].

## Design

### The matrix document

A single page in the public docs (natural home: `docs/guide/`, linked from
the README and getting-started), structured as:

1. **Salvage binary**: OS/arch table (from [[spec 0033]] R1's artifact set),
   plus "anything Go supports" as *Expected* for source builds.
2. **Runtime prerequisites**: minimum Docker Engine version for the container
   engines; the `exec` engine's no-Docker row; Go minimum (1.23, `go.mod:3`)
   for source builds only.
3. **Per-engine tables**, one row per engine × version-range, columns:
   *artifact/format*, *server/tool versions*, *tier*, *evidence* (link to the
   harness + the recorded run). Initial content is the honest snapshot:
   verified combinations (e.g. PG 16/17 incl. TimescaleDB, MongoDB 7) enter
   as Supported; spec-complete-but-unverified combinations (restic, borg,
   MySQL 8 pending their live end-to-end runs) enter as Expected until their
   section-C verification lands, at which point they graduate.

The matrix carries a "last verified for release vX.Y.Z" line and changes to
it are changelog-visible ([[spec 0034]] R3).

### Evidence: the harness pattern, generalized

`dev/pgbackrest/`, `dev/mysql/`, `dev/mongodb/` already contain reproducible
make-backup harnesses. The rule this spec adds: **a Supported cell exists iff
a harness exists for it and a passing `salvage run` against its output is
recorded** (in CI where feasible; as a documented manual run where CI can't
host the daemon). This is deliberately the same evidence bar the engine specs
set for themselves — the matrix just makes it a queryable, user-facing index
of that evidence rather than folklore.

CI need not run the full matrix on every push; a scheduled or release-gating
matrix job suffices (cost-driven; Open question).

### Pinned defaults

The floating defaults become pinned major tags:
`restic/restic:latest` → a specific `restic/restic:0.x` tag, and
`ghcr.io/borgbackup/borg:stable` → a specific version tag
(`internal/config/config.go:280,296`). Users can always override
`restore.image`; the *default* must be deterministic so the matrix can state
what an out-of-the-box run uses. Bumping a pinned default is an ordinary,
changelog-noted change.

### Out-of-matrix behavior

Detectable mismatches degrade with dignity:

- Where Salvage can read a version before committing (PG major from
  `PG_VERSION` — the `inspect` path; server versions via the engine's own
  tooling where cheap), an out-of-matrix version produces a clear message
  naming the detected version and the matrix, as a stderr warning when
  proceeding is plausible or an operational error (exit 2) when it is not.
- Where it cannot detect, the matrix itself is the answer: the docs page is
  the first support response, not a shrug.

## Requirements

**R1 — Published matrix.** The public docs MUST contain a single
supported-platforms page covering: binary OS/arch; minimum Docker version and
the `exec` no-Docker exception; and per-engine supported artifact-format and
server/tool version ranges. It MUST be linked from the README and the
getting-started guide.

**R2 — Tiered claims.** Every matrix cell MUST carry a tier: *Supported*
(verified; defects treated as bugs) or *Expected* (best-effort). The docs
MUST define both terms. Combinations absent from the matrix are unclaimed.

**R3 — Evidence-backed Supported tier.** Every Supported cell MUST reference
a reproducible verification harness (the `dev/<engine>/` pattern) and a
recorded passing run of it. A cell without evidence MUST be listed as
Expected or removed. Engines whose end-to-end verification is currently
deferred (restic, borg, MySQL — `specs/BACKLOG.md` section C) MUST NOT enter
as Supported until that verification is recorded.

**R4 — Deterministic default images.** No default `restore.image` value in
`internal/config/config.go` may use a floating tag (`latest`, `stable`, or
equivalent). Defaults MUST pin at least a major version, and each default
MUST appear in the matrix. Changing a default is a changelog-visible change
([[spec 0034]] R3).

**R5 — Detectable mismatches are named.** Where an engine can cheaply detect
the artifact or server version before or during a run, encountering an
out-of-matrix version MUST produce a message that names the detected version
and refers to the supported matrix — as a warning when the run can
meaningfully proceed, as an operational error (exit 2) when it cannot. It
MUST NOT surface as an unexplained restore failure when detection was
possible.

**R6 — Matrix is versioned with releases.** The matrix MUST state which
Salvage release it was last verified against, and matrix changes (tier
promotions, new versions, dropped versions, default-image bumps) MUST appear
in the changelog. Dropping a previously Supported version follows the
deprecation discipline of [[spec 0035]] R2 in spirit: announced before
removed.

## Open questions

- **Version floors.** The concrete initial ranges — oldest PG major (13?
  14?), MySQL 8-only or 5.7, MongoDB 6/7, restic repo v1/v2, borg 1.x vs 2.x,
  pgBackRest minimum — need deciding from evidence and demand, not defaulted
  in this spec. MariaDB's relationship to the MySQL engine is the largest
  single unknown.
- **CI shape and cost.** Full matrix per release vs a scheduled sampled
  matrix plus release-gating smoke set; where the Docker-daemon-hosting
  runners live. Interacts with existing Forgejo CI (`.forgejo/workflows/`).
- **Docker alternatives.** Podman and colima largely present Docker-compatible
  daemons; whether to list them (as Expected?) or stay silent.
- **Evidence publication.** Whether recorded runs are published as raw CI
  links, as attestations of the harness runs (pleasingly self-referential),
  or summarized in the matrix page only.

## Acceptance criteria

1. The matrix page exists in `docs/guide/`, linked from `README.md` and
   `01-getting-started.md`, containing all three sections of R1 with every
   cell tiered per R2.
2. Every Supported cell links to a `dev/<engine>/` harness and a recorded
   passing run; restic/borg/MySQL appear as Expected unless their live
   end-to-end runs have been recorded (R3).
3. `grep -n "latest\|stable" internal/config/config.go` shows no floating
   tag in any default image assignment; the pinned defaults each appear in
   the matrix (R4).
4. Running against a data directory whose detected PG major is outside the
   matrix produces a message naming the detected version and the matrix
   (R5).
5. The matrix page names the release it was last verified against, and the
   changelog for the release that introduces it records the matrix's
   creation (R6).
