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
		t.Fatalf("LoadFrom：%v", err)
	}
	if cfg.Server.Addr != ":6969" {
		t.Fatalf("监听地址 = %q", cfg.Server.Addr)
	}
	if cfg.Tenant.DefaultID != 1 {
		t.Fatalf("默认租户 = %d", cfg.Tenant.DefaultID)
	}
	if cfg.State.Dir != "data" {
		t.Fatalf("状态目录 = %q", cfg.State.Dir)
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
		t.Fatalf("LoadFrom：%v", err)
	}
	if cfg.Server.Addr != ":9100" {
		t.Errorf("环境变量应优先，监听地址 = %q", cfg.Server.Addr)
	}
	if cfg.Tenant.DefaultID != 9 {
		t.Errorf("环境变量应优先，租户 = %d", cfg.Tenant.DefaultID)
	}
	if cfg.State.Dir != "/tmp/from-env" {
		t.Errorf("环境变量应优先，状态目录 = %q", cfg.State.Dir)
	}
	if cfg.Server.RequestTimeout.Value() != 45*time.Second {
		t.Errorf("文件中的取值丢失，超时 = %v", cfg.Server.RequestTimeout)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Errorf("文件中的日志配置丢失：%+v", cfg.Log)
	}
}

func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "nope.json"), func(string) string { return "" })
	if err != nil {
		t.Fatalf("文件缺失不应报错：%v", err)
	}
	if cfg.Server.Addr != ":6969" {
		t.Fatalf("监听地址 = %q", cfg.Server.Addr)
	}
}

func TestInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path, func(string) string { return "" }); err == nil {
		t.Fatal("应当返回解析错误")
	}
}

func TestEngineAndAuthDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.SelfUpdate.Enabled || cfg.SelfUpdate.Repo == "" {
		t.Fatalf("自更新默认值不符: %+v", cfg.SelfUpdate)
	}
	if cfg.Engine.MaxConcurrent != 2 {
		t.Fatalf("engine.max_concurrent 默认值 = %d", cfg.Engine.MaxConcurrent)
	}
	if cfg.Engine.PlannerInterval.Value() != 2*time.Second ||
		cfg.Engine.WorkerInterval.Value() != 200*time.Millisecond ||
		cfg.Engine.LeaseTTL.Value() != 30*time.Second ||
		cfg.Engine.HeartbeatInterval.Value() != 10*time.Second {
		t.Fatalf("引擎时间默认值不符合预期: %+v", cfg.Engine)
	}
	if cfg.Auth.AdminToken != "" {
		t.Fatalf("auth.admin_token 默认应为空")
	}
}

func TestEngineEnvOverride(t *testing.T) {
	env := map[string]string{
		"BEACONX_MAX_CONCURRENT":   "4",
		"BEACONX_PLANNER_INTERVAL": "5s",
		"BEACONX_LEASE_TTL":        "45s",
		"BEACONX_ADMIN_TOKEN":      "secret-token",
	}
	cfg, err := LoadFrom("", func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Engine.MaxConcurrent != 4 || cfg.Engine.PlannerInterval.Value() != 5*time.Second ||
		cfg.Engine.LeaseTTL.Value() != 45*time.Second || cfg.Auth.AdminToken != "secret-token" {
		t.Fatalf("环境变量覆盖失败: %+v / %+v", cfg.Engine, cfg.Auth)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(*Config){
		"监听地址":     func(c *Config) { c.Server.Addr = "" },
		"租户":       func(c *Config) { c.Tenant.DefaultID = 0 },
		"状态目录":     func(c *Config) { c.State.Dir = "" },
		"日志级别":     func(c *Config) { c.Log.Level = "verbose" },
		"日志格式":     func(c *Config) { c.Log.Format = "yaml" },
		"最大连接数":    func(c *Config) { c.Database.MaxConns = 0 },
		"最小连接数超上限": func(c *Config) { c.Database.MinConns = 99 },
		"请求超时":     func(c *Config) { c.Server.RequestTimeout = 0 },
		"关闭超时":     func(c *Config) { c.Server.ShutdownTimeout = 0 },
		"请求体上限":    func(c *Config) { c.Server.MaxBodyBytes = 0 },
		"最大并发":     func(c *Config) { c.Engine.MaxConcurrent = 0 },
		"租约时长":     func(c *Config) { c.Engine.LeaseTTL = 0 },
		"策略动作":     func(c *Config) { c.Policy.DefaultAction = "maybe" },
		"审批超时":     func(c *Config) { c.Policy.ApprovalTimeout = 0 },
		"证据密钥":     func(c *Config) { c.Evidence.KeyHex = "abcd" },
		"模型部分配置":   func(c *Config) { c.LLM.BaseURL = "https://api.example.com" },
		"模型超时":     func(c *Config) { c.LLM.BaseURL = "https://x"; c.LLM.Model = "m"; c.LLM.Timeout = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s：应当返回校验错误", name)
			}
		})
	}
}

func TestEnvParseErrors(t *testing.T) {
	env := map[string]string{"BEACONX_DB_MAX_CONNS": "many"}
	if _, err := LoadFrom("", func(k string) string { return env[k] }); err == nil {
		t.Fatal("BEACONX_DB_MAX_CONNS 应当返回解析错误")
	}
	env = map[string]string{"BEACONX_REQUEST_TIMEOUT": "soon"}
	if _, err := LoadFrom("", func(k string) string { return env[k] }); err == nil {
		t.Fatal("BEACONX_REQUEST_TIMEOUT 应当返回解析错误")
	}
}
