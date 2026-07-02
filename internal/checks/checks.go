// Package checks orchestrates assertion evaluation against a restored target.
//
// Orchestration is horizontal and engine-agnostic (spec 0017 R3): Run iterates
// the configured checks, dispatches each to the evaluator registered for its
// kind, and returns a result per check. The *evaluation* of a kind is vertical —
// an engine registers an Evaluator that knows how to run that kind against its
// RestoredTarget. The built-in "sql" evaluator lives in this package (sql.go);
// non-SQL engines (restic/borg, MongoDB, object-storage) register their own
// kinds from their own packages so this file never learns about them.
package checks

import (
	"context"
	"fmt"

	"salvage.sh/internal/config"
	"salvage.sh/internal/report"
)

// Target is the restored thing a check runs against, passed opaquely through the
// orchestration. It is deliberately empty: Run does not know what capability any
// given kind needs, so each Evaluator type-asserts the Target to the interface it
// requires (the sql evaluator to Queryer, a future file-check to a filesystem
// handle, etc.). This is the seam that decouples orchestration from SQL.
type Target = any

// Evaluator runs one check of a particular kind against target and returns its
// result. An evaluator that cannot use target (wrong capability) returns a
// non-OK CheckResult with Error set — never a panic.
type Evaluator func(ctx context.Context, target Target, c config.Check) report.CheckResult

// evaluators maps a check kind to its evaluator. It is populated by init()s
// (RegisterEvaluator) and only read after that, so no locking is needed —
// mirroring the engine SPI registry (spec 0016).
var evaluators = map[string]Evaluator{}

// RegisterEvaluator registers e as the evaluator for kind. It panics on an empty
// kind or a duplicate registration — both are programmer errors caught at init.
func RegisterEvaluator(kind string, e Evaluator) {
	if kind == "" {
		panic("checks: RegisterEvaluator with empty kind")
	}
	if _, dup := evaluators[kind]; dup {
		panic("checks: duplicate evaluator for kind " + kind)
	}
	evaluators[kind] = e
}

// kindOf returns the check's kind, defaulting an empty kind to "sql" — the
// historical single kind. Existing (kind-less) configs therefore route to the
// sql evaluator, byte-identically to before.
func kindOf(c config.Check) string {
	if c.Kind == "" {
		return "sql"
	}
	return c.Kind
}

// Run evaluates every check against target and returns a result per check, in
// order. Each check is dispatched by its kind to the registered evaluator;
// severity is carried through so the caller's verdict rule is unchanged. An
// unknown kind (no registered evaluator) yields a clear failing result rather
// than a panic, so a misconfigured kind fails the verdict instead of the process.
func Run(ctx context.Context, target Target, checks []config.Check) []report.CheckResult {
	out := make([]report.CheckResult, 0, len(checks))
	for _, c := range checks {
		kind := kindOf(c)
		eval, ok := evaluators[kind]
		if !ok {
			out = append(out, report.CheckResult{
				Name:     c.Name,
				Severity: c.Severity,
				Error:    fmt.Sprintf("unknown check kind %q", kind),
			})
			continue
		}
		out = append(out, eval(ctx, target, c))
	}
	return out
}
