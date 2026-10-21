package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/plan"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B2 任务范围 / 计划 / 作战图 / 节点接口的端到端测试。
//
// 复用 assets_integration_test.go 的 apiHarness（真实 PostgreSQL，未设置
// BEACONX_TEST_PG_DSN 时整体跳过）；每个用例使用独立租户并在清理时级联删除。

// ---------- 测试用响应结构 ----------

type scopeEntryJSON struct {
	ID             int64    `json:"id"`
	MissionID      int64    `json:"mission_id"`
	Kind           string   `json:"kind"`
	Value          string   `json:"value"`
	Note           string   `json:"note"`
	AllowedActions []string `json:"allowed_actions"`
}

type scopeListJSON struct {
	Scope []scopeEntryJSON `json:"scope"`
}

type planStepJSON struct {
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Priority  int      `json:"priority"`
	DependsOn []string `json:"depends_on"`
}

type planListJSON struct {
	Steps []planStepJSON `json:"steps"`
}

type nodeJSON struct {
	ID        int64          `json:"id"`
	MissionID int64          `json:"mission_id"`
	Kind      string         `json:"kind"`
	State     string         `json:"state"`
	Title     string         `json:"title"`
	Payload   map[string]any `json:"payload"`
}

type graphJSON struct {
	Nodes []nodeJSON `json:"nodes"`
	Edges []struct {
		ID    int64  `json:"id"`
		SrcID int64  `json:"src_id"`
		DstID int64  `json:"dst_id"`
		Rel   string `json:"rel"`
	} `json:"edges"`
	Anchors []struct {
		NodeID  int64  `json:"node_id"`
		AssetID int64  `json:"asset_id"`
		Role    string `json:"role"`
	} `json:"anchors"`
}

// ---------- 小工具 ----------

// testCreateMission 在当前测试租户下建一个任务。
func testCreateMission(t *testing.T, h *apiHarness, name string) pg.Mission {
	t.Helper()
	mission, err := h.store.CreateMission(context.Background(), h.tenantA, pg.MissionInput{
		Name: name, Goal: "验证 B2 接口", Description: "集成测试任务",
	})
	if err != nil {
		t.Fatalf("创建测试任务失败: %v", err)
	}
	return mission
}

// decodeBody 把响应体解码到 dst；解码失败直接失败用例。
func decodeBody(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v（body=%s）", err, raw)
	}
}

// assertArrayField 断言响应体中的顶层字段是 JSON 数组而不是 null。
func assertArrayField(t *testing.T, raw []byte, field string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	decodeBody(t, raw, &envelope)
	value, ok := envelope[field]
	if !ok {
		t.Fatalf("响应缺少字段 %q（body=%s）", field, raw)
	}
	if strings.TrimSpace(string(value)) == "null" {
		t.Fatalf("字段 %q 不应为 null（body=%s）", field, raw)
	}
}

// ---------- 任务范围 ----------

func TestMissionScopeAPI(t *testing.T) {
	h := newAPIHarness(t)
	mission := testCreateMission(t, h, "范围接口测试")
	scopePath := fmt.Sprintf("/api/v1/missions/%d/scope", mission.ID)

	t.Run("空列表返回空数组", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, scopePath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		assertArrayField(t, raw, "scope")
		var list scopeListJSON
		decodeBody(t, raw, &list)
		if len(list.Scope) != 0 {
			t.Fatalf("空任务范围应为空，实际 %+v", list.Scope)
		}
	})

	t.Run("非法 kind 返回 400", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPost, scopePath, testAPIToken,
			`{"kind":"bogus","value":"example.com"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("新增并读取", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPost, scopePath, testAPIToken,
			`{"kind":"root_domain","value":"example.com","note":"主域","allowed_actions":["probe"]}`)
		if status != http.StatusCreated {
			t.Fatalf("状态码 = %d，期望 201（body=%s）", status, raw)
		}
		var created scopeEntryJSON
		decodeBody(t, raw, &created)
		if created.ID <= 0 || created.Kind != "root_domain" || created.Value != "example.com" {
			t.Fatalf("创建响应不符合预期: %+v", created)
		}
		if created.MissionID != mission.ID {
			t.Fatalf("mission_id = %d，期望 %d", created.MissionID, mission.ID)
		}
		if len(created.AllowedActions) != 1 || created.AllowedActions[0] != "probe" {
			t.Fatalf("allowed_actions 未保留: %+v", created.AllowedActions)
		}

		status, raw, _ = h.do(t, http.MethodGet, scopePath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("列表状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var list scopeListJSON
		decodeBody(t, raw, &list)
		if len(list.Scope) != 1 || list.Scope[0].ID != created.ID {
			t.Fatalf("列表应包含刚创建的条目，实际 %+v", list.Scope)
		}

		t.Run("重复创建返回 409", func(t *testing.T) {
			status, raw, _ := h.do(t, http.MethodPost, scopePath, testAPIToken,
				`{"kind":"root_domain","value":"example.com"}`)
			if status != http.StatusConflict {
				t.Fatalf("状态码 = %d，期望 409（body=%s）", status, raw)
			}
		})

		t.Run("删除后列表为空，重复删除 404", func(t *testing.T) {
			deletePath := fmt.Sprintf("%s/%d", scopePath, created.ID)
			status, raw, _ := h.do(t, http.MethodDelete, deletePath, testAPIToken, "")
			if status != http.StatusNoContent {
				t.Fatalf("删除状态码 = %d，期望 204（body=%s）", status, raw)
			}
			status, raw, _ = h.do(t, http.MethodGet, scopePath, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("列表状态码 = %d，期望 200（body=%s）", status, raw)
			}
			var list scopeListJSON
			decodeBody(t, raw, &list)
			if len(list.Scope) != 0 {
				t.Fatalf("删除后范围应为空，实际 %+v", list.Scope)
			}
			status, raw, _ = h.do(t, http.MethodDelete, deletePath, testAPIToken, "")
			if status != http.StatusNotFound {
				t.Fatalf("重复删除状态码 = %d，期望 404（body=%s）", status, raw)
			}
		})
	})

	t.Run("任务不存在返回 404", func(t *testing.T) {
		missing := "/api/v1/missions/999999999/scope"
		status, raw, _ := h.do(t, http.MethodGet, missing, testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, scopePath, "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- 计划 ----------

func TestMissionPlanAPI(t *testing.T) {
	h := newAPIHarness(t)
	mission := testCreateMission(t, h, "计划接口测试")
	planPath := fmt.Sprintf("/api/v1/missions/%d/plan", mission.ID)

	t.Run("空计划返回空数组", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, planPath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		assertArrayField(t, raw, "steps")
		var list planListJSON
		decodeBody(t, raw, &list)
		if len(list.Steps) != 0 {
			t.Fatalf("空计划应为空，实际 %+v", list.Steps)
		}
	})

	t.Run("读取预置步骤与依赖", func(t *testing.T) {
		ctx := context.Background()
		inserted, err := h.store.UpsertPlanSteps(ctx, h.tenantA, mission.ID, []plan.Step{
			{Key: "recon", Title: "侦察", Detail: "收集资产"},
			{Key: "enum", Title: "枚举", DependsOn: []string{"recon"}},
		})
		if err != nil {
			t.Fatalf("预置计划步骤失败: %v", err)
		}
		if inserted != 2 {
			t.Fatalf("应插入 2 条步骤，实际 %d", inserted)
		}

		status, raw, _ := h.do(t, http.MethodGet, planPath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var list planListJSON
		decodeBody(t, raw, &list)
		if len(list.Steps) != 2 {
			t.Fatalf("步骤数 = %d，期望 2（body=%s）", len(list.Steps), raw)
		}
		byKey := map[string]planStepJSON{}
		for _, st := range list.Steps {
			byKey[st.Key] = st
		}
		if got := byKey["enum"]; len(got.DependsOn) != 1 || got.DependsOn[0] != "recon" {
			t.Fatalf("enum 的依赖应为 [recon]，实际 %+v", got.DependsOn)
		}
		for _, key := range []string{"recon", "enum"} {
			if got := byKey[key].Status; got != string(plan.StepPending) {
				t.Fatalf("步骤 %s 状态应为 pending，实际 %q", key, got)
			}
		}
		if byKey["recon"].Priority != 0 {
			t.Fatalf("默认 priority 应为 0，实际 %d", byKey["recon"].Priority)
		}
	})

	t.Run("任务不存在返回 404", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/missions/999999999/plan", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})
}

// ---------- 作战图 ----------

func TestMissionGraphAPI(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	mission := testCreateMission(t, h, "作战图接口测试")

	goalNode, err := h.store.AddNode(ctx, h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeGoal, State: graph.StateOpen, Title: "拿到管理后台", Body: "目标",
	})
	if err != nil {
		t.Fatalf("创建目标节点失败: %v", err)
	}
	actionNode, err := h.store.AddNode(ctx, h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "探测登录页",
		Payload: map[string]any{"tool": "http_probe"},
	})
	if err != nil {
		t.Fatalf("创建动作节点失败: %v", err)
	}
	factNode, err := h.store.AddNode(ctx, h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateDone, Title: "登录页使用表单认证",
	})
	if err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	if _, err := h.store.AddEdge(ctx, h.tenantA, mission.ID, goalNode.ID, actionNode.ID, graph.RelSpawns); err != nil {
		t.Fatalf("创建边失败: %v", err)
	}
	assetRow, err := h.store.CreateAsset(ctx, h.tenantA, pg.AssetInput{
		Type: asset.KindRootDomain, Label: "example.com",
	})
	if err != nil {
		t.Fatalf("创建资产失败: %v", err)
	}
	if err := h.store.AddAnchor(ctx, h.tenantA, goalNode.ID, assetRow.ID, graph.RoleTarget); err != nil {
		t.Fatalf("创建锚点失败: %v", err)
	}

	graphPath := fmt.Sprintf("/api/v1/missions/%d/graph", mission.ID)

	t.Run("返回节点边锚点", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, graphPath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		for _, field := range []string{"nodes", "edges", "anchors"} {
			assertArrayField(t, raw, field)
		}
		var view graphJSON
		decodeBody(t, raw, &view)
		if len(view.Nodes) != 3 {
			t.Fatalf("节点数 = %d，期望 3", len(view.Nodes))
		}
		if len(view.Edges) != 1 || view.Edges[0].Rel != string(graph.RelSpawns) {
			t.Fatalf("边不符合预期: %+v", view.Edges)
		}
		if len(view.Anchors) != 1 || view.Anchors[0].Role != string(graph.RoleTarget) {
			t.Fatalf("锚点不符合预期: %+v", view.Anchors)
		}
		for _, n := range view.Nodes {
			if n.ID == actionNode.ID && n.Payload["tool"] != "http_probe" {
				t.Fatalf("动作节点 payload 未返回: %+v", n.Payload)
			}
		}
	})

	t.Run("kind 过滤", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, graphPath+"?kind=fact", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var view graphJSON
		decodeBody(t, raw, &view)
		if len(view.Nodes) != 1 || view.Nodes[0].Kind != string(graph.NodeFact) || view.Nodes[0].ID != factNode.ID {
			t.Fatalf("kind=fact 过滤结果不符合预期: %+v", view.Nodes)
		}
	})

	t.Run("state 过滤", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, graphPath+"?state=open", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var view graphJSON
		decodeBody(t, raw, &view)
		if len(view.Nodes) != 2 {
			t.Fatalf("state=open 应返回 2 个节点，实际 %+v", view.Nodes)
		}
	})

	t.Run("kind 与 state 组合过滤", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, graphPath+"?kind=goal&state=open", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var view graphJSON
		decodeBody(t, raw, &view)
		if len(view.Nodes) != 1 || view.Nodes[0].ID != goalNode.ID {
			t.Fatalf("组合过滤结果不符合预期: %+v", view.Nodes)
		}
	})

	t.Run("非法 kind/state 返回 400", func(t *testing.T) {
		for _, path := range []string{graphPath + "?kind=bogus", graphPath + "?state=bogus"} {
			status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
			if status != http.StatusBadRequest {
				t.Fatalf("路径 %s 状态码 = %d，期望 400（body=%s）", path, status, raw)
			}
		}
	})

	t.Run("limit 截断与钳制", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, graphPath+"?limit=1", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var view graphJSON
		decodeBody(t, raw, &view)
		if len(view.Nodes) != 1 {
			t.Fatalf("limit=1 应返回 1 个节点，实际 %d", len(view.Nodes))
		}

		// 超过上限按 2000 处理（这里只有 3 个节点，验证不报错且返回全部）。
		status, raw, _ = h.do(t, http.MethodGet, graphPath+"?limit=99999", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("超大 limit 状态码 = %d，期望 200（body=%s）", status, raw)
		}
		decodeBody(t, raw, &view)
		if len(view.Nodes) != 3 {
			t.Fatalf("超大 limit 应返回全部 3 个节点，实际 %d", len(view.Nodes))
		}
	})

	t.Run("任务不存在返回 404", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/missions/999999999/graph", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})
}

// ---------- 节点详情与状态 ----------

func TestNodeAPI(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	mission := testCreateMission(t, h, "节点接口测试")
	actionNode, err := h.store.AddNode(ctx, h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "执行探测",
		Payload: map[string]any{"tool": "shell_exec", "args": map[string]any{"command": "id"}},
	})
	if err != nil {
		t.Fatalf("创建动作节点失败: %v", err)
	}
	factNode, err := h.store.AddNode(ctx, h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateDone, Title: "已确认的事实",
	})
	if err != nil {
		t.Fatalf("创建事实节点失败: %v", err)
	}
	nodePath := fmt.Sprintf("/api/v1/nodes/%d", actionNode.ID)

	t.Run("读取节点详情", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, nodePath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var node nodeJSON
		decodeBody(t, raw, &node)
		if node.ID != actionNode.ID || node.Kind != string(graph.NodeAction) {
			t.Fatalf("节点详情不符合预期: %+v", node)
		}
		if node.Payload["tool"] != "shell_exec" {
			t.Fatalf("payload 未返回: %+v", node.Payload)
		}
	})

	t.Run("节点不存在返回 404", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/nodes/999999999", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("PATCH 合法状态", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPatch, nodePath+"/state", testAPIToken, `{"state":"running"}`)
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var node nodeJSON
		decodeBody(t, raw, &node)
		if node.State != string(graph.StateRunning) {
			t.Fatalf("状态应更新为 running，实际 %q", node.State)
		}

		status, raw, _ = h.do(t, http.MethodPatch, nodePath+"/state", testAPIToken, `{"state":"done"}`)
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		decodeBody(t, raw, &node)
		if node.State != string(graph.StateDone) {
			t.Fatalf("状态应更新为 done，实际 %q", node.State)
		}
	})

	t.Run("非法状态取值返回 400", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPatch, nodePath+"/state", testAPIToken, `{"state":"bogus"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("kind 规则拒绝 fact→running", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d/state", factNode.ID),
			testAPIToken, `{"state":"running"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("PATCH 不存在的节点返回 404", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPatch, "/api/v1/nodes/999999999/state", testAPIToken, `{"state":"done"}`)
		if status != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPatch, nodePath+"/state", "", `{"state":"done"}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- 租户隔离 ----------

func TestGraphAPITenantIsolation(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	// 用租户 B 造数据，然后以租户 A 的令牌访问，必须 404（不区分「不存在」与
	// 「属于其他租户」）。
	missionB, err := h.store.CreateMission(ctx, h.tenantB, pg.MissionInput{Name: "租户 B 的任务"})
	if err != nil {
		t.Fatalf("创建租户 B 任务失败: %v", err)
	}
	nodeB, err := h.store.AddNode(ctx, h.tenantB, missionB.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "租户 B 节点",
	})
	if err != nil {
		t.Fatalf("创建租户 B 节点失败: %v", err)
	}
	if err := h.store.AddMissionScope(ctx, h.tenantB, missionB.ID, []pg.ScopeEntry{
		{Kind: "root_domain", Value: "tenant-b.example"},
	}); err != nil {
		t.Fatalf("写入租户 B 范围失败: %v", err)
	}

	requests := []string{
		fmt.Sprintf("/api/v1/missions/%d/scope", missionB.ID),
		fmt.Sprintf("/api/v1/missions/%d/plan", missionB.ID),
		fmt.Sprintf("/api/v1/missions/%d/graph", missionB.ID),
		fmt.Sprintf("/api/v1/nodes/%d", nodeB.ID),
	}
	for _, path := range requests {
		status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("跨租户访问 %s 状态码 = %d，期望 404（body=%s）", path, status, raw)
		}
	}
}
