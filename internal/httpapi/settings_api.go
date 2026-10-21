package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/orzl1/BeaconX/internal/app/executor"
	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B4 运行时设置接口（P-B4-18..19）。
//
// 存储：settings 表（迁移 0008），键使用与 config 嵌套路径一致的点号命名，
// value 为 JSONB。HTTP 层负责白名单、类型与范围校验，并把扁平的键值投影成
// 前端期望的嵌套结构：
//
//	{"settings":{
//	   "max_concurrent":2,
//	   "default_budget":{"max_tokens":0,"max_steps":0},
//	   "default_action":"ask",
//	   "approval_timeout":"60s"
//	}}
//
// 热生效：启动时 main 用 ApplyPersistedRuntimeSettings 从 settings 表恢复运行参数
// （缺省键沿用 config 注入的 SettingsFallback）；PATCH 成功后立即调用
// executor/engine/runtime 的线程安全 setter。任务默认预算保存在 RuntimeConfig，
// 创建任务未显式给预算时使用。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/settings", Summary: "运行时设置",
			Build: func(o Options) http.HandlerFunc { return o.handleSettingsGet },
		},
		FeatureRoute{
			Method: http.MethodPatch, Pattern: "/api/v1/settings", Summary: "更新运行时设置",
			Build: func(o Options) http.HandlerFunc { return o.handleSettingsPatch },
		},
	)
}

// 设置键（持久化用点号命名，与 config 的嵌套路径一致）。
const (
	settingKeyMaxConcurrent    = "engine.max_concurrent"
	settingKeyDefaultAction    = "policy.default_action"
	settingKeyApprovalTimeout  = "policy.approval_timeout"
	settingKeyDefaultMaxTokens = "mission.default_max_tokens"
	settingKeyDefaultMaxSteps  = "mission.default_max_steps"
)

// 默认值：与 config.Default() 保持一致（数据库无记录时使用）。
const (
	defaultMaxConcurrent   = 2
	defaultApprovalSeconds = 60
)

// settingsBudgetView 是默认任务预算视图。
type settingsBudgetView struct {
	MaxTokens int64 `json:"max_tokens"`
	MaxSteps  int   `json:"max_steps"`
}

// settingsView 是 GET/PATCH /settings 返回的生效设置。
type settingsView struct {
	MaxConcurrent   int                `json:"max_concurrent"`
	DefaultBudget   settingsBudgetView `json:"default_budget"`
	DefaultAction   string             `json:"default_action"`
	ApprovalTimeout string             `json:"approval_timeout"`
}

// settingsResponse 是设置接口的响应信封。
type settingsResponse struct {
	Settings settingsView `json:"settings"`
}

// RuntimeSettings 是运行期可热更新的有效参数快照。
//
// 它同时承担两个职责：启动时合并「settings 表值 + config 回退」，PATCH 后作为
// executor/engine/runtime 的即时应用输入。
type RuntimeSettings struct {
	MaxConcurrent    int
	DefaultAction    policy.Action
	ApprovalTimeout  time.Duration
	DefaultMaxTokens int64
	DefaultMaxSteps  int
}

// DefaultRuntimeSettings 返回与 config.Default 一致的内置默认值。
func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		MaxConcurrent:    defaultMaxConcurrent,
		DefaultAction:    policy.Ask,
		ApprovalTimeout:  time.Duration(defaultApprovalSeconds) * time.Second,
		DefaultMaxTokens: 0,
		DefaultMaxSteps:  0,
	}
}

// normalized 用内置默认修正零值/非法字段（settings 表无值或 config 回退时）。
func (s RuntimeSettings) normalized() RuntimeSettings {
	def := DefaultRuntimeSettings()
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = def.MaxConcurrent
	}
	if !s.DefaultAction.Valid() {
		s.DefaultAction = def.DefaultAction
	}
	if s.ApprovalTimeout <= 0 {
		s.ApprovalTimeout = def.ApprovalTimeout
	}
	if s.DefaultMaxTokens < 0 {
		s.DefaultMaxTokens = 0
	}
	if s.DefaultMaxSteps < 0 {
		s.DefaultMaxSteps = 0
	}
	return s
}

// view 把有效参数投影成 HTTP 设置视图。
func (s RuntimeSettings) view() settingsView {
	return settingsView{
		MaxConcurrent:   s.MaxConcurrent,
		DefaultBudget:   settingsBudgetView{MaxTokens: s.DefaultMaxTokens, MaxSteps: s.DefaultMaxSteps},
		DefaultAction:   string(s.DefaultAction),
		ApprovalTimeout: formatTimeout(s.ApprovalTimeout),
	}
}

// RuntimeConfig 保存创建任务时使用的默认预算，可被 PATCH 热更新。
//
// 用独立对象而不是直接改 Options：Options 是值传递，handler 各自拿到副本，
// 必须有一个指针对象承载跨请求可见的运行时状态。
type RuntimeConfig struct {
	mu        sync.RWMutex
	maxTokens int64
	maxSteps  int
}

// NewRuntimeConfig 用回退设置初始化默认预算。
func NewRuntimeConfig(fallback RuntimeSettings) *RuntimeConfig {
	fallback = fallback.normalized()
	return &RuntimeConfig{maxTokens: fallback.DefaultMaxTokens, maxSteps: fallback.DefaultMaxSteps}
}

// MissionDefaults 返回创建任务时使用的默认预算；RuntimeConfig 为 nil 时返回零值。
func (c *RuntimeConfig) MissionDefaults() (int64, int) {
	if c == nil {
		return 0, 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.maxTokens, c.maxSteps
}

// SetMissionDefaults 热更新任务默认预算。
func (c *RuntimeConfig) SetMissionDefaults(maxTokens int64, maxSteps int) {
	if c == nil {
		return
	}
	if maxTokens < 0 {
		maxTokens = 0
	}
	if maxSteps < 0 {
		maxSteps = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxTokens = maxTokens
	c.maxSteps = maxSteps
}

// effectiveRuntimeSettings 合并 settings 表与回退配置；非法/损坏的持久化值回退。
func effectiveRuntimeSettings(ctx context.Context, store *pg.Store, tenantID int64, fallback RuntimeSettings) (RuntimeSettings, error) {
	effective := fallback.normalized()
	raw, err := store.GetSettingsMap(ctx, tenantID)
	if err != nil {
		return RuntimeSettings{}, err
	}
	if v, ok := raw[settingKeyMaxConcurrent]; ok {
		if n, parseErr := parseJSONInt(v); parseErr == nil && n >= 1 && n <= 64 {
			effective.MaxConcurrent = n
		}
	}
	if v, ok := raw[settingKeyDefaultAction]; ok {
		var action string
		if json.Unmarshal(v, &action) == nil && policy.Action(action).Valid() {
			effective.DefaultAction = policy.Action(action)
		}
	}
	if v, ok := raw[settingKeyApprovalTimeout]; ok {
		if d, parseErr := parseTimeoutValue(v); parseErr == nil {
			effective.ApprovalTimeout = d
		}
	}
	if v, ok := raw[settingKeyDefaultMaxTokens]; ok {
		var n int64
		if json.Unmarshal(v, &n) == nil && n >= 0 {
			effective.DefaultMaxTokens = n
		}
	}
	if v, ok := raw[settingKeyDefaultMaxSteps]; ok {
		if n, parseErr := parseJSONInt(v); parseErr == nil && n >= 0 {
			effective.DefaultMaxSteps = n
		}
	}
	return effective, nil
}

// loadSettingsView 读取「settings 表 + config 回退」合并后的有效视图。
func (o Options) loadSettingsView(r *http.Request) (settingsView, error) {
	effective, err := effectiveRuntimeSettings(r.Context(), o.Store, o.TenantID, o.SettingsFallback)
	if err != nil {
		return settingsView{}, err
	}
	return effective.view(), nil
}

// applyRuntimeSettings 把有效参数即时应用到运行时组件（nil 组件跳过）。
func (o Options) applyRuntimeSettings(effective RuntimeSettings) {
	applyRuntimeTo(effective, o.Engine, o.Executor, o.Runtime)
}

// applyRuntimeTo 把有效参数写入 executor/engine/runtime；供 PATCH 与 main 启动共用。
func applyRuntimeTo(effective RuntimeSettings, engine *runner.Engine, exec *executor.Executor, runtime *RuntimeConfig) {
	effective = effective.normalized()
	if exec != nil {
		exec.SetDefaultAction(effective.DefaultAction)
		exec.SetApprovalTimeout(effective.ApprovalTimeout)
	}
	if engine != nil {
		engine.SetMaxConcurrent(effective.MaxConcurrent)
	}
	if runtime != nil {
		runtime.SetMissionDefaults(effective.DefaultMaxTokens, effective.DefaultMaxSteps)
	}
}

// LoadPersistedRuntimeSettings 只读取并合并「settings 表 + 回退配置」，不应用。
//
// main 在构造 executor/engine 之前调用，把有效值直接写进它们的 Config；这样
// 进程首次启动就不会先按旧值构造再覆盖一次。
func LoadPersistedRuntimeSettings(ctx context.Context, store *pg.Store, tenantID int64, fallback RuntimeSettings) (RuntimeSettings, error) {
	return effectiveRuntimeSettings(ctx, store, tenantID, fallback)
}

// ApplyPersistedRuntimeSettings 在进程启动时从 settings 表恢复运行参数并应用。
//
// fallback 是 config 的有效值；settings 缺省键沿用 fallback。main 在 engine.Start
// 之前调用，保证首次调度就使用持久化的并发上限。
func ApplyPersistedRuntimeSettings(ctx context.Context, store *pg.Store, tenantID int64,
	fallback RuntimeSettings, engine *runner.Engine, exec *executor.Executor, runtime *RuntimeConfig) (RuntimeSettings, error) {
	effective, err := effectiveRuntimeSettings(ctx, store, tenantID, fallback)
	if err != nil {
		return RuntimeSettings{}, err
	}
	applyRuntimeTo(effective, engine, exec, runtime)
	return effective, nil
}

// handleSettingsGet 返回生效设置（持久化值 + 默认值补齐）。
func (o Options) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	view, err := o.loadSettingsView(r)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settingsResponse{Settings: view})
}

// handleSettingsPatch 局部更新设置并返回更新后的完整设置。
func (o Options) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var patch map[string]json.RawMessage
	if !decodeJSONBody(w, r, &patch) {
		return
	}
	if patch == nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "请求体必须是 JSON 对象")
		return
	}
	if len(patch) == 0 {
		view, err := o.loadSettingsView(r)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, settingsResponse{Settings: view})
		return
	}
	updates, err := normalizeSettingsPatch(patch)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := o.Store.UpsertSettings(r.Context(), o.TenantID, updates); err != nil {
		writeStoreError(w, r, err)
		return
	}
	effective, err := effectiveRuntimeSettings(r.Context(), o.Store, o.TenantID, o.SettingsFallback)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	// 持久化成功后再应用：executor/engine/runtime 都是线程安全 setter，
	// 正在运行的任务/调度循环读取的是新值，无需重启。
	o.applyRuntimeSettings(effective)
	writeJSON(w, http.StatusOK, settingsResponse{Settings: effective.view()})
}

// normalizeSettingsPatch 把请求里的扁平键（也接受点号键）规范化为持久化键值。
//
// 支持的键：
//   - max_concurrent / engine.max_concurrent：1..64 的整数
//   - default_action / policy.default_action：allow|ask|deny
//   - approval_timeout / policy.approval_timeout：1..3600 秒（接受 "60s" 或整数秒）
//   - default_budget：{max_tokens>=0, max_steps>=0}
//   - mission.default_max_tokens / mission.default_max_steps：>=0
//
// 未知键、类型错误、越界一律返回中文错误（HTTP 层映射 400）。
func normalizeSettingsPatch(patch map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	// 固定遍历顺序，保证多个非法键时错误信息稳定（便于测试与排障）。
	keys := make([]string, 0, len(patch))
	for key := range patch {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	updates := make(map[string]json.RawMessage, len(patch))
	for _, key := range keys {
		value := patch[key]
		switch key {
		case "max_concurrent", settingKeyMaxConcurrent:
			n, err := parseJSONInt(value)
			if err != nil {
				return nil, fmt.Errorf("max_concurrent 必须是整数")
			}
			if n < 1 || n > 64 {
				return nil, fmt.Errorf("max_concurrent 必须在 1..64 之间")
			}
			updates[settingKeyMaxConcurrent] = mustJSON(n)

		case "default_action", settingKeyDefaultAction:
			var action string
			if err := json.Unmarshal(value, &action); err != nil {
				return nil, fmt.Errorf("default_action 必须是字符串")
			}
			if !policy.Action(action).Valid() {
				return nil, fmt.Errorf("default_action 只能是 allow、ask 或 deny")
			}
			updates[settingKeyDefaultAction] = mustJSON(action)

		case "approval_timeout", settingKeyApprovalTimeout:
			d, err := parseTimeoutValue(value)
			if err != nil {
				return nil, fmt.Errorf("approval_timeout 必须是 1..3600 秒的时长（如 \"60s\"）或整数秒")
			}
			updates[settingKeyApprovalTimeout] = mustJSON(formatTimeout(d))

		case "default_budget":
			budgetUpdates, err := parseBudgetPatch(value)
			if err != nil {
				return nil, err
			}
			for key, value := range budgetUpdates {
				updates[key] = value
			}

		case settingKeyDefaultMaxTokens:
			n, err := parseJSONInt64(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("mission.default_max_tokens 必须是 >=0 的整数")
			}
			updates[settingKeyDefaultMaxTokens] = mustJSON(n)

		case settingKeyDefaultMaxSteps:
			n, err := parseJSONInt(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("mission.default_max_steps 必须是 >=0 的整数")
			}
			updates[settingKeyDefaultMaxSteps] = mustJSON(n)

		default:
			return nil, fmt.Errorf("未知设置键 %q", key)
		}
	}
	return updates, nil
}

// parseBudgetPatch 解析 default_budget 对象，只返回请求里显式给出的字段。
//
// 局部更新语义：只带 max_tokens 时不应把已有的 max_steps 重置为 0。未知字段
// 直接拒绝，避免拼写错误被静默忽略。
func parseBudgetPatch(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var budget struct {
		MaxTokens *int64 `json:"max_tokens"`
		MaxSteps  *int   `json:"max_steps"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&budget); err != nil {
		return nil, fmt.Errorf("default_budget 必须是只含 max_tokens/max_steps 的对象")
	}
	updates := map[string]json.RawMessage{}
	if budget.MaxTokens != nil {
		if *budget.MaxTokens < 0 {
			return nil, fmt.Errorf("default_budget.max_tokens 必须 >=0")
		}
		updates[settingKeyDefaultMaxTokens] = mustJSON(*budget.MaxTokens)
	}
	if budget.MaxSteps != nil {
		if *budget.MaxSteps < 0 {
			return nil, fmt.Errorf("default_budget.max_steps 必须 >=0")
		}
		updates[settingKeyDefaultMaxSteps] = mustJSON(*budget.MaxSteps)
	}
	return updates, nil
}

// parseJSONInt 把 JSON 数字解析为 int（拒绝字符串、小数与超出范围的值）。
func parseJSONInt(raw json.RawMessage) (int, error) {
	n, err := parseJSONInt64(raw)
	if err != nil {
		return 0, err
	}
	if n > int64(^uint(0)>>1) || n < -int64(^uint(0)>>1)-1 {
		return 0, fmt.Errorf("整数超出范围")
	}
	return int(n), nil
}

// parseJSONInt64 把 JSON 数字解析为 int64。
func parseJSONInt64(raw json.RawMessage) (int64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// formatTimeout 把时长序列化成「整秒」字符串（如 "60s" / "90s"），与前端契约
// 的示例形式一致；设置只接受整秒（parseTimeoutValue 会拒绝小数秒）。
func formatTimeout(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(d/time.Second))
}

// parseTimeoutValue 解析审批超时：JSON 字符串按 time.ParseDuration，JSON 数字按秒。
// 合法范围 1..3600 秒。
func parseTimeoutValue(raw json.RawMessage) (time.Duration, error) {
	trimmed := strings.TrimSpace(string(raw))
	var seconds int64
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, err
		}
		d, err := time.ParseDuration(text)
		if err != nil {
			return 0, err
		}
		seconds = int64(d / time.Second)
		if d%time.Second != 0 {
			return 0, fmt.Errorf("不支持不足 1 秒的超时")
		}
	} else {
		n, err := parseJSONInt64(raw)
		if err != nil {
			return 0, err
		}
		seconds = n
	}
	if seconds < 1 || seconds > 3600 {
		return 0, fmt.Errorf("超时必须在 1..3600 秒之间")
	}
	return time.Duration(seconds) * time.Second, nil
}

// mustJSON 把值编码为 JSON；这里只用于已知可编码的标量。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// 标量编码不会失败；保守返回 null 而不是 panic。
		return json.RawMessage("null")
	}
	return json.RawMessage(b)
}

// defaultPolicyAction 读取 policy.default_action（simulate 的默认裁决），
// 读不到或值损坏时回退 ask（默认询问 = 默认安全）。
func (o Options) defaultPolicyAction(r *http.Request) (policy.Action, error) {
	view, err := o.loadSettingsView(r)
	if err != nil {
		return policy.Ask, err
	}
	return policy.Action(view.DefaultAction), nil
}
