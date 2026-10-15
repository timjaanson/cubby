// Package version holds the build version and the process start time.
package version

import "time"

// Version is set at build time with
// -ldflags "-X github.com/timjaanson/cubby/internal/version.Version=...".
var Version = "dev"

// BuildTime is when the process started. It is the Last-Modified time for
// /kv.js and the base for the uptime in admin stats.
var BuildTime = time.Now()
