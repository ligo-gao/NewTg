package billing

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"newtg/gateway/internal/store"
)

// 本文件是 M5 在线充值的计费核心（modules-04 §4.A/B/C/E，F-04-01/03/04/05/
// 06/10/11/13；裁决 P-1/P-2/P-3/P-6/P-7/P-15）。
//
// 三条不可违反的口径：
//  1. 金额只信服务端订单（GC / I-F-07）：回调金额只用于比对，不一致即
//     `failed` + 告警，绝不按渠道金额入账。
//  2. 幂等双锚：`orders.idempotency_key` 唯一（创建防重）与
//     `orders.provider_txn_id` 唯一（回调防重）；成功统一走
//     `WHERE id=? AND status='pending'` 条件 UPDATE（P-1/P-2）。
//  3. 入账闭环（P-15）：条件 UPDATE + FundingSource.ApplyTx + paid_at +
//     raw_callback + credited_credits 在**同一事务**内完成，任一步失败整体
//     回滚，避免「订单已成功、钱没到」或重复入账。
//
// 渠道协议全部用标准库实现（P-7：不新增 SDK 依赖）。base URL/HTTP client
// 可注入，测试用 httptest 桩验证签名与幂等，不需要真实 Stripe/支付宝。

// 支付配置键（键名冻结：system-config-mapping §4.7 / modules-08 §3.20）。
// 所有键都是站点键，由 configcenter 下发；缺键时按下方冻结默认值处理。
const (
	ConfigOnlineEnabled        = "pay.online_enabled"
	ConfigAmountPresetsCents   = "pay.amount_presets_cny" // 元数组，落库预算时为分
	ConfigMinTopupCents        = "pay.min_topup_cny"      // 元
	ConfigStripeEnabled        = "pay.stripe.enabled"
	ConfigStripeSecretKey      = "pay.stripe.secret_key"
	ConfigStripeWebhookSecret  = "pay.stripe.webhook_secret"
	ConfigStripeMinTopupCents  = "pay.stripe.min_topup_cny" // 元
	ConfigStripePublishableKey = "pay.stripe.publishable_key"
	ConfigAlipayEnabled        = "pay.alipay.enabled"
	ConfigAlipayAppID          = "pay.alipay.app_id"
	ConfigAlipayPrivateKey     = "pay.alipay.private_key"
	ConfigAlipayPublicKey      = "pay.alipay.public_key"
	ConfigAlipayGatewayURL     = "pay.alipay.gateway_url"
	ConfigAlipayMinTopupCents  = "pay.alipay.min_topup_cny" // 元
	ConfigAlipaySandbox        = "pay.alipay.sandbox"
)

// 渠道常量与冻结默认值。
const (
	ProviderStripe = "stripe"
	ProviderAlipay = "alipay"

	// DefaultStripeBaseURL 是 Stripe API 根地址；测试注入 httptest URL。
	DefaultStripeBaseURL = "https://api.stripe.com"
	// DefaultAlipayGatewayURL 是支付宝正式网关（modules-08：沙箱为
	// https://openapi-sandbox.dl.alipaydev.com/gateway.do，由配置覆盖）。
	DefaultAlipayGatewayURL = "https://openapi.alipay.com/gateway.do"
	// StripeTimestampTolerance 是签名时间戳容差（security-06 §2.1 冻结 300s）。
	// 超过窗口的事件拒绝：防重放的第一道是时间窗，第二道才是 txn 唯一约束。
	StripeTimestampTolerance = 300 * time.Second
	// AlipayCurrency 是支付宝结算币种（人民币），回调没有币种字段。
	AlipayCurrency = "CNY"
	// maxIdempotencyKeyRunes 与 orders.idempotency_key 的实际使用口径一致：
	// UUID v4 是 36 字符；128 字符上限给出等价于 UUID 的安全余量，
	// 同时避免把超长字符串写进唯一索引。
	maxIdempotencyKeyRunes = 128
	// maxWebhookBodyBytes webhook 原始体上限：合法事件远小于此；设上限
	// 防止未认证端点被大体积请求拖垮（验签需要完整原始字节，不能流式）。
	maxWebhookBodyBytes = 1 << 20
)

// defaultAmountPresetsCents 是 pay.amount_presets_cny 缺键时的冻结默认
// [10,30,50,100,300,500] 元，换算为分。
var defaultAmountPresetsCents = []int64{1000, 3000, 5000, 10000, 30000, 50000}

// 充值错误面。HTTP 层用 errors.Is 映射状态码；错误文本绝不包含密钥或
// 渠道原始响应（渠道失败细节只进结构化日志）。
var (
	ErrTopupDisabled       = errors.New("billing: online top-up is disabled")
	ErrProviderDisabled    = errors.New("billing: payment provider is not enabled or not configured")
	ErrInvalidProvider     = errors.New("billing: invalid payment provider")
	ErrInvalidCurrency     = errors.New("billing: invalid currency")
	ErrInvalidIdempotency  = errors.New("billing: invalid idempotency key")
	ErrIdempotencyConflict = errors.New("billing: idempotency key belongs to another user")
	ErrProviderUnavailable = errors.New("billing: payment provider unavailable")
	ErrWebhookSignature    = errors.New("billing: webhook signature verification failed")
	ErrWebhookPayload      = errors.New("billing: invalid webhook payload")
	ErrOrderNotFound       = errors.New("billing: order not found")
	ErrOrderStateConflict  = errors.New("billing: order state conflict")
	ErrAmountMismatch      = errors.New("billing: callback amount mismatch")
	ErrCurrencyMismatch    = errors.New("billing: callback currency mismatch")
	ErrPaymentConfig       = errors.New("billing: payment configuration incomplete")
)

// SecretOpener 是配置中心 SecretSealer 的最小读取面。billing 不直接依赖
// configcenter，避免计费核心被配置中心实现绑死；生产装配传入 sealer。
type SecretOpener interface {
	Open(sealed string) (string, error)
}

// TopupOptions 是 GET /api/topup/config 的服务端投影（P-9）。
//
// enabled_providers 的判定是「pay.online_enabled ∧ provider.enabled ∧
// 配置完整（含密钥可解封）」——模块 08 F-04-01 的「已注册 ∧ 已启用 ∧
// 配置完整」，本任务只实现两个渠道。
type TopupOptions struct {
	AmountOptions        []int64  `json:"amount_options"`
	MinTopup             int64    `json:"min_topup"`
	MaxTopup             int64    `json:"max_topup"`
	QuotaPerUnit         int64    `json:"quota_per_unit"`
	EnabledProviders     []string `json:"enabled_providers"`
	StripePublishableKey string   `json:"-"`
}

// TopupOrder 是一次创建订单的结果。
//
// 与 brief 的 `(*Order, error)` 相比多出了渠道载荷：P-9 要求返回
// `client_secret` / `qr_code`，而这两者都不落库（client_secret 等同支付
// 凭据，落库会扩大泄漏面）。Replayed 用于 HTTP 层区分 201/200。
type TopupOrder struct {
	Order        *store.Order
	ClientSecret string
	QRCode       string
	Replayed     bool
}

// TopupService 是在线充值的应用服务，可并发使用。
type TopupService struct {
	db      *gorm.DB
	funding FundingSource
	cfg     ConfigReader
	sealer  SecretOpener
	log     *slog.Logger
	now     func() time.Time
	http    *http.Client
	// stripeBaseURL 是 Stripe API 根地址（测试注入 httptest）。
	stripeBaseURL string
	// alipayGatewayURL 覆盖 pay.alipay.gateway_url（测试注入 httptest）。
	alipayGatewayURL string
}

// TopupServiceConfig 是 TopupService 的装配输入。
type TopupServiceConfig struct {
	DB      *gorm.DB
	Funding FundingSource
	Config  ConfigReader
	Sealer  SecretOpener
	Logger  *slog.Logger
	// Now 是签名时间窗与 out_trade_no 的时钟接缝；nil 时 time.Now。
	Now func() time.Time
	// HTTPClient 是所有渠道出站请求的客户端；nil 时 10s 超时的默认值。
	HTTPClient *http.Client
	// StripeBaseURL 覆盖 Stripe API 根地址（测试）。
	StripeBaseURL string
	// AlipayGatewayURL 覆盖支付宝网关（测试）；生产由 pay.alipay.gateway_url 决定。
	AlipayGatewayURL string
}

// NewTopupService 构造充值服务。DB 与 Funding 必填：订单、余额与流水必须
// 同库同事务（P-15）。cfg 可为 nil（全部走冻结默认值，但渠道默认关闭，
// 因此实际无法下单）。
func NewTopupService(cfg TopupServiceConfig) (*TopupService, error) {
	if cfg.DB == nil {
		return nil, errors.New("billing: TopupServiceConfig.DB is required")
	}
	funding := cfg.Funding
	if funding == nil {
		funding = NewWalletFunding(cfg.DB)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	client := cfg.HTTPClient
	if client == nil {
		// 渠道出站必须有超时：验签/下单请求悬挂会占满 handler goroutine。
		client = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimSpace(cfg.StripeBaseURL)
	if base == "" {
		base = DefaultStripeBaseURL
	}
	return &TopupService{
		db:               cfg.DB,
		funding:          funding,
		cfg:              cfg.Config,
		sealer:           cfg.Sealer,
		log:              log,
		now:              now,
		http:             client,
		stripeBaseURL:    strings.TrimRight(base, "/"),
		alipayGatewayURL: strings.TrimSpace(cfg.AlipayGatewayURL),
	}, nil
}

// ---------------------------------------------------------------------------
// 配置读取
// ---------------------------------------------------------------------------

func (s *TopupService) configBool(key string, def bool) bool {
	if s.cfg == nil {
		return def
	}
	raw, ok := s.cfg.Get(key)
	if !ok {
		return def
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

func (s *TopupService) configString(key string) string {
	if s.cfg == nil {
		return ""
	}
	raw, ok := s.cfg.Get(key)
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// configSecret 读取密钥类键。配置中心写入时经 AES-256-GCM 密封，因此有
// sealer 时必须解封；解封失败绝不回退到「把密文当明文用」——那会让验签
// 以不可诊断的方式永远失败，也让错误配置看起来像攻击。
func (s *TopupService) configSecret(key string) (string, error) {
	sealed := s.configString(key)
	if sealed == "" {
		return "", nil
	}
	if s.sealer == nil {
		return sealed, nil
	}
	plain, err := s.sealer.Open(sealed)
	if err != nil {
		return "", fmt.Errorf("%w: %s cannot be unsealed", ErrPaymentConfig, key)
	}
	return strings.TrimSpace(plain), nil
}

// configYuan 读取以「元」为单位的整数键并换算为分。缺键返回 def。
// 非法值返回配置错误而不是静默回退：金额下限被悄悄改回默认值会让运营
// 以为限制已生效，实际没有。
func (s *TopupService) configYuan(key string, def int64) (int64, error) {
	if s.cfg == nil {
		return def, nil
	}
	raw, ok := s.cfg.Get(key)
	if !ok {
		return def, nil
	}
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return def, nil
	}
	if strings.HasPrefix(text, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, fmt.Errorf("%w: %s", ErrPaymentConfig, key)
		}
		text = strings.TrimSpace(str)
	}
	yuan, err := strconv.ParseInt(text, 10, 64)
	if err != nil || yuan < 0 {
		return 0, fmt.Errorf("%w: %s must be a non-negative integer", ErrPaymentConfig, key)
	}
	if yuan > MaxTopupCents/100 {
		return 0, fmt.Errorf("%w: %s exceeds max top-up", ErrPaymentConfig, key)
	}
	return yuan * 100, nil
}

// quotaPerUnit 读取 billing.quota_per_unit（与 RedemptionService 同口径）。
func (s *TopupService) quotaPerUnit() (int64, error) {
	if s.cfg == nil {
		return DefaultQuotaPerUnit, nil
	}
	raw, ok := s.cfg.Get(ConfigQuotaPerUnit)
	if !ok {
		return DefaultQuotaPerUnit, nil
	}
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return DefaultQuotaPerUnit, nil
	}
	if strings.HasPrefix(text, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrInvalidQuotaPerUnit, err)
		}
		text = strings.TrimSpace(str)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 1 || value > 1_000_000 {
		return 0, fmt.Errorf("%w: %q must be an integer in 1-1000000", ErrInvalidQuotaPerUnit, text)
	}
	return value, nil
}

// amountPresets 读取 pay.amount_presets_cny（元数组）并换算为分。
func (s *TopupService) amountPresets() ([]int64, error) {
	if s.cfg == nil {
		return append([]int64(nil), defaultAmountPresetsCents...), nil
	}
	raw, ok := s.cfg.Get(ConfigAmountPresetsCents)
	if !ok {
		return append([]int64(nil), defaultAmountPresetsCents...), nil
	}
	var yuan []int64
	if err := json.Unmarshal(raw, &yuan); err != nil {
		return nil, fmt.Errorf("%w: %s must be an integer array", ErrPaymentConfig, ConfigAmountPresetsCents)
	}
	if len(yuan) == 0 || len(yuan) > 20 {
		return nil, fmt.Errorf("%w: %s must contain 1-20 entries", ErrPaymentConfig, ConfigAmountPresetsCents)
	}
	out := make([]int64, 0, len(yuan))
	for _, v := range yuan {
		if v <= 0 || v*100 > MaxTopupCents {
			return nil, fmt.Errorf("%w: %s contains invalid amount %d", ErrPaymentConfig, ConfigAmountPresetsCents, v)
		}
		out = append(out, v*100)
	}
	return out, nil
}

// providerReady 判定某渠道是否可对外展示/下单，并返回其最低金额。
//
// P-13：`pay.online_enabled` 与 `pay.<provider>.enabled` 同时成立且配置完整
// （密钥存在、可解封）才允许创建订单。配置不完整返回 ErrProviderDisabled
// 而不是「继续尝试」——静默回退会把配置错误变成用户的支付失败。
func (s *TopupService) providerReady(provider string) (int64, error) {
	if !s.configBool(ConfigOnlineEnabled, true) {
		return 0, ErrTopupDisabled
	}
	switch provider {
	case ProviderStripe:
		if !s.configBool(ConfigStripeEnabled, false) {
			return 0, ErrProviderDisabled
		}
		secret, err := s.configSecret(ConfigStripeSecretKey)
		if err != nil {
			return 0, err
		}
		webhook, err := s.configSecret(ConfigStripeWebhookSecret)
		if err != nil {
			return 0, err
		}
		if secret == "" || webhook == "" {
			return 0, fmt.Errorf("%w: stripe secret_key/webhook_secret missing", ErrProviderDisabled)
		}
		return s.configYuan(ConfigStripeMinTopupCents, DefaultProviderMinTopupCents)
	case ProviderAlipay:
		if !s.configBool(ConfigAlipayEnabled, false) {
			return 0, ErrProviderDisabled
		}
		appID := s.configString(ConfigAlipayAppID)
		privateKey, err := s.configSecret(ConfigAlipayPrivateKey)
		if err != nil {
			return 0, err
		}
		publicKey, err := s.configSecret(ConfigAlipayPublicKey)
		if err != nil {
			return 0, err
		}
		if appID == "" || privateKey == "" || publicKey == "" {
			return 0, fmt.Errorf("%w: alipay app_id/private_key/public_key missing", ErrProviderDisabled)
		}
		// 私钥/公钥必须是可解析的 PEM：与其在用户点「去支付」时才 500，
		// 不如在可用性判定阶段就把该渠道标记为不可用。
		if _, err := parseRSAPrivateKey(privateKey); err != nil {
			return 0, fmt.Errorf("%w: alipay private_key invalid", ErrProviderDisabled)
		}
		if _, err := parseRSAPublicKey(publicKey); err != nil {
			return 0, fmt.Errorf("%w: alipay public_key invalid", ErrProviderDisabled)
		}
		return s.configYuan(ConfigAlipayMinTopupCents, DefaultProviderMinTopupCents)
	default:
		return 0, ErrInvalidProvider
	}
}

// Options 返回充值页配置（P-9）。任何配置错误都向上抛：充值页宁可显示
// 错误，也不能展示一份与下单校验不一致的金额档位。
func (s *TopupService) Options(_ context.Context) (TopupOptions, error) {
	presets, err := s.amountPresets()
	if err != nil {
		return TopupOptions{}, err
	}
	globalMin, err := s.configYuan(ConfigMinTopupCents, DefaultMinTopupCents)
	if err != nil {
		return TopupOptions{}, err
	}
	qpu, err := s.quotaPerUnit()
	if err != nil {
		return TopupOptions{}, err
	}
	opts := TopupOptions{
		AmountOptions:    presets,
		MinTopup:         globalMin,
		MaxTopup:         MaxTopupCents,
		QuotaPerUnit:     qpu,
		EnabledProviders: []string{},
	}
	// 全局最低与渠道最低在下单时取 max（F-04-01 步骤 4）；P-9 冻结的响应
	// 形状只有全局 min_topup，渠道级下限由服务端在 POST /api/orders 再次
	// 校验，前端无需重复实现。
	for _, provider := range []string{ProviderStripe, ProviderAlipay} {
		if _, err := s.providerReady(provider); err != nil {
			if errors.Is(err, ErrProviderDisabled) || errors.Is(err, ErrTopupDisabled) {
				continue
			}
			return TopupOptions{}, err
		}
		opts.EnabledProviders = append(opts.EnabledProviders, provider)
	}
	if containsString(opts.EnabledProviders, ProviderStripe) {
		opts.StripePublishableKey = s.configString(ConfigStripePublishableKey)
	}
	return opts, nil
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 创建订单
// ---------------------------------------------------------------------------

// CreateTopupOrder 幂等创建订单并创建渠道支付单。
//
// P-2 的冲突处理：先按 idempotency_key 查已有订单——同用户直接返回；
// 不同用户返回 ErrIdempotencyConflict（全表唯一约束，不能因为 UUID 巧合
// 把别人的订单内容回给当前用户）。插入撞唯一约束时重查一次，覆盖并发竞态。
func (s *TopupService) CreateTopupOrder(ctx context.Context, userID, amount int64,
	currency, provider, idempotencyKey string) (*TopupOrder, error) {

	if userID <= 0 {
		return nil, fmt.Errorf("%w: user id must be > 0", ErrInvalidFundingCredit)
	}
	if provider != ProviderStripe && provider != ProviderAlipay {
		return nil, ErrInvalidProvider
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" || utf8.RuneCountInString(key) > maxIdempotencyKeyRunes || hasControlChars(key) {
		return nil, ErrInvalidIdempotency
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		// 站点计价币种默认 CNY（modules-04 的金额档位即人民币）；
		// 显式传入时必须是 ISO-4217 三字母大写（orders_currency_chk）。
		currency = AlipayCurrency
	}
	if !validCurrency(currency) {
		return nil, ErrInvalidCurrency
	}
	// 支付宝只按人民币收款：在落订单之前拒绝，避免留下一个注定失败的
	// failed 订单（渠道能力差异应在下单入口就暴露）。
	if provider == ProviderAlipay && currency != AlipayCurrency {
		return nil, ErrInvalidCurrency
	}

	providerMin, err := s.providerReady(provider)
	if err != nil {
		return nil, err
	}
	globalMin, err := s.configYuan(ConfigMinTopupCents, DefaultMinTopupCents)
	if err != nil {
		return nil, err
	}
	if err := ValidateTopupAmount(amount, globalMin, providerMin, MaxTopupCents); err != nil {
		return nil, err
	}

	// 第一步：幂等查重（命中即返回，不再调用渠道）。
	if existing, err := s.findOrderByIdempotencyKey(ctx, key); err != nil {
		return nil, err
	} else if existing != nil {
		return s.replayOrder(existing, userID)
	}

	order := &store.Order{
		UserID:         userID,
		Provider:       provider,
		Amount:         amount,
		Currency:       currency,
		Status:         string(OrderPending),
		IdempotencyKey: key,
	}
	err = s.db.WithContext(ctx).Raw(`
		INSERT INTO orders (user_id, provider, amount, currency, status, idempotency_key)
		VALUES (?, ?, ?, ?, 'pending', ?)
		RETURNING id, created_at, updated_at`,
		userID, provider, amount, currency, key).
		Row().Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			// 并发同 key：后到者按首次结果返回。回查失败说明冲突来自
			// provider_txn_id（本任务在插入时不可能），按内部错误处理。
			existing, findErr := s.findOrderByIdempotencyKey(ctx, key)
			if findErr != nil {
				return nil, findErr
			}
			if existing != nil {
				return s.replayOrder(existing, userID)
			}
		}
		return nil, fmt.Errorf("billing: create order: %w", err)
	}

	// 第二步：创建渠道支付单。失败时订单置 failed（F-04-03 异常流程：
	// PaymentIntent 创建失败 → 订单 failed + Toast），绝不留下永久 pending。
	switch provider {
	case ProviderStripe:
		result, createErr := s.createStripePaymentIntent(ctx, order)
		if createErr != nil {
			s.markOrderFailed(ctx, order.ID, failureCodeForProvider(createErr))
			return nil, fmt.Errorf("%w: stripe payment intent", ErrProviderUnavailable)
		}
		order.ProviderTxnID = &result.id
		return &TopupOrder{Order: order, ClientSecret: result.clientSecret}, nil
	case ProviderAlipay:
		// out_trade_no 由我们在调用前分配并写库：异步通知只能靠它找回
		// 订单，必须在 precreate 之前就存在（回调早于响应返回也不丢单）。
		outTradeNo := fmt.Sprintf("TG%d", order.ID)
		if err := s.db.WithContext(ctx).Exec(
			`UPDATE orders SET provider_txn_id = ?, updated_at = now()
			 WHERE id = ? AND status = 'pending'`, outTradeNo, order.ID).Error; err != nil {
			s.markOrderFailed(ctx, order.ID, "provider_unavailable")
			return nil, fmt.Errorf("billing: store alipay out_trade_no: %w", err)
		}
		order.ProviderTxnID = &outTradeNo
		qrCode, createErr := s.createAlipayPrecreate(ctx, outTradeNo, amount, currency)
		if createErr != nil {
			s.markOrderFailed(ctx, order.ID, failureCodeForProvider(createErr))
			return nil, fmt.Errorf("%w: alipay precreate", ErrProviderUnavailable)
		}
		return &TopupOrder{Order: order, QRCode: qrCode}, nil
	}
	return nil, ErrInvalidProvider
}

// replayOrder 返回已有订单；user_id 不一致时 409（P-2）。
//
// 回放不重新创建渠道支付单：重复 POST 必须无副作用。首次响应中的
// client_secret/qr_code 不再返回（有意：client_secret 是支付凭据，
// 不应因为持有旧 idempotency key 就无限次取回）。
func (s *TopupService) replayOrder(order *store.Order, userID int64) (*TopupOrder, error) {
	if order.UserID != userID {
		return nil, ErrIdempotencyConflict
	}
	return &TopupOrder{Order: order, Replayed: true}, nil
}

func (s *TopupService) findOrderByIdempotencyKey(ctx context.Context, key string) (*store.Order, error) {
	var order store.Order
	err := s.db.WithContext(ctx).Where("idempotency_key = ?", key).First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("billing: lookup idempotent order: %w", err)
	}
	return &order, nil
}

// failureCodeForProvider 把渠道错误映射到 orders_failure_code_chk 的受控
// 词汇。4xx（配置/参数被渠道拒绝）→ provider_rejected；网络/5xx →
// provider_unavailable。
func failureCodeForProvider(err error) string {
	var pe *providerHTTPError
	if errors.As(err, &pe) && pe.status >= 400 && pe.status < 500 {
		return "provider_rejected"
	}
	return "provider_unavailable"
}

// markOrderFailed 只迁移 pending → failed；终态订单不被渠道失败改写。
func (s *TopupService) markOrderFailed(ctx context.Context, orderID int64, code string) {
	res := s.db.WithContext(ctx).Exec(
		`UPDATE orders SET status = 'failed', failure_code = ?, updated_at = now()
		 WHERE id = ? AND status = 'pending'`, code, orderID)
	if res.Error != nil {
		s.log.Error("mark order failed", "order_id", orderID, "error", res.Error)
	}
}

func validCurrency(v string) bool {
	if len(v) != 3 {
		return false
	}
	for _, r := range v {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func hasControlChars(v string) bool {
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Stripe
// ---------------------------------------------------------------------------

type stripePaymentIntent struct {
	id           string
	clientSecret string
}

// providerHTTPError 携带渠道 HTTP 状态（不包含响应正文，避免密钥/卡信息
// 进入错误链和日志）。
type providerHTTPError struct {
	provider string
	status   int
}

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("billing: %s returned HTTP %d", e.provider, e.status)
}

// createStripePaymentIntent 用表单 POST 创建 PaymentIntent（P-7）。
//
// 只发送金额、币种与订单号元数据；卡信息由 Stripe Elements 在浏览器端
// 直接提交给 Stripe，网关永远不接触卡号（PCI 范围最小化）。
func (s *TopupService) createStripePaymentIntent(ctx context.Context, order *store.Order) (*stripePaymentIntent, error) {
	secret, err := s.configSecret(ConfigStripeSecretKey)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("amount", strconv.FormatInt(order.Amount, 10))
	form.Set("currency", strings.ToLower(order.Currency))
	form.Set("payment_method_types[0]", "card")
	form.Set("metadata[order_id]", strconv.FormatInt(order.ID, 10))
	form.Set("metadata[user_id]", strconv.FormatInt(order.UserID, 10))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.stripeBaseURL+"/v1/payment_intents", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing: stripe payment intent request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("billing: read stripe response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &providerHTTPError{provider: "stripe", status: resp.StatusCode}
	}
	var parsed struct {
		ID           string `json:"id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.ID == "" || parsed.ClientSecret == "" {
		return nil, fmt.Errorf("billing: stripe response missing id/client_secret")
	}
	// provider_txn_id 是回调防重的唯一锚：必须在返回前落库，否则支付成功的
	// webhook 找不到订单。条件 UPDATE 保证终态订单不再被改写。
	if err := s.db.WithContext(ctx).Exec(
		`UPDATE orders SET provider_txn_id = ?, updated_at = now()
		 WHERE id = ? AND status = 'pending'`, parsed.ID, order.ID).Error; err != nil {
		return nil, fmt.Errorf("billing: store stripe payment intent id: %w", err)
	}
	return &stripePaymentIntent{id: parsed.ID, clientSecret: parsed.ClientSecret}, nil
}

// VerifyStripeSignature 验证 `Stripe-Signature: t=...,v1=...`。
//
// 计时攻击防护用 hmac.Equal；时间窗 300s 防重放（security-06 §2.1）。
// 签名对象是 `t + "." + 原始请求体`：必须使用未经 JSON 反序列化的原始
// 字节（SI-F-04），否则字段重排会让合法签名失效。
func VerifyStripeSignature(payload []byte, header, secret string, now time.Time) error {
	if secret == "" {
		return fmt.Errorf("%w: webhook secret not configured", ErrWebhookSignature)
	}
	var (
		timestamp int64 = -1
		sigs      []string
	)
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			v, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return fmt.Errorf("%w: invalid timestamp", ErrWebhookSignature)
			}
			timestamp = v
		case "v1":
			sigs = append(sigs, kv[1])
		}
	}
	if timestamp < 0 || len(sigs) == 0 {
		return fmt.Errorf("%w: missing t/v1", ErrWebhookSignature)
	}
	skew := now.Unix() - timestamp
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(StripeTimestampTolerance/time.Second) {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrWebhookSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := mac.Sum(nil)
	for _, sig := range sigs {
		got, err := hex.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, expected) {
			return nil
		}
	}
	return fmt.Errorf("%w: signature mismatch", ErrWebhookSignature)
}

// stripeEvent 只声明本任务实际消费的字段；其余事件字段一律忽略。
type stripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object struct {
			ID             string `json:"id"`
			AmountReceived int64  `json:"amount_received"`
			Currency       string `json:"currency"`
			Status         string `json:"status"`
		} `json:"object"`
	} `json:"data"`
}

// HandleStripeWebhook 验签并处理 Stripe 异步通知（F-04-04）。
//
// 只处理 `payment_intent.succeeded`；其他事件返回 nil（200），否则 Stripe
// 会对无关事件无限重投。订单不存在时返回 ErrOrderNotFound，由 HTTP 层记
// 告警并 200（避免重投风暴；security-06 的 RC-F-04 提案保留为后续改进）。
func (s *TopupService) HandleStripeWebhook(ctx context.Context, payload []byte, sig string) error {
	secret, err := s.configSecret(ConfigStripeWebhookSecret)
	if err != nil {
		return err
	}
	if err := VerifyStripeSignature(payload, sig, secret, s.now()); err != nil {
		return err
	}
	if len(payload) > maxWebhookBodyBytes {
		return fmt.Errorf("%w: body too large", ErrWebhookPayload)
	}
	var event stripeEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("%w: %v", ErrWebhookPayload, err)
	}
	if event.Type != "payment_intent.succeeded" {
		// processing/canceled 等事件按状态机忽略；乱序到达的 succeeded
		// 后面的 processing 更不能把已成功订单改回待支付。
		return nil
	}
	if event.Data.Object.ID == "" {
		return fmt.Errorf("%w: missing payment intent id", ErrWebhookPayload)
	}
	order, err := s.findOrderByProviderTxn(ctx, event.Data.Object.ID)
	if err != nil {
		return err
	}
	if order.Provider != ProviderStripe {
		// 渠道密钥隔离（security-06 §2.10）：用 Stripe 回调去结算支付宝
		// 订单属异常场景，告警但不入账。
		return fmt.Errorf("%w: txn %s belongs to provider %s", ErrOrderStateConflict,
			event.Data.Object.ID, order.Provider)
	}
	raw := marshalWhitelisted(map[string]any{
		"provider":          ProviderStripe,
		"event_id":          event.ID,
		"event_type":        event.Type,
		"payment_intent":    event.Data.Object.ID,
		"amount_received":   event.Data.Object.AmountReceived,
		"currency":          event.Data.Object.Currency,
		"payment_status":    event.Data.Object.Status,
		"provider_currency": strings.ToUpper(event.Data.Object.Currency),
		"provider_amount":   event.Data.Object.AmountReceived,
	})
	return s.settleOrder(ctx, order, event.Data.Object.AmountReceived,
		strings.ToUpper(event.Data.Object.Currency), raw)
}

// ---------------------------------------------------------------------------
// 支付宝
// ---------------------------------------------------------------------------

// Alipay 协议参数（RSA2 已冻结为唯一签名算法；不再支持 RSA1）。
const (
	alipaySignTypeRSA2 = "RSA2"
	alipayFormat       = "JSON"
	alipayCharset      = "utf-8"
	alipayVersion      = "1.0"
)

// alipayCST 是支付宝网关要求的北京时间（UTC+8，无夏令时）。
func alipayCST() *time.Location { return time.FixedZone("CST", 8*3600) }

func (s *TopupService) alipayGateway() string {
	if s.alipayGatewayURL != "" {
		return s.alipayGatewayURL
	}
	if v := s.configString(ConfigAlipayGatewayURL); v != "" {
		return v
	}
	return DefaultAlipayGatewayURL
}

// alipayParams 组装公共参数（含 sign），返回可直接 GET 的 url.Values。
//
// 签名串 = 除 sign/sign_type 外的参数按键名排序后的 `k=v&...`。值使用
// 原始明文（biz_content 是 JSON 字符串），与支付宝规范一致；URL 编码只
// 发生在传输层，不参与签名。
func (s *TopupService) alipayParams(method, bizContent, privateKeyPEM string) (url.Values, error) {
	params := map[string]string{
		"app_id":      s.configString(ConfigAlipayAppID),
		"method":      method,
		"format":      alipayFormat,
		"charset":     alipayCharset,
		"sign_type":   alipaySignTypeRSA2,
		"timestamp":   s.now().In(alipayCST()).Format("2006-01-02 15:04:05"),
		"version":     alipayVersion,
		"biz_content": bizContent,
	}
	sign, err := signRSA2(params, privateKeyPEM)
	if err != nil {
		return nil, err
	}
	params["sign"] = sign
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values, nil
}

// alipayCall 发起一次 signed GET 并校验响应签名。
//
// 响应签名覆盖 `alipay_<method>_response` 节点的**原始 JSON 字节**：用
// json.RawMessage 保留原文，避免「反序列化再序列化」改变空白/转义顺序而
// 让合法响应验签失败（与 webhook 原始体保真同一原则）。
func (s *TopupService) alipayCall(ctx context.Context, method, bizContent, privateKeyPEM, publicKeyPEM string) (json.RawMessage, error) {
	values, err := s.alipayParams(method, bizContent, privateKeyPEM)
	if err != nil {
		return nil, err
	}
	endpoint := s.alipayGateway()
	target := endpoint
	if strings.Contains(endpoint, "?") {
		target += "&" + values.Encode()
	} else {
		target += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("billing: alipay request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("billing: read alipay response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &providerHTTPError{provider: "alipay", status: resp.StatusCode}
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("billing: parse alipay response: %w", err)
	}
	nodeKey := "alipay_" + strings.ReplaceAll(method, ".", "_") + "_response"
	node, ok := envelope[nodeKey]
	if !ok {
		return nil, fmt.Errorf("billing: alipay response missing %s", nodeKey)
	}
	var sign string
	if raw, ok := envelope["sign"]; ok {
		_ = json.Unmarshal(raw, &sign)
	}
	if sign == "" {
		return nil, fmt.Errorf("billing: alipay response unsigned")
	}
	if err := verifyRSA2Raw(node, sign, publicKeyPEM); err != nil {
		return nil, fmt.Errorf("billing: alipay response signature: %w", err)
	}
	return node, nil
}

// createAlipayPrecreate 调用 `alipay.trade.precreate` 获取二维码内容。
func (s *TopupService) createAlipayPrecreate(ctx context.Context, outTradeNo string, amountCents int64, currency string) (string, error) {
	privateKey, err := s.configSecret(ConfigAlipayPrivateKey)
	if err != nil {
		return "", err
	}
	publicKey, err := s.configSecret(ConfigAlipayPublicKey)
	if err != nil {
		return "", err
	}
	// 支付宝只按人民币收款；订单币种非 CNY 时创建渠道单没有意义，直接拒绝。
	if currency != AlipayCurrency {
		return "", fmt.Errorf("%w: alipay only supports CNY", ErrInvalidCurrency)
	}
	biz, err := json.Marshal(map[string]string{
		"out_trade_no": outTradeNo,
		"total_amount": centsToYuanString(amountCents),
		"subject":      "Wallet top-up",
	})
	if err != nil {
		return "", err
	}
	node, err := s.alipayCall(ctx, "alipay.trade.precreate", string(biz), privateKey, publicKey)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Code   string `json:"code"`
		Msg    string `json:"msg"`
		QRCode string `json:"qr_code"`
	}
	if err := json.Unmarshal(node, &parsed); err != nil {
		return "", fmt.Errorf("billing: parse alipay precreate response: %w", err)
	}
	if parsed.Code != "10000" || parsed.QRCode == "" {
		// 渠道拒绝（参数/商户状态）不把原文带进错误：其中可能含商户号。
		return "", fmt.Errorf("%w: alipay precreate code %s", ErrProviderUnavailable, parsed.Code)
	}
	return parsed.QRCode, nil
}

// AlipayTradeState 是 `alipay.trade.query` 的常用字段（Task 4 轮询兜底使用）。
type AlipayTradeState struct {
	OutTradeNo  string
	TradeStatus string
	TotalAmount string
}

// QueryAlipayTrade 主动查询交易状态（Task 4 回调丢失兜底）。
func (s *TopupService) QueryAlipayTrade(ctx context.Context, outTradeNo string) (*AlipayTradeState, error) {
	privateKey, err := s.configSecret(ConfigAlipayPrivateKey)
	if err != nil {
		return nil, err
	}
	publicKey, err := s.configSecret(ConfigAlipayPublicKey)
	if err != nil {
		return nil, err
	}
	biz, err := json.Marshal(map[string]string{"out_trade_no": outTradeNo})
	if err != nil {
		return nil, err
	}
	node, err := s.alipayCall(ctx, "alipay.trade.query", string(biz), privateKey, publicKey)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Code        string `json:"code"`
		OutTradeNo  string `json:"out_trade_no"`
		TradeStatus string `json:"trade_status"`
		TotalAmount string `json:"total_amount"`
	}
	if err := json.Unmarshal(node, &parsed); err != nil {
		return nil, fmt.Errorf("billing: parse alipay query response: %w", err)
	}
	if parsed.Code != "10000" {
		return nil, fmt.Errorf("%w: alipay query code %s", ErrProviderUnavailable, parsed.Code)
	}
	return &AlipayTradeState{
		OutTradeNo:  parsed.OutTradeNo,
		TradeStatus: parsed.TradeStatus,
		TotalAmount: parsed.TotalAmount,
	}, nil
}

// CloseAlipayTrade 关闭交易（Task 4 订单超时 worker 使用）。无关单能力
// 的渠道错误按 provider_unavailable 上报，由调用方决定是否只置 expired。
func (s *TopupService) CloseAlipayTrade(ctx context.Context, outTradeNo string) error {
	privateKey, err := s.configSecret(ConfigAlipayPrivateKey)
	if err != nil {
		return err
	}
	publicKey, err := s.configSecret(ConfigAlipayPublicKey)
	if err != nil {
		return err
	}
	biz, err := json.Marshal(map[string]string{"out_trade_no": outTradeNo})
	if err != nil {
		return err
	}
	node, err := s.alipayCall(ctx, "alipay.trade.close", string(biz), privateKey, publicKey)
	if err != nil {
		return err
	}
	var parsed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(node, &parsed); err != nil || parsed.Code != "10000" {
		return fmt.Errorf("%w: alipay close rejected", ErrProviderUnavailable)
	}
	return nil
}

// HandleAlipayNotify 验签并处理支付宝异步通知（F-04-06）。
//
// 只有 `TRADE_SUCCESS` 触发入账；`WAIT_BUYER_PAY` 等中间态返回 nil
// （HTTP 层回 success），既不推进状态也不让支付宝重投。
func (s *TopupService) HandleAlipayNotify(ctx context.Context, params url.Values) error {
	publicKey, err := s.configSecret(ConfigAlipayPublicKey)
	if err != nil {
		return err
	}
	if publicKey == "" {
		return fmt.Errorf("%w: alipay public_key not configured", ErrWebhookSignature)
	}
	flat := make(map[string]string, len(params))
	for key, values := range params {
		if len(values) > 0 {
			flat[key] = values[len(values)-1]
		}
	}
	if err := verifyRSA2(flat, publicKey); err != nil {
		return err
	}
	// 次级校验：手续费/密钥按 app 隔离，通知的 app_id 必须与本站一致。
	if configured := s.configString(ConfigAlipayAppID); configured != "" {
		if got := flat["app_id"]; got != "" && got != configured {
			return fmt.Errorf("%w: app_id mismatch", ErrWebhookPayload)
		}
	}
	if status := flat["trade_status"]; status != "TRADE_SUCCESS" {
		return nil
	}
	outTradeNo := flat["out_trade_no"]
	if outTradeNo == "" {
		return fmt.Errorf("%w: missing out_trade_no", ErrWebhookPayload)
	}
	order, err := s.findOrderByProviderTxn(ctx, outTradeNo)
	if err != nil {
		return err
	}
	if order.Provider != ProviderAlipay {
		return fmt.Errorf("%w: txn %s belongs to provider %s", ErrOrderStateConflict,
			outTradeNo, order.Provider)
	}
	cents, err := yuanToCents(flat["total_amount"])
	if err != nil {
		return fmt.Errorf("%w: total_amount", ErrWebhookPayload)
	}
	raw := marshalWhitelisted(map[string]any{
		"provider":     ProviderAlipay,
		"app_id":       flat["app_id"],
		"out_trade_no": outTradeNo,
		"trade_no":     flat["trade_no"],
		"trade_status": flat["trade_status"],
		"total_amount": flat["total_amount"],
	})
	return s.settleOrder(ctx, order, cents, AlipayCurrency, raw)
}

// signRSA2 对参数做支付宝 RSA2 签名（SHA256withRSA + PKCS#1 v1.5）。
func signRSA2(params map[string]string, privateKeyPEM string) (string, error) {
	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonicalAlipayString(params)))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// verifyRSA2 验证 url.Values 形态的通知（已 URL 解码后传入）。
func verifyRSA2(params map[string]string, publicKeyPEM string) error {
	sign := strings.TrimSpace(params["sign"])
	if sign == "" {
		return fmt.Errorf("%w: missing sign", ErrWebhookSignature)
	}
	if st := params["sign_type"]; st != "" && st != alipaySignTypeRSA2 {
		return fmt.Errorf("%w: unsupported sign_type %s", ErrWebhookSignature, st)
	}
	key, err := parseRSAPublicKey(publicKeyPEM)
	if err != nil {
		return fmt.Errorf("%w: invalid public key", ErrWebhookSignature)
	}
	decoded, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return fmt.Errorf("%w: sign is not base64", ErrWebhookSignature)
	}
	digest := sha256.Sum256([]byte(canonicalAlipayString(params)))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], decoded); err != nil {
		return fmt.Errorf("%w: signature mismatch", ErrWebhookSignature)
	}
	return nil
}

// verifyRSA2Raw 验证 JSON 响应节点：签名对象是节点的原始字节。
func verifyRSA2Raw(raw json.RawMessage, sign, publicKeyPEM string) error {
	key, err := parseRSAPublicKey(publicKeyPEM)
	if err != nil {
		return ErrWebhookSignature
	}
	decoded, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return ErrWebhookSignature
	}
	digest := sha256.Sum256(raw)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], decoded)
}

// canonicalAlipayString 生成签名串：去掉 sign/sign_type，按键名升序，
// 以 `k=v` 用 & 连接。空值参数同样参与（支付宝规范如此，不能省略）。
func canonicalAlipayString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		if key == "sign" || key == "sign_type" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, key := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(params[key])
	}
	return b.String()
}

// parseRSAPrivateKey 接受 PKCS#1 与 PKCS#8 两种 PEM（运营从支付宝工具
// 导出的是 PKCS#8；旧文档常见 PKCS#1）。
func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("billing: invalid private key PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("billing: invalid private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("billing: private key is not RSA")
	}
	return key, nil
}

// parseRSAPublicKey 接受 PKIX 与 PKCS#1 公钥 PEM。
func parseRSAPublicKey(pemText string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemText)))
	if block == nil {
		return nil, errors.New("billing: invalid public key PEM")
	}
	if parsed, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		key, ok := parsed.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("billing: public key is not RSA")
		}
		return key, nil
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("billing: invalid public key")
}

// centsToYuanString 把分格式化为两位小数的元字符串（整数运算，无浮点）。
func centsToYuanString(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// yuanToCents 把支付宝的元字符串（"10.00"）换算为分。小数位超过 2 或
// 非数字一律拒绝：回调金额是资金比对输入，不能让精度歧义通过。
func yuanToCents(value string) (int64, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		return 0, err
	}
	cents := d.Mul(decimal.NewFromInt(100))
	if !cents.IsInteger() {
		return 0, errors.New("billing: amount has more than 2 decimal places")
	}
	if cents.Sign() <= 0 {
		return 0, errors.New("billing: amount must be positive")
	}
	return cents.IntPart(), nil
}

// ---------------------------------------------------------------------------
// 统一入账（回调与后续轮询/人工核实共用）
// ---------------------------------------------------------------------------

func (s *TopupService) findOrderByProviderTxn(ctx context.Context, txnID string) (*store.Order, error) {
	var order store.Order
	err := s.db.WithContext(ctx).Where("provider_txn_id = ?", txnID).First(&order).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 回调早于订单落库/订单不存在：告警由 HTTP 层记录，这里只给语义错误。
		return nil, fmt.Errorf("%w: provider_txn_id %s", ErrOrderNotFound, txnID)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: lookup order by provider txn: %w", err)
	}
	return &order, nil
}

// settleOrder 是唯一的入账入口：比对金额/币种后条件 UPDATE + ApplyTx。
//
// 幂等语义：
//   - 订单已 succeeded：直接返回 nil，不重复入账；
//   - 其他终态（failed/expired/cancelled）：返回 ErrOrderStateConflict，
//     由 HTTP 层告警并回 2xx（重投不会让终态变得可入账）；
//   - pending：条件 UPDATE 抢到才入账，affected=0 时回查状态收口并发。
func (s *TopupService) settleOrder(ctx context.Context, order *store.Order,
	providerAmount int64, providerCurrency string, raw []byte) error {

	if order.Status == string(OrderSucceeded) {
		return nil
	}
	if order.Status != string(OrderPending) {
		s.log.Warn("payment callback for non-pending order",
			"order_id", order.ID, "provider", order.Provider, "status", order.Status)
		return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, order.Status)
	}

	// 金额/币种比对在入账之前完成；任何不一致都不写余额、不写流水。
	if providerCurrency != "" && providerCurrency != order.Currency {
		return s.failMismatch(ctx, order, "currency_mismatch", raw, map[string]any{
			"expected_currency": order.Currency,
			"got_currency":      providerCurrency,
		}, ErrCurrencyMismatch)
	}
	if providerAmount != order.Amount {
		return s.failMismatch(ctx, order, "amount_mismatch", raw, map[string]any{
			"expected_amount": order.Amount,
			"got_amount":      providerAmount,
		}, ErrAmountMismatch)
	}

	quotaPerUnit, err := s.quotaPerUnit()
	if err != nil {
		return err
	}
	ratios, err := s.topupGroupRatios()
	if err != nil {
		return err
	}
	group, err := s.userGroup(ctx, order.UserID)
	if err != nil {
		return err
	}
	ratio, err := GroupRatio(ratios, group)
	if err != nil {
		return err
	}
	credits, err := ComputeCredits(order.Amount, quotaPerUnit, ratio)
	if err != nil {
		return err
	}
	if credits <= 0 {
		// 极小倍率把到账积分抹平：写入 0 积分流水违反 DB CHECK，且用户会
		// 看到「支付成功但没到账」。整体回滚，订单保持 pending 等人工处理。
		return fmt.Errorf("%w: credits floor to zero for order %d", ErrInvalidFundingCredit, order.ID)
	}

	credited := false
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// P-15：条件 UPDATE 与入账同事务。否则「订单成功、入账失败」会
		// 留下永远无法重试的成功订单（重投时 status != pending）。
		res := tx.Exec(`
			UPDATE orders
			SET status = 'succeeded', paid_at = now(), raw_callback = ?,
			    credited_credits = ?, provider_amount = ?, provider_currency = ?,
			    currency_scale = 2, updated_at = now()
			WHERE id = ? AND status = 'pending'`,
			raw, credits, providerAmount, providerCurrency, order.ID)
		if res.Error != nil {
			return fmt.Errorf("billing: settle order %d: %w", order.ID, res.Error)
		}
		if res.RowsAffected == 0 {
			// 并发回调/轮询已先行迁移：同一事务内回查当前状态。
			var status string
			if err := tx.Raw(`SELECT status FROM orders WHERE id = ?`, order.ID).
				Row().Scan(&status); err != nil {
				return fmt.Errorf("billing: re-read order %d: %w", order.ID, err)
			}
			if status == string(OrderSucceeded) {
				return nil
			}
			return fmt.Errorf("%w: order %d is %s", ErrOrderStateConflict, order.ID, status)
		}
		if _, _, err := s.funding.ApplyTx(ctx, tx, Credit{
			UserID:        order.UserID,
			Amount:        credits,
			Type:          CreditTypeTopup,
			ReferenceType: "order",
			ReferenceID:   strconv.FormatInt(order.ID, 10),
			Note:          "online topup " + order.Provider,
		}); err != nil {
			return err
		}
		credited = true
		return nil
	})
	if err != nil {
		return err
	}
	if credited {
		s.log.Info("payment credited",
			"order_id", order.ID, "provider", order.Provider, "credits", credits,
			"amount", order.Amount, "currency", order.Currency)
		order.Status = string(OrderSucceeded)
		order.CreditedCredits = credits
	}
	return nil
}

// failMismatch 把订单置 failed 并写受控失败码；绝不入账。
//
// 条件 UPDATE 保证不会把并发已成功的订单降级；若回查发现已 succeeded，
// 视为「成功路径先到」并返回 nil（金额比对失败但钱已按订单金额入账，
// 属于需要人工核对的异常，告警已经由调用方日志覆盖）。
func (s *TopupService) failMismatch(ctx context.Context, order *store.Order,
	failureCode string, raw []byte, detail map[string]any, mismatchErr error) error {

	// 结构化告警：订单号、渠道、期望/实收。绝不记录签名头或卡信息。
	args := []any{"order_id", order.ID, "provider", order.Provider, "failure_code", failureCode}
	for key, value := range detail {
		args = append(args, key, value)
	}
	s.log.Error("payment callback rejected: order/amount mismatch", args...)

	res := s.db.WithContext(ctx).Exec(
		`UPDATE orders SET status = 'failed', failure_code = ?, raw_callback = ?, updated_at = now()
		 WHERE id = ? AND status = 'pending'`, failureCode, raw, order.ID)
	if res.Error != nil {
		return fmt.Errorf("billing: mark order %d failed: %w", order.ID, res.Error)
	}
	if res.RowsAffected > 0 {
		order.Status = string(OrderFailed)
		return mismatchErr
	}
	var status string
	if err := s.db.WithContext(ctx).Raw(`SELECT status FROM orders WHERE id = ?`, order.ID).
		Row().Scan(&status); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("billing: re-read order %d: %w", order.ID, err)
	}
	if status == string(OrderSucceeded) {
		return nil
	}
	return mismatchErr
}

func (s *TopupService) topupGroupRatios() (map[string]decimal.Decimal, error) {
	if s.cfg == nil {
		return map[string]decimal.Decimal{}, nil
	}
	raw, ok := s.cfg.Get(ConfigTopupGroupRatio)
	if !ok {
		return map[string]decimal.Decimal{}, nil
	}
	return ParseTopupGroupRatios(raw)
}

func (s *TopupService) userGroup(ctx context.Context, userID int64) (string, error) {
	var group string
	err := s.db.WithContext(ctx).Raw(`SELECT "group" FROM users WHERE id = ?`, userID).
		Row().Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: user %d", ErrUserNotFound, userID)
	}
	if err != nil {
		return "", fmt.Errorf("billing: read user group for %d: %w", userID, err)
	}
	return group, nil
}

// marshalWhitelisted 把白名单字段序列化为 raw_callback。
//
// security-06 §2.1 / RC-F-02：只保留对账必需字段，签名头、卡信息一律不落库。
// 序列化失败返回 nil（jsonb NULL），绝不让审计字段阻断资金路径。
func marshalWhitelisted(fields map[string]any) []byte {
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return raw
}
