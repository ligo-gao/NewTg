package billing

// refund.go —— 退款金额学（Refund 专用）。
//
// 会话状态机（Refund/NeedsRefund 的早返回）在 session.go，原子退款事务
// （关 hold + 一行 release）在 ledger.go 的 refundSession；本文件只放
// 退款专用的金额助手，把「不超扣」的推理集中在一处：
//   - 退款额不得大于**持久化事实上已扣减**的金额。会话内存的 consumed 只是
//     累计缓存，崩溃/状态损坏/同 requestID 的异常双会话都可能让它与账本
//     不一致；按内存值退款会放大余额（brief 的「不超扣」）。
//   - 异常输入（负数、int64 溢出）必须钳制/拒绝并 warn：资金面修正若静默
//     发生，对账时无法解释差异，也掩盖了调用方的状态缺陷。
//
// 设计权威：billing-00 §6.2（Refund 全额退款、幂等）与 billing-03 §2.4
// （幂等保证汇编）、§6.2（基线钱包退款「无幂等保护、不可重试」——新系统
// 以数据库锚点替代该乙类怪癖）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// errRefundOverflow 表示退款额（held + consumed）越过 int64。
//
// 溢出会回绕为负数：若继续按「没有可退金额」处理，Refund 会被当作幂等命中
// 并把会话置为已退款，形成「状态已退、钱未回补」的空洞。宁可报错让调用方
// 重试或转人工核账。
var errRefundOverflow = errors.New("billing: refund amount overflows int64")

// isUniqueViolation 识别 PostgreSQL 唯一约束冲突（SQLSTATE 23505）。
//
// 退款事务里唯一的 INSERT 是 release 流水；wallet_release_refid_uq 是
// `(reference_id) WHERE type='release'` 的部分唯一索引——同一 requestID
// 只允许一行 release。并发退款争抢时，后提交者的 INSERT 会撞上该索引，
// 这恰恰证明资金已被先提交者回补（见 refundSession 的幂等锚点）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// refundableConsumed 返回本次退款可以回补的增量扣减额。
//
// 上界是**数据库**中该 requestID 的 consume 流水合计（persistedConsumed）：
//   - memory < 0：异常输入，钳制为 0 并 warn（负退款会变成扣款方向）；
//   - memory > 持久化值：钳制到持久化值并 warn（绝不放大余额）；
//   - memory ≤ 持久化值：采信 memory。持久化值更大意味着账本里还有本会话
//     不知道的增量扣减（同 requestID 的异常双会话），退回它们会越过本会话的
//     预扣事实；保守方向是不多退。
func refundableConsumed(ctx context.Context, tx *gorm.DB, requestID string, userID, memory int64) (int64, error) {
	if memory < 0 {
		slog.Warn("billing: refund consumed is negative, clamped to zero",
			"request_id", requestID, "consumed", memory)
		return 0, nil
	}
	if memory == 0 {
		// 常见失败路径没有增量扣减：省掉一次 SUM 查询。
		return 0, nil
	}
	persisted, err := persistedConsumed(ctx, tx, requestID, userID)
	if err != nil {
		return 0, err
	}
	if memory > persisted {
		slog.Warn("billing: refund consumed exceeds persisted charges, clamped",
			"request_id", requestID, "consumed", memory, "persisted", persisted)
		return persisted, nil
	}
	return memory, nil
}

// persistedConsumed 汇总该 requestID 已实际扣减的增量积分（type='consume'）。
//
// 同时校验这些行的 user_id：同一 requestID 的扣减流水必须属于同一用户，
// 否则数据已损坏——把别人的扣减退到本用户钱包会直接放大余额（billing-03
// §2.4「钱包退款无幂等保护」的另一面：任何不确定都必须 fail-closed）。
func persistedConsumed(ctx context.Context, tx *gorm.DB, requestID string, userID int64) (int64, error) {
	var (
		total    int64
		users    int64
		onlyUser int64
	)
	row := tx.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(-amount), 0), COUNT(DISTINCT user_id), COALESCE(MIN(user_id), 0)
		 FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ? AND type = 'consume'`,
		requestID).Row()
	if err := row.Scan(&total, &users, &onlyUser); err != nil {
		return 0, fmt.Errorf("billing: sum consumed charges for %s: %w", requestID, err)
	}
	if users > 1 || (users == 1 && onlyUser != userID) {
		return 0, fmt.Errorf("billing: consume rows for %s do not all belong to user %d", requestID, userID)
	}
	if total < 0 {
		// consume 流水的 amount 必须为负；出现正数和说明账本被外部写坏。
		return 0, fmt.Errorf("billing: negative consumed total for %s", requestID)
	}
	return total, nil
}

// refundTotal 计算退款入账额（held + consumed），并拒绝 int64 溢出。
//
// held 来自数据库 hold 行（权威），consumed 已经过 refundableConsumed 钳制。
// 非负校验放在这里而不是只依赖调用方的注释：负数会反转资金方向。
func refundTotal(held, consumed int64) (int64, error) {
	if held < 0 || consumed < 0 {
		return 0, fmt.Errorf("%w: held=%d consumed=%d", errRefundOverflow, held, consumed)
	}
	if held > math.MaxInt64-consumed {
		return 0, fmt.Errorf("%w: held=%d consumed=%d", errRefundOverflow, held, consumed)
	}
	return held + consumed, nil
}
