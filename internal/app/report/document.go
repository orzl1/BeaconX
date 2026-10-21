// Package report 把任务报告渲染成多种交付格式：Markdown、HTML、DOCX、PDF。
//
// 设计原则：
//
//   - **只读投影**：渲染器只消费 Document，不访问数据库、不读时钟（生成时间由
//     调用方传入），因此同一份数据总是得到同一份字节输出（PDF/DOCX 中不写
//     当前时间）；
//   - **零第三方依赖**：DOCX 用标准库 archive/zip 拼最小 OOXML；PDF 手写最小
//     PDF 1.4 结构，中文使用 Type0/UniGB-UCS2-H 的 Adobe 标准 CJK 字体引用
//     （STSong-Light，不嵌入字体文件）——不依赖 CGO、字体文件或第三方库；
//   - **空数据显式表达**：发现/步骤/事实为空时输出「无」段落，读者能区分
//     「没有数据」与「渲染漏了」。
package report

import "time"

// Field 是「任务信息」中的一行键值。
type Field struct {
	Name  string
	Value string
}

// FindingRow 是发现表格的一行。
type FindingRow struct {
	Severity string
	Status   string
	Title    string
}

// StepRow 是计划步骤表格的一行。
type StepRow struct {
	Key    string
	Status string
}

// Fact 是事实节点的一段（正文会由各格式自行截断/换行）。
type Fact struct {
	Title string
	Body  string
}

// EvidenceStats 是证据统计。
type EvidenceStats struct {
	TrafficCount  int
	SnapshotCount int
}

// Document 是与存储无关的报告数据模型。
type Document struct {
	MissionName string
	MissionID   int64
	GeneratedAt time.Time
	Fields      []Field
	Findings    []FindingRow
	Steps       []StepRow
	Facts       []Fact
	// TotalFacts 是事实节点总数（Facts 可能只包含前 N 条展示项）。
	TotalFacts int
	Evidence   EvidenceStats
}
