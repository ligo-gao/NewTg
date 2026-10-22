//go:build integration

package platform

// W13-EMAIL 装配验收：生产 newGatewayAPI 路径必须注入非 nil 的事务性邮件
// 适配器；全新库没有 notify.smtp.* 配置，因此验证码/密码重置端点必须
// fail-closed 503（email_unavailable），而不是 204 假成功。
//
// 运行（测试库按任务约定自建 newtg_dom_w13e / newtg_dom_w13e_logs）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w13e?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w13e_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration -p 1 ./internal/platform/ -run TestIntegrationGatewayEmailAssembly -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/config"
)

func TestIntegrationGatewayEmailAssembly(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if dsn == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过邮件装配集成测试")
	}

	mainDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	t.Cleanup(func() { _ = mainDB.Close() })
	logsDB, err := sql.Open("pgx", logsDSN)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })
	if err := mainDB.Ping(); err != nil {
		t.Fatalf("ping main db: %v", err)
	}
	if err := logsDB.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}

	cfg := &config.Config{
		SessionSecret:           "w13-email-session-secret",
		CredentialEncryptionKey: "w13-email-credential-key",
		PublicBaseURL:           "http://localhost:3000",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	assembly, err := newGatewayAPI(context.Background(), cfg, mainDB, logsDB, rdb, logger)
	if err != nil {
		t.Fatalf("newGatewayAPI: %v", err)
	}
	if assembly == nil || assembly.Handler == nil {
		t.Fatal("assembly/handler = nil, want the management router")
	}
	t.Cleanup(assembly.AttemptFailureReporterStop)

	// 装配断言：去掉 platform 注入会让这个用例直接红（W13 变异基线）。
	if assembly.EmailSender == nil {
		t.Fatal("assembly.EmailSender = nil, want the transactional email adapter")
	}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	post := func(path, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		assembly.Handler.ServeHTTP(rec, req)
		var problem map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &problem)
		return rec, problem
	}

	// 全新库无 notify.smtp.* → 验证码端点 503 + 固定 problem type。
	rec, problem := post("/api/auth/email-code",
		`{"email":"w13-`+suffix+`@example.com","purpose":"register"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("email-code = %d, want 503（body=%q）", rec.Code, rec.Body.String())
	}
	if got, _ := problem["type"].(string); !strings.HasSuffix(got, "/email_unavailable") {
		t.Fatalf("email-code problem.type = %v, want email_unavailable", problem["type"])
	}

	// 密码重置同样 fail-closed，且对不存在的邮箱也 503（不泄露存在性）。
	rec, problem = post("/api/auth/password-reset/request",
		`{"email":"nobody-`+suffix+`@example.com"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("password-reset = %d, want 503（body=%q）", rec.Code, rec.Body.String())
	}
	if got, _ := problem["type"].(string); !strings.HasSuffix(got, "/email_unavailable") {
		t.Fatalf("password-reset problem.type = %v, want email_unavailable", problem["type"])
	}
}
