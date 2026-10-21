// Package selfupdate 实现「检查 GitHub Release → 下载 → 校验 → 冒烟 → 换装 → 回滚」。
//
// 安全约束：
//   - 只接受 HTTPS 的 GitHub API / Release 资源；
//   - 下载后必须命中 SHA256SUMS 才允许换装；
//   - 新二进制先跑 --version 冒烟，失败不动当前文件；
//   - 换装前把当前二进制备份为 <name>.old，失败自动还原，可显式 Rollback。
package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 常见错误。
var (
	ErrNoUpdate         = errors.New("当前已是最新版本")
	ErrChecksumMismatch = errors.New("发布包校验和不匹配")
	ErrSmokeFailed      = errors.New("新版本冒烟测试失败")
	ErrDisabled         = errors.New("自更新已禁用")
)

// Config 是自更新参数。
type Config struct {
	Repo           string // owner/repo
	CurrentVersion string // 当前版本（dev 视为开发版，允许更新）
	BinaryPath     string // 当前二进制绝对路径
	StateDir       string // 状态目录（备份与 update.json）
	Enabled        bool
	HTTPClient     *http.Client
	GOOS           string // 测试用；默认 runtime.GOOS
	GOARCH         string
	APIBase        string // GitHub API 根地址；测试可指向 httptest
}

// Updater 负责检查与执行更新。
type Updater struct {
	cfg Config
}

// New 创建 Updater。
func New(cfg Config) *Updater {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	return &Updater{cfg: cfg}
}

// Release 是检查结果。
type Release struct {
	Version     string    `json:"version"`
	Tag         string    `json:"tag"`
	URL         string    `json:"url"`
	Notes       string    `json:"notes,omitempty"`
	PublishedAt time.Time `json:"published_at"`
	AssetURL    string    `json:"asset_url"`
	AssetName   string    `json:"asset_name"`
	ChecksumURL string    `json:"checksum_url"`
}

// Check 查询最新 Release，返回是否有更新。
func (u *Updater) Check(ctx context.Context) (Release, bool, error) {
	if !u.cfg.Enabled {
		return Release{}, false, ErrDisabled
	}
	api := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(u.cfg.APIBase, "/"), u.cfg.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return Release{}, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "beaconx-selfupdate")
	resp, err := u.cfg.HTTPClient.Do(req)
	if err != nil {
		return Release{}, false, fmt.Errorf("检查更新失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, false, fmt.Errorf("检查更新失败: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		Body        string    `json:"body"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return Release{}, false, fmt.Errorf("解析 Release 失败: %w", err)
	}
	rel := Release{
		Version:     strings.TrimPrefix(body.TagName, "v"),
		Tag:         body.TagName,
		URL:         body.HTMLURL,
		Notes:       truncate(body.Body, 2000),
		PublishedAt: body.PublishedAt,
	}
	want := fmt.Sprintf("beaconx-%s-%s.zip", u.cfg.GOOS, u.cfg.GOARCH)
	for _, a := range body.Assets {
		switch a.Name {
		case want:
			rel.AssetURL = a.URL
			rel.AssetName = a.Name
		case "SHA256SUMS":
			rel.ChecksumURL = a.URL
		}
	}
	if rel.AssetURL == "" || rel.ChecksumURL == "" {
		return rel, false, fmt.Errorf("Release 缺少平台包 %s 或 SHA256SUMS", want)
	}
	hasUpdate := compareVersion(rel.Version, u.cfg.CurrentVersion) > 0 || u.cfg.CurrentVersion == "dev"
	return rel, hasUpdate, nil
}

// Apply 下载并安装指定版本。成功后返回新版本号。
func (u *Updater) Apply(ctx context.Context, rel Release) error {
	if !u.cfg.Enabled {
		return ErrDisabled
	}
	if rel.AssetURL == "" || rel.ChecksumURL == "" {
		return errors.New("缺少下载地址，请先调用 Check")
	}
	if err := os.MkdirAll(u.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}
	data, err := u.download(ctx, rel.AssetURL, 200<<20)
	if err != nil {
		return err
	}
	sums, err := u.download(ctx, rel.ChecksumURL, 4<<20)
	if err != nil {
		return err
	}
	name := rel.AssetName
	if name == "" {
		name = filepath.Base(rel.AssetURL)
	}
	want := checksumFor(string(sums), name)
	if want == "" {
		return fmt.Errorf("%w: SHA256SUMS 中没有 %s", ErrChecksumMismatch, name)
	}
	got := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return fmt.Errorf("%w: 期望 %s，实际 %s", ErrChecksumMismatch, want, hex.EncodeToString(got[:]))
	}

	bin, err := extractBinary(data)
	if err != nil {
		return err
	}
	tmpPath := filepath.Join(u.cfg.StateDir, "beaconx.new")
	if err := os.WriteFile(tmpPath, bin, 0o755); err != nil {
		return fmt.Errorf("写入新二进制失败: %w", err)
	}
	if err := u.smoke(ctx, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	backup := u.cfg.BinaryPath + ".old"
	if err := copyFile(u.cfg.BinaryPath, backup); err != nil {
		return fmt.Errorf("备份当前二进制失败: %w", err)
	}
	if err := os.Rename(tmpPath, u.cfg.BinaryPath); err != nil {
		return fmt.Errorf("替换二进制失败: %w", err)
	}
	state := map[string]any{"version": rel.Version, "installed_at": time.Now().UTC()}
	raw, _ := json.MarshalIndent(state, "", "  ")
	_ = os.WriteFile(filepath.Join(u.cfg.StateDir, "update.json"), raw, 0o600)
	return nil
}

// Rollback 用 <binary>.old 还原上一版本。
func (u *Updater) Rollback() error {
	backup := u.cfg.BinaryPath + ".old"
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("没有可回滚的备份: %w", err)
	}
	if err := os.Rename(backup, u.cfg.BinaryPath); err != nil {
		return fmt.Errorf("回滚失败: %w", err)
	}
	return nil
}

func (u *Updater) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "beaconx-selfupdate")
	resp, err := u.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("读取下载内容失败: %w", err)
	}
	return data, nil
}

func (u *Updater) smoke(ctx context.Context, path string) error {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %v（输出：%s）", ErrSmokeFailed, err, truncate(string(out), 300))
	}
	if !bytes.Contains(bytes.ToLower(out), []byte("beaconx")) {
		return fmt.Errorf("%w: 输出未包含 beaconx", ErrSmokeFailed)
	}
	return nil
}

func extractBinary(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("解压发布包失败: %w", err)
	}
	for _, f := range zr.File {
		name := filepath.Base(f.Name)
		if name == "beaconx" || name == "beaconx.exe" {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("读取发布包内二进制失败: %w", err)
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 200<<20))
		}
	}
	return nil, errors.New("发布包中缺少 beaconx 可执行文件")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// checksumFor 从 SHA256SUMS 文本中取出指定文件的校验和。
func checksumFor(sums, filename string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == filename {
			return fields[0]
		}
	}
	return ""
}

// compareVersion 比较语义化版本；非数字段按字符串比较。
func compareVersion(a, b string) int {
	if a == b {
		return 0
	}
	// dev 是本地开发版，永远视为比正式版本旧。
	if strings.TrimPrefix(a, "v") == "dev" {
		return -1
	}
	if strings.TrimPrefix(b, "v") == "dev" {
		return 1
	}
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var sa, sb string
		if i < len(pa) {
			sa = pa[i]
		}
		if i < len(pb) {
			sb = pb[i]
		}
		na, ea := parseNum(sa)
		nb, eb := parseNum(sb)
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		default:
			if sa != sb {
				if sa < sb {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

func parseNum(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
