package httpapi

// WorkerPresenceProbe 单测（miniredis）：网关侧判断「是否有 worker 实例在线」。
//
// 信号是实例心跳而不是任务租约：租约只在执行期存在，空闲的在线 worker 与
// 停机 worker 在租约上无法区分。

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newProbeTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	// MaxRetries=-1：Redis 关闭后立即返回错误，不重试到超时。
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// writeInstance 直接写一条心跳（绕过心跳循环，保证用例不依赖 goroutine 时序）。
func writeInstance(t *testing.T, mr *miniredis.Miniredis, id, role string) {
	t.Helper()
	key := InstanceHeartbeatKey(id)
	mr.HSet(key, "status", instanceStatusLive, "role", role,
		"started_at", "2026-10-28T00:00:00Z", "last_seen", "2026-10-28T00:00:00Z")
}

func TestWorkerPresenceProbeOnlyGateway(t *testing.T) {
	mr, rdb := newProbeTestRedis(t)
	writeInstance(t, mr, "gw-1", instanceRoleGateway)

	available, reason := WorkerPresenceProbe(rdb)(context.Background())
	if available || reason != "worker_unavailable" {
		t.Fatalf("只有 gateway 心跳时返回 (%v, %q)，期望 (false, worker_unavailable)", available, reason)
	}
}

func TestWorkerPresenceProbeWithWorker(t *testing.T) {
	mr, rdb := newProbeTestRedis(t)
	writeInstance(t, mr, "gw-1", instanceRoleGateway)
	writeInstance(t, mr, "worker-1", instanceRoleWorker)

	available, reason := WorkerPresenceProbe(rdb)(context.Background())
	if !available || reason != "" {
		t.Fatalf("含 worker 心跳时返回 (%v, %q)，期望 (true, \"\")", available, reason)
	}
}

func TestWorkerPresenceProbeFailsOpen(t *testing.T) {
	// Redis 故障不能把管理面全部手动任务禁用：故障期仍放行，滞留的
	// pending 行由 24h 兜底收敛。
	mr, rdb := newProbeTestRedis(t)
	writeInstance(t, mr, "gw-1", instanceRoleGateway)
	mr.Close()

	available, reason := WorkerPresenceProbe(rdb)(context.Background())
	if !available || reason != "" {
		t.Fatalf("Redis 不可用时返回 (%v, %q)，期望 fail-open (true, \"\")", available, reason)
	}
}

func TestWorkerPresenceProbeNilRedis(t *testing.T) {
	available, reason := WorkerPresenceProbe(nil)(context.Background())
	if !available || reason != "" {
		t.Fatalf("rdb=nil 时返回 (%v, %q)，期望 fail-open (true, \"\")", available, reason)
	}
}
