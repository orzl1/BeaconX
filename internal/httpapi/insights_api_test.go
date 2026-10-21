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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/finding"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/llm"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/tenant"
	"github.com/orzl1/BeaconX/migrations"
)

// M6.2-B3 洞察接口（流量/命令/LLM 记录/用量/Dashboard）的端到端测试。
//
// 依赖真实 PostgreSQL 与本地临时证据目录：未设置 BEACONX_TEST_PG_DSN 时整体跳过。
// 每个用例使用独立租户，清理时删除租户靠外键级联清空数据。

// insightsHarness 是 B3 测试所需的服务端、数据库与证据存储句柄。
type insightsHarness struct {
	client      *http.Client
	baseURL     string
	store       *pg.Store
	db          *pg.DB
	tenantA     int64
	tenantB     int64
	evidence    *evidence.Store
	evidenceDir string
}

// newInsightsHarness 启动绑定租户 A 的 HTTP 服务（含证据存储），并创建租户 B。
func newInsightsHarness(t *testing.T) *insightsHarness {
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
			id, fmt.Sprintf("insights-api-%d", id), "洞察接口测试租户"); err != nil {
			t.Fatalf("创建测试租户失败: %v", err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = db.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = ANY($1)`, []int64{tenantA, tenantB})
	})

	evidenceDir := t.TempDir()
	evStore, err := evidence.New(evidence.Config{Dir: evidenceDir, Key: bytes.Repeat([]byte{0x42}, 32)})
	if err != nil {
		t.Fatalf("创建证据存储失败: %v", err)
	}

	h := &insightsHarness{
		store: pg.NewStore(db), db: db,
		tenantA: tenantA, tenantB: tenantB,
		evidence: evStore, evidenceDir: evidenceDir,
	}
	srv := h.newServer(t, true, true)
	h.client = srv.Client()
	h.baseURL = srv.URL
	return h
}

// newServer 用当前租户/存储构建一个 HTTP 服务；可选是否装配 Store 与 Evidence，
// 用于测试 503 分支。
func (h *insightsHarness) newServer(t *testing.T, withStore, withEvidence bool) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{
		Logger:         logger,
		RequestTimeout: 5 * time.Second,
		TenantResolver: StaticTenant(tenant.ID(h.tenantA)),
		Authenticator:  NewTokenAuthenticator(testAPIToken),
		TenantID:       h.tenantA,
	}
	if withStore {
		opts.Store = h.store
	}
	if withEvidence {
		opts.Evidence = h.evidence
	}
	handler, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("构建 HTTP 处理器失败: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// do 发送请求并读取完整响应体；token 为空表示不带 Authorization 头。
func (h *insightsHarness) do(t *testing.T, method, path, token, body string) (int, []byte, http.Header) {
	t.Helper()
	return doAt(t, h.client, h.baseURL, method, path, token, body)
}

// doAt 向指定服务地址发送请求。
func doAt(t *testing.T, client *http.Client, baseURL, method, path, token, body string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
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

// createMission 在当前租户下创建任务。
func (h *insightsHarness) createMission(t *testing.T, name string) pg.Mission {
	t.Helper()
	mission, err := h.store.CreateMission(context.Background(), h.tenantA, pg.MissionInput{
		Name: name, Goal: "验证 B3 接口", Description: "洞察接口测试任务",
	})
	if err != nil {
		t.Fatalf("创建测试任务失败: %v", err)
	}
	return mission
}

// putBlob 写入一段测试正文并返回引用。
func (h *insightsHarness) putBlob(t *testing.T, data []byte) evidence.BlobRef {
	t.Helper()
	ref, err := h.evidence.PutBlob(context.Background(), data)
	if err != nil {
		t.Fatalf("写入证据 blob 失败: %v", err)
	}
	return ref
}

// insightArrayField 断言响应体中的顶层字段是 JSON 数组而不是 null。
func insightArrayField(t *testing.T, raw []byte, field string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v（body=%s）", err, raw)
	}
	value, ok := envelope[field]
	if !ok {
		t.Fatalf("响应缺少字段 %q（body=%s）", field, raw)
	}
	if strings.TrimSpace(string(value)) == "null" {
		t.Fatalf("字段 %q 不应为 null（body=%s）", field, raw)
	}
}

// ---------- 类型 ----------

type trafficJSON struct {
	ID           int64  `json:"id"`
	MissionID    int64  `json:"mission_id"`
	Tool         string `json:"tool"`
	Phase        string `json:"phase"`
	RequestHash  string `json:"request_hash"`
	RequestSize  int64  `json:"request_size"`
	ResponseHash string `json:"response_hash"`
	ResponseSize int64  `json:"response_size"`
	Error        string `json:"error"`
}

type trafficListJSON struct {
	Traffic []trafficJSON `json:"traffic"`
}

type auditJSON struct {
	ID        int64  `json:"id"`
	MissionID int64  `json:"mission_id"`
	Tool      string `json:"tool"`
	Action    string `json:"action"`
	RuleName  string `json:"rule_name"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

type commandListJSON struct {
	Commands []auditJSON `json:"commands"`
}

type llmRecordJSON struct {
	ID               int64  `json:"id"`
	MissionID        *int64 `json:"mission_id"`
	Role             string `json:"role"`
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

type llmRecordListJSON struct {
	Records []llmRecordJSON `json:"records"`
}

type usageRoleJSON struct {
	Role        string `json:"role"`
	Calls       int64  `json:"calls"`
	TotalTokens int64  `json:"total_tokens"`
}

type usageModelJSON struct {
	Model       string `json:"model"`
	Calls       int64  `json:"calls"`
	TotalTokens int64  `json:"total_tokens"`
}

type missionUsageJSON struct {
	MissionID   int64            `json:"mission_id"`
	MaxTokens   int64            `json:"max_tokens"`
	SpentTokens int64            `json:"spent_tokens"`
	MaxSteps    int              `json:"max_steps"`
	StepsUsed   int              `json:"steps_used"`
	Records     []llmRecordJSON  `json:"records"`
	ByRole      []usageRoleJSON  `json:"by_role"`
	ByModel     []usageModelJSON `json:"by_model"`
}

type usageGroupJSON struct {
	Key              string `json:"key"`
	Calls            int64  `json:"calls"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

type usageSummaryJSON struct {
	GroupBy string           `json:"group_by"`
	Groups  []usageGroupJSON `json:"groups"`
}

type dashboardJSON struct {
	Missions struct {
		Total    int64            `json:"total"`
		ByStatus map[string]int64 `json:"by_status"`
	} `json:"missions"`
	Tokens struct {
		SpentTotal int64 `json:"spent_total"`
		Calls      int64 `json:"calls"`
		Today      int64 `json:"today"`
	} `json:"tokens"`
	Approvals struct {
		Pending int64 `json:"pending"`
	} `json:"approvals"`
	Findings struct {
		Total      int64            `json:"total"`
		BySeverity map[string]int64 `json:"by_severity"`
	} `json:"findings"`
	Assets struct {
		Total int64 `json:"total"`
	} `json:"assets"`
	Traffic struct {
		Total int64 `json:"total"`
	} `json:"traffic"`
	RecentMissions []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Status      string `json:"status"`
		SpentTokens int64  `json:"spent_tokens"`
		CreatedAt   string `json:"created_at"`
	} `json:"recent_missions"`
}

// ---------- 流量 ----------

func TestInsightsTrafficAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "流量接口测试")

	requestData := []byte(`{"command":"id"}`)
	responseData := []byte(`{"stdout":"uid=0(root)"}`)
	reqRef := h.putBlob(t, requestData)
	respRef := h.putBlob(t, responseData)

	finishedID, err := h.store.BeginTraffic(ctx, h.tenantA, mission.ID, nil, "shell_exec", reqRef, "none")
	if err != nil {
		t.Fatalf("写入请求阶段失败: %v", err)
	}
	if err := h.store.FinishTraffic(ctx, h.tenantA, finishedID, respRef, 0, ""); err != nil {
		t.Fatalf("写入响应阶段失败: %v", err)
	}
	pendingReq := h.putBlob(t, []byte(`{"url":"https://example.com"}`))
	pendingID, err := h.store.BeginTraffic(ctx, h.tenantA, mission.ID, nil, "http_probe", pendingReq, "none")
	if err != nil {
		t.Fatalf("写入未完成采集失败: %v", err)
	}

	t.Run("列表与过滤", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/traffic", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		insightArrayField(t, raw, "traffic")
		var list trafficListJSON
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("解析流量列表失败: %v", err)
		}
		if len(list.Traffic) != 2 {
			t.Fatalf("流量条数 = %d，期望 2", len(list.Traffic))
		}
		if list.Traffic[0].ID != pendingID {
			t.Fatalf("应按 id 倒序，首条 = %d，期望 %d", list.Traffic[0].ID, pendingID)
		}

		cases := []struct {
			name string
			path string
			want int
		}{
			{name: "按任务过滤", path: fmt.Sprintf("/api/v1/traffic?mission_id=%d", mission.ID), want: 2},
			{name: "按工具过滤", path: "/api/v1/traffic?tool=shell_exec", want: 1},
			{name: "按阶段过滤", path: "/api/v1/traffic?phase=intent", want: 1},
			{name: "按结果阶段过滤", path: "/api/v1/traffic?phase=result", want: 1},
			{name: "不存在的任务过滤为空", path: "/api/v1/traffic?mission_id=999999999", want: 0},
			{name: "limit 截断", path: "/api/v1/traffic?limit=1", want: 1},
			{name: "limit 超上限被钳制", path: "/api/v1/traffic?limit=99999", want: 2},
		}
		for _, c := range cases {
			status, raw, _ := h.do(t, http.MethodGet, c.path, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("%s 状态码 = %d，期望 200（body=%s）", c.name, status, raw)
			}
			var got trafficListJSON
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s 解析失败: %v", c.name, err)
			}
			if len(got.Traffic) != c.want {
				t.Fatalf("%s 条数 = %d，期望 %d", c.name, len(got.Traffic), c.want)
			}
		}

		for _, path := range []string{
			"/api/v1/traffic?phase=bogus",
			"/api/v1/traffic?limit=0",
			"/api/v1/traffic?limit=abc",
			"/api/v1/traffic?mission_id=abc",
		} {
			status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
			if status != http.StatusBadRequest {
				t.Fatalf("路径 %s 状态码 = %d，期望 400（body=%s）", path, status, raw)
			}
		}
	})

	t.Run("详情", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d", finishedID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var got trafficJSON
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("解析详情失败: %v", err)
		}
		if got.ID != finishedID || got.Phase != "result" || got.Tool != "shell_exec" {
			t.Fatalf("详情不符合预期: %+v", got)
		}
		if got.ResponseSize != int64(len(responseData)) {
			t.Fatalf("响应大小 = %d，期望 %d", got.ResponseSize, len(responseData))
		}

		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/traffic/999999999", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("不存在记录状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("读取解密正文", func(t *testing.T) {
		status, raw, header := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d/request", finishedID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("请求正文状态码 = %d，期望 200（body=%s）", status, raw)
		}
		if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("Content-Type = %q，期望 text/plain", ct)
		}
		if !bytes.Equal(raw, requestData) {
			t.Fatalf("请求正文 = %q，期望 %q", raw, requestData)
		}

		status, raw, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d/response", finishedID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("响应正文状态码 = %d，期望 200（body=%s）", status, raw)
		}
		if !bytes.Equal(raw, responseData) {
			t.Fatalf("响应正文 = %q，期望 %q", raw, responseData)
		}

		// 未完成的采集没有响应正文。
		status, raw, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d/response", pendingID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("未完成采集的响应状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("Evidence 未装配返回 503", func(t *testing.T) {
		srv := h.newServer(t, true, false)
		status, raw, _ := doAt(t, srv.Client(), srv.URL,
			http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d/request", finishedID), testAPIToken, "")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("状态码 = %d，期望 503（body=%s）", status, raw)
		}
	})

	t.Run("正文被篡改返回 409", func(t *testing.T) {
		// 直接覆盖响应 blob 的密文文件，读取时必须被 AEAD 校验拦下。
		path := filepath.Join(h.evidenceDir, "blobs", respRef.Hash[:2], respRef.Hash+".enc")
		if err := os.WriteFile(path, []byte("tampered-ciphertext"), 0o600); err != nil {
			t.Fatalf("篡改 blob 失败: %v", err)
		}
		status, raw, _ := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d/response", finishedID), testAPIToken, "")
		if status != http.StatusConflict {
			t.Fatalf("篡改后状态码 = %d，期望 409（body=%s）", status, raw)
		}
		// 请求正文未被篡改，仍可读。
		status, raw, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d/request", finishedID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("未篡改的请求正文状态码 = %d，期望 200（body=%s）", status, raw)
		}
	})

	t.Run("删除", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/traffic/%d", pendingID), testAPIToken, "")
		if status != http.StatusNoContent {
			t.Fatalf("删除状态码 = %d，期望 204（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/traffic/%d", pendingID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("删除后状态码 = %d，期望 404（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodDelete, fmt.Sprintf("/api/v1/traffic/%d", pendingID), testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("重复删除状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/traffic", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- 命令历史 ----------

func TestInsightsCommandsAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "命令历史测试")

	if err := h.store.InsertAudit(ctx, h.tenantA, mission.ID, nil, "shell_exec",
		policy.Allow, "允许 shell", []byte(`{"command":"id"}`), "executed", "ok"); err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}
	if err := h.store.InsertAudit(ctx, h.tenantA, mission.ID, nil, "http_probe",
		policy.Ask, "HTTP 需审批", []byte(`{"method":"POST"}`), "rejected_by_user", "人工拒绝"); err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}

	t.Run("列表与过滤", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/commands", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		insightArrayField(t, raw, "commands")
		var list commandListJSON
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("解析命令列表失败: %v", err)
		}
		if len(list.Commands) != 2 {
			t.Fatalf("命令条数 = %d，期望 2", len(list.Commands))
		}

		cases := []struct {
			name string
			path string
			want int
		}{
			{name: "按工具过滤", path: "/api/v1/commands?tool=shell_exec", want: 1},
			{name: "按结果过滤", path: "/api/v1/commands?outcome=rejected_by_user", want: 1},
			{name: "按任务过滤", path: fmt.Sprintf("/api/v1/commands?mission_id=%d", mission.ID), want: 2},
			{name: "limit 截断", path: "/api/v1/commands?limit=1", want: 1},
			{name: "limit 钳制", path: "/api/v1/commands?limit=99999", want: 2},
		}
		for _, c := range cases {
			status, raw, _ := h.do(t, http.MethodGet, c.path, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("%s 状态码 = %d，期望 200（body=%s）", c.name, status, raw)
			}
			var got commandListJSON
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s 解析失败: %v", c.name, err)
			}
			if len(got.Commands) != c.want {
				t.Fatalf("%s 条数 = %d，期望 %d", c.name, len(got.Commands), c.want)
			}
		}

		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/commands?limit=0", testAPIToken, "")
		if status != http.StatusBadRequest {
			t.Fatalf("非法 limit 状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("详情与 404", func(t *testing.T) {
		var list commandListJSON
		_, raw, _ := h.do(t, http.MethodGet, "/api/v1/commands", testAPIToken, "")
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("解析命令列表失败: %v", err)
		}
		id := list.Commands[0].ID
		status, raw, _ := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/commands/%d", id), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("详情状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var event auditJSON
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("解析命令详情失败: %v", err)
		}
		if event.ID != id || event.MissionID != mission.ID {
			t.Fatalf("命令详情不符合预期: %+v", event)
		}

		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/commands/999999999", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("不存在命令状态码 = %d，期望 404（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/commands", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- LLM 记录 ----------

func TestInsightsLLMRecordsAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "LLM 记录测试")
	if err := h.store.RecordLLMUsage(ctx, h.tenantA, mission.ID, "planner", "model-a",
		llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}
	if err := h.store.RecordLLMUsage(ctx, h.tenantA, mission.ID, "worker", "model-b",
		llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}

	t.Run("列表与过滤", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/llm-records", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		insightArrayField(t, raw, "records")
		var list llmRecordListJSON
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("解析记录失败: %v", err)
		}
		if len(list.Records) != 2 {
			t.Fatalf("记录条数 = %d，期望 2", len(list.Records))
		}
		if list.Records[0].Model != "model-b" {
			t.Fatalf("应按 id 倒序（最新 model-b 在前），实际 %+v", list.Records[0])
		}

		cases := []struct {
			name string
			path string
			want int
		}{
			{name: "按角色过滤", path: "/api/v1/llm-records?role=worker", want: 1},
			{name: "按模型过滤", path: "/api/v1/llm-records?model=model-a", want: 1},
			{name: "按任务过滤", path: fmt.Sprintf("/api/v1/llm-records?mission_id=%d", mission.ID), want: 2},
			{name: "limit 截断", path: "/api/v1/llm-records?limit=1", want: 1},
			{name: "limit 钳制", path: "/api/v1/llm-records?limit=99999", want: 2},
		}
		for _, c := range cases {
			status, raw, _ := h.do(t, http.MethodGet, c.path, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("%s 状态码 = %d，期望 200（body=%s）", c.name, status, raw)
			}
			var got llmRecordListJSON
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s 解析失败: %v", c.name, err)
			}
			if len(got.Records) != c.want {
				t.Fatalf("%s 条数 = %d，期望 %d", c.name, len(got.Records), c.want)
			}
		}

		status, raw, _ = h.do(t, http.MethodGet, "/api/v1/llm-records?limit=-1", testAPIToken, "")
		if status != http.StatusBadRequest {
			t.Fatalf("非法 limit 状态码 = %d，期望 400（body=%s）", status, raw)
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/llm-records", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- 任务用量 ----------

func TestMissionUsageAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "任务用量测试")
	emptyMission := h.createMission(t, "空任务")

	if err := h.store.RecordLLMUsage(ctx, h.tenantA, mission.ID, "planner", "model-a",
		llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}
	if err := h.store.RecordLLMUsage(ctx, h.tenantA, mission.ID, "worker", "model-b",
		llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}

	t.Run("聚合正确", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/missions/%d/usage", mission.ID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var usage missionUsageJSON
		if err := json.Unmarshal(raw, &usage); err != nil {
			t.Fatalf("解析用量失败: %v", err)
		}
		if usage.MissionID != mission.ID {
			t.Fatalf("mission_id = %d，期望 %d", usage.MissionID, mission.ID)
		}
		if usage.SpentTokens != 45 {
			t.Fatalf("spent_tokens = %d，期望 45", usage.SpentTokens)
		}
		if len(usage.Records) != 2 {
			t.Fatalf("记录条数 = %d，期望 2", len(usage.Records))
		}
		if len(usage.ByRole) != 2 || len(usage.ByModel) != 2 {
			t.Fatalf("按角色/模型聚合条数不符合预期: role=%+v model=%+v", usage.ByRole, usage.ByModel)
		}
		roleByKey := map[string]usageRoleJSON{}
		for _, item := range usage.ByRole {
			roleByKey[item.Role] = item
		}
		if item := roleByKey["planner"]; item.Calls != 1 || item.TotalTokens != 15 {
			t.Fatalf("planner 聚合不符合预期: %+v", item)
		}
		if item := roleByKey["worker"]; item.Calls != 1 || item.TotalTokens != 30 {
			t.Fatalf("worker 聚合不符合预期: %+v", item)
		}
	})

	t.Run("空任务返回空数组", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet,
			fmt.Sprintf("/api/v1/missions/%d/usage", emptyMission.ID), testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		for _, field := range []string{"records", "by_role", "by_model"} {
			if strings.TrimSpace(string(envelope[field])) == "null" {
				t.Fatalf("字段 %q 不应为 null（body=%s）", field, raw)
			}
		}
	})

	t.Run("任务不存在与未认证", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/missions/999999999/usage", testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("不存在任务状态码 = %d，期望 404（body=%s）", status, raw)
		}
		status, raw, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/missions/%d/usage", mission.ID), "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("未认证状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- 用量聚合 ----------

func TestLLMUsageSummaryAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	mission := h.createMission(t, "用量聚合测试")
	for _, item := range []struct {
		role, model string
		usage       llm.Usage
	}{
		{"worker", "model-a", llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{"worker", "model-a", llm.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}},
		{"planner", "model-b", llm.Usage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30}},
	} {
		if err := h.store.RecordLLMUsage(ctx, h.tenantA, mission.ID, item.role, item.model, item.usage); err != nil {
			t.Fatalf("写入用量失败: %v", err)
		}
	}

	t.Run("按模型聚合（默认）", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/llm-usage/summary", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var summary usageSummaryJSON
		if err := json.Unmarshal(raw, &summary); err != nil {
			t.Fatalf("解析聚合失败: %v", err)
		}
		if summary.GroupBy != "model" {
			t.Fatalf("group_by = %q，期望 model", summary.GroupBy)
		}
		byKey := map[string]usageGroupJSON{}
		for _, g := range summary.Groups {
			byKey[g.Key] = g
		}
		if g := byKey["model-a"]; g.Calls != 2 || g.TotalTokens != 18 || g.PromptTokens != 12 || g.CompletionTokens != 6 {
			t.Fatalf("model-a 聚合不符合预期: %+v", g)
		}
		if g := byKey["model-b"]; g.Calls != 1 || g.TotalTokens != 30 {
			t.Fatalf("model-b 聚合不符合预期: %+v", g)
		}
	})

	t.Run("按角色与按天聚合", func(t *testing.T) {
		for _, groupBy := range []string{"role", "day"} {
			status, raw, _ := h.do(t, http.MethodGet, "/api/v1/llm-usage/summary?group_by="+groupBy, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("group_by=%s 状态码 = %d，期望 200（body=%s）", groupBy, status, raw)
			}
			var summary usageSummaryJSON
			if err := json.Unmarshal(raw, &summary); err != nil {
				t.Fatalf("group_by=%s 解析失败: %v", groupBy, err)
			}
			if len(summary.Groups) == 0 {
				t.Fatalf("group_by=%s 分组为空", groupBy)
			}
			if groupBy == "day" {
				// 数据库 date_trunc('day') 使用会话时区（测试库为 UTC），
				// 这里同时接受本地与 UTC 的今天，避免跨时区边界误报。
				todayLocal := time.Now().Format("2006-01-02")
				todayUTC := time.Now().UTC().Format("2006-01-02")
				found := false
				for _, g := range summary.Groups {
					if g.Key == todayLocal || g.Key == todayUTC {
						found = true
						if g.Calls != 3 || g.TotalTokens != 48 {
							t.Fatalf("当天聚合不符合预期: %+v", g)
						}
					}
				}
				if !found {
					t.Fatalf("day 分组缺少今天（%q/%q）: %+v", todayLocal, todayUTC, summary.Groups)
				}
			}
		}
	})

	t.Run("days 时间窗口", func(t *testing.T) {
		// 把一条记录改到 40 天前：days=30 应排除，days=60 应包含。
		if _, err := h.db.Pool().Exec(ctx,
			`UPDATE llm_usage SET created_at = now() - interval '40 days'
			 WHERE tenant_id = $1 AND id = (SELECT max(id) FROM llm_usage WHERE tenant_id = $1)`,
			h.tenantA); err != nil {
			t.Fatalf("调整记录时间失败: %v", err)
		}
		check := func(days string, wantCalls int64) {
			status, raw, _ := h.do(t, http.MethodGet,
				"/api/v1/llm-usage/summary?group_by=model&days="+days, testAPIToken, "")
			if status != http.StatusOK {
				t.Fatalf("days=%s 状态码 = %d，期望 200（body=%s）", days, status, raw)
			}
			var summary usageSummaryJSON
			if err := json.Unmarshal(raw, &summary); err != nil {
				t.Fatalf("days=%s 解析失败: %v", days, err)
			}
			var calls int64
			for _, g := range summary.Groups {
				calls += g.Calls
			}
			if calls != wantCalls {
				t.Fatalf("days=%s 调用数 = %d，期望 %d（groups=%+v）", days, calls, wantCalls, summary.Groups)
			}
		}
		check("30", 2)
		check("60", 3)
	})

	t.Run("非法参数", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/llm-usage/summary?group_by=bogus",
			"/api/v1/llm-usage/summary?days=0",
			"/api/v1/llm-usage/summary?days=abc",
			"/api/v1/llm-usage/summary?mission_id=abc",
		} {
			status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
			if status != http.StatusBadRequest {
				t.Fatalf("路径 %s 状态码 = %d，期望 400（body=%s）", path, status, raw)
			}
		}
		// 筛选不存在的任务 → 200 + 空数组。
		status, raw, _ := h.do(t, http.MethodGet,
			"/api/v1/llm-usage/summary?mission_id=999999999", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("空过滤状态码 = %d，期望 200（body=%s）", status, raw)
		}
		insightArrayField(t, raw, "groups")
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/llm-usage/summary", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- Dashboard ----------

func TestDashboardAPI(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()

	t.Run("空租户全零", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/dashboard", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var dash dashboardJSON
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatalf("解析 Dashboard 失败: %v", err)
		}
		if dash.Missions.Total != 0 || dash.Tokens.Calls != 0 || dash.Traffic.Total != 0 {
			t.Fatalf("空租户计数应为 0: %+v", dash)
		}
		if len(dash.Missions.ByStatus) != 7 {
			t.Fatalf("by_status 应固定 7 个键，实际 %d", len(dash.Missions.ByStatus))
		}
		if len(dash.Findings.BySeverity) != 5 {
			t.Fatalf("by_severity 应固定 5 个键，实际 %d", len(dash.Findings.BySeverity))
		}
		if dash.RecentMissions == nil {
			t.Fatal("recent_missions 不应为 null")
		}
	})

	// 造数据：三个任务（queued/running/done）、一个资产、两个发现（high/info）、
	// 一条待审批、一条流量、一次 LLM 调用。
	m1 := h.createMission(t, "任务一")
	m2 := h.createMission(t, "任务二")
	m3 := h.createMission(t, "任务三")
	for _, item := range []struct {
		id     int64
		status string
	}{{m2.ID, "running"}, {m3.ID, "done"}} {
		if _, err := h.db.Pool().Exec(ctx,
			`UPDATE missions SET status = $3 WHERE tenant_id = $1 AND id = $2`,
			h.tenantA, item.id, item.status); err != nil {
			t.Fatalf("设置任务状态失败: %v", err)
		}
	}
	if _, err := h.store.CreateAsset(ctx, h.tenantA, pg.AssetInput{
		Type: asset.KindRootDomain, Label: "example.com",
	}); err != nil {
		t.Fatalf("创建资产失败: %v", err)
	}
	for _, sev := range []finding.Severity{finding.SeverityHigh, finding.SeverityInfo} {
		if _, err := h.store.CreateFinding(ctx, h.tenantA, m1.ID, pg.FindingInput{
			Title: "测试发现", Severity: sev,
		}); err != nil {
			t.Fatalf("创建发现失败: %v", err)
		}
	}
	node, err := h.store.AddNode(ctx, h.tenantA, m1.ID, pg.NodeInput{
		Kind: graph.NodeAction, State: graph.StateOpen, Title: "等待审批的动作",
	})
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	if _, err := h.store.CreateApproval(ctx, h.tenantA, m1.ID, node.ID, "shell_exec", []byte(`{}`), time.Minute); err != nil {
		t.Fatalf("创建审批失败: %v", err)
	}
	reqRef := h.putBlob(t, []byte(`{"command":"id"}`))
	if _, err := h.store.BeginTraffic(ctx, h.tenantA, m1.ID, nil, "shell_exec", reqRef, "none"); err != nil {
		t.Fatalf("写入流量失败: %v", err)
	}
	if err := h.store.RecordLLMUsage(ctx, h.tenantA, m1.ID, "worker", "model-a",
		llm.Usage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}); err != nil {
		t.Fatalf("写入用量失败: %v", err)
	}

	t.Run("聚合计数", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/dashboard", testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, raw)
		}
		var dash dashboardJSON
		if err := json.Unmarshal(raw, &dash); err != nil {
			t.Fatalf("解析 Dashboard 失败: %v", err)
		}
		if dash.Missions.Total != 3 {
			t.Fatalf("任务总数 = %d，期望 3", dash.Missions.Total)
		}
		wantStatus := map[string]int64{"queued": 1, "running": 1, "done": 1, "paused": 0, "finalizing": 0, "failed": 0, "cancelled": 0}
		for k, want := range wantStatus {
			if got := dash.Missions.ByStatus[k]; got != want {
				t.Fatalf("by_status[%s] = %d，期望 %d", k, got, want)
			}
		}
		if dash.Tokens.SpentTotal != 7 {
			t.Fatalf("spent_total = %d，期望 7", dash.Tokens.SpentTotal)
		}
		if dash.Tokens.Calls != 1 || dash.Tokens.Today != 7 {
			t.Fatalf("token 调用/当天不符合预期: %+v", dash.Tokens)
		}
		if dash.Approvals.Pending != 1 {
			t.Fatalf("pending = %d，期望 1", dash.Approvals.Pending)
		}
		if dash.Findings.Total != 2 || dash.Findings.BySeverity["high"] != 1 || dash.Findings.BySeverity["info"] != 1 {
			t.Fatalf("发现统计不符合预期: %+v", dash.Findings)
		}
		if dash.Assets.Total != 1 || dash.Traffic.Total != 1 {
			t.Fatalf("资产/流量统计不符合预期: assets=%d traffic=%d", dash.Assets.Total, dash.Traffic.Total)
		}
		if len(dash.RecentMissions) != 3 {
			t.Fatalf("recent_missions 条数 = %d，期望 3", len(dash.RecentMissions))
		}
		wantIDs := []int64{m3.ID, m2.ID, m1.ID}
		for i, want := range wantIDs {
			if dash.RecentMissions[i].ID != want {
				t.Fatalf("recent_missions[%d].id = %d，期望 %d", i, dash.RecentMissions[i].ID, want)
			}
		}
		if dash.RecentMissions[0].Name != "任务三" || dash.RecentMissions[0].Status != "done" {
			t.Fatalf("最近任务字段不符合预期: %+v", dash.RecentMissions[0])
		}
	})

	t.Run("未认证返回 401", func(t *testing.T) {
		status, raw, _ := h.do(t, http.MethodGet, "/api/v1/dashboard", "", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，期望 401（body=%s）", status, raw)
		}
	})
}

// ---------- 跨租户隔离 ----------

func TestInsightsTenantIsolation(t *testing.T) {
	h := newInsightsHarness(t)
	ctx := context.Background()
	// 租户 B 的数据：任务、流量、审计、用量。
	missionB, err := h.store.CreateMission(ctx, h.tenantB, pg.MissionInput{Name: "租户 B 的任务"})
	if err != nil {
		t.Fatalf("创建租户 B 任务失败: %v", err)
	}
	ref := h.putBlob(t, []byte(`{"command":"id"}`))
	trafficB, err := h.store.BeginTraffic(ctx, h.tenantB, missionB.ID, nil, "shell_exec", ref, "none")
	if err != nil {
		t.Fatalf("写入租户 B 流量失败: %v", err)
	}
	if err := h.store.InsertAudit(ctx, h.tenantB, missionB.ID, nil, "shell_exec",
		policy.Allow, "", []byte(`{}`), "executed", ""); err != nil {
		t.Fatalf("写入租户 B 审计失败: %v", err)
	}
	if err := h.store.RecordLLMUsage(ctx, h.tenantB, missionB.ID, "worker", "model-b",
		llm.Usage{TotalTokens: 9}); err != nil {
		t.Fatalf("写入租户 B 用量失败: %v", err)
	}

	// 以租户 A 的身份访问，跨租户数据不可见。
	for _, path := range []string{
		fmt.Sprintf("/api/v1/traffic/%d", trafficB),
		fmt.Sprintf("/api/v1/missions/%d/usage", missionB.ID),
	} {
		status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
		if status != http.StatusNotFound {
			t.Fatalf("跨租户访问 %s 状态码 = %d，期望 404（body=%s）", path, status, raw)
		}
	}

	for _, path := range []string{"/api/v1/traffic", "/api/v1/commands", "/api/v1/llm-records"} {
		status, raw, _ := h.do(t, http.MethodGet, path, testAPIToken, "")
		if status != http.StatusOK {
			t.Fatalf("列表 %s 状态码 = %d，期望 200（body=%s）", path, status, raw)
		}
		field := map[string]string{
			"/api/v1/traffic":     "traffic",
			"/api/v1/commands":    "commands",
			"/api/v1/llm-records": "records",
		}[path]
		insightArrayField(t, raw, field)
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("解析 %s 失败: %v", path, err)
		}
		if got := string(envelope[field]); got != "[]" {
			t.Fatalf("%s 在租户 A 应为空，实际 %s", path, got)
		}
	}

	// Dashboard 也不应统计到租户 B 的数据。
	status, raw, _ := h.do(t, http.MethodGet, "/api/v1/dashboard", testAPIToken, "")
	if status != http.StatusOK {
		t.Fatalf("Dashboard 状态码 = %d，期望 200（body=%s）", status, raw)
	}
	var dash dashboardJSON
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("解析 Dashboard 失败: %v", err)
	}
	if dash.Missions.Total != 0 || dash.Traffic.Total != 0 || dash.Tokens.Calls != 0 {
		t.Fatalf("Dashboard 不应统计租户 B 数据: %+v", dash)
	}
}

// ---------- Store 未装配 ----------

func TestInsightsStoreUnavailable(t *testing.T) {
	h := newInsightsHarness(t)
	srv := h.newServer(t, false, true)
	for _, path := range []string{
		"/api/v1/traffic",
		"/api/v1/commands",
		"/api/v1/llm-records",
		"/api/v1/dashboard",
	} {
		status, raw, _ := doAt(t, srv.Client(), srv.URL, http.MethodGet, path, testAPIToken, "")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503（body=%s）", path, status, raw)
		}
	}
}
