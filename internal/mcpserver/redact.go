package mcpserver

import (
	"encoding/json"
	"os"
	"regexp"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// This file is the MCP server's secret-hygiene gate (spec 0032 R6): every byte
// a tool call returns — success payloads AND error messages — passes through
// here before leaving the server, because MCP output lands directly in an LLM
// context window.
//
// The known-secret VALUE scrub delegates to spec 0027's shared implementation
// (report.NewScrubber), so the report boundary and the MCP boundary cannot
// diverge on the mechanics (exact-value replacement, longest-value-first
// ordering). What stays here is the MCP-boundary-specific layer:
//
//   - which secrets are collected (per-call: config-forwarded env values,
//     the attest API key, the stored login credential);
//   - the secret-SHAPED pattern pass (bearer tokens, DSN userinfo passwords)
//     — the report path detects these via report.ScanForCredentials and
//     refuses, but a tool result must be delivered, so here they are scrubbed;
//   - the structural JSON walk that applies the scrub to whole payloads.
//
// Secrets are deliberately registered unnamed: every scrubbed value renders as
// the same bare [REDACTED] marker, the shape this boundary has always emitted.

const redactedMarker = "[REDACTED]"

// minSecretLen mirrors the shared redaction path's threshold (spec 0027 R3):
// values shorter than this are not meaningful secrets and scrubbing them would
// shred ordinary text.
const minSecretLen = 4

var (
	// bearerRe matches Authorization-style bearer credentials wherever they
	// leak into a string (e.g. an HTTP error echoing a request header).
	bearerRe = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{4,}`)
	// dsnPassRe matches the password portion of URL userinfo, e.g.
	// postgres://user:secret@host/db.
	dsnPassRe = regexp.MustCompile(`(://[^/:@\s]+:)[^@\s]+(@)`)
)

// redactor scrubs known secret values and secret-shaped patterns from strings
// and structured payloads.
type redactor struct {
	secrets []report.Secret
	scrub   func(string) string // built lazily from secrets; nil = stale
}

// newRedactor builds a redactor for one tool call. It collects every secret
// value resolvable at this point: values of the env vars the config forwards
// by name (source.pass_env, restore.env — the exact channels spec 0027 names),
// the attest API key env var, and the stored login credential. cfg may be nil
// (no config in play); the environment-level secrets are still collected.
func newRedactor(cfg *config.Config) *redactor {
	r := &redactor{}
	if cfg != nil {
		for _, name := range cfg.Target.Source.PassEnv {
			r.addSecret(os.Getenv(name))
		}
		for _, name := range cfg.Target.Restore.Env {
			r.addSecret(os.Getenv(name))
		}
		if cfg.Attest != nil && cfg.Attest.APIKeyEnv != "" {
			r.addSecret(os.Getenv(cfg.Attest.APIKeyEnv))
		}
	}
	r.addSecret(os.Getenv("SALVAGE_ATTEST_KEY"))
	if creds, err := loadCredentials(); err == nil && creds != nil {
		r.addSecret(creds.APIKey)
	}
	return r
}

// addSecret registers a literal value to scrub. Values shorter than
// minSecretLen are ignored: they are not meaningful secrets and would shred
// ordinary text. The value is registered unnamed so it redacts to the bare
// [REDACTED] marker (see the file comment).
func (r *redactor) addSecret(s string) {
	if len(s) < minSecretLen {
		return
	}
	r.secrets = append(r.secrets, report.Secret{Value: s})
	r.scrub = nil // rebuild on next use
}

// scrubFn returns the shared known-secret scrub for the current secret set.
func (r *redactor) scrubFn() func(string) string {
	if r.scrub == nil {
		r.scrub = report.NewScrubber(r.secrets)
	}
	return r.scrub
}

// redactString scrubs one string: literal secret values first (shared spec
// 0027 scrub), then the MCP boundary's secret-shaped patterns.
func (r *redactor) redactString(s string) string {
	s = r.scrubFn()(s)
	s = bearerRe.ReplaceAllString(s, "Bearer "+redactedMarker)
	s = dsnPassRe.ReplaceAllString(s, "${1}"+redactedMarker+"${2}")
	return s
}

// redactPayload scrubs an arbitrary payload by normalizing it through JSON and
// walking every string value. Working on decoded strings (not encoded bytes)
// means a secret containing JSON metacharacters is still matched.
func (r *redactor) redactPayload(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		return nil, err
	}
	return r.walk(decoded), nil
}

func (r *redactor) walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = r.walk(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = r.walk(e)
		}
		return t
	case string:
		return r.redactString(t)
	default:
		return v
	}
}
