package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Backlog S6 contracts: -verbose/-quiet act on stderr diagnostics only —
// report JSON bytes, stdout output, and exit codes never change; -quiet
// suppresses non-error stderr; -verbose raw output is stderr-only (spec 0027
// R4) and never lands in the report JSON.

// --- handler + level mapping --------------------------------------------------

func TestVerbosityLevelMapping(t *testing.T) {
	cases := []struct {
		name    string
		verbose bool
		quiet   bool
		want    slog.Level
	}{
		{"default is info", false, false, slog.LevelInfo},
		{"verbose is debug", true, false, slog.LevelDebug},
		{"quiet is error-only", false, true, slog.LevelError},
		{"quiet wins over verbose", true, true, slog.LevelError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &verbosity{verbose: tc.verbose, quiet: tc.quiet}
			if got := v.level(); got != tc.want {
				t.Errorf("level() = %v, want %v", got, tc.want)
			}
		})
	}
}

// -quiet suppresses non-error diagnostics; errors still print, bare and
// byte-identical to the pre-slog Fprintln texts.
func TestPlainHandlerQuietSuppressesNonErrors(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, slog.LevelError) // the -quiet level
	l.Debug("debug detail")
	l.Info("wrote salvage.generated.yaml")
	l.Warn("warning: could not sign report locally: nope")
	if buf.Len() != 0 {
		t.Fatalf("non-error diagnostics must be suppressed at the -quiet level, got %q", buf.String())
	}
	l.Error("config error: boom")
	if got := buf.String(); got != "config error: boom\n" {
		t.Errorf("error line = %q, want the bare message with no level/timestamp prefix", got)
	}
}

func TestPlainHandlerAttrsAndVerbose(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, slog.LevelDebug) // the -verbose level
	l.Debug("config loaded", "path", "x.yaml", "target", "t1")
	if got := buf.String(); got != "config loaded path=x.yaml target=t1\n" {
		t.Errorf("debug line = %q", got)
	}
	buf.Reset()
	l.With("target", "t1").Info("hello")
	if got := buf.String(); got != "hello target=t1\n" {
		t.Errorf("WithAttrs line = %q", got)
	}
}

// --- end-to-end: cmdRun under the flags ----------------------------------------

// runConfig is an exec-engine config whose restore command emits a two-line
// combined output, so the report carries a redacted warnings preview and
// -verbose has raw output to surface.
const runConfig = `
target:
  name: s6-test
  type: exec
  restore:
    command: ["sh", "-c", "echo restore-line-one; echo restore-line-two"]
`

func writeRunConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "salvage.yaml")
	if err := os.WriteFile(p, []byte(runConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// captureCmdRun runs cmdRun in-process with stdout/stderr swapped to pipes and
// returns what each stream received. The run must pass (a fail would os.Exit
// and kill the test binary — loudly, which is what we want if the harness
// breaks).
func captureCmdRun(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		// v.apply() inside cmdRun rebound the package logger to the swapped
		// stderr; restore the default so later tests are unaffected.
		logger = newLogger(oldErr, slog.LevelInfo)
	}()

	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errCh <- string(b) }()

	cmdRun(args)
	outW.Close()
	errW.Close()
	return <-outCh, <-errCh
}

// normalizeReportJSON parses one report document and strips the volatile
// timing fields so two runs of the same config compare equal.
func normalizeReportJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, s)
	}
	// A single clean JSON document: nothing but whitespace may follow.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout is not a SINGLE clean JSON document (trailing %v): %s", extra, s)
	}
	delete(m, "started_at")
	delete(m, "finished_at")
	delete(m, "duration_ms")
	if restore, ok := m["restore"].(map[string]any); ok {
		delete(restore, "duration_ms")
	}
	return m
}

func TestRunJSONStdoutByteStableUnderVerbosityFlags(t *testing.T) {
	cfg := writeRunConfig(t)
	base := []string{"-json", "-config", cfg}

	outDef, errDef := captureCmdRun(t, base...)
	outVerb, errVerb := captureCmdRun(t, append([]string{"-verbose"}, base...)...)
	outQuiet, errQuiet := captureCmdRun(t, append([]string{"-quiet"}, base...)...)

	def := normalizeReportJSON(t, outDef)
	verb := normalizeReportJSON(t, outVerb)
	quiet := normalizeReportJSON(t, outQuiet)
	if !reflect.DeepEqual(def, verb) {
		t.Errorf("-verbose changed the report JSON on stdout:\ndefault: %s\nverbose: %s", outDef, outVerb)
	}
	if !reflect.DeepEqual(def, quiet) {
		t.Errorf("-quiet changed the report JSON on stdout:\ndefault: %s\nquiet: %s", outDef, outQuiet)
	}

	// -quiet: a passing run emits no stderr at all (no errors happened).
	if errQuiet != "" {
		t.Errorf("-quiet must suppress non-error stderr, got %q", errQuiet)
	}
	// Default: raw output is not shown without -show-output/-verbose.
	if strings.Contains(errDef, "raw restore output") {
		t.Errorf("raw output printed without -verbose/-show-output: %q", errDef)
	}
	// -verbose: raw output goes to stderr only (spec 0027 R4)...
	if !strings.Contains(errVerb, "--- raw restore output") || !strings.Contains(errVerb, "restore-line-two") {
		t.Errorf("-verbose should print the raw restore output on stderr, got %q", errVerb)
	}
	// ...and never into the report JSON on stdout (the redacted preview keeps
	// only the first line + fingerprint).
	if strings.Contains(outVerb, "restore-line-two") {
		t.Errorf("raw restore output leaked into the report JSON: %s", outVerb)
	}
	// -verbose adds detail on stderr.
	if !strings.Contains(errVerb, "config loaded") {
		t.Errorf("-verbose should add debug detail on stderr, got %q", errVerb)
	}
}

// durationRe normalizes the human summary's per-run timings.
var durationRe = regexp.MustCompile(`\(\d+ms\)`)

func TestRunHumanStdoutStableUnderVerbosityFlags(t *testing.T) {
	cfg := writeRunConfig(t)
	base := []string{"-config", cfg}

	outDef, _ := captureCmdRun(t, base...)
	outVerb, _ := captureCmdRun(t, append([]string{"-verbose"}, base...)...)
	outQuiet, errQuiet := captureCmdRun(t, append([]string{"-quiet"}, base...)...)
	outShow, errShow := captureCmdRun(t, append([]string{"-show-output"}, base...)...)

	norm := func(s string) string { return durationRe.ReplaceAllString(s, "(Xms)") }
	if norm(outDef) != norm(outVerb) {
		t.Errorf("-verbose changed the human summary on stdout:\ndefault: %q\nverbose: %q", outDef, outVerb)
	}
	if norm(outDef) != norm(outQuiet) {
		t.Errorf("-quiet changed the human summary on stdout:\ndefault: %q\nquiet: %q", outDef, outQuiet)
	}
	if !strings.Contains(outDef, "verdict   PASS") {
		t.Errorf("summary lost its verdict line: %q", outDef)
	}
	if errQuiet != "" {
		t.Errorf("-quiet must suppress non-error stderr, got %q", errQuiet)
	}
	// -show-output keeps working: raw output on stderr, stdout unchanged.
	if !strings.Contains(errShow, "--- raw restore output") {
		t.Errorf("-show-output should still print raw output on stderr, got %q", errShow)
	}
	if norm(outDef) != norm(outShow) {
		t.Errorf("-show-output changed stdout:\ndefault: %q\nshow-output: %q", outDef, outShow)
	}
}
