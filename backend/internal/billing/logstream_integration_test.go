//go:build integration

package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
)

// 本文件是 W10-N2 日志批处理的真 PG + Redis 验收（R-W10-02 / R-W10-11）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10n2?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10n2_logs?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR='localhost:56379' \
//	  go test -tags=integration -p 1 ./internal/billing/ -run UsageLogStream -count=1
//
// 未设置 LOGS_DSN 时按「主库名 + _logs」派生，便于隔离域库。

func openLogStreamLogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		mainDSN := os.Getenv("NEWTG_TEST_DB_DSN")
		if mainDSN == "" {
			t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过日志批处理集成测试")
		}
		u, err := url.Parse(mainDSN)
		if err != nil || u.Scheme == "" {
			t.Fatalf("解析 NEWTG_TEST_DB_DSN 失败: %v", err)
		}
		name := strings.TrimPrefix(u.Path, "/")
		if !strings.HasSuffix(name, "_logs") {
			name += "_logs"
		}
		u.Path = "/" + name
		dsn = u.String()
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	requireUsageLogsSchema(t, db)
	return db
}

func openLogStreamRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过日志批处理集成测试")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("ping test redis: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Del(context.Background(), UsageLogStreamKey).Err()
		_ = client.Close()
	})
	return client
}

// resetUsageLogStream 清空 Stream 与消费组，保证用例从干净状态开始。
func resetUsageLogStream(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	_ = rdb.XGroupDestroy(ctx, UsageLogStreamKey, UsageLogStreamGroup).Err()
	if err := rdb.Del(ctx, UsageLogStreamKey).Err(); err != nil {
		t.Fatalf("reset stream: %v", err)
	}
}

// logStreamEvent 构造一条真实投影的事件。
func logStreamEvent(t *testing.T, requestID string, status int16, cost, refunded int64,
	createdAt time.Time) UsageLogEvent {
	t.Helper()
	rc := &contract.RelayContext{
		RequestID:   requestID,
		UserID:      7,
		ChannelID:   42,
		OriginModel: "gpt-4o",
		UsingGroup:  "vip",
		RequestPath: "/v1/chat/completions",
		ChannelType: contract.ChannelTypeOpenAI,
	}
	var fact *contract.UsageFact
	if status != UsageLogStatusRefunded {
		fact = &contract.UsageFact{
			Model: "gpt-4o",
			Usage: contract.Usage{Prompt: 100, Completion: 20, CacheRead: 5, CacheWrite: 1},
		}
	}
	row, err := buildUsageLogRow(fact, rc, SettlementOutcome{
		Cost: cost, PreDeducted: cost, Refunded: refunded, Status: status,
		ErrorMessage: map[bool]string{true: "request abandoned", false: ""}[status == UsageLogStatusRefunded],
	}, false)
	if err != nil {
		t.Fatalf("buildUsageLogRow: %v", err)
	}
	return eventFromUsageLogRow(row, createdAt, fact != nil, "openai")
}

// logStreamMessage 把事件包装成 Stream 消息（不需要真的入队即可测试消费者）。
func logStreamMessage(t *testing.T, id string, event UsageLogEvent) redis.XMessage {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return redis.XMessage{ID: id, Values: map[string]any{"event": string(payload)}}
}

func cleanupUsageLogRequest(t *testing.T, db *sql.DB, requestID string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM usage_logs WHERE request_id = $1::uuid`, requestID)
	})
}

// countUsageLogRows 统计某请求的日志行数。
func countUsageLogRows(t *testing.T, db *sql.DB, requestID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM usage_logs WHERE request_id = $1::uuid`, requestID).
		Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func newTestLogStreamConsumer(t *testing.T, rdb *redis.Client, logsDB *sql.DB,
	consumer string) *usageLogConsumer {
	t.Helper()
	c, err := newUsageLogConsumer(UsageLogStreamDeps{
		Redis:     rdb,
		LogsDB:    logsDB,
		Consumer:  consumer,
		ClaimIdle: 200 * time.Millisecond,
		Block:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	if err := c.ensureGroup(context.Background()); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	return c
}

// TestIntegrationUsageLogStreamWritesExplicitCreatedAt 验证批量落库与
// created_at 由事件携带（月分区归属正确，不漂移到冲刷时刻）。
func TestIntegrationUsageLogStreamWritesExplicitCreatedAt(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-write")

	now := time.Now().UTC().Truncate(time.Millisecond)
	current := time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)
	next := current.AddDate(0, 1, 0)

	ridCurrent := newTestUUID(t)
	ridNext := newTestUUID(t)
	cleanupUsageLogRequest(t, db, ridCurrent)
	cleanupUsageLogRequest(t, db, ridNext)

	evCurrent := logStreamEvent(t, ridCurrent, UsageLogStatusSuccess, 123, 0, current)
	evNext := logStreamEvent(t, ridNext, UsageLogStatusSuccess, 456, 0, next)

	msgs := []redis.XMessage{
		logStreamMessage(t, "1-0", evCurrent),
		logStreamMessage(t, "2-0", evNext),
	}
	if err := c.consume(context.Background(), msgs); err != nil {
		t.Fatalf("consume: %v", err)
	}

	for _, tc := range []struct {
		rid       string
		wantCost  int64
		createdAt time.Time
		partition string
	}{
		{ridCurrent, 123, current, fmt.Sprintf("usage_logs_%s", current.Format("200601"))},
		{ridNext, 456, next, fmt.Sprintf("usage_logs_%s", next.Format("200601"))},
	} {
		var (
			gotCost   int64
			gotTime   time.Time
			gotStatus int16
			gotModel  string
			gotUser   int64
			tableoid  string
		)
		err := db.QueryRow(`
			SELECT cost, created_at, status, model, user_id, tableoid::regclass::text
			  FROM usage_logs WHERE request_id = $1::uuid`, tc.rid).
			Scan(&gotCost, &gotTime, &gotStatus, &gotModel, &gotUser, &tableoid)
		if err != nil {
			t.Fatalf("read row %s: %v", tc.rid, err)
		}
		if gotCost != tc.wantCost {
			t.Fatalf("%s cost = %d, want %d", tc.rid, gotCost, tc.wantCost)
		}
		if !gotTime.UTC().Equal(tc.createdAt) {
			t.Fatalf("%s created_at = %s, want %s (event time, not flush time)",
				tc.rid, gotTime.UTC(), tc.createdAt)
		}
		if gotStatus != UsageLogStatusSuccess || gotModel != "gpt-4o" || gotUser != 7 {
			t.Fatalf("%s row = status:%d model:%q user:%d", tc.rid, gotStatus, gotModel, gotUser)
		}
		if tableoid != tc.partition {
			t.Fatalf("%s landed in %s, want %s", tc.rid, tableoid, tc.partition)
		}
	}
}

// TestIntegrationUsageLogStreamDedupsDuplicates 验证批内与跨批重复只写一行。
func TestIntegrationUsageLogStreamDedupsDuplicates(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-dedup")

	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)
	event := logStreamEvent(t, rid, UsageLogStatusSuccess, 42, 0, time.Now().UTC())

	// 批内重复：同一事件两条消息（不同消息 ID，内容相同）。
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "1-0", event),
		logStreamMessage(t, "2-0", event),
	}); err != nil {
		t.Fatalf("consume batch: %v", err)
	}
	if n := countUsageLogRows(t, db, rid); n != 1 {
		t.Fatalf("rows after in-batch duplicates = %d, want 1", n)
	}

	// 跨批重复：再次投递同一内容，DB 存在性检查兜底。
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "3-0", event),
	}); err != nil {
		t.Fatalf("consume redelivery: %v", err)
	}
	if n := countUsageLogRows(t, db, rid); n != 1 {
		t.Fatalf("rows after redelivery = %d, want 1", n)
	}
	// 幂等键必须落盘，后续重复走快路径。
	if got := rdb.Exists(context.Background(),
		dedupKey(rid, UsageLogStatusSuccess)).Val(); got != 1 {
		t.Fatalf("dedup key missing after successful insert")
	}
}

// TestIntegrationUsageLogStreamRefundConvergenceIsOneWay 验证 refunded 单向收敛：
// 乱序、重复、回退方向都不能把已退款行改回 success，金额只增不减。
func TestIntegrationUsageLogStreamRefundConvergenceIsOneWay(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-refund")

	now := time.Now().UTC()

	// 场景 1：success 先落库，随后退款；再投递旧的 success 不得回退。
	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)
	success := logStreamEvent(t, rid, UsageLogStatusSuccess, 100, 0, now)
	if err := c.consume(context.Background(), []redis.XMessage{logStreamMessage(t, "1-0", success)}); err != nil {
		t.Fatalf("consume success: %v", err)
	}
	refund := logStreamEvent(t, rid, UsageLogStatusRefunded, 0, 60, now.Add(time.Second))
	if err := c.consume(context.Background(), []redis.XMessage{logStreamMessage(t, "2-0", refund)}); err != nil {
		t.Fatalf("consume refund: %v", err)
	}
	assertUsageLogRefunded(t, db, rid, 60)

	// 旧 success 重投：不能把 refunded 回退成 success。
	if err := c.consume(context.Background(), []redis.XMessage{logStreamMessage(t, "3-0", success)}); err != nil {
		t.Fatalf("consume late success: %v", err)
	}
	assertUsageLogRefunded(t, db, rid, 60)
	if n := countUsageLogRows(t, db, rid); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	// 更小的退款金额不得把已有金额改小（GREATEST）。
	smallRefund := logStreamEvent(t, rid, UsageLogStatusRefunded, 0, 10, now.Add(2*time.Second))
	if err := c.consume(context.Background(), []redis.XMessage{logStreamMessage(t, "4-0", smallRefund)}); err != nil {
		t.Fatalf("consume smaller refund: %v", err)
	}
	assertUsageLogRefunded(t, db, rid, 60)

	// 场景 2：refunded 先到（worker 崩溃兜底），success 后到不得插第二行。
	rid2 := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid2)
	refundFirst := logStreamEvent(t, rid2, UsageLogStatusRefunded, 0, 30, now)
	if err := c.consume(context.Background(), []redis.XMessage{logStreamMessage(t, "5-0", refundFirst)}); err != nil {
		t.Fatalf("consume refund-first: %v", err)
	}
	lateSuccess := logStreamEvent(t, rid2, UsageLogStatusSuccess, 90, 0, now.Add(time.Second))
	if err := c.consume(context.Background(), []redis.XMessage{logStreamMessage(t, "6-0", lateSuccess)}); err != nil {
		t.Fatalf("consume late success 2: %v", err)
	}
	assertUsageLogRefunded(t, db, rid2, 30)
	if n := countUsageLogRows(t, db, rid2); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func assertUsageLogRefunded(t *testing.T, db *sql.DB, requestID string, wantRefunded int64) {
	t.Helper()
	var (
		status   int16
		refunded int64
	)
	if err := db.QueryRow(
		`SELECT status, refunded FROM usage_logs WHERE request_id = $1::uuid`,
		requestID).Scan(&status, &refunded); err != nil {
		t.Fatalf("read refunded row: %v", err)
	}
	if status != UsageLogStatusRefunded {
		t.Fatalf("status = %d, want refunded (%d)", status, UsageLogStatusRefunded)
	}
	if refunded != wantRefunded {
		t.Fatalf("refunded = %d, want %d", refunded, wantRefunded)
	}
}

// TestIntegrationUsageLogStreamInsertFailureKeepsPending 验证 INSERT 失败时不
// XACK：消息留在 pending，60s（测试缩短）后由 XAUTOCLAIM 重投。
func TestIntegrationUsageLogStreamInsertFailureKeepsPending(t *testing.T) {
	rdb := openLogStreamRedis(t)
	resetUsageLogStream(t, rdb)
	brokenDB := dbWithoutUsageLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUsageLogStreamLoop(ctx, UsageLogStreamDeps{
			Redis:     rdb,
			LogsDB:    brokenDB,
			Consumer:  "w10n2-failing-loop",
			ClaimIdle: 100 * time.Millisecond,
			Block:     50 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	rid := newTestUUID(t)
	event := logStreamEvent(t, rid, UsageLogStatusSuccess, 10, 0, time.Now().UTC())
	if err := NewRedisUsageLogQueue(rdb).Enqueue(context.Background(), event); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 等消费循环把消息投递出去并失败若干轮；pending 必须**持续存在**，
	// 而不是在 XACK 与 INSERT 之间被短暂观测到。
	time.Sleep(500 * time.Millisecond)
	pending := pendingCount(t, rdb)
	if pending == 0 {
		t.Fatal("no pending entries: the consumer must not XACK before INSERT succeeds")
	}
}

// pendingCount 读取消费组当前的 pending 数量。
func pendingCount(t *testing.T, rdb *redis.Client) int64 {
	t.Helper()
	pending, err := rdb.XPending(context.Background(), UsageLogStreamKey, UsageLogStreamGroup).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0
		}
		t.Fatalf("xpending: %v", err)
	}
	if pending == nil {
		return 0
	}
	return pending.Count
}

// dbWithoutUsageLogs 返回一个没有 usage_logs 表的库（主库），用于制造写入失败。
func dbWithoutUsageLogs(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open main db: %v", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		t.Fatalf("ping main db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestIntegrationUsageLogStreamClaimRedeliversWithoutDoubleWrite 验证消费者
// 崩溃后 XAUTOCLAIM 重投，且去重不会双写。
func TestIntegrationUsageLogStreamClaimRedeliversWithoutDoubleWrite(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)

	crashed := newTestLogStreamConsumer(t, rdb, db, "w10n2-crashed")
	heir := newTestLogStreamConsumer(t, rdb, db, "w10n2-heir")

	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)
	event := logStreamEvent(t, rid, UsageLogStatusSuccess, 77, 0, time.Now().UTC())
	if err := NewRedisUsageLogQueue(rdb).Enqueue(context.Background(), event); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 第一个消费者取到消息但不 ack（模拟处理前崩溃）。
	claimed, err := crashed.readOnce(context.Background())
	if err != nil {
		t.Fatalf("crashed read: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("crashed read = %d messages, want 1", len(claimed))
	}
	if n := countUsageLogRows(t, db, rid); n != 0 {
		t.Fatalf("rows before redelivery = %d, want 0", n)
	}

	// 等过 claim idle，幸存者 XAUTOCLAIM 重投并处理。
	time.Sleep(300 * time.Millisecond)
	redelivered, err := heir.readOnce(context.Background())
	if err != nil {
		t.Fatalf("heir read: %v", err)
	}
	if len(redelivered) != 1 {
		t.Fatalf("redelivered = %d messages, want 1", len(redelivered))
	}
	if err := heir.consume(context.Background(), redelivered); err != nil {
		t.Fatalf("heir consume: %v", err)
	}
	if n := countUsageLogRows(t, db, rid); n != 1 {
		t.Fatalf("rows after redelivery = %d, want 1", n)
	}
	// ack 之后 pending 必须清空。
	pending, err := rdb.XPending(context.Background(), UsageLogStreamKey, UsageLogStreamGroup).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("pending after ack = %d, want 0", pending.Count)
	}
}

// TestIntegrationUsageLogStreamLagMetric 验证积压指标上报。
func TestIntegrationUsageLogStreamLagMetric(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-lag")

	queue := NewRedisUsageLogQueue(rdb)
	for i := 0; i < 3; i++ {
		rid := newTestUUID(t)
		cleanupUsageLogRequest(t, db, rid)
		if err := queue.Enqueue(context.Background(),
			logStreamEvent(t, rid, UsageLogStatusSuccess, 1, 0, time.Now().UTC())); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	c.reportLag(context.Background())

	value, ok := gatherGauge(t, "newtg_worker_usage_logs_stream_lag")
	if !ok {
		t.Fatal("newtg_worker_usage_logs_stream_lag not exposed")
	}
	if value != 3 {
		t.Fatalf("stream lag = %v, want 3", value)
	}
}

// TestIntegrationUsageLogStreamNormalizesCompactUUID 覆盖 C1：生产侧 request_id
// 可能是 32 位无横线 hex，消费边界必须统一成 PG 的标准形态，否则跨批去重与
// 跨批退款收敛全部失配（会插第二行）。
func TestIntegrationUsageLogStreamNormalizesCompactUUID(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-compact")

	now := time.Now().UTC()
	canonical := newTestUUID(t)
	compact := strings.ReplaceAll(canonical, "-", "")
	cleanupUsageLogRequest(t, db, canonical)

	success := logStreamEvent(t, compact, UsageLogStatusSuccess, 100, 0, now)
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "1-0", success),
	}); err != nil {
		t.Fatalf("consume success: %v", err)
	}
	if n := countUsageLogRows(t, db, canonical); n != 1 {
		t.Fatalf("rows after first success = %d, want 1", n)
	}

	// 删掉幂等键，强制走 DB 存在性检查：形态不一致时这里会重复插入。
	if err := rdb.Del(context.Background(), dedupKey(canonical, UsageLogStatusSuccess)).Err(); err != nil {
		t.Fatalf("delete dedup key: %v", err)
	}
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "2-0", success),
	}); err != nil {
		t.Fatalf("consume redelivery: %v", err)
	}
	if n := countUsageLogRows(t, db, canonical); n != 1 {
		t.Fatalf("rows after compact-id redelivery = %d, want 1", n)
	}

	// success → refund：必须收敛已有行，而不是插第二行。
	refund := logStreamEvent(t, compact, UsageLogStatusRefunded, 0, 60, now.Add(time.Second))
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "3-0", refund),
	}); err != nil {
		t.Fatalf("consume refund: %v", err)
	}
	assertUsageLogRefunded(t, db, canonical, 60)
	if n := countUsageLogRows(t, db, canonical); n != 1 {
		t.Fatalf("rows after refund = %d, want 1", n)
	}

	// 旧 success 重投不得回退。
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "4-0", success),
	}); err != nil {
		t.Fatalf("consume late success: %v", err)
	}
	assertUsageLogRefunded(t, db, canonical, 60)

	// refund → success：同样不得插第二行或回退。
	canonical2 := newTestUUID(t)
	compact2 := strings.ReplaceAll(canonical2, "-", "")
	cleanupUsageLogRequest(t, db, canonical2)
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "5-0",
			logStreamEvent(t, compact2, UsageLogStatusRefunded, 0, 30, now)),
	}); err != nil {
		t.Fatalf("consume refund-first: %v", err)
	}
	if err := c.consume(context.Background(), []redis.XMessage{
		logStreamMessage(t, "6-0",
			logStreamEvent(t, compact2, UsageLogStatusSuccess, 90, 0, now.Add(time.Second))),
	}); err != nil {
		t.Fatalf("consume late success 2: %v", err)
	}
	assertUsageLogRefunded(t, db, canonical2, 30)
	if n := countUsageLogRows(t, db, canonical2); n != 1 {
		t.Fatalf("rows after refund-first = %d, want 1", n)
	}
}

// TestIntegrationUsageLogStreamPoisonsInvalidRequestID 覆盖 I2：非法 request_id
// 不能被留在 pending 永久阻塞整条流。
func TestIntegrationUsageLogStreamPoisonsInvalidRequestID(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-poison")

	payload, err := json.Marshal(UsageLogEvent{RequestID: "not-a-uuid", Status: UsageLogStatusSuccess})
	if err != nil {
		t.Fatalf("marshal poison: %v", err)
	}
	if err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: UsageLogStreamKey, Values: []any{"event", string(payload)},
	}).Err(); err != nil {
		t.Fatalf("xadd poison: %v", err)
	}

	msgs, err := c.readOnce(context.Background())
	if err != nil {
		t.Fatalf("read poison: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("read = %d, want 1", len(msgs))
	}
	if err := c.consume(context.Background(), msgs); err != nil {
		t.Fatalf("consume poison: %v", err)
	}
	if pending := pendingCount(t, rdb); pending != 0 {
		t.Fatalf("poison message left pending = %d, want 0 (must be acked)", pending)
	}

	// 后续正常消息必须仍能被处理（流没有被毒消息卡住）。
	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)
	if err := NewRedisUsageLogQueue(rdb).Enqueue(context.Background(),
		logStreamEvent(t, rid, UsageLogStatusSuccess, 5, 0, time.Now().UTC())); err != nil {
		t.Fatalf("enqueue valid: %v", err)
	}
	msgs, err = c.readOnce(context.Background())
	if err != nil || len(msgs) != 1 {
		t.Fatalf("read valid = %d (%v), want 1", len(msgs), err)
	}
	if err := c.consume(context.Background(), msgs); err != nil {
		t.Fatalf("consume valid: %v", err)
	}
	if n := countUsageLogRows(t, db, rid); n != 1 {
		t.Fatalf("valid row after poison = %d, want 1", n)
	}
}

// TestIntegrationUsageLogStreamRetryAfterInsertFailureLands 覆盖 I1：INSERT 失败
// 时不得提前写幂等键；重投后必须真正落库。
func TestIntegrationUsageLogStreamRetryAfterInsertFailureLands(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	brokenDB := dbWithoutUsageLogs(t)
	resetUsageLogStream(t, rdb)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUsageLogStreamLoop(ctx, UsageLogStreamDeps{
			Redis:     rdb,
			LogsDB:    brokenDB,
			Consumer:  "w10n2-retry-failing",
			ClaimIdle: 100 * time.Millisecond,
			Block:     50 * time.Millisecond,
		})
	}()

	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)
	event := logStreamEvent(t, rid, UsageLogStatusSuccess, 10, 0, time.Now().UTC())
	if err := NewRedisUsageLogQueue(rdb).Enqueue(context.Background(), event); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 等失败轮次发生：消息 pending，且幂等键**不得**存在（提前置键会让重投被跳过）。
	time.Sleep(400 * time.Millisecond)
	if pending := pendingCount(t, rdb); pending == 0 {
		t.Fatal("no pending entries after insert failure")
	}
	if got := rdb.Exists(context.Background(), dedupKey(rid, UsageLogStatusSuccess)).Val(); got != 0 {
		t.Fatal("dedup key written before the insert committed; a later retry would be skipped and the log lost")
	}
	cancel()
	<-done

	// 幸存消费者领取 pending 并真正落库。
	time.Sleep(300 * time.Millisecond)
	heir := newTestLogStreamConsumer(t, rdb, db, "w10n2-retry-heir")
	msgs, err := heir.readOnce(context.Background())
	if err != nil {
		t.Fatalf("heir read: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("heir claimed %d messages, want 1", len(msgs))
	}
	if err := heir.consume(context.Background(), msgs); err != nil {
		t.Fatalf("heir consume: %v", err)
	}
	if n := countUsageLogRows(t, db, rid); n != 1 {
		t.Fatalf("rows after redelivery = %d, want 1", n)
	}
	if pending := pendingCount(t, rdb); pending != 0 {
		t.Fatalf("pending after ack = %d, want 0", pending)
	}
}

// captureLogHandler 记录 slog 记录（积压告警断言）。
type captureLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureLogHandler) WithGroup(string) slog.Handler { return h }

func (h *captureLogHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.records))
	for _, r := range h.records {
		out = append(out, r.Message)
	}
	return out
}

// TestIntegrationUsageLogStreamLagIncludesPending 验证 lag 口径包含 pending。
func TestIntegrationUsageLogStreamLagIncludesPending(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)
	c := newTestLogStreamConsumer(t, rdb, db, "w10n2-lag-pending")

	queue := NewRedisUsageLogQueue(rdb)
	for i := 0; i < 2; i++ {
		rid := newTestUUID(t)
		cleanupUsageLogRequest(t, db, rid)
		if err := queue.Enqueue(context.Background(),
			logStreamEvent(t, rid, UsageLogStatusSuccess, 1, 0, time.Now().UTC())); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// 投递但不 ack → Pending=2，Lag=0。
	msgs, err := c.readOnce(context.Background())
	if err != nil || len(msgs) != 2 {
		t.Fatalf("read = %d (%v), want 2", len(msgs), err)
	}
	c.reportLag(context.Background())
	value, ok := gatherGauge(t, "newtg_worker_usage_logs_stream_lag")
	if !ok {
		t.Fatal("lag metric missing")
	}
	if value != 2 {
		t.Fatalf("lag = %v, want 2 (pending counts toward the backlog)", value)
	}
	_ = c.consume(context.Background(), msgs) // 清理 pending，避免影响后续用例
}

// TestIntegrationUsageLogStreamLagWarnsAboveThreshold 验证超阈值会告警。
func TestIntegrationUsageLogStreamLagWarnsAboveThreshold(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)

	handler := &captureLogHandler{}
	c, err := newUsageLogConsumer(UsageLogStreamDeps{
		Redis:    rdb,
		LogsDB:   db,
		Consumer: "w10n2-lag-warn",
		Logger:   slog.New(handler),
	})
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	if err := c.ensureGroup(context.Background()); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	c.lagThreshold = 0 // 任何积压都应告警（测试接缝）

	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)
	if err := NewRedisUsageLogQueue(rdb).Enqueue(context.Background(),
		logStreamEvent(t, rid, UsageLogStatusSuccess, 1, 0, time.Now().UTC())); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	c.reportLag(context.Background())

	found := false
	for _, message := range handler.messages() {
		if strings.Contains(message, "backlog exceeds the alert threshold") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no backlog warning recorded: %v", handler.messages())
	}
}

// gatherGauge 从默认 registry 中读取单个 gauge 的值。
func gatherGauge(t *testing.T, name string) (float64, bool) {
	t.Helper()
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if g := metric.GetGauge(); g != nil {
				return g.GetValue(), true
			}
		}
	}
	return 0, false
}

// TestIntegrationUsageLogStreamShutdownDrains 验证停机时已取出的批次冲刷并 ack。
func TestIntegrationUsageLogStreamShutdownDrains(t *testing.T) {
	rdb := openLogStreamRedis(t)
	db := openLogStreamLogsDB(t)
	resetUsageLogStream(t, rdb)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUsageLogStreamLoop(ctx, UsageLogStreamDeps{
			Redis:     rdb,
			LogsDB:    db,
			Consumer:  "w10n2-drain",
			ClaimIdle: 100 * time.Millisecond,
			Block:     50 * time.Millisecond,
		})
	}()

	queue := NewRedisUsageLogQueue(rdb)
	const n = 5
	rids := make([]string, n)
	for i := 0; i < n; i++ {
		rids[i] = newTestUUID(t)
		cleanupUsageLogRequest(t, db, rids[i])
		if err := queue.Enqueue(context.Background(),
			logStreamEvent(t, rids[i], UsageLogStatusSuccess, 5, 0, time.Now().UTC())); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		total := 0
		for _, rid := range rids {
			total += countUsageLogRows(t, db, rid)
		}
		if total == n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, rid := range rids {
		if got := countUsageLogRows(t, db, rid); got != 1 {
			t.Fatalf("request %s rows = %d, want 1", rid, got)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loop returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer loop did not stop after cancel")
	}
	pending, err := rdb.XPending(context.Background(), UsageLogStreamKey, UsageLogStreamGroup).Result()
	if err != nil {
		t.Fatalf("xpending: %v", err)
	}
	if pending.Count != 0 {
		t.Fatalf("pending after drain = %d, want 0", pending.Count)
	}
}

// TestIntegrationUsageLogStreamEnqueueFailureFallsBackSync 验证生产侧 XADD 失败
// 会同步回退：日志仍然落库，且 created_at 走数据库默认值（同步路径语义不变）。
func TestIntegrationUsageLogStreamEnqueueFailureFallsBackSync(t *testing.T) {
	db := openLogStreamLogsDB(t)
	rid := newTestUUID(t)
	cleanupUsageLogRequest(t, db, rid)

	eventTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	writer := NewDBLogger(db).WithBatchQueue(&captureQueue{err: errors.New("redis unavailable")}, nil)
	writer.now = func() time.Time { return eventTime }

	rc := &contract.RelayContext{
		RequestID: rid, UserID: 1, OriginModel: "gpt-4o", ChannelType: contract.ChannelTypeOpenAI,
	}
	if err := writer.Saturate(context.Background(), testUsageLogFact(), rc,
		SettlementOutcome{Cost: 55, PreDeducted: 55, Status: UsageLogStatusSuccess}); err != nil {
		t.Fatalf("Saturate fallback: %v", err)
	}

	var (
		cost      int64
		createdAt time.Time
	)
	if err := db.QueryRow(`SELECT cost, created_at FROM usage_logs WHERE request_id = $1::uuid`, rid).
		Scan(&cost, &createdAt); err != nil {
		t.Fatalf("read fallback row: %v", err)
	}
	if cost != 55 {
		t.Fatalf("cost = %d, want 55", cost)
	}
	if createdAt.UTC().Year() == eventTime.Year() {
		t.Fatalf("created_at = %s; synchronous fallback must use DB now(), not the event time",
			createdAt.UTC())
	}
}

// testUsageLogFact 是一个最小可用用量事实。
func testUsageLogFact() *contract.UsageFact {
	return &contract.UsageFact{
		Model: "gpt-4o",
		Usage: contract.Usage{Prompt: 10, Completion: 2},
	}
}
