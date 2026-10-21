package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/events"
)

// SSE 处理器的默认参数。
const (
	defaultSSEHeartbeat   = 20 * time.Second
	defaultSSEReplayLimit = 500
)

// SSEOptions 配置 SSE 事件流处理器。
type SSEOptions struct {
	// MissionID 从路径或查询参数解析任务标识；返回错误或非正数时响应 400。
	MissionID func(*http.Request) (int64, error)
	// Replay 回放 afterID 之后的事件（数据库补洞）；为 nil 时跳过回放。
	Replay func(ctx context.Context, missionID, afterID int64, limit int) ([]events.Event, error)
	// Subscribe 订阅实时事件；返回的函数必须幂等。
	Subscribe func(missionID int64) (<-chan events.Event, func())
	// Heartbeat 心跳间隔，<=0 时使用 20s。
	Heartbeat time.Duration
	// ReplayLimit 单次回放的事件上限，<=0 时使用 500。
	ReplayLimit int
}

// Handler 返回 SSE 事件流的处理器。
//
// 处理顺序是“先订阅、再回放、后转发”：订阅在回放之前建立，回放期间写入
// 的新事件会先进入订阅通道，之后按 id 去重补发，因此不会出现回放与实时流
// 之间的空档。游标优先取 Last-Event-ID 头，其次取 ?since= 查询参数。
func (o SSEOptions) Handler() http.HandlerFunc {
	heartbeat := o.Heartbeat
	if heartbeat <= 0 {
		heartbeat = defaultSSEHeartbeat
	}
	replayLimit := o.ReplayLimit
	if replayLimit <= 0 {
		replayLimit = defaultSSEReplayLimit
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if o.MissionID == nil {
			writeError(w, r, http.StatusInternalServerError, "sse_not_configured", "未配置任务解析器")
			return
		}
		missionID, err := o.MissionID(r)
		if err != nil || missionID <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "任务标识不合法")
			return
		}
		if o.Subscribe == nil {
			writeError(w, r, http.StatusServiceUnavailable, "sse_unavailable", "事件流不可用")
			return
		}

		afterID := parseEventCursor(r)

		// 先订阅再回放：回放执行期间产生的事件不会丢失。
		live, cancel := o.Subscribe(missionID)
		defer cancel()

		var replay []events.Event
		if o.Replay != nil {
			replay, err = o.Replay(r.Context(), missionID, afterID, replayLimit)
			if err != nil {
				// 还没有写过任何帧，仍可以返回标准的 JSON 错误。
				writeError(w, r, http.StatusInternalServerError, "replay_failed", "回放事件失败")
				return
			}
			sort.SliceStable(replay, func(i, j int) bool { return replay[i].ID < replay[j].ID })
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flushSSE(w)

		// lastSentID 同时作为去重游标：客户端已有的 id <= afterID 的事件不再补发。
		lastSentID := afterID
		for _, ev := range replay {
			if ev.ID <= lastSentID {
				continue
			}
			if err := writeEventFrame(w, ev); err != nil {
				return
			}
			lastSentID = ev.ID
			flushSSE(w)
		}

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-live:
				if !ok {
					return
				}
				if ev.MissionID != 0 && ev.MissionID != missionID {
					continue
				}
				if ev.ID <= lastSentID {
					continue
				}
				if err := writeEventFrame(w, ev); err != nil {
					return
				}
				lastSentID = ev.ID
				flushSSE(w)
			case <-ticker.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return
				}
				flushSSE(w)
			}
		}
	}
}

// parseEventCursor 解析客户端已有的事件游标。
//
// 优先使用 Last-Event-ID（浏览器断线重连时自动带上），其次使用 ?since=；
// 空值或非法值一律按 0 处理。
func parseEventCursor(r *http.Request) int64 {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("since"))
	}
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// writeEventFrame 写出一帧 SSE。payload 存在时原样作为 data（多行会拆成多条
// data: 行），否则把整个事件对象序列化为 JSON。
func writeEventFrame(w http.ResponseWriter, ev events.Event) error {
	payload := ev.Payload
	if len(payload) == 0 {
		encoded, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		payload = encoded
	}
	typ := ev.Type
	if typ == "" {
		typ = "message"
	}
	// 事件类型会直接出现在帧头里，去掉换行避免注入额外的帧。
	typ = strings.NewReplacer("\r", " ", "\n", " ").Replace(typ)

	var sb strings.Builder
	fmt.Fprintf(&sb, "id: %d\nevent: %s\n", ev.ID, typ)
	for _, line := range strings.Split(string(payload), "\n") {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	_, err := fmt.Fprint(w, sb.String())
	return err
}

// flushSSE 在底层支持时立即把缓冲刷给客户端。
func flushSSE(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
