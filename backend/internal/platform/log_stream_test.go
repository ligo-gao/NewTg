package platform

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/configcenter"
)

// captureLogHandler 记录所有 slog 记录，供「没有启动消费者」的断言使用。
type captureLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureLogHandler) WithGroup(string) slog.Handler { return h }

func (h *captureLogHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// TestStartUsageLogStreamWorkerDisabledDoesNotStart 验证默认关闭时：
//   - 不启动消费 goroutine（用 nil Redis/日志库也不会触发装配错误日志）；
//   - 返回可安全调用的停止函数。
func TestStartUsageLogStreamWorkerDisabledDoesNotStart(t *testing.T) {
	// 空配置中心的快照 → 未配置键 → 默认 false。
	center := configcenter.New(nil, nil)
	if usageLogBatchEnabled(center, nil) {
		t.Fatal("usageLogBatchEnabled = true for an empty config center, want false")
	}

	handler := &captureLogHandler{}
	stop := startUsageLogStreamWorker(context.Background(), center, nil, nil, nil, "", "", slog.New(handler))
	if stop == nil {
		t.Fatal("stop function is nil")
	}
	stop()

	// 若误启动消费者，nil Redis 会让 newUsageLogConsumer 返回错误并记录 Error。
	time.Sleep(50 * time.Millisecond)
	for _, record := range handler.snapshot() {
		if record.Level >= slog.LevelError {
			t.Fatalf("disabled worker logged an error (consumer started?): %s", record.Message)
		}
	}
}
