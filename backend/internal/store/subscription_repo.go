package store

// 订阅套餐数据层（D5a；billing-08 §2–§5/§7、platform-04 §4.5 V10）。
//
// 【为什么收敛在 store】订阅域有三类不变量是**数据层不变量**，不能让每个
// handler/worker 各自记得执行：
//  1. 状态迁移只能单向（active → expired|cancelled），并发下必须用条件
//     UPDATE + RowsAffected 判定，而不是「先读后写」；
//  2. 订阅预扣按 request_id 幂等，且一次预扣只能落在一个实例（R-D5-02）；
//  3. 额度重置必须推进 next_reset_at，否则同一周期会被反复重置（双倍赠额）。
//
// 【为什么套餐删除不设 FK】platform-04 §3.4 V5 统一逻辑引用。删除保护在
// 同一事务内 `SELECT ... FOR UPDATE` 锁套餐行后统计 subscriptions 引用；
// CreateSubscription 对套餐行 `FOR SHARE`，两者互斥，消除「统计时无引用 →
// 删除 → 并发购买写入悬挂 plan_id」的窗口（D1-07 同款做法）。
//
// 【为什么 amount_total = 0 不在 DB 做上界守卫】0 表示无限额度（R-D5-07）；
// `amount_used <= amount_total` 只在 amount_total > 0 时由应用层（会话 C）
// 守卫。DB 只守 amount_used >= 0，否则无限额度实例的第一次消费就会被拒。
//
// 【时间口径】所有时间列 timestamptz；调用方传入的 now 统一转 UTC，避免
// 跨时区进程对「同一时刻」得出不同判定（到期/重置都是边界敏感操作）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// 订阅域哨兵错误。handler 据此映射 404/409/422。
var (
	// ErrPlanNotFound：套餐行不存在。
	ErrPlanNotFound = errors.New("store: subscription plan not found")
	// ErrPlanDuplicate：plan_key 已存在（UNIQUE）→ 409。
	ErrPlanDuplicate = errors.New("store: subscription plan key already exists")
	// ErrPlanInvalid：套餐字段不合法（长度/取值域/DB CHECK 兜底）→ 422。
	ErrPlanInvalid = errors.New("store: subscription plan input is invalid")

	// ErrSubscriptionNotFound：订阅实例不存在。
	ErrSubscriptionNotFound = errors.New("store: subscription not found")
	// ErrSubscriptionInvalid：实例字段不合法或违反 DB CHECK → 422。
	ErrSubscriptionInvalid = errors.New("store: subscription input is invalid")
	// ErrSubscriptionDuplicate：同一订单已建过实例（order_id UNIQUE）→ 409。
	ErrSubscriptionDuplicate = errors.New("store: subscription already exists for this order")
	// ErrSubscriptionNotActive：实例不是 active（终态不可迁移）→ 409。
	ErrSubscriptionNotActive = errors.New("store: subscription is not active")

	// ErrPreConsumeNotFound：预扣记录不存在。
	ErrPreConsumeNotFound = errors.New("store: subscription pre-consume record not found")
	// ErrPreConsumeRefunded：同一 requestId 已退款，禁止再预扣（防双花）。
	ErrPreConsumeRefunded = errors.New("store: subscription pre-consume record was already refunded")
	// ErrPreConsumeInvalid：requestId 非法或预扣额 <= 0。
	ErrPreConsumeInvalid = errors.New("store: subscription pre-consume input is invalid")
)

// ErrPlanReferenced 表示套餐仍被引用，删除被拒（HTTP 409）。
//
// 【为什么停用/终态实例也算引用】逻辑引用没有 FK；已过期/已取消实例仍通过
// plan_id 指向套餐（历史可追溯、账单日志可回查）。只数 active 会让删除
// 留下悬挂引用。
//
// 【为什么未终结订单也算引用】订阅购买先落 orders（biz_kind='subscribe',
// biz_ref_id=plan_id），入账回调随后才创建实例。若只看 subscriptions，用户
// 刚下单（订单仍 pending）而回调稍后到达时管理员可以删掉套餐，回调就只能
// 撞 ErrPlanNotFound，订单成为无法履约的孤儿。只计 pending/succeeded：
// cancelled/expired/failed 已是终态，不会再创建实例，继续阻塞会让用户放弃
// 支付后套餐永远删不掉。
//
// ReferenceCount 是两类引用之和（实例数 + 未终结订阅订单数），供响应层直接
// 展示总量；OrderReferenceCount 单独给出订单侧计数，便于区分「已购实例」与
// 「在途订单」。调用方用 errors.As 读取计数，用 errors.Is(err,
// ErrPlanReferenced{}) 判断错误类别。
type ErrPlanReferenced struct {
	ReferenceCount      int64
	OrderReferenceCount int64
}

func (e ErrPlanReferenced) Error() string {
	return fmt.Sprintf("store: subscription plan is referenced by %d instance(s) and %d open order(s)",
		e.ReferenceCount-e.OrderReferenceCount, e.OrderReferenceCount)
}

// Is 让 errors.Is(err, ErrPlanReferenced{}) 可用于「是否引用类错误」的判断。
func (e ErrPlanReferenced) Is(target error) bool {
	_, ok := target.(ErrPlanReferenced)
	return ok
}

// SubscriptionPlanInput 是**创建**套餐的写入值。
//
// ResetOnRenew / AllowWalletOverflow / Status 用指针表达「未提交 ⇒ 取 DB
// 默认值」（true/true/'enabled'）。显式 false / "disabled" 必须原样落库，
// 故不能用普通 bool + GORM default 标签（包级零值纪律，见 models.go 头注）。
type SubscriptionPlanInput struct {
	PlanKey             string
	Title               string
	Description         *string
	PeriodDays          int32
	PriceAmount         int64
	Currency            string
	GrantCredits        int64
	ResetOnRenew        *bool
	AllowWalletOverflow *bool
	ModelScope          []byte
	ConcurrencyLimit    *int32
	GraceDays           int32
	Status              *string
	SortOrder           int32
}

// SubscriptionPlanUpdate 是全量**编辑**套餐的写入值（PUT 语义）。
//
// 编辑只影响未来购买：已购实例持有快照（R-D5-03），这里不需要（也不允许）
// 回写 subscriptions。
type SubscriptionPlanUpdate struct {
	PlanKey             string
	Title               string
	Description         *string
	PeriodDays          int32
	PriceAmount         int64
	Currency            string
	GrantCredits        int64
	ResetOnRenew        bool
	AllowWalletOverflow bool
	ModelScope          []byte
	ConcurrencyLimit    *int32
	GraceDays           int32
	Status              string
	SortOrder           int32
}

// SubscriptionCreateInput 是订单绑定时创建实例的写入值。
//
// 所有套餐派生的列都显式传入（而不是在事务内重读 plan）：购买回调可能在
// 管理员编辑套餐之后到达，快照必须取「用户下单时看到的条件」，由调用方
// 从订单幂等记录/下单快照中取值。PlanID 仅用于存在性与删除保护校验。
//
// StartAt 为零值时取 now()；EndAt 必须显式给出（end_at > start_at 由 DB
// CHECK 与应用层双重校验）。OrderID 为 nil 表示不绑订单（人工/赠送实例）。
type SubscriptionCreateInput struct {
	UserID              int64
	PlanID              int64
	PlanTitleSnapshot   string
	StartAt             time.Time
	EndAt               time.Time
	NextResetAt         *time.Time
	AmountTotal         int64
	AmountUsed          int64
	AllowWalletOverflow bool
	AutoRenew           bool
	CancelAtPeriodEnd   bool
	OrderID             *int64
	Currency            string
	PricePaidAmount     int64
	PeriodDays          int32
	ResetOnRenew        bool
	GraceDays           int32
	ModelScope          []byte
	ConcurrencyLimit    *int32
}

// SubscriptionRepo 是订阅域的存储接缝（D5b/S4 依赖；单测可注入假实现）。
type SubscriptionRepo interface {
	// Plans
	CreatePlan(ctx context.Context, in SubscriptionPlanInput) (SubscriptionPlan, error)
	GetPlan(ctx context.Context, id int64) (SubscriptionPlan, error)
	UpdatePlan(ctx context.Context, id int64, in SubscriptionPlanUpdate) (SubscriptionPlan, error)
	ListPlans(ctx context.Context, status string) ([]SubscriptionPlan, error)
	EnablePlan(ctx context.Context, id int64) error
	DisablePlan(ctx context.Context, id int64) error
	DeletePlan(ctx context.Context, id int64) error
	ResetPlanQuota(ctx context.Context, planID int64, advanceNextReset bool) (int64, error)

	// Subscriptions
	CreateSubscription(ctx context.Context, in SubscriptionCreateInput) (Subscription, error)
	GetSubscription(ctx context.Context, id int64) (Subscription, error)
	ListSubscriptionsByUser(ctx context.Context, userID int64) ([]Subscription, error)
	ListActiveSubscriptions(ctx context.Context, userID int64, now time.Time) ([]Subscription, error)
	HasActiveUserSubscription(ctx context.Context, userID int64, now time.Time) (bool, error)
	UserActiveSubscriptionsAllowWalletOverflow(ctx context.Context, userID int64, now time.Time) (bool, error)
	CancelAtPeriodEnd(ctx context.Context, id int64) error
	CancelImmediately(ctx context.Context, id int64) error
	ExpireDue(ctx context.Context, now time.Time) (int64, error)
	DueForReset(ctx context.Context, now time.Time) ([]Subscription, error)
	ResetSubscriptionQuota(ctx context.Context, id int64, now time.Time) (int64, error)

	// Pre-consume idempotency records
	InsertPreConsume(ctx context.Context, requestID string, subscriptionID int64, amount int64) (SubscriptionPreConsume, error)
	GetPreConsume(ctx context.Context, requestID string) (SubscriptionPreConsume, error)
	MarkPreConsumeRefunded(ctx context.Context, requestID string) error
}

// SQLSubscriptionRepo 是 SubscriptionRepo 的 PostgreSQL 实现。
type SQLSubscriptionRepo struct {
	db *sql.DB
}

// NewSubscriptionRepo 构造订阅仓储。db 必须非 nil：订阅没有内存兜底，
// 静默返回空结果会把「数据库故障」伪装成「无订阅」并让资金路径错误回退。
func NewSubscriptionRepo(db *sql.DB) *SQLSubscriptionRepo {
	return &SQLSubscriptionRepo{db: db}
}

const planColumns = `id, plan_key, title, description, period_days, price_amount, currency,
	grant_credits, reset_on_renew, allow_wallet_overflow, model_scope,
	concurrency_limit, grace_days, status, sort_order, created_at, updated_at`

const subscriptionColumns = `id, user_id, plan_id, plan_title_snapshot, status, start_at,
	end_at, next_reset_at, amount_total, amount_used, allow_wallet_overflow,
	auto_renew, cancel_at_period_end, order_id, currency, price_paid_amount,
	period_days, reset_on_renew, grace_days, model_scope, concurrency_limit,
	created_at, updated_at`

const preConsumeColumns = `request_id, subscription_id, status, pre_consumed, created_at`

func scanPlan(row interface{ Scan(...any) error }) (SubscriptionPlan, error) {
	var p SubscriptionPlan
	err := row.Scan(&p.ID, &p.PlanKey, &p.Title, &p.Description, &p.PeriodDays,
		&p.PriceAmount, &p.Currency, &p.GrantCredits, &p.ResetOnRenew,
		&p.AllowWalletOverflow, &p.ModelScope, &p.ConcurrencyLimit,
		&p.GraceDays, &p.Status, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func scanSubscription(row interface{ Scan(...any) error }) (Subscription, error) {
	var s Subscription
	err := row.Scan(&s.ID, &s.UserID, &s.PlanID, &s.PlanTitleSnapshot, &s.Status,
		&s.StartAt, &s.EndAt, &s.NextResetAt, &s.AmountTotal, &s.AmountUsed,
		&s.AllowWalletOverflow, &s.AutoRenew, &s.CancelAtPeriodEnd, &s.OrderID,
		&s.Currency, &s.PricePaidAmount, &s.PeriodDays, &s.ResetOnRenew,
		&s.GraceDays, &s.ModelScope, &s.ConcurrencyLimit, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func scanPreConsume(row interface{ Scan(...any) error }) (SubscriptionPreConsume, error) {
	var rec SubscriptionPreConsume
	err := row.Scan(&rec.RequestID, &rec.SubscriptionID, &rec.Status,
		&rec.PreConsumed, &rec.CreatedAt)
	return rec, err
}

// validatePlanFields 在写库前做与 DB CHECK 同口径的校验，让 handler 得到
// 可读的 422 而不是 23514 的原始错误。字符长度按 rune 计数，对齐 PG 的
// char_length（DB 仍以 CHECK 兜底，应用层校验不是唯一防线）。
func validatePlanFields(planKey, title string, description *string, periodDays int32,
	priceAmount, grantCredits int64, graceDays int32, status string) error {
	if strings.TrimSpace(planKey) == "" {
		return ErrPlanInvalid
	}
	if n := utf8.RuneCountInString(title); n < 1 || n > 64 {
		return ErrPlanInvalid
	}
	if description != nil && utf8.RuneCountInString(*description) > 255 {
		return ErrPlanInvalid
	}
	if periodDays < 1 || priceAmount < 0 || grantCredits < 0 || graceDays < 0 {
		return ErrPlanInvalid
	}
	if status != "" && status != "enabled" && status != "disabled" {
		return ErrPlanInvalid
	}
	return nil
}

// CreatePlan 插入套餐。
func (r *SQLSubscriptionRepo) CreatePlan(ctx context.Context, in SubscriptionPlanInput) (SubscriptionPlan, error) {
	resetOnRenew := true
	if in.ResetOnRenew != nil {
		resetOnRenew = *in.ResetOnRenew
	}
	allowOverflow := true
	if in.AllowWalletOverflow != nil {
		allowOverflow = *in.AllowWalletOverflow
	}
	status := "enabled"
	if in.Status != nil {
		status = *in.Status
	}
	if err := validatePlanFields(in.PlanKey, in.Title, in.Description,
		in.PeriodDays, in.PriceAmount, in.GrantCredits, in.GraceDays, status); err != nil {
		return SubscriptionPlan{}, err
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO subscription_plans
			(plan_key, title, description, period_days, price_amount, currency,
			 grant_credits, reset_on_renew, allow_wallet_overflow, model_scope,
			 concurrency_limit, grace_days, status, sort_order)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+planColumns,
		strings.TrimSpace(in.PlanKey), in.Title, in.Description, in.PeriodDays,
		in.PriceAmount, in.Currency, in.GrantCredits, resetOnRenew, allowOverflow,
		nullableJSON(in.ModelScope), in.ConcurrencyLimit, in.GraceDays, status, in.SortOrder)
	p, err := scanPlan(row)
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return SubscriptionPlan{}, ErrPlanDuplicate
		case isCheckViolation(err), isInvalidTextRepresentation(err),
			isStringDataRightTruncation(err):
			return SubscriptionPlan{}, ErrPlanInvalid
		}
		return SubscriptionPlan{}, fmt.Errorf("store: create subscription plan(%s): %w", in.PlanKey, err)
	}
	return p, nil
}

// GetPlan 按 id 读取套餐。
func (r *SQLSubscriptionRepo) GetPlan(ctx context.Context, id int64) (SubscriptionPlan, error) {
	p, err := scanPlan(r.db.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM subscription_plans WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SubscriptionPlan{}, ErrPlanNotFound
	}
	if err != nil {
		return SubscriptionPlan{}, fmt.Errorf("store: get subscription plan(%d): %w", id, err)
	}
	return p, nil
}

// UpdatePlan 全量更新套餐（PUT）。不触碰已购实例：实例持有快照列。
func (r *SQLSubscriptionRepo) UpdatePlan(ctx context.Context, id int64, in SubscriptionPlanUpdate) (SubscriptionPlan, error) {
	if err := validatePlanFields(in.PlanKey, in.Title, in.Description,
		in.PeriodDays, in.PriceAmount, in.GrantCredits, in.GraceDays, in.Status); err != nil {
		return SubscriptionPlan{}, err
	}
	p, err := scanPlan(r.db.QueryRowContext(ctx, `
		UPDATE subscription_plans SET
			plan_key = $1, title = $2, description = $3, period_days = $4,
			price_amount = $5, currency = $6, grant_credits = $7,
			reset_on_renew = $8, allow_wallet_overflow = $9, model_scope = $10,
			concurrency_limit = $11, grace_days = $12, status = $13,
			sort_order = $14, updated_at = now()
		WHERE id = $15
		RETURNING `+planColumns,
		strings.TrimSpace(in.PlanKey), in.Title, in.Description, in.PeriodDays,
		in.PriceAmount, in.Currency, in.GrantCredits, in.ResetOnRenew,
		in.AllowWalletOverflow, nullableJSON(in.ModelScope), in.ConcurrencyLimit,
		in.GraceDays, in.Status, in.SortOrder, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SubscriptionPlan{}, ErrPlanNotFound
	}
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return SubscriptionPlan{}, ErrPlanDuplicate
		case isCheckViolation(err), isInvalidTextRepresentation(err),
			isStringDataRightTruncation(err):
			return SubscriptionPlan{}, ErrPlanInvalid
		}
		return SubscriptionPlan{}, fmt.Errorf("store: update subscription plan(%d): %w", id, err)
	}
	return p, nil
}

// ListPlans 返回套餐列表。status 为空表示全部（管理侧）；"enabled" 是用户侧
// 可购列表。排序固定 sort_order asc, id asc（billing-08 §2.1 展示口径）。
func (r *SQLSubscriptionRepo) ListPlans(ctx context.Context, status string) ([]SubscriptionPlan, error) {
	query := `SELECT ` + planColumns + ` FROM subscription_plans`
	args := make([]any, 0, 1)
	if status != "" {
		args = append(args, status)
		query += ` WHERE status = $1`
	}
	query += ` ORDER BY sort_order ASC, id ASC`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list subscription plans: %w", err)
	}
	defer func() { _ = rows.Close() }()

	plans := make([]SubscriptionPlan, 0, 16)
	for rows.Next() {
		p, scanErr := scanPlan(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan subscription plan: %w", scanErr)
		}
		plans = append(plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscription plans: %w", err)
	}
	return plans, nil
}

// EnablePlan / DisablePlan：启停只影响新购买，已购实例继续消费至到期
// （billing-08 §10-28）。条件是 id 存在，重复启停是幂等的（UPDATE 仍命中行）。
func (r *SQLSubscriptionRepo) EnablePlan(ctx context.Context, id int64) error {
	return r.setPlanStatus(ctx, id, "enabled")
}

func (r *SQLSubscriptionRepo) DisablePlan(ctx context.Context, id int64) error {
	return r.setPlanStatus(ctx, id, "disabled")
}

func (r *SQLSubscriptionRepo) setPlanStatus(ctx context.Context, id int64, status string) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE subscription_plans SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("store: set subscription plan(%d) status=%s: %w", id, status, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: set subscription plan(%d) rows: %w", id, err)
	}
	if affected == 0 {
		return ErrPlanNotFound
	}
	return nil
}

// DeletePlan 删除套餐；有实例或未终结订阅订单引用时返回 ErrPlanReferenced。
//
// 事务内先 `FOR UPDATE` 锁套餐行：CreateSubscription 对同一行 `FOR SHARE`，
// 因此「统计引用 → 删除」之间不可能插入新实例。统计口径包含全部实例状态
// （expired/cancelled 实例同样是逻辑引用，见 ErrPlanReferenced 注释），以及
// orders 中 biz_kind='subscribe' 且 status IN ('pending','succeeded') 的
// 未终结订单：回调尚未落成实例的窗口里，订单本身就是必须保护的引用。
//
// 【订单计数与行锁】下单路径（S3）在创建订单的同一事务里对 plan 行取
// `FOR SHARE`（billing.loadSubscriptionPlanForShare），与本事务的
// `FOR UPDATE` 互斥：删除先持锁时，并发下单等待后查不到行而失败；下单
// 先持锁时，删除等待提交后统计到新增的 pending 订单并返回 ErrPlanReferenced。
// 因此「统计订单 → 删除」之间不可能插入新的未终结订阅订单。
func (r *SQLSubscriptionRepo) DeletePlan(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete subscription plan(%d): %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var locked int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM subscription_plans WHERE id = $1 FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPlanNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock subscription plan(%d): %w", id, err)
	}
	var refs int64
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM subscriptions WHERE plan_id = $1`, id).Scan(&refs); err != nil {
		return fmt.Errorf("store: count subscriptions of plan(%d): %w", id, err)
	}
	var orderRefs int64
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM orders
		WHERE biz_kind = 'subscribe' AND biz_ref_id = $1
		  AND status IN ('pending', 'succeeded')`, id).Scan(&orderRefs); err != nil {
		return fmt.Errorf("store: count open subscribe orders of plan(%d): %w", id, err)
	}
	if refs+orderRefs > 0 {
		return ErrPlanReferenced{ReferenceCount: refs + orderRefs, OrderReferenceCount: orderRefs}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM subscription_plans WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: delete subscription plan(%d): %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete subscription plan(%d): %w", id, err)
	}
	return nil
}

// ResetPlanQuota 批量重置某套餐下全部**活跃**实例的 amount_used（billing-08
// §15.1 管理侧 reset-quota）。
//
// 【为什么必须同步推进 next_reset_at】该端点与 B12 的周期重置读同一个游标。
// 只把 amount_used 清零而不推进 next_reset_at，同一周期内 B12 会再重置一次
// ——同一周期双倍赠额（§10-9 断言在管理路径同样适用）。因此：
//   - advanceNextReset=true 且 next_reset_at 非空 → 前移一个周期；
//   - next_reset_at 为空（reset_on_renew=false）→ 保持 NULL，B12 本就不会
//     重置该实例，不存在叠加。
//
// 单条条件 UPDATE + RowsAffected：并发购买的新实例要么被包含、要么不在
// 本次 WHERE 快照里，不会出现「先查后改」的中间态。
func (r *SQLSubscriptionRepo) ResetPlanQuota(ctx context.Context, planID int64, advanceNextReset bool) (int64, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM subscription_plans WHERE id = $1)`, planID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: check subscription plan(%d): %w", planID, err)
	}
	if !exists {
		return 0, ErrPlanNotFound
	}
	result, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET amount_used = 0,
		    next_reset_at = CASE
		        WHEN $2 AND next_reset_at IS NOT NULL
		        THEN next_reset_at + make_interval(days => period_days)
		        ELSE next_reset_at
		    END,
		    updated_at = now()
		WHERE plan_id = $1 AND status = 'active'`, planID, advanceNextReset)
	if err != nil {
		return 0, fmt.Errorf("store: reset quota of plan(%d): %w", planID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: reset quota of plan(%d) rows: %w", planID, err)
	}
	return affected, nil
}

// CreateSubscription 创建订阅实例（订单入账/人工赠送调用）。
//
// 快照列由调用方显式传入（见 SubscriptionCreateInput 注释）。事务内对
// plan_id `FOR SHARE`：套餐必须存在，且与 DeletePlan 的 FOR UPDATE 互斥。
// order_id UNIQUE 是「同一订单只建一条实例」的最后防线（回调重复投递）。
func (r *SQLSubscriptionRepo) CreateSubscription(ctx context.Context, in SubscriptionCreateInput) (Subscription, error) {
	startAt := in.StartAt
	if startAt.IsZero() {
		startAt = time.Now().UTC()
	} else {
		startAt = startAt.UTC()
	}
	endAt := in.EndAt.UTC()
	if !endAt.After(startAt) {
		return Subscription{}, ErrSubscriptionInvalid
	}
	if in.PeriodDays < 1 || in.AmountTotal < 0 || in.AmountUsed < 0 ||
		in.PricePaidAmount < 0 || in.GraceDays < 0 ||
		strings.TrimSpace(in.PlanTitleSnapshot) == "" || strings.TrimSpace(in.Currency) == "" {
		return Subscription{}, ErrSubscriptionInvalid
	}
	status := "active"

	// reset_on_renew=false 表示这个实例没有周期重置：next_reset_at 必须归一到
	// NULL。若保留调用方传入的游标，DueForReset 会把该实例扫进 B12，
	// ResetSubscriptionQuota 却因 reset_on_renew=false 拒绝执行——扫描与执行
	// 口径不一致会形成永远「到期待重置」的僵尸行。NULL 是唯一自洽的表示。
	var nextResetAt any
	if in.ResetOnRenew {
		nextResetAt = nullableTime(in.NextResetAt)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Subscription{}, fmt.Errorf("store: begin create subscription: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var planID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM subscription_plans WHERE id = $1 FOR SHARE`, in.PlanID).Scan(&planID)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrPlanNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: check subscription plan(%d): %w", in.PlanID, err)
	}

	s, err := scanSubscription(tx.QueryRowContext(ctx, `
		INSERT INTO subscriptions
			(user_id, plan_id, plan_title_snapshot, status, start_at, end_at,
			 next_reset_at, amount_total, amount_used, allow_wallet_overflow,
			 auto_renew, cancel_at_period_end, order_id, currency,
			 price_paid_amount, period_days, reset_on_renew, grace_days,
			 model_scope, concurrency_limit)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
		        $14, $15, $16, $17, $18, $19, $20)
		RETURNING `+subscriptionColumns,
		in.UserID, in.PlanID, in.PlanTitleSnapshot, status, startAt, endAt,
		nextResetAt, in.AmountTotal, in.AmountUsed,
		in.AllowWalletOverflow, in.AutoRenew, in.CancelAtPeriodEnd, in.OrderID,
		in.Currency, in.PricePaidAmount, in.PeriodDays, in.ResetOnRenew,
		in.GraceDays, nullableJSON(in.ModelScope), in.ConcurrencyLimit))
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return Subscription{}, ErrSubscriptionDuplicate
		case isCheckViolation(err), isInvalidTextRepresentation(err),
			isStringDataRightTruncation(err):
			return Subscription{}, ErrSubscriptionInvalid
		}
		return Subscription{}, fmt.Errorf("store: create subscription(user=%d plan=%d): %w",
			in.UserID, in.PlanID, err)
	}
	if err := tx.Commit(); err != nil {
		return Subscription{}, fmt.Errorf("store: commit create subscription: %w", err)
	}
	return s, nil
}

// GetSubscription 按 id 读取实例。
func (r *SQLSubscriptionRepo) GetSubscription(ctx context.Context, id int64) (Subscription, error) {
	s, err := scanSubscription(r.db.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: get subscription(%d): %w", id, err)
	}
	return s, nil
}

// ListSubscriptionsByUser 返回用户全部实例（含已终结），供「我的订阅」历史
// 与管理侧 Manage Subscriptions（billing-08 §15.4）。新购买在前。
func (r *SQLSubscriptionRepo) ListSubscriptionsByUser(ctx context.Context, userID int64) ([]Subscription, error) {
	return r.querySubscriptions(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions
		 WHERE user_id = $1 ORDER BY start_at DESC, id DESC`, userID)
}

// ListActiveSubscriptions 返回当前可消费的实例，按 end_at asc, id asc 排序
// （billing-08 §4.1：资金分派逐个尝试扣减的冻结顺序）。
//
// 到期判定含快照宽限期：end_at + grace_days > now。grace_days=0 时等价于
// end_at > now。
func (r *SQLSubscriptionRepo) ListActiveSubscriptions(ctx context.Context, userID int64, now time.Time) ([]Subscription, error) {
	return r.querySubscriptions(ctx, `
		SELECT `+subscriptionColumns+` FROM subscriptions
		WHERE user_id = $1 AND status = 'active'
		  AND end_at + make_interval(days => grace_days) > $2
		ORDER BY end_at ASC, id ASC`, userID, now.UTC())
}

func (r *SQLSubscriptionRepo) querySubscriptions(ctx context.Context, query string, args ...any) ([]Subscription, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]Subscription, 0, 8)
	for rows.Next() {
		s, scanErr := scanSubscription(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan subscription: %w", scanErr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscriptions: %w", err)
	}
	return out, nil
}

// HasActiveUserSubscription 实现 billing-08 §4.1 的读侧契约。
func (r *SQLSubscriptionRepo) HasActiveUserSubscription(ctx context.Context, userID int64, now time.Time) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE user_id = $1 AND status = 'active'
			  AND end_at + make_interval(days => grace_days) > $2
		)`, userID, now.UTC()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: has active subscription(user=%d): %w", userID, err)
	}
	return ok, nil
}

// UserActiveSubscriptionsAllowWalletOverflow 实现 billing-08 §4.1：
// 任一活跃实例的 allow_wallet_overflow 为真即允许 subscription_first 回落钱包。
func (r *SQLSubscriptionRepo) UserActiveSubscriptionsAllowWalletOverflow(ctx context.Context, userID int64, now time.Time) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE user_id = $1 AND status = 'active'
			  AND end_at + make_interval(days => grace_days) > $2
			  AND allow_wallet_overflow
		)`, userID, now.UTC()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: subscription wallet overflow(user=%d): %w", userID, err)
	}
	return ok, nil
}

// CancelAtPeriodEnd 置「周期末取消」：实例仍 active 至 end_at，只是不再续费
// （billing-08 §3.5 P1）。已置位时重复调用仍命中行（幂等）。
func (r *SQLSubscriptionRepo) CancelAtPeriodEnd(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET cancel_at_period_end = true, updated_at = now()
		WHERE id = $1 AND status = 'active'`, id)
	if err != nil {
		return fmt.Errorf("store: cancel subscription(%d) at period end: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: cancel subscription(%d) rows: %w", id, err)
	}
	if affected == 1 {
		return nil
	}
	// affected=0：区分「不存在」与「已终态」。终态不能再置取消标记，返回
	// ErrSubscriptionNotActive 让 handler 映射 409（而不是假装成功）。
	status, findErr := r.subscriptionStatus(ctx, id)
	if findErr != nil {
		return findErr
	}
	if status == "" {
		return ErrSubscriptionNotFound
	}
	return ErrSubscriptionNotActive
}

// CancelImmediately 立即取消（管理侧 P2，billing-08 §3.5）。
//
// 条件 UPDATE 只命中 active；0 行时：
//   - 行不存在 → ErrSubscriptionNotFound；
//   - 已是 cancelled → nil（重复调用幂等，目标终态已达成）；
//   - 已是 expired → ErrSubscriptionNotActive（终态不可迁移）。
func (r *SQLSubscriptionRepo) CancelImmediately(ctx context.Context, id int64) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status = 'active'`, id)
	if err != nil {
		return fmt.Errorf("store: cancel subscription(%d): %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: cancel subscription(%d) rows: %w", id, err)
	}
	if affected == 1 {
		return nil
	}
	status, findErr := r.subscriptionStatus(ctx, id)
	if findErr != nil {
		return findErr
	}
	switch status {
	case "":
		return ErrSubscriptionNotFound
	case "cancelled":
		return nil
	default:
		return ErrSubscriptionNotActive
	}
}

func (r *SQLSubscriptionRepo) subscriptionStatus(ctx context.Context, id int64) (string, error) {
	var status string
	err := r.db.QueryRowContext(ctx,
		`SELECT status FROM subscriptions WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read subscription(%d) status: %w", id, err)
	}
	return status, nil
}

// ExpireDue 把到期实例置 expired（B12；billing-08 §3.4 / §7）。
//
// 条件 UPDATE 是唯一迁移守卫：重复执行只会在第一次命中，之后 affected=0
// 即幂等返回。到期分两支（M4 裁决）：
//   - cancel_at_period_end=true：用户已明确不续费，周期末即终止，**不享宽限**
//     （end_at <= now）；
//   - 其余实例：宽限期只属于「可能续费」的周期末（end_at + grace_days <= now）。
//
// 若把宽限无条件套用到已取消实例，用户点了「周期末取消」后仍能多用
// grace_days 天，与取消语义冲突。
// 注意：到期实例仍允许在飞请求结算 delta（§3.4），本函数不碰 amount_used。
func (r *SQLSubscriptionRepo) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions SET status = 'expired', updated_at = now()
		WHERE status = 'active'
		  AND (
		        (cancel_at_period_end AND end_at <= $1)
		     OR (NOT cancel_at_period_end
		         AND end_at + make_interval(days => grace_days) <= $1)
		  )`, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("store: expire subscriptions: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: expire subscriptions rows: %w", err)
	}
	return affected, nil
}

// DueForReset 返回 next_reset_at 已到期的活跃实例（B12 重置扫描）。
//
// 只读：真正的重置由 ResetSubscriptionQuota 的条件 UPDATE 完成，扫描与写入
// 分离让 worker 可以分批处理且幂等。
//
// `end_at > now` 是 H3 裁决：宽限期（end_at < now < end_at+grace_days）内
// 不得发放新周期额度。与 ExpireDue 配合后，恰在周期内的实例才进入重置；
// 宽限期内仍 active 的实例被这里排除，宽限结束则被 ExpireDue 迁移。
//
// LIMIT 让单次扫描的返回集有界：积压时本轮只取最旧的一批，重置后它们
// 的游标前移、自然离开候选集，下一轮继续；不会把整表 due 行一次读进内存。
func (r *SQLSubscriptionRepo) DueForReset(ctx context.Context, now time.Time) ([]Subscription, error) {
	return r.querySubscriptions(ctx, `
		SELECT `+subscriptionColumns+` FROM subscriptions
		WHERE status = 'active' AND next_reset_at IS NOT NULL AND next_reset_at <= $1
		  AND end_at > $1
		ORDER BY next_reset_at ASC, id ASC
		LIMIT `+strconv.Itoa(ResetScanLimit), now.UTC())
}

// ResetScanLimit 是 DueForReset 单轮返回上限（LOW：积压时有界扫描）。
const ResetScanLimit = 1000

// ResetSubscriptionQuota 重置单个实例并**原子推进** next_reset_at（B12）。
//
// 条件里带 `next_reset_at <= $2`：第二次 pass 时游标已被前移到未来，
// RowsAffected=0 —— 同一周期不会双倍重置（billing-08 §7 / §10-9）。
// 条件里还带 `reset_on_renew`：该列是购买瞬间快照，false 表示用户买的
// 「不周期重置」合同，即使历史行里残留了 next_reset_at 也不能被重置。
// 推进量用快照 period_days 而不是读当前套餐：续费/编辑不得改变本实例的周期。
//
// 【H3：宽限期不重置】`end_at > $2` 排除 end_at < now < end_at+grace 的
// 实例。宽限期只延长「能否继续消费」，不发放新周期额度（§3.4）；
// 若此处不设守卫，宽限期内 next_reset_at 到期会把 amount_used 清零，
// 等于凭空赠送一个周期。等值边界（end_at = now）也按不重置处理。
//
// 【M2：一次推进到第一个严格晚于 now 的边界】停机多轮后 next_reset_at
// 可能落后 now 好几个周期。若只 +period，需要多轮扫描才能追上，且每轮都
// 会重复「归零」同一实例（虽然 amount_used 已为 0，仍是无谓写与日志噪声）。
// 周期数 = FLOOR((now - next_reset_at) / period) + 1：
//   - 停机 62 天、周期 30 天 → floor(62/30)+1 = 3 → 前进 90 天；
//   - 恰好整周期（差 30 天）→ floor(1)+1 = 2，跳过落在 now 上的边界，
//     保证推进后严格 > now（否则下一轮会再次命中并多跳一个周期）。
//
// 【与在飞请求的竞态】设计已登记（billing-08 §13-3）：重置归零时冻结该实例
// 的在飞请求，其 Terminate delta 会落到已重置计数上。本函数不等待在飞租约，
// 调用方（worker）按 §13-3 的处置决定是否在重置前检查租约。
func (r *SQLSubscriptionRepo) ResetSubscriptionQuota(ctx context.Context, id int64, now time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions
		SET amount_used = 0,
		    next_reset_at = next_reset_at + make_interval(days =>
		        (FLOOR(EXTRACT(EPOCH FROM ($2 - next_reset_at))
		               / (period_days * 86400.0))::int + 1) * period_days),
		    updated_at = now()
		WHERE id = $1 AND status = 'active' AND reset_on_renew
		  AND next_reset_at IS NOT NULL AND next_reset_at <= $2
		  AND end_at > $2
		  AND period_days > 0`, id, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("store: reset subscription(%d) quota: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: reset subscription(%d) rows: %w", id, err)
	}
	return affected, nil
}

// DeletePreConsumeBefore 删除 created_at 早于 cutoff 的预扣幂等记录（B12）。
//
// 【为什么分批】保留期 7 天（billing-04 §3.4；billing-08 §7），单次清理在
// 积压时可能删除数十万行。一个大 DELETE 会持有长事务、放大 WAL 与锁等待，
// 把并发预扣/退款的事务拖慢甚至超时。LIMIT 子查询让每次删除的行数有界，
// 调用方（worker）循环多批，直到返回 affected < limit。
//
// 【created_at 索引】清理谓词是 `created_at < cutoff`，子查询按 created_at
// 排序取 LIMIT；迁移 20261020190000 为 (created_at) 建索引，让每批只触碰
// 索引上的前 N 行，而不是在保留窗口内反复顺序扫描。索引的写放大只落在
// 每秒少量预扣插入上，远小于清理时的扫描收益。
//
// ORDER BY created_at, request_id 让批次顺序确定（同秒行按 PK 稳定），
// 避免同一批反复选中相同行导致空转。
func (r *SQLSubscriptionRepo) DeletePreConsumeBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM subscription_pre_consume_records
		WHERE request_id IN (
			SELECT request_id FROM subscription_pre_consume_records
			WHERE created_at < $1
			ORDER BY created_at ASC, request_id ASC
			LIMIT $2
		)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("store: delete subscription pre-consume records: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete subscription pre-consume rows: %w", err)
	}
	return affected, nil
}

// InsertPreConsume 写入/读取订阅预扣幂等记录（billing-08 §4.4、R-D5-02）。
//
// 语义：
//   - 第一次 requestId：插入 consumed 行并返回；
//   - 已存在且 consumed：幂等返回既有行（不重复扣减）；
//   - 已存在且 refunded：返回 ErrPreConsumeRefunded ——「已退款再预扣」是
//     硬禁止（防单次请求双花，§4.4 表格第 2 行）。
//
// 【为什么一次预扣只能落一个实例】request_id 是主键，行上只有一个
// subscription_id；跨实例拆分会让退款/兜底恢复无法确定回退哪条 amount_used
// （R-D5-02）。
//
// 【并发】ON CONFLICT DO NOTHING 让两个同 requestId 的并发插入收敛为一条：
// 后到者阻塞到先到者提交，然后走 SELECT 返回同一行。
func (r *SQLSubscriptionRepo) InsertPreConsume(ctx context.Context, requestID string, subscriptionID int64, amount int64) (SubscriptionPreConsume, error) {
	if strings.TrimSpace(requestID) == "" || subscriptionID <= 0 || amount <= 0 {
		return SubscriptionPreConsume{}, ErrPreConsumeInvalid
	}
	rec, err := scanPreConsume(r.db.QueryRowContext(ctx, `
		INSERT INTO subscription_pre_consume_records
			(request_id, subscription_id, status, pre_consumed)
		VALUES ($1, $2, 'consumed', $3)
		ON CONFLICT (request_id) DO NOTHING
		RETURNING `+preConsumeColumns, requestID, subscriptionID, amount))
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if isInvalidTextRepresentation(err) {
			return SubscriptionPreConsume{}, ErrPreConsumeInvalid
		}
		return SubscriptionPreConsume{}, fmt.Errorf("store: insert subscription pre-consume(%s): %w", requestID, err)
	}

	// 冲突：读取既有行。不能用 DO UPDATE 返回，因为已有行可能已是 refunded，
	// 那种情况必须报错而不是悄悄改写（幂等 ≠ 允许重放已退款请求）。
	existing, readErr := r.GetPreConsume(ctx, requestID)
	if readErr != nil {
		return SubscriptionPreConsume{}, readErr
	}
	if existing.Status == "refunded" {
		return SubscriptionPreConsume{}, ErrPreConsumeRefunded
	}
	return existing, nil
}

// GetPreConsume 按 requestId 读取幂等记录。
func (r *SQLSubscriptionRepo) GetPreConsume(ctx context.Context, requestID string) (SubscriptionPreConsume, error) {
	rec, err := scanPreConsume(r.db.QueryRowContext(ctx,
		`SELECT `+preConsumeColumns+` FROM subscription_pre_consume_records WHERE request_id = $1`,
		requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return SubscriptionPreConsume{}, ErrPreConsumeNotFound
	}
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return SubscriptionPreConsume{}, ErrPreConsumeInvalid
		}
		return SubscriptionPreConsume{}, fmt.Errorf("store: get subscription pre-consume(%s): %w", requestID, err)
	}
	return rec, nil
}

// MarkPreConsumeRefunded 把预扣记录置 refunded（请求失败退款，A 类）。
//
// 条件 UPDATE 只命中 consumed：已 refunded 时 RowsAffected=0，再查状态确认
// 后返回 nil（退款按 requestId 幂等，billing-08 §3.7/§4.5）；行不存在才是
// ErrPreConsumeNotFound。绝不从 refunded 回到 consumed。
func (r *SQLSubscriptionRepo) MarkPreConsumeRefunded(ctx context.Context, requestID string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE subscription_pre_consume_records SET status = 'refunded'
		WHERE request_id = $1 AND status = 'consumed'`, requestID)
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return ErrPreConsumeInvalid
		}
		return fmt.Errorf("store: mark subscription pre-consume(%s) refunded: %w", requestID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: mark subscription pre-consume(%s) rows: %w", requestID, err)
	}
	if affected == 1 {
		return nil
	}
	existing, getErr := r.GetPreConsume(ctx, requestID)
	if getErr != nil {
		return getErr
	}
	if existing.Status == "refunded" {
		return nil
	}
	// 理论上不可达：条件只放了 consumed，0 行且状态不是 refunded。
	return fmt.Errorf("store: subscription pre-consume(%s) refused transition from %s",
		requestID, existing.Status)
}

// nullableJSON 把空切片翻译为 SQL NULL：ModelScope 的 NULL 与 '[]' 语义
// 不同（NULL=全部模型；'[]'=没有任何模型可用），不能互相顶替。
func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

// isStringDataRightTruncation 识别 char(n)/varchar(n) 超长（SQLSTATE 22001）。
//
// 订阅域最典型的触发点是 currency char(3)：传 "USDX" 时 PostgreSQL 在求值
// CHECK（23514）之前就先报 string_data_right_truncation。若不单独映射，用户的
// 四位币种会被 handler 当作内部错误返回 500，而不是可读的 422。
func isStringDataRightTruncation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22001"
}
