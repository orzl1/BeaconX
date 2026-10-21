package httpapi

import (
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/orzl1/BeaconX/internal/app/workspace"
)

// M6.2-B5 工作区文件接口（P-B5-24..28）。
//
// 路由通过 RegisterFeatureRoutes 在 init() 注册；任务隔离由 missions 表校验，
// 文件操作由 internal/app/workspace 负责（路径穿越/符号链接/大小限制）。
//
// 状态目录解析：BEACONX_HOME > config.state.dir > ./data，按请求解析，便于集成
// 测试用 t.Setenv 指向临时目录；服务实例按目录缓存（同目录复用）。
// 任务根目录：<state.dir>/tenants/<tenantID>/workspace/missions/<missionID>/。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/workspace/files", Summary: "工作区文件列表",
			Build: func(o Options) http.HandlerFunc { return o.handleWorkspaceList },
		},
		FeatureRoute{
			Method: http.MethodGet, Pattern: "/api/v1/workspace/files/{path...}", Summary: "读取工作区文件",
			Build: func(o Options) http.HandlerFunc { return o.handleWorkspaceRead },
		},
		FeatureRoute{
			Method: http.MethodPut, Pattern: "/api/v1/workspace/files/{path...}", Summary: "写入工作区文本文件",
			Build: func(o Options) http.HandlerFunc { return o.handleWorkspaceWrite },
		},
		FeatureRoute{
			Method: http.MethodPost, Pattern: "/api/v1/workspace/upload", Summary: "上传文件到工作区",
			Build: func(o Options) http.HandlerFunc { return o.handleWorkspaceUpload },
		},
		FeatureRoute{
			Method: http.MethodDelete, Pattern: "/api/v1/workspace/files/{path...}", Summary: "删除工作区文件/目录",
			Build: func(o Options) http.HandlerFunc { return o.handleWorkspaceDelete },
		},
	)
}

var (
	workspaceSvcMu  sync.Mutex
	workspaceSvc    *workspace.Service
	workspaceSvcDir string
)

// workspaceService 返回当前状态目录对应的服务；目录变化（测试注入）时重建。
func workspaceService() (*workspace.Service, error) {
	dir := workspace.DefaultStateDir()
	workspaceSvcMu.Lock()
	defer workspaceSvcMu.Unlock()
	if workspaceSvc != nil && workspaceSvcDir == dir {
		return workspaceSvc, nil
	}
	svc, err := workspace.New(dir)
	if err != nil {
		return nil, err
	}
	workspaceSvc, workspaceSvcDir = svc, dir
	return svc, nil
}

// workspaceMissionID 解析并校验 ?mission_id=，同时确认任务属于当前租户。
func (o Options) workspaceMissionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("mission_id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "mission_id 必须为正整数")
		return 0, false
	}
	if !o.requireMission(w, r, id) {
		return 0, false
	}
	return id, true
}

// handleWorkspaceList 递归列出任务工作区（最多 2000 项，可再按 limit 截断）。
func (o Options) handleWorkspaceList(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.workspaceMissionID(w, r)
	if !ok {
		return
	}
	svc, err := workspaceService()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "工作区未就绪")
		return
	}
	entries, err := svc.List(o.TenantID, missionID, r.URL.Query().Get("path"))
	if err != nil {
		writeWorkspaceError(w, r, err)
		return
	}
	limit := workspace.MaxListEntries
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n <= 0 {
			writeError(w, r, http.StatusBadRequest, "bad_request", "limit 必须为正整数")
			return
		}
		if n < limit {
			limit = n
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	// 配额信息是 additive 字段：前端可据此展示进度条；统计失败时降级为 0。
	used, usageErr := svc.Usage(o.TenantID, missionID)
	if usageErr != nil {
		used = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files":       entries,
		"root":        workspace.RootLabel(missionID),
		"quota_bytes": svc.QuotaBytes(),
		"used_bytes":  used,
	})
}

// handleWorkspaceRead 读取工作区文本文件。
func (o Options) handleWorkspaceRead(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.workspaceMissionID(w, r)
	if !ok {
		return
	}
	svc, err := workspaceService()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "工作区未就绪")
		return
	}
	rel := r.PathValue("path")
	data, err := svc.Read(o.TenantID, missionID, rel)
	if err != nil {
		writeWorkspaceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    workspacePathLabel(rel),
		"content": string(data),
	})
}

// handleWorkspaceWrite 写入 UTF-8 文本文件（≤1MB，含 NUL/非法 UTF-8 → 400）。
func (o Options) handleWorkspaceWrite(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.workspaceMissionID(w, r)
	if !ok {
		return
	}
	svc, err := workspaceService()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "工作区未就绪")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, workspace.MaxFileBytes+1)
	data, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		var maxErr *http.MaxBytesError
		if errors.As(readErr, &maxErr) || strings.Contains(readErr.Error(), "request body too large") {
			writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "文件超过 1MB 上限")
			return
		}
		writeError(w, r, http.StatusBadRequest, "bad_request", "读取请求体失败")
		return
	}
	if int64(len(data)) > workspace.MaxFileBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "文件超过 1MB 上限")
		return
	}
	entry, err := svc.Write(o.TenantID, missionID, r.PathValue("path"), data)
	if err != nil {
		writeWorkspaceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": entry.Path, "size": entry.Size})
}

// handleWorkspaceUpload 接收 multipart 上传（单文件 ≤10MB）。
//
// 表单字段：file（必填）；path（可选，落盘目录，相对任务根目录）。
func (o Options) handleWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.workspaceMissionID(w, r)
	if !ok {
		return
	}
	svc, err := workspaceService()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "工作区未就绪")
		return
	}
	// multipart 有边界与头部开销，比单文件上限多留 1MB。
	r.Body = http.MaxBytesReader(w, r.Body, workspace.MaxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(workspace.MaxUploadBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || strings.Contains(err.Error(), "request body too large") {
			writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "上传超过 10MB 上限")
			return
		}
		writeError(w, r, http.StatusBadRequest, "bad_request", "multipart 请求体不合法")
		return
	}
	// 兼容 file / files 两种表单字段名（前端组件与 curl 习惯不同）。
	file, header, err := r.FormFile("file")
	if err != nil {
		file, header, err = r.FormFile("files")
	}
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "缺少 file 字段")
		return
	}
	defer func() { _ = file.Close() }()
	if header.Size > workspace.MaxUploadBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "上传文件超过 10MB 上限")
		return
	}
	dir := r.FormValue("path")
	if strings.TrimSpace(dir) == "" {
		dir = r.FormValue("dir")
	}
	entry, err := svc.Upload(o.TenantID, missionID, dir, header.Filename, file, header.Size)
	if err != nil {
		writeWorkspaceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// handleWorkspaceDelete 删除文件或目录；非空目录需 ?recursive=true。
func (o Options) handleWorkspaceDelete(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.workspaceMissionID(w, r)
	if !ok {
		return
	}
	svc, err := workspaceService()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal_error", "工作区未就绪")
		return
	}
	recursive := false
	if raw := strings.TrimSpace(r.URL.Query().Get("recursive")); raw != "" {
		switch strings.ToLower(raw) {
		case "true", "1":
			recursive = true
		case "false", "0":
			recursive = false
		default:
			writeError(w, r, http.StatusBadRequest, "bad_request", "recursive 只能是 true 或 false")
			return
		}
	}
	if err := svc.Delete(o.TenantID, missionID, r.PathValue("path"), recursive); err != nil {
		writeWorkspaceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeWorkspaceError 把工作区语义错误映射为 HTTP 状态码。
func writeWorkspaceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, workspace.ErrInvalidPath),
		errors.Is(err, workspace.ErrBinary),
		errors.Is(err, workspace.ErrIsDir),
		errors.Is(err, workspace.ErrNotDir):
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, workspace.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, workspace.ErrDirNotEmpty):
		writeError(w, r, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, workspace.ErrQuota):
		writeError(w, r, http.StatusRequestEntityTooLarge, "quota_exceeded", err.Error())
	case errors.Is(err, workspace.ErrTooLarge):
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "工作区文件操作失败")
	}
}

// workspacePathLabel 返回对外展示的规范化相对路径（/ 分隔、去掉 ./ 前缀）。
func workspacePathLabel(rel string) string {
	label := path.Clean(strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/"))
	return strings.TrimPrefix(label, "./")
}
