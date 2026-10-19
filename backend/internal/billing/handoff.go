package billing

import (
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
// 语义与 Session.Terminate 完全一致：按 fact 求实际成本、释放 hold、多退
// 少补、写 usage_logs。幂等：已结算/已退款重复调用返回 nil。
func (e *Engine) TerminateByRequestID(requestID string, fact contract.UsageFact) error {
	if requestID == "" {
		return ErrMissingRequestID
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
