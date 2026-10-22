package platform

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/migrations"
	migrationslogs "newtg/gateway/migrations-logs"
	migrationsmetrics "newtg/gateway/migrations-metrics"
)

// TestExpectedSchemaVersion 用 fixture FS 验证「最高 14 位时间戳版本」口径：
// 取最大的 up 版本（20260103000003），down 文件与子目录不参与。
func TestExpectedSchemaVersion(t *testing.T) {
	fixture := fstest.MapFS{
		"20260101000001_init.up.sql":     {Data: []byte("-- up 1")},
		"20260101000001_init.down.sql":   {Data: []byte("-- down 1")},
		"20260102000002_more.up.sql":     {Data: []byte("-- up 2")},
		"20260102000002_more.down.sql":   {Data: []byte("-- down 2")},
		"20260103000003_latest.up.sql":   {Data: []byte("-- up 3")},
		"subdir/20260104000004_x.up.sql": {Data: []byte("-- 子目录不计数")},
	}

	got, err := expectedSchemaVersionFS(fixture)
	if err != nil {
		t.Fatalf("expectedSchemaVersionFS: %v", err)
	}
	if got != 20260103000003 {
		t.Fatalf("expectedSchemaVersionFS = %d, want 20260103000003", got)
	}
}

// TestExpectedSchemaVersionErrors：无 up 文件或版本前缀非 14 位数字 → error。
func TestExpectedSchemaVersionErrors(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"no up files": {
			"20260101000001_init.down.sql": {Data: []byte("-- down")},
		},
		"short prefix": {
			"123_init.up.sql": {Data: []byte("-- up")},
		},
		"non-digit prefix": {
			"2026010100000x_init.up.sql": {Data: []byte("-- up")},
		},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := expectedSchemaVersionFS(fsys); err == nil {
				t.Fatalf("expectedSchemaVersionFS(%s) = nil error, want error", name)
			}
		})
	}
}

// embeddedUpVersionPattern 按 platform-04 §2.3 V1 的文件名口径匹配：
// {14 位 UTC 时间戳}_{name}.up.sql。测试用它与实现解耦地推导期望版本，
// 新增迁移时无需再手改断言（M0 评审 Task 6）。
var embeddedUpVersionPattern = regexp.MustCompile(`^(\d{14})_.*\.up\.sql$`)

// highestEmbeddedUpVersion 独立于 ExpectedSchemaVersion，从 FS 文件名推导最高
// up 版本。故意复用「正则 + ParseUint」而非被测实现的分割逻辑，避免测试与实现
// 共享同一处错误时仍然通过。
func highestEmbeddedUpVersion(t *testing.T, fsys fs.FS) uint {
	t.Helper()
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var (
		highest uint
		found   bool
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := embeddedUpVersionPattern.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}
		version, err := strconv.ParseUint(matches[1], 10, 64)
		if err != nil {
			t.Fatalf("parse version from %q: %v", entry.Name(), err)
		}
		if !found || uint(version) > highest {
			highest = uint(version)
			found = true
		}
	}
	if !found {
		t.Fatalf("embedded FS contains no *.up.sql files")
	}
	return highest
}

// TestExpectedSchemaVersionEmbedded 验证公共签名对真实嵌入目录的版本解析：
// 期望值从嵌入 FS 的 up 文件名推导，而不是硬编码时间戳——否则每新增一条
// 迁移都会误报失败（M0 评审 Task 6 的加固点）。
func TestExpectedSchemaVersionEmbedded(t *testing.T) {
	cases := []struct {
		name string
		fs   embed.FS
	}{
		{name: "main", fs: migrations.FS},
		{name: "logs", fs: migrationslogs.FS},
		{name: "metrics", fs: migrationsmetrics.FS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExpectedSchemaVersion(tc.fs)
			if err != nil {
				t.Fatalf("ExpectedSchemaVersion(%s): %v", tc.name, err)
			}
			if want := highestEmbeddedUpVersion(t, tc.fs); got != want {
				t.Fatalf("ExpectedSchemaVersion(%s) = %d, want %d (derived from embedded FS)",
					tc.name, got, want)
			}
		})
	}
}

// TestMigrateAdvisoryLockKey 冻结咨询锁键（跨进程串行化的契约值）。
func TestMigrateAdvisoryLockKey(t *testing.T) {
	if migrateAdvisoryLockKey != 727272 {
		t.Fatalf("migrateAdvisoryLockKey = %d, want 727272", migrateAdvisoryLockKey)
	}
}

// TestMigrateLockRetryInterval 冻结锁重试间隔（platform-04 迁移串行化契约：
// 拿不到咨询锁时每 2s 重试；改动必须显式同步测试与文档）。
func TestMigrateLockRetryInterval(t *testing.T) {
	if migrateLockRetryInterval != 2*time.Second {
		t.Fatalf("migrateLockRetryInterval = %v, want 2s", migrateLockRetryInterval)
	}
}

// applyCall 记录一次迁移执行：DSN 与源目录类别（main|logs）。
type applyCall struct {
	dsn  string
	kind string
}

// sourceKind 从嵌入源识别迁移流：日志库 init 含 usage_logs，主库 init 不含。
func sourceKind(src embed.FS) string {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return "error: " + err.Error()
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		data, err := fs.ReadFile(src, e.Name())
		if err != nil {
			return "error: " + err.Error()
		}
		if strings.Contains(string(data), "CREATE TABLE usage_logs") {
			return "logs"
		}
		if strings.Contains(string(data), "CREATE TABLE users") {
			return "main"
		}
		return "error: unrecognized migration source"
	}
	return "error: no *.up.sql"
}

// recordingExecutor 恒拿锁、记录 Up 顺序；用于断言主库先于日志库与错误传播。
type recordingExecutor struct {
	mu       sync.Mutex
	applies  []applyCall
	upErr    map[string]error
	releases int
	lockDSN  string
}

func (r *recordingExecutor) AcquireLock(ctx context.Context, dsn string) (migrationLock, error) {
	r.mu.Lock()
	r.lockDSN = dsn
	r.mu.Unlock()
	return &recordingLock{owner: r}, nil
}

func (r *recordingExecutor) Up(ctx context.Context, dsn string, src embed.FS) error {
	r.mu.Lock()
	r.applies = append(r.applies, applyCall{dsn: dsn, kind: sourceKind(src)})
	r.mu.Unlock()
	if err := r.upErr[dsn]; err != nil {
		return err
	}
	return nil
}

type recordingLock struct{ owner *recordingExecutor }

func (l *recordingLock) Release(ctx context.Context) error {
	l.owner.mu.Lock()
	l.owner.releases++
	l.owner.mu.Unlock()
	return nil
}

// TestRunMigrateOrdering：主库先于日志库；日志库 DSN 取自 DB_LOGS_DSN；
// 咨询锁取自主库 DSN 且在迁移结束后释放。
func TestRunMigrateOrdering(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL: "postgres://main/db?sslmode=disable",
		DBLogsDSN:   "postgres://logs/db?sslmode=disable",
	}
	exec := &recordingExecutor{}

	if err := runMigrate(context.Background(), cfg, exec); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}

	want := []applyCall{
		{dsn: cfg.DatabaseURL, kind: "main"},
		{dsn: cfg.DBLogsDSN, kind: "logs"},
	}
	if len(exec.applies) != len(want) {
		t.Fatalf("applies = %+v, want %d calls", exec.applies, len(want))
	}
	for i := range want {
		if exec.applies[i] != want[i] {
			t.Fatalf("applies[%d] = %+v, want %+v", i, exec.applies[i], want[i])
		}
	}
	if exec.lockDSN != cfg.DatabaseURL {
		t.Fatalf("advisory lock acquired on %q, want main DSN %q", exec.lockDSN, cfg.DatabaseURL)
	}
	if exec.releases != 1 {
		t.Fatalf("lock releases = %d, want 1", exec.releases)
	}
}

// TestRunMigrateRejectsEmptyLogsDSN：DB_LOGS_DSN 为空时 fail-fast，绝不静默跳过
// 日志库迁移（platform-03 §3.1 #22 必填；M0 控制器裁决）。取锁前返回——空 DSN
// 不是需要串行化的迁移，避免无意义的数据库往返。
func TestRunMigrateRejectsEmptyLogsDSN(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://main/db"}
	exec := &recordingExecutor{}

	err := runMigrate(context.Background(), cfg, exec)
	if err == nil {
		t.Fatal("runMigrate with empty DB_LOGS_DSN: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "DB_LOGS_DSN") {
		t.Fatalf("runMigrate error = %q, want it to mention DB_LOGS_DSN", err)
	}
	if len(exec.applies) != 0 {
		t.Fatalf("applies = %+v, want none", exec.applies)
	}
	if exec.lockDSN != "" || exec.releases != 0 {
		t.Fatalf("lock touched on empty DSN: lockDSN=%q releases=%d", exec.lockDSN, exec.releases)
	}
}

// TestRunMigrateStopsOnMainError：主库失败时不再迁移日志库，锁仍释放。
func TestRunMigrateStopsOnMainError(t *testing.T) {
	boom := errors.New("main migration exploded")
	cfg := &config.Config{
		DatabaseURL: "postgres://main/db",
		DBLogsDSN:   "postgres://logs/db",
	}
	exec := &recordingExecutor{upErr: map[string]error{cfg.DatabaseURL: boom}}

	err := runMigrate(context.Background(), cfg, exec)
	if !errors.Is(err, boom) {
		t.Fatalf("runMigrate error = %v, want wrapped %v", err, boom)
	}
	if len(exec.applies) != 1 {
		t.Fatalf("applies = %+v, want only main attempt", exec.applies)
	}
	if exec.releases != 1 {
		t.Fatalf("lock releases = %d, want 1", exec.releases)
	}
}

// blockingExecutor 让第一个 RunMigrate 在 Up 中持锁停留，第二个 RunMigrate
// 在 AcquireLock 上等到 ctx 结束；以此断言互斥（第二个不进入迁移函数）。
type blockingExecutor struct {
	mu      sync.Mutex
	held    bool
	upCalls atomic.Int32
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (e *blockingExecutor) AcquireLock(ctx context.Context, dsn string) (migrationLock, error) {
	e.mu.Lock()
	if e.held {
		e.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	e.held = true
	e.mu.Unlock()
	return &blockingLock{owner: e}, nil
}

func (e *blockingExecutor) Up(ctx context.Context, dsn string, src embed.FS) error {
	e.upCalls.Add(1)
	e.once.Do(func() { close(e.started) })
	<-e.unblock
	return nil
}

type blockingLock struct{ owner *blockingExecutor }

func (l *blockingLock) Release(ctx context.Context) error {
	l.owner.mu.Lock()
	l.owner.held = false
	l.owner.mu.Unlock()
	return nil
}

// TestRunMigrateLockConflict：第一个 goroutine 持锁期间，第二个 RunMigrate
// 阻塞在锁上（ctx 到期返回），绝不进入迁移函数；首个迁移完成后正常跑完两库。
func TestRunMigrateLockConflict(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL: "postgres://main/db",
		DBLogsDSN:   "postgres://logs/db",
	}
	exec := &blockingExecutor{
		started: make(chan struct{}),
		unblock: make(chan struct{}),
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- runMigrate(context.Background(), cfg, exec) }()

	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first RunMigrate did not enter migration in 2s")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	secondDone := make(chan error, 1)
	go func() { secondDone <- runMigrate(ctx2, cfg, exec) }()

	select {
	case err := <-secondDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second RunMigrate error = %v, want context deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second RunMigrate did not return after ctx deadline")
	}
	if got := exec.upCalls.Load(); got != 1 {
		t.Fatalf("second RunMigrate entered migration: Up calls = %d, want 1 (first main only)", got)
	}

	close(exec.unblock)
	if err := <-firstDone; err != nil {
		t.Fatalf("first RunMigrate: %v", err)
	}
	if got := exec.upCalls.Load(); got != 2 {
		t.Fatalf("first RunMigrate Up calls = %d, want 2 (main+logs)", got)
	}
}
