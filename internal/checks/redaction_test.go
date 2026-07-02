package checks

import (
	"testing"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// Spec 0027 R2/R5: EvaluateScalar propagates the keep_literal opt-in onto the
// result for report redaction, and never alters the evaluation itself.
func TestEvaluateScalarPropagatesKeepLiteral(t *testing.T) {
	eq := "expected value"
	res := report.CheckResult{}
	c := config.Check{Equals: &eq, KeepLiteral: true}
	if !EvaluateScalar("expected value", c, &res) {
		t.Fatal("equals expectation should be handled")
	}
	if !res.OK {
		t.Errorf("equals should pass (got %q)", res.Got)
	}
	if !res.KeepLiteral {
		t.Error("KeepLiteral not propagated to the scalar result")
	}

	res = report.CheckResult{}
	c.KeepLiteral = false
	EvaluateScalar("expected value", c, &res)
	if res.KeepLiteral {
		t.Error("KeepLiteral set without the opt-in")
	}
}
