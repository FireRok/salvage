// Command salvage proves a backup actually restores and works.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/alert"
	"salvage.sh/internal/attest"
	"salvage.sh/internal/config"
	"salvage.sh/internal/engine"
	"salvage.sh/internal/ephemeral"
	"salvage.sh/internal/inspect"
	"salvage.sh/internal/report"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "check":
		cmdCheck(os.Args[2:])
	case "inspect":
		cmdInspect(os.Args[2:])
	case "scaffold":
		cmdScaffold(os.Args[2:])
	case "last-good":
		cmdLastGood(os.Args[2:])
	case "fleet":
		cmdFleet(os.Args[2:])
	case "schedule":
		cmdSchedule(os.Args[2:])
	case "login":
		cmdLogin(os.Args[2:])
	case "logout":
		cmdLogout(os.Args[2:])
	case "attest":
		cmdAttest(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "version", "-v", "--version":
		cmdVersion(os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`salvage — prove your backups actually restore.

Usage:
  salvage run    [-json] -config salvage.yaml   restore a backup into a throwaway db and assert it works (-json: full report to stdout)
  salvage check  -config salvage.yaml   validate config and preflight docker (no restore)
  salvage inspect [-json] <pgdata-dir>  offline pre-flight: report PG version + required extensions (no start)
  salvage scaffold [-cap N] -config salvage.yaml restore + introspect (postgres/mysql/restic/borg/exec), then emit a starter config with auto-generated checks
  salvage last-good -config salvage.yaml  walk the backup chain newest-first (pgbackrest/restic/borg); report the freshest restorable backup — each candidate is a full restore, so use -max to bound long histories
  salvage fleet    -config salvage.yaml   survey a repo's units (pgbackrest stanzas; a restic/borg repo is one unit), metadata only; optionally emit a config per unit
  salvage schedule -config salvage.yaml   print a systemd timer + cron line to run 'salvage attest' on a cadence
  salvage login    [-endpoint URL]        sign in via your browser and store an API key locally (device flow)
  salvage logout                          remove the stored API key
  salvage attest   -config salvage.yaml   run the test, then submit the signed report to a hosted attestation notary
  salvage verify   [-json] <id|url>        fetch an attestation and verify it offline against Firerok's public key
  salvage mcp                              serve Salvage as an MCP server over stdio (agent tools for run/check/inspect/last-good/fleet/verify/attest/scaffold)
  salvage version [-check]                 print the version; -check also asks the release API whether a newer release exists (never auto-updates)
  salvage help

Diagnostics:
  run, check, scaffold, last-good, fleet, and attest accept -verbose and -quiet.
  Both act on stderr diagnostics only: -quiet suppresses everything but errors,
  -verbose adds detail (on run/attest, the raw secret-scrubbed command output).
  Neither changes stdout output, report JSON, or exit codes.

Exit codes:
  0  pass    restore succeeded and every check passed
  1  fail    restore failed or a check failed (a result, not a crash)
  2  error   operational problem (bad config, docker unavailable)
`)
}

func cmdScaffold(args []string) {
	fs := flag.NewFlagSet("scaffold", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "config providing the source + restore to introspect")
	outPath := fs.String("o", "", "write the generated config here (default: stdout)")
	capN := fs.Int("cap", engine.DefaultScaffoldCap, "max tables/directories per family to generate checks for (top-N by observed size)")
	v := addVerbosityFlags(fs)
	_ = fs.Parse(args)
	v.apply()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config error: " + err.Error())
		os.Exit(2)
	}
	logger.Debug("config loaded", "path", *cfgPath, "target", cfg.Target.Name, "type", cfg.Target.Type, "cap", *capN)
	rendered, err := engine.ScaffoldWithCap(context.Background(), cfg, *capN)
	if err != nil {
		logger.Error("scaffold error: " + err.Error())
		os.Exit(2)
	}
	if *outPath == "" {
		os.Stdout.Write(rendered)
		return
	}
	if err := os.WriteFile(*outPath, rendered, 0o644); err != nil {
		logger.Error("write: " + err.Error())
		os.Exit(2)
	}
	logger.Info("wrote " + *outPath)
}

func cmdLastGood(args []string) {
	fs := flag.NewFlagSet("last-good", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "config with the chain-backed source (pgbackrest, restic, or borg) + restore")
	maxTry := fs.Int("max", 0, "max backups to try (0 = until the first that restores); every try is a full restore, so cap this on large restic/borg histories")
	asJSON := fs.Bool("json", false, "emit JSON")
	v := addVerbosityFlags(fs)
	_ = fs.Parse(args)
	v.apply()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config error: " + err.Error())
		os.Exit(2)
	}
	logger.Debug("config loaded", "path", *cfgPath, "target", cfg.Target.Name, "type", cfg.Target.Type, "max", *maxTry)
	lg, err := engine.LastGood(context.Background(), cfg, *maxTry)
	if err != nil {
		logger.Error("last-good error: " + err.Error())
		os.Exit(2)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(lg, "", "  ")
		fmt.Println(string(b))
	} else {
		printLastGood(lg)
	}
	if lg.RecoveryPoint == nil {
		os.Exit(1)
	}
}

func printLastGood(lg *report.LastGood) {
	fmt.Printf("\nsalvage last-good: stanza %q\n", lg.Stanza)
	for _, v := range lg.Tested {
		mark := "FAIL"
		if v.Verdict == "pass" {
			mark = "PASS"
		}
		fmt.Printf("  %-4s  %-34s %s\n", mark, v.Label, v.Timestamp.Format("2006-01-02 15:04"))
		if v.Reason != "" {
			fmt.Printf("        reason: %s\n", v.Reason)
		}
	}
	if lg.RecoveryPoint != nil {
		fmt.Printf("\n  recovery point: %s  (%s old)\n",
			lg.RecoveryPoint.Label, time.Since(lg.RecoveryPoint.Timestamp).Round(time.Minute))
	} else {
		fmt.Println("\n  recovery point: NONE — no backup in the chain restored")
	}
}

func cmdFleet(args []string) {
	fs := flag.NewFlagSet("fleet", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "config providing the repo (source) + restore image")
	outDir := fs.String("o", "", "write a per-unit skeleton config into this directory")
	asJSON := fs.Bool("json", false, "emit JSON")
	v := addVerbosityFlags(fs)
	_ = fs.Parse(args)
	v.apply()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config error: " + err.Error())
		os.Exit(2)
	}
	logger.Debug("config loaded", "path", *cfgPath, "target", cfg.Target.Name, "type", cfg.Target.Type)
	fl, err := engine.Fleet(context.Background(), cfg, *outDir)
	if err != nil {
		logger.Error("fleet error: " + err.Error())
		os.Exit(2)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(fl, "", "  ")
		fmt.Println(string(b))
	} else {
		printFleet(fl)
	}
	if fleetDegraded(fl) {
		os.Exit(1)
	}
}

// fleetDegraded reports whether the fleet survey warrants a failing exit
// (backlog S2; spec 0029 R5): any surveyed unit is degraded or has zero
// backups, or the repo has no units at all. Exit 0 only when the survey is
// non-empty and every unit is healthy with at least one backup — a result, not
// a crash, so it exits 1 (like last-good's RecoveryPoint == nil), and only
// after the report (JSON or human) has been emitted.
func fleetDegraded(fl *report.Fleet) bool {
	if len(fl.Stanzas) == 0 {
		return true
	}
	for _, s := range fl.Stanzas {
		if s.Status != "ok" || s.BackupCount == 0 {
			return true
		}
	}
	return false
}

func printFleet(fl *report.Fleet) {
	fmt.Printf("\nsalvage fleet: %d stanza(s)\n", len(fl.Stanzas))
	for _, s := range fl.Stanzas {
		newest := "(no backups)"
		if s.NewestBackup != nil {
			newest = s.NewestBackup.Format("2006-01-02 15:04") + "  " + s.NewestLabel
		}
		fmt.Printf("  %-24s %-8s backups=%-3d newest: %s\n", s.Name, s.Status, s.BackupCount, newest)
		if s.ConfigWritten != "" {
			fmt.Printf("        wrote %s\n", s.ConfigWritten)
		}
	}
}

func cmdSchedule(args []string) {
	fs := flag.NewFlagSet("schedule", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "config to attest on a schedule")
	every := fs.String("every", "1d", "interval: 1h, 12h, 1d, 7d, 1w")
	_ = fs.Parse(args)

	n, unit, ok := parseEvery(*every)
	if !ok {
		fmt.Fprintln(os.Stderr, "invalid -every (use forms like 1h, 12h, 1d, 7d, 1w)")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*cfgPath)
	if err != nil {
		abs = *cfgPath
	}
	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = "salvage"
	}
	cron := cronFor(n, unit)
	cronLine := cron + " " + bin + " attest -config " + abs
	if cron == "" {
		cronLine = "# (no simple cron for " + *every + " — use the systemd timer above)"
	}

	fmt.Printf(`# salvage schedule — run 'salvage attest' every %[1]s.
# The unattended run needs an API key: run 'salvage login' as this user, or set
# SALVAGE_ATTEST_KEY (a portal-generated key) in the environment/unit below.

### systemd (recommended) — /etc/systemd/system/salvage-attest.service
[Unit]
Description=Salvage restore-test attestation
[Service]
Type=oneshot
# Environment=SALVAGE_ATTEST_KEY=sk_...     # or rely on ~/.salvage/credentials
ExecStart=%[2]s attest -config %[3]s

### /etc/systemd/system/salvage-attest.timer
[Unit]
Description=Run salvage attest every %[1]s
[Timer]
OnUnitActiveSec=%[1]s
OnBootSec=5min
Persistent=true
[Install]
WantedBy=timers.target

# enable:  sudo systemctl daemon-reload && sudo systemctl enable --now salvage-attest.timer

### cron equivalent (crontab -e)
%[4]s
`, *every, bin, abs, cronLine)
}

// parseEvery parses an interval like "12h", "7d", "1w" into (value, unit).
func parseEvery(s string) (int, string, bool) {
	if len(s) < 2 {
		return 0, "", false
	}
	unit := s[len(s)-1:]
	if unit != "h" && unit != "d" && unit != "w" {
		return 0, "", false
	}
	v, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || v <= 0 {
		return 0, "", false
	}
	return v, unit, true
}

// cronFor maps the clean interval cases to a cron expression, or "" when cron
// can't express it (systemd's OnUnitActiveSec can).
func cronFor(n int, unit string) string {
	switch unit {
	case "h":
		if n == 1 {
			return "0 * * * *"
		}
		if 24%n == 0 {
			return fmt.Sprintf("0 */%d * * *", n)
		}
	case "d":
		if n == 1 {
			return "0 3 * * *"
		}
		if n == 7 {
			return "0 3 * * 0"
		}
	case "w":
		if n == 1 {
			return "0 3 * * 0"
		}
	}
	return ""
}

func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	endpoint := fs.String("endpoint", "https://attest.salvage.sh", "notary base URL")
	_ = fs.Parse(args)
	ctx := context.Background()

	dc, err := attest.DeviceStart(ctx, *endpoint)
	if err != nil {
		fmt.Fprintln(os.Stderr, "login error:", err)
		os.Exit(2)
	}
	fmt.Printf("\nTo sign in, open:\n  %s\n\nand confirm this code:  %s\n\n", dc.VerificationURIComplete, dc.UserCode)
	openBrowser(dc.VerificationURIComplete)

	interval := dc.Interval
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	fmt.Print("Waiting for approval")
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval) * time.Second)
		tok, status, err := attest.DevicePoll(ctx, *endpoint, dc.DeviceCode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "\npoll error:", err)
			os.Exit(2)
		}
		if tok != nil {
			if err := attest.SaveCredentials(&attest.Credentials{Endpoint: *endpoint, APIKey: tok.APIKey, OrgID: tok.OrgID, OrgName: tok.OrgName}); err != nil {
				fmt.Fprintln(os.Stderr, "\nsave credentials:", err)
				os.Exit(2)
			}
			fmt.Println("\n\n✓ Signed in — stored an API key in ~/.salvage/credentials.")
			switch tok.OrgName {
			case "", "personal":
				fmt.Println("  Attestations from this machine will land in your personal ledger.")
			default:
				fmt.Printf("  Attestations from this machine will land in the %q org's ledger.\n", tok.OrgName)
			}
			fmt.Println("  salvage attest will use the key automatically.")
			return
		}
		switch status {
		case "authorization_pending", "":
			fmt.Print(".")
		case "slow_down":
			interval += 5
		default:
			fmt.Fprintf(os.Stderr, "\nlogin failed: %s\n", status)
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stderr, "\nlogin timed out")
	os.Exit(1)
}

func cmdLogout(args []string) {
	if err := attest.ClearCredentials(); err != nil {
		fmt.Fprintln(os.Stderr, "logout:", err)
		os.Exit(2)
	}
	fmt.Println("Signed out — removed ~/.salvage/credentials.")
}

func openBrowser(url string) {
	if os.Getenv("SALVAGE_NO_BROWSER") == "1" {
		return // headless/SSH login: user opens the printed URL themselves
	}
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}

func cmdAttest(args []string) {
	fs := flag.NewFlagSet("attest", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "config to run + submit")
	reportPath := fs.String("report", "", "submit an existing report file instead of running the test")
	sigPath := fs.String("sig", "", "signature sidecar for -report (optional)")
	endpoint := fs.String("endpoint", "", "notary base URL (overrides config attest.endpoint)")
	keyEnv := fs.String("key-env", "", "env var holding the API key (overrides config attest.api_key_env)")
	v := addVerbosityFlags(fs)
	_ = fs.Parse(args)
	v.apply()

	cfg, cfgErr := config.Load(*cfgPath)

	var reportBytes []byte
	var sigB64, pubB64 string

	if *reportPath != "" {
		b, err := os.ReadFile(*reportPath)
		if err != nil {
			logger.Error("read report: " + err.Error())
			os.Exit(2)
		}
		reportBytes = b
		if *sigPath != "" {
			sb, err := os.ReadFile(*sigPath)
			if err != nil {
				logger.Error("read sig: " + err.Error())
				os.Exit(2)
			}
			var s report.Signature
			if err := json.Unmarshal(sb, &s); err != nil {
				logger.Error("parse sig: " + err.Error())
				os.Exit(2)
			}
			sigB64, pubB64 = s.Signature, s.PublicKey
		}
	} else {
		if cfgErr != nil {
			logger.Error("config error: " + cfgErr.Error())
			os.Exit(2)
		}
		logger.Debug("config loaded", "path", *cfgPath, "target", cfg.Target.Name, "type", cfg.Target.Type)
		rep, runErr := engine.Run(context.Background(), cfg)
		// Spec 0027 R4: under -verbose the raw (still secret-scrubbed) command
		// output goes to stderr only; the report bytes below never carry it.
		// Redact is idempotent, so WriteJSON's internal call becomes a no-op.
		if raw := rep.Redact(); v.showRaw(false) {
			raw.Fprint(os.Stderr)
		}
		b, _ := rep.WriteJSON("") // bytes only; do not write to disk here
		reportBytes = b
		// A tenant signature is optional; produce one when a signing key is configured.
		if cfg.Report.KeyPath != "" {
			if s, serr := report.Sign(cfg.Report.KeyPath, b); serr == nil {
				sigB64, pubB64 = s.Signature, s.PublicKey
			} else {
				logger.Warn("warning: could not sign report locally: " + serr.Error())
			}
		}
		printSummary(rep)
		// Spec 0030 R1: attest runs the same test, so the run hooks fire here
		// too — after the report bytes are finalized (no file is written on
		// this path, so $SALVAGE_REPORT is unset), before exit/submission.
		fireAlertHook(cfg, rep, reportBytes, "", runErr != nil)
		if runErr != nil {
			logger.Error("operational error: " + runErr.Error())
			os.Exit(2)
		}
	}

	// Spec 0027 R7: pattern-gate the bytes about to leave the machine. Default
	// is refuse — also when no config is available (e.g. -report submissions).
	scanMode := "refuse"
	if cfgErr == nil && cfg.Attest != nil && cfg.Attest.SecretScan != "" {
		scanMode = cfg.Attest.SecretScan
	}
	if scanMode != "off" {
		if matches := report.ScanForCredentials(reportBytes); len(matches) > 0 {
			// In refuse mode the matches are the reason for the exit-2 error, so
			// they print at error level (visible under -quiet); in warn mode they
			// are ordinary warnings.
			lvl := slog.LevelError
			if scanMode == "warn" {
				lvl = slog.LevelWarn
			}
			for _, m := range matches {
				logger.Log(context.Background(), lvl,
					fmt.Sprintf("secret scan: %s matched %d time(s) in the report", m.Pattern, m.Count))
			}
			if scanMode != "warn" {
				logger.Error("refusing to submit (attest.secret_scan: refuse; set to warn or off to override)")
				os.Exit(2)
			}
		}
	}

	// Resolve endpoint + API key. Precedence: flags → config → stored login
	// credentials (from `salvage login`).
	creds, _ := attest.LoadCredentials()
	ep := *endpoint
	if ep == "" && cfgErr == nil && cfg.Attest != nil {
		ep = cfg.Attest.Endpoint
	}
	if ep == "" && creds != nil {
		ep = creds.Endpoint
	}
	if ep == "" {
		logger.Error("no notary endpoint (set -endpoint, attest.endpoint, or run salvage login)")
		os.Exit(2)
	}
	envName := *keyEnv
	if envName == "" && cfgErr == nil && cfg.Attest != nil {
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
		logger.Error(fmt.Sprintf("no API key (set %s, or run salvage login)", envName))
		os.Exit(2)
	}

	logger.Debug("submitting report", "endpoint", ep)
	resp, err := attest.Submit(context.Background(), ep, apiKey, reportBytes, sigB64, pubB64)
	if err != nil {
		logger.Error("attest error: " + err.Error())
		os.Exit(2)
	}
	shareURL := resp.VerifyURL
	if shareURL == "" {
		shareURL = resp.URL
	}
	fmt.Printf("\nattested: %s\n", shareURL)
	fmt.Printf("  verdict %s   seq %d\n", strings.ToUpper(resp.Verdict), resp.Seq)
	fmt.Printf("  %s\n", resp.Notice)
	fmt.Printf("  verify with: salvage verify %s\n", resp.ID)
}

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	endpoint := fs.String("endpoint", "https://attest.salvage.sh", "notary base URL (for bare-id lookups)")
	asJSON := fs.Bool("json", false, "emit a machine verdict object as JSON (exit codes unchanged)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: salvage verify [-json] <attestation-id|url>")
		os.Exit(2)
	}
	rec, err := attest.Fetch(context.Background(), *endpoint, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify error:", err)
		os.Exit(2)
	}
	checks, ok := attest.Verify(rec)
	if *asJSON {
		b, err := json.MarshalIndent(report.NewVerifyVerdict(rec, checks, ok), "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "encode json:", err)
			os.Exit(2)
		}
		fmt.Println(string(b))
		if !ok {
			os.Exit(1) // invalid attestation still exits 1 (spec 0026 R5)
		}
		return
	}
	fmt.Printf("\nsalvage verify: %s\n", rec.ID)
	fmt.Printf("  target %q  verdict %s  seq %d  key %s\n",
		rec.Target, strings.ToUpper(rec.Verdict), rec.Seq, rec.KeyID)
	for _, c := range checks {
		mark := "FAIL"
		if c.OK {
			mark = "ok"
		}
		fmt.Printf("  %-4s %-18s %s\n", mark, c.Name, c.Detail)
	}
	if rec.Notice != "" {
		fmt.Printf("\n  %s\n", rec.Notice)
	}
	if !ok {
		fmt.Println("\n  ATTESTATION INVALID")
		os.Exit(1)
	}
	fmt.Println("\n  attestation is genuine")
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "path to config file")
	asJSON := fs.Bool("json", false, "write the full report JSON to stdout instead of the human summary (exit codes unchanged)")
	showOutput := fs.Bool("show-output", false, "print raw (still secret-scrubbed) restore output to stderr; never serialized into the report (also implied by -verbose)")
	v := addVerbosityFlags(fs)
	_ = fs.Parse(args)
	v.apply()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config error: " + err.Error())
		os.Exit(2)
	}
	logger.Debug("config loaded", "path", *cfgPath, "target", cfg.Target.Name, "type", cfg.Target.Type)

	rep, runErr := engine.Run(context.Background(), cfg)
	logger.Debug("run finished", "verdict", rep.Verdict, "duration_ms", rep.DurationMS)

	// Spec 0027 R4: raw restore output is stderr-only, never serialized.
	// Redact is idempotent, so WriteJSON's internal call becomes a no-op.
	if raw := rep.Redact(); v.showRaw(*showOutput) {
		raw.Fprint(os.Stderr)
	}
	b, werr := rep.WriteJSON(cfg.Report.Out)
	if werr != nil {
		logger.Error("write report: " + werr.Error())
	}
	if cfg.Report.Out != "" && werr == nil {
		logger.Debug("report written", "path", cfg.Report.Out)
	}
	if cfg.Report.Sign && werr == nil {
		if sig, serr := report.Sign(cfg.Report.KeyPath, b); serr != nil {
			logger.Error("sign report: " + serr.Error())
		} else if cfg.Report.Out != "" {
			if err := report.WriteSignature(cfg.Report.Out+".sig", sig); err != nil {
				logger.Error("write signature: " + err.Error())
			}
		}
	}

	if *asJSON {
		// Spec 0026 R2/R3: stdout gets the exact bytes WriteJSON produced — the
		// same bytes written to report.out (plus the same trailing newline), so
		// the two destinations are byte-identical. Diagnostics above went to
		// stderr, so stdout is a single clean JSON document.
		if b != nil {
			os.Stdout.Write(append(b, '\n'))
		}
	} else {
		printSummary(rep)
	}

	// Spec 0030 R1/R2: the alert hook fires only after the report is written
	// (above) and never changes the exit code decided below.
	fireAlertHook(cfg, rep, b, cfg.Report.Out, runErr != nil)

	if runErr != nil {
		logger.Error("operational error: " + runErr.Error())
		os.Exit(2)
	}
	if !rep.Passed() {
		os.Exit(1)
	}
}

// fireAlertHook invokes the configured alerts hook for a finished run (spec
// 0030 R1): on_fail on a fail verdict or an operational error, on_success on
// a pass. reportJSON must be the exact bytes report.WriteJSON produced — the
// single redacted serialization (spec 0027) — never a re-serialization. The
// hook is best-effort by design (spec 0030 R2): every caller has already
// written the report, a hook failure is only logged to stderr, and the exit
// code is decided solely by the run outcome.
func fireAlertHook(cfg *config.Config, rep *report.Report, reportJSON []byte, reportPath string, opErr bool) {
	if cfg.Alerts == nil {
		return
	}
	spec := cfg.Alerts.OnSuccess
	if opErr || !rep.Passed() {
		spec = cfg.Alerts.OnFail
	}
	if spec == "" {
		return
	}
	if reportJSON == nil {
		// Warn, not error: the hook is best-effort by design and the exit code
		// is already decided by the run outcome (-quiet suppresses this).
		logger.Warn("alert hook: skipped (report was not rendered)")
		return
	}
	h := alert.Hook{Spec: spec, Timeout: cfg.Alerts.Timeout.Std()}
	if err := h.Fire(context.Background(), reportJSON, reportPath); err != nil {
		logger.Warn("alert hook: " + err.Error())
	}
}

func cmdCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "path to config file")
	v := addVerbosityFlags(fs)
	_ = fs.Parse(args)
	v.apply()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("config error: " + err.Error())
		os.Exit(2)
	}
	logger.Debug("config loaded", "path", *cfgPath, "target", cfg.Target.Name, "type", cfg.Target.Type)
	// The exec engine (spec 0020) is Docker-free: it runs the customer's own
	// restore command on the host. Only preflight Docker for the container
	// engines, and report installed-vs-not-running distinctly (spec 0020 fix A).
	if cfg.Target.Type == "exec" {
		fmt.Printf("ok — target %q valid (exec: no docker needed), %d check(s) defined\n",
			cfg.Target.Name, len(cfg.Target.Checks))
		return
	}
	if err := ephemeral.Preflight(context.Background()); err != nil {
		logger.Error(err.Error())
		os.Exit(2)
	}
	fmt.Printf("ok — target %q valid, docker reachable, %d check(s) defined\n",
		cfg.Target.Name, len(cfg.Target.Checks))
}

func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "emit the inspection as JSON")
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: salvage inspect [-json] <pgdata-dir>")
		os.Exit(2)
	}

	res, err := inspect.Inspect(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "inspect error:", err)
		os.Exit(2)
	}

	if *asJSON {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "encode json:", err)
			os.Exit(2)
		}
		fmt.Println(string(b))
		return
	}

	exts := "(none)"
	if len(res.RequiredPreloadExtensions) > 0 {
		exts = strings.Join(res.RequiredPreloadExtensions, ", ")
	}
	fmt.Printf("salvage inspect: %s\n", fs.Arg(0))
	fmt.Printf("  pg version          %s\n", res.PGVersion)
	fmt.Printf("  required extensions %s\n", exts)
	fmt.Printf("  databases           %d\n", res.DatabaseCount)
}

func printSummary(rep *report.Report) {
	fmt.Printf("\nsalvage: target %q\n", rep.Target)
	if rep.Restore.OK {
		fmt.Printf("  restore   ok    (%dms)\n", rep.Restore.DurationMS)
		if rep.Restore.Warnings != "" {
			fmt.Printf("  restore   warn  %s\n", rep.Restore.Warnings)
		}
	} else {
		fmt.Printf("  restore   FAIL  %s\n", rep.Restore.Error)
	}
	for _, c := range rep.Checks {
		switch {
		case c.Error != "":
			fmt.Printf("  check     ERR   %-26s %s\n", c.Name, c.Error)
		case c.OK:
			fmt.Printf("  check     ok    %-26s %s\n", c.Name, c.Detail)
		default:
			fmt.Printf("  check     FAIL  %-26s got=%s %s\n", c.Name, c.Got, c.Detail)
		}
	}
	fmt.Printf("  verdict   %s\n", strings.ToUpper(rep.Verdict))
}
