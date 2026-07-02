// Package mongodb implements the MongoDB engine (spec 0025): a document-store
// engine registered against target.type "mongodb". Unlike MySQL (spec 0024,
// which reuses the existing "sql" check kind unchanged) and unlike restic/borg
// (which share internal/probe's file/command kinds because they are both
// "things with a filesystem"), MongoDB has neither a SQL surface nor a
// filesystem to probe. It restores a mongodump archive into a throwaway
// container and registers two check kinds of its own —
// collection_count/doc_query — extending the check-kind seam (spec 0017 R3) a
// third way: an engine that owns kinds self-contained to itself, not shared via
// internal/probe and not the reused sql kind.
package mongodb

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"salvage.sh/internal/checks"
	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/report"
)

func init() {
	spi.Register(Engine{})
	checks.RegisterEvaluator("collection_count", evalCollectionCount)
	checks.RegisterEvaluator("doc_query", evalDocQuery)
	// The engine owns the load-time validation of its check kinds, just as it
	// owns their evaluators — the additive-extension seam (spec 0016 R6): no
	// case for these kinds exists in config.Validate.
	config.RegisterCheckValidator("collection_count", validateCollectionCountCheck)
	config.RegisterCheckValidator("doc_query", validateDocQueryCheck)
}

// Engine is the MongoDB engine. Stateless; each Restore stands up its own
// throwaway container.
type Engine struct{}

// The engine contributes its own config validation (spec 0016 R6, backlog S5).
var _ spi.ConfigValidator = Engine{}

func (Engine) Type() string { return "mongodb" }

// ValidateConfig checks a target.type mongodb source (spec 0025): a logical
// mongodump --archive file, mirroring the MySQL engine's shape. MongoDB has
// only one source kind in v1 (no physical/oplog restore yet — see spec 0025
// Open questions). These rules — and their messages — used to live in
// config.Validate's central switch; the engine now contributes them itself via
// spi.ConfigValidator.
func (Engine) ValidateConfig(cfg *config.Config) error {
	t := cfg.Target
	switch t.Source.Kind {
	case "mongodb", "":
		if t.Source.Path == "" {
			return fmt.Errorf("target.source.path is required for target.type mongodb")
		}
		if _, err := os.Stat(t.Source.Path); err != nil {
			return fmt.Errorf("target.source.path: %w", err)
		}
	default:
		return fmt.Errorf("target.source.kind %q unsupported for target.type mongodb (only \"mongodb\")", t.Source.Kind)
	}
	return nil
}

// validateCollectionCountCheck is the load-time rule for the collection_count
// kind, registered with config so a mistake fails `salvage check` at load
// rather than at runtime. Messages are unchanged from the pre-seam
// config.Validate.
func validateCollectionCountCheck(targetType string, i int, ck config.Check) error {
	if targetType != "mongodb" {
		return fmt.Errorf("checks[%d] (%s): kind %q is only valid for target.type mongodb", i, ck.Name, "collection_count")
	}
	if ck.Collection == "" {
		return fmt.Errorf("checks[%d] (%s): collection is required for kind collection_count", i, ck.Name)
	}
	if ck.ExpectMin == nil && ck.ExpectMax == nil && ck.Equals == nil {
		return fmt.Errorf("checks[%d] (%s): needs an expectation (expect_min/expect_max or equals) for kind collection_count", i, ck.Name)
	}
	return nil
}

// validateDocQueryCheck is the load-time rule for the doc_query kind. Alongside
// equals/expect_min/expect_max it accepts max_age (backlog S3): a doc_query
// check can assert a timestamp field is no older than a configured window,
// evaluated by the same shared freshness logic the sql kind uses.
func validateDocQueryCheck(targetType string, i int, ck config.Check) error {
	if targetType != "mongodb" {
		return fmt.Errorf("checks[%d] (%s): kind %q is only valid for target.type mongodb", i, ck.Name, "doc_query")
	}
	if ck.Collection == "" {
		return fmt.Errorf("checks[%d] (%s): collection is required for kind doc_query", i, ck.Name)
	}
	if ck.Filter == "" {
		return fmt.Errorf("checks[%d] (%s): filter is required for kind doc_query", i, ck.Name)
	}
	if ck.Field == "" {
		return fmt.Errorf("checks[%d] (%s): field is required for kind doc_query", i, ck.Name)
	}
	if ck.ExpectMin == nil && ck.ExpectMax == nil && ck.Equals == nil && ck.MaxAge == nil {
		return fmt.Errorf("checks[%d] (%s): needs an expectation (equals, expect_min, expect_max, or max_age) for kind doc_query", i, ck.Name)
	}
	return nil
}

// Restore stands up a throwaway MongoDB container and loads the configured
// mongodump archive into it, returning a live RestoredTarget whose
// CountDocuments/FindOneField shell to the mongosh CLI (satisfying
// MongoQueryer). It preserves the operational-vs-verdict split:
// environment/secret/Docker problems are wrapped as spi.Fault (operational); an
// archive that fails to load is a bare error (a "fail" verdict).
//
// v1 supports only a logical (mongodump --archive) restore — see spec 0025
// Open questions for a physical/oplog restore, which is deferred.
func (Engine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, "", spi.Faultf(err) // operational: docker missing/unreachable
	}
	src := cfg.Target.Source
	if err := requireEnv(src.PassEnv); err != nil {
		return nil, "", spi.Faultf(err)
	}
	db, err := ephemeral.StartMongoDB(ctx, cfg.Target.Restore.Image, cfg.Target.Restore.Database)
	if err != nil {
		return nil, "", spi.Faultf(err) // operational: couldn't create the environment
	}
	if _, err := db.Restore(ctx, src.Path); err != nil {
		_ = db.Stop()
		return nil, "", err // verdict fail: the archive did not load
	}
	return db, "", nil
}

// requireEnv fails if any named pass_env var is unset — the same by-name secret
// precondition the other engines enforce before touching Docker. MongoDB v1 has
// no required pass_env vars (the root credential is a fixed dev credential
// inside the throwaway container, never the customer's), but source.pass_env is
// still honored/forwarded should a future source need it.
func requireEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) == "" {
			return fmt.Errorf("required env %s is not set (export it before running salvage)", name)
		}
	}
	return nil
}

// MongoQueryer is the capability the collection_count/doc_query evaluators
// require of a checks.Target — the MongoDB analogue of checks.Queryer (sql) and
// probe.FileProber (restic/borg/exec). *ephemeral.MongoDB implements it by
// shelling to mongosh.
type MongoQueryer interface {
	// CountDocuments returns the number of documents in collection matching
	// filterJSON (a JSON filter document; "" counts every document).
	CountDocuments(ctx context.Context, collection, filterJSON string) (int64, error)
	// FindOneField runs findOne(filterJSON) against collection and returns the
	// dotted field path's value as a scalar string. It errors if no document
	// matches, or if the field is absent on the matched document.
	FindOneField(ctx context.Context, collection, filterJSON, field string) (string, error)
}

// queryer type-asserts the opaque target to MongoQueryer, returning a clear
// failing result when the target cannot answer Mongo queries (e.g. a SQL or
// filesystem engine's target reached a mongodb check). Never panics — the same
// pattern internal/checks/sql.go and internal/probe already use.
func queryer(target checks.Target, c config.Check) (MongoQueryer, *report.CheckResult) {
	q, ok := target.(MongoQueryer)
	if !ok {
		return nil, &report.CheckResult{
			Name:     c.Name,
			Severity: c.Severity,
			Error:    "this check requires a MongoDB-queryable target (target.type mongodb)",
		}
	}
	return q, nil
}

// evalCollectionCount evaluates the "collection_count" kind: db.<collection>.
// countDocuments(<filter>) as a scalar, then hands the count to the shared
// scalar-expectation evaluator (checks.EvaluateScalar) — the same
// expect_min/expect_max/equals code path the sql kind runs.
func evalCollectionCount(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	q, fail := queryer(target, c)
	if fail != nil {
		return *fail
	}
	if c.Collection == "" {
		res.Error = "collection_count check requires collection"
		return res
	}
	n, err := q.CountDocuments(ctx, c.Collection, c.Filter)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if !checks.EvaluateScalar(strconv.FormatInt(n, 10), c, &res) {
		res.Error = "collection_count check needs an expectation (expect_min/expect_max or equals)"
	}
	return res
}

// evalDocQuery evaluates the "doc_query" kind: findOne(filter) against
// collection, extracting field's value as a scalar string, then handing it to
// the shared scalar-expectation evaluator (checks.EvaluateScalar) — the exact
// code path the sql kind runs, so equals/expect_min/expect_max behave
// identically, and max_age freshness (backlog S3) works against a timestamp
// field (mongosh renders BSON dates as ISO-8601 via Date.toISOString()).
func evalDocQuery(ctx context.Context, target checks.Target, c config.Check) report.CheckResult {
	res := report.CheckResult{Name: c.Name, Severity: c.Severity}
	q, fail := queryer(target, c)
	if fail != nil {
		return *fail
	}
	if c.Collection == "" || c.Filter == "" || c.Field == "" {
		res.Error = "doc_query check requires collection, filter, and field"
		return res
	}
	got, err := q.FindOneField(ctx, c.Collection, c.Filter, c.Field)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if !checks.EvaluateScalar(got, c, &res) {
		res.Error = "doc_query check needs an expectation (equals, expect_min, expect_max, or max_age)"
	}
	return res
}
