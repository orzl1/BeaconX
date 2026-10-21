// Package runner 是任务运行时：负责准入调度、规划/执行循环、暂停恢复与恢复重建。
//
// M2 阶段不接入 LLM：Planner 与 Worker 由接口注入，默认实现分别是「无事可做即完成」
// 与「把动作标记完成」。M3 会用真实 Agent 实现替换它们，引擎逻辑不变。
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// 常见错误。
var (
	// ErrNotRunning 表示任务当前不在可操作状态。
	ErrNotRunning = errors.New("任务当前状态不允许该操作")
	// ErrAlreadyStarted 表示引擎已启动。
	ErrAlreadyStarted = errors.New("引擎已启动")
)

// Config 是运行时参数。
type Config struct {
	TenantID          int64
	MaxConcurrent     int
	PlannerInterval   time.Duration
	WorkerInterval    time.Duration
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	Owner             string
}

func (c Config) withDefaults() Config {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 2
	}
	if c.PlannerInterval <= 0 {
		c.PlannerInterval = 2 * time.Second
	}
	if c.WorkerInterval <= 0 {
		c.WorkerInterval = 200 * time.Millisecond
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 10 * time.Second
	}
	if c.Owner == "" {
		c.Owner = "runner-1"
	}
	return c
}

// Planner 决定任务是否已经完成，并在 M3 负责生成新的动作节点。
type Planner interface {
	Plan(ctx context.Context, mission pg.Mission) (done bool, err error)
}

// Worker 执行单条动作，返回该动作的终态。
type Worker interface {
	Execute(ctx context.Context, mission pg.Mission, action pg.Node) (graph.NodeState, error)
}

// EventSink 接收任务事件（由 main 接到 SSE Broker）。
type EventSink func(ev pg.MissionEvent)

// Engine 管理本实例内的任务运行时。
type Engine struct {
	cfg     Config
	store   *pg.Store
	planner Planner
	worker  Worker
	log     *slog.Logger
	sink    EventSink

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	runs   map[int64]*missionRun
	closed bool
}

type missionRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// New 创建引擎。
func New(cfg Config, store *pg.Store, planner Planner, worker Worker, log *slog.Logger, sink EventSink) *Engine {
	if planner == nil {
		planner = IdlePlanner{}
	}
	if worker == nil {
		worker = NoopWorker{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:     cfg.withDefaults(),
		store:   store,
		planner: planner,
		worker:  worker,
		log:     log.With("component", "runner"),
		sink:    sink,
		runs:    map[int64]*missionRun{},
	}
}

// Start 执行恢复并启动调度循环。
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.ctx != nil {
		e.mu.Unlock()
		return ErrAlreadyStarted
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.mu.Unlock()

	// 恢复：过期租约回收 + 上次遗留的 running/finalizing 任务重新排队。
	expired, err := e.store.ResetExpiredLeases(e.ctx, e.cfg.TenantID)
	if err != nil {
		e.log.Error("回收过期租约失败", "error", err)
	}
	recovered, err := e.store.ResetRunningMissions(e.ctx, e.cfg.TenantID)
	if err != nil {
		e.log.Error("恢复遗留任务失败", "error", err)
	}
	e.log.Info("运行时就绪", "回收动作", expired, "恢复任务", recovered, "最大并发", e.cfg.MaxConcurrent)

	e.wg.Add(1)
	go e.scheduleLoop()
	return nil
}

// Stop 停止调度与全部任务协程。
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	if e.ctx == nil {
		e.mu.Unlock()
		return nil
	}
	cancel := e.cancel
	e.closed = true
	for _, run := range e.runs {
		run.cancel()
	}
	e.mu.Unlock()

	cancel()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("停止引擎超时: %w", ctx.Err())
	}
}

func (e *Engine) scheduleLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.admitOnce()
		}
	}
}

func (e *Engine) admitOnce() {
	e.mu.Lock()
	running := len(e.runs)
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return
	}
	slots := e.cfg.MaxConcurrent - running
	if slots <= 0 {
		return
	}

	queued, err := e.store.ListQueuedMissions(e.ctx, e.cfg.TenantID, slots)
	if err != nil {
		e.log.Error("读取排队任务失败", "error", err)
		return
	}
	for _, mission := range queued {
		ev, applied, err := e.store.TransitionMission(e.ctx, e.cfg.TenantID, mission.ID,
			[]task.Status{task.StatusQueued}, task.StatusRunning,
			task.EvAdmitted, "system", "准入调度", nil)
		if err != nil {
			e.log.Error("任务准入失败", "mission_id", mission.ID, "error", err)
			continue
		}
		if !applied {
			continue
		}
		e.emit(ev)
		e.mu.Lock()
		_, exists := e.runs[mission.ID]
		if !exists && !e.closed {
			e.wg.Add(1)
			go e.runMission(mission, e.ctx)
		}
		e.mu.Unlock()
	}
}

func (e *Engine) runMission(mission pg.Mission, parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	run := &missionRun{cancel: cancel, done: make(chan struct{})}

	e.mu.Lock()
	e.runs[mission.ID] = run
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.runs, mission.ID)
		e.mu.Unlock()
		close(run.done)
		e.wg.Done()
	}()

	e.log.Info("任务开始执行", "mission_id", mission.ID)
	plannerTicker := time.NewTicker(e.cfg.PlannerInterval)
	defer plannerTicker.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		// 规划：到点询问一次；完成且没有待办动作时收尾。
		select {
		case <-plannerTicker.C:
			done, err := e.planner.Plan(ctx, mission)
			if err != nil {
				e.log.Warn("规划失败", "mission_id", mission.ID, "error", err)
			} else if done {
				open, countErr := e.store.CountOpenIntents(ctx, e.cfg.TenantID, mission.ID)
				if countErr != nil {
					e.log.Warn("统计待办动作失败", "mission_id", mission.ID, "error", countErr)
				} else if open == 0 {
					e.finalize(ctx, mission.ID)
					return
				}
			}
		default:
		}

		// 执行：领取一条动作并执行，结束后释放租约。
		action, err := e.store.ClaimNextIntent(ctx, e.cfg.TenantID, mission.ID, e.cfg.Owner, e.cfg.LeaseTTL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.log.Warn("领取动作失败", "mission_id", mission.ID, "error", err)
			if !sleep(ctx, e.cfg.WorkerInterval) {
				return
			}
			continue
		}
		if action == nil {
			if !sleep(ctx, e.cfg.WorkerInterval) {
				return
			}
			continue
		}

		state, execErr := e.executeWithHeartbeat(ctx, mission, *action)
		if execErr != nil {
			state = graph.StateBlocked
			e.log.Warn("动作执行失败", "mission_id", mission.ID, "action_id", action.ID, "error", execErr)
		}
		if _, err := e.store.FinishIntent(ctx, e.cfg.TenantID, action.ID, e.cfg.Owner, state); err != nil {
			e.log.Warn("结束动作失败", "mission_id", mission.ID, "action_id", action.ID, "error", err)
		}
	}
}

func (e *Engine) executeWithHeartbeat(ctx context.Context, mission pg.Mission, action pg.Node) (graph.NodeState, error) {
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go func() {
		ticker := time.NewTicker(e.cfg.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				ok, err := e.store.HeartbeatIntent(hbCtx, e.cfg.TenantID, action.ID, e.cfg.Owner, e.cfg.LeaseTTL)
				if err != nil || !ok {
					return
				}
			}
		}
	}()
	return e.worker.Execute(ctx, mission, action)
}

func (e *Engine) finalize(ctx context.Context, missionID int64) {
	ev, applied, err := e.store.TransitionMission(ctx, e.cfg.TenantID, missionID,
		[]task.Status{task.StatusRunning}, task.StatusFinalizing,
		task.EvFinalizing, "system", "无可执行动作，进入收尾", nil)
	if err != nil {
		e.log.Error("进入收尾失败", "mission_id", missionID, "error", err)
		return
	}
	if !applied {
		return
	}
	e.emit(ev)

	doneEv, applied, err := e.store.TransitionMission(ctx, e.cfg.TenantID, missionID,
		[]task.Status{task.StatusFinalizing}, task.StatusDone,
		task.EvCompleted, "system", "任务完成", nil)
	if err != nil {
		e.log.Error("完成任务失败", "mission_id", missionID, "error", err)
		return
	}
	if applied {
		e.emit(doneEv)
		e.log.Info("任务完成", "mission_id", missionID)
	}
}

// Pause 暂停运行中的任务：先迁移状态，再取消执行协程。
func (e *Engine) Pause(ctx context.Context, missionID int64) error {
	ev, applied, err := e.store.TransitionMission(ctx, e.cfg.TenantID, missionID,
		[]task.Status{task.StatusRunning}, task.StatusPaused,
		task.EvPaused, "user", "用户暂停", nil)
	if err != nil {
		return err
	}
	if !applied {
		return ErrNotRunning
	}
	e.emit(ev)
	e.stopRun(missionID)
	e.releaseLeases(missionID)
	return nil
}

// Resume 把暂停的任务重新放回队列，由调度器再次准入。
func (e *Engine) Resume(ctx context.Context, missionID int64) error {
	ev, applied, err := e.store.TransitionMission(ctx, e.cfg.TenantID, missionID,
		[]task.Status{task.StatusPaused}, task.StatusQueued,
		task.EvResumed, "user", "用户恢复", nil)
	if err != nil {
		return err
	}
	if !applied {
		return ErrNotRunning
	}
	e.emit(ev)
	return nil
}

// Cancel 取消任务（排队/运行/暂停/收尾中均可）。
func (e *Engine) Cancel(ctx context.Context, missionID int64) error {
	from := []task.Status{task.StatusQueued, task.StatusRunning, task.StatusPaused, task.StatusFinalizing}
	ev, applied, err := e.store.TransitionMission(ctx, e.cfg.TenantID, missionID,
		from, task.StatusCancelled, task.EvCancelled, "user", "用户取消", nil)
	if err != nil {
		return err
	}
	if !applied {
		return ErrNotRunning
	}
	e.emit(ev)
	e.stopRun(missionID)
	e.releaseLeases(missionID)
	return nil
}

// Delete 停止并删除任务及其级联数据。
func (e *Engine) Delete(ctx context.Context, missionID int64) error {
	_ = e.Cancel(ctx, missionID)
	e.stopRun(missionID)
	return e.store.DeleteMission(ctx, e.cfg.TenantID, missionID)
}

// stopRun 取消并等待某个任务的执行协程退出（若不在本实例则立即返回）。
func (e *Engine) stopRun(missionID int64) {
	e.mu.Lock()
	run := e.runs[missionID]
	e.mu.Unlock()
	if run == nil {
		return
	}
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		e.log.Warn("等待任务协程退出超时", "mission_id", missionID)
	}
}

// releaseLeases 把任务下未完成动作放回 open，避免暂停/取消后动作被租约卡住。
func (e *Engine) releaseLeases(missionID int64) {
	if _, err := e.store.ReleaseMissionLeases(context.Background(), e.cfg.TenantID, missionID); err != nil {
		e.log.Warn("释放动作租约失败", "mission_id", missionID, "error", err)
	}
}

func (e *Engine) emit(ev pg.MissionEvent) {
	if e.sink == nil {
		return
	}
	e.sink(ev)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// IdlePlanner 是 M2 的默认规划器：没有待办动作时判定任务完成。
type IdlePlanner struct{}

// Plan 实现 Planner。
func (IdlePlanner) Plan(context.Context, pg.Mission) (bool, error) { return true, nil }

// NoopWorker 是 M2 的默认执行器：直接把动作标记为完成。
type NoopWorker struct{}

// Execute 实现 Worker。
func (NoopWorker) Execute(context.Context, pg.Mission, pg.Node) (graph.NodeState, error) {
	return graph.StateDone, nil
}
