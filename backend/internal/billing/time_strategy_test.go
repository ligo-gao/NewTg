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
}

func (f *fakeRequestPricingStore) Upsert(context.Context, store.RequestPricing) error {
	f.upsert++
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
