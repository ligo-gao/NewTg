package billing

import (
	"database/sql"
	"testing"
	"time"

	"newtg/gateway/internal/perfmetrics"
)

type recordingPerf struct {
	observations []perfmetrics.Observation
}

func (r *recordingPerf) Observe(obs perfmetrics.Observation) {
	r.observations = append(r.observations, obs)
}

// G-M6C-13：Saturate 的日志投影必须完整转成性能采样，且 refunded 不进入
// 采样；未装配采集器时是 no-op。
func TestObservePerfProjectsUsageLogRow(t *testing.T) {
	now := time.Date(2026, 10, 22, 10, 0, 30, 0, time.UTC)
	recorder := &recordingPerf{}
	logger := NewDBLogger(nil).WithPerfMetrics(recorder)
	logger.now = func() time.Time { return now }

	row := usageLogRow{
		model:            "gpt-test",
		stream:           true,
		status:           UsageLogStatusSuccess,
		latencyMS:        sql.NullInt64{Int64: 120, Valid: true},
		firstTokenMS:     sql.NullInt64{Int64: 30, Valid: true},
		completionTokens: 50,
	}
	logger.observePerf(row)

	if len(recorder.observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(recorder.observations))
	}
	got := recorder.observations[0]
	if got.Model != "gpt-test" || got.Status != perfmetrics.StatusSuccess || !got.Stream {
		t.Fatalf("observation identity = %+v", got)
	}
	if got.LatencyMS == nil || *got.LatencyMS != 120 {
		t.Fatalf("latency = %v, want 120", got.LatencyMS)
	}
	if got.FirstTokenMS == nil || *got.FirstTokenMS != 30 {
		t.Fatalf("ttft = %v, want 30", got.FirstTokenMS)
	}
	if got.CompletionTokens != 50 || !got.ObservedAt.Equal(now) {
		t.Fatalf("observation = %+v, want completion=50 observedAt=%v", got, now)
	}
}

func TestObservePerfSkipsRefundedAndMissingLatency(t *testing.T) {
	recorder := &recordingPerf{}
	logger := NewDBLogger(nil).WithPerfMetrics(recorder)

	// refunded 是结算后的异步事实，不属于完成时三态。
	logger.observePerf(usageLogRow{model: "m", status: UsageLogStatusRefunded})
	if len(recorder.observations) != 0 {
		t.Fatalf("refunded must not be sampled, got %+v", recorder.observations)
	}

	// 延迟缺失时仍计请求数，但 LatencyMS 保持 nil（不写 0 冒充 0ms）。
	logger.observePerf(usageLogRow{model: "m", status: UsageLogStatusError})
	if len(recorder.observations) != 1 {
		t.Fatalf("missing latency observation = %d, want 1", len(recorder.observations))
	}
	if recorder.observations[0].LatencyMS != nil {
		t.Fatalf("missing latency must stay nil, got %v", *recorder.observations[0].LatencyMS)
	}
}

func TestObservePerfWithoutRecorderIsNoop(t *testing.T) {
	logger := NewDBLogger(nil)
	// 不得 panic。
	logger.observePerf(usageLogRow{model: "m", status: UsageLogStatusSuccess})
}
