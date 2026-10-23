package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/billing/pricing"
	platformconfig "newtg/gateway/internal/config"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
	"newtg/gateway/internal/store"
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
	tokenID   int64 // rc.TokenID；0 表示无绑定令牌（跳过令牌额度）

	// requestTime 是本请求的**冻结时间**（spec §5.2）：创建会话时取一次，
	// 预扣、结算、跨组重试都传给定价源 ResolveAt。长请求跨整点时按请求开始
	// 时间计价，避免预扣与结算命中不同窗口。
	requestTime time.Time
	// frozenMultiplier 非 nil 表示本会话的结算来自异步/handoff 重建：
	// request_pricing 读回了预扣时命中的时段倍率，必须用 ResolveFixed
	// 原样复用，绝不再按当前时间重新匹配窗口。
	frozenMultiplier *float64

	snap        *contract.PriceSnapshot
	model       string
	group       string
	usedVars    map[string]bool
	usedVarsKey string // 已缓存的 usedVars 对应的表达式哈希/源码

	preConsumed    int64 // 当前仍处于 hold 的金额（数据库行是权威，内存用于早返回）
	consumed       int64 // ChargeIncrement 已实际扣减的累计
	tokenDeducted  int64 // 本会话已从 api_tokens.remain_quota 扣减的累计（R-73）
	estimatedQuota int64 // 预扣预算（求值失败时的保守回退值）
	reservedImages int   // 请求域图像张数（结算回退用）
	imageRequest   bool
	freeModel      bool
	trusted        bool

	// 资金来源是每请求冻结一次的（billing-04 §3.3）：预扣与退款必须复用
	// 同一选择，不能中途重新读取偏好。S4a 实现来源选择/预扣/退款；S4b 在
	// subscription_settlement.go 中实现结算 delta、增量与崩溃兜底。
	fundingSource           contract.FundingKind // 默认零值 = FundingWallet
	subscriptionID          int64                // 订阅来源冻结的实例（结算复用同一 ID）
	subscriptionPreConsumed int64                // 订阅来源冻结的预扣额
	fundingPlan             *fundingPlan         // 偏好 + 候选订阅的冻结快照
	// subscriptionReplayed 表示本次会话命中了订阅预扣的幂等记录（created=false）。
	// 这种会话没有自己的令牌扣减，也绝不能去退款：订阅记录由首个执行者持有，
	// 重复退款会把额度退给错误的请求（LOW）。
	subscriptionReplayed bool
	// billingPreferenceText 是任务/恢复路径回填的偏好文本（结算日志键）。
	// 这类会话没有 PreConsume 阶段的 fundingPlan，偏好由 handoff 在重建会话
	// 时从资金接缝读取后冻结在这里（LOW：任务成功日志也带 billing_preference）。
	billingPreferenceText string
	// subscriptionTerminal / terminalCheckAt 是订阅增量路径的本地终态缓存
	// （M2）：registry 的 settled/refunded 一旦观测到就永久拒绝新增量；
	// 未观测到终态时按 subscriptionTerminalCheckTTL 节流查询，避免每个
	// response.done 都打一次数据库。
	subscriptionTerminal bool
	terminalCheckAt      time.Time
	// settlementLockHeld 表示调用方（handoff/worker）已经持有本 requestID 的
	// 结算锁，会话内的订阅 Terminate/Refund 不得再次获取（Redis SETNX 不可
	// 重入，二次获取会必然返回 ErrSettlementInProgress）。
	settlementLockHeld bool

	// forcedCost 是 T5 的任务计价表达式求值结果（整数积分）。
	//
	// 非 nil 时 Terminate 跳过「按快照重算」：任务结算的价格已在提交期冻结
	// （P-08），结算只读快照，不得重新 Resolve 当前定价——重读会让任务运行
	// 期间的改价改变已承诺金额，且重建会话没有价格快照时会把成本算成 0。
	// 差额（多退少补）仍由既有的 hold CAS / settleHolds 完成。
	forcedCost *int64

	preConsumeCalled bool
	settled          bool
	refunded         bool
	fundingSettled   bool
	// detached 标记会话已交接给任务轮询器：心跳/租约已停止，hold 保留。
	// 它是幂等位而不是终态位——Detach 之后仍允许 Refund/Terminate 按
	// requestID 结算（账本行是唯一权威）。
	detached bool

	leaseRegistered bool
	leaseStop       chan struct{}
	leaseDone       chan struct{}

	// subLeaseStop/subLeaseDone 是订阅在飞租约（request_registry.in_flight）
	// 的续租循环（R-W9B-02）。与上面的 Redis 租约分开：钱包路径的租约由
	// Redis 承载（M3 既有行为，不得回归），订阅路径的租约必须落 PG 且与
	// 预扣记录同事务出现。
	subLeaseStop chan struct{}
	subLeaseDone chan struct{}

	// demoExemptSet/demoExemptValue 缓存 §3.17.2 的演示账号判定。判定结果在
	// 一次请求内不变（部署开关 + 令牌标志），缓存避免 Reserve/Increment/
	// Terminate 各查一次库。值为 true 时全会话零资金变动。
	demoExemptSet   bool
	demoExemptValue bool
}

// 编译期断言：Session 必须完整实现冻结的计费会话接口。
var _ contract.BillingSession = (*Session)(nil)

// Session 也是任务平台的异步交接实现：Detach 停止本会话的心跳；两个
// ByRequestID 转发给引擎（它们按持久化账本行重建，不需要原 Session）。
var _ contract.BillingHandoff = (*Session)(nil)

// opCtx 构造单次账本/定价操作的有界上下文（理由见 billingOpTimeout）。
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), billingOpTimeout)
}

// demoExempt 判定本会话是否命中 §3.17.2 的「演示账号不计入账单」口径。
//
// 存在两条互斥的身份路径，二者都只在**部署有效演示模式**下成立：
//   - 令牌身份（tokenID > 0）：令牌必须 unlimited_quota=true。两个条件缺一
//     不可——只认设置会把普通用户的 unlimited 令牌也免单（扩大豁免面），只认
//     令牌会在非演示站免单（改变既有语义）。
//   - 会话身份（tokenID == 0，即 /pg Playground）：同样免单（控制端 W13 裁决）。
//     /pg 复用 handleChatCompletions，但没有令牌，若只豁免令牌路径，演示
//     访客经 Playground 会真实扣演示账号余额或得到 402，留下设计未声明的
//     资损面。演示模式的写面已 deny-by-default、注册被拒，/pg 与 /v1/* 同属
//     「试用中继」，成本兜底是 §3.17.2 的两个日计数器；本函数只免除计费，
//     额度计数仍在 checkRelayRateLimit 里照常执行。
//
// 边界（不得放宽）：会话身份必须同时满足 rc.UsingGroup 非空这一**既有标记**。
// BuildRelayContext 对会话路径把 UsingGroup 留空，唯一会为会话身份回源并写入
// 分组的地方是 applyPlaygroundSession（/pg 的 chat 路径）。若只判断
// tokenID == 0，未来任何无令牌的会话平面（如任务重建会话）都会被静默免单；
// 因此豁免面严格限定为「演示模式 + /pg 会话身份」，不是「演示模式下所有请求」。
//
// 优先使用鉴权层投影（rc.TokenUnlimitedQuota，零额外查询）；任务/恢复路径
// 重建的会话没有投影时回落一次 DB 查询（仅演示模式）。
func (s *Session) demoExempt(ctx context.Context) bool {
	if s.demoExemptSet {
		return s.demoExemptValue
	}
	exempt := false
	if s.engine != nil && platformconfig.DemoEnabled(s.engine.cfg) {
		switch {
		case s.tokenID > 0:
			if s.rc != nil && s.rc.TokenUnlimitedQuota {
				exempt = true
			} else {
				exempt = s.engine.demoExemptToken(ctx, s.tokenID)
			}
		case s.rc != nil && s.rc.UsingGroup != "":
			// tokenID == 0 + UsingGroup 非空 = applyPlaygroundSession 写入的
			// /pg 会话身份（见上方边界说明）。
			exempt = true
		}
	}
	s.demoExemptSet, s.demoExemptValue = true, exempt
	return exempt
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
func (s *Session) PreConsume(est contract.QuotaEstimate) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// W10-METRICS：预扣与 402 拒绝是付费漏斗的入口口径。观察函数内部
	// recover，采样失败只丢样本；defer 只读会话字段，不改变任何业务路径。
	// model 在函数前段才从 est/rc 补齐，因此延迟到返回时取。
	defer func() {
		if err == nil {
			metrics.ObservePreConsume(s.model, s.group, s.preConsumed)
			return
		}
		if errors.Is(err, ErrInsufficientQuota) {
			model := s.model
			if model == "" {
				model = est.Model
			}
			metrics.ObserveInsufficientQuota(model)
		}
	}()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if s.settled || s.refunded {
		return fmt.Errorf("%w: request %s", ErrSessionClosed, s.requestID)
	}

	ctx, cancel := opCtx()
	defer cancel()

	// N1：已结案的 requestID 绝不能再预扣。重复会话/中继重放/T9 跨组
	// 「无会话→新建会话+PreConsume」都可能拿到已关闭的 requestID；若放行，
	// 新 hold 之后无法再写 release（wallet_release_refid_uq），退款会静默
	// 丢失。已提交但尚未结算的 hold 仍是 status='hold'，不是 terminal，
	// 因此「提交结果未知后的重试」不受影响。
	terminal, termErr := s.engine.ledger.requestTerminal(ctx, s.requestID)
	if termErr != nil {
		return fmt.Errorf("billing: check request terminal state: %w", termErr)
	}
	if terminal {
		s.markTerminalLocked(ctx)
		return fmt.Errorf("%w: %s", ErrRequestClosed, s.requestID)
	}

	// Minor-8（billing-03 §5.5）：预扣前已存在配额钳制 → 价格错误 400，
	// 不得继续用饱和后的估算值预扣（SkipRetry 由 T9 映射）。
	if s.rc != nil && s.rc.QuotaClamp != nil {
		return fmt.Errorf("%w: pre-existing quota clamp (op=%s)", ErrPriceError, s.rc.QuotaClamp.Op)
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
	// 预扣成功时冻结 request_time 与命中的时段倍率（spec §5.2）。放在 defer
	// 里覆盖全部分支（信任旁路、免费模型、余额不足前的零预算等）；
	// 失败只 WARN，不改变已成功的资金结果。
	defer func() {
		if succeeded {
			s.persistRequestPricing(ctx)
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
	s.reservedImages = estimateImageCount(est)

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

	// §3.17.2 的计费口径（控制端裁决 I1）必须在**所有**资金分支之前短路，
	// 包括下面的免费模型 max_tokens 预扣：后者在 hold > 0 时会调用
	// ApplyDeltaWithToken 真实冻结钱包（见 preConsumeFreeModelLocked）。若把
	// 这段判定放在免费模型分支之后，零价模型 + max_tokens 的演示请求仍会
	// 产生 hold/钱包流水，违背「演示账号不计入任何用户账单」。
	//
	// 这是演示账号专属口径，不是 unlimited_quota 的通用语义：非演示模式下
	// unlimited 令牌仍会扣 users.balance（令牌余额门另由 tokenUsable 判定）。
	if s.demoExempt(ctx) {
		// Trusted 只用于让既有的 Reserve 早返回；真正的资金跳过在本函数
		// 的提前返回 + Reserve/ChargeIncrement 的同类判定里。
		s.trusted = true
		succeeded = true
		return nil
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
		// M-2：估算期也会发生钳制（价格/上限配置错误），必须与结算期同口径
		// 记录，否则 A6 只能看到结算期的一半事件。
		if clamp, ok := pricing.AsClamp(err); ok {
			s.noteQuotaClamp(clamp)
		}
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

	// S4a：每请求解析并冻结资金来源（billing-04 §3.3）。信任旁路与来源
	// 选择正交——旁路只影响「扣多少」，不影响「从哪扣」；但订阅侧永不
	// 旁路（订阅是有限额账户，必须真实扣，billing-00 §6.4）。因此先选源：
	// 只有主来源是钱包时才允许命中 trusted；主来源是订阅时必须继续走
	// 真实扣减。
	plan, err := s.resolveFundingPlanLocked(ctx)
	if err != nil {
		return err
	}
	if plan.primaryWallet() {
		trusted, trustErr := s.engine.shouldTrust(ctx, s.rc, est, budget)
		if trustErr != nil {
			return trustErr
		}
		if trusted {
			s.trusted = true
			s.fundingSource = contract.FundingWallet
			succeeded = true
			return nil
		}
		if budget == 0 {
			// 估算为 0（无计费用量/零价）：不产生 hold。结算时若真实用量
			// 为正，走「少补」直接扣减（billing-00 §6.5：少补存在）。
			s.fundingSource = contract.FundingWallet
			succeeded = true
			return nil
		}
	}
	if err := s.chargeFundingLocked(ctx, plan, budget); err != nil {
		return err
	}
	succeeded = true
	return nil
}

// staticallyZeroPriced 判定本次请求是否属于「免费模型」。
//
// 【R-40 关键约束】免费判定只能来自**定价数据本身**，绝不能来自估算结果：
// 估算 P=0（CountToken 关闭、实时格式等）会让任何付费 Ratio 模型的
// ComputeQuota 返回 0；若据此判免费，结算会把真实用量按 0 收费（全站白送）。
//
// 分模式判定：
//   - FreeModel 标志：模型级零价事实，直接免费；
//   - 按次（UsePrice/PerCall）：单价 ModelPrice == 0 才免费；
//   - 传统倍率：所有**绝对价格**字段为 0 才免费（ModelRatio/CompletionRatio/
//     CacheRatio/CacheCreation*/5m/1h）。Image/Audio 是相对输入的乘数，
//     输入价为 0 时它们不产生费用；输入价 > 0 时已由上述字段判定为付费。
//   - tiered 表达式：只有「不引用任何 token 用量变量」且估算为 0 时，0 才与
//     用量无关、可判定为零价（如 fixed(0)）。引用 token 变量的表达式，
//     估算 0 只反映本次估算，不说明模型免费。
func staticallyZeroPriced(snap *contract.PriceSnapshot, usedVars map[string]bool, base int64) bool {
	if snap == nil {
		return false
	}
	if snap.Price.FreeModel {
		return true
	}
	if snap.Price.UsePrice {
		return snap.Price.ModelPrice == 0
	}
	switch snap.Mode {
	case contract.BillingModePerCall:
		return snap.Price.ModelPrice == 0
	case contract.BillingModeRatio:
		return !ratioHasPositivePrice(snap.Price)
	case contract.BillingModeTieredExpr:
		if base != 0 {
			return false
		}
		for _, key := range tokenExprVars {
			if usedVars[key] {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// ratioHasPositivePrice 判断传统倍率模式是否存在任何会产生费用的绝对价格。
//
// 与 pricing 的 pricePositive 同一口径（R9：Ratio 字段是「积分 / 1M tokens」
// 的绝对价格）：六个绝对价格全 0 才是真正的零价模型。ImageRatio/AudioRatio
// 是相对输入价的乘数，不作为独立的正价证据。
func ratioHasPositivePrice(p contract.PriceData) bool {
	return p.ModelRatio > 0 ||
		p.CompletionRatio > 0 ||
		p.CacheRatio > 0 ||
		p.CacheCreationRatio > 0 ||
		p.CacheCreation5mRatio > 0 ||
		p.CacheCreation1hRatio > 0
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

	// R-73：免费模型的 max_tokens 预扣同样走「令牌额度 + 钱包」同一事务。
	if _, err := s.engine.ledger.ApplyDeltaWithToken(ctx, s.userID, s.tokenID, -hold,
		"consume_pre", "hold", "usage_log", s.requestID); err != nil {
		return err // 余额/令牌额度不足 → *QuotaError（402）
	}
	s.preConsumed = hold
	s.tokenDeducted = hold
	return s.registerLeaseLocked(ctx)
}

// Reserve 仅上浮预留，不产生消耗（billing-00 §6.2 补遗 3）。
//
// 参数 delta 是**增量**（relay-04/06 的调用点是 Reserve(+Δ)）。delta ≤ 0
// 等价于「目标 ≤ 当前预留」的 no-op。上浮会真实冻结钱包余额（写第二行
// consume_pre+hold），否则图片两段预扣的「不允许欠费后继续提交」形同虚设
// （billing-03 §3.6 / billing-05 §2.4）。
//
// 【无预算上限（裁决）】会话不设 Reserve 总预算 ceiling：C-10/D12 的
// 「第二段 n 越界 = 整请求 400」是**中继侧**对 AttemptImageCount 的校验
// （QuotaEstimate.AttemptImageCount 的上界），会话只拿到换算后的 quota 增量，
// 无法反推 n。越界必须在 relay 归一化层拒绝，不能在此按金额猜测。
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
	if s.fundingSource == contract.FundingSubscription {
		// 订阅预扣是单实例额度，不支持上浮。实时用量差异由 ChargeIncrement
		// 直接追加订阅额度（S4b），最终差额由 Terminate 结算；这里若继续走
		// 钱包预留会形成双重计费：订阅已按预算真实扣款。
		return nil
	}
	if delta <= 0 {
		// 仅上浮：零/负增量不改变预留，也不应清 trusted（没有真实预留发生）。
		return nil
	}

	ctx, cancel := opCtx()
	defer cancel()

	// §3.17.2：演示账号不产生任何真实预留（连图片两段预扣也不例外）。
	// 放在 trusted 之前：trusted 对图片请求会主动清位并真实扣款。
	if s.demoExempt(ctx) {
		return nil
	}

	if s.trusted {
		if !s.imageRequest {
			return nil
		}
		// 图片请求必须先冻结资金再出站：清 trusted 后走真实预留。
		s.trusted = false
	}

	// Important-1：已结案（含 worker 已兜底退款）的请求绝不能再产生新 hold。
	// 新 hold 之后无法再写 release（wallet_release_refid_uq），会把 Refund
	// 永久卡在 23505。发现结案即把会话置终结并 fail closed。
	terminal, err := s.engine.ledger.requestTerminal(ctx, s.requestID)
	if err != nil {
		return fmt.Errorf("billing: check request terminal state: %w", err)
	}
	if terminal {
		s.markTerminalLocked(ctx)
		return fmt.Errorf("%w: %s", ErrRequestClosed, s.requestID)
	}

	// N3：跨组重试后必须按**当前分组**冻结快照；旧分组的倍率不得用于
	// 新增预留（否则 Reserve 冻结的金额与最终结算价不同源）。
	if err := s.refreshSnapshotForGroupLocked(ctx); err != nil {
		return err
	}

	if _, err := s.engine.ledger.ApplyDeltaWithToken(ctx, s.userID, s.tokenID, -delta,
		"consume_pre", "hold", "usage_log", s.requestID); err != nil {
		return err
	}
	s.preConsumed += delta
	s.tokenDeducted += delta

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

	// §3.17.2：演示账号的实时增量也不得变成真实消费（否则流式请求会在
	// response.done 时扣钱包）。金额仍由 Terminate 求值并写入 usage_logs。
	if s.demoExempt(ctx) {
		return nil
	}

	if s.fundingSource == contract.FundingSubscription {
		// S4b：实时增量立即结算到订阅账户并同步令牌额度。绝不能走钱包
		// 扣减：订阅来源没有 wallet hold 行，钱包扣款会成为第二笔费用。
		return s.chargeIncrementSubscriptionLocked(ctx, delta)
	}

	// Important-1：结案后不得再增量扣减（否则产生无法归属到 hold 的消费行）。
	terminal, err := s.engine.ledger.requestTerminal(ctx, s.requestID)
	if err != nil {
		return fmt.Errorf("billing: check request terminal state: %w", err)
	}
	if terminal {
		s.markTerminalLocked(ctx)
		return fmt.Errorf("%w: %s", ErrRequestClosed, s.requestID)
	}

	// N3：实时增量按当前分组的快照累计（跨组后旧快照不得继续使用）。
	if err := s.refreshSnapshotForGroupLocked(ctx); err != nil {
		return err
	}

	// R-73：增量扣减同样是「令牌额度 + 钱包」同一事务。
	if _, err := s.engine.ledger.ApplyDeltaWithToken(ctx, s.userID, s.tokenID, -delta,
		"consume", "confirmed", "usage_log", s.requestID); err != nil {
		return err
	}
	s.consumed += delta
	s.tokenDeducted += delta
	return nil
}

// Terminate 终结会话并结算最终差额（多退少补，含少补）。
//
// 跨库顺序（裁决）：usage_logs 在独立日志库，**不存在跨库事务**。
// 基础顺序为：
//  1. 只读预检 hold 状态（worker 已退款的会话不再写成功日志）；
//  2. 构造 SettlementOutcome 并调用 Saturator.Saturate（按 request_id 幂等）；
//  3. 主库结算（关 hold + 差额流水，同一事务）。
//
// 【两条 charge-first 例外】需要向钱包**补扣**时（无 hold 补扣、或成本
// 超出已预扣+已增量），顺序反转为「先主库扣款、后写日志」：否则扣款事务
// 回滚后 success 日志会永久按全价夸大收入（R-18 / T5 复核 I5）。此时
// 日志写失败不会再重试扣款，只上抛错误供补写。
//
// 其余方向的失败面：第 2 步失败 → 不结算，保留 hold，返回 error；第 3 步
// 失败 → 日志已写，hold 仍在，worker 兜底退款时把日志置 refunded
// （billing-06 §5.3 的兜底路径）。
func (s *Session) Terminate(fact contract.UsageFact) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// W10-METRICS：结算积分是营收口径（§5.4 点名的「计费金额」）。
	// settledCost 只在资金事务提交成功后赋值；观察发生在返回前，不影响
	// 任何 early-return/幂等语义。订阅来源的结算走独立函数，不在本 defer
	// 覆盖范围内（首批只统计钱包结算）。
	var settledCost int64
	defer func() {
		if err == nil && settledCost > 0 {
			metrics.ObserveSettle(s.model, s.group, billingModeName(s.snap), settledCost)
		}
	}()

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

	if s.fundingSource == contract.FundingSubscription {
		// S4b：订阅结算走独立路径（PostConsumeUserSubscriptionDelta + 令牌
		// 差额 + subscription_* 日志键）。绝不能落回下面的钱包结算路径：
		// 订阅来源没有 consume_pre hold，`noHoldSettlement` 会把实际成本
		// 再向钱包扣一次。
		return s.terminateSubscriptionLocked(ctx, fact)
	}

	st, err := s.engine.ledger.openHoldState(ctx, s.requestID)
	if err != nil {
		return err
	}
	if st.Held == 0 && st.Closed {
		// 其它执行者（worker 兜底/并发终结）已经关闭 hold；不再写结算日志，
		// 避免把 refunded 事实覆盖成 success。按关闭状态区分「已结算」与
		// 「已退款」，不要一律标成 refunded。
		s.finishLocked(ctx, st.ClosedStatus == "released" || st.ClosedStatus == "expired")
		return nil
	}

	userID := s.userID
	if userID == 0 {
		userID = st.UserID
	}
	// ByRequestID 重建的会话没有原始 rc.UserID；日志行必须记录真实归属，
	// 因此从账本行回填（rc 只承载 requestID 时 UserID 为 0）。
	if s.rc != nil && s.rc.UserID == 0 && userID != 0 {
		s.rc.UserID = int(userID)
	}

	// N3：结算必须使用最终选中分组的快照。跨组重试（relay.WithUsingGroup）
	// 会改写 rc.UsingGroup；若与预扣时冻结快照的分组不同，这里重新解析并
	// 覆盖 s.snap 与 rc.PriceSnapshot，旧组倍率绝不参与最终金额。
	//
	// T5：forcedCost 非 nil 时价格已在提交期冻结（任务计价表达式），不得
	// 重新解析——跳过整段快照刷新与定价求值，只做资金差额收敛。
	var cost int64
	if s.forcedCost != nil {
		cost = *s.forcedCost
	} else {
		if err := s.refreshSnapshotForGroupLocked(ctx); err != nil {
			return err
		}

		snap, err := s.currentSnapshotLocked(ctx, fact.Model)
		if err != nil {
			return err
		}
		// 上游格式事实（只读派生元信息）：Claude 语义 = 缓存读是独立维度。
		// usage_logs.other.usage_semantic 的生产写者（R-63 相邻的 T9 项）。
		s.noteUsageSemanticFromFactLocked(snap, fact.Usage)
		cost = s.actualCostLocked(snap, fact)
	}

	// §3.17.2 的计费口径（控制端裁决 I1）：演示账号的中继请求照常写
	// usage_logs（运维可见真实成本），但**不扣用户余额、不写钱包流水**。
	// 预扣阶段已被短路，所以正常路径没有 hold；这里仍做一次防御性收尾，
	// 避免部署开关在请求进行中被切换时留下悬挂 hold。
	if s.demoExempt(ctx) {
		return s.terminateDemoExemptLocked(ctx, fact, cost, st)
	}

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

	// I3：无 hold 且需要补扣时，这笔钱没有任何 worker 可回收的 hold 锚点。
	// 必须**先扣款、后写成功日志**：扣款失败（余额不足/DB 错误）时不得落
	// success 日志，否则日志会声称一笔从未收上来的费用已结算。
	// R-41：无 hold 的结算同时用 requestID 作用域 Redis 锁 + 事务内
	// settlementExists 复核，防止两个 Session 对象对同一请求双扣/双退。
	// 有 hold 的路径保持裁决顺序（先 Saturate、后主库结算），CAS 即锚点。
	noHoldSettlement := st.Held == 0 && !st.Closed
	if noHoldSettlement {
		releaseLock, lockErr := s.engine.acquireSettlementLock(ctx, s.requestID)
		if lockErr != nil {
			return lockErr
		}
		defer releaseLock()
	}

	noHoldCharge := noHoldSettlement && cost > s.consumed
	if noHoldCharge {
		if _, _, err := s.engine.ledger.settleHolds(ctx, userID, s.requestID, s.consumed, cost); err != nil {
			if errors.Is(err, errHoldClosed) {
				status, statErr := s.engine.ledger.closedHoldStatus(ctx, s.requestID)
				if statErr != nil {
					// Minor-7：无法确认结案状态时不得假定 settled（会掩盖退款事实）。
					return fmt.Errorf("billing: read closed hold status: %w", statErr)
				}
				s.finishLocked(ctx, status == "released" || status == "expired")
				return nil
			}
			return fmt.Errorf("billing: charge without hold: %w", err)
		}
		settledCost = cost
		s.consumed = cost
		// R-73：钱包已提交，随后把令牌额度调整到最终成本（失败只记日志）。
		s.adjustTokenToCostLocked(ctx, cost)
		if s.engine.saturator != nil {
			if err := s.engine.saturator.Saturate(ctx, &fact, s.rc, outcome); err != nil {
				// 钱已扣且无 hold 可退：保持 settled（重试会二次扣款），
				// 返回日志错误供上层记录/补写。绝不把已收的钱静默退回。
				s.finishLocked(ctx, false)
				return fmt.Errorf("billing: write usage log after charging: %w", err)
			}
		}
		s.finishLocked(ctx, false)
		return nil
	}

	// R-18：hold 不足以覆盖最终成本时，settleHolds 会额外向钱包补扣；补扣
	// 可能因余额不足/DB 故障整事务回滚。若此时先写了 success 日志，账单会
	// 永久按全价夸大收入、而钱并未收上来（且无告警）。因此「少补」路径与
	// noHoldCharge 同口径：**先资金、后日志**。退款/持平方向不需要额外
	// 扣款，保留「先日志后结算」的既有裁决（CAS 保证不会多退）。
	if cost > preDeducted+s.consumed {
		if _, _, err := s.engine.ledger.settleHolds(ctx, userID, s.requestID, s.consumed, cost); err != nil {
			if errors.Is(err, errHoldClosed) {
				status, statErr := s.engine.ledger.closedHoldStatus(ctx, s.requestID)
				if statErr != nil {
					return fmt.Errorf("billing: read closed hold status: %w", statErr)
				}
				s.finishLocked(ctx, status == "released" || status == "expired")
				return nil
			}
			// 补扣失败：hold 保留，不写 success 日志，由补偿扫描重试。
			return fmt.Errorf("billing: settle holds with extra charge: %w", err)
		}
		settledCost = cost
		s.consumed = cost
		// R-73：钱包已提交，随后把令牌额度调整到最终成本（失败只记日志）。
		s.adjustTokenToCostLocked(ctx, cost)
		if s.engine.saturator != nil {
			if err := s.engine.saturator.Saturate(ctx, &fact, s.rc, outcome); err != nil {
				// 钱已扣且 hold 已关：重试会二次扣款，保持 settled 并上抛日志错误。
				s.finishLocked(ctx, false)
				return fmt.Errorf("billing: write usage log after charging: %w", err)
			}
		}
		s.finishLocked(ctx, false)
		return nil
	}

	// T5 复核 I5 的补扣分支已在上面处理（先资金、后日志）；这里只剩
	// 「退款/持平方向」的既有顺序：先写日志、后主库结算（CAS 是幂等锚点）。
	if s.engine.saturator != nil {
		if err := s.engine.saturator.Saturate(ctx, &fact, s.rc, outcome); err != nil {
			// 日志写失败：不结算；hold 留着由 worker 兜底退款。
			// 钱不能因为日志库抖动而永久冻结，也不能因为日志失败被回滚。
			return fmt.Errorf("billing: write usage log: %w", err)
		}
	}

	_, _, err = s.engine.ledger.settleHolds(ctx, userID, s.requestID, s.consumed, cost)
	if errors.Is(err, errHoldClosed) {
		status, statErr := s.engine.ledger.closedHoldStatus(ctx, s.requestID)
		if statErr != nil {
			// Minor-7：无法确认结案状态时不得假定 settled（会掩盖退款事实）。
			return fmt.Errorf("billing: read closed hold status: %w", statErr)
		}
		s.finishLocked(ctx, status == "released" || status == "expired")
		return nil
	}
	if err != nil {
		// 主库结算失败：hold 保留，由 worker 退款并把日志置 refunded。
		// 这是两库一致性裁决下的既定失败面，不是跨库事务。
		return fmt.Errorf("billing: settle holds: %w", err)
	}
	settledCost = cost

	// R-73：资金先提交，再调整令牌额度（billing-03 §2.5/§5.7）。
	s.adjustTokenToCostLocked(ctx, cost)
	s.finishLocked(ctx, false)
	return nil
}

// terminateDemoExemptLocked 完成演示账号的「只记账、不动钱」结算。
//
// 与正常 Terminate 的差异只有资金段：不调 settleHolds、不调
// adjustTokenToCost、不产生 settled 营收指标。usage_logs 仍按真实成本写入
// （§3.17.2「照常写日志，便于运维看成本」），outcome 的 pre_deducted/
// refunded 恒为 0。
func (s *Session) terminateDemoExemptLocked(
	ctx context.Context, fact contract.UsageFact, cost int64, st holdState,
) error {
	// 防御：若历史/开关切换留下了 hold，必须释放，不能悬挂。正常路径
	// PreConsume 已短路，st.Held == 0。
	if st.Held > 0 {
		if _, _, err := s.engine.ledger.releaseHolds(
			ctx, s.userID, s.tokenID, s.requestID, "released"); err != nil &&
			!errors.Is(err, errNoOpenHold) {
			return fmt.Errorf("billing: release demo hold %s: %w", s.requestID, err)
		}
	}
	if st.Closed {
		// 已有执行者关闭了 hold：不覆盖退款事实，按幂等命中处理。
		s.finishLocked(ctx, st.ClosedStatus == "released" || st.ClosedStatus == "expired")
		return nil
	}
	outcome := SettlementOutcome{
		Cost:        cost,
		PreDeducted: 0,
		Refunded:    0,
		Status:      UsageLogStatusSuccess,
	}
	if s.engine.saturator != nil {
		if err := s.engine.saturator.Saturate(ctx, &fact, s.rc, outcome); err != nil {
			// 日志写失败不改变「没有资金动作」这一事实；直接上抛供上层记录。
			return fmt.Errorf("billing: write usage log for demo exempt request: %w", err)
		}
	}
	s.finishLocked(ctx, false)
	return nil
}

// Refund 全额退回当前会话的预扣与增量扣减（幂等）。
//
// 「全额」包括两部分（billing-00 §6.2）：
//   - 仍处于 hold 的预扣；
//   - ChargeIncrement 已实际扣减的增量（连接级真实扣费）。
//
// 已结算 / 已退款 / 无需退款 → nil（早返回表）。退款额来自数据库 hold 行
// 加会话累计的 consumed（后者在同事务内钳制到数据库已扣减额，见
// refundableConsumed），合并为一行 release。重复调用时：有 hold → CAS 未
// 命中；无 hold → 命中共用的 billing:* 结案标记（settlementExists）；
// 最后 release 唯一索引兜底并发窗口。三者都按幂等命中处理。
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

	if s.fundingSource == contract.FundingSubscription {
		return s.refundSubscriptionLocked(ctx)
	}

	if s.preConsumed == 0 {
		// 无 hold 锚点（如 trusted 会话只有 ChargeIncrement 增量）：
		// refundSession 以结案标记复核，再用 requestID 锁收窄并发窗口（R-41）。
		releaseLock, lockErr := s.engine.acquireSettlementLock(ctx, s.requestID)
		if lockErr != nil {
			return lockErr
		}
		defer releaseLock()
	}

	// ND1：日志里的 pre_deducted 只应记录**预扣 hold**，不能把
	// ChargeIncrement 实耗算进去（trusted/无 hold 会话没有预扣）。
	preDeductedForLog := s.preConsumed
	// R-W9B-03：钱包退款与令牌回补在 refundSession 的同一事务内。失败时
	// 整笔回滚（hold 仍开放），调用方可以完整重放，绝不会「钱退了、令牌没退」。
	refundedAmount, _, err := s.engine.ledger.refundSession(
		ctx, s.userID, s.tokenID, s.requestID, s.consumed)
	if errors.Is(err, errHoldClosed) || errors.Is(err, errNoOpenHold) {
		// 已被其它执行者关闭：幂等命中，不再写第二行 release。
		// 也不回补令牌/写退款日志：结案方已经按它自己的路径处理过。
		s.refunded = true
		s.preConsumed = 0
		s.consumed = 0
		s.tokenDeducted = 0
		s.stopHeartbeatLocked()
		s.removeLeaseLocked(ctx)
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing: refund: %w", err)
	}
	s.tokenDeducted = 0

	// Minor-3：Refund 必须把已写出的终态 usage_logs 行收敛为 refunded
	// （T6 的 Saturator 按 request_id 幂等更新）。钱已经退回；日志失败
	// 返回错误供上层告警，但会话仍标记 refunded（重试不会重复退款）。
	if s.engine.saturator != nil {
		outcome := SettlementOutcome{
			Cost:        0,
			PreDeducted: preDeductedForLog,
			Refunded:    refundedAmount,
			Status:      UsageLogStatusRefunded,
		}
		if logErr := s.engine.saturator.Saturate(ctx, nil, s.rc, outcome); logErr != nil {
			s.refunded = true
			s.preConsumed = 0
			s.consumed = 0
			s.stopHeartbeatLocked()
			s.removeLeaseLocked(ctx)
			return fmt.Errorf("billing: write refund log: %w", logErr)
		}
	}

	// W10-METRICS：同步退款按金额计入 billing_refund_credits_total。
	// 放在终态置位之前：即便下一步的状态写失败，资金已经退出，指标应保留。
	metrics.ObserveRefund("sync", refundedAmount)

	s.refunded = true
	s.preConsumed = 0
	s.consumed = 0
	s.stopHeartbeatLocked()
	s.removeLeaseLocked(ctx)
	return nil
}

// refundSubscriptionLocked 全额退回订阅来源的预扣（billing-04 §4）。
//
// 与钱包路径的非对称点是**刻意**的（重写高危区）：
//   - 订阅退款按 requestId 幂等、最多 3 次、退避 200/400ms；
//   - 钱包退款非幂等、不重试；
//   - 订阅预扣没有 wallet hold 行，因此不得走 releaseHolds，否则会把
//     不属于本次请求的钱包流水当成 hold 处理；
//   - R-W9B-03：增量回退、预扣退款、amount_used 回退、令牌回补与 registry
//     终态在存储层的**同一个事务**内（RefundSubscriptionRequest）。
func (s *Session) refundSubscriptionLocked(ctx context.Context) error {
	preDeductedForLog := s.preConsumed
	// S4b：ChargeIncrement 立即落账的增量也必须随全额退款回退。否则请求
	// 失败后订阅 amount_used 会留下「已交付但已退款」的消耗，用户额度被
	// 永久占用；钱包路径的 refundSession 同样把 consumed 一并退回。
	consumedRefunded := s.consumed

	// 幂等回放会话不得退款：订阅记录属于首执行者（LOW）。
	if s.subscriptionReplayed {
		s.refunded = true
		s.preConsumed = 0
		s.consumed = 0
		s.subscriptionPreConsumed = 0
		s.stopHeartbeatLocked()
		s.removeLeaseLocked(ctx)
		return nil
	}

	// M1：与 Terminate/崩溃兜底共用 requestID 结算锁，锁内复核终态。
	// handoff 已预取锁时 settlementLockHeld=true，这里不会二次获取。
	releaseLock, lockErr := s.ensureSettlementLockLocked(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer releaseLock()
	state, stateErr := s.engine.subscriptionFunding.RequestRegistryState(ctx, s.requestID)
	if stateErr != nil {
		return fmt.Errorf("billing: read subscription refund state for %s: %w", s.requestID, stateErr)
	}
	switch state {
	case "settled":
		// 结算已收尾：失败回调不得再退款（终态互斥）。
		s.finishLocked(ctx, false)
		return nil
	case "refunded":
		s.finishLocked(ctx, true)
		return nil
	}

	// R-W9B-03：增量回退、预扣退款、令牌回补、registry 终态在存储层是**同一个
	// 事务**。任一步失败整笔回滚（amount_used / 预扣 / 令牌都保持原状），可
	// 完整重放；不会再出现「预扣退了、令牌没补」或「令牌补了、registry 没写」。
	result, err := s.engine.refundSubscriptionRequest(ctx, store.SubscriptionRefund{
		RequestID:   s.requestID,
		UserID:      s.userID,
		TokenID:     s.tokenID,
		TokenAmount: s.tokenDeducted,
		Consumed:    consumedRefunded,
	})
	if err != nil {
		return fmt.Errorf("billing: refund subscription request: %w", err)
	}
	if !result.Applied {
		// 幂等命中：预扣已 refunded 或 registry 已 settled（另一执行者先收尾）。
		// 事务没有改动任何资金状态，按已有终态结束会话即可。
		if result.Terminal == "refunded" {
			s.finishLocked(ctx, true)
		} else {
			s.finishLocked(ctx, false)
		}
		return nil
	}
	if consumedRefunded > 0 && !result.IncrementReversed {
		// M3：周期重置后 `amount_used >= consumed` 守卫不命中，增量回退是
		// 方向安全的 no-op；预扣退款仍然完成。
		slog.Warn("billing: subscription increment refund skipped; amount_used no longer covers it (period reset or concurrent reversal)",
			"request_id", s.requestID, "subscription_id", s.subscriptionID,
			"increment_deducted", consumedRefunded)
	}
	// 生产适配器会把令牌回补并入退款事务；非事务适配器（测试替身）返回
	// TokenCredited=false，这里退化为 best-effort，保持与旧行为一致。
	if !result.TokenCredited && s.tokenDeducted > 0 {
		if tokenErr := s.engine.ledger.CreditToken(ctx, s.userID, s.tokenID, s.tokenDeducted); tokenErr != nil {
			slog.Warn("billing: token quota restore failed after subscription refund",
				"request_id", s.requestID, "token_id", s.tokenID,
				"amount", s.tokenDeducted, "error", tokenErr)
		}
	}
	s.consumed = 0
	s.subscriptionPreConsumed = 0
	s.tokenDeducted = 0
	// W10-METRICS：订阅路径的退款同样计入 billing_refund_credits_total；
	// reason=sync 表示请求内同步释放（worker 兜底是另一条路径）。
	metrics.ObserveRefund("sync", preDeductedForLog+consumedRefunded)

	if s.engine.saturator != nil {
		outcome := SettlementOutcome{
			Cost:          0,
			PreDeducted:   preDeductedForLog,
			Refunded:      preDeductedForLog + consumedRefunded,
			Status:        UsageLogStatusRefunded,
			FundingSource: contract.FundingSubscription,
		}
		if logErr := s.engine.saturator.Saturate(ctx, nil, s.rc, outcome); logErr != nil {
			s.refunded = true
			s.preConsumed = 0
			s.consumed = 0
			s.subscriptionPreConsumed = 0
			s.stopHeartbeatLocked()
			s.removeLeaseLocked(ctx)
			return fmt.Errorf("billing: write subscription refund log: %w", logErr)
		}
	}

	// S4b：记录退款终态，供兜底扫描/迟到的任务回调排除。失败只告警：
	// 预扣记录已是 refunded，扫描同样会跳过；不影响资金正确性。
	if err := s.engine.subscriptionFunding.MarkRequestRefunded(ctx, s.requestID, s.userID); err != nil {
		slog.Warn("billing: record subscription refund state failed",
			"request_id", s.requestID, "error", err)
	}

	s.refunded = true
	s.preConsumed = 0
	s.consumed = 0
	s.subscriptionPreConsumed = 0
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

// Detach 把会话交接给任务轮询器（contract.BillingHandoff）。
//
// 只做两件事：停止心跳、移除在飞租约索引。**保留 hold 不结算**——任务仍在
// 上游运行，钱的冻结状态就是它的预算。之后由 T6b 用持久化的 requestID 调
// TerminateByRequestID / RefundByRequestID 收敛。
//
// 为什么不删 hold：删了就相当于结算/退款；任务终态前任何收敛都是错的。
// 为什么移除租约：租约是「请求进程是否还活着」的心跳，任务已交给轮询器，
// 原进程不再是它的守护者；保留租约会让 worker 永远不回收真正的悬挂任务。
//
// 幂等：已 detach/结算/退款都返回 nil（调用方可以安全重试）。
func (s *Session) Detach(requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.requestID == "" {
		return ErrMissingRequestID
	}
	if requestID != "" && requestID != s.requestID {
		// 交接错 ID 会停掉别的请求的心跳，直接拒绝而不是「尽力而为」。
		return fmt.Errorf("%w: detach %s for session %s", ErrRequestClosed, requestID, s.requestID)
	}
	if s.detached || s.settled || s.refunded {
		return nil
	}
	ctx, cancel := opCtx()
	defer cancel()
	s.stopHeartbeatLocked()
	s.removeLeaseLocked(ctx)
	s.detached = true
	return nil
}

// TerminateByRequestID 按持久化 requestID 结算（T6b 轮询器入口）。
// 转发给引擎：没有进程内 Session 时账本是唯一权威。
//
// 必须带上本会话的 tokenID：inline 任务的成功终结同样要归属到发起令牌
// （T6b 复核 H1）；轮询器另有 token-aware 入口直接从任务行取 token_id。
func (s *Session) TerminateByRequestID(requestID string, fact contract.UsageFact) error {
	return s.engine.TerminateByRequestIDForToken(requestID, s.tokenID, fact)
}

// TerminateByRequestIDCost 按**已求值**的任务成本结算（T5 即时终态路径）。
//
// 与 TerminateByRequestID 的差别只在「成本从哪来」：任务计价表达式的
// 结果是积分，结算时只读提交期冻结的成本，不再按模型快照重算。转发给
// 引擎后仍复用同一条 hold CAS / 结算锁，因此与轮询路径的幂等口径一致。
func (s *Session) TerminateByRequestIDCost(requestID string, cost int64, fact contract.UsageFact) error {
	return s.engine.TerminateByRequestIDForTokenCost(requestID, s.tokenID, cost, fact)
}

// RefundByRequestID 按持久化 requestID 全额退款（T6b 失败/超时入口）。
// 同上：带上 tokenID 才能回补 PreConsume 扣掉的令牌额度。
func (s *Session) RefundByRequestID(requestID string) error {
	return s.engine.RefundByRequestIDForToken(requestID, s.tokenID)
}

// NeedsRefund 判断调用方是否应走退款路径。
//
// fundingSettled 后必须为 false：资金已经按真实用量提交，再退会重复返还
// （billing-03 §2.3）。需要退款的情形是「仍有 hold」或「有增量扣减」
// （ChargeIncrement 的实际消耗；Refund 会连同 hold 一起全额退回）。
// 订阅在 tokenConsumed==0 但 preConsumed>0 时仍需退款，该分支属 M5。
func (s *Session) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *Session) needsRefundLocked() bool {
	return !s.settled && !s.refunded && !s.fundingSettled &&
		(s.preConsumed > 0 || s.consumed > 0)
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

	// 只有恢复**本会话**的请求时才带上本会话的令牌身份；恢复他人请求时
	// tokenID=0，避免把额度补到错误的令牌上（worker 走 Engine 入口并携带
	// 租约里的正确 tokenID）。
	tokenID := int64(0)
	if requestID == s.requestID {
		tokenID = s.tokenID
	}
	_, _, err := s.engine.ledger.releaseAbandoned(ctx, requestID, tokenID)
	if errors.Is(err, errHoldClosed) || errors.Is(err, errNoOpenHold) {
		// 无 hold 或已被处理：无预扣可退，直接返回 nil。
		if requestID == s.requestID {
			s.refunded = true
			s.preConsumed = 0
			s.tokenDeducted = 0
			s.stopHeartbeatLocked()
			s.removeLeaseLocked(ctx)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing: recover abandoned %s: %w", requestID, err)
	}

	if requestID == s.requestID {
		// ND2：hold 退回与令牌回补已在 releaseAbandoned 的同一事务内完成
		// （只回补 hold 金额，增量保持已扣，R-39）。
		s.refunded = true
		s.preConsumed = 0
		s.tokenDeducted = 0
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

// markTerminalLocked 把会话置为「请求已在他处结案」的终结态（Important-1）。
//
// 不置 refunded：结案可能是结算而非退款，错误标记会让日志/审计误判。
// settled+fundingSettled 已足以让 Terminate/Refund/NeedsRefund 全部早返回。
func (s *Session) markTerminalLocked(ctx context.Context) {
	s.settled = true
	s.fundingSettled = true
	s.preConsumed = 0
	s.tokenDeducted = 0
	s.stopHeartbeatLocked()
	s.removeLeaseLocked(ctx)
}

// ensureSettlementLockLocked 确保本会话持有 requestID 结算锁（调用方持有 s.mu）。
//
// 订阅来源没有 wallet hold 行可做 CAS 锚点，结算/退款/崩溃兜底必须共用同一把
// requestID 锁串行化（M1）：否则实时连接 Terminate 与恢复扫描可以在「扫描已
// 判定未结算」之后交错落地，出现「已退款又结算」或「已结算又退款」。
//
// 已持有（handoff/worker 预取）时返回 no-op：Redis SETNX 不可重入，重复获取
// 会必然失败。no-op 释放不会清 settlementLockHeld，由预取方负责释放。
func (s *Session) ensureSettlementLockLocked(ctx context.Context) (func(), error) {
	if s.settlementLockHeld {
		return func() {}, nil
	}
	release, err := s.engine.acquireSettlementLock(ctx, s.requestID)
	if err != nil {
		return nil, err
	}
	s.settlementLockHeld = true
	return func() {
		s.settlementLockHeld = false
		release()
	}, nil
}

// adjustTokenToCostLocked 把令牌额度调整到最终结算成本（R-73）。
//
// 顺序契约（billing-03 §2.5/§5.7）：钱包资金已提交后才调整令牌；令牌调整
// 失败**只记日志**，不回滚资金，也不改变会话已 settled 的事实。
// adjustment = cost − tokenDeducted：>0 表示少补（再扣令牌），<0 表示多退
// （回补令牌）；符号与 AdjustToken 的约定一致。
func (s *Session) adjustTokenToCostLocked(ctx context.Context, cost int64) {
	adjustment := cost - s.tokenDeducted
	if adjustment == 0 {
		return
	}
	if err := s.engine.ledger.AdjustToken(ctx, s.userID, s.tokenID, adjustment); err != nil {
		slog.Warn("billing: token quota adjustment failed after wallet settlement",
			"request_id", s.requestID, "token_id", s.tokenID,
			"adjustment", adjustment, "error", err)
		return
	}
	s.tokenDeducted = cost
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
	// 只有模型级 FreeModel 事实在结算时直接短路为 0。**不信任预扣阶段的
	// s.freeModel 分类**（R-40）：若估算为 0 导致误判，结算必须仍按真实
	// 用量与真实价格求值；真正零价的模型 ComputeQuota 自身会返回 0
	// （Ratio 全零价 / PerCall 单价 0 / 恒零表达式）。
	if snap.Price.FreeModel {
		return 0
	}
	usedVars, err := s.usedVarsForLocked(snap)
	if err != nil {
		return s.keepPreConsumedCostLocked()
	}
	// 即使 Usage 为空也必须走表达式求值（C1）：`image_count * 10000`、常量
	// 表达式、按次价等「与 usage 无关」的计费项仍可能产生费用。空 usage 的
	// 零价判定由 ComputeQuota 自己完成（billing-00 §7.5 的「无计费用量→0」
	// 是它的职责），会话层不得用提前返回绕过定价。
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
	// W10-METRICS：只记首个钳制事件（与审计口径一致）。op/kind 都是冻结
	// 枚举，基数有界；这是 A6「钳制 > 0 即告警」的唯一数据源。
	metrics.ObserveQuotaClamp(clamp.Op, clampKindName(clamp.Kind))
}

// billingModeName 把契约枚举映射为指标标签（platform-05 §3.2.4 冻结取值）。
func billingModeName(snap *contract.PriceSnapshot) string {
	if snap == nil {
		return "unknown"
	}
	switch snap.Mode {
	case contract.BillingModePerCall:
		return "per_request"
	case contract.BillingModeTieredExpr:
		return "tiered_expr"
	case contract.BillingModeRatio:
		return "ratio"
	default:
		return "unknown"
	}
}

// clampKindName 把 ClampKind 映射为指标标签（overflow/underflow/nan）。
func clampKindName(kind contract.ClampKind) string {
	switch kind {
	case contract.ClampUnderflow:
		return "underflow"
	case contract.ClampNaN:
		return "nan"
	default:
		return "overflow"
	}
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

// resolveAt 用会话冻结的请求时间解析定价（spec §5.2）。
//
// 三条路径共用本函数：预扣（resolveSnapshotLocked）、结算
// （currentSnapshotLocked）、跨组重试（refreshSnapshotForGroupLocked）。
// 定价源实现 pricing.TimeSource 时传 requestTime，否则退回 Resolve（该源
// 没有时段概念，行为与旧版一致）。
//
// frozenMultiplier 非 nil 表示会话由 request_pricing 重建：必须用
// ResolveFixed 复用预扣时的倍率，不能把 requestTime 再交给 ResolveAt
// 重新匹配（策略可能在预扣后被修改，节假日缓存也可能已刷新）。
func (s *Session) resolveAt(ctx context.Context, model, group string) (contract.PriceSnapshot, error) {
	src := s.engine.source
	if s.frozenMultiplier != nil {
		if ts, ok := src.(pricing.TimeSource); ok {
			return ts.ResolveFixed(ctx, model, group, *s.frozenMultiplier)
		}
		// 定价源不支持时段冻结：按当前定价继续，但必须告警——这是配置
		// 错误（装配了 request_pricing 却没有时段感知的定价源）。
		slog.Warn("billing: pricing source cannot apply frozen time multiplier; using current pricing",
			"request_id", s.requestID)
	}
	if ts, ok := src.(pricing.TimeSource); ok {
		at := s.requestTime
		if at.IsZero() {
			// 手工构造的会话（测试/handoff 分支）没有冻结时间：用当前时间
			// 只用于首次解析，结算路径由 Session 快照冻结后续值。
			at = time.Now()
		}
		return ts.ResolveAt(ctx, model, group, at)
	}
	return src.Resolve(ctx, model, group)
}

// persistRequestPricing 写入本请求的冻结计价事实（spec §5.2）。
//
// 【失败方向】写失败只 WARN 不阻断请求：预扣的资金已经提交，回滚预扣
// 换不来正确性；同步结算仍用内存快照。代价是异步 handoff 结算读不到记录，
// 会退回既有行为并再次 WARN（见 Engine.loadFrozenPricing）。
//
// 【非 UUID 的 requestID】生产 RequestID 是 32 位十六进制；内部测试/演示
// 可能用 "req-xxx"。这类 id 无法写进 uuid 主键，直接跳过而不是让每次
// 预扣都失败。
func (s *Session) persistRequestPricing(ctx context.Context) {
	if s.engine == nil || s.engine.requestPricing == nil || s.requestID == "" {
		return
	}
	if !store.ValidRequestPricingID(s.requestID) {
		return
	}
	multiplier := 1.0
	window := ""
	if s.snap != nil {
		if s.snap.TimeMultiplier > 0 {
			multiplier = s.snap.TimeMultiplier
		}
		window = s.snap.TimeWindow
	}
	requestTime := s.requestTime
	if requestTime.IsZero() {
		requestTime = time.Now()
	}
	rec := store.RequestPricing{
		RequestID:      s.requestID,
		RequestTime:    requestTime,
		TimeMultiplier: multiplier,
		TimeWindow:     window,
	}
	if err := s.engine.requestPricing.Upsert(ctx, rec); err != nil {
		slog.Warn("billing: persist frozen request pricing failed; async settlement will fall back",
			"request_id", s.requestID, "error", err)
	}
}

// resolveSnapshotLocked 返回本次请求的定价快照。
//
// 优先使用 rc.PriceSnapshot（M2 已冻结/透传）；没有时调定价源 Resolve。
// 两条路径都把快照记到 s.snap，并在快照上补写上游格式事实
// （IsClaudeUsageSemantic，见 inheritUsageSemanticLocked）。
func (s *Session) resolveSnapshotLocked(ctx context.Context, est contract.QuotaEstimate) (*contract.PriceSnapshot, error) {
	if s.rc != nil && s.rc.PriceSnapshot != nil {
		if err := requirePerCallSnapshot(est.UsePrice, s.rc.PriceSnapshot); err != nil {
			return nil, err
		}
		s.snap = s.rc.PriceSnapshot
		s.inheritUsageSemanticLocked(s.snap)
		return s.snap, nil
	}
	if s.snap != nil {
		if err := requirePerCallSnapshot(est.UsePrice, s.snap); err != nil {
			return nil, err
		}
		s.inheritUsageSemanticLocked(s.snap)
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
	// 【W8/MJ-d · UsePrice 严格解析】固定价任务族（MJ）以 UsePrice 标记
	// 「本次请求只能按配置的按次价计费」。通用 Resolve 对未知模型回落默认
	// 倍率价，配合空 token 用量会估算成 0（静默免费）——这是失败方向错误。
	// 因此 UsePrice 一律走严格解析：没有启用的 mode=1 行（或分组覆盖价）
	// 就返回 ErrNoPerCallPrice，由调用方 fail-closed。
	if est.UsePrice {
		strict, ok := s.engine.source.(pricing.PerCallSource)
		if !ok {
			return nil, fmt.Errorf("%w: 定价源不支持严格按次价", pricing.ErrNoPerCallPrice)
		}
		snap, err := strict.ResolvePerCall(ctx, model, group)
		if err != nil {
			return nil, fmt.Errorf("billing: resolve per-call pricing for model %q: %w", model, err)
		}
		s.snap = &snap
		s.inheritUsageSemanticLocked(s.snap)
		s.adoptSnapshotLocked(s.snap)
		return s.snap, nil
	}
	snap, err := s.resolveAt(ctx, model, group)
	if err != nil {
		return nil, fmt.Errorf("billing: resolve pricing for model %q: %w", model, err)
	}
	s.snap = &snap
	s.inheritUsageSemanticLocked(s.snap)
	s.adoptSnapshotLocked(s.snap)
	return s.snap, nil
}

// requirePerCallSnapshot 校验「UsePrice 估算」拿到的快照确实是按次价。
//
// 调用方可以预置 rc.PriceSnapshot（例如免计费档位写入的显式零价快照）。
// 如果预置的是倍率快照而估算声称 UsePrice，金额会按倍率公式重算——两者
// 组合起来正是「静默错价」。这里在预算前拒绝，绝不猜调用方意图。
func requirePerCallSnapshot(usePrice bool, snap *contract.PriceSnapshot) error {
	if !usePrice || snap == nil {
		return nil
	}
	if snap.Mode != contract.BillingModePerCall || !snap.Price.UsePrice {
		return fmt.Errorf("%w: UsePrice 估算要求按次快照（mode=%d use_price=%v）",
			pricing.ErrNoPerCallPrice, snap.Mode, snap.Price.UsePrice)
	}
	return nil
}

// currentSnapshotLocked 在结算时取最终快照。
//
// 跨组重试期间 relay 会刷新 rc.PriceSnapshot（新分组的倍率）；结算必须使用
// 最终选中分组的快照，而不是 PreConsume 时的旧快照。分组切换由
// refreshSnapshotForGroupLocked 在调用本函数**之前**处理；这里只负责读取
// 并补写上游格式事实。
func (s *Session) currentSnapshotLocked(ctx context.Context, fallbackModel string) (*contract.PriceSnapshot, error) {
	if s.rc != nil && s.rc.PriceSnapshot != nil {
		s.snap = s.rc.PriceSnapshot
		s.inheritUsageSemanticLocked(s.snap)
		return s.snap, nil
	}
	if s.snap != nil {
		s.inheritUsageSemanticLocked(s.snap)
		return s.snap, nil
	}
	if s.engine.source == nil {
		return nil, ErrNoPricing
	}
	model := s.model
	if model == "" {
		model = fallbackModel
	}
	// 分组以 RelayContext 的当前值为准（跨组重试的唯一写入点是
	// relay.WithUsingGroup）；s.group 只是 PreConsume 时的历史值。
	group := s.group
	if s.rc != nil && strings.TrimSpace(s.rc.UsingGroup) != "" {
		group = strings.TrimSpace(s.rc.UsingGroup)
	}
	snap, err := s.resolveAt(ctx, model, group)
	if err != nil {
		return nil, fmt.Errorf("billing: resolve pricing for model %q: %w", model, err)
	}
	s.snap = &snap
	s.inheritUsageSemanticLocked(s.snap)
	s.adoptSnapshotLocked(s.snap)
	return s.snap, nil
}

// refreshSnapshotForGroupLocked 在 Reserve/ChargeIncrement/Terminate 前检查
// 分组是否已切换（N3）：RelayContext.UsingGroup 与当前快照的分组不同时，
// 通过定价源重新解析并覆盖 s.snap 与 rc.PriceSnapshot。
//
// 为什么必须刷新：auto 跨组重试会用新分组重新选路并结算；若沿用 PreConsume
// 时冻结的旧分组快照（GroupRatio/覆盖价/表达式都不同），最终金额与选中分组
// 不符——这是 N3 复现的「跨组重试按预扣快照结算」。刷新失败按 fail-closed
// 上抛（保守方向：宁可不结算让 worker 退款，也不能按错误分组收钱）。
//
// 没有任何已冻结快照时无需刷新：PreConsume 会按当前分组首次解析。
func (s *Session) refreshSnapshotForGroupLocked(ctx context.Context) error {
	if s.engine.source == nil || s.rc == nil {
		return nil
	}
	group := strings.TrimSpace(s.rc.UsingGroup)
	if group == "" {
		return nil
	}
	current := s.snap
	if current == nil {
		current = s.rc.PriceSnapshot
	}
	if current == nil {
		return nil
	}
	if current.Price.GroupRatioInfo.Group == group {
		return nil
	}
	model := s.model
	if model == "" {
		model = s.rc.OriginModel
	}
	snap, err := s.resolveAt(ctx, model, group)
	if err != nil {
		return fmt.Errorf("billing: refresh pricing for group %q: %w", group, err)
	}
	s.snap = &snap
	s.group = group
	s.inheritUsageSemanticLocked(s.snap)
	// 这里是**覆盖**而不是 adoptSnapshotLocked 的「只补空」：调用方要的
	// 就是让 rc 从旧组快照切到新组快照。
	s.rc.PriceSnapshot = s.snap
	return nil
}

// claudeRawFormat 是 relay.FormatClaude 的字面量。
//
// 为什么在计费包内重复字面量：billing 不得 import internal/relay
// （.golangci.yml 的 billing-boundary），而契约的 RelayFormat 只承诺
// 「协议标识」没有导出常量。字面量 "claude" 与 relay.FormatClaude 一致。
const claudeRawFormat = "claude"

// inheritUsageSemanticLocked 按**入站协议事实**标注快照的 Claude 语义。
//
// IsClaudeUsageSemantic 是契约冻结的只读派生元信息（契约 §3.3 / B0 §3）；
// Saturator 据此写 usage_logs.other.usage_semantic=anthropic。M2 之前没有
// 任何写入方（N3），这里在快照进入会话时补齐：Claude 入站（/v1/messages）
// 的 input_tokens 不含缓存读，缓存读是独立计费维度。
func (s *Session) inheritUsageSemanticLocked(snap *contract.PriceSnapshot) {
	if snap == nil || snap.IsClaudeUsageSemantic || s.rc == nil {
		return
	}
	if string(s.rc.RawFormat) == claudeRawFormat {
		snap.IsClaudeUsageSemantic = true
	}
}

// noteUsageSemanticFromFactLocked 用**结算用量事实**补写 Claude 语义。
//
// 【为什么不能只看 PromptIncludesCache=false】该布尔在「协议未声明」时就是
// Go 零值 false：`x-model-response-usage` 头解析器在缺 `prompt_includes_cache`
// 时默认 false，Gemini 语义同样使用 false。若据此判定 Claude，会把
// header 派生/非 Claude 请求记成 usage_semantic="anthropic"（虚假审计值）。
//
// 因此事实路径要求一个**正向 Claude 指示**：上游原始 usage 携带 Claude 专有
// 缓存键（`cache_read_input_tokens` / `cache_creation_input_tokens`），或
// 协议路径（RawFormat=claude，见 inheritUsageSemanticLocked）已置位。仅有
// false 布尔 + 非零用量不放行。
func (s *Session) noteUsageSemanticFromFactLocked(snap *contract.PriceSnapshot, u contract.Usage) {
	if snap == nil || snap.IsClaudeUsageSemantic || u.PromptIncludesCache {
		return
	}
	if !usageHasAnyFact(u) || !hasClaudeUsageKeys(u.Raw) {
		return
	}
	snap.IsClaudeUsageSemantic = true
}

// claudeRawUsageKeys 是 Anthropic usage 的专有缓存维度键（relay-04 的
// Claude 适配器原样保留在 Usage.Raw）。出现任一即证明上游说的是 Claude 语义。
var claudeRawUsageKeys = []string{
	"cache_read_input_tokens",
	"cache_creation_input_tokens",
}

// hasClaudeUsageKeys 判断原始 usage 是否携带 Claude 专有键。
//
// 只做键存在性判定（值可为 0）：Claude 上游可能在无缓存命中时仍发送
// `cache_read_input_tokens: 0`，键本身是协议事实。
func hasClaudeUsageKeys(raw map[string]any) bool {
	if len(raw) == 0 {
		return false
	}
	for _, key := range claudeRawUsageKeys {
		if _, ok := raw[key]; ok {
			return true
		}
	}
	return false
}

// usageHasAnyFact 判断用量中是否存在任一非零计费维度。
func usageHasAnyFact(u contract.Usage) bool {
	return u.Prompt != 0 || u.Completion != 0 || u.Total != 0 ||
		u.CacheRead != 0 || u.CacheWrite != 0 || u.Reasoning != 0 ||
		u.AudioSeconds != 0 || u.Reserved != 0 ||
		u.PromptDetail != (contract.UsageDetail{}) ||
		u.CompletionDetail != (contract.UsageDetail{})
}

// adoptSnapshotLocked 把解析出的快照回填到 rc.PriceSnapshot（Minor-2）。
//
// Saturator 需要通过 rc 读取 billing_mode/表达式等元信息落日志；中继在
// PreConsume 前没有设置快照时，由会话回填同一个冻结对象。只补元信息，
// 不改变任何定价结果；已有快照（中继冻结）时不覆盖。
func (s *Session) adoptSnapshotLocked(snap *contract.PriceSnapshot) {
	if s.rc != nil && snap != nil && s.rc.PriceSnapshot == nil {
		s.rc.PriceSnapshot = snap
	}
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
// 失败时回滚全部资金并终结会话（fail-closed）：留下无心跳的冻结资金会让
// worker 无法判定；而回滚会写入一行 release，若允许同 requestID 重试再结算，
// 第二次 release 会撞 wallet_release_refid_uq 部分唯一索引。
//
// 回滚必须是**全额**（R-30）：登记失败可能发生在 Reserve 之后，此时
// ChargeIncrement 的增量扣减已经落账。只退 hold 会留下「会话已置 refunded
// 但增量未回补」的空洞，因此有增量时走 refundSession（hold + consumed
// 合并为一行 release）；没有增量时保持 hold-only 的轻量路径。
func (s *Session) registerLeaseLocked(ctx context.Context) error {
	if s.engine.registry == nil || s.leaseRegistered {
		return nil
	}
	if err := s.engine.registry.Register(ctx, s.requestID, s.userID, s.tokenID,
		time.Duration(contract.InflightLeaseTTLSeconds)*time.Second); err != nil {
		var rbErr error
		// R-W9B-03：两条回滚路径都把令牌回补并入资金事务，登记失败后不再有
		// 独立的 CreditToken（否则会二次回补或漏补）。
		if s.consumed > 0 {
			_, _, rbErr = s.engine.ledger.refundSession(ctx, s.userID, s.tokenID, s.requestID, s.consumed)
		} else {
			_, _, rbErr = s.engine.ledger.releaseHolds(ctx, s.userID, s.tokenID, s.requestID, "released")
		}
		if rbErr != nil && !errors.Is(rbErr, errHoldClosed) && !errors.Is(rbErr, errNoOpenHold) {
			return fmt.Errorf("billing: register lease: %v (rollback failed: %w)", err, rbErr)
		}
		s.preConsumed = 0
		s.consumed = 0
		s.tokenDeducted = 0
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
//
// 两个循环都要停：钱包会话只有 Redis 租约；订阅会话只有 PG 在飞租约；
// 混合来源（wallet_first 回退订阅）理论上可能先后启动两者，统一停掉
// 才能保证会话终结后不再有续租写入。
func (s *Session) stopHeartbeatLocked() {
	if s.leaseStop != nil {
		close(s.leaseStop)
		s.leaseStop = nil
	}
	if s.leaseDone != nil {
		<-s.leaseDone
		s.leaseDone = nil
	}
	if s.subLeaseStop != nil {
		close(s.subLeaseStop)
		s.subLeaseStop = nil
	}
	if s.subLeaseDone != nil {
		<-s.subLeaseDone
		s.subLeaseDone = nil
	}
}

// startSubscriptionLeaseHeartbeatLocked 启动订阅 in-flight 租约的续租循环
// （调用方必须持有 s.mu）。
//
// 只在预扣事务成功（created=true）后调用：回放会话不拥有订阅记录，不能
// 替首执行者续租。续租失败**只记日志**（R-W9B-02）：租约过期只会让兜底
// 扫描多一个候选，资金方向仍由终态 CAS 与租约守卫仲裁，绝不因为一次心跳
// 写失败就提前退款或停止请求。
func (s *Session) startSubscriptionLeaseHeartbeatLocked() {
	if s.engine.subscriptionFunding == nil || s.subLeaseStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	s.subLeaseStop = stop
	s.subLeaseDone = done

	interval := s.engine.subscriptionLeaseHeartbeatInterval()
	requestID := s.requestID
	funding := s.engine.subscriptionFunding
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// 与钱包租约同口径：请求取消不应终止续租，只有会话终结信号才停。
		base := context.WithoutCancel(context.Background())
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(base, billingOpTimeout)
				if _, err := funding.RenewInflightLease(ctx, []string{requestID}); err != nil {
					slog.Warn("billing: subscription inflight lease renewal failed; settlement direction unchanged",
						"request_id", requestID, "error", err)
				}
				cancel()
			}
		}
	}()
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

// estimateImageCount 返回请求域图像张数：
//   - 已声明（ImageCount/AttemptImageCount > 0）→ 精确值（≤ MaxImageN）；
//   - 未声明 → imageCountUnset（-1），由定价层按模式缺省（请求域 1）。
//
// 估值不再无条件补 1：-1 与会话结算域的「有效 0 张」必须可区分（R-24/I2）。
func estimateImageCount(est contract.QuotaEstimate) int {
	n := est.ImageCount
	if n <= 0 {
		n = est.AttemptImageCount
	}
	if n <= 0 {
		return imageCountUnset
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
