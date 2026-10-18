package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件是 Task 10（F-01-15 换绑邮箱 / F-01-17 删除账号 / F-01-22 通知偏好）的
// 单元验收。全部依赖都是内存假实现（miniredis + 进程内用户表 + 假审计 Execer），
// 未打 integration 标签时不触碰外部 PG/Redis（GC-8）。
//
// 路由钩子：Task 10 不修改 routes.go，由路由装配方调用
// (*AccountHandlers).RegisterAccountRoutes（见 handlers_account.go 的文档）。

const accountTestBaseURL = "https://app.example.com"

// accountTestPassword 与其它 httpapi 单测共用 authTestPassword / authTestPasswordHash，
// 避免每个用例重复支付 argon2id 成本。

// ---------------------------------------------------------------------------
// 内存仓储
// ---------------------------------------------------------------------------

// accountTestToken 是一条仍是「有效」的 API 令牌（只保存哈希，与生产同口径）。
type accountTestToken struct {
	ID     int64
	UserID int64
}

// accountTestRepo 复用 Task 6 的内存用户表（AuthRepo 由嵌入提供），补上
// AccountRepo 的删除/换邮箱/通知偏好读写。真实实现见 SQLAccountRepo。
type accountTestRepo struct {
	*fakeAuthRepo

	mu       sync.Mutex
	tokens   map[string]accountTestToken // key_hash → token
	prefs    map[int64]json.RawMessage
	nextTok  int64
	deleted  []int64
	emailErr error
}

func newAccountTestRepo() *accountTestRepo {
	return &accountTestRepo{
		fakeAuthRepo: newFakeAuthRepo(),
		tokens:       map[string]accountTestToken{},
		prefs:        map[int64]json.RawMessage{},
	}
}

func (r *accountTestRepo) GetAccount(_ context.Context, userID int64) (*AccountRecord, error) {
	r.fakeAuthRepo.mu.Lock()
	defer r.fakeAuthRepo.mu.Unlock()
	u, ok := r.users[userID]
	if !ok {
		return nil, nil
	}
	rec := &AccountRecord{ID: u.ID, Username: u.Username, Role: u.Role, Status: u.Status}
	if u.Email != "" {
		email := u.Email
		rec.Email = &email
	}
	return rec, nil
}

func (r *accountTestRepo) ChangeEmail(_ context.Context, userID int64, newEmail string) (time.Time, error) {
	r.mu.Lock()
	err := r.emailErr
	r.mu.Unlock()
	if err != nil {
		return time.Time{}, err
	}
	r.fakeAuthRepo.mu.Lock()
	defer r.fakeAuthRepo.mu.Unlock()
	u, ok := r.users[userID]
	if !ok {
		return time.Time{}, ErrAccountNotFound
	}
	if id, taken := r.byEmail[strings.ToLower(newEmail)]; taken && id != userID {
		return time.Time{}, ErrEmailTaken
	}
	if u.Email != "" {
		delete(r.byEmail, strings.ToLower(u.Email))
	}
	u.Email = newEmail
	u.EmailVerified = true
	r.byEmail[strings.ToLower(newEmail)] = userID
	return time.Now().UTC().Truncate(time.Second), nil
}

func (r *accountTestRepo) DeleteAccount(_ context.Context, userID int64) ([]int64, error) {
	r.fakeAuthRepo.mu.Lock()
	defer r.fakeAuthRepo.mu.Unlock()
	u, ok := r.users[userID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	delete(r.users, userID)
	delete(r.byUsername, u.Username)
	if u.Email != "" {
		delete(r.byEmail, strings.ToLower(u.Email))
	}
	r.mu.Lock()
	delete(r.prefs, userID)
	var tokenIDs []int64
	for hash, tok := range r.tokens {
		if tok.UserID == userID {
			tokenIDs = append(tokenIDs, tok.ID)
			delete(r.tokens, hash)
		}
	}
	r.deleted = append(r.deleted, userID)
	r.mu.Unlock()
	return tokenIDs, nil
}

func (r *accountTestRepo) NotificationPreferences(_ context.Context, userID int64) (json.RawMessage, error) {
	r.fakeAuthRepo.mu.Lock()
	_, ok := r.users[userID]
	r.fakeAuthRepo.mu.Unlock()
	if !ok {
		return nil, ErrAccountNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.prefs[userID]
	if !ok || len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	return append(json.RawMessage(nil), raw...), nil
}

func (r *accountTestRepo) UpdateNotificationPreferences(_ context.Context, userID int64, prefs json.RawMessage) error {
	r.fakeAuthRepo.mu.Lock()
	_, ok := r.users[userID]
	r.fakeAuthRepo.mu.Unlock()
	if !ok {
		return ErrAccountNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefs[userID] = append(json.RawMessage(nil), prefs...)
	return nil
}

func (r *accountTestRepo) addToken(userID int64, keyHash string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextTok++
	r.tokens[keyHash] = accountTestToken{ID: r.nextTok, UserID: userID}
	return r.nextTok
}

func (r *accountTestRepo) tokenCount(userID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, tok := range r.tokens {
		if tok.UserID == userID {
			n++
		}
	}
	return n
}

// accountTestLookup 是 auth.TokenLookup 的内存实现：令牌被硬删除后 Resolve
// 返回 (nil, nil)，SessionAuth 的 tokenUsable(nil) 即 401。
type accountTestLookup struct{ repo *accountTestRepo }

func (l accountTestLookup) Resolve(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
	l.repo.mu.Lock()
	defer l.repo.mu.Unlock()
	tok, ok := l.repo.tokens[keyHash]
	if !ok {
		return nil, nil
	}
	return &auth.TokenMeta{
		ID:             tok.ID,
		UserID:         tok.UserID,
		Status:         "enabled",
		Group:          "default",
		UnlimitedQuota: true,
	}, nil
}

// ---------------------------------------------------------------------------
// 再认证材料 / PII 匿名化 / 通知发送的假实现
// ---------------------------------------------------------------------------

// accountTestReauthSource 返回预先登记的再认证材料；用户行被删除后返回 nil。
type accountTestReauthSource struct {
	repo      *accountTestRepo
	mu        sync.Mutex
	materials map[int64]*auth.ReauthMaterial
}

func (s *accountTestReauthSource) set(userID int64, m *auth.ReauthMaterial) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.materials[userID] = m
}

func (s *accountTestReauthSource) ReauthMaterial(_ context.Context, userID int64) (*auth.ReauthMaterial, error) {
	s.repo.fakeAuthRepo.mu.Lock()
	_, ok := s.repo.users[userID]
	s.repo.fakeAuthRepo.mu.Unlock()
	if !ok {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.materials[userID]
	if !ok {
		return nil, nil
	}
	copied := *m
	return &copied, nil
}

type anonymizeCall struct {
	UserID   int64
	Username string
}

type accountTestAnonymizer struct {
	mu    sync.Mutex
	calls []anonymizeCall
	err   error
}

func (a *accountTestAnonymizer) AnonymizeUser(_ context.Context, userID int64, anonymizedUsername string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, anonymizeCall{UserID: userID, Username: anonymizedUsername})
	return a.err
}

func (a *accountTestAnonymizer) snapshot() []anonymizeCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]anonymizeCall(nil), a.calls...)
}

type notifTestCall struct {
	Channel string
	Target  string
}

type accountTestNotifSender struct {
	mu    sync.Mutex
	calls []notifTestCall
	err   error
}

func (s *accountTestNotifSender) SendTestNotification(_ context.Context, channel, target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, notifTestCall{Channel: channel, Target: target})
	return nil
}

func (s *accountTestNotifSender) snapshot() []notifTestCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notifTestCall(nil), s.calls...)
}

// accountAuditExec 捕获 audit_logs 的 INSERT 参数（顺序与 audit.insertSQL 一致）。
type accountAuditExec struct {
	mu    sync.Mutex
	calls [][]any
}

func (e *accountAuditExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, append([]any(nil), args...))
	return accountResult(1), nil
}

func (e *accountAuditExec) snapshot() [][]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]any, len(e.calls))
	for i, call := range e.calls {
		out[i] = append([]any(nil), call...)
	}
	return out
}

type accountResult int64

func (r accountResult) LastInsertId() (int64, error) { return 0, nil }
func (r accountResult) RowsAffected() (int64, error) { return int64(r), nil }

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type accountTestEnv struct {
	t        *testing.T
	router   *Router
	handler  *AccountHandlers
	repo     *accountTestRepo
	auditEx  *accountAuditExec
	anon     *accountTestAnonymizer
	source   *accountTestReauthSource
	sender   *accountTestNotifSender
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	sessions *auth.SessionManager
	mfaCode  string // VerifyTOTP seam 唯一接受的码；空串视为无条件通过
}

// newAccountTestEnv 组装完整受保护链（SessionAuth → CSRF → Origin），并把
// 本任务的端点挂到 rt.protected（与生产装配同一形态）。
func newAccountTestEnv(t *testing.T) *accountTestEnv {
	t.Helper()
	return newAccountTestEnvWithConfig(t, nil)
}

// newAccountTestEnvWithConfig 允许注入 configcenter 读取面（换绑邮箱的域名
// 白名单与注册路径共用同一配置键）。
func newAccountTestEnvWithConfig(t *testing.T, config mapConfig) *accountTestEnv {
	t.Helper()
	if config == nil {
		// 构造器要求显式 Config（nil 会静默关闭域名白名单）；空 map 表示
		// 全部取设计默认值（白名单未启用）。
		config = mapConfig{}
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newAccountTestRepo()
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	auditEx := &accountAuditExec{}
	anon := &accountTestAnonymizer{}
	sender := &accountTestNotifSender{}
	source := &accountTestReauthSource{
		repo:      repo,
		materials: map[int64]*auth.ReauthMaterial{},
	}

	env := &accountTestEnv{
		t: t, repo: repo, auditEx: auditEx, anon: anon, source: source,
		sender: sender, mr: mr, rdb: rdb, sessions: sessions,
		mfaCode: "123456",
	}

	handler, err := NewAccountHandlers(AccountHandlerConfig{
		Repo:       repo,
		Redis:      rdb,
		Sessions:   sessions,
		Audit:      audit.New(auditEx),
		Anonymizer: anon,
		// 假源已返回解密后的明文 secret；测试用它验证「启用 MFA 必须给第二因子」，
		// 无需真实 TOTP 算法。生产装配注入 auth.VerifyTOTP。
		ReauthUsers: source,
		VerifyTOTP: func(_, code string, _ time.Time) bool {
			return env.mfaCode == "" || code == env.mfaCode
		},
		SessionSecret:      []byte(testSessionSecret),
		NotificationSender: sender,
		Config:             config,
	})
	if err != nil {
		t.Fatalf("NewAccountHandlers: %v", err)
	}
	env.handler = handler

	router, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          repo,
		SessionSecret: []byte(testSessionSecret),
		PublicBaseURL: accountTestBaseURL,
		TokenStore:    accountTestLookup{repo: repo},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	handler.RegisterAccountRoutes(router.protected, nil)
	env.router = router
	return env
}

// addUser 建一个启用状态、带口令的用户并登记其再认证材料。
func (e *accountTestEnv) addUser(username, email string, totpEnabled bool) *UserRecord {
	e.t.Helper()
	u := e.repo.add(UserRecord{
		Username:     username,
		Email:        email,
		PasswordHash: authTestPasswordHash(),
		Role:         rbac.RoleUser,
		Status:       "enabled",
		TOTPEnabled:  totpEnabled,
	})
	e.source.set(u.ID, &auth.ReauthMaterial{
		PasswordHash: authTestPasswordHash(),
		TOTPEnabled:  totpEnabled,
		// 假源返回的 secret 已是明文；测试的 VerifyTOTP 接缝只比对固定码。
		TOTPSecret: "JBSWY3DPEHPK3PXP",
	})
	return u
}

func (e *accountTestEnv) request(method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "AccountTest/1.0")
	if mutate != nil {
		mutate(req)
	}
	return serve(e.router, req)
}

func (e *accountTestEnv) sessionWithCSRF(userID int64) (sid, csrf string) {
	e.t.Helper()
	sid = mustSession(e.t, e.sessions, userID, "user")
	csrf = auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		e.t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

// authedRequest 走完整受保护链：会话 Cookie + CSRF + 同源 Origin。
func (e *accountTestEnv) authedRequest(method, path string, body any, sid, csrf string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.request(method, path, body, func(r *http.Request) {
		r.Host = "app.example.com"
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
		r.Header.Set(auth.CSRFHeaderName, csrf)
		r.Header.Set("Origin", accountTestBaseURL)
	})
}

func (e *accountTestEnv) bearerRequest(method, path string, body any, plain string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.request(method, path, body, func(r *http.Request) {
		r.Host = "relay.example.com"
		r.Header.Set("Authorization", "Bearer "+plain)
	})
}

func (e *accountTestEnv) issueEmailCode(email, purpose string) string {
	e.t.Helper()
	code, err := e.handler.codes.issue(context.Background(), normalizeEmail(email), purpose)
	if err != nil {
		e.t.Fatalf("issue email code: %v", err)
	}
	return code
}

// auditEvent 返回最近一条审计调用的 detail JSON（第 9 个参数，见 audit.insertSQL）。
func (e *accountTestEnv) lastAuditDetail(t *testing.T) map[string]any {
	t.Helper()
	calls := e.auditEx.snapshot()
	if len(calls) == 0 {
		t.Fatal("没有写入任何审计事件")
	}
	call := calls[len(calls)-1]
	if len(call) < 12 {
		t.Fatalf("审计参数数量 = %d，期望 12", len(call))
	}
	detail := call[9]
	var out map[string]any
	switch v := detail.(type) {
	case string:
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			t.Fatalf("detail 不是合法 JSON: %v（%q）", err, v)
		}
	case []byte:
		if err := json.Unmarshal(v, &out); err != nil {
			t.Fatalf("detail 不是合法 JSON: %v", err)
		}
	default:
		t.Fatalf("detail 类型 = %T，期望 string/[]byte", detail)
	}
	return out
}

// accountProblemType 解出 RFC 7807 的 type（本文件独立于其它任务的测试
// helper，避免并行任务改签名时的交叉编译失败）。
func accountProblemType(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var p problemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem 体不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return p.Type
}

func decodeAccountEmailResponse(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return body
}

// ---------------------------------------------------------------------------
// F-01-17 删除账号
// ---------------------------------------------------------------------------

// TestAccountDeleteHardDeleteTerminalState 覆盖 brief (a) / TC-F-01-17-01：
// 成功后 users 行消失、api_tokens 消失、全部会话撤销、PII 匿名化被调用、
// 审计写入 user.delete（category=operation）。
func TestAccountDeleteHardDeleteTerminalState(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	env.repo.addToken(user.ID, auth.HashTokenKey("nkt-account-delete-token"))
	sid, csrf := env.sessionWithCSRF(user.ID)
	otherSID := mustSession(t, env.sessions, user.ID, "user")

	rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "alice",
		"password":         authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	// B4：删号成功响应必须清 Cookie 并要求浏览器清除缓存/存储。
	if got := rr.Header().Get("Clear-Site-Data"); got != clearSiteDataHeader {
		t.Fatalf("Clear-Site-Data = %q，期望 %q", got, clearSiteDataHeader)
	}
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		c := cookieByName(rr, name)
		if c == nil || c.Value != "" || c.MaxAge > 0 {
			t.Fatalf("删号必须清除 Cookie %s（cookie=%+v）", name, c)
		}
	}

	if env.repo.users[user.ID] != nil {
		t.Fatal("users 行必须被硬删除")
	}
	if env.repo.tokenCount(user.ID) != 0 {
		t.Fatal("api_tokens 必须被硬删除")
	}
	for i, oldSID := range []string{sid, otherSID} {
		if _, err := env.sessions.Get(context.Background(), oldSID); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("会话 %d err = %v，期望 ErrSessionNotFound", i+1, err)
		}
	}

	calls := env.anon.snapshot()
	if len(calls) != 1 || calls[0].UserID != user.ID || calls[0].Username != fmt.Sprintf("deleted_%d", user.ID) {
		t.Fatalf("PII 匿名化调用 = %+v，期望 [{user=%d username=deleted_%d}]", calls, user.ID, user.ID)
	}

	details := env.auditEx.snapshot()
	if len(details) != 1 {
		t.Fatalf("审计事件数量 = %d，期望 1", len(details))
	}
	if category := details[0][2]; category != "operation" {
		t.Fatalf("category = %v，期望 operation（user.delete）", category)
	}
	if status, ok := details[0][7].(int16); !ok || status != http.StatusNoContent {
		t.Fatalf("审计 status_code = %v（%T），期望 204", details[0][7], details[0][7])
	}
	detail := env.lastAuditDetail(t)
	if detail["action"] != audit.ActionUserDelete {
		t.Fatalf("detail.action = %v，期望 %s（writer 注入的冻结动作）", detail["action"], audit.ActionUserDelete)
	}
	if detail["label"] != "account_deleted" {
		t.Fatalf("detail.label = %v，期望 account_deleted", detail["label"])
	}
}

// TestAccountDeleteOldTokenUnauthorized 覆盖 brief (b) / TC-F-01-17-03：
// 删除后旧 Bearer 令牌不能再通过 SessionAuth（令牌行已硬删除）。
func TestAccountDeleteOldTokenUnauthorized(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	const plain = "nkt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	env.repo.addToken(user.ID, auth.HashTokenKey(plain))

	if rr := env.bearerRequest(http.MethodGet, "/api/notification-preferences", nil, plain); rr.Code != http.StatusOK {
		t.Fatalf("删除前 Bearer status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	sid, csrf := env.sessionWithCSRF(user.ID)
	rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "alice",
		"password":         authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("删除 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	if rr := env.bearerRequest(http.MethodGet, "/api/notification-preferences", nil, plain); rr.Code != http.StatusUnauthorized {
		t.Fatalf("删除后旧令牌 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountDeleteUsernameReRegistrable 覆盖 brief (c) / TC-F-01-17-04：
// 硬删除释放用户名（UNIQUE 不再被占用），同名可重新注册。
func TestAccountDeleteUsernameReRegistrable(t *testing.T) {
	env := newAccountTestEnv(t)
	register := func(email string) *httptest.ResponseRecorder {
		return env.request(http.MethodPost, "/api/auth/register", map[string]any{
			"username":           "alice",
			"email":              email,
			"password":           authTestPassword,
			"confirmPassword":    authTestPassword,
			"agree_terms":        true,
			"email_verification": false,
		}, nil)
	}

	if rr := register("alice@example.com"); rr.Code != http.StatusCreated {
		t.Fatalf("首次注册 status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	first := env.repo.byUsername["alice"]
	if first == 0 {
		t.Fatal("首次注册未写入用户名")
	}
	// 注册端点不经过本测试的 addUser 辅助，需登记再认证材料（生产由 DB 读取）。
	env.source.set(first, &auth.ReauthMaterial{PasswordHash: authTestPasswordHash()})

	sid, csrf := env.sessionWithCSRF(first)
	if rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "alice",
		"password":         authTestPassword,
	}, sid, csrf); rr.Code != http.StatusNoContent {
		t.Fatalf("删除 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	if rr := register("alice-new@example.com"); rr.Code != http.StatusCreated {
		t.Fatalf("同名重新注册 status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	second := env.repo.byUsername["alice"]
	if second == 0 || second == first {
		t.Fatalf("重新注册 id = %d（旧 %d），期望新行", second, first)
	}
}

// TestAccountDeleteUsernameMismatch：confirm_username 必须逐字匹配；不匹配
// 400 且不产生任何删除/匿名化/审计副作用（TC-F-01-17-02）。
func TestAccountDeleteUsernameMismatch(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)

	rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "alice2",
		"password":         authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if env.repo.users[user.ID] == nil {
		t.Fatal("用户名不匹配不得删除用户")
	}
	if len(env.anon.snapshot()) != 0 || len(env.auditEx.snapshot()) != 0 {
		t.Fatal("用户名不匹配不得触发匿名化或审计写入")
	}
}

// TestAccountDeleteRequiresMFAFactor 覆盖 brief (d) / TC-F-01-17-04：
// 已启用 MFA 时缺 TOTP 或 TOTP 错误 → 403 reauth_required 且不删除。
func TestAccountDeleteRequiresMFAFactor(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", true)
	sid, csrf := env.sessionWithCSRF(user.ID)

	for name, body := range map[string]map[string]any{
		"缺第二因子": {"confirm_username": "alice", "password": authTestPassword},
		"错误的码":  {"confirm_username": "alice", "password": authTestPassword, "totp_code": "000000"},
	} {
		t.Run(name, func(t *testing.T) {
			rr := env.authedRequest(http.MethodDelete, "/api/account", body, sid, csrf)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
			}
			if got := accountProblemType(t, rr); got != problemTypeReauthRequired {
				t.Fatalf("problem type = %q，期望 %q", got, problemTypeReauthRequired)
			}
			if env.repo.users[user.ID] == nil {
				t.Fatal("再认证失败不得删除用户")
			}
		})
	}

	rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "alice",
		"password":         authTestPassword,
		"totp_code":        env.mfaCode,
	}, sid, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("带正确第二因子 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountDeleteAdminRejected：role=admin 不可自助删除（F-01-17 权限假设）。
func TestAccountDeleteAdminRejected(t *testing.T) {
	env := newAccountTestEnv(t)
	u := env.repo.add(UserRecord{
		Username: "admin1", Email: "admin1@example.com",
		PasswordHash: authTestPasswordHash(), Role: rbac.RoleAdmin, Status: "enabled",
	})
	env.source.set(u.ID, &auth.ReauthMaterial{PasswordHash: authTestPasswordHash()})
	sid, csrf := env.sessionWithCSRF(u.ID)

	rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "admin1",
		"password":         authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if env.repo.users[u.ID] == nil {
		t.Fatal("管理员行不得被删除")
	}
}

// TestAccountDeleteIdempotentNotFound：仍持有有效 Principal 但用户行已不存在
// （并发删除/半状态）时返回 404；重复删除不会触碰匿名化或审计。
func TestAccountDeleteIdempotentNotFound(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	// 模拟并发删除：会话仍在，但行已消失。
	env.repo.fakeAuthRepo.mu.Lock()
	delete(env.repo.users, user.ID)
	delete(env.repo.byUsername, user.Username)
	delete(env.repo.byEmail, "alice@example.com")
	env.repo.fakeAuthRepo.mu.Unlock()

	rr := env.authedRequest(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": "alice",
		"password":         authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.anon.snapshot()) != 0 {
		t.Fatal("404 时不得触发匿名化")
	}
}

// ---------------------------------------------------------------------------
// F-01-15 换绑邮箱
// ---------------------------------------------------------------------------

// TestAccountEmailChangeRequiresReauth 覆盖 brief (e) / TC-F-01-15-03：
// 未过全量再认证 → 403 且邮箱不变、验证码不被消费；通过后验证码才生效。
func TestAccountEmailChangeRequiresReauth(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	otherSID := mustSession(t, env.sessions, user.ID, "user")
	code := env.issueEmailCode("new@example.com", "bind")

	// 当前口令错误：403 reauth_required，不落库。
	rr := env.authedRequest(http.MethodPut, "/api/email", map[string]any{
		"new_email":        "new@example.com",
		"code":             code,
		"current_password": "wrong-password",
	}, sid, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("错误口令 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if got := accountProblemType(t, rr); got != problemTypeReauthRequired {
		t.Fatalf("problem type = %q，期望 %q", got, problemTypeReauthRequired)
	}
	if got := env.repo.users[user.ID].Email; got != "alice@example.com" {
		t.Fatalf("再认证失败后邮箱 = %q，期望保持原值", got)
	}

	// 同一验证码仍可用（证明失败路径没有消费它）。
	rr = env.authedRequest(http.MethodPut, "/api/email", map[string]any{
		"new_email":        "new@example.com",
		"code":             code,
		"current_password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("再认证通过 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.users[user.ID].Email; got != "new@example.com" {
		t.Fatalf("邮箱 = %q，期望 new@example.com", got)
	}
	if !env.repo.users[user.ID].EmailVerified {
		t.Fatal("换绑成功后 email_verified 必须为 true")
	}
	// B5：换绑必须吊销其它会话，保留当前会话（会话生命周期状态机）。
	if _, err := env.sessions.Get(context.Background(), otherSID); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("换绑后其它会话 err = %v，期望 ErrSessionNotFound", err)
	}
	if _, err := env.sessions.Get(context.Background(), sid); err != nil {
		t.Fatalf("换绑不得吊销当前会话: %v", err)
	}
	body := decodeAccountEmailResponse(t, rr)
	if body["email"] != "new@example.com" || body["email_verified"] != true {
		t.Fatalf("响应 = %v，期望 email/email_verified", body)
	}

	detail := env.lastAuditDetail(t)
	if detail["action"] != audit.ActionUserUpdate {
		t.Fatalf("审计 detail.action = %v，期望 %s", detail["action"], audit.ActionUserUpdate)
	}
	if detail["label"] != "email_changed" {
		t.Fatalf("审计 detail.label = %v，期望 email_changed", detail["label"])
	}
	if detail["field"] != "email" || detail["result"] != "success" {
		t.Fatalf("email 事件缺少 field/result: %v", detail)
	}
	calls := env.auditEx.snapshot()
	if status, ok := calls[len(calls)-1][7].(int16); !ok || status != http.StatusOK {
		t.Fatalf("审计 status_code = %v（%T），期望 200", calls[len(calls)-1][7], calls[len(calls)-1][7])
	}
	for _, call := range env.auditEx.snapshot() {
		serialized := fmt.Sprint(call...)
		if strings.Contains(serialized, authTestPassword) {
			t.Fatalf("审计参数包含口令明文: %v", call)
		}
	}
}

// TestAccountEmailChangeRequiresMFAFactor：MFA 已启用时缺 TOTP → 403 不落库。
func TestAccountEmailChangeRequiresMFAFactor(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", true)
	sid, csrf := env.sessionWithCSRF(user.ID)
	code := env.issueEmailCode("new@example.com", "bind")

	rr := env.authedRequest(http.MethodPut, "/api/email", map[string]any{
		"new_email":        "new@example.com",
		"code":             code,
		"current_password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 TOTP status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.users[user.ID].Email; got != "alice@example.com" {
		t.Fatalf("邮箱 = %q，期望保持原值", got)
	}

	rr = env.authedRequest(http.MethodPut, "/api/email", map[string]any{
		"new_email":        "new@example.com",
		"code":             code,
		"current_password": authTestPassword,
		"totp_code":        env.mfaCode,
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("带 TOTP status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountEmailChangeValidation：新邮箱格式、验证码错误、邮箱已占用（409）。
func TestAccountEmailChangeValidation(t *testing.T) {
	t.Run("新邮箱格式", func(t *testing.T) {
		env := newAccountTestEnv(t)
		user := env.addUser("alice", "alice@example.com", false)
		sid, csrf := env.sessionWithCSRF(user.ID)
		rr := env.authedRequest(http.MethodPut, "/api/email", map[string]any{
			"new_email": "not-an-email", "code": "123456", "current_password": authTestPassword,
		}, sid, csrf)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
		}
	})

	t.Run("验证码错误", func(t *testing.T) {
		env := newAccountTestEnv(t)
		user := env.addUser("alice", "alice@example.com", false)
		sid, csrf := env.sessionWithCSRF(user.ID)
		env.issueEmailCode("new@example.com", "bind")
		rr := env.authedRequest(http.MethodPut, "/api/email", map[string]any{
			"new_email": "new@example.com", "code": "000000", "current_password": authTestPassword,
		}, sid, csrf)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
		}
		if got := env.repo.users[user.ID].Email; got != "alice@example.com" {
			t.Fatalf("邮箱 = %q，期望保持原值", got)
		}
	})

	t.Run("邮箱已占用", func(t *testing.T) {
		env := newAccountTestEnv(t)
		user := env.addUser("alice", "alice@example.com", false)
		env.addUser("bob", "bob@example.com", false)
		sid, csrf := env.sessionWithCSRF(user.ID)
		code := env.issueEmailCode("bob@example.com", "bind")
		rr := env.authedRequest(http.MethodPut, "/api/email", map[string]any{
			"new_email": "bob@example.com", "code": code, "current_password": authTestPassword,
		}, sid, csrf)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
		}
		if got := env.repo.users[user.ID].Email; got != "alice@example.com" {
			t.Fatalf("冲突后邮箱 = %q，期望保持原值", got)
		}
	})
}

// TestAccountEmailChangeDomainAllowlist：换绑邮箱复用注册路径的域名白名单
// （system-config-mapping.md:169）。未启用时放行；启用后非白名单域名 422
// email_domain_not_allowed，且不落库、不消费验证码。
func TestAccountEmailChangeDomainAllowlist(t *testing.T) {
	config := mapConfig{
		"auth.email_domain_restrict_enabled": configBool(true),
		"auth.email_domain_allowlist":        json.RawMessage(`["example.com","*.corp.example"]`),
	}
	env := newAccountTestEnvWithConfig(t, config)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)

	blockedCode := env.issueEmailCode("new@blocked.test", "bind")
	rr := env.authedRequest(http.MethodPut, "/api/email", map[string]any{
		"new_email": "new@blocked.test", "code": blockedCode, "current_password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非白名单域名 status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if got := accountProblemType(t, rr); got != authProblemEmailDomainDenied {
		t.Fatalf("problem type = %q，期望 %q", got, authProblemEmailDomainDenied)
	}
	if got := env.repo.users[user.ID].Email; got != "alice@example.com" {
		t.Fatalf("域名被拒后邮箱 = %q，期望保持原值", got)
	}

	// 通配子域命中白名单 → 正常换绑；同一验证码未被上一步消费。
	allowedCode := env.issueEmailCode("alice@team.corp.example", "bind")
	rr = env.authedRequest(http.MethodPut, "/api/email", map[string]any{
		"new_email": "alice@team.corp.example", "code": allowedCode, "current_password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("白名单域名 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.users[user.ID].Email; got != "alice@team.corp.example" {
		t.Fatalf("邮箱 = %q，期望 alice@team.corp.example", got)
	}
}

// ---------------------------------------------------------------------------
// F-01-22 通知偏好
// ---------------------------------------------------------------------------

func getPrefs(t *testing.T, env *accountTestEnv, sid, csrf string) map[string]any {
	t.Helper()
	rr := env.authedRequest(http.MethodGet, "/api/notification-preferences", nil, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET prefs status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET prefs 响应不是合法 JSON: %v", err)
	}
	return out
}

// TestAccountNotificationPreferencesSparseMerge：PUT 是稀疏合并，未提交的
// 类别/子键保持原值；GET 回读同一视图。
func TestAccountNotificationPreferencesSparseMerge(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)

	if prefs := getPrefs(t, env, sid, csrf); len(prefs) != 0 {
		t.Fatalf("默认通知偏好 = %v，期望空对象", prefs)
	}

	// 库内 JSON null（历史数据/手工写入）必须归一为 {}，而不是把 null 回传给前端。
	env.repo.mu.Lock()
	env.repo.prefs[user.ID] = json.RawMessage("null")
	env.repo.mu.Unlock()
	if prefs := getPrefs(t, env, sid, csrf); len(prefs) != 0 {
		t.Fatalf("JSON null 应归一为 {}，实际 %v", prefs)
	}

	rr := env.authedRequest(http.MethodPut, "/api/notification-preferences", map[string]any{
		"balance_warning": map[string]any{
			"enabled": true, "threshold": 100, "channels": []string{"email"},
		},
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("首次 PUT status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.authedRequest(http.MethodPut, "/api/notification-preferences", map[string]any{
		"usage_digest": map[string]any{"enabled": true, "channels": []string{"email", "webhook"}},
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("第二次 PUT status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	prefs := getPrefs(t, env, sid, csrf)
	balance, ok := prefs["balance_warning"].(map[string]any)
	if !ok {
		t.Fatalf("balance_warning 缺失或类型错误: %v", prefs)
	}
	if balance["enabled"] != true || balance["threshold"] != float64(100) {
		t.Fatalf("balance_warning = %v，期望被保留", balance)
	}
	digest, ok := prefs["usage_digest"].(map[string]any)
	if !ok || digest["enabled"] != true {
		t.Fatalf("usage_digest = %v，期望已合并", prefs["usage_digest"])
	}

	// 稀疏合并：只改 threshold 时 channels 保留。
	rr = env.authedRequest(http.MethodPut, "/api/notification-preferences", map[string]any{
		"balance_warning": map[string]any{"threshold": 250},
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("第三次 PUT status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	balance = getPrefs(t, env, sid, csrf)["balance_warning"].(map[string]any)
	if balance["threshold"] != float64(250) {
		t.Fatalf("threshold = %v，期望 250", balance["threshold"])
	}
	if channels, ok := balance["channels"].([]any); !ok || len(channels) != 1 || channels[0] != "email" {
		t.Fatalf("channels = %v，稀疏合并不得丢字段", balance["channels"])
	}

	// 测试消息：email 渠道未指定目标时回落到账号邮箱。
	if rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "email"}, sid, csrf); rr.Code != http.StatusOK {
		t.Fatalf("test status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	calls := env.sender.snapshot()
	if len(calls) != 1 || calls[0].Channel != "email" || calls[0].Target != "alice@example.com" {
		t.Fatalf("发送器调用 = %+v，期望 email → alice@example.com", calls)
	}

	// 渠道 URL 可以显式清除：null 与空串都要生效（否则含密钥的旧 URL 永远删不掉）。
	rr = env.authedRequest(http.MethodPut, "/api/notification-preferences", map[string]any{
		"webhook_url": "https://hooks.example.com/alice",
		"bark_url":    "https://bark.example.com/alice",
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("写入 URL status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if prefs := getPrefs(t, env, sid, csrf); prefs["webhook_url"] == nil || prefs["bark_url"] == nil {
		t.Fatalf("URL 未写入: %v", prefs)
	}
	rr = env.authedRequest(http.MethodPut, "/api/notification-preferences", map[string]any{
		"webhook_url": nil,
		"bark_url":    "",
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("清除 URL status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	prefs = getPrefs(t, env, sid, csrf)
	if _, present := prefs["webhook_url"]; present {
		t.Fatalf("webhook_url 应被 null 清除: %v", prefs)
	}
	if _, present := prefs["bark_url"]; present {
		t.Fatalf("bark_url 应被空串清除: %v", prefs)
	}

	detail := env.lastAuditDetail(t)
	if detail["action"] != audit.ActionUserUpdate {
		t.Fatalf("审计 detail.action = %v，期望 %s", detail["action"], audit.ActionUserUpdate)
	}
	if detail["label"] != "notification_pref_updated" {
		t.Fatalf("审计 detail.label = %v，期望 notification_pref_updated", detail["label"])
	}
}

// TestAccountNotificationPreferencesValidation：未知键/非法值 400 且不落库。
func TestAccountNotificationPreferencesValidation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"未知键", map[string]any{"unknown_key": true}},
		{"未知子键", map[string]any{"balance_warning": map[string]any{"surprise": true}}},
		{"非法渠道", map[string]any{"balance_warning": map[string]any{"enabled": true, "channels": []string{"sms"}}}},
		{"启用但无渠道", map[string]any{"usage_digest": map[string]any{"enabled": true, "channels": []string{}}}},
		{"负阈值", map[string]any{"balance_warning": map[string]any{"threshold": -1}}},
		{"安全通知不可关闭", map[string]any{"security_alert": map[string]any{"enabled": false}}},
		{"非法 URL", map[string]any{"webhook_url": "ftp://example.com/hook"}},
		{"非整数阈值", map[string]any{"balance_warning": map[string]any{"threshold": 1.5}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAccountTestEnv(t)
			user := env.addUser("alice", "alice@example.com", false)
			sid, csrf := env.sessionWithCSRF(user.ID)
			rr := env.authedRequest(http.MethodPut, "/api/notification-preferences", tc.body, sid, csrf)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
			if prefs := getPrefs(t, env, sid, csrf); len(prefs) != 0 {
				t.Fatalf("非法输入不得落库，实际 %v", prefs)
			}
		})
	}
}

// TestAccountNotificationTestRateLimit：测试消息 3 次/分钟/用户，第 4 次 429。
func TestAccountNotificationTestRateLimit(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	body := map[string]any{"channel": "email"}

	for i := 0; i < 3; i++ {
		if rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test", body, sid, csrf); rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test", body, sid, csrf)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 4 次 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}

	// 渠道校验发生在限流计数之前：非法渠道仍是 400。
	rr = env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "sms"}, sid, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法渠道 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountNotificationTestDeliveryFailure：发送器返回渠道错误 → 502（供
// 前端行内展示具体失败）。独立环境避免被上一用例的 3 次/分钟限流挡住。
func TestAccountNotificationTestDeliveryFailure(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	env.sender.mu.Lock()
	env.sender.err = errors.New("webhook returned 404")
	env.sender.mu.Unlock()
	if rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "webhook", "target": "https://example.com/hook"}, sid, csrf); rr.Code != http.StatusBadGateway {
		t.Fatalf("发送失败 status = %d，期望 502（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountNotificationTestTargetValidation：显式 email 目标也必须过邮箱
// 格式校验（阻止 CR/LF 头注入与多收件人写法进入 SMTP 实现）；URL 型渠道的
// 目标必须是合法 http(s) URL。
func TestAccountNotificationTestTargetValidation(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)

	for name, body := range map[string]map[string]any{
		"邮箱含换行":     {"channel": "email", "target": "victim@example.com\r\nBcc: attacker@example.com"},
		"非邮箱目标":     {"channel": "email", "target": "not-an-email"},
		"多收件人":      {"channel": "email", "target": "a@example.com,b@example.com"},
		"空 webhook": {"channel": "webhook"},
	} {
		t.Run(name, func(t *testing.T) {
			rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test", body, sid, csrf)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
		})
	}

	// 非法目标不消耗 3 次/分钟配额：先打 4 次 400，随后仍应有完整配额。
	for i := 0; i < 4; i++ {
		rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
			map[string]any{"channel": "email", "target": "not-an-email"}, sid, csrf)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("非法目标第 %d 次 status = %d，期望 400", i+1, rr.Code)
		}
	}

	// 合法显式邮箱目标：规范化后交给发送器。
	rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "email", "target": "  Custom@Example.COM "}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("合法目标 status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	calls := env.sender.snapshot()
	if len(calls) != 1 || calls[0].Target != "custom@example.com" {
		t.Fatalf("发送器目标 = %+v，期望规范化后的 custom@example.com", calls)
	}

	// 配额仍是 3 次/分钟：已成功 1 次，再成功 2 次后第 4 次才 429。
	for i := 0; i < 2; i++ {
		if rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
			map[string]any{"channel": "email"}, sid, csrf); rr.Code != http.StatusOK {
			t.Fatalf("合法请求第 %d 次 status = %d，期望 200（body=%q）", i+2, rr.Code, rr.Body.String())
		}
	}
	if rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
		map[string]any{"channel": "email"}, sid, csrf); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("配额应只计合法请求：第 4 次 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountNotificationTestSenderNotConfigured：渠道未配置时 503，且不消耗
// 测试消息配额（先查 sender 再 INCR）。
func TestAccountNotificationTestSenderNotConfigured(t *testing.T) {
	env := newAccountTestEnv(t)
	user := env.addUser("alice", "alice@example.com", false)
	sid, csrf := env.sessionWithCSRF(user.ID)
	env.handler.sender = nil

	for i := 0; i < 5; i++ {
		rr := env.authedRequest(http.MethodPost, "/api/notification-preferences/test",
			map[string]any{"channel": "email"}, sid, csrf)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("第 %d 次 status = %d，期望 503（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}
}

// TestAccountEndpointsRequireAuth：五个端点都必须先过 SessionAuth。
func TestAccountEndpointsRequireAuth(t *testing.T) {
	env := newAccountTestEnv(t)
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodDelete, "/api/account", map[string]any{}},
		{http.MethodPut, "/api/email", map[string]any{}},
		{http.MethodGet, "/api/notification-preferences", nil},
		{http.MethodPut, "/api/notification-preferences", map[string]any{}},
		{http.MethodPost, "/api/notification-preferences/test", map[string]any{}},
	}
	for _, tc := range cases {
		rr := env.request(tc.method, tc.path, tc.body, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d，期望 401（body=%q）", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// TestNewAccountHandlersRequiresDependencies：装配期必须 fail-fast，不能让
// 缺少 Redis/Sessions/仓储/再认证源/PII 匿名化的配置带病运行。
func TestNewAccountHandlersRequiresDependencies(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	sessRDB := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close(); _ = sessRDB.Close() })
	base := AccountHandlerConfig{
		Repo:          newAccountTestRepo(),
		Redis:         rdb,
		Sessions:      auth.NewSessionManager(sessRDB, []byte(testSessionSecret)),
		Anonymizer:    &accountTestAnonymizer{},
		SessionSecret: []byte(testSessionSecret),
		Config:        mapConfig{},
		ReauthUsers: &accountTestReauthSource{
			repo: newAccountTestRepo(), materials: map[int64]*auth.ReauthMaterial{},
		},
	}

	cases := []struct {
		name string
		mut  func(*AccountHandlerConfig)
	}{
		{"缺 Redis", func(c *AccountHandlerConfig) { c.Redis = nil }},
		{"缺 Sessions", func(c *AccountHandlerConfig) { c.Sessions = nil }},
		{"缺仓储", func(c *AccountHandlerConfig) { c.Repo = nil }},
		{"缺再认证源", func(c *AccountHandlerConfig) { c.ReauthUsers = nil }},
		{"缺 PII 匿名化", func(c *AccountHandlerConfig) { c.Anonymizer = nil }},
		{"缺 SessionSecret", func(c *AccountHandlerConfig) { c.SessionSecret = nil }},
		{"缺 Config", func(c *AccountHandlerConfig) { c.Config = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mut(&cfg)
			if _, err := NewAccountHandlers(cfg); err == nil {
				t.Fatalf("%s 时 NewAccountHandlers 必须返回错误", tc.name)
			}
		})
	}
}
