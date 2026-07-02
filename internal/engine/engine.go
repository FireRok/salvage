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
	"sort"
	"strings"
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
	_ "salvage.sh/internal/engine/borg"
	_ "salvage.sh/internal/engine/exec"
	_ "salvage.sh/internal/engine/mongodb"
	_ "salvage.sh/internal/engine/mysql"
	_ "salvage.sh/internal/engine/postgres"
	_ "salvage.sh/internal/engine/restic"
)

// Run executes the full restore-test for cfg.
//
// The returned error is non-nil only for *operational* failures (e.g. Docker is
// unavailable, a required secret env var is missing) — those exit 2. A backup
// that simply fails to restore, or a check that fails, is a normal result: the
// report's verdict is "fail" and err is nil.
func Run(ctx context.Context, cfg *config.Config) (*report.Report, error) {
	rep := report.New(cfg.Target.Name, version.Version)
	// Spec 0027 R3: register the resolved values of every secret-bearing env
	// var from the config so serialization scrubs them wherever they appear.
	rep.SetKnownSecrets(report.KnownSecretsFromEnv(os.Getenv, cfg.SecretEnvNames()))
	// Method records which engine performed the restore (spec 0020 R7): for the
	// exec engine this marks the restore as operator-supplied so no downstream
	// artifact reads as "Salvage independently restored this."
	rep.Restore.Method = cfg.Target.Type
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

// DefaultScaffoldCap is the default top-N-by-size cap on per-subject generated
// checks (spec 0028 R6): at most N tables/directories per cappable family keep
// their heuristic checks, so a wide schema or deep tree scaffolds to a
// reviewable config, not noise. Structural checks are never capped.
const DefaultScaffoldCap = 50

// Scaffold restores the backup, asks the engine to discover candidate checks
// from the restored target, verifies each against the snapshot, and returns a
// rendered YAML target config. See specs 0009 and 0028. It is an operational
// helper, not a verdict: any failure returns an error.
func Scaffold(ctx context.Context, cfg *config.Config) ([]byte, error) {
	return ScaffoldWithCap(ctx, cfg, DefaultScaffoldCap)
}

// ScaffoldWithCap is Scaffold with an explicit per-family subject cap (0 or
// negative means DefaultScaffoldCap).
//
// Discovery is per-engine (spec 0017 R4, 0028 R1): the engine — not the
// restored target — is asserted to the optional spi.Scaffolder capability, and
// an engine that omits it gates off with a clear "not supported" error,
// mirroring how last-good/fleet are gated on their capabilities. Everything
// after Discover is horizontal: the deterministic cap, the verify-by-running
// safety net, and the YAML emission are shared by every engine.
func ScaffoldWithCap(ctx context.Context, cfg *config.Config, capN int) ([]byte, error) {
	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		return nil, err
	}
	sc, ok := eng.(spi.Scaffolder)
	if !ok {
		return nil, fmt.Errorf("scaffold is not supported for target.type %q", cfg.Target.Type)
	}
	if capN <= 0 {
		capN = DefaultScaffoldCap
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	rt, _, rerr := eng.Restore(ctx, cfg)
	if rerr != nil {
		return nil, fmt.Errorf("restore: %w", rerr)
	}
	defer rt.Stop()

	cands, err := sc.Discover(ctx, rt, cfg)
	if err != nil {
		return nil, fmt.Errorf("discover: %w", err)
	}
	capped, truncated := capCandidates(cands, capN)
	generated := verifyChecks(ctx, rt, capped)

	name := cfg.Target.Name
	if name == "" {
		name = "scaffolded-target"
	}
	out := cfg.Report.Out
	if out == "" {
		out = "./salvage-report.json"
	}
	var notes []string
	if truncated {
		// Honest output (spec 0028 R6): say the config was capped and how to widen.
		notes = append(notes,
			fmt.Sprintf("NOTE: generated checks were capped to the %d largest tables/directories", capN),
			"per family (top-N by observed size). Re-run with a higher -cap to widen.")
	}
	built := scaffold.Build(name, cfg.Target.Type, cfg.Target.Source, cfg.Target.Restore, generated, out)
	return scaffold.Render(built, notes...)
}

// capCandidates applies the shared, deterministic top-N-by-size cap (spec 0028
// R6) and returns the surviving checks in their original proposal order.
//
// The cap operates on *subjects* (a table, a directory), not individual checks:
// within each named group, the N subjects with the largest Weight survive (ties
// keep first-proposed order), and every candidate of a surviving subject is
// kept — so a table's row-count floor and freshness check live or die together.
// Ungrouped candidates (structural/presence checks) are never capped. truncated
// reports whether anything was dropped.
func capCandidates(cands []spi.ScaffoldCandidate, capN int) ([]config.Check, bool) {
	// Rank subjects per group by max observed weight, first appearance breaking ties.
	type subj struct {
		weight int64
		order  int // first-appearance index, the deterministic tie-break
	}
	groups := map[string]map[string]*subj{}
	for i, c := range cands {
		if c.Group == "" {
			continue
		}
		g := groups[c.Group]
		if g == nil {
			g = map[string]*subj{}
			groups[c.Group] = g
		}
		s := g[c.Subject]
		if s == nil {
			g[c.Subject] = &subj{weight: c.Weight, order: i}
		} else if c.Weight > s.weight {
			s.weight = c.Weight
		}
	}
	keep := map[string]map[string]bool{}
	truncated := false
	for name, g := range groups {
		subjects := make([]string, 0, len(g))
		for s := range g {
			subjects = append(subjects, s)
		}
		sort.Slice(subjects, func(a, b int) bool {
			sa, sb := g[subjects[a]], g[subjects[b]]
			if sa.weight != sb.weight {
				return sa.weight > sb.weight
			}
			return sa.order < sb.order
		})
		if len(subjects) > capN {
			subjects = subjects[:capN]
			truncated = true
		}
		kept := map[string]bool{}
		for _, s := range subjects {
			kept[s] = true
		}
		keep[name] = kept
	}
	out := make([]config.Check, 0, len(cands))
	for _, c := range cands {
		if c.Group != "" && !keep[c.Group][c.Subject] {
			continue
		}
		out = append(out, c.Check)
	}
	return out, truncated
}

// verifyChecks runs each check against the live restored target and keeps only
// those that execute and pass on the known-good snapshot (spec 0009 R5, 0028
// R5). target is passed opaquely to checks.Run, which dispatches by kind — so
// sql, file_*, http, and command candidates are all verified identically.
func verifyChecks(ctx context.Context, target checks.Target, in []config.Check) []config.Check {
	out := make([]config.Check, 0, len(in))
	for _, c := range in {
		res := checks.Run(ctx, target, []config.Check{c})
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
// It requires an engine that implements spi.ChainTester (pgBackRest, restic,
// and borg today); for any other engine it returns a clear "not supported"
// error. The capability assertion is the whole gate (spec 0029 R1) — an engine
// that implements ChainTester lights up last-good with no change here.
func LastGood(ctx context.Context, cfg *config.Config, maxTry int) (*report.LastGood, error) {
	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		return nil, err
	}
	ct, ok := eng.(spi.ChainTester)
	if !ok {
		return nil, fmt.Errorf("last-good is not supported for target.type %q", cfg.Target.Type)
	}

	// Stanza is a pgBackRest-only detail; for a filesystem engine it is simply
	// empty and the report renders a blank stanza line (spec 0029) — the
	// per-backup verdicts carry the engine's own unit identity (labels).
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

// Fleet enumerates every unit in a repo (a cheap, metadata-only survey — no
// restore): each pgBackRest stanza, or the repository itself for a filesystem
// engine. When outDir is non-empty it also writes a per-unit skeleton config
// there (structural checks only; the user runs `salvage scaffold` against each
// to add data checks). See specs 0011 and 0029.
//
// It requires an engine that implements spi.FleetSurveyor (pgBackRest, restic,
// and borg today); for any other engine it returns a clear "not supported"
// error. The capability assertion is the whole gate (spec 0029 R1). A degraded
// unit is a *finding* (reported, and the CLI exits 1 from it), not an error
// from here — err is reserved for "could not survey".
func Fleet(ctx context.Context, cfg *config.Config, outDir string) (*report.Fleet, error) {
	eng, err := spi.Lookup(cfg.Target.Type)
	if err != nil {
		return nil, err
	}
	fs, ok := eng.(spi.FleetSurveyor)
	if !ok {
		return nil, fmt.Errorf("fleet is not supported for target.type %q", cfg.Target.Type)
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
//
// The structural checks come from the engine when it implements
// spi.SkeletonChecker (filesystem engines emit file checks a run can actually
// evaluate); otherwise they are the default SQL structural checks — the
// pgBackRest skeletons are byte-identical to before (spec 0029 R7). The
// skeleton's target.type is the surveyed target's type, so a restic/borg
// skeleton re-parses under its own engine via config.Load (spec 0029 R4).
func writeSkeleton(cfg *config.Config, fs spi.FleetSurveyor, unit, outDir string) (string, error) {
	src := fs.SkeletonSource(cfg, unit)
	var structural []config.Check
	if sc, ok := fs.(spi.SkeletonChecker); ok {
		structural = sc.SkeletonChecks()
	} else {
		structural = discover.GenerateChecks(&discover.Discovery{}) // the 3 required structural checks
	}
	// A pgBackRest stanza name is already filename-safe; a filesystem unit is a
	// repository identity (a path or URL), so derive a safe file/report name.
	name := skeletonFileName(unit)
	skel := scaffold.Build(unit, cfg.Target.Type, src, cfg.Target.Restore, structural, "./salvage-report-"+name+".json")
	rendered, err := scaffold.RenderSkeleton(skel)
	if err != nil {
		return "", fmt.Errorf("render skeleton for %s: %w", unit, err)
	}
	path := filepath.Join(outDir, name+".yaml")
	if err := os.WriteFile(path, rendered, 0o644); err != nil {
		return "", fmt.Errorf("write skeleton for %s: %w", unit, err)
	}
	return path, nil
}

// skeletonFileName maps a unit name to a safe skeleton file stem: alphanumerics
// plus "._-" pass through (every ordinary stanza name is unchanged), anything
// else — path separators, URL punctuation in a repository identity — becomes
// "-". Leading/trailing separators are trimmed so "/srv/repo" → "srv-repo".
func skeletonFileName(unit string) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, unit)
	mapped = strings.Trim(mapped, "-.")
	if mapped == "" {
		return "repository"
	}
	return mapped
}
