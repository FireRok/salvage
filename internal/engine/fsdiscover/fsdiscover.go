// Package fsdiscover is the shared tree-walk scaffold discovery for filesystem
// engines (spec 0028 R4): it observes a restored file tree through a
// probe.FileProber and proposes candidate checks that pin its current shape,
// using only existing check kinds (file_exists, file_count). The restic and
// borg engines call it against their docker-exec probers; the exec engine calls
// it against its host prober when a restore workdir is declared (spec 0028 R7 /
// spec 0021's filesystem observation) — one implementation behind the single
// spi.Scaffolder seam.
//
// The walk is bounded and deterministic (spec 0028 R6, 0021 R2): only the top
// level of the tree is enumerated, listings are sorted (LC_ALL=C), and
// selection rules are pure functions of the listing — so re-running scaffold
// against the same restored target produces identical output. checksum
// proposals are deliberately absent: they default off (many files legitimately
// change between backups) and remain an explicit per-glob opt-in (spec 0028 R4).
package fsdiscover

import (
	"context"
	"fmt"
	"strings"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/probe"
)

// maxAnchors bounds how many stable anchor files Discover proposes — "a
// handful" (spec 0028 R4): enough to pin the tree's identity, few enough to
// review. Directory candidates are capped separately by the shared emission
// layer's top-N policy.
const maxAnchors = 5

// The two bounded, deterministic listing commands. POSIX find + sort, so they
// run identically in the restic/borg containers (busybox) and on the exec
// host's sh; the prober's RunCommand contract puts the cwd at the restore root.
const (
	listFilesCmd = `find . -mindepth 1 -maxdepth 1 -type f -print 2>/dev/null | LC_ALL=C sort`
	listDirsCmd  = `find . -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | LC_ALL=C sort`
)

// Discover walks the restored tree behind fp and proposes candidate checks
// (spec 0028 R4):
//
//   - a non-empty-root presence check — **required**;
//   - `file_exists` on up to maxAnchors stable anchor files (top-level,
//     lexically first, non-temp) — **advisory**;
//   - `file_count` floors on the top-level directories, floor = observed count
//     — **advisory**, cap-grouped by observed population so the shared
//     emission layer keeps the most-populated directories.
//
// Every threshold is the observed value, so candidates pass by construction on
// the snapshot they came from; the orchestrator's verify-by-running net (spec
// 0028 R5) drops any that don't.
func Discover(ctx context.Context, fp probe.FileProber) ([]spi.ScaffoldCandidate, error) {
	files, err := listTopLevel(ctx, fp, listFilesCmd)
	if err != nil {
		return nil, fmt.Errorf("list restored files: %w", err)
	}
	dirs, err := listTopLevel(ctx, fp, listDirsCmd)
	if err != nil {
		return nil, fmt.Errorf("list restored directories: %w", err)
	}

	cands := []spi.ScaffoldCandidate{{
		// Structural presence (required, never capped): the restore root came
		// back non-empty — the filesystem analogue of schema_present.
		Check: config.Check{
			Name:      "restore_root_nonempty",
			Kind:      "file_count",
			Path:      "*",
			ExpectMin: f64ptr(1),
			Severity:  "required",
		},
	}}

	// Anchor files (advisory): deterministically the lexically-first non-temp
	// top-level files. Existence-only — content pinning (checksum) stays opt-in.
	anchors := 0
	for _, f := range files {
		if anchors >= maxAnchors {
			break
		}
		if isTempName(f) {
			continue
		}
		anchors++
		cands = append(cands, spi.ScaffoldCandidate{
			Check: config.Check{
				Name:     "anchor_" + sanitize(f) + "_exists",
				Kind:     "file_exists",
				Path:     f,
				Bool:     boolptr(true),
				Severity: "advisory",
			},
			Group:   "anchor",
			Subject: f,
		})
	}

	// Per top-level directory (advisory): a file-count floor at the observed
	// population. The observed count comes from the same Count the check kind
	// evaluates, so the floor holds by construction on this snapshot.
	for _, d := range dirs {
		if isTempName(d) {
			continue
		}
		pattern := d + "/*"
		n, err := fp.Count(ctx, pattern)
		if err != nil {
			return nil, fmt.Errorf("count files under %s: %w", d, err)
		}
		if n <= 0 {
			continue // an empty directory has no floor worth pinning
		}
		cands = append(cands, spi.ScaffoldCandidate{
			Check: config.Check{
				Name:      "dir_" + sanitize(d) + "_files",
				Kind:      "file_count",
				Path:      pattern,
				ExpectMin: f64ptr(float64(n)),
				Severity:  "advisory",
			},
			Group:   "dir",
			Subject: d,
			Weight:  int64(n),
		})
	}
	return cands, nil
}

// listTopLevel runs one of the bounded listing commands and returns the entries
// as restore-root-relative names, in the command's sorted order.
func listTopLevel(ctx context.Context, fp probe.FileProber, cmd string) ([]string, error) {
	out, exit, err := fp.RunCommand(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if exit != 0 {
		return nil, fmt.Errorf("listing command exited %d: %s", exit, strings.TrimSpace(out))
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		name := strings.TrimPrefix(line, "./")
		if name == "" || name == "." {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// isTempName reports whether a name looks transient — scratch files that
// legitimately differ between backups make unstable anchors (spec 0028 Open
// questions: start conservative).
func isTempName(name string) bool {
	base := name
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || strings.HasPrefix(base, ".") {
		return true // dotfiles: often editor/tool state
	}
	lower := strings.ToLower(base)
	return strings.HasSuffix(base, "~") ||
		strings.HasSuffix(lower, ".tmp") ||
		strings.HasSuffix(lower, ".swp") ||
		strings.HasSuffix(lower, ".part") ||
		strings.HasSuffix(lower, ".lock")
}

// sanitize reduces a path to a lowercase, name-safe token for use in a check
// name (purely cosmetic; the check's Path carries the real name).
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "entry"
	}
	return out
}

func f64ptr(f float64) *float64 { return &f }
func boolptr(b bool) *bool      { return &b }
