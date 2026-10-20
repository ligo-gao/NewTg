package billing

// Epay（易支付）渠道适配器（modules-04 F-04-21；本批 C1）。
//
// 协议要点（与支付宝/Stripe 的差异）：
//   - 「创建支付」是**本地表单构造**，不是渠道 API 调用：把 pid/金额/单号/
//     回调地址按 submit.php 的表单字段拼好并签名，前端把表单 POST 到
//     收银台即可。因此本渠道没有网络失败面，也没有 client_secret/二维码。
//   - 签名是 MD5，不是 RSA：按键名 ASCII 升序拼接 `a=b&...` 后**直接**追加
//     密钥再 md5（小写），密钥前没有 `&`（标准易支付的 `$signstr.$key`）。
//     空值不参与（渠道侧实现普遍如此），`sign` 与 `sign_type` 都不参与——
//     `sign_type` 是渠道自行追加的元数据，若把它算进签名会与标准易支付
//     网关互不兼容。少数网关的「&key」变体（`$signstr."&".$key`）由
//     `pay.epay.sign_mode=amp` 显式选择，默认仍是标准口径。
//   - 回调可能是 GET 也可能是 POST（表单编码），因此处理器使用 r.Form
//     （query 与 body 的合并视图），两者走同一条验签路径。
//   - 没有主动查询/关单能力（F-04-21 明确声明）；超时关单只置 expired。
//     因此主动核实/人工完成的渠道查询返回 ErrProviderActionUnsupported，
//     由 HTTP 层映射为「该动作不支持」而不是 502（那不是渠道故障）。
//
// 金额口径：订单 amount 是分，渠道 money 是**元字符串（两位小数）**；
// 回调换算用 yuanToCents（小数位 >2 拒绝，不四舍五入），再交给
// settleOrder 做与其它渠道完全一致的金额/币种比对与幂等入账。

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"newtg/gateway/internal/store"
)

// Epay 配置键（键名见 docs/system-config-mapping.md §4.7；`pay.epay.key`
// 为密钥类，落库密封、响应掩码）。
const (
	ConfigEpayEnabled   = "pay.epay.enabled"
	ConfigEpayAPIURL    = "pay.epay.api_url"
	ConfigEpayPID       = "pay.epay.pid"
	ConfigEpayKey       = "pay.epay.key"
	ConfigEpayNotifyURL = "pay.epay.notify_url"
	ConfigEpayReturnURL = "pay.epay.return_url"
	// ConfigEpaySignMode 选择签名拼接口径；默认（空值）等价 append。
	ConfigEpaySignMode = "pay.epay.sign_mode"

	// ConfigSiteServerAddress 是公开基址（不带末尾斜杠），用于推导
	// notify_url / return_url 的默认值。缺键时回退本地默认。
	ConfigSiteServerAddress = "site.server_address"
)

// Epay 签名拼接口径。标准易支付是 canonical 直接接 key；amp 变体在两者
// 之间多一个 `&`。两种口径无法用同一 key 同时兼容，因此必须显式选择，
// 默认保持标准算法（不猜测网关实现）。
const (
	EpaySignModeAppend = "append"
	EpaySignModeAmp    = "amp"
)

// ErrProviderActionUnsupported 表示渠道本身不具备该动作的能力（不是瞬时
// 故障）。epay 无主动查询接口，主动核实/人工完成必须返回它，HTTP 层据此
// 回 409/400 固定文案，而不是把它当成 502「渠道不可用」。
var ErrProviderActionUnsupported = errors.New("billing: payment provider does not support this action")

// ProviderEpay 是易支付渠道标识（orders.provider 取值域已含 epay）。
const ProviderEpay = "epay"

// DefaultSiteServerAddress 与 settings 注册表的 site.server_address 默认值
// 一致：只用于「运营未配置回调地址」时的本地默认，生产必须显式配置。
const DefaultSiteServerAddress = "http://localhost:3000"

// epayType 是提交给收银台的支付类型（字段名 `type`）。易支付用 `type`
// 区分微信/支付宝；本渠道定位为「微信易支付」，固定 wxpay。若未来需要让
// 用户在收银台自选，再把它升级为配置项（当前任务未要求）。
const epayType = "wxpay"

// epaySubmitPath 是易支付标准下单入口。
const epaySubmitPath = "/submit.php"

// epaySubmitURL 把配置的 api_url 归一为 submit.php 地址。
//
// 运营粘贴的可能是站点根（https://pay.example.com/）也可能是完整入口
// （.../submit.php）；两种都接受，重复拼接 submit.php 是最常见的接入错误。
func epaySubmitURL(apiURL string) string {
	base := strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if strings.HasSuffix(base, epaySubmitPath) {
		return base
	}
	return base + epaySubmitPath
}

// epayBaseURL 返回站点公开基址（去末尾斜杠）。
func (s *TopupService) epayBaseURL() string {
	base := s.configString(ConfigSiteServerAddress)
	if base == "" {
		base = DefaultSiteServerAddress
	}
	return strings.TrimRight(base, "/")
}

// epayNotifyURL 返回回调地址：显式配置优先，否则由站点基址推导。
func (s *TopupService) epayNotifyURL() string {
	if configured := s.configString(ConfigEpayNotifyURL); configured != "" {
		return configured
	}
	return s.epayBaseURL() + "/api/webhooks/epay"
}

// epayReturnURL 返回支付完成后的前端落地页。配置值既可是绝对 URL，也可是
// 站内路径；后者拼上站点基址，因为渠道只会跳转绝对地址。
func (s *TopupService) epayReturnURL() string {
	configured := s.configString(ConfigEpayReturnURL)
	if configured == "" {
		return s.epayBaseURL() + "/wallet?pay=epay"
	}
	if strings.HasPrefix(configured, "/") {
		return s.epayBaseURL() + configured
	}
	return configured
}

// epaySignMode 归一化签名拼接口径。空值（未配置）按 append 处理：这是
// 标准易支付算法，也是升级前所有已部署实例的行为，不能因新增键而改变。
// 非空且未知的取值属于配置错误——静默回退会让运营以为 amp 已生效，
// 实际全部回调验签失败，因此返回 ErrPaymentConfig。
func (s *TopupService) epaySignMode() (string, error) {
	switch mode := s.configString(ConfigEpaySignMode); mode {
	case "", EpaySignModeAppend:
		return EpaySignModeAppend, nil
	case EpaySignModeAmp:
		return EpaySignModeAmp, nil
	default:
		return "", fmt.Errorf("%w: %s=%q", ErrPaymentConfig, ConfigEpaySignMode, mode)
	}
}

// epayEmptyQuery 判断 raw 是否带「空查询标记」。
//
// url.Parse("https://pay.example.com/?") 得到 ForceQuery=true 而 RawQuery=""，
// 只检查 RawQuery 会把这种地址当成合法基址/回调地址放行；拼接 submit.php
// 后它变成 "submit.php?"，渠道侧对空 `?` 的接受度不一致。
func epayEmptyQuery(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.ForceQuery
}

// epayCanonical 生成签名串：排除 sign/sign_type 与空值，按键名升序
// `k=v` 以 `&` 连接。返回值不含密钥。
func epayCanonical(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key, value := range params {
		if key == "sign" || key == "sign_type" || value == "" {
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

// epaySign 计算 MD5 签名（小写）。
//
// mode 为空等价 append。append 是标准易支付 `$signstr.$key`（密钥直接
// 追加，中间没有 `&`）；amp 是部分网关的 `$signstr."&".$key` 变体。下单与
// 回调必须使用同一口径，否则自己签出的表单/接受的回调会互相矛盾。
func epaySign(params map[string]string, key, mode string) string {
	canonical := epayCanonical(params)
	if mode == EpaySignModeAmp {
		canonical += "&" + key
	} else {
		canonical += key
	}
	sum := md5.Sum([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// VerifyEpaySignature 以常量时间比较回调签名。
//
// 先算期望值再做 subtle.ConstantTimeCompare：直接字符串比较会把签名
// 前缀匹配长度泄漏给攻击者（理论上可逐字节猜签名）。签名长度不等时
// ConstantTimeCompare 立即返回 0，不构成可利用的信号（MD5 定长 32）。
func VerifyEpaySignature(params map[string]string, key, mode string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("%w: epay key not configured", ErrWebhookSignature)
	}
	got := strings.ToLower(strings.TrimSpace(params["sign"]))
	if got == "" {
		return fmt.Errorf("%w: missing sign", ErrWebhookSignature)
	}
	expected := epaySign(params, key, mode)
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return fmt.Errorf("%w: signature mismatch", ErrWebhookSignature)
	}
	return nil
}

// createEpayPayment 构造收银台表单（F-04-21「创建支付」）。
//
// provider_txn_id 必须在返回前落库：回调只携带 out_trade_no，用户可能在
// 前端收到响应前就完成支付。它同时是回调防重的唯一锚（唯一约束）。
func (s *TopupService) createEpayPayment(ctx context.Context, order *store.Order) (*TopupOrder, error) {
	apiURL := s.configString(ConfigEpayAPIURL)
	// configString 已去首尾空白；这里再 TrimSpace 是显式归一化，保证「配置里
	// pid 带尾随空格」不会让下单表单与回调校验使用两个不同值。
	pid := strings.TrimSpace(s.configString(ConfigEpayPID))
	key, err := s.configSecret(ConfigEpayKey)
	if err != nil {
		return nil, err
	}
	signMode, err := s.epaySignMode()
	if err != nil {
		return nil, err
	}
	if apiURL == "" || pid == "" || key == "" {
		return nil, fmt.Errorf("%w: epay api_url/pid/key missing", ErrPaymentConfig)
	}
	outTradeNo := fmt.Sprintf("TGEP%d", order.ID)
	res := s.db.WithContext(ctx).Exec(
		`UPDATE orders SET provider_txn_id = ?, updated_at = now()
		 WHERE id = ? AND status = 'pending'`, outTradeNo, order.ID)
	if res.Error != nil {
		return nil, fmt.Errorf("billing: store epay out_trade_no: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// 订单在创建渠道单期间并发进入终态（或已被删除）：不能再把表单交给
		// 用户，否则回调会命中一个无法入账的单号。markOrderFailed 只降级
		// pending，不会覆盖并发已成功的订单。
		s.markOrderFailed(ctx, order.ID, "order_state_conflict")
		return nil, fmt.Errorf("%w: order %d is no longer pending", ErrOrderStateConflict, order.ID)
	}
	order.ProviderTxnID = &outTradeNo

	// 字段口径（标准易支付 submit.php）：`type` 是支付类型（不是 `type_out`）；
	// `sign_type=MD5` 与 `device=pc` 是渠道识别的元数据，其中 sign_type 不参与
	// 签名（epayCanonical 排除），device 参与。
	fields := map[string]string{
		"pid":          pid,
		"type":         epayType,
		"out_trade_no": outTradeNo,
		"notify_url":   s.epayNotifyURL(),
		"return_url":   s.epayReturnURL(),
		"name":         fmt.Sprintf("NewTG order %d", order.ID),
		"money":        centsToYuanString(order.Amount),
		"param":        "",
		"sign_type":    "MD5",
		"device":       "pc",
	}
	fields["sign"] = epaySign(fields, key, signMode)
	return &TopupOrder{Order: order, PayURL: epaySubmitURL(apiURL), FormFields: fields}, nil
}

// HandleEpayNotify 验签并处理易支付异步通知（GET/POST 表单或 query）。
//
// 与 HandleAlipayNotify 的差异：
//   - 渠道未启用/配置不完整一律拒绝（HTTP 503），避免把回调当作入账旁路；
//   - 只认 `TRADE_SUCCESS`；中间态直接 nil（HTTP 层回 success 停止重投）；
//   - 订单不存在与非法签名同样表现为失败文本，不暴露订单号是否存在。
func (s *TopupService) HandleEpayNotify(ctx context.Context, params url.Values) error {
	// providerReady 同时校验 enabled 与 api_url/pid/key 的完整性；
	// 未启用时返回 ErrProviderDisabled（HTTP 503），不进入验签。
	if _, err := s.providerReady(ProviderEpay); err != nil {
		return err
	}
	key, err := s.configSecret(ConfigEpayKey)
	if err != nil {
		return err
	}
	signMode, err := s.epaySignMode()
	if err != nil {
		return err
	}
	flat := make(map[string]string, len(params))
	for name, values := range params {
		if len(values) > 0 {
			flat[name] = values[len(values)-1]
		}
	}
	if err := VerifyEpaySignature(flat, key, signMode); err != nil {
		return err
	}
	// 次级校验：同一密钥可能被多个站点共用（渠道后台按 pid 隔离），
	// pid 必须与本站配置**完全一致**；缺失 pid 也拒绝——否则共用密钥的
	// 另一站点带签名的回调会被本站入账。两侧都 TrimSpace 归一：配置侧
	// 的尾随空格是常见的复制粘贴错误，不能让它把所有合法回调判成不符。
	configuredPID := strings.TrimSpace(s.configString(ConfigEpayPID))
	if configuredPID == "" {
		return fmt.Errorf("%w: epay pid not configured", ErrPaymentConfig)
	}
	if got := strings.TrimSpace(flat["pid"]); got != configuredPID {
		return fmt.Errorf("%w: pid mismatch", ErrWebhookPayload)
	}
	if status := flat["trade_status"]; status != "TRADE_SUCCESS" {
		return nil
	}
	outTradeNo := strings.TrimSpace(flat["out_trade_no"])
	if outTradeNo == "" {
		return fmt.Errorf("%w: missing out_trade_no", ErrWebhookPayload)
	}
	order, err := s.findOrderByProviderTxn(ctx, outTradeNo)
	if err != nil {
		return err
	}
	if order.Provider != ProviderEpay {
		// 渠道密钥隔离（security-06 §2.10）：用 epay 回调去结算其它渠道
		// 订单属异常，告警但不入账。
		return fmt.Errorf("%w: txn %s belongs to provider %s",
			ErrOrderStateConflict, outTradeNo, order.Provider)
	}
	cents, err := yuanToCents(flat["money"])
	if err != nil {
		return fmt.Errorf("%w: money", ErrWebhookPayload)
	}
	// raw_callback 是 jsonb 列，不能存原始表单文本；保存对账必需字段的
	// 白名单 JSON 对象（签名与密钥一律不落库）。
	raw := marshalWhitelisted(map[string]any{
		"provider":     ProviderEpay,
		"pid":          flat["pid"],
		"out_trade_no": outTradeNo,
		"trade_no":     flat["trade_no"],
		"trade_status": flat["trade_status"],
		"type":         flat["type"],
		"money":        flat["money"],
	})
	return s.settleOrder(ctx, order, cents, AlipayCurrency, raw)
}
