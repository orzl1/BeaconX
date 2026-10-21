// Package events 提供进程内的事件总线，用于把任务事件广播给 SSE 订阅者。
//
// 总线是尽力分发：发布永不阻塞，订阅者缓冲满时会被丢弃（关闭其通道），
// 因此慢客户端不会拖垮事件生产路径。遗漏的事件由持久化存储负责，SSE 处理器
// 在订阅之后通过回放补齐。
package events

import (
	"encoding/json"
	"sync"
	"time"
)

// Event 是总线上的一个任务事件。
type Event struct {
	ID        int64           `json:"id"`
	MissionID int64           `json:"mission_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// defaultBuffer 是订阅者通道的默认缓冲大小。
const defaultBuffer = 256

// Broker 是按任务分发事件的内存总线，可被多个 goroutine 并发使用。
//
// 每个订阅者持有独立的缓冲通道；Publish 对缓冲已满的订阅者采取“丢弃该
// 订阅者”的策略，而不是阻塞发布方。被丢弃的订阅者会看到通道关闭，SSE
// 处理器据此结束流，客户端重连后再通过回放补洞。
type Broker struct {
	mu     sync.Mutex
	buffer int
	subs   map[int64]map[*subscriber]struct{}
	closed bool
}

// subscriber 是一个订阅者的状态。
//
// 通道只允许在持有 Broker.mu 时关闭；once 保证取消、丢弃、关闭总线三条
// 路径同时发生时也只关闭一次。
type subscriber struct {
	ch   chan Event
	once sync.Once
}

func (s *subscriber) close() {
	s.once.Do(func() { close(s.ch) })
}

// NewBroker 创建事件总线；buffer 为单个订阅者的通道缓冲大小，<=0 时使用 256。
func NewBroker(buffer int) *Broker {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	return &Broker{
		buffer: buffer,
		subs:   make(map[int64]map[*subscriber]struct{}),
	}
}

// Subscribe 订阅某个任务的事件，返回事件通道与取消函数。
//
// 取消函数是幂等的，可重复调用。通道在以下情况被关闭：调用取消函数、
// 总线 Close、或订阅者积压过多被 Publish 丢弃。总线已关闭时返回一个
// 已关闭的通道，取消函数为空操作。
func (b *Broker) Subscribe(missionID int64) (<-chan Event, func()) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	sub := &subscriber{ch: make(chan Event, b.buffer)}
	if b.subs[missionID] == nil {
		b.subs[missionID] = make(map[*subscriber]struct{})
	}
	b.subs[missionID][sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() { b.unsubscribe(missionID, sub) })
	}
	return sub.ch, cancel
}

// unsubscribe 移除订阅者并关闭其通道；重复调用是安全的。
func (b *Broker) unsubscribe(missionID int64, sub *subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if set, ok := b.subs[missionID]; ok {
		delete(set, sub)
		if len(set) == 0 {
			delete(b.subs, missionID)
		}
	}
	sub.close()
}

// Publish 把事件分发给该任务的所有订阅者。
//
// 该调用永不阻塞：若某个订阅者的缓冲已满，则把该订阅者从总线中移除并关闭
// 其通道。事件按值复制，多个订阅者可以安全地读取同一份数据。
func (b *Broker) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	set := b.subs[ev.MissionID]
	for sub := range set {
		select {
		case sub.ch <- ev:
		default:
			// 慢消费者：丢弃并关闭，避免拖慢发布方。
			delete(set, sub)
			sub.close()
		}
	}
	if len(set) == 0 {
		delete(b.subs, ev.MissionID)
	}
}

// Subscribers 返回某个任务当前的订阅者数量。
func (b *Broker) Subscribers(missionID int64) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[missionID])
}

// Close 关闭总线：关闭所有订阅者通道，之后 Publish 为空操作。
//
// Close 是幂等的，可重复调用。
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, set := range b.subs {
		for sub := range set {
			sub.close()
		}
	}
	b.subs = make(map[int64]map[*subscriber]struct{})
}
