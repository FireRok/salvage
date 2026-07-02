package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// A check with no kind parses with an empty Kind (meaning "sql"), and an
// explicit kind: sql parses verbatim — both unchanged from historical behaviour.
func TestCheckKindDefaultsAndParses(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	cfgPath := writeFile(t, dir, "salvage.yaml", fmt.Sprintf(`
target:
  name: t1
  source:
    kind: sql
    path: %q
  checks:
    - name: implicit
      sql: "select 1"
      expect_min: 1
    - name: explicit
      kind: sql
      sql: "select 1"
      expect_min: 1
`, dump))
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// An unset kind stays "" (Run defaults it to sql); existing configs unchanged.
	if c.Target.Checks[0].Kind != "" {
		t.Errorf("implicit check kind = %q, want empty", c.Target.Checks[0].Kind)
	}
	if c.Target.Checks[1].Kind != "sql" {
		t.Errorf("explicit check kind = %q, want sql", c.Target.Checks[1].Kind)
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

// A restic config parses, defaults (image + snapshot), and validates its
// non-SQL check kinds — the spec 0018 allow-list step.
func TestResticValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "restic.yaml", `
target:
  name: r
  type: restic
  source:
    kind: restic
    repository: /repo
    repo_volume: salvage-restic-repo
  checks:
    - name: config_present
      kind: file_exists
      path: etc/app.conf
    - name: data_files
      kind: file_count
      path: "data/*.csv"
      expect_min: 2
    - name: seed_checksum
      kind: checksum
      path: data/seed.csv
      equals: "deadbeef"
    - name: readable
      kind: command
      command: "cat etc/app.conf > /dev/null"
`)
	c, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Type != "restic" {
		t.Errorf("type = %q, want restic", c.Target.Type)
	}
	if c.Target.Restore.Image != "restic/restic:0.19.0" {
		t.Errorf("image default = %q, want restic/restic:0.19.0 (pinned, backlog S10)", c.Target.Restore.Image)
	}
	if c.Target.Source.Snapshot != "latest" {
		t.Errorf("snapshot default = %q, want latest", c.Target.Source.Snapshot)
	}
	if len(c.Target.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(c.Target.Checks))
	}

	// A repository supplied by reference (RESTIC_REPOSITORY in pass_env) is fine.
	byRef := writeFile(t, dir, "restic-ref.yaml", `
target:
  name: r
  type: restic
  source:
    kind: restic
    pass_env: [RESTIC_REPOSITORY, RESTIC_PASSWORD]
  checks:
    - name: present
      kind: file_exists
      path: a
`)
	if _, err := Load(byRef); err != nil {
		t.Fatalf("by-reference repo should validate: %v", err)
	}

	// No repository at all (neither inline nor by reference) must error.
	noRepo := writeFile(t, dir, "restic-norepo.yaml", "target:\n  name: r\n  type: restic\n  source:\n    kind: restic\n  checks:\n    - name: a\n      kind: file_exists\n      path: a\n")
	if _, err := Load(noRepo); err == nil {
		t.Fatal("expected error when neither repository nor RESTIC_REPOSITORY is set")
	}

	// A file_exists check without a path must error.
	noPath := writeFile(t, dir, "restic-nopath.yaml", "target:\n  name: r\n  type: restic\n  source:\n    kind: restic\n    repository: /r\n  checks:\n    - name: a\n      kind: file_exists\n")
	if _, err := Load(noPath); err == nil {
		t.Fatal("expected error for file_exists without path")
	}

	// A restic check kind on a postgres target must error (kinds are engine-scoped).
	wrongEngine := writeFile(t, dir, "wrong.yaml", `
target:
  name: p
  type: postgres
  source:
    kind: pgbackrest
    stanza: s
  restore:
    image: img
  checks:
    - name: a
      kind: file_exists
      path: a
`)
	if _, err := Load(wrongEngine); err == nil {
		t.Fatal("expected error for a restic check kind on a postgres target")
	}
}

// A borg config parses, defaults the image, and validates its non-SQL check
// kinds — the spec 0022 allow-list step (a near-exact sibling of restic).
func TestBorgValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "borg.yaml", `
target:
  name: b
  type: borg
  source:
    kind: borg
    repository: /repo
    archive: nightly-2026
    repo_volume: salvage-borg-repo
    pass_env: [BORG_PASSPHRASE]
  checks:
    - name: config_present
      kind: file_exists
      path: etc/app.conf
    - name: data_files
      kind: file_count
      path: "data/*.csv"
      expect_min: 2
    - name: seed_checksum
      kind: checksum
      path: data/seed.csv
      equals: "deadbeef"
    - name: readable
      kind: command
      command: "cat etc/app.conf > /dev/null"
`)
	c, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Type != "borg" {
		t.Errorf("type = %q, want borg", c.Target.Type)
	}
	if c.Target.Restore.Image != "ghcr.io/borgmatic-collective/borgmatic:2.1.6" {
		t.Errorf("image default = %q, want ghcr.io/borgmatic-collective/borgmatic:2.1.6 (pinned, backlog S10)", c.Target.Restore.Image)
	}
	if c.Target.Source.RepoPath != "/repo" {
		t.Errorf("repo_path default = %q, want /repo", c.Target.Source.RepoPath)
	}
	if len(c.Target.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(c.Target.Checks))
	}

	// A repository supplied by reference (BORG_REPO in pass_env) is fine.
	byRef := writeFile(t, dir, "borg-ref.yaml", `
target:
  name: b
  type: borg
  source:
    kind: borg
    archive: nightly-2026
    pass_env: [BORG_REPO, BORG_PASSPHRASE]
  checks:
    - name: present
      kind: file_exists
      path: a
`)
	if _, err := Load(byRef); err != nil {
		t.Fatalf("by-reference repo should validate: %v", err)
	}

	// No repository at all (neither inline nor by reference) must error.
	noRepo := writeFile(t, dir, "borg-norepo.yaml", "target:\n  name: b\n  type: borg\n  source:\n    kind: borg\n    archive: a\n  checks:\n    - name: a\n      kind: file_exists\n      path: a\n")
	if _, err := Load(noRepo); err == nil {
		t.Fatal("expected error when neither repository nor BORG_REPO is set")
	}

	// A missing archive must error (borg has no "latest" alias).
	noArchive := writeFile(t, dir, "borg-noarchive.yaml", "target:\n  name: b\n  type: borg\n  source:\n    kind: borg\n    repository: /r\n  checks:\n    - name: a\n      kind: file_exists\n      path: a\n")
	if _, err := Load(noArchive); err == nil {
		t.Fatal("expected error when archive is unset")
	}

	// A file_exists check without a path must error.
	noPath := writeFile(t, dir, "borg-nopath.yaml", "target:\n  name: b\n  type: borg\n  source:\n    kind: borg\n    repository: /r\n    archive: a\n  checks:\n    - name: a\n      kind: file_exists\n")
	if _, err := Load(noPath); err == nil {
		t.Fatal("expected error for file_exists without path")
	}

	// The wrong source kind on a borg target must error.
	wrongKind := writeFile(t, dir, "borg-wrongkind.yaml", "target:\n  name: b\n  type: borg\n  source:\n    kind: restic\n    repository: /r\n    archive: a\n  checks:\n    - name: a\n      kind: file_exists\n      path: a\n")
	if _, err := Load(wrongKind); err == nil {
		t.Fatal("expected error for a non-borg source kind on a borg target")
	}
}

// A mysql config parses, defaults the image/database/user, and reuses the sql
// check kind unchanged — the spec 0024 allow-list step. MySQL is a SQL engine
// (Postgres's closest sibling), not a file-probe engine, so its checks use the
// same shape as Postgres's, with no restic/borg-style kind restriction.
func TestMySQLValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")

	good := writeFile(t, dir, "mysql.yaml", fmt.Sprintf(`
target:
  name: m
  type: mysql
  source:
    kind: mysql
    path: %q
  checks:
    - name: orders_not_empty
      sql: "select count(*) from orders"
      expect_min: 1
    - name: latest_order_recent
      sql: "select max(created_at) from orders"
      max_age: 48h
`, dump))
	c, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Type != "mysql" {
		t.Errorf("type = %q, want mysql", c.Target.Type)
	}
	if c.Target.Restore.Image != "mysql:8.4.10" {
		t.Errorf("image default = %q, want mysql:8.4.10 (pinned, backlog S10)", c.Target.Restore.Image)
	}
	if c.Target.Restore.Database != "salvage_restore_test" {
		t.Errorf("database default = %q, want salvage_restore_test", c.Target.Restore.Database)
	}
	if c.Target.Restore.User != "root" {
		t.Errorf("user default = %q, want root", c.Target.Restore.User)
	}
	if len(c.Target.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(c.Target.Checks))
	}

	// A missing source.path must error.
	noPath := writeFile(t, dir, "mysql-nopath.yaml", "target:\n  name: m\n  type: mysql\n  source:\n    kind: mysql\n  checks:\n    - name: a\n      sql: \"select 1\"\n      expect_min: 1\n")
	if _, err := Load(noPath); err == nil {
		t.Fatal("expected error when source.path is unset")
	}

	// A nonexistent dump path must error.
	badPath := writeFile(t, dir, "mysql-badpath.yaml", "target:\n  name: m\n  type: mysql\n  source:\n    kind: mysql\n    path: /nonexistent/dump.sql\n  checks:\n    - name: a\n      sql: \"select 1\"\n      expect_min: 1\n")
	if _, err := Load(badPath); err == nil {
		t.Fatal("expected error for a nonexistent dump path")
	}

	// The wrong source kind on a mysql target must error.
	wrongKind := writeFile(t, dir, "mysql-wrongkind.yaml", fmt.Sprintf("target:\n  name: m\n  type: mysql\n  source:\n    kind: pg_dump\n    path: %q\n  checks:\n    - name: a\n      sql: \"select 1\"\n      expect_min: 1\n", dump))
	if _, err := Load(wrongKind); err == nil {
		t.Fatal("expected error for a non-mysql source kind on a mysql target")
	}

	// An unknown check kind on a mysql target must error (file/command kinds are
	// restic/borg/exec-only; mysql is a SQL engine, not a file-probe engine).
	fileKind := writeFile(t, dir, "mysql-filekind.yaml", fmt.Sprintf("target:\n  name: m\n  type: mysql\n  source:\n    kind: mysql\n    path: %q\n  checks:\n    - name: a\n      kind: file_exists\n      path: a\n", dump))
	if _, err := Load(fileKind); err == nil {
		t.Fatal("expected error for a file_exists check kind on a mysql target")
	}
}

func TestExecValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()

	good := writeFile(t, dir, "exec.yaml", `
target:
  name: byo
  type: exec
  restore:
    command: ["/opt/restore.sh"]
    env: [RESTORE_TARGET, PGHOST]
    workdir: /opt
    timeout: 30m
  checks:
    - name: healthz
      kind: http
      url: http://127.0.0.1:8080/healthz
      expect_status: 200
      expect_body_contains: '"db":"ok"'
    - name: rows
      kind: command
      command: "psql -tAc 'select 1'"
    - name: manifest
      kind: file_exists
      path: /scratch/MANIFEST
    - name: tables
      kind: file_count
      path: "/scratch/tables/*"
      expect_min: 1
`)
	c, err := Load(good)
	if err != nil {
		t.Fatalf("valid exec config should load: %v", err)
	}
	if c.Target.Type != "exec" {
		t.Errorf("type = %q, want exec", c.Target.Type)
	}
	if len(c.Target.Restore.Command) != 1 || c.Target.Restore.Command[0] != "/opt/restore.sh" {
		t.Errorf("command = %v, want [/opt/restore.sh]", c.Target.Restore.Command)
	}
	if len(c.Target.Checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(c.Target.Checks))
	}

	// Missing restore.command must fail at load.
	noCmd := writeFile(t, dir, "exec-nocmd.yaml", `
target:
  name: byo
  type: exec
  checks:
    - name: h
      kind: http
      url: http://x
`)
	if _, err := Load(noCmd); err == nil {
		t.Fatal("expected error when restore.command is missing")
	}

	// An http check without a url must fail.
	noURL := writeFile(t, dir, "exec-nourl.yaml", `
target:
  name: byo
  type: exec
  restore:
    command: ["/x"]
  checks:
    - name: h
      kind: http
`)
	if _, err := Load(noURL); err == nil {
		t.Fatal("expected error for http check without url")
	}

	// An unknown kind must fail at config load, not runtime.
	unknown := writeFile(t, dir, "exec-unknown.yaml", `
target:
  name: byo
  type: exec
  restore:
    command: ["/x"]
  checks:
    - name: h
      kind: bogus
`)
	if _, err := Load(unknown); err == nil {
		t.Fatal("expected error for an unknown check kind")
	}

	// http is capability-gated, not exec-only (backlog S4): a restic target's
	// prober carries HTTP (from the Salvage host), so an http check validates.
	httpOnRestic := writeFile(t, dir, "exec-httprestic.yaml", `
target:
  name: r
  type: restic
  source:
    kind: restic
    repository: /repo
  checks:
    - name: h
      kind: http
      url: http://x
`)
	if _, err := Load(httpOnRestic); err != nil {
		t.Fatalf("an http check on a restic target should validate (backlog S4): %v", err)
	}

	// Where no HTTP-capable prober exists (a SQL engine), http is still
	// rejected at load with a message naming the capable target types.
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	httpOnPostgres := writeFile(t, dir, "exec-httppostgres.yaml", fmt.Sprintf(`
target:
  name: p
  type: postgres
  source:
    kind: pg_dump
    path: %q
  checks:
    - name: h
      kind: http
      url: http://x
`, dump))
	_, err = Load(httpOnPostgres)
	if err == nil {
		t.Fatal("expected error for an http check on a postgres target (no HTTP prober)")
	}
	for _, want := range []string{"HTTP prober", "exec", "restic", "borg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("http rejection should mention %q; got %v", want, err)
		}
	}
}

// A mongodb config parses, defaults the image/database/user, and validates its
// two new check kinds (collection_count/doc_query) — spec 0025's allow-list
// step, mirroring TestMySQLValidationAndDefaults. Unlike MySQL (which reuses
// the sql kind), MongoDB registers its own kinds, so a sql check on a mongodb
// target (and a mongodb-only kind on any other target) must also error.
func TestMongoDBValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	archive := writeFile(t, dir, "dump.archive", "-- archive")

	good := writeFile(t, dir, "mongodb.yaml", fmt.Sprintf(`
target:
  name: m
  type: mongodb
  source:
    kind: mongodb
    path: %q
  checks:
    - name: orders_not_empty
      kind: collection_count
      collection: orders
      expect_min: 1
    - name: latest_order_status
      kind: doc_query
      collection: orders
      filter: '{"_id":"o1"}'
      field: status
      equals: "shipped"
`, archive))
	c, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Target.Type != "mongodb" {
		t.Errorf("type = %q, want mongodb", c.Target.Type)
	}
	if c.Target.Restore.Image != "mongo:7.0.37" {
		t.Errorf("image default = %q, want mongo:7.0.37 (pinned, backlog S10)", c.Target.Restore.Image)
	}
	if c.Target.Restore.Database != "salvage_restore_test" {
		t.Errorf("database default = %q, want salvage_restore_test", c.Target.Restore.Database)
	}
	if c.Target.Restore.User != "root" {
		t.Errorf("user default = %q, want root", c.Target.Restore.User)
	}
	if len(c.Target.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(c.Target.Checks))
	}

	// A missing source.path must error.
	noPath := writeFile(t, dir, "mongodb-nopath.yaml", "target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n  checks:\n    - name: a\n      kind: collection_count\n      collection: orders\n      expect_min: 1\n")
	if _, err := Load(noPath); err == nil {
		t.Fatal("expected error when source.path is unset")
	}

	// A nonexistent archive path must error.
	badPath := writeFile(t, dir, "mongodb-badpath.yaml", "target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: /nonexistent/dump.archive\n  checks:\n    - name: a\n      kind: collection_count\n      collection: orders\n      expect_min: 1\n")
	if _, err := Load(badPath); err == nil {
		t.Fatal("expected error for a nonexistent archive path")
	}

	// The wrong source kind on a mongodb target must error.
	wrongKind := writeFile(t, dir, "mongodb-wrongkind.yaml", fmt.Sprintf("target:\n  name: m\n  type: mongodb\n  source:\n    kind: mysql\n    path: %q\n  checks:\n    - name: a\n      kind: collection_count\n      collection: orders\n      expect_min: 1\n", archive))
	if _, err := Load(wrongKind); err == nil {
		t.Fatal("expected error for a non-mongodb source kind on a mongodb target")
	}

	// collection_count missing collection must error.
	noCollection := writeFile(t, dir, "mongodb-nocollection.yaml", fmt.Sprintf("target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: %q\n  checks:\n    - name: a\n      kind: collection_count\n      expect_min: 1\n", archive))
	if _, err := Load(noCollection); err == nil {
		t.Fatal("expected error for collection_count missing collection")
	}

	// collection_count missing an expectation must error.
	noExpect := writeFile(t, dir, "mongodb-noexpect.yaml", fmt.Sprintf("target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: %q\n  checks:\n    - name: a\n      kind: collection_count\n      collection: orders\n", archive))
	if _, err := Load(noExpect); err == nil {
		t.Fatal("expected error for collection_count missing an expectation")
	}

	// doc_query missing collection/filter/field must each error.
	for _, body := range []string{
		"target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: %q\n  checks:\n    - name: a\n      kind: doc_query\n      filter: '{\"_id\":1}'\n      field: status\n      equals: \"x\"\n",
		"target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: %q\n  checks:\n    - name: a\n      kind: doc_query\n      collection: orders\n      field: status\n      equals: \"x\"\n",
		"target:\n  name: m\n  type: mongodb\n  source:\n    kind: mongodb\n    path: %q\n  checks:\n    - name: a\n      kind: doc_query\n      collection: orders\n      filter: '{\"_id\":1}'\n      equals: \"x\"\n",
	} {
		p := writeFile(t, dir, "mongodb-docquery-missing.yaml", fmt.Sprintf(body, archive))
		if _, err := Load(p); err == nil {
			t.Fatalf("expected error for incomplete doc_query config: %s", body)
		}
	}

	// Note: unlike the file/command kinds (which validateCheck restricts by
	// target type via isFileProbeTarget), the sql kind's validation has no
	// target-type restriction (mirroring how a mysql target's sql checks reuse
	// the exact same unrestricted validation path as Postgres — spec 0024). So a
	// `kind: sql` check on a mongodb target parses/validates fine at config-load
	// time; it only fails at evaluation time, when evalSQL's type-assert to
	// checks.Queryer fails against a MongoDB target that doesn't implement it
	// (covered by TestEvalCollectionCount_WrongTarget/TestEvalDocQuery_WrongTarget
	// in internal/engine/mongodb, the mirror-image case: a collection_count/
	// doc_query check against a non-MongoDB target). That runtime type-assert
	// failure path — not a config-time restriction — is what spec 0025 relies on
	// for "an unsupported kind on a target fails cleanly", matching the existing
	// unknown-kind/type-assert-failure pattern other kinds already use.

	// A collection_count check kind on a non-mongodb target must error.
	collectionOnMySQL := writeFile(t, dir, "mongodb-collectiononmysql.yaml", fmt.Sprintf("target:\n  name: m\n  type: mysql\n  source:\n    kind: mysql\n    path: %q\n  checks:\n    - name: a\n      kind: collection_count\n      collection: orders\n      expect_min: 1\n", archive))
	if _, err := Load(collectionOnMySQL); err == nil {
		t.Fatal("expected error for a collection_count check kind on a mysql target")
	}
}
