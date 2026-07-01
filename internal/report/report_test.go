package report

import "testing"

func TestFinalizeAdvisoryFailureKeepsPass(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{
		{Name: "ok", OK: true, Severity: "required"},
		{Name: "warn", OK: false, Severity: "advisory"},
	}
	r.Finalize()
	if r.Verdict != "pass" {
		t.Errorf("verdict = %q, want pass (a failing advisory check must not fail the verdict)", r.Verdict)
	}
}

func TestFinalizeRequiredFailureFails(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{
		{Name: "warn", OK: false, Severity: "advisory"},
		{Name: "must", OK: false, Severity: "required"},
	}
	r.Finalize()
	if r.Verdict != "fail" {
		t.Errorf("verdict = %q, want fail (a failing required check must fail the verdict)", r.Verdict)
	}
}

// An empty severity defaults to required behaviour at verdict time.
func TestFinalizeEmptySeverityFailsLikeRequired(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = true
	r.Checks = []CheckResult{{Name: "legacy", OK: false}}
	r.Finalize()
	if r.Verdict != "fail" {
		t.Errorf("verdict = %q, want fail (empty severity must behave as required)", r.Verdict)
	}
}

func TestFinalizeFailedRestoreAlwaysFails(t *testing.T) {
	r := New("t", "test")
	r.Restore.OK = false
	r.Checks = []CheckResult{{Name: "warn", OK: false, Severity: "advisory"}}
	r.Finalize()
	if r.Verdict != "fail" {
		t.Errorf("verdict = %q, want fail (a failed restore always fails)", r.Verdict)
	}
}
