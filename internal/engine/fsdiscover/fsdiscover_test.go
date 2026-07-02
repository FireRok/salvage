package fsdiscover

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeProber scripts the two listing commands and the per-directory counts, so
// the walk is exercised without Docker. It intentionally answers only what
// Discover is specified to ask: an unexpected command or pattern fails the test.
type fakeProber struct {
	files  []string // top-level files, pre-sorted (as `sort` would emit)
	dirs   []string // top-level dirs, pre-sorted
	counts map[string]int
}

func (f *fakeProber) Exists(ctx context.Context, path string) (bool, error) { return true, nil }
func (f *fakeProber) Sha256(ctx context.Context, path string) (string, error) {
	return "", fmt.Errorf("not used")
}

func (f *fakeProber) Count(ctx context.Context, pattern string) (int, error) {
	n, ok := f.counts[pattern]
	if !ok {
		return 0, fmt.Errorf("unexpected Count pattern %q", pattern)
	}
	return n, nil
}

func (f *fakeProber) RunCommand(ctx context.Context, cmd string) (string, int, error) {
	switch cmd {
	case listFilesCmd:
		return joinFind(f.files), 0, nil
	case listDirsCmd:
		return joinFind(f.dirs), 0, nil
	}
	return "", -1, fmt.Errorf("unexpected command %q", cmd)
}

func joinFind(names []string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString("./" + n + "\n")
	}
	return b.String()
}

// Spec 0028 R4: the walk proposes a required non-empty-root check, advisory
// existence anchors on the lexically-first non-temp top-level files (a bounded
// handful), and advisory file-count floors on populated directories — and never
// a checksum (default off) or a check for an absent/empty path.
func TestDiscoverTreeWalk(t *testing.T) {
	fp := &fakeProber{
		// Sorted as `LC_ALL=C sort` would emit; dotfiles and temp suffixes must be
		// skipped, and the anchor cap must stop at maxAnchors keepers.
		files: []string{".env", "README.md", "app.conf", "backup.tar~", "data.bin",
			"scratch.tmp", "v1.sql", "v2.sql", "v3.sql"},
		dirs: []string{"cache", "etc", "var"},
		counts: map[string]int{
			"cache/*": 0, // empty: no floor worth pinning
			"etc/*":   4,
			"var/*":   250,
		},
	}
	cands, err := Discover(context.Background(), fp)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	var names []string
	for _, c := range cands {
		names = append(names, c.Check.Name)
	}
	want := []string{
		"restore_root_nonempty",
		"anchor_readme_md_exists", "anchor_app_conf_exists", "anchor_data_bin_exists",
		"anchor_v1_sql_exists", "anchor_v2_sql_exists",
		"dir_etc_files", "dir_var_files",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates = %v, want %v", names, want)
	}

	root := cands[0]
	if root.Check.Kind != "file_count" || root.Check.Path != "*" ||
		root.Check.Severity != "required" || root.Group != "" {
		t.Errorf("root check = %+v (group %q), want a required, uncapped file_count on *", root.Check, root.Group)
	}

	for _, c := range cands[1:6] {
		if c.Check.Kind != "file_exists" || c.Check.Severity != "advisory" ||
			c.Check.Bool == nil || !*c.Check.Bool || c.Group != "anchor" {
			t.Errorf("anchor = %+v (group %q), want an advisory file_exists anchor", c.Check, c.Group)
		}
	}

	varDir := cands[len(cands)-1]
	if varDir.Check.Kind != "file_count" || varDir.Check.Path != "var/*" ||
		varDir.Check.ExpectMin == nil || *varDir.Check.ExpectMin != 250 ||
		varDir.Check.Severity != "advisory" {
		t.Errorf("dir check = %+v, want an advisory floor of the observed 250", varDir.Check)
	}
	if varDir.Group != "dir" || varDir.Subject != "var" || varDir.Weight != 250 {
		t.Errorf("dir cap metadata = %q/%q/%d, want dir/var/250", varDir.Group, varDir.Subject, varDir.Weight)
	}

	for _, c := range cands {
		if c.Check.Kind == "checksum" {
			t.Errorf("checksum proposed by default: %+v (must be opt-in, spec 0028 R4)", c.Check)
		}
	}
}

// An empty restored tree still yields the structural presence check — which
// verify-by-running will then fail and drop is NOT the desired path here: an
// empty tree means the restore is broken, and the required check documents the
// expectation. Discovery itself must not error.
func TestDiscoverEmptyTree(t *testing.T) {
	fp := &fakeProber{counts: map[string]int{}}
	cands, err := Discover(context.Background(), fp)
	if err != nil {
		t.Fatalf("Discover on an empty tree: %v", err)
	}
	if len(cands) != 1 || cands[0].Check.Name != "restore_root_nonempty" {
		t.Errorf("candidates = %+v, want only the root presence check", cands)
	}
}
