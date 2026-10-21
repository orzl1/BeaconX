// Package httpapi wires the HTTP surface: routes, middleware and handlers.
//
// The route table is declarative and exported so it can later back generated
// API documentation and contract tests instead of drifting silently.
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

// Authenticator validates a request. Implementations arrive with the auth
// milestone; M0 only needs the interface so protected routes fail closed.
type Authenticator interface {
	Authenticate(r *http.Request) error
}

// TenantResolver chooses the tenant for a request. v1 uses a static resolver;
// later implementations derive it from the authenticated principal.
type TenantResolver interface {
	Resolve(r *http.Request) tenant.ID
}

// StaticTenant resolves every request to one tenant.
type StaticTenant tenant.ID

// Resolve implements TenantResolver.
func (s StaticTenant) Resolve(*http.Request) tenant.ID { return tenant.ID(s) }

// Route is one HTTP endpoint. Handlers are bound in NewHandler from the same
// table, so the exported surface and the served surface cannot drift.
type Route struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	Summary string `json:"summary"`
	Public  bool   `json:"public"`
}

// HandlerFunc is the signature used by the route binder.
type HandlerFunc func(w http.ResponseWriter, r *http.Request)

// Options configures the HTTP handler.
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

// RequestID returns the request identifier stored in ctx.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

// Routes is the declarative route table for M0.
//
// New endpoints must be added here first; the binder refuses to start when a
// route has no handler, which keeps the table and the server in sync.
func Routes() []Route {
	return []Route{
		{Method: http.MethodGet, Pattern: "/healthz", Summary: "Liveness probe", Public: true},
		{Method: http.MethodGet, Pattern: "/readyz", Summary: "Readiness probe", Public: true},
		{Method: http.MethodGet, Pattern: "/api/v1/system/info", Summary: "Build and schema info", Public: true},
		{Method: http.MethodGet, Pattern: "/api/v1/system/tenant", Summary: "Resolved tenant", Public: true},
	}
}

// ErrUnboundRoute is returned when the table contains a route without a handler.
var ErrUnboundRoute = errors.New("route has no handler")

// NewHandler builds the full HTTP handler.
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
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "service info is not available")
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
				writeError(w, r, http.StatusServiceUnavailable, "auth_unavailable", "authentication is not configured")
				return
			}
			if err := o.Authenticator.Authenticate(r); err != nil {
				writeError(w, r, http.StatusUnauthorized, "unauthorized", "authentication required")
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
				o.Logger.Error("panic recovered",
					"request_id", RequestID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				writeError(w, r, http.StatusInternalServerError, "internal_error", "internal server error")
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
		o.Logger.Info("http request",
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
			writeError(w, r, http.StatusGatewayTimeout, "timeout", "request timed out")
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

// Flush keeps streaming handlers working through the logging wrappers.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
