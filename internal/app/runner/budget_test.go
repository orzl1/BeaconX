package runner

import (
	"context"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

func pgMission(name string) pg.MissionInput { return pg.MissionInput{Name: name} }

// TestBudgetPause 验证 token 预算耗尽时任务被暂停，而不是继续烧钱或硬失败。
func TestBudgetPause(t *testing.T) {
	s, tenantID := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mission, err := s.CreateMission(ctx, tenantID, pgMission("预算暂停测试"))
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := s.SetMissionBudget(ctx, tenantID, mission.ID, 1, 0); err != nil {
		t.Fatalf("设置预算失败: %v", err)
	}
	if err := s.RecordLLMUsage(ctx, tenantID, mission.ID, "planner", "fake", llm.Usage{TotalTokens: 10}); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}

	engine := newEngine(t, s, tenantID, 1, NoopWorker{})
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(shutdownCtx)
	}()

	waitStatus(t, s, tenantID, mission.ID, task.StatusPaused)

	events, err := s.ListMissionEvents(ctx, tenantID, mission.ID, 0, 100)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == "paused" && ev.Reason == "模型 token 预算超限暂停" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少预算暂停事件: %+v", events)
	}
}
