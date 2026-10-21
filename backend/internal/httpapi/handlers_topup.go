// Package httpapi 的在线充值端点（M5 Task 3 / modules-04 §4.A/B/C/E，
// F-04-01/03/04/05/06/10/11/13；裁决 P-9/P-10/P-12/P-13/P-15）。
//
// 端点面（P-9 冻结）：
//
//	GET  /api/topup/config           已登录用户：档位/上下限/汇率/可用渠道
//	POST /api/orders                 已登录用户：Idempotency-Key 幂等下单
//	GET  /api/orders                 本人订单（管理员 orders:read 可看全部）
//	GET  /api/orders/{id}            订单详情；他人订单 404（管理员 orders:read 例外）
//	POST /api/orders/{id}/cancel     本人/orders:manage：pending → cancelled
//	POST /api/orders/{id}/verify     本人/orders:manage：主动核实渠道并复用幂等入账
//	POST /api/orders/{id}/complete   管理员 orders:manage：人工完成（须渠道已收款）
//	POST /api/orders/{id}/refund     管理员 settings:manage：订阅订单退款（S3）
//	POST /api/webhooks/stripe        公网：Stripe-Signature 验签后入账
//	POST /api/webhooks/alipay        公网：RSA2 验签后入账
//
// 安全口径：
//   - webhook 是未认证端点，依赖验签 + **每个 provider 独立**的 60 次/分钟
//     固定窗口限流（F-08-16 / G-EXEC-003 · R-07-2）；Redis 不可用时 fail
//     closed（503），绝不因为限流器故障而无限放量。
//   - 金额、积分换算、幂等与入账原子性全在 billing.TopupService；本层只做
//     身份提取、参数形状校验与错误映射。
//   - 响应永不包含密钥、签名头或渠道原始错误；`client_secret` 只在首次创建
//     的响应出现（它等同支付凭据，不落库、不回放）。

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// webhook 限流阈值（F-08-16 唯一阈值来源：支付 webhook 60 次/分钟/端点）。
const (
	webhookRateLimit    = 60
	webhookRateWindow   = time.Minute
	webhookRateStripe   = "newtg:webhook:rate:stripe:"
	webhookRateAlipay   = "newtg:webhook:rate:alipay:"
	webhookRateEpay     = "newtg:webhook:rate:epay:"
	webhookMaxBodyBytes = 1 << 20

	// 订单分页（P-9）：默认 20，上限 100；p 上限与其它管理列表一致。
	orderPageSizeDefault = 20
	orderPageSizeMax     = 100
	orderPageMax         = 1_000_000
	orderSearchMaxRunes  = 128

	// 主动核实限流：每订单每分钟 5 次（F-04-12 冻结）。键含订单号，
	// 用户无法通过切换订单号放大对同一订单的渠道调用。
	orderVerifyRateLimit  = 5
	orderVerifyRateWindow = time.Minute
	orderVerifyRatePrefix = "newtg:order:verify:"
)

// problem type URI（沿用 https://newtg.dev/problems/<slug> 约定）。
const (
	problemTypeInvalidTopup        = "https://newtg.dev/problems/invalid_topup"
	problemTypeTopupUnavailable    = "https://newtg.dev/problems/topup_unavailable"
	problemTypeOrderConflict       = "https://newtg.dev/problems/order_state_conflict"
	problemTypeProviderUnavailable = "https://newtg.dev/problems/provider_unavailable"
	problemTypeProviderActionUnsup = "https://newtg.dev/problems/provider_action_unsupported"
	problemTypeOrderNotPaid        = "https://newtg.dev/problems/order_not_paid"
	// S3 退款：P-13 的「已开始/已消耗」与渠道退款失败各自独立 type，
	// 前端/运营可机读区分「不能退」与「暂时没退成功」。
	problemTypeSubscriptionInUse = "https://newtg.dev/problems/subscription_in_use"
	problemTypeRefundFailed      = "https://newtg.dev/problems/refund_provider_failed"
)

// TopupHandlerConfig 是充值端点的装配输入。
type TopupHandlerConfig struct {
	DB *gorm.DB
	// Service 与 DB 二选一：不提供 Service 时用 DB + Config 构造默认服务
	// （WalletFunding 入账）。单测可注入指向 httptest 桩的 Service。
	Service *billing.TopupService
	Redis   *redis.Client
	Config  ConfigReader
	Sealer  configcenter.SecretSealer
	Logger  *slog.Logger
	// Authz 用于解析管理员权限（orders:read/manage）。生产必填：
	// 会话 Principal 的 Permissions 为 nil，而 nil 在 RBAC 语义里是
	// DB NULL=FULL；不解析会让受限管理员在订单端点被误判为全权。
	// nil 仅用于已显式注入权威权限集合的测试。
	Authz *rbac.AuthzMiddleware
	// Audit 记录资金动作（S3 退款）。生产必填；nil 时退款仍成功但无审计，
	// 仅用于不触退款的单测装配。
	Audit *audit.Logger
	// Now 是限流窗口时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// StripeBaseURL / AlipayGatewayURL 是渠道地址覆盖（测试）。
	StripeBaseURL    string
	AlipayGatewayURL string
	// Webhook 是充值入账成功后的 payment_success 事件生产者（B1b）。
	// 仅在使用 DB 构造默认服务时生效；显式注入 Service 的装配由调用方
	// 自行把生产者接进该 Service。
	Webhook billing.PaymentSuccessWebhook
}

// TopupHandlers 持有充值端点的依赖，可并发使用。
type TopupHandlers struct {
	svc   *billing.TopupService
	rdb   *redis.Client
	log   *slog.Logger
	now   func() time.Time
	authz *rbac.AuthzMiddleware
	audit *audit.Logger
}

// NewTopupHandlers 构造充值端点。Redis 必填：webhook 限流是公网资金接口
// 的防刷闸门，缺 Redis 时不能静默不限流（P-12 fail closed）。
func NewTopupHandlers(cfg TopupHandlerConfig) (*TopupHandlers, error) {
	if cfg.Redis == nil {
		return nil, errors.New("httpapi: TopupHandlerConfig.Redis is required (webhook rate limit)")
	}
	svc := cfg.Service
	if svc == nil {
		if cfg.DB == nil {
			return nil, errors.New("httpapi: TopupHandlerConfig.Service or DB is required")
		}
		created, err := billing.NewTopupService(billing.TopupServiceConfig{
			DB:               cfg.DB,
			Config:           cfg.Config,
			Sealer:           cfg.Sealer,
			Logger:           cfg.Logger,
			Now:              cfg.Now,
			StripeBaseURL:    cfg.StripeBaseURL,
			AlipayGatewayURL: cfg.AlipayGatewayURL,
			Webhook:          cfg.Webhook,
		})
		if err != nil {
			return nil, err
		}
		svc = created
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &TopupHandlers{svc: svc, rdb: cfg.Redis, log: log, now: now, authz: cfg.Authz, audit: cfg.Audit}, nil
}

// Service 暴露底层支付服务，供订阅购买（S3）复用同一渠道实现。
// 返回 nil 表示 handler 未装配（构造失败时不会走到这里）。
func (h *TopupHandlers) Service() *billing.TopupService {
	if h == nil {
		return nil
	}
	return h.svc
}

// RegisterTopupRoutes 注册已登录用户端点。wrap 语义同其它 Register*Routes。
func (h *TopupHandlers) RegisterTopupRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mount := func(pattern string, handler http.Handler) {
		if wrap != nil {
			handler = wrap(handler)
		}
		mux.Handle(pattern, handler)
	}
	mount("GET /api/topup/config", http.HandlerFunc(h.handleConfig))
	mount("POST /api/orders", http.HandlerFunc(h.handleCreateOrder))
	mount("GET /api/orders", http.HandlerFunc(h.handleListOrders))
	mount("GET /api/orders/{id}", http.HandlerFunc(h.handleGetOrder))
	mount("POST /api/orders/{id}/cancel", http.HandlerFunc(h.handleCancelOrder))
	mount("POST /api/orders/{id}/verify", http.HandlerFunc(h.handleVerifyOrder))
	mount("POST /api/orders/{id}/complete", http.HandlerFunc(h.handleCompleteOrder))
	mount("POST /api/orders/{id}/refund", http.HandlerFunc(h.handleRefundOrder))
}

// RegisterWebhookRoutes 把两个公网回调挂到根 mux。
//
// 使用 HandlePublic 精确模式绕过 SessionAuth/CSRF/Origin：渠道服务器没有
// 会话，也不发送同源头；端点安全完全由验签 + 限流承担。
func (h *TopupHandlers) RegisterWebhookRoutes(rt *Router) {
	rt.HandlePublic("POST /api/webhooks/stripe", http.HandlerFunc(h.handleStripeWebhook))
	rt.HandlePublic("POST /api/webhooks/alipay", http.HandlerFunc(h.handleAlipayWebhook))
	// epay 的通知方法由渠道配置决定，GET（query）与 POST（表单）都很常见；
	// 两种方法走同一验签与入账路径。
	rt.HandlePublic("GET /api/webhooks/epay", http.HandlerFunc(h.handleEpayWebhook))
	rt.HandlePublic("POST /api/webhooks/epay", http.HandlerFunc(h.handleEpayWebhook))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type topupProviderView struct {
	PublishableKey string `json:"publishable_key,omitempty"`
	// MinTopup 是该渠道自己的最低充值（分）。有效下限 = max(全局, 渠道)
	// 由前端计算；服务端下单时用同一 providerReady 结果复查（F-04-01）。
	MinTopup int64 `json:"min_topup"`
}

type topupConfigResponse struct {
	AmountOptions    []int64                      `json:"amount_options"`
	MinTopup         int64                        `json:"min_topup"`
	MaxTopup         int64                        `json:"max_topup"`
	QuotaPerUnit     int64                        `json:"quota_per_unit"`
	CreditsRatio     float64                      `json:"credits_ratio"`
	EnabledProviders []string                     `json:"enabled_providers"`
	Providers        map[string]topupProviderView `json:"providers"`
}

type topupOrderView struct {
	ID              int64      `json:"id"`
	Provider        string     `json:"provider"`
	Amount          int64      `json:"amount"`
	Currency        string     `json:"currency"`
	Status          string     `json:"status"`
	CreditedCredits int64      `json:"credited_credits"`
	FailureCode     *string    `json:"failure_code,omitempty"`
	PaidAt          *time.Time `json:"paid_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type createOrderRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Provider string `json:"provider"`
}

type createOrderResponse struct {
	Order        topupOrderView    `json:"order"`
	ClientSecret string            `json:"client_secret,omitempty"`
	QRCode       string            `json:"qr_code,omitempty"`
	PayURL       string            `json:"pay_url,omitempty"`
	FormFields   map[string]string `json:"form_fields,omitempty"`
}

// orderView 是订单列表/详情的统一投影（F-04-08/F-04-09）。
//
// raw_callback 只在 includeRaw（管理员详情）时填充；用户视图连字段名都
// 不出现（omitempty + json.RawMessage），避免越权证据进入用户响应。
// expires_at 是 created_at + 30min 的派生值（P-1，不落库）。
type orderView struct {
	ID              int64      `json:"id"`
	UserID          int64      `json:"user_id"`
	Provider        string     `json:"provider"`
	Amount          int64      `json:"amount"`
	Currency        string     `json:"currency"`
	Status          string     `json:"status"`
	CreditedCredits int64      `json:"credited_credits"`
	FailureCode     *string    `json:"failure_code"`
	PaidAt          *time.Time `json:"paid_at"`
	ManualCompleted bool       `json:"manual_completed"`
	ManualReason    *string    `json:"manual_reason"`
	// S3 退款事实（只在退款相关状态有值；管理员和用户都可读）。
	RefundedAmount    int64           `json:"refunded_amount"`
	RefundedAt        *time.Time      `json:"refunded_at"`
	RefundReason      *string         `json:"refund_reason"`
	RefundRequestedAt *time.Time      `json:"refund_requested_at"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	ExpiresAt         time.Time       `json:"expires_at"`
	RawCallback       json.RawMessage `json:"raw_callback,omitempty"`
}

func orderViewFrom(order *store.Order, includeRaw bool) orderView {
	view := orderView{
		ID:                order.ID,
		UserID:            order.UserID,
		Provider:          order.Provider,
		Amount:            order.Amount,
		Currency:          order.Currency,
		Status:            order.Status,
		CreditedCredits:   order.CreditedCredits,
		FailureCode:       order.FailureCode,
		PaidAt:            order.PaidAt,
		ManualCompleted:   order.ManualCompleted,
		ManualReason:      order.ManualReason,
		RefundedAmount:    order.RefundedAmount,
		RefundedAt:        order.RefundedAt,
		RefundReason:      order.RefundReason,
		RefundRequestedAt: order.RefundRequestedAt,
		CreatedAt:         order.CreatedAt,
		UpdatedAt:         order.UpdatedAt,
		ExpiresAt:         billing.OrderExpiresAt(order.CreatedAt),
	}
	if includeRaw && len(order.RawCallback) > 0 {
		view.RawCallback = json.RawMessage(order.RawCallback)
	}
	return view
}

type orderListView struct {
	Items    []orderView `json:"items"`
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
}

type orderVerifyResponse struct {
	Paid  bool      `json:"paid"`
	Order orderView `json:"order"`
}

type completeOrderRequest struct {
	ManualReason string `json:"manual_reason"`
}

// refundOrderRequest 是管理员退款的请求体（S3）。
type refundOrderRequest struct {
	Reason string `json:"reason"`
}

// ---------------------------------------------------------------------------
// GET /api/topup/config
// ---------------------------------------------------------------------------

func (h *TopupHandlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	opts, err := h.svc.Options(r.Context(), userID)
	if err != nil {
		// 配置错误属运营面问题：500 + 日志，绝不展示一份无法下单的档位。
		h.log.Error("topup config unavailable", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Top-up configuration unavailable")
		return
	}
	resp := topupConfigResponse{
		AmountOptions:    opts.AmountOptions,
		MinTopup:         opts.MinTopup,
		MaxTopup:         opts.MaxTopup,
		QuotaPerUnit:     opts.QuotaPerUnit,
		CreditsRatio:     opts.CreditsRatio,
		EnabledProviders: opts.EnabledProviders,
		Providers:        make(map[string]topupProviderView, len(opts.EnabledProviders)),
	}
	for _, provider := range opts.EnabledProviders {
		view := topupProviderView{MinTopup: opts.ProviderMinTopups[provider]}
		// P-17：publishable_key 是公开值（前端 Stripe Elements 需要），
		// 只随可用渠道下发；secret/webhook secret 永不进入任何响应。
		if provider == billing.ProviderStripe {
			view.PublishableKey = opts.StripePublishableKey
		}
		resp.Providers[provider] = view
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// POST /api/orders
// ---------------------------------------------------------------------------

func (h *TopupHandlers) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	// Idempotency-Key 缺失直接 400：服务端不能替客户端生成 key，否则
	// 重试会产生新订单（F-04-10 的整个防重语义就失效了）。
	key := r.Header.Get("Idempotency-Key")
	var req createOrderRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request body")
		return
	}
	result, err := h.svc.CreateTopupOrder(r.Context(), userID, req.Amount, req.Currency, req.Provider, key)
	if err != nil {
		h.writeOrderError(w, err)
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

func topupOrderViewFrom(order *store.Order) topupOrderView {
	return topupOrderView{
		ID:              order.ID,
		Provider:        order.Provider,
		Amount:          order.Amount,
		Currency:        order.Currency,
		Status:          order.Status,
		CreditedCredits: order.CreditedCredits,
		FailureCode:     order.FailureCode,
		PaidAt:          order.PaidAt,
		CreatedAt:       order.CreatedAt,
	}
}

// ---------------------------------------------------------------------------
// 订单中心与人工旁路（M5 Task 4 / P-9/P-18/P-19）
// ---------------------------------------------------------------------------

// orderPrincipal 提取身份并解析权威权限。
//
// 为什么不能直接用会话 Principal：SessionAuth 只填 UserID/Role，
// Permissions=nil 在 RBAC 里等价于 DB NULL=FULL。若不解析，一个没有
// orders:read 的受限管理员会被这里当成全权管理员；解析失败则 fail closed。
func (h *TopupHandlers) orderPrincipal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return nil, false
	}
	if h.authz != nil {
		resolved, err := h.authz.ResolvePermissions(r.Context(), *p)
		if err != nil {
			h.log.Error("order permission resolve failed", "user_id", p.UserID, "error", err)
			writeProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden")
			return nil, false
		}
		p = &resolved
	}
	return p, true
}

// parseOrderID 把 {id} 映射为主键；不可解析一律 404（不区分「格式错」
// 与「不存在」，避免为 ID 枚举提供额外信号）。
func (h *TopupHandlers) parseOrderID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return 0, false
	}
	return id, true
}

// loadOrder 读取订单；不存在时写 404。
func (h *TopupHandlers) loadOrder(w http.ResponseWriter, r *http.Request) (*store.Order, bool) {
	id, ok := h.parseOrderID(w, r)
	if !ok {
		return nil, false
	}
	order, err := h.svc.GetOrder(r.Context(), id)
	if errors.Is(err, billing.ErrOrderNotFound) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return nil, false
	}
	if err != nil {
		h.log.Error("get order failed", "order_id", id, "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return nil, false
	}
	return order, true
}

// orderVisible 实现 P-18 的数据级范围：本人永远可见；管理员只有持有
// orders:read 才获得全局范围。不可见一律 404（不确认资源存在）。
func orderVisible(p *auth.Principal, order *store.Order) bool {
	return order.UserID == p.UserID || rbac.HasPermission(*p, rbac.PermOrdersRead)
}

// writeOrderProblem 是订单端点共用的状态码映射。
func writeOrderProblem(w http.ResponseWriter, status int, problemType, title, detail string) {
	writeProblemDetail(w, status, problemType, title, detail)
}

func (h *TopupHandlers) handleListOrders(w http.ResponseWriter, r *http.Request) {
	p, ok := h.orderPrincipal(w, r)
	if !ok {
		return
	}
	page, pageSize := 1, orderPageSizeDefault
	if raw := strings.TrimSpace(r.URL.Query().Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > orderPageMax {
			writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("p must be between 1 and %d", orderPageMax))
			return
		}
		page = value
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > orderPageSizeMax {
			writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				fmt.Sprintf("page_size must be between 1 and %d", orderPageSizeMax))
			return
		}
		pageSize = value
	}

	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !billing.OrderStatus(status).Valid() {
		writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"status must be one of pending, succeeded, failed, expired, cancelled, refund_required, refunded, refund_failed")
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(search)) > orderSearchMaxRunes {
		writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			fmt.Sprintf("q must be at most %d characters", orderSearchMaxRunes))
		return
	}

	// 范围：无 orders:read 的用户只能看自己；显式指定他人 user_id 直接
	// 403（静默忽略会让调用方误以为筛选生效）。
	userID := p.UserID
	if rbac.HasPermission(*p, rbac.PermOrdersRead) {
		userID = 0
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("user_id")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"user_id must be a positive integer")
			return
		}
		if !rbac.HasPermission(*p, rbac.PermOrdersRead) && value != p.UserID {
			// 控制台裁决（M5 Task 4 review round 1）：列表的 user_id 是筛选
			// 参数而不是资源寻址，403 不会泄漏任何订单是否存在；此处刻意
			// 不用 404（404 语义只用于 /api/orders/{id} 形态）。
			writeProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden")
			return
		}
		userID = value
	}

	orders, total, err := h.svc.ListOrders(r.Context(), billing.OrderListFilter{
		UserID:   userID,
		Status:   status,
		Query:    search,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		h.log.Error("list orders failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	items := make([]orderView, 0, len(orders))
	for i := range orders {
		// 列表永不返回 raw_callback（即使管理员）：详情页才需要原始回调。
		items = append(items, orderViewFrom(&orders[i], false))
	}
	writeJSON(w, http.StatusOK, orderListView{
		Items: items, Total: total, Page: page, PageSize: pageSize,
	})
}

func (h *TopupHandlers) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := h.orderPrincipal(w, r)
	if !ok {
		return
	}
	order, ok := h.loadOrder(w, r)
	if !ok {
		return
	}
	if !orderVisible(p, order) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	// raw_callback 仅管理员可见（F-04-09；P-9）。
	writeJSON(w, http.StatusOK, orderViewFrom(order, p.Role == rbac.RoleAdmin))
}

func (h *TopupHandlers) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := h.orderPrincipal(w, r)
	if !ok {
		return
	}
	order, ok := h.loadOrder(w, r)
	if !ok {
		return
	}
	if order.UserID != p.UserID && !rbac.HasPermission(*p, rbac.PermOrdersManage) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if err := h.svc.CancelOrder(r.Context(), order); err != nil {
		if errors.Is(err, billing.ErrOrderStateConflict) ||
			errors.Is(err, billing.ErrInvalidOrderTransition) {
			writeOrderProblem(w, http.StatusConflict, problemTypeOrderConflict,
				"Order state conflict", "Only pending orders can be cancelled.")
			return
		}
		h.log.Error("cancel order failed", "order_id", order.ID, "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		return
	}
	writeJSON(w, http.StatusOK, orderViewFrom(order, p.Role == rbac.RoleAdmin))
}

func (h *TopupHandlers) handleVerifyOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := h.orderPrincipal(w, r)
	if !ok {
		return
	}
	order, ok := h.loadOrder(w, r)
	if !ok {
		return
	}
	// 主动核实会触发入账（状态变更），因此数据级范围比只读端点更严：
	// 本人或 orders:manage。只有 orders:read 的管理员不能替他人核实。
	if order.UserID != p.UserID && !rbac.HasPermission(*p, rbac.PermOrdersManage) {
		writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		return
	}
	if !h.allowOrderVerify(w, r, order.ID) {
		return
	}
	result, err := h.svc.VerifyOrder(r.Context(), order)
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrOrderStateConflict),
			errors.Is(err, billing.ErrInvalidOrderTransition):
			writeOrderProblem(w, http.StatusConflict, problemTypeOrderConflict,
				"Order state conflict", "Only pending orders can be verified.")
		case errors.Is(err, billing.ErrProviderActionUnsupported):
			// epay 等无主动查询能力的渠道：请求本身合法，但该渠道没有这个
			// 动作。用 409 + 固定 problem type 与「渠道故障 502」区分开，
			// 避免用户把「不支持核实」当成可重试的瞬时错误。
			writeOrderProblem(w, http.StatusConflict, problemTypeProviderActionUnsup,
				"Action not supported", "This payment provider does not support order verification.")
		case errors.Is(err, billing.ErrProviderUnavailable),
			errors.Is(err, billing.ErrProviderDisabled),
			errors.Is(err, billing.ErrPaymentConfig):
			writeOrderProblem(w, http.StatusBadGateway, problemTypeProviderUnavailable,
				"Payment provider unavailable", "Failed to query the payment provider.")
		case errors.Is(err, billing.ErrAmountMismatch),
			errors.Is(err, billing.ErrCurrencyMismatch):
			writeOrderProblem(w, http.StatusConflict, problemTypeOrderConflict,
				"Order state conflict", "The provider amount does not match the order.")
		default:
			h.log.Error("verify order failed", "order_id", order.ID, "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, orderVerifyResponse{
		Paid:  result.Paid,
		Order: orderViewFrom(result.Order, p.Role == rbac.RoleAdmin),
	})
}

// allowOrderVerify 是「每订单 5 次/分钟」固定窗口限流。
//
// Redis 故障 fail closed（503）：主动核实会打渠道 API，限流器不可用时
// 放量等于把渠道凭据变成可无限调用的探针。
func (h *TopupHandlers) allowOrderVerify(w http.ResponseWriter, r *http.Request, orderID int64) bool {
	// 键不含时间桶而是「首次请求时设置 60s TTL」：固定窗口在分钟边界
	// 会允许瞬时 10 次，滚动窗口更贴近「每分钟 5 次」的冻结语义。
	key := fmt.Sprintf("%s%d", orderVerifyRatePrefix, orderID)
	count, err := h.rdb.Incr(r.Context(), key).Result()
	if err != nil {
		h.log.Error("order verify rate limiter unavailable", "order_id", orderID, "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Service temporarily unavailable")
		return false
	}
	if count == 1 {
		h.rdb.Expire(r.Context(), key, orderVerifyRateWindow)
	}
	if count > orderVerifyRateLimit {
		w.Header().Set("Retry-After", strconv.Itoa(int(orderVerifyRateWindow/time.Second)))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return false
	}
	return true
}

func (h *TopupHandlers) handleCompleteOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := h.orderPrincipal(w, r)
	if !ok {
		return
	}
	// 功能级：人工完成只属于 orders:manage。非管理员一律 403（不是 404：
	// 该动作本身不可见，隐藏订单存在性没有意义）。
	if !rbac.HasPermission(*p, rbac.PermOrdersManage) {
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden")
		return
	}
	order, ok := h.loadOrder(w, r)
	if !ok {
		return
	}
	var req completeOrderRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object with manual_reason")
		return
	}
	if err := h.svc.CompleteOrder(r.Context(), order, p.UserID, req.ManualReason); err != nil {
		switch {
		case errors.Is(err, billing.ErrInvalidManualReason):
			writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"manual_reason must be 10-200 characters")
		case errors.Is(err, billing.ErrProviderActionUnsupported):
			// 人工完成的前提是渠道能确认收款；epay 没有查询接口，管理员
			// 不能通过该端点完成订单（迟到支付需走退款/人工对账流程）。
			// 400 + 固定文案，且不写任何状态。
			writeOrderProblem(w, http.StatusBadRequest, problemTypeProviderActionUnsup,
				"Action not supported", "This payment provider does not support order confirmation.")
		case errors.Is(err, billing.ErrOrderNotPaid),
			errors.Is(err, billing.ErrProviderUnavailable),
			errors.Is(err, billing.ErrProviderDisabled),
			errors.Is(err, billing.ErrPaymentConfig),
			errors.Is(err, billing.ErrAmountMismatch),
			errors.Is(err, billing.ErrCurrencyMismatch):
			// P-19：渠道未确认收款/查询失败 → 400，绝不入账。
			writeOrderProblem(w, http.StatusBadRequest, problemTypeOrderNotPaid,
				"Payment not confirmed", "The provider has not confirmed payment for this order.")
		case errors.Is(err, billing.ErrOrderStateConflict),
			errors.Is(err, billing.ErrInvalidOrderTransition):
			writeOrderProblem(w, http.StatusConflict, problemTypeOrderConflict,
				"Order state conflict", "Only pending orders can be completed.")
		default:
			h.log.Error("complete order failed", "order_id", order.ID, "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		}
		return
	}
	writeJSON(w, http.StatusOK, orderViewFrom(order, true))
}

// handleRefundOrder 实现管理员退款（S3；brief 冻结为 settings:manage）。
//
// 退款是资金动作，权限比订单查看更严：orders:read 不够，必须有
// settings:manage（与套餐管理同级的运营权限）。订单状态机、P-13 资格、
// 渠道调用、幂等与事件全部在 billing.RefundSubscription；本层只做
// 身份/权限/请求形态与错误映射。
func (h *TopupHandlers) handleRefundOrder(w http.ResponseWriter, r *http.Request) {
	p, ok := h.orderPrincipal(w, r)
	if !ok {
		return
	}
	if !rbac.HasPermission(*p, rbac.PermSettingsManage) {
		writeProblem(w, http.StatusForbidden, authProblemForbidden, "Forbidden")
		return
	}
	order, ok := h.loadOrder(w, r)
	if !ok {
		return
	}
	var req refundOrderRequest
	if err := readAuthJSON(r, &req); err != nil {
		writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
			"request body must be a JSON object with reason")
		return
	}
	result, err := h.svc.RefundSubscription(r.Context(), billing.RefundInput{
		OrderID:     order.ID,
		Reason:      req.Reason,
		ActorUserID: p.UserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, billing.ErrInvalidRefundReason):
			writeOrderProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid request",
				"reason must be 1-200 characters")
		case errors.Is(err, billing.ErrSubscriptionInUse):
			// P-13：已开始或已消耗的订阅不能全额退款。
			writeOrderProblem(w, http.StatusUnprocessableEntity, problemTypeSubscriptionInUse,
				"Subscription in use", "Only subscriptions that have not started and have not been used can be refunded.")
		case errors.Is(err, billing.ErrProviderActionUnsupported):
			// 易支付等渠道没有退款 API：请求合法但渠道无此能力，409 而不是
			// 502（重试不会改变结果）。
			writeOrderProblem(w, http.StatusConflict, problemTypeProviderActionUnsup,
				"Action not supported", "This payment provider does not support refunds.")
		case errors.Is(err, billing.ErrRefundWalletLimit):
			// M-6：回补会把钱包推过 2^53−1 上界——业务结果而非程序故障，
			// 用 409 让运营改成人工处理。
			writeOrderProblem(w, http.StatusConflict, problemTypeRefundFailed,
				"Wallet limit exceeded", "The refund would exceed the wallet balance limit; handle it manually.")
		case errors.Is(err, billing.ErrRefundProviderFailed):
			// R-32：订单已置 refund_failed，可用同一端点重试。
			writeOrderProblem(w, http.StatusBadGateway, problemTypeRefundFailed,
				"Refund failed at provider", "The provider refund failed; the order is marked refund_failed and can be retried.")
		case errors.Is(err, billing.ErrOrderStateConflict),
			errors.Is(err, billing.ErrInvalidOrderTransition),
			errors.Is(err, billing.ErrRefundNotSupported):
			writeOrderProblem(w, http.StatusConflict, problemTypeOrderConflict,
				"Order state conflict", "This order cannot be refunded in its current state.")
		case errors.Is(err, billing.ErrOrderNotFound):
			writeProblem(w, http.StatusNotFound, problemTypeNotFoundSelf, "Not Found")
		default:
			h.log.Error("refund order failed", "order_id", order.ID, "error", err)
			writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
		}
		return
	}
	// R-37：资金动作必须留审计（日志库与业务库分离，detail 不受 R-34 的
	// 事件三键约束）。幂等重放也记录一次成功操作，便于追溯管理员的每次尝试。
	if err := writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action:     audit.ActionOrderRefund,
		Actor:      p.UserID,
		IP:         tokenRemoteIP(r),
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: http.StatusOK,
		Detail: adminMarshalRaw(map[string]any{
			"order_id": result.Order.ID,
			"amount":   result.Amount,
			"reason":   req.Reason,
			"actor":    p.UserID,
		}),
	}); err != nil {
		h.log.Warn("order refund audit write failed",
			"order_id", order.ID, "actor", p.UserID, "error", err)
	}
	// raw_callback 仅管理员可见（退款端点本身要求 settings:manage）。
	writeJSON(w, http.StatusOK, orderViewFrom(result.Order, true))
}

// writeOrderError 把计费错误映射为稳定的 HTTP 面。
func (h *TopupHandlers) writeOrderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrInvalidTopupAmount),
		errors.Is(err, billing.ErrInvalidProvider),
		errors.Is(err, billing.ErrInvalidCurrency),
		errors.Is(err, billing.ErrInvalidIdempotency):
		writeProblemDetail(w, http.StatusBadRequest, problemTypeInvalidTopup,
			"Invalid top-up request", "The amount, currency, provider or idempotency key is invalid.")
	case errors.Is(err, billing.ErrTopupDisabled):
		writeProblemDetail(w, http.StatusForbidden, problemTypeTopupUnavailable,
			"Online top-up is unavailable", "Online top-up is currently disabled.")
	case errors.Is(err, billing.ErrProviderDisabled),
		errors.Is(err, billing.ErrPaymentConfig):
		// P-13：渠道未启用或配置不完整 → 明确拒绝，不静默回退到其它渠道。
		writeProblemDetail(w, http.StatusForbidden, problemTypeTopupUnavailable,
			"Payment provider is unavailable", "The selected payment provider is not available.")
	case errors.Is(err, billing.ErrIdempotencyConflict):
		writeProblemDetail(w, http.StatusConflict, problemTypeOrderConflict,
			"Order state conflict", "This idempotency key belongs to another order.")
	case errors.Is(err, billing.ErrProviderUnavailable):
		writeProblemDetail(w, http.StatusBadGateway, problemTypeProviderUnavailable,
			"Payment provider unavailable", "Failed to initialize payment with the provider.")
	default:
		h.log.Error("create topup order failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
	}
}

// currentUser 提取已认证主体。路由已挂 SessionAuth，这里只是纵深防御：
// 没有 principal 时绝不按 userID=0 建单。
func (h *TopupHandlers) currentUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok || p == nil || p.UserID <= 0 {
		writeProblem(w, http.StatusUnauthorized, problemTypeUnauthorized, "Unauthorized")
		return 0, false
	}
	return p.UserID, true
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

// allowWebhook 执行「每 provider 独立桶」的 60 次/分钟固定窗口限流。
//
// 键只含端点前缀与窗口序号：限流面是端点而非调用方（渠道回调 IP 不固定，
// 按 IP 分桶会失去保护意义）。Redis 故障时 fail closed：资金入口不能因为
// 限流器不可用而放量。
func (h *TopupHandlers) allowWebhook(w http.ResponseWriter, r *http.Request, prefix string) bool {
	status, retryAfter := h.checkWebhookLimit(r, prefix)
	if status == 0 {
		return true
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	if status == http.StatusTooManyRequests {
		writeProblem(w, status, authProblemTooManyRequests, "Too many requests")
		return false
	}
	writeProblem(w, status, authProblemServiceUnavailable, "Service temporarily unavailable")
	return false
}

// checkWebhookLimit 是固定窗口限流的共享实现。返回 status=0 表示放行；
// 否则返回应回写的 HTTP 状态与 Retry-After 秒数。把它与响应形状分离，
// 是因为 epay 要求纯文本 ack，而 Stripe/支付宝走 RFC 7807。
//
// 键只含端点前缀与窗口序号：限流面是端点而非调用方（渠道回调 IP 不固定，
// 按 IP 分桶会失去保护意义）。Redis 故障时 fail closed：资金入口不能因为
// 限流器不可用而放量。
func (h *TopupHandlers) checkWebhookLimit(r *http.Request, prefix string) (int, int) {
	now := h.now()
	bucket := now.Unix() / int64(webhookRateWindow/time.Second)
	key := fmt.Sprintf("%s%d", prefix, bucket)

	count, err := h.rdb.Incr(r.Context(), key).Result()
	if err != nil {
		h.log.Error("webhook rate limiter unavailable", "prefix", prefix, "error", err)
		return http.StatusServiceUnavailable, 5
	}
	if count == 1 {
		h.rdb.Expire(r.Context(), key, webhookRateWindow+time.Second)
	}
	if count > webhookRateLimit {
		retry := time.Unix((bucket+1)*int64(webhookRateWindow/time.Second), 0).Sub(now)
		seconds := int(retry / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return http.StatusTooManyRequests, seconds
	}
	return 0, 0
}

func (h *TopupHandlers) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.allowWebhook(w, r, webhookRateStripe) {
		return
	}
	// 验签必须使用原始字节：先读 body，绝不先做 JSON 反序列化（SI-F-04）。
	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes+1))
	if err != nil || len(body) > webhookMaxBodyBytes {
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid webhook body")
		return
	}
	err = h.svc.HandleStripeWebhook(r.Context(), body, r.Header.Get("Stripe-Signature"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, billing.ErrWebhookSignature),
		errors.Is(err, billing.ErrWebhookPayload),
		errors.Is(err, billing.ErrPaymentConfig),
		errors.Is(err, billing.ErrProviderDisabled):
		// 验签/载荷失败：非 2xx 让 Stripe 记录投递失败，但绝不改订单。
		writeProblem(w, http.StatusBadRequest, authProblemBadRequest, "Invalid webhook")
	case errors.Is(err, billing.ErrOrderNotFound):
		// 订单不存在（含回调早于落库）：告警并 200，避免无限重投
		// （modules-04 F-04-04 步骤 2）。
		h.log.Warn("stripe webhook for unknown order", "error", err)
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, billing.ErrAmountMismatch), errors.Is(err, billing.ErrCurrencyMismatch):
		// 金额不一致已由服务层置 failed + 告警；重投不会改变结果。
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, billing.ErrOrderStateConflict):
		h.log.Warn("stripe webhook for terminal order", "error", err)
		w.WriteHeader(http.StatusOK)
	default:
		h.log.Error("stripe webhook processing failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, authProblemInternal, "Internal server error")
	}
}

func (h *TopupHandlers) handleAlipayWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.allowWebhook(w, r, webhookRateAlipay) {
		return
	}
	// 限制原始体大小；ParseForm 默认上限更大，未认证端点不能依赖它。
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBodyBytes)
	if err := r.ParseForm(); err != nil {
		h.writeAlipayAck(w, http.StatusBadRequest, false)
		return
	}
	err := h.svc.HandleAlipayNotify(r.Context(), r.PostForm)
	switch {
	case err == nil:
		h.writeAlipayAck(w, http.StatusOK, true)
	case errors.Is(err, billing.ErrWebhookSignature),
		errors.Is(err, billing.ErrWebhookPayload),
		errors.Is(err, billing.ErrPaymentConfig),
		errors.Is(err, billing.ErrProviderDisabled):
		// 验签失败：回纯文本 failure，条件与 Stripe 同（不改订单）。
		h.writeAlipayAck(w, http.StatusBadRequest, false)
	case errors.Is(err, billing.ErrOrderNotFound):
		h.log.Warn("alipay notify for unknown order", "error", err)
		h.writeAlipayAck(w, http.StatusOK, true)
	case errors.Is(err, billing.ErrAmountMismatch), errors.Is(err, billing.ErrCurrencyMismatch):
		// F-04-06：金额不符 → failed + 告警，返回 failure。
		h.writeAlipayAck(w, http.StatusBadRequest, false)
	case errors.Is(err, billing.ErrOrderStateConflict):
		h.log.Warn("alipay notify for terminal order", "error", err)
		h.writeAlipayAck(w, http.StatusOK, true)
	default:
		h.log.Error("alipay notify processing failed", "error", err)
		h.writeAlipayAck(w, http.StatusInternalServerError, false)
	}
}

// writeAlipayAck 输出支付宝要求的纯文本 ack（不是 JSON/problem）。
func (h *TopupHandlers) writeAlipayAck(w http.ResponseWriter, status int, success bool) {
	h.writePaymentAck(w, status, success, "success", "failure")
}

// writePaymentAck 是渠道纯文本 ack 的唯一写出点。
func (h *TopupHandlers) writePaymentAck(w http.ResponseWriter, status int, success bool,
	successText, failureText string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if success {
		_, _ = io.WriteString(w, successText)
		return
	}
	_, _ = io.WriteString(w, failureText)
}

// handleEpayWebhook 处理易支付通知（F-04-21）。
//
// 响应形状由渠道冻结为纯文本 `success`/`fail`（不是 JSON、不是 problem）：
// 渠道只根据文本决定是否重投，JSON 错误体会被当成「未确认」无限重发。
// 未启用/配置不完整返回 503 + fail（任务书明确要求）。
func (h *TopupHandlers) handleEpayWebhook(w http.ResponseWriter, r *http.Request) {
	status, retryAfter := h.checkWebhookLimit(r, webhookRateEpay)
	if status != 0 {
		if retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		}
		h.writePaymentAck(w, status, false, "success", "fail")
		return
	}
	// GET 的回调参数在 query，POST 在表单；ParseForm 合并两者到 r.Form。
	// 限制原始体大小：未认证端点不能依赖默认上限。
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBodyBytes)
	if err := r.ParseForm(); err != nil {
		h.writePaymentAck(w, http.StatusBadRequest, false, "success", "fail")
		return
	}
	err := h.svc.HandleEpayNotify(r.Context(), r.Form)
	switch {
	case err == nil:
		h.writePaymentAck(w, http.StatusOK, true, "success", "fail")
	case errors.Is(err, billing.ErrWebhookSignature),
		errors.Is(err, billing.ErrWebhookPayload):
		// 非法签名/载荷：400 + fail，绝不入账。日志只记分类，不记完整 query
		// （其中含签名与商户参数）。
		h.log.Warn("epay notify rejected", "reason", "signature_or_payload")
		h.writePaymentAck(w, http.StatusBadRequest, false, "success", "fail")
	case errors.Is(err, billing.ErrAmountMismatch),
		errors.Is(err, billing.ErrCurrencyMismatch):
		// 金额不符已由服务层置 failed + 告警；400 + fail 让渠道停止重投。
		h.writePaymentAck(w, http.StatusBadRequest, false, "success", "fail")
	case errors.Is(err, billing.ErrOrderNotFound):
		// 订单不存在不泄露存在性：与非法签名同形状的 400 + fail。
		h.log.Warn("epay notify for unknown order")
		h.writePaymentAck(w, http.StatusBadRequest, false, "success", "fail")
	case errors.Is(err, billing.ErrProviderDisabled),
		errors.Is(err, billing.ErrTopupDisabled):
		// 渠道未启用或配置不完整：503，渠道会重投到运营修复为止。
		h.log.Warn("epay notify while provider unavailable")
		h.writePaymentAck(w, http.StatusServiceUnavailable, false, "success", "fail")
	case errors.Is(err, billing.ErrPaymentConfig):
		h.log.Error("epay notify configuration error")
		h.writePaymentAck(w, http.StatusServiceUnavailable, false, "success", "fail")
	case errors.Is(err, billing.ErrOrderStateConflict):
		// 终态（failed/expired/cancelled）订单收到迟到回调：回 success 停止
		// 重投；状态机不允许把它改回可入账（与支付宝同口径）。
		h.log.Warn("epay notify for terminal order", "order_error", err)
		h.writePaymentAck(w, http.StatusOK, true, "success", "fail")
	default:
		h.log.Error("epay notify processing failed", "error", err)
		h.writePaymentAck(w, http.StatusInternalServerError, false, "success", "fail")
	}
}
