package httpapi

// B3：/api/ 默认缓存策略的统一响应中间件。这里通过完整装配（BuildGatewayRouter）
// 发真实请求，证明登录/注册/2FA setup//api/me//api/sessions 均带 no-store，
// 且 /api/geoip 可以显式覆盖为 private, max-age=300。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
)

func TestAPIResponsesDefaultNoStore(t *testing.T) {
	deps, _ := gatewayDepsForTest(t)
	const userID int64 = 1
	selfRepo := newFakeSelfRepo()
	selfRepo.add(userID, SelfRecord{Username: "alice", Email: strPtr("alice@example.com")})
	deps.SelfRepo = selfRepo
	twoFA := newFakeTwoFARepo()
	twoFA.add(TwoFAUser{Username: "alice", Email: "alice@example.com", Status: "enabled"})
	deps.TwoFARepo = twoFA

	rt, err := BuildGatewayRouter(deps)
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}

	ctx := context.Background()
	sid, err := deps.Sessions.Create(ctx, userID, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := deps.Sessions.IssueCSRF(ctx, sid, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}

	do := func(method, path, body string, withAuth bool) *httptest.ResponseRecorder {
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if withAuth {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
			req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
			req.Header.Set(auth.CSRFHeaderName, csrf)
			req.Header.Set("Origin", "http://localhost:3000")
		}
		rr := httptest.NewRecorder()
		rt.ServeHTTP(rr, req)
		return rr
	}

	noStore := []struct {
		name     string
		method   string
		path     string
		body     string
		withAuth bool
	}{
		{"login", http.MethodPost, "/api/auth/login", `{"identifier":"nobody","password":"wrong"}`, false},
		{"register", http.MethodPost, "/api/auth/register", `{}`, false},
		{"2fa setup", http.MethodPost, "/api/2fa/setup", `{}`, true},
		{"me", http.MethodGet, "/api/me", "", true},
		{"sessions", http.MethodGet, "/api/sessions", "", true},
	}
	for _, tc := range noStore {
		rr := do(tc.method, tc.path, tc.body, tc.withAuth)
		if rr.Code >= 400 && tc.name != "login" && tc.name != "register" {
			t.Fatalf("%s status = %d（body=%q）", tc.name, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q，期望 no-store（status=%d）", tc.name, got, rr.Code)
		}
		if got := rr.Header().Get("Pragma"); got != "no-cache" {
			t.Fatalf("%s Pragma = %q，期望 no-cache", tc.name, got)
		}
	}

	// geoip 显式覆盖默认值：按客户端缓存 5 分钟。
	rr := do(http.MethodGet, "/api/geoip", "", false)
	if rr.Code != http.StatusOK {
		t.Fatalf("geoip status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, max-age=300" {
		t.Fatalf("geoip Cache-Control = %q，期望 private, max-age=300", got)
	}
	if got := rr.Header().Get("Pragma"); got == "no-cache" {
		t.Fatal("geoip 覆盖 Cache-Control 后不应仍带 Pragma: no-cache 语义冲突")
	}
}
