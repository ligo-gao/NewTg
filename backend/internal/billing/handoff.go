package billing

import (
	"errors"
	"fmt"

	"newtg/gateway/internal/contract"
)

// 本文件是任务平台的**按 requestID 结算入口**（contract.BillingHandoff 的
// 引擎侧）。任务提交返回后原 Session 已 Detach（心跳停止、hold 保留），
// 轮询器只有持久化的 requestID；如果把 Session 对象一起带下去，轮询器就
// 必须持有进程内状态——重启即失效。因此这里从账本行重建最小会话：
// requestID 是锚点，userID / hold 来自 wallet_transactions 的 hold 行。

// TerminateByRequestID 在任务终态时结算（T6b 调用）。
//
// 两种语义：
//
//  1. **空 fact（Model 为空且用量全零）= 按预扣额终结**。这是 T6b 的成功
//     任务默认路径：没有任务计价表达式时，差额不可信，但 hold 必须释放，
//     否则 Detach 后 hold 无租约、补偿扫描也看不到，用户余额会被永久冻结。
//     该路径**不解析定价**（不按 token 重算），直接把 hold 结清为 settled、
//     金额不变（delta=0），并写 success 日志。
//  2. 非空 fact：与 Session.Terminate 完全一致——按 fact 求实际成本、释放
//     hold、多退少补、写 usage_logs。
//
// 幂等：已结算/已退款重复调用返回 nil。
func (e *Engine) TerminateByRequestID(requestID string, fact contract.UsageFact) error {
	if requestID == "" {
		return ErrMissingRequestID
	}
	if fact.Model == "" && usageIsZero(fact.Usage) {
		return e.terminateAtHeld(requestID, fact)
	}
	// rc 只承载持久化事实：requestID 是日志幂等键，UserID 由 Terminate 从
	// hold 行回填（见 session.go 的 ByRequestID 注释）。
	s := &Session{
		engine:    e,
		requestID: requestID,
		rc:        &contract.RelayContext{RequestID: requestID},
	}
	return s.Terminate(fact)
}

// terminateAtHeld 把 hold 按**预扣额**结清（cost = held，差额 0）。
//
// 为什么不能复用 Session.Terminate：重建会话没有冻结的价格快照，Terminate
// 会按当前定价重新 Resolve（模型为空直接失败；按 token/倍率模式则把空用量
// 算成 0 并全额退款——成功任务变免费服务）。任务成功且没有计价表达式时，
// 唯一可信的金额就是当初预扣的 hold；这里显式以它为最终成本。
//
// 顺序沿用 Terminate 的裁决：先写 usage_logs、后主库结算。日志失败则不结算
// （hold 保留，补偿扫描会重试），钱不能因为日志库抖动被静默吞掉。
func (e *Engine) terminateAtHeld(requestID string, fact contract.UsageFact) error {
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
	rc := &contract.RelayContext{RequestID: requestID, UserID: int(st.UserID)}
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
// 从账本读出仍处于 hold 的金额并水合到临时会话，再走统一的 Refund 路径
// ——不复制退款逻辑，退款日志/令牌回补/幂等锚点都保持单一实现。
// 已结算/已退款/无 hold 重复调用返回 nil。
func (e *Engine) RefundByRequestID(requestID string) error {
	if requestID == "" {
		return ErrMissingRequestID
	}
	ctx, cancel := opCtx()
	defer cancel()

	st, err := e.ledger.openHoldState(ctx, requestID)
	if err != nil {
		return fmt.Errorf("billing: read task holds for %s: %w", requestID, err)
	}
	s := &Session{
		engine:    e,
		requestID: requestID,
		rc:        &contract.RelayContext{RequestID: requestID, UserID: int(st.UserID)},
		userID:    st.UserID,
		// 水合 hold：Refund 的早返回条件看内存 preConsumed；账本行才是权威。
		preConsumed: st.Held,
	}
	return s.Refund()
}
