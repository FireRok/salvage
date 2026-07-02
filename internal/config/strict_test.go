package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Strict decoding: a misspelled key is a load error that names the key, not a
// silently dropped field. The misspellings below are the exact failure modes
// the strict decoder exists to catch: a dropped expectation (expct_min) or a
// dropped secret-forwarding list (pass_evn) would silently weaken the test.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")

	cases := map[string]string{
		"expct_min": `
target:
  name: t1
  source:
    kind: sql
    path: ` + dump + `
  checks:
    - name: c1
      sql: "select 1"
      expct_min: 1
`,
		"pass_evn": `
target:
  name: t1
  source:
    kind: sql
    path: ` + dump + `
    pass_evn: [PGPASSWORD]
  checks:
    - name: c1
      sql: "select 1"
      expect_min: 1
`,
		"snapshto": `
target:
  name: r
  type: restic
  source:
    kind: restic
    repository: /repo
    snapshto: latest
`,
	}
	for key, body := range cases {
		p := writeFile(t, dir, "bad-"+key+".yaml", body)
		_, err := Load(p)
		if err == nil {
			t.Errorf("%s: expected a load error for the unknown key", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error should name the unknown key; got %q", key, err)
		}
	}
}

// An empty config file is not a strict-decode error; validation reports what is
// missing, matching the pre-strict decoder's behaviour.
func TestLoadEmptyFileFailsValidationNotParse(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "empty.yaml", "")
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected a validation error for an empty config")
	}
	if strings.Contains(err.Error(), "parse config") {
		t.Errorf("an empty file should fail validation, not parsing; got %q", err)
	}
}

// Every shipped example config must survive strict decoding — no example may
// carry a key the schema does not know. Decode-only (not Load): the examples
// reference dump paths and repositories that do not exist on a dev machine.
func TestExampleConfigsDecodeStrictly(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "salvage*.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no example configs found at repo root (salvage*.example.yaml)")
	}
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parse(b); err != nil {
			t.Errorf("%s: strict decode failed: %v", filepath.Base(p), err)
		}
	}
}
