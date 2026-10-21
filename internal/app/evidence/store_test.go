package evidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testKey 生成一把确定性的 32 字节测试密钥；seed 不同即不同密钥。
func testKey(seed byte) []byte {
	key := make([]byte, keySize)
	for i := range key {
		key[i] = seed + byte(i)
	}
	return key
}

// newTestStore 在临时目录下创建证据存储。
func newTestStore(t *testing.T, seed byte) *Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "evidence")
	st, err := New(Config{Dir: dir, Key: testKey(seed)})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	return st
}

func mustPut(t *testing.T, st *Store, data []byte) BlobRef {
	t.Helper()
	ref, err := st.PutBlob(context.Background(), data)
	if err != nil {
		t.Fatalf("PutBlob：%v", err)
	}
	return ref
}

func mustGet(t *testing.T, st *Store, ref BlobRef) []byte {
	t.Helper()
	got, err := st.GetBlob(context.Background(), ref)
	if err != nil {
		t.Fatalf("GetBlob：%v", err)
	}
	return got
}

func TestNewValidatesKey(t *testing.T) {
	for _, size := range []int{0, 1, 16, 31, 33, 64} {
		_, err := New(Config{Dir: t.TempDir(), Key: make([]byte, size)})
		if err == nil {
			t.Errorf("密钥长度 %d 应被拒绝", size)
			continue
		}
		if !strings.Contains(err.Error(), "32") {
			t.Errorf("密钥长度 %d 的错误信息应提示 32 字节，实际：%v", size, err)
		}
	}
	if _, err := New(Config{Dir: t.TempDir(), Key: testKey(1)}); err != nil {
		t.Fatalf("32 字节密钥应被接受：%v", err)
	}
}

func TestNewRejectsEmptyDir(t *testing.T) {
	if _, err := New(Config{Key: testKey(1)}); err == nil {
		t.Fatal("空目录应被拒绝")
	}
}

func TestNewCreatesPrivateDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 POSIX 权限位没有意义")
	}
	dir := filepath.Join(t.TempDir(), "nested", "evidence")
	if _, err := New(Config{Dir: dir, Key: testKey(1)}); err != nil {
		t.Fatalf("New：%v", err)
	}
	for _, path := range []string{dir, filepath.Join(dir, blobDirName)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("缺少目录 %s：%v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("目录 %s 权限 = %o，期望 700", path, perm)
		}
	}
}

func TestPutGetBlobRoundTrip(t *testing.T) {
	st := newTestStore(t, 1)
	payloads := [][]byte{
		nil,
		[]byte(""),
		[]byte("hello evidence"),
		{0x00, 0x01, 0xff, 0x00, 0xfe},
		bytes.Repeat([]byte("0123456789abcdef"), 8192),
	}
	for _, payload := range payloads {
		ref := mustPut(t, st, payload)
		if ref.Size != int64(len(payload)) {
			t.Errorf("size = %d，期望 %d", ref.Size, len(payload))
		}
		sum := sha256.Sum256(payload)
		if want := hex.EncodeToString(sum[:]); ref.Hash != want {
			t.Errorf("hash = %s，期望 %s", ref.Hash, want)
		}
		if got := mustGet(t, st, ref); !bytes.Equal(got, payload) {
			t.Errorf("往返内容不一致：得到 %d 字节，期望 %d 字节", len(got), len(payload))
		}
	}
}

func TestBlobFileShapeAndPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 POSIX 权限位没有意义")
	}
	st := newTestStore(t, 1)
	payload := []byte("SECRET-PLAINTEXT-MARKER")
	ref := mustPut(t, st, payload)

	path := st.blobPath(ref.Hash)
	if wantSuffix := blobExt; !strings.HasSuffix(path, wantSuffix) {
		t.Errorf("路径 %s 应以 %s 结尾", path, wantSuffix)
	}
	if dir := filepath.Base(filepath.Dir(path)); dir != ref.Hash[:2] {
		t.Errorf("bucket 目录 = %s，期望 %s", dir, ref.Hash[:2])
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("blob 文件：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("blob 文件权限 = %o，期望 600", perm)
	}
	bucketInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("bucket 目录：%v", err)
	}
	if perm := bucketInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("bucket 目录权限 = %o，期望 700", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 blob：%v", err)
	}
	if want := nonceSize + len(payload) + tagSize; len(raw) != want {
		t.Errorf("密文长度 = %d，期望 %d（nonce||密文）", len(raw), want)
	}
	if bytes.Contains(raw, payload) {
		t.Error("密文中不应出现明文")
	}
}

func TestPutBlobReusesExistingFile(t *testing.T) {
	st := newTestStore(t, 1)
	payload := []byte("idempotent-evidence")
	first := mustPut(t, st, payload)
	path := st.blobPath(first.Hash)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 blob：%v", err)
	}
	second := mustPut(t, st, payload)
	if first != second {
		t.Fatalf("同一内容应返回相同引用：%+v vs %+v", first, second)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("再次读取 blob：%v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("已存在且校验通过的 blob 不应被重写")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读取 bucket 目录：%v", err)
	}
	if len(entries) != 1 {
		t.Errorf("bucket 目录应只有 1 个文件，实际 %d 个", len(entries))
	}
}

func TestPutBlobConcurrentSameContent(t *testing.T) {
	st := newTestStore(t, 1)
	payload := bytes.Repeat([]byte("concurrent-evidence-"), 4096)

	const workers = 16
	refs := make([]BlobRef, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			refs[i], errs[i] = st.PutBlob(context.Background(), payload)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发 PutBlob[%d]：%v", i, err)
		}
		if refs[i] != refs[0] {
			t.Fatalf("并发 PutBlob[%d] 引用不一致：%+v vs %+v", i, refs[i], refs[0])
		}
	}
	if got := mustGet(t, st, refs[0]); !bytes.Equal(got, payload) {
		t.Fatal("并发写入后读取内容不一致")
	}
	entries, err := os.ReadDir(filepath.Dir(st.blobPath(refs[0].Hash)))
	if err != nil {
		t.Fatalf("读取 bucket 目录：%v", err)
	}
	if len(entries) != 1 {
		t.Errorf("并发写同一内容后 bucket 目录应只有 1 个文件，实际 %d 个", len(entries))
	}
}

func TestGetBlobDetectsTampering(t *testing.T) {
	mutations := map[string]func(path string, raw []byte) ([]byte, error){
		"翻转密文字节": func(_ string, raw []byte) ([]byte, error) {
			raw[len(raw)-1] ^= 0xff
			return raw, nil
		},
		"截断到只剩 nonce": func(_ string, raw []byte) ([]byte, error) {
			return raw[:nonceSize], nil
		},
		"追加垃圾字节": func(_ string, raw []byte) ([]byte, error) {
			return append(raw, 0x00), nil
		},
		"替换为完全无关内容": func(_ string, _ []byte) ([]byte, error) {
			return []byte("not-an-encrypted-blob"), nil
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			st := newTestStore(t, 1)
			ref := mustPut(t, st, []byte("tamper-me"))
			path := st.blobPath(ref.Hash)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读取 blob：%v", err)
			}
			mutated, err := mutate(path, raw)
			if err != nil {
				t.Fatalf("构造篡改内容：%v", err)
			}
			if err := os.WriteFile(path, mutated, 0o600); err != nil {
				t.Fatalf("写入篡改内容：%v", err)
			}
			if _, err := st.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
				t.Fatalf("篡改后应返回 ErrTampered，实际：%v", err)
			}
		})
	}
}

func TestGetBlobMissingFile(t *testing.T) {
	st := newTestStore(t, 1)
	ref := mustPut(t, st, []byte("vanishing"))
	if err := os.Remove(st.blobPath(ref.Hash)); err != nil {
		t.Fatalf("删除 blob：%v", err)
	}
	if _, err := st.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
		t.Fatalf("文件缺失应返回 ErrTampered，实际：%v", err)
	}
}

func TestPutBlobRefusesCorruptExisting(t *testing.T) {
	st := newTestStore(t, 1)
	payload := []byte("must-not-be-overwritten")
	ref := mustPut(t, st, payload)
	path := st.blobPath(ref.Hash)
	corrupt := []byte("corrupted-in-place")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("写入损坏内容：%v", err)
	}
	if _, err := st.PutBlob(context.Background(), payload); !errors.Is(err, ErrTampered) {
		t.Fatalf("已存在损坏 blob 时应返回 ErrTampered，实际：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 blob：%v", err)
	}
	if !bytes.Equal(raw, corrupt) {
		t.Error("损坏的 blob 不应被新内容覆盖")
	}
}

func TestGetBlobWrongKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "evidence")
	stA, err := New(Config{Dir: dir, Key: testKey(1)})
	if err != nil {
		t.Fatalf("New A：%v", err)
	}
	ref := mustPut(t, stA, []byte("encrypted-with-key-a"))

	stB, err := New(Config{Dir: dir, Key: testKey(2)})
	if err != nil {
		t.Fatalf("New B：%v", err)
	}
	if _, err := stB.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
		t.Fatalf("错误密钥应返回 ErrTampered，实际：%v", err)
	}
	// 已存在但用另一把密钥加密的 blob 同样不能被静默覆盖。
	if _, err := stB.PutBlob(context.Background(), []byte("encrypted-with-key-a")); !errors.Is(err, ErrTampered) {
		t.Fatalf("错误密钥下 PutBlob 应返回 ErrTampered，实际：%v", err)
	}
}

func TestGetBlobRejectsInvalidRefs(t *testing.T) {
	st := newTestStore(t, 1)
	valid := sha256Hex([]byte("x"))
	if _, err := st.GetBlob(context.Background(), BlobRef{Hash: valid, Size: 0}); !errors.Is(err, ErrTampered) {
		// 文件不存在，属于篡改/缺失语义。
		t.Fatalf("不存在的合法哈希应返回 ErrTampered，实际：%v", err)
	}
	cases := []BlobRef{
		{Hash: "", Size: 0},
		{Hash: "abc", Size: 0},
		{Hash: strings.Repeat("A", hashHexLen), Size: 0},
		{Hash: "../" + valid, Size: 0},
		{Hash: valid[:hashHexLen-1] + "g", Size: 0},
		{Hash: valid, Size: -1},
	}
	for _, ref := range cases {
		if _, err := st.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
			t.Errorf("非法引用 %+v 应返回 ErrTampered，实际：%v", ref, err)
		}
	}

	ref := mustPut(t, st, []byte("size-mismatch"))
	ref.Size++
	if _, err := st.GetBlob(context.Background(), ref); !errors.Is(err, ErrTampered) {
		t.Fatalf("大小不匹配应返回 ErrTampered，实际：%v", err)
	}
}

func TestBlobOperationsHonorContext(t *testing.T) {
	st := newTestStore(t, 1)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.PutBlob(cancelled, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 PutBlob 应返回 context.Canceled，实际：%v", err)
	}
	ref := mustPut(t, st, []byte("y"))
	if _, err := st.GetBlob(cancelled, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 GetBlob 应返回 context.Canceled，实际：%v", err)
	}
}

func TestNewCopiesKey(t *testing.T) {
	key := testKey(7)
	dir := filepath.Join(t.TempDir(), "evidence")
	st, err := New(Config{Dir: dir, Key: key})
	if err != nil {
		t.Fatalf("New：%v", err)
	}
	key[0] ^= 0xff // 调用方事后改密钥不应影响 Store。
	ref := mustPut(t, st, []byte("key-copy"))
	if got := mustGet(t, st, ref); string(got) != "key-copy" {
		t.Fatalf("内容 = %q，期望 key-copy", got)
	}

	// 同一把原始密钥重新打开同一目录，必须能读出旧证据（派生是确定性的）。
	reopened, err := New(Config{Dir: dir, Key: testKey(7)})
	if err != nil {
		t.Fatalf("重新打开：%v", err)
	}
	if got := mustGet(t, reopened, ref); string(got) != "key-copy" {
		t.Fatalf("重新打开后内容 = %q，期望 key-copy", got)
	}
}

func TestBlobNonceIsRandomized(t *testing.T) {
	st := newTestStore(t, 1)
	first := mustPut(t, st, []byte("first"))
	second := mustPut(t, st, []byte("second"))
	rawFirst, err := os.ReadFile(st.blobPath(first.Hash))
	if err != nil {
		t.Fatalf("读取第一个 blob：%v", err)
	}
	rawSecond, err := os.ReadFile(st.blobPath(second.Hash))
	if err != nil {
		t.Fatalf("读取第二个 blob：%v", err)
	}
	if bytes.Equal(rawFirst[:nonceSize], rawSecond[:nonceSize]) {
		t.Error("两次加密的 nonce 不应相同")
	}
}
