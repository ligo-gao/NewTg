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
