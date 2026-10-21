package httpapi

import "net/http"

// 企业与「企业级资产范围」接口（M6.2-B1）。
//
// 响应约定与任务接口保持一致：列表用「资源名 + 数组」包一层，详情直接返回
// 对象；创建 201、删除 204；错误统一走 errorEnvelope（400/404/409/503）。

// companyCreateRequest 是创建企业的请求体。
type companyCreateRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// companyScopeCreateRequest 是新增企业范围条目的请求体。
type companyScopeCreateRequest struct {
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// handleListCompanies 列出企业，支持按名称/slug 模糊搜索与 limit 钳制。
func (o Options) handleListCompanies(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companies, err := o.Store.ListCompanies(r.Context(), o.TenantID,
		r.URL.Query().Get("q"), listLimitParam(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"companies": companies})
}

// handleCreateCompany 创建企业；slug 冲突返回 409。
func (o Options) handleCreateCompany(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	var req companyCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	company, err := o.Store.CreateCompany(r.Context(), o.TenantID, req.Name, req.Slug)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, company)
}

// handleGetCompany 读取单个企业。
func (o Options) handleGetCompany(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companyID, ok := o.companyID(w, r)
	if !ok {
		return
	}
	company, err := o.Store.GetCompany(r.Context(), o.TenantID, companyID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, company)
}

// handleUpdateCompany 局部更新企业：name/slug 非空才更新，attrs 支持整体替换/清空。
func (o Options) handleUpdateCompany(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companyID, ok := o.companyID(w, r)
	if !ok {
		return
	}
	patch, ok := parseCompanyPatch(w, r)
	if !ok {
		return
	}
	company, err := o.Store.PatchCompany(r.Context(), o.TenantID, companyID, patch)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, company)
}

// handleDeleteCompany 删除企业；其范围条目随外键级联删除。
func (o Options) handleDeleteCompany(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companyID, ok := o.companyID(w, r)
	if !ok {
		return
	}
	if err := o.Store.DeleteCompany(r.Context(), o.TenantID, companyID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListCompanyScope 列出企业的资产范围条目。
func (o Options) handleListCompanyScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companyID, ok := o.companyID(w, r)
	if !ok {
		return
	}
	scope, err := o.Store.ListCompanyScope(r.Context(), o.TenantID, companyID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": scope})
}

// handleAddCompanyScope 新增企业范围条目；同一 (kind, value) 重复返回 409。
func (o Options) handleAddCompanyScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companyID, ok := o.companyID(w, r)
	if !ok {
		return
	}
	var req companyScopeCreateRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	item, err := o.Store.AddCompanyScope(r.Context(), o.TenantID, companyID,
		req.Kind, req.Value, req.Source)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

// handleDeleteCompanyScope 删除企业的一条范围条目。
func (o Options) handleDeleteCompanyScope(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	companyID, ok := o.companyID(w, r)
	if !ok {
		return
	}
	scopeID, err := pathInt64Param(r, "scope_id", "企业范围")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if err := o.Store.DeleteCompanyScope(r.Context(), o.TenantID, companyID, scopeID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// companyID 解析并校验路径中的企业标识；非法时已写好 400 响应。
func (o Options) companyID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "企业")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}
