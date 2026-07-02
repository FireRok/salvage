package mysql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
)

// fakeMySQL is a canned rowQueryer: introspection queries are answered from a
// script keyed by exact SQL, so the test also pins the generated SQL (MySQL's
// information_schema, backtick quoting — never Postgres catalogs).
type fakeMySQL struct {
	rows    map[string][][]string
	queried []string
}

func (f *fakeMySQL) Stop() error { return nil }

func (f *fakeMySQL) QueryRows(ctx context.Context, sql string) ([][]string, error) {
	f.queried = append(f.queried, sql)
	r, ok := f.rows[sql]
	if !ok {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}
	return r, nil
}

func discoverCfg() *config.Config {
	return &config.Config{Target: config.Target{
		Type:    "mysql",
		Source:  config.Source{Kind: "mysql", Path: "dump.sql"},
		Restore: config.Restore{Database: "appdb"},
	}}
}

func newFakeMySQL() *fakeMySQL {
	return &fakeMySQL{rows: map[string][][]string{
		`SELECT table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema = 'appdb' AND table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys') ORDER BY table_name`: {
			{"blobs"}, {"empty"}, {"events"}, {"orders"},
		},
		`SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = 'appdb' AND data_type IN ('timestamp', 'datetime') ORDER BY table_name, ordinal_position`: {
			{"events", "ts"},
			{"orders", "created_at"},
			{"orders", "updated_at"},
		},
		"SELECT count(*) FROM `appdb`.`blobs`":                                                      {{"7"}},
		"SELECT count(*) FROM `appdb`.`empty`":                                                      {{"0"}},
		"SELECT count(*) FROM `appdb`.`events`":                                                     {{"5000"}},
		"SELECT count(*) FROM `appdb`.`orders`":                                                     {{"100"}},
		"SELECT COALESCE(TIMESTAMPDIFF(SECOND, max(`ts`), NOW()), 0) FROM `appdb`.`events`":         {{"100000"}},
		"SELECT COALESCE(TIMESTAMPDIFF(SECOND, max(`updated_at`), NOW()), 0) FROM `appdb`.`orders`": {{"3600"}},
	}}
}

// Spec 0028 R3: MySQL discovery introspects MySQL's own information_schema and
// emits only sql-kind checks — required structural checks, advisory observed
// row-count floors and freshness bounds — with backtick-quoted identifiers and
// deterministic freshness-column selection (updated_at preferred).
func TestMySQLDiscover(t *testing.T) {
	fake := newFakeMySQL()
	cands, err := Engine{}.Discover(context.Background(), fake, discoverCfg())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	byName := map[string]spi.ScaffoldCandidate{}
	var names []string
	for _, c := range cands {
		byName[c.Check.Name] = c
		names = append(names, c.Check.Name)
	}
	want := []string{
		"server_reachable", "schema_present",
		"blobs_min_rows",
		"events_min_rows", "events_fresh",
		"orders_min_rows", "orders_fresh",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates = %v, want %v", names, want)
	}

	// Structural checks: required, never capped (no group).
	for _, n := range []string{"server_reachable", "schema_present"} {
		c := byName[n]
		if c.Check.Severity != "required" || c.Group != "" {
			t.Errorf("%s = severity %q group %q, want required and ungrouped", n, c.Check.Severity, c.Group)
		}
	}
	if sp := byName["schema_present"]; !strings.Contains(sp.Check.SQL, "information_schema.tables") ||
		!strings.Contains(sp.Check.SQL, "table_schema = 'appdb'") {
		t.Errorf("schema_present SQL = %q, want a MySQL information_schema count", sp.Check.SQL)
	}

	// Row-count floors: observed count, advisory, cap-grouped by table with the
	// row count as weight.
	orders := byName["orders_min_rows"]
	if orders.Check.SQL != "SELECT count(*) FROM `appdb`.`orders`" {
		t.Errorf("orders_min_rows SQL = %q, want a backtick-qualified count", orders.Check.SQL)
	}
	if orders.Check.ExpectMin == nil || *orders.Check.ExpectMin != 100 || orders.Check.Severity != "advisory" {
		t.Errorf("orders_min_rows = %+v, want advisory floor 100", orders.Check)
	}
	if orders.Group != "table" || orders.Subject != "orders" || orders.Weight != 100 {
		t.Errorf("orders_min_rows cap metadata = %q/%q/%d, want table/orders/100", orders.Group, orders.Subject, orders.Weight)
	}

	// Freshness: prefers updated_at over created_at, threshold = observed × margin
	// (floored to 24h). orders' observed 1h → floor 24h; events' 100000s × 5.
	of := byName["orders_fresh"]
	if of.Check.SQL != "SELECT max(`updated_at`) FROM `appdb`.`orders`" {
		t.Errorf("orders_fresh SQL = %q, want max(updated_at) (deterministic preference)", of.Check.SQL)
	}
	if of.Check.MaxAge == nil || of.Check.MaxAge.Std() != 24*time.Hour {
		t.Errorf("orders_fresh max_age = %v, want the 24h floor", of.Check.MaxAge)
	}
	ef := byName["events_fresh"]
	if ef.Check.MaxAge == nil || ef.Check.MaxAge.Std() != 500000*time.Second {
		t.Errorf("events_fresh max_age = %v, want 500000s (observed × 5)", ef.Check.MaxAge)
	}

	// The empty table contributes nothing; no query ever touched Postgres catalogs.
	for _, q := range fake.queried {
		if strings.Contains(q, "pg_") || strings.Contains(q, `"`) {
			t.Errorf("discovery issued a Postgres-shaped query: %s", q)
		}
	}
}

// A restored target without row queries is a clear engine-level error, not a
// panic — the orchestrator's capability gate already filtered unknown engines.
func TestMySQLDiscoverNeedsRowQueryer(t *testing.T) {
	type bare struct{ spi.RestoredTarget }
	_, err := Engine{}.Discover(context.Background(), bare{}, discoverCfg())
	if err == nil || !strings.Contains(err.Error(), "does not answer row queries") {
		t.Errorf("Discover(non-queryer) error = %v, want the row-query error", err)
	}
}
