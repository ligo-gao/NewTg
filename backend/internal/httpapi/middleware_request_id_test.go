package httpapi

// M6c Task 7：全局 X-Request-ID（R-PUB-10）测试。

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestRequestIDAdoptsValidInbound(t *testing.T) {
	var seen string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/status", http.NoBody)
	req.Header.Set("X-Request-ID", "trace-abc.123_XYZ")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Request-ID"); got != "trace-abc.123_XYZ" {
		t.Fatalf("合法入站值应原样采纳，得到 %q", got)
	}
	if seen != "trace-abc.123_XYZ" {
		t.Fatalf("context 中的 Request-ID = %q", seen)
	}
}

func TestRequestIDGeneratesForInvalidInbound(t *testing.T) {
	for _, inbound := range []string{
		"",                       // 缺失
		"has space",              // 非法字符
		"bad\nheader",            // CR/LF 注入尝试
		strings.Repeat("a", 129), // 超长
	} {
		handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/status", http.NoBody)
		if inbound != "" {
			req.Header.Set("X-Request-ID", inbound)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		got := rr.Header().Get("X-Request-ID")
		if !uuidV4Pattern.MatchString(got) {
			t.Fatalf("入站值 %q 应生成 UUID v4，得到 %q", inbound, got)
		}
	}
}

func TestRequestIDCoversErrorResponses(t *testing.T) {
	// 404/500 等错误路径同样经过中间件（header 在 handler 之前写入）。
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/me", http.NoBody))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
	if rr.Header().Get("X-Request-ID") == "" {
		t.Fatal("错误响应必须带 X-Request-ID")
	}
}

func TestRequestIDDoesNotFightDownstreamSets(t *testing.T) {
	// 中继端点自行设置 X-Request-ID：中间件先写、下游后写，最终以中继的
	// 计费幂等键为准（不出现两个 id 互相覆盖的歧义）。
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "relay-billing-id")
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", http.NoBody))
	if got := rr.Header().Get("X-Request-ID"); got != "relay-billing-id" {
		t.Fatalf("下游覆盖应生效，得到 %q", got)
	}
}
