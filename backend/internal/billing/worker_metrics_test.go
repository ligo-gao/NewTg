package billing

// W10-METRICS 接线级用例（I-3）：worker 兜底扫描的 task/abandoned 指标。
// 不依赖 DB/Redis：scanOnce 的 lister/recoverer 都是接口。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
)

type w10MetricsLister struct {
	entries []AbandonedRequest
	err     error
}

func (l w10MetricsLister) ListAbandoned(context.Context, time.Time) ([]AbandonedRequest, error) {
	return l.entries, l.err
}

type w10MetricsRecoverer struct{ calls int }

func (r *w10MetricsRecoverer) RecoverAbandonedByID(context.Context, string, int64) error {
	r.calls++
	return nil
}

func w10MetricsGauge(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.Metric {
			match := true
			for wantName, wantValue := range labels {
				found := false
				for _, pair := range m.Label {
					if pair.GetName() == wantName {
						found = pair.GetValue() == wantValue
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match && m.GetGauge() != nil {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func w10MetricsCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		total := 0.0
		for _, m := range fam.Metric {
			match := true
			for wantName, wantValue := range labels {
				found := false
				for _, pair := range m.Label {
					if pair.GetName() == wantName {
						found = pair.GetValue() == wantValue
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				total += m.GetCounter().GetValue()
			}
		}
		return total
	}
	return 0
}

func TestWorkerAbandonedMetricsWiring(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Unix(1_700_000_000, 0)

	runLabels := map[string]string{"task_type": "preconsume_reconcile", "status": "succeeded"}
	failLabels := map[string]string{"task_type": "preconsume_reconcile", "status": "failed"}
	refundLabels := map[string]string{"source": "wallet"}
	runsBefore := w10MetricsCounter(t, "newtg_worker_task_runs_total", runLabels)
	failsBefore := w10MetricsCounter(t, "newtg_worker_task_runs_total", failLabels)
	refundsBefore := w10MetricsCounter(t, "newtg_worker_abandoned_refund_total", refundLabels)

	recoverer := &w10MetricsRecoverer{}
	scanOnce(context.Background(), w10MetricsLister{
		entries: []AbandonedRequest{{RequestID: "w10m-abandoned", UserID: 1}},
	}, recoverer, log, func() time.Time { return now })

	if recoverer.calls != 1 {
		t.Fatalf("recover calls = %d, want 1", recoverer.calls)
	}
	if delta := w10MetricsCounter(t, "newtg_worker_task_runs_total", runLabels) - runsBefore; delta != 1 {
		t.Fatalf("task run delta = %v, want 1", delta)
	}
	if delta := w10MetricsCounter(t, "newtg_worker_abandoned_refund_total", refundLabels) - refundsBefore; delta != 1 {
		t.Fatalf("abandoned refund delta = %v, want 1", delta)
	}
	age, ok := w10MetricsGauge(t, "newtg_worker_abandoned_oldest_age_seconds", nil)
	if !ok || age < float64(contract.AbandonedWindowSeconds) {
		t.Fatalf("abandoned oldest age = %v (ok=%v), want >= %d", age, ok, contract.AbandonedWindowSeconds)
	}

	// 扫描失败：只记 failed，不得把失败当成功。
	scanOnce(context.Background(), w10MetricsLister{err: errors.New("redis down")},
		recoverer, log, func() time.Time { return now })
	if delta := w10MetricsCounter(t, "newtg_worker_task_runs_total", failLabels) - failsBefore; delta != 1 {
		t.Fatalf("failed task run delta = %v, want 1", delta)
	}
}
