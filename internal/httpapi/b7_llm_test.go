package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/ai"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B7：LLM 请求/响应正文采集与查看接口的端到端测试。
//
// 用假 LLM 驱动真实的 ai.Planner / ai.Worker（不联网），验证：
//   - 每次调用都落一条 llm_exchanges（planner / worker / worker-tool）；
//   - 请求里的敏感键（tool_calls.arguments.password）与响应里的密钥文本被脱敏；
//   - 超过 64KB 的正文被 UTF-8 安全截断并标记 truncated；
//   - BEACONX_LLM_CAPTURE=false 时不写入；
//   - 详情/请求/响应三个端点的 200/404/401/400/跨租户语义。

// b7FakeLLM 是按调用顺序返回预设结果的假 LLM。
type b7FakeLLM struct {
	jsonResponses []llm.Response
	toolResponses []llm.ToolResponse
	failWith      error
	jsonCalls     int
	toolCalls     int
}

// CompleteJSON 实现 ai.LLM。
func (f *b7FakeLLM) CompleteJSON(_ context.Context, _ []llm.Message) (llm.Response, error) {
	f.jsonCalls++
	if f.failWith != nil {
		return llm.Response{}, f.failWith
	}
	if index := f.jsonCalls - 1; index < len(f.jsonResponses) {
		return f.jsonResponses[index], nil
	}
	return llm.Response{Content: `{"fact":"默认结论","detail":"默认细节","done":true}`}, nil
}

// CompleteWithTools 实现 ai.LLM。
func (f *b7FakeLLM) CompleteWithTools(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ string) (llm.ToolResponse, error) {
	f.toolCalls++
	if f.failWith != nil {
		return llm.ToolResponse{}, f.failWith
	}
	if index := f.toolCalls - 1; index < len(f.toolResponses) {
		return f.toolResponses[index], nil
	}
	return llm.ToolResponse{Content: `{"fact":"工具轮结束","detail":"ok","done":true}`}, nil
}

// b7FakeToolRunner 是固定的工具执行结果。
type b7FakeToolRunner struct{}

// ExecuteText 实现 ai.ToolRunner。
func (b7FakeToolRunner) ExecuteText(context.Context, int64, int64, string, json.RawMessage) (string, error) {
	return "uid=0(root)", nil
}

// b7CountExchanges 统计任务下的调用记录数。
func b7CountExchanges(t *testing.T, h *insightsHarness, missionID int64) int {
	t.Helper()
	var count int
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM llm_exchanges WHERE tenant_id = $1 AND mission_id = $2`,
		h.tenantA, missionID).Scan(&count); err != nil {
		t.Fatalf("统计 LLM 调用记录失败: %v", err)
	}
	return count
}

// b7ExchangeIDs 按角色列出任务下的调用记录 id（升序）。
func b7ExchangeIDs(t *testing.T, h *insightsHarness, missionID int64, role string) []int64 {
	t.Helper()
	rows, err := h.db.Pool().Query(t.Context(),
		`SELECT id FROM llm_exchanges WHERE tenant_id = $1 AND mission_id = $2 AND role = $3 ORDER BY id`,
		h.tenantA, missionID, role)
	if err != nil {
		t.Fatalf("查询调用记录失败: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描调用记录失败: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// b7Get 读取原始响应（用于验证 Content-Type 与纯文本内容）。
func (h *insightsHarness) b7Get(t *testing.T, path string) (int, []byte, http.Header) {
	t.Helper()
	return h.do(t, http.MethodGet, path, testAPIToken, "")
}

// TestB7LLMCaptureEndToEnd 验证 planner/worker 调用后正文被采集，并可通过 API 查看。
func TestB7LLMCaptureEndToEnd(t *testing.T) {
	h := newInsightsHarness(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("BEACONX_LLM_CAPTURE", "true")

	// planner 产出一步一动作；worker 再给出观察结论。
	fake := &b7FakeLLM{jsonResponses: []llm.Response{
		{Content: `{"summary":"拆解","steps":[{"key":"recon","title":"侦察","detail":"确认目标可达"}],"actions":[{"step_key":"recon","kind":"probe","target":"example.com","detail":"检查首页"}],"done":false,"reason":""}`, Usage: llm.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}},
		{Content: `{"fact":"完成","detail":"首页可访问","done":true}`, Usage: llm.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}},
	}}
	mission := h.createMission(t, "B7 采集任务")
	planner := ai.NewPlanner(h.store, fake, "fake-model", ai.Config{}, logger)
	if _, err := planner.Plan(t.Context(), mission); err != nil {
		t.Fatalf("规划失败: %v", err)
	}

	// 找到 planner 创建的动作节点。
	nodes, err := h.store.ListNodes(t.Context(), h.tenantA, mission.ID, 100)
	if err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	var action *pg.Node
	for i := range nodes {
		if nodes[i].Kind == graph.NodeAction {
			action = &nodes[i]
			break
		}
	}
	if action == nil {
		t.Fatal("planner 未创建动作节点")
	}
	worker := ai.NewWorker(h.store, fake, "fake-model", logger)
	if _, err := worker.Execute(t.Context(), mission, *action); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if got := b7CountExchanges(t, h, mission.ID); got < 2 {
		t.Fatalf("调用记录数 = %d，期望 ≥2", got)
	}
	plannerIDs := b7ExchangeIDs(t, h, mission.ID, "planner")
	workerIDs := b7ExchangeIDs(t, h, mission.ID, "worker")
	if len(plannerIDs) == 0 || len(workerIDs) == 0 {
		t.Fatalf("缺少 planner/worker 记录: planner=%v worker=%v", plannerIDs, workerIDs)
	}

	t.Run("详情不含正文", func(t *testing.T) {
		raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/llm-records/%d", plannerIDs[0]), "", http.StatusOK)
		var detail struct {
			ID           int64  `json:"id"`
			MissionID    int64  `json:"mission_id"`
			Role         string `json:"role"`
			Model        string `json:"model"`
			PromptTokens int    `json:"prompt_tokens"`
			TotalTokens  int    `json:"total_tokens"`
			RequestBytes int    `json:"request_bytes"`
			ResponseSize int    `json:"response_bytes"`
			Redacted     bool   `json:"redacted"`
		}
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatalf("解析详情失败: %v（%s）", err, raw)
		}
		if detail.ID != plannerIDs[0] || detail.MissionID != mission.ID || detail.Role != "planner" {
			t.Fatalf("详情字段异常: %+v", detail)
		}
		if detail.Model != "fake-model" || detail.PromptTokens != 11 || detail.TotalTokens != 18 {
			t.Fatalf("模型/用量字段异常: %+v", detail)
		}
		if detail.RequestBytes <= 0 || detail.ResponseSize <= 0 || !detail.Redacted {
			t.Fatalf("字节数/脱敏标记异常: %+v", detail)
		}
		// 详情接口不应带出 messages 正文。
		if strings.Contains(string(raw), "messages") {
			t.Fatalf("详情泄漏正文: %s", raw)
		}
	})

	t.Run("请求正文可读且为 JSON", func(t *testing.T) {
		status, raw, header := h.b7Get(t, fmt.Sprintf("/api/v1/llm-records/%d/request", plannerIDs[0]))
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，响应 %s", status, raw)
		}
		if ct := header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("Content-Type = %q", ct)
		}
		var doc struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("请求正文不是合法 JSON: %v（%s）", err, raw)
		}
		if len(doc.Messages) == 0 || doc.Messages[0].Role != "system" {
			t.Fatalf("messages 结构异常: %+v", doc.Messages)
		}
		if !strings.Contains(doc.Messages[1].Content, "B7 采集任务") {
			t.Fatalf("请求正文缺少任务上下文: %q", doc.Messages[1].Content)
		}
	})

	t.Run("响应正文为纯文本", func(t *testing.T) {
		status, raw, header := h.b7Get(t, fmt.Sprintf("/api/v1/llm-records/%d/response", workerIDs[0]))
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d", status)
		}
		if ct := header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Fatalf("Content-Type = %q", ct)
		}
		if !strings.Contains(string(raw), "首页可访问") {
			t.Fatalf("响应正文不符合预期: %s", raw)
		}
	})

	t.Run("既有列表接口不受影响", func(t *testing.T) {
		status, raw, _ := h.b7Get(t, "/api/v1/llm-records")
		if status != http.StatusOK {
			t.Fatalf("列表状态码 = %d，响应 %s", status, raw)
		}
		if !strings.Contains(string(raw), `"records"`) {
			t.Fatalf("列表响应缺少 records: %s", raw)
		}
	})
}

// TestB7LLMCaptureToolArgsRedacted 验证工具调用参数里的密码被结构脱敏。
func TestB7LLMCaptureToolArgsRedacted(t *testing.T) {
	h := newInsightsHarness(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("BEACONX_LLM_CAPTURE", "true")

	fake := &b7FakeLLM{toolResponses: []llm.ToolResponse{
		{ToolCalls: []llm.ToolCall{{
			ID:        "call-1",
			Name:      "shell_exec",
			Arguments: `{"cmd":"id","password":"hunter2"}`,
		}}, Usage: llm.Usage{TotalTokens: 6}},
		{Content: `{"fact":"完成","detail":"拿到 root","done":true}`, Usage: llm.Usage{TotalTokens: 4}},
	}}
	mission := h.createMission(t, "B7 工具脱敏任务")
	action, err := h.store.AddNode(t.Context(), h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "动作",
		Payload: map[string]any{"step_key": "recon", "kind": "probe", "target": "example.com"},
	})
	if err != nil {
		t.Fatalf("创建动作节点失败: %v", err)
	}
	worker := ai.NewWorker(h.store, fake, "fake-model", logger).
		SetTools(b7FakeToolRunner{}, []llm.ToolDef{{Name: "shell_exec", Description: "执行命令"}})
	if _, err := worker.Execute(t.Context(), mission, action); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	ids := b7ExchangeIDs(t, h, mission.ID, "worker-tool")
	if len(ids) < 2 {
		t.Fatalf("worker-tool 记录数 = %d，期望 ≥2", len(ids))
	}

	// 第 0 轮请求里还没有工具调用；工具调用出现在第 0 轮响应与第 1 轮请求里。
	status, raw, header := h.b7Get(t, fmt.Sprintf("/api/v1/llm-records/%d/request", ids[1]))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	text := string(raw)
	if strings.Contains(text, "hunter2") {
		t.Fatalf("工具参数密码未脱敏: %s", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("缺少脱敏占位: %s", text)
	}
	if !strings.Contains(text, "tool_calls") {
		t.Fatalf("请求缺少 tool_calls: %s", text)
	}

	// 第一轮只有工具调用、没有文本：响应端应能看到 tool_calls。
	status, raw, _ = h.b7Get(t, fmt.Sprintf("/api/v1/llm-records/%d/response", ids[0]))
	if status != http.StatusOK {
		t.Fatalf("响应状态码 = %d", status)
	}
	if !strings.Contains(string(raw), "tool_calls") || strings.Contains(string(raw), "hunter2") {
		t.Fatalf("工具调用响应不符合预期: %s", raw)
	}
}

// TestB7LLMCaptureTruncation 验证 64KB 截断与 truncated 标记。
func TestB7LLMCaptureTruncation(t *testing.T) {
	h := newInsightsHarness(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("BEACONX_LLM_CAPTURE", "true")

	huge := strings.Repeat("响", 40000) // 120000 字节
	fake := &b7FakeLLM{jsonResponses: []llm.Response{{Content: huge}}}
	mission := h.createMission(t, "B7 截断任务")
	action, err := h.store.AddNode(t.Context(), h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "动作",
	})
	if err != nil {
		t.Fatalf("创建动作节点失败: %v", err)
	}
	worker := ai.NewWorker(h.store, fake, "fake-model", logger)
	if _, err := worker.Execute(t.Context(), mission, action); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	ids := b7ExchangeIDs(t, h, mission.ID, "worker")
	if len(ids) != 1 {
		t.Fatalf("worker 记录数 = %d", len(ids))
	}
	raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/llm-records/%d", ids[0]), "", http.StatusOK)
	var detail struct {
		Truncated     bool `json:"truncated"`
		ResponseBytes int  `json:"response_bytes"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("解析详情失败: %v", err)
	}
	if !detail.Truncated {
		t.Fatalf("超大响应应标记 truncated: %s", raw)
	}
	if detail.ResponseBytes <= 0 || detail.ResponseBytes > 64*1024+64 {
		t.Fatalf("response_bytes = %d，期望接近 64KB 上限", detail.ResponseBytes)
	}
	status, body, _ := h.b7Get(t, fmt.Sprintf("/api/v1/llm-records/%d/response", ids[0]))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d", status)
	}
	if len(body) >= len(huge) {
		t.Fatalf("响应未截断：%d 字节", len(body))
	}
	if !strings.HasSuffix(string(body), "…(截断)") {
		t.Fatalf("缺少截断标记: %s", string(body[len(body)-20:]))
	}
}

// TestB7LLMCaptureDisabled 验证 BEACONX_LLM_CAPTURE=false 时不写记录。
func TestB7LLMCaptureDisabled(t *testing.T) {
	h := newInsightsHarness(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("BEACONX_LLM_CAPTURE", "false")

	fake := &b7FakeLLM{jsonResponses: []llm.Response{{
		Content: `{"summary":"拆解","steps":[{"key":"recon","title":"侦察","detail":"d"}],"actions":[],"done":false,"reason":""}`,
	}}}
	mission := h.createMission(t, "B7 关闭采集任务")
	planner := ai.NewPlanner(h.store, fake, "fake-model", ai.Config{}, logger)
	if _, err := planner.Plan(t.Context(), mission); err != nil {
		t.Fatalf("规划失败: %v", err)
	}
	if got := b7CountExchanges(t, h, mission.ID); got != 0 {
		t.Fatalf("采集关闭时仍写入 %d 条记录", got)
	}
}

// TestB7LLMCaptureAPIErrors 覆盖 404/400/401/跨租户。
func TestB7LLMCaptureAPIErrors(t *testing.T) {
	h := newInsightsHarness(t)
	t.Setenv("BEACONX_LLM_CAPTURE", "true")

	// 造一条本租户记录，供「正常读取」对照。
	mission := h.createMission(t, "B7 错误路径任务")
	id, err := h.store.RecordLLMExchange(t.Context(), h.tenantA, pg.LLMExchangeInput{
		MissionID: mission.ID, Role: "planner", Model: "fake-model",
		Request:  []byte(`{"messages":[{"role":"system","content":"x"}]}`),
		Response: "ok", RequestBytes: 38, ResponseBytes: 2, Redacted: true,
	})
	if err != nil {
		t.Fatalf("写入调用记录失败: %v", err)
	}

	t.Run("不存在与非法标识", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/llm-records/999999999",
			"/api/v1/llm-records/999999999/request",
			"/api/v1/llm-records/999999999/response",
		} {
			h.b4(t, http.MethodGet, path, "", http.StatusNotFound)
		}
		h.b4(t, http.MethodGet, "/api/v1/llm-records/abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/llm-records/abc/request", "", http.StatusBadRequest)
	})

	t.Run("未认证", func(t *testing.T) {
		status, _, _ := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/llm-records/%d", id), "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401", status)
		}
	})

	t.Run("跨租户 404", func(t *testing.T) {
		foreignMission, err := h.store.CreateMission(t.Context(), h.tenantB, pg.MissionInput{Name: "他租户任务"})
		if err != nil {
			t.Fatalf("创建他租户任务失败: %v", err)
		}
		foreignID, err := h.store.RecordLLMExchange(t.Context(), h.tenantB, pg.LLMExchangeInput{
			MissionID: foreignMission.ID, Role: "planner", Model: "m",
			Request: []byte(`{"messages":[]}`), Redacted: true,
		})
		if err != nil {
			t.Fatalf("写入他租户调用记录失败: %v", err)
		}
		for _, path := range []string{
			fmt.Sprintf("/api/v1/llm-records/%d", foreignID),
			fmt.Sprintf("/api/v1/llm-records/%d/request", foreignID),
			fmt.Sprintf("/api/v1/llm-records/%d/response", foreignID),
		} {
			h.b4(t, http.MethodGet, path, "", http.StatusNotFound)
		}
		// 本租户记录仍可读。
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/llm-records/%d", id), "", http.StatusOK)
	})

	_ = time.Now
}
