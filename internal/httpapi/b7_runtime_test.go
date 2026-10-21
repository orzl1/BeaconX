package httpapi

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/executor"
	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B7 运行时收口测试：settings 热生效/重启恢复、轮换后重签、策略不可变、sync 恢复。

// b7Logger 返回丢弃输出的 logger，避免测试噪音。
func b7Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// b7Engine 构造一个未启动的引擎（仅用于运行时参数断言/任务创建）。
func b7Engine(store *pg.Store, tenantID int64, maxConcurrent int) *runner.Engine {
	return runner.New(runner.Config{
		TenantID:          tenantID,
		MaxConcurrent:     maxConcurrent,
		PlannerInterval:   time.Hour,
		WorkerInterval:    time.Hour,
		LeaseTTL:          time.Minute,
		HeartbeatInterval: time.Second,
	}, store, runner.IdlePlanner{}, runner.NoopWorker{}, b7Logger(), nil)
}

// b7Executor 构造一个执行器（不依赖 Evidence）。
func b7Executor(t *testing.T, store *pg.Store, tenantID int64) *executor.Executor {
	t.Helper()
	registry, err := tools.NewRegistry(tools.NewHTTPProbe())
	if err != nil {
		t.Fatalf("构造工具注册表失败: %v", err)
	}
	return executor.New(store, registry, executor.Config{
		TenantID:      tenantID,
		DefaultAction: policy.Ask,
		AskTimeout:    60 * time.Second,
	}, b7Logger())
}

// TestB7SettingsHotApplyAndRestart 覆盖 PATCH 即时生效、任务默认预算、重启从 DB 恢复。
func TestB7SettingsHotApplyAndRestart(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	ctx := context.Background()
	engine := b7Engine(store, tenantID, 2)
	exec := b7Executor(t, store, tenantID)
	runtimeCfg := NewRuntimeConfig(DefaultRuntimeSettings())
	opts := Options{Store: store, Engine: engine, Executor: exec, Runtime: runtimeCfg, TenantID: tenantID}

	// PATCH 后即时应用：调度并发、默认策略动作、审批超时、任务默认预算。
	body := `{"max_concurrent":3,"default_action":"deny","approval_timeout":"90s",` +
		`"default_budget":{"max_tokens":1234,"max_steps":7}}`
	rec := invokeHandler(t, opts.handleSettingsPatch, http.MethodPatch, "/api/v1/settings", body, nil)
	requireStatus(t, rec, http.StatusOK)
	if got := engine.MaxConcurrent(); got != 3 {
		t.Errorf("engine 并发上限 = %d，期望 3", got)
	}
	if got := exec.DefaultAction(); got != policy.Deny {
		t.Errorf("executor 默认动作 = %q，期望 deny", got)
	}
	if got := exec.ApprovalTimeout(); got != 90*time.Second {
		t.Errorf("executor 审批超时 = %v，期望 90s", got)
	}
	if tokens, steps := runtimeCfg.MissionDefaults(); tokens != 1234 || steps != 7 {
		t.Errorf("任务默认预算 = (%d,%d)，期望 (1234,7)", tokens, steps)
	}

	// 创建任务未显式给预算 → 使用默认预算。
	rec = invokeHandler(t, opts.handleCreateMission, http.MethodPost, "/api/v1/missions", `{"name":"默认预算任务"}`, nil)
	requireStatus(t, rec, http.StatusCreated)
	mission := decodeResponse[pg.Mission](t, rec)
	maxTokens, _, maxSteps, _, err := store.MissionBudget(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("读取任务预算失败: %v", err)
	}
	if maxTokens != 1234 || maxSteps != 7 {
		t.Errorf("任务实际预算 = (%d,%d)，期望 (1234,7)", maxTokens, maxSteps)
	}

	// 模拟重启：新 engine/executor/runtime + 启动配置回退，应恢复数据库中的值。
	restartFallback := RuntimeSettings{MaxConcurrent: 8, DefaultAction: policy.Allow, ApprovalTimeout: 30 * time.Second}
	engine2 := b7Engine(store, tenantID, 8)
	exec2 := b7Executor(t, store, tenantID)
	runtime2 := NewRuntimeConfig(restartFallback)
	effective, err := ApplyPersistedRuntimeSettings(ctx, store, tenantID, restartFallback, engine2, exec2, runtime2)
	if err != nil {
		t.Fatalf("启动恢复设置失败: %v", err)
	}
	if effective.MaxConcurrent != 3 || effective.DefaultAction != policy.Deny || effective.ApprovalTimeout != 90*time.Second {
		t.Fatalf("恢复的有效设置不符合预期: %+v", effective)
	}
	if engine2.MaxConcurrent() != 3 || exec2.DefaultAction() != policy.Deny || exec2.ApprovalTimeout() != 90*time.Second {
		t.Fatalf("重启后组件参数未恢复: engine=%d action=%q timeout=%v",
			engine2.MaxConcurrent(), exec2.DefaultAction(), exec2.ApprovalTimeout())
	}
	if tokens, steps := runtime2.MissionDefaults(); tokens != 1234 || steps != 7 {
		t.Fatalf("重启后任务默认预算未恢复: (%d,%d)", tokens, steps)
	}

	// 无 settings 记录的租户：沿用启动配置回退。
	otherTenant := mustExtraTenant(t)
	engine3 := b7Engine(store, otherTenant, 8)
	exec3 := b7Executor(t, store, otherTenant)
	runtime3 := NewRuntimeConfig(restartFallback)
	effective, err = ApplyPersistedRuntimeSettings(ctx, store, otherTenant, restartFallback, engine3, exec3, runtime3)
	if err != nil {
		t.Fatalf("无设置租户恢复失败: %v", err)
	}
	if effective.MaxConcurrent != 8 || effective.DefaultAction != policy.Allow || effective.ApprovalTimeout != 30*time.Second {
		t.Fatalf("无 settings 时应沿用回退配置，实际: %+v", effective)
	}

	// 非法 PATCH 仍然 400（热生效不放松校验）。
	rec = invokeHandler(t, opts.handleSettingsPatch, http.MethodPatch, "/api/v1/settings", `{"max_concurrent":0}`, nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

// TestB7RotateResignsSnapshots 覆盖密钥轮换后自动重签：Verify 全部通过且签名列改变。
func TestB7RotateResignsSnapshots(t *testing.T) {
	store, ev, tenantID, _ := b6Setup(t)
	mission := mustMission(t, store, tenantID, "轮换重签任务")
	ctx := context.Background()

	makeSnapshot := func(payload string) pg.EvidenceSnapshot {
		req := b5trafficRef(t, ev, `{"url":"https://example.com/`+payload+`"}`)
		resp := b5trafficRef(t, ev, "HTTP 200\n\n"+payload)
		return b5trafficSnapshot(t, store, ev, tenantID, mission.ID, req, resp, "http_probe")
	}
	first := makeSnapshot("first")
	second := makeSnapshot("second")

	opts := Options{Store: store, Evidence: ev, TenantID: tenantID}
	body := `{"new_key_hex":"` + hex.EncodeToString(b6Key(7)) + `"}`
	rec := invokeHandler(t, opts.handleEvidenceRotate, http.MethodPost, "/api/v1/evidence/rotate", body, nil)
	requireStatus(t, rec, http.StatusOK)
	var result struct {
		Rotated  int `json:"rotated"`
		Resigned int `json:"resigned"`
	}
	b5trafficDecodeInto(t, rec, &result)
	if result.Rotated < 2 || result.Resigned != 2 {
		t.Fatalf("轮换结果 = %+v，期望 rotated>=2 resigned=2", result)
	}

	// 签名列必须变化，并且 Verify 全部通过（新密钥重签成功）。
	reshaped, err := store.GetEvidenceSnapshot(ctx, tenantID, first.ID)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	resignedSecond, err := store.GetEvidenceSnapshot(ctx, tenantID, second.ID)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if reshaped.Signature == first.Signature {
		t.Error("第一个快照签名未变化")
	}
	if resignedSecond.Signature == second.Signature {
		// 同一份元数据 + 同一签名密钥必然得到同一签名；轮换后应不同。
		t.Error("第二个快照签名未变化")
	}
	for _, snapshot := range []pg.EvidenceSnapshot{reshaped, resignedSecond} {
		rec = invokeHandler(t, opts.handleVerifyEvidence, http.MethodGet,
			fmt.Sprintf("/api/v1/evidence/%d/verify", snapshot.ID), "", map[string]string{"id": fmt.Sprint(snapshot.ID)})
		requireStatus(t, rec, http.StatusOK)
		verified := decodeResponse[struct {
			Verified bool `json:"verified"`
		}](t, rec)
		if !verified.Verified {
			t.Errorf("快照 %d Verify 未通过", snapshot.ID)
		}
	}
}

// TestB7PolicyImmutableHardened 覆盖 immutable 规则只允许改 enabled。
func TestB7PolicyImmutableHardened(t *testing.T) {
	h := b4PolicyNewHarness(t)
	ctx := context.Background()
	if err := h.store.EnsurePolicyRules(ctx, h.tenantA, policy.SeedDefaults()); err != nil {
		t.Fatalf("写入内置规则失败: %v", err)
	}
	rules, err := h.store.ListPolicyRulesFilter(ctx, h.tenantA, pg.PolicyRuleFilter{Limit: 100})
	if err != nil {
		t.Fatalf("读取规则失败: %v", err)
	}
	var immutable pg.PolicyRule
	for _, rule := range rules {
		if rule.Immutable {
			immutable = rule
			break
		}
	}
	if immutable.ID == 0 {
		t.Fatal("未找到内置不可变规则")
	}
	path := fmt.Sprintf("/api/v1/policy/rules/%d", immutable.ID)
	for _, body := range []string{
		`{"priority":201}`,
		`{"arg_pattern":".*"}`,
		`{"timeout_seconds":1}`,
		`{"name":"改个名字"}`,
		`{"tool":"http_probe"}`,
		`{"action":"allow"}`,
	} {
		status, raw := h.do(t, http.MethodPatch, path, testAPIToken, body)
		if status != http.StatusConflict {
			t.Fatalf("修改不可变规则 %s 状态码 = %d，期望 409（body=%s）", body, status, raw)
		}
	}
	status, raw := h.do(t, http.MethodPatch, path, testAPIToken, `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("启停不可变规则状态码 = %d，期望 200（body=%s）", status, raw)
	}
}

// TestB7SyncRecoveryAndRetention 覆盖崩溃恢复与终态记录保留清理。
func TestB7SyncRecoveryAndRetention(t *testing.T) {
	store, tenantID := findingsHTTPEnv(t)
	ctx := context.Background()
	source, err := store.CreateSyncSource(ctx, tenantID, pg.SyncSourceInput{
		Name: "b7 同步源", Type: pg.SyncSourceManual, Config: map[string]any{},
	})
	if err != nil {
		t.Fatalf("创建同步源失败: %v", err)
	}
	newRun := func() pg.SyncRun {
		run, err := store.CreateSyncRun(ctx, tenantID, source.ID, nil)
		if err != nil {
			t.Fatalf("创建同步运行失败: %v", err)
		}
		return run
	}

	stale := newRun()
	fresh := newRun()
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`UPDATE sync_runs SET started_at = now() - interval '1 hour' WHERE tenant_id=$1 AND id=$2`,
		tenantID, stale.ID); err != nil {
		t.Fatalf("构造过期 running 失败: %v", err)
	}

	recovered, err := store.RecoverStaleSyncRuns(ctx, tenantID, 15*time.Minute)
	if err != nil {
		t.Fatalf("恢复同步运行失败: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("恢复数量 = %d，期望 1", recovered)
	}
	recoveredRun, err := store.GetSyncRun(ctx, tenantID, stale.ID)
	if err != nil {
		t.Fatalf("读取恢复后的运行失败: %v", err)
	}
	if recoveredRun.Status != pg.SyncRunFailed || recoveredRun.Error != "进程重启恢复" || recoveredRun.FinishedAt == nil {
		t.Fatalf("恢复后的运行不符合预期: %+v", recoveredRun)
	}
	freshRun, err := store.GetSyncRun(ctx, tenantID, fresh.ID)
	if err != nil {
		t.Fatalf("读取最新运行失败: %v", err)
	}
	if freshRun.Status != pg.SyncRunRunning {
		t.Fatalf("未超时的 running 不应被恢复，实际: %s", freshRun.Status)
	}

	// 保留期：只删除 finished_at 早于 30 天的终态记录。
	oldRun := newRun()
	if err := store.FinishSyncRun(ctx, tenantID, oldRun.ID, pg.SyncRunSucceeded, nil, ""); err != nil {
		t.Fatalf("结束同步运行失败: %v", err)
	}
	recentRun := newRun()
	if err := store.FinishSyncRun(ctx, tenantID, recentRun.ID, pg.SyncRunFailed, nil, "boom"); err != nil {
		t.Fatalf("结束同步运行失败: %v", err)
	}
	if _, err := findingsHTTPDB.Pool().Exec(ctx,
		`UPDATE sync_runs SET finished_at = now() - interval '40 days' WHERE tenant_id=$1 AND id=$2`,
		tenantID, oldRun.ID); err != nil {
		t.Fatalf("构造过期终态失败: %v", err)
	}
	deleted, err := store.DeleteOldSyncRuns(ctx, tenantID, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("清理过期运行失败: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("清理数量 = %d，期望 1", deleted)
	}
	if _, err := store.GetSyncRun(ctx, tenantID, oldRun.ID); err == nil {
		t.Error("过期终态记录应被删除")
	}
	if _, err := store.GetSyncRun(ctx, tenantID, recentRun.ID); err != nil {
		t.Errorf("未过期终态记录不应被删除: %v", err)
	}
	if _, err := store.GetSyncRun(ctx, tenantID, fresh.ID); err != nil {
		t.Errorf("running 记录不应被清理: %v", err)
	}

	// retention<=0 表示不清理。
	kept := newRun()
	if err := store.FinishSyncRun(ctx, tenantID, kept.ID, pg.SyncRunSucceeded, nil, ""); err != nil {
		t.Fatalf("结束同步运行失败: %v", err)
	}
	if deleted, err := store.DeleteOldSyncRuns(ctx, tenantID, 0); err != nil || deleted != 0 {
		t.Fatalf("retention=0 应不清理，deleted=%d err=%v", deleted, err)
	}
	if _, err := store.GetSyncRun(ctx, tenantID, kept.ID); err != nil {
		t.Errorf("retention=0 不应删除记录: %v", err)
	}
}
