package billing

import (
	"context"
	"errors"
	"fmt"
)

// RecoverAbandonedByID 是崩溃兜底的引擎入口（worker/T9 调用）。
//
// 会话对象是请求作用域、持有内存态（Reserved/trusted/consumed），进程崩溃
// 后无法复原；因此这里不重建 Session，而是按 billing-00 §6.2 冻结的持久化
// 状态面从数据库行复原：
//
//   - 钱包路径（M3 唯一实现）：wallet_transactions 中
//     type='consume_pre' AND status='hold' AND reference_id=requestID。
//     金额与 userID 都来自这些行，不信任任何内存值。
//   - 订阅预扣记录：属 M5（billing-08），M3 只留接缝、不实现（R-14）。
//   - usage_logs 占位：M3 没有日志库写入器（Task 6 的 DBLogger 尚未落地）；
//     该状态面是辅助判据，不影响钱包退款正确性（R-14）。
//
// 【R-39】只退剩余 hold。ChargeIncrement 写入的 consume 行对应已经交付给
// 用户的用量，不随崩溃退款；releaseAbandoned 只关闭 status='hold' 的
// consume_pre 行，恰好符合该裁决。
//
// 【幂等】releaseAbandoned 以 `status='hold'` 的条件 UPDATE 为 CAS 锚点：
// 重复扫描、worker 与同步 Refund 并发都不会产生第二行 release（数据库还有
// wallet_release_refid_uq 部分唯一索引兜底）。已 settled/refunded/无 hold
// 一律视为幂等命中，直接返回 nil。
//
// 【失败语义】DB 错误原样上抛，worker 记日志并在下一轮重试；绝不把
// 「数据库暂不可用」当成「已退款」。退款成功后才 Remove 租约项；Remove
// 失败也返回错误，允许下一轮重扫（退款本身幂等，不会重复退钱）。
func (e *Engine) RecoverAbandonedByID(ctx context.Context, requestID string) error {
	if requestID == "" {
		return ErrMissingRequestID
	}

	// 单次回收使用有界上下文：worker 的循环不能因为一次数据库挂起而
	// 整轮卡死（与会话内 billingOpTimeout 同口径）。
	opCtx, cancel := context.WithTimeout(ctx, billingOpTimeout)
	defer cancel()

	_, _, err := e.ledger.releaseAbandoned(opCtx, requestID)
	switch {
	case errors.Is(err, errHoldClosed):
		// 预扣行已处于 settled/released/expired：正常结算或此前的兜底已经
		// 处理过。不得再退，也不得改写已关闭的状态。
		err = nil
	case errors.Is(err, errNoOpenHold):
		// 完全没有 consume_pre 行（信任旁路/零预算/从未预扣）：没有可退的
		// 冻结资金。ChargeIncrement 的 consume 行不属于兜底退款范围（R-39）。
		err = nil
	case err != nil:
		return fmt.Errorf("billing: recover abandoned request %s: %w", requestID, err)
	}

	// 退款成功（或确认无需退款）后清理租约索引。失败时保留索引项，下一轮
	// 扫描会再次命中本函数；重复调用是幂等的，不会二次退款。
	if e.registry != nil {
		if rerr := e.registry.Remove(opCtx, requestID); rerr != nil {
			return fmt.Errorf("billing: remove abandoned lease %s: %w", requestID, rerr)
		}
	}
	return nil
}
