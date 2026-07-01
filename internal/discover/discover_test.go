package discover

import (
	"context"
	"strings"
	"testing"
	"time"

	"salvage.sh/internal/config"
)

// fakeQueryer answers QueryRows by matching the issued SQL against substrings.
// The first matching rule (in registration order) wins, so more specific rules
// must be registered before broader ones.
type fakeQueryer struct {
	rules []rule
	seen  []string
}

type rule struct {
	match string
	rows  [][]string
}

func (f *fakeQueryer) on(match string, rows [][]string) *fakeQueryer {
	f.rules = append(f.rules, rule{match: match, rows: rows})
	return f
}

func (f *fakeQueryer) QueryRows(_ context.Context, sql string) ([][]string, error) {
	f.seen = append(f.seen, sql)
	for _, r := range f.rules {
		if strings.Contains(sql, r.match) {
			return r.rows, nil
		}
	}
	// Unmatched count/age queries default to a single zero so Introspect does
	// not error on tables a test did not bother to stub.
	return [][]string{{"0"}}, nil
}

// baseFake stubs a cluster with two databases, a hypertable "metrics" (time
// column "ts", observed 1h old), a plain table "orders" with a created_at
// timestamptz (observed 2h old) and rows, an empty table "audit", and the
// timescaledb extension installed.
func baseFake() *fakeQueryer {
	f := &fakeQueryer{}
	f.on("FROM pg_database WHERE", [][]string{{"app"}, {"postgres"}})
	f.on("information_schema.tables WHERE", [][]string{
		{"public", "metrics"},
		{"public", "orders"},
		{"public", "audit"},
	})
	f.on("information_schema.columns WHERE", [][]string{
		{"public", "metrics", "ts", "timestamp with time zone"},
		{"public", "orders", "created_at", "timestamp with time zone"},
		{"public", "audit", "logged_at", "timestamp without time zone"},
	})
	f.on("FROM pg_extension ORDER BY", [][]string{{"timescaledb"}})
	f.on("timescaledb_information.dimensions", [][]string{
		{"public", "metrics", "ts"},
	})
	// metrics has 3 chunks (basis for the chunk-preservation check).
	f.on("timescaledb_information.chunks WHERE", [][]string{{"3"}})
	// Row counts: orders non-empty, audit empty. metrics is a hypertable, not
	// emitted as a non-empty check, but Introspect still counts it.
	f.on(`count(*) FROM "public"."orders"`, [][]string{{"42"}})
	f.on(`count(*) FROM "public"."audit"`, [][]string{{"0"}})
	f.on(`count(*) FROM "public"."metrics"`, [][]string{{"7"}})
	// Observed ages (seconds): metrics 1h, orders 2h. Match on the EXTRACT
	// expression so these win over the plain count(*) rules above.
	f.on(`EXTRACT(EPOCH FROM (now() - max("ts")))::bigint FROM "public"."metrics"`, [][]string{{"3600"}})
	f.on(`EXTRACT(EPOCH FROM (now() - max("created_at")))::bigint FROM "public"."orders"`, [][]string{{"7200"}})
	return f
}

func TestIntrospectParses(t *testing.T) {
	d, err := Introspect(context.Background(), baseFake(), "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	if len(d.Databases) != 2 {
		t.Errorf("databases: got %d want 2 (%+v)", len(d.Databases), d.Databases)
	}
	if len(d.Tables) != 3 {
		t.Fatalf("tables: got %d want 3 (%+v)", len(d.Tables), d.Tables)
	}

	byName := map[string]Table{}
	for _, tb := range d.Tables {
		byName[tb.Name] = tb
	}
	if byName["orders"].Rows != 42 {
		t.Errorf("orders rows: got %d want 42", byName["orders"].Rows)
	}
	if byName["audit"].Rows != 0 {
		t.Errorf("audit rows: got %d want 0", byName["audit"].Rows)
	}

	if len(d.Hypertables) != 1 {
		t.Fatalf("hypertables: got %d want 1 (%+v)", len(d.Hypertables), d.Hypertables)
	}
	ht := d.Hypertables[0]
	if ht.Name != "metrics" || ht.TimeColumn != "ts" {
		t.Errorf("hypertable: got %q col %q want metrics/ts", ht.Name, ht.TimeColumn)
	}
	if ht.ObservedAge != time.Hour {
		t.Errorf("hypertable observed age: got %s want 1h", ht.ObservedAge)
	}

	if len(d.FreshnessCandidates) != 1 {
		t.Fatalf("freshness candidates: got %d want 1 (%+v)", len(d.FreshnessCandidates), d.FreshnessCandidates)
	}
	fc := d.FreshnessCandidates[0]
	if fc.Name != "orders" || fc.TimeColumn != "created_at" {
		t.Errorf("freshness candidate: got %q col %q want orders/created_at", fc.Name, fc.TimeColumn)
	}
	if fc.ObservedAge != 2*time.Hour {
		t.Errorf("freshness candidate observed age: got %s want 2h", fc.ObservedAge)
	}

	if len(d.Extensions) != 1 || d.Extensions[0] != "timescaledb" {
		t.Errorf("extensions: got %+v want [timescaledb]", d.Extensions)
	}
}

func TestIntrospectGuardsTimescaleWhenAbsent(t *testing.T) {
	f := &fakeQueryer{}
	f.on("FROM pg_database WHERE", [][]string{{"app"}})
	f.on("information_schema.tables WHERE", [][]string{{"public", "orders"}})
	f.on("information_schema.columns WHERE", [][]string{})
	f.on("FROM pg_extension ORDER BY", [][]string{{"pgcrypto"}})
	f.on(`count(*) FROM "public"."orders"`, [][]string{{"3"}})

	d, err := Introspect(context.Background(), f, "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if len(d.Hypertables) != 0 {
		t.Errorf("hypertables: got %d want 0 (timescaledb absent)", len(d.Hypertables))
	}
	for _, sql := range f.seen {
		if strings.Contains(sql, "timescaledb_information") {
			t.Errorf("queried timescaledb catalog despite extension absent: %q", sql)
		}
	}
}

func TestGenerateChecksStructural(t *testing.T) {
	d, err := Introspect(context.Background(), baseFake(), "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	checks := GenerateChecks(d)
	byName := index(checks)

	for _, name := range []string{"server_reachable", "has_user_database", "schema_present"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("missing structural check %q", name)
		}
		if c.Severity != "required" {
			t.Errorf("%s severity: got %q want required", name, c.Severity)
		}
	}

	sr := byName["server_reachable"]
	if sr.SQL != "SELECT 1" || sr.Equals == nil || *sr.Equals != "1" {
		t.Errorf("server_reachable wrong: %+v", sr)
	}
	hud := byName["has_user_database"]
	if hud.ExpectMin == nil || *hud.ExpectMin != 1 {
		t.Errorf("has_user_database expect_min: %+v", hud.ExpectMin)
	}
}

func TestGenerateChecksHeuristicsAdvisory(t *testing.T) {
	d, err := Introspect(context.Background(), baseFake(), "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	checks := GenerateChecks(d)
	byName := index(checks)

	// orders is non-empty → advisory non-empty check; audit is empty → none.
	ne, ok := byName["orders_nonempty"]
	if !ok {
		t.Fatalf("missing orders_nonempty check")
	}
	if ne.Severity != "advisory" {
		t.Errorf("orders_nonempty severity: got %q want advisory", ne.Severity)
	}
	if !strings.Contains(ne.SQL, `"public"."orders"`) {
		t.Errorf("orders_nonempty not quoting identifier: %q", ne.SQL)
	}
	if _, bad := byName["audit_nonempty"]; bad {
		t.Errorf("emitted non-empty check for empty table audit")
	}

	// extension loaded check, advisory + boolean.
	ext, ok := byName["timescaledb_loaded"]
	if !ok {
		t.Fatalf("missing timescaledb_loaded check")
	}
	if ext.Severity != "advisory" || ext.Bool == nil || *ext.Bool != true {
		t.Errorf("timescaledb_loaded wrong: %+v", ext)
	}
	if !strings.Contains(ext.SQL, "'timescaledb'") {
		t.Errorf("timescaledb_loaded not quoting literal: %q", ext.SQL)
	}
}

func TestGenerateChecksHypertableFreshness(t *testing.T) {
	d, err := Introspect(context.Background(), baseFake(), "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	checks := GenerateChecks(d)
	byName := index(checks)

	fresh, ok := byName["metrics_fresh"]
	if !ok {
		t.Fatalf("missing metrics_fresh hypertable freshness check")
	}
	if fresh.Severity != "advisory" {
		t.Errorf("metrics_fresh severity: got %q want advisory", fresh.Severity)
	}
	// Freshness check on the actual time column, quoted.
	if !strings.Contains(fresh.SQL, `max("ts")`) || !strings.Contains(fresh.SQL, `"public"."metrics"`) {
		t.Errorf("metrics_fresh SQL wrong: %q", fresh.SQL)
	}
	if fresh.MaxAge == nil {
		t.Fatalf("metrics_fresh has no MaxAge")
	}
	// Observed 1h × 5 = 5h, floored to 24h.
	if got := fresh.MaxAge.Std(); got != 24*time.Hour {
		t.Errorf("metrics_fresh MaxAge: got %s want 24h (floor)", got)
	}
}

func TestDiscoveryExcludesTimescaleInternalSchemas(t *testing.T) {
	// scaffold must not descend into TimescaleDB's internal chunk/catalog schemas
	// (they produce per-chunk noise). The table + column discovery queries carry
	// the exclusion; behavioral coverage is the corpus timescale entry.
	f := baseFake()
	if _, err := Introspect(context.Background(), f, "app"); err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	var tablesQ, colsQ string
	for _, s := range f.seen {
		if strings.Contains(s, "information_schema.tables WHERE") {
			tablesQ = s
		}
		if strings.Contains(s, "information_schema.columns WHERE") {
			colsQ = s
		}
	}
	for _, sch := range []string{"_timescaledb_internal", "_timescaledb_catalog", "_timescaledb_config"} {
		if !strings.Contains(tablesQ, sch) {
			t.Errorf("tables discovery query does not exclude %s", sch)
		}
		if !strings.Contains(colsQ, sch) {
			t.Errorf("columns discovery query does not exclude %s", sch)
		}
	}
}

func TestGenerateChecksHypertableIntegrity(t *testing.T) {
	d, err := Introspect(context.Background(), baseFake(), "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if d.Hypertables[0].Chunks != 3 {
		t.Fatalf("metrics chunks: got %d want 3", d.Hypertables[0].Chunks)
	}
	checks := GenerateChecks(d)
	byName := index(checks)

	// is_hypertable: catches a restore that degraded the hypertable to a plain
	// table (catalog registration lost).
	isHT, ok := byName["metrics_is_hypertable"]
	if !ok {
		t.Fatalf("missing metrics_is_hypertable check")
	}
	if isHT.Bool == nil || !*isHT.Bool || isHT.Severity != "advisory" {
		t.Errorf("metrics_is_hypertable wrong: %+v", isHT)
	}
	if !strings.Contains(isHT.SQL, "timescaledb_information.hypertables") ||
		!strings.Contains(isHT.SQL, "'public'") || !strings.Contains(isHT.SQL, "'metrics'") {
		t.Errorf("metrics_is_hypertable SQL wrong: %q", isHT.SQL)
	}

	// chunks: floor at the observed chunk count.
	chunks, ok := byName["metrics_chunks"]
	if !ok {
		t.Fatalf("missing metrics_chunks check")
	}
	if chunks.ExpectMin == nil || *chunks.ExpectMin != 3 || chunks.Severity != "advisory" {
		t.Errorf("metrics_chunks wrong: %+v", chunks)
	}
	if !strings.Contains(chunks.SQL, "timescaledb_information.chunks") {
		t.Errorf("metrics_chunks SQL wrong: %q", chunks.SQL)
	}
}

func TestGenerateChecksEmptyHypertableOmitsChunkFloor(t *testing.T) {
	f := baseFake()
	// An empty hypertable reports zero chunks; the chunk-floor check is then
	// omitted (a >=0 floor asserts nothing), but is_hypertable still applies.
	// Prepend so this wins over baseFake's chunks-> 3 rule (first match wins).
	f.rules = append([]rule{{match: "timescaledb_information.chunks WHERE", rows: [][]string{{"0"}}}}, f.rules...)
	d, err := Introspect(context.Background(), f, "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	byName := index(GenerateChecks(d))
	if _, ok := byName["metrics_chunks"]; ok {
		t.Errorf("metrics_chunks should be omitted when chunk count is zero")
	}
	if _, ok := byName["metrics_is_hypertable"]; !ok {
		t.Errorf("metrics_is_hypertable should still be present for an empty hypertable")
	}
}

func TestGenerateChecksPlainFreshnessDerivedFromObserved(t *testing.T) {
	// orders observed 2h: 2h × 5 = 10h, below the 24h floor → 24h.
	d, err := Introspect(context.Background(), baseFake(), "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	checks := GenerateChecks(d)
	byName := index(checks)

	fresh, ok := byName["orders_fresh"]
	if !ok {
		t.Fatalf("missing orders_fresh check")
	}
	if !strings.Contains(fresh.SQL, `max("created_at")`) {
		t.Errorf("orders_fresh SQL wrong: %q", fresh.SQL)
	}
	if fresh.MaxAge == nil || fresh.MaxAge.Std() != 24*time.Hour {
		t.Errorf("orders_fresh MaxAge: got %v want 24h", fresh.MaxAge)
	}
}

func TestMaxAgeAboveFloorScalesWithObserved(t *testing.T) {
	// 10h observed × 5 = 50h, above the 24h floor → 50h. Confirms threshold
	// tracks observed data rather than always pinning to the floor.
	got := maxAgeFromObserved(10 * time.Hour)
	if got == nil || got.Std() != 50*time.Hour {
		t.Errorf("maxAgeFromObserved(10h): got %v want 50h", got)
	}
}

func TestNonEmptyCheckCap(t *testing.T) {
	const total = 75
	f := &fakeQueryer{}
	f.on("FROM pg_database WHERE", [][]string{{"app"}})

	tables := make([][]string, 0, total)
	for i := 0; i < total; i++ {
		tables = append(tables, []string{"public", "t" + pad(i)})
	}
	f.on("information_schema.tables WHERE", tables)
	f.on("information_schema.columns WHERE", [][]string{})
	f.on("FROM pg_extension ORDER BY", [][]string{})
	// Every table reports 5 rows (the default fake answer is 0; override).
	f.on("count(*) FROM ", [][]string{{"5"}})

	d, err := Introspect(context.Background(), f, "app")
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	checks := GenerateChecks(d)
	n := 0
	for _, c := range checks {
		if strings.HasSuffix(c.Name, "_nonempty") {
			n++
		}
	}
	if n != nonEmptyTableCap {
		t.Errorf("non-empty checks: got %d want cap %d", n, nonEmptyTableCap)
	}
}

func TestQuotingDefendsAgainstInjection(t *testing.T) {
	// A table name with an embedded quote must be doubled, not break out.
	tb := Table{Schema: "pub", Name: `ev"il`}
	q := tb.Qualified()
	if q != `"pub"."ev""il"` {
		t.Errorf("Qualified quoting: got %q", q)
	}
}

func index(checks []config.Check) map[string]config.Check {
	m := make(map[string]config.Check, len(checks))
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

func pad(i int) string {
	s := ""
	if i < 10 {
		s = "0"
	}
	return s + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
