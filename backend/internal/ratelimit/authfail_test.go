package ratelimit

// R-03 预认证失败限流的内核单测（miniredis）。
//
// 契约：60 次失败 / IP / 分钟，达到阈值即锁 10 分钟；锁按 IP 隔离；
// 窗口过期后重新计数；Redis 故障原样上抛（fail-open 由调用方决定，
// 本包不替调用方吞错）。

import (
	"context"
	"testing"
	"time"
)

const authFailTestIP = "203.0.113.7"

func TestRecordAuthFailureLocksAtThreshold(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()

	for i := 1; i <= AuthFailureLimit; i++ {
		blocked, _, err := l.AuthFailureBlocked(ctx, authFailTestIP)
		if err != nil {
			t.Fatalf("第 %d 次 AuthFailureBlocked error: %v", i, err)
		}
		if blocked {
			t.Fatalf("第 %d 次失败前不应锁定（阈值 %d）", i, AuthFailureLimit)
		}
		if err := l.RecordAuthFailure(ctx, authFailTestIP); err != nil {
			t.Fatalf("第 %d 次 RecordAuthFailure error: %v", i, err)
		}
	}

	// 第 limit 次失败后已到阈值：下一次请求（第 limit+1 次）必须被锁。
	blocked, retryAfter, err := l.AuthFailureBlocked(ctx, authFailTestIP)
	if err != nil {
		t.Fatalf("锁定期 AuthFailureBlocked error: %v", err)
	}
	if !blocked {
		t.Fatalf("%d 次失败后未锁定；阈值语义必须是「第 %d 次失败即锁」",
			AuthFailureLimit, AuthFailureLimit)
	}
	if retryAfter <= 0 || retryAfter > AuthFailureLockTTL {
		t.Fatalf("RetryAfter = %v，期望 (0, %v]", retryAfter, AuthFailureLockTTL)
	}
	if ttl := mr.TTL(AuthFailureLockKey(authFailTestIP)); ttl <= 0 || ttl > AuthFailureLockTTL {
		t.Fatalf("锁键 TTL = %v，期望 (0, %v]（超限必须锁 10 分钟）", ttl, AuthFailureLockTTL)
	}
	// 计数键仍活在 1 分钟窗口内：锁由独立键承载，不能因为计数器过期而提前解锁。
	if !mr.Exists(AuthFailureKey(authFailTestIP)) {
		t.Fatal("阈值达成后 1 分钟窗口内的计数键不应消失")
	}
}

func TestAuthFailureLockIsPerIP(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()

	for i := 0; i < AuthFailureLimit; i++ {
		if err := l.RecordAuthFailure(ctx, authFailTestIP); err != nil {
			t.Fatalf("RecordAuthFailure error: %v", err)
		}
	}

	blocked, _, err := l.AuthFailureBlocked(ctx, authFailTestIP)
	if err != nil || !blocked {
		t.Fatalf("已超阈值的 IP 必须被锁：blocked=%v err=%v", blocked, err)
	}

	// 邻居 IP 不受影响（TRUSTED_PROXIES 修复后每个真实客户端独立计数）。
	other := "203.0.113.8"
	blocked, _, err = l.AuthFailureBlocked(ctx, other)
	if err != nil {
		t.Fatalf("其它 IP AuthFailureBlocked error: %v", err)
	}
	if blocked {
		t.Fatal("另一个 IP 不得因邻居超限被锁")
	}
	if mr.Exists(AuthFailureLockKey(other)) {
		t.Fatal("其它 IP 不应产生锁键")
	}
}

func TestAuthFailureLockExpires(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()

	for i := 0; i < AuthFailureLimit; i++ {
		if err := l.RecordAuthFailure(ctx, authFailTestIP); err != nil {
			t.Fatalf("RecordAuthFailure error: %v", err)
		}
	}
	if blocked, _, _ := l.AuthFailureBlocked(ctx, authFailTestIP); !blocked {
		t.Fatal("前置条件失败：应已锁定")
	}

	mr.FastForward(AuthFailureLockTTL + time.Second)

	blocked, _, err := l.AuthFailureBlocked(ctx, authFailTestIP)
	if err != nil {
		t.Fatalf("过期后 AuthFailureBlocked error: %v", err)
	}
	if blocked {
		t.Fatalf("锁应在 %v 后自动解除（不能是永久桶）", AuthFailureLockTTL)
	}
}

func TestAuthFailureWindowResets(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()

	// 窗口内只差一次就达阈值，但窗口一过计数必须清零。
	for i := 0; i < AuthFailureLimit-1; i++ {
		if err := l.RecordAuthFailure(ctx, authFailTestIP); err != nil {
			t.Fatalf("RecordAuthFailure error: %v", err)
		}
	}
	mr.FastForward(AuthFailureWindow + time.Second)
	if err := l.RecordAuthFailure(ctx, authFailTestIP); err != nil {
		t.Fatalf("窗口重置后 RecordAuthFailure error: %v", err)
	}
	blocked, _, err := l.AuthFailureBlocked(ctx, authFailTestIP)
	if err != nil {
		t.Fatalf("AuthFailureBlocked error: %v", err)
	}
	if blocked {
		t.Fatal("新窗口第一次失败不应触发锁定（旧窗口计数必须过期）")
	}
}

func TestAuthFailureLockWithoutTTLIsRepaired(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()

	// 模拟异常数据：锁键存在但没有过期时间。必须判定为已锁定，并补上
	// 有限 TTL——否则就是永久封禁（A11 同口径）。
	if err := mr.Set(AuthFailureLockKey(authFailTestIP), "1"); err != nil {
		t.Fatalf("miniredis Set: %v", err)
	}
	blocked, retryAfter, err := l.AuthFailureBlocked(ctx, authFailTestIP)
	if err != nil {
		t.Fatalf("AuthFailureBlocked error: %v", err)
	}
	if !blocked {
		t.Fatal("无 TTL 的锁键仍表示已锁定")
	}
	if retryAfter != AuthFailureLockTTL {
		t.Fatalf("RetryAfter = %v，期望 %v", retryAfter, AuthFailureLockTTL)
	}
	if ttl := mr.TTL(AuthFailureLockKey(authFailTestIP)); ttl <= 0 || ttl > AuthFailureLockTTL {
		t.Fatalf("修复后的锁键 TTL = %v，期望 (0, %v]", ttl, AuthFailureLockTTL)
	}
}

func TestAuthFailureRedisErrorSurfaces(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()
	mr.Close()

	if err := l.RecordAuthFailure(ctx, authFailTestIP); err == nil {
		t.Fatal("Redis 不可用时 RecordAuthFailure 必须返回 error（由调用方 fail-open）")
	}
	if _, _, err := l.AuthFailureBlocked(ctx, authFailTestIP); err == nil {
		t.Fatal("Redis 不可用时 AuthFailureBlocked 必须返回 error（由调用方 fail-open）")
	}
}

func TestAuthFailureEmptyIPRejected(t *testing.T) {
	_, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	ctx := context.Background()

	if err := l.RecordAuthFailure(ctx, "  "); err == nil {
		t.Fatal("空 IP 必须报错：共享空键会把所有匿名来源算成一个桶")
	}
	if _, _, err := l.AuthFailureBlocked(ctx, ""); err == nil {
		t.Fatal("空 IP 必须报错")
	}
}
