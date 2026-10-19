package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// /pg/* 会话中继的单元验收（M6c Task 13 / relay-01 §1.5）：
//   - 仅会话 Cookie 可调，任何 Authorization 头（即使会话有效）→ 403；
//   - 路径重写到 /v1 后复用同一条中继管线（计费照常、TokenID=0）；
//   - 模型不在当前分组 → 403 且不进入计费；
//   - 未支持的 /pg/* → 404 OpenAI 信封；
//   - 同一 mux 上 /v1 的令牌路径行为不变。

type playgroundGroupsFake struct {
	mu        sync.Mutex
	group     string
	groups    []store.UsableGroup
	userErr   error
	groupsErr error
	calls     int
}

func (f *playgroundGroupsFake) UserGroup(_ context.Context, _ int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.userErr != nil {
		return "", f.userErr
	}
	return f.group, nil
}

func (f *playgroundGroupsFake) UsableGroupsForUser(_ context.Context, _ int64) ([]store.UsableGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.groupsErr != nil {
		return nil, f.groupsErr
	}
	return f.groups, nil
}

type playgroundTestEnv struct {
	t        *testing.T
	mux      *http.ServeMux
	sessions *auth.SessionManager
	groups   *playgroundGroupsFake
	channels *fakeRelayChannelStore
	billing  *recordingBilling
	// rc 是路由层实际构造的 RelayContext（由 Billing 工厂捕获）。
	rc *contract.RelayContext
}

func newPlaygroundTestEnv(t *testing.T, groups *playgroundGroupsFake) *playgroundTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for _, frame := range []string{
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}` + "\n\n",
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
				`data: {"id":"c1","object":"chat.completion.chunk","model":"gpt-test","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}` + "\n\n",
				"data: [DONE]\n\n",
			} {
				_, _ = io.WriteString(w, frame)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	sealer := NewSecretSealer(relaySealerMaster)
	baseURL := upstream.URL
	channels := &fakeRelayChannelStore{
		candidates: []contract.ChannelCandidate{{ChannelID: int(relayTestChannelID), Priority: 10, Weight: 1}},
		channels: map[int64]*store.Channel{
			relayTestChannelID: {
				ID:              relayTestChannelID,
				Name:            "pg-upstream",
				Type:            string(contract.ChannelTypeOpenAI),
				BaseURL:         &baseURL,
				Credentials:     sealedTestCredentials(t, sealer),
				SupportedModels: []byte(`["gpt-test"]`),
				ModelMapping:    []byte(`{}`),
				Status:          1,
				CreatedAt:       time.Now().UTC(),
			},
		},
		models: []string{"gpt-test"},
	}
	tokens := newFakeRelayTokenStore(relayTestToken, &auth.TokenMeta{
		ID: relayTestTokenID, UserID: relayTestUserID, Status: "enabled", UnlimitedQuota: true,
	})
	billing := newRecordingBilling()
	env := &playgroundTestEnv{
		t: t, sessions: sessions, groups: groups, channels: channels, billing: billing,
	}
	handler := NewRelayHandler(RelayHandlerOptions{
		Channels:   channels,
		Tokens:     tokens,
		Access:     &fakeRelayAccess{role: "admin", status: "enabled"},
		Sealer:     sealer,
		Client:     upstream.Client(),
		UserGroups: groups,
		Billing: func(rc *contract.RelayContext) contract.BillingSession {
			env.rc = rc
			return billing
		},
	})
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterPlayground(mux, sessions, "")
	env.mux = mux
	return env
}

// session 创建会话并签发 CSRF，返回 Cookie + 头所需的值。
func (e *playgroundTestEnv) session(t *testing.T) (string, string) {
	t.Helper()
	sid, err := e.sessions.Create(context.Background(), 42, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	csrf := auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		t.Fatalf("issue csrf: %v", err)
	}
	return sid, csrf
}

func (e *playgroundTestEnv) do(t *testing.T, path, body, sid, csrf string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
		req.Header.Set("Origin", "http://example.com")
	}
	if csrf != "" {
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: csrf})
		req.Header.Set(auth.CSRFHeaderName, csrf)
	}
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, req)
	return w
}

func newSessionOnlyGroups() *playgroundGroupsFake {
	return &playgroundGroupsFake{
		group: "default",
		groups: []store.UsableGroup{{
			Name: "default", Ratio: 100, Models: []string{"gpt-test"},
		}},
	}
}

const chatBody = `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`
const chatStreamBody = `{"model":"gpt-test","messages":[{"role":"user","content":"hello"}],"stream":true}`

// TestPlaygroundSessionStreamsAndBills：会话可调、走流式桥接、计费会话的
// rc 是会话身份（TokenID=0）且用 user 分组路由。
func TestPlaygroundSessionStreamsAndBills(t *testing.T) {
	env := newPlaygroundTestEnv(t, newSessionOnlyGroups())
	sid, csrf := env.session(t)

	w := env.do(t, "/pg/chat/completions", chatStreamBody, sid, csrf, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("流缺少 [DONE]: %s", w.Body.String())
	}
	if env.rc == nil {
		t.Fatal("计费会话未构造（Billing 工厂没有被调用）")
	}
	if env.rc.TokenID != 0 {
		t.Fatalf("rc.TokenID = %d, want 0（会话身份无令牌）", env.rc.TokenID)
	}
	if env.rc.UserID != 42 {
		t.Fatalf("rc.UserID = %d, want 42", env.rc.UserID)
	}
	if env.rc.UsingGroup != "default" || env.rc.UserGroup != "default" {
		t.Fatalf("分组 = %q/%q, want default/default", env.rc.UsingGroup, env.rc.UserGroup)
	}
	env.channels.mu.Lock()
	lastGroup := env.channels.lastGroup
	env.channels.mu.Unlock()
	if lastGroup != "default" {
		t.Fatalf("候选查询分组 = %q, want default", lastGroup)
	}
	if env.billing.terminateCount() != 1 {
		t.Fatalf("Terminate 次数 = %d, want 1（成功流恰好结算一次）", env.billing.terminateCount())
	}
}

// TestPlaygroundRejectsBearerEvenWithSession：TC-A-26。
func TestPlaygroundRejectsBearerEvenWithSession(t *testing.T) {
	env := newPlaygroundTestEnv(t, newSessionOnlyGroups())
	sid, csrf := env.session(t)

	w := env.do(t, "/pg/chat/completions", chatBody, sid, csrf, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+relayTestToken)
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403（body=%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"session_required"`) {
		t.Fatalf("body = %s, want session_required", w.Body.String())
	}
	if env.billing.terminateCount() != 0 || env.billing.preConsumeCount() != 0 {
		t.Fatal("被拒绝的 Bearer 请求不得进入计费")
	}
}

// TestPlaygroundRequiresCSRFAndSession：无 Cookie → 401；有 Cookie 无 CSRF → 403。
func TestPlaygroundRequiresSessionAndCSRF(t *testing.T) {
	env := newPlaygroundTestEnv(t, newSessionOnlyGroups())

	if w := env.do(t, "/pg/chat/completions", chatBody, "", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("无会话 status = %d, want 401（body=%s）", w.Code, w.Body.String())
	}
	sid, _ := env.session(t)
	if w := env.do(t, "/pg/chat/completions", chatBody, sid, "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF status = %d, want 403（body=%s）", w.Code, w.Body.String())
	}
}

// TestPlaygroundModelNotInGroupIs403BeforeBilling：TC-F-02-19-06 的单元部分。
func TestPlaygroundModelNotInGroupIs403BeforeBilling(t *testing.T) {
	env := newPlaygroundTestEnv(t, newSessionOnlyGroups())
	sid, csrf := env.session(t)

	w := env.do(t, "/pg/chat/completions",
		`{"model":"not-in-group","messages":[{"role":"user","content":"x"}]}`, sid, csrf, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403（body=%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"model_not_available"`) {
		t.Fatalf("body = %s, want model_not_available", w.Body.String())
	}
	if env.billing.preConsumeCount() != 0 {
		t.Fatalf("PreConsume 次数 = %d, want 0（403 必须在预扣前）", env.billing.preConsumeCount())
	}
}

// TestPlaygroundUnknownPathIs404：未支持的 /pg/* 返回 OpenAI 信封 404。
func TestPlaygroundUnknownPathIs404(t *testing.T) {
	env := newPlaygroundTestEnv(t, newSessionOnlyGroups())
	sid, csrf := env.session(t)

	w := env.do(t, "/pg/files", `{}`, sid, csrf, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（body=%s）", w.Code, w.Body.String())
	}
	var envelope relayErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("404 必须是 OpenAI 信封: %v（body=%s）", err, w.Body.String())
	}
	if envelope.Error.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", envelope.Error.Code)
	}
}

// TestPlaygroundDoesNotWeakenV1TokenPath：同一 mux 上 /v1 仍由 RelayAuth
// 校验令牌，Bearer 请求照常 200（回归：/pg 的新增不得改变 /v1 鉴权）。
func TestPlaygroundDoesNotWeakenV1TokenPath(t *testing.T) {
	env := newPlaygroundTestEnv(t, newSessionOnlyGroups())

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+relayTestToken)
	w := httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/v1 status = %d, want 200（body=%s）", w.Code, w.Body.String())
	}
	if env.rc == nil || env.rc.TokenID != int(relayTestTokenID) {
		t.Fatalf("rc.TokenID = %v, want %d", env.rc, relayTestTokenID)
	}

	// 无令牌的 /v1 仍必须 401（会话 Cookie 不能用于 /v1）。
	sid, _ := env.session(t)
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	w = httptest.NewRecorder()
	env.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/v1 仅会话 status = %d, want 401", w.Code)
	}
}
