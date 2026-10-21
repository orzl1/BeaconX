package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B4c：企业 PATCH 扩展 attrs（P-B4-22）。
//
// 既有 PATCH 只接受 name/slug；这里把请求体扩展为可选的 attrs。attrs 用
// json.RawMessage 承载三态：字段缺省=保持不变；JSON 对象=整体替换；JSON null=清空为 {}。
// 空字符串的 name/slug 仍然表示「保持不变」，与 B1 的语义保持一致。

// companyPatchRequest 是更新企业的请求体。
type companyPatchRequest struct {
	Name  *string         `json:"name"`
	Slug  *string         `json:"slug"`
	Attrs json.RawMessage `json:"attrs"`
}

// parseCompanyPatch 解析 PATCH 请求体；非法 attrs 直接写 400 并返回 false。
func parseCompanyPatch(w http.ResponseWriter, r *http.Request) (pg.CompanyPatch, bool) {
	var req companyPatchRequest
	if !decodeJSONBody(w, r, &req) {
		return pg.CompanyPatch{}, false
	}
	out := pg.CompanyPatch{}
	// 空串视为「没传」：保持 B1「空值不变」的兼容语义。
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		out.Name = req.Name
	}
	if req.Slug != nil && strings.TrimSpace(*req.Slug) != "" {
		out.Slug = req.Slug
	}
	if len(req.Attrs) > 0 {
		raw := bytes.TrimSpace(req.Attrs)
		if bytes.Equal(raw, []byte("null")) {
			out.ClearAttrs = true
		} else {
			var attrs map[string]any
			if err := json.Unmarshal(raw, &attrs); err != nil || attrs == nil {
				writeError(w, r, http.StatusBadRequest, "bad_request", "attrs 必须是 JSON 对象或 null")
				return pg.CompanyPatch{}, false
			}
			out.Attrs = attrs
		}
	}
	return out, true
}
