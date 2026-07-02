// Package mysql implements the MySQL engine (spec 0024): a SQL engine
// registered against target.type "mysql". Unlike restic/borg (filesystem
// engines validated with file/command probes), MySQL is structurally the
// Postgres engine's closest sibling: it restores a logical dump into a
// throwaway MySQL container and returns a target that answers scalar SQL
// queries — so it reuses the existing "sql" check kind (internal/checks/sql.go)
// unchanged. No new check evaluator is registered here.
package mysql

import (
	"context"
	"fmt"
	"os"

	"salvage.sh/internal/config"
	"salvage.sh/internal/engine/spi"
	"salvage.sh/internal/ephemeral"
)

func init() { spi.Register(Engine{}) }

// Engine is the MySQL engine. Stateless; each Restore stands up its own
// throwaway container.
type Engine struct{}

// The engine contributes its own config validation (spec 0016 R6).
var _ spi.ConfigValidator = Engine{}

func (Engine) Type() string { return "mysql" }

// ValidateConfig checks a target.type mysql source (spec 0024): a logical dump
// file, the SQL-engine sibling of Postgres's pg_dump/sql validation. MySQL has
// only one source kind in v1 (no physical/binlog restore yet — see spec 0024
// Open questions), so the shape is simpler than Postgres's switch. These rules
// — and their messages — used to live in config.Validate's central switch.
func (Engine) ValidateConfig(cfg *config.Config) error {
	t := cfg.Target
	switch t.Source.Kind {
	case "mysql", "":
		if t.Source.Path == "" {
			return fmt.Errorf("target.source.path is required for target.type mysql")
		}
		if _, err := os.Stat(t.Source.Path); err != nil {
			return fmt.Errorf("target.source.path: %w", err)
		}
	default:
		return fmt.Errorf("target.source.kind %q unsupported for target.type mysql (only \"mysql\")", t.Source.Kind)
	}
	return nil
}

// Restore stands up a throwaway MySQL container and loads the configured dump
// into it, returning a live RestoredTarget whose Query shells to the mysql CLI
// (satisfying checks.Queryer). It preserves the operational-vs-verdict split:
// environment/secret/Docker problems are wrapped as spi.Fault (operational); a
// dump that fails to load is a bare error (a "fail" verdict).
//
// v1 supports only a logical (.sql dump) restore — see spec 0024 Open
// questions for physical/binlog restore, which is deferred.
func (Engine) Restore(ctx context.Context, cfg *config.Config) (spi.RestoredTarget, string, error) {
	if err := ephemeral.Preflight(ctx); err != nil {
		return nil, "", spi.Faultf(err) // operational: docker missing/unreachable
	}
	src := cfg.Target.Source
	if err := requireEnv(src.PassEnv); err != nil {
		return nil, "", spi.Faultf(err)
	}
	db, err := ephemeral.StartMySQL(ctx, cfg.Target.Restore.Image, cfg.Target.Restore.Database)
	if err != nil {
		return nil, "", spi.Faultf(err) // operational: couldn't create the environment
	}
	if _, err := db.Restore(ctx, src.Path); err != nil {
		_ = db.Stop()
		return nil, "", err // verdict fail: the dump did not load
	}
	return db, "", nil
}

// requireEnv fails if any named pass_env var is unset — the same by-name secret
// precondition the other engines enforce before touching Docker. MySQL v1 has no
// required pass_env vars (the root password is a fixed dev credential inside the
// throwaway container, never the customer's), but source.pass_env is still
// honored/forwarded should a future source need it.
func requireEnv(names []string) error {
	for _, name := range names {
		if os.Getenv(name) == "" {
			return fmt.Errorf("required env %s is not set (export it before running salvage)", name)
		}
	}
	return nil
}
