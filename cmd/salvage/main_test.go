package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"salvage.sh/internal/attest"
	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// Backlog S2: fleet exits non-zero when any surveyed stanza is degraded or
// empty; exit 0 only when all are healthy and non-empty.
func TestFleetDegraded(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		fl   *report.Fleet
		want bool
	}{
		{
			name: "all healthy and non-empty",
			fl: &report.Fleet{Stanzas: []report.StanzaSummary{
				{Name: "a", Status: "ok", BackupCount: 3, NewestBackup: &now},
				{Name: "b", Status: "ok", BackupCount: 1, NewestBackup: &now},
			}},
			want: false,
		},
		{
			name: "one degraded stanza",
			fl: &report.Fleet{Stanzas: []report.StanzaSummary{
				{Name: "a", Status: "ok", BackupCount: 3},
				{Name: "b", Status: "error (missing stanza data)", BackupCount: 2},
			}},
			want: true,
		},
		{
			name: "one empty stanza",
			fl: &report.Fleet{Stanzas: []report.StanzaSummary{
				{Name: "a", Status: "ok", BackupCount: 0},
			}},
			want: true,
		},
		{
			name: "no stanzas at all",
			fl:   &report.Fleet{},
			want: true,
		},
		// Spec 0029 R5: the same contract for a filesystem engine's single-unit
		// survey — a restic/borg repo folds to one unit whose status is "ok",
		// "empty", or an error string.
		{
			name: "healthy filesystem repository (one unit)",
			fl: &report.Fleet{Stanzas: []report.StanzaSummary{
				{Name: "/srv/repo", Status: "ok", BackupCount: 12, NewestLabel: "cccc1111", NewestBackup: &now},
			}},
			want: false,
		},
		{
			name: "empty filesystem repository",
			fl: &report.Fleet{Stanzas: []report.StanzaSummary{
				{Name: "/srv/repo", Status: "empty", BackupCount: 0},
			}},
			want: true,
		},
		{
			name: "unreachable filesystem repository",
			fl: &report.Fleet{Stanzas: []report.StanzaSummary{
				{Name: "/srv/repo", Status: "Fatal: wrong password or no key found", BackupCount: 0},
			}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fleetDegraded(tc.fl); got != tc.want {
				t.Errorf("fleetDegraded = %v, want %v", got, tc.want)
			}
		})
	}
}

// Spec 0026 R4: the verify -json object carries id, target, verdict, seq,
// key_id, the per-check transcript, a validity boolean, and schema_version.
func TestVerifyVerdictJSONContract(t *testing.T) {
	rec := &attest.Record{
		ID:      "att_123",
		Target:  "orders-db",
		Verdict: "pass",
		Seq:     42,
		KeyID:   "frk1",
		Notice:  "note",
	}
	checks := []attest.Check{
		{Name: "chain hash", OK: true, Detail: "entry_hash matches the signed fields"},
		{Name: "firerok signature", OK: false, Detail: "Firerok signature INVALID"},
	}

	b, err := json.MarshalIndent(report.NewVerifyVerdict(rec, checks, false), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("verify -json output is not valid JSON: %v", err)
	}

	for _, k := range []string{"schema_version", "id", "target", "verdict", "seq", "key_id", "valid", "checks"} {
		if _, ok := m[k]; !ok {
			t.Errorf("verify -json object is missing required key %q", k)
		}
	}
	if n, ok := m["schema_version"].(float64); !ok || int(n) != report.SchemaVersion {
		t.Errorf("schema_version = %v, want %d", m["schema_version"], report.SchemaVersion)
	}
	if m["id"] != "att_123" || m["target"] != "orders-db" || m["verdict"] != "pass" || m["key_id"] != "frk1" {
		t.Errorf("scalar fields did not carry through: %v", m)
	}
	if n, ok := m["seq"].(float64); !ok || int64(n) != 42 {
		t.Errorf("seq = %v, want 42", m["seq"])
	}
	if valid, ok := m["valid"].(bool); !ok || valid {
		t.Errorf("valid = %v, want false", m["valid"])
	}
	cs, ok := m["checks"].([]any)
	if !ok || len(cs) != 2 {
		t.Fatalf("checks = %v, want the 2-entry transcript", m["checks"])
	}
	first, ok := cs[0].(map[string]any)
	if !ok || first["name"] != "chain hash" || first["ok"] != true {
		t.Errorf("check transcript entry did not carry through: %v", cs[0])
	}
}

// Spec 0030 R1: on_fail fires on a fail verdict or an operational error,
// on_success on a pass — each hook receiving the run's exact report bytes —
// and a run with no alerts block fires nothing. Spec 0030 R2 (a hook failure
// never changes the exit code) holds by construction: fireAlertHook returns
// nothing and the callers decide the exit solely from the run outcome.
func TestFireAlertHookSelection(t *testing.T) {
	repFor := func(verdict string) *report.Report {
		r := report.New("t1", "test")
		r.Verdict = verdict
		return r
	}
	cases := []struct {
		name     string
		verdict  string
		opErr    bool
		wantHook string // which sentinel file the fired hook writes
	}{
		{"fail verdict fires on_fail", "fail", false, "fail"},
		{"operational error fires on_fail", "pass", true, "fail"},
		{"pass fires on_success", "pass", false, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			failOut := filepath.Join(dir, "fail")
			okOut := filepath.Join(dir, "success")
			cfg := &config.Config{Alerts: &config.Alerts{
				OnFail:    `cat > "` + failOut + `"`,
				OnSuccess: `cat > "` + okOut + `"`,
			}}
			payload := []byte(`{"schema_version":1}`)
			fireAlertHook(cfg, repFor(tc.verdict), payload, "", tc.opErr)

			want, other := failOut, okOut
			if tc.wantHook == "success" {
				want, other = okOut, failOut
			}
			body, err := os.ReadFile(want)
			if err != nil {
				t.Fatalf("expected the %s hook to fire: %v", tc.wantHook, err)
			}
			if string(body) != string(payload) {
				t.Errorf("hook stdin = %s, want the exact report bytes", body)
			}
			if _, err := os.Stat(other); err == nil {
				t.Errorf("the other hook fired too")
			}
		})
	}
}

// A config without an alerts block, or without the matching hook, fires
// nothing — and a failing hook is only logged, never fatal (spec 0030 R2).
func TestFireAlertHookAbsentOrFailingIsQuiet(t *testing.T) {
	rep := report.New("t1", "test")
	rep.Verdict = "fail"

	// No alerts block at all.
	fireAlertHook(&config.Config{}, rep, []byte(`{}`), "", false)

	// on_success configured but the run failed: nothing to fire.
	dir := t.TempDir()
	okOut := filepath.Join(dir, "success")
	cfg := &config.Config{Alerts: &config.Alerts{OnSuccess: `cat > "` + okOut + `"`}}
	fireAlertHook(cfg, rep, []byte(`{}`), "", false)
	if _, err := os.Stat(okOut); err == nil {
		t.Error("on_success fired for a fail verdict")
	}

	// A hook that exits non-zero must not panic or exit the process.
	cfg = &config.Config{Alerts: &config.Alerts{OnFail: "exit 3"}}
	fireAlertHook(cfg, rep, []byte(`{}`), "", false)
}

// Spec 0026 R4: an empty transcript still serializes as [], keeping the object
// shape stable for consumers.
func TestVerifyVerdictEmptyChecksIsArray(t *testing.T) {
	v := report.NewVerifyVerdict(&attest.Record{ID: "x"}, nil, true)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(m["checks"]) != "[]" {
		t.Errorf("checks = %s, want [] (never null)", m["checks"])
	}
}
