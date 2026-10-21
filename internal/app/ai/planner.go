package ai

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/plan"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// Planner 用 LLM 生成计划步骤与下一步动作，实现 runner.Planner。
type Planner struct {
	store  *pg.Store
	client LLM
	cfg    Config
	log    *slog.Logger
	model  string
}

// NewPlanner 创建规划器。
func NewPlanner(store *pg.Store, client LLM, model string, cfg Config, log *slog.Logger) *Planner {
	if log == nil {
		log = slog.Default()
	}
	return &Planner{store: store, client: client, cfg: cfg.withDefaults(), log: log.With("component", "ai-planner"), model: model}
}

// Plan 实现 runner.Planner。
func (p *Planner) Plan(ctx context.Context, mission pg.Mission) (bool, error) {
	tenantID, missionID := mission.TenantID, mission.ID

	maxTokens, spent, maxSteps, stepsUsed, err := p.store.MissionBudget(ctx, tenantID, missionID)
	if err != nil {
		return false, err
	}
	if maxTokens > 0 && spent >= maxTokens {
		p.appendEvent(ctx, tenantID, missionID, "budget_exhausted", "模型 token 预算已用尽，结束任务")
		return true, nil
	}

	nodes, err := p.store.ListNodes(ctx, tenantID, missionID, 500)
	if err != nil {
		return false, err
	}
	logNodeSummary(p.log, missionID, nodes)

	steps, err := p.store.ListPlanSteps(ctx, tenantID, missionID)
	if err != nil {
		return false, err
	}
	// 动作结果回写到步骤：done → 步骤完成，blocked/exhausted → 步骤失败。
	changed := false
	for _, n := range nodes {
		if n.Kind != graph.NodeAction {
			continue
		}
		stepKey, _, _, _ := actionPayload(n)
		if stepKey == "" {
			continue
		}
		for _, st := range steps {
			if st.Key != stepKey {
				continue
			}
			switch {
			case n.State == graph.StateDone && st.Status != plan.StepDone:
				if ok, err := p.store.SetPlanStepStatus(ctx, tenantID, missionID, stepKey, plan.StepDone); err == nil && ok {
					changed = true
				}
			case (n.State == graph.StateBlocked || n.State == graph.StateExhausted) && st.Status != plan.StepFailed:
				if ok, err := p.store.SetPlanStepStatus(ctx, tenantID, missionID, stepKey, plan.StepFailed); err == nil && ok {
					changed = true
				}
			}
		}
	}
	if changed {
		if steps, err = p.store.ListPlanSteps(ctx, tenantID, missionID); err != nil {
			return false, err
		}
	}

	// 有动作正在进行时不规划，避免每轮重复消耗 token。
	openActions := 0
	for _, n := range nodes {
		if n.Kind == graph.NodeAction && (n.State == graph.StateOpen || n.State == graph.StateRunning) {
			openActions++
		}
	}
	if openActions > 0 {
		return false, nil
	}

	if maxSteps > 0 && stepsUsed >= maxSteps {
		p.appendEvent(ctx, tenantID, missionID, "step_budget_exhausted", "步骤预算已用尽，结束任务")
		return true, nil
	}

	// 汇总最近事实。
	facts := make([]pg.Node, 0, 10)
	for _, n := range nodes {
		if n.Kind == graph.NodeFact && n.State == graph.StateDone {
			facts = append(facts, n)
		}
	}
	if len(facts) > 10 {
		facts = facts[len(facts)-10:]
	}

	snap := snapshot{MissionName: mission.Name, Goal: mission.Goal, Steps: steps, Facts: facts, OpenActions: openActions}
	decision, err := p.decide(ctx, tenantID, missionID, snap)
	if err != nil {
		// 规划失败不阻塞任务；记录事件，等下一轮重试。
		p.appendEvent(ctx, tenantID, missionID, "planner_error", truncate(err.Error(), 500))
		return false, nil
	}

	if err := p.applyDecision(ctx, tenantID, missionID, decision); err != nil {
		return false, nil
	}

	// 模型可能过早宣布完成：只要还有未完成步骤，就追问一次，
	// 避免「只做了第一步就结束」导致攻击链中断。追问仍完成时才接受。
	if decision.Done {
		steps, err = p.store.ListPlanSteps(ctx, tenantID, missionID)
		if err == nil {
			if pending := pendingStepKeys(steps); len(pending) > 0 {
				p.appendEvent(ctx, tenantID, missionID, "planner_done_with_pending",
					"模型宣布完成但仍有未完成步骤："+strings.Join(pending, ","))
				follow, ferr := p.followup(ctx, tenantID, missionID, snap, pending)
				if ferr == nil {
					if err := p.applyDecision(ctx, tenantID, missionID, follow); err != nil {
						p.log.Warn("追问决策落库失败", "mission_id", missionID, "error", err)
					} else {
						decision = follow
					}
				}
			}
		}
	}

	if decision.Done {
		// 接受完成时把仍未完成的步骤标为 skipped，避免计划里留下永远 pending 的悬空步骤。
		if steps, err := p.store.ListPlanSteps(ctx, tenantID, missionID); err == nil {
			for _, st := range steps {
				if st.Status.Terminal() {
					continue
				}
				_, _ = p.store.SetPlanStepStatus(ctx, tenantID, missionID, st.Key, plan.StepSkipped)
			}
		}
	}
	return decision.Done, nil
}

// applyDecision 持久化步骤并为依赖已满足的步骤创建动作。
func (p *Planner) applyDecision(ctx context.Context, tenantID, missionID int64, decision plan.Decision) error {
	if len(decision.Steps) > p.cfg.MaxStepsPerPlan {
		decision.Steps = decision.Steps[:p.cfg.MaxStepsPerPlan]
	}
	inserted, err := p.store.UpsertPlanSteps(ctx, tenantID, missionID, decision.Steps)
	if err != nil {
		p.appendEvent(ctx, tenantID, missionID, "planner_persist_error", truncate(err.Error(), 500))
		return err
	}
	if inserted > 0 {
		_ = p.store.IncrementMissionSteps(ctx, tenantID, missionID, inserted)
	}

	// 只给「依赖已满足」的步骤创建动作；未就绪的动作等后续轮次。
	ready, err := p.store.ReadyPlanSteps(ctx, tenantID, missionID)
	if err != nil {
		return err
	}
	readySet := make(map[string]bool, len(ready))
	for _, st := range ready {
		readySet[st.Key] = true
	}
	created := 0
	for _, action := range decision.Actions {
		if created >= p.cfg.MaxActionsPerRound {
			break
		}
		if !readySet[action.StepKey] {
			continue
		}
		title := fmt.Sprintf("%s：%s", action.Kind, truncate(action.Detail, 80))
		if strings.TrimSpace(action.Detail) == "" {
			title = action.Kind + "：" + action.Target
		}
		payload := map[string]any{
			"step_key": action.StepKey,
			"kind":     action.Kind,
			"target":   action.Target,
			"detail":   action.Detail,
		}
		if _, err := p.store.AddNode(ctx, tenantID, missionID, pg.NodeInput{
			Kind:    graph.NodeAction,
			State:   graph.StateOpen,
			Title:   title,
			Payload: payload,
		}); err != nil {
			p.log.Warn("创建动作失败", "mission_id", missionID, "error", err)
			continue
		}
		created++
	}
	return nil
}

// followup 在模型过早完成时追问下一步动作。
func (p *Planner) followup(ctx context.Context, tenantID, missionID int64, snap snapshot, pending []string) (plan.Decision, error) {
	messages := []llm.Message{
		{Role: "system", Content: plannerSystemNext},
		{Role: "user", Content: renderSnapshot(snap)},
		{Role: "user", Content: "仍有未完成步骤：" + strings.Join(pending, ",") +
			"。请为依赖已满足的步骤生成动作；若确实无法推进，请在 reason 中说明受阻原因，不要直接返回空动作且 done=true。"},
	}
	resp, err := p.client.CompleteJSON(ctx, messages)
	if err != nil {
		return plan.Decision{}, err
	}
	p.recordUsage(ctx, tenantID, missionID, "planner-followup", resp)
	return plan.ParseDecisionWithKnown(resp.Content, knownStepSet(snap.Steps))
}

// knownStepSet 把当前计划步骤转成校验用的「已知 key」集合。
func knownStepSet(steps []pg.PlanStep) map[string]bool {
	out := make(map[string]bool, len(steps))
	for _, st := range steps {
		out[st.Key] = true
	}
	return out
}

// pendingStepKeys 返回仍未完成的步骤 key。
func pendingStepKeys(steps []pg.PlanStep) []string {
	var out []string
	for _, st := range steps {
		if !st.Status.Terminal() {
			out = append(out, st.Key)
		}
	}
	return out
}

// decide 调用模型并解析决策；解析失败会重试一次。
func (p *Planner) decide(ctx context.Context, tenantID, missionID int64, snap snapshot) (plan.Decision, error) {
	system := plannerSystemNext
	if len(snap.Steps) == 0 {
		system = plannerSystemDecompose
	}
	messages := []llm.Message{
		{Role: "system", Content: system},
		{Role: "user", Content: renderSnapshot(snap)},
	}
	resp, err := p.client.CompleteJSON(ctx, messages)
	if err != nil {
		return plan.Decision{}, err
	}
	p.recordUsage(ctx, tenantID, missionID, "planner", resp)

	known := knownStepSet(snap.Steps)
	decision, err := plan.ParseDecisionWithKnown(resp.Content, known)
	if err == nil {
		return decision, nil
	}
	p.log.Warn("规划输出解析失败，重试一次", "mission_id", missionID, "error", err)

	retry := append(messages,
		llm.Message{Role: "assistant", Content: resp.Content},
		llm.Message{Role: "user", Content: "上一次输出无法解析：" + err.Error() + "。请只输出一个符合规定结构的 JSON 对象，不要解释、不要围栏。"},
	)
	resp2, err2 := p.client.CompleteJSON(ctx, retry)
	if err2 != nil {
		return plan.Decision{}, err2
	}
	p.recordUsage(ctx, tenantID, missionID, "planner-retry", resp2)
	return plan.ParseDecisionWithKnown(resp2.Content, known)
}

func (p *Planner) recordUsage(ctx context.Context, tenantID, missionID int64, role string, resp llm.Response) {
	if err := p.store.RecordLLMUsage(ctx, tenantID, missionID, role, p.model, resp.Usage); err != nil {
		p.log.Warn("记录模型用量失败", "mission_id", missionID, "error", err)
	}
}

func (p *Planner) appendEvent(ctx context.Context, tenantID, missionID int64, typ, reason string) {
	if _, err := p.store.AppendMissionEvent(ctx, tenantID, missionID, task.EventType(typ), "system", reason, nil); err != nil {
		p.log.Warn("写入任务事件失败", "mission_id", missionID, "type", typ, "error", err)
	}
}
