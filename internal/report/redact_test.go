package report

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// plantedSecret is the credential every leak test plants in captured output.
// Realistic enough to matter, unique enough that a hit in the serialized bytes
// is unambiguous.
const plantedSecret = "s3cr3t-hunter2-XYZZY"

func lookupPlanted(name string) string {
	switch name {
	case "PGBACKREST_REPO1_S3_KEY_SECRET":
		return plantedSecret
	case "EMPTY_VAR":
		return ""
	case "SHORT_VAR":
		return "ab" // below minSecretLen: must be skipped, never scrubbed
	}
	return ""
}

// newLeakyReport builds a report whose every captured-output surface carries
// the planted secret: restore warnings + error, and a check's got/detail/error.
func newLeakyReport() *Report {
	r := New("t", "test")
	r.Restore.OK = true
	r.Restore.Warnings = "restoring…\nconnection postgres://app:" + plantedSecret + "@db:5432/prod\ndone"
	r.Restore.Error = "curl -u admin:" + plantedSecret + " failed"
	r.Checks = []CheckResult{{
		Name:   "echoes",
		OK:     false,
		Got:    "token=" + plantedSecret + "\nsecond line",
		Detail: "stdout was token=" + plantedSecret,
		Error:  "command printed " + plantedSecret,
	}}
	r.Finalize()
	r.SetKnownSecrets(KnownSecretsFromEnv(lookupPlanted,
		[]string{"PGBACKREST_REPO1_S3_KEY_SECRET", "EMPTY_VAR", "SHORT_VAR"}))
	return r
}

// Spec 0027 acceptance 2 (the load-bearing one): a secret planted in restore
// combined output and in a check's Got never appears in the serialized report
// bytes — the exact bytes handed to report.out and attest.Submit.
func TestPlantedSecretNeverInSerializedBytes(t *testing.T) {
	r := newLeakyReport()
	b, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if bytes.Contains(b, []byte(plantedSecret)) {
		t.Fatalf("serialized report bytes contain the planted secret:\n%s", b)
	}
	if !bytes.Contains(b, []byte("[REDACTED:PGBACKREST_REPO1_S3_KEY_SECRET]")) {
		t.Errorf("expected a [REDACTED:<name>] marker in the bytes:\n%s", b)
	}
}

// R1/R8: the restore tail is redacted on failure paths too — timeout and
// non-zero exit are the paths most likely to echo a credential.
func TestPlantedSecretRedactedOnRestoreFailurePaths(t *testing.T) {
	for _, tc := range []struct{ name, errMsg string }{
		{"non-zero exit", "restore command exited non-zero: psql: password " + plantedSecret + " rejected"},
		{"timeout", "restore command timed out after 1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New("t", "test")
			r.Restore.OK = false
			r.Restore.Error = tc.errMsg
			r.Restore.Warnings = "attempting restore with " + plantedSecret + "\n" + strings.Repeat("log line\n", 200)
			r.Finalize()
			r.SetKnownSecrets([]Secret{{Name: "PASS", Value: plantedSecret}})
			b, err := r.WriteJSON("")
			if err != nil {
				t.Fatalf("WriteJSON: %v", err)
			}
			if bytes.Contains(b, []byte(plantedSecret)) {
				t.Fatalf("failure-path report bytes contain the planted secret:\n%s", b)
			}
		})
	}
}

// R1/acceptance 3: Warnings is stored as a bounded preview + sha256
// fingerprint, never the raw multiline stream.
func TestWarningsBoundedPreviewPlusHash(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Restore.Warnings = strings.Repeat("x", 300) + "\nline two\nline three"
	r.Finalize()
	r.Redact()

	w := r.Restore.Warnings
	if strings.ContainsAny(w, "\n\r") {
		t.Errorf("redacted warnings still multiline: %q", w)
	}
	if !regexp.MustCompile(`\[sha256:[0-9a-f]{64}\]$`).MatchString(w) {
		t.Errorf("redacted warnings missing sha256 fingerprint: %q", w)
	}
	if len(w) > previewMax+len(" […] [sha256:]")+64+8 {
		t.Errorf("redacted warnings not bounded (len %d): %q", len(w), w)
	}
	if strings.Contains(w, "line two") {
		t.Errorf("redacted warnings kept content beyond the first line: %q", w)
	}
}

// R8: the transform is idempotent — redacting an already-redacted report is a
// no-op, and the serialized bytes are stable across repeated WriteJSON calls.
func TestRedactIdempotent(t *testing.T) {
	r := newLeakyReport()
	raw1 := r.Redact()
	if raw1.Empty() {
		t.Fatal("first Redact should report replaced raw output")
	}
	b1, err := r.WriteJSON("") // WriteJSON redacts again internally
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	raw2 := r.Redact()
	if !raw2.Empty() {
		t.Errorf("second Redact changed an already-redacted report: %+v", raw2)
	}
	b2, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Error("serialized bytes changed across repeated redaction")
	}
}

// R2: scalar Got values (digests, counts, booleans, exit codes) and short
// asserted literals are preserved byte-identically.
func TestScalarGotPreserved(t *testing.T) {
	scalars := []string{
		"42", "true", "t", "exit 0",
		strings.Repeat("ab12", 16), // a 64-hex checksum digest
		"2026-07-01 10:00:00",
	}
	r := New("t", "test")
	r.Restore.OK = true
	for _, s := range scalars {
		r.Checks = append(r.Checks, CheckResult{Name: s, OK: true, Got: s})
	}
	r.Finalize()
	r.Redact()
	for i, s := range scalars {
		if r.Checks[i].Got != s {
			t.Errorf("scalar Got %q changed to %q", s, r.Checks[i].Got)
		}
	}
}

// R2: free-text Got (multiline or oversized program output) is reduced to a
// bounded scrubbed preview + fingerprint.
func TestFreeTextGotBounded(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{
		{Name: "multiline", Got: "first\nsecond\nthird"},
		{Name: "oversized", Got: strings.Repeat("z", gotInlineMax+1)},
	}
	r.Finalize()
	r.Redact()
	for _, c := range r.Checks {
		if strings.ContainsAny(c.Got, "\n\r") {
			t.Errorf("%s: redacted Got still multiline: %q", c.Name, c.Got)
		}
		if !strings.Contains(c.Got, "[sha256:") {
			t.Errorf("%s: redacted Got missing fingerprint: %q", c.Name, c.Got)
		}
	}
	if !strings.HasPrefix(r.Checks[0].Got, "first") {
		t.Errorf("multiline preview should keep the first line: %q", r.Checks[0].Got)
	}
}

// R2/R3/R5: keep_literal retains the exact (multiline) literal but never
// bypasses known-secret scrubbing.
func TestKeepLiteralRetainsButStillScrubs(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{{
		Name:        "literal",
		Got:         "header\nvalue=" + plantedSecret + "\nfooter",
		KeepLiteral: true,
	}}
	r.Finalize()
	r.SetKnownSecrets([]Secret{{Name: "PASS", Value: plantedSecret}})
	r.Redact()
	got := r.Checks[0].Got
	if !strings.Contains(got, "header\n") || !strings.Contains(got, "\nfooter") {
		t.Errorf("keep_literal lost the literal structure: %q", got)
	}
	if strings.Contains(got, plantedSecret) {
		t.Fatalf("keep_literal bypassed known-secret scrubbing: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:PASS]") {
		t.Errorf("expected scrub marker in kept literal: %q", got)
	}
}

// R3: empty/too-short values are skipped; a secret that is a substring of
// another is still fully covered (longest-first replacement).
func TestKnownSecretsFromEnvSkipsUnsafeValues(t *testing.T) {
	secrets := KnownSecretsFromEnv(lookupPlanted, []string{"EMPTY_VAR", "SHORT_VAR", "MISSING", "PGBACKREST_REPO1_S3_KEY_SECRET"})
	if len(secrets) != 1 || secrets[0].Name != "PGBACKREST_REPO1_S3_KEY_SECRET" {
		t.Fatalf("want only the real secret resolved, got %+v", secrets)
	}
	// Overlapping secrets: the longer value must be replaced first.
	sc := scrubber([]Secret{{Name: "A", Value: "hunter2"}, {Name: "B", Value: "hunter2-extended"}})
	out := sc("x hunter2-extended y hunter2 z")
	if strings.Contains(out, "hunter2") {
		t.Errorf("overlapping secrets left a remainder: %q", out)
	}
	if !strings.Contains(out, "[REDACTED:B]") || !strings.Contains(out, "[REDACTED:A]") {
		t.Errorf("expected both markers: %q", out)
	}
}

// R4/acceptance 4: Redact returns the raw captured output for the local-only
// verbose path, and that raw output is absent from the serialized bytes.
func TestRawOutputForVerboseNeverSerialized(t *testing.T) {
	r := newLeakyReport()
	rawWarnings := r.Restore.Warnings
	rawGot := r.Checks[0].Got

	raw := r.Redact()
	if raw.RestoreWarnings != rawWarnings {
		t.Errorf("RawOutput.RestoreWarnings = %q, want the raw tail", raw.RestoreWarnings)
	}
	if len(raw.Checks) != 1 || raw.Checks[0].Got != rawGot {
		t.Errorf("RawOutput.Checks = %+v, want the raw got", raw.Checks)
	}
	var buf bytes.Buffer
	raw.Fprint(&buf)
	if !strings.Contains(buf.String(), plantedSecret) {
		t.Error("verbose stderr output should carry the raw (secret-bearing) text")
	}

	b, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if bytes.Contains(b, []byte(rawWarnings)) || bytes.Contains(b, []byte(rawGot)) {
		t.Error("raw captured output leaked into serialized report bytes")
	}
}

// R6/acceptance 5: redaction adds no serialized field — the emitted JSON's
// top-level and nested key sets are unchanged, so canonicalization, signing,
// and verify see the same shape as before (schema_version 1, additive-only).
func TestRedactionAddsNoSerializedFields(t *testing.T) {
	r := newLeakyReport()
	b, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["secrets"]; ok {
		t.Error("known-secret set leaked into serialized report")
	}
	check := m["checks"].([]any)[0].(map[string]any)
	for k := range check {
		switch k {
		case "name", "ok", "severity", "got", "detail", "error":
		default:
			t.Errorf("unexpected serialized check field %q — report evolution must be additive and schema-synced", k)
		}
	}
}

// R7: the pattern gate flags common credential shapes and stays quiet on a
// clean (redacted) report.
func TestScanForCredentials(t *testing.T) {
	hits := map[string]string{
		"aws-access-key-id": `restore used key AKIAIOSFODNN7EXAMPLE ok`,
		"private-key-pem":   `-----BEGIN RSA PRIVATE KEY-----\nMIIE...`,
		"bearer-token":      `Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9`,
		"url-credentials":   `dialing postgres://app:hunter2pass@db:5432/prod`,
	}
	for pattern, text := range hits {
		matches := ScanForCredentials([]byte(text))
		found := false
		for _, m := range matches {
			if m.Pattern == pattern {
				found = true
				if strings.Contains(m.Pattern, "hunter2") {
					t.Errorf("match must not echo the credential: %+v", m)
				}
			}
		}
		if !found {
			t.Errorf("pattern %q not detected in %q (got %+v)", pattern, text, matches)
		}
	}

	// A redacted report scans clean: markers never trip the gate.
	r := newLeakyReport()
	b, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if matches := ScanForCredentials(b); len(matches) != 0 {
		t.Errorf("redacted report should scan clean, got %+v\n%s", matches, b)
	}
	// A scrubbed URL credential must not re-trip the gate.
	scrubbed := []byte(`postgres://app:[REDACTED:PGPASS]@db:5432/prod`)
	if matches := ScanForCredentials(scrubbed); len(matches) != 0 {
		t.Errorf("scrub marker re-tripped the gate: %+v", matches)
	}
}

// NewScrubber is the boundary-layer export of the known-secret scrub (used by
// the MCP server, spec 0032 R6): named secrets keep the spec 0027
// [REDACTED:<name>] marker, unnamed secrets render as the bare [REDACTED]
// marker, and longer values are still replaced first.
func TestNewScrubberNamedAndUnnamedMarkers(t *testing.T) {
	scrub := NewScrubber([]Secret{
		{Name: "PGPASS", Value: "hunter2-named"},
		{Value: "hunter2-named-and-longer"}, // unnamed, contains the named one
	})
	got := scrub("a hunter2-named b hunter2-named-and-longer c")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("secret value survived the scrub: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:PGPASS]") {
		t.Errorf("named secret should use the named marker: %q", got)
	}
	if !strings.Contains(got, " [REDACTED] ") {
		t.Errorf("unnamed secret should use the bare marker: %q", got)
	}
	// Longest-first: the longer unnamed value must not be left as
	// "[REDACTED:PGPASS]-and-longer".
	if strings.Contains(got, "-and-longer") {
		t.Errorf("longer secret was shredded by a shorter substring match: %q", got)
	}
}
