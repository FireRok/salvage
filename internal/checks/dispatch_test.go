package checks

import (
	"context"
	"strings"
	"testing"

	"salvage.sh/internal/config"
)

// An empty kind must route to the sql evaluator — the historical single kind —
// so existing kind-less configs behave exactly as before.
func TestKindDefaultsToSQL(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{val: "5"}, []config.Check{{Name: "n", SQL: "x", ExpectMin: pf(1)}})
	if !r[0].OK {
		t.Errorf("empty kind should evaluate as sql and pass; detail=%s err=%s", r[0].Detail, r[0].Error)
	}
	// An explicit kind:"sql" is identical to the default.
	r2 := Run(ctx, fakeQ{val: "5"}, []config.Check{{Name: "n", Kind: "sql", SQL: "x", ExpectMin: pf(1)}})
	if !r2[0].OK {
		t.Errorf("explicit sql kind should pass; detail=%s err=%s", r2[0].Detail, r2[0].Error)
	}
}

// An unknown kind (no registered evaluator) must fail cleanly with a naming
// error, not panic — a misconfigured kind fails the verdict, not the process.
func TestUnknownKindFailsCleanly(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{val: "5"}, []config.Check{{Name: "n", Kind: "file_exists", SQL: "x", ExpectMin: pf(1)}})
	if r[0].OK {
		t.Fatal("unknown kind should not pass")
	}
	if r[0].Error == "" {
		t.Fatal("unknown kind should set Error")
	}
	if want := "file_exists"; !strings.Contains(r[0].Error, want) {
		t.Errorf("error should name the unknown kind %q; got %q", want, r[0].Error)
	}
}

// Severity must propagate through dispatch even when the kind is unknown, so the
// caller's advisory/required verdict rule still sees it.
func TestUnknownKindPropagatesSeverity(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, fakeQ{}, []config.Check{{Name: "n", Kind: "bogus", Severity: "advisory"}})
	if r[0].Severity != "advisory" {
		t.Errorf("severity = %q, want advisory", r[0].Severity)
	}
}

// notAQueryer is a target that satisfies the minimal RestoredTarget contract
// (Stop) but not Queryer — like a future non-SQL engine's target reaching a sql
// check. The sql evaluator must fail cleanly, not panic on the type-assert.
type notAQueryer struct{}

func (notAQueryer) Stop() error { return nil }

func TestSQLEvaluatorNonQueryerTargetFailsCleanly(t *testing.T) {
	ctx := context.Background()
	r := Run(ctx, notAQueryer{}, []config.Check{{Name: "n", SQL: "x", ExpectMin: pf(1)}})
	if r[0].OK {
		t.Fatal("a non-Queryer target should not pass a sql check")
	}
	if r[0].Error == "" {
		t.Fatal("a non-Queryer target should set Error")
	}
}
