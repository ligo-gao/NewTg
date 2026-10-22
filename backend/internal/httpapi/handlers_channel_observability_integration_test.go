//go:build integration

package httpapi

// W16-1：渠道观测与测试面的真实 PG + 日志库 + Redis 验收。
//
// 单测用 fake 证明了 handler 契约；本文件把真实的 store.ChannelMetricsRepo
// （日志库 usage_logs）与 Redis 缓存/限流接到 handler 上，证明：
//  1. GET /api/channels/{id}/metrics 的 24 点/跨渠道数据来自真日志库；
//  2. 第二次请求命中 Redis 缓存键（newtg:channel_metrics:<id>:<window>:1h）；
//  3. POST /api/channels/{id}/test 第 11 次 429 + Retry-After，且不同管理员互不影响。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w16a?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w16a_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration -p 1 ./internal/httpapi -run TestChannelObservabilityIntegration -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

func openW16LogsDBForHTTP(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过 W16-1 handler 集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	return db
}

func openW16Redis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过 W16-1 handler 集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return rdb
}

func insertW16Channel(t *testing.T, db *sql.DB, sealer *SecretSealer) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			model_mapping, weight, priority, status, auto_ban)
		VALUES ($1, 'openai', 'https://upstream.example.com', $2, '["gpt-4o"]'::jsonb,
			'{}'::jsonb, 1, 0, 1, true)
		RETURNING id`,
		fmt.Sprintf("w16-obs-%d", time.Now().UnixNano()),
		mustSealCredentials(t, sealer, ChannelCredentials{APIKey: "sk-w16-obs-1234"}),
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

func insertW16UsageLogRow(t *testing.T, db *sql.DB, channelID int64, at time.Time, status int16) {
	t.Helper()
	month := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	next := month.AddDate(0, 1, 0)
	if _, err := db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS usage_logs_%s PARTITION OF usage_logs FOR VALUES FROM ('%s') TO ('%s')`,
		month.Format("200601"), month.Format(time.RFC3339), next.Format(time.RFC3339))); err != nil {
		t.Fatalf("ensure partition: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO usage_logs (request_id, user_id, channel_id, model, status, latency_ms, cost, created_at)
		VALUES ($1, $2, $3, 'gpt-4o', $4, 100, 5, $5)`,
		uuid.NewString(), channelID, channelID, status, at.UTC()); err != nil {
		t.Fatalf("insert usage_log: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM usage_logs WHERE channel_id = $1`, channelID) })
}

// TestChannelMetricsEndpointIntegration 走真实日志库 + Redis 缓存。
func TestChannelMetricsEndpointIntegration(t *testing.T) {
	mainDB := openChannelPatchTestDB(t)
	logsDB := openW16LogsDBForHTTP(t)
	rdb := openW16Redis(t)
	sealer := NewSecretSealer(testSealerMaster)

	channelID := insertW16Channel(t, mainDB, sealer)
	now := time.Now().UTC()
	insertW16UsageLogRow(t, logsDB, channelID, now.Add(-1*time.Hour), 1)
	insertW16UsageLogRow(t, logsDB, channelID, now.Add(-2*time.Hour), 1)
	insertW16UsageLogRow(t, logsDB, channelID, now.Add(-3*time.Hour), 2)

	ctx := context.Background()
	_ = rdb.Del(ctx, channelMetricsCacheKey(channelID, "24h")).Err()

	h := NewChannelHandler(store.NewChannelRepo(mainDB), newFakeUserAccess(), sealer,
		ChannelHandlerOptions{
			Idempotency: NewMemoryIdempotencyStore(),
			Metrics:     store.NewChannelMetricsRepo(logsDB),
			Redis:       rdb,
		})
	mux := http.NewServeMux()
	h.Register(mux)

	get := func() *httptest.ResponseRecorder {
		return serveWithPrincipal(t, mux,
			httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channels/%d/metrics", channelID), nil),
			channelAdminPrincipal(PermChannelsRead))
	}

	rr := get()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	var body channelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if len(body.Points) != 24 {
		t.Fatalf("points = %d, want 24", len(body.Points))
	}
	if body.Summary.RequestCount != 3 || body.Summary.SuccessCount != 2 || body.Summary.FailureCount != 1 {
		t.Fatalf("summary = %+v, want requests=3 success=2 failure=1", body.Summary)
	}

	key := channelMetricsCacheKey(channelID, "24h")
	if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 1 {
		t.Fatalf("cache key %q exists=%d err=%v, want 1", key, n, err)
	}
	if ttl := rdb.TTL(ctx, key).Val(); ttl <= 0 || ttl > channelMetricsCacheTTL {
		t.Fatalf("cache TTL = %s, want (0, %s]", ttl, channelMetricsCacheTTL)
	}

	// 第二次必须命中缓存（日志库仍在，但缓存键存在是命中的充分条件；
	// 查询次数由单测的 fake 计数严格证明）。
	if rr := get(); rr.Code != http.StatusOK {
		t.Fatalf("second status = %d", rr.Code)
	}

	// 越权 403。
	if rr := serveWithPrincipal(t, mux,
		httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channels/%d/metrics", channelID), nil),
		channelAdminPrincipal(PermChannelsManage)); rr.Code != http.StatusForbidden {
		t.Fatalf("without channels:read status = %d, want 403", rr.Code)
	}
}

// TestChannelTestRateLimitIntegration 走真实 Redis：10 次放行、第 11 次
// 429 + Retry-After，且另一个管理员自己的桶不受影响。
func TestChannelTestRateLimitIntegration(t *testing.T) {
	mainDB := openChannelPatchTestDB(t)
	rdb := openW16Redis(t)
	sealer := NewSecretSealer(testSealerMaster)
	channelID := insertW16Channel(t, mainDB, sealer)

	ctx := context.Background()
	_ = rdb.Del(ctx, ChannelTestRateLimitKey(testAdminID), ChannelTestRateLimitKey(2)).Err()
	t.Cleanup(func() {
		_ = rdb.Del(ctx, ChannelTestRateLimitKey(testAdminID), ChannelTestRateLimitKey(2)).Err()
	})

	tester := &fakeChannelTester{latency: 5}
	h := NewChannelHandler(store.NewChannelRepo(mainDB), newFakeUserAccess(), sealer,
		ChannelHandlerOptions{
			Idempotency: NewMemoryIdempotencyStore(),
			Redis:       rdb,
			Tester:      tester.test,
		})
	mux := http.NewServeMux()
	h.Register(mux)

	doTest := func(p *auth.Principal) *httptest.ResponseRecorder {
		return serveWithPrincipal(t, mux,
			jsonReq(t, http.MethodPost, fmt.Sprintf("/api/channels/%d/test", channelID),
				map[string]any{"model": "gpt-4o"}),
			p)
	}

	admin := channelAdminPrincipal(PermChannelsManage)
	for i := 0; i < ChannelTestRateLimit; i++ {
		if rr := doTest(admin); rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d (body=%s)", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := doTest(admin)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("11th status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}

	otherAdmin := &auth.Principal{UserID: 2, Role: "admin", Permissions: []string{PermChannelsManage}}
	if rr := doTest(otherAdmin); rr.Code != http.StatusOK {
		t.Fatalf("other admin status = %d, want 200 (per-admin bucket)", rr.Code)
	}
}
