// Package postgres implements the Postgres engine (spec 0016): the first — and,
// in v1, only — engine registered against target.type "postgres". It wraps the
// logic that used to live inline in internal/engine: the source.kind switch
// (pg_dump/sql/pgbackrest), the ephemeral restore environments, and the
// pgBackRest-specific last-good/fleet capabilities. Behaviour is unchanged; this
// package just moves it behind the SPI so non-Postgres engines can be added
// later without touching the orchestrator.
package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/discover"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/pgbrinfo"
)

func init() { spi.Register(Engine{}) }

// Engine is the Postgres engine. It carries no state; each Restore stands up its
// own ephemeral environment.
type Engine struct{}

// The engine contributes its own config validation (spec 0016 R6) and check
// discovery for `salvage scaffold` (spec 0028 R2).
var (
	_ spi.ConfigValidator = Engine{}
	_ spi.Scaffolder      = Engine{}
)

func (Engine) Type() string { return "postgres" }

// ValidateConfig applies the Postgres source allow-list: the pg_dump/sql/
// pgbackrest rules — and their messages — that used to live in
// config.Validate's central switch, now contributed by the engine itself via
// spi.ConfigValidator.
func (Engine) ValidateConfig(cfg *config.Config) error {
	t := cfg.Target
	switch t.Source.Kind {
	case "pg_dump", "sql":
		if t.Source.Path == "" {
			return fmt.Errorf("target.source.path is required for kind %q", t.Source.Kind)
		}
		if _, err := os.Stat(t.Source.Path); err != nil {
			return fmt.Errorf("target.source.path: %w", err)
		}
	case "pgbackrest":
		if t.Source.Stanza == "" {
			return fmt.Errorf("target.source.stanza is required for pgbackrest")
		}
		if t.Restore.Image == "" {
			return fmt.Errorf("target.restore.image is required for pgbackrest (needs postgres + pgbackrest)")
		}
	case "":
		return fmt.Errorf("target.source.kind is required (pg_dump|sql|pgbackrest)")
	default:
		return fmt.Errorf("target.source.kind %q unsupported (pg_dump|sql|pgbackrest)", t.Source.Kind)
	}
	return nil
}

// Restore stands up the ephemeral environment for cfg's source kind, restores
// into it, and returns a live RestoredTarget. It preserves the original
// operational-vs-verdict error split: environment/secret/Docker problems are
// wrapped as spi.Fault (operational); a backup that fails to restore is a bare
// error (a "fail" verdict). warnings is pg_restore's benign-error note, if any.
func (Engine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, "", spi.Faultf(err) // operational: docker missing/unreachable
	}
	src := cfg.Target.Source
	switch src.Kind {
	case "pgbackrest":
		if err := requireEnv(src.PassEnv); err != nil {
			return nil, "", spi.Faultf(err)
		}
		env, err := startPgBackRest(ctx, cfg)
		if err != nil {
			return nil, "", spi.Faultf(err) // operational: couldn't create the environment
		}
		if err := env.Restore(ctx, src.Stanza, ""); err != nil {
			_ = env.Stop()
			return nil, "", err // verdict fail: the backup did not restore/recover
		}
		return env, "", nil

	default: // pg_dump, sql
		pg, err := ephemeral.StartPostgres(ctx, cfg.Target.Restore.Image, cfg.Target.Restore.Database)
		if err != nil {
			return nil, "", spi.Faultf(err) // operational
		}
		warn, rerr := pg.Restore(ctx, src.Kind, src.Path)
		if rerr != nil {
			_ = pg.Stop()
			return nil, "", rerr // verdict fail
		}
		return pg, warn, nil
	}
}

// Discover proposes candidate checks from the restored cluster: it asserts the
// restored target to discover.RowQueryer and calls the existing Postgres
// catalog introspection verbatim. Part of the spi.Scaffolder capability behind
// `salvage scaffold` (spec 0028 R2) — Postgres is the first implementer, and
// this is a behaviour-preserving wrap: the discovery logic, heuristics, and
// thresholds in internal/discover are unchanged, so scaffold output is
// byte-identical to before the capability existed.
//
// Candidates carry no cap group: internal/discover applies its own historical
// per-table cap (spec 0009 R4), and re-capping in the shared emission layer
// could reorder or re-truncate — violating the byte-identical guarantee.
func (Engine) Discover(ctx context.Context, rt spi.RestoredTarget, cfg *config.Config) ([]spi.ScaffoldCandidate, error) {
	rq, ok := rt.(discover.RowQueryer)
	if !ok {
		return nil, fmt.Errorf("restored target for target.type %q does not answer row queries", cfg.Target.Type)
	}
	disc, err := discover.Introspect(ctx, rq, cfg.Target.Restore.Database)
	if err != nil {
		return nil, fmt.Errorf("introspect: %w", err)
	}
	generated := discover.GenerateChecks(disc)
	cands := make([]spi.ScaffoldCandidate, len(generated))
	for i, c := range generated {
		cands[i] = spi.ScaffoldCandidate{Check: c}
	}
	return cands, nil
}

// Chain returns the pgBackRest backup chain (newest first) for cfg's stanza,
// standing up a short-lived env just to read `pgbackrest info`. Part of the
// spi.ChainTester capability that backs `last-good`.
func (Engine) Chain(ctx context.Context, cfg *config.Config) ([]spi.Backup, error) {
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, err
	}
	if err := requireEnv(cfg.Target.Source.PassEnv); err != nil {
		return nil, err
	}
	stanza := cfg.Target.Source.Stanza
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
	out := make([]spi.Backup, len(backups))
	for i, b := range backups {
		out[i] = spi.Backup{Label: b.Label, Type: b.Type, Timestamp: b.Timestamp}
	}
	return out, nil
}

// TestBackup restore-tests one backup (pinned by label) with cfg's checks in a
// fresh, throwaway env. Returns "" on success or a short failure reason (a
// restore error, or the first failing required check). Part of spi.ChainTester.
func (Engine) TestBackup(ctx context.Context, cfg *config.Config, label string) string {
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

// Survey enumerates every stanza in the pgBackRest repo (metadata only, no
// restore). Part of the spi.FleetSurveyor capability that backs `fleet`.
func (Engine) Survey(ctx context.Context, cfg *config.Config) ([]spi.FleetUnit, error) {
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, err
	}
	if err := requireEnv(cfg.Target.Source.PassEnv); err != nil {
		return nil, err
	}
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
	units := make([]spi.FleetUnit, 0, len(stanzas))
	for _, s := range stanzas {
		u := spi.FleetUnit{
			Name:        s.Name,
			Status:      statusText(s),
			BackupCount: len(s.Backups),
		}
		if nb, ok := s.Newest(); ok {
			u.NewestLabel = nb.Label
			ts := nb.Timestamp
			u.NewestBackup = &ts
		}
		units = append(units, u)
	}
	return units, nil
}

// SkeletonSource returns the Source to embed in a per-stanza skeleton config: the
// configured source with the stanza swapped in, so repo location + credentials
// carry over. Part of spi.FleetSurveyor.
func (Engine) SkeletonSource(cfg *config.Config, unit string) config.Source {
	src := cfg.Target.Source
	src.Stanza = unit
	return src
}

// requireEnv fails if any named pass_env var is unset — the same precondition the
// orchestrator used to enforce inline before touching Docker.
func requireEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) == "" {
			return fmt.Errorf("required env %s is not set (export it before running salvage)", name)
		}
	}
	return nil
}

func startPgBackRest(ctx context.Context, cfg *config.Config) (*ephemeral.PgBackRest, error) {
	return ephemeral.StartRestoreEnv(ctx, cfg.Target.Restore.Image,
		cfg.Target.Source.RepoPath, cfg.Target.Source.RepoVolume,
		cfg.Target.Restore.Database, cfg.Target.Restore.User,
		cfg.Target.Source.PassEnv, cfg.Target.Restore.PreloadLibraries)
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

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
