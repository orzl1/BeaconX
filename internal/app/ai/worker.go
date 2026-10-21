package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// Worker 用 LLM 执行单条动作，并把观察写成 fact 节点，实现 runner.Worker。
type Worker struct {
	store  *pg.Store
	client *llm.Client
	model  string
	log    *slog.Logger
}

// NewWorker 创建执行器。
func NewWorker(store *pg.Store, client *llm.Client, model string, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{store: store, client: client, model: model, log: log.With("component", "ai-worker")}
}

// Execute 实现 runner.Worker。
func (w *Worker) Execute(ctx context.Context, mission pg.Mission, action pg.Node) (graph.NodeState, error) {
	stepKey, kind, target, detail := actionPayload(action)
	user := fmt.Sprintf("任务：%s\n目标：%s\n动作类型：%s\n目标：%s\n动作要求：%s\n",
		mission.Name, mission.Goal, kind, target, detail)
	messages := []llm.Message{
		{Role: "system", Content: workerSystem},
		{Role: "user", Content: user},
	}

	resp, err := w.client.CompleteJSON(ctx, messages)
	if err != nil {
		return graph.StateBlocked, fmt.Errorf("执行器调用模型失败: %w", err)
	}
	if err := w.store.RecordLLMUsage(ctx, mission.TenantID, mission.ID, "worker", w.model, resp.Usage); err != nil {
		w.log.Warn("记录模型用量失败", "mission_id", mission.ID, "error", err)
	}

	obs, err := parseObservation(resp.Content)
	if err != nil {
		// 模型没有按结构输出时，退化为把原始文本存为观察，保证信息不丢。
		obs = workerObservation{Fact: truncate(strings.TrimSpace(resp.Content), 120), Detail: resp.Content, Done: true}
	}
	if strings.TrimSpace(obs.Fact) == "" {
		obs.Fact = "动作观察"
	}
	body := obs.Detail
	if strings.TrimSpace(body) == "" {
		body = obs.Fact
	}
	payload := map[string]any{
		"action_id": action.ID,
		"step_key":  stepKey,
		"kind":      kind,
		"target":    target,
	}
	if _, err := w.store.AddNode(ctx, mission.TenantID, mission.ID, pg.NodeInput{
		Kind:    graph.NodeFact,
		State:   graph.StateDone,
		Title:   truncate(obs.Fact, 120),
		Body:    body,
		Payload: payload,
	}); err != nil {
		return graph.StateBlocked, fmt.Errorf("写入事实节点失败: %w", err)
	}
	return graph.StateDone, nil
}
