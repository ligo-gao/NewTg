package httpapi

// D6b 公开排行榜端点的单测：用内存 RankingsRepo 覆盖 period 校验、DTO 字段
// 与 NULL 口径、ETag/304、缓存、限流与 503。真实 SQL 读取由 D6a 的
// store/rankings 集成测试覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rankings"
	"newtg/gateway/internal/store"
)

// fakeRankingsRepo 是 store.RankingsRepo 的内存实现。
type fakeRankingsRepo struct {
	snap  store.RankingsSnapshot
	found bool
	err   error
	calls atomic.Int32
}

func (f *fakeRankingsRepo) LatestRun(context.Context, string) (store.RankingsSnapshot, bool, error) {
	f.calls.Add(1)
	if f.err != nil {
		return store.RankingsSnapshot{}, false, f.err
	}
	return f.snap, f.found, nil
}

func (f *fakeRankingsRepo) RunByID(context.Context, int64) (store.RankingsSnapshot, bool, error) {
	return f.snap, f.found, f.err
}

func (f *fakeRankingsRepo) DeleteOlderRuns(context.Context, string, int) (int64, error) {
	return 0, nil
}

type rankingsTestEnv struct {
	t      *testing.T
	router *Router
	rdb    *redis.Client
}

func newRankingsTestEnv(t *testing.T, cfg mapConfig, repo *fakeRankingsRepo, now func() time.Time) *rankingsTestEnv {
	t.Helper()
	if cfg == nil {
		cfg = mapConfig{}
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	handlers, err := NewRankingsHandlers(RankingsHandlerConfig{
		Repo: repo, Redis: rdb, Config: cfg, Now: now,
	})
	if err != nil {
		t.Fatalf("NewRankingsHandlers: %v", err)
	}

	rt, err := NewRouter(RouterConfig{
		Redis:    rdb,
		Sessions: auth.NewSessionManager(rdb, []byte(testSessionSecret)),
		Repo:     newFakeAuthRepo(),
		Config:   cfg,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	// 直接挂到根 mux：NewRouter 只有在提供 DB 时才自动装配排行榜仓储，
	// 单测用显式注入避免依赖数据库。
	handlers.RegisterRankingsRoutes(rt)
	return &rankingsTestEnv{t: t, router: rt, rdb: rdb}
}

func (e *rankingsTestEnv) get(path string, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return serve(e.router, req)
}

func intP(v int) *int             { return &v }
func int64P(v int64) *int64       { return &v }
func int16P(v int16) *int16       { return &v }
func stringP(v string) *string    { return &v }
func float64P(v float64) *float64 { return &v }

func testRankingsSnapshot(now time.Time) store.RankingsSnapshot {
	start := now.Add(-time.Hour)
	return store.RankingsSnapshot{
		Run: store.RankingsSnapshotRun{
			ID: 7, Period: "week", WindowStart: start, WindowEnd: now,
			SnapshotAt: now.Add(-180 * time.Second), SampleThreshold: 100,
			Status: "ok", ModelCount: 2,
		},
		Models: []store.RankingsModelStat{
			{
				RunID: 7, Model: "gpt-x", DisplayName: nil, Author: nil,
				TotalTokens: 1000, Requests: 10, Rank: intP(1), PrevRank: nil,
				DeltaTokens: int64P(1000), DeltaPct: nil, Slot: int16P(1),
			},
			{
				RunID: 7, Model: "claude-y", DisplayName: stringP("Claude Y"),
				Author: stringP("Anthropic"), TotalTokens: 500, Requests: 6,
				Rank: intP(2), PrevRank: intP(1), DeltaTokens: int64P(-100),
				DeltaPct: float64P(-16.67), Slot: int16P(2),
			},
		},
		Providers: []store.RankingsProviderStat{
			{RunID: 7, Author: "Anthropic", TotalTokens: 500, SharePct: float64P(33.33), Rank: intP(1), Slot: int16P(2)},
			{RunID: 7, Author: "Unknown", TotalTokens: 1000, SharePct: float64P(66.67), Rank: intP(2), Slot: nil},
		},
		Timeseries: []store.RankingsTimeseries{
			{Model: "gpt-x", Granularity: "day", BucketStart: start.Add(1 * time.Hour), Tokens: 400},
			{Model: "gpt-x", Granularity: "day", BucketStart: start.Add(25 * time.Hour), Tokens: 600},
			{Model: "claude-y", Granularity: "day", BucketStart: start.Add(1 * time.Hour), Tokens: 300},
			{Model: "claude-y", Granularity: "day", BucketStart: start.Add(25 * time.Hour), Tokens: 200},
		},
	}
}

func TestRankingsRejectsInvalidPeriod(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	env := newRankingsTestEnv(t, nil, &fakeRankingsRepo{}, func() time.Time { return now })
	rr := env.get("/api/rankings?period=decade", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
	if p := decodeProblem(t, rr); p.Type != authProblemBadRequest {
		t.Fatalf("problem.type = %q", p.Type)
	}
}

func TestRankingsDTOAndNullSemantics(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	repo := &fakeRankingsRepo{snap: testRankingsSnapshot(now), found: true}
	env := newRankingsTestEnv(t, mapConfig{
		"site.timezone":             configString("Asia/Shanghai"),
		"site.rankings_min_samples": configInt(120),
	}, repo, func() time.Time { return now })

	rr := env.get("/api/rankings?period=week", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != rankingsCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", got, rankingsCacheControl)
	}
	if got := rr.Header().Get("ETag"); got == "" || !strings.HasPrefix(got, `"week:`) {
		t.Fatalf("ETag = %q", got)
	}

	var body RankingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Period != "week" || body.Status != "ok" {
		t.Fatalf("period/status = %q/%q", body.Period, body.Status)
	}
	if body.Timezone != "Asia/Shanghai" {
		t.Fatalf("timezone = %q", body.Timezone)
	}
	// 窗口与 computed_at 以站点时区输出（UTC 04:00 = +08:00 12:00）。
	if body.Window.End != "2026-10-20T12:00:00+08:00" {
		t.Fatalf("window.end = %q", body.Window.End)
	}
	if body.ComputedAt == nil || *body.ComputedAt != "2026-10-20T11:57:00+08:00" {
		t.Fatalf("computed_at = %v", body.ComputedAt)
	}
	if body.FreshnessSeconds == nil || *body.FreshnessSeconds != 180 {
		t.Fatalf("freshness_seconds = %v", body.FreshnessSeconds)
	}
	if body.SampleThreshold != 100 {
		t.Fatalf("sample_threshold = %d (snapshot value wins)", body.SampleThreshold)
	}

	if len(body.Models) != 2 {
		t.Fatalf("models = %d", len(body.Models))
	}
	first := body.Models[0]
	if first.DisplayName != "gpt-x" {
		t.Fatalf("display_name 缺省应回退 model，得到 %q", first.DisplayName)
	}
	if first.Author != "Unknown" {
		t.Fatalf("author 缺省应回退 Unknown，得到 %q", first.Author)
	}
	if first.DeltaPct != nil {
		t.Fatalf("delta_pct 无上窗应为 null，得到 %v", *first.DeltaPct)
	}
	if first.DeltaTokens == nil || *first.DeltaTokens != 1000 {
		t.Fatalf("delta_tokens = %v", first.DeltaTokens)
	}
	if first.PrevRank != nil {
		t.Fatalf("prev_rank 无上窗应为 null")
	}
	second := body.Models[1]
	if second.DisplayName != "Claude Y" || second.Author != "Anthropic" {
		t.Fatalf("second model = %+v", second)
	}
	if second.DeltaPct == nil || *second.DeltaPct != -16.67 {
		t.Fatalf("second delta_pct = %v", second.DeltaPct)
	}

	// movers：gpt-x 上窗缺失不进榜；claude-y 下降项。
	if len(body.Movers.Up) != 0 || len(body.Movers.Down) != 1 {
		t.Fatalf("movers = %+v", body.Movers)
	}
	if body.Movers.Down[0].Model != "claude-y" || body.Movers.Down[0].PrevTokens != 600 {
		t.Fatalf("down mover = %+v", body.Movers.Down[0])
	}

	if len(body.Providers) != 2 {
		t.Fatalf("providers = %d", len(body.Providers))
	}
	// providers 不含 series（R-D6-05）：逐个对象的键集合里不得出现 series。
	var raw struct {
		Providers []map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	for i, provider := range raw.Providers {
		if _, ok := provider["series"]; ok {
			t.Fatalf("providers[%d] 不得含 series（R-D6-05）", i)
		}
	}

	if body.Trend.Granularity != "day" || len(body.Trend.Buckets) != 2 {
		t.Fatalf("trend = %+v", body.Trend)
	}
	if len(body.Trend.Series) != 2 {
		t.Fatalf("trend series = %d", len(body.Trend.Series))
	}
	// 每系列 tokens 与 buckets 等长；缺失点为 0。
	for _, series := range body.Trend.Series {
		if len(series.Tokens) != len(body.Trend.Buckets) {
			t.Fatalf("series %q tokens=%d buckets=%d", series.Model, len(series.Tokens), len(body.Trend.Buckets))
		}
	}
}

func TestRankingsETagAnd304(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	repo := &fakeRankingsRepo{snap: testRankingsSnapshot(now), found: true}
	env := newRankingsTestEnv(t, nil, repo, func() time.Time { return now })

	first := env.get("/api/rankings?period=week", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	second := env.get("/api/rankings?period=week", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("second status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 body = %q", second.Body.String())
	}
	if second.Header().Get("Cache-Control") != rankingsCacheControl {
		t.Fatalf("304 Cache-Control = %q", second.Header().Get("Cache-Control"))
	}
}

func TestRankingsServesFromCache(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	repo := &fakeRankingsRepo{snap: testRankingsSnapshot(now), found: true}
	env := newRankingsTestEnv(t, nil, repo, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if rr := env.get("/api/rankings?period=week", nil); rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rr.Code)
		}
	}
	if got := repo.calls.Load(); got != 1 {
		t.Fatalf("repo calls = %d, want 1（缓存命中后不应回源）", got)
	}
}

func TestRankingsColdStart(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	env := newRankingsTestEnv(t, mapConfig{
		"site.timezone":             configString("Asia/Shanghai"),
		"site.rankings_min_samples": configInt(55),
	}, &fakeRankingsRepo{found: false}, func() time.Time { return now })

	rr := env.get("/api/rankings", nil) // 默认 week
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body RankingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Period != "week" || body.Status != "empty" {
		t.Fatalf("period/status = %q/%q", body.Period, body.Status)
	}
	if body.ComputedAt != nil || body.FreshnessSeconds != nil {
		t.Fatalf("冷启动 computed_at/freshness 应为 null: %v/%v", body.ComputedAt, body.FreshnessSeconds)
	}
	if body.SampleThreshold != 55 {
		t.Fatalf("sample_threshold = %d, want 55", body.SampleThreshold)
	}
	if len(body.Models) != 0 || len(body.Providers) != 0 {
		t.Fatalf("冷启动应返回空数组")
	}
	if body.Movers.Up == nil || body.Movers.Down == nil || body.Trend.Buckets == nil {
		t.Fatal("冷启动数组字段必须是 [] 而不是 null")
	}
}

func TestRankingsSnapshotUnavailableReturns503(t *testing.T) {
	repo := &fakeRankingsRepo{err: errors.New("relation does not exist")}
	env := newRankingsTestEnv(t, nil, repo, nil)
	rr := env.get("/api/rankings?period=month", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 应带 Retry-After")
	}
}

func TestRankingsRateLimit(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	env := newRankingsTestEnv(t, nil, &fakeRankingsRepo{found: false}, func() time.Time { return now })
	// B8 桶键：IP+path（period 不参与键）。
	key := PublicReadRateLimitKey("192.0.2.1", "/api/rankings")
	if err := env.rdb.Set(context.Background(), key, PublicReadRateLimit, time.Minute).Err(); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	rr := env.get("/api/rankings?period=week", nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}
}

func TestRankingsTrendOtherAndSlot(t *testing.T) {
	now := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	start := now.Add(-2 * time.Hour)
	snap := store.RankingsSnapshot{
		Run: store.RankingsSnapshotRun{
			Period: "today", WindowStart: start, WindowEnd: now, SnapshotAt: now,
			SampleThreshold: 50, Status: "ok",
		},
	}
	// 8 个模型：前 6 进 series，第 7/8 归 Other（§C1.9 上限）。
	for i := 0; i < 8; i++ {
		model := "m" + string(rune('a'+i))
		snap.Models = append(snap.Models, store.RankingsModelStat{
			Model: model, TotalTokens: int64(100 - i), Requests: 10,
			Rank: intP(i + 1), Slot: int16P(int16(i + 1)),
		})
		snap.Timeseries = append(snap.Timeseries,
			store.RankingsTimeseries{Model: model, Granularity: "hour", BucketStart: start, Tokens: 10},
			store.RankingsTimeseries{Model: model, Granularity: "hour", BucketStart: now.Add(-time.Hour), Tokens: 20},
		)
	}
	// 非当前粒度的桶必须被忽略。
	snap.Timeseries = append(snap.Timeseries,
		store.RankingsTimeseries{Model: "ma", Granularity: "day", BucketStart: start, Tokens: 999})

	env := newRankingsTestEnv(t, nil, &fakeRankingsRepo{snap: snap, found: true},
		func() time.Time { return now })
	rr := env.get("/api/rankings?period=today", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body RankingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Trend.Granularity != "hour" {
		t.Fatalf("granularity = %q", body.Trend.Granularity)
	}
	if len(body.Trend.Series) != 7 {
		t.Fatalf("series = %d, want 6 + Other", len(body.Trend.Series))
	}
	other := body.Trend.Series[len(body.Trend.Series)-1]
	if other.Model != "Other" || other.Slot != nil {
		t.Fatalf("Other series = %+v", other)
	}
	if other.Tokens[0] != 20 || other.Tokens[1] != 40 {
		t.Fatalf("Other tokens = %v, want [20 40]", other.Tokens)
	}
	// 前 6 个系列的槽位来自实体注册表（1..6），不会被重排。
	for i := 0; i < 6; i++ {
		if body.Trend.Series[i].Slot == nil || *body.Trend.Series[i].Slot != i+1 {
			t.Fatalf("series[%d] slot = %v", i, body.Trend.Series[i].Slot)
		}
	}
	// 粒度常量由 period 派生（today→hour），即使 timeseries 里混入 day 行。
	if rankings.PeriodToday.Granularity() != rankings.GranularityHour {
		t.Fatal("D6a 粒度口径变更")
	}
}
