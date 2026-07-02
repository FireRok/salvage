// Package borg implements the borg engine (spec 0022): Salvage's second non-SQL
// engine and second filesystem engine, registered against target.type "borg". It
// is a near-exact sibling of the restic engine (spec 0018): it extracts a
// BorgBackup archive into a throwaway container and validates it with
// file/command checks instead of SQL, inheriting the report, verdict,
// attestation, and dead-man's-switch layers unchanged.
//
// The file/command check kinds (file_exists, file_count, checksum, command) and
// the FileProber capability they assert live in internal/probe (spec 0020): the
// borg RestoredTarget satisfies probe.FileProber via docker exec, and importing
// probe wires those kinds in. borg registers NO new evaluators — it
// inherits the shared prober exactly as restic and exec do.
package borg

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

// Engine is the borg engine. Stateless; each Restore stands up its own throwaway
// container.
type Engine struct{}

// The engine contributes its own config validation (spec 0016 R6), tree-walk
// check discovery for `salvage scaffold` (spec 0028 R4), and its probe
// capability declaration (backlog S4).
var (
	_ spi.ConfigValidator    = Engine{}
	_ spi.Scaffolder         = Engine{}
	_ spi.CapabilityDeclarer = Engine{}
)

func (Engine) Type() string { return "borg" }

// TargetCapabilities declares what the restored target can carry (backlog S4):
// file probes via docker exec, and HTTP via the embedded host prober — the
// same set as restic. Part of spi.CapabilityDeclarer.
func (Engine) TargetCapabilities() []config.TargetCapability {
	return []config.TargetCapability{config.CapabilityFileProbe, config.CapabilityHTTPProbe}
}

// Discover proposes file checks from a bounded, deterministic walk of the
// extracted archive (spec 0028 R4), shared with restic and exec via fsdiscover:
// a required non-empty-root presence check, advisory file_exists anchors, and
// advisory file_count floors on the most-populated directories — all existing
// kinds, no new evaluator. Part of spi.Scaffolder.
func (Engine) Discover(ctx context.Context, rt spi.RestoredTarget, cfg *config.Config) ([]spi.ScaffoldCandidate, error) {
	fp, ok := rt.(probe.FileProber)
	if !ok {
		return nil, fmt.Errorf("restored target for target.type %q does not expose file probes", cfg.Target.Type)
	}
	return fsdiscover.Discover(ctx, fp)
}

// ValidateConfig checks a target.type borg source (spec 0022). It mirrors the
// restic engine's ValidateConfig — the repository is supplied inline
// (Source.Repository, a non-secret path/URL) or by reference (BORG_REPO in
// pass_env) — and additionally requires an archive, since borg has no "latest"
// alias to fall back on. These rules — and their messages — used to live in
// config.Validate's central switch.
func (Engine) ValidateConfig(cfg *config.Config) error {
	t := cfg.Target
	if t.Source.Kind != "borg" {
		return fmt.Errorf("target.source.kind %q unsupported for target.type borg (only \"borg\")", t.Source.Kind)
	}
	if t.Source.Repository == "" && !slices.Contains(t.Source.PassEnv, "BORG_REPO") {
		return fmt.Errorf("target.source: set repository, or forward BORG_REPO via pass_env")
	}
	if t.Source.Archive == "" {
		return fmt.Errorf("target.source.archive is required for borg (no \"latest\" alias)")
	}
	return nil
}

// Restore stands up the borg container, extracts the configured archive, and
// returns a live RestoredTarget exposing a probe.FileProber. It preserves the
// operational-vs-verdict split: a missing pass_env secret or a Docker/container
// problem is a spi.Fault (operational); an archive that fails to extract is a
// bare error (a "fail" verdict). borg extracts are quiet, so warnings is "".
func (Engine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	src := cfg.Target.Source
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, "", spi.Faultf(err) // operational: docker missing/unreachable
	}
	if err := requireEnv(src.PassEnv); err != nil {
		return nil, "", spi.Faultf(err)
	}
	env, err := ephemeral.StartBorg(ctx, cfg.Target.Restore.Image,
		src.Repository, src.RepoVolume, src.RepoPath, src.PassEnv)
	if err != nil {
		return nil, "", spi.Faultf(err) // operational: couldn't create the environment
	}
	if err := env.Restore(ctx, src.Archive); err != nil {
		_ = env.Stop()
		return nil, "", err // verdict fail: the archive did not extract
	}
	return env, "", nil
}

// requireEnv fails if any named pass_env var is unset — the same by-name secret
// precondition the restic/pgBackRest paths enforce before touching Docker.
func requireEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) == "" {
			return fmt.Errorf("required env %s is not set (export it before running salvage)", name)
		}
	}
	return nil
}
