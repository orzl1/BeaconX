package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg, err := LoadFrom("", func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Server.Addr != ":8787" {
		t.Fatalf("addr = %q", cfg.Server.Addr)
	}
	if cfg.Tenant.DefaultID != 1 {
		t.Fatalf("default tenant = %d", cfg.Tenant.DefaultID)
	}
	if cfg.State.Dir != "data" {
		t.Fatalf("state dir = %q", cfg.State.Dir)
	}
}

func TestFileThenEnvPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
	  "server": {"addr": ":9000", "request_timeout": "45s"},
	  "tenant": {"default_id": 7},
	  "state": {"dir": "/tmp/from-file"},
	  "log": {"level": "debug", "format": "text"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{
		"BEACONX_ADDR":              ":9100",
		"BEACONX_TENANT_DEFAULT_ID": "9",
		"BEACONX_HOME":              "/tmp/from-env",
	}
	cfg, err := LoadFrom(path, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Server.Addr != ":9100" {
		t.Errorf("env should win, addr = %q", cfg.Server.Addr)
	}
	if cfg.Tenant.DefaultID != 9 {
		t.Errorf("env should win, tenant = %d", cfg.Tenant.DefaultID)
	}
	if cfg.State.Dir != "/tmp/from-env" {
		t.Errorf("env should win, state = %q", cfg.State.Dir)
	}
	if cfg.Server.RequestTimeout.Value() != 45*time.Second {
		t.Errorf("file value lost, timeout = %v", cfg.Server.RequestTimeout)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Errorf("file log config lost: %+v", cfg.Log)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "nope.json"), func(string) string { return "" })
	if err != nil {
		t.Fatalf("missing file must not fail: %v", err)
	}
	if cfg.Server.Addr != ":8787" {
		t.Fatalf("addr = %q", cfg.Server.Addr)
	}
}

func TestInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path, func(string) string { return "" }); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(*Config){
		"addr":         func(c *Config) { c.Server.Addr = "" },
		"tenant":       func(c *Config) { c.Tenant.DefaultID = 0 },
		"state":        func(c *Config) { c.State.Dir = "" },
		"log level":    func(c *Config) { c.Log.Level = "verbose" },
		"log format":   func(c *Config) { c.Log.Format = "yaml" },
		"max conns":    func(c *Config) { c.Database.MaxConns = 0 },
		"min>max":      func(c *Config) { c.Database.MinConns = 99 },
		"req timeout":  func(c *Config) { c.Server.RequestTimeout = 0 },
		"shut timeout": func(c *Config) { c.Server.ShutdownTimeout = 0 },
		"max body":     func(c *Config) { c.Server.MaxBodyBytes = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s: expected validation error", name)
			}
		})
	}
}

func TestEnvParseErrors(t *testing.T) {
	env := map[string]string{"BEACONX_DB_MAX_CONNS": "many"}
	if _, err := LoadFrom("", func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected parse error for BEACONX_DB_MAX_CONNS")
	}
	env = map[string]string{"BEACONX_REQUEST_TIMEOUT": "soon"}
	if _, err := LoadFrom("", func(k string) string { return env[k] }); err == nil {
		t.Fatal("expected parse error for BEACONX_REQUEST_TIMEOUT")
	}
}
