// Package version holds build metadata, injected at build time via -ldflags.
package version

var (
	// Version is the semantic version, set by the release build.
	Version = "0.0.0-dev"
	// Commit is the git SHA the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC3339).
	Date = "unknown"
)

// String renders a human-readable version line.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
