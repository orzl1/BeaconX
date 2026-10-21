package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// authRequest 构造一个带可选 Bearer 头与查询参数的请求。
func authRequest(bearer, queryToken string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stream", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if queryToken != "" {
		q := req.URL.Query()
		q.Set("token", queryToken)
		req.URL.RawQuery = q.Encode()
	}
	return req
}

func TestTokenAuthenticator(t *testing.T) {
	const token = "s3cr3t-token"

	tests := []struct {
		name       string
		auth       *TokenAuthenticator
		bearer     string
		queryToken string
		wantOK     bool
	}{
		{name: "Bearer 正确", auth: NewTokenAuthenticator(token), bearer: token, wantOK: true},
		{name: "Bearer 错误", auth: NewTokenAuthenticator(token), bearer: "wrong", wantOK: false},
		{name: "查询参数正确", auth: NewTokenAuthenticator(token), queryToken: token, wantOK: true},
		{name: "查询参数错误", auth: NewTokenAuthenticator(token), queryToken: "wrong", wantOK: false},
		{name: "缺少令牌", auth: NewTokenAuthenticator(token), wantOK: false},
		{name: "空令牌认证器拒绝一切", auth: NewTokenAuthenticator(""), bearer: token, wantOK: false},
		{name: "空令牌认证器拒绝空值", auth: NewTokenAuthenticator(""), wantOK: false},
		{name: "空令牌认证器拒绝查询参数", auth: NewTokenAuthenticator(""), queryToken: token, wantOK: false},
		{name: "Bearer 优先于查询参数（查询参数错误）", auth: NewTokenAuthenticator(token), bearer: token, queryToken: "wrong", wantOK: true},
		{name: "Bearer 优先于查询参数（Bearer 错误）", auth: NewTokenAuthenticator(token), bearer: "wrong", queryToken: token, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.auth.Authenticate(authRequest(tt.bearer, tt.queryToken))
			if tt.wantOK && err != nil {
				t.Fatalf("期望认证通过，实际错误：%v", err)
			}
			if !tt.wantOK {
				if !errors.Is(err, ErrUnauthorized) {
					t.Fatalf("期望 ErrUnauthorized，实际：%v", err)
				}
			}
		})
	}
}

func TestTokenAuthenticatorAcceptsLowercaseScheme(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer s3cr3t-token")
	if err := NewTokenAuthenticator("s3cr3t-token").Authenticate(req); err != nil {
		t.Fatalf("Bearer 方案应大小写不敏感：%v", err)
	}
}

func TestTokenAuthenticatorFallsBackToQueryOnMalformedHeader(t *testing.T) {
	req := authRequest("", "s3cr3t-token")
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if err := NewTokenAuthenticator("s3cr3t-token").Authenticate(req); err != nil {
		t.Fatalf("非 Bearer 头应回退到查询参数：%v", err)
	}
}

func TestTokenAuthenticatorEmptyQueryValueIsRejected(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?token=", nil)
	if err := NewTokenAuthenticator("s3cr3t-token").Authenticate(req); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("空查询参数应被拒绝，实际：%v", err)
	}
}

func TestTokenAuthenticatorImplementsAuthenticator(t *testing.T) {
	var _ Authenticator = NewTokenAuthenticator("x")
}
