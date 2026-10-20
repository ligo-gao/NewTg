package billing

import (
	"errors"
	"fmt"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// 本文件是任务平台的**按 requestID 结算入口**（contract.BillingHandoff 的
// 引擎侧）。任务提交返回后原 Session 已 Detach（心跳停止、hold 保留），
// 轮询器只有持久化的 requestID；如果把 Session 对象一起带下去，轮询器就
// 必须持有进程内状态——重启即失效。因此这里从账本行重建最小会话：
// requestID 是锚点，userID / hold 来自 wallet_transactions 的 hold 行。

// TerminateByRequestID 在任务终态时结算（T6b 调用）。
//
// Deprecated: 任务路径必须使用 TerminateByRequestIDForToken。无令牌版本
// 以 tokenID=0 结算，令牌额度不参与调整/归属；新调用方误用会重新引入
// T6b 复核 H1 的钱包/令牌不一致。保留仅为兼容既有测试与内部调用。
func (e *Engine) TerminateByRequestID(requestID string, fact contract.UsageFact) error {
	return e.TerminateByRequestIDForToken(requestID, 0, fact)
}

// TerminateByRequestIDForToken 是任务结算的 token-aware 入口。
//
// tokenID 来自任务行 `private_data.token_id`（T6a 持久化）：结算日志必须能
// 归属到发起任务的令牌；at-held 路径本身不调整令牌额度（cost == 已预扣），
// 但 usage_logs.token_id 必须正确。
func (e *Engine) TerminateByRequestIDForToken(requestID string, tokenID int64, fact contract.UsageFact) error {
	if requestID == "" {
		return ErrMissingRequestID
	}
	if fact.Model == "" && usageIsZero(fact.Usage) {
		return e.terminateAtHeld(requestID, tokenID, fact)
	}
	// 从账本水合 hold 与令牌扣减：非空 fact 会「多退少补」，令牌额度也必须
	// 跟着差额调整。若只设 tokenID 而不水合 tokenDeducted，Terminate 内的
	// adjustTokenToCost 会以 tokenDeducted=0 计算，CreditToken 命中 0 行、
	// 日志后返回 nil，造成钱包与令牌不一致（T6b 复核 MEDIUM-A）。
	ctx, cancel := opCtx()
	defer cancel()
	st, err := e.ledger.openHoldState(ctx, requestID)
	if err != nil {
		return fmt.Errorf("billing: read task holds for %s: %w", requestID, err)
	}
	s := &Session{
		engine:        e,
		requestID:     requestID,
		tokenID:       tokenID,
		userID:        st.UserID,
		preConsumed:   st.Held,
		tokenDeducted: st.Held,
		rc: &contract.RelayContext{
			RequestID: requestID,
			UserID:    int(st.UserID),
			TokenID:   int(tokenID),
		},
	}
	return s.Terminate(fact)
}

// terminateAtHeld 把 hold 按**预扣额**结清（cost = held，差额 0）。
//
// 这是 T6b 成功任务的默认路径（空 fact）：没有任务计价表达式时差额不可信，
// 但 hold 必须释放，否则 Detach 后 hold 无租约、补偿扫描也看不到，用户余额
// 会被永久冻结。
//
// 为什么不能复用 Session.Terminate：重建会话没有冻结的价格快照，Terminate
// 会按当前定价重新 Resolve（模型为空直接失败；按 token/倍率模式则把空用量
// 算成 0 并全额退款——成功任务变免费服务）。任务成功且没有计价表达式时，
// 唯一可信的金额就是当初预扣的 hold；这里显式以它为最终成本。
//
// tokenID 只用于 usage_logs 归属；金额不变 ⇒ 令牌额度无需调整。
//
// 顺序沿用 Terminate 的裁决：先写 usage_logs、后主库结算。日志失败则不结算
// （hold 保留，补偿扫描会重试），钱不能因为日志库抖动被静默吞掉。
func (e *Engine) terminateAtHeld(requestID string, tokenID int64, fact contract.UsageFact) error {
	ctx, cancel := opCtx()
	defer cancel()

	st, err := e.ledger.openHoldState(ctx, requestID)
	if err != nil {
		return fmt.Errorf("billing: read task holds for %s: %w", requestID, err)
	}
	if st.Held == 0 {
		// 已结算/已退款（closed）或本来就没有 hold：幂等 no-op。
		return nil
	}
	cost := st.Held
	outcome := SettlementOutcome{
		Cost:        cost,
		PreDeducted: cost,
		Refunded:    0,
		Status:      UsageLogStatusSuccess,
	}
	rc := &contract.RelayContext{
		RequestID: requestID,
		UserID:    int(st.UserID),
		TokenID:   int(tokenID),
	}
	// fact 本身为空：usage_logs 的 pre_deducted/cost 由 outcome 承载，
	// usage 字段保持零值（没有可信的上游用量事实）。
	if e.saturator != nil {
		if err := e.saturator.Saturate(ctx, &fact, rc, outcome); err != nil {
			return fmt.Errorf("billing: write usage log for task settlement: %w", err)
		}
	}
	_, _, err = e.ledger.settleHolds(ctx, st.UserID, requestID, 0, cost)
	if errors.Is(err, errHoldClosed) {
		// 并发结算/worker 兜底已关闭 hold：日志写入按 requestID 幂等，
		// 这里按幂等命中处理。
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing: settle task holds: %w", err)
	}
	return nil
}

// usageIsZero 判断用量事实是否全零（空 fact 的判据）。
//
// 只检查计费数字段；Source/关系布尔/Raw 不参与：调用方只可能是任务成功
// 的「按预扣额终结」路径，传入的是零值 UsageFact。
func usageIsZero(u contract.Usage) bool {
	return u.Prompt == 0 && u.Completion == 0 && u.Total == 0 &&
		u.CacheRead == 0 && u.CacheWrite == 0 && u.Reasoning == 0 &&
		u.AudioSeconds == 0 && u.Reserved == 0
}

// RefundByRequestID 在任务失败/超时时全额退款（T6b 调用）。
//
// Deprecated: 任务路径必须使用 RefundByRequestIDForToken。无令牌版本
// 退款只回补钱包，PreConsume 扣掉的令牌额度不会退回（T6b 复核 H1）。
// 保留仅为兼容既有测试与内部调用。
func (e *Engine) RefundByRequestID(requestID string) error {
	return e.RefundByRequestIDForToken(requestID, 0)
}

// RefundByRequestIDForToken 是任务退款/超时的 token-aware 入口。
//
// 两条资金来源分别处理（S4a H1）：
//   - 订阅来源没有 wallet_transactions hold 行，按 requestID 查
//     subscription_pre_consume_records：存在且未退款时回退订阅额度并回补
//     令牌，退款走与同步 Refund 相同的 3 次退避重试；
//   - 无订阅记录时维持原有钱包路径（从账本水合 hold）。
//
// 钱包路径继续走统一的 Refund：不复制退款逻辑，退款日志/令牌回补/幂等锚点
// （release 唯一索引 + 结案标记）都保持单一实现。
// 已结算/已退款/无 hold 重复调用返回 nil。
func (e *Engine) RefundByRequestIDForToken(requestID string, tokenID int64) error {
	if requestID == "" {
		return ErrMissingRequestID
	}
	ctx, cancel := opCtx()
	defer cancel()

	// H1：异步任务失败/超时的订阅退款。记录存在即说明本次请求由订阅预扣
	// （资金来源每请求冻结，不会同时有钱包 hold）。
	if e.subscriptionFunding != nil {
		rec, ownerID, recErr := e.subscriptionFunding.PreConsumeRecord(ctx, requestID)
		switch {
		case recErr == nil:
			if rec.Status == "refunded" {
				// 已退款：幂等命中。绝不能再次 MarkPreConsumeRefunded 之外的
				// 任何回补（仓储侧本身也幂等，但令牌回补必须跳过）。
				return nil
			}
			s := &Session{
				engine:        e,
				requestID:     requestID,
				userID:        ownerID,
				tokenID:       tokenID,
				fundingSource: contract.FundingSubscription,
				rc: &contract.RelayContext{
					RequestID: requestID,
					UserID:    int(ownerID),
					TokenID:   int(tokenID),
				},
				// 水合订阅预扣与对应的令牌扣减：Refund 的早返回条件看
				// preConsumed，CreditToken 的条件看 tokenDeducted。两者在
				// chargeSubscriptionLocked 中是同一金额。
				subscriptionID:          rec.SubscriptionID,
				subscriptionPreConsumed: rec.PreConsumed,
				preConsumed:             rec.PreConsumed,
				tokenDeducted:           rec.PreConsumed,
			}
			return s.Refund()
		case errors.Is(recErr, store.ErrPreConsumeNotFound):
			// 不是订阅来源：继续走钱包路径。
		default:
			return fmt.Errorf("billing: read subscription pre-consume for %s: %w", requestID, recErr)
		}
	}

	st, err := e.ledger.openHoldState(ctx, requestID)
	if err != nil {
		return fmt.Errorf("billing: read task holds for %s: %w", requestID, err)
	}
	s := &Session{
		engine:    e,
		requestID: requestID,
		rc: &contract.RelayContext{
			RequestID: requestID,
			UserID:    int(st.UserID),
			TokenID:   int(tokenID),
		},
		userID:  st.UserID,
		tokenID: tokenID,
		// 水合 hold 与令牌扣减：Refund 的早返回条件看内存 preConsumed，
		// CreditToken 的条件看 tokenDeducted；账本行才是权威。
		preConsumed:   st.Held,
		tokenDeducted: st.Held,
	}
	return s.Refund()
}
