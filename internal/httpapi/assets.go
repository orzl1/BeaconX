package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orzl1/BeaconX/internal/domain/asset"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// 资产、任务资产关联与覆盖度接口（M6.2-B1）。
//
// 本文件同时提供 M6.2-B1 各 handler 共用的 HTTP 小工具：
// store 就绪检查、路径/查询参数解析、请求体解码与 store 错误 → HTTP 状态码映射。

// 列表接口的 limit 约定：缺省 50，上限 200（与任务列表一致）。
const (
	defaultHTTPLimit = 50
	maxHTTPLimit     = 200
)

// assetCreateRequest 是创建资产的请求体。
//
// identities 可省略：省略时创建一条「暂无身份键」的资产（人工登记场景）；
// 提供时按身份键走 upsert 语义（同一身份键已存在则复用该资产）。
type assetCreateRequest struct {
	Type       string           `json:"type"`
	Label      string           `json:"label"`
	ParentID   *int64           `json:"parent_id"`
	Attrs      map[string]any   `json:"attrs"`
	Identities []asset.Identity `json:"identities"`
}

// assetPatchRequest 是更新资产的请求体。
//
// parent_id 用 json.RawMessage 承载，以区分三种意图：
// 字段缺省（nil）=保持不变；JSON null=清空父资产；数字=设置为该父资产。
type assetPatchRequest struct {
	Label    *string         `json:"label"`
	ParentID json.RawMessage `json:"parent_id"`
	Attrs    map[string]any  `json:"attrs"`
}

// missionAssetLinkRequest 是把资产登记到任务的请求体。
type missionAssetLinkRequest struct {
	AssetID int64  `json:"asset_id"`
	Source  string `json:"source"`
}

// missionAssetLinkResponse 是登记成功后的关联视图。
type missionAssetLinkResponse struct {
	MissionID int64  `json:"mission_id"`
	AssetID   int64  `json:"asset_id"`
	Source    string `json:"source"`
}

// ---------- 通用小工具 ----------

// storeReady 确认数据存储已装配；未就绪时返回 503，与任务接口的 runtime_unavailable 区分。
func (o Options) storeReady(w http.ResponseWriter, r *http.Request) bool {
	if o.Store == nil {
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", "数据存储未就绪")
		return false
	}
	return true
}

// pathInt64Param 解析路径参数为正整数；失败时返回带中文说明的 ErrInvalidInput。
func pathInt64Param(r *http.Request, name, label string) (int64, error) {
	raw := strings.TrimSpace(r.PathValue(name))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w：%s标识 %q 不合法", pg.ErrInvalidInput, label, raw)
	}
	return id, nil
}

// listLimitParam 读取 ?limit=，缺省 50、上限 200；非数字或非正数按缺省处理。
func listLimitParam(r *http.Request) int {
	limit := defaultHTTPLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxHTTPLimit {
		limit = maxHTTPLimit
	}
	return limit
}

// decodeJSONBody 以 1MB 上限解码请求体；失败时返回 400。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return false
	}
	return true
}

// writeStoreError 把 store 层错误映射为 HTTP 状态码与统一错误信封。
//
//	ErrNotFound / pgx.ErrNoRows            → 404 not_found
//	ErrConflict / ErrIdentityConflict      → 409 conflict
//	ErrInvalidInput / ErrInvalidTenant     → 400 bad_request
//	其它                                    → 500 internal_error（不泄漏内部细节）
func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pg.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, r, http.StatusNotFound, "not_found", fallbackMessage(err, "资源不存在"))
	case errors.Is(err, pg.ErrConflict), errors.Is(err, pg.ErrIdentityConflict):
		writeError(w, r, http.StatusConflict, "conflict", fallbackMessage(err, "资源冲突"))
	case errors.Is(err, pg.ErrInvalidInput), errors.Is(err, pg.ErrInvalidTenant):
		writeError(w, r, http.StatusBadRequest, "bad_request", fallbackMessage(err, "请求参数不合法"))
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "服务内部错误")
	}
}

// fallbackMessage 返回错误文本；文本为空时使用给定兜底文案。
func fallbackMessage(err error, fallback string) string {
	if err != nil {
		if msg := strings.TrimSpace(err.Error()); msg != "" {
			return msg
		}
	}
	return fallback
}

// normalizeIdentities 用领域包归一化客户端提交的身份键，保证同一资产不会因为
// 大小写、默认端口、结尾点等写法差异被写成多条身份。
func normalizeIdentities(raw []asset.Identity) ([]asset.Identity, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]asset.Identity, 0, len(raw))
	for i, item := range raw {
		kind := asset.Kind(strings.TrimSpace(string(item.Kind)))
		if !kind.Valid() {
			return nil, fmt.Errorf("%w：第 %d 个身份的类型 %q 非法", pg.ErrInvalidInput, i+1, string(item.Kind))
		}
		normalized, err := asset.Normalize(kind, item.Key)
		if err != nil {
			return nil, fmt.Errorf("%w：第 %d 个身份不合法（%v）", pg.ErrInvalidInput, i+1, err)
		}
		out = append(out, normalized)
	}
	return out, nil
}

// ---------- 资产 ----------

// handleListAssets 按 label 模糊 + type 精确列出资产。
func (o Options) handleListAssets(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	filter := pg.AssetSearch{Limit: listLimitParam(r), Query: r.URL.Query().Get("q")}
	if rawType := strings.TrimSpace(r.URL.Query().Get("type")); rawType != "" {
		kind := asset.Kind(rawType)
		if !kind.Valid() {
			writeError(w, r, http.StatusBadRequest, "bad_request", fmt.Sprintf("非法资产类型 %q", rawType))
			return
		}
		filter.Type = &kind
	}
	assets, err := o.Store.SearchAssets(r.Context(), o.TenantID, filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": assets})
}

// handleCreateAsset 创建资产：带身份键时走 upsert，省略身份键时创建裸资产。
func (o Options) handleCreateAsset(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req assetCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	kind := asset.Kind(strings.TrimSpace(req.Type))
	if !kind.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("非法资产类型 %q（允许：root_domain/subdomain/ip/service/app/endpoint）", req.Type))
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "资产标签不能为空")
		return
	}
	if req.ParentID != nil && *req.ParentID <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "父资产标识必须为正数")
		return
	}
	identities, err := normalizeIdentities(req.Identities)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	in := pg.AssetInput{
		Type:       kind,
		Label:      label,
		ParentID:   req.ParentID,
		Attrs:      req.Attrs,
		Identities: identities,
	}
	var created pg.Asset
	if len(identities) > 0 {
		// 身份键命中已有资产时复用该资产（幂等 upsert），并补充/更新标签与属性。
		created, err = o.Store.UpsertAsset(r.Context(), o.TenantID, in)
	} else {
		created, err = o.Store.CreateAsset(r.Context(), o.TenantID, in)
	}
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleGetAsset 读取单个资产（含身份键，便于控制台直接展示）。
func (o Options) handleGetAsset(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	assetID, ok := o.assetID(w, r)
	if !ok {
		return
	}
	item, err := o.Store.GetAsset(r.Context(), o.TenantID, assetID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// handleUpdateAsset 局部更新资产：label / parent_id / attrs 均可选。
func (o Options) handleUpdateAsset(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	assetID, ok := o.assetID(w, r)
	if !ok {
		return
	}
	var req assetPatchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	update := pg.AssetUpdate{Label: req.Label, Attrs: req.Attrs}
	if req.ParentID != nil {
		trimmed := bytes.TrimSpace(req.ParentID)
		if string(trimmed) == "null" {
			update.ClearParent = true
		} else {
			var parentID int64
			if err := json.Unmarshal(trimmed, &parentID); err != nil || parentID <= 0 {
				writeError(w, r, http.StatusBadRequest, "bad_request", "父资产标识必须为正整数或 null")
				return
			}
			update.ParentID = &parentID
		}
	}
	item, err := o.Store.UpdateAsset(r.Context(), o.TenantID, assetID, update)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// handleDeleteAsset 删除资产；身份键与任务关联级联删除，子资产的 parent_id 置空。
func (o Options) handleDeleteAsset(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	assetID, ok := o.assetID(w, r)
	if !ok {
		return
	}
	if err := o.Store.DeleteAsset(r.Context(), o.TenantID, assetID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListAssetIdentities 列出资产的全部身份键。
func (o Options) handleListAssetIdentities(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	assetID, ok := o.assetID(w, r)
	if !ok {
		return
	}
	identities, err := o.Store.ListAssetIdentities(r.Context(), o.TenantID, assetID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": identities})
}

// assetID 解析路径中的资产标识。
func (o Options) assetID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "资产")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// ---------- 任务资产与覆盖度 ----------

// handleListMissionAssets 列出任务已登记的资产。
func (o Options) handleListMissionAssets(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	assets, err := o.Store.ListMissionAssets(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": assets})
}

// handleAddMissionAsset 把资产手动登记到任务；重复登记返回 409。
func (o Options) handleAddMissionAsset(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	var req missionAssetLinkRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.AssetID <= 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "asset_id 必须为正整数")
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "manual"
	}
	if err := o.Store.AddMissionAssetLink(r.Context(), o.TenantID, missionID, req.AssetID, source); err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, missionAssetLinkResponse{
		MissionID: missionID,
		AssetID:   req.AssetID,
		Source:    source,
	})
}

// handleRemoveMissionAsset 解除任务与资产的关联。
func (o Options) handleRemoveMissionAsset(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	assetID, err := pathInt64Param(r, "asset_id", "资产")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.RemoveMissionAssetLink(r.Context(), o.TenantID, missionID, assetID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMissionCoverage 返回任务资产覆盖度；任务无资产时返回全零而不是报错。
func (o Options) handleMissionCoverage(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	coverage, err := o.Store.Coverage(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, coverage)
}

// missionPathID 解析路径中的任务标识，并确认任务存在（不存在返回 404）。
//
// 这样任务资产/覆盖度接口不会在「任务不存在」时静默返回空列表或全零覆盖度。
func (o Options) missionPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "任务")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	if _, err := o.Store.GetMission(r.Context(), o.TenantID, id); err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}
