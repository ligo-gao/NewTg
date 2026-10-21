package store

// 支付令牌数据层（S4；P-14 / R-42）。
//
// 【零回显是读取面的契约】TokenCipher 只供计费 worker 解封使用；HTTP 层
// 只读 Brand/Last4/ExpMonth/ExpYear。任何日志、审计 detail、错误文本都
// 不得包含密文或明文令牌（密文虽不可直接使用，但仍是支付凭据的句柄）。
//
// 【删除即 fail-closed】删除令牌与「关联订阅 auto_renew=false + 事件」
// 在同一事务内完成（R-42）。若分两步，中间崩溃会让 worker 拿着已删除的
// 令牌反复尝试扣费，用户还会持续收到失败通知。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// paymentMethodIDPattern 是 PG uuid 文本形态。先做形态校验再进 SQL：非法
// 文本会触发 22P02，把它统一映射成 404/422 而不是暴露驱动错误。
var paymentMethodIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidPaymentMethodID 做 UUID 形态校验（handler 与 worker 共用）。
func ValidPaymentMethodID(id string) bool {
	return paymentMethodIDPattern.MatchString(strings.TrimSpace(id))
}

// PaymentMethod 是 payment_methods 的一行。
//
// TokenCipher 是 SecretSealer 的输出（"v1:" + base64(nonce||ciphertext)）；
// 它是密文，但仍是敏感句柄，读取响应必须投影掉。
type PaymentMethod struct {
	ID          string
	UserID      int64
	Provider    string
	TokenCipher string
	Brand       string
	Last4       string
	ExpMonth    int16
	ExpYear     int16
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PaymentMethodInput 是一次令牌写入。TokenCipher 必须是已封装的密文：
// 数据层不做加解密，避免「某条路径忘了封口就落明文」。
type PaymentMethodInput struct {
	UserID      int64
	Provider    string
	TokenCipher string
	Brand       string
	Last4       string
	ExpMonth    int16
	ExpYear     int16
}

// PaymentMethodRepo 是令牌 CRUD 的存储接缝。生产实现为
// *SQLPaymentMethodRepo；billing 的服务层通过窄接口使用它。
type PaymentMethodRepo interface {
	Create(ctx context.Context, in PaymentMethodInput) (PaymentMethod, error)
	ListByUser(ctx context.Context, userID int64) ([]PaymentMethod, error)
	GetForUser(ctx context.Context, userID int64, id string) (PaymentMethod, error)
	// Delete 删除令牌，并返回因此被关闭自动续费的订阅 id（R-42）。
	Delete(ctx context.Context, userID int64, id string) ([]int64, error)
	// MarkInvalid 把令牌置为 invalid（解密失败/渠道拒绝）。保留行以便审计。
	MarkInvalid(ctx context.Context, userID int64, id string) error
}

// SQLPaymentMethodRepo 是 PostgreSQL 实现。
type SQLPaymentMethodRepo struct {
	db *sql.DB
}

// NewPaymentMethodRepo 构造仓储。db 必填。
func NewPaymentMethodRepo(db *sql.DB) *SQLPaymentMethodRepo {
	return &SQLPaymentMethodRepo{db: db}
}

const paymentMethodColumns = `id, user_id, provider, token_cipher, brand, last4,
	exp_month, exp_year, status, created_at, updated_at`

func scanPaymentMethod(row interface{ Scan(...any) error }) (PaymentMethod, error) {
	var p PaymentMethod
	err := row.Scan(&p.ID, &p.UserID, &p.Provider, &p.TokenCipher, &p.Brand,
		&p.Last4, &p.ExpMonth, &p.ExpYear, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// Create 插入一条令牌并返回落库行（id/created_at 由 DB 生成）。
//
// 【为什么校验放在这里】handler 会先做一次可读校验，但数据层不能依赖
// 调用方：brand/last4/有效期同时受 CHECK 约束，提前归一化让错误是
// ErrPaymentMethodInvalid 而不是 23514。
func (r *SQLPaymentMethodRepo) Create(ctx context.Context, in PaymentMethodInput) (PaymentMethod, error) {
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	in.Brand = strings.TrimSpace(in.Brand)
	in.Last4 = strings.TrimSpace(in.Last4)
	if in.UserID <= 0 || in.Provider != "stripe" ||
		in.TokenCipher == "" || len(in.TokenCipher) > 4096 ||
		in.Brand == "" || len(in.Brand) > 32 ||
		len(in.Last4) != 4 || in.ExpMonth < 1 || in.ExpMonth > 12 ||
		in.ExpYear < 2000 || in.ExpYear > 9999 {
		return PaymentMethod{}, ErrPaymentMethodInvalid
	}
	p, err := scanPaymentMethod(r.db.QueryRowContext(ctx, `
		INSERT INTO payment_methods
			(user_id, provider, token_cipher, brand, last4, exp_month, exp_year)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+paymentMethodColumns,
		in.UserID, in.Provider, in.TokenCipher, in.Brand, in.Last4,
		in.ExpMonth, in.ExpYear))
	if err != nil {
		if isCheckViolation(err) || isStringDataRightTruncation(err) {
			return PaymentMethod{}, ErrPaymentMethodInvalid
		}
		return PaymentMethod{}, fmt.Errorf("store: create payment method: %w", err)
	}
	return p, nil
}

// ListByUser 返回当前用户的令牌（新的在前）。数据范围只由 userID 决定。
func (r *SQLPaymentMethodRepo) ListByUser(ctx context.Context, userID int64) ([]PaymentMethod, error) {
	if userID <= 0 {
		return nil, ErrPaymentMethodNotFound
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+paymentMethodColumns+`
		  FROM payment_methods WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list payment methods: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PaymentMethod
	for rows.Next() {
		p, err := scanPaymentMethod(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan payment method: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate payment methods: %w", err)
	}
	return out, nil
}

// GetForUser 读取单个令牌。归属不符与不存在统一 ErrPaymentMethodNotFound。
func (r *SQLPaymentMethodRepo) GetForUser(ctx context.Context, userID int64, id string) (PaymentMethod, error) {
	if userID <= 0 || !ValidPaymentMethodID(id) {
		return PaymentMethod{}, ErrPaymentMethodNotFound
	}
	p, err := scanPaymentMethod(r.db.QueryRowContext(ctx, `
		SELECT `+paymentMethodColumns+` FROM payment_methods
		 WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return PaymentMethod{}, ErrPaymentMethodNotFound
	}
	if err != nil {
		return PaymentMethod{}, fmt.Errorf("store: get payment method(%s): %w", id, err)
	}
	return p, nil
}

// Delete 删除令牌并 fail-closed 关闭引用它的订阅（R-42）。
//
// 返回被关闭的订阅 id 列表（调用方写审计/日志）。删除不存在的令牌与
// 归属不符统一 ErrPaymentMethodNotFound（不泄露存在性）。
func (r *SQLPaymentMethodRepo) Delete(ctx context.Context, userID int64, id string) ([]int64, error) {
	if userID <= 0 || !ValidPaymentMethodID(id) {
		return nil, ErrPaymentMethodNotFound
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin delete payment method: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var locked string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM payment_methods WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		id, userID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPaymentMethodNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: lock payment method(%s): %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM payment_methods WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
		return nil, fmt.Errorf("store: delete payment method(%s): %w", id, err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM subscriptions
		 WHERE user_id = $1 AND auto_renew_payment_method_id = $2
		 ORDER BY id`, userID, id)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions using payment method(%s): %w", id, err)
	}
	var ids []int64
	for rows.Next() {
		var subID int64
		if err := rows.Scan(&subID); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: scan subscription using payment method(%s): %w", id, err)
		}
		ids = append(ids, subID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: iterate subscriptions using payment method(%s): %w", id, err)
	}
	_ = rows.Close()

	for _, subID := range ids {
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions
			   SET auto_renew = false,
			       auto_renew_failures = 0,
			       auto_renew_attempted_at = NULL,
			       updated_at = now()
			 WHERE id = $1`, subID); err != nil {
			return nil, fmt.Errorf("store: disable auto-renew(%d) after token delete: %w", subID, err)
		}
		// detail 只含 id 与受控原因（R-18 的 id 口径）；不写令牌密文。
		if err := AppendSubscriptionEvent(ctx, tx, subID,
			SubscriptionEventRenewalFailed, map[string]any{
				"reason":            "payment_method_deleted",
				"payment_method_id": id,
			}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit delete payment method(%s): %w", id, err)
	}
	return ids, nil
}

// MarkInvalid 把令牌置为 invalid（解密失败或渠道明确拒绝该令牌时）。
// 已删除/归属不符返回 ErrPaymentMethodNotFound；幂等（重复置位不报错）。
func (r *SQLPaymentMethodRepo) MarkInvalid(ctx context.Context, userID int64, id string) error {
	if userID <= 0 || !ValidPaymentMethodID(id) {
		return ErrPaymentMethodNotFound
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE payment_methods SET status = 'invalid', updated_at = now()
		 WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("store: mark payment method(%s) invalid: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark payment method(%s) invalid rows: %w", id, err)
	}
	if affected == 0 {
		return ErrPaymentMethodNotFound
	}
	return nil
}
