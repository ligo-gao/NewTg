//go:build integration

package httpapi

// W17-2 系统任务端点的集成验收（真实 PostgreSQL + Redis）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w17b?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w17b_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration -p 1 ./internal/httpapi/ -run TestSystemTasksIntegration -v
//
// 覆盖 TC-F-08-21-05/06/07：
//   - 列表来自静态注册表 + worker_tasks 调度行；
//   - 详情返回运行历史；
//   - 白名单触发 202 且运行行 trigger=manual；
//   - 非白名单 403、未知 404、重复触发 409、每次触发写审计。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/worker"
)

func systemTaskIntegrationDBs(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" || logsDSN == "" {
		t.Skip("NEWTG_TEST_DB_DSN / NEWTG_TEST_LOGS_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	logs, err := sql.Open("pgx", logsDSN)
	if err != nil {
		_ = db.Close()
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = logs.Close(); _ = db.Close() })
	return db, logs
}

func TestSystemTasksIntegration(t *testing.T) {
	db, logs := systemTaskIntegrationDBs(t)
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	// 专用库：只清理本框架三表，避免残余 pending 行影响断言。
	for _, table := range []string{"worker_task_runs", "worker_task_locks", "worker_tasks"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"worker_task_runs", "worker_task_locks", "worker_tasks"} {
			_, _ = db.Exec("DELETE FROM " + table)
		}
		_, _ = logs.Exec(`DELETE FROM audit_logs WHERE detail->>'event' = 'system.task.trigger'`)
	})

	registry, err := worker.DefaultRegistry()
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
	// 只绑定白名单任务的一轮执行；其余任务的 Run 为 nil（既有循环独占）。
	if err := registry.BindRun("worker_history_cleanup", func(context.Context) (worker.Result, error) {
		return worker.Result{"deleted_runs": 0}, nil
	}); err != nil {
		t.Fatalf("bind run: %v", err)
	}
	service, err := worker.NewService(worker.ServiceConfig{
		Registry: registry,
		Store:    store,
		Lease:    worker.NewRedisLease(rdb),
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	mux := http.NewServeMux()
	NewSystemTaskHandlers(SystemTaskHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(logs),
		Idem:    NewRedisIdempotencyStore(rdb),
		Logger:  testLogger(),
	}).Register(mux)

	// 列表：注册表全集。
	list := httptest.NewRecorder()
	mux.ServeHTTP(list, settingsRequest(http.MethodGet, "/api/system/tasks?page_size=100",
		"", systemTaskAdmin(), nil))
	if list.Code != http.StatusOK {
		t.Fatalf("列表 = %d，期望 200", list.Code)
	}
	var page worker.TaskPage
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if page.Total != len(registry.Tasks()) {
		t.Fatalf("列表总数 = %d，期望 %d", page.Total, len(registry.Tasks()))
	}

	// 详情：未知任务 404。
	unknown := httptest.NewRecorder()
	mux.ServeHTTP(unknown, settingsRequest(http.MethodGet, "/api/system/tasks/no-such-task",
		"", systemTaskAdmin(), nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("未知详情 = %d，期望 404", unknown.Code)
	}

	// 非白名单触发 403。
	forbidden := httptest.NewRecorder()
	mux.ServeHTTP(forbidden, settingsRequest(http.MethodPost,
		"/api/system/tasks/async_task_poll:trigger", "", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": fmt.Sprintf("int-forbidden-%d", time.Now().UnixNano())}))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("非白名单触发 = %d，期望 403", forbidden.Code)
	}

	// 白名单触发 202。
	accepted := httptest.NewRecorder()
	mux.ServeHTTP(accepted, settingsRequest(http.MethodPost,
		"/api/system/tasks/worker_history_cleanup:trigger", "", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": fmt.Sprintf("int-accepted-%d", time.Now().UnixNano())}))
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("白名单触发 = %d，期望 202（body=%s）", accepted.Code, accepted.Body.String())
	}
	var trigger worker.TriggerResult
	if err := json.Unmarshal(accepted.Body.Bytes(), &trigger); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}

	// 已入队（pending）时重复触发 → 409。
	conflict := httptest.NewRecorder()
	mux.ServeHTTP(conflict, settingsRequest(http.MethodPost,
		"/api/system/tasks/worker_history_cleanup:trigger", "", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": fmt.Sprintf("int-conflict-%d", time.Now().UnixNano())}))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("重复触发 = %d，期望 409", conflict.Code)
	}

	// 调度 pass 认领 pending 行并执行。
	scheduler, err := worker.NewScheduler(worker.SchedulerConfig{
		Registry:     registry,
		Store:        store,
		Lease:        worker.NewRedisLease(rdb),
		NodeID:       "integration-node",
		Logger:       testLogger(),
		PassInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	scheduler.Pass(ctx)
	scheduler.Wait()

	runs, total, err := store.ListRuns(ctx, "worker_history_cleanup", 10, 0)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if total != 1 || len(runs) != 1 {
		t.Fatalf("运行行数 = %d/%d，期望 1", total, len(runs))
	}
	if runs[0].ID != trigger.RunID || runs[0].Trigger != worker.TriggerManual ||
		runs[0].Status != worker.RunSucceeded {
		t.Fatalf("运行 = %+v，期望 manual/succeeded id=%d", runs[0], trigger.RunID)
	}

	// 详情返回最近运行。
	detail := httptest.NewRecorder()
	mux.ServeHTTP(detail, settingsRequest(http.MethodGet,
		"/api/system/tasks/worker_history_cleanup", "", systemTaskAdmin(), nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("详情 = %d，期望 200", detail.Code)
	}
	var payload worker.TaskDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(payload.Runs.Items) != 1 || payload.Runs.Items[0].Trigger != worker.TriggerManual {
		t.Fatalf("详情运行 = %+v", payload.Runs)
	}

	// 原始 JSON 键名断言（F-1）：这是运行历史 DTO 缺 json tag 的唯一有效防线。
	// 把响应解回 worker.TaskDetail 是不够的——Go 的 encoding/json 在解 PascalCase
	// 时同样能匹配字段名，旧的无 tag 结构体也能让上面那条断言通过，而前端读的是
	// snake_case，整行渲染为空。所以这里必须检查原始键名：若 Run 又丢掉 tag，
	// 下面的 snake_case 键会全部缺失、PascalCase 键会出现，立即失败。
	var rawDetail struct {
		Runs struct {
			Items []map[string]any `json:"items"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &rawDetail); err != nil {
		t.Fatalf("decode raw detail: %v", err)
	}
	if len(rawDetail.Runs.Items) != 1 {
		t.Fatalf("原始运行条目数 = %d，期望 1", len(rawDetail.Runs.Items))
	}
	rawRun := rawDetail.Runs.Items[0]
	for _, key := range []string{
		"id", "task_type", "status", "node_id", "started_at", "finished_at", "result", "trigger",
	} {
		if _, ok := rawRun[key]; !ok {
			t.Fatalf("运行条目缺少 snake_case 键 %q（实际键：%v）", key, rawRun)
		}
	}
	for _, key := range []string{
		"ID", "TaskType", "Status", "NodeID", "StartedAt", "FinishedAt", "Error", "Result", "Trigger",
	} {
		if _, ok := rawRun[key]; ok {
			t.Fatalf("运行条目出现 PascalCase 键 %q（实际键：%v）；Run 缺少 json tag", key, rawRun)
		}
	}
	// node_id 来自调度器，同时证明该字段不是被 omitempty 吞掉。
	if rawRun["node_id"] != "integration-node" {
		t.Fatalf("运行条目 node_id = %v，期望 integration-node", rawRun["node_id"])
	}

	// 审计：每次触发（403/202/409）各一条，且 detail 不泄露节点标识。
	var auditCount int
	if err := logs.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs
		WHERE detail->>'event' = 'system.task.trigger'`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if auditCount != 3 {
		t.Fatalf("触发审计条数 = %d，期望 3", auditCount)
	}
	var leaked int
	if err := logs.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs
		WHERE detail->>'event' = 'system.task.trigger'
		  AND detail::text LIKE '%integration-node%'`).Scan(&leaked); err != nil {
		t.Fatalf("query audit leak: %v", err)
	}
	if leaked != 0 {
		t.Fatal("审计 detail 不得包含节点标识")
	}
}

// TestSystemTasksTriggerRequiresOnlineWorker 锁定 F-3：worker 不在线时管理面
// 必须提前暴露不可用（列表 available=false + blocked_reason），触发当场 409
// worker_unavailable，且**零 pending 行**——否则管理员会以为任务已排期，而
// 那条 pending 行永远不会被执行。
func TestSystemTasksTriggerRequiresOnlineWorker(t *testing.T) {
	db, logs := systemTaskIntegrationDBs(t)
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	for _, table := range []string{"worker_task_runs", "worker_task_locks", "worker_tasks"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"worker_task_runs", "worker_task_locks", "worker_tasks"} {
			_, _ = db.Exec("DELETE FROM " + table)
		}
		_, _ = logs.Exec(`DELETE FROM audit_logs WHERE detail->>'event' = 'system.task.trigger'`)
	})

	registry, err := worker.DefaultRegistry()
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
	// 第二个 Service 模拟网关进程：不 BindRun（Run 全为 nil），且探针恒报
	// 「没有 worker 在线」。
	service, err := worker.NewService(worker.ServiceConfig{
		Registry: registry,
		Store:    store,
		Lease:    worker.NewRedisLease(rdb),
		Logger:   testLogger(),
		RuntimeAvailability: func(context.Context) (bool, string) {
			return false, worker.BlockedCodeWorkerUnavailable
		},
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	mux := http.NewServeMux()
	NewSystemTaskHandlers(SystemTaskHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Audit:   audit.New(logs),
		Idem:    NewRedisIdempotencyStore(rdb),
		Logger:  testLogger(),
	}).Register(mux)

	// 列表：白名单任务标 unavailable，非白名单任务恒为 available。
	list := httptest.NewRecorder()
	mux.ServeHTTP(list, settingsRequest(http.MethodGet, "/api/system/tasks?page_size=100",
		"", systemTaskAdmin(), nil))
	if list.Code != http.StatusOK {
		t.Fatalf("列表 = %d，期望 200", list.Code)
	}
	var page worker.TaskPage
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	byName := make(map[string]worker.TaskSummary, len(page.Items))
	for _, item := range page.Items {
		byName[item.TaskType] = item
	}
	cleanupTask, ok := byName["worker_history_cleanup"]
	if !ok {
		t.Fatalf("列表缺少 worker_history_cleanup：%v", byName)
	}
	if !cleanupTask.ManualTriggerAllowed || cleanupTask.Available {
		t.Fatalf("worker_history_cleanup 摘要 = %+v，期望可手动触发且 available=false", cleanupTask)
	}
	if cleanupTask.BlockedReason != worker.BlockedCodeWorkerUnavailable {
		t.Fatalf("blocked_reason = %q，期望 %q", cleanupTask.BlockedReason, worker.BlockedCodeWorkerUnavailable)
	}
	if pollTask, ok := byName["async_task_poll"]; !ok || !pollTask.Available || pollTask.BlockedReason != "" {
		t.Fatalf("非白名单任务摘要 = %+v，期望 available=true 且无 blocked_reason", pollTask)
	}

	// 触发：409 + problem type 以 worker_unavailable 结尾。
	blocked := httptest.NewRecorder()
	mux.ServeHTTP(blocked, settingsRequest(http.MethodPost,
		"/api/system/tasks/worker_history_cleanup:trigger", "", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": fmt.Sprintf("int-worker-offline-%d", time.Now().UnixNano())}))
	if blocked.Code != http.StatusConflict {
		t.Fatalf("worker 离线触发 = %d，期望 409（body=%s）", blocked.Code, blocked.Body.String())
	}
	var problem map[string]any
	if err := json.Unmarshal(blocked.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	typeURI, _ := problem["type"].(string)
	if !strings.HasSuffix(typeURI, "/"+worker.BlockedCodeWorkerUnavailable) {
		t.Fatalf("problem type = %q，期望以 %q 结尾", typeURI, worker.BlockedCodeWorkerUnavailable)
	}

	// 副作用为零：零条 pending 行（也零条任何运行行）。
	var pendingRows int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM worker_task_runs
		 WHERE task_type = 'worker_history_cleanup' AND status = 'pending'`).Scan(&pendingRows); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingRows != 0 {
		t.Fatalf("worker 离线仍写下 %d 条 pending 行", pendingRows)
	}
	if _, total, err := store.ListRuns(ctx, "worker_history_cleanup", 5, 0); err != nil || total != 0 {
		t.Fatalf("worker 离线触发产生了运行行: total=%d err=%v", total, err)
	}
}

// TestLogCleanupTriggerHonoursPartitionDropSwitch 锁定控制端裁决：
// 开关关闭 → 409 log_partition_drop_disabled、零执行、审计 outcome=blocked；
// 开关打开 → 202，调度 pass 执行一次（测试用 fake Run，不接触 retention DSN）。
func TestLogCleanupTriggerHonoursPartitionDropSwitch(t *testing.T) {
	db, logs := systemTaskIntegrationDBs(t)
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	for _, table := range []string{"worker_task_runs", "worker_task_locks", "worker_tasks"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	if _, err := logs.ExecContext(ctx, `DELETE FROM audit_logs WHERE detail->>'event' = 'system.task.trigger'`); err != nil {
		t.Fatalf("clear audit: %v", err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"worker_task_runs", "worker_task_locks", "worker_tasks"} {
			_, _ = db.Exec("DELETE FROM " + table)
		}
		_, _ = logs.Exec(`DELETE FROM audit_logs WHERE detail->>'event' = 'system.task.trigger'`)
	})

	var enabled int32
	var executions int32
	task := worker.Task{
		Name:                 "log_cleanup",
		Interval:             0,
		Timeout:              time.Second,
		ManualTriggerAllowed: true,
		BlockedCode:          "log_partition_drop_disabled",
		Enabled:              func() bool { return atomic.LoadInt32(&enabled) == 1 },
		Run: func(context.Context) (worker.Result, error) {
			atomic.AddInt32(&executions, 1)
			return worker.Result{"dropped_partitions": 0}, nil
		},
	}
	registry, err := worker.NewRegistry(task)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	store, _ := worker.NewSQLStore(db)
	if err := store.EnsureTaskRows(ctx, registry.Tasks()); err != nil {
		t.Fatalf("ensure rows: %v", err)
	}
	service, err := worker.NewService(worker.ServiceConfig{
		Registry: registry, Store: store, Lease: worker.NewRedisLease(rdb), Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	mux := http.NewServeMux()
	NewSystemTaskHandlers(SystemTaskHandlerConfig{
		Service: service, Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(logs),
		Idem: NewRedisIdempotencyStore(rdb), Logger: testLogger(),
	}).Register(mux)

	// 关：409 + 固定错误码 + 零执行。
	blocked := httptest.NewRecorder()
	mux.ServeHTTP(blocked, settingsRequest(http.MethodPost,
		"/api/system/tasks/log_cleanup:trigger", "", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": fmt.Sprintf("drop-off-%d", time.Now().UnixNano())}))
	if blocked.Code != http.StatusConflict {
		t.Fatalf("开关关闭触发 = %d，期望 409（body=%s）", blocked.Code, blocked.Body.String())
	}
	var problem map[string]any
	if err := json.Unmarshal(blocked.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem["type"] != "https://newtg.dev/problems/log_partition_drop_disabled" {
		t.Fatalf("problem type = %v", problem["type"])
	}
	if _, total, err := store.ListRuns(ctx, "log_cleanup", 5, 0); err != nil || total != 0 {
		t.Fatalf("开关关闭不得产生运行行: total=%d err=%v", total, err)
	}
	if got := atomic.LoadInt32(&executions); got != 0 {
		t.Fatalf("开关关闭仍执行了 %d 次", got)
	}
	var blockedAudit int
	if err := logs.QueryRowContext(ctx, `SELECT count(*) FROM audit_logs
		WHERE detail->>'event' = 'system.task.trigger' AND detail->>'outcome' = 'blocked'`).Scan(&blockedAudit); err != nil {
		t.Fatalf("query blocked audit: %v", err)
	}
	if blockedAudit != 1 {
		t.Fatalf("blocked 审计 = %d，期望 1", blockedAudit)
	}

	// 开：202 + 调度执行一次。
	atomic.StoreInt32(&enabled, 1)
	accepted := httptest.NewRecorder()
	mux.ServeHTTP(accepted, settingsRequest(http.MethodPost,
		"/api/system/tasks/log_cleanup:trigger", "", systemTaskAdmin(),
		map[string]string{"Idempotency-Key": fmt.Sprintf("drop-on-%d", time.Now().UnixNano())}))
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("开关打开触发 = %d，期望 202（body=%s）", accepted.Code, accepted.Body.String())
	}
	scheduler, err := worker.NewScheduler(worker.SchedulerConfig{
		Registry: registry, Store: store, Lease: worker.NewRedisLease(rdb),
		NodeID: "integration-node", Logger: testLogger(), PassInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	scheduler.Pass(ctx)
	scheduler.Wait()
	if got := atomic.LoadInt32(&executions); got != 1 {
		t.Fatalf("开关打开执行次数 = %d，期望 1", got)
	}
	runs, total, err := store.ListRuns(ctx, "log_cleanup", 5, 0)
	if err != nil || total != 1 || runs[0].Status != worker.RunSucceeded {
		t.Fatalf("运行记录 = %+v total=%d err=%v", runs, total, err)
	}
}
