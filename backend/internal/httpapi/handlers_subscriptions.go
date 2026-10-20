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
//	GET    /api/billing-preference                    用户：扣费偏好
//	PUT    /api/billing-preference                    用户：扣费偏好（四值归一）
//	POST   /api/subscription/purchase                 用户：购买占位（501，零写入）
//
// 设计约束：
//   - 管理端点全部要求 settings:manage（§15.1）；写动作登记字段白名单，未知字段
//     由 RBAC 字段层直接 403（绝不静默丢弃）。
//   - 用户侧一律以 Principal.UserID 为唯一数据范围：接口不接收 user_id，也不存在
//     可枚举他人实例的路径（§15.4）。管理侧单用户列表以 users:read 做功能级门。
//   - 购买端点只返回固定的 501：S3（支付/OAuth 收银台）尚未实现，此处若悄悄
//     创建订单会让前端误以为已下单；零写入保证重放/并发都不会留下副作用。
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
	problemTypePurchaseDisable = "https://newtg.dev/problems/subscription_purchase_disabled"
)

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
}

// SubscriptionHandlers 持有订阅端点依赖，可并发使用。
type SubscriptionHandlers struct {
	repo  store.SubscriptionRepo
	db    *sql.DB
	rdb   *redis.Client
	authz *rbac.AuthzMiddleware
	audit *audit.Logger
	log   *slog.Logger
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
		audit: cfg.Audit, log: logger,
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
		"plan_id": nil,
	}})
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

// parsePlanPathID 解析路径 id。非法 id 在契约里等同于不存在（404），
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
	h.auditPlan(r, p, audit.ActionSubscriptionPlanCreate, map[string]any{
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
	h.auditPlan(r, p, audit.ActionSubscriptionPlanUpdate, map[string]any{
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
	h.auditPlan(r, p, audit.ActionSubscriptionPlanDelete, map[string]any{"id": id}, http.StatusNoContent)
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
	h.auditPlan(r, p, action, map[string]any{
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
	h.auditPlan(r, p, audit.ActionSubscriptionPlanResetQuota, map[string]any{
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
// 用户端：购买占位
// ---------------------------------------------------------------------------

// handlePurchase 是 S3 支付接缝实现前的固定占位。
//
// 【为什么返回 501 而不是 400/503】契约要求前端在购买页明确呈现「暂不可用」，
// 501 是「服务端未实现该功能」的语义；400 会被误读为参数错误，503 会被重试
// 逻辑当作临时故障。响应 type/title 固定为 subscription_purchase_disabled，
// 前端按 type 显示文案。
//
// 【零写入】不读写订单、订阅、幂等记录或 Redis：任何写入都会让 S3 上线前的
// 重放/并发测试留下脏订单。Idempotency-Key 仍按契约要求存在性校验，缺失即 400。
func (h *SubscriptionHandlers) handlePurchase(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminPrincipal(w, r); !ok {
		return
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) == "" {
		writeAdminProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"Idempotency-Key header is required")
		return
	}
	writeSubscriptionFieldsProblem(w, http.StatusNotImplemented, problemTypePurchaseDisable,
		"subscription_purchase_disabled",
		"subscription purchase is not available yet", nil)
}

// ---------------------------------------------------------------------------
// 审计
// ---------------------------------------------------------------------------

// auditPlan 写套餐管理动作审计。detail 只含 id/plan_key/受影响计数——绝不
// 记录整行，避免把价格/额度配置复制进审计表（D1b 目录审计同口径）。
func (h *SubscriptionHandlers) auditPlan(r *http.Request, p *auth.Principal, action string, detail map[string]any, status int) {
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
