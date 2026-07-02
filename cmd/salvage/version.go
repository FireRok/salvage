package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"salvage.sh/internal/version"
)

// latestReleaseURL is the single stable endpoint `version -check` queries
// (spec 0033 R6). The request carries nothing beyond the HTTP request itself —
// no identifiers, no telemetry — and uses only the Go standard library (R8).
const latestReleaseURL = "https://api.github.com/repos/firerok/salvage/releases/latest"

// installOneLiner is the documented one-command install (spec 0033 R3/R7).
const installOneLiner = "curl -fsSL https://salvage.sh/install.sh | sh"

// cmdVersion implements `salvage version [-check]` (spec 0033 R6). Without
// -check it prints the running version and performs no network I/O at all.
// With -check it additionally queries the latest published release, prints
// both versions, and exits 0 (up to date), 1 (a newer release exists — a
// result, not a crash), or 2 (the check could not be performed). Salvage
// never modifies its own binary: -check reports, the operator acts.
func cmdVersion(args []string) {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	check := fs.Bool("check", false, "also query the latest released version and compare (the only version path that touches the network)")
	_ = fs.Parse(args)

	fmt.Println("salvage " + version.String())
	if !*check {
		return
	}
	os.Exit(runUpdateCheck(os.Stdout, os.Stderr, version.Version, latestReleaseURL))
}

// runUpdateCheck fetches the latest release tag from apiURL, compares it with
// the running version, and returns the exit code per spec 0033 R6. A running
// version that is not a strict release version (a dev or off-tag build)
// cannot be meaningfully compared, so that is a failed check (2), not a
// result.
func runUpdateCheck(out, errw io.Writer, running, apiURL string) int {
	latest, err := fetchLatestVersion(apiURL)
	if err != nil {
		fmt.Fprintf(errw, "update check failed: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "  running  %s\n  latest   %s\n", running, strings.TrimPrefix(latest, "v"))
	rv, rok := parseSemver(running)
	if !rok {
		fmt.Fprintf(errw, "cannot compare: running version %q is not a release version (dev or off-tag build)\n", running)
		return 2
	}
	lv, lok := parseSemver(latest)
	if !lok {
		fmt.Fprintf(errw, "cannot compare: latest release tag %q is not a version\n", latest)
		return 2
	}
	if semverLess(rv, lv) {
		fmt.Fprintf(out, "\n  a newer release is available — install it with:\n    %s\n", installOneLiner)
		return 1
	}
	fmt.Fprintln(out, "  up to date")
	return 0
}

// fetchLatestVersion resolves the latest release tag (e.g. "v0.2.0") from a
// GitHub releases/latest API endpoint. Bounded by a 10s timeout.
func fetchLatestVersion(apiURL string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", apiURL, resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("parse release response: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("release response carried no tag_name")
	}
	return rel.TagName, nil
}

// parseSemver parses a strict X.Y.Z release version, tolerating a leading
// "v". Anything else — "0.0.0-dev", "(devel)", a git-describe form like
// "v0.2.0-3-gabc1234" — is not a released version and reports false.
func parseSemver(s string) ([3]int, bool) {
	var v [3]int
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		if p == "" {
			return v, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, false
			}
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// semverLess reports whether a precedes b.
func semverLess(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
