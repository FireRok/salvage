// Package alert fires the client-side alert hooks of spec 0030 (realizing
// spec 0007 R4): after a run's verdict is finalized and its report is written,
// the operator-configured `alerts.on_fail` / `alerts.on_success` hook — a
// command or an http(s) URL — is invoked with the run's report JSON.
//
// Hooks are best-effort and secondary (spec 0030 R2): exit-code composition
// stays primary, a hook that errors or times out is logged by the caller and
// never changes the run's exit code, and no daemon is introduced (spec 0007
// R1) — the hook runs inline in the one-shot process, which then exits.
//
// Secrets are handled by reference (spec 0030 R7, consistent with spec 0003):
// a URL hook carries a token as a `*_ref=env:NAME` query parameter, resolved
// from the environment only at delivery time, so no secret is ever written
// into salvage.yaml — and no error path here echoes a resolved value.
package alert

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultTimeout bounds a hook invocation when the config sets no
// alerts.timeout (spec 0030 R2: "a bounded timeout").
const DefaultTimeout = 30 * time.Second

// Hook is one configured alert hook, ready to fire.
type Hook struct {
	// Spec is the configured hook value: a command line (run via `sh -c`) or
	// an http(s):// URL. This is the by-reference form — it may contain
	// `*_ref=env:NAME` parameters but never a literal secret — so it is safe
	// to echo in error messages.
	Spec string
	// Timeout bounds the invocation; zero or negative means DefaultTimeout.
	Timeout time.Duration
	// Getenv resolves env references at delivery time (nil = os.Getenv).
	Getenv func(string) string
	// Stderr receives a command hook's combined output (nil = os.Stderr), so
	// stdout stays a single clean JSON document under `run -json`.
	Stderr io.Writer
}

// IsURL reports whether spec is a URL hook. https is the documented form
// (spec 0030 R1); plain http is accepted too — for loopback receivers and so
// a value that *looks* like a URL is never handed to a shell by surprise.
func IsURL(spec string) bool {
	return strings.HasPrefix(spec, "https://") || strings.HasPrefix(spec, "http://")
}

// Fire delivers the report JSON to the hook: POSTed with
// Content-Type: application/json for a URL hook, piped to stdin for a command
// hook (with the report file path, when one exists, in $SALVAGE_REPORT).
// The reportJSON bytes are the exact redacted bytes report.WriteJSON produced
// — the caller must never serialize a report by another path (spec 0027).
func (h Hook) Fire(ctx context.Context, reportJSON []byte, reportPath string) error {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if IsURL(h.Spec) {
		return h.fireURL(ctx, reportJSON)
	}
	return h.fireCommand(ctx, reportJSON, reportPath)
}

// fireURL POSTs the report JSON to the resolved hook URL. Every error path
// names the *configured* spec (refs only), never the resolved URL — the
// resolved query may carry a token, and secrets must never surface in errors
// or reports (spec 0030 R7).
func (h Hook) fireURL(ctx context.Context, payload []byte) error {
	getenv := h.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	resolved, err := resolveURL(h.Spec, getenv)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolved, bytes.NewReader(payload))
	if err != nil {
		// err would echo the resolved URL; report the configured form.
		return fmt.Errorf("hook %s: invalid URL", h.Spec)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A *url.Error wraps the resolved URL string; unwrap to the transport
		// error beneath it before echoing anything.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("hook POST %s: %w", h.Spec, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hook POST %s: status %s", h.Spec, resp.Status)
	}
	return nil
}

// fireCommand runs the hook command via `sh -c` with the report JSON on stdin
// and, when a report file was written, its path in $SALVAGE_REPORT — the
// Unix-composition contract of spec 0030 R1.
func (h Hook) fireCommand(ctx context.Context, payload []byte, reportPath string) error {
	out := h.Stderr
	if out == nil {
		out = os.Stderr
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", h.Spec)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Env = os.Environ()
	if reportPath != "" {
		cmd.Env = append(cmd.Env, "SALVAGE_REPORT="+reportPath)
	}
	// If out is a pipe (tests, captured stderr), Wait would otherwise linger
	// on descendants holding it after the timeout kill; bound that too.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("hook command timed out (alerts.timeout): %w", ctx.Err())
		}
		return fmt.Errorf("hook command: %w", err)
	}
	return nil
}

// ValidateSpec applies the load-time rules for a hook spec (spec 0030 R7).
// A command spec has no rules here — the shell interprets it at fire time. A
// URL spec must parse, must not embed credentials (user:pass@), and every
// `*_ref` query parameter must be a well-formed env reference (`env:NAME`),
// never a literal value.
func ValidateSpec(spec string) error {
	if !IsURL(spec) {
		return nil
	}
	u, err := url.Parse(spec)
	if err != nil {
		return fmt.Errorf("invalid hook URL: %v", err)
	}
	if u.User != nil {
		return errors.New("hook URL must not embed credentials (user:pass@); pass the token by reference (token_ref=env:NAME) per spec 0030")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid hook URL query: %v", err)
	}
	for _, name := range refParamNames(q) {
		for _, v := range q[name] {
			if _, ok := envRef(v); !ok {
				return fmt.Errorf("hook URL parameter %q must be an env reference (%s=env:NAME), never a literal secret (spec 0030)", name, name)
			}
		}
	}
	return nil
}

// RefEnvNames returns the env-var names a URL hook references via
// `*_ref=env:NAME` parameters, sorted. The caller folds them into the
// config's known-secret set (config.SecretEnvNames → spec 0027 R3) so a hook
// token can never surface in a report even if it leaks into captured output.
func RefEnvNames(spec string) []string {
	if !IsURL(spec) {
		return nil
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil
	}
	var names []string
	q := u.Query()
	for _, name := range refParamNames(q) {
		for _, v := range q[name] {
			if envName, ok := envRef(v); ok {
				names = append(names, envName)
			}
		}
	}
	sort.Strings(names)
	return names
}

// resolveURL expands the by-reference query parameters of a URL hook at
// delivery time: `token_ref=env:NAME` becomes `token=<value of $NAME>`. The
// plaintext exists only in the outgoing request; errors name the env var,
// never a value.
func resolveURL(spec string, getenv func(string) string) (string, error) {
	u, err := url.Parse(spec)
	if err != nil {
		return "", fmt.Errorf("hook URL: %v", err)
	}
	q := u.Query()
	refs := refParamNames(q)
	if len(refs) == 0 {
		return spec, nil
	}
	for _, name := range refs {
		for _, v := range q[name] {
			envName, ok := envRef(v)
			if !ok {
				return "", fmt.Errorf("hook URL parameter %q is not an env reference (%s=env:NAME)", name, name)
			}
			val := getenv(envName)
			if val == "" {
				return "", fmt.Errorf("hook URL parameter %q references env %s, which is unset", name, envName)
			}
			q.Add(strings.TrimSuffix(name, "_ref"), val)
		}
		q.Del(name)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// refParamNames returns the `*_ref` parameter names of a parsed query,
// sorted — collected up front so callers can rewrite q while iterating.
func refParamNames(q url.Values) []string {
	var names []string
	for name := range q {
		if strings.HasSuffix(name, "_ref") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// envRef parses a by-reference parameter value of the form "env:NAME".
func envRef(v string) (string, bool) {
	name, ok := strings.CutPrefix(v, "env:")
	if !ok || name == "" {
		return "", false
	}
	return name, true
}
