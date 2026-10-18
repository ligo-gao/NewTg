package httpapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeTestSealer 是可逆的测试用信封（前缀 + base64），用于证明幂等缓存体
// 不是明文；SIV 语义（同一明文不同密文）不属于本测试关注点。
type fakeTestSealer struct{ fail bool }

const fakeSealPrefix = "sealed:"

func (s fakeTestSealer) Seal(plain string) (string, error) {
	if s.fail {
		return "", errors.New("seal failed")
	}
	return fakeSealPrefix + base64.RawStdEncoding.EncodeToString([]byte(plain)), nil
}

func (s fakeTestSealer) Open(sealed string) (string, error) {
	if s.fail {
		return "", errors.New("open failed")
	}
	if !strings.HasPrefix(sealed, fakeSealPrefix) {
		return "", errors.New("not sealed")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, fakeSealPrefix))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type tokenTestEnv struct {
	handlers *TokenHandlers
	svc      *fakeTokenService
	audit    *captureExec
	mux      *http.ServeMux
	mr       *miniredis.Miniredis
	rdb      *redis.Client
}

func newTokenTestEnv(t *testing.T) *tokenTestEnv {
	t.Helper()
	svc := &fakeTokenService{}
	capt := &captureExec{}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	h := NewTokenHandlers(svc, audit.New(capt), rdb, fakeTestSealer{})
	mux := http.NewServeMux()
	h.RegisterTokenRoutes(mux, nil)
	return &tokenTestEnv{handlers: h, svc: svc, audit: capt, mux: mux, mr: mr, rdb: rdb}
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
			// 评审结论：M3 接入 usage_logs 之前 total_granted 返回 null。
			if resp["total_granted"] != nil {
				t.Fatalf("total_granted = %v，期望 null", resp["total_granted"])
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

// TestTokenResponseIncludesUpdatedAtAndPatchRoundTrips 覆盖评审 Critical：
// 创建/列表/详情必须下发 updated_at，客户端才能拿它做乐观锁 PATCH。
func TestTokenResponseIncludesUpdatedAtAndPatchRoundTrips(t *testing.T) {
	env := newTokenTestEnv(t)
	lockedAt := time.Date(2026, 10, 18, 10, 30, 0, 123000000, time.UTC)
	tok := sampleToken()
	tok.UpdatedAt = lockedAt

	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return []tokenstore.Created{{Token: tok, Key: "nkt-" + strings.Repeat("A", 48)}}, nil
	}
	env.svc.listFn = func(context.Context, int64, tokenstore.ListFilter) ([]*tokenstore.Token, int64, error) {
		return []*tokenstore.Token{tok}, 1, nil
	}
	env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) {
		return tok, nil
	}
	env.svc.updateFn = func(_ context.Context, _, _ int64, in tokenstore.UpdateInput) (*tokenstore.Token, error) {
		if in.UpdatedAt == nil || !in.UpdatedAt.Equal(lockedAt) {
			return nil, tokenstore.ErrConflict
		}
		updated := *tok
		if in.Name != nil {
			updated.Name = *in.Name
		}
		updated.UpdatedAt = lockedAt.Add(time.Minute)
		return &updated, nil
	}

	// 创建响应带 updated_at。
	createdRR := env.do(http.MethodPost, "/api/tokens", `{"name":"prod"}`, nil)
	if createdRR.Code != http.StatusOK {
		t.Fatalf("create status = %d（body=%q）", createdRR.Code, createdRR.Body.String())
	}
	var created struct {
		Created []struct {
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"created"`
	}
	decodeJSON(t, createdRR, &created)
	if len(created.Created) != 1 || !created.Created[0].UpdatedAt.Equal(lockedAt) {
		t.Fatalf("创建响应 updated_at = %+v，期望 %v", created.Created, lockedAt)
	}

	// 列表响应带 updated_at。
	listRR := env.do(http.MethodGet, "/api/tokens", "", nil)
	var list listTokensResponse
	decodeJSON(t, listRR, &list)
	if len(list.Items) != 1 || !list.Items[0].UpdatedAt.Equal(lockedAt) {
		t.Fatalf("列表响应 updated_at 缺失/错误: %+v", list.Items)
	}

	// 用创建响应里的 updated_at 做 PATCH → 成功。
	patchBody := `{"name":"prod-v2","updated_at":"` + lockedAt.Format(time.RFC3339Nano) + `"}`
	if rr := env.do(http.MethodPatch, "/api/tokens/7", patchBody, nil); rr.Code != http.StatusOK {
		t.Fatalf("正确版本 PATCH status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	// 陈旧的 updated_at → 409。
	stale := `{"name":"stale","updated_at":"2026-10-18T09:00:00Z"}`
	if rr := env.do(http.MethodPatch, "/api/tokens/7", stale, nil); rr.Code != http.StatusConflict {
		t.Fatalf("陈旧版本 PATCH status = %d，期望 409", rr.Code)
	}
}

// TestTokenResponsesSetNoStoreHeaders 覆盖评审 Important 4：
// 令牌响应（尤其创建/重置密钥）必须禁止中间层缓存。
func TestTokenResponsesSetNoStoreHeaders(t *testing.T) {
	env := newTokenTestEnv(t)
	tok := sampleToken()
	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return []tokenstore.Created{{Token: tok, Key: "nkt-" + strings.Repeat("A", 48)}}, nil
	}
	assertNoStore := func(label string, rr *httptest.ResponseRecorder) {
		t.Helper()
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q，期望 no-store", label, got)
		}
		if got := rr.Header().Get("Pragma"); got != "no-cache" {
			t.Fatalf("%s Pragma = %q，期望 no-cache", label, got)
		}
	}
	assertNoStore("创建", env.do(http.MethodPost, "/api/tokens", `{"name":"prod"}`, nil))
	assertNoStore("列表", env.do(http.MethodGet, "/api/tokens", "", nil))

	// 错误响应同样 no-store。
	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return nil, &tokenstore.ValidationError{Field: "name", Message: "bad"}
	}
	assertNoStore("校验错误", env.do(http.MethodPost, "/api/tokens", `{"name":""}`, nil))
}

// TestTokenResetKeyIsIdempotent 覆盖评审 Important 6：
// 同一 Idempotency-Key 重放 reset-key 只重置一次；不同 token id 不互相命中。
func TestTokenResetKeyIsIdempotent(t *testing.T) {
	env := newTokenTestEnv(t)
	tok := sampleToken()
	env.svc.getFn = func(context.Context, int64, int64) (*tokenstore.Token, error) { return tok, nil }
	calls := 0
	env.svc.resetFn = func(context.Context, int64, int64) (string, string, *tokenstore.Token, error) {
		calls++
		return "nkt-" + strings.Repeat("Z", 48), "nkt-" + strings.Repeat("•", 24), tok, nil
	}

	headers := map[string]string{"Idempotency-Key": "reset-once"}
	first := env.do(http.MethodPost, "/api/tokens/7:reset-key", "", headers)
	second := env.do(http.MethodPost, "/api/tokens/7:reset-key", "", headers)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("status = %d/%d（body=%q / %q）", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("幂等重放响应不一致：%q vs %q", first.Body.String(), second.Body.String())
	}
	if calls != 1 {
		t.Fatalf("ResetKey 调用 = %d，期望 1", calls)
	}

	// 另一个 token id 使用同一幂等键必须独立执行。
	if rr := env.do(http.MethodPost, "/api/tokens/8:reset-key", "", headers); rr.Code != http.StatusOK {
		t.Fatalf("token 8 reset status = %d", rr.Code)
	}
	if calls != 2 {
		t.Fatalf("不同 token 的 ResetKey 调用 = %d，期望 2（缓存键必须含路径/id）", calls)
	}
}

// TestTokenCreateAcceptsEpochAndRFC3339ExpiresAt 覆盖评审 Important 7：
// expires_at 入参同时接受 epoch 秒与 RFC 3339 字符串。
func TestTokenCreateAcceptsEpochAndRFC3339ExpiresAt(t *testing.T) {
	env := newTokenTestEnv(t)
	var got *time.Time
	env.svc.createFn = func(_ context.Context, _ int64, in tokenstore.CreateInput) ([]tokenstore.Created, error) {
		got = in.ExpiresAt
		return []tokenstore.Created{}, nil
	}

	epoch := int64(1893456000) // 2030-01-01T00:00:00Z
	if rr := env.do(http.MethodPost, "/api/tokens", `{"name":"x","expires_at":1893456000}`, nil); rr.Code != http.StatusOK {
		t.Fatalf("epoch 输入 status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if got == nil || !got.Equal(time.Unix(epoch, 0).UTC()) {
		t.Fatalf("epoch 解析 = %v，期望 %v", got, time.Unix(epoch, 0).UTC())
	}

	if rr := env.do(http.MethodPost, "/api/tokens", `{"name":"x","expires_at":"2030-01-01T00:00:00Z"}`, nil); rr.Code != http.StatusOK {
		t.Fatalf("RFC3339 输入 status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if got == nil || !got.Equal(time.Unix(epoch, 0).UTC()) {
		t.Fatalf("RFC3339 解析 = %v", got)
	}

	// 非法格式 → 400。
	if rr := env.do(http.MethodPost, "/api/tokens", `{"name":"x","expires_at":"tomorrow"}`, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 expires_at status = %d，期望 400", rr.Code)
	}
}

// TestRouterHandlePublicBypassesProtectedSubtree 覆盖评审 Important 5：
// usage 的公共挂载必须优先于 /api/ 受保护子树。
func TestRouterHandlePublicBypassesProtectedSubtree(t *testing.T) {
	rt := &Router{mux: http.NewServeMux(), protected: http.NewServeMux()}
	rt.mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rt.HandlePublic("GET /api/usage/token", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/usage/token", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("usage status = %d，期望 200（公共挂载未生效）", rr.Code)
	}
	rr = httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/tokens", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("其他 /api 路径 status = %d，期望走受保护子树", rr.Code)
	}
}

// TestRouterMountsTokenHandlers 覆盖评审 Important 5 的自动接线：
// CRUD 走 SessionAuth（未认证 → unauthorized），usage 走公共精确路由
// （未带 Bearer 时由 handler 自己返回 invalid_token）。
func TestRouterMountsTokenHandlers(t *testing.T) {
	env := newTokenTestEnv(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))

	rt, err := NewRouter(RouterConfig{
		Redis:         rdb,
		Sessions:      sessions,
		Repo:          newFakeAuthRepo(),
		Config:        mapConfig{},
		SessionSecret: []byte(testSessionSecret),
		EmailSender:   &fakeEmailSender{},
		TokenHandlers: env.handlers,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/usage/token", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("usage status = %d，期望 401", rr.Code)
	}
	var p map[string]any
	decodeJSON(t, rr, &p)
	if p["type"] != problemTypeInvalidToken {
		t.Fatalf("usage problem.type = %v，期望 %s（说明未经 SessionAuth）", p["type"], problemTypeInvalidToken)
	}

	rr = httptest.NewRecorder()
	rt.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/tokens", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("tokens status = %d，期望 401", rr.Code)
	}
	decodeJSON(t, rr, &p)
	if p["type"] != problemTypeUnauthorized {
		t.Fatalf("tokens problem.type = %v，期望 %s（受保护链）", p["type"], problemTypeUnauthorized)
	}
}

// TestTokenIdempotencyCacheSealed 覆盖 B2：含一次性明文 key 的幂等响应必须
// 以信封加密落 Redis，重放仍返回首次原文。
func TestTokenIdempotencyCacheSealed(t *testing.T) {
	env := newTokenTestEnv(t)
	tok := sampleToken()
	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return []tokenstore.Created{{Token: tok, Key: "nkt-" + strings.Repeat("A", 48)}}, nil
	}
	body := `{"name":"prod","group":"default","unlimited_quota":true}`
	headers := map[string]string{"Idempotency-Key": "sealed-1"}

	first := env.do(http.MethodPost, "/api/tokens", body, headers)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", first.Code, first.Body.String())
	}

	var cacheKey string
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "newtg:idem:token:") && !strings.HasSuffix(key, ":lock") {
			cacheKey = key
		}
	}
	if cacheKey == "" {
		t.Fatal("未找到幂等缓存键")
	}
	raw, err := env.mr.Get(cacheKey)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if !strings.HasPrefix(raw, fakeSealPrefix) {
		t.Fatalf("幂等缓存不是密封信封: %q", raw)
	}
	opened, err := fakeTestSealer{}.Open(raw)
	if err != nil {
		t.Fatalf("open sealed cache: %v", err)
	}
	if strings.Contains(raw, "nkt-") {
		t.Fatalf("Redis 中出现明文 key: %q", raw)
	}
	if !strings.Contains(opened, "nkt-"+strings.Repeat("A", 48)) {
		t.Fatalf("密封体内缺少首次响应: %q", opened)
	}

	second := env.do(http.MethodPost, "/api/tokens", body, headers)
	if second.Code != http.StatusOK || second.Body.String() != first.Body.String() {
		t.Fatalf("重放不一致：first=%q second=%q", first.Body.String(), second.Body.String())
	}
	if env.svc.createCalls != 1 {
		t.Fatalf("Create 调用 = %d，期望 1", env.svc.createCalls)
	}
}

// TestTokenIdempotencyDisabledWithoutSealer 覆盖 B2 的降级：sealer 为 nil 时
// 完全跳过缓存，绝不能回落明文。
func TestTokenIdempotencyDisabledWithoutSealer(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return []tokenstore.Created{{Token: sampleToken(), Key: "nkt-" + strings.Repeat("B", 48)}}, nil
	}
	h := NewTokenHandlers(env.svc, nil, env.rdb, nil)
	mux := http.NewServeMux()
	h.RegisterTokenRoutes(mux, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/tokens",
		strings.NewReader(`{"name":"prod","group":"default","unlimited_quota":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "no-sealer")
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{UserID: testUserID}))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	for _, key := range env.mr.Keys() {
		if strings.HasPrefix(key, "newtg:idem:token:") {
			t.Fatalf("sealer=nil 时不得写幂等缓存，发现键 %q", key)
		}
	}
}

// TestTokenIdempotencyUndecryptableKeyIsMiss 覆盖 B2：解密失败按未命中处理并
// 删除脏键（旧版本明文/密钥轮换），随后正常执行并写入密封结果。
func TestTokenIdempotencyUndecryptableKeyIsMiss(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.createFn = func(context.Context, int64, tokenstore.CreateInput) ([]tokenstore.Created, error) {
		return []tokenstore.Created{{Token: sampleToken(), Key: "nkt-" + strings.Repeat("C", 48)}}, nil
	}
	cacheKey := "newtg:idem:token:42:create:/api/tokens:legacy"
	if err := env.mr.Set(cacheKey, "plaintext-legacy-value"); err != nil {
		t.Fatalf("seed legacy cache: %v", err)
	}

	rr := env.do(http.MethodPost, "/api/tokens",
		`{"name":"prod","group":"default","unlimited_quota":true}`,
		map[string]string{"Idempotency-Key": "legacy"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	if env.svc.createCalls != 1 {
		t.Fatalf("Create 调用 = %d，期望 1（脏键必须视为未命中）", env.svc.createCalls)
	}
	raw, err := env.mr.Get(cacheKey)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if raw == "plaintext-legacy-value" || !strings.HasPrefix(raw, fakeSealPrefix) {
		t.Fatalf("脏键未被替换为密封结果: %q", raw)
	}
}

// TestTokenListRejectsInvalidQueryParams 覆盖 B9：p 超上限、status/quota 非法
// 枚举必须 400（不得静默忽略后返回全量），且非法请求不触达存储层。
func TestTokenListRejectsInvalidQueryParams(t *testing.T) {
	env := newTokenTestEnv(t)
	for _, tc := range []struct{ query, field string }{
		{"?p=100001", "p"},
		{"?status=bogus", "status"},
		{"?quota=bogus", "quota"},
	} {
		rr := env.do(http.MethodGet, "/api/tokens"+tc.query, "", nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d，期望 400（body=%q）", tc.query, rr.Code, rr.Body.String())
		}
		var p map[string]any
		decodeJSON(t, rr, &p)
		if p["field"] != tc.field {
			t.Fatalf("%s problem.field = %v，期望 %s", tc.query, p["field"], tc.field)
		}
	}
	if env.svc.listCalls != 0 {
		t.Fatalf("非法参数触达存储层 %d 次，期望 0", env.svc.listCalls)
	}
	for _, query := range []string{"?status=exhausted", "?quota=low", "?p=100000"} {
		if rr := env.do(http.MethodGet, "/api/tokens"+query, "", nil); rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d，期望 200（body=%q）", query, rr.Code, rr.Body.String())
		}
	}
}

// TestTokenBatchBackendErrorMapsTo503 覆盖 C6：tokenstore.ErrBackend（基础设施
// 故障）必须 503，且响应绝不携带底层错误文本（内网地址/SQL 细节）。
func TestTokenBatchBackendErrorMapsTo503(t *testing.T) {
	env := newTokenTestEnv(t)
	infra := errors.New("dial tcp 10.1.2.3:5432: connect: connection refused")
	env.svc.batchFn = func(context.Context, int64, []int64) (int, []tokenstore.BatchFailure, error) {
		return 0, nil, fmt.Errorf("%w: %w", tokenstore.ErrBackend, infra)
	}
	rr := env.do(http.MethodPost, "/api/tokens:batch-enable", `{"ids":[1]}`, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 必须带 Retry-After")
	}
	for _, leak := range []string{"dial tcp", "10.1.2.3", "connection refused"} {
		if strings.Contains(rr.Body.String(), leak) {
			t.Fatalf("响应泄露底层错误文本 %q: %s", leak, rr.Body.String())
		}
	}
}

// TestTokenBatchPartialSuccessAuditedOnError 覆盖 D1 复核新增项：批量在
// succeeded>0 后遇到 ErrBackend 时，已提交的变更仍必须写一条 failed 审计，
// 且审计不含底层错误文本。
func TestTokenBatchPartialSuccessAuditedOnError(t *testing.T) {
	env := newTokenTestEnv(t)
	env.svc.batchFn = func(context.Context, int64, []int64) (int, []tokenstore.BatchFailure, error) {
		return 7, []tokenstore.BatchFailure{{ID: 8, Detail: "internal error"}},
			fmt.Errorf("%w: %w", tokenstore.ErrBackend, errors.New("db down"))
	}
	rr := env.do(http.MethodPost, "/api/tokens:batch-delete", `{"ids":[1,2,3,4,5,6,7,8]}`, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503（body=%q）", rr.Code, rr.Body.String())
	}
	rows := env.audit.snapshot()
	if len(rows) != 1 {
		t.Fatalf("审计行 = %d，期望 1（部分成功必须留痕）", len(rows))
	}
	args := rows[0]
	if args[8] != "failed" {
		t.Fatalf("审计 result = %v，期望 failed", args[8])
	}
	if status, ok := args[7].(int16); !ok || status != http.StatusServiceUnavailable {
		t.Fatalf("审计 status_code = %v（%T），期望 503", args[7], args[7])
	}
	detail, ok := args[9].(string)
	if !ok {
		t.Fatalf("detail 类型 = %T，期望 string", args[9])
	}
	if !strings.Contains(detail, `"succeeded":7`) || !strings.Contains(detail, `"batch_action":"delete"`) {
		t.Fatalf("detail = %s，缺少 succeeded/batch_action", detail)
	}
	if strings.Contains(detail, "db down") {
		t.Fatalf("审计泄露底层错误文本: %s", detail)
	}
}
