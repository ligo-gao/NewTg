package httpapi

// 本文件固定 RelayAuth 的载体提取与 M2 客户端 IP 策略（relay-01 §3.1/§3.4）。
// 与 relay_endpoints_test.go 同包：复用其中的内存 fake 与 httptest 装配。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/store"
)

func TestRelayCredentialCarriers(t *testing.T) {
	const key = "sk-carrier-test"
	cases := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		want    string
		wantOK  bool
	}{
		{
			name:    "Authorization Bearer 总是生效",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": "Bearer " + key},
			want:    key,
			wantOK:  true,
		},
		{
			name:    "Authorization 裸 key",
			path:    "/v1/chat/completions",
			headers: map[string]string{"Authorization": key},
			want:    key,
			wantOK:  true,
		},
		{
			name:    "x-api-key 仅模型/消息路径生效",
			path:    "/v1/models",
			headers: map[string]string{"x-api-key": key},
			want:    key,
			wantOK:  true,
		},
		{
			name:    "x-api-key 在 chat 路径不生效",
			path:    "/v1/chat/completions",
			headers: map[string]string{"x-api-key": key},
			wantOK:  false,
		},
		{
			name:    "x-goog-api-key 在 messages 路径生效",
			path:    "/v1/messages",
			headers: map[string]string{"x-goog-api-key": key},
			want:    key,
			wantOK:  true,
		},
		{
			name:    "x-goog-api-key 在模型路径生效",
			path:    "/v1/models",
			headers: map[string]string{"x-goog-api-key": key},
			want:    key,
			wantOK:  true,
		},
		{
			name:   "?key= 在 /v1/models 生效",
			path:   "/v1/models?key=" + key,
			want:   key,
			wantOK: true,
		},
		{
			name:   "?key= 在 /v1beta/models 生效",
			path:   "/v1beta/models?key=" + key,
			want:   key,
			wantOK: true,
		},
		{
			name:   "?key= 在 chat 路径不生效",
			path:   "/v1/chat/completions?key=" + key,
			wantOK: false,
		},
		{
			name:    "Authorization 优先于兼容载体",
			path:    "/v1/models?key=query-key",
			headers: map[string]string{"Authorization": "Bearer " + key, "x-api-key": "header-key"},
			want:    key,
			wantOK:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tc.path, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			got, ok := relayCredentialFromRequest(req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("credential = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRelayDeletedUserMapsTo401(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when the user row is gone")
	}))
	defer upstream.Close()

	env := newRelayTestEnv(t, upstream, nil)
	env.access.mu.Lock()
	env.access.err = store.ErrUserNotFound
	env.access.mu.Unlock()

	w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401（已删除用户 = 身份失效，不是 500）; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"type":"new_api_error"`) {
		t.Fatalf("body = %s, want OpenAI error envelope", w.Body.String())
	}
}

func TestParseTrustedProxies(t *testing.T) {
	prefixes := parseTrustedProxies("10.0.0.0/8, 192.0.2.1 , not-an-ip, 2001:db8::/32")
	if len(prefixes) != 3 {
		t.Fatalf("parseTrustedProxies returned %d prefixes, want 3（非法条目必须被忽略）", len(prefixes))
	}
	if parseTrustedProxies("") != nil {
		t.Fatal("空配置必须返回 nil（不信任任何代理）")
	}
}
