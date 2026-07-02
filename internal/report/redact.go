// Report redaction & secret hygiene (spec 0027).
//
// Spec 0003 closed the *input* half of secret discipline (credentials forwarded
// by reference, never printed). This file closes the *output* half: a restore
// or check command that echoes a credential must not be able to smuggle it past
// the report boundary into report.out or the counter-signed attestation bytes.
//
// Two mechanisms compose, applied in order (spec 0027 Design):
//
//	(A) captured-output redaction (structural): the free-text program-output
//	    fields — RestoreResult.Warnings (the restore combined-output tail) and
//	    free-text CheckResult.Got — are bounded and de-identified regardless of
//	    content, stored as a scrubbed first-line preview plus a SHA-256
//	    fingerprint.
//	(B) known-secret scrubbing (value-based): every occurrence of each resolved
//	    known secret value (source.pass_env / restore.env variables, plus any
//	    engine-forwarded container password registered for the run) is replaced
//	    with a fixed [REDACTED:<name>] marker. Exact-value, low-false-positive —
//	    Salvage already holds these values; no guessing.
//
// The transform runs in-place, is idempotent (R8), and is invoked by WriteJSON
// — the single serializer for report.out, `run -json` stdout, and the attest
// submission — so redaction is default-on for every durable or counter-signed
// artifact (R5) with no opt-in required at any call site. It changes only what
// is *captured*: schema_version, the field set, canonicalization, signing, and
// `salvage verify` are untouched (R6).

package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// previewMax bounds the scrubbed first-line preview kept when a captured
	// string is structurally redacted (spec 0027 Design A). Long enough to
	// triage ("pg_restore: error: ..."), short enough that the report never
	// carries a meaningful slice of the raw stream.
	previewMax = 120

	// gotInlineMax is the bound under which a single-line Got value is stored
	// as-is (after known-secret scrubbing). Scalar Got values — counts,
	// booleans, sha256 digests, "exit 0", short asserted literals — all fit and
	// are preserved unchanged (R2); anything longer or multi-line is free-text
	// program output and is reduced to a preview + fingerprint.
	gotInlineMax = 256

	// minSecretLen is the shortest known-secret value that is scrubbed. Empty
	// or very short values are skipped to avoid degenerate matches that would
	// shred unrelated text (R3: "too short to scrub safely MUST be skipped").
	minSecretLen = 4
)

// Secret is one named credential value known to the run — the resolved value of
// a pass_env/restore.env variable, or an engine-forwarded container password.
// The name appears in the redaction marker; the value never appears anywhere.
type Secret struct {
	Name  string
	Value string
}

// KnownSecretsFromEnv resolves the named environment variables into the known
// secret set for a run (spec 0027 R3). Names whose resolved value is empty or
// too short to scrub safely are skipped. The caller passes the by-reference
// credential names it forwarded: source.pass_env plus, for the exec engine,
// restore.env (see config.SecretEnvNames).
func KnownSecretsFromEnv(lookup func(string) string, names []string) []Secret {
	out := make([]Secret, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		v := lookup(n)
		if len(v) < minSecretLen {
			continue // R3: empty/too-short values are skipped, never scrubbed
		}
		out = append(out, Secret{Name: n, Value: v})
	}
	return out
}

// SetKnownSecrets records the known secret set scrubbed from every captured
// string at serialization time (spec 0027 R3). The set is held on the report
// (unexported — never serialized) so the single serializer, WriteJSON, applies
// it on every path without call sites having to remember.
func (r *Report) SetKnownSecrets(secrets []Secret) {
	r.secrets = nil
	for _, s := range secrets {
		r.AddKnownSecret(s.Name, s.Value)
	}
}

// AddKnownSecret adds one named secret value to the report's known set —
// e.g. an engine-forwarded container password (MYSQL_PWD, MONGO_PWD, PGPASSWORD
// analogues, spec 0027 R3) that did not arrive via pass_env. Values too short
// to scrub safely are skipped.
func (r *Report) AddKnownSecret(name, value string) {
	if len(value) < minSecretLen {
		return
	}
	r.secrets = append(r.secrets, Secret{Name: name, Value: value})
}

// RawCheckOutput pairs a check name with the raw Got value redaction replaced.
type RawCheckOutput struct {
	Name string
	Got  string
}

// RawOutput is what Redact removed from the report: the raw captured strings,
// returned so a local-only verbose flag can surface them on stderr (spec 0027
// R4). It exists only in memory — it is never part of the report, the signing
// payload, or an attest submission.
type RawOutput struct {
	RestoreWarnings string
	Checks          []RawCheckOutput
}

// Empty reports whether redaction changed nothing (nothing raw to show).
func (ro *RawOutput) Empty() bool {
	return ro == nil || (ro.RestoreWarnings == "" && len(ro.Checks) == 0)
}

// Fprint writes the raw captured output for human debugging. Intended for
// stderr under a local-only verbose flag (spec 0027 R4); never write this to a
// file that could travel.
func (ro *RawOutput) Fprint(w io.Writer) {
	if ro.Empty() {
		return
	}
	if ro.RestoreWarnings != "" {
		fmt.Fprintf(w, "--- raw restore output (local only, not in report) ---\n%s\n", ro.RestoreWarnings)
	}
	for _, c := range ro.Checks {
		fmt.Fprintf(w, "--- raw check output %q (local only, not in report) ---\n%s\n", c.Name, c.Got)
	}
}

// redactedFingerprint matches the trailing fingerprint marker a structurally
// redacted value carries — the idempotence guard (R8): a value that already
// ends in a fingerprint is not redacted again.
var redactedFingerprint = regexp.MustCompile(`\[sha256:[0-9a-f]{64}\]$`)

// Redact applies the spec 0027 transform to the report in place and returns
// the raw values it replaced (for the local-only verbose path, R4). It is
// idempotent: redacting an already-redacted report changes nothing and returns
// an empty RawOutput. WriteJSON calls it automatically, so an explicit call is
// only needed to capture the raw output before serialization.
func (r *Report) Redact() *RawOutput {
	raw := &RawOutput{}
	scrub := scrubber(r.secrets)

	// (R1) Restore combined-output tail: bounded preview + fingerprint on every
	// path. Restore.Error can embed the first line of that same tail (the exec
	// engine's non-zero-exit detail), so it is scrubbed too.
	if w := r.Restore.Warnings; w != "" && !redactedFingerprint.MatchString(w) {
		red := redactCaptured(w, scrub)
		if red != w {
			raw.RestoreWarnings = w
			r.Restore.Warnings = red
		}
	}
	r.Restore.Error = scrub(r.Restore.Error)

	// (R2/R3) Check results: Got is bounded + scrubbed (free text) or preserved
	// (short scalars); Detail and Error may carry first lines of program output
	// (e.g. evalCommand's failure detail), so they are scrubbed as well.
	for i := range r.Checks {
		c := &r.Checks[i]
		if c.Got != "" {
			red := redactGot(c.Got, c.KeepLiteral, scrub)
			if red != c.Got {
				raw.Checks = append(raw.Checks, RawCheckOutput{Name: c.Name, Got: c.Got})
				c.Got = red
			}
		}
		c.Detail = scrub(c.Detail)
		c.Error = scrub(c.Error)
	}
	return raw
}

// redactCaptured reduces a captured program-output stream to its bounded,
// non-secret-bearing form (spec 0027 Design A): a scrubbed, truncated
// first-line preview plus a SHA-256 fingerprint of the full scrubbed stream.
// The hash is computed over the *scrubbed* stream so it remains a stable
// fingerprint of the output without being usable to confirm guesses of a
// removed secret (see the spec's hash-salting open question).
func redactCaptured(s string, scrub func(string) string) string {
	scrubbed := scrub(s)
	sum := sha256.Sum256([]byte(scrubbed))
	preview := truncate(firstLine(scrubbed), previewMax)
	if preview == "" {
		return "[sha256:" + hex.EncodeToString(sum[:]) + "]"
	}
	return preview + " [sha256:" + hex.EncodeToString(sum[:]) + "]"
}

// redactGot redacts one Got value (spec 0027 R2). Short single-line values —
// the scalar kinds (checksum digests, counts, booleans, "exit N") and typical
// asserted literals — pass through unchanged apart from known-secret scrubbing.
// Longer or multi-line values are free-text program output and are reduced to
// a preview + fingerprint. keepLiteral is the explicit per-check opt-in (R2/R5,
// config keep_literal): the exact literal is retained for a byte-equal
// assertion, but it still passes known-secret scrubbing (R3).
func redactGot(got string, keepLiteral bool, scrub func(string) string) string {
	scrubbed := scrub(got)
	if keepLiteral {
		return scrubbed
	}
	if !strings.ContainsAny(scrubbed, "\n\r") && len(scrubbed) <= gotInlineMax {
		return scrubbed
	}
	return redactCaptured(got, scrub)
}

// NewScrubber exposes the known-secret scrub to boundary layers outside the
// Report transform — the MCP server (spec 0032 R6) scrubs every byte a tool
// call returns with it. A Secret with an empty Name redacts to the bare
// "[REDACTED]" marker: at a boundary that collects ambient credential values
// (stored login keys, forwarded env), a stable per-value name is not always
// available, and a uniform marker leaks nothing about which secret matched.
func NewScrubber(secrets []Secret) func(string) string { return scrubber(secrets) }

// scrubber returns the known-secret scrub (spec 0027 Design B): an exact-value
// replacement of every occurrence of each secret with [REDACTED:<name>] (or
// the bare [REDACTED] when the secret is unnamed — see NewScrubber).
// Longer values are replaced first so a secret that contains another as a
// substring cannot leave a recognizable remainder behind.
func scrubber(secrets []Secret) func(string) string {
	if len(secrets) == 0 {
		return func(s string) string { return s }
	}
	ordered := make([]Secret, len(secrets))
	copy(ordered, secrets)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i].Value) > len(ordered[j].Value) })
	return func(s string) string {
		if s == "" {
			return s
		}
		for _, sec := range ordered {
			if sec.Value == "" {
				continue
			}
			marker := "[REDACTED]"
			if sec.Name != "" {
				marker = "[REDACTED:" + sec.Name + "]"
			}
			s = strings.ReplaceAll(s, sec.Value, marker)
		}
		return s
	}
}

// firstLine returns s up to the first newline, trimmed.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// truncate bounds s to at most max bytes without slicing mid-rune, appending an
// ellipsis when it cut anything.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
