package modelperf

import (
	"math"
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
	// nearest-rank：3 个样本时 ceil(0.95*3)=3，p95 取最大值 300；若实现改成
	// 线性插值会得到 290，分位数口径就与手工测速漂移了。
	if test.TTFTMSP95 == nil || *test.TTFTMSP95 != 300 {
		t.Fatalf("ttft p95 = %v, want 300", test.TTFTMSP95)
	}
	if test.LatencyMSP50 == nil || *test.LatencyMSP50 != 1000 {
		t.Fatalf("latency p50 = %v, want 1000", test.LatencyMSP50)
	}
	if test.LatencyMSP95 == nil || *test.LatencyMSP95 != 1100 {
		t.Fatalf("latency p95 = %v, want 1100", test.LatencyMSP95)
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

// TestAggregateSkipsSuccessfulSamplesWithNilMetrics 钉住「成功但字段为 nil」的
// 样本只计入成功率，不进入分位数与均值。
//
// 为什么重要：真实上游可能只返回部分指标（例如非流式响应没有 TTFT）。若把
// nil 当成 0 参与聚合，首页性能表会凭空出现 0ms 的 p50/均值，把「没采到」伪装
// 成「极快」；若不判 nil 直接解引用，则会 panic 掉整轮探测。
func TestAggregateSkipsSuccessfulSamplesWithNilMetrics(t *testing.T) {
	test, runs := Aggregate(&store.Channel{ID: 2, Type: "openai"}, "m", 2,
		[]channelhealth.PerfRunResult{
			{Success: true, TTFTMS: intPtr(100), LatencyMS: intPtr(900), OutputTokens: intPtr(40), TPS: floatPtr(20)},
			{Success: true}, // 成功但没有任何指标：应被跳过而不是按 0 记账
		}, nil)

	// 成功率的分母是请求轮数，与指标是否存在无关：两次都成功就是 1.0。
	if test.SuccessCount != 2 || test.SuccessRate != 1 {
		t.Fatalf("success = %d/%v, want 2/1", test.SuccessCount, test.SuccessRate)
	}
	if test.TTFTMSP50 == nil || *test.TTFTMSP50 != 100 ||
		test.TTFTMSP95 == nil || *test.TTFTMSP95 != 100 {
		t.Fatalf("ttft p50/p95 = %v/%v, want 100/100 (nil samples must not become 0)",
			test.TTFTMSP50, test.TTFTMSP95)
	}
	if test.LatencyMSP50 == nil || *test.LatencyMSP50 != 900 ||
		test.LatencyMSP95 == nil || *test.LatencyMSP95 != 900 {
		t.Fatalf("latency p50/p95 = %v/%v, want 900/900", test.LatencyMSP50, test.LatencyMSP95)
	}
	if test.TPSAvg == nil || *test.TPSAvg != 20 {
		t.Fatalf("tps avg = %v, want 20 (nil samples must not drag the mean to 0)", test.TPSAvg)
	}
	if test.OutputTokensAvg == nil || *test.OutputTokensAvg != 40 {
		t.Fatalf("tokens avg = %v, want 40", test.OutputTokensAvg)
	}
	// 明细行仍保留两条：nil 指标只是不参与汇总，不能把这次运行从历史里丢掉。
	if len(runs) != 2 || !runs[1].Success {
		t.Fatalf("run rows = %+v, want both successful runs projected", runs)
	}
}

// TestAggregateZeroRunsKeepsSuccessRateAtZero 钉住 runs == 0 的保护。
//
// 为什么重要：SuccessRate 直接写进汇总行与 API。没有 runs > 0 保护时，
// 0 个请求会算出 NaN（0/0）或 +Inf（有成功样本却 runs=0）；NaN/Inf 既不能
// 写进数据库数值列，也会让 JSON 序列化失败。保护行为是成功率先落 0。
func TestAggregateZeroRunsKeepsSuccessRateAtZero(t *testing.T) {
	cases := []struct {
		name    string
		results []channelhealth.PerfRunResult
	}{
		{"no results", nil},
		{"successes without requested runs", []channelhealth.PerfRunResult{{Success: true, TTFTMS: intPtr(100)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			test, _ := Aggregate(&store.Channel{ID: 3}, "m", 0, tc.results, nil)
			if math.IsNaN(test.SuccessRate) || math.IsInf(test.SuccessRate, 0) {
				t.Fatalf("success rate = %v, want a finite 0 (runs > 0 guard missing)", test.SuccessRate)
			}
			if test.SuccessRate != 0 {
				t.Fatalf("success rate = %v, want 0 when runs == 0", test.SuccessRate)
			}
		})
	}
}

func TestAggregateAllFailedLeavesMetricsNil(t *testing.T) {
	test, _ := Aggregate(&store.Channel{ID: 1, Type: "openai"}, "m", 3,
		[]channelhealth.PerfRunResult{{Success: false}, {Success: false}, {Success: false}}, int64Ptr(42))
	// 无成功样本时所有指标都必须是 nil：0 是「测到了 0ms」的有效测量，nil 才是
	// 「没有样本」。前端据此显示「—」，把 nil 退化成 0 会让全失败模型看起来极快。
	if test.SuccessRate != 0 || test.TTFTMSP50 != nil || test.TTFTMSP95 != nil ||
		test.LatencyMSP50 != nil || test.LatencyMSP95 != nil ||
		test.TPSAvg != nil || test.OutputTokensAvg != nil {
		t.Fatalf("all-failed aggregate must be rate 0 + all metrics nil: %+v", test)
	}
	if test.CreatedBy == nil || *test.CreatedBy != 42 {
		t.Fatalf("created_by not propagated: %v", test.CreatedBy)
	}
}

func int64Ptr(v int64) *int64 { return &v }
