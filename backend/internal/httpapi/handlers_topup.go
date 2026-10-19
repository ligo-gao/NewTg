// Package httpapi 的在线充值端点（M5 Task 3 / modules-04 §4.A/B/C/E，
// F-04-01/03/04/05/06/10/11/13；裁决 P-9/P-10/P-12/P-13/P-15）。
//
// 端点面（P-9 冻结）：
//
//	GET  /api/topup/config        已登录用户：档位/上下限/汇率/可用渠道
//	POST /api/orders              已登录用户：Idempotency-Key 幂等下单
//	POST /api/webhooks/stripe     公网：Stripe-Signature 验签后入账
//	POST /api/webhooks/alipay     公网：RSA2 验签后入账
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/store"
)

// webhook 限流阈值（F-08-16 唯一阈值来源：支付 webhook 60 次/分钟/端点）。
const (
	webhookRateLimit    = 60
	webhookRateWindow   = time.Minute
	webhookRateStripe   = "newtg:webhook:rate:stripe:"
	webhookRateAlipay   = "newtg:webhook:rate:alipay:"
	webhookMaxBodyBytes = 1 << 20
)

// problem type URI（沿用 https://newtg.dev/problems/<slug> 约定）。
const (
	problemTypeInvalidTopup        = "https://newtg.dev/problems/invalid_topup"
	problemTypeTopupUnavailable    = "https://newtg.dev/problems/topup_unavailable"
	problemTypeOrderConflict       = "https://newtg.dev/problems/order_state_conflict"
	problemTypeProviderUnavailable = "https://newtg.dev/problems/provider_unavailable"
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
	// Now 是限流窗口时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// StripeBaseURL / AlipayGatewayURL 是渠道地址覆盖（测试）。
	StripeBaseURL    string
	AlipayGatewayURL string
}

// TopupHandlers 持有充值端点的依赖，可并发使用。
type TopupHandlers struct {
	svc *billing.TopupService
	rdb *redis.Client
	log *slog.Logger
	now func() time.Time
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
	return &TopupHandlers{svc: svc, rdb: cfg.Redis, log: log, now: now}, nil
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
}

// RegisterWebhookRoutes 把两个公网回调挂到根 mux。
//
// 使用 HandlePublic 精确模式绕过 SessionAuth/CSRF/Origin：渠道服务器没有
// 会话，也不发送同源头；端点安全完全由验签 + 限流承担。
func (h *TopupHandlers) RegisterWebhookRoutes(rt *Router) {
	rt.HandlePublic("POST /api/webhooks/stripe", http.HandlerFunc(h.handleStripeWebhook))
	rt.HandlePublic("POST /api/webhooks/alipay", http.HandlerFunc(h.handleAlipayWebhook))
}

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

type topupProviderView struct {
	PublishableKey string `json:"publishable_key,omitempty"`
}

type topupConfigResponse struct {
	AmountOptions    []int64                      `json:"amount_options"`
	MinTopup         int64                        `json:"min_topup"`
	MaxTopup         int64                        `json:"max_topup"`
	QuotaPerUnit     int64                        `json:"quota_per_unit"`
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
	Order        topupOrderView `json:"order"`
	ClientSecret string         `json:"client_secret,omitempty"`
	QRCode       string         `json:"qr_code,omitempty"`
}

// ---------------------------------------------------------------------------
// GET /api/topup/config
// ---------------------------------------------------------------------------

func (h *TopupHandlers) handleConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.currentUser(w, r); !ok {
		return
	}
	opts, err := h.svc.Options(r.Context())
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
		EnabledProviders: opts.EnabledProviders,
		Providers:        make(map[string]topupProviderView, len(opts.EnabledProviders)),
	}
	for _, provider := range opts.EnabledProviders {
		view := topupProviderView{}
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
	now := h.now()
	bucket := now.Unix() / int64(webhookRateWindow/time.Second)
	key := fmt.Sprintf("%s%d", prefix, bucket)

	count, err := h.rdb.Incr(r.Context(), key).Result()
	if err != nil {
		h.log.Error("webhook rate limiter unavailable", "prefix", prefix, "error", err)
		w.Header().Set("Retry-After", "5")
		writeProblem(w, http.StatusServiceUnavailable, authProblemServiceUnavailable,
			"Service temporarily unavailable")
		return false
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
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeProblem(w, http.StatusTooManyRequests, authProblemTooManyRequests, "Too many requests")
		return false
	}
	return true
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if success {
		_, _ = io.WriteString(w, "success")
		return
	}
	_, _ = io.WriteString(w, "failure")
}
