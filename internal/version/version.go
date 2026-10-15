// Package version holds the build version and the process start time.
package version

import (
	"runtime/debug"
	"time"
)

// Version is set at build time by build.sh with
// -ldflags "-X github.com/timjaanson/cubby/internal/version.Version=...".
// Without that, it falls back to the version Go stamps into every binary:
// the module version for `go install ...@v0.6.0`, or a pseudo-version from
// git for a plain `go build` in a checkout.
var Version = "dev"

// BuildTime is when the process started. It is the Last-Modified time for
// /kv.js and the base for the uptime in admin stats.
var BuildTime = time.Now()

func init() {
	if Version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}
