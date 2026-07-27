// Package version exposes build information stamped in at link time.
package version

import (
	"fmt"
	"runtime"
)

// Populated via -ldflags at build time, e.g.
//
//	go build -ldflags "-X github.com/ashdaily/spoofy/internal/version.Version=v0.1.0"
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders a single-line build identifier.
func String() string {
	return fmt.Sprintf("spoofy %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
