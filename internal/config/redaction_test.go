package config

import (
	"fmt"
	"strings"
	"testing"
)

// Spec 0027 R2/R5: keep_literal is the explicit opt-in to store a check's
// exact got literal; it parses, and it is rejected without the equals
// expectation that justifies it.
func TestKeepLiteralParsesAndRequiresEquals(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")

	ok := writeFile(t, dir, "ok.yaml", fmt.Sprintf(`
target:
  name: t
  source:
    kind: sql
    path: %q
  checks:
    - name: exact
      sql: select banner from meta
      equals: "release 4.2"
      keep_literal: true
`, dump))
	c, err := Load(ok)
	if err != nil {
		t.Fatalf("keep_literal with equals should load: %v", err)
	}
	if !c.Target.Checks[0].KeepLiteral {
		t.Error("keep_literal did not parse")
	}

	bad := writeFile(t, dir, "bad.yaml", fmt.Sprintf(`
target:
  name: t
  source:
    kind: sql
    path: %q
  checks:
    - name: no-equals
      sql: select count(*) from x
      expect_min: 1
      keep_literal: true
`, dump))
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "keep_literal") {
		t.Errorf("keep_literal without equals should fail with a keep_literal error, got %v", err)
	}
}

// Spec 0027 R7: attest.secret_scan accepts its three modes (and empty =
// default refuse) and rejects anything else at load.
func TestAttestSecretScanValidation(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "dump.sql", "-- dump")
	tmpl := `
target:
  name: t
  source:
    kind: sql
    path: %q
  checks:
    - name: c
      sql: select 1
      expect_min: 1
attest:
  endpoint: https://attest.example
  secret_scan: %q
`
	for _, mode := range []string{"refuse", "warn", "off"} {
		p := writeFile(t, dir, "scan-"+mode+".yaml", fmt.Sprintf(tmpl, dump, mode))
		if _, err := Load(p); err != nil {
			t.Errorf("secret_scan %q should be valid: %v", mode, err)
		}
	}
	p := writeFile(t, dir, "scan-bad.yaml", fmt.Sprintf(tmpl, dump, "loud"))
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "secret_scan") {
		t.Errorf("secret_scan \"loud\" should fail with a secret_scan error, got %v", err)
	}
}

// Spec 0027 R3: SecretEnvNames is the full by-reference credential name set —
// source.pass_env plus the exec engine's restore.env.
func TestSecretEnvNames(t *testing.T) {
	c := &Config{}
	c.Target.Source.PassEnv = []string{"RESTIC_PASSWORD", "AWS_SECRET_ACCESS_KEY"}
	c.Target.Restore.Env = []string{"PGPASSWORD"}
	got := c.SecretEnvNames()
	want := []string{"RESTIC_PASSWORD", "AWS_SECRET_ACCESS_KEY", "PGPASSWORD"}
	if len(got) != len(want) {
		t.Fatalf("SecretEnvNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SecretEnvNames = %v, want %v", got, want)
		}
	}
}
