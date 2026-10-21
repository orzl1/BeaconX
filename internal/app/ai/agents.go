package ai

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/orzl1/BeaconX/internal/config"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// Agent 定义接入（M6.2-B5 清理项）。
//
// agents 表里的 role=planner / role=worker 定义会按任务解析并覆盖内置默认：
//   - system_prompt：替换内置 system 提示词（为空则保持内置）；
//   - model：覆盖默认模型名（默认由 NewPlanner/NewWorker 的 model 参数给出）；
//   - tools：仅 worker 使用，只把定义里列出的工具暴露给模型；未注册的工具忽略并
//     Warn（定义可以先于工具注册表存在）。
//
// 约束与取舍：
//   - 每任务每角色只查一次库并缓存；`InvalidateAgents()` 可清空缓存（测试/热更新）；
//   - 查询失败/DB 错误不阻塞任务：记录 Warn 并回退内置默认（避免任务卡死）；
//   - model 覆盖需要能构造对应模型的 LLM：优先使用 LLM 的可选 `WithModel` 能力；
//     否则用启动配置（BEACONX_LLM_* / config.json）构造一个同端点、不同模型名的
//     客户端并缓存。若两者都不可用，记录 Warn 并回退默认模型（请求与用量都按
//     实际使用的模型记录，避免账实不符）；
//   - 首次应用一条定义时记录日志与 mission 事件 `agent_applied`（含 role/key），
//     便于审计与测试断言。

const (
	agentRolePlanner = "planner"
	agentRoleWorker  = "worker"
)

// agentConfig 是解析后的启用 Agent 定义；nil 表示沿用内置默认。
type agentConfig struct {
	Key          string
	SystemPrompt string
	Model        string
	Tools        []string
}

// ModelSwitcher 是 LLM 的可选能力：按模型名派生一个新的 LLM 实例（不改原实例）。
// 测试替身或未来的多模型客户端可以实现它；*llm.Client / *llm.Router 目前不实现，
// 因此生产上会走「按启动配置构造同端点客户端」的兜底方案。
type ModelSwitcher interface {
	WithModel(model string) LLM
}

// agentCacheKey 是缓存键：mission 内按角色缓存（mission id 全局唯一，仍带租户防误用）。
type agentCacheKey struct {
	tenantID  int64
	missionID int64
	role      string
}

// agentResolver 负责按任务解析启用 Agent 定义并缓存。
type agentResolver struct {
	store *pg.Store
	log   *slog.Logger

	mu           sync.Mutex
	loaded       map[agentCacheKey]bool
	cache        map[agentCacheKey]*agentConfig
	applied      map[agentCacheKey]bool
	modelClients map[string]LLM
}

// newAgentResolver 创建解析器；store 为空时永远回退默认（便于单元测试）。
func newAgentResolver(store *pg.Store, log *slog.Logger) *agentResolver {
	if log == nil {
		log = slog.Default()
	}
	return &agentResolver{
		store:        store,
		log:          log.With("component", "ai-agents"),
		loaded:       map[agentCacheKey]bool{},
		cache:        map[agentCacheKey]*agentConfig{},
		applied:      map[agentCacheKey]bool{},
		modelClients: map[string]LLM{},
	}
}

// invalidate 清空任务/模型缓存（下次调用重新查库）。
func (r *agentResolver) invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaded = map[agentCacheKey]bool{}
	r.cache = map[agentCacheKey]*agentConfig{}
	r.applied = map[agentCacheKey]bool{}
	// modelClients 保留：它只依赖模型名与启动配置，清空没有收益（且配置变更不在
	// 本批处理范围内）。需要时可通过重建 Planner/Worker 完全重置。
}

// resolve 返回任务在指定角色下启用的第一个 Agent（按 id 升序）；没有则 nil。
//
// 查库失败时记录 Warn 并缓存「无定义」结果，避免每轮重复打 DB 与刷日志；
// 需要恢复时调用 InvalidateAgents()。
func (r *agentResolver) resolve(ctx context.Context, mission pg.Mission, role string) *agentConfig {
	if r == nil || r.store == nil {
		return nil
	}
	key := agentCacheKey{tenantID: mission.TenantID, missionID: mission.ID, role: role}

	r.mu.Lock()
	if r.loaded[key] {
		cfg := r.cache[key]
		r.mu.Unlock()
		return cfg
	}
	r.mu.Unlock()

	enabled := true
	agents, err := r.store.ListAgents(ctx, mission.TenantID, pg.AgentFilter{
		Role: role, Enabled: &enabled, Limit: 1,
	})
	var cfg *agentConfig
	switch {
	case err != nil:
		r.log.Warn("解析 Agent 定义失败，回退内置默认",
			"role", role, "mission_id", mission.ID, "error", err)
	case len(agents) > 0:
		agent := agents[0]
		cfg = &agentConfig{
			Key:          agent.Key,
			SystemPrompt: agent.SystemPrompt,
			Model:        strings.TrimSpace(agent.Model),
			Tools:        append([]string(nil), agent.Tools...),
		}
	}

	r.mu.Lock()
	r.loaded[key] = true
	r.cache[key] = cfg
	alreadyApplied := r.applied[key]
	if cfg != nil {
		r.applied[key] = true
	}
	r.mu.Unlock()

	if cfg != nil && !alreadyApplied {
		r.markApplied(ctx, mission, role, cfg.Key)
	}
	return cfg
}

// markApplied 记录「本任务首次应用某个 Agent 定义」：日志 + mission 事件。
func (r *agentResolver) markApplied(ctx context.Context, mission pg.Mission, role, key string) {
	r.log.Info("agent_applied",
		"role", role, "key", key, "mission_id", mission.ID)
	reason := "role=" + role + " key=" + key
	if _, err := r.store.AppendMissionEvent(ctx, mission.TenantID, mission.ID,
		task.EventType("agent_applied"), "system", reason,
		map[string]any{"role": role, "key": key}); err != nil {
		r.log.Warn("写入 agent_applied 事件失败",
			"role", role, "key", key, "mission_id", mission.ID, "error", err)
	}
}

// clientForModel 返回用于指定模型的 LLM：优先用原实例的 WithModel，其次按启动配置
// 构造同端点客户端。ok=false 表示无法切换（调用方应回退默认模型并 Warn）。
func (r *agentResolver) clientForModel(base LLM, model string) (LLM, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return base, false
	}
	r.mu.Lock()
	if cached, ok := r.modelClients[model]; ok {
		r.mu.Unlock()
		return cached, cached != nil
	}
	r.mu.Unlock()

	if switcher, ok := base.(ModelSwitcher); ok {
		if derived := switcher.WithModel(model); derived != nil {
			r.mu.Lock()
			r.modelClients[model] = derived
			r.mu.Unlock()
			return derived, true
		}
	}

	client, err := buildModelClient(model)
	if err != nil {
		r.log.Warn("无法为 Agent 指定模型构造 LLM，回退默认模型",
			"model", model, "error", err)
		r.mu.Lock()
		r.modelClients[model] = nil
		r.mu.Unlock()
		return base, false
	}
	r.mu.Lock()
	r.modelClients[model] = client
	r.mu.Unlock()
	return client, true
}

// buildModelClient 用启动配置（BEACONX_LLM_* / config.json）构造指定模型的客户端。
//
// 只复用端点与鉴权参数，不读取任务/设置级配置：模型覆盖是「同一端点换模型名」，
// 换端点/换 provider 属于 M5 模型路由的职责。
func buildModelClient(model string) (LLM, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.LLM.BaseURL) == "" {
		return nil, errNoLLMBaseURL
	}
	return llm.NewClient(llm.Config{
		BaseURL:     cfg.LLM.BaseURL,
		APIKey:      cfg.LLM.APIKey,
		Model:       model,
		MaxTokens:   cfg.LLM.MaxTokens,
		Temperature: cfg.LLM.Temperature,
		Timeout:     cfg.LLM.Timeout.Value(),
		MaxRetries:  cfg.LLM.MaxRetries,
	})
}

// errNoLLMBaseURL 表示启动配置里没有 LLM 端点，无法按模型名派生客户端。
var errNoLLMBaseURL = errors.New("启动配置未提供 LLM base_url，无法切换模型")

// plannerAgentRun 是一次规划运行使用的 Agent 配置（已解析并合并默认）。
type plannerAgentRun struct {
	client LLM
	model  string
	system string // 非空表示用自定义 system_prompt 替换内置提示词
}

// workerAgentRun 是一次执行运行使用的 Agent 配置。
type workerAgentRun struct {
	client  LLM
	model   string
	system  string
	toolDef []llm.ToolDef
}

// filterToolDefs 按 Agent 定义过滤可用工具，并记录定义里未注册的工具。
//
// 定义可以包含未来版本才有的工具名：这里只忽略并 Warn，不阻塞执行。
func filterToolDefs(defs []llm.ToolDef, allowed []string, log *slog.Logger, missionID int64) []llm.ToolDef {
	if log == nil {
		log = slog.Default()
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			allowedSet[trimmed] = true
		}
	}
	available := make(map[string]bool, len(defs))
	out := make([]llm.ToolDef, 0, len(defs))
	for _, def := range defs {
		available[def.Name] = true
		if allowedSet[def.Name] {
			out = append(out, def)
		}
	}
	for name := range allowedSet {
		if !available[name] {
			log.Warn("Agent 定义包含未知工具，已忽略（未注册到工具注册表）",
				"tool", name, "mission_id", missionID)
		}
	}
	return out
}
