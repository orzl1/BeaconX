// Package graph 定义**作战图**（Operation Graph）的领域模型：节点类型、节点
// 状态、边语义与锚点角色，以及它们之间的合法性校验。
//
// 作战图是单个任务的推进链——节点是知识，边是因果。它和资产图（asset 包）
// 是两套独立的模型，只通过锚点（Anchor）相连：锚点回答“这条知识关于哪个
// 资产”，而资产本身存放在跨任务共享的资产图里。
//
// 关键约定：
//
//   - 节点类型（NodeKind）决定“这条记录是什么”：目标、假设、动作、事实、
//     发现、提示、折叠摘要；
//   - 节点状态（NodeState）是**跨类型共享**的五个状态，不是每个类型各有一套；
//     具体某个类型允许哪些状态由 ValidateNode 收敛；
//   - 边（EdgeRel）的方向统一为“从上游指向下游”：spawns/derived_from 表示
//     派生关系，yields 表示动作产出事实，proves 表示证据证明目标，covers 是
//     折叠视图的派生态（不是因果）；
//   - 锚点角色（AnchorRole）区分“授权目标”“只是碰过”“真的测过”“作为证据”：
//     target 决定覆盖度分母，tested 决定分子，touched 只是接触记录，
//     evidence 用于漏洞证据链；详见 asset 包与存储层。
//
// 本包是纯领域逻辑：只使用标准库，不依赖数据库与其它内部包。
package graph

import (
	"errors"
	"fmt"
)

// NodeKind 是作战图的节点类型。取值即存储层字面量，不要随意改动。
type NodeKind string

const (
	// NodeGoal 是任务目标：可验证的验收条件，例如“拿到管理后台权限”。
	NodeGoal NodeKind = "goal"
	// NodeHypothesis 是假设：一个待验证的判断，由规划器产生（取代旧式“意图”）。
	NodeHypothesis NodeKind = "hypothesis"
	// NodeAction 是动作：最小工作单元，执行一次工具调用并写回结果。
	NodeAction NodeKind = "action"
	// NodeFact 是事实：已被观察证实的结论，必须可追溯到动作与证据。
	NodeFact NodeKind = "fact"
	// NodeFinding 是发现：具备安全影响的漏洞记录。
	NodeFinding NodeKind = "finding"
	// NodeHint 是提示：人（或控制台）注入给规划器的战略信息。
	NodeHint NodeKind = "hint"
	// NodeDigest 是折叠摘要：把冷区节点折叠成的可逆摘要块（不是原始知识）。
	NodeDigest NodeKind = "digest"
)

// Valid 报告 k 是否是已知的节点类型。
func (k NodeKind) Valid() bool {
	switch k {
	case NodeGoal, NodeHypothesis, NodeAction, NodeFact, NodeFinding, NodeHint, NodeDigest:
		return true
	}
	return false
}

// String 返回节点类型的中文字称，用于错误信息。
func (k NodeKind) String() string {
	switch k {
	case NodeGoal:
		return "目标"
	case NodeHypothesis:
		return "假设"
	case NodeAction:
		return "动作"
	case NodeFact:
		return "事实"
	case NodeFinding:
		return "发现"
	case NodeHint:
		return "提示"
	case NodeDigest:
		return "折叠摘要"
	}
	return string(k)
}

// NodeState 是节点状态。五个状态跨类型共享，由 ValidateNode 按类型收敛
// 允许的取值。
type NodeState string

const (
	// StateOpen 表示待开始 / 待领取。
	StateOpen NodeState = "open"
	// StateRunning 表示正在执行（只有动作这类会被领取的节点才允许）。
	StateRunning NodeState = "running"
	// StateDone 表示正常完成（目标语境下即“已达成”）。
	StateDone NodeState = "done"
	// StateExhausted 表示已穷尽：方向试尽但没有进展，不再重试。
	StateExhausted NodeState = "exhausted"
	// StateBlocked 表示受阻：被外部条件挡住（权限、依赖、策略拒绝），可人工解除。
	StateBlocked NodeState = "blocked"
)

// Valid 报告 s 是否是已知的节点状态。
func (s NodeState) Valid() bool {
	switch s {
	case StateOpen, StateRunning, StateDone, StateExhausted, StateBlocked:
		return true
	}
	return false
}

// String 返回节点状态的中文字称，用于错误信息。
func (s NodeState) String() string {
	switch s {
	case StateOpen:
		return "待开始"
	case StateRunning:
		return "执行中"
	case StateDone:
		return "已完成"
	case StateExhausted:
		return "已穷尽"
	case StateBlocked:
		return "受阻"
	}
	return string(s)
}

// EdgeRel 是作战图的边语义。方向统一为“上游 → 下游”。
type EdgeRel string

const (
	// RelSpawns 表示派生：目标由任务根派生，或动作由目标/假设派生。
	RelSpawns EdgeRel = "spawns"
	// RelDerivedFrom 表示“由……推导而来”：假设/动作基于已确认的事实或发现。
	RelDerivedFrom EdgeRel = "derived_from"
	// RelYields 表示产出：动作产出事实或发现。
	RelYields EdgeRel = "yields"
	// RelProves 表示证明：证据节点证明某个目标达成。
	RelProves EdgeRel = "proves"
	// RelCovers 表示折叠覆盖：摘要节点覆盖了哪些成员。它是派生态，
	// 不是因果关系，遍历因果链时必须排除。
	RelCovers EdgeRel = "covers"
)

// Valid 报告 r 是否是已知的边语义。
func (r EdgeRel) Valid() bool {
	switch r {
	case RelSpawns, RelDerivedFrom, RelYields, RelProves, RelCovers:
		return true
	}
	return false
}

// String 返回边语义的中文字称，用于错误信息。
func (r EdgeRel) String() string {
	switch r {
	case RelSpawns:
		return "派生"
	case RelDerivedFrom:
		return "推导自"
	case RelYields:
		return "产出"
	case RelProves:
		return "证明"
	case RelCovers:
		return "折叠覆盖"
	}
	return string(r)
}

// AnchorRole 是节点与资产之间锚点的角色，决定这条锚点能否计入覆盖度。
type AnchorRole string

const (
	// RoleTarget 表示资产是这次测试的授权目标（进入覆盖度分母）。
	RoleTarget AnchorRole = "target"
	// RoleTouched 表示只是接触过（例如探测到一个端口），不计入已测。
	RoleTouched AnchorRole = "touched"
	// RoleTested 表示确实做过测试并留下了事实，计入覆盖度分子。
	RoleTested AnchorRole = "tested"
	// RoleEvidence 表示该资产作为漏洞证据被引用，不计入“已测覆盖度”。
	RoleEvidence AnchorRole = "evidence"
)

// Valid 报告 r 是否是已知的锚点角色。
func (r AnchorRole) Valid() bool {
	switch r {
	case RoleTarget, RoleTouched, RoleTested, RoleEvidence:
		return true
	}
	return false
}

// String 返回锚点角色的中文字称，用于错误信息。
func (r AnchorRole) String() string {
	switch r {
	case RoleTarget:
		return "授权目标"
	case RoleTouched:
		return "接触过"
	case RoleTested:
		return "已测试"
	case RoleEvidence:
		return "证据"
	}
	return string(r)
}

// ValidateNode 校验一个节点类型与状态的组合是否合法。
//
// 先校验取值本身是否已知，再按类型收敛状态：
//
//   - 目标（goal）不允许 running 与 exhausted：目标没有“执行中”，也不会
//     “穷尽”——达成是 done，放弃是 blocked（或由上层改为其它终态）；
//   - 事实 / 发现 / 提示 / 折叠摘要不允许 running：它们是被写入的知识，
//     不是会被领取执行的工作单元；写入即终态；
//   - 假设（hypothesis）与动作（action）允许全部五个状态。
//
// 这条规则刻意保持宽松：除上述两条外不再额外限制，避免把业务上的状态机
// 细节硬编码到领域校验里。
func ValidateNode(kind NodeKind, state NodeState) error {
	if !kind.Valid() {
		return fmt.Errorf("未知的节点类型：%q", string(kind))
	}
	if !state.Valid() {
		return fmt.Errorf("未知的节点状态：%q", string(state))
	}

	switch kind {
	case NodeGoal:
		if state == StateRunning || state == StateExhausted {
			return fmt.Errorf("%s不允许状态「%s」（允许：待开始 / 已完成 / 受阻）", kind, state)
		}
	case NodeFact, NodeFinding, NodeHint, NodeDigest:
		if state == StateRunning {
			return fmt.Errorf("%s不允许状态「%s」（知识类节点写入即终态，不会被领取执行）", kind, state)
		}
	}
	return nil
}

// ValidateEdge 校验边语义是否合法。
//
// 这里只校验 rel 取值；两端节点是否存在、是否属于同一任务、是否构成环，
// 属于存储层与图算法层的职责（本包不持有图结构）。
func ValidateEdge(rel EdgeRel) error {
	if !rel.Valid() {
		return fmt.Errorf("未知的边语义：%q", string(rel))
	}
	return nil
}

// ValidateAnchor 校验锚点角色是否合法。
//
// 这里只校验角色取值；资产 id 是否可访问、节点是否存在由存储层负责。
func ValidateAnchor(role AnchorRole) error {
	if !role.Valid() {
		return fmt.Errorf("未知的锚点角色：%q", string(role))
	}
	return nil
}

// ErrNotFound 预留给存储层：按身份查找资产或节点时未命中。
//
// 领域校验不产生该错误，但放在这里可以避免各存储层各自定义一套
// “未找到”语义导致调用方无法用 errors.Is 统一判断。
var ErrNotFound = errors.New("未找到")
