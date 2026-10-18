package httpapi

// M3 Task 7 的单元验收：定价管理端点的 RBAC / 分页 / 乐观锁 / 幂等 /
// 表达式校验 / 预览换算 / 分组覆盖价。
//
// 依赖全部是内存假实现（fakePricingRepo + miniredis），未打 integration 标签时
// 不触碰外部 PG/Redis（GC-8）。真实 SQL 的乐观锁与唯一约束由
// handlers_pricing_integration_test.go 覆盖。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 假仓储
// ---------------------------------------------------------------------------

type fakePricingRepo struct {
	mu          sync.Mutex
	models      map[string]*store.ModelPricingRecord
	groups      map[string]*store.GroupPricingRecord
	nextID      int64
	clock       time.Time
	groupsExist map[string]bool
}

func newFakePricingRepo() *fakePricingRepo {
	return &fakePricingRepo{
		models:      map[string]*store.ModelPricingRecord{},
		groups:      map[string]*store.GroupPricingRecord{},
		clock:       time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC),
		groupsExist: map[string]bool{"default": true, "vip": true},
	}
}

func (r *fakePricingRepo) adv() time.Time {
	r.clock = r.clock.Add(time.Second)
	return r.clock
}

func (r *fakePricingRepo) clone(rec store.ModelPricingRecord) store.ModelPricingRecord { return rec }

func (r *fakePricingRepo) ListModelPricing(_ context.Context, filter store.ModelPricingFilter) ([]store.ModelPricingRecord, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]store.ModelPricingRecord, 0, len(r.models))
	for _, rec := range r.models {
		if filter.Keyword != "" && !strings.HasPrefix(strings.ToLower(rec.Model), strings.ToLower(filter.Keyword)) {
			continue
		}
		switch filter.Mode {
		case "tiered_expr":
			if rec.BillingExpr == nil {
				continue
			}
		case "per_token":
			if rec.BillingExpr != nil || rec.Mode != 0 {
				continue
			}
		case "per_request":
			if rec.BillingExpr != nil || rec.Mode != 1 {
				continue
			}
		}
		if filter.Status != nil && rec.Status != *filter.Status {
			continue
		}
		items = append(items, *rec)
	}
	switch filter.Sort {
	case "-model":
		sort.Slice(items, func(i, j int) bool { return items[i].Model > items[j].Model })
	case "prompt_price":
		sort.SliceStable(items, func(i, j int) bool {
			return priceLess(items[i].PromptPrice, items[j].PromptPrice)
		})
	case "-prompt_price":
		sort.SliceStable(items, func(i, j int) bool {
			return priceLess(items[j].PromptPrice, items[i].PromptPrice)
		})
	default:
		sort.Slice(items, func(i, j int) bool { return items[i].Model < items[j].Model })
	}
	total := int64(len(items))
	start := (filter.Page - 1) * filter.PageSize
	if start >= len(items) {
		return nil, total, nil
	}
	end := start + filter.PageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], total, nil
}

func priceLess(a, b *int64) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return *a < *b
}

func (r *fakePricingRepo) GetModelPricing(_ context.Context, model string) (store.ModelPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[model]
	if !ok {
		return store.ModelPricingRecord{}, store.ErrPricingNotFound
	}
	return *rec, nil
}

func (r *fakePricingRepo) CreateModelPricing(_ context.Context, in store.ModelPricingInput) (store.ModelPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.models[in.Model]; exists {
		return store.ModelPricingRecord{}, store.ErrPricingDuplicate
	}
	r.nextID++
	now := r.adv()
	rec := &store.ModelPricingRecord{
		ID: r.nextID, Model: in.Model, Mode: in.Mode, BillingExpr: in.BillingExpr,
		PromptPrice: in.PromptPrice, CompletionPrice: in.CompletionPrice,
		CacheReadPrice: in.CacheReadPrice, CacheWritePrice: in.CacheWritePrice,
		Author: in.Author, Vendor: in.Vendor, Status: in.Status,
		CreatedAt: now, UpdatedAt: now,
	}
	r.models[in.Model] = rec
	return *rec, nil
}

func (r *fakePricingRepo) UpdateModelPricing(_ context.Context, model string, patch store.ModelPricingPatch, expect time.Time) (store.ModelPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[model]
	if !ok {
		return store.ModelPricingRecord{}, store.ErrPricingNotFound
	}
	if !rec.UpdatedAt.Equal(expect) {
		return store.ModelPricingRecord{}, store.ErrPricingConflict
	}
	if patch.Mode != nil {
		rec.Mode = *patch.Mode
	}
	if patch.BillingExprSet {
		rec.BillingExpr = patch.BillingExpr
	}
	if patch.PromptPriceSet {
		rec.PromptPrice = patch.PromptPrice
	}
	if patch.CompletionPriceSet {
		rec.CompletionPrice = patch.CompletionPrice
	}
	if patch.CacheReadPriceSet {
		rec.CacheReadPrice = patch.CacheReadPrice
	}
	if patch.CacheWritePriceSet {
		rec.CacheWritePrice = patch.CacheWritePrice
	}
	if patch.Author != nil {
		rec.Author = *patch.Author
	}
	if patch.VendorSet {
		rec.Vendor = patch.Vendor
	}
	if patch.Status != nil {
		rec.Status = *patch.Status
	}
	rec.UpdatedAt = r.adv()
	return *rec, nil
}

func (r *fakePricingRepo) DeleteModelPricing(_ context.Context, model string, expect time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[model]
	if !ok {
		return store.ErrPricingNotFound
	}
	if !rec.UpdatedAt.Equal(expect) {
		return store.ErrPricingConflict
	}
	delete(r.models, model)
	for key, gp := range r.groups {
		if gp.Model == model {
			delete(r.groups, key)
		}
	}
	return nil
}

func (r *fakePricingRepo) SetModelPricingStatus(_ context.Context, model string, status int16, expect time.Time) (store.ModelPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.models[model]
	if !ok {
		return store.ModelPricingRecord{}, store.ErrPricingNotFound
	}
	if !rec.UpdatedAt.Equal(expect) {
		return store.ModelPricingRecord{}, store.ErrPricingConflict
	}
	rec.Status = status
	rec.UpdatedAt = r.adv()
	return *rec, nil
}

func (r *fakePricingRepo) ListPricingModels(_ context.Context, filter store.PricingModelFilter) ([]store.PricingModelOption, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]store.PricingModelOption, 0, len(r.models))
	for _, rec := range r.models {
		if filter.Keyword != "" && !strings.HasPrefix(strings.ToLower(rec.Model), strings.ToLower(filter.Keyword)) {
			continue
		}
		if filter.Status != nil && rec.Status != *filter.Status {
			continue
		}
		mode := "per_token"
		if rec.BillingExpr != nil {
			mode = "tiered_expr"
		} else if rec.Mode == 1 {
			mode = "per_request"
		}
		items = append(items, store.PricingModelOption{Model: rec.Model, Mode: mode, Status: rec.Status})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Model < items[j].Model })
	if filter.Limit > 0 && len(items) > filter.Limit {
		items = items[:filter.Limit]
	}
	return items, nil
}

func groupKey(group, model string) string { return group + "\x00" + model }

func (r *fakePricingRepo) withSource(rec store.GroupPricingRecord) store.GroupPricingRecord {
	if source, ok := r.models[rec.Model]; ok {
		rec.SourceStatus = source.Status
		switch {
		case source.BillingExpr != nil:
			rec.SourceMode = "tiered_expr"
		case source.Mode == 1:
			rec.SourceMode = "per_request"
		default:
			rec.SourceMode = "per_token"
		}
	}
	return rec
}

func (r *fakePricingRepo) ListGroupPricing(_ context.Context, filter store.GroupPricingFilter) ([]store.GroupPricingRecord, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]store.GroupPricingRecord, 0, len(r.groups))
	for _, rec := range r.groups {
		if filter.Group != "" && rec.Group != filter.Group {
			continue
		}
		if filter.Keyword != "" && !strings.HasPrefix(strings.ToLower(rec.Model), strings.ToLower(filter.Keyword)) {
			continue
		}
		if filter.Status != nil && rec.Status != *filter.Status {
			continue
		}
		items = append(items, r.withSource(*rec))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Group == items[j].Group {
			return items[i].Model < items[j].Model
		}
		return items[i].Group < items[j].Group
	})
	total := int64(len(items))
	start := (filter.Page - 1) * filter.PageSize
	if start >= len(items) {
		return nil, total, nil
	}
	end := start + filter.PageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], total, nil
}

func (r *fakePricingRepo) CreateGroupPricing(_ context.Context, in store.GroupPricingInput) (store.GroupPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.groupsExist[in.Group] {
		return store.GroupPricingRecord{}, store.ErrPricingInvalidGroup
	}
	source, ok := r.models[in.Model]
	if !ok {
		return store.GroupPricingRecord{}, store.ErrPricingInvalidModel
	}
	if source.BillingExpr != nil {
		return store.GroupPricingRecord{}, store.ErrPricingSourceIsExpression
	}
	if source.Mode == 1 && (in.CompletionPrice != 0 || in.CacheReadPrice != 0 || in.CacheWritePrice != 0) {
		return store.GroupPricingRecord{}, store.ErrPricingPerRequestFields
	}
	key := groupKey(in.Group, in.Model)
	if _, exists := r.groups[key]; exists {
		return store.GroupPricingRecord{}, store.ErrPricingDuplicate
	}
	r.nextID++
	now := r.adv()
	rec := &store.GroupPricingRecord{
		ID: r.nextID, Group: in.Group, Model: in.Model,
		PromptPrice: in.PromptPrice, CompletionPrice: in.CompletionPrice,
		CacheReadPrice: in.CacheReadPrice, CacheWritePrice: in.CacheWritePrice,
		Status: in.Status, CreatedAt: now, UpdatedAt: now,
	}
	r.groups[key] = rec
	return r.withSource(*rec), nil
}

func (r *fakePricingRepo) UpdateGroupPricing(_ context.Context, group, model string, patch store.GroupPricingPatch, expect time.Time) (store.GroupPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.groups[groupKey(group, model)]
	if !ok {
		return store.GroupPricingRecord{}, store.ErrPricingNotFound
	}
	if !rec.UpdatedAt.Equal(expect) {
		return store.GroupPricingRecord{}, store.ErrPricingConflict
	}
	if patch.PromptPrice != nil || patch.CompletionPrice != nil ||
		patch.CacheReadPrice != nil || patch.CacheWritePrice != nil {
		source, ok := r.models[model]
		if !ok {
			return store.GroupPricingRecord{}, store.ErrPricingInvalidModel
		}
		if source.BillingExpr != nil {
			return store.GroupPricingRecord{}, store.ErrPricingSourceIsExpression
		}
		if source.Mode == 1 &&
			((patch.CompletionPrice != nil && *patch.CompletionPrice != 0) ||
				(patch.CacheReadPrice != nil && *patch.CacheReadPrice != 0) ||
				(patch.CacheWritePrice != nil && *patch.CacheWritePrice != 0)) {
			return store.GroupPricingRecord{}, store.ErrPricingPerRequestFields
		}
	}
	if patch.PromptPrice != nil {
		rec.PromptPrice = *patch.PromptPrice
	}
	if patch.CompletionPrice != nil {
		rec.CompletionPrice = *patch.CompletionPrice
	}
	if patch.CacheReadPrice != nil {
		rec.CacheReadPrice = *patch.CacheReadPrice
	}
	if patch.CacheWritePrice != nil {
		rec.CacheWritePrice = *patch.CacheWritePrice
	}
	if patch.Status != nil {
		rec.Status = *patch.Status
	}
	rec.UpdatedAt = r.adv()
	return r.withSource(*rec), nil
}

func (r *fakePricingRepo) DeleteGroupPricing(_ context.Context, group, model string, expect time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := groupKey(group, model)
	rec, ok := r.groups[key]
	if !ok {
		return store.ErrPricingNotFound
	}
	if !rec.UpdatedAt.Equal(expect) {
		return store.ErrPricingConflict
	}
	delete(r.groups, key)
	return nil
}

func (r *fakePricingRepo) SetGroupPricingStatus(_ context.Context, group, model string, status int16, expect time.Time) (store.GroupPricingRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.groups[groupKey(group, model)]
	if !ok {
		return store.GroupPricingRecord{}, store.ErrPricingNotFound
	}
	if !rec.UpdatedAt.Equal(expect) {
		return store.GroupPricingRecord{}, store.ErrPricingConflict
	}
	rec.Status = status
	rec.UpdatedAt = r.adv()
	return r.withSource(*rec), nil
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type pricingTestEnv struct {
	t        *testing.T
	repo     *fakePricingRepo
	handlers *PricingHandlers
	router   http.Handler
}

func newPricingTestEnv(t *testing.T) *pricingTestEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := newFakePricingRepo()
	handlers, err := NewPricingHandlers(PricingHandlerConfig{
		Repo: repo, Redis: rdb, Authz: rbac.NewAuthzMiddleware(), Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewPricingHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.RegisterPricingRoutes(mux, nil)
	return &pricingTestEnv{t: t, repo: repo, handlers: handlers, router: mux}
}

func (e *pricingTestEnv) request(p *auth.Principal, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
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
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func fullPricingAdmin() *auth.Principal {
	return &auth.Principal{UserID: 77, Role: rbac.RoleAdmin}
}

func pricingAdmin(perms ...string) *auth.Principal {
	return &auth.Principal{UserID: 77, Role: rbac.RoleAdmin, Permissions: perms}
}

func decodePricingProblem(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return body
}

func decodeModelPricingView(t *testing.T, rr *httptest.ResponseRecorder) modelPricingView {
	t.Helper()
	var view modelPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("pricing view JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func decodeModelPricingList(t *testing.T, rr *httptest.ResponseRecorder) modelPricingListView {
	t.Helper()
	var list modelPricingListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("pricing list JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return list
}

func int64Ptr(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// (a) 表达式校验
// ---------------------------------------------------------------------------

func TestPricingCreateRejectsInvalidExpression(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "gpt-invalid", "billing_expr": "p * "}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	problem := decodePricingProblem(t, rr)
	fields, _ := problem["fields"].([]any)
	if len(fields) != 1 || fields[0] != "billing_expr" {
		t.Fatalf("problem.fields = %v，期望 [billing_expr]", problem["fields"])
	}
	if _, ok := env.repo.models["gpt-invalid"]; ok {
		t.Fatal("非法表达式不得落库")
	}

	// 空白表达式按 R-37 拒绝（不得静默回落倍率）。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "gpt-blank", "billing_expr": "   "}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空白表达式 status = %d，期望 400", rr.Code)
	}
}

func TestPricingCreateExpressionMode(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "gpt-expr", "billing_expr": "p * 0.001 + c * 0.002"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	if view.Mode != pricingModeTieredExpr || view.BillingExpr == nil {
		t.Fatalf("view = %+v，期望 tiered_expr 且带 billing_expr", view)
	}
	if view.PromptPrice != nil || view.CompletionPrice != nil {
		t.Fatalf("表达式模式不得保存数值价列：%+v", view)
	}

	// 表达式 + 数值价列混用 → 400。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "gpt-mix", "billing_expr": "p", "prompt_price": 1}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("混用 status = %d，期望 400", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// (b) RBAC
// ---------------------------------------------------------------------------

func TestPricingRBAC(t *testing.T) {
	env := newPricingTestEnv(t)
	env.repo.models["seed"] = &store.ModelPricingRecord{
		ID: 1, Model: "seed", Mode: 0, Author: "Unknown", Status: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	// 普通用户：任何定价端点都 403。
	for _, path := range []string{"/api/model-pricing", "/api/model-pricing/models", "/api/group-pricing"} {
		if rr := env.request(&auth.Principal{UserID: 2, Role: rbac.RoleUser}, http.MethodGet, path, nil, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("user GET %s = %d，期望 403", path, rr.Code)
		}
	}
	// 只有 pricing:read 的 admin：读通过、写 403。
	readOnly := pricingAdmin(rbac.PermPricingRead)
	if rr := env.request(readOnly, http.MethodGet, "/api/model-pricing", nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("pricing:read GET = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(readOnly, http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "nope"}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("pricing:read POST = %d，期望 403", rr.Code)
	}
	// 只有 pricing:manage 的 admin：写通过、读 403。
	manageOnly := pricingAdmin(rbac.PermPricingManage)
	if rr := env.request(manageOnly, http.MethodGet, "/api/model-pricing", nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("pricing:manage GET = %d，期望 403", rr.Code)
	}
	if rr := env.request(manageOnly, http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "managed", "prompt_price": 10}, nil); rr.Code != http.StatusCreated {
		t.Fatalf("pricing:manage POST = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestPricingFieldPolicyRejectsUnknownFields(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "x", "password": "secret"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未知字段 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "field_not_writable") {
		t.Fatalf("problem.type 应为 field_not_writable：%s", rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// (c) 分页 / 筛选
// ---------------------------------------------------------------------------

func TestPricingListPaginationAndFilters(t *testing.T) {
	env := newPricingTestEnv(t)
	for i := 0; i < 25; i++ {
		model := fmt.Sprintf("model-%02d", i)
		status := int16(1)
		if i%5 == 0 {
			status = 2
		}
		env.repo.models[model] = &store.ModelPricingRecord{
			ID: int64(i + 1), Model: model, Mode: 0, Status: status,
			PromptPrice: int64Ptr(int64(i * 100)), Author: "Unknown",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing?p=2&page_size=10", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	list := decodeModelPricingList(t, rr)
	if list.Total != 25 || list.Page != 2 || list.PageSize != 10 || len(list.Items) != 10 {
		t.Fatalf("分页 = total %d page %d size %d items %d", list.Total, list.Page, list.PageSize, len(list.Items))
	}
	if list.QuotaPerUnit != 100 {
		t.Fatalf("quota_per_unit = %d，期望 100", list.QuotaPerUnit)
	}
	if list.Items[0].Model != "model-10" {
		t.Fatalf("第二页首项 = %s，期望 model-10", list.Items[0].Model)
	}

	// 非法分页/排序 → 400（不静默忽略）。
	for _, query := range []string{"p=0", "page_size=101", "sort=password", "mode=unknown", "status=9"} {
		if rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing?"+query, nil, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400", query, rr.Code)
		}
	}

	disabled := int16(2)
	rr = env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing?status=disabled&page_size=100", nil, nil)
	list = decodeModelPricingList(t, rr)
	if list.Total != int64(len(filterStatus(env.repo.models, disabled))) {
		t.Fatalf("status 筛选 total = %d", list.Total)
	}
}

func filterStatus(models map[string]*store.ModelPricingRecord, status int16) []string {
	var out []string
	for _, rec := range models {
		if rec.Status == status {
			out = append(out, rec.Model)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// (d) 乐观锁
// ---------------------------------------------------------------------------

func TestPricingOptimisticLock(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "locked", "prompt_price": 100}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	stale := view.UpdatedAt.Add(-time.Second)

	// 陈旧版本 PATCH → 409。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/locked",
		map[string]any{"updated_at": stale.Format(time.RFC3339Nano), "prompt_price": 200}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale PATCH = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	// 缺 updated_at → 400。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/locked",
		map[string]any{"prompt_price": 200}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 updated_at = %d，期望 400", rr.Code)
	}
	// 正确版本 → 200。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/locked",
		map[string]any{"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 200}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("fresh PATCH = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.PromptPrice == nil || *view.PromptPrice != 200 {
		t.Fatalf("prompt_price = %v，期望 200", view.PromptPrice)
	}

	// 陈旧版本禁用 → 409；正确版本禁用/启用 → 200。
	stale = view.UpdatedAt.Add(-time.Second)
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing/locked:disable",
		map[string]any{"updated_at": stale.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale disable = %d，期望 409", rr.Code)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing/locked:disable",
		map[string]any{"updated_at": view.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.Status != 2 {
		t.Fatalf("status = %d，期望 2（停用）", view.Status)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing/locked:enable",
		map[string]any{"updated_at": view.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.Status != 1 {
		t.Fatalf("status = %d，期望 1（启用）", view.Status)
	}

	// 陈旧版本 DELETE → 409；正确版本 → 204。
	stale = view.UpdatedAt.Add(-time.Second)
	rr = env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/model-pricing/locked?updated_at="+url.QueryEscape(stale.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale DELETE = %d，期望 409", rr.Code)
	}
	rr = env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/model-pricing/locked?updated_at="+url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if _, err := env.repo.GetModelPricing(context.Background(), "locked"); err == nil {
		t.Fatal("删除后仍能读到行")
	}
}

// ---------------------------------------------------------------------------
// (e) 幂等
// ---------------------------------------------------------------------------

func TestPricingCreateIdempotency(t *testing.T) {
	env := newPricingTestEnv(t)
	headers := map[string]string{"Idempotency-Key": "idem-1"}
	body := map[string]any{"model": "idem-model", "prompt_price": 100}
	first := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", body, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d（body=%q）", first.Code, first.Body.String())
	}
	second := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", body, headers)
	if second.Code != http.StatusCreated {
		t.Fatalf("replay = %d，期望 201（body=%q）", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("重放响应不一致：%q vs %q", second.Body.String(), first.Body.String())
	}
	if len(env.repo.models) != 1 {
		t.Fatalf("幂等重放产生了 %d 行，期望 1", len(env.repo.models))
	}

	// 同 key 不同 body → 422 idempotency_key_reuse。
	conflict := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "idem-other", "prompt_price": 100}, headers)
	if conflict.Code != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse = %d，期望 422（body=%q）", conflict.Code, conflict.Body.String())
	}

	// 不带 key：每次执行（依赖唯一键 409 兜底）。
	noKey := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "idem-model", "prompt_price": 100}, nil)
	if noKey.Code != http.StatusConflict {
		t.Fatalf("无 key 重名 = %d，期望 409", noKey.Code)
	}
}

// ---------------------------------------------------------------------------
// 生命周期 / 模式切换
// ---------------------------------------------------------------------------

func TestPricingLifecycleAndSensitiveFields(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", map[string]any{
		"model": "gpt-4o", "mode": "per_token", "prompt_price": 250,
		"completion_price": 1000, "cache_read_price": 25, "cache_write_price": 300,
		"author": "openai", "vendor": "OpenAI",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	if view.Free || view.Mode != "per_token" || *view.PromptPrice != 250 {
		t.Fatalf("view = %+v", view)
	}
	for _, forbidden := range []string{"password", "key_hash", "secret", "billing_expr_hash"} {
		if strings.Contains(strings.ToLower(rr.Body.String()), forbidden) {
			t.Fatalf("响应含敏感字段 %q：%s", forbidden, rr.Body.String())
		}
	}

	// 切到 per_request：忽略/隐藏的价列不得在该请求里提交。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/gpt-4o", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "mode": "per_request", "prompt_price": 5,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("switch mode = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.Mode != "per_request" || *view.PromptPrice != 5 {
		t.Fatalf("切 per_request 后 view = %+v", view)
	}
	// 隐藏列保留原值：切回 per_token 时恢复（UI「隐藏字段不清值」）。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/gpt-4o", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "mode": "per_token",
	}, nil)
	view = decodeModelPricingView(t, rr)
	if view.CompletionPrice == nil || *view.CompletionPrice != 1000 {
		t.Fatalf("切回 per_token 后 completion_price = %v，期望保留 1000", view.CompletionPrice)
	}

	// 删除。
	rr = env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/model-pricing/gpt-4o?updated_at="+url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestPricingExpressionModeTransitions(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "trans", "prompt_price": 100, "completion_price": 200}, nil)
	view := decodeModelPricingView(t, rr)

	// 只改成 per_token 而不显式清空表达式：当前非表达式，允许。
	// 先切表达式（显式）。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/trans", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano),
		"mode":       "tiered_expr", "billing_expr": "p * 0.001",
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("切表达式 = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.PromptPrice != nil || view.CompletionPrice != nil {
		t.Fatalf("切表达式后价列应清空：%+v", view)
	}

	// 表达式行上写数值价 → 422。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/trans", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 5,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("表达式行写价 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}

	// 退出表达式但只给 mode → 422，要求显式 billing_expr:null。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/trans", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "mode": "per_token",
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("歧义退出表达式 = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}

	// 显式 null + mode → 200。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/trans", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano),
		"mode":       "per_token", "billing_expr": nil, "prompt_price": 42,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("清空表达式 = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.Mode != "per_token" || view.BillingExpr != nil || *view.PromptPrice != 42 {
		t.Fatalf("退出表达式后 view = %+v", view)
	}
}

func TestPricingPerRequestRejectsHiddenPriceFields(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", map[string]any{
		"model": "per-call", "mode": "per_request", "prompt_price": 10, "completion_price": 20,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("per_request 混入 completion_price = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 预览 / 换算
// ---------------------------------------------------------------------------

func TestPricingPreviewAndYuanConversion(t *testing.T) {
	env := newPricingTestEnv(t)

	// 绝对值 spec：prompt_price=1234 积分/1M，p=1M → 1234 积分 = 12.34 元。
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview", map[string]any{
		"spec":   map[string]any{"prompt_price": 1234},
		"model":  "gpt-4o",
		"params": map[string]any{"p": 1_000_000, "image_count": pricing.ImageCountUnset},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview pricingPreviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	if preview.Quota != 1234 || preview.QuotaPerUnit != 100 || preview.AmountYuan != "12.34" {
		t.Fatalf("preview = %+v，期望 quota=1234 qpu=100 yuan=12.34", preview)
	}
	if preview.Mode != "per_token" || preview.FreeModel {
		t.Fatalf("mode/free = %s/%v", preview.Mode, preview.FreeModel)
	}

	// 表达式 spec（字符串形态）：p=100M，p*0.001 → 100000 USD/1M → 10 积分。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview", map[string]any{
		"spec":   `{"mode":"tiered_expr","expr":"p * 0.001"}`,
		"params": map[string]any{"p": 100_000_000},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("表达式 preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	if preview.Quota != 10 || preview.AmountYuan != "0.10" || preview.Mode != pricingModeTieredExpr {
		t.Fatalf("表达式 preview = %+v，期望 quota=10 yuan=0.10", preview)
	}

	// 按次 + 图片张数：250 × 2 = 500。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview", map[string]any{
		"spec":   map[string]any{"mode": "per_request", "model_price": 250},
		"params": map[string]any{"image_count": 2},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("per_call preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	if preview.Quota != 500 || preview.Mode != "per_request" {
		t.Fatalf("per_call preview = %+v，期望 quota=500", preview)
	}

	// 非法 spec → 400 + fields=spec。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview", map[string]any{
		"spec": map[string]any{"prompt_price": -1},
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 spec = %d，期望 400", rr.Code)
	}
	problem := decodePricingProblem(t, rr)
	fields, _ := problem["fields"].([]any)
	if len(fields) != 1 || fields[0] != "spec" {
		t.Fatalf("fields = %v，期望 [spec]", problem["fields"])
	}

	// 负计费量 → 400 + fields=params。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview", map[string]any{
		"spec": map[string]any{"prompt_price": 1}, "params": map[string]any{"p": -1},
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("负参数 = %d，期望 400", rr.Code)
	}
	problem = decodePricingProblem(t, rr)
	fields, _ = problem["fields"].([]any)
	if len(fields) != 1 || fields[0] != "params" {
		t.Fatalf("fields = %v，期望 [params]", problem["fields"])
	}
}

func TestPricingQuotaToYuan(t *testing.T) {
	cases := map[int64]string{
		0: "0.00", 1: "0.01", 49: "0.49", 50: "0.50",
		1234: "12.34", 1235: "12.35", 99: "0.99", 100: "1.00",
	}
	for quota, want := range cases {
		if got := quotaToYuan(quota); got != want {
			t.Fatalf("quotaToYuan(%d) = %s，期望 %s", quota, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 模型选择器
// ---------------------------------------------------------------------------

func TestPricingModelsSelector(t *testing.T) {
	env := newPricingTestEnv(t)
	for i, model := range []string{"alpha", "beta", "gamma"} {
		status := int16(1)
		if i == 2 {
			status = 2
		}
		env.repo.models[model] = &store.ModelPricingRecord{
			ID: int64(i + 1), Model: model, Mode: 0, Status: status,
			Author: "Unknown", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
	}
	rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing/models", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("models = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list pricingModelListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("models JSON: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("默认只返回启用模型，得到 %d 项", len(list.Items))
	}
	rr = env.request(fullPricingAdmin(), http.MethodGet, "/api/model-pricing/models?status=all", nil, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("models JSON: %v", err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("status=all 应返回 3 项，得到 %d", len(list.Items))
	}
}

// ---------------------------------------------------------------------------
// 分组覆盖价
// ---------------------------------------------------------------------------

func TestPricingGroupPricingCRUD(t *testing.T) {
	env := newPricingTestEnv(t)
	source := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "gpt-5", "prompt_price": 1000, "completion_price": 2000}, nil)
	if source.Code != http.StatusCreated {
		t.Fatalf("seed source = %d（body=%q）", source.Code, source.Body.String())
	}

	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "vip", "model": "gpt-5", "prompt_price": 400,
		"completion_price": 1200, "cache_read_price": 40, "cache_write_price": 120,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create override = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var view groupPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("group view JSON: %v", err)
	}
	if view.Mode != "per_token" || view.SourceStatus != 1 || view.PromptPrice != 400 {
		t.Fatalf("view = %+v", view)
	}

	// 重复 (group, model) → 409。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "vip", "model": "gpt-5",
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate override = %d，期望 409", rr.Code)
	}
	// 未知分组 / 未定价模型 → 422。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "nope", "model": "gpt-5",
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown group = %d，期望 422", rr.Code)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "vip", "model": "unpriced",
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown model = %d，期望 422", rr.Code)
	}

	// 列表。
	rr = env.request(fullPricingAdmin(), http.MethodGet, "/api/group-pricing?group=vip", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var list groupPricingListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("group list JSON: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("list = %+v", list)
	}
	// F7：sort=model 在白名单内；未知排序 400（不静默忽略）。
	if rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/group-pricing?sort=model", nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("sort=model = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(fullPricingAdmin(), http.MethodGet, "/api/group-pricing?sort=password", nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("sort=password = %d，期望 400", rr.Code)
	}

	// PATCH 乐观锁。
	stale := view.UpdatedAt.Add(-time.Second)
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/group-pricing/vip/gpt-5", map[string]any{
		"updated_at": stale.Format(time.RFC3339Nano), "prompt_price": 500,
	}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale override PATCH = %d，期望 409", rr.Code)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/group-pricing/vip/gpt-5", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 500,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("override PATCH = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("group view JSON: %v", err)
	}
	if view.PromptPrice != 500 {
		t.Fatalf("prompt_price = %d，期望 500", view.PromptPrice)
	}

	// 停用/启用。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing/vip/gpt-5:disable",
		map[string]any{"updated_at": view.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("override disable = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("group view JSON: %v", err)
	}
	if view.Status != 2 {
		t.Fatalf("status = %d，期望 2", view.Status)
	}

	// DELETE。
	rr = env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/group-pricing/vip/gpt-5?updated_at="+url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("override DELETE = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestPricingDeleteModelCascadesOverrides(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "cascade", "prompt_price": 1}, nil)
	modelView := decodeModelPricingView(t, rr)
	if rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "vip", "model": "cascade",
	}, nil); rr.Code != http.StatusCreated {
		t.Fatalf("override create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/model-pricing/cascade?updated_at="+url.QueryEscape(modelView.UpdatedAt.Format(time.RFC3339Nano)), nil, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("model delete = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.groups) != 0 {
		t.Fatalf("删除模型后仍剩 %d 条覆盖价，期望级联清理", len(env.repo.groups))
	}
}

// ---------------------------------------------------------------------------
// 路由细节
// ---------------------------------------------------------------------------

func TestPricingUnknownEndpoints(t *testing.T) {
	env := newPricingTestEnv(t)
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/model-pricing/missing", nil},
		{http.MethodPost, "/api/model-pricing/missing:explode", map[string]any{"updated_at": time.Now().Format(time.RFC3339Nano)}},
		{http.MethodPatch, "/api/model-pricing/missing", map[string]any{"updated_at": time.Now().Format(time.RFC3339Nano)}},
		{http.MethodPost, "/api/group-pricing/vip/gpt-5", map[string]any{"updated_at": time.Now().Format(time.RFC3339Nano)}},
	}
	for _, tc := range cases {
		rr := env.request(fullPricingAdmin(), tc.method, tc.path, tc.body, nil)
		if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d，期望 404/405（body=%q）", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 复审补测（F1/F4/F6/F7）
// ---------------------------------------------------------------------------

func assertProblemFields(t *testing.T, rr *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	problem := decodePricingProblem(t, rr)
	raw, _ := problem["fields"].([]any)
	got := make([]string, 0, len(raw))
	for _, item := range raw {
		got = append(got, fmt.Sprint(item))
	}
	if len(got) != len(want) {
		t.Fatalf("problem.fields = %v，期望 %v（body=%q）", got, want, rr.Body.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("problem.fields = %v，期望 %v", got, want)
		}
	}
}

// F1：价格取值错误（负值/超界/非整数/字符串）统一 422 + fields。
func TestPricingPriceRangeValidation(t *testing.T) {
	env := newPricingTestEnv(t)
	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"negative", map[string]any{"model": "bad-1", "prompt_price": -1}, "prompt_price"},
		{"overflow", map[string]any{"model": "bad-2", "prompt_price": 9007199254740992}, "prompt_price"},
		{"string", map[string]any{"model": "bad-3", "prompt_price": "10"}, "prompt_price"},
		{"fraction", map[string]any{"model": "bad-4", "prompt_price": 1.5}, "prompt_price"},
		{"cache-negative", map[string]any{"model": "bad-5", "cache_write_price": -3}, "cache_write_price"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", tc.body, nil)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
			}
			assertProblemFields(t, rr, tc.field)
			problem := decodePricingProblem(t, rr)
			if problem["type"] != problemTypeValidationFailed {
				t.Fatalf("problem.type = %v，期望 %s", problem["type"], problemTypeValidationFailed)
			}
		})
	}

	// 更新路径同样 422。
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "bad-update", "prompt_price": 10}, nil)
	view := decodeModelPricingView(t, rr)
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/bad-update", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": -5,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update negative = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "prompt_price")

	// 分组覆盖价的创建/更新同样 422。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "vip", "model": "bad-update", "completion_price": -1,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("group create negative = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "completion_price")
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing", map[string]any{
		"group": "vip", "model": "bad-update", "prompt_price": 5,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("group seed = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var override groupPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("group JSON: %v", err)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/group-pricing/vip/bad-update", map[string]any{
		"updated_at": override.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": -2,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("group update negative = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "prompt_price")
}

// F7/A3-F7：author 是 NOT NULL 列，创建与更新的显式 null 都拒绝；
// vendor null 允许清空。
func TestPricingAuthorPatchSemantics(t *testing.T) {
	env := newPricingTestEnv(t)
	// 创建时显式 null → 422（不静默用默认值）。
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "authors-null", "prompt_price": 1, "author": nil}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create author null = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "author")

	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "authors", "prompt_price": 1}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	if view.Author != "Unknown" {
		t.Fatalf("author = %q，期望默认 Unknown", view.Author)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/authors", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "author": nil,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("author null = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "author")
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/authors", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "author": "   ",
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("author blank = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "author")
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/authors", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "author": "Unknown", "vendor": nil,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("author reset = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// F6：模型名允许 ':'，贯穿 create/PATCH/action/DELETE（含分组覆盖价）。
func TestPricingColonModelLifecycle(t *testing.T) {
	env := newPricingTestEnv(t)
	model := "vendor:model"
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": model, "prompt_price": 10}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)
	if view.Model != model {
		t.Fatalf("model = %q，期望 %q", view.Model, model)
	}

	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/"+model, map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 20,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)

	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing/"+model+":disable",
		map[string]any{"updated_at": view.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("action colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view = decodeModelPricingView(t, rr)
	if view.Status != 2 {
		t.Fatalf("status = %d，期望 2", view.Status)
	}

	// 分组覆盖价：PATCH/DELETE 把整段 tail 当模型名（不误当动作）。
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "vip", "model": model, "prompt_price": 5}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("group create colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var override groupPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("group JSON: %v", err)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/group-pricing/vip/"+model, map[string]any{
		"updated_at": override.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 6,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("group patch colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("group JSON: %v", err)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing/vip/"+model+":disable",
		map[string]any{"updated_at": override.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("group action colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("group JSON: %v", err)
	}
	rr = env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/group-pricing/vip/"+model+"?updated_at="+url.QueryEscape(override.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("group delete colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(fullPricingAdmin(), http.MethodDelete,
		"/api/model-pricing/"+model+"?updated_at="+url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete colon = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// F6：免费标记只在该模型全部有效价为零时为 true。
func TestPricingFreeFlagRule(t *testing.T) {
	env := newPricingTestEnv(t)
	cases := []struct {
		model string
		body  map[string]any
		free  bool
	}{
		{"free-token", map[string]any{"model": "free-token", "prompt_price": 0}, true},
		{"paid-completion", map[string]any{"model": "paid-completion", "prompt_price": 0, "completion_price": 5}, false},
		{"free-all", map[string]any{"model": "free-all", "prompt_price": 0, "completion_price": 0, "cache_read_price": 0, "cache_write_price": 0}, true},
		{"free-percall", map[string]any{"model": "free-percall", "mode": "per_request", "prompt_price": 0}, true},
		{"paid-percall", map[string]any{"model": "paid-percall", "mode": "per_request", "prompt_price": 7}, false},
		{"unset-prompt", map[string]any{"model": "unset-prompt"}, false},
		{"expression", map[string]any{"model": "expression", "billing_expr": "p"}, false},
	}
	for _, tc := range cases {
		rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing", tc.body, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("%s create = %d（body=%q）", tc.model, rr.Code, rr.Body.String())
		}
		view := decodeModelPricingView(t, rr)
		if view.Free != tc.free {
			t.Fatalf("%s free = %v，期望 %v（view=%+v）", tc.model, view.Free, tc.free, view)
		}
	}
}

// F6：预览的饱和/钳制标记（正向溢出 → clamped=true + clamp 审计）。
func TestPricingPreviewClamp(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing:preview", map[string]any{
		"spec":   map[string]any{"mode": "tiered_expr", "expr": "p * 1000000000"},
		"params": map[string]any{"p": 1_000_000},
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("clamp preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var preview pricingPreviewView
	if err := json.Unmarshal(rr.Body.Bytes(), &preview); err != nil {
		t.Fatalf("preview JSON: %v", err)
	}
	if !preview.Clamped || preview.Quota != contract.MaxQuota || preview.Clamp == nil {
		t.Fatalf("preview = %+v，期望 clamped + quota=%d", preview, contract.MaxQuota)
	}
	if preview.Clamp.Kind != "overflow" {
		t.Fatalf("clamp.kind = %q，期望 overflow", preview.Clamp.Kind)
	}
}

// F6：F4 的无效覆盖价（表达式源 / per_request 源的 completion+cache）。
func TestPricingGroupOverrideRejectsNoOpSources(t *testing.T) {
	env := newPricingTestEnv(t)
	if rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "expr-src", "billing_expr": "p"}, nil); rr.Code != http.StatusCreated {
		t.Fatalf("expr source = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "vip", "model": "expr-src", "prompt_price": 1}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expr override = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "model")

	if rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "percall-src", "mode": "per_request", "prompt_price": 10}, nil); rr.Code != http.StatusCreated {
		t.Fatalf("percall source = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "vip", "model": "percall-src", "completion_price": 5}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("percall completion override = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "completion_price", "cache_read_price", "cache_write_price")
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "vip", "model": "percall-src", "prompt_price": 3}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("percall prompt-only override = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var override groupPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("group JSON: %v", err)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/group-pricing/vip/percall-src", map[string]any{
		"updated_at": override.UpdatedAt.Format(time.RFC3339Nano), "cache_read_price": 5,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("percall cache override = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
}

// F6：RBAC 矩阵覆盖 preview / update / delete / action。
func TestPricingRBACMatrix(t *testing.T) {
	env := newPricingTestEnv(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seed := func(model string) store.ModelPricingRecord {
		rec := store.ModelPricingRecord{
			ID: int64(len(env.repo.models) + 1), Model: model, Mode: 0, Author: "Unknown",
			Status: 1, CreatedAt: now, UpdatedAt: now,
		}
		env.repo.models[model] = &rec
		return rec
	}
	previewBody := map[string]any{"spec": map[string]any{"prompt_price": 1}, "params": map[string]any{"p": 1_000_000}}
	readOnly := pricingAdmin(rbac.PermPricingRead)
	manageOnly := pricingAdmin(rbac.PermPricingManage)

	if rr := env.request(readOnly, http.MethodPost, "/api/model-pricing:preview", previewBody, nil); rr.Code != http.StatusOK {
		t.Fatalf("read preview = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if rr := env.request(manageOnly, http.MethodPost, "/api/model-pricing:preview", previewBody, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("manage preview = %d，期望 403", rr.Code)
	}

	readSeed := seed("rbac-read")
	if rr := env.request(readOnly, http.MethodPatch, "/api/model-pricing/rbac-read", map[string]any{
		"updated_at": readSeed.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 2,
	}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("read patch = %d，期望 403", rr.Code)
	}
	if rr := env.request(readOnly, http.MethodDelete, "/api/model-pricing/rbac-read?updated_at="+
		url.QueryEscape(readSeed.UpdatedAt.Format(time.RFC3339Nano)), nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("read delete = %d，期望 403", rr.Code)
	}
	if rr := env.request(readOnly, http.MethodPost, "/api/model-pricing/rbac-read:disable",
		map[string]any{"updated_at": readSeed.UpdatedAt.Format(time.RFC3339Nano)}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("read action = %d，期望 403", rr.Code)
	}

	writeSeed := seed("rbac-write")
	rr := env.request(manageOnly, http.MethodPatch, "/api/model-pricing/rbac-write", map[string]any{
		"updated_at": writeSeed.UpdatedAt.Format(time.RFC3339Nano), "prompt_price": 3,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("manage patch = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var view modelPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("view JSON: %v", err)
	}
	rr = env.request(manageOnly, http.MethodPost, "/api/model-pricing/rbac-write:disable",
		map[string]any{"updated_at": view.UpdatedAt.Format(time.RFC3339Nano)}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("manage action = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("view JSON: %v", err)
	}
	rr = env.request(manageOnly, http.MethodDelete, "/api/model-pricing/rbac-write?updated_at="+
		url.QueryEscape(view.UpdatedAt.Format(time.RFC3339Nano)), nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("manage delete = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// F6：GatewayDeps.PricingRepo 装配接缝把定价路由挂到受保护子树。
func TestPricingAssemblySeam(t *testing.T) {
	deps, _ := gatewayDepsForTest(t)
	repo := newFakePricingRepo()
	repo.models["seeded"] = &store.ModelPricingRecord{
		ID: 1, Model: "seeded", Mode: 0, Author: "Unknown", Status: 1,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	deps.PricingRepo = repo
	rt, err := BuildGatewayRouter(deps)
	if err != nil {
		t.Fatalf("BuildGatewayRouter: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/model-pricing", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), fullPricingAdmin()))
	rr := serve(rt.protected, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mounted route = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	list := decodeModelPricingList(t, rr)
	if list.Total != 1 || list.Items[0].Model != "seeded" {
		t.Fatalf("list = %+v", list)
	}
}

// A3-F3：per_request 的隐藏价校验必须在「未提交 mode」时也生效。
func TestPricingPerRequestHiddenPricesOnUpdate(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "prc-update", "mode": "per_request", "prompt_price": 10}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d（body=%q）", rr.Code, rr.Body.String())
	}
	view := decodeModelPricingView(t, rr)

	for _, value := range []any{5, 0} {
		rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/prc-update", map[string]any{
			"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "completion_price": value,
		}, nil)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("completion=%v = %d，期望 422（body=%q）", value, rr.Code, rr.Body.String())
		}
		assertProblemFields(t, rr, "completion_price", "cache_read_price", "cache_write_price")
	}
	// 显式 null 只清空底层列（per_request 不读），允许。
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/model-pricing/prc-update", map[string]any{
		"updated_at": view.UpdatedAt.Format(time.RFC3339Nano), "completion_price": nil,
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("completion null = %d（body=%q）", rr.Code, rr.Body.String())
	}
}

// A3-F2：路径参数不合法（空格/控制字符/大写分组）必须 400，不能进 pgx。
func TestPricingPathParamValidation(t *testing.T) {
	env := newPricingTestEnv(t)
	updated := url.QueryEscape(time.Now().UTC().Format(time.RFC3339Nano))
	cases := []struct {
		method string
		path   string
		body   any
		field  string
	}{
		{http.MethodPatch, "/api/model-pricing/bad%20model", map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339Nano)}, "model"},
		{http.MethodPatch, "/api/model-pricing/bad%00model", map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339Nano)}, "model"},
		{http.MethodDelete, "/api/model-pricing/bad%20model?updated_at=" + updated, nil, "model"},
		{http.MethodPost, "/api/model-pricing/bad%20model:disable", map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339Nano)}, "model"},
		{http.MethodPatch, "/api/group-pricing/BadGroup/some-model", map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339Nano)}, "group"},
		{http.MethodDelete, "/api/group-pricing/vip/bad%20model?updated_at=" + updated, nil, "model"},
		{http.MethodPost, "/api/group-pricing/vip/bad%20model:disable", map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339Nano)}, "model"},
	}
	for _, tc := range cases {
		rr := env.request(fullPricingAdmin(), tc.method, tc.path, tc.body, nil)
		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d，期望 400/404（body=%q）", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		if rr.Code == http.StatusBadRequest {
			assertProblemFields(t, rr, tc.field)
		}
	}
}

// A3-F4：group 价列的 JSON null 不得静默当 0（会变成显式免费）。
func TestPricingGroupPriceNullRejected(t *testing.T) {
	env := newPricingTestEnv(t)
	rr := env.request(fullPricingAdmin(), http.MethodPost, "/api/model-pricing",
		map[string]any{"model": "group-null", "prompt_price": 100}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed = %d（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "vip", "model": "group-null", "prompt_price": nil}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create null price = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "prompt_price")

	rr = env.request(fullPricingAdmin(), http.MethodPost, "/api/group-pricing",
		map[string]any{"group": "vip", "model": "group-null", "prompt_price": 5}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("override seed = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var override groupPricingView
	if err := json.Unmarshal(rr.Body.Bytes(), &override); err != nil {
		t.Fatalf("group JSON: %v", err)
	}
	rr = env.request(fullPricingAdmin(), http.MethodPatch, "/api/group-pricing/vip/group-null", map[string]any{
		"updated_at": override.UpdatedAt.Format(time.RFC3339Nano), "cache_read_price": nil,
	}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update null price = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	assertProblemFields(t, rr, "cache_read_price")
}
