package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// LLM 请求/响应正文采集的存储层（B7，表见 migrations/0017_llm_exchanges.sql）。
//
// 与 llm_usage 的分工：llm_usage 只记 token 计量；本表记录每次调用实际发送的
// messages 与模型返回内容，用于排障与审计复盘。正文在写入前已脱敏并截断
// （见 internal/app/ai/llm_capture.go），存储层不再做二次处理。

// LLMExchange 是一条 LLM 调用正文记录。
type LLMExchange struct {
	TenantID         int64     `json:"tenant_id"`
	ID               int64     `json:"id"`
	MissionID        int64     `json:"mission_id"`
	Role             string    `json:"role"`
	Model            string    `json:"model"`
	Request          []byte    `json:"-"`
	Response         string    `json:"-"`
	Error            string    `json:"error"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	RequestBytes     int       `json:"request_bytes"`
	ResponseBytes    int       `json:"response_bytes"`
	Truncated        bool      `json:"truncated"`
	Redacted         bool      `json:"redacted"`
	CreatedAt        time.Time `json:"created_at"`
}

// LLMExchangeInput 是写入一条调用记录的输入。
//
// Request 必须是合法 JSON（已脱敏的 messages 结构）；Response/Error 都是
// 已脱敏、已截断的文本。
type LLMExchangeInput struct {
	MissionID        int64
	Role             string
	Model            string
	Request          []byte
	Response         string
	Error            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	RequestBytes     int
	ResponseBytes    int
	Truncated        bool
	Redacted         bool
}

// RecordLLMExchange 追加一条调用正文记录，返回自增 id。
func (s *Store) RecordLLMExchange(ctx context.Context, tenantID int64, in LLMExchangeInput) (int64, error) {
	if err := checkTenant(tenantID); err != nil {
		return 0, err
	}
	if in.MissionID <= 0 {
		return 0, fmt.Errorf("%w：任务标识必须为正数", ErrInvalidInput)
	}
	request := in.Request
	if len(request) == 0 {
		request = []byte("{}")
	}
	var id int64
	err := s.db.pool.QueryRow(ctx, `
		INSERT INTO llm_exchanges (
		    tenant_id, mission_id, role, model, request, response, error,
		    prompt_tokens, completion_tokens, total_tokens,
		    request_bytes, response_bytes, truncated, redacted)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING id`,
		tenantID, in.MissionID, in.Role, in.Model, string(request), in.Response, in.Error,
		in.PromptTokens, in.CompletionTokens, in.TotalTokens,
		in.RequestBytes, in.ResponseBytes, in.Truncated, in.Redacted).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写入 LLM 调用记录失败: %w", err)
	}
	return id, nil
}

// GetLLMExchange 按 ID 读取一条调用记录（含 request/response 正文）。
//
// 列表接口不返回正文；只有详情/请求/响应端点会用到正文，因此正文由本方法
// 一次性取出，调用方自行决定是否输出。
func (s *Store) GetLLMExchange(ctx context.Context, tenantID, exchangeID int64) (LLMExchange, error) {
	if err := checkTenant(tenantID); err != nil {
		return LLMExchange{}, err
	}
	if exchangeID <= 0 {
		return LLMExchange{}, fmt.Errorf("%w：记录标识必须为正数", ErrInvalidInput)
	}
	var (
		out LLMExchange
		raw []byte
	)
	err := s.db.pool.QueryRow(ctx, `
		SELECT tenant_id, id, mission_id, role, model, request, response, error,
		       prompt_tokens, completion_tokens, total_tokens,
		       request_bytes, response_bytes, truncated, redacted, created_at
		FROM llm_exchanges WHERE tenant_id = $1 AND id = $2`,
		tenantID, exchangeID).
		Scan(&out.TenantID, &out.ID, &out.MissionID, &out.Role, &out.Model, &raw, &out.Response,
			&out.Error, &out.PromptTokens, &out.CompletionTokens, &out.TotalTokens,
			&out.RequestBytes, &out.ResponseBytes, &out.Truncated, &out.Redacted, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LLMExchange{}, fmt.Errorf("%w：调用记录 %d", ErrNotFound, exchangeID)
	}
	if err != nil {
		return LLMExchange{}, fmt.Errorf("读取 LLM 调用记录失败: %w", err)
	}
	out.Request = raw
	return out, nil
}
