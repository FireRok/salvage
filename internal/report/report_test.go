package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFinalizeAdvisoryFailureKeepsPass(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{
		{Name: "ok", OK: true, Severity: "required"},
		{Name: "warn", OK: false, Severity: "advisory"},
	}
	r.Finalize()
	if r.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass (a failing advisory check must not fail the verdict)", r.Verdict)
	}
}

func TestFinalizeRequiredFailureFails(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{
		{Name: "warn", OK: false, Severity: "advisory"},
		{Name: "must", OK: false, Severity: "required"},
	}
	r.Finalize()
	if r.Verdict != "fail" {
		t.Errorf("verdict = %q, want fail (a failing required check must fail the verdict)", r.Verdict)
	}
}

// An empty severity defaults to required behaviour at verdict time.
func TestFinalizeEmptySeverityFailsLikeRequired(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{{Name: "legacy", OK: false}}
	r.Finalize()
	if r.Verdict != "fail" {
		t.Errorf("verdict = %q, want fail (empty severity must behave as required)", r.Verdict)
	}
}

func TestFinalizeFailedRestoreAlwaysFails(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = false
	r.Checks = []CheckResult{{Name: "warn", OK: false, Severity: "advisory"}}
	r.Finalize()
	if r.Verdict != "fail" {
		t.Errorf("verdict = %q, want fail (a failed restore always fails)", r.Verdict)
	}
}

// Spec 0026 R1: New stamps schema_version from the package constant, and it
// serializes into the JSON bytes on every path (WriteJSON is the single
// serializer for file, stdout, and attestation submission).
func TestSchemaVersionStampedAndSerialized(t *testing.T) {
	r := New("t", "test")
	if r.SchemaVersion != SchemaVersion {
		t.Fatalf("New did not stamp SchemaVersion: got %d, want %d", r.SchemaVersion, SchemaVersion)
	}
	if SchemaVersion < 1 {
		t.Fatalf("SchemaVersion = %d, must be a monotonic integer starting at 1", SchemaVersion)
	}
	r.Restore.OK = true
	r.Finalize()

	b, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("report bytes are not valid JSON: %v", err)
	}
	got, present := m["schema_version"]
	if !present {
		t.Fatal(`report JSON is missing "schema_version"`)
	}
	if n, ok := got.(float64); !ok || int(n) != SchemaVersion {
		t.Errorf("schema_version = %v, want %d", got, SchemaVersion)
	}

	// Round-trip: a consumer decoding the bytes back into Report sees the version.
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.SchemaVersion != SchemaVersion {
		t.Errorf("round-trip schema_version = %d, want %d", back.SchemaVersion, SchemaVersion)
	}
}

// Spec 0026 R3: the file at report.out and the returned bytes (which run -json
// writes to stdout, with the same trailing newline) must be byte-identical.
func TestWriteJSONFileMatchesReturnedBytes(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Finalize()

	path := filepath.Join(t.TempDir(), "report.json")
	b, err := r.WriteJSON(path)
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report.out: %v", err)
	}
	if !bytes.Equal(fileBytes, append(b, '\n')) {
		t.Error("report.out bytes differ from the rendered bytes + newline; the -json stdout and the file must be byte-identical")
	}
}

// Spec 0026 R7/acceptance 7: an emitted report satisfies the published schema
// document (report.v1.schema.json, published at schema.salvage.sh/report/v1.json).
// Stdlib-only contract test: every property the schema marks required is
// present in the emitted JSON, and the schema's pinned schema_version const
// matches the package constant.
func TestEmittedReportSatisfiesPublishedSchema(t *testing.T) {
	sb, err := os.ReadFile("report.v1.schema.json")
	if err != nil {
		t.Fatalf("read published schema: %v", err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Const any `json:"const"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(sb, &schema); err != nil {
		t.Fatalf("published schema is not valid JSON: %v", err)
	}
	if len(schema.Required) == 0 {
		t.Fatal("published schema declares no required properties")
	}
	if c, ok := schema.Properties["schema_version"]; !ok {
		t.Error("published schema does not pin schema_version")
	} else if n, isNum := c.Const.(float64); !isNum || int(n) != SchemaVersion {
		t.Errorf("published schema pins schema_version const = %v, package constant is %d — bumping the constant requires publishing a new schema document", c.Const, SchemaVersion)
	}

	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{{Name: "c", OK: true, Severity: "required"}}
	r.Finalize()
	b, err := r.WriteJSON("")
	if err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("report bytes are not valid JSON: %v", err)
	}
	for _, req := range schema.Required {
		if _, ok := m[req]; !ok {
			t.Errorf("emitted report is missing required property %q from the published schema", req)
		}
	}
	// And the reverse: the report emits no top-level field the schema doesn't know.
	for k := range m {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("emitted report carries top-level field %q not described by the published schema — publish a new schema version, never mutate v1", k)
		}
	}
}
