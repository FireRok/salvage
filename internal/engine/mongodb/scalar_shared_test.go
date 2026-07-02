package mongodb

import (
	"context"
	"testing"
	"time"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
)

// sqlFake is a minimal checks.Queryer so the same expectation cases can run
// through the sql kind.
type sqlFake struct{ val string }

func (f sqlFake) Query(_ context.Context, _ string) (string, error) { return f.val, nil }

func durp(d time.Duration) *config.Duration {
	x := config.Duration(d)
	return &x
}

// One suite, both kinds (backlog S7): every scalar-expectation case is run
// through the sql evaluator AND the doc_query evaluator via checks.Run, proving
// they share one code path — including max_age freshness (backlog S3), which
// doc_query gains from the shared evaluator.
func TestScalarExpectationsSharedAcrossKinds(t *testing.T) {
	ctx := context.Background()
	freshISO := time.Now().UTC().Add(-1 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	staleISO := time.Now().UTC().Add(-72 * time.Hour).Format("2006-01-02T15:04:05.000Z")

	cases := []struct {
		name    string
		scalar  string // the value both targets return
		expect  config.Check
		wantOK  bool
		wantErr bool // an evaluation error (unparseable scalar), not a mismatch
	}{
		{"expect_min pass", "5", config.Check{ExpectMin: float64p(1)}, true, false},
		{"expect_min fail", "0", config.Check{ExpectMin: float64p(1)}, false, false},
		{"expect_max fail", "100", config.Check{ExpectMax: float64p(10)}, false, false},
		{"bounds pass", "5", config.Check{ExpectMin: float64p(1), ExpectMax: float64p(10)}, true, false},
		{"bounds non-numeric", "oops", config.Check{ExpectMin: float64p(1)}, false, true},
		{"equals pass", "shipped", config.Check{Equals: strp("shipped")}, true, false},
		{"equals fail", "pending", config.Check{Equals: strp("shipped")}, false, false},
		{"max_age fresh", freshISO, config.Check{MaxAge: durp(24 * time.Hour)}, true, false},
		{"max_age stale", staleISO, config.Check{MaxAge: durp(24 * time.Hour)}, false, false},
		{"max_age unparseable", "not-a-time", config.Check{MaxAge: durp(24 * time.Hour)}, false, true},
	}

	for _, tc := range cases {
		// sql kind: expectation fields on a sql check against a Queryer.
		sqlCheck := tc.expect
		sqlCheck.Name = tc.name
		sqlCheck.SQL = "select x"
		sqlRes := checks.Run(ctx, sqlFake{val: tc.scalar}, []config.Check{sqlCheck})[0]

		// doc_query kind: the same expectation fields against a MongoQueryer.
		docCheck := tc.expect
		docCheck.Name = tc.name
		docCheck.Kind = "doc_query"
		docCheck.Collection = "orders"
		docCheck.Filter = `{"_id":"o1"}`
		docCheck.Field = "v"
		docRes := checks.Run(ctx, &fakeQueryer{field: tc.scalar}, []config.Check{docCheck})[0]

		for kind, res := range map[string]struct {
			OK            bool
			Error, Detail string
		}{
			"sql":       {sqlRes.OK, sqlRes.Error, sqlRes.Detail},
			"doc_query": {docRes.OK, docRes.Error, docRes.Detail},
		} {
			if res.OK != tc.wantOK {
				t.Errorf("%s [%s]: OK = %v, want %v (detail=%s err=%s)", tc.name, kind, res.OK, tc.wantOK, res.Detail, res.Error)
			}
			if tc.wantErr && res.Error == "" {
				t.Errorf("%s [%s]: expected an evaluation error", tc.name, kind)
			}
			if !tc.wantErr && res.Error != "" {
				t.Errorf("%s [%s]: unexpected error %q", tc.name, kind, res.Error)
			}
		}

		// Shared code path means identical detail/error strings, not just the
		// same verdict.
		if sqlRes.Detail != docRes.Detail {
			t.Errorf("%s: detail diverged between kinds: sql=%q doc_query=%q", tc.name, sqlRes.Detail, docRes.Detail)
		}
		if sqlRes.Error != docRes.Error {
			t.Errorf("%s: error diverged between kinds: sql=%q doc_query=%q", tc.name, sqlRes.Error, docRes.Error)
		}
	}
}

// doc_query freshness end to end at the evaluator level (backlog S3): a
// timestamp field within the window passes, a stale one fails the verdict, and
// a non-timestamp scalar surfaces as an evaluation error.
func TestEvalDocQuery_MaxAge(t *testing.T) {
	ctx := context.Background()
	base := config.Check{
		Name: "latest_order_recent", Collection: "orders",
		Filter: `{"_id":"latest"}`, Field: "created_at",
		MaxAge: durp(24 * time.Hour),
	}

	fresh := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	if res := evalDocQuery(ctx, &fakeQueryer{field: fresh}, base); !res.OK {
		t.Errorf("fresh timestamp should pass: %+v", res)
	}

	stale := time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02T15:04:05.000Z")
	if res := evalDocQuery(ctx, &fakeQueryer{field: stale}, base); res.OK {
		t.Errorf("stale timestamp should fail the verdict: %+v", res)
	} else if res.Error != "" {
		t.Errorf("staleness is a mismatch, not an evaluation error: %+v", res)
	}

	if res := evalDocQuery(ctx, &fakeQueryer{field: "shipped"}, base); res.OK || res.Error == "" {
		t.Errorf("non-timestamp field under max_age should surface as an error: %+v", res)
	}
}
