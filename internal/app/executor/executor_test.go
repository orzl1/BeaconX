package executor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

func setup(t *testing.T) (*Executor, *pg.Store, int64, int64) {
	t.Helper()
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过执行器集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := pg.Open(ctx, pg.Options{DSN: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := migrate.Apply(ctx, db.Pool(), migrations.FS); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	tenantID := time.Now().UnixNano()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`,
		tenantID, fmt.Sprintf("exec-test-%d", tenantID), "执行器测试租户"); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = db.Pool().Exec(c, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	store := pg.NewStore(db)
	if err := store.EnsurePolicyRules(ctx, tenantID, policy.SeedDefaults()); err != nil {
		t.Fatalf("写入内置规则失败: %v", err)
	}
	mission, err := store.CreateMission(ctx, tenantID, pg.MissionInput{Name: "执行器测试"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	node, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "测试动作"})
	if err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}
	registry, err := tools.NewRegistry(tools.NewHTTPProbe(), tools.NewShellExec())
	if err != nil {
		t.Fatalf("创建注册表失败: %v", err)
	}
	exec := New(store, registry, Config{TenantID: tenantID, DefaultAction: policy.Ask, AskTimeout: 2 * time.Second, PollInterval: 50 * time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return exec, store, mission.ID, node.ID
}

func TestAllowRuleExecutesAndAudits(t *testing.T) {
	exec, store, missionID, nodeID := setup(t)
	ctx := context.Background()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer target.Close()

	res, err := exec.Execute(ctx, missionID, nodeID, "http_probe", []byte(`{"url":"`+target.URL+`","method":"GET"}`))
	if err != nil {
		t.Fatalf("GET 应被允许: %v", err)
	}
	if !strings.Contains(res.Output, "HTTP 200") {
		t.Fatalf("输出不符合预期: %s", res.Output)
	}
	audit, err := store.ListAudit(ctx, exec.cfg.TenantID, missionID, 10)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if len(audit) != 1 || audit[0].Outcome != "executed" || audit[0].Action != "allow" {
		t.Fatalf("审计不符合预期: %+v", audit)
	}
}

func TestAskApprovedExecutes(t *testing.T) {
	exec, store, missionID, nodeID := setup(t)
	ctx := context.Background()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("created"))
	}))
	defer target.Close()

	// 后台审批：等待 pending 项并批准。
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending, err := store.ListApprovals(context.Background(), exec.cfg.TenantID, "pending", 10)
			if err == nil && len(pending) > 0 {
				_, _, _ = store.DecideApproval(context.Background(), exec.cfg.TenantID, pending[0].ID, true, "tester", "允许")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	_, err := exec.Execute(ctx, missionID, nodeID, "http_probe", []byte(`{"url":"`+target.URL+`","method":"POST","body":"x"}`))
	if err != nil {
		t.Fatalf("批准后应执行成功: %v", err)
	}
	audit, _ := store.ListAudit(ctx, exec.cfg.TenantID, missionID, 10)
	if len(audit) == 0 || audit[0].Outcome != "executed" {
		t.Fatalf("审计不符合预期: %+v", audit)
	}
}

func TestDenyRuleBlocks(t *testing.T) {
	exec, store, missionID, nodeID := setup(t)
	ctx := context.Background()
	_, err := exec.Execute(ctx, missionID, nodeID, "shell_exec", []byte(`{"command":"rm -rf /"}`))
	if err == nil || !strings.Contains(err.Error(), "策略拒绝") {
		t.Fatalf("危险命令应被拒绝，实际: %v", err)
	}
	audit, _ := store.ListAudit(ctx, exec.cfg.TenantID, missionID, 10)
	if len(audit) != 1 || audit[0].Action != "deny" || audit[0].Outcome != "denied_by_rule" {
		t.Fatalf("拒绝审计不符合预期: %+v", audit)
	}
}

func TestAskRejected(t *testing.T) {
	exec, store, missionID, nodeID := setup(t)
	ctx := context.Background()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			pending, err := store.ListApprovals(context.Background(), exec.cfg.TenantID, "pending", 10)
			if err == nil && len(pending) > 0 {
				_, _, _ = store.DecideApproval(context.Background(), exec.cfg.TenantID, pending[0].ID, false, "tester", "不允许")
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	_, err := exec.Execute(ctx, missionID, nodeID, "shell_exec", []byte(`{"command":"echo hello"}`))
	if err == nil || !strings.Contains(err.Error(), "未获人工批准") {
		t.Fatalf("拒绝后应报错: %v", err)
	}
	audit, _ := store.ListAudit(ctx, exec.cfg.TenantID, missionID, 10)
	if len(audit) == 0 || audit[0].Outcome != "rejected_by_user" {
		t.Fatalf("审计不符合预期: %+v", audit)
	}
}
