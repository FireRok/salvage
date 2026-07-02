// Package probe holds the check kinds shared by every engine whose RestoredTarget
// is probed by running commands, inspecting files, or making HTTP requests —
// rather than answering SQL. It owns the FileProber and HTTPProber capability
// interfaces and the evaluators keyed to them, and registers those kinds once in
// its init().
//
// This is a behaviour-preserving lift: the FileProber contract and the
// file_exists/file_count/checksum/command evaluators used to live in the restic
// engine (spec 0018) and register there. Both the restic engine (a docker-exec
// FileProber) and the exec engine (a host-based FileProber, spec 0020) now
// satisfy probe.FileProber and inherit these four kinds without duplicating the
// registration — Go runs a package's init once, so blank-importing either engine
// pulls this package in exactly once.
package probe

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

func init() {
	// The non-SQL check kinds. Each type-asserts checks.Target to the capability
	// it needs (FileProber or HTTPProber), so the horizontal orchestration
	// (checks.Run) stays engine-agnostic (spec 0017 R3).
	checks.RegisterEvaluator("file_exists", evalFileExists)
	checks.RegisterEvaluator("file_count", evalFileCount)
	checks.RegisterEvaluator("checksum", evalChecksum)
	checks.RegisterEvaluator("command", evalCommand)
	checks.RegisterEvaluator("http", evalHTTP)
}

// FileProber is what a probed RestoredTarget exposes instead of a SQL Queryer:
// read-only probes of the restored tree, plus a command runner. The file/command
// check evaluators type-assert the opaque checks.Target to this interface. The
// restic engine implements it via `docker exec`; the exec engine implements it
// against the Salvage host (os/exec, os.Stat, filepath.Walk, crypto/sha256).
type FileProber interface {
	// Exists reports whether path is present in the restored tree.
	Exists(ctx context.Context, path string) (bool, error)
	// Count returns how many files match pattern (a find-style glob) in the tree.
	Count(ctx context.Context, pattern string) (int, error)
	// Sha256 returns the lowercase hex sha256 of the file at path.
	Sha256(ctx context.Context, path string) (string, error)
	// RunCommand runs cmd in the restored tree, returning stdout and the exit
	// code. err is non-nil only for an operational failure (the command could not
	// be run at all); a non-zero exit is reported via exit, not err.
	RunCommand(ctx context.Context, cmd string) (out string, exit int, err error)
}

// firstLine returns s up to the first newline (a compact detail for a failing
// command), trimmed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// --- file/command check evaluators (spec 0017 R3 kinds) ---

// prober type-asserts the opaque target to FileProber, returning a clear failing
// result when the target cannot probe files (e.g. a SQL engine's target reached a
// file check). Never panics.
func prober(target checks.Target, c config.Check) (FileProber, *report.CheckResult) {
	fp, ok := target.(FileProber)
	if !ok {
		return nil, &report.CheckResult{
			Name:     c.Name,
			Severity: c.Severity,
			Error:    "file check requires a filesystem-probeable target (target.type restic, borg, or exec)",
		}
	}
	return fp, nil
}

// evalFileExists passes iff the path's presence matches the Bool expectation
// (default true, i.e. "the file must exist").
func evalFileExists(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	fp, fail := prober(target, c)
	if fail != nil {
		return *fail
	}
	want := true
	if c.Bool != nil {
		want = *c.Bool
	}
	present, err := fp.Exists(ctx, c.Path)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Got = strconv.FormatBool(present)
	res.OK = present == want
	if !res.OK {
		if want {
			res.Detail = "expected " + c.Path + " to exist"
		} else {
			res.Detail = "expected " + c.Path + " to be absent"
		}
	}
	return res
}

// evalFileCount passes iff the number of files matching Path is within
// expect_min/expect_max (either bound optional).
func evalFileCount(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	fp, fail := prober(target, c)
	if fail != nil {
		return *fail
	}
	n, err := fp.Count(ctx, c.Path)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Got = strconv.Itoa(n)
	v := float64(n)
	res.OK = true
	switch {
	case c.ExpectMin != nil && v < *c.ExpectMin:
		res.OK = false
		res.Detail = fmt.Sprintf("%d < min %g", n, *c.ExpectMin)
	case c.ExpectMax != nil && v > *c.ExpectMax:
		res.OK = false
		res.Detail = fmt.Sprintf("%d > max %g", n, *c.ExpectMax)
	default:
		res.Detail = fmt.Sprintf("%d within bounds", n)
	}
	return res
}

// evalChecksum passes iff the sha256 of Path equals the expected hex (Equals).
func evalChecksum(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	fp, fail := prober(target, c)
	if fail != nil {
		return *fail
	}
	if c.Equals == nil {
		res.Error = "checksum check needs an expected sha256 in equals"
		return res
	}
	sum, err := fp.Sha256(ctx, c.Path)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Got = sum
	res.OK = sum == *c.Equals
	if !res.OK {
		res.Detail = "want " + *c.Equals
	}
	return res
}

// evalCommand runs Command in the restored tree. It passes iff the command
// exits 0 — or, when Equals is set, iff stdout equals it (exit code ignored),
// mirroring the sql equals expectation for a command's output.
func evalCommand(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	fp, fail := prober(target, c)
	if fail != nil {
		return *fail
	}
	out, exit, err := fp.RunCommand(ctx, c.Command)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if c.Equals != nil {
		res.Got = out
		// Propagate the opt-in to keep the exact stdout literal through report
		// redaction (spec 0027 R2/R5): the equals comparison above always uses
		// the raw value; what is *stored* is decided at serialization, where
		// known-secret scrubbing still applies even to a kept literal.
		res.KeepLiteral = c.KeepLiteral
		res.OK = out == *c.Equals
		if !res.OK {
			res.Detail = "want " + *c.Equals
		}
		return res
	}
	res.Got = "exit " + strconv.Itoa(exit)
	res.OK = exit == 0
	if !res.OK {
		res.Detail = firstLine(out)
	}
	return res
}
