package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件是 Task 6 的单元验收：注册 / 登录 / 登出 / 失败锁定 / 公开状态。
// 全部依赖都用内存假实现（miniredis + fakeAuthRepo + fakeEmailSender），
// 未打 integration 标签时不得触碰外部 PG/Redis（GC-8）。

const authTestPassword = "CorrectHorse9!"

// authTestPasswordHash 只算一次 argon2id；同一口令的所有测试用户复用同一哈希，
// 避免每个用例都付出 ~30ms 的 KDF 成本。
var authTestPasswordHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(authTestPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// mapConfig 是 configcenter.Center 的读取面假实现（Get 签名逐字一致）。
type mapConfig map[string]json.RawMessage

func (m mapConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

func configBool(v bool) json.RawMessage { return json.RawMessage(strconv.FormatBool(v)) }
func configInt(v int64) json.RawMessage { return json.RawMessage(strconv.FormatInt(v, 10)) }
func configString(v string) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

// fakeEmailSender 捕获明文验证码供测试断言；生产中由 SMTP 实现替换。
type fakeEmailSender struct {
	mu    sync.Mutex
	codes []sentEmailCode
	err   error
}

type sentEmailCode struct {
	Email   string
	Purpose string
	Code    string
}

func (f *fakeEmailSender) SendEmailCode(_ context.Context, email, purpose, code string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.codes = append(f.codes, sentEmailCode{Email: email, Purpose: purpose, Code: code})
	return nil
}

func (f *fakeEmailSender) last(t *testing.T) sentEmailCode {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.codes) == 0 {
		t.Fatal("没有任何验证码被发送")
	}
	return f.codes[len(f.codes)-1]
}

func (f *fakeEmailSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.codes)
}

// fakeAuthRepo 是 AuthRepo 的内存实现。nil 与空切片的权限语义与 DB 对齐：
// 未记录的 userID 返回 nil（NULL=FULL），显式登记空切片返回空集合。
type fakeAuthRepo struct {
	mu            sync.Mutex
	users         map[int64]*UserRecord
	byUsername    map[string]int64
	byEmail       map[string]int64
	nextID        int64
	createRole    string
	forceCreateEr error
	created       []CreateUserInput
	permissions   map[int64][]string
}

func newFakeAuthRepo() *fakeAuthRepo {
	return &fakeAuthRepo{
		users:       map[int64]*UserRecord{},
		byUsername:  map[string]int64{},
		byEmail:     map[string]int64{},
		permissions: map[int64][]string{},
	}
}

func (r *fakeAuthRepo) add(u UserRecord) *UserRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	u.ID = r.nextID
	if u.Role == "" {
		u.Role = rbac.RoleUser
	}
	if u.Status == "" {
		u.Status = "enabled"
	}
	rec := u
	r.users[u.ID] = &rec
	r.byUsername[u.Username] = u.ID
	if u.Email != "" {
		r.byEmail[strings.ToLower(u.Email)] = u.ID
	}
	return &rec
}

func (r *fakeAuthRepo) FindUserByIdentifier(_ context.Context, identifier string) (*UserRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.byUsername[identifier]; ok {
		u := *r.users[id]
		return &u, nil
	}
	if id, ok := r.byEmail[strings.ToLower(identifier)]; ok {
		u := *r.users[id]
		return &u, nil
	}
	return nil, nil
}

func (r *fakeAuthRepo) UsernameOrEmailTaken(_ context.Context, username, email string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, byName := r.byUsername[username]
	_, byMail := r.byEmail[strings.ToLower(email)]
	return byName || byMail, nil
}

func (r *fakeAuthRepo) CreateUser(_ context.Context, in CreateUserInput) (CreatedUser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.forceCreateEr != nil {
		return CreatedUser{}, r.forceCreateEr
	}
	if _, ok := r.byUsername[in.Username]; ok {
		return CreatedUser{}, ErrDuplicateUser
	}
	if _, ok := r.byEmail[strings.ToLower(in.Email)]; ok {
		return CreatedUser{}, ErrDuplicateUser
	}
	r.nextID++
	role := r.createRole
	if role == "" {
		role = rbac.RoleUser
	}
	r.users[r.nextID] = &UserRecord{
		ID:            r.nextID,
		Username:      in.Username,
		Email:         in.Email,
		PasswordHash:  in.PasswordHash,
		Role:          role,
		Status:        "enabled",
		EmailVerified: in.EmailVerified,
	}
	r.byUsername[in.Username] = r.nextID
	r.byEmail[strings.ToLower(in.Email)] = r.nextID
	r.created = append(r.created, in)
	return CreatedUser{ID: r.nextID, Role: role, Status: "enabled"}, nil
}

func (r *fakeAuthRepo) Load(_ context.Context, userID int64) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	perms, ok := r.permissions[userID]
	if !ok {
		return nil, nil // 未登记 = DB NULL = FULL
	}
	return perms, nil
}

// staticPermissions 是可控的 PermissionLoader，用于验证 Task 5 的装载钩子。
type staticPermissions struct {
	perms []string
	err   error
}

func (s staticPermissions) Load(_ context.Context, _ int64) ([]string, error) {
	return s.perms, s.err
}

type authTestOptions struct {
	repo          *fakeAuthRepo
	sender        *fakeEmailSender
	permissions   rbac.PermissionLoader
	authz         *rbac.AuthzMiddleware
	publicBaseURL string
}

type authTestEnv struct {
	t        *testing.T
	router   *Router
	repo     *fakeAuthRepo
	sender   *fakeEmailSender
	mr       *miniredis.Miniredis
	rdb      *redis.Client
	sessions *auth.SessionManager
	config   mapConfig
}

func newAuthTestEnv(t *testing.T, cfg mapConfig, opts authTestOptions) *authTestEnv {
	t.Helper()
	if cfg == nil {
		cfg = mapConfig{}
	}
	repo := opts.repo
	if repo == nil {
		repo = newFakeAuthRepo()
	}
	sender := opts.sender
	if sender == nil {
		sender = &fakeEmailSender{}
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))

	rt, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          repo,
		Config:        cfg,
		SessionSecret: []byte(testSessionSecret),
		PublicBaseURL: opts.publicBaseURL,
		EmailSender:   sender,
		Permissions:   opts.permissions,
		Authz:         opts.authz,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	return &authTestEnv{
		t:        t,
		router:   rt,
		repo:     repo,
		sender:   sender,
		mr:       mr,
		rdb:      rdb,
		sessions: sessions,
		config:   cfg,
	}
}

// request 发起一次 JSON 请求；mutate 用于追加 Cookie/Header。
func (e *authTestEnv) request(method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
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

func cookieByName(rr *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func registerPayload(username, email, password, code string) map[string]any {
	body := map[string]any{
		"username":        username,
		"email":           email,
		"password":        password,
		"confirmPassword": password,
		"agree_terms":     true,
	}
	if code != "" {
		body["email_code"] = code
	}
	return body
}

func (e *authTestEnv) login(identifier, password string) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": identifier,
		"password":   password,
	}, nil)
}

func decodeAuthUser(t *testing.T, rr *httptest.ResponseRecorder) authSessionResponse {
	t.Helper()
	var resp authSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解码会话响应失败: %v（body=%q）", err, rr.Body.String())
	}
	return resp
}

// mustSessionFor 创建指定角色的会话（绕过登录，供受保护路由测试使用）。
func (e *authTestEnv) mustSessionFor(userID int64, role string) string {
	e.t.Helper()
	sid, err := e.sessions.Create(context.Background(), userID, auth.SessionOpts{
		UA: "AuthTest/1.0", IP: "192.0.2.10", Role: role,
	})
	if err != nil {
		e.t.Fatalf("Create session: %v", err)
	}
	return sid
}

// ---------------------------------------------------------------------------
// GET /api/status
// ---------------------------------------------------------------------------

func TestAuthStatusEndpoint(t *testing.T) {
	t.Run("默认值", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		rr := env.request(http.MethodGet, "/api/status", nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q，期望 application/json", ct)
		}
		var got statusResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("status JSON 非法: %v（body=%q）", err, rr.Body.String())
		}
		if !got.Auth.SignupEnabled || !got.Auth.PasswordLoginEnabled || got.Auth.EmailVerificationRequired {
			t.Errorf("默认 auth 开关不对: %+v", got.Auth)
		}
		if got.Auth.Turnstile.Enabled || got.Auth.Turnstile.SiteKey != "" {
			t.Errorf("默认 Turnstile 应为关闭: %+v", got.Auth.Turnstile)
		}
		if got.PasswordLoginEncryptionEnabled {
			t.Error("password_login_encryption_enabled 默认应为 false")
		}
		wantLangs := []string{"en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"}
		if strings.Join(got.Languages, ",") != strings.Join(wantLangs, ",") {
			t.Errorf("languages = %v，期望 canonical 7 语 %v", got.Languages, wantLangs)
		}
		if got.DefaultLanguage != "en" {
			t.Errorf("default_language = %q，期望 en", got.DefaultLanguage)
		}
	})

	t.Run("配置覆盖", func(t *testing.T) {
		cfg := mapConfig{
			"auth.signup_enabled":                    configBool(false),
			"auth.password_login_enabled":            configBool(false),
			"auth.email_verification_required":       configBool(true),
			"auth.turnstile.enabled":                 configBool(true),
			"auth.turnstile.site_key":                configString("turnstile-site-key"),
			"auth.login_password_encryption.enabled": configBool(true),
		}
		env := newAuthTestEnv(t, cfg, authTestOptions{})
		rr := env.request(http.MethodGet, "/api/status", nil, nil)
		var got statusResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("status JSON 非法: %v", err)
		}
		if got.Auth.SignupEnabled || got.Auth.PasswordLoginEnabled {
			t.Errorf("开关未按配置下发: %+v", got.Auth)
		}
		if !got.Auth.EmailVerificationRequired {
			t.Error("email_verification_required 未按配置下发")
		}
		if !got.Auth.Turnstile.Enabled || got.Auth.Turnstile.SiteKey != "turnstile-site-key" {
			t.Errorf("turnstile 未按配置下发: %+v", got.Auth.Turnstile)
		}
		if !got.PasswordLoginEncryptionEnabled {
			t.Error("password_login_encryption_enabled 未按配置下发")
		}
		if strings.Contains(rr.Body.String(), "secret_key") {
			t.Fatal("status 不得下发 secret_key（modules-08 §1.4.1 不公开清单）")
		}
	})
}

// ---------------------------------------------------------------------------
// 注册
// ---------------------------------------------------------------------------

// TestAuthRegisterAutoLogin 覆盖 brief (a) TC-F-01-01-01。
func TestAuthRegisterAutoLogin(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	rr := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("alice", "Alice@Example.com", "N7x!qPz2wR", ""), nil)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	sess := cookieByName(rr, auth.SessionCookieName)
	if sess == nil {
		t.Fatal("注册成功必须下发会话 Cookie")
	}
	if !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteLaxMode || sess.Path != "/" {
		t.Errorf("会话 Cookie 属性不符: %+v（需 HttpOnly+Secure+Lax+Path=/）", sess)
	}
	csrf := cookieByName(rr, auth.CSRFCookieName)
	if csrf == nil {
		t.Fatal("注册成功必须下发 CSRF Cookie")
	}
	if csrf.HttpOnly {
		t.Error("CSRF Cookie 必须非 HttpOnly（前端需读取）")
	}
	if !csrf.Secure || csrf.SameSite != http.SameSiteLaxMode || csrf.Path != "/" {
		t.Errorf("CSRF Cookie 属性不符: %+v", csrf)
	}
	if got := env.mr.HGet("sess:"+sess.Value, "user_id"); got != "1" {
		t.Errorf("Redis 会话 user_id = %q，期望 1", got)
	}
	if got := env.mr.HGet("sess:"+sess.Value, "auth_level"); got != rbac.RoleUser {
		t.Errorf("auth_level = %q，期望 user（来自 DB 行）", got)
	}

	var resp authSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("注册响应 JSON 非法: %v", err)
	}
	if resp.User.ID != 1 || resp.User.Username != "alice" || resp.User.Role != rbac.RoleUser {
		t.Errorf("注册返回用户 = %+v", resp.User)
	}
	if strings.Contains(rr.Body.String(), "password_hash") || strings.Contains(rr.Body.String(), "argon2") ||
		strings.Contains(rr.Body.String(), "N7x!qPz2wR") {
		t.Fatal("响应不得包含口令哈希或明文（GC-6）")
	}

	// 注册即登录：返回的 Cookie 能通过受保护链路。
	var seen *auth.Principal
	env.router.HandleProtected("GET /api/whoami", captureHandler(&seen))
	protected := env.request(http.MethodGet, "/api/whoami", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Value})
	})
	if protected.Code != http.StatusNoContent {
		t.Fatalf("注册会话访问受保护路由 status = %d，期望 204", protected.Code)
	}
	if seen == nil || seen.UserID != 1 {
		t.Fatalf("Principal = %+v，期望 UserID=1", seen)
	}
}

// TestAuthRegisterDuplicateUniform 覆盖 brief (b) TC-F-01-01-02：
// 重名与重邮箱必须返回逐字相同的 409，不得暴露是哪一项冲突。
func TestAuthRegisterDuplicateUniform(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", Email: "alice@example.com"})

	byName := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("alice", "other@example.com", "N7x!qPz2wR", ""), nil)
	byMail := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("other", "alice@example.com", "N7x!qPz2wR", ""), nil)

	if byName.Code != http.StatusConflict || byMail.Code != http.StatusConflict {
		t.Fatalf("重复注册 status = %d/%d，期望 409/409（body=%q / %q）",
			byName.Code, byMail.Code, byName.Body.String(), byMail.Body.String())
	}
	if byName.Body.String() != byMail.Body.String() {
		t.Fatalf("重名与重邮箱文案不一致，构成字段级枚举信道：\nname=%q\nmail=%q",
			byName.Body.String(), byMail.Body.String())
	}
	if p := decodeProblem(t, byName); p.Type != authProblemConflict {
		t.Errorf("problem.type = %q，期望 %q", p.Type, authProblemConflict)
	}
	if strings.Contains(byName.Body.String(), "fields") {
		t.Error("409 不得携带字段级扩展（会泄露冲突字段）")
	}

	// 竞态路径：预检查未命中但 INSERT 撞唯一键，也必须落同一 409。
	raceRepo := newFakeAuthRepo()
	raceRepo.forceCreateEr = ErrDuplicateUser
	raceEnv := newAuthTestEnv(t, nil, authTestOptions{repo: raceRepo})
	race := raceEnv.request(http.MethodPost, "/api/auth/register",
		registerPayload("racer", "racer@example.com", "N7x!qPz2wR", ""), nil)
	if race.Code != http.StatusConflict || race.Body.String() != byName.Body.String() {
		t.Fatalf("竞态重复注册 = %d %q，期望与预检查同形 409 %q",
			race.Code, race.Body.String(), byName.Body.String())
	}
}

// TestAuthRegisterPasswordPolicy 覆盖 brief (c) TC-F-01-01-03：
// 纯长度策略（Unicode code point 计数），不限制字符组成。
func TestAuthRegisterPasswordPolicy(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
		want     int
	}{
		{"7 位被拒", "weak", "1234567", http.StatusBadRequest},
		{"129 位被拒", "toolong", strings.Repeat("a", 129), http.StatusBadRequest},
		{"纯数字 8 位被接受", "numeric", "84736290", http.StatusCreated},
		{"常见弱口令被拒", "common", "password1", http.StatusBadRequest},
		{"包含用户名的口令被拒", "alice", "alice-is-my-pass", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newAuthTestEnv(t, nil, authTestOptions{})
			rr := env.request(http.MethodPost, "/api/auth/register",
				registerPayload(tc.username, tc.username+"@example.com", tc.password, ""), nil)
			if rr.Code != tc.want {
				t.Fatalf("status = %d，期望 %d（body=%q）", rr.Code, tc.want, rr.Body.String())
			}
		})
	}

	t.Run("confirmPassword 不一致", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		body := registerPayload("bob", "bob@example.com", "N7x!qPz2wR", "")
		body["confirmPassword"] = "N7x!qPz2wX"
		rr := env.request(http.MethodPost, "/api/auth/register", body, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
		}
	})

	t.Run("未同意条款", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		body := registerPayload("carol", "carol@example.com", "N7x!qPz2wR", "")
		body["agree_terms"] = false
		rr := env.request(http.MethodPost, "/api/auth/register", body, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
		}
	})

	t.Run("emoji 按 code point 计数", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		// 7 个 emoji = 7 个 rune（14 个 UTF-16 code unit），必须按 7 拒绝。
		weak := strings.Repeat("😀", 7)
		rr := env.request(http.MethodPost, "/api/auth/register",
			registerPayload("emoji", "emoji@example.com", weak, ""), nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("7 个 emoji 的 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
		}
	})
}

func TestAuthRegisterSignupDisabled(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{"auth.signup_enabled": configBool(false)}, authTestOptions{})
	rr := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("alice", "alice@example.com", "N7x!qPz2wR", ""), nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.created) != 0 {
		t.Fatal("注册关闭时不得写入用户行")
	}
}

func TestAuthRegisterEmailVerification(t *testing.T) {
	cfg := mapConfig{"auth.email_verification_required": configBool(true)}
	env := newAuthTestEnv(t, cfg, authTestOptions{})

	without := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("alice", "alice@example.com", "N7x!qPz2wR", ""), nil)
	if without.Code != http.StatusBadRequest {
		t.Fatalf("缺验证码注册 status = %d，期望 400（body=%q）", without.Code, without.Body.String())
	}

	if rr := env.request(http.MethodPost, "/api/auth/email-code",
		map[string]any{"email": "alice@example.com", "purpose": "register"}, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("发送验证码 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	code := env.sender.last(t).Code
	if len(code) != 6 {
		t.Fatalf("验证码 = %q，期望 6 位数字", code)
	}

	ok := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("alice", "alice@example.com", "N7x!qPz2wR", code), nil)
	if ok.Code != http.StatusCreated {
		t.Fatalf("带验证码注册 status = %d，期望 201（body=%q）", ok.Code, ok.Body.String())
	}
	resp := decodeAuthUser(t, ok)
	if !resp.User.EmailVerified {
		t.Error("验证码通过后 email_verified 应为 true")
	}
	if len(env.repo.created) != 1 || !env.repo.created[0].EmailVerified {
		t.Fatalf("CreateUser 入参未携带已验证标记: %+v", env.repo.created)
	}

	// 验证码一次性：同一码第二次使用必须失败（换个用户名避开 409）。
	reuse := env.request(http.MethodPost, "/api/auth/register",
		registerPayload("bob", "alice@example.com", "N7x!qPz2wR", code), nil)
	if reuse.Code != http.StatusConflict && reuse.Code != http.StatusBadRequest {
		t.Fatalf("复用验证码 status = %d，期望 409/400（body=%q）", reuse.Code, reuse.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 邮箱验证码
// ---------------------------------------------------------------------------

func TestAuthEmailCodeRateLimits(t *testing.T) {
	t.Run("同邮箱 60s 一次", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		first := env.request(http.MethodPost, "/api/auth/email-code",
			map[string]any{"email": "alice@example.com", "purpose": "register"}, nil)
		if first.Code != http.StatusNoContent {
			t.Fatalf("首次发送 status = %d，期望 204（body=%q）", first.Code, first.Body.String())
		}
		second := env.request(http.MethodPost, "/api/auth/email-code",
			map[string]any{"email": "alice@example.com", "purpose": "register"}, nil)
		if second.Code != http.StatusTooManyRequests {
			t.Fatalf("60s 内重发 status = %d，期望 429（body=%q）", second.Code, second.Body.String())
		}
		retry, err := strconv.Atoi(second.Header().Get("Retry-After"))
		if err != nil || retry <= 0 || retry > 60 {
			t.Errorf("Retry-After = %q，期望 1..60 秒", second.Header().Get("Retry-After"))
		}
		if env.sender.count() != 1 {
			t.Errorf("发送次数 = %d，期望仅 1 次", env.sender.count())
		}
	})

	t.Run("同 IP 10 次每小时", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		for i := 0; i < 10; i++ {
			rr := env.request(http.MethodPost, "/api/auth/email-code",
				map[string]any{"email": "user" + strconv.Itoa(i) + "@example.com", "purpose": "reset"}, nil)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("第 %d 次发送 status = %d，期望 204（body=%q）", i+1, rr.Code, rr.Body.String())
			}
		}
		blocked := env.request(http.MethodPost, "/api/auth/email-code",
			map[string]any{"email": "user10@example.com", "purpose": "reset"}, nil)
		if blocked.Code != http.StatusTooManyRequests {
			t.Fatalf("第 11 次发送 status = %d，期望 429（body=%q）", blocked.Code, blocked.Body.String())
		}
		if blocked.Header().Get("Retry-After") == "" {
			t.Error("429 必须携带 Retry-After")
		}
	})

	t.Run("验证码只存哈希且不落明文", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		if rr := env.request(http.MethodPost, "/api/auth/email-code",
			map[string]any{"email": "hash@example.com", "purpose": "register"}, nil); rr.Code != http.StatusNoContent {
			t.Fatalf("发送 status = %d", rr.Code)
		}
		code := env.sender.last(t).Code
		stored, _ := env.mr.Get("email_code:register:hash@example.com")
		if stored == "" {
			t.Fatal("Redis 中不存在验证码键")
		}
		if stored == code {
			t.Fatal("Redis 中不得存验证码明文")
		}
		if len(stored) != 64 {
			t.Fatalf("验证码哈希长度 = %d，期望 64 位十六进制", len(stored))
		}
		if ttl := env.mr.TTL("email_code:register:hash@example.com"); ttl <= 0 || ttl > 10*time.Minute {
			t.Errorf("验证码 TTL = %v，期望 (0, 10m]", ttl)
		}
	})

	t.Run("非法 purpose/邮箱", func(t *testing.T) {
		env := newAuthTestEnv(t, nil, authTestOptions{})
		for _, body := range []map[string]any{
			{"email": "alice@example.com", "purpose": "nonsense"},
			{"email": "not-an-email", "purpose": "register"},
			{"email": "", "purpose": "register"},
		} {
			if rr := env.request(http.MethodPost, "/api/auth/email-code", body, nil); rr.Code != http.StatusBadRequest {
				t.Errorf("%v status = %d，期望 400", body, rr.Code)
			}
		}
	})
}

func TestEmailCodeStoreOneTimeAndAttempts(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	store := env.router.handlers.codes
	ctx := context.Background()

	code, err := store.issue(ctx, "alice@example.com", "register")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if ok, err := store.consume(ctx, "alice@example.com", "register", code); err != nil || !ok {
		t.Fatalf("首次使用 = %v/%v，期望 true/nil", ok, err)
	}
	if ok, _ := store.consume(ctx, "alice@example.com", "register", code); ok {
		t.Fatal("验证码必须一次性使用")
	}

	code, err = store.issue(ctx, "alice@example.com", "reset")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	for i := 0; i < 3; i++ {
		if ok, _ := store.consume(ctx, "alice@example.com", "reset", "000000"); ok {
			t.Fatal("错误验证码不得通过")
		}
	}
	if ok, _ := store.consume(ctx, "alice@example.com", "reset", code); ok {
		t.Fatal("连续 3 次错误后验证码必须作废（TC-F-01-01-04）")
	}
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

// TestAuthLoginSuccessRoleFromDB 覆盖 ruling 3：会话角色只能来自 DB 行。
func TestAuthLoginSuccessRoleFromDB(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	stored := env.repo.add(UserRecord{
		Username: "admin", Email: "admin@example.com",
		Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})

	rr := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "admin",
		"password":   authTestPassword,
		"role":       rbac.RoleUser, // 客户端伪造角色必须被忽略
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	sess := cookieByName(rr, auth.SessionCookieName)
	if sess == nil {
		t.Fatal("登录成功必须下发会话 Cookie")
	}
	if got := env.mr.HGet("sess:"+sess.Value, "auth_level"); got != rbac.RoleAdmin {
		t.Fatalf("auth_level = %q，期望来自 DB 的 admin", got)
	}

	var seen *auth.Principal
	env.router.HandleProtected("GET /api/whoami", captureHandler(&seen))
	protected := env.request(http.MethodGet, "/api/whoami", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Value})
	})
	if protected.Code != http.StatusNoContent {
		t.Fatalf("受保护路由 status = %d，期望 204", protected.Code)
	}
	if seen == nil || seen.Role != rbac.RoleAdmin || seen.UserID != stored.ID {
		t.Fatalf("Principal = %+v，期望 DB 角色 admin / userID %d", seen, stored.ID)
	}
	if strings.Contains(rr.Body.String(), "hash") || strings.Contains(rr.Body.String(), authTestPassword) {
		t.Fatal("登录响应不得包含口令或哈希")
	}

	// 邮箱也可作为 identifier（大小写不敏感）。
	byEmail := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "ADMIN@example.com",
		"password":   authTestPassword,
	}, nil)
	if byEmail.Code != http.StatusOK {
		t.Fatalf("邮箱登录 status = %d，期望 200（body=%q）", byEmail.Code, byEmail.Body.String())
	}
}

// TestAuthLoginBadCredentialsUniformAndDummyVerify 覆盖 brief (f) TC-F-01-03-05：
// 不存在用户与错误口令返回同一文案，且未知用户执行 dummy verify（防时序枚举）。
func TestAuthLoginBadCredentialsUniformAndDummyVerify(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	alice := env.repo.add(UserRecord{
		Username: "alice", Email: "alice@example.com", PasswordHash: authTestPasswordHash(),
	})

	var mu sync.Mutex
	type call struct{ hash, plain string }
	var calls []call
	env.router.handlers.verifyPassword = func(hash, plain string) bool {
		mu.Lock()
		calls = append(calls, call{hash: hash, plain: plain})
		mu.Unlock()
		return false
	}

	wrong := env.login("alice", "WrongPassword1!")
	unknown := env.login("nobody", "WrongPassword1!")
	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d/%d，期望 401/401", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Fatalf("错误口令与未知用户文案不同，构成用户枚举信道：\nwrong=%q\nunknown=%q",
			wrong.Body.String(), unknown.Body.String())
	}
	if cookieByName(wrong, auth.SessionCookieName) != nil || cookieByName(unknown, auth.SessionCookieName) != nil {
		t.Fatal("登录失败不得下发会话 Cookie")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("VerifyPassword 调用次数 = %d，期望 2（含未知用户 dummy verify）", len(calls))
	}
	if calls[0].hash != alice.PasswordHash {
		t.Errorf("已存在用户校验未使用 DB 哈希")
	}
	if calls[1].hash == "" {
		t.Fatal("未知用户必须执行 dummy verify（空哈希会提前返回，破坏时序一致性）")
	}
	if calls[1].hash == alice.PasswordHash {
		t.Error("dummy verify 不应复用真实用户哈希")
	}
}

// TestAuthLoginBannedUser 覆盖 brief (e) TC-F-01-03-04：封禁用户 401。
func TestAuthLoginBannedUser(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{
		Username: "banned", Email: "banned@example.com",
		Status: "disabled", PasswordHash: authTestPasswordHash(),
	})

	rr := env.login("banned", authTestPassword)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("封禁用户 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}
	if p := decodeProblem(t, rr); p.Type != authProblemAccountDisabled {
		t.Errorf("problem.type = %q，期望 %q", p.Type, authProblemAccountDisabled)
	}
	if cookieByName(rr, auth.SessionCookieName) != nil {
		t.Fatal("封禁用户不得获得会话")
	}

	// 错误口令不得暴露封禁状态（先验口令，再判状态）。
	wrong := env.login("banned", "WrongPassword1!")
	if p := decodeProblem(t, wrong); p.Type != problemTypeUnauthorized {
		t.Errorf("错误口令 + 封禁账号 type = %q，期望统一 unauthorized", p.Type)
	}
}

// TestAuthLoginLocksAfterFiveFailures 覆盖 brief (d) TC-F-01-03-03。
func TestAuthLoginLocksAfterFiveFailures(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})
	env.router.handlers.verifyPassword = func(_, plain string) bool { return plain == authTestPassword }

	for i := 0; i < LoginMaxFailures; i++ {
		rr := env.login("alice", "WrongPassword1!")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败 status = %d，期望 401（body=%q）", i+1, rr.Code, rr.Body.String())
		}
	}

	locked := env.login("alice", authTestPassword)
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次（正确口令）status = %d，期望 429 锁定（body=%q）", locked.Code, locked.Body.String())
	}
	if p := decodeProblem(t, locked); p.Type != authProblemTooManyRequests {
		t.Errorf("锁定 problem.type = %q，期望 %q", p.Type, authProblemTooManyRequests)
	}
	retry, err := strconv.Atoi(locked.Header().Get("Retry-After"))
	if err != nil || retry <= 0 || retry > int(LoginLockDuration.Seconds()) {
		t.Errorf("Retry-After = %q，期望 1..%v 秒", locked.Header().Get("Retry-After"), LoginLockDuration.Seconds())
	}
	keys := env.router.handlers.limiter.buckets("192.0.2.1", "alice")
	if len(keys) != 2 {
		t.Fatalf("已知 identifier 的桶数 = %d，期望双桶", len(keys))
	}
	for _, key := range keys {
		if ttl := env.mr.TTL(key); ttl <= 0 || ttl > LoginLockDuration {
			t.Errorf("桶 %s TTL = %v，期望 (0, %v]", key, ttl, LoginLockDuration)
		}
	}

	// 15 分钟后解锁。
	env.mr.FastForward(LoginLockDuration + time.Second)
	if rr := env.login("alice", authTestPassword); rr.Code != http.StatusOK {
		t.Fatalf("锁定到期后 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}

	// 成功登录必须清零计数（连续失败语义）。
	env2 := newAuthTestEnv(t, nil, authTestOptions{})
	env2.repo.add(UserRecord{Username: "bob", PasswordHash: authTestPasswordHash()})
	env2.router.handlers.verifyPassword = func(_, plain string) bool { return plain == authTestPassword }
	for i := 0; i < LoginMaxFailures-1; i++ {
		env2.login("bob", "WrongPassword1!")
	}
	if rr := env2.login("bob", authTestPassword); rr.Code != http.StatusOK {
		t.Fatalf("4 次失败后正确登录 status = %d，期望 200", rr.Code)
	}
	for i := 0; i < LoginMaxFailures-1; i++ {
		env2.login("bob", "WrongPassword1!")
	}
	if rr := env2.login("bob", authTestPassword); rr.Code != http.StatusOK {
		t.Fatalf("成功登录未清零计数，后续 4 次失败即被锁: status = %d", rr.Code)
	}
}

func TestAuthLoginLockIsPerIdentifierNotPerIP(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})
	env.repo.add(UserRecord{Username: "bob", PasswordHash: authTestPasswordHash()})
	env.router.handlers.verifyPassword = func(_, plain string) bool { return plain == authTestPassword }

	for i := 0; i < LoginMaxFailures; i++ {
		env.login("alice", "WrongPassword1!")
	}
	if rr := env.login("alice", authTestPassword); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("alice status = %d，期望 429", rr.Code)
	}
	if rr := env.login("bob", authTestPassword); rr.Code != http.StatusOK {
		t.Fatalf("bob（同 IP 不同账号）status = %d，期望 200：锁定必须按 identifier 隔离", rr.Code)
	}
}

func TestAuthLoginUnknownIdentifierCountsOnlyIP(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.router.handlers.verifyPassword = func(string, string) bool { return false }

	for i := 0; i < LoginMaxFailures; i++ {
		rr := env.login("ghost"+strconv.Itoa(i), "Whatever1!")
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("未知用户第 %d 次 status = %d，期望 401", i+1, rr.Code)
		}
	}
	locked := env.login("ghostX", "Whatever1!")
	if locked.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 个未知 identifier status = %d，期望按 IP 桶 429（body=%q）",
			locked.Code, locked.Body.String())
	}
	// 未知 identifier 不得写入 identifier-only 桶（否则攻击者可恶意锁定同名未来账号）。
	if env.mr.Exists("login:fail:id:" + loginIdentifierHash("ghost0")) {
		t.Fatal("未知 identifier 不得产生 identifier-only 桶")
	}
}

// TestAuthLoginTwoFactorPending：TOTP 用户登录只落 pending_2fa 票据，不建会话。
func TestAuthLoginTwoFactorPending(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{
		Username: "totp", PasswordHash: authTestPasswordHash(), TOTPEnabled: true,
	})

	rr := env.login("totp", authTestPassword)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("2FA 响应 JSON 非法: %v", err)
	}
	if resp.Status != "pending_2fa" || resp.Ticket == "" {
		t.Fatalf("2FA 响应 = %+v，期望 status=pending_2fa 且 ticket 非空", resp)
	}
	if cookieByName(rr, auth.SessionCookieName) != nil {
		t.Fatal("2FA 挑战前不得建立会话")
	}
	ttl := env.mr.TTL("pending_2fa:" + resp.Ticket)
	if ttl <= 0 || ttl > 5*time.Minute {
		t.Errorf("pending_2fa TTL = %v，期望 (0, 5m]", ttl)
	}
}

func TestAuthLoginPasswordDisabled(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{"auth.password_login_enabled": configBool(false)}, authTestOptions{})
	rr := env.login("alice", "Whatever1!")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestAuthLoginLimiterFailClosed(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})
	env.mr.Close()

	rr := env.login("alice", authTestPassword)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis 不可用时 login status = %d，期望 503 fail-closed（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("fail-closed 503 必须携带有界 Retry-After（security-07 §2.3）")
	}
	if cookieByName(rr, auth.SessionCookieName) != nil {
		t.Fatal("限流器故障时不得建立会话")
	}

	codeRR := env.request(http.MethodPost, "/api/auth/email-code",
		map[string]any{"email": "alice@example.com", "purpose": "register"}, nil)
	if codeRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis 不可用时 email-code status = %d，期望 503", codeRR.Code)
	}
}

// ---------------------------------------------------------------------------
// 登出
// ---------------------------------------------------------------------------

// TestAuthLogoutRevokesAndRepeatable 覆盖 brief (g)(h) TC-F-01-04-01/02。
func TestAuthLogoutRevokesAndRepeatable(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})
	login := env.login("alice", authTestPassword)
	if login.Code != http.StatusOK {
		t.Fatalf("登录 status = %d", login.Code)
	}
	sess := cookieByName(login, auth.SessionCookieName)
	if sess == nil {
		t.Fatal("登录未下发会话 Cookie")
	}

	var seen *auth.Principal
	env.router.HandleProtected("GET /api/whoami", captureHandler(&seen))
	withCookie := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Value})
	}
	if rr := env.request(http.MethodGet, "/api/whoami", nil, withCookie); rr.Code != http.StatusNoContent {
		t.Fatalf("登出前受保护路由 status = %d，期望 204", rr.Code)
	}

	logout := env.request(http.MethodPost, "/api/logout", nil, withCookie)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("登出 status = %d，期望 204（body=%q）", logout.Code, logout.Body.String())
	}
	for _, name := range []string{auth.SessionCookieName, auth.CSRFCookieName} {
		c := cookieByName(logout, name)
		if c == nil {
			t.Errorf("登出必须清理 Cookie %s", name)
			continue
		}
		if c.Value != "" || c.MaxAge > 0 {
			t.Errorf("登出未清除 %s: Value=%q MaxAge=%d", name, c.Value, c.MaxAge)
		}
	}

	seen = nil
	if rr := env.request(http.MethodGet, "/api/whoami", nil, withCookie); rr.Code != http.StatusUnauthorized {
		t.Fatalf("登出后原 Cookie status = %d，期望 401", rr.Code)
	}
	if seen != nil {
		t.Fatal("登出后不得注入 Principal")
	}

	// 重复登出仍为 204（幂等）。
	if rr := env.request(http.MethodPost, "/api/logout", nil, withCookie); rr.Code != http.StatusNoContent {
		t.Fatalf("重复登出 status = %d，期望 204", rr.Code)
	}
	// 无 Cookie 的登出同样 204（public-safe）。
	if rr := env.request(http.MethodPost, "/api/logout", nil, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("无 Cookie 登出 status = %d，期望 204", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 中间件装配
// ---------------------------------------------------------------------------

// TestAuthPublicEndpointsBypassCSRF：公开端点不得被 CSRF 拦截（ruling 1）。
func TestAuthPublicEndpointsBypassCSRF(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{Username: "alice", PasswordHash: authTestPasswordHash()})

	// 已登录用户再次登录：带会话 Cookie 但没有 X-CSRF-Token，必须成功。
	sid := env.mustSessionFor(1, rbac.RoleUser)
	csrfToken := auth.NewCSRFToken()
	if err := env.sessions.IssueCSRF(context.Background(), sid, csrfToken); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	login := env.request(http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": "alice", "password": authTestPassword,
	}, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	})
	if login.Code != http.StatusOK {
		t.Fatalf("公开登录端点被 CSRF 拦截: status = %d（body=%q）", login.Code, login.Body.String())
	}

	// 登出同样豁免 CSRF（authenticated but public-safe）。
	logout := env.request(http.MethodPost, "/api/logout", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	})
	if logout.Code != http.StatusNoContent {
		t.Fatalf("登出被 CSRF 拦截: status = %d（body=%q）", logout.Code, logout.Body.String())
	}

	// 对照：受保护端点的 POST 仍必须校验 CSRF。
	sid2 := env.mustSessionFor(1, rbac.RoleUser)
	env.router.HandleProtected("POST /api/protected", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	protected := env.request(http.MethodPost, "/api/protected", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid2})
	})
	if protected.Code != http.StatusForbidden {
		t.Fatalf("受保护 POST 缺 CSRF status = %d，期望 403", protected.Code)
	}
}

// TestProtectedChainOrder 冻结 SessionAuth → CSRF → OriginRefererCheck 的顺序。
func TestProtectedChainOrder(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{publicBaseURL: "https://app.example.com"})
	sid := env.mustSessionFor(1, rbac.RoleUser)
	csrfToken := auth.NewCSRFToken()
	if err := env.sessions.IssueCSRF(context.Background(), sid, csrfToken); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	env.router.HandleProtected("POST /api/protected", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name   string
		cookie bool
		csrf   string
		origin string
		want   int
	}{
		{"未认证优先于 CSRF/Origin", false, "", "https://evil.example", http.StatusUnauthorized},
		{"认证后 CSRF 先于 Origin", true, "", "https://evil.example", http.StatusForbidden},
		{"跨源 Origin 被拒", true, csrfToken, "https://evil.example", http.StatusForbidden},
		{"同源放行", true, csrfToken, "https://app.example.com", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(http.MethodPost, "/api/protected", nil, func(r *http.Request) {
				r.Host = "app.example.com"
				if tc.cookie {
					r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
				}
				if tc.csrf != "" {
					r.Header.Set(auth.CSRFHeaderName, tc.csrf)
				}
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
			})
			if rr.Code != tc.want {
				t.Fatalf("status = %d，期望 %d（body=%q）", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// TestPermissionsLoaderRestrictedAdmin：接线 Task 5 的 PermissionLoader 后，
// 受限 admin 的 Principal.Permissions 由 DB 权威值填充（不能把 nil 当 FULL）。
func TestPermissionsLoaderRestrictedAdmin(t *testing.T) {
	authz := rbac.NewAuthzMiddleware()
	loader := staticPermissions{perms: []string{rbac.PermUsersRead}}
	env := newAuthTestEnv(t, nil, authTestOptions{permissions: loader, authz: authz})

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	env.router.HandleProtected("GET /api/admin/read",
		authz.Handler(rbac.PermUsersRead, rbac.AuthzAction{ResourceKind: "user"})(ok))
	env.router.HandleProtected("PATCH /api/admin/update",
		authz.Handler(rbac.PermUsersUpdate, rbac.AuthzAction{ResourceKind: "user"})(ok))
	env.router.HandleProtected("GET /api/admin/denied-on-load",
		authz.Handler(rbac.PermUsersRead, rbac.AuthzAction{ResourceKind: "user"})(ok))

	sid := env.mustSessionFor(7, rbac.RoleAdmin)
	withCookie := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	}

	if rr := env.request(http.MethodGet, "/api/admin/read", nil, withCookie); rr.Code != http.StatusNoContent {
		t.Fatalf("受限 admin 持有 users:read 时 status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(http.MethodPatch, "/api/admin/update", nil, withCookie); rr.Code != http.StatusForbidden {
		t.Fatalf("受限 admin 缺 users:update 时 status = %d，期望 403", rr.Code)
	}

	// 装载失败必须 fail-closed（403），不得把 nil 当 FULL。
	failAuthz := rbac.NewAuthzMiddleware()
	failEnv := newAuthTestEnv(t, nil, authTestOptions{
		permissions: staticPermissions{err: errors.New("db down")},
		authz:       failAuthz,
	})
	failEnv.router.HandleProtected("GET /api/admin/read",
		failAuthz.Handler(rbac.PermUsersRead, rbac.AuthzAction{ResourceKind: "user"})(ok))
	failSid := failEnv.mustSessionFor(8, rbac.RoleAdmin)
	failRR := failEnv.request(http.MethodGet, "/api/admin/read", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: failSid})
	})
	if failRR.Code != http.StatusForbidden {
		t.Fatalf("权限装载失败 status = %d，期望 403 fail-closed", failRR.Code)
	}

	// role=user 即使故意持有 "*" 也不能通过管理动作（role 是外层门）。
	userAuthz := rbac.NewAuthzMiddleware()
	userEnv := newAuthTestEnv(t, nil, authTestOptions{
		permissions: staticPermissions{perms: []string{rbac.WildcardPermission}},
		authz:       userAuthz,
	})
	userEnv.router.HandleProtected("GET /api/admin/read",
		userAuthz.Handler(rbac.PermUsersRead, rbac.AuthzAction{ResourceKind: "user"})(ok))
	userSid := userEnv.mustSessionFor(9, rbac.RoleUser)
	userRR := userEnv.request(http.MethodGet, "/api/admin/read", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: userSid})
	})
	if userRR.Code != http.StatusForbidden {
		t.Fatalf("user 越权 status = %d，期望 403", userRR.Code)
	}
}
