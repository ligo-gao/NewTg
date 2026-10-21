package httpapi

// D5b 订阅套餐端点的单元验收：内存假仓储 + miniredis，不依赖 PG。
// 真实 SQL（引用保护计数、reset-quota 游标推进、users.billing_preference）
// 由 handlers_subscriptions_integration_test.go 覆盖。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// fakeSubscriptionPurchase 记录购买服务收到的入参，供 handler 单测断言
// 「金额/币种全部由服务端按套餐读取」与错误映射。真实定价/事务行为由
// billing 与集成测试覆盖。
type fakeSubscriptionPurchase struct {
	calls        []subscriptionPurchaseCall
	renewCalls   []subscriptionRenewalCall
	changeCalls  []billing.ChangePlanInput
	refundCalls  []subscriptionRefundCall
	result       *billing.TopupOrder
	err          error
	renewResult  *billing.TopupOrder
	renewErr     error
	changeResult *billing.ChangePlanResult
	changeErr    error
	refundResult *billing.RefundRequest
	refundErr    error
}

// subscriptionRefundCall 记录用户侧退款申请（S3）。
type subscriptionRefundCall struct {
	subscriptionID int64
	userID         int64
	reason         string
}

// RequestSubscriptionRefund 是 S3 退款申请的桩：只记录入参并返回预设结果，
// 真实 P-13 判定与订单标记由 billing 集成测试覆盖。
func (f *fakeSubscriptionPurchase) RequestSubscriptionRefund(_ context.Context,
	subscriptionID, userID int64, reason string) (*billing.RefundRequest, error) {

	f.refundCalls = append(f.refundCalls, subscriptionRefundCall{
		subscriptionID: subscriptionID, userID: userID, reason: reason,
	})
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	if f.refundResult != nil {
		return f.refundResult, nil
	}
	return &billing.RefundRequest{
		OrderID: 10, RequestedAt: time.Unix(1_700_000_000, 0).UTC(), Reason: reason,
	}, nil
}

type subscriptionRenewalCall struct {
	userID         int64
	subscriptionID int64
	provider       string
	idempotencyKey string
}

type subscriptionPurchaseCall struct {
	userID         int64
	planID         int64
	provider       string
	idempotencyKey string
}

func (f *fakeSubscriptionPurchase) CreateSubscriptionOrder(_ context.Context,
	userID, planID int64, provider, idempotencyKey string) (*billing.TopupOrder, error) {

	f.calls = append(f.calls, subscriptionPurchaseCall{
		userID: userID, planID: planID, provider: provider, idempotencyKey: idempotencyKey,
	})
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &billing.TopupOrder{Order: &store.Order{
		ID: 10, UserID: userID, Provider: provider, Amount: 9900,
		Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindSubscribe,
		BizRefID: &planID, IdempotencyKey: idempotencyKey,
	}}, nil
}

func (f *fakeSubscriptionPurchase) CreateSubscriptionRenewalOrder(_ context.Context,
	userID, subscriptionID int64, provider, idempotencyKey string) (*billing.TopupOrder, error) {

	f.renewCalls = append(f.renewCalls, subscriptionRenewalCall{
		userID: userID, subscriptionID: subscriptionID,
		provider: provider, idempotencyKey: idempotencyKey,
	})
	if f.renewErr != nil {
		return nil, f.renewErr
	}
	if f.renewResult != nil {
		return f.renewResult, nil
	}
	return &billing.TopupOrder{Order: &store.Order{
		ID: 20, UserID: userID, Provider: provider, Amount: 9900,
		Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindRenew,
		BizRefID: &subscriptionID, IdempotencyKey: idempotencyKey,
	}}, nil
}

// ChangePlan 记录升降级入参并返回桩结果（S2）。真实折算/事务行为由
// billing 的集成测试覆盖。
func (f *fakeSubscriptionPurchase) ChangePlan(_ context.Context,
	in billing.ChangePlanInput) (billing.ChangePlanResult, error) {

	f.changeCalls = append(f.changeCalls, in)
	if f.changeErr != nil {
		return billing.ChangePlanResult{}, f.changeErr
	}
	if f.changeResult != nil {
		return *f.changeResult, nil
	}
	return billing.ChangePlanResult{
		OrderID: 30, Order: &store.Order{
			ID: 30, UserID: in.UserID, Provider: in.Provider, Amount: 14950,
			Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindChange,
			BizRefID: &in.SubscriptionID, IdempotencyKey: in.IdempotencyKey,
		},
		PriceDelta: 14950, CreditsDelta: 4000, EventKind: store.SubscriptionEventUpgraded,
		FromPlanID: 1, NewPlanID: in.NewPlanID,
	}, nil
}

// ---------------------------------------------------------------------------
// 假仓储
// ---------------------------------------------------------------------------

type fakeSubscriptionRepo struct {
	mu             sync.Mutex
	plans          map[int64]*store.SubscriptionPlan
	subs           map[int64]*store.Subscription
	pendingOrders  map[int64]int
	nextPlanID     int64
	nextSubID      int64
	resets         []resetCall
	preConsume     map[string]store.SubscriptionPreConsume
	events         map[int64][]store.SubscriptionEvent
	writeCallCount int
	// leakForeign 模拟仓储实现漂移：ListSubscriptionsByUser 故意返回他人实例。
	// handler 仍必须按归属过滤后再算 total/分页（L2 防御用例）。
	leakForeign bool
}

type resetCall struct {
	planID  int64
	advance bool
}

func newFakeSubscriptionRepo() *fakeSubscriptionRepo {
	return &fakeSubscriptionRepo{
		plans:         map[int64]*store.SubscriptionPlan{},
		subs:          map[int64]*store.Subscription{},
		pendingOrders: map[int64]int{},
		preConsume:    map[string]store.SubscriptionPreConsume{},
		events:        map[int64][]store.SubscriptionEvent{},
	}
}

func (r *fakeSubscriptionRepo) CreatePlan(_ context.Context, in store.SubscriptionPlanInput) (store.SubscriptionPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	for _, plan := range r.plans {
		if plan.PlanKey == strings.TrimSpace(in.PlanKey) {
			return store.SubscriptionPlan{}, store.ErrPlanDuplicate
		}
	}
	r.nextPlanID++
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC).Add(time.Duration(r.nextPlanID) * time.Second)
	plan := &store.SubscriptionPlan{
		ID: r.nextPlanID, PlanKey: strings.TrimSpace(in.PlanKey), Title: in.Title,
		Description: in.Description, PeriodDays: in.PeriodDays, PriceAmount: in.PriceAmount,
		Currency: in.Currency, GrantCredits: in.GrantCredits,
		ResetOnRenew: boolOr(in.ResetOnRenew, true), AllowWalletOverflow: boolOr(in.AllowWalletOverflow, true),
		ModelScope: in.ModelScope, ConcurrencyLimit: in.ConcurrencyLimit,
		GraceDays: in.GraceDays, Status: stringOr(in.Status, "enabled"),
		SortOrder: in.SortOrder, CreatedAt: now, UpdatedAt: now,
	}
	r.plans[plan.ID] = plan
	return *plan, nil
}

func boolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func stringOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func (r *fakeSubscriptionRepo) GetPlan(_ context.Context, id int64) (store.SubscriptionPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	plan, ok := r.plans[id]
	if !ok {
		return store.SubscriptionPlan{}, store.ErrPlanNotFound
	}
	return *plan, nil
}

func (r *fakeSubscriptionRepo) UpdatePlan(_ context.Context, id int64, in store.SubscriptionPlanUpdate) (store.SubscriptionPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	plan, ok := r.plans[id]
	if !ok {
		return store.SubscriptionPlan{}, store.ErrPlanNotFound
	}
	for otherID, other := range r.plans {
		if otherID != id && other.PlanKey == strings.TrimSpace(in.PlanKey) {
			return store.SubscriptionPlan{}, store.ErrPlanDuplicate
		}
	}
	plan.PlanKey = strings.TrimSpace(in.PlanKey)
	plan.Title = in.Title
	plan.Description = in.Description
	plan.PeriodDays = in.PeriodDays
	plan.PriceAmount = in.PriceAmount
	plan.Currency = in.Currency
	plan.GrantCredits = in.GrantCredits
	plan.ResetOnRenew = in.ResetOnRenew
	plan.AllowWalletOverflow = in.AllowWalletOverflow
	plan.ModelScope = in.ModelScope
	plan.ConcurrencyLimit = in.ConcurrencyLimit
	plan.GraceDays = in.GraceDays
	plan.Status = in.Status
	plan.SortOrder = in.SortOrder
	plan.UpdatedAt = plan.UpdatedAt.Add(time.Second)
	return *plan, nil
}

func (r *fakeSubscriptionRepo) ListPlans(_ context.Context, status string) ([]store.SubscriptionPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.SubscriptionPlan, 0, len(r.plans))
	for _, plan := range r.plans {
		if status != "" && plan.Status != status {
			continue
		}
		out = append(out, *plan)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (r *fakeSubscriptionRepo) EnablePlan(_ context.Context, id int64) error {
	return r.setStatus(id, "enabled")
}

func (r *fakeSubscriptionRepo) DisablePlan(_ context.Context, id int64) error {
	return r.setStatus(id, "disabled")
}

func (r *fakeSubscriptionRepo) setStatus(id int64, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	plan, ok := r.plans[id]
	if !ok {
		return store.ErrPlanNotFound
	}
	plan.Status = status
	return nil
}

func (r *fakeSubscriptionRepo) DeletePlan(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	if _, ok := r.plans[id]; !ok {
		return store.ErrPlanNotFound
	}
	instances := int64(0)
	for _, sub := range r.subs {
		if sub.PlanID == id {
			instances++
		}
	}
	orders := int64(r.pendingOrders[id])
	if instances+orders > 0 {
		return store.ErrPlanReferenced{ReferenceCount: instances + orders, OrderReferenceCount: orders}
	}
	delete(r.plans, id)
	return nil
}

func (r *fakeSubscriptionRepo) ResetPlanQuota(_ context.Context, planID int64, advance bool) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	if _, ok := r.plans[planID]; !ok {
		return 0, store.ErrPlanNotFound
	}
	r.resets = append(r.resets, resetCall{planID: planID, advance: advance})
	var affected int64
	for _, sub := range r.subs {
		if sub.PlanID != planID || sub.Status != "active" {
			continue
		}
		sub.AmountUsed = 0
		if advance && sub.NextResetAt != nil {
			next := sub.NextResetAt.Add(time.Duration(sub.PeriodDays) * 24 * time.Hour)
			sub.NextResetAt = &next
		}
		affected++
	}
	return affected, nil
}

func (r *fakeSubscriptionRepo) CreateSubscription(_ context.Context, in store.SubscriptionCreateInput) (store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	r.nextSubID++
	sub := &store.Subscription{
		ID: r.nextSubID, UserID: in.UserID, PlanID: in.PlanID,
		PlanTitleSnapshot: in.PlanTitleSnapshot, Status: "active",
		StartAt: in.StartAt, EndAt: in.EndAt, NextResetAt: in.NextResetAt,
		AmountTotal: in.AmountTotal, AmountUsed: in.AmountUsed,
		AllowWalletOverflow: in.AllowWalletOverflow, AutoRenew: in.AutoRenew,
		CancelAtPeriodEnd: in.CancelAtPeriodEnd, OrderID: in.OrderID,
		Currency: in.Currency, PricePaidAmount: in.PricePaidAmount,
		PeriodDays: in.PeriodDays, ResetOnRenew: in.ResetOnRenew,
		GraceDays: in.GraceDays, ModelScope: in.ModelScope,
		ConcurrencyLimit: in.ConcurrencyLimit,
	}
	r.subs[sub.ID] = sub
	return *sub, nil
}

func (r *fakeSubscriptionRepo) GetSubscription(_ context.Context, id int64) (store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.subs[id]
	if !ok {
		return store.Subscription{}, store.ErrSubscriptionNotFound
	}
	return *sub, nil
}

func (r *fakeSubscriptionRepo) ListSubscriptionEvents(_ context.Context,
	subscriptionID int64, afterSeq int64, limit int) ([]store.SubscriptionEvent, error) {

	r.mu.Lock()
	defer r.mu.Unlock()
	if subscriptionID <= 0 || afterSeq < 0 {
		return nil, store.ErrSubscriptionEventInvalid
	}
	out := make([]store.SubscriptionEvent, 0, len(r.events[subscriptionID]))
	for _, event := range r.events[subscriptionID] {
		if event.Seq <= afterSeq {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, event)
	}
	return out, nil
}

func (r *fakeSubscriptionRepo) ListSubscriptionsByUser(_ context.Context, userID int64) ([]store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Subscription, 0, len(r.subs))
	for _, sub := range r.subs {
		if r.leakForeign || sub.UserID == userID {
			out = append(out, *sub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (r *fakeSubscriptionRepo) ListActiveSubscriptions(_ context.Context, userID int64, now time.Time) ([]store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Subscription, 0)
	for _, sub := range r.subs {
		if sub.UserID == userID && sub.Status == "active" && sub.EndAt.After(now) {
			out = append(out, *sub)
		}
	}
	return out, nil
}

func (r *fakeSubscriptionRepo) HasActiveUserSubscription(ctx context.Context, userID int64, now time.Time) (bool, error) {
	subs, err := r.ListActiveSubscriptions(ctx, userID, now)
	return len(subs) > 0, err
}

func (r *fakeSubscriptionRepo) UserActiveSubscriptionsAllowWalletOverflow(ctx context.Context, userID int64, now time.Time) (bool, error) {
	subs, err := r.ListActiveSubscriptions(ctx, userID, now)
	if err != nil {
		return false, err
	}
	for _, sub := range subs {
		if sub.AllowWalletOverflow {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeSubscriptionRepo) CancelAtPeriodEnd(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.subs[id]
	if !ok {
		return store.ErrSubscriptionNotFound
	}
	if sub.Status != "active" {
		return store.ErrSubscriptionNotActive
	}
	sub.CancelAtPeriodEnd = true
	return nil
}

func (r *fakeSubscriptionRepo) CancelImmediately(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.subs[id]
	if !ok {
		return store.ErrSubscriptionNotFound
	}
	if sub.Status == "expired" {
		return store.ErrSubscriptionNotActive
	}
	// R-48：立即取消保留 active 并把 end_at 提前到 now，清掉周期末标记与
	// 自动续费开关（否则 S4 worker 会继续对已取消订阅扣款）。
	sub.CancelAtPeriodEnd = false
	sub.AutoRenew = false
	sub.EndAt = time.Now().UTC()
	return nil
}

func (r *fakeSubscriptionRepo) ExpireDue(_ context.Context, now time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var affected int64
	for _, sub := range r.subs {
		if sub.Status == "active" && !sub.EndAt.After(now) {
			sub.Status = "expired"
			affected++
		}
	}
	return affected, nil
}

func (r *fakeSubscriptionRepo) DueForReset(_ context.Context, now time.Time) ([]store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Subscription, 0)
	for _, sub := range r.subs {
		if sub.Status == "active" && sub.NextResetAt != nil && !sub.NextResetAt.After(now) {
			out = append(out, *sub)
		}
	}
	return out, nil
}

func (r *fakeSubscriptionRepo) ResetSubscriptionQuota(_ context.Context, id int64, _ time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.subs[id]
	if !ok || sub.Status != "active" || !sub.ResetOnRenew || sub.NextResetAt == nil {
		return 0, nil
	}
	sub.AmountUsed = 0
	return 1, nil
}

func (r *fakeSubscriptionRepo) InsertPreConsume(_ context.Context, requestID string, subscriptionID int64, amount int64) (store.SubscriptionPreConsume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeCallCount++
	rec := store.SubscriptionPreConsume{RequestID: requestID, SubscriptionID: subscriptionID, Status: "consumed", PreConsumed: amount}
	r.preConsume[requestID] = rec
	return rec, nil
}

func (r *fakeSubscriptionRepo) PreConsumeUserSubscription(_ context.Context, requestID string, subscriptionID int64, amount int64) (store.SubscriptionPreConsume, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.preConsume[requestID]; ok {
		if existing.Status == "refunded" {
			return store.SubscriptionPreConsume{}, false, store.ErrPreConsumeRefunded
		}
		return existing, false, nil
	}
	r.writeCallCount++
	rec := store.SubscriptionPreConsume{RequestID: requestID, SubscriptionID: subscriptionID, Status: "consumed", PreConsumed: amount}
	r.preConsume[requestID] = rec
	if sub, ok := r.subs[subscriptionID]; ok {
		sub.AmountUsed += amount
	}
	return rec, true, nil
}

func (r *fakeSubscriptionRepo) GetPreConsume(_ context.Context, requestID string) (store.SubscriptionPreConsume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.preConsume[requestID]
	if !ok {
		return store.SubscriptionPreConsume{}, store.ErrPreConsumeNotFound
	}
	return rec, nil
}

func (r *fakeSubscriptionRepo) MarkPreConsumeRefunded(_ context.Context, requestID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.preConsume[requestID]; !ok {
		return store.ErrPreConsumeNotFound
	}
	r.preConsume[requestID] = store.SubscriptionPreConsume{RequestID: requestID, Status: "refunded"}
	return nil
}

// PostConsumeUserSubscriptionDelta 是 S4b 结算增量的内存实现（handler
// 测试不覆盖结算语义，仅满足接口；真实语义由 store/billing 集成用例覆盖）。
func (r *fakeSubscriptionRepo) PostConsumeUserSubscriptionDelta(_ context.Context,
	subscriptionID, delta int64) (store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.subs[subscriptionID]
	if !ok {
		return store.Subscription{}, store.ErrSubscriptionNotFound
	}
	used := sub.AmountUsed + delta
	if used < 0 {
		used = 0
	}
	if sub.AmountTotal > 0 && used > sub.AmountTotal {
		return store.Subscription{}, store.ErrSubscriptionSettleExceeded
	}
	sub.AmountUsed = used
	return *sub, nil
}

// RefundSubscriptionIncrement 是 M3 守卫的内存实现：amount_used >= amount
// 才回退，否则返回 false（周期重置后不侵蚀新周期用量）。
func (r *fakeSubscriptionRepo) RefundSubscriptionIncrement(_ context.Context,
	subscriptionID, amount int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub, ok := r.subs[subscriptionID]
	if !ok {
		return false, store.ErrSubscriptionNotFound
	}
	if amount <= 0 || sub.AmountUsed < amount {
		return false, nil
	}
	sub.AmountUsed -= amount
	return true, nil
}

// ListStalePreConsumes 返回早于 before 的 consumed 记录（handler 测试的
// 假仓储不区分 registry 终态）。
func (r *fakeSubscriptionRepo) ListStalePreConsumes(_ context.Context,
	before time.Time, limit int) ([]store.SubscriptionPreConsume, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.SubscriptionPreConsume, 0, limit)
	for _, rec := range r.preConsume {
		if rec.Status == "consumed" && rec.CreatedAt.Before(before) {
			out = append(out, rec)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 测试装配
// ---------------------------------------------------------------------------

type subscriptionTestEnv struct {
	repo     *fakeSubscriptionRepo
	router   http.Handler
	rdb      *redis.Client
	mr       *miniredis.Miniredis
	audit    *subAuditExecer
	purchase *fakeSubscriptionPurchase
}

// subAuditExecer 捕获 audit_logs 的 INSERT 参数（列序见 audit.insertSQL）。
type subAuditExecer struct {
	mu   sync.Mutex
	args [][]any
}

func (e *subAuditExecer) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.args = append(e.args, append([]any(nil), args...))
	return subCaptureResult{}, nil
}

type subCaptureResult struct{}

func (subCaptureResult) LastInsertId() (int64, error) { return 0, nil }
func (subCaptureResult) RowsAffected() (int64, error) { return 1, nil }

func (e *subAuditExecer) last(t *testing.T) []any {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.args) == 0 {
		t.Fatal("no audit event captured")
	}
	return e.args[len(e.args)-1]
}

func newSubscriptionTestEnv(t *testing.T) *subscriptionTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := newFakeSubscriptionRepo()
	capture := &subAuditExecer{}
	purchase := &fakeSubscriptionPurchase{}
	// 这些用例不触碰 users 表；*sql.DB 零值只作为构造期非 nil 占位，
	// 让「DB 必填」校验仍然真实执行。
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: repo, DB: &sql.DB{}, Redis: rdb,
		Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(capture), Logger: testLogger(),
		Purchase: purchase,
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &subscriptionTestEnv{repo: repo, router: mux, rdb: rdb, mr: mr, audit: capture, purchase: purchase}
}

func (e *subscriptionTestEnv) do(t *testing.T, principal *auth.Principal, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
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

func adminFull() *auth.Principal {
	return &auth.Principal{UserID: 1, Role: rbac.RoleAdmin}
}

func adminRestricted(perms ...string) *auth.Principal {
	return &auth.Principal{UserID: 2, Role: rbac.RoleAdmin, Permissions: perms}
}

func endUser(id int64) *auth.Principal {
	return &auth.Principal{UserID: id, Role: rbac.RoleUser}
}

func validPlanBody(key string) map[string]any {
	return map[string]any{
		"plan_key": key, "title": "Pro Monthly", "description": " monthly plan ",
		"period_days": 30, "price_amount": 9900, "currency": "CNY",
		"grant_credits": 1000000, "model_scope": []string{"gpt-4o", "gpt-4o", "claude-3"},
		"sort_order": 10,
	}
}

// ---------------------------------------------------------------------------
// 权限
// ---------------------------------------------------------------------------

func TestSubscriptionEndpointPermissions(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	cases := []struct {
		name       string
		principal  *auth.Principal
		method     string
		path       string
		body       any
		wantStatus int
	}{
		{"admin full list", adminFull(), http.MethodGet, "/api/subscription-plans", nil, 200},
		{"restricted admin without settings:manage", adminRestricted(rbac.PermUsersRead), http.MethodGet, "/api/subscription-plans", nil, 403},
		{"end user cannot list plans", endUser(9), http.MethodGet, "/api/subscription-plans", nil, 403},
		{"unauthenticated（authz 层失败关闭）", nil, http.MethodGet, "/api/subscription-plans", nil, 403},
		{"end user can list available plans", endUser(9), http.MethodGet, "/api/subscription/plans", nil, 200},
		{"unauthenticated available plans（handler fail-closed）", nil, http.MethodGet, "/api/subscription/plans", nil, 401},
		{"end user cannot read another user's subscriptions via admin path", endUser(9), http.MethodGet, "/api/users/1/subscriptions", nil, 403},
		{"restricted admin with users:read can read user subscriptions", adminRestricted(rbac.PermUsersRead), http.MethodGet, "/api/users/1/subscriptions", nil, 200},
		{"admin without users:read cannot read user subscriptions", adminRestricted(rbac.PermSettingsManage), http.MethodGet, "/api/users/1/subscriptions", nil, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.do(t, tc.principal, tc.method, tc.path, tc.body, nil)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// CRUD / 校验 / 冲突 / 引用保护
// ---------------------------------------------------------------------------

func TestSubscriptionPlanCRUDAndValidation(t *testing.T) {
	env := newSubscriptionTestEnv(t)

	rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", validPlanBody("pro-monthly"), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var created subscriptionPlanView
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.PlanKey != "pro-monthly" || len(created.ModelScope) != 2 || created.ModelScope[0] != "gpt-4o" {
		t.Fatalf("created = %+v（model_scope 应去重且保序）", created)
	}
	if created.Title != "Pro Monthly" || created.Description == nil || *created.Description != "monthly plan" {
		t.Fatalf("created trim 口径错误: %+v", created)
	}

	t.Run("plan_key 冲突 409", func(t *testing.T) {
		rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", validPlanBody("pro-monthly"), nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("duplicate = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decodeSubProblemType(t, rr); got != problemTypePlanKeyConflict {
			t.Fatalf("type = %s", got)
		}
	})

	t.Run("缺失必填字段 400", func(t *testing.T) {
		body := validPlanBody("no-title")
		delete(body, "title")
		rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("missing = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	invalid := []struct {
		name  string
		key   string
		patch func(map[string]any)
		field string
	}{
		{"plan_key 大写字", "Bad-Key", nil, "plan_key"},
		{"title 超长", "v-title", func(b map[string]any) { b["title"] = strings.Repeat("x", 65) }, "title"},
		{"description 超长", "v-desc", func(b map[string]any) { b["description"] = strings.Repeat("x", 256) }, "description"},
		{"period_days 0", "v-period", func(b map[string]any) { b["period_days"] = 0 }, "period_days"},
		{"price 负数", "v-price", func(b map[string]any) { b["price_amount"] = -1 }, "price_amount"},
		{"currency 非 CNY", "v-currency", func(b map[string]any) { b["currency"] = "USD" }, "currency"},
		{"grant 负数", "v-grant", func(b map[string]any) { b["grant_credits"] = -1 }, "grant_credits"},
		{"grace 负数", "v-grace", func(b map[string]any) { b["grace_days"] = -1 }, "grace_days"},
		{"concurrency 0", "v-concurrency", func(b map[string]any) { b["concurrency_limit"] = 0 }, "concurrency_limit"},
		{"model_scope 空元素", "v-scope-empty", func(b map[string]any) { b["model_scope"] = []string{"a", " "} }, "model_scope"},
		{"model_scope 非数组", "v-scope-type", func(b map[string]any) { b["model_scope"] = map[string]any{"a": 1} }, "model_scope"},
		{"status 越界", "v-status", func(b map[string]any) { b["status"] = "paused" }, "status"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			body := validPlanBody(tc.key)
			if tc.patch != nil {
				tc.patch(body)
			}
			rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, nil)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
			}
			problem := decodeSubProblemFields(t, rr)
			if len(problem.Fields) == 0 || problem.Fields[0] != tc.field {
				t.Fatalf("fields = %v，期望首项 %s", problem.Fields, tc.field)
			}
		})
	}

	t.Run("类型错误 400", func(t *testing.T) {
		body := validPlanBody("v-type")
		body["period_days"] = "thirty"
		rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("未知字段 403 且零写入", func(t *testing.T) {
		before := env.repo.writeCallCount
		body := validPlanBody("unknown-field")
		body["price_amount_yuan"] = 99
		rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
		}
		if env.repo.writeCallCount != before {
			t.Fatalf("未知字段请求不得写库")
		}
	})

	t.Run("GET/PUT/404", func(t *testing.T) {
		rr := env.do(t, adminFull(), http.MethodGet, fmt.Sprintf("/api/subscription-plans/%d", created.ID), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("get = %d", rr.Code)
		}
		body := validPlanBody("pro-monthly-v2")
		body["title"] = "Pro Monthly v2"
		body["status"] = "disabled"
		rr = env.do(t, adminFull(), http.MethodPut, fmt.Sprintf("/api/subscription-plans/%d", created.ID), body, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("put = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var updated subscriptionPlanView
		_ = json.Unmarshal(rr.Body.Bytes(), &updated)
		if updated.PlanKey != "pro-monthly-v2" || updated.Status != "disabled" || updated.Title != "Pro Monthly v2" {
			t.Fatalf("updated = %+v", updated)
		}
		rr = env.do(t, adminFull(), http.MethodGet, "/api/subscription-plans/99999", nil, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("missing get = %d", rr.Code)
		}
		rr = env.do(t, adminFull(), http.MethodPut, "/api/subscription-plans/99999", validPlanBody("nope"), nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("missing put = %d", rr.Code)
		}
	})

	t.Run("启停幂等", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			rr := env.do(t, adminFull(), http.MethodPost, fmt.Sprintf("/api/subscription-plans/%d/disable", created.ID), nil, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("disable #%d = %d（body=%s）", i, rr.Code, rr.Body.String())
			}
		}
		rr := env.do(t, adminFull(), http.MethodPost, fmt.Sprintf("/api/subscription-plans/%d/enable", created.ID), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("enable = %d", rr.Code)
		}
	})

	// 未知字段在写动作上一律 403（字段白名单失败关闭），且请求不得触达 handler。
	t.Run("PUT/DELETE/启停的未知字段 403", func(t *testing.T) {
		writeCalls := env.repo.writeCallCount
		unknown := func() map[string]any { return map[string]any{"unexpected": true} }
		cases := []struct {
			name   string
			method string
			path   string
			body   map[string]any
		}{
			{"PUT", http.MethodPut, fmt.Sprintf("/api/subscription-plans/%d", created.ID), unknown()},
			{"DELETE", http.MethodDelete, fmt.Sprintf("/api/subscription-plans/%d", created.ID), unknown()},
			{"enable", http.MethodPost, fmt.Sprintf("/api/subscription-plans/%d/enable", created.ID), unknown()},
			{"disable", http.MethodPost, fmt.Sprintf("/api/subscription-plans/%d/disable", created.ID), unknown()},
		}
		for _, tc := range cases {
			rr := env.do(t, adminFull(), tc.method, tc.path, tc.body, nil)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s 未知字段 = %d，期望 403（body=%s）", tc.name, rr.Code, rr.Body.String())
			}
		}
		if env.repo.writeCallCount != writeCalls {
			t.Fatalf("被字段层拒绝的请求不得写库：%d → %d", writeCalls, env.repo.writeCallCount)
		}
	})
}

// ---------------------------------------------------------------------------
// 列表分页 / 关键词 / 状态
// ---------------------------------------------------------------------------

func TestSubscriptionPlanListPagination(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	for i := 0; i < 5; i++ {
		body := validPlanBody(fmt.Sprintf("plan-%c", 'a'+i))
		body["title"] = fmt.Sprintf("Plan %c", 'A'+i)
		body["sort_order"] = i
		if i%2 == 1 {
			body["status"] = "disabled"
		}
		if rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, nil); rr.Code != http.StatusCreated {
			t.Fatalf("seed create = %d", rr.Code)
		}
	}

	rr := env.do(t, adminFull(), http.MethodGet, "/api/subscription-plans?p=1&page_size=2", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d", rr.Code)
	}
	var page subscriptionPlanListView
	_ = json.Unmarshal(rr.Body.Bytes(), &page)
	if page.Total != 5 || len(page.Items) != 2 || page.Page != 1 || page.PageSize != 2 {
		t.Fatalf("page = %+v", page)
	}
	if page.Items[0].PlanKey != "plan-a" || page.Items[1].PlanKey != "plan-b" {
		t.Fatalf("排序应按 sort_order,id: %v", page.Items)
	}

	rr = env.do(t, adminFull(), http.MethodGet, "/api/subscription-plans?status=enabled", nil, nil)
	var enabled subscriptionPlanListView
	_ = json.Unmarshal(rr.Body.Bytes(), &enabled)
	if enabled.Total != 3 {
		t.Fatalf("enabled total = %d，期望 3", enabled.Total)
	}

	rr = env.do(t, adminFull(), http.MethodGet, "/api/subscription-plans?keyword=PLAN-C", nil, nil)
	var keyword subscriptionPlanListView
	_ = json.Unmarshal(rr.Body.Bytes(), &keyword)
	if keyword.Total != 1 || keyword.Items[0].PlanKey != "plan-c" {
		t.Fatalf("keyword 结果 = %+v", keyword)
	}

	rr = env.do(t, adminFull(), http.MethodGet, "/api/subscription-plans?status=paused", nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 status = %d，期望 400", rr.Code)
	}
	rr = env.do(t, adminFull(), http.MethodGet, "/api/subscription-plans?page_size=101", nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("超限 page_size = %d，期望 400", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 删除引用保护 / reset-quota
// ---------------------------------------------------------------------------

// TestResetQuotaOptionalBodyUsesContentNotContentLength 钉死 L4：
// body 是否存在的判据必须是读取后的实际内容，而不是 ContentLength。
// chunked/未知长度（-1）的空 body 与纯空白 body 都应视为「无 body」，
// 照常重置且 advance_next_reset 取默认 false。
func TestResetQuotaOptionalBodyUsesContentNotContentLength(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", validPlanBody("reset-empty"), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var plan subscriptionPlanView
	_ = json.Unmarshal(rr.Body.Bytes(), &plan)

	cases := []struct {
		name    string
		body    io.Reader
		length  int64
		wantAdv bool
	}{
		{"chunked/未知长度的空 body", http.NoBody, -1, false},
		{"纯空白 body", strings.NewReader("  \n\t "), 6, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(env.repo.resets)
			req := httptest.NewRequest(http.MethodPost,
				fmt.Sprintf("/api/subscription-plans/%d/reset-quota", plan.ID), tc.body)
			req.ContentLength = tc.length
			req = req.WithContext(auth.WithPrincipal(req.Context(), adminFull()))
			rr := serve(env.router, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
			}
			if len(env.repo.resets) != before+1 {
				t.Fatalf("reset 调用次数 = %d，期望 %d", len(env.repo.resets), before+1)
			}
			if got := env.repo.resets[len(env.repo.resets)-1]; got.advance != tc.wantAdv {
				t.Fatalf("advance = %v，期望 %v", got.advance, tc.wantAdv)
			}
		})
	}
}

func TestSubscriptionPlanReferenceProtectionAndReset(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", validPlanBody("guarded"), nil)
	var plan subscriptionPlanView
	_ = json.Unmarshal(rr.Body.Bytes(), &plan)

	env.repo.pendingOrders[plan.ID] = 1
	rr = env.do(t, adminFull(), http.MethodDelete, fmt.Sprintf("/api/subscription-plans/%d", plan.ID), nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("delete referenced = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var problem map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &problem)
	if problem["title"] != "plan_referenced" || problem["reference_count"] != float64(1) {
		t.Fatalf("problem = %v", problem)
	}

	// 实例引用（含终态）同样阻止删除；实例 + 订单计数相加。
	next := time.Now().UTC().Add(24 * time.Hour)
	if _, err := env.repo.CreateSubscription(context.Background(), store.SubscriptionCreateInput{
		UserID: 42, PlanID: plan.ID, PlanTitleSnapshot: "Guarded",
		StartAt: time.Now().UTC(), EndAt: next, AmountTotal: 100,
		Currency: "CNY", PeriodDays: 30, ResetOnRenew: true,
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	rr = env.do(t, adminFull(), http.MethodDelete, fmt.Sprintf("/api/subscription-plans/%d", plan.ID), nil, nil)
	_ = json.Unmarshal(rr.Body.Bytes(), &problem)
	if problem["reference_count"] != float64(2) {
		t.Fatalf("reference_count = %v，期望 2（实例+订单）", problem["reference_count"])
	}

	// reset-quota：只算活跃实例，advance 传给仓储且审计只记计数。
	rr = env.do(t, adminFull(), http.MethodPost, fmt.Sprintf("/api/subscription-plans/%d/reset-quota", plan.ID),
		map[string]any{"advance_next_reset": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var reset subscriptionResetQuotaView
	_ = json.Unmarshal(rr.Body.Bytes(), &reset)
	if reset.Affected != 1 {
		t.Fatalf("affected = %d，期望 1", reset.Affected)
	}
	if len(env.repo.resets) != 1 || !env.repo.resets[0].advance {
		t.Fatalf("resets = %+v", env.repo.resets)
	}

	// 审计 detail 不含整行（无 title/price/grant）；只允许 id/plan_key/count。
	// 用真实审计内容断言，而不是恒真的 writeCallCount==0 之类的计数检查。
	args := env.audit.last(t)
	raw, _ := args[9].([]byte)
	if raw == nil {
		if text, ok := args[9].(string); ok {
			raw = []byte(text)
		}
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("审计 detail 不是 JSON: %v（%v）", err, args[9])
	}
	if detail["action"] != audit.ActionSubscriptionPlanResetQuota || detail["affected"] != float64(1) {
		t.Fatalf("审计 detail = %v，期望 reset_quota/affected=1", detail)
	}
	for _, forbidden := range []string{"title", "price_amount", "grant_credits"} {
		if _, present := detail[forbidden]; present {
			t.Fatalf("审计 detail 不得含整行字段 %q: %v", forbidden, detail)
		}
	}
}

// TestSubscriptionPlanAuditHasNoSensitiveFields 验证审计 detail 只含标识/计数，
// 不把整行（标题、价格、额度）复制进 audit_logs。
func TestSubscriptionPlanAuditHasNoSensitiveFields(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	body := validPlanBody("audited-plan")
	rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d", rr.Code)
	}
	var plan subscriptionPlanView
	_ = json.Unmarshal(rr.Body.Bytes(), &plan)

	if rr := env.do(t, adminFull(), http.MethodPost,
		fmt.Sprintf("/api/subscription-plans/%d/reset-quota", plan.ID), map[string]any{"advance_next_reset": true}, nil); rr.Code != http.StatusOK {
		t.Fatalf("reset = %d", rr.Code)
	}

	args := env.audit.last(t)
	if category, _ := args[2].(string); category != "operation" {
		t.Fatalf("category = %v，期望 operation", args[2])
	}
	var detail map[string]any
	raw := ""
	switch value := args[9].(type) {
	case []byte:
		raw = string(value)
	case string:
		raw = value
	default:
		t.Fatalf("detail 类型 = %T", args[9])
	}
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		t.Fatalf("detail 不是 JSON: %v（%v）", err, args[9])
	}
	allowed := map[string]bool{
		"action": true, "id": true, "plan_key": true, "affected": true,
	}
	for key := range detail {
		if !allowed[key] {
			t.Fatalf("审计 detail 含敏感/整行字段 %q: %v", key, detail)
		}
	}
	if detail["action"] != audit.ActionSubscriptionPlanResetQuota {
		t.Fatalf("action = %v", detail["action"])
	}
}

// ---------------------------------------------------------------------------
// 用户隔离
// ---------------------------------------------------------------------------

func TestUserSubscriptionIsolation(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", validPlanBody("shared"), nil)
	var plan subscriptionPlanView
	_ = json.Unmarshal(rr.Body.Bytes(), &plan)

	base := time.Now().UTC()
	for _, userID := range []int64{7, 9} {
		_, err := env.repo.CreateSubscription(context.Background(), store.SubscriptionCreateInput{
			UserID: userID, PlanID: plan.ID, PlanTitleSnapshot: "Shared",
			StartAt: base, EndAt: base.Add(30 * 24 * time.Hour),
			AmountTotal: 100, AmountUsed: 10, Currency: "CNY",
			PeriodDays: 30, ResetOnRenew: true,
		})
		if err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
	}

	rr = env.do(t, endUser(7), http.MethodGet, "/api/subscriptions", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("my subscriptions = %d", rr.Code)
	}
	userRaw := rr.Body.String()
	var mine userSubscriptionListView
	_ = json.Unmarshal(rr.Body.Bytes(), &mine)
	if mine.Total != 1 || len(mine.Items) != 1 {
		t.Fatalf("用户 7 应只看到 1 条，得到 %+v", mine)
	}
	if mine.Items[0].AmountRemain == nil || *mine.Items[0].AmountRemain != 90 {
		t.Fatalf("amount_remain = %v", mine.Items[0].AmountRemain)
	}

	// 管理侧列表带 user_id，且严格按路径 id 过滤。
	rr = env.do(t, adminRestricted(rbac.PermUsersRead), http.MethodGet, "/api/users/9/subscriptions", nil, nil)
	var managed adminSubscriptionListView
	_ = json.Unmarshal(rr.Body.Bytes(), &managed)
	if managed.Total != 1 || managed.Items[0].UserID != 9 {
		t.Fatalf("managed = %+v", managed)
	}

	// 用户侧不能拿到管理侧投影（不得出现 user_id/order_id/价格/内部回退开关）。
	for _, forbidden := range []string{"order_id", "user_id", "price_paid_amount", "allow_wallet_overflow"} {
		if strings.Contains(userRaw, forbidden) {
			t.Fatalf("字段 %s 不应出现在用户响应: %s", forbidden, userRaw)
		}
	}
}

// TestUserSubscriptionAmountRemainPaginationAndOwnership 覆盖 D5b 收口项：
//   - M1：amount_total=0（无限额度）时 amount_remain 整个省略；有限额度时
//     必现且值为 total-used（最小 0）；
//   - 用户侧分页：total 是全集计数，各页 items 不重叠、不丢行；
//   - L2：仓储漂移（返回他人实例）时先按归属过滤再算 total/分页，
//     保证 total == len(items) 且绝不下发他人数据。
func TestUserSubscriptionAmountRemainPaginationAndOwnership(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	rr := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", validPlanBody("remain-plan"), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create plan = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var plan subscriptionPlanView
	_ = json.Unmarshal(rr.Body.Bytes(), &plan)

	base := time.Now().UTC()
	seed := func(userID, total, used int64) int64 {
		sub, err := env.repo.CreateSubscription(context.Background(), store.SubscriptionCreateInput{
			UserID: userID, PlanID: plan.ID, PlanTitleSnapshot: "Remain",
			StartAt: base, EndAt: base.Add(30 * 24 * time.Hour),
			AmountTotal: total, AmountUsed: used,
			Currency: "CNY", PeriodDays: 30, ResetOnRenew: true,
		})
		if err != nil {
			t.Fatalf("seed subscription: %v", err)
		}
		return sub.ID
	}
	unlimitedID := seed(7, 0, 0) // 无限额度：没有分母
	for i := 0; i < 4; i++ {
		seed(7, 100, 10)
	}
	foreignID := seed(9, 100, 10)

	rr = env.do(t, endUser(7), http.MethodGet, "/api/subscriptions?page_size=100", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("my subscriptions = %d", rr.Code)
	}
	var rawList struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &rawList); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if rawList.Total != 5 || len(rawList.Items) != 5 {
		t.Fatalf("total/items = %d/%d，期望 5/5", rawList.Total, len(rawList.Items))
	}
	sawUnlimited, sawLimited := false, false
	for _, item := range rawList.Items {
		if int64(item["id"].(float64)) == unlimitedID {
			sawUnlimited = true
			if _, present := item["amount_remain"]; present {
				t.Fatalf("amount_total=0 时 amount_remain 必须省略: %v", item)
			}
			continue
		}
		sawLimited = true
		remain, present := item["amount_remain"]
		if !present {
			t.Fatalf("有限额度必须返回 amount_remain: %v", item)
		}
		if remain != float64(90) {
			t.Fatalf("amount_remain = %v，期望 90（100-10）", remain)
		}
	}
	if !sawUnlimited || !sawLimited {
		t.Fatalf("用例未覆盖两种额度形态：unlimited=%v limited=%v", sawUnlimited, sawLimited)
	}

	// 分页：3 页（2+2+1），每页 total 恒为 5，行不重叠也不遗漏。
	seen := map[float64]bool{}
	for page := 1; page <= 3; page++ {
		rr = env.do(t, endUser(7), http.MethodGet,
			fmt.Sprintf("/api/subscriptions?p=%d&page_size=2", page), nil, nil)
		var view userSubscriptionListView
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode page %d: %v", page, err)
		}
		want := 2
		if page == 3 {
			want = 1
		}
		if view.Total != 5 || len(view.Items) != want {
			t.Fatalf("page %d total/items = %d/%d，期望 5/%d", page, view.Total, len(view.Items), want)
		}
		for _, item := range view.Items {
			if seen[float64(item.ID)] {
				t.Fatalf("分页出现重复行 id=%d", item.ID)
			}
			seen[float64(item.ID)] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("分页共返回 %d 行，期望 5", len(seen))
	}

	// L2 防御：仓储返回他人实例时，管理侧与用户侧都必须先过滤再计数。
	env.repo.leakForeign = true
	rr = env.do(t, adminRestricted(rbac.PermUsersRead), http.MethodGet,
		"/api/users/7/subscriptions?page_size=100", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin user list = %d", rr.Code)
	}
	var managed adminSubscriptionListView
	if err := json.Unmarshal(rr.Body.Bytes(), &managed); err != nil {
		t.Fatalf("decode managed: %v", err)
	}
	if managed.Total != int64(len(managed.Items)) || managed.Total != 5 {
		t.Fatalf("管理侧 total/items = %d/%d，期望 5/5（先过滤后计数）", managed.Total, len(managed.Items))
	}
	for _, item := range managed.Items {
		if item.UserID != 7 {
			t.Fatalf("管理侧泄漏他人实例: %+v", item)
		}
	}

	rr = env.do(t, endUser(7), http.MethodGet, "/api/subscriptions?page_size=100", nil, nil)
	var mine userSubscriptionListView
	if err := json.Unmarshal(rr.Body.Bytes(), &mine); err != nil {
		t.Fatalf("decode mine: %v", err)
	}
	if mine.Total != int64(len(mine.Items)) || mine.Total != 5 {
		t.Fatalf("用户侧 total/items = %d/%d，期望 5/5（先过滤后计数）", mine.Total, len(mine.Items))
	}
	for _, item := range mine.Items {
		if item.ID == foreignID {
			t.Fatalf("用户侧泄漏他人实例 id=%d", foreignID)
		}
	}
}

// ---------------------------------------------------------------------------
// 偏好归一 / 购买占位
// ---------------------------------------------------------------------------

func TestNormalizeBillingPreference(t *testing.T) {
	cases := map[string]string{
		"subscription_first": store.BillingPreferenceSubscriptionFirst,
		"wallet_first":       store.BillingPreferenceWalletFirst,
		"subscription_only":  store.BillingPreferenceSubscriptionOnly,
		"wallet_only":        store.BillingPreferenceWalletOnly,
		" wallet_first ":     store.BillingPreferenceWalletFirst,
		"":                   store.BillingPreferenceSubscriptionFirst,
		"unknown":            store.BillingPreferenceSubscriptionFirst,
		"WALLET_FIRST":       store.BillingPreferenceSubscriptionFirst,
	}
	for input, want := range cases {
		if got := normalizeBillingPreference(input); got != want {
			t.Errorf("normalize(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestPurchaseRequiresIdempotencyKeyAndFields(t *testing.T) {
	env := newSubscriptionTestEnv(t)

	// Idempotency-Key 缺失：服务端绝不能替客户端生成 key。
	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1, "provider": "stripe"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺少 Idempotency-Key = %d，期望 400（body=%s）", rr.Code, rr.Body.String())
	}
	if len(env.purchase.calls) != 0 {
		t.Fatal("被拒绝的请求不得调用支付服务")
	}

	headers := map[string]string{"Idempotency-Key": "purchase-1"}
	// 未知字段由 RBAC 字段层拒绝（403），不静默丢弃。
	rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1, "provider": "stripe", "coupon": "x"}, headers)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未知字段 = %d，期望 403（body=%s）", rr.Code, rr.Body.String())
	}

	// plan_id / provider 缺失 → 400，且不调用服务。
	for name, body := range map[string]map[string]any{
		"missing plan_id":  {"provider": "stripe"},
		"missing provider": {"plan_id": 1},
		"zero plan_id":     {"plan_id": 0, "provider": "stripe"},
	} {
		rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase", body, headers)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d，期望 400（body=%s）", name, rr.Code, rr.Body.String())
		}
	}
	if len(env.purchase.calls) != 0 {
		t.Fatalf("非法请求不得调用支付服务: %+v", env.purchase.calls)
	}

	rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1, "provider": "paypal"}, headers)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 provider = %d，期望 400", rr.Code)
	}
}

func TestPurchaseDelegatesToPaymentService(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 42, "provider": "Alipay"},
		map[string]string{"Idempotency-Key": "purchase-42"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("purchase = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if len(env.purchase.calls) != 1 {
		t.Fatalf("支付服务调用次数 = %d，期望 1", len(env.purchase.calls))
	}
	call := env.purchase.calls[0]
	if call.userID != 7 || call.planID != 42 || call.provider != "alipay" ||
		call.idempotencyKey != "purchase-42" {
		t.Fatalf("入参 = %+v", call)
	}
	var resp createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Order.ID != 10 || resp.Order.Provider != "alipay" {
		t.Fatalf("响应订单 = %+v", resp.Order)
	}
	// 金额/币种来自服务端订单，不取自请求体。
	if resp.Order.Amount != 9900 || resp.Order.Currency != "CNY" {
		t.Fatalf("订单金额/币种 = %d/%s", resp.Order.Amount, resp.Order.Currency)
	}
}

// TestPurchaseWalletProviderAndInsufficientBalance 覆盖 P2b：provider=wallet
// 被原样交给计费服务；余额不足（*billing.QuotaError）映射 402。
func TestPurchaseWalletProviderAndInsufficientBalance(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	env.purchase.result = &billing.TopupOrder{
		Order: &store.Order{ID: 11, UserID: 7, Provider: billing.ProviderWallet,
			Amount: 9900, Currency: "CNY", Status: "succeeded",
			BizKind: store.OrderBizKindSubscribe},
	}
	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 42, "provider": " Wallet "},
		map[string]string{"Idempotency-Key": "purchase-wallet"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("wallet purchase = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if len(env.purchase.calls) != 1 || env.purchase.calls[0].provider != billing.ProviderWallet {
		t.Fatalf("wallet 未原样交给计费服务: %+v", env.purchase.calls)
	}
	var resp createOrderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ClientSecret != "" || resp.QRCode != "" || resp.Order.Status != "succeeded" {
		t.Fatalf("余额购买响应 = %+v", resp)
	}

	env.purchase.err = &billing.QuotaError{
		Class: contract.ErrInsufficientQuota, Required: 9900, Available: 100,
	}
	rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 42, "provider": "wallet"},
		map[string]string{"Idempotency-Key": "purchase-wallet-short"})
	if rr.Code != http.StatusPaymentRequired {
		t.Fatalf("余额不足 = %d，期望 402（body=%s）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient_wallet_balance") {
		t.Fatalf("402 problem type 不符: %s", rr.Body.String())
	}
}

func TestPurchaseReplayReturns200(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	env.purchase.result = &billing.TopupOrder{
		Order: &store.Order{ID: 10, UserID: 7, Provider: "stripe", Amount: 9900,
			Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindSubscribe},
		Replayed: true,
	}
	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1, "provider": "stripe"},
		map[string]string{"Idempotency-Key": "purchase-replay"})
	if rr.Code != http.StatusOK {
		t.Fatalf("重放 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
}

func TestPurchaseErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"plan unavailable", billing.ErrSubscriptionPlanUnavailable, http.StatusConflict},
		{"idempotency conflict", billing.ErrIdempotencyConflict, http.StatusConflict},
		{"plan invalid", billing.ErrSubscriptionPlanInvalid, http.StatusUnprocessableEntity},
		{"currency", billing.ErrInvalidCurrency, http.StatusUnprocessableEntity},
		{"provider disabled", billing.ErrProviderDisabled, http.StatusServiceUnavailable},
		{"payment config", billing.ErrPaymentConfig, http.StatusServiceUnavailable},
		{"provider down", billing.ErrProviderUnavailable, http.StatusBadGateway},
		{"invalid provider", billing.ErrInvalidProvider, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newSubscriptionTestEnv(t)
			env.purchase.err = tc.err
			rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
				map[string]any{"plan_id": 1, "provider": "stripe"},
				map[string]string{"Idempotency-Key": "purchase-err"})
			if rr.Code != tc.status {
				t.Fatalf("%v = %d，期望 %d（body=%s）", tc.err, rr.Code, tc.status, rr.Body.String())
			}
		})
	}
}

func TestPurchaseWithoutPaymentServiceFailsClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: newFakeSubscriptionRepo(), DB: &sql.DB{}, Redis: rdb,
		Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(&subAuditExecer{}), Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/subscription/purchase",
		strings.NewReader(`{"plan_id":1,"provider":"stripe"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "nil-purchase")
	req = req.WithContext(auth.WithPrincipal(req.Context(), endUser(7)))
	rr := serve(mux, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配支付服务 = %d，期望 503（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 必须带 Retry-After")
	}
}

// TestPurchaseRateLimitedPerUserAndPlan 覆盖 S3 收口 M1：固定窗口
// 5 次/分钟，维度是「用户 × 套餐」。
func TestPurchaseRateLimitedPerUserAndPlan(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	body := map[string]any{"plan_id": 42, "provider": "stripe"}

	for i := 0; i < 5; i++ {
		rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase", body,
			map[string]string{"Idempotency-Key": fmt.Sprintf("rl-ok-%d", i)})
		if rr.Code != http.StatusCreated {
			t.Fatalf("第 %d 次下单 = %d（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	if len(env.purchase.calls) != 5 {
		t.Fatalf("限流前支付服务调用次数 = %d，期望 5", len(env.purchase.calls))
	}

	// 第 6 次：429 + Retry-After，且不调用支付服务。
	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase", body,
		map[string]string{"Idempotency-Key": "rl-blocked"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次下单 = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
	if len(env.purchase.calls) != 5 {
		t.Fatalf("超限请求不得调用支付服务，调用次数 = %d", len(env.purchase.calls))
	}

	// 换套餐：另一维度，不受影响。
	rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 43, "provider": "stripe"},
		map[string]string{"Idempotency-Key": "rl-other-plan"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("另一套餐 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	// 换用户：同一套餐的另一个维度，也不受影响。
	rr = env.do(t, endUser(8), http.MethodPost, "/api/subscription/purchase", body,
		map[string]string{"Idempotency-Key": "rl-other-user"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("另一用户 = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestPurchaseWalletRateLimited 证明 provider=wallet 仍走同一「用户 × 套餐」
// 固定窗口限流（P2b 冻结语义 1：余额购买不得绕过防重闸）。
func TestPurchaseWalletRateLimited(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	body := map[string]any{"plan_id": 77, "provider": billing.ProviderWallet}
	for i := 0; i < 5; i++ {
		rr := env.do(t, endUser(9), http.MethodPost, "/api/subscription/purchase", body,
			map[string]string{"Idempotency-Key": fmt.Sprintf("rl-wallet-%d", i)})
		if rr.Code != http.StatusCreated {
			t.Fatalf("第 %d 次余额购买 = %d（body=%s）", i+1, rr.Code, rr.Body.String())
		}
	}
	rr := env.do(t, endUser(9), http.MethodPost, "/api/subscription/purchase", body,
		map[string]string{"Idempotency-Key": "rl-wallet-blocked"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("第 6 次余额购买 = %d，期望 429（body=%s）", rr.Code, rr.Body.String())
	}
	if len(env.purchase.calls) != 5 {
		t.Fatalf("超限请求不得调用计费服务，调用次数 = %d", len(env.purchase.calls))
	}
}

// TestPurchaseRateLimiterFailsClosed 覆盖 S3 收口 M1：Redis 不可用时
// 无法证明配额，必须 503 fail-closed，而不是放行下单。
func TestPurchaseRateLimiterFailsClosed(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	env.mr.Close()

	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 42, "provider": "stripe"},
		map[string]string{"Idempotency-Key": "rl-redis-down"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis 故障 = %d，期望 503 fail-closed（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("fail-closed 503 必须带 Retry-After")
	}
	if len(env.purchase.calls) != 0 {
		t.Fatalf("Redis 故障时不得调用支付服务，调用次数 = %d", len(env.purchase.calls))
	}
}

// ---------------------------------------------------------------------------
// 创建幂等重放
// ---------------------------------------------------------------------------

func TestSubscriptionPlanCreateIdempotency(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	body := validPlanBody("idem-plan")
	headers := map[string]string{"Idempotency-Key": "create-1"}

	rr1 := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, headers)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first create = %d（body=%s）", rr1.Code, rr1.Body.String())
	}
	rr2 := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", body, headers)
	if rr2.Code != http.StatusCreated || rr2.Body.String() != rr1.Body.String() {
		t.Fatalf("replay = %d（body=%s），期望逐字重放", rr2.Code, rr2.Body.String())
	}

	different := validPlanBody("idem-plan-2")
	rr3 := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", different, headers)
	if rr3.Code != http.StatusUnprocessableEntity {
		t.Fatalf("同 key 不同 body = %d，期望 422", rr3.Code)
	}

	// 未知字段 + Idempotency-Key：字段层 403 必须在幂等层之前生效——
	// 既不能写库，也不能把 403 结果或进行中锁留在 Redis（否则同 key 的
	// 后续合法请求会被回放错误或被锁挡住）。
	bad := validPlanBody("idem-bad")
	bad["price_amount_yuan"] = 99
	badKey := "create-bad"
	writeCalls := env.repo.writeCallCount
	rr4 := env.do(t, adminFull(), http.MethodPost, "/api/subscription-plans", bad,
		map[string]string{"Idempotency-Key": badKey})
	if rr4.Code != http.StatusForbidden {
		t.Fatalf("未知字段 + Idempotency-Key = %d，期望 403（body=%s）", rr4.Code, rr4.Body.String())
	}
	cacheKey := fmt.Sprintf("newtg:idem:subscription_plan:create:%d:%s", int64(1), badKey)
	exists, err := env.rdb.Exists(context.Background(), cacheKey).Result()
	if err != nil {
		t.Fatalf("exists cache: %v", err)
	}
	if exists != 0 {
		t.Fatalf("403 响应不得进入幂等缓存（key=%s）", cacheKey)
	}
	lockExists, err := env.rdb.Exists(context.Background(), cacheKey+":lock").Result()
	if err != nil {
		t.Fatalf("exists lock: %v", err)
	}
	if lockExists != 0 {
		t.Fatalf("被拒绝的请求不得遗留幂等锁")
	}
	if env.repo.writeCallCount != writeCalls {
		t.Fatalf("被字段层拒绝的创建请求不得写库")
	}
}

// ---------------------------------------------------------------------------
// 用户周期末取消（billing-08 §15.2 P1）
// ---------------------------------------------------------------------------

// seedSubscription 直接写入假仓储，返回实例 ID。用于构造 active/终态两类样本。
func seedSubscription(t *testing.T, env *subscriptionTestEnv, userID int64, status string) int64 {
	t.Helper()
	now := time.Now().UTC()
	sub, err := env.repo.CreateSubscription(context.Background(), store.SubscriptionCreateInput{
		UserID: userID, PlanID: 1, PlanTitleSnapshot: "Pro",
		StartAt: now.Add(-time.Hour), EndAt: now.Add(29 * 24 * time.Hour),
		AmountTotal: 1000, PeriodDays: 30,
	})
	if err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	env.repo.mu.Lock()
	env.repo.subs[sub.ID].Status = status
	env.repo.mu.Unlock()
	return sub.ID
}

func TestSubscriptionRenewEndpoint(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	ownerID, otherID := int64(7), int64(8)
	activeID := seedSubscription(t, env, ownerID, "active")
	expiredID := seedSubscription(t, env, ownerID, "expired")
	cancelledID := seedSubscription(t, env, ownerID, "cancelled")
	foreignID := seedSubscription(t, env, otherID, "active")
	headers := func(key string) map[string]string { return map[string]string{"Idempotency-Key": key} }
	renewPath := func(id int64) string {
		return fmt.Sprintf("/api/subscriptions/%d/renew", id)
	}

	t.Run("Idempotency-Key 必带", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(activeID),
			map[string]any{"provider": "stripe"}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("缺少 Idempotency-Key = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if len(env.purchase.renewCalls) != 0 {
			t.Fatal("被拒绝的请求不得调用支付服务")
		}
	})

	t.Run("未知字段 403", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(activeID),
			map[string]any{"provider": "stripe", "amount": 1}, headers("renew-extra"))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("未知字段 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("provider 缺失或非法 400", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"missing provider": {},
			"invalid provider": {"provider": "paypal"},
			// wallet 只允许购买，不允许续费：续费仍是渠道支付单，没有
			// 余额扣款分支，接受它会产生无法履约的订单。
			"wallet provider": {"provider": "wallet"},
		} {
			rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(activeID), body, headers("renew-bad"))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d（body=%s）", name, rr.Code, rr.Body.String())
			}
		}
		if len(env.purchase.renewCalls) != 0 {
			t.Fatalf("非法请求不得调用支付服务: %+v", env.purchase.renewCalls)
		}
	})

	t.Run("他人实例 404", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(foreignID),
			map[string]any{"provider": "stripe"}, headers("renew-foreign"))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("他人实例 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if len(env.purchase.renewCalls) != 0 {
			t.Fatal("他人实例不得调用支付服务")
		}
	})

	t.Run("cancelled 409", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(cancelledID),
			map[string]any{"provider": "stripe"}, headers("renew-cancelled"))
		if rr.Code != http.StatusConflict {
			t.Fatalf("cancelled = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decodeSubProblemType(t, rr); got != problemTypeSubscriptionNotRenewable {
			t.Fatalf("type = %s", got)
		}
	})

	t.Run("active/expired 委托支付服务", func(t *testing.T) {
		for name, id := range map[string]int64{"active": activeID, "expired": expiredID} {
			rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(id),
				map[string]any{"provider": "Alipay"}, headers("renew-"+name))
			if rr.Code != http.StatusCreated {
				t.Fatalf("%s = %d（body=%s）", name, rr.Code, rr.Body.String())
			}
			call := env.purchase.renewCalls[len(env.purchase.renewCalls)-1]
			if call.userID != ownerID || call.subscriptionID != id || call.provider != "alipay" ||
				call.idempotencyKey != "renew-"+name {
				t.Fatalf("%s 入参 = %+v", name, call)
			}
		}
	})

	t.Run("重放 200", func(t *testing.T) {
		env.purchase.renewResult = &billing.TopupOrder{
			Order: &store.Order{ID: 20, UserID: ownerID, Provider: "stripe", Amount: 9900,
				Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindRenew},
			Replayed: true,
		}
		rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(activeID),
			map[string]any{"provider": "stripe"}, headers("renew-replay"))
		if rr.Code != http.StatusOK {
			t.Fatalf("重放 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("错误映射", func(t *testing.T) {
		cases := []struct {
			name   string
			err    error
			status int
		}{
			{"subscription missing", billing.ErrRenewalSubscriptionNotFound, http.StatusNotFound},
			{"not renewable", billing.ErrSubscriptionNotRenewable, http.StatusConflict},
			{"plan unavailable", billing.ErrSubscriptionPlanUnavailable, http.StatusConflict},
			{"idempotency conflict", billing.ErrIdempotencyConflict, http.StatusConflict},
			{"plan invalid", billing.ErrSubscriptionPlanInvalid, http.StatusUnprocessableEntity},
			{"currency", billing.ErrInvalidCurrency, http.StatusUnprocessableEntity},
			{"provider disabled", billing.ErrProviderDisabled, http.StatusServiceUnavailable},
			{"provider down", billing.ErrProviderUnavailable, http.StatusBadGateway},
			{"invalid idempotency", billing.ErrInvalidIdempotency, http.StatusBadRequest},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env.purchase.renewResult = nil
				env.purchase.renewErr = tc.err
				rr := env.do(t, endUser(ownerID), http.MethodPost, renewPath(activeID),
					map[string]any{"provider": "stripe"}, headers("renew-err"))
				if rr.Code != tc.status {
					t.Fatalf("%v = %d，期望 %d（body=%s）", tc.err, rr.Code, tc.status, rr.Body.String())
				}
			})
		}
	})
}

func TestSubscriptionRenewWithoutPaymentServiceFailsClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := newFakeSubscriptionRepo()
	repo.subs[1] = &store.Subscription{
		ID: 1, UserID: 7, PlanID: 1, Status: "active",
		StartAt: time.Now().UTC().Add(-time.Hour),
		EndAt:   time.Now().UTC().Add(time.Hour),
	}
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: repo, DB: &sql.DB{}, Redis: rdb,
		Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(&subAuditExecer{}), Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/subscriptions/1/renew",
		strings.NewReader(`{"provider":"stripe"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "nil-renew")
	req = req.WithContext(auth.WithPrincipal(req.Context(), endUser(7)))
	rr := serve(mux, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配支付服务 = %d，期望 503（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 必须带 Retry-After")
	}
}

// TestSubscriptionChangeEndpoint 覆盖 S2 升降级端点的请求形态、归属/状态守卫
// 与错误映射。折算公式与事务语义由 billing 的集成测试覆盖，这里只钉住
// handler 的契约（入参归一、幂等键、problem type）。
func TestSubscriptionChangeEndpoint(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	ownerID, otherID := int64(7), int64(8)
	activeID := seedSubscription(t, env, ownerID, "active")
	cancelledID := seedSubscription(t, env, ownerID, "cancelled")
	foreignID := seedSubscription(t, env, otherID, "active")
	headers := func(key string) map[string]string { return map[string]string{"Idempotency-Key": key} }
	changePath := func(id int64) string {
		return fmt.Sprintf("/api/subscriptions/%d/change", id)
	}
	body := func(planID int64, provider string) map[string]any {
		out := map[string]any{"plan_id": planID}
		if provider != "" {
			out["provider"] = provider
		}
		return out
	}

	t.Run("Idempotency-Key 必带", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID),
			body(2, "stripe"), nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("缺少 Idempotency-Key = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if len(env.purchase.changeCalls) != 0 {
			t.Fatal("被拒绝的请求不得调用支付服务")
		}
	})

	t.Run("未知字段 403", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID),
			map[string]any{"plan_id": 2, "provider": "stripe", "price_delta": 1}, headers("change-extra"))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("未知字段 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("plan_id/provider 缺失或非法 400", func(t *testing.T) {
		cases := map[string]map[string]any{
			"missing plan_id":  {"provider": "stripe"},
			"zero plan_id":     {"plan_id": 0, "provider": "stripe"},
			"missing provider": {"plan_id": 2},
			"invalid provider": {"plan_id": 2, "provider": "paypal"},
			// wallet 是余额购买专属，升降级没有余额扣款分支。
			"wallet provider": {"plan_id": 2, "provider": "wallet"},
		}
		for name, reqBody := range cases {
			rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID), reqBody,
				headers("change-bad"))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d（body=%s）", name, rr.Code, rr.Body.String())
			}
		}
		if len(env.purchase.changeCalls) != 0 {
			t.Fatalf("非法请求不得调用支付服务: %+v", env.purchase.changeCalls)
		}
	})

	t.Run("他人实例 404", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(foreignID),
			body(2, "stripe"), headers("change-foreign"))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("他人实例 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if len(env.purchase.changeCalls) != 0 {
			t.Fatal("他人实例不得调用支付服务")
		}
	})

	t.Run("cancelled 409", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(cancelledID),
			body(2, "stripe"), headers("change-cancelled"))
		if rr.Code != http.StatusConflict {
			t.Fatalf("cancelled = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decodeSubProblemType(t, rr); got != problemTypeSubscriptionNotChangeable {
			t.Fatalf("type = %s", got)
		}
		if len(env.purchase.changeCalls) != 0 {
			t.Fatal("终态实例不得调用支付服务")
		}
	})

	t.Run("active 委托支付服务并回显折算结果", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID),
			body(5, "Alipay"), headers("change-ok"))
		if rr.Code != http.StatusCreated {
			t.Fatalf("active = %d（body=%s）", rr.Code, rr.Body.String())
		}
		call := env.purchase.changeCalls[len(env.purchase.changeCalls)-1]
		if call.UserID != ownerID || call.SubscriptionID != activeID || call.NewPlanID != 5 ||
			call.Provider != "alipay" || call.IdempotencyKey != "change-ok" {
			t.Fatalf("入参 = %+v", call)
		}
		var view subscriptionChangeView
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode change view: %v", err)
		}
		if view.ToPlanID != 5 || view.FromPlanID != 1 || view.EventKind != store.SubscriptionEventUpgraded ||
			view.PriceDelta != 14950 || view.CreditsDelta != 4000 {
			t.Fatalf("响应 = %+v", view)
		}
	})

	t.Run("重放 200", func(t *testing.T) {
		replay := billing.ChangePlanResult{
			OrderID: 30, Order: &store.Order{ID: 30, UserID: ownerID, Provider: "stripe",
				Amount: 14950, Currency: "CNY", Status: "pending", BizKind: store.OrderBizKindChange},
			Replayed: true, PriceDelta: 14950, EventKind: store.SubscriptionEventUpgraded,
			FromPlanID: 1, NewPlanID: 2,
		}
		env.purchase.changeResult = &replay
		t.Cleanup(func() { env.purchase.changeResult = nil })
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID),
			body(2, "stripe"), headers("change-replay"))
		if rr.Code != http.StatusOK {
			t.Fatalf("重放 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("错误映射", func(t *testing.T) {
		cases := []struct {
			name   string
			err    error
			status int
		}{
			{"subscription missing", billing.ErrChangeSubscriptionNotFound, http.StatusNotFound},
			{"not changeable", billing.ErrSubscriptionNotChangeable, http.StatusConflict},
			{"plan unavailable", billing.ErrSubscriptionPlanUnavailable, http.StatusConflict},
			{"idempotency conflict", billing.ErrIdempotencyConflict, http.StatusConflict},
			{"pending change order", billing.ErrChangeOrderPending, http.StatusConflict},
			{"plan unchanged", billing.ErrChangePlanUnchanged, http.StatusUnprocessableEntity},
			{"cycle mismatch", billing.ErrSubscriptionCycleMismatch, http.StatusUnprocessableEntity},
			{"plan invalid", billing.ErrSubscriptionPlanInvalid, http.StatusUnprocessableEntity},
			{"provider disabled", billing.ErrProviderDisabled, http.StatusServiceUnavailable},
			{"provider down", billing.ErrProviderUnavailable, http.StatusBadGateway},
			{"invalid idempotency", billing.ErrInvalidIdempotency, http.StatusBadRequest},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				env.purchase.changeResult = nil
				env.purchase.changeErr = tc.err
				t.Cleanup(func() { env.purchase.changeErr = nil })
				rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID),
					body(2, "stripe"), headers("change-err"))
				if rr.Code != tc.status {
					t.Fatalf("%v = %d，期望 %d（body=%s）", tc.err, rr.Code, tc.status, rr.Body.String())
				}
			})
		}
	})

	t.Run("跨周期 422 用独立 problem type", func(t *testing.T) {
		env.purchase.changeResult = nil
		env.purchase.changeErr = billing.ErrSubscriptionCycleMismatch
		t.Cleanup(func() { env.purchase.changeErr = nil })
		rr := env.do(t, endUser(ownerID), http.MethodPost, changePath(activeID),
			body(2, "stripe"), headers("change-cycle"))
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("跨周期 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		if got := decodeSubProblemType(t, rr); got != problemTypeCycleMismatch {
			t.Fatalf("type = %s，期望 %s", got, problemTypeCycleMismatch)
		}
	})
}

func TestSubscriptionCancelAtPeriodEnd(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	ownerID, otherID := int64(7), int64(8)
	activeID := seedSubscription(t, env, ownerID, "active")
	foreignID := seedSubscription(t, env, otherID, "active")
	expiredID := seedSubscription(t, env, ownerID, "expired")
	cancelledID := seedSubscription(t, env, ownerID, "cancelled")

	t.Run("本人取消成功且幂等", func(t *testing.T) {
		for attempt := 1; attempt <= 2; attempt++ {
			rr := env.do(t, endUser(ownerID), http.MethodPost,
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
			// 用户侧投影不得带管理面/订单/价格字段（§15.4）。
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

		args := env.audit.last(t)
		if category, _ := args[2].(string); category != "operation" {
			t.Fatalf("audit category = %v，期望 operation", args[2])
		}
		detail := decodeSubAuditDetail(t, args[9])
		if detail["action"] != audit.ActionSubscriptionCancel {
			t.Fatalf("audit action = %v", detail["action"])
		}
		if detail["id"] != float64(activeID) || len(detail) != 2 {
			t.Fatalf("audit detail 只应含 action/id: %v", detail)
		}
	})

	t.Run("他人实例统一 404 且不改状态", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/cancel", foreignID), nil, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("他人实例 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		env.repo.mu.Lock()
		defer env.repo.mu.Unlock()
		if env.repo.subs[foreignID].CancelAtPeriodEnd {
			t.Fatal("他人实例不应被置位")
		}
	})

	t.Run("终态 409", func(t *testing.T) {
		for name, id := range map[string]int64{"expired": expiredID, "cancelled": cancelledID} {
			rr := env.do(t, endUser(ownerID), http.MethodPost,
				fmt.Sprintf("/api/subscriptions/%d/cancel", id), nil, nil)
			if rr.Code != http.StatusConflict {
				t.Fatalf("%s = %d（body=%s）", name, rr.Code, rr.Body.String())
			}
			if got := decodeSubProblemType(t, rr); got != problemTypeSubscriptionNotActive {
				t.Fatalf("%s type = %s", name, got)
			}
		}
	})

	t.Run("未登录 403", func(t *testing.T) {
		rr := env.do(t, nil, http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/cancel", activeID), nil, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("未登录 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("不存在/非法 id 404", func(t *testing.T) {
		for _, path := range []string{"/api/subscriptions/999999/cancel", "/api/subscriptions/not-a-number/cancel"} {
			rr := env.do(t, endUser(ownerID), http.MethodPost, path, nil, nil)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s = %d（body=%s）", path, rr.Code, rr.Body.String())
			}
		}
	})

	t.Run("携带字段 403 field_not_writable", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost,
			fmt.Sprintf("/api/subscriptions/%d/cancel", activeID), map[string]any{"cancel_at_period_end": false}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("带 body = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

// TestAdminCancelUserSubscription 覆盖 C3/R-08/R-47/R-48：管理侧取消端点。
//
//   - 权限：users:update（用户管理写入口）；settings:manage / 只读 / 普通用户 403。
//   - 归属：路径用户与实例归属不一致、实例不存在、非法 id 一律 404（不可区分）。
//   - immediate=true：R-48 的立即取消（end_at 提前、保留 active/宽限）；
//     false/缺省：与用户侧周期末取消同语义。
//   - 终态 409；重复调用幂等 200。
func TestAdminCancelUserSubscription(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	ownerID, otherID := int64(7), int64(8)
	activeID := seedSubscription(t, env, ownerID, "active")
	foreignID := seedSubscription(t, env, otherID, "active")
	expiredID := seedSubscription(t, env, ownerID, "expired")
	cancelledID := seedSubscription(t, env, ownerID, "cancelled")

	path := func(userID, subID int64) string {
		return fmt.Sprintf("/api/users/%d/subscriptions/%d/cancel", userID, subID)
	}

	t.Run("权限：users:update 通过，其它管理键与普通用户 403", func(t *testing.T) {
		cases := []struct {
			name       string
			principal  *auth.Principal
			wantStatus int
		}{
			{"users:update", adminRestricted(rbac.PermUsersUpdate), http.StatusOK},
			{"settings:manage", adminRestricted(rbac.PermSettingsManage), http.StatusForbidden},
			{"users:read only", adminRestricted(rbac.PermUsersRead), http.StatusForbidden},
			{"end user", endUser(ownerID), http.StatusForbidden},
			{"unauthenticated", nil, http.StatusForbidden},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rr := env.do(t, tc.principal, http.MethodPost, path(ownerID, activeID),
					map[string]any{"immediate": true}, nil)
				if rr.Code != tc.wantStatus {
					t.Fatalf("status = %d，期望 %d（body=%s）", rr.Code, tc.wantStatus, rr.Body.String())
				}
			})
		}
	})

	t.Run("立即取消保留 active 且提前 end_at", func(t *testing.T) {
		// 先置周期末取消与自动续费，验证立即取消会把两者都清掉（后者是
		// S4 worker 不再扣款的根因）。
		env.repo.mu.Lock()
		env.repo.subs[activeID].CancelAtPeriodEnd = true
		env.repo.subs[activeID].AutoRenew = true
		env.repo.mu.Unlock()

		rr := env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost,
			path(ownerID, activeID), map[string]any{"immediate": true}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("立即取消 = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var view adminSubscriptionView
		if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode subscription: %v", err)
		}
		if view.Status != "active" || view.CancelAtPeriodEnd {
			t.Fatalf("立即取消后视图 = %+v，want active 且 cancel_at_period_end=false", view)
		}
		if view.AutoRenew {
			t.Fatal("立即取消后 auto_renew 仍为 true：S4 worker 会继续扣款")
		}
		if view.EndAt.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("立即取消后 end_at = %v，期望提前到 now", view.EndAt)
		}
		// 管理投影必须带 user_id（管理对账口径）。
		if view.UserID != ownerID {
			t.Fatalf("admin 视图 user_id = %d，want %d", view.UserID, ownerID)
		}

		args := env.audit.last(t)
		detail := decodeSubAuditDetail(t, args[9])
		if detail["action"] != audit.ActionSubscriptionCancel ||
			detail["id"] != float64(activeID) || detail["immediate"] != true {
			t.Fatalf("audit detail = %v", detail)
		}

		// 重复调用幂等 200，不再改状态。
		rr = env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost,
			path(ownerID, activeID), map[string]any{"immediate": true}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("重复立即取消 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("immediate=false/缺省走周期末取消", func(t *testing.T) {
		id := seedSubscription(t, env, ownerID, "active")
		for _, body := range []any{nil, map[string]any{"immediate": false}} {
			rr := env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost,
				path(ownerID, id), body, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("周期末取消 = %d（body=%s）", rr.Code, rr.Body.String())
			}
			var view adminSubscriptionView
			if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
				t.Fatalf("decode subscription: %v", err)
			}
			if view.Status != "active" || !view.CancelAtPeriodEnd {
				t.Fatalf("周期末取消后视图 = %+v，want active + flag", view)
			}
		}
	})

	t.Run("他人实例与不存在统一 404，且不改他人状态", func(t *testing.T) {
		var bodies [][]byte
		for _, target := range []string{path(ownerID, foreignID), path(ownerID, 999999), path(ownerID, 0)} {
			rr := env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost, target,
				map[string]any{"immediate": true}, nil)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("%s = %d（body=%s）", target, rr.Code, rr.Body.String())
			}
			bodies = append(bodies, rr.Body.Bytes())
		}
		// 逐字节相等：他人订阅/不存在/非法 id 三类失败的响应体必须完全
		// 一致，不能靠「都是 404」之外的细节泄露归属。
		for index := 1; index < len(bodies); index++ {
			if string(bodies[index]) != string(bodies[0]) {
				t.Fatalf("404 响应体不一致：\n%s\nvs\n%s", bodies[0], bodies[index])
			}
		}
		env.repo.mu.Lock()
		defer env.repo.mu.Unlock()
		if env.repo.subs[foreignID].CancelAtPeriodEnd {
			t.Fatal("他人实例不应被置位")
		}
	})

	t.Run("终态 409", func(t *testing.T) {
		for name, id := range map[string]int64{"expired": expiredID, "cancelled-period-end": cancelledID} {
			rr := env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost,
				path(ownerID, id), map[string]any{"immediate": false}, nil)
			if rr.Code != http.StatusConflict {
				t.Fatalf("%s = %d（body=%s）", name, rr.Code, rr.Body.String())
			}
			if got := decodeSubProblemType(t, rr); got != problemTypeSubscriptionNotActive {
				t.Fatalf("%s type = %s", name, got)
			}
		}
		// immediate=true 对 expired 同样是 409。
		rr := env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost,
			path(ownerID, expiredID), map[string]any{"immediate": true}, nil)
		if rr.Code != http.StatusConflict {
			t.Fatalf("expired 立即取消 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("未知字段 403 field_not_writable", func(t *testing.T) {
		rr := env.do(t, adminRestricted(rbac.PermUsersUpdate), http.MethodPost,
			path(ownerID, activeID), map[string]any{"end_at": "2020-01-01T00:00:00Z"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("未知字段 = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

type ioReader = interface {
	Read(p []byte) (int, error)
}

func decodeSubProblemType(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return problem.Type
}

// decodeSubAuditDetail 解出审计 INSERT 的 detail 参数（jsonb 以 []byte/string 传入）。
func decodeSubAuditDetail(t *testing.T, value any) map[string]any {
	t.Helper()
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		t.Fatalf("detail 类型 = %T", value)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("detail 不是 JSON: %v（%s）", err, raw)
	}
	return detail
}

func decodeSubProblemFields(t *testing.T, rr *httptest.ResponseRecorder) struct {
	Fields []string `json:"fields"`
} {
	t.Helper()
	var problem struct {
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode problem fields: %v", err)
	}
	return problem
}

// ---------------------------------------------------------------------------
// S1：状态迁移事件流读取
// ---------------------------------------------------------------------------

// TestSubscriptionEventsVisibilityAndShape 验证 R-19：本人或 users:read 可见，
// 他人订阅 404（不是 403），after_seq 增量过滤，缺失身份 fail-closed。
func TestSubscriptionEventsVisibilityAndShape(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	ownerID, otherID := int64(41), int64(42)
	subscriptionID := int64(7)
	env.repo.mu.Lock()
	env.repo.subs[subscriptionID] = &store.Subscription{
		ID: subscriptionID, UserID: ownerID, PlanID: 1, Status: "active",
	}
	env.repo.events[subscriptionID] = []store.SubscriptionEvent{
		{SubscriptionID: subscriptionID, Seq: 1, Kind: store.SubscriptionEventCreated,
			Detail: []byte(`{"plan_id":1,"amount_total":1000}`), CreatedAt: time.Unix(1000, 0).UTC()},
		{SubscriptionID: subscriptionID, Seq: 2, Kind: store.SubscriptionEventCancelled,
			Detail: []byte(`{"immediate":false}`), CreatedAt: time.Unix(2000, 0).UTC()},
	}
	env.repo.mu.Unlock()

	t.Run("本人可见", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events", subscriptionID), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var body subscriptionEventListView
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 2 || body.Events[0].Seq != 1 ||
			body.Events[1].Kind != store.SubscriptionEventCancelled {
			t.Fatalf("events = %+v", body.Events)
		}
		if string(body.Events[0].Detail) != `{"plan_id":1,"amount_total":1000}` {
			t.Fatalf("detail 透传失真: %s", body.Events[0].Detail)
		}
	})

	t.Run("after_seq 增量", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events?after_seq=1", subscriptionID), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var body subscriptionEventListView
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Events) != 1 || body.Events[0].Seq != 2 {
			t.Fatalf("after_seq=1 events = %+v", body.Events)
		}
	})

	t.Run("他人订阅 404 而非 403", func(t *testing.T) {
		rr := env.do(t, endUser(otherID), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events", subscriptionID), nil, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d，期望 404（不得泄露 ID 存在性，body=%s）",
				rr.Code, rr.Body.String())
		}
	})

	t.Run("users:read 管理员可读他人订阅", func(t *testing.T) {
		rr := env.do(t, adminRestricted(rbac.PermUsersRead), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events", subscriptionID), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("无 users:read 的管理员按普通用户口径 404", func(t *testing.T) {
		rr := env.do(t, adminRestricted(rbac.PermSettingsManage), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events", subscriptionID), nil, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d，期望 404", rr.Code)
		}
	})

	t.Run("不存在与非法 after_seq", func(t *testing.T) {
		if rr := env.do(t, endUser(ownerID), http.MethodGet,
			"/api/subscriptions/999/events", nil, nil); rr.Code != http.StatusNotFound {
			t.Fatalf("不存在订阅 = %d", rr.Code)
		}
		if rr := env.do(t, endUser(ownerID), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events?after_seq=-1", subscriptionID), nil, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("负数 after_seq = %d，期望 400", rr.Code)
		}
		if rr := env.do(t, endUser(ownerID), http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events?after_seq=abc", subscriptionID), nil, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("非数字 after_seq = %d，期望 400", rr.Code)
		}
	})

	t.Run("未认证 fail-closed", func(t *testing.T) {
		rr := env.do(t, nil, http.MethodGet,
			fmt.Sprintf("/api/subscriptions/%d/events", subscriptionID), nil, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("未认证 = %d，期望 401", rr.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// S3：用户侧退款申请（只写请求，不直接退款）
// ---------------------------------------------------------------------------

// TestRequestSubscriptionRefundHandler 验证端点契约：本人可申请、他人 404、
// 缺失 reason 400、P-13 冲突映射 422、未装配支付服务 fail closed。
func TestRequestSubscriptionRefundHandler(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	ownerID, otherID, subscriptionID := int64(51), int64(52), int64(9)
	env.repo.mu.Lock()
	env.repo.subs[subscriptionID] = &store.Subscription{
		ID: subscriptionID, UserID: ownerID, PlanID: 1, Status: "active",
	}
	env.repo.mu.Unlock()
	path := fmt.Sprintf("/api/subscriptions/%d/refund-request", subscriptionID)

	t.Run("本人申请成功且只写请求", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, path,
			map[string]any{"reason": "买错了套餐"}, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
		}
		var body subscriptionRefundRequestView
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.OrderID != 10 || body.Reason != "买错了套餐" || body.RequestedAt.IsZero() {
			t.Fatalf("body = %+v", body)
		}
		if len(env.purchase.refundCalls) != 1 ||
			env.purchase.refundCalls[0].subscriptionID != subscriptionID ||
			env.purchase.refundCalls[0].userID != ownerID {
			t.Fatalf("refund calls = %+v", env.purchase.refundCalls)
		}
	})

	t.Run("他人实例统一 404 且不调用服务", func(t *testing.T) {
		before := len(env.purchase.refundCalls)
		rr := env.do(t, endUser(otherID), http.MethodPost, path,
			map[string]any{"reason": "想退款"}, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d，期望 404", rr.Code)
		}
		if len(env.purchase.refundCalls) != before {
			t.Fatalf("越权请求不应到达退款服务: %+v", env.purchase.refundCalls)
		}
	})

	t.Run("缺失 reason 400", func(t *testing.T) {
		rr := env.do(t, endUser(ownerID), http.MethodPost, path, map[string]any{}, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400", rr.Code)
		}
	})

	t.Run("P-13 冲突映射 422", func(t *testing.T) {
		env.purchase.refundErr = billing.ErrSubscriptionInUse
		defer func() { env.purchase.refundErr = nil }()
		rr := env.do(t, endUser(ownerID), http.MethodPost, path,
			map[string]any{"reason": "已开始使用"}, nil)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
		}
	})

	t.Run("未认证 fail-closed", func(t *testing.T) {
		rr := env.do(t, nil, http.MethodPost, path, map[string]any{"reason": "x"}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d，期望 403（authz 中间件 fail closed）", rr.Code)
		}
	})
}
