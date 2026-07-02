package alert

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Spec 0030 R1: a URL hook receives the run's report JSON as a POST body with
// Content-Type: application/json — the same bytes Salvage wrote to disk.
func TestURLHookPostsReportJSON(t *testing.T) {
	payload := []byte(`{"schema_version":1,"verdict":"fail"}`)
	var gotMethod, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	h := Hook{Spec: srv.URL + "/salvage"}
	if err := h.Fire(context.Background(), payload, ""); err != nil {
		t.Fatalf("fire: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Errorf("body = %s, want the exact report bytes", gotBody)
	}
}

// Spec 0030 R7: a `*_ref=env:NAME` parameter is resolved from the environment
// only at delivery time — the request carries the value under the trimmed
// name, and the reference parameter itself is gone.
func TestURLHookResolvesEnvRefs(t *testing.T) {
	const secret = "s3cr3t-token-value"
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
	}))
	defer srv.Close()

	h := Hook{
		Spec: srv.URL + "/salvage?token_ref=env:SALVAGE_HOOK_TOKEN&keep=1",
		Getenv: func(name string) string {
			if name == "SALVAGE_HOOK_TOKEN" {
				return secret
			}
			return ""
		},
	}
	if err := h.Fire(context.Background(), []byte(`{}`), ""); err != nil {
		t.Fatalf("fire: %v", err)
	}
	q := gotQuery
	if !strings.Contains(q, "token="+secret) {
		t.Errorf("query %q does not carry the resolved token", q)
	}
	if strings.Contains(q, "token_ref") {
		t.Errorf("query %q still carries the reference parameter", q)
	}
	if !strings.Contains(q, "keep=1") {
		t.Errorf("query %q dropped a non-ref parameter", q)
	}
}

// Spec 0030 R7: an unset env reference is a delivery error naming the env
// var — no request is made, and no error path ever echoes a secret value.
func TestURLHookUnsetEnvRefFails(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	h := Hook{
		Spec:   srv.URL + "?token_ref=env:SALVAGE_MISSING_TOKEN",
		Getenv: func(string) string { return "" },
	}
	err := h.Fire(context.Background(), []byte(`{}`), "")
	if err == nil {
		t.Fatal("expected an error for an unset env reference")
	}
	if !strings.Contains(err.Error(), "SALVAGE_MISSING_TOKEN") {
		t.Errorf("error %q should name the env var", err)
	}
	if calls.Load() != 0 {
		t.Errorf("request was sent despite the unresolved reference")
	}
}

// A non-2xx response is a hook error, and the error echoes only the
// configured (by-reference) spec — never the resolved token (spec 0030 R7).
func TestURLHookNon2xxErrorRedactsSecret(t *testing.T) {
	const secret = "super-secret-999"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	h := Hook{
		Spec:   srv.URL + "?token_ref=env:TOK",
		Getenv: func(string) string { return secret },
	}
	err := h.Fire(context.Background(), []byte(`{}`), "")
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should carry the status", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q leaks the resolved secret", err)
	}
}

// Spec 0030 R2: a hook that hangs is cut off at the bounded timeout, and the
// timeout error does not echo the resolved URL (a *url.Error would).
func TestURLHookTimeoutRedactsSecret(t *testing.T) {
	const secret = "timeout-secret-42"
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	h := Hook{
		Spec:    srv.URL + "?token_ref=env:TOK",
		Timeout: 100 * time.Millisecond,
		Getenv:  func(string) string { return secret },
	}
	start := time.Now()
	err := h.Fire(context.Background(), []byte(`{}`), "")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("hook was not bounded by the timeout (took %v)", elapsed)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("timeout error %q leaks the resolved secret", err)
	}
}

// Spec 0030 R1: a command hook receives the report JSON on stdin and the
// report file path in $SALVAGE_REPORT.
func TestCommandHookStdinAndReportPath(t *testing.T) {
	payload := []byte(`{"schema_version":1,"verdict":"pass"}`)
	dir := t.TempDir()
	gotBody := filepath.Join(dir, "body.json")
	gotPath := filepath.Join(dir, "path.txt")

	h := Hook{Spec: `cat > "` + gotBody + `" && printf '%s' "$SALVAGE_REPORT" > "` + gotPath + `"`}
	if err := h.Fire(context.Background(), payload, "/reports/out.json"); err != nil {
		t.Fatalf("fire: %v", err)
	}
	body, err := os.ReadFile(gotBody)
	if err != nil {
		t.Fatalf("hook did not write stdin capture: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("stdin = %s, want the exact report bytes", body)
	}
	path, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("hook did not write $SALVAGE_REPORT capture: %v", err)
	}
	if string(path) != "/reports/out.json" {
		t.Errorf("$SALVAGE_REPORT = %q, want /reports/out.json", path)
	}
}

// A command hook that exits non-zero returns an error for the caller to log;
// deciding that the run's exit code is unchanged is the caller's job (R2).
func TestCommandHookNonZeroExitIsError(t *testing.T) {
	h := Hook{Spec: "exit 7", Stderr: io.Discard}
	err := h.Fire(context.Background(), []byte(`{}`), "")
	if err == nil {
		t.Fatal("expected an error for a failing hook command")
	}
	if !strings.Contains(err.Error(), "hook command") {
		t.Errorf("error %q should identify the hook command", err)
	}
}

// Spec 0030 R2: a command hook that hangs past the bounded timeout is killed.
func TestCommandHookTimeout(t *testing.T) {
	h := Hook{Spec: "sleep 30", Timeout: 100 * time.Millisecond, Stderr: io.Discard}
	start := time.Now()
	err := h.Fire(context.Background(), []byte(`{}`), "")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("hook was not bounded by the timeout (took %v)", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q should say the hook timed out", err)
	}
}

// http:// and https:// specs are URL hooks; everything else is a command —
// so a URL-looking value is never handed to a shell.
func TestIsURL(t *testing.T) {
	cases := map[string]bool{
		"https://hooks.example/salvage": true,
		"http://127.0.0.1:8080/hook":    true,
		"./notify.sh":                   false,
		"curl -s https://x":             false,
		"":                              false,
	}
	for spec, want := range cases {
		if got := IsURL(spec); got != want {
			t.Errorf("IsURL(%q) = %v, want %v", spec, got, want)
		}
	}
}

// Spec 0030 R7 at load time: embedded credentials and literal `*_ref` values
// are config errors; a clean by-reference URL and any command pass.
func TestValidateSpec(t *testing.T) {
	if err := ValidateSpec("./notify.sh --flag"); err != nil {
		t.Errorf("command spec should validate: %v", err)
	}
	if err := ValidateSpec("https://hooks.example/s?token_ref=env:TOK&x=1"); err != nil {
		t.Errorf("by-reference URL should validate: %v", err)
	}
	if err := ValidateSpec("https://user:pass@hooks.example/s"); err == nil {
		t.Error("embedded user:pass@ should be rejected")
	}
	if err := ValidateSpec("https://hooks.example/s?token_ref=literal-secret"); err == nil {
		t.Error("a literal *_ref value should be rejected")
	}
	if err := ValidateSpec("https://hooks.example/s?token_ref=env:"); err == nil {
		t.Error("an empty env reference should be rejected")
	}
}

// RefEnvNames feeds the known-secret set (spec 0027 R3): every env var a URL
// hook references, and nothing for a command hook.
func TestRefEnvNames(t *testing.T) {
	got := RefEnvNames("https://hooks.example/s?b_ref=env:B_TOK&a_ref=env:A_TOK&x=1")
	if len(got) != 2 || got[0] != "A_TOK" || got[1] != "B_TOK" {
		t.Errorf("RefEnvNames = %v, want [A_TOK B_TOK]", got)
	}
	if got := RefEnvNames("./notify.sh"); got != nil {
		t.Errorf("RefEnvNames(command) = %v, want nil", got)
	}
}
