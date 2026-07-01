// Package config loads and validates a Salvage target definition.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level Salvage configuration for a single restore-test target.
type Config struct {
	Target Target  `yaml:"target"`
	Report Report  `yaml:"report"`
	Attest *Attest `yaml:"attest,omitempty"`
}

// Attest configures submission to a hosted attestation notary (spec 0012). The
// API key is never stored in the file: APIKeyEnv names the environment variable
// that holds it (like Source.PassEnv), read at submit time.
type Attest struct {
	// Endpoint is the notary base URL, e.g. https://attest.salvage.sh
	Endpoint string `yaml:"endpoint"`
	// APIKeyEnv names the env var holding the tenant API key (default
	// SALVAGE_ATTEST_KEY).
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
}

// Target describes the backup to test and how to bring it back to life.
type Target struct {
	// Name is a human label used in reports (e.g. "prod-orders-db").
	Name string `yaml:"name"`
	// Type selects the engine. Only "postgres" is supported today.
	Type    string  `yaml:"type"`
	Source  Source  `yaml:"source"`
	Restore Restore `yaml:"restore"`
	Checks  []Check `yaml:"checks"`
}

// Source points at the backup to restore.
//
//   - Logical ("pg_dump"/"sql"): a local dump file given by Path.
//   - Physical ("pgbackrest"): a pgBackRest repo, restored via the tool's own
//     `restore` command. RepoVolume (optional) mounts a local-filesystem repo;
//     for a remote repo (S3/R2) omit it and rely on the image's pgbackrest.conf,
//     forwarding credentials via PassEnv.
type Source struct {
	Kind string `yaml:"kind"` // pg_dump | sql | pgbackrest

	// Logical sources.
	Path string `yaml:"path,omitempty"`

	// pgBackRest sources.
	Stanza     string `yaml:"stanza,omitempty"`
	RepoVolume string `yaml:"repo_volume,omitempty"`
	RepoPath   string `yaml:"repo_path,omitempty"`
	// PassEnv forwards named environment variables from Salvage's own process
	// into the restore container *by name*, so secret values never appear in
	// command arguments — e.g. PGBACKREST_REPO1_S3_KEY[_SECRET] for an S3/R2 repo.
	PassEnv []string `yaml:"pass_env,omitempty"`
}

// Restore configures the throwaway environment the backup is rehydrated into.
type Restore struct {
	// Image is the container image. Logical restores default to "postgres:16";
	// pgBackRest restores need an image carrying both postgres and pgbackrest
	// (plus any extensions the source uses, e.g. timescaledb) — no default.
	Image string `yaml:"image"`
	// Database is the database checks connect to.
	Database string `yaml:"database"`
	// User is the role checks connect as (default "postgres").
	User string `yaml:"user,omitempty"`
	// PreloadLibraries seeds shared_preload_libraries in a synthesized
	// postgresql.conf when the restored cluster keeps its config outside PGDATA
	// (e.g. Debian-packaged Postgres) — needed for extensions like timescaledb.
	PreloadLibraries []string `yaml:"preload_libraries,omitempty"`
	// Timeout bounds the whole restore phase.
	Timeout Duration `yaml:"timeout"`
}

// Check is a single assertion run against the restored database. Exactly one of
// the expectation fields should be set.
type Check struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`

	ExpectMin *float64  `yaml:"expect_min,omitempty"`
	ExpectMax *float64  `yaml:"expect_max,omitempty"`
	Equals    *string   `yaml:"equals,omitempty"`
	MaxAge    *Duration `yaml:"max_age,omitempty"`
	// Bool asserts the scalar SQL result is a boolean equal to *Bool
	// (e.g. SELECT count(*) = 0 FROM orphaned_rows must be true).
	Bool *bool `yaml:"bool,omitempty"`

	// Severity is "required" (a failure fails the verdict) or "advisory" (a
	// failure is recorded but does not fail the verdict). Defaults to "required".
	Severity string `yaml:"severity,omitempty"`
}

// Report controls verdict output.
type Report struct {
	Format  string `yaml:"format,omitempty"`
	Out     string `yaml:"out,omitempty"`
	Sign    bool   `yaml:"sign,omitempty"`
	KeyPath string `yaml:"key_path,omitempty"`
}

// Duration is a time.Duration that unmarshals from a Go duration string ("24h").
type Duration time.Duration

// UnmarshalYAML parses a duration string like "10m" or "24h".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// MarshalYAML renders the duration as a parseable string (e.g. "30m0s"), the
// inverse of UnmarshalYAML — so a Config round-trips cleanly through YAML.
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Load reads, decodes, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	t := &c.Target
	if t.Type == "" {
		t.Type = "postgres"
	}
	switch t.Source.Kind {
	case "pgbackrest":
		if t.Restore.Database == "" {
			t.Restore.Database = "postgres"
		}
		if t.Source.RepoPath == "" {
			t.Source.RepoPath = "/var/lib/pgbackrest"
		}
	default:
		if t.Restore.Image == "" {
			t.Restore.Image = "postgres:16"
		}
		if t.Restore.Database == "" {
			t.Restore.Database = "salvage_restore_test"
		}
	}
	if t.Restore.User == "" {
		t.Restore.User = "postgres"
	}
	if t.Restore.Timeout == 0 {
		t.Restore.Timeout = Duration(10 * time.Minute)
	}
	if c.Report.Format == "" {
		c.Report.Format = "json"
	}
	for i := range t.Checks {
		if t.Checks[i].Severity == "" {
			t.Checks[i].Severity = "required"
		}
	}
}

// Validate returns the first configuration error, if any.
func (c *Config) Validate() error {
	t := c.Target
	if t.Type != "postgres" {
		return fmt.Errorf("target.type %q unsupported (only \"postgres\" in v1)", t.Type)
	}
	switch t.Source.Kind {
	case "pg_dump", "sql":
		if t.Source.Path == "" {
			return fmt.Errorf("target.source.path is required for kind %q", t.Source.Kind)
		}
		if _, err := os.Stat(t.Source.Path); err != nil {
			return fmt.Errorf("target.source.path: %w", err)
		}
	case "pgbackrest":
		if t.Source.Stanza == "" {
			return fmt.Errorf("target.source.stanza is required for pgbackrest")
		}
		if t.Restore.Image == "" {
			return fmt.Errorf("target.restore.image is required for pgbackrest (needs postgres + pgbackrest)")
		}
	case "":
		return fmt.Errorf("target.source.kind is required (pg_dump|sql|pgbackrest)")
	default:
		return fmt.Errorf("target.source.kind %q unsupported (pg_dump|sql|pgbackrest)", t.Source.Kind)
	}
	for i, ck := range t.Checks {
		if ck.Name == "" {
			return fmt.Errorf("checks[%d]: name is required", i)
		}
		if ck.SQL == "" {
			return fmt.Errorf("checks[%d] (%s): sql is required", i, ck.Name)
		}
		n := 0
		if ck.ExpectMin != nil || ck.ExpectMax != nil {
			n++
		}
		if ck.Equals != nil {
			n++
		}
		if ck.MaxAge != nil {
			n++
		}
		if ck.Bool != nil {
			n++
		}
		if n == 0 {
			return fmt.Errorf("checks[%d] (%s): needs one expectation (expect_min/expect_max, equals, max_age, or bool)", i, ck.Name)
		}
		if n > 1 {
			return fmt.Errorf("checks[%d] (%s): exactly one expectation allowed (expect_min/expect_max, equals, max_age, or bool)", i, ck.Name)
		}
		switch ck.Severity {
		case "", "required", "advisory":
		default:
			return fmt.Errorf("checks[%d] (%s): severity %q unsupported (required|advisory)", i, ck.Name, ck.Severity)
		}
	}
	if c.Report.Format != "json" {
		return fmt.Errorf("report.format %q unsupported (only \"json\" in v1)", c.Report.Format)
	}
	return nil
}
