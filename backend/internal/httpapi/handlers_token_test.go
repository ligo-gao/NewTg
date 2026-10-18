package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/tokenstore"
)

const testUserID int64 = 42

// fakeTokenService 是 TokenService 的内存实现：handler 单测不触 PG/Redis（GC-8）。
type fakeTokenService struct {
	mu sync.Mutex

	createCalls int
	listCalls   int

	createFn     func(ctx context.Context, userID int64, in tokenstore.CreateInput) ([]tokenstore.Created, error)
	listFn       func(ctx context.Context, userID int64, f tokenstore.ListFilter) ([]*tokenstore.Token, int64, error)
	getFn        func(ctx context.Context, userID, id int64) (*tokenstore.Token, error)
	updateFn     func(ctx context.Context, userID, id int64, in tokenstore.UpdateInput) (*tokenstore.Token, error)
	deleteFn     func(ctx context.Context, userID, id int64, expected *time.Time) error
	enableFn     func(ctx context.Context, userID, id int64) (*tokenstore.Token, error)
	disableFn    func(ctx context.Context, userID, id int64) (*tokenstore.Token, error)
	resetFn      func(ctx context.Context, userID, id int64) (string, string, *tokenstore.Token, error)
	batchFn      func(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error)
	autoGroupsFn func(ctx context.Context, userID int64) ([]tokenstore.AutoGroup, int, error)
	resolveFn    func(ctx context.Context, keyHash string) (*auth.TokenMeta, error)
}

func (f *fakeTokenService) Create(ctx context.Context, userID int64, in tokenstore.CreateInput) ([]tokenstore.Created, error) {
	f.mu.Lock()
	f.createCalls++
	fn := f.createFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, in)
	}
	return nil, nil
}

func (f *fakeTokenService) List(ctx context.Context, userID int64, filter tokenstore.ListFilter) ([]*tokenstore.Token, int64, error) {
	f.mu.Lock()
	f.listCalls++
	fn := f.listFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, userID, filter)
	}
	return nil, 0, nil
}

func (f *fakeTokenService) Get(ctx context.Context, userID, id int64) (*tokenstore.Token, error) {
	if f.getFn != nil {
		return f.getFn(ctx, userID, id)
	}
	return nil, tokenstore.ErrNotFound
}

func (f *fakeTokenService) Update(ctx context.Context, userID, id int64, in tokenstore.UpdateInput) (*tokenstore.Token, error) {
	if f.updateFn != nil {
		return f.updateFn(ctx, userID, id, in)
	}
	return nil, tokenstore.ErrNotFound
}

func (f *fakeTokenService) Delete(ctx context.Context, userID, id int64, expected *time.Time) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, userID, id, expected)
	}
	return tokenstore.ErrNotFound
}

func (f *fakeTokenService) Enable(ctx context.Context, userID, id int64) (*tokenstore.Token, error) {
	if f.enableFn != nil {
		return f.enableFn(ctx, userID, id)
	}
	return nil, tokenstore.ErrNotFound
}

func (f *fakeTokenService) Disable(ctx context.Context, userID, id int64) (*tokenstore.Token, error) {
	if f.disableFn != nil {
		return f.disableFn(ctx, userID, id)
	}
	return nil, tokenstore.ErrNotFound
}

func (f *fakeTokenService) ResetKey(ctx context.Context, userID, id int64) (string, string, *tokenstore.Token, error) {
	if f.resetFn != nil {
		return f.resetFn(ctx, userID, id)
	}
	return "", "", nil, tokenstore.ErrNotFound
}

func (f *fakeTokenService) BatchEnable(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error) {
	if f.batchFn != nil {
		return f.batchFn(ctx, userID, ids)
	}
	return 0, nil, nil
}

func (f *fakeTokenService) BatchDisable(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error) {
	if f.batchFn != nil {
		return f.batchFn(ctx, userID, ids)
	}
	return 0, nil, nil
}

func (f *fakeTokenService) BatchDelete(ctx context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error) {
	if f.batchFn != nil {
		return f.batchFn(ctx, userID, ids)
	}
	return 0, nil, nil
}

func (f *fakeTokenService) AutoGroups(ctx context.Context, userID int64) ([]tokenstore.AutoGroup, int, error) {
	if f.autoGroupsFn != nil {
		return f.autoGroupsFn(ctx, userID)
	}
	return nil, tokenstore.DefaultMaxAutoGroups, nil
}

func (f *fakeTokenService) Resolve(ctx context.Context, keyHash string) (*auth.TokenMeta, error) {
	if f.resolveFn != nil {
		return f.resolveFn(ctx, keyHash)
	}
	return nil, tokenstore.ErrInvalidToken
}

func (f *fakeTokenService) CrossGroupRetryMaxGroups() int {
	return tokenstore.DefaultCrossGroupRetryMaxGroups
}

// captureExec 捕获审计 INSERT 的实参。
type captureExec struct {
	mu    sync.Mutex
	calls [][]any
}

func (c *captureExec) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, args)
	return captureResult(1), nil
}

func (c *captureExec) snapshot() [][]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]any, len(c.calls))
	copy(out, c.calls)
	return out
}

type captureResult int64

func (r captureResult) LastInsertId() (int64, error) { return 0, nil }
func (r captureResult) RowsAffected() (int64, error) { return int64(r), nil }

type tokenTestEnv struct {
	handlers *TokenHandlers
	svc      *fakeTokenService
	audit    *captureExec
	mux      *http.ServeMux
	mr       *miniredis.Miniredis
}

func newTokenTestEnv(t *testing.T) *tokenTestEnv {
	t.Helper()
	svc := &fakeTokenService{}
	capt := &captureExec{}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	h := NewTokenHandlers(svc, audit.New(capt), rdb)
	mux := http.NewServeMux()
	h.RegisterTokenRoutes(mux, nil)
	return &tokenTestEnv{handlers: h, svc: svc, audit: capt, mux: mux, mr: mr}
}

// do 发一个已注入 Principal 的请求（生产上由 SessionAuth 注入）。
func (e *tokenTestEnv) do(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	r = r.WithContext(auth.WithPrincipal(r.Context(), &auth.Principal{UserID: testUserID}))
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, r)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
}

func sampleToken() *tokenstore.Token {
	remain := int64(1234)
	return &tokenstore.Token{
		ID:              7,
		UserID:          testUserID,
		Name:            "prod",
		Status:          tokenstore.StatusEnabled,
		Group:           "default",
		RemainQuota:     &remain,
		ModelLimits:     []string{"gpt-x"},
		AllowIPs:        []string{"10.0.0.0/8"},
		AutoGroups:      []string{"g1"},
		CrossGroupRetry: true,
		CreatedAt:       time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC),
	}
}

// TestTokenCreateReturnsKeyOnceAndIsIdempotent 覆盖 brief：
// key 只在创建响应出现一次；同一 Idempotency-Key 重放不重复创建。
func TestTokenCreateReturnsKeyOnceAndIsIdempotent(t *testing.T) {
	env := newTokenTestEnv(t)
	tok := sampleToken()
	env.svc.createFn = func(_ context.Context, userID int64, in tokenstore.CreateInput) ([]tokenstore.Created, error) {
		if userID != testUserID {
			t.Fatalf("userID = %d，期望 %d（必须来自 Principal）", userID, testUserID)
		}
		return []tokenstore.Created{{Token: tok, Key: "nkt-" + strings.Repeat("A", 48)}}, nil
	}

	body := `{"name":"prod","group":"default","unlimited_quota":true}`
	headers := map[string]string{"Idempotency-Key": "idem-1"}

	first := env.do(http.MethodPost, "/api/tokens", body, headers)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%q）", first.Code, first.Body.String())
	}
	var resp struct {
		Created []map[string]any `json:"created"`
	}
	decodeJSON(t, first, &resp)
	if len(resp.Created) != 1 {
		t.Fatalf("created = %d，期望 1", len(resp.Created))
	}
	if got := resp.Created[0]["key"]; got != "nkt-"+strings.Repeat("A", 48) {
		t.Fatalf("创建响应缺少明文 key: %v", got)
	}
	if got := resp.Created[0]["key_masked"]; got != "nkt-"+strings.Repeat("•", 24) {
		t.Fatalf("key_masked = %v", got)
	}

	// 重放：结果逐字节一致，且没有第二次 Create。
	second := env.do(http.MethodPost, "/api/tokens", body, headers)
	if second.Code != http.StatusOK || second.Body.String() != first.Body.String() {
		t.Fatalf("幂等重放响应不一致：first=%q second=%q", first.Body.String(), second.Body.String())
	}
	if env.svc.createCalls != 1 {
		t.Fatalf("Create 调用 = %d，期望 1（幂等键必须去重）", env.svc.createCalls)
	}

	// 不同幂等键 → 第二次创建。
	third := env.do(http.MethodPost, "/api/tokens", body, map[string]string{"Idempotency-Key": "idem-2"})
	if third.Code != http.StatusOK {
		t.Fatalf("新幂等键 status = %d", third.Code)
	}
	if env.svc.createCalls != 2 {
		t.Fatalf("Create 调用 = %d，期望 2", env.svc.createCalls)
	}

	// 审计：after 快照存在且不含 key/hash。
	rows := env.audit.snapshot()
	if len(rows) != 2 {
		t.Fatalf("审计行 = %d，期望 2（每次真实创建一条）", len(rows))
	}
	for _, args := range rows {
		if len(args) != 12 {
			t.Fatalf("审计实参 = %d，期望 12", len(args))
		}
		if args[8] != "success" || args[2] != "operation" {
			t.Errorf("审计 result/category = %v/%v，期望 success/operation", args[8], args[2])
		}
		after, _ := json.Marshal(args[11])
		if !strings.Contains(string(after), "prod") {
			t.Errorf("审计 after 快照缺少令牌名: %s", after)
		}
		if strings.Contains(string(after), strings.Repeat("A", 48)) || strings.Contains(string(after), "key_hash") {
			t.Errorf("审计 after 快照泄露密钥材料: %s", after)
		}
	}
}

// TestTokenCreateValidationErrorMapsTo400：字段级错误 → RFC 7807 400 + field。
func TestTokenCreateValidationErrorMapsTo400(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return nil, &tokenstore.ValidationError{Field: "auto_groups", Message: "自动分组至少需要一个分组"}
	}
	rr := env.do(http.MethodPost, "/api/tokens", `{"name":"x","group":"auto"}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var p map[string]any
	decodeJSON(t, rr, &p)
	if p["field"] != "auto_groups" {
		t.Fatalf("problem.field = %v，期望 auto_groups", p["field"])
	}
}

// TestTokenListNeverReturnsKeyMaterial 覆盖 R-08/GC-6：
// 列表响应只有掩码，没有明文 key 或 key_hash；筛选参数透传。
func TestTokenListNeverReturnsKeyMaterial(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.listFn = func(_ context.Context, userID int64, f tokenstore.ListFilter) ([]*tokenstore.Token, int64, error) {
		if userID != testUserID {
			t.Fatalf("userID = %d", userID)
		}
		if f.Keyword != "prod" || f.Status != "expired" || f.Page != 2 || f.PageSize != 50 {
			t.Fatalf("筛选参数未透传: %+v", f)
		}
		return []*tokenstore.Token{sampleToken()}, 1, nil
	}
	rr := env.do(http.MethodGet, "/api/tokens?keyword=prod&status=expired&p=2&page_size=50", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []map[string]any `json:"items"`
		Total int64            `json:"total"`
		Page  int              `json:"page"`
		Size  int              `json:"page_size"`
	}
	decodeJSON(t, rr, &resp)
	if resp.Total != 1 || resp.Page != 2 || resp.Size != 50 || len(resp.Items) != 1 {
		t.Fatalf("分页信封错误: %+v", resp)
	}
	item := resp.Items[0]
	if _, ok := item["key"]; ok {
		t.Fatal("列表响应不得包含 key 字段")
	}
	if _, ok := item["key_hash"]; ok {
		t.Fatal("列表响应不得包含 key_hash 字段")
	}
	if item["key_masked"] != "nkt-"+strings.Repeat("•", 24) {
		t.Fatalf("key_masked = %v", item["key_masked"])
	}
	if item["status"] != tokenstore.StatusEnabled {
		t.Fatalf("派生 status = %v，期望 enabled", item["status"])
	}
	if item["quota_percent"] != float64(100) {
		t.Fatalf("quota_percent = %v，期望 100", item["quota_percent"])
	}
}

// TestTokenUpdateOptimisticLockConflict 覆盖 brief (g)：陈旧 updated_at → 409。
func TestTokenUpdateOptimisticLockConflict(t *testing.T) {
	env := newTokenTestEnv(t)
	stale := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
		return sampleToken(), nil
	}
	env.svc.updateFn = func(_ context.Context, _, id int64, in tokenstore.UpdateInput) (*tokenstore.Token, error) {
		if id != 7 {
			t.Fatalf("id = %d", id)
		}
		if in.UpdatedAt == nil || !in.UpdatedAt.Equal(stale) {
			t.Fatalf("expected updated_at 未透传: %+v", in.UpdatedAt)
		}
		return nil, tokenstore.ErrConflict
	}
	rr := env.do(http.MethodPatch, "/api/tokens/7", `{"name":"new","updated_at":"2026-10-18T09:00:00Z"}`, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%q）", rr.Code, rr.Body.String())
	}
	var p map[string]any
	decodeJSON(t, rr, &p)
	if !strings.HasSuffix(p["type"].(string), "/conflict") {
		t.Fatalf("problem.type = %v，期望 .../conflict", p["type"])
	}
}

// TestTokenUpdateRequiresUpdatedAt：缺乐观锁版本 → 400，且不触达存储。
func TestTokenUpdateRequiresUpdatedAt(t *testing.T) {
	env := newTokenTestEnv(t)
	called := false
	env.svc.updateFn = func(context.Context, int64, int64, tokenstore.UpdateInput) (*tokenstore.Token, error) {
		called = true
		return nil, nil
	}
	rr := env.do(http.MethodPatch, "/api/tokens/7", `{"name":"new"}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", rr.Code)
	}
	if called {
		t.Fatal("缺 updated_at 不得触达存储")
	}
}

// TestTokenOwnershipMapsTo404：他人/不存在的令牌统一 404，不泄露存在性。
func TestTokenOwnershipMapsTo404(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
		return nil, tokenstore.ErrNotFound
	}
	env.svc.deleteFn = func(context.Context, int64, int64, *time.Time) error {
		return tokenstore.ErrNotFound
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/tokens/99"},
		{http.MethodDelete, "/api/tokens/99"},
	} {
		rr := env.do(tc.method, tc.path, "", nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s status = %d，期望 404", tc.method, tc.path, rr.Code)
		}
	}
}

// TestTokenBatchPartialFailure 覆盖 brief (f) 的 HTTP 面：部分失败明细原样下发。
func TestTokenBatchPartialFailure(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.batchFn = func(_ context.Context, userID int64, ids []int64) (int, []tokenstore.BatchFailure, error) {
		if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 99 {
			t.Fatalf("ids = %v", ids)
		}
		return 2, []tokenstore.BatchFailure{{ID: 99, Detail: "token not found"}}, nil
	}
	rr := env.do(http.MethodPost, "/api/tokens:batch-disable", `{"ids":[1,2,99]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var resp struct {
		Succeeded int `json:"succeeded"`
		Failed    []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Detail string `json:"detail"`
		} `json:"failed"`
	}
	decodeJSON(t, rr, &resp)
	if resp.Succeeded != 2 || len(resp.Failed) != 1 || resp.Failed[0].ID != 99 || resp.Failed[0].Detail == "" {
		t.Fatalf("批量响应错误: %+v", resp)
	}
}

// TestTokenBatchLimit：ids 超过 100 → 400；空 ids → 400。
func TestTokenBatchLimit(t *testing.T) {
	env := newTokenTestEnv(t)
	ids := make([]int64, 101)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	raw, _ := json.Marshal(map[string]any{"ids": ids})
	if rr := env.do(http.MethodPost, "/api/tokens:batch-enable", string(raw), nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("101 个 ids status = %d，期望 400", rr.Code)
	}
	if rr := env.do(http.MethodPost, "/api/tokens:batch-enable", `{"ids":[]}`, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("空 ids status = %d，期望 400", rr.Code)
	}
}

// TestTokenSingleActions：enable/disable/reset-key 的路由与响应。
func TestTokenSingleActions(t *testing.T) {
	env := newTokenTestEnv(t)
	tok := sampleToken()
	tok.Status = tokenstore.StatusDisabled
	env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) { return tok, nil }
	env.svc.enableFn = func(context.Context, int64, int64) (*tokenstore.Token, error) { return tok, nil }
	env.svc.disableFn = func(context.Context, int64, int64) (*tokenstore.Token, error) { return tok, nil }
	env.svc.resetFn = func(context.Context, int64, int64) (string, string, *tokenstore.Token, error) {
		return "nkt-" + strings.Repeat("Z", 48), "nkt-" + strings.Repeat("•", 24), tok, nil
	}

	for _, action := range []string{"enable", "disable"} {
		rr := env.do(http.MethodPost, "/api/tokens/7:"+action, "", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d（body=%q）", action, rr.Code, rr.Body.String())
		}
	}
	rr := env.do(http.MethodPost, "/api/tokens/7:reset-key", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-key status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	decodeJSON(t, rr, &resp)
	if resp["key"] != "nkt-"+strings.Repeat("Z", 48) {
		t.Fatalf("reset-key 未返回新明文: %v", resp)
	}
	if resp["key_masked"] != "nkt-"+strings.Repeat("•", 24) {
		t.Fatalf("key_masked = %v", resp["key_masked"])
	}

	// 未知动作 → 404，不得落入 enable/disable。
	if rr := env.do(http.MethodPost, "/api/tokens/7:explode", "", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("未知动作 status = %d，期望 404", rr.Code)
	}
}

// TestTokenAutoGroupsEndpoint：{groups:[{name,ratio}],max_count}。
func TestTokenAutoGroupsEndpoint(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.autoGroupsFn = func(_ context.Context, userID int64) ([]tokenstore.AutoGroup, int, error) {
		if userID != testUserID {
			t.Fatalf("userID = %d", userID)
		}
		return []tokenstore.AutoGroup{{Name: "g1", Ratio: 1.5}}, 20, nil
	}
	rr := env.do(http.MethodGet, "/api/token/auto-groups", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct {
		Groups []struct {
			Name  string  `json:"name"`
			Ratio float64 `json:"ratio"`
		} `json:"groups"`
		MaxCount int `json:"max_count"`
	}
	decodeJSON(t, rr, &resp)
	if resp.MaxCount != 20 || len(resp.Groups) != 1 || resp.Groups[0].Name != "g1" || resp.Groups[0].Ratio != 1.5 {
		t.Fatalf("auto-groups 响应错误: %+v", resp)
	}
}

// TestTokenUsageSelfRead 覆盖 I-29 与 brief 的 usage 端点语义：
// 只认 Bearer 自身 key；禁用/过期/不存在 → 401 对应 code；耗尽 → 200。
func TestTokenUsageSelfRead(t *testing.T) {
	env := newTokenTestEnv(t)
	bearer := "Bearer nkt-" + strings.Repeat("Q", 48)

	cases := []struct {
		name       string
		resolveErr error
		meta       *auth.TokenMeta
		token      *tokenstore.Token
		wantStatus int
		wantType   string
	}{
		{
			name:       "额度耗尽仍 200",
			meta:       &auth.TokenMeta{ID: 7, UserID: testUserID, Status: tokenstore.StatusEnabled, RemainQuota: 0},
			token:      func() *tokenstore.Token { tok := sampleToken(); zero := int64(0); tok.RemainQuota = &zero; return tok }(),
			wantStatus: http.StatusOK,
		},
		{
			name: "不限额度 200 且 total_available=null",
			meta: &auth.TokenMeta{ID: 7, UserID: testUserID, Status: tokenstore.StatusEnabled, UnlimitedQuota: true},
			token: func() *tokenstore.Token {
				tok := sampleToken()
				tok.RemainQuota = nil
				return tok
			}(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "已禁用",
			resolveErr: tokenstore.ErrTokenDisabled,
			wantStatus: http.StatusUnauthorized,
			wantType:   "https://newtg.dev/problems/token_disabled",
		},
		{
			name:       "已过期",
			resolveErr: tokenstore.ErrTokenExpired,
			wantStatus: http.StatusUnauthorized,
			wantType:   "https://newtg.dev/problems/token_expired",
		},
		{
			name:       "不存在或已删除",
			resolveErr: tokenstore.ErrInvalidToken,
			wantStatus: http.StatusUnauthorized,
			wantType:   "https://newtg.dev/problems/invalid_token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env.svc.resolveFn = func(_ context.Context, keyHash string) (*auth.TokenMeta, error) {
				if keyHash != auth.HashTokenKey("nkt-"+strings.Repeat("Q", 48)) {
					t.Fatalf("Resolve 未收到完整明文哈希: %q", keyHash)
				}
				return tc.meta, tc.resolveErr
			}
			env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
				if tc.token == nil {
					return nil, tokenstore.ErrNotFound
				}
				return tc.token, nil
			}

			rr := env.do(http.MethodGet, "/api/usage/token", "", map[string]string{"Authorization": bearer})
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d，期望 %d（body=%q）", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if tc.wantType != "" {
				var p map[string]any
				decodeJSON(t, rr, &p)
				if p["type"] != tc.wantType {
					t.Fatalf("problem.type = %v，期望 %v", p["type"], tc.wantType)
				}
				return
			}
			var resp map[string]any
			decodeJSON(t, rr, &resp)
			if resp["object"] != "token_usage" {
				t.Fatalf("object = %v，期望 token_usage", resp["object"])
			}
			if resp["name"] != "prod" {
				t.Fatalf("name = %v", resp["name"])
			}
		})
	}

	// 无 Bearer（仅有会话 Cookie 形态）→ 401，不得借用会话身份。
	rr := env.do(http.MethodGet, "/api/usage/token", "", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无 Bearer status = %d，期望 401", rr.Code)
	}
}

// TestTokenRouteRegistrationPatterns 冻结路由挂载形态：冒号动作不会被
// {id} 通配吞掉，CRUD 与批量端点互不遮蔽。
func TestTokenRouteRegistrationPatterns(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
		return sampleToken(), nil
	}
	env.svc.enableFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
		return sampleToken(), nil
	}
	env.svc.disableFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
		return sampleToken(), nil
	}
	routes := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/tokens", http.StatusOK},
		{http.MethodGet, "/api/token/auto-groups", http.StatusOK},
		{http.MethodPost, "/api/tokens:batch-enable", http.StatusOK},
		{http.MethodPost, "/api/tokens:batch-disable", http.StatusOK},
		{http.MethodPost, "/api/tokens:batch-delete", http.StatusOK},
		{http.MethodPost, "/api/tokens/7:enable", http.StatusOK},
		{http.MethodGet, "/api/tokens/7", http.StatusOK},
	}
	for _, route := range routes {
		body := ""
		if strings.Contains(route.path, "batch") {
			body = `{"ids":[1]}`
		}
		if rr := env.do(route.method, route.path, body, nil); rr.Code != route.want {
			t.Fatalf("%s %s status = %d，期望 %d（body=%q）", route.method, route.path, rr.Code, route.want, rr.Body.String())
		}
	}
}
