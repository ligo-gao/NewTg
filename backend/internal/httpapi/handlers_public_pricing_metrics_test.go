package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

type fakePublicModelMetrics struct {
	data     store.PublicModelMetrics
	err      error
	called   bool
	gotModel string
	gotStart time.Time
	gotEnd   time.Time

	batchModels []string
	batchData   map[string]store.PublicModelMetrics
	batchErr    error
	batchCalled bool
	batchStart  time.Time
	batchEnd    time.Time
}

func (f *fakePublicModelMetrics) LoadPublicModelMetrics(
	_ context.Context,
	model string,
	start, end time.Time,
) (store.PublicModelMetrics, error) {
	f.called = true
	f.gotModel = model
	f.gotStart = start
	f.gotEnd = end
	return f.data, f.err
}

func (f *fakePublicModelMetrics) LoadPublicModelMetricsBatch(
	_ context.Context,
	models []string,
	start, end time.Time,
) (map[string]store.PublicModelMetrics, error) {
	f.batchCalled = true
	f.batchModels = append([]string(nil), models...)
	f.batchStart = start
	f.batchEnd = end
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	return f.batchData, nil
}

// fakePublicPerfStore 是公开性能接口的测速事实接缝（最新一次主动测试）。
type fakePublicPerfStore struct {
	test    store.ModelPerformanceTest
	ok      bool
	err     error
	called  bool
	modelID string

	batchModels []string
	batchTests  map[string]store.ModelPerformanceTest
	batchErr    error
}

func (f *fakePublicPerfStore) Create(
	_ context.Context, test store.ModelPerformanceTest, _ []store.ModelPerformanceRun,
) (store.ModelPerformanceTest, error) {
	return test, nil
}

func (f *fakePublicPerfStore) ListRecent(
	_ context.Context, _ int64, _ string, _ int,
) ([]store.ModelPerformanceTest, error) {
	return nil, nil
}

func (f *fakePublicPerfStore) LatestByModel(
	_ context.Context, model string,
) (store.ModelPerformanceTest, bool, error) {
	f.called = true
	f.modelID = model
	if f.err != nil {
		return store.ModelPerformanceTest{}, false, f.err
	}
	return f.test, f.ok, nil
}

func (f *fakePublicPerfStore) LatestByModels(
	_ context.Context,
	models []string,
) (map[string]store.ModelPerformanceTest, error) {
	f.batchModels = append([]string(nil), models...)
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	return f.batchTests, nil
}

func newMetricsTestHandler(t *testing.T, metrics store.PublicModelMetricsSource) *PublicPricingHandlers {
	t.Helper()
	return newMetricsTestHandlerWithPerf(t, metrics, nil)
}

func newMetricsTestHandlerWithPerf(
	t *testing.T,
	metrics store.PublicModelMetricsSource,
	perf store.ModelPerformanceStore,
) *PublicPricingHandlers {
	t.Helper()
	handlers, err := NewPublicPricingHandlers(PublicPricingHandlerConfig{
		Repo:    &fakePublicPricingRepo{catalog: fixtureCatalog()},
		Metrics: metrics,
		Perf:    perf,
	})
	if err != nil {
		t.Fatalf("NewPublicPricingHandlers: %v", err)
	}
	return handlers
}

func requestMetrics(
	t *testing.T,
	handlers *PublicPricingHandlers,
	modelID, rawQuery string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/pricing/x/metrics"+rawQuery, http.NoBody)
	req.SetPathValue("model_id", modelID)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	handlers.handlePublicPricingMetrics(rr, req)
	return rr
}

func TestPublicMetricsSummaryAndTPS(t *testing.T) {
	metrics := &fakePublicModelMetrics{data: store.PublicModelMetrics{
		SampleSize:       100,
		SuccessCount:     90,
		Failed24h:        3,
		LatencySumMs:     100_000,
		CompletionTokens: 50_000,
		AvgLatencyMs:     publicFloatPtr(1000.4),
		AvgTTFTMs:        publicFloatPtr(200.6),
		Groups: []store.PublicGroupMetrics{
			{Group: "default", SampleSize: 80, SuccessCount: 72, AvgLatencyMs: publicFloatPtr(900), AvgTTFTMs: publicFloatPtr(180)},
			{Group: "vip", SampleSize: 20, SuccessCount: 18, AvgLatencyMs: publicFloatPtr(1400), AvgTTFTMs: nil},
		},
	}}
	handlers := newMetricsTestHandler(t, metrics)

	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	var body PublicModelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Range != "7d" {
		t.Fatalf("range = %q", body.Range)
	}
	if body.Summary.SampleSize != 100 {
		t.Fatalf("sample_size = %d", body.Summary.SampleSize)
	}
	if body.Summary.AvgLatencyMs == nil || *body.Summary.AvgLatencyMs != 1000 {
		t.Fatalf("avg_latency_ms = %v", body.Summary.AvgLatencyMs)
	}
	if body.Summary.AvgTTFTMs == nil || *body.Summary.AvgTTFTMs != 201 {
		t.Fatalf("avg_ttft_ms = %v", body.Summary.AvgTTFTMs)
	}
	if body.Summary.TPS == nil || *body.Summary.TPS != 500 {
		t.Fatalf("tps = %v", body.Summary.TPS)
	}
	if body.Summary.SuccessRate == nil || *body.Summary.SuccessRate != 0.9 {
		t.Fatalf("success_rate = %v", body.Summary.SuccessRate)
	}
	if body.Summary.Failed24h == nil || *body.Summary.Failed24h != 3 {
		t.Fatalf("failed_24h = %v", body.Summary.Failed24h)
	}
	if len(body.PerGroup) != 2 {
		t.Fatalf("per_group len = %d", len(body.PerGroup))
	}
	if body.PerGroup[1].AvgTTFTMs != nil {
		t.Fatalf("vip ttft should be null, got %v", body.PerGroup[1].AvgTTFTMs)
	}
	if !metrics.called || metrics.gotModel != "openai/gpt-5" {
		t.Fatalf("metrics source called=%v model=%q", metrics.called, metrics.gotModel)
	}
	if got := metrics.gotEnd.Sub(metrics.gotStart); got != 7*24*time.Hour {
		t.Fatalf("window = %v", got)
	}
}

func TestPublicMetricsNoDataUsesNulls(t *testing.T) {
	handlers := newMetricsTestHandler(t, &fakePublicModelMetrics{})
	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=24h", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	var body PublicModelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Summary.SampleSize != 0 {
		t.Fatalf("sample_size = %d", body.Summary.SampleSize)
	}
	if body.Summary.AvgLatencyMs != nil || body.Summary.AvgTTFTMs != nil ||
		body.Summary.TPS != nil || body.Summary.SuccessRate != nil || body.Summary.Failed24h != nil {
		t.Fatalf("expected all-null summary, got %+v", body.Summary)
	}
	if body.PerGroup == nil {
		t.Fatalf("per_group must be [] not null")
	}
}

func TestPublicMetricsHiddenModelIs404(t *testing.T) {
	handlers := newMetricsTestHandler(t, &fakePublicModelMetrics{})
	rr := requestMetrics(t, handlers, "not/visible", "?range=7d", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestPublicMetricsInvalidRangeIs400(t *testing.T) {
	handlers := newMetricsTestHandler(t, &fakePublicModelMetrics{})
	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=30d", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestPublicMetricsWithoutSourceIs503(t *testing.T) {
	handlers := newMetricsTestHandler(t, nil)
	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestPublicMetricsETag(t *testing.T) {
	handlers := newMetricsTestHandler(t, &fakePublicModelMetrics{})
	first := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatalf("missing ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("cache-control = %q", got)
	}
	second := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d", second.Code)
	}
}

func TestPublicMetricsIncludesLatestTest(t *testing.T) {
	testedAt := time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC)
	perf := &fakePublicPerfStore{ok: true, test: store.ModelPerformanceTest{
		ID:           7,
		Model:        "openai/gpt-5",
		Runs:         120,
		SuccessCount: 118,
		SuccessRate:  0.9833,
		TTFTMSP50:    publicIntPtr(180),
		TTFTMSP95:    publicIntPtr(400),
		LatencyMSP50: publicIntPtr(900),
		LatencyMSP95: publicIntPtr(1700),
		TPSAvg:       publicFloatPtr(55.5),
		CreatedAt:    testedAt,
	}}
	handlers := newMetricsTestHandlerWithPerf(t, &fakePublicModelMetrics{}, perf)

	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	var body PublicModelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	latest := body.LatestTest
	if latest == nil {
		t.Fatalf("latest_test = nil, body=%s", rr.Body.String())
	}
	if latest.TTFTMSP50 == nil || *latest.TTFTMSP50 != 180 {
		t.Fatalf("ttft_ms_p50 = %v", latest.TTFTMSP50)
	}
	if latest.TTFTMSP95 == nil || *latest.TTFTMSP95 != 400 {
		t.Fatalf("ttft_ms_p95 = %v", latest.TTFTMSP95)
	}
	if latest.LatencyMSP50 == nil || *latest.LatencyMSP50 != 900 {
		t.Fatalf("latency_ms_p50 = %v", latest.LatencyMSP50)
	}
	if latest.LatencyMSP95 == nil || *latest.LatencyMSP95 != 1700 {
		t.Fatalf("latency_ms_p95 = %v", latest.LatencyMSP95)
	}
	if latest.TPS == nil || *latest.TPS != 55.5 {
		t.Fatalf("tps = %v", latest.TPS)
	}
	if latest.SuccessRate != 0.9833 {
		t.Fatalf("success_rate = %v", latest.SuccessRate)
	}
	if latest.SampleSize != 120 {
		t.Fatalf("sample_size = %d", latest.SampleSize)
	}
	if latest.TestedAt != "2026-10-23T12:30:00Z" {
		t.Fatalf("tested_at = %q", latest.TestedAt)
	}
	if !perf.called || perf.modelID != "openai/gpt-5" {
		t.Fatalf("perf source called=%v model=%q", perf.called, perf.modelID)
	}
}

func TestPublicMetricsLatestTestNullsWhenAbsent(t *testing.T) {
	// nil 接缝：旧装配或测试环境没有主库时，指标端点仍须 200 且 latest_test=null。
	handlers := newMetricsTestHandler(t, &fakePublicModelMetrics{})
	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body PublicModelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LatestTest != nil {
		t.Fatalf("latest_test = %+v, want null", body.LatestTest)
	}

	// 无记录（ok=false）同样是 null，而不是零值对象。
	perf := &fakePublicPerfStore{}
	handlers = newMetricsTestHandlerWithPerf(t, &fakePublicModelMetrics{}, perf)
	rr = requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LatestTest != nil {
		t.Fatalf("latest_test = %+v, want null", body.LatestTest)
	}
}

func TestPublicMetricsLatestTestErrorIsFailOpen(t *testing.T) {
	perf := &fakePublicPerfStore{err: errors.New("perf db is down")}
	handlers := newMetricsTestHandlerWithPerf(t, &fakePublicModelMetrics{}, perf)
	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.LatestTest != nil {
		t.Fatalf("latest_test = %+v, want null", body.LatestTest)
	}
}

func TestPublicMetricsETagChangesWithLatestTest(t *testing.T) {
	perf := &fakePublicPerfStore{ok: true, test: store.ModelPerformanceTest{
		ID:        1,
		Model:     "openai/gpt-5",
		CreatedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
	}}
	handlers := newMetricsTestHandlerWithPerf(t, &fakePublicModelMetrics{}, perf)

	first := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	firstETag := first.Header().Get("ETag")
	if firstETag == "" {
		t.Fatalf("missing ETag")
	}

	// 新一次主动测试必须让缓存键失效：旧 ETag 不能再命中 304。
	perf.test.ID = 2
	perf.test.CreatedAt = time.Date(2026, 10, 23, 12, 5, 0, 0, time.UTC)
	second := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d",
		map[string]string{"If-None-Match": firstETag})
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a new probe", second.Code)
	}
	secondETag := second.Header().Get("ETag")
	if secondETag == "" || secondETag == firstETag {
		t.Fatalf("ETag did not change: first=%q second=%q", firstETag, secondETag)
	}
	if got := second.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("cache-control = %q", got)
	}

	third := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d",
		map[string]string{"If-None-Match": secondETag})
	if third.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", third.Code)
	}
}

// 用量日志库故障不能让整张性能表变成「暂不可用」：首页只消费 latest_test，
// 因此 summary 降级为零样本、latest_test 照常返回，响应仍是 200。
func TestPublicPricingMetricsFailsOpenWhenLogsDBIsDown(t *testing.T) {
	// 故意给一份非零数据：真实的 SQL 实现出错时才返回零值，只有这样才能
	// 钉住 handler 显式清零（否则删掉 `data = store.PublicModelMetrics{}`
	// 测试仍然全绿）。
	metrics := &fakePublicModelMetrics{
		data: store.PublicModelMetrics{
			SampleSize:   7,
			SuccessCount: 7,
			AvgTTFTMs:    publicFloatPtr(99),
			Groups:       []store.PublicGroupMetrics{{Group: "default", SampleSize: 7, SuccessCount: 7}},
		},
		err: errors.New("logs db down"),
	}
	perf := &fakePublicPerfStore{
		ok: true,
		test: store.ModelPerformanceTest{
			ID: 9, Model: "openai/gpt-5", Runs: 3, SuccessCount: 3, SuccessRate: 1,
			TTFTMSP50: publicIntPtr(210), TPSAvg: publicFloatPtr(42.5),
			CreatedAt: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC),
		},
	}
	handlers := newMetricsTestHandlerWithPerf(t, metrics, perf)

	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s（summary 必须 fail-open）", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Summary.SampleSize != 0 || body.Summary.AvgTTFTMs != nil || body.Summary.TPS != nil {
		t.Fatalf("degraded summary must be zero-sample: %+v", body.Summary)
	}
	if len(body.PerGroup) != 0 {
		t.Fatalf("degraded per_group must be empty: %+v", body.PerGroup)
	}
	if body.LatestTest == nil || body.LatestTest.TTFTMSP50 == nil || *body.LatestTest.TTFTMSP50 != 210 {
		t.Fatalf("latest_test must survive the degraded summary: %+v", body.LatestTest)
	}
	if !metrics.called {
		t.Fatalf("metrics source was not called")
	}
}

// 目录读取失败仍是 500：可见性判断依赖目录，不能与日志库故障一样降级。
func TestPublicMetricsCatalogFailureStays500(t *testing.T) {
	metrics := &fakePublicModelMetrics{}
	handlers, err := NewPublicPricingHandlers(PublicPricingHandlerConfig{
		Repo:    &fakePublicPricingRepo{catalogErr: errors.New("catalog db down")},
		Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewPublicPricingHandlers: %v", err)
	}

	rr := requestMetrics(t, handlers, "openai/gpt-5", "?range=7d", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rr.Code, rr.Body.String())
	}
	if metrics.called {
		t.Fatalf("usage summary must not be read when the catalog fails")
	}
}

// 降级响应与正常的零样本响应内容几乎相同（summary 全空），缓存键必须区分二者：
// 否则日志库恢复后，共享缓存还会把降级结果回放 60 秒。哈希段必须不同，
// 不能只靠分钟桶滚动「碰巧」不同。
func TestPublicMetricsDegradedETagDiffersFromZeroSample(t *testing.T) {
	degradedHandlers := newMetricsTestHandler(t,
		&fakePublicModelMetrics{err: errors.New("logs db down")})
	normalHandlers := newMetricsTestHandler(t, &fakePublicModelMetrics{})

	degraded := requestMetrics(t, degradedHandlers, "openai/gpt-5", "?range=7d", nil)
	normal := requestMetrics(t, normalHandlers, "openai/gpt-5", "?range=7d", nil)
	if degraded.Code != http.StatusOK || normal.Code != http.StatusOK {
		t.Fatalf("status degraded=%d normal=%d", degraded.Code, normal.Code)
	}

	degradedETag := degraded.Header().Get("ETag")
	normalETag := normal.Header().Get("ETag")
	if degradedETag == "" || normalETag == "" {
		t.Fatalf("missing ETag: degraded=%q normal=%q", degradedETag, normalETag)
	}
	if degradedETag == normalETag {
		t.Fatalf("degraded ETag must differ from zero-sample ETag: %q", degradedETag)
	}
	if metricsETagHash(t, degradedETag) == metricsETagHash(t, normalETag) {
		t.Fatalf("degraded ETag hash segment must differ: degraded=%q normal=%q",
			degradedETag, normalETag)
	}
}

// metricsETagHash 取出 `"metrics-v1-<hash>-<minute>"` 中的哈希段，用于忽略
// 分钟桶滚动带来的干扰。
func metricsETagHash(t *testing.T, etag string) string {
	t.Helper()
	const prefix = `"metrics-v1-`
	if !strings.HasPrefix(etag, prefix) {
		t.Fatalf("unexpected ETag format: %q", etag)
	}
	hash, _, ok := strings.Cut(strings.TrimPrefix(etag, prefix), "-")
	if !ok || hash == "" {
		t.Fatalf("unexpected ETag format: %q", etag)
	}
	return hash
}

func publicFloatPtr(v float64) *float64 { return &v }

func publicIntPtr(v int) *int { return &v }
