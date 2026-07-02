package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salvage.sh/internal/config"
)

// Spec 0028 R7: the exec engine's scaffold-assist runs through the same
// spi.Scaffolder seam as every other engine, observing the tree the restore
// command left in restore.workdir with the shared fsdiscover walk — here
// against a real directory via the host prober.
func TestExecDiscoverWalksWorkdir(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"app.conf":     "conf",
		"ignored.tmp":  "scratch",
		"data/a.dat":   "a",
		"data/b.dat":   "b",
		"data/c/d.dat": "d",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Target: config.Target{
		Type:    "exec",
		Restore: config.Restore{Command: []string{"true"}, Workdir: dir},
	}}
	rt := &Host{workdir: dir, env: os.Environ()}

	cands, err := Engine{}.Discover(context.Background(), rt, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byName := map[string]config.Check{}
	for _, c := range cands {
		byName[c.Check.Name] = c.Check
	}
	if _, ok := byName["restore_root_nonempty"]; !ok {
		t.Errorf("missing the required root presence check; got %v", byName)
	}
	if a, ok := byName["anchor_app_conf_exists"]; !ok || a.Kind != "file_exists" || a.Path != "app.conf" {
		t.Errorf("anchor_app_conf_exists = %+v, want a file_exists on app.conf", a)
	}
	if _, ok := byName["anchor_ignored_tmp_exists"]; ok {
		t.Error("temp file proposed as an anchor")
	}
	// The host prober's Count is a single-level file glob: data/* holds a.dat,
	// b.dat (c is a directory, excluded) — the floor is the observed 2, so the
	// emitted check passes by construction on this same prober.
	d, ok := byName["dir_data_files"]
	if !ok || d.ExpectMin == nil || *d.ExpectMin != 2 {
		t.Errorf("dir_data_files = %+v, want an advisory floor of 2", d)
	}
}

// Without a declared workdir there is nothing observable yet (the observe.*
// hints of spec 0021 have no config surface): scaffold must explain what to
// declare, not guess or panic.
func TestExecDiscoverNeedsWorkdir(t *testing.T) {
	cfg := &config.Config{Target: config.Target{
		Type:    "exec",
		Restore: config.Restore{Command: []string{"true"}},
	}}
	_, err := Engine{}.Discover(context.Background(), &Host{}, cfg)
	if err == nil || !strings.Contains(err.Error(), "restore.workdir") {
		t.Errorf("Discover without workdir = %v, want an error naming restore.workdir", err)
	}
}
