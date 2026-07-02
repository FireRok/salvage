package exec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"salvage.sh/internal/probe"
)

// Host is the exec engine's RestoredTarget: a host prober. It runs command checks
// via os/exec, file probes via os/path/filepath/crypto/sha256, and HTTP checks
// via net/http (the embedded probe.HostHTTP) — all from the Salvage host, where
// the restore command just ran (spec 0020 R2/R3). It satisfies probe.FileProber,
// probe.HTTPProber, and spi.RestoredTarget (Stop).
type Host struct {
	probe.HostHTTP

	// workdir is the checks' working directory (the restore's workdir). Relative
	// paths in file probes and command checks resolve here; "" means the Salvage
	// process's cwd.
	workdir string
	// env is the environment command checks run with (the Salvage process env).
	env []string
	// cleanup, if non-empty, is an argv run once on Stop().
	cleanup []string

	stopOnce sync.Once
	stopErr  error
}

var (
	_ probe.FileProber = (*Host)(nil)
	_ probe.HTTPProber = (*Host)(nil)
)

// abs resolves a config path against the checks' workdir. An absolute path is
// returned unchanged; a relative path joins the workdir (or cwd when unset).
func (h *Host) abs(path string) string {
	if filepath.IsAbs(path) || h.workdir == "" {
		return path
	}
	return filepath.Join(h.workdir, path)
}

// Exists reports whether path is present on the host filesystem.
func (h *Host) Exists(_ context.Context, path string) (bool, error) {
	_, err := os.Stat(h.abs(path))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err // an operational stat error (e.g. permission), not "absent"
}

// Count returns how many files match pattern, a filepath.Glob-style glob
// resolved against the workdir. Directories are excluded, mirroring the restic
// engine's "count of files" semantics.
func (h *Host) Count(_ context.Context, pattern string) (int, error) {
	matches, err := filepath.Glob(h.abs(pattern))
	if err != nil {
		return 0, fmt.Errorf("count: bad glob %q: %w", pattern, err)
	}
	n := 0
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue // a match that vanished between glob and stat: skip it
		}
		if !fi.IsDir() {
			n++
		}
	}
	return n, nil
}

// Sha256 returns the lowercase hex sha256 of the file at path.
func (h *Host) Sha256(_ context.Context, path string) (string, error) {
	f, err := os.Open(h.abs(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// RunCommand runs cmd via `sh -c` in the workdir with the inherited environment,
// returning stdout, the exit code, and an error only for an operational failure
// (the command could not be started at all). A non-zero exit is reported via
// exit, not err — so a check distinguishes "ran and failed" (a verdict) from
// "could not run" (operational). This mirrors the restic prober's contract.
func (h *Host) RunCommand(ctx context.Context, cmd string) (string, int, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = h.workdir
	c.Env = h.env
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	out := stdout.String()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// Ran and exited non-zero: report the exit code, not an error. Surface
			// stderr in the returned stdout tail so a failing command has a reason.
			code := ee.ExitCode()
			if out == "" {
				out = strings.TrimRight(stderr.String(), "\n")
			}
			return out, code, nil
		}
		// Could not start (sh missing, context cancelled): operational.
		return out, -1, err
	}
	return out, 0, nil
}

// Stop runs the optional cleanup command once. Its failure is a warning, never a
// verdict change (spec 0020 R1/R2); Stop is idempotent (safe to call more than
// once) and returns the cleanup error only for the caller's logging.
func (h *Host) Stop() error {
	h.stopOnce.Do(func() {
		if len(h.cleanup) == 0 {
			return
		}
		// Fresh context so cleanup still runs if the parent ctx timed out.
		c := exec.Command(h.cleanup[0], h.cleanup[1:]...)
		c.Dir = h.workdir
		c.Env = h.env
		if err := c.Run(); err != nil {
			h.stopErr = fmt.Errorf("cleanup command failed: %w", err)
		}
	})
	return h.stopErr
}
