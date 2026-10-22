package billing

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/settings"
)

// 本文件是 W10-N2 批处理链路的纯单测（不需要 PG/Redis）。
// 集成验收见 logstream_integration_test.go。

// captureQueue 是记录事件的 fake 生产者。
type captureQueue struct {
	events []UsageLogEvent
	err    error
}

func (q *captureQueue) Enqueue(_ context.Context, event UsageLogEvent) error {
	q.events = append(q.events, event)
	return q.err
}

// TestSaturateEnqueueSuccessSkipsSyncWrite 验证 XADD 成功即受理：
// 日志库句柄存在但不可用（未建连），若代码误走同步路径会立刻报错。
func TestSaturateEnqueueSuccessSkipsSyncWrite(t *testing.T) {
	// sql.Open 只解析 DSN，不建连；只要不打到 DB，句柄是否可达无关。
	db, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	fixed := time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC)
	queue := &captureQueue{}
	logger := NewDBLogger(db).WithBatchQueue(queue, nil)
	logger.now = func() time.Time { return fixed }

	outcome := SettlementOutcome{
		Cost: 321, PreDeducted: 500, Refunded: 179, Status: UsageLogStatusSuccess,
	}
	rc := testRelayContext()
	rc.ChannelType = contract.ChannelTypeOpenAI
	if err := logger.Saturate(context.Background(), testUsageFact(), rc, outcome); err != nil {
		t.Fatalf("Saturate with queue: %v", err)
	}
	if len(queue.events) != 1 {
		t.Fatalf("enqueued events = %d, want 1", len(queue.events))
	}
	event := queue.events[0]
	if event.RequestID != testRelayContext().RequestID {
		t.Fatalf("request_id = %q", event.RequestID)
	}
	// created_at 必须是事件时刻并显式带出（否则月分区会漂移到冲刷时刻）。
	if !event.CreatedAt.Equal(fixed) {
		t.Fatalf("created_at = %s, want %s", event.CreatedAt, fixed)
	}
	if !event.FeedEligible {
		t.Fatal("feed_eligible = false, want true for a real usage fact")
	}
	if event.FeedUpstream != "openai" {
		t.Fatalf("feed_upstream = %q, want openai", event.FeedUpstream)
	}
	if event.Cost != 321 || event.PreDeducted != 500 || event.Refunded != 179 {
		t.Fatalf("amounts = cost:%d pre:%d refunded:%d", event.Cost, event.PreDeducted, event.Refunded)
	}
	if event.PromptTokens != 1000 || event.CompletionTokens != 200 {
		t.Fatalf("tokens = %d/%d", event.PromptTokens, event.CompletionTokens)
	}
}

// TestSaturateEnqueueFailureFallsBackToSync 验证 XADD 失败后会走到同步写入：
// 用不可用的日志库触发同步路径的明确错误（证明不是直接返回队列错误）。
func TestSaturateEnqueueFailureFallsBackToSync(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	queue := &captureQueue{err: errors.New("redis down")}
	logger := NewDBLogger(db).WithBatchQueue(queue, nil)
	err = logger.Saturate(context.Background(), testUsageFact(), testRelayContext(),
		SettlementOutcome{Cost: 1, Status: UsageLogStatusSuccess})
	if err == nil {
		t.Fatal("Saturate = nil, want the synchronous write error after enqueue failure")
	}
	// 错误必须来自同步写入（begin tx），而不是队列错误本身。
	if errors.Is(err, queue.err) {
		t.Fatalf("Saturate returned the queue error directly (%v); fallback did not run", err)
	}
	if len(queue.events) != 1 {
		t.Fatalf("enqueue attempts = %d, want 1", len(queue.events))
	}
}

// TestUsageLogEventRoundTrip 保证传输表示不丢字段。
func TestUsageLogEventRoundTrip(t *testing.T) {
	row, err := buildUsageLogRow(testUsageFact(), testRelayContext(), SettlementOutcome{
		Cost: 10, PreDeducted: 20, Refunded: 10, Status: UsageLogStatusSuccess,
		ErrorClass: "upstream_timeout", ErrorMessage: "deadline exceeded",
	})
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	createdAt := time.Date(2026, 3, 15, 23, 59, 59, 0, time.UTC)
	event := eventFromUsageLogRow(row, createdAt, true, "anthropic")
	got := event.usageLogRow()

	if got.requestID != row.requestID || got.model != row.model || got.group != row.group {
		t.Fatalf("identity fields drifted: %+v vs %+v", got, row)
	}
	if got.userID != row.userID || got.cost != row.cost || got.refunded != row.refunded ||
		got.preDeducted != row.preDeducted {
		t.Fatalf("amount fields drifted: %+v vs %+v", got, row)
	}
	if got.promptTokens != row.promptTokens || got.completionTokens != row.completionTokens ||
		got.cacheReadTokens != row.cacheReadTokens || got.cacheWriteTokens != row.cacheWriteTokens {
		t.Fatalf("token fields drifted: %+v vs %+v", got, row)
	}
	if got.stream != row.stream || got.status != row.status {
		t.Fatalf("status fields drifted: %+v vs %+v", got, row)
	}
	if got.tokenID != row.tokenID || got.channelID != row.channelID ||
		got.latencyMS != row.latencyMS || got.firstTokenMS != row.firstTokenMS {
		t.Fatalf("nullable fields drifted: %+v vs %+v", got, row)
	}
	if got.errorClass != row.errorClass || got.errorMessage != row.errorMessage {
		t.Fatalf("error fields drifted: %+v vs %+v", got, row)
	}
	if got.other != row.other {
		t.Fatalf("other drifted: %q vs %q", got.other, row.other)
	}
	if !event.CreatedAt.Equal(createdAt) {
		t.Fatalf("created_at = %s, want %s", event.CreatedAt, createdAt)
	}
}

// TestUsageLogQueueAddArgsFreezesMaxLen 断言 MAXLEN ~100000 契约（R-W10-11）。
func TestUsageLogQueueAddArgsFreezesMaxLen(t *testing.T) {
	q := NewRedisUsageLogQueue(nil)
	args := q.addArgs([]byte(`{"request_id":"x"}`))
	if args.Stream != UsageLogStreamKey {
		t.Fatalf("stream = %q, want %q", args.Stream, UsageLogStreamKey)
	}
	if args.MaxLen != UsageLogStreamMaxLen || UsageLogStreamMaxLen != 100000 {
		t.Fatalf("maxlen = %d, want 100000", args.MaxLen)
	}
	if !args.Approx {
		t.Fatal("Approx = false, want approximate MAXLEN trim")
	}
	values, ok := args.Values.([]any)
	if !ok || len(values) != 2 || values[0] != "event" {
		t.Fatalf("values = %#v", args.Values)
	}

	// 测试接缝必须覆盖默认值，否则「MAXLEN 被删掉」的变异无法被断言抓住。
	q.maxLen = 4
	if got := q.addArgs(nil).MaxLen; got != 4 {
		t.Fatalf("maxLen override = %d, want 4", got)
	}
}

// TestReduceUsageLogBatchDedupsWithinBatch 验证批内去重与退款归并。
func TestReduceUsageLogBatchDedupsWithinBatch(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	events := []UsageLogEvent{
		{RequestID: "b", Status: UsageLogStatusSuccess, Cost: 10, CreatedAt: t0},
		{RequestID: "a", Status: UsageLogStatusSuccess, Cost: 20, CreatedAt: t0},
		// 同一请求的重复成功事件：只保留第一条。
		{RequestID: "a", Status: UsageLogStatusSuccess, Cost: 999, CreatedAt: t0.Add(time.Second)},
		// 同一请求的退款事件：保留金额更大的一条。
		{RequestID: "a", Status: UsageLogStatusRefunded, Refunded: 5, CreatedAt: t0},
		{RequestID: "a", Status: UsageLogStatusRefunded, Refunded: 7, CreatedAt: t0.Add(time.Second)},
		{RequestID: "b", Status: UsageLogStatusRefunded, Refunded: 3, CreatedAt: t0},
	}
	entries := reduceUsageLogBatch(events)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// 排序保证顾问锁顺序确定。
	if entries[0].requestID != "a" || entries[1].requestID != "b" {
		t.Fatalf("entries not sorted: %q, %q", entries[0].requestID, entries[1].requestID)
	}
	a := entries[0]
	if a.insert == nil || a.insert.Cost != 20 {
		t.Fatalf("duplicate success not deduped: %+v", a.insert)
	}
	if a.refund == nil || a.refund.Refunded != 7 {
		t.Fatalf("refund not merged to max amount: %+v", a.refund)
	}
}

// TestReduceUsageLogBatchRefundOnly 验证只有退款事件时生成收敛目标。
func TestReduceUsageLogBatchRefundOnly(t *testing.T) {
	entries := reduceUsageLogBatch([]UsageLogEvent{
		{RequestID: "r1", Status: UsageLogStatusRefunded, Refunded: 9, Cost: 3},
	})
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].insert != nil {
		t.Fatal("refund-only entry should not have a non-refunded insert")
	}
	if entries[0].refund == nil || entries[0].refund.Refunded != 9 {
		t.Fatalf("refund = %+v", entries[0].refund)
	}
}

// TestDedupKeySeparatesStatuses 验证 success/refunded 不互相吞掉。
func TestDedupKeySeparatesStatuses(t *testing.T) {
	success := dedupKey("rid-1", UsageLogStatusSuccess)
	refunded := dedupKey("rid-1", UsageLogStatusRefunded)
	if success == refunded {
		t.Fatalf("dedup key collides across statuses: %q", success)
	}
	if success != UsageLogDedupKeyPrefix+"rid-1:1" {
		t.Fatalf("dedup key = %q", success)
	}
}

// TestLogBatchSettingDefaultsOff 锁定安全上线口径：开关默认关闭。
func TestLogBatchSettingDefaultsOff(t *testing.T) {
	setting, ok := settings.Lookup("log.batch_enabled")
	if !ok {
		t.Fatal("log.batch_enabled is not registered")
	}
	if setting.Type != settings.TypeBool {
		t.Fatalf("type = %s, want bool", setting.Type)
	}
	if setting.Default != false {
		t.Fatalf("default = %#v, want false (synchronous path first)", setting.Default)
	}
	if !settings.IsKnownSection("log") {
		t.Fatal("log section is not declared")
	}
}

// TestUsageLogConsumerRequiresDeps 验证装配守卫。
func TestUsageLogConsumerRequiresDeps(t *testing.T) {
	if _, err := newUsageLogConsumer(UsageLogStreamDeps{}); err == nil {
		t.Fatal("newUsageLogConsumer(no deps) = nil error")
	}
	db, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := newUsageLogConsumer(UsageLogStreamDeps{LogsDB: db}); err == nil {
		t.Fatal("newUsageLogConsumer(no redis) = nil error")
	}
}
