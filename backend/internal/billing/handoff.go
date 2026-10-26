package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/store"
)

// RequestPricingStore 是 request_pricing 的读写缝（spec §5.2）。
//
// *store.RequestPricingRepo 是生产实现；单测用内存 fake。方法集合只包含
// 计费需要的三个操作：预扣写、结算读、周期清理。
type RequestPricingStore interface {
	Upsert(ctx context.Context, rec store.RequestPricing) error
	Get(ctx context.Context, requestID string) (store.RequestPricing, bool, error)
	DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

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
			if needsFrozenPricing(cost, fact) {
				if _, err := e.loadFrozenPricing(ctx, requestID, s); err != nil {
					return err
				}
			}
			// 冻结折扣独立于定价加载：冻结成本（cost != nil）路径不读时间/
			// 倍率，但折扣仍必须复用预扣时的值，否则按原价追回差额。
			if err := e.loadFrozenDiscount(ctx, requestID, s); err != nil {
				slog.Warn("billing: load frozen discount failed; settling without discount",
					"request_id", requestID, "error", err)
			}
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
	// 非空 fact 会按模型快照重算：时段倍率必须来自预扣时的冻结事实，
	// 不能按结算时刻重新匹配窗口（spec §5.2）。读回失败 fail-closed。
	if needsFrozenPricing(cost, fact) {
		if _, err := e.loadFrozenPricing(ctx, requestID, s); err != nil {
			return err
		}
	}
	// 冻结折扣独立于定价加载：冻结成本与非空 fact 两条路径都必须复用预扣
	// 时冻结的折扣，否则预扣已折、结算按原价，差额会被追回。
	if err := e.loadFrozenDiscount(ctx, requestID, s); err != nil {
		slog.Warn("billing: load frozen discount failed; settling without discount",
			"request_id", requestID, "error", err)
	}
	return s.Terminate(fact)
}

// needsFrozenPricing 判断重建的会话是否会解析价格快照。
//
// 只有「需要按 fact 重算」的路径才依赖 request_pricing；按预扣额结清
// （terminateAtHeld / 订阅 delta=0）与冻结成本不读定价，缺行不应阻塞
// 一个纯资金收尾操作。
func needsFrozenPricing(cost *int64, fact contract.UsageFact) bool {
	if cost != nil {
		return false
	}
	return fact.Model != "" || !usageIsZero(fact.Usage)
}

// loadFrozenPricing 把 request_pricing 的冻结时间与倍率写入重建的会话。
//
// 【失败方向】读表错误返回 error（fail-closed）：用 time.Now() 兜底会让
// 跨整点/预扣后改策略的请求按另一套价结算，比拒绝结算更危险。缺行是另一
// 回事——本功能上线前的在飞请求没有记录，按既有行为继续并 WARN（plan
// Task 4「if absent, fall back to the existing behavior and log a WARN」）。
func (e *Engine) loadFrozenPricing(ctx context.Context, requestID string, s *Session) (bool, error) {
	if e.requestPricing == nil || s == nil {
		return false, nil
	}
	// 非 UUID 的 requestID 不可能有记录（persistRequestPricing 同样跳过）：
	// 按缺行处理，避免内部/演示 id 的结算被 uuid 校验永久卡住。
	if !store.ValidRequestPricingID(requestID) {
		slog.Warn("billing: request_id is not a UUID; no frozen request pricing available",
			"request_id", requestID)
		return false, nil
	}
	rec, ok, err := e.requestPricing.Get(ctx, requestID)
	if err != nil {
		return false, fmt.Errorf("billing: read frozen request pricing for %s: %w", requestID, err)
	}
	if !ok {
		slog.Warn("billing: request_pricing row missing; settling with current pricing",
			"request_id", requestID)
		return false, nil
	}
	s.requestTime = rec.RequestTime
	multiplier := rec.TimeMultiplier
	s.frozenMultiplier = &multiplier
	// 同一行顺带恢复冻结折扣，不重复查询：预扣与结算必须命中同一折扣。
	s.applyFrozenDiscount(rec.DiscountRate, rec.DiscountTarget)
	return true, nil
}

// loadFrozenDiscount 把 request_pricing 里冻结的客户折扣写入重建的会话。
//
// 与 loadFrozenPricing 分开：定价只影响「按 fact 重算」的路径，折扣则
// 影响**所有**按成本收敛的路径（含 forcedCost）。两者共用一个 requestID
// 锚点，但加载条件不同。
//
// 【失败方向】best-effort：读表错误只由调用方 WARN，不阻塞结算——折扣是
// 让利，不是资金收尾的前置条件；缺行（本功能上线前的在飞请求）按
// 「已解析、无折扣」继续，避免后续路径再查一次。非 UUID 的 requestID
// 不可能有行（persistRequestPricing 同样跳过），按缺行处理。
//
// 【不变量】重建会话只结算、不产生增量：handoff 只调用 Terminate /
// terminateAtHeld，从不调用 Reserve / ChargeIncrement，所以累计口径不会
// 从零重算。若将来有路径在重建会话上恢复增量，必须同时恢复 rawConsumed
// 与冻结折扣，否则累计折扣会从 0 重算并多收。rawConsumed 不落
// request_pricing（没有这一列，也不该加）。
func (e *Engine) loadFrozenDiscount(ctx context.Context, requestID string, s *Session) error {
	if e.requestPricing == nil || s == nil || s.discountResolved {
		return nil
	}
	if !store.ValidRequestPricingID(requestID) {
		slog.Warn("billing: request_id is not a UUID; no frozen discount available",
			"request_id", requestID)
		s.applyFrozenDiscount(1, "")
		return nil
	}
	rec, ok, err := e.requestPricing.Get(ctx, requestID)
	if err != nil {
		return fmt.Errorf("billing: read frozen discount for %s: %w", requestID, err)
	}
	if !ok {
		slog.Warn("billing: request_pricing row missing; settling without discount",
			"request_id", requestID)
		// Rate=1 + 空 Target 是「已解析、无折扣」的确定态：阻止后续路径再查，
		// 也与零值 {0,""}（未解析）区分。
		s.applyFrozenDiscount(1, "")
		return nil
	}
	s.applyFrozenDiscount(rec.DiscountRate, rec.DiscountTarget)
	return nil
}

// frozenDiscount 读取冻结折扣的纯查询版本（供不建会话的 settle-at-held 使用）。
//
// 与 loadFrozenDiscount 同一口径，但返回零值而不是写入会话：terminateAtHeld
// 直接以 hold 为成本，只需要折扣事实来补日志。任何读失败/缺行都返回零值
// （不写折扣键），绝不阻塞资金收尾。
func (e *Engine) frozenDiscount(ctx context.Context, requestID string) contract.Discount {
	if e.requestPricing == nil || !store.ValidRequestPricingID(requestID) {
		return contract.Discount{}
	}
	rec, ok, err := e.requestPricing.Get(ctx, requestID)
	if err != nil {
		slog.Warn("billing: load frozen discount failed; settling without discount",
			"request_id", requestID, "error", err)
		return contract.Discount{}
	}
	if !ok {
		return contract.Discount{}
	}
	return contract.Discount{Rate: rec.DiscountRate, Target: rec.DiscountTarget}
}

// RefreshInflightLeases 续租一批订阅请求的 in-flight 租约（R-W9B-02）。
//
// 任务提交时会话 Detach，流式心跳随之停止；之后由任务轮询器成为任务的
// 守护者。单轮轮询处理最多 1000 个任务、任务间隔 1s，整轮可能远超租约
// TTL，因此轮询器必须在处理每个任务前续租（并在轮批次量续租一次），
// 否则排队靠后的活跃任务会被兜底扫描当成崩溃退款，任务完成后再结算就
// 变成免费（G-DOM-35）。
//
// 非订阅来源/未装配订阅时返回 nil：钱包任务的在飞证据是 hold 行，由既有
// Redis 租约与账本回收路径处理，不受本方法影响。
//
// 续租失败只上抛错误供调用方告警；**不得**改变结算方向——租约过期只会
// 让扫描多一个候选，终态 CAS 仍是唯一仲裁者。
func (e *Engine) RefreshInflightLeases(ctx context.Context, requestIDs []string) error {
	if e.subscriptionFunding == nil || len(requestIDs) == 0 {
		return nil
	}
	if _, err := e.subscriptionFunding.RenewInflightLease(ctx, requestIDs); err != nil {
		return fmt.Errorf("billing: refresh subscription inflight leases: %w", err)
	}
	return nil
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

	// hold 已经是预扣时的折后金额；冻结折扣用于在日志里补出原价事实
	// （rawKnown=false → 按 net/rate 反推）。读不到折扣就不写折扣键。
	d := e.frozenDiscount(ctx, requestID)

	st, err := e.ledger.openHoldState(ctx, requestID)
	if err != nil {
		return fmt.Errorf("billing: read task holds for %s: %w", requestID, err)
	}
	if st.Held == 0 {
		// §3.17.2（控制端裁决 I1）：演示账号的提交阶段没有 hold，但 usage_logs
		// 仍要照常写，否则运维看不到演示站的「成本」。这里以 0 成本补一条成功
		// 日志；真正有计价表达式的任务会走 TerminateByRequestIDForTokenCost，
		// 由 Session 的免单分支写入真实成本。
		if !e.demoExemptToken(ctx, tokenID) {
			// 已结算/已退款（closed）或本来就没有 hold：幂等 no-op。
			return nil
		}
		outcome := SettlementOutcome{
			Cost: 0, PreDeducted: 0, Refunded: 0, Status: UsageLogStatusSuccess,
		}
		rc := &contract.RelayContext{
			RequestID: requestID,
			UserID:    int(st.UserID),
			TokenID:   int(tokenID),
		}
		if e.saturator != nil {
			if err := e.saturator.Saturate(ctx, &fact, rc, outcome); err != nil {
				return fmt.Errorf("billing: write usage log for demo task: %w", err)
			}
		}
		return nil
	}
	cost := st.Held
	// 按预扣额结清：cost 是折后金额，原价未知（rawKnown=false）时由
	// discountFacts 用 cost/rate 反推；全免（rate=0）只写 Target 不写原价。
	originalCost, discountRate, discountTarget := discountFacts(d, cost, cost, false)
	outcome := SettlementOutcome{
		Cost:           cost,
		PreDeducted:    cost,
		Refunded:       0,
		Status:         UsageLogStatusSuccess,
		OriginalCost:   originalCost,
		DiscountRate:   discountRate,
		DiscountTarget: discountTarget,
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
