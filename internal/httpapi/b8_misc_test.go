package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M6.2-B8 清理测试：Skills 版本保留可配置 + versions.has_meta 标记。
//
// 复用 b5agentsHarness（真 PostgreSQL + 临时状态目录）与 b6 的 skills 辅助函数；
// 未设置 BEACONX_TEST_PG_DSN 时整体跳过。
//
// 说明：MCP stdio 的 Windows 分支不在本文件（这里是 httpapi 包，且 Windows CI
// 只跑 internal/app/mcp 与 internal/infra/llm）；平台断言在
// internal/app/mcp/sandbox_test.go 的 TestStdioUnsupportedOnWindows 中。

// b8SkillVersionsDir 返回某技能在磁盘上的版本根目录。
func b8SkillVersionsDir(skillsDir string, tenantID int64, name string) string {
	return filepath.Join(skillsDir, fmt.Sprintf("%d", tenantID), name, ".versions")
}

// b8ListSkillVersionDirs 返回磁盘上的版本目录名（用于断言保留份数）。
func b8ListSkillVersionDirs(t *testing.T, skillsDir string, tenantID int64, name string) []string {
	t.Helper()
	entries, err := os.ReadDir(b8SkillVersionsDir(skillsDir, tenantID, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读取版本目录失败: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out
}

// TestB8SkillsKeepVersionsConfigurable 验证 BEACONX_SKILLS_KEEP_VERSIONS 生效。
func TestB8SkillsKeepVersionsConfigurable(t *testing.T) {
	h := b5agentsNewHarness(t)
	skillsDir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", skillsDir)
	t.Setenv("BEACONX_SKILLS_KEEP_VERSIONS", "2")

	b6Do(t, h, http.MethodPost, "/api/v1/skills", `{"name":"keep-two"}`, http.StatusCreated)
	for i := 0; i < 5; i++ {
		b6SkillsWriteFile(t, h, "keep-two", "notes.md", fmt.Sprintf("v%d", i), http.StatusOK)
	}
	versions := b6SkillsListVersions(t, h, "keep-two")
	if len(versions) != 2 {
		t.Fatalf("keep=2 时版本数 = %d，期望 2（%v）", len(versions), versions)
	}
	if dirs := b8ListSkillVersionDirs(t, skillsDir, h.tenantA, "keep-two"); len(dirs) != 2 {
		t.Fatalf("keep=2 时磁盘版本目录数 = %d，期望 2（%v）", len(dirs), dirs)
	}
	// 保留的是最近两份：内容应按时间倒序对应最后两次写入前的状态。
	if versions[0]["timestamp"].(string) < versions[1]["timestamp"].(string) {
		t.Fatalf("版本未按时间降序: %v", versions)
	}
}

// TestB8SkillsKeepVersionsInvalid 验证非法配置回退默认 5。
func TestB8SkillsKeepVersionsInvalid(t *testing.T) {
	h := b5agentsNewHarness(t)
	skillsDir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", skillsDir)

	cases := []struct {
		name  string
		value string
	}{
		{name: "空值", value: ""},
		{name: "零", value: "0"},
		{name: "负数", value: "-3"},
		{name: "超上限", value: "51"},
		{name: "非数字", value: "abc"},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("BEACONX_SKILLS_KEEP_VERSIONS", testCase.value)
			skillName := fmt.Sprintf("fallback-%d", index)
			b6Do(t, h, http.MethodPost, "/api/v1/skills", `{"name":"`+skillName+`"}`, http.StatusCreated)
			for i := 0; i < 7; i++ {
				b6SkillsWriteFile(t, h, skillName, "notes.md", fmt.Sprintf("v%d", i), http.StatusOK)
			}
			versions := b6SkillsListVersions(t, h, skillName)
			if len(versions) != 5 {
				t.Fatalf("非法值 %q 应回退默认 5，实际版本数 = %d", testCase.value, len(versions))
			}
		})
	}
}

// TestB8SkillsVersionHasMeta 验证版本列表的 has_meta 标记。
func TestB8SkillsVersionHasMeta(t *testing.T) {
	h := b5agentsNewHarness(t)
	skillsDir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", skillsDir)

	b6Do(t, h, http.MethodPost, "/api/v1/skills", `{"name":"meta-flag"}`, http.StatusCreated)
	b6SkillsWriteFile(t, h, "meta-flag", "a.md", "one", http.StatusOK)
	b6SkillsWriteFile(t, h, "meta-flag", "a.md", "two", http.StatusOK)

	versions := b6SkillsListVersions(t, h, "meta-flag")
	if len(versions) != 2 {
		t.Fatalf("版本数 = %d，期望 2", len(versions))
	}
	for _, version := range versions {
		if version["has_meta"] != true {
			t.Fatalf("新版本应标记 has_meta=true: %v", version)
		}
	}

	// 手工删除一份快照的 meta.json（模拟 B7 之前的旧版本）。
	oldest := versions[len(versions)-1]["timestamp"].(string)
	metaPath := filepath.Join(b8SkillVersionsDir(skillsDir, h.tenantA, "meta-flag"), oldest, "meta.json")
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("删除 meta.json 失败: %v", err)
	}
	versions = b6SkillsListVersions(t, h, "meta-flag")
	withMeta, withoutMeta := 0, 0
	for _, version := range versions {
		if version["has_meta"] == true {
			withMeta++
		} else {
			withoutMeta++
			if version["timestamp"].(string) != oldest {
				t.Fatalf("has_meta=false 的版本不是被删 meta 的那份: %v", version)
			}
		}
	}
	if withMeta != 1 || withoutMeta != 1 {
		t.Fatalf("has_meta 统计 = 有 %d / 无 %d，期望 1/1", withMeta, withoutMeta)
	}
}

// TestB8SkillsRestoreLegacyWarning 验证旧版本（无 meta）restore 仍返回 warning。
func TestB8SkillsRestoreLegacyWarning(t *testing.T) {
	h := b5agentsNewHarness(t)
	skillsDir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", skillsDir)

	b6Do(t, h, http.MethodPost, "/api/v1/skills",
		`{"name":"legacy-warning","description":"before"}`, http.StatusCreated)
	b6SkillsWriteFile(t, h, "legacy-warning", "a.md", "content-1", http.StatusOK)
	b6SkillsWriteFile(t, h, "legacy-warning", "a.md", "content-2", http.StatusOK)
	versions := b6SkillsListVersions(t, h, "legacy-warning")
	if len(versions) != 2 {
		t.Fatalf("版本数 = %d，期望 2", len(versions))
	}
	stamp := versions[0]["timestamp"].(string)
	if err := os.Remove(filepath.Join(b8SkillVersionsDir(skillsDir, h.tenantA, "legacy-warning"), stamp, "meta.json")); err != nil {
		t.Fatalf("删除 meta.json 失败: %v", err)
	}

	raw := b6Do(t, h, http.MethodPost,
		"/api/v1/skills/legacy-warning/versions/"+stamp+"/restore", "", http.StatusOK)
	var restored struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("解析恢复响应失败: %v", err)
	}
	if !strings.Contains(restored.Warning, "meta.json") {
		t.Fatalf("无 meta 版本应返回 warning，实际 %q", restored.Warning)
	}
	if got := b6SkillsReadFile(t, h, "legacy-warning", "a.md"); got != "content-1" {
		t.Fatalf("文件内容未按版本回滚: %q", got)
	}
}
