package billing

// live_feed 事件投影与投递的单测（D6b；modules-09 F-09-02）。
//
// 用 miniredis 验证 list/频道语义；脱敏用「敏感字面量不得出现在 JSON」的
// 白名单反向断言，而不是逐字段核对（新增字段时反向断言才会失败）。

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/contract"
)

func TestFeedEventRedactionAndMapping(t *testing.T) {
	row := usageLogRow{
		requestID:        "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a",
		model:            "claude-3-5-sonnet",
		promptTokens:     1234,
		completionTokens: 567,
		latencyMS:        sql.NullInt64{Int64: 890, Valid: true},
	}
	rc := &contract.RelayContext{
		RequestID:   row.requestID,
		UserID:      4242,
		ChannelID:   777,
		ChannelType: contract.ChannelTypeAnthropic,
		APIBase:     "https://internal-upstream.example/v1",
		Credential:  "sk-SENSITIVE-KEY",
		RequestPath: "/v1/messages",
		OriginModel: row.model,
	}
	event := buildFeedEvent(row, rc, SettlementOutcome{Status: UsageLogStatusSuccess},
		time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC))

	if len(event.ID) != 8 {
		t.Fatalf("id length = %d, want 8", len(event.ID))
	}
	if _, err := hex.DecodeString(event.ID); err != nil {
		t.Fatalf("id %q 不是十六进制: %v", event.ID, err)
	}
	if event.Upstream != "anthropic" {
		t.Fatalf("upstream = %q", event.Upstream)
	}
	if event.TokensIn != 1234 || event.TokensOut != 567 || event.LatencyMS != 890 {
		t.Fatalf("event = %+v", event)
	}
	if event.Status != "ok" {
		t.Fatalf("status = %q", event.Status)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(payload)
	for _, forbidden := range []string{
		"4242", "777", "internal-upstream", "SENSITIVE", "sk-", "/v1/messages",
		row.requestID, "user_id", "channel_id", "base_url", "credential",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("事件泄露敏感值 %q: %s", forbidden, body)
		}
	}
	// 白名单字段之外不应出现额外键（防止未来把 row 直接序列化）。
	var keys map[string]any
	if err := json.Unmarshal(payload, &keys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	allowed := map[string]struct{}{
		"id": {}, "ts": {}, "model": {}, "upstream": {}, "tokens_in": {},
		"tokens_out": {}, "latency_ms": {}, "status": {},
	}
	for key := range keys {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("未预期的公开字段 %q", key)
		}
	}
	// 裁决 A-1：逐行事件不含任何费用字段，CostCredits 只走 json:"-"。
	if strings.Contains(body, "cost") || strings.Contains(body, "credit") {
		t.Fatalf("逐行事件泄露费用字段: %s", body)
	}
}

func TestFeedUpstreamFamily(t *testing.T) {
	cases := []struct {
		kind contract.ChannelType
		want string
	}{
		{contract.ChannelTypeUnknown, "unknown"},
		{contract.ChannelTypeAnthropic, "anthropic"},
		{contract.ChannelTypeMoonshot, "anthropic"},
		{contract.ChannelTypeGemini, "gemini"},
		{contract.ChannelTypeVertexAi, "gemini"},
		{contract.ChannelTypeOpenAI, "openai"},
		{contract.ChannelTypeAzure, "openai"},
	}
	for _, tc := range cases {
		if got := feedUpstreamFamily(tc.kind); got != tc.want {
			t.Fatalf("%q → %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestFeedEventErrorStatus(t *testing.T) {
	row := usageLogRow{requestID: "x", model: "m"}
	event := buildFeedEvent(row, &contract.RelayContext{}, SettlementOutcome{Status: UsageLogStatusError}, time.Now())
	if event.Status != "error" {
		t.Fatalf("status = %q, want error", event.Status)
	}
	if event.Upstream != "unknown" {
		t.Fatalf("无渠道来源 upstream = %q, want unknown", event.Upstream)
	}
	if event.LatencyMS != 0 {
		t.Fatalf("NULL latency = %d, want 0", event.LatencyMS)
	}
}

func TestRedisFeedPublisherListTrimAndChannel(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	pubsub := rdb.Subscribe(ctx, FeedChannel)
	t.Cleanup(func() { _ = pubsub.Close() })
	// 等待订阅确认（Receive 读到 subscribe 确认帧）。
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// 预置 199 条，再投递 2 条，验证 LTRIM 只保留最近 200 条。
	for i := 0; i < FeedListMax-1; i++ {
		if err := rdb.LPush(ctx, FeedListKey, `{"id":"seed"}`).Err(); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	pub := NewRedisFeedPublisher(rdb)
	for i := 0; i < 2; i++ {
		if err := pub.PublishCall(ctx, FeedEvent{ID: "abcd0000", Model: "m", Status: "ok"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	if got := rdb.LLen(ctx, FeedListKey).Val(); got != FeedListMax {
		t.Fatalf("list length = %d, want %d", got, FeedListMax)
	}

	msg := <-pubsub.Channel()
	if msg.Channel != FeedChannel {
		t.Fatalf("channel = %q", msg.Channel)
	}
	var event FeedEvent
	if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
		t.Fatalf("decode published: %v", err)
	}
	if event.Model != "m" || event.ID != "abcd0000" {
		t.Fatalf("published event = %+v", event)
	}
}

func TestRedisFeedPublisherNilClient(t *testing.T) {
	pub := NewRedisFeedPublisher(nil)
	if err := pub.PublishCall(context.Background(), FeedEvent{}); err == nil {
		t.Fatal("nil redis client 应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 聚合条窗口（F-09-02 / A6 冻结语义）
// ---------------------------------------------------------------------------

func newFeedWindowTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// 投递侧：calls 计所有事件，cost 只计成功事件；桶带 10 分钟 TTL。
func TestRedisFeedPublisherMaintainsWindowBuckets(t *testing.T) {
	mr, rdb := newFeedWindowTestRedis(t)
	ctx := context.Background()
	fixed := time.Date(2026, 10, 20, 4, 7, 30, 0, time.UTC)

	pub := NewRedisFeedPublisher(rdb)
	pub.now = func() time.Time { return fixed }

	cases := []FeedEvent{
		{ID: "ok000001", Model: "m", Status: "ok", CostCredits: 30},
		// 失败事件即使带着（不应存在的）费用也只计 calls。
		{ID: "err00002", Model: "m", Status: "error", CostCredits: 999},
		{ID: "ok000003", Model: "m", Status: "ok", CostCredits: 20},
	}
	for _, event := range cases {
		if err := pub.PublishCall(ctx, event); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	window, err := ReadFeedWindow(ctx, rdb, fixed)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if window.Calls != 3 || window.CostCredits != 50 {
		t.Fatalf("window = %+v, want calls=3 cost=50", window)
	}

	minute := FeedWindowMinute(fixed)
	for _, key := range []string{FeedWindowCallsKey(minute), FeedWindowCostKey(minute)} {
		if ttl := mr.TTL(key); ttl != FeedWindowBucketTTL {
			t.Fatalf("%s TTL = %s, want %s", key, ttl, FeedWindowBucketTTL)
		}
	}
}

// 读取侧：只汇总当前分钟桶（F-09-02 近 60 秒近似）；前一分钟、更早与
// 未来桶一律忽略。
func TestReadFeedWindowOnlyCountsCurrentMinuteBucket(t *testing.T) {
	_, rdb := newFeedWindowTestRedis(t)
	ctx := context.Background()
	fixed := time.Date(2026, 10, 20, 4, 7, 30, 0, time.UTC)
	minute := FeedWindowMinute(fixed)

	if FeedWindowMinutes != 1 {
		t.Fatalf("FeedWindowMinutes = %d, want 1（近 60 秒冻结语义）", FeedWindowMinutes)
	}

	// minute → (calls, cost)
	seed := map[int64][2]int64{
		minute:     {2, 70},   // 当前分钟：唯一计入
		minute - 1: {99, 999}, // 上一分钟：窗口外
		minute - 4: {5, 100},  // 更早：窗口外
		minute + 1: {7, 7},    // 未来桶，不可信也不计入
	}
	for offset, values := range seed {
		if err := rdb.Set(ctx, FeedWindowCallsKey(offset), values[0], 0).Err(); err != nil {
			t.Fatalf("seed calls: %v", err)
		}
		if err := rdb.Set(ctx, FeedWindowCostKey(offset), values[1], 0).Err(); err != nil {
			t.Fatalf("seed cost: %v", err)
		}
	}

	window, err := ReadFeedWindow(ctx, rdb, fixed)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if window.Calls != 2 || window.CostCredits != 70 {
		t.Fatalf("window = %+v, want calls=2 cost=70（仅当前分钟桶）", window)
	}
}

// 坏桶（手工写入/版本升级）按 0 处理，不能把公开列表变成 5xx。
func TestReadFeedWindowToleratesCorruptBuckets(t *testing.T) {
	_, rdb := newFeedWindowTestRedis(t)
	ctx := context.Background()
	fixed := time.Date(2026, 10, 20, 4, 7, 30, 0, time.UTC)
	minute := FeedWindowMinute(fixed)
	if err := rdb.Set(ctx, FeedWindowCallsKey(minute), "not-a-number", 0).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := rdb.Set(ctx, FeedWindowCostKey(minute), "-5", 0).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	window, err := ReadFeedWindow(ctx, rdb, fixed)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if window.Calls != 0 || window.CostCredits != 0 {
		t.Fatalf("坏桶应按 0: %+v", window)
	}
}

// buildFeedEvent 只把成功结算的 cost 带入投递侧聚合，且绝不进 JSON。
func TestBuildFeedEventCarriesCostOnlyForSuccess(t *testing.T) {
	row := usageLogRow{requestID: "x", model: "m"}
	event := buildFeedEvent(row, &contract.RelayContext{},
		SettlementOutcome{Status: UsageLogStatusSuccess, Cost: 42}, time.Now())
	if event.CostCredits != 42 {
		t.Fatalf("success CostCredits = %d, want 42", event.CostCredits)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), "cost") {
		t.Fatalf("CostCredits 不得序列化到公开事件: %s", payload)
	}

	for _, status := range []int16{UsageLogStatusError, UsageLogStatusRefused, UsageLogStatusRefunded} {
		event := buildFeedEvent(row, &contract.RelayContext{},
			SettlementOutcome{Status: status, Cost: 42, RetainedCost: 42}, time.Now())
		if event.CostCredits != 0 {
			t.Fatalf("status=%d CostCredits = %d, want 0", status, event.CostCredits)
		}
	}
}
