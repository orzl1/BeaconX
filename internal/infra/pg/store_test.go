package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/migrations"
)

// 集成测试依赖真实 PostgreSQL：设置 BEACONX_TEST_PG_DSN 后才会执行，
// 未设置时整组测试跳过，保证 `go test ./...` 在无数据库环境下仍可通过。

var (
	testOnce sync.Once
	testDB   *DB
	testErr  error
)

func testStore(t *testing.T) (*Store, int64) {
	t.Helper()
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过数据库集成测试")
	}

	testOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		testDB, testErr = Open(ctx, Options{
			DSN:            dsn,
			MaxConns:       6,
			MinConns:       1,
			ConnectTimeout: 10 * time.Second,
		})
		if testErr != nil {
			return
		}
		_, testErr = migrate.Apply(ctx, testDB.Pool(), migrations.FS)
	})
	if testErr != nil {
		t.Fatalf("准备测试数据库失败: %v", testErr)
	}

	// 每个测试用独立租户，天然隔离；结束时删除租户，级联清理全部数据。
	tenantID := time.Now().UnixNano()
	ctx := context.Background()
	if _, err := testDB.Pool().Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantID, fmt.Sprintf("test-%d", tenantID), "集成测试租户"); err != nil {
		t.Fatalf("创建测试租户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := testDB.Pool().Exec(cleanupCtx, `DELETE FROM tenants WHERE id = $1`, tenantID); err != nil {
			t.Logf("清理测试租户失败（可忽略）: %v", err)
		}
	})
	return NewStore(testDB), tenantID
}

func mustIdentity(t *testing.T, kind asset.Kind, raw string) asset.Identity {
	t.Helper()
	idn, err := asset.Normalize(kind, raw)
	if err != nil {
		t.Fatalf("归一化身份 %s=%s 失败: %v", kind, raw, err)
	}
	return idn
}

func mustAsset(t *testing.T, s *Store, tenantID int64, kind asset.Kind, label string, identities ...asset.Identity) Asset {
	t.Helper()
	a, err := s.UpsertAsset(context.Background(), tenantID, AssetInput{
		Type:       kind,
		Label:      label,
		Identities: identities,
	})
	if err != nil {
		t.Fatalf("写入资产 %s=%s 失败: %v", kind, label, err)
	}
	return a
}

func TestAssetUpsertIsIdempotent(t *testing.T) {
	s, tenantID := testStore(t)

	idn := mustIdentity(t, asset.KindSubdomain, "API.Example.COM.")
	a1 := mustAsset(t, s, tenantID, asset.KindSubdomain, "api.example.com", idn)
	a2 := mustAsset(t, s, tenantID, asset.KindSubdomain, "api.example.com", idn)

	if a1.ID != a2.ID {
		t.Fatalf("同一身份键应复用资产：%d != %d", a1.ID, a2.ID)
	}
	if len(a2.Identities) != 1 {
		t.Fatalf("身份键数量应为 1，实际 %d", len(a2.Identities))
	}
	if a2.Identities[0].Key != "api.example.com" {
		t.Fatalf("身份键未归一化：%q", a2.Identities[0].Key)
	}
}

func TestAssetUpsertMergesMultipleIdentities(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()

	domainID := mustIdentity(t, asset.KindSubdomain, "app.example.com")
	ipID := mustIdentity(t, asset.KindIP, "192.0.2.7")
	first := mustAsset(t, s, tenantID, asset.KindSubdomain, "app.example.com", domainID)
	merged := mustAsset(t, s, tenantID, asset.KindSubdomain, "app.example.com", domainID, ipID)

	if merged.ID != first.ID {
		t.Fatalf("补充身份键不应创建新资产：%d != %d", merged.ID, first.ID)
	}
	if len(merged.Identities) != 2 {
		t.Fatalf("应有 2 个身份键，实际 %d", len(merged.Identities))
	}

	// 用第二个身份键也能解析到同一资产。
	got, err := s.ResolveAssetByIdentity(ctx, tenantID, asset.KindIP, "192.0.2.7")
	if err != nil {
		t.Fatalf("按身份解析资产失败: %v", err)
	}
	if got != first.ID {
		t.Fatalf("按身份解析得到 %d，期望 %d", got, first.ID)
	}
}

func TestTenantIsolation(t *testing.T) {
	s, tenantA := testStore(t)
	ctx := context.Background()
	tenantB := time.Now().UnixNano() + 7
	if _, err := testDB.Pool().Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, $2, $3)`,
		tenantB, fmt.Sprintf("test-%d", tenantB), "隔离测试租户"); err != nil {
		t.Fatalf("创建第二个租户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testDB.Pool().Exec(context.Background(), `DELETE FROM tenants WHERE id = $1`, tenantB)
	})

	idnA := mustIdentity(t, asset.KindSubdomain, "same.example.com")
	a := mustAsset(t, s, tenantA, asset.KindSubdomain, "same.example.com", idnA)
	b := mustAsset(t, s, tenantB, asset.KindSubdomain, "same.example.com", idnA)
	if a.ID == b.ID {
		t.Fatalf("不同租户的同一身份键不应复用同一资产")
	}

	// 用 B 的租户读 A 的资产必须失败。
	if _, err := s.GetAsset(ctx, tenantB, a.ID); err == nil {
		t.Fatalf("跨租户读取资产应失败")
	}

	listA, err := s.ListAssets(ctx, tenantA, AssetFilter{})
	if err != nil {
		t.Fatalf("列出 A 租户资产失败: %v", err)
	}
	for _, item := range listA {
		if item.TenantID != tenantA {
			t.Fatalf("A 租户列表混入租户 %d 的资产", item.TenantID)
		}
	}
	if len(listA) != 1 {
		t.Fatalf("A 租户应有 1 个资产，实际 %d", len(listA))
	}
}

func TestListAssetsFilterAndPagination(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i)
		mustAsset(t, s, tenantID, asset.KindIP, ip, mustIdentity(t, asset.KindIP, ip))
	}

	ipKind := asset.KindIP
	page1, err := s.ListAssets(ctx, tenantID, AssetFilter{Type: &ipKind, Limit: 2})
	if err != nil {
		t.Fatalf("第一页查询失败: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("第一页应有 2 条，实际 %d", len(page1))
	}
	last := page1[len(page1)-1].ID
	page2, err := s.ListAssets(ctx, tenantID, AssetFilter{Type: &ipKind, Limit: 2, AfterID: last})
	if err != nil {
		t.Fatalf("第二页查询失败: %v", err)
	}
	if len(page2) != 2 || page2[0].ID <= last {
		t.Fatalf("第二页分页不正确: %+v", page2)
	}
	rootKind := asset.KindRootDomain
	empty, err := s.ListAssets(ctx, tenantID, AssetFilter{Type: &rootKind})
	if err != nil {
		t.Fatalf("按类型过滤失败: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("root_domain 类型应为空，实际 %d", len(empty))
	}
}

func TestMissionScopeLinksAndCoverage(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()

	mustAsset(t, s, tenantID, asset.KindRootDomain, "example.com",
		mustIdentity(t, asset.KindRootDomain, "example.com"))
	subA := mustAsset(t, s, tenantID, asset.KindSubdomain, "a.example.com",
		mustIdentity(t, asset.KindSubdomain, "a.example.com"))
	mustAsset(t, s, tenantID, asset.KindSubdomain, "b.example.com",
		mustIdentity(t, asset.KindSubdomain, "b.example.com"))
	outsideIP := mustAsset(t, s, tenantID, asset.KindIP, "203.0.113.9",
		mustIdentity(t, asset.KindIP, "203.0.113.9"))

	mission, err := s.CreateMission(ctx, tenantID, MissionInput{Name: "覆盖度测试", Goal: "验证覆盖度语义"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if err := s.AddMissionScope(ctx, tenantID, mission.ID, []ScopeEntry{
		{Kind: "root_domain", Value: "example.com", AllowedActions: []string{"http"}},
	}); err != nil {
		t.Fatalf("写入任务范围失败: %v", err)
	}
	linked, err := s.LinkMissionAssetsByScope(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("按范围登记资产失败: %v", err)
	}
	if linked != 3 {
		t.Fatalf("根域应登记 3 个资产（根域 + 2 子域），实际 %d", linked)
	}

	// 初始覆盖度为 0/3。
	cov, err := s.Coverage(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("计算覆盖度失败: %v", err)
	}
	if cov.Total != 3 || cov.Tested != 0 {
		t.Fatalf("初始覆盖度应为 0/3，实际 %d/%d", cov.Tested, cov.Total)
	}

	// 一个 fact 节点 tested 锚定 subA：1/3。
	fact, err := s.AddNode(ctx, tenantID, mission.ID, NodeInput{
		Kind:  graph.NodeFact,
		State: graph.StateDone,
		Title: "确认 a 子域存在未授权接口",
	})
	if err != nil {
		t.Fatalf("创建 fact 节点失败: %v", err)
	}
	if err := s.AddAnchor(ctx, tenantID, fact.ID, subA.ID, graph.RoleTested); err != nil {
		t.Fatalf("写入 tested 锚点失败: %v", err)
	}
	cov, err = s.Coverage(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("计算覆盖度失败: %v", err)
	}
	if cov.Total != 3 || cov.Tested != 1 {
		t.Fatalf("覆盖度应为 1/3，实际 %d/%d", cov.Tested, cov.Total)
	}

	// hint 锚定范围外资产：不进入分母，覆盖度仍为 1/3。
	hint, err := s.AddNode(ctx, tenantID, mission.ID, NodeInput{
		Kind:  graph.NodeHint,
		State: graph.StateDone,
		Title: "范围外线索：203.0.113.9 可能有服务",
	})
	if err != nil {
		t.Fatalf("创建 hint 节点失败: %v", err)
	}
	if err := s.AddAnchor(ctx, tenantID, hint.ID, outsideIP.ID, graph.RoleTouched); err != nil {
		t.Fatalf("写入 touched 锚点失败: %v", err)
	}
	cov, err = s.Coverage(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("计算覆盖度失败: %v", err)
	}
	if cov.Total != 3 || cov.Tested != 1 {
		t.Fatalf("hint 不应拉低覆盖度，期望 1/3，实际 %d/%d", cov.Tested, cov.Total)
	}

	// target 锚点把范围外资产纳入分母：1/4。
	hypo, err := s.AddNode(ctx, tenantID, mission.ID, NodeInput{
		Kind:  graph.NodeHypothesis,
		State: graph.StateOpen,
		Title: "验证 203.0.113.9 的服务",
	})
	if err != nil {
		t.Fatalf("创建 hypothesis 节点失败: %v", err)
	}
	if err := s.AddAnchor(ctx, tenantID, hypo.ID, outsideIP.ID, graph.RoleTarget); err != nil {
		t.Fatalf("写入 target 锚点失败: %v", err)
	}
	cov, err = s.Coverage(ctx, tenantID, mission.ID)
	if err != nil {
		t.Fatalf("计算覆盖度失败: %v", err)
	}
	if cov.Total != 4 || cov.Tested != 1 {
		t.Fatalf("target 锚点应扩大分母，期望 1/4，实际 %d/%d", cov.Tested, cov.Total)
	}
	if len(cov.ByType) != 3 {
		t.Fatalf("按类型应拆成 3 组（根域/子域/IP），实际 %d", len(cov.ByType))
	}
}

func TestEdgeMustStayInMission(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()
	m1, err := s.CreateMission(ctx, tenantID, MissionInput{Name: "任务一"})
	if err != nil {
		t.Fatalf("创建任务一失败: %v", err)
	}
	m2, err := s.CreateMission(ctx, tenantID, MissionInput{Name: "任务二"})
	if err != nil {
		t.Fatalf("创建任务二失败: %v", err)
	}
	n1, err := s.AddNode(ctx, tenantID, m1.ID, NodeInput{Kind: graph.NodeGoal, State: graph.StateOpen, Title: "目标一"})
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	n2, err := s.AddNode(ctx, tenantID, m2.ID, NodeInput{Kind: graph.NodeGoal, State: graph.StateOpen, Title: "目标二"})
	if err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	if _, err := s.AddEdge(ctx, tenantID, m1.ID, n1.ID, n2.ID, graph.RelSpawns); err == nil {
		t.Fatalf("跨任务连边应被数据库拒绝")
	}
}

func TestNodeValidation(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()
	m, err := s.CreateMission(ctx, tenantID, MissionInput{Name: "校验任务"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	if _, err := s.AddNode(ctx, tenantID, m.ID, NodeInput{Kind: "bogus", State: graph.StateOpen, Title: "x"}); err == nil {
		t.Fatalf("非法节点类型应被拒绝")
	}
	if _, err := s.AddNode(ctx, tenantID, m.ID, NodeInput{Kind: graph.NodeFact, State: graph.StateRunning, Title: "x"}); err == nil {
		t.Fatalf("fact 节点不应允许 running 状态")
	}
	if _, err := s.AddNode(ctx, tenantID, m.ID, NodeInput{Kind: graph.NodeAction, State: graph.StateRunning, Title: "x"}); err != nil {
		t.Fatalf("action 节点应允许 running 状态: %v", err)
	}
}

func TestUpsertRejectsInvalidParent(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()

	root := mustAsset(t, s, tenantID, asset.KindRootDomain, "parent.example.com",
		mustIdentity(t, asset.KindRootDomain, "parent.example.com"))

	missing := int64(9_999_999_999)
	if _, err := s.UpsertAsset(ctx, tenantID, AssetInput{
		Type:       asset.KindSubdomain,
		Label:      "child.example.com",
		ParentID:   &missing,
		Identities: []asset.Identity{mustIdentity(t, asset.KindSubdomain, "child.example.com")},
	}); err == nil {
		t.Fatalf("父资产不存在时应拒绝写入")
	}

	if _, err := s.UpsertAsset(ctx, tenantID, AssetInput{
		Type:       asset.KindRootDomain,
		Label:      "parent.example.com",
		ParentID:   &root.ID,
		Identities: []asset.Identity{mustIdentity(t, asset.KindRootDomain, "parent.example.com")},
	}); err == nil {
		t.Fatalf("资产不应以自身作为父资产")
	}

	child := mustAsset(t, s, tenantID, asset.KindSubdomain, "child.example.com",
		mustIdentity(t, asset.KindSubdomain, "child.example.com"))
	if _, err := s.UpsertAsset(ctx, tenantID, AssetInput{
		Type:       asset.KindSubdomain,
		Label:      "child.example.com",
		ParentID:   &root.ID,
		Identities: []asset.Identity{mustIdentity(t, asset.KindSubdomain, "child.example.com")},
	}); err != nil {
		t.Fatalf("合法父资产应写入成功: %v", err)
	}
	got, err := s.GetAsset(ctx, tenantID, child.ID)
	if err != nil {
		t.Fatalf("读取子资产失败: %v", err)
	}
	if got.ParentID == nil || *got.ParentID != root.ID {
		t.Fatalf("父资产未生效: %+v", got.ParentID)
	}
}

func TestSetNodeStateRespectsKindRules(t *testing.T) {
	s, tenantID := testStore(t)
	ctx := context.Background()
	m, err := s.CreateMission(ctx, tenantID, MissionInput{Name: "状态校验任务"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	fact, err := s.AddNode(ctx, tenantID, m.ID, NodeInput{Kind: graph.NodeFact, State: graph.StateDone, Title: "事实"})
	if err != nil {
		t.Fatalf("创建 fact 节点失败: %v", err)
	}
	if err := s.SetNodeState(ctx, tenantID, fact.ID, graph.StateRunning); err == nil {
		t.Fatalf("fact 节点不应被更新为 running")
	}
	action, err := s.AddNode(ctx, tenantID, m.ID, NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "动作"})
	if err != nil {
		t.Fatalf("创建 action 节点失败: %v", err)
	}
	if err := s.SetNodeState(ctx, tenantID, action.ID, graph.StateRunning); err != nil {
		t.Fatalf("action 节点应允许 running: %v", err)
	}
	got, err := s.GetNode(ctx, tenantID, action.ID)
	if err != nil {
		t.Fatalf("读取节点失败: %v", err)
	}
	if got.State != graph.StateRunning {
		t.Fatalf("节点状态未更新，实际 %q", got.State)
	}
}

func TestTenantRequired(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	if _, err := s.GetAsset(ctx, 0, 1); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("租户 0 应返回 ErrInvalidTenant，实际 %v", err)
	}
	if _, err := s.CreateMission(ctx, -1, MissionInput{Name: "x"}); !errors.Is(err, ErrInvalidTenant) {
		t.Fatalf("负租户应返回 ErrInvalidTenant，实际 %v", err)
	}
}

// TestAssetQueryPerf100k 验证 10 万资产规模下的查询延迟，默认跳过；
// 设置 BEACONX_PERF=1 后执行（需要真实数据库）。
func TestAssetQueryPerf100k(t *testing.T) {
	if os.Getenv("BEACONX_PERF") != "1" {
		t.Skip("未设置 BEACONX_PERF=1，跳过性能测试")
	}
	s, tenantID := testStore(t)
	ctx := context.Background()

	mission, err := s.CreateMission(ctx, tenantID, MissionInput{Name: "性能任务"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	const total = 100_000
	rows := make([][]any, 0, total)
	for i := 0; i < total; i++ {
		rows = append(rows, []any{tenantID, "ip", fmt.Sprintf("10.%d.%d.%d", i/65536%256, (i/256)%256, i%256)})
	}
	if _, err := testDB.Pool().CopyFrom(ctx, pgx.Identifier{"assets"},
		[]string{"tenant_id", "type", "label"}, pgx.CopyFromRows(rows)); err != nil {
		t.Fatalf("批量写入资产失败: %v", err)
	}
	if _, err := testDB.Pool().Exec(ctx,
		`INSERT INTO mission_asset_links (tenant_id, mission_id, asset_id, source)
		 SELECT tenant_id, $2, id, 'scope' FROM assets WHERE tenant_id = $1`,
		tenantID, mission.ID); err != nil {
		t.Fatalf("批量登记任务资产失败: %v", err)
	}

	measure := func(name string, fn func() error) time.Duration {
		t.Helper()
		if err := fn(); err != nil {
			t.Fatalf("%s 预热失败: %v", name, err)
		}
		const rounds = 20
		lat := make([]time.Duration, 0, rounds)
		for i := 0; i < rounds; i++ {
			start := time.Now()
			if err := fn(); err != nil {
				t.Fatalf("%s 执行失败: %v", name, err)
			}
			lat = append(lat, time.Since(start))
		}
		// 取第 95 百分位。
		for i := 0; i < len(lat); i++ {
			for j := i + 1; j < len(lat); j++ {
				if lat[j] < lat[i] {
					lat[i], lat[j] = lat[j], lat[i]
				}
			}
		}
		p95 := lat[(len(lat)*95)/100]
		t.Logf("%s 第 95 百分位延迟: %v", name, p95)
		return p95
	}

	ipKind := asset.KindIP
	p95List := measure("资产列表（type=ip，limit=50）", func() error {
		_, err := s.ListAssets(ctx, tenantID, AssetFilter{Type: &ipKind, Limit: 50})
		return err
	})
	p95Coverage := measure("覆盖度查询（10 万资产）", func() error {
		_, err := s.Coverage(ctx, tenantID, mission.ID)
		return err
	})
	if p95List > 200*time.Millisecond || p95Coverage > 200*time.Millisecond {
		t.Fatalf("查询延迟超标：列表 %v，覆盖度 %v（上限 200ms）", p95List, p95Coverage)
	}
}
