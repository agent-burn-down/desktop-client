// Package version exposes build metadata injected at link time via -ldflags.
package version

import "fmt"

// Set by goreleaser / Makefile:
//
//	-X github.com/agent-burn-down/desktop-client/internal/version.Version=v1.2.3
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the human-readable version string used by --version.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
