//go:build integration

package platform

// W17-2 worker 调度器集成验收（billing-06 §2.3.1 / §2.5 / §13.5）。
//
// 运行（隔离库需先 migrate up）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w17b?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration -p 1 ./internal/platform/ -run TestWorkerTask -v
//
// 覆盖：
//   - 两副本并发只一个执行（Redis SET NX PX + DB generation CAS）；
//   - 运行中触发 → 409（服务层，不依赖 HTTP）；
//   - 崩溃后租约过期可接管，且过期 running 行被标记 failed；
//   - 成功/失败/超时三态，手动 vs 定时触发；
//   - Enabled() 谓词为假时空闲不建行。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/worker"
)

func workerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func workerTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func uniqueWorkerTaskName(prefix string) string {
	return fmt.Sprintf("test_%s_%d", prefix, time.Now().UnixNano())
}

func cleanupWorkerTask(t *testing.T, db *sql.DB, rdb *redis.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		_, _ = db.Exec(`DELETE FROM worker_task_runs WHERE task_type = $1`, name)
		_, _ = db.Exec(`DELETE FROM worker_task_locks WHERE task_type = $1`, name)
		_, _ = db.Exec(`DELETE FROM worker_tasks WHERE task_type = $1`, name)
		_ = rdb.Del(context.Background(), worker.LeaseKey(name)).Err()
	}
}

func newWorkerScheduler(t *testing.T, registry *worker.Registry, store *worker.SQLStore,
	rdb *redis.Client, node string) *worker.Scheduler {
	t.Helper()
	scheduler, err := worker.NewScheduler(worker.SchedulerConfig{
		Registry:          registry,
		Store:             store,
		Lease:             worker.NewRedisLease(rdb),
		NodeID:            node,
		PassInterval:      time.Hour, // 测试手动调用 Pass，不跑 ticker
		LeaseTTL:          2 * time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		CleanupInterval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	return scheduler
}

func TestWorkerTaskLeaseSingleExecutor(t *testing.T) {
	db := workerTestDB(t)
	rdb := workerTestRedis(t)
	ctx := context.Background()
	name := uniqueWorkerTaskName("lease")
	t.Cleanup(func() { cleanupWorkerTask(t, db, rdb, name) })

	var executions int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	task := worker.Task{
		Name:        name,
		Interval:    time.Hour,
		Scheduled:   true,
		Timeout:     5 * time.Second,
		Concurrency: worker.ConcurrencySingle,
		Run: func(ctx context.Context) (worker.Result, error) {
			atomic.AddInt32(&executions, 1)
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return worker.Result{"ok": true}, nil
		},
	}
	registry, err := worker.NewRegistry(task)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	store, err := worker.NewSQLStore(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}
	s1 := newWorkerScheduler(t, registry, store, rdb, "node-a")
	s2 := newWorkerScheduler(t, registry, store, rdb, "node-b")

	// 两个副本必须**并发**扫描：串行调用时第一个副本已经把 last_run_at
	// 推进，第二个副本在到期判定就跳过，测不到租约这一层。
	var passWG sync.WaitGroup
	passWG.Add(2)
	go func() { defer passWG.Done(); s1.Pass(ctx) }()
	go func() { defer passWG.Done(); s2.Pass(ctx) }()
	passWG.Wait()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("首个调度器未启动执行")
	}
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&executions); got != 1 {
		t.Fatalf("执行次数 = %d，期望 1（两副本只能有一个执行者）", got)
	}
	close(release)
	s1.Wait()
	s2.Wait()

	runs, total, err := store.ListRuns(ctx, name, 10, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if total != 1 || len(runs) != 1 {
		t.Fatalf("运行行数 = %d/%d，期望 1", total, len(runs))
	}
	if runs[0].Status != worker.RunSucceeded || runs[0].Trigger != worker.TriggerSchedule {
		t.Fatalf("运行 = %+v，期望 succeeded/schedule", runs[0])
	}
}

func TestWorkerTaskTriggerWhileRunningConflicts(t *testing.T) {
	db := workerTestDB(t)
	rdb := workerTestRedis(t)
	ctx := context.Background()
	name := uniqueWorkerTaskName("running")
	t.Cleanup(func() { cleanupWorkerTask(t, db, rdb, name) })

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	task := worker.Task{
		Name:                 name,
		Interval:             time.Hour,
		Scheduled:            true,
		Timeout:              5 * time.Second,
		ManualTriggerAllowed: true,
		Run: func(ctx context.Context) (worker.Result, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil, nil
		},
	}
	registry, _ := worker.NewRegistry(task)
	store, _ := worker.NewSQLStore(db)
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}
	scheduler := newWorkerScheduler(t, registry, store, rdb, "node-a")
	service, err := worker.NewService(worker.ServiceConfig{
		Registry: registry,
		Store:    store,
		Lease:    worker.NewRedisLease(rdb),
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	scheduler.Pass(ctx)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("任务未启动")
	}
	if _, err := service.Trigger(ctx, name); !errors.Is(err, worker.ErrTaskAlreadyRunning) {
		t.Fatalf("运行中触发 err = %v，期望 ErrTaskAlreadyRunning", err)
	}
	close(release)
	scheduler.Wait()
}

func TestWorkerTaskExpiredLeaseIsTakenOver(t *testing.T) {
	db := workerTestDB(t)
	rdb := workerTestRedis(t)
	ctx := context.Background()
	name := uniqueWorkerTaskName("expired")
	t.Cleanup(func() { cleanupWorkerTask(t, db, rdb, name) })

	task := worker.Task{
		Name:      name,
		Interval:  time.Hour,
		Scheduled: true,
		Timeout:   2 * time.Second,
		Run: func(context.Context) (worker.Result, error) {
			return worker.Result{"taken_over": true}, nil
		},
	}
	registry, _ := worker.NewRegistry(task)
	store, _ := worker.NewSQLStore(db)
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}
	// 模拟崩溃节点：租约已过期，但仍有一条 running 行。
	now := time.Now().UTC()
	started := now.Add(-2 * time.Minute)
	deadRun, err := store.CreateRun(ctx, worker.CreateRunInput{
		TaskType: name, Status: worker.RunRunning, Trigger: worker.TriggerSchedule,
		NodeID: "dead-node", StartedAt: &started,
	})
	if err != nil {
		t.Fatalf("create dead run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worker_task_locks (task_type, holder_id, lease_expires_at, heartbeat_at, generation)
		VALUES ($1, 'dead-node', $2, $2, 7)`, name, now.Add(-time.Minute)); err != nil {
		t.Fatalf("insert expired lock: %v", err)
	}

	scheduler := newWorkerScheduler(t, registry, store, rdb, "node-live")
	scheduler.CleanupExpired(ctx)

	runs, _, err := store.ListRuns(ctx, name, 10, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != deadRun {
		t.Fatalf("清理想运行行异常: %+v", runs)
	}
	if runs[0].Status != worker.RunFailed || runs[0].Error != worker.ErrClassLeaseExpired {
		t.Fatalf("过期运行 = status %s error %q，期望 failed/%s",
			runs[0].Status, runs[0].Error, worker.ErrClassLeaseExpired)
	}
	if runs[0].FinishedAt == nil {
		t.Fatal("过期运行必须写 finished_at")
	}

	// 过期租约可被新节点接管：下一次 pass 应创建并执行新运行。
	scheduler.Pass(ctx)
	scheduler.Wait()
	runs, total, err := store.ListRuns(ctx, name, 10, 0)
	if err != nil {
		t.Fatalf("list runs after takeover: %v", err)
	}
	if total != 2 {
		t.Fatalf("接管后运行数 = %d，期望 2", total)
	}
}

func TestWorkerTaskRunStatesAndManualTrigger(t *testing.T) {
	db := workerTestDB(t)
	rdb := workerTestRedis(t)
	ctx := context.Background()
	successName := uniqueWorkerTaskName("success")
	failureName := uniqueWorkerTaskName("failure")
	timeoutName := uniqueWorkerTaskName("timeout")
	t.Cleanup(func() { cleanupWorkerTask(t, db, rdb, successName, failureName, timeoutName) })

	success := worker.Task{Name: successName, Interval: time.Hour, Scheduled: true, Timeout: 2 * time.Second,
		ManualTriggerAllowed: true,
		Run:                  func(context.Context) (worker.Result, error) { return worker.Result{"done": 1}, nil }}
	failure := worker.Task{Name: failureName, Interval: time.Hour, Scheduled: true, Timeout: 2 * time.Second,
		Run: func(context.Context) (worker.Result, error) {
			return nil, errors.New("upstream secret must not leak")
		}}
	timeout := worker.Task{Name: timeoutName, Interval: time.Hour, Scheduled: true, Timeout: 80 * time.Millisecond,
		Run: func(ctx context.Context) (worker.Result, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	registry, err := worker.NewRegistry(success, failure, timeout)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	store, _ := worker.NewSQLStore(db)
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}
	scheduler := newWorkerScheduler(t, registry, store, rdb, "node-a")
	scheduler.Pass(ctx)
	scheduler.Wait()

	want := map[string]struct{ status, errClass string }{
		successName: {worker.RunSucceeded, ""},
		failureName: {worker.RunFailed, worker.ErrClassExecution},
		timeoutName: {worker.RunFailed, worker.ErrClassTimeout},
	}
	for name, expected := range want {
		runs, total, err := store.ListRuns(ctx, name, 5, 0)
		if err != nil {
			t.Fatalf("list runs %s: %v", name, err)
		}
		if total != 1 {
			t.Fatalf("%s 运行数 = %d，期望 1", name, total)
		}
		run := runs[0]
		if run.Status != expected.status || run.Error != expected.errClass {
			t.Fatalf("%s = %s/%q，期望 %s/%q", name, run.Status, run.Error, expected.status, expected.errClass)
		}
		if run.FinishedAt == nil {
			t.Fatalf("%s 必须写 finished_at", name)
		}
		if name == failureName && run.Error == "upstream secret must not leak" {
			t.Fatal("原始错误文本不得写入运行历史")
		}
	}

	// 手动触发：写 pending → 调度 pass 认领 → trigger=manual。
	service, err := worker.NewService(worker.ServiceConfig{
		Registry: registry, Store: store, Lease: worker.NewRedisLease(rdb),
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	triggered, err := service.Trigger(ctx, successName)
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	scheduler.Pass(ctx)
	scheduler.Wait()
	runs, total, err := store.ListRuns(ctx, successName, 5, 0)
	if err != nil {
		t.Fatalf("list runs after trigger: %v", err)
	}
	if total != 2 {
		t.Fatalf("手动触发后运行数 = %d，期望 2", total)
	}
	manual := runs[0]
	if manual.Trigger != worker.TriggerManual || manual.ID != triggered.RunID {
		t.Fatalf("手动运行 = trigger %s id %d，期望 manual/%d", manual.Trigger, manual.ID, triggered.RunID)
	}
}

func TestWorkerTaskIdlePredicateCreatesNoRow(t *testing.T) {
	db := workerTestDB(t)
	rdb := workerTestRedis(t)
	ctx := context.Background()
	name := uniqueWorkerTaskName("idle")
	t.Cleanup(func() { cleanupWorkerTask(t, db, rdb, name) })

	var calls int32
	task := worker.Task{
		Name:      name,
		Interval:  time.Millisecond,
		Scheduled: true,
		Timeout:   time.Second,
		Enabled:   func() bool { return false },
		Run: func(context.Context) (worker.Result, error) {
			atomic.AddInt32(&calls, 1)
			return nil, nil
		},
	}
	registry, _ := worker.NewRegistry(task)
	store, _ := worker.NewSQLStore(db)
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}
	scheduler := newWorkerScheduler(t, registry, store, rdb, "node-a")
	scheduler.Pass(ctx)
	time.Sleep(100 * time.Millisecond)
	scheduler.Wait()

	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("谓词为假仍执行了 %d 次", got)
	}
	_, total, err := store.ListRuns(ctx, name, 5, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if total != 0 {
		t.Fatalf("空闲任务产生了 %d 条运行记录", total)
	}
	state, ok, err := store.GetTaskState(ctx, name)
	if err != nil || !ok {
		t.Fatalf("get state: %v %v", ok, err)
	}
	if state.LastRunAt != nil {
		t.Fatalf("谓词为假时不得推进 last_run_at: %v", state.LastRunAt)
	}
}

// TestWorkerTaskLeaseUsesDatabaseClock 锁定「租约时间由仓储层 now() 生成」：
// 租约窗口与到期比较必须来自同一台 PG，调用方进程时钟不参与。否则跨副本
// 时钟快几秒的节点会在租约尚未到期时提前抢占（同任务双执行）。
func TestWorkerTaskLeaseUsesDatabaseClock(t *testing.T) {
	db := workerTestDB(t)
	ctx := context.Background()
	name := uniqueWorkerTaskName("dbclock")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM worker_task_runs WHERE task_type = $1`, name)
		_, _ = db.Exec(`DELETE FROM worker_task_locks WHERE task_type = $1`, name)
		_, _ = db.Exec(`DELETE FROM worker_tasks WHERE task_type = $1`, name)
	})
	store, err := worker.NewSQLStore(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	registry, _ := worker.NewRegistry(worker.Task{Name: name, Interval: time.Hour})
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}

	const ttl = 60 * time.Second
	generation, ok, err := store.ClaimLock(ctx, name, "node-a", ttl)
	if err != nil || !ok {
		t.Fatalf("claim = %v/%v/%v", generation, ok, err)
	}

	var lease, heartbeat, dbNow time.Time
	if err := db.QueryRowContext(ctx, `
		SELECT lease_expires_at, heartbeat_at, now()
		  FROM worker_task_locks WHERE task_type = $1`, name).Scan(&lease, &heartbeat, &dbNow); err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if lease.Sub(heartbeat) < ttl-time.Second || lease.Sub(heartbeat) > ttl+time.Second {
		t.Fatalf("租约窗口 = %v，期望约 %v（必须由 DB now() + ttl 生成）", lease.Sub(heartbeat), ttl)
	}
	if heartbeat.Sub(dbNow).Abs() > 5*time.Second {
		t.Fatalf("heartbeat 与 DB now() 相差 %v，说明写入了进程时钟", heartbeat.Sub(dbNow))
	}
	if !lease.After(dbNow) {
		t.Fatalf("租约未到期却被判定为过期: lease=%v dbNow=%v", lease, dbNow)
	}

	// 租约有效期内另一节点不得抢占（CAS 用的是 DB 时间）。
	if _, ok, err := store.ClaimLock(ctx, name, "node-b", ttl); err != nil || ok {
		t.Fatalf("有效租约被抢占: ok=%v err=%v", ok, err)
	}
	active, err := store.LeaseActive(ctx, name)
	if err != nil || !active {
		t.Fatalf("LeaseActive = %v/%v，期望 true", active, err)
	}
}
