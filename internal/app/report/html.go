package report

import (
	"fmt"
	"html"
	"strings"
)

// HTML 渲染自包含的 HTML 报告（内联样式，不依赖外部资源）。
//
// 所有动态文本都经过 html.EscapeString；发现/步骤用表格，事实用列表。
func HTML(doc Document) string {
	var b strings.Builder
	esc := html.EscapeString

	b.WriteString("<!DOCTYPE html>\n<html lang=\"zh-CN\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	fmt.Fprintf(&b, "<title>任务报告：%s</title>\n", esc(doc.MissionName))
	b.WriteString("<style>body{font-family:-apple-system,\"Segoe UI\",\"Noto Sans CJK SC\",sans-serif;")
	b.WriteString("max-width:960px;margin:32px auto;padding:0 20px;color:#1f2328;line-height:1.6}")
	b.WriteString("h1{border-bottom:1px solid #d0d7de;padding-bottom:.3em}")
	b.WriteString("table{border-collapse:collapse;width:100%;margin:8px 0 16px}")
	b.WriteString("th,td{border:1px solid #d0d7de;padding:6px 10px;text-align:left}")
	b.WriteString("th{background:#f6f8fa}code{background:#f6f8fa;padding:1px 4px;border-radius:4px}")
	b.WriteString("</style>\n</head>\n<body>\n")

	fmt.Fprintf(&b, "<h1>任务报告：%s</h1>\n", esc(doc.MissionName))

	b.WriteString("<h2>任务信息</h2>\n<ul>\n")
	for _, field := range doc.Fields {
		fmt.Fprintf(&b, "<li><strong>%s：</strong>%s</li>\n", esc(field.Name), esc(field.Value))
	}
	fmt.Fprintf(&b, "<li><strong>报告生成时间：</strong>%s</li>\n", esc(formatTime(doc.GeneratedAt)))
	b.WriteString("</ul>\n")

	fmt.Fprintf(&b, "<h2>发现（%d）</h2>\n", len(doc.Findings))
	if len(doc.Findings) == 0 {
		b.WriteString("<p>无：本次任务尚未登记发现。</p>\n")
	} else {
		b.WriteString("<table><thead><tr><th>严重级别</th><th>状态</th><th>标题</th></tr></thead><tbody>\n")
		for _, row := range doc.Findings {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td></tr>\n",
				esc(strings.ToUpper(row.Severity)), esc(row.Status), esc(row.Title))
		}
		b.WriteString("</tbody></table>\n")
	}

	fmt.Fprintf(&b, "<h2>计划步骤（%d）</h2>\n", len(doc.Steps))
	if len(doc.Steps) == 0 {
		b.WriteString("<p>无：任务尚未生成计划步骤。</p>\n")
	} else {
		b.WriteString("<table><thead><tr><th>步骤</th><th>状态</th></tr></thead><tbody>\n")
		for _, row := range doc.Steps {
			fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td></tr>\n", esc(row.Key), esc(row.Status))
		}
		b.WriteString("</tbody></table>\n")
	}

	fmt.Fprintf(&b, "<h2>事实节点（%d）</h2>\n", doc.TotalFacts)
	if doc.TotalFacts == 0 {
		b.WriteString("<p>无：任务尚未记录事实节点。</p>\n")
	} else {
		for i, fact := range doc.Facts {
			fmt.Fprintf(&b, "<h3>%d. %s</h3>\n", i+1, esc(fact.Title))
			if body := strings.TrimSpace(fact.Body); body != "" {
				fmt.Fprintf(&b, "<p>%s</p>\n", esc(truncateRunes(flatten(body), 240)))
			}
		}
		if doc.TotalFacts > len(doc.Facts) {
			fmt.Fprintf(&b, "<p><em>仅展示前 %d 条事实节点（共 %d 条）。</em></p>\n", len(doc.Facts), doc.TotalFacts)
		}
	}

	b.WriteString("<h2>证据统计</h2>\n<ul>\n")
	fmt.Fprintf(&b, "<li>流量采集记录：%d 条</li>\n", doc.Evidence.TrafficCount)
	fmt.Fprintf(&b, "<li>证据快照：%d 条</li>\n", doc.Evidence.SnapshotCount)
	b.WriteString("</ul>\n</body>\n</html>\n")

	return b.String()
}
