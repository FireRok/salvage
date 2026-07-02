package checks

import (
	"context"
	"strings"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// Queryer runs a scalar SQL query. *ephemeral.Postgres satisfies it.
type Queryer interface {
	Query(ctx context.Context, sql string) (string, error)
}

// The sql kind is the default (empty) kind — today's behaviour. Registering it
// here (rather than in an engine) keeps the SQL semantics one canonical copy
// that every SQL-backed engine shares; a target satisfies it simply by
// implementing Queryer.
func init() { RegisterEvaluator("sql", evaluateSQL) }

// evaluateSQL runs the check's SQL against target and evaluates the expectation.
// It type-asserts target to Queryer — the capability a SQL check needs — and
// returns a clear failing result if the target cannot answer SQL (e.g. a
// non-SQL engine's target reached a sql check). The expectation itself is the
// shared scalar evaluator (scalar.go), so its semantics and every result/detail
// string are identical to the pre-seam evaluate().
func evaluateSQL(ctx context.Context, target Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	q, ok := target.(Queryer)
	if !ok {
		res.Error = "sql check requires a SQL-queryable target"
		return res
	}
	got, err := q.Query(ctx, c.SQL)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if !EvaluateScalar(strings.TrimSpace(got), c, &res) {
		res.Error = "no expectation configured"
	}
	return res
}
