package httpapi

import (
	"net/http"
	"time"

	"github.com/orzl1/BeaconX/internal/infra/pg"
)

// B7：LLM 请求/响应正文的查看接口。
//
// 三个端点都以「调用记录」为中心：
//   - 详情只返回元信息（字节数、脱敏/截断标记、token 用量），不返回正文；
//   - /request 返回已脱敏的 messages JSON；
//   - /response 返回已脱敏的响应/错误文本。
//
// 列表仍用既有的 GET /api/v1/llm-records（来自 llm_usage 计量表），本文件不改它。
// 路由通过 init() + RegisterFeatureRoutes + Build 注册，不修改 handler.go。

func init() {
	RegisterFeatureRoutes(
		FeatureRoute{
			Method:  http.MethodGet,
			Pattern: "/api/v1/llm-records/{id}",
			Summary: "LLM 调用记录详情（不含正文，含字节数与脱敏标记）",
			Build:   func(o Options) http.HandlerFunc { return o.handleGetLLMExchange },
		},
		FeatureRoute{
			Method:  http.MethodGet,
			Pattern: "/api/v1/llm-records/{id}/request",
			Summary: "LLM 调用请求正文（已脱敏 messages，application/json）",
			Build:   func(o Options) http.HandlerFunc { return o.handleGetLLMExchangeRequest },
		},
		FeatureRoute{
			Method:  http.MethodGet,
			Pattern: "/api/v1/llm-records/{id}/response",
			Summary: "LLM 调用响应正文（已脱敏，text/plain）",
			Build:   func(o Options) http.HandlerFunc { return o.handleGetLLMExchangeResponse },
		},
	)
}

// llmExchangeDetail 是记录详情响应（显式排除 request/response 正文）。
type llmExchangeDetail struct {
	ID               int64     `json:"id"`
	MissionID        int64     `json:"mission_id"`
	Role             string    `json:"role"`
	Model            string    `json:"model"`
	Error            string    `json:"error,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	RequestBytes     int       `json:"request_bytes"`
	ResponseBytes    int       `json:"response_bytes"`
	Truncated        bool      `json:"truncated"`
	Redacted         bool      `json:"redacted"`
	CreatedAt        time.Time `json:"created_at"`
}

// llmExchangeID 解析路径中的调用记录标识。
func llmExchangeID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := pathInt64Param(r, "id", "LLM 调用记录")
	if err != nil {
		writeStoreError(w, r, err)
		return 0, false
	}
	return id, true
}

// fetchLLMExchange 读取记录（跨租户与不存在都走 404）。
func (o Options) fetchLLMExchange(w http.ResponseWriter, r *http.Request) (pg.LLMExchange, bool) {
	id, ok := llmExchangeID(w, r)
	if !ok {
		return pg.LLMExchange{}, false
	}
	exchange, err := o.Store.GetLLMExchange(r.Context(), o.TenantID, id)
	if err != nil {
		writeStoreError(w, r, err)
		return pg.LLMExchange{}, false
	}
	return exchange, true
}

// handleGetLLMExchange 返回调用记录元信息（不含正文）。
func (o Options) handleGetLLMExchange(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	exchange, ok := o.fetchLLMExchange(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, llmExchangeDetail{
		ID:               exchange.ID,
		MissionID:        exchange.MissionID,
		Role:             exchange.Role,
		Model:            exchange.Model,
		Error:            exchange.Error,
		PromptTokens:     exchange.PromptTokens,
		CompletionTokens: exchange.CompletionTokens,
		TotalTokens:      exchange.TotalTokens,
		RequestBytes:     exchange.RequestBytes,
		ResponseBytes:    exchange.ResponseBytes,
		Truncated:        exchange.Truncated,
		Redacted:         exchange.Redacted,
		CreatedAt:        exchange.CreatedAt,
	})
}

// handleGetLLMExchangeRequest 直接返回已脱敏的请求 JSON。
//
// 不做二次格式化：入库存的就是合法 JSON（写入前已脱敏），原样输出可以保证
// 前端/排障工具拿到的结构与模型实际收到的结构一致。
func (o Options) handleGetLLMExchangeRequest(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	exchange, ok := o.fetchLLMExchange(w, r)
	if !ok {
		return
	}
	body := exchange.Request
	if len(body) == 0 {
		body = []byte("{}")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleGetLLMExchangeResponse 返回已脱敏的响应文本；失败调用返回错误文本。
func (o Options) handleGetLLMExchangeResponse(w http.ResponseWriter, r *http.Request) {
	if !o.storeReady(w, r) {
		return
	}
	exchange, ok := o.fetchLLMExchange(w, r)
	if !ok {
		return
	}
	body := exchange.Response
	if body == "" && exchange.Error != "" {
		body = "错误：" + exchange.Error
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
