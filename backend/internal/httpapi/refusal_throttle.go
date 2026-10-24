package httpapi

import (
	"context"
	"sync"
	"time"

	"newtg/gateway/internal/billing"
)

// 拒绝行的按用户写入节流（每进程内存计数）。
//
// 背景：中继前的拒绝（结构非法、鉴权失败、模型准入、限流）发生在入口限流
// **之前**。一个已认证的调用方可以用畸形请求循环制造失败，而每条拒绝都要
// 写一行 usage_logs——没有上限时，这个功能本身就是一条写放大通道。
//
// 语义：同一用户在窗口内最多写入 refusalLogLimitPerWindow 行，超出的拒绝
// 直接丢弃（RecordRefusal 返回 nil；调用方本来就把它当作尽力而为的旁路，
// 丢弃不改变任何 HTTP 响应）。UserID==0（无法说明是谁的鉴权失败）共用
// 同一个桶，因此匿名拒绝同样有上限。
//
// 计数留在进程内而不是 Redis：写日志这条旁路必须在 Redis 故障时继续可用，
// 且鉴权失败的记录不能因为限流器不可用而反向阻塞请求。代价是多副本部署时
// 上限按副本数放大——这是一个明确的容量取舍，不是遗漏。
const (
	refusalLogLimitPerWindow = 30
	refusalLogWindow         = time.Minute
	// refusalLogMaxBuckets 是内存桶的上界。超过时先清扫过期桶；仍然满则
	// 整体重置（宁可放宽一拍，也不让一个可被构造的 user_id 集合撑爆内存）。
	refusalLogMaxBuckets = 4096
)

// NewThrottledRefusalRecorder 给拒绝记录器加上按用户节流。
//
// inner 为 nil 时返回 nil：保持「未装配日志库 = no-op」的既有语义，
// 避免调用方需要额外判断一层。
func NewThrottledRefusalRecorder(inner RefusalRecorder) RefusalRecorder {
	if inner == nil {
		return nil
	}
	return newRefusalThrottle(inner, refusalLogLimitPerWindow, refusalLogWindow, time.Now)
}

type refusalThrottle struct {
	inner  RefusalRecorder
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	buckets map[int64]refusalBucket
}

type refusalBucket struct {
	start time.Time
	count int
}

// newRefusalThrottle 组装节流器；now 注入时钟以便单测推进窗口。
func newRefusalThrottle(inner RefusalRecorder, limit int, window time.Duration,
	now func() time.Time) *refusalThrottle {
	return &refusalThrottle{
		inner:   inner,
		limit:   limit,
		window:  window,
		now:     now,
		buckets: make(map[int64]refusalBucket),
	}
}

// RecordRefusal 实现 RefusalRecorder：窗口内超限的拒绝被静默丢弃。
func (t *refusalThrottle) RecordRefusal(ctx context.Context, in billing.Refusal) error {
	if !t.allow(in.UserID) {
		return nil
	}
	return t.inner.RecordRefusal(ctx, in)
}

// allow 判断该用户本次是否还能写一行，并在允许时占用一个名额。
func (t *refusalThrottle) allow(userID int64) bool {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()

	bucket, ok := t.buckets[userID]
	if !ok || now.Sub(bucket.start) >= t.window {
		if !ok && len(t.buckets) >= refusalLogMaxBuckets {
			t.sweepLocked(now)
		}
		t.buckets[userID] = refusalBucket{start: now, count: 1}
		return true
	}
	if bucket.count >= t.limit {
		return false
	}
	bucket.count++
	t.buckets[userID] = bucket
	return true
}

// sweepLocked 清掉过期桶；若仍然达到上界，整体重置以保住内存上界。
func (t *refusalThrottle) sweepLocked(now time.Time) {
	for id, bucket := range t.buckets {
		if now.Sub(bucket.start) >= t.window {
			delete(t.buckets, id)
		}
	}
	if len(t.buckets) >= refusalLogMaxBuckets {
		t.buckets = make(map[int64]refusalBucket)
	}
}
