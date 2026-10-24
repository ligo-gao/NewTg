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

func TestPublicMetricsListMetricsUnavailable(t *testing.T) {
	handlers := newMetricsTestHandlerWithPerf(t, nil, nil)

	rr := requestMetricsList(t, handlers, "?range=7d", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != "https://newtg.dev/problems/metrics-unavailable" {
		t.Fatalf("problem type = %q", problem.Type)
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
