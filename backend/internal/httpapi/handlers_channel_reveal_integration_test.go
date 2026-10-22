//go:build integration

package httpapi

// W11：渠道凭据受控揭示的真实 PostgreSQL + Redis 验收。
//
// 单元测试证明了 handler 契约；本文件证明三件只有真基础设施才能证明的事：
//  1. SQLAccountReauthSource 能从 users 表读出真实口令哈希并通过步进认证；
//  2. audit_logs（独立日志库）确实落库，且 detail/其他列**不含凭据明文**；
//  3. 管理员维限流键在 Redis 中生效（第 N+1 次 429）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w11?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w11_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestChannelRevealIntegration -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

const (
	revealIntegrationPassword = "W11RevealIntegration9!"
	revealIntegrationSecret   = "sk-w11-integration-secret-9876"
)

type revealIntegrationEnv struct {
	mainDB  *sql.DB
	logsDB  *sql.DB
	rdb     *redis.Client
	mux     *http.ServeMux
	userID  int64
	channel int64
}

func openRevealIntegrationEnv(t *testing.T) *revealIntegrationEnv {
	t.Helper()
	mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if mainDSN == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过 W11 集成测试")
	}

	mainDB, err := sql.Open("pgx", mainDSN)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })
	if err := mainDB.Ping(); err != nil {
		t.Fatalf("ping main db: %v", err)
	}
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })
	if err := logsDB.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}

	env := &revealIntegrationEnv{mainDB: mainDB, logsDB: logsDB, rdb: rdb}
	env.userID = env.insertAdminUser(t)
	env.channel = env.insertChannel(t)

	// 用户名唯一且满足 users_username_chk（3–32 字符，[a-zA-Z0-9_-]）。
	source, err := NewSQLAccountReauthSource(mainDB, "w11-reveal-integration-encryption-key")
	if err != nil {
		t.Fatalf("reauth source: %v", err)
	}
	gate := auth.NewReauthGate(rdb, source, auth.VerifyTOTP)
	auditor := audit.New(logsDB)
	sealer := NewSecretSealer(testSealerMaster)
	h := NewChannelHandler(store.NewChannelRepo(mainDB), newFakeUserAccess(), sealer,
		ChannelHandlerOptions{
			Idempotency: NewMemoryIdempotencyStore(),
			Redis:       rdb,
			Reauth:      gate,
			Audit:       auditor,
		})
	env.mux = http.NewServeMux()
	h.Register(env.mux)

	t.Cleanup(func() {
		ctx := context.Background()
		// 只清理本用例的数据与键，绝不 FLUSH 共享测试 Redis/日志库。
		_, _ = logsDB.ExecContext(ctx, `DELETE FROM audit_logs WHERE user_id = $1`, env.userID)
		_, _ = mainDB.ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, env.channel)
		_, _ = mainDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, env.userID)
		_ = rdb.Del(ctx,
			ChannelRevealRateLimitKey(env.userID),
			"reauth:"+strconv.FormatInt(env.userID, 10),
			"reauth_fail:"+strconv.FormatInt(env.userID, 10)).Err()
	})
	return env
}

func (e *revealIntegrationEnv) insertAdminUser(t *testing.T) int64 {
	t.Helper()
	hash, err := auth.HashPassword(revealIntegrationPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	username := fmt.Sprintf("w11reveal%d", time.Now().UnixNano()%1_000_000_000_000)
	var id int64
	err = e.mainDB.QueryRow(`
		INSERT INTO users (username, password_hash, role, status)
		VALUES ($1, $2, 'admin', 'enabled')
		RETURNING id`, username, hash).Scan(&id)
	if err != nil {
		t.Fatalf("insert admin user: %v", err)
	}
	return id
}

func (e *revealIntegrationEnv) insertChannel(t *testing.T) int64 {
	t.Helper()
	sealer := NewSecretSealer(testSealerMaster)
	sealed, err := sealer.Seal(mustJSONBytes(t, ChannelCredentials{APIKey: revealIntegrationSecret}))
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	name := fmt.Sprintf("w11-reveal-%d", time.Now().UnixNano())
	var id int64
	err = e.mainDB.QueryRow(`
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			model_mapping, weight, priority, status, auto_ban)
		VALUES ($1, 'openai', 'https://upstream.example.com', $2, '["gpt-4o"]'::jsonb,
			'{}'::jsonb, 1, 0, 1, true)
		RETURNING id`, name, sealed).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

func (e *revealIntegrationEnv) reveal(t *testing.T, rawID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonReq(t, http.MethodPost, "/api/channels/"+rawID+"/credentials/reveal", body)
	return serveWithPrincipal(t, e.mux, req, &auth.Principal{
		UserID:      e.userID,
		Role:        "admin",
		Permissions: []string{"*"},
	})
}

type auditDetailRow struct {
	result   string
	category string
	detail   string
}

func (e *revealIntegrationEnv) auditRows(t *testing.T) []auditDetailRow {
	t.Helper()
	rows, err := e.logsDB.Query(`
		SELECT result, category, detail::text
		FROM audit_logs
		WHERE user_id = $1 AND detail->>'action' = 'channel.credentials_reveal'
		ORDER BY id`, e.userID)
	if err != nil {
		t.Fatalf("query audit_logs: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditDetailRow
	for rows.Next() {
		var r auditDetailRow
		if err := rows.Scan(&r.result, &r.category, &r.detail); err != nil {
			t.Fatalf("scan audit_logs: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit_logs: %v", err)
	}
	return out
}

func TestChannelRevealIntegration(t *testing.T) {
	env := openRevealIntegrationEnv(t)
	id := strconv.FormatInt(env.channel, 10)

	t.Run("success_returns_plaintext_once_and_audits_metadata", func(t *testing.T) {
		rr := env.reveal(t, id, map[string]any{
			"reason":   "integration migration check",
			"password": revealIntegrationPassword,
		})
		if rr.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q，期望 no-store", got)
		}
		if !strings.Contains(rr.Body.String(), revealIntegrationSecret) {
			t.Fatalf("成功响应缺少凭据明文：%s", rr.Body.String())
		}

		rows := env.auditRows(t)
		if len(rows) != 1 {
			t.Fatalf("审计行数 = %d，期望 1（rows=%+v）", len(rows), rows)
		}
		row := rows[0]
		if row.result != "success" || row.category != "operation" {
			t.Fatalf("审计 result/category = %q/%q，期望 success/operation", row.result, row.category)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(row.detail), &detail); err != nil {
			t.Fatalf("审计 detail 不是合法 JSON: %v", err)
		}
		if detail["channel_id"] != float64(env.channel) ||
			detail["reason"] != "integration migration check" {
			t.Fatalf("审计 detail = %v，期望 channel_id/reason", detail)
		}
		for _, argument := range []string{row.detail} {
			if strings.Contains(argument, revealIntegrationSecret) ||
				strings.Contains(argument, "sk-w11") {
				t.Fatalf("审计包含凭据明文：%s", argument)
			}
		}

		// 步进认证只授权本次请求：Redis 不得出现再认证窗口。
		if n, err := env.rdb.Exists(context.Background(), "reauth:"+strconv.FormatInt(env.userID, 10)).Result(); err != nil || n != 0 {
			t.Fatalf("揭示后 reauth 窗口存在（n=%d err=%v），期望 0", n, err)
		}
	})

	t.Run("wrong_password_is_audited_without_plaintext", func(t *testing.T) {
		rr := env.reveal(t, id, map[string]any{
			"reason":   "wrong factor",
			"password": "definitely-not-the-password",
		})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("状态码 = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
		}
		rows := env.auditRows(t)
		failed := 0
		for _, row := range rows {
			if row.result == "failed" {
				failed++
			}
			if strings.Contains(row.detail, revealIntegrationSecret) {
				t.Fatalf("审计包含凭据明文：%s", row.detail)
			}
		}
		if failed != 1 {
			t.Fatalf("失败审计行数 = %d，期望 1（rows=%+v）", failed, rows)
		}
	})

	t.Run("rate_limit_is_enforced_in_redis", func(t *testing.T) {
		ctx := context.Background()
		key := ChannelRevealRateLimitKey(env.userID)
		if err := env.rdb.Set(ctx, key, strconv.Itoa(ChannelRevealRateLimit), 0).Err(); err != nil {
			t.Fatalf("seed rate key: %v", err)
		}
		defer func() { _ = env.rdb.Del(ctx, key).Err() }()

		rr := env.reveal(t, id, map[string]any{
			"reason":   "over limit",
			"password": revealIntegrationPassword,
		})
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("状态码 = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Retry-After") == "" {
			t.Fatal("429 必须带 Retry-After")
		}
		if strings.Contains(rr.Body.String(), revealIntegrationSecret) {
			t.Fatalf("429 响应包含凭据明文：%s", rr.Body.String())
		}
	})

	t.Run("missing_channel_is_404", func(t *testing.T) {
		rr := env.reveal(t, "9223372036854775807", map[string]any{
			"reason":   "missing",
			"password": revealIntegrationPassword,
		})
		if rr.Code != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404（body=%s）", rr.Code, rr.Body.String())
		}
	})
}
