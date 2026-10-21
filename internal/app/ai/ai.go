// Package ai 用真实 LLM 实现 M2 定义的两个运行时接口：
//
//   - Planner：读取任务态势，产出结构化计划（plan.Step）与下一步动作；
//   - Worker：执行单条动作，把观察写成 fact 节点。
//
// M3 阶段还没有真实工具（M4 接入），因此执行器的“观察”是模型基于上下文给出
// 的可复核结论；接入工具后只需替换 Execute 的实现，计划与引擎不变。
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

// LLM 是规划/执行所需的模型能力；*llm.Client 与 llm.Router 都实现它。
type LLM interface {
	CompleteJSON(ctx context.Context, messages []llm.Message) (llm.Response, error)
	CompleteWithTools(ctx context.Context, messages []llm.Message, tools []llm.ToolDef, toolChoice string) (llm.ToolResponse, error)
}

// Config 是规划/执行行为参数。
type Config struct {
	// MaxStepsPerPlan 单次规划允许新增的步骤上限。
	MaxStepsPerPlan int
	// MaxActionsPerRound 每轮规划最多创建的动作数。
	MaxActionsPerRound int
}

func (c Config) withDefaults() Config {
	if c.MaxStepsPerPlan <= 0 {
		c.MaxStepsPerPlan = 8
	}
	if c.MaxActionsPerRound <= 0 {
		c.MaxActionsPerRound = 3
	}
	return c
}

// snapshot 是一次规划请求看到的任务态势。
type snapshot struct {
	MissionName string
	Goal        string
	Steps       []pg.PlanStep
	Facts       []pg.Node
	OpenActions int
}

// renderSnapshot 把态势渲染成提示词里的用户消息。
func renderSnapshot(s snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "任务：%s\n目标：%s\n", s.MissionName, s.Goal)
	if len(s.Steps) == 0 {
		b.WriteString("当前计划：尚未拆分步骤。\n")
	} else {
		b.WriteString("当前计划：\n")
		for _, st := range s.Steps {
			deps := "无"
			if len(st.DependsOn) > 0 {
				deps = strings.Join(st.DependsOn, ",")
			}
			fmt.Fprintf(&b, "- [%s] %s（状态=%s，依赖=%s）%s\n", st.Key, st.Title, st.Status, deps, st.Detail)
		}
	}
	if len(s.Facts) == 0 {
		b.WriteString("已确认事实：暂无。\n")
	} else {
		b.WriteString("已确认事实：\n")
		for i, f := range s.Facts {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "- %s：%s\n", f.Title, truncate(f.Body, 300))
		}
	}
	fmt.Fprintf(&b, "进行中的动作数：%d\n", s.OpenActions)
	return b.String()
}

const plannerSystemDecompose = `你是授权安全测试平台 BeaconX 的规划器。当前任务已有目标与授权范围。
请把目标拆成 3–8 个可验证的步骤，并为「不依赖其他步骤」的步骤给出最多 3 个动作。
只输出一个 JSON 对象，不要输出任何其他文字、不要使用 Markdown 围栏：
{"summary":"本轮判断","steps":[{"key":"recon","title":"步骤标题","detail":"验收标准","depends_on":[]}],"actions":[{"step_key":"recon","kind":"probe","target":"目标","detail":"要验证什么"}],"done":false,"reason":""}
约束：
- 步骤 key 只用小写字母、数字、下划线、连字符；depends_on 只能引用已有或本次步骤；
- kind 只能是 probe、verify、report、other；
- 只围绕已授权目标，不要臆造未授权目标；
- 目标已经明确完成时返回 done=true 并说明 reason。`

const plannerSystemNext = `你是授权安全测试平台 BeaconX 的规划器。请根据当前计划与已确认事实决定下一步。
只输出一个 JSON 对象，不要输出任何其他文字、不要使用 Markdown 围栏：
{"summary":"本轮判断","steps":[],"actions":[{"step_key":"已有步骤key","kind":"probe","target":"目标","detail":"要验证什么"}],"done":false,"reason":""}
约束：
- 动作的 step_key 必须指向当前计划里已存在且依赖已满足的步骤；
- 每轮最多 3 个动作；没有可推进方向时返回 done=true，并在 reason 里说明是完成还是受阻；
- 不要重复已经完成的步骤。`

const workerSystem = `你是授权安全测试平台 BeaconX 的执行器。你收到一条动作，请在当前上下文下给出可复核的观察结论。
只输出一个 JSON 对象，不要输出任何其他文字、不要使用 Markdown 围栏：
{"fact":"一句话结论","detail":"依据、推理与限制条件","done":true}
约束：不编造工具输出；如果缺少执行条件，在 detail 中明确说明“未实际执行”及原因。`

// workerObservation 是执行器期望的模型输出结构。
type workerObservation struct {
	Fact   string `json:"fact"`
	Detail string `json:"detail"`
	Done   bool   `json:"done"`
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// parseObservation 从模型输出里解析观察 JSON，允许围栏与前后杂质。
func parseObservation(raw string) (workerObservation, error) {
	candidates := []string{raw}
	if start := strings.Index(raw, "{"); start >= 0 {
		if end := strings.LastIndex(raw, "}"); end > start {
			candidates = append([]string{raw[start : end+1]}, candidates...)
		}
	}
	var lastErr error
	for _, c := range candidates {
		var obs workerObservation
		if err := json.Unmarshal([]byte(c), &obs); err == nil && strings.TrimSpace(obs.Fact) != "" {
			return obs, nil
		} else if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("输出不是合法 JSON 对象")
	}
	return workerObservation{}, lastErr
}

// actionPayload 从动作节点 payload 中解析常用字段。
func actionPayload(n pg.Node) (stepKey, kind, target, detail string) {
	if n.Payload == nil {
		return "", "", "", ""
	}
	str := func(key string) string {
		if v, ok := n.Payload[key].(string); ok {
			return v
		}
		return ""
	}
	return str("step_key"), str("kind"), str("target"), str("detail")
}

// logNodeSummary 用于调试输出节点类型与状态。
func logNodeSummary(log *slog.Logger, missionID int64, nodes []pg.Node) {
	count := map[graph.NodeKind]int{}
	for _, n := range nodes {
		count[n.Kind]++
	}
	log.Debug("任务节点概况", "mission_id", missionID, "actions", count[graph.NodeAction], "facts", count[graph.NodeFact])
}

// llmUsage 便于统一记录用量。
func usageFrom(resp llm.Response) llm.Usage { return resp.Usage }
