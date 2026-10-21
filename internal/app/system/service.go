// Package system exposes read-only process and database status.
package system

import (
	"context"
	"time"
)

// Database is the subset of the database used by readiness checks.
type Database interface {
	Ping(ctx context.Context) error
	AppliedMigration(ctx context.Context) (int64, error)
}

// Service reports information about this process.
type Service struct {
	DB              Database
	LatestMigration int64
	Build           BuildInfo
	StartedAt       time.Time
	Version         string
	Commit          string
	BuildTime       string
}

// BuildInfo mirrors build metadata without importing the version package.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// Info is the response payload for GET /api/v1/system/info.
type Info struct {
	Service         string    `json:"service"`
	Version         string    `json:"version"`
	Commit          string    `json:"commit"`
	BuildTime       string    `json:"build_time"`
	StartedAt       time.Time `json:"started_at"`
	Uptime          string    `json:"uptime"`
	SchemaVersion   int64     `json:"schema_version"`
	LatestMigration int64     `json:"latest_migration"`
}

// Check is one readiness probe result.
type Check struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Readiness is the response payload for GET /readyz.
type Readiness struct {
	Ready  bool    `json:"ready"`
	Checks []Check `json:"checks"`
}

// NewInfo builds the info payload.
func (s *Service) NewInfo(ctx context.Context) Info {
	applied, err := s.schemaVersion(ctx)
	if err != nil {
		applied = 0
	}
	return Info{
		Service:         "beaconx",
		Version:         s.Build.Version,
		Commit:          s.Build.Commit,
		BuildTime:       s.Build.BuildTime,
		StartedAt:       s.StartedAt,
		Uptime:          uptime(s.StartedAt),
		SchemaVersion:   applied,
		LatestMigration: s.LatestMigration,
	}
}

// Readiness checks the dependencies required to serve traffic.
func (s *Service) Readiness(ctx context.Context) Readiness {
	out := Readiness{Checks: make([]Check, 0, 2)}

	if s.DB == nil {
		out.Checks = append(out.Checks, Check{Name: "database", OK: false, Error: "not configured"})
		return out
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.DB.Ping(pingCtx); err != nil {
		out.Checks = append(out.Checks, Check{Name: "database", OK: false, Error: err.Error()})
	} else {
		out.Checks = append(out.Checks, Check{Name: "database", OK: true})
	}

	version, err := s.schemaVersion(ctx)
	switch {
	case err != nil:
		out.Checks = append(out.Checks, Check{Name: "schema", OK: false, Error: err.Error()})
	case version < s.LatestMigration:
		out.Checks = append(out.Checks, Check{
			Name:  "schema",
			OK:    false,
			Error: "pending migrations",
		})
	default:
		out.Checks = append(out.Checks, Check{Name: "schema", OK: true})
	}

	out.Ready = true
	for _, c := range out.Checks {
		if !c.OK {
			out.Ready = false
		}
	}
	return out
}

func (s *Service) schemaVersion(ctx context.Context) (int64, error) {
	if s.DB == nil {
		return 0, nil
	}
	return s.DB.AppliedMigration(ctx)
}

func uptime(startedAt time.Time) string {
	if startedAt.IsZero() {
		return "0s"
	}
	d := time.Since(startedAt).Round(time.Second)
	if d < 0 {
		return "0s"
	}
	return d.String()
}
