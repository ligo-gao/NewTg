//go:build integration

package notify

// N8 Webhook worker 的真实 PostgreSQL 验收（Redis 用 miniredis）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_b1?sslmode=disable' \
//	  go test -tags=integration ./internal/notify/ -run WebhookWorker -v
//
// 覆盖：成功/失败重试间隔/4 次后 failed、SKIP LOCKED 单发、暂停 51 次后停止
// 发送、事件订阅抑制、未启用不领取、停机与启动即扫、失败文案与日志脱敏。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// fakeWebhookWorkerSender 是可编排的发送器：按调用顺序返回预设结果。
type fakeWebhookWorkerSender struct {
	mu     sync.Mutex
	active bool
	calls  []webhookWorkerCall
	// onSend 在每次调用时执行，可返回本次结果；nil 时使用 result/err。
	onSend func(call webhookWorkerCall) (WebhookResult, error)
	// delay 模拟网络耗时（并发单发用例必须让第一个占用真实持续一段时间）。
	delay time.Duration
	// sent 在每次调用完成后非阻塞发送（启动扫描用例等待首轮）。
	sent chan struct{}
}

type webhookWorkerCall struct {
	payload []byte
	event   string
	eventID string
}

func (f *fakeWebhookWorkerSender) Active(context.Context) bool { return f.active }

func (f *fakeWebhookWorkerSender) Send(_ context.Context, payload []byte, event, eventID string) (WebhookResult, error) {
	call := webhookWorkerCall{payload: append([]byte(nil), payload...), event: event, eventID: eventID}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	var result WebhookResult
	var err error
	if f.onSend != nil {
		result, err = f.onSend(call)
	} else {
		result, err = WebhookResult{StatusCode: 200, OK: true}, nil
	}
	if f.sent != nil {
		select {
		case f.sent <- struct{}{}:
		default:
		}
	}
	return result, err
}

func (f *fakeWebhookWorkerSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeWebhookWorkerSender) callsSnapshot() []webhookWorkerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]webhookWorkerCall(nil), f.calls...)
}

func newWebhookWorkerRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// cleanWebhookDeliveries 保证每个用例只看到自己插入的投递行。
func cleanWebhookDeliveries(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM notification_deliveries`); err != nil {
		t.Fatalf("clean deliveries: %v", err)
	}
}

type webhookDeliverySeed struct {
	dedupe   string
	category string
	payload  any // string / nil
	attempt  int
	next     *time.Time
	status   string
}

func insertWebhookDelivery(t *testing.T, db *sql.DB, seed webhookDeliverySeed) int64 {
	t.Helper()
	if seed.status == "" {
		seed.status = "queued"
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO notification_deliveries
		(template_key, category, channel, recipient, dedupe_key, status, attempt,
		 next_retry_at, payload)
		VALUES (NULL, $1, 'webhook', $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		seed.category, "https://hooks.example.com/endpoint", seed.dedupe,
		seed.status, seed.attempt, seed.next, seed.payload).Scan(&id); err != nil {
		t.Fatalf("insert webhook delivery: %v", err)
	}
	return id
}

func readWebhookDelivery(t *testing.T, db *sql.DB, id int64) (status string, attempt int, next *time.Time, lastError *string) {
	t.Helper()
	var nextRetry sql.NullTime
	var lastErr sql.NullString
	if err := db.QueryRow(`SELECT status, attempt, next_retry_at, last_error
		FROM notification_deliveries WHERE id = $1`, id).
		Scan(&status, &attempt, &nextRetry, &lastErr); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if nextRetry.Valid {
		value := nextRetry.Time
		next = &value
	}
	if lastErr.Valid {
		value := lastErr.String
		lastError = &value
	}
	return status, attempt, next, lastError
}

func webhookWorkerForTest(t *testing.T, db *sql.DB, rdb *redis.Client, sender WebhookWorkerSender, now time.Time) *WebhookWorker {
	t.Helper()
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	worker, err := NewWebhookWorker(WebhookWorkerDeps{
		Store:  store,
		Sender: sender,
		Redis:  rdb,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewWebhookWorker: %v", err)
	}
	return worker
}

func TestWebhookWorkerSuccessMarksSentAndResetsStreak(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "balance:7:20261020", category: "billing.balance_low",
		payload: `{"event":"balance_low","user_id":7}`, attempt: 0,
	})
	if err := rdb.Set(ctx, WebhookFailStreakKey, "5", 0).Err(); err != nil {
		t.Fatalf("seed streak: %v", err)
	}

	sender := &fakeWebhookWorkerSender{active: true}
	worker := webhookWorkerForTest(t, db, rdb, sender, now)
	result := worker.RunOnce(ctx)
	if result.Claimed != 1 || result.Sent != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}

	status, attempt, next, lastError := readWebhookDelivery(t, db, id)
	if status != "sent" || attempt != 1 || next != nil || lastError != nil {
		t.Fatalf("delivery = status=%s attempt=%d next=%v last_error=%v", status, attempt, next, lastError)
	}
	if exists, err := rdb.Exists(ctx, WebhookFailStreakKey).Result(); err != nil || exists != 0 {
		t.Fatalf("成功后 fail_streak 未清零：exists=%d err=%v", exists, err)
	}

	calls := sender.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("Send 调用数 = %d", len(calls))
	}
	if calls[0].event != "balance_low" {
		t.Fatalf("event = %q，期望 balance_low", calls[0].event)
	}
	if calls[0].eventID != "balance:7:20261020" {
		t.Fatalf("eventID = %q，期望投递行 dedupe_key", calls[0].eventID)
	}
	// payload 来自投递记录。payload 列是 jsonb，PostgreSQL 会重排/格式化，
	// 因此这里比较语义而不是原始字节；签名覆盖的是 worker 实际发送的这份
	// 字节（WebhookSender 只序列化一次）。
	var gotPayload map[string]any
	if err := json.Unmarshal(calls[0].payload, &gotPayload); err != nil {
		t.Fatalf("payload 不是 JSON: %v（%s）", err, calls[0].payload)
	}
	if gotPayload["event"] != "balance_low" || gotPayload["user_id"] != float64(7) {
		t.Fatalf("payload = %s", calls[0].payload)
	}
}

func TestWebhookWorkerRetryScheduleEndsFailed(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "balance:8:20261020", category: "billing.balance_low",
		payload: `{"event":"balance_low","user_id":8}`, attempt: 0,
	})
	sender := &fakeWebhookWorkerSender{
		active: true,
		onSend: func(webhookWorkerCall) (WebhookResult, error) {
			return WebhookResult{}, ErrWebhookTransport
		},
	}
	worker := webhookWorkerForTest(t, db, rdb, sender, now)

	wantDelays := []time.Duration{time.Minute, 5 * time.Minute, 25 * time.Minute}
	for i, wantDelay := range wantDelays {
		result := worker.RunOnce(ctx)
		if result.Failed != 1 {
			t.Fatalf("第 %d 轮 result = %+v", i+1, result)
		}
		status, attempt, next, lastError := readWebhookDelivery(t, db, id)
		if status != "queued" {
			t.Fatalf("第 %d 次失败后 status = %s，期望 queued", i+1, status)
		}
		if attempt != i+1 {
			t.Fatalf("第 %d 次失败后 attempt = %d", i+1, attempt)
		}
		if next == nil || !next.Equal(now.Add(wantDelay)) {
			t.Fatalf("第 %d 次失败后 next_retry_at = %v，期望 %v", i+1, next, now.Add(wantDelay))
		}
		if lastError == nil || *lastError != "webhook request failed" {
			t.Fatalf("last_error = %v", lastError)
		}
		// 把重试时刻拨到过去，模拟时间流逝后的下一轮扫描。
		if _, err := db.Exec(`UPDATE notification_deliveries SET next_retry_at = $2 WHERE id = $1`,
			id, now.Add(-time.Second)); err != nil {
			t.Fatalf("rewind next_retry_at: %v", err)
		}
	}

	// 第 4 次失败：终态 failed，不再安排重试。
	result := worker.RunOnce(ctx)
	if result.Failed != 1 {
		t.Fatalf("第 4 轮 result = %+v", result)
	}
	status, attempt, next, _ := readWebhookDelivery(t, db, id)
	if status != "failed" || attempt != webhookMaxAttempts || next != nil {
		t.Fatalf("终态 = status=%s attempt=%d next=%v", status, attempt, next)
	}
	if sender.callCount() != webhookMaxAttempts {
		t.Fatalf("Send 调用数 = %d，期望 %d", sender.callCount(), webhookMaxAttempts)
	}
}

func TestWebhookWorkerSkipLockedSendsEachRowOnce(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	const rows = 10
	for i := 0; i < rows; i++ {
		insertWebhookDelivery(t, db, webhookDeliverySeed{
			dedupe:   fmt.Sprintf("balance:%d:20261020", i),
			category: "billing.balance_low",
			payload:  fmt.Sprintf(`{"event":"balance_low","user_id":%d}`, i),
		})
	}

	// 直接验证行锁语义：A 占用后 B 必须跳过已锁行、领取另一行，绝不拿到同一行。
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	claimA, ok, err := store.ClaimWebhookDelivery(ctx, now)
	if err != nil || !ok {
		t.Fatalf("claim A = %v,%v", ok, err)
	}
	claimB, okB, err := store.ClaimWebhookDelivery(ctx, now)
	if err != nil || !okB {
		t.Fatalf("claim B = %v,%v", okB, err)
	}
	if claimB.Delivery.ID == claimA.Delivery.ID {
		t.Fatalf("B 拿到了被 A 锁住的同一行：%d（应 SKIP LOCKED）", claimB.Delivery.ID)
	}
	claimA.Release()
	claimB.Release()

	// 两个 worker 并发扫同一批：每行只能被发送一次。
	sender := &fakeWebhookWorkerSender{active: true, delay: 15 * time.Millisecond}
	store1, _ := NewStore(db)
	store2, _ := NewStore(db)
	newWorker := func(s *Store) *WebhookWorker {
		w, err := NewWebhookWorker(WebhookWorkerDeps{
			Store: s, Sender: sender, Redis: rdb,
			Now:       func() time.Time { return now },
			BatchSize: rows * 2,
		})
		if err != nil {
			t.Fatalf("NewWebhookWorker: %v", err)
		}
		return w
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); newWorker(store1).RunOnce(ctx) }()
	go func() { defer wg.Done(); newWorker(store2).RunOnce(ctx) }()
	wg.Wait()

	if sender.callCount() != rows {
		t.Fatalf("Send 调用数 = %d，期望 %d（重复或遗漏）", sender.callCount(), rows)
	}
	seen := map[string]bool{}
	for _, call := range sender.callsSnapshot() {
		if seen[call.eventID] {
			t.Fatalf("eventID %q 被重复发送", call.eventID)
		}
		seen[call.eventID] = true
	}
	var sent int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE channel = 'webhook' AND status = 'sent'`).Scan(&sent); err != nil {
		t.Fatalf("count sent: %v", err)
	}
	if sent != rows {
		t.Fatalf("sent 行数 = %d，期望 %d", sent, rows)
	}

	// 崩溃/回滚后的行仍可被领取（锁不是永久租约）。
	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "crash:1", category: "billing.balance_low", payload: `{"event":"balance_low"}`,
	})
	claimC, ok, err := store.ClaimWebhookDelivery(ctx, now)
	if err != nil || !ok || claimC.Delivery.ID != id {
		t.Fatalf("claim C = %+v,%v,%v", claimC, ok, err)
	}
	claimC.Release()
	worker := newWorker(store1)
	worker.batchSize = 1
	worker.RunOnce(ctx)
	status, _, _, _ := readWebhookDelivery(t, db, id)
	if status != "sent" {
		t.Fatalf("回滚后的行 status = %s，期望 sent", status)
	}
}

func TestWebhookWorkerPausesAfterFiftyOneConsecutiveFailures(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	const rows = 60
	for i := 0; i < rows; i++ {
		insertWebhookDelivery(t, db, webhookDeliverySeed{
			dedupe:   fmt.Sprintf("pause:%d", i),
			category: "billing.balance_low",
			payload:  `{"event":"balance_low"}`,
		})
	}
	sender := &fakeWebhookWorkerSender{
		active: true,
		onSend: func(webhookWorkerCall) (WebhookResult, error) {
			return WebhookResult{}, ErrWebhookTransport
		},
	}
	store, _ := NewStore(db)
	worker, err := NewWebhookWorker(WebhookWorkerDeps{
		Store: store, Sender: sender, Redis: rdb,
		Now:       func() time.Time { return now },
		BatchSize: rows,
	})
	if err != nil {
		t.Fatalf("NewWebhookWorker: %v", err)
	}

	result := worker.RunOnce(ctx)
	if result.Paused != true {
		t.Fatalf("result.Paused = false，期望暂停")
	}
	// 阈值是「超过 50」：第 51 次失败后立即暂停，剩余 9 条不再发送。
	if sender.callCount() != WebhookPauseThreshold+1 {
		t.Fatalf("Send 调用数 = %d，期望 %d", sender.callCount(), WebhookPauseThreshold+1)
	}
	if exists, err := rdb.Exists(ctx, WebhookPausedKey).Result(); err != nil || exists != 1 {
		t.Fatalf("paused 标记 = %d,%v", exists, err)
	}
	streak, err := rdb.Get(ctx, WebhookFailStreakKey).Int()
	if err != nil || streak != WebhookPauseThreshold+1 {
		t.Fatalf("fail_streak = %d,%v", streak, err)
	}
	var attempted, untouched int
	if err := db.QueryRow(`SELECT
		count(*) FILTER (WHERE attempt = 1),
		count(*) FILTER (WHERE attempt = 0)
		FROM notification_deliveries WHERE channel = 'webhook' AND status = 'queued'`).
		Scan(&attempted, &untouched); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempted != WebhookPauseThreshold+1 || untouched != rows-(WebhookPauseThreshold+1) {
		t.Fatalf("attempt 分布 = %d/%d，期望 51/%d", attempted, untouched, rows-51)
	}

	// 暂停期间：即使把重试时刻拨到过去，也不得再发送。
	if _, err := db.Exec(`UPDATE notification_deliveries SET next_retry_at = $1
		WHERE channel = 'webhook' AND status = 'queued'`, now.Add(-time.Hour)); err != nil {
		t.Fatalf("rewind retries: %v", err)
	}
	before := sender.callCount()
	if again := worker.RunOnce(ctx); again.Claimed != 0 || again.Paused != true {
		t.Fatalf("暂停后 RunOnce = %+v", again)
	}
	if sender.callCount() != before {
		t.Fatalf("暂停期间仍发送了 %d 条", sender.callCount()-before)
	}

	// 恢复（模拟 resume 端点清键）后，worker 下一轮继续领取，且计数从 0 重新累计。
	if err := rdb.Del(ctx, WebhookPausedKey, WebhookFailStreakKey).Err(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	sender.onSend = nil // 恢复后成功
	if resumed := worker.RunOnce(ctx); resumed.Claimed == 0 {
		t.Fatalf("恢复后没有继续领取：%+v", resumed)
	}
}

func TestWebhookWorkerSuppressesUnsubscribedAndRejectsUnsendableRows(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	unsubscribed := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "unsubscribed:1", category: "billing.balance_low",
		payload: `{"event":"balance_low"}`,
	})
	// billing.usage_digest 没有 Webhook 订阅面。
	unsupported := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "unsupported:1", category: "billing.usage_digest",
		payload: `{"event":"usage_digest"}`,
	})
	// webhook payload 为空。
	empty := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "empty:1", category: "billing.payment_success", payload: nil,
	})

	sender := &fakeWebhookWorkerSender{
		active: true,
		onSend: func(webhookWorkerCall) (WebhookResult, error) {
			return WebhookResult{}, ErrEventNotSubscribed
		},
	}
	worker := webhookWorkerForTest(t, db, rdb, sender, now)
	result := worker.RunOnce(ctx)
	if result.Claimed != 3 || result.Suppressed != 1 || result.Failed != 2 {
		t.Fatalf("result = %+v", result)
	}

	status, attempt, _, lastError := readWebhookDelivery(t, db, unsubscribed)
	if status != "suppressed" || attempt != 0 || lastError == nil || *lastError != "event is not subscribed" {
		t.Fatalf("unsubscribed = %s/%d/%v", status, attempt, lastError)
	}
	status, attempt, _, lastError = readWebhookDelivery(t, db, unsupported)
	if status != "failed" || attempt != 0 || lastError == nil || *lastError != "unsupported webhook category" {
		t.Fatalf("unsupported = %s/%d/%v", status, attempt, lastError)
	}
	status, attempt, _, lastError = readWebhookDelivery(t, db, empty)
	if status != "failed" || attempt != 0 || lastError == nil || *lastError != "webhook payload is missing" {
		t.Fatalf("empty = %s/%d/%v", status, attempt, lastError)
	}
	// 本地数据问题不是端点故障：不得累计连续失败。
	if exists, _ := rdb.Exists(ctx, WebhookFailStreakKey).Result(); exists != 0 {
		t.Fatalf("本地数据问题被计入 fail_streak")
	}
	if sender.callCount() != 1 {
		t.Fatalf("Send 调用数 = %d，只有订阅过滤那条应进入发送器", sender.callCount())
	}
}

func TestWebhookWorkerInactiveLeavesQueueUntouched(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "inactive:1", category: "billing.balance_low", payload: `{"event":"balance_low"}`,
	})
	sender := &fakeWebhookWorkerSender{active: false}
	worker := webhookWorkerForTest(t, db, rdb, sender, now)
	if result := worker.RunOnce(ctx); result.Claimed != 0 {
		t.Fatalf("未启用时仍领取：%+v", result)
	}
	status, attempt, next, _ := readWebhookDelivery(t, db, id)
	if status != "queued" || attempt != 0 || next != nil {
		t.Fatalf("未启用时行被改动：%s/%d/%v", status, attempt, next)
	}
}

func TestWebhookWorkerLogsAndLastErrorAreSanitized(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	const secretURL = "https://hooks.example.com/hook?token=super-secret-value"
	const payloadSecret = "payload-secret"
	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "sanitize:1", category: "billing.balance_low",
		payload: `{"event":"balance_low","secret":"` + payloadSecret + `"}`,
	})
	sender := &fakeWebhookWorkerSender{
		active: true,
		onSend: func(webhookWorkerCall) (WebhookResult, error) {
			// 模拟底层错误携带完整 URL 与响应体；worker 必须只保留分类文案。
			return WebhookResult{}, fmt.Errorf("%w: %s body=%s", ErrWebhookTransport, secretURL, payloadSecret)
		},
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store, _ := NewStore(db)
	worker, err := NewWebhookWorker(WebhookWorkerDeps{
		Store: store, Sender: sender, Redis: rdb, Logger: logger,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewWebhookWorker: %v", err)
	}
	worker.RunOnce(ctx)

	_, _, _, lastError := readWebhookDelivery(t, db, id)
	if lastError == nil || *lastError != "webhook request failed" {
		t.Fatalf("last_error = %v", lastError)
	}
	logs := buf.String()
	for _, leak := range []string{"super-secret-value", "token=", "hooks.example.com", payloadSecret} {
		if strings.Contains(logs, leak) {
			t.Fatalf("日志泄露 %q:\n%s", leak, logs)
		}
	}
	if strings.Contains(*lastError, "hooks.example.com") || strings.Contains(*lastError, "token=") {
		t.Fatalf("last_error 泄露目标地址：%q", *lastError)
	}
}

func TestWebhookWorkerStartupScanAndShutdown(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "startup:1", category: "billing.payment_success",
		payload: `{"event":"payment_success"}`,
	})
	sender := &fakeWebhookWorkerSender{active: true, sent: make(chan struct{}, 1)}
	store, _ := NewStore(db)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWebhookWorkerLoop(ctx, WebhookWorkerDeps{
			Store: store, Sender: sender, Redis: rdb,
			Now: func() time.Time { return now },
			// 周期远大于测试时长：能收到发送信号就证明是启动即扫，而不是 ticker。
			Interval: time.Hour,
		})
	}()

	select {
	case <-sender.sent:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("启动后未立即扫描队列")
	}
	// Send 的返回值与 MarkSent 提交之间有一个很短窗口；等状态落库后再停机，
	// 否则 ctx 取消会让 UPDATE 失败并回滚（行保持 queued，这是预期的停机
	// 安全行为，但不是本用例要断言的路径）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, attempt, _, _ := readWebhookDelivery(t, db, id)
		if status == "sent" && attempt == 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("启动扫描后 = %s/%d，期望 sent/1", status, attempt)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWebhookWorkerLoop = %v，期望 nil（优雅停机）", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 worker 未退出")
	}

	status, attempt, _, _ := readWebhookDelivery(t, db, id)
	if status != "sent" || attempt != 1 {
		t.Fatalf("启动扫描后 = %s/%d", status, attempt)
	}

	// 停机后不留下悬挂占用：可以再次领取到不存在的行（队列为空）。
	if _, ok, err := store.ClaimWebhookDelivery(context.Background(), now); err != nil || ok {
		t.Fatalf("停机后 claim = %v,%v，期望空队列", ok, err)
	}
}

// 保证 fake 与生产发送器接口一致（编译期钉死）。
var _ WebhookWorkerSender = (*fakeWebhookWorkerSender)(nil)
var _ WebhookWorkerSender = (*WebhookSender)(nil)

// deadWebhookRedis 返回一个必定连不上的客户端（127.0.0.1:1 无监听）：
// 用于验证 Redis 故障时 isPaused/countFailure 的 fail-open 语义。
func deadWebhookRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		MaxRetries:   -1,
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestWebhookWorkerSkipsNotDueRows 是 LOW-3 的回归：next_retry_at 尚未到期
// 的行不得被领取，attempt/status 原样保留。
func TestWebhookWorkerSkipsNotDueRows(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	rdb := newWebhookWorkerRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	future := now.Add(5 * time.Minute)
	id := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "not-due:1", category: "billing.balance_low",
		payload: `{"event":"balance_low"}`, next: &future,
	})
	sender := &fakeWebhookWorkerSender{active: true}
	worker := webhookWorkerForTest(t, db, rdb, sender, now)
	if result := worker.RunOnce(ctx); result.Claimed != 0 {
		t.Fatalf("未到期行被领取：%+v", result)
	}
	if sender.callCount() != 0 {
		t.Fatalf("未到期行仍被发送 %d 次", sender.callCount())
	}
	status, attempt, next, _ := readWebhookDelivery(t, db, id)
	if status != "queued" || attempt != 0 || next == nil || !next.Equal(future) {
		t.Fatalf("行被改动：%s/%d/%v", status, attempt, next)
	}
}

// TestWebhookWorkerReleasesRowsWhenConfigDisappears 是 LOW-2 的回归：
// 投递期间配置被关闭（ErrWebhookDisabled）或清空（ErrWebhookNotConfigured）
// 都必须 Release——不消耗 attempt、不计连续失败，且本轮立即停止。
func TestWebhookWorkerReleasesRowsWhenConfigDisappears(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"disabled", ErrWebhookDisabled},
		{"not_configured", ErrWebhookNotConfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openNotifyTestDB(t)
			cleanWebhookDeliveries(t, db)
			rdb := newWebhookWorkerRedis(t)
			ctx := context.Background()
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

			ids := []int64{
				insertWebhookDelivery(t, db, webhookDeliverySeed{
					dedupe: "release-a", category: "billing.balance_low",
					payload: `{"event":"balance_low"}`,
				}),
				insertWebhookDelivery(t, db, webhookDeliverySeed{
					dedupe: "release-b", category: "billing.balance_low",
					payload: `{"event":"balance_low"}`,
				}),
			}
			sender := &fakeWebhookWorkerSender{
				active: true,
				onSend: func(webhookWorkerCall) (WebhookResult, error) {
					return WebhookResult{}, tc.err
				},
			}
			worker := webhookWorkerForTest(t, db, rdb, sender, now)
			result := worker.RunOnce(ctx)
			if result.Claimed != 1 || result.Sent != 0 || result.Failed != 0 {
				t.Fatalf("result = %+v，期望只领取 1 条并释放", result)
			}
			// 释放后本轮必须停止：第二条不应被同一轮尝试。
			if sender.callCount() != 1 {
				t.Fatalf("Send 调用数 = %d，期望 1", sender.callCount())
			}
			for _, id := range ids {
				status, attempt, next, lastError := readWebhookDelivery(t, db, id)
				if status != "queued" || attempt != 0 || next != nil || lastError != nil {
					t.Fatalf("%s 被消耗：%s/%d/%v/%v", tc.name, status, attempt, next, lastError)
				}
			}
			if exists, err := rdb.Exists(ctx, WebhookFailStreakKey).Result(); err != nil || exists != 0 {
				t.Fatalf("连续失败计数被写入：exists=%d err=%v", exists, err)
			}
		})
	}
}

// TestWebhookWorkerRedisFailureFailsOpen 是 LOW-3 的回归：Redis 不可用时
// isPaused 视为未暂停、countFailure 不阻断投递（fail-open），正确性仍由
// 投递行的行锁与重试状态保证。
func TestWebhookWorkerRedisFailureFailsOpen(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanWebhookDeliveries(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	t.Run("isPaused", func(t *testing.T) {
		id := insertWebhookDelivery(t, db, webhookDeliverySeed{
			dedupe: "redis-fail-paused", category: "billing.balance_low",
			payload: `{"event":"balance_low"}`,
		})
		sender := &fakeWebhookWorkerSender{active: true}
		worker := webhookWorkerForTest(t, db, deadWebhookRedis(t), sender, now)
		result := worker.RunOnce(ctx)
		if result.Paused || result.Claimed != 1 || result.Sent != 1 {
			t.Fatalf("Redis 故障时不应暂停/跳过：%+v", result)
		}
		status, attempt, _, _ := readWebhookDelivery(t, db, id)
		if status != "sent" || attempt != 1 {
			t.Fatalf("投递未完成：%s/%d", status, attempt)
		}
	})

	t.Run("countFailure", func(t *testing.T) {
		id := insertWebhookDelivery(t, db, webhookDeliverySeed{
			dedupe: "redis-fail-count", category: "billing.balance_low",
			payload: `{"event":"balance_low"}`,
		})
		sender := &fakeWebhookWorkerSender{
			active: true,
			onSend: func(webhookWorkerCall) (WebhookResult, error) {
				return WebhookResult{}, ErrWebhookTransport
			},
		}
		worker := webhookWorkerForTest(t, db, deadWebhookRedis(t), sender, now)
		result := worker.RunOnce(ctx)
		if result.Failed != 1 {
			t.Fatalf("计数失败不应阻断失败落库：%+v", result)
		}
		status, attempt, next, lastError := readWebhookDelivery(t, db, id)
		if status != "queued" || attempt != 1 || next == nil ||
			lastError == nil || *lastError != "webhook request failed" {
			t.Fatalf("失败状态未落库：%s/%d/%v/%v", status, attempt, next, lastError)
		}
	})
}

// installWebhookMarkFailureTrigger 让指定投递行的 UPDATE 抛错，用于验证
// MarkSent/MarkFailed 失败时的「中止本轮」语义（LOW-1）。
func installWebhookMarkFailureTrigger(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	const fn = "newtg_test_fail_mark_update"
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.id = %d THEN
				RAISE EXCEPTION 'forced mark failure';
			END IF;
			RETURN NEW;
		END;
		$$`, fn, id)); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE UPDATE ON notification_deliveries
		FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn)); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON notification_deliveries`, fn))
		_, _ = db.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// TestWebhookWorkerAbortsRoundWhenMarkFails 是 LOW-1 的回归：MarkSent 或
// MarkFailed 落库失败时，占用已回滚、行仍是 queued；本轮必须立即停止，
// 否则同一行会被同一轮再次领取并重复发送。
func TestWebhookWorkerAbortsRoundWhenMarkFails(t *testing.T) {
	cases := []struct {
		name   string
		sender func(call webhookWorkerCall) (WebhookResult, error)
	}{
		{
			name: "mark_sent",
			sender: func(webhookWorkerCall) (WebhookResult, error) {
				return WebhookResult{StatusCode: 200, OK: true}, nil
			},
		},
		{
			name: "mark_failed",
			sender: func(webhookWorkerCall) (WebhookResult, error) {
				return WebhookResult{}, ErrWebhookTransport
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openNotifyTestDB(t)
			cleanWebhookDeliveries(t, db)
			rdb := newWebhookWorkerRedis(t)
			ctx := context.Background()
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

			first := insertWebhookDelivery(t, db, webhookDeliverySeed{
				dedupe: "mark-fail-1", category: "billing.balance_low",
				payload: `{"event":"balance_low"}`,
			})
			second := insertWebhookDelivery(t, db, webhookDeliverySeed{
				dedupe: "mark-fail-2", category: "billing.balance_low",
				payload: `{"event":"balance_low"}`,
			})
			installWebhookMarkFailureTrigger(t, db, first)

			sender := &fakeWebhookWorkerSender{active: true, onSend: tc.sender}
			worker := webhookWorkerForTest(t, db, rdb, sender, now)
			result := worker.RunOnce(ctx)
			if result.Claimed != 1 || result.Sent != 0 || result.Failed != 0 {
				t.Fatalf("result = %+v，期望领取 1 条后中止且不计数", result)
			}
			// 关键断言：同一轮不得再领取/发送第二条。
			if sender.callCount() != 1 {
				t.Fatalf("Send 调用数 = %d，期望 1（Mark 失败后必须中止本轮）", sender.callCount())
			}
			for _, id := range []int64{first, second} {
				status, attempt, _, _ := readWebhookDelivery(t, db, id)
				if status != "queued" || attempt != 0 {
					t.Fatalf("行 %d 未回到 queued/attempt=0：%s/%d", id, status, attempt)
				}
			}
		})
	}
}
