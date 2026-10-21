package billing

// 支付令牌服务（S4；P-14 / R-42）。
//
// 【只支持 Stripe】令牌是前端用 Stripe.js 创建并在浏览器端提交给 Stripe 的
// PaymentMethod id（pm_...）。网关不接触卡号/PAN，PCI 范围保持最小。服务层
// 收到 id 后**回查 Stripe** 取 brand/last4/exp，而不是相信客户端提交的
// 展示元数据：客户端可伪造 brand 造成误导，且回查能顺带确认令牌真实存在。
//
// 【加密落库】回查成功后立刻用 SecretSealer（AES-256-GCM）封装令牌；明文
// 只存在于本函数栈帧。数据库中只有密文；读取响应零回显（只回 brand/last4/exp）。
//
// 【删除 fail-closed】删除由 store 在同一事务里关闭引用它的订阅（R-42），
// 本层只做归属/形态校验与日志。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"newtg/gateway/internal/store"
)

var (
	// ErrPaymentMethodsUnavailable：支付服务未装配或 Stripe 未配置（HTTP 503）。
	ErrPaymentMethodsUnavailable = errors.New("billing: payment methods are unavailable")
	// ErrInvalidPaymentToken：令牌形态非法或 Stripe 不认识该令牌（HTTP 422）。
	ErrInvalidPaymentToken = errors.New("billing: payment token is invalid")
)

// stripePaymentMethodIDPattern 限制令牌字符集：它是 URL 路径片段（回查
// Stripe 时直接拼接），必须拒绝 `/`、`?`、空白等可改变请求目标的输入。
var stripePaymentMethodIDPattern = regexp.MustCompile(`^pm_[A-Za-z0-9_-]{4,255}$`)

// PaymentMethodServiceConfig 是支付令牌服务的装配输入。
type PaymentMethodServiceConfig struct {
	Repo store.PaymentMethodRepo
	// Subs 提供自动续费偏好写入（*store.SQLSubscriptionRepo）。与令牌仓储
	// 分开是因为 PUT auto-renew 改的是 subscriptions 行，不碰令牌表。
	Subs   AutoRenewPreferenceStore
	Config ConfigReader
	Sealer PaymentTokenSealer
	Logger *slog.Logger
	// HTTPClient / StripeBaseURL 是测试接缝；生产使用 10s 超时与官方 API。
	HTTPClient    *http.Client
	StripeBaseURL string
}

// AutoRenewPreferenceStore 是自动续费偏好写入的最小接缝。
type AutoRenewPreferenceStore interface {
	SetSubscriptionAutoRenew(ctx context.Context, userID, subscriptionID int64,
		enabled bool, paymentMethodID *string) (store.Subscription, error)
}

// PaymentMethodService 实现令牌 CRUD 与订阅自动续费偏好写入。
type PaymentMethodService struct {
	repo          store.PaymentMethodRepo
	subs          AutoRenewPreferenceStore
	cfg           ConfigReader
	sealer        PaymentTokenSealer
	log           *slog.Logger
	http          *http.Client
	stripeBaseURL string
}

// NewPaymentMethodService 构造服务。Repo、Subs 与 Sealer 必填。
func NewPaymentMethodService(cfg PaymentMethodServiceConfig) (*PaymentMethodService, error) {
	if cfg.Repo == nil {
		return nil, errors.New("billing: PaymentMethodServiceConfig.Repo is required")
	}
	if cfg.Subs == nil {
		return nil, errors.New("billing: PaymentMethodServiceConfig.Subs is required")
	}
	if cfg.Sealer == nil {
		return nil, errors.New("billing: PaymentMethodServiceConfig.Sealer is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimSpace(cfg.StripeBaseURL)
	if base == "" {
		base = DefaultStripeBaseURL
	}
	return &PaymentMethodService{
		repo: cfg.Repo, subs: cfg.Subs, cfg: cfg.Config, sealer: cfg.Sealer, log: log,
		http: client, stripeBaseURL: strings.TrimRight(base, "/"),
	}, nil
}

// CreatePaymentMethod 校验并保存一个 Stripe 令牌。
//
// 入参 provider 只有 stripe（P-14）；token 是 pm_ id。展示字段全部来自
// Stripe 的 PaymentMethod 对象，客户端无法伪造。
func (s *PaymentMethodService) CreatePaymentMethod(ctx context.Context,
	userID int64, provider, token string) (store.PaymentMethod, error) {

	if userID <= 0 {
		return store.PaymentMethod{}, ErrInvalidPaymentToken
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider != ProviderStripe {
		return store.PaymentMethod{}, ErrInvalidProvider
	}
	token = strings.TrimSpace(token)
	if !stripePaymentMethodIDPattern.MatchString(token) {
		return store.PaymentMethod{}, ErrInvalidPaymentToken
	}
	card, err := s.lookupStripeCard(ctx, token)
	if err != nil {
		return store.PaymentMethod{}, err
	}
	cipher, err := s.sealer.Seal(token)
	if err != nil {
		// 封装失败绝不降级为明文存储。
		return store.PaymentMethod{}, fmt.Errorf("billing: seal payment token: %w", err)
	}
	return s.repo.Create(ctx, store.PaymentMethodInput{
		UserID:      userID,
		Provider:    provider,
		TokenCipher: cipher,
		Brand:       card.brand,
		Last4:       card.last4,
		ExpMonth:    card.expMonth,
		ExpYear:     card.expYear,
	})
}

// ListPaymentMethods 返回当前用户的令牌（含密文列；HTTP 层负责投影）。
func (s *PaymentMethodService) ListPaymentMethods(ctx context.Context,
	userID int64) ([]store.PaymentMethod, error) {
	return s.repo.ListByUser(ctx, userID)
}

// DeletePaymentMethod 删除令牌；返回被关闭自动续费的订阅 id（R-42）。
func (s *PaymentMethodService) DeletePaymentMethod(ctx context.Context,
	userID int64, id string) ([]int64, error) {
	if !store.ValidPaymentMethodID(id) {
		return nil, store.ErrPaymentMethodNotFound
	}
	disabled, err := s.repo.Delete(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if len(disabled) > 0 {
		s.log.Info("payment method deleted; auto-renew disabled",
			"user_id", userID, "payment_method_id", id, "subscriptions", disabled)
	}
	return disabled, nil
}

// SetSubscriptionAutoRenew 写入自动续费偏好（归属/令牌有效性由 store 校验）。
func (s *PaymentMethodService) SetSubscriptionAutoRenew(ctx context.Context,
	userID, subscriptionID int64, enabled bool, paymentMethodID *string) (store.Subscription, error) {
	if paymentMethodID != nil {
		trimmed := strings.TrimSpace(*paymentMethodID)
		if trimmed != "" && !store.ValidPaymentMethodID(trimmed) {
			return store.Subscription{}, store.ErrAutoRenewPaymentMethodInvalid
		}
		if trimmed == "" {
			paymentMethodID = nil
		} else {
			paymentMethodID = &trimmed
		}
	}
	return s.subs.SetSubscriptionAutoRenew(ctx, userID, subscriptionID, enabled, paymentMethodID)
}

// stripeCard 是回查得到的展示元数据（非敏感）。
type stripeCard struct {
	brand    string
	last4    string
	expMonth int16
	expYear  int16
}

// lookupStripeCard 用 GET /v1/payment_methods/{id} 验证令牌并取展示字段。
//
// 不解析/不保留任何卡指纹、账单地址或客户信息：网关只需要 brand/last4/exp。
func (s *PaymentMethodService) lookupStripeCard(ctx context.Context, token string) (stripeCard, error) {
	secret, err := s.stripeSecret()
	if err != nil {
		return stripeCard{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.stripeBaseURL+"/v1/payment_methods/"+token, nil)
	if err != nil {
		return stripeCard{}, ErrInvalidPaymentToken
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := s.http.Do(req)
	if err != nil {
		return stripeCard{}, fmt.Errorf("%w: stripe lookup", ErrProviderUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return stripeCard{}, fmt.Errorf("%w: read stripe lookup response", ErrProviderUnavailable)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest {
		// 令牌不存在/格式被渠道拒绝：返回 422，不把渠道原始错误透出。
		return stripeCard{}, ErrInvalidPaymentToken
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return stripeCard{}, fmt.Errorf("%w: stripe lookup HTTP %d",
			ErrProviderUnavailable, resp.StatusCode)
	}
	var parsed struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Card *struct {
			Brand    string `json:"brand"`
			Last4    string `json:"last4"`
			ExpMonth int16  `json:"exp_month"`
			ExpYear  int16  `json:"exp_year"`
		} `json:"card"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID != token ||
		parsed.Type != "card" || parsed.Card == nil ||
		parsed.Card.Brand == "" || len(parsed.Card.Last4) != 4 {
		return stripeCard{}, ErrInvalidPaymentToken
	}
	return stripeCard{
		brand:    parsed.Card.Brand,
		last4:    parsed.Card.Last4,
		expMonth: parsed.Card.ExpMonth,
		expYear:  parsed.Card.ExpYear,
	}, nil
}

// stripeSecret 读取并解封 pay.stripe.secret_key（与充值的密钥读取同口径）。
func (s *PaymentMethodService) stripeSecret() (string, error) {
	if s.cfg == nil {
		return "", ErrPaymentMethodsUnavailable
	}
	raw, ok := s.cfg.Get(ConfigStripeSecretKey)
	if !ok {
		return "", ErrPaymentMethodsUnavailable
	}
	var sealed string
	if err := json.Unmarshal(raw, &sealed); err != nil {
		return "", ErrPaymentMethodsUnavailable
	}
	sealed = strings.TrimSpace(sealed)
	if sealed == "" {
		return "", ErrPaymentMethodsUnavailable
	}
	plain, err := s.sealer.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: stripe secret key cannot be unsealed", ErrPaymentConfig)
	}
	return strings.TrimSpace(plain), nil
}
