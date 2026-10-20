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
	// 为什么必须大于一次正常请求的生命周期：长连接/任务在窗口内仍可能
	// 正常 Terminate；过早退款会让结算 delta 落到已退款的记录上（存储层
	// 幂等短路后额度少退一次），且用户会被误判为「免费」。
	ConfigSubscriptionRecoveryWindowSeconds = "billing.subscription_recovery_window_seconds"

	defaultSubscriptionRecoveryWindowSeconds = int64(900)

	// subscriptionRecoveryScanLimit 是单轮扫描上限：积压时有界处理，其余
	// 下一轮继续（与 DueForReset/DeletePreConsumeBefore 同口径）。
	subscriptionRecoveryScanLimit = 100
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

	already := s.preConsumed + s.consumed
	cost := already
	if !emptyUsageFact(fact) {
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
		cost = s.actualCostLocked(snap, fact)
	}

	delta := cost - already
	settle, err := s.engine.subscriptionFunding.PostConsumeUserSubscriptionDelta(
		ctx, s.subscriptionID, delta)
	if err != nil {
		// 结算失败（越界/数据库故障）：本次 delta 未落地，保留预扣。绝不
		// 静默吞错；request_registry 的 settled 由 Terminate 成功才写，
		// 因此这里显式标记 settlement_pending，供日志/对账与兜底扫描识别。
		slog.Warn("billing: subscription settlement failed; keeping pre-consume for compensation",
			"request_id", s.requestID, "subscription_id", s.subscriptionID,
			"delta", delta, "settlement_pending", true, "error", err)
		return fmt.Errorf("billing: settle subscription delta: %w", err)
	}

	// 结算成功后立刻记录 request_registry 终态：预扣记录在成功结算后仍是
	// consumed，没有这条记录，15 分钟后的兜底扫描会把已结算的请求再退一次。
	if err := s.engine.subscriptionFunding.MarkRequestSettled(
		ctx, s.requestID, s.userID); err != nil {
		// registry 写失败不改变「订阅已结算」事实；无法回滚 delta（重试会
		// 二次结算）。显式告警 settlement_pending，兜底扫描仍有窗口误退，
		// 但方向是「可能多退」而非静默吞钱，交由对账/日志定位。
		slog.Warn("billing: record subscription settlement state failed; settlement_pending",
			"request_id", s.requestID, "user_id", s.userID,
			"settlement_pending", true, "error", err)
	}

	// 资金已提交，随后把令牌额度从**已扣额**调整到最终成本（R-73）。
	// 令牌不足只记 settlement_pending：订阅侧的账已经正确，重试结算会把
	// 订阅 delta 再落一次，因此不能以错误形式把会话留在未结算态。
	s.adjustSubscriptionTokenLocked(ctx, cost)

	refunded := int64(0)
	if already > cost {
		refunded = already - cost
	}
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
// 偏好来自 resolveFundingPlanLocked 的冻结快照；未解析（理论上订阅来源
// 必然已解析）或映射失败时返回空串，日志层据此省略该键而不是编造默认值。
func (s *Session) billingPreferenceTextLocked() string {
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

	// Important-1：结案后不得再产生订阅消耗（否则额度无法归属到请求）。
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

// RecoverStaleSubscriptionPreConsumes 扫描并幂等退款长期未结算的订阅预扣
// （billing-06 §5.2；worker 每轮调用）。
//
// 【判据】subscription_pre_consume_records.status='consumed' 且创建时间早于
// now − 配置窗口（默认 15 分钟）；存储层同时排除 request_registry 已
// settled/refunded 的记录。
//
// 【并发】MarkPreConsumeRefunded 在事务内 FOR UPDATE 并短路 refunded，因此
// 与正常 Terminate/Refund 并发时最多一方退回额度；重复扫描无副作用。
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
	window, err := e.subscriptionRecoveryWindow()
	if err != nil {
		return 0, err
	}
	recs, err := e.subscriptionFunding.ListStalePreConsumes(ctx, now.UTC().Add(-window), limit)
	if err != nil {
		return 0, fmt.Errorf("billing: list stale subscription pre-consume records: %w", err)
	}

	recovered := 0
	var failures []error
	for _, rec := range recs {
		if ctx.Err() != nil {
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
		if err := e.refundSubscriptionPreConsume(ctx, rec.RequestID); err != nil {
			release()
			// 「不得静默吞错」：逐条告警，整批继续。
			slog.Warn("billing: refund stale subscription pre-consume failed; retrying next round",
				"request_id", rec.RequestID, "subscription_id", rec.SubscriptionID,
				"pre_consumed", rec.PreConsumed, "error", err)
			failures = append(failures, fmt.Errorf("%s: %w", rec.RequestID, err))
			continue
		}
		// 记录退款终态，供其它执行者（迟到的 Terminate/任务回调）观察。
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
