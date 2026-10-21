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
}

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// RequestID 返回 ctx 中保存的请求标识。
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// Routes 是 M0 的声明式路由表。
//
// 新增端点必须先加在这里；当某条路由没有对应处理器时绑定器会拒绝启动，
// 从而保持路由表与服务端一致。
func Routes() []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: "/healthz", Summary: "存活探针", Public: true},
		{Method: http.MethodGet, Pattern: "/readyz", Summary: "就绪探针", Public: true},
		{Method: http.MethodGet, Pattern: "/api/v1/system/info", Summary: "构建与数据库 schema 信息", Public: true},
		{Method: http.MethodGet, Pattern: "/api/v1/system/tenant", Summary: "解析后的租户", Public: true},
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
	for _, rt := range Routes() {
		handler, err := opts.bind(rt)
		if err != nil {
			return nil, err
		}
		mux.Handle(rt.Method+" "+rt.Pattern, opts.withAuth(rt, handler))
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
