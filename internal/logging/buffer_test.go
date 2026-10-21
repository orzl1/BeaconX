package logging

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// newTestBuffer 创建小容量缓冲，避免测试依赖 2000 条的真实容量。
func newTestBuffer(t *testing.T, capacity int) *LogBuffer {
	t.Helper()
	return newLogBuffer(capacity)
}

// TestRingBufferKeepsNewestAndAssignsIncreasingIDs 验证环形覆盖与 ID 单调递增。
func TestRingBufferKeepsNewestAndAssignsIncreasingIDs(t *testing.T) {
	buf := newTestBuffer(t, 4)
	for _, msg := range []string{"a", "b", "c", "d", "e", "f"} {
		buf.append(Entry{Time: time.Now(), Level: "info", Msg: msg})
	}
	if got := buf.Len(); got != 4 {
		t.Fatalf("Len = %d，期望容量 4", got)
	}
	entries := buf.Recent("", 0)
	if len(entries) != 4 {
		t.Fatalf("Recent 返回 %d 条，期望 4", len(entries))
	}
	want := []string{"c", "d", "e", "f"}
	for i, msg := range want {
		if entries[i].Msg != msg {
			t.Errorf("entries[%d].Msg = %q，期望 %q（按旧→新）", i, entries[i].Msg, msg)
		}
	}
	if entries[0].ID != 3 || entries[3].ID != 6 {
		t.Errorf("ID 范围 = %d..%d，期望 3..6", entries[0].ID, entries[3].ID)
	}
}

// TestRecentFiltersLevelAndLimit 验证级别过滤与条数上限。
func TestRecentFiltersLevelAndLimit(t *testing.T) {
	buf := newTestBuffer(t, 16)
	levels := []string{"debug", "info", "warn", "error", "info", "warn"}
	for _, level := range levels {
		buf.append(Entry{Time: time.Now(), Level: level, Msg: level})
	}
	if got := buf.Recent("warn", 0); len(got) != 2 {
		t.Errorf("warn 条目 = %d，期望 2", len(got))
	}
	if got := buf.Recent("", 2); len(got) != 2 || got[1].Msg != "warn" {
		t.Errorf("取最近 2 条 = %+v，期望以 warn 结尾", got)
	}
	if got := buf.Recent("error", 100); len(got) != 1 || got[0].Msg != "error" {
		t.Errorf("error 过滤 = %+v", got)
	}
	if got := buf.Recent("bogus", 0); len(got) != 0 {
		t.Errorf("未知级别应返回空，实际 %d 条", len(got))
	}
	if got := buf.Recent("", 0); got == nil {
		t.Error("Recent 返回值不应为 nil")
	}
}

// TestSubscribeReceivesEntriesAndCancelCloses 验证订阅广播与取消语义。
func TestSubscribeReceivesEntriesAndCancelCloses(t *testing.T) {
	buf := newTestBuffer(t, 8)
	ch, cancel := buf.Subscribe()
	buf.append(Entry{Time: time.Now(), Level: "info", Msg: "live"})

	select {
	case entry := <-ch:
		if entry.Msg != "live" {
			t.Fatalf("收到 %q，期望 live", entry.Msg)
		}
	case <-time.After(time.Second):
		t.Fatal("订阅者未收到实时日志")
	}

	cancel()
	cancel() // 幂等
	if _, ok := <-ch; ok {
		t.Error("取消后通道应关闭")
	}
}

// TestTeeHandlerCapturesGroupedAttrs 验证 tee 会把预置属性、分组与单条属性
// 简化进缓冲条目，同时不影响底层 handler。
func TestTeeHandlerCapturesGroupedAttrs(t *testing.T) {
	buf := newTestBuffer(t, 8)
	base := slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(newTeeHandler(base, buf)).With("mission_id", 7).WithGroup("tool")
	logger.Info("探测完成", "name", "probe", "err", errors.New("boom"), "dur", time.Second, "ok", true)

	entries := buf.Recent("", 1)
	if len(entries) != 1 {
		t.Fatalf("缓冲条目 = %d，期望 1", len(entries))
	}
	entry := entries[0]
	if entry.Msg != "探测完成" || entry.Level != "info" {
		t.Fatalf("条目 = %+v", entry)
	}
	checks := map[string]any{
		"mission_id": int64(7),
		"tool.name":  "probe",
		"tool.err":   "boom",
		"tool.dur":   "1s",
		"tool.ok":    true,
	}
	for key, want := range checks {
		if got := entry.Attrs[key]; got != want {
			t.Errorf("attrs[%q] = %#v，期望 %#v", key, got, want)
		}
	}
}

// TestTeeHandlerAppendsAttrsFromWith 验证 WithAttrs 预置属性也会进入缓冲。
func TestTeeHandlerAppendsAttrsFromWith(t *testing.T) {
	buf := newTestBuffer(t, 4)
	base := slog.NewTextHandler(io.Discard, nil)
	logger := slog.New(newTeeHandler(base, buf)).With("component", "test")
	logger.Warn("带预置属性")

	entries := buf.Recent("warn", 1)
	if len(entries) != 1 {
		t.Fatalf("缓冲条目 = %d，期望 1", len(entries))
	}
	if got := entries[0].Attrs["component"]; got != "test" {
		t.Errorf("component = %#v，期望 test", got)
	}
}

// TestLevelNameBuckets 验证自定义 slog 级别按区间归档。
func TestLevelNameBuckets(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug:     "debug",
		slog.LevelInfo:      "info",
		slog.LevelWarn:      "warn",
		slog.LevelError:     "error",
		slog.LevelInfo + 2:  "info",
		slog.LevelError + 4: "error",
	}
	for level, want := range cases {
		if got := levelName(level); got != want {
			t.Errorf("levelName(%v) = %q，期望 %q", level, got, want)
		}
	}
}

// TestBufferSingletonIsStable 验证 Buffer() 返回同一实例且容量固定。
func TestBufferSingletonIsStable(t *testing.T) {
	first := Buffer()
	second := Buffer()
	if first != second {
		t.Fatal("Buffer() 应返回同一实例")
	}
	if got := cap(first.ring); got != BufferCapacity {
		t.Errorf("容量 = %d，期望 %d", got, BufferCapacity)
	}
}
