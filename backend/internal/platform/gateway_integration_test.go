//go:build integration

package platform

// gateway 角色的端到端集成验收：真实 PostgreSQL（主库 + 日志库）与 Redis，
// 走生产 connectors（RunGateway → OpenDB/OpenLogsDB/OpenRedis → migrate 关闭
// → newGatewayAPI → newGatewayMux），证明 /healthz、/readyz 与 M1 API 在同一
// server 上可用，且登录会建立真实会话。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/platform/ -run TestIntegrationGateway -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/config"
)

func TestIntegrationGatewayServesM1API(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if dsn == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}

	addr := freeAddr(t)
	t.Setenv("DATABASE_URL", dsn)
	t.Setenv("DB_LOGS_DSN", logsDSN)
	t.Setenv("REDIS_URL", "redis://"+redisAddr)
	t.Setenv("SESSION_SECRET", "gateway-integration-session-secret")
	t.Setenv("CREDENTIAL_ENCRYPTION_KEY", "gateway-integration-credential-key")
	t.Setenv("DB_MIGRATE_ON_START", "false")
	t.Setenv("METRICS_ADDR", "0")
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:3000")
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("APP_ENV", "production")
	t.Setenv("LOG_FORMAT", "json")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunGateway(ctx, cfg) }()

	waitHealthy(t, addr)

	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		// 只读测试响应；关闭失败不影响断言。
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return resp, string(body)
	}

	// /readyz 五项：DB/Redis/迁移版本/配置装载/启动完成。
	if resp, body := get("/readyz"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d, body=%s", resp.StatusCode, body)
	}

	// 公开端点无需会话。
	if resp, body := get("/api/status"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/status = %d, body=%s", resp.StatusCode, body)
	} else {
		var status struct {
			Languages []string `json:"languages"`
		}
		if err := json.Unmarshal([]byte(body), &status); err != nil || len(status.Languages) == 0 {
			t.Fatalf("/api/status body = %q（缺少公开投影字段）", body)
		}
	}
	if resp, body := get("/api/auth/login/encryption-key"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/auth/login/encryption-key = %d, body=%s", resp.StatusCode, body)
	} else if !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("encryption-key body = %q, want enabled:false", body)
	}
	if resp, body := get("/api/usage/token"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/api/usage/token = %d, body=%s（应进入公共 usage handler）", resp.StatusCode, body)
	} else if !strings.Contains(body, "invalid_token") {
		t.Fatalf("/api/usage/token body = %q, want invalid_token", body)
	}

	// 受保护端点无会话一律 401。
	for _, path := range []string{"/api/tokens", "/api/me", "/api/sessions", "/api/users", "/api/groups"} {
		resp, body := get(path)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s = %d, body=%s, want 401", path, resp.StatusCode, body)
		}
		if !strings.Contains(body, "unauthorized") {
			t.Fatalf("%s body = %q, want problem type unauthorized", path, body)
		}
	}

	// setup 公开可达（空体 → 400，而不是 401/404）。
	resp, body := postJSON(t, client, "http://"+addr+"/api/setup", `{}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /api/setup = %d, body=%s, want 400", resp.StatusCode, body)
	}

	// 真实注册/登录链：建用户 → 登录 → 会话访问 /api/me。
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ensureIntegrationGroup(t, db)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	username := "gw_" + suffix
	password := "integration-correct-horse"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	var userID int64
	err = db.QueryRow(`
		INSERT INTO users (username, email, email_verified_at, password_hash)
		VALUES ($1, $2, now(), $3)
		RETURNING id`, username, username+"@example.com", hash).Scan(&userID)
	if err != nil {
		t.Fatalf("insert integration user: %v", err)
	}
	t.Cleanup(func() { cleanupIntegrationUser(t, db, userID) })

	loginBody := fmt.Sprintf(`{"identifier":%q,"password":%q}`, username, password)
	resp, body = postJSON(t, client, "http://"+addr+"/api/auth/login", loginBody, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, body=%s", resp.StatusCode, body)
	}
	var cookies []*http.Cookie
	cookies = append(cookies, resp.Cookies()...)
	if len(cookies) == 0 {
		t.Fatal("login 未签发会话 Cookie")
	}
	// 会话访问 /api/me（Cookie 路径，GET 为安全方法，不触发 CSRF）。
	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/api/me", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	meResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/me: %v", err)
	}
	meBody, _ := io.ReadAll(meResp.Body)
	_ = meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me = %d, body=%s", meResp.StatusCode, meBody)
	}
	if !strings.Contains(string(meBody), username) {
		t.Fatalf("/api/me body = %s, want username %q", meBody, username)
	}

	// 优雅停机：ctx cancel 后 RunGateway 返回 nil。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunGateway returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunGateway did not stop after ctx cancel")
	}
}

func postJSON(t *testing.T, client *http.Client, url, body string, cookies []*http.Cookie) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	raw, err := io.ReadAll(resp.Body)
	// 只读测试响应；关闭失败不影响断言。
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, string(raw)
}

func ensureIntegrationGroup(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO user_groups (name) VALUES ('default')
		ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("seed default group: %v", err)
	}
}

func cleanupIntegrationUser(t *testing.T, db *sql.DB, userID int64) {
	t.Helper()
	for _, stmt := range []string{
		`DELETE FROM wallet_transactions WHERE user_id = $1`,
		`DELETE FROM login_sessions WHERE user_id = $1`,
		`DELETE FROM api_tokens WHERE user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := db.Exec(stmt, userID); err != nil {
			t.Logf("cleanup %q for user %d: %v", stmt, userID, err)
		}
	}
}
