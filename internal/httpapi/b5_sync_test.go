package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B5d：外部数据源同步（Sync）的端到端测试（P-B5-29..37）。
//
// 复用 insightsHarness（真 PostgreSQL + 绑定租户 A 的 HTTP 服务）；
// 异步运行通过 GET /sync/runs 轮询到终态，不 sleep 固定时长。

// b5syncBody 生成 JSON 请求体。
func b5syncBody(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码请求体失败: %v", err)
	}
	return string(raw)
}

// b5syncSourceBody 生成数据源创建/更新请求体。
func b5syncSourceBody(t *testing.T, name, typ string, config map[string]any, extra map[string]any) string {
	t.Helper()
	payload := map[string]any{"name": name, "type": typ, "config": config}
	for key, value := range extra {
		payload[key] = value
	}
	return b5syncBody(t, payload)
}

// b5syncCreateSource 通过 HTTP 创建数据源。
func (h *insightsHarness) b5syncCreateSource(t *testing.T, name, typ string, config map[string]any) pg.SyncSource {
	t.Helper()
	raw := h.b4(t, http.MethodPost, "/api/v1/sync/sources",
		b5syncSourceBody(t, name, typ, config, nil), http.StatusCreated)
	return decode[pg.SyncSource](t, raw)
}

// b5syncCreateProject 通过 HTTP 创建外部项目。
func (h *insightsHarness) b5syncCreateProject(t *testing.T, sourceID int64, externalID, name string) pg.SyncProject {
	t.Helper()
	raw := h.b4(t, http.MethodPost, "/api/v1/sync/projects", b5syncBody(t, map[string]any{
		"source_id":   sourceID,
		"external_id": externalID,
		"name":        name,
		"data":        map[string]any{"k": "v"},
	}), http.StatusCreated)
	return decode[pg.SyncProject](t, raw)
}

// b5syncStartRun 触发同步并校验 202 + running。
func (h *insightsHarness) b5syncStartRun(t *testing.T, body string) pg.SyncRun {
	t.Helper()
	raw := h.b4(t, http.MethodPost, "/api/v1/sync/run", body, http.StatusAccepted)
	run := decode[pg.SyncRun](t, raw)
	if run.ID == 0 {
		t.Fatalf("运行记录缺少 id: %s", raw)
	}
	if run.Status != pg.SyncRunRunning {
		t.Fatalf("新建运行状态 = %q，期望 running", run.Status)
	}
	return run
}

// b5syncListRuns 读取运行记录列表。
func (h *insightsHarness) b5syncListRuns(t *testing.T, query string) []pg.SyncRun {
	t.Helper()
	raw := h.b4(t, http.MethodGet, "/api/v1/sync/runs"+query, "", http.StatusOK)
	var envelope struct {
		Runs []pg.SyncRun `json:"runs"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析运行列表失败: %v（%s）", err, raw)
	}
	return envelope.Runs
}

// b5syncWaitRun 轮询运行记录直到终态（succeeded/failed），返回最终记录。
func (h *insightsHarness) b5syncWaitRun(t *testing.T, sourceID, runID int64) pg.SyncRun {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		runs := h.b5syncListRuns(t, fmt.Sprintf("?source_id=%d&limit=200", sourceID))
		for _, run := range runs {
			if run.ID != runID {
				continue
			}
			if run.Status == pg.SyncRunSucceeded || run.Status == pg.SyncRunFailed {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("同步运行 %d 未在期限内结束", runID)
	return pg.SyncRun{}
}

// b5syncListProjects 读取外部项目列表。
func (h *insightsHarness) b5syncListProjects(t *testing.T, query string) []pg.SyncProject {
	t.Helper()
	raw := h.b4(t, http.MethodGet, "/api/v1/sync/projects"+query, "", http.StatusOK)
	var envelope struct {
		Projects []pg.SyncProject `json:"projects"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析项目列表失败: %v（%s）", err, raw)
	}
	return envelope.Projects
}

// ---------- 数据源 ----------

func TestB5SyncSourceAPI(t *testing.T) {
	h := newInsightsHarness(t)
	mark := fmt.Sprintf("src-%d", time.Now().UnixNano())

	t.Run("鉴权", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/sync/sources", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("无令牌状态码 = %d，响应 %s", status, raw)
		}
	})

	t.Run("创建与列表", func(t *testing.T) {
		source := h.b5syncCreateSource(t, mark, "manual", nil)
		if source.ID == 0 || source.Type != "manual" || !source.Enabled {
			t.Fatalf("创建结果不符合预期: %+v", source)
		}
		raw := h.b4(t, http.MethodGet, "/api/v1/sync/sources", "", http.StatusOK)
		var list struct {
			Sources []pg.SyncSource `json:"sources"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("解析列表失败: %v", err)
		}
		if len(list.Sources) == 0 {
			t.Fatal("列表为空")
		}
	})

	t.Run("创建校验", func(t *testing.T) {
		// 类型非法。
		raw := h.b4(t, http.MethodPost, "/api/v1/sync/sources",
			b5syncSourceBody(t, mark+"-bad-type", "csv", nil, nil), http.StatusBadRequest)
		if code := errorCode(t, raw); code != "bad_request" {
			t.Fatalf("错误码 = %q", code)
		}
		// http_json 缺少 url。
		h.b4(t, http.MethodPost, "/api/v1/sync/sources",
			b5syncSourceBody(t, mark+"-no-url", "http_json", map[string]any{}, nil), http.StatusBadRequest)
		// url 协议非法。
		h.b4(t, http.MethodPost, "/api/v1/sync/sources",
			b5syncSourceBody(t, mark+"-ftp", "http_json", map[string]any{"url": "ftp://example.com/x"}, nil),
			http.StatusBadRequest)
		// config 不是对象。
		h.b4(t, http.MethodPost, "/api/v1/sync/sources",
			fmt.Sprintf(`{"name":"%s-arr","type":"manual","config":[1,2]}`, mark), http.StatusBadRequest)
		// headers 值不是字符串。
		h.b4(t, http.MethodPost, "/api/v1/sync/sources",
			b5syncSourceBody(t, mark+"-hdr", "http_json",
				map[string]any{"url": "https://example.com/x", "headers": map[string]any{"X-N": 1}}, nil),
			http.StatusBadRequest)
	})

	t.Run("重名冲突", func(t *testing.T) {
		raw := h.b4(t, http.MethodPost, "/api/v1/sync/sources",
			b5syncSourceBody(t, mark, "manual", nil, nil), http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}
	})

	t.Run("详情与更新", func(t *testing.T) {
		raw := h.b4(t, http.MethodGet, "/api/v1/sync/sources?limit=200", "", http.StatusOK)
		var list struct {
			Sources []pg.SyncSource `json:"sources"`
		}
		_ = json.Unmarshal(raw, &list)
		var sourceID int64
		for _, item := range list.Sources {
			if item.Name == mark {
				sourceID = item.ID
			}
		}
		if sourceID == 0 {
			t.Fatal("未找到刚创建的数据源")
		}

		raw = h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/sync/sources/%d", sourceID), "", http.StatusOK)
		if got := decode[pg.SyncSource](t, raw); got.ID != sourceID {
			t.Fatalf("详情 id = %d", got.ID)
		}

		raw = h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/sync/sources/%d", sourceID),
			b5syncBody(t, map[string]any{"name": mark + "-renamed", "enabled": false, "config": map[string]any{"note": "x"}}),
			http.StatusOK)
		updated := decode[pg.SyncSource](t, raw)
		if updated.Name != mark+"-renamed" || updated.Enabled {
			t.Fatalf("更新结果不符合预期: %+v", updated)
		}
		if updated.Config["note"] != "x" {
			t.Fatalf("config 未更新: %+v", updated.Config)
		}

		h.b4(t, http.MethodPatch, "/api/v1/sync/sources/999999999",
			b5syncBody(t, map[string]any{"name": "x"}), http.StatusNotFound)
		// 非法类型 PATCH 也要 400。
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/sync/sources/%d", sourceID),
			b5syncBody(t, map[string]any{"type": "csv"}), http.StatusBadRequest)
	})

	t.Run("敏感 headers 掩码", func(t *testing.T) {
		source := h.b5syncCreateSource(t, mark+"-mask", "http_json", map[string]any{
			"url": "http://user:pass@example.com/api",
			"headers": map[string]any{
				"Authorization": "Bearer secret-token",
				"X-Api-Key":     "key-123",
				"X-Trace":       "trace-1",
			},
		})
		headers, ok := source.Config["headers"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 headers: %+v", source.Config)
		}
		if headers["Authorization"] != "***" || headers["X-Api-Key"] != "***" {
			t.Fatalf("敏感 header 未掩码: %+v", headers)
		}
		if headers["X-Trace"] != "trace-1" {
			t.Fatalf("非敏感 header 被误掩码: %+v", headers)
		}
		urlText, _ := source.Config["url"].(string)
		if strings.Contains(urlText, "pass") || !strings.Contains(urlText, "***") {
			t.Fatalf("URL userinfo 未掩码: %q", urlText)
		}
		// 详情接口同样掩码。
		raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/sync/sources/%d", source.ID), "", http.StatusOK)
		if strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "pass@") {
			t.Fatalf("详情接口泄漏凭据: %s", raw)
		}
	})

	t.Run("跨租户与删除", func(t *testing.T) {
		foreign, err := h.store.CreateSyncSource(t.Context(), h.tenantB, pg.SyncSourceInput{Name: mark + "-b", Type: "manual"})
		if err != nil {
			t.Fatalf("创建他租户数据源失败: %v", err)
		}
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/sync/sources/%d", foreign.ID), "", http.StatusNotFound)
		h.b4(t, http.MethodDelete, fmt.Sprintf("/api/v1/sync/sources/%d", foreign.ID), "", http.StatusNotFound)

		raw := h.b4(t, http.MethodGet, "/api/v1/sync/sources?limit=200", "", http.StatusOK)
		var list struct {
			Sources []pg.SyncSource `json:"sources"`
		}
		_ = json.Unmarshal(raw, &list)
		for _, item := range list.Sources {
			if item.ID == foreign.ID {
				t.Fatalf("跨租户数据源泄漏进列表: %+v", item)
			}
		}

		// 删除本租户数据源：204，再查 404。
		var ownID int64
		for _, item := range list.Sources {
			if strings.HasPrefix(item.Name, mark) {
				ownID = item.ID
				break
			}
		}
		if ownID == 0 {
			t.Fatal("未找到可删除的数据源")
		}
		h.b4(t, http.MethodDelete, fmt.Sprintf("/api/v1/sync/sources/%d", ownID), "", http.StatusNoContent)
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/sync/sources/%d", ownID), "", http.StatusNotFound)
	})
}

// ---------- 外部项目 ----------

func TestB5SyncProjectAPI(t *testing.T) {
	h := newInsightsHarness(t)
	mark := fmt.Sprintf("prj-%d", time.Now().UnixNano())
	source := h.b5syncCreateSource(t, mark, "manual", nil)

	t.Run("创建与过滤", func(t *testing.T) {
		p1 := h.b5syncCreateProject(t, source.ID, "ext-1", "项目一")
		if p1.SourceID != source.ID || p1.ExternalID != "ext-1" || p1.Name != "项目一" {
			t.Fatalf("创建结果不符合预期: %+v", p1)
		}
		// remote_id 作为 external_id 的别名。
		raw := h.b4(t, http.MethodPost, "/api/v1/sync/projects", b5syncBody(t, map[string]any{
			"source_id": source.ID, "remote_id": "ext-2", "name": "项目二",
		}), http.StatusCreated)
		if got := decode[pg.SyncProject](t, raw); got.ExternalID != "ext-2" {
			t.Fatalf("remote_id 未作为 external_id: %+v", got)
		}

		projects := h.b5syncListProjects(t, fmt.Sprintf("?source_id=%d", source.ID))
		if len(projects) != 2 {
			t.Fatalf("项目数 = %d，期望 2", len(projects))
		}
		for _, project := range projects {
			if project.Data["k"] != "v" && project.ExternalID == "ext-1" {
				t.Fatalf("data 未保存: %+v", project.Data)
			}
		}
		if all := h.b5syncListProjects(t, ""); len(all) < 2 {
			t.Fatalf("不带过滤的项目列表 = %d", len(all))
		}
	})

	t.Run("校验与冲突", func(t *testing.T) {
		raw := h.b4(t, http.MethodPost, "/api/v1/sync/projects", b5syncBody(t, map[string]any{
			"source_id": source.ID, "external_id": "ext-1",
		}), http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}
		h.b4(t, http.MethodPost, "/api/v1/sync/projects",
			b5syncBody(t, map[string]any{"source_id": source.ID}), http.StatusBadRequest)
		h.b4(t, http.MethodPost, "/api/v1/sync/projects",
			b5syncBody(t, map[string]any{"external_id": "x"}), http.StatusBadRequest)
		h.b4(t, http.MethodPost, "/api/v1/sync/projects",
			b5syncBody(t, map[string]any{"source_id": 999999999, "external_id": "x"}), http.StatusNotFound)
		h.b4(t, http.MethodGet, "/api/v1/sync/projects?source_id=abc", "", http.StatusBadRequest)
	})

	t.Run("跨租户不可见", func(t *testing.T) {
		foreign, err := h.store.CreateSyncSource(t.Context(), h.tenantB, pg.SyncSourceInput{Name: mark + "-b", Type: "manual"})
		if err != nil {
			t.Fatalf("创建他租户数据源失败: %v", err)
		}
		if _, err := h.store.UpsertSyncProject(t.Context(), h.tenantB, pg.SyncProjectInput{
			SourceID: foreign.ID, ExternalID: "b-1", Name: "他租户项目",
		}); err != nil {
			t.Fatalf("创建他租户项目失败: %v", err)
		}
		h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/sync/projects?source_id=%d", foreign.ID), "", http.StatusOK)
		if projects := h.b5syncListProjects(t, fmt.Sprintf("?source_id=%d", foreign.ID)); len(projects) != 0 {
			t.Fatalf("跨租户项目泄漏: %+v", projects)
		}
	})
}

// ---------- 同步运行 ----------

func TestB5SyncRunAPI(t *testing.T) {
	h := newInsightsHarness(t)
	mark := fmt.Sprintf("run-%d", time.Now().UnixNano())
	source := h.b5syncCreateSource(t, mark, "manual", nil)
	h.b5syncCreateProject(t, source.ID, "p1", "项目一")
	h.b5syncCreateProject(t, source.ID, "p2", "项目二")

	t.Run("manual 运行统计", func(t *testing.T) {
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": source.ID}))
		final := h.b5syncWaitRun(t, source.ID, run.ID)
		if final.Status != pg.SyncRunSucceeded {
			t.Fatalf("运行状态 = %q，错误 %q", final.Status, final.Error)
		}
		if final.Stats["projects"] != float64(2) && final.Stats["projects"] != 2 {
			t.Fatalf("projects 统计 = %v", final.Stats["projects"])
		}
		if final.FinishedAt == nil || final.StartedAt == nil {
			t.Fatalf("时间戳缺失: %+v", final)
		}
	})

	t.Run("运行记录过滤", func(t *testing.T) {
		runs := h.b5syncListRuns(t, fmt.Sprintf("?source_id=%d", source.ID))
		if len(runs) == 0 {
			t.Fatal("按数据源过滤的运行记录为空")
		}
		succeeded := h.b5syncListRuns(t, fmt.Sprintf("?source_id=%d&status=succeeded", source.ID))
		if len(succeeded) == 0 {
			t.Fatal("按状态过滤的运行记录为空")
		}
		h.b4(t, http.MethodGet, "/api/v1/sync/runs?status=bogus", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/sync/runs?source_id=abc", "", http.StatusBadRequest)
	})

	t.Run("项目级运行", func(t *testing.T) {
		projects := h.b5syncListProjects(t, fmt.Sprintf("?source_id=%d", source.ID))
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"project_id": projects[0].ID}))
		final := h.b5syncWaitRun(t, source.ID, run.ID)
		if final.Status != pg.SyncRunSucceeded {
			t.Fatalf("项目级运行失败: %+v", final)
		}
		if final.ProjectID == nil || *final.ProjectID != projects[0].ID {
			t.Fatalf("project_id 未记录: %+v", final.ProjectID)
		}
		if final.Stats["projects"] != float64(1) && final.Stats["projects"] != 1 {
			t.Fatalf("项目级统计 = %v", final.Stats["projects"])
		}
	})

	t.Run("参数校验", func(t *testing.T) {
		h.b4(t, http.MethodPost, "/api/v1/sync/run", `{}`, http.StatusBadRequest)
		h.b4(t, http.MethodPost, "/api/v1/sync/run",
			b5syncBody(t, map[string]any{"source_id": source.ID, "mode": "http_json"}), http.StatusBadRequest)
		h.b4(t, http.MethodPost, "/api/v1/sync/run",
			b5syncBody(t, map[string]any{"source_id": 999999999}), http.StatusNotFound)
		h.b4(t, http.MethodPost, "/api/v1/sync/run",
			b5syncBody(t, map[string]any{"project_id": 999999999}), http.StatusNotFound)
	})

	t.Run("停用的数据源运行失败", func(t *testing.T) {
		disabled := h.b5syncCreateSource(t, mark+"-off", "manual", nil)
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/sync/sources/%d", disabled.ID),
			b5syncBody(t, map[string]any{"enabled": false}), http.StatusOK)
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": disabled.ID}))
		final := h.b5syncWaitRun(t, disabled.ID, run.ID)
		if final.Status != pg.SyncRunFailed {
			t.Fatalf("停用数据源运行应失败，实际 %q", final.Status)
		}
		if !strings.Contains(final.Error, "已停用") {
			t.Fatalf("错误信息未说明停用: %q", final.Error)
		}
	})
}

// ---------- http_json 适配器与 SSRF ----------

func TestB5SyncHTTPJSONAdapter(t *testing.T) {
	h := newInsightsHarness(t)
	mark := fmt.Sprintf("http-%d", time.Now().UnixNano())

	projectsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[{"id":"p1","name":"项目一"},{"id":2,"name":"项目二"}]}`))
	}))
	defer projectsServer.Close()
	assetsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"assets":[
			{"type":"root_domain","value":"Example.COM."},
			{"type":"ip","value":"192.0.2.9"},
			{"type":"planet","value":"x"}
		]}`))
	}))
	defer assetsServer.Close()
	failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failingServer.Close()
	brokenJSONServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer brokenJSONServer.Close()
	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer redirectServer.Close()

	// httptest 服务都监听 127.0.0.1，需要允许私网才能访问。
	t.Setenv("BEACONX_SYNC_ALLOW_PRIVATE", "true")

	t.Run("导入 projects 并幂等合并", func(t *testing.T) {
		source := h.b5syncCreateSource(t, mark+"-projects", "http_json",
			map[string]any{"url": projectsServer.URL})
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": source.ID}))
		final := h.b5syncWaitRun(t, source.ID, run.ID)
		if final.Status != pg.SyncRunSucceeded {
			t.Fatalf("运行失败: %+v", final)
		}
		if final.Stats["projects"] != float64(2) && final.Stats["projects"] != 2 {
			t.Fatalf("projects 统计 = %v", final.Stats["projects"])
		}
		if projects := h.b5syncListProjects(t, fmt.Sprintf("?source_id=%d", source.ID)); len(projects) != 2 {
			t.Fatalf("导入后项目数 = %d", len(projects))
		}

		// 再跑一次：upsert 语义，不产生重复项目。
		run2 := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": source.ID}))
		final2 := h.b5syncWaitRun(t, source.ID, run2.ID)
		if final2.Status != pg.SyncRunSucceeded {
			t.Fatalf("第二次运行失败: %+v", final2)
		}
		if projects := h.b5syncListProjects(t, fmt.Sprintf("?source_id=%d", source.ID)); len(projects) != 2 {
			t.Fatalf("重复同步后项目数 = %d，期望仍为 2", len(projects))
		}
	})

	t.Run("导入 assets 且归一化", func(t *testing.T) {
		source := h.b5syncCreateSource(t, mark+"-assets", "http_json",
			map[string]any{"url": assetsServer.URL})
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": source.ID}))
		final := h.b5syncWaitRun(t, source.ID, run.ID)
		if final.Status != pg.SyncRunSucceeded {
			t.Fatalf("运行失败: %+v", final)
		}
		if final.Stats["assets"] != float64(2) && final.Stats["assets"] != 2 {
			t.Fatalf("assets 统计 = %v", final.Stats["assets"])
		}
		if final.Stats["skipped"] != float64(1) && final.Stats["skipped"] != 1 {
			t.Fatalf("skipped 统计 = %v（非法类型应被跳过）", final.Stats["skipped"])
		}

		// 归一化：Example.COM. 应落成 example.com（小写、无尾点）。
		raw := h.b4(t, http.MethodGet, "/api/v1/assets?q=example.com&limit=200", "", http.StatusOK)
		var list struct {
			Assets []pg.Asset `json:"assets"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("解析资产列表失败: %v", err)
		}
		found := false
		for _, item := range list.Assets {
			if item.Type != "root_domain" {
				continue
			}
			detail := decode[pg.Asset](t,
				h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d", item.ID), "", http.StatusOK))
			for _, idn := range detail.Identities {
				if idn.Key == "example.com" {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("导入的根域资产未按 example.com 归一化")
		}
	})

	t.Run("上游失败写 failed 且不泄漏 URL", func(t *testing.T) {
		source := h.b5syncCreateSource(t, mark+"-500", "http_json",
			map[string]any{"url": failingServer.URL})
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": source.ID}))
		final := h.b5syncWaitRun(t, source.ID, run.ID)
		if final.Status != pg.SyncRunFailed {
			t.Fatalf("运行状态 = %q，期望 failed", final.Status)
		}
		if !strings.Contains(final.Error, "HTTP 500") {
			t.Fatalf("错误信息未包含上游状态: %q", final.Error)
		}
		if strings.Contains(final.Error, "127.0.0.1") {
			t.Fatalf("错误信息泄漏了上游地址: %q", final.Error)
		}

		broken := h.b5syncCreateSource(t, mark+"-broken", "http_json",
			map[string]any{"url": brokenJSONServer.URL})
		run = h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": broken.ID}))
		final = h.b5syncWaitRun(t, broken.ID, run.ID)
		if final.Status != pg.SyncRunFailed || !strings.Contains(final.Error, "不是合法 JSON") {
			t.Fatalf("非法 JSON 处理异常: %+v", final)
		}

		redirected := h.b5syncCreateSource(t, mark+"-redirect", "http_json",
			map[string]any{"url": redirectServer.URL})
		run = h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": redirected.ID}))
		final = h.b5syncWaitRun(t, redirected.ID, run.ID)
		if final.Status != pg.SyncRunFailed || !strings.Contains(final.Error, "重定向") {
			t.Fatalf("重定向应被拒绝: %+v", final)
		}
	})

	t.Run("SSRF 默认拒绝回环，允许后放行", func(t *testing.T) {
		t.Setenv("BEACONX_SYNC_ALLOW_PRIVATE", "false")
		blocked := h.b5syncCreateSource(t, mark+"-ssrf", "http_json",
			map[string]any{"url": projectsServer.URL})
		run := h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": blocked.ID}))
		final := h.b5syncWaitRun(t, blocked.ID, run.ID)
		if final.Status != pg.SyncRunFailed {
			t.Fatalf("默认应拒绝回环地址，实际状态 %q", final.Status)
		}
		if !strings.Contains(final.Error, "回环或内网") {
			t.Fatalf("错误信息应说明地址策略: %q", final.Error)
		}

		t.Setenv("BEACONX_SYNC_ALLOW_PRIVATE", "true")
		allowed := h.b5syncCreateSource(t, mark+"-ssrf-on", "http_json",
			map[string]any{"url": projectsServer.URL})
		run = h.b5syncStartRun(t, b5syncBody(t, map[string]any{"source_id": allowed.ID}))
		final = h.b5syncWaitRun(t, allowed.ID, run.ID)
		if final.Status != pg.SyncRunSucceeded {
			t.Fatalf("allow_private 开启后应成功: %+v", final)
		}
	})
}
