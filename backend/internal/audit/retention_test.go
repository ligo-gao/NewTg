package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// 本文件验收 W10-N11 的纯逻辑面（不依赖 PG/Redis）：
//   - 两个分支的 cutoff 口径（日历月 vs 固定天数）；
//   - 0 = 关闭（不删除/不擦除），不是「立即处理」；
//   - 分批与单轮上限；
//   - 自审计每轮最多一条、无变化不写（有界自引用）；
//   - 动作名进入审计分类表（自记录可落库）。

func testRetentionLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type retentionCall struct {
	cutoff time.Time
	limit  int
}

func newTestRetentionWorker(deleteFn, eraseFn func(context.Context, time.Time, int) (int64, error)) *retentionWorker {
	return &retentionWorker{
		db:              &sql.DB{}, // 关闭分支/注入 fake 时不会真正触库
		log:             testRetentionLogger(),
		now:             func() time.Time { return time.Now() },
		interval:        RetentionInterval,
		retentionMonths: DefaultRetentionMonths,
		ipEraseDays:     DefaultIPEraseDays,
		batchSize:       RetentionBatchSize,
		maxBatches:      RetentionMaxBatchesPerPass,
		deleteFn:        deleteFn,
		eraseIPFn:       eraseFn,
	}
}

func TestRetentionLoopDisabledBranchesNoop(t *testing.T) {
	// 两个键都显式 0：不启动循环、不触库（零值 *sql.DB 也不会被使用）。
	err := RunRetentionLoop(context.Background(), RetentionDeps{
		DB:              &sql.DB{},
		RetentionMonths: 0,
		IPEraseDays:     0,
	})
	if err != nil {
		t.Fatalf("both disabled = %v, want nil", err)
	}
}

func TestRetentionLoopRequiresRetentionDB(t *testing.T) {
	if err := RunRetentionLoop(context.Background(), RetentionDeps{
		RetentionMonths: 12, IPEraseDays: 90,
	}); err == nil {
		t.Fatal("nil db = nil error, want error")
	}
}

func TestRunOnceUsesCalendarMonthAndDayCutoffs(t *testing.T) {
	now := time.Date(2027, 2, 28, 12, 0, 0, 0, time.UTC)
	var deleteCutoff, eraseCutoff time.Time
	w := newTestRetentionWorker(
		func(_ context.Context, cutoff time.Time, _ int) (int64, error) {
			deleteCutoff = cutoff
			return 0, nil
		},
		func(_ context.Context, cutoff time.Time, _ int) (int64, error) {
			eraseCutoff = cutoff
			return 0, nil
		})
	w.now = func() time.Time { return now }

	w.runOnce(context.Background())

	// 12 个日历月前（不是 360 天）：2026-02-28。
	wantDelete := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	if !deleteCutoff.Equal(wantDelete) {
		t.Fatalf("row cutoff = %v, want %v", deleteCutoff, wantDelete)
	}
	// IP 擦除按固定 90 天。
	wantErase := now.Add(-90 * 24 * time.Hour)
	if !eraseCutoff.Equal(wantErase) {
		t.Fatalf("ip cutoff = %v, want %v", eraseCutoff, wantErase)
	}
}

func TestRunOnceZeroMonthDisablesOnlyRowDeletion(t *testing.T) {
	var deleteCalled, eraseCalled bool
	w := newTestRetentionWorker(
		func(context.Context, time.Time, int) (int64, error) { deleteCalled = true; return 0, nil },
		func(context.Context, time.Time, int) (int64, error) { eraseCalled = true; return 0, nil })
	w.retentionMonths = 0

	w.runOnce(context.Background())
	if deleteCalled {
		t.Fatal("retention_months=0 must not delete rows")
	}
	if !eraseCalled {
		t.Fatal("ip erase must still run when only retention_months=0")
	}
}

func TestRunOnceZeroDaysDisablesOnlyIPErasure(t *testing.T) {
	var deleteCalled, eraseCalled bool
	w := newTestRetentionWorker(
		func(context.Context, time.Time, int) (int64, error) { deleteCalled = true; return 0, nil },
		func(context.Context, time.Time, int) (int64, error) { eraseCalled = true; return 0, nil })
	w.ipEraseDays = 0

	w.runOnce(context.Background())
	if !deleteCalled {
		t.Fatal("row deletion must still run when only ip_erase_days=0")
	}
	if eraseCalled {
		t.Fatal("ip_erase_days=0 must not erase")
	}
}

func TestRunBatchesStopsBelowBatchAndHonorsMax(t *testing.T) {
	// 第一批满额、第二批不足：总 140，调用 2 次。
	calls := 0
	w := newTestRetentionWorker(nil, nil)
	w.batchSize = 100
	w.maxBatches = 10
	fn := func(context.Context, time.Time, int) (int64, error) {
		calls++
		if calls == 1 {
			return 100, nil
		}
		return 40, nil
	}
	n, err := w.runBatches(context.Background(), fn, time.Now())
	if err != nil || n != 140 || calls != 2 {
		t.Fatalf("batches: n=%d calls=%d err=%v, want 140/2/nil", n, calls, err)
	}

	// 永远满额：单轮上限截断，不会无限循环。
	calls = 0
	w.maxBatches = 3
	n, err = w.runBatches(context.Background(), func(context.Context, time.Time, int) (int64, error) {
		calls++
		return 100, nil
	}, time.Now())
	if err != nil || n != 300 || calls != 3 {
		t.Fatalf("max batches: n=%d calls=%d err=%v, want 300/3/nil", n, calls, err)
	}
}

func TestRunOnceSelfAuditBoundedAndOnlyOnChange(t *testing.T) {
	var details []json.RawMessage
	auditFn := func(_ context.Context, detail json.RawMessage) error {
		details = append(details, detail)
		return nil
	}

	// 无变化：不写自审计。
	w := newTestRetentionWorker(
		func(context.Context, time.Time, int) (int64, error) { return 0, nil },
		func(context.Context, time.Time, int) (int64, error) { return 0, nil })
	w.audit = auditFn
	w.runOnce(context.Background())
	if len(details) != 0 {
		t.Fatalf("no-op pass wrote %d self-audit rows, want 0", len(details))
	}

	// 有变化：无论分几批，每轮只写一条，且只含计数/窗口参数。
	w = newTestRetentionWorker(
		func(context.Context, time.Time, int) (int64, error) { return 500, nil },
		func(context.Context, time.Time, int) (int64, error) { return 70, nil })
	w.audit = auditFn
	w.runOnce(context.Background())
	if len(details) != 1 {
		t.Fatalf("self-audit rows = %d, want 1", len(details))
	}
	var payload map[string]any
	if err := json.Unmarshal(details[0], &payload); err != nil {
		t.Fatalf("self-audit detail is not JSON: %v", err)
	}
	for _, key := range []string{"deleted_rows", "erased_ips", "retention_months", "ip_erase_days"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("self-audit detail missing %q: %s", key, details[0])
		}
	}
}

func TestRunOnceDeleteFailureSkipsEraseAndAudit(t *testing.T) {
	var eraseCalled, auditCalled bool
	w := newTestRetentionWorker(
		func(context.Context, time.Time, int) (int64, error) {
			return 0, errors.New("permission denied")
		},
		func(context.Context, time.Time, int) (int64, error) { eraseCalled = true; return 0, nil })
	w.audit = func(context.Context, json.RawMessage) error { auditCalled = true; return nil }

	w.runOnce(context.Background())
	if eraseCalled {
		t.Fatal("ip erase ran after row deletion failed")
	}
	if auditCalled {
		t.Fatal("self-audit written after a failed pass")
	}
}

func TestAuditRetentionActionIsRegistered(t *testing.T) {
	category, ok := Category(ActionAuditRetention)
	if !ok {
		t.Fatalf("action %q is not in the audit category map", ActionAuditRetention)
	}
	if category != categoryOperation {
		t.Fatalf("category = %q, want operation", category)
	}
}
