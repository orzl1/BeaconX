package evidence

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestRedactHeadersRedactsSensitive(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer super-secret")
	h.Set("Cookie", "sid=session-secret")
	h.Set("Set-Cookie", "a=1")
	h.Add("Set-Cookie", "b=2")
	h.Set("Proxy-Authorization", "Basic proxy-secret")
	h.Set("X-Api-Key", "api-key-secret")
	h.Set("X-Auth-Token", "auth-token-secret")
	h.Set("X-Goog-Api-Key", "google-key-secret")
	h.Set("X-Session-Secret", "session-secret")
	h.Set("Content-Type", "application/json")
	h.Set("X-Request-ID", "req-1")
	h.Add("Accept", "text/html")
	h.Add("Accept", "application/json")

	out := RedactHeaders(h)
	for _, name := range []string{
		"Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization",
		"X-Api-Key", "X-Auth-Token", "X-Goog-Api-Key", "X-Session-Secret",
	} {
		values := out.Values(name)
		if len(values) != 1 || values[0] != redactedValue {
			t.Errorf("头 %s 应被整体替换为 %s，实际：%v", name, redactedValue, values)
		}
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q，期望原样保留", got)
	}
	if got := out.Get("X-Request-ID"); got != "req-1" {
		t.Errorf("X-Request-ID = %q，期望原样保留", got)
	}
	if got := out.Values("Accept"); len(got) != 2 || got[0] != "text/html" || got[1] != "application/json" {
		t.Errorf("Accept = %v，期望保留多值顺序", got)
	}
	// 入参不能被修改。
	if got := h.Get("Authorization"); got != "Bearer super-secret" {
		t.Errorf("RedactHeaders 不应修改入参，Authorization = %q", got)
	}
}

func TestRedactHeadersNonCanonicalKeys(t *testing.T) {
	// http.Header 允许直接构造非规范键；脱敏时按大小写不敏感匹配，
	// 同时保留调用方使用的键拼写。
	h := http.Header{
		"authorization": {"Bearer x"},
		"x-api-key":     {"k"},
		"X-Keep":        {"v"},
	}
	out := RedactHeaders(h)
	if got := out["authorization"]; len(got) != 1 || got[0] != redactedValue {
		t.Errorf("authorization = %v，期望 %s", got, redactedValue)
	}
	if got := out["x-api-key"]; len(got) != 1 || got[0] != redactedValue {
		t.Errorf("x-api-key = %v，期望 %s", got, redactedValue)
	}
	if got := out["X-Keep"]; len(got) != 1 || got[0] != "v" {
		t.Errorf("X-Keep = %v，期望原样保留", got)
	}
}

func TestRedactHeadersNilAndEmpty(t *testing.T) {
	if got := RedactHeaders(nil); got != nil {
		t.Errorf("nil 头应返回 nil，实际：%v", got)
	}
	if got := RedactHeaders(http.Header{}); got == nil || len(got) != 0 {
		t.Errorf("空头应返回非 nil 空副本，实际：%v", got)
	}
}

func TestRedactJSONRedactsNested(t *testing.T) {
	raw := []byte(`{
		"user": {"name": "alice", "password": "p@ss", "nested": {"api_key": "k-123", "keep": 1}},
		"entries": [{"token": "t1"}, {"Token": "t2"}, {"keep": "v"}],
		"authorization": "Bearer raw-secret",
		"Cookie": "sid=raw-secret",
		"credential": {"id": "cred-1"},
		"secret": 42,
		"count": 3,
		"ok": true,
		"none": null,
		"list": [1, "two", false, null],
		"emptyObj": {},
		"emptyArr": []
	}`)
	out, err := RedactJSON(raw)
	if err != nil {
		t.Fatalf("RedactJSON：%v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("输出不是合法 JSON：%v", err)
	}

	user, ok := doc["user"].(map[string]any)
	if !ok {
		t.Fatalf("user 类型 = %T，期望对象", doc["user"])
	}
	if user["name"] != "alice" {
		t.Errorf("user.name = %v，期望保留", user["name"])
	}
	if user["password"] != redactedValue {
		t.Errorf("user.password = %v，期望 %s", user["password"], redactedValue)
	}
	nested, ok := user["nested"].(map[string]any)
	if !ok {
		t.Fatalf("user.nested 类型 = %T，期望对象", user["nested"])
	}
	if nested["api_key"] != redactedValue {
		t.Errorf("user.nested.api_key = %v，期望 %s", nested["api_key"], redactedValue)
	}
	if nested["keep"] != float64(1) {
		t.Errorf("user.nested.keep = %v，期望 1", nested["keep"])
	}

	entries, ok := doc["entries"].([]any)
	if !ok || len(entries) != 3 {
		t.Fatalf("entries = %v，期望 3 元素数组", doc["entries"])
	}
	firstEntry, ok := entries[0].(map[string]any)
	if !ok || firstEntry["token"] != redactedValue {
		t.Errorf("entries[0] = %v，期望 token 被脱敏", entries[0])
	}
	secondEntry, ok := entries[1].(map[string]any)
	if !ok || secondEntry["Token"] != redactedValue {
		t.Errorf("entries[1] = %v，期望 Token 被脱敏", entries[1])
	}
	thirdEntry, ok := entries[2].(map[string]any)
	if !ok || thirdEntry["keep"] != "v" {
		t.Errorf("entries[2] = %v，期望保留 keep=v", entries[2])
	}

	for _, key := range []string{"authorization", "Cookie", "credential", "secret"} {
		if doc[key] != redactedValue {
			t.Errorf("%s = %v，期望整体替换为 %s", key, doc[key], redactedValue)
		}
	}
	if doc["count"] != float64(3) {
		t.Errorf("count = %v，期望 3", doc["count"])
	}
	if doc["ok"] != true {
		t.Errorf("ok = %v，期望 true", doc["ok"])
	}
	if doc["none"] != nil {
		t.Errorf("none = %v，期望 null", doc["none"])
	}
	if list, ok := doc["list"].([]any); !ok || len(list) != 4 || list[0] != float64(1) || list[1] != "two" || list[2] != false || list[3] != nil {
		t.Errorf("list = %v，期望结构与类型保留", doc["list"])
	}
	if emptyObj, ok := doc["emptyObj"].(map[string]any); !ok || len(emptyObj) != 0 {
		t.Errorf("emptyObj = %v，期望空对象", doc["emptyObj"])
	}
	if emptyArr, ok := doc["emptyArr"].([]any); !ok || len(emptyArr) != 0 {
		t.Errorf("emptyArr = %v，期望空数组", doc["emptyArr"])
	}

	// 原始敏感值不应残留在输出里。
	for _, leaked := range []string{"p@ss", "k-123", "t1", "t2", "raw-secret", "cred-1"} {
		if bytes.Contains(out, []byte(leaked)) {
			t.Errorf("输出中残留敏感值 %q：%s", leaked, out)
		}
	}
}

func TestRedactJSONKeepsNonSensitiveStructure(t *testing.T) {
	raw := []byte(`{"a":{"b":[1,2,{"c":true}]},"d":null,"e":"text"}`)
	out, err := RedactJSON(raw)
	if err != nil {
		t.Fatalf("RedactJSON：%v", err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("解析输入：%v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("解析输出：%v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("无敏感键时结构应原样保留：\n得到：%v\n期望：%v", got, want)
	}
}

func TestRedactJSONRedactsCaseInsensitive(t *testing.T) {
	raw := []byte(`{"PASSWORD":"a","Api_Key":"b","ACCESS-TOKEN":"c","ClientSecret":"d","X-Credential":"e","keep":"f"}`)
	out, err := RedactJSON(raw)
	if err != nil {
		t.Fatalf("RedactJSON：%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("解析输出：%v", err)
	}
	for _, key := range []string{"PASSWORD", "Api_Key", "ACCESS-TOKEN", "ClientSecret", "X-Credential"} {
		if doc[key] != redactedValue {
			t.Errorf("%s = %v，期望 %s", key, doc[key], redactedValue)
		}
	}
	if doc["keep"] != "f" {
		t.Errorf("keep = %v，期望保留", doc["keep"])
	}
}

func TestRedactJSONInvalid(t *testing.T) {
	invalid := [][]byte{
		nil,
		[]byte(""),
		[]byte("   "),
		[]byte("{"),
		[]byte(`{"a":}`),
		[]byte(`{"a":1} {"b":2}`),
		[]byte("not json"),
	}
	for _, raw := range invalid {
		if _, err := RedactJSON(raw); err == nil {
			t.Errorf("非法 JSON %q 应返回错误", raw)
		}
	}

	valid := [][]byte{
		[]byte("null"),
		[]byte("[]"),
		[]byte("{}"),
		[]byte("true"),
		[]byte(`"text"`),
		[]byte("123"),
	}
	for _, raw := range valid {
		out, err := RedactJSON(raw)
		if err != nil {
			t.Errorf("合法 JSON %q 不应报错：%v", raw, err)
			continue
		}
		if !json.Valid(out) {
			t.Errorf("输出不是合法 JSON：%s", out)
		}
	}
}

func TestRedactJSONRootNonObject(t *testing.T) {
	out, err := RedactJSON([]byte(`[{"password":"x"},2]`))
	if err != nil {
		t.Fatalf("RedactJSON：%v", err)
	}
	var list []any
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("解析输出：%v", err)
	}
	if len(list) != 2 {
		t.Fatalf("数组长度 = %d，期望 2", len(list))
	}
	first, ok := list[0].(map[string]any)
	if !ok || first["password"] != redactedValue {
		t.Errorf("list[0] = %v，期望 password 被脱敏", list[0])
	}
	if list[1] != float64(2) {
		t.Errorf("list[1] = %v，期望 2", list[1])
	}

	scalar, err := RedactJSON([]byte(`"password"`))
	if err != nil {
		t.Fatalf("RedactJSON：%v", err)
	}
	if string(scalar) != `"password"` {
		t.Errorf("标量根应原样输出，实际：%s", scalar)
	}
}

func TestRedactJSONIdempotent(t *testing.T) {
	raw := []byte(`{"user":{"password":"p","token":"t"},"list":[{"secret":"s"}],"keep":1}`)
	first, err := RedactJSON(raw)
	if err != nil {
		t.Fatalf("第一次 RedactJSON：%v", err)
	}
	second, err := RedactJSON(first)
	if err != nil {
		t.Fatalf("第二次 RedactJSON：%v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("重复脱敏结果应稳定：\n第一次：%s\n第二次：%s", first, second)
	}
}

func TestRedactJSONPreservesNumbers(t *testing.T) {
	raw := []byte(`{"big":9007199254740993,"float":1.5,"exp":1e100,"neg":-0.0}`)
	out, err := RedactJSON(raw)
	if err != nil {
		t.Fatalf("RedactJSON：%v", err)
	}
	for _, literal := range []string{"9007199254740993", "1.5", "1e100", "-0.0"} {
		if !bytes.Contains(out, []byte(literal)) {
			t.Errorf("数字原文 %s 应被保留：%s", literal, out)
		}
	}
}
