//go:build integration

package httpapi

// W7/M5-b 集成验收：真实 PostgreSQL（主库 + 日志库）+ Redis + 真实计费引擎。
//
// 单元测试（relay_rate_limit_test.go）用 fake 计费断言「429 不调用 PreConsume」；
// 这里补上真正的资金副作用：被 429 拒绝的请求不得写 usage_logs、不得动余额、
// 不得触达上游。运行：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_m5b \
//	NEWTG_TEST_LOGS_DSN=postgres://newtg:newtg@localhost:55432/newtg_dom_m5b_logs \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationM5b -v

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"newtg/gateway/internal/ratelimit"
	"newtg/gateway/internal/store"
)

// TestIntegrationM5bRateLimitRejectsBeforeBilling 覆盖 R-M5-04/05/09/10 的资金面：
//   - 第 1 次请求正常计费（成功行 + 余额扣减）；
//   - 第 2 次被 B10 拒绝 → 429 + Retry-After，余额不变、usage_logs 不增、
//     上游不再被调用；
//   - Redis 键计数为 2（被拒请求本身已在 B10 自增，口径含失败请求）。
func TestIntegrationM5bRateLimitRejectsBeforeBilling(t *testing.T) {
	mainDB, logsDB, gormDB := m2TestDBs(t)
	rdb := m2TestRedis(t)
	suffix := m2Suffix()
	group := "m5b_" + suffix
	model := "m5b-model-" + suffix
	m2SeedGroup(t, mainDB, group, 100, 1)
	m2SeedModel(t, mainDB, model, 100)
	userID := m2SeedUser(t, mainDB, group, 10_000)
	m2CleanupUsageLogs(t, logsDB, userID)
	_, plain := m2SeedToken(t, mainDB, userID, group, `[]`, false, nil, nil)
	upstream := m2Counter(t, http.StatusOK, `{"id":"ok","object":"chat.completion","choices":[]}`)
	m2SeedChannel(t, mainDB, NewSecretSealer(relaySealerMaster), "m5b-"+suffix, group, upstream.server.URL, model)

	// 只有 B10 开启（1 次/分钟）；B11/B12 缺键 → 注册表默认 0 = 不限。
	cfg := m2BillingConfig{
		ratelimit.KeyUserModelRPM: json.RawMessage("1"),
	}
	t.Cleanup(func() { _, _ = rdb.Del(t.Context(), ratelimit.UserKey(userID)).Result() })

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
		// 生产装配同款：复用同一个 Redis 客户端。
		RateLimiter: ratelimit.New(rdb),
	})

	// 第 1 次：正常放行并完成真实预扣/结算。
	if w := m2Request(t, handler, plain, model, "203.0.113.9:1"); w.Code != http.StatusOK {
		t.Fatalf("第 1 次 status = %d，期望 200；body=%s", w.Code, w.Body.String())
	}
	rows := m2UsageRows(t, logsDB, userID)
	if len(rows) != 1 {
		t.Fatalf("第 1 次 usage_logs = %d，期望 1", len(rows))
	}
	balanceAfterFirst := m2Balance(t, mainDB, userID)
	if balanceAfterFirst != 9_900 {
		t.Fatalf("第 1 次后余额 = %d，期望 9900", balanceAfterFirst)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("第 1 次上游调用 = %d，期望 1", got)
	}

	// 第 2 次：B10 超限 → 429，零资金/零流水/零上游。
	w := m2Request(t, handler, plain, model, "203.0.113.9:1")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次 status = %d，期望 429；body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 缺少 Retry-After")
	}
	if got := decodeRelayError(t, w.Body.Bytes()).Error.Code; got != "rate_limit_exceeded" {
		t.Fatalf("code = %q，期望 rate_limit_exceeded", got)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("429 之后上游调用 = %d，期望仍为 1", got)
	}
	if rows := m2UsageRows(t, logsDB, userID); len(rows) != 1 {
		t.Fatalf("429 之后 usage_logs = %d，期望仍为 1（不得预扣/落流水）", len(rows))
	}
	if balance := m2Balance(t, mainDB, userID); balance != balanceAfterFirst {
		t.Fatalf("429 之后余额 = %d，期望不变（%d）", balance, balanceAfterFirst)
	}
	// 被拒请求本身已计入 B10（计数口径含失败请求）。
	if got, err := rdb.Get(t.Context(), ratelimit.UserKey(userID)).Int64(); err != nil || got != 2 {
		t.Fatalf("B10 计数 = %d（err=%v），期望 2", got, err)
	}
}
