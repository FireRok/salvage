// Package discover introspects a restored Postgres cluster's system catalogs
// and derives a deterministic baseline of checks from the observed state.
//
// It performs no LLM calls and opens no network connections of its own: every
// query is issued through an injected RowQueryer, so the package is pure logic
// and fully testable with a fake. Thresholds are computed from values observed
// in the live catalogs (current row counts, the freshness of the newest row),
// never hard-coded — a scaffolded config passes by construction on the
// known-good snapshot it was generated from.
package discover

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/config"
)

// RowQueryer runs a SQL query and returns one []string per result row, with
// cells in SELECT order. The integrator implements this against the ephemeral
// restore environment; discover only depends on the interface.
type RowQueryer interface {
	QueryRows(ctx context.Context, sql string) ([][]string, error)
}

// Database is a non-template database in the restored cluster.
type Database struct {
	Name string
}

// Column is a timestamp-bearing column on a user table.
type Column struct {
	Name string
	// Type is the information_schema data_type, e.g. "timestamp with time zone".
	Type string
}

// IsTimestamptz reports whether the column carries a time zone, i.e. it is a
// timestamptz. Freshness candidates on plain tables are restricted to these.
func (c Column) IsTimestamptz() bool {
	return strings.Contains(strings.ToLower(c.Type), "with time zone")
}

// Table is a user table (excludes system schemas) with its observed row count
// and any timestamp/timestamptz columns.
type Table struct {
	Schema string
	Name   string
	// Rows is the current row count observed via SELECT count(*).
	Rows int64
	// TimeColumns are the timestamp/timestamptz columns on this table, in
	// catalog order.
	TimeColumns []Column
}

// Qualified returns the safely double-quoted schema-qualified table name,
// suitable for interpolation into generated SQL.
func (t Table) Qualified() string {
	return quoteIdent(t.Schema) + "." + quoteIdent(t.Name)
}

// Hypertable is a TimescaleDB hypertable with its time-partitioning column.
type Hypertable struct {
	Schema     string
	Name       string
	TimeColumn string
	// ObservedAge is now() - max(time_column) at introspection time, the basis
	// for the derived freshness threshold.
	ObservedAge time.Duration
	// Chunks is the number of chunks observed at introspection time — the basis
	// for the derived chunk-preservation threshold. A restore that silently drops
	// the TimescaleDB catalog leaves the data as a plain table with zero chunks.
	Chunks int64
}

// Qualified returns the safely double-quoted schema-qualified hypertable name.
func (h Hypertable) Qualified() string {
	return quoteIdent(h.Schema) + "." + quoteIdent(h.Name)
}

// FreshnessCandidate is a plain (non-hypertable) table that carries exactly one
// obvious timestamptz column, making it a freshness target.
type FreshnessCandidate struct {
	Schema     string
	Name       string
	TimeColumn string
	// ObservedAge is now() - max(time_column) at introspection time.
	ObservedAge time.Duration
}

// Qualified returns the safely double-quoted schema-qualified table name.
func (f FreshnessCandidate) Qualified() string {
	return quoteIdent(f.Schema) + "." + quoteIdent(f.Name)
}

// Discovery is the structured result of introspecting one database.
type Discovery struct {
	// Database is the database that was introspected.
	Database string
	// Databases are the non-template databases in the cluster.
	Databases []Database
	// Tables are the user tables (with row counts and timestamp columns).
	Tables []Table
	// Hypertables are TimescaleDB hypertables (empty if timescaledb absent).
	Hypertables []Hypertable
	// FreshnessCandidates are plain tables with one obvious timestamptz column.
	FreshnessCandidates []FreshnessCandidate
	// Extensions are installed extension names (from pg_extension).
	Extensions []string
}

// nonEmptyTableCap bounds how many per-table non-empty checks GenerateChecks
// emits, so a wide schema does not produce an unwieldy config. If more
// non-empty tables exist than the cap, the extra tables are simply omitted
// (the structural schema_present check still covers their existence).
const nonEmptyTableCap = 50

// freshnessMargin multiplies the observed age to set MaxAge: a table whose
// newest row is an hour old gets a 5h budget, floored to at least 24h so a
// freshly loaded snapshot does not false-FAIL on clock skew or slow ingest.
const freshnessMargin = 5

// freshnessFloor is the minimum MaxAge for any freshness check.
const freshnessFloor = 24 * time.Hour

// knownPreloadExtensions are extensions that require shared_preload_libraries;
// for each one installed we emit an advisory "loaded" boolean check.
var knownPreloadExtensions = map[string]bool{
	"timescaledb": true,
}

// obviousTimeColumnNames are the column names treated as an obvious freshness
// column on a plain table. A plain table qualifies only when it has exactly one
// timestamptz column whose name is in this set.
var obviousTimeColumnNames = map[string]bool{
	"created_at":  true,
	"updated_at":  true,
	"inserted_at": true,
	"ts":          true,
	"time":        true,
	"event_time":  true,
}

// systemSchemaList is the SQL IN-list of schemas excluded from table/column
// discovery: Postgres catalogs plus TimescaleDB's internal implementation schemas.
// The per-chunk tables live in _timescaledb_internal and TimescaleDB's own catalog
// in _timescaledb_catalog/_config/_cache; discovering them makes scaffold emit
// noisy per-chunk freshness/non-empty checks. The hypertable itself is already
// covered by the is_hypertable/chunks/fresh checks, so its internals are excluded.
const systemSchemaList = `'pg_catalog', 'information_schema', '_timescaledb_internal', '_timescaledb_catalog', '_timescaledb_config', '_timescaledb_cache'`

// Introspect reads the system catalogs of the given database through q and
// returns a structured Discovery. The timescaledb-specific query is guarded so
// a cluster without the extension does not error.
func Introspect(ctx context.Context, q RowQueryer, database string) (*Discovery, error) {
	d := &Discovery{Database: database}

	// Non-template databases.
	rows, err := q.QueryRows(ctx, `SELECT datname FROM pg_database WHERE datname NOT IN ('template0', 'template1') ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("query pg_database: %w", err)
	}
	for _, r := range rows {
		if len(r) < 1 {
			continue
		}
		d.Databases = append(d.Databases, Database{Name: r[0]})
	}

	// User tables (exclude system schemas).
	rows, err = q.QueryRows(ctx, `SELECT table_schema, table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema NOT IN (`+systemSchemaList+`) ORDER BY table_schema, table_name`)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.tables: %w", err)
	}
	tables := make([]Table, 0, len(rows))
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		tables = append(tables, Table{Schema: r[0], Name: r[1]})
	}

	// Timestamp/timestamptz columns per user table.
	rows, err = q.QueryRows(ctx, `SELECT table_schema, table_name, column_name, data_type FROM information_schema.columns WHERE table_schema NOT IN (`+systemSchemaList+`) AND data_type IN ('timestamp without time zone', 'timestamp with time zone') ORDER BY table_schema, table_name, ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.columns: %w", err)
	}
	colsByTable := map[string][]Column{}
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		key := r[0] + "\x00" + r[1]
		colsByTable[key] = append(colsByTable[key], Column{Name: r[2], Type: r[3]})
	}
	for i := range tables {
		key := tables[i].Schema + "\x00" + tables[i].Name
		tables[i].TimeColumns = colsByTable[key]
	}

	// Current row count per table.
	for i := range tables {
		countSQL := fmt.Sprintf("SELECT count(*) FROM %s", tables[i].Qualified())
		cr, err := q.QueryRows(ctx, countSQL)
		if err != nil {
			return nil, fmt.Errorf("count rows of %s: %w", tables[i].Qualified(), err)
		}
		tables[i].Rows = scalarInt(cr)
	}
	d.Tables = tables

	// Installed extensions.
	rows, err = q.QueryRows(ctx, `SELECT extname FROM pg_extension ORDER BY extname`)
	if err != nil {
		return nil, fmt.Errorf("query pg_extension: %w", err)
	}
	for _, r := range rows {
		if len(r) < 1 {
			continue
		}
		d.Extensions = append(d.Extensions, r[0])
	}

	// TimescaleDB hypertables, guarded: only attempt when timescaledb is
	// installed, so a vanilla Postgres does not error on the missing schema.
	if hasExtension(d.Extensions, "timescaledb") {
		rows, err = q.QueryRows(ctx, `SELECT hypertable_schema, hypertable_name, column_name FROM timescaledb_information.dimensions WHERE dimension_type = 'Time' ORDER BY hypertable_schema, hypertable_name`)
		if err != nil {
			return nil, fmt.Errorf("query timescaledb_information.dimensions: %w", err)
		}
		for _, r := range rows {
			if len(r) < 3 {
				continue
			}
			ht := Hypertable{Schema: r[0], Name: r[1], TimeColumn: r[2]}
			age, err := observedAge(ctx, q, ht.Qualified(), ht.TimeColumn)
			if err != nil {
				return nil, err
			}
			ht.ObservedAge = age
			chunks, err := hypertableChunks(ctx, q, ht.Schema, ht.Name)
			if err != nil {
				return nil, err
			}
			ht.Chunks = chunks
			d.Hypertables = append(d.Hypertables, ht)
		}
	}

	// Freshness candidates: plain tables (not hypertables) with exactly one
	// obvious timestamptz column.
	hyperKeys := map[string]bool{}
	for _, ht := range d.Hypertables {
		hyperKeys[ht.Schema+"\x00"+ht.Name] = true
	}
	for _, t := range tables {
		if hyperKeys[t.Schema+"\x00"+t.Name] {
			continue
		}
		col, ok := obviousTimeColumn(t)
		if !ok {
			continue
		}
		fc := FreshnessCandidate{Schema: t.Schema, Name: t.Name, TimeColumn: col}
		age, err := observedAge(ctx, q, fc.Qualified(), fc.TimeColumn)
		if err != nil {
			return nil, err
		}
		fc.ObservedAge = age
		d.FreshnessCandidates = append(d.FreshnessCandidates, fc)
	}

	return d, nil
}

// observedAge queries now() - max(<col>) for a table, returning the observed
// staleness of its newest row. A table with no rows (NULL max) yields a zero
// age, which the floor in GenerateChecks then absorbs.
func observedAge(ctx context.Context, q RowQueryer, qualified, col string) (time.Duration, error) {
	sql := fmt.Sprintf("SELECT EXTRACT(EPOCH FROM (now() - max(%s)))::bigint FROM %s", quoteIdent(col), qualified)
	rows, err := q.QueryRows(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("observe age of %s.%s: %w", qualified, col, err)
	}
	secs := scalarInt(rows)
	if secs < 0 {
		secs = 0
	}
	return time.Duration(secs) * time.Second, nil
}

// hypertableChunks returns the number of chunks for a hypertable, the basis for
// the chunk-preservation check. Zero is a legitimate answer (an empty
// hypertable), so it is not treated as an error.
func hypertableChunks(ctx context.Context, q RowQueryer, schema, name string) (int64, error) {
	sql := fmt.Sprintf(
		"SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_schema = %s AND hypertable_name = %s",
		quoteLiteral(schema), quoteLiteral(name))
	rows, err := q.QueryRows(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("count chunks of %s.%s: %w", schema, name, err)
	}
	return scalarInt(rows), nil
}

// obviousTimeColumn returns the single obvious timestamptz column name on a
// plain table, if exactly one such column exists.
func obviousTimeColumn(t Table) (string, bool) {
	var found []string
	for _, c := range t.TimeColumns {
		if c.IsTimestamptz() && obviousTimeColumnNames[strings.ToLower(c.Name)] {
			found = append(found, c.Name)
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return "", false
}

// GenerateChecks derives the deterministic baseline of checks from a Discovery.
// Structural/presence checks are required; every heuristic check (non-empty,
// freshness, extension) is advisory, so a scaffolded config never false-FAILs
// out of the box. All thresholds come from observed values in d.
func GenerateChecks(d *Discovery) []config.Check {
	checks := make([]config.Check, 0, 8+len(d.Tables)+len(d.Hypertables)+len(d.FreshnessCandidates))

	// Structural checks (required).
	checks = append(checks,
		config.Check{
			Name:     "server_reachable",
			SQL:      "SELECT 1",
			Equals:   strptr("1"),
			Severity: "required",
		},
		config.Check{
			Name:      "has_user_database",
			SQL:       "SELECT count(*) FROM pg_database WHERE datname NOT IN ('template0', 'template1')",
			ExpectMin: f64ptr(1),
			Severity:  "required",
		},
		config.Check{
			Name:      "schema_present",
			SQL:       "SELECT count(*) FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')",
			ExpectMin: f64ptr(1),
			Severity:  "required",
		},
	)

	// Per non-empty user table: non-empty check (advisory), capped at
	// nonEmptyTableCap. Tables beyond the cap are omitted.
	emitted := 0
	for _, t := range d.Tables {
		if t.Rows <= 0 {
			continue
		}
		if emitted >= nonEmptyTableCap {
			break
		}
		emitted++
		checks = append(checks, config.Check{
			Name:      checkName(t.Schema, t.Name, "nonempty"),
			SQL:       fmt.Sprintf("SELECT count(*) FROM (SELECT 1 FROM %s LIMIT 1) x", t.Qualified()),
			ExpectMin: f64ptr(1),
			Severity:  "advisory",
		})
	}

	// Per hypertable: integrity + freshness checks (advisory).
	//
	// is_hypertable guards the scariest silent-restore failure — the data comes
	// back but the TimescaleDB catalog registration is lost, so a hypertable
	// degrades to an ordinary table (partitioning, compression and retention
	// policies gone). chunks guards against a hypertable that restored with fewer
	// chunks than it had, using the observed count as the floor. fresh is the
	// recency check on its actual time column.
	for _, ht := range d.Hypertables {
		checks = append(checks, config.Check{
			Name:     checkName(ht.Schema, ht.Name, "is_hypertable"),
			SQL:      fmt.Sprintf("SELECT count(*) > 0 FROM timescaledb_information.hypertables WHERE hypertable_schema = %s AND hypertable_name = %s", quoteLiteral(ht.Schema), quoteLiteral(ht.Name)),
			Bool:     boolptr(true),
			Severity: "advisory",
		})
		if ht.Chunks > 0 {
			checks = append(checks, config.Check{
				Name:      checkName(ht.Schema, ht.Name, "chunks"),
				SQL:       fmt.Sprintf("SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_schema = %s AND hypertable_name = %s", quoteLiteral(ht.Schema), quoteLiteral(ht.Name)),
				ExpectMin: f64ptr(float64(ht.Chunks)),
				Severity:  "advisory",
			})
		}
		checks = append(checks, config.Check{
			Name:     checkName(ht.Schema, ht.Name, "fresh"),
			SQL:      fmt.Sprintf("SELECT max(%s) FROM %s", quoteIdent(ht.TimeColumn), ht.Qualified()),
			MaxAge:   maxAgeFromObserved(ht.ObservedAge),
			Severity: "advisory",
		})
	}

	// Per plain freshness candidate: freshness check (advisory).
	for _, fc := range d.FreshnessCandidates {
		checks = append(checks, config.Check{
			Name:     checkName(fc.Schema, fc.Name, "fresh"),
			SQL:      fmt.Sprintf("SELECT max(%s) FROM %s", quoteIdent(fc.TimeColumn), fc.Qualified()),
			MaxAge:   maxAgeFromObserved(fc.ObservedAge),
			Severity: "advisory",
		})
	}

	// Per known-preload extension that is installed: a "loaded" boolean check
	// (advisory).
	for _, ext := range d.Extensions {
		if !knownPreloadExtensions[ext] {
			continue
		}
		checks = append(checks, config.Check{
			Name:     sanitize(ext) + "_loaded",
			SQL:      fmt.Sprintf("SELECT count(*) > 0 FROM pg_extension WHERE extname = %s", quoteLiteral(ext)),
			Bool:     boolptr(true),
			Severity: "advisory",
		})
	}

	return checks
}

// maxAgeFromObserved derives a MaxAge from an observed age: observed × margin,
// floored to freshnessFloor. The threshold is therefore always strictly larger
// than the observed age, so the check passes on the snapshot it was derived
// from.
func maxAgeFromObserved(observed time.Duration) *config.Duration {
	budget := observed * freshnessMargin
	if budget < freshnessFloor {
		budget = freshnessFloor
	}
	d := config.Duration(budget)
	return &d
}

// checkName builds a stable check name from a schema-qualified table and a
// suffix. The "public" schema is elided for brevity (public.orders → orders).
func checkName(schema, name, suffix string) string {
	base := sanitize(name)
	if schema != "" && schema != "public" {
		base = sanitize(schema) + "_" + base
	}
	return base + "_" + suffix
}

// sanitize reduces an identifier to a lowercase, name-safe token for use in a
// check name (purely cosmetic; does not affect SQL safety).
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// quoteIdent double-quotes a Postgres identifier, doubling embedded quotes, so
// it is safe to interpolate into generated SQL.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral single-quotes a Postgres string literal, doubling embedded
// quotes.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// scalarInt reads the first cell of the first row as an int64, defaulting to 0
// for empty/NULL/unparseable results.
func scalarInt(rows [][]string) int64 {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(rows[0][0]), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// hasExtension reports whether name is in the installed-extension list.
func hasExtension(exts []string, name string) bool {
	for _, e := range exts {
		if e == name {
			return true
		}
	}
	return false
}

func strptr(s string) *string   { return &s }
func f64ptr(f float64) *float64 { return &f }
func boolptr(b bool) *bool      { return &b }
