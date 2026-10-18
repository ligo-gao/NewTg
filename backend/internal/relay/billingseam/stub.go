// Package billingseam 是中继与计费引擎之间的**接缝桩**。
//
// 【M3 替换点 · GC-3】M2 只调用 contract.BillingSession，不实现任何结算逻辑。
// 在 M3 计费引擎落地前，本包用内存桩封死 frozen 签名（contract/types.go §6.1），
// M3 只需替换构造点（NewStub / NewStubWithLimit），调用方无需改动。
// 因此本文件的导出面刻意保持很小：8 个契约方法 + 少量测试用只读访问器。
//
// 【禁止下沉定价/归一化 · GC-3】计费接缝保持单向：定价、归一化、资金来源、
// 幂等落库都属于 M3 计费引擎。本文件只记录调用与内存状态，**不得**出现任何
// 定价、token 归一化、折扣或资金逻辑。
//
// 【GC-4：计费不阻塞中继】桩不做真实扣费，默认恒放行；只有显式通过
// NewStubWithLimit 构造时才按「预留不得越过正上限」模拟余额不足（402）语义，
// 让路由层可以测试 ErrInsufficientQuota 的失败分支。
//
// 【单会话契约】一个 Stub 实例代表**一个请求/会话**（sessions 只会是 0 或 1）：
// 第二次 PreConsume 返回 ErrSessionAlreadyStarted；Terminate 后该实例即终结。
// 路由测试若需并发多请求，应为每个请求构造独立的桩。
//
// 【无外部依赖】仅标准库 + internal/contract：不触碰 DB/Redis（GC-3），
// 也不 import store/config/auth（depguard 的 relay-boundary 亦禁止）。
// 并发安全：路由测试可能跨 goroutine 复用同一个桩，故全部状态由 sync.Mutex 保护。
package billingseam

import (
	"errors"
	"fmt"
	"sync"

	"newtg/gateway/internal/contract"
)

// 编译期断言：冻结的契约有 8 个方法（PreConsume / Reserve / ChargeIncrement /
// Terminate / Refund / Reserved / NeedsRefund / RecoverAbandoned），
// 桩必须逐字实现，防止 M3 替换时才发现签名漂移（GC-3）。
var _ contract.BillingSession = (*Stub)(nil)

// ErrInsufficientQuota 是桩在「预留将超过上限」时返回的哨兵错误。
//
// 【为什么不直接用 contract.ErrInsufficientQuota】契约里它是 ErrorClass
// （int 枚举，见 contract/types.go §4），不是 error —— errors.Is 的 target
// 形参类型为 error，编译期无法接受它。但 M2 路由测试必须能断言 402 语义
// （GC-4），故本包导出同名哨兵。**该哨兵是桩专用的**：M3 真实实现返回契约
// 错误时，调用方应按 contract.ErrorClass 判定。
var ErrInsufficientQuota = errors.New("billingseam: insufficient quota")

// InsufficientQuotaError 是限额拒绝的结构化错误（两条限额路径都返回它）。
//
// 与哨兵的分工：
//   - Unwrap() 返回 ErrInsufficientQuota，供 errors.Is 快速断言桩的 402 分支；
//   - Class 携带契约的 contract.ErrInsufficientQuota（ErrorClass）—— 契约只冻结
//     了枚举而没有 error 值，且 M3 的 internal/billing 不能 import 本包（依赖方向
//     禁止），故需要程序化分类的消费方应 errors.As 到本类型并读取 Class，
//     而不是依赖哨兵（哨兵仅属于桩）。
type InsufficientQuotaError struct {
	Class     contract.ErrorClass // 契约错误类：恒为 contract.ErrInsufficientQuota
	Reserved  int64               // 拒绝时的当前预留
	Requested int64               // 本次请求的增量（PreConsume 的名义预留或 Reserve 的 delta）
	Limit     int64               // 生效的正上限
}

// Error 实现 error。不再重复加 "billingseam: " 前缀：包名只由 Unwrap 链上的
// 哨兵文本携带，避免错误消息出现两次包名。
func (e *InsufficientQuotaError) Error() string {
	return fmt.Sprintf(
		"insufficient quota: reserved %d + requested %d exceeds limit %d (contract.ErrInsufficientQuota=%d)",
		e.Reserved, e.Requested, e.Limit, int(e.Class),
	)
}

// Unwrap 让 errors.Is(err, ErrInsufficientQuota) 成立（桩专用哨兵）。
func (e *InsufficientQuotaError) Unwrap() error { return ErrInsufficientQuota }

// ErrSessionAlreadyStarted 表示在单会话桩上重复调用 PreConsume。
//
// 一个 Stub 实例只代表一个请求/会话（见包注释）；拒绝重复预扣可让路由测试
// 尽早暴露「同一会话被复用」的生命周期错误，而不是在内存计数里悄悄叠加。
var ErrSessionAlreadyStarted = errors.New("billingseam: single-session stub already started by PreConsume")

// Stub 是 contract.BillingSession 的内存桩。
//
// 【单会话】一个实例代表一个请求/会话：PreConsume 成功后实例即被占用，
// 再次 PreConsume 返回 ErrSessionAlreadyStarted；Terminate/Refund 后实例终结。
// 不要把同一个桩用于多个请求。
//
// 零值不可用：必须经 NewStub / NewStubWithLimit 构造（limit 的语义需要构造器
// 区分「未设置上限」与「上限为 0」，并避免复制含 mutex 的结构体）。
type Stub struct {
	mu sync.Mutex // 保护以下全部字段（路由测试可能并发复用）
	// limit 是预留上限：> 0 生效；<= 0 表示不设上限（NewStub 的默认）。
	limit int64
	// reserved 是当前预留量（PreConsume 的名义预留 + Reserve 的增量上浮）。
	// 不变量：limit > 0 时 0 <= reserved <= limit（所有上浮路径都按剩余额度比较，
	// 避免 reserved+delta 的 int64 溢出绕过限额）。
	reserved int64
	// started 标记本实例是否已被 PreConsume 占用（单会话契约，不因 Terminate 复位）。
	started bool
	// sessions 是存活的会话数：PreConsume 成功 +1，Terminate −1（只会是 0 或 1）。
	sessions int
	// terminated / refunded 是会话状态位，用于幂等早返回。
	terminated bool
	refunded   bool
	// chargedTotal / chargedFacts 记录 ChargeIncrement 的调用（只写内存，不落库）。
	chargedTotal int64
	chargedFacts []contract.UsageFact
}

// NewStub 返回不设额度上限的桩：PreConsume 恒放行（GC-4 的默认行为）。
func NewStub() *Stub { return NewStubWithLimit(0) }

// NewStubWithLimit 返回带预留上限的桩：limit > 0 时，PreConsume / Reserve
// 使 reserved 超过 limit 即返回 *InsufficientQuotaError（errors.Is 命中
// ErrInsufficientQuota），模拟 GC-4 的 402；limit <= 0 表示不设上限。
func NewStubWithLimit(limit int64) *Stub {
	return &Stub{limit: limit}
}

// PreConsume 首次预扣。桩的语义：
//   - 单会话：本实例已被 PreConsume 占用过 → 返回 ErrSessionAlreadyStarted
//     （一个 Stub 只代表一个请求/会话；Terminate 后也不复位）。
//   - 名义预留 = *est.MaxTokens（用户声明的输出上界）；MaxTokens == nil 时取 0。
//     之所以默认 0：桩没有定价能力（GC-3 禁止在此做定价），不能凭空编造预留额。
//   - 成功调用开启唯一会话：reserved += 名义预留，sessions = 1。
//   - limit > 0 且名义预留超过剩余额度 → 返回 *InsufficientQuotaError
//     （包装 ErrInsufficientQuota），且不改变任何状态（路由层据此映射 HTTP 402）。
func (s *Stub) PreConsume(est contract.QuotaEstimate) error {
	nominal := int64(0)
	if est.MaxTokens != nil {
		nominal = int64(*est.MaxTokens)
	}
	// 负上界（调用方违约）按 0 处理，避免反向削减已有预留。
	if nominal < 0 {
		nominal = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return ErrSessionAlreadyStarted
	}
	// 用「剩余额度」比较而非 reserved+nominal：后者在 limit 接近 MaxInt64 时
	// 可能溢出成负数，从而绕过限额（Reserved 不变量保证 reserved <= limit）。
	if s.limit > 0 && nominal > s.limit-s.reserved {
		return &InsufficientQuotaError{
			Class: contract.ErrInsufficientQuota, Reserved: s.reserved, Requested: nominal, Limit: s.limit,
		}
	}
	s.started = true
	s.reserved += nominal
	s.sessions = 1
	return nil
}

// Reserve 仅上浮预留、不产生消耗（契约 §6.1 补遗 3：Reserve 与 ChargeIncrement
// 职责严格分离）。
//
// 参数按契约的**增量**语义解释（billing-00 §6.2 决议 2：调用方以
// delta = target − Reserved() 换算）。delta <= 0 视为「已达标」→ no-op；
// 否则 reserved += delta。
//
// 幂等性由调用方按目标差额调用保证，本桩**不做按参去重**：实时/多模态路径
// 允许对同一增量连续上浮（两次 +50 必须得到 +100），若按参数去重会系统性少预留。
func (s *Stub) Reserve(delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if delta <= 0 {
		return nil // 已达标或非法增量：只上浮，不下降
	}
	// 剩余额度比较，避免 reserved+delta 的 int64 溢出绕过限额。
	if s.limit > 0 && delta > s.limit-s.reserved {
		return &InsufficientQuotaError{
			Class: contract.ErrInsufficientQuota, Reserved: s.reserved, Requested: delta, Limit: s.limit,
		}
	}
	s.reserved += delta
	return nil
}

// ChargeIncrement 记录一次实际扣减与用量事实。
//
// 【只写内存 · GC-3】桩不落库（无 DB/Redis 依赖），不建模资金账户，
// 也不改动 reserved：真实扣费与流水属于 M3 计费引擎。
// delta 按调用原样累计，负值不拒绝 —— 记录的是调用事实，钳制属 M3。
func (s *Stub) ChargeIncrement(delta int64, fact contract.UsageFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.chargedTotal += delta
	s.chargedFacts = append(s.chargedFacts, fact)
	return nil
}

// Terminate 终结会话并**释放预留 hold**：reserved 清零，使 GC-7 的
// 「预算释放」可以通过 Reserved() 观察（Task 3 会断言 Terminate 后 Reserved()==0）。
//
// 幂等：重复调用返回 nil；sessions 只在 > 0 时减一，因此重复终结不会把计数
// 减成负数（单会话模型下 sessions 只会是 0 或 1）。桩不计算最终差额
// （GC-3：结算属 M3）。
func (s *Stub) Terminate(fact contract.UsageFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.terminated {
		return nil
	}
	s.terminated = true
	s.reserved = 0
	if s.sessions > 0 {
		s.sessions--
	}
	return nil
}

// Refund 全额退款。遵循契约的早返回矩阵（billing-00 §6.2）：
// 已终结（settled）/ 已退款 / 无需退款时 no-op；否则置 refunded 并把 reserved 清零。
// 幂等：重复调用返回 nil。注意 settled 分支：Terminate 已释放 hold（reserved=0），
// 此后再 Refund 只返回 nil，不会把 Refunded() 置真、也不会改动状态。
func (s *Stub) Refund() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.refunded || !s.needsRefundLocked() {
		return nil
	}
	s.refunded = true
	s.reserved = 0
	return nil
}

// Reserved 返回当前预留量。
func (s *Stub) Reserved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserved
}

// NeedsRefund 报告是否还需要走退款路径：仅当存在**未终结、未退款的正预留**时
// 为 true。预留为 0（如 PreConsume 时 MaxTokens == nil）无需退款。
func (s *Stub) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

// needsRefundLocked 是 NeedsRefund 的无锁版本，供持锁方法内部复用。
func (s *Stub) needsRefundLocked() bool {
	return !s.terminated && !s.refunded && s.reserved > 0
}

// RecoverAbandoned 是崩溃兜底入口（契约 §6.2 · 决议 A3：按 requestID 终结悬挂
// 会话并全额退款）。桩是纯内存态、没有可恢复的持久化会话，故按**幂等 no-op**
// 处理并返回 nil；真实实现归 M3（消费方为 worker 的兜底扫描）。
func (s *Stub) RecoverAbandoned(requestID string) error {
	return nil
}

// Sessions 返回当前存活的会话数（单会话模型下为 0 或 1：PreConsume 成功置 1，
// Terminate 置 0）。只读访问器，供路由测试断言生命周期；不属于 contract.BillingSession。
func (s *Stub) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

// ChargedFacts 返回 ChargeIncrement 记录过的用量事实副本（防止测试篡改内部状态）。
func (s *Stub) ChargedFacts() []contract.UsageFact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.UsageFact(nil), s.chargedFacts...)
}

// ChargedTotal 返回 ChargeIncrement 累计的 delta 总和。
func (s *Stub) ChargedTotal() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chargedTotal
}

// Terminated 返回会话是否已终结。
func (s *Stub) Terminated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminated
}

// Refunded 返回会话是否已退款。
func (s *Stub) Refunded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refunded
}
