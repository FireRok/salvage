package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salvage.sh/internal/attest"
	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// --- session harness --------------------------------------------------------

// runSession feeds newline-delimited JSON-RPC messages through Serve and
// returns the decoded responses, exercising the real stdio transport framing.
func runSession(t *testing.T, msgs ...string) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, m := range msgs {
		in.WriteString(m)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := Serve(&in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response is not one JSON object per line: %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

const initMsg = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
const initializedMsg = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

// callMsg builds a tools/call request with id 9.
func callMsg(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// callTool runs a full initialize + tools/call session and returns the call's
// result object.
func callTool(t *testing.T, tool string, args map[string]any) map[string]any {
	t.Helper()
	resps := runSession(t, initMsg, initializedMsg, callMsg(t, tool, args))
	if len(resps) != 2 {
		t.Fatalf("expected 2 responses (initialize + call), got %d", len(resps))
	}
	last := resps[len(resps)-1]
	if last["error"] != nil {
		t.Fatalf("tools/call returned a protocol error: %v", last["error"])
	}
	res, ok := last["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call result missing: %v", last)
	}
	return res
}

// structured extracts structuredContent from a call result.
func structured(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent in %v", res)
	}
	return sc
}

// toolErr asserts the result is a tool error and returns its structured body.
func toolErr(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	if res["isError"] != true {
		t.Fatalf("expected isError=true, got %v", res)
	}
	sc := structured(t, res)
	e, ok := sc["error"].(map[string]any)
	if !ok {
		t.Fatalf("tool error missing structured error body: %v", sc)
	}
	return e
}

// --- hermetic stubs ----------------------------------------------------------

// hermetic detaches the session from this machine: no stored credentials, no
// ambient attest key.
func hermetic(t *testing.T) {
	t.Helper()
	t.Setenv("SALVAGE_ATTEST_KEY", "")
	stubCreds(t, nil)
}

func stubCreds(t *testing.T, c *attest.Credentials) {
	t.Helper()
	old := loadCredentials
	loadCredentials = func() (*attest.Credentials, error) { return c, nil }
	t.Cleanup(func() { loadCredentials = old })
}

func stubRun(t *testing.T, fn func(context.Context, *config.Config) (*report.Report, error)) {
	t.Helper()
	old := runRestore
	runRestore = fn
	t.Cleanup(func() { runRestore = old })
}

func stubPreflight(t *testing.T, err error) {
	t.Helper()
	old := dockerPreflight
	dockerPreflight = func(context.Context) error { return err }
	t.Cleanup(func() { dockerPreflight = old })
}

func stubFetch(t *testing.T, fn func(context.Context, string, string) (*attest.Record, error)) {
	t.Helper()
	old := fetchAttestation
	fetchAttestation = fn
	t.Cleanup(func() { fetchAttestation = old })
}

func stubSubmit(t *testing.T, fn func(context.Context, string, string, []byte, string, string) (*attest.SubmitResponse, error)) {
	t.Helper()
	old := submitReport
	submitReport = fn
	t.Cleanup(func() { submitReport = old })
}

// writeConfig writes a YAML config into a temp dir and returns its path.
func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "salvage.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const execConfig = `
target:
  name: t1
  type: exec
  restore:
    command: ["true"]
`

// passingReport builds a finalized passing report the stubs can return.
func passingReport() *report.Report {
	rep := report.New("t1", "test")
	rep.Restore.OK = true
	rep.Checks = []report.CheckResult{{Name: "rows", OK: true, Detail: "42"}}
	rep.Finalize()
	return rep
}

// --- protocol tests (spec 0032 R1, R3, acceptance 1) -------------------------

func TestInitializeHandshake(t *testing.T) {
	resps := runSession(t, initMsg, initializedMsg)
	if len(resps) != 1 {
		t.Fatalf("expected exactly 1 response (notification gets none), got %d", len(resps))
	}
	res := resps[0]["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion: got %v", res["protocolVersion"])
	}
	si := res["serverInfo"].(map[string]any)
	if si["name"] != "salvage" {
		t.Errorf("serverInfo.name: got %v", si["name"])
	}
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("capabilities.tools not advertised")
	}
}

func TestInitializeUnknownVersionFallsBack(t *testing.T) {
	msg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`
	resps := runSession(t, msg)
	res := resps[0]["result"].(map[string]any)
	if res["protocolVersion"] != latestProtocolVersion {
		t.Errorf("expected fallback to %s, got %v", latestProtocolVersion, res["protocolVersion"])
	}
}

func TestListToolsAdvertisesSetSchemasAndClassifications(t *testing.T) {
	resps := runSession(t, initMsg, initializedMsg,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	res := resps[1]["result"].(map[string]any)
	tools := res["tools"].([]any)

	wantClass := map[string]string{
		"salvage_run":       "restore-executing",
		"salvage_check":     "restore-executing",
		"salvage_inspect":   "read-only",
		"salvage_last_good": "restore-executing",
		"salvage_fleet":     "read-only",
		"salvage_verify":    "read-only",
		"salvage_attest":    "mutating",
		"salvage_scaffold":  "mutating",
	}
	got := map[string]map[string]any{}
	for _, raw := range tools {
		tl := raw.(map[string]any)
		got[tl["name"].(string)] = tl
	}
	for name, class := range wantClass {
		tl, ok := got[name]
		if !ok {
			t.Errorf("tool %s not advertised", name)
			continue
		}
		meta := tl["_meta"].(map[string]any)
		if meta["salvage.sh/classification"] != class {
			t.Errorf("%s classification: want %q got %v", name, class, meta["salvage.sh/classification"])
		}
		ann := tl["annotations"].(map[string]any)
		if wantRO := class == "read-only"; ann["readOnlyHint"] != wantRO {
			t.Errorf("%s readOnlyHint: want %v got %v", name, wantRO, ann["readOnlyHint"])
		}
		schema := tl["inputSchema"].(map[string]any)
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s inputSchema not a strict object schema: %v", name, schema)
		}
		if tl["description"] == "" {
			t.Errorf("%s has no description", name)
		}
	}
	if len(got) != len(wantClass) {
		t.Errorf("tool count: want %d got %d (%v)", len(wantClass), len(got), got)
	}
	// login/logout/schedule must not be exposed (spec 0032 R4).
	for _, banned := range []string{"salvage_login", "salvage_logout", "salvage_schedule"} {
		if _, ok := got[banned]; ok {
			t.Errorf("%s must not be exposed as a tool", banned)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	resps := runSession(t, `{"jsonrpc":"2.0","id":5,"method":"resources/list"}`)
	e := resps[0]["error"].(map[string]any)
	if e["code"].(float64) != codeMethodNotFound {
		t.Errorf("expected -32601, got %v", e)
	}
}

func TestUnknownTool(t *testing.T) {
	resps := runSession(t, callMsg(t, "salvage_nuke", nil))
	e, ok := resps[0]["error"].(map[string]any)
	if !ok || e["code"].(float64) != codeInvalidParams {
		t.Errorf("expected -32602 for unknown tool, got %v", resps[0])
	}
}

func TestMalformedJSONLine(t *testing.T) {
	resps := runSession(t, `{this is not json`)
	e := resps[0]["error"].(map[string]any)
	if e["code"].(float64) != codeParseError {
		t.Errorf("expected -32700, got %v", e)
	}
}

// --- argument validation (spec 0032 R3) --------------------------------------

func TestInvalidArguments(t *testing.T) {
	hermetic(t)
	cases := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"missing required", "salvage_run", map[string]any{}},
		{"unknown key", "salvage_run", map[string]any{"config": "x.yaml", "api_key": "sk_nope"}},
		{"wrong type", "salvage_last_good", map[string]any{"config": "x.yaml", "max": "three"}},
		{"non-integer number", "salvage_last_good", map[string]any{"config": "x.yaml", "max": 1.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := toolErr(t, callTool(t, tc.tool, tc.args))
			if e["type"] != "invalid_arguments" {
				t.Errorf("want invalid_arguments, got %v", e)
			}
		})
	}
}

func TestConfigLoadFailureIsOperationalError(t *testing.T) {
	hermetic(t)
	e := toolErr(t, callTool(t, "salvage_run", map[string]any{"config": "/nonexistent/salvage.yaml"}))
	if e["type"] != "operational_error" {
		t.Errorf("want operational_error, got %v", e)
	}
}

// --- verdict vs operational (spec 0032 R7, acceptance 5) ----------------------

func TestRunVerdictFailIsSuccessfulCall(t *testing.T) {
	hermetic(t)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		rep := report.New("t1", "test")
		rep.Restore.OK = false
		rep.Restore.Error = "pg_restore: archive is corrupt"
		rep.Finalize()
		return rep, nil // a bad backup is a result, not an error
	})
	res := callTool(t, "salvage_run", map[string]any{"config": writeConfig(t, execConfig)})
	if res["isError"] == true {
		t.Fatalf("verdict fail must be a successful tool call: %v", res)
	}
	sc := structured(t, res)
	if sc["verdict"] != "fail" {
		t.Errorf("verdict: want fail, got %v", sc["verdict"])
	}
	if sc["schema_version"].(float64) != float64(report.SchemaVersion) {
		t.Errorf("schema_version missing/wrong: %v", sc["schema_version"])
	}
}

func TestRunOperationalErrorIsToolError(t *testing.T) {
	hermetic(t)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		rep := report.New("t1", "test")
		rep.Finalize()
		return rep, errors.New("docker is not reachable")
	})
	e := toolErr(t, callTool(t, "salvage_run", map[string]any{"config": writeConfig(t, execConfig)}))
	if e["type"] != "operational_error" || !strings.Contains(e["message"].(string), "docker") {
		t.Errorf("want structured operational_error mentioning docker, got %v", e)
	}
}

func TestRunPassReturnsVersionedReport(t *testing.T) {
	hermetic(t)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		return passingReport(), nil
	})
	res := callTool(t, "salvage_run", map[string]any{"config": writeConfig(t, execConfig)})
	sc := structured(t, res)
	if sc["verdict"] != "pass" || sc["schema_version"].(float64) != 1 {
		t.Errorf("want versioned passing report, got %v", sc)
	}
	checks := sc["checks"].([]any)
	if len(checks) != 1 {
		t.Errorf("per-check results missing: %v", sc)
	}
	// The text content mirrors the structured payload (both are the 0026 JSON,
	// not scraped CLI text).
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"schema_version"`) {
		t.Errorf("text content is not the report JSON: %s", text)
	}
}

// --- redaction (spec 0032 R6, acceptance 4) -----------------------------------

func TestSecretsAreRedacted(t *testing.T) {
	hermetic(t)
	const secret = "hunter2-super-secret-value"
	t.Setenv("MCP_TEST_SECRET", secret)
	cfg := writeConfig(t, `
target:
  name: t1
  type: exec
  source:
    pass_env: [MCP_TEST_SECRET]
  restore:
    command: ["true"]
`)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		rep := report.New("t1", "test")
		rep.Restore.OK = false
		rep.Restore.Error = fmt.Sprintf(
			"restore failed: postgres://admin:dbpass123@db.internal/x auth with %s (Authorization: Bearer tok_abcdef123456)", secret)
		rep.Finalize()
		return rep, nil
	})
	res := callTool(t, "salvage_run", map[string]any{"config": cfg})
	raw, _ := json.Marshal(res)
	for _, leak := range []string{secret, "dbpass123", "tok_abcdef123456"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("secret %q leaked into tool output: %s", leak, raw)
		}
	}
	if !bytes.Contains(raw, []byte(redactedMarker)) {
		t.Errorf("expected %s marker in output: %s", redactedMarker, raw)
	}
}

func TestSecretsAreRedactedInToolErrors(t *testing.T) {
	hermetic(t)
	const secret = "hunter2-super-secret-value"
	t.Setenv("MCP_TEST_SECRET", secret)
	cfg := writeConfig(t, `
target:
  name: t1
  type: exec
  source:
    pass_env: [MCP_TEST_SECRET]
  restore:
    command: ["true"]
`)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		rep := report.New("t1", "test")
		rep.Finalize()
		return rep, fmt.Errorf("could not reach postgres://u:%s@host", secret)
	})
	e := toolErr(t, callTool(t, "salvage_run", map[string]any{"config": cfg}))
	if strings.Contains(e["message"].(string), secret) {
		t.Errorf("secret leaked into tool-error message: %v", e["message"])
	}
}

// --- check ---------------------------------------------------------------------

func TestCheckExecNeedsNoDocker(t *testing.T) {
	hermetic(t)
	stubPreflight(t, errors.New("preflight must not be called for exec"))
	res := callTool(t, "salvage_check", map[string]any{"config": writeConfig(t, execConfig)})
	sc := structured(t, res)
	if sc["ok"] != true || sc["docker_needed"] != false {
		t.Errorf("exec check: %v", sc)
	}
}

func TestCheckDockerDownIsOperational(t *testing.T) {
	hermetic(t)
	stubPreflight(t, errors.New("docker daemon unreachable"))
	cfg := writeConfig(t, `
target:
  name: pgb
  type: postgres
  source:
    kind: pgbackrest
    stanza: main
  restore:
    image: example/pg-pgbackrest:16
`)
	e := toolErr(t, callTool(t, "salvage_check", map[string]any{"config": cfg}))
	if e["type"] != "operational_error" {
		t.Errorf("want operational_error, got %v", e)
	}
}

// --- inspect --------------------------------------------------------------------

func TestInspectReadsPGData(t *testing.T) {
	hermetic(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("17\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgresql.conf"),
		[]byte("shared_preload_libraries = 'timescaledb'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "base", "16384"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := callTool(t, "salvage_inspect", map[string]any{"pgdata": dir})
	sc := structured(t, res)
	if sc["pg_version"] != "17" || sc["database_count"].(float64) != 1 {
		t.Errorf("inspect result: %v", sc)
	}
}

// --- verify (spec 0032 R5, acceptance 3) ------------------------------------------

func TestVerifyReturnsStructuredVerdict(t *testing.T) {
	hermetic(t)
	var gotEndpoint string
	stubFetch(t, func(_ context.Context, endpoint, id string) (*attest.Record, error) {
		gotEndpoint = endpoint
		return &attest.Record{
			ID: id, Target: "prod-db", Verdict: "pass", Seq: 7, KeyID: "fk1",
			EntryHash: "deadbeef", Signature: "c2ln",
		}, nil
	})
	res := callTool(t, "salvage_verify", map[string]any{"id": "att_123"})
	if gotEndpoint != defaultEndpoint {
		t.Errorf("endpoint: want default %s, got %s", defaultEndpoint, gotEndpoint)
	}
	if res["isError"] == true {
		t.Fatalf("a tampered attestation is a result, not a tool error: %v", res)
	}
	sc := structured(t, res)
	if sc["valid"] != false || sc["id"] != "att_123" || sc["schema_version"].(float64) != 1 {
		t.Errorf("verify verdict: %v", sc)
	}
	if _, ok := sc["checks"].([]any); !ok {
		t.Errorf("per-step checks missing: %v", sc)
	}
}

func TestVerifyEndpointFromCredentialsAndKeyNeverLeaks(t *testing.T) {
	t.Setenv("SALVAGE_ATTEST_KEY", "")
	const key = "sk_live_abcdef0123456789"
	stubCreds(t, &attest.Credentials{Endpoint: "https://custom.example", APIKey: key})
	var gotEndpoint string
	stubFetch(t, func(_ context.Context, endpoint, _ string) (*attest.Record, error) {
		gotEndpoint = endpoint
		// Worst case: an upstream error echoing the credential.
		return nil, fmt.Errorf("HTTP 401: bad key %s", key)
	})
	res := callTool(t, "salvage_verify", map[string]any{"id": "att_123"})
	if gotEndpoint != "https://custom.example" {
		t.Errorf("endpoint should resolve from credentials, got %s", gotEndpoint)
	}
	raw, _ := json.Marshal(res)
	if bytes.Contains(raw, []byte(key)) {
		t.Errorf("stored API key leaked into output: %s", raw)
	}
}

// --- attest (spec 0032 R5) ----------------------------------------------------------

const attestConfig = `
target:
  name: t1
  type: exec
  restore:
    command: ["true"]
attest:
  endpoint: https://notary.example
`

func TestAttestNotAuthenticated(t *testing.T) {
	hermetic(t)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		return passingReport(), nil
	})
	e := toolErr(t, callTool(t, "salvage_attest", map[string]any{"config": writeConfig(t, attestConfig)}))
	if e["type"] != "not_authenticated" {
		t.Fatalf("want not_authenticated, got %v", e)
	}
	if remedy, _ := e["remedy"].(string); !strings.Contains(remedy, "salvage login") {
		t.Errorf("remedy must name `salvage login`: %v", e)
	}
}

func TestAttestSubmitsByReferenceAndReturnsReceipt(t *testing.T) {
	stubCreds(t, nil)
	const key = "sk_env_9876543210fedcba"
	t.Setenv("SALVAGE_ATTEST_KEY", key)
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		return passingReport(), nil
	})
	var gotKey, gotEndpoint string
	var gotReport []byte
	stubSubmit(t, func(_ context.Context, endpoint, apiKey string, rep []byte, _, _ string) (*attest.SubmitResponse, error) {
		gotEndpoint, gotKey, gotReport = endpoint, apiKey, rep
		return &attest.SubmitResponse{ID: "att_9", Verdict: "pass", Seq: 42, VerifyURL: "https://notary.example/a/att_9"}, nil
	})
	res := callTool(t, "salvage_attest", map[string]any{"config": writeConfig(t, attestConfig)})
	if gotKey != key {
		t.Errorf("API key must resolve from the environment by reference")
	}
	if gotEndpoint != "https://notary.example" {
		t.Errorf("endpoint: got %s", gotEndpoint)
	}
	if !bytes.Contains(gotReport, []byte(`"schema_version"`)) {
		t.Errorf("submitted report is not the versioned 0026 payload: %s", gotReport)
	}
	sc := structured(t, res)
	if sc["id"] != "att_9" || sc["seq"].(float64) != 42 {
		t.Errorf("receipt: %v", sc)
	}
	raw, _ := json.Marshal(res)
	if bytes.Contains(raw, []byte(key)) {
		t.Errorf("API key leaked into the receipt output: %s", raw)
	}
}

func TestAttestOperationalRunErrorDoesNotSubmit(t *testing.T) {
	stubCreds(t, nil)
	t.Setenv("SALVAGE_ATTEST_KEY", "sk_env_9876543210fedcba")
	stubRun(t, func(_ context.Context, _ *config.Config) (*report.Report, error) {
		rep := report.New("t1", "test")
		rep.Finalize()
		return rep, errors.New("docker unavailable")
	})
	submitted := false
	stubSubmit(t, func(context.Context, string, string, []byte, string, string) (*attest.SubmitResponse, error) {
		submitted = true
		return nil, nil
	})
	e := toolErr(t, callTool(t, "salvage_attest", map[string]any{"config": writeConfig(t, attestConfig)}))
	if e["type"] != "operational_error" {
		t.Errorf("want operational_error, got %v", e)
	}
	if submitted {
		t.Error("an operational run failure must not submit to the ledger (CLI exits 2 before submitting)")
	}
}

// --- redactor unit coverage ----------------------------------------------------

func TestRedactorPatterns(t *testing.T) {
	stubCreds(t, nil)
	t.Setenv("SALVAGE_ATTEST_KEY", "")
	r := newRedactor(nil)
	r.addSecret("topsecretvalue")
	in := "dsn postgres://app:p4ssw0rd@db:5432/x header Bearer abc.def-123 literal topsecretvalue done"
	out := r.redactString(in)
	for _, leak := range []string{"p4ssw0rd", "abc.def-123", "topsecretvalue"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q: %s", leak, out)
		}
	}
	if !strings.Contains(out, "postgres://app:"+redactedMarker+"@db:5432/x") {
		t.Errorf("DSN user should survive, password redacted: %s", out)
	}
}
