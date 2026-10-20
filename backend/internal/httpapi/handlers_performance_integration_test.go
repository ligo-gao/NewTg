//go:build integration

package httpapi

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// D3a 的 recent-operations 需要真实 audit_logs（jsonb 过滤 + 时间排序），
// 因此单独放在集成测试里。运行方式见包内其它 integration 文件；未设置
// NEWTG_TEST_LOGS_DSN 时跳过。

type perfIntegrationEnv struct {
	mux *http.ServeMux
	db  *sql.DB
	env map[string]string
}

func newPerfIntegrationEnv(t *testing.T) *perfIntegrationEnv {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	ensureAuditLogsTable(t, db)

	env := &perfIntegrationEnv{
		db:  db,
		env: map[string]string{"INSTANCE_ID": "itest-gateway"},
	}
	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{
		LogDB:  db,
		Audit:  audit.New(db),
		Logger: testLogger(),
		Authz:  rbac.NewAuthzMiddleware(),
		Getenv: func(key string) string { return env.env[key] },
		Hostname: func() (string, error) {
			return "itest-gateway", nil
		},
	})
	mux := http.NewServeMux()
	handlers.Register(mux)
	env.mux = mux
	return env
}

// TestIntegrationPerformanceRecentOperationsEndToEnd：一次 GC 写真实
// audit_logs，再由 recent-operations 读回（category/detail.action 过滤、
// actor、detail 字段投影）。
func TestIntegrationPerformanceRecentOperationsEndToEnd(t *testing.T) {
	env := newPerfIntegrationEnv(t)
	const userID = int64(424242)
	if _, err := env.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID)
	})

	principal := &auth.Principal{
		UserID: userID, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage},
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodPost, "/api/performance/gc", "",
		principal, map[string]string{"X-Confirm-Action": "true"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("gc = %d（body=%s）", rr.Code, rr.Body.String())
	}

	var category, action, result string
	if err := env.db.QueryRow(`SELECT category, detail->>'action', result
		FROM audit_logs WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, userID).
		Scan(&category, &action, &result); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if category != "operation" || action != audit.ActionSystemGCRun || result != "success" {
		t.Fatalf("审计行 = category=%s action=%s result=%s", category, action, result)
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet, "/api/performance/recent-operations",
		"", principal, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("recent = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body recentOperationsResponse
	decodePerfJSON(t, rr, &body)
	if len(body.Items) != 1 {
		t.Fatalf("items = %+v", body.Items)
	}
	item := body.Items[0]
	if item.Action != audit.ActionSystemGCRun || item.InstanceID != "itest-gateway" ||
		item.Result != "success" {
		t.Fatalf("recent item = %+v", item)
	}
	if item.Actor != "user#424242" {
		t.Fatalf("actor = %q，期望审计既有 username 字段", item.Actor)
	}
	if item.Route != "/api/performance/gc" {
		t.Fatalf("route = %q", item.Route)
	}
}

// TestIntegrationPerformanceRecentOperationsTieBreakAndParamProjection：
// 同一 created_at 的行按 id DESC 稳定排序（LOW-2），且日志清理行的
// mode/value 从 detail 投影出来供参数摘要列使用（LOW-7）。
func TestIntegrationPerformanceRecentOperationsTieBreakAndParamProjection(t *testing.T) {
	env := newPerfIntegrationEnv(t)
	const userID = int64(424244)
	if _, err := env.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID)
	})

	sameTime := time.Now().UTC().Add(-time.Hour)
	first := `{"action":"` + audit.ActionSystemLogCleanupRun + `","instance_id":"itest-gateway",` +
		`"mode":"by_count","value":2,"deleted_count":1}`
	second := `{"action":"` + audit.ActionSystemLogCleanupRun + `","instance_id":"itest-gateway",` +
		`"mode":"by_days","value":30,"deleted_count":4}`
	for _, detail := range []string{first, second} {
		if _, err := env.db.Exec(`INSERT INTO audit_logs
			(user_id, username, category, method, route, result, detail, created_at)
			VALUES ($1, 'tester', 'operation', 'DELETE', '/api/performance/logs', 'success', $2::jsonb, $3)`,
			userID, detail, sameTime); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/performance/recent-operations?limit=2", "", perfManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("recent = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body recentOperationsResponse
	decodePerfJSON(t, rr, &body)
	if len(body.Items) != 2 {
		t.Fatalf("items = %+v", body.Items)
	}
	// 同一时间戳：后插入（id 更大）的行在前。
	if body.Items[0].Mode != "by_days" || body.Items[0].Value != 30 {
		t.Fatalf("id DESC tiebreaker 失效或 mode/value 未投影: %+v", body.Items[0])
	}
	if body.Items[1].Mode != "by_count" || body.Items[1].Value != 2 {
		t.Fatalf("第二行 = %+v", body.Items[1])
	}
}

// TestIntegrationPerformanceRecentOperationsFilterAndLimit：只返回三类
// detail.action + category=operation 的行，limit 生效且倒序。
func TestIntegrationPerformanceRecentOperationsFilterAndLimit(t *testing.T) {
	env := newPerfIntegrationEnv(t)
	const userID = int64(424243)
	if _, err := env.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, userID)
	})

	base := time.Now().UTC().Add(-time.Hour)
	rows := []struct {
		category string
		action   string
		offset   time.Duration
	}{
		{"operation", audit.ActionSystemDiskCacheClear, 1 * time.Minute},
		{"operation", audit.ActionSystemGCRun, 2 * time.Minute},
		{"operation", audit.ActionSystemLogCleanupRun, 3 * time.Minute},
		{"operation", "token.create", 4 * time.Minute},
		{"login", audit.ActionSystemGCRun, 5 * time.Minute},
	}
	for _, row := range rows {
		detail := `{"action":"` + row.action + `","instance_id":"itest-gateway","deleted_count":2,` +
			`"freed_bytes":7,"failed_count":0,"duration_ms":5,"skipped_active":false}`
		if _, err := env.db.Exec(`INSERT INTO audit_logs
			(user_id, username, category, method, route, result, detail, created_at)
			VALUES ($1, 'tester', $2, 'DELETE', '/api/performance/test', 'success', $3::jsonb, $4)`,
			userID, row.category, detail, base.Add(-row.offset)); err != nil {
			t.Fatalf("insert audit row: %v", err)
		}
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/performance/recent-operations?limit=2", "", perfManager(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("recent limit=2 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body recentOperationsResponse
	decodePerfJSON(t, rr, &body)
	if len(body.Items) != 2 {
		t.Fatalf("limit=2 items = %+v", body.Items)
	}
	// 最新的是 disk_cache.clear（offset 1m），其次 gc.run（2m）。
	if body.Items[0].Action != audit.ActionSystemDiskCacheClear ||
		body.Items[1].Action != audit.ActionSystemGCRun {
		t.Fatalf("排序/过滤错误: %+v", body.Items)
	}
	if body.Items[0].DeletedCount != 2 || body.Items[0].FreedBytes != 7 ||
		body.Items[0].DurationMS != 5 || body.Items[0].SkippedActive {
		t.Fatalf("detail 投影 = %+v", body.Items[0])
	}

	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, settingsRequest(http.MethodGet,
		"/api/performance/recent-operations", "", perfManager(), nil))
	decodePerfJSON(t, rr, &body)
	if len(body.Items) != 3 {
		t.Fatalf("默认 limit 应只含三类动作的 3 行: %+v", body.Items)
	}
}
