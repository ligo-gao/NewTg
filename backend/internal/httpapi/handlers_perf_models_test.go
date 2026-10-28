package httpapi

// 本文件覆盖 GET /api/performance/models 的形状、合并与状态判定（spec §7.3）。
//
// 三个数据源都用内存 fake：指标库（Source）、最新测速行（ModelPerformanceStore）、
// 渠道目录（ChannelModelCatalog）。端点只读，测试不连任何数据库；真实 SQL
// 形状由 store 包的集成用例覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/perfmetrics"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// modelsTestNow 是 handler 时钟：12:30 让「当前小时 12:00」与更早的整点
// 都有确定位置，便于断言 spark 的 24 个点。
var modelsTestNow = time.Date(2026, 10, 28, 12, 30, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeModelPerfSource 按粒度返回预置桶；两次查询的窗口参数不参与断言。
type fakeModelPerfSource struct {
	minute    []perfmetrics.Bucket
	hourly    []perfmetrics.Bucket
	errMinute error
	errHourly error
	// calls 记录实际使用的粒度（"1m" / "1h"），用于证明两种窗口都查了。
	calls []string
}

func (f *fakeModelPerfSource) ModelPerfBuckets(
	_ context.Context, _, _ time.Time, granularity, _ string,
) ([]perfmetrics.Bucket, time.Time, error) {
	f.calls = append(f.calls, granularity)
	if granularity == "1h" {
		return f.hourly, time.Time{}, f.errHourly
	}
	return f.minute, time.Time{}, f.errMinute
}

// fakeProbeStore 只实现 LatestByModels 的语义；其余写/读方法不会被端点调用。
type fakeProbeStore struct {
	latest map[string]store.ModelPerformanceTest
	err    error
}

func (f *fakeProbeStore) Create(
	context.Context, store.ModelPerformanceTest, []store.ModelPerformanceRun,
) (store.ModelPerformanceTest, error) {
	return store.ModelPerformanceTest{}, nil
}

func (f *fakeProbeStore) ListRecent(
	context.Context, int64, string, int,
) ([]store.ModelPerformanceTest, error) {
	return nil, nil
}

func (f *fakeProbeStore) LatestByModel(context.Context, string) (store.ModelPerformanceTest, bool, error) {
	return store.ModelPerformanceTest{}, false, nil
}

func (f *fakeProbeStore) LatestByModels(
	context.Context, []string,
) (map[string]store.ModelPerformanceTest, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]store.ModelPerformanceTest, len(f.latest))
	for model, test := range f.latest {
		out[model] = test
	}
	return out, nil
}

type fakeChannelCatalog struct {
	enabled    []string
	counts     map[string]int
	enabledErr error
	countsErr  error
}

func (f *fakeChannelCatalog) EnabledModels(context.Context) ([]string, error) {
	return f.enabled, f.enabledErr
}

func (f *fakeChannelCatalog) CountRoutableChannels(context.Context) (map[string]int, error) {
	return f.counts, f.countsErr
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// modelsTestBucket 构造一个有确定 p95 的桶：所有延迟落在 latencyMS 所在
// 的直方图桶里，p95 由桶内线性插值算出，测试断言因此可复现。
func modelsTestBucket(model string, start time.Time, total, success int64, latencyMS int) perfmetrics.Bucket {
	bucket := perfmetrics.Bucket{
		Model:        model,
		Start:        start.UTC(),
		Success:      success,
		Error:        total - success,
		LatencyCount: total,
		LatencySumMS: float64(total) * float64(latencyMS),
		TTFTCount:    total,
	}
	if index := perfmetrics.BucketIndex(latencyMS); index >= 0 {
		bucket.LatencyBuckets[index] = total
	}
	if index := perfmetrics.BucketIndex(latencyMS / 2); index >= 0 {
		bucket.TTFTBuckets[index] = total
	}
	bucket.CompletionTokens = total * 10
	bucket.GenerationSeconds = float64(total)
	bucket.ObservedSeconds = float64(total) * perfmetrics.MinuteSeconds
	bucket.UpSeconds = bucket.ObservedSeconds
	return bucket
}

// modelsTestOverflowBucket 构造延迟 p95 落进 +Inf 桶的桶：只有 finite 个样本
// 归属首桶，其余（total-finite）没有有限桶归属，即 >30s。TTFT 保持全有限。
func modelsTestOverflowBucket(model string, start time.Time, total, finite int64) perfmetrics.Bucket {
	bucket := perfmetrics.Bucket{
		Model:        model,
		Start:        start.UTC(),
		Success:      total,
		LatencyCount: total,
		TTFTCount:    total,
	}
	bucket.LatencyBuckets[0] = finite
	bucket.TTFTBuckets[0] = total
	return bucket
}

func modelsTestProbe(model string, at time.Time, runs, success int, rate float64) store.ModelPerformanceTest {
	return store.ModelPerformanceTest{
		Model:        model,
		Runs:         runs,
		SuccessCount: success,
		SuccessRate:  rate,
		CreatedAt:    at.UTC(),
	}
}

func newModelsTestMux(t *testing.T, cfg PerformanceHandlerConfig) *http.ServeMux {
	t.Helper()
	cfg.Logger = testLogger()
	cfg.Authz = rbac.NewAuthzMiddleware()
	cfg.Now = func() time.Time { return modelsTestNow }
	cfg.Getenv = func(string) string { return "" }
	cfg.Hostname = func() (string, error) { return "test-host", nil }
	handlers := NewPerformanceHandlers(cfg)
	mux := http.NewServeMux()
	handlers.Register(mux)
	return mux
}

func getModels(t *testing.T, mux *http.ServeMux) perfModelsResponse {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/models", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp perfModelsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode models response: %v (body=%s)", err, rr.Body.String())
	}
	return resp
}

func modelsRow(t *testing.T, resp perfModelsResponse, model string) perfModelRow {
	t.Helper()
	for _, row := range resp.Rows {
		if row.Model == model {
			return row
		}
	}
	t.Fatalf("row %q 不存在（现有 %d 行）", model, len(resp.Rows))
	return perfModelRow{}
}

func assertSparkShape(t *testing.T, spark []*float64) {
	t.Helper()
	if len(spark) != perfModelSparkPoints {
		t.Fatalf("spark 点数 = %d, want %d", len(spark), perfModelSparkPoints)
	}
	for i, point := range spark {
		if point != nil && (math.IsNaN(*point) || math.IsInf(*point, 0)) {
			t.Fatalf("spark[%d] 是 NaN/Inf，JSON 编码会失败", i)
		}
	}
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

func TestModelsRowsMergeTrafficAndProbe(t *testing.T) {
	recent := modelsTestNow.Add(-30 * time.Minute)
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{
			modelsTestBucket("traffic-only", recent, 100, 100, 30),
			modelsTestBucket("both", recent, 100, 100, 30),
		},
		hourly: []perfmetrics.Bucket{
			modelsTestBucket("both", recent.Add(-2*time.Hour), 100, 100, 30),
		},
	}
	probes := &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{
		// 只有探针的模型：最近一次测速在 40h 前 → degraded（>36h）。
		"probe-only": modelsTestProbe("probe-only", modelsTestNow.Add(-40*time.Hour), 3, 3, 1),
		"both":       modelsTestProbe("both", modelsTestNow.Add(-time.Hour), 3, 3, 1),
	}}
	channels := &fakeChannelCatalog{
		enabled: []string{"traffic-only", "probe-only", "both"},
		counts:  map[string]int{"traffic-only": 2, "probe-only": 1, "both": 3},
	}

	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf: source, Probes: probes, Channels: channels,
	}))

	if got, want := len(resp.Rows), 3; got != want {
		t.Fatalf("rows 数 = %d, want %d", got, want)
	}
	// 模型按名字排序，前端不需要再排。
	wantOrder := []string{"both", "probe-only", "traffic-only"}
	for i, model := range wantOrder {
		if resp.Rows[i].Model != model {
			t.Errorf("rows[%d].model = %q, want %q", i, resp.Rows[i].Model, model)
		}
	}

	if row := modelsRow(t, resp, "traffic-only"); row.Status != perfModelStatusNormal {
		t.Errorf("traffic-only status = %q, want normal", row.Status)
	}
	if row := modelsRow(t, resp, "probe-only"); row.Status != perfModelStatusDegraded {
		t.Errorf("probe-only status = %q, want degraded（探针 40h 前）", row.Status)
	}
	both := modelsRow(t, resp, "both")
	if both.Status != perfModelStatusNormal {
		t.Errorf("both status = %q, want normal", both.Status)
	}
	if both.TTFTp50 == nil || both.TTFTp95 == nil || both.LatencyP95 == nil || both.TPS == nil {
		t.Errorf("both 的流量列缺失: %+v", both)
	}
	if both.Samples != 100 {
		t.Errorf("both samples = %d, want 100", both.Samples)
	}
	if both.Channels == nil || *both.Channels != 3 {
		t.Errorf("both channels = %v, want 3", both.Channels)
	}
	if both.Probe == nil || !both.Probe.OK || both.Probe.Runs != 3 {
		t.Errorf("both probe = %+v, want ok/runs=3", both.Probe)
	}
	if both.Uptime == nil || *both.Uptime != 1 {
		t.Errorf("both uptime = %v, want 1", both.Uptime)
	}

	probeOnly := modelsRow(t, resp, "probe-only")
	if probeOnly.Samples != 0 || probeOnly.SuccessRate != nil || probeOnly.TTFTp50 != nil {
		t.Errorf("probe-only 不应有流量列: %+v", probeOnly)
	}
	if probeOnly.Probe == nil || !probeOnly.Probe.OK {
		t.Errorf("probe-only probe = %+v, want 有且 ok", probeOnly.Probe)
	}

	if summary := resp.Summary; summary.Models != 3 || summary.Degraded != 1 || summary.Incident != 0 {
		t.Errorf("summary = %+v, want models=3 degraded=1 incident=0", summary)
	}
	if len(source.calls) != 2 || source.calls[0] != "1m" || source.calls[1] != "1h" {
		t.Errorf("查询粒度 = %v, want [1m 1h]", source.calls)
	}
}

func TestModelsNoSampleWhenBothAbsent(t *testing.T) {
	channels := &fakeChannelCatalog{
		enabled: []string{"idle-model", "count-less"},
		// count-less 刻意不在 map 里：channels 必须是 null（不知道），不是 0。
		counts: map[string]int{"idle-model": 1},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     &fakeModelPerfSource{},
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: channels,
	}))

	idle := modelsRow(t, resp, "idle-model")
	if idle.Status != perfModelStatusNoSample {
		t.Errorf("idle-model status = %q, want no_sample", idle.Status)
	}
	if idle.Channels == nil || *idle.Channels != 1 {
		t.Errorf("idle-model channels = %v, want 1", idle.Channels)
	}
	if idle.Probe != nil || idle.Uptime != nil {
		t.Errorf("idle-model 不应有探针列: probe=%+v uptime=%v", idle.Probe, idle.Uptime)
	}
	if idle.Samples != 0 || idle.TTFTp50 != nil || idle.TTFTp95 != nil ||
		idle.LatencyP95 != nil || idle.TPS != nil || idle.SuccessRate != nil {
		t.Errorf("idle-model 不应有流量数字: %+v", idle)
	}
	assertSparkShape(t, idle.Spark)
	for i, point := range idle.Spark {
		if point != nil {
			t.Errorf("idle-model spark[%d] = %v, want null", i, *point)
		}
	}

	countLess := modelsRow(t, resp, "count-less")
	if countLess.Channels != nil {
		t.Errorf("count-less channels = %v, want null（不在 map = 未知）", *countLess.Channels)
	}
	if resp.Summary.NoSample != 2 {
		t.Errorf("summary.no_sample = %d, want 2", resp.Summary.NoSample)
	}
}

func TestModelsIncidentFromProbeFailure(t *testing.T) {
	probes := &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{
		"broken": modelsTestProbe("broken", modelsTestNow.Add(-time.Hour), 3, 0, 0),
	}}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     &fakeModelPerfSource{},
		Probes:   probes,
		Channels: &fakeChannelCatalog{enabled: []string{"broken"}},
	}))

	row := modelsRow(t, resp, "broken")
	if row.Status != perfModelStatusIncident {
		t.Errorf("status = %q, want incident（最新探针全失败）", row.Status)
	}
	if row.Probe == nil || row.Probe.OK {
		t.Errorf("probe = %+v, want 非 nil 且 ok=false", row.Probe)
	}
	if row.Uptime == nil || *row.Uptime != 0 {
		t.Errorf("uptime = %v, want 0", row.Uptime)
	}
	if resp.Summary.Incident != 1 {
		t.Errorf("summary.incident = %d, want 1", resp.Summary.Incident)
	}
}

func TestModelsIncidentFromTrafficSuccessRate(t *testing.T) {
	source := &fakeModelPerfSource{minute: []perfmetrics.Bucket{
		modelsTestBucket("flaky", modelsTestNow.Add(-30*time.Minute), 100, 85, 30),
	}}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"flaky"}},
	}))

	row := modelsRow(t, resp, "flaky")
	if row.Status != perfModelStatusIncident {
		t.Errorf("status = %q, want incident（窗口成功率 0.85 < 0.9）", row.Status)
	}
	if row.SuccessRate == nil || *row.SuccessRate < 0.84 || *row.SuccessRate > 0.86 {
		t.Errorf("success_rate = %v, want ≈0.85", row.SuccessRate)
	}
	if row.Samples != 100 {
		t.Errorf("samples = %d, want 100", row.Samples)
	}
}

func TestModelsDegradedWhenProbeStale(t *testing.T) {
	source := &fakeModelPerfSource{minute: []perfmetrics.Bucket{
		modelsTestBucket("stale", modelsTestNow.Add(-30*time.Minute), 100, 100, 30),
	}}
	probes := &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{
		"stale": modelsTestProbe("stale", modelsTestNow.Add(-40*time.Hour), 3, 3, 1),
	}}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   probes,
		Channels: &fakeChannelCatalog{enabled: []string{"stale"}},
	}))

	row := modelsRow(t, resp, "stale")
	if row.Status != perfModelStatusDegraded {
		t.Errorf("status = %q, want degraded（最近测速 40h 前 > 36h）", row.Status)
	}
	if row.Uptime == nil || *row.Uptime != 1 {
		t.Errorf("uptime = %v, want 1（可用率仍取探针成功率）", row.Uptime)
	}
	if resp.Summary.Degraded != 1 {
		t.Errorf("summary.degraded = %d, want 1", resp.Summary.Degraded)
	}
}

func TestModelsDegradedWhenP95Doubles(t *testing.T) {
	recent := modelsTestNow.Add(-30 * time.Minute)
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{
			// 24h 内延迟落在 [1000,2000] 桶：p95 = 1950ms。
			modelsTestBucket("slow", recent, 100, 100, 1500),
			modelsTestBucket("steady", recent, 100, 100, 30),
		},
		hourly: []perfmetrics.Bucket{
			// 7 天基线全部在 [0,50] 桶：每个非空小时 p95 = 47.5ms。
			modelsTestBucket("slow", recent.Add(-3*time.Hour), 100, 100, 30),
			modelsTestBucket("slow", recent.Add(-4*time.Hour), 100, 100, 30),
			modelsTestBucket("steady", recent.Add(-3*time.Hour), 100, 100, 30),
			modelsTestBucket("steady", recent.Add(-4*time.Hour), 100, 100, 30),
		},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"slow", "steady"}},
	}))

	slow := modelsRow(t, resp, "slow")
	if slow.LatencyP95 == nil || *slow.LatencyP95 <= 2*47.5 {
		t.Fatalf("slow latency_p95 = %v, 期望 > 2×基线中位数", slow.LatencyP95)
	}
	if slow.Status != perfModelStatusDegraded {
		t.Errorf("slow status = %q, want degraded（p95 翻倍）", slow.Status)
	}

	steady := modelsRow(t, resp, "steady")
	if steady.Status != perfModelStatusNormal {
		t.Errorf("steady status = %q, want normal", steady.Status)
	}
	if resp.Summary.Degraded != 1 {
		t.Errorf("summary.degraded = %d, want 1", resp.Summary.Degraded)
	}
}

func TestModelsPerfStoreUnavailableReturnsEmptyRows(t *testing.T) {
	source := &fakeModelPerfSource{
		errMinute: &perfmetrics.UnavailableError{Reason: perfmetrics.ReasonStorageUnavailable},
	}
	// 即使探针/渠道能给出模型，指标库不可用时整表也必须是空态（spec §7.3），
	// 不能拼出「只有探针列」的半张表。
	probes := &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{
		"gpt-4o": modelsTestProbe("gpt-4o", modelsTestNow.Add(-time.Hour), 3, 3, 1),
	}}
	mux := newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   probes,
		Channels: &fakeChannelCatalog{enabled: []string{"gpt-4o"}},
	})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/models", "", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（指标库不可用不整页 500）", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"rows":[]`) {
		t.Errorf("rows 必须是 JSON 空数组，body=%s", rr.Body.String())
	}

	resp := getModels(t, mux)
	if resp.Rows == nil {
		t.Error("rows 解码为 nil，want 空切片")
	}
	if len(resp.Rows) != 0 {
		t.Errorf("rows 数 = %d, want 0", len(resp.Rows))
	}
	if len(resp.Sources) != 2 ||
		resp.Sources[0].Name != perfSourcePerfmetrics ||
		resp.Sources[0].Status != perfSourceUnavailable {
		t.Errorf("sources = %+v, want sources[0]=perfmetrics/unavailable", resp.Sources)
	}
	if resp.Summary.Models != 0 || resp.Summary.Degraded != 0 ||
		resp.Summary.Incident != 0 || resp.Summary.NoSample != 0 {
		t.Errorf("summary = %+v, want 全 0", resp.Summary)
	}
}

// TestModelsBaselineQueryFailureKeepsTable：24h 流量查询成功、7d 基线查询失败时，
// 端点仍返回完整表格；失败只丢掉 degraded 判定的比较基准，不把 perfmetrics 标成
// error，也不清空 rows。基线属于锦上添花，流量列才是表的主体。
func TestModelsBaselineQueryFailureKeepsTable(t *testing.T) {
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{
			modelsTestBucket("live", modelsTestNow.Add(-10*time.Minute), 100, 100, 30),
		},
		errHourly: errors.New("boom"),
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"live"}, counts: map[string]int{"live": 1}},
	}))

	if len(resp.Sources) != 2 ||
		resp.Sources[0].Name != perfSourcePerfmetrics ||
		resp.Sources[0].Status != perfStatusOK {
		t.Errorf("sources = %+v, want perfmetrics/ok（基线失败不改数据源状态）", resp.Sources)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("rows 数 = %d, want 1（基线失败不清空模型表）", len(resp.Rows))
	}
	row := modelsRow(t, resp, "live")
	if row.TPS == nil || row.LatencyP95 == nil || row.TTFTp95 == nil || row.Samples != 100 {
		t.Errorf("流量列缺失: %+v", row)
	}
	if row.Status != perfModelStatusNormal {
		t.Errorf("status = %q, want normal（没有基线就不做 p95 对比，不判 degraded）", row.Status)
	}
	if resp.Summary.Degraded != 0 {
		t.Errorf("summary.degraded = %d, want 0（缺基线不是 degraded）", resp.Summary.Degraded)
	}
	// 两次查询都要发出：证明失败的是基线那一次，而不是 1h 查询被跳过。
	if len(source.calls) != 2 || source.calls[0] != "1m" || source.calls[1] != "1h" {
		t.Errorf("查询粒度 = %v, want [1m 1h]", source.calls)
	}
}

func TestModelsSparkHas24Points(t *testing.T) {
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{
			modelsTestBucket("spark-model", modelsTestNow.Add(-3*time.Hour), 100, 100, 30),
			modelsTestBucket("spark-model", modelsTestNow.Add(-time.Hour), 100, 100, 1500),
			modelsTestBucket("spark-model", modelsTestNow.Add(-10*time.Minute), 100, 100, 30),
		},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"spark-model"}},
	}))

	row := modelsRow(t, resp, "spark-model")
	assertSparkShape(t, row.Spark)
	if len(resp.Rows) != 1 {
		t.Fatalf("rows 数 = %d, want 1", len(resp.Rows))
	}

	// 位置锚定当前小时（12:00 是最后一点）：-3h → 09:00（下标 20）、
	// -1h → 11:00（22）、-10m → 12:00（23）；10:00（21）没有桶必须为 null。
	wantSet := map[int]bool{20: true, 22: true, 23: true}
	for i, point := range row.Spark {
		if wantSet[i] {
			if point == nil {
				t.Errorf("spark[%d] 有样本却为 null", i)
			}
			continue
		}
		if point != nil {
			t.Errorf("spark[%d] = %v, want null（该小时没有样本）", i, *point)
		}
	}
	if row.Spark[23] == nil || *row.Spark[23] <= 0 {
		t.Errorf("spark[23] = %v, want 当前小时的 p95", row.Spark[23])
	}
}

// TestModelsNilPerfKeepsRows：指标库未装配（Perf==nil）不是整表空态——启用模型
// 与探针行照常出行，流量列为 null，sources 标明 perfmetrics 不可用。
func TestModelsNilPerfKeepsRows(t *testing.T) {
	probes := &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{
		"probe-model": modelsTestProbe("probe-model", modelsTestNow.Add(-time.Hour), 3, 3, 1),
	}}
	channels := &fakeChannelCatalog{
		enabled: []string{"probe-model", "idle-model"},
		counts:  map[string]int{"probe-model": 2, "idle-model": 1},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Probes: probes, Channels: channels,
	}))

	if len(resp.Sources) != 2 ||
		resp.Sources[0].Name != perfSourcePerfmetrics ||
		resp.Sources[0].Status != perfSourceUnavailable ||
		resp.Sources[1].Status != perfStatusOK {
		t.Fatalf("sources = %+v, want perfmetrics/unavailable + probes/ok", resp.Sources)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("rows 数 = %d, want 2（启用模型照常出行）", len(resp.Rows))
	}

	for _, model := range []string{"probe-model", "idle-model"} {
		row := modelsRow(t, resp, model)
		if row.Samples != 0 || row.TTFTp50 != nil || row.TTFTp95 != nil ||
			row.LatencyP95 != nil || row.TPS != nil || row.SuccessRate != nil {
			t.Errorf("%s 流量列应为 null: %+v", model, row)
		}
		if row.TTFTP95Over30s || row.LatencyP95Over30s {
			t.Errorf("%s 没有流量却置了 over_30s 标志: %+v", model, row)
		}
		assertSparkShape(t, row.Spark)
	}

	probeRow := modelsRow(t, resp, "probe-model")
	if probeRow.Probe == nil || probeRow.Uptime == nil {
		t.Errorf("probe-model 探针列缺失: %+v", probeRow)
	}
	if probeRow.Status != perfModelStatusNormal {
		t.Errorf("probe-model status = %q, want normal", probeRow.Status)
	}
	if idle := modelsRow(t, resp, "idle-model"); idle.Status != perfModelStatusNoSample {
		t.Errorf("idle-model status = %q, want no_sample", idle.Status)
	}
}

// TestModelsNilProbesKeepsTraffic：探针存储未装配时流量列不受影响，probe/uptime
// 为 null，sources 标明 probes 不可用。
func TestModelsNilProbesKeepsTraffic(t *testing.T) {
	source := &fakeModelPerfSource{minute: []perfmetrics.Bucket{
		modelsTestBucket("live", modelsTestNow.Add(-10*time.Minute), 100, 100, 30),
	}}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Channels: &fakeChannelCatalog{enabled: []string{"live"}, counts: map[string]int{"live": 1}},
	}))

	if len(resp.Sources) != 2 ||
		resp.Sources[0].Status != perfStatusOK ||
		resp.Sources[1].Name != perfSourceProbes ||
		resp.Sources[1].Status != perfSourceUnavailable {
		t.Fatalf("sources = %+v, want perfmetrics/ok + probes/unavailable", resp.Sources)
	}

	row := modelsRow(t, resp, "live")
	if row.Probe != nil || row.Uptime != nil {
		t.Errorf("探针列应为 null: probe=%+v uptime=%v", row.Probe, row.Uptime)
	}
	if row.LatencyP95 == nil || row.TTFTp95 == nil || row.TPS == nil || row.Samples != 100 {
		t.Errorf("流量列缺失: %+v", row)
	}
	if row.Status != perfModelStatusNormal {
		t.Errorf("status = %q, want normal", row.Status)
	}
}

// TestModelsChannelsCountFailureIsNull：渠道数查询失败只让 channels 整列变 null，
// 端点仍 200，其它列正常。
func TestModelsChannelsCountFailureIsNull(t *testing.T) {
	source := &fakeModelPerfSource{minute: []perfmetrics.Bucket{
		modelsTestBucket("counted", modelsTestNow.Add(-10*time.Minute), 100, 100, 30),
	}}
	channels := &fakeChannelCatalog{
		enabled:   []string{"counted"},
		countsErr: errors.New("counts query down"),
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: channels,
	}))

	row := modelsRow(t, resp, "counted")
	if row.Channels != nil {
		t.Errorf("channels = %v, want null（查询失败不是 0 个渠道）", *row.Channels)
	}
	if row.LatencyP95 == nil || row.TTFTp95 == nil || row.TPS == nil || row.Samples != 100 {
		t.Errorf("其它列应正常: %+v", row)
	}
	if row.Status != perfModelStatusNormal {
		t.Errorf("status = %q, want normal", row.Status)
	}
}

// TestModelsIncidentBeatsDegraded：最新探针全失败且已过期（>36h）时，incident
// 优先于 degraded——状态顺序严格 incident → degraded。
func TestModelsIncidentBeatsDegraded(t *testing.T) {
	source := &fakeModelPerfSource{minute: []perfmetrics.Bucket{
		modelsTestBucket("dead", modelsTestNow.Add(-10*time.Minute), 100, 100, 30),
	}}
	probes := &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{
		"dead": modelsTestProbe("dead", modelsTestNow.Add(-40*time.Hour), 3, 0, 0),
	}}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   probes,
		Channels: &fakeChannelCatalog{enabled: []string{"dead"}},
	}))

	row := modelsRow(t, resp, "dead")
	if row.Status != perfModelStatusIncident {
		t.Errorf("status = %q, want incident（全失败优先于过期 degraded）", row.Status)
	}
	if resp.Summary.Incident != 1 || resp.Summary.Degraded != 0 {
		t.Errorf("summary = %+v, want incident=1 degraded=0", resp.Summary)
	}
}

// TestModelsInfinityP95Flagged：24h 延迟 p95 落进 +Inf 桶时，*_ms 仍是
// 30000 下界，但 over_30s 布尔位必须为 true；对应 spark 槽为 null；只要 7d
// 存在有限基线就判 degraded。
func TestModelsInfinityP95Flagged(t *testing.T) {
	recent := modelsTestNow.Add(-10 * time.Minute)
	// 100 个样本里 94 个在 [0,50)，其余 6 个落进 +Inf（>30s）：p95 秩 = 95
	// 越过 94 个有限样本，落进 +Inf 桶。
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{modelsTestOverflowBucket("overflow", recent, 100, 94)},
		hourly: []perfmetrics.Bucket{
			modelsTestBucket("overflow", recent.Add(-3*time.Hour), 100, 100, 20000),
			modelsTestBucket("overflow", recent.Add(-4*time.Hour), 100, 100, 20000),
		},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"overflow"}},
	}))

	row := modelsRow(t, resp, "overflow")
	if !row.LatencyP95Over30s {
		t.Errorf("latency_p95_over_30s = false, want true（p95 落 +Inf 桶）")
	}
	if row.LatencyP95 == nil || *row.LatencyP95 != modelPerfInfinityMS {
		t.Errorf("latency_p95_ms = %v, want 下界标记 %v", row.LatencyP95, modelPerfInfinityMS)
	}
	if row.TTFTP95Over30s {
		t.Errorf("ttft_p95_over_30s = true, want false（TTFT 全在有限桶）")
	}
	if row.Status != perfModelStatusDegraded {
		t.Errorf("status = %q, want degraded（>30s 且存在有限基线）", row.Status)
	}
	assertSparkShape(t, row.Spark)
	// 唯一的分钟桶在 12:20（当前小时 → 下标 23），p95 落 +Inf → null。
	if row.Spark[23] != nil {
		t.Errorf("spark[23] = %v, want null（+Inf 小时不画 30000）", *row.Spark[23])
	}
}

// TestModelsTTFTP50Over30sFlagged：TTFT p50 落进 +Inf 桶时，ttft_p50_ms 仍是
// 30000 下界，ttft_p50_over_30s 必须为 true。
func TestModelsTTFTP50Over30sFlagged(t *testing.T) {
	bucket := perfmetrics.Bucket{
		Model:        "slow-ttft",
		Start:        modelsTestNow.Add(-10 * time.Minute),
		Success:      100,
		LatencyCount: 100,
		TTFTCount:    100,
	}
	bucket.LatencyBuckets[0] = 100
	// 60 个 TTFT 样本 >30s：p50 秩 = 50 越过 40 个有限样本，落进 +Inf 桶。
	bucket.TTFTBuckets[0] = 40

	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     &fakeModelPerfSource{minute: []perfmetrics.Bucket{bucket}},
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"slow-ttft"}},
	}))

	row := modelsRow(t, resp, "slow-ttft")
	if !row.TTFTP50Over30s {
		t.Errorf("ttft_p50_over_30s = false, want true（p50 落 +Inf 桶）")
	}
	if row.TTFTp50 == nil || *row.TTFTp50 != modelPerfInfinityMS {
		t.Errorf("ttft_p50_ms = %v, want 下界标记 %v", row.TTFTp50, modelPerfInfinityMS)
	}
	if !row.TTFTP95Over30s {
		t.Errorf("ttft_p95_over_30s = false, want true（p95 同样落 +Inf 桶）")
	}
	if row.LatencyP95Over30s {
		t.Errorf("latency_p95_over_30s = true, want false（延迟全在有限桶）")
	}
	if row.Status != perfModelStatusNormal {
		t.Errorf("status = %q, want normal", row.Status)
	}
}

// TestModelsNoDegradedWhenBaselineAllInfinity：24h 与 7d 的 p95 全落 +Inf 时
// 没有有限基线可比，不判 degraded。
func TestModelsNoDegradedWhenBaselineAllInfinity(t *testing.T) {
	recent := modelsTestNow.Add(-10 * time.Minute)
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{modelsTestOverflowBucket("all-inf", recent, 100, 94)},
		hourly: []perfmetrics.Bucket{
			modelsTestOverflowBucket("all-inf", recent.Add(-3*time.Hour), 100, 94),
			modelsTestOverflowBucket("all-inf", recent.Add(-4*time.Hour), 100, 94),
		},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"all-inf"}},
	}))

	row := modelsRow(t, resp, "all-inf")
	if !row.LatencyP95Over30s {
		t.Fatalf("latency_p95_over_30s = false, want true（用例前提）")
	}
	if row.Status == perfModelStatusDegraded {
		t.Errorf("status = degraded, want 非 degraded（7d 无有限基线）")
	}
	if resp.Summary.Degraded != 0 {
		t.Errorf("summary.degraded = %d, want 0", resp.Summary.Degraded)
	}
}

// TestModelsDegradedBaselineIgnoresInfinityHours：7d 既有有限小时又有 +Inf
// 小时时，中位数只能用有限小时。有限基线 1950ms、24h p95 29000ms → degraded；
// 若 +Inf 小时的 30000 下界混进中位数（中位数 15975ms，2× = 31950ms），
// 29000 不会超过阈值，状态会错误地留在 normal。
func TestModelsDegradedBaselineIgnoresInfinityHours(t *testing.T) {
	recent := modelsTestNow.Add(-10 * time.Minute)
	source := &fakeModelPerfSource{
		minute: []perfmetrics.Bucket{
			modelsTestBucket("mixed-base", recent, 100, 100, 25000), // p95 = 29000ms
		},
		hourly: []perfmetrics.Bucket{
			modelsTestBucket("mixed-base", recent.Add(-3*time.Hour), 100, 100, 1500),  // p95 = 1950ms
			modelsTestOverflowBucket("mixed-base", recent.Add(-4*time.Hour), 100, 94), // +Inf 小时
		},
	}
	resp := getModels(t, newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     source,
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"mixed-base"}},
	}))

	row := modelsRow(t, resp, "mixed-base")
	if row.LatencyP95Over30s {
		t.Fatalf("latency_p95_over_30s = true, want false（用例前提：24h p95 有限）")
	}
	if row.LatencyP95 == nil || math.Abs(*row.LatencyP95-29000) > 1 {
		t.Fatalf("latency_p95_ms = %v, want ≈29000", row.LatencyP95)
	}
	if row.Status != perfModelStatusDegraded {
		t.Errorf("status = %q, want degraded（中位数只取有限小时）", row.Status)
	}
}

// TestModelsRejectsUnsupportedRange：模型表固定 24h，显式传其它 range 返回 400，
// detail 前缀与 overview 一致（invalid_range:）。
func TestModelsRejectsUnsupportedRange(t *testing.T) {
	mux := newModelsTestMux(t, PerformanceHandlerConfig{
		Perf:     &fakeModelPerfSource{},
		Probes:   &fakeProbeStore{latest: map[string]store.ModelPerformanceTest{}},
		Channels: &fakeChannelCatalog{enabled: []string{"m"}},
	})

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/models?range=7d", "", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（不支持 7d）", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "invalid_range:") {
		t.Errorf("body 缺少 invalid_range 前缀: %s", body)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, perfRequest(http.MethodGet, "/api/performance/models?range=24h", "", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("range=24h status = %d, want 200", rr.Code)
	}
}
