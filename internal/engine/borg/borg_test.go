package borg

import "testing"

func TestType(t *testing.T) {
	if (Engine{}).Type() != "borg" {
		t.Errorf("Type() = %q, want borg", Engine{}.Type())
	}
}

// TestRequireEnv covers the by-name secret precondition: an unset pass_env var
// is an error (surfaced as a spi.Fault by Restore), a set one passes.
func TestRequireEnv(t *testing.T) {
	if err := requireEnv(nil); err != nil {
		t.Errorf("requireEnv(nil) = %v, want nil", err)
	}
	t.Setenv("SALVAGE_BORG_TEST", "x")
	if err := requireEnv([]string{"SALVAGE_BORG_TEST"}); err != nil {
		t.Errorf("requireEnv(set) = %v, want nil", err)
	}
	if err := requireEnv([]string{"SALVAGE_BORG_TEST_UNSET"}); err == nil {
		t.Error("requireEnv(unset) = nil, want error")
	}
}
