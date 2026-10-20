package httpapi

// D6b live_feed 端点的单测：轮询 list 的 limit/脱敏透传/404/429，以及 SSE
// 的事件转发、15s 心跳（测试用短间隔）、单 IP 并发租约与释放。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing"
)

type liveFeedTestEnv struct {
	t        *testing.T
	router   *Router
	rdb      *redis.Client
	handlers *LiveFeedHandlers
}

func newLiveFeedTestEnv(t *testing.T, cfg mapConfig, mutate func(*LiveFeedHandlerConfig)) *liveFeedTestEnv {
	t.Helper()
	if cfg == nil {
		cfg = mapConfig{}
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	handlerCfg := LiveFeedHandlerConfig{Redis: rdb, Config: cfg}
	if mutate != nil {
		mutate(&handlerCfg)
	}
	handlers, err := NewLiveFeedHandlers(handlerCfg)
	if err != nil {
		t.Fatalf("NewLiveFeedHandlers: %v", err)
	}
	// 进程级 Pub/Sub 订阅是后台资源：每个测试结束必须关闭，否则协程与
	// Redis 订阅会跨测试泄漏（-count=200 时尤其明显）。LIFO 顺序保证先
	// 关订阅再关客户端。
	t.Cleanup(func() { _ = handlers.Close() })
	// 只挂 live_feed 路由的根 mux：测试需要自定义心跳/租约间隔，若走
	// NewRouter 的默认装配会出现重复注册。
	rt := &Router{mux: http.NewServeMux(), protected: http.NewServeMux()}
	rt.handler = RequestIDMiddleware(noStoreAPI(rt.mux))
	handlers.RegisterLiveFeedRoutes(rt)
	return &liveFeedTestEnv{t: t, router: rt, rdb: rdb, handlers: handlers}
}

func (e *liveFeedTestEnv) get(path string) *httptest.ResponseRecorder {
	e.t.Helper()
	return serve(e.router, httptest.NewRequest(http.MethodGet, path, http.NoBody))
}

func seedLiveFeedEvents(t *testing.T, rdb *redis.Client, count int) []billing.FeedEvent {
	t.Helper()
	events := make([]billing.FeedEvent, 0, count)
	for i := 0; i < count; i++ {
		events = append(events, billing.FeedEvent{
			ID:    fmt.Sprintf("%08x", i),
			TS:    "2026-10-20T04:00:00Z",
			Model: fmt.Sprintf("model-%03d", i), Upstream: "openai",
			TokensIn: 100 + i, TokensOut: 50 + i, LatencyMS: int64(200 + i), Status: "ok",
		})
	}
	pub := billing.NewRedisFeedPublisher(rdb)
	for _, event := range events {
		if err := pub.PublishCall(context.Background(), event); err != nil {
			t.Fatalf("publish event: %v", err)
		}
	}
	return events
}

func TestLiveFeedListNewestFirstAndLimit(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	seedLiveFeedEvents(t, env.rdb, 25)

	rr := env.get("/api/home/live-feed?limit=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body LiveFeedListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(body.Events))
	}
	// LPUSH 让最新事件在 list 头部；最后发布的 model-024 应排第一。
	var first billing.FeedEvent
	if err := json.Unmarshal(body.Events[0], &first); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if first.Model != "model-024" {
		t.Fatalf("first event model = %q, want model-024（新在上）", first.Model)
	}
}

func TestLiveFeedDefaultLimitAndClamp(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	seedLiveFeedEvents(t, env.rdb, 105)

	rr := env.get("/api/home/live-feed")
	if rr.Code != http.StatusOK {
		t.Fatalf("default status = %d", rr.Code)
	}
	var body LiveFeedListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != liveFeedDefaultLimit {
		t.Fatalf("default events = %d, want %d", len(body.Events), liveFeedDefaultLimit)
	}

	rr = env.get("/api/home/live-feed?limit=500")
	if rr.Code != http.StatusOK {
		t.Fatalf("clamp status = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode clamp: %v", err)
	}
	if len(body.Events) != liveFeedMaxLimit {
		t.Fatalf("clamped events = %d, want %d", len(body.Events), liveFeedMaxLimit)
	}
}

func TestLiveFeedInvalidLimit(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	for _, limit := range []string{"abc", "0", "-1"} {
		rr := env.get("/api/home/live-feed?limit=" + limit)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s status = %d, want 400", limit, rr.Code)
		}
	}
}

func TestLiveFeedDisabledReturns404(t *testing.T) {
	cfg := mapConfig{"site.home_blocks": json.RawMessage(`{"hero":true,"live_feed":false}`)}
	env := newLiveFeedTestEnv(t, cfg, nil)
	for _, path := range []string{"/api/home/live-feed", "/api/home/live-feed/stream"} {
		rr := env.get(path)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rr.Code)
		}
	}
}

func TestLiveFeedEmptyListIsArray(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	rr := env.get("/api/home/live-feed")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"events":[]`) {
		t.Fatalf("空列表应为 []，body=%s", rr.Body.String())
	}
}

func TestLiveFeedRateLimit(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	key := PublicReadRateLimitKey("192.0.2.1", "/api/home/live-feed")
	if err := env.rdb.Set(context.Background(), key, PublicReadRateLimit, time.Minute).Err(); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	rr := env.get("/api/home/live-feed")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

// streamRecorder 是并发安全的 ResponseWriter，用于在测试协程里读取 SSE 帧。
// 标准 httptest.ResponseRecorder 不保证并发读写安全。
type streamRecorder struct {
	mu     sync.Mutex
	header http.Header
	status int
	buf    bytes.Buffer
	wrote  chan struct{}
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header), wrote: make(chan struct{}, 64)}
}

func (s *streamRecorder) Header() http.Header { return s.header }

func (s *streamRecorder) WriteHeader(code int) {
	s.mu.Lock()
	s.status = code
	s.mu.Unlock()
	s.signal()
}

func (s *streamRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.buf.Write(p)
	s.mu.Unlock()
	s.signal()
	return n, err
}

func (s *streamRecorder) Flush() { s.signal() }

func (s *streamRecorder) signal() {
	select {
	case s.wrote <- struct{}{}:
	default:
	}
}

func (s *streamRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *streamRecorder) statusCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// startStream 启动一个 SSE 请求并返回取消函数与完成信号。
func startStream(t *testing.T, rt *Router) (*streamRecorder, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/home/live-feed/stream", http.NoBody).WithContext(ctx)
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.ServeHTTP(rec, req)
	}()
	return rec, cancel, done
}

func waitForLeaseCount(t *testing.T, rdb *redis.Client, ip string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := rdb.ZCard(context.Background(), LiveFeedLeaseKey(ip)).Result()
		if err == nil && n == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	n, _ := rdb.ZCard(context.Background(), LiveFeedLeaseKey(ip)).Result()
	t.Fatalf("lease count = %d, want %d", n, want)
}

func waitForBody(t *testing.T, rec *streamRecorder, substr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.body(), substr) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("SSE body 未出现 %q；body=%q", substr, rec.body())
}

// waitForStatus 等处理协程写出状态行。M1 的契约是「订阅 ack 之后才写
// 200」，所以状态行出现即证明订阅已生效，可以安全发布测试事件。
func waitForStatus(t *testing.T, rec *streamRecorder, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if code := rec.statusCode(); code != 0 {
			if code != want {
				t.Fatalf("SSE status = %d, want %d", code, want)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("SSE 未写出状态码（want %d）", want)
}

// publishUntilSeen 反复发布同一事件直到 SSE 体出现该帧。
//
// 单次 Publish 与订阅侧扇出之间没有跨协程的同步点（Redis ack 只证明
// SUBSCRIBE 生效，不证明读循环已开始），高 -count 下偶发丢帧会让断言
// flake。重发只影响测试，断言仍是「事件最终到达」这一真实语义。
func publishUntilSeen(t *testing.T, rdb *redis.Client, rec *streamRecorder, payload []byte) {
	t.Helper()
	want := "event: call\ndata: " + string(payload) + "\n\n"
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(rec.body(), want) {
			return
		}
		if err := rdb.Publish(context.Background(), billing.FeedChannel, payload).Err(); err != nil {
			t.Fatalf("publish: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("SSE body 未出现事件 %q；body=%q", string(payload), rec.body())
}

func waitForDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler 未在期限内退出")
	}
}

func TestLiveFeedStreamForwardsEventsAndHeartbeat(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, func(cfg *LiveFeedHandlerConfig) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.LeaseTTL = 200 * time.Millisecond
	})
	rec, cancel, done := startStream(t, env.router)
	defer func() { cancel(); <-done }()

	waitForLeaseCount(t, env.rdb, "192.0.2.1", 1)
	// 200 在订阅 ack 之后才写出（M1）：等到状态行再发布，避免把「订阅
	// 尚未生效」误判成丢事件。publishUntilSeen 再兜住扇出时序。
	waitForStatus(t, rec, http.StatusOK)
	payload := []byte(`{"id":"a1b2c3d4","ts":"2026-10-20T04:00:00Z","model":"gpt-4o","upstream":"openai","tokens_in":10,"tokens_out":5,"latency_ms":100,"status":"ok"}`)
	publishUntilSeen(t, env.rdb, rec, payload)

	waitForBody(t, rec, ": heartbeat\n\n")

	if rec.statusCode() != http.StatusOK && rec.statusCode() != 0 {
		t.Fatalf("status = %d", rec.statusCode())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}

	cancel()
	<-done
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 0)
	// 恢复 defer 里的二次 cancel/等待（幂等）。
	cancel = func() {}
}

func TestLiveFeedStreamRejectsSixthConnectionAndReleases(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, func(cfg *LiveFeedHandlerConfig) {
		cfg.MaxConnections = 2 // 测试用 2，等价于生产 5 的语义
		cfg.HeartbeatInterval = time.Second
		cfg.LeaseTTL = 5 * time.Second
	})

	rec1, cancel1, done1 := startStream(t, env.router)
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 1)
	rec2, cancel2, done2 := startStream(t, env.router)
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 2)
	_ = rec1
	_ = rec2

	// 第 3 条连接：同 IP 超过上限 → 429（非流式响应，立即返回）。
	rr := env.get("/api/home/live-feed/stream")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("third connection status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 应带 Retry-After")
	}

	// 释放一条后，名额回到上限，新连接可以进入。
	cancel1()
	<-done1
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 1)

	_, cancel3, done3 := startStream(t, env.router)
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 2)
	cancel2()
	cancel3()
	<-done2
	<-done3
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 0)
}

func TestLiveFeedStreamLeaseExpiresWithoutRenewal(t *testing.T) {
	// 模拟进程崩溃：租约键还在，但成员分数已过期；新连接应能占用名额。
	env := newLiveFeedTestEnv(t, nil, func(cfg *LiveFeedHandlerConfig) {
		cfg.HeartbeatInterval = time.Second
		cfg.LeaseTTL = 30 * time.Millisecond
	})
	key := LiveFeedLeaseKey("192.0.2.1")
	expiredScore := float64(time.Now().Add(-time.Minute).UnixMilli())
	if err := env.rdb.ZAdd(context.Background(), key, redis.Z{Score: expiredScore, Member: "dead"}).Err(); err != nil {
		t.Fatalf("seed stale lease: %v", err)
	}
	rec, cancel, done := startStream(t, env.router)
	defer func() {
		cancel()
		<-done
	}()
	waitForLeaseCount(t, env.rdb, "192.0.2.1", 1)
	if strings.Contains(rec.body(), "problem") {
		t.Fatalf("过期租约不应阻止新连接: %s", rec.body())
	}
}

// M4：list 的唯一数据源不可用时必须 503，不能返回 200 空数组。
func TestLiveFeedListRedisFailureReturns503(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	if err := env.rdb.Close(); err != nil {
		t.Fatalf("close redis client: %v", err)
	}
	rr := env.get("/api/home/live-feed")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 应带 Retry-After")
	}
}

// M4：租约依赖 Redis；无法判定并发数时必须 fail-closed 返回 503。
func TestLiveFeedStreamLeaseFailureReturns503(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	if err := env.rdb.Close(); err != nil {
		t.Fatalf("close redis client: %v", err)
	}
	rr := env.get("/api/home/live-feed/stream")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 应带 Retry-After")
	}
}

// M4：心跳续期让租约在 TTL 之后仍然有效；键被淘汰后续期返回 0，主动断流。
func TestLiveFeedStreamRenewsLeaseAndDropsOnEviction(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, func(cfg *LiveFeedHandlerConfig) {
		cfg.HeartbeatInterval = 20 * time.Millisecond
		cfg.LeaseTTL = 200 * time.Millisecond
	})
	key := LiveFeedLeaseKey("192.0.2.1")
	rec, cancel, done := startStream(t, env.router)
	defer cancel()

	waitForLeaseCount(t, env.rdb, "192.0.2.1", 1)
	waitForStatus(t, rec, http.StatusOK)

	// 超过两倍 TTL 后租约仍应存在（每次心跳续期），证明续期而不是"过期未回收"。
	time.Sleep(450 * time.Millisecond)
	n, err := env.rdb.ZCard(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("zcard: %v", err)
	}
	if n != 1 {
		t.Fatalf("lease count = %d, want 1（续期应保持租约）", n)
	}

	// 键被淘汰：下一次心跳的续期脚本返回 0，handler 必须主动结束流。
	if err := env.rdb.Del(context.Background(), key).Err(); err != nil {
		t.Fatalf("evict lease key: %v", err)
	}
	waitForDone(t, done)
}

// ---------------------------------------------------------------------------
// 聚合条 window（F-09-02 / A6 冻结语义）
// ---------------------------------------------------------------------------

// 聚合条与事件列表共存；逐行事件仍不含任何费用字段。
//
// 【固定时钟】窗口冻结为单个当前分钟桶（近 60 秒近似）后，用真实时钟
// 投递再读取会跨分钟概率性地读到 0；这里给 handler 注入固定时间并直接
// 写该分钟桶。calls/cost 的计数规则（成功计费、失败只计 calls）由
// billing.TestRedisFeedPublisherMaintainsWindowBuckets 用固定时钟覆盖。
func TestLiveFeedListIncludesWindowAggregate(t *testing.T) {
	fixed := time.Date(2026, 10, 20, 4, 0, 30, 0, time.UTC)
	env := newLiveFeedTestEnv(t, nil, func(cfg *LiveFeedHandlerConfig) {
		cfg.Now = func() time.Time { return fixed }
	})
	ctx := context.Background()
	seedLiveFeedEvents(t, env.rdb, 3)
	minute := billing.FeedWindowMinute(fixed)
	if err := env.rdb.Set(ctx, billing.FeedWindowCallsKey(minute), 3, 0).Err(); err != nil {
		t.Fatalf("seed calls bucket: %v", err)
	}
	if err := env.rdb.Set(ctx, billing.FeedWindowCostKey(minute), 42, 0).Err(); err != nil {
		t.Fatalf("seed cost bucket: %v", err)
	}

	rr := env.get("/api/home/live-feed?limit=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body LiveFeedListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 2 {
		t.Fatalf("events = %d, want 2（limit 与 window 并存）", len(body.Events))
	}
	if body.Window.Calls != 3 || body.Window.CostCredits != 42 {
		t.Fatalf("window = %+v, want calls=3 cost=42", body.Window)
	}
	// A-1：逐行事件不得出现费用字段；费用只允许出现在顶层 window。
	for _, raw := range body.Events {
		if strings.Contains(string(raw), "cost") || strings.Contains(string(raw), "credit") {
			t.Fatalf("逐行事件泄露费用: %s", raw)
		}
	}
	if !strings.Contains(rr.Body.String(), `"window":{"calls":3,"cost_credits":42}`) {
		t.Fatalf("响应缺少 window 聚合条: %s", rr.Body.String())
	}
}

// window 读取失败时按冻结语义降级为 0/0，事件照常返回（list 仍可用）。
func TestLiveFeedListWindowFailureDegradesToZero(t *testing.T) {
	env := newLiveFeedTestEnv(t, nil, nil)
	seedLiveFeedEvents(t, env.rdb, 3)
	env.rdb.AddHook(failMGetHook{})

	rr := env.get("/api/home/live-feed")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（window 故障不应拖垮事件列表）", rr.Code)
	}
	var body LiveFeedListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(body.Events))
	}
	if body.Window.Calls != 0 || body.Window.CostCredits != 0 {
		t.Fatalf("window = %+v, want 0/0", body.Window)
	}
}

// failMGetHook 只让 MGET 失败，模拟「list 可读、聚合桶不可读」的部分故障。
type failMGetHook struct{}

func (failMGetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failMGetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if strings.EqualFold(cmd.Name(), "mget") {
			return errors.New("simulated mget failure")
		}
		return next(ctx, cmd)
	}
}

func (failMGetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
