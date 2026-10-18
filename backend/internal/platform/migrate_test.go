package platform

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/migrations"
	migrationslogs "newtg/gateway/migrations-logs"
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

// TestExpectedSchemaVersionEmbedded 验证公共签名对真实嵌入目录的版本解析：
// M0 两个迁移流各有一个 14 位时间戳 init up。
func TestExpectedSchemaVersionEmbedded(t *testing.T) {
	mainVer, err := ExpectedSchemaVersion(migrations.FS)
	if err != nil {
		t.Fatalf("ExpectedSchemaVersion(main): %v", err)
	}
	if mainVer != 20261018053301 {
		t.Fatalf("main expected version = %d, want 20261018053301", mainVer)
	}
	logsVer, err := ExpectedSchemaVersion(migrationslogs.FS)
	if err != nil {
		t.Fatalf("ExpectedSchemaVersion(logs): %v", err)
	}
	if logsVer != 20261018061805 {
		t.Fatalf("logs expected version = %d, want 20261018061805", logsVer)
	}
}

// TestMigrateAdvisoryLockKey 冻结咨询锁键（跨进程串行化的契约值）。
func TestMigrateAdvisoryLockKey(t *testing.T) {
	if migrateAdvisoryLockKey != 727272 {
		t.Fatalf("migrateAdvisoryLockKey = %d, want 727272", migrateAdvisoryLockKey)
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

// TestRunMigrateSkipsUnsetLogsDSN：DB_LOGS_DSN 为空时（config 允许）只迁移主库。
func TestRunMigrateSkipsUnsetLogsDSN(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://main/db"}
	exec := &recordingExecutor{}

	if err := runMigrate(context.Background(), cfg, exec); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if len(exec.applies) != 1 || exec.applies[0].kind != "main" {
		t.Fatalf("applies = %+v, want main only", exec.applies)
	}
	if exec.releases != 1 {
		t.Fatalf("lock releases = %d, want 1", exec.releases)
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
