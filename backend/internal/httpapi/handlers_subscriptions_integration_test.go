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

// TestSubscriptionIntegrationCancelAtPeriodEnd 在真实 PostgreSQL 上验证用户侧
// 周期末取消：条件 UPDATE 的幂等、终态 409、跨用户 404（不泄露存在性）与
// 未认证 403。store 层的单条 SQL 语义另由 subscription_repo_integration_test 覆盖。
func TestSubscriptionIntegrationCancelAtPeriodEnd(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	planID := env.insertPlanRow(t, "cancel-"+subscriptionITKey(), "enabled")
	owner := env.insertUser(t, "user")
	other := env.insertUser(t, "user")
	activeID := env.insertSubscription(t, owner, planID, "active", 10)
	foreignID := env.insertSubscription(t, other, planID, "active", 10)
	expiredID := env.insertSubscription(t, owner, planID, "expired", 10)
	cancelledID := env.insertSubscription(t, owner, planID, "cancelled", 10)
	ownerPrincipal := &auth.Principal{UserID: owner, Role: rbac.RoleUser}

	// 本人取消：200 + 更新后的用户侧投影；重复调用仍 200（幂等）。
	for attempt := 1; attempt <= 2; attempt++ {
		rr := env.do(t, ownerPrincipal, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/cancel", activeID), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("第 %d 次取消 = %d（body=%s）", attempt, rr.Code, rr.Body.String())
		}
		var view userSubscriptionView
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode subscription: %v", err)
		}
		if view.ID != activeID || view.Status != "active" || !view.CancelAtPeriodEnd {
			t.Fatalf("取消后视图 = %+v", view)
		}
		var raw map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode raw: %v", err)
		}
		for _, forbidden := range []string{"user_id", "order_id", "price_paid_amount", "currency", "allow_wallet_overflow"} {
			if _, exists := raw[forbidden]; exists {
				t.Fatalf("用户视图泄露 %q: %v", forbidden, raw)
			}
		}
	}
	var cancelled bool
	if err := env.db.QueryRow(`SELECT cancel_at_period_end FROM subscriptions WHERE id = $1`, activeID).Scan(&cancelled); err != nil {
		t.Fatalf("read cancel flag: %v", err)
	}
	if !cancelled {
		t.Fatal("cancel_at_period_end 未落库")
	}

	// 他人实例与不存在统一 404，且不得改动他人行。
	if rr := env.do(t, ownerPrincipal, http.MethodPost,
		fmt.Sprintf("/api/subscriptions/%d/cancel", foreignID), nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("他人实例 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if err := env.db.QueryRow(`SELECT cancel_at_period_end FROM subscriptions WHERE id = $1`, foreignID).Scan(&cancelled); err != nil {
		t.Fatalf("read foreign flag: %v", err)
	}
	if cancelled {
		t.Fatal("他人实例不应被置位")
	}
	if rr := env.do(t, ownerPrincipal, http.MethodPost,
		"/api/subscriptions/99999999/cancel", nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("不存在实例 = %d（body=%s）", rr.Code, rr.Body.String())
	}

	// 终态 409。
	for name, id := range map[string]int64{"expired": expiredID, "cancelled": cancelledID} {
		rr := env.do(t, ownerPrincipal, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/cancel", id), nil, nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s = %d（body=%s）", name, rr.Code, rr.Body.String())
		}
		if got := decodeSubProblemType(t, rr); got != problemTypeSubscriptionNotActive {
			t.Fatalf("%s type = %s", name, got)
		}
	}

	// authz 层在缺少 Principal 时失败关闭为 403（生产链路上会话中间件会先给 401）。
	if rr := env.do(t, nil, http.MethodPost,
		fmt.Sprintf("/api/subscriptions/%d/cancel", activeID), nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("未登录 = %d（body=%s）", rr.Code, rr.Body.String())
	}

	// 审计：subscription.cancel + detail 仅 action/id。
	args := env.audit.last(t)
	if category, _ := args[2].(string); category != "operation" {
		t.Fatalf("audit category = %v，期望 operation", args[2])
	}
	detail := subAuditObject(t, args[9])
	if detail["action"] != audit.ActionSubscriptionCancel || detail["id"] != float64(activeID) || len(detail) != 2 {
		t.Fatalf("audit detail = %v", detail)
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

// TestSubscriptionIntegrationRenew 覆盖 P2a 手动续费的端点 + 真实 SQL：
// 订单 biz_kind='renew'/biz_ref_id=subscriptions.id，支付回调在真实 webhook
// 路径上**复用同一实例**推进 end_at；重复回调只扩展一次；cancelled 409；
// 停用套餐 409；他人实例 404；钱包零流水。
func TestSubscriptionIntegrationRenew(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	userID := env.insertUser(t, "user")
	otherID := env.insertUser(t, "user")
	planID := env.insertPlanRow(t, "p2a-"+subscriptionITKey(), "enabled")
	subID := env.insertSubscription(t, userID, planID, "active", 321)
	foreignID := env.insertSubscription(t, otherID, planID, "active", 0)
	cancelledID := env.insertSubscription(t, userID, planID, "cancelled", 0)

	var oldEnd time.Time
	if err := env.db.QueryRow(`SELECT end_at FROM subscriptions WHERE id = $1`, subID).
		Scan(&oldEnd); err != nil {
		t.Fatalf("read old end_at: %v", err)
	}
	beforeWallet := env.walletTxCount(t, userID)
	owner := &auth.Principal{UserID: userID, Role: rbac.RoleUser}
	headers := map[string]string{
		"Idempotency-Key": "p2a-reenew-" + subscriptionITKey(),
	}

	rr := env.do(t, owner, http.MethodPost,
		fmt.Sprintf("/api/subscriptions/%d/renew", subID),
		map[string]any{"provider": "stripe"}, headers)
	if rr.Code != http.StatusCreated {
		t.Fatalf("renew = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var created createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode renewal: %v", err)
	}
	if created.Order.Status != "pending" || created.Order.Amount != 9900 || created.ClientSecret == "" {
		t.Fatalf("续费响应 = %+v", created)
	}
	orderID := created.Order.ID
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM orders WHERE id = $1`, orderID)
	})
	var bizKind string
	var bizRef int64
	if err := env.db.QueryRow(`SELECT biz_kind, biz_ref_id FROM orders WHERE id = $1`, orderID).
		Scan(&bizKind, &bizRef); err != nil {
		t.Fatalf("read renewal order: %v", err)
	}
	if bizKind != "renew" || bizRef != subID {
		t.Fatalf("订单判别列 = %s/%d，期望 renew/%d", bizKind, bizRef, subID)
	}

	var txnID string
	if err := env.db.QueryRow(`SELECT provider_txn_id FROM orders WHERE id = $1`, orderID).
		Scan(&txnID); err != nil {
		t.Fatalf("read provider txn: %v", err)
	}
	payload := subscriptionStripeEvent(txnID, 9900)
	sig := subscriptionStripeSig("whsec_test_123", time.Now().Unix(), payload)
	if err := env.purchase.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("renew stripe webhook: %v", err)
	}

	var (
		gotStatus, gotTitle         string
		gotEnd, gotReset            sql.NullTime
		gotUsed, gotTotal, gotPrice int64
		gotCancel                   bool
	)
	if err := env.db.QueryRow(`
		SELECT status, plan_title_snapshot, end_at, next_reset_at,
		       amount_used, amount_total, price_paid_amount, cancel_at_period_end
		FROM subscriptions WHERE id = $1`, subID).Scan(
		&gotStatus, &gotTitle, &gotEnd, &gotReset, &gotUsed, &gotTotal,
		&gotPrice, &gotCancel); err != nil {
		t.Fatalf("read renewed subscription: %v", err)
	}
	if gotStatus != "active" || gotUsed != 0 || gotTotal != 1000 || gotPrice != 9900 {
		t.Fatalf("续费后实例 = status:%s used:%d total:%d price:%d",
			gotStatus, gotUsed, gotTotal, gotPrice)
	}
	if gotCancel {
		t.Fatal("续费必须清 cancel_at_period_end")
	}
	if !gotEnd.Valid || gotEnd.Time.Sub(oldEnd) < 29*24*time.Hour ||
		gotEnd.Time.Sub(oldEnd) > 30*24*time.Hour+time.Minute {
		t.Fatalf("end_at = %s，期望在旧值 %s 上 +30 天", gotEnd.Time, oldEnd)
	}
	if !gotReset.Valid || !gotReset.Time.Equal(gotEnd.Time) {
		t.Fatalf("next_reset_at = %v，期望等于 end_at %s", gotReset.Time, gotEnd.Time)
	}
	if gotTitle != "IT Plan" {
		t.Fatalf("续费快照 title = %s", gotTitle)
	}
	if got := env.walletTxCount(t, userID); got != beforeWallet {
		t.Fatalf("续费不得写钱包流水：%d → %d", beforeWallet, got)
	}

	// 重复回调：订单已 succeeded，只幂等返回，不再推进 end_at。
	if err := env.purchase.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("replay renewal webhook: %v", err)
	}
	var afterEnd time.Time
	if err := env.db.QueryRow(`SELECT end_at FROM subscriptions WHERE id = $1`, subID).
		Scan(&afterEnd); err != nil {
		t.Fatalf("read end_at after replay: %v", err)
	}
	if !afterEnd.Equal(gotEnd.Time) {
		t.Fatalf("重复回调再次推进 end_at：%s → %s", gotEnd.Time, afterEnd)
	}

	t.Run("cancelled 409", func(t *testing.T) {
		rr := env.do(t, owner, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/renew", cancelledID),
			map[string]any{"provider": "stripe"},
			map[string]string{"Idempotency-Key": "p2a-cancelled-" + subscriptionITKey()})
		if rr.Code != http.StatusConflict {
			t.Fatalf("cancelled = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("他人实例 404", func(t *testing.T) {
		rr := env.do(t, owner, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/renew", foreignID),
			map[string]any{"provider": "stripe"},
			map[string]string{"Idempotency-Key": "p2a-foreign-" + subscriptionITKey()})
		if rr.Code != http.StatusNotFound {
			t.Fatalf("他人实例 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("套餐停用 409", func(t *testing.T) {
		disabledPlan := env.insertPlanRow(t, "p2a-disabled-"+subscriptionITKey(), "disabled")
		disabledSub := env.insertSubscription(t, userID, disabledPlan, "active", 0)
		rr := env.do(t, owner, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/renew", disabledSub),
			map[string]any{"provider": "stripe"},
			map[string]string{"Idempotency-Key": "p2a-disabled-" + subscriptionITKey()})
		if rr.Code != http.StatusConflict {
			t.Fatalf("停用套餐 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var count int64
		if err := env.db.QueryRow(`SELECT count(*) FROM orders WHERE biz_kind = 'renew' AND biz_ref_id = $1`,
			disabledSub).Scan(&count); err != nil {
			t.Fatalf("count renewal orders: %v", err)
		}
		if count != 0 {
			t.Fatalf("停用套餐不得留续费订单，得到 %d 行", count)
		}
	})
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

// ---------------------------------------------------------------------------
// P2b 余额购买（provider=wallet）
// ---------------------------------------------------------------------------

// insertWalletUser 创建用户并设置初始余额；清理时先删该用户的订阅/订单/流水。
func (e *subscriptionIntegrationEnv) insertWalletUser(t *testing.T, balance int64) int64 {
	t.Helper()
	userID := e.insertUser(t, "user")
	if _, err := e.db.Exec(`UPDATE users SET balance = $2 WHERE id = $1`, userID, balance); err != nil {
		t.Fatalf("set wallet balance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.db.Exec(`DELETE FROM subscriptions WHERE user_id = $1`, userID)
		_, _ = e.db.Exec(`DELETE FROM orders WHERE user_id = $1`, userID)
		_, _ = e.db.Exec(`DELETE FROM wallet_transactions WHERE user_id = $1`, userID)
	})
	return userID
}

func (e *subscriptionIntegrationEnv) walletBalance(t *testing.T, userID int64) int64 {
	t.Helper()
	var balance int64
	if err := e.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).
		Scan(&balance); err != nil {
		t.Fatalf("read wallet balance: %v", err)
	}
	return balance
}

// TestSubscriptionIntegrationWalletPurchase 覆盖 P2b 的 HTTP 面：余额充足
// 下单即 succeeded（无支付凭据），余额与流水正确；同 key 重放不二次扣款；
// 余额不足 402 且零写入；跨用户同 key 409 且不扣他人余额。
func TestSubscriptionIntegrationWalletPurchase(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	alice := env.insertWalletUser(t, 10_000)
	bob := env.insertWalletUser(t, 5_000)
	planID := env.insertPlanRow(t, "p2b-"+subscriptionITKey(), "enabled")
	key := "p2b-wallet-" + subscriptionITKey()
	principal := func(id int64) *auth.Principal {
		return &auth.Principal{UserID: id, Role: rbac.RoleUser}
	}

	rr := env.do(t, principal(alice), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": planID, "provider": "wallet"},
		map[string]string{"Idempotency-Key": key})
	if rr.Code != http.StatusCreated {
		t.Fatalf("wallet purchase = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var created createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode purchase: %v", err)
	}
	if created.ClientSecret != "" || created.QRCode != "" ||
		created.Order.Status != string(billing.OrderSucceeded) ||
		created.Order.Provider != billing.ProviderWallet {
		t.Fatalf("余额购买响应 = %+v", created)
	}
	if got := env.walletBalance(t, alice); got != 100 {
		t.Fatalf("alice balance = %d，期望 100", got)
	}
	var (
		txCount int64
		amount  int64
		subs    int64
	)
	if err := env.db.QueryRow(`SELECT count(*), COALESCE(min(amount), 0)
		FROM wallet_transactions WHERE user_id = $1 AND type = 'subscription_purchase'`,
		alice).Scan(&txCount, &amount); err != nil {
		t.Fatalf("read wallet tx: %v", err)
	}
	if txCount != 1 || amount != -9_900 {
		t.Fatalf("流水 = count:%d amount:%d，期望 1 / -9900", txCount, amount)
	}
	if err := env.db.QueryRow(`SELECT count(*) FROM subscriptions WHERE order_id = $1`,
		created.Order.ID).Scan(&subs); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	if subs != 1 {
		t.Fatalf("订阅实例数 = %d，期望 1", subs)
	}

	// 同 key 重放：200 + 同一订单，不二次扣款。
	rr = env.do(t, principal(alice), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": planID, "provider": "wallet"},
		map[string]string{"Idempotency-Key": key})
	if rr.Code != http.StatusOK {
		t.Fatalf("replay = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var replayed createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &replayed); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if replayed.Order.ID != created.Order.ID {
		t.Fatalf("重放订单 id = %d，期望 %d", replayed.Order.ID, created.Order.ID)
	}
	if got := env.walletBalance(t, alice); got != 100 {
		t.Fatalf("重放后 alice balance = %d，期望 100", got)
	}

	// 跨用户重放同一 key：409，且不扣 bob 余额。
	rr = env.do(t, principal(bob), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": planID, "provider": "wallet"},
		map[string]string{"Idempotency-Key": key})
	if rr.Code != http.StatusConflict {
		t.Fatalf("跨用户重放 = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	if got := env.walletBalance(t, bob); got != 5_000 {
		t.Fatalf("bob balance = %d，期望 5000", got)
	}

	// 余额不足：402，零写入。
	rr = env.do(t, principal(bob), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": planID, "provider": "wallet"},
		map[string]string{"Idempotency-Key": "p2b-short-" + subscriptionITKey()})
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("余额不足 = %d，期望 402（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient_wallet_balance") {
		t.Fatalf("402 problem type 不符: %s", rr.Body.String())
	}
	var shortTx, shortSubs, shortOrders int64
	if err := env.db.QueryRow(`SELECT
		(SELECT count(*) FROM wallet_transactions WHERE user_id = $1 AND type = 'subscription_purchase'),
		(SELECT count(*) FROM subscriptions WHERE user_id = $1),
		(SELECT count(*) FROM orders WHERE user_id = $1)`,
		bob).Scan(&shortTx, &shortSubs, &shortOrders); err != nil {
		t.Fatalf("read insufficient state: %v", err)
	}
	if shortTx != 0 || shortSubs != 0 || shortOrders != 0 {
		t.Fatalf("余额不足零写入 = tx:%d subs:%d orders:%d", shortTx, shortSubs, shortOrders)
	}
	if got := env.walletBalance(t, bob); got != 5_000 {
		t.Fatalf("余额不足后 bob balance = %d，期望 5000", got)
	}
}

// TestSubscriptionIntegrationEventsVisibility 是 S1 端点的数据库级验收：
// 创建实例与 created 事件同事务可见，本人/users:read 可读，他人 404，
// after_seq 增量读取在真实 SQL 上成立。
func TestSubscriptionIntegrationEventsVisibility(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	planID := env.insertPlanRow(t, "events-"+subscriptionITKey(), "enabled")
	owner := env.insertUser(t, "user")
	other := env.insertUser(t, "user")

	now := time.Now().UTC()
	sub, err := env.repo.CreateSubscription(context.Background(), store.SubscriptionCreateInput{
		UserID:            owner,
		PlanID:            planID,
		PlanTitleSnapshot: "IT Plan",
		StartAt:           now.Add(-time.Hour),
		EndAt:             now.Add(30 * 24 * time.Hour),
		AmountTotal:       1000,
		Currency:          "CNY",
		PricePaidAmount:   9900,
		PeriodDays:        30,
		ResetOnRenew:      true,
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM subscription_events WHERE subscription_id = $1`, sub.ID)
		_, _ = env.db.Exec(`DELETE FROM subscriptions WHERE id = $1`, sub.ID)
	})

	path := fmt.Sprintf("/api/subscriptions/%d/events", sub.ID)
	decode := func(rr *httptest.ResponseRecorder) subscriptionEventListView {
		t.Helper()
		var body subscriptionEventListView
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode events: %v（body=%s）", err, rr.Body.String())
		}
		return body
	}

	rr := env.do(t, &auth.Principal{UserID: owner, Role: rbac.RoleUser}, http.MethodGet, path, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("本人读取 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	body := decode(rr)
	if len(body.Events) != 1 || body.Events[0].Seq != 1 ||
		body.Events[0].Kind != store.SubscriptionEventCreated {
		t.Fatalf("events = %+v", body.Events)
	}
	var detail map[string]any
	if err := json.Unmarshal(body.Events[0].Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail["plan_id"] != float64(planID) || detail["price_paid_amount"] != float64(9900) {
		t.Fatalf("created detail = %v", detail)
	}

	// 他人 404（不是 403）；users:read 管理员 200；无 users:read 管理员 404。
	if rr := env.do(t, &auth.Principal{UserID: other, Role: rbac.RoleUser}, http.MethodGet, path, nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("他人读取 = %d，期望 404", rr.Code)
	}
	if rr := env.do(t, adminRestricted(rbac.PermUsersRead), http.MethodGet, path, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("users:read 管理员读取 = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if rr := env.do(t, adminRestricted(rbac.PermSettingsManage), http.MethodGet, path, nil, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("无 users:read 管理员读取 = %d，期望 404", rr.Code)
	}
	if rr := env.do(t, &auth.Principal{UserID: owner, Role: rbac.RoleUser}, http.MethodGet, path+"?after_seq=1", nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("after_seq 读取 = %d", rr.Code)
	} else if got := decode(rr); len(got.Events) != 0 {
		t.Fatalf("after_seq=1 应为空，实际 %+v", got.Events)
	}
}

// TestSubscriptionIntegrationChangePlan 走真实 HTTP + 真实 PostgreSQL 验证 S2
// 升级链路：应补差价落 change 订单 → 渠道回调 → 同一实例迁移到新套餐、
// 额度重算、追加 upgraded 事件；跨周期/同套餐/终态各自返回契约内的状态码。
func TestSubscriptionIntegrationChangePlan(t *testing.T) {
	env := newSubscriptionIntegrationEnv(t)
	userID := env.insertUser(t, "user")
	oldPlanID := env.insertPlanRow(t, "s2-old-"+subscriptionITKey(), "enabled")
	newPlanID := env.insertPlanRowWithPrice(t, "s2-new-"+subscriptionITKey(), 19900, 5000, 30)

	now := time.Now().UTC().Truncate(time.Second)
	start := now.Add(-15 * 24 * time.Hour)
	var subID int64
	if err := env.db.QueryRow(`
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 currency, price_paid_amount, period_days, reset_on_renew, grace_days)
		VALUES ($1, $2, 'IT Plan', 'active', $3, $4, $4, 1000, 321, true,
		        'CNY', 9900, 30, true, 0)
		RETURNING id`, userID, oldPlanID, start, now.Add(15*24*time.Hour)).Scan(&subID); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM subscription_events WHERE subscription_id = $1`, subID)
		_, _ = env.db.Exec(`DELETE FROM orders WHERE (biz_kind = 'change' AND biz_ref_id = $1)
			OR idempotency_key LIKE 's2-change-%'`, subID)
		_, _ = env.db.Exec(`DELETE FROM subscriptions WHERE id = $1`, subID)
	})
	cancelledID := env.insertSubscription(t, userID, oldPlanID, "cancelled", 0)

	owner := &auth.Principal{UserID: userID, Role: rbac.RoleUser}
	path := fmt.Sprintf("/api/subscriptions/%d/change", subID)
	headers := map[string]string{"Idempotency-Key": "s2-change-" + subscriptionITKey()}

	rr := env.do(t, owner, http.MethodPost, path,
		map[string]any{"plan_id": newPlanID, "provider": "stripe"}, headers)
	if rr.Code != http.StatusCreated {
		t.Fatalf("change = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var created subscriptionChangeView
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode change: %v", err)
	}
	// 剩余 15 天 × 9900 / 30 天 = 4950 ⇒ 应补 = 19900 − 4950 = 14950（±1 抖动）。
	if created.Order.Status != "pending" || created.PriceDelta < 14949 || created.PriceDelta > 14950 {
		t.Fatalf("升降级响应 = %+v", created)
	}
	if created.EventKind != "upgraded" || created.CreditsDelta != 4000 ||
		created.FromPlanID != oldPlanID || created.ToPlanID != newPlanID {
		t.Fatalf("折算回显 = %+v", created)
	}
	orderID := created.Order.ID
	var bizKind string
	var bizRef int64
	if err := env.db.QueryRow(`SELECT biz_kind, biz_ref_id FROM orders WHERE id = $1`, orderID).
		Scan(&bizKind, &bizRef); err != nil {
		t.Fatalf("read change order: %v", err)
	}
	if bizKind != "change" || bizRef != subID {
		t.Fatalf("订单判别列 = %s/%d，期望 change/%d", bizKind, bizRef, subID)
	}

	var txnID string
	if err := env.db.QueryRow(`SELECT provider_txn_id FROM orders WHERE id = $1`, orderID).
		Scan(&txnID); err != nil {
		t.Fatalf("read provider txn: %v", err)
	}
	payload := subscriptionStripeEvent(txnID, created.Order.Amount)
	sig := subscriptionStripeSig("whsec_test_123", time.Now().Unix(), payload)
	if err := env.purchase.HandleStripeWebhook(context.Background(), payload, sig); err != nil {
		t.Fatalf("change stripe webhook: %v", err)
	}

	var (
		gotPlan, gotTotal, gotUsed, gotPrice int64
		gotEnd                               time.Time
		gotTitle                             string
	)
	if err := env.db.QueryRow(`
		SELECT plan_id, plan_title_snapshot, amount_total, amount_used,
		       price_paid_amount, end_at
		FROM subscriptions WHERE id = $1`, subID).Scan(
		&gotPlan, &gotTitle, &gotTotal, &gotUsed, &gotPrice, &gotEnd); err != nil {
		t.Fatalf("read changed subscription: %v", err)
	}
	if gotPlan != newPlanID || gotTotal != 5000 || gotUsed != 321 || gotPrice != 19900 {
		t.Fatalf("升级后实例 = plan:%d total:%d used:%d price:%d",
			gotPlan, gotTotal, gotUsed, gotPrice)
	}
	if gotTitle != "IT Plan" {
		t.Fatalf("升级后快照 title = %s", gotTitle)
	}
	if delta := gotEnd.Sub(now); delta < 29*24*time.Hour || delta > 31*24*time.Hour {
		t.Fatalf("升级后 end_at = %s（距 now %s），期望约 30 天", gotEnd, delta)
	}
	var events int
	if err := env.db.QueryRow(`SELECT count(*) FROM subscription_events
		WHERE subscription_id = $1 AND kind = 'upgraded'`, subID).Scan(&events); err != nil {
		t.Fatalf("count upgraded events: %v", err)
	}
	if events != 1 {
		t.Fatalf("upgraded 事件数 = %d，期望 1", events)
	}

	t.Run("跨周期 422", func(t *testing.T) {
		quarterly := env.insertPlanRowWithPrice(t, "s2-q-"+subscriptionITKey(), 29900, 8000, 90)
		rr := env.do(t, owner, http.MethodPost, path,
			map[string]any{"plan_id": quarterly, "provider": "stripe"},
			map[string]string{"Idempotency-Key": "s2-change-cycle-" + subscriptionITKey()})
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("跨周期 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var problem struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
			t.Fatalf("decode problem: %v", err)
		}
		if problem.Type != problemTypeCycleMismatch {
			t.Fatalf("problem type = %s，期望 %s", problem.Type, problemTypeCycleMismatch)
		}
	})

	t.Run("同套餐 422", func(t *testing.T) {
		rr := env.do(t, owner, http.MethodPost, fmt.Sprintf("/api/subscriptions/%d/change", subID),
			map[string]any{"plan_id": newPlanID, "provider": "stripe"},
			map[string]string{"Idempotency-Key": "s2-change-same-" + subscriptionITKey()})
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("同套餐 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("cancelled 409", func(t *testing.T) {
		rr := env.do(t, owner, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/change", cancelledID),
			map[string]any{"plan_id": newPlanID, "provider": "stripe"},
			map[string]string{"Idempotency-Key": "s2-change-cancel-" + subscriptionITKey()})
		if rr.Code != http.StatusConflict {
			t.Fatalf("cancelled = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

// insertPlanRowWithPrice 插入指定价格/额度/周期的套餐（S2 需要「新旧不同价」
// 与「跨周期」两类样本；insertPlanRow 固定 9900/1000/30 天）。
func (e *subscriptionIntegrationEnv) insertPlanRowWithPrice(t *testing.T,
	planKey string, price, grant int64, periodDays int32) int64 {

	t.Helper()
	var id int64
	err := e.db.QueryRow(`
		INSERT INTO subscription_plans
			(plan_key, title, period_days, price_amount, currency, grant_credits,
			 reset_on_renew, allow_wallet_overflow, grace_days, status, sort_order)
		VALUES ($1, 'IT Plan', $2, $3, 'CNY', $4, true, true, 0, 'enabled', 1)
		RETURNING id`, planKey, periodDays, price, grant).Scan(&id)
	if err != nil {
		t.Fatalf("insert plan with price: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.db.Exec(`DELETE FROM subscriptions WHERE plan_id = $1`, id)
		_, _ = e.db.Exec(`DELETE FROM orders WHERE biz_kind = 'subscribe' AND biz_ref_id = $1`, id)
		_, _ = e.db.Exec(`DELETE FROM subscription_plans WHERE id = $1`, id)
	})
	return id
}
