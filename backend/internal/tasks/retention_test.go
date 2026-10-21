package tasks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// 本文件是 N9 清理循环的无外部依赖单测：族隔离、0=关闭、批量上限与错误
// 传播。真实 SQL 边界（非终态/待补偿/保留期）见 retention_integration_test.go。

func retentionTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTaskRetentionFamilyIsolation(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	calls := map[string]int{}
	w := &retentionWorker{
		log:                 retentionTestLogger(),
		now:                 func() time.Time { return now },
		pluginRetentionDays: 0, // 插件族关闭
		mjRetentionDays:     30,
		batchSize:           5,
		maxBatches:          2,
		deleteFn: func(_ context.Context, family string, days int, _ time.Time, limit int) (int64, error) {
			calls[family]++
			if family == FamilyPlugin {
				t.Fatal("plugin family must not run when its retention is 0")
			}
			if days != 30 {
				t.Fatalf("mj retention days = %d, want 30", days)
			}
			return int64(limit), nil
		},
	}

	w.runFamilies(context.Background())
	// mj 每次返回满批，跑满 maxBatches。
	if calls[FamilyMJ] != 2 {
		t.Fatalf("mj delete calls = %d, want 2", calls[FamilyMJ])
	}
	if calls[FamilyPlugin] != 0 {
		t.Fatalf("plugin delete calls = %d, want 0", calls[FamilyPlugin])
	}
}

func TestTaskRetentionBothDisabledSkipsDatabase(t *testing.T) {
	called := false
	w := &retentionWorker{
		log:                 retentionTestLogger(),
		now:                 time.Now,
		pluginRetentionDays: 0,
		mjRetentionDays:     0,
		batchSize:           5,
		maxBatches:          2,
		deleteFn: func(_ context.Context, _ string, _ int, _ time.Time, _ int) (int64, error) {
			called = true
			return 5, nil
		},
	}
	// runOnce 会先抢锁；这里没有 Redis，直接验证族循环的关闭语义：
	if got := w.reclaimFamily(context.Background(), FamilyPlugin, 0, time.Now()); got != 0 {
		t.Fatalf("disabled family deleted %d rows, want 0", got)
	}
	if called {
		t.Fatal("disabled family must not query the database")
	}
}

func TestTaskRetentionBatchBudgetPerFamily(t *testing.T) {
	calls := map[string]int{}
	w := &retentionWorker{
		log:                 retentionTestLogger(),
		now:                 time.Now,
		pluginRetentionDays: 30,
		mjRetentionDays:     30,
		batchSize:           4,
		maxBatches:          3,
		deleteFn: func(_ context.Context, family string, _ int, _ time.Time, limit int) (int64, error) {
			calls[family]++
			return int64(limit), nil
		},
	}
	// 直接驱动族循环（runOnce 需要 Redis）。每族都必须恰好跑满上限。
	now := time.Now().UTC()
	for _, family := range []string{FamilyPlugin, FamilyMJ} {
		if got := w.reclaimFamily(context.Background(), family, 30, now); got != 4*3 {
			t.Fatalf("%s reclaimed %d, want %d", family, got, 4*3)
		}
	}
	for _, family := range []string{FamilyPlugin, FamilyMJ} {
		if calls[family] != 3 {
			t.Fatalf("%s delete calls = %d, want 3", family, calls[family])
		}
	}
}

func TestTaskRetentionStopsOnShortBatch(t *testing.T) {
	calls := 0
	w := &retentionWorker{
		log:                 retentionTestLogger(),
		now:                 time.Now,
		pluginRetentionDays: 30,
		batchSize:           10,
		maxBatches:          5,
		deleteFn: func(_ context.Context, _ string, _ int, _ time.Time, _ int) (int64, error) {
			calls++
			return 1, nil
		},
	}
	if got := w.reclaimFamily(context.Background(), FamilyPlugin, 30, time.Now()); got != 1 {
		t.Fatalf("reclaimed %d, want 1", got)
	}
	if calls != 1 {
		t.Fatalf("delete calls = %d, want 1 (short batch ends the family pass)", calls)
	}
}

func TestTaskRetentionErrorDoesNotLeakAcrossFamilies(t *testing.T) {
	// 插件族失败后 MJ 仍应继续：runOnce 的循环体逐族独立处理错误。
	// 这里用 deleteFn 的返回值验证 reclaimFamily 的错误边界。
	wantErr := errors.New("db down")
	w := &retentionWorker{
		log:                 retentionTestLogger(),
		now:                 time.Now,
		pluginRetentionDays: 30,
		batchSize:           5,
		maxBatches:          1,
		deleteFn: func(_ context.Context, family string, _ int, _ time.Time, _ int) (int64, error) {
			if family == FamilyPlugin {
				return 0, wantErr
			}
			return 5, nil
		},
	}
	if got := w.reclaimFamily(context.Background(), FamilyPlugin, 30, time.Now()); got != 0 {
		t.Fatalf("plugin reclaimed %d, want 0 on error", got)
	}
	if got := w.reclaimFamily(context.Background(), FamilyMJ, 30, time.Now()); got != 5 {
		t.Fatalf("mj reclaimed %d, want 5", got)
	}
}

func TestDeleteTerminalTasksBeforeRejectsUnknownFamily(t *testing.T) {
	if _, err := DeleteTerminalTasksBefore(context.Background(), nil, "bogus", 30, time.Now(), 10); err == nil {
		t.Fatal("unknown family must return an error, not a silent zero")
	}
}
