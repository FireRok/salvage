// Package restic implements the restic engine (spec 0018): Salvage's first
// non-SQL engine, registered against target.type "restic". It restores a restic
// filesystem snapshot into a throwaway container and validates it with
// file/command checks instead of SQL — exercising the validation-generalization
// seam (spec 0017 R3) end to end. The report, verdict, attestation, and
// dead-man's-switch layers are inherited unchanged; only restore + check
// evaluation are engine-specific here.
//
// The file/command check kinds (file_exists, file_count, checksum, command) and
// the FileProber capability they assert now live in internal/probe (spec 0020):
// the restic RestoredTarget satisfies probe.FileProber via docker exec, and
// importing probe wires those kinds in. The exec engine shares the same
// evaluators against a host prober, so neither engine re-registers them.
package restic

import (
	"context"
	"fmt"
	"os"
	"slices"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/fsdiscover"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/probe"
)

func init() { spi.Register(Engine{}) }

// Engine is the restic engine. Stateless; each Restore stands up its own
// throwaway container.
type Engine struct{}

// The engine contributes its own config validation (spec 0016 R6), tree-walk
// check discovery for `salvage scaffold` (spec 0028 R4), and its probe
// capability declaration (backlog S4).
var (
	_ spi.ConfigValidator    = Engine{}
	_ spi.Scaffolder         = Engine{}
	_ spi.CapabilityDeclarer = Engine{}
)

func (Engine) Type() string { return "restic" }

// TargetCapabilities declares what the restored target can carry (backlog S4):
// file probes via docker exec, and HTTP via the embedded host prober — so the
// file/command kinds and the http kind all validate for target.type restic.
// Part of spi.CapabilityDeclarer.
func (Engine) TargetCapabilities() []config.TargetCapability {
	return []config.TargetCapability{config.CapabilityFileProbe, config.CapabilityHTTPProbe}
}

// Discover proposes file checks from a bounded, deterministic walk of the
// restored tree (spec 0028 R4): a required non-empty-root presence check,
// advisory file_exists anchors, and advisory file_count floors on the
// most-populated directories — all existing kinds, no new evaluator. The walk
// is shared with borg and exec via fsdiscover. Part of spi.Scaffolder.
func (Engine) Discover(ctx context.Context, rt spi.RestoredTarget, cfg *config.Config) ([]spi.ScaffoldCandidate, error) {
	fp, ok := rt.(probe.FileProber)
	if !ok {
		return nil, fmt.Errorf("restored target for target.type %q does not expose file probes", cfg.Target.Type)
	}
	return fsdiscover.Discover(ctx, fp)
}

// ValidateConfig checks a target.type restic source. The repository is supplied
// either inline (Source.Repository, a non-secret path/URL) or by reference
// (RESTIC_REPOSITORY in pass_env); requiring one of the two catches the common
// "repo not configured" mistake without ever demanding a secret in-file. These
// rules — and their messages — used to live in config.Validate's central
// switch; the engine now contributes them itself via spi.ConfigValidator.
func (Engine) ValidateConfig(cfg *config.Config) error {
	t := cfg.Target
	if t.Source.Kind != "restic" {
		return fmt.Errorf("target.source.kind %q unsupported for target.type restic (only \"restic\")", t.Source.Kind)
	}
	if t.Source.Repository == "" && !slices.Contains(t.Source.PassEnv, "RESTIC_REPOSITORY") {
		return fmt.Errorf("target.source: set repository, or forward RESTIC_REPOSITORY via pass_env")
	}
	return nil
}

// Restore stands up the restic container, restores the configured snapshot, and
// returns a live RestoredTarget exposing a probe.FileProber. It preserves the
// operational-vs-verdict split: a missing pass_env secret or a Docker/container
// problem is a spi.Fault (operational); a snapshot that fails to restore is a
// bare error (a "fail" verdict). restic restores are quiet, so warnings is "".
func (Engine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	src := cfg.Target.Source
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, "", spi.Faultf(err) // operational: docker missing/unreachable
	}
	if err := requireEnv(src.PassEnv); err != nil {
		return nil, "", spi.Faultf(err)
	}
	env, err := ephemeral.StartRestic(ctx, cfg.Target.Restore.Image,
		src.Repository, src.RepoVolume, src.RepoPath, src.PassEnv)
	if err != nil {
		return nil, "", spi.Faultf(err) // operational: couldn't create the environment
	}
	if err := env.Restore(ctx, src.Snapshot); err != nil {
		_ = env.Stop()
		return nil, "", err // verdict fail: the snapshot did not restore
	}
	return env, "", nil
}

// requireEnv fails if any named pass_env var is unset — the same by-name secret
// precondition the pgBackRest path enforces before touching Docker.
func requireEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) == "" {
			return fmt.Errorf("required env %s is not set (export it before running salvage)", name)
		}
	}
	return nil
}
