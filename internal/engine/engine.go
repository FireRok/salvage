// Package engine orchestrates a restore-test: stand up a throwaway database,
// restore the backup into it, and assert it actually works.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/discover"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/pgbrinfo"
	"salvage.sh/internal/report"
	"salvage.sh/internal/scaffold"
	"salvage.sh/internal/version"
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

	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	start := time.Now()
	var q checks.Queryer

	switch cfg.Target.Source.Kind {
	case "pgbackrest":
		for _, name := range cfg.Target.Source.PassEnv {
			if os.Getenv(name) == "" {
				err := fmt.Errorf("required env %s is not set (export it before running salvage)", name)
				rep.Restore.OK = false
				rep.Restore.Error = err.Error()
				rep.Finalize()
				return rep, err // operational
			}
		}
		env, err := ephemeral.StartRestoreEnv(ctx, cfg.Target.Restore.Image,
			cfg.Target.Source.RepoPath, cfg.Target.Source.RepoVolume,
			cfg.Target.Restore.Database, cfg.Target.Restore.User,
			cfg.Target.Source.PassEnv, cfg.Target.Restore.PreloadLibraries)
		if err != nil {
			rep.Restore.OK = false
			rep.Restore.Error = err.Error()
			rep.Finalize()
			return rep, err // operational: couldn't create the environment
		}
		defer env.Stop()
		if err := env.Restore(ctx, cfg.Target.Source.Stanza, ""); err != nil {
			rep.Restore.OK = false
			rep.Restore.Error = err.Error()
			rep.Restore.DurationMS = time.Since(start).Milliseconds()
			rep.Finalize()
			return rep, nil // verdict fail: the backup did not restore/recover
		}
		q = env

	default: // pg_dump, sql
		pg, err := ephemeral.StartPostgres(ctx, cfg.Target.Restore.Image, cfg.Target.Restore.Database)
		if err != nil {
			rep.Restore.OK = false
			rep.Restore.Error = err.Error()
			rep.Finalize()
			return rep, err // operational
		}
		defer pg.Stop()
		warn, rerr := pg.Restore(ctx, cfg.Target.Source.Kind, cfg.Target.Source.Path)
		if rerr != nil {
			rep.Restore.OK = false
			rep.Restore.Error = rerr.Error()
			rep.Restore.DurationMS = time.Since(start).Milliseconds()
			rep.Finalize()
			return rep, nil // verdict fail
		}
		rep.Restore.Warnings = warn
		q = pg
	}

	rep.Restore.OK = true
	rep.Restore.DurationMS = time.Since(start).Milliseconds()
	rep.Checks = checks.Run(ctx, q, cfg.Target.Checks)
	rep.Finalize()
	return rep, nil
}

// queryEnv is a live restored cluster that answers both scalar and row queries
// and can be torn down. Both ephemeral restore types satisfy it.
type queryEnv interface {
	checks.Queryer
	discover.RowQueryer
	Stop() error
}

// Scaffold restores the backup, introspects the restored cluster, generates a
// deterministic set of checks (each verified against the snapshot), and returns a
// rendered YAML target config. See spec 0009. It is an operational helper, not a
// verdict: any failure returns an error.
func Scaffold(ctx context.Context, cfg *config.Config) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	var env queryEnv
	switch cfg.Target.Source.Kind {
	case "pgbackrest":
		for _, name := range cfg.Target.Source.PassEnv {
			if os.Getenv(name) == "" {
				return nil, fmt.Errorf("required env %s is not set", name)
			}
		}
		pb, err := ephemeral.StartRestoreEnv(ctx, cfg.Target.Restore.Image,
			cfg.Target.Source.RepoPath, cfg.Target.Source.RepoVolume,
			cfg.Target.Restore.Database, cfg.Target.Restore.User,
			cfg.Target.Source.PassEnv, cfg.Target.Restore.PreloadLibraries)
		if err != nil {
			return nil, err
		}
		env = pb
		defer env.Stop()
		if err := pb.Restore(ctx, cfg.Target.Source.Stanza, ""); err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
	default: // pg_dump, sql
		pg, err := ephemeral.StartPostgres(ctx, cfg.Target.Restore.Image, cfg.Target.Restore.Database)
		if err != nil {
			return nil, err
		}
		env = pg
		defer env.Stop()
		if _, err := pg.Restore(ctx, cfg.Target.Source.Kind, cfg.Target.Source.Path); err != nil {
			return nil, fmt.Errorf("restore: %w", err)
		}
	}

	disc, err := discover.Introspect(ctx, env, cfg.Target.Restore.Database)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	generated := verifyChecks(ctx, env, discover.GenerateChecks(disc))

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

// LastGood walks the pgBackRest backup chain newest->oldest, restore-testing each
// backup with the configured checks, and returns the first that passes as the
// recovery point (spec 0010). maxTry caps how many to try (0 = until the first
// pass). It finds the freshest *restorable* backup; it does not repair or extract.
func LastGood(ctx context.Context, cfg *config.Config, maxTry int) (*report.LastGood, error) {
	if cfg.Target.Source.Kind != "pgbackrest" {
		return nil, fmt.Errorf("last-good supports pgbackrest sources only (got %q)", cfg.Target.Source.Kind)
	}
	for _, name := range cfg.Target.Source.PassEnv {
		if os.Getenv(name) == "" {
			return nil, fmt.Errorf("required env %s is not set", name)
		}
	}
	stanza := cfg.Target.Source.Stanza
	lg := &report.LastGood{Tool: "salvage", Version: version.Version, Stanza: stanza}

	// One short-lived env just to read the backup chain.
	infoEnv, err := startPgBackRest(ctx, cfg)
	if err != nil {
		return nil, err
	}
	raw, ierr := infoEnv.Info(ctx, stanza)
	infoEnv.Stop()
	if ierr != nil {
		return nil, ierr
	}
	backups, err := pgbrinfo.Parse(raw, stanza)
	if err != nil {
		return nil, err
	}

	for i, b := range backups {
		if maxTry > 0 && i >= maxTry {
			break
		}
		v := report.BackupVerdict{Label: b.Label, Type: b.Type, Timestamp: b.Timestamp}
		reason := testBackup(ctx, cfg, b.Label)
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

// Fleet enumerates every stanza in a pgBackRest repo (a cheap, metadata-only
// survey via `pgbackrest info` — no restore). When outDir is non-empty it also
// writes a per-stanza skeleton config there (structural checks only; the user
// runs `salvage scaffold` against each to add data checks). See spec 0011.
func Fleet(ctx context.Context, cfg *config.Config, outDir string) (*report.Fleet, error) {
	if cfg.Target.Source.Kind != "pgbackrest" {
		return nil, fmt.Errorf("fleet supports pgbackrest sources only (got %q)", cfg.Target.Source.Kind)
	}
	for _, name := range cfg.Target.Source.PassEnv {
		if os.Getenv(name) == "" {
			return nil, fmt.Errorf("required env %s is not set", name)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()

	env, err := startPgBackRest(ctx, cfg)
	if err != nil {
		return nil, err
	}
	raw, ierr := env.Info(ctx, "") // empty stanza → every stanza in the repo
	env.Stop()
	if ierr != nil {
		return nil, ierr
	}
	stanzas, err := pgbrinfo.Stanzas(raw)
	if err != nil {
		return nil, err
	}

	if outDir != "" {
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return nil, fmt.Errorf("create output dir: %w", err)
		}
	}

	fl := &report.Fleet{Tool: "salvage", Version: version.Version}
	for _, s := range stanzas {
		sum := report.StanzaSummary{
			Name:        s.Name,
			Status:      statusText(s),
			BackupCount: len(s.Backups),
		}
		if nb, ok := s.Newest(); ok {
			sum.NewestLabel = nb.Label
			ts := nb.Timestamp
			sum.NewestBackup = &ts
		}
		if outDir != "" {
			path, werr := writeSkeleton(cfg, s.Name, outDir)
			if werr != nil {
				return nil, werr
			}
			sum.ConfigWritten = path
		}
		fl.Stanzas = append(fl.Stanzas, sum)
	}
	return fl, nil
}

// statusText renders a stanza's pgBackRest status, defaulting to "ok".
func statusText(s pgbrinfo.Stanza) string {
	if s.StatusMessage != "" {
		return s.StatusMessage
	}
	if s.StatusCode == 0 {
		return "ok"
	}
	return fmt.Sprintf("status %d", s.StatusCode)
}

// writeSkeleton emits a per-stanza skeleton config (structural checks only) into
// outDir, named <stanza>.yaml. The source is inherited from cfg with the stanza
// swapped in, so repo location + credentials carry over.
func writeSkeleton(cfg *config.Config, stanza, outDir string) (string, error) {
	src := cfg.Target.Source
	src.Stanza = stanza
	structural := discover.GenerateChecks(&discover.Discovery{}) // the 3 required structural checks
	skel := scaffold.Build(stanza, src, cfg.Target.Restore, structural, "./salvage-report-"+stanza+".json")
	rendered, err := scaffold.RenderSkeleton(skel)
	if err != nil {
		return "", fmt.Errorf("render skeleton for %s: %w", stanza, err)
	}
	path := filepath.Join(outDir, stanza+".yaml")
	if err := os.WriteFile(path, rendered, 0o644); err != nil {
		return "", fmt.Errorf("write skeleton for %s: %w", stanza, err)
	}
	return path, nil
}

func startPgBackRest(ctx context.Context, cfg *config.Config) (*ephemeral.PgBackRest, error) {
	return ephemeral.StartRestoreEnv(ctx, cfg.Target.Restore.Image,
		cfg.Target.Source.RepoPath, cfg.Target.Source.RepoVolume,
		cfg.Target.Restore.Database, cfg.Target.Restore.User,
		cfg.Target.Source.PassEnv, cfg.Target.Restore.PreloadLibraries)
}

// testBackup restore-tests one backup (pinned by label) with the configured
// checks. Returns "" on success or a short failure reason.
func testBackup(ctx context.Context, cfg *config.Config, label string) string {
	tctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()
	env, err := startPgBackRest(tctx, cfg)
	if err != nil {
		return "could not start restore env: " + err.Error()
	}
	defer env.Stop()
	if err := env.Restore(tctx, cfg.Target.Source.Stanza, label); err != nil {
		return firstLine(err.Error())
	}
	for _, res := range checks.Run(tctx, env, cfg.Target.Checks) {
		if !res.OK && res.Severity != "advisory" {
			if res.Error != "" {
				return res.Name + ": " + res.Error
			}
			return res.Name + ": " + res.Detail
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
