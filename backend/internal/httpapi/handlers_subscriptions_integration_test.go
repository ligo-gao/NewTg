//go:build integration

package httpapi

// D5b 的数据库级验收：真实 PostgreSQL + 真实 store.SQLSubscriptionRepo。
// 覆盖单元测试无法证明的真相：
//   - plan_key 的 UNIQUE（含 disabled 行）→ 409；
//   - 删除保护的实例引用与未终结订单引用计数（D5a 事务语义）；
//   - reset-quota 在 SQL 层同时清零 amount_used 并推进 next_reset_at；
//   - users.billing_preference 的真实读写与 CHECK 兜底；
//   - 用户侧只返回 Principal.UserID 的实例。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d5' \
//	  go test -tags=integration ./internal/httpapi/ -run TestSubscriptionIntegration -v

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

type subscriptionIntegrationEnv struct {
	db       *sql.DB
	repo     *store.SQLSubscriptionRepo
	router   http.Handler
	audit    *subAuditExecer
	purchase *billing.TopupService
}

// newSubscriptionStripeStub 是购买集成测试的 Stripe PaymentIntent 桩。
// intent id 由金额派生，测试可直接构造对应的 succeeded 事件。
func newSubscriptionStripeStub(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payment_intents" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"pi_s3_%s","client_secret":"pi_s3_%s_secret"}`,
			r.PostForm.Get("amount"), r.PostForm.Get("amount"))
	}))
	t.Cleanup(server.Close)
	return server
}

// subscriptionStripeSig / subscriptionStripeEvent 与 billing 的单测同构：
// 这里走真实 HTTP 回调路径，不直接调用 settleOrder。
func subscriptionStripeSig(secret string, timestamp int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

func subscriptionStripeEvent(intentID string, amount int64) []byte {
	return []byte(fmt.Sprintf(
		`{"id":"evt_s3","type":"payment_intent.succeeded","data":{"object":{"id":%q,"amount":%d,"amount_received":%d,"currency":"cny","status":"succeeded"}}}`,
		intentID, amount, amount))
}

func newSubscriptionIntegrationEnv(t *testing.T) *subscriptionIntegrationEnv {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	gormDB, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open gorm: %v", err)
	}
	stripe := newSubscriptionStripeStub(t)
	purchase, err := billing.NewTopupService(billing.TopupServiceConfig{
		DB:            gormDB,
		Config:        topupHTTPTestConfig(stripe.URL),
		StripeBaseURL: stripe.URL,
	})
	if err != nil {
		t.Fatalf("NewTopupService: %v", err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := store.NewSubscriptionRepo(db)
	capture := &subAuditExecer{}
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: repo, DB: db, Redis: rdb, Authz: rbac.NewAuthzMiddleware(),
		Audit: audit.New(capture), Logger: testLogger(), Purchase: purchase,
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &subscriptionIntegrationEnv{
		db: db, repo: repo, router: mux, audit: capture, purchase: purchase,
	}
}

func (e *subscriptionIntegrationEnv) do(t *testing.T, principal *auth.Principal, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if principal != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	}
	return serve(e.router, req)
}

func subscriptionITKey() string {
	return fmt.Sprintf("d5b-%d", time.Now().UnixNano())
}

// subAuditObject 把审计 INSERT 的 jsonb 参数（[]byte 或 string）解码为对象。
func subAuditObject(t *testing.T, value any) map[string]any {
	t.Helper()
	var raw []byte
	switch v := value.(type) {
	case nil:
		t.Fatalf("审计 jsonb 参数为空")
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		t.Fatalf("审计 jsonb 参数类型 = %T", value)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("审计 jsonb 解码失败: %v（%s）", err, raw)
	}
	return out
}

func (e *subscriptionIntegrationEnv) insertUser(t *testing.T, role string) int64 {
	t.Helper()
	username := fmt.Sprintf("d5b_%d_%d", time.Now().UnixNano(), role[0])
	var id int64
	if err := e.db.QueryRow(`INSERT INTO users (username, role) VALUES ($1, $2) RETURNING id`,
		username, role).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

func (e *subscriptionIntegrationEnv) insertPlanRow(t *testing.T, planKey string, status string) int64 {
	t.Helper()
	var id int64
	err := e.db.QueryRow(`
		INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency, grant_credits,
			 reset_on_renew, allow_wallet_overflow, grace_days, status, sort_order)
		VALUES ($1, 'IT Plan', 30, 9900, 'CNY', 1000, true, true, 0, $2, 1)
		RETURNING id`, planKey, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, id) })
	return id
}

func (e *subscriptionIntegrationEnv) insertSubscription(t *testing.T, userID, planID int64, status string, used int64) int64 {
	t.Helper()
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(30 * 24 * time.Hour)
	nextReset := start.Add(24 * time.Hour)
	var id int64
	err := e.db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 currency, price_paid_amount, period_days, reset_on_renew, grace_days)
		VALUES ($1, $2, 'IT Plan', $3, $4, $5, $6, 1000, $7, true,
		        'CNY', 9900, 30, true, 0)
		RETURNING id`,
		userID, planID, status, start, end, nextReset, used).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM subscriptions WHERE id = $1`, id) })
	return id
}

func (e *subscriptionIntegrationEnv) insertOrder(t *testing.T, planID int64, status string) int64 {
	t.Helper()
	var id int64
	err := e.db.QueryRow(`
		INSERT INTO orders (user_id, provider, amount, currency, status,
			idempotency_key, biz_kind, biz_ref_id)
		VALUES (900090, 'stripe', 9900, 'CNY', $1, $2, 'subscribe', $3)
		RETURNING id`,
		status, fmt.Sprintf("d5b-order-%d-%s", time.Now().UnixNano(), status), planID).Scan(&id)
	if err != nil {
		t.Fatalf("insert order: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM orders WHERE id = $1`, id) })
	return id
}

func (e *subscriptionIntegrationEnv) countRows(t *testing.T, table string) int64 {
	t.Helper()
	var count int64
	// table 只来自本测试的常量白名单，不接受外部输入。
	if err := e.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func TestSubscriptionIntegrationCRUDAndReferenceProtection(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	key := subscriptionITKey()
	planKey := "d5b-plan-" + key[4:]

	body := func(title string) map[string]any {
		return map[string]any{
			"plan_key": planKey, "title": title, "period_days": 30,
			"price_amount": 9900, "currency": "CNY", "grant_credits": 1000,
			"model_scope": []string{"gpt-4o"}, "sort_order": 5,
		}
	}
	rr := env.do(t, admin, http.MethodPost, "/api/subscription-plans", body("IT Plan"), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var plan subscriptionPlanView
	if err := json.Unmarshal(rr.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}

	// UNIQUE（含 disabled 行）→ 409。
	rr = env.do(t, admin, http.MethodPost, "/api/subscription-plans", body("Duplicate"), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d（body=%s）", rr.Code, rr.Body.String())
	}

	// 实例引用 + pending 订单引用 → 409，reference_count = 2。
	userID := env.insertUser(t, "user")
	env.insertSubscription(t, userID, plan.ID, "active", 100)
	env.insertOrder(t, plan.ID, "pending")
	rr = env.do(t, admin, http.MethodDelete, fmt.Sprintf("/api/subscription-plans/%d", plan.ID), nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete referenced = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var problem map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &problem)
	if problem["reference_count"] != float64(2) || problem["order_reference_count"] != float64(1) {
		t.Fatalf("problem = %v", problem)
	}

	// 清理引用后删除成功。
	if _, err := env.db.Exec(`DELETE FROM subscriptions WHERE plan_id = $1`, plan.ID); err != nil {
		t.Fatalf("cleanup subscriptions: %v", err)
	}
	if _, err := env.db.Exec(`DELETE FROM orders WHERE biz_ref_id = $1 AND biz_kind = 'subscribe'`, plan.ID); err != nil {
		t.Fatalf("cleanup orders: %v", err)
	}
	rr = env.do(t, admin, http.MethodDelete, fmt.Sprintf("/api/subscription-plans/%d", plan.ID), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d（body=%s）", rr.Code, rr.Body.String())
	}
	rr = env.do(t, admin, http.MethodGet, fmt.Sprintf("/api/subscription-plans/%d", plan.ID), nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get deleted = %d", rr.Code)
	}
}

func TestSubscriptionIntegrationResetQuotaAdvancesCursor(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	key := subscriptionITKey()
	planID := env.insertPlanRow(t, "d5b-reset-"+key[4:], "enabled")
	userID := env.insertUser(t, "user")
	activeID := env.insertSubscription(t, userID, planID, "active", 700)
	env.insertSubscription(t, userID, planID, "cancelled", 500)

	var beforeNext time.Time
	if err := env.db.QueryRow(`SELECT next_reset_at FROM subscriptions WHERE id = $1`, activeID).Scan(&beforeNext); err != nil {
		t.Fatalf("read next_reset_at: %v", err)
	}

	rr := env.do(t, admin, http.MethodPost, fmt.Sprintf("/api/subscription-plans/%d/reset-quota", planID),
		map[string]any{"advance_next_reset": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var reset subscriptionResetQuotaView
	_ = json.Unmarshal(rr.Body.Bytes(), &reset)
	if reset.Affected != 1 {
		t.Fatalf("affected = %d，期望仅活跃实例 1", reset.Affected)
	}

	var used int64
	var afterNext time.Time
	if err := env.db.QueryRow(`SELECT amount_used, next_reset_at FROM subscriptions WHERE id = $1`, activeID).
		Scan(&used, &afterNext); err != nil {
		t.Fatalf("read after reset: %v", err)
	}
	if used != 0 {
		t.Fatalf("amount_used = %d，期望 0", used)
	}
	if !afterNext.Equal(beforeNext.Add(30 * 24 * time.Hour)) {
		t.Fatalf("next_reset_at = %v，期望推进 30 天（from %v）", afterNext, beforeNext)
	}

	// 终态实例不变。
	var cancelledUsed int64
	if err := env.db.QueryRow(`SELECT amount_used FROM subscriptions
		WHERE plan_id = $1 AND status = 'cancelled'`, planID).Scan(&cancelledUsed); err != nil {
		t.Fatalf("read cancelled: %v", err)
	}
	if cancelledUsed != 500 {
		t.Fatalf("终态实例 amount_used = %d，不应被重置", cancelledUsed)
	}
}

func TestSubscriptionIntegrationBillingPreference(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	userID := env.insertUser(t, "user")
	principal := &auth.Principal{UserID: userID, Role: rbac.RoleUser}

	rr := env.do(t, principal, http.MethodGet, "/api/billing-preference", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get default = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var view billingPreferenceView
	_ = json.Unmarshal(rr.Body.Bytes(), &view)
	if view.Preference != store.BillingPreferenceSubscriptionFirst {
		t.Fatalf("default = %q", view.Preference)
	}

	for _, tc := range []struct{ input, want string }{
		{"wallet_first", store.BillingPreferenceWalletFirst},
		{"subscription_only", store.BillingPreferenceSubscriptionOnly},
		{"wallet_only", store.BillingPreferenceWalletOnly},
		{"not-a-preference", store.BillingPreferenceSubscriptionFirst},
		{"", store.BillingPreferenceSubscriptionFirst},
	} {
		rr := env.do(t, principal, http.MethodPut, "/api/billing-preference",
			map[string]any{"preference": tc.input}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("put %q = %d（body=%s）", tc.input, rr.Code, rr.Body.String())
		}
		var got string
		if err := env.db.QueryRow(`SELECT billing_preference FROM users WHERE id = $1`, userID).Scan(&got); err != nil {
			t.Fatalf("read preference: %v", err)
		}
		if got != tc.want {
			t.Fatalf("put %q 落库 = %q，期望 %q", tc.input, got, tc.want)
		}
	}

	// 审计 before/after 必须反映真实迁移：先置 wallet_first，再改 wallet_only，
	// 最后一条审计的 before 必须是落库前的 wallet_first，而不是请求体原文。
	for _, input := range []string{"wallet_first", "wallet_only"} {
		if rr := env.do(t, principal, http.MethodPut, "/api/billing-preference",
			map[string]any{"preference": input}, nil); rr.Code != http.StatusOK {
			t.Fatalf("put %q = %d（body=%s）", input, rr.Code, rr.Body.String())
		}
	}
	args := env.audit.last(t)
	detail := subAuditObject(t, args[9])
	if detail["action"] != audit.ActionBillingPreferenceUpdate {
		t.Fatalf("审计 action = %v，期望 %s", detail["action"], audit.ActionBillingPreferenceUpdate)
	}
	beforePref := subAuditObject(t, args[10])["preference"]
	afterPref := subAuditObject(t, args[11])["preference"]
	if beforePref != store.BillingPreferenceWalletFirst || afterPref != store.BillingPreferenceWalletOnly {
		t.Fatalf("审计 before/after = %v/%v，期望 %q/%q",
			beforePref, afterPref, store.BillingPreferenceWalletFirst, store.BillingPreferenceWalletOnly)
	}

	// 另一用户的偏好不受影响。
	otherID := env.insertUser(t, "user")
	var other string
	if err := env.db.QueryRow(`SELECT billing_preference FROM users WHERE id = $1`, otherID).Scan(&other); err != nil {
		t.Fatalf("read other: %v", err)
	}
	if other != store.BillingPreferenceSubscriptionFirst {
		t.Fatalf("他人偏好被改写为 %q", other)
	}
}

func TestSubscriptionIntegrationUserIsolation(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	key := subscriptionITKey()
	planID := env.insertPlanRow(t, "d5b-iso-"+key[4:], "enabled")
	userA := env.insertUser(t, "user")
	userB := env.insertUser(t, "user")
	env.insertSubscription(t, userA, planID, "active", 10)
	env.insertSubscription(t, userA, planID, "expired", 999)
	env.insertSubscription(t, userB, planID, "active", 20)

	rr := env.do(t, &auth.Principal{UserID: userA, Role: rbac.RoleUser},
		http.MethodGet, "/api/subscriptions", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("my subscriptions = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var mine userSubscriptionListView
	_ = json.Unmarshal(rr.Body.Bytes(), &mine)
	if mine.Total != 2 || len(mine.Items) != 2 {
		t.Fatalf("用户 A 应看到 2 条（含终态），得到 %+v", mine)
	}
	for _, item := range mine.Items {
		if item.Status == "active" && item.AmountRemain == nil {
			t.Fatalf("active 实例应有 amount_remain: %+v", item)
		}
	}

	// 用户侧分页：total 是全集（含终态），items 只含当前页。
	rr = env.do(t, &auth.Principal{UserID: userA, Role: rbac.RoleUser},
		http.MethodGet, "/api/subscriptions?p=2&page_size=1", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("paged my subscriptions = %d", rr.Code)
	}
	var page2 userSubscriptionListView
	_ = json.Unmarshal(rr.Body.Bytes(), &page2)
	if page2.Total != 2 || len(page2.Items) != 1 || page2.Page != 2 || page2.PageSize != 1 {
		t.Fatalf("用户侧分页 = %+v，期望 total=2 page=2 size=1 items=1", page2)
	}
}

// TestSubscriptionIntegrationPurchaseSettles is the S3 end-to-end proof on
// real PostgreSQL: purchase → provider callback → subscriptions row.
//
// 断言：订单判别列/金额/币种来自套餐；支付成功后实例快照正确；钱包流水
// 零新增；重复回调只建一行实例。
func TestSubscriptionIntegrationPurchaseSettles(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	userID := env.insertUser(t, "user")
	planID := env.insertPlanRow(t, "s3-"+subscriptionITKey(), "enabled")
	key := "s3-purchase-" + subscriptionITKey()

	beforeWallet := env.walletTxCount(t, userID)
	rr := env.do(t, &auth.Principal{UserID: userID, Role: rbac.RoleUser},
		http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": planID, "provider": "stripe"},
		map[string]string{"Idempotency-Key": key})
	if rr.Code != http.StatusCreated {
		t.Fatalf("purchase = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var created createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode purchase: %v", err)
	}
	if created.Order.Status != "pending" || created.Order.Amount != 9900 ||
		created.Order.Currency != "CNY" || created.ClientSecret == "" {
		t.Fatalf("购买响应 = %+v", created)
	}
	orderID := created.Order.ID
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM subscriptions WHERE order_id = $1`, orderID)
		_, _ = env.db.Exec(`DELETE FROM orders WHERE id = $1`, orderID)
	})

	var bizKind string
	var bizRef int64
	if err := env.db.QueryRow(`SELECT biz_kind, biz_ref_id FROM orders WHERE id = $1`, orderID).
		Scan(&bizKind, &bizRef); err != nil {
		t.Fatalf("read order: %v", err)
	}
	if bizKind != "subscribe" || bizRef != planID {
		t.Fatalf("订单判别列 = %s/%d，期望 subscribe/%d", bizKind, bizRef, planID)
	}
	var subCount int64
	if err := env.db.QueryRow(`SELECT count(*) FROM subscriptions WHERE order_id = $1`, orderID).
		Scan(&subCount); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subCount != 0 {
		t.Fatalf("支付前不得有实例，得到 %d 行", subCount)
	}

	var txnID string
	if err := env.db.QueryRow(`SELECT provider_txn_id FROM orders WHERE id = $1`, orderID).
		Scan(&txnID); err != nil {
		t.Fatalf("read provider txn: %v", err)
	}
	payload := subscriptionStripeEvent(txnID, 9900)
	sig := subscriptionStripeSig("whsec_test_123", time.Now().Unix(), payload)
	if err := env.purchase.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("stripe webhook: %v", err)
	}

	var (
		gotUser, gotPlan, gotTotal, gotUsed, gotPrice int64
		gotStatus, gotTitle, gotCurrency              string
		gotOrder                                      sql.NullInt64
		gotStart, gotEnd, gotReset                    sql.NullTime
		gotPeriod                                     int
		gotResetOnRenew, gotOverflow                  bool
	)
	if err := env.db.QueryRow(`
		SELECT user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
		       next_reset_at, amount_total, amount_used, allow_wallet_overflow,
		       order_id, currency, price_paid_amount, period_days, reset_on_renew
		FROM subscriptions WHERE order_id = $1`, orderID).Scan(
		&gotUser, &gotPlan, &gotTitle, &gotStatus, &gotStart, &gotEnd,
		&gotReset, &gotTotal, &gotUsed, &gotOverflow, &gotOrder, &gotCurrency,
		&gotPrice, &gotPeriod, &gotResetOnRenew); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if gotUser != userID || gotPlan != planID || gotTitle != "IT Plan" || gotStatus != "active" {
		t.Fatalf("实例身份 = user:%d plan:%d title:%s status:%s", gotUser, gotPlan, gotTitle, gotStatus)
	}
	if gotTotal != 1000 || gotUsed != 0 || !gotOverflow {
		t.Fatalf("额度快照 = total:%d used:%d overflow:%v", gotTotal, gotUsed, gotOverflow)
	}
	if !gotOrder.Valid || gotOrder.Int64 != orderID {
		t.Fatalf("order_id = %v", gotOrder)
	}
	if gotCurrency != "CNY" || gotPrice != 9900 || gotPeriod != 30 || !gotResetOnRenew {
		t.Fatalf("购买快照 = currency:%s price:%d period:%d reset:%v",
			gotCurrency, gotPrice, gotPeriod, gotResetOnRenew)
	}
	if !gotReset.Valid || !gotReset.Time.Equal(gotEnd.Time) {
		t.Fatalf("next_reset_at = %v，期望等于 end_at %v", gotReset.Time, gotEnd.Time)
	}
	if span := gotEnd.Time.Sub(gotStart.Time); span < 29*24*time.Hour || span > 31*24*time.Hour {
		t.Fatalf("end_at-start_at = %v，期望约 30 天", span)
	}
	if got := env.walletTxCount(t, userID); got != beforeWallet {
		t.Fatalf("订阅购买不得写 wallet_transactions：%d → %d", beforeWallet, got)
	}

	// 重复回调：幂等，只一行实例，钱包不变。
	if err := env.purchase.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("replay webhook: %v", err)
	}
	if err := env.db.QueryRow(`SELECT count(*) FROM subscriptions WHERE order_id = $1`, orderID).
		Scan(&subCount); err != nil {
		t.Fatalf("count subscriptions after replay: %v", err)
	}
	if subCount != 1 {
		t.Fatalf("重复回调后实例数 = %d，期望 1", subCount)
	}
	if got := env.walletTxCount(t, userID); got != beforeWallet {
		t.Fatalf("重复回调后钱包流水 = %d，期望 %d", got, beforeWallet)
	}
}

func (e *subscriptionIntegrationEnv) walletTxCount(t *testing.T, userID int64) int64 {
	t.Helper()
	var count int64
	if err := e.db.QueryRow(`SELECT count(*) FROM wallet_transactions WHERE user_id = $1`, userID).
		Scan(&count); err != nil {
		t.Fatalf("count wallet transactions: %v", err)
	}
	return count
}

// TestSubscriptionIntegrationPurchaseRejectsDisabledPlan covers the 409 path on
// real SQL: a disabled plan must not create an order.
func TestSubscriptionIntegrationPurchaseRejectsDisabledPlan(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	userID := env.insertUser(t, "user")
	planID := env.insertPlanRow(t, "s3-disabled-"+subscriptionITKey(), "disabled")
	key := "s3-disabled-purchase-" + subscriptionITKey()

	rr := env.do(t, &auth.Principal{UserID: userID, Role: rbac.RoleUser},
		http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": planID, "provider": "stripe"},
		map[string]string{"Idempotency-Key": key})
	if rr.Code != http.StatusConflict {
		t.Fatalf("停用套餐购买 = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	var count int64
	if err := env.db.QueryRow(`SELECT count(*) FROM orders WHERE idempotency_key = $1`, key).
		Scan(&count); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if count != 0 {
		t.Fatalf("拒绝的购买不得留订单，得到 %d 行", count)
	}
}

func TestSubscriptionIntegrationCreateIdempotency(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	admin := &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
	key := subscriptionITKey()
	planKey := "d5b-idem-" + key[4:]
	body := map[string]any{
		"plan_key": planKey, "title": "Idem", "period_days": 7,
		"price_amount": 100, "currency": "CNY", "grant_credits": 10,
	}
	t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM subscription_plans WHERE plan_key = $1`, planKey) })
	headers := map[string]string{"Idempotency-Key": "it-" + key}

	rr1 := env.do(t, admin, http.MethodPost, "/api/subscription-plans", body, headers)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first = %d（body=%s）", rr1.Code, rr1.Body.String())
	}
	rr2 := env.do(t, admin, http.MethodPost, "/api/subscription-plans", body, headers)
	if rr2.Code != http.StatusCreated || rr2.Body.String() != rr1.Body.String() {
		t.Fatalf("replay = %d（body=%s）", rr2.Code, rr2.Body.String())
	}
	var count int64
	if err := env.db.QueryRow(`SELECT count(*) FROM subscription_plans WHERE plan_key = $1`, planKey).Scan(&count); err != nil {
		t.Fatalf("count plans: %v", err)
	}
	if count != 1 {
		t.Fatalf("plan_key 行数 = %d，期望 1（重放不得重复创建）", count)
	}
	if !strings.HasPrefix(planKey, "d5b-idem-") {
		t.Fatalf("plan key 前缀异常: %s", planKey)
	}
}
