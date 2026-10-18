package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubDBProbe / stubRedisProbe：Checker 字段（窄接口）的内存实现，不触真 PG/Redis（R-03 裁决：
// NewChecker 收具体类型、Checker 内部字段为窄接口、测试直注 stub）。
type stubDBProbe struct {
	selectOneErr error
	version      int
	dirty        bool
	migErr       error
}

func (s stubDBProbe) selectOne(ctx context.Context) error { return s.selectOneErr }

func (s stubDBProbe) migrationState(ctx context.Context) (int, bool, error) {
	return s.version, s.dirty, s.migErr
}

type stubRedisProbe struct{ err error }

func (s stubRedisProbe) ping(ctx context.Context) error { return s.err }

// readyzRecorder 以 httptest 驱动 Checker 并解析信封。
func readyzRecorder(t *testing.T, c *Checker, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	if path == "/readyz" {
		c.HandlerReadyz(rec, req)
	} else {
		c.HandlerHealthz(rec, req)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v; body=%q", err, rec.Body.String())
	}
	return rec, body
}

func newTestChecker(db dbProbe, rdb redisProbe) *Checker {
	c := &Checker{
		db:                db,
		rdb:               rdb,
		migrationExpected: 1, // M0 冻结期望值
		node:              "test-node",
	}
	c.MarkConfigLoaded()
	c.MarkStartupDone()
	return c
}

// TestReadyzAllOK：全 ok → 200，信封恰为 {status,checks,version,node}，
// checks 恰 5 键 postgres|redis|migration|config|startup 全 "ok"。
func TestReadyzAllOK(t *testing.T) {
	c := newTestChecker(stubDBProbe{version: 1}, stubRedisProbe{})
	rec, body := readyzRecorder(t, c, "/readyz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %v, want ok", body["status"])
	}
	if body["version"] == nil || body["version"] == "" {
		t.Fatal("version field missing or empty")
	}
	if body["node"] != "test-node" {
		t.Fatalf("node = %v, want test-node", body["node"])
	}
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("checks missing or not object: %v", body["checks"])
	}
	if len(checks) != 5 {
		t.Fatalf("checks has %d keys, want exactly 5: %v", len(checks), checks)
	}
	for _, k := range []string{"postgres", "redis", "migration", "config", "startup"} {
		if checks[k] != "ok" {
			t.Fatalf("checks[%q] = %v, want ok (all: %v)", k, checks[k], checks)
		}
	}
}

// TestReadyzRedisFail：Redis err → 503 + status=degraded + checks.redis="fail"，其余 4 键仍 "ok"。
func TestReadyzRedisFail(t *testing.T) {
	c := newTestChecker(stubDBProbe{version: 1}, stubRedisProbe{err: errors.New("redis down")})
	rec, body := readyzRecorder(t, c, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if body["status"] != "degraded" {
		t.Fatalf("status field = %v, want degraded", body["status"])
	}
	checks := body["checks"].(map[string]any)
	if len(checks) != 5 {
		t.Fatalf("checks has %d keys, want exactly 5: %v", len(checks), checks)
	}
	if checks["redis"] != "fail" {
		t.Fatalf("checks[redis] = %v, want fail", checks["redis"])
	}
	for _, k := range []string{"postgres", "migration", "config", "startup"} {
		if checks[k] != "ok" {
			t.Fatalf("checks[%q] = %v, want ok (all: %v)", k, checks[k], checks)
		}
	}
}

// TestHealthzAlwaysOK：DB/Redis 全断时 /healthz 仍恒 200 {"status":"ok"}，且不触碰依赖。
func TestHealthzAlwaysOK(t *testing.T) {
	c := newTestChecker(
		stubDBProbe{selectOneErr: errors.New("db down"), migErr: errors.New("db down")},
		stubRedisProbe{err: errors.New("redis down")},
	)
	rec, body := readyzRecorder(t, c, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Fatalf("body = %v, want exactly {\"status\":\"ok\"}", body)
	}
}

// TestReadyzMigrationVersionMismatch：schema_migrations 版本 != migrationExpected → migration="fail"。
func TestReadyzMigrationVersionMismatch(t *testing.T) {
	c := newTestChecker(stubDBProbe{version: 2}, stubRedisProbe{})
	rec, body := readyzRecorder(t, c, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	checks := body["checks"].(map[string]any)
	if checks["migration"] != "fail" {
		t.Fatalf("checks[migration] = %v, want fail (all: %v)", checks["migration"], checks)
	}
	for _, k := range []string{"postgres", "redis", "config", "startup"} {
		if checks[k] != "ok" {
			t.Fatalf("checks[%q] = %v, want ok", k, checks[k])
		}
	}
}

// TestReadyzMigrationDirty：dirty=true → migration="fail"（即使版本号相符）。
func TestReadyzMigrationDirty(t *testing.T) {
	c := newTestChecker(stubDBProbe{version: 1, dirty: true}, stubRedisProbe{})
	rec, body := readyzRecorder(t, c, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	checks := body["checks"].(map[string]any)
	if checks["migration"] != "fail" {
		t.Fatalf("checks[migration] = %v, want fail (all: %v)", checks["migration"], checks)
	}
}

// TestReadyzFlagsUnset：R4/R5 标志未置位时 config/startup="fail" 且整体 degraded。
func TestReadyzFlagsUnset(t *testing.T) {
	c := &Checker{
		db:                stubDBProbe{version: 1},
		rdb:               stubRedisProbe{},
		migrationExpected: 1,
		node:              "test-node",
	}
	rec, body := readyzRecorder(t, c, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	checks := body["checks"].(map[string]any)
	if checks["config"] != "fail" || checks["startup"] != "fail" {
		t.Fatalf("config=%v startup=%v, want fail/fail", checks["config"], checks["startup"])
	}
	for _, k := range []string{"postgres", "redis", "migration"} {
		if checks[k] != "ok" {
			t.Fatalf("checks[%q] = %v, want ok", k, checks[k])
		}
	}

	// 置位后必须翻转为 ok。
	c.MarkConfigLoaded()
	c.MarkStartupDone()
	rec, body = readyzRecorder(t, c, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("after flags set: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	checks = body["checks"].(map[string]any)
	if checks["config"] != "ok" || checks["startup"] != "ok" {
		t.Fatalf("after flags set: config=%v startup=%v, want ok/ok", checks["config"], checks["startup"])
	}
}
