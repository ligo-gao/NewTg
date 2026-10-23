package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func newMetricsTestHandler(t *testing.T, metrics store.PublicModelMetricsSource) *PublicPricingHandlers {
	t.Helper()
	handlers, err := NewPublicPricingHandlers(PublicPricingHandlerConfig{
		Repo:    &fakePublicPricingRepo{catalog: fixtureCatalog()},
		Metrics: metrics,
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

func publicFloatPtr(v float64) *float64 { return &v }
