//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
)

// 本文件是 Task 10 的集成验收：真实 PG16（主库 + 日志库）与 Redis 上的
// 硬删除终态、旧令牌 401、同名重注册、MFA 因子门、换绑邮箱与审计 PII 匿名化。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestAccountIntegration -v
//
// 日志库按计划冻结的测试手法取同一 DSN 并把 dbname 换成 newtg_logs。
// 未设置环境变量时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。

// accountIntegrationKey 是测试用 CREDENTIAL_ENCRYPTION_KEY（32 字节 base64 之外
// 的口令形态也可，auth.NewSecretCipher 会做 SHA-256 拉伸）。
const accountIntegrationKey = "account-integration-credential-encryption-key"

const accountIntegrationMFA = "123456"

// openAccountLogsDB 连接日志库（audit_logs 所在库）。
func openAccountLogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("解析 NEWTG_TEST_DB_DSN 失败: %v", err)
	}
	u.Path = "/newtg_logs"
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// accountSQLTokenLookup 是 Bearer 路径的最小 SQL 实现（与 tokenstore.Resolve
// 的读取面一致；本任务只需要证明令牌行被硬删除后 401）。
type accountSQLTokenLookup struct{ db *sql.DB }

func (l accountSQLTokenLookup) Resolve(ctx context.Context, keyHash string) (*auth.TokenMeta, error) {
	var (
		meta   auth.TokenMeta
		expiry sql.NullTime
		remain sql.NullInt64
	)
	err := l.db.QueryRowContext(ctx, `
		SELECT id, user_id, status, "group", expires_at, remain_quota
		FROM api_tokens WHERE key_hash = $1`, keyHash).
		Scan(&meta.ID, &meta.UserID, &meta.Status, &meta.Group, &expiry, &remain)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if expiry.Valid {
		meta.ExpiresAt = &expiry.Time
	}
	meta.UnlimitedQuota = !remain.Valid
	meta.RemainQuota = remain.Int64
	return &meta, nil
}

type accountIntegrationEnv struct {
	t        *testing.T
	db       *sql.DB
	logs     *sql.DB
	rdb      *redis.Client
	router   *Router
	handler  *AccountHandlers
	sessions *auth.SessionManager
}

func newAccountIntegrationEnv(t *testing.T) *accountIntegrationEnv {
	t.Helper()
	db, rdb := openAuthIntegration(t)
	ensureDefaultGroup(t, db)
	logs := openAccountLogsDB(t)

	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	router, err := NewRouter(RouterConfig{
		DB:            db,
		Redis:         rdb,
		Sessions:      sessions,
		SessionSecret: []byte(testSessionSecret),
		PublicBaseURL: accountTestBaseURL,
		TokenStore:    accountSQLTokenLookup{db: db},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	handler, err := NewAccountHandlers(AccountHandlerConfig{
		DB:            db,
		LogDB:         logs,
		Redis:         rdb,
		Sessions:      sessions,
		Audit:         audit.New(logs),
		EncryptionKey: accountIntegrationKey,
		SessionSecret: []byte(testSessionSecret),
		Config:        mapConfig{},
		VerifyTOTP: func(_, code string, _ time.Time) bool {
			return code == accountIntegrationMFA
		},
		NotificationSender: &accountTestNotifSender{},
	})
	if err != nil {
		t.Fatalf("NewAccountHandlers: %v", err)
	}
	handler.RegisterAccountRoutes(router.protected, nil)
	return &accountIntegrationEnv{
		t: t, db: db, logs: logs, rdb: rdb, router: router,
		handler: handler, sessions: sessions,
	}
}

func (e *accountIntegrationEnv) do(method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
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
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "AccountIntegration/1.0")
	}
	if mutate != nil {
		mutate(req)
	}
	return serve(e.router, req)
}

func (e *accountIntegrationEnv) sessionWithCSRF(userID int64) (sid, csrf string) {
	e.t.Helper()
	sid = mustSession(e.t, e.sessions, userID, "user")
	csrf = auth.NewCSRFToken()
	if err := e.sessions.IssueCSRF(context.Background(), sid, csrf); err != nil {
		e.t.Fatalf("IssueCSRF: %v", err)
	}
	return sid, csrf
}

func (e *accountIntegrationEnv) authed(method, path string, body any, sid, csrf string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(method, path, body, func(r *http.Request) {
		r.Host = "app.example.com"
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sid})
		r.Header.Set(auth.CSRFHeaderName, csrf)
		r.Header.Set("Origin", accountTestBaseURL)
	})
}

// createUser 直接经 SQLAuthRepo 建行（与生产注册同一写路径）。
func (e *accountIntegrationEnv) createUser(username, email string) *CreatedUser {
	e.t.Helper()
	hash, err := auth.HashPassword(authTestPassword)
	if err != nil {
		e.t.Fatalf("HashPassword: %v", err)
	}
	created, err := NewSQLAuthRepo(e.db).CreateUser(context.Background(), CreateUserInput{
		Username: username, Email: email, PasswordHash: hash,
	})
	if err != nil {
		e.t.Fatalf("CreateUser: %v", err)
	}
	e.t.Cleanup(func() { e.cleanup(created.ID) })
	return &created
}

// cleanup 兜底清理（成功的删除路径已自行清空，这里覆盖失败用例的残留）。
func (e *accountIntegrationEnv) cleanup(userID int64) {
	for _, stmt := range []string{
		`DELETE FROM api_tokens WHERE user_id = $1`,
		`DELETE FROM login_sessions WHERE user_id = $1`,
		`DELETE FROM user_oauth_bindings WHERE user_id = $1`,
		`DELETE FROM user_passkeys WHERE user_id = $1`,
		`DELETE FROM checkins WHERE user_id = $1`,
		`DELETE FROM wallet_transactions WHERE user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := e.db.Exec(stmt, userID); err != nil {
			e.t.Logf("cleanup %q for user %d: %v", stmt, userID, err)
		}
	}
	if _, err := e.logs.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID); err != nil {
		e.t.Logf("cleanup audit_logs for user %d: %v", userID, err)
	}
}

func accountCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// TestAccountIntegrationDeleteHardDeleteAndAnonymize 覆盖 brief (a)(b)(f)：
// 硬删除终态、旧 Bearer 令牌 401、保留面 PII 匿名化，以及删除事件的 who/when/ip。
func TestAccountIntegrationDeleteHardDeleteAndAnonymize(t *testing.T) {
	env := newAccountIntegrationEnv(t)
	suffix := uniqueAuthSuffix()
	username := "acc_del_" + suffix
	email := username + "@example.com"
	created := env.createUser(username, email)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 认证族附属行（删除时必须一起清掉，否则会阻塞同名重注册/OAuth 重绑）。
	if _, err := env.db.Exec(`INSERT INTO login_sessions (session_id_hash, user_id) VALUES ($1, $2)`,
		"hash-"+suffix, created.ID); err != nil {
		t.Fatalf("insert login_sessions: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO user_oauth_bindings (user_id, provider, provider_user_id)
		VALUES ($1, 'github', $2)`, created.ID, "gh-"+suffix); err != nil {
		t.Fatalf("insert oauth binding: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO user_passkeys (user_id, name, credential_id, public_key)
		VALUES ($1, 'it-key', $2, $3)`, created.ID, []byte("cred-"+suffix), []byte("pub-"+suffix)); err != nil {
		t.Fatalf("insert passkey: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO checkins (user_id, checkin_date, quota_awarded)
		VALUES ($1, current_date, 0)`, created.ID); err != nil {
		t.Fatalf("insert checkin: %v", err)
	}
	// 保留面：钱包流水必须存活（对账），审计行必须被匿名化。
	if _, err := env.db.Exec(`INSERT INTO wallet_transactions (user_id, type, amount, balance_after)
		VALUES ($1, 'bonus', 10, 10)`, created.ID); err != nil {
		t.Fatalf("insert wallet_transactions: %v", err)
	}
	if _, err := env.logs.Exec(`INSERT INTO audit_logs (user_id, username, category, result, detail)
		VALUES ($1, $2, 'login', 'success', '{"action":"login"}'::jsonb)`, created.ID, username); err != nil {
		t.Fatalf("insert audit_logs: %v", err)
	}

	const plain = "nkt-accountintegrationtoken000000000000000000000000"
	keyHash := auth.HashTokenKey(plain)
	var tokenID int64
	if err := env.db.QueryRow(`INSERT INTO api_tokens (user_id, name, key_hash, "group")
		VALUES ($1, 'it-token', $2, 'default') RETURNING id`, created.ID, keyHash).Scan(&tokenID); err != nil {
		t.Fatalf("insert api_token: %v", err)
	}
	// 制造一条缓存广播的观察点：删除时 handler 会向 token:changed:<id> 发布消息。
	sub := env.rdb.Subscribe(ctx, "token:changed:"+accountItoa(tokenID))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe token:changed: %v", err)
	}
	messages := sub.Channel()

	// 删除前 Bearer 可用。
	if rr := env.do(http.MethodGet, "/api/notification-preferences", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+plain)
	}); rr.Code != http.StatusOK {
		t.Fatalf("删除前 Bearer status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	sid, csrf := env.sessionWithCSRF(created.ID)
	rr := env.authed(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": username,
		"password":         authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("删除 status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	// (a) 终态：users/api_tokens/认证族附属行全部消失。
	if n := accountCount(t, env.db, `SELECT count(*) FROM users WHERE id = $1`, created.ID); n != 0 {
		t.Fatalf("users 行仍存在（%d）", n)
	}
	if n := accountCount(t, env.db, `SELECT count(*) FROM api_tokens WHERE user_id = $1`, created.ID); n != 0 {
		t.Fatalf("api_tokens 仍有 %d 行", n)
	}
	for _, table := range []string{"login_sessions", "user_oauth_bindings", "user_passkeys", "checkins"} {
		if n := accountCount(t, env.db, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, created.ID); n != 0 {
			t.Fatalf("%s 仍有 %d 行", table, n)
		}
	}
	// 保留面：钱包流水仍在（对账），审计用户名已匿名化。
	if n := accountCount(t, env.db, `SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, created.ID); n == 0 {
		t.Fatal("wallet_transactions 必须保留")
	}
	anonymized := "deleted_" + accountItoa(created.ID)
	var oldAuditUser string
	if err := env.logs.QueryRow(`SELECT username FROM audit_logs
		WHERE user_id = $1 AND detail->>'action' = 'login'`, created.ID).Scan(&oldAuditUser); err != nil {
		t.Fatalf("query anonymized audit row: %v", err)
	}
	if oldAuditUser != anonymized {
		t.Fatalf("审计 username = %q，期望 %q", oldAuditUser, anonymized)
	}
	// 删除事件保留 who/when/ip（真实用户名 + 请求 IP/UA）。
	var (
		delUser string
		delIP   sql.NullString
		delUA   sql.NullString
		delEvt  string
	)
	if err := env.logs.QueryRow(`SELECT username, ip, user_agent, detail->>'action'
		FROM audit_logs WHERE user_id = $1 AND detail->>'action' = 'account_deleted'`, created.ID).
		Scan(&delUser, &delIP, &delUA, &delEvt); err != nil {
		t.Fatalf("query account_deleted audit row: %v", err)
	}
	if delUser != username {
		t.Fatalf("删除事件 username = %q，期望真实用户名 %q（保留 who）", delUser, username)
	}
	if !delIP.Valid || delIP.String == "" {
		t.Fatal("删除事件必须保留 IP（保留 where）")
	}
	if !delUA.Valid || delUA.String == "" {
		t.Fatal("删除事件必须保留 User-Agent")
	}

	// (b) 旧令牌 401（令牌行已物理删除）。
	if rr := env.do(http.MethodGet, "/api/notification-preferences", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+plain)
	}); rr.Code != http.StatusUnauthorized {
		t.Fatalf("删除后旧令牌 status = %d，期望 401（body=%q）", rr.Code, rr.Body.String())
	}

	// GC-11：删除时广播 token:changed:<id>，进程内缓存 ≤1s 失效。
	select {
	case msg := <-messages:
		if msg.Channel != "token:changed:"+accountItoa(tokenID) {
			t.Fatalf("广播频道 = %q", msg.Channel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("未收到 token:changed 广播")
	}
}

// TestAccountIntegrationReRegisterSameUsername 覆盖 brief (c)：
// 硬删除后 users_username_uq 释放，同名可重新注册。
func TestAccountIntegrationReRegisterSameUsername(t *testing.T) {
	env := newAccountIntegrationEnv(t)
	suffix := uniqueAuthSuffix()
	username := "acc_re_" + suffix
	created := env.createUser(username, username+"@example.com")

	sid, csrf := env.sessionWithCSRF(created.ID)
	if rr := env.authed(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": username, "password": authTestPassword,
	}, sid, csrf); rr.Code != http.StatusNoContent {
		t.Fatalf("删除 status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	rr := env.do(http.MethodPost, "/api/auth/register", map[string]any{
		"username":        username,
		"email":           username + "-again@example.com",
		"password":        authTestPassword,
		"confirmPassword": authTestPassword,
		"agree_terms":     true,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("同名重新注册 status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var body struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("register 响应: %v", err)
	}
	if body.User.ID == created.ID || body.User.ID == 0 {
		t.Fatalf("重新注册 id = %d，期望新行（旧 %d）", body.User.ID, created.ID)
	}
	if n := accountCount(t, env.db, `SELECT count(*) FROM users WHERE username = $1`, username); n != 1 {
		t.Fatalf("username %q 行数 = %d，期望 1", username, n)
	}
}

// TestAccountIntegrationMFAFactor 覆盖 brief (d)：真实 SQLAccountReauthSource
// 解密 TOTP 种子后，缺第二因子 403；提供正确因子才删除。
func TestAccountIntegrationMFAFactor(t *testing.T) {
	env := newAccountIntegrationEnv(t)
	suffix := uniqueAuthSuffix()
	username := "acc_mfa_" + suffix
	created := env.createUser(username, username+"@example.com")

	cipher, err := auth.NewSecretCipher(accountIntegrationKey)
	if err != nil {
		t.Fatalf("NewSecretCipher: %v", err)
	}
	secretEnc, err := cipher.Encrypt("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := env.db.Exec(`UPDATE users SET totp_secret = $2, totp_enabled = true WHERE id = $1`,
		created.ID, secretEnc); err != nil {
		t.Fatalf("enable totp: %v", err)
	}

	sid, csrf := env.sessionWithCSRF(created.ID)
	rr := env.authed(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": username, "password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 TOTP status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if n := accountCount(t, env.db, `SELECT count(*) FROM users WHERE id = $1`, created.ID); n != 1 {
		t.Fatal("再认证失败不得删除用户")
	}

	rr = env.authed(http.MethodDelete, "/api/account", map[string]any{
		"confirm_username": username, "password": authTestPassword, "totp_code": accountIntegrationMFA,
	}, sid, csrf)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("带 TOTP status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
}

// TestAccountIntegrationEmailChange 覆盖 brief (e)：真实用户表上的换绑邮箱 —
// 未过再认证 403 不落库、验证码不消费、成功后 email_verified_at 置位、
// 已占用邮箱 409。
func TestAccountIntegrationEmailChange(t *testing.T) {
	env := newAccountIntegrationEnv(t)
	suffix := uniqueAuthSuffix()
	username := "acc_mail_" + suffix
	created := env.createUser(username, username+"@example.com")
	newEmail := username + "-new@example.com"
	code, err := env.handler.codes.issue(context.Background(), newEmail, "bind")
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	sid, csrf := env.sessionWithCSRF(created.ID)

	rr := env.authed(http.MethodPut, "/api/email", map[string]any{
		"new_email": newEmail, "code": code, "current_password": "wrong-password",
	}, sid, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("错误口令 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	var stored string
	if err := env.db.QueryRow(`SELECT email FROM users WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("query email: %v", err)
	}
	if stored != username+"@example.com" {
		t.Fatalf("再认证失败后邮箱 = %q，期望不变", stored)
	}

	rr = env.authed(http.MethodPut, "/api/email", map[string]any{
		"new_email": newEmail, "code": code, "current_password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("换绑 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	var verifiedAt sql.NullTime
	if err := env.db.QueryRow(`SELECT email, email_verified_at FROM users WHERE id = $1`, created.ID).
		Scan(&stored, &verifiedAt); err != nil {
		t.Fatalf("query updated email: %v", err)
	}
	if stored != newEmail || !verifiedAt.Valid {
		t.Fatalf("email = %q, verified_at = %v，期望新地址已验证", stored, verifiedAt)
	}

	// 邮箱占用：另一个账号持有目标地址 → 409，且本行不变。
	env.createUser("acc_mail_other_"+suffix, username+"-taken@example.com")
	takenCode, err := env.handler.codes.issue(context.Background(), username+"-taken@example.com", "bind")
	if err != nil {
		t.Fatalf("issue taken code: %v", err)
	}
	rr = env.authed(http.MethodPut, "/api/email", map[string]any{
		"new_email":        username + "-taken@example.com",
		"code":             takenCode,
		"current_password": authTestPassword,
	}, sid, csrf)
	if rr.Code != http.StatusConflict {
		t.Fatalf("占用邮箱 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	if err := env.db.QueryRow(`SELECT email FROM users WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("query email: %v", err)
	}
	if stored != newEmail {
		t.Fatalf("冲突后邮箱 = %q，期望 %q", stored, newEmail)
	}
}

// TestAccountIntegrationNotificationPreferences 验证 SQL 实现的稀疏合并往返。
func TestAccountIntegrationNotificationPreferences(t *testing.T) {
	env := newAccountIntegrationEnv(t)
	suffix := uniqueAuthSuffix()
	created := env.createUser("acc_pref_"+suffix, "acc_pref_"+suffix+"@example.com")
	sid, csrf := env.sessionWithCSRF(created.ID)

	rr := env.authed(http.MethodPut, "/api/notification-preferences", map[string]any{
		"balance_warning": map[string]any{
			"enabled": true, "threshold": 42, "channels": []string{"email"},
		},
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.authed(http.MethodPut, "/api/notification-preferences", map[string]any{
		"balance_warning": map[string]any{"threshold": 7},
	}, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("稀疏 PUT status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.authed(http.MethodGet, "/api/notification-preferences", nil, sid, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var prefs struct {
		BalanceWarning struct {
			Enabled   bool     `json:"enabled"`
			Threshold int64    `json:"threshold"`
			Channels  []string `json:"channels"`
		} `json:"balance_warning"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &prefs); err != nil {
		t.Fatalf("GET 响应: %v（body=%q）", err, rr.Body.String())
	}
	if !prefs.BalanceWarning.Enabled || prefs.BalanceWarning.Threshold != 7 ||
		len(prefs.BalanceWarning.Channels) != 1 || prefs.BalanceWarning.Channels[0] != "email" {
		t.Fatalf("合并结果 = %+v，期望 threshold=7 且 channels 保留", prefs.BalanceWarning)
	}
}

// itoa 是 strconv.FormatInt 的短别名（本文件多处拼 deleted_{id}/频道名）。
func accountItoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
