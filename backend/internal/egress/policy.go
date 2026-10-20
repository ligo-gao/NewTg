// Package egress 是 NEW TG 的统一出站执行器（security-04 §2.4 / B2）。
//
// 它把「管理员可填 URL + 服务端主动出站」的三处调用面——通知 Webhook、
// 插件市场拉取、模型目录同步——收敛到同一套 SSRF 策略：
//
//   - 协议：仅 http/https；strict 模式仅 https；
//   - 端口：http 80 / https 443（strict 仅 443）；
//   - userinfo 一律拒绝（凭据会进日志与错误文本）；
//   - 域名：allowlist / denylist（支持精确与 *.example.com 通配）；
//   - 地址：basic 拒绝 loopback/私网/link-local/组播/未指定/CGNAT/保留段；
//     strict 额外拒绝元数据地址与 IPv6 本地地址；allow_private_ip 放行
//     RFC1918/ULA，但仍拒绝 loopback/link-local/元数据；
//   - 拨号：只解析一次、校验全部候选 IP，然后直接拨已校验的 IP（防 DNS
//     rebinding）；主机名只由 http.Transport 用于 TLS SNI/证书校验；
//   - 重定向：执行器默认不跟随；需要跟随的调用方使用 ClientOptions.
//     FollowRedirects=true，每一跳重新校验并受 max_redirects 限制。
//
// 策略来自 sec.ssrf.*（默认值以 settings 注册表为单一真源，本包不复制）。
// 未实现代理出口：net.proxy.scope 未接入（见 docs/未完成清单.md）。
package egress

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"newtg/gateway/internal/settings"
)

// 配置键（与 settings 注册表逐字一致）。
const (
	KeyMode                  = "sec.ssrf.mode"
	KeyEnabled               = "sec.ssrf.enabled"
	KeyDomainMode            = "sec.ssrf.domain_mode"
	KeyDomainList            = "sec.ssrf.domain_list"
	KeyAllowPrivateIP        = "sec.ssrf.allow_private_ip"
	KeyMaxRedirects          = "sec.ssrf.max_redirects"
	KeyConnectTimeoutSeconds = "sec.ssrf.connect_timeout_seconds"
)

// 错误分类。调用方按类别映射，绝不把底层 *url.Error 原样外传——它的
// Error() 会拼上完整 URL（含 query），可能泄露端点 token。
var (
	// ErrBlocked：策略拒绝目标（协议/端口/userinfo/域名/地址）。
	ErrBlocked = errors.New("egress: target blocked by SSRF policy")
	// ErrUnresolved：主机解析失败或没有返回地址。
	ErrUnresolved = errors.New("egress: host did not resolve")
	// ErrRedirect：重定向被拒绝（超限或下一跳未通过校验）。
	ErrRedirect = errors.New("egress: redirect rejected")
)

// Mode 是策略强度。
type Mode string

const (
	ModeOff    Mode = "off"
	ModeBasic  Mode = "basic"
	ModeStrict Mode = "strict"
)

// DomainMode 是域名清单的匹配方向。`all` 是既有注册表值，等价于不设
// 域名限制（向后兼容旧默认值）；新配置应使用 allowlist/denylist。
type DomainMode string

const (
	DomainAll       DomainMode = "all"
	DomainAllowlist DomainMode = "allowlist"
	DomainDenylist  DomainMode = "denylist"
)

// ConfigReader 与 configcenter.Center.Get 的方法集一致（避免反向依赖）。
type ConfigReader interface {
	Get(key string) (json.RawMessage, bool)
}

// Policy 是一次出站校验的完整策略。
type Policy struct {
	Mode           Mode
	DomainMode     DomainMode
	DomainList     []string
	AllowPrivateIP bool
	MaxRedirects   int
	ConnectTimeout time.Duration

	// AllowLoopback / AllowAnyPort 是**仅供测试装配**的接缝：
	// httptest 只能监听 127.0.0.1 的随机端口。生产路径 PolicyFromConfig
	// 永不设置它们；把任一字段暴露成设置键都会直接架空虚拟主机防护，
	// 因此刻意不做配置化。
	AllowLoopback bool
	AllowAnyPort  bool
}

// DefaultPolicy 返回与 settings 注册表一致的安全默认值。
//
// 默认值不在本包硬编码第二份：注册表是唯一真源（modules-08 §3.16 Q1）。
func DefaultPolicy() Policy {
	return Policy{
		Mode:           ModeBasic,
		DomainMode:     DomainAllowlist,
		DomainList:     defaultDomainList(),
		MaxRedirects:   3,
		ConnectTimeout: 10 * time.Second,
	}
}

func defaultDomainList() []string {
	value := settingDefault(KeyDomainList)
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func settingDefault(key string) any {
	if setting, ok := settings.Lookup(key); ok {
		return settings.DefaultValue(setting)
	}
	return nil
}

// PolicyFromConfig 从 sec.ssrf.* 读取策略。缺失/损坏的键回退到注册表
// 默认值；任何无法识别的枚举都按更严的方向解释（allowlist / basic）。
func PolicyFromConfig(cfg ConfigReader) Policy {
	policy := DefaultPolicy()
	if cfg == nil {
		return policy
	}

	if raw, ok := cfg.Get(KeyMode); ok {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			switch Mode(strings.ToLower(strings.TrimSpace(value))) {
			case ModeOff:
				policy.Mode = ModeOff
			case ModeBasic:
				policy.Mode = ModeBasic
			case ModeStrict:
				policy.Mode = ModeStrict
			}
		}
	} else if raw, ok := cfg.Get(KeyEnabled); ok {
		// 兼容旧布尔开关：只有显式 false 才等价 off；缺失/损坏保持 basic。
		var enabled bool
		if json.Unmarshal(raw, &enabled) == nil && !enabled {
			policy.Mode = ModeOff
		}
	}

	if raw, ok := cfg.Get(KeyDomainMode); ok {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			switch DomainMode(strings.ToLower(strings.TrimSpace(value))) {
			case DomainAll:
				policy.DomainMode = DomainAll
			case DomainAllowlist:
				policy.DomainMode = DomainAllowlist
			case DomainDenylist:
				policy.DomainMode = DomainDenylist
			}
		}
	}

	if raw, ok := cfg.Get(KeyDomainList); ok {
		var domains []string
		if json.Unmarshal(raw, &domains) == nil {
			policy.DomainList = domains
		} else if policy.DomainMode == DomainAllowlist {
			// 损坏的允许清单 fail-closed：空列表 = 不放行任何域名。
			policy.DomainList = nil
		}
	}

	if raw, ok := cfg.Get(KeyAllowPrivateIP); ok {
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			policy.AllowPrivateIP = value
		}
	}
	if raw, ok := cfg.Get(KeyMaxRedirects); ok {
		var value int
		if json.Unmarshal(raw, &value) == nil && value >= 0 && value <= 10 {
			policy.MaxRedirects = value
		}
	}
	if raw, ok := cfg.Get(KeyConnectTimeoutSeconds); ok {
		var value int
		if json.Unmarshal(raw, &value) == nil && value > 0 && value <= 120 {
			policy.ConnectTimeout = time.Duration(value) * time.Second
		}
	}
	return policy
}

// Guard 是策略的执行器，可并发使用。
type Guard struct {
	policy  Policy
	resolve func(context.Context, string) ([]net.IP, error)
	dialer  *net.Dialer
}

// NewGuard 构造执行器。零值字段按安全默认补齐。
func NewGuard(policy Policy) *Guard {
	if policy.Mode != ModeOff && policy.Mode != ModeBasic && policy.Mode != ModeStrict {
		policy.Mode = ModeBasic
	}
	if policy.DomainMode != DomainAll && policy.DomainMode != DomainAllowlist &&
		policy.DomainMode != DomainDenylist {
		// 程序化构造的零值不携带清单：按「不设域名限制」处理，地址规则
		// 仍然生效；配置路径永远显式给出 allowlist（默认）。
		policy.DomainMode = DomainAll
	}
	if policy.ConnectTimeout <= 0 {
		policy.ConnectTimeout = 10 * time.Second
	}
	p := policy
	p.DomainList = append([]string(nil), policy.DomainList...)
	return &Guard{policy: p, dialer: &net.Dialer{Timeout: p.ConnectTimeout}}
}

// WithResolver 返回使用自定义解析器的副本（测试接缝；nil 恢复系统解析器）。
func (g *Guard) WithResolver(resolve func(context.Context, string) ([]net.IP, error)) *Guard {
	clone := *g
	clone.resolve = resolve
	return &clone
}

// Policy 返回执行器持有的策略副本。
func (g *Guard) Policy() Policy { return g.policy }

// ValidateURLShape 只做无需 DNS 的形状校验：协议、端口、userinfo、域名。
// 拨号前使用它可以保住「整个请求只解析一次」的性质（解析在 DialContext
// 中完成，校验与连接使用同一个答案）。
func (g *Guard) ValidateURLShape(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		// 不回显解析错误：url.Parse 的文本会带上完整入参。
		return nil, fmt.Errorf("%w: invalid url", ErrBlocked)
	}
	if err := g.validateShape(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ValidateURL 做完整校验：形状 + 解析全部地址并逐个检查。调用方若随后
// 仍用 Guard.NewClient 出站，拨号会再次校验最终 IP（防 rebinding），这里
// 的解析只用于尽早给出 4xx/策略错误。
func (g *Guard) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	parsed, err := g.ValidateURLShape(raw)
	if err != nil {
		return nil, err
	}
	if g.policy.Mode == ModeOff {
		return parsed, nil
	}
	if err := g.validateHostIPs(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

func (g *Guard) validateShape(parsed *url.URL) error {
	if g.policy.Mode == ModeOff {
		return nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: unsupported scheme", ErrBlocked)
	}
	if g.policy.Mode == ModeStrict && scheme != "https" {
		return fmt.Errorf("%w: strict mode requires https", ErrBlocked)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: userinfo is not allowed", ErrBlocked)
	}
	if !g.policy.AllowAnyPort {
		port := parsed.Port()
		if port != "" {
			if scheme == "http" && port != "80" {
				return fmt.Errorf("%w: port not allowed", ErrBlocked)
			}
			if scheme == "https" && port != "443" {
				return fmt.Errorf("%w: port not allowed", ErrBlocked)
			}
		}
	}
	host := normalizeHost(parsed.Hostname())
	switch g.policy.DomainMode {
	case DomainAllowlist:
		if !g.domainListed(host) {
			return fmt.Errorf("%w: host is not in the allowlist", ErrBlocked)
		}
	case DomainDenylist:
		if g.domainListed(host) {
			return fmt.Errorf("%w: host is denied", ErrBlocked)
		}
	}
	return nil
}

func (g *Guard) domainListed(host string) bool {
	for _, pattern := range g.policy.DomainList {
		if domainMatches(pattern, host) {
			return true
		}
	}
	return false
}

// domainAllowsPrivateOverride 实现冻结语义：allow_private_ip=true 时，
// 只有**显式列入 allowlist** 的主机可以访问 loopback/link-local/元数据。
func (g *Guard) domainAllowsPrivateOverride(host string) bool {
	return g.policy.AllowPrivateIP &&
		g.policy.DomainMode == DomainAllowlist &&
		g.domainListed(normalizeHost(host))
}

func (g *Guard) validateHostIPs(ctx context.Context, host string) error {
	if g.domainAllowsPrivateOverride(host) {
		return nil
	}
	ips, err := g.resolveHost(ctx, host)
	if err != nil {
		return err
	}
	for _, ip := range ips {
		if g.ipBlocked(ip) {
			return fmt.Errorf("%w: address is not permitted", ErrBlocked)
		}
	}
	return nil
}

func (g *Guard) resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrBlocked)
	}
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	var ips []net.IP
	var err error
	if g.resolve != nil {
		ips, err = g.resolve(ctx, host)
	} else {
		ips, err = net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
	if err != nil || len(ips) == 0 {
		// 解析失败不当作普通连接错误：那会让管理员误判为对端故障。
		return nil, fmt.Errorf("%w: %s", ErrUnresolved, host)
	}
	return ips, nil
}

// DialContext 是反 rebinding 的核心：解析一次 → 校验全部候选 IP →
// 直接拨已校验的 IP。绝不把主机名交给 net.Dialer 再解析。
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid address", ErrBlocked)
	}
	ips, err := g.resolveHost(ctx, host)
	if err != nil {
		return nil, err
	}
	if g.policy.Mode != ModeOff && !g.domainAllowsPrivateOverride(host) {
		for _, ip := range ips {
			if g.ipBlocked(ip) {
				return nil, fmt.Errorf("%w: address is not permitted", ErrBlocked)
			}
		}
	}
	var lastErr error
	for _, ip := range ips {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, dialErr := g.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		return nil, fmt.Errorf("%w: no address to dial", ErrBlocked)
	}
	return nil, lastErr
}

// ClientOptions 是 NewClient 的调用方参数。
type ClientOptions struct {
	// Timeout 是整次请求（含重定向）的上限；<=0 时取 30s。
	Timeout time.Duration
	// FollowRedirects 为 true 时每一跳重新校验，上限 Policy.MaxRedirects。
	FollowRedirects bool
	// TLSConfig 仅供测试注入 httptest 的自签证书；生产保持 nil。
	TLSConfig *tls.Config
}

// NewClient 构造一个只经执行器出站的 http.Client：禁用代理（代理会让
// 拨号目标是代理而不是目标主机，策略校验对象与实际连接对象分离），
// 默认不跟随重定向。
func (g *Guard) NewClient(opts ClientOptions) *http.Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           g.DialContext,
		TLSHandshakeTimeout:   g.policy.ConnectTimeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	if opts.TLSConfig != nil {
		transport.TLSClientConfig = opts.TLSConfig
	}
	client := &http.Client{Transport: transport, Timeout: timeout}
	if opts.FollowRedirects {
		maxRedirects := g.policy.MaxRedirects
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrRedirect)
			}
			// 每一跳都重新走完整校验（含 DNS 地址检查）；拨号时会再次
			// 校验最终 IP，所以 CheckRedirect 的解析不构成 rebinding 窗口。
			if _, err := g.ValidateURL(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		}
	} else {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// domainMatches 支持精确主机与 `*.example.com`（匹配任意子域，不匹配裸域）。
func domainMatches(pattern, host string) bool {
	pattern = normalizeHost(pattern)
	host = normalizeHost(host)
	if pattern == "" || host == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".example.com"
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}
	return host == pattern
}

// ipBlocked 实现 §2 的地址分类。调用方已处理 allowlist 覆盖。
func (g *Guard) ipBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() {
		return !g.policy.AllowLoopback
	}
	// allow_private_ip 放行 RFC1918/ULA，但 loopback（上面）、link-local、
	// 元数据、CGNAT/保留段仍然拒绝。
	if g.policy.AllowPrivateIP {
		if isMetadataIP(ip) {
			return true
		}
		if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
			return true
		}
		return isReservedIP(ip)
	}
	if ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if isReservedIP(ip) {
		return true
	}
	if g.policy.Mode == ModeStrict {
		if isMetadataIP(ip) || isIPv6Local(ip) {
			return true
		}
	}
	return false
}

// isMetadataIP 是云元数据服务的已知地址。basic 模式下 link-local 的
// 169.254.0.0/16 已被拦截；strict 额外覆盖非 link-local 的元数据端点。
func isMetadataIP(ip net.IP) bool {
	metadata := []string{
		"169.254.169.254", // AWS / GCP / Azure / DigitalOcean IMDS
		"169.254.169.253", // AWS DNS
		"169.254.170.2",   // AWS ECS task metadata
		"100.100.100.200", // Alibaba Cloud
		"192.0.0.192",     // Oracle Cloud
		"168.63.129.16",   // Azure wire server
		"fd00:ec2::254",   // AWS IMDS over IPv6
	}
	for _, raw := range metadata {
		if ip.Equal(net.ParseIP(raw)) {
			return true
		}
	}
	return false
}

// isReservedIP 覆盖 CGNAT 与不能作为公网目标的保留段。TEST-NET
// （192.0.2/24、198.51.100/24、203.0.113/24）不在此列：它们常用于
// 文档/测试，且不指向真实内网资源。
//
// IPv6 的隧道/转换前缀必须显式处理：6to4（2002::/16）与 NAT64
// （64:ff9b::/96）的末 32 位是 IPv4 地址，转换后可以指向私网/元数据；
// IPv4-compatible（::/96）不被 Go 的 To4() 识别，若不处理，
// `::10.0.0.8` 一类目标会绕过全部 IPv4 规则。三类一律按内嵌 IPv4
// 重新判定（6to4 整体视为保留——该机制已废弃）。
func isReservedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		return isReservedIPv4(v4)
	}
	if len(ip) != net.IPv6len {
		return false
	}
	// IPv4-compatible ::/96：前 96 位全零。内嵌 IPv4 必须走与字面量
	// IPv4 完全相同的分类（私网/loopback/link-local/保留段）。
	if allZeroBytes(ip[:12]) {
		embedded := net.IP(ip[12:16])
		return embedded.IsUnspecified() || embedded.IsLoopback() ||
			embedded.IsPrivate() || embedded.IsLinkLocalUnicast() ||
			embedded.IsLinkLocalMulticast() || embedded.IsMulticast() ||
			isReservedIPv4(embedded)
	}
	// 2002::/16 6to4：内嵌任意 IPv4，且已废弃，整体拒绝。
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return true
	}
	// 64:ff9b::/96 NAT64 well-known prefix：转换后内嵌 IPv4。
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b &&
		allZeroBytes(ip[4:12]) {
		return true
	}
	// IPv6 保留：100::/64（discard）、2001:db8::/32（documentation）。
	if ip[0] == 0x01 && ip[1] == 0x00 && allZeroBytes(ip[2:8]) {
		return true
	}
	if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return true
	}
	return false
}

// isReservedIPv4 是 IPv4 保留段分类（也用于 IPv6 中内嵌的 IPv4）。
func isReservedIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 0: // 0.0.0.0/8（含 unspecified）
		return true
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
		return true
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
		return true
	case v4[0] >= 240: // 240.0.0.0/4（含 255.255.255.255）
		return true
	}
	return false
}

func allZeroBytes(bytes []byte) bool {
	for _, b := range bytes {
		if b != 0 {
			return false
		}
	}
	return true
}

// isIPv6Local 覆盖 strict 额外拒绝的 IPv6 本地地址（ULA、link-local、
// site-local、loopback）。ULA/link-local/loopback 在 basic 已由
// IsPrivate/IsLinkLocal* 覆盖，这里显式列出以免依赖标准库的边界变化。
func isIPv6Local(ip net.IP) bool {
	if ip.To4() != nil || len(ip) != net.IPv6len {
		return false
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() {
		return true
	}
	// fec0::/10（已废弃的 site-local）。
	return ip[0] == 0xfe && (ip[1]&0xc0) == 0xc0
}
