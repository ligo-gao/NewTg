package httpapi

// 本文件是 Task 8「上游账号」端点的验收：权限门（settings:manage）、
// 字段校验（时区 422）、删除引用冲突（409）与快照 upsert 的审计/溯源。
//
// 依赖复用 handlers_users_test.go 的 adminAuditCapture/fullAdmin/limitedAdmin/
// plainUser 与 middleware_test.go 的 serve。所有仓储都是内存假实现，不触 PG。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// ---------------------------------------------------------------------------
// 假仓储
// ---------------------------------------------------------------------------

// fakeUpstreamAccountRepo 是 UpstreamAccountRepo 的内存实现。
type fakeUpstreamAccountRepo struct {
	mu sync.Mutex

	accounts map[int64]store.UpstreamAccount
	nextID   int64

	getErr    error
	createErr error
	updateErr error
	deleteErr error
	listErr   error

	listCalls   int
	getCalls    int
	createCalls int
	updateCalls int
	deleteCalls int
	lastCreate  store.UpstreamAccount
	lastPatch   store.UpstreamAccountPatch
}

// callCount 汇总所有仓储调用次数，供 403 用例断言「鉴权层拦下、业务层未触达」。
func (r *fakeUpstreamAccountRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls + r.getCalls + r.createCalls + r.updateCalls + r.deleteCalls
}

func newFakeUpstreamAccountRepo() *fakeUpstreamAccountRepo {
	return &fakeUpstreamAccountRepo{
		accounts: make(map[int64]store.UpstreamAccount),
		nextID:   1,
	}
}

// seed 插入一条账号并返回带 id 的记录（测试固定时间戳，便于乐观锁断言）。
func (r *fakeUpstreamAccountRepo) seed(a store.UpstreamAccount) store.UpstreamAccount {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.ID == 0 {
		a.ID = r.nextID
		r.nextID++
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = a.CreatedAt
	}
	if a.Currency == "" {
		a.Currency = "CNY"
	}
	if a.Timezone == "" {
		a.Timezone = "Asia/Shanghai"
	}
	if a.Status == 0 {
		a.Status = 1
	}
	r.accounts[a.ID] = a
	return a
}

func (r *fakeUpstreamAccountRepo) List(_ context.Context, status *int16, page, pageSize int) ([]store.UpstreamAccount, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	if r.listErr != nil {
		return nil, 0, r.listErr
	}
	items := make([]store.UpstreamAccount, 0, len(r.accounts))
	for id := int64(1); id < r.nextID; id++ {
		rec, ok := r.accounts[id]
		if !ok {
			continue
		}
		if status != nil && rec.Status != *status {
			continue
		}
		items = append(items, rec)
	}
	total := int64(len(items))
	start := (page - 1) * pageSize
	if start >= len(items) {
		return []store.UpstreamAccount{}, total, nil
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], total, nil
}

func (r *fakeUpstreamAccountRepo) Get(_ context.Context, id int64) (*store.UpstreamAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.getCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	rec, ok := r.accounts[id]
	if !ok {
		return nil, store.ErrUpstreamAccountNotFound
	}
	out := rec
	return &out, nil
}

func (r *fakeUpstreamAccountRepo) Create(_ context.Context, a store.UpstreamAccount) (*store.UpstreamAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.createCalls++
	r.lastCreate = a
	if r.createErr != nil {
		return nil, r.createErr
	}
	for _, rec := range r.accounts {
		if rec.Name == a.Name {
			return nil, store.ErrNameConflict
		}
	}
	if a.Currency == "" {
		a.Currency = "CNY"
	}
	if a.Timezone == "" {
		a.Timezone = "Asia/Shanghai"
	}
	if a.Status == 0 {
		a.Status = 1
	}
	a.ID = r.nextID
	r.nextID++
	a.CreatedAt = time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	a.UpdatedAt = a.CreatedAt
	r.accounts[a.ID] = a
	out := a
	return &out, nil
}

func (r *fakeUpstreamAccountRepo) Update(_ context.Context, id int64, p store.UpstreamAccountPatch) (*store.UpstreamAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	r.lastPatch = p
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	rec, ok := r.accounts[id]
	if !ok {
		return nil, store.ErrUpstreamAccountNotFound
	}
	if !p.ExpectedUpdatedAt.IsZero() && !p.ExpectedUpdatedAt.Equal(rec.UpdatedAt) {
		return nil, store.ErrUpstreamAccountConflict
	}
	if p.Name != nil {
		rec.Name = *p.Name
	}
	if p.Provider != nil {
		rec.Provider = *p.Provider
	}
	if p.Currency != nil {
		rec.Currency = *p.Currency
	}
	if p.Timezone != nil {
		rec.Timezone = *p.Timezone
	}
	if p.ConsoleURLSet {
		rec.ConsoleURL = p.ConsoleURL
	}
	if p.Status != nil {
		rec.Status = *p.Status
	}
	if p.Note != nil {
		rec.Note = *p.Note
	}
	rec.UpdatedAt = rec.UpdatedAt.Add(time.Second)
	r.accounts[id] = rec
	out := rec
	return &out, nil
}

func (r *fakeUpstreamAccountRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteCalls++
	if r.deleteErr != nil {
		return r.deleteErr
	}
	if _, ok := r.accounts[id]; !ok {
		return store.ErrUpstreamAccountNotFound
	}
	delete(r.accounts, id)
	return nil
}

// fakeUpstreamSnapshotRepo 是 UpstreamSnapshotRepo 的内存实现。
type fakeUpstreamSnapshotRepo struct {
	mu sync.Mutex

	upsertCalls int
	lastItems   []store.UpstreamSnapshot
	upsertErr   error

	listErr     error
	listAccount int64
	listFrom    time.Time
	listTo      time.Time
	listRows    []store.UpstreamSnapshot
}

func newFakeUpstreamSnapshotRepo() *fakeUpstreamSnapshotRepo {
	return &fakeUpstreamSnapshotRepo{}
}

func (r *fakeUpstreamSnapshotRepo) Upsert(_ context.Context, items []store.UpstreamSnapshot) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsertCalls++
	r.lastItems = append([]store.UpstreamSnapshot(nil), items...)
	if r.upsertErr != nil {
		return 0, r.upsertErr
	}
	return len(items), nil
}

func (r *fakeUpstreamSnapshotRepo) ListRange(_ context.Context, accountID int64, from, to time.Time) ([]store.UpstreamSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listAccount, r.listFrom, r.listTo = accountID, from, to
	if r.listErr != nil {
		return nil, r.listErr
	}
	return append([]store.UpstreamSnapshot(nil), r.listRows...), nil
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

type upstreamTestEnv struct {
	t         *testing.T
	accounts  *fakeUpstreamAccountRepo
	snapshots *fakeUpstreamSnapshotRepo
	audit     *adminAuditCapture
	now       time.Time
	router    http.Handler
}

// newUpstreamTestEnv 装配账号 + 快照端点（同一 authz，边角与生产装配一致）。
func newUpstreamTestEnv(t *testing.T) *upstreamTestEnv {
	t.Helper()
	accounts := newFakeUpstreamAccountRepo()
	snapshots := newFakeUpstreamSnapshotRepo()
	capture := &adminAuditCapture{}
	authz := rbac.NewAuthzMiddleware()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	accountHandlers, err := NewUpstreamAccountHandlers(UpstreamAccountHandlerConfig{
		Repo: accounts, Authz: authz, Audit: audit.New(capture), Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewUpstreamAccountHandlers: %v", err)
	}
	snapshotHandlers, err := NewUpstreamSnapshotHandlers(UpstreamSnapshotHandlerConfig{
		Repo: snapshots, Accounts: accounts, Authz: authz, Audit: audit.New(capture),
		Logger: logger, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewUpstreamSnapshotHandlers: %v", err)
	}

	mux := http.NewServeMux()
	accountHandlers.RegisterRoutes(mux, nil)
	snapshotHandlers.RegisterRoutes(mux, nil)
	return &upstreamTestEnv{
		t: t, accounts: accounts, snapshots: snapshots, audit: capture, now: now, router: mux,
	}
}

func (e *upstreamTestEnv) request(p *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
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

// decodeUpstreamProblem 解码 RFC 7807 响应体（保留扩展成员 fields）。
func decodeUpstreamProblem(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("problem JSON 非法: %v（body=%q）", err, rr.Body.String())
	}
	return body
}

func problemFields(problem map[string]any) []string {
	raw, _ := problem["fields"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if field, ok := item.(string); ok {
			out = append(out, field)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 验收测试（brief Step 1）
// ---------------------------------------------------------------------------

func TestListUpstreamAccountsRequiresSettingsManage(t *testing.T) {
	env := newUpstreamTestEnv(t)
	env.accounts.seed(store.UpstreamAccount{Name: "openai-main", Provider: "openai"})

	rr := env.request(limitedAdmin(rbac.PermUsersRead), http.MethodGet, "/api/upstream-accounts", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}

	// plain user 也必须 403（角色是外层门）。
	rr = env.request(plainUser(), http.MethodGet, "/api/upstream-accounts", nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("普通用户 status = %d，期望 403（body=%q）", rr.Code, rr.Body.String())
	}

	rr = env.request(limitedAdmin(rbac.PermSettingsManage), http.MethodGet, "/api/upstream-accounts", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("持 settings:manage status = %d，期望 200（body=%q）", rr.Code, rr.Body.String())
	}
}

func TestCreateUpstreamAccountRejectsInvalidTimezone(t *testing.T) {
	env := newUpstreamTestEnv(t)

	rr := env.request(fullAdmin(), http.MethodPost, "/api/upstream-accounts", map[string]any{
		"name":     "bad-tz",
		"provider": "openai",
		"timezone": "Mars/Olympus",
	})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法时区 status = %d，期望 422（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	problem := decodeUpstreamProblem(t, rr)
	if problemType, _ := problem["type"].(string); !strings.HasSuffix(problemType, "/invalid-field") {
		t.Fatalf("problem.type = %v，期望以 /invalid-field 结尾", problem["type"])
	}
	if fields := problemFields(problem); !containsString(fields, "timezone") {
		t.Fatalf("problem.fields = %v，期望包含 timezone", fields)
	}
	if env.accounts.createCalls != 0 {
		t.Fatalf("校验失败仍调用了仓储 %d 次", env.accounts.createCalls)
	}
}

func TestDeleteUpstreamAccountInUseReturns409(t *testing.T) {
	env := newUpstreamTestEnv(t)
	rec := env.accounts.seed(store.UpstreamAccount{Name: "bound", Provider: "openai"})
	env.accounts.deleteErr = store.ErrUpstreamAccountInUse

	rr := env.request(fullAdmin(), http.MethodDelete, "/api/upstream-accounts/"+strconv.FormatInt(rec.ID, 10), nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("删除被引用账号 status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
}

// TestUpstreamAccountRoutesRequireSettingsManage 覆盖全部四个账号路由的鉴权门。
// 只测 GET 会漏掉写路由：POST/PATCH/DELETE 各自登记了独立的 action 与字段
// 策略，任一路由忘套 authz.Handler 都必须在 CI 里失败。断言仓储零调用，
// 排除「路由确实进了业务层、只是碰巧后置失败」的假阳性。
func TestUpstreamAccountRoutesRequireSettingsManage(t *testing.T) {
	routes := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list", http.MethodGet, "/api/upstream-accounts", nil},
		{"create", http.MethodPost, "/api/upstream-accounts",
			map[string]any{"name": "gate", "provider": "openai"}},
		{"update", http.MethodPatch, "/api/upstream-accounts/1",
			map[string]any{"expected_updated_at": time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}},
		{"delete", http.MethodDelete, "/api/upstream-accounts/1", nil},
	}
	principals := []struct {
		name string
		p    *auth.Principal
	}{
		// 一个有别的权限、但缺 settings:manage 的受限管理员。
		{"limited admin", limitedAdmin(rbac.PermUsersRead)},
		{"plain user", plainUser()},
	}

	for _, route := range routes {
		for _, who := range principals {
			t.Run(route.name+"/"+who.name, func(t *testing.T) {
				env := newUpstreamTestEnv(t)
				// 目标行真实存在，确保若鉴权缺失 DELETE/PATCH 会成功触达仓储。
				env.accounts.seed(store.UpstreamAccount{Name: "gate", Provider: "openai"})

				rr := env.request(who.p, route.method, route.path, route.body)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("%s 缺 settings:manage status = %d，期望 403（body=%q）",
						route.method, rr.Code, rr.Body.String())
				}
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
					t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
				}
				if got := env.accounts.callCount(); got != 0 {
					t.Fatalf("鉴权拒绝后仓储仍被调用 %d 次", got)
				}
			})
		}
	}
}

// TestUpdateUpstreamAccountPatchTriState 固定 PATCH 的三态语义与错误映射：
// 缺省 = 不修改、console_url 显式 null = 清空、present-but-empty 时区 = 422、
// 乐观锁失配 = 409、重名 409 的 detail 必须是账号口径（不得透出渠道文案）。
func TestUpdateUpstreamAccountPatchTriState(t *testing.T) {
	consoleURL := "https://console.example.com/admin"

	seedAccount := func(env *upstreamTestEnv) store.UpstreamAccount {
		return env.accounts.seed(store.UpstreamAccount{
			Name:       "acct-main",
			Provider:   "openai",
			Currency:   "USD",
			Timezone:   "America/New_York",
			ConsoleURL: &consoleURL,
			Note:       "original note",
		})
	}

	tests := []struct {
		name               string
		setup              func(*upstreamTestEnv) store.UpstreamAccount
		body               func(store.UpstreamAccount) map[string]any
		wantStatus         int
		wantFields         []string
		wantDetailContains string
		wantDetailExcludes string
		check              func(*testing.T, *upstreamTestEnv, store.UpstreamAccount)
	}{
		{
			name:  "absent fields are left unchanged",
			setup: seedAccount,
			body: func(rec store.UpstreamAccount) map[string]any {
				return map[string]any{
					"expected_updated_at": rec.UpdatedAt,
					"note":                "changed note",
				}
			},
			wantStatus: http.StatusOK,
			check: func(t *testing.T, env *upstreamTestEnv, before store.UpstreamAccount) {
				t.Helper()
				got, err := env.accounts.Get(context.Background(), before.ID)
				if err != nil {
					t.Fatalf("读取更新后的账号: %v", err)
				}
				if got.Note != "changed note" {
					t.Fatalf("note = %q，期望 changed note", got.Note)
				}
				if got.Provider != before.Provider || got.Currency != before.Currency ||
					got.Timezone != before.Timezone {
					t.Fatalf("未提交字段被改动: got provider=%q currency=%q timezone=%q, before provider=%q currency=%q timezone=%q",
						got.Provider, got.Currency, got.Timezone,
						before.Provider, before.Currency, before.Timezone)
				}
				if got.ConsoleURL == nil || *got.ConsoleURL != consoleURL {
					t.Fatalf("未提交的 console_url 被改动: %v", got.ConsoleURL)
				}
				patch := env.accounts.lastPatch
				if patch.Name != nil || patch.Provider != nil || patch.Currency != nil ||
					patch.Timezone != nil || patch.Status != nil || patch.ConsoleURLSet {
					t.Fatalf("缺省字段泄漏进 patch: %+v", patch)
				}
				if patch.Note == nil || *patch.Note != "changed note" {
					t.Fatalf("patch.note = %v，期望 changed note", patch.Note)
				}
			},
		},
		{
			name:  "console_url null clears the stored url",
			setup: seedAccount,
			body: func(rec store.UpstreamAccount) map[string]any {
				return map[string]any{
					"expected_updated_at": rec.UpdatedAt,
					"console_url":         nil,
				}
			},
			wantStatus: http.StatusOK,
			check: func(t *testing.T, env *upstreamTestEnv, before store.UpstreamAccount) {
				t.Helper()
				got, err := env.accounts.Get(context.Background(), before.ID)
				if err != nil {
					t.Fatalf("读取更新后的账号: %v", err)
				}
				if got.ConsoleURL != nil {
					t.Fatalf("console_url = %q，期望清空为 nil", *got.ConsoleURL)
				}
				if !env.accounts.lastPatch.ConsoleURLSet || env.accounts.lastPatch.ConsoleURL != nil {
					t.Fatalf("patch console_url = set:%v value:%v，期望 set:true value:nil",
						env.accounts.lastPatch.ConsoleURLSet, env.accounts.lastPatch.ConsoleURL)
				}
			},
		},
		{
			name:  "absent console_url preserves the stored url",
			setup: seedAccount,
			body: func(rec store.UpstreamAccount) map[string]any {
				return map[string]any{"expected_updated_at": rec.UpdatedAt}
			},
			wantStatus: http.StatusOK,
			check: func(t *testing.T, env *upstreamTestEnv, before store.UpstreamAccount) {
				t.Helper()
				got, err := env.accounts.Get(context.Background(), before.ID)
				if err != nil {
					t.Fatalf("读取更新后的账号: %v", err)
				}
				if got.ConsoleURL == nil || *got.ConsoleURL != consoleURL {
					t.Fatalf("console_url = %v，期望保持不变 %q", got.ConsoleURL, consoleURL)
				}
				if env.accounts.lastPatch.ConsoleURLSet {
					t.Fatal("缺省 console_url 不得设置 ConsoleURLSet")
				}
			},
		},
		{
			name:  "present but empty timezone is rejected before the store",
			setup: seedAccount,
			body: func(rec store.UpstreamAccount) map[string]any {
				return map[string]any{"expected_updated_at": rec.UpdatedAt, "timezone": ""}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantFields: []string{"timezone"},
			check: func(t *testing.T, env *upstreamTestEnv, _ store.UpstreamAccount) {
				t.Helper()
				if got := env.accounts.callCount(); got != 0 {
					t.Fatalf("空时区仍调用了仓储 %d 次", got)
				}
			},
		},
		{
			name:  "stale expected_updated_at is a conflict",
			setup: seedAccount,
			body: func(rec store.UpstreamAccount) map[string]any {
				return map[string]any{
					"expected_updated_at": rec.UpdatedAt.Add(-time.Second),
					"note":                "should not persist",
				}
			},
			wantStatus: http.StatusConflict,
			check: func(t *testing.T, env *upstreamTestEnv, before store.UpstreamAccount) {
				t.Helper()
				got, err := env.accounts.Get(context.Background(), before.ID)
				if err != nil {
					t.Fatalf("读取账号: %v", err)
				}
				if got.Note != before.Note {
					t.Fatalf("冲突更新不得写入: note = %q，before = %q", got.Note, before.Note)
				}
				if env.accounts.updateCalls != 1 {
					t.Fatalf("updateCalls = %d，期望 1（乐观锁由仓储拒绝）", env.accounts.updateCalls)
				}
			},
		},
		{
			name: "duplicate name keeps the account-flavoured detail",
			setup: func(env *upstreamTestEnv) store.UpstreamAccount {
				rec := env.accounts.seed(store.UpstreamAccount{Name: "acct-main", Provider: "openai"})
				env.accounts.seed(store.UpstreamAccount{Name: "already-taken", Provider: "openai"})
				env.accounts.updateErr = store.ErrNameConflict
				return rec
			},
			body: func(rec store.UpstreamAccount) map[string]any {
				return map[string]any{
					"expected_updated_at": rec.UpdatedAt,
					"name":                "already-taken",
				}
			},
			wantStatus:         http.StatusConflict,
			wantDetailContains: "upstream account name already exists",
			wantDetailExcludes: "channel",
		},
		{
			name:  "unknown id is 404",
			setup: func(*upstreamTestEnv) store.UpstreamAccount { return store.UpstreamAccount{ID: 9999} },
			body: func(store.UpstreamAccount) map[string]any {
				return map[string]any{"expected_updated_at": time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
			},
			wantStatus: http.StatusNotFound,
			check: func(t *testing.T, env *upstreamTestEnv, _ store.UpstreamAccount) {
				t.Helper()
				if env.accounts.updateCalls != 0 {
					t.Fatalf("不存在的 id 仍调用了 Update %d 次", env.accounts.updateCalls)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newUpstreamTestEnv(t)
			before := tc.setup(env)
			rr := env.request(fullAdmin(), http.MethodPatch,
				"/api/upstream-accounts/"+strconv.FormatInt(before.ID, 10), tc.body(before))
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d，期望 %d（body=%q）", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantStatus >= 400 {
				if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
					t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
				}
				problem := decodeUpstreamProblem(t, rr)
				fields := problemFields(problem)
				for _, want := range tc.wantFields {
					if !containsString(fields, want) {
						t.Fatalf("problem.fields = %v，期望包含 %s", fields, want)
					}
				}
				detail, _ := problem["detail"].(string)
				if tc.wantDetailContains != "" && !strings.Contains(detail, tc.wantDetailContains) {
					t.Fatalf("problem.detail = %q，期望包含 %q", detail, tc.wantDetailContains)
				}
				if tc.wantDetailExcludes != "" && strings.Contains(detail, tc.wantDetailExcludes) {
					t.Fatalf("problem.detail = %q，不得包含 %q", detail, tc.wantDetailExcludes)
				}
			}
			if tc.check != nil {
				tc.check(t, env, before)
			}
		})
	}
}
