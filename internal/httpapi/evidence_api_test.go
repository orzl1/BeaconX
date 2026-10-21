package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// 证据接口端到端：冻结 → 校验通过；篡改数据库字段 → 409 tampered。
func TestEvidenceAPI(t *testing.T) {
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过证据接口测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := pg.Open(ctx, pg.Options{DSN: dsn, MaxConns: 4, MinConns: 1, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("连接数据库失败: %v", err)
	}
	defer db.Close()
	if _, err := migrate.Apply(ctx, db.Pool(), migrations.FS); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	tenantID := time.Now().UnixNano()
	if _, err := db.Pool().Exec(ctx, `INSERT INTO tenants (id, slug, name) VALUES ($1,$2,$3)`,
		tenantID, "evidence-api-"+time.Now().Format("150405.000000000"), "证据接口租户"); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = db.Pool().Exec(c, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	store := pg.NewStore(db)
	mission, err := store.CreateMission(ctx, tenantID, pg.MissionInput{Name: "证据接口"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	ev, err := evidence.New(evidence.Config{Dir: t.TempDir(), Key: key})
	if err != nil {
		t.Fatalf("创建证据存储失败: %v", err)
	}
	reqRef, _ := ev.PutBlob(ctx, []byte(`{"url":"https://example.com","method":"GET"}`))
	respRef, _ := ev.PutBlob(ctx, []byte("HTTP 200 OK"))
	trafficID, err := store.BeginTraffic(ctx, tenantID, mission.ID, nil, "http_probe", reqRef, "json-keys")
	if err != nil {
		t.Fatalf("写采集失败: %v", err)
	}
	if err := store.FinishTraffic(ctx, tenantID, trafficID, respRef, 200, ""); err != nil {
		t.Fatalf("完成采集失败: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := NewHandler(Options{
		Logger: logger, TenantID: tenantID, Store: store, Evidence: ev,
		Authenticator: NewTokenAuthenticator("t"),
	})
	if err != nil {
		t.Fatalf("构建处理器失败: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	do := func(method, path string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return resp
	}

	// 冻结证据。
	resp := do(http.MethodPost, "/api/v1/traffic/"+itoa(trafficID)+"/evidence")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("冻结证据 status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	var snapID int64
	if err := db.Pool().QueryRow(ctx,
		`SELECT id FROM evidence_snapshots WHERE tenant_id=$1 ORDER BY id DESC LIMIT 1`, tenantID).Scan(&snapID); err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}

	verify := func() int {
		r := do(http.MethodGet, "/api/v1/evidence/"+itoa(snapID)+"/verify")
		defer r.Body.Close()
		return r.StatusCode
	}
	if code := verify(); code != http.StatusOK {
		t.Fatalf("未篡改时校验 status=%d", code)
	}

	// 篡改数据库字段 → 409。
	if _, err := db.Pool().Exec(ctx,
		`UPDATE evidence_snapshots SET tool='tampered' WHERE tenant_id=$1 AND id=$2`, tenantID, snapID); err != nil {
		t.Fatalf("篡改失败: %v", err)
	}
	if code := verify(); code != http.StatusConflict {
		t.Fatalf("篡改后校验 status=%d，期望 409", code)
	}
}
