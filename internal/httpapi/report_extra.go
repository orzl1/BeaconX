package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/app/report"
	"github.com/orzl1/BeaconX/internal/domain/graph"
)

// B5：任务报告的多格式导出（P-B5-40）。
//
// 既有 GET /api/v1/missions/{id}/report 保持 Markdown 默认行为；?format=html|docx|pdf
// 由本文件接管（report_api.go 里仅做一次格式分发，避免重复实现数据组装）。
//
// 渲染全部由 internal/app/report 完成（零第三方依赖）：
//   - html：自包含 HTML（内联样式、转义动态文本）；
//   - docx：标准库 zip 拼最小 OOXML，Word/WPS/LibreOffice 可打开；
//   - pdf ：最小 PDF 1.4，中文用 STSong-Light + UniGB-UCS2-H（引用而非嵌入字体，
//     阅读器用自带 CJK 字体替换渲染；无 CJK 字体时会缺字但文件合法）。

// handleMissionReportFormat 按 format 渲染并下载报告。
func (o Options) handleMissionReportFormat(w http.ResponseWriter, r *http.Request, missionID int64, format string) {
	if !o.storeReady(w, r) {
		return
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if format != "html" && format != "docx" && format != "pdf" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "format 必须是 markdown、html、docx 或 pdf")
		return
	}

	mission, err := o.Store.GetMission(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	findings, err := o.Store.ListFindings(r.Context(), o.TenantID, missionID, "", "", reportFindingsLimit)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	steps, err := o.Store.ListPlanSteps(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	nodes, err := o.Store.ListNodes(r.Context(), o.TenantID, missionID, 1000)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	stats, err := o.Store.MissionEvidenceStats(r.Context(), o.TenantID, missionID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}

	facts := make([]report.Fact, 0, 16)
	for _, node := range nodes {
		if node.Kind != graph.NodeFact {
			continue
		}
		facts = append(facts, report.Fact{Title: node.Title, Body: node.Body})
	}
	totalFacts := len(facts)
	if len(facts) > reportFactsLimit {
		facts = facts[:reportFactsLimit]
	}

	doc := report.Document{
		MissionName: mission.Name,
		MissionID:   mission.ID,
		GeneratedAt: time.Now().UTC(),
		Fields: []report.Field{
			{Name: "任务 ID", Value: strconv.FormatInt(mission.ID, 10)},
			{Name: "任务名称", Value: mission.Name},
			{Name: "任务目标", Value: reportOrDefault(mission.Goal, "（未填写）")},
			{Name: "任务状态", Value: mission.Status},
			{Name: "创建时间", Value: reportFormatTime(mission.CreatedAt)},
			{Name: "开始时间", Value: reportFormatTimePtr(mission.StartedAt)},
			{Name: "结束时间", Value: reportFormatTimePtr(mission.FinishedAt)},
			{Name: "截止时间", Value: reportFormatTimePtr(mission.DeadlineAt)},
			{Name: "Token 预算", Value: reportBudget(mission.MaxTokens, mission.SpentTokens)},
			{Name: "步骤预算", Value: reportBudget(int64(mission.MaxSteps), int64(mission.StepsUsed))},
		},
		Findings:   make([]report.FindingRow, 0, len(findings)),
		Steps:      make([]report.StepRow, 0, len(steps)),
		Facts:      facts,
		TotalFacts: totalFacts,
		Evidence: report.EvidenceStats{
			TrafficCount:  stats.TrafficCount,
			SnapshotCount: stats.SnapshotCount,
		},
	}
	for _, item := range findings {
		doc.Findings = append(doc.Findings, report.FindingRow{
			Severity: string(item.Severity), Status: string(item.Status), Title: item.Title,
		})
	}
	for _, step := range steps {
		doc.Steps = append(doc.Steps, report.StepRow{Key: step.Key, Status: string(step.Status)})
	}

	switch format {
	case "html":
		body := report.HTML(doc)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"mission-%d-report.html\"", missionID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	case "docx":
		body, err := report.DOCX(doc)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "生成 DOCX 失败")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.wordprocessingml.document")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"mission-%d-report.docx\"", missionID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case "pdf":
		body, err := report.PDF(doc)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "生成 PDF 失败")
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"mission-%d-report.pdf\"", missionID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
