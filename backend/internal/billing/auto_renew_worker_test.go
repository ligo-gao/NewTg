package billing

// S4 自动续费验收（P-14 / P-16 / P-17 / R-40 / R-41 / R-42）。
//
// 两组用例：
//   - 纯单测（fake store/charger/miniredis）：窗口判定、失败阈值、取消周期末
//     续费跳过、令牌失效 fail-closed、续费幂等键；
//   - DB 用例（NEWTG_TEST_DB_DSN 未设置时 Skip）：真正走
//     createRenewalOrder → Stripe off-session → settleSubscriptionRenewal →
//     store.RenewSubscription，断言 end_at 顺延与 renewed 事件（R-41）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

// testTokenSealer 是可识别的 AES-GCM 替身：真实实现由 credential SecretSealer
// 提供，单测只需要验证「落库值与明文不同、Open 能还原」。
type testTokenSealer struct{}

func (testTokenSealer) Seal(plaintext string) (string, error) {
	return "sealed:" + plaintext, nil
}

func (testTokenSealer) Open(sealed string) (string, error) {
	if !strings.HasPrefix(sealed, "sealed:") {
		return "", errors.New("bad sealed token")
	}
	return strings.TrimPrefix(sealed, "sealed:"), nil
}

type failingTokenSealer struct{ testTokenSealer }

func (failingTokenSealer) Open(string) (string, error) {
	return "", errors.New("cannot open")
}

// fakeAutoRenewStore 模拟 store 的自动续费写入口（阈值/事件语义与生产一致）。
type fakeAutoRenewStore struct {
	due          []store.Subscription
	current      map[int64]store.Subscription
	failures     map[int64]int32
	disabled     map[int64]bool
	failureEvent []map[string]any
	disableEvent []map[string]any
	attempted    map[int64]time.Time
}

func newFakeAutoRenewStore(subs ...store.Subscription) *fakeAutoRenewStore {
	f := &fakeAutoRenewStore{
		due:       subs,
		current:   map[int64]store.Subscription{},
		failures:  map[int64]int32{},
		disabled:  map[int64]bool{},
		attempted: map[int64]time.Time{},
	}
	for _, sub := range subs {
		f.current[sub.ID] = sub
		f.failures[sub.ID] = sub.AutoRenewFailures
	}
	return f
}

func (f *fakeAutoRenewStore) ListDueForAutoRenew(context.Context, time.Time,
	time.Duration, time.Duration, int) ([]store.Subscription, error) {
	return f.due, nil
}

func (f *fakeAutoRenewStore) GetSubscription(_ context.Context, id int64) (store.Subscription, error) {
	sub, ok := f.current[id]
	if !ok {
		return store.Subscription{}, store.ErrSubscriptionNotFound
	}
	return sub, nil
}

func (f *fakeAutoRenewStore) MarkAutoRenewAttempt(_ context.Context, id int64, now time.Time) (bool, error) {
	f.attempted[id] = now
	return true, nil
}

func (f *fakeAutoRenewStore) RecordAutoRenewFailure(_ context.Context, id int64, now time.Time,
	threshold int, detail map[string]any) (store.AutoRenewFailureState, error) {

	f.failures[id]++
	state := store.AutoRenewFailureState{Failures: f.failures[id]}
	if state.Failures >= int32(threshold) {
		state.Disabled = true
		f.disabled[id] = true
		sub := f.current[id]
		sub.AutoRenew = false
		f.current[id] = sub
	}
	payload := map[string]any{"failures": state.Failures, "auto_renew_disabled": state.Disabled}
	for k, v := range detail {
		payload[k] = v
	}
	f.failureEvent = append(f.failureEvent, payload)
	f.attempted[id] = now
	return state, nil
}

func (f *fakeAutoRenewStore) RecordAutoRenewSuccess(_ context.Context, id int64, now time.Time) error {
	f.failures[id] = 0
	f.attempted[id] = now
	return nil
}

func (f *fakeAutoRenewStore) DisableAutoRenew(_ context.Context, id int64, detail map[string]any) error {
	f.disabled[id] = true
	f.disableEvent = append(f.disableEvent, detail)
	sub := f.current[id]
	sub.AutoRenew = false
	f.current[id] = sub
	return nil
}

type fakeAutoRenewMethods struct {
	method        store.PaymentMethod
	err           error
	invalidMarked bool
}

func (f *fakeAutoRenewMethods) GetForUser(context.Context, int64, string) (store.PaymentMethod, error) {
	if f.err != nil {
		return store.PaymentMethod{}, f.err
	}
	return f.method, nil
}

func (f *fakeAutoRenewMethods) MarkInvalid(context.Context, int64, string) error {
	f.invalidMarked = true
	return nil
}

type fakeCharger struct {
	calls []AutoRenewChargeInput
	err   error
}

func (f *fakeCharger) ChargeAutoRenewal(_ context.Context,
	in AutoRenewChargeInput) (AutoRenewChargeResult, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return AutoRenewChargeResult{}, f.err
	}
	return AutoRenewChargeResult{OrderID: 1, Amount: 5000, Currency: "CNY"}, nil
}

type fakeNotifier struct{ notices []RenewalFailureNotice }

func (f *fakeNotifier) NotifyRenewalFailure(_ context.Context, n RenewalFailureNotice) error {
	f.notices = append(f.notices, n)
	return nil
}

func newAutoRenewTestWorker(t *testing.T, subs AutoRenewSubscriptionStore,
	methods AutoRenewPaymentMethods, charger AutoRenewCharger,
	sealer PaymentTokenSealer, now time.Time) *AutoRenewWorker {

	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return newAutoRenewTestWorkerOnRedis(t, rdb, subs, methods, charger, sealer, now)
}

// newAutoRenewTestWorkerOnRedis 用共享的 Redis 构造 worker（双副本锁竞争用例）。
func newAutoRenewTestWorkerOnRedis(t *testing.T, rdb *redis.Client,
	subs AutoRenewSubscriptionStore, methods AutoRenewPaymentMethods,
	charger AutoRenewCharger, sealer PaymentTokenSealer, now time.Time) *AutoRenewWorker {

	t.Helper()
	worker, err := NewAutoRenewWorker(AutoRenewDeps{
		Subs: subs, Methods: methods, Charger: charger, Sealer: sealer,
		Redis: rdb, Notifier: &fakeNotifier{}, Now: func() time.Time { return now },
		LeadHours: 24, RetryInterval: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("NewAutoRenewWorker: %v", err)
	}
	return worker
}

func autoRenewTestSubscription(id int64, now time.Time, endAt time.Time) store.Subscription {
	methodID := "11111111-1111-1111-1111-111111111111"
	return store.Subscription{
		ID: id, UserID: 7, PlanID: 3, PlanTitleSnapshot: "Basic",
		Status: "active", StartAt: now.Add(-29 * 24 * time.Hour), EndAt: endAt,
		AutoRenew: true, AutoRenewPaymentMethodID: &methodID,
		AmountTotal: 1000, Currency: "CNY", PricePaidAmount: 5000,
		PeriodDays: 30, GraceDays: 0,
	}
}

func autoRenewTestMethod(methodID string) store.PaymentMethod {
	return store.PaymentMethod{
		ID: methodID, UserID: 7, Provider: "stripe",
		TokenCipher: "sealed:pm_test_123", Brand: "visa", Last4: "4242",
		ExpMonth: 12, ExpYear: 2030, Status: "active",
	}
}

// Step 1（单测部分）：窗口内实例触发扣费，幂等键冻结为
// auto-renew:<id>:<end_at unix>:<attempt>。
func TestAutoRenewChargesInsideWindow(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(11, now, now.Add(2*time.Hour))
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)}
	charger := &fakeCharger{}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, testTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Charged != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v, want charged=1 failed=0", res)
	}
	if len(charger.calls) != 1 {
		t.Fatalf("charge calls = %d, want 1", len(charger.calls))
	}
	if !charger.calls[0].CycleEndAt.Equal(sub.EndAt) {
		t.Fatalf("cycle anchor = %s, want %s", charger.calls[0].CycleEndAt, sub.EndAt)
	}
	if charger.calls[0].PaymentMethodID != "pm_test_123" {
		t.Fatalf("payment method = %q, want decrypted token", charger.calls[0].PaymentMethodID)
	}
	if subs.failures[sub.ID] != 0 {
		t.Fatalf("failures = %d, want 0 after success", subs.failures[sub.ID])
	}
}

// Step 2：连续 3 次失败后 auto_renew=false，且每次失败都有 renewal_failed 事件。
func TestAutoRenewFailureThresholdDisables(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(12, now, now.Add(time.Hour))
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)}
	charger := &fakeCharger{err: &AutoRenewChargeFailure{Code: "card_declined"}}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, testTokenSealer{}, now)

	for i := 0; i < 3; i++ {
		if _, err := worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce #%d: %v", i+1, err)
		}
	}
	if subs.failures[sub.ID] != 3 {
		t.Fatalf("failures = %d, want 3", subs.failures[sub.ID])
	}
	if !subs.disabled[sub.ID] {
		t.Fatal("auto_renew should be disabled after 3 failures")
	}
	if len(subs.failureEvent) != 3 {
		t.Fatalf("renewal_failed events = %d, want 3", len(subs.failureEvent))
	}
	last := subs.failureEvent[2]
	if last["reason"] != "card_declined" || last["auto_renew_disabled"] != true {
		t.Fatalf("third event detail = %v, want reason+disabled", last)
	}
}

// Step 3：cancel_at_period_end=true 不续费（R-40 用户明确不续费优先）。
func TestAutoRenewSkipsCancelAtPeriodEnd(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(13, now, now.Add(time.Hour))
	sub.CancelAtPeriodEnd = true
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)}
	charger := &fakeCharger{}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, testTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Skipped != 1 || res.Charged != 0 {
		t.Fatalf("result = %+v, want skipped=1 charged=0", res)
	}
	if len(charger.calls) != 0 {
		t.Fatalf("charge calls = %d, want 0", len(charger.calls))
	}
}

// R-42：令牌不存在 → 不扣费、自动续费 fail-closed 关闭并写事件。
func TestAutoRenewMissingPaymentMethodFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(14, now, now.Add(time.Hour))
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{err: store.ErrPaymentMethodNotFound}
	charger := &fakeCharger{}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, testTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Disabled != 1 || res.Charged != 0 {
		t.Fatalf("result = %+v, want disabled=1 charged=0", res)
	}
	if len(charger.calls) != 0 {
		t.Fatal("must not charge when the payment method is missing")
	}
	if len(subs.disableEvent) != 1 ||
		subs.disableEvent[0]["reason"] != "payment_method_missing" {
		t.Fatalf("disable events = %v, want payment_method_missing", subs.disableEvent)
	}
}

// R-42：密文无法解封 → 标记失效并 fail-closed，绝不把密文当令牌扣款。
func TestAutoRenewUnreadableTokenFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(15, now, now.Add(time.Hour))
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)}
	charger := &fakeCharger{}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, failingTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Disabled != 1 || !methods.invalidMarked {
		t.Fatalf("result = %+v invalidMarked=%v, want disabled + marked invalid", res, methods.invalidMarked)
	}
	if len(charger.calls) != 0 {
		t.Fatal("must not charge with an unreadable token")
	}
}

// ---------------------------------------------------------------------------
// DB 端到端：R-41（复用 RenewSubscription）与 end_at/renewed 事件
// ---------------------------------------------------------------------------

func insertAutoRenewSubscription(t *testing.T, db *sql.DB, userID, planID int64,
	methodID string, endAt time.Time) int64 {
	t.Helper()
	var id int64
	err := db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 amount_total, amount_used, allow_wallet_overflow, auto_renew,
			 auto_renew_payment_method_id, currency, price_paid_amount,
			 period_days, reset_on_renew, grace_days)
		VALUES ($1, $2, 'Auto Renew Plan', 'active', $3, $4, 1000, 0, true, true,
		        $5, 'CNY', 5000, 30, true, 0)
		RETURNING id`, userID, planID, endAt.Add(-29*24*time.Hour), endAt, methodID).Scan(&id)
	if err != nil {
		t.Fatalf("insert auto-renew subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM orders WHERE biz_kind = 'renew' AND biz_ref_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM subscription_events WHERE subscription_id = $1`, id)
		_, _ = db.Exec(`DELETE FROM subscriptions WHERE id = $1`, id)
	})
	return id
}

// TestAutoRenewEndToEndExtendsSubscription 覆盖 Step 1 的完整资金链路：
// 窗口内 → 幂等下单 → Stripe off-session succeeded → 订单 succeeded +
// end_at 顺延 30 天 + renewed 事件，且 amount_used 归零 / failures 清零。
func TestAutoRenewEndToEndExtendsSubscription(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	userID := createBillingTestUser(t, db, 0)
	planID := insertSubscriptionPlan(t, db, 5000, 1000, 30, true, "enabled", "")

	methodRepo := store.NewPaymentMethodRepo(db)
	method, err := methodRepo.Create(context.Background(), store.PaymentMethodInput{
		UserID: userID, Provider: "stripe", TokenCipher: "sealed:pm_e2e_123",
		Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030,
	})
	if err != nil {
		t.Fatalf("create payment method: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM payment_methods WHERE id = $1`, method.ID) })

	oldEnd := now.Add(2 * time.Hour)
	subID := insertAutoRenewSubscription(t, db, userID, planID, method.ID, oldEnd)

	var sawIdempotencyKey string
	var sawPaymentMethod string
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payment_intents" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		sawIdempotencyKey = r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		sawPaymentMethod = form.Get("payment_method")
		if form.Get("confirm") != "true" || form.Get("off_session") != "true" {
			t.Errorf("charge must be off-session+confirm, form=%v", form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"pi_auto_1","status":"succeeded","amount":5000,"currency":"cny"}`)
	}))
	defer stripe.Close()

	cfg := mapConfig{
		ConfigOnlineEnabled:       json.RawMessage("true"),
		ConfigStripeEnabled:       json.RawMessage("true"),
		ConfigStripeSecretKey:     json.RawMessage(`"sk_test_auto"`),
		ConfigStripeWebhookSecret: json.RawMessage(`"whsec_test_auto"`),
	}
	topup, err := NewTopupService(TopupServiceConfig{
		DB: gormDB, Config: cfg, Logger: autoRenewTestLogger(), StripeBaseURL: stripe.URL,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	worker := newAutoRenewTestWorker(t,
		store.NewSubscriptionRepo(db), methodRepo, topup, testTokenSealer{}, now)
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Charged != 1 {
		t.Fatalf("result = %+v, want charged=1", res)
	}
	if sawPaymentMethod != "pm_e2e_123" {
		t.Fatalf("payment_method = %q, want decrypted pm id", sawPaymentMethod)
	}
	if !strings.HasPrefix(sawIdempotencyKey, fmt.Sprintf("auto-renew:%d:%d:", subID, oldEnd.Unix())) {
		t.Fatalf("idempotency key = %q, want auto-renew:<sub>:<end> prefix", sawIdempotencyKey)
	}

	var (
		newEnd        time.Time
		amountUsed    int64
		failures      int32
		autoRenew     bool
		orderStatus   string
		orderAmount   int64
		renewedEvents int
	)
	if err := db.QueryRow(`SELECT end_at, amount_used, auto_renew_failures, auto_renew
		FROM subscriptions WHERE id = $1`, subID).
		Scan(&newEnd, &amountUsed, &failures, &autoRenew); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if got := newEnd.Sub(oldEnd); got < 30*24*time.Hour-time.Minute || got > 30*24*time.Hour+time.Minute {
		t.Fatalf("end_at extended by %v, want ~30d", got)
	}
	if amountUsed != 0 || failures != 0 || !autoRenew {
		t.Fatalf("used=%d failures=%d autoRenew=%v, want 0/0/true", amountUsed, failures, autoRenew)
	}
	if err := db.QueryRow(`SELECT status, amount FROM orders
		WHERE biz_kind = 'renew' AND biz_ref_id = $1`, subID).
		Scan(&orderStatus, &orderAmount); err != nil {
		t.Fatalf("read renewal order: %v", err)
	}
	if orderStatus != "succeeded" || orderAmount != 5000 {
		t.Fatalf("order status=%s amount=%d, want succeeded/5000", orderStatus, orderAmount)
	}
	if err := db.QueryRow(`SELECT count(*) FROM subscription_events
		WHERE subscription_id = $1 AND kind = 'renewed'`, subID).Scan(&renewedEvents); err != nil {
		t.Fatalf("count renewed events: %v", err)
	}
	if renewedEvents != 1 {
		t.Fatalf("renewed events = %d, want 1", renewedEvents)
	}
}

// TestPaymentMethodDeleteDisablesAutoRenew 覆盖 R-42 的删除路径：删除令牌与
// 关闭关联订阅的 auto_renew + renewal_failed 事件在同一事务内完成。
func TestPaymentMethodDeleteDisablesAutoRenew(t *testing.T) {
	_, db := openBillingTestDB(t)
	now := time.Now().UTC()
	userID := createBillingTestUser(t, db, 0)
	planID := insertSubscriptionPlan(t, db, 5000, 1000, 30, true, "enabled", "")
	repo := store.NewPaymentMethodRepo(db)
	method, err := repo.Create(context.Background(), store.PaymentMethodInput{
		UserID: userID, Provider: "stripe", TokenCipher: "sealed:pm_del_1_padding",
		Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030,
	})
	if err != nil {
		t.Fatalf("create payment method: %v", err)
	}
	subID := insertAutoRenewSubscription(t, db, userID, planID, method.ID, now.Add(time.Hour))

	disabled, err := repo.Delete(context.Background(), userID, method.ID)
	if err != nil {
		t.Fatalf("delete payment method: %v", err)
	}
	if len(disabled) != 1 || disabled[0] != subID {
		t.Fatalf("disabled subscriptions = %v, want [%d]", disabled, subID)
	}
	var autoRenew bool
	if err := db.QueryRow(`SELECT auto_renew FROM subscriptions WHERE id = $1`, subID).
		Scan(&autoRenew); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if autoRenew {
		t.Fatal("auto_renew must be false after the token was deleted (fail-closed)")
	}
	var events int
	if err := db.QueryRow(`SELECT count(*) FROM subscription_events
		WHERE subscription_id = $1 AND kind = 'renewal_failed'
		  AND detail->>'reason' = 'payment_method_deleted'`, subID).Scan(&events); err != nil {
		t.Fatalf("count renewal_failed events: %v", err)
	}
	if events != 1 {
		t.Fatalf("renewal_failed events = %d, want 1", events)
	}
}

// autoRenewTestLogger 丢弃日志：单测只关心状态。
func autoRenewTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// R-44：retryable 失败（网络/5xx/结算）不计入用户连续失败，也不发通知；
// 订单保持 pending，下一轮用同一幂等键继续确认。
func TestAutoRenewRetryableFailureDoesNotCount(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(21, now, now.Add(time.Hour))
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)}
	charger := &fakeCharger{err: &AutoRenewChargeFailure{
		Code: "provider_unavailable", Retryable: true, OrderID: 77,
	}}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, testTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Retried != 1 || res.Failed != 0 || res.Disabled != 0 {
		t.Fatalf("result = %+v, want retried=1 failed=0 disabled=0", res)
	}
	if subs.failures[sub.ID] != 0 {
		t.Fatalf("failures = %d, want 0 for a retryable failure", subs.failures[sub.ID])
	}
	if len(subs.failureEvent) != 0 {
		t.Fatalf("retryable failure must not write renewal_failed events: %v", subs.failureEvent)
	}
	if subs.disabled[sub.ID] {
		t.Fatal("retryable failures must not disable auto-renew")
	}
}

// R-42：渠道明确判定令牌不可用 → MarkInvalid + fail-closed，不计 3 次阈值。
func TestAutoRenewInvalidTokenFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	sub := autoRenewTestSubscription(22, now, now.Add(time.Hour))
	subs := newFakeAutoRenewStore(sub)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)}
	charger := &fakeCharger{err: &AutoRenewChargeFailure{
		Code: "expired_card", InvalidToken: true, OrderID: 78,
	}}
	worker := newAutoRenewTestWorker(t, subs, methods, charger, testTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Disabled != 1 || !methods.invalidMarked {
		t.Fatalf("result = %+v invalidMarked=%v, want disabled + marked invalid",
			res, methods.invalidMarked)
	}
	if len(subs.disableEvent) != 1 ||
		subs.disableEvent[0]["reason"] != "payment_method_invalid" {
		t.Fatalf("disable events = %v", subs.disableEvent)
	}
}

// blockingCharger 让第一个副本停在渠道调用内，验证第二个副本拿不到锁。
type blockingCharger struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingCharger) ChargeAutoRenewal(context.Context,
	AutoRenewChargeInput) (AutoRenewChargeResult, error) {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return AutoRenewChargeResult{OrderID: 1, Amount: 5000, Currency: "CNY"}, nil
}

// TestAutoRenewLockContentionBetweenReplicas 覆盖 P-17 的双副本互斥：
// 第一个副本持有 subscriptions:renew:<id> 时，第二个副本必须跳过而不是扣款。
// 变异把 acquireLock 改成恒 true 时，本用例会红（第二个副本会调用 charger）。
func TestAutoRenewLockContentionBetweenReplicas(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	sub := autoRenewTestSubscription(23, now, now.Add(time.Hour))
	method := autoRenewTestMethod(*sub.AutoRenewPaymentMethodID)

	blocking := &blockingCharger{entered: make(chan struct{}), release: make(chan struct{})}
	w1 := newAutoRenewTestWorkerOnRedis(t, rdb,
		newFakeAutoRenewStore(sub), &fakeAutoRenewMethods{method: method},
		blocking, testTokenSealer{}, now)

	secondCharger := &fakeCharger{}
	w2 := newAutoRenewTestWorkerOnRedis(t, rdb,
		newFakeAutoRenewStore(sub), &fakeAutoRenewMethods{method: method},
		secondCharger, testTokenSealer{}, now)

	done := make(chan struct{})
	go func() {
		_, _ = w1.RunOnce(context.Background())
		close(done)
	}()
	<-blocking.entered

	res2, err := w2.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second replica RunOnce: %v", err)
	}
	if res2.Charged != 0 || res2.Skipped != 1 {
		t.Fatalf("second replica result = %+v, want charged=0 skipped=1", res2)
	}
	if len(secondCharger.calls) != 0 {
		t.Fatalf("second replica charged %d time(s) while the lock was held",
			len(secondCharger.calls))
	}

	close(blocking.release)
	<-done
}

// TestAutoRenewLeadHoursConfigBoundaries 覆盖 R-40 的 1–168 配置边界：
// 0/169/非数字回退默认 24，边界值原样生效。
func TestAutoRenewLeadHoursConfigBoundaries(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`1`, 1},
		{`24`, 24},
		{`168`, 168},
		{`0`, DefaultAutoRenewLeadHours},
		{`169`, DefaultAutoRenewLeadHours},
		{`"abc"`, DefaultAutoRenewLeadHours},
		{`-5`, DefaultAutoRenewLeadHours},
	}
	for _, tc := range cases {
		w := &AutoRenewWorker{
			cfg: mapConfig{ConfigAutoRenewLeadHours: json.RawMessage(tc.raw)},
			log: autoRenewTestLogger(),
		}
		if got := int(w.leadDuration() / time.Hour); got != tc.want {
			t.Fatalf("lead for %s = %d, want %d", tc.raw, got, tc.want)
		}
	}
	// 缺键：默认 24。
	nilCfg := &AutoRenewWorker{log: autoRenewTestLogger()}
	if got := int(nilCfg.leadDuration() / time.Hour); got != DefaultAutoRenewLeadHours {
		t.Fatalf("default lead = %d, want %d", got, DefaultAutoRenewLeadHours)
	}
}

// TestAutoRenewRejectsUnsafeBatchBudget 覆盖 R-45 的装配期约束：
// batch limit × 单笔扣款上限必须小于单轮 deadline。
func TestAutoRenewRejectsUnsafeBatchBudget(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	_, err := NewAutoRenewWorker(AutoRenewDeps{
		Subs: newFakeAutoRenewStore(), Methods: &fakeAutoRenewMethods{},
		Charger: &fakeCharger{}, Sealer: testTokenSealer{}, Redis: rdb,
		PassTimeout: 10 * time.Second, BatchLimit: 5,
	})
	if err == nil {
		t.Fatal("unsafe batch budget (5 x 5s >= 10s) must be rejected at construction")
	}
}

// R-44：retryable 失败（Stripe 5xx）后重试必须复用同一 Idempotency-Key
// 与同一订单，绝不能换键二次扣款。
func TestAutoRenewRetryableReusesOrderAndKey(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	userID := createBillingTestUser(t, db, 0)
	planID := insertSubscriptionPlan(t, db, 5000, 1000, 30, true, "enabled", "")
	methodRepo := store.NewPaymentMethodRepo(db)
	method, err := methodRepo.Create(context.Background(), store.PaymentMethodInput{
		UserID: userID, Provider: "stripe", TokenCipher: "sealed:pm_retry_1_padding",
		Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030,
	})
	if err != nil {
		t.Fatalf("create payment method: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM payment_methods WHERE id = $1`, method.ID) })
	subID := insertAutoRenewSubscription(t, db, userID, planID, method.ID, now.Add(2*time.Hour))

	var (
		mu    sync.Mutex
		calls int
		keys  []string
	)
	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		mu.Lock()
		calls++
		call := calls
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if form.Get("confirm") != "true" {
			http.Error(w, "unexpected unconfirmed intent", http.StatusBadRequest)
			return
		}
		if call == 1 {
			// 第一次：模拟渠道 500（用户的钱可能已扣、也可能没扣；网关必须
			// 保持 pending 并用同一幂等键重试，由 Stripe 判定最终状态）。
			http.Error(w, "upstream error", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w,
			`{"id":"pi_retry_1","status":"succeeded","amount":5000,"currency":"cny"}`)
	}))
	defer stripe.Close()

	cfg := mapConfig{
		ConfigOnlineEnabled:       json.RawMessage("true"),
		ConfigStripeEnabled:       json.RawMessage("true"),
		ConfigStripeSecretKey:     json.RawMessage(`"sk_test_retry"`),
		ConfigStripeWebhookSecret: json.RawMessage(`"whsec_test_retry"`),
	}
	topup, err := NewTopupService(TopupServiceConfig{
		DB: gormDB, Config: cfg, Logger: autoRenewTestLogger(), StripeBaseURL: stripe.URL,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	worker := newAutoRenewTestWorker(t, store.NewSubscriptionRepo(db), methodRepo,
		topup, testTokenSealer{}, now)

	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if res.Retried != 1 || res.Failed != 0 {
		t.Fatalf("first result = %+v, want retried=1 failed=0", res)
	}
	var orderCount int
	var status string
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE biz_kind='renew' AND biz_ref_id=$1`,
		subID).Scan(&orderCount); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("orders = %d after a retryable failure, want 1 pending order", orderCount)
	}
	if err := db.QueryRow(`SELECT status FROM orders WHERE biz_kind='renew' AND biz_ref_id=$1`,
		subID).Scan(&status); err != nil {
		t.Fatalf("read order status: %v", err)
	}
	if status != "pending" {
		t.Fatalf("order status = %s, want pending after a retryable failure", status)
	}

	// 跳过 6h 退避后重试。
	if _, err := db.Exec(`UPDATE subscriptions SET auto_renew_attempted_at = $2 WHERE id = $1`,
		subID, now.Add(-7*time.Hour)); err != nil {
		t.Fatalf("reset attempt time: %v", err)
	}
	res, err = worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if res.Charged != 1 {
		t.Fatalf("second result = %+v, want charged=1", res)
	}
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE biz_kind='renew' AND biz_ref_id=$1`,
		subID).Scan(&orderCount); err != nil {
		t.Fatalf("count orders after retry: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("orders = %d after retry, want the same single order", orderCount)
	}
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] == "" {
		t.Fatalf("idempotency keys = %v, want the same non-empty key on both attempts", keys)
	}
}

// R-43：手动续费订单在途时，自动续费必须跳过（不建第二笔订单、不扣款）；
// 手动结算成功后同一周期再加一笔（模拟旧订单晚到的回调）只能转
// refund_required，end_at 只加一个周期。
func TestManualAndAutoRenewSameCycleOnlyOneCharge(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	userID := createBillingTestUser(t, db, 0)
	planID := insertSubscriptionPlan(t, db, 5000, 1000, 30, true, "enabled", "")
	methodRepo := store.NewPaymentMethodRepo(db)
	method, err := methodRepo.Create(context.Background(), store.PaymentMethodInput{
		UserID: userID, Provider: "stripe", TokenCipher: "sealed:pm_manual_1_padding",
		Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030,
	})
	if err != nil {
		t.Fatalf("create payment method: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM payment_methods WHERE id = $1`, method.ID) })
	oldEnd := now.Add(2 * time.Hour)
	subID := insertAutoRenewSubscription(t, db, userID, planID, method.ID, oldEnd)

	stripe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		if form.Get("confirm") == "true" {
			// 自动续费路径不应该走到这里（手动订单在途时应被跳过）。
			_, _ = io.WriteString(w,
				`{"id":"pi_auto_unexpected","status":"succeeded","amount":5000,"currency":"cny"}`)
			return
		}
		// 手动下单：创建未确认的 PaymentIntent。
		_, _ = io.WriteString(w, `{"id":"pi_manual_1","client_secret":"cs_manual_1"}`)
	}))
	defer stripe.Close()

	cfg := mapConfig{
		ConfigOnlineEnabled:       json.RawMessage("true"),
		ConfigStripeEnabled:       json.RawMessage("true"),
		ConfigStripeSecretKey:     json.RawMessage(`"sk_test_manual"`),
		ConfigStripeWebhookSecret: json.RawMessage(`"whsec_test_123"`),
	}
	topup, err := NewTopupService(TopupServiceConfig{
		DB: gormDB, Config: cfg, Logger: autoRenewTestLogger(), StripeBaseURL: stripe.URL,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	manual, err := topup.CreateSubscriptionRenewalOrder(context.Background(),
		userID, subID, "stripe", "manual-cycle-1")
	if err != nil {
		t.Fatalf("manual renewal order: %v", err)
	}
	if manual.ClientSecret == "" {
		t.Fatal("manual order must carry the client secret")
	}

	worker := newAutoRenewTestWorker(t, store.NewSubscriptionRepo(db), methodRepo,
		topup, testTokenSealer{}, now)
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("auto RunOnce: %v", err)
	}
	if res.Charged != 0 || res.Skipped != 1 {
		t.Fatalf("auto result = %+v, want charged=0 skipped=1 (manual order in flight)", res)
	}
	var orderCount int
	if err := db.QueryRow(`SELECT count(*) FROM orders WHERE biz_kind='renew' AND biz_ref_id=$1`,
		subID).Scan(&orderCount); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orderCount != 1 {
		t.Fatalf("orders = %d, want only the manual order", orderCount)
	}

	// 手动支付成功：end_at 只加一个周期。
	if err := renewViaStripe(t, topup, db, manual.Order.ID, 5000); err != nil {
		t.Fatalf("settle manual renewal: %v", err)
	}
	var afterManual time.Time
	if err := db.QueryRow(`SELECT end_at FROM subscriptions WHERE id=$1`, subID).
		Scan(&afterManual); err != nil {
		t.Fatalf("read end_at: %v", err)
	}
	if got := afterManual.Sub(oldEnd); got < 30*24*time.Hour-time.Minute ||
		got > 30*24*time.Hour+time.Minute {
		t.Fatalf("end_at advanced by %v, want ~30d", got)
	}

	// 模拟同周期的旧订单晚到回调：用旧快照（cycle_end_at=oldEnd）插一笔
	// pending 订单并结算，必须转 refund_required，end_at 保持不变。
	var metadata []byte
	if err := db.QueryRow(`SELECT channel_metadata FROM orders WHERE id=$1`,
		manual.Order.ID).Scan(&metadata); err != nil {
		t.Fatalf("read manual metadata: %v", err)
	}
	var staleID int64
	if err := db.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status,
			idempotency_key, biz_kind, biz_ref_id, channel_metadata)
		VALUES ($1, 'stripe', 5000, 'CNY', 'pending', 'stale-cycle-1', 'renew', $2, $3)
		RETURNING id`, userID, subID, metadata).Scan(&staleID); err != nil {
		t.Fatalf("insert stale order: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM orders WHERE id = $1`, staleID) })
	stale := &store.Order{
		ID: staleID, UserID: userID, Provider: "stripe", Amount: 5000,
		Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindRenew,
		BizRefID: &subID, ChannelMetadata: metadata,
	}
	if err := topup.settleSubscriptionRenewal(context.Background(), stale,
		5000, "CNY", []byte(`{"id":"evt_stale"}`)); err != nil {
		t.Fatalf("stale renewal settlement must succeed (payment accepted): %v", err)
	}
	var staleStatus string
	if err := db.QueryRow(`SELECT status FROM orders WHERE id=$1`, staleID).
		Scan(&staleStatus); err != nil {
		t.Fatalf("read stale order: %v", err)
	}
	if staleStatus != "refund_required" {
		t.Fatalf("stale order status = %s, want refund_required", staleStatus)
	}
	var afterStale time.Time
	if err := db.QueryRow(`SELECT end_at FROM subscriptions WHERE id=$1`, subID).
		Scan(&afterStale); err != nil {
		t.Fatalf("read end_at after stale settlement: %v", err)
	}
	if !afterStale.Equal(afterManual) {
		t.Fatalf("end_at moved from %s to %s after the stale settlement, want unchanged",
			afterManual, afterStale)
	}
	var succeeded int
	if err := db.QueryRow(`SELECT count(*) FROM orders
		WHERE biz_kind='renew' AND biz_ref_id=$1 AND status='succeeded'`, subID).
		Scan(&succeeded); err != nil {
		t.Fatalf("count succeeded orders: %v", err)
	}
	if succeeded != 1 {
		t.Fatalf("succeeded renewal orders = %d, want exactly 1", succeeded)
	}
}

// slowCharger 模拟一笔耗时扣款（忽略 ctx，模拟已经在途的渠道调用）。
type slowCharger struct {
	delay time.Duration
	calls []AutoRenewChargeInput
}

func (c *slowCharger) ChargeAutoRenewal(_ context.Context,
	in AutoRenewChargeInput) (AutoRenewChargeResult, error) {
	c.calls = append(c.calls, in)
	time.Sleep(c.delay)
	return AutoRenewChargeResult{OrderID: 1, Amount: 5000, Currency: "CNY"}, nil
}

// R-45：单轮预算耗尽时必须提前结束，不能在 deadline 中途启动下一笔；
// 已完成项保留（不回滚），剩余候选留给下一轮。
func TestAutoRenewPassDeadlineDefersRemaining(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	first := autoRenewTestSubscription(31, now, now.Add(time.Hour))
	second := autoRenewTestSubscription(32, now, now.Add(time.Hour))
	subs := newFakeAutoRenewStore(first, second)
	methods := &fakeAutoRenewMethods{method: autoRenewTestMethod(*first.AutoRenewPaymentMethodID)}
	// 第一笔耗时 7s；PassTimeout=11s，扣款上限 5s → 剩余 4s 时放弃第二笔。
	charger := &slowCharger{delay: 7 * time.Second}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	worker, err := NewAutoRenewWorker(AutoRenewDeps{
		Subs: subs, Methods: methods, Charger: charger, Sealer: testTokenSealer{},
		Redis: rdb, Notifier: &fakeNotifier{}, Now: func() time.Time { return now },
		LeadHours: 24, RetryInterval: time.Nanosecond,
		PassTimeout: 11 * time.Second, BatchLimit: 2,
	})
	if err != nil {
		t.Fatalf("NewAutoRenewWorker: %v", err)
	}

	started := time.Now()
	res, err := worker.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	elapsed := time.Since(started)
	if res.Scanned != 2 || res.Charged != 1 {
		t.Fatalf("result = %+v, want scanned=2 charged=1 (second deferred)", res)
	}
	if len(charger.calls) != 1 {
		t.Fatalf("charge calls = %d, want 1 (no new charge started on the last 4s)", len(charger.calls))
	}
	if elapsed > 9*time.Second {
		t.Fatalf("pass took %v, want it to end after the first charge instead of running the second", elapsed)
	}
	// 已完成项保留；第二笔没有任何副作用（未被尝试、未记失败）。
	if subs.failures[first.ID] != 0 || subs.failures[second.ID] != 0 {
		t.Fatalf("failures = %v, want no failures", subs.failures)
	}
	if _, attempted := subs.attempted[second.ID]; attempted {
		t.Fatal("the deferred subscription must not be attempted")
	}
}

// 渠道错误分类（R-44 / R-42）：409 幂等冲突与「非 Stripe 响应」可按重试；
// 只有响应体证明 payment_method 资源不存在才判令牌失效。
func TestChargeStripeOffSessionClassifiesErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		retryable bool
		invalid   bool
	}{
		{"idempotency conflict", http.StatusConflict,
			`{"error":{"code":"idempotency_error","type":"idempotency_error"}}`, true, false},
		{"non-stripe 404 (wrong base url)", http.StatusNotFound,
			`<html>not found</html>`, true, false},
		{"unknown payment method", http.StatusNotFound,
			`{"error":{"code":"resource_missing","param":"payment_method"}}`, false, true},
		{"other stripe rejection", http.StatusBadRequest,
			`{"error":{"code":"parameter_invalid","param":"amount"}}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			svc, err := NewTopupService(TopupServiceConfig{
				DB: &gorm.DB{}, Config: mapConfig{
					ConfigStripeSecretKey: json.RawMessage(`"sk_test_classify"`),
				},
				Logger: autoRenewTestLogger(), StripeBaseURL: server.URL,
			})
			if err != nil {
				t.Fatalf("NewTopupService: %v", err)
			}
			_, err = svc.chargeStripeOffSession(context.Background(), &store.Order{
				ID: 1, UserID: 2, Amount: 5000, Currency: "CNY",
			}, "pm_test_classify", "key-1")
			var failure *AutoRenewChargeFailure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v, want AutoRenewChargeFailure", err)
			}
			if failure.Retryable != tc.retryable || failure.InvalidToken != tc.invalid {
				t.Fatalf("failure = %+v, want retryable=%v invalid=%v",
					failure, tc.retryable, tc.invalid)
			}
		})
	}
}
