package httpapi

// M6c Task 7 首页统计端点测试：口径/0 值/缓存/缓存头/限流/故障语义。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"newtg/gateway/internal/store"
)

func TestHomeStatsCountsAndCache(t *testing.T) {
	repo := &fakePublicPricingRepo{counts: store.PublicCounts{Models: 130, Upstreams: 42}}
	env := newPublicTestEnv(t, nil, repo, nil)

	rr := env.request(http.MethodGet, "/api/home/stats", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var body HomeStatsView
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if body.Models != 130 || body.Upstreams != 42 {
		t.Fatalf("真实计数未下发: %+v", body)
	}
	// endpoints/routing_dims 是冻结常量（R-PUB-08）。
	if body.Endpoints != 3 || body.RoutingDims != 6 {
		t.Fatalf("冻结常量错误: endpoints=%d routing_dims=%d", body.Endpoints, body.RoutingDims)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", cc)
	}

	// 5 分钟服务端缓存：窗口内第二次请求不再触达仓储。
	if repo.countsCalls.Load() != 1 {
		t.Fatalf("首次请求应装载一次，calls=%d", repo.countsCalls.Load())
	}
	env.request(http.MethodGet, "/api/home/stats", nil, nil)
	if repo.countsCalls.Load() != 1 {
		t.Fatalf("缓存窗口内不应重复查询，calls=%d", repo.countsCalls.Load())
	}
}

func TestHomeStatsZeroValues(t *testing.T) {
	repo := &fakePublicPricingRepo{counts: store.PublicCounts{Models: 0, Upstreams: 0}}
	env := newPublicTestEnv(t, nil, repo, nil)
	rr := env.request(http.MethodGet, "/api/home/stats", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body map[string]int64
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	// 0 如实返回（前端按 F-09-04 隐藏），不是省略字段。
	if value, ok := body["models"]; !ok || value != 0 {
		t.Fatalf("0 值必须如实下发: %v", body)
	}
	if value, ok := body["upstreams"]; !ok || value != 0 {
		t.Fatalf("0 值必须如实下发: %v", body)
	}
}

func TestHomeStatsRepoErrorIs500(t *testing.T) {
	repo := &fakePublicPricingRepo{countsErr: errors.New("db down")}
	env := newPublicTestEnv(t, nil, repo, nil)
	rr := env.request(http.MethodGet, "/api/home/stats", nil, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("DB 故障应 500，得到 %d", rr.Code)
	}
	if body := rr.Body.String(); body == "{}" || body == "" {
		t.Fatalf("失败不得返回 200 空对象或空 body: %q", body)
	}
}

func TestHomeStatsRateLimit(t *testing.T) {
	repo := &fakePublicPricingRepo{counts: store.PublicCounts{Models: 1, Upstreams: 1}}
	env := newPublicTestEnv(t, nil, repo, nil)

	// B8：60 次/分钟/IP+path。前 60 次放行，第 61 次 429。
	for i := 0; i < PublicReadRateLimit; i++ {
		if rr := env.request(http.MethodGet, "/api/home/stats", nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求 = %d", i+1, rr.Code)
		}
	}
	rr := env.request(http.MethodGet, "/api/home/stats", nil, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("超限请求应 429，得到 %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
}

// TestHomeStatsCacheExpiry 直接构造 handler，用时钟接缝验证 TTL 到期后重查。
func TestHomeStatsCacheExpiry(t *testing.T) {
	repo := &fakePublicPricingRepo{counts: store.PublicCounts{Models: 1, Upstreams: 1}}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	handler, err := NewHomeStatsHandlers(HomeStatsHandlerConfig{
		Repo: repo, CacheTTL: time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewHomeStatsHandlers: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/home/stats", handler.handleHomeStats)

	serve := func() *httptest.ResponseRecorder {
		return serveWith(mux, httptest.NewRequest(http.MethodGet, "/api/home/stats", http.NoBody))
	}
	if rr := serve(); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	serve()
	if repo.countsCalls.Load() != 1 {
		t.Fatalf("窗口内 calls=%d", repo.countsCalls.Load())
	}
	now = now.Add(61 * time.Second)
	serve()
	if repo.countsCalls.Load() != 2 {
		t.Fatalf("TTL 到期后应重查，calls=%d", repo.countsCalls.Load())
	}
}

func serveWith(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}
