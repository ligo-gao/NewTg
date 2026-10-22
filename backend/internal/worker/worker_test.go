package worker

// 不依赖 PG/Redis 的纯逻辑单测：错误分类、派生时间、分页与展示截断。
// 这些是调度器与 API 的共享判定，回归时最容易被误改。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNextDueAt(t *testing.T) {
	started := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	if got := nextDueAt(started, 0); got != nil {
		t.Fatalf("非周期任务 next_due_at = %v，期望 nil", got)
	}
	got := nextDueAt(started, 5*time.Minute)
	if got == nil || !got.Equal(started.Add(5*time.Minute)) {
		t.Fatalf("next_due_at = %v，期望 %v", got, started.Add(5*time.Minute))
	}
	if got.Location() != time.UTC {
		t.Fatalf("next_due_at 必须为 UTC，实际 %v", got.Location())
	}
}

func TestExecuteTaskClassifiesErrors(t *testing.T) {
	cases := []struct {
		name      string
		run       func(context.Context) (Result, error)
		wantClass string
	}{
		{"success", func(context.Context) (Result, error) { return Result{"ok": 1}, nil }, ""},
		{"execution", func(context.Context) (Result, error) {
			return nil, errors.New("upstream said no")
		}, ErrClassExecution},
		{"deadline", func(context.Context) (Result, error) {
			return nil, context.DeadlineExceeded
		}, ErrClassTimeout},
		{"blocked", func(context.Context) (Result, error) {
			return nil, &TaskBlockedError{Code: "log_partition_drop_disabled"}
		}, ErrClassBlocked},
		{"panic", func(context.Context) (Result, error) {
			panic("boom")
		}, ErrClassPanic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err, class := executeTask(context.Background(), Task{Name: tc.name, Run: tc.run})
			if class != tc.wantClass {
				t.Fatalf("class = %q，期望 %q", class, tc.wantClass)
			}
			if tc.wantClass == "" && err != nil {
				t.Fatalf("成功任务返回 err = %v", err)
			}
			_ = result
		})
	}
}

func TestExecuteTaskCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err, class := executeTask(ctx, Task{
		Name: "cancelled",
		Run: func(ctx context.Context) (Result, error) {
			return nil, ctx.Err()
		},
	})
	if class != ErrClassCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("class/err = %q/%v，期望 cancelled/context.Canceled", class, err)
	}
}

func TestTruncateHolder(t *testing.T) {
	short := "worker-1"
	if got := truncateHolder(short); got != short {
		t.Fatalf("短标识被改动: %q", got)
	}
	long := make([]byte, 80)
	for i := range long {
		long[i] = 'a'
	}
	if got := truncateHolder(string(long)); len(got) != 64 {
		t.Fatalf("截断长度 = %d，期望 64", len(got))
	}
}

func TestNormalizePage(t *testing.T) {
	cases := []struct {
		page, size       int
		wantPage, wantSz int
	}{
		{0, 0, 1, 20},
		{-1, 101, 1, 100},
		{3, 50, 3, 50},
		{2, 1000, 2, 100},
	}
	for _, tc := range cases {
		page, size := normalizePage(tc.page, tc.size)
		if page != tc.wantPage || size != tc.wantSz {
			t.Fatalf("normalizePage(%d,%d) = %d/%d，期望 %d/%d",
				tc.page, tc.size, page, size, tc.wantPage, tc.wantSz)
		}
	}
}

func TestRegistrySetEnabledUnknownTask(t *testing.T) {
	registry, err := NewRegistry(Task{Name: "known"})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if err := registry.SetEnabled("missing", func() bool { return false }, "code"); err == nil {
		t.Fatal("给未注册任务设置谓词必须失败")
	}
	if err := registry.SetEnabled("known", func() bool { return false }, "code"); err != nil {
		t.Fatalf("SetEnabled(known): %v", err)
	}
	task, _ := registry.Get("known")
	if task.Enabled == nil || task.Enabled() || task.BlockedCode != "code" {
		t.Fatalf("谓词未生效: %+v", task)
	}
}
