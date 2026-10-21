package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/events"
)

// sseTestHooks 记录处理器对回调的调用情况，供测试断言。
type sseTestHooks struct {
	live       chan events.Event
	cancels    atomic.Int32
	subscribes atomic.Int32
	missionID  atomic.Int64
	afterID    atomic.Int64
	limit      atomic.Int64
	replay     []events.Event
	replayErr  error
}

func newSSETestHooks() *sseTestHooks {
	return &sseTestHooks{live: make(chan events.Event, 16)}
}

func (h *sseTestHooks) options() SSEOptions {
	return SSEOptions{
		MissionID: func(*http.Request) (int64, error) { return 42, nil },
		Subscribe: func(missionID int64) (<-chan events.Event, func()) {
			h.subscribes.Add(1)
			return h.live, func() { h.cancels.Add(1) }
		},
		Replay: func(_ context.Context, missionID, afterID int64, limit int) ([]events.Event, error) {
			h.missionID.Store(missionID)
			h.afterID.Store(afterID)
			h.limit.Store(int64(limit))
			if h.replayErr != nil {
				return nil, h.replayErr
			}
			return h.replay, nil
		},
	}
}

// sseTestFrame 是一帧解析后的 SSE 数据。
type sseTestFrame struct {
	id    string
	event string
	data  []string
}

func (f sseTestFrame) dataText() string { return strings.Join(f.data, "\n") }

// readSSEFrame 读取一帧完整的 SSE；注释行（心跳）会被跳过。
func readSSEFrame(t *testing.T, r *bufio.Reader) sseTestFrame {
	t.Helper()
	var frame sseTestFrame
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("读取 SSE 帧失败：%v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if frame.id == "" && frame.event == "" && len(frame.data) == 0 {
				continue
			}
			return frame
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			frame.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = append(frame.data, strings.TrimPrefix(line, "data: "))
		default:
			t.Fatalf("无法解析的 SSE 行：%q", line)
		}
	}
}

// startSSERequest 启动临时服务器并建立 SSE 连接。
func startSSERequest(t *testing.T, opts SSEOptions, target string, headers map[string]string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	srv := httptest.NewServer(opts.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+target, nil)
	if err != nil {
		cancel()
		t.Fatalf("构造请求失败：%v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("建立 SSE 连接失败：%v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, bufio.NewReader(resp.Body), cancel
}

// waitFor 在限定时间内等待条件成立。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func TestSSEReplayThenLiveDedup(t *testing.T) {
	hooks := newSSETestHooks()
	hooks.replay = []events.Event{
		{ID: 4, MissionID: 42, Type: "node.done", Payload: json.RawMessage(`{"n":4}`)},
		{ID: 2, MissionID: 42, Type: "node.old"},
		{ID: 3, MissionID: 42, Type: "node.open", Payload: json.RawMessage(`{"n":3}`)},
		{ID: 1, MissionID: 42, Type: "node.old"},
	}
	resp, reader, cancel := startSSERequest(t, hooks.options(), "/events?since=1", map[string]string{
		"Last-Event-ID": "2",
	})
	defer cancel()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q", got)
	}

	// 回放按 id 升序输出，跳过 <= 游标的重复项。
	frame3 := readSSEFrame(t, reader)
	frame4 := readSSEFrame(t, reader)
	if frame3.id != "3" || frame3.event != "node.open" || frame3.dataText() != `{"n":3}` {
		t.Fatalf("第一帧 = %+v", frame3)
	}
	if frame4.id != "4" || frame4.event != "node.done" || frame4.dataText() != `{"n":4}` {
		t.Fatalf("第二帧 = %+v", frame4)
	}
	if got := hooks.afterID.Load(); got != 2 {
		t.Fatalf("回放游标 = %d，期望 2（Last-Event-ID 优先于 since）", got)
	}
	if got := hooks.limit.Load(); got != defaultSSEReplayLimit {
		t.Fatalf("默认回放上限 = %d，期望 %d", got, defaultSSEReplayLimit)
	}
	if got := hooks.missionID.Load(); got != 42 {
		t.Fatalf("任务标识 = %d，期望 42", got)
	}
	if got := hooks.cancels.Load(); got != 0 {
		t.Fatalf("流仍在进行时取消函数不应被调用，实际 %d 次", got)
	}

	// 实时事件：重复 id 4 被跳过，5、6 依次输出。
	hooks.live <- events.Event{ID: 4, MissionID: 42, Type: "node.done"}
	hooks.live <- events.Event{ID: 5, MissionID: 42, Type: "node.live", Payload: json.RawMessage(`{"live":5}`)}
	hooks.live <- events.Event{ID: 6, MissionID: 42, Type: "node.live"}
	frame5 := readSSEFrame(t, reader)
	frame6 := readSSEFrame(t, reader)
	if frame5.id != "5" || frame5.dataText() != `{"live":5}` {
		t.Fatalf("实时第一帧 = %+v", frame5)
	}
	if frame6.id != "6" {
		t.Fatalf("实时第二帧 = %+v", frame6)
	}

	cancel()
	waitFor(t, func() bool { return hooks.cancels.Load() >= 1 })
}

func TestSSECursorPriorityAndParsing(t *testing.T) {
	tests := []struct {
		name   string
		header string
		since  string
		want   int64
	}{
		{name: "Last-Event-ID 优先", header: "2", since: "1", want: 2},
		{name: "回退到 since", header: "", since: "7", want: 7},
		{name: "非法的 Last-Event-ID 视为 0", header: "abc", since: "7", want: 0},
		{name: "负数的 since 视为 0", header: "", since: "-3", want: 0},
		{name: "空游标为 0", header: "", since: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := newSSETestHooks()
			opts := hooks.options()
			// 用已取消的上下文让处理器在写完响应头后立即退出。
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodGet, "/events?since="+tt.since, nil).WithContext(ctx)
			if tt.header != "" {
				req.Header.Set("Last-Event-ID", tt.header)
			}
			rec := httptest.NewRecorder()
			opts.Handler()(rec, req)
			if got := hooks.afterID.Load(); got != tt.want {
				t.Fatalf("回放游标 = %d，期望 %d", got, tt.want)
			}
		})
	}
}

func TestSSEReplayLimit(t *testing.T) {
	t.Run("默认值", func(t *testing.T) {
		hooks := newSSETestHooks()
		hooks.replayErr = errors.New("模拟数据库故障")
		rec := httptest.NewRecorder()
		hooks.options().Handler()(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
		if got := hooks.limit.Load(); got != defaultSSEReplayLimit {
			t.Fatalf("回放上限 = %d，期望 %d", got, defaultSSEReplayLimit)
		}
	})
	t.Run("显式设置", func(t *testing.T) {
		hooks := newSSETestHooks()
		hooks.replayErr = errors.New("模拟数据库故障")
		opts := hooks.options()
		opts.ReplayLimit = 7
		rec := httptest.NewRecorder()
		opts.Handler()(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
		if got := hooks.limit.Load(); got != 7 {
			t.Fatalf("回放上限 = %d，期望 7", got)
		}
	})
}

func TestSSEReplayErrorReturns500(t *testing.T) {
	hooks := newSSETestHooks()
	hooks.replayErr = errors.New("模拟数据库故障")
	rec := httptest.NewRecorder()
	hooks.options().Handler()(rec, httptest.NewRequest(http.MethodGet, "/events", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "replay_failed") {
		t.Fatalf("响应体 = %s", rec.Body.String())
	}
	// 失败路径同样必须调用取消函数。
	if got := hooks.cancels.Load(); got != 1 {
		t.Fatalf("取消函数调用次数 = %d，期望 1", got)
	}
}

func TestSSEInvalidMissionID(t *testing.T) {
	tests := []struct {
		name      string
		missionID func(*http.Request) (int64, error)
	}{
		{name: "零值", missionID: func(*http.Request) (int64, error) { return 0, nil }},
		{name: "负数", missionID: func(*http.Request) (int64, error) { return -1, nil }},
		{name: "解析错误", missionID: func(*http.Request) (int64, error) { return 0, errors.New("非法任务") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := newSSETestHooks()
			opts := hooks.options()
			opts.MissionID = tt.missionID
			rec := httptest.NewRecorder()
			opts.Handler()(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "bad_request") {
				t.Fatalf("响应体 = %s", rec.Body.String())
			}
			if got := hooks.subscribes.Load(); got != 0 {
				t.Fatalf("非法任务不应建立订阅，实际 %d 次", got)
			}
		})
	}
}

func TestSSEMissingCallbacks(t *testing.T) {
	t.Run("缺少任务解析器", func(t *testing.T) {
		rec := httptest.NewRecorder()
		SSEOptions{}.Handler()(rec, httptest.NewRequest(http.MethodGet, "/events", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("状态码 = %d，期望 500", rec.Code)
		}
	})
	t.Run("缺少订阅函数", func(t *testing.T) {
		rec := httptest.NewRecorder()
		SSEOptions{MissionID: func(*http.Request) (int64, error) { return 1, nil }}.Handler()(
			rec, httptest.NewRequest(http.MethodGet, "/events", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("状态码 = %d，期望 503", rec.Code)
		}
	})
}

func TestSSEHeartbeat(t *testing.T) {
	hooks := newSSETestHooks()
	opts := hooks.options()
	opts.Heartbeat = 10 * time.Millisecond
	resp, reader, cancel := startSSERequest(t, opts, "/events", nil)
	defer cancel()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("未收到心跳注释行")
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("读取心跳失败：%v", err)
		}
		if strings.TrimSpace(line) == ": ping" {
			return
		}
	}
}

func TestSSEClientDisconnectCancelsSubscription(t *testing.T) {
	hooks := newSSETestHooks()
	hooks.replay = []events.Event{{ID: 1, MissionID: 42, Type: "node.open"}}
	resp, reader, cancel := startSSERequest(t, hooks.options(), "/events", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if frame := readSSEFrame(t, reader); frame.id != "1" {
		t.Fatalf("回放帧 = %+v", frame)
	}

	cancel()
	_ = resp.Body.Close()
	waitFor(t, func() bool { return hooks.cancels.Load() >= 1 })
}

func TestWriteEventFrame(t *testing.T) {
	t.Run("单行 payload 原样输出", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ev := events.Event{ID: 7, Type: "node.done", Payload: json.RawMessage(`{"x":1}`)}
		if err := writeEventFrame(rec, ev); err != nil {
			t.Fatalf("写帧失败：%v", err)
		}
		want := "id: 7\nevent: node.done\ndata: {\"x\":1}\n\n"
		if got := rec.Body.String(); got != want {
			t.Fatalf("帧 = %q，期望 %q", got, want)
		}
	})
	t.Run("多行 payload 拆成多条 data 行", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ev := events.Event{ID: 8, Type: "multi", Payload: json.RawMessage("{\"a\":1}\n{\"b\":2}")}
		if err := writeEventFrame(rec, ev); err != nil {
			t.Fatalf("写帧失败：%v", err)
		}
		want := "id: 8\nevent: multi\ndata: {\"a\":1}\ndata: {\"b\":2}\n\n"
		if got := rec.Body.String(); got != want {
			t.Fatalf("帧 = %q，期望 %q", got, want)
		}
	})
	t.Run("缺少 payload 时序列化整个事件", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ev := events.Event{ID: 9, MissionID: 42, Type: "empty"}
		if err := writeEventFrame(rec, ev); err != nil {
			t.Fatalf("写帧失败：%v", err)
		}
		body := rec.Body.String()
		if !strings.HasPrefix(body, "id: 9\nevent: empty\ndata: ") {
			t.Fatalf("帧 = %q", body)
		}
		if !strings.Contains(body, `"mission_id":42`) {
			t.Fatalf("帧缺少任务标识：%q", body)
		}
		if !strings.HasSuffix(body, "\n\n") {
			t.Fatalf("帧未以空行结束：%q", body)
		}
	})
	t.Run("事件类型中的换行被清理", func(t *testing.T) {
		rec := httptest.NewRecorder()
		ev := events.Event{ID: 10, Type: "bad\nid: 99", Payload: json.RawMessage(`{}`)}
		if err := writeEventFrame(rec, ev); err != nil {
			t.Fatalf("写帧失败：%v", err)
		}
		body := rec.Body.String()
		if strings.Count(body, "\n") != 4 {
			t.Fatalf("类型中的换行未被清理：%q", body)
		}
	})
}
