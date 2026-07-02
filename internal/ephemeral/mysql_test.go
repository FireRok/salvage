package ephemeral

import (
	"testing"

	"salvage.sh/internal/checks"
)

// Compile-time assertion: *MySQL satisfies checks.Queryer, so the existing sql
// check evaluator (internal/checks/sql.go) works against a MySQL target with no
// new evaluator code (spec 0024 R2).
var _ checks.Queryer = (*MySQL)(nil)

// TestFirstField covers the pure parser that reduces mysql -N -B output (tab-
// separated, newline-terminated) to the single scalar checks.Queryer expects —
// mirroring how parseRows/psql -tAqc feed Postgres's Query.
func TestFirstField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single scalar", "1\n", "1"},
		{"no trailing newline", "42", "42"},
		{"only first column of first row", "5\textra\n", "5"},
		{"only first row when multiple", "3\n4\n5\n", "3"},
		{"empty output", "", ""},
		{"whitespace only", "   \n", ""},
		{"timestamp scalar", "2026-07-01 12:00:00\n", "2026-07-01 12:00:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstField(tc.in); got != tc.want {
				t.Errorf("firstField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
