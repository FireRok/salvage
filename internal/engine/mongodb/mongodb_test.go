package mongodb

import (
	"context"
	"errors"
	"testing"

	"salvage.sh/internal/config"
	"salvage.sh/internal/ephemeral"
)

// Compile-time assertion: *ephemeral.MongoDB satisfies MongoQueryer, so the
// collection_count/doc_query evaluators in this package work against a
// MongoDB target with no adapter — mirroring the checks.Queryer assertion in
// internal/ephemeral/mysql_test.go (reversed here because MongoQueryer lives in
// this package, not internal/checks — see the note in
// internal/ephemeral/mongodb_test.go for why the assertion can't live there).
var _ MongoQueryer = (*ephemeral.MongoDB)(nil)

func TestType(t *testing.T) {
	if (Engine{}).Type() != "mongodb" {
		t.Errorf("Type() = %q, want mongodb", Engine{}.Type())
	}
}

// TestRequireEnv covers the by-name secret precondition: an unset pass_env var
// is an error (surfaced as a spi.Fault by Restore), a set one passes, and no
// pass_env at all is fine (MongoDB v1 has none required) — mirroring
// internal/engine/mysql's TestRequireEnv.
func TestRequireEnv(t *testing.T) {
	if err := requireEnv(nil); err != nil {
		t.Errorf("requireEnv(nil) = %v, want nil", err)
	}
	t.Setenv("SALVAGE_MONGODB_TEST", "x")
	if err := requireEnv([]string{"SALVAGE_MONGODB_TEST"}); err != nil {
		t.Errorf("requireEnv(set) = %v, want nil", err)
	}
	if err := requireEnv([]string{"SALVAGE_MONGODB_TEST_UNSET"}); err == nil {
		t.Error("requireEnv(unset) = nil, want error")
	}
}

// fakeQueryer is a minimal MongoQueryer for exercising the evaluators without
// Docker — analogous to how the sql/probe evaluators are tested against a
// hand-rolled fake rather than a live database.
type fakeQueryer struct {
	count    int64
	countErr error
	field    string
	fieldErr error

	gotCollection string
	gotFilter     string
	gotField      string
}

func (f *fakeQueryer) CountDocuments(_ context.Context, collection, filter string) (int64, error) {
	f.gotCollection, f.gotFilter = collection, filter
	return f.count, f.countErr
}

func (f *fakeQueryer) FindOneField(_ context.Context, collection, filter, field string) (string, error) {
	f.gotCollection, f.gotFilter, f.gotField = collection, filter, field
	return f.field, f.fieldErr
}

func TestEvalCollectionCount_WrongTarget(t *testing.T) {
	res := evalCollectionCount(context.Background(), "not-a-mongo-target", config.Check{Name: "x"})
	if res.OK {
		t.Fatal("expected failing result for a non-MongoQueryer target")
	}
	if res.Error == "" {
		t.Fatal("expected an error message for a non-MongoQueryer target")
	}
}

func TestEvalDocQuery_WrongTarget(t *testing.T) {
	res := evalDocQuery(context.Background(), 42, config.Check{Name: "x"})
	if res.OK {
		t.Fatal("expected failing result for a non-MongoQueryer target")
	}
	if res.Error == "" {
		t.Fatal("expected an error message for a non-MongoQueryer target")
	}
}

func float64p(v float64) *float64 { return &v }
func strp(s string) *string       { return &s }

func TestEvalCollectionCount_MissingCollection(t *testing.T) {
	res := evalCollectionCount(context.Background(), &fakeQueryer{}, config.Check{Name: "x", ExpectMin: float64p(1)})
	if res.OK || res.Error == "" {
		t.Fatal("expected a failing result when collection is unset")
	}
}

func TestEvalCollectionCount_Bounds(t *testing.T) {
	q := &fakeQueryer{count: 5}
	res := evalCollectionCount(context.Background(), q, config.Check{
		Name: "x", Collection: "orders", Filter: `{"status":"active"}`,
		ExpectMin: float64p(1), ExpectMax: float64p(10),
	})
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
	if res.Got != "5" {
		t.Errorf("got = %q, want 5", res.Got)
	}
	if q.gotCollection != "orders" || q.gotFilter != `{"status":"active"}` {
		t.Errorf("CountDocuments called with (%q, %q)", q.gotCollection, q.gotFilter)
	}

	q2 := &fakeQueryer{count: 100}
	res2 := evalCollectionCount(context.Background(), q2, config.Check{
		Name: "x", Collection: "orders", ExpectMax: float64p(10),
	})
	if res2.OK {
		t.Fatal("expected failing result when count exceeds expect_max")
	}
}

func TestEvalCollectionCount_Equals(t *testing.T) {
	q := &fakeQueryer{count: 3}
	res := evalCollectionCount(context.Background(), q, config.Check{
		Name: "x", Collection: "orders", Equals: strp("3"),
	})
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
}

func TestEvalCollectionCount_QueryError(t *testing.T) {
	q := &fakeQueryer{countErr: errors.New("boom")}
	res := evalCollectionCount(context.Background(), q, config.Check{
		Name: "x", Collection: "orders", ExpectMin: float64p(1),
	})
	if res.OK || res.Error == "" {
		t.Fatal("expected a failing result on query error")
	}
}

func TestEvalDocQuery_MissingFields(t *testing.T) {
	cases := []config.Check{
		{Name: "x", Filter: `{"a":1}`, Field: "status", Equals: strp("shipped")},
		{Name: "x", Collection: "orders", Field: "status", Equals: strp("shipped")},
		{Name: "x", Collection: "orders", Filter: `{"a":1}`, Equals: strp("shipped")},
	}
	for _, c := range cases {
		res := evalDocQuery(context.Background(), &fakeQueryer{}, c)
		if res.OK || res.Error == "" {
			t.Errorf("expected failing result for incomplete check %+v", c)
		}
	}
}

func TestEvalDocQuery_Equals(t *testing.T) {
	q := &fakeQueryer{field: "shipped"}
	res := evalDocQuery(context.Background(), q, config.Check{
		Name: "x", Collection: "orders", Filter: `{"_id":"o1"}`, Field: "status", Equals: strp("shipped"),
	})
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}
	if q.gotCollection != "orders" || q.gotFilter != `{"_id":"o1"}` || q.gotField != "status" {
		t.Errorf("FindOneField called with (%q, %q, %q)", q.gotCollection, q.gotFilter, q.gotField)
	}

	q2 := &fakeQueryer{field: "pending"}
	res2 := evalDocQuery(context.Background(), q2, config.Check{
		Name: "x", Collection: "orders", Filter: `{"_id":"o1"}`, Field: "status", Equals: strp("shipped"),
	})
	if res2.OK {
		t.Fatal("expected failing result when field value does not equal expectation")
	}
}

func TestEvalDocQuery_NumericBounds(t *testing.T) {
	q := &fakeQueryer{field: "3"}
	res := evalDocQuery(context.Background(), q, config.Check{
		Name: "x", Collection: "meta", Filter: `{"_id":"m1"}`, Field: "version",
		ExpectMin: float64p(1), ExpectMax: float64p(5),
	})
	if !res.OK {
		t.Fatalf("expected OK, got %+v", res)
	}

	q2 := &fakeQueryer{field: "not-a-number"}
	res2 := evalDocQuery(context.Background(), q2, config.Check{
		Name: "x", Collection: "meta", Filter: `{"_id":"m1"}`, Field: "version",
		ExpectMin: float64p(1),
	})
	if res2.OK || res2.Error == "" {
		t.Fatal("expected a failing result when the field value is not numeric")
	}
}

func TestEvalDocQuery_FindError(t *testing.T) {
	q := &fakeQueryer{fieldErr: errors.New("no document matched")}
	res := evalDocQuery(context.Background(), q, config.Check{
		Name: "x", Collection: "orders", Filter: `{"_id":"missing"}`, Field: "status", Equals: strp("shipped"),
	})
	if res.OK || res.Error == "" {
		t.Fatal("expected a failing result on find error")
	}
}
