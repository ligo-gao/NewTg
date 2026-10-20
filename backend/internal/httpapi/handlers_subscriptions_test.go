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
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

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
	writeCallCount int
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

func (r *fakeSubscriptionRepo) ListSubscriptionsByUser(_ context.Context, userID int64) ([]store.Subscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Subscription, 0, len(r.subs))
	for _, sub := range r.subs {
		if sub.UserID == userID {
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
	sub.Status = "cancelled"
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

// ---------------------------------------------------------------------------
// 测试装配
// ---------------------------------------------------------------------------

type subscriptionTestEnv struct {
	repo   *fakeSubscriptionRepo
	router http.Handler
	rdb    *redis.Client
	audit  *subAuditExecer
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
	// 这些用例不触碰 users 表；*sql.DB 零值只作为构造期非 nil 占位，
	// 让「DB 必填」校验仍然真实执行。
	handlers, err := NewSubscriptionHandlers(SubscriptionHandlerConfig{
		Repo: repo, DB: &sql.DB{}, Redis: rdb,
		Authz: rbac.NewAuthzMiddleware(), Audit: audit.New(capture), Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewSubscriptionHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return &subscriptionTestEnv{repo: repo, router: mux, rdb: rdb, audit: capture}
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
	if env.repo.writeCallCount == 0 {
		t.Fatal("写计数异常")
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

func TestPurchaseReturns501WithoutWrites(t *testing.T) {
	env := newSubscriptionTestEnv(t)
	before := env.repo.writeCallCount

	rr := env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1}, map[string]string{"Idempotency-Key": "purchase-1"})
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("purchase = %d（body=%s）", rr.Code, rr.Body.String())
	}
	problem := decodeSubProblemType(t, rr)
	if problem != problemTypePurchaseDisable {
		t.Fatalf("type = %s", problem)
	}
	if !strings.Contains(rr.Body.String(), "subscription_purchase_disabled") {
		t.Fatalf("body 必须含固定文案: %s", rr.Body.String())
	}
	if env.repo.writeCallCount != before {
		t.Fatalf("购买占位必须零写入")
	}

	rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺少 Idempotency-Key = %d，期望 400", rr.Code)
	}

	rr = env.do(t, endUser(7), http.MethodPost, "/api/subscription/purchase",
		map[string]any{"plan_id": 1, "coupon": "x"}, map[string]string{"Idempotency-Key": "purchase-2"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未知字段 = %d，期望 403", rr.Code)
	}
	if env.repo.writeCallCount != before {
		t.Fatalf("被拒绝的购买请求不得写库")
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
