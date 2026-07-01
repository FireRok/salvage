// Package engine orchestrates a restore-test: stand up a throwaway database,
// restore the backup into it, and assert it actually works.
//
// The orchestration is engine-agnostic. It resolves the engine for the config's
// target.type from the SPI registry (spec 0016) and drives it through the
// spi.Engine interface; the Postgres-specific mechanics live behind that seam in
// internal/engine/postgres. Adding a new database/backup type is implementing
// spi.Engine + registering it, with no change here.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/discover"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/report"
	"salvage.sh/internal/scaffold"
	"salvage.sh/internal/version"

	// Register the built-in engines. Blank import for their init() side effect;
	// this is the single place engines are wired into the CLI.
	_ "salvage.sh/internal/engine/postgres"
)

// Run executes the full restore-test for cfg.
//
// The returned error is non-nil only for *operational* failures (e.g. Docker is
// unavailable, a required secret env var is missing) — those exit 2. A backup
// that simply fails to restore, or a check that fails, is a normal result: the
// report's verdict is "fail" and err is nil.
func Run(ctx context.Context, cfg *config.Config) (*report.Report, error) {
	rep := report.New(cfg.Target.Name, version.Version)
	rep.Restore.Image = cfg.Target.Restore.Image
	rep.Restore.Database = cfg.Target.Restore.Database

	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		rep.Restore.OK = false
		rep.Restore.Error = err.Error()
		rep.Finalize()
		return rep, err // operational: unknown target type
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	start := time.Now()
	rt, warn, rerr := eng.Restore(ctx, cfg)
	if rerr != nil {
		rep.Restore.OK = false
		rep.Restore.Error = rerr.Error()
		if isFault(rerr) {
			// Operational failure (Docker down, missing secret, could not create
			// the environment): no verdict, exit non-zero.
			rep.Finalize()
			return rep, rerr
		}
		// The backup did not restore/recover: a normal "fail" verdict.
		rep.Restore.DurationMS = time.Since(start).Milliseconds()
		rep.Finalize()
		return rep, nil
	}
	defer rt.Stop()

	rep.Restore.OK = true
	rep.Restore.Warnings = warn
	rep.Restore.DurationMS = time.Since(start).Milliseconds()
	rep.Checks = checks.Run(ctx, rt, cfg.Target.Checks)
	rep.Finalize()
	return rep, nil
}

// isFault reports whether err is an operational fault (as opposed to a
// restore-verdict failure). Uses errors.As so wrapped faults still match.
func isFault(err error) bool {
	var f *spi.Fault
	return errors.As(err, &f)
}

// Scaffold restores the backup, introspects the restored cluster, generates a
// deterministic set of checks (each verified against the snapshot), and returns a
// rendered YAML target config. See spec 0009. It is an operational helper, not a
// verdict: any failure returns an error.
func Scaffold(ctx context.Context, cfg *config.Config) ([]byte, error) {
	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	rt, _, rerr := eng.Restore(ctx, cfg)
	if rerr != nil {
		return nil, fmt.Errorf("restore: %w", rerr)
	}
	defer rt.Stop()

	disc, err := discover.Introspect(ctx, rt, cfg.Target.Restore.Database)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	generated := verifyChecks(ctx, rt, discover.GenerateChecks(disc))

	name := cfg.Target.Name
	if name == "" {
		name = "scaffolded-target"
	}
	out := cfg.Report.Out
	if out == "" {
		out = "./salvage-report.json"
	}
	return scaffold.Render(scaffold.Build(name, cfg.Target.Source, cfg.Target.Restore, generated, out))
}

// verifyChecks runs each check against the live cluster and keeps only those that
// execute and pass on the known-good snapshot (spec 0009 R5).
func verifyChecks(ctx context.Context, q checks.Queryer, in []config.Check) []config.Check {
	out := make([]config.Check, 0, len(in))
	for _, c := range in {
		res := checks.Run(ctx, q, []config.Check{c})
		if len(res) == 1 && res[0].OK {
			out = append(out, c)
		}
	}
	return out
}

// LastGood walks the backup chain newest->oldest, restore-testing each backup
// with the configured checks, and returns the first that passes as the recovery
// point (spec 0010). maxTry caps how many to try (0 = until the first pass). It
// finds the freshest *restorable* backup; it does not repair or extract.
//
// It requires an engine that implements spi.ChainTester (pgBackRest today); for
// any other engine/source it returns a clear "not supported" error.
func LastGood(ctx context.Context, cfg *config.Config, maxTry int) (*report.LastGood, error) {
	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		return nil, err
	}
	ct, ok := eng.(spi.ChainTester)
	if !ok {
		return nil, fmt.Errorf("last-good is not supported for target.type %q", cfg.Target.Type)
	}
	// last-good is pgBackRest-specific today; the ChainTester capability is only
	// meaningful for a chain-backed source.
	if cfg.Target.Source.Kind != "pgbackrest" {
		return nil, fmt.Errorf("last-good supports pgbackrest sources only (got %q)", cfg.Target.Source.Kind)
	}

	stanza := cfg.Target.Source.Stanza
	lg := &report.LastGood{Tool: "salvage", Version: version.Version, Stanza: stanza}

	backups, err := ct.Chain(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for i, b := range backups {
		if maxTry > 0 && i >= maxTry {
			break
		}
		v := report.BackupVerdict{Label: b.Label, Type: b.Type, Timestamp: b.Timestamp}
		reason := ct.TestBackup(ctx, cfg, b.Label)
		if reason == "" {
			v.Verdict = "pass"
			lg.Tested = append(lg.Tested, v)
			lg.RecoveryPoint = &lg.Tested[len(lg.Tested)-1]
			return lg, nil
		}
		v.Verdict = "fail"
		v.Reason = reason
		lg.Tested = append(lg.Tested, v)
	}
	return lg, nil
}

// Fleet enumerates every unit (pgBackRest stanza) in a repo (a cheap,
// metadata-only survey — no restore). When outDir is non-empty it also writes a
// per-unit skeleton config there (structural checks only; the user runs
// `salvage scaffold` against each to add data checks). See spec 0011.
//
// It requires an engine that implements spi.FleetSurveyor (pgBackRest today);
// for any other engine/source it returns a clear "not supported" error.
func Fleet(ctx context.Context, cfg *config.Config, outDir string) (*report.Fleet, error) {
	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		return nil, err
	}
	fs, ok := eng.(spi.FleetSurveyor)
	if !ok {
		return nil, fmt.Errorf("fleet is not supported for target.type %q", cfg.Target.Type)
	}
	if cfg.Target.Source.Kind != "pgbackrest" {
		return nil, fmt.Errorf("fleet supports pgbackrest sources only (got %q)", cfg.Target.Source.Kind)
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	units, err := fs.Survey(ctx, cfg)
	if err != nil {
		return nil, err
	}

	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, fmt.Errorf("create output dir: %w", err)
		}
	}

	fl := &report.Fleet{Tool: "salvage", Version: version.Version}
	for _, u := range units {
		sum := report.StanzaSummary{
			Name:         u.Name,
			Status:       u.Status,
			BackupCount:  u.BackupCount,
			NewestLabel:  u.NewestLabel,
			NewestBackup: u.NewestBackup,
		}
		if outDir != "" {
			path, werr := writeSkeleton(cfg, fs, u.Name, outDir)
			if werr != nil {
				return nil, werr
			}
			sum.ConfigWritten = path
		}
		fl.Stanzas = append(fl.Stanzas, sum)
	}
	return fl, nil
}

// writeSkeleton emits a per-unit skeleton config (structural checks only) into
// outDir, named <unit>.yaml. The source is obtained from the engine's
// SkeletonSource so repo location + credentials carry over with the unit swapped in.
func writeSkeleton(cfg *config.Config, fs spi.FleetSurveyor, unit, outDir string) (string, error) {
	src := fs.SkeletonSource(cfg, unit)
	structural := discover.GenerateChecks(&discover.Discovery{}) // the 3 required structural checks
	skel := scaffold.Build(unit, src, cfg.Target.Restore, structural, "./salvage-report-"+unit+".json")
	rendered, err := scaffold.RenderSkeleton(skel)
	if err != nil {
		return "", fmt.Errorf("render skeleton for %s: %w", unit, err)
	}
	path := filepath.Join(outDir, unit+".yaml")
	if err := os.WriteFile(path, rendered, 0o644); err != nil {
		return "", fmt.Errorf("write skeleton for %s: %w", unit, err)
	}
	return path, nil
}
