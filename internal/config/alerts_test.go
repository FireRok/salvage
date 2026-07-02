package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// loadAlertsConfig writes and loads a minimal valid config carrying the given
// alerts block (spec 0030).
func loadAlertsConfig(t *testing.T, alertsYAML string) (*Config, error) {
	t.Helper()
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
%s`, dump, alertsYAML))
	return Load(cfgPath)
}

// Spec 0030 R1: the alerts block parses under strict decoding, with both
// hooks and the bounded timeout.
func TestAlertsBlockParses(t *testing.T) {
	c, err := loadAlertsConfig(t, `
alerts:
  on_fail: "./notify.sh"
  on_success: "https://hooks.example/salvage?token_ref=env:SALVAGE_HOOK_TOKEN"
  timeout: 10s
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Alerts == nil {
		t.Fatal("alerts block did not parse")
	}
	if c.Alerts.OnFail != "./notify.sh" {
		t.Errorf("on_fail = %q", c.Alerts.OnFail)
	}
	if !strings.Contains(c.Alerts.OnSuccess, "token_ref=env:SALVAGE_HOOK_TOKEN") {
		t.Errorf("on_success = %q", c.Alerts.OnSuccess)
	}
	if c.Alerts.Timeout.Std() != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", c.Alerts.Timeout.Std())
	}
}

// An alerts block with no hooks configured can only be a mistake.
func TestAlertsBlockEmptyIsError(t *testing.T) {
	_, err := loadAlertsConfig(t, "alerts: {}\n")
	if err == nil || !strings.Contains(err.Error(), "on_fail/on_success") {
		t.Fatalf("expected an empty-alerts error, got %v", err)
	}
}

// Spec 0030 R7: a URL hook must reference its token, never embed it — an
// embedded user:pass@ or a literal *_ref value fails at load.
func TestAlertsURLSecretsRejectedAtLoad(t *testing.T) {
	_, err := loadAlertsConfig(t, `
alerts:
  on_fail: "https://user:hunter2@hooks.example/salvage"
`)
	if err == nil || !strings.Contains(err.Error(), "alerts.on_fail") {
		t.Fatalf("expected an embedded-credentials error, got %v", err)
	}

	_, err = loadAlertsConfig(t, `
alerts:
  on_success: "https://hooks.example/salvage?token_ref=literal-secret"
`)
	if err == nil || !strings.Contains(err.Error(), "env reference") {
		t.Fatalf("expected a literal-ref error, got %v", err)
	}
}

// Strict decoding (spec 0026 lineage): a misspelled alerts key fails at load.
func TestAlertsUnknownKeyIsError(t *testing.T) {
	_, err := loadAlertsConfig(t, `
alerts:
  on_fial: "./notify.sh"
`)
	if err == nil || !strings.Contains(err.Error(), "on_fial") {
		t.Fatalf("expected a strict-decode error naming on_fial, got %v", err)
	}
}

// Spec 0030 R7 + spec 0027 R3: env vars referenced by a URL hook join the
// known-secret set so a hook token can never appear in a report.
func TestSecretEnvNamesIncludeHookRefs(t *testing.T) {
	c, err := loadAlertsConfig(t, `
alerts:
  on_fail: "https://hooks.example/f?token_ref=env:FAIL_TOK"
  on_success: "https://hooks.example/s?token_ref=env:OK_TOK"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := c.SecretEnvNames()
	got := strings.Join(names, ",")
	for _, want := range []string{"FAIL_TOK", "OK_TOK"} {
		if !strings.Contains(got, want) {
			t.Errorf("SecretEnvNames = %v, missing %s", names, want)
		}
	}
}
