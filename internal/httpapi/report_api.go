package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/orzl1/BeaconX/internal/domain/graph"
	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// M6.2-B2：任务 Markdown 报告。
//
// 报告是「只读投影」：不改任何状态，只把任务元信息、发现、计划步骤、事实节点
// 与证据统计拼成 Markdown。所有数据都来自现有 Store 方法，生成过程不调用模型，
// 因此同一份数据总是得到同一份报告（除生成时间外）。

const (
	// reportFindingsLimit 是报告纳入的发现数量上限（远大于正常任务数量）。
	reportFindingsLimit = 500
	// reportFactsLimit 是报告渲染的事实节点上限：事实可能很多，报告只展示前
	// 100 条并标注总数，避免一次下载过大。
	reportFactsLimit = 100
	// reportFactBodyRunes 是单条事实正文的截断长度（按 rune 计）。
	reportFactBodyRunes = 240
)

// handleMissionReport 生成并返回任务的 Markdown 报告。
//
// 成功时 Content-Type 固定为 text/markdown; charset=utf-8；任务不存在返回 404
// （由 missionPathID 统一处理）。
func (o Options) handleMissionReport(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	missionID, ok := o.missionPathID(w, r)
	if !ok {
		return
	}
	// B5：多格式导出（html/docx/pdf）由 report_extra.go 接管；缺省或
	// format=markdown 时保持原有 Markdown 行为不变。
	if format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format"))); format != "" && format != "markdown" {
		o.handleMissionReportFormat(w, r, missionID, format)
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

	facts := make([]pg.Node, 0, 16)
	for _, node := range nodes {
		if node.Kind == graph.NodeFact {
			facts = append(facts, node)
		}
	}
	totalFacts := len(facts)
	if len(facts) > reportFactsLimit {
		facts = facts[:reportFactsLimit]
	}

	body := renderMissionReport(mission, findings, steps, facts, totalFacts, stats, time.Now().UTC())
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// renderMissionReport 把任务数据渲染成 Markdown。
//
// 空发现 / 空步骤 / 空事实都必须显式输出「无」段落，而不是整节省略——读者
// 需要区分「没有数据」与「报告生成漏了」。
func renderMissionReport(
	mission pg.Mission,
	findings []pg.Finding,
	steps []pg.PlanStep,
	facts []pg.Node,
	totalFacts int,
	stats pg.MissionEvidenceStats,
	generatedAt time.Time,
) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# 任务报告：%s\n\n", reportCell(mission.Name, 200))

	b.WriteString("## 任务信息\n\n")
	fmt.Fprintf(&b, "- 任务 ID：%d\n", mission.ID)
	fmt.Fprintf(&b, "- 任务名称：%s\n", reportCell(mission.Name, 200))
	fmt.Fprintf(&b, "- 任务目标：%s\n", reportCell(reportOrDefault(mission.Goal, "（未填写）"), 500))
	fmt.Fprintf(&b, "- 任务状态：%s\n", mission.Status)
	fmt.Fprintf(&b, "- 创建时间：%s\n", reportFormatTime(mission.CreatedAt))
	fmt.Fprintf(&b, "- 开始时间：%s\n", reportFormatTimePtr(mission.StartedAt))
	fmt.Fprintf(&b, "- 结束时间：%s\n", reportFormatTimePtr(mission.FinishedAt))
	fmt.Fprintf(&b, "- 截止时间：%s\n", reportFormatTimePtr(mission.DeadlineAt))
	fmt.Fprintf(&b, "- Token 预算：%s\n", reportBudget(mission.MaxTokens, mission.SpentTokens))
	fmt.Fprintf(&b, "- 步骤预算：%s\n", reportBudget(int64(mission.MaxSteps), int64(mission.StepsUsed)))
	fmt.Fprintf(&b, "- 报告生成时间：%s\n\n", reportFormatTime(generatedAt))

	fmt.Fprintf(&b, "## 发现（%d）\n\n", len(findings))
	if len(findings) == 0 {
		b.WriteString("无：本次任务尚未登记发现。\n\n")
	} else {
		b.WriteString("| 严重级别 | 状态 | 标题 |\n| --- | --- | --- |\n")
		for _, f := range findings {
			fmt.Fprintf(&b, "| %s | %s | %s |\n",
				strings.ToUpper(string(f.Severity)), reportCell(string(f.Status), 40), reportCell(f.Title, 200))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## 计划步骤（%d）\n\n", len(steps))
	if len(steps) == 0 {
		b.WriteString("无：任务尚未生成计划步骤。\n\n")
	} else {
		b.WriteString("| 步骤 | 状态 |\n| --- | --- |\n")
		for _, step := range steps {
			fmt.Fprintf(&b, "| %s | %s |\n", reportCell(step.Key, 64), reportCell(string(step.Status), 40))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## 事实节点（%d）\n\n", totalFacts)
	if totalFacts == 0 {
		b.WriteString("无：任务尚未记录事实节点。\n\n")
	} else {
		for i, fact := range facts {
			fmt.Fprintf(&b, "### %d. %s\n\n", i+1, reportCell(fact.Title, 200))
			if body := strings.TrimSpace(fact.Body); body != "" {
				fmt.Fprintf(&b, "%s\n\n", reportTruncateRunes(reportCell(body, 0), reportFactBodyRunes))
			}
		}
		if totalFacts > len(facts) {
			fmt.Fprintf(&b, "> 仅展示前 %d 条事实节点（共 %d 条）。\n\n", len(facts), totalFacts)
		}
	}

	b.WriteString("## 证据统计\n\n")
	fmt.Fprintf(&b, "- 流量采集记录：%d 条\n", stats.TrafficCount)
	fmt.Fprintf(&b, "- 证据快照：%d 条\n", stats.SnapshotCount)

	return b.String()
}

// reportBudget 渲染「预算（已用）」；上限为 0 表示不限制。
func reportBudget(max, used int64) string {
	if max <= 0 {
		return fmt.Sprintf("不限制（已用 %d）", used)
	}
	return fmt.Sprintf("%d（已用 %d）", max, used)
}

// reportCell 把任意文本压成可安全放进 Markdown 表格单元格的单行文本。
//
// 竖线会被转义，换行/回车折叠为空格；maxRunes > 0 时按 rune 截断。
func reportCell(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if maxRunes > 0 {
		s = reportTruncateRunes(s, maxRunes)
	}
	return s
}

// reportTruncateRunes 按 rune 截断并追加省略号，避免切碎多字节字符。
func reportTruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// reportOrDefault 返回非空文本；空白时返回兜底文案。
func reportOrDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// reportFormatTime 以 UTC 渲染时间，保证报告在不同部署时区下可比较。
func reportFormatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}

// reportFormatTimePtr 渲染可空时间；nil 显示为破折号。
func reportFormatTimePtr(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return reportFormatTime(*t)
}
