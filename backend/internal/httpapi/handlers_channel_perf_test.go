package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/channelhealth"
	"newtg/gateway/internal/store"
)

// 本文件覆盖渠道测速管理端点（POST/GET /api/channels/{id}/performance-tests）。
//
// 纪律与 handlers_channel_metrics_test.go 相同：只用手写 fake，不连 PG/Redis。
// 真实 PerfRunner 的上游行为由 channelhealth/perf_test.go 覆盖，真实 SQL 形状
// 由 store 的集成用例覆盖；这里只约束 handler 的聚合口径、权限、限流与错误映射。

// ---------- 内存 fake（ChannelPerformer / ModelPerformanceStore） ----------

type fakeChannelPerformer struct {
	mu sync.Mutex

	calls    int
	gotID    int64
	gotModel string
	gotRuns  int
	results  []channelhealth.PerfRunResult
}

func (f *fakeChannelPerformer) run(_ context.Context, ch *store.Channel, model string, runs int) []channelhealth.PerfRunResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotID = ch.ID
	f.gotModel = model
	f.gotRuns = runs
	return f.results
}

func (f *fakeChannelPerformer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeModelPerformanceStore struct {
	mu sync.Mutex

	createCalls int
	createErr   error
	lastTest    store.ModelPerformanceTest
	lastRuns    []store.ModelPerformanceRun
	nextID      int64

	listCalls     int
	listErr       error
	lastChannelID int64
	lastModel     string
	lastLimit     int
	items         []store.ModelPerformanceTest
}

func (f *fakeModelPerformanceStore) Create(_ context.Context, test store.ModelPerformanceTest, runs []store.ModelPerformanceRun) (store.ModelPerformanceTest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.lastTest = test
	f.lastRuns = append([]store.ModelPerformanceRun(nil), runs...)
	if f.createErr != nil {
		return store.ModelPerformanceTest{}, f.createErr
	}
	f.nextID++
	test.ID = f.nextID
	test.CreatedAt = time.Date(2026, 10, 23, 12, 0, int(f.nextID), 0, time.UTC)
	return test, nil
}

func (f *fakeModelPerformanceStore) ListRecent(_ context.Context, channelID int64, model string, limit int) ([]store.ModelPerformanceTest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.lastChannelID = channelID
	f.lastModel = model
	f.lastLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]store.ModelPerformanceTest(nil), f.items...), nil
}

func (f *fakeModelPerformanceStore) LatestByModel(_ context.Context, _ string) (store.ModelPerformanceTest, bool, error) {
	return store.ModelPerformanceTest{}, false, nil
}

// LatestByModels 满足接口的批量读面；管理面用例不读取批量接口。
func (f *fakeModelPerformanceStore) LatestByModels(_ context.Context, _ []string) (map[string]store.ModelPerformanceTest, error) {
	return nil, nil
}

func perfIntPtr(v int) *int { return &v }

// ---------- 脚手架 ----------

func newChannelPerfTestEnv(t *testing.T, opts ChannelHandlerOptions) *channelTestEnv {
	t.Helper()
	env := newChannelTestEnv(t, opts)
	env.store.add(&store.Channel{
		ID:              1,
		Name:            "alpha",
		Type:            "openai",
		Status:          1,
		SupportedModels: []byte(`["gpt-4o","gpt-4o-mini"]`),
	})
	return env
}

// knownPerfRuns 是确定性的样本：3 次成功 + 1 次失败，便于手算分位数与均值。
func knownPerfRuns() []channelhealth.PerfRunResult {
	return []channelhealth.PerfRunResult{
		{Success: true, TTFTMS: perfIntPtr(100), LatencyMS: perfIntPtr(500), OutputTokens: perfIntPtr(50), TPS: floatPtr(100)},
		{Success: true, TTFTMS: perfIntPtr(200), LatencyMS: perfIntPtr(700), OutputTokens: perfIntPtr(50), TPS: floatPtr(80)},
		{Success: true, TTFTMS: perfIntPtr(300), LatencyMS: perfIntPtr(900), OutputTokens: perfIntPtr(50), TPS: floatPtr(60)},
		{Success: false, ErrorClass: channelhealth.PerfClassUpstream},
	}
}

func postPerformanceTest(t *testing.T, env *channelTestEnv, body any, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonReq(t, http.MethodPost, "/api/channels/1/performance-tests", body)
	return serveWithPrincipal(t, env.mux, req, p)
}

// ---------- 用例 ----------

// TestChannelPerformanceTestAggregatesRuns 覆盖：一次 Performer 调用、成功率、
// nearest-rank P50/P95、均值，以及 Create 收到逐次明细。
func TestChannelPerformanceTestAggregatesRuns(t *testing.T) {
	performer := &fakeChannelPerformer{results: knownPerfRuns()}
	st := &fakeModelPerformanceStore{}
	env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
		Performer:   performer.run,
		Performance: st,
	})

	rr := postPerformanceTest(t, env,
		map[string]any{"model": "gpt-4o", "runs": 4},
		channelAdminPrincipal(PermChannelsManage))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	if performer.callCount() != 1 {
		t.Fatalf("performer calls = %d, want 1", performer.callCount())
	}
	performer.mu.Lock()
	gotID, gotModel, gotRuns := performer.gotID, performer.gotModel, performer.gotRuns
	performer.mu.Unlock()
	if gotID != 1 || gotModel != "gpt-4o" || gotRuns != 4 {
		t.Fatalf("performer args = (%d,%q,%d), want (1,gpt-4o,4)", gotID, gotModel, gotRuns)
	}

	st.mu.Lock()
	lastTest, lastRuns, createCalls := st.lastTest, st.lastRuns, st.createCalls
	st.mu.Unlock()
	if createCalls != 1 {
		t.Fatalf("Create calls = %d, want 1", createCalls)
	}
	if lastTest.ChannelID != 1 || lastTest.Model != "gpt-4o" || lastTest.Provider != "openai" {
		t.Fatalf("test identity = %+v", lastTest)
	}
	if lastTest.Runs != 4 || lastTest.SuccessCount != 3 || lastTest.SuccessRate != 0.75 {
		t.Fatalf("runs/success = %d/%d/%v, want 4/3/0.75", lastTest.Runs, lastTest.SuccessCount, lastTest.SuccessRate)
	}
	if lastTest.CreatedBy == nil || *lastTest.CreatedBy != testAdminID {
		t.Fatalf("created_by = %v, want %d", lastTest.CreatedBy, testAdminID)
	}
	assertIntPtr(t, "ttft_ms_p50", lastTest.TTFTMSP50, 200)
	assertIntPtr(t, "ttft_ms_p95", lastTest.TTFTMSP95, 300)
	assertIntPtr(t, "latency_ms_p50", lastTest.LatencyMSP50, 700)
	assertIntPtr(t, "latency_ms_p95", lastTest.LatencyMSP95, 900)
	if lastTest.TPSAvg == nil || *lastTest.TPSAvg != 80 {
		t.Fatalf("tps_avg = %v, want 80", lastTest.TPSAvg)
	}
	if lastTest.OutputTokensAvg == nil || *lastTest.OutputTokensAvg != 50 {
		t.Fatalf("output_tokens_avg = %v, want 50", lastTest.OutputTokensAvg)
	}

	if len(lastRuns) != 4 {
		t.Fatalf("persisted runs = %d, want 4", len(lastRuns))
	}
	for i, run := range lastRuns {
		if run.RunIndex != i {
			t.Fatalf("run[%d].RunIndex = %d", i, run.RunIndex)
		}
	}
	if !lastRuns[0].Success || lastRuns[0].TTFTMS == nil || *lastRuns[0].TTFTMS != 100 {
		t.Fatalf("run[0] = %+v", lastRuns[0])
	}
	if lastRuns[3].Success || lastRuns[3].ErrorClass != channelhealth.PerfClassUpstream {
		t.Fatalf("failed run = %+v", lastRuns[3])
	}

	var body struct {
		ID              int64    `json:"id"`
		ChannelID       int64    `json:"channel_id"`
		Model           string   `json:"model"`
		Provider        string   `json:"provider"`
		Runs            int      `json:"runs"`
		SuccessCount    int      `json:"success_count"`
		SuccessRate     float64  `json:"success_rate"`
		TTFTMSP50       *int     `json:"ttft_ms_p50"`
		TTFTMSP95       *int     `json:"ttft_ms_p95"`
		LatencyMSP50    *int     `json:"latency_ms_p50"`
		LatencyMSP95    *int     `json:"latency_ms_p95"`
		TPSAvg          *float64 `json:"tps_avg"`
		OutputTokensAvg *float64 `json:"output_tokens_avg"`
		CreatedAt       string   `json:"created_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if body.ID == 0 || body.ChannelID != 1 || body.Runs != 4 || body.SuccessCount != 3 || body.SuccessRate != 0.75 {
		t.Fatalf("response identity = %+v", body)
	}
	assertIntPtr(t, "response ttft_ms_p50", body.TTFTMSP50, 200)
	assertIntPtr(t, "response latency_ms_p95", body.LatencyMSP95, 900)
	if _, err := time.Parse(time.RFC3339, body.CreatedAt); err != nil {
		t.Fatalf("created_at %q is not RFC3339: %v", body.CreatedAt, err)
	}
}

func assertIntPtr(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}

// TestChannelPerformanceTestNilSamplesStayNull 覆盖：失败 run 的可空字段不参与
// 聚合；全部失败时 P50/P95 与均值必须是 null 而不是 0。
func TestChannelPerformanceTestNilSamplesStayNull(t *testing.T) {
	performer := &fakeChannelPerformer{results: []channelhealth.PerfRunResult{
		{Success: false, ErrorClass: channelhealth.PerfClassUpstream},
		{Success: false, ErrorClass: channelhealth.PerfClassUpstream},
	}}
	st := &fakeModelPerformanceStore{}
	env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
		Performer:   performer.run,
		Performance: st,
	})

	rr := postPerformanceTest(t, env,
		map[string]any{"model": "gpt-4o", "runs": 2},
		channelAdminPrincipal(PermChannelsManage))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	st.mu.Lock()
	lastTest := st.lastTest
	st.mu.Unlock()
	if lastTest.SuccessCount != 0 || lastTest.SuccessRate != 0 {
		t.Fatalf("success = %d/%v, want 0/0", lastTest.SuccessCount, lastTest.SuccessRate)
	}
	if lastTest.TTFTMSP50 != nil || lastTest.TTFTMSP95 != nil ||
		lastTest.LatencyMSP50 != nil || lastTest.LatencyMSP95 != nil ||
		lastTest.TPSAvg != nil || lastTest.OutputTokensAvg != nil {
		t.Fatalf("expected all nullable aggregates to be nil, got %+v", lastTest)
	}

	body := decodeJSONResponse[map[string]any](t, rr)
	for _, key := range []string{"ttft_ms_p50", "ttft_ms_p95", "latency_ms_p50", "latency_ms_p95", "tps_avg", "output_tokens_avg"} {
		if value, ok := body[key]; !ok {
			t.Fatalf("response missing %q (explicit null expected)", key)
		} else if value != nil {
			t.Fatalf("response %q = %v, want null", key, value)
		}
	}
}

// TestChannelPerformanceTestValidation 覆盖：模型必填且属于 supported_models，
// runs 1..5；非法请求不调用 Performer。
func TestChannelPerformanceTestValidation(t *testing.T) {
	cases := []struct {
		name string
		body any
	}{
		{"missing_model", map[string]any{"runs": 3}},
		{"blank_model", map[string]any{"model": "   ", "runs": 3}},
		{"unsupported_model", map[string]any{"model": "not-listed", "runs": 3}},
		{"runs_too_large", map[string]any{"model": "gpt-4o", "runs": 6}},
		{"runs_negative", map[string]any{"model": "gpt-4o", "runs": -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			performer := &fakeChannelPerformer{results: knownPerfRuns()}
			st := &fakeModelPerformanceStore{}
			env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
				Performer:   performer.run,
				Performance: st,
			})
			rr := postPerformanceTest(t, env, tc.body, channelAdminPrincipal(PermChannelsManage))
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body=%s)", rr.Code, rr.Body.String())
			}
			if performer.callCount() != 0 {
				t.Fatalf("performer called %d times on invalid request", performer.callCount())
			}
			if st.createCalls != 0 {
				t.Fatalf("Create called %d times on invalid request", st.createCalls)
			}
		})
	}

	t.Run("missing_channel_is_404", func(t *testing.T) {
		performer := &fakeChannelPerformer{results: knownPerfRuns()}
		env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
			Performer:   performer.run,
			Performance: &fakeModelPerformanceStore{},
		})
		req := jsonReq(t, http.MethodPost, "/api/channels/999/performance-tests",
			map[string]any{"model": "gpt-4o"})
		rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal(PermChannelsManage))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rr.Code, rr.Body.String())
		}
	})
}

// TestChannelPerformanceTestUnavailable 覆盖：Performer 或存储未装配 → 503，
// 且绝不返回伪造数据。
func TestChannelPerformanceTestUnavailable(t *testing.T) {
	t.Run("nil_performer", func(t *testing.T) {
		st := &fakeModelPerformanceStore{}
		env := newChannelPerfTestEnv(t, ChannelHandlerOptions{Performance: st})
		rr := postPerformanceTest(t, env,
			map[string]any{"model": "gpt-4o"}, channelAdminPrincipal(PermChannelsManage))
		assertPerformanceUnavailable(t, rr)
		if st.createCalls != 0 {
			t.Fatalf("Create called %d times without performer", st.createCalls)
		}
	})
	t.Run("nil_store", func(t *testing.T) {
		performer := &fakeChannelPerformer{results: knownPerfRuns()}
		env := newChannelPerfTestEnv(t, ChannelHandlerOptions{Performer: performer.run})
		rr := postPerformanceTest(t, env,
			map[string]any{"model": "gpt-4o"}, channelAdminPrincipal(PermChannelsManage))
		assertPerformanceUnavailable(t, rr)
		if performer.callCount() != 0 {
			t.Fatalf("performer called %d times without store", performer.callCount())
		}
	})
	t.Run("create_error_is_500_and_no_fabricated_body", func(t *testing.T) {
		performer := &fakeChannelPerformer{results: knownPerfRuns()}
		st := &fakeModelPerformanceStore{createErr: errors.New("db is down")}
		env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
			Performer:   performer.run,
			Performance: st,
		})
		rr := postPerformanceTest(t, env,
			map[string]any{"model": "gpt-4o", "runs": 4}, channelAdminPrincipal(PermChannelsManage))
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500 (body=%s)", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatalf("content-type = %q", rr.Header().Get("Content-Type"))
		}
	})
}

func assertPerformanceUnavailable(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
	}
	problem := decodeProblemBody(t, rr)
	if problem["type"] != "https://newtg.dev/problems/channel_performance_unavailable" {
		t.Fatalf("problem type = %v", problem["type"])
	}
}

// TestChannelPerformanceList 覆盖：GET 透传 model/limit，默认 limit=20，
// 返回 {items:[...]}；nil 存储 → 503。
func TestChannelPerformanceList(t *testing.T) {
	created := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	st := &fakeModelPerformanceStore{items: []store.ModelPerformanceTest{
		{ID: 2, ChannelID: 1, Model: "gpt-4o", Provider: "openai", Runs: 3, SuccessCount: 2, SuccessRate: 2.0 / 3.0, CreatedAt: created},
		{ID: 1, ChannelID: 1, Model: "gpt-4o", Provider: "openai", Runs: 3, SuccessCount: 1, SuccessRate: 1.0 / 3.0, CreatedAt: created.Add(-time.Hour)},
	}}
	env := newChannelPerfTestEnv(t, ChannelHandlerOptions{Performance: st})

	rr := serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels/1/performance-tests?model=gpt-4o&limit=7", nil),
		channelAdminPrincipal(PermChannelsRead))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	st.mu.Lock()
	gotChannel, gotModel, gotLimit := st.lastChannelID, st.lastModel, st.lastLimit
	st.mu.Unlock()
	if gotChannel != 1 || gotModel != "gpt-4o" || gotLimit != 7 {
		t.Fatalf("store args = (%d,%q,%d), want (1,gpt-4o,7)", gotChannel, gotModel, gotLimit)
	}
	var body struct {
		Items []struct {
			ID          int64   `json:"id"`
			SuccessRate float64 `json:"success_rate"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v (body=%s)", err, rr.Body.String())
	}
	if len(body.Items) != 2 || body.Items[0].ID != 2 || body.Items[1].ID != 1 {
		t.Fatalf("items = %+v", body.Items)
	}

	t.Run("default_limit_is_20", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/performance-tests", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.lastLimit != 20 {
			t.Fatalf("limit = %d, want 20", st.lastLimit)
		}
	})

	t.Run("invalid_limit_is_422", func(t *testing.T) {
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/performance-tests?limit=101", nil),
			channelAdminPrincipal(PermChannelsRead))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body=%s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("nil_store_is_503", func(t *testing.T) {
		env := newChannelPerfTestEnv(t, ChannelHandlerOptions{})
		rr := serveWithPrincipal(t, env.mux,
			httptest.NewRequest(http.MethodGet, "/api/channels/1/performance-tests", nil),
			channelAdminPrincipal(PermChannelsRead))
		assertPerformanceUnavailable(t, rr)
	})
}

// TestChannelPerformancePermissions 覆盖：POST 需要 channels:manage，GET 需要
// channels:read；非管理员即使持有权限字符串也 403。
func TestChannelPerformancePermissions(t *testing.T) {
	performer := &fakeChannelPerformer{results: knownPerfRuns()}
	st := &fakeModelPerformanceStore{}
	env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
		Performer:   performer.run,
		Performance: st,
	})

	legacy := &auth.Principal{UserID: testRegularID, Role: "user", Permissions: []string{PermChannelsManage, PermChannelsRead}}

	rr := postPerformanceTest(t, env, map[string]any{"model": "gpt-4o"}, legacy)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST as non-admin status = %d, want 403", rr.Code)
	}
	rr = serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels/1/performance-tests", nil), legacy)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET as non-admin status = %d, want 403", rr.Code)
	}

	// 管理员只有 channels:manage：GET 仍因缺 channels:read 被拒。
	rr = serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels/1/performance-tests", nil),
		channelAdminPrincipal(PermChannelsManage))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET without channels:read status = %d, want 403", rr.Code)
	}
	if performer.callCount() != 0 || st.createCalls != 0 {
		t.Fatalf("denied requests reached performer/store: %d/%d", performer.callCount(), st.createCalls)
	}
}

// TestChannelPerformanceTestRateLimit 覆盖：复用渠道测试桶，超限 429 且不调用
// Performer。
func TestChannelPerformanceTestRateLimit(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	performer := &fakeChannelPerformer{results: knownPerfRuns()}
	st := &fakeModelPerformanceStore{}
	env := newChannelPerfTestEnv(t, ChannelHandlerOptions{
		Performer:   performer.run,
		Performance: st,
		Redis:       rdb,
	})
	ctx := context.Background()
	_ = rdb.Del(ctx, ChannelTestRateLimitKey(testAdminID)).Err()

	for i := 0; i < ChannelTestRateLimit; i++ {
		rr := postPerformanceTest(t, env, map[string]any{"model": "gpt-4o"}, channelAdminPrincipal(PermChannelsManage))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d (body=%s)", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := postPerformanceTest(t, env, map[string]any{"model": "gpt-4o"}, channelAdminPrincipal(PermChannelsManage))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429 (body=%s)", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatalf("missing Retry-After header")
	}
	if performer.callCount() != ChannelTestRateLimit {
		t.Fatalf("performer calls = %d, want %d", performer.callCount(), ChannelTestRateLimit)
	}
}
