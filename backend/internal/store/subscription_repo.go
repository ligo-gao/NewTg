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
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	// ErrSubscriptionQuotaInsufficient：所选实例额度不足（amount_total>0 且
	// amount_used+amount 越上界），或实例已不在可消费状态。资金分派据此
	// 继续尝试下一个候选；全部耗尽时由计费侧映射 402。
	ErrSubscriptionQuotaInsufficient = errors.New("store: subscription quota insufficient")

	// ErrSubscriptionSettleExceeded：结算增量会把 amount_used 推过 amount_total
	// 上界（仅 amount_total>0 时成立）。与预扣的 ErrSubscriptionQuotaInsufficient
	// 刻意区分：预扣失败可以继续尝试下一个候选，结算越界则只能拒绝本次结算、
	// **保留已预扣额度**，不得部分结算（billing-08 §5.1）。
	ErrSubscriptionSettleExceeded = errors.New("store: subscription settlement would exceed total")
	// ErrSubscriptionDeltaOverflow：结算增量的整数运算溢出 int64（数据损坏/
	// 恶意增量），必须拒绝而不是回绕出一个合法金额。
	ErrSubscriptionDeltaOverflow = errors.New("store: subscription settlement delta overflows")

	// ErrSubscriptionEventInvalid：事件入参非法（未知 kind / 空订阅 / 负数
	// after_seq）。未知 kind 必须拒绝而不是落库：kind 是冻结白名单（R-18），
	// 让未来拼错的 kind 静默落库会污染事件流且无法被 CHECK 之外的下游识别。
	ErrSubscriptionEventInvalid = errors.New("store: subscription event input is invalid")
	// ErrSubscriptionEventSeqConflict：seq 唯一键冲突。正常情况下订阅行锁
	// 已经串行化，出现它说明有写方绕过了锁或数据库出现了非预期的并发；
	// 调用方必须**整体重试事务**，而不是跳过该 seq（跳号会让 after_seq
	// 增量拉取的客户端永久漏事件）。
	ErrSubscriptionEventSeqConflict = errors.New("store: subscription event sequence conflict")
)

// 订阅事件 kind 白名单（R-18 冻结）。S1 只产出 created/renewed/cancelled/
// expired/reset；upgraded/downgraded/refunded 由 S2/S3 接入，白名单先冻结，
// 避免各批次对同一含义使用不同拼写。
const (
	SubscriptionEventCreated    = "created"
	SubscriptionEventRenewed    = "renewed"
	SubscriptionEventUpgraded   = "upgraded"
	SubscriptionEventDowngraded = "downgraded"
	SubscriptionEventRefunded   = "refunded"
	SubscriptionEventCancelled  = "cancelled"
	SubscriptionEventExpired    = "expired"
	SubscriptionEventReset      = "reset"
)

// subscriptionEventKinds 是 kind 白名单的唯一判定源。新增 kind 必须同时改
// 迁移里的 CHECK 与 EnumChecks（schema 集成测试双向对账）。
var subscriptionEventKinds = map[string]struct{}{
	SubscriptionEventCreated:    {},
	SubscriptionEventRenewed:    {},
	SubscriptionEventUpgraded:   {},
	SubscriptionEventDowngraded: {},
	SubscriptionEventRefunded:   {},
	SubscriptionEventCancelled:  {},
	SubscriptionEventExpired:    {},
	SubscriptionEventReset:      {},
}

// SubscriptionEvent 是一条订阅状态迁移事件（S1）。
//
// Detail 保留 jsonb 原文（[]byte）：读侧（HTTP 端点）直接透传，不在 store
// 重新编码一次；空值在写入时归一为 `{}`，不会出现 SQL NULL。
type SubscriptionEvent struct {
	SubscriptionID int64
	Seq            int64
	Kind           string
	Detail         []byte
	CreatedAt      time.Time
}

// SubscriptionEventScanLimit 是单次事件拉取的上限：事件流只增不减，
// 无界读取会让长生命周期订阅的一次请求把整段历史装进内存。
const SubscriptionEventScanLimit = 500

// ExpireScanLimit 是 ExpireDue 单次事务处理的到期行上界（R-22）。
//
// 到期扫描原来是一条无界 UPDATE：积压时整个批次一个事务，期间持有全部到期
// 订阅行锁，阻塞这些实例上的预扣/结算/续费。按批上界 + worker 循环到返回
// 0 行，把锁持有时间与单事务 WAL 有界化；每批仍然「状态迁移 + 事件」原子。
const ExpireScanLimit = 1000

// subscriptionEventTxAttempts 是「状态迁移 + 事件」事务在 seq 唯一键冲突时
// 的整体重试次数（含首次）。冲突是订阅行锁之外的兜底，正常路径不会消耗。
const subscriptionEventTxAttempts = 3

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
	// ListSubscriptionEvents 返回某订阅 seq > afterSeq 的事件（升序，上限
	// limit）。可见性（本人 / users:read）属于 HTTP 层，store 不做归属过滤。
	ListSubscriptionEvents(ctx context.Context, subscriptionID int64, afterSeq int64, limit int) ([]SubscriptionEvent, error)

	// Pre-consume idempotency records
	InsertPreConsume(ctx context.Context, requestID string, subscriptionID int64, amount int64) (SubscriptionPreConsume, error)
	// PreConsumeUserSubscription 是资金分派（billing-04 §3.4 / billing-08 §4.1）
	// 的单实例预扣：幂等记录与 amount_used 增量在同一事务内落地。
	// created=false 表示命中既有幂等记录（本次未再扣额度）。
	PreConsumeUserSubscription(ctx context.Context, requestID string, subscriptionID int64, amount int64) (rec SubscriptionPreConsume, created bool, err error)
	GetPreConsume(ctx context.Context, requestID string) (SubscriptionPreConsume, error)
	MarkPreConsumeRefunded(ctx context.Context, requestID string) error
	// PostConsumeUserSubscriptionDelta 在结算时追加**可正可负**的已用额度增量
	// （billing-04 §3.5 / billing-08 §5.1）：正为少补、负为多退，下界钳到 0；
	// amount_total>0 且结果越界时拒绝。返回更新后的实例快照（成功日志键用）。
	PostConsumeUserSubscriptionDelta(ctx context.Context, subscriptionID, delta int64) (Subscription, error)
	// RefundSubscriptionIncrement 回退 ChargeIncrement 已立即落账的增量，
	// 仅在 amount_used >= amount 时生效（M3 周期重置守卫）。返回 false 表示
	// 本周期已不再持有这笔增量，调用方应跳过而不得把 amount_used 扣成负数。
	RefundSubscriptionIncrement(ctx context.Context, subscriptionID, amount int64) (bool, error)
	// ListStalePreConsumes 返回「已预扣但仍未结算/退款」且早于 before 的幂等
	// 记录，供 worker 崩溃兜底（billing-06 §5.2）。request_registry 已进入
	// settled/refunded 的记录被排除：正常结算/退款已经处理过它们。
	ListStalePreConsumes(ctx context.Context, before time.Time, limit int) ([]SubscriptionPreConsume, error)
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
// 【S1/R-21 事件】管理侧重置与 worker 的周期重置语义相同（amount_used 归零、
// 游标前移），事件流里必须同样出现 reset，否则用户/审计会看到额度无解释地
// 归零。两条路径用 detail.actor 区分来源（admin / worker），kind 仍是冻结的
// reset（不新增 cancel_scheduled 式的第二套取值）。
//
// 【R-22 分批】按 id 游标分批（每批 ≤ ResetScanLimit）而不是一条无界 UPDATE：
// 大套餐下逐个订阅追加事件会让单事务持有大量行锁与 WAL。每批一个事务，
// 「状态迁移 + 事件」仍原子；循环用 id > afterID 推进，不会被同一批反复命中。
// 每批的 CTE 带 ORDER BY id + FOR UPDATE，并发调用按 id 顺序加锁，避免死锁。
func (r *SQLSubscriptionRepo) ResetPlanQuota(ctx context.Context, planID int64, advanceNextReset bool) (int64, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM subscription_plans WHERE id = $1)`, planID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: check subscription plan(%d): %w", planID, err)
	}
	if !exists {
		return 0, ErrPlanNotFound
	}
	var total int64
	var afterID int64
	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		var (
			batch  int64
			lastID int64
		)
		err := r.withEventTx(ctx, fmt.Sprintf("reset quota of plan(%d)", planID), func(tx *sql.Tx) error {
			// 每次尝试都从空切片开始：withEventTx 在 seq 冲突时会重跑整个闭包，
			// 若 ids/totals 声明在闭包外，重试会把同一批 id 再 append 一遍，
			// 导致事件重复写入、affected 翻倍（复核 Important 实测）。
			ids := make([]int64, 0, ResetScanLimit)
			totals := make([]int64, 0, ResetScanLimit)
			batch, lastID = 0, 0

			rows, err := tx.QueryContext(ctx, `
				WITH targets AS (
					SELECT id FROM subscriptions
					 WHERE plan_id = $1 AND status = 'active' AND id > $3
					 ORDER BY id
					 LIMIT $4
					 FOR UPDATE
				)
				UPDATE subscriptions s
				SET amount_used = 0,
				    next_reset_at = CASE
				        WHEN $2 AND s.next_reset_at IS NOT NULL
				        THEN s.next_reset_at + make_interval(days => s.period_days)
				        ELSE s.next_reset_at
				    END,
				    updated_at = now()
				FROM targets t
				WHERE s.id = t.id
				RETURNING s.id, s.amount_total`,
				planID, advanceNextReset, afterID, ResetScanLimit)
			if err != nil {
				return fmt.Errorf("store: reset quota of plan(%d): %w", planID, err)
			}
			for rows.Next() {
				var (
					id          int64
					amountTotal int64
				)
				if err := rows.Scan(&id, &amountTotal); err != nil {
					_ = rows.Close()
					return fmt.Errorf("store: scan reset subscription of plan(%d): %w", planID, err)
				}
				ids = append(ids, id)
				totals = append(totals, amountTotal)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: iterate reset subscriptions of plan(%d): %w", planID, err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("store: close reset subscriptions of plan(%d): %w", planID, err)
			}
			for i, id := range ids {
				if id > lastID {
					lastID = id
				}
				if err := AppendSubscriptionEvent(ctx, tx, id, SubscriptionEventReset, map[string]any{
					"amount_total": totals[i],
					"actor":        "admin",
				}); err != nil {
					return err
				}
			}
			batch = int64(len(ids))
			return nil
		})
		if err != nil {
			return total, err
		}
		total += batch
		if batch == 0 {
			return total, nil
		}
		afterID = lastID
	}
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

	// 【事件】创建实例与 created 事件同事务提交（S1/R-17）：订阅一出现就有一条
	// 「它是什么、从哪来」的账本，后续迁移的 seq 从 2 开始连续递增。
	var created Subscription
	err := r.withEventTx(ctx, fmt.Sprintf("create subscription(user=%d plan=%d)", in.UserID, in.PlanID),
		func(tx *sql.Tx) error {
			var planID int64
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM subscription_plans WHERE id = $1 FOR SHARE`, in.PlanID).Scan(&planID)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPlanNotFound
			}
			if err != nil {
				return fmt.Errorf("store: check subscription plan(%d): %w", in.PlanID, err)
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
					return ErrSubscriptionDuplicate
				case isCheckViolation(err), isInvalidTextRepresentation(err),
					isStringDataRightTruncation(err):
					return ErrSubscriptionInvalid
				}
				return fmt.Errorf("store: create subscription(user=%d plan=%d): %w",
					in.UserID, in.PlanID, err)
			}
			// detail 只含 id 与金额（R-18）：不写支付凭据、邮箱或任何用户输入。
			detail := map[string]any{
				"plan_id":           in.PlanID,
				"amount_total":      in.AmountTotal,
				"price_paid_amount": in.PricePaidAmount,
			}
			if in.OrderID != nil {
				detail["order_id"] = *in.OrderID
			}
			if err := AppendSubscriptionEvent(ctx, tx, s.ID, SubscriptionEventCreated, detail); err != nil {
				return err
			}
			created = s
			return nil
		})
	if err != nil {
		return Subscription{}, err
	}
	return created, nil
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
//
// 【G-DOM-30 ②】cancel_at_period_end=true 的实例**不享宽限**：用户已明确
// 不续费，周期末即终止（end_at > now 才可消费），与 ExpireDue 的到期口径
// 一致。若这里仍按 grace_days 放行，已取消实例会在「到期后」继续被资金
// 分派选中，而 ExpireDue 已把它标 expired 的竞态窗口里尤其危险。
func (r *SQLSubscriptionRepo) ListActiveSubscriptions(ctx context.Context, userID int64, now time.Time) ([]Subscription, error) {
	return r.querySubscriptions(ctx, `
		SELECT `+subscriptionColumns+` FROM subscriptions
		WHERE user_id = $1 AND status = 'active'
		  AND end_at + make_interval(days => grace_days) > $2
		  AND (NOT cancel_at_period_end OR end_at > $2)
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
//
// G-DOM-30 ②：周期末取消的实例只活到 end_at，不享宽限。
func (r *SQLSubscriptionRepo) HasActiveUserSubscription(ctx context.Context, userID int64, now time.Time) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE user_id = $1 AND status = 'active'
			  AND end_at + make_interval(days => grace_days) > $2
			  AND (NOT cancel_at_period_end OR end_at > $2)
		)`, userID, now.UTC()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: has active subscription(user=%d): %w", userID, err)
	}
	return ok, nil
}

// UserActiveSubscriptionsAllowWalletOverflow 实现 billing-08 §4.1：
// 任一活跃实例的 allow_wallet_overflow 为真即允许 subscription_first 回落钱包。
// G-DOM-30 ②：周期末取消的实例不享宽限，不能在到期后继续提供溢出能力。
func (r *SQLSubscriptionRepo) UserActiveSubscriptionsAllowWalletOverflow(ctx context.Context, userID int64, now time.Time) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE user_id = $1 AND status = 'active'
			  AND end_at + make_interval(days => grace_days) > $2
			  AND (NOT cancel_at_period_end OR end_at > $2)
			  AND allow_wallet_overflow
		)`, userID, now.UTC()).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: subscription wallet overflow(user=%d): %w", userID, err)
	}
	return ok, nil
}

// CancelAtPeriodEnd 置「周期末取消」：实例仍 active 至 end_at，只是不再续费
// （billing-08 §3.5 P1）。已置位时重复调用仍命中原行（幂等）。
//
// 【为什么改成行锁内先读后写】S1 要求取消与 cancelled 事件同事务（R-17）。
// 先 `SELECT ... FOR UPDATE` 再判定，才能区分三种 0 行语义（不存在 /
// 已终态 / 已置位），并且只有「本次真正把 false 改成 true」才追加事件：
// 幂等重放不应在事件流里留下第二条 cancelled。行锁与条件 UPDATE 等价地
// 串行化并发取消；Get 与 UPDATE 之间不再有状态竞争窗口。
func (r *SQLSubscriptionRepo) CancelAtPeriodEnd(ctx context.Context, id int64) error {
	return r.withEventTx(ctx, fmt.Sprintf("cancel subscription(%d) at period end", id), func(tx *sql.Tx) error {
		var (
			status  string
			already bool
		)
		err := tx.QueryRowContext(ctx, `
			SELECT status, cancel_at_period_end FROM subscriptions
			 WHERE id = $1 FOR UPDATE`, id).Scan(&status, &already)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read subscription(%d) for cancel at period end: %w", id, err)
		}
		if status != "active" {
			// 终态不能再置取消标记（与旧条件 UPDATE 的 0 行分支同语义）。
			return ErrSubscriptionNotActive
		}
		if already {
			return nil // 幂等：目标状态已达成，不重复追加事件。
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET cancel_at_period_end = true, updated_at = now()
			 WHERE id = $1`, id); err != nil {
			return fmt.Errorf("store: cancel subscription(%d) at period end: %w", id, err)
		}
		// detail 只放「取消何时生效」这一非敏感判别位：周期末取消不立即
		// 终结实例，与 CancelImmediately 的 cancelled 事件需要可区分。
		// R-20 裁决：detail.immediate 保留——R-18 的「只放 id 与金额」按
		// 意图规则理解（防敏感信息），且 kind 白名单冻结为八值、不为
		// 「周期末取消」新增第九种 kind。后来人不要改回无判别位的形式。
		return AppendSubscriptionEvent(ctx, tx, id, SubscriptionEventCancelled,
			map[string]any{"immediate": false})
	})
}

// CancelImmediately 立即取消（管理侧 P2，billing-08 §3.5）。
//
// 行锁内判定（S1）：
//   - 行不存在 → ErrSubscriptionNotFound；
//   - 已是 cancelled → nil（重复调用幂等，目标终态已达成，不再追加事件）；
//   - 已是 expired → ErrSubscriptionNotActive（终态不可迁移）；
//   - active → 置 cancelled 并同事务追加 cancelled(immediate=true) 事件。
func (r *SQLSubscriptionRepo) CancelImmediately(ctx context.Context, id int64) error {
	return r.withEventTx(ctx, fmt.Sprintf("cancel subscription(%d)", id), func(tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx,
			`SELECT status FROM subscriptions WHERE id = $1 FOR UPDATE`, id).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		if err != nil {
			return fmt.Errorf("store: read subscription(%d) status: %w", id, err)
		}
		switch status {
		case "cancelled":
			return nil
		case "active":
			if _, err := tx.ExecContext(ctx, `
				UPDATE subscriptions SET status = 'cancelled', updated_at = now()
				 WHERE id = $1`, id); err != nil {
				return fmt.Errorf("store: cancel subscription(%d): %w", id, err)
			}
			return AppendSubscriptionEvent(ctx, tx, id, SubscriptionEventCancelled,
				map[string]any{"immediate": true})
		default:
			return ErrSubscriptionNotActive
		}
	})
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
//
// 【S1 事件】每条被迁移的行在同一事务内追加 expired 事件。批量 UPDATE 用
// RETURNING id 取得**本次真正迁移**的行；并发/重复执行 affected=0 时不会
// 产生空事件。逐个追加而非一条批量 INSERT，是为了复用 AppendSubscriptionEvent
// 的行锁 + seq 生成口径（事件流必须连续无洞）。
//
// 【R-22 分批 + M3 加锁顺序】单批 ≤ ExpireScanLimit：无界批次会在积压时持有
// 全部到期行锁，阻塞这些实例上的预扣/结算/续费。CTE 里 `ORDER BY id` 让并发
// 调用以相同顺序加锁（不同顺序的批量 UPDATE 在行交错时会产生 40P01 死锁）。
// worker 循环调用本函数，直到某批返回不足 ExpireScanLimit（扫描集已清空）。
func (r *SQLSubscriptionRepo) ExpireDue(ctx context.Context, now time.Time) (int64, error) {
	var expired int64
	err := r.withEventTx(ctx, "expire subscriptions", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			WITH targets AS (
				SELECT id FROM subscriptions
				 WHERE status = 'active'
				   AND (
				         (cancel_at_period_end AND end_at <= $1)
				      OR (NOT cancel_at_period_end
				          AND end_at + make_interval(days => grace_days) <= $1)
				   )
				 ORDER BY id
				 LIMIT $2
				 FOR UPDATE
			)
			UPDATE subscriptions s
			SET status = 'expired', updated_at = now()
			FROM targets t
			WHERE s.id = t.id AND s.status = 'active'
			RETURNING s.id`, now.UTC(), ExpireScanLimit)
		if err != nil {
			return fmt.Errorf("store: expire subscriptions: %w", err)
		}
		ids := make([]int64, 0, 16)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: scan expiring subscription: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: iterate expiring subscriptions: %w", err)
		}
		// 必须先关闭结果集再发下一条语句：pgx 连接在结果集读完/关闭前不能复用。
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: close expiring subscriptions: %w", err)
		}
		for _, id := range ids {
			// detail 留空：到期原因完全由 kind 与订阅自身的 end_at/grace_days
			// 快照表达（R-18 只允许 id 与金额，不重复状态字段）。
			if err := AppendSubscriptionEvent(ctx, tx, id, SubscriptionEventExpired, nil); err != nil {
				return err
			}
		}
		expired = int64(len(ids))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return expired, nil
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
// 【S1 事件】本次真正重置（affected=1）时才追加 reset 事件，且与 UPDATE 同
// 事务：并发 worker / 重复 pass 中只有一个能命中条件 UPDATE，事件同样只出
// 一条，不会出现「同一周期两条 reset」。detail 带该实例本周期总额度（金额，
// R-18），供事后解释「重置回了多少额度」。
func (r *SQLSubscriptionRepo) ResetSubscriptionQuota(ctx context.Context, id int64, now time.Time) (int64, error) {
	var affected int64
	err := r.withEventTx(ctx, fmt.Sprintf("reset subscription(%d) quota", id), func(tx *sql.Tx) error {
		var amountTotal int64
		err := tx.QueryRowContext(ctx, `
			UPDATE subscriptions
			SET amount_used = 0,
			    next_reset_at = next_reset_at + make_interval(days =>
			        (FLOOR(EXTRACT(EPOCH FROM ($2 - next_reset_at))
			               / (period_days * 86400.0))::int + 1) * period_days),
			    updated_at = now()
			WHERE id = $1 AND status = 'active' AND reset_on_renew
			  AND next_reset_at IS NOT NULL AND next_reset_at <= $2
			  AND end_at > $2
			  AND period_days > 0
			RETURNING amount_total`, id, now.UTC()).Scan(&amountTotal)
		if errors.Is(err, sql.ErrNoRows) {
			affected = 0
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: reset subscription(%d) quota: %w", id, err)
		}
		affected = 1
		return AppendSubscriptionEvent(ctx, tx, id, SubscriptionEventReset, map[string]any{
			"amount_total": amountTotal,
			// R-21：与管理员批量重置同 kind，用 actor 区分来源（worker/admin）。
			"actor": "worker",
		})
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// SubscriptionTx 是订阅写方法参与**调用方事务**的最小执行面。
//
// P2a 续费的订单 succeeded 与实例扩展必须原子提交（billing-08 §3.3 方案甲）：
// 若 store 自开 *sql.Tx，会留下「订单已成功、end_at 未推进」的窗口。
// *sql.DB、*sql.Tx 与 gorm.ConnPool 都满足该接口；billing 在 GORM 事务内
// 传入 tx.Statement.ConnPool。
type SubscriptionTx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RenewalExtendInput 是一次手动续费要写入实例的**下单快照**。
//
// 快照列全部来自订单冻结的条件（R-D5-03），而不是结算时重读套餐：
// 管理员可能在等待支付期间改价/改额度，用户已付款的合同不能随之变化。
// UserID / PlanID 是归属与身份条件：只有「该用户的该套餐实例」才能被扩展，
// 防止订单元数据错配或篡改把续费额度打到别的实例上。
type RenewalExtendInput struct {
	SubscriptionID      int64
	UserID              int64
	PlanID              int64
	PlanTitleSnapshot   string
	PeriodDays          int32
	GrantCredits        int64
	ResetOnRenew        bool
	AllowWalletOverflow bool
	ModelScope          []byte
	ConcurrencyLimit    *int32
	GraceDays           int32
	Currency            string
	PricePaidAmount     int64
	Now                 time.Time
}

// RenewSubscription 在调用方事务内**条件扩展**一个续费实例（P2a）。
//
// 冻结语义（任务书 P2a）：
//   - `end_at = GREATEST(end_at, now) + period_days`：active 实例在当前
//     到期日上累加；expired 实例从 now 起算（续费后恢复 active，而不是
//     从早已过去的 end_at 再补一个周期）。
//   - `amount_used = 0`：续费视为新周期合同，已用额度归零。
//   - `next_reset_at`：reset_on_renew=true → 新的 end_at；false → NULL
//     （与 CreateSubscription 同口径：没有周期重置的实例不得留游标，
//     否则 DueForReset 会把它扫成永远到期待重置的僵尸行）。
//   - `status = 'active'` 且清 `cancel_at_period_end`：用户用续费表达了
//     「继续」的意图，周期末取消标记随新周期失效。
//   - 快照列（title/period/grant/overflow/scope/limit/grace/currency/
//     price）按本次订单快照更新：R-D5-03 对续费同样适用，续费是按**当前
//     套餐**订立的新周期合同，而不是沿用首购时的旧条件。
//
// 条件 `status IN ('active','expired')` 是唯一迁移守卫：cancelled 实例
// 不可续费（RowsAffected=0 → ErrSubscriptionNotActive）；重复调用由调用方
// 的订单状态守卫（pending→succeeded 只发生一次）兜住，本函数自身也因
// 返回完整行而可安全重放（幂等扩两次的唯一防线是订单状态）。
func RenewSubscription(ctx context.Context, q SubscriptionTx,
	in RenewalExtendInput) (Subscription, error) {

	if in.SubscriptionID <= 0 || in.UserID <= 0 || in.PlanID <= 0 ||
		in.PeriodDays < 1 || in.GrantCredits < 0 || in.GraceDays < 0 ||
		in.PricePaidAmount < 0 || strings.TrimSpace(in.PlanTitleSnapshot) == "" ||
		strings.TrimSpace(in.Currency) == "" {
		return Subscription{}, ErrSubscriptionInvalid
	}
	s, err := scanSubscription(q.QueryRowContext(ctx, `
		UPDATE subscriptions
		SET end_at = GREATEST(end_at, $2) + make_interval(days => $3),
		    amount_total = $4,
		    amount_used = 0,
		    next_reset_at = CASE WHEN $5
		        THEN GREATEST(end_at, $2) + make_interval(days => $3)
		        ELSE NULL END,
		    status = 'active',
		    cancel_at_period_end = false,
		    plan_title_snapshot = $6,
		    period_days = $3,
		    reset_on_renew = $5,
		    allow_wallet_overflow = $7,
		    grace_days = $8,
		    model_scope = $9,
		    concurrency_limit = $10,
		    currency = $11,
		    price_paid_amount = $12,
		    updated_at = now()
		WHERE id = $1 AND user_id = $13 AND plan_id = $14
		  AND status IN ('active', 'expired')
		RETURNING `+subscriptionColumns,
		in.SubscriptionID, in.Now.UTC(), in.PeriodDays, in.GrantCredits,
		in.ResetOnRenew, in.PlanTitleSnapshot, in.AllowWalletOverflow,
		in.GraceDays, nullableJSON(in.ModelScope), in.ConcurrencyLimit,
		in.Currency, in.PricePaidAmount, in.UserID, in.PlanID))
	if errors.Is(err, sql.ErrNoRows) {
		// 0 行：区分「不存在/归属或套餐不符」与「cancelled 终态」。
		// 归属不符与不存在统一 ErrSubscriptionNotFound，不泄露跨用户存在性。
		var status string
		findErr := q.QueryRowContext(ctx, `
			SELECT status FROM subscriptions
			 WHERE id = $1 AND user_id = $2 AND plan_id = $3`,
			in.SubscriptionID, in.UserID, in.PlanID).Scan(&status)
		if errors.Is(findErr, sql.ErrNoRows) {
			return Subscription{}, ErrSubscriptionNotFound
		}
		if findErr != nil {
			return Subscription{}, fmt.Errorf("store: read subscription(%d) for renewal: %w",
				in.SubscriptionID, findErr)
		}
		return Subscription{}, fmt.Errorf("%w: subscription %d is %s",
			ErrSubscriptionNotActive, in.SubscriptionID, status)
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: renew subscription(%d): %w", in.SubscriptionID, err)
	}
	// 【S1 事件】续费与 renewed 事件必须同事务：调用方（billing）把订单
	// pending→succeeded 与本次条件扩展放在同一 GORM 事务里，事件也落在
	// 这个事务中，支付成功但事件缺失/事件先于支付可见的窗口都不存在。
	// detail 只含 id 与金额（R-18），周期长度属于合同快照、可由订单回查。
	if err := AppendSubscriptionEvent(ctx, q, s.ID, SubscriptionEventRenewed, map[string]any{
		"plan_id":           in.PlanID,
		"amount_total":      in.GrantCredits,
		"price_paid_amount": in.PricePaidAmount,
	}); err != nil {
		return Subscription{}, err
	}
	return s, nil
}

// AppendSubscriptionEvent 在调用方事务内追加一条订阅状态迁移事件（S1/R-17）。
//
// 【为什么必须与状态变更同事务】事件是「这次迁移发生了什么」的账本。若先
// 提交状态再单独补事件，进程崩溃或连接断开就会留下「状态变了但没有事件」
// 或「有事件但状态没变」的不可解释历史，S2/S3 的退款/升降级判断也会读到
// 与状态不一致的事件流。原子提交是事件流作为审计/排障依据的前提。
//
// 【seq 为什么在订阅行锁内生成】seq 是同一订阅内连续无洞的序号，客户端用
// after_seq 增量拉取。并发两条迁移若各自 `SELECT MAX(seq)+1`，会读到同一个
// max 并撞唯一键 (subscription_id, seq)。先对本订阅行 `FOR UPDATE` 取得写
// 锁，把同一订阅的事件追加串行化，再计算 max+1；不同订阅之间互不阻塞。
//
// 【为什么锁的是订阅行而不是事件表】PostgreSQL 拒绝对聚合使用 FOR UPDATE
// （"FOR UPDATE is not allowed with aggregate functions"），所以 R-17 的
// 「`SELECT COALESCE(MAX(seq),0)+1 ... FOR UPDATE` 在订阅行锁内」实现为：
// 先锁 subscriptions 行，再在同一事务里做聚合查询。语义就是要求的
// 「以订阅为粒度的互斥」，且可执行。
//
// 【唯一键冲突】即使有行锁，仍把冲突翻译为 ErrSubscriptionEventSeqConflict，
// 让调用方整体重试事务（见 withEventTx / billing 的 subscriptionTx），
// 绝不跳号——跳号会让 after_seq 客户端漏事件。
func AppendSubscriptionEvent(ctx context.Context, tx SubscriptionTx,
	subscriptionID int64, kind string, detail map[string]any) error {

	if subscriptionID <= 0 {
		return ErrSubscriptionEventInvalid
	}
	if _, ok := subscriptionEventKinds[kind]; !ok {
		return fmt.Errorf("%w: unknown kind %q", ErrSubscriptionEventInvalid, kind)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		// detail 由服务端构造；编码失败说明放了不可编码的值（如 chan），
		// 属于编程错误，进事件流前就必须失败，不能落一条空 detail。
		return fmt.Errorf("%w: marshal detail: %v", ErrSubscriptionEventInvalid, err)
	}

	var locked int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM subscriptions WHERE id = $1 FOR UPDATE`, subscriptionID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSubscriptionNotFound
	}
	if err != nil {
		return fmt.Errorf("store: lock subscription(%d) for event: %w", subscriptionID, err)
	}

	var seq int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1
		  FROM subscription_events WHERE subscription_id = $1`, subscriptionID).Scan(&seq); err != nil {
		return fmt.Errorf("store: next subscription(%d) event seq: %w", subscriptionID, err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_events (subscription_id, seq, kind, detail)
		VALUES ($1, $2, $3, $4)`, subscriptionID, seq, kind, payload); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: subscription %d seq %d", ErrSubscriptionEventSeqConflict, subscriptionID, seq)
		}
		return fmt.Errorf("store: append subscription(%d) event %q: %w", subscriptionID, kind, err)
	}
	return nil
}

// withEventTx 执行「状态迁移 + 事件」事务，并在事件 seq 唯一键冲突时整体重试。
//
// 【为什么整体重试】迁移 UPDATE 与事件 INSERT 在同一事务里；冲突发生时事务
// 已被 PostgreSQL 标记为 aborted，只有在**新事务**里重新求值条件 UPDATE
// （幂等：重复执行 affected 可能为 0）才能安全重来。跳号或忽略事件都会破坏
// 「状态与事件一一对应」的契约。
//
// 回调执行时必须只使用事务内读到的数据，且不得有事务外副作用：重试会重新
// 执行整个闭包。
func (r *SQLSubscriptionRepo) withEventTx(ctx context.Context, op string, fn func(*sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < subscriptionEventTxAttempts; attempt++ {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: begin %s: %w", op, err)
		}
		err = fn(tx)
		if err == nil {
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("store: commit %s: %w", op, commitErr)
			}
			return nil
		}
		_ = tx.Rollback()
		if !errors.Is(err, ErrSubscriptionEventSeqConflict) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("store: %s after %d attempts: %w", op, subscriptionEventTxAttempts, lastErr)
}

// ListSubscriptionEvents 返回某订阅 seq > afterSeq 的事件，按 seq 升序。
//
// 可见性（本人或 users:read）是 HTTP 层职责：store 只按订阅 id 取数，
// 不做归属过滤，避免「数据层静默少返回」掩盖越权调用。
func (r *SQLSubscriptionRepo) ListSubscriptionEvents(ctx context.Context,
	subscriptionID int64, afterSeq int64, limit int) ([]SubscriptionEvent, error) {

	if subscriptionID <= 0 || afterSeq < 0 {
		return nil, ErrSubscriptionEventInvalid
	}
	if limit <= 0 || limit > SubscriptionEventScanLimit {
		limit = SubscriptionEventScanLimit
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT subscription_id, seq, kind, detail, created_at
		  FROM subscription_events
		 WHERE subscription_id = $1 AND seq > $2
		 ORDER BY seq ASC
		 LIMIT $3`, subscriptionID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list subscription(%d) events: %w", subscriptionID, err)
	}
	defer func() { _ = rows.Close() }()

	events := make([]SubscriptionEvent, 0, 16)
	for rows.Next() {
		var e SubscriptionEvent
		if err := rows.Scan(&e.SubscriptionID, &e.Seq, &e.Kind, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan subscription(%d) event: %w", subscriptionID, err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate subscription(%d) events: %w", subscriptionID, err)
	}
	return events, nil
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

// PreConsumeUserSubscription 在**同一事务**内完成订阅预扣的两件事：
//
//  1. 写/读 request_id 幂等记录（唯一键即幂等根，billing-04 §3.4）；
//  2. 对选定的单个实例执行条件 `amount_used` 增量（禁止跨实例拆分，
//     R-D5-02 —— 退款/兜底恢复只能按记录上的 subscription_id 回退）。
//
// 判定顺序与 InsertPreConsume 一致：已 refunded 再预扣报错；已 consumed
// 幂等返回既有行且不再扣额度（created=false）。额度不足 / 实例已不可消费
// 返回 ErrSubscriptionQuotaInsufficient，调用方继续下一个候选；由于整个
// 事务回滚，失败的尝试不会留下幂等记录或任何额度增量。
//
// 条件 UPDATE 的 `status='active'`、宽限判定与周期末取消判定在写入时重新
// 求值：列表选择与扣减之间的到期竞态不能让已终结实例继续被消费。
// G-DOM-30 ②：cancel_at_period_end=true 的实例不享宽限（end_at > now）。
func (r *SQLSubscriptionRepo) PreConsumeUserSubscription(ctx context.Context,
	requestID string, subscriptionID int64, amount int64) (SubscriptionPreConsume, bool, error) {

	if strings.TrimSpace(requestID) == "" || subscriptionID <= 0 || amount <= 0 {
		return SubscriptionPreConsume{}, false, ErrPreConsumeInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return SubscriptionPreConsume{}, false, fmt.Errorf("store: begin subscription pre-consume: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rec, err := scanPreConsume(tx.QueryRowContext(ctx, `
		INSERT INTO subscription_pre_consume_records
			(request_id, subscription_id, status, pre_consumed)
		VALUES ($1, $2, 'consumed', $3)
		ON CONFLICT (request_id) DO NOTHING
		RETURNING `+preConsumeColumns, requestID, subscriptionID, amount))
	switch {
	case err == nil:
		// 新记录：只有本次真正插入的一方可以增加 amount_used。
		// 上界守卫只在 amount_total > 0 时生效（0 = 无限额度，R-D5-07）；
		// 下界由 DB CHECK 与这里的加法共同保证。
		res, execErr := tx.ExecContext(ctx, `
			UPDATE subscriptions
			   SET amount_used = amount_used + $2, updated_at = now()
			 WHERE id = $1
			   AND status = 'active'
			   AND end_at + make_interval(days => grace_days) > now()
			   AND (NOT cancel_at_period_end OR end_at > now())
			   AND (amount_total = 0 OR amount_used + $2 <= amount_total)`,
			subscriptionID, amount)
		if execErr != nil {
			return SubscriptionPreConsume{}, false,
				fmt.Errorf("store: apply subscription pre-consume(%s): %w", requestID, execErr)
		}
		affected, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return SubscriptionPreConsume{}, false,
				fmt.Errorf("store: subscription pre-consume(%s) rows: %w", requestID, rowsErr)
		}
		if affected != 1 {
			// 事务随 rollback 一起撤销刚插入的幂等记录：候选耗尽必须能继续
			// 尝试下一个实例，不能被一条「已预扣」记录卡死。
			return SubscriptionPreConsume{}, false, ErrSubscriptionQuotaInsufficient
		}
		if commitErr := tx.Commit(); commitErr != nil {
			// 【未知窗口（LOW，已登记）】Commit 返回错误时事务的真实结果
			// 不可知：可能已提交（幂等记录与 amount_used 增量都已落库），
			// 也可能已回滚。调用方收到错误会回滚令牌额度并可能重试；若实际
			// 已提交，重试会命中幂等记录（created=false）不再扣额度，资金
			// 不会双扣，但本次请求的令牌回滚与订阅扣减可能不一致。数据库
			// 连接级故障下无法消除该窗口，按「未知则上抛、由调用方/worker
			// 对账」处理（见 未完成清单 G-DOM-31）。
			return SubscriptionPreConsume{}, false,
				fmt.Errorf("store: commit subscription pre-consume(%s): %w", requestID, commitErr)
		}
		return rec, true, nil
	case errors.Is(err, sql.ErrNoRows):
		// 唯一键冲突：读取既有行。不能用 DO UPDATE 返回：已 refunded
		// 的行必须报错而不是被改写回 consumed（幂等 ≠ 允许重放已退款请求）。
		existing, readErr := scanPreConsume(tx.QueryRowContext(ctx,
			`SELECT `+preConsumeColumns+` FROM subscription_pre_consume_records WHERE request_id = $1`,
			requestID))
		if readErr != nil {
			return SubscriptionPreConsume{}, false, fmt.Errorf("store: read subscription pre-consume(%s): %w", requestID, readErr)
		}
		if existing.Status == "refunded" {
			return SubscriptionPreConsume{}, false, ErrPreConsumeRefunded
		}
		return existing, false, nil
	default:
		if isInvalidTextRepresentation(err) {
			return SubscriptionPreConsume{}, false, ErrPreConsumeInvalid
		}
		return SubscriptionPreConsume{}, false,
			fmt.Errorf("store: insert subscription pre-consume(%s): %w", requestID, err)
	}
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

// MarkPreConsumeRefunded 把预扣记录置 refunded，并在同一事务内回退该实例
// 的 `amount_used`（请求失败退款，A 类；billing-08 §3.7/§4.5、TC-H-22）。
//
// 条件 UPDATE 只命中 consumed：已 refunded 时直接返回 nil（退款按 requestId
// 幂等）；行不存在才是 ErrPreConsumeNotFound。绝不从 refunded 回到 consumed。
//
// 先用 `FOR UPDATE` 锁住记录行再判定状态：两个并发退款只有一个能把
// consumed→refunded，第二个看到 refunded 后幂等返回，**不会**二次回退额度。
// 【M3 竞态守卫】amount_used 回退带 `amount_used >= $2` 条件：若周期重置
// 已把计数归零（或在飞请求与重置交叉），本次退款变成 no-op，绝不能用上一
// 周期的预扣额去抵扣新周期的已用量。代价是极端交错下少退一次（用户少得
// 一点额度，方向安全）；该残余已登记在未完成清单 G-DOM-32。
// GREATEST(...,0) 只作最后的下界兜底（与 DB CHECK 同口径）。
func (r *SQLSubscriptionRepo) MarkPreConsumeRefunded(ctx context.Context, requestID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin subscription refund(%s): %w", requestID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		subscriptionID int64
		status         string
		preConsumed    int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT subscription_id, status, pre_consumed
		  FROM subscription_pre_consume_records
		 WHERE request_id = $1
		 FOR UPDATE`, requestID).Scan(&subscriptionID, &status, &preConsumed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPreConsumeNotFound
	}
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return ErrPreConsumeInvalid
		}
		return fmt.Errorf("store: read subscription pre-consume(%s): %w", requestID, err)
	}
	if status == "refunded" {
		return nil // 幂等命中：额度已回退过，不能再退一次。
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE subscription_pre_consume_records SET status = 'refunded'
		WHERE request_id = $1 AND status = 'consumed'`, requestID); err != nil {
		return fmt.Errorf("store: mark subscription pre-consume(%s) refunded: %w", requestID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE subscriptions
		   SET amount_used = GREATEST(amount_used - $2, 0), updated_at = now()
		 WHERE id = $1 AND amount_used >= $2`, subscriptionID, preConsumed); err != nil {
		return fmt.Errorf("store: reverse subscription pre-consume(%s) usage: %w", requestID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit subscription refund(%s): %w", requestID, err)
	}
	return nil
}

// PostConsumeUserSubscriptionDelta 追加结算增量（billing-04 §3.5、billing-08 §5.1）。
//
// 语义（冻结）：
//   - newUsed = max(amount_used + delta, 0)：订阅已用不得为负；
//   - amount_total > 0 且 newUsed > amount_total → 拒绝（不得超卖）；调用方
//     保留预扣、不做部分结算；
//   - amount_total = 0 表示无限额度（R-D5-07），只守下界；
//   - 行锁（SELECT ... FOR UPDATE）让并发结算/退款串行化，避免 read-modify-write
//     丢更新。
//
// 【为什么返回整行而不是 error】成功日志需要同一次加锁读到的
// amount_total/amount_used/plan_title_snapshot；结算后另起一次查询会与并发
// 的预扣/重置产生不一致的「事后快照」。返回事务内看到的最终值，日志与账本
// 同源。
//
// 【delta=0】空 fact / 按次结算不改变额度，但仍返回实例快照供日志使用；
// 不加更新（避免无谓的 updated_at 写放大）。
func (r *SQLSubscriptionRepo) PostConsumeUserSubscriptionDelta(ctx context.Context,
	subscriptionID, delta int64) (Subscription, error) {
	if subscriptionID <= 0 {
		return Subscription{}, ErrSubscriptionInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Subscription{}, fmt.Errorf("store: begin subscription delta(%d): %w", subscriptionID, err)
	}
	defer func() { _ = tx.Rollback() }()

	sub, err := scanSubscription(tx.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = $1 FOR UPDATE`, subscriptionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrSubscriptionNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("store: lock subscription(%d): %w", subscriptionID, err)
	}

	newUsed := sub.AmountUsed
	switch {
	case delta > 0:
		if sub.AmountUsed > math.MaxInt64-delta {
			return Subscription{}, ErrSubscriptionDeltaOverflow
		}
		newUsed = sub.AmountUsed + delta
	case delta < 0:
		// 正数 + 负数不会溢出；结果可能落到 0 以下，按冻结语义钳到 0。
		newUsed = sub.AmountUsed + delta
		if newUsed < 0 {
			newUsed = 0
		}
	}
	if sub.AmountTotal > 0 && newUsed > sub.AmountTotal {
		// 事务回滚：本次 delta 完全不落地（不做部分结算）。
		return Subscription{}, fmt.Errorf("%w: subscription %d used=%d delta=%d total=%d",
			ErrSubscriptionSettleExceeded, subscriptionID, sub.AmountUsed, delta, sub.AmountTotal)
	}
	if newUsed != sub.AmountUsed {
		if _, err := tx.ExecContext(ctx, `
			UPDATE subscriptions SET amount_used = $2, updated_at = now()
			 WHERE id = $1`, subscriptionID, newUsed); err != nil {
			return Subscription{}, fmt.Errorf("store: apply subscription delta(%d): %w", subscriptionID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Subscription{}, fmt.Errorf("store: commit subscription delta(%d): %w", subscriptionID, err)
	}
	sub.AmountUsed = newUsed
	return sub, nil
}

// RefundSubscriptionIncrement 回退实时增量（ChargeIncrement）在本周期已追加的
// amount_used（M3）。
//
// 与 `PostConsumeUserSubscriptionDelta(-amount)` 的差别是条件 UPDATE：
// `amount_used >= $2` 未命中时不更新并返回 false，而不是把计数钳到 0。
// 周期重置会先把 amount_used 归零；若此时仍做减法，旧周期的增量回退会侵蚀
// 新周期已经发生的用量（用户新周期的额度被莫名恢复）。宁可少退一次（方向
// 安全：只会少给用户额度，不会多送也不会产生负值），残余登记在
// 未完成清单 G-DOM-34②/G-DOM-35。
func (r *SQLSubscriptionRepo) RefundSubscriptionIncrement(ctx context.Context,
	subscriptionID, amount int64) (bool, error) {
	if subscriptionID <= 0 || amount <= 0 {
		return false, ErrSubscriptionInvalid
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE subscriptions
		   SET amount_used = amount_used - $2, updated_at = now()
		 WHERE id = $1 AND amount_used >= $2`, subscriptionID, amount)
	if err != nil {
		return false, fmt.Errorf("store: reverse subscription increment(%d): %w", subscriptionID, err)
	}
	affected, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return false, fmt.Errorf("store: reverse subscription increment(%d) rows: %w", subscriptionID, rowsErr)
	}
	return affected == 1, nil
}

// ListStalePreConsumes 列出可被崩溃兜底退款的订阅预扣记录（billing-06 §5.2）。
//
// 判据：
//   - status='consumed'：仍在预扣中（refunded 已退过，幂等短路由仓储保证，
//     这里直接排除以减少扫描量）；
//   - created_at < before：调用方按可配置窗口（默认 15 分钟）计算；
//   - request_registry 中同 request_id 的记录不是 settled/refunded：
//     registry 行缺失也纳入（M3 尚未写该表；缺失只表示没有相反证据）。
//
// 【为什么用 NOT EXISTS 而不是 JOIN】registry 是可选证据面，缺失必须继续
// 处理；LEFT JOIN 需要额外的 NULL 判定，NOT EXISTS 直接表达「没有已结案
// 证据」。
//
// ORDER BY/LIMIT 让每轮扫描有界且顺序稳定（积压时先处理最旧的记录）。
func (r *SQLSubscriptionRepo) ListStalePreConsumes(ctx context.Context,
	before time.Time, limit int) ([]SubscriptionPreConsume, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT r.request_id, r.subscription_id, r.status, r.pre_consumed, r.created_at
		  FROM subscription_pre_consume_records r
		 WHERE r.status = 'consumed'
		   AND r.created_at < $1
		   AND NOT EXISTS (
		        SELECT 1 FROM request_registry rr
		         WHERE rr.request_id = r.request_id
		           AND rr.state IN ('settled', 'refunded'))
		 ORDER BY r.created_at ASC, r.request_id ASC
		 LIMIT $2`, before.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list stale subscription pre-consume records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]SubscriptionPreConsume, 0, limit)
	for rows.Next() {
		rec, scanErr := scanPreConsume(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan stale subscription pre-consume record: %w", scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate stale subscription pre-consume records: %w", err)
	}
	return out, nil
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
