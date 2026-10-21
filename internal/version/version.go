// Package version exposes build metadata. Values are injected at build time:
//
//	go build -ldflags "-X github.com/orzl1/BeaconX/internal/version.Version=1.2.3 \
//	  -X github.com/orzl1/BeaconX/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/orzl1/BeaconX/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

import "time"

var (
	// Version is the semantic version of this build ("dev" for local builds).
	Version = "dev"
	// Commit is the source revision this binary was built from.
	Commit = "unknown"
	// BuildTime is the UTC build timestamp.
	BuildTime = "unknown"
)

// Info is the runtime view of build metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// Current returns the build metadata of the running binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildTime: BuildTime}
}

// Uptime renders an uptime duration in a rounded, human-readable form.
func Uptime(startedAt time.Time) string {
	if startedAt.IsZero() {
		return "0s"
	}
	d := time.Since(startedAt).Round(time.Second)
	if d < 0 {
		return "0s"
	}
	return d.String()
}
