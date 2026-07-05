// Package version exposes build-time version metadata.
package version

import "runtime"

// Version is set at link time via -ldflags "-X ...version.Version=...".
// Defaults to "dev" when unset (e.g. `go run`, `go test`).
var Version = "dev"

// Commit is set at link time; defaults to "unknown".
var Commit = "unknown"

// BuildTime is set at link time; defaults to "unknown".
var BuildTime = "unknown"

// Info returns a structured version payload for /api/v1/system and CLI --version.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	Compiler  string `json:"compiler"`
	Platform  string `json:"platform"`
}

// Get returns the current build Info.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
		Compiler:  runtime.Compiler,
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String returns a human-readable version line.
func String() string {
	return "openviking " + Version + " " + runtime.GOOS + "/" + runtime.GOARCH + " " + runtime.Version()
}
