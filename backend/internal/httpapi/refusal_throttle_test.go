package httpapi

import (
	"context"
	"testing"
	"time"

	"newtg/gateway/internal/billing"
)

// countingRecorder 记录被放行的请求 id，用于观察节流结果。
type countingRecorder struct {
	ids []string
	err error
}

func (c *countingRecorder) RecordRefusal(_ context.Context, in billing.Refusal) error {
	c.ids = append(c.ids, in.RequestID)
	return c.err
}

func refusalFor(userID int64, requestID string) billing.Refusal {
	return billing.Refusal{
		RequestID: requestID,
		UserID:    userID,
		Stage:     billing.RefusalStageAuth,
	}
}

// TestRefusalThrottleLimitsPerUser 钉死「同一用户在窗口内只写 N 行」，
// 且用户之间互不影响。
func TestRefusalThrottleLimitsPerUser(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	inner := &countingRecorder{}
	throttle := newRefusalThrottle(inner, 3, time.Minute, func() time.Time { return now })

	for i := 0; i < 5; i++ {
		if err := throttle.RecordRefusal(context.Background(), refusalFor(7, "u7")); err != nil {
			t.Fatalf("RecordRefusal: %v", err)
		}
	}
	if got := len(inner.ids); got != 3 {
		t.Fatalf("user 7 写入 %d 行, want 3", got)
	}
	// 另一个用户不受 user 7 的配额影响。
	if err := throttle.RecordRefusal(context.Background(), refusalFor(8, "u8")); err != nil {
		t.Fatalf("RecordRefusal: %v", err)
	}
	if got := len(inner.ids); got != 4 {
		t.Fatalf("total 写入 %d 行, want 4", got)
	}
}

// TestRefusalThrottleWindowRollover 验证窗口到期后重新放行。
func TestRefusalThrottleWindowRollover(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	inner := &countingRecorder{}
	throttle := newRefusalThrottle(inner, 1, time.Minute, func() time.Time { return now })

	_ = throttle.RecordRefusal(context.Background(), refusalFor(7, "first"))
	_ = throttle.RecordRefusal(context.Background(), refusalFor(7, "blocked"))
	if got := len(inner.ids); got != 1 {
		t.Fatalf("窗口内写入 %d 行, want 1", got)
	}

	now = now.Add(time.Minute)
	_ = throttle.RecordRefusal(context.Background(), refusalFor(7, "next-window"))
	if got := len(inner.ids); got != 2 {
		t.Fatalf("新窗口写入 %d 行, want 2", got)
	}
	if inner.ids[1] != "next-window" {
		t.Fatalf("新窗口第一行 = %q, want next-window", inner.ids[1])
	}
}

// TestRefusalThrottleAnonymousShared 验证 UserID==0 共用同一个桶：
// 无法说明身份的鉴权失败同样有上限，避免匿名请求成为写放大入口。
func TestRefusalThrottleAnonymousShared(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	inner := &countingRecorder{}
	throttle := newRefusalThrottle(inner, 2, time.Minute, func() time.Time { return now })

	for i := 0; i < 4; i++ {
		_ = throttle.RecordRefusal(context.Background(), refusalFor(0, "anon"))
	}
	if got := len(inner.ids); got != 2 {
		t.Fatalf("匿名写入 %d 行, want 2", got)
	}
}

// TestRefusalThrottleBucketBound 验证桶数达到上界时会清扫过期桶，
// 不会随被构造的 user_id 无界增长。
func TestRefusalThrottleBucketBound(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	throttle := newRefusalThrottle(&countingRecorder{}, 1, time.Minute,
		func() time.Time { return now })

	for i := int64(0); i < refusalLogMaxBuckets; i++ {
		throttle.allow(i)
	}
	if got := len(throttle.buckets); got != refusalLogMaxBuckets {
		t.Fatalf("buckets = %d, want %d", got, refusalLogMaxBuckets)
	}

	// 窗口推进后，新的 user_id 触发清扫：过期桶全部释放。
	now = now.Add(2 * time.Minute)
	throttle.allow(-1)
	if got := len(throttle.buckets); got != 1 {
		t.Fatalf("清扫后 buckets = %d, want 1", got)
	}
}

// TestRefusalThrottlePassesErrors 验证内层错误原样返回：节流只决定「写不写」，
// 不改变落库成功/失败的语义。
func TestRefusalThrottlePassesErrors(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	wantErr := context.Canceled
	inner := &countingRecorder{err: wantErr}
	throttle := newRefusalThrottle(inner, 1, time.Minute, func() time.Time { return now })

	if err := throttle.RecordRefusal(context.Background(), refusalFor(7, "x")); err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestNewThrottledRefusalRecorderNilInner 验证未装配日志库时仍是 no-op。
func TestNewThrottledRefusalRecorderNilInner(t *testing.T) {
	if got := NewThrottledRefusalRecorder(nil); got != nil {
		t.Fatalf("NewThrottledRefusalRecorder(nil) = %#v, want nil", got)
	}
}
