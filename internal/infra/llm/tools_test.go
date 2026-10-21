package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// toolResponseBody 构造一个包含 tool_calls 的成功响应。
func toolResponseBody(content string, calls []map[string]any) map[string]any {
	rawCalls := make([]any, 0, len(calls))
	for _, call := range calls {
		rawCalls = append(rawCalls, map[string]any{
			"id":   call["id"],
			"type": "function",
			"function": map[string]any{
				"name":      call["name"],
				"arguments": call["arguments"],
			},
		})
	}
	message := map[string]any{"role": "assistant", "tool_calls": rawCalls}
	if content == "" {
		message["content"] = nil
	} else {
		message["content"] = content
	}
	return map[string]any{
		"model": "deepseek-chat",
		"choices": []any{map[string]any{
			"message":       message,
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{
			"prompt_tokens":     5,
			"completion_tokens": 6,
			"total_tokens":      11,
		},
	}
}

func TestCompleteWithToolsRequestShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeJSON(w, toolResponseBody("", []map[string]any{
			{"id": "call_1", "name": "get_weather", "arguments": `{"city":"北京"}`},
		}))
	}))
	defer srv.Close()

	tools := []ToolDef{
		{
			Name:        "get_weather",
			Description: "查询指定城市的天气",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		},
		{Name: "ping"},
	}
	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "北京天气"}}, tools, "auto")
	if err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}

	rawTools, ok := body["tools"].([]any)
	if !ok || len(rawTools) != 2 {
		t.Fatalf("tools = %v", body["tools"])
	}
	first, _ := rawTools[0].(map[string]any)
	if first["type"] != "function" {
		t.Errorf("tools[0].type = %v，期望 function", first["type"])
	}
	firstFn, _ := first["function"].(map[string]any)
	if firstFn["name"] != "get_weather" || firstFn["description"] != "查询指定城市的天气" {
		t.Errorf("tools[0].function = %v", firstFn)
	}
	params, ok := firstFn["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Errorf("tools[0].function.parameters = %v", firstFn["parameters"])
	}
	second, _ := rawTools[1].(map[string]any)
	secondFn, _ := second["function"].(map[string]any)
	if secondFn["name"] != "ping" {
		t.Errorf("tools[1].function.name = %v", secondFn["name"])
	}
	if _, hasDescription := secondFn["description"]; hasDescription {
		t.Errorf("空 description 不应发送：%v", secondFn)
	}
	if _, hasParameters := secondFn["parameters"]; hasParameters {
		t.Errorf("nil parameters 不应发送：%v", secondFn)
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v，期望 auto", body["tool_choice"])
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCalls = %+v", resp.ToolCalls)
	}
}

func TestCompleteWithToolsOmitsEmptyToolsFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeChatSuccess(w, "m", "普通回答", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, "")
	if err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}
	if _, ok := body["tools"]; ok {
		t.Errorf("tools 为空时不应发送该字段：%v", body["tools"])
	}
	if _, ok := body["tool_choice"]; ok {
		t.Errorf("tool_choice 为空时不应发送该字段：%v", body["tool_choice"])
	}
	// 模型选择直接回答（没有 tool_calls）时也是合法结果。
	if resp.Content != "普通回答" || len(resp.ToolCalls) != 0 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestCompleteWithToolsParsesSingleToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, toolResponseBody("", []map[string]any{
			{"id": "call_1", "name": "get_weather", "arguments": `{"city":"北京"}`},
		}))
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "get_weather"}}, "auto")
	if err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}
	if resp.Content != "" {
		t.Errorf("Content = %q，期望空字符串（只有 tool_calls）", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	call := resp.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "get_weather" || call.Arguments != `{"city":"北京"}` {
		t.Errorf("ToolCall = %+v", call)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Usage.TotalTokens != 11 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if resp.Model != "deepseek-chat" {
		t.Errorf("Model = %q", resp.Model)
	}
}

func TestCompleteWithToolsParsesMultipleToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, toolResponseBody("我先查询两个城市", []map[string]any{
			{"id": "call_a", "name": "get_weather", "arguments": `{"city":"北京"}`},
			{"id": "call_b", "name": "ping", "arguments": ""},
		}))
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	resp, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "get_weather"}, {Name: "ping"}}, "auto")
	if err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}
	if resp.Content != "我先查询两个城市" {
		t.Errorf("Content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
	if got := resp.ToolCalls[0]; got.ID != "call_a" || got.Name != "get_weather" || got.Arguments != `{"city":"北京"}` {
		t.Errorf("ToolCalls[0] = %+v", got)
	}
	// 无参数工具的空 arguments 必须原样保留为空字符串，而不是报错。
	if got := resp.ToolCalls[1]; got.ID != "call_b" || got.Name != "ping" || got.Arguments != "" {
		t.Errorf("ToolCalls[1] = %+v", got)
	}
}

func TestCompleteWithToolsToolMessageRoundTrip(t *testing.T) {
	var messages []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		messages = body.Messages
		writeChatSuccess(w, "m", "北京 20 度", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	replay := []Message{
		{Role: "user", Content: "北京天气"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`},
		}},
		{Role: "tool", ToolCallID: "call_1", Content: `{"temp":20}`},
	}
	resp, err := c.CompleteWithTools(context.Background(), replay, []ToolDef{{Name: "get_weather"}}, "auto")
	if err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}
	if resp.Content != "北京 20 度" {
		t.Errorf("Content = %q", resp.Content)
	}

	if len(messages) != 3 {
		t.Fatalf("messages 数量 = %d", len(messages))
	}
	assistant := messages[1]
	rawCalls, ok := assistant["tool_calls"].([]any)
	if !ok || len(rawCalls) != 1 {
		t.Fatalf("assistant.tool_calls = %v", assistant["tool_calls"])
	}
	call, _ := rawCalls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Errorf("tool_calls[0] = %v", call)
	}
	if _, flatName := call["name"]; flatName {
		t.Errorf("tool_calls 必须是 OpenAI 嵌套结构，不应有扁平 name：%v", call)
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("tool_calls[0].function = %v", fn)
	}
	toolMsg := messages[2]
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != `{"temp":20}` {
		t.Errorf("tool 消息 = %v", toolMsg)
	}
}

func TestCompleteWithToolsToolChoiceNone(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		writeChatSuccess(w, "m", "不调用工具", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	if _, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "get_weather"}}, "none"); err != nil {
		t.Fatalf("CompleteWithTools：%v", err)
	}
	if body["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v，期望 none", body["tool_choice"])
	}
}

func TestCompleteWithToolsRejectsEmptyResponse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""}}]}`)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "get_weather"}}, "auto")
	if err == nil || !strings.Contains(err.Error(), "tool_calls") {
		t.Fatalf("err = %v，期望提示既无内容也无 tool_calls", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("空响应不应重试，请求次数 = %d", got)
	}
}

func TestCompleteWithToolsRetriesOn429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limit"}`)
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", MaxRetries: 1})
	_, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "get_weather"}}, "auto")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("错误 = %v，期望 *APIError(429)", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("请求次数 = %d，期望 2（1 次重试）", got)
	}
}

func TestCompleteWithToolsRedactsAPIKey(t *testing.T) {
	const apiKey = "sk-tool-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{
			"error": map[string]any{"message": "invalid api key " + apiKey},
		})
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m", APIKey: apiKey})
	_, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "get_weather"}}, "auto")
	if err == nil {
		t.Fatal("期望 401 报错")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("错误文本泄漏了 API Key：%q", err.Error())
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 = %v，期望 *APIError(401)", err)
	}
	if strings.Contains(apiErr.Body, apiKey) {
		t.Errorf("APIError.Body 泄漏了 API Key：%q", apiErr.Body)
	}
}

func TestCompleteStillRejectsToolOnlyResponse(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeJSON(w, toolResponseBody("", []map[string]any{
			{"id": "call_1", "name": "get_weather", "arguments": "{}"},
		}))
	}))
	defer srv.Close()

	// 兼容性：Complete 不声明 tools，仍按旧语义要求有正文；
	// 不能因为响应里出现 tool_calls 就静默返回空内容。
	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "内容为空") {
		t.Fatalf("err = %v，期望内容为空错误", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("请求次数 = %d", got)
	}
}

func TestCompleteWithToolsRejectsInvalidSchema(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		writeChatSuccess(w, "m", "不应到达", "stop")
	}))
	defer srv.Close()

	c := mustNewClient(t, Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.CompleteWithTools(context.Background(), []Message{{Role: "user", Content: "hi"}},
		[]ToolDef{{Name: "bad", Parameters: json.RawMessage(`{not-json`)}}, "auto")
	if err == nil || !strings.Contains(err.Error(), "序列化") {
		t.Fatalf("err = %v，期望序列化失败", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("请求体非法时不应发出请求，请求次数 = %d", got)
	}
}

func TestToolTypesJSONShape(t *testing.T) {
	defBody, err := json.Marshal(ToolDef{
		Name:        "get_weather",
		Description: "查询天气",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("序列化 ToolDef：%v", err)
	}
	var def map[string]any
	if err := json.Unmarshal(defBody, &def); err != nil {
		t.Fatalf("反序列化 ToolDef：%v", err)
	}
	if def["name"] != "get_weather" || def["description"] != "查询天气" {
		t.Errorf("ToolDef = %v", def)
	}
	if _, ok := def["parameters"].(map[string]any); !ok {
		t.Errorf("ToolDef.Parameters 应为 JSON 对象：%v", def["parameters"])
	}

	msgBody, err := json.Marshal(ToolMessage{ToolCallID: "call_1", Content: "结果"})
	if err != nil {
		t.Fatalf("序列化 ToolMessage：%v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(msgBody, &msg); err != nil {
		t.Fatalf("反序列化 ToolMessage：%v", err)
	}
	if msg["tool_call_id"] != "call_1" || msg["content"] != "结果" {
		t.Errorf("ToolMessage = %v", msg)
	}
}
