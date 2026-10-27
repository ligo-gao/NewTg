package modelperf

import (
	"testing"

	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

func intPtr(v int) *int           { return &v }
func floatPtr(v float64) *float64 { return &v }

func TestAggregateUsesOnlySuccessfulSamples(t *testing.T) {
	ch := &store.Channel{ID: 7, Type: "openai"}
	results := []channelhealth.PerfRunResult{
		{Success: true, TTFTMS: intPtr(100), LatencyMS: intPtr(900), OutputTokens: intPtr(60), TPS: floatPtr(30)},
		{Success: true, TTFTMS: intPtr(200), LatencyMS: intPtr(1000), OutputTokens: intPtr(63), TPS: floatPtr(50)},
		{Success: true, TTFTMS: intPtr(300), LatencyMS: intPtr(1100), OutputTokens: intPtr(66), TPS: floatPtr(70)},
		{Success: false, TTFTMS: intPtr(9999), ErrorClass: "timeout"},
	}
	test, runs := Aggregate(ch, "gpt-4o", 4, results, nil)

	if test.ChannelID != 7 || test.Model != "gpt-4o" || test.Provider != "openai" || test.Runs != 4 {
		t.Fatalf("identity fields wrong: %+v", test)
	}
	if test.SuccessCount != 3 || test.SuccessRate != 0.75 {
		t.Fatalf("success = %d/%v, want 3/0.75", test.SuccessCount, test.SuccessRate)
	}
	if test.TTFTMSP50 == nil || *test.TTFTMSP50 != 200 {
		t.Fatalf("ttft p50 = %v, want 200 (failed run must not enter samples)", test.TTFTMSP50)
	}
	if test.LatencyMSP50 == nil || *test.LatencyMSP50 != 1000 {
		t.Fatalf("latency p50 = %v, want 1000", test.LatencyMSP50)
	}
	if test.TPSAvg == nil || *test.TPSAvg != 50 {
		t.Fatalf("tps avg = %v, want 50", test.TPSAvg)
	}
	if test.OutputTokensAvg == nil || *test.OutputTokensAvg != 63 {
		t.Fatalf("tokens avg = %v, want 63", test.OutputTokensAvg)
	}
	if test.CreatedBy != nil {
		t.Fatalf("created_by = %v, want nil for scheduled runs", test.CreatedBy)
	}
	if len(runs) != 4 || runs[3].Success || runs[3].ErrorClass != "timeout" {
		t.Fatalf("run rows not projected: %+v", runs)
	}
}

func TestAggregateAllFailedLeavesMetricsNil(t *testing.T) {
	test, _ := Aggregate(&store.Channel{ID: 1, Type: "openai"}, "m", 3,
		[]channelhealth.PerfRunResult{{Success: false}, {Success: false}, {Success: false}}, int64Ptr(42))
	if test.SuccessRate != 0 || test.TTFTMSP50 != nil || test.TPSAvg != nil {
		t.Fatalf("all-failed aggregate must be rate 0 + nil metrics: %+v", test)
	}
	if test.CreatedBy == nil || *test.CreatedBy != 42 {
		t.Fatalf("created_by not propagated: %v", test.CreatedBy)
	}
}

func int64Ptr(v int64) *int64 { return &v }
