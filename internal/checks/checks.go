// Package checks evaluates assertions against a restored database.
package checks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// Queryer runs a scalar SQL query. *ephemeral.Postgres satisfies it.
type Queryer interface {
	Query(ctx context.Context, sql string) (string, error)
}

// Run evaluates every check and returns a result per check, in order.
func Run(ctx context.Context, q Queryer, checks []config.Check) []report.CheckResult {
	out := make([]report.CheckResult, 0, len(checks))
	for _, c := range checks {
		out = append(out, evaluate(ctx, q, c))
	}
	return out
}

func evaluate(ctx context.Context, q Queryer, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	got, err := q.Query(ctx, c.SQL)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	got = strings.TrimSpace(got)
	res.Got = got

	switch {
	case c.ExpectMin != nil || c.ExpectMax != nil:
		v, perr := strconv.ParseFloat(got, 64)
		if perr != nil {
			res.Error = fmt.Sprintf("expected a number, got %q", got)
			return res
		}
		res.OK = true
		switch {
		case c.ExpectMin != nil && v < *c.ExpectMin:
			res.OK = false
			res.Detail = fmt.Sprintf("%g < min %g", v, *c.ExpectMin)
		case c.ExpectMax != nil && v > *c.ExpectMax:
			res.OK = false
			res.Detail = fmt.Sprintf("%g > max %g", v, *c.ExpectMax)
		default:
			res.Detail = fmt.Sprintf("%g within bounds", v)
		}
	case c.Equals != nil:
		res.OK = got == *c.Equals
		if !res.OK {
			res.Detail = fmt.Sprintf("want %q", *c.Equals)
		}
	case c.MaxAge != nil:
		ts, perr := parseTime(got)
		if perr != nil {
			res.Error = fmt.Sprintf("expected a timestamp, got %q", got)
			return res
		}
		age := time.Since(ts)
		res.OK = age <= c.MaxAge.Std()
		res.Detail = fmt.Sprintf("age %s (max %s)", age.Round(time.Second), c.MaxAge.Std())
	case c.Bool != nil:
		b, perr := parseBool(got)
		if perr != nil {
			res.Error = fmt.Sprintf("expected a boolean, got %q", got)
			return res
		}
		res.OK = b == *c.Bool
		if !res.OK {
			res.Detail = fmt.Sprintf("want %t", *c.Bool)
		}
	default:
		res.Error = "no expectation configured"
	}
	return res
}

var tsLayouts = []string{
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02",
}

// parseBool reads a Postgres boolean scalar. Postgres renders booleans as "t"/"f"
// but SQL predicates and casts may also yield "true"/"false" or "1"/"0".
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "true", "1":
		return true, nil
	case "f", "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("unrecognized boolean %q", s)
}

func parseTime(s string) (time.Time, error) {
	for _, l := range tsLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}
