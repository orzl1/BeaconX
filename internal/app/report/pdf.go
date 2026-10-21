package report

import (
	"bytes"
	"fmt"
	"strings"
)

// PDF 生成最小可打开的 PDF 1.4 文本报告（无第三方依赖、不嵌入字体文件）。
//
// 中文方案：使用 Adobe 标准 CJK 字体引用 STSong-Light + UniGB-UCS2-H 编码，
// 文本以 UTF-16BE 十六进制串写入内容流。PDF 阅读器会用自带的 CJK 字体替换
// 渲染（Acrobat 需安装亚洲字体包；macOS Preview/Chrome/WPS/福昕通常可替换）。
//
// 这是有意的取舍：
//   - 不嵌入字体 → 文件小、无字体授权问题、生成路径完全确定性；
//   - 代价 → 当阅读器完全没有 CJK 字体时中文会显示为缺字方块，但文件本身是
//     合法 PDF，可打开、可复制、可打印。
//
// 页面为 A4（595×842）、单栏、固定行距；按行数自动分页。
const (
	pdfPageWidth    = 595
	pdfPageHeight   = 842
	pdfMarginX      = 50
	pdfStartY       = 792
	pdfFontSize     = 11
	pdfLeading      = 16
	pdfLinesPerPage = 44
	pdfMaxRunes     = 46
)

// PDF 把报告渲染成 PDF 字节流。
func PDF(doc Document) ([]byte, error) {
	pages := paginateLines(wrapLines(pdfTextLines(doc), pdfMaxRunes), pdfLinesPerPage)
	if len(pages) == 0 {
		pages = [][]string{{""}}
	}

	pageObjStart := 3
	contentObjStart := pageObjStart + len(pages)
	fontType0 := contentObjStart + len(pages)
	cidFont := fontType0 + 1
	descriptor := fontType0 + 2
	totalObjects := descriptor

	kids := make([]string, 0, len(pages))
	contents := make(map[int]string, len(pages))
	pageObjects := make(map[int]string, len(pages))
	for i, lines := range pages {
		pageNum := pageObjStart + i
		contentNum := contentObjStart + i
		kids = append(kids, fmt.Sprintf("%d 0 R", pageNum))
		contents[contentNum] = pdfContentStream(lines)
		pageObjects[pageNum] = fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] "+
				"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>",
			pdfPageWidth, pdfPageHeight, fontType0, contentNum)
	}

	objects := make(map[int]string, totalObjects)
	objects[1] = "<< /Type /Catalog /Pages 2 0 R >>"
	objects[2] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages))
	for num, body := range pageObjects {
		objects[num] = body
	}
	for num, stream := range contents {
		objects[num] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)
	}
	objects[fontType0] = fmt.Sprintf(
		"<< /Type /Font /Subtype /Type0 /BaseFont /STSong-Light "+
			"/Encoding /UniGB-UCS2-H /DescendantFonts [%d 0 R] >>", cidFont)
	objects[cidFont] = fmt.Sprintf(
		"<< /Type /Font /Subtype /CIDFontType0 /BaseFont /STSong-Light "+
			"/CIDSystemInfo << /Registry (Adobe) /Ordering (GB1) /Supplement 4 >> "+
			"/FontDescriptor %d 0 R /DW 1000 >>", descriptor)
	objects[descriptor] = "<< /Type /FontDescriptor /FontName /STSong-Light /Flags 6 " +
		"/FontBBox [-25 -254 1000 880] /ItalicAngle 0 /Ascent 880 /Descent -120 " +
		"/CapHeight 880 /StemV 93 >>"

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")
	offsets := make([]int, totalObjects+1)
	for num := 1; num <= totalObjects; num++ {
		body, ok := objects[num]
		if !ok {
			return nil, fmt.Errorf("PDF 对象 %d 缺失", num)
		}
		offsets[num] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", num, body)
	}
	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", totalObjects+1)
	buf.WriteString("0000000000 65535 f \n")
	for num := 1; num <= totalObjects; num++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[num])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", totalObjects+1, xrefOffset)
	return buf.Bytes(), nil
}

// pdfTextLines 把报告数据摊平成 PDF 文本行（不含自动换行）。
func pdfTextLines(doc Document) []string {
	lines := []string{"任务报告：" + doc.MissionName, ""}
	lines = append(lines, "一、任务信息")
	for _, field := range doc.Fields {
		lines = append(lines, field.Name+"："+field.Value)
	}
	lines = append(lines, "报告生成时间："+formatTime(doc.GeneratedAt), "")

	lines = append(lines, fmt.Sprintf("二、发现（%d）", len(doc.Findings)))
	if len(doc.Findings) == 0 {
		lines = append(lines, "无：本次任务尚未登记发现。")
	} else {
		for _, row := range doc.Findings {
			lines = append(lines, fmt.Sprintf("[%s][%s] %s", strings.ToUpper(row.Severity), row.Status, row.Title))
		}
	}
	lines = append(lines, "")

	lines = append(lines, fmt.Sprintf("三、计划步骤（%d）", len(doc.Steps)))
	if len(doc.Steps) == 0 {
		lines = append(lines, "无：任务尚未生成计划步骤。")
	} else {
		for _, row := range doc.Steps {
			lines = append(lines, fmt.Sprintf("%s：%s", row.Key, row.Status))
		}
	}
	lines = append(lines, "")

	lines = append(lines, fmt.Sprintf("四、事实节点（%d）", doc.TotalFacts))
	if doc.TotalFacts == 0 {
		lines = append(lines, "无：任务尚未记录事实节点。")
	} else {
		for i, fact := range doc.Facts {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, fact.Title))
			if body := strings.TrimSpace(fact.Body); body != "" {
				lines = append(lines, truncateRunes(flatten(body), 240))
			}
		}
		if doc.TotalFacts > len(doc.Facts) {
			lines = append(lines, fmt.Sprintf("仅展示前 %d 条事实节点（共 %d 条）。", len(doc.Facts), doc.TotalFacts))
		}
	}
	lines = append(lines, "")

	lines = append(lines, "五、证据统计",
		fmt.Sprintf("流量采集记录：%d 条", doc.Evidence.TrafficCount),
		fmt.Sprintf("证据快照：%d 条", doc.Evidence.SnapshotCount))
	return lines
}

// wrapLines 把每行按 rune 上限折行；空行保留（用于段落间隔）。
func wrapLines(lines []string, maxRunes int) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		runes := []rune(line)
		if len(runes) == 0 {
			out = append(out, "")
			continue
		}
		for len(runes) > maxRunes {
			out = append(out, string(runes[:maxRunes]))
			runes = runes[maxRunes:]
		}
		out = append(out, string(runes))
	}
	return out
}

// paginateLines 按固定行数分页；最后一页允许不满。
func paginateLines(lines []string, perPage int) [][]string {
	if perPage <= 0 {
		perPage = pdfLinesPerPage
	}
	var pages [][]string
	for len(lines) > 0 {
		n := perPage
		if len(lines) < n {
			n = len(lines)
		}
		pages = append(pages, lines[:n])
		lines = lines[n:]
	}
	return pages
}

// pdfContentStream 生成一页的内容流；文本统一用 UTF-16BE 十六进制串，
// 由 UniGB-UCS2-H 编码映射到 STSong-Light 的 CJK 字形。
func pdfContentStream(lines []string) string {
	var b strings.Builder
	b.WriteString("BT\n")
	fmt.Fprintf(&b, "/F1 %d Tf\n", pdfFontSize)
	fmt.Fprintf(&b, "%d TL\n", pdfLeading)
	fmt.Fprintf(&b, "%d %d Td\n", pdfMarginX, pdfStartY)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			b.WriteString("T*\n")
			continue
		}
		b.WriteString(pdfHexText(line))
		b.WriteString(" Tj\nT*\n")
	}
	b.WriteString("ET")
	return b.String()
}

// pdfHexText 把字符串编码为 PDF 十六进制文本串（UTF-16BE，BMP 字符）。
//
// 非 BMP 字符（emoji 等）GB1 没有对应字形，统一降级为 '?'，避免出现无效码位。
func pdfHexText(s string) string {
	var b strings.Builder
	b.WriteString("<")
	for _, r := range s {
		if r > 0xFFFF {
			r = '?'
		}
		fmt.Fprintf(&b, "%04X", r)
	}
	b.WriteString(">")
	return b.String()
}
