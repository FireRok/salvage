# 0037 — Documentation coverage parity

- **Status:** Proposed
- **Created:** 2026-07-01
- **Owner:** Firerok

## Context

The user guide (`docs/guide/`, seven chapters) is well-structured and was
accurate when written — and has since fallen behind the code by three whole
engines. Concretely, as of today:

- `docs/guide/03-engines.md:21` states "Three engines ship today:
  **postgres**, **restic**, and **exec**" (restated at `:137`), while the borg
  ([[spec 0022]]), MySQL ([[spec 0024]]), and MongoDB ([[spec 0025]]) engines
  are all **Implemented** — with defaults wired in
  (`internal/config/config.go:296-330`) and worked example configs sitting at
  the repo root (`salvage.borg.example.yaml`, `salvage.mysql.example.yaml`,
  `salvage.mongodb.example.yaml`) that no doc references.
- The configuration reference's check-kind coverage
  (`docs/guide/02-configuration.md:125-171`) documents `sql`, the filesystem
  kinds, `command`, and `http` — but not MongoDB's `collection_count` and
  `doc_query` kinds ([[spec 0025]]), which are therefore undiscoverable
  without reading a spec or the source.
- The README's Sources table (`README.md:78-83`) lists only postgres and
  restic rows; the guide index's chapter summaries
  (`docs/guide/README.md:13-18`) have the same staleness.
- There is **no CI-integration documentation** at all. The scheduling chapter
  covers cron/systemd (`docs/guide/06-scheduling-and-monitoring.md`), but the
  most common evaluation path — "run `salvage run` as a scheduled CI job and
  gate on the exit code" — has no recipe, despite the exit-code contract
  ([[spec 0000]] R4) and machine output ([[spec 0026]]) existing precisely for
  it.

For a product whose adoption path is self-serve evaluation of a public repo, an
undocumented feature is indistinguishable from a missing one: an evaluator
skimming the guide today concludes Salvage cannot test their MySQL or MongoDB
backups, and no amount of implemented code corrects that impression. The
inverse failure is worse still for a verification product — docs *claiming*
more than ships would undercut the credibility the whole attestation story
depends on.

The root cause is structural, not negligence: engine specs define their own
acceptance criteria, and none of them includes "the guide knows about me."
This spec adds the missing invariant — **docs parity as a property of
Implemented, not a follow-up task** — plus the one-time remediation of the
current gaps and the missing CI-integration chapter.

## Goals

- **A parity invariant**: every engine (and every check kind) with an
  Implemented spec is discoverable and usable from the user guide alone — an
  engine section, its check kinds in the configuration reference, and a
  referenced worked example config.
- **One-time remediation** of the current known gaps: borg, MySQL, and
  MongoDB engine sections; `collection_count`/`doc_query` reference entries;
  the README Sources table; the guide index summaries.
- **A per-engine quickstart path**: each engine section carries a copy-paste
  route from its example config to a first verdict, so no engine's first run
  requires spelunking.
- **A CI-integration chapter**: recipes for running Salvage in scheduled CI
  (exit-code gating, `-json` capture, secrets-by-`pass_env`, the
  Docker-daemon requirement), which is both a docs gap and the natural home
  for [[spec 0033]]'s container-in-CI question.
- **Doc-referenced configs stay valid**: every example config the docs point
  at validates, mechanically, so documentation and code cannot silently
  diverge in the direction that burns users.

## Non-goals

- **A rendered documentation website.** Hosting/rendering the guide (vs
  markdown in-repo) is orthogonal; this spec is about *coverage*, whatever
  the rendering. The guide already ships in the public repo and versions with
  the code — that property is kept, not changed.
- **Docs for Proposed features.** Parity binds at Implemented. Documenting
  futures would create the over-claiming failure mode this spec exists to
  prevent.
- **Marketing and positioning copy.** The guide is operator documentation;
  positioning lives elsewhere.
- **API/hosted-service reference docs** beyond what the attestation chapter
  already covers — worthwhile, but a different artifact with a different
  audience; can be spec'd separately if the hosted surface grows.
- **The supported-versions matrix content** — that page is [[spec 0036]]'s;
  this spec only requires the guide link to it.

## Design

### The parity invariant

"Implemented" for an engine spec comes to mean: code ships **and** the guide
covers it. Coverage, concretely, is four artifacts:

1. a section in `03-engines.md` (what it restores, its `target.type` and
   `source` kinds, Docker or not, honest-scope notes — the shape the
   postgres/restic/exec sections already model);
2. reference entries in `02-configuration.md` for any check kinds or config
   keys the engine introduces;
3. a repo-root `salvage.<engine>.example.yaml` linked from both the engine
   section and the guide index (all six engines already *have* example
   configs — the gap is purely linkage for the newest three);
4. a row in the README Sources table.

Prospectively, this bar attaches to the transition itself: an engine spec's
move to Implemented status includes its docs coverage, exactly as it includes
its tests. (This adds a clause to the working definition of the Implemented
status in `specs/README.md` conventions; existing specs' statuses are not
retroactively churned — the three current violations are simply fixed.)

### Per-engine quickstart

Each engine section ends with a short "first run" block: copy the example
config, the two or three fields that must change (artifact path, image,
credentials env), then `salvage check` → `salvage run`. The getting-started
chapter stays Postgres-first (right default) but gains a pointer: "testing
something else? each engine section has its own first-run block."

### The CI-integration chapter

A new guide chapter (joining the index at `docs/guide/README.md`) with
recipes for the unattended-verification pattern in CI schedulers:

- the generic shape: scheduled job → `salvage run -config …` → gate on exit
  code (`0`/`1`/`2` table restated), capture the report via `-json`
  ([[spec 0026]]) or `report.out` as the build artifact;
- a worked GitHub Actions scheduled-workflow example and a generic-runner
  translation note (the pattern is three lines of any CI's YAML; one concrete
  example plus the contract beats five half-maintained vendor examples —
  Open question);
- the operational notes CI users hit first: the runner needs a reachable
  Docker daemon (except `exec` targets), secrets flow by name via
  `source.pass_env` from the CI secret store ([[spec 0003]] posture), and
  `salvage attest` in CI for the notary path ([[spec 0012]]).

### Keeping examples honest

Every example config referenced from the docs must pass `salvage check`'s
config-validation (the Docker preflight being environment-dependent is fine —
validation is the load-bearing part, and with BACKLOG S1's strict parsing it
becomes a real drift tripwire: a renamed key breaks the example loudly). This
runs in CI, so a config change that orphans a documented example fails before
it ships.

## Requirements

**R1 — Parity invariant.** Every engine whose spec is Implemented MUST have:
a section in `03-engines.md`; configuration-reference entries in
`02-configuration.md` for every check kind and config key it introduces; a
linked repo-root example config; and a row in the README Sources table. From
this spec's acceptance onward, an engine spec MUST NOT be marked Implemented
without this coverage.

**R2 — Remediation of current gaps.** The borg, MySQL, and MongoDB engines
MUST be brought to R1 coverage: engine sections; `collection_count` and
`doc_query` documented in the check-kinds reference; the three existing
example configs linked; README Sources rows added; the "three engines ship
today" claims (`03-engines.md:21`, `:137`) and the guide index summaries
corrected.

**R3 — Per-engine first run.** Each engine section MUST contain a first-run
block: which example config to copy, which fields must change, and the
`check`/`run` commands — sufficient to reach a first verdict for that engine
using the guide alone.

**R4 — CI-integration chapter.** The guide MUST gain a CI-integration chapter,
listed in the guide index, covering: exit-code gating (restating the
`0`/`1`/`2` contract), machine-readable report capture, the Docker-daemon
requirement and the `exec` exception, and secrets via `pass_env` from a CI
secret store — with at least one complete worked scheduled-CI example.

**R5 — Doc-referenced examples validate.** Every example config referenced
from `docs/guide/` or `README.md` MUST pass `salvage check`'s config
validation, and CI MUST enforce this so example/config drift fails a build
rather than a user.

**R6 — No forward claims.** The guide MUST NOT describe unshipped
functionality as available. Roadmap references MUST be marked as such (the
existing `03-engines.md` roadmap section's style is the model).

## Open questions

- **Mechanizing the parity check.** R1 is checkable by inspection; a small
  script could cross-reference Implemented engine specs against guide
  anchors and README rows and run in CI beside R5's config validation. Worth
  it now, or once a fourth drift incident occurs?
- **How many CI vendors.** One deep GitHub Actions example plus the generic
  contract, or additional named recipes (GitLab CI, Jenkins, Forgejo/Gitea
  Actions)? More examples help evaluators and rot faster. Recommended: one
  deep + generic translation notes, expand on demand.
- **The `salvage`-in-a-container recipe.** The CI chapter is the natural home
  for running Salvage itself as a container (socket-mount vs DinD), but that
  distribution artifact is [[spec 0033]]'s open question — the chapter should
  gain the recipe if and when that image ships.
- **Command-reference generation.** `04-commands.md` currently matches the
  `usage()` text (`cmd/salvage/main.go:65-88`) by hand-maintenance; whether
  to generate or CI-diff the two is a smaller instance of the same parity
  problem, deferred.

## Acceptance criteria

1. `grep -n "Three engines" docs/guide/03-engines.md` returns nothing; the
   engines chapter has sections for all six shipped engines, each with a
   first-run block (R1, R2, R3).
2. `grep -n "collection_count\|doc_query" docs/guide/02-configuration.md`
   finds reference entries for both kinds (R2).
3. The README Sources table contains rows for borg, MySQL, MongoDB, and exec
   targets, each linking a repo-root example config (R2).
4. A new CI-integration chapter exists, is listed in
   `docs/guide/README.md`'s contents, and contains a complete scheduled-CI
   worked example with exit-code gating and report capture (R4).
5. CI runs config validation over every doc-referenced example config, and
   deliberately misspelling a key in one example makes that CI check fail
   (R5).
6. Following only the guide, an evaluator can reach a first verdict for a
   MySQL backup and a MongoDB backup without opening `specs/` or the Go
   source (R1/R3, the point of the exercise).
