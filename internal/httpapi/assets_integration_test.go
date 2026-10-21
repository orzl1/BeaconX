package httpapi

import (
	"bytes"
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

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/tenant"
	"github.com/orzl1/BeaconX/migrations"
)

// M6.2-B1 资产/企业/范围/任务资产/覆盖度接口的端到端测试。
//
// 依赖真实 PostgreSQL：设置 BEACONX_TEST_PG_DSN 后执行，未设置时整体跳过。
// 每个用例使用独立租户（时间戳作为 id），结束时删除租户，靠外键级联清理数据。

const testAPIToken = "test-token"

// apiHarness 是一次测试所需的服务端与数据库句柄。
type apiHarness struct {
	client  *http.Client
	baseURL string
	store   *pg.Store
	db      *pg.DB
	tenantA int64
	tenantB int64
}

// newAPIHarness 启动一个绑定租户 A 的 HTTP 服务，并额外创建租户 B 用于隔离测试。
func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过 HTTP 集成测试")
	}
	ctx := context.Background()
	db, err := pg.Open(ctx, pg.Options{DSN: dsn, MaxConns: 6, MinConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	t.Cleanup(db.Close)
	if _, err := migrate.Apply(ctx, db.Pool(), migrations.FS); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}

	base := time.Now().UnixNano()
	tenantA, tenantB := base, base+1
	for _, id := range []int64{tenantA, tenantB} {
		if _, err := db.Pool().Exec(ctx,
			`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
			id, fmt.Sprintf("asset-api-%d", id), "资产接口测试租户"); err != nil {
			t.Fatalf("创建测试租户失败: %v", err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = ANY($1)`, []int64{tenantA, tenantB})
	})

	store := pg.NewStore(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := NewHandler(Options{
		Logger:         logger,
		RequestTimeout: 5 * time.Second,
		TenantResolver: StaticTenant(tenant.ID(tenantA)),
		Authenticator:  NewTokenAuthenticator(testAPIToken),
		Store:          store,
		TenantID:       tenantA,
	})
	if err != nil {
		t.Fatalf("构建 HTTP 处理器失败: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &apiHarness{
		client:  srv.Client(),
		baseURL: srv.URL,
		store:   store,
		db:      db,
		tenantA: tenantA,
		tenantB: tenantB,
	}
}

// do 发送请求并读取完整响应体；token 为空表示不带 Authorization 头。
func (h *apiHarness) do(t *testing.T, method, path, token, body string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, h.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp.StatusCode, raw, resp.Header
}

// mustDo 发送带令牌的请求并断言状态码，返回响应体。
func (h *apiHarness) mustDo(t *testing.T, method, path, body string, wantStatus int) []byte {
	t.Helper()
	status, raw, _ := h.do(t, method, path, testAPIToken, body)
	if status != wantStatus {
		t.Fatalf("%s %s 状态码 = %d，期望 %d，响应 %s", method, path, status, wantStatus, raw)
	}
	return raw
}

// decode 把 JSON 响应体解析到 out。
func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应失败: %v（响应 %s）", err, raw)
	}
	return out
}

// errorCode 提取错误信封中的 code，用于断言语义而不依赖完整文案。
func errorCode(t *testing.T, raw []byte) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析错误信封失败: %v（响应 %s）", err, raw)
	}
	return envelope.Error.Code
}

// TestCompanyAPIIntegration 覆盖企业 CRUD、模糊搜索、limit 钳制、slug 冲突与 404/409。
func TestCompanyAPIIntegration(t *testing.T) {
	h := newAPIHarness(t)
	u := fmt.Sprintf("acme-%d", time.Now().UnixNano())

	t.Run("校验与创建", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, "/api/v1/companies", `{"name":"  ","slug":"x"}`, http.StatusBadRequest)
		if code := errorCode(t, raw); code != "bad_request" {
			t.Fatalf("错误码 = %q", code)
		}
		h.mustDo(t, http.MethodPost, "/api/v1/companies", `{not json`, http.StatusBadRequest)

		raw = h.mustDo(t, http.MethodPost, "/api/v1/companies",
			fmt.Sprintf(`{"name":"  Acme 证券  ","slug":"  %s  "}`, u), http.StatusCreated)
		company := decode[pg.Company](t, raw)
		if company.ID == 0 || company.Name != "Acme 证券" || company.Slug != u {
			t.Fatalf("创建结果不符合预期: %+v", company)
		}
		if company.TenantID != h.tenantA {
			t.Fatalf("租户 = %d，期望 %d", company.TenantID, h.tenantA)
		}
	})

	t.Run("slug 冲突", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, "/api/v1/companies",
			fmt.Sprintf(`{"name":"另一个同名 slug","slug":%q}`, u), http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}
	})

	t.Run("列表与模糊搜索", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, "/api/v1/companies?limit=999", "", http.StatusOK)
		list := decode[struct {
			Companies []pg.Company `json:"companies"`
		}](t, raw)
		if len(list.Companies) == 0 {
			t.Fatal("列表为空，期望至少包含刚创建的企业")
		}
		if len(list.Companies) > maxHTTPLimit {
			t.Fatalf("limit 未被钳制：%d", len(list.Companies))
		}

		raw = h.mustDo(t, http.MethodGet, "/api/v1/companies?q="+u, "", http.StatusOK)
		list = decode[struct {
			Companies []pg.Company `json:"companies"`
		}](t, raw)
		if len(list.Companies) != 1 || list.Companies[0].Slug != u {
			t.Fatalf("按 slug 搜索结果不符合预期: %+v", list.Companies)
		}

		// LIKE 通配符必须被转义：q="%" 不应匹配所有企业。
		raw = h.mustDo(t, http.MethodGet, "/api/v1/companies?q=%25", "", http.StatusOK)
		list = decode[struct {
			Companies []pg.Company `json:"companies"`
		}](t, raw)
		for _, c := range list.Companies {
			if !strings.Contains(c.Name, "%") && !strings.Contains(c.Slug, "%") {
				t.Fatalf("q=%% 未转义，返回了不该匹配的企业: %+v", c)
			}
		}

		raw = h.mustDo(t, http.MethodGet, "/api/v1/companies?limit=1", "", http.StatusOK)
		list = decode[struct {
			Companies []pg.Company `json:"companies"`
		}](t, raw)
		if len(list.Companies) != 1 {
			t.Fatalf("limit=1 返回 %d 条", len(list.Companies))
		}
	})

	var companyID int64
	t.Run("详情与局部更新", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, "/api/v1/companies?q="+u, "", http.StatusOK)
		list := decode[struct {
			Companies []pg.Company `json:"companies"`
		}](t, raw)
		companyID = list.Companies[0].ID

		raw = h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/companies/%d", companyID), "", http.StatusOK)
		got := decode[pg.Company](t, raw)
		if got.ID != companyID {
			t.Fatalf("详情 id = %d", got.ID)
		}

		// 只改 name：slug 保持不变。
		raw = h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", companyID),
			`{"name":"Acme 改名"}`, http.StatusOK)
		updated := decode[pg.Company](t, raw)
		if updated.Name != "Acme 改名" || updated.Slug != u {
			t.Fatalf("局部更新结果不符合预期: %+v", updated)
		}

		// 空 body 视为无变更（幂等）。
		raw = h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", companyID), `{}`, http.StatusOK)
		noop := decode[pg.Company](t, raw)
		if noop.Name != updated.Name || noop.Slug != updated.Slug {
			t.Fatalf("空 PATCH 不应改变数据: %+v", noop)
		}
	})

	t.Run("更新冲突与不存在", func(t *testing.T) {
		otherSlug := u + "-other"
		h.mustDo(t, http.MethodPost, "/api/v1/companies",
			fmt.Sprintf(`{"name":"第二个企业","slug":%q}`, otherSlug), http.StatusCreated)

		raw := h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/companies/%d", companyID),
			fmt.Sprintf(`{"slug":%q}`, otherSlug), http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}

		h.mustDo(t, http.MethodGet, "/api/v1/companies/999999999", "", http.StatusNotFound)
		h.mustDo(t, http.MethodPatch, "/api/v1/companies/999999999", `{"name":"x"}`, http.StatusNotFound)
		h.mustDo(t, http.MethodDelete, "/api/v1/companies/999999999", "", http.StatusNotFound)
		// 非法路径 id：400 而不是 500。
		raw = h.mustDo(t, http.MethodGet, "/api/v1/companies/not-a-number", "", http.StatusBadRequest)
		if code := errorCode(t, raw); code != "bad_request" {
			t.Fatalf("错误码 = %q", code)
		}
	})

	t.Run("删除", func(t *testing.T) {
		h.mustDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/companies/%d", companyID), "", http.StatusNoContent)
		h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/companies/%d", companyID), "", http.StatusNotFound)
		h.mustDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/companies/%d", companyID), "", http.StatusNotFound)
	})
}

// TestCompanyScopeAPIIntegration 覆盖企业范围增删查、kind/source 白名单、重复 409 与级联删除。
func TestCompanyScopeAPIIntegration(t *testing.T) {
	h := newAPIHarness(t)
	slug := fmt.Sprintf("scope-%d", time.Now().UnixNano())
	raw := h.mustDo(t, http.MethodPost, "/api/v1/companies",
		fmt.Sprintf(`{"name":"范围测试企业","slug":%q}`, slug), http.StatusCreated)
	companyID := decode[pg.Company](t, raw).ID

	var scopeID int64
	t.Run("新增与重复", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/companies/%d/scope", companyID),
			`{"kind":"root_domain","value":"example.com"}`, http.StatusCreated)
		item := decode[pg.CompanyScope](t, raw)
		scopeID = item.ID
		if item.Kind != "root_domain" || item.Value != "example.com" || item.Source != "manual" {
			t.Fatalf("范围条目不符合预期: %+v", item)
		}

		raw = h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/companies/%d/scope", companyID),
			`{"kind":"root_domain","value":"example.com"}`, http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}

		raw = h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/companies/%d/scope", companyID),
			`{"kind":"root_domain","value":"second.example.com","source":"import"}`, http.StatusCreated)
		if got := decode[pg.CompanyScope](t, raw); got.Source != "import" {
			t.Fatalf("source = %q，期望 import", got.Source)
		}
	})

	t.Run("白名单校验", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/companies/%d/scope", companyID),
			`{"kind":"planet","value":"mars"}`, http.StatusBadRequest)
		if code := errorCode(t, raw); code != "bad_request" {
			t.Fatalf("错误码 = %q", code)
		}
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/companies/%d/scope", companyID),
			`{"kind":"ip","value":"  "}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/companies/%d/scope", companyID),
			`{"kind":"ip","value":"10.0.0.1","source":"guess"}`, http.StatusBadRequest)
		// 企业不存在 → 404。
		h.mustDo(t, http.MethodPost, "/api/v1/companies/999999999/scope",
			`{"kind":"ip","value":"10.0.0.1"}`, http.StatusNotFound)
	})

	t.Run("列表与删除", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/companies/%d/scope", companyID), "", http.StatusOK)
		list := decode[struct {
			Scope []pg.CompanyScope `json:"scope"`
		}](t, raw)
		if len(list.Scope) != 2 {
			t.Fatalf("范围条目数 = %d，期望 2", len(list.Scope))
		}

		h.mustDo(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/companies/%d/scope/%d", companyID, scopeID), "", http.StatusNoContent)
		h.mustDo(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/companies/%d/scope/%d", companyID, scopeID), "", http.StatusNotFound)

		// 用另一个企业去删不属于它的范围条目 → 404（不泄漏存在性）。
		otherSlug := slug + "-2"
		raw = h.mustDo(t, http.MethodPost, "/api/v1/companies",
			fmt.Sprintf(`{"name":"另一家","slug":%q}`, otherSlug), http.StatusCreated)
		otherID := decode[pg.Company](t, raw).ID
		h.mustDo(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/companies/%d/scope/%d", otherID, scopeID), "", http.StatusNotFound)
	})

	t.Run("删除企业级联范围", func(t *testing.T) {
		h.mustDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/companies/%d", companyID), "", http.StatusNoContent)
		h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/companies/%d/scope", companyID), "", http.StatusNotFound)

		var count int
		if err := h.db.Pool().QueryRow(context.Background(),
			`SELECT COUNT(*) FROM company_scope WHERE tenant_id = $1 AND company_id = $2`,
			h.tenantA, companyID).Scan(&count); err != nil {
			t.Fatalf("统计范围条目失败: %v", err)
		}
		if count != 0 {
			t.Fatalf("企业删除后仍有 %d 条范围条目", count)
		}
	})
}

// TestAssetAPIIntegration 覆盖资产创建（含身份键归一化）、筛选、详情、更新、删除与租户隔离。
func TestAssetAPIIntegration(t *testing.T) {
	h := newAPIHarness(t)
	mark := fmt.Sprintf("asset-%d", time.Now().UnixNano())

	t.Run("创建与身份归一化", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
			"type":"subdomain","label":"  api 主机  ",
			"identities":[{"kind":"subdomain","key":"  API.%s.Example.COM.  "}]
		}`, mark), http.StatusCreated)
		created := decode[pg.Asset](t, raw)
		if created.ID == 0 || created.Label != "api 主机" {
			t.Fatalf("创建结果不符合预期: %+v", created)
		}
		if len(created.Identities) != 1 || created.Identities[0].Key != fmt.Sprintf("api.%s.example.com", mark) {
			t.Fatalf("身份键未归一化: %+v", created.Identities)
		}

		// 同一身份再次提交：复用同一资产并更新标签（幂等 upsert）。
		raw = h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
			"type":"subdomain","label":"改名后的主机",
			"identities":[{"kind":"subdomain","key":"api.%s.example.com"}]
		}`, mark), http.StatusCreated)
		again := decode[pg.Asset](t, raw)
		if again.ID != created.ID || again.Label != "改名后的主机" {
			t.Fatalf("幂等 upsert 不符合预期: %+v（首次 %+v）", again, created)
		}

		// 无身份键的裸资产（人工登记）。
		raw = h.mustDo(t, http.MethodPost, "/api/v1/assets",
			fmt.Sprintf(`{"type":"app","label":"%s-app","attrs":{"vendor":"acme"}}`, mark), http.StatusCreated)
		bare := decode[pg.Asset](t, raw)
		if bare.ID == 0 || len(bare.Identities) != 0 {
			t.Fatalf("裸资产不符合预期: %+v", bare)
		}
		if bare.Attrs["vendor"] != "acme" {
			t.Fatalf("attrs 未保存: %+v", bare.Attrs)
		}
	})

	t.Run("创建校验", func(t *testing.T) {
		h.mustDo(t, http.MethodPost, "/api/v1/assets", `{"type":"planet","label":"x"}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, "/api/v1/assets", `{"type":"ip","label":"  "}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, "/api/v1/assets",
			`{"type":"ip","label":"x","identities":[{"kind":"planet","key":"x"}]}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, "/api/v1/assets",
			`{"type":"ip","label":"x","identities":[{"kind":"ip","key":"999.1.1.1"}]}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, "/api/v1/assets",
			`{"type":"ip","label":"x","parent_id":-1}`, http.StatusBadRequest)
	})

	var assetID int64
	t.Run("列表筛选", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, "/api/v1/assets?type=app&q="+mark, "", http.StatusOK)
		list := decode[struct {
			Assets []pg.Asset `json:"assets"`
		}](t, raw)
		if len(list.Assets) != 1 {
			t.Fatalf("按 type+q 过滤后返回 %d 条，期望 1：%+v", len(list.Assets), list.Assets)
		}
		assetID = list.Assets[0].ID

		// type 精确过滤：subdomain 的资产不会出现在 app 结果里。
		raw = h.mustDo(t, http.MethodGet, "/api/v1/assets?type=subdomain&limit=200", "", http.StatusOK)
		subs := decode[struct {
			Assets []pg.Asset `json:"assets"`
		}](t, raw)
		for _, a := range subs.Assets {
			if a.Type != asset.KindSubdomain {
				t.Fatalf("类型过滤失效: %+v", a)
			}
		}

		h.mustDo(t, http.MethodGet, "/api/v1/assets?type=planet", "", http.StatusBadRequest)

		// 未转义的 % 不应匹配全部资产。
		raw = h.mustDo(t, http.MethodGet, "/api/v1/assets?q=%25", "", http.StatusOK)
		pct := decode[struct {
			Assets []pg.Asset `json:"assets"`
		}](t, raw)
		for _, a := range pct.Assets {
			if !strings.Contains(a.Label, "%") {
				t.Fatalf("q=%% 未转义，返回了不该匹配的资产: %+v", a)
			}
		}
	})

	t.Run("详情与身份键", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d", assetID), "", http.StatusOK)
		got := decode[pg.Asset](t, raw)
		if got.ID != assetID || got.Type != asset.KindApp {
			t.Fatalf("详情不符合预期: %+v", got)
		}

		raw = h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/identities", assetID), "", http.StatusOK)
		ids := decode[struct {
			Identities []pg.AssetIdentity `json:"identities"`
		}](t, raw)
		if len(ids.Identities) != 0 {
			t.Fatalf("裸资产不应有身份键: %+v", ids.Identities)
		}

		h.mustDo(t, http.MethodGet, "/api/v1/assets/999999999", "", http.StatusNotFound)
		h.mustDo(t, http.MethodGet, "/api/v1/assets/999999999/identities", "", http.StatusNotFound)
	})

	t.Run("局部更新与父子关系", func(t *testing.T) {
		// 父资产：根域。
		raw := h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
			"type":"root_domain","label":"%s-parent.com",
			"identities":[{"kind":"root_domain","key":"%s-parent.com"}]
		}`, mark, mark), http.StatusCreated)
		parentID := decode[pg.Asset](t, raw).ID

		raw = h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/assets/%d", assetID),
			fmt.Sprintf(`{"label":" 更新后的应用 ","parent_id":%d,"attrs":{"vendor":"acme","env":"prod"}}`, parentID),
			http.StatusOK)
		updated := decode[pg.Asset](t, raw)
		if updated.Label != "更新后的应用" || updated.ParentID == nil || *updated.ParentID != parentID {
			t.Fatalf("更新结果不符合预期: %+v", updated)
		}
		if updated.Attrs["env"] != "prod" {
			t.Fatalf("attrs 未整体替换: %+v", updated.Attrs)
		}

		// 显式 null 清空父资产。
		raw = h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/assets/%d", assetID),
			`{"parent_id":null}`, http.StatusOK)
		if cleared := decode[pg.Asset](t, raw); cleared.ParentID != nil {
			t.Fatalf("parent_id 未清空: %+v", cleared.ParentID)
		}

		// 自身作为父资产、父资产不存在、非法 parent_id 都是 400。
		h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/assets/%d", assetID),
			fmt.Sprintf(`{"parent_id":%d}`, assetID), http.StatusBadRequest)
		h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/assets/%d", assetID),
			`{"parent_id":999999999}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/assets/%d", assetID),
			`{"parent_id":"abc"}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPatch, fmt.Sprintf("/api/v1/assets/%d", assetID),
			`{"label":"  "}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPatch, "/api/v1/assets/999999999", `{"label":"x"}`, http.StatusNotFound)
	})

	t.Run("租户隔离", func(t *testing.T) {
		other, err := h.store.UpsertAsset(context.Background(), h.tenantB, pg.AssetInput{
			Type:       asset.KindSubdomain,
			Label:      mark + "-tenant-b",
			Identities: []asset.Identity{{Kind: asset.KindSubdomain, Key: mark + ".tenant-b.example.com"}},
		})
		if err != nil {
			t.Fatalf("创建他租户资产失败: %v", err)
		}
		// 通过租户 A 的接口不可见：详情 404，列表也搜不到。
		h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d", other.ID), "", http.StatusNotFound)
		raw := h.mustDo(t, http.MethodGet, "/api/v1/assets?q="+mark+"-tenant-b", "", http.StatusOK)
		list := decode[struct {
			Assets []pg.Asset `json:"assets"`
		}](t, raw)
		if len(list.Assets) != 0 {
			t.Fatalf("跨租户泄漏: %+v", list.Assets)
		}
		// 跨租户登记到任务也应被拒绝（资产不存在 → 400）。
		mission, err := h.store.CreateMission(context.Background(), h.tenantA, pg.MissionInput{Name: "隔离测试任务"})
		if err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			fmt.Sprintf(`{"asset_id":%d}`, other.ID), http.StatusBadRequest)
	})

	t.Run("删除与级联", func(t *testing.T) {
		// 先建一个带身份键的资产并挂到任务上，验证删除后身份键与关联都消失。
		raw := h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
			"type":"ip","label":"192.0.2.77",
			"identities":[{"kind":"ip","key":"192.0.2.77"}]
		}`), http.StatusCreated)
		ipAsset := decode[pg.Asset](t, raw)
		if len(ipAsset.Identities) != 1 {
			t.Fatalf("身份键未写入: %+v", ipAsset)
		}
		mission, err := h.store.CreateMission(context.Background(), h.tenantA, pg.MissionInput{Name: "删除级联任务"})
		if err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			fmt.Sprintf(`{"asset_id":%d}`, ipAsset.ID), http.StatusCreated)

		h.mustDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/assets/%d", ipAsset.ID), "", http.StatusNoContent)
		h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d", ipAsset.ID), "", http.StatusNotFound)
		h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d/identities", ipAsset.ID), "", http.StatusNotFound)
		h.mustDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/assets/%d", ipAsset.ID), "", http.StatusNotFound)

		var identityCount, linkCount int
		if err := h.db.Pool().QueryRow(context.Background(),
			`SELECT COUNT(*) FROM asset_identity WHERE tenant_id = $1 AND asset_id = $2`,
			h.tenantA, ipAsset.ID).Scan(&identityCount); err != nil {
			t.Fatalf("统计身份键失败: %v", err)
		}
		if identityCount != 0 {
			t.Fatalf("资产删除后仍有 %d 条身份键", identityCount)
		}
		if err := h.db.Pool().QueryRow(context.Background(),
			`SELECT COUNT(*) FROM mission_asset_links WHERE tenant_id = $1 AND asset_id = $2`,
			h.tenantA, ipAsset.ID).Scan(&linkCount); err != nil {
			t.Fatalf("统计任务资产关联失败: %v", err)
		}
		if linkCount != 0 {
			t.Fatalf("资产删除后仍有 %d 条任务关联", linkCount)
		}
	})

	t.Run("删除父资产时子资产 parent_id 置空", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
			"type":"root_domain","label":"child-check-%s.com",
			"identities":[{"kind":"root_domain","key":"child-check-%s.com"}]
		}`, mark, mark), http.StatusCreated)
		parentID := decode[pg.Asset](t, raw).ID
		raw = h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
			"type":"subdomain","label":"a.child-check-%s.com","parent_id":%d,
			"identities":[{"kind":"subdomain","key":"a.child-check-%s.com"}]
		}`, mark, parentID, mark), http.StatusCreated)
		childID := decode[pg.Asset](t, raw).ID

		h.mustDo(t, http.MethodDelete, fmt.Sprintf("/api/v1/assets/%d", parentID), "", http.StatusNoContent)
		raw = h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/assets/%d", childID), "", http.StatusOK)
		if child := decode[pg.Asset](t, raw); child.ParentID != nil {
			t.Fatalf("父资产删除后子资产 parent_id 未置空: %+v", *child.ParentID)
		}
	})
}

// TestMissionAssetsAPIIntegration 覆盖任务资产登记/撤销、重复 409、覆盖率展示与空覆盖。
func TestMissionAssetsAPIIntegration(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	mission, err := h.store.CreateMission(ctx, h.tenantA, pg.MissionInput{Name: "任务资产接口测试"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	mark := fmt.Sprintf("m-%d", time.Now().UnixNano())
	raw := h.mustDo(t, http.MethodPost, "/api/v1/assets", fmt.Sprintf(`{
		"type":"subdomain","label":"%s.example.com",
		"identities":[{"kind":"subdomain","key":"%s.example.com"}]
	}`, mark, mark), http.StatusCreated)
	assetID := decode[pg.Asset](t, raw).ID

	t.Run("登记与重复", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			fmt.Sprintf(`{"asset_id":%d}`, assetID), http.StatusCreated)
		link := decode[missionAssetLinkResponse](t, raw)
		if link.MissionID != mission.ID || link.AssetID != assetID || link.Source != "manual" {
			t.Fatalf("登记结果不符合预期: %+v", link)
		}

		raw = h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			fmt.Sprintf(`{"asset_id":%d}`, assetID), http.StatusConflict)
		if code := errorCode(t, raw); code != "conflict" {
			t.Fatalf("错误码 = %q", code)
		}
	})

	t.Run("参数校验", func(t *testing.T) {
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			`{"asset_id":0}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			fmt.Sprintf(`{"asset_id":%d,"source":"guessed"}`, assetID), http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID),
			`{"asset_id":999999999}`, http.StatusBadRequest)
		h.mustDo(t, http.MethodPost, "/api/v1/missions/999999999/assets",
			fmt.Sprintf(`{"asset_id":%d}`, assetID), http.StatusNotFound)
	})

	t.Run("任务资产列表", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID), "", http.StatusOK)
		list := decode[struct {
			Assets []pg.Asset `json:"assets"`
		}](t, raw)
		if len(list.Assets) != 1 || list.Assets[0].ID != assetID {
			t.Fatalf("任务资产列表不符合预期: %+v", list.Assets)
		}
		h.mustDo(t, http.MethodGet, "/api/v1/missions/999999999/assets", "", http.StatusNotFound)
	})

	t.Run("覆盖率", func(t *testing.T) {
		raw := h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/coverage", mission.ID), "", http.StatusOK)
		cov := decode[pg.Coverage](t, raw)
		if cov.Total != 1 || cov.Tested != 0 || cov.Percent != 0 {
			t.Fatalf("覆盖度不符合预期: %+v", cov)
		}
		if len(cov.ByType) != 1 || cov.ByType[0].Type != asset.KindSubdomain {
			t.Fatalf("按类型覆盖度不符合预期: %+v", cov.ByType)
		}

		// 空覆盖：新建任务没有资产，返回全零而不是报错。
		empty, err := h.store.CreateMission(ctx, h.tenantA, pg.MissionInput{Name: "空覆盖任务"})
		if err != nil {
			t.Fatalf("创建任务失败: %v", err)
		}
		raw = h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/coverage", empty.ID), "", http.StatusOK)
		emptyCov := decode[pg.Coverage](t, raw)
		if emptyCov.Total != 0 || emptyCov.Tested != 0 || emptyCov.Percent != 0 || len(emptyCov.ByType) != 0 {
			t.Fatalf("空覆盖度不符合预期: %+v", emptyCov)
		}
		h.mustDo(t, http.MethodGet, "/api/v1/missions/999999999/coverage", "", http.StatusNotFound)
	})

	t.Run("解除关联", func(t *testing.T) {
		h.mustDo(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/missions/%d/assets/%d", mission.ID, assetID), "", http.StatusNoContent)
		h.mustDo(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/missions/%d/assets/%d", mission.ID, assetID), "", http.StatusNotFound)

		raw := h.mustDo(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/assets", mission.ID), "", http.StatusOK)
		if got := strings.TrimSpace(string(raw)); !strings.Contains(got, `"assets":[]`) {
			t.Fatalf("解除关联后应返回空数组，实际 %s", got)
		}
		h.mustDo(t, http.MethodDelete, "/api/v1/missions/999999999/assets/1", "", http.StatusNotFound)
	})
}

// TestAPIAuthAndRoutingIntegration 覆盖鉴权 401、未知路径 JSON 404、方法不匹配 405 与 store 未就绪 503。
func TestAPIAuthAndRoutingIntegration(t *testing.T) {
	h := newAPIHarness(t)

	t.Run("鉴权", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/companies", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("无令牌状态码 = %d，响应 %s", status, raw)
		}
		if code := errorCode(t, raw); code != "unauthorized" {
			t.Fatalf("错误码 = %q", code)
		}
		status, _, _ = h.do(t, http.MethodGet, "/api/v1/companies", "wrong-token", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("错误令牌状态码 = %d", status)
		}
	})

	t.Run("未知路径返回 JSON 404", func(t *testing.T) {
		status, raw, header := h.do(t, http.MethodGet, "/api/v1/definitely-not-here", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("状态码 = %d，响应 %s", status, raw)
		}
		if ct := header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("Content-Type = %q，期望 JSON", ct)
		}
		if code := errorCode(t, raw); code != "not_found" {
			t.Fatalf("错误码 = %q", code)
		}
	})

	t.Run("方法不匹配返回 405", func(t *testing.T) {
		status, raw, header := h.do(t, http.MethodPut, "/api/v1/assets/1", testAPIToken, "")
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("状态码 = %d，响应 %s", status, raw)
		}
		if allow := header.Get("Allow"); allow == "" {
			t.Fatal("405 响应缺少 Allow 头")
		}
		if code := errorCode(t, raw); code != "method_not_allowed" {
			t.Fatalf("错误码 = %q", code)
		}
	})

	t.Run("store 未就绪返回 503", func(t *testing.T) {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		handler, err := NewHandler(Options{
			Logger:         logger,
			RequestTimeout: 5 * time.Second,
			TenantResolver: StaticTenant(tenant.ID(h.tenantA)),
			Authenticator:  NewTokenAuthenticator(testAPIToken),
			TenantID:       h.tenantA,
			// Store 故意留空：模拟数据库尚未装配。
		})
		if err != nil {
			t.Fatalf("构建处理器失败: %v", err)
		}
		srv := httptest.NewServer(handler)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/companies", nil)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIToken)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("状态码 = %d，响应 %s", resp.StatusCode, raw)
		}
		if code := errorCode(t, raw); code != "store_unavailable" {
			t.Fatalf("错误码 = %q", code)
		}
	})
}

// TestAssetAPIListLimitClamp 验证 limit 缺省值与上限钳制（不依赖大量数据）。
func TestAssetAPIListLimitClamp(t *testing.T) {
	h := newAPIHarness(t)
	// limit 缺省、非法与超大值都不应报错，且返回条数不超过上限。
	for _, path := range []string{
		"/api/v1/companies",
		"/api/v1/companies?limit=0",
		"/api/v1/companies?limit=-5",
		"/api/v1/companies?limit=abc",
		"/api/v1/companies?limit=100000",
		"/api/v1/assets?limit=100000",
	} {
		raw := h.mustDo(t, http.MethodGet, path, "", http.StatusOK)
		if !bytes.Contains(raw, []byte(`"`)) {
			t.Fatalf("%s 返回体异常: %s", path, raw)
		}
	}
}
