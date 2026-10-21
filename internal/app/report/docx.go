package report

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
)

// DOCX 生成最小可打开的 Word 文档（.docx）。
//
// 只使用标准库 archive/zip 拼装 OOXML 包：
//
//	[Content_Types].xml
//	_rels/.rels
//	word/document.xml
//	word/styles.xml
//	word/_rels/document.xml.rels
//
// 不嵌入图片/主题/字体，中文由 Word/WPS/LibreOffice 用系统字体渲染。段落使用
// 显式字号与加粗，不依赖内置样式名，避免不同软件对 styles.xml 的解释差异。
func DOCX(doc Document) ([]byte, error) {
	document, err := docxDocumentXML(doc)
	if err != nil {
		return nil, err
	}

	files := []struct {
		name string
		body string
	}{
		{"[Content_Types].xml", docxContentTypes},
		{"_rels/.rels", docxRootRels},
		{"word/document.xml", document},
		{"word/styles.xml", docxStyles},
		{"word/_rels/document.xml.rels", docxDocumentRels},
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, file := range files {
		w, err := zw.Create(file.name)
		if err != nil {
			return nil, fmt.Errorf("创建 DOCX 部件 %s 失败: %w", file.name, err)
		}
		if _, err := w.Write([]byte(file.body)); err != nil {
			return nil, fmt.Errorf("写入 DOCX 部件 %s 失败: %w", file.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("关闭 DOCX 包失败: %w", err)
	}
	return buf.Bytes(), nil
}

const docxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
<Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/>
</Types>`

const docxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>`

const docxDocumentRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
</Relationships>`

const docxStyles = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:styles xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:docDefaults>
<w:rPrDefault><w:rPr><w:rFonts w:ascii="Calibri" w:eastAsia="SimSun" w:hAnsi="Calibri"/><w:sz w:val="22"/></w:rPr></w:rPrDefault>
<w:pPrDefault><w:pPr><w:spacing w:after="120" w:line="276" w:lineRule="auto"/></w:pPr></w:pPrDefault>
</w:docDefaults>
<w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style>
</w:styles>`

// docxDocumentXML 把报告数据渲染成 word/document.xml。
func docxDocumentXML(doc Document) (string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n")
	b.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + "\n")

	b.WriteString(docxParagraph("任务报告："+doc.MissionName, true, 36))

	b.WriteString(docxParagraph("任务信息", true, 30))
	for _, field := range doc.Fields {
		b.WriteString(docxParagraph(field.Name+"："+field.Value, false, 22))
	}
	b.WriteString(docxParagraph("报告生成时间："+formatTime(doc.GeneratedAt), false, 22))

	b.WriteString(docxParagraph(fmt.Sprintf("发现（%d）", len(doc.Findings)), true, 30))
	if len(doc.Findings) == 0 {
		b.WriteString(docxParagraph("无：本次任务尚未登记发现。", false, 22))
	} else {
		for _, row := range doc.Findings {
			b.WriteString(docxParagraph(fmt.Sprintf("[%s][%s] %s", strings.ToUpper(row.Severity), row.Status, row.Title), false, 22))
		}
	}

	b.WriteString(docxParagraph(fmt.Sprintf("计划步骤（%d）", len(doc.Steps)), true, 30))
	if len(doc.Steps) == 0 {
		b.WriteString(docxParagraph("无：任务尚未生成计划步骤。", false, 22))
	} else {
		for _, row := range doc.Steps {
			b.WriteString(docxParagraph(fmt.Sprintf("%s：%s", row.Key, row.Status), false, 22))
		}
	}

	b.WriteString(docxParagraph(fmt.Sprintf("事实节点（%d）", doc.TotalFacts), true, 30))
	if doc.TotalFacts == 0 {
		b.WriteString(docxParagraph("无：任务尚未记录事实节点。", false, 22))
	} else {
		for i, fact := range doc.Facts {
			b.WriteString(docxParagraph(fmt.Sprintf("%d. %s", i+1, fact.Title), true, 24))
			if body := strings.TrimSpace(fact.Body); body != "" {
				b.WriteString(docxParagraph(truncateRunes(flatten(body), 240), false, 22))
			}
		}
		if doc.TotalFacts > len(doc.Facts) {
			b.WriteString(docxParagraph(fmt.Sprintf("仅展示前 %d 条事实节点（共 %d 条）。", len(doc.Facts), doc.TotalFacts), false, 20))
		}
	}

	b.WriteString(docxParagraph("证据统计", true, 30))
	b.WriteString(docxParagraph(fmt.Sprintf("流量采集记录：%d 条", doc.Evidence.TrafficCount), false, 22))
	b.WriteString(docxParagraph(fmt.Sprintf("证据快照：%d 条", doc.Evidence.SnapshotCount), false, 22))

	b.WriteString(`<w:sectPr><w:pgSz w:w="11906" w:h="16838"/>` +
		`<w:pgMar w:top="1440" w:right="1440" w:bottom="1440" w:left="1440"/></w:sectPr>` + "\n")
	b.WriteString("</w:body></w:document>\n")
	return b.String(), nil
}

// docxParagraph 生成一个带显式字号/加粗的段落；text 会被 XML 转义。
func docxParagraph(text string, bold bool, sizeHalfPoints int) string {
	var props strings.Builder
	props.WriteString("<w:p><w:pPr><w:spacing w:after=\"120\"/></w:pPr><w:r><w:rPr>")
	if bold {
		props.WriteString("<w:b/>")
	}
	fmt.Fprintf(&props, `<w:sz w:val="%d"/>`, sizeHalfPoints)
	props.WriteString("</w:rPr>")
	fmt.Fprintf(&props, `<w:t xml:space="preserve">%s</w:t></w:r></w:p>`, xmlEscape(text))
	return props.String() + "\n"
}

// xmlEscape 转义 XML 文本节点/属性中的保留字符。
func xmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}
