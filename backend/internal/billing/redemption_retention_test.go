package billing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// 本文件是 N4 回收循环的无外部依赖单测：批量上限、0=关闭、短批停止与
// 错误传播。真实 SQL 边界（未过期/已用/未超期不删）见
// redemption_retention_integration_test.go。

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRedemptionRetentionReclaimBatchBudget(t *testing.T) {
	calls := 0
	w := &redemptionRetentionWorker{
		log:           discardLogger(),
		now:           func() time.Time { return time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC) },
		retentionDays: 90,
		batchSize:     5,
		maxBatches:    3,
		deleteFn: func(_ context.Context, _ time.Time, limit int) (int64, error) {
			calls++
			return int64(limit), nil
		},
	}

	got := w.reclaim(context.Background())
	if want := int64(5 * 3); got != want {
		t.Fatalf("reclaim = %d, want %d", got, want)
	}
	// 每批都满，必须恰好跑满 maxBatches 后停止（不允许无限循环）。
	if calls != 3 {
		t.Fatalf("delete calls = %d, want 3 (max batches)", calls)
	}
}

func TestRedemptionRetentionStopsOnShortBatch(t *testing.T) {
	calls := 0
	w := &redemptionRetentionWorker{
		log:           discardLogger(),
		now:           time.Now,
		retentionDays: 90,
		batchSize:     10,
		maxBatches:    5,
		deleteFn: func(_ context.Context, _ time.Time, _ int) (int64, error) {
			calls++
			return 2, nil // 不足一批 = 已清空
		},
	}
	if got := w.reclaim(context.Background()); got != 2 {
		t.Fatalf("reclaim = %d, want 2", got)
	}
	if calls != 1 {
		t.Fatalf("delete calls = %d, want 1 (short batch ends the pass)", calls)
	}
}

func TestRedemptionRetentionDisabledSkipsDatabase(t *testing.T) {
	called := false
	w := &redemptionRetentionWorker{
		log:           discardLogger(),
		now:           time.Now,
		retentionDays: 0, // 0 = 关闭（冻结语义）
		batchSize:     10,
		maxBatches:    5,
		deleteFn: func(_ context.Context, _ time.Time, _ int) (int64, error) {
			called = true
			return 10, nil
		},
	}
	if got := w.reclaim(context.Background()); got != 0 {
		t.Fatalf("reclaim = %d, want 0 when retention is disabled", got)
	}
	if called {
		t.Fatal("disabled retention must not query the database")
	}
}

func TestRedemptionRetentionDeleteErrorStopsPass(t *testing.T) {
	calls := 0
	wantErr := errors.New("db down")
	w := &redemptionRetentionWorker{
		log:           discardLogger(),
		now:           time.Now,
		retentionDays: 90,
		batchSize:     5,
		maxBatches:    5,
		deleteFn: func(_ context.Context, _ time.Time, _ int) (int64, error) {
			calls++
			if calls == 2 {
				return 0, wantErr
			}
			return 5, nil
		},
	}
	// 第一批成功 5 行，第二批报错：返回已删数且不再继续。
	if got := w.reclaim(context.Background()); got != 5 {
		t.Fatalf("reclaim = %d, want 5 (first batch only)", got)
	}
	if calls != 2 {
		t.Fatalf("delete calls = %d, want 2 (error ends the pass)", calls)
	}
}

func TestRedemptionRetentionCutoffUsesExpiresAt(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var gotCutoff time.Time
	w := &redemptionRetentionWorker{
		log:           discardLogger(),
		now:           func() time.Time { return now },
		retentionDays: 90,
		batchSize:     10,
		maxBatches:    1,
		deleteFn: func(_ context.Context, cutoff time.Time, _ int) (int64, error) {
			gotCutoff = cutoff
			return 0, nil
		},
	}
	w.reclaim(context.Background())
	if want := now.Add(-90 * 24 * time.Hour); !gotCutoff.Equal(want) {
		t.Fatalf("cutoff = %s, want %s", gotCutoff, want)
	}
}
