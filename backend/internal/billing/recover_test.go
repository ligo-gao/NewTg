package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
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
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
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

	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("first recover: %v", err)
	}
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
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
			errs[i] = e.RecoverAbandonedByID(ctx, req2, 0)
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
	if err := registry.Register(ctx, req, userID, 0, time.Duration(5)*time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}

	e := NewEngine(gormDB, nil, rdb, nil, nil, registry, nil)
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
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
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
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
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
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
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}
	if got := userBalance(t, db, userID); got != 60 {
		t.Fatalf("balance = %d, want 60（consume 不随兜底退款）", got)
	}
	if n := countReleaseRows(t, db, req); n != 0 {
		t.Fatalf("release rows = %d, want 0", n)
	}

	// 完全未知的 requestID 同样返回 nil（没有可退的持久化状态）。
	if err := e.RecoverAbandonedByID(ctx, testRequestID("recover-unknown"), 0); err != nil {
		t.Fatalf("unknown request recover: %v", err)
	}
	if err := e.RecoverAbandonedByID(ctx, "", 0); err == nil {
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
	if err := NewRegistry(rdb).Register(ctx, req, userID, 0, time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mr.Close()

	e := NewEngine(gormDB, nil, rdb, nil, nil, NewRegistry(rdb), nil)
	if err := e.RecoverAbandonedByID(ctx, req, 0); err == nil {
		t.Fatal("recover with redis down = nil, want error（租约清理失败需下一轮重试）")
	}
	// 资金已经退还；重试只能命中断言幂等，不得再退。
	if got := userBalance(t, db, userID); got != 200 {
		t.Fatalf("balance = %d, want 200", got)
	}
	if n := countReleaseRows(t, db, req); n != 1 {
		t.Fatalf("release rows = %d, want 1", n)
	}
	if err := e.RecoverAbandonedByID(ctx, req, 0); err == nil {
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

// openTask5LogsDB 连接日志库（NEWTG_TEST_DB_DSN 的 dbname 换成 newtg_logs），
// 用于验证 worker 退款占位行。未配置测试库时跳过。
//
// 不使用 T6 的 openLogsTestDB（它带 integration 构建标签），这样本文件的
// 集成用例在普通构建下也能编译并按环境变量决定是否跳过。
func openTask5LogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过计费集成测试")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Fatalf("parse NEWTG_TEST_DB_DSN: %v", err)
	}
	u.Path = "/newtg_logs"
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var reg sql.NullString
	if err := db.QueryRow(`SELECT to_regclass('usage_logs')::text`).Scan(&reg); err != nil || !reg.Valid {
		t.Fatalf("logs db missing usage_logs (err=%v)", err)
	}
	return db
}

// newTask5UUID 生成 v4 UUID：usage_logs.request_id 是 uuid 列，
// wallet_transactions.reference_id 是 varchar(64)，两者共用同一字符串。
func newTask5UUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TestRecoverAbandonedWritesRefundedLog 验证 worker 退款占位经 DBLogger.Saturate
// 写入：status=refunded、cost=0、refunded/pre_deducted 为 hold 金额；重复
// 回收收敛到同一行（不产生第二行）。
func TestRecoverAbandonedWritesRefundedLog(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openTask5LogsDB(t)
	userID := createBillingTestUser(t, db, 500)
	req := newTask5UUID(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, req) })

	if _, err := NewLedger(gormDB).ApplyDelta(ctx, userID, -120,
		"consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}

	e := NewEngine(gormDB, logsDB, nil, nil, nil, nil, NewDBLogger(logsDB))
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}

	var (
		status                 int16
		cost, refunded, preDed int64
		logUser                int64
		errorClass             string
	)
	if err := logsDB.QueryRow(
		`SELECT status, cost, refunded, pre_deducted, user_id, COALESCE(error_class, '')
		   FROM usage_logs WHERE request_id = $1::uuid`, req).
		Scan(&status, &cost, &refunded, &preDed, &logUser, &errorClass); err != nil {
		t.Fatalf("read usage_logs: %v", err)
	}
	if status != int16(UsageLogStatusRefunded) || cost != 0 {
		t.Fatalf("status/cost = %d/%d, want %d/0", status, cost, UsageLogStatusRefunded)
	}
	if refunded != 120 || preDed != 120 {
		t.Fatalf("refunded/pre_deducted = %d/%d, want 120/120", refunded, preDed)
	}
	if logUser != userID {
		t.Fatalf("user_id = %d, want %d", logUser, userID)
	}
	if errorClass != "abandoned" {
		t.Fatalf("error_class = %q, want abandoned", errorClass)
	}

	// 重复回收：expired 预扣行仍在，Saturate 按 request_id 收敛而不是插新行。
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("second recover: %v", err)
	}
	var n int
	if err := logsDB.QueryRow(
		`SELECT count(*) FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&n); err != nil {
		t.Fatalf("count usage_logs: %v", err)
	}
	if n != 1 {
		t.Fatalf("usage_logs rows = %d, want 1", n)
	}
}

// TestRecoverAbandonedRestoresTokenQuota 覆盖 R-88：租约携带的 token_id 让
// worker 在崩溃恢复时把 hold 部分加回 api_tokens.remain_quota；ChargeIncrement
// 的增量保持已扣（只回补 held），重复回收不重复回补。
func TestRecoverAbandonedRestoresTokenQuota(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 1000)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("recover-token")
	ctx := context.Background()

	ledger := NewLedger(gormDB)
	// 预扣 300：钱包与令牌在同一事务内扣减（R-73）。
	if _, err := ledger.ApplyDeltaWithToken(ctx, userID, tokenID, -300,
		"consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	// 增量 50：已交付用量，崩溃退款不退还（R-39）。
	if _, err := ledger.ApplyDeltaWithToken(ctx, userID, tokenID, -50,
		"consume", "confirmed", "usage_log", req); err != nil {
		t.Fatalf("increment: %v", err)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 150 {
		t.Fatalf("token remain before recovery = %v, want 150", got)
	}

	e := NewEngine(gormDB, nil, nil, nil, nil, nil, nil)
	if err := e.RecoverAbandonedByID(ctx, req, tokenID); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}
	if got := userBalance(t, db, userID); got != 950 {
		t.Fatalf("balance = %d, want 950（只退 hold 300）", got)
	}
	// 令牌：150 + 退回的 hold 300 = 450；增量 50 保留已扣。
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 450 {
		t.Fatalf("token remain = %v, want 450（回补 hold，不补增量）", got)
	}

	// 重复回收：hold 已关闭，不得二次回补令牌额度。
	if err := e.RecoverAbandonedByID(ctx, req, tokenID); err != nil {
		t.Fatalf("second recover: %v", err)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 450 {
		t.Fatalf("token remain after retry = %v, want 450", got)
	}
}

// TestRecoverAbandonedTokenIDZeroIsNoop 覆盖旧版租约/未绑定令牌（tokenID=0）：
// 钱包照常退款，令牌额度不回补（no-op），不影响恢复成功。
func TestRecoverAbandonedTokenIDZeroIsNoop(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 500)
	tokenID := createBillingTestToken(t, db, userID, int64(500))
	req := testRequestID("recover-token-zero")
	ctx := context.Background()

	if _, err := NewLedger(gormDB).ApplyDeltaWithToken(ctx, userID, tokenID, -200,
		"consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 300 {
		t.Fatalf("token remain before recovery = %v, want 300", got)
	}

	e := NewEngine(gormDB, nil, nil, nil, nil, nil, nil)
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}
	if got := userBalance(t, db, userID); got != 500 {
		t.Fatalf("balance = %d, want 500（钱包退款不受令牌身份缺失影响）", got)
	}
	if got := tokenRemain(t, db, tokenID); !got.Valid || got.Int64 != 300 {
		t.Fatalf("token remain = %v, want 300（tokenID=0 时 no-op）", got)
	}
}

// TestRecoverAbandonedLogRecordsRetainedIncrement 覆盖 R-55/R-59：兜底退款
// 保留 ChargeIncrement 已扣的消耗（R-39），refunded 日志的 cost 必须记录
// 保留额而不是 0，否则日志与钱包对不上（钱包净扣 50，日志 cost 也必须是 50）。
func TestRecoverAbandonedLogRecordsRetainedIncrement(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openTask5LogsDB(t)
	userID := createBillingTestUser(t, db, 1000)
	req := newTask5UUID(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, req) })

	ledger := NewLedger(gormDB)
	if _, err := ledger.ApplyDelta(ctx, userID, -300, "consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := ledger.ApplyDelta(ctx, userID, -50, "consume", "confirmed", "usage_log", req); err != nil {
		t.Fatalf("consume increment: %v", err)
	}

	e := NewEngine(gormDB, logsDB, nil, nil, nil, nil, NewDBLogger(logsDB))
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}

	// 钱包：1000 − 300 − 50 + 300 = 950，净保留 50。
	if got := userBalance(t, db, userID); got != 950 {
		t.Fatalf("balance = %d, want 950（只退 hold 300）", got)
	}
	var (
		status                 int16
		cost, refunded, preDed int64
	)
	if err := logsDB.QueryRow(
		`SELECT status, cost, refunded, pre_deducted
		   FROM usage_logs WHERE request_id = $1::uuid`, req).
		Scan(&status, &cost, &refunded, &preDed); err != nil {
		t.Fatalf("read usage_logs: %v", err)
	}
	if status != int16(UsageLogStatusRefunded) {
		t.Fatalf("status = %d, want refunded", status)
	}
	if cost != 50 {
		t.Fatalf("cost = %d, want 50（保留的 ChargeIncrement 消耗）", cost)
	}
	if refunded != 300 || preDed != 300 {
		t.Fatalf("refunded/pre_deducted = %d/%d, want 300/300", refunded, preDed)
	}

	// 重复回收走「已有行 → 收敛」分支（R-25/R-55）：必须保留 cost=50，
	// 不能把已记录的保留消耗清零，也不能插第二行。
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("second recover: %v", err)
	}
	var n int
	if err := logsDB.QueryRow(
		`SELECT count(*), COALESCE(MAX(cost), 0)
		   FROM usage_logs WHERE request_id = $1::uuid`, req).
		Scan(&n, &cost); err != nil {
		t.Fatalf("re-read usage_logs: %v", err)
	}
	if n != 1 || cost != 50 {
		t.Fatalf("after converge: rows=%d cost=%d, want 1/50", n, cost)
	}
}

// TestRecoverAbandonedLogFailureRetries 验证日志库不可用时返回错误保留租约，
// 下一轮（新的 Saturator）能补写占位；资金不重复退。
func TestRecoverAbandonedLogFailureRetries(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openTask5LogsDB(t)
	userID := createBillingTestUser(t, db, 300)
	req := newTask5UUID(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, req) })

	if _, err := NewLedger(gormDB).ApplyDelta(ctx, userID, -90,
		"consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}

	// NewDBLogger(nil) 模拟日志库不可用：退款已完成，但占位写失败。
	eBad := NewEngine(gormDB, logsDB, nil, nil, nil, nil, NewDBLogger(nil))
	if err := eBad.RecoverAbandonedByID(ctx, req, 0); err == nil {
		t.Fatal("recover with broken logger = nil, want error")
	}
	if got := userBalance(t, db, userID); got != 300 {
		t.Fatalf("balance = %d, want 300（退款已完成）", got)
	}

	// 下一轮：expired 行仍在 → 重新写占位；资金不重复退。
	eGood := NewEngine(gormDB, logsDB, nil, nil, nil, nil, NewDBLogger(logsDB))
	if err := eGood.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("retry recover: %v", err)
	}
	var status int16
	if err := logsDB.QueryRow(
		`SELECT status FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&status); err != nil {
		t.Fatalf("read usage_logs: %v", err)
	}
	if status != int16(UsageLogStatusRefunded) {
		t.Fatalf("status = %d, want refunded", status)
	}
	if got := userBalance(t, db, userID); got != 300 {
		t.Fatalf("balance after retry = %d, want 300（不得二次退款）", got)
	}
	if n := countReleaseRows(t, db, req); n != 1 {
		t.Fatalf("release rows = %d, want 1", n)
	}
}

// TestRecoverAbandonedSettledDoesNotWriteRefundedLog 验证正常结算过的请求
// 不会被 worker 改写成 refunded（R-25 的单向收敛只对 expired 事实生效）。
func TestRecoverAbandonedSettledDoesNotWriteRefundedLog(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	logsDB := openTask5LogsDB(t)
	userID := createBillingTestUser(t, db, 300)
	req := newTask5UUID(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = logsDB.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, req) })

	ledger := NewLedger(gormDB)
	if _, err := ledger.ApplyDelta(ctx, userID, -100, "consume_pre", "hold", "usage_log", req); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, _, err := ledger.settleHolds(ctx, userID, req, 0, 0); err != nil {
		t.Fatalf("settle: %v", err)
	}

	e := NewEngine(gormDB, logsDB, nil, nil, nil, nil, NewDBLogger(logsDB))
	if err := e.RecoverAbandonedByID(ctx, req, 0); err != nil {
		t.Fatalf("RecoverAbandonedByID: %v", err)
	}
	var n int
	if err := logsDB.QueryRow(
		`SELECT count(*) FROM usage_logs WHERE request_id = $1::uuid`, req).Scan(&n); err != nil {
		t.Fatalf("count usage_logs: %v", err)
	}
	if n != 0 {
		t.Fatalf("usage_logs rows = %d, want 0（settled 不是兜底退款）", n)
	}
}
