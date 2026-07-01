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

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/discover"
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

// RestoredTarget is a live restored database the orchestrator drives. It answers
// scalar queries (for checks) and row queries (for scaffold introspection), and
// can be torn down. Stop must be safe to call more than once.
type RestoredTarget interface {
	checks.Queryer
	discover.RowQueryer
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

// ChainTester is the optional capability behind `salvage last-good`: enumerate a
// backup chain newest→oldest and restore-test a specific backup by label. Engines
// whose backups form a testable chain (pgBackRest today) implement it; others do
// not, and last-good returns a clear "not supported for target type X" error.
type ChainTester interface {
	// Chain returns the backups in the source's chain, newest first.
	Chain(ctx context.Context, cfg *config.Config) ([]Backup, error)
	// TestBackup restore-tests the backup pinned by label against cfg's checks.
	// It returns "" on success, or a short failure reason (a restore error, or
	// the first failing required check) — never an operational abort mid-search.
	TestBackup(ctx context.Context, cfg *config.Config, label string) string
}

// FleetSurveyor is the optional capability behind `salvage fleet`: a cheap,
// metadata-only enumeration of every logical unit (pgBackRest stanza) in a repo,
// with no restore. Engines that group many backups under one repo implement it.
type FleetSurveyor interface {
	// Survey returns one entry per unit discovered in the repo.
	Survey(ctx context.Context, cfg *config.Config) ([]FleetUnit, error)
	// SkeletonSource returns the Source to embed in a per-unit skeleton config,
	// inheriting repo location + credentials from cfg with the unit swapped in.
	SkeletonSource(cfg *config.Config, unit string) config.Source
}

// Backup is one restorable point in a chain (engine-agnostic view of a
// pgBackRest backup). Timestamp is the backup-stop time.
type Backup struct {
	Label     string
	Type      string
	Timestamp time.Time
}

// FleetUnit is one logical unit found by a fleet survey (a pgBackRest stanza).
type FleetUnit struct {
	Name        string
	Status      string
	BackupCount int
	// NewestLabel/NewestBackup describe the freshest backup, if any.
	NewestLabel  string
	NewestBackup *time.Time
}
