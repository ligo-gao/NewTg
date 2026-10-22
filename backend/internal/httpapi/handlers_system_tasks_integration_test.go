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
