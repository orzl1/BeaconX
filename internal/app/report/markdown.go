package report

import (
	"fmt"
	"strings"
	"time"
)

// Markdown 渲染 Markdown 报告。
//
// 与 HTTP 层既有的 /report 输出保持同样的章节结构，便于前端/导出物对照；
// 这里的实现独立于 httpapi，方便单元测试与后续复用（例如归档包内附带报告）。
func Markdown(doc Document) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# 任务报告：%s\n\n", doc.MissionName)

	b.WriteString("## 任务信息\n\n")
	for _, field := range doc.Fields {
		fmt.Fprintf(&b, "- %s：%s\n", field.Name, field.Value)
	}
	fmt.Fprintf(&b, "- 报告生成时间：%s\n\n", formatTime(doc.GeneratedAt))

	fmt.Fprintf(&b, "## 发现（%d）\n\n", len(doc.Findings))
	if len(doc.Findings) == 0 {
		b.WriteString("无：本次任务尚未登记发现。\n\n")
	} else {
		b.WriteString("| 严重级别 | 状态 | 标题 |\n| --- | --- | --- |\n")
		for _, row := range doc.Findings {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", strings.ToUpper(row.Severity), cell(row.Status), cell(row.Title))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## 计划步骤（%d）\n\n", len(doc.Steps))
	if len(doc.Steps) == 0 {
		b.WriteString("无：任务尚未生成计划步骤。\n\n")
	} else {
		b.WriteString("| 步骤 | 状态 |\n| --- | --- |\n")
		for _, row := range doc.Steps {
			fmt.Fprintf(&b, "| %s | %s |\n", cell(row.Key), cell(row.Status))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## 事实节点（%d）\n\n", doc.TotalFacts)
	if doc.TotalFacts == 0 {
		b.WriteString("无：任务尚未记录事实节点。\n\n")
	} else {
		for i, fact := range doc.Facts {
			fmt.Fprintf(&b, "### %d. %s\n\n", i+1, cell(fact.Title))
			if body := strings.TrimSpace(fact.Body); body != "" {
				fmt.Fprintf(&b, "%s\n\n", truncateRunes(flatten(body), 240))
			}
		}
		if doc.TotalFacts > len(doc.Facts) {
			fmt.Fprintf(&b, "> 仅展示前 %d 条事实节点（共 %d 条）。\n\n", len(doc.Facts), doc.TotalFacts)
		}
	}

	b.WriteString("## 证据统计\n\n")
	fmt.Fprintf(&b, "- 流量采集记录：%d 条\n", doc.Evidence.TrafficCount)
	fmt.Fprintf(&b, "- 证据快照：%d 条\n", doc.Evidence.SnapshotCount)

	return b.String()
}

// formatTime 统一报告时间格式（UTC，跨部署可比较）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}

// cell 把文本压成可放进 Markdown 表格单元格的单行文本。
func cell(s string) string {
	s = flatten(s)
	return strings.ReplaceAll(s, "|", `\|`)
}

// flatten 把多行文本折叠为单行（用于表格/摘要类字段）。
func flatten(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// truncateRunes 按 rune 截断并追加省略号，避免切碎多字节字符。
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
