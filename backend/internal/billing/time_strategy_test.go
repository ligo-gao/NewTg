package billing

// 冻结请求时间与时段倍率的单元验收（spec §5.2）：
//   - 预扣/结算/跨组重试共用会话创建时冻结的 requestTime；
//   - 由 request_pricing 重建的 handoff 会话用 ResolveFixed 复用预扣倍率，
//     不再按当前时间匹配窗口；
//   - 读表错误 fail-closed，缺行按既有行为继续。

import (
	"context"
	"errors"
	"testing"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// fakeTimeSource 记录每次解析的时间/倍率，并返回固定快照。
type fakeTimeSource struct {
	snap       contract.PriceSnapshot
	err        error
	atCalls    []time.Time
	fixedCalls []float64
	resolveN   int
}

func (f *fakeTimeSource) Resolve(context.Context, string, string) (contract.PriceSnapshot, error) {
	f.resolveN++
	return f.snap, f.err
}

func (f *fakeTimeSource) ResolveAt(_ context.Context, _, _ string, at time.Time) (contract.PriceSnapshot, error) {
	f.atCalls = append(f.atCalls, at)
	return f.snap, f.err
}

func (f *fakeTimeSource) ResolveFixed(_ context.Context, _, _ string, multiplier float64) (contract.PriceSnapshot, error) {
	f.fixedCalls = append(f.fixedCalls, multiplier)
	snap := f.snap
	snap.TimeMultiplier = multiplier
	return snap, f.err
}

// fakeRequestPricingStore 是 RequestPricingStore 的内存实现。
type fakeRequestPricingStore struct {
	rec    store.RequestPricing
	ok     bool
	getErr error
	upsert int
	// upserted 记录最后一次写入的整条事实：折扣冻结断言需要看到 Upsert
	// 实际收到的值，而不仅是调用次数。
	upserted store.RequestPricing
}

func (f *fakeRequestPricingStore) Upsert(_ context.Context, rec store.RequestPricing) error {
	f.upsert++
	f.upserted = rec
	return nil
}

func (f *fakeRequestPricingStore) Get(context.Context, string) (store.RequestPricing, bool, error) {
	return f.rec, f.ok, f.getErr
}

func (f *fakeRequestPricingStore) DeleteBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}

// TestSessionResolveAtReusesFrozenRequestTime 验证预扣与结算的两次解析都
// 使用同一个 requestTime（跨整点不换价）。
func TestSessionResolveAtReusesFrozenRequestTime(t *testing.T) {
	src := &fakeTimeSource{}
	frozen := time.Date(2026, 10, 26, 23, 59, 30, 0, time.UTC)
	s := &Session{engine: &Engine{source: src}, requestTime: frozen}

	for i := 0; i < 2; i++ {
		if _, err := s.resolveAt(context.Background(), "deepseek-flash", "default"); err != nil {
			t.Fatalf("resolveAt: %v", err)
		}
	}
	if len(src.atCalls) != 2 {
		t.Fatalf("ResolveAt calls = %d, want 2", len(src.atCalls))
	}
	for i, at := range src.atCalls {
		if !at.Equal(frozen) {
			t.Fatalf("ResolveAt[%d] at = %v, want frozen %v", i, at, frozen)
		}
	}
	if src.resolveN != 0 || len(src.fixedCalls) != 0 {
		t.Fatalf("不应走 Resolve/ResolveFixed: resolve=%d fixed=%v", src.resolveN, src.fixedCalls)
	}
}

// TestSessionResolveAtUsesFrozenMultiplier 验证重建会话只按冻结倍率缩放。
func TestSessionResolveAtUsesFrozenMultiplier(t *testing.T) {
	src := &fakeTimeSource{}
	multiplier := 0.5
	s := &Session{
		engine:           &Engine{source: src},
		requestTime:      time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
		frozenMultiplier: &multiplier,
	}
	out, err := s.resolveAt(context.Background(), "deepseek-flash", "default")
	if err != nil {
		t.Fatalf("resolveAt: %v", err)
	}
	if len(src.fixedCalls) != 1 || src.fixedCalls[0] != 0.5 {
		t.Fatalf("ResolveFixed calls = %v, want [0.5]", src.fixedCalls)
	}
	if len(src.atCalls) != 0 {
		t.Fatalf("不应调用 ResolveAt: %v", src.atCalls)
	}
	if out.TimeMultiplier != 0.5 {
		t.Fatalf("TimeMultiplier = %v, want 0.5", out.TimeMultiplier)
	}
}

// TestLoadFrozenPricingUsesStoredRecord 验证 handoff 重建会话从
// request_pricing 读回时间与倍率，并在后续解析中走 ResolveFixed。
func TestLoadFrozenPricingUsesStoredRecord(t *testing.T) {
	src := &fakeTimeSource{}
	storedAt := time.Date(2026, 10, 26, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))
	repo := &fakeRequestPricingStore{
		rec: store.RequestPricing{
			RequestID:      "0123456789abcdef0123456789abcdef",
			RequestTime:    storedAt,
			TimeMultiplier: 1.0,
			TimeWindow:     "上午高峰",
		},
		ok: true,
	}
	e := &Engine{source: src, requestPricing: repo}
	s := &Session{engine: e, requestID: repo.rec.RequestID}

	ok, err := e.loadFrozenPricing(context.Background(), s.requestID, s)
	if err != nil || !ok {
		t.Fatalf("loadFrozenPricing = %v,%v, want true,nil", ok, err)
	}
	if !s.requestTime.Equal(storedAt) {
		t.Fatalf("requestTime = %v, want %v", s.requestTime, storedAt)
	}
	if _, err := s.resolveAt(context.Background(), "m", "default"); err != nil {
		t.Fatalf("resolveAt: %v", err)
	}
	if len(src.fixedCalls) != 1 || src.fixedCalls[0] != 1.0 {
		t.Fatalf("ResolveFixed calls = %v, want [1]", src.fixedCalls)
	}
}

// TestLoadFrozenPricingFailClosedOnReadError 验证读表失败返回错误，绝不用
// time.Now() 兜底（spec §5.2）。
func TestLoadFrozenPricingFailClosedOnReadError(t *testing.T) {
	repo := &fakeRequestPricingStore{getErr: errors.New("db down")}
	e := &Engine{source: &fakeTimeSource{}, requestPricing: repo}
	s := &Session{engine: e, requestID: "0123456789abcdef0123456789abcdef"}

	if _, err := e.loadFrozenPricing(context.Background(), s.requestID, s); err == nil {
		t.Fatal("loadFrozenPricing = nil err, want fail-closed")
	}
	if s.frozenMultiplier != nil {
		t.Fatal("读失败后不应写入 frozenMultiplier")
	}
}

// TestLoadFrozenPricingMissingRowFallsBack 验证上线前在飞请求（无行）按既有
// 行为继续，且不注入冻结倍率。
func TestLoadFrozenPricingMissingRowFallsBack(t *testing.T) {
	repo := &fakeRequestPricingStore{ok: false}
	e := &Engine{source: &fakeTimeSource{}, requestPricing: repo}
	s := &Session{engine: e, requestID: "0123456789abcdef0123456789abcdef"}

	ok, err := e.loadFrozenPricing(context.Background(), s.requestID, s)
	if err != nil || ok {
		t.Fatalf("loadFrozenPricing = %v,%v, want false,nil", ok, err)
	}
	if s.frozenMultiplier != nil {
		t.Fatal("缺行时不应写入 frozenMultiplier")
	}
}

// TestNeedsFrozenPricing 验证只有「按 fact 重算」的结算才依赖冻结事实。
func TestNeedsFrozenPricing(t *testing.T) {
	zero := contract.UsageFact{}
	if needsFrozenPricing(nil, zero) {
		t.Fatal("空 fact（按预扣额结清）不应解析定价")
	}
	if needsFrozenPricing(nil, contract.UsageFact{Model: "m"}) != true {
		t.Fatal("非空 fact 需要冻结倍率")
	}
	cost := int64(10)
	if needsFrozenPricing(&cost, contract.UsageFact{Model: "m"}) {
		t.Fatal("冻结成本不需要解析定价")
	}
}

// TestPersistRequestPricingFreezesDiscount 验证预扣持久化把会话里已冻结的
// 客户折扣一并写进 request_pricing：异步结算只拿得到 requestID，折扣若不
// 落库，重建会话就会按原价结算并追回预扣时已让利的部分。
func TestPersistRequestPricingFreezesDiscount(t *testing.T) {
	repo := &fakeRequestPricingStore{}
	e := &Engine{requestPricing: repo}
	s := &Session{
		engine:    e,
		requestID: "0123456789abcdef0123456789abcdef",
		discount:  contract.Discount{Rate: 0.9, Target: "user:7"},
	}

	s.persistRequestPricing(context.Background())

	if repo.upsert != 1 {
		t.Fatalf("Upsert calls = %d, want 1", repo.upsert)
	}
	if repo.upserted.DiscountRate != 0.9 || repo.upserted.DiscountTarget != "user:7" {
		t.Fatalf("Upsert 折扣 = (%v, %q), want (0.9, \"user:7\")",
			repo.upserted.DiscountRate, repo.upserted.DiscountTarget)
	}
}

// TestLoadFrozenPricingRestoresDiscount 验证 loadFrozenPricing 在同一次 Get
// 里顺带恢复冻结折扣，不重复查询。
func TestLoadFrozenPricingRestoresDiscount(t *testing.T) {
	repo := &fakeRequestPricingStore{
		rec: store.RequestPricing{
			RequestID:      "0123456789abcdef0123456789abcdef",
			RequestTime:    time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC),
			TimeMultiplier: 1.0,
			DiscountRate:   0.8,
			DiscountTarget: "group:acme",
		},
		ok: true,
	}
	e := &Engine{source: &fakeTimeSource{}, requestPricing: repo}
	s := &Session{engine: e, requestID: repo.rec.RequestID}

	ok, err := e.loadFrozenPricing(context.Background(), s.requestID, s)
	if err != nil || !ok {
		t.Fatalf("loadFrozenPricing = %v,%v, want true,nil", ok, err)
	}
	if !s.discountResolved {
		t.Fatal("loadFrozenPricing 必须把折扣标记为已解析")
	}
	if s.discount.Rate != 0.8 || s.discount.Target != "group:acme" {
		t.Fatalf("discount = %+v, want 0.8/group:acme", s.discount)
	}
}

// TestLoadFrozenDiscountWarnsOnMissingRow 验证缺行（上线前在飞请求）时按
// 「已解析、无折扣」继续：不返回错误，也不留下需要再查一次的未解析态。
func TestLoadFrozenDiscountWarnsOnMissingRow(t *testing.T) {
	repo := &fakeRequestPricingStore{ok: false}
	e := &Engine{requestPricing: repo}
	s := &Session{engine: e, requestID: "0123456789abcdef0123456789abcdef"}

	if err := e.loadFrozenDiscount(context.Background(), s.requestID, s); err != nil {
		t.Fatalf("loadFrozenDiscount(缺行) = %v, want nil", err)
	}
	if !s.discountResolved {
		t.Fatal("缺行也必须标记 discountResolved（确定态：原价）")
	}
	if s.discount.Rate != 1 || s.discount.Target != "" {
		t.Fatalf("discount = %+v, want 1/空（原价）", s.discount)
	}
}
