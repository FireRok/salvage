# Salvage backlog — stories & deferred items

Smaller, testable stories that don't (yet) warrant a full numbered spec, plus
pointers to larger items already tracked as **Open questions** inside shipped
specs. Numbered specs remain the source of truth for *what* Salvage should do;
this file is the holding pen for gaps found during the 2026-07-01 product audit
so nothing is lost. Promote an item to a numbered spec when it grows a real
design surface.

Each story is written so an implementer (human or agent) can build it and a test
can verify it — the same bar as a spec requirement.

## A. Small stories (fix-shaped, no spec needed)

**S1 — Strict config parsing (reject unknown keys).** *(Done 2026-07-01.)*
`config.Load` decodes with a plain `yaml.Unmarshal` (`internal/config/config.go:253`),
so a misspelled key (`expct_min`, `snapshto`, `pass_evn`) is silently dropped —
it can disable a check's expectation or a source field with **no error at load**.
Switch to a strict decoder (`yaml.NewDecoder(r).KnownFields(true)` or equivalent)
so unknown/misspelled keys fail `salvage check`.
*Acceptance:* a config with one misspelled key exits `2` from `salvage check` with
a message naming the key; all example configs still validate.

**S2 — `fleet` non-zero exit on degraded/empty repo.** *(Done 2026-07-01, with spec 0026.)*
`cmdFleet` (`cmd/salvage/main.go:165-188`) always exits `0`, even when a stanza is
degraded or has zero backups; only config/operational errors exit `2`. This gives
cron/CI no failure signal, unlike `cmdLastGood` (`:140-142`, exits `1`).
*Absorbed by* [[spec 0029]] **R5** — listed here as the discrete fix in case it
lands ahead of the broader cross-engine work.
*Acceptance:* `salvage fleet` exits non-zero when any surveyed stanza is degraded
or empty; exit `0` only when all are healthy and non-empty.

**S3 — Freshness (`max_age`) beyond the `sql` kind.** *(Done for `doc_query` 2026-07-01; `command`/`file_*` freshness still open in `internal/probe`.)*
`max_age` is only honored under the `sql` kind (`config.go:533`); MongoDB's
`doc_query` supports only `equals`/`expect_min`/`expect_max`
(`internal/engine/mongodb/mongodb.go:178-204`) and the file/command probe kinds
have no freshness at all. "Is the data recent?" is a headline value prop but is
inexpressible outside Postgres/MySQL. Add a shared freshness expectation usable by
`doc_query` (and, where meaningful, `command`/`file_*`). Relates to [[spec 0017]].
*Acceptance:* a `doc_query` check can assert a timestamp scalar is no older than a
configured window; a mismatch fails the verdict.

**S4 — `http` check kind available beyond the exec engine.** *(Done 2026-07-01 — spi.CapabilityDeclarer.)*
`kind: http` is rejected for any `target.type` but `exec`
(`config.go:565-568`); yet a service restored under restic/borg (reachable via a
`docker exec` prober) could be HTTP-probed too. Generalize `http` to any target
that exposes an HTTP-capable prober, not just `exec`. Relates to [[spec 0017]] /
[[spec 0020]].
*Acceptance:* an `http` check validates and runs against a non-exec target that
provides an HTTP prober; still rejected where no prober exists.

**S5 — Per-engine config validation.** *(Done 2026-07-01.)*
Per [[spec 0016]] Open questions, `config.Validate` still centrally allow-lists
`target.type` and knows each engine's source kinds, rather than each engine
contributing its own `Validate(cfg)`. This is the one core touch-point that
partially breaks 0016 R6's additive-extension promise. Move validation behind an
optional engine capability so a new engine adds a sibling file only.
*Acceptance:* adding a hypothetical engine requires no edit to `config.Validate`'s
core switch; existing per-engine validation messages are unchanged.

**S6 — Structured, leveled logging + verbosity flags.** *(Done 2026-07-01.)*
There is no `log`/`slog` anywhere in `internal/` or `cmd/`; all diagnostics are
`fmt.Fprintln(os.Stderr, …)`, with no `--verbose`/`--quiet`/log-level control —
awkward for unattended `schedule`+`attest` runs and fleet automation. Add leveled
logging and `--verbose`/`--quiet`. The `--verbose` raw-output mode must align with
[[spec 0027]] (raw command output goes to stderr, never into report JSON).
*Acceptance:* `--quiet` suppresses non-error stderr; `--verbose` adds detail;
neither changes report JSON or exit codes.

**S7 — Share the numeric-scalar evaluator across kinds.** *(Done 2026-07-01 — `checks.EvaluateScalar`; probe kinds still private, see S3 note.)*
MongoDB's `doc_query` re-implements `equals`/`expect_min`/`expect_max` by hand
(`mongodb.go:178-204`) rather than reusing the `sql` kind's numeric-scalar
expectation logic. Factor a shared scalar-expectation evaluator so all
single-scalar kinds share one code path. Robustness; relates to [[spec 0017]].
*Acceptance:* `doc_query` and `sql` expectations are evaluated by shared code;
behavior unchanged; a single test suite covers both.

**S8 — Stale engine coverage in the guide and README.** *(Done 2026-07-01 via spec 0037 R2 remediation.)*
`docs/guide/03-engines.md:21` (and `:137`) still says "Three engines ship
today: postgres, restic, exec" while borg/MySQL/MongoDB ([[spec 0022]]/
[[spec 0024]]/[[spec 0025]]) are Implemented with repo-root example configs
nothing links to; `docs/guide/02-configuration.md` lacks the
`collection_count`/`doc_query` kinds and the README Sources table
(`README.md:78-83`) lists only postgres+restic rows.
*Absorbed by* [[spec 0037]] **R2** — listed here as the discrete, urgent fix in
case it lands ahead of the full docs-parity work.
*Acceptance:* the guide/README claims match the shipped engine set; both new
check kinds appear in the configuration reference; each example config is
linked from the docs.

**S9 — Release notes derive from a curated changelog, not the mirrored commit log.** *(Done 2026-07-01 — CHANGELOG.md seeded; release workflow extracts the tag section fail-closed.)*
`.goreleaser.yaml` sets `changelog: use: github`, so GitHub Release notes are
generated from the public mirror's commit history — a curated snapshot, not a
user-facing narrative. Point release notes at a maintained `CHANGELOG.md`
section instead. *Absorbed by* [[spec 0034]] **R3/R4** — the discrete config
fix can land with the first `CHANGELOG.md`.
*Acceptance:* the next release's GitHub Release body matches its
`CHANGELOG.md` section; `use: github` is gone from `.goreleaser.yaml`.

**S10 — Pin the floating default restore images.** *(Done 2026-07-01 — pinned to the live-verified versions.)*
Two engine defaults float: `restic/restic:latest`
(`internal/config/config.go:280`) and `ghcr.io/borgbackup/borg:stable`
(`config.go:296`) — so the default behavior of a pinned Salvage release
changes whenever upstream retags, and no supported-versions claim can be made
about it. Pin both to specific version tags (users can still override
`restore.image`). *Absorbed by* [[spec 0036]] **R4** — safe to land
immediately.
*Acceptance:* no default `restore.image` assignment in
`internal/config/config.go` uses `latest`/`stable`; existing example configs
and tests still pass.

## B. Larger deferred items (already tracked as Open questions — candidate future specs)

These are real but heavier; each is named in a shipped spec's Non-goals/Open
questions. Promote to a numbered spec when there's appetite or a customer need.

- **MySQL physical/binlog restore (xtrabackup) + PITR** — [[spec 0024]] Open
  questions. Would warrant its own `source.kind` and is the natural home for a
  MySQL `ChainTester`/`FleetSurveyor` (see [[spec 0029]]).
- **MongoDB physical / filesystem-snapshot restore + oplog PITR** — [[spec 0025]]
  Non-goals; hardened topologies (replica sets, sharded, auth/TLS) and
  cross-minor-version archive restores are unverified.
- **Independent (hosted) execution tier** — the recurring big one: Firerok
  *supplies and runs* the restore environment and signs that, versus today's
  notary that counter-signs a restore the customer ran. Schema slot already
  reserved (`method: notary|hosted-exec`) in [[spec 0012]]; named in
  [[spec 0002]], [[spec 0020]], and the README roadmap. Needs credential custody +
  per-test compute, which breaks the standing no-custody / low-cost constraint.
- **Public transparency log** (cross-tenant Merkle root, periodically published)
  so even Firerok cannot rewrite history — [[spec 0012]] Open questions.
- **Standard attestation envelope** (in-toto / DSSE / SLSA provenance) instead of
  the bespoke format; plus attestation expiry/revocation semantics —
  [[spec 0002]] Open questions.
- **Evidence-pack rendered PDF** (a real PDF library render vs today's
  print-to-PDF) — [[spec 0019]].
- **LLM-assisted `--smart` check generation** (reads schema + aggregate stats) —
  [[spec 0009]] Non-goal; distinct from the deterministic cross-engine scaffold in
  [[spec 0028]].

## C. Verification debt (QA, not features)

- **End-to-end engine runs against live Docker.** *(Closed 2026-07-01 — see `dev/<engine>/VERIFIED.md`.)* restic/borg/MySQL engine specs
  ([[spec 0018]], [[spec 0022]], [[spec 0024]]) were written without a live Docker
  daemon and explicitly deferred their end-to-end verification; MongoDB
  ([[spec 0025]]) was confirmed live. Close the loop with a real-Docker run of each
  non-Postgres engine and record it (a `dev/<engine>/make-backup.sh` + a PASS run,
  as MySQL/MongoDB already have).
