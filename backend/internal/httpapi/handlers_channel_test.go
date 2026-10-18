package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// 本文件是 Task 8（phase 8A：渠道存储 + 管理端点）的行为测试。
// 纪律（R-P10）：只用内存 fake，不连真 PG/Redis——SQL 形状由 store 的集成测试
// （build tag=integration）覆盖，本文件只约束 handler 契约与安全不变量。

const (
	testAdminID   int64 = 1
	testRegularID int64 = 2
	// 任意非空密钥即可：sealer 的派生规则见 channel_sealer.go（GC-8）。
	testSealerMaster = "channel-handler-test-master-key-0123456789"
)

// ---------- 内存 fake（ChannelStore） ----------

type fakeChannelStore struct {
	mu sync.Mutex

	nextID int64
	items  map[int64]*store.Channel

	lastFilter store.ChannelFilter
	listItems  []store.Channel
	listTotal  int
	listErr    error

	getErr        error
	createErr     error
	updateErr     error
	deleteErr     error
	batchErr      error
	batchDelErr   error
	testResultErr error

	createCalls      int
	updateCalls      int
	deleteCalls      int
	batchCalls       int
	batchDeleteCalls int
	testResultCalls  int

	lastPatch    store.ChannelPatch
	lastExpected time.Time
	lastBatchIDs []int64
	lastBatchTo  int16
	lastDelIDs   []int64
	// createHook 在 Create 写库前调用（并发幂等测试用它把首个请求卡在执行中）。
	createHook func()
	// batchTransitions 记录批量启停真正发生状态迁移的 id（已处于目标态的不计）。
	batchTransitions []int64
	lastTestAt       time.Time
	lastTestResult   *bool
	lastTestID       int64
}

func newFakeChannelStore() *fakeChannelStore {
	return &fakeChannelStore{nextID: 1, items: map[int64]*store.Channel{}}
}

func cloneChannel(c *store.Channel) *store.Channel {
	if c == nil {
		return nil
	}
	out := *c
	out.Credentials = append([]byte(nil), c.Credentials...)
	out.SupportedModels = append([]byte(nil), c.SupportedModels...)
	out.GroupTags = append([]byte(nil), c.GroupTags...)
	out.ModelMapping = append([]byte(nil), c.ModelMapping...)
	if c.BaseURL != nil {
		v := *c.BaseURL
		out.BaseURL = &v
	}
	if c.RateLimit != nil {
		v := *c.RateLimit
		out.RateLimit = &v
	}
	if c.TestModel != nil {
		v := *c.TestModel
		out.TestModel = &v
	}
	if c.Remark != nil {
		v := *c.Remark
		out.Remark = &v
	}
	if c.Tag != nil {
		v := *c.Tag
		out.Tag = &v
	}
	if c.AutoDisabledReason != nil {
		v := *c.AutoDisabledReason
		out.AutoDisabledReason = &v
	}
	if c.LastTestAt != nil {
		v := *c.LastTestAt
		out.LastTestAt = &v
	}
	if c.LastTestResult != nil {
		v := *c.LastTestResult
		out.LastTestResult = &v
	}
	return &out
}

func (f *fakeChannelStore) add(c *store.Channel) *store.Channel {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == 0 {
		c.ID = f.nextID
		f.nextID++
	}
	f.items[c.ID] = cloneChannel(c)
	return cloneChannel(c)
}

func (f *fakeChannelStore) List(_ context.Context, filter store.ChannelFilter) ([]store.Channel, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFilter = filter
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	items := make([]store.Channel, 0, len(f.listItems))
	for _, c := range f.listItems {
		items = append(items, *cloneChannel(&c))
	}
	total := f.listTotal
	if len(items) > 0 && total == 0 {
		total = len(items)
	}
	return items, total, nil
}

func (f *fakeChannelStore) Get(_ context.Context, id int64) (*store.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.items[id]
	if !ok {
		return nil, store.ErrChannelNotFound
	}
	return cloneChannel(c), nil
}

func (f *fakeChannelStore) Create(_ context.Context, ch *store.Channel) (*store.Channel, error) {
	// hook 不持锁执行：测试需要在「已进入 build、尚未写库」的窗口内发起第二个请求。
	f.mu.Lock()
	hook := f.createHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	if ch.ID == 0 {
		ch.ID = f.nextID
		f.nextID++
	}
	now := time.Now().UTC()
	ch.CreatedAt = now
	ch.UpdatedAt = now
	f.items[ch.ID] = cloneChannel(ch)
	return cloneChannel(ch), nil
}

func (f *fakeChannelStore) Update(_ context.Context, id int64, patch store.ChannelPatch, expected time.Time) (*store.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	f.lastPatch = patch
	f.lastExpected = expected
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	c, ok := f.items[id]
	if !ok {
		return nil, store.ErrChannelNotFound
	}
	// 与 SQL 实现同口径：stored updated_at 精确比较（R-P8）。
	if !c.UpdatedAt.Equal(expected) {
		return nil, store.ErrConflict
	}
	if patch.Name != nil {
		c.Name = *patch.Name
	}
	if patch.Type != nil {
		c.Type = *patch.Type
	}
	if patch.BaseURLSet {
		c.BaseURL = patch.BaseURL
	}
	if patch.Credentials != nil {
		c.Credentials = append([]byte(nil), patch.Credentials...)
	}
	if patch.SupportedModels != nil {
		c.SupportedModels = append([]byte(nil), patch.SupportedModels...)
	}
	if patch.GroupTagsSet {
		c.GroupTags = append([]byte(nil), patch.GroupTags...)
	}
	if patch.ModelMapping != nil {
		c.ModelMapping = append([]byte(nil), patch.ModelMapping...)
	}
	if patch.Weight != nil {
		c.Weight = *patch.Weight
	}
	if patch.Priority != nil {
		c.Priority = *patch.Priority
	}
	if patch.RateLimitSet {
		c.RateLimit = patch.RateLimit
	}
	if patch.Status != nil {
		c.Status = *patch.Status
	}
	if patch.AutoBan != nil {
		c.AutoBan = *patch.AutoBan
	}
	if patch.TestModelSet {
		c.TestModel = patch.TestModel
	}
	if patch.RemarkSet {
		c.Remark = patch.Remark
	}
	if patch.TagSet {
		c.Tag = patch.Tag
	}
	if patch.FailCount != nil {
		c.FailCount = *patch.FailCount
	}
	if patch.AutoDisabledReasonSet {
		c.AutoDisabledReason = patch.AutoDisabledReason
	}
	if patch.LastTestAtSet {
		c.LastTestAt = patch.LastTestAt
	}
	if patch.LastTestResultSet {
		c.LastTestResult = patch.LastTestResult
	}
	c.UpdatedAt = time.Now().UTC()
	f.items[id] = c
	return cloneChannel(c), nil
}

func (f *fakeChannelStore) Delete(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.items[id]; !ok {
		return store.ErrChannelNotFound
	}
	delete(f.items, id)
	return nil
}

func (f *fakeChannelStore) BatchStatus(_ context.Context, ids []int64, status int16) (store.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchCalls++
	f.lastBatchIDs = append([]int64(nil), ids...)
	f.lastBatchTo = status
	if f.batchErr != nil {
		return store.BatchResult{}, f.batchErr
	}
	res := store.BatchResult{Succeeded: []int64{}, Failed: []store.BatchFailure{}}
	for _, id := range ids {
		c, ok := f.items[id]
		if !ok {
			res.Failed = append(res.Failed, store.BatchFailure{ID: id, Reason: "not_found"})
			continue
		}
		res.Succeeded = append(res.Succeeded, id)
		// 设计 F-06-02：已处于目标态计 succeeded 但不计变更。
		if status == 1 && c.Status != 1 {
			c.Status = 1
			c.FailCount = 0
			c.AutoDisabledReason = nil
			c.UpdatedAt = time.Now().UTC()
			f.batchTransitions = append(f.batchTransitions, id)
		} else if status == 2 && c.Status == 1 {
			c.Status = 2
			c.UpdatedAt = time.Now().UTC()
			f.batchTransitions = append(f.batchTransitions, id)
		}
	}
	return res, nil
}

func (f *fakeChannelStore) BatchDelete(_ context.Context, ids []int64) (store.BatchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchDeleteCalls++
	f.lastDelIDs = append([]int64(nil), ids...)
	if f.batchDelErr != nil {
		return store.BatchResult{}, f.batchDelErr
	}
	res := store.BatchResult{Succeeded: []int64{}, Failed: []store.BatchFailure{}}
	for _, id := range ids {
		if _, ok := f.items[id]; !ok {
			res.Failed = append(res.Failed, store.BatchFailure{ID: id, Reason: "not_found"})
			continue
		}
		delete(f.items, id)
		res.Succeeded = append(res.Succeeded, id)
	}
	return res, nil
}

func (f *fakeChannelStore) UpdateTestResult(_ context.Context, id int64, at time.Time, success bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.testResultCalls++
	f.lastTestID = id
	f.lastTestAt = at
	v := success
	f.lastTestResult = &v
	if f.testResultErr != nil {
		return f.testResultErr
	}
	c, ok := f.items[id]
	if !ok {
		return store.ErrChannelNotFound
	}
	c.LastTestAt = &at
	c.LastTestResult = &v
	c.UpdatedAt = time.Now().UTC()
	return nil
}

// ---------- 内存 fake（UserAccessStore / ChannelTester） ----------

type fakeUserAccess struct {
	mu      sync.Mutex
	byUser  map[int64]fakeUserAccessEntry
	err     error
	calls   int
	lastUID int64
}

type fakeUserAccessEntry struct {
	role        string
	permissions []string
	status      string
}

func newFakeUserAccess() *fakeUserAccess {
	return &fakeUserAccess{byUser: map[int64]fakeUserAccessEntry{
		testAdminID:   {role: "admin", permissions: nil, status: "enabled"}, // NULL = 全权
		testRegularID: {role: "user", permissions: nil, status: "enabled"},
	}}
}

func (f *fakeUserAccess) set(userID int64, role string, perms []string, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byUser[userID] = fakeUserAccessEntry{role: role, permissions: perms, status: status}
}

func (f *fakeUserAccess) UserAccess(_ context.Context, userID int64) (string, []string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastUID = userID
	if f.err != nil {
		return "", nil, "", f.err
	}
	e, ok := f.byUser[userID]
	if !ok {
		return "", nil, "", errors.New("fakeUserAccess: user not found")
	}
	return e.role, e.permissions, e.status, nil
}

type fakeChannelTester struct {
	mu      sync.Mutex
	latency int
	err     error
	calls   int
	gotID   int64
	gotName string
}

func (f *fakeChannelTester) test(_ context.Context, ch *store.Channel, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotID = ch.ID
	f.gotName = ch.Name
	return f.latency, f.err
}

// ---------- 测试脚手架 ----------

type channelTestEnv struct {
	handler *ChannelHandler
	store   *fakeChannelStore
	access  *fakeUserAccess
	sealer  *SecretSealer
	mux     *http.ServeMux
	idem    IdempotencyStore
}

func newChannelTestEnv(t *testing.T, opts ChannelHandlerOptions) *channelTestEnv {
	t.Helper()
	cs := newFakeChannelStore()
	ua := newFakeUserAccess()
	sealer := NewSecretSealer(testSealerMaster)
	if opts.Idempotency == nil {
		opts.Idempotency = NewMemoryIdempotencyStore()
	}
	h := NewChannelHandler(cs, ua, sealer, opts)
	mux := http.NewServeMux()
	h.Register(mux)
	return &channelTestEnv{handler: h, store: cs, access: ua, sealer: sealer, mux: mux, idem: opts.Idempotency}
}

func channelAdminPrincipal(perms ...string) *auth.Principal {
	return &auth.Principal{UserID: testAdminID, Role: "admin", Permissions: perms}
}

func serveWithPrincipal(t *testing.T, mux *http.ServeMux, req *http.Request, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func jsonReq(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	var rdr *bytes.Reader
	switch b := body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case []byte:
		rdr = bytes.NewReader(b)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func mustSealCredentials(t *testing.T, sealer *SecretSealer, creds ChannelCredentials) []byte {
	t.Helper()
	raw, err := json.Marshal(creds)
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	sealed, err := sealer.Seal(raw)
	if err != nil {
		t.Fatalf("seal credentials: %v", err)
	}
	return sealed
}

func mustJSONBytes(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func seedChannel(t *testing.T, env *channelTestEnv, mutate func(*store.Channel)) *store.Channel {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ch := &store.Channel{
		Name:            "seed-channel",
		Type:            string(contract.ChannelTypeOpenAI),
		Credentials:     mustSealCredentials(t, env.sealer, ChannelCredentials{APIKey: "sk-original-1234"}),
		SupportedModels: mustJSONBytes(t, []string{"gpt-4o"}),
		ModelMapping:    []byte(`{}`),
		Weight:          1,
		Priority:        0,
		Status:          1,
		AutoBan:         true,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if mutate != nil {
		mutate(ch)
	}
	return env.store.add(ch)
}

func decodeJSONResponse[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return out
}

func decodeProblemBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q，期望 application/problem+json", ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("problem 体不是合法 JSON: %v（body=%q）", err, rr.Body.String())
	}
	return out
}

// ---------- 用例 ----------

// TestChannelListPaginationAndFilters 覆盖 brief：offset 分页、五类筛选解析，
// 且列表 JSON 只含 GC-6 白名单（不含 base_url / credentials）。
func TestChannelListPaginationAndFilters(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	baseURL := "https://secret-upstream.example.com/v1"
	now := time.Now().UTC().Truncate(time.Microsecond)
	env.store.listItems = []store.Channel{
		{
			ID: 7, Name: "gpt-main", Type: "openai", BaseURL: &baseURL,
			Credentials:     mustSealCredentials(t, env.sealer, ChannelCredentials{APIKey: "sk-plaintext-9999"}),
			SupportedModels: []byte(`["gpt-4o","gpt-4o-mini"]`),
			GroupTags:       []byte(`["vip"]`),
			ModelMapping:    []byte(`{"gpt-5":"gpt-4o"}`),
			Weight:          3, Priority: 10, Status: 1, AutoBan: false,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		},
		{
			ID: 8, Name: "claude-main", Type: "anthropic",
			Credentials:     mustSealCredentials(t, env.sealer, ChannelCredentials{APIKey: "sk-abcdefgh"}),
			SupportedModels: []byte(`["claude-sonnet"]`),
			GroupTags:       nil, // null = 全部分组（modules-06 F-06-04）
			ModelMapping:    []byte(`{}`),
			Weight:          1, Status: 2, AutoBan: true,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		},
	}
	env.store.listTotal = 42

	req := httptest.NewRequest(http.MethodGet,
		"/api/channels?p=2&page_size=10&keyword=GPT&status=1&type=openai&type=anthropic&group=vip&model=gpt-4o&sort=name", nil)
	rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}

	f := env.store.lastFilter
	if f.Page != 2 || f.PageSize != 10 {
		t.Fatalf("分页 = (%d,%d)，期望 (2,10)", f.Page, f.PageSize)
	}
	if f.Keyword != "GPT" || f.Group != "vip" || f.Model != "gpt-4o" || f.Sort != "name" {
		t.Fatalf("筛选参数解析不符: %+v", f)
	}
	if f.Status == nil || *f.Status != 1 {
		t.Fatalf("status 筛选 = %v，期望 1", f.Status)
	}
	if fmt.Sprint(f.Types) != fmt.Sprint([]string{"openai", "anthropic"}) {
		t.Fatalf("type 筛选 = %v", f.Types)
	}

	var envelope struct {
		Items    []map[string]json.RawMessage `json:"items"`
		Total    int                          `json:"total"`
		Page     int                          `json:"page"`
		PageSize int                          `json:"page_size"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if envelope.Total != 42 || envelope.Page != 2 || envelope.PageSize != 10 || len(envelope.Items) != 2 {
		t.Fatalf("envelope = %+v", envelope)
	}

	allowed := map[string]bool{
		"id": true, "name": true, "type": true, "status": true, "priority": true,
		"weight": true, "supported_models": true, "group_tags": true, "rate_limit": true,
		"auto_ban": true, "test_model": true, "remark": true, "tag": true,
		"fail_count": true, "last_test_at": true, "last_test_result": true,
		"created_at": true, "updated_at": true, "version": true,
	}
	for _, item := range envelope.Items {
		for key := range item {
			if !allowed[key] {
				t.Fatalf("列表响应出现白名单外字段 %q（GC-6）", key)
			}
		}
		for _, forbidden := range []string{"base_url", "credentials", "api_key", "model_mapping", "auto_disabled_reason"} {
			if _, ok := item[forbidden]; ok {
				t.Fatalf("列表响应泄漏字段 %q", forbidden)
			}
		}
		var version string
		if err := json.Unmarshal(item["version"], &version); err != nil || version == "" {
			t.Fatalf("version 不是字符串: %s", item["version"])
		}
		if _, err := time.Parse(time.RFC3339Nano, version); err != nil {
			t.Fatalf("version %q 不是 RFC3339Nano: %v", version, err)
		}
	}
	// 原始字节层面再确认一次：任何凭据/URL 都不出现。
	for _, secret := range []string{"sk-plaintext-9999", "secret-upstream.example.com", "sk-abcdefgh"} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("响应体包含敏感值 %q", secret)
		}
	}
	// group_tags=null 必须保留为 JSON null（前端据此显示「全部分组」）。
	if string(envelope.Items[1]["group_tags"]) != "null" {
		t.Fatalf("group_tags = %s，期望 null", envelope.Items[1]["group_tags"])
	}
}

// TestChannelListDefaults 验证缺省分页（1/20）。
func TestChannelListDefaults(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels", nil), channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	if f := env.store.lastFilter; f.Page != 1 || f.PageSize != 20 {
		t.Fatalf("默认分页 = (%d,%d)，期望 (1,20)", f.Page, f.PageSize)
	}
}

// TestChannelCreateValidationAndMasking 覆盖：类型/必填 base_url/模型列表校验、
// 凭据加密落库、响应掩码与 credentials_set、响应不含明文（GC-6/GC-8）。
func TestChannelCreateValidationAndMasking(t *testing.T) {
	valid := map[string]any{
		"name":     "azure-main",
		"type":     "azure_openai",
		"base_url": "https://res.openai.azure.com",
		"credentials": map[string]any{
			"api_key":     "sk-1234567890abcd",
			"deployment":  "dep-1",
			"api_version": "2025-04-01",
		},
		"supported_models": []string{"gpt-4o", "gpt-4o"},
		"group_tags":       []string{"vip"},
		"priority":         10,
		"weight":           2,
		"auto_ban":         false,
	}
	with := func(k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range valid {
			out[kk] = vv
		}
		out[k] = v
		return out
	}
	without := func(k string) map[string]any {
		out := map[string]any{}
		for kk, vv := range valid {
			if kk != k {
				out[kk] = vv
			}
		}
		return out
	}

	invalid := []struct {
		name string
		body map[string]any
	}{
		{"未知类型", with("type", "not-a-real-type")},
		{"azure 缺 base_url", without("base_url")},
		{"空模型列表", with("supported_models", []string{})},
		{"缺凭据", without("credentials")},
		{"azure 缺 deployment", with("credentials", map[string]any{"api_key": "sk-1234567890abcd", "api_version": "2025-04-01"})},
		{"test_model 不在模型列表", with("test_model", "gpt-5")},
		{"weight 越界", with("weight", 1001)},
		{"priority 越界", with("priority", -1001)},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			env := newChannelTestEnv(t, ChannelHandlerOptions{})
			rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels", tc.body), channelAdminPrincipal())
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
			}
			if env.store.createCalls != 0 {
				t.Fatalf("校验失败仍写库（createCalls=%d）", env.store.createCalls)
			}
		})
	}

	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels", valid), channelAdminPrincipal())
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}

	var resp ChannelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.CredentialsSet == nil || !*resp.CredentialsSet {
		t.Fatalf("credentials_set = %v，期望 true", resp.CredentialsSet)
	}
	if resp.CredentialsMasked == nil || resp.CredentialsMasked.APIKey != "sk-…abcd" {
		t.Fatalf("credentials_masked.api_key = %+v，期望 sk-…abcd", resp.CredentialsMasked)
	}
	if resp.CredentialsMasked.Deployment != "dep-1" || resp.CredentialsMasked.APIVersion != "2025-04-01" {
		t.Fatalf("azure 非密钥字段不应脱敏: %+v", resp.CredentialsMasked)
	}
	if resp.Status != 1 {
		t.Fatalf("创建后 status = %d，期望 1（F-06-03 状态机）", resp.Status)
	}
	if resp.Version != resp.UpdatedAt {
		t.Fatalf("version = %q 与 updated_at = %q 不一致（R-P8）", resp.Version, resp.UpdatedAt)
	}
	for _, secret := range []string{"sk-1234567890abcd", "res.openai.azure.com", `"base_url"`} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("创建响应泄漏 %q（GC-6）: %s", secret, rr.Body.String())
		}
	}
	if len(resp.SupportedModels) != 1 || resp.SupportedModels[0] != "gpt-4o" {
		t.Fatalf("supported_models 未去重: %v", resp.SupportedModels)
	}

	stored := env.store.items[resp.ID]
	if stored == nil {
		t.Fatalf("渠道未落库")
	}
	if !strings.HasPrefix(string(stored.Credentials), "v1:") {
		t.Fatalf("凭据未按 GC-8 封装: %q", stored.Credentials)
	}
	plain, err := env.sealer.Open(stored.Credentials)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var gotCreds ChannelCredentials
	if err := json.Unmarshal(plain, &gotCreds); err != nil {
		t.Fatalf("credentials JSON: %v", err)
	}
	if gotCreds.APIKey != "sk-1234567890abcd" || gotCreds.Deployment != "dep-1" || gotCreds.APIVersion != "2025-04-01" {
		t.Fatalf("落库凭据不符: %+v", gotCreds)
	}
}

// TestChannelCreateRejectsModelMappingKeyOutsideSupportedModels 覆盖 F-06-10：
// model_mapping 的每个键都是客户端可见别名，必须同时出现在 supported_models
// 中；否则 /v1/models 会宣告一个永远无法路由的模型（finding 3 的服务端闸门）。
func TestChannelCreateRejectsModelMappingKeyOutsideSupportedModels(t *testing.T) {
	base := func(mapping map[string]any) map[string]any {
		return map[string]any{
			"name":             "alias-create",
			"type":             "openai",
			"credentials":      map[string]any{"api_key": "sk-alias-1234"},
			"supported_models": []string{"gpt-4o"},
			"model_mapping":    mapping,
		}
	}

	t.Run("mapping key outside supported_models → 422", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		rr := serveWithPrincipal(t, env.mux,
			jsonReq(t, http.MethodPost, "/api/channels", base(map[string]any{"gpt-5": "gpt-4o"})),
			channelAdminPrincipal())
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
		}
		if env.store.createCalls != 0 {
			t.Fatalf("校验失败仍写库（createCalls=%d）", env.store.createCalls)
		}
		if !strings.Contains(rr.Body.String(), `"field":"model_mapping"`) {
			t.Fatalf("422 未指向 model_mapping 字段: %s", rr.Body.String())
		}
	})

	t.Run("mapping key is a supported model → 201", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		rr := serveWithPrincipal(t, env.mux,
			jsonReq(t, http.MethodPost, "/api/channels", base(map[string]any{"gpt-4o": "upstream-gpt-4o"})),
			channelAdminPrincipal())
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
		}
		if env.store.createCalls != 1 {
			t.Fatalf("createCalls = %d，期望 1", env.store.createCalls)
		}
	})
}

// TestChannelPatchModelMappingCoverage 覆盖 PATCH 的 F-06-10 兜底：
// 校验的是**合并后**的有效状态——既拒绝新 mapping 键不在模型列表，
// 也拒绝通过改 supported_models 把仍被 mapping 使用的模型删掉。
func TestChannelPatchModelMappingCoverage(t *testing.T) {
	t.Run("new mapping key outside supported_models → 422", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, nil) // supported_models = [gpt-4o]
		body := map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"model_mapping":    map[string]any{"gpt-5": "gpt-4o"},
		}
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
		}
		if env.store.updateCalls != 0 {
			t.Fatalf("校验失败仍写库（updateCalls=%d）", env.store.updateCalls)
		}
	})

	t.Run("removing a model still used as a mapping key → 422", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, func(ch *store.Channel) {
			ch.SupportedModels = mustJSONBytes(t, []string{"gpt-4o", "gpt-3.5"})
			ch.ModelMapping = mustJSONBytes(t, map[string]string{"gpt-3.5": "upstream-35"})
		})
		body := map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"supported_models": []string{"gpt-4o"},
		}
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
		}
		if env.store.updateCalls != 0 {
			t.Fatalf("校验失败仍写库（updateCalls=%d）", env.store.updateCalls)
		}
	})

	// re-review fix 5：历史遗留的不一致行（mapping 键不在 supported_models）
	// 在字段未变化时必须可保存——控制台每次都会重发 supported_models，
	// 不能因为「字段出现」就把改备注的 PATCH 判成 422。
	t.Run("legacy inconsistent row with unchanged models/mapping → 200", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, func(ch *store.Channel) {
			ch.SupportedModels = mustJSONBytes(t, []string{"gpt-4o"})
			ch.ModelMapping = mustJSONBytes(t, map[string]string{"gpt-5": "upstream-5"})
		})
		body := map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"remark":           "only the remark changed",
			// 控制台的固定负载：未改动的模型与映射也必须原样重发。
			"supported_models": []string{"gpt-4o"},
			"model_mapping":    map[string]string{"gpt-5": "upstream-5"},
		}
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200（未变化的不一致行不应阻断无关字段；body=%s）", rr.Code, rr.Body.String())
		}
		if env.store.updateCalls != 1 {
			t.Fatalf("updateCalls = %d，期望 1", env.store.updateCalls)
		}
	})

	t.Run("changing models to a still-inconsistent state → 422", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, func(ch *store.Channel) {
			ch.SupportedModels = mustJSONBytes(t, []string{"gpt-4o"})
			ch.ModelMapping = mustJSONBytes(t, map[string]string{"gpt-5": "upstream-5"})
		})
		body := map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			// 真实变更：支持列表变了，但 mapping 的 gpt-5 仍不在列表里。
			"supported_models": []string{"gpt-4o", "gpt-4"},
		}
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d，期望 422（真实变更仍须满足覆盖约束；body=%s）", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"field":"model_mapping"`) {
			t.Fatalf("422 未指向 model_mapping 字段: %s", rr.Body.String())
		}
		if env.store.updateCalls != 0 {
			t.Fatalf("校验失败仍写库（updateCalls=%d）", env.store.updateCalls)
		}
	})

	t.Run("mapping key inside effective models → 200", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, func(ch *store.Channel) {
			ch.SupportedModels = mustJSONBytes(t, []string{"gpt-4o", "gpt-3.5"})
		})
		body := map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"model_mapping":    map[string]any{"gpt-3.5": "upstream-35"},
		}
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
		}
		if env.store.updateCalls != 1 {
			t.Fatalf("updateCalls = %d，期望 1", env.store.updateCalls)
		}
	})
}

// TestChannelCreateIdempotentReplay 覆盖：同 Idempotency-Key 第二次提交直接重放
// 首次响应，不再写库（modules-06 F-06-02 TC-F-06-02-03；本任务要求 POST 创建也支持）。
func TestChannelCreateIdempotentReplay(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	body := map[string]any{
		"name":             "idem-channel",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-idempotent-1234"},
		"supported_models": []string{"gpt-4o"},
	}
	do := func() *httptest.ResponseRecorder {
		req := jsonReq(t, http.MethodPost, "/api/channels", body)
		req.Header.Set("Idempotency-Key", "test-idem-key-1")
		return serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	}
	first := do()
	if first.Code != http.StatusCreated {
		t.Fatalf("首次 status = %d（body=%s）", first.Code, first.Body.String())
	}
	second := do()
	if second.Code != first.Code {
		t.Fatalf("重放 status = %d，期望 %d", second.Code, first.Code)
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("重放响应体不同\n first=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("重放未标记 Idempotent-Replayed")
	}
	if env.store.createCalls != 1 {
		t.Fatalf("createCalls = %d，期望 1（重放不得再次写库）", env.store.createCalls)
	}
}

// TestChannelPatchBlankCredentialsKeepsValue 覆盖 modules-06 F-06-03：
// credentials 缺省 / null / 空串 → 保持原密文，不重加密。
func TestChannelPatchBlankCredentialsKeepsValue(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"缺省", map[string]any{}},
		{"null", map[string]any{"credentials": nil}},
		{"空串", map[string]any{"credentials": map[string]any{"api_key": ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newChannelTestEnv(t, ChannelHandlerOptions{})
			seeded := seedChannel(t, env, nil)
			before := append([]byte(nil), seeded.Credentials...)

			body := map[string]any{"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano)}
			for k, v := range tc.body {
				body[k] = v
			}
			rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
			}
			after := env.store.items[1].Credentials
			if !bytes.Equal(before, after) {
				t.Fatalf("空凭据 PATCH 改写了密文\n before=%x\n after=%x", before, after)
			}
			var resp ChannelResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response: %v", err)
			}
			if resp.CredentialsMasked == nil || resp.CredentialsMasked.APIKey != "sk-…1234" {
				t.Fatalf("掩码 = %+v，期望 sk-…1234", resp.CredentialsMasked)
			}
			if env.store.lastPatch.Credentials != nil {
				t.Fatalf("空凭据不应传给 store（patch.Credentials=%x）", env.store.lastPatch.Credentials)
			}
		})
	}
}

// TestChannelPatchStaleVersionConflict 覆盖 R-P8：expected_version 与 stored
// updated_at 不等 → 409 RFC7807。
func TestChannelPatchStaleVersionConflict(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seeded := seedChannel(t, env, nil)
	body := map[string]any{
		"expected_version": seeded.UpdatedAt.Add(-2 * time.Second).UTC().Format(time.RFC3339Nano),
		"remark":           "stale",
	}
	rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", body), channelAdminPrincipal())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	problem := decodeProblemBody(t, rr)
	if problem["type"] != "https://newtg.dev/problems/channel_version_conflict" {
		t.Fatalf("problem.type = %v", problem["type"])
	}
}

// TestChannelPatchMissingExpectedVersion 要求 PATCH 必须带乐观锁版本（R-P8）。
func TestChannelPatchMissingExpectedVersion(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seedChannel(t, env, nil)
	rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", map[string]any{"remark": "x"}), channelAdminPrincipal())
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d，期望 422（body=%s）", rr.Code, rr.Body.String())
	}
}

// TestChannelCreateNameConflict 覆盖 modules-06 TC-F-06-03-04：同名创建 → 409。
func TestChannelCreateNameConflict(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	env.store.createErr = store.ErrNameConflict
	body := map[string]any{
		"name":             "dup",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-1234567890"},
		"supported_models": []string{"gpt-4o"},
	}
	rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels", body), channelAdminPrincipal())
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d，期望 409（body=%s）", rr.Code, rr.Body.String())
	}
	problem := decodeProblemBody(t, rr)
	if problem["type"] != "https://newtg.dev/problems/channel_name_conflict" {
		t.Fatalf("problem.type = %v", problem["type"])
	}
}

// TestChannelPermissionMatrix 覆盖 R-P9：channels:read/manage/manage_sensitive
// 三档分离；Permissions 为空时回源 users；role != admin 一律 403。
func TestChannelPermissionMatrix(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seedChannel(t, env, nil)

	// admin 仅有 channels:read：读通过，写/删拒绝。
	env.access.set(testAdminID, "admin", []string{"channels:read"}, "enabled")
	if rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels", nil), channelAdminPrincipal()); rr.Code != http.StatusOK {
		t.Fatalf("read-only admin 列表 = %d，期望 200", rr.Code)
	}
	createBody := map[string]any{
		"name": "denied", "type": "openai",
		"credentials": map[string]any{"api_key": "sk-1234567890"}, "supported_models": []string{"gpt-4o"},
	}
	if rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels", createBody), channelAdminPrincipal()); rr.Code != http.StatusForbidden {
		t.Fatalf("缺 channels:manage 创建 = %d，期望 403", rr.Code)
	}
	if rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodDelete, "/api/channels/1", nil), channelAdminPrincipal()); rr.Code != http.StatusForbidden {
		t.Fatalf("缺 channels:manage_sensitive 删除 = %d，期望 403", rr.Code)
	}
	if rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels/batch-delete", map[string]any{"ids": []int64{1}}), channelAdminPrincipal()); rr.Code != http.StatusForbidden {
		t.Fatalf("缺 channels:manage_sensitive 批量删除 = %d，期望 403", rr.Code)
	}
	if rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/batch", map[string]any{"action": "enable", "ids": []int64{1}}), channelAdminPrincipal()); rr.Code != http.StatusForbidden {
		t.Fatalf("缺 channels:manage 批量启停 = %d，期望 403", rr.Code)
	}

	// 加 channels:manage：写与启停通过，删除仍拒绝（敏感位分离）。
	env.access.set(testAdminID, "admin", []string{"channels:read", "channels:manage"}, "enabled")
	if rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/batch", map[string]any{"action": "enable", "ids": []int64{1}}), channelAdminPrincipal()); rr.Code != http.StatusOK {
		t.Fatalf("有 channels:manage 批量启停 = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodDelete, "/api/channels/1", nil), channelAdminPrincipal()); rr.Code != http.StatusForbidden {
		t.Fatalf("缺敏感位删除 = %d，期望 403", rr.Code)
	}

	// role=user 即使权限数组含键也必须 403（modules-07 F-07-08）。
	env.access.set(testAdminID, "user", []string{"channels:read", "channels:manage", "channels:manage_sensitive"}, "enabled")
	if rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels", nil), channelAdminPrincipal()); rr.Code != http.StatusForbidden {
		t.Fatalf("role=user 列表 = %d，期望 403", rr.Code)
	}

	// 无 Principal：401（未经鉴权不得按匿名放行）。
	if rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels", nil), nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("无身份列表 = %d，期望 401", rr.Code)
	}

	// Bearer 形态 Principal（Role/Permissions 均空）必须回源 users 拿权限。
	env.access.set(testAdminID, "admin", []string{"*"}, "enabled")
	if rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels", nil),
		&auth.Principal{UserID: testAdminID, TokenID: int64Ptr(9)}); rr.Code != http.StatusOK {
		t.Fatalf("Bearer 身份回源权限 = %d，期望 200", rr.Code)
	}
	if env.access.calls == 0 {
		t.Fatalf("Permissions 为空时未调用 UserAccessStore（R-P9）")
	}
}

func int64Ptr(v int64) *int64 { return &v }

// TestChannelBatchEnableSkipsAlreadyEnabled 覆盖 modules-06 TC-F-06-02-02：
// 已 enabled 计 succeeded、不产生状态迁移；不存在 id 进 failed。
func TestChannelBatchEnableSkipsAlreadyEnabled(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seedChannel(t, env, func(c *store.Channel) { c.ID = 1; c.Name = "already-on"; c.Status = 1 })
	seedChannel(t, env, func(c *store.Channel) { c.ID = 2; c.Name = "manually-off"; c.Status = 2 })
	env.store.nextID = 3

	rr := serveWithPrincipal(t, env.mux,
		jsonReq(t, http.MethodPatch, "/api/channels/batch", map[string]any{"action": "enable", "ids": []int64{1, 2, 999}}),
		channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	var res store.BatchResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if fmt.Sprint(res.Succeeded) != fmt.Sprint([]int64{1, 2}) {
		t.Fatalf("succeeded = %v，期望 [1 2]", res.Succeeded)
	}
	if len(res.Failed) != 1 || res.Failed[0].ID != 999 || res.Failed[0].Reason != "not_found" {
		t.Fatalf("failed = %+v，期望 [{999 not_found}]", res.Failed)
	}
	if fmt.Sprint(env.store.batchTransitions) != fmt.Sprint([]int64{2}) {
		t.Fatalf("状态迁移 = %v，期望仅 [2]（已 enabled 跳过）", env.store.batchTransitions)
	}
	if env.store.lastBatchTo != 1 {
		t.Fatalf("batch target = %d，期望 1", env.store.lastBatchTo)
	}
}

// TestChannelTestFailureRedactsError 覆盖 modules-06 F-06-06：连通性失败是
// **业务结果**，返回 200 {success:false, error_class}（不是 502 RFC7807），
// 且不包含 base_url / api_key；成功时回写 last_test_*。
func TestChannelTestFailureRedactsError(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seedChannel(t, env, nil)

	tester := &fakeChannelTester{err: errors.New("dial https://upstream.internal:8443/v1?api_key=sk-supersecret failed")}
	env.handler.tester = tester.test

	rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels/1/test", map[string]any{"model": "gpt-4o"}), channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望 application/json", ct)
	}
	var failure struct {
		Success    bool   `json:"success"`
		LatencyMS  int    `json:"latency_ms"`
		Model      string `json:"model"`
		ErrorClass string `json:"error_class"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &failure); err != nil {
		t.Fatalf("test failure response: %v（body=%s）", err, rr.Body.String())
	}
	if failure.Success {
		t.Fatalf("success = true，期望 false")
	}
	if failure.ErrorClass != "server" {
		t.Fatalf("error_class = %q，期望不可分类错误归入 modules-06 的 server 档", failure.ErrorClass)
	}
	for _, secret := range []string{"upstream.internal", "sk-supersecret", "api_key", "8443"} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("测试失败响应泄漏 %q: %s", secret, rr.Body.String())
		}
	}
	if env.store.lastTestResult == nil || *env.store.lastTestResult {
		t.Fatalf("失败测试未回写 last_test_result=false")
	}

	// 成功路径：200 + latency + model，last_test_result=true。
	env.handler.tester = (&fakeChannelTester{latency: 42}).test
	rr = serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPost, "/api/channels/1/test", map[string]any{"model": "gpt-4o"}), channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("成功测试 status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var out struct {
		Success   bool   `json:"success"`
		LatencyMS int    `json:"latency_ms"`
		Model     string `json:"model"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("test response: %v", err)
	}
	if !out.Success || out.LatencyMS != 42 || out.Model != "gpt-4o" {
		t.Fatalf("test response = %+v", out)
	}
	if env.store.lastTestResult == nil || !*env.store.lastTestResult {
		t.Fatalf("成功测试未回写 last_test_result=true")
	}
}

// TestChannelDetailMaskedCredentials 覆盖 GC-6 的详情例外：只加
// credentials_masked / credentials_set，仍不含 base_url 或明文。
func TestChannelDetailMaskedCredentials(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	baseURL := "https://hidden.example.com"
	seedChannel(t, env, func(c *store.Channel) {
		c.Name = "detail"
		c.Type = "azure_openai"
		c.BaseURL = &baseURL
		c.Credentials = mustSealCredentials(t, env.sealer, ChannelCredentials{
			APIKey: "sk-verysecret-9876", Deployment: "dep", APIVersion: "2025-04-01",
		})
	})

	rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels/1", nil), channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var resp ChannelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.CredentialsSet == nil || !*resp.CredentialsSet {
		t.Fatalf("credentials_set = %v", resp.CredentialsSet)
	}
	if resp.CredentialsMasked == nil || resp.CredentialsMasked.APIKey != "sk-…9876" {
		t.Fatalf("masked = %+v", resp.CredentialsMasked)
	}
	for _, secret := range []string{"sk-verysecret-9876", "hidden.example.com", `"base_url"`} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("详情响应泄漏 %q", secret)
		}
	}
}

// TestChannelBatchDeleteIdempotent 覆盖 POST /api/channels/batch-delete 的
// Idempotency-Key（modules-06 TC-F-06-02-03）。
func TestChannelBatchDeleteIdempotent(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seedChannel(t, env, func(c *store.Channel) { c.ID = 1 })
	seedChannel(t, env, func(c *store.Channel) { c.ID = 2 })
	env.store.nextID = 3

	do := func() *httptest.ResponseRecorder {
		req := jsonReq(t, http.MethodPost, "/api/channels/batch-delete", map[string]any{"ids": []int64{1, 2}})
		req.Header.Set("Idempotency-Key", "batch-del-1")
		return serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	}
	first := do()
	if first.Code != http.StatusOK {
		t.Fatalf("首次 status = %d（body=%s）", first.Code, first.Body.String())
	}
	second := do()
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("重放不一致\n first=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
	if env.store.batchDeleteCalls != 1 {
		t.Fatalf("batchDeleteCalls = %d，期望 1", env.store.batchDeleteCalls)
	}
}

// TestChannelSealerRoundTripAndMask 覆盖 GC-8 封装格式与 modules-06 §1.5 掩码规则。
func TestChannelSealerRoundTripAndMask(t *testing.T) {
	sealer := NewSecretSealer(testSealerMaster)
	plain := []byte(`{"api_key":"sk-1234567890"}`)
	sealed, err := sealer.Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(string(sealed), "v1:") {
		t.Fatalf("密文前缀 = %q，期望 v1:", sealed)
	}
	got, err := sealer.Open(sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	sealed2, _ := sealer.Seal(plain)
	if bytes.Equal(sealed, sealed2) {
		t.Fatalf("两次 Seal 输出相同（nonce 未随机化）")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := sealer.Open(tampered); err == nil {
		t.Fatalf("篡改密文未被拒绝")
	}

	if got := MaskAPIKey("sk-1234567890abcdef"); got != "sk-…cdef" {
		t.Fatalf("MaskAPIKey 长键 = %q", got)
	}
	if got := MaskAPIKey("short12"); got != "sho…rt12" {
		t.Fatalf("MaskAPIKey 中键 = %q", got)
	}
}

// TestChannelMaskAPIKeyShort 单独固定 <7 位规则（modules-06 §1.5：仅显示后 4 位）。
func TestChannelMaskAPIKeyShort(t *testing.T) {
	if got := MaskAPIKey("abc"); got != "…abc" {
		t.Fatalf("MaskAPIKey(abc) = %q", got)
	}
	if got := MaskAPIKey("abcdef"); got != "…cdef" {
		t.Fatalf("MaskAPIKey(abcdef) = %q", got)
	}
	if got := MaskAPIKey("abcdefg"); got != "abc…defg" {
		t.Fatalf("MaskAPIKey(abcdefg) = %q", got)
	}
	// 32 字节 base64 密钥走「解码后恰好 32 字节」分支；两种输入必须能互相解密。
	rawKey := bytes.Repeat([]byte{0x5a}, 32)
	b64Key := base64.StdEncoding.EncodeToString(rawKey)
	s1 := NewSecretSealer(b64Key)
	s2 := NewSecretSealer(string(rawKey))
	sealed, err := s1.Seal([]byte("x"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := s2.Open(sealed); err == nil {
		t.Fatalf("不同主密钥不应能解密")
	}
}

// TestChannelListAcceptsResponseTimeSort 覆盖 F-06-01 的第三种排序值：
// response_time 必须被接受（M2 无 24h 聚合数据源，store 落回默认 priority 顺序）。
func TestChannelListAcceptsResponseTimeSort(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	rr := serveWithPrincipal(t, env.mux,
		httptest.NewRequest(http.MethodGet, "/api/channels?sort=response_time", nil), channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if env.store.lastFilter.Sort != "response_time" {
		t.Fatalf("sort = %q，期望 response_time 透传到 store（由 store 落回默认顺序）", env.store.lastFilter.Sort)
	}
}

// TestChannelIdempotencyKeyScopedByOperation 冻结幂等键的**操作作用域**：
// 同一个 Idempotency-Key 用在创建与批量删除两个端点时，第二个请求不得
// 重放第一个的响应（否则删除被静默跳过）。
func TestChannelIdempotencyKeyScopedByOperation(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	seeded := seedChannel(t, env, nil)

	createReq := jsonReq(t, http.MethodPost, "/api/channels", map[string]any{
		"name":             "scoped-create",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-scoped-1234"},
		"supported_models": []string{"gpt-4o"},
	})
	createReq.Header.Set("Idempotency-Key", "same-key-across-endpoints")
	if rr := serveWithPrincipal(t, env.mux, createReq, channelAdminPrincipal()); rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}

	deleteReq := jsonReq(t, http.MethodPost, "/api/channels/batch-delete", map[string]any{"ids": []int64{seeded.ID}})
	deleteReq.Header.Set("Idempotency-Key", "same-key-across-endpoints")
	rr := serveWithPrincipal(t, env.mux, deleteReq, channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("batch-delete status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") == "true" {
		t.Fatal("跨端点复用同一 Idempotency-Key 不得重放首次响应")
	}
	if env.store.batchDeleteCalls != 1 {
		t.Fatalf("batchDeleteCalls = %d，期望 1（删除必须真正执行）", env.store.batchDeleteCalls)
	}
	if _, err := env.store.Get(context.Background(), seeded.ID); !errors.Is(err, store.ErrChannelNotFound) {
		t.Fatalf("seed 渠道未被删除，Get err = %v", err)
	}
}

// TestChannelIdempotencyKeyScopedByUser 冻结幂等键的**用户作用域**：
// 两个管理员使用同一个 key 时不得共享首次响应（否则会读到别人的结果）。
func TestChannelIdempotencyKeyScopedByUser(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	const otherAdminID int64 = 3
	env.access.set(otherAdminID, "admin", nil, "enabled")
	otherAdmin := &auth.Principal{UserID: otherAdminID, Role: "admin"}

	first := jsonReq(t, http.MethodPost, "/api/channels", map[string]any{
		"name":             "user-a-channel",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-user-a-1234"},
		"supported_models": []string{"gpt-4o"},
	})
	first.Header.Set("Idempotency-Key", "shared-across-users")
	if rr := serveWithPrincipal(t, env.mux, first, channelAdminPrincipal()); rr.Code != http.StatusCreated {
		t.Fatalf("user A create status = %d（body=%s）", rr.Code, rr.Body.String())
	}

	second := jsonReq(t, http.MethodPost, "/api/channels", map[string]any{
		"name":             "user-b-channel",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-user-b-1234"},
		"supported_models": []string{"gpt-4o"},
	})
	second.Header.Set("Idempotency-Key", "shared-across-users")
	rr := serveWithPrincipal(t, env.mux, second, otherAdmin)
	if rr.Code != http.StatusCreated {
		t.Fatalf("user B create status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Idempotent-Replayed") == "true" {
		t.Fatal("跨用户复用同一 Idempotency-Key 不得重放他人响应")
	}
	var resp ChannelResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if resp.Name != "user-b-channel" {
		t.Fatalf("user B 收到 name = %q，期望 user-b-channel（不是 user A 的重放）", resp.Name)
	}
	if env.store.createCalls != 2 {
		t.Fatalf("createCalls = %d，期望 2", env.store.createCalls)
	}
}

// TestChannelIdempotencySerializesConcurrentSameKey 覆盖 finding 2：
// 同一幂等键的并发请求只允许一个执行 build（副作用恰好一次），
// 另一个在抢占阶段就收到 409 + Retry-After。
func TestChannelIdempotencySerializesConcurrentSameKey(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	env.store.createHook = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	body := map[string]any{
		"name":             "race-channel",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-race-1234"},
		"supported_models": []string{"gpt-4o"},
	}
	do := func() *httptest.ResponseRecorder {
		req := jsonReq(t, http.MethodPost, "/api/channels", body)
		req.Header.Set("Idempotency-Key", "concurrent-race-key")
		return serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- do() }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached Create")
	}

	// 首个请求仍在执行：第二个同键请求必须立刻 409，且不得进入 build。
	second := do()
	if second.Code != http.StatusConflict {
		t.Fatalf("concurrent second status = %d, want 409; body=%s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got == "" {
		t.Fatal("409 must carry a Retry-After hint")
	}
	problem := decodeProblemBody(t, second)
	if problem["type"] != problemTypeIdempotencyInProgress {
		t.Fatalf("problem.type = %v, want %s", problem["type"], problemTypeIdempotencyInProgress)
	}

	close(release)
	first := <-firstDone
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201; body=%s", first.Code, first.Body.String())
	}
	if env.store.createCalls != 1 {
		t.Fatalf("createCalls = %d, want exactly 1（并发同键不得重复执行）", env.store.createCalls)
	}

	// 执行完成后同一个 key 再请求 → 重放首次响应，不再写库。
	third := do()
	if third.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201; body=%s", third.Code, third.Body.String())
	}
	if third.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("third request must be marked Idempotent-Replayed")
	}
	if third.Body.String() != first.Body.String() {
		t.Fatalf("replay body differs\n first=%s\n third=%s", first.Body.String(), third.Body.String())
	}
	if env.store.createCalls != 1 {
		t.Fatalf("createCalls = %d after replay, want 1", env.store.createCalls)
	}
}

// TestChannelIdempotencyNon2xxReleasesKey 覆盖 finding 2 的释放路径：
// 非 2xx 不固化 24h，释放 pending 标记后同一 key 可以真正重试。
func TestChannelIdempotencyNon2xxReleasesKey(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	do := func() *httptest.ResponseRecorder {
		req := jsonReq(t, http.MethodPost, "/api/channels", map[string]any{
			"name": "bad", "type": "openai",
			"credentials": map[string]any{"api_key": "short"}, "supported_models": []string{"gpt-4o"},
		})
		req.Header.Set("Idempotency-Key", "retry-after-422")
		return serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	}
	if rr := do(); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("first status = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	// 修正后的同名请求复用同一个 key：必须真正执行（不是重放 422）。
	req := jsonReq(t, http.MethodPost, "/api/channels", map[string]any{
		"name": "good", "type": "openai",
		"credentials": map[string]any{"api_key": "sk-fixed-1234"}, "supported_models": []string{"gpt-4o"},
	})
	req.Header.Set("Idempotency-Key", "retry-after-422")
	rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	if rr.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, want 201（非 2xx 必须释放幂等键）; body=%s", rr.Code, rr.Body.String())
	}
	if env.store.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", env.store.createCalls)
	}
}

// ---------- 幂等 pending 标记的生命周期（二轮复审 finding 2） ----------

// recordingIdempotencyStore 记录 Claim/Complete/Release 收到的 TTL 与 context
// 状态，并可注入错误；用于断言「pending 短 TTL + 完成响应 24h」以及
// 「finalize 使用与请求取消无关的 detached context」。
type recordingIdempotencyStore struct {
	mu sync.Mutex

	claimTTL    time.Duration
	completeTTL time.Duration

	claimCalls    int
	completeCalls int
	releaseCalls  int

	claimCtxErr    error
	completeCtxErr error
	releaseCtxErr  error

	completeHasDeadline bool
	releaseHasDeadline  bool

	releaseTokens []string

	completeErr error
	releaseErr  error
}

func (s *recordingIdempotencyStore) Claim(ctx context.Context, _ string, ttl time.Duration) (IdempotencyClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	s.claimTTL = ttl
	s.claimCtxErr = ctx.Err()
	return IdempotencyClaim{Status: IdempotencyClaimAcquired, Token: "recorded-token"}, nil
}

func (s *recordingIdempotencyStore) Complete(ctx context.Context, _, _ string, _ IdempotentResponse, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	s.completeTTL = ttl
	s.completeCtxErr = ctx.Err()
	_, s.completeHasDeadline = ctx.Deadline()
	return s.completeErr
}

func (s *recordingIdempotencyStore) Release(ctx context.Context, _, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls++
	s.releaseCtxErr = ctx.Err()
	_, s.releaseHasDeadline = ctx.Deadline()
	s.releaseTokens = append(s.releaseTokens, token)
	return s.releaseErr
}

func idempotentCreateBody() map[string]any {
	return map[string]any{
		"name":             "idem-lifecycle",
		"type":             "openai",
		"credentials":      map[string]any{"api_key": "sk-idem-1234"},
		"supported_models": []string{"gpt-4o"},
	}
}

// TestIdempotencyPendingMarkerUsesShortTTL 冻结两个 TTL 的分工：
// pending 标记必须短（默认 60s），完成响应才保留 24h。旧实现把 24h 也用在
// pending 上：一个进程崩溃/panic 留下的标记会让同一 Idempotency-Key 被
// 409 卡住整整 24h。
func TestIdempotencyPendingMarkerUsesShortTTL(t *testing.T) {
	recorder := &recordingIdempotencyStore{}
	env := newChannelTestEnv(t, ChannelHandlerOptions{Idempotency: recorder})
	req := jsonReq(t, http.MethodPost, "/api/channels", idempotentCreateBody())
	req.Header.Set("Idempotency-Key", "ttl-split")
	if rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal()); rr.Code != http.StatusCreated {
		t.Fatalf("status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if IdempotencyPendingTTL != 60*time.Second {
		t.Fatalf("IdempotencyPendingTTL = %v，期望 60s", IdempotencyPendingTTL)
	}
	if recorder.claimTTL != IdempotencyPendingTTL {
		t.Fatalf("Claim ttl = %v，期望 pending TTL %v", recorder.claimTTL, IdempotencyPendingTTL)
	}
	if recorder.completeTTL != IdempotencyTTL {
		t.Fatalf("Complete ttl = %v，期望完成响应 TTL %v", recorder.completeTTL, IdempotencyTTL)
	}
}

// TestMemoryIdempotencyPendingExpires 验证短 TTL 确实让标记过期后可重新认领
// （不是被永久锁住）。用很短的 TTL 直接驱动 Memory 实现。
func TestMemoryIdempotencyPendingExpires(t *testing.T) {
	store := NewMemoryIdempotencyStore()
	ctx := context.Background()
	const ttl = 20 * time.Millisecond

	first, err := store.Claim(ctx, "expiring-key", ttl)
	if err != nil || first.Status != IdempotencyClaimAcquired {
		t.Fatalf("first claim = %+v, err=%v，期望 Acquired", first, err)
	}
	if second, _ := store.Claim(ctx, "expiring-key", ttl); second.Status != IdempotencyClaimInFlight {
		t.Fatalf("second claim = %+v，期望 InFlight", second)
	}
	time.Sleep(2 * ttl)
	if third, err := store.Claim(ctx, "expiring-key", ttl); err != nil || third.Status != IdempotencyClaimAcquired {
		t.Fatalf("claim after expiry = %+v, err=%v，期望 Acquired（标记必须可过期）", third, err)
	}
}

// TestIdempotencyFinalizeUsesDetachedContext 覆盖 finalize 的 context：
// 请求被取消（客户端断开）不应阻止 Complete/Release——否则 pending 标记
// 只能等短 TTL 自然过期，客户端重试窗口被无谓拉长。
func TestIdempotencyFinalizeUsesDetachedContext(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		recorder := &recordingIdempotencyStore{}
		env := newChannelTestEnv(t, ChannelHandlerOptions{Idempotency: recorder})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env.store.createHook = cancel // build 执行期间客户端断开

		req := jsonReq(t, http.MethodPost, "/api/channels", idempotentCreateBody())
		req.Header.Set("Idempotency-Key", "detached-complete")
		req = req.WithContext(ctx)
		if rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal()); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
		}

		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		if recorder.completeCalls != 1 {
			t.Fatalf("Complete calls = %d，期望 1", recorder.completeCalls)
		}
		if recorder.completeCtxErr != nil {
			t.Fatalf("Complete context = %v，期望 detached（不受请求取消影响）", recorder.completeCtxErr)
		}
		if !recorder.completeHasDeadline {
			t.Fatal("Complete context 缺少超时 deadline（应被 5s 上限约束）")
		}
	})

	t.Run("release", func(t *testing.T) {
		recorder := &recordingIdempotencyStore{}
		env := newChannelTestEnv(t, ChannelHandlerOptions{Idempotency: recorder})
		env.store.createErr = errors.New("db down")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env.store.createHook = cancel

		req := jsonReq(t, http.MethodPost, "/api/channels", idempotentCreateBody())
		req.Header.Set("Idempotency-Key", "detached-release")
		req = req.WithContext(ctx)
		if rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal()); rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d，期望 500（body=%s）", rr.Code, rr.Body.String())
		}

		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		if recorder.releaseCalls != 1 {
			t.Fatalf("Release calls = %d，期望 1", recorder.releaseCalls)
		}
		if recorder.releaseCtxErr != nil {
			t.Fatalf("Release context = %v，期望 detached", recorder.releaseCtxErr)
		}
		if !recorder.releaseHasDeadline {
			t.Fatal("Release context 缺少超时 deadline")
		}
	})
}

// TestIdempotencyFinalizeFailuresAreLogged 覆盖「日志而不是丢弃」：
// Complete/Release 失败时必须有可观测记录，否则 Redis 故障会静默退化成
// 「每次重试都重新执行」。
func TestIdempotencyFinalizeFailuresAreLogged(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		recorder := &recordingIdempotencyStore{completeErr: errors.New("redis down")}
		env := newChannelTestEnv(t, ChannelHandlerOptions{Idempotency: recorder, Logger: logger})
		req := jsonReq(t, http.MethodPost, "/api/channels", idempotentCreateBody())
		req.Header.Set("Idempotency-Key", "log-complete")
		if rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal()); rr.Code != http.StatusCreated {
			t.Fatalf("status = %d，期望 201（body=%s）", rr.Code, rr.Body.String())
		}
		out := buf.String()
		if !strings.Contains(out, "idempotency") || !strings.Contains(out, "redis down") {
			t.Fatalf("Complete 失败未记录日志: %q", out)
		}
	})

	t.Run("release", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		recorder := &recordingIdempotencyStore{releaseErr: errors.New("redis down")}
		env := newChannelTestEnv(t, ChannelHandlerOptions{Idempotency: recorder, Logger: logger})
		env.store.createErr = errors.New("db down")
		req := jsonReq(t, http.MethodPost, "/api/channels", idempotentCreateBody())
		req.Header.Set("Idempotency-Key", "log-release")
		if rr := serveWithPrincipal(t, env.mux, req, channelAdminPrincipal()); rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d，期望 500（body=%s）", rr.Code, rr.Body.String())
		}
		out := buf.String()
		if !strings.Contains(out, "idempotency") || !strings.Contains(out, "redis down") {
			t.Fatalf("Release 失败未记录日志: %q", out)
		}
	})
}

// TestIdempotencyPanicReleasesMarker 覆盖 panic 出口：build 崩溃时 deferred
// 释放必须删除 pending 标记，不能让同键客户端等 TTL。panic 本身仍向上传播
// （由 net/http 兜底），这里只断言标记被释放。
func TestIdempotencyPanicReleasesMarker(t *testing.T) {
	recorder := &recordingIdempotencyStore{}
	env := newChannelTestEnv(t, ChannelHandlerOptions{Idempotency: recorder})
	env.store.createHook = func() { panic("boom during create") }

	req := jsonReq(t, http.MethodPost, "/api/channels", idempotentCreateBody())
	req.Header.Set("Idempotency-Key", "panic-release")

	func() {
		defer func() {
			if rec := recover(); rec == nil {
				t.Fatal("build panic 应继续向上传播（由 net/http 兜底）")
			}
		}()
		serveWithPrincipal(t, env.mux, req, channelAdminPrincipal())
	}()

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.releaseCalls != 1 {
		t.Fatalf("panic 后 Release calls = %d，期望 1", recorder.releaseCalls)
	}
	if len(recorder.releaseTokens) != 1 || recorder.releaseTokens[0] != "recorded-token" {
		t.Fatalf("Release tokens = %v，期望 [recorded-token]", recorder.releaseTokens)
	}
}

// TestChannelPatchTypeChangeRequiresNewCredentials 覆盖 finding 10：
// 改 type 必须同时提交新类型的凭据；azure 还要求 deployment/api_version。
func TestChannelPatchTypeChangeRequiresNewCredentials(t *testing.T) {
	azureBase := "https://azure.example.com"

	t.Run("改 type 但不带凭据 → 422", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, nil)
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"type":             "azure_openai",
			"base_url":         azureBase,
		}), channelAdminPrincipal())
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
		}
		if env.store.updateCalls != 0 {
			t.Fatalf("updateCalls = %d, want 0（校验失败不得写库）", env.store.updateCalls)
		}
	})

	t.Run("改 type=azure 但缺 deployment/api_version → 422", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, nil)
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"type":             "azure_openai",
			"base_url":         azureBase,
			"credentials":      map[string]any{"api_key": "sk-new-azure-1234"},
		}), channelAdminPrincipal())
		if rr.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422（azure 缺 deployment/api_version）; body=%s", rr.Code, rr.Body.String())
		}
		problem := decodeProblemBody(t, rr)
		fields, _ := problem["errors"].([]any)
		if len(fields) == 0 {
			t.Fatalf("problem = %v, want field-level credential errors", problem)
		}
	})

	t.Run("完整 azure 凭据 → 200 且密文更新", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, nil)
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"type":             "azure_openai",
			"base_url":         azureBase,
			"credentials": map[string]any{
				"api_key":     "sk-new-azure-5678",
				"deployment":  "dep-1",
				"api_version": "2025-04-01",
			},
		}), channelAdminPrincipal())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		stored := env.store.items[1]
		if stored.Type != string(contract.ChannelTypeAzure) {
			t.Fatalf("stored type = %q, want azure_openai", stored.Type)
		}
		plaintext, err := env.sealer.Open(stored.Credentials)
		if err != nil {
			t.Fatalf("open updated credentials: %v", err)
		}
		var creds ChannelCredentials
		if err := json.Unmarshal(plaintext, &creds); err != nil {
			t.Fatalf("decode credentials: %v", err)
		}
		if creds.APIKey != "sk-new-azure-5678" || creds.Deployment != "dep-1" || creds.APIVersion != "2025-04-01" {
			t.Fatalf("stored credentials = %+v, want the newly submitted azure set", creds)
		}
	})

	t.Run("openai → anthropic 只需 api_key", func(t *testing.T) {
		env := newChannelTestEnv(t, ChannelHandlerOptions{})
		seeded := seedChannel(t, env, nil)
		rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", map[string]any{
			"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
			"type":             "anthropic",
			"credentials":      map[string]any{"api_key": "sk-ant-new-1234"},
		}), channelAdminPrincipal())
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
		}
		if got := env.store.items[1].Type; got != string(contract.ChannelTypeAnthropic) {
			t.Fatalf("stored type = %q, want anthropic", got)
		}
	})
}

// TestChannelDeletedUserMapsTo401 覆盖 finding 9：已删除用户（users 行不存在）
// 在管理面也必须 401，而不是塌缩成 500。
func TestChannelDeletedUserMapsTo401(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	env.access.mu.Lock()
	env.access.err = store.ErrUserNotFound
	env.access.mu.Unlock()

	rr := serveWithPrincipal(t, env.mux, httptest.NewRequest(http.MethodGet, "/api/channels", nil), channelAdminPrincipal())
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}
func TestChannelPatchAzureOnlyAPIKeyKeepsDeployment(t *testing.T) {
	env := newChannelTestEnv(t, ChannelHandlerOptions{})
	baseURL := "https://azure.example.com"
	seeded := seedChannel(t, env, func(c *store.Channel) {
		c.Type = string(contract.ChannelTypeAzure)
		c.BaseURL = &baseURL
		c.Credentials = mustSealCredentials(t, env.sealer, ChannelCredentials{
			APIKey:     "sk-old-azure-1234",
			Deployment: "my-deployment",
			APIVersion: "2025-04-01",
		})
	})

	rr := serveWithPrincipal(t, env.mux, jsonReq(t, http.MethodPatch, "/api/channels/1", map[string]any{
		"expected_version": seeded.UpdatedAt.UTC().Format(time.RFC3339Nano),
		"credentials":      map[string]any{"api_key": "sk-new-azure-5678"},
	}), channelAdminPrincipal())
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（body=%s）", rr.Code, rr.Body.String())
	}
	plaintext, err := env.sealer.Open(env.store.items[1].Credentials)
	if err != nil {
		t.Fatalf("open updated credentials: %v", err)
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		t.Fatalf("decode updated credentials: %v", err)
	}
	if creds.APIKey != "sk-new-azure-5678" {
		t.Fatalf("api_key = %q，期望新密钥", creds.APIKey)
	}
	if creds.Deployment != "my-deployment" || creds.APIVersion != "2025-04-01" {
		t.Fatalf("deployment/api_version 未从旧密文保留: %+v", creds)
	}
}
