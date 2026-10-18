package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"newtg/gateway/internal/contract"
)

// RecoverAbandonedByID 是崩溃兜底的引擎入口（worker/T9 调用）。
//
// 会话对象是请求作用域、持有内存态（Reserved/trusted/consumed），进程崩溃
// 后无法复原；因此这里不重建 Session，而是按 billing-00 §6.2 冻结的持久化
// 状态面从数据库行复原：
//
//   - 钱包路径（M3 唯一资金来源）：wallet_transactions 中
//     type='consume_pre' AND status='hold' AND reference_id=requestID。
//     金额与 userID 都来自这些行，不信任任何内存值。
//   - 订阅预扣记录：属 M5（billing-08），M3 只留接缝、不实现（R-14）。
//   - 用量日志占位：在钱包退款后经 Saturator（Task 6 的 DBLogger）写
//     status=refunded 的占位/收敛行。必须走 Saturate 而不是直接 SQL：
//     它用 request_id 事务级咨询锁串行化与 Terminate 的并发写，并保证
//     R-25 的单向收敛（已有 success 行只能收敛为 refunded，绝不反向覆盖）。
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
// 【R-88】由租约携带的 tokenID 回补令牌额度：钱包 hold 退回后，把**同一
// hold 金额**加回 api_tokens.remain_quota（NULL 无限额/令牌已删除 → no-op）。
// ChargeIncrement 的增量仍保持已扣（R-39），所以只回补 held，不回补 consumed。
//
// 【失败语义】DB 错误原样上抛，worker 记日志并在下一轮重试；绝不把
// 「数据库暂不可用」当成「已退款」。退款成功后才写日志占位、再 Remove
// 租约；日志/Redis 失败也返回错误，允许下一轮重扫（退款本身幂等，不会
// 重复退钱；日志占位按 request_id 幂等，也不会产生第二行）。
func (e *Engine) RecoverAbandonedByID(ctx context.Context, requestID string, tokenID int64) error {
	if requestID == "" {
		return ErrMissingRequestID
	}

	// 单次回收使用有界上下文：worker 的循环不能因为一次数据库挂起而
	// 整轮卡死（与会话内 billingOpTimeout 同口径）。
	opCtx, cancel := context.WithTimeout(ctx, billingOpTimeout)
	defer cancel()

	held, _, err := e.ledger.releaseAbandoned(opCtx, requestID)
	switch {
	case errors.Is(err, errHoldClosed):
		// 预扣行已处于 settled/released/expired：正常结算或此前的兜底已经
		// 处理过。不得再退，也不得改写已关闭的状态；下面的日志收敛只对
		// 「expired（兜底回收）」事实生效。
		err = nil
	case errors.Is(err, errNoOpenHold):
		// 完全没有 consume_pre 行（信任旁路/零预算/从未预扣）：没有可退的
		// 冻结资金。ChargeIncrement 的 consume 行不属于兜底退款范围（R-39）。
		err = nil
	case err != nil:
		return fmt.Errorf("billing: recover abandoned request %s: %w", requestID, err)
	}

	// R-88：钱包 hold 已退回；把同一 hold 金额回补到令牌额度。
	// best-effort：令牌侧失败只记日志，不能让已经完成的钱包退款变成错误
	// （否则 worker 会重扫，但钱包已幂等 no-op，令牌却永远补不上）。
	if held > 0 && tokenID > 0 {
		userID, found, ownerErr := e.ledger.holdOwner(opCtx, requestID)
		switch {
		case ownerErr != nil:
			slog.Warn("billing: read hold owner for token restore failed",
				"request_id", requestID, "token_id", tokenID, "error", ownerErr)
		case found:
			if tokenErr := e.ledger.AdjustTokenQuota(opCtx, userID, tokenID, held); tokenErr != nil {
				slog.Warn("billing: token quota restore failed after abandoned recovery",
					"request_id", requestID, "token_id", tokenID, "amount", held, "error", tokenErr)
			}
		}
	}

	// 日志占位必须在 Remove 之前：写失败时保留租约索引，下一轮重试；
	// 若先 Remove，日志库瞬断会让退款事实永久缺行。
	// 仅有 expired 预扣行时才写（正常 settled/released 不是兜底退款）。
	if e.saturator != nil {
		if lerr := e.writeAbandonedRefundLog(opCtx, requestID); lerr != nil {
			return lerr
		}
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

// writeAbandonedRefundLog 把「已被兜底回收」的事实写成 usage_logs.refunded。
//
// 数据来源固定为主库持久化状态：status='expired' 的预扣行（releaseAbandoned
// 的产物）。这同时覆盖重试路径：首轮钱已退但日志库失败时，expired 行仍在，
// 下一轮会重新调用 Saturate；Saturate 按 request_id 幂等（已有行则收敛）。
//
// 若不存在 expired 预扣行（正常结算 settled、同步退款 released、或本来
// 没有 hold），本函数不写任何日志——worker 不得把一次正常结算改写成退款。
func (e *Engine) writeAbandonedRefundLog(ctx context.Context, requestID string) error {
	rows, err := e.db.WithContext(ctx).Raw(
		`SELECT user_id, COALESCE(SUM(-amount), 0)
		   FROM wallet_transactions
		  WHERE reference_type = 'usage_log' AND reference_id = ?
		    AND type = 'consume_pre' AND status = 'expired'
		  GROUP BY user_id`, requestID).Rows()
	if err != nil {
		return fmt.Errorf("billing: read abandoned refund rows for %s: %w", requestID, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		userID   int64
		refunded int64
		found    int
	)
	for rows.Next() {
		var uid, amount int64
		if err := rows.Scan(&uid, &amount); err != nil {
			return fmt.Errorf("billing: scan abandoned refund rows for %s: %w", requestID, err)
		}
		userID, refunded = uid, amount
		found++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("billing: iterate abandoned refund rows for %s: %w", requestID, err)
	}
	if found == 0 {
		return nil
	}
	if found > 1 {
		// 同一 requestID 的预扣行跨用户是数据损坏；继续写日志会把用户归属
		// 记错，必须暴露而不是任选一行。
		return fmt.Errorf("billing: expired hold rows for %s span multiple users", requestID)
	}

	// R-55/R-59：兜底只退剩余 hold（R-39），ChargeIncrement 已扣的增量
	// 保留不退。日志必须与钱包一致——status=refunded 时 cost 记录**保留的
	// 消耗**、refunded 记录退回的 hold；cost=0 只适用于纯全额退款。
	// 增量行是 type='consume' 的 confirmed 流水（结算少补行在崩溃兜底场景
	// 不会同时存在：worker 先回收时 Terminate 看到已关闭的 hold 不再结算）。
	var retained int64
	if err := e.db.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(-amount), 0) FROM wallet_transactions
		  WHERE reference_type = 'usage_log' AND reference_id = ?
		    AND type = 'consume'`, requestID).Row().Scan(&retained); err != nil {
		return fmt.Errorf("billing: read retained consumption for %s: %w", requestID, err)
	}
	if retained < 0 {
		// 数据损坏（consume 行金额为正）；DBLogger 也会拒绝负金额。
		return fmt.Errorf("billing: negative retained consumption %d for %s", retained, requestID)
	}

	// worker 没有原始 RelayContext：requestID 与 userID 是持久化状态面里
	// 可靠的部分，模型/渠道/令牌等列写 NULL（DBLogger 对零值即 NULL）。
	rc := &contract.RelayContext{
		RequestID: requestID,
		UserID:    int(userID),
	}
	outcome := SettlementOutcome{
		Cost:         retained,
		PreDeducted:  refunded,
		Refunded:     refunded,
		RetainedCost: retained,
		Status:       UsageLogStatusRefunded,
		ErrorClass:   "abandoned",
		ErrorMessage: "request abandoned; hold refunded by worker",
	}
	if err := e.saturator.Saturate(ctx, nil, rc, outcome); err != nil {
		return fmt.Errorf("billing: write abandoned refund log for %s: %w", requestID, err)
	}
	return nil
}
