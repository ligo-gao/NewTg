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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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
