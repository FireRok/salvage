package mysql

import "testing"

func TestType(t *testing.T) {
	if (Engine{}).Type() != "mysql" {
		t.Errorf("Type() = %q, want mysql", Engine{}.Type())
	}
}

// TestRequireEnv covers the by-name secret precondition: an unset pass_env var
// is an error (surfaced as a spi.Fault by Restore), a set one passes, and no
// pass_env at all is fine (MySQL v1 has none required).
func TestRequireEnv(t *testing.T) {
	if err := requireEnv(nil); err != nil {
		t.Errorf("requireEnv(nil) = %v, want nil", err)
	}
	t.Setenv("SALVAGE_MYSQL_TEST", "x")
	if err := requireEnv([]string{"SALVAGE_MYSQL_TEST"}); err != nil {
		t.Errorf("requireEnv(set) = %v, want nil", err)
	}
	if err := requireEnv([]string{"SALVAGE_MYSQL_TEST_UNSET"}); err == nil {
		t.Error("requireEnv(unset) = nil, want error")
	}
}
