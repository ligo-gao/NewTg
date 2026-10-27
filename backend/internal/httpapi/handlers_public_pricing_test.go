package httpapi

// M6c Task 7 公开定价目录的单测：用内存仓储覆盖契约语义（分页/搜索/排序/
// facet/降级/金额字符串/is_free/分组价/ETag/404），SQL 可见集合与版本派生
// 由 pricing_public_repo_integration_test.go 覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/store"
)

// fakePublicPricingRepo 是 PublicPricingRepo 的内存实现。
type fakePublicPricingRepo struct {
	catalog store.PublicPricingCatalog
	counts  store.PublicCounts
	// 错误注入（首页统计需要验证 DB 故障不得返回 200 空对象）。
	catalogErr error
	countsErr  error
	// 渠道可用性接缝默认返回 true：绝大多数公开面用例关心的是目录/指标视图，
	// 而不是渠道状态。需要模拟「失去全部可用渠道」时置 noRoutable；需要模拟
	// 查询故障（handler 必须 fail-open）时置 routableErr。
	noRoutable    bool
	routableErr   error
	routableCalls int
	routableModel string

	catalogCalls atomic.Int32
	countsCalls  atomic.Int32
}

func (f *fakePublicPricingRepo) LoadPublicPricingCatalog(context.Context) (store.PublicPricingCatalog, error) {
	f.catalogCalls.Add(1)
	if f.catalogErr != nil {
		return store.PublicPricingCatalog{}, f.catalogErr
	}
	return f.catalog, nil
}

func (f *fakePublicPricingRepo) LoadPublicCounts(context.Context) (store.PublicCounts, error) {
	f.countsCalls.Add(1)
	if f.countsErr != nil {
		return store.PublicCounts{}, f.countsErr
	}
	return f.counts, nil
}

func (f *fakePublicPricingRepo) HasRoutableChannel(_ context.Context, model string) (bool, error) {
	f.routableCalls++
	f.routableModel = model
	if f.routableErr != nil {
		return false, f.routableErr
	}
	return !f.noRoutable, nil
}

// fakePublicGroups 是软鉴权的用户分组接缝。
type fakePublicGroups map[int64]string

func (f fakePublicGroups) UserGroup(_ context.Context, userID int64) (string, bool, error) {
	group, ok := f[userID]
	return group, ok, nil
}

type publicTestEnv struct {
	t        *testing.T
	router   *Router
	rdb      *redis.Client
	sessions *auth.SessionManager
	repo     *fakePublicPricingRepo
}

func newPublicTestEnv(t *testing.T, cfg mapConfig, repo *fakePublicPricingRepo, groups PublicUserGroupLookup) *publicTestEnv {
	t.Helper()
	if cfg == nil {
		cfg = mapConfig{}
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	rt, err := NewRouter(RouterConfig{
		Redis:                 rdb,
		Sessions:              sessions,
		Repo:                  newFakeAuthRepo(),
		Config:                cfg,
		SessionSecret:         []byte(testSessionSecret),
		PublicPricingRepo:     repo,
		PublicUserGroupLookup: groups,
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return &publicTestEnv{t: t, router: rt, rdb: rdb, sessions: sessions, repo: repo}
}

func (e *publicTestEnv) request(method, path string, headers map[string]string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, http.NoBody)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if mutate != nil {
		mutate(req)
	}
	return serve(e.router, req)
}

func (e *publicTestEnv) sessionCookie(t *testing.T, userID int64) *http.Cookie {
	t.Helper()
	sid, err := e.sessions.Create(context.Background(), userID, auth.SessionOpts{Role: "user"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: sid}
}

func (e *publicTestEnv) decodeList(t *testing.T, path string, headers map[string]string, mutate func(*http.Request)) (int, PublicPricingList, *httptest.ResponseRecorder) {
	t.Helper()
	rr := e.request(http.MethodGet, path, headers, mutate)
	var body PublicPricingList
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("解码列表失败: %v（body=%q）", err, rr.Body.String())
		}
	}
	return rr.Code, body, rr
}

func publicInt64Ptr(v int64) *int64 { return &v }

func publicStringPtr(v string) *string { return &v }

// publicJSON 把测试数据编码成 model_meta 的 jsonb 列形态。
func publicJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// fixtureCatalog 构造覆盖主要分支的目录：
//   - openai/gpt-5      per_token  prompt 500 / completion 1500 / cache 50/150
//   - openai/gpt-free   per_token  全 0（免费）
//   - anthropic/claude  per_token  prompt 0 / completion 100（部分 0，不免费）
//   - vendorless-model  per_token  prompt NULL（NULL 保留、null 展示）
//   - gemini/flash      per_request prompt 200（输出/缓存不展示）
//   - image/dall-e      per_token  prompt 300 / 其余 NULL（非免费）
func fixtureCatalog() store.PublicPricingCatalog {
	return store.PublicPricingCatalog{
		Models: []store.PublicModelPricing{
			{Model: "openai/gpt-5", Mode: 0, PromptPrice: publicInt64Ptr(500), CompletionPrice: publicInt64Ptr(1500), CacheReadPrice: publicInt64Ptr(50), CacheWritePrice: publicInt64Ptr(150)},
			{Model: "openai/gpt-free", Mode: 0, PromptPrice: publicInt64Ptr(0), CompletionPrice: publicInt64Ptr(0), CacheReadPrice: publicInt64Ptr(0), CacheWritePrice: publicInt64Ptr(0)},
			{Model: "anthropic/claude", Mode: 0, PromptPrice: publicInt64Ptr(0), CompletionPrice: publicInt64Ptr(100), CacheReadPrice: publicInt64Ptr(0), CacheWritePrice: publicInt64Ptr(0)},
			{Model: "vendorless-model", Mode: 0, PromptPrice: nil, CompletionPrice: publicInt64Ptr(100), CacheReadPrice: publicInt64Ptr(1), CacheWritePrice: publicInt64Ptr(2)},
			{Model: "gemini/flash", Mode: 1, PromptPrice: publicInt64Ptr(200)},
			{Model: "image/dall-e", Mode: 0, PromptPrice: publicInt64Ptr(300), CompletionPrice: nil, CacheReadPrice: nil, CacheWritePrice: nil},
		},
		Groups: []store.PublicGroup{
			{Name: "default", Ratio: 100},
			{Name: "vip", Ratio: 120},
		},
		Overrides: []store.PublicGroupPricing{
			{Group: "vip", Model: "openai/gpt-5", PromptPrice: 600, CompletionPrice: 1800, CacheReadPrice: 60, CacheWritePrice: 180},
		},
		Version:     "deadbeefdeadbeef",
		GeneratedAt: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	}
}

func TestPublicPricingListMoneyAndVisibility(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)

	code, body, rr := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", code, rr.Body.String())
	}
	if body.Total != "6" || body.Page != 1 || body.PageSize != 100 {
		t.Fatalf("分页信封错误: total=%s page=%d page_size=%d", body.Total, body.Page, body.PageSize)
	}
	byID := map[string]PublicModelCard{}
	for _, item := range body.Items {
		byID[item.ModelID] = item
	}

	// 金额一律字符串（前端不做 Number 解析）；NULL 保持 null（R-PUB-03）。
	gpt := byID["openai/gpt-5"]
	if gpt.PromptPrice == nil || *gpt.PromptPrice != "500" ||
		gpt.CompletionPrice == nil || *gpt.CompletionPrice != "1500" ||
		gpt.CacheReadPrice == nil || *gpt.CacheReadPrice != "50" ||
		gpt.CacheWritePrice == nil || *gpt.CacheWritePrice != "150" {
		t.Fatalf("金额未按字符串下发: %+v", gpt)
	}
	// 本 fixture 无档案：display_name 回退 model_id、vendor 回退前缀（D1-15）；
	// 有档案时改用档案（TestPublicPricingMetadataCards）。
	if gpt.Vendor != "openai" || gpt.DisplayName != "openai/gpt-5" {
		t.Fatalf("无档案时 vendor/display_name 降级错误: %+v", gpt)
	}
	nullPrice := byID["vendorless-model"]
	if nullPrice.PromptPrice != nil {
		t.Fatalf("NULL 输入价必须下发 null，得到 %v", *nullPrice.PromptPrice)
	}

	// is_free：四项显式 0 才是免费；部分 0 / NULL 都不是。
	if !byID["openai/gpt-free"].IsFree {
		t.Fatal("全 0 模型应标记免费")
	}
	if byID["anthropic/claude"].IsFree {
		t.Fatal("部分价格为 0 不应标记免费")
	}
	if byID["image/dall-e"].IsFree {
		t.Fatal("completion/cache 为 NULL 不应标记免费（NULL 不算 0）")
	}
	// per_request：只展示 prompt，输出/缓存隐藏为 null。
	request := byID["gemini/flash"]
	if request.Mode != "per_request" || request.CompletionPrice != nil || request.CacheReadPrice != nil {
		t.Fatalf("per_request 卡片错误: %+v", request)
	}

	// 不泄露内部字段：响应的键集合只包含契约字段。
	raw, _ := json.Marshal(gpt)
	for _, forbidden := range []string{"billing_expr", "\"id\"", "author", "status", "channel"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("卡片泄露内部字段 %q: %s", forbidden, raw)
		}
	}
}

func TestPublicPricingSearchSortPagination(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)

	// 搜索：大小写不敏感的子串（model_id）。
	_, body, _ := env.decodeList(t, "/api/pricing?search=GPT&page_size=100", nil, nil)
	if body.Total != "2" {
		t.Fatalf("search=GPT total=%s，期望 2", body.Total)
	}

	// price_asc：per_token 组在前，NULL 输入价排组末，组内按输入价升序。
	_, asc, _ := env.decodeList(t, "/api/pricing?sort=price_asc&page_size=100", nil, nil)
	wantAsc := []string{
		"anthropic/claude", "openai/gpt-free", "image/dall-e", "openai/gpt-5",
		"vendorless-model", "gemini/flash",
	}
	if got := modelIDs(asc.Items); !equalStrings(got, wantAsc) {
		t.Fatalf("price_asc 顺序 = %v，期望 %v", got, wantAsc)
	}
	// price_desc：per_token 仍在前（两组不混排），组内降序，NULL 仍最后。
	_, desc, _ := env.decodeList(t, "/api/pricing?sort=price_desc&page_size=100", nil, nil)
	wantDesc := []string{
		"openai/gpt-5", "image/dall-e", "anthropic/claude", "openai/gpt-free",
		"vendorless-model", "gemini/flash",
	}
	if got := modelIDs(desc.Items); !equalStrings(got, wantDesc) {
		t.Fatalf("price_desc 顺序 = %v，期望 %v", got, wantDesc)
	}

	// 非法 sort 回退 name（R-PUB-04）。
	_, fallback, _ := env.decodeList(t, "/api/pricing?sort=bogus&page_size=100", nil, nil)
	names := modelIDs(fallback.Items)
	for i := 1; i < len(names); i++ {
		if strings.ToLower(names[i-1]) > strings.ToLower(names[i]) {
			t.Fatalf("非法 sort 未回退 name：%v", names)
		}
	}

	// 分页：稳定、不重不漏；越界回退最后一页。
	_, page1, _ := env.decodeList(t, "/api/pricing?page_size=2&p=1", nil, nil)
	_, page2, _ := env.decodeList(t, "/api/pricing?page_size=2&p=2", nil, nil)
	_, page9, _ := env.decodeList(t, "/api/pricing?page_size=2&p=9", nil, nil)
	if len(page1.Items) != 2 || len(page2.Items) != 2 || page1.Items[0].ModelID == page2.Items[0].ModelID {
		t.Fatalf("分页不稳定: p1=%v p2=%v", modelIDs(page1.Items), modelIDs(page2.Items))
	}
	if page9.Page != 3 || len(page9.Items) != 2 {
		t.Fatalf("越界页应回退最后一页（page=%d len=%d）", page9.Page, len(page9.Items))
	}

	// 非法 p/page_size 回退默认（首屏 + 24）。
	_, invalid, _ := env.decodeList(t, "/api/pricing?p=abc&page_size=-1", nil, nil)
	if invalid.Page != 1 || invalid.PageSize != 24 {
		t.Fatalf("非法分页参数未降级: page=%d size=%d", invalid.Page, invalid.PageSize)
	}
	// page_size 上限 100（clamp，不是丢弃）。
	_, clamped, _ := env.decodeList(t, "/api/pricing?page_size=1000", nil, nil)
	if clamped.PageSize != 100 {
		t.Fatalf("page_size 上限未生效: %d", clamped.PageSize)
	}
}

func TestPublicPricingFacets(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)

	// 无筛选：vendor 计数（无前缀模型不生成空 vendor）。
	_, body, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	vendors := map[string]int64{}
	for _, option := range body.Facets.Vendors {
		vendors[option.Value] = option.Count
	}
	if vendors["openai"] != 2 || vendors["anthropic"] != 1 || vendors["gemini"] != 1 || vendors["image"] != 1 ||
		len(vendors) != 4 {
		t.Fatalf("vendor facet 计数错误: %+v", body.Facets.Vendors)
	}
	if body.Facets.Capabilities == nil || len(body.Facets.Capabilities) != 0 ||
		len(body.Facets.Context) != 0 || len(body.Facets.Endpoints) != 0 {
		t.Fatalf("无档案时 capability/context/endpoint facet 必须为空数组（不是 null）: %+v", body.Facets)
	}
	// pricing_types 计数。
	types := map[string]int64{}
	for _, option := range body.Facets.PricingTypes {
		types[option.Value] = option.Count
	}
	if types["token"] != 5 || types["request"] != 1 {
		t.Fatalf("pricing_types 计数错误: %+v", body.Facets.PricingTypes)
	}
	// 匿名 + expose=false：分组倍率不下发（R-PUB-05）。
	for _, group := range body.Facets.Groups {
		if group.Ratio != nil {
			t.Fatalf("匿名且 expose=false 时不应下发分组倍率: %+v", group)
		}
	}

	// vendor 筛选：本组自身不参与计数（选中 openai 后其他 vendor 计数仍可见）。
	_, filtered, _ := env.decodeList(t, "/api/pricing?vendor=openai,anthropic&page_size=100", nil, nil)
	if filtered.Total != "3" {
		t.Fatalf("vendor 多选 total=%s，期望 3", filtered.Total)
	}
	counts := map[string]int64{}
	for _, option := range filtered.Facets.Vendors {
		counts[option.Value] = option.Count
	}
	if counts["openai"] != 2 || counts["anthropic"] != 1 || counts["gemini"] != 1 {
		t.Fatalf("vendor facet 未忽略自身筛选: %+v", filtered.Facets.Vendors)
	}

	// 不存在的 vendor 值被丢弃，其余条件照常生效（F-09-22）。
	_, dropped, _ := env.decodeList(t, "/api/pricing?vendor=nope&page_size=100", nil, nil)
	if dropped.Total != "6" {
		t.Fatalf("不存在的 vendor 应被丢弃，total=%s", dropped.Total)
	}

	// quotaType=token：pricing_types facet 忽略自身筛选（仍能看到 request 计数）。
	_, tokenOnly, _ := env.decodeList(t, "/api/pricing?quotaType=token&page_size=100", nil, nil)
	if tokenOnly.Total != "5" {
		t.Fatalf("quotaType=token total=%s，期望 5", tokenOnly.Total)
	}
	types = map[string]int64{}
	for _, option := range tokenOnly.Facets.PricingTypes {
		types[option.Value] = option.Count
	}
	if types["request"] != 1 {
		t.Fatalf("pricing_types facet 未忽略自身筛选: %+v", tokenOnly.Facets.PricingTypes)
	}
	// 非法 quotaType 回退 all。
	_, all, _ := env.decodeList(t, "/api/pricing?quotaType=bogus&page_size=100", nil, nil)
	if all.Total != "6" {
		t.Fatalf("非法 quotaType 应回退 all，total=%s", all.Total)
	}

	// 空集仍是 200 + items:[]（F-09-22 强制）。
	code, empty, _ := env.decodeList(t, "/api/pricing?search=zzz-no-match", nil, nil)
	if code != http.StatusOK || empty.Total != "0" || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("空集语义错误: code=%d total=%s items=%v", code, empty.Total, empty.Items)
	}
}

func TestPublicPricingGroupPriceVisibility(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}

	// 匿名 + expose=false：不返回 group_price。
	env := newPublicTestEnv(t, nil, repo, fakePublicGroups{42: "vip"})
	_, anonymous, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	for _, item := range anonymous.Items {
		if item.GroupPrice != nil {
			t.Fatalf("匿名且 expose=false 不应下发 group_price: %+v", item)
		}
	}

	// 显式 group 同样受 expose=false 限制（R-PUB-05 覆盖 F-09-15 的匿名倍率）。
	_, explicit, _ := env.decodeList(t, "/api/pricing?group=vip&page_size=100", nil, nil)
	for _, item := range explicit.Items {
		if item.GroupPrice != nil {
			t.Fatalf("匿名显式 group 在 expose=false 时不应下发 group_price: %+v", item)
		}
	}

	// expose=true：匿名显式 group 可看预览价（覆盖价优先 / 倍率整数运算）。
	exposed := newPublicTestEnv(t, mapConfig{
		"billing.expose_group_ratios": configBool(true),
	}, repo, fakePublicGroups{42: "vip"})
	_, preview, _ := exposed.decodeList(t, "/api/pricing?group=vip&page_size=100", nil, nil)
	byID := cardsByID(preview.Items)
	// vip 对 openai/gpt-5 有覆盖价：source=override，值取覆盖价。
	override := byID["openai/gpt-5"].GroupPrice
	if override == nil || override.Source != "override" || override.Prompt == nil || *override.Prompt != "600" ||
		override.Ratio == nil || *override.Ratio != 120 {
		t.Fatalf("覆盖价口径错误: %+v", override)
	}
	// 无覆盖价：基准 × 120 ÷ 100（服务端整数运算；300×120/100=360）。
	ratio := byID["image/dall-e"].GroupPrice
	if ratio == nil || ratio.Source != "ratio" || ratio.Prompt == nil || *ratio.Prompt != "360" ||
		ratio.Completion != nil || ratio.CacheRead != nil {
		t.Fatalf("倍率价口径错误: %+v", ratio)
	}
	// NULL 基准价保持 null（不伪造 0）。
	if byID["vendorless-model"].GroupPrice.Prompt != nil {
		t.Fatalf("NULL 基准价的分组价应为 null: %+v", byID["vendorless-model"].GroupPrice)
	}
	// expose=true 时 facet 倍率下发。
	ratioFacet := groupFacetByValue(preview.Facets.Groups, "vip")
	if ratioFacet == nil || ratioFacet.Ratio == nil || *ratioFacet.Ratio != 120 {
		t.Fatalf("expose=true 时分组 facet 应下发倍率: %+v", preview.Facets.Groups)
	}

	// 登录用户：即使 expose=false 也返回自己分组的 group_price。
	cookie := env.sessionCookie(t, 42)
	_, logged, _ := env.decodeList(t, "/api/pricing?page_size=100", map[string]string{
		"Cookie": cookie.Name + "=" + cookie.Value,
	}, nil)
	loggedVIP := cardsByID(logged.Items)["openai/gpt-5"].GroupPrice
	if loggedVIP == nil || loggedVIP.Group != "vip" || loggedVIP.Prompt == nil || *loggedVIP.Prompt != "600" {
		t.Fatalf("登录用户应看到自己分组的实付价: %+v", loggedVIP)
	}
	// 登录用户的分组 facet 带倍率。
	if ratioFacet := groupFacetByValue(logged.Facets.Groups, "vip"); ratioFacet == nil || ratioFacet.Ratio == nil {
		t.Fatalf("登录用户的 group facet 应含倍率: %+v", logged.Facets.Groups)
	}
	// 个性化响应必须 private（不能进共享缓存）。
	rr := env.request(http.MethodGet, "/api/pricing?page_size=10", nil, func(r *http.Request) {
		r.AddCookie(cookie)
	})
	if cc := rr.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "private") {
		t.Fatalf("登录用户的定价响应应 private，得到 %q", cc)
	}
	if vary := rr.Header().Get("Vary"); !strings.Contains(vary, "Cookie") {
		t.Fatalf("个性化响应 Vary 应含 Cookie，得到 %q", vary)
	}

	// include_group_price=0：显式关闭个性化价格。
	_, suppressed, _ := env.decodeList(t, "/api/pricing?page_size=100&include_group_price=0",
		map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}, nil)
	for _, item := range suppressed.Items {
		if item.GroupPrice != nil {
			t.Fatalf("include_group_price=0 仍下发 group_price: %+v", item)
		}
	}
}

func TestPublicPricingETagAndCache(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)

	rr := env.request(http.MethodGet, "/api/pricing", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	etag := rr.Header().Get("ETag")
	if etag != `"pricing-vdeadbeefdeadbeef"` {
		t.Fatalf("ETag = %q", etag)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=30, stale-while-revalidate=30" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if vary := rr.Header().Get("Vary"); !strings.Contains(vary, "Accept-Encoding") {
		t.Fatalf("Vary = %q", vary)
	}

	// If-None-Match 命中 → 304 无 body（弱比较前缀也接受）。
	conditional := env.request(http.MethodGet, "/api/pricing", map[string]string{"If-None-Match": "W/" + etag}, nil)
	if conditional.Code != http.StatusNotModified {
		t.Fatalf("条件请求 status = %d，期望 304", conditional.Code)
	}
	if conditional.Body.Len() != 0 {
		t.Fatalf("304 必须无 body，得到 %q", conditional.Body.String())
	}
	if conditional.Header().Get("ETag") != etag {
		t.Fatal("304 响应必须回带 ETag")
	}

	// 不匹配 → 200 全量。
	miss := env.request(http.MethodGet, "/api/pricing", map[string]string{"If-None-Match": `"pricing-vother"`}, nil)
	if miss.Code != http.StatusOK || miss.Body.Len() == 0 {
		t.Fatalf("ETag 不匹配应返回 200 全量: %d", miss.Code)
	}
}

func TestPublicPricingDetail(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)

	// URL 编码的 `/` 必须能匹配 {model_id} 并解码为原始模型名。
	encoded := url.PathEscape("openai/gpt-5")
	rr := env.request(http.MethodGet, "/api/pricing/"+encoded, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("详情 status = %d（body=%s）", rr.Code, rr.Body.String())
	}
	var detail PublicModelDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("解码详情失败: %v", err)
	}
	if detail.ModelID != "openai/gpt-5" || detail.AuthStyle != "bearer" {
		t.Fatalf("详情基础字段错误: %+v", detail)
	}
	if detail.Modalities == nil || len(detail.Modalities) != 0 ||
		detail.Parameters == nil || detail.GroupPrices == nil || detail.RateLimits == nil {
		t.Fatalf("详情缺失元数据必须以空数组表达: %+v", detail)
	}
	if detail.GroupPrices == nil || len(detail.GroupPrices) != 0 {
		t.Fatalf("匿名且 expose=false 时 group_prices 应为空: %+v", detail.GroupPrices)
	}
	etag := rr.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"pricing-vdeadbeefdeadbeef-`) {
		t.Fatalf("详情 ETag = %q", etag)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=30") {
		t.Fatalf("详情 Cache-Control = %q", cc)
	}

	// 详情 304。
	conditional := env.request(http.MethodGet, "/api/pricing/"+encoded, map[string]string{"If-None-Match": etag}, nil)
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("详情条件请求 = %d body=%q", conditional.Code, conditional.Body.String())
	}

	// 404：不存在（停用/表达式/无定价在 SQL 层已被排除，同一条分支）。
	missing := env.request(http.MethodGet, "/api/pricing/"+url.PathEscape("nope/missing"), nil, nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在模型 status = %d", missing.Code)
	}
	var problem map[string]any
	if err := json.Unmarshal(missing.Body.Bytes(), &problem); err != nil {
		t.Fatalf("解码 problem 失败: %v", err)
	}
	if problem["type"] != problemTypeModelNotFound {
		t.Fatalf("problem.type = %v", problem["type"])
	}

	// P1 metrics：已实现真实聚合；测试装配没有日志库，summary 降级为零样本，
	// 但端点仍是 200 —— 公开性能页的可用性不能挂在一个可选数据源上。
	metrics := env.request(http.MethodGet, "/api/pricing/"+encoded+"/metrics", nil, nil)
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200 (degraded), body=%s", metrics.Code, metrics.Body.String())
	}
	var metricsBody PublicModelMetricsResponse
	if err := json.Unmarshal(metrics.Body.Bytes(), &metricsBody); err != nil {
		t.Fatalf("metrics 解码失败: %v", err)
	}
	if metricsBody.Summary.SampleSize != 0 {
		t.Fatalf("无日志库时 summary sample_size = %d, want 0", metricsBody.Summary.SampleSize)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func modelIDs(items []PublicModelCard) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ModelID
	}
	return out
}

func cardsByID(items []PublicModelCard) map[string]PublicModelCard {
	out := make(map[string]PublicModelCard, len(items))
	for _, item := range items {
		out[item.ModelID] = item
	}
	return out
}

func groupFacetByValue(items []PublicGroupFacet, value string) *PublicGroupFacet {
	for i := range items {
		if items[i].Value == value {
			return &items[i]
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPublicPricingRepoErrorIs500(t *testing.T) {
	repo := &fakePublicPricingRepo{catalogErr: errors.New("db down")}
	env := newPublicTestEnv(t, nil, repo, nil)
	rr := env.request(http.MethodGet, "/api/pricing", nil, nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("DB 故障应 500，得到 %d", rr.Code)
	}
}

// 未匹配的定价子路径必须由公开兜底返回 404 model-not-found，而不是落进
// 受保护 /api/ 子树得到 401（复核 LOW-1）。
func TestPublicPricingUnmatchedSubpathsAre404(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)

	for _, path := range []string{
		"/api/pricing/openai/gpt-5", // 未编码的多段模型名
		"/api/pricing/",             // 尾斜杠（空 rest）
		"/api/pricing/nonexistent",  // 单段但不存在
	} {
		rr := env.request(http.MethodGet, path, nil, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d（期望 404，body=%s）", path, rr.Code, rr.Body.String())
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("%s Content-Type = %q", path, ct)
		}
		var problem map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s 解码 problem 失败: %v", path, err)
		}
		if problem["type"] != problemTypeModelNotFound {
			t.Fatalf("%s problem.type = %v，期望 %s", path, problem["type"], problemTypeModelNotFound)
		}
	}
}

// If-None-Match: * 按 RFC 9110 匹配任意当前表示。
func TestPublicPricingETagStar(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalog()}
	env := newPublicTestEnv(t, nil, repo, nil)
	rr := env.request(http.MethodGet, "/api/pricing", map[string]string{"If-None-Match": "*"}, nil)
	if rr.Code != http.StatusNotModified || rr.Body.Len() != 0 {
		t.Fatalf("If-None-Match:* = %d body=%q", rr.Code, rr.Body.String())
	}
}

// ---------------------------------------------------------------------------
// D1c：公开定价元数据集成（D1-13…D1-23）
// ---------------------------------------------------------------------------

// fixtureCatalogWithMeta 在基础目录上挂载档案与供应商，覆盖：
//   - openai/gpt-5      精确档案 + vendors.name（一级回退）；
//   - anthropic/claude  精确档案 + 完整详情字段；
//   - gemini/flash      前缀档案（name_rule=1），供应商回退到 model_id 前缀；
//   - vendorless-model  无档案，供应商回退到 model_pricing.vendor；
//   - openai/gpt-free   无档案，display_name 回退 model_id；
//   - image/dall-e      精确命中**停用**档案（status=2）⇒ 整条剔除。
func fixtureCatalogWithMeta() store.PublicPricingCatalog {
	catalog := fixtureCatalog()
	for i := range catalog.Models {
		if catalog.Models[i].Model == "vendorless-model" {
			catalog.Models[i].Vendor = publicStringPtr("Acme")
		}
	}
	released := time.Date(2025, 8, 7, 0, 0, 0, 0, time.UTC)
	catalog.Vendors = []store.PublicVendor{
		{ID: 1, Name: "OpenAI", Status: 1},
		{ID: 2, Name: "Anthropic", Status: 1},
		{ID: 3, Name: "Retired Vendor", Status: 2},
	}
	catalog.Meta = []store.ModelMeta{
		{
			ModelName: "openai/gpt-5", DisplayName: publicStringPtr("GPT-5"),
			Description: publicStringPtr("Flagship model."),
			Tags:        publicJSON([]string{"long-context"}),
			VendorID:    publicInt64Ptr(1),
			Endpoints: publicJSON(map[string][]string{
				"/v1/chat/completions": {"POST"},
				"/v1/messages":         {"POST"},
				"/private/unknown":     {"POST"},
			}),
			Capabilities:  publicJSON([]string{"text", "vision"}),
			ContextLength: publicInt64Ptr(128000),
			MaxOutput:     publicInt64Ptr(32000),
			Status:        1, NameRule: 0,
		},
		{
			ModelName: "anthropic/claude", DisplayName: publicStringPtr("Claude"),
			VendorID:     publicInt64Ptr(2),
			Capabilities: publicJSON([]string{"text", "reasoning"}),
			Modalities:   publicJSON([]string{"text", "image"}),
			Parameters: publicJSON([]map[string]any{{
				"name": "temperature", "required": false, "type": "number",
				"default": 1, "description": "Sampling temperature.",
			}}),
			ContextLength:   publicInt64Ptr(200000),
			KnowledgeCutoff: publicStringPtr("2025-03"),
			ReleasedAt:      &released,
			Status:          1, NameRule: 0,
		},
		{
			ModelName: "gemini/", DisplayName: publicStringPtr("Gemini Flash"),
			Capabilities:  publicJSON([]string{"text"}),
			Endpoints:     publicJSON(map[string][]string{"/v1beta/models/{model}:generateContent": {"POST"}}),
			ContextLength: publicInt64Ptr(32768),
			Status:        1, NameRule: 1,
		},
		{
			// 停用档案：与 image/dall-e 精确命中 ⇒ 公开面剔除（D1-13）。
			ModelName: "image/dall-e", DisplayName: publicStringPtr("Hidden"),
			Status: 2, NameRule: 0,
		},
	}
	return catalog
}

func TestPublicPricingMetadataCards(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalogWithMeta()}
	env := newPublicTestEnv(t, nil, repo, nil)

	code, body, rr := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d（body=%s）", code, rr.Body.String())
	}
	if body.Total != "5" {
		t.Fatalf("停用档案命中的模型必须剔除，total=%s", body.Total)
	}
	cards := cardsByID(body.Items)
	if _, ok := cards["image/dall-e"]; ok {
		t.Fatal("status=2 档案命中的模型不得出现在 items")
	}

	// 有档案：卡片字段来自档案。
	gpt := cards["openai/gpt-5"]
	if gpt.DisplayName != "GPT-5" || gpt.Vendor != "OpenAI" {
		t.Fatalf("档案 display_name/vendor 未生效: %+v", gpt)
	}
	if !equalStrings(gpt.Capabilities, []string{"text", "vision"}) {
		t.Fatalf("capabilities = %v", gpt.Capabilities)
	}
	if gpt.ContextLength == nil || *gpt.ContextLength != 128000 ||
		gpt.MaxOutput == nil || *gpt.MaxOutput != 32000 {
		t.Fatalf("context_length/max_output 未生效: %+v", gpt)
	}
	if !equalStrings(gpt.Endpoints, []string{"chat.completions", "messages"}) {
		t.Fatalf("endpoints 派生错误（未知路径必须忽略）: %v", gpt.Endpoints)
	}
	if !equalStrings(gpt.Tags, []string{"long-context"}) || gpt.Description != "Flagship model." {
		t.Fatalf("tags/description 未生效: %+v", gpt)
	}
	// 价格仍只来自 model_pricing，不得从元数据推导。
	if gpt.PromptPrice == nil || *gpt.PromptPrice != "500" || gpt.IsFree {
		t.Fatalf("价格字段被元数据污染: %+v", gpt)
	}

	// 无档案：D1-15 降级。
	free := cards["openai/gpt-free"]
	if free.DisplayName != "openai/gpt-free" || free.Vendor != "openai" ||
		free.Capabilities == nil || len(free.Capabilities) != 0 ||
		free.Endpoints == nil || len(free.Endpoints) != 0 ||
		free.Tags == nil || len(free.Tags) != 0 || free.Description != "" ||
		free.ContextLength != nil || free.MaxOutput != nil {
		t.Fatalf("无档案卡片必须完全降级: %+v", free)
	}

	// vendor 三级回退：vendors.name → model_pricing.vendor → model_id 前缀。
	if cards["vendorless-model"].Vendor != "Acme" {
		t.Fatalf("第二级回退（model_pricing.vendor）失败: %+v", cards["vendorless-model"])
	}
	if cards["gemini/flash"].Vendor != "gemini" {
		t.Fatalf("第三级回退（model_id 前缀）失败: %+v", cards["gemini/flash"])
	}
}

func TestPublicPricingMetadataDetail(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalogWithMeta()}
	env := newPublicTestEnv(t, nil, repo, nil)

	decode := func(model string) PublicModelDetail {
		t.Helper()
		rr := env.request(http.MethodGet, "/api/pricing/"+url.PathEscape(model), nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s 详情 status = %d（body=%s）", model, rr.Code, rr.Body.String())
		}
		var detail PublicModelDetail
		if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
			t.Fatalf("解码详情失败: %v", err)
		}
		return detail
	}

	claude := decode("anthropic/claude")
	if !equalStrings(claude.Modalities, []string{"text", "image"}) {
		t.Fatalf("modalities = %v", claude.Modalities)
	}
	if claude.KnowledgeCutoff == nil || *claude.KnowledgeCutoff != "2025-03" {
		t.Fatalf("knowledge_cutoff = %v", claude.KnowledgeCutoff)
	}
	if claude.ReleasedAt == nil || *claude.ReleasedAt != "2025-08-07" {
		t.Fatalf("released_at = %v", claude.ReleasedAt)
	}
	if len(claude.Parameters) != 1 || claude.Parameters[0].Name != "temperature" ||
		claude.Parameters[0].Type != "number" || claude.Parameters[0].Required ||
		claude.Parameters[0].Description != "Sampling temperature." {
		t.Fatalf("parameters = %+v", claude.Parameters)
	}

	// 无档案：详情追加字段按 D1-15 降级为空数组/null。
	free := decode("openai/gpt-free")
	if free.Modalities == nil || len(free.Modalities) != 0 ||
		free.Parameters == nil || len(free.Parameters) != 0 ||
		free.KnowledgeCutoff != nil || free.ReleasedAt != nil {
		t.Fatalf("无档案详情必须降级: %+v", free)
	}

	// 停用档案命中的模型详情 404（与不存在同一分支）。
	hidden := env.request(http.MethodGet, "/api/pricing/"+url.PathEscape("image/dall-e"), nil, nil)
	if hidden.Code != http.StatusNotFound {
		t.Fatalf("停用档案模型详情 status = %d，期望 404", hidden.Code)
	}
	var problem map[string]any
	if err := json.Unmarshal(hidden.Body.Bytes(), &problem); err != nil || problem["type"] != problemTypeModelNotFound {
		t.Fatalf("详情 404 problem = %v", problem)
	}
}

func TestPublicPricingMetadataFacets(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalogWithMeta()}
	env := newPublicTestEnv(t, nil, repo, nil)

	facetCounts := func(options []PublicFacetOption) map[string]int64 {
		out := make(map[string]int64, len(options))
		for _, option := range options {
			out[option.Value] = option.Count
		}
		return out
	}

	_, body, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	vendors := facetCounts(body.Facets.Vendors)
	if vendors["openai"] != 2 || vendors["anthropic"] != 1 || vendors["acme"] != 1 || vendors["gemini"] != 1 {
		t.Fatalf("vendor facet 计数错误（vendors.name 优先）: %+v", body.Facets.Vendors)
	}
	caps := facetCounts(body.Facets.Capabilities)
	if caps["text"] != 3 || caps["vision"] != 1 || caps["reasoning"] != 1 {
		t.Fatalf("capability facet 计数错误: %+v", body.Facets.Capabilities)
	}
	contexts := facetCounts(body.Facets.Context)
	if contexts["32k"] != 1 || contexts["128k"] != 1 || contexts["200k+"] != 1 || len(contexts) != 3 {
		t.Fatalf("context facet 计数错误: %+v", body.Facets.Context)
	}
	endpoints := facetCounts(body.Facets.Endpoints)
	if endpoints["chat.completions"] != 1 || endpoints["messages"] != 1 ||
		endpoints["generatecontent"] != 1 || len(endpoints) != 3 {
		t.Fatalf("endpoint facet 计数错误: %+v", body.Facets.Endpoints)
	}

	// 跨组 AND：capability=vision 且 context=128k → 只剩 openai/gpt-5。
	_, and, _ := env.decodeList(t, "/api/pricing?capability=vision&context=128k&page_size=100", nil, nil)
	if and.Total != "1" || and.Items[0].ModelID != "openai/gpt-5" {
		t.Fatalf("跨组 AND 失败: total=%s items=%v", and.Total, modelIDs(and.Items))
	}

	// 组内 OR：capability=vision,reasoning → 两个模型。
	_, or, _ := env.decodeList(t, "/api/pricing?capability=vision,reasoning&page_size=100", nil, nil)
	if or.Total != "2" {
		t.Fatalf("组内 OR 失败: total=%s", or.Total)
	}

	// facet 计数忽略自身筛选：capability=vision 时 capability 组仍按全集计数。
	_, self, _ := env.decodeList(t, "/api/pricing?capability=vision&page_size=100", nil, nil)
	if got := facetCounts(self.Facets.Capabilities); got["text"] != 3 || got["vision"] != 1 {
		t.Fatalf("capability facet 未忽略自身筛选: %+v", self.Facets.Capabilities)
	}
	// 其它组照常受筛选影响：context facet 只剩 vision 模型的 128K。
	if got := facetCounts(self.Facets.Context); got["128k"] != 1 || len(got) != 1 {
		t.Fatalf("context facet 未应用其它组筛选: %+v", self.Facets.Context)
	}

	// 未知值丢弃：bogus 不产生筛选。
	_, dropped, _ := env.decodeList(t, "/api/pricing?capability=bogus&context=999k&endpointType=nope&page_size=100", nil, nil)
	if dropped.Total != "5" {
		t.Fatalf("未知 facet 值应被丢弃，total=%s", dropped.Total)
	}

	// 端点筛选值大小写归一（generateContent → generatecontent）。
	_, endpoint, _ := env.decodeList(t, "/api/pricing?endpointType=generateContent&page_size=100", nil, nil)
	if endpoint.Total != "1" || endpoint.Items[0].ModelID != "gemini/flash" {
		t.Fatalf("endpointType 筛选失败: total=%s", endpoint.Total)
	}
}

func TestPublicContextBucketBoundaries(t *testing.T) {
	cases := []struct {
		length *int64
		want   string
	}{
		{nil, ""},
		{publicInt64Ptr(0), "8K"},
		{publicInt64Ptr(8_192), "8K"},
		{publicInt64Ptr(8_193), "32K"},
		{publicInt64Ptr(32_768), "32K"},
		{publicInt64Ptr(32_769), "128K"},
		{publicInt64Ptr(131_072), "128K"},
		{publicInt64Ptr(131_073), "200K+"},
	}
	for _, tc := range cases {
		if got := publicContextBucket(tc.length); got != tc.want {
			t.Fatalf("context bucket(%v) = %q，期望 %q", tc.length, got, tc.want)
		}
	}
}

func TestPublicPricingNameRuleResolution(t *testing.T) {
	models := []store.PublicModelPricing{}
	add := func(name string) {
		models = append(models, store.PublicModelPricing{
			Model: name, Mode: 0, PromptPrice: publicInt64Ptr(100),
		})
	}
	for _, name := range []string{
		"rule/exact", "pre/exact2", "org/prefix-target", "zz-suffix",
		"xx-mid-yy", "aaabbb", "xx/pre-target-suf",
	} {
		add(name)
	}
	meta := []store.ModelMeta{
		// exact > prefix：两个规则都命中时取精确。
		{ModelName: "rule/exact", DisplayName: publicStringPtr("exact"), Status: 1, NameRule: 0},
		{ModelName: "rule/", DisplayName: publicStringPtr("prefix"), Status: 1, NameRule: 1},
		// exact > prefix（再来一组，确保不是顺序偶然）。
		{ModelName: "pre/exact2", DisplayName: publicStringPtr("exact2"), Status: 1, NameRule: 0},
		{ModelName: "org/", DisplayName: publicStringPtr("org-prefix"), Status: 1, NameRule: 1},
		// prefix > suffix。
		{ModelName: "xx/", DisplayName: publicStringPtr("xx-prefix"), Status: 1, NameRule: 1},
		{ModelName: "pre-target-suf", DisplayName: publicStringPtr("suffix"), Status: 1, NameRule: 3},
		// suffix > contains。
		{ModelName: "-suffix", DisplayName: publicStringPtr("suffix-win"), Status: 1, NameRule: 3},
		{ModelName: "zz", DisplayName: publicStringPtr("contains"), Status: 1, NameRule: 2},
		// 只有 contains 命中。
		{ModelName: "mid", DisplayName: publicStringPtr("contains-win"), Status: 1, NameRule: 2},
		// 同规则 tie-break：更长的前缀优先。
		{ModelName: "aa", DisplayName: publicStringPtr("short-prefix"), Status: 1, NameRule: 1},
		{ModelName: "aaa", DisplayName: publicStringPtr("long-prefix"), Status: 1, NameRule: 1},
	}
	repo := &fakePublicPricingRepo{catalog: store.PublicPricingCatalog{
		Models: models, Meta: meta, Version: "v1", GeneratedAt: time.Now().UTC(),
	}}
	env := newPublicTestEnv(t, nil, repo, nil)
	_, body, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	names := make(map[string]string, len(body.Items))
	for _, item := range body.Items {
		names[item.ModelID] = item.DisplayName
	}
	want := map[string]string{
		"rule/exact":        "exact",
		"pre/exact2":        "exact2",
		"org/prefix-target": "org-prefix",
		"zz-suffix":         "suffix-win",
		"xx-mid-yy":         "contains-win",
		"aaabbb":            "long-prefix",
		"xx/pre-target-suf": "xx-prefix",
	}
	for model, display := range want {
		if names[model] != display {
			t.Fatalf("%s display_name = %q，期望 %q（全表 %v）", model, names[model], display, names)
		}
	}
}

// 冻结枚举预置：context 的四个分档与 endpoint 的八个枚举是完整合法取值域。
// 目录当前没有命中该值的模型时，筛选必须返回 0 条，而不是被当成「不存在的
// 值」丢弃后返回全集。
func TestPublicPricingFrozenEnumerationFilters(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalogWithMeta()}
	env := newPublicTestEnv(t, nil, repo, nil)

	// 目录里没有 8K 模型（fixture 只有 32K/128K/200K+）。
	_, eightK, _ := env.decodeList(t, "/api/pricing?context=8k&page_size=100", nil, nil)
	if eightK.Total != "0" || len(eightK.Items) != 0 {
		t.Fatalf("合法但无命中的 context=8k 应返回 0 条，得到 total=%s items=%v",
			eightK.Total, modelIDs(eightK.Items))
	}

	// 目录里没有 embeddings 模型。
	_, embeddings, _ := env.decodeList(t, "/api/pricing?endpointType=embeddings&page_size=100", nil, nil)
	if embeddings.Total != "0" || len(embeddings.Items) != 0 {
		t.Fatalf("合法但无命中的 endpointType=embeddings 应返回 0 条，得到 total=%s items=%v",
			embeddings.Total, modelIDs(embeddings.Items))
	}

	// 真正不在冻结枚举里的值仍然丢弃（不产生筛选）。
	_, bogus, _ := env.decodeList(t, "/api/pricing?context=999k&endpointType=telepathy&page_size=100", nil, nil)
	if bogus.Total != "5" {
		t.Fatalf("未知枚举值应被丢弃，total=%s", bogus.Total)
	}
}

// 档案 capabilities 的大小写折叠去重：facet 计数与筛选 total 必须同口径，
// 输出保留首个原值。
func TestPublicPricingCapabilityDedup(t *testing.T) {
	catalog := store.PublicPricingCatalog{
		Models: []store.PublicModelPricing{
			{Model: "dup/repeat", Mode: 0, PromptPrice: publicInt64Ptr(100)},
			{Model: "dup/mixed", Mode: 0, PromptPrice: publicInt64Ptr(200)},
		},
		Meta: []store.ModelMeta{
			{
				ModelName: "dup/repeat", Status: 1, NameRule: 0,
				Capabilities: publicJSON([]string{"text", "text"}),
			},
			{
				ModelName: "dup/mixed", Status: 1, NameRule: 0,
				Capabilities: publicJSON([]string{"Text", "text"}),
			},
		},
		Version: "v1", GeneratedAt: time.Now().UTC(),
	}
	repo := &fakePublicPricingRepo{catalog: catalog}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, body, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	var facetCount int64
	for _, option := range body.Facets.Capabilities {
		if option.Value == "text" {
			facetCount = option.Count
			if option.Label != "text" {
				t.Fatalf("capability facet label 应保留首个原值 text，得到 %q", option.Label)
			}
		}
	}
	if facetCount != 2 {
		t.Fatalf("重复能力只能按模型计一次，facet text 计数 = %d，期望 2（%+v）",
			facetCount, body.Facets.Capabilities)
	}

	_, filtered, _ := env.decodeList(t, "/api/pricing?capability=text&page_size=100", nil, nil)
	if filtered.Total != "2" || int64(len(filtered.Items)) != facetCount {
		t.Fatalf("capability=text total=%s 与 facet 计数 %d 不一致", filtered.Total, facetCount)
	}
	if got := filtered.Facets.Capabilities; len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("自身 facet 计数错误: %+v", got)
	}

	cards := cardsByID(body.Items)
	if got := cards["dup/repeat"].Capabilities; !equalStrings(got, []string{"text"}) {
		t.Fatalf("重复能力输出未去重: %v", got)
	}
	if got := cards["dup/mixed"].Capabilities; !equalStrings(got, []string{"Text"}) {
		t.Fatalf("大小写变体应保留首个原值: %v", got)
	}
}

// display_name 的判空与下发使用同一口径：返回 TrimSpace 后的值。
func TestPublicPricingDisplayNameTrimmed(t *testing.T) {
	catalog := store.PublicPricingCatalog{
		Models: []store.PublicModelPricing{
			{Model: "trim/name", Mode: 0, PromptPrice: publicInt64Ptr(100)},
		},
		Meta: []store.ModelMeta{{
			ModelName: "trim/name", Status: 1, NameRule: 0,
			DisplayName: publicStringPtr("  Padded Name  "),
		}},
		Version: "v1", GeneratedAt: time.Now().UTC(),
	}
	repo := &fakePublicPricingRepo{catalog: catalog}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, body, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if got := cardsByID(body.Items)["trim/name"].DisplayName; got != "Padded Name" {
		t.Fatalf("display_name = %q，期望 TrimSpace 后的 Padded Name", got)
	}
}

// parameters 逐元素解析：一个非法元素不能把整张参数表清空。
func TestDecodeModelParametersSkipsBadElements(t *testing.T) {
	raw := []byte(`[{"name":"ok","required":true,"type":"number","default":1,"description":"d"},1,"bad",null,{"name":" "},{"type":"string"}]`)
	params := decodeModelParameters(raw)
	if len(params) != 1 {
		t.Fatalf("params = %+v，期望只保留 1 条合法元素", params)
	}
	got := params[0]
	if got.Name != "ok" || !got.Required || got.Type != "number" || got.Description != "d" {
		t.Fatalf("合法元素解析错误: %+v", got)
	}
	if value, ok := got.Default.(float64); !ok || value != 1 {
		t.Fatalf("default = %#v，期望 1", got.Default)
	}
}

// 停用的精确档案压过启用的前缀档案：模型整体隐藏（列表无、详情 404），
// 不能被更宽的前缀档案救活。
func TestPublicPricingDisabledExactBeatsActivePrefix(t *testing.T) {
	catalog := store.PublicPricingCatalog{
		Models: []store.PublicModelPricing{
			{Model: "exact/pre", Mode: 0, PromptPrice: publicInt64Ptr(100)},
		},
		Meta: []store.ModelMeta{
			{ModelName: "exact/pre", DisplayName: publicStringPtr("Disabled Exact"), Status: 2, NameRule: 0},
			{ModelName: "exact/", DisplayName: publicStringPtr("Active Prefix"), Status: 1, NameRule: 1},
		},
		Version: "v1", GeneratedAt: time.Now().UTC(),
	}
	repo := &fakePublicPricingRepo{catalog: catalog}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, body, _ := env.decodeList(t, "/api/pricing?page_size=100", nil, nil)
	if body.Total != "0" || len(body.Items) != 0 {
		t.Fatalf("停用精确档案应压过启用前缀档案，得到 total=%s items=%v",
			body.Total, modelIDs(body.Items))
	}

	detail := env.request(http.MethodGet, "/api/pricing/"+url.PathEscape("exact/pre"), nil, nil)
	if detail.Code != http.StatusNotFound {
		t.Fatalf("停用精确档案模型详情 status = %d，期望 404", detail.Code)
	}
}

// 参数形态：capability[] 方括号数组、endpoint_type 别名、单组 50 值上限。
func TestPublicPricingFacetParameterForms(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalogWithMeta()}
	env := newPublicTestEnv(t, nil, repo, nil)

	_, bracket, _ := env.decodeList(t, "/api/pricing?capability[]=vision&page_size=100", nil, nil)
	if bracket.Total != "1" || len(bracket.Items) != 1 || bracket.Items[0].ModelID != "openai/gpt-5" {
		t.Fatalf("capability[] 形态失败: total=%s items=%v", bracket.Total, modelIDs(bracket.Items))
	}

	_, alias, _ := env.decodeList(t, "/api/pricing?endpoint_type=generateContent&page_size=100", nil, nil)
	if alias.Total != "1" || len(alias.Items) != 1 || alias.Items[0].ModelID != "gemini/flash" {
		t.Fatalf("endpoint_type 别名失败: total=%s items=%v", alias.Total, modelIDs(alias.Items))
	}

	values := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		values = append(values, fmt.Sprintf("v%02d", i))
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/pricing?capability="+strings.Join(values, ","), http.NoBody)
	parsed := parsePublicPricingQuery(req)
	if len(parsed.Capability) != publicPricingMaxVendorFilters {
		t.Fatalf("capability 取值数 = %d，期望上限 %d", len(parsed.Capability), publicPricingMaxVendorFilters)
	}
	if parsed.Capability[0] != "v00" || parsed.Capability[len(parsed.Capability)-1] != "v49" {
		t.Fatalf("50 值上限应保留前 50 个，得到首 %q 尾 %q",
			parsed.Capability[0], parsed.Capability[len(parsed.Capability)-1])
	}
}

func TestPublicPricingMetadataETag(t *testing.T) {
	repo := &fakePublicPricingRepo{catalog: fixtureCatalogWithMeta()}
	env := newPublicTestEnv(t, nil, repo, nil)

	first := env.request(http.MethodGet, "/api/pricing", nil, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d", first.Code)
	}
	etag := first.Header().Get("ETag")

	// 模拟元数据编辑后版本变化：ETag 必须随之变化，否则 304 会让缓存
	// 继续返回旧名称/旧 facet（D1-22）。
	repo.catalog.Version = "cafebabecafebabe"
	second := env.request(http.MethodGet, "/api/pricing", map[string]string{"If-None-Match": etag}, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("元数据版本变化后条件请求应 200，得到 %d", second.Code)
	}
	if second.Header().Get("ETag") == etag {
		t.Fatal("版本变化后 ETag 未变化")
	}
}
