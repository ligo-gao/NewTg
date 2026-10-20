package billing

// B1b：结算成功后的 payment_success Webhook 生产者验收（真实 PostgreSQL）。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_b1b?sslmode=disable' \
//	  go test ./internal/billing/ -run TestSettleOrderPaymentSuccessWebhook -v
//
// 覆盖：成功入队一次、重复回调幂等、未订阅不入队、入队失败不影响入账、
// payload 字段与最小脱敏。通知表与订单/钱包同库，测试直接查
// notification_deliveries 断言投递行。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/store"
)

// fakeWebhookSubscription 是订阅过滤的测试替身。
type fakeWebhookSubscription struct {
	events map[string]bool
}

func (f fakeWebhookSubscription) Subscribed(event string) bool { return f.events[event] }

// failingPaymentWebhook 模拟投递写入失败：结算必须仍然成功。
type failingPaymentWebhook struct{ calls int }

func (f *failingPaymentWebhook) EnqueuePaymentSuccess(context.Context, notify.PaymentSuccessEvent) error {
	f.calls++
	return errors.New("notification_deliveries unavailable")
}

func newWebhookTestService(t *testing.T, gormDB *gorm.DB, db *sql.DB,
	subscription notify.WebhookEventSubscription) *TopupService {
	t.Helper()
	store, err := notify.NewStore(db)
	if err != nil {
		t.Fatalf("notify.NewStore: %v", err)
	}
	producer, err := notify.NewWebhookEventProducer(store, subscription, nil)
	if err != nil {
		t.Fatalf("notify.NewWebhookEventProducer: %v", err)
	}
	svc, err := NewTopupService(TopupServiceConfig{
		DB:      gormDB,
		Config:  mapConfig{ConfigQuotaPerUnit: json.RawMessage("100")},
		Webhook: producer,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	return svc
}

// insertPendingTopupOrder 直接落一条 pending 充值单，绕开渠道下单样板。
func insertPendingTopupOrder(t *testing.T, db *sql.DB, userID, amount int64) int64 {
	t.Helper()
	key := fmt.Sprintf("b1b-%d-%d", time.Now().UnixNano(), topupTestSeq.Add(1))
	txn := "pi_b1b_" + key
	var id int64
	if err := db.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status, idempotency_key, provider_txn_id)
		VALUES ($1, 'stripe', $2, 'CNY', 'pending', $3, $4)
		RETURNING id`, userID, amount, key, txn).Scan(&id); err != nil {
		t.Fatalf("insert pending order: %v", err)
	}
	return id
}

func countPaymentSuccessDeliveries(t *testing.T, db *sql.DB, orderID int64) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE channel = 'webhook' AND dedupe_key = $1`,
		fmt.Sprintf("order:%d:payment_success", orderID)).Scan(&count); err != nil {
		t.Fatalf("count payment deliveries: %v", err)
	}
	return count
}

// recordingPaymentWebhook 记录事件，用于不依赖 DB 的分支断言。
type recordingPaymentWebhook struct {
	events []notify.PaymentSuccessEvent
}

func (r *recordingPaymentWebhook) EnqueuePaymentSuccess(_ context.Context, event notify.PaymentSuccessEvent) error {
	r.events = append(r.events, event)
	return nil
}

// 订阅购买/续费订单即使走到 settleOrder 的状态早返回，也不得产生充值事件。
func TestPaymentSuccessWebhookSkipsSubscriptionOrders(t *testing.T) {
	producer := &recordingPaymentWebhook{}
	svc := &TopupService{
		webhook: producer,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:     time.Now,
	}
	svc.enqueuePaymentSuccess(context.Background(), &store.Order{
		ID: 1, Status: string(OrderSucceeded), BizKind: store.OrderBizKindSubscribe,
	})
	svc.enqueuePaymentSuccess(context.Background(), &store.Order{
		ID: 2, Status: string(OrderSucceeded), BizKind: store.OrderBizKindRenew,
	})
	if len(producer.events) != 0 {
		t.Fatalf("subscription orders produced %d payment events, want 0", len(producer.events))
	}

	svc.enqueuePaymentSuccess(context.Background(), &store.Order{
		ID: 3, Status: string(OrderSucceeded), BizKind: store.OrderBizKindTopup,
		Amount: 100, CreditedCredits: 100, Provider: "stripe",
	})
	if len(producer.events) != 1 || producer.events[0].OrderID != 3 {
		t.Fatalf("topup order events = %+v, want one for order 3", producer.events)
	}
}

func TestSettleOrderPaymentSuccessWebhookEnqueuesOnce(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	orderID := insertPendingTopupOrder(t, db, userID, 1000)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key = $1`,
			fmt.Sprintf("order:%d:payment_success", orderID))
	})

	svc := newWebhookTestService(t, gormDB, db, fakeWebhookSubscription{
		events: map[string]bool{"payment_success": true},
	})
	order, err := svc.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if err := svc.settleOrder(context.Background(), order, 1000, "CNY", []byte(`{"provider":"stripe"}`)); err != nil {
		t.Fatalf("settleOrder: %v", err)
	}
	assertOrderStatus(t, db, orderID, "succeeded")
	assertWalletCount(t, db, userID, 1)

	if got := countPaymentSuccessDeliveries(t, db, orderID); got != 1 {
		t.Fatalf("payment_success deliveries = %d, want 1", got)
	}

	var (
		category, channel, dedupe, status string
		payload                           []byte
	)
	if err := db.QueryRow(`SELECT category, channel, dedupe_key, status, payload
		FROM notification_deliveries WHERE dedupe_key = $1`,
		fmt.Sprintf("order:%d:payment_success", orderID)).
		Scan(&category, &channel, &dedupe, &status, &payload); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if category != "billing.payment_success" || channel != "webhook" || status != "queued" {
		t.Fatalf("category/channel/status = %s/%s/%s", category, channel, status)
	}
	if dedupe != fmt.Sprintf("order:%d:payment_success", orderID) {
		t.Fatalf("dedupe_key = %q", dedupe)
	}

	var body struct {
		Event       string `json:"event"`
		OrderID     int64  `json:"order_id"`
		AmountCents int64  `json:"amount_cents"`
		Credits     int64  `json:"credits"`
		Provider    string `json:"provider"`
		PaidAt      string `json:"paid_at"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload JSON: %v（%s）", err, payload)
	}
	if body.Event != "payment_success" || body.OrderID != orderID ||
		body.AmountCents != 1000 || body.Credits != 1000 || body.Provider != "stripe" {
		t.Fatalf("payload = %+v", body)
	}
	if _, err := time.Parse(time.RFC3339, body.PaidAt); err != nil {
		t.Fatalf("paid_at %q is not RFC3339: %v", body.PaidAt, err)
	}
	// 最小脱敏：载荷不得出现口令/令牌类键或渠道密钥。
	lower := strings.ToLower(string(payload))
	for _, forbidden := range []string{"password", "secret", "token", "api_key", "sk_test"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("payload leaks %q: %s", forbidden, payload)
		}
	}

	// 重复回调/重放：投递行仍只有一条。
	if err := svc.settleOrder(context.Background(), order, 1000, "CNY", []byte(`{"provider":"stripe"}`)); err != nil {
		t.Fatalf("replayed settleOrder: %v", err)
	}
	if got := countPaymentSuccessDeliveries(t, db, orderID); got != 1 {
		t.Fatalf("after replay deliveries = %d, want 1", got)
	}
	assertWalletCount(t, db, userID, 1)
}

func TestSettleOrderPaymentSuccessWebhookSkipsUnsubscribed(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	orderID := insertPendingTopupOrder(t, db, userID, 500)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key = $1`,
			fmt.Sprintf("order:%d:payment_success", orderID))
	})

	svc := newWebhookTestService(t, gormDB, db, fakeWebhookSubscription{
		events: map[string]bool{"balance_low": true},
	})
	order, err := svc.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if err := svc.settleOrder(context.Background(), order, 500, "CNY", []byte(`{}`)); err != nil {
		t.Fatalf("settleOrder: %v", err)
	}
	assertOrderStatus(t, db, orderID, "succeeded")
	assertWalletCount(t, db, userID, 1)
	if got := countPaymentSuccessDeliveries(t, db, orderID); got != 0 {
		t.Fatalf("unsubscribed event wrote %d deliveries, want 0", got)
	}
}

func TestSettleOrderPaymentSuccessWebhookFailureDoesNotAffectCredit(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	orderID := insertPendingTopupOrder(t, db, userID, 700)

	producer := &failingPaymentWebhook{}
	svc, err := NewTopupService(TopupServiceConfig{
		DB:      gormDB,
		Config:  mapConfig{ConfigQuotaPerUnit: json.RawMessage("100")},
		Webhook: producer,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}
	order, err := svc.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if err := svc.settleOrder(context.Background(), order, 700, "CNY", []byte(`{}`)); err != nil {
		t.Fatalf("settleOrder must ignore webhook failure: %v", err)
	}
	if producer.calls != 1 {
		t.Fatalf("producer calls = %d, want 1", producer.calls)
	}
	assertOrderStatus(t, db, orderID, "succeeded")
	assertWalletCount(t, db, userID, 1)
}

// 极小场景：金额不一致的失败订单绝不能产生 payment_success 事件。
func TestSettleOrderMismatchDoesNotEnqueueWebhook(t *testing.T) {
	gormDB, db := openBillingTestDB(t)
	userID := createBillingTestUser(t, db, 0)
	deleteTestOrders(t, db, userID)
	orderID := insertPendingTopupOrder(t, db, userID, 1000)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key = $1`,
			fmt.Sprintf("order:%d:payment_success", orderID))
	})

	svc := newWebhookTestService(t, gormDB, db, fakeWebhookSubscription{
		events: map[string]bool{"payment_success": true},
	})
	order, err := svc.GetOrder(context.Background(), orderID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if err := svc.settleOrder(context.Background(), order, 999, "CNY", []byte(`{}`)); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("settleOrder err = %v, want ErrAmountMismatch", err)
	}
	assertOrderStatus(t, db, orderID, "failed")
	if got := countPaymentSuccessDeliveries(t, db, orderID); got != 0 {
		t.Fatalf("failed order wrote %d deliveries, want 0", got)
	}
}
