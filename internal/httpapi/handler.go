// Package httpapi 组装 HTTP 层：路由、中间件与处理器。
//
// 路由表是声明式且导出的，之后可以支撑自动生成的 API 文档与契约测试，
// 而不会悄悄发生漂移。
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/events"
	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/app/executor"
	"github.com/orzl1/BeaconX/internal/app/runner"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/internal/infra/selfupdate"
	"github.com/orzl1/BeaconX/internal/tenant"
)

// Authenticator 校验请求。具体实现随 auth 里程碑引入；M0 只需要这一接口，
// 以便受保护路由在缺少认证时默认拒绝（fail closed）。
type Authenticator interface {
	Authenticate(r *http.Request) error
}

// TenantResolver 为请求选择租户。v1 使用静态解析器；后续实现将依据
// 已认证主体推导租户。
type TenantResolver interface {
	Resolve(r *http.Request) tenant.ID
}

// StaticTenant 将所有请求解析到同一个租户。
type StaticTenant tenant.ID

// Resolve 实现 TenantResolver。
func (s StaticTenant) Resolve(*http.Request) tenant.ID { return tenant.ID(s) }

// Route 是一个 HTTP 端点。处理器在 NewHandler 中依据同一张表绑定，
// 因此导出面与实际服务的接口面不会漂移。
type Route struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	Summary string `json:"summary"`
	Public  bool   `json:"public"`
}

// HandlerFunc 是路由绑定器使用的签名。
type HandlerFunc func(w http.ResponseWriter, r *http.Request)

// Options 配置 HTTP 处理器。
type Options struct {
	Logger         *slog.Logger
	RequestTimeout time.Duration
	MaxBodyBytes   int64
	TenantResolver TenantResolver
	Authenticator  Authenticator
	Info           func(ctx context.Context) any
	Ready          func(ctx context.Context) (any, bool)

	// M2 任务运行时依赖；缺省时相关接口返回 503。
	Store    *pg.Store
	Engine   *runner.Engine
	Broker   *events.Broker
	Evidence *evidence.Store
	// Executor 是工具调用的唯一执行入口（策略门 + 审批 + 审计 + 采集）。
	// 依赖它的接口（如流量重放）在未接线时必须返回 503，不能静默旁路。
	Executor *executor.Executor
	Updater  *selfupdate.Updater
	WebUI    http.Handler
	TenantID int64
}

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// RequestID 返回 ctx 中保存的请求标识。
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// Routes 是声明式路由表。
//
// 新增端点必须先加在这里；当某条路由没有对应处理器时绑定器会拒绝启动，
// 从而保持路由表与服务端一致。
func Routes() []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: "/healthz", Summary: "存活探针", Public: true},
		{Method: http.MethodGet, Pattern: "/readyz", Summary: "就绪探针", Public: true},
		{Method: http.MethodGet, Pattern: "/api/v1/system/info", Summary: "构建与数据库 schema 信息", Public: true},
		{Method: http.MethodGet, Pattern: "/api/v1/system/tenant", Summary: "解析后的租户", Public: true},
		{Method: http.MethodPost, Pattern: "/api/v1/missions", Summary: "创建任务"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions", Summary: "任务列表"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}", Summary: "任务详情"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/pause", Summary: "暂停任务"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/resume", Summary: "恢复任务"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/cancel", Summary: "取消任务"},
		{Method: http.MethodDelete, Pattern: "/api/v1/missions/{id}", Summary: "删除任务"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/events", Summary: "任务事件流（SSE）"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/audit", Summary: "任务工具调用审计"},
		{Method: http.MethodGet, Pattern: "/api/v1/approvals", Summary: "审批列表"},
		{Method: http.MethodPost, Pattern: "/api/v1/approvals/{id}/decide", Summary: "审批决策"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/traffic", Summary: "任务流量采集记录"},
		{Method: http.MethodPost, Pattern: "/api/v1/traffic/{id}/evidence", Summary: "把采集记录冻结为证据"},
		{Method: http.MethodGet, Pattern: "/api/v1/evidence/{id}/verify", Summary: "校验证据完整性"},
		// M6.2-B1：企业、企业范围、资产与任务资产/覆盖度。
		{Method: http.MethodGet, Pattern: "/api/v1/companies", Summary: "企业列表（?q= 名称/slug 模糊搜索）"},
		{Method: http.MethodPost, Pattern: "/api/v1/companies", Summary: "创建企业"},
		{Method: http.MethodGet, Pattern: "/api/v1/companies/{id}", Summary: "企业详情"},
		{Method: http.MethodPatch, Pattern: "/api/v1/companies/{id}", Summary: "更新企业（name/slug 可选）"},
		{Method: http.MethodDelete, Pattern: "/api/v1/companies/{id}", Summary: "删除企业（级联删除范围）"},
		{Method: http.MethodGet, Pattern: "/api/v1/companies/{id}/scope", Summary: "企业资产范围列表"},
		{Method: http.MethodPost, Pattern: "/api/v1/companies/{id}/scope", Summary: "新增企业资产范围"},
		{Method: http.MethodDelete, Pattern: "/api/v1/companies/{id}/scope/{scope_id}", Summary: "删除企业资产范围"},
		{Method: http.MethodGet, Pattern: "/api/v1/assets", Summary: "资产列表（?type= 精确、?q= 标签模糊）"},
		{Method: http.MethodPost, Pattern: "/api/v1/assets", Summary: "创建资产（可同时写入身份键）"},
		{Method: http.MethodGet, Pattern: "/api/v1/assets/{id}", Summary: "资产详情（含身份键）"},
		{Method: http.MethodPatch, Pattern: "/api/v1/assets/{id}", Summary: "更新资产（label/parent_id/attrs 可选）"},
		{Method: http.MethodDelete, Pattern: "/api/v1/assets/{id}", Summary: "删除资产（级联身份与任务关联）"},
		{Method: http.MethodGet, Pattern: "/api/v1/assets/{id}/identities", Summary: "资产身份键列表"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/assets", Summary: "任务已登记资产"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/assets", Summary: "把资产登记到任务"},
		{Method: http.MethodDelete, Pattern: "/api/v1/missions/{id}/assets/{asset_id}", Summary: "解除任务资产关联"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/coverage", Summary: "任务资产覆盖度"},
		// M6.2-B2：任务范围、计划、作战图与节点。
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/scope", Summary: "任务范围列表"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/scope", Summary: "新增任务范围"},
		{Method: http.MethodDelete, Pattern: "/api/v1/missions/{id}/scope/{scope_id}", Summary: "删除任务范围"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/plan", Summary: "任务计划步骤"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/graph", Summary: "任务作战图（节点/边/锚点，支持 ?kind=&state=&limit=）"},
		{Method: http.MethodGet, Pattern: "/api/v1/nodes/{id}", Summary: "作战图节点详情"},
		{Method: http.MethodPatch, Pattern: "/api/v1/nodes/{id}/state", Summary: "更新作战图节点状态"},
		{Method: http.MethodGet, Pattern: "/api/v1/system/update/check", Summary: "检查新版本"},
		{Method: http.MethodPost, Pattern: "/api/v1/system/update/apply", Summary: "下载并安装新版本"},
		{Method: http.MethodPost, Pattern: "/api/v1/system/update/rollback", Summary: "回滚到上一版本"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/findings", Summary: "发现列表"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/findings", Summary: "创建发现"},
		{Method: http.MethodGet, Pattern: "/api/v1/findings/{id}", Summary: "发现详情"},
		{Method: http.MethodPatch, Pattern: "/api/v1/findings/{id}", Summary: "更新发现"},
		{Method: http.MethodPost, Pattern: "/api/v1/findings/{id}/status", Summary: "发现状态流转"},
		{Method: http.MethodGet, Pattern: "/api/v1/findings/{id}/evidence", Summary: "发现关联证据"},
		{Method: http.MethodPost, Pattern: "/api/v1/findings/{id}/evidence", Summary: "绑定证据到发现"},
		{Method: http.MethodDelete, Pattern: "/api/v1/findings/{id}/evidence/{snapshot_id}", Summary: "解除证据绑定"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/retests", Summary: "复测列表"},
		{Method: http.MethodPost, Pattern: "/api/v1/missions/{id}/retests", Summary: "发起复测"},
		{Method: http.MethodGet, Pattern: "/api/v1/retests/{id}", Summary: "复测详情"},
		{Method: http.MethodPatch, Pattern: "/api/v1/retests/{id}", Summary: "更新复测结果"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/report", Summary: "生成任务 Markdown 报告"},
		// M6.2-B3：流量/命令/LLM 记录/用量/Dashboard。
		{Method: http.MethodGet, Pattern: "/api/v1/traffic", Summary: "流量采集记录列表（?mission_id=&tool=&phase=&limit=）"},
		{Method: http.MethodGet, Pattern: "/api/v1/traffic/{id}", Summary: "流量采集详情"},
		{Method: http.MethodGet, Pattern: "/api/v1/traffic/{id}/request", Summary: "流量请求正文（解密）"},
		{Method: http.MethodGet, Pattern: "/api/v1/traffic/{id}/response", Summary: "流量响应正文（解密）"},
		{Method: http.MethodDelete, Pattern: "/api/v1/traffic/{id}", Summary: "删除流量采集记录"},
		{Method: http.MethodGet, Pattern: "/api/v1/commands", Summary: "命令执行历史（?mission_id=&tool=&outcome=&limit=）"},
		{Method: http.MethodGet, Pattern: "/api/v1/commands/{id}", Summary: "命令执行详情"},
		{Method: http.MethodGet, Pattern: "/api/v1/llm-records", Summary: "LLM 调用记录（?mission_id=&role=&model=&limit=）"},
		{Method: http.MethodGet, Pattern: "/api/v1/missions/{id}/usage", Summary: "任务用量与聚合"},
		{Method: http.MethodGet, Pattern: "/api/v1/llm-usage/summary", Summary: "LLM 用量聚合（?group_by=model|role|day&mission_id=&days=）"},
		{Method: http.MethodGet, Pattern: "/api/v1/dashboard", Summary: "Dashboard 聚合计数"},
	}
}

// ErrUnboundRoute 表示路由表中存在没有处理器的路由。
var ErrUnboundRoute = errors.New("路由没有对应的处理器")

// NewHandler 构建完整的 HTTP 处理器。
func NewHandler(opts Options) (http.Handler, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 30 * time.Second
	}
	if opts.TenantResolver == nil {
		opts.TenantResolver = StaticTenant(tenant.DefaultID)
	}

	mux := http.NewServeMux()
	if opts.WebUI != nil {
		mux.Handle("/", opts.WebUI)
	}
	for _, rt := range Routes() {
		handler, err := opts.bind(rt)
		if err != nil {
			return nil, err
		}
		mux.Handle(rt.Method+" "+rt.Pattern, opts.withAuth(rt, handler))
	}
	// 功能模块自行注册的路由（B4/B5 等）复用同一套鉴权与兜底逻辑。
	for _, fr := range featureRouteList() {
		handler := fr.Build(opts)
		if handler == nil {
			return nil, errors.New("feature 路由工厂返回空处理器: " + fr.Method + " " + fr.Pattern)
		}
		mux.Handle(fr.Method+" "+fr.Pattern, opts.withAuth(Route{
			Method: fr.Method, Pattern: fr.Pattern, Summary: fr.Summary, Public: fr.Public,
		}, HandlerFunc(handler)))
	}
	// 未嵌入前端时，未匹配的路径也要返回 JSON 错误信封，否则默认的纯文本
	// "404 page not found" 会让客户端无法统一解析。这里把兜底路由放在最后
	// 注册，保证更具体的路径优先命中。
	if opts.WebUI == nil {
		all := append(append([]Route(nil), Routes()...), featureRouteSpecs()...)
		mux.Handle("/", newFallbackHandler(all))
	}

	var h http.Handler = mux
	h = opts.limitBody(h)
	h = opts.timeout(h)
	h = opts.withTenant(h)
	h = opts.accessLog(h)
	h = withRequestID(h)
	h = opts.recover(h)
	return h, nil
}

// newFallbackHandler 构建「未匹配路径」的兜底处理器。
//
// 两种情形分开返回，避免把「方法写错」误报成「接口不存在」：
//   - 路径存在但请求方法不在路由表中：405 + Allow 头；
//   - 路径完全不存在：404 的 JSON 错误信封。
func newFallbackHandler(routes []Route) http.Handler {
	allowed := make(map[string][]string, len(routes))
	// 用一个「任意方法」的辅助 mux 判断路径是否存在：把每条路由的 pattern
	// 去掉方法注册一遍，命中即说明该路径有处理器。
	pathMux := http.NewServeMux()
	for _, rt := range routes {
		if _, seen := allowed[rt.Pattern]; !seen {
			pathMux.Handle(rt.Pattern, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		}
		allowed[rt.Pattern] = append(allowed[rt.Pattern], rt.Method)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := pathMux.Handler(r); pattern != "" {
			if methods, ok := allowed[pattern]; ok {
				w.Header().Set("Allow", strings.Join(methods, ", "))
				writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "该路径不支持此请求方法")
				return
			}
		}
		writeError(w, r, http.StatusNotFound, "not_found", "接口不存在")
	})
}

func (o Options) bind(rt Route) (HandlerFunc, error) {
	switch rt.Pattern {
	case "/healthz":
		return handleHealthz, nil
	case "/readyz":
		return o.handleReadyz, nil
	case "/api/v1/system/info":
		return o.handleInfo, nil
	case "/api/v1/system/tenant":
		return o.handleTenant, nil
	case "/api/v1/missions":
		if rt.Method == http.MethodPost {
			return o.handleCreateMission, nil
		}
		return o.handleListMissions, nil
	case "/api/v1/missions/{id}":
		if rt.Method == http.MethodDelete {
			return o.handleDeleteMission, nil
		}
		return o.handleGetMission, nil
	case "/api/v1/missions/{id}/pause":
		return o.handlePauseMission, nil
	case "/api/v1/missions/{id}/resume":
		return o.handleResumeMission, nil
	case "/api/v1/missions/{id}/cancel":
		return o.handleCancelMission, nil
	case "/api/v1/missions/{id}/events":
		return o.handleMissionEvents, nil
	case "/api/v1/missions/{id}/audit":
		return o.handleMissionAudit, nil
	case "/api/v1/approvals":
		return o.handleListApprovals, nil
	case "/api/v1/approvals/{id}/decide":
		return o.handleDecideApproval, nil
	case "/api/v1/missions/{id}/traffic":
		return o.handleListTraffic, nil
	case "/api/v1/traffic/{id}/evidence":
		return o.handleCreateEvidence, nil
	case "/api/v1/evidence/{id}/verify":
		return o.handleVerifyEvidence, nil
	case "/api/v1/companies":
		if rt.Method == http.MethodPost {
			return o.handleCreateCompany, nil
		}
		return o.handleListCompanies, nil
	case "/api/v1/companies/{id}":
		switch rt.Method {
		case http.MethodPatch:
			return o.handleUpdateCompany, nil
		case http.MethodDelete:
			return o.handleDeleteCompany, nil
		}
		return o.handleGetCompany, nil
	case "/api/v1/companies/{id}/scope":
		if rt.Method == http.MethodPost {
			return o.handleAddCompanyScope, nil
		}
		return o.handleListCompanyScope, nil
	case "/api/v1/companies/{id}/scope/{scope_id}":
		return o.handleDeleteCompanyScope, nil
	case "/api/v1/assets":
		if rt.Method == http.MethodPost {
			return o.handleCreateAsset, nil
		}
		return o.handleListAssets, nil
	case "/api/v1/assets/{id}":
		switch rt.Method {
		case http.MethodPatch:
			return o.handleUpdateAsset, nil
		case http.MethodDelete:
			return o.handleDeleteAsset, nil
		}
		return o.handleGetAsset, nil
	case "/api/v1/assets/{id}/identities":
		return o.handleListAssetIdentities, nil
	case "/api/v1/missions/{id}/assets":
		if rt.Method == http.MethodPost {
			return o.handleAddMissionAsset, nil
		}
		return o.handleListMissionAssets, nil
	case "/api/v1/missions/{id}/assets/{asset_id}":
		return o.handleRemoveMissionAsset, nil
	case "/api/v1/missions/{id}/coverage":
		return o.handleMissionCoverage, nil
	case "/api/v1/missions/{id}/scope":
		if rt.Method == http.MethodPost {
			return o.handleAddMissionScope, nil
		}
		return o.handleListMissionScope, nil
	case "/api/v1/missions/{id}/scope/{scope_id}":
		return o.handleDeleteMissionScope, nil
	case "/api/v1/missions/{id}/plan":
		return o.handleMissionPlan, nil
	case "/api/v1/missions/{id}/graph":
		return o.handleMissionGraph, nil
	case "/api/v1/missions/{id}/findings":
		if rt.Method == http.MethodPost {
			return o.handleCreateFinding, nil
		}
		return o.handleListFindings, nil
	case "/api/v1/findings/{id}":
		if rt.Method == http.MethodPatch {
			return o.handleUpdateFinding, nil
		}
		return o.handleGetFinding, nil
	case "/api/v1/findings/{id}/status":
		return o.handleTransitionFinding, nil
	case "/api/v1/findings/{id}/evidence":
		if rt.Method == http.MethodPost {
			return o.handleAddFindingEvidence, nil
		}
		return o.handleListFindingEvidence, nil
	case "/api/v1/findings/{id}/evidence/{snapshot_id}":
		return o.handleRemoveFindingEvidence, nil
	case "/api/v1/missions/{id}/retests":
		if rt.Method == http.MethodPost {
			return o.handleCreateRetest, nil
		}
		return o.handleListRetests, nil
	case "/api/v1/retests/{id}":
		if rt.Method == http.MethodPatch {
			return o.handlePatchRetest, nil
		}
		return o.handleGetRetest, nil
	case "/api/v1/missions/{id}/report":
		return o.handleMissionReport, nil
	case "/api/v1/traffic":
		return o.handleTrafficList, nil
	case "/api/v1/traffic/{id}":
		if rt.Method == http.MethodDelete {
			return o.handleTrafficDelete, nil
		}
		return o.handleTrafficGet, nil
	case "/api/v1/traffic/{id}/request":
		return o.handleTrafficRequest, nil
	case "/api/v1/traffic/{id}/response":
		return o.handleTrafficResponse, nil
	case "/api/v1/commands":
		return o.handleCommandList, nil
	case "/api/v1/commands/{id}":
		return o.handleCommandGet, nil
	case "/api/v1/llm-records":
		return o.handleLLMRecordList, nil
	case "/api/v1/missions/{id}/usage":
		return o.handleMissionUsage, nil
	case "/api/v1/llm-usage/summary":
		return o.handleLLMUsageSummary, nil
	case "/api/v1/dashboard":
		return o.handleDashboard, nil
	case "/api/v1/nodes/{id}":
		return o.handleNodeGet, nil
	case "/api/v1/nodes/{id}/state":
		return o.handleNodePatchState, nil
	case "/api/v1/system/update/check":
		return o.handleUpdateCheck, nil
	case "/api/v1/system/update/apply":
		return o.handleUpdateApply, nil
	case "/api/v1/system/update/rollback":
		return o.handleUpdateRollback, nil
	default:
		return nil, ErrUnboundRoute
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (o Options) handleReadyz(w http.ResponseWriter, r *http.Request) {
	status := http.StatusOK
	var payload any = map[string]any{"ready": true, "checks": []any{}}
	if o.Ready != nil {
		var ready bool
		payload, ready = o.Ready(r.Context())
		if !ready {
			status = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, status, payload)
}

func (o Options) handleInfo(w http.ResponseWriter, r *http.Request) {
	if o.Info == nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "服务信息不可用")
		return
	}
	writeJSON(w, http.StatusOK, o.Info(r.Context()))
}

func (o Options) handleTenant(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tenant_id": int64(tenant.MustFrom(r.Context()))})
}

func (o Options) withAuth(rt Route, next HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !rt.Public {
			if o.Authenticator == nil {
				writeError(w, r, http.StatusServiceUnavailable, "auth_unavailable", "未配置认证")
				return
			}
			if err := o.Authenticator.Authenticate(r); err != nil {
				writeError(w, r, http.StatusUnauthorized, "unauthorized", "需要认证")
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestID(r.Context()),
	}})
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				id = "unknown"
			} else {
				id = hex.EncodeToString(b[:])
			}
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

func (o Options) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				o.Logger.Error("已恢复 panic",
					"request_id", RequestID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				writeError(w, r, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (o Options) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		o.Logger.Info("HTTP 请求",
			"request_id", RequestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"tenant", int64(tenant.MustFrom(r.Context())),
		)
	})
}

func (o Options) withTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := o.TenantResolver.Resolve(r)
		if id <= 0 {
			id = tenant.DefaultID
		}
		next.ServeHTTP(w, r.WithContext(tenant.With(r.Context(), id)))
	})
}

func (o Options) timeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), o.RequestTimeout)
		defer cancel()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		if ctx.Err() == context.DeadlineExceeded && !rec.wroteHeader {
			writeError(w, r, http.StatusGatewayTimeout, "timeout", "请求超时")
		}
	})
}

func (o Options) limitBody(next http.Handler) http.Handler {
	if o.MaxBodyBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, o.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush 保证流式处理器在日志包装层中仍能正常工作。
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
