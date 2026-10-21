package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B8：作战图过滤下推 + 大图分页（去掉 1000 行静默截断）。
//
// 关键回归点：旧实现在 pg 层最多取 1000 行再内存过滤，节点数 >1000 时会漏；
// 现在 kind/state/after_id 全部下推 SQL，分页遍历应能取全。

// b8GraphView 是 GET /missions/{id}/graph 的响应形状。
type b8GraphView struct {
	Nodes       []pg.Node   `json:"nodes"`
	Edges       []pg.Edge   `json:"edges"`
	Anchors     []pg.Anchor `json:"anchors"`
	NextAfterID int64       `json:"next_after_id"`
}

// b8InsertNodes 批量插入节点（用 generate_series 加速，避免逐条请求）。
func b8InsertNodes(t *testing.T, h *insightsHarness, missionID int64, kind graph.NodeKind, state graph.NodeState, count int, prefix string) {
	t.Helper()
	if _, err := h.db.Pool().Exec(t.Context(), `
		INSERT INTO operation_nodes (tenant_id, mission_id, kind, state, title, body, payload)
		SELECT $1, $2, $3, $4, $5 || g, '', '{}'::jsonb
		FROM generate_series(1, $6) AS g`,
		h.tenantA, missionID, string(kind), string(state), prefix, count); err != nil {
		t.Fatalf("批量插入节点失败: %v", err)
	}
}

// b8Fetch 请求一页作战图节点。
func b8Fetch(t *testing.T, h *insightsHarness, missionID int64, query string) b8GraphView {
	t.Helper()
	raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph%s", missionID, query), "", http.StatusOK)
	var view b8GraphView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("解析图响应失败: %v（%s）", err, raw)
	}
	return view
}

// b8Walk 按 after_id 游标遍历全部分页，返回所有节点 id（同时断言严格递增、无重复）。
func b8Walk(t *testing.T, h *insightsHarness, missionID int64, baseQuery string, limit int) []int64 {
	t.Helper()
	var ids []int64
	seen := map[int64]bool{}
	afterID := int64(0)
	for page := 0; page < 100; page++ {
		separator := "?"
		if strings.Contains(baseQuery, "?") {
			separator = "&"
		}
		query := baseQuery + separator + fmt.Sprintf("limit=%d", limit)
		if afterID > 0 {
			query += fmt.Sprintf("&after_id=%d", afterID)
		}
		view := b8Fetch(t, h, missionID, query)
		for _, node := range view.Nodes {
			if seen[node.ID] {
				t.Fatalf("分页出现重复节点: %d", node.ID)
			}
			seen[node.ID] = true
			if len(ids) > 0 && node.ID <= ids[len(ids)-1] {
				t.Fatalf("节点 id 不是严格递增: %d 在 %d 之后", node.ID, ids[len(ids)-1])
			}
			ids = append(ids, node.ID)
		}
		if view.NextAfterID == 0 {
			return ids
		}
		if len(view.Nodes) == 0 {
			t.Fatalf("next_after_id=%d 但本页为空（游标死循环）", view.NextAfterID)
		}
		afterID = view.NextAfterID
	}
	t.Fatal("分页遍历超过 100 页，疑似游标未推进")
	return nil
}

func TestB8GraphFilterPushdownAndPagination(t *testing.T) {
	h := newInsightsHarness(t)
	mission := h.createMission(t, "大图分页测试")

	// 1201 个 fact（旧实现在这里会漏 201 个）+ 60 个 hypothesis + 30 个 goal。
	b8InsertNodes(t, h, mission.ID, graph.NodeFact, graph.StateDone, 1201, "事实-")
	b8InsertNodes(t, h, mission.ID, graph.NodeHypothesis, graph.StateOpen, 60, "假设-")
	b8InsertNodes(t, h, mission.ID, graph.NodeGoal, graph.StateOpen, 30, "目标-")

	t.Run("无过滤遍历取全 1201 个事实", func(t *testing.T) {
		ids := b8Walk(t, h, mission.ID, "?kind=fact", 200)
		if len(ids) != 1201 {
			t.Fatalf("kind=fact 遍历到 %d 个节点，期望 1201（旧实现会截断在 1000）", len(ids))
		}
	})

	t.Run("无过滤遍历取全全部 1291 个节点", func(t *testing.T) {
		ids := b8Walk(t, h, mission.ID, "", 200)
		if len(ids) != 1201+60+30 {
			t.Fatalf("全量遍历到 %d 个节点，期望 %d", len(ids), 1201+60+30)
		}
	})

	t.Run("kind 过滤分页不重叠且不漏", func(t *testing.T) {
		ids := b8Walk(t, h, mission.ID, "?kind=hypothesis", 20)
		if len(ids) != 60 {
			t.Fatalf("kind=hypothesis 遍历到 %d 个节点，期望 60", len(ids))
		}
		// 逐页抽查节点类型（第一页）。
		view := b8Fetch(t, h, mission.ID, "?kind=hypothesis&limit=20")
		for _, node := range view.Nodes {
			if node.Kind != graph.NodeHypothesis {
				t.Fatalf("kind 过滤失效: %+v", node)
			}
		}
	})

	t.Run("kind+state 组合过滤", func(t *testing.T) {
		view := b8Fetch(t, h, mission.ID, "?kind=goal&state=open&limit=100")
		if len(view.Nodes) != 30 || view.NextAfterID != 0 {
			t.Fatalf("组合过滤结果异常: len=%d next=%d", len(view.Nodes), view.NextAfterID)
		}
		// 没有匹配状态时返回空数组 + 游标 0。
		empty := b8Fetch(t, h, mission.ID, "?kind=goal&state=blocked")
		if len(empty.Nodes) != 0 || empty.NextAfterID != 0 {
			t.Fatalf("空结果异常: %+v", empty)
		}
	})

	t.Run("默认分页与响应形状兼容", func(t *testing.T) {
		view := b8Fetch(t, h, mission.ID, "")
		if len(view.Nodes) != 500 {
			t.Fatalf("默认 limit 返回 %d 个节点，期望 500", len(view.Nodes))
		}
		if view.NextAfterID != view.Nodes[len(view.Nodes)-1].ID {
			t.Fatalf("next_after_id = %d，期望最后一条 %d", view.NextAfterID, view.Nodes[len(view.Nodes)-1].ID)
		}
		// 无 after_id 时 edges/anchors 仍是全量数组（而非 null）。
		if view.Edges == nil || view.Anchors == nil {
			t.Fatalf("edges/anchors 应为非 nil 数组: %+v", view)
		}
	})

	t.Run("after_id 边界", func(t *testing.T) {
		all := b8Walk(t, h, mission.ID, "?kind=fact", 2000)
		if len(all) != 1201 {
			t.Fatalf("准备数据异常: %d", len(all))
		}
		last := all[len(all)-1]
		// 游标指向最后一条：没有下一页。
		if page := b8Fetch(t, h, mission.ID, fmt.Sprintf("?kind=fact&after_id=%d", last)); len(page.Nodes) != 0 || page.NextAfterID != 0 {
			t.Fatalf("游标在末尾时异常: %+v", page)
		}
		// 游标指向倒数第二条：只剩一条。
		page := b8Fetch(t, h, mission.ID, fmt.Sprintf("?kind=fact&after_id=%d", all[len(all)-2]))
		if len(page.Nodes) != 1 || page.Nodes[0].ID != last || page.NextAfterID != 0 {
			t.Fatalf("末尾单条分页异常: %+v", page)
		}
	})

	t.Run("参数校验与 404", func(t *testing.T) {
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph?kind=planet", mission.ID), "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph?state=unknown", mission.ID), "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph?after_id=abc", mission.ID), "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/missions/999999999/graph", "", http.StatusNotFound)
	})
}

// TestB8GraphLimitBeyondThousand 专门验证「不再有 1000 行上限」：
// limit > 1000 时必须真的返回那么多行（旧实现最多 1000）。
func TestB8GraphLimitBeyondThousand(t *testing.T) {
	h := newInsightsHarness(t)
	mission := h.createMission(t, "超千分页测试")
	b8InsertNodes(t, h, mission.ID, graph.NodeFact, graph.StateDone, 2500, "节点-")

	t.Run("limit=1500 返回 1500 行", func(t *testing.T) {
		page := b8Fetch(t, h, mission.ID, "?limit=1500")
		if len(page.Nodes) != 1500 {
			t.Fatalf("limit=1500 返回 %d 行（旧实现会截断到 1000）", len(page.Nodes))
		}
		if page.NextAfterID != page.Nodes[len(page.Nodes)-1].ID {
			t.Fatalf("next_after_id 异常: %d", page.NextAfterID)
		}
		// 第二页取剩余 1000 条，且与第一页不重叠。
		second := b8Fetch(t, h, mission.ID, fmt.Sprintf("?limit=1500&after_id=%d", page.NextAfterID))
		if len(second.Nodes) != 1000 || second.NextAfterID != 0 {
			t.Fatalf("第二页异常: len=%d next=%d", len(second.Nodes), second.NextAfterID)
		}
		if second.Nodes[0].ID <= page.Nodes[len(page.Nodes)-1].ID {
			t.Fatalf("第二页与第一页重叠: %d <= %d", second.Nodes[0].ID, page.Nodes[len(page.Nodes)-1].ID)
		}
	})

	t.Run("limit 超过上限被钳制到 2000", func(t *testing.T) {
		page := b8Fetch(t, h, mission.ID, "?limit=99999")
		if len(page.Nodes) != 2000 {
			t.Fatalf("limit 应钳制到 2000，实际 %d", len(page.Nodes))
		}
	})

	t.Run("跨租户不可见", func(t *testing.T) {
		foreign, err := h.store.CreateMission(t.Context(), h.tenantB, pg.MissionInput{Name: "他租户任务"})
		if err != nil {
			t.Fatalf("创建他租户任务失败: %v", err)
		}
		if _, err := h.db.Pool().Exec(t.Context(), `
			INSERT INTO operation_nodes (tenant_id, mission_id, kind, state, title)
			VALUES ($1, $2, 'fact', 'done', '他租户节点')`, h.tenantB, foreign.ID); err != nil {
			t.Fatalf("插入他租户节点失败: %v", err)
		}
		// 租户 A 通过任务 id 访问他租户任务 → 404。
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/graph", foreign.ID), "", http.StatusNotFound)
		// 本租户任务的分页结果不受污染。
		ids := b8Walk(t, h, mission.ID, "", 500)
		if len(ids) != 2500 {
			t.Fatalf("本租户遍历到 %d 个节点，期望 2500", len(ids))
		}
	})
}
