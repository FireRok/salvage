package attest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// signedRecord builds a valid, Firerok-signed attestation under a throwaway key
// registered in FirerokKeys, mirroring exactly what the notary does server-side.
func signedRecord(t *testing.T) (*Record, func()) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	const keyID = "test-key"
	FirerokKeys[keyID] = base64.StdEncoding.EncodeToString(pub)

	report := []byte(`{"tool":"salvage","target":"prod-db","verdict":"pass"}`)
	sum := sha256.Sum256(report)
	rec := &Record{
		ID:           "att_deadbeef",
		TenantID:     "t_acme",
		Seq:          1,
		CreatedAt:    1751000000,
		Verdict:      "pass",
		ReportSha256: hex.EncodeToString(sum[:]),
		PrevHash:     strings.Repeat("0", 64),
		KeyID:        keyID,
		ReportRaw:    string(report),
	}
	eh := sha256.Sum256([]byte(canonicalEntry(rec)))
	rec.EntryHash = hex.EncodeToString(eh[:])
	rec.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, eh[:]))
	return rec, func() { delete(FirerokKeys, keyID) }
}

func TestVerifyGenuine(t *testing.T) {
	rec, cleanup := signedRecord(t)
	defer cleanup()
	checks, ok := Verify(rec)
	if !ok {
		t.Fatalf("genuine record failed to verify: %+v", checks)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
}

func TestVerifyToleratesUnpaddedKey(t *testing.T) {
	// Regression: a Firerok key stored WITHOUT base64 "=" padding (e.g. mangled by
	// a `cut -d=` extraction) must still verify, not silently report INVALID.
	rec, cleanup := signedRecord(t)
	defer cleanup()
	padded := FirerokKeys[rec.KeyID]
	FirerokKeys[rec.KeyID] = strings.TrimRight(padded, "=") // strip padding
	if _, ok := Verify(rec); !ok {
		t.Fatal("unpadded Firerok key failed to verify (b64 padding tolerance regressed)")
	}
}

func TestVerifyDetectsTamperedVerdict(t *testing.T) {
	rec, cleanup := signedRecord(t)
	defer cleanup()
	rec.Verdict = "fail" // flip pass→fail without re-signing
	if _, ok := Verify(rec); ok {
		t.Fatal("tampered verdict was not detected (chain hash should mismatch)")
	}
}

func TestVerifyDetectsTamperedTimestamp(t *testing.T) {
	rec, cleanup := signedRecord(t)
	defer cleanup()
	rec.CreatedAt = 1 // backdate
	if _, ok := Verify(rec); ok {
		t.Fatal("backdated timestamp was not detected")
	}
}

func TestVerifyDetectsTamperedReportBody(t *testing.T) {
	rec, cleanup := signedRecord(t)
	defer cleanup()
	rec.ReportRaw = `{"tool":"salvage","target":"prod-db","verdict":"fail"}` // swap the body
	if _, ok := Verify(rec); ok {
		t.Fatal("swapped report body was not detected (report hash should mismatch)")
	}
}

func TestVerifyRejectsForgedSignature(t *testing.T) {
	rec, cleanup := signedRecord(t)
	defer cleanup()
	// Re-sign the (valid) entry hash with a DIFFERENT key not in FirerokKeys.
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	eh, _ := hex.DecodeString(rec.EntryHash)
	rec.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(wrong, eh))
	if _, ok := Verify(rec); ok {
		t.Fatal("signature from a non-Firerok key was accepted")
	}
}

func TestVerifyRejectsUnknownKeyID(t *testing.T) {
	rec, cleanup := signedRecord(t)
	defer cleanup()
	rec.KeyID = "not-a-real-key"
	if _, ok := Verify(rec); ok {
		t.Fatal("unknown key_id was accepted")
	}
}
