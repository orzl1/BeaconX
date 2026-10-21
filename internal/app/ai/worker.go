package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// ToolRunner 是执行工具调用的能力（由 internal/app/executor 提供）。
type ToolRunner interface {
	ExecuteText(ctx context.Context, missionID, nodeID int64, toolName string, args json.RawMessage) (string, error)
}

const workerToolSystem = `你是授权安全测试平台 BeaconX 的执行器。你可以调用提供的工具完成任务，然后给出可复核的结论。
规则：
1. 需要真实环境信息时先调用工具，不要凭空断言；工具返回失败时如实记录。
2. 最多调用 6 轮工具；信息足够后停止调用，直接输出最终结论。
3. 最终结论必须是一个 JSON 对象，不要输出其他文字、不要使用 Markdown 围栏：
{"fact":"一句话结论","detail":"依据、工具结果与限制条件","done":true}
4. 不编造工具输出；确实无法执行时在 detail 中说明“未实际执行”及原因。`

// Worker 用 LLM 执行单条动作，并把观察写成 fact 节点，实现 runner.Worker。
type Worker struct {
	store         *pg.Store
	client        *llm.Client
	model         string
	log           *slog.Logger
	toolRunner    ToolRunner
	toolDefs      []llm.ToolDef
	maxToolRounds int
}

// NewWorker 创建执行器。
func NewWorker(store *pg.Store, client *llm.Client, model string, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{store: store, client: client, model: model, log: log.With("component", "ai-worker"), maxToolRounds: 6}
}

// SetTools 装配工具调用能力；未装配时退化为纯文本观察模式。
func (w *Worker) SetTools(runner ToolRunner, defs []llm.ToolDef) *Worker {
	w.toolRunner = runner
	w.toolDefs = defs
	return w
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

	var final string
	if w.toolRunner != nil && len(w.toolDefs) > 0 {
		messages[0].Content = workerToolSystem
		var err error
		final, err = w.runToolLoop(ctx, mission, action, messages)
		if err != nil {
			return graph.StateBlocked, err
		}
	} else {
		resp, err := w.client.CompleteJSON(ctx, messages)
		if err != nil {
			return graph.StateBlocked, fmt.Errorf("执行器调用模型失败: %w", err)
		}
		if err := w.store.RecordLLMUsage(ctx, mission.TenantID, mission.ID, "worker", w.model, resp.Usage); err != nil {
			w.log.Warn("记录模型用量失败", "mission_id", mission.ID, "error", err)
		}
		final = resp.Content
	}

	obs, err := parseObservation(final)
	if err != nil {
		obs = workerObservation{Fact: truncate(strings.TrimSpace(final), 120), Detail: final, Done: true}
	}
	return w.persistFact(ctx, mission, action, obs, stepKey, kind, target)
}

// runToolLoop 执行「模型 → 工具 → 模型」循环，返回最终文本。
func (w *Worker) runToolLoop(ctx context.Context, mission pg.Mission, action pg.Node, messages []llm.Message) (string, error) {
	if w.maxToolRounds <= 0 {
		w.maxToolRounds = 6
	}
	var final string
	for round := 0; round < w.maxToolRounds; round++ {
		resp, err := w.client.CompleteWithTools(ctx, messages, w.toolDefs, "auto")
		if err != nil {
			return "", fmt.Errorf("执行器调用模型失败: %w", err)
		}
		if err := w.store.RecordLLMUsage(ctx, mission.TenantID, mission.ID, "worker-tool", w.model, resp.Usage); err != nil {
			w.log.Warn("记录模型用量失败", "mission_id", mission.ID, "error", err)
		}
		if len(resp.ToolCalls) == 0 {
			final = resp.Content
			break
		}
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		for _, call := range resp.ToolCalls {
			out, execErr := w.toolRunner.ExecuteText(ctx, mission.ID, action.ID, call.Name, json.RawMessage(call.Arguments))
			content := out
			if execErr != nil {
				content = "工具执行失败：" + execErr.Error()
			}
			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    truncate(content, 4000),
			})
		}
	}
	if strings.TrimSpace(final) == "" {
		final = "工具调用轮次用尽，未给出最终结论"
	}
	return final, nil
}

func (w *Worker) persistFact(ctx context.Context, mission pg.Mission, action pg.Node, obs workerObservation, stepKey, kind, target string) (graph.NodeState, error) {
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
