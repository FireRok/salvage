// Package spi defines the engine service-provider interface: the seam the
// orchestrator drives instead of hardcoding Postgres. An engine handles one
// target.type (e.g. "postgres"); adding a new backup/database type is a matter
// of implementing Engine, registering it, and — for anything beyond the common
// restore+check lifecycle — implementing an optional capability interface.
//
// The core (internal/engine) depends only on this package plus checks/discover/
// report/config; it never imports a concrete engine. Concrete engines live in
// their own packages (e.g. internal/engine/postgres) and Register themselves in
// an init(), so the CLI wires them in by blank-importing the engine packages.
package spi

import (
	"context"
	"time"

	"salvage.sh/internal/config"
)

// Engine restore-tests one target type. Type() reports the config target.type it
// handles; Restore stands up a throwaway environment, restores the backup into
// it, and returns a live RestoredTarget the orchestrator can query and tear down.
//
// Restore's error contract mirrors the caller's verdict/operational split:
//   - A nil error with a live RestoredTarget means the restore succeeded.
//   - A *Fault* (see below) means an operational failure (Docker down, a missing
//     secret) — the caller exits non-zero without a verdict.
//   - Any other non-nil error means the backup did not restore — a normal "fail"
//     verdict, not an operational error.
//
// warnings is a human-readable note when the restore succeeded but was noisy
// (e.g. pg_restore ignored benign "already exists" errors); it is surfaced in the
// report, never treated as failure. It is "" on the physical/pgBackRest path.
type Engine interface {
	Type() string
	Restore(ctx context.Context, cfg *config.Config) (rt RestoredTarget, warnings string, err error)
}

// RestoredTarget is a live restored backup the orchestrator drives. It is
// deliberately minimal — only teardown is universal across backup types (spec
// 0017 R3). The orchestrator passes it opaquely to checks.Run, where each check
// evaluator type-asserts it to the capability that kind needs:
//   - the "sql" evaluator asserts checks.Queryer (Query), which the Postgres
//     target implements;
//   - an engine's own Scaffolder.Discover asserts whatever introspection handle
//     its target exposes (a row-queryer, a file prober) — the orchestrator only
//     sees the Scaffolder capability (spec 0028 R1).
//
// A non-SQL engine's target implements whatever its own kinds assert instead of
// Query/QueryRows, without changing this interface. Stop must be safe to call
// more than once.
type RestoredTarget interface {
	Stop() error
}

// Fault marks an operational failure as distinct from a restore-verdict failure.
// The orchestrator returns Fault errors to the CLI as exit-2 operational errors;
// a plain error from Restore is a "fail" verdict with a nil operational error.
// An engine wraps environment/secret/Docker problems in a Fault; it returns a
// bare error when the *backup itself* failed to restore.
type Fault struct{ Err error }

func (f *Fault) Error() string { return f.Err.Error() }
func (f *Fault) Unwrap() error { return f.Err }

// Faultf wraps err as an operational Fault.
func Faultf(err error) *Fault { return &Fault{Err: err} }

// ConfigValidator is the optional engine capability behind per-engine config
// validation (spec 0016 R6): an engine that implements it owns the load-time
// validation of its target's source/restore shape — the rules that used to sit
// in config.Validate's central switch. Register discovers the capability and
// wires it into config's validator registry, so config.Validate dispatches to
// the engine and adding a new engine needs no core edit. An engine without it
// still gets its target.type registered as known, with no engine-specific
// validation. Engine-owned check kinds are validated separately, via
// config.RegisterCheckValidator.
type ConfigValidator interface {
	ValidateConfig(cfg *config.Config) error
}

// CapabilityDeclarer is the optional engine capability behind the shared probe
// check kinds' load-time gating (backlog S4): an engine that implements it
// declares which probe capabilities its RestoredTarget exposes (file probes,
// an HTTP prober), and Register wires the declaration into config's capability
// registry — so a `kind: http` or `kind: file_exists` check validates for any
// engine that declares the matching capability, with no core allow-list edit.
// The declaration is a static promise about the RestoredTarget; the evaluators
// still type-assert at run time and fail cleanly if it is broken.
type CapabilityDeclarer interface {
	TargetCapabilities() []config.TargetCapability
}

// ChainTester is the optional capability behind `salvage last-good`: enumerate a
// backup chain newest→oldest and restore-test a specific backup by label. Engines
// whose backups form a testable chain implement it — pgBackRest's backup chain,
// restic's snapshot history, borg's archive list (spec 0029). Others do not, and
// last-good returns a clear "not supported for target type X" error. The
// capability is the sole dispatch gate: no source.kind check sits behind it.
type ChainTester interface {
	// Chain returns the backups in the source's chain, newest first.
	Chain(ctx context.Context, cfg *config.Config) ([]Backup, error)
	// TestBackup restore-tests the backup pinned by label against cfg's checks.
	// It returns "" on success, or a short failure reason (a restore error, or
	// the first failing required check) — never an operational abort mid-search.
	TestBackup(ctx context.Context, cfg *config.Config, label string) string
}

// FleetSurveyor is the optional capability behind `salvage fleet`: a cheap,
// metadata-only enumeration of every logical unit in a repo, with no restore.
// Engines that group many backups under one repo implement it. A unit is a
// pgBackRest stanza; for a filesystem engine (restic/borg) the repository itself
// surveys as one unit — the honest analogue of a single stanza (spec 0029).
type FleetSurveyor interface {
	// Survey returns one entry per unit discovered in the repo.
	Survey(ctx context.Context, cfg *config.Config) ([]FleetUnit, error)
	// SkeletonSource returns the Source to embed in a per-unit skeleton config,
	// inheriting repo location + credentials from cfg with the unit swapped in.
	SkeletonSource(cfg *config.Config, unit string) config.Source
}

// SkeletonChecker optionally accompanies FleetSurveyor: it supplies the
// structural checks embedded in the per-unit skeleton configs `fleet -o` emits.
// Filesystem engines implement it to emit file checks instead of the default
// SQL structural checks (which only a SQL engine can run). An engine without it
// gets the default — the Postgres/pgBackRest skeletons are unchanged.
type SkeletonChecker interface {
	SkeletonChecks() []config.Check
}

// Scaffolder is the optional capability behind `salvage scaffold` (spec 0028):
// an engine that implements it opts into check discovery, exactly as ChainTester
// and FleetSurveyor opt an engine into last-good/fleet. An engine that omits it
// gates off with the existing "scaffold is not supported for target.type X"
// message — no core change.
//
// This is the single discovery seam (spec 0028 R7): catalog scaffolding
// (Postgres, MySQL), tree-walk scaffolding (restic/borg), and the exec engine's
// observe-and-recommend flow (spec 0021) are all implementations of this one
// interface. The orchestrator knows nothing about what an engine observes — a
// SQL catalog, a file tree, or caller hints.
type Scaffolder interface {
	// Discover observes the just-restored target and proposes candidate checks
	// that pin its current shape. It MUST NOT run them — verification against
	// the known-good snapshot is the orchestrator's shared job (spec 0028 R5).
	// Thresholds must be derived from observed state (floors, not pins) so every
	// candidate passes by construction on the snapshot it came from.
	Discover(ctx context.Context, rt RestoredTarget, cfg *config.Config) ([]ScaffoldCandidate, error)
}

// ScaffoldCandidate is one check proposed by an engine's Discover, carrying the
// metadata the shared emission layer needs to apply the deterministic
// top-N-by-size cap (spec 0028 R6) without understanding the engine's domain.
type ScaffoldCandidate struct {
	Check config.Check
	// Group names the cappable family the candidate belongs to (e.g. "table",
	// "dir", "anchor"). Empty means the candidate is never capped — structural/
	// presence checks are O(1) and always emitted.
	Group string
	// Subject identifies the entity within the group (a table, a directory):
	// the cap keeps the top-N subjects per group, and every candidate of a
	// surviving subject is kept, so a table's row-count floor and freshness
	// check live or die together.
	Subject string
	// Weight is the subject's observed size (rows for a table, files for a
	// directory). Larger weights are kept first; ties keep first-proposed order,
	// so re-running scaffold is reproducible.
	Weight int64
}

// Backup is one restorable point in a chain (engine-agnostic view of a
// pgBackRest backup, restic snapshot, or borg archive). Timestamp is the
// backup-stop/snapshot time.
type Backup struct {
	Label     string
	Type      string
	Timestamp time.Time
}

// FleetUnit is one logical unit found by a fleet survey (a pgBackRest stanza,
// or a restic/borg repository).
type FleetUnit struct {
	Name        string
	Status      string
	BackupCount int
	// NewestLabel/NewestBackup describe the freshest backup, if any.
	NewestLabel  string
	NewestBackup *time.Time
}
