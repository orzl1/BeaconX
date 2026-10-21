// Package config 加载运行时配置。
//
// 优先级（从高到低）：
//
//  1. 环境变量（BEACONX_*）
//  2. BEACONX_CONFIG 指向的 JSON 文件（默认：./config.json）
//  3. 内置默认值
//
// 业务配置存放在数据库中，并通过 UI 编辑；本文件只承载启动所必需的内容：
// 监听地址、数据库、默认租户、状态目录与日志。
package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPath 是未设置 BEACONX_CONFIG 时读取的配置文件。
const DefaultPath = "config.json"

// Config 是完整的启动配置。
type Config struct {
	Server     Server     `json:"server"`
	Database   Database   `json:"database"`
	Tenant     Tenant     `json:"tenant"`
	State      State      `json:"state"`
	Log        Log        `json:"log"`
	Engine     Engine     `json:"engine"`
	Auth       Auth       `json:"auth"`
	LLM        LLM        `json:"llm"`
	Policy     Policy     `json:"policy"`
	Evidence   Evidence   `json:"evidence"`
	SelfUpdate SelfUpdate `json:"selfupdate"`
}

// SelfUpdate 是自更新配置。
type SelfUpdate struct {
	Enabled bool   `json:"enabled"`
	Repo    string `json:"repo"`
}

// Evidence 是证据存储配置。
type Evidence struct {
	Dir    string `json:"dir"`
	KeyHex string `json:"key_hex"`
}

// Engine 是任务运行时参数。
type Engine struct {
	MaxConcurrent     int      `json:"max_concurrent"`
	PlannerInterval   Duration `json:"planner_interval"`
	WorkerInterval    Duration `json:"worker_interval"`
	LeaseTTL          Duration `json:"lease_ttl"`
	HeartbeatInterval Duration `json:"heartbeat_interval"`
}

// Policy 是工具调用策略的默认行为。
type Policy struct {
	DefaultAction   string   `json:"default_action"` // allow|ask|deny
	ApprovalTimeout Duration `json:"approval_timeout"`
}

// Auth 是管理接口鉴权配置。
type Auth struct {
	AdminToken string `json:"admin_token"`
}

// LLMProfile 是路由中的一个模型端点。
type LLMProfile struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

// LLM 是 OpenAI 兼容模型端点配置；base_url 与 model 同时非空时启用 AI 规划。
type LLM struct {
	// Fallbacks 是故障转移候选（按顺序尝试）。
	Fallbacks   []LLMProfile `json:"fallbacks,omitempty"`
	BaseURL     string       `json:"base_url"`
	APIKey      string       `json:"api_key"`
	Model       string       `json:"model"`
	MaxTokens   int          `json:"max_tokens"`
	Temperature *float64     `json:"temperature,omitempty"`
	Timeout     Duration     `json:"timeout"`
	MaxRetries  int          `json:"max_retries"`
}

// Duration 是一个便于 JSON 序列化的 time.Duration，既接受 Go duration 字符串
// （"30s"），也接受以纳秒为单位的整数。
type Duration time.Duration

// Value 返回标准库的 time.Duration。
func (d Duration) Value() time.Duration { return time.Duration(d) }

// UnmarshalJSON 实现 json.Unmarshaler。
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

// MarshalJSON 实现 json.Marshaler。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Server 控制 HTTP 监听器与请求生命周期。
type Server struct {
	Addr              string   `json:"addr"`
	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	IdleTimeout       Duration `json:"idle_timeout"`
	RequestTimeout    Duration `json:"request_timeout"`
	ShutdownTimeout   Duration `json:"shutdown_timeout"`
	MaxBodyBytes      int64    `json:"max_body_bytes"`
}

// Database 是 PostgreSQL 连接配置。
type Database struct {
	DSN            string   `json:"dsn"`
	MaxConns       int32    `json:"max_conns"`
	MinConns       int32    `json:"min_conns"`
	ConnectTimeout Duration `json:"connect_timeout"`
}

// Tenant 选择单租户部署下的默认租户。
type Tenant struct {
	DefaultID int64 `json:"default_id"`
}

// State 是会话、证据、流量与密钥所在的根目录。
type State struct {
	Dir string `json:"dir"`
}

// Log 控制结构化日志。
type Log struct {
	Level  string `json:"level"`  // 取值：debug | info | warn | error
	Format string `json:"format"` // 取值：json | text
}

// Default 返回内置默认值。
func Default() Config {
	return Config{
		Server: Server{
			Addr:              ":6969",
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
		Policy: Policy{
			DefaultAction:   "ask",
			ApprovalTimeout: Duration(60 * time.Second),
		},
		SelfUpdate: SelfUpdate{
			Enabled: true,
			Repo:    "orzl1/BeaconX",
		},
		LLM: LLM{
			MaxTokens:  2048,
			Timeout:    Duration(60 * time.Second),
			MaxRetries: 2,
		},
		Engine: Engine{
			MaxConcurrent:     2,
			PlannerInterval:   Duration(2 * time.Second),
			WorkerInterval:    Duration(200 * time.Millisecond),
			LeaseTTL:          Duration(30 * time.Second),
			HeartbeatInterval: Duration(10 * time.Second),
		},
	}
}

// Load 使用进程环境读取配置。
func Load() (Config, error) {
	return LoadFrom(envOr("BEACONX_CONFIG", DefaultPath), os.Getenv)
}

// LoadFrom 从显式路径与环境变量访问器读取配置。文件缺失不算错误：
// 此时会应用默认值与环境变量。
func LoadFrom(path string, getenv func(string) string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return Config{}, fmt.Errorf("解析配置 %s 失败：%w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// 默认值加环境变量本身就是一份有效配置。
		default:
			return Config{}, fmt.Errorf("读取配置 %s 失败：%w", path, err)
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
		{"BEACONX_MAX_CONCURRENT", func(v string) error { return parseInto(v, &cfg.Engine.MaxConcurrent, "BEACONX_MAX_CONCURRENT") }},
		{"BEACONX_PLANNER_INTERVAL", func(v string) error { return parseDuration(v, &cfg.Engine.PlannerInterval, "BEACONX_PLANNER_INTERVAL") }},
		{"BEACONX_WORKER_INTERVAL", func(v string) error { return parseDuration(v, &cfg.Engine.WorkerInterval, "BEACONX_WORKER_INTERVAL") }},
		{"BEACONX_LEASE_TTL", func(v string) error { return parseDuration(v, &cfg.Engine.LeaseTTL, "BEACONX_LEASE_TTL") }},
		{"BEACONX_HEARTBEAT_INTERVAL", func(v string) error {
			return parseDuration(v, &cfg.Engine.HeartbeatInterval, "BEACONX_HEARTBEAT_INTERVAL")
		}},
		{"BEACONX_ADMIN_TOKEN", func(v string) error { cfg.Auth.AdminToken = v; return nil }},
		{"BEACONX_UPDATE_ENABLED", func(v string) error {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("BEACONX_UPDATE_ENABLED 不是布尔值: %w", err)
			}
			cfg.SelfUpdate.Enabled = b
			return nil
		}},
		{"BEACONX_UPDATE_REPO", func(v string) error { cfg.SelfUpdate.Repo = v; return nil }},
		{"BEACONX_EVIDENCE_DIR", func(v string) error { cfg.Evidence.Dir = v; return nil }},
		{"BEACONX_EVIDENCE_KEY", func(v string) error { cfg.Evidence.KeyHex = v; return nil }},
		{"BEACONX_POLICY_DEFAULT_ACTION", func(v string) error { cfg.Policy.DefaultAction = v; return nil }},
		{"BEACONX_APPROVAL_TIMEOUT", func(v string) error { return parseDuration(v, &cfg.Policy.ApprovalTimeout, "BEACONX_APPROVAL_TIMEOUT") }},
		{"BEACONX_LLM_BASE_URL", func(v string) error { cfg.LLM.BaseURL = v; return nil }},
		{"BEACONX_LLM_API_KEY", func(v string) error { cfg.LLM.APIKey = v; return nil }},
		{"BEACONX_LLM_MODEL", func(v string) error { cfg.LLM.Model = v; return nil }},
		{"BEACONX_LLM_MAX_TOKENS", func(v string) error { return parseInto(v, &cfg.LLM.MaxTokens, "BEACONX_LLM_MAX_TOKENS") }},
		{"BEACONX_LLM_TIMEOUT", func(v string) error { return parseDuration(v, &cfg.LLM.Timeout, "BEACONX_LLM_TIMEOUT") }},
		{"BEACONX_LLM_MAX_RETRIES", func(v string) error { return parseInto(v, &cfg.LLM.MaxRetries, "BEACONX_LLM_MAX_RETRIES") }},
	} {
		if v := strings.TrimSpace(getenv(e.key)); v != "" {
			if err := e.set(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseInto[T ~int | ~int32 | ~int64](v string, dst *T, name string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%s：%w", name, err)
	}
	*dst = T(n)
	return nil
}

func parseDuration(v string, dst *Duration, name string) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s：%w", name, err)
	}
	*dst = Duration(d)
	return nil
}

// Validate 拒绝无法启动的配置。
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return errors.New("server.addr 不能为空")
	}
	if c.Tenant.DefaultID <= 0 {
		return errors.New("tenant.default_id 必须为正数")
	}
	if strings.TrimSpace(c.State.Dir) == "" {
		return errors.New("state.dir 不能为空")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q 不是 debug|info|warn|error 之一", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		return fmt.Errorf("log.format %q 不是 json|text 之一", c.Log.Format)
	}
	if c.Database.MaxConns < 1 {
		return errors.New("database.max_conns 必须大于等于 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return errors.New("database.min_conns 必须介于 0 与 database.max_conns 之间")
	}
	if c.Server.RequestTimeout <= 0 {
		return errors.New("server.request_timeout 必须为正数")
	}
	if c.Server.ShutdownTimeout <= 0 {
		return errors.New("server.shutdown_timeout 必须为正数")
	}
	if c.Server.MaxBodyBytes <= 0 {
		return errors.New("server.max_body_bytes 必须为正数")
	}
	if c.Engine.MaxConcurrent <= 0 {
		return errors.New("engine.max_concurrent 必须大于等于 1")
	}
	for name, d := range map[string]Duration{
		"engine.planner_interval":   c.Engine.PlannerInterval,
		"engine.worker_interval":    c.Engine.WorkerInterval,
		"engine.lease_ttl":          c.Engine.LeaseTTL,
		"engine.heartbeat_interval": c.Engine.HeartbeatInterval,
	} {
		if d.Value() <= 0 {
			return fmt.Errorf("%s 必须为正数", name)
		}
	}
	switch c.Policy.DefaultAction {
	case "allow", "ask", "deny":
	default:
		return fmt.Errorf("policy.default_action %q 不是 allow|ask|deny 之一", c.Policy.DefaultAction)
	}
	if c.Policy.ApprovalTimeout.Value() <= 0 {
		return errors.New("policy.approval_timeout 必须为正数")
	}
	if c.SelfUpdate.Enabled && strings.TrimSpace(c.SelfUpdate.Repo) == "" {
		return errors.New("selfupdate.repo 不能为空")
	}
	if c.Evidence.KeyHex != "" {
		key, err := hex.DecodeString(c.Evidence.KeyHex)
		if err != nil || len(key) != 32 {
			return errors.New("evidence.key_hex 必须是 64 位十六进制字符串（32 字节）")
		}
	}
	if (c.LLM.BaseURL == "") != (c.LLM.Model == "") {
		return errors.New("llm.base_url 与 llm.model 必须同时设置")
	}
	for i, fb := range c.LLM.Fallbacks {
		if fb.BaseURL == "" || fb.Model == "" {
			return fmt.Errorf("llm.fallbacks[%d] 必须同时设置 base_url 与 model", i)
		}
	}
	if c.LLM.BaseURL != "" {
		if c.LLM.MaxTokens <= 0 {
			return errors.New("llm.max_tokens 必须为正数")
		}
		if c.LLM.Timeout.Value() <= 0 {
			return errors.New("llm.timeout 必须为正数")
		}
		if c.LLM.MaxRetries < 0 {
			return errors.New("llm.max_retries 必须大于等于 0")
		}
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
