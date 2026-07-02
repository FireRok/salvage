// Engine-backed validation tests (spec 0016 R6). This file is an *external*
// test package so it can blank-import the concrete engines — package config
// itself never imports them. Importing them here registers every engine's
// target.type (and its optional ValidateConfig / check-kind validators) for the
// whole test binary, which the in-package tests in config_test.go rely on: they
// Load postgres/restic/borg/mysql/mongodb/exec configs and expect the
// engine-contributed validation to run, exactly as the CLI binary does via
// internal/engine's blank imports.
package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salvage.sh/internal/config"

	_ "salvage.sh/internal/engine/borg"
	_ "salvage.sh/internal/engine/exec"
	_ "salvage.sh/internal/engine/mongodb"
	_ "salvage.sh/internal/engine/mysql"
	_ "salvage.sh/internal/engine/postgres"
	_ "salvage.sh/internal/engine/restic"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// An unregistered target.type is rejected at load, and the error names both the
// bad type and the registered alternatives.
func TestUnknownTargetTypeRejected(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "bad.yaml", "target:\n  name: x\n  type: sqlite\n  source:\n    kind: sqlite\n")
	_, err := config.Load(p)
	if err == nil {
		t.Fatal("expected error for an unregistered target.type")
	}
	for _, want := range []string{"sqlite", "unsupported", "postgres", "mongodb"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got %q", want, err)
		}
	}
}

// init plays the role of a hypothetical new engine's init(): it registers a
// target.type and a check kind purely from "its own package" — the
// additive-extension promise (spec 0016 R6) that TestAdditiveEngineExtension
// exercises, with no edit to config.Validate's core path.
func init() {
	config.RegisterTargetValidator("faketype", func(c *config.Config) error {
		if c.Target.Source.Path == "" {
			return fmt.Errorf("target.source.path is required for target.type faketype")
		}
		return nil
	})
	config.RegisterCheckValidator("fake_kind", func(targetType string, i int, ck config.Check) error {
		if targetType != "faketype" {
			return fmt.Errorf("checks[%d] (%s): kind \"fake_kind\" is only valid for target.type faketype", i, ck.Name)
		}
		if ck.Path == "" {
			return fmt.Errorf("checks[%d] (%s): path is required for kind fake_kind", i, ck.Name)
		}
		return nil
	})
}

// The additive-extension promise (spec 0016 R6): a hypothetical new engine
// makes its target.type and check kind valid purely by registering validators
// from its own package (see init above) — no edit to config.Validate's core
// path.
func TestAdditiveEngineExtension(t *testing.T) {
	dir := t.TempDir()
	good := write(t, dir, "fake.yaml", `
target:
  name: f
  type: faketype
  source:
    kind: faketype
    path: /anything
  checks:
    - name: c
      kind: fake_kind
      path: some/path
`)
	if _, err := config.Load(good); err != nil {
		t.Fatalf("registered engine type + kind should load: %v", err)
	}

	// The engine-contributed target validator runs and its error propagates.
	badSource := write(t, dir, "fake-nosource.yaml", "target:\n  name: f\n  type: faketype\n  source:\n    kind: faketype\n")
	if _, err := config.Load(badSource); err == nil || !strings.Contains(err.Error(), "faketype") {
		t.Fatalf("expected the registered target validator's error, got %v", err)
	}

	// The engine-contributed check validator runs and its error propagates.
	badCheck := write(t, dir, "fake-nopath.yaml", `
target:
  name: f
  type: faketype
  source:
    kind: faketype
    path: /anything
  checks:
    - name: c
      kind: fake_kind
`)
	if _, err := config.Load(badCheck); err == nil || !strings.Contains(err.Error(), "fake_kind") {
		t.Fatalf("expected the registered check validator's error, got %v", err)
	}

	// A registered kind is still gated to its engine.
	wrongEngine := write(t, dir, "fake-wrongengine.yaml", `
target:
  name: e
  type: exec
  restore:
    command: ["/x"]
  checks:
    - name: c
      kind: fake_kind
      path: p
`)
	if _, err := config.Load(wrongEngine); err == nil {
		t.Fatal("expected error for fake_kind on a non-faketype target")
	}
}

// A doc_query check accepts max_age as its expectation (freshness beyond the
// sql kind, backlog S3): a config asserting a timestamp field is no older than
// a window loads cleanly, and doc_query still rejects a check with no
// expectation at all.
func TestDocQueryMaxAgeValidates(t *testing.T) {
	dir := t.TempDir()
	archive := write(t, dir, "dump.archive", "-- archive")

	good := write(t, dir, "mongodb.yaml", fmt.Sprintf(`
target:
  name: m
  type: mongodb
  source:
    kind: mongodb
    path: %q
  checks:
    - name: latest_order_recent
      kind: doc_query
      collection: orders
      filter: '{"_id":"latest"}'
      field: created_at
      max_age: 48h
`, archive))
	c, err := config.Load(good)
	if err != nil {
		t.Fatalf("doc_query with max_age should load: %v", err)
	}
	if c.Target.Checks[0].MaxAge == nil {
		t.Fatal("max_age did not parse onto the doc_query check")
	}

	noExpect := write(t, dir, "mongodb-noexpect.yaml", fmt.Sprintf("target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: %q\n  checks:\n    - name: a\n      kind: doc_query\n      collection: orders\n      filter: '{\"_id\":1}'\n      field: status\n", archive))
	if _, err := config.Load(noExpect); err == nil {
		t.Fatal("expected error for doc_query with no expectation")
	} else if !strings.Contains(err.Error(), "max_age") {
		t.Errorf("the no-expectation error should list max_age as an option; got %v", err)
	}
}
