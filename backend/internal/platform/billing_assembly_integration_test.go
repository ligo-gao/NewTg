//go:build integration

package platform

// F3 装配验收：newGatewayAPI 的 gatewayAssembly 必须把**同一个**定价源接给
// 计费引擎与管理面 OnChange（R-63），并且真实 Invalidate 能让新价立即生效。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/platform/ -run TestIntegrationGatewayPricingOnChange -v

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/config"
)

// TestIntegrationGatewayPricingOnChangeInvalidatesSource 冻结 R-63 的装配：
//   - assembly.PricingSource 与管理面 OnChange 是同一 source；
//   - 缓存命中期间直接改库不会立即生效；
//   - 调用 assembly.PricingOnChange()（即注入 GatewayDeps 的那个回调）后，
//     下一次 Resolve 读到新价。
func TestIntegrationGatewayPricingOnChangeInvalidatesSource(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	redisAddr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if dsn == "" || logsDSN == "" || redisAddr == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN / NEWTG_TEST_REDIS_ADDR 未设置，跳过装配集成测试")
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

	// 定价/默认分组依赖的种子行（与 billing 集成测试同口径，幂等）。
	if _, err := mainDB.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}
	model := fmt.Sprintf("t9-onchange-%d", time.Now().UnixNano())
	if _, err := mainDB.Exec(
		`INSERT INTO model_pricing (model, mode, prompt_price, author, status)
		 VALUES ($1, 1, 100, 't9-test', 1)`, model); err != nil {
		t.Fatalf("insert model_pricing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = mainDB.Exec(`DELETE FROM model_pricing WHERE model = $1`, model)
	})

	cfg := &config.Config{
		SessionSecret:           "billing-assembly-session-secret",
		CredentialEncryptionKey: "billing-assembly-credential-key",
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
	// W7/M4-a 生产装配断言：自动禁用 reporter 与停机排空钩子都必须存在。
	// 缺 reporter 会让中继的自动禁用接缝静默退化；缺 Stop 会让停机排空
	// 写入撞上正在关闭的连接池（I-2）。
	if assembly.AttemptFailureReporter == nil {
		t.Fatal("assembly.AttemptFailureReporter = nil, want *channelhealth.Reporter")
	}
	if assembly.AttemptFailureReporterStop == nil {
		t.Fatal("assembly.AttemptFailureReporterStop = nil, want cancel+Wait hook")
	}
	// 本用例的 ctx 永不取消；显式停掉 reporter，避免测试进程泄漏 goroutine。
	t.Cleanup(assembly.AttemptFailureReporterStop)
	if assembly.Billing == nil {
		t.Fatal("assembly.Billing = nil, want engine.NewSession")
	}
	if assembly.ModelStatus == nil {
		t.Fatal("assembly.ModelStatus = nil, want the pricing repo")
	}
	if assembly.PricingSource == nil || assembly.PricingOnChange == nil {
		t.Fatalf("pricing wiring incomplete: source_nil=%v onChange_nil=%v, want both non-nil",
			assembly.PricingSource == nil, assembly.PricingOnChange == nil)
	}

	ctx := context.Background()
	resolvePrice := func() float64 {
		t.Helper()
		snap, err := assembly.PricingSource.Resolve(ctx, model, "default")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		return snap.Price.ModelPrice
	}
	if got := resolvePrice(); got != 100 {
		t.Fatalf("initial price = %v, want 100", got)
	}

	// 直接改库：缓存命中期间（30s TTL 内）必须仍读旧价。
	if _, err := mainDB.Exec(
		`UPDATE model_pricing SET prompt_price = 200, updated_at = now() WHERE model = $1`, model); err != nil {
		t.Fatalf("update model_pricing: %v", err)
	}
	if got := resolvePrice(); got != 100 {
		t.Fatalf("cached price = %v, want 100（30s 缓存应命中；测试必须在 TTL 内完成）", got)
	}

	// 管理面写成功后的回调：生产由 PricingHandlerConfig.OnChange 触发。
	assembly.PricingOnChange()
	if got := resolvePrice(); got != 200 {
		t.Fatalf("price after invalidation = %v, want 200（OnChange 必须清空定价缓存）", got)
	}
}
