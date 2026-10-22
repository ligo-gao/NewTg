package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/store"
)

// W16-1：渠道指标端点（F-06-11）与渠道测试限流（F-08-16）的 handler 契约。
//
// 纪律同 handlers_channel_test.go：不连真 PG/日志库；日志库聚合的 SQL 形状由
// store 的集成用例覆盖，本文件只证明缓存、窗口校验、权限/404/503 映射与限流。

// ---------- 内存 fake（ChannelMetricsStore） ----------

type fakeChannelMetricsStore struct {
	mu sync.Mutex

	getCalls     int
	summaryCalls int
	lastChannel  int64
	lastWindow   string
	lastIDs      []int64

	getErr     error
	summaryErr error
	// summaries 是批量查询的返回值；未命中的渠道由 handler 填零值概要。
	summaries map[int64]store.ChannelSummary
}

func (f *fakeChannelMetricsStore) Get(_ context.Context, channelID int64, window string, _ time.Time) (*store.ChannelMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	f.lastChannel = channelID
	f.lastWindow = window
	if f.getErr != nil {
		return nil, f.getErr
	}
	return metricsFixture(window), nil
}

func (f *fakeChannelMetricsStore) Summaries(_ context.Context, channelIDs []int64, _ time.Time) (map[int64]store.ChannelSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaryCalls++
	f.lastIDs = append([]int64(nil), channelIDs...)
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	return f.summaries, nil
}

func (f *fakeChannelMetricsStore) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, f.summaryCalls
}

func (f *fakeChannelMetricsStore) lastGet() (int64, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastChannel, f.lastWindow
}

// metricsFixture 造一个与 window 桶数严格一致的响应（24/168）。
func metricsFixture(window string) *store.ChannelMetrics {
	n, _ := store.ChannelMetricsWindowBuckets(window)
	points := make([]store.ChannelMetricsPoint, n)
	base := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for i := range points {
		points[i] = store.ChannelMetricsPoint{
			BucketStart:  base.Add(time.Duration(i) * time.Hour),
			RequestCount: 3,
			SuccessCount: 2,
			FailureCount: 1,
			SuccessRate:  floatPtr(2.0 / 3.0),
			AvgLatencyMS: floatPtr(300),
			AvgCost:      floatPtr(20),
		}
	}
	return &store.ChannelMetrics{
		Points: points,
		Summary: store.ChannelMetricsSummary{
			RequestCount:    3,
			SuccessCount:    2,
			FailureCount:    1,
			SuccessRate:     floatPtr(2.0 / 3.0),
			AvgLatencyMS:    floatPtr(300),
			AvgCost:         floatPtr(20),
			CostTotal:       60,
			UsedCredits:     600,
			UsedCredits24h:  60,
			RequestCount24h: 3,
		},
	}
}

func floatPtr(v float64) *float64 { return &v }

func newChannelMetricsTestEnv(t *testing.T, ms *fakeChannelMetricsStore, opts ChannelHandlerOptions) *channelTestEnv {
	t.Helper()
	if ms == nil {
		ms = &fakeChannelMetricsStore{}
	}
	opts.Metrics = ms
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Date(2026, 10, 22, 12, 34, 0, 0, time.UTC) }
	}
	env := newChannelTestEnv(t, opts)
	env.store.add(&store.Channel{
		ID:              1,
		Name:            "alpha",
		Type:            "openai",
		Status:          1,
		SupportedModels: []byte(`["gpt-4","gpt-4o"]`),
	})
	return env
}

func decodeMetricsBody(t *testing.T, rr *httptest.ResponseRecorder) channelMetricsResponse {
	t.Helper()
	var out channelMetricsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode metrics response: %v (body=%s)", err, rr.Body.String())
	}
	return out
}

// TestChannelMetricsEndpointWindows 覆盖：默认 24h、显式 7d 的 24/168 点形状、
// 非法窗口 422、渠道不存在 404、权限不足 403。
func TestChannelMetricsEndpointWindows(t *testing.T) {
	ms := &fakeChannelMetricsStore{}
	env := newChannelMetricsTestEnv(t, ms, ChannelHandlerOptions{})

	t.Run("default_window_is_24h", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := decodeMetricsBody(t, rr)
		if body.Window != store.ChannelMetricsWindow24h {
			t.Fatalf("window = %q, want 24h", body.Window)
		}
		if len(body.Points) != 24 {
			t.Fatalf("points = %d, want 24", len(body.Points))
		}
		if body.ChannelID != 1 || body.BucketSeconds != 3600 {
			t.Fatalf("channel_id/bucket_seconds = %d/%d", body.ChannelID, body.BucketSeconds)
		}
	})

	t.Run("seven_days_has_168_points", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics?window=7d", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := decodeMetricsBody(t, rr)
		if body.Window != store.ChannelMetricsWindow7d {
			t.Fatalf("window = %q, want 7d", body.Window)
		}
		if len(body.Points) != 168 {
			t.Fatalf("points = %d, want 168", len(body.Points))
		}
		// 假 store 必须收到 7d，而不是被默认值覆盖。
		if _, window := ms.lastGet(); window != "7d" {
			t.Fatalf("store window = %q, want 7d", window)
		}
	})

	t.Run("invalid_window_is_422", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics?window=30d", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("missing_channel_is_404", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/999/metrics", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("without_channels_read_is_403", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics", nil),
			channelAdminPrincipal(PermChannelsManage))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
		}
	})
}

// TestChannelMetricsUnavailable 覆盖：日志库未装配 / 查询失败 → 503（RFC7807），
// 而不是把内部错误伪装成 500 或返回空序列。
func TestChannelMetricsUnavailable(t *testing.T) {
	t.Run("nil_metrics_store", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		env.store.add(&store.Channel{ID: 1, Name: "alpha", Type: "openai", Status: 1})
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("logs_query_error", func(t *testing.T) {
		ms := &fakeChannelMetricsStore{getErr: errors.New("logs db is down")}
		env := newChannelMetricsTestEnv(t, ms, ChannelHandlerOptions{})
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
		}
	})
}

// TestChannelMetricsCacheHitAndWindowIsolation 覆盖：
//   - 第二次同窗口请求命中 Redis，不再查日志库；
//   - 24h 与 7d 是两个缓存条目（键含 window，变异点 ① 的守护用例）。
func TestChannelMetricsCacheHitAndWindowIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ms := &fakeChannelMetricsStore{}
	env := newChannelMetricsTestEnv(t, ms, ChannelHandlerOptions{Redis: rdb})

	get := func(window string) *httptest.ResponseRecorder {
		target := "/api/channels/1/metrics?window=" + window
		return serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, target, nil),
			channelAdminPrincipal(PermChannelsRead))
	}

	if rr := get("24h"); rr.Code != http.StatusOK {
		t.Fatalf("first 24h status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	if rr := get("24h"); rr.Code != http.StatusOK {
		t.Fatalf("second 24h status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	if calls, _ := ms.calls(); calls != 1 {
		t.Fatalf("logs queries after two 24h requests = %d, want 1 (cache hit)", calls)
	}

	if rr := get("7d"); rr.Code != http.StatusOK {
		t.Fatalf("7d status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	if calls, _ := ms.calls(); calls != 2 {
		t.Fatalf("logs queries after 7d request = %d, want 2 (window must be part of cache key)", calls)
	}

	keys := mr.Keys()
	want24h := channelMetricsCacheKey(1, "24h")
	want7d := channelMetricsCacheKey(1, "7d")
	if !containsMetricKey(keys, want24h) || !containsMetricKey(keys, want7d) {
		t.Fatalf("cache keys = %v, want both %q and %q", keys, want24h, want7d)
	}
	if ttl := mr.TTL(want24h); ttl != channelMetricsCacheTTL {
		t.Fatalf("cache TTL = %s, want %s", ttl, channelMetricsCacheTTL)
	}
}

// TestChannelMetricsRedisFailureFallsBackToLogs 覆盖：Redis 不可用时 fail-open，
// 直查日志库并正常返回（指标读路径不因缓存层故障 503）。
func TestChannelMetricsRedisFailureFallsBackToLogs(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		MaxRetries:   -1,
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	ms := &fakeChannelMetricsStore{}
	env := newChannelMetricsTestEnv(t, ms, ChannelHandlerOptions{Redis: rdb})
	rr := serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels/1/metrics", nil),
		channelAdminPrincipal(PermChannelsRead))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when Redis is down (body=%s)", rr.Code, rr.Body.String())
	}
	if calls, _ := ms.calls(); calls != 1 {
		t.Fatalf("logs queries = %d, want 1 (must fall back to direct query)", calls)
	}
}

// TestChannelListEmbedsMetricsWithOneBatchQuery 覆盖 W16-1 列表概要：
//   - 一次批量查询（不是 N+1）；
//   - 有样本渠道回填概要，无样本渠道得到零值概要（比率 null）；
//   - 日志库故障时列表仍 200、整批省略 metrics。
func TestChannelListEmbedsMetricsWithOneBatchQuery(t *testing.T) {
	ms := &fakeChannelMetricsStore{summaries: map[int64]store.ChannelSummary{
		1: {RequestCount: 7, SuccessRate: floatPtr(0.5), AvgLatencyMS: floatPtr(120)},
	}}
	env := newChannelMetricsTestEnv(t, ms, ChannelHandlerOptions{})

	items := make([]store.Channel, 0, 30)
	for i := int64(1); i <= 30; i++ {
		items = append(items, store.Channel{
			ID:              i,
			Name:            "ch" + strconv.FormatInt(i, 10),
			Type:            "openai",
			Status:          1,
			SupportedModels: []byte(`["gpt-4"]`),
		})
	}
	env.store.listItems = items

	rr := serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels", nil),
		channelAdminPrincipal(PermChannelsRead))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var body channelListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if _, summaryCalls := ms.calls(); summaryCalls != 1 {
		t.Fatalf("summary batch calls = %d, want exactly 1 (no N+1)", summaryCalls)
	}
	if len(ms.lastIDs) != 30 {
		t.Fatalf("batch ids = %d, want 30", len(ms.lastIDs))
	}
	first := body.Items[0]
	if first.Metrics == nil || first.Metrics.RequestCount != 7 || first.Metrics.SuccessRate == nil {
		t.Fatalf("channel 1 metrics = %+v, want request_count=7 and rate", first.Metrics)
	}
	second := body.Items[1]
	if second.Metrics == nil || second.Metrics.RequestCount != 0 ||
		second.Metrics.SuccessRate != nil || second.Metrics.AvgLatencyMS != nil {
		t.Fatalf("channel 2 metrics = %+v, want zero counts and null rates", second.Metrics)
	}
}

func TestChannelListOmitsMetricsWhenLogsUnavailable(t *testing.T) {
	ms := &fakeChannelMetricsStore{summaryErr: errors.New("logs db down")}
	env := newChannelMetricsTestEnv(t, ms, ChannelHandlerOptions{})
	env.store.listItems = []store.Channel{{ID: 1, Name: "alpha", Type: "openai", Status: 1}}

	rr := serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels", nil),
		channelAdminPrincipal(PermChannelsRead))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when logs db is down (body=%s)", rr.Code, rr.Body.String())
	}
	var body channelListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Metrics != nil {
		t.Fatalf("metrics = %+v, want omitted when logs db is unavailable", body.Items[0].Metrics)
	}
}

// TestChannelTestRateLimit 覆盖：渠道测试 10 次/分钟/管理员，第 11 次 429 +
// Retry-After（变异点 ③ 的守护用例）。
func TestChannelTestRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tester := &fakeChannelTester{latency: 12}
	env := newChannelTestEnv(t, ChannelHandlerOptions{Redis: rdb, Tester: tester.test})
	env.store.add(&store.Channel{
		ID: 1, Name: "alpha", Type: "openai", Status: 1,
		SupportedModels: []byte(`["gpt-4"]`),
	})

	doTest := func() *httptest.ResponseRecorder {
		return serveWithPrincipal(t, env.mux,
			jsonReq(t, http.MethodPost, "/api/channels/1/test", map[string]any{"model": "gpt-4"}),
			channelAdminPrincipal(PermChannelsManage))
	}

	for i := 0; i < ChannelTestRateLimit; i++ {
		if rr := doTest(); rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (body=%s)", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := doTest()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request status = %d, want 429 (body=%s)", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}
}

// TestChannelTestRateLimitRedisDownFailsOpen 覆盖：Redis 不可用时测试接口
// fail-open（200），而不是把管理员的连通性测试打成 503。
func TestChannelTestRateLimitRedisDownFailsOpen(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		MaxRetries:   -1,
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	tester := &fakeChannelTester{latency: 12}
	env := newChannelTestEnv(t, ChannelHandlerOptions{Redis: rdb, Tester: tester.test})
	env.store.add(&store.Channel{
		ID: 1, Name: "alpha", Type: "openai", Status: 1,
		SupportedModels: []byte(`["gpt-4"]`),
	})
	req := jsonReq(t, http.MethodPost, "/api/channels/1/test", map[string]any{"model": "gpt-4"})
	rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal(PermChannelsManage))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open) when Redis is down (body=%s)", rr.Code, rr.Body.String())
	}
}

func containsMetricKey(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
