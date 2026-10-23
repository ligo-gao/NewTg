//go:build integration

package store

// request_pricing 仓储的数据库级验收：真实 PostgreSQL（spec §5.2）。
//
// 运行（隔离库，先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test_verify_t4?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestRequestPricingRepo -v

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRequestPricingRepoUpsertGetAndCleanup(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewRequestPricingRepo(db)
	ctx := context.Background()
	requestID := uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM request_pricing WHERE request_id = $1::uuid`, requestID)
	})

	if _, ok, err := repo.Get(ctx, requestID); err != nil || ok {
		t.Fatalf("Get(missing) = ok=%v err=%v, want false,nil", ok, err)
	}

	// timestamptz 的精度是微秒；截断后再比较。
	requestTime := time.Now().UTC().Truncate(time.Microsecond)
	rec := RequestPricing{
		RequestID:      requestID,
		RequestTime:    requestTime,
		TimeMultiplier: 0.5,
		TimeWindow:     "上午高峰",
	}
	if err := repo.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok, err := repo.Get(ctx, requestID)
	if err != nil || !ok {
		t.Fatalf("Get = ok=%v err=%v, want true,nil", ok, err)
	}
	if got.RequestID != requestID {
		t.Fatalf("RequestID = %q, want %q", got.RequestID, requestID)
	}
	if !got.RequestTime.Equal(requestTime) {
		t.Fatalf("RequestTime = %v, want %v", got.RequestTime, requestTime)
	}
	if got.TimeMultiplier != 0.5 || got.TimeWindow != "上午高峰" {
		t.Fatalf("record = %+v", got)
	}

	// Upsert 幂等覆盖：同一 requestID 重试写回同一事实。
	rec.TimeMultiplier = 1.0
	rec.TimeWindow = "下午高峰"
	if err := repo.Upsert(ctx, rec); err != nil {
		t.Fatalf("Upsert(update): %v", err)
	}
	got, ok, err = repo.Get(ctx, requestID)
	if err != nil || !ok {
		t.Fatalf("Get(after update) = ok=%v err=%v", ok, err)
	}
	if got.TimeMultiplier != 1.0 || got.TimeWindow != "下午高峰" {
		t.Fatalf("updated record = %+v", got)
	}

	// cutoff 在 created_at 之前：不删。
	if n, err := repo.DeleteBefore(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("DeleteBefore(keep) = %d,%v, want 0,nil", n, err)
	}
	if _, ok, _ := repo.Get(ctx, requestID); !ok {
		t.Fatal("DeleteBefore(keep) 删除了未过期的行")
	}
	// cutoff 在 created_at 之后：删除。
	if n, err := repo.DeleteBefore(ctx, time.Now().Add(time.Minute)); err != nil || n < 1 {
		t.Fatalf("DeleteBefore(delete) = %d,%v, want >=1,nil", n, err)
	}
	if _, ok, _ := repo.Get(ctx, requestID); ok {
		t.Fatal("DeleteBefore(delete) 未删除过期行")
	}
}

func TestRequestPricingRepoRejectsInvalidInput(t *testing.T) {
	db := openPublicPricingTestDB(t)
	repo := NewRequestPricingRepo(db)
	ctx := context.Background()

	if err := repo.Upsert(ctx, RequestPricing{RequestID: "req-not-a-uuid"}); !errors.Is(err, ErrInvalidRequestPricingID) {
		t.Fatalf("invalid id err = %v, want ErrInvalidRequestPricingID", err)
	}
	if ValidRequestPricingID("req-not-a-uuid") {
		t.Fatal("ValidRequestPricingID(non-uuid) = true")
	}
	if !ValidRequestPricingID(uuid.NewString()) {
		t.Fatal("ValidRequestPricingID(uuid) = false")
	}
	// 32 位十六进制（生产 relay.newRequestID 的形态）是合法 uuid 输入。
	if !ValidRequestPricingID("0123456789abcdef0123456789abcdef") {
		t.Fatal("ValidRequestPricingID(32 hex) = false")
	}
	if err := repo.Upsert(ctx, RequestPricing{
		RequestID:      uuid.NewString(),
		TimeMultiplier: 0.5,
	}); err == nil {
		t.Fatal("Upsert(zero request_time) = nil, want error")
	}
}
