package notify

// WebhookSender 是 F-08-20 的出站 Webhook 发送器（配置键 notify.webhook.*）。
//
// 设计权威：modules-08 F-08-20 / §3.9.6，签名语义逐字遵循 modules-03
// F-03-10「投递」第 9 条：
//
//   - 头：X-NewTG-Timestamp（Unix 秒）+ X-NewTG-Signature: sha256=<hex>；
//   - 原文：HMAC-SHA256(secret, timestamp + "." + 请求体)；
//   - **签名覆盖实际发送的字节**。因此本实现先把 payload 固定为 []byte，
//     签名该切片，再用同一个切片构造请求体——签名之后不做任何重排、
//     重新序列化、字符集转换或压缩；
//   - 超时 10s；3xx 不跟随（重定向会把已签名请求带到未校验目标）；
//   - 响应体最多读 4KiB 且**永不进入错误文本**；错误只带主机名，绝不带
//     完整 URL（url.Error 会拼上 query，可能含端点方 token）。
//
// payload 的结构归模块 01/03/04（本包只接受不透明字节），event 只用于
// 订阅过滤，eventID 供 N8 投递记录关联与排障——线上头契约只冻结了
// timestamp/signature 两个头，这里不额外发明请求头。

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
)

// 配置键（与 settings 注册表逐字一致）。
const (
	KeyWebhookEnabled = "notify.webhook.enabled"
	KeyWebhookURL     = "notify.webhook.url"
	KeyWebhookSecret  = "notify.webhook.secret"
	KeyWebhookEvents  = "notify.webhook.events"
)

// 线协议常量（F-03-10 第 9 条冻结）。
const (
	WebhookTimestampHeader = "X-NewTG-Timestamp"
	WebhookSignatureHeader = "X-NewTG-Signature"
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
	// Client 是测试缝隙；nil 时使用内置客户端（直连、禁用代理、拨号前
	// 再次校验目标 IP）。注入的客户端仍会被强制加上「不跟随 3xx」策略。
	Client *http.Client
	// Resolve 是 SSRF 守卫的 DNS 缝隙；nil 时用系统解析器。
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	// Now 是时间戳缝隙（签名固定向量测试）。
	Now func() time.Time
	// AllowPrivateTargets 仅供测试装配：httptest 监听 127.0.0.1，生产必须
	// 保持 false。打开后同时放开 https 与私网限制——绝不可由设置项驱动。
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
// eventID 不参与签名、也不额外上头：它属于投递记录（N8）的幂等锚，
// 线上协议只冻结 timestamp/signature，见文件头说明。
func (s *WebhookSender) Send(ctx context.Context, payload []byte, event, eventID string) (WebhookResult, error) {
	_ = eventID
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
	return s.deliver(ctx, settings, payload)
}

// SendTest 是测试端点专用入口：**忽略 enabled 与事件订阅**（测试的目的
// 就是验证未启用的配置；也不写投递记录）。URL/secret 未配置仍拒绝。
func (s *WebhookSender) SendTest(ctx context.Context, payload []byte) (WebhookResult, error) {
	settings, err := s.settings()
	if err != nil {
		return WebhookResult{}, err
	}
	if settings.URL == "" || settings.Secret == "" {
		return WebhookResult{}, ErrWebhookNotConfigured
	}
	return s.deliver(ctx, settings, payload)
}

func (s *WebhookSender) deliver(ctx context.Context, settings webhookSettings, payload []byte) (WebhookResult, error) {
	target, err := url.Parse(settings.URL)
	if err != nil || target.Hostname() == "" {
		return WebhookResult{}, fmt.Errorf("%w: invalid url", ErrWebhookBlockedTarget)
	}
	if !s.AllowPrivateTargets {
		// D2-11 只实现了 https：F-08-20 的「本地调试 http」例外被刻意收紧，
		// 避免签名密钥与业务 payload 在明文链路上传输。
		if target.Scheme != "https" {
			return WebhookResult{}, fmt.Errorf("%w: https is required", ErrWebhookBlockedTarget)
		}
		if target.User != nil || target.Fragment != "" {
			return WebhookResult{}, fmt.Errorf("%w: userinfo or fragment in url", ErrWebhookBlockedTarget)
		}
		if err := s.guardHost(ctx, target.Hostname()); err != nil {
			return WebhookResult{}, err
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

	resp, err := s.client().Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return WebhookResult{}, fmt.Errorf("%w: timeout", ErrWebhookTransport)
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
	if s.Client != nil {
		// 注入的客户端按值复制后强制策略；不缓存，测试可在多次投递间替换。
		client := *s.Client
		if client.Timeout <= 0 {
			client.Timeout = timeout
		}
		client.CheckRedirect = noWebhookRedirect
		return &client
	}
	s.clientOnce.Do(func() {
		dialer := &net.Dialer{Timeout: timeout}
		s.baseClient = &http.Client{
			Timeout:       timeout,
			CheckRedirect: noWebhookRedirect,
			Transport: &http.Transport{
				// 不使用 HTTP(S)_PROXY：代理会让拨号目标是代理而不是 webhook 主机，
				// 守卫校验的对象与实际连接对象分离（SSRF 旁路）。
				Proxy:                 nil,
				DialContext:           s.guardedDial(dialer),
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

// noWebhookRedirect 让 3xx 以响应返回而不是被跟随（D2-12）。
func noWebhookRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// guardedDial 在真正拨号前再解析并校验一次目标 IP。仅做发送前检查会留下
// DNS rebinding 窗口（校验时解析到公网、拨号时解析到 127.0.0.1）。
func (s *WebhookSender) guardedDial(dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid address", ErrWebhookBlockedTarget)
		}
		if err := s.guardHost(ctx, host); err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, network, address)
	}
}

// guardHost 是最小 SSRF 守卫（D2-12/D2-16：不实现完整 sec.ssrf.* 策略）：
// 拒绝 loopback、私网、link-local、未指定、多播与**无法解析**的主机名。
// 任一解析结果落入禁区即整体拒绝（多 A 记录部分私网同样危险）。
func (s *WebhookSender) guardHost(ctx context.Context, host string) error {
	if s.AllowPrivateTargets {
		return nil
	}
	if host == "" {
		return fmt.Errorf("%w: empty host", ErrWebhookBlockedTarget)
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("%w: private address", ErrWebhookBlockedTarget)
		}
		return nil
	}
	ips, err := s.lookupIP(ctx, host)
	if err != nil || len(ips) == 0 {
		// 未解析 host 一律拒绝：既防 DNS rebinding，也避免把解析失败伪装成
		// 普通连接错误（那会让管理员误以为是端点挂了）。
		return fmt.Errorf("%w: host does not resolve", ErrWebhookBlockedTarget)
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			return fmt.Errorf("%w: private address", ErrWebhookBlockedTarget)
		}
	}
	return nil
}

func (s *WebhookSender) lookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if s.Resolve != nil {
		return s.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast()
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
