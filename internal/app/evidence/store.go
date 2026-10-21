// Package evidence 提供 BeaconX 的漏洞证据存储：正文加密落盘、内容哈希寻址、
// 快照签名与脱敏工具。
//
// 设计要点：
//   - blob 以明文 SHA-256 内容寻址，落盘路径为
//     <Dir>/blobs/<hash 前两位>/<hash>.enc，文件内容为 nonce||AES-256-GCM 密文，
//     明文正文从不写入磁盘；
//   - Snapshot 的 ID 是规范化元数据的 SHA-256，Signature 是对「含 ID、不含
//     Signature」的规范化 JSON 做 HMAC-SHA256；元数据任一字段被改动都会在
//     Verify 时返回 ErrTampered；
//   - 写入先落临时文件、fsync 后 rename，崩溃最多留下无引用的临时文件，不会留下
//     半个 blob；
//   - 加密子密钥与签名子密钥由同一把 32 字节主密钥经 HMAC-SHA256 派生，避免同一
//     密钥同时用于加密与签名；主密钥不落盘，也不会出现在错误信息里。
package evidence

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ErrTampered 表示证据内容与元数据不一致：哈希、签名或密文校验失败。
//
// 调用方应使用 errors.Is 判断，而不是比较错误字符串。
var ErrTampered = errors.New("证据校验失败：内容哈希或签名不匹配")

const (
	// keySize 是系统唯一主密钥的长度：32 字节同时满足 AES-256 与 HMAC-SHA256。
	keySize = 32
	// nonceSize 是 AES-GCM 的标准随机数长度。
	nonceSize = 12
	// tagSize 是 AES-GCM 认证标签长度，用于判断密文是否短到不可能合法。
	tagSize = 16
	// hashHexLen 是 SHA-256 十六进制字符串长度。
	hashHexLen = sha256.Size * 2
	// blobDirName 是证据根目录下的 blob 子目录名。
	blobDirName = "blobs"
	// blobExt 是加密 blob 的扩展名。
	blobExt = ".enc"
)

// 子密钥派生标签。改标签等于换密钥，会让所有存量证据无法解密/验签，
// 因此标签带版本号，只能新增不能修改。
const (
	encKeyLabel = "beaconx/evidence/aes-256-gcm/v1"
	macKeyLabel = "beaconx/evidence/hmac-sha256/v1"
)

// Config 是证据存储的初始化参数。
type Config struct {
	// Dir 是证据根目录，不存在时由 New 创建（权限 0700）。
	Dir string
	// Key 是 32 字节主密钥，用于派生 AES-256-GCM 加密子密钥与 HMAC-SHA256
	// 签名子密钥。Key 不会被写入磁盘，也不会出现在任何错误信息里。
	Key []byte
}

// BlobRef 指向一段按内容寻址的正文。
//
// Hash 是明文字节的 SHA-256 十六进制串；Size 是明文字节数。两者在读取时都会
// 被重新校验，任何一个不匹配都说明证据被改动或密钥不对。
type BlobRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Store 是证据存储句柄，可被多个 goroutine 并发使用。
//
// mu 保护密钥读取与轮换：普通读写取读锁，RotateKey 取写锁。这样轮换期间不会
// 出现「新 blob 用旧 key 加密」或「读到半完成状态」的混合密钥窗口。
type Store struct {
	mu     sync.RWMutex
	dir    string
	encKey []byte
	macKey []byte
}

// New 创建证据存储，并确保根目录与 blob 子目录存在（权限 0700）。
//
// New 会复制并派生密钥，之后修改传入的 Key 切片不会影响 Store。
func New(cfg Config) (*Store, error) {
	dir := cfg.Dir
	if dir == "" {
		return nil, errors.New("证据目录不能为空")
	}
	if len(cfg.Key) != keySize {
		return nil, fmt.Errorf("证据密钥长度必须为 %d 字节，当前为 %d 字节", keySize, len(cfg.Key))
	}
	encKey := deriveKey(cfg.Key, encKeyLabel)
	macKey := deriveKey(cfg.Key, macKeyLabel)
	if err := os.MkdirAll(filepath.Join(dir, blobDirName), 0o700); err != nil {
		return nil, fmt.Errorf("创建证据目录失败: %w", err)
	}
	return &Store{dir: dir, encKey: encKey, macKey: macKey}, nil
}

// deriveKey 用 HMAC-SHA256 从主密钥派生一把 32 字节子密钥。
//
// 这是 HKDF-Expand 的单块特例：主密钥是随机 32 字节，标签是固定 info，
// 因此派生结果确定且两把子密钥互不相关。
func deriveKey(master []byte, label string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// hmacDigest 返回 key 对 data 的 HMAC-SHA256。
func hmacDigest(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// sha256Hex 返回 data 的 SHA-256 十六进制串。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// blobPath 返回某个哈希对应的加密 blob 路径。
//
// 调用方必须先校验哈希格式，避免用未校验的字符串拼路径。
func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.dir, blobDirName, hash[:2], hash+blobExt)
}

// validHash 判断哈希是否为 64 位小写十六进制串。
func validHash(hash string) bool {
	if len(hash) != hashHexLen {
		return false
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// PutBlob 加密并保存一段正文，返回内容寻址引用。
//
// 相同内容幂等：已存在且能通过解密与哈希校验时直接复用，不重写文件；
// 已存在但校验失败时返回 ErrTampered，绝不用新内容覆盖疑似被篡改的证据。
func (s *Store) PutBlob(ctx context.Context, data []byte) (BlobRef, error) {
	if err := contextErr(ctx); err != nil {
		return BlobRef{}, fmt.Errorf("写入证据 blob 已中止: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ref := BlobRef{Hash: sha256Hex(data), Size: int64(len(data))}
	path := s.blobPath(ref.Hash)

	switch _, err := os.Stat(path); {
	case err == nil:
		existing, readErr := s.readBlob(path, ref)
		if readErr != nil {
			return BlobRef{}, readErr
		}
		if !bytes.Equal(existing, data) {
			return BlobRef{}, fmt.Errorf("%w：已存在的 blob 内容与哈希不一致（hash=%s）", ErrTampered, ref.Hash)
		}
		return ref, nil
	case errors.Is(err, os.ErrNotExist):
		// 正常路径：继续写入。
	default:
		return BlobRef{}, fmt.Errorf("检查证据 blob 失败: %w", err)
	}

	if err := contextErr(ctx); err != nil {
		return BlobRef{}, fmt.Errorf("写入证据 blob 已中止: %w", err)
	}
	payload, err := s.sealBlob(ref.Hash, data)
	if err != nil {
		return BlobRef{}, err
	}
	if err := s.writeBlobFile(path, payload); err != nil {
		return BlobRef{}, err
	}
	return ref, nil
}

// GetBlob 读取、解密并校验一段正文。
//
// 文件缺失、密文被改动、密钥不匹配、大小或哈希与引用不一致时都返回
// ErrTampered（附带具体原因），调用方可用 errors.Is 统一处理。
func (s *Store) GetBlob(ctx context.Context, ref BlobRef) ([]byte, error) {
	if err := contextErr(ctx); err != nil {
		return nil, fmt.Errorf("读取证据 blob 已中止: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !validHash(ref.Hash) {
		return nil, fmt.Errorf("%w：blob 哈希必须为 %d 位小写十六进制串（hash=%q）", ErrTampered, hashHexLen, ref.Hash)
	}
	if ref.Size < 0 {
		return nil, fmt.Errorf("%w：blob 大小不能为负数（size=%d）", ErrTampered, ref.Size)
	}
	plain, err := s.readBlob(s.blobPath(ref.Hash), ref)
	if err != nil {
		return nil, err
	}
	if err := contextErr(ctx); err != nil {
		return nil, fmt.Errorf("读取证据 blob 已中止: %w", err)
	}
	return plain, nil
}

// sealBlob 用当前加密子密钥加密正文，返回 nonce||密文。
func (s *Store) sealBlob(hash string, plain []byte) ([]byte, error) {
	return sealWithKey(s.encKey, hash, plain)
}

// sealWithKey 用指定 AES-256 子密钥加密正文，返回 nonce||密文。
//
// AAD 使用明文哈希，把密文绑定到内容地址：把合法 blob 文件复制到另一个哈希
// 路径下也会解密失败，而不是只在末尾的哈希比对时才被发现。
func sealWithKey(encKey []byte, hash string, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("初始化证据加密器失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化证据加密模式失败: %w", err)
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成证据加密随机数失败: %w", err)
	}
	return gcm.Seal(nonce, nonce, plain, []byte(hash)), nil
}

// decryptWithKey 用指定 AES-256 子密钥解密 nonce||密文，并校验 AAD 中的哈希。
func decryptWithKey(encKey []byte, hash string, raw []byte) ([]byte, error) {
	if len(raw) < nonceSize+tagSize {
		return nil, fmt.Errorf("%w：blob 密文长度不足（hash=%s）", ErrTampered, hash)
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return nil, fmt.Errorf("初始化证据解密器失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化证据解密模式失败: %w", err)
	}
	plain, err := gcm.Open(nil, raw[:nonceSize], raw[nonceSize:], []byte(hash))
	if err != nil {
		return nil, fmt.Errorf("%w：blob 解密失败，文件被篡改或密钥不匹配（hash=%s）", ErrTampered, hash)
	}
	return plain, nil
}

// readBlob 读取并校验一个加密 blob，返回明文。
func (s *Store) readBlob(path string, ref BlobRef) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w：blob 文件不存在（hash=%s）", ErrTampered, ref.Hash)
		}
		return nil, fmt.Errorf("读取证据 blob 失败: %w", err)
	}
	plain, err := decryptWithKey(s.encKey, ref.Hash, raw)
	if err != nil {
		return nil, err
	}
	if int64(len(plain)) != ref.Size {
		return nil, fmt.Errorf("%w：blob 大小不匹配（hash=%s，引用 %d 字节，实际 %d 字节）",
			ErrTampered, ref.Hash, ref.Size, len(plain))
	}
	if got := sha256Hex(plain); got != ref.Hash {
		return nil, fmt.Errorf("%w：blob 内容哈希不匹配（引用 %s，实际 %s）", ErrTampered, ref.Hash, got)
	}
	return plain, nil
}

// writeBlobFile 把加密内容原子地写到 path：临时文件 → fsync → rename → 目录 fsync。
func (s *Store) writeBlobFile(path string, payload []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建证据 blob 目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".blob-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时证据文件失败: %w", err)
	}
	tmpName := tmp.Name()
	// rename 成功后临时文件名已不存在，这次删除是幂等的兜底清理。
	defer func() { _ = os.Remove(tmpName) }()

	// CreateTemp 默认就是 0600，这里显式设置，避免依赖实现细节。
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置临时证据文件权限失败: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时证据文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时证据文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时证据文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("提交证据 blob 失败: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("同步证据 blob 目录失败: %w", err)
	}
	return nil
}

// syncDir 对目录做 fsync，确保 rename 结果在崩溃后仍然可见。
//
// Windows 不支持对目录 fsync，直接视为成功（部署上另有 ACL 要求）。
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// contextErr 返回 ctx 的取消原因；nil ctx 视为未取消。
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// RotateKey 用新主密钥重新加密全部 blob，并在全部成功后切换内存密钥。
//
// 语义与安全边界（务必先读）：
//   - newKey 必须是 32 字节主密钥；只处理 <Dir>/blobs 下的 .enc 文件；
//   - 逐个文件执行「旧 key 解密（AAD=哈希）→ 新 key 加密（新随机 nonce）→
//     临时文件 + rename 原子替换」；
//   - 全部成功后同时切换加密子密钥与签名子密钥。注意：签名子密钥变化会让**存量
//     Snapshot 的 HMAC 签名失效**（Verify 返回 ErrTampered），而证据包无数据库
//     访问、无法自动重签。这是「无 key-id、单密钥」方案的已知限制：生产轮换必须
//     先备份 PostgreSQL 与 evidence 目录，并配套离线重签流程（后续里程碑）；
//   - 任一步失败时，对已替换的文件尝试用旧 key 重加密回滚（best-effort），并返回
//     已完成数量与错误；若回滚也失败，磁盘可能处于混合密钥状态，必须人工从备份
//     恢复。调用方应把进程置为只读/停机后再轮换，避免新的写入；
//   - 轮换全程持有写锁，PutBlob/GetBlob/Seal/Verify 会阻塞，直到轮换结束。
//
// 返回值 count 是本次成功重新加密的 blob 数量（失败场景为失败前已完成的数量）。
func (s *Store) RotateKey(ctx context.Context, newKey []byte) (int, error) {
	if len(newKey) != keySize {
		return 0, fmt.Errorf("新证据密钥长度必须为 %d 字节，当前为 %d 字节", keySize, len(newKey))
	}
	newEncKey := deriveKey(newKey, encKeyLabel)
	newMACKey := deriveKey(newKey, macKeyLabel)

	s.mu.Lock()
	defer s.mu.Unlock()

	oldEncKey := s.encKey
	paths, err := s.blobFiles()
	if err != nil {
		return 0, err
	}
	rotated := make([]string, 0, len(paths))
	for _, path := range paths {
		if err := contextErr(ctx); err != nil {
			rolled := s.rollbackRotated(rotated, newEncKey, oldEncKey)
			return len(rotated), fmt.Errorf("证据密钥轮换已中止（已回滚 %d 个）: %w", rolled, err)
		}
		hash := strings.TrimSuffix(filepath.Base(path), blobExt)
		if !validHash(hash) {
			rolled := s.rollbackRotated(rotated, newEncKey, oldEncKey)
			return len(rotated), fmt.Errorf("轮换失败：blob 文件名 %q 不是合法哈希（已回滚 %d 个）", filepath.Base(path), rolled)
		}
		if err := s.reencryptFile(path, hash, oldEncKey, newEncKey); err != nil {
			rolled := s.rollbackRotated(rotated, newEncKey, oldEncKey)
			return len(rotated), fmt.Errorf("轮换 blob %s 失败（已回滚 %d 个）: %w", hash, rolled, err)
		}
		rotated = append(rotated, path)
	}
	s.encKey = newEncKey
	s.macKey = newMACKey
	return len(rotated), nil
}

// blobFiles 返回 <Dir>/blobs 下全部 .enc 文件（按字典序，保证轮换过程可复现）。
func (s *Store) blobFiles() ([]string, error) {
	root := filepath.Join(s.dir, blobDirName)
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(entry.Name(), blobExt) {
			paths = append(paths, path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("遍历证据 blob 失败: %w", err)
	}
	return paths, nil
}

// reencryptFile 用旧密钥解密单个 blob，再用新密钥加密并原子替换。
func (s *Store) reencryptFile(path, hash string, oldEncKey, newEncKey []byte) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 blob 失败: %w", err)
	}
	plain, err := decryptWithKey(oldEncKey, hash, raw)
	if err != nil {
		return err
	}
	if got := sha256Hex(plain); got != hash {
		return fmt.Errorf("%w：blob 内容哈希与文件名不一致（文件 %s，实际 %s）", ErrTampered, hash, got)
	}
	payload, err := sealWithKey(newEncKey, hash, plain)
	if err != nil {
		return err
	}
	return s.writeBlobFile(path, payload)
}

// rollbackRotated 尝试把已用新密钥重写的 blob 用旧密钥写回。
//
// 返回成功回滚的数量。任何单个文件失败都跳过并继续（尽最大努力恢复到旧密钥
// 状态）；失败的文件需要人工从备份恢复。
func (s *Store) rollbackRotated(paths []string, newEncKey, oldEncKey []byte) int {
	rolled := 0
	for _, path := range paths {
		hash := strings.TrimSuffix(filepath.Base(path), blobExt)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		plain, err := decryptWithKey(newEncKey, hash, raw)
		if err != nil {
			continue
		}
		payload, err := sealWithKey(oldEncKey, hash, plain)
		if err != nil {
			continue
		}
		if err := s.writeBlobFile(path, payload); err != nil {
			continue
		}
		rolled++
	}
	return rolled
}
