//go:build integration

package httpapi

// W13-DEMO 集成验收：真实 PostgreSQL（主库 + 日志库）+ Redis + 真实计费引擎。
//
// 运行（与其它 W 系列同一套环境变量）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w13d?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w13d_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	go test -tags=integration -p 1 ./internal/httpapi/ -run TestIntegrationW13d -v
//
// 断言重点（§3.17.2）：
//   - 第 21 次同 IP 请求 429 + Retry-After，且被拒请求不写 usage_logs、
//     不动余额、不触达上游；
//   - Redis 键形状 demo:relay:{yyyymmdd}:ip:{sha256} 与 :global，均带 TTL
//     （跨零点自动归零，无定时任务）；
//   - 站点时区决定 yyyymmdd（默认 Asia/Shanghai）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/store"
)

func TestIntegrationW13dDemoRelayQuota(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	group := "w13d_" + suffix
	model := "w13d-model-" + suffix
	m2SeedGroup(t, mainDB, group, 100, 1)
	m2SeedModel(t, mainDB, model, 100)
	userID := m2SeedUser(t, mainDB, group, 1_000_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	_, plain := m2SeedToken(t, mainDB, userID, group, `[]`, false, nil, nil)
	upstream := m2Counter(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	m2SeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "w13d-"+suffix, group, upstream.server.URL, model)

	cfg := m2BillingConfig{
		keyDemoEnabled:          json.RawMessage("true"),
		keyDemoRelayPerIPDaily:  json.RawMessage("20"),
		keyDemoRelayGlobalDaily: json.RawMessage("2000"),
		// B10/B11/B12 关闭：本用例只验证演示额度。
		ratelimit.KeyUserModelRPM:  json.RawMessage("0"),
		ratelimit.KeyTokenRPM:      json.RawMessage("0"),
		ratelimit.KeyUserGlobalRPM: json.RawMessage("0"),
	}
	// 清理本用例的演示键（日期由站点时区决定，这里用模式删除兜底）。
	cleanup := func() {
		ctx := context.Background()
		keys, _ := rdb.Keys(ctx, "demo:relay:*").Result()
		if len(keys) > 0 {
			_, _ = rdb.Del(ctx, keys...).Result()
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	tokenRepo := store.NewTokenRepo(mainDB)
	pricingRepo := store.NewPricingRepo(mainDB)
	handler := NewRelayHandler(RelayHandlerOptions{
		Channels:        store.NewChannelRepo(mainDB),
		Tokens:          tokenRepo,
		Access:          tokenRepo,
		Sealer:          NewSecretSealer(relaySealerMaster),
		ModelStatus:     pricingRepo,
		GroupConfigured: pricingRepo.GroupConfigured,
		Client:          &http.Client{Timeout: 10 * time.Second},
		Billing:         m2NewEngine(t, gormDB, logsDB, rdb).NewSession,
		Config:          cfg,
		RateLimiter:     ratelimit.New(rdb),
	})

	const ip = "203.0.113.88"
	for i := 0; i < 20; i++ {
		w := m2Request(t, handler, plain, model, ip+":1")
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
		}
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 20 {
		t.Fatalf("20 次成功后 usage_logs = %d，期望 20", len(rows))
	}
	balanceAfter20 := m2Balance(t, mainDB, userID)
	if balanceAfter20 >= 1_000_000 {
		t.Fatalf("20 次成功应有真实扣费，余额 = %d", balanceAfter20)
	}

	// 第 21 次：429 + Retry-After，且零计费副作用。
	w := m2Request(t, handler, plain, model, ip+":1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 21 次 = %d，期望 429；body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After 缺失")
	}
	if envelope := decodeRelayError(t, w.Body.Bytes()); envelope.Error.Code != demoRelayQuotaExceededCode {
		t.Fatalf("code = %q，期望 %s", envelope.Error.Code, demoRelayQuotaExceededCode)
	}
	if got := len(m2UsageRows(t, logsDB, userID)); got != 20 {
		t.Fatalf("被拒请求写入 usage_logs：行数 = %d，期望 20", got)
	}
	if got := m2Balance(t, mainDB, userID); got != balanceAfter20 {
		t.Fatalf("被拒请求改变余额：%d → %d", balanceAfter20, got)
	}
	if got := upstream.count(); got != 20 {
		t.Fatalf("被拒请求触达上游：调用数 = %d，期望 20", got)
	}

	// Redis 键形状 + TTL（到次日 00:00 的剩余时间，必须 > 0 且 ≤ 25h）。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	date := time.Now().In(loc).Format("20060102")
	ipKey := fmt.Sprintf("demo:relay:%s:ip:%s", date, demoIPHash(ip))
	globalKey := fmt.Sprintf("demo:relay:%s:global", date)
	if got, err := rdb.Get(context.Background(), ipKey).Int64(); err != nil || got != 21 {
		t.Fatalf("per-IP 计数 = %d (err=%v)，期望 21（被拒请求也自增，与 B 池同口径）", got, err)
	}
	for _, key := range []string{ipKey, globalKey} {
		ttl, err := rdb.TTL(context.Background(), key).Result()
		if err != nil {
			t.Fatalf("TTL %s: %v", key, err)
		}
		if ttl <= 0 || ttl > 25*time.Hour {
			t.Fatalf("%s TTL = %v，期望 (0, 25h]（次日 00:00 过期）", key, ttl)
		}
	}
	if strings.Contains(ipKey, ip) {
		t.Fatalf("Redis 键含明文 IP：%q", ipKey)
	}
}

// ---------------------------------------------------------------------------
// I1：演示账号「不计入任何用户账单」（§3.17.2 / 控制端裁决 I1）
// ---------------------------------------------------------------------------

// w13dNewEngine 构造带演示开关的真实计费引擎（真实 PG + Redis）。
func w13dNewEngine(t *testing.T, gormDB *gorm.DB, logsDB *sql.DB, rdb *redis.Client,
	cfg m2BillingConfig,
) *billing.Engine {
	t.Helper()
	source, err := pricing.NewSource(gormDB, nil)
	if err != nil {
		t.Fatalf("pricing.NewSource: %v", err)
	}
	return billing.NewEngine(gormDB, logsDB, rdb, cfg, source,
		billing.NewRegistry(rdb), billing.NewDBLogger(logsDB))
}

// w13dBillingCase 是 I1 的最小端到端装置：一个用户 + 一个 unlimited 令牌
// + 一个渠道 + 真实计费引擎。
type w13dBillingCase struct {
	mainDB   *sql.DB
	logsDB   *sql.DB
	rdb      *redis.Client
	handler  *RelayHandler
	userID   int64
	plain    string
	model    string
	upstream *m2CounterUpstream
}

func newW13dBillingCase(t *testing.T, demo bool, upstreamStatus int) *w13dBillingCase {
	t.Helper()
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	group := "w13dfix_" + suffix
	model := "w13dfix-model-" + suffix
	m2SeedGroup(t, mainDB, group, 100, 1)
	m2SeedModel(t, mainDB, model, 100)
	userID := m2SeedUser(t, mainDB, group, 1_000_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	_, plain := m2SeedToken(t, mainDB, userID, group, `[]`, false, nil, nil)
	upstream := m2Counter(t, upstreamStatus, `{"id":"ok","object":"chat.completion","choices":[]}`)
	m2SeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "w13dfix-"+suffix, group, upstream.server.URL, model)

	cfg := m2BillingConfig{
		keyDemoEnabled:                json.RawMessage(strconv.FormatBool(demo)),
		keyDemoRelayPerIPDaily:        json.RawMessage("1000"),
		keyDemoRelayGlobalDaily:       json.RawMessage("10000"),
		billing.ConfigPreConsumeRatio: json.RawMessage("1.0"),
		billing.ConfigTrustQuotaUSD:   json.RawMessage("0"),
		// 本用例只验证计费口径，关闭 B10/B11/B12。
		ratelimit.KeyUserModelRPM:  json.RawMessage("0"),
		ratelimit.KeyTokenRPM:      json.RawMessage("0"),
		ratelimit.KeyUserGlobalRPM: json.RawMessage("0"),
	}
	engine := w13dNewEngine(t, gormDB, logsDB, rdb, cfg)
	tokenRepo := store.NewTokenRepo(mainDB)
	pricingRepo := store.NewPricingRepo(mainDB)
	handler := NewRelayHandler(RelayHandlerOptions{
		Channels:        store.NewChannelRepo(mainDB),
		Tokens:          tokenRepo,
		Access:          tokenRepo,
		Sealer:          NewSecretSealer(relaySealerMaster),
		ModelStatus:     pricingRepo,
		GroupConfigured: pricingRepo.GroupConfigured,
		Client:          &http.Client{Timeout: 10 * time.Second},
		Billing:         engine.NewSession,
		Config:          cfg,
		RateLimiter:     ratelimit.New(rdb),
	})
	cleanup := func() {
		ctx := context.Background()
		keys, _ := rdb.Keys(ctx, "demo:relay:*").Result()
		if len(keys) > 0 {
			_, _ = rdb.Del(ctx, keys...).Result()
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	return &w13dBillingCase{
		mainDB: mainDB, logsDB: logsDB, rdb: rdb, handler: handler,
		userID: userID, plain: plain, model: model, upstream: upstream,
	}
}

func w13dHoldRows(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND status = 'hold'`, userID).Scan(&n); err != nil {
		t.Fatalf("count hold rows: %v", err)
	}
	return n
}

func TestIntegrationW13dFixDemoBilling(t *testing.T) {
	t.Run("demo unlimited token is not billed", func(t *testing.T) {
		c := newW13dBillingCase(t, true, http.StatusOK)
		const ip = "203.0.113.111"
		for i := 0; i < 20; i++ {
			w := m2Request(t, c.handler, c.plain, c.model, ip+":1")
			if w.Code != http.StatusOK {
				t.Fatalf("第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
			}
		}
		if rows := m2UsageRows(t, c.logsDB, c.userID); len(rows) != 20 {
			t.Fatalf("usage_logs = %d，期望 20（演示站仍要照常记日志）", len(rows))
		}
		if got := m2Balance(t, c.mainDB, c.userID); got != 1_000_000 {
			t.Fatalf("演示模式 + unlimited 令牌余额 = %d，期望不变 1000000", got)
		}
		if got := m2WalletRows(t, c.mainDB, c.userID); got != 0 {
			t.Fatalf("钱包流水 = %d 行，期望 0（不得写钱包流水）", got)
		}
		if got := w13dHoldRows(t, c.mainDB, c.userID); got != 0 {
			t.Fatalf("悬挂 hold 行 = %d，期望 0", got)
		}
		if got := c.upstream.count(); got != 20 {
			t.Fatalf("上游调用 = %d，期望 20", got)
		}
	})

	t.Run("non-demo unlimited token is still billed", func(t *testing.T) {
		// 关闭演示模式：同样 unlimited 令牌必须按正常规则扣用户余额——
		// 锁定豁免面，防止把 demo 口径扩大成 unlimited_quota 的通用语义。
		c := newW13dBillingCase(t, false, http.StatusOK)
		const ip = "203.0.113.112"
		for i := 0; i < 3; i++ {
			if w := m2Request(t, c.handler, c.plain, c.model, ip+":1"); w.Code != http.StatusOK {
				t.Fatalf("第 %d 次 = %d，期望 200；body=%s", i+1, w.Code, w.Body.String())
			}
		}
		if got := m2Balance(t, c.mainDB, c.userID); got >= 1_000_000 {
			t.Fatalf("非演示模式余额 = %d，期望按正常规则扣减", got)
		}
		if got := m2WalletRows(t, c.mainDB, c.userID); got == 0 {
			t.Fatal("非演示模式应产生钱包流水")
		}
		if got := w13dHoldRows(t, c.mainDB, c.userID); got != 0 {
			t.Fatalf("结算后悬挂 hold 行 = %d，期望 0", got)
		}
	})

	t.Run("demo upstream failure leaves no hold", func(t *testing.T) {
		c := newW13dBillingCase(t, true, http.StatusInternalServerError)
		w := m2Request(t, c.handler, c.plain, c.model, "203.0.113.113:1")
		if w.Code == http.StatusOK {
			t.Fatalf("上游 500 时不应得到 200；body=%s", w.Body.String())
		}
		if got := m2Balance(t, c.mainDB, c.userID); got != 1_000_000 {
			t.Fatalf("失败路径改变余额：%d，期望 1000000", got)
		}
		if got := m2WalletRows(t, c.mainDB, c.userID); got != 0 {
			t.Fatalf("失败路径钱包流水 = %d 行，期望 0", got)
		}
		if got := w13dHoldRows(t, c.mainDB, c.userID); got != 0 {
			t.Fatalf("失败路径悬挂 hold = %d，期望 0", got)
		}
	})
}
