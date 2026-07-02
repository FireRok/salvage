package probe_test

import (
	"context"
	"errors"
	"testing"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"

	// Wire in the shared check kinds under test (probe.init registers them).
	_ "salvage.sh/internal/probe"
)

// fakeProber is a scripted FileProber for evaluator tests: no Docker, just the
// answers each probe should return. It mirrors the fake the restic engine used
// before the four kinds moved into the probe package (behaviour-preserving).
type fakeProber struct {
	exists    bool
	existsErr error
	count     int
	countErr  error
	sum       string
	sumErr    error
	out       string
	exit      int
	cmdErr    error
}

func (f fakeProber) Exists(context.Context, string) (bool, error)   { return f.exists, f.existsErr }
func (f fakeProber) Count(context.Context, string) (int, error)     { return f.count, f.countErr }
func (f fakeProber) Sha256(context.Context, string) (string, error) { return f.sum, f.sumErr }
func (f fakeProber) RunCommand(context.Context, string) (string, int, error) {
	return f.out, f.exit, f.cmdErr
}

func pf(f float64) *float64 { return &f }
func ps(s string) *string   { return &s }
func pb(b bool) *bool       { return &b }

// run through checks.Run so the kind dispatch (the registered evaluators) is
// exercised exactly as production does it.
func run(t *testing.T, target checks.Target, c config.Check) (ok bool, detail, errStr, got string) {
	t.Helper()
	r := checks.Run(context.Background(), target, []config.Check{c})
	if len(r) != 1 {
		t.Fatalf("want 1 result, got %d", len(r))
	}
	return r[0].OK, r[0].Detail, r[0].Error, r[0].Got
}

func TestFileExists(t *testing.T) {
	// Present + expect-exists (default) → pass.
	if ok, _, es, _ := run(t, fakeProber{exists: true}, config.Check{Name: "e", Kind: "file_exists", Path: "a"}); !ok {
		t.Errorf("present file should pass (err=%s)", es)
	}
	// Absent + expect-exists → fail.
	if ok, _, _, _ := run(t, fakeProber{exists: false}, config.Check{Name: "e", Kind: "file_exists", Path: "a"}); ok {
		t.Error("absent file should fail expect-exists")
	}
	// Absent + expect-absent (bool:false) → pass.
	if ok, _, es, _ := run(t, fakeProber{exists: false}, config.Check{Name: "e", Kind: "file_exists", Path: "a", Bool: pb(false)}); !ok {
		t.Errorf("absent file should pass expect-absent (err=%s)", es)
	}
	// Probe error → non-OK with Error set.
	ok, _, es, _ := run(t, fakeProber{existsErr: errors.New("boom")}, config.Check{Name: "e", Kind: "file_exists", Path: "a"})
	if ok || es == "" {
		t.Errorf("probe error should surface (ok=%v err=%q)", ok, es)
	}
}

func TestFileCount(t *testing.T) {
	if ok, d, _, got := run(t, fakeProber{count: 3}, config.Check{Name: "c", Kind: "file_count", Path: "*.txt", ExpectMin: pf(1)}); !ok || got != "3" {
		t.Errorf("3 >= min 1 should pass (ok=%v detail=%s got=%s)", ok, d, got)
	}
	if ok, d, _, _ := run(t, fakeProber{count: 0}, config.Check{Name: "c", Kind: "file_count", Path: "*.txt", ExpectMin: pf(1)}); ok {
		t.Errorf("0 < min 1 should fail (detail=%s)", d)
	}
	if ok, d, _, _ := run(t, fakeProber{count: 9}, config.Check{Name: "c", Kind: "file_count", Path: "*.txt", ExpectMax: pf(5)}); ok {
		t.Errorf("9 > max 5 should fail (detail=%s)", d)
	}
}

func TestChecksum(t *testing.T) {
	if ok, _, es, _ := run(t, fakeProber{sum: "abc123"}, config.Check{Name: "s", Kind: "checksum", Path: "a", Equals: ps("abc123")}); !ok {
		t.Errorf("matching sha256 should pass (err=%s)", es)
	}
	if ok, d, _, _ := run(t, fakeProber{sum: "deadbeef"}, config.Check{Name: "s", Kind: "checksum", Path: "a", Equals: ps("abc123")}); ok {
		t.Errorf("mismatched sha256 should fail (detail=%s)", d)
	}
	ok, _, es, _ := run(t, fakeProber{sumErr: errors.New("no file")}, config.Check{Name: "s", Kind: "checksum", Path: "a", Equals: ps("abc123")})
	if ok || es == "" {
		t.Errorf("sha256 error should surface (ok=%v err=%q)", ok, es)
	}
}

func TestCommand(t *testing.T) {
	// exit 0 → pass.
	if ok, d, es, _ := run(t, fakeProber{exit: 0}, config.Check{Name: "cmd", Kind: "command", Command: "true"}); !ok {
		t.Errorf("exit 0 should pass (detail=%s err=%s)", d, es)
	}
	// non-zero exit → fail, got carries "exit N".
	if ok, _, _, got := run(t, fakeProber{out: "nope\nmore", exit: 2}, config.Check{Name: "cmd", Kind: "command", Command: "false"}); ok || got != "exit 2" {
		t.Errorf("non-zero exit should fail with got=exit 2 (ok=%v got=%s)", ok, got)
	}
	// equals on stdout → judged by output, exit ignored.
	if ok, _, _, _ := run(t, fakeProber{out: "ok", exit: 1}, config.Check{Name: "cmd", Kind: "command", Command: "echo ok", Equals: ps("ok")}); !ok {
		t.Error("equals on matching stdout should pass regardless of exit")
	}
	// operational failure (could not run) → Error set.
	ok, _, es, _ := run(t, fakeProber{cmdErr: errors.New("docker gone")}, config.Check{Name: "cmd", Kind: "command", Command: "x"})
	if ok || es == "" {
		t.Errorf("command run error should surface (ok=%v err=%q)", ok, es)
	}
}

// notAProber satisfies the minimal RestoredTarget (Stop) but neither FileProber
// nor HTTPProber — like a SQL engine's target reaching a file/http check. Every
// probe evaluator must fail cleanly, never panic on the type-assert.
type notAProber struct{}

func (notAProber) Stop() error { return nil }

func TestBadTargetFailsCleanly(t *testing.T) {
	for _, c := range []config.Check{
		{Name: "e", Kind: "file_exists", Path: "a"},
		{Name: "c", Kind: "file_count", Path: "*", ExpectMin: pf(1)},
		{Name: "s", Kind: "checksum", Path: "a", Equals: ps("x")},
		{Name: "m", Kind: "command", Command: "true"},
		{Name: "h", Kind: "http", URL: "http://x"},
	} {
		ok, _, es, _ := run(t, notAProber{}, c)
		if ok || es == "" {
			t.Errorf("kind %q on a non-prober target should fail with Error (ok=%v err=%q)", c.Kind, ok, es)
		}
	}
}
