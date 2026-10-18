package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"newtg/gateway/internal/contract"
)

// 账本层负责「余额变更 + 恰一行流水」的原子性（GC-3 / billing-05）。
//
// 设计要点：
//   - 余额只经条件 UPDATE 变更：减法带 `balance >= n` 谓词，加法带钱包
//     安全整数上界谓词。**任何路径都不得产生负余额**——数据库的
//     users_balance_chk (balance >= 0) 是第二道防线，应用层在第一道路径
//     就用 RowsAffected 判定失败。
//   - balance_after 来自同一条 UPDATE 的 `RETURNING balance`：读与写在同一个
//     语句内完成，不会出现「先读余额、后更新」的竞态窗口。
//   - 每笔余额变动恰插入一行 wallet_transactions（amount 有符号、不可为 0）。
//     预扣行 type=consume_pre + status=hold；结算差额 consume/release；
//     全额退款 release；兜底过期 release（holders 见 modules-03 F-03-04）。
//
// 枚举值在应用层再校验一次（数据库 CHECK 是同一份 M0 契约）。这样非法调用
// 会得到可读错误，而不是难以定位的 SQLSTATE 23514。

var (
	// errHoldClosed 表示该 requestID 的预扣行已被结算/退款（CAS 未命中）。
	// 调用方把它当作幂等命中，而不是失败。
	errHoldClosed = errors.New("billing: hold already closed")
	// errNoOpenHold 表示该 requestID 没有预扣行，也没有任何已关闭的预扣行。
	errNoOpenHold = errors.New("billing: no hold for request")
)

// validWalletTxType 是 wallet_transactions.type 的 M0 CHECK 枚举
// （backend/migrations/20261018053301_init.up.sql §10）。
var validWalletTxType = map[string]bool{
	"topup":              true,
	"redemption":         true,
	"consume_pre":        true,
	"consume":            true,
	"release":            true,
	"reversal":           true,
	"admin_adjustment":   true,
	"bonus":              true,
	"affiliate_transfer": true,
	"checkin_bonus":      true,
}

// validWalletTxStatus 是 wallet_transactions.status 的 M0 CHECK 枚举。
var validWalletTxStatus = map[string]bool{
	"hold":      true,
	"settled":   true,
	"released":  true,
	"expired":   true,
	"confirmed": true,
}

// validWalletRefType 是 wallet_transactions.reference_type 的 M0 CHECK 枚举；
// 空串表示 NULL（无关联对象）。
var validWalletRefType = map[string]bool{
	"":                   true,
	"order":              true,
	"redemption":         true,
	"usage_log":          true,
	"admin_op":           true,
	"reversal":           true,
	"signup_grant":       true,
	"promo":              true,
	"checkin":            true,
	"affiliate_transfer": true,
}

// Ledger 是 wallet_transactions 与 users.balance 的原子访问层。
type Ledger struct {
	db *gorm.DB
}

// NewLedger 构造账本访问器；db 必须是主库（非日志库）的 GORM 连接。
func NewLedger(db *gorm.DB) *Ledger {
	return &Ledger{db: db}
}

// ApplyDelta 在**同一主库事务**内完成一次余额变更并插入恰一行流水。
//
// delta 有符号：正数入账（release/topup/...），负数出账（consume_pre/consume/...）。
// 负数路径的条件 UPDATE 带 `balance >= n`；未命中时返回 *QuotaError（Class =
// ErrInsufficientQuota），由 PreConsume 原样上抛给中继映射 402。
//
// reference_type 为空串时写 NULL；reference_id 为空串时写 NULL。
func (l *Ledger) ApplyDelta(ctx context.Context, userID, delta int64,
	txType, status, refType, refID string) (balanceAfter int64, err error) {

	if delta == 0 {
		// 数据库 CHECK amount <> 0；应用层提前拒绝，避免把「零变动」伪装成流水。
		return 0, errors.New("billing: ledger delta must not be zero")
	}
	if !validWalletTxType[txType] {
		return 0, fmt.Errorf("billing: invalid wallet transaction type %q", txType)
	}
	if !validWalletTxStatus[status] {
		return 0, fmt.Errorf("billing: invalid wallet transaction status %q", status)
	}
	if !validWalletRefType[refType] {
		return 0, fmt.Errorf("billing: invalid wallet reference type %q", refType)
	}

	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var txErr error
		balanceAfter, txErr = applyDeltaTx(ctx, tx, userID, delta,
			txType, status, refType, refID)
		return txErr
	})
	if err != nil {
		return 0, err
	}
	return balanceAfter, nil
}

// applyDeltaTx 是 ApplyDelta 的事务内核；也供持有同一事务的结算/退款路径复用。
func applyDeltaTx(ctx context.Context, tx *gorm.DB, userID, delta int64,
	txType, status, refType, refID string) (int64, error) {

	var balanceAfter int64
	var row *sql.Row
	if delta < 0 {
		amount := -delta
		// 条件 DELETE 语义的减法：谓词在行锁内重新求值（READ COMMITTED），
		// 并发的两笔预扣因此串行化，绝不会同时通过余额检查。
		row = tx.WithContext(ctx).Raw(
			`UPDATE users SET balance = balance - ?, updated_at = now()
			 WHERE id = ? AND balance >= ?
			 RETURNING balance`,
			amount, userID, amount).Row()
	} else {
		// 钱包域上界（2^53−1）：余额会被前端与 JSON 往返，超过 JS 安全整数
		// 就无法精确显示。由于 balance 列是 bigint，这里的谓词防止加法溢出
		// 到负数（Go int64 回绕）。
		row = tx.WithContext(ctx).Raw(
			`UPDATE users SET balance = balance + ?, updated_at = now()
			 WHERE id = ? AND balance <= ?
			 RETURNING balance`,
			delta, userID, contract.MaxWalletQuota-delta).Row()
	}

	if err := row.Scan(&balanceAfter); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, classifyBalanceMiss(ctx, tx, userID, delta)
		}
		return 0, fmt.Errorf("billing: update balance for user %d: %w", userID, err)
	}

	if res := tx.WithContext(ctx).Exec(
		`INSERT INTO wallet_transactions
		   (user_id, type, amount, balance_after, status, reference_type, reference_id)
		 VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''))`,
		userID, txType, delta, balanceAfter, status, refType, refID); res.Error != nil {
		return 0, fmt.Errorf("billing: insert wallet transaction for user %d: %w", userID, res.Error)
	}
	return balanceAfter, nil
}

// classifyBalanceMiss 区分「用户不存在」「减法余额不足」与「加法越过钱包上界」。
func classifyBalanceMiss(ctx context.Context, tx *gorm.DB, userID, delta int64) error {
	var balance int64
	err := tx.WithContext(ctx).Raw(`SELECT balance FROM users WHERE id = ?`, userID).
		Row().Scan(&balance)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: user %d", ErrUserNotFound, userID)
	}
	if err != nil {
		return fmt.Errorf("billing: read balance for user %d: %w", userID, err)
	}
	if delta < 0 {
		return &QuotaError{
			Class:     contract.ErrInsufficientQuota,
			Required:  -delta,
			Available: balance,
		}
	}
	return fmt.Errorf("%w: user %d balance %d + %d exceeds wallet limit",
		ErrWalletLimit, userID, balance, delta)
}

// holdState 是某 requestID 的预扣行快照（只读预检，不参与事务判定）。
type holdState struct {
	Held   int64 // 仍处于 hold 的金额（正数）
	UserID int64
	Closed bool // 已存在 settled/released/expired 的预扣行
	Rows   int  // consume_pre 行数（任意状态）
}

// openHoldState 读取 requestID 的预扣行状态。
//
// Terminate 在调用 Saturator **之前**用它做只读预检：若 worker 已经兜底退款
// （hold 已变 released/expired），就不再写一条「成功结算」日志覆盖掉退款事实。
// 该查询不改变任何状态，不违反「先日志、后主库结算」的顺序裁决。
func (l *Ledger) openHoldState(ctx context.Context, requestID string) (holdState, error) {
	rows, err := l.db.WithContext(ctx).Raw(
		`SELECT user_id, amount, status FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ?
		   AND type = 'consume_pre'
		 ORDER BY id`, requestID).Rows()
	if err != nil {
		return holdState{}, fmt.Errorf("billing: read hold state for %s: %w", requestID, err)
	}
	defer func() { _ = rows.Close() }()

	var st holdState
	for rows.Next() {
		var (
			userID int64
			amount int64
			status string
		)
		if err := rows.Scan(&userID, &amount, &status); err != nil {
			return holdState{}, fmt.Errorf("billing: scan hold state for %s: %w", requestID, err)
		}
		if st.UserID != 0 && st.UserID != userID {
			// 同一 requestID 的预扣行必须属于同一用户；出现跨用户说明数据损坏，
			// 继续退款会退错人。
			return holdState{}, fmt.Errorf("billing: hold rows for %s span multiple users", requestID)
		}
		st.UserID = userID
		st.Rows++
		switch status {
		case "hold":
			st.Held += -amount
		case "settled", "released", "expired":
			st.Closed = true
		}
	}
	if err := rows.Err(); err != nil {
		return holdState{}, fmt.Errorf("billing: iterate hold state for %s: %w", requestID, err)
	}
	return st, nil
}

// settleHolds 终结一个请求的全部预扣行，并应用最终差额（多退少补）。
//
// 语义：cost 是本次请求的**实际总应扣**（真实用量求值结果）。钱包已经直接
// 扣过 consumed（ChargeIncrement 增量扣减），所以结算差额为：
//
//	delta = cost − consumed − held
//
// delta < 0 → 从 hold 释放（release 行）；delta > 0 → 补扣（consume 行）。
// 预扣行与差额行在同一事务内提交：不会出现「hold 已关但钱没退/没补」的空洞。
//
// 返回 errHoldClosed 表示该请求已被其它执行者（worker 兜底或重复结算）关闭，
// 调用方应视为幂等命中。
func (l *Ledger) settleHolds(ctx context.Context, userID int64, requestID string,
	consumed, cost int64) (held, balanceAfter int64, err error) {

	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var txErr error
		held, balanceAfter, txErr = closeHoldRows(ctx, tx, userID, requestID, "settled")
		if txErr != nil {
			return txErr
		}
		delta := cost - consumed - held
		if delta == 0 {
			return nil
		}
		// 注意 applyDeltaTx 的符号约定：正数=入账、负数=出账。
		// delta>0 是「还差这么多要扣」→ 传 -delta；delta<0 是「要退」→
		// 传 -delta 也正好是正数。因此两种方向都传 -delta，由流水类型表达语义。
		txType := "consume"
		if delta < 0 {
			txType = "release"
		}
		balanceAfter, txErr = applyDeltaTx(ctx, tx, userID, -delta, txType, "confirmed", "usage_log", requestID)
		return txErr
	})
	return held, balanceAfter, err
}

// releaseHolds 把一个请求的全部预扣行置为 released/expired 并全额退回。
//
// Refund 用 released（请求失败的全额退款）；worker 兜底用 expired
// （modules-03 F-03-04 的状态机）。金额来自数据库中的 hold 行，而不是会话
// 内存值——崩溃后由持久化状态复原（billing-00 §6.2 的幂等锚）。
func (l *Ledger) releaseHolds(ctx context.Context, userID int64, requestID, finalStatus string) (
	held, balanceAfter int64, err error) {

	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var txErr error
		held, balanceAfter, txErr = closeHoldRows(ctx, tx, userID, requestID, finalStatus)
		if txErr != nil {
			return txErr
		}
		if held == 0 {
			return errNoOpenHold
		}
		// 退款是入账：applyDeltaTx 正数表示 credit。
		balanceAfter, txErr = applyDeltaTx(ctx, tx, userID, held, "release", "confirmed", "usage_log", requestID)
		return txErr
	})
	return held, balanceAfter, err
}

// releaseAbandoned 是崩溃兜底的退款入口：从数据库行复原用户与金额，
// 不依赖任何会话内存态（billing-03 §2.6）。
//
// CAS 锚点：`status = 'hold'` 的条件 UPDATE。影响 0 行即代表已被其它执行者
// 处理，返回 errHoldClosed / errNoOpenHold，重复扫描不产生第二行 release。
// 退还行与 hold→expired 在同一事务内提交。
func (l *Ledger) releaseAbandoned(ctx context.Context, requestID string) (
	held, balanceAfter int64, err error) {

	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// FOR UPDATE 与后续 UPDATE 在同一事务内：多副本 worker 并发扫描同一
		// requestID 时，后到者会阻塞到前者提交，然后看到 status 已不是 hold。
		rows, qerr := tx.WithContext(ctx).Raw(
			`SELECT user_id, amount FROM wallet_transactions
			 WHERE reference_type = 'usage_log' AND reference_id = ?
			   AND type = 'consume_pre' AND status = 'hold'
			 FOR UPDATE`, requestID).Rows()
		if qerr != nil {
			return fmt.Errorf("billing: lock holds for %s: %w", requestID, qerr)
		}
		var (
			userID int64
			total  int64
			count  int
		)
		for rows.Next() {
			var uid, amount int64
			if scanErr := rows.Scan(&uid, &amount); scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("billing: scan holds for %s: %w", requestID, scanErr)
			}
			if count > 0 && uid != userID {
				_ = rows.Close()
				return fmt.Errorf("billing: hold rows for %s span multiple users", requestID)
			}
			userID = uid
			total += -amount
			count++
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return fmt.Errorf("billing: iterate holds for %s: %w", requestID, rowsErr)
		}
		// 先关闭游标再发 UPDATE：同一事务内 pgx 不允许在未消费完的结果集上
		// 执行新语句。
		_ = rows.Close()

		if count == 0 {
			closed, cerr := holdsAlreadyClosed(ctx, tx, requestID)
			if cerr != nil {
				return cerr
			}
			if closed {
				return errHoldClosed
			}
			return errNoOpenHold
		}

		held, balanceAfter, qerr = closeHoldRows(ctx, tx, userID, requestID, "expired")
		if qerr != nil {
			return qerr
		}
		if held == 0 {
			// 理论上不可能：上面已数到 count>0 的 hold 行。
			return errNoOpenHold
		}
		// 兜底退款同样是入账。
		balanceAfter, qerr = applyDeltaTx(ctx, tx, userID, held, "release", "confirmed", "usage_log", requestID)
		return qerr
	})
	return held, balanceAfter, err
}

// closeHoldRows 把 requestID 的全部 hold 行置为 finalStatus，返回关闭金额合计。
//
// 若没有 hold 行：
//   - 已存在同 requestID 的 settled/released/expired 预扣行 → errHoldClosed；
//   - 完全没有任何预扣行（预扣额度为 0 / 信任旁路 / 从未 PreConsume）→ 返回 0，
//     由调用方继续按差额补扣（少补路径）。
func closeHoldRows(ctx context.Context, tx *gorm.DB, userID int64, requestID, finalStatus string) (
	held, balanceAfter int64, err error) {

	if !validWalletTxStatus[finalStatus] {
		return 0, 0, fmt.Errorf("billing: invalid hold close status %q", finalStatus)
	}
	rows, qerr := tx.WithContext(ctx).Raw(
		`UPDATE wallet_transactions SET status = ?, updated_at = now()
		 WHERE reference_type = 'usage_log' AND reference_id = ?
		   AND type = 'consume_pre' AND status = 'hold'
		 RETURNING user_id, amount`, finalStatus, requestID).Rows()
	if qerr != nil {
		return 0, 0, fmt.Errorf("billing: close holds for %s: %w", requestID, qerr)
	}
	var seen int
	for rows.Next() {
		var (
			uid    int64
			amount int64
		)
		if scanErr := rows.Scan(&uid, &amount); scanErr != nil {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("billing: scan closed holds for %s: %w", requestID, scanErr)
		}
		if uid != userID {
			_ = rows.Close()
			return 0, 0, fmt.Errorf("billing: hold rows for %s do not belong to user %d", requestID, userID)
		}
		held += -amount
		seen++
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return 0, 0, fmt.Errorf("billing: iterate closed holds for %s: %w", requestID, rowsErr)
	}
	_ = rows.Close()

	if seen == 0 {
		closed, cerr := holdsAlreadyClosed(ctx, tx, requestID)
		if cerr != nil {
			return 0, 0, cerr
		}
		if closed {
			return 0, 0, errHoldClosed
		}
	}
	// balanceAfter 由随后的差额变更填写；没有差额时调用方不需要它。
	return held, 0, nil
}

// holdsAlreadyClosed 检查该 requestID 是否已有终结态的预扣行。
func holdsAlreadyClosed(ctx context.Context, tx *gorm.DB, requestID string) (bool, error) {
	var n int64
	err := tx.WithContext(ctx).Raw(
		`SELECT count(*) FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ?
		   AND type = 'consume_pre' AND status IN ('settled', 'released', 'expired')`,
		requestID).Row().Scan(&n)
	if err != nil {
		return false, fmt.Errorf("billing: check closed holds for %s: %w", requestID, err)
	}
	return n > 0, nil
}
