//go:build integration

package store

// request_pricing 折扣列的数据库级验收（spec §5.2 / Task 6）。
//
// 冻结折扣与时间/倍率必须落在同一行：任务轮询与 handoff 重建只拿得到
// requestID，读不回折扣就会按原价结算，把预扣时已让利的部分再追回来。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://.../newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestRequestPricingRepoDiscount -v

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRequestPricingRepoDiscountRoundTrip 验证折扣两列的写入/读回往返，
// 以及迁移前写入的旧行（只填四列）读回列默认值 1 与空串。
func TestRequestPricingRepoDiscountRoundTrip(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewRequestPricingRepo(db)
	ctx := context.Background()

	requestID := uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, requestID)
	})

	rec := RequestPricing{
		RequestID:      requestID,
		RequestTime:    time.Now().UTC().Truncate(time.Microsecond),
		TimeMultiplier: 1,
		DiscountRate:   0.85,
		DiscountTarget: "group:acme",
	}
	if err := repo.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok, err := repo.Get(ctx, requestID)
	if err != nil || !ok {
		t.Fatalf("Get = ok=%v err=%v, want true,nil", ok, err)
	}
	if got.DiscountRate != 0.85 || got.DiscountTarget != "group:acme" {
		t.Fatalf("折扣 = (%v, %q), want (0.85, \"group:acme\")", got.DiscountRate, got.DiscountTarget)
	}

	// 旧行：迁移新增两列之前写入，只填原有四列。NOT NULL + DEFAULT 保证
	// 读回 1 / ""（不需要 COALESCE 或指针字段）。
	legacyID := uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, legacyID)
	})
	if _, err := db.Exec(
		`INSERT INTO request_pricing (request_id, request_time, time_multiplier, time_window)
		 VALUES ($1::uuid, $2, $3, $4)`,
		legacyID, time.Now().UTC(), 1.0, ""); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	legacy, ok, err := repo.Get(ctx, legacyID)
	if err != nil || !ok {
		t.Fatalf("Get(legacy) = ok=%v err=%v, want true,nil", ok, err)
	}
	if legacy.DiscountRate != 1 || legacy.DiscountTarget != "" {
		t.Fatalf("legacy 折扣 = (%v, %q), want (1, \"\")", legacy.DiscountRate, legacy.DiscountTarget)
	}
}
