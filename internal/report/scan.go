// Secret-pattern gate (spec 0027 R7): a defense-in-depth scan over the final
// report bytes for common credential shapes, run by `attest` before submission.
// It is the backstop for secrets *outside* the known-value set the Redact
// scrub removes — e.g. a credential the restore script fetched itself and never
// told Salvage about — and the last gate before the notary counter-signs.

package report

import "regexp"

// PatternMatch describes one credential-pattern hit in a report's bytes. It
// deliberately carries no matched text: the point of the gate is to keep the
// value out of durable artifacts, so it must not echo the value either.
type PatternMatch struct {
	// Pattern is the stable identifier of the credential shape that matched
	// (e.g. "aws-access-key-id").
	Pattern string
	// Count is how many times the pattern matched.
	Count int
}

// credentialPatterns are the high-signal credential shapes the gate refuses on
// by default. Kept small and precise: this is a refusal gate, and a noisy
// pattern would train operators to configure it down to warn (or off). The
// spec's open question on customer-supplied patterns is deferred; these cover
// the shapes named in spec 0027 (Design: "Optional secret-pattern scan").
var credentialPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	// AWS access-key ids (long-term AKIA…, temporary ASIA…).
	{"aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	// PEM-encoded private key material of any type (RSA/EC/OPENSSH/PKCS#8…).
	{"private-key-pem", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	// Bearer tokens ("Authorization: Bearer eyJ…" echoed by curl -v and such).
	{"bearer-token", regexp.MustCompile(`(?i)\bbearer[ \t]+[a-z0-9\-._~+/]{20,}=*`)},
	// URL-embedded credentials: scheme://user:password@host. Square brackets
	// are excluded from the password charset so a scrubbed credential —
	// scheme://user:[REDACTED:NAME]@host — does not re-trip the gate.
	{"url-credentials", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@"'` + "`" + `]+:[^@/\s"'` + "`" + `\[\]]{3,}@`)},
}

// ScanForCredentials scans report bytes for common credential shapes and
// returns one PatternMatch per pattern that hit (spec 0027 R7). An empty
// result means the gate passes. The caller (the attest path) decides between
// refusing and warning per configuration (config attest.secret_scan); this
// function only detects. Redaction markers ([REDACTED:NAME], [sha256:…]) never
// match any pattern, so a properly redacted report scans clean.
func ScanForCredentials(b []byte) []PatternMatch {
	var out []PatternMatch
	for _, p := range credentialPatterns {
		if n := len(p.re.FindAll(b, -1)); n > 0 {
			out = append(out, PatternMatch{Pattern: p.name, Count: n})
		}
	}
	return out
}
