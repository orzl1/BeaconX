package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/orzl1/BeaconX/internal/domain/asset"
)

// B4c：资产身份键独立写删、任务范围编译预览，以及游标分页的公共工具。
//
// 路由通过 init() 里的 RegisterFeatureRoutes 注册（Build 工厂在 NewHandler 时
// 注入 Options），因此本文件不需要修改 handler.go 的路由表。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method:  http.MethodPost,
			Pattern: "/api/v1/assets/{id}/identities",
			Summary: "新增资产身份键（归一化后写入）",
			Build:   func(o Options) http.HandlerFunc { return o.handleAddAssetIdentity },
		},
		FeatureRoute{
			Method:  http.MethodDelete,
			Pattern: "/api/v1/assets/{id}/identities/{kind}/{key...}",
			Summary: "删除资产身份键",
			Build:   func(o Options) http.HandlerFunc { return o.handleDeleteAssetIdentity },
		},
		FeatureRoute{
			Method:  http.MethodPost,
			Pattern: "/api/v1/missions/{id}/scope/compile",
			Summary: "把任务范围编译为执行期策略草案（预览，不落库）",
			Build:   func(o Options) http.HandlerFunc { return o.handleCompileMissionScope },
		},
	)
}

// assetIdentityCreateRequest 是新增身份键的请求体。
type assetIdentityCreateRequest struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

// handleAddAssetIdentity 给资产追加一条身份键。
//
// 身份键先经 domain/asset.Normalize 归一化（大小写、默认端口、尾点等写法差异
// 收敛为同一键），再写入；重复身份返回 409。
func (o Options) handleAddAssetIdentity(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	assetID, ok := o.assetID(w, r)
	if !ok {
		return
	}
	var req assetIdentityCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	kind := asset.Kind(strings.TrimSpace(req.Kind))
	if !kind.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("非法身份类型 %q（允许：root_domain/subdomain/ip/service/app/endpoint）", req.Kind))
		return
	}
	normalized, err := asset.Normalize(kind, req.Key)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	created, err := o.Store.AddAssetIdentity(r.Context(), o.TenantID, assetID, normalized)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleDeleteAssetIdentity 删除资产的一条身份键。
//
// 路径中的 key 需要由客户端做一次 URL 编码（例如 URL 身份键里的 "://" 要写成
// "%3A%2F%2F"），ServeMux 的 {key...} 通配符负责解码成原始键；这里再用
// domain/asset.Normalize 归一化，保证与写入时的键一致。
func (o Options) handleDeleteAssetIdentity(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	assetID, ok := o.assetID(w, r)
	if !ok {
		return
	}
	kind := asset.Kind(strings.TrimSpace(r.PathValue("kind")))
	if !kind.Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("非法身份类型 %q", r.PathValue("kind")))
		return
	}
	rawKey := r.PathValue("key")
	if strings.TrimSpace(rawKey) == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "身份键不能为空")
		return
	}
	normalized, err := asset.Normalize(kind, rawKey)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := o.Store.DeleteAssetIdentity(r.Context(), o.TenantID, assetID, normalized); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleCompileMissionScope 返回任务范围的执行期策略草案（预览，不落库）。
func (o Options) handleCompileMissionScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, err := pathInt64Param(r, "id", "任务")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	compiled, err := o.Store.CompileMissionScope(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, compiled)
}

// ---------- 游标分页公共工具 ----------

// parseAfterID 解析 ?after_id=：缺省或 0 表示不启用游标；非数字或负数返回 400。
//
// 与 limit 不同，这里不做静默回退：游标写错意味着前端翻页逻辑有 bug，
// 静默当成首页会让人看到重复数据而难以察觉。
func parseAfterID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("after_id"))
	if raw == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 0 {
		writeError(w, r, http.StatusBadRequest, "bad_request", "after_id 必须为非负整数")
		return 0, false
	}
	return id, true
}

// cursorNext 计算下一页游标：本页没取满（或为空）说明没有下一页，返回 0。
//
// 本页取满时用最后一条的 id 作为边界；调用方下一页会多取一次空结果并自然把
// next_after_id 归零，因此不需要额外的 count 查询。
func cursorNext(count, limit int, lastID int64) int64 {
	if count == 0 || count < limit || lastID <= 0 {
		return 0
	}
	return lastID
}
