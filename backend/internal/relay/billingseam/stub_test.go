package billingseam

import (
	"errors"
	"math"
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

	// MaxTokens 为 nil：桩没有定价能力，按文档默认预留 0（单会话模型：用独立桩）。
	noMax := NewStub()
	if err := noMax.PreConsume(contract.QuotaEstimate{}); err != nil {
		t.Fatalf("PreConsume() without MaxTokens unexpected error: %v", err)
	}
	if got := noMax.Reserved(); got != 0 {
		t.Errorf("Reserved() with nil MaxTokens = %d, want 0 (default reservation is 0)", got)
	}
	if got := noMax.Sessions(); got != 1 {
		t.Errorf("Sessions() = %d, want 1", got)
	}
}

// TestPreConsumeLimitInsufficient 覆盖 GC-4 的 402 语义模拟：
// 名义预留超过上限时返回可被 errors.Is / errors.As 识别的额度不足错误，且状态不变。
func TestPreConsumeLimitInsufficient(t *testing.T) {
	st := NewStubWithLimit(50)

	err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(51)}) // 0+51 > 50
	if err == nil {
		t.Fatal("PreConsume() over limit = nil, want insufficient quota error")
	}
	if !errors.Is(err, ErrInsufficientQuota) {
		t.Errorf("PreConsume() error = %v, want errors.Is(_, ErrInsufficientQuota)", err)
	}
	var quotaErr *InsufficientQuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("PreConsume() error = %T, want *InsufficientQuotaError", err)
	}
	if quotaErr.Class != contract.ErrInsufficientQuota {
		t.Errorf("InsufficientQuotaError.Class = %d, want contract.ErrInsufficientQuota", quotaErr.Class)
	}
	if quotaErr.Reserved != 0 || quotaErr.Requested != 51 || quotaErr.Limit != 50 {
		t.Errorf("InsufficientQuotaError fields = {%d, %d, %d}, want {0, 51, 50}",
			quotaErr.Reserved, quotaErr.Requested, quotaErr.Limit)
	}
	if got := st.Reserved(); got != 0 {
		t.Errorf("Reserved() after failed PreConsume = %d, want 0 (unchanged)", got)
	}
	if got := st.Sessions(); got != 0 {
		t.Errorf("Sessions() after failed PreConsume = %d, want 0 (unchanged)", got)
	}
	if st.Terminated() {
		t.Error("Terminated() = true after failed PreConsume, want false")
	}

	// 恰好达到上限是允许的（边界不越界）；失败的调用不占用单会话名额。
	exact := NewStubWithLimit(50)
	if err := exact.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(50)}); err != nil {
		t.Fatalf("PreConsume() exactly at limit unexpected error: %v", err)
	}
	if got := exact.Reserved(); got != 50 {
		t.Errorf("Reserved() = %d, want 50", got)
	}
}

// TestPreConsumeSingleSession 覆盖单会话契约：一个 Stub 只接受一次 PreConsume，
// 重复调用返回 ErrSessionAlreadyStarted，且不改变已建立的会话状态（Terminate 后亦然）。
func TestPreConsumeSingleSession(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(10)}); err != nil {
		t.Fatalf("first PreConsume() unexpected error: %v", err)
	}

	err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(10)})
	if err == nil {
		t.Fatal("second PreConsume() = nil, want ErrSessionAlreadyStarted")
	}
	if !errors.Is(err, ErrSessionAlreadyStarted) {
		t.Errorf("second PreConsume() error = %v, want errors.Is(_, ErrSessionAlreadyStarted)", err)
	}
	if got := st.Reserved(); got != 10 {
		t.Errorf("Reserved() = %d, want 10 (unchanged)", got)
	}
	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() = %d, want 1 (unchanged)", got)
	}

	// Terminate 后实例仍视为已占用（单会话契约不因终结而复位）。
	if err := st.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate() unexpected error: %v", err)
	}
	if err := st.PreConsume(contract.QuotaEstimate{}); !errors.Is(err, ErrSessionAlreadyStarted) {
		t.Errorf("PreConsume() after Terminate error = %v, want ErrSessionAlreadyStarted", err)
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

// TestReserveLimitInsufficient 覆盖 Reserve 同样受上限约束（含 int64 溢出防护）。
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
	var quotaErr *InsufficientQuotaError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("Reserve() error = %T, want *InsufficientQuotaError", err)
	}
	if quotaErr.Class != contract.ErrInsufficientQuota || quotaErr.Reserved != 60 ||
		quotaErr.Requested != 41 || quotaErr.Limit != 100 {
		t.Errorf("InsufficientQuotaError = {Class:%d Reserved:%d Requested:%d Limit:%d}, want {ErrInsufficientQuota, 60, 41, 100}",
			quotaErr.Class, quotaErr.Reserved, quotaErr.Requested, quotaErr.Limit)
	}
	if got := st.Reserved(); got != 60 {
		t.Errorf("Reserved() after failed Reserve = %d, want 60", got)
	}

	// 溢出防护：用「剩余额度」比较，避免 reserved+delta 溢出成负数而绕过限额。
	huge := NewStubWithLimit(math.MaxInt64)
	if err := huge.Reserve(math.MaxInt64); err != nil {
		t.Fatalf("Reserve(MaxInt64) unexpected error: %v", err)
	}
	if err := huge.Reserve(1); !errors.Is(err, ErrInsufficientQuota) {
		t.Errorf("Reserve(1) at MaxInt64 limit error = %v, want ErrInsufficientQuota (overflow guard)", err)
	}
	if got := huge.Reserved(); got != math.MaxInt64 {
		t.Errorf("Reserved() = %d, want MaxInt64", got)
	}
}

// TestTerminateIdempotent 覆盖：单会话 PreConsume→Terminate 把 Sessions 和 Reserved
// 都归零（GC-7 的预算释放可观察）；重复终结返回 nil 且不会减成负数。
func TestTerminateIdempotent(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(70)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	if got := st.Sessions(); got != 1 {
		t.Fatalf("Sessions() = %d, want 1", got)
	}
	if got := st.Reserved(); got != 70 {
		t.Fatalf("Reserved() = %d, want 70", got)
	}

	if err := st.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("Terminate() unexpected error: %v", err)
	}
	if got := st.Sessions(); got != 0 {
		t.Errorf("Sessions() after Terminate = %d, want 0", got)
	}
	if got := st.Reserved(); got != 0 {
		t.Errorf("Reserved() after Terminate = %d, want 0 (hold released)", got)
	}
	if !st.Terminated() {
		t.Error("Terminated() = false, want true")
	}

	if err := st.Terminate(contract.UsageFact{Model: "m"}); err != nil {
		t.Fatalf("second Terminate() unexpected error: %v", err)
	}
	if got := st.Sessions(); got != 0 {
		t.Errorf("Sessions() after repeated Terminate = %d, want 0 (idempotent)", got)
	}
	if got := st.Reserved(); got != 0 {
		t.Errorf("Reserved() after repeated Terminate = %d, want 0", got)
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

// TestRefundAfterTerminate 覆盖 settled 早返回分支：Terminate 已释放 hold，
// 再 Refund 只返回 nil，不置 Refunded()、不改动会话状态（幂等、无状态污染）。
func TestRefundAfterTerminate(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(100)}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}
	if err := st.Terminate(contract.UsageFact{}); err != nil {
		t.Fatalf("Terminate() unexpected error: %v", err)
	}
	if st.NeedsRefund() {
		t.Fatal("NeedsRefund() = true after Terminate, want false (settled)")
	}

	for i := 0; i < 2; i++ {
		if err := st.Refund(); err != nil {
			t.Fatalf("Refund() #%d after Terminate unexpected error: %v", i, err)
		}
	}
	if st.Refunded() {
		t.Error("Refunded() = true after settled Refund, want false (early-return branch)")
	}
	if got := st.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d, want 0", got)
	}
	if got := st.Sessions(); got != 0 {
		t.Errorf("Sessions() = %d, want 0", got)
	}
	if !st.Terminated() {
		t.Error("Terminated() = false, want true (state not corrupted)")
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
// 用 -race 运行时该用例同时充当竞态检测（单会话：先 PreConsume，再并发上浮/扣减）。
func TestConcurrentUse(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{}); err != nil {
		t.Fatalf("PreConsume() unexpected error: %v", err)
	}

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if err := st.Reserve(1); err != nil {
				t.Errorf("Reserve() unexpected error: %v", err)
			}
			if err := st.ChargeIncrement(1, contract.UsageFact{Model: "m"}); err != nil {
				t.Errorf("ChargeIncrement() unexpected error: %v", err)
			}
			// 并发读路径同样要经 mutex，顺带覆盖访问器。
			_ = st.Reserved()
			_ = st.NeedsRefund()
			_ = st.ChargedFacts()
		}()
	}
	wg.Wait()

	if got := st.Sessions(); got != 1 {
		t.Errorf("Sessions() = %d, want 1 (single session)", got)
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

// TestRefundClosesZeroReservationSession 冻结 T9 的失败出口语义：
// PreConsume 未传 MaxTokens（名义预留 0）时，Refund 仍必须终结会话计数
// （Relay 的失败路径统一走 Refund；若桩不关闭会话，路由测试会把已退款
// 的请求显示为存活）。
func TestRefundClosesZeroReservationSession(t *testing.T) {
	st := NewStub()
	if err := st.PreConsume(contract.QuotaEstimate{Model: "m"}); err != nil {
		t.Fatalf("PreConsume: %v", err)
	}
	if st.Reserved() != 0 || st.Sessions() != 1 {
		t.Fatalf("reserved=%d sessions=%d, want 0/1", st.Reserved(), st.Sessions())
	}
	if err := st.Refund(); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if !st.Refunded() {
		t.Fatal("Refunded() = false, want true（失败出口必须可观察）")
	}
	if st.Sessions() != 0 || st.Reserved() != 0 {
		t.Fatalf("after Refund sessions=%d reserved=%d, want 0/0", st.Sessions(), st.Reserved())
	}
	// 幂等：再次 Refund 不改变状态。
	if err := st.Refund(); err != nil || st.Sessions() != 0 {
		t.Fatalf("second Refund: err=%v sessions=%d", err, st.Sessions())
	}
	// 未开启的会话（PreConsume 失败）Refund 是 no-op。
	empty := NewStubWithLimit(1)
	if err := empty.PreConsume(contract.QuotaEstimate{MaxTokens: intPtr(10)}); err == nil {
		t.Fatal("PreConsume should fail over the limit")
	}
	if err := empty.Refund(); err != nil {
		t.Fatalf("Refund on unstarted stub: %v", err)
	}
	if empty.Refunded() || empty.Sessions() != 0 {
		t.Fatalf("unstarted Refund must be a no-op: refunded=%v sessions=%d", empty.Refunded(), empty.Sessions())
	}
}
