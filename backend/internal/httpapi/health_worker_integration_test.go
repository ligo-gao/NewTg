//go:build integration

package httpapi

// W7/M4-b：健康 worker → 共享探测 → 真实 PostgreSQL/Redis 的端到端验收。
//
// 覆盖 brief §3 的真库断言：
//   - enabled + 401 → auto_disabled（reason/审计/站内通知）；
//   - auto_disabled + 成功 + auto_enable → enabled（reason/fail_count 清空、
//     channel.auto_enable 审计）；
//   - manual_disabled 永不被恢复；
//   - 无 adapter 的渠道类型整轮跳过、不改 last_test_*。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_m4_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	  go test -tags=integration -p 1 ./internal/httpapi/ -run TestChannelHealthWorkerIntegration -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

func openWorkerTestDB(t *testing.T, env string) *sql.DB {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skip(env + " 未设置，跳过健康 worker 集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", env, err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping %s: %v", env, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// workerTestRedis 优先使用真实测试 Redis；未配置时退化为进程内 miniredis。
func workerTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	if addr := os.Getenv("NEWTG_TEST_REDIS_ADDR"); addr != "" {
		client := redis.NewClient(&redis.Options{Addr: addr})
		if err := client.Ping(context.Background()).Err(); err != nil {
			t.Fatalf("ping test redis: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })
		_ = client.Del(context.Background(), channelhealth.ChannelHealthCheckLockKey).Err()
		return client
	}
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type workerItFixture struct {
	db       *sql.DB
	logsDB   *sql.DB
	repo     *store.ChannelRepo
	notifier *notify.Service
	auditor  *audit.Logger
	redis    *redis.Client
	upstream *httptest.Server
	serverMu atomic.Int64 // 0 = 401, 1 = 200
}

func newWorkerItFixture(t *testing.T) *workerItFixture {
	t.Helper()
	f := &workerItFixture{
		db:     openWorkerTestDB(t, "NEWTG_TEST_DB_DSN"),
		logsDB: openWorkerTestDB(t, "NEWTG_TEST_LOGS_DSN"),
		redis:  workerTestRedis(t),
	}
	f.repo = store.NewChannelRepo(f.db)
	notifyStore, err := notify.NewStore(f.db)
	if err != nil {
		t.Fatalf("notify.NewStore: %v", err)
	}
	svc, err := notify.NewService(notify.ServiceConfig{
		Store:  notifyStore,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("notify.NewService: %v", err)
	}
	if err := svc.EnsureSeeded(context.Background()); err != nil {
		t.Fatalf("seed notification templates: %v", err)
	}
	f.notifier = svc
	f.auditor = audit.New(f.logsDB)

	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch f.serverMu.Load() {
		case 1: // 恢复
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		case 2: // 403
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"forbidden"}}`)
		case 3: // 429 + 默认关键词
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, "Your credit balance is too low")
		default: // 401
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
		}
	}))
	t.Cleanup(f.upstream.Close)
	return f
}

func (f *workerItFixture) insertChannel(t *testing.T, typ string, status int16, autoBan bool) int64 {
	t.Helper()
	name := fmt.Sprintf("m4b-it-%d-%d", time.Now().UnixNano(), status)
	var id int64
	err := f.db.QueryRow(`
		INSERT INTO channels (name, type, base_url, credentials, supported_models,
			model_mapping, weight, priority, status, auto_ban)
		VALUES ($1, $2, $3, $4, '["gpt-test"]'::jsonb, '{}'::jsonb, 1, 0, $5, $6)
		RETURNING id`,
		name, typ, f.upstream.URL, sealedTestCredentials(t, NewSecretSealer(relaySealerMaster)),
		status, autoBan).Scan(&id)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() { _, _ = f.db.Exec(`DELETE FROM channels WHERE id = $1`, id) })
	return id
}

func (f *workerItFixture) worker(t *testing.T, settings map[string]any) *channelhealth.Worker {
	t.Helper()
	// 渠道凭据由 relaySealerMaster 封装；prober 必须用同一把 sealer。
	prober, err := channelhealth.NewProber(channelhealth.ProberConfig{
		Opener:  NewSecretSealer(relaySealerMaster),
		Client:  f.upstream.Client(),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewProber: %v", err)
	}
	worker, err := channelhealth.NewWorker(channelhealth.WorkerConfig{
		Store:       f.repo,
		Config:      autoDisableConfig(rawSettings(t, settings)),
		Prober:      prober,
		Locker:      channelhealth.NewRedisLocker(f.redis),
		Audit:       f.auditor,
		Notifier:    f.notifier,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		ChannelsURL: "/admin/channels",
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return worker
}

func rawSettings(t *testing.T, values map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		out[key] = raw
	}
	return out
}

func (f *workerItFixture) channelRow(t *testing.T, id int64) *store.Channel {
	t.Helper()
	channel, err := f.repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get channel %d: %v", id, err)
	}
	return channel
}

func (f *workerItFixture) auditCount(t *testing.T, id int64, action string) int {
	t.Helper()
	var count int
	err := f.logsDB.QueryRow(`
		SELECT count(*) FROM audit_logs
		WHERE detail->>'action' = $1 AND detail->>'channel_id' = $2`,
		action, strconv.FormatInt(id, 10)).Scan(&count)
	if err != nil {
		t.Fatalf("count audits: %v", err)
	}
	return count
}

func (f *workerItFixture) deliveryCount(t *testing.T, id int64) int {
	t.Helper()
	var count int
	err := f.db.QueryRow(`
		SELECT count(*) FROM notification_deliveries
		WHERE channel = 'inapp' AND category = 'ops.channel_auto_disabled'
		  AND dedupe_key LIKE $1`,
		fmt.Sprintf("channel_auto_disabled:%d:%%", id)).Scan(&count)
	if err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return count
}

// deliveryCountFor 只统计本用例创建的管理员，避免对集成库中既有 admin 人口敏感。
func (f *workerItFixture) deliveryCountFor(t *testing.T, id, userID int64) int {
	t.Helper()
	var count int
	err := f.db.QueryRow(`
		SELECT count(*) FROM notification_deliveries
		WHERE channel = 'inapp' AND category = 'ops.channel_auto_disabled'
		  AND recipient_user_id = $2 AND dedupe_key LIKE $1`,
		fmt.Sprintf("channel_auto_disabled:%d:%%", id), userID).Scan(&count)
	if err != nil {
		t.Fatalf("count deliveries for user: %v", err)
	}
	return count
}

// TestChannelHealthWorkerIntegrationDisableThenRecover 走完整一轮：401 → 禁用
// （审计 + 站内通知）→ 上游恢复 → 自动恢复（reason/fail_count 清空 + 审计）。
func TestChannelHealthWorkerIntegrationDisableThenRecover(t *testing.T) {
	f := newWorkerItFixture(t)
	adminID := insertWorkerTestsAdmin(t, f.db)
	channelID := f.insertChannel(t, "openai", 1, true)
	t.Cleanup(func() {
		_, _ = f.db.Exec(`DELETE FROM notification_deliveries
			WHERE dedupe_key LIKE $1`, fmt.Sprintf("channel_auto_disabled:%d:%%", channelID))
	})

	// 第一轮：上游 401 → 自动禁用（开关开启）。
	worker := f.worker(t, map[string]any{
		channelhealth.KeyAutoDisableEnabled: true,
	})
	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce(disable): %v", err)
	}
	if stats.Tested != 1 || stats.Disabled != 1 {
		t.Fatalf("第一轮 stats = %+v, want tested=1 disabled=1", stats)
	}
	disabled := f.channelRow(t, channelID)
	if disabled.Status != 3 || disabled.AutoDisabledReason == nil ||
		*disabled.AutoDisabledReason != channelhealth.ReasonAuthError {
		t.Fatalf("channel = status %d reason %v, want 3/%s",
			disabled.Status, disabled.AutoDisabledReason, channelhealth.ReasonAuthError)
	}
	if disabled.LastTestResult == nil || *disabled.LastTestResult {
		t.Fatalf("last_test_result = %v, want false", disabled.LastTestResult)
	}
	if got := f.auditCount(t, channelID, audit.ActionChannelAutoDisable); got != 1 {
		t.Fatalf("channel.auto_disable 审计 = %d, want 1", got)
	}
	if got := f.deliveryCountFor(t, channelID, adminID); got < 1 {
		t.Fatalf("本用例 admin 的站内通知 = %d, want >= 1", got)
	}
	deliveriesBeforeRecovery := f.deliveryCount(t, channelID)

	// 第二轮：上游恢复 200 + auto_enable 开启 → 自动恢复。
	f.serverMu.Store(1)
	worker = f.worker(t, map[string]any{
		channelhealth.KeyAutoDisableEnabled: true,
		channelhealth.KeyAutoEnableEnabled:  true,
	})
	stats, err = worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce(recover): %v", err)
	}
	if stats.OK != 1 || stats.Enabled != 1 {
		t.Fatalf("第二轮 stats = %+v, want ok=1 enabled=1", stats)
	}
	recovered := f.channelRow(t, channelID)
	if recovered.Status != 1 || recovered.AutoDisabledReason != nil || recovered.FailCount != 0 {
		t.Fatalf("recovered = status %d reason %v fail %d, want 1/NULL/0（R-M4-16）",
			recovered.Status, recovered.AutoDisabledReason, recovered.FailCount)
	}
	if got := f.auditCount(t, channelID, audit.ActionChannelAutoEnable); got != 1 {
		t.Fatalf("channel.auto_enable 审计 = %d, want 1", got)
	}
	// 恢复不发送通知：投递行数保持第一轮的规模。
	if got := f.deliveryCount(t, channelID); got != deliveriesBeforeRecovery {
		t.Fatalf("恢复后通知行 = %d, want %d（不得新增）", got, deliveriesBeforeRecovery)
	}
}

// TestChannelHealthWorkerIntegrationFailureReasons 覆盖真实 PG 上的另外两条
// 触发：403（ErrUpstreamAuth 含 403）与 429 + 默认关键词（R-M4-01/12）。
func TestChannelHealthWorkerIntegrationFailureReasons(t *testing.T) {
	cases := []struct {
		name       string
		mode       int64
		wantReason string
	}{
		{name: "forbidden-403", mode: 2, wantReason: channelhealth.ReasonAuthError},
		{name: "keyword-429", mode: 3, wantReason: channelhealth.ReasonKeyword},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkerItFixture(t)
			f.serverMu.Store(tc.mode)
			channelID := f.insertChannel(t, "openai", 1, true)
			t.Cleanup(func() {
				_, _ = f.db.Exec(`DELETE FROM notification_deliveries
					WHERE dedupe_key LIKE $1`,
					fmt.Sprintf("channel_auto_disabled:%d:%%", channelID))
			})
			worker := f.worker(t, map[string]any{channelhealth.KeyAutoDisableEnabled: true})
			stats, err := worker.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if stats.Disabled != 1 {
				t.Fatalf("stats = %+v, want disabled=1", stats)
			}
			channel := f.channelRow(t, channelID)
			if channel.Status != 3 || channel.AutoDisabledReason == nil ||
				*channel.AutoDisabledReason != tc.wantReason {
				t.Fatalf("channel = status %d reason %v, want 3/%s",
					channel.Status, channel.AutoDisabledReason, tc.wantReason)
			}
			if got := f.auditCount(t, channelID, audit.ActionChannelAutoDisable); got != 1 {
				t.Fatalf("channel.auto_disable 审计 = %d, want 1", got)
			}
		})
	}
}

// TestChannelHealthWorkerIntegrationManualDisabledNeverRecovers：人工禁用
// 行不在扫描集，也不会因成功探测被恢复。
func TestChannelHealthWorkerIntegrationManualDisabledNeverRecovers(t *testing.T) {
	f := newWorkerItFixture(t)
	f.serverMu.Store(1)
	channelID := f.insertChannel(t, "openai", 2, true)

	worker := f.worker(t, map[string]any{
		channelhealth.KeyAutoEnableEnabled: true,
	})
	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Tested != 0 {
		t.Fatalf("manual_disabled 被探测: stats = %+v", stats)
	}
	channel := f.channelRow(t, channelID)
	if channel.Status != 2 {
		t.Fatalf("manual_disabled status = %d, want 2", channel.Status)
	}
	if got := f.auditCount(t, channelID, audit.ActionChannelAutoEnable); got != 0 {
		t.Fatalf("manual_disabled 产生 auto_enable 审计 %d 条", got)
	}
}

// TestChannelHealthWorkerIntegrationUnsupportedSkipped：无 adapter 的类型
// 整轮跳过、不改 last_test_*（R-M4-06；变异：去掉跳过 → 本用例红）。
func TestChannelHealthWorkerIntegrationUnsupportedSkipped(t *testing.T) {
	f := newWorkerItFixture(t)
	channelID := f.insertChannel(t, "aws", 1, true)

	worker := f.worker(t, map[string]any{
		channelhealth.KeyAutoDisableEnabled: true,
	})
	stats, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.SkippedUnsupported != 1 || stats.Tested != 0 {
		t.Fatalf("stats = %+v, want skipped_unsupported=1 tested=0", stats)
	}
	channel := f.channelRow(t, channelID)
	if channel.Status != 1 {
		t.Fatalf("status = %d, want 1（不得误禁用）", channel.Status)
	}
	if channel.LastTestAt != nil || channel.LastTestResult != nil {
		t.Fatalf("无 adapter 的渠道被写了 last_test_*: %v/%v",
			channel.LastTestAt, channel.LastTestResult)
	}
}

// insertWorkerTestsAdmin 插入一个管理用户并登记清理。
func insertWorkerTestsAdmin(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	username := "m4badmin" + strings.ReplaceAll(fmt.Sprintf("%d", time.Now().UnixNano()), "-", "")
	var id int64
	if err := db.QueryRow(`
		INSERT INTO users (username, role, status, language)
		VALUES ($1, 'admin', 'enabled', 'en')
		RETURNING id`, username).Scan(&id); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}
