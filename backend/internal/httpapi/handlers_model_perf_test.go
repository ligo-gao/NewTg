package httpapi

import (
	"errors"
	"testing"
	"time"

	"newtg/gateway/internal/perfmetrics"
)

// F-05-08 分位、成功率、事故派生与徽章的纯函数用例（不依赖数据库）。
// 这些是看板口径的核心：桶内线性插值、+Inf 下界标记、refused 分母语义、
// 无流量空值语义。

func TestModelPerfQuantileInterpolation(t *testing.T) {
	var buckets [modelPerfHistogramBuckets]int64
	buckets[1] = 100 // 全部落在 (50,100] ms
	p50, over, ok := quantileFromBuckets(100, buckets, 0.50)
	if !ok || over || p50 != 75 {
		t.Fatalf("p50 = (%v,%v,%v), want (75,false,true)", p50, over, ok)
	}
	p95, over, ok := quantileFromBuckets(100, buckets, 0.95)
	if !ok || over || p95 != 97.5 {
		t.Fatalf("p95 = (%v,%v,%v), want (97.5,false,true)", p95, over, ok)
	}

	// 跨桶：50 个在首桶、50 个在第二桶。p50 恰好落在桶边界。
	var cross [modelPerfHistogramBuckets]int64
	cross[0], cross[1] = 50, 50
	p50, _, _ = quantileFromBuckets(100, cross, 0.50)
	if p50 != 50 {
		t.Fatalf("cross-bucket p50 = %v, want 50 at boundary", p50)
	}

	// +Inf：下界标记，不伪造有限值；P50 同样必须带标记。3 个样本中只有
	// 1 个落在最后的有限桶，P50/P95 都进入 +Inf。
	var inf [modelPerfHistogramBuckets]int64
	inf[8] = 1
	for _, q := range []float64{0.50, 0.95} {
		value, over, ok := quantileFromBuckets(3, inf, q)
		if !ok || !over || value != modelPerfInfinityMS {
			t.Fatalf("+Inf quantile q=%v = (%v,%v,%v), want (30000,true,true)", q, value, over, ok)
		}
	}

	// 无样本。
	if _, _, ok := quantileFromBuckets(0, [modelPerfHistogramBuckets]int64{}, 0.5); ok {
		t.Fatalf("no samples must report ok=false")
	}
}

func TestModelPerfAggregateEmptySemantics(t *testing.T) {
	var agg modelPerfAggregate
	summary := agg.summary(0)
	if summary.TTFTp50 != nil || summary.TTFTp95 != nil || summary.LatencyP50 != nil ||
		summary.LatencyP95 != nil || summary.SuccessRate != nil || summary.Uptime != nil ||
		summary.Incidents != nil || summary.TPS != nil || summary.SampleSize != nil {
		t.Fatalf("empty aggregate must be null-valued, got %+v", summary)
	}
	if summary.Refused != 0 {
		t.Fatalf("empty refused = %d, want 0", summary.Refused)
	}
}

func TestModelPerfSuccessRateCountsRefusedInDenominator(t *testing.T) {
	agg := modelPerfAggregate{Success: 8, Error: 1, Refused: 1}
	rate := agg.successRate()
	if rate == nil || *rate != 0.8 {
		t.Fatalf("success rate = %v, want 0.8 (refused counts in denominator)", rate)
	}
	summary := agg.summary(0)
	if summary.Refused != 1 {
		t.Fatalf("refused = %d, want 1", summary.Refused)
	}
	if summary.Incidents == nil || *summary.Incidents != 0 {
		t.Fatalf("traffic without incidents must be 0, got %v", summary.Incidents)
	}
}

func TestModelPerfTPS(t *testing.T) {
	agg := modelPerfAggregate{Completion: 1000, Generation: 20}
	tps := agg.tps()
	if tps == nil || *tps != 50 {
		t.Fatalf("tps = %v, want 50", tps)
	}
	if value := (&modelPerfAggregate{Completion: 10, Generation: 0}).tps(); value != nil {
		t.Fatalf("zero generation must be null, got %v", *value)
	}
}

func TestDeriveIncidents(t *testing.T) {
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	bad := func(offset int) ModelPerfBucket {
		return ModelPerfBucket{
			Start:           start.Add(time.Duration(offset) * time.Minute),
			Error:           10,
			ObservedSeconds: 60,
			UpSeconds:       0,
		}
	}
	good := func(offset int) ModelPerfBucket {
		return ModelPerfBucket{
			Start:           start.Add(time.Duration(offset) * time.Minute),
			Success:         10,
			ObservedSeconds: 60,
			UpSeconds:       60,
		}
	}

	// 连续 6 个坏分钟 → 一个 6 分钟事故；前后好分钟不应合并。
	buckets := []ModelPerfBucket{good(0), bad(1), bad(2), bad(3), bad(4), bad(5), bad(6), good(7)}
	incidents := deriveIncidents(buckets, "1m")
	if len(incidents) != 1 {
		t.Fatalf("incidents = %d, want 1 (%+v)", len(incidents), incidents)
	}
	if incidents[0].DurationSeconds != 360 {
		t.Fatalf("incident duration = %ds, want 360", incidents[0].DurationSeconds)
	}
	if incidents[0].Severity != "major" {
		t.Fatalf("severity = %s, want major", incidents[0].Severity)
	}

	// 两段坏分钟之间只隔 2 个好分钟（<5 分钟）→ 合并为一段。
	buckets = []ModelPerfBucket{bad(0), bad(1), bad(2), good(3), good(4), bad(5), bad(6), bad(7)}
	merged := deriveIncidents(buckets, "1m")
	if len(merged) != 1 {
		t.Fatalf("merged incidents = %d, want 1 (%+v)", len(merged), merged)
	}
	if merged[0].DurationSeconds != 480 {
		t.Fatalf("merged duration = %ds, want 480", merged[0].DurationSeconds)
	}

	// 仅 3 个坏分钟（<5 分钟最短时长）→ 不构成事故。
	short := deriveIncidents([]ModelPerfBucket{bad(0), bad(1), bad(2)}, "1m")
	if len(short) != 0 {
		t.Fatalf("short outage incidents = %d, want 0 (%+v)", len(short), short)
	}

	// 无流量分钟不计入分母；但 1 分钟的数据缺口 < 合并窗口，按规范与
	// 前后两段合并为同一事故（7 分钟）。
	gap := []ModelPerfBucket{bad(0), bad(1), bad(2), bad(3), bad(4), {Start: start.Add(5 * time.Minute)}, bad(6)}
	if got := deriveIncidents(gap, "1m"); len(got) != 1 || got[0].DurationSeconds != 420 {
		t.Fatalf("gap handling incidents = %+v, want one 420s incident", got)
	}
}

func TestModelPerfSummaryOver30sFlags(t *testing.T) {
	// 样本全部落在 +Inf：P50/P95 都必须带 over_30s 标记，不能只标记 P95。
	agg := modelPerfAggregate{TTFTCount: 10, LatencyCount: 10}
	summary := agg.summary(0)
	if !summary.TTFTP50Over30s || !summary.TTFTP95Over30s || !summary.LatP50Over30s || !summary.LatP95Over30s {
		t.Fatalf("+Inf flags = %+v, want all four over_30s", summary)
	}
}

func TestModelPerfBadge(t *testing.T) {
	sample := int64(100)
	rate := func(value float64) *float64 { return &value }
	cases := []struct {
		name    string
		summary modelPerfSummary
		ongoing bool
		want    string
	}{
		{"no sample", modelPerfSummary{}, false, "no_data"},
		{"low sample", modelPerfSummary{SampleSize: int64Ptr(19), SuccessRate: rate(1)}, false, "no_data"},
		{"incident", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(0.8)}, false, "incident"},
		{"ongoing incident", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), Uptime: rate(1)}, true, "incident"},
		{"degraded success", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(0.95)}, false, "degraded"},
		{"degraded uptime lower bound", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), Uptime: rate(0.99)}, false, "degraded"},
		{"degraded uptime mid", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), Uptime: rate(0.9949)}, false, "degraded"},
		{"healthy uptime boundary", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), Uptime: rate(0.995)}, false, "healthy"},
		{"slow", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), TTFTp95: rate(3500), Uptime: rate(1)}, false, "slow"},
		{"fast", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), TTFTp95: rate(800), Uptime: rate(1)}, false, "fast"},
		{"healthy", modelPerfSummary{SampleSize: &sample, SuccessRate: rate(1), Uptime: rate(1)}, false, "healthy"},
	}
	for _, tc := range cases {
		if got := modelPerfBadge(tc.summary, tc.ongoing); got != tc.want {
			t.Fatalf("%s badge = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestModelPerfUnavailableReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dsn missing", &perfmetrics.UnavailableError{Reason: perfmetrics.ReasonDSNMissing}, perfmetrics.ReasonDSNMissing},
		{"storage unavailable", &perfmetrics.UnavailableError{Reason: perfmetrics.ReasonStorageUnavailable}, perfmetrics.ReasonStorageUnavailable},
		{"empty reason", &perfmetrics.UnavailableError{}, perfmetrics.ReasonStorageUnavailable},
		{"generic query error", errors.New("boom: postgres://user:secret@host/db"), perfmetrics.ReasonQueryFailed},
	}
	for _, tc := range cases {
		if got := modelPerfUnavailableReason(tc.err); got != tc.want {
			t.Fatalf("%s reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}
