package logging

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 本文件为进程日志增加「环形缓冲 + 实时订阅」能力，供 /system/logs 的后端接口
// 读取与推送。设计约束：
//
//   - 单例缓冲：Buffer() 返回进程级唯一实例；New 构造日志器时把 tee 挂到
//     slog handler 上，业务代码不需要知道缓冲的存在；
//   - 容量固定：只保留最近 BufferCapacity 条，内存占用可预估，不提供运行时
//     修改容量（避免「全局可变配置」带来的观测不一致）；
//   - 非阻塞订阅：SSE 客户端过慢时直接丢弃该客户端的帧，而不是阻塞写日志的
//     业务 goroutine——日志永远不能成为业务链路的背压来源；
//   - 属性简化：slog 的 Attr 统一转成 map[string]any（标量保留类型，复杂值
//     退化为字符串），保证可以稳定 JSON 序列化。

// BufferCapacity 是环形缓冲保留的最近日志条数。
const BufferCapacity = 2000

// 订阅通道缓冲长度：慢客户端在积压超过该长度后逐条丢弃。
const subscriberBuffer = 64

// Entry 是一条被缓冲的结构化日志。
//
// ID 单调递增，既用于 SSE 帧的 id 字段，也方便前端去重/增量拉取。
type Entry struct {
	ID    uint64         `json:"id"`
	Time  time.Time      `json:"time"`
	Level string         `json:"level"` // debug | info | warn | error
	Msg   string         `json:"msg"`
	Attrs map[string]any `json:"attrs"` // 恒为对象（无属性时为 {}），前端无需判空
}

// LogBuffer 是线程安全的日志环形缓冲 + 订阅中心。
type LogBuffer struct {
	mu      sync.Mutex
	ring    []Entry
	start   int
	size    int
	seq     uint64
	subs    map[uint64]chan Entry
	nextSub uint64
}

// newLogBuffer 创建指定容量的缓冲；容量非法时回退到 BufferCapacity。
func newLogBuffer(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = BufferCapacity
	}
	return &LogBuffer{
		ring: make([]Entry, capacity),
		subs: make(map[uint64]chan Entry),
	}
}

var (
	bufferOnce     sync.Once
	bufferInstance *LogBuffer
)

// Buffer 返回进程级日志缓冲单例。
//
// 单例在第一次调用时创建；容量固定为 BufferCapacity，不提供运行时修改入口。
func Buffer() *LogBuffer {
	bufferOnce.Do(func() {
		bufferInstance = newLogBuffer(BufferCapacity)
	})
	return bufferInstance
}

// append 写入一条日志并广播给所有订阅者。
//
// 广播使用非阻塞发送：订阅者积压时丢弃该条，不拖慢写日志的业务 goroutine。
func (b *LogBuffer) append(entry Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	entry.ID = b.seq
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	if b.size < len(b.ring) {
		b.ring[(b.start+b.size)%len(b.ring)] = entry
		b.size++
	} else {
		b.ring[b.start] = entry
		b.start = (b.start + 1) % len(b.ring)
	}
	for _, ch := range b.subs {
		select {
		case ch <- entry:
		default:
			// 慢订阅者丢帧：日志流是可观测性辅助，不允许反压业务。
		}
	}
}

// Recent 返回最近 limit 条日志（按时间升序，旧→新）。
//
// level 非空时只返回该级别；limit<=0 时返回全部缓冲内容。返回值恒为非 nil。
func (b *LogBuffer) Recent(level string, limit int) []Entry {
	level = strings.ToLower(strings.TrimSpace(level))
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Entry, 0, b.size)
	for i := b.size - 1; i >= 0; i-- {
		entry := b.ring[(b.start+i)%len(b.ring)]
		if level != "" && entry.Level != level {
			continue
		}
		out = append(out, entry)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	// 上面是从新到旧收集，翻转为旧→新，和 SSE 推送顺序一致。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Len 返回当前缓冲的日志条数（用于测试与诊断）。
func (b *LogBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.size
}

// Subscribe 订阅实时日志。
//
// 返回只读通道与取消函数；取消函数幂等，调用后通道会被关闭。订阅者如果
// 处理不过来，通道满时新日志会被丢弃而不是阻塞写入方。
func (b *LogBuffer) Subscribe() (<-chan Entry, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	id := b.nextSub
	b.nextSub++
	ch := make(chan Entry, subscriberBuffer)
	b.subs[id] = ch

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
	}
	return ch, cancel
}

// reset 清空缓冲内容（仅测试使用；订阅关系保持不变）。
func (b *LogBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ring = make([]Entry, len(b.ring))
	b.start = 0
	b.size = 0
	b.seq = 0
}

// ---------- slog tee ----------

// newTeeHandler 包装底层 handler：日志照常输出到原目标，同时写入环形缓冲。
func newTeeHandler(base slog.Handler, buf *LogBuffer) slog.Handler {
	return &teeHandler{base: base, buf: buf}
}

// teeHandler 是「原样输出 + 缓冲」的 slog.Handler 装饰器。
type teeHandler struct {
	base slog.Handler
	buf  *LogBuffer
	// pre 是 WithAttrs 预置属性按当时分组前缀展开后的结果；之后 WithGroup
	// 不会反过来给这些属性加上新前缀（slog 的语义如此）。
	pre    map[string]any
	groups []string
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.base.Handle(ctx, record)

	prefix := strings.Join(h.groups, ".")
	attrs := make(map[string]any, len(h.pre)+record.NumAttrs())
	for key, value := range h.pre {
		attrs[key] = value
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(attrs, prefix, attr)
		return true
	})
	h.buf.append(Entry{
		Time:  record.Time,
		Level: levelName(record.Level),
		Msg:   record.Message,
		Attrs: attrs,
	})
	return err
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make(map[string]any, len(h.pre)+len(attrs))
	for key, value := range h.pre {
		merged[key] = value
	}
	prefix := strings.Join(h.groups, ".")
	for _, attr := range attrs {
		appendAttr(merged, prefix, attr)
	}
	return &teeHandler{
		base:   h.base.WithAttrs(attrs),
		buf:    h.buf,
		pre:    merged,
		groups: h.groups,
	}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &teeHandler{
		base:   h.base.WithGroup(name),
		buf:    h.buf,
		pre:    h.pre,
		groups: groups,
	}
}

// levelName 把 slog 级别归一到四个可筛选的档位；自定义级别按区间归档。
func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// appendAttr 把一个 slog.Attr 展开进 map；分组用「父.子」拼成扁平键，
// 这样 JSON 侧结构稳定，不需要为嵌套分组引入递归 schema。
func appendAttr(dst map[string]any, prefix string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	key := attr.Key
	if prefix != "" && key != "" {
		key = prefix + "." + key
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, sub := range attr.Value.Group() {
			appendAttr(dst, key, sub)
		}
		return
	}
	if key == "" {
		return
	}
	dst[key] = simplifyValue(attr.Value)
}

// simplifyValue 把 slog.Value 转成可稳定 JSON 序列化的值。
//
// 标量保留原类型；时间统一为 UTC RFC3339Nano；error/Stringer 取字符串；
// 其余复杂值退化为 fmt 字符串（与「属性简化」的取舍一致）。
func simplifyValue(value slog.Value) any {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindInt64:
		return value.Int64()
	case slog.KindUint64:
		return value.Uint64()
	case slog.KindFloat64:
		return value.Float64()
	case slog.KindBool:
		return value.Bool()
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindTime:
		return value.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		switch typed := value.Any().(type) {
		case error:
			return typed.Error()
		case fmt.Stringer:
			return typed.String()
		default:
			return fmt.Sprintf("%v", typed)
		}
	default:
		return fmt.Sprintf("%v", value.Any())
	}
}
