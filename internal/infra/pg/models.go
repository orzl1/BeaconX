package pg

import (
	"errors"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/graph"
)

// 常见错误：调用方可以用 errors.Is 判断。
var (
	// ErrInvalidTenant 表示租户标识非法（必须为正数）。
	ErrInvalidTenant = errors.New("租户标识必须为正数")
	// ErrInvalidInput 表示输入参数不满足写入要求。
	ErrInvalidInput = errors.New("输入参数不合法")
	// ErrIdentityConflict 表示同一批身份键指向了不同的资产，需要人工合并。
	ErrIdentityConflict = errors.New("身份键冲突：同一批身份指向了不同资产")
)

// Company 是资产归属的公司。
type Company struct {
	TenantID  int64          `json:"tenant_id"`
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	Slug      string         `json:"slug"`
	Attrs     map[string]any `json:"attrs"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// Asset 是资产图中的一行资产。
type Asset struct {
	TenantID   int64            `json:"tenant_id"`
	ID         int64            `json:"id"`
	Type       asset.Kind       `json:"type"`
	Label      string           `json:"label"`
	ParentID   *int64           `json:"parent_id,omitempty"`
	Attrs      map[string]any   `json:"attrs"`
	Identities []asset.Identity `json:"identities,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// AssetInput 是写入资产的输入。身份键必须已经过 domain/asset 归一化。
type AssetInput struct {
	Type       asset.Kind
	Label      string
	ParentID   *int64
	Attrs      map[string]any
	Identities []asset.Identity
}

// AssetFilter 是资产列表过滤条件。
type AssetFilter struct {
	Type     *asset.Kind
	ParentID *int64
	AfterID  int64
	Limit    int
}

// Mission 是一次授权测试任务。
type Mission struct {
	TenantID    int64      `json:"tenant_id"`
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Goal        string     `json:"goal"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	Queued      bool       `json:"queued"`
	QueuedAt    *time.Time `json:"queued_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	DeadlineAt  *time.Time `json:"deadline_at,omitempty"`
	Reason      string     `json:"reason"`
	MaxTokens   int64      `json:"max_tokens"`
	SpentTokens int64      `json:"spent_tokens"`
	MaxSteps    int        `json:"max_steps"`
	StepsUsed   int        `json:"steps_used"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// MissionEvent 是 mission_events 中的一条只追加事件。
type MissionEvent struct {
	TenantID  int64          `json:"tenant_id"`
	ID        int64          `json:"id"`
	MissionID int64          `json:"mission_id"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	Reason    string         `json:"reason"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

// MissionInput 是创建任务的输入。
type MissionInput struct {
	Name        string
	Goal        string
	Description string
}

// MissionStatus 是任务状态枚举。
type MissionStatus string

// 任务状态取值，与数据库 CHECK 约束保持一致。
const (
	MissionDraft     MissionStatus = "draft"
	MissionActive    MissionStatus = "active"
	MissionPaused    MissionStatus = "paused"
	MissionDone      MissionStatus = "done"
	MissionFailed    MissionStatus = "failed"
	MissionCancelled MissionStatus = "cancelled"
)

// ScopeEntry 是一条任务范围条目。Value 必须是归一化后的形式。
type ScopeEntry struct {
	Kind           string
	Value          string
	AllowedActions []string
	Note           string
}

// Node 是作战图节点。
type Node struct {
	TenantID  int64           `json:"tenant_id"`
	ID        int64           `json:"id"`
	MissionID int64           `json:"mission_id"`
	Kind      graph.NodeKind  `json:"kind"`
	State     graph.NodeState `json:"state"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Payload   map[string]any  `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// NodeInput 是写入节点的输入。
type NodeInput struct {
	Kind    graph.NodeKind
	State   graph.NodeState
	Title   string
	Body    string
	Payload map[string]any
}

// Edge 是作战图的一条边。
type Edge struct {
	TenantID  int64         `json:"tenant_id"`
	ID        int64         `json:"id"`
	MissionID int64         `json:"mission_id"`
	SrcID     int64         `json:"src_id"`
	DstID     int64         `json:"dst_id"`
	Rel       graph.EdgeRel `json:"rel"`
	CreatedAt time.Time     `json:"created_at"`
}

// Anchor 把作战图节点锚定到资产。
type Anchor struct {
	TenantID  int64            `json:"tenant_id"`
	NodeID    int64            `json:"node_id"`
	AssetID   int64            `json:"asset_id"`
	Role      graph.AnchorRole `json:"role"`
	CreatedAt time.Time        `json:"created_at"`
}

// Coverage 是任务资产覆盖度。
type Coverage struct {
	Total   int            `json:"total"`
	Tested  int            `json:"tested"`
	Percent float64        `json:"percent"`
	ByType  []TypeCoverage `json:"by_type"`
}

// TypeCoverage 是按资产类型拆分的覆盖度。
type TypeCoverage struct {
	Type    asset.Kind `json:"type"`
	Total   int        `json:"total"`
	Tested  int        `json:"tested"`
	Percent float64    `json:"percent"`
}

// percent 计算百分比；分母为 0 时返回 0。
func percent(tested, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(tested) * 100 / float64(total)
}
