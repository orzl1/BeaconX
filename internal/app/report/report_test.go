package report

import (
	"archive/zip"
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sampleDocument() Document {
	return Document{
		MissionName: "季度 <报告> & 复测",
		MissionID:   7,
		GeneratedAt: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC),
		Fields: []Field{
			{Name: "任务 ID", Value: "7"},
			{Name: "任务状态", Value: "done"},
		},
		Findings: []FindingRow{
			{Severity: "critical", Status: "confirmed", Title: "SQL 注入"},
			{Severity: "high", Status: "open", Title: "反射型 XSS"},
		},
		Steps: []StepRow{
			{Key: "recon", Status: "done"},
		},
		Facts:      []Fact{{Title: "目标使用 nginx", Body: "Server: nginx/1.25"}},
		TotalFacts: 1,
		Evidence:   EvidenceStats{TrafficCount: 3, SnapshotCount: 1},
	}
}

// TestMarkdownSections 校验 Markdown 章节与表格内容。
func TestMarkdownSections(t *testing.T) {
	md := Markdown(sampleDocument())
	for _, want := range []string{
		"# 任务报告：", "## 任务信息", "## 发现（2）", "SQL 注入", "## 计划步骤（1）",
		"recon", "## 事实节点（1）", "目标使用 nginx", "## 证据统计", "流量采集记录：3 条",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown 缺少片段 %q", want)
		}
	}
}

// TestHTMLContentAndEscaping 校验 HTML 结构、标题与转义。
func TestHTMLContentAndEscaping(t *testing.T) {
	doc := sampleDocument()
	out := HTML(doc)
	for _, want := range []string{
		"<!DOCTYPE html>", "<title>任务报告：", "<h1>任务报告：", "<table>",
		"SQL 注入", "recon", "目标使用 nginx",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("HTML 缺少片段 %q", want)
		}
	}
	if strings.Contains(out, "<报告>") {
		t.Error("HTML 未转义任务名中的尖括号")
	}
	if !strings.Contains(out, "&lt;报告&gt;") || !strings.Contains(out, "&amp;") {
		t.Error("HTML 未正确转义特殊字符")
	}
}

// TestDOCXIsOpenablePackage 校验 DOCX 是可被 zip 读取的 OOXML 包且含关键部件。
func TestDOCXIsOpenablePackage(t *testing.T) {
	data, err := DOCX(sampleDocument())
	if err != nil {
		t.Fatalf("DOCX：%v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("DOCX 不是合法 zip：%v", err)
	}
	files := map[string]string{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("打开 DOCX 部件 %s 失败: %v", file.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("读取 DOCX 部件 %s 失败: %v", file.Name, err)
		}
		files[file.Name] = string(body)
	}
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "word/document.xml", "word/styles.xml"} {
		if _, ok := files[name]; !ok {
			t.Errorf("DOCX 缺少部件 %s", name)
		}
	}
	document := files["word/document.xml"]
	if !strings.Contains(document, "任务报告：") || !strings.Contains(document, "SQL 注入") {
		t.Error("word/document.xml 缺少报告内容")
	}
	if strings.Contains(document, "<报告>") {
		t.Error("word/document.xml 未转义特殊字符")
	}
	if !strings.Contains(files["[Content_Types].xml"], "wordprocessingml.document.main+xml") {
		t.Error("[Content_Types].xml 缺少 document content type")
	}
}

// TestPDFIsStructurallyValid 校验 PDF 头尾、字体引用与 xref 偏移可解析。
func TestPDFIsStructurallyValid(t *testing.T) {
	data, err := PDF(sampleDocument())
	if err != nil {
		t.Fatalf("PDF：%v", err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF-1.4")) {
		t.Fatalf("PDF 头不合法: %q", data[:8])
	}
	if !bytes.HasSuffix(data, []byte("%%EOF\n")) {
		t.Error("PDF 缺少 EOF 结束标记")
	}
	if !bytes.Contains(data, []byte("/STSong-Light")) || !bytes.Contains(data, []byte("/UniGB-UCS2-H")) {
		t.Error("PDF 缺少 CJK 字体引用")
	}

	// 解析 startxref 并确认指向的 xref 表存在。
	idx := bytes.LastIndex(data, []byte("startxref\n"))
	if idx < 0 {
		t.Fatal("PDF 缺少 startxref")
	}
	rest := data[idx+len("startxref\n"):]
	line := rest[:bytes.IndexByte(rest, '\n')]
	offset, err := strconv.Atoi(string(line))
	if err != nil || offset <= 0 || offset >= len(data) {
		t.Fatalf("startxref 偏移非法: %q", line)
	}
	if !bytes.HasPrefix(data[offset:], []byte("xref\n")) {
		t.Fatalf("startxref 未指向 xref 表: %q", data[offset:offset+16])
	}
	// 对象计数与 trailer /Size 一致。
	if !bytes.Contains(data, []byte("trailer\n<< /Size ")) {
		t.Error("PDF 缺少 trailer")
	}
}

// TestEmptySectionsRenderNone 校验空发现/步骤/事实在四种格式里都显式输出「无」。
func TestEmptySectionsRenderNone(t *testing.T) {
	doc := Document{MissionName: "空任务", GeneratedAt: time.Unix(0, 0)}
	outputs := map[string]string{
		"markdown": Markdown(doc),
		"html":     HTML(doc),
	}
	docxData, err := DOCX(doc)
	if err != nil {
		t.Fatalf("DOCX：%v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(docxData), int64(len(docxData)))
	if err != nil {
		t.Fatalf("DOCX：%v", err)
	}
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		rc, _ := file.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		outputs["docx"] = string(body)
	}
	pdfData, err := PDF(doc)
	if err != nil {
		t.Fatalf("PDF：%v", err)
	}
	// PDF 文本是 UTF-16BE 十六进制；「无」= U+65E0，编码为 65E0。
	if !bytes.Contains(pdfData, []byte("65E0")) {
		t.Error("PDF 未包含「无」段落文本")
	}
	for name, out := range outputs {
		if !strings.Contains(out, "无：") {
			t.Errorf("%s 空数据段落缺少「无：」", name)
		}
		if !strings.Contains(out, "任务尚未记录事实节点") {
			t.Errorf("%s 缺少空事实段落", name)
		}
	}
}
