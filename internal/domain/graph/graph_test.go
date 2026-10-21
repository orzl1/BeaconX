package graph

import (
	"errors"
	"strings"
	"testing"
)

// TestNodeKindValid 覆盖全部合法节点类型与若干非法取值。
func TestNodeKindValid(t *testing.T) {
	valid := []NodeKind{
		NodeGoal, NodeHypothesis, NodeAction, NodeFact, NodeFinding, NodeHint, NodeDigest,
	}
	for _, k := range valid {
		if !k.Valid() {
			t.Errorf("节点类型 %q 应合法", string(k))
		}
		if k.String() == string(k) {
			t.Errorf("节点类型 %q 缺少中文字称", string(k))
		}
	}
	invalid := []NodeKind{"", "intent", "Goal", "fact ", "begin", "unknown"}
	for _, k := range invalid {
		if k.Valid() {
			t.Errorf("节点类型 %q 应非法", string(k))
		}
		if k.Valid() {
			continue
		}
		if k.String() != string(k) {
			t.Errorf("未知节点类型 %q 的中文字称应回退为原值，实际 %q", string(k), k.String())
		}
	}
}

// TestNodeStateValid 覆盖全部合法状态与非法取值。
func TestNodeStateValid(t *testing.T) {
	valid := []NodeState{StateOpen, StateRunning, StateDone, StateExhausted, StateBlocked}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("节点状态 %q 应合法", string(s))
		}
		if s.String() == string(s) {
			t.Errorf("节点状态 %q 缺少中文字称", string(s))
		}
	}
	invalid := []NodeState{"", "paused", "Open", "deleted", "running ", "superseded"}
	for _, s := range invalid {
		if s.Valid() {
			t.Errorf("节点状态 %q 应非法", string(s))
		}
	}
}

// TestEdgeRelValid 覆盖全部合法边语义与非法取值。
func TestEdgeRelValid(t *testing.T) {
	valid := []EdgeRel{RelSpawns, RelDerivedFrom, RelYields, RelProves, RelCovers}
	for _, r := range valid {
		if !r.Valid() {
			t.Errorf("边语义 %q 应合法", string(r))
		}
		if r.String() == string(r) {
			t.Errorf("边语义 %q 缺少中文字称", string(r))
		}
	}
	invalid := []EdgeRel{"", "causes", "Spawns", "derived-from", "cover"}
	for _, r := range invalid {
		if r.Valid() {
			t.Errorf("边语义 %q 应非法", string(r))
		}
	}
}

// TestAnchorRoleValid 覆盖全部合法锚点角色与非法取值。
func TestAnchorRoleValid(t *testing.T) {
	valid := []AnchorRole{RoleTarget, RoleTouched, RoleTested, RoleEvidence}
	for _, r := range valid {
		if !r.Valid() {
			t.Errorf("锚点角色 %q 应合法", string(r))
		}
		if r.String() == string(r) {
			t.Errorf("锚点角色 %q 缺少中文字称", string(r))
		}
	}
	invalid := []AnchorRole{"", "test", "Target", "tested ", "evidenced"}
	for _, r := range invalid {
		if r.Valid() {
			t.Errorf("锚点角色 %q 应非法", string(r))
		}
	}
}

// TestWireValues 锁定所有序列化取值：这些字符串直接写入数据库，
// 改动会造成存量数据不兼容。
func TestWireValues(t *testing.T) {
	nodeKinds := map[NodeKind]string{
		NodeGoal:       "goal",
		NodeHypothesis: "hypothesis",
		NodeAction:     "action",
		NodeFact:       "fact",
		NodeFinding:    "finding",
		NodeHint:       "hint",
		NodeDigest:     "digest",
	}
	for kind, want := range nodeKinds {
		if string(kind) != want {
			t.Errorf("节点类型取值 = %q，期望 %q", string(kind), want)
		}
	}

	states := map[NodeState]string{
		StateOpen:      "open",
		StateRunning:   "running",
		StateDone:      "done",
		StateExhausted: "exhausted",
		StateBlocked:   "blocked",
	}
	for state, want := range states {
		if string(state) != want {
			t.Errorf("节点状态取值 = %q，期望 %q", string(state), want)
		}
	}

	rels := map[EdgeRel]string{
		RelSpawns:      "spawns",
		RelDerivedFrom: "derived_from",
		RelYields:      "yields",
		RelProves:      "proves",
		RelCovers:      "covers",
	}
	for rel, want := range rels {
		if string(rel) != want {
			t.Errorf("边语义取值 = %q，期望 %q", string(rel), want)
		}
	}

	roles := map[AnchorRole]string{
		RoleTarget:   "target",
		RoleTouched:  "touched",
		RoleTested:   "tested",
		RoleEvidence: "evidence",
	}
	for role, want := range roles {
		if string(role) != want {
			t.Errorf("锚点角色取值 = %q，期望 %q", string(role), want)
		}
	}
}

// TestValidateNode 是 kind × state 的完整矩阵：每个组合都给出明确期望，
// 保证规则改动必须同时改测试，不会被无意放宽。
func TestValidateNode(t *testing.T) {
	// 规则：goal 不允许 running/exhausted；知识类节点（fact/finding/hint/digest）
	// 不允许 running；hypothesis 与 action 允许全部五个状态。
	cases := []struct {
		kind  NodeKind
		state NodeState
		ok    bool
	}{
		{NodeGoal, StateOpen, true},
		{NodeGoal, StateRunning, false},
		{NodeGoal, StateDone, true},
		{NodeGoal, StateExhausted, false},
		{NodeGoal, StateBlocked, true},

		{NodeHypothesis, StateOpen, true},
		{NodeHypothesis, StateRunning, true},
		{NodeHypothesis, StateDone, true},
		{NodeHypothesis, StateExhausted, true},
		{NodeHypothesis, StateBlocked, true},

		{NodeAction, StateOpen, true},
		{NodeAction, StateRunning, true},
		{NodeAction, StateDone, true},
		{NodeAction, StateExhausted, true},
		{NodeAction, StateBlocked, true},

		{NodeFact, StateOpen, true},
		{NodeFact, StateRunning, false},
		{NodeFact, StateDone, true},
		{NodeFact, StateExhausted, true},
		{NodeFact, StateBlocked, true},

		{NodeFinding, StateOpen, true},
		{NodeFinding, StateRunning, false},
		{NodeFinding, StateDone, true},
		{NodeFinding, StateExhausted, true},
		{NodeFinding, StateBlocked, true},

		{NodeHint, StateOpen, true},
		{NodeHint, StateRunning, false},
		{NodeHint, StateDone, true},
		{NodeHint, StateExhausted, true},
		{NodeHint, StateBlocked, true},

		{NodeDigest, StateOpen, true},
		{NodeDigest, StateRunning, false},
		{NodeDigest, StateDone, true},
		{NodeDigest, StateExhausted, true},
		{NodeDigest, StateBlocked, true},
	}

	for _, tc := range cases {
		name := string(tc.kind) + "/" + string(tc.state)
		t.Run(name, func(t *testing.T) {
			err := ValidateNode(tc.kind, tc.state)
			if tc.ok && err != nil {
				t.Fatalf("ValidateNode(%q, %q) 应通过，实际报错：%v", tc.kind, tc.state, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("ValidateNode(%q, %q) 应报错，实际通过", tc.kind, tc.state)
				}
				// 错误信息必须点明是哪个类型/状态，方便定位调用方。
				if !strings.Contains(err.Error(), string(tc.kind)) && !strings.Contains(err.Error(), tc.kind.String()) {
					t.Errorf("错误信息 %q 未包含节点类型", err.Error())
				}
				if !strings.Contains(err.Error(), tc.state.String()) {
					t.Errorf("错误信息 %q 未包含状态的中文字称", err.Error())
				}
			}
		})
	}
}

// TestValidateNodeInvalidValues 覆盖 kind 或 state 本身未知的情况：
// 必须优先报“未知取值”，而不是落到按类型的状态规则里。
func TestValidateNodeInvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		kind    NodeKind
		state   NodeState
		wantErr string
	}{
		{name: "未知类型", kind: NodeKind("intent"), state: StateOpen, wantErr: "未知的节点类型"},
		{name: "空类型", kind: NodeKind(""), state: StateOpen, wantErr: "未知的节点类型"},
		{name: "未知状态", kind: NodeGoal, state: NodeState("paused"), wantErr: "未知的节点状态"},
		{name: "空状态", kind: NodeAction, state: NodeState(""), wantErr: "未知的节点状态"},
		{name: "类型与状态都非法时先报类型", kind: NodeKind("intent"), state: NodeState("paused"), wantErr: "未知的节点类型"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNode(tc.kind, tc.state)
			if err == nil {
				t.Fatalf("ValidateNode(%q, %q) 应报错", tc.kind, tc.state)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 = %q，期望包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidateEdge 覆盖边语义校验。
func TestValidateEdge(t *testing.T) {
	for _, rel := range []EdgeRel{RelSpawns, RelDerivedFrom, RelYields, RelProves, RelCovers} {
		if err := ValidateEdge(rel); err != nil {
			t.Errorf("ValidateEdge(%q) 意外报错：%v", rel, err)
		}
	}
	for _, rel := range []EdgeRel{"", "causes", "Spawns", "covers "} {
		err := ValidateEdge(rel)
		if err == nil {
			t.Errorf("ValidateEdge(%q) 应报错", rel)
			continue
		}
		if !strings.Contains(err.Error(), "未知的边语义") {
			t.Errorf("错误信息 = %q", err.Error())
		}
	}
}

// TestValidateAnchor 覆盖锚点角色校验。
func TestValidateAnchor(t *testing.T) {
	for _, role := range []AnchorRole{RoleTarget, RoleTouched, RoleTested, RoleEvidence} {
		if err := ValidateAnchor(role); err != nil {
			t.Errorf("ValidateAnchor(%q) 意外报错：%v", role, err)
		}
	}
	for _, role := range []AnchorRole{"", "test", "Target", "tested "} {
		err := ValidateAnchor(role)
		if err == nil {
			t.Errorf("ValidateAnchor(%q) 应报错", role)
			continue
		}
		if !strings.Contains(err.Error(), "未知的锚点角色") {
			t.Errorf("错误信息 = %q", err.Error())
		}
	}
}

// TestErrNotFound 验证预留的“未找到”错误可用 errors.Is 判断，
// 存储层据此统一处理未命中语义。
func TestErrNotFound(t *testing.T) {
	if ErrNotFound == nil {
		t.Fatal("ErrNotFound 不应为 nil")
	}
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatal("ErrNotFound 应能被 errors.Is 识别")
	}
	if !strings.Contains(ErrNotFound.Error(), "未找到") {
		t.Errorf("ErrNotFound 信息应为中文，实际 %q", ErrNotFound.Error())
	}
}
