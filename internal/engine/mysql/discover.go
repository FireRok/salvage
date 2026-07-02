// MySQL scaffold discovery (spec 0028 R3): introspect MySQL's own
// information_schema — never Postgres catalogs — and propose `sql`-kind checks
// whose thresholds come from observed state. This file registers no new check
// evaluator: every emitted check reuses the existing sql kind exactly as spec
// 0024 R2 did for `run`, so the seam added here is discovery, not evaluation.
//
// The introspection lives in this engine package rather than a generalized
// internal/discover (spec 0028 Open questions): MySQL's information_schema
// differs from Postgres's catalog in shape and quoting, so the engines share
// only the spi.Scaffolder seam and the horizontal emission layer.

package mysql

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
)

// The engine implements check discovery for `salvage scaffold` (spec 0028 R3).
var _ spi.Scaffolder = Engine{}

// rowQueryer is the multi-row introspection handle the restored MySQL target
// exposes (ephemeral.MySQL.QueryRows). Declared engine-locally so discovery
// depends on the interface, not the container type — fully testable with a fake.
type rowQueryer interface {
	QueryRows(ctx context.Context, sql string) ([][]string, error)
}

// mysqlSystemSchemas is the SQL IN-list of MySQL's own schemas, excluded from
// user-table discovery (spec 0028 R3).
const mysqlSystemSchemas = `'mysql', 'information_schema', 'performance_schema', 'sys'`

// Freshness thresholds mirror the Postgres scaffold heuristics (spec 0009 R4):
// max_age = observed age × margin, floored so a freshly loaded snapshot never
// false-FAILs on clock skew or slow ingest.
const (
	freshnessMargin = 5
	freshnessFloor  = 24 * time.Hour
)

// obviousTimeColumnNames are the column names treated as an obvious freshness
// column — the same name set the Postgres scaffold uses (spec 0009 R4).
var obviousTimeColumnNames = map[string]bool{
	"created_at":  true,
	"updated_at":  true,
	"inserted_at": true,
	"ts":          true,
	"time":        true,
	"event_time":  true,
}

// table is a user table in the restored database with its observed row count
// and any timestamp/datetime columns (in ordinal order).
type table struct {
	Name        string
	Rows        int64
	TimeColumns []string
}

// Discover introspects the restored database's information_schema and proposes
// sql-kind candidate checks (spec 0028 R3):
//
//   - reachability and a user-table-count floor — **required**, the MySQL
//     analogues of Postgres's server_reachable/schema_present;
//   - a row-count floor per non-empty table (expect_min = observed count, a
//     floor so growth never false-FAILs) — **advisory**;
//   - a freshness bound per non-empty table with an obvious timestamp/datetime
//     column (max_age = observed age × margin) — **advisory**.
//
// Per-table candidates carry the "table" cap group with the observed row count
// as weight, so the shared emission layer keeps the top-N largest tables on a
// wide schema (spec 0028 R6). Identifiers are backtick-quoted (the MySQL
// analogue of spec 0009 R4's quoting rule). Part of spi.Scaffolder.
func (Engine) Discover(ctx context.Context, rt spi.RestoredTarget, cfg *config.Config) ([]spi.ScaffoldCandidate, error) {
	q, ok := rt.(rowQueryer)
	if !ok {
		return nil, fmt.Errorf("restored target for target.type %q does not answer row queries", cfg.Target.Type)
	}
	tables, err := introspect(ctx, q, cfg.Target.Restore.Database)
	if err != nil {
		return nil, err
	}
	return generate(ctx, q, cfg.Target.Restore.Database, tables)
}

// introspect reads the user tables of database from information_schema: names
// (deterministic order), row counts, and timestamp/datetime columns.
func introspect(ctx context.Context, q rowQueryer, database string) ([]table, error) {
	rows, err := q.QueryRows(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema = `+quoteLiteral(database)+
			` AND table_schema NOT IN (`+mysqlSystemSchemas+`) ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.tables: %w", err)
	}
	tables := make([]table, 0, len(rows))
	for _, r := range rows {
		if len(r) < 1 || r[0] == "" {
			continue
		}
		tables = append(tables, table{Name: r[0]})
	}

	// Timestamp/datetime columns per table, in ordinal order — the basis for the
	// deterministic freshness-column pick.
	rows, err = q.QueryRows(ctx,
		`SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = `+quoteLiteral(database)+
			` AND data_type IN ('timestamp', 'datetime') ORDER BY table_name, ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("query information_schema.columns: %w", err)
	}
	colsByTable := map[string][]string{}
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		colsByTable[r[0]] = append(colsByTable[r[0]], r[1])
	}

	for i := range tables {
		tables[i].TimeColumns = colsByTable[tables[i].Name]
		cr, err := q.QueryRows(ctx, "SELECT count(*) FROM "+qualified(database, tables[i].Name))
		if err != nil {
			return nil, fmt.Errorf("count rows of %s: %w", tables[i].Name, err)
		}
		tables[i].Rows = scalarInt(cr)
	}
	return tables, nil
}

// generate derives the candidate checks from the introspected tables. All
// thresholds come from observed values; structural checks are required and
// heuristic checks advisory (spec 0009 R6), so a scaffolded config never
// false-FAILs out of the box.
func generate(ctx context.Context, q rowQueryer, database string, tables []table) ([]spi.ScaffoldCandidate, error) {
	cands := make([]spi.ScaffoldCandidate, 0, 2+2*len(tables))

	// Structural presence (required, never capped): the MySQL analogues of
	// Postgres's server_reachable/schema_present.
	cands = append(cands,
		spi.ScaffoldCandidate{Check: config.Check{
			Name:     "server_reachable",
			SQL:      "SELECT 1",
			Equals:   strptr("1"),
			Severity: "required",
		}},
		spi.ScaffoldCandidate{Check: config.Check{
			Name: "schema_present",
			SQL: "SELECT count(*) FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema = " +
				quoteLiteral(database),
			ExpectMin: f64ptr(1),
			Severity:  "required",
		}},
	)

	for _, t := range tables {
		if t.Rows <= 0 {
			continue // an empty table has no floor and no newest row to observe
		}
		// Row-count floor (advisory): expect_min = observed count, a floor so
		// growth never false-FAILs (spec 0028 R3).
		cands = append(cands, spi.ScaffoldCandidate{
			Check: config.Check{
				Name:      sanitize(t.Name) + "_min_rows",
				SQL:       "SELECT count(*) FROM " + qualified(database, t.Name),
				ExpectMin: f64ptr(float64(t.Rows)),
				Severity:  "advisory",
			},
			Group:   "table",
			Subject: t.Name,
			Weight:  t.Rows,
		})

		col, ok := freshnessColumn(t)
		if !ok {
			continue
		}
		age, err := observedAge(ctx, q, database, t.Name, col)
		if err != nil {
			return nil, err
		}
		cands = append(cands, spi.ScaffoldCandidate{
			Check: config.Check{
				Name:     sanitize(t.Name) + "_fresh",
				SQL:      "SELECT max(" + quoteIdent(col) + ") FROM " + qualified(database, t.Name),
				MaxAge:   maxAgeFromObserved(age),
				Severity: "advisory",
			},
			Group:   "table",
			Subject: t.Name,
			Weight:  t.Rows,
		})
	}
	return cands, nil
}

// freshnessColumn picks the freshness column for a table deterministically
// (spec 0028 Open questions): among the obvious-named timestamp/datetime
// columns, prefer updated_at, else the first by ordinal position.
func freshnessColumn(t table) (string, bool) {
	var first string
	for _, c := range t.TimeColumns {
		if !obviousTimeColumnNames[strings.ToLower(c)] {
			continue
		}
		if strings.EqualFold(c, "updated_at") {
			return c, true
		}
		if first == "" {
			first = c
		}
	}
	return first, first != ""
}

// observedAge queries the staleness of the table's newest row in seconds.
// COALESCE absorbs a NULL max (no rows) into 0, which the floor then covers.
func observedAge(ctx context.Context, q rowQueryer, database, tbl, col string) (time.Duration, error) {
	sql := "SELECT COALESCE(TIMESTAMPDIFF(SECOND, max(" + quoteIdent(col) + "), NOW()), 0) FROM " + qualified(database, tbl)
	rows, err := q.QueryRows(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("observe age of %s.%s: %w", tbl, col, err)
	}
	secs := scalarInt(rows)
	if secs < 0 {
		secs = 0
	}
	return time.Duration(secs) * time.Second, nil
}

// maxAgeFromObserved derives a max_age from an observed age: observed × margin,
// floored — always strictly larger than the observed age, so the check passes
// on the snapshot it was derived from.
func maxAgeFromObserved(observed time.Duration) *config.Duration {
	budget := observed * freshnessMargin
	if budget < freshnessFloor {
		budget = freshnessFloor
	}
	d := config.Duration(budget)
	return &d
}

// qualified returns the safely backtick-quoted schema-qualified table name,
// suitable for interpolation into generated SQL.
func qualified(database, tbl string) string {
	return quoteIdent(database) + "." + quoteIdent(tbl)
}

// quoteIdent backtick-quotes a MySQL identifier, doubling embedded backticks —
// the MySQL analogue of the Postgres scaffold's double-quoting rule.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// quoteLiteral single-quotes a SQL string literal, doubling embedded quotes.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
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

func strptr(s string) *string   { return &s }
func f64ptr(f float64) *float64 { return &f }
