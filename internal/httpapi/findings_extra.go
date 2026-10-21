package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/finding"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B4c：跨任务发现列表与 CSV 导出（P-B4-24）。
//
// 与任务内 `GET /api/v1/missions/{id}/findings` 并存：本文件的路由不带任务前缀，
// 用于「发现」总览页；过滤条件一致（mission_id/severity/status/asset_id），
// 并支持 after_id 游标与 limit 钳制。
//
// 路由通过 init() 注册，不改 handler.go。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method:  http.MethodGet,
			Pattern: "/api/v1/findings",
			Summary: "跨任务发现列表（?mission_id=&severity=&status=&asset_id=&after_id=&limit=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleListFindingsCross },
		},
		FeatureRoute{
			Method:  http.MethodGet,
			Pattern: "/api/v1/findings/export",
			Summary: "导出发现为 CSV（?format=csv&mission_id=&severity=&status=&asset_id=）",
			Build:   func(o Options) http.HandlerFunc { return o.handleExportFindingsCSV },
		},
	)
}

// findingsCSVHeader 是导出 CSV 的固定列顺序。
const findingsCSVHeader = "id,mission_id,title,severity,status,asset_id,created_at"

// handleListFindingsCross 跨任务列出发现（id 倒序）。
func (o Options) handleListFindingsCross(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	filter, limit, ok := o.findingCrossFilter(w, r, defaultHTTPLimit, maxHTTPLimit)
	if !ok {
		return
	}
	findings, err := o.Store.ListFindingsCross(r.Context(), o.TenantID, filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if findings == nil {
		findings = []pg.Finding{}
	}
	var lastID int64
	if len(findings) > 0 {
		lastID = findings[len(findings)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"findings":      findings,
		"next_after_id": cursorNext(len(findings), limit, lastID),
	})
}

// handleExportFindingsCSV 把发现导出为 CSV（字段固定、空结果仅表头）。
//
// 导出不复用分页游标：一次导出当前过滤条件下的全部匹配（最多 10000 条），
// 按 id 倒序。表头即使没有数据也会输出，便于前端直接下载空模板。
func (o Options) handleExportFindingsCSV(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format != "" && format != "csv" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "format 只支持 csv")
		return
	}
	filter, _, ok := o.findingCrossFilter(w, r, defaultHTTPLimit, maxHTTPLimit)
	if !ok {
		return
	}
	filter.AfterID = 0 // 导出不使用游标
	findings, err := o.Store.ExportFindings(r.Context(), o.TenantID, filter)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="findings.csv"`)
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write(strings.Split(findingsCSVHeader, ","))
	for _, item := range findings {
		assetID := ""
		if item.AssetID != nil {
			assetID = strconv.FormatInt(*item.AssetID, 10)
		}
		record := []string{
			strconv.FormatInt(item.ID, 10),
			strconv.FormatInt(item.MissionID, 10),
			csvSafe(item.Title),
			string(item.Severity),
			string(item.Status),
			assetID,
			item.CreatedAt.UTC().Format(time.RFC3339),
		}
		if err := cw.Write(record); err != nil {
			// 响应头已发出，无法再改状态码；记录中断即可，客户端会看到截断的文件。
			return
		}
	}
}

// csvSafe 降低 CSV 公式注入风险：以 = + - @ 开头的单元格在表格软件里会被当作公式，
// 前缀一个单引号让它按文本显示（与常见导出工具的做法一致）。
func csvSafe(value string) string {
	if value == "" {
		return ""
	}
	switch value[0] {
	case '=', '+', '-', '@':
		return "'" + value
	}
	return value
}

// findingCrossFilter 解析跨任务发现列表/导出的公共查询参数。
//
// 返回的 pg.FindingCrossFilter 中 Limit 已按 maxLimit 钳制；第二个返回值是
// 钳制后的 limit，供调用方计算 next_after_id。
func (o Options) findingCrossFilter(w http.ResponseWriter, r *http.Request, def, maxLimit int) (pg.FindingCrossFilter, int, bool) {
	query := r.URL.Query()
	missionID, ok := parseOptionalQueryID(w, r, "mission_id", "mission_id")
	if !ok {
		return pg.FindingCrossFilter{}, 0, false
	}
	assetID, ok := parseOptionalQueryID(w, r, "asset_id", "asset_id")
	if !ok {
		return pg.FindingCrossFilter{}, 0, false
	}
	afterID, ok := parseAfterID(w, r)
	if !ok {
		return pg.FindingCrossFilter{}, 0, false
	}
	severity := strings.TrimSpace(query.Get("severity"))
	if severity != "" && !finding.Severity(severity).Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("severity 不是合法的严重级别: %q", severity))
		return pg.FindingCrossFilter{}, 0, false
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && !finding.Status(status).Valid() {
		writeError(w, r, http.StatusBadRequest, "bad_request",
			fmt.Sprintf("status 不是合法的发现状态: %q", status))
		return pg.FindingCrossFilter{}, 0, false
	}
	limit, ok := parseStrictLimit(w, r, def, maxLimit)
	if !ok {
		return pg.FindingCrossFilter{}, 0, false
	}
	return pg.FindingCrossFilter{
		MissionID: missionID,
		Severity:  severity,
		Status:    status,
		AssetID:   assetID,
		AfterID:   afterID,
		Limit:     limit,
	}, limit, true
}
