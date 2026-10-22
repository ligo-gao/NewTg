package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrPaymentComplianceUserNotFound 表示确认目标用户不存在。管理端正常路径下
// 不会出现（principal 已由会话认证），但数据被并发删除时必须明确区分于 500。
var ErrPaymentComplianceUserNotFound = errors.New("store: payment compliance user not found")

// PaymentComplianceRepo 读写 users.payment_compliance_confirmed_at。
//
// 确认状态只有一个事实：首次确认时刻。仓库层只做幂等写入——已确认的行
// 不再改变时间，调用方据 newly 决定是否写一条审计。审计是 who/when 的
// 权威面；本表列只服务「已确认过的管理员不再提示」的读面。
type PaymentComplianceRepo struct{ db *sql.DB }

// NewPaymentComplianceRepo 构造仓库。
func NewPaymentComplianceRepo(db *sql.DB) *PaymentComplianceRepo {
	return &PaymentComplianceRepo{db: db}
}

// Status 返回用户的首次确认时刻；confirmed=false 表示从未确认。
func (r *PaymentComplianceRepo) Status(ctx context.Context, userID int64) (time.Time, bool, error) {
	var at sql.NullTime
	err := r.db.QueryRowContext(ctx,
		`SELECT payment_compliance_confirmed_at FROM users WHERE id = $1`, userID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, ErrPaymentComplianceUserNotFound
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: read payment compliance: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return at.Time.UTC(), true, nil
}

// Confirm 幂等地记录首次确认时刻，并返回该时刻与本次是否新写入。
//
// 用 SELECT ... FOR UPDATE 保护「检查—写入」窗口：并发确认只产生一个时间，
// 后到者拿到同一时刻且 newly=false，因此不会重复写审计。updated_at 同步前进，
// 与该行其它设置写入（/api/self/setting 的乐观锁）保持同一版本语义。
func (r *PaymentComplianceRepo) Confirm(ctx context.Context, userID int64) (time.Time, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: begin payment compliance: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT payment_compliance_confirmed_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, ErrPaymentComplianceUserNotFound
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: lock payment compliance: %w", err)
	}

	newly := !existing.Valid
	at := existing.Time
	if newly {
		err = tx.QueryRowContext(ctx,
			`UPDATE users
			 SET payment_compliance_confirmed_at = now(), updated_at = now()
			 WHERE id = $1
			 RETURNING payment_compliance_confirmed_at`, userID).Scan(&at)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("store: confirm payment compliance: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return time.Time{}, false, fmt.Errorf("store: commit payment compliance: %w", err)
	}
	return at.UTC(), newly, nil
}
