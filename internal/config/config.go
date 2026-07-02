// Package config loads and validates a Salvage target definition.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"salvage.sh/internal/alert"
)

// Config is the top-level Salvage configuration for a single restore-test target.
type Config struct {
	Target Target  `yaml:"target"`
	Report Report  `yaml:"report"`
	Attest *Attest `yaml:"attest,omitempty"`
	Alerts *Alerts `yaml:"alerts,omitempty"`
}

// Alerts configures the optional client-side alert hooks (spec 0030 R1,
// realizing spec 0007 R4). Each hook is a command line (run via `sh -c` with
// the report JSON on stdin and the report path in $SALVAGE_REPORT) or an
// http(s):// URL (the report JSON is POSTed as application/json). on_fail
// fires on a fail verdict or an operational error; on_success on a pass.
// Hooks are best-effort: their failure is logged and never changes the run's
// exit code (spec 0030 R2), and no daemon is involved — the hook runs inline
// in the one-shot process.
//
// A URL hook's token is passed by reference, never embedded (spec 0030 R7):
// a `token_ref=env:SALVAGE_HOOK_TOKEN` query parameter is resolved from the
// environment at delivery time into `token=<value>`, so no secret is written
// into this file — the same by-reference posture as Source.PassEnv.
type Alerts struct {
	OnFail    string `yaml:"on_fail,omitempty"`
	OnSuccess string `yaml:"on_success,omitempty"`
	// Timeout bounds each hook invocation (spec 0030 R2). Default 30s.
	Timeout Duration `yaml:"timeout,omitempty"`
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
	// SecretScan controls the pre-submission credential-pattern gate (spec 0027
	// R7): a scan of the final report bytes for common credential shapes (AWS
	// access-key ids, PEM private keys, bearer tokens, URL-embedded user:pass@)
	// run before anything reaches the notary.
	//   - "refuse" (the default when empty): a match refuses the submission.
	//   - "warn": a match prints a loud stderr warning and proceeds.
	//   - "off": the gate is disabled.
	// This is a backstop for secrets outside the known-value scrub (a credential
	// the restore script fetched itself); the default is the safe one because a
	// secret counter-signed into the shared ledger cannot be unpublished.
	SecretScan string `yaml:"secret_scan,omitempty"`
}

// Target describes the backup to test and how to bring it back to life.
type Target struct {
	// Name is a human label used in reports (e.g. "prod-orders-db").
	Name string `yaml:"name"`
	// Type selects the engine: "postgres"/"mysql" (SQL), "restic"/"borg"
	// (filesystem), "mongodb" (document store, spec 0025), or "exec"
	// (bring-your-own-restore: run a customer command, spec 0020).
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
//   - Filesystem ("restic"): a restic repo (target.type restic). A Snapshot
//     (default "latest") is restored via `restic restore`. The repository comes
//     from RESTIC_REPOSITORY via PassEnv, or from Repository (a plain path/URL,
//     never a secret). RepoVolume (optional) mounts a local-filesystem repo.
//   - Filesystem ("borg"): a BorgBackup repo (target.type borg). An Archive
//     (required) is extracted via `borg extract`. The repository comes from
//     BORG_REPO via PassEnv, or from Repository (a plain path/URL, never a
//     secret); the passphrase is forwarded by name (BORG_PASSPHRASE) via PassEnv.
//     RepoVolume (optional) mounts a local-filesystem repo. A near-exact sibling
//     of the restic source (spec 0022).
//   - Logical ("mysql", target.type mysql, spec 0024): a local `.sql` dump file
//     given by Path, loaded into a throwaway MySQL container via the `mysql`
//     CLI — the SQL-engine sibling of Postgres's "sql" kind. Checks against the
//     restored target reuse the existing `sql` check kind unchanged.
//   - Logical ("mongodb", target.type mongodb, spec 0025): a local
//     `mongodump --archive` file given by Path, loaded into a throwaway MongoDB
//     container via `mongorestore`. MongoDB is neither a SQL nor a filesystem
//     engine, so it registers its own check kinds (collection_count/doc_query)
//     rather than reusing sql or the restic/borg file/command kinds.
type Source struct {
	Kind string `yaml:"kind"` // pg_dump | sql | pgbackrest | restic | borg | mysql | mongodb

	// Logical sources.
	Path string `yaml:"path,omitempty"`

	// pgBackRest sources.
	Stanza     string `yaml:"stanza,omitempty"`
	RepoVolume string `yaml:"repo_volume,omitempty"`
	RepoPath   string `yaml:"repo_path,omitempty"`

	// restic sources.
	//
	// Snapshot pins the snapshot to restore ("latest" by default). Repository is
	// a non-secret repo location (path/URL); it is set as RESTIC_REPOSITORY in the
	// container. When the repository or its password *is* a secret (or a backend
	// needs credentials), leave Repository empty and forward the value by name via
	// PassEnv (RESTIC_REPOSITORY, RESTIC_PASSWORD, AWS_*, B2_*, etc.) — exactly the
	// by-reference model the pgBackRest path uses.
	Snapshot   string `yaml:"snapshot,omitempty"`
	Repository string `yaml:"repository,omitempty"`

	// borg sources (spec 0022).
	//
	// Archive pins the BorgBackup archive to extract (required; borg has no
	// "latest" alias like restic). The repository handling mirrors restic:
	// Repository is a non-secret repo location set as BORG_REPO, or forward
	// BORG_REPO by name via PassEnv when it is a secret; the passphrase is always
	// forwarded by name (BORG_PASSPHRASE) via PassEnv, never written in-file.
	Archive string `yaml:"archive,omitempty"`

	// PassEnv forwards named environment variables from Salvage's own process
	// into the restore container *by name*, so secret values never appear in
	// command arguments — e.g. PGBACKREST_REPO1_S3_KEY[_SECRET] for an S3/R2 repo,
	// or RESTIC_PASSWORD / AWS_* for a restic repo.
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

	// The following fields configure the exec engine (target.type exec, spec
	// 0020): Salvage runs the customer's own restore command instead of standing
	// up a container. They are yaml-omitempty so container-engine configs are
	// unaffected.
	//
	//   - Command is the argv slice Salvage runs to perform the restore. Its
	//     exit 0 means the restore succeeded; a non-zero exit is a "fail" verdict
	//     (not an operational error). Required for target.type exec.
	//   - Env names host environment variables to pass through to the command
	//     (by name — values come from Salvage's own process), like Source.PassEnv.
	//   - Workdir is the command's working directory (optional).
	//   - Cleanup is an optional argv run on Stop() (idempotent; its failure is a
	//     warning, never a verdict change).
	Command []string `yaml:"command,omitempty"`
	Env     []string `yaml:"env,omitempty"`
	Workdir string   `yaml:"workdir,omitempty"`
	Cleanup []string `yaml:"cleanup,omitempty"`

	// Timeout bounds the whole restore phase.
	Timeout Duration `yaml:"timeout"`
}

// Check is a single assertion run against the restored database. Exactly one of
// the expectation fields should be set.
type Check struct {
	Name string `yaml:"name"`
	// Kind selects the evaluator (spec 0017 R3). Empty means "sql" — the only
	// kind today and the historical behaviour — so existing configs are
	// unchanged. Non-SQL engines (restic/borg, MongoDB, object-storage) will
	// register their own kinds; the orchestration, verdict, and report are the
	// same for every kind.
	Kind string `yaml:"kind,omitempty"`
	SQL  string `yaml:"sql"`

	// Path and Command are the subjects for non-SQL kinds; the sql kind ignores
	// them. Both are yaml-omitempty so existing SQL configs are byte-identical.
	//   - file kinds (file_exists, file_count, checksum) probe Path (a path or,
	//     for file_count, a `find`-style glob) within the restored tree.
	//   - the command kind runs Command in the restored tree.
	// The expectation fields are reused across kinds: Bool for file_exists,
	// ExpectMin/ExpectMax for file_count, Equals for checksum/command stdout.
	Path    string `yaml:"path,omitempty"`
	Command string `yaml:"command,omitempty"`

	// http kind fields (spec 0020 R3), consumed by the exec engine's HTTPProber.
	// All are yaml-omitempty so non-http checks are byte-identical.
	//   - URL is the request target (required for kind http).
	//   - Method defaults to GET.
	//   - Headers/Body are optional request headers and body.
	//   - ExpectStatus defaults to 200.
	//   - ExpectBodyContains asserts the response body contains a substring.
	//   - ExpectJSON is a single "dotted.path=value" assertion over a JSON body.
	URL                string            `yaml:"url,omitempty"`
	Method             string            `yaml:"method,omitempty"`
	Headers            map[string]string `yaml:"headers,omitempty"`
	Body               string            `yaml:"body,omitempty"`
	ExpectStatus       *int              `yaml:"expect_status,omitempty"`
	ExpectBodyContains string            `yaml:"expect_body_contains,omitempty"`
	ExpectJSON         string            `yaml:"expect_json,omitempty"`

	// MongoDB kind fields (spec 0025), consumed by internal/engine/mongodb's
	// collection_count/doc_query evaluators. All are yaml-omitempty so non-mongodb
	// checks are byte-identical.
	//   - Collection is the collection name (required for both kinds).
	//   - Filter is a JSON filter document string (e.g. `{"status":"active"}`).
	//     Optional for collection_count (empty/omitted counts every document);
	//     required for doc_query (it identifies the one document to read).
	//   - Field is the dotted field path to read from the matched document for
	//     doc_query (e.g. "status" or "meta.version"). Not used by
	//     collection_count.
	// The expectation fields are reused across kinds: ExpectMin/ExpectMax/Equals
	// for collection_count (a scalar count); Equals/ExpectMin/ExpectMax/MaxAge
	// for doc_query (a scalar field value; max_age asserts a timestamp field is
	// no older than the configured window).
	Collection string `yaml:"collection,omitempty"`
	Filter     string `yaml:"filter,omitempty"`
	Field      string `yaml:"field,omitempty"`

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

	// KeepLiteral is the explicit opt-in (spec 0027 R2/R5) to store the check's
	// exact got value in the report instead of the default bounded, scrubbed
	// preview — for a command/sql check whose byte-equal `equals` assertion
	// needs the full literal recorded. It requires an equals expectation, and
	// known-secret scrubbing (spec 0027 R3) still applies to the stored value:
	// this widens the bound, never the secret discipline.
	KeepLiteral bool `yaml:"keep_literal,omitempty"`
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
	c, err := parse(b)
	if err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// parse strictly decodes a config document. Decoding is strict
// (KnownFields): an unknown or misspelled key (expct_min, snapshto, pass_evn)
// is a load error naming the key, never a silently dropped field — a dropped
// expectation or source field could otherwise disable a check with no error at
// load.
func parse(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		// An empty document is not a parse error; validation reports what's
		// missing — the same outcome as the pre-strict decoder.
		if errors.Is(err, io.EOF) {
			return &c, nil
		}
		return nil, fmt.Errorf("parse config: %w", err)
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
	case "restic":
		// Filesystem engine (spec 0018): no database. The restic/restic image
		// carries the restic binary; snapshot "latest" is the sensible default.
		// The image is pinned to the version verified end-to-end (backlog S10,
		// dev/restic/VERIFIED.md: restic 0.19.0) so a pinned Salvage release does
		// not change behavior when upstream retags; users override restore.image.
		if t.Restore.Image == "" {
			t.Restore.Image = "restic/restic:0.19.0"
		}
		if t.Source.Snapshot == "" {
			t.Source.Snapshot = "latest"
		}
		// A local-repo volume mounts where the repository points; default the
		// mount path to the (non-secret, local) repository path so a config that
		// sets repository + repo_volume needs no explicit repo_path.
		if t.Source.RepoVolume != "" && t.Source.RepoPath == "" {
			t.Source.RepoPath = t.Source.Repository
		}
	case "borg":
		// Filesystem engine (spec 0022): no database. borg publishes no official
		// image; the borgmatic image ships borg on PATH. Pinned to a release tag
		// carrying the borg version verified end-to-end (backlog S10,
		// dev/borg/VERIFIED.md: borg 1.4.4); users override restore.image. borg
		// has no "latest" archive alias, so there is no snapshot/archive default —
		// the archive is required (validated below).
		if t.Restore.Image == "" {
			t.Restore.Image = "ghcr.io/borgmatic-collective/borgmatic:2.1.6"
		}
		if t.Source.RepoVolume != "" && t.Source.RepoPath == "" {
			t.Source.RepoPath = t.Source.Repository
		}
	case "mysql":
		// SQL engine (spec 0024): a logical dump loaded into a throwaway MySQL
		// container. The image is pinned to the version verified end-to-end
		// (backlog S10, dev/mysql/VERIFIED.md: MySQL Server 8.4.10); the database
		// defaults to a dedicated restore-test schema, mirroring the Postgres
		// logical default.
		if t.Restore.Image == "" {
			t.Restore.Image = "mysql:8.4.10"
		}
		if t.Restore.Database == "" {
			t.Restore.Database = "salvage_restore_test"
		}
		if t.Restore.User == "" {
			t.Restore.User = "root"
		}
	case "mongodb":
		// Document-store engine (spec 0025): a mongodump archive loaded into a
		// throwaway MongoDB container. The image is pinned to the verified
		// version (backlog S10: MongoDB 7.0.37, which bundles mongosh — see spec
		// 0025 Open questions); the database defaults to a dedicated restore-test
		// namespace, mirroring MySQL.
		if t.Restore.Image == "" {
			t.Restore.Image = "mongo:7.0.37"
		}
		if t.Restore.Database == "" {
			t.Restore.Database = "salvage_restore_test"
		}
		if t.Restore.User == "" {
			t.Restore.User = "root"
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

// TargetValidator validates the engine-specific parts of a loaded Config — the
// source shape and any restore fields the engine requires. Engines contribute
// one via spi.Register (implementing spi.ConfigValidator) or directly via
// RegisterTargetValidator; a nil validator marks the target.type as known with
// no engine-specific rules.
type TargetValidator func(c *Config) error

// CheckValidator validates one check of a registered kind at load time — the
// engine-owned counterpart of the core kinds validateCheck knows (sql, the
// file/command probe kinds, http). targetType lets a validator restrict its
// kind to the engine that owns it.
type CheckValidator func(targetType string, i int, ck Check) error

// TargetCapability names one probe capability an engine's RestoredTarget
// exposes to the shared check kinds (backlog S4). The core kinds validateCheck
// gates on capability, not on a hardcoded engine list, so a new engine lights
// up the file/command/http kinds by declaring the capability at registration
// (spi.CapabilityDeclarer) — the same additive-extension promise as spec 0016
// R6.
type TargetCapability string

const (
	// CapabilityFileProbe: the RestoredTarget satisfies probe.FileProber — the
	// file_exists/file_count/checksum/command kinds can run against it.
	CapabilityFileProbe TargetCapability = "file-probe"
	// CapabilityHTTPProbe: the RestoredTarget satisfies probe.HTTPProber — the
	// http kind can run against it.
	CapabilityHTTPProbe TargetCapability = "http-probe"
)

// The validation registries (spec 0016 R6): populated by init()s — engine
// registration via spi.Register, or direct Register*Validator calls — and only
// read after that, so no locking is needed, mirroring the engine SPI registry
// and checks.RegisterEvaluator.
var (
	targetValidators   = map[string]TargetValidator{}
	checkValidators    = map[string]CheckValidator{}
	targetCapabilities = map[string]map[TargetCapability]bool{}
)

// RegisterTargetValidator registers v as the engine-specific validation for
// targetType (nil = known type, no extra validation). It panics on an empty
// type or a duplicate registration — programmer errors caught at init.
func RegisterTargetValidator(targetType string, v TargetValidator) {
	if targetType == "" {
		panic("config: RegisterTargetValidator with empty target type")
	}
	if _, dup := targetValidators[targetType]; dup {
		panic("config: duplicate target validator for type " + targetType)
	}
	targetValidators[targetType] = v
}

// RegisterCheckValidator registers v as the load-time validation for an
// engine-owned check kind. It panics on an empty kind, a nil validator, or a
// duplicate registration — programmer errors caught at init.
func RegisterCheckValidator(kind string, v CheckValidator) {
	if kind == "" {
		panic("config: RegisterCheckValidator with empty kind")
	}
	if v == nil {
		panic("config: RegisterCheckValidator with nil validator for kind " + kind)
	}
	if _, dup := checkValidators[kind]; dup {
		panic("config: duplicate check validator for kind " + kind)
	}
	checkValidators[kind] = v
}

// RegisterTargetCapabilities declares which shared probe capabilities
// targetType's RestoredTarget exposes. Wired by spi.Register when the engine
// implements spi.CapabilityDeclarer; safe to call more than once (capabilities
// accumulate — they describe the target, they do not conflict).
func RegisterTargetCapabilities(targetType string, caps ...TargetCapability) {
	if targetType == "" {
		panic("config: RegisterTargetCapabilities with empty target type")
	}
	m := targetCapabilities[targetType]
	if m == nil {
		m = map[TargetCapability]bool{}
		targetCapabilities[targetType] = m
	}
	for _, c := range caps {
		m[c] = true
	}
}

// targetHasCapability reports whether targetType declared cap. When no engine
// has registered any capabilities at all (a library/test context that imports
// only this package), it falls back to the historical built-in sets so
// engine-less validation behaves as before — mirroring how target.type
// validation defers when targetValidators is empty.
func targetHasCapability(targetType string, cap TargetCapability) bool {
	if len(targetCapabilities) == 0 {
		switch targetType {
		case "restic", "borg", "exec":
			return true
		default:
			return false
		}
	}
	return targetCapabilities[targetType][cap]
}

// capabilityTypes returns the target types declaring cap, sorted, for error
// messages ("only valid for target.type borg, exec, or restic").
func capabilityTypes(cap TargetCapability) []string {
	if len(targetCapabilities) == 0 {
		return []string{"borg", "exec", "restic"} // the built-in fallback set
	}
	out := make([]string, 0, len(targetCapabilities))
	for t, caps := range targetCapabilities {
		if caps[cap] {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// orList joins names as "a", "a or b", or "a, b, or c" for error messages.
func orList(names []string) string {
	switch len(names) {
	case 0:
		return "(none)"
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

// TargetTypes returns the registered target types, sorted, for error messages
// and diagnostics.
func TargetTypes() []string {
	out := make([]string, 0, len(targetValidators))
	for t := range targetValidators {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Validate returns the first configuration error, if any.
//
// The engine allow-list step (spec 0016 R6) is contributed by the engines
// themselves: each engine registers its target.type (and, optionally, its
// source/check validation) at init, so adding a new engine requires no edit
// here — the additive-extension promise. When no engines are linked in at all
// (a library or test context that imports only this package), target.type
// validation is deferred to the engine lookup at run time (spi.Lookup), which
// rejects unknown types with its own operational error.
func (c *Config) Validate() error {
	t := c.Target
	if len(targetValidators) > 0 {
		v, known := targetValidators[t.Type]
		if !known {
			return fmt.Errorf("target.type %q unsupported (%s)", t.Type, strings.Join(TargetTypes(), "|"))
		}
		if v != nil {
			if err := v(c); err != nil {
				return err
			}
		}
	}
	for i, ck := range t.Checks {
		if err := validateCheck(t.Type, i, ck); err != nil {
			return err
		}
	}
	if c.Report.Format != "json" {
		return fmt.Errorf("report.format %q unsupported (only \"json\" in v1)", c.Report.Format)
	}
	if c.Attest != nil {
		switch c.Attest.SecretScan {
		case "", "refuse", "warn", "off":
		default:
			return fmt.Errorf("attest.secret_scan %q unsupported (refuse|warn|off)", c.Attest.SecretScan)
		}
	}
	if c.Alerts != nil {
		if c.Alerts.OnFail == "" && c.Alerts.OnSuccess == "" {
			return errors.New("alerts: at least one of on_fail/on_success is required")
		}
		if c.Alerts.Timeout < 0 {
			return errors.New("alerts.timeout must be positive")
		}
		for _, h := range []struct{ key, spec string }{
			{"on_fail", c.Alerts.OnFail},
			{"on_success", c.Alerts.OnSuccess},
		} {
			if h.spec == "" {
				continue
			}
			if err := alert.ValidateSpec(h.spec); err != nil {
				return fmt.Errorf("alerts.%s: %w", h.key, err)
			}
		}
	}
	return nil
}

// SecretEnvNames returns the environment-variable names whose resolved values
// form the report's known secret set (spec 0027 R3): every source.pass_env
// name plus, for the exec engine, every restore.env pass-through name — the
// full set of credentials this config forwards by reference. The caller
// resolves them (report.KnownSecretsFromEnv) and hands the values to the
// report so any that leak through captured output are scrubbed before
// serialization.
func (c *Config) SecretEnvNames() []string {
	names := make([]string, 0, len(c.Target.Source.PassEnv)+len(c.Target.Restore.Env))
	names = append(names, c.Target.Source.PassEnv...)
	names = append(names, c.Target.Restore.Env...)
	// Alert-hook tokens are referenced the same way (spec 0030 R7:
	// token_ref=env:NAME); fold them in so a hook token can never surface in
	// a report even if it leaks into captured output.
	if c.Alerts != nil {
		names = append(names, alert.RefEnvNames(c.Alerts.OnFail)...)
		names = append(names, alert.RefEnvNames(c.Alerts.OnSuccess)...)
	}
	return names
}

// validateCheck applies the per-kind check rules for the target engine. The sql
// kind keeps its verbatim "exactly one expectation" rule; restic kinds each
// require the fields they consume, so a file_count without bounds or a checksum
// without equals fails at load rather than at runtime. The kinds handled inline
// here are the core/cross-engine ones (sql, internal/probe's file/command/http);
// an engine-owned kind (e.g. MongoDB's collection_count/doc_query) is validated
// by the CheckValidator its engine registered, so no case is added here when an
// engine brings its own kinds.
func validateCheck(targetType string, i int, ck Check) error {
	if ck.Name == "" {
		return fmt.Errorf("checks[%d]: name is required", i)
	}
	switch ck.Severity {
	case "", "required", "advisory":
	default:
		return fmt.Errorf("checks[%d] (%s): severity %q unsupported (required|advisory)", i, ck.Name, ck.Severity)
	}
	// keep_literal widens the redaction bound for a byte-equal assertion (spec
	// 0027 R2/R5); without an equals expectation there is no literal to keep,
	// so it can only be a mistake — reject at load rather than silently ignore.
	if ck.KeepLiteral && ck.Equals == nil {
		return fmt.Errorf("checks[%d] (%s): keep_literal requires an equals expectation (spec 0027)", i, ck.Name)
	}

	kind := ck.Kind
	if kind == "" {
		kind = "sql"
	}
	switch kind {
	case "sql":
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
	case "file_exists", "file_count", "checksum":
		if !targetHasCapability(targetType, CapabilityFileProbe) {
			return fmt.Errorf("checks[%d] (%s): kind %q is only valid for target.type %s (targets with a file prober)",
				i, ck.Name, kind, orList(capabilityTypes(CapabilityFileProbe)))
		}
		if ck.Path == "" {
			return fmt.Errorf("checks[%d] (%s): path is required for kind %q", i, ck.Name, kind)
		}
		if kind == "checksum" && ck.Equals == nil {
			return fmt.Errorf("checks[%d] (%s): equals (expected sha256) is required for kind checksum", i, ck.Name)
		}
		if kind == "file_count" && ck.ExpectMin == nil && ck.ExpectMax == nil {
			return fmt.Errorf("checks[%d] (%s): expect_min and/or expect_max is required for kind file_count", i, ck.Name)
		}
	case "command":
		if !targetHasCapability(targetType, CapabilityFileProbe) {
			return fmt.Errorf("checks[%d] (%s): kind %q is only valid for target.type %s (targets with a file prober)",
				i, ck.Name, kind, orList(capabilityTypes(CapabilityFileProbe)))
		}
		if ck.Command == "" {
			return fmt.Errorf("checks[%d] (%s): command is required for kind command", i, ck.Name)
		}
	case "http":
		if !targetHasCapability(targetType, CapabilityHTTPProbe) {
			return fmt.Errorf("checks[%d] (%s): kind %q needs a target with an HTTP prober — only valid for target.type %s",
				i, ck.Name, kind, orList(capabilityTypes(CapabilityHTTPProbe)))
		}
		if ck.URL == "" {
			return fmt.Errorf("checks[%d] (%s): url is required for kind http", i, ck.Name)
		}
	default:
		if v, ok := checkValidators[kind]; ok {
			return v(targetType, i, ck)
		}
		return fmt.Errorf("checks[%d] (%s): kind %q unsupported", i, ck.Name, kind)
	}
	return nil
}
