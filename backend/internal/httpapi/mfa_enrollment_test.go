package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// R-04 管理员 MFA 强制绑定：登录判定、关闭拒绝、普通用户不受影响。
//
// 本文件的第一批用例只依赖既有 HTTP 契约（登录响应/2fa disable），用来先
// 把行为差异钉死；中间件与强制绑定端点见文件后半部分。

// TestAdminLoginRequiresMFAEnrollment 覆盖 R-04 第 1 条：
// role=admin && !totp_enabled && admin_required=true → 不签发会话，只发 enroll 票据。
func TestAdminLoginRequiresMFAEnrollment(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	stored := env.repo.add(UserRecord{
		Username: "admin-enroll", Email: "admin-enroll@example.com",
		Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})

	rr := env.login("admin-enroll", authTestPassword)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if cookieByName(rr, auth.SessionCookieName) != nil {
		t.Fatal("强制绑定期间不得签发完整会话")
	}

	var resp struct {
		Status string `json:"status"`
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("登录响应 JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	if resp.Status != "mfa_enrollment_required" {
		t.Fatalf("status = %q，期望 mfa_enrollment_required", resp.Status)
	}
	if resp.Ticket == "" {
		t.Fatal("mfa_enrollment_required 必须携带 ticket")
	}

	// scope=enroll：票据落在独立键空间，登录挑战（pending_2fa:）看不到它。
	raw, err := env.rdb.Get(context.Background(), "pending_2fa:enroll:"+resp.Ticket).Result()
	if err != nil {
		t.Fatalf("enroll 票据未写入 Redis: %v", err)
	}
	if raw != strconv.FormatInt(stored.ID, 10) {
		t.Fatalf("enroll 票据 user_id = %q，期望 %d", raw, stored.ID)
	}
	if ttl := env.mr.TTL("pending_2fa:enroll:" + resp.Ticket); ttl <= 0 || ttl > 10*time.Minute {
		t.Fatalf("enroll 票据 TTL = %v，期望 (0, 10m]", ttl)
	}
	if ttl := env.mr.TTL("pending_2fa:" + resp.Ticket); ttl > 0 {
		t.Fatal("enroll 票据不得复用登录挑战键空间（scope 必须隔离）")
	}
}

// TestAdminLoginEnrollmentSettingOffKeepsCurrentBehavior 覆盖 R-04「admin_required=false
// 时行为回到当前」。
func TestAdminLoginEnrollmentSettingOffKeepsCurrentBehavior(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{
		"auth.mfa.admin_required": configBool(false),
	}, authTestOptions{})
	env.repo.add(UserRecord{
		Username: "admin-off", Email: "admin-off@example.com",
		Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})

	rr := env.login("admin-off", authTestPassword)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if cookieByName(rr, auth.SessionCookieName) == nil {
		t.Fatal("admin_required=false 时必须按现状直接签发会话")
	}
}

// TestRegularUserLoginNotAffectedByAdminEnrollment：普通用户即使 admin_required=true
// 也不进入强制绑定流程。
func TestRegularUserLoginNotAffectedByAdminEnrollment(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{
		Username: "regular", Email: "regular@example.com",
		Role: rbac.RoleUser, PasswordHash: authTestPasswordHash(),
	})

	rr := env.login("regular", authTestPassword)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if cookieByName(rr, auth.SessionCookieName) == nil {
		t.Fatal("普通用户必须直接拿到会话")
	}
}

// TestAdminWithTOTPStillUsesChallenge：已绑定 TOTP 的管理员仍走原有 pending_2fa 挑战。
func TestAdminWithTOTPStillUsesChallenge(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{})
	env.repo.add(UserRecord{
		Username: "admin-totp", Email: "admin-totp@example.com",
		Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(), TOTPEnabled: true,
	})

	rr := env.login("admin-totp", authTestPassword)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("登录响应 JSON 非法: %v", err)
	}
	if resp.Status != "pending_2fa" {
		t.Fatalf("status = %q，期望 pending_2fa", resp.Status)
	}
	if cookieByName(rr, auth.SessionCookieName) != nil {
		t.Fatal("挑战前不得建立会话")
	}
}

// TestAdminDisableTwoFARejectedWhenRequired 覆盖 R-04 第 3 条：admin_required=true
// 时管理员关闭 2FA 返回 403 mfa_required_for_admin 并写审计。
func TestAdminDisableTwoFARejectedWhenRequired(t *testing.T) {
	env := newTwoFATestEnv(t)
	user := env.repo.add(TwoFAUser{
		Username: "admin-2fa", Role: rbac.RoleAdmin,
		PasswordHash: twoFATestPasswordHash(), TOTPEnabled: true,
		TOTPSecretEnc: "whatever", BackupCodes: []string{"hash"},
	})

	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable", map[string]any{
		"password": twoFATestPassword,
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("disable status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type  string `json:"type"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem JSON 非法: %v", err)
	}
	if problem.Type != "https://newtg.dev/problems/mfa_required_for_admin" {
		t.Fatalf("problem type = %q，期望 mfa_required_for_admin", problem.Type)
	}
	if problem.Title == "" {
		t.Fatal("403 响应必须有明确文案")
	}
	if got := env.repo.get(user.ID); got == nil || !got.TOTPEnabled {
		t.Fatal("被拒绝的 disable 不得改变 totp_enabled")
	}

	// 审计必须记录这次拒绝（action=auth.2fa_disable，detail 带原因）。
	found := false
	for _, args := range env.auditExec.snapshot() {
		for _, arg := range args {
			raw, err := json.Marshal(arg)
			if err != nil {
				continue
			}
			if strings.Contains(string(raw), "mfa_required_for_admin") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("禁用被拒必须写一条带 mfa_required_for_admin 的审计")
	}
}

// TestAdminDisableTwoFAAllowedWhenSettingOff：admin_required=false 时管理员仍可关闭。
func TestAdminDisableTwoFAAllowedWhenSettingOff(t *testing.T) {
	env := newTwoFATestEnvWithConfig(t, mapConfig{
		"auth.mfa.admin_required": configBool(false),
	})
	user := env.repo.add(TwoFAUser{
		Username: "admin-2fa-off", Role: rbac.RoleAdmin,
		PasswordHash: twoFATestPasswordHash(), TOTPEnabled: true,
		TOTPSecretEnc: "whatever", BackupCodes: []string{"hash"},
	})

	rr := env.authedRequest(user.ID, http.MethodPost, "/api/2fa/disable", map[string]any{
		"password": twoFATestPassword,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.get(user.ID); got == nil || got.TOTPEnabled {
		t.Fatal("admin_required=false 时 disable 应生效")
	}
}

// ---------------------------------------------------------------------------
// 中间件兜底：RequireMFAEnrolled
// ---------------------------------------------------------------------------

func newMFALookup(enrolled bool, err error) func(context.Context, int64) (bool, error) {
	return func(context.Context, int64) (bool, error) { return enrolled, err }
}

// authedAdminRequest 建一个带 CSRF/Origin 的受保护请求。
func authedAdminRequest(t *testing.T, env *authTestEnv, sid, csrf, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return env.request(method, path, nil, func(r *http.Request) {
		r.Header.Set(auth.CSRFHeaderName, csrf)
		r.Header.Set("Origin", "http://"+r.Host)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
	})
}

func newAdminSession(t *testing.T, env *authTestEnv, userID int64) (sid, csrf string) {
	t.Helper()
	sid = env.mustSessionFor(userID, rbac.RoleAdmin)
	csrf = auth.NewCSRFToken()
	if err := env.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

func okProtected(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// newRoleSession 按指定角色签发会话（普通用户场景不能复用 newAdminSession）。
func newRoleSession(t *testing.T, env *authTestEnv, userID int64, role string) (sid, csrf string) {
	t.Helper()
	sid = env.mustSessionFor(userID, role)
	csrf = auth.NewCSRFToken()
	if err := env.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

// TestRequireMFAEnrolledAdminBlockedUnlessWhitelisted 覆盖 R-04 第 2 条：
// 未绑定管理员只能访问白名单，其余管理 API 403 + 明确错误码。
func TestRequireMFAEnrolledAdminBlockedUnlessWhitelisted(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{mfa: &MFAEnrollmentPolicy{
		Lookup: newMFALookup(false, nil),
	}})
	admin := env.repo.add(UserRecord{
		Username: "mfa-admin", Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})

	for _, pattern := range []string{
		"GET /api/whoami", "GET /api/users", "GET /api/me", "GET /api/self",
		"POST /api/2fa/setup", "POST /api/2fa/enable", "POST /api/auth/logout",
	} {
		env.router.HandleProtected(pattern, http.HandlerFunc(okProtected))
	}

	sid, csrf := newAdminSession(t, env, admin.ID)

	blocked := authedAdminRequest(t, env, sid, csrf, http.MethodGet, "/api/users")
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("管理 API status = %d，期望 403（body=%q）", blocked.Code, blocked.Body.String())
	}
	var problem struct {
		Type  string `json:"type"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(blocked.Body.Bytes(), &problem); err != nil {
		t.Fatalf("problem JSON 非法: %v", err)
	}
	if problem.Type != "https://newtg.dev/problems/mfa_enrollment_required" {
		t.Fatalf("problem type = %q", problem.Type)
	}
	if problem.Title == "" {
		t.Fatal("403 必须有明确文案")
	}

	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/me"},
		{http.MethodGet, "/api/self"},
		{http.MethodPost, "/api/2fa/setup"},
		{http.MethodPost, "/api/2fa/enable"},
		{http.MethodPost, "/api/auth/logout"},
	}
	for _, req := range allowed {
		if rr := authedAdminRequest(t, env, sid, csrf, req.method, req.path); rr.Code != http.StatusNoContent {
			t.Fatalf("%s %s status = %d，期望放行（body=%q）", req.method, req.path, rr.Code, rr.Body.String())
		}
	}
	// /api/status 是公开端点，不经过本中间件也必须 200。
	if rr := env.request(http.MethodGet, "/api/status", nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("/api/status = %d，期望 200", rr.Code)
	}
}

// TestRequireMFAEnrolledAllowsEnrolledAdmin：已绑定管理员不受影响。
func TestRequireMFAEnrolledAllowsEnrolledAdmin(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{mfa: &MFAEnrollmentPolicy{
		Lookup: newMFALookup(true, nil),
	}})
	admin := env.repo.add(UserRecord{
		Username: "mfa-admin-ok", Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})
	env.router.HandleProtected("GET /api/users", http.HandlerFunc(okProtected))
	sid, csrf := newAdminSession(t, env, admin.ID)

	if rr := authedAdminRequest(t, env, sid, csrf, http.MethodGet, "/api/users"); rr.Code != http.StatusNoContent {
		t.Fatalf("已绑定管理员 status = %d，期望放行", rr.Code)
	}
}

// TestRequireMFAEnrolledIgnoresRegularUsers：普通用户完全不受影响。
func TestRequireMFAEnrolledIgnoresRegularUsers(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{mfa: &MFAEnrollmentPolicy{
		Lookup: newMFALookup(false, nil),
	}})
	user := env.repo.add(UserRecord{
		Username: "mfa-user", Role: rbac.RoleUser, PasswordHash: authTestPasswordHash(),
	})
	env.router.HandleProtected("GET /api/whoami", http.HandlerFunc(okProtected))
	sid, csrf := newRoleSession(t, env, user.ID, rbac.RoleUser)

	if rr := authedAdminRequest(t, env, sid, csrf, http.MethodGet, "/api/whoami"); rr.Code != http.StatusNoContent {
		t.Fatalf("普通用户 status = %d，期望放行", rr.Code)
	}
}

// TestRequireMFAEnrolledSettingOff：admin_required=false 时与当前行为一致。
func TestRequireMFAEnrolledSettingOff(t *testing.T) {
	env := newAuthTestEnv(t, mapConfig{
		"auth.mfa.admin_required": configBool(false),
	}, authTestOptions{mfa: &MFAEnrollmentPolicy{
		Config: mapConfig{"auth.mfa.admin_required": configBool(false)},
		Lookup: newMFALookup(false, nil),
	}})
	admin := env.repo.add(UserRecord{
		Username: "mfa-admin-off", Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})
	env.router.HandleProtected("GET /api/users", http.HandlerFunc(okProtected))
	sid, csrf := newAdminSession(t, env, admin.ID)

	if rr := authedAdminRequest(t, env, sid, csrf, http.MethodGet, "/api/users"); rr.Code != http.StatusNoContent {
		t.Fatalf("admin_required=false status = %d，期望放行", rr.Code)
	}
}

// TestRequireMFAEnrolledLookupFailureFailsClosed：无法确认绑定状态时不得放行。
func TestRequireMFAEnrolledLookupFailureFailsClosed(t *testing.T) {
	env := newAuthTestEnv(t, nil, authTestOptions{mfa: &MFAEnrollmentPolicy{
		Lookup: newMFALookup(false, errors.New("db down")),
	}})
	admin := env.repo.add(UserRecord{
		Username: "mfa-admin-err", Role: rbac.RoleAdmin, PasswordHash: authTestPasswordHash(),
	})
	env.router.HandleProtected("GET /api/users", http.HandlerFunc(okProtected))
	sid, csrf := newAdminSession(t, env, admin.ID)

	if rr := authedAdminRequest(t, env, sid, csrf, http.MethodGet, "/api/users"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("查询失败 status = %d，期望 503", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 强制绑定端点（scope=enroll）
// ---------------------------------------------------------------------------

type enrollTestEnv struct {
	t        *testing.T
	router   http.Handler
	repo     *fakeTwoFARepo
	rdb      *redis.Client
	mr       *miniredis.Miniredis
	sessions *auth.SessionManager
}

func newEnrollTestEnv(t *testing.T) *enrollTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	repo := newFakeTwoFARepo()

	h, err := NewTwoFAHandlers(TwoFAConfig{
		Repo:          repo,
		Redis:         rdb,
		Sessions:      sessions,
		EncryptionKey: twoFATestEncryptionKey,
		Audit:         audit.New(&fakeTwoFAAuditExec{}),
		Now:           func() time.Time { return twoFATestNow },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewTwoFAHandlers: %v", err)
	}
	protected := http.NewServeMux()
	root := http.NewServeMux()
	Register2FARoutes(root, protected, h)
	root.Handle("/api/", Protect(sessions, nil, "")(protected))

	return &enrollTestEnv{
		t: t, router: root, repo: repo, rdb: rdb, mr: mr, sessions: sessions,
	}
}

func (e *enrollTestEnv) request(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return serve(e.router, req)
}

// enrollTicket 按 R-04 契约直接写 Redis（ticket → user_id，10 分钟）。
func (e *enrollTestEnv) enrollTicket(userID int64) string {
	e.t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		e.t.Fatalf("rand: %v", err)
	}
	ticket := base64.RawURLEncoding.EncodeToString(buf)
	if err := e.rdb.Set(context.Background(), twoFAEnrollTicketKeyPrefix+ticket,
		strconv.FormatInt(userID, 10), pending2FAEnrollTTL).Err(); err != nil {
		e.t.Fatalf("store ticket: %v", err)
	}
	return ticket
}

func (e *enrollTestEnv) addAdmin(username string) *TwoFAUser {
	return e.repo.add(TwoFAUser{
		Username: username, Role: rbac.RoleAdmin,
		PasswordHash: twoFATestPasswordHash(), Status: "enabled",
	})
}

func (e *enrollTestEnv) setup(ticket string) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/auth/2fa/enroll/setup", map[string]any{"ticket": ticket})
}

func (e *enrollTestEnv) enable(ticket, secret, code string) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/auth/2fa/enroll/enable", map[string]any{
		"ticket": ticket, "secret": secret, "code": code,
	})
}

func (e *enrollTestEnv) confirm(ticket string, confirmed bool) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/auth/2fa/enroll/confirm", map[string]any{
		"ticket": ticket, "backup_codes_confirmed": confirmed,
	})
}

// TestEnrollSetupRejectsMissingOrUnknownTicket：绑定端点只信任 enroll ticket。
func TestEnrollSetupRejectsMissingOrUnknownTicket(t *testing.T) {
	env := newEnrollTestEnv(t)
	for _, ticket := range []string{"", "not-a-ticket"} {
		rr := env.setup(ticket)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("ticket=%q status = %d，期望 400（body=%q）", ticket, rr.Code, rr.Body.String())
		}
	}
}

// TestEnrollFlowRequiresBackupCodeConfirmation 覆盖 R-04 第 4 条：
// 验证码通过后 totp_enabled 仍为 false，只有确认已保存备用码才启用并建立会话。
func TestEnrollFlowRequiresBackupCodeConfirmation(t *testing.T) {
	env := newEnrollTestEnv(t)
	admin := env.addAdmin("enroll-admin")
	ticket := env.enrollTicket(admin.ID)

	// setup：返回 secret 与 otpauth URI，不改变状态。
	setup := env.setup(ticket)
	if setup.Code != http.StatusOK {
		t.Fatalf("setup status = %d，期望 200（body=%q）", setup.Code, setup.Body.String())
	}
	var setupResp twoFASetupResponse
	if err := json.Unmarshal(setup.Body.Bytes(), &setupResp); err != nil {
		t.Fatalf("setup JSON 非法: %v", err)
	}
	if setupResp.Secret == "" || !strings.HasPrefix(setupResp.OTPAuthURL, "otpauth://totp/") {
		t.Fatalf("setup 响应 = %+v", setupResp)
	}
	if got := env.repo.get(admin.ID); got == nil || got.TOTPEnabled {
		t.Fatal("setup 不得写 totp_enabled")
	}

	// enable：验证码通过、返回备用码，但仍不落 enabled。
	enable := env.enable(ticket, twoFATestSecret, twoFATestCode)
	if enable.Code != http.StatusOK {
		t.Fatalf("enable status = %d，期望 200（body=%q）", enable.Code, enable.Body.String())
	}
	var codesResp struct {
		Status      string   `json:"status"`
		BackupCodes []string `json:"backup_codes"`
	}
	if err := json.Unmarshal(enable.Body.Bytes(), &codesResp); err != nil {
		t.Fatalf("enable JSON 非法: %v", err)
	}
	if len(codesResp.BackupCodes) != 8 {
		t.Fatalf("备用码数量 = %d，期望 8", len(codesResp.BackupCodes))
	}
	if got := env.repo.get(admin.ID); got == nil || got.TOTPEnabled {
		t.Fatal("确认备用码之前 totp_enabled 必须保持 false")
	}

	// 未确认：拒绝且不启用。
	if rr := env.confirm(ticket, false); rr.Code != http.StatusBadRequest {
		t.Fatalf("未确认 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.get(admin.ID); got == nil || got.TOTPEnabled {
		t.Fatal("未确认备用码不得启用 2FA")
	}

	// 确认后：启用 + 签发会话 + 票据单次消费。
	confirmed := env.confirm(ticket, true)
	if confirmed.Code != http.StatusOK {
		t.Fatalf("confirm status = %d，期望 200（body=%q）", confirmed.Code, confirmed.Body.String())
	}
	var session authSessionResponse
	if err := json.Unmarshal(confirmed.Body.Bytes(), &session); err != nil {
		t.Fatalf("confirm JSON 非法: %v", err)
	}
	if session.User.ID != admin.ID {
		t.Fatalf("会话用户 = %d，期望 %d", session.User.ID, admin.ID)
	}
	if cookieByName(confirmed, auth.SessionCookieName) == nil {
		t.Fatal("绑定完成后必须签发正式会话")
	}
	if got := env.repo.get(admin.ID); got == nil || !got.TOTPEnabled {
		t.Fatal("确认备用码后 totp_enabled 必须为 true")
	}

	// 单次使用：同一 ticket 不能再次确认。
	if rr := env.confirm(ticket, true); rr.Code != http.StatusBadRequest {
		t.Fatalf("重复 confirm status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestEnrollWrongCodeDoesNotPersist：错误验证码不得产生任何写入。
func TestEnrollWrongCodeDoesNotPersist(t *testing.T) {
	env := newEnrollTestEnv(t)
	admin := env.addAdmin("enroll-wrong-code")
	ticket := env.enrollTicket(admin.ID)

	rr := env.enable(ticket, twoFATestSecret, "000000")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("错误码 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if got := env.repo.get(admin.ID); got == nil || got.TOTPEnabled {
		t.Fatal("错误验证码不得启用 2FA")
	}
	if confirmed := env.confirm(ticket, true); confirmed.Code != http.StatusBadRequest {
		t.Fatalf("未完成 enable 就 confirm status = %d，期望 400", confirmed.Code)
	}
}

// TestEnrollRejectsNonAdminTicket：即使拿到票据，非管理员也不能走绑定流程。
func TestEnrollRejectsNonAdminTicket(t *testing.T) {
	env := newEnrollTestEnv(t)
	user := env.repo.add(TwoFAUser{
		Username: "plain-user", Role: rbac.RoleUser,
		PasswordHash: twoFATestPasswordHash(), Status: "enabled",
	})
	ticket := env.enrollTicket(user.ID)

	if rr := env.setup(ticket); rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 setup status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
}
