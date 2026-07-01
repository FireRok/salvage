// Package report builds the verdict for a restore-test and optionally signs it.
package report

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Report is the full outcome of a single restore-test.
type Report struct {
	Tool       string        `json:"tool"`
	Version    string        `json:"version"`
	Target     string        `json:"target"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	DurationMS int64         `json:"duration_ms"`
	Restore    RestoreResult `json:"restore"`
	Checks     []CheckResult `json:"checks"`
	Verdict    string        `json:"verdict"` // "pass" | "fail"
}

// RestoreResult records whether the backup came back at all.
type RestoreResult struct {
	OK         bool   `json:"ok"`
	Image      string `json:"image"`
	Database   string `json:"database"`
	DurationMS int64  `json:"duration_ms"`
	// Warnings records a non-fatal note — e.g. pg_restore skipped some objects
	// that already existed in the restore image (benign for extensions).
	Warnings string `json:"warnings,omitempty"`
	Error    string `json:"error,omitempty"`
}

// CheckResult records the outcome of one assertion against the restored data.
type CheckResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Severity is "required" or "advisory". A failing advisory check is recorded
	// but does not fail the verdict.
	Severity string `json:"severity,omitempty"`
	Got      string `json:"got,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Error    string `json:"error,omitempty"`
}

// New starts a report clock for a target.
func New(target, version string) *Report {
	return &Report{Tool: "salvage", Version: version, Target: target, StartedAt: time.Now()}
}

// Finalize stamps timings and computes the pass/fail verdict.
func (r *Report) Finalize() {
	r.FinishedAt = time.Now()
	r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	r.Verdict = "pass"
	if !r.Restore.OK {
		r.Verdict = "fail"
		return
	}
	for _, c := range r.Checks {
		// Only a failing required check fails the verdict. Failing advisory
		// checks are recorded but keep the verdict a pass. Severity defaults to
		// required, so a result with an empty severity still fails the verdict.
		if !c.OK && c.Severity != "advisory" {
			r.Verdict = "fail"
			return
		}
	}
}

// Passed reports whether the verdict is a pass.
func (r *Report) Passed() bool { return r.Verdict == "pass" }

// WriteJSON renders the report and, if path is non-empty, writes it there.
// It always returns the rendered bytes (used as the signing payload).
func (r *Report) WriteJSON(path string) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return b, err
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			return b, err
		}
	}
	return b, nil
}

// Signature is a local ed25519 signature over a report.
type Signature struct {
	Algorithm string    `json:"algorithm"`
	PublicKey string    `json:"public_key"`
	Signature string    `json:"signature"`
	SignedAt  time.Time `json:"signed_at"`
	Note      string    `json:"note"`
}

// Sign produces an ed25519 signature over payload using the key at keyPath,
// generating and persisting a new key if none exists.
//
// A local signature proves the report wasn't altered after signing. It does
// NOT prove the test was run independently — auditor-grade *independent*
// attestation is the hosted service on the roadmap.
func Sign(keyPath string, payload []byte) (*Signature, error) {
	priv, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, payload)
	pub := priv.Public().(ed25519.PublicKey)
	return &Signature{
		Algorithm: "ed25519",
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		Signature: base64.StdEncoding.EncodeToString(sig),
		SignedAt:  time.Now(),
		Note:      "Local signature: integrity only, not independent verification.",
	}, nil
}

// WriteSignature writes a signature sidecar as JSON.
func WriteSignature(path string, s *Signature) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil {
			return nil, fmt.Errorf("decode signing key: %w", derr)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("signing key has wrong size (%d, want %d)", len(raw), ed25519.PrivateKeySize)
		}
		return ed25519.PrivateKey(raw), nil
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(priv)
		if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	return priv, nil
}

// BackupVerdict records the restore-test of one backup during a last-known-good
// search over a pgBackRest chain.
type BackupVerdict struct {
	Label     string    `json:"label"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Verdict   string    `json:"verdict"` // "pass" | "fail"
	Reason    string    `json:"reason,omitempty"`
}

// LastGood is the result of walking a backup chain newest-first to find the
// freshest restorable backup. It identifies the freshest *restorable* point — it
// does not repair or extract anything.
type LastGood struct {
	Tool          string          `json:"tool"`
	Version       string          `json:"version"`
	Stanza        string          `json:"stanza"`
	Tested        []BackupVerdict `json:"tested"`                   // newest first, through the first pass
	RecoveryPoint *BackupVerdict  `json:"recovery_point,omitempty"` // the first pass; nil if none restore
}

// StanzaSummary is one stanza's chain summary from fleet discovery (metadata
// only — no restore is performed).
type StanzaSummary struct {
	Name          string     `json:"name"`
	Status        string     `json:"status"` // "ok" or the pgBackRest status message
	BackupCount   int        `json:"backup_count"`
	NewestLabel   string     `json:"newest_label,omitempty"`
	NewestBackup  *time.Time `json:"newest_backup,omitempty"`
	ConfigWritten string     `json:"config_written,omitempty"` // path of the emitted skeleton, if any
}

// Fleet is the result of enumerating every stanza in a pgBackRest repo. It is a
// cheap, metadata-only survey (from `pgbackrest info`); it does not restore.
type Fleet struct {
	Tool    string          `json:"tool"`
	Version string          `json:"version"`
	Stanzas []StanzaSummary `json:"stanzas"`
}
