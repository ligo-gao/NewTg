//go:build integration

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

// 本文件是 W10-N4 兑换码回收的真 PG + Redis 验收（R-W10-05）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10n?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration ./internal/billing/ -run RedemptionRetention -count=1

func openRedemptionRetentionDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过兑换码回收集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openRedemptionRetentionRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过兑换码回收集成测试")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping test redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

var redemptionRetentionSeq int

// insertRedemptionFixture 写入一枚兑换码；batch 作为隔离与清理锚点。
func insertRedemptionFixture(t *testing.T, db *sql.DB, batch, status string, expiresAt *time.Time) int64 {
	t.Helper()
	redemptionRetentionSeq++
	hash := fmt.Sprintf("retention-%s-%d", batch, redemptionRetentionSeq)
	var id int64
	err := db.QueryRow(`
		INSERT INTO redemption_codes (code_hash, credits, status, batch_name, created_by, expires_at)
		VALUES ($1, 100, $2, $3, 1, $4)
		RETURNING id`, hash, status, batch, expiresAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert redemption fixture: %v", err)
	}
	return id
}

func countRedemptionBatch(t *testing.T, db *sql.DB, batch string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM redemption_codes WHERE batch_name = $1`, batch).
		Scan(&n); err != nil {
		t.Fatalf("count redemption batch: %v", err)
	}
	return n
}

func TestRedemptionRetentionDeletesOnlyExpiredUnusedBeyondWindow(t *testing.T) {
	db := openRedemptionRetentionDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-90 * 24 * time.Hour)
	batch := fmt.Sprintf("w10n4-%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE batch_name = $1`, batch) })

	past := cutoff.Add(-time.Hour)
	insideWindow := cutoff.Add(time.Hour) // 已过期但未超保留期
	future := now.Add(time.Hour)

	expiredOld := insertRedemptionFixture(t, db, batch, "unused", &past)
	insertRedemptionFixture(t, db, batch, "unused", &insideWindow) // 未超期
	insertRedemptionFixture(t, db, batch, "unused", &future)       // 未过期
	usedOld := insertRedemptionFixture(t, db, batch, "used", &past)
	disabledOld := insertRedemptionFixture(t, db, batch, "disabled", &past)
	insertRedemptionFixture(t, db, batch, "unused", nil) // 永不过期

	deleted, err := DeleteExpiredRedemptionCodes(ctx, db, cutoff, 100)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if n := countRedemptionBatch(t, db, batch); n != 5 {
		t.Fatalf("remaining rows = %d, want 5", n)
	}

	// R11：不物化过期状态——被删除的只有一枚 unused+expired，其它行的
	// status 原样保留。
	var status string
	if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE id = $1`, usedOld).Scan(&status); err != nil {
		t.Fatalf("read used row: %v", err)
	}
	if status != "used" {
		t.Fatalf("used status = %q, want used", status)
	}
	if err := db.QueryRow(`SELECT status FROM redemption_codes WHERE id = $1`, disabledOld).Scan(&status); err != nil {
		t.Fatalf("read disabled row: %v", err)
	}
	if status != "disabled" {
		t.Fatalf("disabled status = %q, want disabled", status)
	}
	var gone int
	if err := db.QueryRow(`SELECT count(*) FROM redemption_codes WHERE id = $1`, expiredOld).Scan(&gone); err != nil {
		t.Fatalf("check deleted row: %v", err)
	}
	if gone != 0 {
		t.Fatalf("expired unused row still present")
	}
}

func TestRedemptionRetentionBatchLimitAndIdempotency(t *testing.T) {
	db := openRedemptionRetentionDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-90 * 24 * time.Hour)
	batch := fmt.Sprintf("w10n4b-%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE batch_name = $1`, batch) })

	for i := 0; i < 5; i++ {
		exp := cutoff.Add(-time.Duration(i+1) * time.Hour)
		insertRedemptionFixture(t, db, batch, "unused", &exp)
	}

	for _, want := range []int64{2, 2, 1} {
		got, err := DeleteExpiredRedemptionCodes(ctx, db, cutoff, 2)
		if err != nil {
			t.Fatalf("delete batch: %v", err)
		}
		if got != want {
			t.Fatalf("batch deleted = %d, want %d", got, want)
		}
	}
	// 幂等：没有可删行时返回 0，不报错。
	got, err := DeleteExpiredRedemptionCodes(ctx, db, cutoff, 2)
	if err != nil || got != 0 {
		t.Fatalf("repeat delete = (%d, %v), want (0, nil)", got, err)
	}
}

func TestRedemptionRetentionWorkerDisabledKeepsRows(t *testing.T) {
	db := openRedemptionRetentionDB(t)
	rdb := openRedemptionRetentionRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	batch := fmt.Sprintf("w10n4d-%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE batch_name = $1`, batch) })

	past := now.Add(-365 * 24 * time.Hour)
	insertRedemptionFixture(t, db, batch, "unused", &past)

	w, err := newRedemptionRetentionWorker(RedemptionRetentionDeps{
		DB:            db,
		Redis:         rdb,
		Now:           func() time.Time { return now },
		RetentionDays: 0, // 0 = 关闭
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	w.runOnce(ctx)
	if n := countRedemptionBatch(t, db, batch); n != 1 {
		t.Fatalf("disabled retention deleted rows; remaining = %d, want 1", n)
	}
}

func TestRedemptionRetentionWorkerRunOnce(t *testing.T) {
	db := openRedemptionRetentionDB(t)
	rdb := openRedemptionRetentionRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	batch := fmt.Sprintf("w10n4w-%d", time.Now().UnixNano()%1_000_000)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM redemption_codes WHERE batch_name = $1`, batch) })

	past := now.Add(-200 * 24 * time.Hour)
	insertRedemptionFixture(t, db, batch, "unused", &past)

	w, err := newRedemptionRetentionWorker(RedemptionRetentionDeps{
		DB:            db,
		Redis:         rdb,
		Now:           func() time.Time { return now },
		RetentionDays: 90,
		BatchSize:     10,
		MaxBatches:    2,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	w.runOnce(ctx)
	if n := countRedemptionBatch(t, db, batch); n != 0 {
		t.Fatalf("worker left %d rows, want 0", n)
	}
	// 幂等：再跑一轮不报错、无行可删。
	w.runOnce(ctx)
}
