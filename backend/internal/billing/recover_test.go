package billing

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 本文件的回收测试使用真实 PostgreSQL（与 ledger_test.go 相同手法）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable'
//
// 未设置时跳过。Redis 侧使用 miniredis，可以观察租约清理结果。

// TestRecoverAbandonedOnlyRemainingHold 覆盖 R-39：兜底只退仍处于 hold 的
// 预扣，ChargeIncrement 的 consume 行对应已交付用量，不随崩溃退款。
func TestRecoverAbandonedOnlyRemainingHold(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 1000)
	ledger := NewLedger(gormDB)
	req := testRequestID("recover-r39")
	ctx := context.Background()

	if _, err := ledger.ApplyDelta(ctx, userID, -300, "consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := ledger.ApplyDelta(ctx, userID, -50, "consume", "confirmed", "usage_log", req); err != nil {
		t.Fatalf("consume increment: %v", err)
	}

	e := NewEngine(gormDB, nil, nil, nil, nil, nil, nil)
	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}

	if got := userBalance(t, db, userID); got != 950 {
		t.Fatalf("balance = %d, want 950（只退 hold 300，consume 50 保留）", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3", rows)
	}
	if rows[0].Type != "consume_pre" || rows[0].Status != "expired" {
		t.Fatalf("hold row = %+v, want consume_pre/expired", rows[0])
	}
	if rows[1].Type != "consume" || rows[1].Status != "confirmed" {
		t.Fatalf("increment row = %+v, want consume/confirmed", rows[1])
	}
	if rows[2].Type != "release" || rows[2].Amount != 300 || rows[2].Status != "confirmed" {
		t.Fatalf("release row = %+v, want release +300 confirmed", rows[2])
	}
	// R-41 的结案标记同时是兜底退款的审计锚点。
	notes := walletNotes(t, db, req)
	if notes[2] != recoverNote {
		t.Fatalf("release note = %q, want %q", notes[2], recoverNote)
	}
}

// TestRecoverAbandonedIdempotent 覆盖重复扫描（顺序 + 并发）：
// 同一 requestID 只产生一行 release，余额只返还一次。
func TestRecoverAbandonedIdempotent(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	ledger := NewLedger(gormDB)
	req := testRequestID("recover-idem")
	ctx := context.Background()

	if _, err := ledger.ApplyDelta(ctx, userID, -200, "consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	e := NewEngine(gormDB, nil, nil, nil, nil, nil, nil)

	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("first recover: %v", err)
	}
	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("second recover: %v", err)
	}

	// 并发路径：两个执行者同时扫描到同一 requestID（单副本部署下也可能
	// 因滚动升级新旧副本并存），CAS 必须只允许一个赢家。
	req2 := testRequestID("recover-concurrent")
	if _, err := ledger.ApplyDelta(ctx, userID, -100, "consume_pre", "hold", "usage_log", req2); err != nil {
		t.Fatalf("hold 2: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = e.RecoverAbandonedByID(ctx, req2)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent recover %d: %v", i, err)
		}
	}

	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（不得重复退款）", got)
	}
	if n := countReleaseRows(t, db, req); n != 1 {
		t.Fatalf("release rows for %s = %d, want 1", req, n)
	}
	if n := countReleaseRows(t, db, req2); n != 1 {
		t.Fatalf("release rows for %s = %d, want 1", req2, n)
	}
}

// TestRecoverAbandonedRemovesRegistry 验证退款成功后清理 Redis 租约；
// 重复回收保持幂等。
func TestRecoverAbandonedRemovesRegistry(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 400)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	registry := NewRegistry(rdb)

	req := testRequestID("recover-registry")
	ctx := context.Background()
	if _, err := NewLedger(gormDB).ApplyDelta(ctx, userID, -120,
		"consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := registry.Register(ctx, req, userID, time.Duration(5)*time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}

	e := NewEngine(gormDB, nil, rdb, nil, nil, registry, nil)
	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}
	// 崩溃场景：注册租约后请求方再未 Terminate/Refund，兜底必须把钱退回。
	if got := userBalance(t, db, userID); got != 400 {
		t.Fatalf("balance = %d, want 400（预扣已全额退回）", got)
	}
	if mr.Exists(leaseKey(req)) {
		t.Fatal("lease hash still exists after recovery")
	}
	// 索引空 ZSET 会被 Redis 自动删除（miniredis 报 "no such key"）。
	if members, _ := mr.ZMembers(registryIndexKey); len(members) != 0 {
		t.Fatalf("index members = %v, want empty", members)
	}
	// 再跑一次：没有 hold 行，仍然返回 nil 且不产生第二行 release。
	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if n := countReleaseRows(t, db, req); n != 1 {
		t.Fatalf("release rows = %d, want 1", n)
	}
}

// TestRecoverAbandonedSettledReturnsNil 验证已结算会话直接返回 nil，
// 不覆盖既有结算事实，也不重复退款。
func TestRecoverAbandonedSettledReturnsNil(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 300)
	ledger := NewLedger(gormDB)
	req := testRequestID("recover-settled")
	ctx := context.Background()

	if _, err := ledger.ApplyDelta(ctx, userID, -100, "consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	// 正常结算（成本 0 → 全额释放 hold），hold 行变为 settled。
	if _, _, err := ledger.settleHolds(ctx, userID, req, 0, 0); err != nil {
		t.Fatalf("settle: %v", err)
	}

	e := NewEngine(gormDB, nil, nil, nil, nil, nil, nil)
	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("RecoverAbandonedByID on settled request: %v", err)
	}
	if got := userBalance(t, db, userID); got != 300 {
		t.Fatalf("balance = %d, want 300（不得二次退款）", got)
	}
	rows := readWalletRows(t, db, req)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2（settle 的 release 行 + hold 行）", rows)
	}
	if rows[0].Status != "settled" {
		t.Fatalf("hold status = %q, want settled", rows[0].Status)
	}
}

// TestRecoverAbandonedNoHold 验证无 hold 的会话（信任旁路/零预算/纯增量）
// 无需退款，直接返回 nil；ChargeIncrement 的 consume 行不得被兜底退回。
func TestRecoverAbandonedNoHold(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100)
	ledger := NewLedger(gormDB)
	req := testRequestID("recover-nohold")
	ctx := context.Background()

	if _, err := ledger.ApplyDelta(ctx, userID, -40, "consume", "confirmed", "usage_log", req); err != nil {
		t.Fatalf("increment: %v", err)
	}
	e := NewEngine(gormDB, nil, nil, nil, nil, nil, nil)
	if err := e.RecoverAbandonedByID(ctx, req); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}
	if got := userBalance(t, db, userID); got != 60 {
		t.Fatalf("balance = %d, want 60（consume 不随兜底退款）", got)
	}
	if n := countReleaseRows(t, db, req); n != 0 {
		t.Fatalf("release rows = %d, want 0", n)
	}

	// 完全未知的 requestID 同样返回 nil（没有可退的持久化状态）。
	if err := e.RecoverAbandonedByID(ctx, testRequestID("recover-unknown")); err != nil {
		t.Fatalf("unknown request recover: %v", err)
	}
	if err := e.RecoverAbandonedByID(ctx, ""); err == nil {
		t.Fatal("empty requestID = nil, want error")
	}
}

// TestRecoverAbandonedRegistryFailure 验证「钱已退、租约没删」的失败面：
// 返回错误让 worker 下一轮重试；重试幂等，不会二次退款。
func TestRecoverAbandonedRegistryFailure(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 200)
	ledger := NewLedger(gormDB)
	req := testRequestID("recover-redis-down")
	ctx := context.Background()

	if _, err := ledger.ApplyDelta(ctx, userID, -80, "consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		MaxRetries:  -1,
		DialTimeout: 100 * time.Millisecond,
		ReadTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := NewRegistry(rdb).Register(ctx, req, userID, time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mr.Close()

	e := NewEngine(gormDB, nil, rdb, nil, nil, NewRegistry(rdb), nil)
	if err := e.RecoverAbandonedByID(ctx, req); err == nil {
		t.Fatal("recover with redis down = nil, want error（租约清理失败需下一轮重试）")
	}
	// 资金已经退还；重试只能命中断言幂等，不得再退。
	if got := userBalance(t, db, userID); got != 200 {
		t.Fatalf("balance = %d, want 200", got)
	}
	if n := countReleaseRows(t, db, req); n != 1 {
		t.Fatalf("release rows = %d, want 1", n)
	}
	if err := e.RecoverAbandonedByID(ctx, req); err == nil {
		t.Fatal("retry with redis down = nil, want error")
	}
	if n := countReleaseRows(t, db, req); n != 1 {
		t.Fatalf("release rows after retry = %d, want 1", n)
	}
}

// countReleaseRows 统计某 requestID 的 release 流水行数（幂等断言用）。
func countReleaseRows(t *testing.T, db *sql.DB, requestID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = $1 AND type = 'release'`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("count release rows: %v", err)
	}
	return n
}
