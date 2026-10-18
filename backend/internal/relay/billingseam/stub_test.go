package billingseam

import (
	"errors"
	"sync"
	"testing"

	"newtg/gateway/internal/contract"
)

// 编译期断言：桩必须实现冻结的 contract.BillingSession 全部 8 个方法
// （GC-3 的「M3 前封死签名」；签名以 contract/types.go §6.1 为准，不以计划草稿为准）。
var _ contract.BillingSession = NewStub()

// intPtr 是取 *int 的测试辅助。
func intPtr(v int) *int { return &v }

// TestPreConsumeAllowsAndReserves 覆盖：默认放行、按 MaxTokens 记预留、会话计数 +1，
// 以及 MaxTokens==nil 时文档化的默认预留 0。
func TestPreConsumeAllowsAndReserves(t *testing.T) {
	st := NewStub()

	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(120)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 120 {
		t.Errorf("Reserved() = %d, want 120", got)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() = %d, want 1", got)
	}

	// MaxTokens 为 nil：桩没有定价能力，按文档默认预留 0，但仍算开启了一个会话。
	if err := st.PreConsume(contract.QuotaEstimate{}); err != nil {
		t.Fatalf("PreConsume() without MaxTokens unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 120 {
		t.Errorf("Reserved() after nil MaxTokens = %d, want 120 (default reservation is 0)", got)
	}
	if got := st.Sessions(); got != 2 {
		t.Errorf("Sessions() = %d, want 2", got)
	}
}

// TestPreConsumeLimitInsufficient 覆盖 GC-4 的 402 语义模拟：
// reserved+名义预留 > limit 时返回可被 errors.Is 识别的额度不足错误，且状态不变。
func TestPreConsumeLimitInsufficient(t *testing.T) {
	st := NewStubWithLimit(50)

	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(30)}); err != nil {
		t.Fatalf("PreConsume(30) under limit unexpected error: %v", err)
	}

	err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(21)}) // 30+21 > 50
	if err == nil {
		t.Fatal("PreConsume() over limit = nil, want insufficient quota error")
	}
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Errorf("PreConsume() error = %v, want errors.Is(_, ErrInsufficientQuota)", err)
	}
	if got := st.Reserved(); got != 30 {
		t.Errorf("Reserved() after failed PreConsume = %d, want 30 (unchanged)", got)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() after failed PreConsume = %d, want 1 (unchanged)", got)
	}

	// 恰好达到上限是允许的（边界不越界）。
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(20)}); err != nil {
		t.Fatalf("PreConsume() exactly at limit unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 50 {
		t.Errorf("Reserved() = %d, want 50", got)
	}
}

// TestLimitNonPositiveIsUnlimited 覆盖 limit <= 0 表示不设上限。
func TestLimitNonPositiveIsUnlimited(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		st := NewStubWithLimit(limit)
		if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(1 << 30)}); err != nil {
			t.Errorf("NewStubWithLimit(%d).PreConsume() unexpected error: %v", limit, err)
		}
	}
}

// TestReserveIncrementSemantics 覆盖冻结的增量语义（billing-00 §6.2 决议 2）：
// delta <= 0 为已达标 no-op；正增量只上浮。幂等由调用方以 target−Reserved() 保证。
func TestReserveIncrementSemantics(t *testing.T) {
	st := NewStub()

	if err := st.Reserve(100); err != nil {
		t.Fatalf("Reserve(100) unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 100 {
		t.Fatalf("Reserved() = %d, want 100", got)
	}

	// 已达标（调用方换算后传 0/负数）→ no-op。
	if err := st.Reserve(0); err != nil {
		t.Fatalf("Reserve(0) unexpected error: %v", err)
	}
	if err := st.Reserve(-25); err != nil {
		t.Fatalf("Reserve(-25) unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 100 {
		t.Errorf("Reserved() after non-positive Reserve = %d, want 100", got)
	}

	// 正增量继续上浮；多轮尝试允许对同一增量连续累加。
	if err := st.Reserve(50); err != nil {
		t.Fatalf("Reserve(50) unexpected error: %v", err)
	}
	if err := st.Reserve(50); err != nil {
		t.Fatalf("Reserve(50) again unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 200 {
		t.Errorf("Reserved() = %d, want 200", got)
	}
	if got := st.Sessions(); got != 0 {
		t.Errorf("Sessions() = %d, want 0 (Reserve must not open a session)", got)
	}
}

// TestReserveLimitInsufficient 覆盖 Reserve 同样受上限约束。
func TestReserveLimitInsufficient(t *testing.T) {
	st := NewStubWithLimit(100)
	if err := st.Reserve(60); err != nil {
		t.Fatalf("Reserve(60) unexpected error: %v", err)
	}
	err := st.Reserve(41) // 60+41 > 100
	if err == nil {
		t.Fatal("Reserve() over limit = nil, want insufficient quota error")
	}
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Errorf("Reserve() error = %v, want errors.Is(_, ErrInsufficientQuota)", err)
	}
	if got := st.Reserved(); got != 60 {
		t.Errorf("Reserved() after failed Reserve = %d, want 60", got)
	}
}

// TestTerminateIdempotent 覆盖：终结把会话计数减一；重复终结返回 nil 且不会减成负数。
func TestTerminateIdempotent(t *testing.T) {
	st := NewStub()
	for i := 0; i < 2; i++ {
		if err := st.PreConsume(contract.QuotaEstimate{}); err != nil {
			t.Fatalf("PreConsume() #%d unexpected error: %v", i, err)
		}
	}
	if got := st.Sessions(); got != 2 {
		t.Fatalf("Sessions() = %d, want 2", got)
	}

	if err := st.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate() unexpected error: %v", err)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() after Terminate = %d, want 1", got)
	}
	if !st.Terminated() {
		t.Error("Terminated() = false, want true")
	}

	if err := st.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("second Terminate() unexpected error: %v", err)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() after repeated Terminate = %d, want 1 (idempotent)", got)
	}

	// 没有会话时终结也不能把计数减成负数。
	empty := NewStub()
	if err := empty.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate() on empty stub unexpected error: %v", err)
	}
	if got := empty.Sessions(); got != 0 {
		t.Errorf("Sessions() = %d, want 0 (must not go negative)", got)
	}
}

// TestChargeIncrementRecordsInMemory 覆盖：ChargeIncrement 只写内存（不落库、不依赖
// DB/Redis —— 本包只 import 标准库与 contract），记录事实与累计 delta，不动预留与会话数。
func TestChargeIncrementRecordsInMemory(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(100)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}

	factA := contract.UsageFact{Model: "gpt-a", Usage: contract.Usage{Prompt: 10}}
	factB := contract.UsageFact{Model: "gpt-b", Usage: contract.Usage{Prompt: 20}}
	if err := st.ChargeIncrement(30, factA); err != nil {
		t.Fatalf("ChargeIncrement(30) unexpected error: %v", err)
	}
	if err := st.ChargeIncrement(20, factB); err != nil {
		t.Fatalf("ChargeIncrement(20) unexpected error: %v", err)
	}

	if got := st.ChargedTotal(); got != 50 {
		t.Errorf("ChargedTotal() = %d, want 50", got)
	}
	facts := st.ChargedFacts()
	if len(facts) != 2 {
		t.Fatalf("ChargedFacts() len = %d, want 2", len(facts))
	}
	if facts[0].Model != "gpt-a" || facts[1].Model != "gpt-b" {
		t.Errorf("ChargedFacts() models = [%q, %q], want [gpt-a, gpt-b]", facts[0].Model, facts[1].Model)
	}
	if got := st.Reserved(); got != 100 {
		t.Errorf("Reserved() = %d, want 100 (ChargeIncrement must not touch reservation)", got)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() = %d, want 1", got)
	}
}

// TestRefundIdempotent 覆盖：全额退款清零预留、置退款位；重复退款幂等。
func TestRefundIdempotent(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(80)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	if !st.NeedsRefund() {
		t.Fatal("NeedsRefund() = false before Refund, want true")
	}

	if err := st.Refund(); err != nil {
		t.Fatalf("Refund() unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 0 {
		t.Errorf("Reserved() after Refund = %d, want 0", got)
	}
	if !st.Refunded() {
		t.Error("Refunded() = false, want true")
	}
	if st.NeedsRefund() {
		t.Error("NeedsRefund() = true after Refund, want false")
	}

	if err := st.Refund(); err != nil {
		t.Fatalf("second Refund() unexpected error: %v", err)
	}
	if got := st.Reserved(); got != 0 {
		t.Errorf("Reserved() after repeated Refund = %d, want 0", got)
	}
}

// TestNeedsRefund 覆盖需求判定：只有「存在未终结、未退款的正预留」才需要退款。
func TestNeedsRefund(t *testing.T) {
	noReservation := NewStub()
	if err := noReservation.PreConsume(contract.QuotaEstimate{}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	if noReservation.NeedsRefund() {
		t.Error("NeedsRefund() = true with zero reservation, want false")
	}

	settled := NewStub()
	if err := settled.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(10)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	if !settled.NeedsRefund() {
		t.Error("NeedsRefund() = false with positive reservation, want true")
	}
	if err := settled.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate() unexpected error: %v", err)
	}
	if settled.NeedsRefund() {
		t.Error("NeedsRefund() = true after Terminate, want false (settled)")
	}
}

// TestRecoverAbandonedIdempotent 覆盖崩溃兜底入口在桩上幂等且不改任何内存状态。
func TestRecoverAbandonedIdempotent(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(10)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	before := st.Reserved()

	for i := 0; i < 2; i++ {
		if err := st.RecoverAbandoned("req-1"); err != nil {
			t.Fatalf("RecoverAbandoned() #%d unexpected error: %v", i, err)
		}
	}
	if got := st.Reserved(); got != before {
		t.Errorf("Reserved() after RecoverAbandoned = %d, want %d", got, before)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() after RecoverAbandoned = %d, want 1", got)
	}
}

// TestConcurrentUse 覆盖线程安全：路由测试可能跨 goroutine 复用同一个桩。
// 用 -race 运行时该用例同时充当竞态检测。
func TestConcurrentUse(t *testing.T) {
	st := NewStub()
	const n = 100

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := st.PreConsume(contract.QuotaEstimate{}); err != nil {
				t.Errorf("PreConsume() unexpected error: %v", err)
			}
			if err := st.Reserve(1); err != nil {
				t.Errorf("Reserve() unexpected error: %v", err)
			}
			if err := st.ChargeIncrement(1, contract.UsageFact{Model: "m"}); err != nil {
				t.Errorf("ChargeIncrement() unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := st.Sessions(); got != n {
		t.Errorf("Sessions() = %d, want %d", got, n)
	}
	if got := st.Reserved(); got != n {
		t.Errorf("Reserved() = %d, want %d", got, n)
	}
	if got := st.ChargedTotal(); got != n {
		t.Errorf("ChargedTotal() = %d, want %d", got, n)
	}
	if got := len(st.ChargedFacts()); got != n {
		t.Errorf("len(ChargedFacts()) = %d, want %d", got, n)
	}
}
