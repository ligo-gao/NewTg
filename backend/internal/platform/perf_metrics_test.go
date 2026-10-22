package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/internal/perfmetrics"
)

// G-M6C-13：platform 必须消费 PERF_METRICS_ENABLED / PERF_METRICS_DSN，并在
// 指标库缺失/不可达时 fail-closed（不构造采集器、不回退日志库）。
func TestNewPerfMetricsAssemblyHonorsEnabledFlag(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("disabled", func(t *testing.T) {
		a, err := newPerfMetricsAssembly(context.Background(), &config.Config{PerfMetricsEnabled: false}, logger)
		if err != nil {
			t.Fatalf("disabled assembly: %v", err)
		}
		if a.collector != nil || a.source != nil || a.stop != nil {
			t.Fatalf("PERF_METRICS_ENABLED=false must be a zero assembly: %+v", a)
		}
	})

	t.Run("enabled without dsn", func(t *testing.T) {
		a, err := newPerfMetricsAssembly(context.Background(), &config.Config{
			PerfMetricsEnabled:       true,
			PerfMetricsFlushInterval: time.Minute,
		}, logger)
		if err != nil {
			t.Fatalf("enabled assembly: %v", err)
		}
		if a.collector != nil {
			t.Fatalf("missing DSN must not construct a collector")
		}
		if a.source == nil {
			t.Fatalf("missing DSN must expose an unavailable source")
		}
		_, _, queryErr := a.source.ModelPerfBuckets(context.Background(), time.Now(), time.Now(), "1m", "")
		var unavailable *perfmetrics.UnavailableError
		if !errors.As(queryErr, &unavailable) || unavailable.Reason != perfmetrics.ReasonDSNMissing {
			t.Fatalf("source error = %v, want reason %q", queryErr, perfmetrics.ReasonDSNMissing)
		}
	})

	t.Run("enabled with unreachable dsn", func(t *testing.T) {
		a, err := newPerfMetricsAssembly(context.Background(), &config.Config{
			PerfMetricsEnabled:       true,
			PerfMetricsFlushInterval: time.Minute,
			PerfMetricsDSN:           "postgres://newtg:super-secret@127.0.0.1:1/newtg_metrics?sslmode=disable&connect_timeout=1",
		}, logger)
		if err != nil {
			t.Fatalf("unreachable assembly must not fail startup: %v", err)
		}
		if a.collector != nil {
			t.Fatalf("unreachable metrics DB must not construct a collector")
		}
		if a.source == nil {
			t.Fatalf("unreachable metrics DB must expose an unavailable source")
		}
		_, _, queryErr := a.source.ModelPerfBuckets(context.Background(), time.Now(), time.Now(), "1m", "")
		var unavailable *perfmetrics.UnavailableError
		if !errors.As(queryErr, &unavailable) || unavailable.Reason != perfmetrics.ReasonStorageUnavailable {
			t.Fatalf("source error = %v, want reason %q", queryErr, perfmetrics.ReasonStorageUnavailable)
		}
		// 脱敏：不可用原因/错误不得回显 DSN 或口令。
		if strings.Contains(queryErr.Error(), "super-secret") {
			t.Fatalf("error leaks DSN secret: %v", queryErr)
		}
	})
}
