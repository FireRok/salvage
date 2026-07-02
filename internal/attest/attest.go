// Package attest is the client side of the hosted attestation notary (spec 0012):
// it submits a signed report and verifies a returned attestation OFFLINE against
// Firerok's published public key. The notary service itself is proprietary; this
// package depends only on its open wire protocol.
package attest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// FirerokKeys maps a key id to Firerok's base64 raw Ed25519 public key. Baked in
// so `salvage verify` needs no network round-trip to trust a signature. Multiple
// entries support rotation (old attestations verify under their original key id).
//
// fk1 is the production attestation key for https://attest.salvage.sh. When
// rotating, ADD the new key id here (keep old ones so historical attestations
// still verify) and switch the notary's FIREROK_KEY_ID.
var FirerokKeys = map[string]string{
	"fk1": "KQBWrs49zi2/qG0bFx3aKXqsxRdSSZ2Bve65b3Mcg1I=",
}

// b64 decodes standard base64, tolerating missing "=" padding (some producers
// strip it). Without this, an unpadded key/signature silently fails to decode
// and verification reports INVALID for a genuine attestation.
func b64(s string) ([]byte, error) {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(s)
}

// Record is one attestation as returned by GET /v1/attestations/:id.
type Record struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	Seq          int64           `json:"seq"`
	CreatedAt    int64           `json:"created_at"`
	Method       string          `json:"method"`
	Target       string          `json:"target"`
	Verdict      string          `json:"verdict"`
	ReportSha256 string          `json:"report_sha256"`
	TenantPubkey string          `json:"tenant_pubkey"`
	TenantSigOK  bool            `json:"tenant_sig_ok"`
	PrevHash     string          `json:"prev_hash"`
	EntryHash    string          `json:"entry_hash"`
	Signature    string          `json:"signature"`
	KeyID        string          `json:"key_id"`
	ReportRaw    string          `json:"report_raw"`
	Report       json.RawMessage `json:"report"`
	URL          string          `json:"url"`
	Notice       string          `json:"notice"`
}

// Check is one verification step and its outcome.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// canonicalEntry builds the exact string the notary hashes into entry_hash. It
// MUST match the server (src/index.js) byte-for-byte.
func canonicalEntry(r *Record) string {
	return strings.Join([]string{
		"v1", r.PrevHash, r.ID, r.TenantID,
		strconv.FormatInt(r.Seq, 10), strconv.FormatInt(r.CreatedAt, 10),
		r.ReportSha256, r.Verdict,
	}, "\n")
}

// Verify checks an attestation offline: the entry hash binds the fields, Firerok's
// signature binds the entry hash, and the report bytes bind to report_sha256. It
// returns the per-step results and whether ALL required steps passed.
func Verify(r *Record) ([]Check, bool) {
	var checks []Check

	// 1. entry_hash commits to the record's fields.
	want := sha256.Sum256([]byte(canonicalEntry(r)))
	entryOK := hex.EncodeToString(want[:]) == r.EntryHash
	checks = append(checks, Check{"chain hash", entryOK, "entry_hash matches the signed fields"})

	// 2. Firerok's signature over the entry hash, under the baked-in public key.
	sigOK := false
	pubB64, known := FirerokKeys[r.KeyID]
	detail := "Firerok signature valid"
	switch {
	case !known:
		detail = "unknown key_id " + r.KeyID + " (not a trusted Firerok key)"
	default:
		pub, e1 := b64(pubB64)
		sig, e2 := b64(r.Signature)
		eh, e3 := hex.DecodeString(r.EntryHash)
		if e1 == nil && e2 == nil && e3 == nil && len(pub) == ed25519.PublicKeySize {
			sigOK = ed25519.Verify(ed25519.PublicKey(pub), eh, sig)
		}
		if !sigOK {
			detail = "Firerok signature INVALID"
		}
	}
	checks = append(checks, Check{"firerok signature", sigOK, detail})

	// 3. The stored report body hashes to report_sha256.
	reportOK := true
	if r.ReportRaw != "" {
		sum := sha256.Sum256([]byte(r.ReportRaw))
		reportOK = hex.EncodeToString(sum[:]) == r.ReportSha256
		checks = append(checks, Check{"report hash", reportOK, "report body matches report_sha256"})
	}

	// Tenant signature is verified server-side at submit; surface the recorded flag.
	if r.TenantPubkey != "" {
		checks = append(checks, Check{"tenant signature", r.TenantSigOK, "verified by the notary at submit time"})
	}

	return checks, entryOK && sigOK && reportOK
}

// Fetch GETs an attestation by id (or full URL) from the notary.
func Fetch(ctx context.Context, endpoint, idOrURL string) (*Record, error) {
	url := idOrURL
	if !strings.HasPrefix(idOrURL, "http") {
		url = strings.TrimRight(endpoint, "/") + "/v1/attestations/" + idOrURL
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch attestation: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("decode attestation: %w", err)
	}
	return &rec, nil
}

// SubmitResponse is the notary's reply to a successful submission.
type SubmitResponse struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	VerifyURL string `json:"verify_url"`
	Seq       int64  `json:"seq"`
	Verdict   string `json:"verdict"`
	EntryHash string `json:"entry_hash"`
	Signature string `json:"signature"`
	KeyID     string `json:"key_id"`
	Notice    string `json:"notice"`
	Error     string `json:"error"`
}

// Submit POSTs the exact report bytes (with the optional tenant signature) to the
// notary and returns its attestation record.
func Submit(ctx context.Context, endpoint, apiKey string, report []byte, sigB64, pubB64 string) (*SubmitResponse, error) {
	url := strings.TrimRight(endpoint, "/") + "/v1/attestations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(report))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if sigB64 != "" && pubB64 != "" {
		req.Header.Set("X-Salvage-Signature", sigB64)
		req.Header.Set("X-Salvage-Pubkey", pubB64)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out SubmitResponse
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusCreated {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("submit attestation: HTTP %d: %s", resp.StatusCode, msg)
	}
	return &out, nil
}

func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// --- device authorization flow (`salvage login`) + stored credentials -------

// DeviceCode is the notary's response to POST /device/code.
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceStart begins the device authorization flow.
func DeviceStart(ctx context.Context, endpoint string) (*DeviceCode, error) {
	url := strings.TrimRight(endpoint, "/") + "/device/code"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device/code: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var dc DeviceCode
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, err
	}
	return &dc, nil
}

// DeviceToken is the notary's successful response to POST /device/token: the
// minted key plus the org it is pinned to (org_name "personal" = the account's
// own ledger), so the caller can say where attestations will land.
type DeviceToken struct {
	APIKey  string `json:"api_key"`
	OrgID   string `json:"org_id"`
	OrgName string `json:"org_name"`
}

// DevicePoll polls once for the token. It returns the token on success, or a
// status string ("authorization_pending", "slow_down", or a terminal error) that
// the caller uses to decide whether to keep polling.
func DevicePoll(ctx context.Context, endpoint, deviceCode string) (tok *DeviceToken, status string, err error) {
	url := strings.TrimRight(endpoint, "/") + "/device/token"
	b, _ := json.Marshal(map[string]string{"device_code": deviceCode})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		DeviceToken
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if out.APIKey != "" {
		return &out.DeviceToken, "", nil
	}
	return nil, out.Error, nil
}

// Credentials is the locally stored login state (~/.salvage/credentials).
// OrgID/OrgName record which org the key is pinned to (empty on files written
// before org pinning existed — those keys resolve to the personal ledger).
type Credentials struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
	OrgID    string `json:"org_id,omitempty"`
	OrgName  string `json:"org_name,omitempty"`
}

func credsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".salvage", "credentials"), nil
}

// LoadCredentials reads stored login state; returns nil (no error) if absent.
func LoadCredentials() (*Credentials, error) {
	p, err := credsPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveCredentials writes login state with 0600 perms.
func SaveCredentials(c *Credentials) error {
	p, err := credsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// ClearCredentials removes stored login state (logout).
func ClearCredentials() error {
	p, err := credsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
