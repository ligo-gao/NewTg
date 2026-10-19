package httpapi

// M6c Task 7 公开定价目录的单测：用内存仓储覆盖契约语义（分页/搜索/排序/
// facet/降级/金额字符串/is_free/分组价/ETag/404），SQL 可见集合与版本派生
// 由 pricing_public_repo_integration_test.go 覆盖。

import (
	"context"
	"encoding/json"
	"errors"
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
	if gpt.Vendor != "openai" || gpt.DisplayName != "openai/gpt-5" {
		t.Fatalf("vendor/display_name 口径错误: %+v", gpt)
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
		t.Fatalf("缺失元数据的 facet 必须为空数组（不是 null）: %+v", body.Facets)
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

	// P1 metrics：只注册 404，不返回伪造数据。
	metrics := env.request(http.MethodGet, "/api/pricing/"+encoded+"/metrics", nil, nil)
	if metrics.Code != http.StatusNotFound {
		t.Fatalf("metrics status = %d", metrics.Code)
	}
	if err := json.Unmarshal(metrics.Body.Bytes(), &problem); err != nil || problem["type"] != problemTypeNotImplemented {
		t.Fatalf("metrics problem = %v", problem)
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
