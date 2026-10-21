// Package workspace 提供按「租户 / 任务」隔离的工作目录与安全文件操作。
//
// 目录布局（与状态目录约定一致）：
//
//	<stateDir>/tenants/<tenantID>/workspace/missions/<missionID>/
//
// 安全边界：
//   - 所有相对路径先 filepath.Clean，再校验必须落在任务根目录内；绝对路径、
//     ".." 越界、盘符路径、NUL 字节一律拒绝；
//   - 解析已存在路径时用 EvalSymlinks 校验真实位置仍在根目录内，拒绝通过
//     符号链接逃逸（根目录本身允许是符号链接，以解析后的真实路径为基准）；
//   - 文本文件必须是合法 UTF-8 且不含 NUL（二进制拒绝）；
//   - 单文件写入 ≤1MB、上传 ≤10MB、读取 ≤10MB；目录递归列表 ≤2000 项。
//
// 本包只做文件系统操作，不访问数据库；任务是否存在、是否属于当前租户由调用方
// （HTTP 层）先用 Store 校验。
package workspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/orzl1/BeaconX/internal/config"
)

// 文件与列表的大小上限。
const (
	// MaxFileBytes 是 PUT 写文本的单文件上限（1MB）。
	MaxFileBytes int64 = 1 << 20
	// MaxUploadBytes 是 multipart 上传的单文件上限（10MB）。
	MaxUploadBytes int64 = 10 << 20
	// MaxReadBytes 是单次读取的上限；超过时拒绝，避免把超大文件读进内存。
	MaxReadBytes int64 = 10 << 20
	// MaxListEntries 是递归列目录的最大条目数（超出截断）。
	MaxListEntries = 2000
	// DefaultQuotaMB 是每任务工作区的默认总大小上限（100MB）。
	DefaultQuotaMB int64 = 100
	// quotaEnv 是覆盖默认配额的配置键（单位 MB；<=0 或非数字用默认值）。
	quotaEnv = "BEACONX_WORKSPACE_QUOTA_MB"
)

// 语义化错误：HTTP 层据此映射 400/404/409/413。
var (
	// ErrInvalidPath 表示路径非法（绝对路径、越界、含 NUL、符号链接逃逸等）。
	ErrInvalidPath = errors.New("路径不合法")
	// ErrNotFound 表示目标文件/目录不存在。
	ErrNotFound = errors.New("文件或目录不存在")
	// ErrIsDir 表示目标是目录而不是文件。
	ErrIsDir = errors.New("目标是一个目录")
	// ErrNotDir 表示目标不是目录。
	ErrNotDir = errors.New("目标不是目录")
	// ErrBinary 表示内容不是 UTF-8 文本（含 NUL 或非法编码）。
	ErrBinary = errors.New("文件不是 UTF-8 文本")
	// ErrTooLarge 表示内容超过单文件大小上限。
	ErrTooLarge = errors.New("内容超过大小上限")
	// ErrDirNotEmpty 表示删除非空目录但没有 recursive=true。
	ErrDirNotEmpty = errors.New("目录非空")
	// ErrQuota 表示写入会让任务工作区超过总大小配额。
	ErrQuota = errors.New("工作区用量超过配额")
)

// Entry 是目录列表中的一项（路径相对任务根目录，使用 / 分隔）。
type Entry struct {
	Path       string    `json:"path"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	IsDir      bool      `json:"is_dir"`
}

// Service 是某个状态目录下的工作区服务，可并发使用。
type Service struct {
	stateDir string
	// lockMu 保护 locks；locks 按 tenant/mission 提供「同一任务串行写入」的
	// 互斥锁，用于把「统计配额 + 写入」做成临界区，避免并发写超卖。
	lockMu sync.Mutex
	locks  map[string]*sync.Mutex
}

// New 创建服务并确保状态目录存在（0700）。
func New(stateDir string) (*Service, error) {
	dir := strings.TrimSpace(stateDir)
	if dir == "" {
		return nil, errors.New("状态目录不能为空")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("解析状态目录失败: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("创建状态目录失败: %w", err)
	}
	return &Service{stateDir: abs, locks: map[string]*sync.Mutex{}}, nil
}

// DefaultStateDir 解析默认状态目录（BEACONX_HOME > config.state.dir > ./data）。
//
// HTTP 层每次请求解析（而不是启动时缓存），这样集成测试可以 t.Setenv 注入
// 临时目录，生产也只付一次 stat 级开销。
func DefaultStateDir() string {
	if v := strings.TrimSpace(os.Getenv("BEACONX_HOME")); v != "" {
		return v
	}
	if cfg, err := config.Load(); err == nil && strings.TrimSpace(cfg.State.Dir) != "" {
		return cfg.State.Dir
	}
	return "data"
}

// QuotaBytes 返回每任务工作区的总大小上限（字节）。
//
// 配置键 BEACONX_WORKSPACE_QUOTA_MB：<=0、空或非数字时使用 DefaultQuotaMB。
// 每次调用重新读取，便于测试用 t.Setenv 注入小配额。
func (s *Service) QuotaBytes() int64 {
	raw := strings.TrimSpace(os.Getenv(quotaEnv))
	if raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb > 0 {
			return mb << 20
		}
	}
	return DefaultQuotaMB << 20
}

// Usage 统计任务工作区的已用字节数（只累加普通文件，跳过符号链接与目录）。
//
// 目录不存在视为 0；统计失败返回错误（调用方决定是否降级展示）。
func (s *Service) Usage(tenantID, missionID int64) (int64, error) {
	if tenantID <= 0 || missionID <= 0 {
		return 0, fmt.Errorf("%w: 租户或任务标识非法", ErrInvalidPath)
	}
	root := s.MissionRoot(tenantID, missionID)
	if info, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err == nil && !info.IsDir() {
		return 0, fmt.Errorf("%w: 任务工作区不是目录", ErrInvalidPath)
	}
	var used int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil // 统计期间消失的文件：跳过
		}
		used += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("统计工作区用量失败: %w", err)
	}
	return used, nil
}

// missionLock 返回某任务工作区的互斥锁（不存在则创建）。
func (s *Service) missionLock(tenantID, missionID int64) *sync.Mutex {
	key := strconv.FormatInt(tenantID, 10) + "/" + strconv.FormatInt(missionID, 10)
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	lock := s.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		s.locks[key] = lock
	}
	return lock
}

// checkQuotaLocked 在持有任务锁时校验「写入后」是否超配额。
//
// newBytes 是本次写入的字节数；replacedBytes 是被覆盖文件的原有大小（新建为 0），
// 这样覆盖写不会因为旧文件也被计入而误判。
func (s *Service) checkQuotaLocked(tenantID, missionID, newBytes, replacedBytes int64) error {
	used, err := s.Usage(tenantID, missionID)
	if err != nil {
		return err
	}
	quota := s.QuotaBytes()
	projected := used - replacedBytes + newBytes
	if projected > quota {
		return fmt.Errorf("%w: 写入后约 %d 字节，超过任务上限 %d 字节", ErrQuota, projected, quota)
	}
	return nil
}

// MissionRoot 返回任务工作区根目录（不创建）。
func (s *Service) MissionRoot(tenantID, missionID int64) string {
	return filepath.Join(s.stateDir, "tenants", strconv.FormatInt(tenantID, 10),
		"workspace", "missions", strconv.FormatInt(missionID, 10))
}

// RootLabel 返回对外展示的相对根标签，例如 "missions/7"。
func RootLabel(missionID int64) string {
	return "missions/" + strconv.FormatInt(missionID, 10)
}

// List 递归列出任务工作区下 relDir 的条目（最多 MaxListEntries 项）。
//
// 路径不存在返回 ErrNotFound；返回条目按 path 字典序，目录在前一条的普通项
// 也带 is_dir=true。符号链接项直接跳过（不跟随，避免越界读取）。
func (s *Service) List(tenantID, missionID int64, relDir string) ([]Entry, error) {
	root, abs, err := s.resolve(tenantID, missionID, relDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: 目录不存在", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("读取目录失败: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: 列表目标不是目录", ErrNotDir)
	}

	out := []Entry{}
	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == abs {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// 符号链接不跟随、不展示：它可能是跨任务/跨租户的逃逸入口。
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		entry := Entry{Path: filepath.ToSlash(rel), IsDir: d.IsDir()}
		if info, infoErr := d.Info(); infoErr == nil {
			entry.ModifiedAt = info.ModTime().UTC()
			if !info.IsDir() {
				entry.Size = info.Size()
			}
		}
		out = append(out, entry)
		if len(out) >= MaxListEntries {
			return errListLimit
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errListLimit) {
		return nil, fmt.Errorf("遍历目录失败: %w", walkErr)
	}
	// WalkDir 本身按字典序，但保险起见再排序一次，保证不同文件系统的输出稳定。
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// errListLimit 用于在 WalkDir 中提前终止（不是对外错误）。
var errListLimit = errors.New("列表条目达到上限")

// Read 读取任务工作区内的 UTF-8 文本文件。
func (s *Service) Read(tenantID, missionID int64, relFile string) ([]byte, error) {
	_, abs, err := s.resolve(tenantID, missionID, relFile)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: 文件不存在", ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: 目标是目录", ErrIsDir)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: 不允许读取符号链接", ErrInvalidPath)
	}
	if info.Size() > MaxReadBytes {
		return nil, fmt.Errorf("%w: 文件超过 %d 字节", ErrTooLarge, MaxReadBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	if err := validateText(data); err != nil {
		return nil, err
	}
	return data, nil
}

// Write 写入 UTF-8 文本文件（自动创建父目录），返回写入后的条目。
func (s *Service) Write(tenantID, missionID int64, relFile string, data []byte) (Entry, error) {
	if int64(len(data)) > MaxFileBytes {
		return Entry{}, fmt.Errorf("%w: 单文件最多 %d 字节", ErrTooLarge, MaxFileBytes)
	}
	if err := validateText(data); err != nil {
		return Entry{}, err
	}
	_, abs, err := s.resolve(tenantID, missionID, relFile)
	if err != nil {
		return Entry{}, err
	}
	// 配额是「统计 + 写入」的复合操作：同一任务的写串行化，避免并发超卖。
	lock := s.missionLock(tenantID, missionID)
	lock.Lock()
	defer lock.Unlock()
	var replacedBytes int64
	if info, statErr := os.Lstat(abs); statErr == nil {
		if info.IsDir() {
			return Entry{}, fmt.Errorf("%w: 目标是目录", ErrIsDir)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return Entry{}, fmt.Errorf("%w: 不允许写入符号链接", ErrInvalidPath)
		}
		replacedBytes = info.Size()
	}
	if err := s.checkQuotaLocked(tenantID, missionID, int64(len(data)), replacedBytes); err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Entry{}, fmt.Errorf("创建父目录失败: %w", err)
	}
	// MkdirAll 之后重新校验一次，防止父目录在这一瞬间被替换成指向外部的符号链接。
	if err := s.verifyInside(tenantID, missionID, abs); err != nil {
		return Entry{}, err
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return Entry{}, fmt.Errorf("写入文件失败: %w", err)
	}
	return s.entryFor(tenantID, missionID, relFile)
}

// Upload 把上传流写入任务工作区的 dir 目录，文件名取 base(filename)。
//
// filename 只保留基础名（去掉客户端可能带上的路径），size 是已读取的字节数
// （调用方应先按 MaxUploadBytes 限制读取长度）。
func (s *Service) Upload(tenantID, missionID int64, dir, filename string, r io.Reader, size int64) (Entry, error) {
	base := filepath.Base(strings.TrimSpace(filename))
	if base == "" || base == "." || base == ".." || strings.ContainsRune(base, 0) {
		return Entry{}, fmt.Errorf("%w: 文件名不合法", ErrInvalidPath)
	}
	if size > MaxUploadBytes {
		return Entry{}, fmt.Errorf("%w: 上传文件最多 %d 字节", ErrTooLarge, MaxUploadBytes)
	}
	rel := base
	if strings.TrimSpace(dir) != "" {
		cleanDir, err := cleanRel(dir)
		if err != nil {
			return Entry{}, err
		}
		if cleanDir == "." {
			rel = base
		} else {
			rel = filepath.Join(cleanDir, base)
		}
	}
	_, abs, err := s.resolve(tenantID, missionID, rel)
	if err != nil {
		return Entry{}, err
	}
	lock := s.missionLock(tenantID, missionID)
	lock.Lock()
	defer lock.Unlock()
	var replacedBytes int64
	if info, statErr := os.Lstat(abs); statErr == nil && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		replacedBytes = info.Size()
	}
	// 上传大小未知（size<0）时按单文件上限保守估计，宁可多拒不可超卖。
	projected := size
	if projected < 0 {
		projected = MaxUploadBytes
	}
	if err := s.checkQuotaLocked(tenantID, missionID, projected, replacedBytes); err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Entry{}, fmt.Errorf("创建上传目录失败: %w", err)
	}
	if err := s.verifyInside(tenantID, missionID, abs); err != nil {
		return Entry{}, err
	}
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return Entry{}, fmt.Errorf("创建上传文件失败: %w", err)
	}
	written, copyErr := io.Copy(file, io.LimitReader(r, MaxUploadBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return Entry{}, fmt.Errorf("写入上传文件失败: %w", copyErr)
	}
	if closeErr != nil {
		return Entry{}, fmt.Errorf("关闭上传文件失败: %w", closeErr)
	}
	if written > MaxUploadBytes {
		_ = os.Remove(abs)
		return Entry{}, fmt.Errorf("%w: 上传文件最多 %d 字节", ErrTooLarge, MaxUploadBytes)
	}
	// size 可能由调用方低估或为未知：按实际写入量再核对一次配额。
	if written != replacedBytes {
		if quotaErr := s.checkQuotaLocked(tenantID, missionID, written, replacedBytes); quotaErr != nil {
			_ = os.Remove(abs)
			return Entry{}, quotaErr
		}
	}
	// 注意：上传允许任意二进制内容（脚本、字典、工具包等），只有「文本写入」
	// 与「文本读取」要求 UTF-8；读取二进制文件会返回 ErrBinary。
	return s.entryFor(tenantID, missionID, rel)
}

// Delete 删除文件或目录；目录默认必须为空，recursive=true 时递归删除。
func (s *Service) Delete(tenantID, missionID int64, rel string, recursive bool) error {
	clean, err := cleanRel(rel)
	if err != nil {
		return err
	}
	if clean == "." {
		return fmt.Errorf("%w: 不能删除任务工作区根目录", ErrInvalidPath)
	}
	_, abs, err := s.resolve(tenantID, missionID, clean)
	if err != nil {
		return err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: 文件或目录不存在", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("读取目标失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: 不允许删除符号链接", ErrInvalidPath)
	}
	if info.IsDir() {
		if recursive {
			if err := os.RemoveAll(abs); err != nil {
				return fmt.Errorf("递归删除目录失败: %w", err)
			}
			return nil
		}
		children, readErr := os.ReadDir(abs)
		if readErr != nil {
			return fmt.Errorf("读取目录失败: %w", readErr)
		}
		if len(children) > 0 {
			return fmt.Errorf("%w: 目录非空，需 recursive=true", ErrDirNotEmpty)
		}
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("删除目录失败: %w", err)
		}
		return nil
	}
	if err := os.Remove(abs); err != nil {
		return fmt.Errorf("删除文件失败: %w", err)
	}
	return nil
}

// resolve 校验相对路径并返回（任务根目录, 绝对路径）。
// 返回的根目录已确保存在（mkdir 0700）。
func (s *Service) resolve(tenantID, missionID int64, rel string) (root, abs string, err error) {
	if tenantID <= 0 || missionID <= 0 {
		return "", "", fmt.Errorf("%w: 租户或任务标识非法", ErrInvalidPath)
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return "", "", err
	}
	root = s.MissionRoot(tenantID, missionID)
	if mkErr := os.MkdirAll(root, 0o700); mkErr != nil {
		return "", "", fmt.Errorf("创建任务工作区失败: %w", mkErr)
	}
	abs = filepath.Join(root, clean)
	if !within(root, abs) {
		return "", "", fmt.Errorf("%w: 路径越界", ErrInvalidPath)
	}
	if err := s.verifyInside(tenantID, missionID, abs); err != nil {
		return "", "", err
	}
	return root, abs, nil
}

// verifyInside 解析 abs 已存在的最深祖先的真实路径，确认仍在任务根目录内。
//
// 根目录自身允许是符号链接（macOS 的 /var → /private/var 等）：以 EvalSymlinks
// 解析后的真实根为基准比较。
func (s *Service) verifyInside(tenantID, missionID int64, abs string) error {
	root := s.MissionRoot(tenantID, missionID)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		// 根目录刚创建过，解析失败说明环境异常；保守拒绝。
		return fmt.Errorf("%w: 无法解析任务根目录", ErrInvalidPath)
	}
	probe := abs
	for {
		if _, statErr := os.Lstat(probe); statErr == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("%w: 路径无法解析", ErrInvalidPath)
		}
		probe = parent
	}
	real, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return fmt.Errorf("%w: 路径无法解析", ErrInvalidPath)
	}
	if !within(realRoot, real) {
		return fmt.Errorf("%w: 路径通过符号链接越界", ErrInvalidPath)
	}
	return nil
}

// entryFor 读取写入后的文件信息。
func (s *Service) entryFor(tenantID, missionID int64, rel string) (Entry, error) {
	root, abs, err := s.resolve(tenantID, missionID, rel)
	if err != nil {
		return Entry{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Entry{}, fmt.Errorf("读取写入结果失败: %w", err)
	}
	relPath, err := filepath.Rel(root, abs)
	if err != nil {
		return Entry{}, fmt.Errorf("计算相对路径失败: %w", err)
	}
	entry := Entry{
		Path:       filepath.ToSlash(relPath),
		ModifiedAt: info.ModTime().UTC(),
		IsDir:      info.IsDir(),
	}
	if !info.IsDir() {
		entry.Size = info.Size()
	}
	return entry, nil
}

// cleanRel 校验相对路径：非绝对、无盘符、无 NUL、Clean 后不允许以 ".." 开头。
func cleanRel(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: 路径包含 NUL 字节", ErrInvalidPath)
	}
	// URL 解码后可能出现反斜杠；Windows 风格分隔符统一按分隔符处理。
	rel = strings.ReplaceAll(rel, "\\", "/")
	if rel == "" {
		return ".", nil
	}
	if path.IsAbs(rel) || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("%w: 不允许绝对路径", ErrInvalidPath)
	}
	cleaned := path.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: 路径不能越过任务工作区", ErrInvalidPath)
	}
	return cleaned, nil
}

// within 报告 target 是否等于 root 或位于 root 之下（均为已清理的绝对路径）。
func within(root, target string) bool {
	if target == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(target, prefix)
}

// validateText 校验内容是 UTF-8 文本且不含 NUL（二进制拒绝）。
func validateText(data []byte) error {
	if strings.IndexByte(string(data), 0) >= 0 {
		return fmt.Errorf("%w: 内容包含 NUL 字节", ErrBinary)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: 内容不是合法 UTF-8", ErrBinary)
	}
	return nil
}
