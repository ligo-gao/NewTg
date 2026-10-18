package billing

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件用 fake lister/recoverer 覆盖 worker 循环：启动即扫、周期重试、
// ctx 取消优雅退出、单请求失败隔离、分区维护接缝。不触真 DB/Redis。

type workerTestLister struct {
	mu      sync.Mutex
	calls   int
	results [][]string
	errs    []error
}

func (l *workerTestLister) ListAbandoned(context.Context, time.Time) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.calls
	l.calls++
	var err error
	if i < len(l.errs) {
		err = l.errs[i]
	}
	if err != nil {
		return nil, err
	}
	if i < len(l.results) {
		return l.results[i], nil
	}
	if len(l.results) > 0 {
		// 后续轮次重复最后一组，模拟「租约未被删除时会被反复扫到」。
		return l.results[len(l.results)-1], nil
	}
	return nil, nil
}

func (l *workerTestLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

type workerTestRecoverer struct {
	mu   sync.Mutex
	ids  []string
	errs map[string]error
}

func (r *workerTestRecoverer) RecoverAbandonedByID(_ context.Context, requestID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, requestID)
	if r.errs != nil {
		return r.errs[requestID]
	}
	return nil
}

func (r *workerTestRecoverer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

func (r *workerTestRecoverer) attempted(requestID string) bool {
	for _, id := range r.snapshot() {
		if id == requestID {
			return true
		}
	}
	return false
}

func workerTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func workerWaitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestWorkerLoopRecoversAbandonedAndStops 验证启动即扫一轮、周期扫描与
// ctx 取消后的优雅退出（返回 nil）。
func TestWorkerLoopRecoversAbandonedAndStops(t *testing.T) {
	lister := &workerTestLister{results: [][]string{{"req-1", "req-2"}}}
	recoverer := &workerTestRecoverer{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWorkerLoop(ctx, WorkerDeps{
			Abandoned:    lister,
			Recoverer:    recoverer,
			Logger:       workerTestLogger(),
			ScanInterval: 5 * time.Millisecond,
		})
	}()

	workerWaitFor(t, func() bool {
		return recoverer.attempted("req-1") && recoverer.attempted("req-2")
	}, "startup scan to recover both requests")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWorkerLoop = %v, want nil on ctx cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunWorkerLoop did not exit after cancel")
	}
}

// TestWorkerLoopScanFailureRetries 验证扫描失败不被当作「无悬挂」：
// 记日志、继续循环、下一轮重试。
func TestWorkerLoopScanFailureRetries(t *testing.T) {
	lister := &workerTestLister{
		results: [][]string{nil, {"req-retry"}},
		errs:    []error{errors.New("redis down"), nil},
	}
	recoverer := &workerTestRecoverer{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWorkerLoop(ctx, WorkerDeps{
			Abandoned:    lister,
			Recoverer:    recoverer,
			Logger:       workerTestLogger(),
			ScanInterval: 5 * time.Millisecond,
		})
	}()

	workerWaitFor(t, func() bool { return recoverer.attempted("req-retry") }, "retry scan to recover request")
	if got := lister.callCount(); got < 2 {
		t.Fatalf("lister calls = %d, want >= 2", got)
	}
	cancel()
	<-done
}

// TestWorkerLoopRecoverFailureIsolated 验证单个请求回收失败不打断同轮其它
// 请求，也不终止循环。
func TestWorkerLoopRecoverFailureIsolated(t *testing.T) {
	lister := &workerTestLister{results: [][]string{{"req-bad", "req-good"}}}
	recoverer := &workerTestRecoverer{errs: map[string]error{"req-bad": errors.New("db down")}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWorkerLoop(ctx, WorkerDeps{
			Abandoned:    lister,
			Recoverer:    recoverer,
			Logger:       workerTestLogger(),
			ScanInterval: 50 * time.Millisecond,
		})
	}()

	workerWaitFor(t, func() bool {
		return recoverer.attempted("req-bad") && recoverer.attempted("req-good")
	}, "both requests to be attempted")

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWorkerLoop = %v, want nil", err)
	}
}

// TestWorkerLoopPartitionMaintenance 验证分区维护在启动时执行一次，并按
// PartitionInterval 周期重试；失败只记录，不阻断扫描。
func TestWorkerLoopPartitionMaintenance(t *testing.T) {
	// sql.Open 不建立连接；fake 分区函数不会使用该句柄。
	sqlDB, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var partitionCalls atomic.Int64
	lister := &workerTestLister{}
	recoverer := &workerTestRecoverer{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWorkerLoop(ctx, WorkerDeps{
			Abandoned:         lister,
			Recoverer:         recoverer,
			LogsDB:            sqlDB,
			Logger:            workerTestLogger(),
			ScanInterval:      5 * time.Millisecond,
			PartitionInterval: 5 * time.Millisecond,
			EnsurePartitions: func(context.Context, *sql.DB, time.Time) error {
				partitionCalls.Add(1)
				return errors.New("simulated permission failure")
			},
		})
	}()

	workerWaitFor(t, func() bool { return partitionCalls.Load() >= 2 }, "periodic partition maintenance")
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWorkerLoop = %v, want nil even when partition maintenance fails", err)
	}
}

// TestWorkerLoopPartitionDDLWarn 验证 N4/R-56：分区维护返回
// *PartitionDDLUnavailableError（运行期身份无 DDL）时，worker 记 **WARN**
// 并给出「用 migrate 凭据扩窗」的 ops 信息，而不是 ERROR。
func TestWorkerLoopPartitionDDLWarn(t *testing.T) {
	sqlDB, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWorkerLoop(ctx, WorkerDeps{
			Abandoned: &workerTestLister{},
			Recoverer: &workerTestRecoverer{},
			LogsDB:    sqlDB,
			Logger:    logger,
			EnsurePartitions: func(context.Context, *sql.DB, time.Time) error {
				// 让启动序的 ensure 在返回前请求停机，保证断言读取时日志已写完。
				cancel()
				return &PartitionDDLUnavailableError{Schema: "public"}
			},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWorkerLoop = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunWorkerLoop did not exit")
	}

	out := logs.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("缺 WARN 日志: %s", out)
	}
	if !strings.Contains(out, "partition DDL unavailable") {
		t.Fatalf("缺 ops 信息: %s", out)
	}
	if !strings.Contains(out, "migrate credentials") {
		t.Fatalf("缺扩窗指引: %s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("DDL 权限缺失不得记 ERROR: %s", out)
	}
}

// TestWorkerLoopRequiresDeps 验证装配错误在启动时显式暴露。
func TestWorkerLoopRequiresDeps(t *testing.T) {
	if err := RunWorkerLoop(context.Background(), WorkerDeps{}); err == nil {
		t.Fatal("RunWorkerLoop without deps = nil, want error")
	}
	if err := RunWorkerLoop(context.Background(), WorkerDeps{
		Abandoned: &workerTestLister{},
	}); err == nil {
		t.Fatal("RunWorkerLoop without recoverer = nil, want error")
	}
}

// TestWorkerLoopCanceledContext 验证已取消的上下文不启动任何扫描。
func TestWorkerLoopCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lister := &workerTestLister{results: [][]string{{"req-never"}}}
	if err := RunWorkerLoop(ctx, WorkerDeps{
		Abandoned: lister,
		Recoverer: &workerTestRecoverer{},
		Logger:    workerTestLogger(),
	}); err != nil {
		t.Fatalf("RunWorkerLoop = %v, want nil", err)
	}
	if got := lister.callCount(); got != 0 {
		t.Fatalf("lister calls = %d, want 0", got)
	}
}
