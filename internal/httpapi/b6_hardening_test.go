package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// M6.2-B6 硬化测试：Workspace 配额 + Skills 版本历史。
//
// 复用 b5agentsHarness（真 PostgreSQL + 临时状态目录 + 租户 A/B），未设置
// BEACONX_TEST_PG_DSN 时整体跳过。MCP stdio 沙箱的单测在
// internal/app/mcp/sandbox_test.go（需要直接操作包内开关与真实子进程）。

// b6Do 发送请求并断言状态码，返回响应体。
func b6Do(t *testing.T, h *b5agentsHarness, method, path, body string, want int) []byte {
	t.Helper()
	status, raw, _ := h.do(t, method, path, testAPIToken, body)
	if status != want {
		t.Fatalf("%s %s 状态码 = %d，期望 %d，响应 %s", method, path, status, want, raw)
	}
	return raw
}

// b6WorkspacePath 构造任务工作区文件 URL。
func b6WorkspacePath(missionID int64, rel string) string {
	if rel == "" {
		return fmt.Sprintf("/api/v1/workspace/files?mission_id=%d", missionID)
	}
	return fmt.Sprintf("/api/v1/workspace/files/%s?mission_id=%d", rel, missionID)
}

// b6WorkspaceQuota 读取列表接口返回的配额与用量。
func b6WorkspaceQuota(t *testing.T, h *b5agentsHarness, missionID int64) (quota, used int64) {
	t.Helper()
	raw := b6Do(t, h, http.MethodGet, b6WorkspacePath(missionID, ""), "", http.StatusOK)
	var list struct {
		QuotaBytes int64 `json:"quota_bytes"`
		UsedBytes  int64 `json:"used_bytes"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析工作区列表失败: %v", err)
	}
	return list.QuotaBytes, list.UsedBytes
}

// TestB6WorkspaceQuota 验证小配额下的写入拒绝、覆盖折算与配额字段。
func TestB6WorkspaceQuota(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_WORKSPACE_QUOTA_MB", "1")
	mission := h.createMission(t, h.tenantA, "配额测试任务")

	quota, used := b6WorkspaceQuota(t, h, mission.ID)
	if quota != 1<<20 {
		t.Fatalf("quota_bytes = %d，期望 %d", quota, int64(1<<20))
	}
	if used != 0 {
		t.Fatalf("空工作区 used_bytes = %d，期望 0", used)
	}

	// 700KB 正常写入。
	b6Do(t, h, http.MethodPut, b6WorkspacePath(mission.ID, "big/a.txt"),
		strings.Repeat("A", 700<<10), http.StatusOK)
	quota, used = b6WorkspaceQuota(t, h, mission.ID)
	if quota != 1<<20 || used != 700<<10 {
		t.Fatalf("写入后 quota/used = %d/%d，期望 %d/%d", quota, used, int64(1<<20), int64(700<<10))
	}

	// 再写 400KB → 总量 1100KB > 1024KB，413 且错误码为 quota_exceeded。
	status, raw, _ := h.do(t, http.MethodPut, b6WorkspacePath(mission.ID, "big/b.txt"),
		testAPIToken, strings.Repeat("B", 400<<10))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超配额状态码 = %d，期望 413，响应 %s", status, raw)
	}
	if !strings.Contains(string(raw), "quota_exceeded") {
		t.Fatalf("超配额错误码不明确: %s", raw)
	}
	if _, used = b6WorkspaceQuota(t, h, mission.ID); used != 700<<10 {
		t.Fatalf("超配额写入不应改变用量，used = %d", used)
	}

	// 覆盖同名文件按「替换」折算：700KB 覆盖 700KB 仍应成功。
	b6Do(t, h, http.MethodPut, b6WorkspacePath(mission.ID, "big/a.txt"),
		strings.Repeat("C", 700<<10), http.StatusOK)
	if _, used = b6WorkspaceQuota(t, h, mission.ID); used != 700<<10 {
		t.Fatalf("覆盖后 used = %d，期望 %d", used, int64(700<<10))
	}

	// 还剩 324KB 空间：写 300KB 成功，写 400KB 失败。
	b6Do(t, h, http.MethodPut, b6WorkspacePath(mission.ID, "small/c.txt"),
		strings.Repeat("D", 300<<10), http.StatusOK)
	status, raw, _ = h.do(t, http.MethodPut, b6WorkspacePath(mission.ID, "small/d.txt"),
		testAPIToken, strings.Repeat("E", 400<<10))
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("再次超配额状态码 = %d，期望 413，响应 %s", status, raw)
	}

	// 删除大文件后空间释放，可以继续写。
	b6Do(t, h, http.MethodDelete, b6WorkspacePath(mission.ID, "big/a.txt"), "", http.StatusNoContent)
	b6Do(t, h, http.MethodPut, b6WorkspacePath(mission.ID, "small/e.txt"),
		strings.Repeat("F", 700<<10), http.StatusOK)
}

// TestB6WorkspaceQuotaDefault 验证未配置时使用默认 100MB。
func TestB6WorkspaceQuotaDefault(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_WORKSPACE_QUOTA_MB", "0")
	mission := h.createMission(t, h.tenantA, "默认配额任务")
	quota, _ := b6WorkspaceQuota(t, h, mission.ID)
	if quota != 100<<20 {
		t.Fatalf("默认 quota_bytes = %d，期望 %d", quota, int64(100<<20))
	}
}

// TestB6WorkspaceQuotaConcurrent 验证并发写入不会超卖（配额 1MB、5 个 400KB
// 并发请求最多成功 2 个）。
func TestB6WorkspaceQuotaConcurrent(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_WORKSPACE_QUOTA_MB", "1")
	mission := h.createMission(t, h.tenantA, "并发配额任务")

	const workers = 5
	const size = 400 << 10
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = make([]int, 0, workers)
		errs     = make([]error, 0, workers)
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			url := h.baseURL + b6WorkspacePath(mission.ID, fmt.Sprintf("concurrent/%d.txt", index))
			req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(strings.Repeat("X", size)))
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			req.Header.Set("Authorization", "Bearer "+testAPIToken)
			resp, err := h.client.Do(req)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer resp.Body.Close()
			mu.Lock()
			statuses = append(statuses, resp.StatusCode)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发请求出错: %v", errs)
	}
	successes := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusRequestEntityTooLarge:
		default:
			t.Fatalf("并发写入出现意外状态码 %d（全部=%v）", status, statuses)
		}
	}
	if successes != 2 {
		t.Fatalf("1MB 配额下 400KB×5 并发成功数 = %d，期望 2（全部=%v）", successes, statuses)
	}
	quota, used := b6WorkspaceQuota(t, h, mission.ID)
	if used > quota {
		t.Fatalf("超卖：used=%d > quota=%d", used, quota)
	}
	if used != int64(successes*size) {
		t.Fatalf("used = %d，期望 %d", used, int64(successes*size))
	}
}

// ---------- Skills 版本历史 ----------

// b6SkillsListVersions 读取版本列表。
func b6SkillsListVersions(t *testing.T, h *b5agentsHarness, name string) []map[string]any {
	t.Helper()
	raw := b6Do(t, h, http.MethodGet, "/api/v1/skills/"+name+"/versions", "", http.StatusOK)
	var list struct {
		Versions []map[string]any `json:"versions"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析版本列表失败: %v", err)
	}
	return list.Versions
}

// b6SkillsWriteFile 写入技能文件。
func b6SkillsWriteFile(t *testing.T, h *b5agentsHarness, name, rel, content string, want int) {
	t.Helper()
	body := fmt.Sprintf(`{"content":%q}`, content)
	b6Do(t, h, http.MethodPut, "/api/v1/skills/"+name+"/files/"+rel, body, want)
}

// b6SkillsReadFile 读取技能文件并返回内容。
func b6SkillsReadFile(t *testing.T, h *b5agentsHarness, name, rel string) string {
	t.Helper()
	raw := b6Do(t, h, http.MethodGet, "/api/v1/skills/"+name+"/files/"+rel, "", http.StatusOK)
	var file struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("解析文件内容失败: %v", err)
	}
	return file.Content
}

// TestB6SkillsVersions 验证「修改→版本出现→恢复旧内容」主链路与 .versions 不可见。
func TestB6SkillsVersions(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_SKILLS_DIR", t.TempDir())
	b6Do(t, h, http.MethodPost, "/api/v1/skills", `{"name":"versioned","description":"版本测试"}`, http.StatusCreated)

	// 创建后还没写入过文件：没有快照。
	if versions := b6SkillsListVersions(t, h, "versioned"); len(versions) != 0 {
		t.Fatalf("初始版本列表应为空，实际 %v", versions)
	}

	b6SkillsWriteFile(t, h, "versioned", "notes.md", "v1", http.StatusOK)
	versions := b6SkillsListVersions(t, h, "versioned")
	if len(versions) != 1 {
		t.Fatalf("第一次写入后版本数 = %d，期望 1", len(versions))
	}
	firstVersion := versions[0]["timestamp"].(string)
	if !strings.Contains(firstVersion, "T") {
		t.Fatalf("版本时间戳格式异常: %q", firstVersion)
	}
	if versions[0]["files"].(float64) < 1 || versions[0]["size"].(float64) <= 0 {
		t.Fatalf("版本快照统计异常: %v", versions[0])
	}

	b6SkillsWriteFile(t, h, "versioned", "notes.md", "v2-content", http.StatusOK)
	if versions = b6SkillsListVersions(t, h, "versioned"); len(versions) != 2 {
		t.Fatalf("第二次写入后版本数 = %d，期望 2", len(versions))
	}
	// 第二次写入产生的快照（时间戳最大）保存的是 v1 内容。
	versionBeforeV2 := versions[0]["timestamp"].(string)
	if got := b6SkillsReadFile(t, h, "versioned", "notes.md"); got != "v2-content" {
		t.Fatalf("当前内容 = %q", got)
	}

	// 恢复「第二次写入前」的快照 → notes.md 回到 v1。
	b6Do(t, h, http.MethodPost,
		fmt.Sprintf("/api/v1/skills/versioned/versions/%s/restore", versionBeforeV2), "", http.StatusOK)
	if got := b6SkillsReadFile(t, h, "versioned", "notes.md"); got != "v1" {
		t.Fatalf("恢复后内容 = %q，期望 v1", got)
	}
	// 恢复本身也留下一份「恢复前」快照。
	if versions = b6SkillsListVersions(t, h, "versioned"); len(versions) != 3 {
		t.Fatalf("恢复后版本数 = %d，期望 3", len(versions))
	}

	// .versions 不出现在文件列表、不参与文件数统计。
	raw := b6Do(t, h, http.MethodGet, "/api/v1/skills/versioned/files", "", http.StatusOK)
	var files struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatalf("解析文件列表失败: %v", err)
	}
	for _, file := range files.Files {
		path, _ := file["path"].(string)
		if strings.Contains(path, ".versions") {
			t.Fatalf("文件列表泄漏了版本目录: %s", path)
		}
	}
	if len(files.Files) != 2 { // SKILL.md + notes.md
		t.Fatalf("文件数 = %d，期望 2（%v）", len(files.Files), files.Files)
	}
	detail := b6Do(t, h, http.MethodGet, "/api/v1/skills/versioned", "", http.StatusOK)
	var skill struct {
		FileCount int `json:"file_count"`
		Files     int `json:"files"`
	}
	if err := json.Unmarshal(detail, &skill); err != nil {
		t.Fatalf("解析技能详情失败: %v", err)
	}
	if skill.FileCount != 2 || skill.Files != 2 {
		t.Fatalf("文件数统计被版本目录污染: %+v", skill)
	}

	// .versions 不能经文件接口直接访问。
	raw = b6Do(t, h, http.MethodGet,
		"/api/v1/skills/versioned/files/.versions/"+versionBeforeV2+"/notes.md", "", http.StatusBadRequest)
	if !strings.Contains(string(raw), "版本") {
		t.Fatalf(".versions 拒绝理由不明确: %s", raw)
	}

	// 非法/不存在的版本时间戳。
	b6Do(t, h, http.MethodPost, "/api/v1/skills/versioned/versions/not-a-timestamp/restore", "", http.StatusBadRequest)
	b6Do(t, h, http.MethodPost, "/api/v1/skills/versioned/versions/20990101T000000.000000000/restore", "", http.StatusNotFound)
}

// TestB6SkillsVersionLimit 验证只保留最近 5 份快照。
func TestB6SkillsVersionLimit(t *testing.T) {
	h := b5agentsNewHarness(t)
	skillsDir := t.TempDir()
	t.Setenv("BEACONX_SKILLS_DIR", skillsDir)
	b6Do(t, h, http.MethodPost, "/api/v1/skills", `{"name":"many-versions"}`, http.StatusCreated)

	for i := 0; i < 8; i++ {
		b6SkillsWriteFile(t, h, "many-versions", "notes.md", fmt.Sprintf("content-%d", i), http.StatusOK)
	}
	versions := b6SkillsListVersions(t, h, "many-versions")
	if len(versions) != 5 {
		t.Fatalf("版本数 = %d，期望上限 5", len(versions))
	}
	versionsDir := filepath.Join(skillsDir, fmt.Sprintf("%d", h.tenantA), "many-versions", ".versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		t.Fatalf("读取版本目录失败: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("磁盘版本目录数 = %d，期望 5", len(entries))
	}
	// 时间戳降序，且可恢复最近一份。
	newest := versions[0]["timestamp"].(string)
	b6Do(t, h, http.MethodPost,
		fmt.Sprintf("/api/v1/skills/many-versions/versions/%s/restore", newest), "", http.StatusOK)
}

// TestB6SkillsUploadSnapshot 验证上传覆盖前也会打快照，且 .versions 在目录替换后仍保留。
func TestB6SkillsUploadSnapshot(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_SKILLS_DIR", t.TempDir())

	oldZip := b5mcpZip(t, map[string]string{
		"SKILL.md": "---\nname: snapshot-skill\ndescription: 旧版本\n---\n",
		"a.md":     "old-content",
	})
	if status, raw := b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload", oldZip); status != http.StatusCreated {
		t.Fatalf("上传旧版本状态码 = %d，响应 %s", status, raw)
	}
	newZip := b5mcpZip(t, map[string]string{
		"SKILL.md": "---\nname: snapshot-skill\ndescription: 新版本\n---\n",
		"a.md":     "new-content",
	})
	if status, raw := b5mcpUpload(t, h.client, h.baseURL, "/api/v1/skills/upload?overwrite=true", newZip); status != http.StatusCreated {
		t.Fatalf("覆盖上传状态码 = %d，响应 %s", status, raw)
	}
	versions := b6SkillsListVersions(t, h, "snapshot-skill")
	if len(versions) != 1 {
		t.Fatalf("覆盖上传后版本数 = %d，期望 1", len(versions))
	}
	// 恢复后应回到旧内容，且 .versions 仍然存在（没被目录替换删掉）。
	timestamp := versions[0]["timestamp"].(string)
	b6Do(t, h, http.MethodPost,
		fmt.Sprintf("/api/v1/skills/snapshot-skill/versions/%s/restore", timestamp), "", http.StatusOK)
	if got := b6SkillsReadFile(t, h, "snapshot-skill", "a.md"); got != "old-content" {
		t.Fatalf("恢复后 a.md = %q，期望 old-content", got)
	}
	if versions = b6SkillsListVersions(t, h, "snapshot-skill"); len(versions) < 2 {
		t.Fatalf("恢复后的版本数 = %d，期望 >= 2（覆盖快照 + 恢复前快照）", len(versions))
	}
}

// TestB6SkillsVersionsTenantIsolation 验证版本接口同样按租户隔离。
func TestB6SkillsVersionsTenantIsolation(t *testing.T) {
	h := b5agentsNewHarness(t)
	t.Setenv("BEACONX_SKILLS_DIR", t.TempDir())
	serverB := h.newServer(t, h.tenantB, true)

	b6Do(t, h, http.MethodPost, "/api/v1/skills", `{"name":"tenant-only"}`, http.StatusCreated)
	b6SkillsWriteFile(t, h, "tenant-only", "a.md", "a", http.StatusOK)

	status, raw, _ := doAt(t, serverB.Client(), serverB.URL, http.MethodGet, "/api/v1/skills/tenant-only/versions", testAPIToken, "")
	if status != http.StatusNotFound {
		t.Fatalf("租户B读版本状态码 = %d，期望 404（body=%s）", status, raw)
	}
}
