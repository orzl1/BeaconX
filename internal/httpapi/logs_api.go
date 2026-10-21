package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/logging"
)

// B4：应用日志查询与实时流（P-B4-09/10）。
//
// 数据来源是 internal/logging 的进程级环形缓冲：业务代码照常 slog 打日志，
// 这里只读，不引入第二套日志通道。路由通过 RegisterFeatureRoutes 在 init()
// 注册，由 NewHandler 在构造时注入 Options（日志接口本身不依赖 Store）。

const (
	// logsDefaultLimit 是 GET /api/v1/logs 的默认返回条数。
	logsDefaultLimit = 200
	// logsMaxLimit 是 GET /api/v1/logs 允许的最大条数。
	logsMaxLimit = 1000
)

// logsSSEHeartbeat 是日志 SSE 的心跳间隔。
//
// 生产固定 20s；集成测试可以临时改小以验证心跳帧（仅测试读写该变量）。
var logsSSEHeartbeat = 20 * time.Second

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/logs",
			Summary: "应用日志查询（?level=&limit=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleLogsList },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/logs/stream",
			Summary: "应用日志实时流（SSE，event: log）",
			Build:   func(o Options) http.HandlerFunc { return o.handleLogsStream },
		},
	)
}

// handleLogsList 返回环形缓冲中的最近日志。
//
// 响应形如 {"logs":[{"id":1,"time":"…","level":"info","msg":"…","attrs":{…}}]}；
// level 为空表示不筛选，limit 缺省 200、上限 1000（按时间升序，旧→新）。
func (o Options) handleLogsList(w http.ResponseWriter, r *http.Request) {
	level, ok := parseLogsLevel(w, r)
	if !ok {
		return
	}
	limit, ok := parseLogsLimit(w, r)
	if !ok {
		return
	}
	entries := logging.Buffer().Recent(level, limit)
	if entries == nil {
		entries = []logging.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": entries})
}

// handleLogsStream 以 SSE 推送实时日志。
//
// 帧格式：id 为日志 ID，event 固定为 log，data 为与 GET 接口相同的条目
// JSON；心跳为注释帧 `: ping`。客户端断开（请求 ctx 结束）即退出。
func (o Options) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, "stream_unavailable", "当前响应不支持流式输出")
		return
	}

	// 先订阅再写响应头：避免「已经连上但订阅尚未建立」的日志丢失窗口。
	entries, cancel := logging.Buffer().Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := logsSSEHeartbeat
	if heartbeat <= 0 {
		heartbeat = 20 * time.Second
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case entry, ok := <-entries:
			if !ok {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue // 单条序列化失败不应中断整条流
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", entry.ID, data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// parseLogsLevel 解析并校验 ?level=；空值表示不筛选。
func parseLogsLevel(w http.ResponseWriter, r *http.Request) (string, bool) {
	level := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level")))
	switch level {
	case "", "debug", "info", "warn", "error":
		return level, true
	default:
		writeError(w, r, http.StatusBadRequest, "bad_request", "level 必须是 debug、info、warn 或 error")
		return "", false
	}
}

// parseLogsLimit 解析 ?limit=：缺省 200、上限 1000；非数字返回 400。
func parseLogsLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return logsDefaultLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "limit 必须是正整数")
		return 0, false
	}
	if limit <= 0 {
		return logsDefaultLimit, true
	}
	if limit > logsMaxLimit {
		limit = logsMaxLimit
	}
	return limit, true
}
