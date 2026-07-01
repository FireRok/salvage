package checks

import (
	"context"
	"testing"
	"time"

	"salvage.sh/internal/config"
)

type fakeQ struct {
	val string
	err error
}

func (f fakeQ) Query(ctx context.Context, sql string) (string, error) { return f.val, f.err }

func pf(f float64) *float64 { return &f }
func ps(s string) *string   { return &s }
func pb(b bool) *bool       { return &b }
func pd(d time.Duration) *config.Duration {
	x := config.Duration(d)
	return &x
}

func TestExpectMin(t *testing.T) {
	ctx := context.Background()
	pass := Run(ctx, fakeQ{val: "5"}, []config.Check{{Name: "n", SQL: "x", ExpectMin: pf(1)}})
	if !pass[0].OK {
		t.Errorf("want pass, got fail (detail=%s)", pass[0].Detail)
	}
	fail := Run(ctx, fakeQ{val: "0"}, []config.Check{{Name: "n", SQL: "x", ExpectMin: pf(1)}})
	if fail[0].OK {
		t.Error("want fail for 0 < min 1")
	}
}

func TestEquals(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{val: "42"}, []config.Check{{Name: "n", SQL: "x", Equals: ps("42")}})
	if !r[0].OK {
		t.Errorf("want equals pass, got fail (detail=%s)", r[0].Detail)
	}
}

func TestMaxAge(t *testing.T) {
	ctx := context.Background()
	recent := time.Now().Add(-1 * time.Hour).Format("2006-01-02 15:04:05")
	r := Run(ctx, fakeQ{val: recent}, []config.Check{{Name: "n", SQL: "x", MaxAge: pd(24 * time.Hour)}})
	if !r[0].OK {
		t.Errorf("want recent within 24h to pass (detail=%s err=%s)", r[0].Detail, r[0].Error)
	}
	old := time.Now().Add(-72 * time.Hour).Format("2006-01-02 15:04:05")
	r2 := Run(ctx, fakeQ{val: old}, []config.Check{{Name: "n", SQL: "x", MaxAge: pd(24 * time.Hour)}})
	if r2[0].OK {
		t.Error("want stale (72h) to fail against 24h max")
	}
}

func TestBool(t *testing.T) {
	ctx := context.Background()
	// Postgres renders true as "t"; the predicate must hold.
	pass := Run(ctx, fakeQ{val: "t"}, []config.Check{{Name: "n", SQL: "x", Bool: pb(true)}})
	if !pass[0].OK {
		t.Errorf("want bool pass for t, got fail (detail=%s err=%s)", pass[0].Detail, pass[0].Error)
	}
	// "false"/"0" likewise satisfy a Bool:false expectation.
	for _, v := range []string{"f", "false", "0"} {
		r := Run(ctx, fakeQ{val: v}, []config.Check{{Name: "n", SQL: "x", Bool: pb(false)}})
		if !r[0].OK {
			t.Errorf("want bool pass for %q against false (detail=%s err=%s)", v, r[0].Detail, r[0].Error)
		}
	}
	// Mismatch: query returns false but we expect true.
	fail := Run(ctx, fakeQ{val: "false"}, []config.Check{{Name: "n", SQL: "x", Bool: pb(true)}})
	if fail[0].OK {
		t.Error("want bool fail for false against true")
	}
}

func TestBoolUnparseable(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{val: "maybe"}, []config.Check{{Name: "n", SQL: "x", Bool: pb(true)}})
	if r[0].OK || r[0].Error == "" {
		t.Errorf("want a non-boolean scalar to surface as an error (ok=%v err=%q)", r[0].OK, r[0].Error)
	}
}

func TestSeverityPropagates(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{val: "5"}, []config.Check{{Name: "n", SQL: "x", ExpectMin: pf(1), Severity: "advisory"}})
	if r[0].Severity != "advisory" {
		t.Errorf("severity = %q, want advisory", r[0].Severity)
	}
}

func TestQueryError(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{err: context.DeadlineExceeded}, []config.Check{{Name: "n", SQL: "x", Equals: ps("1")}})
	if r[0].OK || r[0].Error == "" {
		t.Error("want a query error to surface as a non-OK result with Error set")
	}
}
