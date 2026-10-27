package platform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"newtg/gateway/internal/worker"
)

// TestModelPerfErrClass 钉住定时轮次错误 → 运行历史固定分类的映射。
//
// 为什么重要：error 列是管理员判断「这轮为什么没出数」的唯一线索。
// timeout 与 cancelled 写反时管理面会把超时显示成「被取消」（或反之），
// 诊断方向完全相反；原始 err 文本可能带上游响应/凭据，绝不能被当作分类
// 落库。分类口径必须与 worker.executeTask 一致，否则同一任务的手动行与
// 定时行会出现两种错误语义。
func TestModelPerfErrClass(t *testing.T) {
	// 固定分类值本身是管理面/前端的契约（按字符串过滤、展示），先钉住常量，
	// 防止有人改文案而下面的映射测试因为直接用常量比较仍然全绿。
	if worker.ErrClassTimeout != "timeout" ||
		worker.ErrClassCancelled != "cancelled" ||
		worker.ErrClassExecution != "execution_error" {
		t.Fatalf("worker error classes drifted: timeout=%q cancelled=%q execution=%q",
			worker.ErrClassTimeout, worker.ErrClassCancelled, worker.ErrClassExecution)
	}
	if worker.ErrClassTimeout == worker.ErrClassCancelled ||
		worker.ErrClassTimeout == worker.ErrClassExecution ||
		worker.ErrClassCancelled == worker.ErrClassExecution {
		t.Fatalf("worker error classes must be distinct: %q %q %q",
			worker.ErrClassTimeout, worker.ErrClassCancelled, worker.ErrClassExecution)
	}

	background := context.Background()
	cancelledCtx := func() context.Context {
		ctx, cancel := context.WithCancel(background)
		cancel()
		return ctx
	}
	// 已过期的 deadline ctx：ctx.Err() 恒为 DeadlineExceeded，无需等待。
	deadlineCtx := func() context.Context {
		ctx, cancel := context.WithDeadline(background, time.Now().Add(-time.Minute))
		t.Cleanup(cancel)
		return ctx
	}

	cases := []struct {
		name string
		ctx  func() context.Context
		err  error
		want string
	}{
		{
			name: "deadline exceeded in error",
			ctx:  func() context.Context { return background },
			err:  fmt.Errorf("probe: %w", context.DeadlineExceeded),
			want: worker.ErrClassTimeout,
		},
		{
			name: "deadline exceeded on context",
			ctx:  deadlineCtx,
			err:  errors.New("round aborted"),
			want: worker.ErrClassTimeout,
		},
		{
			// 超时判定先于取消：ctx 已取消但 err 是超时包装时仍记 timeout，
			// 这正是「两个分支写反」会翻转的那一格。
			name: "deadline error wins over cancelled context",
			ctx:  cancelledCtx,
			err:  fmt.Errorf("probe: %w", context.DeadlineExceeded),
			want: worker.ErrClassTimeout,
		},
		{
			name: "cancelled error and cancelled context",
			ctx:  cancelledCtx,
			err:  context.Canceled,
			want: worker.ErrClassCancelled,
		},
		{
			name: "cancelled context with generic error",
			ctx:  cancelledCtx,
			err:  errors.New("shutting down"),
			want: worker.ErrClassCancelled,
		},
		{
			// 与 worker.executeTask 同口径：取消只看 ctx.Err()，不看 err 本身。
			// 生产里 RunOnce 的取消一定伴随 ctx 取消，这条只是钉住框架语义。
			name: "bare cancelled error with healthy context falls through",
			ctx:  func() context.Context { return background },
			err:  context.Canceled,
			want: worker.ErrClassExecution,
		},
		{
			name: "generic error",
			ctx:  func() context.Context { return background },
			err:  errors.New("upstream returned 502"),
			want: worker.ErrClassExecution,
		},
		{
			// 记录器只在 roundErr != nil 时调用本函数（失败行才写 error 列，
			// 成功行留空）。这里仍钉住 nil 走 default 分支：函数契约不因调用
			// 方式而变，将来若有新的调用方直接传 nil 也不会拿到空分类。
			name: "nil error",
			ctx:  func() context.Context { return background },
			err:  nil,
			want: worker.ErrClassExecution,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelPerfErrClass(tc.ctx(), tc.err); got != tc.want {
				t.Fatalf("modelPerfErrClass(ctx, %v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
