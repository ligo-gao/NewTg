package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
)

// maxImageN 是图像张数上界（relay-00 §10.1 MaxImageN = 128）。
// 契约包只以注释引用该常量而未导出，故在此落一份；请求域 [1,128] 的
// 400 拒绝由中继归一化层负责，计费侧只在结算域做防御性钳制。
const maxImageN = 128

// billingOpTimeout 是会话内单次数据库操作的上限。
//
// 冻结的 BillingSession 方法没有 context 参数（R-08），无法继承请求取消；
// 用固定超时避免数据库抖动把中继 goroutine 永久挂住。超时后：
// 预扣失败 → 请求失败；结算失败 → 保留 hold，由 worker 兜底退款。
const billingOpTimeout = 10 * time.Second

// Session 是一次请求的计费会话（contract.BillingSession 的真实实现）。
//
// 状态位（billing-00 §6.2 / billing-03 §2.3）：
//
//	settled         Terminate 完成；二次 Terminate 直接 no-op
//	refunded        Refund / RecoverAbandoned 完成；不再扣减
//	fundingSettled  Terminate 已提交资金；NeedsRefund 必须为 false（防重复退）
//	trusted         命中信任旁路；Reserve 普通请求 no-op，图片请求例外
//
// 所有方法用同一把互斥锁串行化：会话是每请求对象，锁竞争只在同一请求的
// 并发回调（如实时连接的多个 response.done）之间发生，不构成全局瓶颈。
type Session struct {
	mu sync.Mutex

	engine *Engine
	rc     *contract.RelayContext

	requestID string
	userID    int64

	snap        *contract.PriceSnapshot
	model       string
	group       string
	usedVars    map[string]bool
	usedVarsKey string // 已缓存的 usedVars 对应的表达式哈希/源码

	preConsumed    int64 // 当前仍处于 hold 的金额（数据库行是权威，内存用于早返回）
	consumed       int64 // ChargeIncrement 已实际扣减的累计
	estimatedQuota int64 // 预扣预算（求值失败时的保守回退值）
	reservedImages int   // 请求域图像张数（结算回退用）
	imageRequest   bool
	freeModel      bool
	trusted        bool

	preConsumeCalled bool
	settled          bool
	refunded         bool
	fundingSettled   bool

	leaseRegistered bool
	leaseStop       chan struct{}
	leaseDone       chan struct{}
}

// 编译期断言：Session 必须完整实现冻结的计费会话接口。
var _ contract.BillingSession = (*Session)(nil)

// opCtx 构造单次账本/定价操作的有界上下文（理由见 billingOpTimeout）。
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), billingOpTimeout)
}

// PreConsume 首次预扣（billing-03 §3）。
//
// 预算 = pricing.ComputeQuota(冻结快照, 估算 TokenParams) × pre_consume_ratio
// × pre_consume_multiplier（仅传统倍率输入 / tiered token 分支；R-13 冻结了
// 「QuotaEstimate 无 MaxCost」这一点）。
//
// 余额不足返回 *QuotaError（errors.Is(err, ErrInsufficientQuota) 成立），
// 由 T9 映射 HTTP 402。同一会话二次 PreConsume 返回 ErrBudgetExceeded
// （C-10/D12 的 400 语义）。
func (s *Session) PreConsume(est contract.QuotaEstimate) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if s.settled || s.refunded {
		return fmt.Errorf("%w: request %s", ErrSessionClosed, s.requestID)
	}
	if s.preConsumeCalled {
		// 二次预扣超出预算：C-10/D12 裁决要求整请求 400，不得静默钳制到
		// 首次预算（钳制会把越界请求变成「部分保护」，与上界前置原则冲突）。
		return fmt.Errorf("%w: PreConsume called twice for request %s", ErrBudgetExceeded, s.requestID)
	}
	// 只有**成功**的预扣才锁定会话：配置/余额/租约失败后允许调用方重新发起
	// （瞬时失败不应把会话永久置于「已预扣」状态）。
	s.preConsumeCalled = true
	succeeded := false
	defer func() {
		if !succeeded {
			s.preConsumeCalled = false
		}
	}()

	s.model = est.Model
	if s.model == "" && s.rc != nil {
		s.model = s.rc.OriginModel
	}
	s.group = est.Group
	if s.group == "" && s.rc != nil {
		s.group = s.rc.UsingGroup
	}
	s.imageRequest = est.ImageCount > 0 || est.AttemptImageCount > 0
	s.reservedImages = reservedImageCount(est)

	ctx, cancel := opCtx()
	defer cancel()

	snap, err := s.resolveSnapshotLocked(ctx, est)
	if err != nil {
		return err
	}

	multiplier, err := s.engine.preConsumeMultiplier(snap)
	if err != nil {
		return err
	}
	ratio, err := s.engine.preConsumeRatio()
	if err != nil {
		return err
	}
	usedVars, err := s.usedVarsForLocked(snap)
	if err != nil {
		return err
	}

	// PriceData.FreeModel 是模型级零价事实，无需再求值/估算：直接进入
	// F-08-12 的 max_tokens 预扣策略（同时避免零价模型上残留的表达式配置
	// 让请求因求值错误而失败）。
	if snap.Price.FreeModel {
		s.freeModel = true
		if err := s.preConsumeFreeModelLocked(ctx, est); err != nil {
			return err
		}
		succeeded = true
		return nil
	}

	params := NormalizeTokens(estimateUsage(est), *snap, usedVars)
	params.ImageCount = s.reservedImages
	base, err := pricing.ComputeQuota(*snap, params, usedVars)
	if err != nil {
		return fmt.Errorf("billing: estimate quota: %w", err)
	}
	if base < 0 {
		// 负预算进入账本会反转资金方向；估算阶段出现负值是定价配置错误。
		return fmt.Errorf("%w: negative estimated quota %d", ErrInvalidConfig, base)
	}
	s.estimatedQuota = base

	// F-08-12 免费模型预扣策略：零价模型不做「按估算价预扣」（估算价就是 0），
	// 而是预扣 max_tokens 上界，防止恶意 max_tokens 放大造成上游滥用。
	// 与 billing-00 §6.4 的余额信任旁路是两套独立策略：免费模型只走本策略。
	if staticallyZeroPriced(snap, usedVars, base) {
		s.freeModel = true
		if err := s.preConsumeFreeModelLocked(ctx, est); err != nil {
			return err
		}
		succeeded = true
		return nil
	}

	factor := ratio
	if preConsumeMultiplierApplies(snap, usedVars) {
		factor *= multiplier
	}
	budget := base
	if factor != 1 {
		budget, err = pricing.QuotaFromFloatStrict(float64(base) * factor)
		if err != nil {
			return fmt.Errorf("billing: scale estimated quota: %w", err)
		}
		if budget < 0 {
			return fmt.Errorf("%w: negative scaled quota %d", ErrInvalidConfig, budget)
		}
	}

	// 信任旁路在余额扣减之前判定：命中后 effectiveQuota=0，不写 hold 流水。
	// 若命中，结算仍按真实用量扣费（信任只是免预扣，不是免费）。
	trusted, err := s.engine.shouldTrust(ctx, s.rc, est)
	if err != nil {
		return err
	}
	if trusted {
		s.trusted = true
		succeeded = true
		return nil
	}

	if budget == 0 {
		// 估算为 0（无计费用量/零价）：不产生 hold。结算时若真实用量为正，
		// 走「少补」直接扣减（billing-00 §6.5：少补存在）。
		succeeded = true
		return nil
	}

	if _, err := s.engine.ledger.ApplyDelta(ctx, s.userID, -budget,
		"consume_pre", "hold", "usage_log", s.requestID); err != nil {
		return err // 余额不足时是 *QuotaError
	}
	s.preConsumed = budget

	// 预扣成功后写租约（billing-03 §2.6）：只有真实 hold 才需要崩溃兜底。
	if err := s.registerLeaseLocked(ctx); err != nil {
		return err
	}
	succeeded = true
	return nil
}

// staticallyZeroPriced 判定本次请求是否属于「免费模型」。
//
// 判定（R-24）：PriceData.FreeModel，或估算配额为 0。
//
// tiered 表达式需要额外约束：预扣估算的 P 可能为 0（CountToken 关闭、实时
// 格式等），表达式 `p * 1` 的估算结果也是 0，但这**不说明模型免费**。
// 若据此把请求判为免费，结算会把真实用量按 0 收费（白送）。因此对表达式
// 模式，只有「不引用任何 token 用量变量」时，0 才与用量无关、可判定为零价
// （例如 fixed(0)）。Ratio/PerCall 的 0 来自价格字段本身，无需该约束。
func staticallyZeroPriced(snap *contract.PriceSnapshot, usedVars map[string]bool, base int64) bool {
	if snap == nil {
		return false
	}
	if snap.Price.FreeModel {
		return true
	}
	if base != 0 {
		return false
	}
	if snap.Mode != contract.BillingModeTieredExpr {
		return true
	}
	for _, key := range tokenExprVars {
		if usedVars[key] {
			return false
		}
	}
	return true
}

// preConsumeFreeModelLocked 实现 F-08-12 免费模型预扣策略（调用方持有 s.mu）。
//
// 免费模型的估算价是 0，直接按价预扣没有意义；这里按 max_tokens 上界预扣：
//
//	hold = ceil(MaxTokens × free_model_preconsume_multiplier)
//
// MaxTokens 缺失/≤0 → hold 0（不写流水）。结算价格为 0，Terminate 全额
// 退回 hold；进程崩溃时 hold 由租约超时后的 worker 兜底退（判定窗口
// AbandonedWindowSeconds=600s，即规格的 10 分钟）。
//
// 可信用户（历史累计消费 Σ type='consume' ≥ 阈值）跳过预扣。该判定与
// billing-00 §6.4 的余额信任旁路（billing.trust_quota_usd）是分开的两套策略。
func (s *Session) preConsumeFreeModelLocked(ctx context.Context, est contract.QuotaEstimate) error {
	enabled, err := s.engine.freeModelPreconsumeEnabled()
	if err != nil {
		return err
	}
	if !enabled {
		s.trusted = true
		return nil
	}

	multiplier, err := s.engine.freeModelPreconsumeMultiplier()
	if err != nil {
		return err
	}
	trustCredits, err := s.engine.freeModelPreconsumeTrustCredits()
	if err != nil {
		return err
	}
	if trustCredits > 0 {
		consumed, err := s.engine.cumulativeConsumption(ctx, s.userID)
		if err != nil {
			return err
		}
		if consumed >= trustCredits {
			s.trusted = true
			return nil
		}
	}

	maxTokens := 0
	if est.MaxTokens != nil && *est.MaxTokens > 0 {
		maxTokens = *est.MaxTokens
	}
	hold, err := freeModelHold(maxTokens, multiplier)
	if err != nil {
		return err
	}
	if hold <= 0 {
		s.trusted = true
		return nil
	}

	if _, err := s.engine.ledger.ApplyDelta(ctx, s.userID, -hold,
		"consume_pre", "hold", "usage_log", s.requestID); err != nil {
		return err // 余额不足 → *QuotaError（402）
	}
	s.preConsumed = hold
	return s.registerLeaseLocked(ctx)
}

// Reserve 仅上浮预留，不产生消耗（billing-00 §6.2 补遗 3）。
//
// 参数 delta 是**增量**（relay-04/06 的调用点是 Reserve(+Δ)）。delta ≤ 0
// 等价于「目标 ≤ 当前预留」的 no-op。上浮会真实冻结钱包余额（写第二行
// consume_pre+hold），否则图片两段预扣的「不允许欠费后继续提交」形同虚设
// （billing-03 §3.6 / billing-05 §2.4）。
//
// trusted 会话普通请求 no-op；图片请求例外：清 trusted 并真正预留
// （billing-00 §6.4）。
func (s *Session) Reserve(delta int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if s.settled || s.refunded {
		// 已终结的会话不再接受上浮；幂等语义下返回 nil 而不是错误。
		return nil
	}
	if s.freeModel {
		return nil
	}
	if s.trusted {
		if !s.imageRequest {
			return nil
		}
		// 图片请求必须先冻结资金再出站：清 trusted 后走真实预留。
		s.trusted = false
	}
	if delta <= 0 {
		return nil
	}

	ctx, cancel := opCtx()
	defer cancel()

	if _, err := s.engine.ledger.ApplyDelta(ctx, s.userID, -delta,
		"consume_pre", "hold", "usage_log", s.requestID); err != nil {
		return err
	}
	s.preConsumed += delta

	// 预扣预算为 0 / 信任旁路时首次 Reserve 才产生 hold，此时需要补建租约，
	// 否则崩溃后这笔冻结无人兜底。
	if err := s.registerLeaseLocked(ctx); err != nil {
		return err
	}
	return nil
}

// ChargeIncrement 实际扣减并累计（连接级，实时每个 response.done）。
//
// 与 Reserve 严格分离（billing-00 §6.2 补遗 3）：这里产生 consume 流水，
// 是真实消耗；预扣 hold 保持到 Terminate 再统一释放。因此结算差额按
// cost − consumed − held 计算，不会把增量扣减重复计一次。
func (s *Session) ChargeIncrement(delta int64, fact contract.UsageFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if s.settled || s.refunded {
		return fmt.Errorf("%w: request %s", ErrSessionClosed, s.requestID)
	}
	if delta <= 0 {
		return nil
	}
	// fact 用于日志/审计上下文；增量金额以调用方传入的 delta 为准
	// （实时连接已按每个 response.done 完成本地计数/上游用量换算）。
	_ = fact

	ctx, cancel := opCtx()
	defer cancel()

	if _, err := s.engine.ledger.ApplyDelta(ctx, s.userID, -delta,
		"consume", "confirmed", "usage_log", s.requestID); err != nil {
		return err
	}
	s.consumed += delta
	return nil
}

// Terminate 终结会话并结算最终差额（多退少补，含少补）。
//
// 跨库顺序（裁决）：usage_logs 在独立日志库，**不存在跨库事务**。
// 固定顺序为：
//  1. 只读预检 hold 状态（worker 已退款的会话不再写成功日志）；
//  2. 构造 SettlementOutcome 并调用 Saturator.Saturate（按 request_id 幂等）；
//  3. 主库结算（关 hold + 差额流水，同一事务）。
//
// 若第 2 步失败 → 不结算，保留 hold，返回 error；若第 3 步失败 → 日志已写，
// hold 仍在，worker 兜底退款时把日志置 refunded（billing-06 §5.3 的兜底路径）。
func (s *Session) Terminate(fact contract.UsageFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if s.settled {
		return nil // 幂等
	}
	if s.refunded {
		// 已全额退款的会话不得再扣：早返回表在 refunded 上同样短路，
		// 否则失败路径的 defer Refund → Terminate 顺序会二次收费。
		return nil
	}

	ctx, cancel := opCtx()
	defer cancel()

	st, err := s.engine.ledger.openHoldState(ctx, s.requestID)
	if err != nil {
		return err
	}
	if st.Held == 0 && st.Closed {
		// 其它执行者（worker 兜底/并发终结）已经关闭 hold；不再写结算日志，
		// 避免把 refunded 事实覆盖成 success。
		s.finishLocked(ctx, true)
		return nil
	}

	userID := s.userID
	if userID == 0 {
		userID = st.UserID
	}

	snap, err := s.currentSnapshotLocked(ctx, fact.Model)
	if err != nil {
		return err
	}
	cost := s.actualCostLocked(snap, fact)

	preDeducted := s.preConsumed
	if preDeducted == 0 && st.Held > 0 {
		// 内存值缺失（例如会话被重建）时以数据库 hold 为准。
		preDeducted = st.Held
	}
	refunded := max(preDeducted+s.consumed-cost, 0)

	outcome := SettlementOutcome{
		Cost:        cost,
		PreDeducted: preDeducted,
		Refunded:    refunded,
		Status:      UsageLogStatusSuccess,
	}
	if s.engine.saturator != nil {
		if err := s.engine.saturator.Saturate(ctx, &fact, s.rc, outcome); err != nil {
			// 日志写失败：不结算；hold 留着由 worker 兜底退款。
			// 钱不能因为日志库抖动而永久冻结，也不能因为日志失败被回滚。
			return fmt.Errorf("billing: write usage log: %w", err)
		}
	}

	_, _, err = s.engine.ledger.settleHolds(ctx, userID, s.requestID, s.consumed, cost)
	if errors.Is(err, errHoldClosed) {
		s.finishLocked(ctx, true)
		return nil
	}
	if err != nil {
		// 主库结算失败：hold 保留，由 worker 退款并把日志置 refunded。
		// 这是两库一致性裁决下的既定失败面，不是跨库事务。
		return fmt.Errorf("billing: settle holds: %w", err)
	}

	s.finishLocked(ctx, false)
	return nil
}

// Refund 全额退回当前仍处于 hold 的预扣（幂等）。
//
// 已结算 / 已退款 / 无需退款 → nil（早返回表）。退款额来自数据库 hold 行，
// 不信任内存值：重复调用时 CAS 已无 hold 行，返回 errHoldClosed 后按幂等
// 命中处理。ChargeIncrement 已实际扣减的增量**不退**：那是真实消耗，
// 失败路径按中继的部分计费语义处理（billing-00 §9.3）。
func (s *Session) Refund() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if s.settled || s.refunded || !s.needsRefundLocked() {
		return nil
	}

	ctx, cancel := opCtx()
	defer cancel()

	_, _, err := s.engine.ledger.releaseHolds(ctx, s.userID, s.requestID, "released")
	if errors.Is(err, errHoldClosed) || errors.Is(err, errNoOpenHold) {
		// 已被其它执行者关闭：幂等命中，不再写第二行 release。
		s.refunded = true
		s.preConsumed = 0
		s.stopHeartbeatLocked()
		s.removeLeaseLocked(ctx)
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing: refund: %w", err)
	}

	s.refunded = true
	s.preConsumed = 0
	s.stopHeartbeatLocked()
	s.removeLeaseLocked(ctx)
	return nil
}

// Reserved 返回当前预留量（仍处于 hold 的金额）。
func (s *Session) Reserved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preConsumed
}

// NeedsRefund 判断调用方是否应走退款路径。
//
// fundingSettled 后必须为 false：资金已经按真实用量提交，再退会重复返还
// （billing-03 §2.3）。订阅在 tokenConsumed==0 但 preConsumed>0 时仍需退款，
// 该分支属 M5；钱包路径以 preConsumed>0 为准。
func (s *Session) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *Session) needsRefundLocked() bool {
	return !s.settled && !s.refunded && !s.fundingSettled && s.preConsumed > 0
}

// RecoverAbandoned 是崩溃兜底的会话入口（billing-00 §6.2 / billing-03 §2.6）。
//
// 按持久化状态复原：不依赖本会话内存，从 wallet_transactions 的 hold 行读取
// 用户与金额。幂等锚点是 `status='hold'` 的条件 UPDATE：重复扫描无副作用。
// T5 的 worker 会调用它（生产上可能通过重建的会话对象调用）。
func (s *Session) RecoverAbandoned(requestID string) error {
	if requestID == "" {
		requestID = s.requestID
	}
	if requestID == "" {
		return ErrMissingRequestID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if requestID == s.requestID && (s.settled || s.refunded) {
		return nil
	}

	ctx, cancel := opCtx()
	defer cancel()

	_, _, err := s.engine.ledger.releaseAbandoned(ctx, requestID)
	if errors.Is(err, errHoldClosed) || errors.Is(err, errNoOpenHold) {
		// 无 hold 或已被处理：无预扣可退，直接返回 nil。
		if requestID == s.requestID {
			s.refunded = true
			s.preConsumed = 0
			s.stopHeartbeatLocked()
			s.removeLeaseLocked(ctx)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing: recover abandoned %s: %w", requestID, err)
	}

	if requestID == s.requestID {
		s.refunded = true
		s.preConsumed = 0
		s.stopHeartbeatLocked()
		s.removeLeaseLocked(ctx)
	}
	return nil
}

// finishLocked 置终结状态并清理租约（调用方必须持有 s.mu）。
func (s *Session) finishLocked(ctx context.Context, refundedByOther bool) {
	s.settled = true
	s.fundingSettled = true
	s.preConsumed = 0
	if refundedByOther {
		s.refunded = true
	}
	s.stopHeartbeatLocked()
	s.removeLeaseLocked(ctx)
}

// actualCostLocked 按真实用量计算最终应扣总额。
//
// 失败行为（billing-03 §5.6 / billing-00 §7.5）：
//   - 表达式求值/编译失败 → **保留**已预扣与已增量扣减的额度（保守方向，
//     防止「表达式写错 → 免费白送」），不是退款；
//   - 完全无计费用量 → 配额 0，由多退少补释放 hold。
func (s *Session) actualCostLocked(snap *contract.PriceSnapshot, fact contract.UsageFact) int64 {
	if snap == nil {
		return 0
	}
	// 免费模型（PriceData.FreeModel 或 F-08-12 判定的零价模型）结算价为 0：
	// Terminate 全额退回按 max_tokens 预扣的 hold。
	if s.freeModel || snap.Price.FreeModel {
		return 0
	}
	usedVars, err := s.usedVarsForLocked(snap)
	if err != nil {
		return s.keepPreConsumedCostLocked()
	}
	if usageIsEmpty(fact.Usage) && !snap.Price.UsePrice && snap.Mode != contract.BillingModePerCall {
		// 无任何计费用量 → 配额 0（billing-00 §7.5）。但 ChargeIncrement 的
		// 增量是**实际消耗**：若终止事实为空而存在增量，不能把它们退掉，
		// 否则实时路径可「先增量扣、再空事实终结」套取退款。无增量时才为 0。
		return s.consumed
	}

	params := NormalizeTokens(fact.Usage, *snap, usedVars)
	if s.imageRequest {
		params.ImageCount = s.adoptImageCount(fact)
	}
	cost, err := pricing.ComputeQuota(*snap, params, usedVars)
	if clamp, ok := pricing.AsClamp(err); ok {
		// 结算路径的饱和是**审计事件**而不是失败：*Checked 的契约就是
		// 「用饱和值完成结算，同时把钳制标记落日志」（billing-00 §7.3）。
		// 首个非 nil 标记胜出，写入 rc.QuotaClamp（billing-07 §4.2）。
		s.noteQuotaClamp(clamp)
		return cost
	}
	if err != nil || cost < 0 {
		return s.keepPreConsumedCostLocked()
	}
	return cost
}

// noteQuotaClamp 记录首个钳制事件；日志层在写 usage_logs 前读取它
// （Saturator 的 Task 6 实现负责注入 other.admin_info.quota_saturation）。
//
// 只保留第一个非 nil 标记：一次结算可能发生多次饱和，但审计只要求
// 「发生过且可定位」，保留首因比覆盖成最后一个更有诊断价值。
func (s *Session) noteQuotaClamp(clamp *contract.QuotaClamp) {
	if clamp == nil || s.rc == nil || s.rc.QuotaClamp != nil {
		return
	}
	s.rc.QuotaClamp = clamp
}

// keepPreConsumedCostLocked 是求值失败的保守回退：保留预扣 + 已增量扣减；
// 两者都为 0 时取估算额。
func (s *Session) keepPreConsumedCostLocked() int64 {
	cost := s.preConsumed + s.consumed
	if cost == 0 {
		cost = s.estimatedQuota
	}
	return max(cost, 0)
}

// adoptImageCount 采信 UsageFact.ImageCount（billing-05 §2.2/§2.3）。
//
// reserved_default 表示上游零值/越界 → 保持请求预留量；否则结算域 [0,128]
// 内一律采信（0 合法且计 0，这是决议 B1 有意的乙类偏离）。
func (s *Session) adoptImageCount(fact contract.UsageFact) int {
	if fact.Usage.Source == contract.UsageReservedDefault {
		return s.reservedImages
	}
	if fact.ImageCount < 0 || fact.ImageCount > maxImageN {
		return s.reservedImages
	}
	return fact.ImageCount
}

// resolveSnapshotLocked 返回本次请求的定价快照。
//
// 优先使用 rc.PriceSnapshot（M2 已冻结/透传）；没有时调定价源 Resolve。
func (s *Session) resolveSnapshotLocked(ctx context.Context, est contract.QuotaEstimate) (*contract.PriceSnapshot, error) {
	if s.rc != nil && s.rc.PriceSnapshot != nil {
		return s.rc.PriceSnapshot, nil
	}
	if s.snap != nil {
		return s.snap, nil
	}
	if s.engine.source == nil {
		return nil, ErrNoPricing
	}
	model := est.Model
	if model == "" {
		model = s.model
	}
	group := est.Group
	if group == "" {
		group = s.group
	}
	snap, err := s.engine.source.Resolve(ctx, model, group)
	if err != nil {
		return nil, fmt.Errorf("billing: resolve pricing for model %q: %w", model, err)
	}
	s.snap = &snap
	return s.snap, nil
}

// currentSnapshotLocked 在结算时取最终快照。
//
// 跨组重试期间 relay 会刷新 rc.PriceSnapshot（新分组的倍率）；结算必须使用
// 最终选中分组的快照，而不是 PreConsume 时的旧快照。
func (s *Session) currentSnapshotLocked(ctx context.Context, fallbackModel string) (*contract.PriceSnapshot, error) {
	if s.rc != nil && s.rc.PriceSnapshot != nil {
		return s.rc.PriceSnapshot, nil
	}
	if s.snap != nil {
		return s.snap, nil
	}
	if s.engine.source == nil {
		return nil, ErrNoPricing
	}
	model := s.model
	if model == "" {
		model = fallbackModel
	}
	snap, err := s.engine.source.Resolve(ctx, model, s.group)
	if err != nil {
		return nil, fmt.Errorf("billing: resolve pricing for model %q: %w", model, err)
	}
	s.snap = &snap
	return s.snap, nil
}

// usedVarsForLocked 返回「已单独定价的子类变量」集合（contract.UsedVars 语义）。
//
// 三种来源：
//   - tiered 表达式：AST 内省结果（含函数名；归一化只关心 token 变量键）；
//   - 传统倍率：**合成全集**。ratio 公式把每个子类作为独立加项
//     （pricing.computeRatioQuota），所以它们都算「已单独定价」；若传 nil，
//     P/C 会保留子类，公式再加一次子类价 —— 同一批 token 被计两次。
//   - 按次（UsePrice/PerCall）：token 参数不参与金额，返回 nil。
//
// 缓存按 ExprHash/源码键复用，避免同一会话多次结算重复编译（billing-03 §4.11）。
func (s *Session) usedVarsForLocked(snap *contract.PriceSnapshot) (map[string]bool, error) {
	if snap == nil {
		return nil, nil
	}
	if snap.Mode == contract.BillingModeRatio && !snap.Price.UsePrice {
		return ratioPricedVars, nil
	}
	if snap.Mode != contract.BillingModeTieredExpr || snap.ExprString == "" {
		return nil, nil
	}
	key := snap.ExprHash
	if key == "" {
		key = snap.ExprString
	}
	if s.usedVarsKey == key {
		return s.usedVars, nil
	}
	ev, err := expr.Compile(snap.ExprString)
	if err != nil {
		return nil, fmt.Errorf("billing: compile pricing expression: %w", err)
	}
	s.usedVars = ev.UsedVars()
	s.usedVarsKey = key
	return s.usedVars, nil
}

// ratioPricedVars 是传统倍率公式中拥有独立加项的子类变量。
// 只读共享 map：任何调用方都不得修改它。
var ratioPricedVars = map[string]bool{
	"cr":     true,
	"cc":     true,
	"cc1h":   true,
	"img":    true,
	"img_cr": true,
	"ai":     true,
	"img_o":  true,
	"ao":     true,
}

// registerLeaseLocked 在真实 hold 写入后建立在飞租约（调用方持有 s.mu）。
//
// 失败时回滚全部 hold 并终结会话（fail-closed）：留下无心跳的冻结资金会让
// worker 无法判定；而回滚会写入一行 release，若允许同 requestID 重试再结算，
// 第二次 release 会撞 wallet_release_refid_uq 部分唯一索引。
func (s *Session) registerLeaseLocked(ctx context.Context) error {
	if s.engine.registry == nil || s.leaseRegistered {
		return nil
	}
	if err := s.engine.registry.Register(ctx, s.requestID, s.userID,
		time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		if _, _, rbErr := s.engine.ledger.releaseHolds(ctx, s.userID, s.requestID, "released"); rbErr != nil &&
			!errors.Is(rbErr, errHoldClosed) && !errors.Is(rbErr, errNoOpenHold) {
			return fmt.Errorf("billing: register lease: %v (rollback failed: %w)", err, rbErr)
		}
		s.preConsumed = 0
		s.refunded = true
		return fmt.Errorf("billing: register inflight lease: %w", err)
	}
	s.leaseRegistered = true
	s.startHeartbeatLocked()
	return nil
}

// startHeartbeatLocked 启动租约续约循环（调用方必须持有 s.mu）。
func (s *Session) startHeartbeatLocked() {
	if s.engine.registry == nil || s.leaseStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.leaseStop = stop
	s.leaseDone = done

	interval := s.engine.heartbeatInterval()
	requestID := s.requestID
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// 心跳必须比请求生命周期更长：冻结的接口没有 ctx，且请求取消不应
		// 终止续约（否则长请求会被 worker 误判为悬挂）。只有会话终结信号
		// 才停止心跳。
		base := context.WithoutCancel(context.Background())
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(base, billingOpTimeout)
				_ = s.engine.registry.Heartbeat(ctx, requestID)
				cancel()
			}
		}
	}()
}

// stopHeartbeatLocked 停止并等待续约循环退出（调用方必须持有 s.mu）。
func (s *Session) stopHeartbeatLocked() {
	if s.leaseStop != nil {
		close(s.leaseStop)
		s.leaseStop = nil
	}
	if s.leaseDone != nil {
		<-s.leaseDone
		s.leaseDone = nil
	}
}

// removeLeaseLocked 删除在飞租约（调用方必须持有 s.mu）。
func (s *Session) removeLeaseLocked(ctx context.Context) {
	if s.engine.registry == nil || !s.leaseRegistered {
		return
	}
	// 删除失败不影响资金一致性：租约 TTL 到期后 worker 扫描会看到 hold 行
	// 已关闭（settled/released/expired），RecoverAbandoned 是幂等的。
	_ = s.engine.registry.Remove(ctx, s.requestID)
	s.leaseRegistered = false
}

// estimateUsage 由 QuotaEstimate 构造估算用量。
//
// C 恒为 0：tiered token 预扣以 P=估算输入、C=0 求值（billing-03 §3.1）；
// 传统倍率也「仅估算输入，无输出 token 估算」（billing-03 §3.2 / §9.3 乙类
// 怪癖：输出全靠结算少补）。MaxTokens 保留进 Raw 供诊断，不进入预扣金额。
//
// PromptIncludesCache=true：估算没有上游格式事实，且各子类均为 0，两种分支
// 结果相同；显式取 true 避免 Claude 的 cr 加回路径被无意义地触发。
func estimateUsage(est contract.QuotaEstimate) contract.Usage {
	u := contract.Usage{
		Prompt:              max(est.PromptTokens, 0),
		PromptIncludesCache: true,
		AudioSeconds:        max(est.AudioSeconds, 0),
		Source:              contract.UsageEstimated,
	}
	if est.MaxTokens != nil || est.TaskDuration > 0 || est.FixedPriceKey != "" {
		raw := make(map[string]any, 3)
		if est.MaxTokens != nil {
			raw["max_tokens"] = *est.MaxTokens
		}
		if est.TaskDuration > 0 {
			raw["task_duration_seconds"] = est.TaskDuration
		}
		if est.FixedPriceKey != "" {
			raw["fixed_price_key"] = est.FixedPriceKey
		}
		u.Raw = raw
	}
	return u
}

// reservedImageCount 返回请求域图像张数（默认 1；第二段 n 作为回退）。
func reservedImageCount(est contract.QuotaEstimate) int {
	n := est.ImageCount
	if n <= 0 {
		n = est.AttemptImageCount
	}
	if n <= 0 {
		n = 1
	}
	return min(n, maxImageN)
}

// tokenExprVars 是表达式中的 token 计价变量（契约 §4.1 冻结集合）。
var tokenExprVars = []string{"p", "c", "len", "cr", "cc", "cc1h", "img", "img_cr", "ai", "img_o", "ao"}

// preConsumeMultiplierApplies 判断预扣上浮系数是否作用于本次定价
// （billing-03 §3.2：只放大传统倍率的输入费用与 tiered token 分支；
// tiered 固定价（按次/请求计价）不受影响，结算侧一律不受影响）。
func preConsumeMultiplierApplies(snap *contract.PriceSnapshot, usedVars map[string]bool) bool {
	if snap == nil || snap.Price.UsePrice || snap.Mode == contract.BillingModePerCall {
		return false
	}
	if snap.Mode == contract.BillingModeRatio {
		return true
	}
	for _, key := range tokenExprVars {
		if usedVars[key] {
			return true
		}
	}
	return false
}

// usageIsEmpty 判断是否完全没有计费用量（billing-00 §7.5：配额置 0）。
func usageIsEmpty(u contract.Usage) bool {
	return u.Prompt == 0 &&
		u.Completion == 0 &&
		u.CacheRead == 0 &&
		u.CacheWrite == 0 &&
		u.Reasoning == 0 &&
		u.AudioSeconds == 0 &&
		u.PromptDetail == (contract.UsageDetail{}) &&
		u.CompletionDetail == (contract.UsageDetail{})
}
