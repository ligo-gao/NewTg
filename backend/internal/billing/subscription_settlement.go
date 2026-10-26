package billing

// 订阅来源的结算接缝（S4b；billing-04 §3.5/§4、billing-08 §5）。
//
// S4a 只实现「从哪扣」与预扣/退款。本文件补齐真正落在订阅账户上的三处：
//  1. Terminate：把 actualCost 与已扣额度（预扣 + 实时增量）的差额结算为
//     `PostConsumeUserSubscriptionDelta`，同步令牌额度，并写 subscription_*
//     成功日志键；
//  2. ChargeIncrement：实时增量立即追加到订阅账户（不能等到连接结束才结算，
//     否则长连接可以无限透支订阅总额）；
//  3. 崩溃兜底：扫描长期未结算的预扣记录并幂等退款。
//
// 订阅与钱包的最大区别是**没有 wallet_transactions hold 行**：钱包的
// 「多退少补」由 hold 行关闭 + release 流水表达，而订阅的权威状态在
// subscriptions.amount_used 与 subscription_pre_consume_records。因此订阅
// 路径绝不能落入 settleHolds/releaseHolds——那会把不属于本请求的钱包流水
// 当成 hold 处理，或凭空向钱包补扣一次。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// 订阅崩溃兜底的扫描参数。
const (
	// ConfigSubscriptionRecoveryWindowSeconds 是「预扣记录多久仍未结算即
	// 视为悬挂」的可配置窗口（默认 900s = 15 分钟）。
	//
	// 配置面：该键只从 configcenter 的 KV 读取（库内配置），**未**注册到
	// settings registry 的管理面白名单；运维需直接写配置行，界面不可编辑
	// （LOW：明确口径，避免误以为管理面可调）。
	//
	// 为什么必须大于一次正常请求的生命周期：长连接/任务在窗口内仍可能
	// 正常 Terminate；过早退款会让结算 delta 落到已退款的记录上（存储层
	// 幂等短路后额度少退一次），且用户会被误判为「免费」。
	ConfigSubscriptionRecoveryWindowSeconds = "billing.subscription_recovery_window_seconds"

	defaultSubscriptionRecoveryWindowSeconds = int64(900)

	// ConfigSubscriptionInflightLeaseTTLSeconds 是订阅在飞租约的心跳超时阈值
	// （R-W9B-02）。它与上面的兜底**窗口**是两组参数，严格分开：
	//   - 窗口（默认 900s）决定「预扣记录多老才纳入扫描」；
	//   - 租约 TTL（默认 120s）决定「心跳停多久才算进程已死」。
	// 必须满足 minSubscriptionInflightLeaseTTLSeconds <= TTL < 窗口：用窗口
	// 当心跳阈值会把持续续租的长任务误退；用 TTL 当窗口会让刚预扣的请求
	// 立刻可退。
	//
	// 【为什么 TTL 有 120s 下界（I-1 复核）】单次 processTask 的最坏耗时约
	// 55s（插件 5s + 出站 30s + parse 5s + 结算 10s）。虽然任务内会在出站
	// 前后与结算前续租，但任何一次续租与下一次之间的最大间隔仍受单段耗时
	// 约束；TTL 小于单任务预算时，一次慢出站就能让租约在处理中途过期，
	// 兜底扫描会把仍在运行的任务当成崩溃退款。120s 给单段与调度抖动留出
	// 两倍以上裕量，同时远小于 900s 窗口。测试可用 Engine.subLeaseTTL 注入
	// 更短值来验证续租行为（生产配置路径仍强制下界）。
	ConfigSubscriptionInflightLeaseTTLSeconds = "billing.subscription_inflight_lease_ttl_seconds"

	// defaultSubscriptionInflightLeaseTTLSeconds 与下界同值：默认配置必须
	// 满足自身校验，不允许「默认值非法、只有显式配置才合法」。
	defaultSubscriptionInflightLeaseTTLSeconds = int64(120)

	// minSubscriptionInflightLeaseTTLSeconds 是租约 TTL 的安全下界（I-1）：
	// 必须大于单任务最坏预算，否则任务处理中途会失去租约。
	minSubscriptionInflightLeaseTTLSeconds = int64(120)

	// subscriptionRecoveryScanLimit 是单轮扫描上限：积压时有界处理，其余
	// 下一轮继续（与 DueForReset/DeletePreConsumeBefore 同口径）。
	subscriptionRecoveryScanLimit = 100

	// subscriptionTerminalCheckTTL 是 ChargeIncrement 查询 registry 终态的
	// 最短间隔（M2）。实时连接每个 response.done 都会调用增量，逐事件查库
	// 会把日志级调用放大成数据库压力；1s 的缓存窗口只延迟「外部执行者写入
	// 终态后本会话停止追加」的发现时间，方向安全（最多多记 1s 的真实用量，
	// 不会少记已交付用量）。
	subscriptionTerminalCheckTTL = time.Second
)

// terminateSubscriptionLocked 结算订阅来源的请求（调用方持有 s.mu）。
//
// 差额口径：delta = actualCost − 已扣额度。这里的「已扣额度」是
// `subscriptionPreConsumed + consumed`：
//   - 合同 §6.5 的 `delta = actual − preConsumed` 以「preConsumed 是全部已
//     预扣」为前提；
//   - 但实时连接的 ChargeIncrement 会立即追加订阅额度（本文件另一入口），
//     那部分已真实落账。若仍只用冻结的预扣额计算，增量会被第二次扣一遍。
//
// 因此 delta 对「本次请求已经从订阅扣掉的全部额度」求差，既满足合同
// 语义（delta = 实际 − 已扣），也避免实时路径双扣。
func (s *Session) terminateSubscriptionLocked(ctx context.Context, fact contract.UsageFact) error {
	if s.engine.subscriptionFunding == nil {
		return fmt.Errorf("%w: subscription settlement requires subscription funding", ErrInvalidConfig)
	}
	if s.subscriptionID <= 0 {
		return fmt.Errorf("billing: subscription session %s has no frozen subscription id", s.requestID)
	}
	// 幂等回放会话没有自己的订阅扣减：首执行者持有记录，回放方结算会
	// 把同一请求的额度再调一次。直接按已结算收尾。
	if s.subscriptionReplayed {
		s.finishLocked(ctx, false)
		return nil
	}

	// M1：订阅结算与退款/崩溃兜底共用 requestID 结算锁。没有这把锁时，
	// 恢复扫描可以在「读 registry = 未结算」之后与本次结算交错，把同一个
	// 预扣既退款又结算。handoff/worker 已预取锁时会话标志为 true，这里 no-op。
	releaseLock, lockErr := s.ensureSettlementLockLocked(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer releaseLock()

	// 锁内复核终态：另一个执行者可能在本会话进入前已完成结算/退款。registry
	// 是订阅路径的权威终态证据；预扣记录是第二道（同步退款即使 registry 写
	// 失败也会把记录置 refunded）。
	state, stateErr := s.engine.subscriptionFunding.RequestRegistryState(ctx, s.requestID)
	if stateErr != nil {
		return fmt.Errorf("billing: read subscription settlement state for %s: %w", s.requestID, stateErr)
	}
	switch state {
	case "settled":
		s.finishLocked(ctx, false)
		return nil
	case "refunded":
		s.finishLocked(ctx, true)
		return nil
	}
	if rec, _, recErr := s.engine.subscriptionFunding.PreConsumeRecord(ctx, s.requestID); recErr == nil {
		if rec.Status == "refunded" {
			s.finishLocked(ctx, true)
			return nil
		}
	} else if !errors.Is(recErr, store.ErrPreConsumeNotFound) {
		return fmt.Errorf("billing: re-read subscription pre-consume for %s: %w", s.requestID, recErr)
	}

	already := s.preConsumed + s.consumed
	cost := already
	rawKnown := false
	if s.forcedCost != nil {
		// T5：任务计价表达式在提交期冻结，结算只读该成本，不重新解析定价
		// （订阅路径同样不得按当前配置重算）。
		cost = *s.forcedCost
		rawKnown = true
	} else if !emptyUsageFact(fact) {
		// N3：结算必须使用最终选中分组的快照。
		if err := s.refreshSnapshotForGroupLocked(ctx); err != nil {
			return err
		}
		snap, err := s.currentSnapshotLocked(ctx, fact.Model)
		if err != nil {
			// 没有快照时不能猜价格：保留已预扣额度（保守方向），由兜底退款
			// 在窗口后处置。钱包路径同样在此 fail closed。
			return err
		}
		s.noteUsageSemanticFromFactLocked(snap, fact.Usage)
		cost, rawKnown = s.actualCostLocked(snap, fact)
	}

	// §5.3：与钱包路径同口径——折扣只应用在原始成本上，且只应用一次。
	// 空 fact 与求值失败时 cost 已是折后的已扣额（rawKnown=false），
	// 再打折会二次让利。
	rawCost := cost
	if rawKnown {
		cost = ApplyDiscount(cost, s.discount)
	}

	delta := cost - already
	// R-W9B-01：delta 与 request_registry 的 settled 终态在**同一个 PG 事务**
	// 内提交。registry 是「已结算」的唯一持久化证据；把两者拆成两步会留下
	// 「已扣订阅、无终态」的崩溃窗口，兜底扫描随后会二次退款（G-DOM-36）。
	// applied=false 表示 CAS 未命中：另一执行者已结算/退款，本次事务整笔
	// 回滚（delta 未落地），按已有终态收敛即可。
	settle, applied, err := s.engine.subscriptionFunding.SettleSubscriptionDelta(
		ctx, s.requestID, s.userID, s.subscriptionID, delta)
	if err != nil {
		// 结算失败（越界/数据库故障）：本次 delta 未落地，保留预扣。绝不
		// 静默吞错；registry 终态与 delta 同事务，失败即整体回滚，兜底扫描
		// 仍能看到 consumed 记录并负责补偿。
		slog.Warn("billing: subscription settlement failed; keeping pre-consume for compensation",
			"request_id", s.requestID, "subscription_id", s.subscriptionID,
			"delta", delta, "pre_consumed", s.preConsumed,
			"increment_deducted", s.consumed, "settlement_pending", true, "error", err)
		return fmt.Errorf("billing: settle subscription delta: %w", err)
	}
	if !applied {
		// 终态 CAS 未命中（不可能是本次事务的部分提交：失败即回滚）。再读
		// registry 区分「已结算」与「已退款」，不要一律标成 refunded。
		state, stateErr := s.engine.subscriptionFunding.RequestRegistryState(ctx, s.requestID)
		if stateErr != nil {
			return fmt.Errorf("billing: read subscription terminal state for %s: %w", s.requestID, stateErr)
		}
		if state == "refunded" {
			s.finishLocked(ctx, true)
			return nil
		}
		// settled（或 registry 行缺失但预扣已被置 refunded 的旧数据）：两者
		// 都是「已在别处结案」，finishLocked(false) 不再持有可退金额。
		s.finishLocked(ctx, false)
		return nil
	}

	// 资金已提交，随后把令牌额度从**已扣额**调整到最终成本（R-73）。
	// 令牌不足只记 settlement_pending：订阅侧的账已经正确，重试结算会把
	// 订阅 delta 再落一次（CAS 会拦住），以错误形式留在未结算态没有意义。
	s.adjustSubscriptionTokenLocked(ctx, cost)

	refunded := int64(0)
	if already > cost {
		refunded = already - cost
	}
	originalCost, discountRate, discountTarget := s.ledgerDiscount(rawCost, cost, rawKnown)
	outcome := SettlementOutcome{
		Cost:                    cost,
		PreDeducted:             s.preConsumed,
		Refunded:                refunded,
		Status:                  UsageLogStatusSuccess,
		FundingSource:           contract.FundingSubscription,
		SubscriptionID:          s.subscriptionID,
		SubscriptionPreConsumed: s.preConsumed,
		SubscriptionPostDelta:   delta,
		SubscriptionPlanID:      settle.PlanID,
		SubscriptionPlanTitle:   settle.PlanTitle,
		SubscriptionTotal:       settle.AmountTotal,
		SubscriptionUsed:        settle.AmountUsed,
		BillingPreference:       s.billingPreferenceTextLocked(),
		OriginalCost:            originalCost,
		DiscountRate:            discountRate,
		DiscountTarget:          discountTarget,
	}
	if s.engine.saturator != nil {
		if err := s.engine.saturator.Saturate(ctx, &fact, s.rc, outcome); err != nil {
			// 钱已结算，日志失败不能触发重试（会二次结算订阅额度）。与钱包
			// 「无 hold 补扣」路径同一裁决：标记终结并上抛供上层告警。
			s.finishLocked(ctx, false)
			return fmt.Errorf("billing: write subscription usage log: %w", err)
		}
	}
	s.finishLocked(ctx, false)
	return nil
}

// adjustSubscriptionTokenLocked 把令牌额度调整到最终成本。
//
// adjustment = cost − tokenDeducted。tokenDeducted 与订阅已扣额度同步
// （预扣 + 实时增量），因此正数为少补、负数为多退。
//
// 令牌不足（正差额 DebitToken 失败）不回滚已提交的订阅结算：两者是两笔账，
// 回滚订阅会让真实消耗变成免费。按 billing-03 §5.7 只记 Warn 并显式标记
// settlement_pending，交由对账处理。
func (s *Session) adjustSubscriptionTokenLocked(ctx context.Context, cost int64) {
	adjustment := cost - s.tokenDeducted
	if adjustment == 0 {
		return
	}
	if err := s.engine.ledger.AdjustToken(ctx, s.userID, s.tokenID, adjustment); err != nil {
		slog.Warn("billing: token quota adjustment failed after subscription settlement; settlement_pending",
			"request_id", s.requestID, "token_id", s.tokenID,
			"adjustment", adjustment, "settlement_pending", true, "error", err)
		return
	}
	s.tokenDeducted = cost
}

// billingPreferenceTextLocked 返回本请求冻结的用户偏好文本（日志键）。
//
// 偏好来自 resolveFundingPlanLocked 的冻结快照；任务/恢复路径重建的会话没有
// fundingPlan，由 handoff 读取后回填 billingPreferenceText（LOW）。两者都不
// 可用时返回空串，日志层据此省略该键而不是编造默认值。
func (s *Session) billingPreferenceTextLocked() string {
	if s.billingPreferenceText != "" {
		return s.billingPreferenceText
	}
	if s.fundingPlan == nil {
		return ""
	}
	text, err := store.BillingPreferenceText(s.fundingPlan.preference)
	if err != nil {
		return ""
	}
	return text
}

// chargeIncrementSubscriptionLocked 把实时增量立即追加到订阅账户。
//
// 为什么不能推迟到 Terminate：实时连接（WSS）可以持续数小时且不设请求级
// 上界，若增量只在连接结束时结算，用户可以在订阅额度耗尽后继续消耗。
//
// 顺序：先订阅账户、后令牌额度。订阅增量落账后不可回滚（存储层没有按
// requestId 的增量幂等记录），因此令牌扣减失败时请求失败但已发生的增量
// 保留——这是冻结语义：增量代表已经交付给用户的用量，不允许因为一次
// 令牌侧故障而白送。错误原样上抛，由调用方按请求失败处理。
func (s *Session) chargeIncrementSubscriptionLocked(ctx context.Context, delta int64) error {
	if s.engine.subscriptionFunding == nil {
		return fmt.Errorf("%w: subscription increment requires subscription funding", ErrInvalidConfig)
	}
	if s.subscriptionID <= 0 {
		return fmt.Errorf("billing: subscription session %s has no frozen subscription id", s.requestID)
	}
	if s.subscriptionReplayed {
		// 回放会话没有自己的订阅扣减；再追加会二次消耗首执行者的额度。
		return nil
	}

	// M2：结案后不得再产生订阅消耗（否则额度无法归属到请求）。订阅来源
	// 没有钱包流水，ledger.requestTerminal 永远看不到这条请求；终态证据是
	// request_registry。逐事件查库代价过高，用本地终态位 + 短 TTL 节流：
	//   - 一旦观测到 settled/refunded 立即永久拒绝；
	//   - 未观测到终态时最多每 subscriptionTerminalCheckTTL 查一次。
	// 缓存窗口内的残余（外部结案后最多再追加 1s 用量）登记在 G-DOM-35⑤。
	if s.subscriptionTerminal {
		s.markTerminalLocked(ctx)
		return fmt.Errorf("%w: %s", ErrRequestClosed, s.requestID)
	}
	if s.terminalCheckAt.IsZero() || time.Since(s.terminalCheckAt) >= subscriptionTerminalCheckTTL {
		state, stateErr := s.engine.subscriptionFunding.RequestRegistryState(ctx, s.requestID)
		if stateErr != nil {
			return fmt.Errorf("billing: check subscription terminal state: %w", stateErr)
		}
		s.terminalCheckAt = time.Now()
		if state == "settled" || state == "refunded" {
			s.subscriptionTerminal = true
			s.markTerminalLocked(ctx)
			return fmt.Errorf("%w: %s", ErrRequestClosed, s.requestID)
		}
	}

	// N3：实时增量按当前分组的快照累计（跨组后旧快照不得继续使用）。
	if err := s.refreshSnapshotForGroupLocked(ctx); err != nil {
		return err
	}

	if _, err := s.engine.subscriptionFunding.PostConsumeUserSubscriptionDelta(
		ctx, s.subscriptionID, delta); err != nil {
		slog.Warn("billing: subscription increment settlement failed; settlement_pending",
			"request_id", s.requestID, "subscription_id", s.subscriptionID,
			"delta", delta, "settlement_pending", true, "error", err)
		return fmt.Errorf("billing: settle subscription increment: %w", err)
	}
	s.consumed += delta

	// R-73：订阅账户已提交，随后同步令牌额度。失败时增量不可回滚（注释见
	// 函数头），但必须让请求失败而不是静默少扣令牌。
	if err := s.engine.ledger.DebitToken(ctx, s.userID, s.tokenID, delta); err != nil {
		slog.Warn("billing: token debit failed after subscription increment; increment is retained",
			"request_id", s.requestID, "token_id", s.tokenID,
			"delta", delta, "settlement_pending", true, "error", err)
		return fmt.Errorf("billing: debit token for subscription increment: %w", err)
	}
	s.tokenDeducted += delta
	return nil
}

// emptyUsageFact 判断结算事实是否为空（任务按预扣额终结的判据）。
//
// 只检查计费数字段：Source/关系布尔/Raw 不参与。空 fact 没有可信用量，
// 唯一可用的金额就是已预扣额（delta=0）。
func emptyUsageFact(fact contract.UsageFact) bool {
	return fact.Model == "" && usageIsZero(fact.Usage)
}

// markRequestSettled 已随 R-W9B-01 删除：registry 终态不再由独立语句尽力
// 写入，而是与 delta 同事务（见 SettleSubscriptionDelta）。保留
// MarkRequestSettled 适配器方法只作为审计/调试接缝，正确性不依赖它。

// RecoverStaleSubscriptionPreConsumes 扫描并幂等退款长期未结算的订阅预扣
// （billing-06 §5.2；worker 每轮调用）。
//
// 【判据】subscription_pre_consume_records.status='consumed' 且创建时间早于
// now − 配置窗口（默认 15 分钟），**并且**在飞租约缺失或已过期（R-W9B-02）：
// registry 行缺失、state 非终态且非 in_flight，或 in_flight 且心跳早于
// now − 租约 TTL。持续续租的长流/长任务（流式会话心跳、任务轮询器逐任务
// 续租）因此不会被退款；真正崩溃的进程在 TTL 后即可回收。
//
// 【并发】扫描与**所有**结算/退款入口（实时 Queue 的 Session.Terminate/
// Refund 与任务 handoff）共用 requestID 结算锁（M1），因此「扫描判定未结算」
// 与「正常收尾」不会交错；RefundSubscriptionRequest 在事务内 FOR UPDATE 并
// 以 registry refunded CAS 作为第二道幂等，重复扫描无副作用。
// 扫描与续租的并发由退款事务内的租约守卫仲裁：续租先提交则本轮不退款
// （Applied=false, Terminal="in_flight"），退款先提交则续租的 in_flight
// 条件不再命中，不会出现「已退款又被续租复活」。
//
// 【令牌额度】预扣记录不存 tokenID，worker 无法回补令牌。这里只退订阅账户；
// 令牌额度由请求所属租约的既有兜底路径处理（有租约时）或留在令牌上——
// 方向安全（少退给用户，不多送）。该限制在注释中显式保留，不静默。
//
// 失败不中断整批：单条失败记录错误并继续，返回值给出本轮成功数；只要存在
// 失败就返回非 nil error，让 worker 在下一轮重试并打日志。
func (e *Engine) RecoverStaleSubscriptionPreConsumes(
	ctx context.Context, now time.Time, limit int) (int, error) {
	if e.subscriptionFunding == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = subscriptionRecoveryScanLimit
	}
	window, leaseTTL, err := e.subscriptionLeaseParams()
	if err != nil {
		return 0, err
	}
	nowUTC := now.UTC()
	leaseExpiredBefore := nowUTC.Add(-leaseTTL)
	recs, err := e.subscriptionFunding.ListStalePreConsumes(
		ctx, nowUTC.Add(-window), leaseExpiredBefore, limit)
	if err != nil {
		return 0, fmt.Errorf("billing: list stale subscription pre-consume records: %w", err)
	}

	recovered := 0
	var failures []error
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			break
		}
		// 与 Terminate/Refund 共用 requestID 结算锁：扫描到记录后、真正退款
		// 前必须确认没有正常结算正在收尾。锁被占用说明请求仍活跃，本轮跳过
		// （记录仍在候选集，下一轮重扫）。
		release, lockErr := e.acquireSettlementLock(ctx, rec.RequestID)
		if errors.Is(lockErr, ErrSettlementInProgress) {
			continue
		}
		if lockErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", rec.RequestID, lockErr))
			continue
		}
		state, stateErr := e.subscriptionFunding.RequestRegistryState(ctx, rec.RequestID)
		if stateErr != nil {
			release()
			slog.Warn("billing: read request settlement state failed; retrying next round",
				"request_id", rec.RequestID, "error", stateErr)
			failures = append(failures, fmt.Errorf("%s: %w", rec.RequestID, stateErr))
			continue
		}
		if state == "settled" || state == "refunded" {
			// 持锁后的复核：候选查询与退款之间可能已完成正常结算/退款。
			release()
			continue
		}
		// R-W9B-03：订阅退款（含 amount_used 回退、预扣终态、registry 终态）
		// 在存储层是**一个事务**。这里不传令牌：预扣记录不存 tokenID，worker
		// 无法回补令牌（方向安全的「少退」，见函数头）。锁内复核到的未终态
		// 只是优化；真正的仲裁仍是事务内的 registry CAS + 租约守卫。
		//
		// LeaseExpiredBefore 让扫描与续租在同一事务内线性化（R-W9B-02）：
		// 判定之后请求续租成功的，整笔退款回滚并返回 Terminal="in_flight"。
		result, err := e.refundSubscriptionRequest(ctx, store.SubscriptionRefund{
			RequestID:          rec.RequestID,
			LeaseExpiredBefore: leaseExpiredBefore,
		})
		if err != nil {
			release()
			// 「不得静默吞错」：逐条告警，整批继续。
			slog.Warn("billing: refund stale subscription pre-consume failed; retrying next round",
				"request_id", rec.RequestID, "subscription_id", rec.SubscriptionID,
				"pre_consumed", rec.PreConsumed, "error", err)
			failures = append(failures, fmt.Errorf("%s: %w", rec.RequestID, err))
			continue
		}
		if !result.Applied {
			// 幂等命中或租约仍在飞：预扣已 refunded / registry 已 settled /
			// 心跳在过期界之后。事务没有做任何资金变更，跳过。
			release()
			continue
		}
		// 记录退款终态（审计兜底；资金正确性已由上面的同事务 CAS 保证）。
		// user_id 在预扣表里不存在，写 0；该表无 FK，0 只表示 worker 未知归属。
		if markErr := e.subscriptionFunding.MarkRequestRefunded(ctx, rec.RequestID, 0); markErr != nil {
			slog.Warn("billing: record stale subscription refund state failed",
				"request_id", rec.RequestID, "error", markErr)
		}
		release()
		recovered++
	}
	if len(failures) > 0 {
		return recovered, fmt.Errorf("billing: %d stale subscription pre-consume refund(s) failed: %w",
			len(failures), errors.Join(failures...))
	}
	return recovered, nil
}

// subscriptionRecoveryWindow 读取崩溃兜底窗口。
//
// 非法值（≤0）按配置错误上抛而不是禁用扫描：禁用会让崩溃请求的订阅额度
// 永久冻结在 amount_used 上（用户额度被静默吞掉），这是资金正确性问题，
// 不能靠运维「记得开」。
func (e *Engine) subscriptionRecoveryWindow() (time.Duration, error) {
	seconds, err := e.configInt64(ConfigSubscriptionRecoveryWindowSeconds,
		defaultSubscriptionRecoveryWindowSeconds)
	if err != nil {
		return 0, err
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("%w: %s must be > 0", ErrInvalidConfig,
			ConfigSubscriptionRecoveryWindowSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

// subscriptionLeaseTTL 读取订阅在飞租约的 TTL（R-W9B-02）。
//
// 两条来源：测试注入的 e.subLeaseTTL 优先；生产走配置键。非法值（≤0、
// 低于 120s 安全下界）按配置错误上抛而不是退化成「永不过期」或「过短」：
// 永不过期会让崩溃请求永远不被回收，过短会让单任务处理中途失去租约（I-1）。
func (e *Engine) subscriptionLeaseTTL() (time.Duration, error) {
	if e.subLeaseTTL > 0 {
		return e.subLeaseTTL, nil
	}
	seconds, err := e.configInt64(ConfigSubscriptionInflightLeaseTTLSeconds,
		defaultSubscriptionInflightLeaseTTLSeconds)
	if err != nil {
		return 0, err
	}
	if seconds < minSubscriptionInflightLeaseTTLSeconds {
		return 0, fmt.Errorf("%w: %s must be >= %ds (single-task budget safety floor)",
			ErrInvalidConfig, ConfigSubscriptionInflightLeaseTTLSeconds,
			minSubscriptionInflightLeaseTTLSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}

// subscriptionLeaseParams 读取并校验「兜底窗口 + 租约 TTL」两组参数。
//
// 集中在一处是为了让扫描入口与启动期校验（ValidateSubscriptionLeaseConfig）
// 使用**同一套**规则，不会出现「扫描时合法、启动时没查」或反向漂移。
func (e *Engine) subscriptionLeaseParams() (window, leaseTTL time.Duration, err error) {
	window, err = e.subscriptionRecoveryWindow()
	if err != nil {
		return 0, 0, err
	}
	leaseTTL, err = e.subscriptionLeaseTTL()
	if err != nil {
		return 0, 0, err
	}
	if window <= leaseTTL {
		// 两组参数混用会让判定失去意义：窗口必须严格更大，才能同时表达
		// 「够老」与「心跳已停」。配置错误必须显式上抛，不能静默退化成
		// 只要到期就退（那是 G-DOM-35 的原始缺陷）。
		return 0, 0, fmt.Errorf("%w: %s (%s) must exceed %s (%s)",
			ErrInvalidConfig, ConfigSubscriptionRecoveryWindowSeconds, window,
			ConfigSubscriptionInflightLeaseTTLSeconds, leaseTTL)
	}
	return window, leaseTTL, nil
}

// ValidateSubscriptionLeaseConfig 在启动/装配期校验订阅租约配置（M-1 复核）。
//
// 为什么前移到装配期：若只在扫描时校验，配置错误要等到 worker 第一轮扫描
// 才以「兜底失败」的形式暴露（且可能被当作瞬时 DB 故障重试），运维很难
// 区分。启动期失败让错误配置无法上线。
//
// 未装配订阅（纯钱包部署）时 no-op：两条参数只服务于订阅扫描。
func (e *Engine) ValidateSubscriptionLeaseConfig() error {
	if e.subscriptionFunding == nil {
		return nil
	}
	_, _, err := e.subscriptionLeaseParams()
	return err
}

// subscriptionLeaseHeartbeatInterval 返回订阅租约的续租间隔。
//
// 固定取契约心跳间隔（30s）对 120s 的 TTL 仍留出 4 倍裕量；这里再与
// TTL/3 取小，保证 TTL 被测试/未来配置压小时任意单个续租失败后仍有两次
// 机会。测试专用旋钮 subHeartbeat 优先；下限 10ms 防止配置成 0 导致忙循环。
//
// 刻意不复用 e.heartbeat：那是钱包 Redis 租约的测试旋钮，钱包用例把它压到
// 10ms；订阅心跳若跟随，会把每个订阅会话变成每 10ms 一次数据库写。
func (e *Engine) subscriptionLeaseHeartbeatInterval() time.Duration {
	if e.subHeartbeat > 0 {
		return e.subHeartbeat
	}
	ttl, err := e.subscriptionLeaseTTL()
	if err != nil {
		ttl = time.Duration(defaultSubscriptionInflightLeaseTTLSeconds) * time.Second
	}
	interval := time.Duration(contract.InflightHeartbeatSeconds) * time.Second
	if third := ttl / 3; third > 0 && third < interval {
		interval = third
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	return interval
}
