package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5-2：技能（Skills）管理接口（P-B5-08..17）。
//
// 存储模型：
//   - 元数据在数据库表 skills（迁移 0011）；
//   - 正文在租户状态目录 <state>/skills/<tenant_id>/<name>/。
//     状态目录默认是 <BEACONX_HOME>/skills（BEACONX_HOME 缺省 data），可用
//     BEACONX_SKILLS_DIR 覆盖（测试与自定义部署用）。按租户分子目录是为了让
//     「同名技能跨租户」在磁盘上也互不可见，而不是只靠数据库行隔离。
//
// 安全约束（都会在错误里给出中文说明）：
//   - 技能名必须匹配 ^[a-z0-9][a-z0-9_-]{0,63}$（与 pg.ValidateSkillName 一致）；
//   - 所有文件路径先做「绝对路径 / .. / 反斜杠 / NUL」校验，再拼接并复核前缀，
//     最后解析父目录符号链接，确保写入不逃出技能目录；
//   - zip 上传：总解压体积 ≤10MiB、文件数 ≤200、拒绝符号链接/目录穿越/绝对路径，
//     只接受文本扩展名（md/txt/yaml/yml/json/py/sh/js/ts）；
//   - 单文件读取/写入上限 1MiB。
const (
	// skillMaxZipBytes 是上传 zip 的大小上限（压缩后）。
	skillMaxZipBytes int64 = 10 << 20
	// skillMaxExtractBytes 是解压后总字节上限。
	skillMaxExtractBytes int64 = 10 << 20
	// skillMaxArchiveFiles 是 zip 内文件数上限。
	skillMaxArchiveFiles = 200
	// skillMaxTextBytes 是单文件读写上限。
	skillMaxTextBytes int64 = 1 << 20
	// skillDefaultVersion 与 pg.DefaultSkillVersion 保持一致。
	skillDefaultVersion = "0.1.0"
	// skillRootEnv / skillHomeEnv 决定技能根目录。
	skillRootEnv     = "BEACONX_SKILLS_DIR"
	skillHomeEnv     = "BEACONX_HOME"
	skillHomeDefault = "data"
)

// 版本历史（最小实现）：快照落在技能目录内的 .versions/<timestamp>/。
//
//   - 每次 PUT 文件、每次上传覆盖前都会先给当前技能目录打一份快照；
//   - 只保留最近 skillKeepVersions() 份（默认 5，可用 BEACONX_SKILLS_KEEP_VERSIONS
//     配置为 1..50），更旧的自动清理；
//   - .versions 目录自身不参与文件列表与文件数统计，避免污染用户视图；
//   - timestamp 用 UTC 固定宽度格式（无分隔符），既可排序也可安全用于路径。
const (
	// skillVersionsDirName 是版本快照在技能目录内的子目录名。
	skillVersionsDirName = ".versions"
	// skillDefaultKeepVersions 是版本保留份数的默认值。
	skillDefaultKeepVersions = 5
	// skillMinKeepVersions / skillMaxKeepVersionsLimit 是允许的配置区间；
	// 超出区间或无法解析时回退默认值（fail-safe，不让配置错误变成无限保留）。
	skillMinKeepVersions      = 1
	skillMaxKeepVersionsLimit = 50
	// skillKeepVersionsEnv 是版本保留份数的配置键（每次裁剪时读取）。
	skillKeepVersionsEnv = "BEACONX_SKILLS_KEEP_VERSIONS"
	// skillVersionMetaFileName 是版本目录内保存元数据的文件名。
	skillVersionMetaFileName = "meta.json"
	// skillVersionTimestampFormat 是版本目录名格式（UTC，精确到纳秒）。
	// 注意：Go 的纳秒占位必须跟在「秒 + 小数点」后面，否则 000 是字面量，
	// 会让同一秒内的多次快照重名并互相覆盖。
	skillVersionTimestampFormat = "20060102T150405.000000000"
)

// skillVersionTimestampPattern 校验版本目录名/URL 参数，防止路径穿越。
var skillVersionTimestampPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}$`)

// skillKeepVersions 返回版本保留份数：环境变量 BEACONX_SKILLS_KEEP_VERSIONS，
// 默认 5，只接受 1..50；空/非法/越界一律回退默认值。
//
// 每次裁剪都重新读取，便于测试用 t.Setenv 注入；生产上改配置无需重启。
func skillKeepVersions() int {
	raw := strings.TrimSpace(os.Getenv(skillKeepVersionsEnv))
	if raw == "" {
		return skillDefaultKeepVersions
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < skillMinKeepVersions || value > skillMaxKeepVersionsLimit {
		return skillDefaultKeepVersions
	}
	return value
}

// skillAllowedExtensions 是可读写的文本扩展名（上传与 PUT 都校验）。
var skillAllowedExtensions = map[string]bool{
	".md": true, ".txt": true, ".yaml": true, ".yml": true, ".json": true,
	".py": true, ".sh": true, ".js": true, ".ts": true,
}

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/skills",
			Summary: "技能列表（含缺失统计）",
			Build:   func(o Options) http.HandlerFunc { return o.handleListSkills },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/skills",
			Summary: "新建技能",
			Build:   func(o Options) http.HandlerFunc { return o.handleCreateSkill },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/skills/{name}",
			Summary: "技能详情",
			Build:   func(o Options) http.HandlerFunc { return o.handleGetSkill },
		},
		FeatureRoute{
			Method: http.MethodPatch, Pattern: "/api/v1/skills/{name}",
			Summary: "更新技能元数据（description/version）",
			Build:   func(o Options) http.HandlerFunc { return o.handleUpdateSkill },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/skills/{name}",
			Summary: "删除技能",
			Build:   func(o Options) http.HandlerFunc { return o.handleDeleteSkill },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/skills/upload",
			Summary: "上传技能 zip（multipart）",
			Build:   func(o Options) http.HandlerFunc { return o.handleUploadSkill },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/skills/{name}/files",
			Summary: "技能文件列表",
			Build:   func(o Options) http.HandlerFunc { return o.handleListSkillFiles },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/skills/{name}/files/{path...}",
			Summary: "读取技能文本文件",
			Build:   func(o Options) http.HandlerFunc { return o.handleReadSkillFile },
		},
		FeatureRoute{
			Method: http.MethodPut, Pattern: "/api/v1/skills/{name}/files/{path...}",
			Summary: "写入技能文本文件",
			Build:   func(o Options) http.HandlerFunc { return o.handleWriteSkillFile },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/skills/{name}/files/{path...}",
			Summary: "删除技能文件",
			Build:   func(o Options) http.HandlerFunc { return o.handleDeleteSkillFile },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/skills/{name}/versions",
			Summary: "技能版本历史",
			Build:   func(o Options) http.HandlerFunc { return o.handleListSkillVersions },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/skills/{name}/versions/{timestamp}/restore",
			Summary: "恢复技能到指定版本",
			Build:   func(o Options) http.HandlerFunc { return o.handleRestoreSkillVersion },
		},
	)
}

// skillFileEntry 是文件列表的一项（扁平数组）。
type skillFileEntry struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	UpdatedAt time.Time `json:"updated_at"`
}

// skillVersionView 是一条版本快照的展示项。
type skillVersionView struct {
	Timestamp string `json:"timestamp"`
	Files     int    `json:"files"`
	Size      int64  `json:"size"`
	// HasMeta 表示该快照是否带 meta.json（B7 之前的旧版本没有，restore 时
	// 只回滚文件内容、不回滚元数据；前端可据此提示用户）。
	HasMeta bool `json:"has_meta"`
}

// skillVersionMeta 是版本快照里保存的技能元数据（用于 restore 时回滚 DB）。
type skillVersionMeta struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	SavedAt     time.Time `json:"saved_at"`
}

// skillVersionMetaFrom 把当前技能行转成快照元数据。
func skillVersionMetaFrom(skill pg.Skill) skillVersionMeta {
	return skillVersionMeta{
		Name:        skill.Name,
		Description: skill.Description,
		Version:     skill.Version,
		SavedAt:     time.Now().UTC(),
	}
}

// skillVersionMetaFor 在「目录存在但数据库行缺失」等场景下兜底构造元数据。
func skillVersionMetaFor(name string, skill pg.Skill, exists bool) skillVersionMeta {
	meta := skillVersionMetaFrom(skill)
	if !exists || strings.TrimSpace(meta.Name) == "" {
		meta.Name = name
	}
	return meta
}

// writeSkillVersionMeta 把元数据写进版本目录；失败视为快照失败。
func writeSkillVersionMeta(versionDir string, meta skillVersionMeta) error {
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("编码版本元数据失败：%w", err)
	}
	path := filepath.Join(versionDir, skillVersionMetaFileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("写入版本元数据失败：%w", err)
	}
	return nil
}

// readSkillVersionMeta 读取版本目录里的 meta.json。
//
// 返回 found=false 表示这是「旧版本」（B7 之前打的快照），调用方只回滚文件并
// 给出 warning，不修改数据库元数据。
func readSkillVersionMeta(versionDir string) (meta skillVersionMeta, found bool, err error) {
	path := filepath.Join(versionDir, skillVersionMetaFileName)
	data, readErr := os.ReadFile(path)
	if errors.Is(readErr, os.ErrNotExist) {
		return skillVersionMeta{}, false, nil
	}
	if readErr != nil {
		return skillVersionMeta{}, false, fmt.Errorf("读取版本元数据失败：%w", readErr)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return skillVersionMeta{}, false, fmt.Errorf("解析版本元数据失败：%w", err)
	}
	return meta, true, nil
}

// skillView 是对外技能视图；files 是磁盘实际文件数，missing 是
// 「元数据记录的文件数 − 磁盘实际文件数」（≥0），用于提示文件被外部删除。
type skillView struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Version     string    `json:"version"`
	Files       int       `json:"files"`
	FileCount   int       `json:"file_count"`
	Missing     int       `json:"missing"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// skillRestoreResponse 是 restore 接口的响应：技能对象 + 可选 warning。
//
// warning 只在「旧版本快照缺少 meta.json、仅回滚文件」时出现，前端可提示用户
// 元数据未回滚。
type skillRestoreResponse struct {
	skillView
	Warning string `json:"warning,omitempty"`
}

// skillsRootDir 解析技能根目录。
func skillsRootDir() string {
	if dir := strings.TrimSpace(os.Getenv(skillRootEnv)); dir != "" {
		return dir
	}
	home := strings.TrimSpace(os.Getenv(skillHomeEnv))
	if home == "" {
		home = skillHomeDefault
	}
	return filepath.Join(home, "skills")
}

// skillTenantDir 返回某租户的技能根目录（不含技能名）。
func skillTenantDir(tenantID int64) string {
	return filepath.Join(skillsRootDir(), strconv.FormatInt(tenantID, 10))
}

// skillDirFor 返回某技能的目录。
func skillDirFor(tenantID int64, name string) string {
	return filepath.Join(skillTenantDir(tenantID), name)
}

// skillPathName 解析并校验 /skills/{name}。
func (o Options) skillPathName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := strings.TrimSpace(r.PathValue("name"))
	if err := pg.ValidateSkillName(name); err != nil {
		writeStoreError(w, r, err)
		return "", false
	}
	return name, true
}

// skillPathFromRequest 解析 /files/{path...} 并做安全校验。
//
// 版本目录 .versions 属于内部历史，不允许经文件接口直接读写/删除，避免绕过
// 版本接口改坏快照。
func skillPathFromRequest(r *http.Request) (string, error) {
	raw := r.PathValue("path")
	rel, err := sanitizeSkillRelPath(raw)
	if err != nil {
		return "", err
	}
	segments := strings.Split(filepath.ToSlash(rel), "/")
	if len(segments) > 0 && segments[0] == skillVersionsDirName {
		return "", fmt.Errorf("%w：%s 目录属于版本历史，不能通过文件接口访问", pg.ErrInvalidInput, skillVersionsDirName)
	}
	return rel, nil
}

// sanitizeSkillRelPath 校验并规范化 zip 条目名/请求路径。
//
// 拒绝：空、绝对路径、反斜杠、NUL、任何 "." 或 ".." 片段；其余按 / 拆段后
// 重新拼接（避免 Windows 与 Unix 分隔符差异）。
func sanitizeSkillRelPath(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return "", fmt.Errorf("%w：文件路径不能为空", pg.ErrInvalidInput)
	}
	if strings.ContainsRune(text, 0) {
		return "", fmt.Errorf("%w：文件路径包含非法字符", pg.ErrInvalidInput)
	}
	if strings.Contains(text, `\`) {
		return "", fmt.Errorf("%w：文件路径不允许反斜杠", pg.ErrInvalidInput)
	}
	if strings.HasPrefix(text, "/") || strings.HasPrefix(text, "\\") {
		return "", fmt.Errorf("%w：不允许绝对路径", pg.ErrInvalidInput)
	}
	segments := strings.Split(text, "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch segment {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w：文件路径不允许包含 ..", pg.ErrInvalidInput)
		default:
			cleaned = append(cleaned, segment)
		}
	}
	if len(cleaned) == 0 {
		return "", fmt.Errorf("%w：文件路径不能为空", pg.ErrInvalidInput)
	}
	return filepath.Join(cleaned...), nil
}

// skillSafeJoin 把相对路径拼到技能目录下，并复核前缀与父目录符号链接。
func skillSafeJoin(baseDir, rel string) (string, error) {
	full := filepath.Join(baseDir, rel)
	baseClean := filepath.Clean(baseDir)
	fullClean := filepath.Clean(full)
	if fullClean != baseClean && !strings.HasPrefix(fullClean, baseClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w：文件路径越界", pg.ErrInvalidInput)
	}
	// 父目录若已存在，解析符号链接后再复核一次前缀，防止借已存在的链接逃逸。
	parent := filepath.Dir(fullClean)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		resolvedBase, baseErr := filepath.EvalSymlinks(baseClean)
		if baseErr == nil {
			if resolved != resolvedBase && !strings.HasPrefix(resolved, resolvedBase+string(os.PathSeparator)) {
				return "", fmt.Errorf("%w：文件路径越界", pg.ErrInvalidInput)
			}
		}
	}
	return fullClean, nil
}

// skillRejectSymlink 拒绝把「已存在的符号链接」当作普通文件读写。
//
// 技能目录内的文件只应由本接口/zip 解压创建（解压已拒绝符号链接）；这里再挡
// 一次是因为有人可能在磁盘上手工放置链接，Linux 的 os.WriteFile 会跟随链接写到
// 目标位置，从而绕过目录边界。
func skillRejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return nil // 不存在：由调用方按「新建」处理
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w：目标路径是符号链接，已拒绝", pg.ErrInvalidInput)
	}
	return nil
}

// skillExtensionAllowed 判断文件扩展名是否在白名单内。
func skillExtensionAllowed(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return skillAllowedExtensions[ext]
}

// countSkillFiles 统计技能目录下的普通文件数（不含目录）。
func countSkillFiles(dir string) int {
	entries, err := listSkillFiles(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// listSkillFiles 返回目录下所有普通文件（按路径排序）；目录不存在时返回空列表。
func listSkillFiles(dir string) ([]skillFileEntry, error) {
	out := make([]skillFileEntry, 0, 8)
	err := filepath.WalkDir(dir, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			// 版本快照不是技能内容：既不展示也不计数。
			if entry.Name() == skillVersionsDirName {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, current)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return nil // 文件在遍历期间消失：跳过而不是整体失败
		}
		out = append(out, skillFileEntry{
			Path:      filepath.ToSlash(rel),
			Size:      info.Size(),
			UpdatedAt: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// skillVersionsRoot 返回技能目录内的版本根目录。
func skillVersionsRoot(skillDir string) string {
	return filepath.Join(skillDir, skillVersionsDirName)
}

// copySkillTree 把 src 内容复制到 dst（自动建父目录），跳过 .versions 与符号链接。
//
// 返回复制的文件数与字节数；src 不存在时返回 ErrNotFound 语义由调用方处理。
func copySkillTree(src, dst string) (int, int64, error) {
	return copySkillTreeSkipping(src, dst, nil)
}

// copySkillContent 复制版本快照的「内容部分」：跳过 .versions、符号链接以及
// 根级 meta.json（meta 由调用方单独恢复，不能写回技能目录）。
func copySkillContent(src, dst string) (int, int64, error) {
	return copySkillTreeSkipping(src, dst, func(rel string) bool {
		return rel == skillVersionMetaFileName
	})
}

// copySkillTreeSkipping 是 copySkillTree 的实现：skip 返回 true 的根级相对路径
// 会被跳过（只支持根级精确匹配，足够覆盖 meta.json 的场景）。
func copySkillTreeSkipping(src, dst string, skip func(rel string) bool) (int, int64, error) {
	files := 0
	var size int64
	err := filepath.WalkDir(src, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == src {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == skillVersionsDirName {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(src, current)
			if err != nil {
				return err
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil // 快照不复制符号链接，避免把逃逸入口带进新版本
		}
		rel, err := filepath.Rel(src, current)
		if err != nil {
			return err
		}
		if skip != nil && skip(rel) {
			return nil
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		written, err := copySkillFile(current, target)
		if err != nil {
			return err
		}
		files++
		size += written
		return nil
	})
	return files, size, err
}

// copySkillFile 以流式方式复制单个文件（避免把大文件一次性读进内存）。
func copySkillFile(src, dst string) (int64, error) {
	source, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return 0, err
	}
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return written, copyErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	if written != info.Size() {
		return written, fmt.Errorf("复制文件不完整：%s", filepath.Base(src))
	}
	return written, nil
}

// snapshotSkillVersion 给技能目录打一份快照（内部调用；prune 控制是否清理旧版本）。
//
// meta 会写入快照目录的 meta.json，供 restore 时同时回滚数据库元数据；目录不存在
// 时返回空时间戳且不报错（新建技能还没内容可快照）。
func snapshotSkillVersion(tenantID int64, name string, meta skillVersionMeta, prune bool) (string, error) {
	dir := skillDirFor(tenantID, name)
	if !dirExists(dir) {
		return "", nil
	}
	root := skillVersionsRoot(dir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("创建版本目录失败：%w", err)
	}
	// 时间戳精确到纳秒；极端情况下同纳秒重复时顺延，保证目录唯一。
	var stamp string
	var dst string
	for attempt := 0; attempt < 100; attempt++ {
		stamp = time.Now().UTC().Format(skillVersionTimestampFormat)
		dst = filepath.Join(root, stamp)
		if !dirExists(dst) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", fmt.Errorf("创建版本快照失败：%w", err)
	}
	if _, _, err := copySkillTree(dir, dst); err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("写入版本快照失败：%w", err)
	}
	if err := writeSkillVersionMeta(dst, meta); err != nil {
		_ = os.RemoveAll(dst)
		return "", err
	}
	if prune {
		pruneSkillVersions(tenantID, name)
	}
	return stamp, nil
}

// pruneSkillVersions 只保留最近 skillKeepVersions() 份快照（按时间戳升序删最旧）。
func pruneSkillVersions(tenantID int64, name string) {
	root := skillVersionsRoot(skillDirFor(tenantID, name))
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	stamps := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && skillVersionTimestampPattern.MatchString(entry.Name()) {
			stamps = append(stamps, entry.Name())
		}
	}
	sort.Strings(stamps)
	keep := skillKeepVersions()
	for len(stamps) > keep {
		_ = os.RemoveAll(filepath.Join(root, stamps[0]))
		stamps = stamps[1:]
	}
}

// listSkillVersions 返回版本快照（时间戳降序）。
func listSkillVersions(tenantID int64, name string) ([]skillVersionView, error) {
	root := skillVersionsRoot(skillDirFor(tenantID, name))
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []skillVersionView{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]skillVersionView, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !skillVersionTimestampPattern.MatchString(entry.Name()) {
			continue
		}
		versionDir := filepath.Join(root, entry.Name())
		files, size := countVersionFiles(versionDir)
		out = append(out, skillVersionView{
			Timestamp: entry.Name(),
			Files:     files,
			Size:      size,
			HasMeta:   fileExists(filepath.Join(versionDir, skillVersionMetaFileName)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	return out, nil
}

// countVersionFiles 统计快照目录下的文件数与总字节数。
func countVersionFiles(dir string) (int, int64) {
	files := 0
	var size int64
	_ = filepath.WalkDir(dir, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(dir, current)
		if relErr == nil && rel == skillVersionMetaFileName {
			return nil // meta.json 不是技能内容，不计入版本文件数与体积
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files++
		size += info.Size()
		return nil
	})
	return files, size
}

// restoreSkillVersion 把技能恢复成某个快照。
//
// 顺序：先给当前状态打快照（恢复本身可回滚）→ 清空除 .versions 外的内容 →
// 把目标快照复制回来 → 清理旧版本。目标版本在快照之后复制，避免被 prune 删掉。
//
// 返回值：版本快照里的元数据（旧版本没有 meta.json 时 found=false，此时只回滚
// 文件内容，调用方应给出 warning）。数据库写入由 HTTP 层完成，保持本函数只做
// 文件系统操作。
func restoreSkillVersion(tenantID int64, name, stamp string, currentMeta skillVersionMeta) (skillVersionMeta, bool, error) {
	dir := skillDirFor(tenantID, name)
	source := filepath.Join(skillVersionsRoot(dir), stamp)
	if !dirExists(source) {
		return skillVersionMeta{}, false, fmt.Errorf("%w：版本 %q 不存在", pg.ErrNotFound, stamp)
	}
	versionMeta, hasMeta, err := readSkillVersionMeta(source)
	if err != nil {
		return skillVersionMeta{}, false, err
	}
	// 先把目标版本复制到临时目录：后续快照可能触发 prune，不能直接引用原路径；
	// copySkillContent 会跳过 meta.json，避免它被写回技能目录。
	tmpRoot := filepath.Join(skillTenantDir(tenantID), "."+name+".restore."+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return skillVersionMeta{}, false, fmt.Errorf("创建恢复临时目录失败：%w", err)
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()
	if _, _, err := copySkillContent(source, tmpRoot); err != nil {
		return skillVersionMeta{}, false, fmt.Errorf("读取版本快照失败：%w", err)
	}
	// 当前状态先入历史（不 prune，避免把要恢复的那份挤掉），并带上当前元数据，
	// 这样「恢复」本身也可以再被恢复回去。
	if _, err := snapshotSkillVersion(tenantID, name, currentMeta, false); err != nil {
		return skillVersionMeta{}, false, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return skillVersionMeta{}, false, fmt.Errorf("读取技能目录失败：%w", err)
	}
	for _, entry := range entries {
		if entry.Name() == skillVersionsDirName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return skillVersionMeta{}, false, fmt.Errorf("清理当前内容失败：%w", err)
		}
	}
	if _, _, err := copySkillContent(tmpRoot, dir); err != nil {
		return skillVersionMeta{}, false, fmt.Errorf("恢复版本内容失败：%w", err)
	}
	pruneSkillVersions(tenantID, name)
	return versionMeta, hasMeta, nil
}

// snapshotBeforeReplace 在整体替换技能目录（上传覆盖）前打快照。
//
// 返回 carry：把 .versions 目录挪到技能目录之外的暂存路径——随后的
// os.RemoveAll(target) 会把技能目录连同 .versions 一起删掉。替换完成后按
// carryMoveBack 放回新的技能目录。
func snapshotBeforeReplace(tenantID int64, name string, meta skillVersionMeta) (carry string, err error) {
	dir := skillDirFor(tenantID, name)
	if !dirExists(dir) {
		return "", nil
	}
	if _, err := snapshotSkillVersion(tenantID, name, meta, true); err != nil {
		return "", err
	}
	versions := skillVersionsRoot(dir)
	if !dirExists(versions) {
		return "", nil
	}
	carry = filepath.Join(skillTenantDir(tenantID), "."+name+".versions."+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Rename(versions, carry); err != nil {
		return "", fmt.Errorf("暂存版本目录失败：%w", err)
	}
	return carry, nil
}

// skillToView 把元数据行 + 磁盘实际情况投影成视图。
func skillToView(skill pg.Skill) skillView {
	actual := countSkillFiles(skillDirFor(skill.TenantID, skill.Name))
	missing := skill.FileCount - actual
	if missing < 0 {
		missing = 0
	}
	return skillView{
		Name:        skill.Name,
		Description: skill.Description,
		Version:     skill.Version,
		Files:       actual,
		FileCount:   skill.FileCount,
		Missing:     missing,
		CreatedAt:   skill.CreatedAt,
		UpdatedAt:   skill.UpdatedAt,
	}
}

// ensureSkillDir 创建技能目录（权限 0700）。
func ensureSkillDir(tenantID int64, name string) error {
	if err := os.MkdirAll(skillDirFor(tenantID, name), 0o700); err != nil {
		return fmt.Errorf("创建技能目录失败：%w", err)
	}
	return nil
}

// removeSkillDir 删除技能目录（不存在视为成功）。
func removeSkillDir(tenantID int64, name string) error {
	if err := os.RemoveAll(skillDirFor(tenantID, name)); err != nil {
		return fmt.Errorf("删除技能目录失败：%w", err)
	}
	return nil
}

// skillMDContent 生成新建技能时的 SKILL.md（带 YAML front matter）。
func skillMDContent(name, description string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	if description != "" {
		b.WriteString("description: " + description + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString("# " + name + "\n")
	return []byte(b.String())
}

// handleListSkills 列出技能（含缺失统计）。
func (o Options) handleListSkills(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	skills, err := o.Store.ListSkills(r.Context(), o.TenantID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	views := make([]skillView, 0, len(skills))
	for _, skill := range skills {
		views = append(views, skillToView(skill))
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": views})
}

// handleCreateSkill 新建技能：写元数据 + 创建目录 + 生成最小 SKILL.md。
func (o Options) handleCreateSkill(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := pg.ValidateSkillName(name); err != nil {
		writeStoreError(w, r, err)
		return
	}
	description := strings.TrimSpace(req.Description)
	skill, err := o.Store.CreateSkill(r.Context(), o.TenantID, pg.SkillInput{
		Name:        name,
		Description: description,
		Version:     req.Version,
		FileCount:   1, // 下面写入 SKILL.md，因此初始文件数为 1
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	dir := skillDirFor(o.TenantID, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// 元数据已写入但目录失败：回滚元数据，避免出现「有库无目录」的半套状态。
		if delErr := o.Store.DeleteSkill(r.Context(), o.TenantID, name); delErr != nil {
			o.Logger.Error("回滚技能元数据失败", "skill", name, "error", delErr)
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "创建技能目录失败")
		return
	}
	skillPath, err := skillSafeJoin(dir, "SKILL.md")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := os.WriteFile(skillPath, skillMDContent(name, description), 0o600); err != nil {
		_ = removeSkillDir(o.TenantID, name)
		if delErr := o.Store.DeleteSkill(r.Context(), o.TenantID, name); delErr != nil {
			o.Logger.Error("回滚技能元数据失败", "skill", name, "error", delErr)
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "写入 SKILL.md 失败")
		return
	}
	writeJSON(w, http.StatusCreated, skillToView(skill))
}

// handleGetSkill 技能详情。
func (o Options) handleGetSkill(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	skill, err := o.Store.GetSkill(r.Context(), o.TenantID, name)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skillToView(skill))
}

// handleUpdateSkill 更新元数据（description/version）。
func (o Options) handleUpdateSkill(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	var req struct {
		Description *string `json:"description"`
		Version     *string `json:"version"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	skill, err := o.Store.UpdateSkill(r.Context(), o.TenantID, name, pg.SkillUpdate{
		Description: req.Description,
		Version:     req.Version,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skillToView(skill))
}

// handleDeleteSkill 删除技能（元数据 + 磁盘目录）。
func (o Options) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	if err := o.Store.DeleteSkill(r.Context(), o.TenantID, name); err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := removeSkillDir(o.TenantID, name); err != nil {
		o.Logger.Error("删除技能目录失败", "skill", name, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListSkillVersions 列出技能的版本快照（时间戳降序）。
func (o Options) handleListSkillVersions(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	if _, err := o.Store.GetSkill(r.Context(), o.TenantID, name); err != nil {
		writeStoreError(w, r, err)
		return
	}
	versions, err := listSkillVersions(o.TenantID, name)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "读取技能版本失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

// handleRestoreSkillVersion 把技能恢复成指定版本，并把当前状态记入历史。
func (o Options) handleRestoreSkillVersion(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	current, err := o.Store.GetSkill(r.Context(), o.TenantID, name)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	timestamp := strings.TrimSpace(r.PathValue("timestamp"))
	if !skillVersionTimestampPattern.MatchString(timestamp) {
		writeStoreError(w, r, fmt.Errorf("%w：版本时间戳格式不合法", pg.ErrInvalidInput))
		return
	}
	versionMeta, hasMeta, err := restoreSkillVersion(o.TenantID, name, timestamp,
		skillVersionMetaFrom(current))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	warning := ""
	if hasMeta {
		// 同时回滚数据库元数据（description/version）；name 是主键不改。
		if _, err := o.Store.RestoreSkillMeta(r.Context(), o.TenantID, name,
			versionMeta.Description, versionMeta.Version); err != nil {
			writeStoreError(w, r, err)
			return
		}
	} else {
		warning = "该版本快照没有 meta.json，仅回滚文件内容，元数据保持当前值"
	}
	o.syncSkillFileCount(r, name)
	skill, err := o.Store.GetSkill(r.Context(), o.TenantID, name)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, skillRestoreResponse{skillView: skillToView(skill), Warning: warning})
}

// handleListSkillFiles 文件列表。
func (o Options) handleListSkillFiles(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	if _, err := o.Store.GetSkill(r.Context(), o.TenantID, name); err != nil {
		writeStoreError(w, r, err)
		return
	}
	files, err := listSkillFiles(skillDirFor(o.TenantID, name))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "读取技能文件列表失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// handleReadSkillFile 读取文本文件（上限 1MiB）。
func (o Options) handleReadSkillFile(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	if _, err := o.Store.GetSkill(r.Context(), o.TenantID, name); err != nil {
		writeStoreError(w, r, err)
		return
	}
	rel, err := skillPathFromRequest(r)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	full, err := skillSafeJoin(skillDirFor(o.TenantID, name), rel)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	info, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		writeStoreError(w, r, fmt.Errorf("%w：文件 %q 不存在", pg.ErrNotFound, filepath.ToSlash(rel)))
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "读取技能文件失败")
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		writeStoreError(w, r, fmt.Errorf("%w：目标路径是符号链接，已拒绝", pg.ErrInvalidInput))
		return
	}
	if info.IsDir() {
		writeStoreError(w, r, fmt.Errorf("%w：%q 是目录，不能按文件读取", pg.ErrInvalidInput, filepath.ToSlash(rel)))
		return
	}
	if info.Size() > skillMaxTextBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "文件超过 1MiB 读取上限")
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "读取技能文件失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    filepath.ToSlash(rel),
		"content": string(data),
		"size":    len(data),
	})
}

// handleWriteSkillFile 覆盖写文本文件（父目录自动创建）。
func (o Options) handleWriteSkillFile(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	skill, err := o.Store.GetSkill(r.Context(), o.TenantID, name)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	rel, err := skillPathFromRequest(r)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !skillExtensionAllowed(rel) {
		writeStoreError(w, r, fmt.Errorf("%w：只允许写入文本类文件（%s）",
			pg.ErrInvalidInput, skillAllowedExtensionText()))
		return
	}
	// 请求体上限：内容上限 + JSON 信封余量。
	r.Body = http.MaxBytesReader(w, r.Body, skillMaxTextBytes+(64<<10))
	var req struct {
		Content *string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "请求体必须是 JSON 对象且 content 为字符串")
		return
	}
	if req.Content == nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "缺少 content 字段")
		return
	}
	content := []byte(*req.Content)
	if int64(len(content)) > skillMaxTextBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, "file_too_large", "文件超过 1MiB 写入上限")
		return
	}
	dir := skillDirFor(o.TenantID, name)
	full, err := skillSafeJoin(dir, rel)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "创建文件目录失败")
		return
	}
	if err := skillRejectSymlink(full); err != nil {
		writeStoreError(w, r, err)
		return
	}
	// 写入前给旧内容打一份版本快照；快照失败不阻塞写入（版本历史是增强能力），
	// 但会记日志，便于运维发现磁盘问题。
	if _, snapErr := snapshotSkillVersion(o.TenantID, name, skillVersionMetaFrom(skill), true); snapErr != nil {
		o.Logger.Warn("技能版本快照失败", "skill", name, "error", snapErr)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "写入技能文件失败")
		return
	}
	o.syncSkillFileCount(r, name)
	writeJSON(w, http.StatusOK, map[string]any{
		"path": filepath.ToSlash(rel),
		"size": len(content),
	})
}

// handleDeleteSkillFile 删除单个文件（目录不允许经此接口删除）。
func (o Options) handleDeleteSkillFile(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	name, ok := o.skillPathName(w, r)
	if !ok {
		return
	}
	if _, err := o.Store.GetSkill(r.Context(), o.TenantID, name); err != nil {
		writeStoreError(w, r, err)
		return
	}
	rel, err := skillPathFromRequest(r)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	full, err := skillSafeJoin(skillDirFor(o.TenantID, name), rel)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	info, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		writeStoreError(w, r, fmt.Errorf("%w：文件 %q 不存在", pg.ErrNotFound, filepath.ToSlash(rel)))
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "读取技能文件失败")
		return
	}
	if info.IsDir() {
		writeStoreError(w, r, fmt.Errorf("%w：%q 是目录，不能用文件接口删除", pg.ErrInvalidInput, filepath.ToSlash(rel)))
		return
	}
	if err := os.Remove(full); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "删除技能文件失败")
		return
	}
	o.syncSkillFileCount(r, name)
	w.WriteHeader(http.StatusNoContent)
}

// syncSkillFileCount 把磁盘实际文件数写回元数据；失败只记日志（不影响主流程）。
func (o Options) syncSkillFileCount(r *http.Request, name string) {
	count := countSkillFiles(skillDirFor(o.TenantID, name))
	if err := o.Store.SetSkillFileCount(r.Context(), o.TenantID, name, count); err != nil {
		o.Logger.Warn("同步技能文件数失败", "skill", name, "error", err)
	}
}

// skillAllowedExtensionText 把白名单扩展名拼成提示文本。
func skillAllowedExtensionText() string {
	exts := make([]string, 0, len(skillAllowedExtensions))
	for ext := range skillAllowedExtensions {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return strings.Join(exts, "/")
}

// handleUploadSkill 上传并安装技能 zip。
//
// 流程：限额读入 → 解压到临时目录（校验路径/符号链接/扩展名/体积/数量）→
// 识别 SKILL.md（根或唯一顶层目录）→ 解析 front matter 取 name/description →
// 校验重名（可用 ?overwrite=true 覆盖）→ 原子改名到目标目录 → upsert 元数据。
func (o Options) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	root := skillsRootDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "创建技能根目录失败")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, skillMaxZipBytes+(1<<20))
	file, header, err := r.FormFile("file")
	if err != nil {
		file, header, err = r.FormFile("zip")
	}
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "缺少上传文件（表单字段 file）或超出大小限制")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, skillMaxZipBytes+1))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "读取上传文件失败")
		return
	}
	if int64(len(data)) > skillMaxZipBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, "zip_too_large", "zip 超过 10MiB 上限")
		return
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "上传内容不是合法的 zip 文件")
		return
	}
	tmpDir, err := os.MkdirTemp(root, ".upload-*")
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "创建临时目录失败")
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	extracted, contentRoot, err := extractSkillZip(reader, tmpDir)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "zip 校验失败："+err.Error())
		return
	}
	name, description, err := resolveSkillIdentity(contentRoot, header.Filename)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	overwrite := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("overwrite")), "true")
	existing, getErr := o.Store.GetSkill(r.Context(), o.TenantID, name)
	exists := getErr == nil
	if getErr != nil && !errors.Is(getErr, pg.ErrNotFound) {
		writeStoreError(w, r, getErr)
		return
	}
	if exists && !overwrite {
		writeError(w, r, http.StatusConflict, "conflict", "技能 "+name+" 已存在（可用 ?overwrite=true 覆盖）")
		return
	}
	version := skillDefaultVersion
	if exists {
		version = existing.Version
	}
	target := skillDirFor(o.TenantID, name)
	if err := os.MkdirAll(skillTenantDir(o.TenantID), 0o700); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "创建租户技能目录失败")
		return
	}
	// 覆盖前把旧目录打成版本快照；.versions 必须暂存到目标之外，因为随后的
	// RemoveAll(target) 会连同它一起删掉。
	carry := ""
	if exists || dirExists(target) {
		if !overwrite {
			writeError(w, r, http.StatusConflict, "conflict", "技能目录 "+name+" 已存在（可用 ?overwrite=true 覆盖）")
			return
		}
		snapshotCarry, snapErr := snapshotBeforeReplace(o.TenantID, name, skillVersionMetaFor(name, existing, exists))
		if snapErr != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "创建版本快照失败")
			return
		}
		carry = snapshotCarry
		if err := os.RemoveAll(target); err != nil {
			if carry != "" {
				_ = os.RemoveAll(carry)
			}
			writeError(w, r, http.StatusInternalServerError, "internal_error", "清理旧技能目录失败")
			return
		}
	}
	if err := os.Rename(contentRoot, target); err != nil {
		if carry != "" {
			_ = os.RemoveAll(carry)
		}
		writeError(w, r, http.StatusInternalServerError, "internal_error", "安装技能目录失败")
		return
	}
	if carry != "" {
		if moveErr := os.Rename(carry, skillVersionsRoot(target)); moveErr != nil {
			o.Logger.Warn("恢复技能版本目录失败", "skill", name, "error", moveErr)
			_ = os.RemoveAll(carry)
		}
	}
	skill, err := o.Store.UpsertSkill(r.Context(), o.TenantID, pg.SkillInput{
		Name:        name,
		Description: description,
		Version:     version,
		FileCount:   extracted,
	})
	if err != nil {
		_ = removeSkillDir(o.TenantID, name)
		writeStoreError(w, r, err)
		return
	}
	views := skillToView(skill)
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":        views.Name,
		"files":       views.Files,
		"description": views.Description,
		"version":     views.Version,
	})
}

// dirExists 判断路径是否是已存在的目录。
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// extractSkillZip 解压 zip 到 dest，返回文件数与「实际内容根目录」。
//
// 内容根目录：SKILL.md 在根时即 dest；若所有内容都在唯一一个顶层目录里且该目录
// 内有 SKILL.md，则返回该子目录（兼容「压缩整个文件夹」的上传习惯）。
func extractSkillZip(reader *zip.Reader, dest string) (int, string, error) {
	if len(reader.File) > skillMaxArchiveFiles*2 {
		return 0, "", fmt.Errorf("zip 条目数超过 %d 上限", skillMaxArchiveFiles*2)
	}
	var (
		files    int
		total    int64
		topNames = map[string]bool{}
		hasFiles bool
	)
	for _, entry := range reader.File {
		rel, err := sanitizeSkillRelPath(entry.Name)
		if err != nil {
			return 0, "", fmt.Errorf("条目 %q 非法：%v", entry.Name, err)
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		mode := entry.Mode()
		if !mode.IsRegular() {
			return 0, "", fmt.Errorf("条目 %q 不是普通文件（不允许符号链接/设备文件）", entry.Name)
		}
		if !skillExtensionAllowed(rel) {
			return 0, "", fmt.Errorf("条目 %q 扩展名不在允许列表（%s）", entry.Name, skillAllowedExtensionText())
		}
		files++
		if files > skillMaxArchiveFiles {
			return 0, "", fmt.Errorf("zip 内文件数超过 %d 上限", skillMaxArchiveFiles)
		}
		if entry.UncompressedSize64 > uint64(skillMaxExtractBytes) {
			return 0, "", fmt.Errorf("条目 %q 超过单文件解压上限", entry.Name)
		}
		full, err := skillSafeJoin(dest, rel)
		if err != nil {
			return 0, "", err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return 0, "", fmt.Errorf("创建目录失败：%w", err)
		}
		source, err := entry.Open()
		if err != nil {
			return 0, "", fmt.Errorf("读取条目 %q 失败：%w", entry.Name, err)
		}
		written, copyErr := copyLimited(source, full, skillMaxExtractBytes-total)
		_ = source.Close()
		if copyErr != nil {
			return 0, "", copyErr
		}
		total += written
		if total > skillMaxExtractBytes {
			return 0, "", fmt.Errorf("解压后总大小超过 10MiB 上限")
		}
		segments := strings.Split(filepath.ToSlash(rel), "/")
		if len(segments) > 0 {
			topNames[segments[0]] = true
		}
		hasFiles = true
	}
	if !hasFiles {
		return 0, "", errors.New("zip 内没有可安装的文件")
	}
	root := dest
	if !fileExists(filepath.Join(dest, "SKILL.md")) {
		// 只有一个顶层目录时，把它当作内容根。
		if len(topNames) == 1 {
			for only := range topNames {
				candidate := filepath.Join(dest, only)
				if dirExists(candidate) && fileExists(filepath.Join(candidate, "SKILL.md")) {
					root = candidate
				}
			}
		}
	}
	if !fileExists(filepath.Join(root, "SKILL.md")) {
		return 0, "", errors.New("缺少 SKILL.md（应位于 zip 根目录或唯一顶层目录内）")
	}
	return files, root, nil
}

// copyLimited 把 src 复制到 dst，最多写 limit 字节；超限返回错误。
func copyLimited(src io.Reader, dst string, limit int64) (int64, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("创建文件失败：%w", err)
	}
	defer out.Close()
	written, err := io.Copy(out, io.LimitReader(src, limit+1))
	if err != nil {
		return written, fmt.Errorf("写入文件失败：%w", err)
	}
	if written > limit {
		return written, errors.New("解压后总大小超过 10MiB 上限")
	}
	return written, nil
}

// fileExists 判断某路径是否为已存在的普通文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolveSkillIdentity 从 SKILL.md front matter 与上传文件名推导技能名/描述。
func resolveSkillIdentity(contentRoot, archiveName string) (string, string, error) {
	data, err := os.ReadFile(filepath.Join(contentRoot, "SKILL.md"))
	if err != nil {
		return "", "", errors.New("读取 SKILL.md 失败")
	}
	name, description := parseSkillFrontMatter(data)
	if strings.TrimSpace(name) == "" {
		name = deriveSkillName(archiveName)
	}
	if err := pg.ValidateSkillName(name); err != nil {
		return "", "", fmt.Errorf("技能名 %q 不合法（必须匹配 ^[a-z0-9][a-z0-9_-]{0,63}$）", name)
	}
	return name, description, nil
}

// parseSkillFrontMatter 解析 YAML front matter 的 name/description 两个简单字段。
//
// 只做最小解析（足够识别技能名与描述），不引入 YAML 依赖：取首个 --- 到下一个
// --- 之间以 "name:" / "description:" 开头的行，去掉首尾引号。
func parseSkillFrontMatter(data []byte) (name, description string) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(strings.TrimLeft(text, " \t"), "---") {
		return "", ""
	}
	lines := strings.Split(text, "\n")
	inFrontMatter := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			if !inFrontMatter {
				inFrontMatter = true
				continue
			}
			break
		}
		if !inFrontMatter {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	return name, description
}

// deriveSkillName 从 zip 文件名推导技能名：小写、非法字符换成 -、去掉首尾 -。
func deriveSkillName(archiveName string) string {
	base := filepath.Base(strings.TrimSpace(archiveName))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	lowered := strings.ToLower(base)
	var b strings.Builder
	for _, r := range lowered {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-_")
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "skill"
	}
	return name
}
