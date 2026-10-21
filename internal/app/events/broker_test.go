package events

import (
	"encoding/json"
	"testing"
	"time"
)

// makeEvent 构造测试事件。
func makeEvent(id, mission int64, typ string) Event {
	return Event{
		ID:        id,
		MissionID: mission,
		Type:      typ,
		Payload:   json.RawMessage(`{"ok":true}`),
		CreatedAt: time.Unix(0, 0).UTC(),
	}
}

// mustReceive 在限定时间内读取一个事件。
func mustReceive(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("通道意外关闭")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("等待事件超时")
		return Event{}
	}
}

// mustBeClosed 断言通道已关闭。
func mustBeClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("通道仍可接收事件，期望已关闭")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待通道关闭超时")
	}
}

func TestBrokerFanOut(t *testing.T) {
	b := NewBroker(8)
	defer b.Close()

	ch1, cancel1 := b.Subscribe(1)
	defer cancel1()
	ch2, cancel2 := b.Subscribe(1)
	defer cancel2()
	other, cancelOther := b.Subscribe(2)
	defer cancelOther()

	if got := b.Subscribers(1); got != 2 {
		t.Fatalf("任务 1 的订阅者数量 = %d，期望 2", got)
	}
	if got := b.Subscribers(2); got != 1 {
		t.Fatalf("任务 2 的订阅者数量 = %d，期望 1", got)
	}

	b.Publish(makeEvent(10, 1, "node.open"))
	for i, ch := range []<-chan Event{ch1, ch2} {
		ev := mustReceive(t, ch)
		if ev.ID != 10 || ev.Type != "node.open" || ev.MissionID != 1 {
			t.Fatalf("订阅者 %d 收到事件 %+v", i, ev)
		}
	}
	select {
	case ev := <-other:
		t.Fatalf("任务 2 的订阅者不应收到任务 1 的事件：%+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBrokerUnsubscribeIsIdempotent(t *testing.T) {
	b := NewBroker(4)
	ch, cancel := b.Subscribe(7)
	if got := b.Subscribers(7); got != 1 {
		t.Fatalf("订阅者数量 = %d，期望 1", got)
	}
	cancel()
	cancel()
	mustBeClosed(t, ch)
	if got := b.Subscribers(7); got != 0 {
		t.Fatalf("取消后订阅者数量 = %d，期望 0", got)
	}
	b.Publish(makeEvent(1, 7, "node.open"))
	b.Close()
}

func TestBrokerSlowConsumerIsDropped(t *testing.T) {
	b := NewBroker(1)
	defer b.Close()

	ch, cancel := b.Subscribe(1)
	defer cancel()

	b.Publish(makeEvent(1, 1, "node.open"))
	// 缓冲已满，第二次发布必须丢弃该订阅者而不是阻塞发布方。
	done := make(chan struct{})
	go func() {
		b.Publish(makeEvent(2, 1, "node.done"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish 被慢消费者阻塞")
	}

	// 已缓冲的事件仍可读出，随后通道关闭。
	ev := mustReceive(t, ch)
	if ev.ID != 1 {
		t.Fatalf("收到事件 ID = %d，期望 1", ev.ID)
	}
	mustBeClosed(t, ch)
	if got := b.Subscribers(1); got != 0 {
		t.Fatalf("慢消费者被丢弃后订阅者数量 = %d，期望 0", got)
	}
}

func TestBrokerClose(t *testing.T) {
	b := NewBroker(4)
	ch1, _ := b.Subscribe(1)
	ch2, _ := b.Subscribe(2)

	b.Close()
	b.Close() // 幂等

	mustBeClosed(t, ch1)
	mustBeClosed(t, ch2)
	if got := b.Subscribers(1); got != 0 {
		t.Fatalf("关闭后订阅者数量 = %d，期望 0", got)
	}

	// 关闭后发布与订阅都不应 panic。
	b.Publish(makeEvent(1, 1, "node.open"))
	late, cancelLate := b.Subscribe(1)
	defer cancelLate()
	mustBeClosed(t, late)
}

func TestBrokerDefaultBuffer(t *testing.T) {
	for _, buffer := range []int{0, -3} {
		b := NewBroker(buffer)
		ch, cancel := b.Subscribe(1)
		if got := cap(ch); got != defaultBuffer {
			t.Fatalf("buffer=%d 时通道容量 = %d，期望 %d", buffer, got, defaultBuffer)
		}
		cancel()
		b.Close()
	}
}
