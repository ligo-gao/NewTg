package notify

// WebhookSender 是 F-08-20 的出站 Webhook 发送器（配置键 notify.webhook.*）。
//
// 设计权威：modules-08 F-08-20 / §3.9.6，签名语义逐字遵循 modules-03
// F-03-10「投递」第 9 条：
//
//   - 头：X-NewTG-Timestamp（Unix 秒）+ X-NewTG-Signature: sha256=<hex>
//     + X-NewTG-Event-Id（投递事件的幂等锚，见下）；
//   - 原文：HMAC-SHA256(secret, timestamp + "." + 请求体)；
//   - **签名覆盖实际发送的字节**。因此本实现先把 payload 固定为 []byte，
//     签名该切片，再用同一个切片构造请求体——签名之后不做任何重排、
//     重新序列化、字符集转换或压缩；
//   - 超时 10s；3xx 不跟随（重定向会把已签名请求带到未校验目标）；
//   - 出站统一走 internal/egress（sec.ssrf.* 策略）：只解析一次 → 校验全部
//     候选 IP → 拨已校验 IP（反 rebinding）；https 强制为 Webhook 额外约束；
//   - 响应体最多读 4KiB 且**永不进入错误文本**；错误只带主机名，绝不带
//     完整 URL（url.Error 会拼上 query，可能含端点方 token）。
//
// 【实现扩展】F-03-10 第 9 条原文冻结的是 timestamp/signature 两个头
// （「B1a 两头冻结口径」）。N8 投递需要把事件幂等锚传给接收方，否则接收方
// 无法在重试之间去重，因此这里增加第三头 X-NewTG-Event-Id（值 = 投递行的
// dedupe_key，缺失时退化为 delivery:<id>）。**签名仍只覆盖
// timestamp + "." + 请求体**，Event-Id 不参与签名：它只用于去重与排障，
// 篡改它不会绕过完整性校验，接收方也不得以它为签名输入。该扩展需回写
// modules-03 F-03-10 #9（见台账 G-DOM-55）。
//
// payload 的结构归模块 01/03/04（本包只接受不透明字节），event 只用于
// 订阅过滤；SendTest 没有业务投递行，因此不发 Event-Id 头（空值省略）。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"newtg/gateway/internal/egress"
)

// 配置键（与 settings 注册表逐字一致）。
const (
	KeyWebhookEnabled = "notify.webhook.enabled"
	KeyWebhookURL     = "notify.webhook.url"
	KeyWebhookSecret  = "notify.webhook.secret"
	KeyWebhookEvents  = "notify.webhook.events"
)

// 线协议常量（F-03-10 第 9 条；Event-Id 为 B1a 实现扩展，见文件头）。
const (
	WebhookTimestampHeader = "X-NewTG-Timestamp"
	WebhookSignatureHeader = "X-NewTG-Signature"
	// WebhookEventIDHeader 携带投递事件的幂等锚（dedupe_key）。它**不参与
	// 签名**，接收方只用于重试去重；值来自生产者，不保证是 UUID。
	WebhookEventIDHeader = "X-NewTG-Event-Id"
	// WebhookDefaultTimeout 是 D2-12 冻结的单次投递超时。
	WebhookDefaultTimeout = 10 * time.Second
	// webhookMaxResponseBytes 是响应体读取上限：只用于读完连接以便复用，
	// 内容本身不参与任何判定与日志。
	webhookMaxResponseBytes = 4 << 10
)

var (
	// ErrWebhookNotConfigured：URL 或 secret 缺失（HTTP 422）。
	ErrWebhookNotConfigured = errors.New("notify: webhook is not configured")
	// ErrWebhookDisabled：开关关闭。生产者据此静默跳过，不是投递失败。
	ErrWebhookDisabled = errors.New("notify: webhook is disabled")
	// ErrEventNotSubscribed：事件不在 notify.webhook.events 内。
	ErrEventNotSubscribed = errors.New("notify: webhook event is not subscribed")
	// ErrWebhookBlockedTarget：最小 SSRF 守卫拒绝目标（私网/loopback/
	// link-local/未解析主机）或 URL 不满足 https 约束。发送前即返回，
	// 不发起任何连接。
	ErrWebhookBlockedTarget = errors.New("notify: webhook target is not allowed")
	// ErrWebhookTransport：连接失败/超时。刻意不包装底层错误——url.Error
	// 的 Error() 会带完整 URL（含 query），是最容易泄露端点凭据的路径。
	ErrWebhookTransport = errors.New("notify: webhook request failed")
)

// WebhookResult 是一次已收到 HTTP 响应的投递结果。OK 只表示 2xx；
// 3xx/4xx/5xx 都算失败，但 StatusCode 原样保留给测试端点展示。
type WebhookResult struct {
	StatusCode int
	OK         bool
}

// WebhookSender 可并发使用。配置每次发送时从配置中心快照读取，
// 因此管理员保存后立即生效，不需要重启进程（与 SMTPSender 同模式）。
type WebhookSender struct {
	cfg    ConfigReader
	sealer SecretOpener

	// Timeout 是单次投递上限；零值用 WebhookDefaultTimeout。
	Timeout time.Duration
	// httpClient 是**仅测试用的接缝**：测试用它注入带 TLS 配置的客户端
	// （httptest）。生产保持 nil，由 client() 构造内置客户端：禁用代理，并在
	// DialContext 中调用统一执行器 egress.Guard.DialContext，完成「解析一次
	// → 校验全部 IP → 拨已校验 IP」。注入的客户端**不经过这条拨号路径**，
	// 地址级校验只剩 deliver 的形状校验（不做 DNS）——安全性弱于内置客户端，
	// 因此未导出，包外代码无法设置（F3；LOW-1 修正旧注释：guardedDial/
	// guardHost 已随 B2 收敛到 internal/egress 删除）。
	httpClient *http.Client
	// Resolve 是 SSRF 守卫的 DNS 缝隙；nil 时用系统解析器。
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// Now 是时间戳缝隙（签名固定向量测试）。
	Now func() time.Time
	// AllowPrivateTargets 仅供测试装配：httptest 只能监听 127.0.0.1 的
	// 随机端口。打开后执行器按 off 模式运行（不再应用 sec.ssrf.* 形状
	// 与地址校验），但**仍保留**「解析一次 → 拨已校验 IP」的拨号路径，
	// 以便反 rebinding 用例仍能观察到解析次数。生产必须保持 false，
	// 绝不可由设置项驱动。
	AllowPrivateTargets bool

	// clientOnce/baseClient 缓存内置客户端：每次投递新建 Transport 会留下
	// 一堆 30s 的 idle 连接，事件流量下等于持续泄漏连接池。
	clientOnce sync.Once
	baseClient *http.Client
}

// NewWebhookSender 构造发送器。cfg 为 nil 时 Configured 恒 false。
func NewWebhookSender(cfg ConfigReader, sealer SecretOpener) *WebhookSender {
	return &WebhookSender{cfg: cfg, sealer: sealer}
}

type webhookSettings struct {
	Enabled bool
	URL     string
	Secret  string
	Events  []string
}

// Configured 报告 URL 与 secret 是否齐备。刻意不看 enabled：测试端点的目的
// 就是验证「尚未启用」的配置（D2-14）。
func (s *WebhookSender) Configured(_ context.Context) bool {
	settings, err := s.settings()
	if err != nil {
		return false
	}
	return settings.URL != "" && settings.Secret != ""
}

// Active 报告 N8 worker 当前是否可以真实投递：开关开启且 URL/secret 齐备。
//
// 与 Configured 分开是刻意的：测试端点忽略 enabled，而 worker 必须受它
// 约束。返回 false 时 worker 不领取投递、也不消耗重试次数——业务事件在
// 管理员打开开关/补齐配置后按原样发出，而不是先被判失败。
func (s *WebhookSender) Active(_ context.Context) bool {
	settings, err := s.settings()
	if err != nil {
		return false
	}
	return settings.Enabled && settings.URL != "" && settings.Secret != ""
}

// TargetHost 返回已保存 URL 的主机名（不含 userinfo/path/query），供测试
// 端点的审计 detail 使用。任何解析失败都返回空串。
func (s *WebhookSender) TargetHost() string {
	settings, err := s.settings()
	if err != nil || settings.URL == "" {
		return ""
	}
	parsed, err := url.Parse(settings.URL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// Subscribed 报告事件是否在订阅列表内。配置损坏时返回 false（fail-closed）。
func (s *WebhookSender) Subscribed(event string) bool {
	settings, err := s.settings()
	if err != nil {
		return false
	}
	return containsEvent(settings.Events, event)
}

// Send 投递一个业务事件：先过开关与订阅过滤，再交给 deliver。
//
// eventID 是投递行的幂等锚（dedupe_key，缺失时由 worker 退化为
// delivery:<id>），作为 X-NewTG-Event-Id 发给接收方，**不参与签名**；
// 见文件头「实现扩展」。
func (s *WebhookSender) Send(ctx context.Context, payload []byte, event, eventID string) (WebhookResult, error) {
	settings, err := s.settings()
	if err != nil {
		return WebhookResult{}, err
	}
	if !settings.Enabled {
		return WebhookResult{}, ErrWebhookDisabled
	}
	if !containsEvent(settings.Events, event) {
		return WebhookResult{}, ErrEventNotSubscribed
	}
	return s.deliver(ctx, settings, payload, eventID)
}

// SendTest 是测试端点专用入口：**忽略 enabled 与事件订阅**（测试的目的
// 就是验证未启用的配置；也不写投递记录）。URL/secret 未配置仍拒绝。
// 测试事件没有投递行，因此不发 X-NewTG-Event-Id。
func (s *WebhookSender) SendTest(ctx context.Context, payload []byte) (WebhookResult, error) {
	settings, err := s.settings()
	if err != nil {
		return WebhookResult{}, err
	}
	if settings.URL == "" || settings.Secret == "" {
		return WebhookResult{}, ErrWebhookNotConfigured
	}
	return s.deliver(ctx, settings, payload, "")
}

func (s *WebhookSender) deliver(ctx context.Context, settings webhookSettings, payload []byte, eventID string) (WebhookResult, error) {
	guard := s.guard()
	// 形状校验（协议/端口/userinfo/域名）**不做 DNS**：解析只发生在拨号
	// 阶段一次，校验对象与连接对象因此一致（防 rebinding）。地址级拒绝
	// 由内置客户端 DialContext 中的 egress.Guard 完成并保留 ErrBlocked 分类。
	target, err := guard.ValidateURLShape(settings.URL)
	if err != nil {
		return WebhookResult{}, fmt.Errorf("%w: invalid url", ErrWebhookBlockedTarget)
	}
	if !s.AllowPrivateTargets {
		// D2-11 只实现了 https：F-08-20 的「本地调试 http」例外被刻意收紧，
		// 避免签名密钥与业务 payload 在明文链路上传输。
		if target.Scheme != "https" {
			return WebhookResult{}, fmt.Errorf("%w: https is required", ErrWebhookBlockedTarget)
		}
		if target.Fragment != "" {
			return WebhookResult{}, fmt.Errorf("%w: fragment in url", ErrWebhookBlockedTarget)
		}
	}

	// 时间戳每次投递重新生成（F-03-10）。Unix 秒是冻结精度。
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(settings.Secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	// 体直接来自 payload 切片：签名与传输共享同一份字节（字节保真）。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return WebhookResult{}, fmt.Errorf("%w: build request", ErrWebhookTransport)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set(WebhookTimestampHeader, timestamp)
	req.Header.Set(WebhookSignatureHeader, signature)
	// 空值省略：SendTest 没有投递行，不应伪造一个幂等锚。
	if eventID != "" {
		req.Header.Set(WebhookEventIDHeader, eventID)
	}

	resp, err := s.client().Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return WebhookResult{}, fmt.Errorf("%w: timeout", ErrWebhookTransport)
		}
		// 拨号阶段的守卫拒绝（解析结果在拨号时落入禁区、或主机不解析）
		// 必须保留分类：调用方据此返回 422 而不是把「目标被拒绝」误报成
		// 端点故障。
		//
		// 但**不得**原样返回 err：http.Client.Do 包的是 *url.Error，其 Error()
		// 会拼上完整 URL（含 query 里的端点 token），而拨号期拒绝正是攻击者/
		// 误配置最容易踩到的路径。重新包装成不含底层错误的哨兵（F1）。
		if errors.Is(err, egress.ErrBlocked) || errors.Is(err, egress.ErrUnresolved) {
			return WebhookResult{}, fmt.Errorf("%w: target rejected at connect", ErrWebhookBlockedTarget)
		}
		// 不包装底层错误：net/url.Error 含完整 URL。
		return WebhookResult{}, fmt.Errorf("%w: transport error", ErrWebhookTransport)
	}
	defer func() { _ = resp.Body.Close() }()
	// 读完（有上限）以便连接复用；响应内容不进入结果/错误/日志。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookMaxResponseBytes))

	return WebhookResult{
		StatusCode: resp.StatusCode,
		OK:         resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices,
	}, nil
}

func (s *WebhookSender) client() *http.Client {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = WebhookDefaultTimeout
	}
	if s.httpClient != nil {
		// 注入的客户端按值复制后强制策略；不缓存，测试可在多次投递间替换。
		client := *s.httpClient
		if client.Timeout <= 0 {
			client.Timeout = timeout
		}
		client.CheckRedirect = noWebhookRedirect
		return &client
	}
	s.clientOnce.Do(func() {
		s.baseClient = &http.Client{
			Timeout:       timeout,
			CheckRedirect: noWebhookRedirect,
			Transport: &http.Transport{
				// 不使用 HTTP(S)_PROXY：代理会让拨号目标是代理而不是 webhook 主机，
				// 守卫校验的对象与实际连接对象分离（SSRF 旁路）。
				Proxy: nil,
				// 每次拨号重新读配置构造执行器：管理员修改 sec.ssrf.* 后无需
				// 重建连接池即可生效。执行器在 DialContext 内完成「解析一次 →
				// 校验全部 IP → 拨已校验 IP」。
				DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					return s.guard().DialContext(ctx, network, address)
				},
				TLSHandshakeTimeout:   timeout,
				ResponseHeaderTimeout: timeout,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          4,
				IdleConnTimeout:       30 * time.Second,
			},
		}
	})
	return s.baseClient
}

// guard 构造本次出站使用的统一执行器。策略每次读取，配置保存后立即生效。
func (s *WebhookSender) guard() *egress.Guard {
	policy := egress.PolicyFromConfig(s.cfg)
	if s.AllowPrivateTargets {
		// 测试接缝：httptest 的 scheme/端口/loopback 都不满足生产策略。
		policy.Mode = egress.ModeOff
	}
	guard := egress.NewGuard(policy)
	if s.Resolve != nil {
		guard = guard.WithResolver(s.Resolve)
	}
	return guard
}

// noWebhookRedirect 让 3xx 以响应返回而不是被跟随（D2-12）。
func noWebhookRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (s *WebhookSender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func containsEvent(events []string, event string) bool {
	for _, item := range events {
		if item == event {
			return true
		}
	}
	return false
}

// settings 读取 notify.webhook 段。JSON 解码失败或密钥解封失败返回错误；
// 调用方因此按「未配置/不可用」处理（fail-closed）。
func (s *WebhookSender) settings() (webhookSettings, error) {
	settings := webhookSettings{Events: []string{"balance_low"}}
	if s.cfg == nil {
		return settings, nil
	}
	settings.Enabled = s.boolValue(KeyWebhookEnabled)
	settings.URL = s.stringValue(KeyWebhookURL)
	// 注册表默认 ["balance_low"]；事件列表损坏时返回错误而不是回退默认，
	// 否则一个被写坏的行会悄悄改变订阅面（fail-closed）。
	if raw, ok := s.raw(KeyWebhookEvents); ok {
		var events []string
		if err := json.Unmarshal(raw, &events); err != nil {
			return settings, errors.New("notify: webhook events is not a string array")
		}
		settings.Events = events
	}

	sealed := s.stringValue(KeyWebhookSecret)
	if sealed != "" {
		if s.sealer == nil {
			return settings, errors.New("notify: webhook secret is sealed but no SecretOpener is configured")
		}
		plain, err := s.sealer.Open(sealed)
		if err != nil {
			return settings, fmt.Errorf("notify: open webhook secret: %w", err)
		}
		settings.Secret = plain
	}
	return settings, nil
}

func (s *WebhookSender) raw(key string) (json.RawMessage, bool) {
	if s.cfg == nil {
		return nil, false
	}
	return s.cfg.Get(key)
}

func (s *WebhookSender) stringValue(key string) string {
	raw, ok := s.raw(key)
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func (s *WebhookSender) boolValue(key string) bool {
	raw, ok := s.raw(key)
	if !ok {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}
