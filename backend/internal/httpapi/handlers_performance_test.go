package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件验收 D3a 性能管理端点（modules-08 §3.19 / F-08-48～50）：
// 权限门、确认头与实例作用域的零副作用顺序、10 分钟阈值、日志白名单/
// 活动文件保护、GC 前后值、三类审计的 category 与 detail.action。
//
// 集成库只用于 recent-operations 的 SQL 过滤（handlers_performance_integration_test.go）；
// 本文件全部使用注入的环境变量与临时目录，不依赖外部服务。

type perfTestEnv struct {
	mux    *http.ServeMux
	env    map[string]string
	audit  *fakeAuditExec
	now    time.Time
	remove func(string) error
	// gc 是运行时 GC 接缝；nil 时走真实 runtime.GC（GC 行为用例）。
	gc func()
}

func newPerfTestEnv(t *testing.T) *perfTestEnv {
	t.Helper()
	return newPerfTestEnvWithRedis(t, nil)
}

// newPerfTestEnvWithRedis 允许注入 Redis 客户端（429 限流用例）。
func newPerfTestEnvWithRedis(t *testing.T, rdb *redis.Client) *perfTestEnv {
	t.Helper()
	exec := &fakeAuditExec{}
	env := &perfTestEnv{
		env:   map[string]string{},
		audit: exec,
		now:   time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{
		Audit:  audit.New(exec),
		Logger: testLogger(),
		Authz:  rbac.NewAuthzMiddleware(),
		Redis:  rdb,
		Now:    func() time.Time { return env.now },
		Getenv: func(key string) string { return env.env[key] },
		Hostname: func() (string, error) {
			return "test-host", nil
		},
		Remove: func(path string) error {
			if env.remove != nil {
				return env.remove(path)
			}
			return os.Remove(path)
		},
		RuntimeGC: func() {
			if env.gc != nil {
				env.gc()
				return
			}
			runtime.GC()
		},
	})
	mux := http.NewServeMux()
	handlers.Register(mux)
	env.mux = mux
	return env
}

func perfManager() *auth.Principal {
	return &auth.Principal{UserID: 1, Role: rbac.RoleAdmin, Permissions: []string{rbac.PermSettingsManage}}
}

func perfRequest(method, target, body string, headers map[string]string) *http.Request {
	return settingsRequest(method, target, body, perfManager(), headers)
}

func decodePerfJSON(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
}

func writePerfFile(t *testing.T, dir, name string, modTime time.Time, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("期望文件仍存在: %s (%v)", path, err)
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("期望文件已删除: %s (%v)", path, err)
	}
}

// TestPerformanceEndpointsRequireSettingsManage：性能管理端点走同一权限门，
// 普通用户直链接口 403（Q2）。
func TestPerformanceEndpointsRequireSettingsManage(t *testing.T) {
	env := newPerfTestEnv(t)
	user := &auth.Principal{UserID: 7, Role: rbac.RoleUser}
	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodGet, "/api/performance/stats", ""},
		{http.MethodDelete, "/api/performance/disk-cache", ""},
		{http.MethodPost, "/api/performance/gc", ""},
		{http.MethodGet, "/api/performance/logs", ""},
		{http.MethodDelete, "/api/performance/logs?mode=by_days&value=30", ""},
		{http.MethodGet, "/api/performance/recent-operations", ""},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, settingsRequest(tc.method, tc.target, tc.body, user, nil))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d，期望 403（body=%s）", tc.method, tc.target, rr.Code, rr.Body.String())
		}
	}
}

// TestPerformanceWritesRequireConfirmHeader：缺头或其他值 → 422 且零删除。
func TestPerformanceWritesRequireConfirmHeader(t *testing.T) {
	env := newPerfTestEnv(t)
	cacheDir := t.TempDir()
	env.env["DISK_CACHE_ENABLED"] = "true"
	env.env["DISK_CACHE_PATH"] = cacheDir
	old := env.now.Add(-time.Hour)
	cacheFile := writePerfFile(t, cacheDir, "cache-a", old, "cache")

	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	logFile := writePerfFile(t, logDir, "newtg-1.log", old, "log")

	cases := []struct {
		method string
		target string
		header map[string]string
	}{
		{http.MethodDelete, "/api/performance/disk-cache", nil},
		{http.MethodDelete, "/api/performance/disk-cache", map[string]string{"X-Confirm-Action": "yes"}},
		{http.MethodDelete, "/api/performance/logs?mode=by_days&value=1", nil},
		{http.MethodPost, "/api/performance/gc", nil},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, perfRequest(tc.method, tc.target, "", tc.header))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s %s = %d，期望 422（body=%s）", tc.method, tc.target, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "confirm_required") {
			t.Fatalf("缺少 CONFIRM_REQUIRED problem type: %s", rr.Body.String())
		}
	}
	assertExists(t, cacheFile)
	assertExists(t, logFile)
	if calls := env.audit.snapshot(); len(calls) != 0 {
		t.Fatalf("缺确认头的请求不得产生审计: %v", calls)
	}
}

// TestPerformanceInstanceMismatchReturns409：instance_id 不属于当前实例 →
// 409 且零删除（Q4）。
func TestPerformanceInstanceMismatchReturns409(t *testing.T) {
	env := newPerfTestEnv(t)
	env.env["INSTANCE_ID"] = "gateway-1"
	cacheDir := t.TempDir()
	env.env["DISK_CACHE_ENABLED"] = "true"
	env.env["DISK_CACHE_PATH"] = cacheDir
	old := env.now.Add(-time.Hour)
	cacheFile := writePerfFile(t, cacheDir, "cache-a", old, "cache")

	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	logFile := writePerfFile(t, logDir, "newtg-1.log", old, "log")

	confirm := map[string]string{"X-Confirm-Action": "true"}
	cases := []struct {
		method string
		target string
		body   string
	}{
		{http.MethodDelete, "/api/performance/disk-cache?instance_id=gateway-2", ""},
		{http.MethodDelete, "/api/performance/logs?mode=by_days&value=1&instance_id=gateway-2", ""},
		{http.MethodPost, "/api/performance/gc", `{"instance_id":"gateway-2"}`},
	}
	// GC 分支的零副作用用注入的接缝计数证明：实例不匹配时请求必须停在
	// h.gc() 之前。不能用进程全局 MemStats.NumGC 前后相等来推断——循环
	// 自身的分配会触发自动 GC，那个断言会 flake（D3a 复核 F1）。
	gcCalls := 0
	env.gc = func() { gcCalls++ }
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, perfRequest(tc.method, tc.target, tc.body, confirm))
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s %s = %d，期望 409（body=%s）", tc.method, tc.target, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "instance_mismatch") {
			t.Fatalf("缺少 INSTANCE_MISMATCH: %s", rr.Body.String())
		}
	}
	if gcCalls != 0 {
		t.Fatalf("实例不匹配的 GC 请求不得执行运行时 GC: 调用 %d 次", gcCalls)
	}
	assertExists(t, cacheFile)
	assertExists(t, logFile)
	// 409 是零副作用拒绝：三个分支都不得写审计。
	if calls := env.audit.snapshot(); len(calls) != 0 {
		t.Fatalf("实例不匹配不得写审计: %v", calls)
	}

	// 匹配当前实例时正常执行（证明 409 只因实例不符）。
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete,
		"/api/performance/disk-cache?instance_id=gateway-1", "", confirm))
	if rr.Code != http.StatusOK {
		t.Fatalf("匹配实例 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestPerformanceStatsFields：字段完整；目录缺失时 cache_stats.enabled=false
// 且 disk_cache_info 零值，不报 500（D3-03）。
func TestPerformanceStatsFields(t *testing.T) {
	env := newPerfTestEnv(t)
	env.env["INSTANCE_ID"] = "gateway-1"
	cacheDir := t.TempDir()
	env.env["DISK_CACHE_ENABLED"] = "true"
	env.env["DISK_CACHE_PATH"] = cacheDir
	env.env["DISK_CACHE_MAX_MB"] = "2048"
	env.env["DISK_CACHE_THRESHOLD_MB"] = "256"
	writePerfFile(t, cacheDir, "a", env.now.Add(-time.Hour), "aaaa")
	writePerfFile(t, cacheDir, "b", env.now, "bb")

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/stats", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("stats = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceStatsResponse
	decodePerfJSON(t, rr, &body)
	if body.InstanceID != "gateway-1" {
		t.Fatalf("instance_id = %q", body.InstanceID)
	}
	if !body.CacheStats.Enabled || body.CacheStats.FileCount != 2 || body.CacheStats.TotalBytes != 6 {
		t.Fatalf("cache_stats = %+v", body.CacheStats)
	}
	if body.DiskCacheInfo.Path != cacheDir || body.DiskCacheInfo.FileCount != 2 || body.DiskCacheInfo.LastModified == nil {
		t.Fatalf("disk_cache_info = %+v", body.DiskCacheInfo)
	}
	if !body.Config.Enabled || body.Config.Path != cacheDir || body.Config.MaxMB != 2048 || body.Config.ThresholdMB != 256 {
		t.Fatalf("config = %+v", body.Config)
	}
	if body.MemoryStats.SysBytes == 0 || body.MemoryStats.NumGoroutine == 0 {
		t.Fatalf("memory_stats = %+v", body.MemoryStats)
	}
	if body.DiskSpaceInfo.TotalBytes == 0 {
		t.Fatalf("disk_space_info = %+v", body.DiskSpaceInfo)
	}

	// 目录不存在：不 500，进入空态。
	env.env["DISK_CACHE_PATH"] = filepath.Join(cacheDir, "missing")
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/stats", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("缺失目录 stats = %d，期望 200", rr.Code)
	}
	decodePerfJSON(t, rr, &body)
	if body.CacheStats.Enabled || body.CacheStats.FileCount != 0 {
		t.Fatalf("缺失目录 cache_stats = %+v", body.CacheStats)
	}
	if body.DiskCacheInfo.Path != "" || body.DiskCacheInfo.FileCount != 0 || body.DiskCacheInfo.LastModified != nil {
		t.Fatalf("缺失目录 disk_cache_info 应为零值: %+v", body.DiskCacheInfo)
	}
}

// TestPerformanceDiskCacheCleanupOnlyInactive：只删 strict mtime < now-10min
// 的普通文件；目录跳过；部分失败进 failed_files（Q7）。
func TestPerformanceDiskCacheCleanupOnlyInactive(t *testing.T) {
	env := newPerfTestEnv(t)
	cacheDir := t.TempDir()
	env.env["DISK_CACHE_ENABLED"] = "true"
	env.env["DISK_CACHE_PATH"] = cacheDir

	old := writePerfFile(t, cacheDir, "old", env.now.Add(-11*time.Minute), "old")
	boundary := writePerfFile(t, cacheDir, "boundary", env.now.Add(-10*time.Minute), "bnd")
	recent := writePerfFile(t, cacheDir, "recent", env.now.Add(-time.Minute), "new")
	subdir := filepath.Join(cacheDir, "sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 符号链接即使目标 mtime 很旧也不能进入删除集：扫描按 Lstat 的
	// Mode().IsRegular() 过滤，删除路径不可能跟随链接碰到目录外文件。
	outside := writePerfFile(t, t.TempDir(), "outside", env.now.Add(-time.Hour), "outside")
	link := filepath.Join(cacheDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	failing := writePerfFile(t, cacheDir, "failing", env.now.Add(-time.Hour), "fail")
	env.remove = func(path string) error {
		if filepath.Base(path) == "failing" {
			return errors.New("permission denied")
		}
		return os.Remove(path)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete, "/api/performance/disk-cache", "",
		map[string]string{"X-Confirm-Action": "TRUE"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("cleanup = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceDiskClearResponse
	decodePerfJSON(t, rr, &body)
	if body.InstanceID != "test-host" {
		t.Fatalf("instance_id = %q", body.InstanceID)
	}
	// old 删除；failing 删除失败；boundary/recent 保留（边界严格小于）。
	if body.DeletedCount != 1 || body.FreedBytes != 3 {
		t.Fatalf("deleted=%d freed=%d（body=%+v）", body.DeletedCount, body.FreedBytes, body)
	}
	if len(body.FailedFiles) != 1 || body.FailedFiles[0] != "failing" {
		t.Fatalf("failed_files = %v", body.FailedFiles)
	}
	assertGone(t, old)
	assertExists(t, boundary)
	assertExists(t, recent)
	assertExists(t, failing)
	if _, err := os.Stat(subdir); err != nil {
		t.Fatalf("子目录必须保留: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("符号链接必须保留: %v", err)
	}
	assertExists(t, outside)

	// 部分失败仍记 success，detail 带 failed_count（D3-10）。
	calls := env.audit.snapshot()
	if len(calls) != 1 {
		t.Fatalf("审计条数 = %d，期望 1", len(calls))
	}
	assertPerfAudit(t, calls[0], audit.ActionSystemDiskCacheClear, "success")
	detail := auditDetail(t, calls[0])
	if detail["failed_count"].(float64) != 1 || detail["deleted_count"].(float64) != 1 {
		t.Fatalf("审计 detail = %v", detail)
	}
}

// TestPerformanceDiskCacheCleanupVanishedFileIsSuccess：扫描后文件被其他
// 清理者/轮转删除（Remove 返回包装后的 os.ErrNotExist）不是失败——清理
// 意图已达成，按 deleted 计数且不进 failed_files。
func TestPerformanceDiskCacheCleanupVanishedFileIsSuccess(t *testing.T) {
	env := newPerfTestEnv(t)
	cacheDir := t.TempDir()
	env.env["DISK_CACHE_ENABLED"] = "true"
	env.env["DISK_CACHE_PATH"] = cacheDir
	vanished := writePerfFile(t, cacheDir, "vanished", env.now.Add(-time.Hour), "gone")
	env.remove = func(path string) error {
		if filepath.Base(path) == "vanished" {
			// 模拟扫描后另一个清理者先删掉：文件确实没了，Remove 报到
			// ErrNotExist。包装一层验证调用方用 errors.Is 而不是 == 判定。
			_ = os.Remove(path)
			return fmt.Errorf("remove %s: %w", path, os.ErrNotExist)
		}
		return os.Remove(path)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete, "/api/performance/disk-cache", "",
		map[string]string{"X-Confirm-Action": "true"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("cleanup = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceDiskClearResponse
	decodePerfJSON(t, rr, &body)
	if body.DeletedCount != 1 || body.FreedBytes != 4 || len(body.FailedFiles) != 0 {
		t.Fatalf("扫描后消失应计为成功: %+v", body)
	}
	assertGone(t, vanished)
	if detail := auditDetail(t, env.audit.snapshot()[0]); detail["failed_count"].(float64) != 0 {
		t.Fatalf("审计 failed_count = %v", detail["failed_count"])
	}
}

// TestPerformanceDiskCacheNotConfigured：未启用/路径缺失 → 422。
func TestPerformanceDiskCacheNotConfigured(t *testing.T) {
	env := newPerfTestEnv(t)
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete, "/api/performance/disk-cache", "",
		map[string]string{"X-Confirm-Action": "true"}))
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "disk_cache_not_configured") {
		t.Fatalf("未配置 = %d（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestPerformanceWriteRateLimit：三个写操作共用 10/min/admin 的固定窗口；
// 第 11 次请求 429 并带 Retry-After（Redis 不可用时才 fail-open）。
func TestPerformanceWriteRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	env := newPerfTestEnvWithRedis(t, rdb)
	env.env["DISK_CACHE_ENABLED"] = "true"
	env.env["DISK_CACHE_PATH"] = t.TempDir()
	confirm := map[string]string{"X-Confirm-Action": "true"}

	for i := 0; i < performanceWriteRateLimit; i++ {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete, "/api/performance/disk-cache", "", confirm))
		if rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次 = %d（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete, "/api/performance/disk-cache", "", confirm))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次 = %d，期望 429（body=%s）",
			performanceWriteRateLimit+1, rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 缺少 Retry-After")
	}
	// 超限请求在列目录/删除之前返回：第 11 次不得再写审计。
	if calls := env.audit.snapshot(); len(calls) != performanceWriteRateLimit {
		t.Fatalf("审计条数 = %d，期望 %d", len(calls), performanceWriteRateLimit)
	}
}

// TestPerformanceGCRunsAndReturnsBeforeAfter：runtime.GC 同步执行、num_gc 前进、
// 重复触发各自写审计（Q8/Q9）。
func TestPerformanceGCRunsAndReturnsBeforeAfter(t *testing.T) {
	env := newPerfTestEnv(t)
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, perfRequest(http.MethodPost, "/api/performance/gc", "",
			map[string]string{"X-Confirm-Action": "true"}))
		if rr.Code != http.StatusOK {
			t.Fatalf("gc #%d = %d（body=%s）", i, rr.Code, rr.Body.String())
		}
		var body performanceGCResponse
		decodePerfJSON(t, rr, &body)
		if body.NumGCAfter <= body.NumGCBefore {
			t.Fatalf("GC 后 num_gc 未前进: %+v", body)
		}
		if body.HeapAllocBefore == 0 || body.HeapAllocAfter == 0 {
			t.Fatalf("堆数据缺失: %+v", body)
		}
		if body.DurationMS < 0 {
			t.Fatalf("duration_ms = %d", body.DurationMS)
		}
	}
	calls := env.audit.snapshot()
	if len(calls) != 2 {
		t.Fatalf("GC 审计条数 = %d，期望 2", len(calls))
	}
	for _, call := range calls {
		assertPerfAudit(t, call, audit.ActionSystemGCRun, "success")
		if detail := auditDetail(t, call); detail["duration_ms"] == nil {
			t.Fatalf("GC 审计缺 duration_ms: %v", detail)
		}
	}
	if runtime.NumGoroutine() == 0 {
		t.Fatal("goroutine 数异常")
	}
}

// TestPerformanceLogsListWhitelist：只统计 newtg-*.log 普通文件；目录、符号
// 链接与其他文件名不计数（Q6）。
func TestPerformanceLogsListWhitelist(t *testing.T) {
	env := newPerfTestEnv(t)
	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	writePerfFile(t, logDir, "newtg-2026-10-20.log", env.now, "log")
	writePerfFile(t, logDir, "other.log", env.now, "other")
	writePerfFile(t, logDir, "newtg-ignore.txt", env.now, "txt")
	if err := os.Mkdir(filepath.Join(logDir, "newtg-dir.log"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := writePerfFile(t, t.TempDir(), "target", env.now, "outside")
	if err := os.Symlink(target, filepath.Join(logDir, "newtg-link.log")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/logs", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("logs = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceLogsResponse
	decodePerfJSON(t, rr, &body)
	if !body.Enabled || body.LogDir != logDir || body.FileCount != 1 || body.TotalSize != 3 {
		t.Fatalf("logs = %+v", body)
	}
	if len(body.Files) != 1 || body.Files[0].Name != "newtg-2026-10-20.log" {
		t.Fatalf("files = %+v", body.Files)
	}
	if body.OldestTime == nil || body.NewestTime == nil {
		t.Fatalf("时间范围缺失: %+v", body)
	}

	// 未配置 LOG_DIR：200 空态（Q10）。
	env.env["LOG_DIR"] = ""
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/logs", "", nil))
	decodePerfJSON(t, rr, &body)
	if body.Enabled || body.FileCount != 0 || body.Files == nil {
		t.Fatalf("未配置空态 = %+v", body)
	}
}

// TestPerformanceLogCleanupByCount：保留最新 N 个；白名单外/目录/符号链接零删除。
func TestPerformanceLogCleanupByCount(t *testing.T) {
	env := newPerfTestEnv(t)
	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	var logs []string
	for i := 0; i < 5; i++ {
		logs = append(logs, writePerfFile(t, logDir,
			"newtg-"+string(rune('a'+i))+".log",
			env.now.Add(-time.Duration(5-i)*time.Hour), "x"))
	}
	other := writePerfFile(t, logDir, "app.log", env.now.Add(-time.Hour), "other")
	dir := filepath.Join(logDir, "newtg-sub.log")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	outside := writePerfFile(t, t.TempDir(), "target", env.now, "outside")
	link := filepath.Join(logDir, "newtg-link.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete,
		"/api/performance/logs?mode=by_count&value=2", "",
		map[string]string{"X-Confirm-Action": "true"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("by_count = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceLogCleanupResponse
	decodePerfJSON(t, rr, &body)
	if body.Mode != "by_count" || body.Value != 2 || body.DeletedCount != 3 || body.FreedBytes != 3 {
		t.Fatalf("by_count = %+v", body)
	}
	if body.SkippedActive {
		t.Fatalf("by_count 下活动文件恒在保留集，skipped_active 应为 false: %+v", body)
	}
	assertGone(t, logs[0])
	assertGone(t, logs[1])
	assertGone(t, logs[2])
	assertExists(t, logs[3])
	assertExists(t, logs[4])
	assertExists(t, other)
	assertExists(t, link)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("目录必须保留: %v", err)
	}
	assertExists(t, outside)
	assertPerfAudit(t, env.audit.snapshot()[0], audit.ActionSystemLogCleanupRun, "success")
}

// TestPerformanceLogCleanupByDaysSkipsActive：按 mtime 截止；活动文件（最新
// mtime）命中删除集时恒跳过并 skipped_active=true（Q6）。
func TestPerformanceLogCleanupByDaysSkipsActive(t *testing.T) {
	env := newPerfTestEnv(t)
	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	oldest := writePerfFile(t, logDir, "newtg-old.log", env.now.Add(-40*24*time.Hour), "aa")
	middle := writePerfFile(t, logDir, "newtg-middle.log", env.now.Add(-31*24*time.Hour), "bbb")
	// 活动文件也早于 cutoff：by_days 会把它算进候选集，但必须跳过。
	// 三个文件全部早于截止时间，最新 mtime 的那个就是活动文件。
	active := writePerfFile(t, logDir, "newtg-active.log", env.now.Add(-30*24*time.Hour-time.Hour), "cccc")

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete,
		"/api/performance/logs?mode=by_days&value=30", "",
		map[string]string{"X-Confirm-Action": "true"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("by_days = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceLogCleanupResponse
	decodePerfJSON(t, rr, &body)
	if body.DeletedCount != 2 || body.FreedBytes != 5 {
		t.Fatalf("by_days = %+v", body)
	}
	if !body.SkippedActive {
		t.Fatalf("活动文件命中删除集时必须 skipped_active=true: %+v", body)
	}
	assertGone(t, oldest)
	assertGone(t, middle)
	assertExists(t, active)
}

// TestPerformanceLogCleanupVanishedFileIsSuccess：与磁盘缓存同一语义，
// 日志文件在扫描后消失按成功计数、不进 failed_files。
func TestPerformanceLogCleanupVanishedFileIsSuccess(t *testing.T) {
	env := newPerfTestEnv(t)
	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	vanished := writePerfFile(t, logDir, "newtg-vanished.log", env.now.Add(-40*24*time.Hour), "aa")
	active := writePerfFile(t, logDir, "newtg-active.log", env.now.Add(-time.Hour), "cccc")
	env.remove = func(path string) error {
		if filepath.Base(path) == "newtg-vanished.log" {
			_ = os.Remove(path)
			return fmt.Errorf("remove %s: %w", path, os.ErrNotExist)
		}
		return os.Remove(path)
	}

	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete,
		"/api/performance/logs?mode=by_days&value=30", "",
		map[string]string{"X-Confirm-Action": "true"}))
	if rr.Code != http.StatusOK {
		t.Fatalf("cleanup = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body performanceLogCleanupResponse
	decodePerfJSON(t, rr, &body)
	if body.DeletedCount != 1 || body.FreedBytes != 2 || len(body.FailedFiles) != 0 {
		t.Fatalf("扫描后消失应计为成功: %+v", body)
	}
	assertGone(t, vanished)
	assertExists(t, active)
}

// TestPerformanceLogCleanupValidation：mode/value 非法与 LOG_DIR 未配置的
// 422 码（请求参数先于部署配置校验）。
func TestPerformanceLogCleanupValidation(t *testing.T) {
	env := newPerfTestEnv(t)
	confirm := map[string]string{"X-Confirm-Action": "true"}

	// 上限用例必须证明校验发生在扫描/删除之前（D3a 复核 F2）：目录先配好并
	// 放入一个远超 1 天截止线的候选文件。若实现先扫描再校验 value，这个文件
	// 就会被删掉；故断言 422 之后它仍在。
	logDir := t.TempDir()
	env.env["LOG_DIR"] = logDir
	oldCandidate := writePerfFile(t, logDir, "newtg-old.log",
		env.now.Add(-100*24*time.Hour), "old")

	cases := []struct {
		target string
		code   string
	}{
		{"/api/performance/logs?mode=by_hours&value=3", "invalid_mode"},
		{"/api/performance/logs?mode=by_days&value=0", "invalid_value"},
		{"/api/performance/logs?mode=by_days&value=abc", "invalid_value"},
		// 上限 36500：106752 天恰是 int64 纳秒溢出的起点，213504 更大；
		// 若放行会让 by_days 的截止时间绕回未来。
		{"/api/performance/logs?mode=by_days&value=36501", "invalid_value"},
		{"/api/performance/logs?mode=by_days&value=106752", "invalid_value"},
		{"/api/performance/logs?mode=by_days&value=213504", "invalid_value"},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete, tc.target, "", confirm))
		if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), tc.code) {
			t.Fatalf("%s = %d（body=%s）", tc.target, rr.Code, rr.Body.String())
		}
	}
	assertExists(t, oldCandidate)

	// LOG_DIR 未配置的 422（Q10）与参数校验是两条独立路径。
	env.env["LOG_DIR"] = ""
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete,
		"/api/performance/logs?mode=by_days&value=30", "", confirm))
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "log_dir_not_configured") {
		t.Fatalf("LOG_DIR 未配置 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if calls := env.audit.snapshot(); len(calls) != 0 {
		t.Fatalf("校验失败不得写审计: %v", calls)
	}

	// 上限本身合法：配好目录后 value=36500 必须通过参数校验（空目录 200）。
	env.env["LOG_DIR"] = t.TempDir()
	rr = httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodDelete,
		"/api/performance/logs?mode=by_days&value=36500", "", confirm))
	if rr.Code != http.StatusOK {
		t.Fatalf("value=36500 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestPerformanceRecentOperationsLimitValidation：limit 越界 422；nil 日志库
// 返回空列表而不是 500。
func TestPerformanceRecentOperationsLimitValidation(t *testing.T) {
	env := newPerfTestEnv(t)
	for _, raw := range []string{"0", "101", "abc"} {
		rr := httptest.NewRecorder()
		env.mux.ServeHTTP(rr, perfRequest(http.MethodGet,
			"/api/performance/recent-operations?limit="+raw, "", nil))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("limit=%s = %d，期望 422", raw, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	env.mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/recent-operations", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("recent = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body recentOperationsResponse
	decodePerfJSON(t, rr, &body)
	if body.Items == nil || len(body.Items) != 0 {
		t.Fatalf("nil 日志库应返回空 items: %+v", body)
	}
}

// TestApplyRecentOperationDetail：detail 缺字段/类型异常时保留零值；
// 日志清理行投影 mode/value 供参数摘要，其余行不输出这两个字段。
func TestApplyRecentOperationDetail(t *testing.T) {
	var item recentOperation
	applyRecentOperationDetail(&item, []byte(`{"instance_id":"n1","mode":"by_days","value":30,`+
		`"deleted_count":3,"freed_bytes":42,"failed_count":1,"duration_ms":8,"skipped_active":true}`))
	if item.InstanceID != "n1" || item.Mode != "by_days" || item.Value != 30 ||
		item.DeletedCount != 3 || item.FreedBytes != 42 ||
		item.FailedCount != 1 || item.DurationMS != 8 || !item.SkippedActive {
		t.Fatalf("detail 解析 = %+v", item)
	}
	// 日志清理行的 JSON 必须带 mode/value（LOW-7 的 D3b 参数摘要依赖）。
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"mode":"by_days"`) || !strings.Contains(string(raw), `"value":30`) {
		t.Fatalf("日志清理行缺少 mode/value: %s", raw)
	}

	item = recentOperation{}
	applyRecentOperationDetail(&item, []byte(`{"deleted_count":"oops"}`))
	if item.DeletedCount != 0 {
		t.Fatalf("脏数据不应 panic 或伪造: %+v", item)
	}
	// GC/磁盘缓存行的 detail 没有 mode/value：可选字段必须被省略。
	raw, err = json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"mode"`) || strings.Contains(string(raw), `"value"`) {
		t.Fatalf("非日志清理行不应输出 mode/value: %s", raw)
	}
}

// assertPerfAudit 断言一条审计的 category/result/action。
func assertPerfAudit(t *testing.T, call []any, wantAction, wantResult string) {
	t.Helper()
	if len(call) < 12 {
		t.Fatalf("审计参数不足: %v", call)
	}
	if call[2] != "operation" {
		t.Fatalf("category = %v，期望 operation", call[2])
	}
	if call[8] != wantResult {
		t.Fatalf("result = %v，期望 %s", call[8], wantResult)
	}
	detail := auditDetail(t, call)
	if detail["action"] != wantAction {
		t.Fatalf("detail.action = %v，期望 %s", detail["action"], wantAction)
	}
	if call[10] != nil || call[11] != nil {
		t.Fatalf("before/after 必须为 null: before=%v after=%v", call[10], call[11])
	}
}

func auditDetail(t *testing.T, call []any) map[string]any {
	t.Helper()
	raw, ok := call[9].(string)
	if !ok {
		t.Fatalf("detail 不是 JSON 字符串: %T", call[9])
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("detail 解码: %v（%s）", err, raw)
	}
	return detail
}

// TestPerfEnvSnapshotRedactsPrometheusURL：快照会进 Task 10 的「运行参数」
// 区（浏览器可见），PROMETHEUS_URL 的 userinfo、query 与 fragment 都必须
// 先脱敏；无法解析的值按空串展示（Validate 会拒绝它，页面显示「未接入」）。
func TestPerfEnvSnapshotRedactsPrometheusURL(t *testing.T) {
	env := map[string]string{"PROMETHEUS_URL": "http://user:pass@prom:9090"}
	handlers := NewPerformanceHandlers(PerformanceHandlerConfig{
		Getenv: func(key string) string { return env[key] },
	})

	if got := handlers.envSnapshot().PrometheusURL; got != "http://user:xxxxx@prom:9090" {
		t.Fatalf("PrometheusURL = %q, want 口令已脱敏", got)
	}

	// query / fragment 也可能带 token：内容整段打码，只留分隔符。
	env["PROMETHEUS_URL"] = "http://prom:9090/metrics?token=s3cr3t#section"
	if got := handlers.envSnapshot().PrometheusURL; got != "http://prom:9090/metrics?…#…" {
		t.Fatalf("PrometheusURL = %q, want query/fragment 内容已打码", got)
	}

	// fragment 内容里可以合法地含 '?'：不能把它当成 query 分隔符，否则
	// '#' 之后的片段前缀会被原样回显。
	env["PROMETHEUS_URL"] = "http://prom:9090/metrics#access_token=s3cr3t?a=b"
	if got := handlers.envSnapshot().PrometheusURL; got != "http://prom:9090/metrics#…" {
		t.Fatalf("PrometheusURL = %q, want 含 '?' 的 fragment 内容也整段打码", got)
	}

	env["PROMETHEUS_URL"] = "://bad"
	if got := handlers.envSnapshot().PrometheusURL; got != "" {
		t.Fatalf("无法解析的 URL 应展示为空串，got %q", got)
	}

	env["PROMETHEUS_URL"] = ""
	if got := handlers.envSnapshot().PrometheusURL; got != "" {
		t.Fatalf("未配置时应为空串，got %q", got)
	}
}
