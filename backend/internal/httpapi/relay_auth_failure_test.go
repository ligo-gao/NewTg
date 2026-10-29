package httpapi

// R-03：/v1 预认证失败限流的中间件行为测试。
//
// 设计权威：docs/2026-10-28-NEWTG漏洞报告复核与整改方案.md R-03。
//
// 覆盖验收项：
//   - 60 次伪造 token 后第 61 次 429（且第 61 次不查库）；
//   - 换 IP 不受影响；
//   - 真实 token 不受影响（成功不计数、不清零）；
//   - Redis 不可用时 fail-open（正常请求不被限流打成 500/429，且打 WARN）；
//   - RelayHandler.AuthMiddleware 生产装配确实接上了同一限流接缝。
//
// 本文件自带最小 fake，避免依赖同包其它测试文件的 helper（并行开发时
// 它们可能处于中间状态）。

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/store"
)

const (
	authFailureTestToken       = "nkt-" + "a234567890123456789012345678901234567890123456"
	authFailureTokenID   int64 = 7
	authFailureUserID    int64 = 1
)

// authFailureTokenStore 是只认识一个真实令牌的内存 fake。
type authFailureTokenStore struct {
	mu     sync.Mutex
	byHash map[string]*auth.TokenMeta
	calls  int
	err    error
}

func newAuthFailureTokenStore() *authFailureTokenStore {
	meta := &auth.TokenMeta{
		ID:             authFailureTokenID,
		UserID:         authFailureUserID,
		Status:         "enabled",
		UnlimitedQuota: true,
	}
	return &authFailureTokenStore{
		byHash: map[string]*auth.TokenMeta{auth.HashTokenKey(authFailureTestToken): meta},
	}
}

func (f *authFailureTokenStore) Resolve(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	meta, ok := f.byHash[keyHash]
	if !ok {
		return nil, store.ErrTokenNotFound
	}
	cp := *meta
	return &cp, nil
}

func (f *authFailureTokenStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// authFailureAccess 是 users.role/status 的内存 fake。
type authFailureAccess struct {
	status string
	err    error
}

func (f *authFailureAccess) UserAccess(context.Context, int64) (string, []string, string, error) {
	return "admin", nil, f.status, f.err
}

// authFailureRig 直接包住 RelayAuth 中间件（不经过 chat 管线），
// 这样断言只针对 R-03，不会被 B10/B11/B12 的 fail-closed 行为干扰。
type authFailureRig struct {
	store   *authFailureTokenStore
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	hits    *atomic.Int32
	logs    *authLogBuffer
	handler http.Handler
}

// authLogBuffer 让 slog 的写入与断言在 -race 下安全。
type authLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *authLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *authLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newAuthFailureRig(t *testing.T) *authFailureRig {
	t.Helper()
	mr := miniredis.RunT(t)
	// MaxRetries=-1：Redis 断开时立即报错，测试不必等待退避重试。
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	store := newAuthFailureTokenStore()
	access := &authFailureAccess{status: "enabled"}
	logs := &authLogBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hits := &atomic.Int32{}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	rig := &authFailureRig{
		store: store,
		mr:    mr,
		rdb:   rdb,
		hits:  hits,
		logs:  logs,
	}
	rig.handler = relayAuthWithGuard(store, access, nil, nil, logger,
		ratelimit.New(rdb))(next)
	return rig
}

// do 发送一次请求；ip 决定 RemoteAddr（clientip 的唯一取值路径从这里解析）。
func (r *authFailureRig) do(t *testing.T, ip, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	req.RemoteAddr = ip + ":44321"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, req)
	return w
}

// fakeToken 返回一个形态合法但存储中不存在的令牌（每次不同）。
func authFailureFakeToken(i int) string {
	return fmt.Sprintf("sk-%048d", i)
}

func TestRelayAuthFailureUsesTrustedProxyClientIP(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	// 直连 peer 属于 TRUSTED_PROXIES：失败桶必须按 XFF 里的真实客户端归桶。
	handler := relayAuthWithGuard(newAuthFailureTokenStore(),
		&authFailureAccess{status: "enabled"},
		parseTrustedProxies("10.0.0.0/8"), nil,
		slog.New(slog.NewTextHandler(&authLogBuffer{}, nil)),
		ratelimit.New(rdb))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	do := func(peer, xff, token string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.RemoteAddr = peer + ":44321"
		req.Header.Set("X-Forwarded-For", xff)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	const edge = "10.0.0.5"
	clientA, clientB := "198.51.100.1", "198.51.100.2"
	for i := 0; i < ratelimit.AuthFailureLimit; i++ {
		if code := do(edge, clientA, authFailureFakeToken(i)); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次状态 = %d，期望 401", i+1, code)
		}
	}
	if code := do(edge, clientA, authFailureFakeToken(1000)); code != http.StatusTooManyRequests {
		t.Fatalf("同一 XFF 客户端第 61 次 = %d，期望 429", code)
	}
	// 同一 edge、不同真实客户端：不得连坐。
	if code := do(edge, clientB, authFailureFakeToken(1001)); code != http.StatusUnauthorized {
		t.Fatalf("另一 XFF 客户端状态 = %d，期望 401", code)
	}
	// 不可信 peer 自带 XFF 必须被忽略：否则伪造 XFF 就能命中别人的桶。
	if code := do("203.0.113.50", clientA, authFailureFakeToken(1002)); code != http.StatusUnauthorized {
		t.Fatalf("不可信 peer 伪造 XFF 状态 = %d，期望 401（XFF 必须被忽略）", code)
	}
}

func TestRelayAuthFailureBlocksAfterThreshold(t *testing.T) {
	rig := newAuthFailureRig(t)
	const ip = "203.0.113.10"

	for i := 0; i < ratelimit.AuthFailureLimit; i++ {
		w := rig.do(t, ip, "/v1/chat/completions", authFailureFakeToken(i))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次伪造 token 状态 = %d，期望 401；body=%s", i+1, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid token") {
			t.Fatalf("第 %d 次 401 文案被改变：%s", i+1, w.Body.String())
		}
	}
	callsBefore := rig.store.callCount()
	if callsBefore != ratelimit.AuthFailureLimit {
		t.Fatalf("令牌校验次数 = %d，期望 %d", callsBefore, ratelimit.AuthFailureLimit)
	}

	w := rig.do(t, ip, "/v1/chat/completions", authFailureFakeToken(ratelimit.AuthFailureLimit))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次状态 = %d，期望 429；body=%s",
			ratelimit.AuthFailureLimit+1, w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 必须带 Retry-After（锁剩余时长）")
	}
	// 预认证拦截：被限流的请求不得再哈希/查库。
	if got := rig.store.callCount(); got != callsBefore {
		t.Fatalf("第 61 次限流请求仍查了令牌库：calls %d → %d", callsBefore, got)
	}
	if strings.Contains(w.Body.String(), "invalid token") {
		t.Fatalf("429 不得复用 401 的 invalid token 文案（避免泄露触发原因）：%s", w.Body.String())
	}

	// Claude 端点族同样返回 429，但信封必须是 Claude 形（R-W18-04）。
	w = rig.do(t, ip, "/v1/messages", authFailureFakeToken(9999))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("/v1/messages 限流状态 = %d，期望 429", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, `"type":"error"`) ||
		!strings.Contains(body, `"rate_limit_error"`) {
		t.Fatalf("/v1/messages 429 必须是 Claude 错误信封，body=%s", body)
	}
}

func TestRelayAuthFailureIsPerIP(t *testing.T) {
	rig := newAuthFailureRig(t)
	blockedIP := "203.0.113.11"
	otherIP := "203.0.113.12"

	for i := 0; i < ratelimit.AuthFailureLimit; i++ {
		if w := rig.do(t, blockedIP, "/v1/chat/completions", authFailureFakeToken(i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次状态 = %d，期望 401", i+1, w.Code)
		}
	}
	if w := rig.do(t, blockedIP, "/v1/chat/completions", authFailureFakeToken(100)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("超限 IP 状态 = %d，期望 429", w.Code)
	}

	// 换 IP：同样的伪造 token 仍走正常 401，真实 token 仍 200。
	if w := rig.do(t, otherIP, "/v1/chat/completions", authFailureFakeToken(101)); w.Code != http.StatusUnauthorized {
		t.Fatalf("其它 IP 伪造 token 状态 = %d，期望 401（不得被邻居连坐）", w.Code)
	}
	if w := rig.do(t, otherIP, "/v1/chat/completions", authFailureTestToken); w.Code != http.StatusOK {
		t.Fatalf("其它 IP 真实 token 状态 = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	if got := rig.hits.Load(); got != 1 {
		t.Fatalf("下游 handler 命中 %d 次，期望 1（只有真实 token 放行）", got)
	}
}

func TestRelayAuthSuccessDoesNotCountOrResetFailures(t *testing.T) {
	rig := newAuthFailureRig(t)
	const ip = "203.0.113.13"

	// 先积累一半失败。
	for i := 0; i < ratelimit.AuthFailureLimit/2; i++ {
		if w := rig.do(t, ip, "/v1/chat/completions", authFailureFakeToken(i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败状态 = %d，期望 401", i+1, w.Code)
		}
	}
	// 中间夹一次成功：既不能给失败计数 +1，也不能把已有计数清零。
	if w := rig.do(t, ip, "/v1/chat/completions", authFailureTestToken); w.Code != http.StatusOK {
		t.Fatalf("真实 token 状态 = %d，期望 200（成功不受失败桶影响）", w.Code)
	}
	// 再积累另一半失败，凑满阈值。
	for i := 0; i < ratelimit.AuthFailureLimit/2; i++ {
		if w := rig.do(t, ip, "/v1/chat/completions", authFailureFakeToken(1000+i)); w.Code != http.StatusUnauthorized {
			t.Fatalf("后半第 %d 次失败状态 = %d，期望 401", i+1, w.Code)
		}
	}
	if w := rig.do(t, ip, "/v1/chat/completions", authFailureFakeToken(2000)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("计数满 %d 次后状态 = %d，期望 429（成功请求不得清零或计数）",
			ratelimit.AuthFailureLimit, w.Code)
	}

	// 成功请求本身不建失败桶：换一个只发成功请求的 IP，键集合里不应出现 authFailure。
	const successOnlyIP = "203.0.113.14"
	for i := 0; i < 3; i++ {
		if w := rig.do(t, successOnlyIP, "/v1/chat/completions", authFailureTestToken); w.Code != http.StatusOK {
			t.Fatalf("成功请求第 %d 次状态 = %d，期望 200", i+1, w.Code)
		}
	}
	for _, key := range rig.mr.Keys() {
		if strings.Contains(key, ratelimit.AuthFailureKey(successOnlyIP)) {
			t.Fatalf("成功请求不应创建失败计数键：%s", key)
		}
	}
}

func TestRelayAuthFailureLimiterFailsOpenOnRedisError(t *testing.T) {
	rig := newAuthFailureRig(t)
	const ip = "203.0.113.15"
	rig.mr.Close() // 模拟 Redis 不可用

	if w := rig.do(t, ip, "/v1/chat/completions", authFailureTestToken); w.Code != http.StatusOK {
		t.Fatalf("Redis 不可用但真实 token 状态 = %d，期望 200（fail-open）", w.Code)
	}
	if got := rig.hits.Load(); got != 1 {
		t.Fatalf("下游 handler 命中 %d 次，期望 1", got)
	}
	// 失败请求仍按原语义 401；限流故障不得把它变成 500。
	if w := rig.do(t, ip, "/v1/chat/completions", authFailureFakeToken(1)); w.Code != http.StatusUnauthorized {
		t.Fatalf("Redis 不可用时伪造 token 状态 = %d，期望 401（不得 500）", w.Code)
	}
	if logs := rig.logs.String(); !strings.Contains(logs, "fail-open") {
		t.Fatalf("Redis 故障必须打 WARN 说明 fail-open；日志=%s", logs)
	}
}

// TestRelayAuthFailureWiredThroughHandler 验证生产装配路径：
// RelayHandler 已有的 RateLimiter 接缝（*ratelimit.Limiter）被 AuthMiddleware
// 识别为失败限流器，无需在 assembly 里新增字段。
func TestRelayAuthFailureWiredThroughHandler(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	h := &RelayHandler{
		tokens:      newAuthFailureTokenStore(),
		access:      &authFailureAccess{status: "enabled"},
		log:         slog.New(slog.NewTextHandler(&authLogBuffer{}, nil)),
		rateLimiter: ratelimit.New(rdb),
	}
	handler := h.AuthMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	const ip = "203.0.113.16"
	do := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.RemoteAddr = ip + ":44321"
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < ratelimit.AuthFailureLimit; i++ {
		if code := do(authFailureFakeToken(i)); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次状态 = %d，期望 401", i+1, code)
		}
	}
	if code := do(authFailureFakeToken(500)); code != http.StatusTooManyRequests {
		t.Fatalf("经 RelayHandler 装配的第 61 次状态 = %d，期望 429", code)
	}
}
