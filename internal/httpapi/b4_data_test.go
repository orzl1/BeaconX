package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/finding"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B4c 数据补全与游标分页的端到端测试（P-B4-20..26）。
//
// 复用 insightsHarness（真 PostgreSQL + 临时证据目录 + 绑定租户 A 的 HTTP 服务）；
// 每个用例独立租户，清理靠删除租户级联。未设置 BEACONX_TEST_PG_DSN 时跳过。

// b4 发送带令牌的请求并断言状态码，返回响应体。
func (h *insightsHarness) b4(t *testing.T, method, path, body string, wantStatus int) []byte {
	t.Helper()
	status, raw, _ := h.do(t, method, path, testAPIToken, body)
	if status != wantStatus {
		t.Fatalf("%s %s 状态码 = %d，期望 %d，响应 %s", method, path, status, wantStatus, raw)
	}
	return raw
}

// b4createAsset 通过 HTTP 创建资产。
func (h *insightsHarness) b4createAsset(t *testing.T, kind asset.Kind, label string, ids []asset.Identity) pg.Asset {
	t.Helper()
	payload := map[string]any{"type": string(kind), "label": label}
	if len(ids) > 0 {
		payload["identities"] = ids
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码资产请求失败: %v", err)
	}
	raw := h.b4(t, http.MethodPost, "/api/v1/assets", string(body), http.StatusCreated)
	return decode[pg.Asset](t, raw)
}

// b4createCompany 通过 HTTP 创建企业。
func (h *insightsHarness) b4createCompany(t *testing.T, name, slug string) pg.Company {
	t.Helper()
	body, err := json.Marshal(map[string]string{"name": name, "slug": slug})
	if err != nil {
		t.Fatalf("编码企业请求失败: %v", err)
	}
	raw := h.b4(t, http.MethodPost, "/api/v1/companies", string(body), http.StatusCreated)
	return decode[pg.Company](t, raw)
}

// b4createFinding 通过 HTTP 在任务下创建发现。
func (h *insightsHarness) b4createFinding(t *testing.T, missionID int64, title, severity string, assetID *int64) pg.Finding {
	t.Helper()
	payload := map[string]any{"title": title, "severity": severity}
	if assetID != nil {
		payload["asset_id"] = *assetID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码发现请求失败: %v", err)
	}
	raw := h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/findings", missionID), string(body), http.StatusCreated)
	return decode[pg.Finding](t, raw)
}

// b4insertTraffic 写入一条已完成的流量采集记录，返回 id。
func (h *insightsHarness) b4insertTraffic(t *testing.T, missionID int64, tool string) int64 {
	t.Helper()
	ctx := context.Background()
	reqRef := h.putBlob(t, []byte(`{"request":"`+tool+`"}`))
	respRef := h.putBlob(t, []byte(`{"response":"ok"}`))
	id, err := h.store.BeginTraffic(ctx, h.tenantA, missionID, nil, tool, reqRef, "none")
	if err != nil {
		t.Fatalf("写入流量请求阶段失败: %v", err)
	}
	if err := h.store.FinishTraffic(ctx, h.tenantA, id, respRef, 200, ""); err != nil {
		t.Fatalf("写入流量响应阶段失败: %v", err)
	}
	return id
}

// b4insertCommand 写入一条命令执行审计，返回 id。
func (h *insightsHarness) b4insertCommand(t *testing.T, missionID int64, tool string) int64 {
	t.Helper()
	if err := h.store.InsertAudit(context.Background(), h.tenantA, missionID, nil,
		tool, policy.Allow, "测试规则", []byte(`{"cmd":"id"}`), "allowed", "测试命令"); err != nil {
		t.Fatalf("写入命令审计失败: %v", err)
	}
	raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/commands?mission_id=%d&limit=1", missionID), "", http.StatusOK)
	page := decode[b4Page[pg.AuditEvent]](t, raw)
	if len(page.List) == 0 {
		t.Fatal("命令审计写入后列表为空")
	}
	return page.List[0].ID
}

// b4insertLLMRecord 写入一条模型调用记录，返回 id。
func (h *insightsHarness) b4insertLLMRecord(t *testing.T, missionID int64, role, model string) int64 {
	t.Helper()
	if err := h.store.RecordLLMUsage(context.Background(), h.tenantA, missionID, role, model,
		llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("写入 LLM 用量失败: %v", err)
	}
	raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/llm-records?mission_id=%d&limit=1", missionID), "", http.StatusOK)
	page := decode[struct {
		Records []pg.LLMUsageRecord `json:"records"`
	}](t, raw)
	if len(page.Records) == 0 {
		t.Fatal("LLM 记录写入后列表为空")
	}
	return page.Records[0].ID
}

// b4Page 是「列表 + 下一页游标」的通用响应形状。
type b4Page[T any] struct {
	List    []T   `json:"-"`
	Next    int64 `json:"next_after_id"`
	rawList []byte
}

// UnmarshalJSON 让同一个泛型结构可以解析不同字段名（assets/findings/traffic/commands/records）。
func (p *b4Page[T]) UnmarshalJSON(data []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if raw, ok := envelope["next_after_id"]; ok {
		if err := json.Unmarshal(raw, &p.Next); err != nil {
			return err
		}
	}
	for _, field := range []string{"assets", "findings", "traffic", "commands", "records", "items"} {
		if raw, ok := envelope[field]; ok {
			var list []T
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			p.List = list
			return nil
		}
	}
	return nil
}

// ---------- 身份键写删（P-B4-20 / P-B4-21） ----------

func TestB4AssetIdentityAPI(t *testing.T) {
	h := newInsightsHarness(t)
	mark := fmt.Sprintf("idn-%d", time.Now().UnixNano())

	target := h.b4createAsset(t, asset.KindSubdomain, mark+".example.com", []asset.Identity{
		{Kind: asset.KindSubdomain, Key: mark + ".example.com"},
	})

	t.Run("新增身份并归一化", func(t *testing.T) {
		// 大写 + 结尾点 → 归一化为小写无尾点。
		raw := h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID),
			fmt.Sprintf(`{"kind":"subdomain","key":"  API.%s.Example.COM.  "}`, mark), http.StatusCreated)
		idn := decode[pg.AssetIdentity](t, raw)
		if idn.Kind != asset.KindSubdomain || idn.Key != "api."+strings.ToLower(mark)+".example.com" {
			t.Fatalf("身份未归一化: %+v", idn)
		}
		if idn.AssetID != target.ID || idn.TenantID != h.tenantA {
			t.Fatalf("身份归属错误: %+v", idn)
		}

		// URL 默认端口被去掉（endpoint 身份）。
		raw = h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID),
			`{"kind":"endpoint","key":"HTTPS://Example.COM:443/admin"}`, http.StatusCreated)
		if got := decode[pg.AssetIdentity](t, raw); got.Key != "https://example.com/admin" {
			t.Fatalf("URL 身份未归一化: %+v", got)
		}

		// 详情与身份列表都应包含新增项。
		raw = h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d", target.ID), "", http.StatusOK)
		detail := decode[pg.Asset](t, raw)
		if len(detail.Identities) != 3 {
			t.Fatalf("详情身份数 = %d，期望 3：%+v", len(detail.Identities), detail.Identities)
		}
		raw = h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID), "", http.StatusOK)
		list := decode[struct {
			Identities []pg.AssetIdentity `json:"identities"`
		}](t, raw)
		if len(list.Identities) != 3 {
			t.Fatalf("身份列表数 = %d，期望 3", len(list.Identities))
		}
	})

	t.Run("重复与占用冲突", func(t *testing.T) {
		// 同一身份用不同写法重复提交 → 归一化后撞唯一键 → 409。
		raw := h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID),
			fmt.Sprintf(`{"kind":"subdomain","key":"API.%s.example.com."}`, mark), http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}

		// 同一身份键被别的资产占用 → 409（不是 500）。
		other := h.b4createAsset(t, asset.KindSubdomain, mark+"-other.example.com", []asset.Identity{
			{Kind: asset.KindSubdomain, Key: mark + "-other.example.com"},
		})
		h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", other.ID),
			fmt.Sprintf(`{"kind":"subdomain","key":"api.%s.example.com"}`, mark), http.StatusConflict)
	})

	t.Run("参数与不存在", func(t *testing.T) {
		h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID),
			`{"kind":"planet","key":"mars"}`, http.StatusBadRequest)
		h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID),
			`{"kind":"ip","key":"999.1.1.1"}`, http.StatusBadRequest)
		h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID),
			`{"kind":"subdomain","key":"   "}`, http.StatusBadRequest)
		h.b4(t, http.MethodPost, "/api/v1/assets/999999999/identities",
			`{"kind":"ip","key":"192.0.2.1"}`, http.StatusNotFound)

		// 跨租户：租户 B 的资产经租户 A 的接口不可见。
		foreign, err := h.store.UpsertAsset(context.Background(), h.tenantB, pg.AssetInput{
			Type:       asset.KindIP,
			Label:      "192.0.2.200",
			Identities: []asset.Identity{{Kind: asset.KindIP, Key: "192.0.2.200"}},
		})
		if err != nil {
			t.Fatalf("创建他租户资产失败: %v", err)
		}
		h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/assets/%d/identities", foreign.ID),
			`{"kind":"ip","key":"192.0.2.201"}`, http.StatusNotFound)
	})

	t.Run("删除身份", func(t *testing.T) {
		// URL 身份键含 "://"，客户端需 URL 编码；服务端 {key...} 通配符解码后归一化匹配。
		key := url.PathEscape("https://example.com/admin")
		path := fmt.Sprintf("/api/v1/assets/%d/identities/endpoint/%s", target.ID, key)
		h.b4(t, http.MethodDelete, path, "", http.StatusNoContent)
		h.b4(t, http.MethodDelete, path, "", http.StatusNotFound)

		raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID), "", http.StatusOK)
		list := decode[struct {
			Identities []pg.AssetIdentity `json:"identities"`
		}](t, raw)
		if len(list.Identities) != 2 {
			t.Fatalf("删除后身份数 = %d，期望 2：%+v", len(list.Identities), list.Identities)
		}

		h.b4(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/assets/%d/identities/planet/mars", target.ID), "", http.StatusBadRequest)
		h.b4(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/assets/%d/identities/subdomain/not-exist.example.com", target.ID), "", http.StatusNotFound)

		// 删除到零条身份是允许的（裸资产合法）。
		raw = h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID), "", http.StatusOK)
		list = decode[struct {
			Identities []pg.AssetIdentity `json:"identities"`
		}](t, raw)
		for _, idn := range list.Identities {
			h.b4(t, http.MethodDelete, fmt.Sprintf("/api/v1/assets/%d/identities/%s/%s",
				target.ID, string(idn.Kind), url.PathEscape(idn.Key)), "", http.StatusNoContent)
		}
		raw = h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/identities", target.ID), "", http.StatusOK)
		list = decode[struct {
			Identities []pg.AssetIdentity `json:"identities"`
		}](t, raw)
		if len(list.Identities) != 0 {
			t.Fatalf("应允许删空身份，仍有 %d 条", len(list.Identities))
		}
	})
}

// ---------- 企业 attrs（P-B4-22） ----------

func TestB4CompanyAttrsAPI(t *testing.T) {
	h := newInsightsHarness(t)
	slug := fmt.Sprintf("attrs-%d", time.Now().UnixNano())
	company := h.b4createCompany(t, "属性测试企业", slug)

	t.Run("设置与替换 attrs", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID),
			`{"attrs":{"industry":"证券","tags":["a","b"]}}`, http.StatusOK)
		got := decode[pg.Company](t, raw)
		if got.Attrs["industry"] != "证券" {
			t.Fatalf("attrs 未保存: %+v", got.Attrs)
		}
		tags, ok := got.Attrs["tags"].([]any)
		if !ok || len(tags) != 2 {
			t.Fatalf("attrs.tags 解析异常: %+v", got.Attrs)
		}

		// 整体替换：旧键消失。
		raw = h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID),
			`{"attrs":{"level":"A"}}`, http.StatusOK)
		got = decode[pg.Company](t, raw)
		if _, exists := got.Attrs["industry"]; exists || got.Attrs["level"] != "A" {
			t.Fatalf("attrs 未整体替换: %+v", got.Attrs)
		}
	})

	t.Run("与 name 一起更新", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID),
			`{"name":"属性测试企业（改名）","attrs":{"updated":true}}`, http.StatusOK)
		got := decode[pg.Company](t, raw)
		if got.Name != "属性测试企业（改名）" || got.Slug != slug {
			t.Fatalf("name/slug 更新异常: %+v", got)
		}
		if got.Attrs["updated"] != true {
			t.Fatalf("attrs 未一起更新: %+v", got.Attrs)
		}
	})

	t.Run("清空 attrs", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID),
			`{"attrs":null}`, http.StatusOK)
		got := decode[pg.Company](t, raw)
		if len(got.Attrs) != 0 {
			t.Fatalf("attrs 未清空: %+v", got.Attrs)
		}
		// 清空后详情读到的也是空对象而不是 null。
		raw = h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/companies/%d", company.ID), "", http.StatusOK)
		got = decode[pg.Company](t, raw)
		if len(got.Attrs) != 0 {
			t.Fatalf("详情 attrs 非空: %+v", got.Attrs)
		}
	})

	t.Run("非法与不存在", func(t *testing.T) {
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID),
			`{"attrs":[1,2]}`, http.StatusBadRequest)
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID),
			`{"attrs":"字符串"}`, http.StatusBadRequest)
		h.b4(t, http.MethodPatch, "/api/v1/companies/999999999", `{"attrs":{"a":1}}`, http.StatusNotFound)
		// 空 PATCH 仍是幂等无变更。
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", company.ID), `{}`, http.StatusOK)
	})
}

// ---------- 发现 node_id 三态（P-B4-23） ----------

func TestB4FindingNodeIDAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "node_id 测试任务")
	other := h.createMission(t, "另一个任务")

	node, err := h.store.AddNode(ctx, h.tenantA, mission.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateDone, Title: "本任务节点",
	})
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	foreignNode, err := h.store.AddNode(ctx, h.tenantA, other.ID, pg.NodeInput{
		Kind: graph.NodeFact, State: graph.StateDone, Title: "别的任务节点",
	})
	if err != nil {
		t.Fatalf("创建跨任务节点失败: %v", err)
	}
	item := h.b4createFinding(t, mission.ID, "node_id 用例", "high", nil)

	t.Run("设置 node_id", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/findings/%d", item.ID),
			fmt.Sprintf(`{"node_id":%d}`, node.ID), http.StatusOK)
		got := decode[pg.Finding](t, raw)
		if got.NodeID == nil || *got.NodeID != node.ID {
			t.Fatalf("node_id 未设置: %+v", got.NodeID)
		}
	})

	t.Run("其他字段更新不影响 node_id", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/findings/%d", item.ID),
			`{"title":"改名后的发现"}`, http.StatusOK)
		got := decode[pg.Finding](t, raw)
		if got.Title != "改名后的发现" {
			t.Fatalf("标题未更新: %+v", got.Title)
		}
		if got.NodeID == nil || *got.NodeID != node.ID {
			t.Fatalf("未传 node_id 时应保持不变，实际 %+v", got.NodeID)
		}
	})

	t.Run("清空 node_id", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/findings/%d", item.ID),
			`{"node_id":null}`, http.StatusOK)
		if got := decode[pg.Finding](t, raw); got.NodeID != nil {
			t.Fatalf("node_id 未清空: %+v", got.NodeID)
		}
	})

	t.Run("跨任务与非法节点", func(t *testing.T) {
		raw := h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/findings/%d", item.ID),
			fmt.Sprintf(`{"node_id":%d}`, foreignNode.ID), http.StatusBadRequest)
		if code := errorCode(t, raw); code != "bad_request" {
			t.Fatalf("错误码 = %q", code)
		}
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/findings/%d", item.ID),
			`{"node_id":999999999}`, http.StatusBadRequest)
		h.b4(t, http.MethodPatch, fmt.Sprintf("/api/v1/findings/%d", item.ID),
			`{"node_id":"abc"}`, http.StatusBadRequest)
		h.b4(t, http.MethodPatch, "/api/v1/findings/999999999", `{"node_id":1}`, http.StatusNotFound)
	})
}

// ---------- 跨任务发现与 CSV 导出（P-B4-24） ----------

func TestB4FindingsCrossAPI(t *testing.T) {
	h := newInsightsHarness(t)
	m1 := h.createMission(t, "跨任务发现-1")
	m2 := h.createMission(t, "跨任务发现-2")

	assetRec := h.b4createAsset(t, asset.KindSubdomain, "finding.example.com", []asset.Identity{
		{Kind: asset.KindSubdomain, Key: "finding.example.com"},
	})
	// 标题故意包含逗号、引号与公式前缀，用于验证 CSV 转义与防注入。
	f1 := h.b4createFinding(t, m1.ID, `alpha, "quoted"`, "high", nil)
	f2 := h.b4createFinding(t, m1.ID, "=SUM(A1:A2)", "low", nil)
	f3 := h.b4createFinding(t, m2.ID, "third finding", "high", &assetRec.ID)

	// 状态：把 f2 置为 confirmed，覆盖 status 过滤。
	h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/findings/%d/status", f2.ID), `{"status":"confirmed"}`, http.StatusOK)

	t.Run("列表与过滤", func(t *testing.T) {
		all := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet, "/api/v1/findings?limit=200", "", http.StatusOK))
		if len(all.List) != 3 {
			t.Fatalf("全量发现数 = %d，期望 3", len(all.List))
		}
		byMission := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/findings?mission_id=%d", m1.ID), "", http.StatusOK))
		if len(byMission.List) != 2 {
			t.Fatalf("按任务过滤 = %d，期望 2", len(byMission.List))
		}
		bySeverity := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			"/api/v1/findings?severity=high", "", http.StatusOK))
		if len(bySeverity.List) != 2 || bySeverity.List[0].Severity != finding.SeverityHigh {
			t.Fatalf("按严重级别过滤异常: %+v", bySeverity.List)
		}
		byStatus := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			"/api/v1/findings?status=confirmed", "", http.StatusOK))
		if len(byStatus.List) != 1 || byStatus.List[0].ID != f2.ID {
			t.Fatalf("按状态过滤异常: %+v", byStatus.List)
		}
		byAsset := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/findings?asset_id=%d", assetRec.ID), "", http.StatusOK))
		if len(byAsset.List) != 1 || byAsset.List[0].ID != f3.ID {
			t.Fatalf("按资产过滤异常: %+v", byAsset.List)
		}
	})

	t.Run("参数校验", func(t *testing.T) {
		h.b4(t, http.MethodGet, "/api/v1/findings?severity=unknown", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/findings?status=unknown", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/findings?mission_id=abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/findings?after_id=abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/findings?limit=0", "", http.StatusBadRequest)
	})

	t.Run("游标分页", func(t *testing.T) {
		page1 := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet, "/api/v1/findings?limit=2", "", http.StatusOK))
		if len(page1.List) != 2 || page1.Next <= 0 {
			t.Fatalf("第一页异常: len=%d next=%d", len(page1.List), page1.Next)
		}
		page2 := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/findings?limit=2&after_id=%d", page1.Next), "", http.StatusOK))
		if len(page2.List) != 1 || page2.Next != 0 {
			t.Fatalf("第二页异常: len=%d next=%d", len(page2.List), page2.Next)
		}
		seen := map[int64]bool{}
		for _, f := range page1.List {
			seen[f.ID] = true
		}
		for _, f := range page2.List {
			if seen[f.ID] {
				t.Fatalf("分页结果重叠: %d", f.ID)
			}
		}
	})

	t.Run("CSV 导出", func(t *testing.T) {
		status, raw, header := h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/findings/export?format=csv&mission_id=%d", m1.ID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("导出状态码 = %d，响应 %s", status, raw)
		}
		if ct := header.Get("Content-Type"); !strings.Contains(ct, "text/csv") {
			t.Fatalf("Content-Type = %q", ct)
		}
		records, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
		if err != nil {
			t.Fatalf("解析 CSV 失败: %v（内容 %s）", err, raw)
		}
		if len(records) != 3 {
			t.Fatalf("CSV 行数 = %d，期望 表头+2 行：%v", len(records), records)
		}
		if strings.Join(records[0], ",") != "id,mission_id,title,severity,status,asset_id,created_at" {
			t.Fatalf("CSV 表头 = %v", records[0])
		}
		// id 倒序：f2 在后创建，所以第一行是 f2。
		if records[1][0] != fmt.Sprintf("%d", f2.ID) {
			t.Fatalf("CSV 顺序不是 id 倒序: %v", records[1])
		}
		// 公式注入被前缀单引号。
		if !strings.HasPrefix(records[1][2], "'=SUM") {
			t.Fatalf("公式前缀未处理: %q", records[1][2])
		}
		// 含逗号与引号的标题应完整保留。
		found := false
		for _, row := range records[1:] {
			if row[2] == `alpha, "quoted"` {
				found = true
			}
		}
		if !found {
			t.Fatalf("CSV 未保留带逗号引号的标题: %v", records)
		}
		// asset_id 为空的发现输出空串。
		for _, row := range records[1:] {
			if row[0] == fmt.Sprintf("%d", f1.ID) && row[5] != "" {
				t.Fatalf("无资产发现的 asset_id 应为空: %v", row)
			}
		}
	})

	t.Run("空导出只有表头", func(t *testing.T) {
		empty := h.createMission(t, "空发现任务")
		status, raw, _ := h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/findings/export?format=csv&mission_id=%d", empty.ID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d", status)
		}
		records, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
		if err != nil {
			t.Fatalf("解析 CSV 失败: %v", err)
		}
		if len(records) != 1 || strings.Join(records[0], ",") != "id,mission_id,title,severity,status,asset_id,created_at" {
			t.Fatalf("空导出应只有表头: %v", records)
		}
	})

	t.Run("导出参数校验", func(t *testing.T) {
		h.b4(t, http.MethodGet, "/api/v1/findings/export?format=xml", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/findings/export?severity=unknown", "", http.StatusBadRequest)
	})
}

// ---------- 任务范围编译预览（P-B4-25） ----------

func TestB4ScopeCompileAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "范围编译测试")

	entries := []pg.ScopeEntry{
		{Kind: "root_domain", Value: "example.com"},
		{Kind: "subdomain", Value: "api.example.com"},
		{Kind: "url", Value: "https://example.com/admin"},
		{Kind: "ip", Value: "192.0.2.10"},
		{Kind: "cidr", Value: "10.0.0.0/24"},
		{Kind: "keyword", Value: "某集团"},
	}
	if err := h.store.AddMissionScope(ctx, h.tenantA, mission.ID, entries); err != nil {
		t.Fatalf("写入任务范围失败: %v", err)
	}

	t.Run("编译出四类规则", func(t *testing.T) {
		raw := h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/scope/compile", mission.ID), "", http.StatusOK)
		var out struct {
			MissionID     int64                  `json:"mission_id"`
			Compiled      int                    `json:"compiled"`
			CompiledRules []pg.CompiledScopeRule `json:"compiled_rules"`
			Notes         []string               `json:"notes"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("解析编译结果失败: %v（%s）", err, raw)
		}
		if out.MissionID != mission.ID {
			t.Fatalf("mission_id = %d", out.MissionID)
		}
		if out.Compiled != 4 || len(out.CompiledRules) != 4 {
			t.Fatalf("规则数 = %d/%d，期望 4：%+v", out.Compiled, len(out.CompiledRules), out.CompiledRules)
		}
		priorities := map[string]int{}
		tools := map[string]bool{}
		for _, rule := range out.CompiledRules {
			if rule.Action != "allow" || rule.Tool != "http_probe" || rule.ArgPattern == "" || rule.Name == "" {
				t.Fatalf("规则字段不完整: %+v", rule)
			}
			priorities[rule.ArgPattern] = rule.Priority
			tools[rule.Tool] = true
		}
		if !tools["http_probe"] {
			t.Fatal("规则工具应为 http_probe")
		}
		// cidr 与 keyword 无法编译：应各有 1 条 note。
		if len(out.Notes) != 2 {
			t.Fatalf("notes = %d，期望 2：%v", len(out.Notes), out.Notes)
		}
		// 优先级：url 120 > 子域/IP 110 > 根域 100（按规则语义断言，不靠 pattern 子串）。
		priorityCount := map[int]int{}
		for _, rule := range out.CompiledRules {
			priorityCount[rule.Priority]++
			switch {
			case strings.Contains(rule.ArgPattern, "admin"):
				if rule.Priority != 120 {
					t.Fatalf("URL 规则优先级 = %d，期望 120", rule.Priority)
				}
			case strings.Contains(rule.ArgPattern, `([a-z0-9_-]+\.)*`):
				if rule.Priority != 100 {
					t.Fatalf("根域规则优先级 = %d，期望 100", rule.Priority)
				}
			}
		}
		if priorityCount[100] != 1 || priorityCount[110] != 2 || priorityCount[120] != 1 {
			t.Fatalf("优先级分布 = %+v，期望 100×1 / 110×2 / 120×1", priorityCount)
		}
	})

	t.Run("空范围返回空规则与说明", func(t *testing.T) {
		empty := h.createMission(t, "空范围任务")
		raw := h.b4(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/scope/compile", empty.ID), "", http.StatusOK)
		var out struct {
			Compiled      int                    `json:"compiled"`
			CompiledRules []pg.CompiledScopeRule `json:"compiled_rules"`
			Notes         []string               `json:"notes"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if out.Compiled != 0 || len(out.CompiledRules) != 0 {
			t.Fatalf("空范围不应生成规则: %+v", out)
		}
		if len(out.Notes) == 0 {
			t.Fatal("空范围应给出说明性 notes")
		}
		if !strings.Contains(string(raw), `"compiled_rules":[]`) {
			t.Fatalf("空规则应输出 [] 而不是 null：%s", raw)
		}
	})

	t.Run("任务不存在与非法标识", func(t *testing.T) {
		h.b4(t, http.MethodPost, "/api/v1/missions/999999999/scope/compile", "", http.StatusNotFound)
		h.b4(t, http.MethodPost, "/api/v1/missions/abc/scope/compile", "", http.StatusBadRequest)
	})
}

// ---------- 游标分页（P-B4-26） ----------

func TestB4CursorPaginationAPI(t *testing.T) {
	h := newInsightsHarness(t)
	mission := h.createMission(t, "游标分页测试")

	t.Run("资产 id 升序游标", func(t *testing.T) {
		var ids []int64
		mark := fmt.Sprintf("cur-%d", time.Now().UnixNano())
		for i := 0; i < 5; i++ {
			item := h.b4createAsset(t, asset.KindSubdomain, fmt.Sprintf("%s-%d.example.com", mark, i),
				[]asset.Identity{{Kind: asset.KindSubdomain, Key: fmt.Sprintf("%s-%d.example.com", mark, i)}})
			ids = append(ids, item.ID)
		}
		page1 := decode[b4Page[pg.Asset]](t, h.b4(t, http.MethodGet,
			"/api/v1/assets?limit=2&q="+mark, "", http.StatusOK))
		if len(page1.List) != 2 || page1.Next != ids[1] {
			t.Fatalf("第一页异常: len=%d next=%d（ids=%v）", len(page1.List), page1.Next, ids)
		}
		page2 := decode[b4Page[pg.Asset]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/assets?limit=2&q=%s&after_id=%d", mark, page1.Next), "", http.StatusOK))
		if len(page2.List) != 2 || page2.Next != ids[3] {
			t.Fatalf("第二页异常: len=%d next=%d", len(page2.List), page2.Next)
		}
		page3 := decode[b4Page[pg.Asset]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/assets?limit=2&q=%s&after_id=%d", mark, page2.Next), "", http.StatusOK))
		if len(page3.List) != 1 || page3.Next != 0 {
			t.Fatalf("第三页异常: len=%d next=%d", len(page3.List), page3.Next)
		}
		seen := map[int64]bool{}
		for _, list := range [][]pg.Asset{page1.List, page2.List, page3.List} {
			for _, item := range list {
				if seen[item.ID] {
					t.Fatalf("资产分页重叠: %d", item.ID)
				}
				seen[item.ID] = true
			}
		}
		h.b4(t, http.MethodGet, "/api/v1/assets?after_id=abc", "", http.StatusBadRequest)
		h.b4(t, http.MethodGet, "/api/v1/assets?after_id=-1", "", http.StatusBadRequest)
	})

	t.Run("任务发现 id 倒序游标", func(t *testing.T) {
		var ids []int64
		for i := 0; i < 3; i++ {
			item := h.b4createFinding(t, mission.ID, fmt.Sprintf("分页发现-%d", i), "info", nil)
			ids = append(ids, item.ID)
		}
		page1 := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/missions/%d/findings?limit=2", mission.ID), "", http.StatusOK))
		if len(page1.List) != 2 || page1.Next != ids[1] {
			t.Fatalf("第一页异常: len=%d next=%d（ids=%v）", len(page1.List), page1.Next, ids)
		}
		page2 := decode[b4Page[pg.Finding]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/missions/%d/findings?limit=2&after_id=%d", mission.ID, page1.Next),
			"", http.StatusOK))
		if len(page2.List) != 1 || page2.Next != 0 {
			t.Fatalf("第二页异常: len=%d next=%d", len(page2.List), page2.Next)
		}
	})

	t.Run("流量 id 倒序游标", func(t *testing.T) {
		var ids []int64
		for i := 0; i < 3; i++ {
			ids = append(ids, h.b4insertTraffic(t, mission.ID, fmt.Sprintf("tool-%d", i)))
		}
		page1 := decode[b4Page[pg.TrafficExchange]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/traffic?mission_id=%d&limit=2", mission.ID), "", http.StatusOK))
		if len(page1.List) != 2 || page1.Next != ids[1] {
			t.Fatalf("流量第一页异常: len=%d next=%d（ids=%v）", len(page1.List), page1.Next, ids)
		}
		page2 := decode[b4Page[pg.TrafficExchange]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/traffic?mission_id=%d&limit=2&after_id=%d", mission.ID, page1.Next),
			"", http.StatusOK))
		if len(page2.List) != 1 || page2.Next != 0 || page2.List[0].ID != ids[0] {
			t.Fatalf("流量第二页异常: %+v", page2)
		}
	})

	t.Run("命令 id 倒序游标", func(t *testing.T) {
		var ids []int64
		for i := 0; i < 3; i++ {
			ids = append(ids, h.b4insertCommand(t, mission.ID, fmt.Sprintf("cmd-%d", i)))
		}
		page1 := decode[b4Page[pg.AuditEvent]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/commands?mission_id=%d&limit=2", mission.ID), "", http.StatusOK))
		if len(page1.List) != 2 || page1.Next != ids[1] {
			t.Fatalf("命令第一页异常: len=%d next=%d（ids=%v）", len(page1.List), page1.Next, ids)
		}
		page2 := decode[b4Page[pg.AuditEvent]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/commands?mission_id=%d&limit=2&after_id=%d", mission.ID, page1.Next),
			"", http.StatusOK))
		if len(page2.List) != 1 || page2.Next != 0 {
			t.Fatalf("命令第二页异常: %+v", page2)
		}
	})

	t.Run("LLM 记录 id 倒序游标", func(t *testing.T) {
		var ids []int64
		for i := 0; i < 3; i++ {
			ids = append(ids, h.b4insertLLMRecord(t, mission.ID, "worker", fmt.Sprintf("model-%d", i)))
		}
		page1 := decode[b4Page[pg.LLMUsageRecord]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/llm-records?mission_id=%d&limit=2", mission.ID), "", http.StatusOK))
		if len(page1.List) != 2 || page1.Next != ids[1] {
			t.Fatalf("LLM 第一页异常: len=%d next=%d（ids=%v）", len(page1.List), page1.Next, ids)
		}
		page2 := decode[b4Page[pg.LLMUsageRecord]](t, h.b4(t, http.MethodGet,
			fmt.Sprintf("/api/v1/llm-records?mission_id=%d&limit=2&after_id=%d", mission.ID, page1.Next),
			"", http.StatusOK))
		if len(page2.List) != 1 || page2.Next != 0 {
			t.Fatalf("LLM 第二页异常: %+v", page2)
		}
	})

	t.Run("空结果游标为零", func(t *testing.T) {
		empty := h.createMission(t, "空列表任务")
		raw := h.b4(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/findings", empty.ID), "", http.StatusOK)
		page := decode[b4Page[pg.Finding]](t, raw)
		if len(page.List) != 0 || page.Next != 0 {
			t.Fatalf("空列表应返回 next_after_id=0: %+v", page)
		}
	})
}
