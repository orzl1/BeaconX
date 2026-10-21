// Package config loads runtime configuration.
//
// Precedence (highest first):
//
//  1. environment variables (BEACONX_*)
//  2. the JSON file pointed to by BEACONX_CONFIG (default: ./config.json)
//  3. built-in defaults
//
// Business configuration lives in the database and is edited through the UI;
// this file only carries what is needed to boot: listener, database, tenant
// default, state directory and logging.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPath is the config file consulted when BEACONX_CONFIG is unset.
const DefaultPath = "config.json"

// Config is the full bootstrap configuration.
type Config struct {
	Server   Server   `json:"server"`
	Database Database `json:"database"`
	Tenant   Tenant   `json:"tenant"`
	State    State    `json:"state"`
	Log      Log      `json:"log"`
}

// Duration is a JSON-friendly time.Duration that accepts either a Go duration
// string ("30s") or an integer number of nanoseconds.
type Duration time.Duration

// Value returns the standard library duration.
func (d Duration) Value() time.Duration { return time.Duration(d) }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(parsed)
		return nil
	}
	var ns int64
	if err := json.Unmarshal(b, &ns); err != nil {
		return err
	}
	*d = Duration(time.Duration(ns))
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Server controls the HTTP listener and request lifecycle.
type Server struct {
	Addr              string   `json:"addr"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	RequestTimeout    Duration `json:"request_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
	MaxBodyBytes      int64    `json:"max_body_bytes"`
}

// Database is the PostgreSQL connection configuration.
type Database struct {
	DSN            string   `json:"dsn"`
	MaxConns       int32    `json:"max_conns"`
	MinConns       int32    `json:"min_conns"`
	ConnectTimeout Duration `json:"connect_timeout"`
}

// Tenant selects the default tenant for a single-tenant deployment.
type Tenant struct {
	DefaultID int64 `json:"default_id"`
}

// State is the home directory for sessions, evidence, traffic and keys.
type State struct {
	Dir string `json:"dir"`
}

// Log controls structured logging.
type Log struct {
	Level  string `json:"level"`  // debug | info | warn | error
	Format string `json:"format"` // json | text
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":8787",
			ReadHeaderTimeout: Duration(10 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			RequestTimeout:    Duration(30 * time.Second),
			ShutdownTimeout:   Duration(15 * time.Second),
			MaxBodyBytes:      8 << 20,
		},
		Database: Database{
			DSN:            "",
			MaxConns:       10,
			MinConns:       1,
			ConnectTimeout: Duration(8 * time.Second),
		},
		Tenant: Tenant{DefaultID: int64(1)},
		State:  State{Dir: "data"},
		Log:    Log{Level: "info", Format: "json"},
	}
}

// Load reads configuration using the process environment.
func Load() (Config, error) {
	return LoadFrom(envOr("BEACONX_CONFIG", DefaultPath), os.Getenv)
}

// LoadFrom reads configuration from an explicit path and an environment
// accessor. A missing file is not an error: defaults plus environment apply.
func LoadFrom(path string, getenv func(string) string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return Config{}, fmt.Errorf("parse config %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// Defaults + environment are a valid configuration.
		default:
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	for _, e := range []struct {
		key string
		set func(string) error
	}{
		{"BEACONX_ADDR", func(v string) error { cfg.Server.Addr = v; return nil }},
		{"BEACONX_DB_DSN", func(v string) error { cfg.Database.DSN = v; return nil }},
		{"BEACONX_DB_MAX_CONNS", func(v string) error { return parseInto(v, &cfg.Database.MaxConns, "BEACONX_DB_MAX_CONNS") }},
		{"BEACONX_DB_MIN_CONNS", func(v string) error { return parseInto(v, &cfg.Database.MinConns, "BEACONX_DB_MIN_CONNS") }},
		{"BEACONX_TENANT_DEFAULT_ID", func(v string) error { return parseInto(v, &cfg.Tenant.DefaultID, "BEACONX_TENANT_DEFAULT_ID") }},
		{"BEACONX_HOME", func(v string) error { cfg.State.Dir = v; return nil }},
		{"BEACONX_LOG_LEVEL", func(v string) error { cfg.Log.Level = v; return nil }},
		{"BEACONX_LOG_FORMAT", func(v string) error { cfg.Log.Format = v; return nil }},
		{"BEACONX_REQUEST_TIMEOUT", func(v string) error { return parseDuration(v, &cfg.Server.RequestTimeout, "BEACONX_REQUEST_TIMEOUT") }},
		{"BEACONX_SHUTDOWN_TIMEOUT", func(v string) error { return parseDuration(v, &cfg.Server.ShutdownTimeout, "BEACONX_SHUTDOWN_TIMEOUT") }},
	} {
		if v := strings.TrimSpace(getenv(e.key)); v != "" {
			if err := e.set(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseInto[T ~int32 | ~int64](v string, dst *T, name string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*dst = T(n)
	return nil
}

func parseDuration(v string, dst *Duration, name string) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*dst = Duration(d)
	return nil
}

// Validate rejects configurations that cannot boot.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return errors.New("server.addr must not be empty")
	}
	if c.Tenant.DefaultID <= 0 {
		return errors.New("tenant.default_id must be positive")
	}
	if strings.TrimSpace(c.State.Dir) == "" {
		return errors.New("state.dir must not be empty")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q is not one of debug|info|warn|error", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		return fmt.Errorf("log.format %q is not one of json|text", c.Log.Format)
	}
	if c.Database.MaxConns < 1 {
		return errors.New("database.max_conns must be >= 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return errors.New("database.min_conns must be between 0 and database.max_conns")
	}
	if c.Server.RequestTimeout <= 0 {
		return errors.New("server.request_timeout must be positive")
	}
	if c.Server.ShutdownTimeout <= 0 {
		return errors.New("server.shutdown_timeout must be positive")
	}
	if c.Server.MaxBodyBytes <= 0 {
		return errors.New("server.max_body_bytes must be positive")
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
