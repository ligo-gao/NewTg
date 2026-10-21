// 订阅套餐端点（D5b；billing-08 §2/§3/§15，platform-04 §4.5 V10）。
//
// 端点面（§15 冻结契约）：
//
//	GET    /api/subscription-plans                    管理：列表（含 disabled）
//	POST   /api/subscription-plans                    管理：创建（plan_key 冲突 409）
//	GET    /api/subscription-plans/{id}               管理：详情
//	PUT    /api/subscription-plans/{id}               管理：全量更新（不追溯已购实例）
//	DELETE /api/subscription-plans/{id}               管理：删除（引用保护 409）
//	POST   /api/subscription-plans/{id}/enable        管理：启用（幂等）
//	POST   /api/subscription-plans/{id}/disable       管理：停用（幂等）
//	POST   /api/subscription-plans/{id}/reset-quota   管理：重置活跃实例额度
//	GET    /api/users/{id}/subscriptions              管理：单用户实例（users:read）
//	GET    /api/subscription/plans                    用户：可购套餐（仅 enabled）
//	GET    /api/subscriptions                         用户：本人实例（含终态）
//	GET    /api/subscriptions/{id}/events             用户：状态迁移事件流（本人或 users:read）
//	POST   /api/subscriptions/{id}/cancel             用户：周期末取消（幂等；终态 409）
//	POST   /api/subscriptions/{id}/renew              用户：手动续费（必须 Idempotency-Key）
//	GET    /api/billing-preference                    用户：扣费偏好
//	PUT    /api/billing-preference                    用户：扣费偏好（四值归一）
//	POST   /api/subscription/purchase                 用户：购买（创建支付订单）
//
// 设计约束：
//   - 管理端点全部要求 settings:manage（§15.1）；写动作登记字段白名单，未知字段
//     由 RBAC 字段层直接 403（绝不静默丢弃）。
//   - 用户侧一律以 Principal.UserID 为唯一数据范围：接口不接收 user_id，也不存在
//     可枚举他人实例的路径（§15.4）。管理侧单用户列表以 users:read 做功能级门。
//   - 周期末取消只置 cancel_at_period_end，不立即作废额度、不退款（§3.5 P1）；
//     归属校验先于状态判定，他人实例与不存在统一 404，终态 409。
//   - 购买端点在 S3 接通支付：订单创建/渠道载荷/幂等全部委托
//     billing.TopupService（与充值同一套实现），本层只做请求形态与错误映射。
//     金额/币种/周期/额度一律由服务端按套餐行读取，不信任请求体。
//   - 所有错误体 RFC 7807；字段校验错误额外带 problem.fields（与定价端点同构）。
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const (
	// subscriptionPlanDefaultPageSize / max 复用全库 offset 分页口径（§15.3）。
	subscriptionPlanDefaultPageSize = 20
	subscriptionPlanMaxPageSize     = 100
	subscriptionPlanMaxPage         = 1_000_000

	subscriptionPlanTitleMaxRunes       = 64
	subscriptionPlanDescriptionMaxRunes = 255
	subscriptionPlanScopeEntryMaxRunes  = 128
)

// planKeyPattern 是 §2.1 / 任务书冻结的稳定外部键字符集。改名不改键；
// 大写与下划线都不接受，避免同一套餐因拼写变体出现两个外部标识。
var planKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// RFC 7807 type URI。冲突类用独立 type 让前端可机读区分「键冲突」与
// 「引用保护」，不必解析人类可读 detail。
const (
	problemTypePlanKeyConflict = "https://newtg.dev/problems/plan_key_conflict"
	problemTypePlanReferenced  = "https://newtg.dev/problems/plan_referenced"
	// 订阅购买（S3）：invalid=请求体/字段非法，conflict=套餐不可购买或
	// 幂等键复用到另一份订单，unavailable=支付渠道未配置。
	problemTypePurchaseInvalid     = "https://newtg.dev/problems/invalid_subscription_purchase"
	problemTypePurchaseConflict    = "https://newtg.dev/problems/subscription_purchase_conflict"
	problemTypePurchaseUnavailable = "https://newtg.dev/problems/subscription_purchase_unavailable"
	// 余额购买（P2b）余额不足：402，与中继的 insufficient_quota 同语义
	// （钱包不得为负）。独立 type 让前端可直接引导充值。
	problemTypePurchaseInsufficient = "https://newtg.dev/problems/insufficient_wallet_balance"
	// 用户周期末取消（billing-08 §3.5 P1）：实例已进入 expired/cancelled
	// 终态时不能再置位。独立 type 让前端可区分「不能取消」与「找不到」。
	problemTypeSubscriptionNotActive = "https://newtg.dev/problems/subscription_not_active"
	// 手动续费（P2a）：invalid=请求体/字段非法，conflict=实例不可续费
	// （cancelled）或幂等键复用到另一份订单，unavailable=支付渠道未配置。
	problemTypeRenewInvalid     = "https://newtg.dev/problems/invalid_subscription_renewal"
	problemTypeRenewConflict    = "https://newtg.dev/problems/subscription_renewal_conflict"
	problemTypeRenewUnavailable = "https://newtg.dev/problems/subscription_renewal_unavailable"
	// 实例处于 cancelled 终态：与「不能取消」分开，前端可据此提示重新购买。
	problemTypeSubscriptionNotRenewable = "https://newtg.dev/problems/subscription_not_renewable"
)

// SubscriptionPurchaseService 是订阅购买的支付接缝。
//
// 生产实现为 *billing.TopupService；接口化让 handler 单测能注入桩，
// 同时避免 httpapi 直接构造 GORM 事务。
type SubscriptionPurchaseService interface {
	CreateSubscriptionOrder(ctx context.Context, userID, planID int64,
		provider, idempotencyKey string) (*billing.TopupOrder, error)
	// CreateSubscriptionRenewalOrder 创建手动续费订单（P2a）。支付成功后由
	// billing 的结算分支推进**同一实例**的 end_at，不新建 subscriptions 行。
	CreateSubscriptionRenewalOrder(ctx context.Context, userID, subscriptionID int64,
		provider, idempotencyKey string) (*billing.TopupOrder, error)
}

// 订阅域动作（字段策略按动作登记；与权限键不同）。
var (
	subscriptionPlanListAction     = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "list"}
	subscriptionPlanReadAction     = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "read"}
	subscriptionPlanCreateAction   = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "create"}
	subscriptionPlanUpdateAction   = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "update"}
	subscriptionPlanDeleteAction   = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "delete"}
	subscriptionPlanStatusAction   = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "status"}
	subscriptionPlanResetAction    = rbac.AuthzAction{ResourceKind: "subscription_plan", ActionName: "reset_quota"}
	adminUserSubscriptionsAction   = rbac.AuthzAction{ResourceKind: "subscription", ActionName: "admin_list_by_user"}
	userBillingPreferenceAction    = rbac.AuthzAction{ResourceKind: "billing_preference", ActionName: "update"}
	userSubscriptionPurchaseAction = rbac.AuthzAction{ResourceKind: "subscription", ActionName: "purchase"}
	userSubscriptionCancelAction   = rbac.AuthzAction{ResourceKind: "subscription", ActionName: "cancel"}
	userSubscriptionRenewAction    = rbac.AuthzAction{ResourceKind: "subscription", ActionName: "renew"}
)

// SubscriptionHandlerConfig 是订阅端点装配输入。
//
// Repo 必填（生产为 store.NewSubscriptionRepo(DB)）。DB 用于 users.billing_preference
// 的读写：D5a 的仓储接缝刻意只覆盖订阅表，偏好属于 users 行，不引入第二个
// 订阅仓储方法（也避免改 store/ 的并行收口）。Redis 用于创建幂等。
type SubscriptionHandlerConfig struct {
	Repo   store.SubscriptionRepo
	DB     *sql.DB
	Redis  *redis.Client
	Authz  *rbac.AuthzMiddleware
	Audit  *audit.Logger
	Logger *slog.Logger
	// Purchase 是订阅购买的支付接缝（生产为 *billing.TopupService）。
	// nil 时购买端点返回 503（fail closed），不会静默创建订单。
	Purchase SubscriptionPurchaseService
}

// SubscriptionHandlers 持有订阅端点依赖，可并发使用。
type SubscriptionHandlers struct {
	repo     store.SubscriptionRepo
	db       *sql.DB
	rdb      *redis.Client
	authz    *rbac.AuthzMiddleware
	audit    *audit.Logger
	log      *slog.Logger
	purchase SubscriptionPurchaseService
}

// NewSubscriptionHandlers 构造订阅端点。
func NewSubscriptionHandlers(cfg SubscriptionHandlerConfig) (*SubscriptionHandlers, error) {
	if cfg.Repo == nil {
		return nil, errors.New("httpapi: SubscriptionHandlerConfig.Repo is required")
	}
	if cfg.DB == nil {
		// 偏好读写没有内存兜底：把「数据库故障」伪装成默认偏好会让用户以为
		// 设置已保存。装配期拒绝，而不是运行期静默回退。
		return nil, errors.New("httpapi: SubscriptionHandlerConfig.DB is required (billing preference)")
	}
	if cfg.Redis == nil {
		// 创建幂等依赖 Redis；缺失即失去重放保护。
		return nil, errors.New("httpapi: SubscriptionHandlerConfig.Redis is required (idempotency)")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &SubscriptionHandlers{
		repo: cfg.Repo, db: cfg.DB, rdb: cfg.Redis, authz: authz,
		audit: cfg.Audit, log: logger, purchase: cfg.Purchase,
	}
	h.registerPolicies()
	return h, nil
}

// registerPolicies 登记写动作字段白名单。未登记动作对任何写请求都 403
// （security-02 §2.6.1 规则 6）。
func (h *SubscriptionHandlers) registerPolicies() {
	planFields := func() *rbac.FieldPolicy {
		return &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
			"plan_key":              nil,
			"title":                 nil,
			"description":           nil,
			"period_days":           nil,
			"price_amount":          nil,
			"currency":              nil,
			"grant_credits":         nil,
			"reset_on_renew":        nil,
			"allow_wallet_overflow": nil,
			"model_scope":           nil, // 元素结构由 handler 校验
			"concurrency_limit":     nil,
			"grace_days":            nil,
			"status":                nil,
			"sort_order":            nil,
		}}
	}
	h.authz.SetWritePolicy(subscriptionPlanCreateAction, planFields())
	h.authz.SetWritePolicy(subscriptionPlanUpdateAction, planFields())
	// 删除/启停没有可写字段：空白名单让任何携带字段的 body 都 403，
	// 无 body 的请求照常通过（与令牌/定价同名动作同构）。
	empty := &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}}
	h.authz.SetWritePolicy(subscriptionPlanDeleteAction, empty)
	h.authz.SetWritePolicy(subscriptionPlanStatusAction, empty)
	h.authz.SetWritePolicy(subscriptionPlanResetAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"advance_next_reset": nil,
	}})
	h.authz.SetWritePolicy(userBillingPreferenceAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"preference": nil,
	}})
	h.authz.SetWritePolicy(userSubscriptionPurchaseAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"plan_id":  nil,
		"provider": nil,
	}})
	// 续费只需 provider：目标实例来自路径，金额/周期一律由服务端按套餐行读取。
	h.authz.SetWritePolicy(userSubscriptionRenewAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"provider": nil,
	}})
	// 周期末取消没有可写字段：空白名单 = 无 body 的请求通过，携带任何字段
	// 的 body 一律 403 field_not_writable（与启停/删除同构）。
	h.authz.SetWritePolicy(userSubscriptionCancelAction, empty)
}

// Register 把全部订阅端点挂到受保护子树（SessionAuth → CSRF → Origin 链之后）。
//
// 用户端写接口也走 authz.Handler("", action)：required 为空表示「只要求已认证」，
// 但字段层仍然生效——未知字段 403 是端点契约，不是管理面专属。
func (h *SubscriptionHandlers) Register(mux *http.ServeMux) {
	admin := func(action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(rbac.PermSettingsManage, action)(next)
	}
	usersRead := func(action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(rbac.PermUsersRead, action)(next)
	}
	self := func(action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler("", action)(next)
	}

	// 管理面：套餐 CRUD / 启停 / 重置。
	mux.Handle("GET /api/subscription-plans", admin(subscriptionPlanListAction, h.handleListPlans))
	mux.Handle("POST /api/subscription-plans",
		admin(subscriptionPlanCreateAction, h.idempotentCreate(h.handleCreatePlan)))
	mux.Handle("GET /api/subscription-plans/{id}", admin(subscriptionPlanReadAction, h.handleGetPlan))
	mux.Handle("PUT /api/subscription-plans/{id}", admin(subscriptionPlanUpdateAction, h.handleUpdatePlan))
	mux.Handle("DELETE /api/subscription-plans/{id}", admin(subscriptionPlanDeleteAction, h.handleDeletePlan))
	mux.Handle("POST /api/subscription-plans/{id}/enable", admin(subscriptionPlanStatusAction, h.handlePlanEnable))
	mux.Handle("POST /api/subscription-plans/{id}/disable", admin(subscriptionPlanStatusAction, h.handlePlanDisable))
	mux.Handle("POST /api/subscription-plans/{id}/reset-quota", admin(subscriptionPlanResetAction, h.handleResetPlanQuota))
	mux.Handle("GET /api/users/{id}/subscriptions", usersRead(adminUserSubscriptionsAction, h.handleListUserSubscriptions))

	// 用户面：数据范围恒为 Principal.UserID（§15.4）。
	mux.Handle("GET /api/subscription/plans", http.HandlerFunc(h.handleListAvailablePlans))
	mux.Handle("GET /api/subscriptions", http.HandlerFunc(h.handleListMySubscriptions))
	// 事件流读取（S1/R-19）：可见性为「本人或 users:read」。这里不挂 authz
	// 权限 wrapper——同一个端点对普通用户与管理员都开放，真正的数据范围判定
	// 在 handler 内（先归属后 404）。缺少身份时 handler fail-closed 401。
	mux.Handle("GET /api/subscriptions/{id}/events",
		http.HandlerFunc(h.handleListSubscriptionEvents))
	mux.Handle("POST /api/subscriptions/{id}/cancel",
		self(userSubscriptionCancelAction, h.handleCancelSubscriptionAtPeriodEnd))
	// 续费与取消同为「本人实例 + 幂等写」：cancelled → 409，active/expired 可续。
	mux.Handle("POST /api/subscriptions/{id}/renew",
		self(userSubscriptionRenewAction, h.handleRenewSubscription))
	mux.Handle("GET /api/billing-preference", http.HandlerFunc(h.handleGetBillingPreference))
	mux.Handle("PUT /api/billing-preference", self(userBillingPreferenceAction, h.handlePutBillingPreference))
	mux.Handle("POST /api/subscription/purchase", self(userSubscriptionPurchaseAction, h.handlePurchase))
}

// ---------------------------------------------------------------------------
// 响应 DTO
// ---------------------------------------------------------------------------

type subscriptionPlanView struct {
	ID                  int64     `json:"id"`
	PlanKey             string    `json:"plan_key"`
	Title               string    `json:"title"`
	Description         *string   `json:"description"`
	PeriodDays          int32     `json:"period_days"`
	PriceAmount         int64     `json:"price_amount"`
	Currency            string    `json:"currency"`
	GrantCredits        int64     `json:"grant_credits"`
	ResetOnRenew        bool      `json:"reset_on_renew"`
	AllowWalletOverflow bool      `json:"allow_wallet_overflow"`
	ModelScope          []string  `json:"model_scope"`
	ConcurrencyLimit    *int32    `json:"concurrency_limit"`
	GraceDays           int32     `json:"grace_days"`
	Status              string    `json:"status"`
	SortOrder           int32     `json:"sort_order"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type subscriptionPlanListView struct {
	Items    []subscriptionPlanView `json:"items"`
	Total    int64                  `json:"total"`
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
}

// availableSubscriptionPlanView 是用户侧套餐卡片的投影。
//
// 【为什么不复用 admin 视图】allow_wallet_overflow 是内部回退开关
// （billing-08 §15.4 明确用户侧不返回）；管理视图把它暴露给用户会泄露
// 「订阅耗尽后是否允许扣钱包」的实现策略。
type availableSubscriptionPlanView struct {
	ID               int64    `json:"id"`
	PlanKey          string   `json:"plan_key"`
	Title            string   `json:"title"`
	Description      *string  `json:"description"`
	PeriodDays       int32    `json:"period_days"`
	PriceAmount      int64    `json:"price_amount"`
	Currency         string   `json:"currency"`
	GrantCredits     int64    `json:"grant_credits"`
	ResetOnRenew     bool     `json:"reset_on_renew"`
	ModelScope       []string `json:"model_scope"`
	ConcurrencyLimit *int32   `json:"concurrency_limit"`
	GraceDays        int32    `json:"grace_days"`
	SortOrder        int32    `json:"sort_order"`
}

type availableSubscriptionPlanListView struct {
	Items []availableSubscriptionPlanView `json:"items"`
}

func newAvailableSubscriptionPlanView(rec store.SubscriptionPlan) availableSubscriptionPlanView {
	return availableSubscriptionPlanView{
		ID: rec.ID, PlanKey: rec.PlanKey, Title: rec.Title, Description: rec.Description,
		PeriodDays: rec.PeriodDays, PriceAmount: rec.PriceAmount, Currency: rec.Currency,
		GrantCredits: rec.GrantCredits, ResetOnRenew: rec.ResetOnRenew,
		ModelScope: decodeModelScope(rec.ModelScope), ConcurrencyLimit: rec.ConcurrencyLimit,
		GraceDays: rec.GraceDays, SortOrder: rec.SortOrder,
	}
}

// userSubscriptionView 是 §15.4 的用户侧投影：order_id/价格/内部回退开关
// 一律不出现，避免把钱包回退策略与订单号暴露给用户。
type userSubscriptionView struct {
	ID                int64      `json:"id"`
	PlanTitleSnapshot string     `json:"plan_title_snapshot"`
	Status            string     `json:"status"`
	StartAt           time.Time  `json:"start_at"`
	EndAt             time.Time  `json:"end_at"`
	NextResetAt       *time.Time `json:"next_reset_at"`
	AmountTotal       int64      `json:"amount_total"`
	AmountUsed        int64      `json:"amount_used"`
	// 【omitempty 是契约的一部分】amount_total=0 表示无限额度，此时没有分母，
	// amount_remain 必须整个省略而不是返回 0/null（user.yaml/admin.yaml 同口径）。
	// 指针 nil + omitempty 正好覆盖 amountRemain 的 nil 分支；有限额度时指针
	// 非 nil，字段照常输出（含 0）。
	AmountRemain      *int64    `json:"amount_remain,omitempty"`
	AutoRenew         bool      `json:"auto_renew"`
	CancelAtPeriodEnd bool      `json:"cancel_at_period_end"`
	PeriodDays        int32     `json:"period_days"`
	GraceDays         int32     `json:"grace_days"`
	ModelScope        []string  `json:"model_scope"`
	ConcurrencyLimit  *int32    `json:"concurrency_limit"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// adminSubscriptionView 额外带 user_id 与订单/价格列（管理对账口径）。
type adminSubscriptionView struct {
	userSubscriptionView
	UserID              int64  `json:"user_id"`
	PlanID              int64  `json:"plan_id"`
	AllowWalletOverflow bool   `json:"allow_wallet_overflow"`
	OrderID             *int64 `json:"order_id"`
	Currency            string `json:"currency"`
	PricePaidAmount     int64  `json:"price_paid_amount"`
}

type userSubscriptionListView struct {
	Items    []userSubscriptionView `json:"items"`
	Total    int64                  `json:"total"`
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
}

type adminSubscriptionListView struct {
	Items    []adminSubscriptionView `json:"items"`
	Total    int64                   `json:"total"`
	Page     int                     `json:"page"`
	PageSize int                     `json:"page_size"`
}

type billingPreferenceView struct {
	Preference string `json:"preference"`
}

// subscriptionResetQuotaView 只返回受影响实例数（审计同样只记计数）。
type subscriptionResetQuotaView struct {
	Affected int64 `json:"affected"`
}

func newSubscriptionPlanView(rec store.SubscriptionPlan) subscriptionPlanView {
	return subscriptionPlanView{
		ID: rec.ID, PlanKey: rec.PlanKey, Title: rec.Title, Description: rec.Description,
		PeriodDays: rec.PeriodDays, PriceAmount: rec.PriceAmount, Currency: rec.Currency,
		GrantCredits: rec.GrantCredits, ResetOnRenew: rec.ResetOnRenew,
		AllowWalletOverflow: rec.AllowWalletOverflow, ModelScope: decodeModelScope(rec.ModelScope),
		ConcurrencyLimit: rec.ConcurrencyLimit, GraceDays: rec.GraceDays, Status: rec.Status,
		SortOrder: rec.SortOrder, CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}
}

// decodeModelScope 把 jsonb 原文转成视图。NULL（未设置 = 全部模型）返回 nil；
// 显式 '[]' 返回空切片——两者语义不同，不能互相顶替（见 store.nullableJSON）。
func decodeModelScope(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if out == nil {
		return []string{}
	}
	return out
}

func amountRemain(total, used int64) *int64 {
	if total <= 0 {
		return nil // 0 = 无限额度，没有分母
	}
	remain := total - used
	if remain < 0 {
		// 在飞请求的多退少补可能让 used 暂时越过 total；展示下界取 0，
		// 不把负数当作可透支额度（结算侧仍按 DB 计数）。
		remain = 0
	}
	return &remain
}

func newUserSubscriptionView(rec store.Subscription) userSubscriptionView {
	return userSubscriptionView{
		ID: rec.ID, PlanTitleSnapshot: rec.PlanTitleSnapshot, Status: rec.Status,
		StartAt: rec.StartAt, EndAt: rec.EndAt, NextResetAt: rec.NextResetAt,
		AmountTotal: rec.AmountTotal, AmountUsed: rec.AmountUsed,
		AmountRemain: amountRemain(rec.AmountTotal, rec.AmountUsed),
		AutoRenew:    rec.AutoRenew, CancelAtPeriodEnd: rec.CancelAtPeriodEnd,
		PeriodDays: rec.PeriodDays, GraceDays: rec.GraceDays,
		ModelScope: decodeModelScope(rec.ModelScope), ConcurrencyLimit: rec.ConcurrencyLimit,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt,
	}
}

func newAdminSubscriptionView(rec store.Subscription) adminSubscriptionView {
	return adminSubscriptionView{
		userSubscriptionView: newUserSubscriptionView(rec),
		UserID:               rec.UserID, PlanID: rec.PlanID,
		AllowWalletOverflow: rec.AllowWalletOverflow, OrderID: rec.OrderID,
		Currency: rec.Currency, PricePaidAmount: rec.PricePaidAmount,
	}
}

// ---------------------------------------------------------------------------
// 请求解析与校验
// ---------------------------------------------------------------------------

// subscriptionPlanPayload 用指针区分「未提交」与「显式零值」。PUT 是全量更新，
// 但 status 例外：启停已有独立幂等端点，省略 status 时保留当前值，避免一次
// 编辑意外把套餐停用（或重新启用）。
type subscriptionPlanPayload struct {
	PlanKey             *string         `json:"plan_key"`
	Title               *string         `json:"title"`
	Description         *string         `json:"description"`
	PeriodDays          *int32          `json:"period_days"`
	PriceAmount         *int64          `json:"price_amount"`
	Currency            *string         `json:"currency"`
	GrantCredits        *int64          `json:"grant_credits"`
	ResetOnRenew        *bool           `json:"reset_on_renew"`
	AllowWalletOverflow *bool           `json:"allow_wallet_overflow"`
	ModelScope          json.RawMessage `json:"model_scope"`
	ConcurrencyLimit    *int32          `json:"concurrency_limit"`
	GraceDays           *int32          `json:"grace_days"`
	SortOrder           *int32          `json:"sort_order"`
	Status              *string         `json:"status"`
}

// normalizedPlan 是校验/归一后的写入值。
type normalizedPlan struct {
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
	SortOrder           int32
	Status              string
}

// fieldProblem 是单字段校验错误；聚合后一次返回，前端可逐项标红。
type fieldProblem struct {
	Field   string
	Message string
}

func writeSubscriptionFieldsProblem(w http.ResponseWriter, status int, problemType, title, detail string, fields []string) {
	writePricingProblemWithFields(w, status, problemType, title, detail, fields)
}

// decodePlanPayload 读取 body。类型/结构错误属于 400；取值域错误留给
// validatePlanPayload 映射 422。
func decodePlanPayload(r *http.Request) (subscriptionPlanPayload, error) {
	var payload subscriptionPlanPayload
	if err := readAuthJSON(r, &payload); err != nil {
		return payload, err
	}
	return payload, nil
}

// validatePlanPayload 把原始 payload 归一为写入值。
//
// 返回 (normalized, missing, fieldProblems)。missing 非空 → 400（参数缺失）；
// fieldProblems 非空 → 422（取值非法，带 fields[]）。
func validatePlanPayload(p subscriptionPlanPayload, currentStatus string) (normalizedPlan, []string, []fieldProblem) {
	var out normalizedPlan
	var missing []string
	var problems []fieldProblem
	add := func(field, message string) { problems = append(problems, fieldProblem{field, message}) }

	if p.PlanKey == nil {
		missing = append(missing, "plan_key")
	}
	if p.Title == nil {
		missing = append(missing, "title")
	}
	if p.PeriodDays == nil {
		missing = append(missing, "period_days")
	}
	if p.PriceAmount == nil {
		missing = append(missing, "price_amount")
	}
	if p.Currency == nil {
		missing = append(missing, "currency")
	}
	if p.GrantCredits == nil {
		missing = append(missing, "grant_credits")
	}

	if p.PlanKey != nil {
		out.PlanKey = strings.TrimSpace(*p.PlanKey)
		if !planKeyPattern.MatchString(out.PlanKey) {
			add("plan_key", "plan_key 必须匹配 ^[a-z0-9][a-z0-9-]{0,63}$")
		}
	}
	if p.Title != nil {
		out.Title = strings.TrimSpace(*p.Title)
		if n := utf8.RuneCountInString(out.Title); n < 1 || n > subscriptionPlanTitleMaxRunes {
			add("title", "title 必须为 1-64 个字符")
		}
	}
	if p.Description != nil {
		trimmed := strings.TrimSpace(*p.Description)
		if utf8.RuneCountInString(trimmed) > subscriptionPlanDescriptionMaxRunes {
			add("description", "description 最多 255 个字符")
		} else if trimmed == "" {
			out.Description = nil
		} else {
			out.Description = &trimmed
		}
	}
	if p.PeriodDays != nil {
		out.PeriodDays = *p.PeriodDays
		if out.PeriodDays < 1 {
			add("period_days", "period_days 必须 >= 1")
		}
	}
	if p.PriceAmount != nil {
		out.PriceAmount = *p.PriceAmount
		if out.PriceAmount < 0 {
			add("price_amount", "price_amount 必须 >= 0")
		}
	}
	if p.Currency != nil {
		out.Currency = strings.TrimSpace(*p.Currency)
		if out.Currency != "CNY" {
			add("currency", "currency 仅支持 CNY")
		}
	}
	if p.GrantCredits != nil {
		out.GrantCredits = *p.GrantCredits
		if out.GrantCredits < 0 {
			add("grant_credits", "grant_credits 必须 >= 0（0 = 无限额度）")
		}
	}

	// 可选字段默认值与 DB 默认一致；显式值原样保留。
	out.ResetOnRenew = true
	if p.ResetOnRenew != nil {
		out.ResetOnRenew = *p.ResetOnRenew
	}
	out.AllowWalletOverflow = true
	if p.AllowWalletOverflow != nil {
		out.AllowWalletOverflow = *p.AllowWalletOverflow
	}
	if p.GraceDays != nil {
		out.GraceDays = *p.GraceDays
		if out.GraceDays < 0 {
			add("grace_days", "grace_days 必须 >= 0")
		}
	}
	if p.ConcurrencyLimit != nil {
		value := *p.ConcurrencyLimit
		if value < 1 {
			add("concurrency_limit", "concurrency_limit 必须为 null 或 >= 1")
		} else {
			out.ConcurrencyLimit = &value
		}
	}
	if p.SortOrder != nil {
		out.SortOrder = *p.SortOrder
	}

	out.Status = currentStatus
	if out.Status == "" {
		out.Status = "enabled"
	}
	if p.Status != nil {
		out.Status = strings.TrimSpace(*p.Status)
	}
	if out.Status != "enabled" && out.Status != "disabled" {
		add("status", "status 必须是 enabled 或 disabled")
	}

	// model_scope：缺省/null = 全部模型（SQL NULL）；数组 = 白名单。
	// 空数组显式表示「没有任何模型可用」，与 NULL 语义不同，必须原样保留。
	if raw := strings.TrimSpace(string(p.ModelScope)); raw != "" && raw != "null" {
		var entries []string
		if err := json.Unmarshal(p.ModelScope, &entries); err != nil {
			add("model_scope", "model_scope 必须是字符串数组或 null")
		} else {
			seen := make(map[string]struct{}, len(entries))
			cleaned := make([]string, 0, len(entries))
			scopeErr := false
			for _, entry := range entries {
				model := strings.TrimSpace(entry)
				if model == "" || utf8.RuneCountInString(model) > subscriptionPlanScopeEntryMaxRunes {
					add("model_scope", "model_scope 元素必须为非空字符串且不超过 128 个字符")
					scopeErr = true
					break
				}
				if _, dup := seen[model]; dup {
					// 去重而不是报错：模型清单是集合语义，重复项无意义，
					// 保留首次出现顺序即可。
					continue
				}
				seen[model] = struct{}{}
				cleaned = append(cleaned, model)
			}
			if !scopeErr {
				encoded, err := json.Marshal(cleaned)
				if err != nil {
					add("model_scope", "model_scope 无法编码")
				} else {
					out.ModelScope = encoded
				}
			}
		}
	}

	return out, missing, problems
}

func (p normalizedPlan) createInput() store.SubscriptionPlanInput {
	reset := p.ResetOnRenew
	overflow := p.AllowWalletOverflow
	status := p.Status
	return store.SubscriptionPlanInput{
		PlanKey: p.PlanKey, Title: p.Title, Description: p.Description,
		PeriodDays: p.PeriodDays, PriceAmount: p.PriceAmount, Currency: p.Currency,
		GrantCredits: p.GrantCredits, ResetOnRenew: &reset,
		AllowWalletOverflow: &overflow, ModelScope: p.ModelScope,
		ConcurrencyLimit: p.ConcurrencyLimit, GraceDays: p.GraceDays,
		Status: &status, SortOrder: p.SortOrder,
	}
}

func (p normalizedPlan) updateInput() store.SubscriptionPlanUpdate {
	return store.SubscriptionPlanUpdate{
		PlanKey: p.PlanKey, Title: p.Title, Description: p.Description,
		PeriodDays: p.PeriodDays, PriceAmount: p.PriceAmount, Currency: p.Currency,
		GrantCredits: p.GrantCredits, ResetOnRenew: p.ResetOnRenew,
		AllowWalletOverflow: p.AllowWalletOverflow, ModelScope: p.ModelScope,
		ConcurrencyLimit: p.ConcurrencyLimit, GraceDays: p.GraceDays,
		Status: p.Status, SortOrder: p.SortOrder,
	}
}

// parsePlanPathID 解析套餐/订阅路径 id。非法 id 在契约里等同于不存在（404），
// 不把它暴露成 400：ID 枚举不应获得比真实 404 更多的信息。
func parsePlanPathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// parseSubscriptionPage 复用全库 offset 分页口径（p 1 基，page_size 默认 20）。
func parseSubscriptionPage(r *http.Request) (int, int, error) {
	page, pageSize := 1, subscriptionPlanDefaultPageSize
	query := r.URL.Query()
	if raw := strings.TrimSpace(query.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > subscriptionPlanMaxPage {
			return 0, 0, fmt.Errorf("p must be between 1 and %d", subscriptionPlanMaxPage)
		}
		page = value
	}
	if raw := strings.TrimSpace(query.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > subscriptionPlanMaxPageSize {
			return 0, 0, fmt.Errorf("page_size must be between 1 and %d", subscriptionPlanMaxPageSize)
		}
		pageSize = value
	}
	return page, pageSize, nil
}

func paginateBounds(total, page, pageSize int) (int, int) {
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func (h *SubscriptionHandlers) writeRepoError(w http.ResponseWriter, op string, err error) {
	var referenced store.ErrPlanReferenced
	switch {
	case errors.Is(err, store.ErrPlanNotFound), errors.Is(err, store.ErrSubscriptionNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, store.ErrPlanDuplicate):
		writeAdminProblem(w, http.StatusConflict, problemTypePlanKeyConflict, "plan_key_conflict",
			"a subscription plan with this plan_key already exists")
	case errors.As(err, &referenced):
		// 引用计数是删除保护的机读依据：实例数 + 未终结订阅订单数（D5a）。
		// 单次写出完整 problem，额外带 reference_count / order_reference_count；
		// problem.yaml 允许 additionalProperties，因此这是契约内扩展。
		writePlanReferencedProblem(w, referenced)
	case errors.Is(err, store.ErrPlanInvalid):
		writeSubscriptionFieldsProblem(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", "subscription plan fields are invalid", nil)
	case errors.Is(err, store.ErrBillingPreferenceInvalid):
		writeSubscriptionFieldsProblem(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", "billing preference is invalid", []string{"preference"})
	default:
		h.log.Error(op+" failed", "error", err)
		writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
	}
}

// writePlanReferencedProblem 输出 409 引用保护，额外带两类计数。
//
// 【为什么不用 writeAdminProblem】dto.Problem 没有计数字段；同一次响应里
// 「先写 409 再补字段」在 net/http 里不可能（header 已发出），因此在这里
// 直接编码完整 problem。type/title 与冻结契约逐字一致。
func writePlanReferencedProblem(w http.ResponseWriter, referenced store.ErrPlanReferenced) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	instanceCount := referenced.ReferenceCount - referenced.OrderReferenceCount
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":                  problemTypePlanReferenced,
		"title":                 "plan_referenced",
		"status":                http.StatusConflict,
		"detail":                fmt.Sprintf("plan is referenced by %d instance(s) and %d open order(s)", instanceCount, referenced.OrderReferenceCount),
		"reference_count":       referenced.ReferenceCount,
		"order_reference_count": referenced.OrderReferenceCount,
	})
}

// ---------------------------------------------------------------------------
// 管理端：套餐 CRUD
// ---------------------------------------------------------------------------

func (h *SubscriptionHandlers) handleListPlans(w http.ResponseWriter, r *http.Request) {
	page, pageSize, err := parseSubscriptionPage(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	q := r.URL.Query()
	status := strings.TrimSpace(q.Get("status"))
	if status != "" && status != "enabled" && status != "disabled" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"status must be enabled or disabled")
		return
	}
	keyword := strings.ToLower(strings.TrimSpace(q.Get("keyword")))

	plans, err := h.repo.ListPlans(r.Context(), status)
	if err != nil {
		h.writeRepoError(w, "list subscription plans", err)
		return
	}
	filtered := plans[:0]
	for _, plan := range plans {
		if keyword != "" &&
			!strings.Contains(strings.ToLower(plan.PlanKey), keyword) &&
			!strings.Contains(strings.ToLower(plan.Title), keyword) {
			continue
		}
		filtered = append(filtered, plan)
	}
	total := len(filtered)
	start, end := paginateBounds(total, page, pageSize)
	items := make([]subscriptionPlanView, 0, end-start)
	for _, plan := range filtered[start:end] {
		items = append(items, newSubscriptionPlanView(plan))
	}
	writeJSON(w, http.StatusOK, subscriptionPlanListView{
		Items: items, Total: int64(total), Page: page, PageSize: pageSize,
	})
}

func (h *SubscriptionHandlers) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	payload, err := decodePlanPayload(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object with valid field types")
		return
	}
	normalized, missing, problems := validatePlanPayload(payload, "")
	if len(missing) > 0 {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"missing required fields: "+strings.Join(missing, ", "))
		return
	}
	if len(problems) > 0 {
		fields := make([]string, 0, len(problems))
		for _, problem := range problems {
			fields = append(fields, problem.Field)
		}
		writeSubscriptionFieldsProblem(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", problems[0].Message, fields)
		return
	}
	plan, err := h.repo.CreatePlan(r.Context(), normalized.createInput())
	if err != nil {
		h.writeRepoError(w, "create subscription plan", err)
		return
	}
	h.auditSubscriptionEvent(r, p, audit.ActionSubscriptionPlanCreate, map[string]any{
		"id": plan.ID, "plan_key": plan.PlanKey,
	}, http.StatusCreated)
	writeJSON(w, http.StatusCreated, newSubscriptionPlanView(plan))
}

func (h *SubscriptionHandlers) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePlanPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	plan, err := h.repo.GetPlan(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "get subscription plan", err)
		return
	}
	writeJSON(w, http.StatusOK, newSubscriptionPlanView(plan))
}

func (h *SubscriptionHandlers) handleUpdatePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	// 先读当前行：省略 status 时保留原状态；不存在则直接 404（而不是先解析
	// body 再报 404，避免对不存在资源的校验开销与信息差）。
	current, err := h.repo.GetPlan(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "get subscription plan before update", err)
		return
	}
	payload, err := decodePlanPayload(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object with valid field types")
		return
	}
	normalized, missing, problems := validatePlanPayload(payload, current.Status)
	if len(missing) > 0 {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"missing required fields: "+strings.Join(missing, ", "))
		return
	}
	if len(problems) > 0 {
		fields := make([]string, 0, len(problems))
		for _, problem := range problems {
			fields = append(fields, problem.Field)
		}
		writeSubscriptionFieldsProblem(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", problems[0].Message, fields)
		return
	}
	plan, err := h.repo.UpdatePlan(r.Context(), id, normalized.updateInput())
	if err != nil {
		h.writeRepoError(w, "update subscription plan", err)
		return
	}
	// 编辑不追溯已购实例：实例持有购买瞬间的快照（R-D5-03），审计只记标识。
	h.auditSubscriptionEvent(r, p, audit.ActionSubscriptionPlanUpdate, map[string]any{
		"id": plan.ID, "plan_key": plan.PlanKey,
	}, http.StatusOK)
	writeJSON(w, http.StatusOK, newSubscriptionPlanView(plan))
}

func (h *SubscriptionHandlers) handleDeletePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if err := h.repo.DeletePlan(r.Context(), id); err != nil {
		h.writeRepoError(w, "delete subscription plan", err)
		return
	}
	h.auditSubscriptionEvent(r, p, audit.ActionSubscriptionPlanDelete, map[string]any{"id": id}, http.StatusNoContent)
	w.WriteHeader(http.StatusNoContent)
}

func (h *SubscriptionHandlers) handlePlanEnable(w http.ResponseWriter, r *http.Request) {
	h.handlePlanStatus(w, r, true)
}

func (h *SubscriptionHandlers) handlePlanDisable(w http.ResponseWriter, r *http.Request) {
	h.handlePlanStatus(w, r, false)
}

// handlePlanStatus 启停是幂等的：重复启停命中同一行并返回当前视图
// （billing-08 §15.3「affected=0 视为已达成」）。
func (h *SubscriptionHandlers) handlePlanStatus(w http.ResponseWriter, r *http.Request, enable bool) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	var err error
	action := audit.ActionSubscriptionPlanDisable
	if enable {
		err = h.repo.EnablePlan(r.Context(), id)
		action = audit.ActionSubscriptionPlanEnable
	} else {
		err = h.repo.DisablePlan(r.Context(), id)
	}
	if err != nil {
		h.writeRepoError(w, "set subscription plan status", err)
		return
	}
	plan, err := h.repo.GetPlan(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "get subscription plan after status change", err)
		return
	}
	h.auditSubscriptionEvent(r, p, action, map[string]any{
		"id": plan.ID, "plan_key": plan.PlanKey,
	}, http.StatusOK)
	writeJSON(w, http.StatusOK, newSubscriptionPlanView(plan))
}

func (h *SubscriptionHandlers) handleResetPlanQuota(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	advance := false
	// body 可选：字段策略已保证只有 advance_next_reset 能出现。
	//
	// 【为什么不用 ContentLength 判定】ContentLength 是客户端可省略的提示：
	// chunked 编码或未知长度时它是 -1，空 body 会被误判成「有 body」并返回 400；
	// 反过来伪造 ContentLength>0 的空 body 也会把「无 body」变成解析错误。
	// 唯一可靠口径是读取后的实际内容：TrimSpace 为空即视为无 body。
	if r.Body != nil {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"unable to read request body")
			return
		}
		if len(strings.TrimSpace(string(raw))) != 0 {
			var payload struct {
				AdvanceNextReset *bool `json:"advance_next_reset"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
					"advance_next_reset must be a boolean")
				return
			}
			if payload.AdvanceNextReset != nil {
				advance = *payload.AdvanceNextReset
			}
		}
	}
	plan, err := h.repo.GetPlan(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "get subscription plan before reset", err)
		return
	}
	affected, err := h.repo.ResetPlanQuota(r.Context(), id, advance)
	if err != nil {
		h.writeRepoError(w, "reset subscription plan quota", err)
		return
	}
	h.auditSubscriptionEvent(r, p, audit.ActionSubscriptionPlanResetQuota, map[string]any{
		"id": id, "plan_key": plan.PlanKey, "affected": affected,
	}, http.StatusOK)
	writeJSON(w, http.StatusOK, subscriptionResetQuotaView{Affected: affected})
}

func (h *SubscriptionHandlers) handleListUserSubscriptions(w http.ResponseWriter, r *http.Request) {
	userID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || userID <= 0 {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	page, pageSize, err := parseSubscriptionPage(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	subs, err := h.repo.ListSubscriptionsByUser(r.Context(), userID)
	if err != nil {
		h.writeRepoError(w, "list user subscriptions", err)
		return
	}
	// 数据层已按 user_id 过滤；这里先做一次归属断言再计算 total/分页，
	// 防止未来仓储实现漂移把他人数据带进 users:read 管理视图。顺序很重要：
	// 若先分页再在循环里 continue，total 会把被剔除的行也算进去，出现
	// total != len(items) 且分页错位（同一页可能只剩 0 条）。
	owned := make([]store.Subscription, 0, len(subs))
	for _, sub := range subs {
		if sub.UserID != userID {
			continue
		}
		owned = append(owned, sub)
	}
	total := len(owned)
	start, end := paginateBounds(total, page, pageSize)
	items := make([]adminSubscriptionView, 0, end-start)
	for _, sub := range owned[start:end] {
		items = append(items, newAdminSubscriptionView(sub))
	}
	writeJSON(w, http.StatusOK, adminSubscriptionListView{
		Items: items, Total: int64(total), Page: page, PageSize: pageSize,
	})
}

// ---------------------------------------------------------------------------
// 用户端：可购套餐 / 我的订阅
// ---------------------------------------------------------------------------

func (h *SubscriptionHandlers) handleListAvailablePlans(w http.ResponseWriter, r *http.Request) {
	// 该端点没有 authz 字段/权限 wrapper（可购列表不按用户分片），因此必须
	// 自己 fail-closed：中间件缺失或顺序错误时绝不能把零值身份当匿名放行。
	// 与其它用户端点同一约定（adminPrincipal → 401）。
	if _, ok := adminPrincipal(w, r); !ok {
		return
	}
	plans, err := h.repo.ListPlans(r.Context(), "enabled")
	if err != nil {
		h.writeRepoError(w, "list available subscription plans", err)
		return
	}
	items := make([]availableSubscriptionPlanView, 0, len(plans))
	for _, plan := range plans {
		items = append(items, newAvailableSubscriptionPlanView(plan))
	}
	writeJSON(w, http.StatusOK, availableSubscriptionPlanListView{Items: items})
}

func (h *SubscriptionHandlers) handleListMySubscriptions(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	page, pageSize, err := parseSubscriptionPage(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	// 数据范围只来自 Principal：不读任何客户端可控的用户标识。
	subs, err := h.repo.ListSubscriptionsByUser(r.Context(), p.UserID)
	if err != nil {
		h.writeRepoError(w, "list my subscriptions", err)
		return
	}
	// 同管理侧：先按 Principal 归属过滤再算 total/分页，保证
	// total == len(items)（单页场景）不会被未来的仓储漂移破坏。
	owned := make([]store.Subscription, 0, len(subs))
	for _, sub := range subs {
		if sub.UserID != p.UserID {
			continue
		}
		owned = append(owned, sub)
	}
	total := len(owned)
	start, end := paginateBounds(total, page, pageSize)
	items := make([]userSubscriptionView, 0, end-start)
	for _, sub := range owned[start:end] {
		items = append(items, newUserSubscriptionView(sub))
	}
	writeJSON(w, http.StatusOK, userSubscriptionListView{
		Items: items, Total: int64(total), Page: page, PageSize: pageSize,
	})
}

// subscriptionEventLimit 是事件流单次返回上限（store 侧再兜底一次）。
// 事件只增不减；after_seq 让客户端分页推进，而不是一次拉完整段历史。
const subscriptionEventLimit = 500

// subscriptionEventView 是一条状态迁移事件。detail 透传 jsonb 对象，
// 字段集由写入方按 R-18 冻结（只有 id 与金额等非敏感值）。
type subscriptionEventView struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"created_at"`
}

type subscriptionEventListView struct {
	Events []subscriptionEventView `json:"events"`
}

// handleListSubscriptionEvents 实现 GET /api/subscriptions/{id}/events?after_seq=
// （S1/R-19）。
//
// 可见性：本人或持 users:read 的管理员。他人订阅返回 **404 而不是 403**——
// 与既有取消/续费的归属口径一致（§15.4）：403 会告诉攻击者「这个 ID 存在，
// 只是不属于你」，形成订阅 ID 枚举信道。为了做到这一点，必须先读订阅做归属
// 判定再取事件；不存在与无权访问走同一响应。
func (h *SubscriptionHandlers) handleListSubscriptionEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	afterSeq, err := parseAfterSeq(r)
	if err != nil {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request", err.Error())
		return
	}
	sub, err := h.repo.GetSubscription(r.Context(), id)
	if err != nil {
		// 不存在 → 404；其它仓储错误 → 500。
		h.writeRepoError(w, "get subscription for events", err)
		return
	}
	if sub.UserID != p.UserID && !rbac.HasPermission(*p, rbac.PermUsersRead) {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	events, err := h.repo.ListSubscriptionEvents(r.Context(), id, afterSeq, subscriptionEventLimit)
	if err != nil {
		h.writeRepoError(w, "list subscription events", err)
		return
	}
	items := make([]subscriptionEventView, 0, len(events))
	for _, event := range events {
		detail := json.RawMessage(event.Detail)
		if len(detail) == 0 {
			// store 写入时归一为 {}；这里对历史/异常行做同口径兜底，
			// 保证响应契约里 detail 永远是对象而不是 null。
			detail = json.RawMessage(`{}`)
		}
		items = append(items, subscriptionEventView{
			Seq: event.Seq, Kind: event.Kind, Detail: detail, CreatedAt: event.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, subscriptionEventListView{Events: items})
}

// parseAfterSeq 解析增量游标：缺省为 0（从头拉取）；负数/非数字是客户端
// 错误 → 400，而不是静默当作 0（静默归一会让分页实现永远重复第一页）。
func parseAfterSeq(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("after_seq"))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return value, nil
}

// handleCancelSubscriptionAtPeriodEnd 实现 billing-08 §15.2 的用户侧 P1
// 「周期末取消」：只置 cancel_at_period_end，不立即作废额度、不退款。
//
// 归属与状态分两步判定，刻意不把 owner 条件塞进 store 的条件 UPDATE：
//   - GetSubscription 先做归属校验；他人实例与不存在统一 404，避免用状态码
//     枚举订阅 ID（§15.4 的不泄漏口径）。
//   - 终态（expired/cancelled）不满足条件 UPDATE 的 status='active'，由 store
//     返回 ErrSubscriptionNotActive → 409；active 但已置位时 UPDATE 仍命中行，
//     因此重复调用保持 200（幂等，§15.3）。
//   - Get 与 UPDATE 之间的状态竞争由 store 的受影响行数兜底：若期间到期，
//     同样落到 409 而不是假装成功。
func (h *SubscriptionHandlers) handleCancelSubscriptionAtPeriodEnd(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		// 非法 id 与不存在同口径：不给出「格式错」这一额外信号。
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}

	sub, err := h.repo.GetSubscription(r.Context(), id)
	if err != nil {
		// ErrSubscriptionNotFound → 404；其余仓储错误 → 500。
		h.writeRepoError(w, "get subscription for cancel", err)
		return
	}
	// 非本人一律 404：不能因为「存在但不属于你」返回 403 而泄露 ID 存在性。
	if sub.UserID != p.UserID {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if sub.Status != "active" {
		writeCancelNotActiveProblem(w)
		return
	}

	if err := h.repo.CancelAtPeriodEnd(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, store.ErrSubscriptionNotFound):
			writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		case errors.Is(err, store.ErrSubscriptionNotActive):
			writeCancelNotActiveProblem(w)
		default:
			h.log.Error("cancel subscription at period end failed", "subscription_id", id, "error", err)
			writeAdminProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error", "")
		}
		return
	}

	// 返回更新后的用户侧投影（不含 user_id/order_id/价格）。重新读取而不是
	// 本地改副本，确保 updated_at 与条件 UPDATE 的事实一致。
	updated, err := h.repo.GetSubscription(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "reload subscription after cancel", err)
		return
	}
	// 审计 detail 仅 id；用户侧动作与套餐管理共用同一审计口径。
	h.auditSubscriptionEvent(r, p, audit.ActionSubscriptionCancel, map[string]any{"id": id}, http.StatusOK)
	writeJSON(w, http.StatusOK, newUserSubscriptionView(updated))
}

func writeCancelNotActiveProblem(w http.ResponseWriter) {
	writeAdminProblem(w, http.StatusConflict, problemTypeSubscriptionNotActive,
		"subscription_not_active", "subscription is no longer active and cannot be cancelled at period end")
}

// ---------------------------------------------------------------------------
// 用户端：扣费偏好
// ---------------------------------------------------------------------------

// normalizeBillingPreference 实现 §4.2 的空值/未知值归一：四个冻结字面量
// 以外的一切输入（含空串）都落到默认 subscription_first。不做 422 是设计
// 裁决——偏好是幂等的用户设置，旧客户端或迁移遗留值不应让设置页永久报错。
//
// 【单一真源】文本 ↔ 枚举的对照表只存在于 store.ParseBillingPreference /
// BillingPreferenceText（store/enum.go 明确禁止另写 switch 或按下标转换）。
// 这里本地再写一份四值 switch 会在新增取值时静默漂移，而且一旦有人把
// contract 枚举误当文本，就会在资金路径上把订阅偏好改写成钱包偏好。
func normalizeBillingPreference(raw string) string {
	preference, err := store.ParseBillingPreference(strings.TrimSpace(raw))
	if err != nil {
		return store.BillingPreferenceSubscriptionFirst
	}
	text, err := store.BillingPreferenceText(preference)
	if err != nil {
		return store.BillingPreferenceSubscriptionFirst
	}
	return text
}

func (h *SubscriptionHandlers) handleGetBillingPreference(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	preference, err := h.readBillingPreference(r.Context(), p.UserID)
	if err != nil {
		h.writeRepoError(w, "read billing preference", err)
		return
	}
	writeJSON(w, http.StatusOK, billingPreferenceView{Preference: preference})
}

func (h *SubscriptionHandlers) handlePutBillingPreference(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	var payload struct {
		Preference *string `json:"preference"`
	}
	if err := readAuthJSON(r, &payload); err != nil {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "preference must be a string", []string{"preference"})
		return
	}
	if payload.Preference == nil {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "preference is required", []string{"preference"})
		return
	}
	before, err := h.readBillingPreference(r.Context(), p.UserID)
	if err != nil {
		h.writeRepoError(w, "read billing preference before update", err)
		return
	}
	after := normalizeBillingPreference(*payload.Preference)
	result, err := h.db.ExecContext(r.Context(),
		`UPDATE users SET billing_preference = $2, updated_at = now() WHERE id = $1`,
		p.UserID, after)
	if err != nil {
		h.writeRepoError(w, "update billing preference", err)
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		h.writeRepoError(w, "update billing preference rows", err)
		return
	}
	if affected == 0 {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	// 审计只记枚举值（before/after），不含任何请求上下文或用户资料。
	if err := writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     audit.ActionBillingPreferenceUpdate,
		Actor:      p.UserID,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: http.StatusOK,
		Before:     adminMarshalRaw(map[string]any{"preference": before}),
		After:      adminMarshalRaw(map[string]any{"preference": after}),
	}); err != nil {
		h.log.Warn("billing preference audit write failed", "actor", p.UserID, "error", err)
	}
	writeJSON(w, http.StatusOK, billingPreferenceView{Preference: after})
}

func (h *SubscriptionHandlers) readBillingPreference(ctx context.Context, userID int64) (string, error) {
	var preference string
	err := h.db.QueryRowContext(ctx,
		`SELECT billing_preference FROM users WHERE id = $1`, userID).Scan(&preference)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrSubscriptionNotFound
	}
	if err != nil {
		return "", fmt.Errorf("httpapi: read billing preference(user=%d): %w", userID, err)
	}
	// DB CHECK 保证四值，但历史/手工数据仍可能越界；按 §4.2 归一而不是
	// 把损坏值原样返回给前端。
	return normalizeBillingPreference(preference), nil
}

// ---------------------------------------------------------------------------
// 用户端：购买（S3）
// ---------------------------------------------------------------------------

// subscriptionPurchaseRequest 是 POST /api/subscription/purchase 的请求体。
//
// 用指针区分「缺失」与零值：plan_id=0 / provider="" 必须 400（参数缺失），
// 而不是落到服务端当作「套餐 0 不存在」的 409。
type subscriptionPurchaseRequest struct {
	PlanID   *int64  `json:"plan_id"`
	Provider *string `json:"provider"`
}

// 订阅购买的固定窗口限流（任务书 S3 收口 M1，冻结值）。
//
// 维度是「用户 × 套餐」而不是用户或 IP：一次购买只针对一个套餐，按套餐
// 隔离既不会让用户浏览/切换套餐时误伤，也能拦住「同一套餐反复下单占用
// 渠道」的自动化重试。键形 `rl:subpurchase:<userID>:<planID>` 是冻结契约
// （运维排障与 Redis 快照都按它检索），不得加前缀层级。
//
// 失败策略 fail-closed：限流状态是购买防重的第一道闸，Redis 不可用时无法
// 证明「未超限」，宁可 503 + Retry-After 也不能放行（与登录/注册同口径，
// security-07 §2.3）。幂等重放同样计数：固定窗口只限制请求频率，不改变
// 「同 key 返回同一订单」的语义。
const (
	subscriptionPurchaseRateKeyPrefix = "rl:subpurchase:"
	subscriptionPurchaseRateLimit     = 5
	subscriptionPurchaseRateWindow    = time.Minute
)

// subscriptionPurchaseRateKey 构造冻结键形 rl:subpurchase:<userID>:<planID>。
func subscriptionPurchaseRateKey(userID, planID int64) string {
	return subscriptionPurchaseRateKeyPrefix + strconv.FormatInt(userID, 10) + ":" +
		strconv.FormatInt(planID, 10)
}

// handlePurchase 创建订阅购买订单（billing-08 §3.2/§15.2，S3）。
//
// 金额/币种/周期/额度全部由 billing 按套餐行读取；本层只校验请求形态。
// Idempotency-Key 必带：缺失即 400，因为服务端替客户端生成 key 会让重试
// 变成新订单（§3.2-3 的整个防重语义失效）。
//
// 0 元套餐由服务端在同一事务内直接建订单+实例，响应仍是同一结构，
// 只是没有 client_secret/qr_code（前端据此进入「已生效」而不是收银台）。
func (h *SubscriptionHandlers) handlePurchase(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypePurchaseInvalid,
			"Invalid subscription purchase", "Idempotency-Key header is required", []string{"Idempotency-Key"})
		return
	}
	var req subscriptionPurchaseRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypePurchaseInvalid,
			"Invalid subscription purchase", "request body must be a JSON object", nil)
		return
	}
	var missing []string
	if req.PlanID == nil {
		missing = append(missing, "plan_id")
	}
	if req.Provider == nil || strings.TrimSpace(*req.Provider) == "" {
		missing = append(missing, "provider")
	}
	if len(missing) > 0 {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypePurchaseInvalid,
			"Invalid subscription purchase", "plan_id and provider are required", missing)
		return
	}
	if *req.PlanID <= 0 {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypePurchaseInvalid,
			"Invalid subscription purchase", "plan_id must be a positive integer", []string{"plan_id"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(*req.Provider))
	// wallet 是余额购买（P2b）：仍是订阅购买订单，仍要求 Idempotency-Key 与
	// 同一限流；服务端在同一事务内扣款+履约，响应没有支付凭据。
	if provider != billing.ProviderStripe && provider != billing.ProviderAlipay &&
		provider != billing.ProviderWallet && provider != billing.ProviderEpay {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypePurchaseInvalid,
			"Invalid subscription purchase", "provider must be stripe, alipay, epay or wallet", []string{"provider"})
		return
	}
	if h.purchase == nil {
		// fail closed：没有支付接缝时绝不静默建单。503 表示「暂时不可用」，
		// 前端可重试；不是 500（服务端缺陷）也不是 501（功能未实现）。
		w.Header().Set("Retry-After", "5")
		writeSubscriptionFieldsProblem(w, http.StatusServiceUnavailable, problemTypePurchaseUnavailable,
			"Subscription purchase is unavailable", "the payment service is not configured", nil)
		return
	}

	// 限流放在请求形态校验之后、创建订单之前：非法请求不消耗配额；
	// 合法请求（含幂等重放）按「用户 × 套餐」计数。
	over, retryAfter, rateErr := allowRequestRate(r.Context(), h.rdb,
		subscriptionPurchaseRateKey(p.UserID, *req.PlanID),
		subscriptionPurchaseRateLimit, subscriptionPurchaseRateWindow)
	if rateErr != nil {
		// fail-closed：无法判定配额时拒绝，而不是放行（security-07 §2.3）。
		h.log.Error("subscription purchase rate limiter failed",
			"user_id", p.UserID, "plan_id", *req.PlanID, "error", rateErr)
		w.Header().Set("Retry-After", "5")
		writeSubscriptionFieldsProblem(w, http.StatusServiceUnavailable, problemTypePurchaseUnavailable,
			"Subscription purchase is unavailable", "the request rate limiter is unavailable", nil)
		return
	}
	if over {
		writeRateLimited(w, retryAfter, "Too many subscription purchase requests")
		return
	}

	result, err := h.purchase.CreateSubscriptionOrder(r.Context(), p.UserID, *req.PlanID, provider, key)
	if err != nil {
		h.writePurchaseError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, createOrderResponse{
		Order:        topupOrderViewFrom(result.Order),
		ClientSecret: result.ClientSecret,
		QRCode:       result.QRCode,
		PayURL:       result.PayURL,
		FormFields:   result.FormFields,
	})
}

// writePurchaseError 把 billing 的购买错误映射为 RFC 7807。
//
// 状态码口径（任务书冻结裁决 6）：
//   - 套餐被删/禁用、幂等键复用到另一份订单                        → 409
//   - 币种不符/金额非法/套餐字段损坏                                → 422
//   - 渠道未启用或配置不完整（provider 未配置）                     → 503
//   - 渠道出站失败（已建单、PaymentIntent/precreate 失败）          → 502
//
// 任何失败都不会留下 succeeded 订单或 subscriptions 行：服务端在同一事务
// 内完成订单状态与实例创建，失败整体回滚。
func (h *SubscriptionHandlers) writePurchaseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrInsufficientQuota):
		// 余额购买（P2b）：条件 UPDATE 未命中，事务已整体回滚（订单/实例/
		// 流水都没有写入）。402 与中继额度不足同口径。
		writeSubscriptionFieldsProblem(w, http.StatusPaymentRequired,
			problemTypePurchaseInsufficient, "Insufficient wallet balance",
			"the wallet balance is not enough for this plan", nil)
	case errors.Is(err, billing.ErrInvalidIdempotency),
		errors.Is(err, billing.ErrInvalidProvider):
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypePurchaseInvalid,
			"Invalid subscription purchase", "provider or Idempotency-Key is invalid", nil)
	case errors.Is(err, billing.ErrSubscriptionPlanUnavailable),
		errors.Is(err, billing.ErrIdempotencyConflict):
		writeSubscriptionFieldsProblem(w, http.StatusConflict, problemTypePurchaseConflict,
			"Subscription purchase conflict",
			"the plan is not available or the Idempotency-Key belongs to another order", []string{"plan_id"})
	case errors.Is(err, billing.ErrSubscriptionPlanInvalid),
		errors.Is(err, billing.ErrInvalidCurrency),
		errors.Is(err, billing.ErrInvalidTopupAmount):
		writeSubscriptionFieldsProblem(w, http.StatusUnprocessableEntity, problemTypePurchaseInvalid,
			"Invalid subscription plan", "the plan price or currency is not supported", []string{"plan_id"})
	case errors.Is(err, billing.ErrTopupDisabled),
		errors.Is(err, billing.ErrProviderDisabled),
		errors.Is(err, billing.ErrPaymentConfig):
		w.Header().Set("Retry-After", "5")
		writeSubscriptionFieldsProblem(w, http.StatusServiceUnavailable, problemTypePurchaseUnavailable,
			"Payment provider is unavailable", "the selected payment provider is not available", []string{"provider"})
	case errors.Is(err, billing.ErrProviderUnavailable):
		writeSubscriptionFieldsProblem(w, http.StatusBadGateway, problemTypePurchaseUnavailable,
			"Payment provider unavailable", "failed to initialize payment with the provider", nil)
	default:
		h.log.Error("create subscription order failed", "error", err)
		writeSubscriptionFieldsProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "", nil)
	}
}

// ---------------------------------------------------------------------------
// 用户端：手动续费（P2a）
// ---------------------------------------------------------------------------

// subscriptionRenewRequest 是 POST /api/subscriptions/{id}/renew 的请求体。
//
// 只有 provider：目标实例来自路径，金额/周期/额度由服务端按实例当前套餐行
// 读取（与购买同一原则，不信任请求体）。
type subscriptionRenewRequest struct {
	Provider *string `json:"provider"`
}

// handleRenewSubscription 创建手动续费订单（billing-08 §3.3 方案甲，P2a）。
//
// 状态口径：
//   - 仅本人实例；他人实例与不存在统一 404（不泄露 ID 存在性）；
//   - `cancelled` 是唯一不可续费的终态 → 409；`active`/`expired` 允许，
//     expired 在支付成功后恢复 active（由 billing 结算推进 end_at 并置状态）；
//   - Idempotency-Key 必带：服务端代生成会让重试变成新订单，防重语义失效。
//
// 响应与购买同构（订单 + 支付载荷）；0 元套餐在同一事务内即时生效，
// 响应无 client_secret/qr_code。
func (h *SubscriptionHandlers) handleRenewSubscription(w http.ResponseWriter, r *http.Request) {
	p, ok := adminPrincipal(w, r)
	if !ok {
		return
	}
	id, ok := parsePlanPathID(r)
	if !ok {
		// 非法 id 与不存在同口径：不给出「格式错」这一额外信号。
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypeRenewInvalid,
			"Invalid subscription renewal", "Idempotency-Key header is required", []string{"Idempotency-Key"})
		return
	}
	var req subscriptionRenewRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypeRenewInvalid,
			"Invalid subscription renewal", "request body must be a JSON object", nil)
		return
	}
	if req.Provider == nil || strings.TrimSpace(*req.Provider) == "" {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypeRenewInvalid,
			"Invalid subscription renewal", "provider is required", []string{"provider"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(*req.Provider))
	if provider != billing.ProviderStripe && provider != billing.ProviderAlipay &&
		provider != billing.ProviderEpay {
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypeRenewInvalid,
			"Invalid subscription renewal", "provider must be stripe, alipay or epay", []string{"provider"})
		return
	}

	// 归属校验先于状态判定：他人实例与不存在统一 404（§15.4）。
	sub, err := h.repo.GetSubscription(r.Context(), id)
	if err != nil {
		h.writeRepoError(w, "get subscription for renewal", err)
		return
	}
	if sub.UserID != p.UserID {
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
		return
	}
	if sub.Status != "active" && sub.Status != "expired" {
		writeRenewNotRenewableProblem(w)
		return
	}
	if h.purchase == nil {
		w.Header().Set("Retry-After", "5")
		writeSubscriptionFieldsProblem(w, http.StatusServiceUnavailable, problemTypeRenewUnavailable,
			"Subscription renewal is unavailable", "the payment service is not configured", nil)
		return
	}

	result, err := h.purchase.CreateSubscriptionRenewalOrder(r.Context(), p.UserID, id, provider, key)
	if err != nil {
		h.writeRenewalError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, createOrderResponse{
		Order:        topupOrderViewFrom(result.Order),
		ClientSecret: result.ClientSecret,
		QRCode:       result.QRCode,
		PayURL:       result.PayURL,
		FormFields:   result.FormFields,
	})
}

func writeRenewNotRenewableProblem(w http.ResponseWriter) {
	writeAdminProblem(w, http.StatusConflict, problemTypeSubscriptionNotRenewable,
		"subscription_not_renewable", "subscription is cancelled and cannot be renewed")
}

// writeRenewalError 把 billing 的续费错误映射为 RFC 7807。
//
// 口径与购买一致（任务书 P2a 冻结语义 5）：套餐停用/删除、幂等键复用、
// 实例不可续费 → 409；币种/金额/套餐字段非法 → 422；渠道未配置 → 503；
// 渠道出站失败 → 502。任何失败都不会留下 succeeded 订单，也不会扩展实例。
func (h *SubscriptionHandlers) writeRenewalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrRenewalSubscriptionNotFound):
		writeAdminProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found", "")
	case errors.Is(err, billing.ErrInvalidIdempotency),
		errors.Is(err, billing.ErrInvalidProvider):
		writeSubscriptionFieldsProblem(w, http.StatusBadRequest, problemTypeRenewInvalid,
			"Invalid subscription renewal", "provider or Idempotency-Key is invalid", nil)
	case errors.Is(err, billing.ErrSubscriptionNotRenewable),
		errors.Is(err, billing.ErrSubscriptionPlanUnavailable),
		errors.Is(err, billing.ErrIdempotencyConflict):
		writeSubscriptionFieldsProblem(w, http.StatusConflict, problemTypeRenewConflict,
			"Subscription renewal conflict",
			"the subscription cannot be renewed or the Idempotency-Key belongs to another order", nil)
	case errors.Is(err, billing.ErrSubscriptionPlanInvalid),
		errors.Is(err, billing.ErrInvalidCurrency),
		errors.Is(err, billing.ErrInvalidTopupAmount):
		writeSubscriptionFieldsProblem(w, http.StatusUnprocessableEntity, problemTypeRenewInvalid,
			"Invalid subscription plan", "the plan price or currency is not supported", nil)
	case errors.Is(err, billing.ErrTopupDisabled),
		errors.Is(err, billing.ErrProviderDisabled),
		errors.Is(err, billing.ErrPaymentConfig):
		w.Header().Set("Retry-After", "5")
		writeSubscriptionFieldsProblem(w, http.StatusServiceUnavailable, problemTypeRenewUnavailable,
			"Payment provider is unavailable", "the selected payment provider is not available", []string{"provider"})
	case errors.Is(err, billing.ErrProviderUnavailable):
		writeSubscriptionFieldsProblem(w, http.StatusBadGateway, problemTypeRenewUnavailable,
			"Payment provider unavailable", "failed to initialize payment with the provider", nil)
	default:
		h.log.Error("create subscription renewal order failed", "error", err)
		writeSubscriptionFieldsProblem(w, http.StatusInternalServerError, authProblemInternal,
			"Internal server error", "", nil)
	}
}

// ---------------------------------------------------------------------------
// 审计
// ---------------------------------------------------------------------------

// auditSubscriptionEvent 写订阅域动作审计（套餐管理 + 用户周期末取消同口径）。
// detail 只含 id/plan_key/受影响计数——绝不记录整行，避免把价格/额度配置
// 复制进审计表（D1b 目录审计同口径）。
func (h *SubscriptionHandlers) auditSubscriptionEvent(r *http.Request, p *auth.Principal, action string, detail map[string]any, status int) {
	if err := writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     action,
		Actor:      p.UserID,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: status,
		Detail:     adminMarshalRaw(detail),
	}); err != nil {
		h.log.Warn("subscription plan audit write failed", "action", action, "actor", p.UserID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// 创建幂等（Idempotency-Key）
// ---------------------------------------------------------------------------

// idempotentCreate 为 POST /api/subscription-plans 提供 24h 幂等重放：
// 同 key + 同 body 返回首次响应；同 key + 不同 body → 422。键里带操作者 ID，
// 避免跨管理员共享幂等空间。与定价创建端点同构。
func (h *SubscriptionHandlers) idempotentCreate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.PrincipalFrom(r.Context())
		if !ok || p == nil || p.UserID == 0 {
			writeAdminProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized", "")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			next(w, r)
			return
		}
		if len(key) > adminIdempotencyKeyHdrMax {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"Idempotency-Key is too long")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
		if err != nil {
			writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"unable to read request body")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		fingerprint := tokenBodyFingerprint(body)

		ctx := r.Context()
		cacheKey := fmt.Sprintf("newtg:idem:subscription_plan:create:%d:%s", p.UserID, key)
		if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
			replayAdminIdempotent(w, cached, fingerprint)
			return
		}
		lockKey := cacheKey + ":lock"
		acquired, lockErr := h.rdb.SetNX(ctx, lockKey, "1", adminIdempotencyLease).Result()
		if lockErr == nil && !acquired {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
						"request cancelled before the idempotent result was ready")
					return
				case <-time.After(25 * time.Millisecond):
				}
				if cached, ok := h.loadIdempotent(ctx, cacheKey); ok {
					replayAdminIdempotent(w, cached, fingerprint)
					return
				}
			}
			writeAdminProblem(w, http.StatusConflict, authProblemConflict, "Conflict",
				"a request with the same Idempotency-Key is still in progress; retry shortly")
			return
		}

		buffer := newAdminBufferedResponse()
		next(buffer, r)
		if buffer.status >= 200 && buffer.status < 300 {
			h.saveIdempotent(ctx, cacheKey, adminCachedHTTPResponse{
				Status: buffer.status, ContentType: buffer.header.Get("Content-Type"),
				Body: buffer.body.String(), Fingerprint: fingerprint,
			})
		}
		if lockErr == nil {
			_ = h.rdb.Del(ctx, lockKey).Err()
		}
		buffer.copyTo(w)
	}
}

func (h *SubscriptionHandlers) loadIdempotent(ctx context.Context, key string) (adminCachedHTTPResponse, bool) {
	raw, err := h.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return adminCachedHTTPResponse{}, false
	}
	var cached adminCachedHTTPResponse
	if err := json.Unmarshal(raw, &cached); err != nil {
		return adminCachedHTTPResponse{}, false
	}
	return cached, true
}

func (h *SubscriptionHandlers) saveIdempotent(ctx context.Context, key string, cached adminCachedHTTPResponse) {
	raw, err := json.Marshal(cached)
	if err != nil {
		return
	}
	// 幂等缓存是尽力而为：Redis 故障时请求已经成功，不能因此回滚业务。
	_ = h.rdb.Set(ctx, key, raw, adminIdempotencyTTL).Err()
}
