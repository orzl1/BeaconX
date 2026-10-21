package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// redactedValue 是脱敏后的统一占位值。
//
// 它本身不包含任何敏感片段，因此对已脱敏数据重复执行脱敏不会改变结果。
const redactedValue = "[REDACTED]"

// redactedHeaderNames 是整值替换的敏感头名（小写）。
//
// 这些是「值本身就是凭据」的头；具体取值见各协议规范，列表按常见攻击面
// 覆盖，不追求穷举，后续新增头名只能追加。
var redactedHeaderNames = map[string]struct{}{
	"authorization":        {},
	"proxy-authorization":  {},
	"cookie":               {},
	"set-cookie":           {},
	"x-api-key":            {},
	"x-auth-token":         {},
	"x-access-token":       {},
	"x-api-token":          {},
	"x-session-token":      {},
	"x-csrf-token":         {},
	"x-xsrf-token":         {},
	"api-key":              {},
	"x-amz-security-token": {},
	"x-goog-api-key":       {},
}

// sensitiveNameFragments 是「键名命中即脱敏」的片段（小写）。
//
// 用于兜住命名变体（如 X-Auth-Token / refresh_token / ApiKey / api-key），
// 代价是少量误报（例如 tokenizer 也会被脱敏）——对证据采集来说，
// 宁可多脱敏，不可漏脱敏。
var sensitiveNameFragments = []string{
	"password",
	"passwd",
	"token",
	"secret",
	"apikey",
	"api_key",
	"api-key",
	"authorization",
	"cookie",
	"credential",
}

// sensitiveName 判断一个头名或 JSON 键名是否属于敏感字段（不区分大小写）。
func sensitiveName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if _, ok := redactedHeaderNames[lower]; ok {
		return true
	}
	for _, fragment := range sensitiveNameFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// RedactHeaders 返回请求/响应头的脱敏副本。
//
// Authorization、Cookie、Set-Cookie、Proxy-Authorization、X-Api-Key、
// X-Auth-Token 等敏感头的值整体替换为 "[REDACTED]"；其余头原样复制。
// 入参不会被修改；nil 返回 nil。
func RedactHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for name, values := range h {
		if sensitiveName(name) {
			// 敏感头只保留一个占位值，避免通过值个数推断原值。
			out[name] = []string{redactedValue}
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// RedactJSON 递归脱敏一段 JSON 文本。
//
// 键名（不区分大小写）包含 password/token/secret/apikey/api_key/
// authorization/cookie/credential 等片段时，其值整体替换为 "[REDACTED]"；
// 非敏感字段保留原始结构与类型（数字、布尔、null、数组、空对象都不变）。
// 输出是压缩后的 JSON，map 键按字典序输出，因此重复脱敏结果稳定。
func RedactJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	// 用 UseNumber 保留数字原文，避免把大整数/int64 精度丢成 float64。
	decoder.UseNumber()
	var doc any
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("JSON 脱敏失败：内容不是合法 JSON: %w", err)
	}
	// 拒绝尾随内容，避免「只脱敏了第一段」的错觉。
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON 脱敏失败：内容在顶层值之后还有多余数据")
		}
		return nil, fmt.Errorf("JSON 脱敏失败：内容不是合法 JSON: %w", err)
	}

	out, err := json.Marshal(redactValue(doc))
	if err != nil {
		return nil, fmt.Errorf("JSON 脱敏失败：序列化结果失败: %w", err)
	}
	return out, nil
}

// redactValue 递归处理已解析的 JSON 值。
func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if sensitiveName(key) {
				typed[key] = redactedValue
				continue
			}
			typed[key] = redactValue(item)
		}
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = redactValue(item)
		}
		return typed
	default:
		return value
	}
}
