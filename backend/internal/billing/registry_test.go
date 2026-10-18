package billing

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/contract"
)

// newTestRegistry 构造连接到 miniredis 的注册表，并返回可控的假时钟。
//
// now 由测试推进：ListAbandoned 的所有判定都基于调用方传入的时间，因此
// 不需要真的等待 600 秒；Redis 侧的 TTL 另由 miniredis.FastForward 模拟。
func newTestRegistry(t *testing.T) (*RequestRegistry, *miniredis.Miniredis, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	reg := NewRegistry(rdb)
	reg.now = func() time.Time { return now }
	return reg, mr, &now
}

// TestRegistryRegister 覆盖 Register 的持久化形状：userID/开始/心跳字段、
// 租约 TTL 与 ZSET 开始时间索引。
func TestRegistryRegister(t *testing.T) {
	reg, mr, now := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "req-1", 42, time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}
	key := leaseKey("req-1")
	if got := mr.TTL(key); got != time.Duration(contract.InflightLeaseTTLSeconds)*time.Second {
		t.Fatalf("TTL = %v, want %ds", got, contract.InflightLeaseTTLSeconds)
	}
	if got := mr.HGet(key, registryFieldUserID); got != "42" {
		t.Fatalf("user_id = %q, want 42", got)
	}
	wantMs := strconv.FormatInt(now.UnixMilli(), 10)
	if got := mr.HGet(key, registryFieldStartedAt); got != wantMs {
		t.Fatalf("started_at_ms = %q, want %s", got, wantMs)
	}
	if got := mr.HGet(key, registryFieldHeartbeatAt); got != wantMs {
		t.Fatalf("heartbeat_at_ms = %q, want %s", got, wantMs)
	}
	score, err := mr.ZScore(registryIndexKey, "req-1")
	if err != nil {
		t.Fatalf("index score: %v", err)
	}
	if int64(score) != now.UnixMilli() {
		t.Fatalf("index score = %v, want %d", score, now.UnixMilli())
	}
}

// TestRegistryHeartbeatRefreshesLease 验证心跳同时更新心跳字段并续期 TTL。
func TestRegistryHeartbeatRefreshesLease(t *testing.T) {
	reg, mr, now := newTestRegistry(t)
	ctx := context.Background()
	if err := reg.Register(ctx, "req-hb", 7, time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}

	*now = now.Add(100 * time.Second)
	if err := reg.Heartbeat(ctx, "req-hb"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	key := leaseKey("req-hb")
	if got := mr.HGet(key, registryFieldHeartbeatAt); got != strconv.FormatInt(now.UnixMilli(), 10) {
		t.Fatalf("heartbeat_at_ms = %q, want %d", got, now.UnixMilli())
	}
	if got := mr.TTL(key); got != time.Duration(contract.InflightLeaseTTLSeconds)*time.Second {
		t.Fatalf("TTL after heartbeat = %v, want %ds", got, contract.InflightLeaseTTLSeconds)
	}
	// 开始时间不因心跳改变：判定窗口以「会话开始」为基准。
	if got := mr.HGet(key, registryFieldStartedAt); got != strconv.FormatInt(now.Add(-100*time.Second).UnixMilli(), 10) {
		t.Fatalf("started_at_ms = %q, want original start", got)
	}
}

// TestRegistryListAbandonedBoundaries 覆盖两组参数的边界（R-17）：
// 窗口（600s）与心跳 TTL（300s）各自独立，窗口内的死亡会话不得被回收。
func TestRegistryListAbandonedBoundaries(t *testing.T) {
	ctx := context.Background()
	lease := time.Duration(contract.InflightLeaseTTLSeconds) * time.Second

	t.Run("窗口未到不回收", func(t *testing.T) {
		reg, _, now := newTestRegistry(t)
		start := *now
		if err := reg.Register(ctx, "req-young", 1, lease); err != nil {
			t.Fatalf("Register: %v", err)
		}
		// 心跳已超 TTL，但会话开始才 300s：窗口保护优先。
		ids, err := reg.ListAbandoned(ctx, start.Add(300*time.Second))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("ids = %v, want empty（窗口 600s 未到）", ids)
		}
	})

	t.Run("窗口边界严格超过", func(t *testing.T) {
		reg, _, now := newTestRegistry(t)
		start := *now
		if err := reg.Register(ctx, "req-boundary", 1, lease); err != nil {
			t.Fatalf("Register: %v", err)
		}
		ids, err := reg.ListAbandoned(ctx, start.Add(time.Duration(contract.AbandonedWindowSeconds)*time.Second))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("ids = %v, want empty（恰好 600s 不算超过）", ids)
		}
		ids, err = reg.ListAbandoned(ctx, start.Add(time.Duration(contract.AbandonedWindowSeconds)*time.Second+time.Millisecond))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 1 || ids[0] != "req-boundary" {
			t.Fatalf("ids = %v, want [req-boundary]", ids)
		}
	})

	t.Run("心跳新鲜不回收", func(t *testing.T) {
		reg, _, now := newTestRegistry(t)
		start := *now
		if err := reg.Register(ctx, "req-live", 1, lease); err != nil {
			t.Fatalf("Register: %v", err)
		}
		// 开始 550s 时仍有心跳（长请求），扫描发生在 601s。
		*now = start.Add(550 * time.Second)
		if err := reg.Heartbeat(ctx, "req-live"); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		ids, err := reg.ListAbandoned(ctx, start.Add(601*time.Second))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("ids = %v, want empty（心跳仅 51s 前）", ids)
		}
	})

	t.Run("心跳边界严格超过", func(t *testing.T) {
		reg, _, now := newTestRegistry(t)
		start := *now
		if err := reg.Register(ctx, "req-hb-boundary", 1, lease); err != nil {
			t.Fatalf("Register: %v", err)
		}
		// 在 301s 时心跳一次；扫描时间 601s 时心跳恰好 300s 前 → 不算超过。
		*now = start.Add(301 * time.Second)
		if err := reg.Heartbeat(ctx, "req-hb-boundary"); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		ids, err := reg.ListAbandoned(ctx, start.Add(601*time.Second))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("ids = %v, want empty（心跳恰好 300s）", ids)
		}
		ids, err = reg.ListAbandoned(ctx, start.Add(602*time.Second))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 1 || ids[0] != "req-hb-boundary" {
			t.Fatalf("ids = %v, want [req-hb-boundary]", ids)
		}
	})

	t.Run("窗口短于TTL的边界场景", func(t *testing.T) {
		reg, _, now := newTestRegistry(t)
		start := *now
		if err := reg.Register(ctx, "req-window-first", 1, lease); err != nil {
			t.Fatalf("Register: %v", err)
		}
		// 开始 400s、心跳 350s 前：两个条件单独看都可能触发，但窗口未到，
		// 不得回收（两组参数不得混用）。
		ids, err := reg.ListAbandoned(ctx, start.Add(400*time.Second))
		if err != nil {
			t.Fatalf("ListAbandoned: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("ids = %v, want empty", ids)
		}
	})
}

// TestRegistryMissingHashIsAbandoned 验证 Hash 因 TTL 过期消失后，ZSET 索引
// 仍能让扫描发现该会话（这正是索引存在的理由）。
func TestRegistryMissingHashIsAbandoned(t *testing.T) {
	reg, mr, now := newTestRegistry(t)
	ctx := context.Background()
	start := *now
	if err := reg.Register(ctx, "req-evicted", 9, time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 模拟租赁 TTL 到期：Hash 被 Redis 删除，索引保留。
	mr.FastForward(time.Duration(contract.InflightLeaseTTLSeconds)*time.Second + time.Second)
	if mr.Exists(leaseKey("req-evicted")) {
		t.Fatal("lease hash should have expired")
	}
	ids, err := reg.ListAbandoned(ctx, start.Add(601*time.Second))
	if err != nil {
		t.Fatalf("ListAbandoned: %v", err)
	}
	if len(ids) != 1 || ids[0] != "req-evicted" {
		t.Fatalf("ids = %v, want [req-evicted]", ids)
	}
}

// TestRegistryRemove 验证 Remove 同时清理 Hash 与索引项（幂等）。
func TestRegistryRemove(t *testing.T) {
	reg, mr, now := newTestRegistry(t)
	ctx := context.Background()
	if err := reg.Register(ctx, "req-rm", 1, time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Remove(ctx, "req-rm"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if mr.Exists(leaseKey("req-rm")) {
		t.Fatal("lease hash still exists after Remove")
	}
	// 索引 ZSET 在最后一个成员被移除后会被 Redis 删除（miniredis 返回
	// "no such key"），两种状态都等价于「索引为空」。
	if members, _ := mr.ZMembers(registryIndexKey); len(members) != 0 {
		t.Fatalf("index members = %v, want empty", members)
	}
	// 重复删除不是错误（幂等终结路径会重复调用）。
	if err := reg.Remove(ctx, "req-rm"); err != nil {
		t.Fatalf("second Remove: %v", err)
	}

	ids, err := reg.ListAbandoned(ctx, now.Add(601*time.Second))
	if err != nil {
		t.Fatalf("ListAbandoned: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want empty after Remove", ids)
	}
}

// TestRegistryIndexKeyDoesNotCollideWithRequestID 钉死索引键与租约键的值域
// 不相交：requestID 字面量为 "index" 时，旧布局 `inflight:index` 会与索引
// ZSET 撞键（WRONGTYPE）。索引键改为 `inflight-index` 后两者独立。
func TestRegistryIndexKeyDoesNotCollideWithRequestID(t *testing.T) {
	if registryIndexKey == leaseKey("index") {
		t.Fatalf("index key %q collides with lease key for requestID \"index\"", registryIndexKey)
	}
	reg, mr, now := newTestRegistry(t)
	ctx := context.Background()
	if err := reg.Register(ctx, "index", 5, time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !mr.Exists(leaseKey("index")) {
		t.Fatal("lease hash for requestID \"index\" missing")
	}
	members, err := mr.ZMembers(registryIndexKey)
	if err != nil || len(members) != 1 || members[0] != "index" {
		t.Fatalf("index members = %v, err = %v, want [index]", members, err)
	}
	// 心跳/删除同样不得触发 WRONGTYPE。
	if err := reg.Heartbeat(ctx, "index"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	ids, err := reg.ListAbandoned(ctx, now.Add(601*time.Second))
	if err != nil {
		t.Fatalf("ListAbandoned: %v", err)
	}
	if len(ids) != 1 || ids[0] != "index" {
		t.Fatalf("ids = %v, want [index]", ids)
	}
	if err := reg.Remove(ctx, "index"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

// TestRegistryRedisFailure 验证 Redis 故障时所有方法返回 error：
// worker 必须把 ListAbandoned 失败当成「本轮失败」而不是「无悬挂」。
func TestRegistryRedisFailure(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:        mr.Addr(),
		MaxRetries:  -1,
		DialTimeout: 100 * time.Millisecond,
		ReadTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	reg := NewRegistry(rdb)

	if err := reg.Register(ctx, "req-down", 1, time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}
	mr.Close()

	if err := reg.Register(ctx, "req-down-2", 1, time.Minute); err == nil {
		t.Fatal("Register with redis down = nil, want error")
	}
	if err := reg.Heartbeat(ctx, "req-down"); err == nil {
		t.Fatal("Heartbeat with redis down = nil, want error")
	}
	if err := reg.Remove(ctx, "req-down"); err == nil {
		t.Fatal("Remove with redis down = nil, want error")
	}
	ids, err := reg.ListAbandoned(ctx, time.Now().Add(601*time.Second))
	if err == nil {
		t.Fatal("ListAbandoned with redis down = nil error, want error")
	}
	if ids != nil {
		t.Fatalf("ids = %v, want nil on error", ids)
	}
}

// TestRegistryNilClient 验证未接线的 Registry 不会静默当作「无租约」。
func TestRegistryNilClient(t *testing.T) {
	reg := NewRegistry(nil)
	ctx := context.Background()
	if err := reg.Register(ctx, "req", 1, time.Minute); err == nil {
		t.Fatal("Register with nil client = nil, want error")
	}
	if _, err := reg.ListAbandoned(ctx, time.Now()); err == nil {
		t.Fatal("ListAbandoned with nil client = nil, want error")
	}
}

// TestRegistryEmptyRequestID 验证空 requestID 被拒绝（幂等键是唯一锚点）。
func TestRegistryEmptyRequestID(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := context.Background()
	if err := reg.Register(ctx, "", 1, time.Minute); !errors.Is(err, ErrMissingRequestID) {
		t.Fatalf("Register empty = %v, want ErrMissingRequestID", err)
	}
	if err := reg.Heartbeat(ctx, ""); !errors.Is(err, ErrMissingRequestID) {
		t.Fatalf("Heartbeat empty = %v, want ErrMissingRequestID", err)
	}
}
