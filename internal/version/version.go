// Package version holds build metadata. Release builds inject it via -ldflags;
// `go install salvage.sh/cmd/salvage@vX` builds have no ldflags, so we fall back
// to Go's embedded build info (module version + VCS revision/time).
package version

import "runtime/debug"

var (
	// Version is the semantic version, set by the release build.
	Version = "0.0.0-dev"
	// Commit is the git SHA the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC3339).
	Date = "unknown"
)

func init() {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	// For `go install ...@vX` the main module version is the tag (e.g. "v0.1.1");
	// for a local build in the module it is "(devel)", which we leave alone.
	if Version == "0.0.0-dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "none" && s.Value != "" {
				Commit = s.Value
				if len(Commit) > 12 {
					Commit = Commit[:12]
				}
			}
		case "vcs.time":
			if Date == "unknown" && s.Value != "" {
				Date = s.Value
			}
		}
	}
}

// String renders a human-readable version line, dropping the commit/date detail
// when it is unknown (e.g. a proxy `go install`) so the output stays clean.
func String() string {
	if Commit == "none" && Date == "unknown" {
		return Version
	}
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
