// last-good/fleet capabilities for the restic engine (spec 0029): the engine
// implements spi.ChainTester and spi.FleetSurveyor, so both commands light up
// through the orchestrator's capability-only gate with no orchestrator change.
//
// The shapes mirror the pgBackRest engine: Chain/Survey read metadata from one
// `restic snapshots --json` call inside the same idle container Restore uses
// (credentials forwarded by name, spec 0018 R5); TestBackup is exactly a `run`
// pinned to one snapshot — the existing restore + checks machinery, not a fork
// (spec 0010 R5). Each TestBackup is a real restore into a throwaway container;
// `last-good -max` is the bound for long histories (spec 0029 R6).
package restic

import (
	"context"
	"fmt"
	"strings"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/ephemeral"
)

// The engine implements the optional last-good/fleet capabilities (spec 0029).
var (
	_ spi.ChainTester     = Engine{}
	_ spi.FleetSurveyor   = Engine{}
	_ spi.SkeletonChecker = Engine{}
)

// snapshotsCmd lists the repository's snapshots as JSON. stderr is merged so a
// failure's reason (bad password, unreachable repo) survives into the captured
// output; parseSnapshots skips any non-JSON preamble (cache warnings etc.).
const snapshotsCmd = "restic snapshots --json 2>&1"

// Chain enumerates the repository's snapshots newest-first, standing up a
// short-lived idle container just to read `restic snapshots --json`. Part of
// spi.ChainTester (spec 0029 R2).
func (e Engine) Chain(ctx context.Context, cfg *config.Config) ([]spi.Backup, error) {
	out, exit, err := e.metadata(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("restic snapshots: %s", commandFailReason(out, exit))
	}
	return parseSnapshots(out)
}

// TestBackup restore-tests one snapshot (pinned by its label) with cfg's checks
// in a fresh throwaway container — the existing restore path (including the
// two-phase network isolation inside ephemeral's Restore) pinned to label
// instead of "latest", then the configured checks. Returns "" on success or a
// short failure reason. Part of spi.ChainTester (spec 0029 R2).
func (Engine) TestBackup(ctx context.Context, cfg *config.Config, label string) string {
	tctx, cancel := context.WithTimeout(ctx, cfg.Target.Restore.Timeout.Std())
	defer cancel()
	src := cfg.Target.Source
	env, err := ephemeral.StartRestic(tctx, cfg.Target.Restore.Image,
		src.Repository, src.RepoVolume, src.RepoPath, src.PassEnv)
	if err != nil {
		return "could not start restore env: " + err.Error()
	}
	defer env.Stop()
	if err := env.Restore(tctx, label); err != nil {
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

// Survey reports the repository as a single fleet unit — the analogue of one
// pgBackRest stanza — from the same one metadata call Chain uses, with no
// restore (spec 0011 R3, spec 0029 R4). An unreachable or failing repository is
// a degraded *finding* (a unit with an error status), not an operational error;
// only "could not even ask" (Docker, missing secret) returns err.
func (e Engine) Survey(ctx context.Context, cfg *config.Config) ([]spi.FleetUnit, error) {
	out, exit, err := e.metadata(ctx, cfg)
	if err != nil {
		return nil, err
	}
	unit := spi.FleetUnit{Name: repoIdentity(cfg)}
	if exit != 0 {
		unit.Status = commandFailReason(out, exit)
		return []spi.FleetUnit{unit}, nil
	}
	backups, err := parseSnapshots(out)
	if err != nil {
		return nil, err
	}
	unit.BackupCount = len(backups)
	if len(backups) == 0 {
		unit.Status = "empty"
	} else {
		unit.Status = "ok"
		unit.NewestLabel = backups[0].Label
		ts := backups[0].Timestamp
		unit.NewestBackup = &ts
	}
	return []spi.FleetUnit{unit}, nil
}

// SkeletonSource returns the source for the per-unit skeleton config: the base
// source unchanged, repository and credentials carried over — a single-repo
// engine has no per-stanza swap (spec 0029 R4). The emitted skeleton is a
// directly usable `run` input.
func (Engine) SkeletonSource(cfg *config.Config, unit string) config.Source {
	return cfg.Target.Source
}

// SkeletonChecks returns the structural checks for a fleet skeleton: the
// filesystem analogue of "schema present" is "the restored tree is non-empty".
// Part of spi.SkeletonChecker.
func (Engine) SkeletonChecks() []config.Check {
	min := 1.0
	return []config.Check{{
		Name:      "restore_not_empty",
		Kind:      "file_count",
		Path:      "*",
		ExpectMin: &min,
		Severity:  "required",
	}}
}

// metadata stands up the idle container, runs the one snapshots-listing call,
// and tears the container down. err is operational only (Docker unavailable, a
// missing pass_env secret, exec failure); a failing restic command comes back
// as (output, non-zero exit, nil).
func (Engine) metadata(ctx context.Context, cfg *config.Config) (string, int, error) {
	if err := ephemeral.Preflight(ctx); err != nil {
		return "", 0, err
	}
	src := cfg.Target.Source
	if err := requireEnv(src.PassEnv); err != nil {
		return "", 0, err
	}
	env, err := ephemeral.StartRestic(ctx, cfg.Target.Restore.Image,
		src.Repository, src.RepoVolume, src.RepoPath, src.PassEnv)
	if err != nil {
		return "", 0, err
	}
	defer env.Stop()
	return env.RunCommand(ctx, snapshotsCmd)
}

// repoIdentity names the surveyed repository from non-secret config values
// only: the in-file repository location when present, else the target name. A
// repository forwarded by name via pass_env may be a secret and is never echoed.
func repoIdentity(cfg *config.Config) string {
	if r := cfg.Target.Source.Repository; r != "" {
		return r
	}
	if n := cfg.Target.Name; n != "" {
		return n
	}
	return "repository"
}

// commandFailReason extracts the most useful line from a failed metadata call's
// merged output — restic prefixes real causes with "Fatal:"/"error:" — falling
// back to the first non-empty line, then the bare exit code.
func commandFailReason(out string, exit int) string {
	var first string
	for _, ln := range strings.Split(out, "\n") {
		l := strings.TrimSpace(ln)
		if l == "" {
			continue
		}
		if first == "" {
			first = l
		}
		low := strings.ToLower(l)
		if strings.HasPrefix(low, "fatal:") || strings.HasPrefix(low, "error:") {
			return l
		}
	}
	if first != "" {
		return first
	}
	return fmt.Sprintf("exited %d", exit)
}

// firstLine truncates a (possibly multi-line) error to its first line, the same
// short-reason shape the pgBackRest TestBackup reports.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
