// Package version exposes build identity injected at link time from git tags.
//
// Default `make build-go` / `go build` leave Version as "dev". Release builds
// and `make build-go VERSION=v1.2.3` set:
//
//	-X github.com/ivanzzeth/specula/internal/version.Version=<tag>
//	-X github.com/ivanzzeth/specula/internal/version.Commit=<sha>
//	-X github.com/ivanzzeth/specula/internal/version.BuildDate=<RFC3339>
package version

import "fmt"

// These are overridden via -ldflags at build time. Do not edit defaults for
// release; CI/Makefile derive them from `git describe` / the push tag.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// String returns a single-line identity suitable for `specula version` and logs.
func String() string {
	return fmt.Sprintf("specula %s (commit %s, built %s)", Version, Commit, BuildDate)
}

// Short is Version alone (tag or "dev").
func Short() string {
	return Version
}
