package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// ErrUnauthorized 表示请求未提供有效令牌。
var ErrUnauthorized = errors.New("未授权")

// TokenAuthenticator 使用静态令牌校验请求。
//
// 令牌有两种携带方式：Authorization: Bearer <token>（普通 API 调用），
// 以及 ?token=<token> 查询参数——浏览器 EventSource 无法自定义请求头，
// 因此 SSE 场景只能走查询参数。两种方式都使用常量时间比较。
type TokenAuthenticator struct {
	token []byte
}

// NewTokenAuthenticator 创建静态令牌认证器。
//
// token 为空时构造出的认证器拒绝所有请求（fail closed）。
func NewTokenAuthenticator(token string) *TokenAuthenticator {
	return &TokenAuthenticator{token: []byte(token)}
}

// Authenticate 实现 Authenticator 接口。
//
// 优先使用 Authorization 头中的 Bearer 令牌，缺失或格式不合法时回退到
// ?token= 查询参数。任何情况下都不接受空令牌。
func (a *TokenAuthenticator) Authenticate(r *http.Request) error {
	if a == nil || len(a.token) == 0 || r == nil {
		return ErrUnauthorized
	}
	supplied := bearerToken(r)
	if supplied == "" {
		supplied = r.URL.Query().Get("token")
	}
	if supplied == "" {
		return ErrUnauthorized
	}
	if subtle.ConstantTimeCompare(a.token, []byte(supplied)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

// bearerToken 从 Authorization 头取出 Bearer 令牌；格式不合法时返回空串。
func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}
