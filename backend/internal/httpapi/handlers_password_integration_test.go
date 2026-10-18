//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 8 的集成验收：PasswordRepo 的 SQL 实现与「找回 → 重置 →
// 全量吊销 → 新口令登录 → 改密」完整 HTTP 链，对真实 PG16 + Redis。
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationPassword -v
//
// 未设置环境变量时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。

// pwdDoJSON 发起一次 JSON 请求；mutate 用于追加 Cookie/Header/RemoteAddr。
func pwdDoJSON(t *testing.T, h http.Handler, method, path string, body any, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
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
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func pwdCookieValue(cookies []*http.Cookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// TestIntegrationPasswordSQLRepo 覆盖 PasswordRepo 的三个 SQL 方法：
// 邮箱大小写不敏感查找、按 id 查找、口令更新同时清除 must_change_password。
func TestIntegrationPasswordSQLRepo(t *testing.T) {
	db, _ := openAuthIntegration(t)
	ensureDefaultGroup(t, db)
	repo := NewSQLAuthRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	suffix := uniqueAuthSuffix()
	username := "pwd_it_" + suffix
	email := "pwd_it_" + suffix + "@example.com"
	oldHash, err := auth.HashPassword("0ld!Passw0rd")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	created, err := repo.CreateUser(ctx, CreateUserInput{
		Username: username, Email: email, PasswordHash: oldHash, EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, created.ID) })
	if _, err := db.Exec(`UPDATE users SET must_change_password = true WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("set must_change_password: %v", err)
	}

	// 邮箱查找对大小写不敏感（注册时前端可能保留大小写）。
	byEmail, err := repo.FindUserByEmail(ctx, "PWD_IT_"+suffix+"@EXAMPLE.COM")
	if err != nil || byEmail == nil || byEmail.ID != created.ID {
		t.Fatalf("FindUserByEmail = %+v/%v，期望用户 %d", byEmail, err, created.ID)
	}
	if missing, err := repo.FindUserByEmail(ctx, "nobody-"+suffix+"@example.com"); err != nil || missing != nil {
		t.Fatalf("未知邮箱应返回 nil,nil；实际 %+v/%v", missing, err)
	}

	byID, err := repo.FindUserByID(ctx, created.ID)
	if err != nil || byID == nil || byID.Username != username || byID.Email != email {
		t.Fatalf("FindUserByID = %+v/%v", byID, err)
	}
	if missing, err := repo.FindUserByID(ctx, created.ID+1_000_000); err != nil || missing != nil {
		t.Fatalf("未知 id 应返回 nil,nil；实际 %+v/%v", missing, err)
	}

	newHash, err := auth.HashPassword("N3w!Passw0rd")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := repo.UpdatePasswordHash(ctx, created.ID, newHash); err != nil {
		t.Fatalf("UpdatePasswordHash: %v", err)
	}
	var (
		storedHash string
		mustChange bool
	)
	if err := db.QueryRow(`SELECT COALESCE(password_hash, ''), must_change_password
		FROM users WHERE id = $1`, created.ID).Scan(&storedHash, &mustChange); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if !auth.VerifyPassword(storedHash, "N3w!Passw0rd") {
		t.Fatal("DB 中的 password_hash 不是新口令的 argon2id 哈希")
	}
	if mustChange {
		t.Fatal("UpdatePasswordHash 必须清除 must_change_password")
	}

	// 不存在的用户必须报错而不是静默成功（否则改密会「成功」却不落库）。
	if err := repo.UpdatePasswordHash(ctx, created.ID+1_000_000, newHash); !errors.Is(err, ErrPasswordUserNotFound) {
		t.Fatalf("未知用户 UpdatePasswordHash err = %v，期望 ErrPasswordUserNotFound", err)
	}
}

// TestIntegrationPasswordResetAndChangeFlow 跑通：
// 登录 → 找回密码 → 重置（旧会话全失效）→ 新口令登录 → 改密（保留当前会话）。
func TestIntegrationPasswordResetAndChangeFlow(t *testing.T) {
	db, rdb := openAuthIntegration(t)
	ensureDefaultGroup(t, db)

	sender := &resetTestSender{}
	router, err := NewRouter(RouterConfig{
		DB:            db,
		Redis:         rdb,
		SessionSecret: []byte(testSessionSecret),
		PublicBaseURL: passwordTestBaseURL,
		EmailSender:   sender,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := router.RegisterPasswordRoutes(passwordTestBaseURL, nil); err != nil {
		t.Fatalf("RegisterPasswordRoutes: %v", err)
	}

	repo := NewSQLAuthRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	suffix := uniqueAuthSuffix()
	username := "flow_pwd_" + suffix
	email := "flow_pwd_" + suffix + "@example.com"
	const oldPassword = "0ld!Passw0rd"
	oldHash, err := auth.HashPassword(oldPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	created, err := repo.CreateUser(ctx, CreateUserInput{
		Username: username, Email: email, PasswordHash: oldHash, EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, created.ID) })

	// 登录拿到会话 Cookie。
	login := pwdDoJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": username, "password": oldPassword,
	}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d（body=%q）", login.Code, login.Body.String())
	}
	sid := pwdCookieValue(login.Result().Cookies(), auth.SessionCookieName)
	if sid == "" {
		t.Fatal("登录未下发会话 Cookie")
	}

	// 找回密码：投递是异步的，轮询等待链接出现（最多 2s）。
	if rr := pwdDoJSON(t, router, http.MethodPost, "/api/auth/password-reset/request",
		map[string]any{"email": email}, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("重置请求 status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for sender.count() == 0 {
		if !time.Now().Before(deadline) {
			t.Fatal("2s 内未收到重置邮件")
		}
		time.Sleep(5 * time.Millisecond)
	}
	link := sender.last(t)
	parsed, err := url.Parse(link.ResetURL)
	if err != nil {
		t.Fatalf("解析重置链接: %v", err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("链接缺少 token: %q", link.ResetURL)
	}

	const resetPassword = "R3set!Passw0rd"
	if rr := pwdDoJSON(t, router, http.MethodPost, "/api/auth/password-reset/confirm", map[string]any{
		"token": token, "new_password": resetPassword, "confirm": resetPassword,
	}, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("confirm status = %d（body=%q）", rr.Code, rr.Body.String())
	}

	// 重置前的会话必须全部失效（Redis 权威存储）。
	manager := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	if _, err := manager.Get(ctx, sid); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Fatalf("重置后旧会话 err = %v，期望 ErrSessionNotFound", err)
	}

	// 旧口令 401、新口令 200。
	if rr := pwdDoJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": username, "password": oldPassword,
	}, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("旧口令登录 status = %d，期望 401", rr.Code)
	}
	relogin := pwdDoJSON(t, router, http.MethodPost, "/api/auth/login", map[string]any{
		"identifier": username, "password": resetPassword,
	}, nil)
	if relogin.Code != http.StatusOK {
		t.Fatalf("新口令登录 status = %d（body=%q）", relogin.Code, relogin.Body.String())
	}
	reloginCookies := relogin.Result().Cookies()
	newSID := pwdCookieValue(reloginCookies, auth.SessionCookieName)
	newCSRF := pwdCookieValue(reloginCookies, auth.CSRFCookieName)
	if newSID == "" || newCSRF == "" {
		t.Fatal("登录未下发会话/CSRF Cookie")
	}

	// 改密：保留当前会话、轮换 CSRF、写库。
	const changedPassword = "Ch4nged!Passw0rd"
	change := pwdDoJSON(t, router, http.MethodPost, "/api/password", map[string]any{
		"current_password": resetPassword,
		"new_password":     changedPassword,
	}, func(r *http.Request) {
		r.Host = "app.example.com"
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: newSID})
		r.Header.Set(auth.CSRFHeaderName, newCSRF)
		r.Header.Set("Origin", passwordTestBaseURL)
	})
	if change.Code != http.StatusNoContent {
		t.Fatalf("改密 status = %d（body=%q）", change.Code, change.Body.String())
	}
	if _, err := manager.Get(ctx, newSID); err != nil {
		t.Fatalf("改密后当前会话 err = %v，期望仍有效", err)
	}
	var stored string
	if err := db.QueryRow(`SELECT COALESCE(password_hash, '') FROM users WHERE id = $1`, created.ID).Scan(&stored); err != nil {
		t.Fatalf("query password_hash: %v", err)
	}
	if !auth.VerifyPassword(stored, changedPassword) {
		t.Fatal("改密未写入 DB")
	}
}
