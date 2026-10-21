package billing

import (
	"errors"
	"fmt"
	"log/slog"

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
	return e.terminateByRequestID(requestID, tokenID, nil, fact)
}

// TerminateByRequestIDForTokenCost 按**已求值**的任务成本结算（T5）。
//
// 成本来自提交期冻结的任务计价表达式（u("字段") 求值后的积分）。与
// TerminateByRequestIDForToken 的差别仅在于「不重算价格」：fact 只用于
// 日志归属，金额由 cost 承载。其余路径（订阅识别、结算锁、hold CAS、
// 令牌额度调整、多退少补）完全复用，保证实时/轮询/补偿三条路径同一语义。
//
// 负成本拒绝：负值会成为抵扣（反向入账），表达式求值层已拒绝负结果，
// 这里是引擎边界的第二道防线。
func (e *Engine) TerminateByRequestIDForTokenCost(requestID string, tokenID, cost int64, fact contract.UsageFact) error {
	if cost < 0 {
		return fmt.Errorf("billing: task settlement cost must be non-negative (got %d)", cost)
	}
	return e.terminateByRequestID(requestID, tokenID, &cost, fact)
}

// terminateByRequestID 是 ByRequestID 结算的共同实现（钱包 + 订阅两条资金路径）。
//
// cost 为 nil 表示按既有语义（空 fact = 按预扣额结清；非空 fact = 按模型
// 快照重算）；非 nil 表示使用调用方给定的冻结成本。
func (e *Engine) terminateByRequestID(requestID string, tokenID int64, cost *int64, fact contract.UsageFact) error {
	if requestID == "" {
		return ErrMissingRequestID
	}

	// S4b：订阅来源的任务/崩溃恢复结算。订阅预扣没有 wallet hold 行，
	// 若继续走下面的钱包水合，空 fact 会直接 no-op（预扣记录永远停留在
	// consumed，最终被兜底扫描误退），非空 fact 则会走「无 hold 补扣」把
	// 成本再向钱包收一次。因此必须先识别订阅来源并按订阅语义重建会话。
	if e.subscriptionFunding != nil {
		ctx, cancel := opCtx()
		defer cancel()
		rec, ownerID, recErr := e.subscriptionFunding.PreConsumeRecord(ctx, requestID)
		switch {
		case recErr == nil:
			if rec.Status == "refunded" {
				// 已退款：终态互斥，不得再结算（否则同一请求双花）。
				return nil
			}
			// 任务轮询器可能并发/重复调用。订阅没有 hold CAS 锚点，用
			// requestID 结算锁 + request_registry 终态复核双重幂等：
			// 已 settled/refunded 说明另一个执行者已经收尾。
			release, lockErr := e.acquireSettlementLock(ctx, requestID)
			if lockErr != nil {
				return lockErr
			}
			defer release()
			state, stateErr := e.subscriptionFunding.RequestRegistryState(ctx, requestID)
			if stateErr != nil {
				return fmt.Errorf("billing: read settlement state for %s: %w", requestID, stateErr)
			}
			if state == "settled" || state == "refunded" {
				return nil
			}
			// 同步退款路径（S4a Refund）不写 registry，但会把记录置 refunded；
			// 持锁后复核一次，避免「先退款、后结算」把已退额度再消耗一遍。
			if latest, _, latestErr := e.subscriptionFunding.PreConsumeRecord(ctx, requestID); latestErr != nil {
				return fmt.Errorf("billing: re-read subscription pre-consume for %s: %w", requestID, latestErr)
			} else if latest.Status == "refunded" {
				return nil
			}
			// 任务/恢复路径的成功结算日志也带 billing_preference（LOW）：
			// 重建会话没有 PreConsume 的 fundingPlan，偏好在这里读取后冻结。
			// 读失败不影响资金正确性，日志省略该键即可。
			prefText := ""
			if pref, prefErr := e.subscriptionFunding.BillingPreference(ctx, ownerID); prefErr != nil {
				slog.Warn("billing: read billing preference for task settlement log failed",
					"request_id", requestID, "error", prefErr)
			} else if text, textErr := store.BillingPreferenceText(pref); textErr == nil {
				prefText = text
			}
			s := &Session{
				engine:        e,
				requestID:     requestID,
				userID:        ownerID,
				tokenID:       tokenID,
				fundingSource: contract.FundingSubscription,
				// 锁已在上方获取：会话内的订阅结算不得二次获取（Redis
				// SETNX 不可重入）。
				settlementLockHeld:    true,
				billingPreferenceText: prefText,
				rc: &contract.RelayContext{
					RequestID: requestID,
					UserID:    int(ownerID),
					TokenID:   int(tokenID),
				},
				// 水合订阅预扣与对应的令牌扣减；结算差额以它们为「已扣额」。
				subscriptionID:          rec.SubscriptionID,
				subscriptionPreConsumed: rec.PreConsumed,
				preConsumed:             rec.PreConsumed,
				tokenDeducted:           rec.PreConsumed,
				// T5：冻结成本优先于快照重算。
				forcedCost: cost,
			}
			// 空 fact 由 terminateSubscriptionLocked 按「预扣额即成本」结算
			// （delta=0），不需要钱包的 terminateAtHeld 分支。forcedCost 非
			// nil 时同函数改用冻结成本。
			return s.Terminate(fact)
		case errors.Is(recErr, store.ErrPreConsumeNotFound):
			// 不是订阅来源：继续钱包路径。
		default:
			return fmt.Errorf("billing: read subscription pre-consume for %s: %w", requestID, recErr)
		}
	}

	if cost == nil && fact.Model == "" && usageIsZero(fact.Usage) {
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
		// T5：任务表达式成本（nil = 按 fact 重算）。
		forcedCost: cost,
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
			// H1：先取 requestID 结算锁，**锁内**再读 registry。锁外预检
			// 存在「读到未结算 → 结算方收尾 → 本方法继续退款」的双花窗口；
			// 锁内复核把窗口关闭。settled/refunded 都是终态，直接返回 nil。
			release, lockErr := e.acquireSettlementLock(ctx, requestID)
			if lockErr != nil {
				return lockErr
			}
			defer release()
			state, stateErr := e.subscriptionFunding.RequestRegistryState(ctx, requestID)
			if stateErr != nil {
				return fmt.Errorf("billing: read settlement state for %s: %w", requestID, stateErr)
			}
			if state == "settled" || state == "refunded" {
				return nil
			}
			// 第二道：预扣记录复核。同步退款路径（S4a Refund）不写 registry，
			// 但会把记录置 refunded；持锁后复核一次，避免「先退款、后任务
			// 失败回调」把同一笔额度再退一遍。
			if latest, _, latestErr := e.subscriptionFunding.PreConsumeRecord(ctx, requestID); latestErr != nil {
				return fmt.Errorf("billing: re-read subscription pre-consume for %s: %w", requestID, latestErr)
			} else if latest.Status == "refunded" {
				return nil
			}
			s := &Session{
				engine:        e,
				requestID:     requestID,
				userID:        ownerID,
				tokenID:       tokenID,
				fundingSource: contract.FundingSubscription,
				// 锁已由本方法持有：会话内的订阅退款不得二次获取。
				settlementLockHeld: true,
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
			if err := s.Refund(); err != nil {
				return err
			}
			// 记录退款终态（幂等；已 settled 不降级）。失败只告警：预扣记录
			// 已是 refunded，兜底扫描同样会排除它，不影响资金正确性。
			if markErr := e.subscriptionFunding.MarkRequestRefunded(ctx, requestID, ownerID); markErr != nil {
				slog.Warn("billing: record subscription refund state failed",
					"request_id", requestID, "error", markErr)
			}
			return nil
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
