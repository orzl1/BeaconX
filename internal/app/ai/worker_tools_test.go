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
	"sync"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

type fakeToolRunner struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeToolRunner) ExecuteText(_ context.Context, _, _ int64, name string, args json.RawMessage) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+string(args))
	f.mu.Unlock()
	return "HTTP 200 OK from tool", nil
}

func (f *fakeToolRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestWorkerToolLoop(t *testing.T) {
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过工具循环集成测试")
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
	if _, err := db.Pool().Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`,
		tenantID, fmt.Sprintf("tool-loop-%d", tenantID), "工具循环测试租户"); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = db.Pool().Exec(c, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	store := pg.NewStore(db)

	var workerCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []llm.Message     `json:"messages"`
			Tools    []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		workerCalls++
		msg := map[string]any{"role": "assistant"}
		if workerCalls == 1 && len(req.Tools) > 0 {
			msg["tool_calls"] = []any{map[string]any{
				"id":   "call-1",
				"type": "function",
				"function": map[string]any{
					"name":      "http_probe",
					"arguments": `{"url":"https://example.com","method":"GET"}`,
				},
			}}
		} else {
			msg["content"] = `{"fact":"工具返回 HTTP 200","detail":"通过 http_probe 实际请求确认","done":true}`
		}
		resp := map[string]any{
			"id": "chatcmpl-tool", "object": "chat.completion", "model": "fake",
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := llm.NewClient(llm.Config{BaseURL: srv.URL, APIKey: "test", Model: "fake"})
	if err != nil {
		t.Fatalf("创建模型客户端失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mission, err := store.CreateMission(ctx, tenantID, pg.MissionInput{Name: "工具循环", Goal: "确认目标可达"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if _, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "probe：确认目标可达",
		Payload: map[string]any{"step_key": "recon", "kind": "probe", "target": "example.com", "detail": "确认可达"},
	}); err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}

	runnerObj := &fakeToolRunner{}
	worker := NewWorker(store, client, "fake", logger).SetTools(runnerObj, []llm.ToolDef{{
		Name: "http_probe", Description: "HTTP 探测",
		Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`),
	}})
	engine := runner.New(runner.Config{
		TenantID: tenantID, MaxConcurrent: 1, PlannerInterval: 50 * time.Millisecond,
		WorkerInterval: 20 * time.Millisecond, LeaseTTL: 5 * time.Second, HeartbeatInterval: time.Second, Owner: "tool-test",
	}, store, runner.IdlePlanner{}, worker, logger, nil)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(c)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := store.GetMission(ctx, tenantID, mission.ID)
		if err == nil && m.Status == string(task.StatusDone) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if runnerObj.count() != 1 {
		t.Fatalf("应调用一次工具，实际 %d 次", runnerObj.count())
	}
	nodes, _ := store.ListNodes(ctx, tenantID, mission.ID, 50)
	facts := 0
	for _, n := range nodes {
		if n.Kind == graph.NodeFact {
			facts++
		}
	}
	if facts != 1 {
		t.Fatalf("应写入 1 条 fact，实际 %d", facts)
	}
	if workerCalls < 2 {
		t.Fatalf("模型应至少被调用两次（工具调用+最终结论），实际 %d", workerCalls)
	}
	var usage int
	if err := db.Pool().QueryRow(ctx, `SELECT count(*) FROM llm_usage WHERE tenant_id=$1 AND mission_id=$2`,
		tenantID, mission.ID).Scan(&usage); err != nil {
		t.Fatalf("读取用量失败: %v", err)
	}
	if usage < 2 {
		t.Fatalf("用量记录过少: %d", usage)
	}
	_ = strings.TrimSpace("")
}
