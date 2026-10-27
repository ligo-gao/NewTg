package httpapi

// 模型库列表页的批量公开指标端点（GET /api/pricing/metrics）单测。
//
// 覆盖可见性（隐藏模型不进入批量读）、数据筛选（无样本且无测速的模型不出现）、
// 共享 ETag/304、指标源缺失 503、测速读取失败 fail-open 与非法 range 400。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

// metricsCatalog 复用 fixtureCatalog()，按模型名取子集并保持目录顺序。
func metricsCatalog(models ...string) store.PublicPricingCatalog {
	catalog := fixtureCatalog()
	if len(models) == 0 {
		return catalog
	}
	wanted := make(map[string]struct{}, len(models))
	for _, model := range models {
		wanted[model] = struct{}{}
	}
	kept := make([]store.PublicModelPricing, 0, len(models))
	for _, row := range catalog.Models {
		if _, ok := wanted[row.Model]; ok {
			kept = append(kept, row)
		}
	}
	catalog.Models = kept
	return catalog
}

// metricsCatalogWithHiddenModel 在 fixtureCatalog() 基础上追加一条停用档案
// （status=2），使 image/dall-e 按列表端点同一可见性规则被剔除。
//
// 这里扩展的是副本而不是共享的 fixtureCatalog：后者被列表/详情用例按
// total=6 断言，直接改它会连带改变无关测试的口径。
func metricsCatalogWithHiddenModel() store.PublicPricingCatalog {
	catalog := fixtureCatalog()
	catalog.Meta = []store.ModelMeta{
		{ModelName: "image/dall-e", Status: 2, NameRule: 0},
	}
	return catalog
}

func newMetricsListHandler(
	t *testing.T,
	catalog store.PublicPricingCatalog,
	metrics store.PublicModelMetricsSource,
	perf store.ModelPerformanceStore,
) *PublicPricingHandlers {
	t.Helper()
	handlers, err := NewPublicPricingHandlers(PublicPricingHandlerConfig{
		Repo:    &fakePublicPricingRepo{catalog: catalog},
		Metrics: metrics,
		Perf:    perf,
	})
	if err != nil {
		t.Fatalf("NewPublicPricingHandlers: %v", err)
	}
	return handlers
}

func requestMetricsList(
	t *testing.T,
	handlers *PublicPricingHandlers,
	rawQuery string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/pricing/metrics"+rawQuery, http.NoBody)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	handlers.handlePublicPricingMetricsList(rr, req)
	return rr
}

// assertBatchWindow 断言批量读收到的窗口长度与「end 约等于当前时刻」。
// start 由 handler 用 end.Add(-window) 精确派生，因此长度可以精确比较；
// end 与 now 之间留一分钟容差（请求调度/CI 抖动的上界）。
func assertBatchWindow(t *testing.T, metrics *fakePublicModelMetrics, want time.Duration) {
	t.Helper()
	if !metrics.batchCalled {
		t.Fatal("LoadPublicModelMetricsBatch 未被调用")
	}
	if got := metrics.batchEnd.Sub(metrics.batchStart); got != want {
		t.Fatalf("batch window = %v, want %v", got, want)
	}
	if delta := time.Since(metrics.batchEnd); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("batch end = %v, not close to now (delta=%v)", metrics.batchEnd, delta)
	}
}

func TestPublicMetricsListOnlyReturnsModelsWithData(t *testing.T) {
	metrics := &fakePublicModelMetrics{batchData: map[string]store.PublicModelMetrics{
		"openai/gpt-5": {
			SampleSize:       12,
			SuccessCount:     10,
			Failed24h:        1,
			LatencySumMs:     12_000,
			CompletionTokens: 6_000,
			AvgLatencyMs:     publicFloatPtr(1000),
			AvgTTFTMs:        publicFloatPtr(200),
		},
	}}
	perf := &fakePublicPerfStore{batchTests: map[string]store.ModelPerformanceTest{
		"anthropic/claude": {
			ID: 3, Model: "anthropic/claude", Runs: 50, SuccessRate: 0.96,
			LatencyMSP50: publicIntPtr(800), LatencyMSP95: publicIntPtr(1500),
			CreatedAt: time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
		},
	}}
	handlers := newMetricsListHandler(t,
		metricsCatalog("openai/gpt-5", "openai/gpt-free", "anthropic/claude"), metrics, perf)

	rr := requestMetricsList(t, handlers, "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	var body PublicModelMetricsList
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Range != "7d" {
		t.Fatalf("range = %q", body.Range)
	}
	assertBatchWindow(t, metrics, 7*24*time.Hour)
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2 (body=%s)", len(body.Items), rr.Body.String())
	}

	byID := make(map[string]PublicModelMetricsItem, len(body.Items))
	for _, item := range body.Items {
		byID[item.ModelID] = item
	}
	if _, ok := byID["openai/gpt-free"]; ok {
		t.Fatal("无样本且无测速的模型不应出现在 items 中")
	}

	usage := byID["openai/gpt-5"]
	if usage.Summary.SampleSize <= 0 {
		t.Fatalf("usage 项 sample_size = %d, want > 0", usage.Summary.SampleSize)
	}
	if usage.LatestTest != nil {
		t.Fatalf("usage-only 项的 latest_test = %+v, want null", usage.LatestTest)
	}
	if usage.Summary.AvgLatencyMs == nil || *usage.Summary.AvgLatencyMs != 1000 {
		t.Fatalf("usage 项 avg_latency_ms = %v", usage.Summary.AvgLatencyMs)
	}

	testOnly := byID["anthropic/claude"]
	if testOnly.Summary.SampleSize != 0 {
		t.Fatalf("test-only 项 sample_size = %d, want 0", testOnly.Summary.SampleSize)
	}
	if testOnly.LatestTest == nil {
		t.Fatal("test-only 项 latest_test = nil, want 最近一次测速")
	}
	if testOnly.Summary.AvgLatencyMs != nil {
		t.Fatalf("test-only 项 avg_latency_ms = %v, want null", testOnly.Summary.AvgLatencyMs)
	}
}

func TestPublicMetricsListPassesVisibleModelsToBatch(t *testing.T) {
	metrics := &fakePublicModelMetrics{batchData: map[string]store.PublicModelMetrics{}}
	perf := &fakePublicPerfStore{batchTests: map[string]store.ModelPerformanceTest{}}
	handlers := newMetricsListHandler(t, metricsCatalogWithHiddenModel(), metrics, perf)

	rr := requestMetricsList(t, handlers, "?range=24h", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	assertBatchWindow(t, metrics, 24*time.Hour)

	// 停用档案（status=2）的 image/dall-e 必须与列表端点一样被剔除，
	// 不得进入 usage_logs 或 model_performance_tests 的批量查询。
	want := []string{"openai/gpt-5", "openai/gpt-free", "anthropic/claude", "vendorless-model", "gemini/flash"}
	if len(metrics.batchModels) != len(want) {
		t.Fatalf("batch models = %v, want %v", metrics.batchModels, want)
	}
	for i, model := range want {
		if metrics.batchModels[i] != model {
			t.Fatalf("batch models[%d] = %q, want %q (all=%v)", i, metrics.batchModels[i], model, metrics.batchModels)
		}
	}
	for _, model := range metrics.batchModels {
		if model == "image/dall-e" {
			t.Fatalf("隐藏模型进入了 metrics 批量读: %v", metrics.batchModels)
		}
	}
	for _, model := range perf.batchModels {
		if model == "image/dall-e" {
			t.Fatalf("隐藏模型进入了测速批量读: %v", perf.batchModels)
		}
	}
}

func TestPublicMetricsListETagAnd304(t *testing.T) {
	handlers := newMetricsTestHandlerWithPerf(t, &fakePublicModelMetrics{}, nil)

	first := requestMetricsList(t, handlers, "?range=24h", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("cache-control = %q", got)
	}

	// 同一分钟、同一身份：ETag 稳定。
	second := requestMetricsList(t, handlers, "?range=24h", nil)
	if got := second.Header().Get("ETag"); got != etag {
		t.Fatalf("ETag 不稳定: first=%q second=%q", etag, got)
	}

	conditional := requestMetricsList(t, handlers, "?range=24h",
		map[string]string{"If-None-Match": etag})
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", conditional.Code)
	}
	if got := conditional.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("304 cache-control = %q", got)
	}
	if conditional.Body.Len() != 0 {
		t.Fatalf("304 响应带 body: %q", conditional.Body.String())
	}
}

func TestPublicMetricsListWithoutSourceDegradesSummary(t *testing.T) {
	// 日志库接缝未装配不再整页 503：summary 降级为零样本，主库里的测速读数
	// 照常返回。性能表的可用性不能挂在一个可选数据源上。
	testedAt := time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC)
	perf := &fakePublicPerfStore{batchTests: map[string]store.ModelPerformanceTest{
		"openai/gpt-5": {
			ID:           11,
			Model:        "openai/gpt-5",
			Runs:         5,
			SuccessCount: 5,
			SuccessRate:  1,
			TTFTMSP50:    publicIntPtr(120),
			CreatedAt:    testedAt,
		},
	}}
	handlers := newMetricsListHandler(t, metricsCatalog("openai/gpt-5"), nil, perf)

	rr := requestMetricsList(t, handlers, "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded), body=%s", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsList
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1 (body=%s)", len(body.Items), rr.Body.String())
	}
	if body.Items[0].Summary.SampleSize != 0 {
		t.Fatalf("降级 summary sample_size = %d, want 0", body.Items[0].Summary.SampleSize)
	}
	if body.Items[0].LatestTest == nil {
		t.Fatal("测速读数在主库，降级时不应丢失")
	}
}

func TestPublicMetricsListBatchFailureDegradesSummary(t *testing.T) {
	// 日志库故障与接缝缺失同口径：保留测速读数，summary 归零。
	metrics := &fakePublicModelMetrics{batchErr: errors.New("logs db is down")}
	perf := &fakePublicPerfStore{batchTests: map[string]store.ModelPerformanceTest{
		"openai/gpt-5": {ID: 12, Model: "openai/gpt-5", Runs: 3, SuccessCount: 3, CreatedAt: time.Now().UTC()},
	}}
	handlers := newMetricsListHandler(t, metricsCatalog("openai/gpt-5"), metrics, perf)

	rr := requestMetricsList(t, handlers, "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded), body=%s", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsList
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Summary.SampleSize != 0 || body.Items[0].LatestTest == nil {
		t.Fatalf("items = %s, want one degraded item with latest_test", rr.Body.String())
	}
}

func TestPublicMetricsListPerfFailureFailsOpen(t *testing.T) {
	metrics := &fakePublicModelMetrics{batchData: map[string]store.PublicModelMetrics{
		"openai/gpt-5": {SampleSize: 5, SuccessCount: 4, AvgLatencyMs: publicFloatPtr(700)},
	}}
	perf := &fakePublicPerfStore{batchErr: errors.New("perf db is down")}
	handlers := newMetricsListHandler(t, metricsCatalog("openai/gpt-5"), metrics, perf)

	rr := requestMetricsList(t, handlers, "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open), body=%s", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsList
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1 (body=%s)", len(body.Items), rr.Body.String())
	}
	if body.Items[0].LatestTest != nil {
		t.Fatalf("latest_test = %+v, want null", body.Items[0].LatestTest)
	}
	if body.Items[0].Summary.SampleSize != 5 {
		t.Fatalf("usage 汇总丢失: sample_size = %d", body.Items[0].Summary.SampleSize)
	}
}

func TestPublicMetricsListRejectsInvalidRange(t *testing.T) {
	handlers := newMetricsTestHandlerWithPerf(t, &fakePublicModelMetrics{}, nil)

	rr := requestMetricsList(t, handlers, "?range=30d", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestPublicMetricsListEmptyCatalogReturnsEmptyItems(t *testing.T) {
	catalog := fixtureCatalog()
	catalog.Models = nil
	metrics := &fakePublicModelMetrics{}
	perf := &fakePublicPerfStore{}
	handlers := newMetricsListHandler(t, catalog, metrics, perf)

	rr := requestMetricsList(t, handlers, "?range=7d", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsList
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// null 与 [] 在解码后分别是 nil 与非 nil 空切片：空目录必须下发 []
	// （契约 required + 前端按数组渲染）。
	if body.Items == nil {
		t.Fatalf("items = null, want []（body=%s）", rr.Body.String())
	}
	if len(body.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(body.Items))
	}
	if metrics.batchCalled {
		t.Fatal("空目录不应查询 usage_logs")
	}
	if len(perf.batchModels) != 0 {
		t.Fatalf("空目录不应查询测速表: %v", perf.batchModels)
	}
}

func TestPublicMetricsListDefaultsTo24h(t *testing.T) {
	metrics := &fakePublicModelMetrics{}
	handlers := newMetricsTestHandlerWithPerf(t, metrics, nil)

	rr := requestMetricsList(t, handlers, "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body PublicModelMetricsList
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Range != "24h" {
		t.Fatalf("range = %q, want 24h", body.Range)
	}
	assertBatchWindow(t, metrics, 24*time.Hour)
}

func TestPublicMetricsListETagChangesWithRangeAndPayload(t *testing.T) {
	perf := &fakePublicPerfStore{batchTests: map[string]store.ModelPerformanceTest{
		"openai/gpt-5": {
			ID: 1, Model: "openai/gpt-5",
			CreatedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		},
	}}
	handlers := newMetricsListHandler(t, metricsCatalog("openai/gpt-5"),
		&fakePublicModelMetrics{}, perf)

	daily := requestMetricsList(t, handlers, "?range=24h", nil)
	if daily.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", daily.Code, daily.Body.String())
	}
	weekly := requestMetricsList(t, handlers, "?range=7d", nil)
	if weekly.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", weekly.Code, weekly.Body.String())
	}
	if daily.Header().Get("ETag") == weekly.Header().Get("ETag") {
		t.Fatalf("range 变化后 ETag 未变: %q", daily.Header().Get("ETag"))
	}

	// 同 range、同分钟，但主动测速身份变化 ⇒ 缓存键必须失效。
	perf.batchTests["openai/gpt-5"] = store.ModelPerformanceTest{
		ID: 2, Model: "openai/gpt-5",
		CreatedAt: time.Date(2026, 10, 23, 12, 5, 0, 0, time.UTC),
	}
	afterProbe := requestMetricsList(t, handlers, "?range=24h", nil)
	if afterProbe.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", afterProbe.Code, afterProbe.Body.String())
	}
	if afterProbe.Header().Get("ETag") == daily.Header().Get("ETag") {
		t.Fatalf("测速身份变化后 ETag 未变: %q", afterProbe.Header().Get("ETag"))
	}
}
