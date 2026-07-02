// The `salvage verify` machine verdict object (spec 0026 R4).
//
// Two surfaces emit this shape: the CLI's `salvage verify -json` and the MCP
// server's salvage_verify tool (spec 0032 R5). It lives here — not in package
// main, which cannot be imported — so both serialize the one type and the JSON
// contract cannot drift.

package report

import "salvage.sh/internal/attest"

// VerifyCheck is one verification step in the verify verdict object.
type VerifyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// VerifyVerdict is the machine verdict object emitted by `salvage verify
// -json` (spec 0026 R4). It shares the report's schema_version counter.
type VerifyVerdict struct {
	SchemaVersion int           `json:"schema_version"`
	ID            string        `json:"id"`
	Target        string        `json:"target"`
	Verdict       string        `json:"verdict"`
	Seq           int64         `json:"seq"`
	KeyID         string        `json:"key_id"`
	Valid         bool          `json:"valid"`
	Checks        []VerifyCheck `json:"checks"`
	Notice        string        `json:"notice,omitempty"`
}

// NewVerifyVerdict maps a fetched attestation and its offline verification
// transcript into the verify verdict object. Checks is always non-nil, so an
// empty transcript serializes as [] (never null) and the object shape stays
// stable for consumers.
func NewVerifyVerdict(rec *attest.Record, checks []attest.Check, valid bool) *VerifyVerdict {
	v := &VerifyVerdict{
		SchemaVersion: SchemaVersion,
		ID:            rec.ID,
		Target:        rec.Target,
		Verdict:       rec.Verdict,
		Seq:           rec.Seq,
		KeyID:         rec.KeyID,
		Valid:         valid,
		Checks:        make([]VerifyCheck, 0, len(checks)),
		Notice:        rec.Notice,
	}
	for _, c := range checks {
		v.Checks = append(v.Checks, VerifyCheck{Name: c.Name, OK: c.OK, Detail: c.Detail})
	}
	return v
}
