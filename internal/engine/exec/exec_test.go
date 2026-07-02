package exec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"

	_ "salvage.sh/internal/probe"
)

func ps(s string) *string   { return &s }
func pf(f float64) *float64 { return &f }
func pi(i int) *int         { return &i }

// runCheck dispatches one check through checks.Run against target, exactly as
// production does (exercising the kind registry).
func runCheck(t *testing.T, target checks.Target, c config.Check) (ok bool, detail, errStr, got string) {
	t.Helper()
	r := checks.Run(context.Background(), target, []config.Check{c})
	if len(r) != 1 {
		t.Fatalf("want 1 result, got %d", len(r))
	}
	return r[0].OK, r[0].Detail, r[0].Error, r[0].Got
}

func TestType(t *testing.T) {
	if (Engine{}).Type() != "exec" {
		t.Errorf("Type() = %q, want exec", Engine{}.Type())
	}
}

// --- http evaluator against a real httptest.Server ---

func TestHTTPEvaluator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"db":"ok","rows":42}`))
		case "/teapot":
			w.WriteHeader(418)
			_, _ = w.Write([]byte("nope"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	host := &Host{}

	// status pass (default 200) + body-contains pass + json-path pass.
	if ok, d, es, got := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/healthz",
		ExpectBodyContains: `"db":"ok"`, ExpectJSON: "db=ok",
	}); !ok || got != "200" {
		t.Errorf("healthz should pass (ok=%v detail=%s err=%s got=%s)", ok, d, es, got)
	}

	// json-path numeric value.
	if ok, d, es, _ := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/healthz", ExpectJSON: "rows=42",
	}); !ok {
		t.Errorf("json numeric path should pass (detail=%s err=%s)", d, es)
	}

	// status fail: default 200 but server returns 418.
	if ok, d, _, got := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/teapot",
	}); ok || got != "418" {
		t.Errorf("418 vs default 200 should fail (ok=%v detail=%s got=%s)", ok, d, got)
	}

	// explicit expect_status matches.
	if ok, _, es, _ := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/teapot", ExpectStatus: pi(418),
	}); !ok {
		t.Errorf("explicit expect_status 418 should pass (err=%s)", es)
	}

	// body-contains fail.
	if ok, d, _, _ := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/healthz", ExpectBodyContains: "missing",
	}); ok {
		t.Errorf("body-contains miss should fail (detail=%s)", d)
	}

	// json-path value mismatch → fail.
	if ok, d, _, _ := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/healthz", ExpectJSON: "db=broken",
	}); ok {
		t.Errorf("json value mismatch should fail (detail=%s)", d)
	}

	// json-path absent key → fail.
	if ok, d, _, _ := runCheck(t, host, config.Check{
		Name: "h", Kind: "http", URL: srv.URL + "/healthz", ExpectJSON: "missing=x",
	}); ok {
		t.Errorf("json absent key should fail (detail=%s)", d)
	}
}

// --- host file probers against a t.TempDir() ---

func TestHostFileProbers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "MANIFEST"), "hello")
	writeFile(t, filepath.Join(dir, "a.txt"), "one")
	writeFile(t, filepath.Join(dir, "b.txt"), "two")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	host := &Host{workdir: dir}

	// file_exists: present (relative path resolves against workdir).
	if ok, _, es, _ := runCheck(t, host, config.Check{Name: "e", Kind: "file_exists", Path: "MANIFEST"}); !ok {
		t.Errorf("MANIFEST should exist (err=%s)", es)
	}
	// file_exists: absent → fail expect-exists.
	if ok, _, _, _ := runCheck(t, host, config.Check{Name: "e", Kind: "file_exists", Path: "nope"}); ok {
		t.Error("absent file should fail expect-exists")
	}
	// file_count: 2 *.txt files (glob), directory excluded.
	if ok, d, _, got := runCheck(t, host, config.Check{Name: "c", Kind: "file_count", Path: "*.txt", ExpectMin: pf(2)}); !ok || got != "2" {
		t.Errorf("2 txt files >= min 2 should pass (ok=%v detail=%s got=%s)", ok, d, got)
	}
	// file_count fail: too many for max.
	if ok, d, _, _ := runCheck(t, host, config.Check{Name: "c", Kind: "file_count", Path: "*.txt", ExpectMax: pf(1)}); ok {
		t.Errorf("2 > max 1 should fail (detail=%s)", d)
	}
	// checksum: sha256("hello").
	const helloSum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if ok, d, es, _ := runCheck(t, host, config.Check{Name: "s", Kind: "checksum", Path: "MANIFEST", Equals: ps(helloSum)}); !ok {
		t.Errorf("checksum of hello should match (detail=%s err=%s)", d, es)
	}
	// checksum mismatch → fail.
	if ok, _, _, _ := runCheck(t, host, config.Check{Name: "s", Kind: "checksum", Path: "MANIFEST", Equals: ps("deadbeef")}); ok {
		t.Error("mismatched checksum should fail")
	}
	// command: runs in the workdir, exit 0.
	if ok, d, es, _ := runCheck(t, host, config.Check{Name: "cmd", Kind: "command", Command: "test -f MANIFEST"}); !ok {
		t.Errorf("command should pass (detail=%s err=%s)", d, es)
	}
	// command: non-zero exit → fail.
	if ok, _, _, got := runCheck(t, host, config.Check{Name: "cmd", Kind: "command", Command: "exit 3"}); ok || got != "exit 3" {
		t.Errorf("exit 3 should fail with got=exit 3 (ok=%v got=%s)", ok, got)
	}
}

// --- exec Restore: operational-vs-verdict split ---

func TestRestoreSuccess(t *testing.T) {
	cfg := execCfg([]string{"sh", "-c", "echo restored; exit 0"})
	rt, warn, err := (Engine{}).Restore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("exit 0 restore should succeed, got err=%v", err)
	}
	if rt == nil {
		t.Fatal("expected a live RestoredTarget")
	}
	defer rt.Stop()
	if warn == "" {
		t.Errorf("expected captured stdout tail, got empty")
	}
}

func TestRestoreVerdictFail(t *testing.T) {
	cfg := execCfg([]string{"sh", "-c", "echo boom >&2; exit 3"})
	rt, _, err := (Engine{}).Restore(context.Background(), cfg)
	if err == nil {
		t.Fatal("exit 3 should be an error (a fail verdict)")
	}
	// It must NOT be an operational fault: a command that ran and exited non-zero
	// is a verdict fail with a nil operational error at the orchestrator.
	var f *spi.Fault
	if errors.As(err, &f) {
		t.Errorf("non-zero exit must be a verdict fail, not a *spi.Fault (got %v)", err)
	}
	if rt != nil {
		t.Errorf("no live target on a failed restore")
	}
}

func TestRestoreTimeoutIsVerdictFail(t *testing.T) {
	cfg := execCfg([]string{"sh", "-c", "sleep 30"})
	cfg.Target.Restore.Timeout = config.Duration(100_000_000) // 100ms
	rt, _, err := (Engine{}).Restore(context.Background(), cfg)
	if err == nil {
		t.Fatal("a restore that exceeds its timeout must error")
	}
	// A timeout is a restore that did not complete in time: a verdict fail, not an
	// operational fault (the command launched fine).
	var f *spi.Fault
	if errors.As(err, &f) {
		t.Errorf("timeout must be a verdict fail, not a *spi.Fault (got %v)", err)
	}
	if rt != nil {
		t.Errorf("no live target when the restore timed out")
	}
}

func TestRestoreCannotLaunchIsFault(t *testing.T) {
	cfg := execCfg([]string{"salvage-no-such-binary-xyz"})
	rt, _, err := (Engine{}).Restore(context.Background(), cfg)
	if err == nil {
		t.Fatal("a missing binary should error")
	}
	var f *spi.Fault
	if !errors.As(err, &f) {
		t.Errorf("cannot-launch must be a *spi.Fault (operational), got %T: %v", err, err)
	}
	if rt != nil {
		t.Errorf("no live target when the process cannot launch")
	}
}

func TestRestoreEmptyCommandIsFault(t *testing.T) {
	cfg := execCfg(nil)
	_, _, err := (Engine{}).Restore(context.Background(), cfg)
	var f *spi.Fault
	if !errors.As(err, &f) {
		t.Errorf("empty command must be a *spi.Fault, got %T: %v", err, err)
	}
}

// --- Stop / cleanup ---

func TestStopRunsCleanupIdempotently(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cleaned")
	h := &Host{cleanup: []string{"sh", "-c", "echo x >> " + marker}}
	if err := h.Stop(); err != nil {
		t.Fatalf("cleanup should succeed: %v", err)
	}
	// Second Stop is a no-op (idempotent): the marker must not grow.
	if err := h.Stop(); err != nil {
		t.Fatalf("second Stop should be a no-op: %v", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("cleanup did not run: %v", err)
	}
	if string(b) != "x\n" {
		t.Errorf("cleanup ran more than once: %q", b)
	}
}

func execCfg(command []string) *config.Config {
	return &config.Config{
		Target: config.Target{
			Type:    "exec",
			Name:    "t",
			Restore: config.Restore{Command: command, Timeout: config.Duration(30_000_000_000)},
		},
		Report: config.Report{Format: "json"},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
