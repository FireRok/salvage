package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndParse(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	cfgPath := writeFile(t, dir, "salvage.yaml", fmt.Sprintf(`
target:
  name: t1
  source:
    kind: sql
    path: %q
  checks:
    - name: c1
      sql: "select 1"
      expect_min: 1
    - name: fresh
      sql: "select now()"
      max_age: 24h
report:
  out: out.json
`, dump))

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Type != "postgres" {
		t.Errorf("type default = %q, want postgres", c.Target.Type)
	}
	if c.Target.Restore.Image != "postgres:16" {
		t.Errorf("image default = %q", c.Target.Restore.Image)
	}
	if c.Target.Restore.Database != "salvage_restore_test" {
		t.Errorf("database default = %q", c.Target.Restore.Database)
	}
	if c.Target.Restore.Timeout.Std() != 10*time.Minute {
		t.Errorf("timeout default = %v", c.Target.Restore.Timeout.Std())
	}
	if len(c.Target.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(c.Target.Checks))
	}
	if c.Target.Checks[1].MaxAge == nil || c.Target.Checks[1].MaxAge.Std() != 24*time.Hour {
		t.Errorf("max_age did not parse: %+v", c.Target.Checks[1].MaxAge)
	}
}

func TestBoolAndSeverityParseAndDefault(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	cfgPath := writeFile(t, dir, "salvage.yaml", fmt.Sprintf(`
target:
  name: t1
  source:
    kind: sql
    path: %q
  checks:
    - name: no-orphans
      sql: "select count(*) = 0 from orphaned"
      bool: true
      severity: advisory
    - name: rowcount
      sql: "select count(*) from t"
      expect_min: 1
`, dump))

	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Checks[0].Bool == nil || *c.Target.Checks[0].Bool != true {
		t.Errorf("bool did not parse: %+v", c.Target.Checks[0].Bool)
	}
	if c.Target.Checks[0].Severity != "advisory" {
		t.Errorf("severity = %q, want advisory", c.Target.Checks[0].Severity)
	}
	// Default severity is "required" so existing configs are unchanged.
	if c.Target.Checks[1].Severity != "required" {
		t.Errorf("default severity = %q, want required", c.Target.Checks[1].Severity)
	}
}

func TestRejectsMultipleExpectations(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	cfgPath := writeFile(t, dir, "salvage.yaml", fmt.Sprintf(`
target:
  name: t1
  source:
    kind: sql
    path: %q
  checks:
    - name: two
      sql: "select 1"
      bool: true
      expect_min: 1
`, dump))
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for a check with two expectations (bool + expect_min)")
	}
}

func TestRejectsBadSeverity(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	cfgPath := writeFile(t, dir, "salvage.yaml", fmt.Sprintf(`
target:
  name: t1
  source:
    kind: sql
    path: %q
  checks:
    - name: c
      sql: "select 1"
      expect_min: 1
      severity: critical
`, dump))
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for an unsupported severity")
	}
}

func TestValidateRejectsMissingKind(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "bad.yaml", "target:\n  name: x\n  source:\n    path: /nope\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for missing source.kind")
	}
}

func TestPgBackRestValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "pgbr.yaml", `
target:
  name: p
  source:
    kind: pgbackrest
    stanza: demo
    repo_volume: salvage-pgbr-repo
  restore:
    image: salvage-pg-pgbackrest:16
  checks:
    - name: c
      sql: "select 1"
      expect_min: 1
`)
	c, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Source.RepoPath != "/var/lib/pgbackrest" {
		t.Errorf("repo_path default = %q", c.Target.Source.RepoPath)
	}
	if c.Target.Restore.Database != "postgres" {
		t.Errorf("database default = %q, want postgres", c.Target.Restore.Database)
	}
	if c.Target.Restore.User != "postgres" {
		t.Errorf("user default = %q, want postgres", c.Target.Restore.User)
	}

	// repo_volume is now optional (remote repos rely on the image config); the
	// stanza is still required, so omitting it must error.
	bad := writeFile(t, dir, "bad.yaml", "target:\n  name: p\n  source:\n    kind: pgbackrest\n    repo_volume: v\n  restore:\n    image: x\n")
	if _, err := Load(bad); err == nil {
		t.Fatal("expected error for missing stanza")
	}
}
