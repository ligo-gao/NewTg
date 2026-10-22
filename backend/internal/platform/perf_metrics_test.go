package platform

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"newtg/gateway/internal/config"
)

// G-M6C-13：platform 必须消费 PERF_METRICS_ENABLED / PERF_METRICS_FLUSH_INTERVAL。
// 禁用时不得构造采集器（零开销）；启用时装配非 nil 的 Source。
func TestNewPerfMetricsCollectorHonorsEnabledFlag(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("disabled", func(t *testing.T) {
		c, err := newPerfMetricsCollector(&config.Config{PerfMetricsEnabled: false}, nil, logger)
		if err != nil {
			t.Fatalf("disabled collector: %v", err)
		}
		if c != nil {
			t.Fatalf("PERF_METRICS_ENABLED=false must not construct a collector")
		}
	})

	t.Run("enabled", func(t *testing.T) {
		cfg := &config.Config{
			PerfMetricsEnabled:       true,
			PerfMetricsFlushInterval: 3 * time.Minute,
			NodeName:                 "node-test",
		}
		c, err := newPerfMetricsCollector(cfg, nil, logger)
		if err != nil {
			t.Fatalf("enabled collector: %v", err)
		}
		if c == nil {
			t.Fatalf("PERF_METRICS_ENABLED=true must construct a collector")
		}
		if c.LastSuccessAt().IsZero() {
			t.Fatalf("collector must record construction heartbeat")
		}
	})

	t.Run("nil config", func(t *testing.T) {
		c, err := newPerfMetricsCollector(nil, nil, logger)
		if err != nil || c != nil {
			t.Fatalf("nil config = (%v,%v), want (nil,nil)", c, err)
		}
	})
}
