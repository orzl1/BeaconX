package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/tenant"
	"github.com/orzl1/BeaconX/migrations"
)

// M6.2-B5b Agent 定义 + 工作区文件接口的端到端测试。
//
// 依赖真实 PostgreSQL 与本地临时状态目录：未设置 BEACONX_TEST_PG_DSN 时整体跳过。
// 工作区根目录通过 t.Setenv("BEACONX_HOME", t.TempDir()) 注入，避免污染仓库内
// 的 data/ 目录。所有 helper 使用 b5agents 前缀，与并行开发的其它测试区分。

type b5agentsHarness struct {
	client    *http.Client
	baseURL   string
	store     *pg.Store
	db        *pg.DB
	tenantA   int64
	tenantB   int64
	stateHome string
}

// b5agentsNewHarness 启动绑定租户 A 的服务，并创建租户 B 用于隔离测试。
func b5agentsNewHarness(t *testing.T) *b5agentsHarness {
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
			id, fmt.Sprintf("b5b-api-%d", id), "Agent/工作区测试租户"); err != nil {
			t.Fatalf("创建测试租户失败: %v", err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = ANY($1)`, []int64{tenantA, tenantB})
	})

	stateHome := t.TempDir()
	t.Setenv("BEACONX_HOME", stateHome)

	h := &b5agentsHarness{
		store: pg.NewStore(db), db: db,
		tenantA: tenantA, tenantB: tenantB,
		stateHome: stateHome,
	}
	srv := h.newServer(t, tenantA, true)
	h.client = srv.Client()
	h.baseURL = srv.URL
	return h
}

// newServer 为指定租户构建 HTTP 服务；withStore=false 用于测试 503 分支。
func (h *b5agentsHarness) newServer(t *testing.T, tenantID int64, withStore bool) *httptest.Server {
	t.Helper()
	opts := Options{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestTimeout: 5 * time.Second,
		TenantResolver: StaticTenant(tenant.ID(tenantID)),
		Authenticator:  NewTokenAuthenticator(testAPIToken),
		TenantID:       tenantID,
	}
	if withStore {
		opts.Store = h.store
	}
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("构建 HTTP 处理器失败: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// do 发送 JSON/文本请求；token 为空表示不带 Authorization 头。
func (h *b5agentsHarness) do(t *testing.T, method, path, token, body string) (int, []byte, http.Header) {
	t.Helper()
	return b5agentsDoRaw(t, h.client, h.baseURL, method, path, token, strings.NewReader(body), "application/json")
}

// b5agentsDoRaw 发送自定义 body/content-type 的请求。
func b5agentsDoRaw(t *testing.T, client *http.Client, baseURL, method, path, token string, body io.Reader, contentType string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
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

// b5agentsDecode 解码响应体。
func b5agentsDecode(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v（body=%s）", err, raw)
	}
}

// b5agentsArray 断言响应顶层字段是数组而不是 null。
func b5agentsArray(t *testing.T, raw []byte, field string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	b5agentsDecode(t, raw, &envelope)
	value, ok := envelope[field]
	if !ok {
		t.Fatalf("响应缺少字段 %q（body=%s）", field, raw)
	}
	if strings.TrimSpace(string(value)) == "null" {
		t.Fatalf("字段 %q 不应为 null（body=%s）", field, raw)
	}
}

// b5agentsCreateMission 在当前租户下创建任务。
func (h *b5agentsHarness) createMission(t *testing.T, tenantID int64, name string) pg.Mission {
	t.Helper()
	mission, err := h.store.CreateMission(context.Background(), tenantID, pg.MissionInput{Name: name})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	return mission
}

// b5agentsMultipart 构造 multipart 上传请求。
func b5agentsMultipart(t *testing.T, fieldName, filename string, data []byte, extra map[string]string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for key, value := range extra {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("写入表单字段失败: %v", err)
		}
	}
	part, err := writer.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("创建 multipart 文件字段失败: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("写入 multipart 内容失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	return &buf, writer.FormDataContentType()
}

// ---------- 类型 ----------

type b5agentsAgentJSON struct {
	ID           int64    `json:"id"`
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	SystemPrompt string   `json:"system_prompt"`
	Tools        []string `json:"tools"`
	Model        string   `json:"model"`
	Enabled      bool     `json:"enabled"`
}

type b5agentsAgentListJSON struct {
	Agents []b5agentsAgentJSON `json:"agents"`
}

type b5agentsFileJSON struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	IsDir      bool   `json:"is_dir"`
}

type b5agentsListJSON struct {
	Files []b5agentsFileJSON `json:"files"`
	Root  string             `json:"root"`
}

// ---------- Agents CRUD ----------

func TestB5AgentsAPI(t *testing.T) {
	h := b5agentsNewHarness(t)

	t.Run("空列表", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/agents", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		b5agentsArray(t, raw, "agents")
		var list b5agentsAgentListJSON
		b5agentsDecode(t, raw, &list)
		if len(list.Agents) != 0 {
			t.Fatalf("空租户 Agent 列表应为空，实际 %+v", list.Agents)
		}
	})

	t.Run("创建与校验", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken,
			`{"key":"recon-agent","name":"侦察 Agent","role":"worker","system_prompt":"你是侦察员","tools":["http_probe","shell_exec"],"model":"m1","enabled":true}`)
		if status != http.StatusCreated {
			t.Fatalf("创建状态码 = %d，期望 201（body=%s）", status, raw)
		}
		var created b5agentsAgentJSON
		b5agentsDecode(t, raw, &created)
		if created.ID <= 0 || created.Key != "recon-agent" || created.Role != "worker" {
			t.Fatalf("创建响应不符合预期: %+v", created)
		}
		if len(created.Tools) != 2 || created.Tools[0] != "http_probe" {
			t.Fatalf("工具列表未保留顺序: %+v", created.Tools)
		}

		// 默认 role/enabled。
		status, raw, _ = h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken, `{"key":"default-agent"}`)
		if status != http.StatusCreated {
			t.Fatalf("默认创建状态码 = %d，期望 201（body=%s）", status, raw)
		}
		var defaultAgent b5agentsAgentJSON
		b5agentsDecode(t, raw, &defaultAgent)
		if defaultAgent.Role != "worker" || !defaultAgent.Enabled || len(defaultAgent.Tools) != 0 {
			t.Fatalf("默认值不符合预期: %+v", defaultAgent)
		}

		// key 重复。
		status, raw, _ = h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken, `{"key":"recon-agent"}`)
		if status != http.StatusConflict {
			t.Fatalf("重复 key 状态码 = %d，期望 409（body=%s）", status, raw)
		}

		for _, body := range []string{
			`{"key":"BadKey"}`,
			`{"key":"-bad"}`,
			`{"key":""}`,
			`{"key":"ok-agent","role":"hacker"}`,
			`{"key":"ok-agent","tools":[""]}`,
			`{"key":"ok-agent","tools":"http_probe"}`,
			fmt.Sprintf(`{"key":%q}`, strings.Repeat("a", 65)),
		} {
			status, raw, _ := h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken, body)
			if status != http.StatusBadRequest {
				t.Fatalf("非法创建体 %s 状态码 = %d，期望 400（body=%s）", body, status, raw)
			}
		}
	})

	t.Run("列表与过滤", func(t *testing.T) {
		// 再造一个停用 Agent。
		status, raw, _ := h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken,
			`{"key":"disabled-agent","role":"retester","enabled":false}`)
		if status != http.StatusCreated {
			t.Fatalf("创建停用 Agent 状态码 = %d，期望 201（body=%s）", status, raw)
		}

		cases := []struct {
			name string
			path string
			want int
		}{
			{name: "全部", path: "/api/v1/agents", want: 3},
			{name: "按角色", path: "/api/v1/agents?role=worker", want: 2},
			{name: "启用中", path: "/api/v1/agents?enabled=true", want: 2},
			{name: "停用", path: "/api/v1/agents?enabled=false", want: 1},
			{name: "limit", path: "/api/v1/agents?limit=1", want: 1},
		}
		for _, c := range cases {
			status, raw, _ := h.do(t, http.MethodGet, c.path, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("%s 状态码 = %d，期望 200（body=%s）", c.name, status, raw)
			}
			var list b5agentsAgentListJSON
			b5agentsDecode(t, raw, &list)
			if len(list.Agents) != c.want {
				t.Fatalf("%s 条数 = %d，期望 %d", c.name, len(list.Agents), c.want)
			}
		}

		for _, path := range []string{
			"/api/v1/agents?role=hacker",
			"/api/v1/agents?enabled=maybe",
			"/api/v1/agents?limit=0",
		} {
			status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
			if status != http.StatusBadRequest {
				t.Fatalf("路径 %s 状态码 = %d，期望 400（body=%s）", path, status, raw)
			}
		}
	})

	t.Run("详情与更新", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/agents/recon-agent", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("详情状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var agent b5agentsAgentJSON
		b5agentsDecode(t, raw, &agent)
		if agent.Key != "recon-agent" {
			t.Fatalf("详情不符合预期: %+v", agent)
		}

		status, raw, _ = h.do(t, http.MethodPatch, "/api/v1/agents/recon-agent", testAPIToken,
			`{"name":"改名后的侦察","enabled":false,"tools":["shell_exec"]}`)
		if status != http.StatusOK {
			t.Fatalf("更新状态码 = %d，期望 200（body=%s）", status, raw)
		}
		b5agentsDecode(t, raw, &agent)
		if agent.Name != "改名后的侦察" || agent.Enabled || len(agent.Tools) != 1 {
			t.Fatalf("更新结果不符合预期: %+v", agent)
		}

		// key 不可修改。
		status, raw, _ = h.do(t, http.MethodPatch, "/api/v1/agents/recon-agent", testAPIToken,
			`{"key":"another-key"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("改 key 状态码 = %d，期望 400（body=%s）", status, raw)
		}

		// 非法角色。
		status, raw, _ = h.do(t, http.MethodPatch, "/api/v1/agents/recon-agent", testAPIToken,
			`{"role":"hacker"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("非法角色状态码 = %d，期望 400（body=%s）", status, raw)
		}

		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/agents/missing-agent", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("不存在 Agent 状态码 = %d，期望 404（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/agents/BadKey", testAPIToken, "")
		if status != http.StatusBadRequest {
			t.Fatalf("非法 key 状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("静态校验", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken,
			`{"key":"check-agent","tools":["http_probe","unknown_tool"]}`)
		if status != http.StatusCreated {
			t.Fatalf("创建状态码 = %d，期望 201（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodPost, "/api/v1/agents/check-agent/test", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("校验状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var result struct {
			Key          string   `json:"key"`
			ValidTools   []string `json:"valid_tools"`
			UnknownTools []string `json:"unknown_tools"`
			Warnings     []string `json:"warnings"`
		}
		b5agentsDecode(t, raw, &result)
		if result.Key != "check-agent" || len(result.ValidTools) != 1 || result.ValidTools[0] != "http_probe" {
			t.Fatalf("校验结果不符合预期: %+v", result)
		}
		if len(result.UnknownTools) != 1 || result.UnknownTools[0] != "unknown_tool" {
			t.Fatalf("未知工具列表不符合预期: %+v", result.UnknownTools)
		}
		if len(result.Warnings) == 0 {
			t.Fatalf("应给出 warnings: %+v", result)
		}

		status, raw, _ = h.do(t, http.MethodPost, "/api/v1/agents/missing-agent/test", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("不存在 Agent 校验状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("删除", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodDelete, "/api/v1/agents/default-agent", testAPIToken, "")
		if status != http.StatusNoContent {
			t.Fatalf("删除状态码 = %d，期望 204（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/agents/default-agent", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("删除后状态码 = %d，期望 404（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodDelete, "/api/v1/agents/default-agent", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("重复删除状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/agents", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// TestB5AgentsCrossTenant 校验 Agent 定义按租户隔离。
func TestB5AgentsCrossTenant(t *testing.T) {
	h := b5agentsNewHarness(t)
	ctx := context.Background()
	if _, err := h.store.CreateAgent(ctx, h.tenantB, pg.AgentInput{
		Key: "tenant-b-agent", Name: "B 的 Agent", Role: pg.AgentRoleWorker, Enabled: true,
	}); err != nil {
		t.Fatalf("创建租户 B Agent 失败: %v", err)
	}

	status, raw, _ := h.do(t, http.MethodGet, "/api/v1/agents", testAPIToken, "")
	if status != http.StatusOK {
		t.Fatalf("列表状态码 = %d，期望 200（body=%s）", status, raw)
	}
	var list b5agentsAgentListJSON
	b5agentsDecode(t, raw, &list)
	if len(list.Agents) != 0 {
		t.Fatalf("租户 A 不应看到租户 B 的 Agent: %+v", list.Agents)
	}
	status, raw, _ = h.do(t, http.MethodGet, "/api/v1/agents/tenant-b-agent", testAPIToken, "")
	if status != http.StatusNotFound {
		t.Fatalf("跨租户详情状态码 = %d，期望 404（body=%s）", status, raw)
	}

	// 同一 key 在租户 A 可以单独创建。
	status, raw, _ = h.do(t, http.MethodPost, "/api/v1/agents", testAPIToken, `{"key":"tenant-b-agent"}`)
	if status != http.StatusCreated {
		t.Fatalf("租户 A 同 key 创建状态码 = %d，期望 201（body=%s）", status, raw)
	}
}

// ---------- Workspace ----------

func TestB5WorkspaceAPI(t *testing.T) {
	h := b5agentsNewHarness(t)
	mission := h.createMission(t, h.tenantA, "工作区测试任务")
	base := fmt.Sprintf("/api/v1/workspace/files?mission_id=%d", mission.ID)

	t.Run("空列表与 root", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, base, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		b5agentsArray(t, raw, "files")
		var list b5agentsListJSON
		b5agentsDecode(t, raw, &list)
		if len(list.Files) != 0 {
			t.Fatalf("空工作区应无文件，实际 %+v", list.Files)
		}
		if list.Root != fmt.Sprintf("missions/%d", mission.ID) {
			t.Fatalf("root = %q，期望 missions/%d", list.Root, mission.ID)
		}
	})

	t.Run("写入/读取/列表", func(t *testing.T) {
		filePath := fmt.Sprintf("/api/v1/workspace/files/notes/a.txt?mission_id=%d", mission.ID)
		status, raw, _ := h.do(t, http.MethodPut, filePath, testAPIToken, "hello workspace")
		if status != http.StatusOK {
			t.Fatalf("写入状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var written struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		b5agentsDecode(t, raw, &written)
		if written.Path != "notes/a.txt" || written.Size != int64(len("hello workspace")) {
			t.Fatalf("写入响应不符合预期: %+v", written)
		}

		status, raw, _ = h.do(t, http.MethodGet, filePath, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("读取状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var read struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		b5agentsDecode(t, raw, &read)
		if read.Content != "hello workspace" || read.Path != "notes/a.txt" {
			t.Fatalf("读取内容不符合预期: %+v", read)
		}

		// 递归列表：应包含目录与文件。
		status, raw, _ = h.do(t, http.MethodGet, base, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("列表状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var list b5agentsListJSON
		b5agentsDecode(t, raw, &list)
		paths := map[string]b5agentsFileJSON{}
		for _, item := range list.Files {
			paths[item.Path] = item
		}
		if entry, ok := paths["notes/a.txt"]; !ok || entry.IsDir || entry.Size != int64(len("hello workspace")) {
			t.Fatalf("列表缺少文件 entry: %+v", list.Files)
		}
		if entry, ok := paths["notes"]; !ok || !entry.IsDir {
			t.Fatalf("列表缺少目录 entry: %+v", list.Files)
		}

		// limit 截断。
		status, raw, _ = h.do(t, http.MethodGet, base+"&limit=1", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("limit 状态码 = %d，期望 200（body=%s）", status, raw)
		}
		b5agentsDecode(t, raw, &list)
		if len(list.Files) != 1 {
			t.Fatalf("limit=1 应返回 1 项，实际 %d", len(list.Files))
		}
	})

	t.Run("路径穿越与二进制拒绝", func(t *testing.T) {
		encoded := "%2e%2e%2fescape.txt"
		cases := []struct{ method, path, body string }{
			{http.MethodGet, fmt.Sprintf("/api/v1/workspace/files/%s?mission_id=%d", encoded, mission.ID), ""},
			{http.MethodPut, fmt.Sprintf("/api/v1/workspace/files/%s?mission_id=%d", encoded, mission.ID), "x"},
			{http.MethodDelete, fmt.Sprintf("/api/v1/workspace/files/%s?mission_id=%d", encoded, mission.ID), ""},
			{http.MethodGet, fmt.Sprintf("/api/v1/workspace/files?mission_id=%d&path=..%%2F..%%2Fetc", mission.ID), ""},
		}
		for _, c := range cases {
			status, raw, _ := h.do(t, c.method, c.path, testAPIToken, c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("%s %s 状态码 = %d，期望 400（body=%s）", c.method, c.path, status, raw)
			}
		}

		// NUL 与非法 UTF-8 都按二进制拒绝。
		writePath := fmt.Sprintf("/api/v1/workspace/files/bin.dat?mission_id=%d", mission.ID)
		status, raw, _ := h.do(t, http.MethodPut, writePath, testAPIToken, "a\x00b")
		if status != http.StatusBadRequest {
			t.Fatalf("NUL 写入状态码 = %d，期望 400（body=%s）", status, raw)
		}
		status, raw, _ = b5agentsDoRaw(t, h.client, h.baseURL, http.MethodPut, writePath, testAPIToken,
			strings.NewReader("\xff\xfe"), "text/plain")
		if status != http.StatusBadRequest {
			t.Fatalf("非法 UTF-8 写入状态码 = %d，期望 400（body=%s）", status, raw)
		}

		// 符号链接逃逸：任务根目录里放一个指向外部的链接。
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
			t.Fatalf("创建外部文件失败: %v", err)
		}
		missionDir := filepath.Join(h.stateHome, "tenants", fmt.Sprintf("%d", h.tenantA),
			"workspace", "missions", fmt.Sprintf("%d", mission.ID))
		if err := os.MkdirAll(missionDir, 0o700); err != nil {
			t.Fatalf("创建任务目录失败: %v", err)
		}
		linkPath := filepath.Join(missionDir, "escape-link")
		if err := os.Symlink(filepath.Join(outside, "secret.txt"), linkPath); err != nil {
			t.Fatalf("创建符号链接失败: %v", err)
		}
		status, raw, _ = h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/workspace/files/escape-link?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusBadRequest {
			t.Fatalf("符号链接读取状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("大小限制", func(t *testing.T) {
		big := strings.Repeat("a", int(1<<20)+10)
		status, raw, _ := h.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/workspace/files/big.txt?mission_id=%d", mission.ID), testAPIToken, big)
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("超大写入状态码 = %d，期望 413（body=%s）", status, raw)
		}
	})

	t.Run("上传", func(t *testing.T) {
		body, contentType := b5agentsMultipart(t, "file", "upload.txt", []byte("uploaded"), nil)
		status, raw, _ := b5agentsDoRaw(t, h.client, h.baseURL, http.MethodPost,
			fmt.Sprintf("/api/v1/workspace/upload?mission_id=%d", mission.ID), testAPIToken, body, contentType)
		if status != http.StatusCreated {
			t.Fatalf("上传状态码 = %d，期望 201（body=%s）", status, raw)
		}
		var entry b5agentsFileJSON
		b5agentsDecode(t, raw, &entry)
		if entry.Path != "upload.txt" || entry.Size != int64(len("uploaded")) {
			t.Fatalf("上传响应不符合预期: %+v", entry)
		}
		status, raw, _ = h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/workspace/files/upload.txt?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("读取上传文件状态码 = %d，期望 200（body=%s）", status, raw)
		}

		// 带 path 目录上传。
		body, contentType = b5agentsMultipart(t, "file", "nested.txt", []byte("nested"),
			map[string]string{"path": "sub"})
		status, raw, _ = b5agentsDoRaw(t, h.client, h.baseURL, http.MethodPost,
			fmt.Sprintf("/api/v1/workspace/upload?mission_id=%d", mission.ID), testAPIToken, body, contentType)
		if status != http.StatusCreated {
			t.Fatalf("目录上传状态码 = %d，期望 201（body=%s）", status, raw)
		}
		b5agentsDecode(t, raw, &entry)
		if entry.Path != "sub/nested.txt" {
			t.Fatalf("目录上传路径不符合预期: %+v", entry)
		}

		// 上传允许二进制（NUL）；但随后按文本读取应返回 400。
		binaryData := []byte{0x00, 0x01, 0x02, 0xff}
		body, contentType = b5agentsMultipart(t, "file", "blob.bin", binaryData, nil)
		status, raw, _ = b5agentsDoRaw(t, h.client, h.baseURL, http.MethodPost,
			fmt.Sprintf("/api/v1/workspace/upload?mission_id=%d", mission.ID), testAPIToken, body, contentType)
		if status != http.StatusCreated {
			t.Fatalf("二进制上传状态码 = %d，期望 201（body=%s）", status, raw)
		}
		b5agentsDecode(t, raw, &entry)
		if entry.Path != "blob.bin" || entry.Size != int64(len(binaryData)) {
			t.Fatalf("二进制上传响应不符合预期: %+v", entry)
		}
		status, raw, _ = h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/workspace/files/blob.bin?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusBadRequest {
			t.Fatalf("读取二进制状态码 = %d，期望 400（body=%s）", status, raw)
		}

		// 缺少 file 字段。
		body, contentType = b5agentsMultipart(t, "other", "x.txt", []byte("x"), nil)
		status, raw, _ = b5agentsDoRaw(t, h.client, h.baseURL, http.MethodPost,
			fmt.Sprintf("/api/v1/workspace/upload?mission_id=%d", mission.ID), testAPIToken, body, contentType)
		if status != http.StatusBadRequest {
			t.Fatalf("缺 file 字段状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("删除", func(t *testing.T) {
		// 非空目录默认拒绝。
		status, raw, _ := h.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/workspace/files/notes?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusConflict {
			t.Fatalf("删除非空目录状态码 = %d，期望 409（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/workspace/files/notes?mission_id=%d&recursive=true", mission.ID), testAPIToken, "")
		if status != http.StatusNoContent {
			t.Fatalf("递归删除状态码 = %d，期望 204（body=%s）", status, raw)
		}

		status, raw, _ = h.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/workspace/files/upload.txt?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusNoContent {
			t.Fatalf("删除文件状态码 = %d，期望 204（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/workspace/files/upload.txt?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("删除后读取状态码 = %d，期望 404（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/workspace/files/upload.txt?mission_id=%d", mission.ID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("重复删除状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("任务与参数校验", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/workspace/files",
			"/api/v1/workspace/files?mission_id=abc",
			"/api/v1/workspace/files?mission_id=0",
			fmt.Sprintf("/api/v1/workspace/files?mission_id=%d&limit=0", mission.ID),
		} {
			status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
			if status != http.StatusBadRequest {
				t.Fatalf("路径 %s 状态码 = %d，期望 400（body=%s）", path, status, raw)
			}
		}
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/workspace/files?mission_id=999999999", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("不存在任务状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("跨租户隔离", func(t *testing.T) {
		missionB := h.createMission(t, h.tenantB, "租户 B 任务")
		// 租户 A 访问 B 的任务 → 404。
		status, raw, _ := h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/workspace/files?mission_id=%d", missionB.ID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("跨租户列表状态码 = %d，期望 404（body=%s）", status, raw)
		}
		// 租户 A 与租户 B 的同名文件互不可见。
		status, raw, _ = h.do(t, http.MethodPut,
			fmt.Sprintf("/api/v1/workspace/files/same.txt?mission_id=%d", mission.ID), testAPIToken, "A")
		if status != http.StatusOK {
			t.Fatalf("租户 A 写入状态码 = %d（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/workspace/files/same.txt?mission_id=%d", missionB.ID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("跨租户读取状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, base, "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// TestB5StoreUnavailable 校验 Store 未装配时返回 503。
func TestB5StoreUnavailable(t *testing.T) {
	h := b5agentsNewHarness(t)
	srv := h.newServer(t, h.tenantA, false)
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/agents"},
		{http.MethodGet, "/api/v1/workspace/files?mission_id=1"},
	} {
		status, raw, _ := b5agentsDoRaw(t, srv.Client(), srv.URL, req.method, req.path, testAPIToken,
			strings.NewReader(""), "application/json")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s %s 状态码 = %d，期望 503（body=%s）", req.method, req.path, status, raw)
		}
	}
}
