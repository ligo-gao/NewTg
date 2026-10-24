package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
	}, false)
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
	// 排障证据必须随事件穿过 Redis 传输表示（JSON 往返）后仍可还原。
	if !row.retryChain.Valid {
		t.Fatal("测试前置：retry_chain 应为非 NULL")
	}
	if event.RetryChain == nil || *event.RetryChain != row.retryChain.String {
		t.Fatalf("retry_chain 未随事件携带: %+v", event.RetryChain)
	}
	if got.retryChain != row.retryChain {
		t.Fatalf("retry_chain drifted: %+v vs %+v", got.retryChain, row.retryChain)
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

// blockingQueue 模拟 Redis 不可达：Enqueue 一直阻塞到上下文超时。
type blockingQueue struct{}

func (blockingQueue) Enqueue(ctx context.Context, _ UsageLogEvent) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestNormalizeUsageLogRequestID 覆盖生产 ID 的形态差异（C1）。
func TestNormalizeUsageLogRequestID(t *testing.T) {
	const canonical = "0b6f5f3e-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
	compact := strings.ReplaceAll(canonical, "-", "")
	for _, raw := range []string{
		canonical,
		compact,
		strings.ToUpper(compact),
		"{" + compact + "}",
		"  " + canonical + "  ",
	} {
		got, err := normalizeUsageLogRequestID(raw)
		if err != nil {
			t.Fatalf("normalize(%q): %v", raw, err)
		}
		if got != canonical {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, canonical)
		}
	}
	for _, raw := range []string{"", "not-a-uuid", compact[:31], compact + "0", "zz" + compact[2:]} {
		if _, err := normalizeUsageLogRequestID(raw); err == nil {
			t.Fatalf("normalize(%q) = nil error, want invalid", raw)
		}
	}
}

// TestSaturateEnqueueNormalizesCompactID 验证事件只携带标准形态的 request_id。
func TestSaturateEnqueueNormalizesCompactID(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	compact := strings.ReplaceAll(testRelayContext().RequestID, "-", "")
	rc := testRelayContext()
	rc.RequestID = compact
	queue := &captureQueue{}
	logger := NewDBLogger(db).WithBatchQueue(queue, nil)
	if err := logger.Saturate(context.Background(), testUsageFact(), rc,
		SettlementOutcome{Cost: 1, Status: UsageLogStatusSuccess}); err != nil {
		t.Fatalf("Saturate: %v", err)
	}
	if len(queue.events) != 1 {
		t.Fatalf("events = %d", len(queue.events))
	}
	if got := queue.events[0].RequestID; got != testRelayContext().RequestID {
		t.Fatalf("event request_id = %q, want canonical %q", got, testRelayContext().RequestID)
	}
}

// TestSaturateEnqueueRefundHasNoFeed 验证退款事件同流但不会进入 live_feed。
func TestSaturateEnqueueRefundHasNoFeed(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://invalid:invalid@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("open placeholder db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	queue := &captureQueue{}
	logger := NewDBLogger(db).WithBatchQueue(queue, nil)
	// fact=nil：worker 的崩溃退款占位，没有可展示的用量。
	if err := logger.Saturate(context.Background(), nil, testRelayContext(),
		SettlementOutcome{Refunded: 50, Status: UsageLogStatusRefunded}); err != nil {
		t.Fatalf("Saturate refund: %v", err)
	}
	if len(queue.events) != 1 {
		t.Fatalf("events = %d", len(queue.events))
	}
	if queue.events[0].Status != UsageLogStatusRefunded {
		t.Fatalf("status = %d, want refunded", queue.events[0].Status)
	}
	if queue.events[0].FeedEligible {
		t.Fatal("refund placeholder must not be feed-eligible")
	}
}

// TestEnqueueUsageLogTimesOut 验证 XADD 有独立短超时（不让 Redis 故障拖住结算）。
func TestEnqueueUsageLogTimesOut(t *testing.T) {
	row, err := buildUsageLogRow(testUsageFact(), testRelayContext(),
		SettlementOutcome{Cost: 1, Status: UsageLogStatusSuccess}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	logger := NewDBLogger(nil).WithBatchQueue(blockingQueue{}, nil)
	start := time.Now()
	err = logger.enqueueUsageLog(context.Background(), row, true, testRelayContext())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("enqueueUsageLog = nil, want timeout error")
	}
	if elapsed < usageLogEnqueueTimeout-100*time.Millisecond {
		t.Fatalf("enqueue returned after %v, want ~%v timeout", elapsed, usageLogEnqueueTimeout)
	}
	if elapsed > usageLogEnqueueTimeout+2*time.Second {
		t.Fatalf("enqueue took %v, timeout did not bound it", elapsed)
	}
}

// TestStreamLagTotal 验证 Pending 与 Lag 叠加，且 Lag=-1 不污染结果。
func TestStreamLagTotal(t *testing.T) {
	if got := streamLagTotal(5, 7); got != 12 {
		t.Fatalf("total = %d, want 12", got)
	}
	if got := streamLagTotal(5, -1); got != 5 {
		t.Fatalf("total with lag=-1 = %d, want 5", got)
	}
	if got := streamLagTotal(0, 0); got != 0 {
		t.Fatalf("total = %d, want 0", got)
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

// TestUsageLogEventRoundTripsReconcileDimensions 验证对账维度经 Redis 批处理
// 事件往返后逐字段保真（含 NULL 与零值语义）。
func TestUsageLogEventRoundTripsReconcileDimensions(t *testing.T) {
	src := int16(1)
	model := "claude-sonnet-4"
	account := int64(3)
	resp := "msg_1"
	ev := UsageLogEvent{
		RequestID: "r", UserID: 1, Model: "m", Status: 1,
		UsageSource: &src, UsageSemantic: &src, ReasoningTokens: 9,
		UpstreamModel: &model, UpstreamAccountID: &account, ResponseID: &resp, Attempts: 3,
	}
	row := ev.usageLogRow()
	back := eventFromUsageLogRow(row, time.Unix(0, 0), false, "")
	if back.UsageSource == nil || *back.UsageSource != 1 ||
		back.ReasoningTokens != 9 || back.Attempts != 3 ||
		back.UpstreamAccountID == nil || *back.UpstreamAccountID != 3 ||
		back.ResponseID == nil || *back.ResponseID != "msg_1" {
		t.Fatalf("round trip lost dimensions: %+v", back)
	}
}

// TestUsageLogEventReconcileFieldsAreOptional 锁定滚动发布兼容性：
// 旧生产者（本版本之前）产出的事件没有对账维度键，必须解出零值；
// 新生产者写零值时 omitempty 不得把这些键塞进事件，旧消费者才不会看到
// 未预期的形状。落库投影还要把 Attempts 归一到 NOT NULL DEFAULT 1。
func TestUsageLogEventReconcileFieldsAreOptional(t *testing.T) {
	legacy := `{"request_id":"r","model":"m","status":1}`
	var ev UsageLogEvent
	if err := json.Unmarshal([]byte(legacy), &ev); err != nil {
		t.Fatalf("unmarshal legacy event: %v", err)
	}
	if ev.UsageSource != nil || ev.UsageSemantic != nil || ev.ReasoningTokens != 0 ||
		ev.UpstreamModel != nil || ev.UpstreamAccountID != nil ||
		ev.ResponseID != nil || ev.Attempts != 0 {
		t.Fatalf("legacy event did not decode to zero dimensions: %+v", ev)
	}
	row := ev.usageLogRow()
	if row.usageSource.Valid || row.usageSemantic.Valid ||
		row.upstreamModel.Valid || row.upstreamAccountID.Valid || row.responseID.Valid {
		t.Fatalf("legacy event must leave nullable dimensions NULL: %+v", row)
	}
	if row.attempts != 1 {
		t.Fatalf("legacy attempts = %d, want coerced default 1", row.attempts)
	}

	payload, err := json.Marshal(UsageLogEvent{RequestID: "r", Model: "m", Status: 1})
	if err != nil {
		t.Fatalf("marshal zero event: %v", err)
	}
	for _, key := range []string{
		`"usage_source"`, `"usage_semantic"`, `"reasoning_tokens"`,
		`"upstream_model"`, `"upstream_account_id"`, `"response_id"`, `"attempts"`,
	} {
		if strings.Contains(string(payload), key) {
			t.Fatalf("zero-valued field %s leaked into the event JSON: %s", key, payload)
		}
	}
}
