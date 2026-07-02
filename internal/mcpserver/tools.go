package mcpserver

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"

	"salvage.sh/internal/attest"
	"salvage.sh/internal/config"
	"salvage.sh/internal/engine"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/inspect"
	"salvage.sh/internal/report"
	"salvage.sh/internal/version"
)

// defaultEndpoint is the notary used when neither the config nor stored login
// credentials name one — the same default `salvage verify` ships.
const defaultEndpoint = "https://attest.salvage.sh"

// serverVersion is the version advertised in initialize serverInfo. `version`
// is not a distinct tool (spec 0032 Non-goals): this is where it surfaces.
func serverVersion() string { return version.String() }

// Seams for the underlying command implementations. Production wiring is the
// exact code path the CLI subcommand uses (spec 0032 Non-goals: the server is
// an adapter, not a re-implementation); tests substitute these to exercise the
// protocol without Docker or a network.
var (
	loadConfig       = config.Load
	runRestore       = engine.Run
	lastGoodSearch   = engine.LastGood
	fleetSurvey      = engine.Fleet
	scaffoldConfig   = engine.Scaffold
	inspectDir       = inspect.Inspect
	dockerPreflight  = ephemeral.Preflight
	fetchAttestation = attest.Fetch
	submitReport     = attest.Submit
	signReport       = report.Sign
	loadCredentials  = attest.LoadCredentials
)

// classification is the machine-readable read-only/mutating class every tool
// carries (spec 0032 R4).
type classification string

const (
	// classReadOnly: reads config, a backup repository, or the hosted ledger;
	// mutates nothing.
	classReadOnly classification = "read-only"
	// classRestoreExecuting: mutates no Salvage state (no ledger write, no file
	// emitted) but executes a real restore — a container, and for
	// target.type exec the customer's own restore command — in an isolated
	// throwaway environment (spec 0003). Not side-effect-free at the OS level;
	// hosts may want to gate it.
	classRestoreExecuting classification = "restore-executing"
	// classMutating: writes durable state — an append-only ledger entry
	// (attest) or emitted configuration an agent may persist (scaffold).
	classMutating classification = "mutating"
)

// toolError is the structured body of a failed tool call (spec 0032 R3, R7):
// an operational problem, never a verdict. Type is machine-branchable.
type toolError struct {
	// Type: "invalid_arguments" | "operational_error" | "not_authenticated" |
	// "not_supported".
	Type    string `json:"type"`
	Message string `json:"message"`
	// Remedy names the human action that fixes the condition, when one exists
	// (e.g. "run `salvage login`").
	Remedy string `json:"remedy,omitempty"`
}

// toolOutcome is what a tool handler produces: exactly one of payload or err,
// plus the redactor scoped to the secrets this call could have touched.
type toolOutcome struct {
	payload any
	err     *toolError
	red     *redactor
}

func errOutcome(red *redactor, typ, msg, remedy string) toolOutcome {
	return toolOutcome{err: &toolError{Type: typ, Message: msg, Remedy: remedy}, red: red}
}

// argSpec declares one tool argument; the advertised JSON Schema and the
// runtime validation are generated from the same declaration so they cannot
// drift.
type argSpec struct {
	name     string
	typ      string // "string" | "integer"
	required bool
	desc     string
}

type toolDef struct {
	name           string
	title          string
	description    string
	classification classification
	openWorld      bool // touches systems beyond the local host (network)
	args           []argSpec
	handler        func(ctx context.Context, args map[string]any) toolOutcome
}

// inputSchema renders the argument declarations as the JSON Schema advertised
// to the host (spec 0032 R3). additionalProperties is false: an unknown
// argument is rejected, mirroring the config loader's strictness.
func (t *toolDef) inputSchema() map[string]any {
	props := map[string]any{}
	var required []string
	for _, a := range t.args {
		props[a.name] = map[string]any{"type": a.typ, "description": a.desc}
		if a.required {
			required = append(required, a.name)
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

// validateArgs enforces the advertised schema at runtime: unknown keys, missing
// required keys, and wrong types are structured invalid_arguments tool errors —
// never a crash or a usage dump (spec 0032 R3).
func validateArgs(args map[string]any, specs []argSpec) *toolError {
	known := map[string]argSpec{}
	for _, s := range specs {
		known[s.name] = s
	}
	for k, v := range args {
		s, ok := known[k]
		if !ok {
			return &toolError{Type: "invalid_arguments", Message: fmt.Sprintf("unknown argument %q", k)}
		}
		switch s.typ {
		case "string":
			str, ok := v.(string)
			if !ok || str == "" {
				return &toolError{Type: "invalid_arguments", Message: fmt.Sprintf("argument %q must be a non-empty string", k)}
			}
		case "integer":
			f, ok := v.(float64)
			if !ok || f != math.Trunc(f) || f < 0 {
				return &toolError{Type: "invalid_arguments", Message: fmt.Sprintf("argument %q must be a non-negative integer", k)}
			}
		}
	}
	for _, s := range specs {
		if s.required {
			if _, ok := args[s.name]; !ok {
				return &toolError{Type: "invalid_arguments", Message: fmt.Sprintf("missing required argument %q", s.name)}
			}
		}
	}
	return nil
}

// loadToolConfig loads and validates the config path argument via the same
// strict loader the CLI uses; a load failure is an operational error (the CLI's
// exit 2), returned with a call-scoped redactor already built.
func loadToolConfig(args map[string]any) (*config.Config, *redactor, *toolError) {
	path, _ := args["config"].(string)
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, newRedactor(nil), &toolError{Type: "operational_error", Message: "config error: " + err.Error()}
	}
	return cfg, newRedactor(cfg), nil
}

// checkVerdict is the structured result of salvage_check. Spec 0026 defined no
// machine payload for `check` (it has no -json flag), so this is a minimal
// versioned object carrying exactly what the CLI's one-line summary states.
type checkVerdict struct {
	SchemaVersion int    `json:"schema_version"`
	Target        string `json:"target"`
	TargetType    string `json:"target_type"`
	DockerNeeded  bool   `json:"docker_needed"`
	ChecksDefined int    `json:"checks_defined"`
	OK            bool   `json:"ok"`
}

// toolset builds the advertised tools (spec 0032 R2). Descriptions are honest
// about side effects — a host and its human decide gating from them plus the
// classification (spec 0032 "Sandboxing").
func toolset() []toolDef {
	configArg := func(what string) argSpec {
		return argSpec{name: "config", typ: "string", required: true,
			desc: "Path to the salvage YAML config " + what + ". Credentials are never accepted here; secrets stay by-reference in the environment."}
	}
	tools := []toolDef{
		{
			name:  "salvage_run",
			title: "Run a restore-test",
			description: "Executes a REAL restore-test: restores the configured backup into an isolated, " +
				"throwaway, network-isolated environment (a Docker container, or for target.type exec the " +
				"customer's own restore command) and runs the configured checks against the restored data. " +
				"Mutates no Salvage state, but it does run a container/command. Returns the versioned report " +
				"JSON (schema_version, verdict, per-check results). A bad backup is a SUCCESSFUL call whose " +
				"payload says verdict \"fail\"; a tool error means the test could not run at all. " +
				"Unlike the CLI, no report file is written and no local signature is produced — the report is " +
				"the tool result.",
			classification: classRestoreExecuting,
			openWorld:      true,
			args:           []argSpec{configArg("describing the backup to restore-test")},
			handler:        handleRun,
		},
		{
			name:  "salvage_check",
			title: "Validate config + preflight",
			description: "Validates the config and preflights the environment (Docker reachability for " +
				"container engines; target.type exec needs no Docker). No restore is performed, but the " +
				"preflight may talk to the local Docker daemon. Returns a small versioned status object.",
			classification: classRestoreExecuting,
			openWorld:      false,
			args:           []argSpec{configArg("to validate")},
			handler:        handleCheck,
		},
		{
			name:  "salvage_inspect",
			title: "Offline PGDATA inspection",
			description: "Offline pre-flight of a restored PostgreSQL data directory: reports the PG major " +
				"version, required shared_preload_libraries extensions, and database count — parsed from files " +
				"on disk, no server started, nothing modified.",
			classification: classReadOnly,
			openWorld:      false,
			args: []argSpec{{name: "pgdata", typ: "string", required: true,
				desc: "Path to the restored PostgreSQL data directory (PGDATA) to inspect."}},
			handler: handleInspect,
		},
		{
			name:  "salvage_last_good",
			title: "Find the last restorable backup",
			description: "Walks a backup chain (pgBackRest, restic, or borg) newest-first, restore-testing each backup until one " +
				"passes, and reports the freshest restorable recovery point. Each candidate is a full " +
				"isolated throwaway restore (like salvage_run) — on long restic/borg histories, bound the " +
				"walk with the max argument. Nothing durable is written. recovery_point is null when " +
				"no backup in the chain restores — that is a result, not a tool error.",
			classification: classRestoreExecuting,
			openWorld:      true,
			args: []argSpec{
				configArg("with the chain-backed source (pgbackrest, restic, or borg) and restore image"),
				{name: "max", typ: "integer", required: false,
					desc: "Maximum number of backups to try (0 or omitted = walk until the first that restores)."},
			},
			handler: handleLastGood,
		},
		{
			name:  "salvage_fleet",
			title: "Survey a backup repo",
			description: "Enumerates every unit in a repository (pgBackRest stanzas; a restic/borg repo is one unit) — a cheap metadata-only survey " +
				"(backup counts, newest backup per unit). No restore is performed and, unlike the CLI's -o " +
				"flag, no per-unit config files are written via this tool.",
			classification: classReadOnly,
			openWorld:      true,
			args:           []argSpec{configArg("providing the repository (source) to survey")},
			handler:        handleFleet,
		},
		{
			name:  "salvage_verify",
			title: "Verify an attestation",
			description: "Fetches an attestation from the notary and verifies it OFFLINE against the pinned " +
				"public key: chain hash, notary signature, and report hash. Returns the versioned verify " +
				"verdict object (valid true/false plus per-step checks). An invalid (tampered) attestation is a " +
				"SUCCESSFUL call with valid=false. The notary endpoint resolves from stored login credentials " +
				"or the default; no credential is accepted or returned.",
			classification: classReadOnly,
			openWorld:      true,
			args: []argSpec{{name: "id", typ: "string", required: true,
				desc: "Attestation id, or a full attestation URL."}},
			handler: handleVerify,
		},
		{
			name:  "salvage_attest",
			title: "Run + attest to the ledger",
			description: "MUTATING: runs the full restore-test (like salvage_run) and then submits the signed " +
				"report to the hosted attestation notary, writing an entry into the append-only ledger. " +
				"Authenticates strictly by reference — the API key comes from the environment or " +
				"~/.salvage/credentials (a human runs `salvage login`); it is never a tool argument and never " +
				"appears in output. Returns the attestation receipt JSON.",
			classification: classMutating,
			openWorld:      true,
			args:           []argSpec{configArg("to restore-test and attest")},
			handler:        handleAttest,
		},
		{
			name:  "salvage_scaffold",
			title: "Generate a starter config",
			description: "MUTATING (emits config): restores the backup into a throwaway environment, " +
				"introspects the restored data, and returns a generated starter salvage.yaml with " +
				"auto-generated checks as a string. Nothing is written to disk by this tool; persisting the " +
				"emitted config is the caller's explicit decision.",
			classification: classMutating,
			openWorld:      true,
			args:           []argSpec{configArg("providing the source and restore to introspect")},
			handler:        handleScaffold,
		},
	}
	// Wrap every handler with argument validation derived from the same specs
	// the advertised schema is generated from.
	for i := range tools {
		def := tools[i]
		tools[i].handler = func(ctx context.Context, args map[string]any) toolOutcome {
			if terr := validateArgs(args, def.args); terr != nil {
				return toolOutcome{err: terr, red: newRedactor(nil)}
			}
			return def.handler(ctx, args)
		}
	}
	return tools
}

// handleRun wraps `salvage run` (spec 0032 R2, R7, R8): the identical
// engine.Run code path and isolation the CLI uses. Verdict fail → successful
// call; operational error → tool error.
func handleRun(ctx context.Context, args map[string]any) toolOutcome {
	cfg, red, terr := loadToolConfig(args)
	if terr != nil {
		return toolOutcome{err: terr, red: red}
	}
	rep, runErr := runRestore(ctx, cfg)
	if runErr != nil {
		return errOutcome(red, "operational_error", "operational error: "+runErr.Error(), "")
	}
	return toolOutcome{payload: rep, red: red}
}

// handleCheck wraps `salvage check`. The exec engine (spec 0020) is
// Docker-free; container engines preflight the Docker daemon. This mirrors
// cmd/salvage cmdCheck (which cannot be imported from package main); the logic
// is a handful of lines and the CLI's output is unstructured text, so a small
// versioned object is built here instead.
func handleCheck(ctx context.Context, args map[string]any) toolOutcome {
	cfg, red, terr := loadToolConfig(args)
	if terr != nil {
		return toolOutcome{err: terr, red: red}
	}
	v := checkVerdict{
		SchemaVersion: report.SchemaVersion,
		Target:        cfg.Target.Name,
		TargetType:    cfg.Target.Type,
		DockerNeeded:  cfg.Target.Type != "exec",
		ChecksDefined: len(cfg.Target.Checks),
		OK:            true,
	}
	if v.DockerNeeded {
		if err := dockerPreflight(ctx); err != nil {
			// The CLI exits 2 here: Docker being unavailable is operational.
			return errOutcome(red, "operational_error", err.Error(), "")
		}
	}
	return toolOutcome{payload: v, red: red}
}

func handleInspect(_ context.Context, args map[string]any) toolOutcome {
	red := newRedactor(nil)
	pgdata, _ := args["pgdata"].(string)
	res, err := inspectDir(pgdata)
	if err != nil {
		return errOutcome(red, "operational_error", "inspect error: "+err.Error(), "")
	}
	return toolOutcome{payload: res, red: red}
}

func handleLastGood(ctx context.Context, args map[string]any) toolOutcome {
	cfg, red, terr := loadToolConfig(args)
	if terr != nil {
		return toolOutcome{err: terr, red: red}
	}
	maxTry := 0
	if f, ok := args["max"].(float64); ok {
		maxTry = int(f)
	}
	lg, err := lastGoodSearch(ctx, cfg, maxTry)
	if err != nil {
		return errOutcome(red, "operational_error", "last-good error: "+err.Error(), "")
	}
	// No restorable backup (CLI exit 1) is a verdict, not a tool error: the
	// payload's recovery_point is null.
	return toolOutcome{payload: lg, red: red}
}

func handleFleet(ctx context.Context, args map[string]any) toolOutcome {
	cfg, red, terr := loadToolConfig(args)
	if terr != nil {
		return toolOutcome{err: terr, red: red}
	}
	// outDir is always empty via MCP: the tool is classified read-only and must
	// not write per-stanza skeleton configs (spec 0032 R4).
	fl, err := fleetSurvey(ctx, cfg, "")
	if err != nil {
		return errOutcome(red, "operational_error", "fleet error: "+err.Error(), "")
	}
	return toolOutcome{payload: fl, red: red}
}

// handleVerify wraps `salvage verify` (spec 0032 R5): the endpoint resolves by
// reference (stored credentials → default), and an invalid attestation is a
// successful call with valid=false — the CLI's exit-1 verdict semantics.
func handleVerify(ctx context.Context, args map[string]any) toolOutcome {
	red := newRedactor(nil)
	id, _ := args["id"].(string)

	endpoint := defaultEndpoint
	if creds, err := loadCredentials(); err == nil && creds != nil && creds.Endpoint != "" {
		endpoint = creds.Endpoint
	}
	rec, err := fetchAttestation(ctx, endpoint, id)
	if err != nil {
		return errOutcome(red, "operational_error", "verify error: "+err.Error(), "")
	}
	checks, ok := attest.Verify(rec)
	// report.VerifyVerdict is the same type the CLI's `verify -json` serializes
	// (spec 0026 R4), so the two surfaces cannot drift.
	return toolOutcome{payload: report.NewVerifyVerdict(rec, checks, ok), red: red}
}

// handleAttest wraps `salvage attest` (run-then-submit; the CLI's -report/-sig
// replay flags are not exposed). Auth is by reference only (spec 0032 R5):
// env var named by attest.api_key_env (default SALVAGE_ATTEST_KEY) →
// ~/.salvage/credentials. The key is registered with the redactor so it can
// never appear in output even via an upstream error string.
func handleAttest(ctx context.Context, args map[string]any) toolOutcome {
	cfg, red, terr := loadToolConfig(args)
	if terr != nil {
		return toolOutcome{err: terr, red: red}
	}

	rep, runErr := runRestore(ctx, cfg)
	reportBytes, _ := rep.WriteJSON("") // bytes only; nothing written to disk
	var sigB64, pubB64 string
	if cfg.Report.KeyPath != "" {
		if s, serr := signReport(cfg.Report.KeyPath, reportBytes); serr == nil {
			sigB64, pubB64 = s.Signature, s.PublicKey
		}
		// A local-signing failure is a warning in the CLI, never fatal: the
		// tenant signature is optional (the notary signs authoritatively).
	}
	if runErr != nil {
		return errOutcome(red, "operational_error", "operational error: "+runErr.Error(), "")
	}

	// Resolve endpoint + API key by reference — config → stored login
	// credentials (the CLI's flag override does not exist on the MCP path).
	creds, _ := loadCredentials()
	endpoint := ""
	if cfg.Attest != nil {
		endpoint = cfg.Attest.Endpoint
	}
	if endpoint == "" && creds != nil {
		endpoint = creds.Endpoint
	}
	if endpoint == "" {
		return errOutcome(red, "operational_error",
			"no notary endpoint configured",
			"set attest.endpoint in the config, or have a human run `salvage login`")
	}
	envName := ""
	if cfg.Attest != nil {
		envName = cfg.Attest.APIKeyEnv
	}
	if envName == "" {
		envName = "SALVAGE_ATTEST_KEY"
	}
	apiKey := os.Getenv(envName)
	if apiKey == "" && creds != nil {
		apiKey = creds.APIKey
	}
	if apiKey == "" {
		return errOutcome(red, "not_authenticated",
			"no API key available (checked the "+envName+" environment variable and ~/.salvage/credentials)",
			"have a human run `salvage login` on this machine, or set "+envName+" in the environment")
	}
	red.addSecret(apiKey)

	resp, err := submitReport(ctx, endpoint, apiKey, reportBytes, sigB64, pubB64)
	if err != nil {
		return errOutcome(red, "operational_error", "attest error: "+err.Error(), "")
	}
	return toolOutcome{payload: resp, red: red}
}

// handleScaffold wraps `salvage scaffold`, returning the rendered YAML in the
// result instead of writing it anywhere — emitting config is the mutation, and
// persisting it stays an explicit caller decision (spec 0032 R4).
func handleScaffold(ctx context.Context, args map[string]any) toolOutcome {
	cfg, red, terr := loadToolConfig(args)
	if terr != nil {
		return toolOutcome{err: terr, red: red}
	}
	rendered, err := scaffoldConfig(ctx, cfg)
	if err != nil {
		return errOutcome(red, "operational_error", "scaffold error: "+err.Error(), "")
	}
	return toolOutcome{payload: map[string]any{
		"target":      cfg.Target.Name,
		"config_yaml": string(rendered),
	}, red: red}
}
