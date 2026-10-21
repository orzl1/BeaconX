package plan

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParseDecision 从规划器的原始输出里提取并解析一次决策。
//
// 支持的输入形态：
//
//   - 纯 JSON 对象；
//   - ``` 或 ```json 围栏（开围栏后的语言标记忽略，内容是否合法由解析结果
//     决定），围栏前后可以夹带说明文字；
//   - 裸 JSON 前后夹带任意杂质（取第一个 '{' 到与它配对的 '}'，字符串里的
//     花括号会被正确跳过）。
//
// 解析成功后做两件事：
//
//  1. 规范化：按步骤去重 DependsOn（保留首次出现的顺序），容忍模型重复输出
//     同一个依赖；
//  2. 调用 Decision.Validate 校验结构合法性。
//
// 错误信息均为中文，并尽量说明失败阶段（空输出 / 找不到 JSON / 围栏不闭合 /
// JSON 语法错误 / 校验失败），便于把错误原样回灌给模型或记入事件。
func ParseDecision(raw string) (Decision, error) {
	return ParseDecisionWithKnown(raw, nil)
}

// ParseDecisionWithKnown 与 ParseDecision 相同，但允许动作引用「已知步骤」。
//
// 规划是增量的：后一次决策往往只给出动作（step_key 指向上一轮已声明的步骤），
// 调用方需要把当前计划里的步骤 key 作为 known 传入，否则会被误判为非法引用。
func ParseDecisionWithKnown(raw string, known map[string]bool) (Decision, error) {
	body, err := extractDecisionJSON(raw)
	if err != nil {
		return Decision{}, err
	}

	var d Decision
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return Decision{}, fmt.Errorf("解析决策 JSON 失败：%w", err)
	}
	d = normalizeDecision(d)
	if err := d.ValidateWithKnown(known); err != nil {
		return Decision{}, fmt.Errorf("决策校验失败：%w", err)
	}
	return d, nil
}

// normalizeDecision 返回规范化后的决策副本：每个步骤的 DependsOn 去重并保留
// 原有顺序。其余字段原样保留（key 不做 trim / 大小写转换——格式问题应当暴露
// 为校验错误，而不是被静默修正）。
func normalizeDecision(d Decision) Decision {
	for i := range d.Steps {
		deps := d.Steps[i].DependsOn
		if len(deps) < 2 {
			continue
		}
		seen := make(map[string]bool, len(deps))
		deduped := make([]string, 0, len(deps))
		for _, dep := range deps {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			deduped = append(deduped, dep)
		}
		// 去重没有实际变化时保留原切片，避免无意义地改写模型输出。
		if len(deduped) != len(deps) {
			d.Steps[i].DependsOn = deduped
		}
	}
	return d
}

// extractDecisionJSON 从原始输出中定位 JSON 对象文本。
func extractDecisionJSON(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("模型输出为空，无法解析决策")
	}
	if strings.HasPrefix(trimmed, "```") {
		return extractFencedJSON(trimmed)
	}
	if strings.HasPrefix(trimmed, "[") {
		return "", errors.New("决策必须是 JSON 对象，而不是 JSON 数组")
	}
	return extractJSONObject(trimmed)
}

// extractFencedJSON 取出围栏内的 JSON 对象。围栏必须闭合，内容不能为空。
func extractFencedJSON(s string) (string, error) {
	rest := strings.TrimPrefix(s, "```")
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		// 常规形态：```json\n{...}\n```；开围栏后同一行是语言标记，直接跳过。
		rest = rest[nl+1:]
	} else {
		// 单行形态：```json {...}```，语言标记与 JSON 在同一行。
		rest = trimFenceLanguage(rest)
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return "", errors.New("JSON 代码围栏未闭合：缺少结尾的 ```")
	}
	inner := strings.TrimSpace(rest[:end])
	if inner == "" {
		return "", errors.New("JSON 代码围栏内容为空")
	}
	return extractJSONObject(inner)
}

// trimFenceLanguage 去掉开围栏后同一行的语言标记（例如 json / JSON）。
func trimFenceLanguage(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 4 && strings.EqualFold(t[:4], "json") {
		return t[4:]
	}
	return t
}

// extractJSONObject 从任意文本中截取第一个完整的 JSON 对象（'{' 到与它配对的
// '}'）。扫描时跟踪字符串与转义，因此字符串值里出现花括号不会破坏配对。
func extractJSONObject(s string) (string, error) {
	// 整体是数组（或围栏内容以数组开头）说明模型给错了顶层类型：直接报错，
	// 避免把数组里的第一个对象误当成一次决策。
	if strings.HasPrefix(strings.TrimSpace(s), "[") {
		return "", errors.New("决策必须是 JSON 对象，而不是 JSON 数组")
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", errors.New("模型输出中未找到 JSON 对象：缺少起始的 '{'")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", errors.New("JSON 对象不完整：缺少与 '{' 配对的 '}'")
}
