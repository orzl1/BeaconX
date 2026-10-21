package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/task"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B6 可观测补口测试：作战图游标分页、Dashboard 小时趋势、全局活动流。
//
// 复用 insightsHarness（真 PostgreSQL + 租户 A 的 HTTP 服务）：graph/dashboard
// 是既有路由的扩展，activity 是 build 注册的新路由（init() 已在包初始化时注册）。

// b6AddNode 在指定任务下创建一个作战图节点。
func (h *insightsHarness) b6AddNode(t *testing.T, missionID int64, title string) pg.Node {
	t.Helper()
	node, err := h.store.AddNode(t.Context(), h.tenantA, missionID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateDone, Title: title,
	})
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return node
}

// b6AddEvent 追加一条任务事件。
func (h *insightsHarness) b6AddEvent(t *testing.T, missionID int64, eventType task.EventType, reason string) pg.MissionEvent {
	t.Helper()
	event, err := h.store.AppendMissionEvent(t.Context(), h.tenantA, missionID, eventType, "system", reason, nil)
	if err != nil {
		t.Fatalf("追加事件失败: %v", err)
	}
	return event
}

// ---------- 作战图游标分页 ----------

func TestB6GraphPagination(t *testing.T) {
	h := newInsightsHarness(t)
	mission := h.createMission(t, "图分页测试")
	var nodes []pg.Node
	for i := 0; i < 5; i++ {
		nodes = append(nodes, h.b6AddNode(t, mission.ID, fmt.Sprintf("节点-%d", i)))
	}

	type graphView struct {
		Nodes       []pg.Node   `json:"nodes"`
		Edges       []pg.Edge   `json:"edges"`
		Anchors     []pg.Anchor `json:"anchors"`
		NextAfterID int64       `json:"next_after_id"`
	}
	getGraph := func(t *testing.T, query string) graphView {
		t.Helper()
		raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph%s", mission.ID, query), "", http.StatusOK)
		var view graphView
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatalf("解析图响应失败: %v（%s）", err, raw)
		}
		return view
	}

	t.Run("第一页与游标", func(t *testing.T) {
		page1 := getGraph(t, "?limit=2")
		if len(page1.Nodes) != 2 || page1.NextAfterID != page1.Nodes[1].ID {
			t.Fatalf("第一页异常: len=%d next=%d", len(page1.Nodes), page1.NextAfterID)
		}
		if page1.NextAfterID != nodes[1].ID {
			t.Fatalf("游标 = %d，期望第二个节点 %d", page1.NextAfterID, nodes[1].ID)
		}
		// 无 after_id 时行为不变：edges/anchors 仍以数组返回。
		if page1.Edges == nil || page1.Anchors == nil {
			t.Fatalf("edges/anchors 应为非 nil 数组: %+v", page1)
		}
	})

	t.Run("第二页无重叠且末页游标归零", func(t *testing.T) {
		page1 := getGraph(t, "?limit=2")
		page2 := getGraph(t, fmt.Sprintf("?limit=2&after_id=%d", page1.NextAfterID))
		if len(page2.Nodes) != 2 {
			t.Fatalf("第二页节点数 = %d", len(page2.Nodes))
		}
		seen := map[int64]bool{}
		for _, node := range page1.Nodes {
			seen[node.ID] = true
		}
		for _, node := range page2.Nodes {
			if seen[node.ID] {
				t.Fatalf("分页重叠: %d", node.ID)
			}
		}
		if page2.NextAfterID != page2.Nodes[1].ID {
			t.Fatalf("第二页游标异常: %d", page2.NextAfterID)
		}

		page3 := getGraph(t, fmt.Sprintf("?limit=2&after_id=%d", page2.NextAfterID))
		if len(page3.Nodes) != 1 || page3.NextAfterID != 0 {
			t.Fatalf("末页异常: len=%d next=%d", len(page3.Nodes), page3.NextAfterID)
		}
		if page3.Nodes[0].ID != nodes[4].ID {
			t.Fatalf("末页节点 = %d，期望 %d", page3.Nodes[0].ID, nodes[4].ID)
		}
	})

	t.Run("游标参数校验", func(t *testing.T) {
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph?after_id=abc", mission.ID), "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph?after_id=-1", mission.ID), "", http.StatusBadRequest)
	})
}

// ---------- Dashboard 小时趋势 ----------

func TestB6DashboardHourly(t *testing.T) {
	h := newInsightsHarness(t)

	// 造数据：1 个任务（创建+完成）、1 条 LLM 用量、1 个审批申请。
	mission := h.createMission(t, "小时趋势测试")
	if err := h.store.RecordLLMUsage(t.Context(), h.tenantA, mission.ID, "worker", "model-a",
		llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("写入 LLM 用量失败: %v", err)
	}
	node := h.b6AddNode(t, mission.ID, "审批节点")
	if _, err := h.store.CreateApproval(t.Context(), h.tenantA, mission.ID, node.ID,
		"shell_exec", []byte(`{"cmd":"id"}`), time.Minute); err != nil {
		t.Fatalf("创建审批失败: %v", err)
	}
	// 直接把任务标成 done 并写 finished_at（等价于执行器完成路径的最小写入）。
	if _, err := h.db.Pool().Exec(t.Context(),
		`UPDATE missions SET status = 'done', finished_at = now() WHERE tenant_id = $1 AND id = $2`,
		h.tenantA, mission.ID); err != nil {
		t.Fatalf("标记任务完成失败: %v", err)
	}

	raw := h.b4(t, http.MethodGet, "/api/v1/dashboard", "", http.StatusOK)
	var dashboard struct {
		Missions struct {
			Total int64 `json:"total"`
		} `json:"missions"`
		Hourly []pg.DashboardHour `json:"hourly"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("解析 dashboard 失败: %v（%s）", err, raw)
	}

	if len(dashboard.Hourly) != 24 {
		t.Fatalf("小时桶数 = %d，期望 24", len(dashboard.Hourly))
	}
	// 桶按时间升序、每小时一个、格式 RFC3339 且为整点。
	previous, err := time.Parse(time.RFC3339, dashboard.Hourly[0].Hour)
	if err != nil {
		t.Fatalf("小时格式不是 RFC3339: %q", dashboard.Hourly[0].Hour)
	}
	if previous.Minute() != 0 || previous.Second() != 0 {
		t.Fatalf("小时不是整点: %v", previous)
	}
	for i := 1; i < len(dashboard.Hourly); i++ {
		current, err := time.Parse(time.RFC3339, dashboard.Hourly[i].Hour)
		if err != nil {
			t.Fatalf("小时格式错误: %q", dashboard.Hourly[i].Hour)
		}
		if current.Sub(previous) != time.Hour {
			t.Fatalf("桶间隔不是 1 小时: %v → %v", previous, current)
		}
		previous = current
	}

	// 当前小时（最后一个桶）应包含刚造的数据。
	last := dashboard.Hourly[len(dashboard.Hourly)-1]
	if last.MissionsCreated < 1 {
		t.Fatalf("当前小时 missions_created = %d，期望 ≥1", last.MissionsCreated)
	}
	if last.MissionsDone < 1 {
		t.Fatalf("当前小时 missions_done = %d，期望 ≥1", last.MissionsDone)
	}
	if last.Tokens < 15 {
		t.Fatalf("当前小时 tokens = %d，期望 ≥15", last.Tokens)
	}
	if last.ApprovalsRequested < 1 {
		t.Fatalf("当前小时 approvals_requested = %d，期望 ≥1", last.ApprovalsRequested)
	}
	// 更早的桶没有数据，应为 0。
	if dashboard.Hourly[0].MissionsCreated != 0 || dashboard.Hourly[0].Tokens != 0 {
		t.Fatalf("空桶未补 0: %+v", dashboard.Hourly[0])
	}
	// 既有字段保持不变。
	if dashboard.Missions.Total < 1 {
		t.Fatalf("missions.total = %d，旧字段应保留", dashboard.Missions.Total)
	}

	t.Run("空租户 24 个零桶", func(t *testing.T) {
		empty := newInsightsHarness(t)
		raw := empty.b4(t, http.MethodGet, "/api/v1/dashboard", "", http.StatusOK)
		var view struct {
			Hourly []pg.DashboardHour `json:"hourly"`
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(view.Hourly) != 24 {
			t.Fatalf("空租户桶数 = %d，期望 24", len(view.Hourly))
		}
		for _, bucket := range view.Hourly {
			if bucket.MissionsCreated != 0 || bucket.MissionsDone != 0 || bucket.Tokens != 0 || bucket.ApprovalsRequested != 0 {
				t.Fatalf("空租户桶非零: %+v", bucket)
			}
		}
	})
}

// ---------- 全局活动流 ----------

func TestB6ActivityStream(t *testing.T) {
	h := newInsightsHarness(t)
	m1 := h.createMission(t, "活动流任务一")
	m2 := h.createMission(t, "活动流任务二")

	// 造 5 条事件：m1 三条（created/started/completed），m2 两条（created/completed）。
	h.b6AddEvent(t, m1.ID, task.EvCreated, "创建")
	h.b6AddEvent(t, m1.ID, task.EvStarted, "开始")
	h.b6AddEvent(t, m1.ID, task.EvCompleted, "完成")
	e4 := h.b6AddEvent(t, m2.ID, task.EvCreated, "创建")
	h.b6AddEvent(t, m2.ID, task.EvCompleted, "完成")

	type activityView struct {
		Activity []struct {
			ID        int64          `json:"id"`
			MissionID int64          `json:"mission_id"`
			Type      string         `json:"type"`
			Payload   map[string]any `json:"payload"`
			CreatedAt string         `json:"created_at"`
		} `json:"activity"`
		NextBeforeID int64 `json:"next_before_id"`
	}
	list := func(t *testing.T, query string) activityView {
		t.Helper()
		raw := h.b4(t, http.MethodGet, "/api/v1/activity"+query, "", http.StatusOK)
		var view activityView
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatalf("解析活动流失败: %v（%s）", err, raw)
		}
		return view
	}

	t.Run("倒序与字段", func(t *testing.T) {
		view := list(t, "?limit=200")
		if len(view.Activity) != 5 {
			t.Fatalf("活动数 = %d，期望 5", len(view.Activity))
		}
		if view.Activity[0].ID <= view.Activity[1].ID {
			t.Fatalf("活动流不是 id 倒序: %d, %d", view.Activity[0].ID, view.Activity[1].ID)
		}
		for _, item := range view.Activity {
			if item.MissionID == 0 || item.Type == "" || item.CreatedAt == "" {
				t.Fatalf("活动字段缺失: %+v", item)
			}
			if _, err := time.Parse(time.RFC3339, item.CreatedAt); err != nil {
				// created_at 由 encoding/json 序列化为 RFC3339Nano；这里宽松校验前缀。
				if !strings.Contains(item.CreatedAt, "T") {
					t.Fatalf("created_at 格式异常: %q", item.CreatedAt)
				}
			}
		}
	})

	t.Run("任务与类型过滤", func(t *testing.T) {
		byMission := list(t, fmt.Sprintf("?mission_id=%d", m1.ID))
		if len(byMission.Activity) != 3 {
			t.Fatalf("按任务过滤 = %d，期望 3", len(byMission.Activity))
		}
		for _, item := range byMission.Activity {
			if item.MissionID != m1.ID {
				t.Fatalf("过滤失效: %+v", item)
			}
		}
		byType := list(t, "?type=completed")
		if len(byType.Activity) != 2 {
			t.Fatalf("按类型过滤 = %d，期望 2", len(byType.Activity))
		}
		for _, item := range byType.Activity {
			if item.Type != "completed" {
				t.Fatalf("类型过滤失效: %+v", item)
			}
		}
		combined := list(t, fmt.Sprintf("?mission_id=%d&type=created", m2.ID))
		if len(combined.Activity) != 1 || combined.Activity[0].ID != e4.ID {
			t.Fatalf("组合过滤异常: %+v", combined.Activity)
		}
	})

	t.Run("游标翻页", func(t *testing.T) {
		page1 := list(t, "?limit=2")
		if len(page1.Activity) != 2 || page1.NextBeforeID == 0 {
			t.Fatalf("第一页异常: %+v", page1)
		}
		if page1.NextBeforeID != page1.Activity[1].ID {
			t.Fatalf("next_before_id = %d，期望 %d", page1.NextBeforeID, page1.Activity[1].ID)
		}
		page2 := list(t, fmt.Sprintf("?limit=2&before_id=%d", page1.NextBeforeID))
		if len(page2.Activity) == 0 {
			t.Fatal("第二页为空，游标未生效")
		}
		seen := map[int64]bool{}
		for _, item := range page1.Activity {
			seen[item.ID] = true
		}
		for _, item := range page2.Activity {
			if seen[item.ID] {
				t.Fatalf("分页重叠: %d", item.ID)
			}
			if item.ID >= page1.NextBeforeID {
				t.Fatalf("第二页包含不小于游标的记录: %+v", item)
			}
		}
		// 一直翻到空页，验证末页 next_before_id=0。
		empty := list(t, "?limit=2&before_id=1")
		if len(empty.Activity) != 0 || empty.NextBeforeID != 0 {
			t.Fatalf("空页应返回空数组且游标为 0: %+v", empty)
		}
	})

	t.Run("不存在的任务返回空数组", func(t *testing.T) {
		raw := h.b4(t, http.MethodGet, "/api/v1/activity?mission_id=999999999", "", http.StatusOK)
		var view activityView
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if len(view.Activity) != 0 {
			t.Fatalf("不存在的任务不应有活动: %+v", view.Activity)
		}
		if !strings.Contains(string(raw), `"activity":[]`) {
			t.Fatalf("空活动应输出 [] 而不是 null: %s", raw)
		}
	})

	t.Run("参数校验与鉴权", func(t *testing.T) {
		h.b4(t, http.MethodGet, "/api/v1/activity?limit=0", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/activity?limit=abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/activity?before_id=abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/activity?mission_id=abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/activity?type="+strings.Repeat("x", 65), "", http.StatusBadRequest)

		status, _, _ := h.do(t, http.MethodGet, "/api/v1/activity", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("无令牌状态码 = %d，期望 401", status)
		}
	})

	t.Run("跨租户隔离", func(t *testing.T) {
		foreignMission, err := h.store.CreateMission(t.Context(), h.tenantB, pg.MissionInput{Name: "他租户任务"})
		if err != nil {
			t.Fatalf("创建他租户任务失败: %v", err)
		}
		foreignEvent, err := h.store.AppendMissionEvent(t.Context(), h.tenantB, foreignMission.ID,
			task.EvCreated, "system", "创建", nil)
		if err != nil {
			t.Fatalf("追加他租户事件失败: %v", err)
		}
		view := list(t, "?limit=200")
		for _, item := range view.Activity {
			if item.ID == foreignEvent.ID || item.MissionID == foreignMission.ID {
				t.Fatalf("跨租户事件泄漏: %+v", item)
			}
		}
		if byMission := list(t, fmt.Sprintf("?mission_id=%d", foreignMission.ID)); len(byMission.Activity) != 0 {
			t.Fatalf("跨租户任务过滤泄漏: %+v", byMission.Activity)
		}
	})
}
