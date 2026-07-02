# 0011 — Fleet discovery (enumerate a whole pgBackRest repo)

- **Status:** Implemented
- **Created:** 2026-06-30
- **Owner:** Firerok

## Context

A pgBackRest repo commonly holds **many stanzas** — one per database cluster an
org backs up. Configuring Salvage one hand-written YAML at a time does not scale
to that, and it was the first objection raised about the tool ("so a DBA writes a
config for every database?"). `salvage scaffold` already removes the per-database
*check-writing* burden; fleet discovery removes the *"which databases even exist"*
burden by reading the repo and enumerating every stanza in it.

Fleet discovery is deliberately **cheap and metadata-only**: it reads
`pgbackrest info` (no restore) so pointing it at a large repo costs seconds, not
the hours a restore-everything sweep would. The expensive per-stanza work
(restore + introspect) is then opt-in, one stanza at a time, via the skeleton
configs it emits.

**Relationship to the hosted plane:** a live, always-current *fleet dashboard*
across many repos is a hosted/paid feature ([[spec 0008]]). This spec is only the
local, one-repo, one-shot enumeration — the OSS on-ramp.

## Goals

- `salvage fleet` enumerates every stanza in a pgBackRest repo with a one-line
  summary each (status, backup count, newest backup + age) — no restore.
- Optionally emit a ready-to-fill **skeleton config per stanza** so a fleet can be
  scaffolded by running `salvage scaffold` against each.

## Non-goals

- Restoring or introspecting each stanza automatically (that is N restores; keep
  fleet cheap). The skeletons hand that back to `scaffold`, opt-in per stanza.
- Cross-repo / always-on fleet views (hosted plane, [[spec 0008]]).
- Guessing each stanza's primary application database — a stanza's cluster can
  hold several databases and the right one is a human call. The skeleton inherits
  the base `restore.database` and tells the user to verify it.

## Requirements

**R1 — `salvage fleet` command.** For a pgBackRest source, enumerate every stanza
in the repo. `-o <dir>` also writes a per-stanza skeleton config. Exit `2` on
operational error; enumeration itself does not produce a pass/fail verdict.

**R2 — Enumerate all stanzas.** Read `pgbackrest info --output=json` **without**
`--stanza`, so every stanza in the repo is reported. Parsing lives in
`internal/pgbrinfo` as a pure, testable function returning each stanza's name,
status, and backups (newest first).

**R3 — Metadata only.** Fleet MUST NOT restore. Its cost is one `pgbackrest info`
call regardless of repo size.

**R4 — Per-stanza summary.** For each stanza report: name, pgBackRest status,
backup count, and the newest backup's label + timestamp (or that it has none).

**R5 — Skeleton emission (opt-in).** With `-o <dir>`, write `<stanza>.yaml` per
stanza: the base source with the stanza swapped in (repo location + credentials
carry over), the base restore image, and only the **required structural** checks
(`server_reachable`, `has_user_database`, `schema_present`). A header directs the
user to verify `restore.database` and run `salvage scaffold` against the file to
add data checks. The skeleton MUST re-parse via `config.Load` and be directly
usable as a `scaffold`/`run` input.

**R6 — Restore is image-generic.** Restoring a given stanza MUST NOT require the
restore image's `pgbackrest.conf` to carry a `pg1-path` for that stanza: Salvage
always restores into its own ephemeral PGDATA and pins `--pg1-path` explicitly, so
one generic image ( `[global]` repo config only ) can restore every stanza in the
fleet.

## Design

```
salvage fleet -config repo.yaml [-o out/]
  → StartRestoreEnv → `pgbackrest info --output=json`   (no --stanza)
  → pgbrinfo.Stanzas(json) → []Stanza{name,status,backups(newest first)}
  → for s in stanzas:
       summarize (status, count, newest)
       if -o: write out/<s>.yaml  (source.stanza=s, structural checks, skeleton header)
  → render fleet report (table / JSON)
```

`internal/pgbrinfo` gains, alongside `Parse`:

```go
type Stanza struct {
    Name          string
    StatusCode    int
    StatusMessage string
    Backups       []Backup // newest first
}
func (s Stanza) Newest() (Backup, bool)
// Stanzas parses `pgbackrest info --output=json` (all stanzas) into a
// per-stanza summary ordered by name.
func Stanzas(infoJSON []byte) ([]Stanza, error)
```

Integration: `PgBackRest.Info(stanza)` accepts an empty stanza (omit `--stanza`),
the pgBackRest restore pins `--pg1-path`, `engine.Fleet`, a `report.Fleet` type,
`scaffold.RenderSkeleton`, and the CLI command.

## Open questions

- Should fleet optionally chase each stanza with a bounded `last-good` (a
  "freshest restorable point per stanza" fleet health report)? Powerful, but that
  is N restores — belongs behind an explicit flag and probably the scheduler.
- Enumerating the databases inside each stanza would need one restore per stanza;
  deferred with the "which database" question.
- S3/R2 repos with many stanzas: `info` is still one call, but credentials scope
  to the repo, not per-stanza — fine for v1.

## Acceptance criteria

1. Against a repo with two stanzas (`alpha`, `beta`), `salvage fleet` lists both
   with correct backup counts and newest-backup labels, performing no restore.
2. `salvage fleet -o <dir>` writes `alpha.yaml` and `beta.yaml` that `config.Load`
   accepts; running `salvage scaffold` against one, then `salvage run`, passes.
3. A stanza absent from the restore image's `pgbackrest.conf` still restores
   (R6 — `--pg1-path` pinned by Salvage).
