package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

// 引擎集成测试依赖真实 PostgreSQL；设置 BEACONX_TEST_PG_DSN 后才执行。

var (
	testOnce sync.Once
	testDB   *pg.DB
	testErr  error
)

func testStore(t *testing.T) (*pg.Store, int64) {
	t.Helper()
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过运行时集成测试")
	}
	testOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		testDB, testErr = pg.Open(ctx, pg.Options{DSN: dsn, MaxConns: 6, MinConns: 1, ConnectTimeout: 10 * time.Second})
		if testErr != nil {
			return
		}
		_, testErr = migrate.Apply(ctx, testDB.Pool(), migrations.FS)
	})
	if testErr != nil {
		t.Fatalf("准备测试数据库失败: %v", testErr)
	}
	tenantID := time.Now().UnixNano()
	if _, err := testDB.Pool().Exec(context.Background(),
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, fmt.Sprintf("runner-test-%d", tenantID), "运行时测试租户"); err != nil {
		t.Fatalf("创建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = testDB.Pool().Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	return pg.NewStore(testDB), tenantID
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitStatus(t *testing.T, s *pg.Store, tenantID, missionID int64, want task.Status) pg.Mission {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		m, err := s.GetMission(context.Background(), tenantID, missionID)
		if err == nil && task.Status(m.Status) == want {
			return m
		}
		time.Sleep(30 * time.Millisecond)
	}
	m, _ := s.GetMission(context.Background(), tenantID, missionID)
	t.Fatalf("等待任务状态 %s 超时，当前 %s", want, m.Status)
	return pg.Mission{}
}

func newEngine(t *testing.T, s *pg.Store, tenantID int64, maxConc int, worker Worker) *Engine {
	t.Helper()
	return New(Config{
		TenantID:          tenantID,
		MaxConcurrent:     maxConc,
		PlannerInterval:   50 * time.Millisecond,
		WorkerInterval:    20 * time.Millisecond,
		LeaseTTL:          2 * time.Second,
		HeartbeatInterval: 500 * time.Millisecond,
		Owner:             "test-runner",
	}, s, IdlePlanner{}, worker, discardLogger(), nil)
}

func TestMissionLifecycleToCompletion(t *testing.T) {
	s, tenantID := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mission, err := s.CreateMission(ctx, tenantID, pg.MissionInput{Name: "生命周期测试"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if _, err := s.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "动作一"}); err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}
	if _, err := s.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "动作二"}); err != nil {
		t.Fatalf("创建动作失败: %v", err)
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

	waitStatus(t, s, tenantID, mission.ID, task.StatusDone)

	nodes, err := s.ListNodes(ctx, tenantID, mission.ID, 10)
	if err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	for _, n := range nodes {
		if n.State != graph.StateDone {
			t.Fatalf("动作 %d 应为 done，实际 %s", n.ID, n.State)
		}
	}
	events, err := s.ListMissionEvents(ctx, tenantID, mission.ID, 0, 100)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	seen := map[string]bool{}
	for _, ev := range events {
		seen[ev.Type] = true
	}
	for _, want := range []string{"created", "admitted", "finalizing", "completed"} {
		// created 事件由 API 层写入；引擎负责其余三类。
		if want == "created" {
			continue
		}
		if !seen[want] {
			t.Fatalf("缺少事件 %s，实际 %v", want, seen)
		}
	}
}

// blockingWorker 在第一次执行时通知测试并等待放行，用于验证暂停/恢复。
type blockingWorker struct {
	started chan int64
	release chan struct{}
	once    sync.Once
}

func (w *blockingWorker) Execute(ctx context.Context, _ pg.Mission, action pg.Node) (graph.NodeState, error) {
	w.once.Do(func() {
		if w.started != nil {
			w.started <- action.ID
		}
	})
	select {
	case <-w.release:
		return graph.StateDone, nil
	case <-ctx.Done():
		return graph.StateBlocked, ctx.Err()
	}
}

func TestPauseResumeCancelLifecycle(t *testing.T) {
	s, tenantID := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mission, err := s.CreateMission(ctx, tenantID, pg.MissionInput{Name: "暂停恢复测试"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if _, err := s.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "阻塞动作"}); err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}

	worker := &blockingWorker{started: make(chan int64, 1), release: make(chan struct{})}
	engine := newEngine(t, s, tenantID, 1, worker)
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(shutdownCtx)
	}()

	select {
	case <-worker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("等待动作开始执行超时")
	}

	if err := engine.Pause(ctx, mission.ID); err != nil {
		t.Fatalf("暂停失败: %v", err)
	}
	waitStatus(t, s, tenantID, mission.ID, task.StatusPaused)

	// 暂停后动作应被放回 open，且不带租约。
	nodes, err := s.ListNodes(ctx, tenantID, mission.ID, 10)
	if err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	if len(nodes) != 1 || nodes[0].State != graph.StateOpen {
		t.Fatalf("暂停后动作应回 open，实际 %+v", nodes)
	}

	if err := engine.Resume(ctx, mission.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 恢复后应再次被准入并重新执行；放行后完成。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		m, err := s.GetMission(ctx, tenantID, mission.ID)
		if err == nil && m.Status == string(task.StatusRunning) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	close(worker.release)
	waitStatus(t, s, tenantID, mission.ID, task.StatusDone)

	// 取消一个已完成任务应返回 ErrNotRunning。
	if err := engine.Cancel(ctx, mission.ID); err == nil {
		t.Fatalf("已完成任务不应允许取消")
	}
}

func TestRecoveryAfterCrash(t *testing.T) {
	s, tenantID := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mission, err := s.CreateMission(ctx, tenantID, pg.MissionInput{Name: "崩溃恢复测试"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if _, err := s.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "遗留动作"}); err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}
	// 模拟崩溃现场：任务 running、动作被领取且租约即将过期。
	if _, applied, err := s.TransitionMission(ctx, tenantID, mission.ID,
		[]task.Status{task.StatusQueued}, task.StatusRunning, task.EvAdmitted, "system", "模拟准入", nil); err != nil || !applied {
		t.Fatalf("模拟运行状态失败: applied=%v err=%v", applied, err)
	}
	if _, err := s.ClaimNextIntent(ctx, tenantID, mission.ID, "ghost-runner", time.Second); err != nil {
		t.Fatalf("模拟领取动作失败: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // 等租约过期

	engine := newEngine(t, s, tenantID, 1, NoopWorker{})
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(shutdownCtx)
	}()

	// 恢复后不允许出现永久 running，最终应完成。
	waitStatus(t, s, tenantID, mission.ID, task.StatusDone)
}

func TestDeleteMission(t *testing.T) {
	s, tenantID := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mission, err := s.CreateMission(ctx, tenantID, pg.MissionInput{Name: "删除测试"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
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

	if err := engine.Delete(ctx, mission.ID); err != nil {
		t.Fatalf("删除任务失败: %v", err)
	}
	if _, err := s.GetMission(ctx, tenantID, mission.ID); err == nil {
		t.Fatalf("删除后仍能读到任务")
	}
}

func TestAdmissionLimit(t *testing.T) {
	s, tenantID := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m1, err := s.CreateMission(ctx, tenantID, pg.MissionInput{Name: "并发任务一"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	m2, err := s.CreateMission(ctx, tenantID, pg.MissionInput{Name: "并发任务二"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	engine := newEngine(t, s, tenantID, 1, slowWorker{delay: 400 * time.Millisecond})
	if err := engine.Start(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = engine.Stop(shutdownCtx)
	}()

	// 并发上限为 1：任一时刻最多一个任务处于 running。
	deadline := time.Now().Add(8 * time.Second)
	doneCount := 0
	for time.Now().Before(deadline) {
		a, _ := s.GetMission(ctx, tenantID, m1.ID)
		b, _ := s.GetMission(ctx, tenantID, m2.ID)
		running := 0
		for _, st := range []string{a.Status, b.Status} {
			if st == string(task.StatusRunning) {
				running++
			}
		}
		if running > 1 {
			t.Fatalf("并发上限被突破：%s / %s", a.Status, b.Status)
		}
		if a.Status == string(task.StatusDone) {
			doneCount++
		}
		if b.Status == string(task.StatusDone) {
			doneCount++
		}
		if doneCount >= 2 {
			return
		}
		doneCount = 0
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("任务未在期限内完成")
}

type slowWorker struct{ delay time.Duration }

func (w slowWorker) Execute(ctx context.Context, _ pg.Mission, _ pg.Node) (graph.NodeState, error) {
	select {
	case <-time.After(w.delay):
		return graph.StateDone, nil
	case <-ctx.Done():
		return graph.StateBlocked, ctx.Err()
	}
}
