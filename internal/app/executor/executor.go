// Package executor 是所有工具调用的唯一入口：先过策略规则，命中 ask 时走人工
// 审批，最后执行工具并写审计。任何 Agent 想调用工具都必须经过这里，避免出现
// “某个 Agent 忘记挂策略检查”的缺口。
package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// Config 是执行器参数。
type Config struct {
	TenantID      int64
	DefaultAction policy.Action
	AskTimeout    time.Duration
	PollInterval  time.Duration
}

func (c Config) withDefaults() Config {
	if !c.DefaultAction.Valid() {
		c.DefaultAction = policy.Ask
	}
	if c.AskTimeout <= 0 {
		c.AskTimeout = 60 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 300 * time.Millisecond
	}
	return c
}

// Executor 执行经策略判定的工具调用。
type Executor struct {
	store    *pg.Store
	registry *tools.Registry
	cfg      Config
	log      *slog.Logger
}

// New 创建执行器。
func New(store *pg.Store, registry *tools.Registry, cfg Config, log *slog.Logger) *Executor {
	if log == nil {
		log = slog.Default()
	}
	return &Executor{store: store, registry: registry, cfg: cfg.withDefaults(), log: log.With("component", "executor")}
}

// Execute 执行一次工具调用。
func (e *Executor) Execute(ctx context.Context, missionID, nodeID int64, toolName string, args json.RawMessage) (tools.Result, error) {
	tool, ok := e.registry.Get(toolName)
	if !ok {
		return tools.Result{}, fmt.Errorf("未知工具 %q", toolName)
	}

	rules, err := e.store.ListPolicyRules(ctx, e.cfg.TenantID)
	if err != nil {
		return tools.Result{}, fmt.Errorf("读取策略规则失败: %w", err)
	}
	decision, err := policy.Evaluate(toolName, args, rules, e.cfg.DefaultAction)
	if err != nil {
		return tools.Result{}, fmt.Errorf("策略判定失败: %w", err)
	}

	switch decision.Action {
	case policy.Deny:
		e.audit(ctx, missionID, nodeID, toolName, args, decision, "denied_by_rule", decision.Reason)
		return tools.Result{}, fmt.Errorf("策略拒绝执行 %s：%s", toolName, decision.Reason)

	case policy.Ask:
		timeout := e.cfg.AskTimeout
		for _, r := range rules {
			if r.Name == decision.RuleName && r.TimeoutSeconds > 0 {
				timeout = time.Duration(r.TimeoutSeconds) * time.Second
			}
		}
		approval, err := e.store.CreateApproval(ctx, e.cfg.TenantID, missionID, nodeID, toolName, args, timeout)
		if err != nil {
			return tools.Result{}, err
		}
		if _, err := e.store.AppendMissionEvent(ctx, e.cfg.TenantID, missionID,
			task.EventType("approval_required"), "system", "工具调用需要人工审批",
			map[string]any{"approval_id": approval.ID, "tool": toolName}); err != nil {
			e.log.Warn("写入审批事件失败", "mission_id", missionID, "error", err)
		}
		approved, reason, err := e.waitApproval(ctx, missionID, nodeID, toolName, approval.ID, timeout)
		if err != nil {
			e.audit(ctx, missionID, nodeID, toolName, args, decision, "approval_unavailable", err.Error())
			return tools.Result{}, err
		}
		if !approved {
			outcome := "rejected_by_user"
			detail := "人工审批未通过"
			if reason == "timeout" {
				outcome = "approval_timeout"
				detail = "审批超时，按拒绝处理"
			}
			e.audit(ctx, missionID, nodeID, toolName, args, decision, outcome, detail)
			if reason == "timeout" {
				return tools.Result{}, fmt.Errorf("工具调用 %s 审批超时", toolName)
			}
			return tools.Result{}, fmt.Errorf("工具调用 %s 未获人工批准", toolName)
		}

	case policy.Allow:
		// 直接执行。
	default:
		return tools.Result{}, fmt.Errorf("未知策略动作 %q", decision.Action)
	}

	res, err := tool.Execute(ctx, args)
	outcome, detail := "executed", ""
	if err != nil {
		outcome, detail = "tool_error", err.Error()
	} else if res.Output != "" {
		detail = truncate(res.Output, 500)
	}
	e.audit(ctx, missionID, nodeID, toolName, args, decision, outcome, detail)
	return res, err
}

// ExecuteText 供 AI Worker 使用：把结果渲染成文本。
func (e *Executor) ExecuteText(ctx context.Context, missionID, nodeID int64, toolName string, args json.RawMessage) (string, error) {
	res, err := e.Execute(ctx, missionID, nodeID, toolName, args)
	if err != nil {
		return "", err
	}
	return res.Output, nil
}

func (e *Executor) waitApproval(ctx context.Context, missionID, nodeID int64, toolName string, approvalID int64, timeout time.Duration) (bool, string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()
	for {
		a, err := e.store.GetApproval(ctx, e.cfg.TenantID, approvalID)
		if err != nil {
			return false, "", err
		}
		switch a.Status {
		case "approved":
			return true, "approved", nil
		case "rejected":
			return false, "rejected", nil
		case "cancelled":
			return false, "cancelled", nil
		case "timeout":
			return false, "timeout", nil
		}
		if time.Now().After(deadline) {
			if _, err := e.store.ExpireApproval(ctx, e.cfg.TenantID, approvalID); err != nil {
				e.log.Warn("审批超时处理失败", "approval_id", approvalID, "error", err)
			}
			return false, "timeout", nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *Executor) audit(ctx context.Context, missionID, nodeID int64, toolName string, args json.RawMessage, d policy.Decision, outcome, detail string) {
	nodePtr := nodeID
	if err := e.store.InsertAudit(ctx, e.cfg.TenantID, missionID, &nodePtr, toolName, d.Action, d.RuleName,
		args, outcome, detail); err != nil {
		e.log.Warn("写入审计失败", "mission_id", missionID, "tool", toolName, "error", err)
	}
}

func truncate(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
