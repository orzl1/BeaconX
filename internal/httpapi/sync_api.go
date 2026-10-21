package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/orzl1/BeaconX/internal/app/syncdata"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B5d：外部数据源同步（Sync）接口（P-B5-29..37）。
//
// 路由在 init() 里通过 RegisterFeatureRoutes + Build 注册（不修改 handler.go）。
// 运行是异步的：POST /sync/run 先创建 running 记录（202），执行器在后台完成后
// 把状态写回 succeeded/failed；调用方通过 GET /sync/runs 轮询。
//
// 响应里的 config 会做掩码：敏感 header 的值替换为 ***，URL 里的 userinfo 用
// url.Redacted 处理，避免把凭据回显给浏览器。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{Method: http.MethodGet, Pattern: "/api/v1/sync/sources", Summary: "数据源列表",
			Build: func(o Options) http.HandlerFunc { return o.handleListSyncSources }},
		FeatureRoute{Method: http.MethodPost, Pattern: "/api/v1/sync/sources", Summary: "新建数据源",
			Build: func(o Options) http.HandlerFunc { return o.handleCreateSyncSource }},
		FeatureRoute{Method: http.MethodGet, Pattern: "/api/v1/sync/sources/{id}", Summary: "数据源详情",
			Build: func(o Options) http.HandlerFunc { return o.handleGetSyncSource }},
		FeatureRoute{Method: http.MethodPatch, Pattern: "/api/v1/sync/sources/{id}", Summary: "更新数据源",
			Build: func(o Options) http.HandlerFunc { return o.handleUpdateSyncSource }},
		FeatureRoute{Method: http.MethodDelete, Pattern: "/api/v1/sync/sources/{id}", Summary: "删除数据源",
			Build: func(o Options) http.HandlerFunc { return o.handleDeleteSyncSource }},
		FeatureRoute{Method: http.MethodGet, Pattern: "/api/v1/sync/projects", Summary: "外部项目列表（?source_id=）",
			Build: func(o Options) http.HandlerFunc { return o.handleListSyncProjects }},
		FeatureRoute{Method: http.MethodPost, Pattern: "/api/v1/sync/projects", Summary: "新建外部项目",
			Build: func(o Options) http.HandlerFunc { return o.handleCreateSyncProject }},
		FeatureRoute{Method: http.MethodPost, Pattern: "/api/v1/sync/run", Summary: "触发一次同步（异步执行）",
			Build: func(o Options) http.HandlerFunc { return o.handleStartSyncRun }},
		FeatureRoute{Method: http.MethodGet, Pattern: "/api/v1/sync/runs", Summary: "同步运行记录（?source_id=&status=&limit=）",
			Build: func(o Options) http.HandlerFunc { return o.handleListSyncRuns }},
	)
}

// syncSourceCreateRequest 是创建数据源的请求体。config 用 RawMessage 承载，
// 以便把「不是 JSON 对象」与「请求体不是 JSON」区分成不同错误。
type syncSourceCreateRequest struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

// syncSourcePatchRequest 是数据源的局部更新请求体：字段缺省表示不变。
type syncSourcePatchRequest struct {
	Name    *string         `json:"name"`
	Type    *string         `json:"type"`
	Config  json.RawMessage `json:"config"`
	Enabled *bool           `json:"enabled"`
}

// syncProjectCreateRequest 是新建外部项目的请求体。
//
// external_id 与 remote_id 都接受（不同上游的字段名不同）；mission_id 若提供，
// 会原样存进 data.mission_id，供前端做「外部项目 ↔ 任务」的弱关联展示。
type syncProjectCreateRequest struct {
	SourceID   int64          `json:"source_id"`
	ExternalID string         `json:"external_id"`
	RemoteID   string         `json:"remote_id"`
	Name       string         `json:"name"`
	Data       map[string]any `json:"data"`
	MissionID  *int64         `json:"mission_id"`
}

// syncRunCreateRequest 是触发同步的请求体。
type syncRunCreateRequest struct {
	SourceID  int64  `json:"source_id"`
	ProjectID *int64 `json:"project_id"`
	Mode      string `json:"mode"`
}

// parseSyncConfig 解析 config：缺省返回 nil（表示不变），null 返回空对象，
// 其它非对象类型返回错误。
func parseSyncConfig(w http.ResponseWriter, r *http.Request, raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return map[string]any{}, true
	}
	var config map[string]any
	if err := json.Unmarshal(trimmed, &config); err != nil || config == nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "config 必须是 JSON 对象")
		return nil, false
	}
	return config, true
}

// ---------- 敏感字段掩码 ----------

// sensitiveHeaderKeys 是需要掩码的 header 名（小写比较，子串匹配）。
var sensitiveHeaderKeys = []string{
	"authorization", "cookie", "token", "secret", "password", "api-key", "apikey", "auth",
}

// maskHeaderName 报告某个 header 名是否敏感。
func maskHeaderName(name string) bool {
	lower := strings.ToLower(name)
	for _, key := range sensitiveHeaderKeys {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}

// maskURLUserInfo 把 URL 里的 userinfo 替换为 ***（无法解析时原样返回）。
//
// 不用 url.User("***") + String()：URL 序列化会把 * 转义成 %2A，回显会变成
// 难读的 http://%2A%2A%2A@host。这里先去掉 userinfo 再拼回可读的 ***。
func maskURLUserInfo(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	withoutUser := *parsed
	withoutUser.User = nil
	masked := withoutUser.String()
	if idx := strings.Index(masked, "://"); idx >= 0 {
		return masked[:idx+3] + "***@" + masked[idx+3:]
	}
	return masked
}

// maskSyncSourceConfig 返回掩码后的 config 副本（不修改入参 map）。
func maskSyncSourceConfig(config map[string]any) map[string]any {
	if len(config) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(config))
	for key, value := range config {
		out[key] = value
	}
	if raw, ok := out["url"].(string); ok {
		out["url"] = maskURLUserInfo(raw)
	}
	if headers, ok := out["headers"].(map[string]any); ok {
		masked := make(map[string]any, len(headers))
		for key, value := range headers {
			if maskHeaderName(key) {
				masked[key] = "***"
				continue
			}
			masked[key] = value
		}
		out["headers"] = masked
	}
	return out
}

// maskSyncSource 返回掩码后的数据源副本。
func maskSyncSource(source pg.SyncSource) pg.SyncSource {
	source.Config = maskSyncSourceConfig(source.Config)
	return source
}

// ---------- 数据源 ----------

// handleListSyncSources 列出数据源（config 已掩码）。
func (o Options) handleListSyncSources(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	sources, err := o.Store.ListSyncSources(r.Context(), o.TenantID, listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	masked := make([]pg.SyncSource, 0, len(sources))
	for _, source := range sources {
		masked = append(masked, maskSyncSource(source))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": masked})
}

// handleCreateSyncSource 创建数据源；重名返回 409。
func (o Options) handleCreateSyncSource(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req syncSourceCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	config, ok := parseSyncConfig(w, r, req.Config)
	if !ok {
		return
	}
	source, err := o.Store.CreateSyncSource(r.Context(), o.TenantID, pg.SyncSourceInput{
		Name:    req.Name,
		Type:    req.Type,
		Config:  config,
		Enabled: req.Enabled,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, maskSyncSource(source))
}

// handleGetSyncSource 读取数据源详情（config 已掩码）。
func (o Options) handleGetSyncSource(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	sourceID, err := pathInt64Param(r, "id", "数据源")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	source, err := o.Store.GetSyncSource(r.Context(), o.TenantID, sourceID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, maskSyncSource(source))
}

// handleUpdateSyncSource 局部更新数据源。
func (o Options) handleUpdateSyncSource(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	sourceID, err := pathInt64Param(r, "id", "数据源")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	var req syncSourcePatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	config, ok := parseSyncConfig(w, r, req.Config)
	if !ok {
		return
	}
	source, err := o.Store.UpdateSyncSource(r.Context(), o.TenantID, sourceID, pg.SyncSourcePatch{
		Name:    req.Name,
		Type:    req.Type,
		Config:  config,
		Enabled: req.Enabled,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, maskSyncSource(source))
}

// handleDeleteSyncSource 删除数据源（项目与运行记录级联删除）。
func (o Options) handleDeleteSyncSource(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	sourceID, err := pathInt64Param(r, "id", "数据源")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.DeleteSyncSource(r.Context(), o.TenantID, sourceID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 外部项目 ----------

// handleListSyncProjects 列出外部项目，支持 ?source_id= 过滤。
func (o Options) handleListSyncProjects(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	sourceID, ok := parseOptionalQueryID(w, r, "source_id", "source_id")
	if !ok {
		return
	}
	projects, err := o.Store.ListSyncProjects(r.Context(), o.TenantID, sourceID, listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// handleCreateSyncProject 新建外部项目；同数据源下 external_id 重复返回 409。
func (o Options) handleCreateSyncProject(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req syncProjectCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	externalID := strings.TrimSpace(req.ExternalID)
	if externalID == "" {
		externalID = strings.TrimSpace(req.RemoteID)
	}
	if externalID == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "external_id（或 remote_id）不能为空")
		return
	}
	if req.SourceID <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "source_id 必须为正整数")
		return
	}
	data := req.Data
	if data == nil {
		data = map[string]any{}
	}
	if req.MissionID != nil {
		if *req.MissionID <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "mission_id 必须为正整数")
			return
		}
		data["mission_id"] = *req.MissionID
	}
	project, err := o.Store.CreateSyncProject(r.Context(), o.TenantID, pg.SyncProjectInput{
		SourceID:   req.SourceID,
		ExternalID: externalID,
		Name:       req.Name,
		Data:       data,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

// ---------- 同步运行 ----------

// handleStartSyncRun 触发一次同步：创建 running 记录后异步执行，返回 202。
func (o Options) handleStartSyncRun(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req syncRunCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.SourceID <= 0 && req.ProjectID == nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "必须提供 source_id 或 project_id")
		return
	}

	sourceID := req.SourceID
	// 只给 project_id 时从项目反推数据源；两者同时给出时必须一致。
	if req.ProjectID != nil {
		if *req.ProjectID <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "project_id 必须为正整数")
			return
		}
		project, err := o.Store.GetSyncProject(r.Context(), o.TenantID, *req.ProjectID)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		if sourceID > 0 && sourceID != project.SourceID {
			writeError(w, r, http.StatusBadRequest, "bad_request", "project_id 与 source_id 不匹配")
			return
		}
		sourceID = project.SourceID
	}

	source, err := o.Store.GetSyncSource(r.Context(), o.TenantID, sourceID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	// mode 只是显式声明预期模式：给了就必须与数据源类型一致，避免「以为在拉取、
	// 实际只是统计」的误操作。
	if mode := strings.TrimSpace(req.Mode); mode != "" && mode != source.Type {
		writeError(w, r, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("mode %q 与数据源类型 %q 不一致", mode, source.Type))
		return
	}

	run, err := o.Store.CreateSyncRun(r.Context(), o.TenantID, sourceID, req.ProjectID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	syncdata.New(o.Store, syncdata.ConfigFromEnv()).Start(r.Context(), o.TenantID, run)
	writeJSON(w, http.StatusAccepted, run)
}

// handleListSyncRuns 列出同步运行记录（id 倒序），支持 source_id/status/limit。
func (o Options) handleListSyncRuns(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	sourceID, ok := parseOptionalQueryID(w, r, "source_id", "source_id")
	if !ok {
		return
	}
	limit, ok := parseStrictLimit(w, r, defaultHTTPLimit, maxHTTPLimit)
	if !ok {
		return
	}
	runs, err := o.Store.ListSyncRuns(r.Context(), o.TenantID, pg.SyncRunFilter{
		SourceID: sourceID,
		Status:   strings.TrimSpace(r.URL.Query().Get("status")),
		Limit:    limit,
	})
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}
