// Command salvage proves a backup actually restores and works.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/attest"
	"salvage.sh/internal/config"
	"salvage.sh/internal/engine"
	"salvage.sh/internal/inspect"
	"salvage.sh/internal/report"
	"salvage.sh/internal/version"
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
	case "version", "-v", "--version":
		fmt.Println("salvage " + version.String())
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
  salvage run    -config salvage.yaml   restore a backup into a throwaway db and assert it works
  salvage check  -config salvage.yaml   validate config and preflight docker (no restore)
  salvage inspect [-json] <pgdata-dir>  offline pre-flight: report PG version + required extensions (no start)
  salvage scaffold -config salvage.yaml restore + introspect, then emit a starter config with auto-generated checks
  salvage last-good -config salvage.yaml  walk a pgbackrest chain newest-first; report the freshest restorable backup
  salvage fleet    -config salvage.yaml   enumerate every stanza in a pgbackrest repo; optionally emit a config per stanza
  salvage schedule -config salvage.yaml   print a systemd timer + cron line to run 'salvage attest' on a cadence
  salvage login    [-endpoint URL]        sign in via your browser and store an API key locally (device flow)
  salvage logout                          remove the stored API key
  salvage attest   -config salvage.yaml   run the test, then submit the signed report to a hosted attestation notary
  salvage verify   <id|url>               fetch an attestation and verify it offline against Firerok's public key
  salvage version
  salvage help

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
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	rendered, err := engine.Scaffold(context.Background(), cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scaffold error:", err)
		os.Exit(2)
	}
	if *outPath == "" {
		os.Stdout.Write(rendered)
		return
	}
	if err := os.WriteFile(*outPath, rendered, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *outPath)
}

func cmdLastGood(args []string) {
	fs := flag.NewFlagSet("last-good", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "config with the pgbackrest source + restore")
	maxTry := fs.Int("max", 0, "max backups to try (0 = until the first that restores)")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	lg, err := engine.LastGood(context.Background(), cfg, *maxTry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "last-good error:", err)
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
	outDir := fs.String("o", "", "write a per-stanza skeleton config into this directory")
	asJSON := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	fl, err := engine.Fleet(context.Background(), cfg, *outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleet error:", err)
		os.Exit(2)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(fl, "", "  ")
		fmt.Println(string(b))
	} else {
		printFleet(fl)
	}
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
		key, status, err := attest.DevicePoll(ctx, *endpoint, dc.DeviceCode)
		if err != nil {
			fmt.Fprintln(os.Stderr, "\npoll error:", err)
			os.Exit(2)
		}
		if key != "" {
			if err := attest.SaveCredentials(&attest.Credentials{Endpoint: *endpoint, APIKey: key}); err != nil {
				fmt.Fprintln(os.Stderr, "\nsave credentials:", err)
				os.Exit(2)
			}
			fmt.Println("\n\n✓ Signed in — stored an API key in ~/.salvage/credentials.")
			fmt.Println("  salvage attest will use it automatically.")
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
	_ = fs.Parse(args)

	cfg, cfgErr := config.Load(*cfgPath)

	var reportBytes []byte
	var sigB64, pubB64 string

	if *reportPath != "" {
		b, err := os.ReadFile(*reportPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read report:", err)
			os.Exit(2)
		}
		reportBytes = b
		if *sigPath != "" {
			sb, err := os.ReadFile(*sigPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "read sig:", err)
				os.Exit(2)
			}
			var s report.Signature
			if err := json.Unmarshal(sb, &s); err != nil {
				fmt.Fprintln(os.Stderr, "parse sig:", err)
				os.Exit(2)
			}
			sigB64, pubB64 = s.Signature, s.PublicKey
		}
	} else {
		if cfgErr != nil {
			fmt.Fprintln(os.Stderr, "config error:", cfgErr)
			os.Exit(2)
		}
		rep, runErr := engine.Run(context.Background(), cfg)
		b, _ := rep.WriteJSON("") // bytes only; do not write to disk here
		reportBytes = b
		// A tenant signature is optional; produce one when a signing key is configured.
		if cfg.Report.KeyPath != "" {
			if s, serr := report.Sign(cfg.Report.KeyPath, b); serr == nil {
				sigB64, pubB64 = s.Signature, s.PublicKey
			} else {
				fmt.Fprintln(os.Stderr, "warning: could not sign report locally:", serr)
			}
		}
		printSummary(rep)
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "operational error:", runErr)
			os.Exit(2)
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
		fmt.Fprintln(os.Stderr, "no notary endpoint (set -endpoint, attest.endpoint, or run salvage login)")
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
		fmt.Fprintf(os.Stderr, "no API key (set %s, or run salvage login)\n", envName)
		os.Exit(2)
	}

	resp, err := attest.Submit(context.Background(), ep, apiKey, reportBytes, sigB64, pubB64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "attest error:", err)
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
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: salvage verify <attestation-id|url>")
		os.Exit(2)
	}
	rec, err := attest.Fetch(context.Background(), *endpoint, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify error:", err)
		os.Exit(2)
	}
	checks, ok := attest.Verify(rec)
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
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}

	rep, runErr := engine.Run(context.Background(), cfg)

	b, werr := rep.WriteJSON(cfg.Report.Out)
	if werr != nil {
		fmt.Fprintln(os.Stderr, "write report:", werr)
	}
	if cfg.Report.Sign && werr == nil {
		if sig, serr := report.Sign(cfg.Report.KeyPath, b); serr != nil {
			fmt.Fprintln(os.Stderr, "sign report:", serr)
		} else if cfg.Report.Out != "" {
			if err := report.WriteSignature(cfg.Report.Out+".sig", sig); err != nil {
				fmt.Fprintln(os.Stderr, "write signature:", err)
			}
		}
	}

	printSummary(rep)

	if runErr != nil {
		fmt.Fprintln(os.Stderr, "operational error:", runErr)
		os.Exit(2)
	}
	if !rep.Passed() {
		os.Exit(1)
	}
}

func cmdCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "salvage.yaml", "path to config file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	if err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "docker not available (is the daemon running?):", err)
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
