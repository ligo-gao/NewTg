package httpapi

// 本文件是 Task 9「对账运行 / 明细 / 概览」端点的验收：
//   - 五个操作（runs、items、summary、run、ack）全部要求 settings:manage；
//   - 手动触发的 Outcome 折叠（skipped → succeeded + 零计数；disabled 是
//     不可达的防御分支）；
//   - ack 的 status 闭集与 note 长度上限（422）；
//   - summary 的 run_id 必填（400）与 run 不存在（404）。
//
// 依赖复用 handlers_users_test.go 的 adminAuditCapture/fullAdmin/limitedAdmin/
// plainUser 与 middleware_test.go 的 serve。仓储与 runner 都是内存假实现，
// 不触 PG，也不启动真实对账。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/reconcile"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 假仓储 / 假 runner
// ---------------------------------------------------------------------------

// fakeReconcileStore 是 ReconcileStore 的内存实现。
type fakeReconcileStore struct {
	mu sync.Mutex

	runs  []store.ReconcileRun
	items []store.ReconcileItem
	days  []store.SummaryDay

	listRunsErr  error
	listItemsErr error
	summaryErr   error
	getRunErr    error
	getItemErr   error
	ackErr       error

	lastAccountID              *int64
	lastRunsPage, lastRunsSz   int
	lastRunID                  int64
	lastSeverity, lastStatus   string
	lastItemsPage, lastItemsSz int

	ackCalls                   int
	lastAckID                  int64
	lastAckStatus, lastAckNote string
}

func newFakeReconcileStore() *fakeReconcileStore { return &fakeReconcileStore{} }

func (s *fakeReconcileStore) addRun(run store.ReconcileRun) store.ReconcileRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.ID == 0 {
		run.ID = int64(len(s.runs) + 1)
	}
	s.runs = append(s.runs, run)
	return run
}

func (s *fakeReconcileStore) addItem(item store.ReconcileItem) store.ReconcileItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.ID == 0 {
		item.ID = int64(len(s.items) + 1)
	}
	s.items = append(s.items, item)
	return item
}

func pageSlice[T any](all []T, page, pageSize int) []T {
	start := (page - 1) * pageSize
	if start >= len(all) {
		return []T{}
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	return append([]T(nil), all[start:end]...)
}

func (s *fakeReconcileStore) ListRuns(_ context.Context, accountID *int64, page, pageSize int) ([]store.ReconcileRun, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAccountID = accountID
	s.lastRunsPage, s.lastRunsSz = page, pageSize
	if s.listRunsErr != nil {
		return nil, 0, s.listRunsErr
	}
	filtered := make([]store.ReconcileRun, 0, len(s.runs))
	for _, run := range s.runs {
		if accountID != nil && run.AccountID != *accountID {
			continue
		}
		filtered = append(filtered, run)
	}
	return pageSlice(filtered, page, pageSize), int64(len(filtered)), nil
}

func (s *fakeReconcileStore) ListItems(_ context.Context, runID int64, severity, status string, page, pageSize int) ([]store.ReconcileItem, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRunID = runID
	s.lastSeverity, s.lastStatus = severity, status
	s.lastItemsPage, s.lastItemsSz = page, pageSize
	if s.listItemsErr != nil {
		return nil, 0, s.listItemsErr
	}
	filtered := make([]store.ReconcileItem, 0, len(s.items))
	for _, item := range s.items {
		if item.RunID != runID {
			continue
		}
		if severity != "" && item.Severity != severity {
			continue
		}
		if status != "" && item.Status != status {
			continue
		}
		filtered = append(filtered, item)
	}
	return pageSlice(filtered, page, pageSize), int64(len(filtered)), nil
}

func (s *fakeReconcileStore) Summary(_ context.Context, runID int64) ([]store.SummaryDay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRunID = runID
	if s.summaryErr != nil {
		return nil, s.summaryErr
	}
	return append([]store.SummaryDay(nil), s.days...), nil
}

func (s *fakeReconcileStore) GetRun(_ context.Context, id int64) (*store.ReconcileRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getRunErr != nil {
		return nil, s.getRunErr
	}
	for i := range s.runs {
		if s.runs[i].ID == id {
			run := s.runs[i]
			return &run, nil
		}
	}
	return nil, store.ErrRunNotFound
}

func (s *fakeReconcileStore) GetItem(_ context.Context, id int64) (*store.ReconcileItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getItemErr != nil {
		return nil, s.getItemErr
	}
	for i := range s.items {
		if s.items[i].ID == id {
			item := s.items[i]
			return &item, nil
		}
	}
	return nil, store.ErrItemNotFound
}

func (s *fakeReconcileStore) AckItem(_ context.Context, id int64, status, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ackCalls++
	s.lastAckID, s.lastAckStatus, s.lastAckNote = id, status, note
	if s.ackErr != nil {
		return s.ackErr
	}
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Status = status
			s.items[i].Note = note
			s.items[i].UpdatedAt = time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
			return nil
		}
	}
	return store.ErrItemNotFound
}

// reconcileRunCall 记录一次 runner.Run 的入参。
type reconcileRunCall struct {
	trigger    string
	accountIDs []int64
}

// fakeReconcileRunner 是 ReconcileRunner 的假实现。
type fakeReconcileRunner struct {
	mu      sync.Mutex
	calls   []reconcileRunCall
	outcome reconcile.Outcome
	err     error
}

func (r *fakeReconcileRunner) Run(_ context.Context, trigger string, accountIDs []int64) (reconcile.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, reconcileRunCall{
		trigger: trigger, accountIDs: append([]int64(nil), accountIDs...),
	})
	return r.outcome, r.err
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type reconcileTestEnv struct {
	t      *testing.T
	store  *fakeReconcileStore
	runner *fakeReconcileRunner
	audit  *adminAuditCapture
	now    time.Time
	router http.Handler
}

// newReconcileTestEnv 装配对账端点。runner 为 nil 的装配用于验证 503 闸门。
func newReconcileTestEnv(t *testing.T, runner *fakeReconcileRunner) *reconcileTestEnv {
	t.Helper()
	fs := newFakeReconcileStore()
	capture := &adminAuditCapture{}
	authz := rbac.NewAuthzMiddleware()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	cfg := ReconcileHandlerConfig{
		Repo:   fs,
		Authz:  authz,
		Audit:  audit.New(capture),
		Logger: logger,
		Now:    func() time.Time { return now },
	}
	if runner != nil {
		cfg.Runner = runner
	}
	handlers, err := NewReconcileHandlers(cfg)
	if err != nil {
		t.Fatalf("NewReconcileHandlers: %v", err)
	}

	mux := http.NewServeMux()
	handlers.RegisterRoutes(mux, nil)
	return &reconcileTestEnv{
		t: t, store: fs, runner: runner, audit: capture, now: now, router: mux,
	}
}

func (e *reconcileTestEnv) request(p *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
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

func decodeReconcileJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return out
}

func jsonInts(t *testing.T, value any) []int64 {
	t.Helper()
	raw, ok := value.([]any)
	if !ok {
		t.Fatalf("值 %v 不是数组", value)
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		number, ok := item.(float64)
		if !ok {
			t.Fatalf("数组元素 %v 不是数字", item)
		}
		out = append(out, int64(number))
	}
	return out
}

// ---------------------------------------------------------------------------
// 验收测试（brief Step 1）
// ---------------------------------------------------------------------------

func TestReconcileRoutesRejectAdminWithoutSettingsManage(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addRun(store.ReconcileRun{ID: 1, AccountID: 1})
	env.store.addItem(store.ReconcileItem{ID: 1, RunID: 1, Status: "open"})

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list runs", http.MethodGet, "/api/upstream-reconciliations/runs", nil},
		{"list items", http.MethodGet, "/api/upstream-reconciliations/runs/1/items", nil},
		{"summary", http.MethodGet, "/api/upstream-reconciliations/summary?run_id=1", nil},
		{"trigger run", http.MethodPost, "/api/upstream-reconciliations/run", map[string]any{}},
		{"ack item", http.MethodPost, "/api/upstream-reconciliations/items/1/ack",
			map[string]any{"status": "acknowledged", "note": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := env.request(limitedAdmin(rbac.PermUsersRead), tc.method, tc.path, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("缺 settings:manage status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
			}
		})
	}
}

func TestTriggerRunReturnsRunIDs(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.runner.outcome = reconcile.Outcome{
		RunIDs: []int64{1, 2}, Items: 9, Alerts: 2, Status: "succeeded",
	}

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/run", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("触发对账 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := decodeReconcileJSON(t, rr)
	if ids := jsonInts(t, body["run_ids"]); len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("run_ids = %v，期望 [1 2]", body["run_ids"])
	}
	if body["items"] != float64(9) {
		t.Fatalf("items = %v，期望 9", body["items"])
	}
	if body["alerts"] != float64(2) {
		t.Fatalf("alerts = %v，期望 2", body["alerts"])
	}
	if body["status"] != "succeeded" {
		t.Fatalf("status = %v，期望 succeeded", body["status"])
	}

	if len(env.runner.calls) != 1 {
		t.Fatalf("runner 调用次数 = %d，期望 1", len(env.runner.calls))
	}
	call := env.runner.calls[0]
	if call.trigger != "manual" {
		t.Fatalf("trigger = %q，期望 manual", call.trigger)
	}
	if len(call.accountIDs) != 0 {
		t.Fatalf("accountIDs = %v，期望为空（全部启用账号）", call.accountIDs)
	}

	calls := env.audit.allCalls()
	if len(calls) != 1 {
		t.Fatalf("审计写入次数 = %d，期望 1", len(calls))
	}
	if !strings.Contains(env.audit.concatenatedArgs(), audit.ActionReconcileRun) {
		t.Fatalf("审计未记录 %s：%s", audit.ActionReconcileRun, env.audit.concatenatedArgs())
	}
}

func TestAckRejectsUnknownStatus(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addItem(store.ReconcileItem{ID: 5, RunID: 1, Status: "open"})

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/items/5/ack",
		map[string]any{"status": "closed", "note": ""})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法 status status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	if env.store.ackCalls != 0 {
		t.Fatalf("校验失败仍调用了 AckItem %d 次", env.store.ackCalls)
	}
}

func TestAckRequiresNoteField(t *testing.T) {
	// note 是必填字段（可空串）：缺字段或显式 null 都必须 400，不能把已有
	// 说明静默清空。
	cases := []struct {
		name string
		body map[string]any
	}{
		{"absent", map[string]any{"status": "resolved"}},
		{"null", map[string]any{"status": "resolved", "note": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcileTestEnv(t, &fakeReconcileRunner{})
			env.store.addItem(store.ReconcileItem{
				ID: 5, RunID: 1, Status: "open", Note: "keep me",
			})

			rr := env.request(fullAdmin(), http.MethodPost,
				"/api/upstream-reconciliations/items/5/ack", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("缺 note status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
			}
			if detail, _ := decodeReconcileJSON(t, rr)["detail"].(string); !strings.Contains(detail, "note is required") {
				t.Fatalf("detail = %q，期望包含 note is required", detail)
			}
			if env.store.ackCalls != 0 {
				t.Fatalf("缺 note 仍调用了 AckItem %d 次（note=%q）", env.store.ackCalls, env.store.lastAckNote)
			}
		})
	}
}

func TestSummaryRequiresRunID(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})

	rr := env.request(fullAdmin(), http.MethodGet, "/api/upstream-reconciliations/summary", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 run_id status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
}

// ---------------------------------------------------------------------------
// 补充验收：决策 3/4/5/6 的边界
// ---------------------------------------------------------------------------

func TestTriggerRunWithoutRunnerReturns503(t *testing.T) {
	env := newReconcileTestEnv(t, nil)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/run", map[string]any{})
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配 runner status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}

	// 读端点不依赖 runner：未装配时仍然可用。
	env.store.addRun(store.ReconcileRun{ID: 1, AccountID: 3, Status: "succeeded"})
	rr = env.request(fullAdmin(), http.MethodGet, "/api/upstream-reconciliations/runs", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("未装配 runner 时 runs status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestTriggerRunSkipsCollapsesToSucceeded(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.runner.outcome = reconcile.Outcome{Status: "skipped"}

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/run", map[string]any{})
	if rr.Code != http.StatusOK {
		t.Fatalf("skipped status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := decodeReconcileJSON(t, rr)
	if body["status"] != "succeeded" {
		t.Fatalf("status = %v，期望 succeeded", body["status"])
	}
	if body["items"] != float64(0) || body["alerts"] != float64(0) {
		t.Fatalf("skipped 计数 = items %v / alerts %v，期望 0/0", body["items"], body["alerts"])
	}
	if ids := jsonInts(t, body["run_ids"]); len(ids) != 0 {
		t.Fatalf("run_ids = %v，期望空数组", ids)
	}
}

func TestTriggerRunPassesAccountID(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.runner.outcome = reconcile.Outcome{RunIDs: []int64{7}, Items: 1, Status: "succeeded"}

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/run",
		map[string]any{"account_id": 7})
	if rr.Code != http.StatusOK {
		t.Fatalf("指定账号 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.runner.calls) != 1 {
		t.Fatalf("runner 调用次数 = %d，期望 1", len(env.runner.calls))
	}
	if ids := env.runner.calls[0].accountIDs; len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("accountIDs = %v，期望 [7]", ids)
	}
}

func TestTriggerRunTimeoutReturns504(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	// runner 超时前可能已提交部分 run：Outcome 带 run ids 与错误一起返回。
	env.runner.outcome = reconcile.Outcome{RunIDs: []int64{3, 4}}
	env.runner.err = context.DeadlineExceeded

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/run", map[string]any{})
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("超时 status = %d，期望 504（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	body := decodeReconcileJSON(t, rr)
	if body["type"] != problemTypeGatewayTimeout {
		t.Fatalf("problem type = %v，期望 %s", body["type"], problemTypeGatewayTimeout)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "created run ids: 3, 4") {
		t.Fatalf("detail = %q，期望列出已创建的 run ids（3, 4）", detail)
	}

	// 超时也必须有审计：这些 run 已经落库，不能没有任何记录指回触发者。
	auditArgs := env.audit.concatenatedArgs()
	if !strings.Contains(auditArgs, audit.ActionReconcileRun) || !strings.Contains(auditArgs, "timeout") {
		t.Fatalf("超时未写 reconcile.run 审计（status=timeout）：%s", auditArgs)
	}
	if !strings.Contains(auditArgs, "3") || !strings.Contains(auditArgs, "4") {
		t.Fatalf("超时审计未记录 run ids：%s", auditArgs)
	}
}

func TestSummaryUnknownRunReturns404(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})

	rr := env.request(fullAdmin(), http.MethodGet, "/api/upstream-reconciliations/summary?run_id=99", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的 run status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
}

func TestSummaryReturnsDaysAndAccount(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addRun(store.ReconcileRun{ID: 4, AccountID: 8})
	delta := int64(120)
	revenue := int64(5000)
	env.store.days = []store.SummaryDay{{
		UsageDate: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC),
		ItemCount: 3, AlertCount: 1, WarnCount: 2,
		DeltaTokenBP: &delta, RevenueMinor: &revenue,
	}}

	rr := env.request(fullAdmin(), http.MethodGet, "/api/upstream-reconciliations/summary?run_id=4", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("summary status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := decodeReconcileJSON(t, rr)
	if body["run_id"] != float64(4) || body["account_id"] != float64(8) {
		t.Fatalf("run_id/account_id = %v/%v，期望 4/8", body["run_id"], body["account_id"])
	}
	days, ok := body["days"].([]any)
	if !ok || len(days) != 1 {
		t.Fatalf("days = %v，期望 1 天", body["days"])
	}
	day, _ := days[0].(map[string]any)
	if day["usage_date"] != "2026-10-23" {
		t.Fatalf("usage_date = %v，期望 2026-10-23", day["usage_date"])
	}
	if day["delta_token_bp"] != float64(120) {
		t.Fatalf("delta_token_bp = %v，期望 120", day["delta_token_bp"])
	}
}

func TestListReconcileItemsUnknownRunReturns404(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})

	rr := env.request(fullAdmin(), http.MethodGet, "/api/upstream-reconciliations/runs/99/items", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的 run items status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestListReconcileItemsRejectsInvalidEnum(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addRun(store.ReconcileRun{ID: 1, AccountID: 1})

	rr := env.request(fullAdmin(), http.MethodGet,
		"/api/upstream-reconciliations/runs/1/items?severity=bogus", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 severity status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.request(fullAdmin(), http.MethodGet,
		"/api/upstream-reconciliations/runs/1/items?status=bogus", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 status status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestListReconcileItemsPaginatesAndFilters(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addRun(store.ReconcileRun{ID: 3, AccountID: 1})
	env.store.addItem(store.ReconcileItem{ID: 1, RunID: 3, Severity: "alert", Status: "open"})
	env.store.addItem(store.ReconcileItem{ID: 2, RunID: 3, Severity: "warn", Status: "acknowledged"})

	rr := env.request(fullAdmin(), http.MethodGet,
		"/api/upstream-reconciliations/runs/3/items?severity=alert&p=1&page_size=10", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("items status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := decodeReconcileJSON(t, rr)
	if body["total"] != float64(1) {
		t.Fatalf("total = %v，期望 1", body["total"])
	}
	if body["page"] != float64(1) || body["page_size"] != float64(10) {
		t.Fatalf("items 分页回声 = page %v / page_size %v，期望 1/10", body["page"], body["page_size"])
	}
	if env.store.lastSeverity != "alert" || env.store.lastStatus != "" {
		t.Fatalf("筛选入参 = severity %q / status %q，期望 alert/空", env.store.lastSeverity, env.store.lastStatus)
	}
	if env.store.lastItemsPage != 1 || env.store.lastItemsSz != 10 {
		t.Fatalf("分页入参 = p %d / size %d，期望 1/10", env.store.lastItemsPage, env.store.lastItemsSz)
	}
}

func TestAckItemUpdatesAndAudits(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addItem(store.ReconcileItem{
		ID: 5, RunID: 1, AccountID: 2, Status: "open", Note: "old",
		UpdatedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
	})

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/items/5/ack",
		map[string]any{"status": "resolved", "note": "checked upstream"})
	if rr.Code != http.StatusOK {
		t.Fatalf("ack status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := decodeReconcileJSON(t, rr)
	if body["id"] != float64(5) || body["status"] != "resolved" || body["note"] != "checked upstream" {
		t.Fatalf("ack 响应 = %v，期望 id=5 status=resolved note=checked upstream", body)
	}
	// updated_at 必须是 AckItem 之后重新读取的值，而不是旧的 2026-10-20。
	if body["updated_at"] != "2026-10-24T12:30:00Z" {
		t.Fatalf("updated_at = %v，期望重读后的 2026-10-24T12:30:00Z", body["updated_at"])
	}
	if env.store.ackCalls != 1 || env.store.lastAckID != 5 ||
		env.store.lastAckStatus != "resolved" || env.store.lastAckNote != "checked upstream" {
		t.Fatalf("AckItem 入参 = id %d status %q note %q，期望 5/resolved/checked upstream",
			env.store.lastAckID, env.store.lastAckStatus, env.store.lastAckNote)
	}
	if !strings.Contains(env.audit.concatenatedArgs(), audit.ActionReconcileItemAck) {
		t.Fatalf("审计未记录 %s：%s", audit.ActionReconcileItemAck, env.audit.concatenatedArgs())
	}
}

func TestAckItemDefaultsToAcknowledged(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addItem(store.ReconcileItem{ID: 6, RunID: 1, Status: "open"})

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/items/6/ack",
		map[string]any{"note": ""})
	if rr.Code != http.StatusOK {
		t.Fatalf("缺省 status ack = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	if env.store.lastAckStatus != "acknowledged" {
		t.Fatalf("缺省 status = %q，期望 acknowledged", env.store.lastAckStatus)
	}
}

func TestAckItemRejectsLongNote(t *testing.T) {
	// 上限按 Unicode 码点计算，不是字节：2000 个 CJK 字符是 6000 字节，
	// 契约允许，用 len() 会误拒。这里把两个边界都钉住。
	cases := []struct {
		name string
		note string
		want int
	}{
		{"2000 ASCII", strings.Repeat("x", 2000), http.StatusOK},
		{"2001 ASCII", strings.Repeat("x", 2001), http.StatusUnprocessableEntity},
		{"2000 CJK runes", strings.Repeat("对", 2000), http.StatusOK},
		{"2001 CJK runes", strings.Repeat("对", 2001), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcileTestEnv(t, &fakeReconcileRunner{})
			env.store.addItem(store.ReconcileItem{ID: 7, RunID: 1, Status: "open"})

			rr := env.request(fullAdmin(), http.MethodPost,
				"/api/upstream-reconciliations/items/7/ack",
				map[string]any{"status": "acknowledged", "note": tc.note})
			if rr.Code != tc.want {
				t.Fatalf("note 长度 %d 字节 status = %d，期望 %d（body=%q）",
					len(tc.note), rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusUnprocessableEntity {
				if env.store.ackCalls != 0 {
					t.Fatalf("校验失败仍调用了 AckItem %d 次", env.store.ackCalls)
				}
				return
			}
			if env.store.ackCalls != 1 || env.store.lastAckNote != tc.note {
				t.Fatalf("AckItem 入参 = 调用 %d 次 / note 长度 %d，期望 1 次 / %d 字节",
					env.store.ackCalls, len(env.store.lastAckNote), len(tc.note))
			}
		})
	}
}

func TestAckUnknownItemReturns404(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/items/99/ack",
		map[string]any{"status": "acknowledged", "note": ""})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在的明细 status = %d，期望 404（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestListReconcileRunsFiltersByAccount(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})
	env.store.addRun(store.ReconcileRun{ID: 1, AccountID: 1})
	env.store.addRun(store.ReconcileRun{ID: 2, AccountID: 2})

	rr := env.request(fullAdmin(), http.MethodGet,
		"/api/upstream-reconciliations/runs?account_id=2&p=1&page_size=20", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("runs status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body := decodeReconcileJSON(t, rr)
	if body["total"] != float64(1) {
		t.Fatalf("total = %v，期望 1", body["total"])
	}
	if body["page"] != float64(1) || body["page_size"] != float64(20) {
		t.Fatalf("分页回声 = page %v / page_size %v，期望 1/20", body["page"], body["page_size"])
	}
	if env.store.lastAccountID == nil || *env.store.lastAccountID != 2 {
		t.Fatalf("account_id 筛选入参 = %v，期望 2", env.store.lastAccountID)
	}

	// page_size 超过上限时静默归一到 100，响应回声也必须是归一后的值。
	rr = env.request(fullAdmin(), http.MethodGet,
		"/api/upstream-reconciliations/runs?p=2&page_size=500", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("page_size=500 status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
	body = decodeReconcileJSON(t, rr)
	if body["page"] != float64(2) || body["page_size"] != float64(100) {
		t.Fatalf("归一后分页回声 = page %v / page_size %v，期望 2/100", body["page"], body["page_size"])
	}
	if env.store.lastRunsPage != 2 || env.store.lastRunsSz != 100 {
		t.Fatalf("仓储分页入参 = p %d / size %d，期望 2/100", env.store.lastRunsPage, env.store.lastRunsSz)
	}
}

func TestTriggerRunRejectsNonPositiveAccountID(t *testing.T) {
	env := newReconcileTestEnv(t, &fakeReconcileRunner{})

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-reconciliations/run",
		map[string]any{"account_id": 0})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("account_id=0 status = %d，期望 400（body=%q）", rr.Code, rr.Body.String())
	}
	if len(env.runner.calls) != 0 {
		t.Fatalf("校验失败仍调用了 runner %d 次", len(env.runner.calls))
	}
}
