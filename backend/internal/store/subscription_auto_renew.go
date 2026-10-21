package store

// 订阅自动续费的数据层（S4；P-16 / P-17 / R-40 / R-42）。
//
// 【为什么这些不变量放在 store】
//  1. 「连续失败 3 次 → auto_renew=false」必须与 renewal_failed 事件原子提交：
//     否则会出现「开关已关但没有事件」或「有两次失败事件、计数却是 0」的
//     不可解释历史（与 S1 的事件同事务口径一致）。
//  2. 「令牌删除 → 关联订阅 fail-closed」同样必须同事务：删除令牌后若
//     关开关失败，worker 会拿着一个已经不存在的令牌反复尝试扣费（R-42）。
//  3. 扣费尝试/失败计数的写入带条件（status='active' AND auto_renew），
//     避免用户在扫描与扣费之间关闭自动续费后仍被写入状态。
//
// 【时间口径】所有比较使用调用方注入的 now（UTC）。续费窗口是
// end_at - lead，重试退避是 auto_renew_attempted_at + retry；跨时区进程
// 必须对「是否到窗」得到同一答案。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 自动续费冻结常量（P-16）。
const (
	// AutoRenewFailureThreshold 是「连续失败几次后关闭自动续费」。
	// 3 次是控制端裁决（P-16），不是可调参数：失败次数跨轮累计、成功清零。
	AutoRenewFailureThreshold = 3

	// AutoRenewScanLimit 是单轮扫描候选实例的上限。到期窗口内的实例通常
	// 很少；上限只防止异常数据（大量实例同时到期）让一轮扫描持有过多行。
	AutoRenewScanLimit = 200
)

// 自动续费/支付令牌的哨兵错误。handler 据此映射 404/409/422。
var (
	// ErrAutoRenewPaymentMethodRequired：启用自动续费却没有可用令牌（422）。
	ErrAutoRenewPaymentMethodRequired = errors.New("store: auto-renew requires a payment method")
	// ErrAutoRenewPaymentMethodInvalid：令牌不属于当前用户、已失效或格式非法（422）。
	ErrAutoRenewPaymentMethodInvalid = errors.New("store: payment method is not usable for auto-renew")
	// ErrPaymentMethodNotFound：令牌不存在或不属于当前用户（404，统一口径
	// 不泄露他人令牌 id 的存在性）。
	ErrPaymentMethodNotFound = errors.New("store: payment method not found")
	// ErrPaymentMethodInvalid：令牌字段非法（422）。
	ErrPaymentMethodInvalid = errors.New("store: payment method input is invalid")
)

// AutoRenewFailureState 是一次失败记录的结果（供 worker 日志/通知使用）。
type AutoRenewFailureState struct {
	// Failures 是本次失败后的连续失败次数。
	Failures int32
	// Disabled 表示本次失败让 auto_renew 从 true 变为 false（P-16 阈值触发）。
	Disabled bool
}

// ListDueForAutoRenew 返回进入续费窗口且允许扣费的实例。
//
// 条件（R-40 / P-17）：
//   - status='active'：终态（expired/cancelled）不自动续费；宽限期的
//     expired 也不在候选内——B12 已按冻结语义结束合同，恢复应走用户显式续费。
//   - auto_renew=true 且未被 cancel_at_period_end 覆盖：用户明确不续费时
//     一律跳过（R-40）。
//   - auto_renew_payment_method_id 非空：没有令牌就没有扣费手段，绝不退回
//     钱包/其它渠道（P-14 只支持 Stripe 令牌）。
//   - end_at <= now + lead：续费窗口 = end_at - lead（R-40）。
//   - 最近一次尝试早于 now - retry：失败重试退避。没有它，每分钟扫描会在
//     一小时内把 3 次机会全部用光，用户来不及换卡就被关闭自动续费。
//
// 【为什么成功续费后不会立刻再扫到】扣费成功会在同一事务里推进 end_at，
// 新的窗口起点随之后移；attempted_at 不参与「本周期已扣过」的判定，避免
// 把「成功时刻」误当成「本周期已履约」而漏掉下一周期。
func (r *SQLSubscriptionRepo) ListDueForAutoRenew(ctx context.Context, now time.Time,
	lead, retry time.Duration, limit int) ([]Subscription, error) {

	if lead < 0 || retry < 0 {
		return nil, ErrSubscriptionInvalid
	}
	if limit <= 0 || limit > AutoRenewScanLimit {
		limit = AutoRenewScanLimit
	}
	now = now.UTC()
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+subscriptionColumns+`
		  FROM subscriptions
		 WHERE status = 'active'
		   AND auto_renew = true
		   AND cancel_at_period_end = false
		   AND auto_renew_payment_method_id IS NOT NULL
		   AND end_at <= $1
		   AND (auto_renew_attempted_at IS NULL OR auto_renew_attempted_at <= $2)
		 ORDER BY end_at ASC
		 LIMIT $3`,
		now.Add(lead), now.Add(-retry), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions due for auto-renew: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan subscription due for auto-renew: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscriptions due for auto-renew: %w", err)
	}
	return out, nil
}

// MarkAutoRenewAttempt 记录一次扣费尝试时刻（在真正调用渠道之前）。
//
// 返回 false 表示实例已不再满足条件（用户关闭了自动续费/取消了周期末
// 续费/已进入终态）：调用方必须放弃本次扣费。先写尝试时刻再扣费是
// at-most-once 的退避锚——进程在「已扣款、未结算」之间崩溃时，重试会
// 使用同一订单幂等键（Stripe idempotency），不会二次扣款。
func (r *SQLSubscriptionRepo) MarkAutoRenewAttempt(ctx context.Context,
	id int64, now time.Time) (bool, error) {

	res, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions
		   SET auto_renew_attempted_at = $2, updated_at = now()
		 WHERE id = $1 AND status = 'active'
		   AND auto_renew = true AND cancel_at_period_end = false`,
		id, now.UTC())
	if err != nil {
		return false, fmt.Errorf("store: mark auto-renew attempt(%d): %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: mark auto-renew attempt(%d) rows: %w", id, err)
	}
	return affected > 0, nil
}

// RecordAutoRenewFailure 记录一次扣费失败，并在达到阈值时关闭自动续费。
//
// 【为什么与事件同事务】失败计数是「为什么自动续费被关掉」的唯一依据；
// 计数与 renewal_failed 事件分家会让排障与用户通知失去可信来源。
//
// 【为什么在事务内重新读取计数】调用方的 failures 可能来自扫描时的旧快照；
// 多副本并发（不同订阅锁互不干扰，但同一订阅的理论竞态）必须由行锁串行化。
// 计数只能由本次事务内的当前值 +1，不能用调用方传值覆盖。
func (r *SQLSubscriptionRepo) RecordAutoRenewFailure(ctx context.Context, id int64,
	now time.Time, threshold int, detail map[string]any) (AutoRenewFailureState, error) {

	var state AutoRenewFailureState
	if id <= 0 {
		return state, ErrSubscriptionNotFound
	}
	if threshold <= 0 {
		threshold = AutoRenewFailureThreshold
	}
	err := r.withEventTx(ctx, fmt.Sprintf("record auto-renew failure(sub=%d)", id),
		func(tx *sql.Tx) error {
			var (
				autoRenew bool
				failures  int32
			)
			err := tx.QueryRowContext(ctx, `
				SELECT auto_renew, auto_renew_failures
				  FROM subscriptions WHERE id = $1 FOR UPDATE`, id).
				Scan(&autoRenew, &failures)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSubscriptionNotFound
			}
			if err != nil {
				return fmt.Errorf("store: lock subscription(%d) for auto-renew failure: %w", id, err)
			}
			failures++
			disabled := failures >= int32(threshold)
			_, err = tx.ExecContext(ctx, `
				UPDATE subscriptions
				   SET auto_renew_failures = $2,
				       auto_renew_attempted_at = $3,
				       auto_renew = CASE WHEN $4 THEN false ELSE auto_renew END,
				       updated_at = now()
				 WHERE id = $1`, id, failures, now.UTC(), disabled)
			if err != nil {
				return fmt.Errorf("store: record auto-renew failure(%d): %w", id, err)
			}
			// 每次失败一条事件（P-16）；detail 是受控词汇，绝不写渠道原始
			// 响应（可能含卡信息/指纹）。auto_renew_disabled 只在阈值触发时为 true。
			payload := map[string]any{"failures": failures, "auto_renew_disabled": disabled}
			for k, v := range detail {
				payload[k] = v
			}
			if err := AppendSubscriptionEvent(ctx, tx, id,
				SubscriptionEventRenewalFailed, payload); err != nil {
				return err
			}
			state.Failures = failures
			state.Disabled = disabled
			return nil
		})
	if err != nil {
		return AutoRenewFailureState{}, err
	}
	return state, nil
}

// RecordAutoRenewSuccess 在扣费成功后清零连续失败计数。
//
// 【为什么不在 RenewSubscription 里顺带清零】RenewSubscription 同时服务手动
// 续费：手动续费也应清零自动续费失败计数（用户已用另一种方式履约），但把
// 这一列写进续费 SQL 会让「续费」与「自动续费状态」两个关注点耦合。这里用
// 一条独立的条件 UPDATE，失败只影响计数、不影响已提交的续费事务；下一轮
// 扫描会因 end_at 已推进而不再命中，不会二次扣费。
func (r *SQLSubscriptionRepo) RecordAutoRenewSuccess(ctx context.Context,
	id int64, now time.Time) error {

	if id <= 0 {
		return ErrSubscriptionNotFound
	}
	if _, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions
		   SET auto_renew_failures = 0, auto_renew_attempted_at = $2, updated_at = now()
		 WHERE id = $1`, id, now.UTC()); err != nil {
		return fmt.Errorf("store: record auto-renew success(%d): %w", id, err)
	}
	return nil
}

// DisableAutoRenew 立即关闭自动续费并写 renewal_failed 事件（R-42）。
//
// 用于「令牌已删除/无法解密/被渠道拒绝」的 fail-closed 路径：不计数、不
// 尝试扣费，直接停用。detail 至少包含 reason，便于用户侧通知与排障。
func (r *SQLSubscriptionRepo) DisableAutoRenew(ctx context.Context, id int64,
	detail map[string]any) error {

	if id <= 0 {
		return ErrSubscriptionNotFound
	}
	return r.withEventTx(ctx, fmt.Sprintf("disable auto-renew(sub=%d)", id),
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `
				UPDATE subscriptions
				   SET auto_renew = false, updated_at = now()
				 WHERE id = $1`, id)
			if err != nil {
				return fmt.Errorf("store: disable auto-renew(%d): %w", id, err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: disable auto-renew(%d) rows: %w", id, err)
			}
			if affected == 0 {
				return ErrSubscriptionNotFound
			}
			payload := map[string]any{}
			for k, v := range detail {
				payload[k] = v
			}
			return AppendSubscriptionEvent(ctx, tx, id,
				SubscriptionEventRenewalFailed, payload)
		})
}

// SetSubscriptionAutoRenew 是用户端 PUT /api/subscriptions/{id}/auto-renew 的写入口。
//
// 【归属先于状态】他人实例与不存在统一 ErrSubscriptionNotFound（不泄露 id
// 存在性）；cancelled 终态返回 ErrSubscriptionNotActive（409）。
//
// 【启用必须给出有效令牌】enabled=true 时：请求未带 payment_method_id 则
// 沿用实例上已绑定的令牌；两者都没有返回 ErrAutoRenewPaymentMethodRequired。
// 令牌必须属于当前用户且 status='active'，否则
// ErrAutoRenewPaymentMethodInvalid——绝不「先打开开关、下次再报错」。
//
// 【失败计数清零】用户显式打开/关闭自动续费视为一次人工介入：连续失败
// 计数归零，attempted_at 清空（下次进入窗口可立即重试）。
func (r *SQLSubscriptionRepo) SetSubscriptionAutoRenew(ctx context.Context, userID, subscriptionID int64,
	enabled bool, paymentMethodID *string) (Subscription, error) {

	if userID <= 0 || subscriptionID <= 0 {
		return Subscription{}, ErrSubscriptionNotFound
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Subscription{}, fmt.Errorf("store: begin auto-renew preference: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanSubscription(tx.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`,
		subscriptionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: load subscription(%d) for auto-renew: %w",
			subscriptionID, err)
	}
	if current.UserID != userID {
		return Subscription{}, ErrSubscriptionNotFound
	}
	if current.Status == "cancelled" {
		return Subscription{}, ErrSubscriptionNotActive
	}

	if !enabled {
		updated, err := scanSubscription(tx.QueryRowContext(ctx, `
			UPDATE subscriptions
			   SET auto_renew = false,
			       auto_renew_failures = 0,
			       auto_renew_attempted_at = NULL,
			       updated_at = now()
			 WHERE id = $1
			RETURNING `+subscriptionColumns, subscriptionID))
		if err != nil {
			return Subscription{}, fmt.Errorf("store: disable auto-renew(%d): %w", subscriptionID, err)
		}
		if err := tx.Commit(); err != nil {
			return Subscription{}, fmt.Errorf("store: commit auto-renew preference: %w", err)
		}
		return updated, nil
	}

	chosen := current.AutoRenewPaymentMethodID
	if paymentMethodID != nil && *paymentMethodID != "" {
		id := *paymentMethodID
		chosen = &id
	}
	if chosen == nil || *chosen == "" {
		return Subscription{}, ErrAutoRenewPaymentMethodRequired
	}
	// FOR SHARE 与删除路径的 FOR UPDATE 互斥：避免「读到 active → 令牌被并发
	// 删除 → 本事务把已删除的令牌绑定到订阅」的窗口（R-42 fail-closed）。
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT status FROM payment_methods WHERE id = $1 AND user_id = $2 FOR SHARE`,
		*chosen, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrAutoRenewPaymentMethodInvalid
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: load payment method(%s): %w", *chosen, err)
	}
	if status != "active" {
		return Subscription{}, ErrAutoRenewPaymentMethodInvalid
	}

	updated, err := scanSubscription(tx.QueryRowContext(ctx, `
		UPDATE subscriptions
		   SET auto_renew = true,
		       auto_renew_payment_method_id = $2,
		       auto_renew_failures = 0,
		       auto_renew_attempted_at = NULL,
		       updated_at = now()
		 WHERE id = $1
		RETURNING `+subscriptionColumns, subscriptionID, *chosen))
	if err != nil {
		return Subscription{}, fmt.Errorf("store: enable auto-renew(%d): %w", subscriptionID, err)
	}
	if err := tx.Commit(); err != nil {
		return Subscription{}, fmt.Errorf("store: commit auto-renew preference: %w", err)
	}
	return updated, nil
}
