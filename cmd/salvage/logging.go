// Leveled stderr diagnostics + the -verbose/-quiet flags (backlog S6).
//
// Diagnostics are stderr-only and leveled through log/slog with a plain text
// handler that writes bare "msg key=value" lines — no timestamp, no level
// prefix — so the diagnostic texts the CLI has always printed are preserved
// byte-for-byte and merely gain level filtering.
//
// The hard contracts:
//
//   - Neither flag changes report JSON bytes, exit codes, or what is written
//     to stdout (`-json` stdout stays a single clean JSON document; the human
//     summaries stay intact).
//   - -quiet suppresses non-error stderr diagnostics; errors still print.
//   - -verbose adds debug-level detail and, on run/attest, the raw
//     (secret-scrubbed) command output — stderr only, never into the report
//     JSON (spec 0027 R4).

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

// logger is the process-wide diagnostics logger. Commands that accept
// -verbose/-quiet reinstall it via verbosity.apply after flag parsing; every
// other command keeps the default info level.
var logger = newLogger(os.Stderr, slog.LevelInfo)

// verbosity carries a command's parsed -verbose/-quiet flags.
type verbosity struct {
	verbose bool
	quiet   bool
}

// addVerbosityFlags registers -verbose and -quiet on a command's flag set.
func addVerbosityFlags(fs *flag.FlagSet) *verbosity {
	v := &verbosity{}
	fs.BoolVar(&v.verbose, "verbose", false,
		"add diagnostic detail on stderr (stdout, report JSON, and exit codes are unchanged)")
	fs.BoolVar(&v.quiet, "quiet", false,
		"suppress non-error diagnostics on stderr (stdout, report JSON, and exit codes are unchanged)")
	return v
}

// apply installs the selected diagnostic level. Called after flag parsing,
// before any diagnostic can be emitted.
func (v *verbosity) apply() {
	logger = newLogger(os.Stderr, v.level())
}

// level maps the flags to a slog level. -quiet wins over -verbose: when both
// are set the caller asked for silence, and errors still print either way.
func (v *verbosity) level() slog.Level {
	switch {
	case v.quiet:
		return slog.LevelError
	case v.verbose:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

// showRaw reports whether the raw (redacted-from-the-report, still
// secret-scrubbed) command output should be printed to stderr (spec 0027 R4).
// -verbose implies it; run's -show-output flag keeps working as the explicit
// opt-in; -quiet suppresses it — raw output is non-error stderr.
func (v *verbosity) showRaw(showOutput bool) bool {
	return (showOutput || v.verbose) && !v.quiet
}

// newLogger builds a logger writing plain lines to w at the given level.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(&plainHandler{mu: &sync.Mutex{}, w: w, level: level})
}

// plainHandler is a minimal slog.Handler for CLI diagnostics: it writes the
// record's message followed by any "key=value" attrs and a newline. No
// timestamps or level tags — these are terminal diagnostics for a human (or a
// journal that stamps its own metadata), not structured log shipping.
type plainHandler struct {
	mu    *sync.Mutex // shared across WithAttrs clones
	w     io.Writer
	level slog.Level
	attrs string // pre-rendered " key=value" pairs from WithAttrs
}

func (h *plainHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *plainHandler) Handle(_ context.Context, r slog.Record) error {
	line := r.Message + h.attrs
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.w, line)
	return err
}

func (h *plainHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	for _, a := range attrs {
		nh.attrs += " " + a.Key + "=" + a.Value.String()
	}
	return &nh
}

// WithGroup is a no-op: this CLI never logs groups.
func (h *plainHandler) WithGroup(string) slog.Handler { return h }
