package checks

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// EvaluateScalar is the shared scalar-expectation evaluator (spec 0017): every
// check kind that reduces to "one scalar value vs one configured expectation"
// runs through it, so the expectation semantics — expect_min/expect_max bounds,
// equals, max_age freshness, bool — are a single canonical code path. The sql
// kind (sql.go) and MongoDB's doc_query/collection_count (internal/engine/
// mongodb) all dispatch here.
//
// It records the outcome on res: res.Got is always set to got; OK/Detail/Error
// reflect the expectation exactly as the historical sql evaluator did. The
// return value reports whether c carried an expectation at all — false means
// nothing beyond res.Got was recorded and the caller supplies its own
// kind-specific "no expectation configured" error (config validation normally
// rules this case out before a check ever runs).
//
// Expectation precedence mirrors the historical sql evaluator: bounds first,
// then equals, max_age, bool. Validation enforces "exactly one" for the sql
// kind, so precedence only matters for kinds whose validation is looser.
func EvaluateScalar(got string, c config.Check, res *report.CheckResult) bool {
	res.Got = got
	// Carry the per-check opt-in to keep the exact Got literal through report
	// redaction (spec 0027 R2/R5). Redaction — including known-secret scrubbing,
	// which keep_literal never bypasses — happens at serialization (report
	// WriteJSON/Redact); here we only propagate the operator's choice.
	res.KeepLiteral = c.KeepLiteral
	switch {
	case c.ExpectMin != nil || c.ExpectMax != nil:
		v, perr := strconv.ParseFloat(got, 64)
		if perr != nil {
			res.Error = fmt.Sprintf("expected a number, got %q", got)
			return true
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
			return true
		}
		age := time.Since(ts)
		res.OK = age <= c.MaxAge.Std()
		res.Detail = fmt.Sprintf("age %s (max %s)", age.Round(time.Second), c.MaxAge.Std())
	case c.Bool != nil:
		b, perr := parseBool(got)
		if perr != nil {
			res.Error = fmt.Sprintf("expected a boolean, got %q", got)
			return true
		}
		res.OK = b == *c.Bool
		if !res.OK {
			res.Detail = fmt.Sprintf("want %t", *c.Bool)
		}
	default:
		return false
	}
	return true
}

// tsLayouts are the timestamp renderings a max_age scalar may arrive in:
// Postgres text-format timestamps (with/without zone and fractional seconds),
// RFC3339 (mongosh renders BSON dates via Date.toISOString()), and a bare date.
var tsLayouts = []string{
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02",
}

// parseBool reads a boolean scalar. Postgres renders booleans as "t"/"f", but
// SQL predicates and casts may also yield "true"/"false" or "1"/"0".
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
