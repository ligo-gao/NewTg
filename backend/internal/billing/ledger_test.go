package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"newtg/gateway/internal/contract"
)

// 本文件的集成测试使用真实 PostgreSQL：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable'
//
// 未设置时跳过（与仓内既有集成测试手法一致）。所有测试用户与流水在
// t.Cleanup 中删除，不污染共享测试库。

var billingTestSeq atomic.Int64

// openBillingTestDB 打开主库并返回 GORM/sql 两个句柄（复用同一连接池）。
func openBillingTestDB(t *testing.T) (*gorm.DB, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过计费集成测试")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	return gormDB, sqlDB
}

// createBillingTestUser 创建一个独立用户（余额可指定），并在测试结束后清理。
func createBillingTestUser(t *testing.T, db *sql.DB, balance int64) int64 {
	t.Helper()
	// users."group" 有到 user_groups.name 的外键；默认分组可能尚未由种子数据
	// 创建，这里幂等补一行。
	if _, err := db.Exec(
		`INSERT INTO user_groups (name, ratio, user_selectable, is_system, status)
		 VALUES ('default', 100, true, true, 1) ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("ensure default group: %v", err)
	}

	username := fmt.Sprintf("bill%d%d", time.Now().UnixNano()%1_000_000, billingTestSeq.Add(1))
	var id int64
	err := db.QueryRow(
		`INSERT INTO users (username, password_hash, balance, "group")
		 VALUES ($1, 'x', $2, 'default') RETURNING id`, username, balance).Scan(&id)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM checkins WHERE user_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// mapConfig 是 ConfigReader 的内存实现。
type mapConfig map[string]json.RawMessage

func (m mapConfig) Get(key string) (json.RawMessage, bool) {
	v, ok := m[key]
	return v, ok
}

// billingTestConfig 返回一套确定性配置：关闭信任旁路，预扣比例 1.0。
func billingTestConfig() ConfigReader {
	return mapConfig{
		ConfigPreConsumeRatio: json.RawMessage("1.0"),
		ConfigTrustQuotaUSD:   json.RawMessage("0"),
	}
}

// testRequestID 生成全局唯一 requestID（wallet_transactions.reference_id 为
// varchar(64)，release 行按 reference_id 部分唯一；测试之间不得串号）。
func testRequestID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), billingTestSeq.Add(1))
}

// perCallSnapshot 返回按次定价快照（单位：积分/次）。
func perCallSnapshot(price float64) *contract.PriceSnapshot {
	return &contract.PriceSnapshot{
		Mode:                 contract.BillingModePerCall,
		Price:                contract.PriceData{UsePrice: true, ModelPrice: price},
		GroupRatio:           1,
		PreConsumeMultiplier: 1,
	}
}

// ratioSnapshot 返回传统倍率快照。M3 的 Ratio 价格单位是「积分 / 1M tokens」
// （pricing 包 R9 口径），所以 1M prompt tokens × price 恰好得到 price 积分。
func ratioSnapshot(inputPerMillion, completionPerMillion float64) *contract.PriceSnapshot {
	return &contract.PriceSnapshot{
		Mode: contract.BillingModeRatio,
		Price: contract.PriceData{
			ModelRatio:           inputPerMillion,
			CompletionRatio:      completionPerMillion,
			CacheRatio:           0,
			CacheCreationRatio:   0,
			CacheCreation5mRatio: 0,
			CacheCreation1hRatio: 0,
			ImageRatio:           1,
			AudioRatio:           1,
			AudioCompletionRatio: 1,
		},
		GroupRatio:           1,
		PreConsumeMultiplier: 1,
	}
}

// walletRow 是 wallet_transactions 的读投影。
type walletRow struct {
	Type         string
	Amount       int64
	BalanceAfter int64
	Status       string
}

// readWalletRows 按 id 升序返回某 requestID 的全部流水。
func readWalletRows(t *testing.T, db *sql.DB, requestID string) []walletRow {
	t.Helper()
	rows, err := db.Query(
		`SELECT type, amount, balance_after, status FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = $1 ORDER BY id`, requestID)
	if err != nil {
		t.Fatalf("read wallet rows: %v", err)
	}
	defer rows.Close()
	var out []walletRow
	for rows.Next() {
		var r walletRow
		if err := rows.Scan(&r.Type, &r.Amount, &r.BalanceAfter, &r.Status); err != nil {
			t.Fatalf("scan wallet row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate wallet rows: %v", err)
	}
	return out
}

// userBalance 读取当前余额。
func userBalance(t *testing.T, db *sql.DB, userID int64) int64 {
	t.Helper()
	var balance int64
	if err := db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return balance
}

// TestLedgerApplyDelta 覆盖账本原子变更的四个方向与失败面：
// 出账条件更新、入账、零/非法参数、余额不足不产生流水。
func TestLedgerApplyDelta(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	ledger := NewLedger(gormDB)
	userID := createBillingTestUser(t, db, 100)
	ctx := context.Background()

	balanceAfter, err := ledger.ApplyDelta(ctx, userID, -30, "consume_pre", "hold", "usage_log", "ledger-1")
	if err != nil {
		t.Fatalf("ApplyDelta(-30): %v", err)
	}
	if balanceAfter != 70 {
		t.Fatalf("balanceAfter = %d, want 70", balanceAfter)
	}

	balanceAfter, err = ledger.ApplyDelta(ctx, userID, 10, "release", "confirmed", "usage_log", "ledger-1")
	if err != nil {
		t.Fatalf("ApplyDelta(+10): %v", err)
	}
	if balanceAfter != 80 {
		t.Fatalf("balanceAfter = %d, want 80", balanceAfter)
	}

	rows := readWalletRows(t, db, "ledger-1")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2（每笔变动恰一行）", len(rows))
	}
	if rows[0] != (walletRow{Type: "consume_pre", Amount: -30, BalanceAfter: 70, Status: "hold"}) {
		t.Fatalf("row0 = %+v", rows[0])
	}
	if rows[1] != (walletRow{Type: "release", Amount: 10, BalanceAfter: 80, Status: "confirmed"}) {
		t.Fatalf("row1 = %+v", rows[1])
	}

	// 余额不足：不得扣成负数，也不得产生流水。
	_, err = ledger.ApplyDelta(ctx, userID, -1000, "consume_pre", "hold", "usage_log", "ledger-insufficient")
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Fatalf("ApplyDelta(-1000) err = %v, want ErrInsufficientQuota", err)
	}
	var qe *QuotaError
	if !errors.As(err, &qe) {
		t.Fatalf("err type = %T, want *QuotaError", err)
	}
	if qe.Class != contract.ErrInsufficientQuota || qe.Required != 1000 || qe.Available != 80 {
		t.Fatalf("QuotaError = %+v", qe)
	}
	if got := userBalance(t, db, userID); got != 80 {
		t.Fatalf("balance = %d, want 80（失败路径不得改动余额）", got)
	}
	if rows := readWalletRows(t, db, "ledger-insufficient"); len(rows) != 0 {
		t.Fatalf("insufficient path wrote %d rows, want 0", len(rows))
	}

	// 零金额与非法枚举必须在应用层拒绝（数据库 CHECK 是第二道防线）。
	if _, err := ledger.ApplyDelta(ctx, userID, 0, "consume", "confirmed", "usage_log", "x"); err == nil {
		t.Fatal("ApplyDelta(0) = nil, want error")
	}
	if _, err := ledger.ApplyDelta(ctx, userID, -1, "not_a_type", "confirmed", "", ""); err == nil {
		t.Fatal("ApplyDelta(invalid type) = nil, want error")
	}
	if _, err := ledger.ApplyDelta(ctx, userID, -1, "consume", "not_a_status", "", ""); err == nil {
		t.Fatal("ApplyDelta(invalid status) = nil, want error")
	}
}

// TestLedgerUserNotFound 验证目标用户不存在时给出明确错误且不写流水。
func TestLedgerUserNotFound(t *testing.T) {
	gormDB, _ := openBillingTestDB(t)
	ledger := NewLedger(gormDB)
	_, err := ledger.ApplyDelta(context.Background(), -12345, -1, "consume", "confirmed", "", "")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

// TestLedgerRejectsMinInt64 验证极端负增量被显式拒绝：-MinInt64 在 int64
// 中回绕为自身，会让出账方向反转并绕过余额谓词。
func TestLedgerRejectsMinInt64(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100)
	ledger := NewLedger(gormDB)
	_, err := ledger.ApplyDelta(context.Background(), userID, math.MinInt64,
		"consume", "confirmed", "usage_log", "minint64")
	if err == nil {
		t.Fatal("ApplyDelta(MinInt64) = nil, want error")
	}
	if got := userBalance(t, db, userID); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
	if rows := readWalletRows(t, db, "minint64"); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

// TestLedgerCancelledContext 验证 ctx 取消时返回错误而不是挂起/半写。
func TestLedgerCancelledContext(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 100)
	ledger := NewLedger(gormDB)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ledger.ApplyDelta(ctx, userID, -10, "consume_pre", "hold", "usage_log", "ctx-cancel")
	if err == nil {
		t.Fatal("ApplyDelta with cancelled ctx = nil, want error")
	}
	if got := userBalance(t, db, userID); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}
}

// TestLedgerClosedDBReturnsError 验证底层连接不可用时返回错误而不是 panic。
func TestLedgerClosedDBReturnsError(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过计费集成测试")
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: raw}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		// gorm 在初始化阶段探测连接并报错，也是可接受的失败面：
		// 关键是「返回 error、不 panic」。
		return
	}
	_, err = NewLedger(gormDB).ApplyDelta(context.Background(), 1, -1,
		"consume", "confirmed", "", "")
	if err == nil {
		t.Fatal("ApplyDelta on closed db = nil, want error")
	}
}
