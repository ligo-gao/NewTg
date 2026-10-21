package channelhealth

// W10-METRICS 接线级用例（I-3）：自动禁用副作用链成功迁移后，
// newtg_channel_auto_disabled_total 必须增长（channel_type/reason 有界）。

import (
	"context"
	"testing"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
	"newtg/gateway/internal/store"
)

func autoDisabledCounter(t *testing.T, channelType string) float64 {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != "newtg_channel_auto_disabled_total" {
			continue
		}
		total := 0.0
		for _, m := range fam.Metric {
			for _, pair := range m.Label {
				if pair.GetName() == "channel_type" && pair.GetValue() == channelType {
					total += m.GetCounter().GetValue()
				}
			}
		}
		return total
	}
	return 0
}

func TestChannelAutoDisabledMetricWiring(t *testing.T) {
	channelType := "w10metrics-type"
	channel := &store.Channel{ID: 7, Name: "c7", Type: channelType, Status: 3}
	storeImpl := &fakeAutoDisableStore{channel: channel, changed: true}
	reporter := testReporter(t, storeImpl, &fakeAudit{}, &fakeNotifier{}, ReporterConfig{})

	before := autoDisabledCounter(t, channelType)
	reporter.ReportAttemptFailure(context.Background(), 7, true,
		contract.ErrUpstreamAuth, 401, []byte(`{"error":"invalid api key"}`))
	if processed := reporter.RunOnce(context.Background()); processed != 1 {
		t.Fatalf("RunOnce processed = %d, want 1", processed)
	}
	if delta := autoDisabledCounter(t, channelType) - before; delta != 1 {
		t.Fatalf("auto_disabled delta = %v, want 1", delta)
	}
}
