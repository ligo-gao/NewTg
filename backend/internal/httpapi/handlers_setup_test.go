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
	"newtg/gateway/internal/configcenter"
)

// 本文件是 Task 11（modules-01 F-01-20 首次安装向导）的单元验收。
// 存储用内存假实现，Redis 用 miniredis；未打 integration 标签时不得触碰
// 外部 PG/Redis（GC-8）。真实事务/并发语义见 handlers_setup_integration_test.go。

const setupTestPassword = "CorrectHorse9!"

// ---------------------------------------------------------------------------
// fake SetupStore
// ---------------------------------------------------------------------------

type setupStoredUser struct {
	Input  SetupInput
	Result SetupResult
}

type fakeSetupStore struct {
	mu sync.Mutex

	initialized   bool
	users         []setupStoredUser
	nextID        int64
	settings      map[string]string
	completeCalls int
	isInitCalls   int

	// 故障注入。
	isInitErr   error
	completeErr error

	// gate 让并发用例确定性地「两个请求都先通过未初始化预检查」。
	gate *setupInitGate
}

func newFakeSetupStore() *fakeSetupStore {
	return &fakeSetupStore{settings: map[string]string{}}
}

func (f *fakeSetupStore) IsInitialized(_ context.Context) (bool, error) {
	if f.gate != nil {
		f.gate.wait()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isInitCalls++
	if f.isInitErr != nil {
		return false, f.isInitErr
	}
	return f.initialized, nil
}

// CompleteSetup 用互斥锁模拟事务 + 锚唯一约束：并发调用里只有一个能落库，
// 其余返回 ErrSetupAlreadyCompleted。
func (f *fakeSetupStore) CompleteSetup(_ context.Context, in SetupInput) (SetupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls++
	if f.completeErr != nil {
		return SetupResult{}, f.completeErr
	}
	if f.initialized {
		return SetupResult{}, ErrSetupAlreadyCompleted
	}
	for _, u := range f.users {
		if u.Input.Username == in.Username || strings.EqualFold(u.Input.Email, in.Email) {
			return SetupResult{}, ErrDuplicateUser
		}
	}
	f.nextID++
	result := SetupResult{UserID: f.nextID, Role: "admin", Status: "enabled"}
	f.users = append(f.users, setupStoredUser{Input: in, Result: result})
	f.settings[SetupCompletedKey] = time.Now().UTC().Format(time.RFC3339Nano)
	f.settings[setupSiteTitleKey] = in.SiteTitle
	f.settings[setupServerAddressKey] = in.ServerAddress
	f.initialized = true
	return result, nil
}

func (f *fakeSetupStore) userCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.users)
}

func (f *fakeSetupStore) lastUser() (setupStoredUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.users) == 0 {
		return setupStoredUser{}, false
	}
	return f.users[len(f.users)-1], true
}

func (f *fakeSetupStore) callCounts() (complete, isInit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completeCalls, f.isInitCalls
}

func (f *fakeSetupStore) setting(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.settings[key]
	return v, ok
}

// setupInitGate 是一道「N 个请求都到达后才放行」的栅栏。没有它，并发用例
// 可能因调度让第一个请求先完成，第二个请求在预检查阶段就直接 409，无法证明
// 事务/锚在「双双重检查通过」后仍只允许一次成功。
type setupInitGate struct {
	mu      sync.Mutex
	arrived int
	target  int
	release chan struct{}
}

func newSetupInitGate(target int) *setupInitGate {
	return &setupInitGate{target: target, release: make(chan struct{})}
}

func (g *setupInitGate) wait() {
	g.mu.Lock()
	g.arrived++
	if g.arrived >= g.target {
		close(g.release)
	}
	release := g.release
	g.mu.Unlock()
	<-release
}

// ---------------------------------------------------------------------------
// test env / helpers
// ---------------------------------------------------------------------------

type setupTestOptions struct {
	store        *fakeSetupStore
	checker      auth.CompromisedPasswordChecker
	withSessions bool
	audit        SetupAuditor
}

type setupTestEnv struct {
	t        *testing.T
	store    *fakeSetupStore
	handlers *SetupHandlers
	mux      *http.ServeMux
	sessions *auth.SessionManager
	rdb      *redis.Client
}

func newSetupTestEnv(t *testing.T, opts setupTestOptions) *setupTestEnv {
	t.Helper()
	store := opts.store
	if store == nil {
		store = newFakeSetupStore()
	}

	var (
		rdb      *redis.Client
		sessions *auth.SessionManager
	)
	if opts.withSessions {
		mr := miniredis.RunT(t)
		rdb = redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
		t.Cleanup(func() { _ = rdb.Close() })
		sessions = auth.NewSessionManager(rdb, []byte(testSessionSecret))
	}

	handlers, err := NewSetupHandlers(SetupConfig{
		Store:          store,
		Sessions:       sessions,
		Checker:        opts.checker,
		Redis:          rdb,
		Audit:          opts.audit,
		AllowNoSession: !opts.withSessions,
	})
	if err != nil {
		t.Fatalf("NewSetupHandlers: %v", err)
	}

	mux := http.NewServeMux()
	handlers.RegisterSetupRoutes(mux)
	return &setupTestEnv{t: t, store: store, handlers: handlers, mux: mux, sessions: sessions, rdb: rdb}
}

func (e *setupTestEnv) post(body any) *httptest.ResponseRecorder {
	e.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		e.t.Fatalf("marshal setup body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SetupTest/1.0")
	req.RemoteAddr = "192.0.2.55:12345"
	return serve(e.mux, req)
}

func setupPayload() map[string]any {
	return map[string]any{
		"site_title":      "Acme Gateway",
		"server_address":  "https://gateway.example.com/",
		"username":        "admin",
		"email":           "admin@example.com",
		"password":        setupTestPassword,
		"confirmPassword": setupTestPassword,
	}
}

func setupPayloadFor(username string) map[string]any {
	body := setupPayload()
	body["username"] = username
	body["email"] = username + "@example.com"
	return body
}

func decodeSetupUser(t *testing.T, rr *httptest.ResponseRecorder) setupUserView {
	t.Helper()
	var resp setupResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码 setup 响应失败: %v（body=%q）", err, rr.Body.String())
	}
	return resp.User
}

// setupResponseCookie 只从本响应的 Set-Cookie 里取，避免依赖其他测试文件的辅助函数。
func setupResponseCookie(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// (a) 首次初始化 + (d) 口令哈希/不回传
// ---------------------------------------------------------------------------

func TestSetupFirstRunCreatesAdminAndAnchor(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{withSessions: true})
	rr := env.post(setupPayload())
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q，期望 application/json", ct)
	}

	user := decodeSetupUser(t, rr)
	if user.ID <= 0 || user.Username != "admin" || user.Email != "admin@example.com" {
		t.Fatalf("user = %+v，与请求不符", user)
	}
	if user.Role != "admin" || user.Status != "enabled" || !user.EmailVerified {
		t.Fatalf("首个用户必须是 role=admin/status=enabled/email_verified=true，实际 %+v", user)
	}

	// (d) 明文口令与哈希都不得出现在响应里。
	body := rr.Body.String()
	if strings.Contains(body, setupTestPassword) {
		t.Fatal("响应泄露了明文口令")
	}
	if strings.Contains(body, "$argon2id$") || strings.Contains(body, "password_hash") {
		t.Fatalf("响应泄露了口令哈希材料: %q", body)
	}

	stored, ok := env.store.lastUser()
	if !ok {
		t.Fatal("store 没有收到 CompleteSetup")
	}
	if stored.Input.PasswordHash == setupTestPassword {
		t.Fatal("落库的是明文口令")
	}
	if !strings.HasPrefix(stored.Input.PasswordHash, "$argon2id$") {
		t.Fatalf("落库口令不是 argon2id PHC: %q", stored.Input.PasswordHash)
	}
	if !auth.VerifyPassword(stored.Input.PasswordHash, setupTestPassword) {
		t.Fatal("HashPassword 产物无法用原文校验")
	}
	if stored.Input.SiteTitle != "Acme Gateway" {
		t.Errorf("site title = %q，期望 Acme Gateway", stored.Input.SiteTitle)
	}
	if stored.Input.ServerAddress != "https://gateway.example.com" {
		t.Errorf("server address = %q，期望去掉末尾斜杠", stored.Input.ServerAddress)
	}

	// 幂等锚与站点配置必须与 admin 同一次 CompleteSetup 写入。
	if _, ok := env.store.setting(SetupCompletedKey); !ok {
		t.Fatalf("缺少幂等锚 %q", SetupCompletedKey)
	}
	if v, ok := env.store.setting(setupSiteTitleKey); !ok || v != "Acme Gateway" {
		t.Errorf("site.title = %q, ok=%v", v, ok)
	}
	if v, ok := env.store.setting(setupServerAddressKey); !ok || v != "https://gateway.example.com" {
		t.Errorf("site.server_address = %q, ok=%v", v, ok)
	}

	// F-01-20：初始化完成自动登录（会话 + CSRF Cookie）。
	sid := setupResponseCookie(rr, auth.SessionCookieName)
	if sid == nil || sid.Value == "" {
		t.Fatal("缺少会话 Cookie（初始化后应自动登录）")
	}
	if !sid.HttpOnly || !sid.Secure || sid.Path != "/" {
		t.Errorf("会话 Cookie 属性不安全: %+v", sid)
	}
	csrf := setupResponseCookie(rr, auth.CSRFCookieName)
	if csrf == nil || csrf.Value == "" {
		t.Fatal("缺少 CSRF Cookie")
	}
	if csrf.HttpOnly {
		t.Error("CSRF Cookie 必须可被前端读取（HttpOnly=false）")
	}
	sess, err := env.sessions.Get(context.Background(), sid.Value)
	if err != nil {
		t.Fatalf("读取自动登录会话失败: %v", err)
	}
	if sess.UserID != user.ID || sess.AuthLevel != "admin" {
		t.Errorf("会话身份 = user %d/%s，期望 %d/admin", sess.UserID, sess.AuthLevel, user.ID)
	}
	if !env.sessions.VerifyCSRF(context.Background(), sid.Value, csrf.Value) {
		t.Error("CSRF 令牌未写入会话")
	}
}

func TestSetupAcceptsSnakeCaseConfirmPassword(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{})
	body := setupPayload()
	delete(body, "confirmPassword")
	body["confirm_password"] = setupTestPassword
	rr := env.post(body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// (b) 重复提交 / (c) 已初始化
// ---------------------------------------------------------------------------

func TestSetupRepeatConflict(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{})

	first := env.post(setupPayload())
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d，期望 201（body=%q）", first.Code, first.Body.String())
	}

	second := env.post(setupPayload())
	if second.Code != http.StatusConflict {
		t.Fatalf("重复 status = %d，期望 409（body=%q）", second.Code, second.Body.String())
	}
	var problem struct {
		Type        string `json:"type"`
		Status      int    `json:"status"`
		Initialized bool   `json:"initialized"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &problem); err != nil {
		t.Fatalf("409 body 不是合法 JSON: %v（body=%q）", err, second.Body.String())
	}
	if !problem.Initialized {
		t.Error("409 应携带 initialized=true 标记")
	}
	if problem.Type != setupProblemAlreadyCompleted || problem.Status != http.StatusConflict {
		t.Errorf("409 problem = %+v", problem)
	}
	if env.store.userCount() != 1 {
		t.Fatalf("重复提交后用户数 = %d，期望 1", env.store.userCount())
	}
}

func TestSetupAlreadyInitializedConflict(t *testing.T) {
	store := newFakeSetupStore()
	store.initialized = true
	env := newSetupTestEnv(t, setupTestOptions{store: store})

	rr := env.post(setupPayload())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	complete, _ := store.callCounts()
	if complete != 0 {
		t.Fatalf("已初始化后不应进入写事务，CompleteSetup 被调用 %d 次", complete)
	}
	if store.userCount() != 0 {
		t.Fatal("已初始化后不应创建用户")
	}
}

// ---------------------------------------------------------------------------
// 并发双提交：恰好一个 201、一个 409
// ---------------------------------------------------------------------------

func TestSetupConcurrentDoubleSubmitOnlyOneWins(t *testing.T) {
	store := newFakeSetupStore()
	// 两个请求都先通过「未初始化」预检查，再进入写路径——这正是需要事务/锚
	// 兜底的竞态窗口。
	store.gate = newSetupInitGate(2)
	env := newSetupTestEnv(t, setupTestOptions{store: store})

	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- env.post(setupPayloadFor(fmt.Sprintf("admin%d", i)))
		}(i)
	}
	wg.Wait()
	close(results)

	statuses := map[int]int{}
	for rr := range results {
		statuses[rr.Code]++
	}
	if statuses[http.StatusCreated] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("并发双提交状态分布 = %v，期望恰好一个 201、一个 409", statuses)
	}
	if got := store.userCount(); got != 1 {
		t.Fatalf("并发双提交后用户数 = %d，期望 1", got)
	}
	if complete, _ := store.callCounts(); complete != 2 {
		t.Fatalf("CompleteSetup 调用次数 = %d，期望 2（两个请求都应到达写路径）", complete)
	}
}

// ---------------------------------------------------------------------------
// 校验与故障
// ---------------------------------------------------------------------------

func TestSetupValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"用户名过短", func(b map[string]any) { b["username"] = "ab" }},
		{"用户名非法字符", func(b map[string]any) { b["username"] = "ad min" }},
		{"邮箱非法", func(b map[string]any) { b["email"] = "not-an-email" }},
		{"口令过短", func(b map[string]any) { b["password"] = "short"; b["confirmPassword"] = "short" }},
		{"两次口令不一致", func(b map[string]any) { b["confirmPassword"] = "DifferentHorse9!" }},
		{"站点名为空", func(b map[string]any) { b["site_title"] = "   " }},
		{"站点名超长", func(b map[string]any) { b["site_title"] = strings.Repeat("a", 65) }},
		{"站点地址非 http", func(b map[string]any) { b["server_address"] = "ftp://example.com" }},
		{"站点地址无主机", func(b map[string]any) { b["server_address"] = "https://" }},
		{"站点地址带查询串", func(b map[string]any) { b["server_address"] = "https://example.com/?a=1" }},
		{"站点地址带 userinfo", func(b map[string]any) { b["server_address"] = "https://user:pass@example.com" }},
		{"站点地址带 fragment", func(b map[string]any) { b["server_address"] = "https://example.com/#frag" }},
		{"站点地址带空 query", func(b map[string]any) { b["server_address"] = "https://example.com?" }},
		{"站点地址带空 fragment", func(b map[string]any) { b["server_address"] = "https://example.com/#" }},
		{"站点地址带空白", func(b map[string]any) { b["server_address"] = "https://example.com/a b" }},
		{"站点地址超长", func(b map[string]any) { b["server_address"] = "https://example.com/" + strings.Repeat("a", 256) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSetupTestEnv(t, setupTestOptions{})
			body := setupPayload()
			tc.mutate(body)
			rr := env.post(body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
			if complete, _ := env.store.callCounts(); complete != 0 {
				t.Fatalf("校验失败的请求不应进入写事务，CompleteSetup 调用 %d 次", complete)
			}
		})
	}
}

func TestSetupRejectsCompromisedPassword(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{})
	body := setupPayload()
	body["password"] = "newtg2026"
	body["confirmPassword"] = "newtg2026"
	rr := env.post(body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "compromised") {
		t.Errorf("响应未说明口令弱/已泄露: %q", rr.Body.String())
	}
	if complete, _ := env.store.callCounts(); complete != 0 {
		t.Fatal("弱口令不应进入写事务")
	}
}

// 站点名与用户名都必须作为每用户上下文词传给 checker（security-01 §2.3）：
// 「NewTG2026」「acme2026」这类可猜测口令不能被纯长度策略放过。
func TestSetupRejectsSiteTitleInPassword(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{})
	body := setupPayload()
	body["site_title"] = "Zebraworks"
	body["password"] = "zebraworks9"
	body["confirmPassword"] = "zebraworks9"
	rr := env.post(body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("含站点名的口令 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "compromised") {
		t.Errorf("响应未说明口令弱/已泄露: %q", rr.Body.String())
	}
}

func TestSetupRejectsUsernameInPassword(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{})
	body := setupPayloadFor("zebracorn")
	body["password"] = "zebracorn123"
	body["confirmPassword"] = "zebracorn123"
	rr := env.post(body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("含用户名的口令 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestSetupStoreErrors(t *testing.T) {
	t.Run("状态检查失败返回 503", func(t *testing.T) {
		store := newFakeSetupStore()
		store.isInitErr = errors.New("db down")
		env := newSetupTestEnv(t, setupTestOptions{store: store})
		rr := env.post(setupPayload())
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Error("503 应带 Retry-After")
		}
		if complete, _ := store.callCounts(); complete != 0 {
			t.Fatal("状态未知时不应进入写事务")
		}
	})

	t.Run("写事务失败返回 500", func(t *testing.T) {
		store := newFakeSetupStore()
		store.completeErr = errors.New("boom")
		env := newSetupTestEnv(t, setupTestOptions{store: store})
		rr := env.post(setupPayload())
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d，期望 500（body=%q）", rr.Code, rr.Body.String())
		}
	})
}

func TestSetupDuplicateAdminConflict(t *testing.T) {
	store := newFakeSetupStore()
	store.users = append(store.users, setupStoredUser{Input: SetupInput{
		Username: "admin", Email: "admin@example.com",
	}})
	env := newSetupTestEnv(t, setupTestOptions{store: store})

	rr := env.post(setupPayload())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	if _, ok := store.setting(SetupCompletedKey); ok {
		t.Fatal("用户名/邮箱冲突时不得写入幂等锚")
	}
}

// ---------------------------------------------------------------------------
// 审计事件 system_initialized（modules-01 F-01-20）
// ---------------------------------------------------------------------------

func TestSetupEmitsSystemInitializedAudit(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
		got   struct {
			action string
			actor  int64
			detail json.RawMessage
		}
	)
	env := newSetupTestEnv(t, setupTestOptions{
		audit: func(_ context.Context, action string, actor int64, detail json.RawMessage) error {
			mu.Lock()
			defer mu.Unlock()
			calls++
			got.action = action
			got.actor = actor
			got.detail = detail
			return nil
		},
	})

	rr := env.post(setupPayload())
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	user := decodeSetupUser(t, rr)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("审计回调调用 %d 次，期望 1", calls)
	}
	if got.action != audit.ActionSystemInitialized {
		t.Errorf("audit action = %q，期望 %q", got.action, audit.ActionSystemInitialized)
	}
	if got.actor != user.ID {
		t.Errorf("audit actor = %d，期望首管 id %d", got.actor, user.ID)
	}
	var detail map[string]any
	if err := json.Unmarshal(got.detail, &detail); err != nil {
		t.Fatalf("audit detail 非法: %v（raw=%q）", err, string(got.detail))
	}
	if detail["action"] != audit.ActionSystemInitialized {
		t.Errorf("detail.action = %v，期望 %q", detail["action"], audit.ActionSystemInitialized)
	}
	if detail["user_id"] != float64(user.ID) || detail["username"] != "admin" {
		t.Errorf("detail 身份字段不对: %v", detail)
	}
}

// 审计失败不得回滚已提交的初始化（日志库独立，且无跨库事务）。
func TestSetupAuditFailureDoesNotRollback(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{
		audit: func(context.Context, string, int64, json.RawMessage) error {
			return errors.New("audit db down")
		},
	})
	rr := env.post(setupPayload())
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（审计失败不回滚，body=%q）", rr.Code, rr.Body.String())
	}
	if env.store.userCount() != 1 {
		t.Fatal("审计失败后 admin 仍应存在")
	}
	if _, ok := env.store.setting(SetupCompletedKey); !ok {
		t.Fatal("审计失败后幂等锚仍应存在")
	}
}

// TestSetupAuditPersistsSystemInitialized 用真实 audit.Logger + fake Execer 证明
// 新动作已被冻结枚举接受，并按 operation 类落库、detail 带 action。
func TestSetupAuditPersistsSystemInitialized(t *testing.T) {
	exec := &setupAuditExec{}
	env := newSetupTestEnv(t, setupTestOptions{audit: audit.New(exec).Log})

	rr := env.post(setupPayload())
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if len(exec.args) != 1 {
		t.Fatalf("审计写入 %d 行，期望 1", len(exec.args))
	}
	args := exec.args[0]
	// 列顺序：user_id, username, category, ip, user_agent, method, route,
	// status_code, result, detail, before, after。
	if args[2] != "operation" {
		t.Errorf("category = %v，期望 operation", args[2])
	}
	detail, ok := args[9].(string)
	if !ok {
		t.Fatalf("detail 类型 = %T，期望 string", args[9])
	}
	if !strings.Contains(detail, `"action":"system_initialized"`) {
		t.Errorf("detail 未记录 action: %s", detail)
	}
	if !strings.Contains(detail, `"username":"admin"`) {
		t.Errorf("detail 未记录用户名: %s", detail)
	}
}

// 建会话失败发生在提交与审计之后：初始化不能回滚，审计仍须落库。
func TestSetupSessionFailureStillAudits(t *testing.T) {
	auditCalled := false
	env := newSetupTestEnv(t, setupTestOptions{
		withSessions: true,
		audit: func(context.Context, string, int64, json.RawMessage) error {
			auditCalled = true
			return nil
		},
	})
	// 只破坏会话管理器的 Redis：限流桶仍可用（否则请求会在限流层 503，
	// 测不到「提交+审计之后建会话失败」的路径）。
	brokenRedis := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = brokenRedis.Close() })
	env.handlers.sessions = auth.NewSessionManager(brokenRedis, []byte(testSessionSecret))

	rr := env.post(setupPayload())
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
	}
	if !auditCalled {
		t.Fatal("建会话失败前必须已写 system_initialized 审计")
	}
	if env.store.userCount() != 1 {
		t.Fatal("会话失败不得回滚已完成的初始化")
	}
}

// setupAuditExec 捕获 audit.Logger 的 INSERT 参数。
type setupAuditExec struct {
	mu   sync.Mutex
	args [][]any
}

func (e *setupAuditExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.args = append(e.args, append([]any(nil), args...))
	return setupAuditResult(1), nil
}

type setupAuditResult int64

func (setupAuditResult) LastInsertId() (int64, error) { return 0, nil }

func (r setupAuditResult) RowsAffected() (int64, error) { return int64(r), nil }

// ---------------------------------------------------------------------------
// 构造与路由
// ---------------------------------------------------------------------------

func TestSetupNewHandlersRequiresStore(t *testing.T) {
	if _, err := NewSetupHandlers(SetupConfig{}); err == nil {
		t.Fatal("缺少 Store 时 NewSetupHandlers 应返回错误")
	}
}

// F-01-20 要求初始化后自动登录：生产装配缺少 Sessions 必须在启动期失败，
// 而不是静默跳过自动登录。测试/受限装配可显式 AllowNoSession.
func TestSetupNewHandlersRequiresSessions(t *testing.T) {
	store := newFakeSetupStore()
	if _, err := NewSetupHandlers(SetupConfig{Store: store}); err == nil {
		t.Fatal("缺少 Sessions 时应返回错误（自动登录是 F-01-20 的验收项）")
	}
	if _, err := NewSetupHandlers(SetupConfig{Store: store, AllowNoSession: true}); err != nil {
		t.Fatalf("显式 AllowNoSession 时应允许构造: %v", err)
	}
}

func TestSetupOnlyPostRouteRegistered(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{})
	req := httptest.NewRequest(http.MethodGet, "/api/setup", nil)
	rr := serve(env.mux, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/setup = %d，期望 405（仅注册 POST）", rr.Code)
	}
}

// TestSetupPublishesConfigChanges：setup 为了事务直接写 system_settings，绕过
// 了 configcenter.Save；提交后必须手动广播冻结频道，否则各副本要等 5 分钟
// 兜底快照才能看到新的 site.title / site.server_address / 初始化锚。
func TestSetupPublishesConfigChanges(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{withSessions: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pubsub := env.rdb.Subscribe(ctx, configcenter.ChangedChannel)
	defer func() { _ = pubsub.Close() }()
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("订阅配置变更频道失败: %v", err)
	}

	if rr := env.post(setupPayload()); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}

	got := map[string]bool{}
	for len(got) < 3 {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			t.Fatalf("等待配置变更广播失败（已收到 %v）: %v", got, err)
		}
		got[msg.Payload] = true
	}
	for _, key := range []string{SetupCompletedKey, setupSiteTitleKey, setupServerAddressKey} {
		if !got[key] {
			t.Errorf("缺少配置变更广播 %q（收到 %v）", key, got)
		}
	}
}

// TestSetupRateLimited 覆盖 B13：/api/setup 的匿名桶在 10 次/分钟后返回
// 429 + Retry-After，避免未初始化窗口被自动化抢占与 argon2 成本轰炸。
func TestSetupRateLimited(t *testing.T) {
	env := newSetupTestEnv(t, setupTestOptions{withSessions: true})
	// 空对象会在业务校验处 400，但仍消耗桶名额（限流先于解析/KDF）。
	for i := 0; i < SetupRateLimit; i++ {
		if rr := env.post(map[string]any{}); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次请求提前触发限流", i+1)
		}
	}
	rr := env.post(map[string]any{})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次请求 status = %d，期望 429（body=%q）", SetupRateLimit+1, rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
	if env.store.userCount() != 0 {
		t.Fatalf("限流后不得创建用户，实际 %d", env.store.userCount())
	}
}

// TestSetupRateLimiterUnavailableFailsClosed 覆盖 B13 的 Redis 故障分支：
// 无法计数时拒绝初始化（503），不得放行到 argon2/写事务。
func TestSetupRateLimiterUnavailableFailsClosed(t *testing.T) {
	store := newFakeSetupStore()
	broken := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = broken.Close() })
	handlers, err := NewSetupHandlers(SetupConfig{
		Store: store, Redis: broken, AllowNoSession: true,
	})
	if err != nil {
		t.Fatalf("NewSetupHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.RegisterSetupRoutes(mux)

	raw, _ := json.Marshal(setupPayload())
	req := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := serve(mux, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
	}
	if store.userCount() != 0 {
		t.Fatalf("限流器不可用时不得创建用户，实际 %d", store.userCount())
	}
}
