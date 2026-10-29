package httpapi

// 本文件固定 RelayAuth 的载体提取与 M2 客户端 IP 策略（relay-01 §3.1/§3.4）。
// 与 relay_endpoints_test.go 同包：复用其中的内存 fake 与 httptest 装配。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/store"
	"newtg/gateway/internal/tokenstore"
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

// TestParseTrustedProxiesRejectsBroadPrefixes 覆盖 R-02：RelayAuth 的令牌 IP
// 白名单判定必须与 internal/clientip 共用同一套 fail-safe 口径。0.0.0.0/0、
// ::/0 或过宽前缀一旦被接受，任意客户端都能自带 XFF 冒充 allow_ips 里的地址，
// 令牌的 IP 白名单形同虚设。
func TestParseTrustedProxiesRejectsBroadPrefixes(t *testing.T) {
	for _, raw := range []string{"0.0.0.0/0", "::/0", "172.28.0.0/7", "2001:db8::/15", "not-an-ip"} {
		if got := parseTrustedProxies(raw); len(got) != 0 {
			t.Fatalf("parseTrustedProxies(%q) = %v，期望空集（畸形/过宽条目不得生效）", raw, got)
		}
	}
}

// ---------- 鉴权阶段的拒绝行（失败排障：401/403 的 requestId 可查） ----------

// authRefusalRequestID 从响应头取 requestId：消息里的 `(requestId: …)` 与
// X-Request-ID 必须与落库行一致，否则管理员拿客户端报的 ID 查不到任何东西。
func authRefusalRequestID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		t.Fatal("X-Request-ID 为空")
	}
	if !strings.Contains(w.Body.String(), "requestId: "+id) {
		t.Fatalf("响应消息 = %s, want 包含 requestId: %s", w.Body.String(), id)
	}
	return id
}

// TestRelayAuthRecordsRefusals 覆盖鉴权阶段应记录的五种拒绝：
// 凭证缺失、未知令牌、令牌不可用、用户被封禁、IP 白名单。
func TestRelayAuthRecordsRefusals(t *testing.T) {
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`

	t.Run("missing credential records anonymous refusal", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("auth failure must not reach upstream")
		}))
		defer upstream.Close()
		env := newRelayTestEnv(t, upstream, nil)
		rec := &fakeRefusalRecorder{}
		env.handler.refusals = rec

		w := env.do(t, http.MethodPost, "/v1/chat/completions", body, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		requestID := authRefusalRequestID(t, w)

		refusals := rec.snapshot()
		if len(refusals) != 1 {
			t.Fatalf("refusals = %d, want 1", len(refusals))
		}
		got := refusals[0]
		if got.RequestID != requestID {
			t.Fatalf("落库 requestId = %q, want %q（必须与响应一致）", got.RequestID, requestID)
		}
		if got.Stage != billing.RefusalStageAuth {
			t.Fatalf("stage = %q, want auth", got.Stage)
		}
		if got.UserID != 0 || got.TokenID != nil {
			t.Fatalf("身份 = %d/%v, want 0/nil（令牌未解析出来，不得猜身份）", got.UserID, got.TokenID)
		}
		if got.ErrorClass != "unauthorized" {
			t.Fatalf("error_class = %q, want unauthorized", got.ErrorClass)
		}
		if got.Endpoint != "chat_completions" {
			t.Fatalf("endpoint = %q, want chat_completions", got.Endpoint)
		}
	})

	t.Run("unknown token records anonymous refusal", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("auth failure must not reach upstream")
		}))
		defer upstream.Close()
		env := newRelayTestEnv(t, upstream, nil)
		rec := &fakeRefusalRecorder{}
		env.handler.refusals = rec

		w := env.do(t, http.MethodPost, "/v1/chat/completions", body,
			map[string]string{"Authorization": "Bearer " + "sk-" + strings.Repeat("z", 48)})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		refusals := rec.snapshot()
		if len(refusals) != 1 || refusals[0].UserID != 0 {
			t.Fatalf("refusals = %+v, want 一条 user 0 的记录", refusals)
		}
	})

	t.Run("unusable token records owner", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("auth failure must not reach upstream")
		}))
		defer upstream.Close()
		env := newRelayTestEnv(t, upstream, nil)
		rec := &fakeRefusalRecorder{}
		env.handler.refusals = rec
		env.tokens.mu.Lock()
		env.tokens.byHash[auth.HashTokenKey(relayTestToken)].Status = "disabled"
		env.tokens.mu.Unlock()

		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		refusals := rec.snapshot()
		if len(refusals) != 1 {
			t.Fatalf("refusals = %d, want 1", len(refusals))
		}
		got := refusals[0]
		if got.UserID != relayTestUserID {
			t.Fatalf("user = %d, want %d", got.UserID, relayTestUserID)
		}
		if got.TokenID == nil || *got.TokenID != relayTestTokenID {
			t.Fatalf("token = %v, want %d", got.TokenID, relayTestTokenID)
		}
	})

	t.Run("banned user records auth_user_banned", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("auth failure must not reach upstream")
		}))
		defer upstream.Close()
		env := newRelayTestEnv(t, upstream, nil)
		rec := &fakeRefusalRecorder{}
		env.handler.refusals = rec
		env.access.mu.Lock()
		env.access.status = "disabled"
		env.access.mu.Unlock()

		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		refusals := rec.snapshot()
		if len(refusals) != 1 || refusals[0].ErrorClass != "auth_user_banned" {
			t.Fatalf("refusals = %+v, want auth_user_banned", refusals)
		}
	})

	t.Run("ip deny records access_denied", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("auth failure must not reach upstream")
		}))
		defer upstream.Close()
		env := newRelayTestEnv(t, upstream, nil)
		rec := &fakeRefusalRecorder{}
		env.handler.refusals = rec
		env.tokens.mu.Lock()
		env.tokens.byHash[auth.HashTokenKey(relayTestToken)].AllowIPs = []string{"10.0.0.9"}
		env.tokens.mu.Unlock()

		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
		refusals := rec.snapshot()
		if len(refusals) != 1 || refusals[0].ErrorClass != "access_denied" {
			t.Fatalf("refusals = %+v, want access_denied", refusals)
		}
	})
}

// TestRelayAuthSkipsRefusalForInfrastructureFailures 钉死「不记网关自身故障」：
// 令牌存储故障与缺失的令牌存储都返回 401，但不得写 refused 行——把存储故障
// 说成用户请求的问题正是本功能要防的倒置判定。
func TestRelayAuthSkipsRefusalForInfrastructureFailures(t *testing.T) {
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`

	t.Run("token store failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("auth failure must not reach upstream")
		}))
		defer upstream.Close()
		env := newRelayTestEnv(t, upstream, nil)
		rec := &fakeRefusalRecorder{}
		env.handler.refusals = rec
		env.tokens.mu.Lock()
		env.tokens.resolveErr = errors.New("redis unavailable")
		env.tokens.mu.Unlock()

		w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401（fail-closed 语义不变）", w.Code)
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("refusals = %d, want 0（存储故障不得记成用户拒绝）", got)
		}
	})

	t.Run("missing token store", func(t *testing.T) {
		rec := &fakeRefusalRecorder{}
		mux := http.NewServeMux()
		mux.Handle("/v1/", RelayAuthWithRecorder(nil, &fakeRelayAccess{status: "enabled"}, nil, rec, nil)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("missing token store must not reach the handler")
			})))
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+relayTestToken)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
		if got := len(rec.snapshot()); got != 0 {
			t.Fatalf("refusals = %d, want 0（接线故障不得记成用户拒绝）", got)
		}
	})
}

// TestRelayAuthRecordsTokenstoreRejections 固定 P2-2 的缓存层接线口径：
// 生产 relay 改为 tokenstore.Store 之后，「令牌不存在/禁用/过期」由它直接
// 以错误返回（不再返回 meta 交给 tokenUsable 判定）。这三类都是确定的用户
// 侧事实，必须照旧写 refused 行——否则换成缓存层会让禁用/过期令牌的拒绝
// 从用量日志里消失，而存储故障仍必须保持「不记」。
func TestRelayAuthRecordsTokenstoreRejections(t *testing.T) {
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name string
		err  error
	}{
		{name: "invalid token", err: tokenstore.ErrInvalidToken},
		{name: "disabled token", err: tokenstore.ErrTokenDisabled},
		{name: "expired token", err: tokenstore.ErrTokenExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("auth failure must not reach upstream")
			}))
			defer upstream.Close()
			env := newRelayTestEnv(t, upstream, nil)
			rec := &fakeRefusalRecorder{}
			env.handler.refusals = rec
			env.tokens.mu.Lock()
			env.tokens.resolveErr = tc.err
			env.tokens.mu.Unlock()

			w := env.doAuthed(t, http.MethodPost, "/v1/chat/completions", body)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			if got := len(rec.snapshot()); got != 1 {
				t.Fatalf("refusals = %d, want 1（确定的令牌拒绝必须落库）", got)
			}
		})
	}
}
