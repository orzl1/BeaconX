package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orzl1/BeaconX/internal/app/evidence"
	"github.com/orzl1/BeaconX/internal/app/tools"
	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/domain/policy"
	"github.com/orzl1/BeaconX/internal/infra/migrate"
	"github.com/orzl1/BeaconX/internal/infra/pg"
	"github.com/orzl1/BeaconX/migrations"
)

func TestEvidenceCaptureAndTamperDetection(t *testing.T) {
	dsn := os.Getenv("BEACONX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 BEACONX_TEST_PG_DSN，跳过证据采集测试")
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
		tenantID, fmt.Sprintf("ev-test-%d", tenantID), "证据测试租户"); err != nil {
		t.Fatalf("创建租户失败: %v", err)
	}
	t.Cleanup(func() {
		c, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		_, _ = db.Pool().Exec(c, `DELETE FROM tenants WHERE id = $1`, tenantID)
	})
	store := pg.NewStore(db)
	if err := store.EnsurePolicyRules(ctx, tenantID, policy.SeedDefaults()); err != nil {
		t.Fatalf("写入规则失败: %v", err)
	}
	mission, err := store.CreateMission(ctx, tenantID, pg.MissionInput{Name: "证据采集"})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	node, err := store.AddNode(ctx, tenantID, mission.ID, pg.NodeInput{Kind: graph.NodeAction, State: graph.StateOpen, Title: "GET"})
	if err != nil {
		t.Fatalf("创建动作失败: %v", err)
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	evDir := t.TempDir()
	evStore, err := evidence.New(evidence.Config{Dir: evDir, Key: key})
	if err != nil {
		t.Fatalf("创建证据存储失败: %v", err)
	}
	registry, _ := tools.NewRegistry(tools.NewHTTPProbe())
	exec := New(store, registry, Config{
		TenantID: tenantID, DefaultAction: policy.Ask, AskTimeout: time.Second,
		PollInterval: 20 * time.Millisecond, Evidence: evStore,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("beaconx-evidence-body"))
	}))
	defer target.Close()

	args, _ := json.Marshal(map[string]string{"url": target.URL, "method": "GET"})
	if _, err := exec.Execute(ctx, mission.ID, node.ID, "http_probe", args); err != nil {
		t.Fatalf("执行工具失败: %v", err)
	}

	traffic, err := store.ListTraffic(ctx, tenantID, mission.ID, 10)
	if err != nil {
		t.Fatalf("读取采集失败: %v", err)
	}
	if len(traffic) != 1 || traffic[0].Phase != "result" || traffic[0].RequestHash == "" || traffic[0].ResponseHash == "" {
		t.Fatalf("采集记录不符合预期: %+v", traffic)
	}
	ex := traffic[0]
	req := evidence.BlobRef{Hash: ex.RequestHash, Size: ex.RequestSize}
	resp := evidence.BlobRef{Hash: ex.ResponseHash, Size: ex.ResponseSize}

	// 正文可解密且内容正确（请求已脱敏）。
	reqBody, err := evStore.GetBlob(ctx, req)
	if err != nil {
		t.Fatalf("读取请求正文失败: %v", err)
	}
	if !strings.Contains(string(reqBody), "http") {
		t.Fatalf("请求正文不符合预期: %s", reqBody)
	}
	respBody, _ := evStore.GetBlob(ctx, resp)
	if !strings.Contains(string(respBody), "beaconx-evidence-body") {
		t.Fatalf("响应正文不符合预期: %s", respBody)
	}

	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	sealed, err := evStore.Seal(evidence.Snapshot{
		MissionID: mission.ID, Tool: ex.Tool, Request: req, Response: resp,
		Redaction: ex.Redaction, CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	snap, err := store.CreateEvidenceSnapshot(ctx, tenantID, mission.ID, ex.Tool, req, resp, sealed.Signature, ex.Redaction, createdAt)
	if err != nil {
		t.Fatalf("创建证据失败: %v", err)
	}

	verify := func() error {
		got, err := store.GetEvidenceSnapshot(ctx, tenantID, snap.ID)
		if err != nil {
			return err
		}
		recomputed, err := evStore.Seal(evidence.Snapshot{
			MissionID: got.MissionID, Tool: got.Tool,
			Request:   evidence.BlobRef{Hash: got.RequestHash, Size: got.RequestSize},
			Response:  evidence.BlobRef{Hash: got.ResponseHash, Size: got.ResponseSize},
			Redaction: got.Redaction, CreatedAt: got.CreatedAt,
		})
		if err != nil {
			return err
		}
		if recomputed.Signature != got.Signature {
			return evidence.ErrTampered
		}
		if _, err := evStore.GetBlob(ctx, evidence.BlobRef{Hash: got.RequestHash, Size: got.RequestSize}); err != nil {
			return err
		}
		_, err = evStore.GetBlob(ctx, evidence.BlobRef{Hash: got.ResponseHash, Size: got.ResponseSize})
		return err
	}
	if err := verify(); err != nil {
		t.Fatalf("未篡改时应校验通过: %v", err)
	}

	// 篡改数据库字段：签名校验必须失败。
	if _, err := db.Pool().Exec(ctx, `UPDATE evidence_snapshots SET tool = 'tampered' WHERE tenant_id=$1 AND id=$2`, tenantID, snap.ID); err != nil {
		t.Fatalf("篡改测试数据失败: %v", err)
	}
	if err := verify(); !errors.Is(err, evidence.ErrTampered) {
		t.Fatalf("数据库字段被篡改时校验应失败，实际: %v", err)
	}

	// 篡改正文文件：内容哈希校验必须失败。
	blobPath := filepath.Join(evDir, "blobs", resp.Hash[:2], resp.Hash+".enc")
	raw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("读取密文失败: %v", err)
	}
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(blobPath, raw, 0o600); err != nil {
		t.Fatalf("篡改密文失败: %v", err)
	}
	if _, err := evStore.GetBlob(ctx, resp); !errors.Is(err, evidence.ErrTampered) {
		t.Fatalf("正文被篡改时应返回 ErrTampered，实际: %v", err)
	}
}
