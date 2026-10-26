package httpapi

// Task 7 单元验收：用户/分组折扣规则 CRUD + 两个模型选择器 + 审计。
//
// 依赖全部是内存假实现（fakeDiscountRepo + 捕获式 audit Execer），未打
// integration 标签时不得触碰外部 PG/Redis（GC-8）。安全链由 rbac 中间件
// 与既有测试共用：这里直接注入 Principal 以隔离端点逻辑。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

var (
	discountTestFrom    = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	discountTestUntil   = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	discountTestCreated = time.Date(2026, 10, 27, 8, 30, 0, 0, time.UTC)
	discountTestUpdated = time.Date(2026, 10, 27, 9, 45, 0, 0, time.UTC)
)

// ---------------------------------------------------------------------------
// 假仓储
// ---------------------------------------------------------------------------

type fakeDiscountRepo struct {
	mu      sync.Mutex
	nextID  int64
	rows    map[int64]store.CustomerDiscount
	targets map[string]bool
	models  []string

	lastListTarget struct{ TargetType, TargetID string }
	lastModelQuery struct {
		Q     string
		Limit int
	}
	createInputs []store.CustomerDiscountInput
	updateInputs []store.CustomerDiscountInput
	deleted      []int64

	createErr error
	updateErr error
	deleteErr error
	listErr   error
	getErr    error
	modelsErr error
	existsErr error
}

func newFakeDiscountRepo() *fakeDiscountRepo {
	return &fakeDiscountRepo{
		rows:    make(map[int64]store.CustomerDiscount),
		targets: make(map[string]bool),
	}
}

func (r *fakeDiscountRepo) add(d store.CustomerDiscount) store.CustomerDiscount {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d.ID == 0 {
		r.nextID++
		d.ID = r.nextID
	} else if d.ID > r.nextID {
		r.nextID = d.ID
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = discountTestCreated
	}
	if d.UpdatedAt.IsZero() {
		d.UpdatedAt = d.CreatedAt
	}
	r.rows[d.ID] = d
	return d
}

func (r *fakeDiscountRepo) allowTarget(targetType, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets[targetType+":"+targetID] = true
}

func (r *fakeDiscountRepo) ListForTarget(_ context.Context, targetType, targetID string) ([]store.CustomerDiscount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastListTarget = struct{ TargetType, TargetID string }{targetType, targetID}
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]store.CustomerDiscount, 0, len(r.rows))
	for _, d := range r.rows {
		if d.TargetType == targetType && d.TargetID == targetID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelName < out[j].ModelName })
	return out, nil
}

func (r *fakeDiscountRepo) Get(_ context.Context, id int64) (store.CustomerDiscount, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return store.CustomerDiscount{}, false, r.getErr
	}
	d, ok := r.rows[id]
	if !ok {
		// 与 store.CustomerDiscountRepo 一致：行不存在时同时返回 ok=false 和
		// ErrDiscountNotFound，handler 必须把它映射成 404 而不是 500。
		return store.CustomerDiscount{}, false, store.ErrDiscountNotFound
	}
	return d, true, nil
}

func (r *fakeDiscountRepo) Create(_ context.Context, in store.CustomerDiscountInput) (store.CustomerDiscount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return store.CustomerDiscount{}, r.createErr
	}
	r.createInputs = append(r.createInputs, in)
	r.nextID++
	d := store.CustomerDiscount{
		ID:          r.nextID,
		ModelName:   in.ModelName,
		TargetType:  in.TargetType,
		TargetID:    in.TargetID,
		RatePercent: in.RatePercent,
		ValidFrom:   in.ValidFrom,
		ValidUntil:  in.ValidUntil,
		Enabled:     in.Enabled,
		CreatedAt:   discountTestCreated,
		UpdatedAt:   discountTestCreated,
	}
	r.rows[d.ID] = d
	return d, nil
}

func (r *fakeDiscountRepo) Update(_ context.Context, id int64, in store.CustomerDiscountInput) (store.CustomerDiscount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return store.CustomerDiscount{}, r.updateErr
	}
	d, ok := r.rows[id]
	if !ok {
		return store.CustomerDiscount{}, store.ErrDiscountNotFound
	}
	r.updateInputs = append(r.updateInputs, in)
	d.ModelName = in.ModelName
	d.TargetType = in.TargetType
	d.TargetID = in.TargetID
	d.RatePercent = in.RatePercent
	d.ValidFrom = in.ValidFrom
	d.ValidUntil = in.ValidUntil
	d.Enabled = in.Enabled
	d.UpdatedAt = discountTestUpdated
	r.rows[id] = d
	return d, nil
}

func (r *fakeDiscountRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteErr != nil {
		return r.deleteErr
	}
	if _, ok := r.rows[id]; !ok {
		return store.ErrDiscountNotFound
	}
	r.deleted = append(r.deleted, id)
	delete(r.rows, id)
	return nil
}

func (r *fakeDiscountRepo) ListEnabledModels(_ context.Context, q string, limit int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastModelQuery = struct {
		Q     string
		Limit int
	}{q, limit}
	if r.modelsErr != nil {
		return nil, r.modelsErr
	}
	needle := strings.ToLower(strings.TrimSpace(q))
	out := make([]string, 0, len(r.models))
	for _, model := range r.models {
		if needle != "" && !strings.Contains(strings.ToLower(model), needle) {
			continue
		}
		out = append(out, model)
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeDiscountRepo) TargetExists(_ context.Context, targetType, targetID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.existsErr != nil {
		return false, r.existsErr
	}
	return r.targets[targetType+":"+targetID], nil
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type discountTestEnv struct {
	t      *testing.T
	repo   *fakeDiscountRepo
	audit  *adminAuditCapture
	authz  *rbac.AuthzMiddleware
	router http.Handler
}

func newDiscountTestEnv(t *testing.T) *discountTestEnv {
	t.Helper()
	repo := newFakeDiscountRepo()
	capture := &adminAuditCapture{}
	authz := rbac.NewAuthzMiddleware()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handlers, err := NewDiscountHandlers(DiscountHandlerConfig{
		Repo: repo, Authz: authz, Audit: audit.New(capture), Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewDiscountHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.RegisterDiscountRoutes(mux, nil)
	return &discountTestEnv{t: t, repo: repo, audit: capture, authz: authz, router: mux}
}

func (e *discountTestEnv) request(p *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
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
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func (e *discountTestEnv) rawRequest(p *auth.Principal, method, path, raw string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	return serve(e.router, req)
}

func decodeDiscountRule(t *testing.T, rr *httptest.ResponseRecorder) discountRuleView {
	t.Helper()
	var view discountRuleView
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatalf("discount rule JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return view
}

func decodeDiscountList(t *testing.T, rr *httptest.ResponseRecorder) discountRuleListView {
	t.Helper()
	var list discountRuleListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("discount list JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return list
}

func decodeDiscountModelList(t *testing.T, rr *httptest.ResponseRecorder) discountModelListView {
	t.Helper()
	var list discountModelListView
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("discount model list JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return list
}

// lastDiscountAuditDetail 解析最后一条审计的 detail。Logger 会在 detail 里
// 统一注入 action（R-43），因此断言 action 也从这里读。
func lastDiscountAuditDetail(t *testing.T, capture *adminAuditCapture) map[string]any {
	t.Helper()
	calls := capture.allCalls()
	if len(calls) == 0 {
		t.Fatal("未写入任何审计")
	}
	arg := calls[len(calls)-1].args[9]
	var raw []byte
	switch value := arg.(type) {
	case string:
		raw = []byte(value)
	case []byte:
		raw = value
	default:
		t.Fatalf("审计 detail 参数类型 = %T，期望 string/[]byte", arg)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("审计 detail JSON 非法: %v（raw=%q）", err, string(raw))
	}
	return detail
}

func userDiscountRule(id int64, targetID, model string, rate int16) store.CustomerDiscount {
	return store.CustomerDiscount{
		ID: id, ModelName: model, TargetType: "user", TargetID: targetID,
		RatePercent: rate, Enabled: true,
		CreatedAt: discountTestCreated, UpdatedAt: discountTestCreated,
	}
}

// ---------------------------------------------------------------------------
// 列表 / 创建
// ---------------------------------------------------------------------------

func TestListUserDiscounts(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.add(store.CustomerDiscount{
		ID: 11, ModelName: "claude-sonnet-5-5", TargetType: "user", TargetID: "5",
		RatePercent: 90, ValidFrom: &discountTestFrom, Enabled: true,
		CreatedAt: discountTestCreated, UpdatedAt: discountTestUpdated,
	})
	env.repo.add(userDiscountRule(12, "5", "gpt-5", 100))

	rr := env.request(fullAdmin(), http.MethodGet, "/api/users/5/discounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	list := decodeDiscountList(t, rr)
	if len(list.Items) != 2 {
		t.Fatalf("items = %d，期望 2（body=%q）", len(list.Items), rr.Body.String())
	}
	// 仓储按 model_name 排序：claude-sonnet-5-5 在前。
	first := list.Items[0]
	if first.ID != 11 || first.ModelName != "claude-sonnet-5-5" {
		t.Fatalf("first = %+v，期望 id=11 model=claude-sonnet-5-5", first)
	}
	if first.RatePercent != 90 || !first.Enabled {
		t.Fatalf("first = %+v，期望 rate_percent=90 enabled=true", first)
	}
	if first.ValidFrom == nil || *first.ValidFrom != "2026-11-01T00:00:00Z" {
		t.Fatalf("valid_from = %v，期望 2026-11-01T00:00:00Z", first.ValidFrom)
	}
	if first.ValidUntil != nil {
		t.Fatalf("valid_until = %v，期望 null", *first.ValidUntil)
	}
	if first.CreatedAt != "2026-10-27T08:30:00Z" || first.UpdatedAt != "2026-10-27T09:45:00Z" {
		t.Fatalf("created/updated = %q/%q", first.CreatedAt, first.UpdatedAt)
	}
	if env.repo.lastListTarget.TargetType != "user" || env.repo.lastListTarget.TargetID != "5" {
		t.Fatalf("仓储 target = %+v，期望 user:5", env.repo.lastListTarget)
	}
}

func TestCreateUserDiscountValidatesRate(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 101,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rate_percent") {
		t.Fatalf("422 body 未标明 rate_percent：%q", rr.Body.String())
	}
	if len(env.repo.createInputs) != 0 {
		t.Fatal("校验失败不得调用仓储 Create")
	}
}

func TestCreateUserDiscountValidatesWindow(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name":   "gpt-5",
		"rate_percent": 90,
		"valid_from":   "2027-01-01T00:00:00Z",
		"valid_until":  "2026-11-01T00:00:00Z",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "valid_until") {
		t.Fatalf("422 body 未标明 valid_until：%q", rr.Body.String())
	}

	// from == until 同样非法（左闭右开区间不能为空）。
	rr = env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name":   "gpt-5",
		"rate_percent": 90,
		"valid_from":   "2026-11-01T00:00:00Z",
		"valid_until":  "2026-11-01T00:00:00Z",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("from==until status = %d，期望 422", rr.Code)
	}
}

func TestCreateUserDiscountUnknownUserIs404(t *testing.T) {
	env := newDiscountTestEnv(t)
	// 没有 allowTarget：TargetExists=false。

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 90,
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.createInputs) != 0 {
		t.Fatal("目标不存在时不得调用仓储 Create")
	}
}

func TestCreateUserDiscountDuplicateIs409(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")
	env.repo.createErr = store.ErrDiscountDuplicate

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 90,
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestCreateUserDiscountDefaultsEnabled(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 90,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.createInputs) != 1 {
		t.Fatalf("Create 调用次数 = %d，期望 1", len(env.repo.createInputs))
	}
	in := env.repo.createInputs[0]
	if in.TargetType != "user" || in.TargetID != "5" || in.ModelName != "gpt-5" || in.RatePercent != 90 {
		t.Fatalf("Create input = %+v", in)
	}
	if !in.Enabled {
		t.Fatal("缺省 enabled 应为 true")
	}
	view := decodeDiscountRule(t, rr)
	if view.ID == 0 || view.ModelName != "gpt-5" || view.RatePercent != 90 || !view.Enabled {
		t.Fatalf("view = %+v", view)
	}
}

func TestCreateDiscountInvalidTimeIs422(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 90, "valid_from": "not-a-time",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "valid_from") {
		t.Fatalf("422 body 未标明 valid_from：%q", rr.Body.String())
	}
}

func TestCreateDiscountModelTooLongIs422(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": strings.Repeat("m", 201), "rate_percent": 90,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 更新 / 删除
// ---------------------------------------------------------------------------

func TestPatchDiscountRejectsModelName(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.add(userDiscountRule(11, "5", "gpt-5", 90))

	rr := env.request(fullAdmin(), http.MethodPatch, "/api/users/5/discounts/11", map[string]any{
		"model_name": "claude-sonnet-5-5", "rate_percent": 80,
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "model_immutable") {
		t.Fatalf("422 body 缺少 model_immutable：%q", rr.Body.String())
	}
	if len(env.repo.updateInputs) != 0 {
		t.Fatal("model_name 不可改，不得调用仓储 Update")
	}
}

func TestPatchDiscountNotOwnedByTargetIs404(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.add(userDiscountRule(11, "6", "gpt-5", 90))

	rr := env.request(fullAdmin(), http.MethodPatch, "/api/users/5/discounts/11", map[string]any{
		"rate_percent": 80,
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.updateInputs) != 0 {
		t.Fatal("归属不匹配不得调用仓储 Update")
	}
}

func TestPatchDiscountUpdatesAndClearsWindow(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.add(store.CustomerDiscount{
		ID: 11, ModelName: "gpt-5", TargetType: "user", TargetID: "5",
		RatePercent: 90, ValidFrom: &discountTestFrom, ValidUntil: &discountTestUntil,
		Enabled: true, CreatedAt: discountTestCreated, UpdatedAt: discountTestCreated,
	})

	rr := env.request(fullAdmin(), http.MethodPatch, "/api/users/5/discounts/11", map[string]any{
		"rate_percent": 80,
		"valid_until":  nil,
		"enabled":      false,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.updateInputs) != 1 {
		t.Fatalf("Update 调用次数 = %d，期望 1", len(env.repo.updateInputs))
	}
	in := env.repo.updateInputs[0]
	if in.ModelName != "gpt-5" {
		t.Fatalf("model_name = %q，期望保留 gpt-5", in.ModelName)
	}
	if in.RatePercent != 80 || in.Enabled {
		t.Fatalf("rate/enabled = %d/%v，期望 80/false", in.RatePercent, in.Enabled)
	}
	if in.ValidFrom == nil || !in.ValidFrom.Equal(discountTestFrom) {
		t.Fatalf("valid_from 应保留原值，实际 %v", in.ValidFrom)
	}
	if in.ValidUntil != nil {
		t.Fatalf("valid_until 应被 null 清空，实际 %v", *in.ValidUntil)
	}
	view := decodeDiscountRule(t, rr)
	if view.RatePercent != 80 || view.Enabled || view.ValidUntil != nil {
		t.Fatalf("view = %+v", view)
	}
	detail := lastDiscountAuditDetail(t, env.audit)
	if detail["action"] != audit.ActionUserDiscountSet {
		t.Fatalf("audit action = %v，期望 %s", detail["action"], audit.ActionUserDiscountSet)
	}
}

func TestDeleteUserDiscount(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.add(userDiscountRule(11, "5", "gpt-5", 90))

	rr := env.request(fullAdmin(), http.MethodDelete, "/api/users/5/discounts/11", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 不应有 body：%q", rr.Body.String())
	}
	if len(env.repo.deleted) != 1 || env.repo.deleted[0] != 11 {
		t.Fatalf("deleted = %v，期望 [11]", env.repo.deleted)
	}
	detail := lastDiscountAuditDetail(t, env.audit)
	if detail["action"] != audit.ActionUserDiscountDelete {
		t.Fatalf("audit action = %v，期望 %s", detail["action"], audit.ActionUserDiscountDelete)
	}
	if detail["target_type"] != "user" || detail["target_id"] != "5" || detail["model_name"] != "gpt-5" {
		t.Fatalf("audit detail = %+v", detail)
	}
	if strings.Contains(env.audit.concatenatedArgs(), "password") {
		t.Fatal("审计不得包含口令字段")
	}
}

func TestDeleteDiscountUnknownIs404(t *testing.T) {
	env := newDiscountTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodDelete, "/api/users/5/discounts/999", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 权限 / BOPLA
// ---------------------------------------------------------------------------

func TestGroupDiscountRequiresGroupsManage(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("group", "acme")

	rr := env.request(limitedAdmin(rbac.PermUsersUpdate), http.MethodGet, "/api/groups/acme/discounts", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("仅 users:update 访问分组折扣 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.repo.rows) != 0 {
		t.Fatal("未授权请求不得到达仓储")
	}

	rr = env.request(limitedAdmin(rbac.PermGroupsManage), http.MethodGet, "/api/groups/acme/discounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups:manage status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestDiscountModelPickerSplitsPermissions(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.models = []string{"gpt-5", "claude-sonnet-5-5"}

	usersOnly := limitedAdmin(rbac.PermUsersUpdate)
	rr := env.request(usersOnly, http.MethodGet, "/api/users/discount-models", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("users:update → /api/users/discount-models status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	list := decodeDiscountModelList(t, rr)
	if len(list.Items) != 2 || list.Items[0].ModelName != "claude-sonnet-5-5" {
		t.Fatalf("model items = %+v", list.Items)
	}
	rr = env.request(usersOnly, http.MethodGet, "/api/groups/discount-models", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("users:update → /api/groups/discount-models status = %d，期望 403", rr.Code)
	}

	groupsOnly := limitedAdmin(rbac.PermGroupsManage)
	rr = env.request(groupsOnly, http.MethodGet, "/api/groups/discount-models", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("groups:manage → /api/groups/discount-models status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	rr = env.request(groupsOnly, http.MethodGet, "/api/users/discount-models", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("groups:manage → /api/users/discount-models status = %d，期望 403", rr.Code)
	}
}

func TestDiscountWriteRejectsUnregisteredField(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 90, "foo": "bar",
	})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "field_not_writable") {
		t.Fatalf("problem.type 应为 field_not_writable：%q", rr.Body.String())
	}
	if len(env.repo.createInputs) != 0 {
		t.Fatal("字段级拒绝后不得调用仓储 Create")
	}
}

func TestCreateDiscountAuditsWithoutSecrets(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("user", "5")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/users/5/discounts", map[string]any{
		"model_name":   "gpt-5",
		"rate_percent": 90,
		"valid_from":   "2026-11-01T00:00:00Z",
		"enabled":      true,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	detail := lastDiscountAuditDetail(t, env.audit)
	if detail["action"] != audit.ActionUserDiscountSet {
		t.Fatalf("audit action = %v，期望 %s", detail["action"], audit.ActionUserDiscountSet)
	}
	if detail["target_type"] != "user" || detail["target_id"] != "5" {
		t.Fatalf("audit target = %v/%v", detail["target_type"], detail["target_id"])
	}
	if detail["model_name"] != "gpt-5" {
		t.Fatalf("audit model_name = %v", detail["model_name"])
	}
	if detail["rate_percent"] != float64(90) {
		t.Fatalf("audit rate_percent = %v（%T），期望 90", detail["rate_percent"], detail["rate_percent"])
	}
	if detail["valid_from"] != "2026-11-01T00:00:00Z" || detail["valid_until"] != nil {
		t.Fatalf("audit window = %v / %v", detail["valid_from"], detail["valid_until"])
	}
	if detail["enabled"] != true {
		t.Fatalf("audit enabled = %v", detail["enabled"])
	}
	if strings.Contains(env.audit.concatenatedArgs(), "password") {
		t.Fatal("审计不得包含口令字段")
	}
}

func TestDiscountEndpointsRejectPlainUser(t *testing.T) {
	env := newDiscountTestEnv(t)

	rr := env.request(plainUser(), http.MethodGet, "/api/users/5/discounts", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("user 角色 status = %d，期望 403", rr.Code)
	}
	rr = env.request(nil, http.MethodGet, "/api/users/discount-models", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("未认证 status = %d，期望 403", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 分组 CRUD
// ---------------------------------------------------------------------------

func TestGroupDiscountCRUDAndAudit(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.allowTarget("group", "acme")

	rr := env.request(fullAdmin(), http.MethodPost, "/api/groups/acme/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 85,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d，期望 201（body=%q）", rr.Code, rr.Body.String())
	}
	created := decodeDiscountRule(t, rr)
	detail := lastDiscountAuditDetail(t, env.audit)
	if detail["action"] != audit.ActionGroupDiscountSet {
		t.Fatalf("create audit action = %v，期望 %s", detail["action"], audit.ActionGroupDiscountSet)
	}
	if detail["target_type"] != "group" || detail["target_id"] != "acme" {
		t.Fatalf("create audit target = %v/%v", detail["target_type"], detail["target_id"])
	}

	rr = env.request(fullAdmin(), http.MethodGet, "/api/groups/acme/discounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d，期望 200", rr.Code)
	}
	if list := decodeDiscountList(t, rr); len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("list = %+v", list.Items)
	}

	rr = env.request(fullAdmin(), http.MethodPatch, "/api/groups/acme/discounts/"+strconv.FormatInt(created.ID, 10), map[string]any{
		"rate_percent": 75,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if in := env.repo.updateInputs[len(env.repo.updateInputs)-1]; in.TargetType != "group" || in.TargetID != "acme" {
		t.Fatalf("update target = %s:%s", in.TargetType, in.TargetID)
	}

	rr = env.request(fullAdmin(), http.MethodDelete, "/api/groups/acme/discounts/"+strconv.FormatInt(created.ID, 10), nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d，期望 204（body=%q）", rr.Code, rr.Body.String())
	}
	detail = lastDiscountAuditDetail(t, env.audit)
	if detail["action"] != audit.ActionGroupDiscountDelete {
		t.Fatalf("delete audit action = %v，期望 %s", detail["action"], audit.ActionGroupDiscountDelete)
	}
}

func TestCreateGroupDiscountUnknownGroupIs404(t *testing.T) {
	env := newDiscountTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/groups/nope/discounts", map[string]any{
		"model_name": "gpt-5", "rate_percent": 90,
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestPatchGroupDiscountNotOwnedByTargetIs404(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.add(store.CustomerDiscount{
		ID: 11, ModelName: "gpt-5", TargetType: "group", TargetID: "other",
		RatePercent: 90, Enabled: true,
		CreatedAt: discountTestCreated, UpdatedAt: discountTestCreated,
	})

	rr := env.request(fullAdmin(), http.MethodPatch, "/api/groups/acme/discounts/11", map[string]any{
		"rate_percent": 80,
	})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 模型选择器的 limit / q 口径
// ---------------------------------------------------------------------------

func TestDiscountModelPickerLimitDefaultsAndClamps(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.models = []string{"gpt-5"}

	cases := []struct {
		query string
		want  int
		code  int
	}{
		{"", 50, http.StatusOK},
		{"?limit=500", 100, http.StatusOK},
		{"?limit=0", 1, http.StatusOK},
		{"?limit=abc", 0, http.StatusBadRequest},
	}
	for _, tc := range cases {
		env.repo.lastModelQuery.Limit = 0
		rr := env.request(fullAdmin(), http.MethodGet, "/api/users/discount-models"+tc.query, nil)
		if rr.Code != tc.code {
			t.Fatalf("query %q status = %d，期望 %d（body=%q）", tc.query, rr.Code, tc.code, rr.Body.String())
		}
		if tc.code == http.StatusOK && env.repo.lastModelQuery.Limit != tc.want {
			t.Fatalf("query %q 传给仓储 limit = %d，期望 %d", tc.query, env.repo.lastModelQuery.Limit, tc.want)
		}
	}
}

func TestDiscountModelPickerPassesQueryThrough(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.models = []string{"gpt-5", "claude-sonnet-5-5"}

	rr := env.request(fullAdmin(), http.MethodGet, "/api/users/discount-models?q=%20sonnet%20", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	// 仓储自己 TrimSpace；handler 原样透传。
	if env.repo.lastModelQuery.Q != " sonnet " {
		t.Fatalf("传给仓储 q = %q，期望原样透传", env.repo.lastModelQuery.Q)
	}
	list := decodeDiscountModelList(t, rr)
	if len(list.Items) != 1 || list.Items[0].ModelName != "claude-sonnet-5-5" {
		t.Fatalf("items = %+v", list.Items)
	}
}

func TestListDiscountsRepoErrorIs500(t *testing.T) {
	env := newDiscountTestEnv(t)
	env.repo.listErr = errors.New("db down")

	rr := env.request(fullAdmin(), http.MethodGet, "/api/users/5/discounts", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，期望 500（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestDiscountRouteIDParsing(t *testing.T) {
	env := newDiscountTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodGet, "/api/users/abc/discounts", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("非法 user id status = %d，期望 404", rr.Code)
	}
	rr = env.request(fullAdmin(), http.MethodPatch, "/api/users/5/discounts/abc", map[string]any{"rate_percent": 80})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("非法 discount_id status = %d，期望 404", rr.Code)
	}
}
