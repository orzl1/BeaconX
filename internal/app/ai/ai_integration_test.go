package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/plan"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

// fakeLLM 模拟 OpenAI 兼容端点：按 system 提示词区分规划器与执行器。
type fakeLLM struct {
	plannerCalls atomic.Int64
	workerCalls  atomic.Int64
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []llm.Message `json:"messages"`
}

func (f *fakeLLM) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		system, user := "", ""
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				system += m.Content
			case "user":
				user += m.Content
			}
		}

		var content string
		switch {
		case strings.Contains(system, "规划器"):
			f.plannerCalls.Add(1)
			if strings.Contains(user, "尚未拆分步骤") {
				decision := plan.Decision{
					Summary: "先做资产确认",
					Steps: []plan.Step{
						{Key: "recon", Title: "资产确认", Detail: "确认目标可达与基础信息"},
					},
					Actions: []plan.Action{
						{StepKey: "recon", Kind: "probe", Target: "example.com", Detail: "确认目标可达"},
					},
				}
				raw, _ := json.Marshal(decision)
				content = string(raw)
			} else {
				raw, _ := json.Marshal(plan.Decision{Done: true, Reason: "目标已完成"})
				content = string(raw)
			}
		case strings.Contains(system, "执行器"):
			f.workerCalls.Add(1)
			raw, _ := json.Marshal(workerObservation{
				Fact:   "example.com 可达且响应正常",
				Detail: "基于任务上下文给出的规划性观察（未实际执行工具）",
				Done:   true,
			})
			content = string(raw)
		default:
			http.Error(w, "unknown role", http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"id":     "chatcmpl-fake",
			"object": "chat.completion",
			"model":  req.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": content},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
}

func TestAIEngineEndToEndWithFakeLLM(t *testing.T) {
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过 AI 集成测试")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := pg.Open(ctx, pg.Options{DSN: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer db.Close()
	if _, err := migrate.Apply(ctx, db.Pool(), migrations.FS); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}

	tenantID := time.Now().UnixNano()
	if _, err := db.Pool().Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, fmt.Sprintf("ai-test-%d", tenantID), "AI 测试租户"); err != nil {
		t.Fatalf("创建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	fake := &fakeLLM{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	client, err := llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "test-key", Model: "fake-model"})
	if err != nil {
		t.Fatalf("创建模型客户端失败: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := pg.NewStore(db)
	mission, err := store.CreateMission(ctx, tenantID, pg.MissionInput{Name: "AI 规划测试", Goal: "确认 example.com 可达并给出结论"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	planner := NewPlanner(store, client, "fake-model", Config{}, logger)
	worker := NewWorker(store, client, "fake-model", logger)
	engine := runner.New(runner.Config{
		TenantID:          tenantID,
		MaxConcurrent:     1,
		PlannerInterval:   80 * time.Millisecond,
		WorkerInterval:    20 * time.Millisecond,
		LeaseTTL:          5 * time.Second,
		HeartbeatInterval: time.Second,
		Owner:             "ai-test",
	}, store, planner, worker, logger, nil)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(stopCtx)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		m, err := store.GetMission(ctx, tenantID, mission.ID)
		if err == nil && m.Status == string(task.StatusDone) {
			mission = m
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if mission.Status != string(task.StatusDone) {
		t.Fatalf("任务未完成，当前状态 %s", mission.Status)
	}

	nodes, err := store.ListNodes(ctx, tenantID, mission.ID, 100)
	if err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	facts := 0
	for _, n := range nodes {
		if n.Kind == graph.NodeFact {
			facts++
		}
	}
	if facts == 0 {
		t.Fatalf("执行器没有产出 fact 节点")
	}

	steps, err := store.ListPlanSteps(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("读取计划步骤失败: %v", err)
	}
	if len(steps) == 0 {
		t.Fatalf("规划器没有写入计划步骤")
	}
	if steps[0].Status != plan.StepDone {
		t.Fatalf("步骤状态应为 done，实际 %s", steps[0].Status)
	}

	var usageCount int
	if err := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM llm_usage WHERE tenant_id = $1 AND mission_id = $2`,
		tenantID, mission.ID).Scan(&usageCount); err != nil {
		t.Fatalf("读取用量失败: %v", err)
	}
	if usageCount < 3 {
		t.Fatalf("模型调用用量记录过少: %d", usageCount)
	}
	if fake.plannerCalls.Load() < 2 || fake.workerCalls.Load() < 1 {
		t.Fatalf("调用次数不符合预期: planner=%d worker=%d", fake.plannerCalls.Load(), fake.workerCalls.Load())
	}
	// 预算累计应大于 0。
	_, spent, _, _, err := store.MissionBudget(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("读取预算失败: %v", err)
	}
	if spent <= 0 {
		t.Fatalf("任务 spent_tokens 未累计")
	}
}
