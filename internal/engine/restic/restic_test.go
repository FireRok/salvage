package restic

import "testing"

func TestType(t *testing.T) {
	if (Engine{}).Type() != "restic" {
		t.Errorf("Type() = %q, want restic", Engine{}.Type())
	}
}
