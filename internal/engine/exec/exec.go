// Package exec implements the exec engine (spec 0020): bring-your-own-restore.
// Salvage does not know how to restore the customer's backup, so the customer
// brings the restore *command*; Salvage runs it, then runs the customer's checks
// — expressed in the Salvage config format — against whatever the command left on
// the host. It is Docker-free: the restore lands wherever the command puts it (a
// local DB, a running service, a directory), and checks run from the Salvage host.
//
// The report/verdict/attestation/dead-man's-switch layers are inherited unchanged
// (spec 0020 R5); the report marks the restore as operator-supplied (R7) via
// report.RestoreResult.Method == "exec", set by the orchestrator.
//
// The four file/command check kinds come from internal/probe (the same evaluators
// the restic engine uses); the host RestoredTarget satisfies probe.FileProber and
// probe.HTTPProber, so only the http kind is new to this engine's surface.
package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/fsdiscover"
	"salvage.sh/internal/engine/spi"

	// The shared file/command/http check kinds register in probe.init; the host
	// RestoredTarget satisfies probe.FileProber and probe.HTTPProber.
	"salvage.sh/internal/probe"
)

func init() { spi.Register(Engine{}) }

// Engine is the exec engine. Stateless; each Restore runs the customer command.
type Engine struct{}

// The engine contributes its own config validation (spec 0016 R6), scaffold
// discovery (specs 0021/0028), and its probe capability declaration (backlog S4).
var (
	_ spi.ConfigValidator    = Engine{}
	_ spi.Scaffolder         = Engine{}
	_ spi.CapabilityDeclarer = Engine{}
)

func (Engine) Type() string { return "exec" }

// TargetCapabilities declares what the host RestoredTarget can carry (backlog
// S4): file probes and HTTP requests, both from the Salvage host (spec 0020
// R2/R3). Part of spi.CapabilityDeclarer.
func (Engine) TargetCapabilities() []config.TargetCapability {
	return []config.TargetCapability{config.CapabilityFileProbe, config.CapabilityHTTPProbe}
}

// Discover is the exec engine's observe-and-recommend flow (spec 0021), wired
// through the single spi.Scaffolder seam (spec 0028 R7): observe what the
// customer's restore command left behind and propose checks that pin its
// current shape, verified by the orchestrator before emit.
//
// Today it implements the filesystem observation: when restore.workdir is
// declared, the restored tree there is walked with the same bounded,
// deterministic discovery restic and borg use (fsdiscover), emitting the
// existing file_exists/file_count kinds against the host prober. The HTTP and
// client-shelled-SQL observations of spec 0021 (observe.base_url, observe.dsn)
// need their config hint surface to land first; they will extend this method,
// not add a second seam. Without a workdir there is nothing observable, so
// scaffold explains what to declare instead of guessing.
func (Engine) Discover(ctx context.Context, rt spi.RestoredTarget, cfg *config.Config) ([]spi.ScaffoldCandidate, error) {
	if cfg.Target.Restore.Workdir == "" {
		return nil, fmt.Errorf("scaffold for target.type exec needs restore.workdir: it names the directory the restore command populates, which is what discovery observes (spec 0021)")
	}
	fp, ok := rt.(probe.FileProber)
	if !ok {
		return nil, fmt.Errorf("restored target for target.type %q does not expose file probes", cfg.Target.Type)
	}
	return fsdiscover.Discover(ctx, fp)
}

// ValidateConfig checks a target.type exec config (spec 0020 R8): the restore
// is a customer command, so there is no source to validate — only
// restore.command must be non-empty. The customer's command is responsible for
// producing a restored, reachable target; Salvage does not stand up an
// environment. This rule — and its message — used to live in config.Validate's
// central switch.
func (Engine) ValidateConfig(cfg *config.Config) error {
	if len(cfg.Target.Restore.Command) == 0 {
		return fmt.Errorf("target.restore.command is required for target.type exec")
	}
	return nil
}

// restoreTailBytes bounds how much of the restore command's combined stdout/
// stderr is captured into the report's restore detail (warnings). A restore may
// be chatty; we keep only the tail so the report stays small but retains the
// most relevant (final) output.
const restoreTailBytes = 4 << 10 // 4 KiB

// Restore runs the customer's restore.command with the declared env/workdir/
// timeout and returns a live host RestoredTarget. It preserves the
// operational-vs-verdict split (spec 0020 R1):
//   - cannot *launch* the process (empty command, unresolvable workdir, binary
//     not found) → *spi.Fault (operational, exit 2, no verdict);
//   - the command *runs but exits non-zero* → a bare error (a "fail" verdict with
//     a nil operational error).
//
// The combined stdout/stderr tail is returned as warnings so it lands in the
// report's restore detail either way.
func (Engine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	r := cfg.Target.Restore
	if len(r.Command) == 0 {
		return nil, "", spi.Faultf(fmt.Errorf("restore.command is empty"))
	}

	// Resolve the binary before launching so a missing binary is a clean
	// operational fault rather than a confusing exec error mid-run.
	if _, err := exec.LookPath(r.Command[0]); err != nil {
		return nil, "", spi.Faultf(fmt.Errorf("restore command %q not found: %w", r.Command[0], err))
	}
	if r.Workdir != "" {
		if fi, err := os.Stat(r.Workdir); err != nil || !fi.IsDir() {
			return nil, "", spi.Faultf(fmt.Errorf("restore workdir %q is not a usable directory", r.Workdir))
		}
	}

	// Bound the restore phase by the declared timeout (defaulted in config). This
	// only wraps the command execution below — checks run later under their own
	// context — so cancelling here on return is correct.
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(r.Timeout))
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, r.Command[0], r.Command[1:]...)
	cmd.Dir = r.Workdir
	cmd.Env = childEnv(r.Env)

	out, err := cmd.CombinedOutput()
	tail := tailString(string(out), restoreTailBytes)

	if err != nil {
		// A timeout is a restore that did not demonstrably succeed in the allotted
		// time: a "fail" verdict (not operational — the command launched fine).
		if ctx.Err() == context.DeadlineExceeded {
			return nil, tail, fmt.Errorf("restore command timed out after %s", time.Duration(r.Timeout))
		}
		// Distinguish "ran but exited non-zero" (a verdict fail) from "could not
		// start at all" (operational). An *exec.ExitError means the process ran.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The restore command ran and failed: a "fail" verdict, not a fault.
			detail := firstLine(tail)
			if detail == "" {
				detail = ee.String()
			}
			return nil, tail, fmt.Errorf("restore command exited non-zero: %s", detail)
		}
		// Could not launch (path/permission/context problem): operational.
		return nil, "", spi.Faultf(fmt.Errorf("run restore command: %w", err))
	}

	return &Host{workdir: r.Workdir, env: childEnv(r.Env), cleanup: r.Cleanup}, tail, nil
}

// childEnv builds the environment for the restore command and, later, command
// checks. It inherits the Salvage process's full environment so PATH and the
// named pass-through vars resolve. The declared `env` names document which host
// variables the restore depends on (spec 0020 model); their values come from the
// host, never from config, and the command runs with the Salvage process's own
// privileges by design (spec 0020 non-goals: not sandboxed). Naming is the
// contract; we pass the whole environment (a superset) rather than filter, which
// would break commands that also rely on PATH/HOME/etc.
func childEnv(names []string) []string {
	_ = names // declarative: the named vars are already present in os.Environ().
	return os.Environ()
}

// firstLine returns s up to the first newline, trimmed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// tailString returns the last max bytes of s (rune-safe at the cut point),
// prefixing an ellipsis marker when truncated.
func tailString(s string, max int) string {
	if len(s) <= max {
		return strings.TrimRight(s, "\n")
	}
	cut := s[len(s)-max:]
	// Avoid slicing mid-rune: drop up to the first newline in the tail.
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return "…" + strings.TrimRight(cut, "\n")
}
