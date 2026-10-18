package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 8 密码端点（F-01-05/06/16）的单元验收。全部依赖是内存假实现
// （miniredis + 进程内用户表），未打 integration 标签时不触碰外部 PG/Redis。
//
// 路由钩子：Task 8 不修改 routes.go，由路由装配方调用
// (*Router).RegisterPasswordRoutes（见 handlers_password.go 的文档）。

const passwordTestBaseURL = "https://app.example.com"

// resetTestSender 同时实现 EmailCodeSender 与 PasswordResetSender：验证码接口
// 未被本任务使用，重置链接按专用方法捕获，测试据此提取明文 token。
type resetTestSender struct {
	mu    sync.Mutex
	links []sentResetLink
	err   error
}

type sentResetLink struct {
	Email    string
	ResetURL string
}

func (s *resetTestSender) SendEmailCode(_ context.Context, _, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *resetTestSender) SendPasswordReset(_ context.Context, email, resetURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.links = append(s.links, sentResetLink{Email: email, ResetURL: resetURL})
	return nil
}

func (s *resetTestSender) last(t *testing.T) sentResetLink {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.links) == 0 {
		t.Fatal("没有任何重置邮件被发送")
	}
	return s.links[len(s.links)-1]
}

func (s *resetTestSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.links)
}

// pwdTestRepo 复用 Task 6 的内存用户表（AuthRepo 的三个方法由嵌入提供），
// 只补 PasswordRepo 需要的按 id/邮箱查找与口令更新。
type pwdTestRepo struct {
	*fakeAuthRepo
	updates []passwordUpdate
}

type passwordUpdate struct {
	UserID int64
	Hash   string
}

func newPwdTestRepo() *pwdTestRepo {
	return &pwdTestRepo{fakeAuthRepo: newFakeAuthRepo()}
}

func (r *pwdTestRepo) FindUserByEmail(_ context.Context, email string) (*UserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byEmail[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return nil, nil
	}
	u := *r.users[id]
	return &u, nil
}

func (r *pwdTestRepo) FindUserByID(_ context.Context, userID int64) (*UserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[userID]
	if !ok {
		return nil, nil
	}
	copied := *u
	return &copied, nil
}

func (r *pwdTestRepo) UpdatePasswordHash(_ context.Context, userID int64, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[userID]
	if !ok {
		return ErrPasswordUserNotFound
	}
	u.PasswordHash = hash
	u.MustChangePassword = false
	r.updates = append(r.updates, passwordUpdate{UserID: userID, Hash: hash})
	return nil
}

func (r *pwdTestRepo) storedHash(t *testing.T, userID int64) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[userID]
	if !ok {
		t.Fatalf("用户 %d 不存在", userID)
	}
	return u.PasswordHash
}

type passwordTestEnv struct {
	t        *testing.T
	router   *Router
	repo     *pwdTestRepo
	sessions *auth.SessionManager
	rdb      *redis.Client
	mr       *miniredis.Miniredis
	sender   *resetTestSender
}

func newPasswordTestEnv(t *testing.T) *passwordTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := newPwdTestRepo()
	sender := &resetTestSender{}
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	router, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          repo,
		SessionSecret: []byte(testSessionSecret),
		PublicBaseURL: passwordTestBaseURL,
		EmailSender:   sender,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := router.RegisterPasswordRoutes(passwordTestBaseURL); err != nil {
		t.Fatalf("RegisterPasswordRoutes: %v", err)
	}
	return &passwordTestEnv{
		t: t, router: router, repo: repo, sessions: sessions,
		rdb: rdb, mr: mr, sender: sender,
	}
}

func (e *passwordTestEnv) request(method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
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
	if mutate != nil {
		mutate(req)
	}
	return serve(e.router, req)
}

// requestReset 从指定 IP 发起找回密码请求。
func (e *passwordTestEnv) requestReset(email, ip string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.request(http.MethodPost, "/api/auth/password-reset/request",
		map[string]any{"email": email}, func(r *http.Request) {
			r.RemoteAddr = ip + ":4321"
		})
}

// resetTokenFor 请求重置并从已发送链接中取出明文 token。
func (e *passwordTestEnv) resetTokenFor(email, ip string) string {
	e.t.Helper()
	rr := e.requestReset(email, ip)
	if rr.Code != http.StatusNoContent {
		e.t.Fatalf("重置请求 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	link := e.sender.last(e.t)
	u, err := url.Parse(link.ResetURL)
	if err != nil {
		e.t.Fatalf("重置链接非法: %v", err)
	}
	token := u.Query().Get("token")
	if token == "" {
		e.t.Fatalf("重置链接缺少 token: %q", link.ResetURL)
	}
	return token
}

func (e *passwordTestEnv) sessionWithCSRF(userID int64) (sid, csrf string) {
	e.t.Helper()
	sid = mustSession(e.t, e.sessions, userID, "user")
	csrf = auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		e.t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

// authedRequest 走完整受保护链：会话 Cookie + CSRF + 同源 Origin。
func (e *passwordTestEnv) authedRequest(method, path string, body any, sid, csrf string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.request(method, path, body, func(r *http.Request) {
		r.Host = "app.example.com"
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
		r.Header.Set(auth.CSRFHeaderName, csrf)
		r.Header.Set("Origin", passwordTestBaseURL)
	})
}

func (e *passwordTestEnv) addUser(username, email, hash string) int64 {
	e.t.Helper()
	u := e.repo.add(UserRecord{Username: username, Email: email, PasswordHash: hash})
	return u.ID
}

func decodeProblemType(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var p problemBody
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("problem 体不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return p.Type
}

// ---------------------------------------------------------------------------
// F-01-05 找回密码
// ---------------------------------------------------------------------------

// TestPasswordResetRequestUniform 覆盖 brief (a) / TC-F-01-05-01：已注册与
// 未注册邮箱必须得到逐字相同的响应，且只给已注册邮箱发信。
func TestPasswordResetRequestUniform(t *testing.T) {
	env := newPasswordTestEnv(t)
	env.addUser("alice", "alice@example.com", authTestPasswordHash())

	known := env.requestReset("alice@example.com", "192.0.2.10")
	unknown := env.requestReset("nobody@example.com", "192.0.2.11")

	if known.Code != http.StatusNoContent || unknown.Code != http.StatusNoContent {
		t.Fatalf("known=%d unknown=%d，均期望 204（防枚举）", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("响应体不同：known=%q unknown=%q（防枚举）", known.Body.String(), unknown.Body.String())
	}
	if env.sender.count() != 1 {
		t.Fatalf("发送次数 = %d，期望只给已注册邮箱发送 1 封", env.sender.count())
	}
	if got := env.sender.last(t).Email; got != "alice@example.com" {
		t.Fatalf("收件人 = %q，期望 alice@example.com", got)
	}
	if !strings.HasPrefix(env.sender.last(t).ResetURL, passwordTestBaseURL+"/reset-password?token=") {
		t.Fatalf("重置链接形态不符: %q", env.sender.last(t).ResetURL)
	}
}

// TestPasswordResetRequestRateLimit 覆盖 brief (b)：同邮箱 60s 重发 429；
// 同 IP 5 次/分钟，第 6 次 429（阈值唯一来源 modules-08 F-08-16）。
func TestPasswordResetRequestRateLimit(t *testing.T) {
	t.Run("同邮箱重发冷却", func(t *testing.T) {
		env := newPasswordTestEnv(t)
		env.addUser("alice", "alice@example.com", authTestPasswordHash())

		if rr := env.requestReset("alice@example.com", "192.0.2.20"); rr.Code != http.StatusNoContent {
			t.Fatalf("首次请求 status = %d，期望 204", rr.Code)
		}
		rr := env.requestReset("alice@example.com", "192.0.2.20")
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("冷却期内重发 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Fatal("429 必须带 Retry-After")
		}
	})

	t.Run("同 IP 每分钟 5 次", func(t *testing.T) {
		env := newPasswordTestEnv(t)
		const ip = "192.0.2.21"
		for i := 0; i < passwordResetIPLimit; i++ {
			email := "user" + string(rune('a'+i)) + "@example.com"
			if rr := env.requestReset(email, ip); rr.Code != http.StatusNoContent {
				t.Fatalf("第 %d 次请求 status = %d，期望 204（body=%q）", i+1, rr.Code, rr.Body.String())
			}
		}
		rr := env.requestReset("userf@example.com", ip)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("第 6 次请求 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
		}
	})
}

// TestPasswordResetTokenValidation：validate 有效 token 200；未知/畸形 token 400。
func TestPasswordResetTokenValidation(t *testing.T) {
	env := newPasswordTestEnv(t)
	env.addUser("alice", "alice@example.com", authTestPasswordHash())
	token := env.resetTokenFor("alice@example.com", "192.0.2.30")

	rr := env.request(http.MethodPost, "/api/auth/password-reset/validate",
		map[string]any{"token": token}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("有效 token validate status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var body struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || !body.Valid {
		t.Fatalf("validate 响应 = %q（err=%v），期望 {\"valid\":true}", rr.Body.String(), err)
	}

	for _, bad := range []string{"", "not-a-token", strings.Repeat("A", 43)} {
		rr := env.request(http.MethodPost, "/api/auth/password-reset/validate",
			map[string]any{"token": bad}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("无效 token %q status = %d，期望 400（body=%q）", bad, rr.Code, rr.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// F-01-06 重置密码
// ---------------------------------------------------------------------------

// TestPasswordResetConfirmFlow 覆盖 brief (c) / TC-F-01-06-01：重置成功后
// 口令更新为 argon2id 新哈希，全部旧会话失效，旧口令不能再登录。
func TestPasswordResetConfirmFlow(t *testing.T) {
	env := newPasswordTestEnv(t)
	const oldPassword = "CorrectHorse9!"
	const newPassword = "N3w!Passw0rd-X"
	userID := env.addUser("alice", "alice@example.com", authTestPasswordHash())

	sid1 := mustSession(t, env.sessions, userID, "user")
	sid2 := mustSession(t, env.sessions, userID, "user")
	token := env.resetTokenFor("alice@example.com", "192.0.2.40")

	rr := env.request(http.MethodPost, "/api/auth/password-reset/confirm", map[string]any{
		"token": token, "new_password": newPassword, "confirm": newPassword,
	}, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("confirm status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	stored := env.repo.storedHash(t, userID)
	if !auth.VerifyPassword(stored, newPassword) {
		t.Fatal("新口令未写入（hash 校验失败）")
	}
	if auth.VerifyPassword(stored, oldPassword) {
		t.Fatal("旧口令仍可通过校验")
	}

	for i, sid := range []string{sid1, sid2} {
		if _, err := env.sessions.Get(context.Background(), sid); !errors.Is(err, auth.ErrSessionNotFound) {
			t.Fatalf("旧会话 %d err = %v，期望 ErrSessionNotFound", i+1, err)
		}
	}

	// 旧口令 401、新口令 200。
	if rr := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "alice", "password": oldPassword,
	}, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("旧口令登录 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "alice", "password": newPassword,
	}, nil); rr.Code != http.StatusOK {
		t.Fatalf("新口令登录 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestPasswordResetTokenSingleUse 覆盖 brief (d) / TC-F-01-06-02：同一 token
// 第二次提交必须 400（消费是原子的，即使第一次改密后没有更新 DB 也一样）。
func TestPasswordResetTokenSingleUse(t *testing.T) {
	env := newPasswordTestEnv(t)
	const newPassword = "N3w!Passw0rd-X"
	env.addUser("alice", "alice@example.com", authTestPasswordHash())
	token := env.resetTokenFor("alice@example.com", "192.0.2.50")

	body := map[string]any{"token": token, "new_password": newPassword, "confirm": newPassword}
	if rr := env.request(http.MethodPost, "/api/auth/password-reset/confirm", body, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("首次 confirm status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(http.MethodPost, "/api/auth/password-reset/confirm", body, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("token 复用 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	// validate 与 confirm 口径一致：已消费的 token 视为无效。
	if rr := env.request(http.MethodPost, "/api/auth/password-reset/validate",
		map[string]any{"token": token}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("已消费 token validate status = %d，期望 400", rr.Code)
	}
}

// TestPasswordResetTokenExpired 覆盖 brief (e) / TC-F-01-06-03：超过 30 分钟
// 的 token 必须 400，且不得更新口令。
func TestPasswordResetTokenExpired(t *testing.T) {
	env := newPasswordTestEnv(t)
	const newPassword = "N3w!Passw0rd-X"
	userID := env.addUser("alice", "alice@example.com", authTestPasswordHash())
	token := env.resetTokenFor("alice@example.com", "192.0.2.60")

	env.mr.FastForward(passwordResetTokenTTL + time.Minute)

	body := map[string]any{"token": token, "new_password": newPassword, "confirm": newPassword}
	if rr := env.request(http.MethodPost, "/api/auth/password-reset/confirm", body, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("过期 token confirm status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(http.MethodPost, "/api/auth/password-reset/validate",
		map[string]any{"token": token}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("过期 token validate status = %d，期望 400", rr.Code)
	}
	if stored := env.repo.storedHash(t, userID); !auth.VerifyPassword(stored, authTestPassword) {
		t.Fatal("过期 token 不得改动口令")
	}
}

// TestPasswordResetConfirmInputChecks：口令长度/确认不一致/弱口令都在消费
// token 之前拒绝，避免一次输入错误就烧掉重置链接。
func TestPasswordResetConfirmInputChecks(t *testing.T) {
	env := newPasswordTestEnv(t)
	env.addUser("alice", "alice@example.com", authTestPasswordHash())
	token := env.resetTokenFor("alice@example.com", "192.0.2.70")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"过短", map[string]any{"token": token, "new_password": "short", "confirm": "short"}},
		{"确认不一致", map[string]any{"token": token, "new_password": "N3w!Passw0rd-X", "confirm": "N3w!Passw0rd-Y"}},
		{"泄露口令", map[string]any{"token": token, "new_password": "password1", "confirm": "password1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(http.MethodPost, "/api/auth/password-reset/confirm", tc.body, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
		})
	}
	// 失败输入不得消费 token：随后仍可正常重置。
	body := map[string]any{"token": token, "new_password": "N3w!Passw0rd-X", "confirm": "N3w!Passw0rd-X"}
	if rr := env.request(http.MethodPost, "/api/auth/password-reset/confirm", body, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("输入校验失败后 token 应仍有效，status = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// F-01-16 修改密码
// ---------------------------------------------------------------------------

// TestPasswordChangeRevokesOtherSessions 覆盖 brief (f) / TC-F-01-16-01：
// 改密后除当前会话外全部失效，当前会话保持可用且 CSRF 轮换。
func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	env := newPasswordTestEnv(t)
	const newPassword = "N3w!Passw0rd-X"
	userID := env.addUser("alice", "alice@example.com", authTestPasswordHash())

	sidCurrent, csrfCurrent := env.sessionWithCSRF(userID)
	sidOther := mustSession(t, env.sessions, userID, "user")

	rr := env.authedRequest(http.MethodPost, "/api/password", map[string]any{
		"current_password": authTestPassword,
		"new_password":     newPassword,
	}, sidCurrent, csrfCurrent)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("改密 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	// 当前会话存活，其余会话失效。
	if _, err := env.sessions.Get(context.Background(), sidCurrent); err != nil {
		t.Fatalf("当前会话 err = %v，期望仍然有效", err)
	}
	if _, err := env.sessions.Get(context.Background(), sidOther); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("其他会话 err = %v，期望 ErrSessionNotFound", err)
	}

	// 口令已更新，且 must_change_password 被清除。
	stored := env.repo.storedHash(t, userID)
	if !auth.VerifyPassword(stored, newPassword) {
		t.Fatal("新口令未写入")
	}
	if env.repo.users[userID].MustChangePassword {
		t.Fatal("改密后 must_change_password 应为 false")
	}

	// CSRF 轮换：响应下发新令牌，旧令牌立即失效。
	rotated := cookieByName(rr, auth.CSRFCookieName)
	if rotated == nil {
		t.Fatal("改密响应必须下发轮换后的 CSRF Cookie")
	}
	if rotated.Value == csrfCurrent {
		t.Fatal("CSRF 令牌未轮换")
	}
	if !env.sessions.VerifyCSRF(context.Background(), sidCurrent, rotated.Value) {
		t.Fatal("轮换后的 CSRF 令牌校验失败")
	}
	if env.sessions.VerifyCSRF(context.Background(), sidCurrent, csrfCurrent) {
		t.Fatal("旧 CSRF 令牌仍然有效")
	}

	// 旧口令不能登录，新口令可以。
	if rr := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "alice", "password": authTestPassword,
	}, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("旧口令登录 status = %d，期望 401", rr.Code)
	}
	if rr := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "alice", "password": newPassword,
	}, nil); rr.Code != http.StatusOK {
		t.Fatalf("新口令登录 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestPasswordChangeWrongCurrentPassword 覆盖 TC-F-01-16-02：当前密码错误
// 返回 400 且不落库、不吊销任何会话。
func TestPasswordChangeWrongCurrentPassword(t *testing.T) {
	env := newPasswordTestEnv(t)
	userID := env.addUser("alice", "alice@example.com", authTestPasswordHash())
	sid, csrf := env.sessionWithCSRF(userID)

	rr := env.authedRequest(http.MethodPost, "/api/password", map[string]any{
		"current_password": "definitely-wrong",
		"new_password":     "N3w!Passw0rd-X",
	}, sid, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.updates) != 0 {
		t.Fatalf("当前密码错误时不得落库，实际更新 %d 次", len(env.repo.updates))
	}
	if _, err := env.sessions.Get(context.Background(), sid); err != nil {
		t.Fatalf("当前密码错误不得吊销会话: %v", err)
	}
}

// TestPasswordChangeRejectsSamePassword：新旧口令相同必须 400（F-01-16）。
func TestPasswordChangeRejectsSamePassword(t *testing.T) {
	env := newPasswordTestEnv(t)
	userID := env.addUser("alice", "alice@example.com", authTestPasswordHash())
	sid, csrf := env.sessionWithCSRF(userID)

	rr := env.authedRequest(http.MethodPost, "/api/password", map[string]any{
		"current_password": authTestPassword,
		"new_password":     authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.updates) != 0 {
		t.Fatal("新旧口令相同时不得落库")
	}
}

// TestPasswordChangeRateLimit：5 次/10 分钟/IP+user，第 6 次 429。
func TestPasswordChangeRateLimit(t *testing.T) {
	env := newPasswordTestEnv(t)
	userID := env.addUser("alice", "alice@example.com", authTestPasswordHash())
	sid, csrf := env.sessionWithCSRF(userID)

	body := map[string]any{
		"current_password": "definitely-wrong",
		"new_password":     "N3w!Passw0rd-X",
	}
	for i := 0; i < passwordChangeLimit; i++ {
		if rr := env.authedRequest(http.MethodPost, "/api/password", body, sid, csrf); rr.Code != http.StatusBadRequest {
			t.Fatalf("第 %d 次 status = %d，期望 400（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := env.authedRequest(http.MethodPost, "/api/password", body, sid, csrf)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次 status = %d，期望 429（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
}

// TestPasswordChangeRequiresAuth：无会话时 401（防止路由钩子挂错位置）。
func TestPasswordChangeRequiresAuth(t *testing.T) {
	env := newPasswordTestEnv(t)
	rr := env.request(http.MethodPost, "/api/password", map[string]any{
		"current_password": "x", "new_password": "N3w!Passw0rd-X",
	}, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("未认证 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestReauthRequiredProblem 覆盖 brief (g) 的 HTTP 面：Task 10 把
// auth.ErrReauthRequired 映射为 403 + RFC 7807 type=reauth_required。
func TestReauthRequiredProblem(t *testing.T) {
	rr := httptest.NewRecorder()
	writeReauthRequired(rr)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403", rr.Code)
	}
	if got := decodeProblemType(t, rr); got != problemTypeReauthRequired {
		t.Fatalf("problem type = %q，期望 %q", got, problemTypeReauthRequired)
	}
}

// TestRegisterPasswordRoutesRequiresPasswordRepo：装配期必须拒绝缺少
// PasswordRepo 的仓储，不能等到请求时才失败。
func TestRegisterPasswordRoutesRequiresPasswordRepo(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	router, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Repo:          newFakeAuthRepo(), // 只有 AuthRepo，没有 PasswordRepo
		SessionSecret: []byte(testSessionSecret),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := router.RegisterPasswordRoutes(""); err == nil {
		t.Fatal("缺少 PasswordRepo 时 RegisterPasswordRoutes 必须返回错误")
	}
}
