// Package version 暴露构建元数据。取值在构建时注入：
//
//	go build -ldflags "-X github.com/orzl1/BeaconX/internal/version.Version=1.2.3 \
//	  -X github.com/orzl1/BeaconX/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/orzl1/BeaconX/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

import "time"

var (
	// Version 是本次构建的语义化版本（本地构建为 "dev"）。
	Version = "dev"
	// Commit 是构建该二进制时所基于的源码修订。
	Commit = "unknown"
	// BuildTime 是 UTC 构建时间戳。
	BuildTime = "unknown"
)

// Info 是构建元数据的运行时视图。
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// Current 返回运行中二进制的构建元数据。
func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildTime: BuildTime}
}

// Uptime 以四舍五入的人类可读形式渲染运行时长。
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
