package evidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRotateKeyReencryptsAllBlobs 验证轮换后同一 Store 可读、新 key 可读、旧 key 不可读。
func TestRotateKeyReencryptsAllBlobs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evidence")
	oldKey := testKey(1)
	newKey := testKey(2)
	store, err := New(Config{Dir: dir, Key: oldKey})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	payloads := [][]byte{[]byte("first"), []byte("第二段正文"), []byte("third")}
	refs := make([]BlobRef, 0, len(payloads))
	for _, payload := range payloads {
		refs = append(refs, mustPut(t, store, payload))
	}

	count, err := store.RotateKey(context.Background(), newKey)
	if err != nil {
		t.Fatalf("RotateKey：%v", err)
	}
	if count != len(refs) {
		t.Fatalf("rotated = %d，期望 %d", count, len(refs))
	}
	// 同一 Store（内存 key 已切换）仍可读。
	for i, ref := range refs {
		if got := mustGet(t, store, ref); string(got) != string(payloads[i]) {
			t.Errorf("轮换后第 %d 条正文不一致", i)
		}
	}
	// 新 key 重新打开可读。
	reopenedNew, err := New(Config{Dir: dir, Key: newKey})
	if err != nil {
		t.Fatalf("New(new)：%v", err)
	}
	for _, ref := range refs {
		if _, err := reopenedNew.GetBlob(context.Background(), ref); err != nil {
			t.Errorf("新 key 读取失败：%v", err)
		}
	}
	// 旧 key 重新打开不可读（应 ErrTampered）。
	reopenedOld, err := New(Config{Dir: dir, Key: oldKey})
	if err != nil {
		t.Fatalf("New(old)：%v", err)
	}
	for _, ref := range refs {
		if _, err := reopenedOld.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
			t.Errorf("旧 key 应返回 ErrTampered，实际：%v", err)
		}
	}
}

// TestRotateKeyRejectsInvalidKey 校验新密钥长度。
func TestRotateKeyRejectsInvalidKey(t *testing.T) {
	store, err := New(Config{Dir: filepath.Join(t.TempDir(), "evidence"), Key: testKey(1)})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	for _, size := range []int{0, 16, 31, 33} {
		if _, err := store.RotateKey(context.Background(), make([]byte, size)); err == nil {
			t.Errorf("长度 %d 的新密钥应被拒绝", size)
		}
	}
}

// TestRotateKeyKeepsTamperDetection 验证轮换后篡改仍被检测。
func TestRotateKeyKeepsTamperDetection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evidence")
	store, err := New(Config{Dir: dir, Key: testKey(1)})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	ref := mustPut(t, store, []byte("tamper-after-rotate"))
	if _, err := store.RotateKey(context.Background(), testKey(2)); err != nil {
		t.Fatalf("RotateKey：%v", err)
	}
	path := store.blobPath(ref.Hash)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 blob：%v", err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写入篡改内容：%v", err)
	}
	if _, err := store.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
		t.Fatalf("轮换后篡改应返回 ErrTampered，实际：%v", err)
	}
}

// TestRotateKeyInvalidatesExistingSnapshotSignatures 记录当前方案的已知限制：
// 签名子密钥随主密钥轮换，存量快照签名会失效（需要离线重签）。
func TestRotateKeyInvalidatesExistingSnapshotSignatures(t *testing.T) {
	store, err := New(Config{Dir: filepath.Join(t.TempDir(), "evidence"), Key: testKey(1)})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	sealed := mustSeal(t, store, testSnapshot())
	if err := store.Verify(sealed); err != nil {
		t.Fatalf("轮换前 Verify：%v", err)
	}
	if _, err := store.RotateKey(context.Background(), testKey(3)); err != nil {
		t.Fatalf("RotateKey：%v", err)
	}
	if err := store.Verify(sealed); !errors.Is(err, ErrTampered) {
		t.Fatalf("轮换后旧签名应失效（ErrTampered），实际：%v", err)
	}
}
