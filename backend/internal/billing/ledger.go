package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

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

// 结案标记（wallet_transactions.note）。无 hold 结算的 DB 幂等锚点（R-41）：
// 同一 requestID 的合法多行中，只有结算/退款路径写这些前缀；ChargeIncrement
// 的增量行不带标记，故不会被误判为「已结案」。
const (
	settlementNote = "billing:settle"
	refundNote     = "billing:refund"
	recoverNote    = "billing:recover"
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
	if delta == math.MinInt64 {
		// -MinInt64 在 int64 中回绕为自身（仍是负数），会让出账方向反转、
		// 甚至绕过 `balance >= amount` 谓词。极端值没有任何合法计费含义，
		// 必须显式拒绝而不是交给溢出后的算术。
		return 0, errors.New("billing: ledger delta out of int64 range")
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

// ApplyDeltaWithToken 在**同一主库事务**内先扣令牌额度、再变更钱包
// （R-73；billing-03 §2.5「先令牌额度，后资金来源」）。
//
// 为什么必须同事务：钱包预扣失败时令牌额度必须回滚；分开两个事务会留下
// 「令牌已扣、钱包没扣」的悬空扣减。tokenID <= 0 或 remain_quota IS NULL
// （无限额）时只变更钱包。delta 为负表示出账；本方法只支持出账方向的
// 令牌扣减（预扣/上浮/增量），正数 delta 不做令牌回补（回补走 CreditToken/
// AdjustToken，由结算/退款路径在正确顺序调用）。
func (l *Ledger) ApplyDeltaWithToken(ctx context.Context, userID, tokenID, delta int64,
	txType, status, refType, refID string) (balanceAfter int64, err error) {

	if delta == 0 {
		return 0, errors.New("billing: ledger delta must not be zero")
	}
	if delta == math.MinInt64 {
		return 0, errors.New("billing: ledger delta out of int64 range")
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
		if delta < 0 {
			if tokenErr := debitTokenTx(ctx, tx, userID, tokenID, -delta); tokenErr != nil {
				return tokenErr
			}
		}
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

// debitTokenTx 在事务内扣减令牌额度（tokenID<=0 / NULL 无限额时 no-op）。
// 令牌必须属于 userID：防止 rc.TokenID 与 rc.UserID 不一致时跨用户扣额。
//
// FOR UPDATE 先锁行再判余额：并发的多次预扣在令牌侧串行化，绝不把
// remain_quota 扣成负数（列上还有 remain_quota >= 0 的 CHECK）。
// 额度不足返回 *QuotaError（billing-03 §3.4：令牌额度不足也是 402）。
func debitTokenTx(ctx context.Context, tx *gorm.DB, userID, tokenID, amount int64) error {
	if tokenID <= 0 || amount <= 0 {
		return nil
	}
	var remain sql.NullInt64
	err := tx.WithContext(ctx).Raw(
		`SELECT remain_quota FROM api_tokens WHERE id = ? AND user_id = ? FOR UPDATE`, tokenID, userID).
		Row().Scan(&remain)
	if errors.Is(err, sql.ErrNoRows) {
		// 请求已通过鉴权却查不到令牌：数据不一致，fail closed。
		return fmt.Errorf("%w: token %d", ErrTokenNotFound, tokenID)
	}
	if err != nil {
		return fmt.Errorf("billing: read token quota: %w", err)
	}
	if !remain.Valid {
		return nil // NULL = 无限额
	}
	if remain.Int64 < amount {
		return &QuotaError{
			Class:     contract.ErrInsufficientQuota,
			Required:  amount,
			Available: remain.Int64,
		}
	}
	if res := tx.WithContext(ctx).Exec(
		`UPDATE api_tokens SET remain_quota = remain_quota - ?, updated_at = now()
		 WHERE id = ?`, amount, tokenID); res.Error != nil {
		return fmt.Errorf("billing: deduct token quota: %w", res.Error)
	}
	return nil
}

// CreditToken 回补令牌额度（tokenID<=0 或 NULL 无限额时 no-op）；
// 同样限定 user_id，避免把额度加回他人的令牌。
//
// 用于退款/结算差额退回/租约回滚；与钱包回补分开调用（billing-03 §2.5：
// Terminate 先资金、后令牌；令牌调整失败只记日志，不回滚已提交的资金）。
func (l *Ledger) CreditToken(ctx context.Context, userID, tokenID, amount int64) error {
	if tokenID <= 0 || amount <= 0 {
		return nil
	}
	if res := l.db.WithContext(ctx).Exec(
		`UPDATE api_tokens SET remain_quota = remain_quota + ?, updated_at = now()
		 WHERE id = ? AND user_id = ? AND remain_quota IS NOT NULL`, amount, tokenID, userID); res.Error != nil {
		return fmt.Errorf("billing: credit token quota: %w", res.Error)
	}
	return nil
}

// AdjustToken 按结算差额调整令牌额度：delta > 0 补扣，delta < 0 回补。
// userID 用于校验令牌归属。
//
// delta 的符号与钱包结算差额一致：`delta = cost − tokenDeducted`。
// 补扣在事务内走 debitTokenTx（条件锁定）；额度不足时返回 *QuotaError，
// 调用方（Terminate）按 billing-03 §5.7 只记日志，不回滚已提交的钱包资金。
func (l *Ledger) AdjustToken(ctx context.Context, userID, tokenID, delta int64) error {
	if tokenID <= 0 || delta == 0 {
		return nil
	}
	if delta == math.MinInt64 {
		return errors.New("billing: token adjustment out of int64 range")
	}
	if delta < 0 {
		return l.CreditToken(ctx, userID, tokenID, -delta)
	}
	return l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return debitTokenTx(ctx, tx, userID, tokenID, delta)
	})
}

// requestTerminal 判断该 requestID 是否已经结案（Important-1）。
//
// 判定面：存在 billing:* 结案标记（结算/退款/兜底），或存在已关闭的
// consume_pre 行（settled/released/expired）。Reserve/ChargeIncrement 在
// 写任何新资金前必须复核它：已退款请求再产生新 hold 会撞
// wallet_release_refid_uq，永远无法再退（wedge）。
func (l *Ledger) requestTerminal(ctx context.Context, requestID string) (bool, error) {
	var n int64
	err := l.db.WithContext(ctx).Raw(
		`SELECT count(*) FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ?
		   AND (note LIKE 'billing:%'
		        OR (type = 'consume_pre' AND status IN ('settled', 'released', 'expired')))`,
		requestID).Row().Scan(&n)
	if err != nil {
		return false, fmt.Errorf("billing: check terminal state for %s: %w", requestID, err)
	}
	return n > 0, nil
}

// applyDeltaTx 是 ApplyDelta 的事务内核；也供持有同一事务的结算/退款路径复用。
func applyDeltaTx(ctx context.Context, tx *gorm.DB, userID, delta int64,
	txType, status, refType, refID string) (int64, error) {
	return applyDeltaTxNote(ctx, tx, userID, delta, txType, status, refType, refID, "")
}

// applyDeltaTxNote 在 applyDeltaTx 基础上写入 note。
//
// 结案标记（note 以 "billing:" 开头）是无 hold 结算的 DB 幂等锚点（R-41）：
// wallet_transactions 没有 requestID 唯一约束（同一请求合法多行），只能靠
// 一行「结算/退款标记」证明该 requestID 已经结案。ChargeIncrement 的增量行
// 不带标记，因此不会被误判为已结算。
func applyDeltaTxNote(ctx context.Context, tx *gorm.DB, userID, delta int64,
	txType, status, refType, refID, note string) (int64, error) {

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
		   (user_id, type, amount, balance_after, status, reference_type, reference_id, note)
		 VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))`,
		userID, txType, delta, balanceAfter, status, refType, refID, note); res.Error != nil {
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
	Held         int64 // 仍处于 hold 的金额（正数）
	UserID       int64
	Closed       bool   // 已存在 settled/released/expired 的预扣行
	ClosedStatus string // 首个已关闭预扣行的状态（settled/released/expired）
	Rows         int    // consume_pre 行数（任意状态）
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
			if st.ClosedStatus == "" {
				st.ClosedStatus = status
			}
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
		if held == 0 {
			// R-41：无 hold 的补扣没有 CAS 锚点。事务内复核该 requestID 是否
			// 已有结算/退款标记行（note = "billing:*"）——两个 Session 对同一
			// requestID 重复结算时，后来者在主库层被拦截。
			already, serr := settlementExists(ctx, tx, requestID)
			if serr != nil {
				return serr
			}
			if already {
				return errHoldClosed
			}
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
		balanceAfter, txErr = applyDeltaTxNote(ctx, tx, userID, -delta, txType,
			"confirmed", "usage_log", requestID, settlementNote)
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
		balanceAfter, txErr = applyDeltaTxNote(ctx, tx, userID, held, "release",
			"confirmed", "usage_log", requestID, refundNote)
		return txErr
	})
	return held, balanceAfter, err
}

// refundSession 全额退回一个会话：关闭全部 hold 行（released），并退回
// ChargeIncrement 已实际扣减的 consumed。
//
// billing-00 §6.2 的「Refund 全额退款」包含两部分：
//   - 预扣 hold（未消耗的冻结额）；
//   - ChargeIncrement 的增量扣减（连接级实际扣费，失败路径同样要退）。
//
// 两者合并为**一行** release：wallet_release_refid_uq 是
// `(reference_id) WHERE type='release'` 的部分唯一索引，同一 requestID
// 只允许一行 release（每次余额变动恰一行流水，GC-3）。
//
// 幂等锚点（按到达顺序）：
//   - hold 行 CAS：`status='hold' → 'released'` 的条件 UPDATE；
//   - 无 hold 会话以「该 requestID 已有 billing:* 结案标记」为锚
//     （settlementExists，与 R-41 的无 hold 结算共用同一套标记）。settled/
//     refunded/recovered 之后不得再退——否则会把已结算请求的增量扣减从订单
//     里退回，或把 R-39 明确不随崩溃退款的成功用量退掉；
//   - 最后，release 唯一索引拒绝任何漏网的第二个释放行（并发窗口兜底）。
//
// 金额权威：held 来自数据库 hold 行；consumed 经 refundableConsumed 钳制到
// 数据库 consume 行合计，会话内存只用于识别「本会话做过增量扣减」。
func (l *Ledger) refundSession(ctx context.Context, userID int64, requestID string, consumed int64) (
	refunded, balanceAfter int64, err error) {

	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		held, _, txErr := closeHoldRows(ctx, tx, userID, requestID, "released")
		if txErr != nil {
			return txErr
		}
		if held == 0 {
			// 有 hold 时 CAS 就是锚点：settle/refund 标记不可能与仍开放的 hold
			// 并存（结算与退款都在关闭 hold 的同一事务里写标记）。只有无 hold
			// 的会话才需要复核结案标记。
			exists, eerr := settlementExists(ctx, tx, requestID)
			if eerr != nil {
				return eerr
			}
			if exists {
				return errHoldClosed
			}
		}
		consumed, txErr = refundableConsumed(ctx, tx, requestID, userID, consumed)
		if txErr != nil {
			return txErr
		}
		total, txErr := refundTotal(held, consumed)
		if txErr != nil {
			return txErr
		}
		if total <= 0 {
			return errNoOpenHold
		}
		balanceAfter, txErr = applyDeltaTxNote(ctx, tx, userID, total, "release",
			"confirmed", "usage_log", requestID, refundNote)
		if txErr != nil {
			// 无 hold 路径上唯一的 INSERT 撞 23505 只可能是
			// wallet_release_refid_uq：另一个执行者已写入同一 requestID 的
			// release 行，资金已被它回补。按幂等命中收敛（事务里没有其它
			// 已发生的变更需要保留）。有 hold 路径的 23505 属于异常状态
			// （如 recover 后又出现新 hold），原样上抛，不做静默容忍。
			if held == 0 && isUniqueViolation(txErr) {
				return errHoldClosed
			}
			return txErr
		}
		refunded = total
		return nil
	})
	return refunded, balanceAfter, err
}

// settlementExists 判断该 requestID 是否已有结算/退款结案标记行（R-41）。
//
// 无 hold 的会话没有 consume_pre 行可做 CAS 锚点；两个 Session 对象对同一
// requestID 依次结算时，后来者通过本查询发现前者的 billing:* 标记行并 no-op。
// 并发窗口由 Redis requestID 锁（Engine.acquireSettlementLock）进一步收窄。
func settlementExists(ctx context.Context, tx *gorm.DB, requestID string) (bool, error) {
	var n int64
	err := tx.WithContext(ctx).Raw(
		`SELECT count(*) FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ?
		   AND note LIKE 'billing:%'`, requestID).Row().Scan(&n)
	if err != nil {
		return false, fmt.Errorf("billing: check settlement markers for %s: %w", requestID, err)
	}
	return n > 0, nil
}

// releaseExists 判断该 requestID 是否已有 release 流水（任意 note）。
//
// 用于兜底退款的收敛（Important-1）：release 行的部分唯一索引是
// `(reference_id) WHERE type='release'`，同一 requestID 只允许一行。若已有
// release（可能来自更早的结算/退款，且没有 billing:* 标记），releaseAbandoned
// 只能收敛 hold 状态，不能再插第二行。
func releaseExists(ctx context.Context, tx *gorm.DB, requestID string) (bool, error) {
	var n int64
	err := tx.WithContext(ctx).Raw(
		`SELECT count(*) FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ? AND type = 'release'`,
		requestID).Row().Scan(&n)
	if err != nil {
		return false, fmt.Errorf("billing: check release rows for %s: %w", requestID, err)
	}
	return n > 0, nil
}

// closedHoldStatus 返回该 requestID 已关闭预扣行的状态（settled/released/expired）。
// 用于 Terminate 撞上 errHoldClosed 时区分「被并发结算」与「被兜底退款」，
// 避免把 settled 误标成 refunded（或反之）。
func (l *Ledger) closedHoldStatus(ctx context.Context, requestID string) (string, error) {
	var status string
	err := l.db.WithContext(ctx).Raw(
		`SELECT status FROM wallet_transactions
		 WHERE reference_type = 'usage_log' AND reference_id = ?
		   AND type = 'consume_pre' AND status IN ('settled', 'released', 'expired')
		 ORDER BY id LIMIT 1`, requestID).Row().Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("billing: read closed hold status for %s: %w", requestID, err)
	}
	return status, nil
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

		// Important-1 收敛：该 requestID 已有结案标记或 release 行时，说明
		// 之前已经结算/退款/兜底过。此时只把残留 hold 置为 expired（收敛
		// 状态），**不再插入第二行 release**——wallet_release_refid_uq 会以
		// 23505 拒绝它，且资金已被前一执行者回补。返回 held=0 让调用方不要
		// 再补令牌额度（钱没有第二次回补）。
		settledBefore, serr := settlementExists(ctx, tx, requestID)
		if serr != nil {
			return serr
		}
		releaseBefore, rerr := releaseExists(ctx, tx, requestID)
		if rerr != nil {
			return rerr
		}
		held, balanceAfter, qerr = closeHoldRows(ctx, tx, userID, requestID, "expired")
		if qerr != nil {
			return qerr
		}
		if held == 0 {
			// 理论上不可能：上面已数到 count>0 的 hold 行。
			return errNoOpenHold
		}
		if settledBefore || releaseBefore {
			held = 0
			balanceAfter = 0
			return nil
		}
		// 兜底退款同样是入账。
		balanceAfter, qerr = applyDeltaTxNote(ctx, tx, userID, held, "release",
			"confirmed", "usage_log", requestID, recoverNote)
		if qerr != nil && isUniqueViolation(qerr) {
			// 竞态：另一执行者在复核与 INSERT 之间写入了 release。回滚本次
			// 余额变更（事务失败即回滚），按幂等命中收敛；残留 hold 下一轮
			// 会被上面的 settledBefore/releaseBefore 分支收敛。
			return errHoldClosed
		}
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
