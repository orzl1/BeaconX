package httpapi

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
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/events"
	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/tenant"
	"github.com/orzl1/BeaconX/migrations"
)

// HTTP 接口端到端测试依赖真实 PostgreSQL；设置 BEACONX_TEST_PG_DSN 后执行。
func TestMissionAPIIntegration(t *testing.T) {
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过 HTTP 集成测试")
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
		tenantID, fmt.Sprintf("http-test-%d", tenantID), "HTTP 测试租户"); err != nil {
		t.Fatalf("创建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	broker := events.NewBroker(64)
	store := pg.NewStore(db)
	engine := runner.New(runner.Config{
		TenantID:          tenantID,
		MaxConcurrent:     2,
		PlannerInterval:   50 * time.Millisecond,
		WorkerInterval:    10 * time.Millisecond,
		LeaseTTL:          2 * time.Second,
		HeartbeatInterval: time.Second,
		Owner:             "http-test",
	}, store, runner.IdlePlanner{}, runner.NoopWorker{}, logger, nil)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(stopCtx)
	}()

	handler, err := NewHandler(Options{
		Logger:         logger,
		RequestTimeout: 5 * time.Second,
		TenantResolver: StaticTenant(tenant.ID(tenantID)),
		Authenticator:  NewTokenAuthenticator("test-token"),
		Store:          store,
		Engine:         engine,
		Broker:         broker,
		TenantID:       tenantID,
	})
	if err != nil {
		t.Fatalf("构建 HTTP 处理器失败: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	do := func(method, path, token, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return resp
	}

	// 未带令牌：401。
	resp := do(http.MethodPost, "/api/v1/missions", "", `{"name":"x"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未授权请求状态码 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 创建任务：201 + queued。
	resp = do(http.MethodPost, "/api/v1/missions", "test-token", `{"name":"接口测试","goal":"验证任务接口"}`)
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("创建任务状态码 = %d，响应 %s", resp.StatusCode, body)
	}
	var created pg.Mission
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	resp.Body.Close()
	if created.ID == 0 || created.Status != "queued" {
		t.Fatalf("创建结果不符合预期: %+v", created)
	}

	// 轮询直到完成。
	deadline := time.Now().Add(6 * time.Second)
	var mission pg.Mission
	for time.Now().Before(deadline) {
		resp = do(http.MethodGet, fmt.Sprintf("/api/v1/missions/%d", created.ID), "test-token", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("读取任务状态码 = %d", resp.StatusCode)
		}
		_ = json.NewDecoder(resp.Body).Decode(&mission)
		resp.Body.Close()
		if mission.Status == "done" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if mission.Status != "done" {
		t.Fatalf("任务未在期限内完成，当前 %s", mission.Status)
	}

	// 事件流：回放应包含 created 与 completed。
	sseReq, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/v1/missions/%d/events?token=test-token&since=0", srv.URL, created.ID), nil)
	if err != nil {
		t.Fatalf("构造 SSE 请求失败: %v", err)
	}
	sseCtx, sseCancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer sseCancel()
	sseReq = sseReq.WithContext(sseCtx)
	sseResp, err := srv.Client().Do(sseReq)
	if err != nil {
		t.Fatalf("SSE 请求失败: %v", err)
	}
	partial, _ := io.ReadAll(sseResp.Body)
	sseResp.Body.Close()
	text := string(partial)
	if !strings.Contains(text, "event: created") || !strings.Contains(text, "event: completed") {
		t.Fatalf("事件流缺少关键事件:\n%s", text)
	}

	// 已完成任务暂停：409。
	resp = do(http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/pause", created.ID), "test-token", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("暂停已完成任务状态码 = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 删除：204，再查 404。
	resp = do(http.MethodDelete, fmt.Sprintf("/api/v1/missions/%d", created.ID), "test-token", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("删除任务状态码 = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = do(http.MethodGet, fmt.Sprintf("/api/v1/missions/%d", created.ID), "test-token", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后查询状态码 = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
