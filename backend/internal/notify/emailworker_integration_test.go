//go:build integration

package notify

// W10-EMAIL 的真实 PostgreSQL 验收（fake SMTP）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w10e?sslmode=disable' \
//	  go test -tags=integration ./internal/notify/ -run Email -v
//
// 覆盖：领取竞争（SKIP LOCKED）、渠道隔离、成功写 Message-ID、1/5/25 重试
// 与 4 次上限、终态 sent/failed/suppressed、偏好 opt-out 的入队与发送前
// 两次判定、事务性类别不被抑制、7 语渲染、模板缺失/停用/变量缺失的终态、
// SMTP 未配置不领取、10 条/秒边界、停机 drain。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEmailWorkerSender 记录每次发送并可按调用编排结果。
type fakeEmailWorkerSender struct {
	mu         sync.Mutex
	configured bool
	calls      []emailWorkerCall
	onSend     func(call emailWorkerCall) (string, error)
	delay      time.Duration
	sent       chan struct{}
	// started 在 Send 进入时非阻塞发送；blockUntilCancel 让 Send 等到 ctx
	// 取消后返回 ctx.Err()（停机 drain 用例）。
	started          chan struct{}
	blockUntilCancel bool
}

type emailWorkerCall struct {
	to      string
	subject string
	body    string
}

func (f *fakeEmailWorkerSender) Configured(context.Context) bool { return f.configured }

func (f *fakeEmailWorkerSender) Send(ctx context.Context, to, subject, body string) (string, error) {
	call := emailWorkerCall{to: to, subject: subject, body: body}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.blockUntilCancel {
		<-ctx.Done()
		return "", ctx.Err()
	}
	var messageID string
	var err error
	if f.onSend != nil {
		messageID, err = f.onSend(call)
	} else {
		messageID = "<test-message@example.com>"
	}
	if f.sent != nil {
		select {
		case f.sent <- struct{}{}:
		default:
		}
	}
	return messageID, err
}

func (f *fakeEmailWorkerSender) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeEmailWorkerSender) callsSnapshot() []emailWorkerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]emailWorkerCall(nil), f.calls...)
}

// emailTestConfig 是 ConfigReader 的静态替身。
type emailTestConfig map[string]json.RawMessage

func (c emailTestConfig) Get(key string) (json.RawMessage, bool) {
	value, ok := c[key]
	return value, ok
}

func emailTestSiteConfig() emailTestConfig {
	return emailTestConfig{
		siteServerAddressKey: json.RawMessage(`"https://gw.example.com/"`),
		siteTitleKey:         json.RawMessage(`"Example Gateway"`),
	}
}

var testUserSeq int64

// insertTestUser 插入一个用户并返回 id。用户名按测试进程序号生成，满足
// users_username_chk 且避免表级唯一冲突。
func insertTestUser(t *testing.T, db *sql.DB, email, language, prefs string, role string) int64 {
	t.Helper()
	username := fmt.Sprintf("w10e%d", atomic.AddInt64(&testUserSeq, 1))
	if role == "" {
		role = "user"
	}
	var prefsArg any
	if strings.TrimSpace(prefs) != "" {
		prefsArg = prefs
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO users (username, email, language, notification_preferences, role)
		VALUES ($1, $2, $3, $4::jsonb, $5) RETURNING id`,
		username, email, language, prefsArg, role).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

type emailDeliverySeed struct {
	dedupe      string
	templateKey string
	category    string
	recipient   string
	userID      *int64
	locale      string
	payload     any
	attempt     int
	next        *time.Time
	status      string
}

func insertEmailDelivery(t *testing.T, db *sql.DB, seed emailDeliverySeed) int64 {
	t.Helper()
	if seed.status == "" {
		seed.status = "queued"
	}
	if seed.templateKey == "" {
		seed.templateKey = seed.category
	}
	var templateKey any
	if seed.templateKey != "" {
		templateKey = seed.templateKey
	}
	var locale any
	if seed.locale != "" {
		locale = seed.locale
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO notification_deliveries
		(template_key, category, channel, recipient, recipient_user_id, locale,
		 dedupe_key, status, attempt, next_retry_at, payload)
		VALUES ($1, $2, 'email', $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		templateKey, seed.category, seed.recipient, seed.userID, locale,
		seed.dedupe, seed.status, seed.attempt, seed.next, seed.payload).Scan(&id); err != nil {
		t.Fatalf("insert email delivery: %v", err)
	}
	return id
}

func readEmailDelivery(t *testing.T, db *sql.DB, id int64) (status string, attempt int, next *time.Time, lastError *string, providerID *string) {
	t.Helper()
	var (
		nextRetry sql.NullTime
		lastErr   sql.NullString
		provider  sql.NullString
	)
	if err := db.QueryRow(`SELECT status, attempt, next_retry_at, last_error, provider_message_id
		FROM notification_deliveries WHERE id = $1`, id).
		Scan(&status, &attempt, &nextRetry, &lastErr, &provider); err != nil {
		t.Fatalf("read email delivery: %v", err)
	}
	if nextRetry.Valid {
		value := nextRetry.Time
		next = &value
	}
	if lastErr.Valid {
		value := lastErr.String
		lastError = &value
	}
	if provider.Valid {
		value := provider.String
		providerID = &value
	}
	return status, attempt, next, lastError, providerID
}

func emailWorkerForTest(t *testing.T, db *sql.DB, sender EmailWorkerSender, now time.Time) *EmailWorker {
	t.Helper()
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	worker, err := NewEmailWorker(EmailWorkerDeps{
		Store:  store,
		Sender: sender,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewEmailWorker: %v", err)
	}
	return worker
}

func emailPayload(t *testing.T, values map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(raw)
}

func paymentFailedPayload(t *testing.T) string {
	return emailPayload(t, map[string]any{
		"amount":       "¥50.00",
		"order_no":     "20261022-ABC123",
		"reason_label": "card declined",
		"failed_at":    "2026-10-22T12:00:00Z",
		"retry_url":    "https://gw.example.com/wallet",
	})
}

func TestEmailWorkerSuccessMarksSentWithMessageID(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	id := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe:    "email:payment_failed:1",
		category:  "billing.payment_failed",
		recipient: "payer@example.com",
		locale:    "en",
		payload:   paymentFailedPayload(t),
	})
	sender := &fakeEmailWorkerSender{configured: true}
	worker := emailWorkerForTest(t, db, sender, now)

	result := worker.RunOnce(ctx)
	if result.Claimed != 1 || result.Sent != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	status, attempt, next, lastError, providerID := readEmailDelivery(t, db, id)
	if status != "sent" || attempt != 1 || next != nil || lastError != nil {
		t.Fatalf("delivery = %s/%d/%v/%v", status, attempt, next, lastError)
	}
	if providerID == nil || *providerID != "<test-message@example.com>" {
		t.Fatalf("provider_message_id = %v", providerID)
	}
	calls := sender.callsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("Send 调用数 = %d", len(calls))
	}
	if calls[0].to != "payer@example.com" {
		t.Fatalf("收件人 = %q，期望投递行的 recipient", calls[0].to)
	}
	if strings.Contains(calls[0].subject, "{{") || strings.Contains(calls[0].body, "{{") {
		t.Fatalf("渲染残留模板变量：subject=%q body=%q", calls[0].subject, calls[0].body)
	}
	if !strings.Contains(calls[0].subject, "20261022-ABC123") {
		t.Fatalf("subject 未包含订单号：%q", calls[0].subject)
	}
}

func TestEmailWorkerRetryScheduleEndsFailed(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	id := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe:    "email:payment_failed:retry",
		category:  "billing.payment_failed",
		recipient: "retry@example.com",
		locale:    "en",
		payload:   paymentFailedPayload(t),
	})
	sender := &fakeEmailWorkerSender{
		configured: true,
		onSend: func(emailWorkerCall) (string, error) {
			return "", fmt.Errorf("smtp: 550 mailbox unavailable")
		},
	}
	worker := emailWorkerForTest(t, db, sender, now)

	wantDelays := []time.Duration{time.Minute, 5 * time.Minute, 25 * time.Minute}
	for i, wantDelay := range wantDelays {
		result := worker.RunOnce(ctx)
		if result.Failed != 1 {
			t.Fatalf("第 %d 轮 result = %+v", i+1, result)
		}
		status, attempt, next, lastError, _ := readEmailDelivery(t, db, id)
		if status != "queued" || attempt != i+1 {
			t.Fatalf("第 %d 次失败后 = %s/%d", i+1, status, attempt)
		}
		if next == nil || !next.Equal(now.Add(wantDelay)) {
			t.Fatalf("第 %d 次失败后 next_retry_at = %v，期望 %v", i+1, next, now.Add(wantDelay))
		}
		if lastError == nil || *lastError != "smtp delivery failed" {
			t.Fatalf("last_error = %v（不得回显 SMTP 原文）", lastError)
		}
		if _, err := db.Exec(`UPDATE notification_deliveries SET next_retry_at = $2 WHERE id = $1`,
			id, now.Add(-time.Second)); err != nil {
			t.Fatalf("rewind next_retry_at: %v", err)
		}
	}

	result := worker.RunOnce(ctx)
	if result.Failed != 1 {
		t.Fatalf("第 4 轮 result = %+v", result)
	}
	status, attempt, next, _, _ := readEmailDelivery(t, db, id)
	if status != "failed" || attempt != emailMaxAttempts || next != nil {
		t.Fatalf("终态 = %s/%d/%v", status, attempt, next)
	}
	if sender.callCount() != emailMaxAttempts {
		t.Fatalf("Send 调用数 = %d，期望 %d", sender.callCount(), emailMaxAttempts)
	}
}

func TestEmailWorkerClaimsOnlyEmailAndEachRowOnce(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	const rows = 10
	wantRecipient := map[string]string{}
	for i := 0; i < rows; i++ {
		dedupe := fmt.Sprintf("email:payment_failed:%d", i)
		recipient := fmt.Sprintf("payer%d@example.com", i)
		wantRecipient[dedupe] = recipient
		insertEmailDelivery(t, db, emailDeliverySeed{
			dedupe: dedupe, category: "billing.payment_failed", recipient: recipient,
			locale: "en", payload: paymentFailedPayload(t),
		})
	}
	// webhook 行不能被 email worker 领取（否则请求体会被当邮件正文发出去）。
	webhookID := insertWebhookDelivery(t, db, webhookDeliverySeed{
		dedupe: "webhook-neighbor", category: "billing.balance_low",
		payload: `{"event":"balance_low"}`,
	})

	// 先直接验证行锁：A 占用后 B 必须拿到另一行，绝不拿到同一行。
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	claimA, ok, err := store.ClaimEmailDelivery(ctx, now)
	if err != nil || !ok {
		t.Fatalf("claim A = %v,%v", ok, err)
	}
	claimB, okB, err := store.ClaimEmailDelivery(ctx, now)
	if err != nil || !okB {
		t.Fatalf("claim B = %v,%v", okB, err)
	}
	if claimA.Delivery.ID == claimB.Delivery.ID {
		t.Fatalf("B 拿到了被 A 锁住的同一行（应 SKIP LOCKED）")
	}
	if claimA.Delivery.Channel != ChannelEmail || claimB.Delivery.Channel != ChannelEmail {
		t.Fatalf("领取到了非 email 行：%s/%s", claimA.Delivery.Channel, claimB.Delivery.Channel)
	}
	claimA.Release()
	claimB.Release()

	sender := &fakeEmailWorkerSender{configured: true, delay: 10 * time.Millisecond}
	newWorker := func() *EmailWorker {
		store, _ := NewStore(db)
		w, err := NewEmailWorker(EmailWorkerDeps{
			Store: store, Sender: sender,
			Now:       func() time.Time { return now },
			BatchSize: rows * 2,
		})
		if err != nil {
			t.Fatalf("NewEmailWorker: %v", err)
		}
		return w
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); newWorker().RunOnce(ctx) }()
	go func() { defer wg.Done(); newWorker().RunOnce(ctx) }()
	wg.Wait()

	if sender.callCount() != rows {
		t.Fatalf("Send 调用数 = %d，期望 %d（重复或遗漏）", sender.callCount(), rows)
	}
	seen := map[string]bool{}
	for _, call := range sender.callsSnapshot() {
		if seen[call.to] {
			t.Fatalf("收件人 %q 被重复发送", call.to)
		}
		seen[call.to] = true
	}
	status, attempt, _, _ := readWebhookDelivery(t, db, webhookID)
	if status != "queued" || attempt != 0 {
		t.Fatalf("webhook 行被 email worker 改动：%s/%d", status, attempt)
	}
}

func TestEmailWorkerSendTimePreferenceGate(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	optedOut := insertTestUser(t, db, "optout@example.com", "en", `{}`, "user")
	optedIn := insertTestUser(t, db, "optin@example.com", "en",
		`{"balance_warning":{"enabled":true,"channels":["email"]}}`, "user")

	lowBalance := emailPayload(t, map[string]any{
		"balance": "10", "threshold": "100",
		"topup_url": "https://gw.example.com/wallet", "site_title": "Example Gateway",
	})
	optedOutID := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:balance_low:optout", category: "billing.balance_low",
		recipient: "optout@example.com", userID: &optedOut, locale: "en",
		payload: lowBalance,
	})
	optedInID := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:balance_low:optin", category: "billing.balance_low",
		recipient: "optin@example.com", userID: &optedIn, locale: "en",
		payload: lowBalance,
	})
	// 事务性类别即使同一用户偏好关闭也必须发送。
	transactionalID := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:payment_failed:optout", category: "billing.payment_failed",
		recipient: "optout@example.com", userID: &optedOut, locale: "en",
		payload: paymentFailedPayload(t),
	})

	sender := &fakeEmailWorkerSender{configured: true}
	worker := emailWorkerForTest(t, db, sender, now)
	result := worker.RunOnce(ctx)
	if result.Claimed != 3 || result.Sent != 2 || result.Suppressed != 1 {
		t.Fatalf("result = %+v", result)
	}
	status, attempt, _, lastError, _ := readEmailDelivery(t, db, optedOutID)
	if status != "suppressed" || attempt != 0 || lastError == nil || *lastError != "recipient opted out of email" {
		t.Fatalf("opt-out 行 = %s/%d/%v", status, attempt, lastError)
	}
	if status, _, _, _, _ := readEmailDelivery(t, db, optedInID); status != "sent" {
		t.Fatalf("opt-in 行 status = %s", status)
	}
	if status, _, _, _, _ := readEmailDelivery(t, db, transactionalID); status != "sent" {
		t.Fatalf("事务性行被抑制：%s", status)
	}

	// 发送前复核：用户入队后退订 → suppressed；事务性不受影响。
	if _, err := db.Exec(`UPDATE users SET notification_preferences =
		'{"balance_warning":{"enabled":false}}'::jsonb WHERE id = $1`, optedIn); err != nil {
		t.Fatalf("update prefs: %v", err)
	}
	pendingID := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:balance_low:later-optout", category: "billing.balance_low",
		recipient: "optin@example.com", userID: &optedIn, locale: "en",
		payload: lowBalance,
	})
	worker.RunOnce(ctx)
	if status, attempt, _, _, _ := readEmailDelivery(t, db, pendingID); status != "suppressed" || attempt != 0 {
		t.Fatalf("发送前退订未抑制：%s/%d", status, attempt)
	}
	// 删除用户后，非事务性投递不得继续发往失效账号。
	deletedUser := insertTestUser(t, db, "gone@example.com", "en",
		`{"balance_warning":{"enabled":true,"channels":["email"]}}`, "user")
	goneID := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:balance_low:gone", category: "billing.balance_low",
		recipient: "gone@example.com", userID: &deletedUser, locale: "en",
		payload: lowBalance,
	})
	if _, err := db.Exec(`DELETE FROM users WHERE id = $1`, deletedUser); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	worker.RunOnce(ctx)
	if status, _, _, _, _ := readEmailDelivery(t, db, goneID); status != "suppressed" {
		t.Fatalf("已删除用户的投递 status = %s，期望 suppressed", status)
	}
	// 只有 opt-in 的余额提醒与事务性邮件真正发出；退订/删除的行都没调用 SMTP。
	if sender.callCount() != 2 {
		t.Fatalf("Send 调用数 = %d，期望 2（opt-in 余额 + 事务性）", sender.callCount())
	}
}

func TestEmailProducerPreferenceGateAtEnqueue(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()

	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	producer, err := NewEmailEventProducer(store, emailTestSiteConfig(), nil)
	if err != nil {
		t.Fatalf("NewEmailEventProducer: %v", err)
	}

	optedOut := insertTestUser(t, db, "optout@example.com", "en",
		`{"balance_warning":{"enabled":false}}`, "user")
	optedIn := insertTestUser(t, db, "optin@example.com", "zh-CN",
		`{"balance_warning":{"enabled":true,"channels":["email"],"custom_email":"alt@example.com"}}`, "user")

	// 入队时 opt-out → 直接写 suppressed，不进入发送队列。
	result, err := producer.EnqueueBalanceLow(ctx, BalanceLowEmailEvent{
		UserID: optedOut, Balance: 10, Threshold: 100,
		DetectedAt: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), SiteDay: "2026-10-22",
	})
	if err != nil {
		t.Fatalf("enqueue opted-out: %v", err)
	}
	if result.Status != EmailEnqueueSuppressed || !result.Inserted {
		t.Fatalf("opt-out 入队结果 = %+v，期望 suppressed+inserted", result)
	}

	// 入队时 opt-in → queued，收件人用 custom_email，语言取 users.language。
	result, err = producer.EnqueueBalanceLow(ctx, BalanceLowEmailEvent{
		UserID: optedIn, Balance: 10, Threshold: 100,
		DetectedAt: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), SiteDay: "2026-10-22",
	})
	if err != nil {
		t.Fatalf("enqueue opted-in: %v", err)
	}
	if result.Status != EmailEnqueueQueued || !result.Inserted {
		t.Fatalf("opt-in 入队结果 = %+v", result)
	}
	var recipient, locale, status string
	if err := db.QueryRow(`SELECT recipient, locale, status FROM notification_deliveries
		WHERE dedupe_key = 'email:balance_low:' || $1::bigint::text || ':2026-10-22'`, optedIn).
		Scan(&recipient, &locale, &status); err != nil {
		t.Fatalf("read queued row: %v", err)
	}
	if recipient != "alt@example.com" || locale != "zh-CN" || status != "queued" {
		t.Fatalf("queued 行 = %s/%s/%s", recipient, locale, status)
	}
	// 同一事件重复入队 → dedupe，不写第二行。
	again, err := producer.EnqueueBalanceLow(ctx, BalanceLowEmailEvent{
		UserID: optedIn, Balance: 10, Threshold: 100,
		DetectedAt: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), SiteDay: "2026-10-22",
	})
	if err != nil || again.Status != EmailEnqueueDuplicate {
		t.Fatalf("重复入队 = %+v,%v，期望 duplicate", again, err)
	}

	// 事务性类别：同一 opt-out 用户仍然入队。
	result, err = producer.EnqueuePaymentFailed(ctx, PaymentFailedEmailEvent{
		UserID: optedOut, AmountLabel: "¥50.00", OrderNo: "20261022-ABC123",
		ReasonLabel: "card declined", FailedAt: time.Now(), DedupeKey: "order:42:payment_failed",
	})
	if err != nil {
		t.Fatalf("enqueue payment_failed: %v", err)
	}
	if result.Status != EmailEnqueueQueued {
		t.Fatalf("事务性入队被偏好拦截：%+v", result)
	}
}

func TestEmailWorkerRendersAllSevenLocales(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	locales := []string{"en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"}
	ids := make([]int64, 0, len(locales))
	for i, locale := range locales {
		ids = append(ids, insertEmailDelivery(t, db, emailDeliverySeed{
			dedupe:   fmt.Sprintf("email:payment_failed:locale:%d", i),
			category: "billing.payment_failed", recipient: "payer@example.com",
			locale: locale, payload: paymentFailedPayload(t),
		}))
	}
	sender := &fakeEmailWorkerSender{configured: true}
	worker := emailWorkerForTest(t, db, sender, now)
	result := worker.RunOnce(ctx)
	if result.Claimed != len(locales) || result.Sent != len(locales) {
		t.Fatalf("result = %+v", result)
	}
	calls := sender.callsSnapshot()
	if len(calls) != len(locales) {
		t.Fatalf("Send 调用数 = %d", len(calls))
	}
	// 领取顺序 ORDER BY id，与插入顺序一致：calls[i] 对应 locales[i]。
	for i, call := range calls {
		if strings.TrimSpace(call.subject) == "" || strings.TrimSpace(call.body) == "" {
			t.Fatalf("%s 空邮件：subject=%q body=%q", locales[i], call.subject, call.body)
		}
		if strings.Contains(call.subject, "{{") || strings.Contains(call.body, "{{") {
			t.Fatalf("%s 渲染残留：%q / %q", locales[i], call.subject, call.body)
		}
		status, _, _, _, _ := readEmailDelivery(t, db, ids[i])
		if status != "sent" {
			t.Fatalf("%s 行 status = %s", locales[i], status)
		}
	}
	if calls[0].body == calls[1].body {
		t.Fatal("zh-CN 与 en 使用了同一份文案，语言回退链未生效")
	}
	if !strings.Contains(calls[0].subject, "[Top-up failed]") ||
		!strings.Contains(calls[1].subject, "充值未完成") {
		t.Fatalf("语言选择错误：en=%q zh-CN=%q", calls[0].subject, calls[1].subject)
	}
}

func TestEmailWorkerTemplateProblemsAreTerminal(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	// 模板行全部缺失 → failed，不重试。
	if _, err := db.Exec(`DELETE FROM notification_templates WHERE "key" = 'billing.order_expired'`); err != nil {
		t.Fatalf("delete templates: %v", err)
	}
	missing := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:order_expired:missing", category: "billing.order_expired",
		recipient: "payer@example.com", locale: "en", payload: `{}`,
	})
	// 模板停用 → failed，不重试。
	if _, err := db.Exec(`UPDATE notification_templates SET enabled = false
		WHERE "key" = 'billing.usage_digest' AND locale = 'en'`); err != nil {
		t.Fatalf("disable template: %v", err)
	}
	disabled := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:usage_digest:disabled", category: "billing.usage_digest",
		recipient: "payer@example.com", locale: "en", payload: `{}`,
	})
	// 必填变量缺失 → failed，last_error=missing_var:{name}。
	missingVar := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:payment_failed:missing-var", category: "billing.payment_failed",
		recipient: "payer@example.com", locale: "en",
		payload: emailPayload(t, map[string]any{"amount": "¥50.00"}),
	})

	sender := &fakeEmailWorkerSender{configured: true}
	worker := emailWorkerForTest(t, db, sender, now)
	result := worker.RunOnce(ctx)
	if result.Claimed != 3 || result.Failed != 3 || result.Sent != 0 {
		t.Fatalf("result = %+v", result)
	}
	if sender.callCount() != 0 {
		t.Fatalf("不可渲染的投递进入了 SMTP：%d 次", sender.callCount())
	}
	cases := []struct {
		id     int64
		reason string
	}{
		{missing, "email template is missing for all locales"},
		{disabled, "email template is disabled"},
		{missingVar, "missing_var:order_no"},
	}
	for _, tc := range cases {
		status, attempt, next, lastError, _ := readEmailDelivery(t, db, tc.id)
		if status != "failed" || attempt != 0 || next != nil {
			t.Fatalf("%s = %s/%d/%v", tc.reason, status, attempt, next)
		}
		if lastError == nil || *lastError != tc.reason {
			t.Fatalf("last_error = %v，期望 %q", lastError, tc.reason)
		}
	}
}

func TestEmailWorkerSMTPNotConfiguredLeavesQueueUntouched(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	id := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:payment_failed:unconfigured", category: "billing.payment_failed",
		recipient: "payer@example.com", locale: "en", payload: paymentFailedPayload(t),
	})
	sender := &fakeEmailWorkerSender{configured: false}
	worker := emailWorkerForTest(t, db, sender, now)
	if result := worker.RunOnce(ctx); result.Claimed != 0 {
		t.Fatalf("SMTP 未配置时仍领取：%+v", result)
	}
	status, attempt, next, _, _ := readEmailDelivery(t, db, id)
	if status != "queued" || attempt != 0 || next != nil {
		t.Fatalf("未配置时行被改动：%s/%d/%v", status, attempt, next)
	}
}

func TestEmailWorkerRateLimitTenPerSecond(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Now()

	const rows = 11
	for i := 0; i < rows; i++ {
		insertEmailDelivery(t, db, emailDeliverySeed{
			dedupe: fmt.Sprintf("email:rate:%d", i), category: "billing.payment_failed",
			recipient: "payer@example.com", locale: "en", payload: paymentFailedPayload(t),
		})
	}
	sender := &fakeEmailWorkerSender{configured: true}
	worker := emailWorkerForTest(t, db, sender, now)
	start := time.Now()
	result := worker.RunOnce(ctx)
	elapsed := time.Since(start)
	if result.Sent != rows {
		t.Fatalf("sent = %d，期望 %d", result.Sent, rows)
	}
	// 前 10 个立即通过，第 11 个必须排队约 100ms。给足调度余量。
	if elapsed < 90*time.Millisecond {
		t.Fatalf("11 封邮件耗时 %v，未体现 10 条/秒限流", elapsed)
	}
}

func TestEmailWorkerLoopStartupScanAndShutdown(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	id := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:startup:1", category: "billing.payment_failed",
		recipient: "payer@example.com", locale: "en", payload: paymentFailedPayload(t),
	})
	sender := &fakeEmailWorkerSender{configured: true, sent: make(chan struct{}, 1)}
	store, _ := NewStore(db)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunEmailWorkerLoop(ctx, EmailWorkerDeps{
			Store: store, Sender: sender,
			Now:      func() time.Time { return now },
			Interval: time.Hour,
		})
	}()

	select {
	case <-sender.sent:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("启动后未立即扫描队列")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, attempt, _, _, _ := readEmailDelivery(t, db, id)
		if status == "sent" && attempt == 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("启动扫描后 = %s/%d", status, attempt)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunEmailWorkerLoop = %v，期望 nil（优雅停机）", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 worker 未退出")
	}
}

// TestEmailWorkerShutdownReleasesInFlightDelivery 验证停机取消发生在 Send
// 期间时，占用被回滚、行保持 queued（不写 failed，不消耗 attempt）。
func TestEmailWorkerShutdownReleasesInFlightDelivery(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	id := insertEmailDelivery(t, db, emailDeliverySeed{
		dedupe: "email:drain:1", category: "billing.payment_failed",
		recipient: "payer@example.com", locale: "en", payload: paymentFailedPayload(t),
	})
	started := make(chan struct{}, 1)
	sender := &fakeEmailWorkerSender{
		configured:       true,
		started:          started,
		blockUntilCancel: true,
	}
	store, _ := NewStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunEmailWorkerLoop(ctx, EmailWorkerDeps{
			Store: store, Sender: sender,
			Now:      func() time.Time { return now },
			Interval: time.Hour,
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Send 未开始")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("停机 = %v，期望 nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 worker 未退出")
	}
	status, attempt, _, _, _ := readEmailDelivery(t, db, id)
	if status != "queued" || attempt != 0 {
		t.Fatalf("停机后行 = %s/%d，期望 queued/0", status, attempt)
	}
}

func TestChannelAutoDisabledEnqueuesAdminEmail(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()

	store, _ := NewStore(db)
	producer, err := NewEmailEventProducer(store, emailTestSiteConfig(), nil)
	if err != nil {
		t.Fatalf("NewEmailEventProducer: %v", err)
	}
	service, err := NewService(ServiceConfig{Store: store, EmailProducer: producer})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	adminID := insertTestUser(t, db, "admin@example.com", "zh-CN", `{}`, "admin")

	if _, err := service.DeliverChannelAutoDisabledInApp(ctx, ChannelAutoDisabledInput{
		ChannelID: 17, ChannelName: "openai-main", ReasonCode: "channel_auth_error",
		DisabledAt:  time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
		ChannelsURL: "/admin/channels", DedupeKey: "channel:17:disabled:20261022",
	}); err != nil {
		t.Fatalf("DeliverChannelAutoDisabledInApp: %v", err)
	}
	var (
		status, category, recipient string
		userID                      int64
	)
	if err := db.QueryRow(`SELECT status, category, recipient, recipient_user_id
		FROM notification_deliveries
		WHERE channel = 'email' AND recipient_user_id = $1`, adminID).
		Scan(&status, &category, &recipient, &userID); err != nil {
		t.Fatalf("read admin email delivery: %v", err)
	}
	if category != "ops.channel_auto_disabled" || status != "queued" ||
		recipient != "admin@example.com" || userID != adminID {
		t.Fatalf("admin email 行 = %s/%s/%s/%d", status, category, recipient, userID)
	}
}

// fakeWebhookSubscription 是不订阅任何事件的替换面，让 N6 扫描在没有
// webhook 出口时仍因 email 生产者存在而运行。
type fakeWebhookSubscription struct{}

func (fakeWebhookSubscription) Subscribed(string) bool { return false }

// TestBalanceLowWorkerEnqueuesEmailForOptedInUser 验证 N6 扫描把 email 行
// 写进队列（webhook 未订阅也照常发邮件），并按站点日期去重。
func TestBalanceLowWorkerEnqueuesEmailForOptedInUser(t *testing.T) {
	db := openNotifyTestDB(t)
	resetNotifyTables(t, db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	store, _ := NewStore(db)
	emailProducer, err := NewEmailEventProducer(store, emailTestSiteConfig(), nil)
	if err != nil {
		t.Fatalf("NewEmailEventProducer: %v", err)
	}
	webhookProducer, err := NewWebhookEventProducer(store, fakeWebhookSubscription{}, nil)
	if err != nil {
		t.Fatalf("NewWebhookEventProducer: %v", err)
	}
	cfg := emailTestConfig{
		"notify.webhook.enabled":               json.RawMessage(`false`),
		"notify.webhook.balance_low_threshold": json.RawMessage(`100`),
	}
	worker, err := NewBalanceLowWorker(BalanceLowWorkerDeps{
		Store:         store,
		Producer:      webhookProducer,
		EmailProducer: emailProducer,
		Config:        cfg,
		Subscription:  fakeWebhookSubscription{},
		Now:           func() time.Time { return now },
		Location:      time.UTC,
	})
	if err != nil {
		t.Fatalf("NewBalanceLowWorker: %v", err)
	}
	userID := insertTestUser(t, db, "low@example.com", "en",
		`{"balance_warning":{"enabled":true,"channels":["email"]}}`, "user")
	if _, err := db.Exec(`UPDATE users SET balance = 50 WHERE id = $1`, userID); err != nil {
		t.Fatalf("set balance: %v", err)
	}

	result := worker.RunOnce(ctx)
	if result.EmailEnqueued != 1 || result.Enqueued != 0 {
		t.Fatalf("首轮 result = %+v（webhook 未订阅，只应有 email）", result)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE channel = 'email' AND category = 'billing.balance_low' AND recipient_user_id = $1`,
		userID).Scan(&count); err != nil {
		t.Fatalf("count email rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("email 行数 = %d，期望 1", count)
	}
	// 同一站点日重复扫描由 dedupe_key 收敛。
	worker.RunOnce(ctx)
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE channel = 'email' AND category = 'billing.balance_low' AND recipient_user_id = $1`,
		userID).Scan(&count); err != nil {
		t.Fatalf("count email rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("重复扫描后 email 行数 = %d，期望 1", count)
	}
}

// 编译期钉死 fake 与接口一致。
var _ EmailWorkerSender = (*fakeEmailWorkerSender)(nil)
var _ ConfigReader = emailTestConfig(nil)
