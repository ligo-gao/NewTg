package plugin

// 注册表热刷新与逐副本运行时状态的单元测试（miniredis，无外部依赖）。
//
// 覆盖：30s ticker（注入短周期）、pub/sub 跨副本 Reload、内容不变不递增
// generation、状态键/TTL/停机删除、聚合排序与 TTL 过期剔除、Redis 故障
// 降级、诊断文本脱敏。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// runtimeTestLogger 丢弃日志：故障降级用例会连续触发 Warn，不应污染测试输出。
func runtimeTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runtimeLoader 是可变的内存版本加载器（与 registry_test 的 fakeLoader
// 不同：本文件的用例需要在运行中改变 active 版本，并统计 LoadActive 次数）。
type runtimeLoader struct {
	mu       sync.Mutex
	versions []ActiveVersion
	err      error
	loads    int
}

func newRuntimeLoader(versions ...ActiveVersion) *runtimeLoader {
	return &runtimeLoader{versions: versions}
}

func (l *runtimeLoader) LoadActive(context.Context) ([]ActiveVersion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loads++
	if l.err != nil {
		return nil, l.err
	}
	return append([]ActiveVersion(nil), l.versions...), nil
}

func (l *runtimeLoader) LoadVersion(_ context.Context, key, version string) (*ActiveVersion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, candidate := range l.versions {
		if candidate.Key == key && candidate.Version == version {
			copied := candidate
			return &copied, nil
		}
	}
	return nil, nil
}

func (l *runtimeLoader) set(versions ...ActiveVersion) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.versions = append([]ActiveVersion(nil), versions...)
}

func (l *runtimeLoader) loadCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loads
}

func newRuntimeTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func newRuntimeTestManager(t *testing.T, loader VersionLoader) *Manager {
	t.Helper()
	manager := MustNewManager(map[string]string{"sample": fixture{}.Source()}, loader,
		Options{MasterEnabled: true})
	return manager
}

func startRuntimeCoordinator(t *testing.T, cfg RuntimeCoordinatorConfig) *RuntimeCoordinator {
	t.Helper()
	coordinator, err := NewRuntimeCoordinator(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeCoordinator: %v", err)
	}
	coordinator.Start(context.Background())
	t.Cleanup(coordinator.Stop)
	return coordinator
}

func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout after %s waiting for %s", timeout, what)
}

func TestRuntimeCoordinatorPubSubTriggersPeerReload(t *testing.T) {
	_, rdb := newRuntimeTestRedis(t)
	loaderA := newRuntimeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	loaderB := newRuntimeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	managerA := newRuntimeTestManager(t, loaderA)
	managerB := newRuntimeTestManager(t, loaderB)

	// interval 取长周期：本用例只允许 pub/sub 触发对端 Reload（ticker 不能
	// 掩盖广播失效）。
	coordinatorA := startRuntimeCoordinator(t, RuntimeCoordinatorConfig{
		Manager: managerA, Redis: rdb, InstanceID: "replica-a", Role: "gateway",
		Interval: time.Hour, Logger: runtimeTestLogger(),
	})
	coordinatorB := startRuntimeCoordinator(t, RuntimeCoordinatorConfig{
		Manager: managerB, Redis: rdb, InstanceID: "replica-b", Role: "gateway",
		Interval: time.Hour, Logger: runtimeTestLogger(),
	})
	// 确认 B 已订阅成功后再发布，避免「Start 已返回但 SUBSCRIBE 尚未确认」
	// 的竞态把用例变成对调度时序的断言。
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if err := coordinatorB.waitSubscribed(waitCtx); err != nil {
		t.Fatalf("replica B subscribe: %v", err)
	}

	eventually(t, 2*time.Second, "replica B initial snapshot", func() bool {
		return managerB.Snapshot().Lookup("sample").Version == "1.0.0"
	})
	baseline := managerB.Snapshot().Generation()

	// 修改 B 的权威源（模拟另一副本已提交的新版本），再由 A 广播失效。
	loaderB.set(activeVersion("sample", "2.0.0", fixture{Version: "2.0.0"}.Source()))
	if err := coordinatorA.PublishPluginChanged(context.Background()); err != nil {
		t.Fatalf("PublishPluginChanged: %v", err)
	}

	eventually(t, 2*time.Second, "replica B reload after pub/sub", func() bool {
		return managerB.Snapshot().Lookup("sample").Version == "2.0.0"
	})
	if managerB.Snapshot().Generation() <= baseline {
		t.Fatalf("对端 Reload 后 generation 应递增：baseline=%d now=%d",
			baseline, managerB.Snapshot().Generation())
	}
}

func TestRuntimeCoordinatorTickerReloadsAndSkipsUnchangedGeneration(t *testing.T) {
	_, rdb := newRuntimeTestRedis(t)
	loader := newRuntimeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	manager := newRuntimeTestManager(t, loader)
	startRuntimeCoordinator(t, RuntimeCoordinatorConfig{
		Manager: manager, Redis: rdb, InstanceID: "ticker-node", Role: "worker",
		Interval: 20 * time.Millisecond, Logger: runtimeTestLogger(),
	})

	eventually(t, 2*time.Second, "initial generation", func() bool {
		return manager.Snapshot().Generation() == 1
	})
	// 内容签名不变：多个 tick 后 generation 必须保持 1（relay-06 §2.3 的
	// 「一次请求全程锁定一个 generation」依赖这一点）。
	eventually(t, 2*time.Second, "multiple ticker reloads", func() bool {
		return loader.loadCount() >= 4
	})
	if got := manager.Snapshot().Generation(); got != 1 {
		t.Fatalf("内容不变时 generation 不得递增：got %d", got)
	}

	loader.set(activeVersion("sample", "2.0.0", fixture{Version: "2.0.0"}.Source()))
	eventually(t, 2*time.Second, "ticker picks up new content", func() bool {
		return manager.Snapshot().Lookup("sample").Version == "2.0.0"
	})
	if got := manager.Snapshot().Generation(); got != 2 {
		t.Fatalf("内容变化后 generation 应为 2：got %d", got)
	}
}

func TestRuntimeStatusKeyShapeTTLAndStopDelete(t *testing.T) {
	mr, rdb := newRuntimeTestRedis(t)
	loader := newRuntimeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	manager := newRuntimeTestManager(t, loader)
	started := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	seen := started.Add(5 * time.Second)
	coordinator := startRuntimeCoordinator(t, RuntimeCoordinatorConfig{
		Manager: manager, Redis: rdb, InstanceID: "status-node", Role: "worker",
		Version: "1.9.0", StartedAt: started, Interval: time.Hour,
		Now: func() time.Time { return seen }, Logger: runtimeTestLogger(),
	})

	key := RuntimeStatusKey("status-node")
	eventually(t, 2*time.Second, "runtime status hash", func() bool {
		return mr.Exists(key)
	})
	values, err := rdb.HGetAll(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("HGetAll: %v", err)
	}
	for _, field := range []string{"status", "version", "role", "started_at", "last_seen"} {
		if values[field] == "" {
			t.Fatalf("状态 hash 缺字段 %q：%+v", field, values)
		}
	}
	if values["version"] != "1.9.0" || values["role"] != "worker" ||
		values["started_at"] != started.Format(time.RFC3339Nano) ||
		values["last_seen"] != seen.Format(time.RFC3339Nano) {
		t.Fatalf("状态 hash 元数据不符：%+v", values)
	}
	var status RuntimeStatus
	if err := json.Unmarshal([]byte(values["status"]), &status); err != nil {
		t.Fatalf("status JSON: %v", err)
	}
	if len(status.Plugins) == 0 {
		t.Fatal("status JSON 应包含逐插件状态")
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > RuntimeStatusTTL {
		t.Fatalf("状态 key TTL = %s，期望 (0, %s]", ttl, RuntimeStatusTTL)
	}

	coordinator.Stop()
	if mr.Exists(key) {
		t.Fatal("Stop 必须删除本实例状态 key")
	}
}

func TestRuntimeAggregateSortsAndDropsExpired(t *testing.T) {
	mr, rdb := newRuntimeTestRedis(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	status := RuntimeStatus{Node: "node-x", Generation: 7, Revision: "rev-1",
		ReloadedAt: now, Plugins: []PluginRuntimeStatus{{Key: "sample", Status: StatusRegistered}}}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, id := range []string{"replica-c", "replica-a", "replica-b"} {
		key := RuntimeStatusKey(id)
		if err := rdb.HSet(context.Background(), key, map[string]interface{}{
			"status": string(raw), "version": "1.0.0", "role": "gateway",
			"started_at": now.Format(time.RFC3339Nano),
			"last_seen":  now.Format(time.RFC3339Nano),
		}).Err(); err != nil {
			t.Fatalf("HSet %s: %v", id, err)
		}
		if err := rdb.Expire(context.Background(), key, RuntimeStatusTTL).Err(); err != nil {
			t.Fatalf("Expire %s: %v", id, err)
		}
	}
	local := LocalRuntimeInstance(status, "replica-m", "gateway", "1.0.0", now, now)
	aggregate := AggregateRuntimeInstances(context.Background(), rdb, local,
		func() time.Time { return now })
	if aggregate.AggregateUnavailable {
		t.Fatal("Redis 可用时 aggregate_unavailable 必须为 false")
	}
	ids := make([]string, len(aggregate.Instances))
	for i, instance := range aggregate.Instances {
		ids[i] = instance.InstanceID
	}
	want := []string{"replica-a", "replica-b", "replica-c", "replica-m"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("聚合排序 = %v，期望 %v", ids, want)
	}

	// TTL 90s：过期实例必须从聚合消失，本地实例仍在。
	mr.FastForward(RuntimeStatusTTL + time.Second)
	aggregate = AggregateRuntimeInstances(context.Background(), rdb, local,
		func() time.Time { return now })
	if len(aggregate.Instances) != 1 || aggregate.Instances[0].InstanceID != "replica-m" {
		t.Fatalf("过期实例未剔除：%+v", aggregate.Instances)
	}
}

func TestRuntimeAggregateRedisFailureDegradesToLocal(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr: mr.Addr(), MaxRetries: -1,
		DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	status := RuntimeStatus{Node: "node-local", Generation: 3, Plugins: []PluginRuntimeStatus{}}
	local := LocalRuntimeInstance(status, "local-node", "gateway", "1.0.0", now, now)
	aggregate := AggregateRuntimeInstances(context.Background(), rdb, local,
		func() time.Time { return now })
	if !aggregate.AggregateUnavailable {
		t.Fatal("Redis 不可用时 aggregate_unavailable 必须为 true")
	}
	if len(aggregate.Instances) != 1 || aggregate.Instances[0].InstanceID != "local-node" ||
		!aggregate.Instances[0].Local {
		t.Fatalf("Redis 不可用时只能返回本地实例：%+v", aggregate.Instances)
	}
}

func TestRuntimeCoordinatorNilRedisStillReloadsLocally(t *testing.T) {
	loader := newRuntimeLoader(activeVersion("sample", "1.0.0", fixture{}.Source()))
	manager := newRuntimeTestManager(t, loader)
	startRuntimeCoordinator(t, RuntimeCoordinatorConfig{
		Manager: manager, InstanceID: "local-only", Interval: 20 * time.Millisecond,
		Logger: runtimeTestLogger(),
	})
	eventually(t, 2*time.Second, "local reload without redis", func() bool {
		return loader.loadCount() >= 3
	})
}

func TestRuntimeStatusRedactsCredentialsURLsAndSource(t *testing.T) {
	status := sanitizeRuntimeStatus(RuntimeStatus{
		Node: "n1", ReloadError: "dial postgres://user:pw@db:5432/x password=hunter2",
		Plugins: []PluginRuntimeStatus{{
			Key: "sample", Status: StatusCompileFailed,
			Errors: Issues{{Key: "sample", Rule: "compile_failed",
				Message: "fetch https://vendor.example/x?api_key=abcd token=efgh"}},
		}},
	})
	if strings.Contains(status.ReloadError, "user:pw") ||
		strings.Contains(status.ReloadError, "hunter2") ||
		strings.Contains(status.ReloadError, "postgres://") {
		t.Fatalf("reload_error 未脱敏：%q", status.ReloadError)
	}
	message := status.Plugins[0].Errors[0].Message
	for _, leak := range []string{"https://", "api_key=abcd", "token=efgh"} {
		if strings.Contains(message, leak) {
			t.Fatalf("诊断文本未脱敏（含 %q）：%q", leak, message)
		}
	}

	// 源码本身不得进入运行时响应。
	marker := "TOP_SECRET_SOURCE_MARKER"
	source := fixture{Append: "const " + marker + " = 'source-body';"}.Source()
	loader := newRuntimeLoader(activeVersion("sample", "2.0.0", source))
	manager := newRuntimeTestManager(t, loader)
	if err := manager.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	raw, err := json.Marshal(manager.Status())
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(raw), marker) || strings.Contains(string(raw), "source_text") {
		t.Fatalf("运行时状态不得输出源码：%s", raw)
	}
}
