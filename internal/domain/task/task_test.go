package task

import (
	"errors"
	"testing"
	"time"
)

// equalEffects 比较两个副作用列表：nil 与空切片视为相等（都表示"无副作用"）。
func equalEffects(got, want []Effect) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestStatusValid 覆盖全部合法状态与若干非法取值。
func TestStatusValid(t *testing.T) {
	valid := []Status{
		StatusQueued, StatusRunning, StatusPaused, StatusFinalizing,
		StatusDone, StatusFailed, StatusCancelled,
	}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("状态 %q 应合法", string(s))
		}
		if s.String() == string(s) {
			t.Errorf("状态 %q 缺少中文字称", string(s))
		}
	}
	invalid := []Status{"", "Queued", "queued ", "done ", "finalized", "canceling", "unknown"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("状态 %q 应非法", string(s))
		}
		if s.String() != string(s) {
			t.Errorf("未知状态 %q 的中文字称应回退为原值，实际 %q", string(s), s.String())
		}
	}
}

// TestStatusTerminal 覆盖全部状态的终态判定。空白状态与未知状态不是终态。
func TestStatusTerminal(t *testing.T) {
	cases := []struct {
		status Status
		want   bool
	}{
		{StatusQueued, false},
		{StatusRunning, false},
		{StatusPaused, false},
		{StatusFinalizing, false},
		{StatusDone, true},
		{StatusFailed, true},
		{StatusCancelled, true},
		{Status(""), false},
		{Status("unknown"), false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.Terminal(); got != tc.want {
				t.Fatalf("Terminal(%q) = %v，期望 %v", string(tc.status), got, tc.want)
			}
		})
	}
}

// TestStatusWireValues 锁定序列化取值：这些字符串直接写入数据库与审计日志。
func TestStatusWireValues(t *testing.T) {
	cases := map[Status]string{
		StatusQueued:     "queued",
		StatusRunning:    "running",
		StatusPaused:     "paused",
		StatusFinalizing: "finalizing",
		StatusDone:       "done",
		StatusFailed:     "failed",
		StatusCancelled:  "cancelled",
	}
	for status, want := range cases {
		if string(status) != want {
			t.Errorf("状态取值 = %q，期望 %q", string(status), want)
		}
	}
}

// TestEventTypeValid 覆盖全部合法事件类型与非法取值（含大小写与拼写错误）。
func TestEventTypeValid(t *testing.T) {
	valid := []EventType{
		EvCreated, EvAdmitted, EvStarted, EvPaused, EvResumed, EvFinalizing,
		EvCompleted, EvFailed, EvCancelled, EvDeleted, EvRecovered,
	}
	for _, typ := range valid {
		if !typ.valid() {
			t.Errorf("事件类型 %q 应合法", string(typ))
		}
		if typ.String() == string(typ) {
			t.Errorf("事件类型 %q 缺少中文字称", string(typ))
		}
	}
	invalid := []EventType{"", "Created", "admit", "finalize", "started ", "delete", "recover", "cancelled_"}
	for _, typ := range invalid {
		if typ.valid() {
			t.Errorf("事件类型 %q 应非法", string(typ))
		}
	}
}

// TestEventTypeWireValues 锁定事件类型的字面量。
func TestEventTypeWireValues(t *testing.T) {
	cases := map[EventType]string{
		EvCreated:    "created",
		EvAdmitted:   "admitted",
		EvStarted:    "started",
		EvPaused:     "paused",
		EvResumed:    "resumed",
		EvFinalizing: "finalizing",
		EvCompleted:  "completed",
		EvFailed:     "failed",
		EvCancelled:  "cancelled",
		EvDeleted:    "deleted",
		EvRecovered:  "recovered",
	}
	for typ, want := range cases {
		if string(typ) != want {
			t.Errorf("事件类型取值 = %q，期望 %q", string(typ), want)
		}
	}
}

// TestEventValidOnlyChecksType 验证 Event.Valid 只看事件类型：
// Reason 与 At 为空/零值、甚至填任意内容都不影响判定。
func TestEventValidOnlyChecksType(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want bool
	}{
		{name: "零值事件", ev: Event{}, want: false},
		{name: "只有合法类型", ev: Event{Type: EvCreated}, want: true},
		{name: "带原因与时间", ev: Event{Type: EvFailed, Reason: "超时", At: time.Unix(1700000000, 0)}, want: true},
		{name: "未知类型带原因", ev: Event{Type: "bogus", Reason: "x", At: time.Now()}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.Valid(); got != tc.want {
				t.Fatalf("Event{Type:%q}.Valid() = %v，期望 %v", string(tc.ev.Type), got, tc.want)
			}
		})
	}
}

// TestEffectValues 覆盖副作用字面量、中文字称与可区分性。
func TestEffectValues(t *testing.T) {
	if string(EffectStartRunners) != "start_runners" {
		t.Errorf("EffectStartRunners = %q", string(EffectStartRunners))
	}
	if string(EffectStopRunners) != "stop_runners" {
		t.Errorf("EffectStopRunners = %q", string(EffectStopRunners))
	}
	if EffectStartRunners == EffectStopRunners {
		t.Fatal("两个副作用不应相同")
	}
	for _, e := range []Effect{EffectStartRunners, EffectStopRunners} {
		if e.String() == string(e) {
			t.Errorf("副作用 %q 缺少中文字称", string(e))
		}
	}
	if Effect("bogus").String() != "bogus" {
		t.Errorf("未知副作用的中文字称应回退为原值，实际 %q", Effect("bogus").String())
	}
}

// TestReduceLegalTransitions 逐条覆盖迁移表里的每一条合法规则，
// 包括下一状态与副作用。这是本包的核心契约。
func TestReduceLegalTransitions(t *testing.T) {
	cases := []struct {
		name     string
		prev     Status
		ev       EventType
		wantNext Status
		wantEff  []Effect
	}{
		{
			name: "空白状态创建后进入排队",
			prev: statusUnset, ev: EvCreated,
			wantNext: StatusQueued,
		},
		{
			name: "排队准入后开始运行并拉起执行器",
			prev: StatusQueued, ev: EvAdmitted,
			wantNext: StatusRunning, wantEff: []Effect{EffectStartRunners},
		},
		{
			name: "排队中取消",
			prev: StatusQueued, ev: EvCancelled,
			wantNext: StatusCancelled,
		},
		{
			name: "排队中删除按取消处理",
			prev: StatusQueued, ev: EvDeleted,
			wantNext: StatusCancelled,
		},
		{
			name: "排队态收到恢复对账是幂等空操作",
			prev: StatusQueued, ev: EvRecovered,
			wantNext: StatusQueued,
		},
		{
			name: "运行中暂停并停止执行器",
			prev: StatusRunning, ev: EvPaused,
			wantNext: StatusPaused, wantEff: []Effect{EffectStopRunners},
		},
		{
			name: "运行中进入收尾并停止领取新动作",
			prev: StatusRunning, ev: EvFinalizing,
			wantNext: StatusFinalizing, wantEff: []Effect{EffectStopRunners},
		},
		{
			name: "运行中失败并停止执行器",
			prev: StatusRunning, ev: EvFailed,
			wantNext: StatusFailed, wantEff: []Effect{EffectStopRunners},
		},
		{
			name: "运行中取消并停止执行器",
			prev: StatusRunning, ev: EvCancelled,
			wantNext: StatusCancelled, wantEff: []Effect{EffectStopRunners},
		},
		{
			name: "运行中删除按取消处理并停止执行器",
			prev: StatusRunning, ev: EvDeleted,
			wantNext: StatusCancelled, wantEff: []Effect{EffectStopRunners},
		},
		{
			name: "运行中崩溃恢复：退回排队并停止执行器",
			prev: StatusRunning, ev: EvRecovered,
			wantNext: StatusQueued, wantEff: []Effect{EffectStopRunners},
		},
		{
			name: "暂停后恢复：回到排队等待重新准入",
			prev: StatusPaused, ev: EvResumed,
			wantNext: StatusQueued,
		},
		{
			name: "暂停中取消（执行器已停，无副作用）",
			prev: StatusPaused, ev: EvCancelled,
			wantNext: StatusCancelled,
		},
		{
			name: "暂停中删除按取消处理",
			prev: StatusPaused, ev: EvDeleted,
			wantNext: StatusCancelled,
		},
		{
			name: "收尾完成即成功",
			prev: StatusFinalizing, ev: EvCompleted,
			wantNext: StatusDone,
		},
		{
			name: "收尾阶段失败",
			prev: StatusFinalizing, ev: EvFailed,
			wantNext: StatusFailed,
		},
		{
			name: "收尾阶段取消",
			prev: StatusFinalizing, ev: EvCancelled,
			wantNext: StatusCancelled,
		},
		{
			name: "收尾阶段删除按取消处理",
			prev: StatusFinalizing, ev: EvDeleted,
			wantNext: StatusCancelled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, effects, err := Reduce(tc.prev, Event{Type: tc.ev, Reason: "测试", At: time.Unix(0, 0)})
			if err != nil {
				t.Fatalf("Reduce(%q, %q) 意外报错：%v", string(tc.prev), tc.ev, err)
			}
			if next != tc.wantNext {
				t.Fatalf("下一状态 = %q，期望 %q", next, tc.wantNext)
			}
			if !equalEffects(effects, tc.wantEff) {
				t.Fatalf("副作用 = %v，期望 %v", effects, tc.wantEff)
			}
		})
	}
}

// TestReduceIllegalTransitions 列出迁移表中不存在的组合（名称自解释），
// 断言它们全部被拒绝，且拒绝时状态不变、无副作用。
func TestReduceIllegalTransitions(t *testing.T) {
	cases := []struct {
		name string
		prev Status
		ev   EventType
	}{
		// 空白状态只接受 created。
		{name: "空白状态直接准入", prev: statusUnset, ev: EvAdmitted},
		{name: "空白状态直接取消", prev: statusUnset, ev: EvCancelled},
		{name: "空白状态直接恢复", prev: statusUnset, ev: EvRecovered},
		{name: "空白状态直接完成", prev: statusUnset, ev: EvCompleted},

		// 排队态不是运行态，只能准入/取消/恢复。
		{name: "排队态直接暂停", prev: StatusQueued, ev: EvPaused},
		{name: "排队态直接进入收尾", prev: StatusQueued, ev: EvFinalizing},
		{name: "排队态直接完成", prev: StatusQueued, ev: EvCompleted},
		{name: "排队态直接失败", prev: StatusQueued, ev: EvFailed},
		{name: "排队态直接启动", prev: StatusQueued, ev: EvStarted},
		{name: "排队态重复创建", prev: StatusQueued, ev: EvCreated},
		{name: "排队态收到恢复以外的未知事件", prev: StatusQueued, ev: EvResumed},

		// 运行态不允许跳过收尾直接完成。
		{name: "运行态直接完成", prev: StatusRunning, ev: EvCompleted},
		{name: "运行态重复准入", prev: StatusRunning, ev: EvAdmitted},
		{name: "运行态重复启动", prev: StatusRunning, ev: EvStarted},
		{name: "运行态重复创建", prev: StatusRunning, ev: EvCreated},

		// 暂停态不能直接运行或收尾。
		{name: "暂停态直接准入", prev: StatusPaused, ev: EvAdmitted},
		{name: "暂停态直接启动", prev: StatusPaused, ev: EvStarted},
		{name: "暂停态直接完成", prev: StatusPaused, ev: EvCompleted},
		{name: "暂停态直接失败", prev: StatusPaused, ev: EvFailed},
		{name: "暂停态进入收尾", prev: StatusPaused, ev: EvFinalizing},
		{name: "暂停态收到恢复", prev: StatusPaused, ev: EvRecovered},

		// 收尾态不再领取动作，也不能回退。
		{name: "收尾态重新准入", prev: StatusFinalizing, ev: EvAdmitted},
		{name: "收尾态暂停", prev: StatusFinalizing, ev: EvPaused},
		{name: "收尾态恢复", prev: StatusFinalizing, ev: EvResumed},
		{name: "收尾态收到恢复对账", prev: StatusFinalizing, ev: EvRecovered},
		{name: "收尾态重复进入收尾", prev: StatusFinalizing, ev: EvFinalizing},
		{name: "收尾态重复创建", prev: StatusFinalizing, ev: EvCreated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, effects, err := Reduce(tc.prev, Event{Type: tc.ev})
			if err == nil {
				t.Fatalf("Reduce(%q, %q) 应报错，实际得到状态 %q、副作用 %v", string(tc.prev), tc.ev, next, effects)
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("错误应为 ErrIllegalTransition，实际：%v", err)
			}
			if next != tc.prev {
				t.Errorf("报错时状态应保持不变：%q → %q", string(tc.prev), next)
			}
			if effects != nil {
				t.Errorf("报错时副作用应为 nil，实际 %v", effects)
			}
		})
	}
}

// TestReduceExhaustiveMatrix 穷举「8 个状态（含空白哨兵）× 11 个事件」
// 的全部 88 种组合，用独立于实现的迁移表作为判定依据。这能保证新增事件或
// 状态时，非法组合不会被悄悄放行。
func TestReduceExhaustiveMatrix(t *testing.T) {
	type outcome struct {
		next    Status
		effects []Effect
	}
	// 独立写出的合法迁移表（与实现刻意分开，避免"实现即规范"）。
	legal := map[[2]string]outcome{
		{"", "created"}: {StatusQueued, nil},

		{"queued", "admitted"}:  {StatusRunning, []Effect{EffectStartRunners}},
		{"queued", "cancelled"}: {StatusCancelled, nil},
		{"queued", "deleted"}:   {StatusCancelled, nil},
		{"queued", "recovered"}: {StatusQueued, nil},

		{"running", "paused"}:     {StatusPaused, []Effect{EffectStopRunners}},
		{"running", "finalizing"}: {StatusFinalizing, []Effect{EffectStopRunners}},
		{"running", "failed"}:     {StatusFailed, []Effect{EffectStopRunners}},
		{"running", "cancelled"}:  {StatusCancelled, []Effect{EffectStopRunners}},
		{"running", "deleted"}:    {StatusCancelled, []Effect{EffectStopRunners}},
		{"running", "recovered"}:  {StatusQueued, []Effect{EffectStopRunners}},

		{"paused", "resumed"}:   {StatusQueued, nil},
		{"paused", "cancelled"}: {StatusCancelled, nil},
		{"paused", "deleted"}:   {StatusCancelled, nil},

		{"finalizing", "completed"}: {StatusDone, nil},
		{"finalizing", "failed"}:    {StatusFailed, nil},
		{"finalizing", "cancelled"}: {StatusCancelled, nil},
		{"finalizing", "deleted"}:   {StatusCancelled, nil},
	}

	statuses := []Status{
		statusUnset, StatusQueued, StatusRunning, StatusPaused,
		StatusFinalizing, StatusDone, StatusFailed, StatusCancelled,
	}
	events := []EventType{
		EvCreated, EvAdmitted, EvStarted, EvPaused, EvResumed, EvFinalizing,
		EvCompleted, EvFailed, EvCancelled, EvDeleted, EvRecovered,
	}

	legalCount, illegalCount := 0, 0
	for _, prev := range statuses {
		for _, ev := range events {
			name := string(prev) + "+" + string(ev)
			if name == "+"+string(ev) {
				name = "（空）+" + string(ev)
			}
			t.Run(name, func(t *testing.T) {
				next, effects, err := Reduce(prev, Event{Type: ev})
				want, ok := legal[[2]string{string(prev), string(ev)}]
				if !ok {
					if err == nil {
						t.Fatalf("非法组合被放行：%q + %q → %q（副作用 %v）", string(prev), ev, next, effects)
					}
					return
				}
				if err != nil {
					t.Fatalf("合法组合被拒绝：%q + %q：%v", string(prev), ev, err)
				}
				if next != want.next {
					t.Fatalf("%q + %q 的下一状态 = %q，期望 %q", string(prev), ev, next, want.next)
				}
				if !equalEffects(effects, want.effects) {
					t.Fatalf("%q + %q 的副作用 = %v，期望 %v", string(prev), ev, effects, want.effects)
				}
			})
			if _, ok := legal[[2]string{string(prev), string(ev)}]; ok {
				legalCount++
			} else {
				illegalCount++
			}
		}
	}

	// 矩阵规模自检：8 × 11 = 88，其中合法 18 条、非法 70 条。
	if legalCount != len(legal) {
		t.Fatalf("迁移表条目数 = %d，矩阵命中 %d", len(legal), legalCount)
	}
	if legalCount+illegalCount != len(statuses)*len(events) {
		t.Fatalf("矩阵规模 = %d，期望 %d", legalCount+illegalCount, len(statuses)*len(events))
	}
	if illegalCount < 20 {
		t.Fatalf("非法组合只有 %d 组，少于验收要求的 20 组", illegalCount)
	}
}

// TestReduceTerminalRejectsAllEvents 验证终态对任何事件都报 ErrTerminal，
// 且不会被误判成非法迁移。
func TestReduceTerminalRejectsAllEvents(t *testing.T) {
	terminals := []Status{StatusDone, StatusFailed, StatusCancelled}
	events := []EventType{
		EvCreated, EvAdmitted, EvStarted, EvPaused, EvResumed, EvFinalizing,
		EvCompleted, EvFailed, EvCancelled, EvDeleted, EvRecovered,
	}
	for _, prev := range terminals {
		for _, ev := range events {
			t.Run(string(prev)+"+"+string(ev), func(t *testing.T) {
				next, effects, err := Reduce(prev, Event{Type: ev})
				if err == nil {
					t.Fatalf("终态 %q 收到 %q 应报错", string(prev), ev)
				}
				if !errors.Is(err, ErrTerminal) {
					t.Fatalf("错误应为 ErrTerminal，实际：%v", err)
				}
				if next != prev {
					t.Errorf("报错时状态应保持不变：%q → %q", string(prev), next)
				}
				if effects != nil {
					t.Errorf("报错时副作用应为 nil，实际 %v", effects)
				}
			})
		}
	}
}

// TestReduceInvalidInput 验证非法状态与非法事件的错误分类，以及校验顺序：
// 状态非法时优先报状态，事件非法的错误不会被状态错误吞掉。
func TestReduceInvalidInput(t *testing.T) {
	cases := []struct {
		name    string
		prev    Status
		ev      Event
		wantErr error
	}{
		{name: "未知状态", prev: Status("paused_"), ev: Event{Type: EvCreated}, wantErr: ErrInvalidStatus},
		{name: "随机状态字符串", prev: Status("RUNNING"), ev: Event{Type: EvCancelled}, wantErr: ErrInvalidStatus},
		{name: "合法状态加未知事件", prev: StatusRunning, ev: Event{Type: "paused_"}, wantErr: ErrInvalidEvent},
		{name: "零值事件", prev: StatusRunning, ev: Event{}, wantErr: ErrInvalidEvent},
		{name: "空白状态加未知事件", prev: statusUnset, ev: Event{Type: "created_"}, wantErr: ErrInvalidEvent},
		{name: "状态与事件都非法时先报状态", prev: Status("bogus"), ev: Event{Type: "bogus"}, wantErr: ErrInvalidStatus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, effects, err := Reduce(tc.prev, tc.ev)
			if err == nil {
				t.Fatalf("Reduce(%q, %q) 应报错", string(tc.prev), tc.ev.Type)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望 errors.Is(%v)", err, tc.wantErr)
			}
			if next != tc.prev {
				t.Errorf("报错时状态应保持不变：%q → %q", string(tc.prev), next)
			}
			if effects != nil {
				t.Errorf("报错时副作用应为 nil，实际 %v", effects)
			}
		})
	}
}

// TestReducePure 验证纯函数性质：同一输入多次调用结果完全一致；
// 返回值中的副作用切片是新分配的，调用方修改它不会污染后续调用；
// 事件按值传入，调用前后内容不变。
func TestReducePure(t *testing.T) {
	inputs := []struct {
		prev Status
		ev   Event
	}{
		{statusUnset, Event{Type: EvCreated, Reason: "创建", At: time.Unix(1, 0)}},
		{StatusQueued, Event{Type: EvAdmitted, Reason: "准入", At: time.Unix(2, 0)}},
		{StatusRunning, Event{Type: EvFinalizing, Reason: "收尾", At: time.Unix(3, 0)}},
		{StatusRunning, Event{Type: EvRecovered, Reason: "崩溃恢复", At: time.Unix(4, 0)}},
		{StatusPaused, Event{Type: EvResumed, Reason: "恢复", At: time.Unix(5, 0)}},
		{StatusFinalizing, Event{Type: EvCompleted, Reason: "完成", At: time.Unix(6, 0)}},
	}
	for _, in := range inputs {
		t.Run(string(in.prev)+"+"+string(in.ev.Type), func(t *testing.T) {
			evBefore := in.ev

			next1, eff1, err1 := Reduce(in.prev, in.ev)
			if err1 != nil {
				t.Fatalf("首次调用意外报错：%v", err1)
			}
			// 篡改第一次返回的切片，验证不影响后续调用的返回值。
			for i := range eff1 {
				eff1[i] = Effect("已被调用方篡改")
			}

			next2, eff2, err2 := Reduce(in.prev, in.ev)
			if err2 != nil {
				t.Fatalf("第二次调用意外报错：%v", err2)
			}
			if next1 != next2 {
				t.Fatalf("两次调用状态不一致：%q vs %q", next1, next2)
			}
			if len(eff1) == len(eff2) && len(eff2) > 0 && eff2[0] == Effect("已被调用方篡改") {
				t.Fatal("副作用切片被跨调用复用，违反纯函数约定")
			}
			if in.ev != evBefore {
				t.Fatalf("入参事件被修改：%+v vs %+v", in.ev, evBefore)
			}
		})
	}
}

// TestReduceReasonDoesNotAffectResult 验证 Reason / At 只是审计信息，
// 不参与状态判定：只改这两个字段不会改变结果。
func TestReduceReasonDoesNotAffectResult(t *testing.T) {
	prev := StatusRunning
	nextA, effA, errA := Reduce(prev, Event{Type: EvFailed, Reason: "第一次原因", At: time.Unix(100, 0)})
	nextB, effB, errB := Reduce(prev, Event{Type: EvFailed, Reason: "", At: time.Time{}})
	if errA != nil || errB != nil {
		t.Fatalf("意外报错：%v / %v", errA, errB)
	}
	if nextA != nextB || !equalEffects(effA, effB) {
		t.Fatalf("Reason/At 影响了结果：%q/%v vs %q/%v", nextA, effA, nextB, effB)
	}
}

// TestReduceRecoveredNuance 单独锁定 recovered 事件的差异化行为：
// 只有 running 需要停执行器，queued 是幂等空操作，其余状态一律拒绝。
func TestReduceRecoveredNuance(t *testing.T) {
	next, effects, err := Reduce(StatusRunning, Event{Type: EvRecovered})
	if err != nil {
		t.Fatalf("running + recovered 应合法：%v", err)
	}
	if next != StatusQueued {
		t.Errorf("running + recovered 下一状态 = %q，期望 queued", next)
	}
	if !equalEffects(effects, []Effect{EffectStopRunners}) {
		t.Errorf("running + recovered 副作用 = %v，期望 [stop_runners]", effects)
	}

	next, effects, err = Reduce(StatusQueued, Event{Type: EvRecovered})
	if err != nil {
		t.Fatalf("queued + recovered 应合法：%v", err)
	}
	if next != StatusQueued {
		t.Errorf("queued + recovered 下一状态 = %q，期望 queued", next)
	}
	if len(effects) != 0 {
		t.Errorf("queued + recovered 不应产生副作用，实际 %v", effects)
	}

	for _, prev := range []Status{StatusPaused, StatusFinalizing} {
		if _, _, err := Reduce(prev, Event{Type: EvRecovered}); err == nil {
			t.Errorf("%q + recovered 应被拒绝", string(prev))
		}
	}
}

// TestReduceCreatedOnlyFromUnset 单独锁定 created 的入口条件：
// 只从空白状态接受，从任何已有状态重复创建都会被拒绝。
func TestReduceCreatedOnlyFromUnset(t *testing.T) {
	if next, effects, err := Reduce(statusUnset, Event{Type: EvCreated}); err != nil || next != StatusQueued || len(effects) != 0 {
		t.Fatalf("空白 + created 应得到 queued 且无副作用，实际 %q/%v/%v", next, effects, err)
	}
	for _, prev := range []Status{
		StatusQueued, StatusRunning, StatusPaused, StatusFinalizing,
		StatusDone, StatusFailed, StatusCancelled,
	} {
		if _, _, err := Reduce(prev, Event{Type: EvCreated}); err == nil {
			t.Errorf("%q + created 应被拒绝", string(prev))
		}
	}
}
