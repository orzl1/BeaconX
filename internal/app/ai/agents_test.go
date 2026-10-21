package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

// M6.2-B5 清理项：Agents 定义接入 Planner/Worker 的集成测试。
//
// 依赖真实 PostgreSQL（BEACONX_TEST_PG_DSN）与本地 httptest 假 LLM：
//   - 假 LLM 捕获 OpenAI 兼容请求体（model/messages/tools），用于断言
//     system_prompt、模型覆盖与工具过滤是否真的生效；
//   - 模型覆盖依赖启动配置构造客户端，因此测试用 BEACONX_LLM_* 环境变量把
//     base_url 指向假 LLM（t.Setenv 自动恢复）。

type agentCapturedRequest struct {
	Model    string        `json:"model"`
	Messages []llm.Message `json:"messages"`
	Tools    []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// agentCapture 收集假 LLM 收到的请求。
type agentCapture struct {
	mu   sync.Mutex
	reqs []agentCapturedRequest
}

func (c *agentCapture) add(req agentCapturedRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
}

func (c *agentCapture) all() []agentCapturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]agentCapturedRequest, len(c.reqs))
	copy(out, c.reqs)
	return out
}

func (c *agentCapture) last() (agentCapturedRequest, bool) {
	reqs := c.all()
	if len(reqs) == 0 {
		return agentCapturedRequest{}, false
	}
	return reqs[len(reqs)-1], true
}

// agentLogBuffer 是线程安全的日志缓冲，用于断言 Warn/Info 文案。
type agentLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *agentLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *agentLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// agentFixture 是一次测试所需的 DB、任务与假 LLM。
type agentFixture struct {
	ctx      context.Context
	db       *pg.DB
	store    *pg.Store
	tenantID int64
	mission  pg.Mission
	srv      *httptest.Server
	capture  *agentCapture
	logs     *agentLogBuffer
	log      *slog.Logger
	client   *llm.Client // 默认模型（default-model）的客户端
}

// newAgentFixture 建库/租户/任务与假 LLM；未设置 DSN 时跳过。
func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过 Agents 接入集成测试")
	}
	ctx := context.Background()
	db, err := pg.Open(ctx, pg.Options{DSN: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := migrate.Apply(ctx, db.Pool(), migrations.FS); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}

	tenantID := time.Now().UnixNano()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, fmt.Sprintf("agents-wire-%d", tenantID), "Agents 接入测试租户"); err != nil {
		t.Fatalf("创建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	store := pg.NewStore(db)
	mission, err := store.CreateMission(ctx, tenantID, pg.MissionInput{Name: "Agents 接入", Goal: "确认目标可达"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	capture := &agentCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req agentCapturedRequest
		_ = json.Unmarshal(raw, &req)
		capture.add(req)

		system := ""
		for _, m := range req.Messages {
			if m.Role == "system" {
				system += m.Content
			}
		}
		// 带 tools 或包含执行器提示词的请求按 worker 观察结果回复；其余按规划决策回复。
		isWorker := len(req.Tools) > 0 || strings.Contains(system, "执行器") ||
			strings.Contains(system, "CUSTOM_WORKER")
		content := `{"summary":"ok","steps":[],"actions":[],"done":true,"reason":"完成"}`
		if isWorker {
			content = `{"fact":"完成","detail":"测试观察","done":true}`
		}
		resp := map[string]any{
			"id": "chatcmpl-agent", "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	// 模型覆盖会用启动配置构造「同端点不同模型名」的客户端，这里把配置指向假 LLM。
	t.Setenv("BEACONX_LLM_BASE_URL", srv.URL)
	t.Setenv("BEACONX_LLM_API_KEY", "test-key")
	t.Setenv("BEACONX_LLM_MODEL", "default-model")

	client, err := llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "test-key", Model: "default-model"})
	if err != nil {
		t.Fatalf("创建默认模型客户端失败: %v", err)
	}
	logBuf := &agentLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &agentFixture{
		ctx: ctx, db: db, store: store, tenantID: tenantID, mission: mission,
		srv: srv, capture: capture, logs: logBuf, log: logger, client: client,
	}
}

// createAgent 写入一条 Agent 定义。
func (f *agentFixture) createAgent(t *testing.T, role, key, prompt, model string, tools []string, enabled bool) pg.Agent {
	t.Helper()
	agent, err := f.store.CreateAgent(f.ctx, f.tenantID, pg.AgentInput{
		Key: key, Name: key, Role: role, SystemPrompt: prompt, Model: model,
		Tools: tools, Enabled: enabled,
	})
	if err != nil {
		t.Fatalf("创建 Agent 定义失败: %v", err)
	}
	return agent
}

// missionEvents 返回指定类型的任务事件。
func (f *agentFixture) missionEvents(t *testing.T, typ string) []pg.MissionEvent {
	t.Helper()
	events, err := f.store.ListMissionEvents(f.ctx, f.tenantID, f.mission.ID, 0, 200)
	if err != nil {
		t.Fatalf("读取任务事件失败: %v", err)
	}
	out := make([]pg.MissionEvent, 0, len(events))
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// usageModels 返回任务用量记录里的模型名（按 id 升序）。
func (f *agentFixture) usageModels(t *testing.T) []string {
	t.Helper()
	rows, err := f.db.Pool().Query(f.ctx,
		`SELECT model FROM llm_usage WHERE tenant_id = $1 AND mission_id = $2 ORDER BY id`,
		f.tenantID, f.mission.ID)
	if err != nil {
		t.Fatalf("读取用量失败: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			t.Fatalf("解析用量失败: %v", err)
		}
		out = append(out, model)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历用量失败: %v", err)
	}
	return out
}

// systemOf 返回请求里拼接后的 system 消息内容。
func systemOf(req agentCapturedRequest) string {
	var b strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}

// ---------- Planner ----------

func TestPlannerAgentDefinitionApplied(t *testing.T) {
	f := newAgentFixture(t)
	f.createAgent(t, pg.AgentRolePlanner, "custom-planner", "CUSTOM_PLANNER_PROMPT", "planner-model", nil, true)

	planner := NewPlanner(f.store, f.client, "default-model", Config{}, f.log)
	met, err := planner.Plan(f.ctx, f.mission)
	if err != nil {
		t.Fatalf("Plan 失败: %v", err)
	}
	if !met {
		t.Fatalf("假 LLM 返回 done=true，应视为完成")
	}

	req, ok := f.capture.last()
	if !ok {
		t.Fatal("假 LLM 未收到请求")
	}
	if req.Model != "planner-model" {
		t.Fatalf("请求模型 = %q，期望 planner-model（Agent 定义覆盖默认模型）", req.Model)
	}
	if got := systemOf(req); got != "CUSTOM_PLANNER_PROMPT" {
		t.Fatalf("system 提示词 = %q，期望自定义 system_prompt", got)
	}

	// agent_applied 只记一次（第二次 Plan 命中缓存/同一定义）。
	if _, err := planner.Plan(f.ctx, f.mission); err != nil {
		t.Fatalf("第二次 Plan 失败: %v", err)
	}
	events := f.missionEvents(t, "agent_applied")
	if len(events) != 1 {
		t.Fatalf("agent_applied 事件数 = %d，期望 1（仅首次应用）", len(events))
	}
	if !strings.Contains(events[0].Reason, "role=planner") || !strings.Contains(events[0].Reason, "custom-planner") {
		t.Fatalf("agent_applied 事件内容不符合预期: %+v", events[0])
	}
	if !strings.Contains(f.logs.String(), "agent_applied") {
		t.Fatalf("日志缺少 agent_applied: %s", f.logs.String())
	}

	// 用量记录使用实际生效的模型名。
	models := f.usageModels(t)
	if len(models) == 0 || models[0] != "planner-model" {
		t.Fatalf("llm_usage 模型记录不符合预期: %v", models)
	}
}

func TestPlannerWithoutAgentDefinition(t *testing.T) {
	f := newAgentFixture(t)
	planner := NewPlanner(f.store, f.client, "default-model", Config{}, f.log)
	if _, err := planner.Plan(f.ctx, f.mission); err != nil {
		t.Fatalf("Plan 失败: %v", err)
	}
	req, ok := f.capture.last()
	if !ok {
		t.Fatal("假 LLM 未收到请求")
	}
	if req.Model != "default-model" {
		t.Fatalf("无 Agent 定义时模型 = %q，期望 default-model（行为不变）", req.Model)
	}
	if !strings.Contains(systemOf(req), "规划器") {
		t.Fatalf("无 Agent 定义时应使用内置规划器提示词: %q", systemOf(req))
	}
	if events := f.missionEvents(t, "agent_applied"); len(events) != 0 {
		t.Fatalf("无 Agent 定义不应产生 agent_applied 事件: %+v", events)
	}
}

func TestPlannerDisabledAgentIgnored(t *testing.T) {
	f := newAgentFixture(t)
	f.createAgent(t, pg.AgentRolePlanner, "disabled-planner", "CUSTOM_PLANNER_PROMPT", "planner-model", nil, false)

	planner := NewPlanner(f.store, f.client, "default-model", Config{}, f.log)
	if _, err := planner.Plan(f.ctx, f.mission); err != nil {
		t.Fatalf("Plan 失败: %v", err)
	}
	req, _ := f.capture.last()
	if req.Model != "default-model" || strings.Contains(systemOf(req), "CUSTOM_PLANNER_PROMPT") {
		t.Fatalf("停用的 Agent 不应生效: model=%q system=%q", req.Model, systemOf(req))
	}
}

func TestInvalidateAgents(t *testing.T) {
	f := newAgentFixture(t)
	planner := NewPlanner(f.store, f.client, "default-model", Config{}, f.log)

	if _, err := planner.Plan(f.ctx, f.mission); err != nil {
		t.Fatalf("首次 Plan 失败: %v", err)
	}
	// 首次解析后缓存「无定义」；此时新增定义仍走默认。
	f.createAgent(t, pg.AgentRolePlanner, "late-planner", "CUSTOM_LATE_PROMPT", "late-model", nil, true)
	if _, err := planner.Plan(f.ctx, f.mission); err != nil {
		t.Fatalf("第二次 Plan 失败: %v", err)
	}
	req, _ := f.capture.last()
	if req.Model != "default-model" || strings.Contains(systemOf(req), "CUSTOM_LATE_PROMPT") {
		t.Fatalf("缓存未失效前不应应用新定义: model=%q", req.Model)
	}

	planner.InvalidateAgents()
	if _, err := planner.Plan(f.ctx, f.mission); err != nil {
		t.Fatalf("失效后 Plan 失败: %v", err)
	}
	req, _ = f.capture.last()
	if req.Model != "late-model" || systemOf(req) != "CUSTOM_LATE_PROMPT" {
		t.Fatalf("InvalidateAgents 后应应用新定义: model=%q system=%q", req.Model, systemOf(req))
	}
}

// ---------- Worker ----------

// agentToolRunner 是测试用工具执行器（不真正执行工具）。
type agentToolRunner struct{}

func (agentToolRunner) ExecuteText(context.Context, int64, int64, string, json.RawMessage) (string, error) {
	return "HTTP 200 OK", nil
}

func TestWorkerAgentDefinitionApplied(t *testing.T) {
	f := newAgentFixture(t)
	f.createAgent(t, pg.AgentRoleWorker, "custom-worker", "CUSTOM_WORKER_PROMPT", "worker-model",
		[]string{"http_probe", "unknown_tool"}, true)

	action, err := f.store.AddNode(f.ctx, f.tenantID, f.mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "probe：确认可达",
		Payload: map[string]any{"step_key": "recon", "kind": "probe", "target": "example.com", "detail": "确认可达"},
	})
	if err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}

	toolDefs := []llm.ToolDef{
		{Name: "http_probe", Description: "HTTP 探测", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "shell_exec", Description: "执行命令", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	worker := NewWorker(f.store, f.client, "default-model", f.log).SetTools(agentToolRunner{}, toolDefs)
	state, err := worker.Execute(f.ctx, f.mission, action)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if state != graph.StateDone {
		t.Fatalf("执行状态 = %s，期望 done", state)
	}

	req, ok := f.capture.last()
	if !ok {
		t.Fatal("假 LLM 未收到请求")
	}
	if req.Model != "worker-model" {
		t.Fatalf("请求模型 = %q，期望 worker-model", req.Model)
	}
	if got := systemOf(req); got != "CUSTOM_WORKER_PROMPT" {
		t.Fatalf("worker system 提示词 = %q，期望自定义提示词", got)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "http_probe" {
		t.Fatalf("工具过滤结果不符合预期: %+v", req.Tools)
	}
	logText := f.logs.String()
	if !strings.Contains(logText, "未知工具") || !strings.Contains(logText, "unknown_tool") {
		t.Fatalf("未知工具应记录 Warn: %s", logText)
	}
	if events := f.missionEvents(t, "agent_applied"); len(events) != 1 {
		t.Fatalf("worker agent_applied 事件数 = %d，期望 1", len(events))
	}
	if models := f.usageModels(t); len(models) == 0 || models[0] != "worker-model" {
		t.Fatalf("worker 用量模型记录不符合预期: %v", models)
	}
}

func TestWorkerWithoutAgentDefinition(t *testing.T) {
	f := newAgentFixture(t)
	action, err := f.store.AddNode(f.ctx, f.tenantID, f.mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "probe：确认可达",
		Payload: map[string]any{"step_key": "recon", "kind": "probe", "target": "example.com", "detail": "确认可达"},
	})
	if err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}
	toolDefs := []llm.ToolDef{
		{Name: "http_probe", Description: "HTTP 探测", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "shell_exec", Description: "执行命令", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	worker := NewWorker(f.store, f.client, "default-model", f.log).SetTools(agentToolRunner{}, toolDefs)
	if _, err := worker.Execute(f.ctx, f.mission, action); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	req, _ := f.capture.last()
	if req.Model != "default-model" {
		t.Fatalf("无 Agent 定义时模型 = %q，期望 default-model", req.Model)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("无 Agent 定义时应暴露全部工具，实际 %+v", req.Tools)
	}
	if !strings.Contains(systemOf(req), "执行器") {
		t.Fatalf("无 Agent 定义时应使用内置执行器提示词: %q", systemOf(req))
	}
	if events := f.missionEvents(t, "agent_applied"); len(events) != 0 {
		t.Fatalf("无 Agent 定义不应产生 agent_applied 事件: %+v", events)
	}
}

// ---------- 失败回退 ----------

func TestAgentResolverDBErrorFallback(t *testing.T) {
	f := newAgentFixture(t)
	// 关闭连接池，模拟 DB 故障：解析必须回退默认且不 panic。
	f.db.Close()

	resolver := newAgentResolver(f.store, f.log)
	if cfg := resolver.resolve(f.ctx, f.mission, agentRolePlanner); cfg != nil {
		t.Fatalf("DB 故障时应回退默认（nil），实际 %+v", cfg)
	}
	if !strings.Contains(f.logs.String(), "回退内置默认") {
		t.Fatalf("DB 故障应记录 Warn: %s", f.logs.String())
	}
}

// TestAgentResolverStoreNilFallback 校验没有任何 Store 时解析器安全返回 nil。
func TestAgentResolverStoreNilFallback(t *testing.T) {
	resolver := newAgentResolver(nil, nil)
	if cfg := resolver.resolve(context.Background(), pg.Mission{TenantID: 1, ID: 1}, agentRolePlanner); cfg != nil {
		t.Fatalf("无 Store 时应返回 nil，实际 %+v", cfg)
	}
}
