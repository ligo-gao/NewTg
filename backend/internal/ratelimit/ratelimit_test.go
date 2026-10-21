package ratelimit

// M5-a 限流器内核单测（miniredis）。用例编号对应 m5-research.md §4.1 的
// A1–A12；A10（阈值解析）在 settings_test.go，A9（键形状）在 keys_test.go。

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const testWindow = time.Minute

// newLimiterTestRedis 启动一个 miniredis 与独立客户端。
// MaxRetries=-1 让 A6 的「Redis 故障」立即返回错误，而不是重试到超时。
func newLimiterTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func mustAllow(t *testing.T, l *Limiter, bucket Bucket, key string) Decision {
	t.Helper()
	decision, err := l.Allow(context.Background(), bucket, key, 1)
	if err != nil {
		t.Fatalf("Allow(%q) 返回 error: %v", key, err)
	}
	return decision
}

// A1：limit<=0 = 不限——恒放行且不建键（0 被误当阈值会变成「全拒」）。
func TestAllowZeroLimitIsUnlimited(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	key := UserKey(42)

	for i := 0; i < 100; i++ {
		decision := mustAllow(t, l, Bucket{Name: BucketUserModel, Limit: 0, Window: testWindow}, key)
		if !decision.Allowed {
			t.Fatalf("limit=0 第 %d 次被拒；0 必须表示不限", i+1)
		}
		if decision.RetryAfter != 0 {
			t.Fatalf("limit=0 的 RetryAfter = %v，期望 0", decision.RetryAfter)
		}
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("limit=0 不应建键，实际键 = %v", keys)
	}

	// 负阈值同样按「不限」处理，不能因脏配置把所有请求打死。
	if decision := mustAllow(t, l, Bucket{Limit: -5, Window: testWindow}, key); !decision.Allowed {
		t.Fatal("负阈值必须按不限处理")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("负阈值不应建键，实际键 = %v", keys)
	}

	// M5-a 复核 M-2：证明短路路径「不访问 Redis」——关掉后端后仍放行且无 error。
	// 若实现先访问 Redis 再判断 limit，这里会拿到 error 或被拒。
	mr.Close()
	if decision, err := l.Allow(context.Background(),
		Bucket{Limit: 0, Window: testWindow}, key, 1); err != nil || !decision.Allowed {
		t.Fatalf("limit=0 在 Redis 关闭后仍应放行：decision=%+v err=%v", decision, err)
	}
}

// A2：第 N 次通过、第 N+1 次拒绝；Retry-After 为窗口剩余整秒。
func TestAllowBoundary(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	bucket := Bucket{Name: BucketUserModel, Limit: 60, Window: testWindow}
	key := UserKey(1)

	for i := 1; i <= 60; i++ {
		if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
			t.Fatalf("第 %d 次应放行（limit=60）", i)
		}
	}
	decision := mustAllow(t, l, bucket, key)
	if decision.Allowed {
		t.Fatal("第 61 次应被拒绝")
	}
	if decision.RetryAfter <= 0 || decision.RetryAfter > testWindow {
		t.Fatalf("RetryAfter = %v，期望 (0, %v]", decision.RetryAfter, testWindow)
	}
	if decision.RetryAfter%time.Second != 0 {
		t.Fatalf("RetryAfter = %v，必须向上取整到整秒", decision.RetryAfter)
	}
	// 被拒请求同样已自增（计数口径含失败请求）。
	if got := mustKeyValue(t, mr, key); got != "61" {
		t.Fatalf("桶计数 = %q，期望 %q", got, "61")
	}
}

// A3：窗口滚动后重新放行；Retry-After 用真实剩余 TTL 而非 window 常量。
func TestAllowWindowRolls(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	bucket := Bucket{Limit: 1, Window: testWindow}
	key := UserKey(7)

	if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
		t.Fatal("窗口内第 1 次应放行")
	}
	if decision := mustAllow(t, l, bucket, key); decision.Allowed {
		t.Fatal("窗口内第 2 次应被拒绝")
	}

	// 窗口过半时被拒：Retry-After 必须是剩余 30s，而不是 60s。
	mr.FastForward(30 * time.Second)
	denied := mustAllow(t, l, bucket, key)
	if denied.Allowed {
		t.Fatal("窗口未结束前仍应拒绝")
	}
	if denied.RetryAfter != 30*time.Second {
		t.Fatalf("窗口过半的 RetryAfter = %v，期望 30s（必须读 PTTL，不能用 window 常量）",
			denied.RetryAfter)
	}

	// 越过窗口后键过期，重新放行且 TTL 重新计时。
	mr.FastForward(31 * time.Second)
	if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
		t.Fatal("窗口滚动后应重新放行")
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > testWindow {
		t.Fatalf("滚动后的 TTL = %v，期望 (0, %v]", ttl, testWindow)
	}
}

// A4：用户/令牌/三个桶彼此隔离。
func TestAllowKeysAreIsolated(t *testing.T) {
	_, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	bucket := Bucket{Limit: 1, Window: testWindow}

	for _, key := range []string{UserKey(1), TokenKey(2), UserGlobalKey(1)} {
		if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
			t.Fatalf("键 %q 首次应放行", key)
		}
		if decision := mustAllow(t, l, bucket, key); decision.Allowed {
			t.Fatalf("键 %q 第二次应被拒绝", key)
		}
	}

	// 不同 userID / tokenID 各自独立。
	for _, key := range []string{UserKey(2), TokenKey(3)} {
		if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
			t.Fatalf("独立键 %q 首次应放行", key)
		}
	}
}

// A5（行为面）：分组覆盖阈值生效；覆盖 0 = 该组不限；未覆盖组走默认。
// 覆盖值从真实配置 JSON 解析（`Resolve`），而不是直接构造 Settings——这样
// 「覆盖值解析放大/缩小」的变异也能被本用例杀死（复核 I-2）。
func TestAllowGroupOverrideThreshold(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	log, _ := newTestLogger()
	settings := Resolve(fakeConfig{
		KeyUserModelRPM:   "60",
		KeyGroupOverrides: `{"vip":1200,"free":0}`,
	}, log)

	// vip：61 次仍放行（默认 60 会在第 61 次拒绝）。
	vipBucket := Bucket{Limit: settings.UserModelLimit("vip"), Window: testWindow}
	for i := 0; i < 61; i++ {
		if decision := mustAllow(t, l, vipBucket, UserKey(1)); !decision.Allowed {
			t.Fatalf("vip 第 %d 次被拒；覆盖值 1200 应生效", i+1)
		}
	}

	// free：覆盖值 0 表示不限，不建键。
	freeBucket := Bucket{Limit: settings.UserModelLimit("free"), Window: testWindow}
	freeKey := UserKey(2)
	for i := 0; i < 70; i++ {
		if decision := mustAllow(t, l, freeBucket, freeKey); !decision.Allowed {
			t.Fatalf("free 第 %d 次被拒；覆盖值 0 表示不限", i+1)
		}
	}
	if mr.Exists(freeKey) {
		t.Fatalf("覆盖值 0 不应建键 %q", freeKey)
	}

	// 未覆盖组：走默认 60。
	unknownBucket := Bucket{Limit: settings.UserModelLimit("unknown"), Window: testWindow}
	unknownKey := UserKey(3)
	for i := 0; i < 60; i++ {
		mustAllow(t, l, unknownBucket, unknownKey)
	}
	if decision := mustAllow(t, l, unknownBucket, unknownKey); decision.Allowed {
		t.Fatal("未覆盖组第 61 次应被默认阈值拒绝")
	}
}

// A6：Redis 故障时 Allow 返回 error，不 panic、不静默放行；
// 错误路径仍回填 bucket 的 FailMode（复核 M-1），供调用方直接决策。
func TestAllowRedisFailureReturnsError(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	mr.Close()

	decision, err := l.Allow(context.Background(),
		Bucket{Limit: 60, Window: testWindow, FailMode: FailOpen}, UserKey(1), 1)
	if err == nil {
		t.Fatal("Redis 不可用时必须返回 error，由调用方决定 fail 策略")
	}
	if decision.Allowed {
		t.Fatalf("故障时 Decision 不得声称放行：%+v", decision)
	}
	if decision.FailMode != FailOpen {
		t.Fatalf("错误路径 FailMode = %v，期望回填 bucket 的 %v", decision.FailMode, FailOpen)
	}
}

// A7：两个 Limiter 实例共享同一 Redis（跨副本）时合计只放行 limit 次。
func TestAllowSharedAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	newClient := func() *redis.Client {
		client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	// 两个副本各自持有客户端，但指向同一个 Redis：计数必须共享。
	replicaA := New(newClient())
	replicaB := New(newClient())
	bucket := Bucket{Limit: 60, Window: testWindow}
	key := UserKey(9)

	allowed := 0
	for i := 0; i < 100; i++ {
		l := replicaA
		if i%2 == 1 {
			l = replicaB
		}
		decision := mustAllow(t, l, bucket, key)
		if decision.Allowed {
			allowed++
		}
	}
	if allowed != 60 {
		t.Fatalf("两个副本合计放行 %d 次，期望 60", allowed)
	}
}

// A8：100 goroutine 并发抢同一桶，恰好 limit 个放行（Lua 原子性）。
func TestAllowConcurrentExactlyLimit(t *testing.T) {
	_, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	bucket := Bucket{Limit: 60, Window: testWindow}
	key := UserKey(11)

	var allowed atomic.Int64
	var failed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			decision, err := l.Allow(context.Background(), bucket, key, 1)
			if err != nil {
				failed.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if failed.Load() != 0 {
		t.Fatalf("%d 个并发请求返回 error", failed.Load())
	}
	if got := allowed.Load(); got != 60 {
		t.Fatalf("并发放行 %d 次，期望恰好 60", got)
	}
}

// A11：预置无 TTL 的键在调用后补上 TTL，不产生永久桶。
func TestAllowRepairsMissingTTL(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	key := UserKey(5)
	mr.Set(key, "5")

	if ttl := mr.TTL(key); ttl != 0 {
		t.Fatalf("前置条件失败：TTL = %v，期望无 TTL", ttl)
	}
	decision := mustAllow(t, l, Bucket{Limit: 10, Window: testWindow}, key)
	if !decision.Allowed {
		t.Fatal("计数 6 <= limit 10，应放行")
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > testWindow {
		t.Fatalf("调用后 TTL = %v，期望 (0, %v]（必须自愈）", ttl, testWindow)
	}
	if got := mustKeyValue(t, mr, key); got != "6" {
		t.Fatalf("桶计数 = %q，期望 %q", got, "6")
	}
}

// A12：窗口中途调高阈值立即生效，已累积计数不清零。
func TestAllowThresholdChangeKeepsCount(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	key := UserKey(13)

	low := Bucket{Limit: 60, Window: testWindow}
	for i := 0; i < 60; i++ {
		mustAllow(t, l, low, key)
	}
	// 模拟配置热更新：同一键、同一窗口，只换阈值。
	high := Bucket{Limit: 120, Window: testWindow}
	for i := 0; i < 60; i++ {
		if decision := mustAllow(t, l, high, key); !decision.Allowed {
			t.Fatalf("阈值调高后第 %d 次应放行", 61+i)
		}
	}
	if got := mustKeyValue(t, mr, key); got != "120" {
		t.Fatalf("桶计数 = %q，期望 120（改阈值不得重置计数）", got)
	}
	if decision := mustAllow(t, l, high, key); decision.Allowed {
		t.Fatal("达到新阈值 120 后应拒绝")
	}
	// 计数继续累加而不是从 1 开始。
	if got := mustKeyValue(t, mr, key); got != strconv.Itoa(121) {
		t.Fatalf("桶计数 = %q，期望 121", got)
	}
}

// cost 形参的契约：cost<=0 不消耗名额；cost>1 按量自增。
func TestAllowCostSemantics(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	key := UserKey(21)
	bucket := Bucket{Limit: 10, Window: testWindow}

	if decision := mustAllowCost(t, l, bucket, key, 0); !decision.Allowed {
		t.Fatal("cost=0 应放行")
	}
	if mr.Exists(key) {
		t.Fatal("cost=0 不应建键")
	}
	if decision := mustAllowCost(t, l, bucket, key, 3); !decision.Allowed {
		t.Fatal("cost=3 在 limit=10 内应放行")
	}
	if got := mustKeyValue(t, mr, key); got != "3" {
		t.Fatalf("桶计数 = %q，期望 3", got)
	}
	if decision := mustAllowCost(t, l, bucket, key, 8); decision.Allowed {
		t.Fatal("3+8=11 > 10 应拒绝")
	}
}

func mustAllowCost(t *testing.T, l *Limiter, bucket Bucket, key string, cost int) Decision {
	t.Helper()
	decision, err := l.Allow(context.Background(), bucket, key, cost)
	if err != nil {
		t.Fatalf("Allow(cost=%d) 返回 error: %v", cost, err)
	}
	return decision
}

// 空键是接线错误，必须返回 error（不能静默共享空桶）；错误路径保留 FailMode。
// 这里用 FailOpen（非零值），否则「未回填」与零值 FailClosed 无法区分。
func TestAllowRejectsEmptyKey(t *testing.T) {
	_, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	decision, err := l.Allow(context.Background(),
		Bucket{Limit: 60, Window: testWindow, FailMode: FailOpen}, "", 1)
	if err == nil {
		t.Fatal("空键必须返回 error")
	}
	if decision.FailMode != FailOpen {
		t.Fatalf("错误路径 FailMode = %v，期望 %v", decision.FailMode, FailOpen)
	}
}

// nil Limiter 同样是接线错误：返回 error 且保留 FailMode（不得 nil 解引用 panic）。
func TestAllowNilLimiterReturnsError(t *testing.T) {
	var l *Limiter
	decision, err := l.Allow(context.Background(),
		Bucket{Limit: 60, Window: testWindow, FailMode: FailOpen}, UserKey(1), 1)
	if err == nil {
		t.Fatal("nil Limiter 必须返回 error")
	}
	if decision.FailMode != FailOpen {
		t.Fatalf("错误路径 FailMode = %v，期望 %v", decision.FailMode, FailOpen)
	}
}

// mustKeyValue 读桶计数（miniredis 的 Get 返回 (value, error)）。
func mustKeyValue(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	value, err := mr.Get(key)
	if err != nil {
		t.Fatalf("读取键 %q 失败: %v", key, err)
	}
	return value
}

// 复核 I-2：覆盖值 1200 的精确边界——第 1200 次放行、第 1201 次拒绝。
// 覆盖值经真实 JSON 解析（Resolve），因此「解析放大/缩小」的变异会在这里变红。
func TestAllowGroupOverrideBoundary1200(t *testing.T) {
	_, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	log, _ := newTestLogger()
	settings := Resolve(fakeConfig{
		KeyGroupOverrides: `{"vip":1200}`,
	}, log)
	if got := settings.UserModelLimit("vip"); got != 1200 {
		t.Fatalf("解析后的 vip 阈值 = %d，期望 1200", got)
	}
	bucket := Bucket{Limit: settings.UserModelLimit("vip"), Window: testWindow}
	key := UserKey(31)

	for i := 1; i <= 1200; i++ {
		if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
			t.Fatalf("vip 第 %d 次应放行（覆盖值 1200）", i)
		}
	}
	if decision := mustAllow(t, l, bucket, key); decision.Allowed {
		t.Fatal("vip 第 1201 次应被拒绝")
	}
}

// 复核 I-1：RetryAfter 的「向上取整到整秒」与「最小 1s」。
func TestRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	cases := []struct {
		name        string
		ttl, window time.Duration
		want        time.Duration
	}{
		{"sub-second ttl ceils to 1s", 500 * time.Millisecond, time.Minute, time.Second},
		{"1ns ceils to 1s", time.Nanosecond, time.Minute, time.Second},
		{"fractional seconds ceil", 30500 * time.Millisecond, time.Minute, 31 * time.Second},
		{"exact second unchanged", 30 * time.Second, time.Minute, 30 * time.Second},
		{"ttl capped at window", 2 * time.Minute, time.Minute, time.Minute},
		{"non-positive ttl uses window", -time.Second, time.Minute, time.Minute},
		// 1s 下限是防御分支：Allow 会把 Window<=0 归一为 1 分钟，脚本也会把
		// PTTL<0 修成窗口，因此 (0,0) 只能由直接调用触发。这里钉住该契约。
		{"1s floor", 0, 0, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfter(tc.ttl, tc.window); got != tc.want {
				t.Errorf("retryAfter(%v, %v) = %v，期望 %v", tc.ttl, tc.window, got, tc.want)
			}
		})
	}
}

// 复核 I-1（计数路径）：预置 500ms TTL 的桶，被拒时 RetryAfter 必须是 1s。
func TestAllowSubSecondTTLReturnsOneSecond(t *testing.T) {
	mr, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	key := UserKey(33)
	if err := mr.Set(key, "1"); err != nil {
		t.Fatalf("预置键失败: %v", err)
	}
	mr.SetTTL(key, 500*time.Millisecond)

	decision := mustAllow(t, l, Bucket{Limit: 1, Window: testWindow}, key)
	if decision.Allowed {
		t.Fatal("count=2 > limit=1 应被拒绝")
	}
	if decision.RetryAfter != time.Second {
		t.Fatalf("RetryAfter = %v，期望向上取整为 1s", decision.RetryAfter)
	}
}

// 复核 M-3：New 不拥有注入的客户端——Close 不能关闭调用方的连接池。
func TestNewDoesNotOwnInjectedClient(t *testing.T) {
	_, rdb := newLimiterTestRedis(t)
	l := New(rdb)
	if err := l.Close(); err != nil {
		t.Fatalf("Close() = %v，期望 nil", err)
	}
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("注入的客户端被关闭了: %v", err)
	}
	if decision := mustAllow(t, l, Bucket{Limit: 1, Window: testWindow}, UserKey(35)); !decision.Allowed {
		t.Fatal("Close 后注入客户端仍应可用")
	}
}

// 复核 M-3：NewWithAddr 自建并接管客户端；Close 后该实例不可再用。
func TestNewWithAddrOwnsClient(t *testing.T) {
	mr := miniredis.RunT(t)
	l, err := NewWithAddr(mr.Addr())
	if err != nil {
		t.Fatalf("NewWithAddr() error: %v", err)
	}
	if decision := mustAllow(t, l, Bucket{Limit: 1, Window: testWindow}, UserKey(37)); !decision.Allowed {
		t.Fatal("首次调用应放行")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close() = %v，期望 nil", err)
	}
	if _, err := l.Allow(context.Background(), Bucket{Limit: 1, Window: testWindow}, UserKey(37), 1); err == nil {
		t.Fatal("Close 后自建客户端应已关闭，Allow 必须返回 error")
	}
	if _, err := NewWithAddr("  "); err == nil {
		t.Fatal("空地址必须返回 error")
	}
}

// 复核 M-5：Window<=0 回落 1 分钟（而不是 PEXPIRE 0 让桶永不计数）。
func TestAllowNonPositiveWindowDefaultsToOneMinute(t *testing.T) {
	for _, window := range []time.Duration{0, -time.Second} {
		mr, rdb := newLimiterTestRedis(t)
		l := New(rdb)
		bucket := Bucket{Limit: 1, Window: window}
		key := UserKey(39)
		if decision := mustAllow(t, l, bucket, key); !decision.Allowed {
			t.Fatalf("window=%v 首次应放行", window)
		}
		if decision := mustAllow(t, l, bucket, key); decision.Allowed {
			t.Fatalf("window=%v 第二次应被拒绝（窗口必须真的生效）", window)
		}
		if ttl := mr.TTL(key); ttl != time.Minute {
			t.Fatalf("window=%v 的 TTL = %v，期望回落到 1m", window, ttl)
		}
	}
}
