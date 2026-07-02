package probe_test

import (
	"context"
	"testing"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
)

// Spec 0027 R2/R5: a command check with a byte-equal assertion propagates the
// keep_literal opt-in onto its result so report redaction can honor it; a
// check without the opt-in does not. The comparison itself always uses the raw
// stdout — redaction governs what is stored, not what is judged.
func TestCommandPropagatesKeepLiteral(t *testing.T) {
	out := "line one\nline two"
	c := config.Check{Name: "lit", Kind: "command", Command: "cat f", Equals: ps(out), KeepLiteral: true}
	r := checks.Run(context.Background(), fakeProber{out: out, exit: 0}, []config.Check{c})
	if len(r) != 1 {
		t.Fatalf("want 1 result, got %d", len(r))
	}
	if !r[0].OK {
		t.Errorf("equals over raw stdout should pass (got %q)", r[0].Got)
	}
	if !r[0].KeepLiteral {
		t.Error("KeepLiteral not propagated to the command result")
	}

	c.KeepLiteral = false
	r = checks.Run(context.Background(), fakeProber{out: out, exit: 0}, []config.Check{c})
	if r[0].KeepLiteral {
		t.Error("KeepLiteral set without the opt-in")
	}
}
