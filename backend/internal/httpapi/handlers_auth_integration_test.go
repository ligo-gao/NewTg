//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 6 的集成验收：SQLAuthRepo / SQLSessionRecorder / 完整注册登录链
// 对真实 PG16 + Redis。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationAuth -v
//
// 未设置环境变量时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。

func openAuthIntegration(t *testing.T) (*sql.DB, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Fatalf("ping test redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return db, rdb
}

// ensureDefaultGroup 补齐 M0 未 seed 的 default 分组行，让 users.group 的
// 外键（platform-04 V5：users.group → user_groups.name）可以接受注册 INSERT。
func ensureDefaultGroup(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO user_groups (name) VALUES ('default')
		ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("seed default group: %v", err)
	}
}

func uniqueAuthSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func cleanupAuthUser(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM wallet_transactions WHERE user_id = $1`,
		`DELETE FROM login_sessions WHERE user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := db.Exec(stmt, userID); err != nil {
			t.Logf("cleanup %q for user %d: %v", stmt, userID, err)
		}
	}
}

// TestIntegrationAuthSQLRepo 覆盖 Task 6 的 SQL 接缝：注册写库（含 GC-5 赠送
// 流水）、按用户名/邮箱查找、admin_permissions NULL/空数组语义、唯一冲突、
// login_sessions 只落哈希。
func TestIntegrationAuthSQLRepo(t *testing.T) {
	db, _ := openAuthIntegration(t)
	ensureDefaultGroup(t, db)
	repo := NewSQLAuthRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := uniqueAuthSuffix()
	username := "it_" + suffix
	email := "it_" + suffix + "@example.com"

	created, err := repo.CreateUser(ctx, CreateUserInput{
		Username:      username,
		Email:         email,
		PasswordHash:  "$argon2id$fake",
		Aff:           "partner-1",
		EmailVerified: true,
		SignupBonus:   250,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, created.ID) })
	if created.Role != "user" || created.Status != "enabled" {
		t.Fatalf("CreatedUser = %+v，期望 DB 默认 role=user/status=enabled", created)
	}

	// 余额与赠送流水（GC-5：一笔余额变动恰一行 wallet_transactions）。
	var (
		balance  int64
		verified bool
		aff      sql.NullString
	)
	if err := db.QueryRow(`SELECT balance, email_verified_at IS NOT NULL, aff FROM users WHERE id = $1`,
		created.ID).Scan(&balance, &verified, &aff); err != nil {
		t.Fatalf("query user row: %v", err)
	}
	if balance != 250 || !verified || aff.String != "partner-1" {
		t.Fatalf("用户行 balance=%d verified=%v aff=%q，期望 250/true/partner-1", balance, verified, aff.String)
	}
	var (
		txType, txStatus, refType, refID, note string
		amount, balanceAfter                   int64
	)
	if err := db.QueryRow(`SELECT type, amount, balance_after, status, reference_type, reference_id, COALESCE(note,'')
		FROM wallet_transactions WHERE user_id = $1`, created.ID).
		Scan(&txType, &amount, &balanceAfter, &txStatus, &refType, &refID, &note); err != nil {
		t.Fatalf("query wallet transaction: %v", err)
	}
	if txType != "bonus" || amount != 250 || balanceAfter != 250 || txStatus != "confirmed" ||
		refType != "signup_grant" || refID != strconv.FormatInt(created.ID, 10) {
		t.Fatalf("赠送流水 = %s/%d/%d/%s/%s/%s，期望 bonus/250/250/confirmed/signup_grant/%d",
			txType, amount, balanceAfter, txStatus, refType, refID, created.ID)
	}

	// 按用户名与邮箱（大小写不敏感）查找。
	byName, err := repo.FindUserByIdentifier(ctx, username)
	if err != nil || byName == nil || byName.ID != created.ID {
		t.Fatalf("按用户名查找 = %+v/%v", byName, err)
	}
	byEmail, err := repo.FindUserByIdentifier(ctx, "IT_"+suffix+"@EXAMPLE.COM")
	if err != nil || byEmail == nil || byEmail.ID != created.ID {
		t.Fatalf("按邮箱（大小写不敏感）查找 = %+v/%v", byEmail, err)
	}
	if missing, err := repo.FindUserByIdentifier(ctx, "no-such-user-"+suffix); err != nil || missing != nil {
		t.Fatalf("未知 identifier 应返回 nil,nil；实际 %+v/%v", missing, err)
	}

	// admin_permissions 语义与 users.role 绑定（Task 14 评审裁决）：
	//   role=admin + NULL       -> nil（FULL）
	//   role=admin + []         -> 非 nil 空集（无任何权限）
	//   role=admin + 数组       -> 显式集合
	//   role=user  + 任何值     -> 非 nil 空集（降权后的陈旧会话不得借 NULL=FULL）
	if _, err := db.Exec(`UPDATE users SET role = 'admin' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("promote test user to admin: %v", err)
	}
	if perms, err := repo.Load(ctx, created.ID); err != nil || perms != nil {
		t.Fatalf("admin + NULL 权限 Load = %#v/%v，期望 nil,nil", perms, err)
	}
	if _, err := db.Exec(`UPDATE users SET role = 'user' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("demote test user: %v", err)
	}
	if perms, err := repo.Load(ctx, created.ID); err != nil || perms == nil || len(perms) != 0 {
		t.Fatalf("role=user + NULL 权限 Load = %#v/%v，期望非 nil 空集", perms, err)
	}
	if _, err := db.Exec(`UPDATE users SET role = 'admin' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("re-promote test user: %v", err)
	}
	if _, err := db.Exec(`UPDATE users SET admin_permissions = '["users:read"]'::jsonb WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("set permissions: %v", err)
	}
	perms, err := repo.Load(ctx, created.ID)
	if err != nil || len(perms) != 1 || perms[0] != "users:read" {
		t.Fatalf("显式权限 Load = %#v/%v", perms, err)
	}
	if _, err := db.Exec(`UPDATE users SET admin_permissions = '[]'::jsonb WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("clear permissions: %v", err)
	}
	empty, err := repo.Load(ctx, created.ID)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("空数组权限 Load = %#v/%v，期望非 nil 空切片（不是 FULL）", empty, err)
	}
	if _, err := db.Exec(`UPDATE users SET admin_permissions = NULL WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("null permissions: %v", err)
	}

	// 唯一冲突映射到 ErrDuplicateUser（username 与 email 两路）。
	if _, err := repo.CreateUser(ctx, CreateUserInput{Username: username, Email: "other-" + suffix + "@example.com"}); err != ErrDuplicateUser {
		t.Fatalf("重复 username err = %v，期望 ErrDuplicateUser", err)
	}
	if _, err := repo.CreateUser(ctx, CreateUserInput{Username: "other_" + suffix, Email: email}); err != ErrDuplicateUser {
		t.Fatalf("重复 email err = %v，期望 ErrDuplicateUser", err)
	}

	// login_sessions 只落 SHA-256(sid)。
	sid := auth.NewCSRFToken() // 任意高熵串，模拟 session id
	recorder := NewSQLSessionRecorder(db)
	if err := recorder.RecordSession(ctx, created.ID, auth.HashSessionID(sid)); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	var storedHash string
	if err := db.QueryRow(`SELECT session_id_hash FROM login_sessions WHERE user_id = $1`, created.ID).Scan(&storedHash); err != nil {
		t.Fatalf("query login_sessions: %v", err)
	}
	if storedHash != auth.HashSessionID(sid) || storedHash == sid {
		t.Fatalf("login_sessions.session_id_hash = %q，期望仅存哈希", storedHash)
	}
}

// TestIntegrationAuthFlow 跑通「验证码 → 注册（自动登录/赠送）→ 登出 → 登录」
// 的完整 HTTP 链，验证 SQL 仓储与 Redis 会话/限流在同一装配下协同工作。
func TestIntegrationAuthFlow(t *testing.T) {
	db, rdb := openAuthIntegration(t)
	ensureDefaultGroup(t, db)

	cfg := mapConfig{
		"auth.email_verification_required": configBool(true),
		"billing.signup_grant_credits":     configInt(100),
	}
	sender := &fakeEmailSender{}
	router, err := NewRouter(RouterConfig{
		DB:            db,
		Redis:         rdb,
		Config:        cfg,
		SessionSecret: []byte(testSessionSecret),
		EmailSender:   sender,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	suffix := uniqueAuthSuffix()
	// 注册赠送按 signup_grant:ip:<sha256(clientIP)> 限 3 次/24h，而共享测试 Redis
	// 会被其他运行/任务复用。httptest 默认 RemoteAddr 恒为 192.0.2.1，同一天第 4 次
	// 运行就会拿到 bonus=0。这里每次运行使用唯一客户端 IP，并在收尾时清理该 IP 的
	// 计数键（其余键随 TTL 自然过期），使断言与运行顺序/并行度无关。
	testIP := "it-" + suffix
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(),
			"signup_grant:ip:"+sha256Hex(testIP),
			"email_code:ip:"+sha256Hex(testIP)).Err()
	})
	username := "flow_" + suffix
	email := "flow_" + suffix + "@example.com"
	password := "N7x!qPz2wR"

	do := func(method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.RemoteAddr = testIP + ":1234"
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if mutate != nil {
			mutate(req)
		}
		return serve(router, req)
	}

	if rr := do(http.MethodPost, "/api/auth/email-code",
		map[string]any{"email": email, "purpose": "register"}, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("email-code status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	code := sender.last(t).Code

	// 验证码只存哈希：必须在注册消费掉键之前断言，否则 GET 落空会让断言空转。
	raw, err := rdb.Get(context.Background(), "email_code:register:"+email).Result()
	if err != nil {
		t.Fatalf("读取验证码键: %v", err)
	}
	if raw == code || len(raw) != 64 {
		t.Fatalf("Redis 验证码值 = %q，期望 64 位哈希且不等于明文", raw)
	}

	registered := do(http.MethodPost, "/api/auth/register",
		registerPayload(username, email, password, code), nil)
	if registered.Code != http.StatusCreated {
		t.Fatalf("register status = %d，期望 201（body=%q）", registered.Code, registered.Body.String())
	}
	var resp authSessionResponse
	if err := json.Unmarshal(registered.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	userID := resp.User.ID
	if userID == 0 {
		t.Fatalf("注册响应缺少 user.id: %s", registered.Body.String())
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, userID) })

	var balance int64
	var verified bool
	if err := db.QueryRow(`SELECT balance, email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID).
		Scan(&balance, &verified); err != nil {
		t.Fatalf("query registered user: %v", err)
	}
	if balance != 100 {
		t.Fatalf("注册赠送后 balance = %d，期望 100", balance)
	}
	if !verified {
		t.Error("验证码注册后 email_verified_at 应已置位")
	}

	// SessionRecorder 是异步落库：轮询等待审计副本出现。
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM login_sessions WHERE user_id = $1`, userID).Scan(&n); err != nil {
			t.Fatalf("count login_sessions: %v", err)
		}
		if n > 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("2s 内未写入 login_sessions 审计副本")
		}
		time.Sleep(20 * time.Millisecond)
	}

	sess := cookieByName(registered, auth.SessionCookieName)
	if sess == nil {
		t.Fatal("注册未下发会话 Cookie")
	}
	withCookie := func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.Value})
	}

	if rr := do(http.MethodPost, "/api/logout", nil, withCookie); rr.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}

	login := do(http.MethodPost, "/api/auth/login",
		map[string]any{"identifier": username, "password": password}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("logout 后重新登录 status = %d，期望 200（body=%q）", login.Code, login.Body.String())
	}
	if cookieByName(login, auth.SessionCookieName) == nil {
		t.Fatal("重新登录未下发会话 Cookie")
	}
}
