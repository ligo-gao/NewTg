package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/egress"
)

// 本文件是任务提交的出站执行器：宿主拥有连接（relay-06 §0.1），插件只
// 返回请求描述符。安全边界：
//   - 仅 http/https；主机白名单 = 渠道 base URL 主机 ∪ 插件 meta.allowedHosts
//     （`credentialless: true` 的公开 CDN 例外，见下）；
//   - 禁止重定向；单次 30s；响应上限 4MiB；
//   - 私网 / 环回 / 链路本地 / 组播的**字面量**地址拒绝；
//   - **DNS 级守卫**：所有出站（submit/query/poll/artifact）都经
//     `internal/egress` 的 Guard：解析全部候选 IP 后逐个校验，并直接拨号到
//     已校验 IP（防 DNS rebinding）；策略来自 `sec.ssrf.*`（B2 的
//     PolicyFromConfig），任务面再叠加「渠道 base URL + allowedHosts」允许清单。
//
// 为什么字面量检查之外还要 Guard：域名可以在校验与拨号之间换答案（TOCTOU），
// 只查字面量等于把 SSRF 防护留给 DNS 服务器的诚实度。
//
// `credentialless: true` 只允许 GET/HEAD、无插件头/体；目标不限于渠道主机
// （公开动态 CDN），但**仍**走 Guard 的地址/重定向规则（security-04 §2.4）。

const (
	// OutboundTimeout 是单次提交请求的总时限（brief 冻结 30s）。
	// 用 Client.Timeout + context 双保险：前者约束传输，后者让插件解析钩子
	// 之后的等待也能随请求取消。
	OutboundTimeout = 30 * time.Second

	// MaxResponseBytes 是上游响应体上限（brief 冻结 4MiB）。
	// 超限必须报错而不是截断：截断会让 parseSubmitResponse 看到不完整 JSON
	// 并可能把失败解析成成功。
	MaxResponseBytes = 4 << 20

	// MaxContentBytes 是**产物内容**代理的流式上限（brief 冻结 64MiB）。
	// 与 MaxResponseBytes 分开：4MiB 是 JSON 控制面，产物是二进制数据面，
	// 两者语义不同。内容路径不整块读入内存——超限时中止流而不是截断。
	MaxContentBytes = 64 << 20

	// ContentIdleTimeout 是内容流**单次读之间**的空闲上限。
	//
	// 与 JSON 控制面的 30s 总时限不同：64MiB 产物在慢链路上可能合法地
	// 传输超过 30s，用总时限会静默截断大文件；空闲超时只惩罚真正卡死的
	// 上游（连续 30s 一个字节都没有）。
	ContentIdleTimeout = 30 * time.Second

	// ContentTotalTimeout 是内容流的**总时长**上限。它防的是「持续滴流、
	// 永不结束」的上游（空闲超时抓不到），同时给慢链路留出 10 分钟余量。
	ContentTotalTimeout = 10 * time.Minute

	// SSEEventMaxBytes 是单个 SSE 事件的累积上限（relay-06 §5.3）。
	// 事件、累积状态与持久化数据各限 1 MiB，且**从不截断**。
	SSEEventMaxBytes = 1 << 20

	// SSETotalTimeout 是单次提交事件流的**外层**上限。
	//
	// 不能用 JSON 控制面的 30s：厂商生成流可能合法地持续数分钟。空闲超时
	// （30s 无字节）负责抓卡死上游；这里只防「持续滴流、永不 done」。
	SSETotalTimeout = 10 * time.Minute

	// MaxFileInlinedBytes 是单个文件与单次请求**内联文件总量**的上限。
	// 设计口径为 MAX_FILE_DOWNLOAD_MB 默认 64MB（relay-06 §3.4）；调用点
	// 若未来把它做成设置项，应在这里换成一个注入的读取面。
	MaxFileInlinedBytes = 64 << 20
)

var (
	// ErrOutboundScheme 表示描述符 URL 不是 http/https。
	ErrOutboundScheme = errors.New("tasks: outbound scheme must be http or https")
	// ErrOutboundHost 表示主机不在白名单内。
	ErrOutboundHost = errors.New("tasks: outbound host is not allowed")
	// ErrOutboundBlockedAddr 表示目标主机是私网/环回/链路本地/组播字面量。
	ErrOutboundBlockedAddr = errors.New("tasks: outbound host resolves to a blocked address literal")
	// ErrOutboundRedirect 表示上游返回重定向；宿主不跟随（防止白名单被绕过）。
	ErrOutboundRedirect = errors.New("tasks: outbound redirect is not allowed")
	// ErrResponseTooLarge 表示响应体超过 MaxResponseBytes。
	ErrResponseTooLarge = errors.New("tasks: upstream response exceeds size limit")
	// ErrUnsupportedResponseType 表示插件请求了本任务未实现的响应类型。
	ErrUnsupportedResponseType = errors.New("tasks: unsupported submit response type")
	// ErrOutboundPolicy 表示目标未通过统一出站策略（egress.Guard）。
	// 底层错误文本只含主机/分类，不含完整 URL 与 query，可安全分类。
	ErrOutboundPolicy = errors.New("tasks: outbound target rejected by SSRF policy")
	// ErrFilePlaceholder 表示 buildSubmitRequest 的文件占位符非法或超限。
	// 调用方应把它映射为客户端可修正的 4xx（占位符形状由插件作者声明）。
	ErrFilePlaceholder = errors.New("tasks: invalid file placeholder")
)

// outboundPolicy 是统一出站策略的进程级装配点。
//
// 为什么用包级原子指针：DoOutbound/DoOutboundStream 的签名被轮询器与
// httpapi 共用，且任务面需要「一次装配、处处生效」——每个请求都读同一份
// sec.ssrf.* 快照。Policy 只在启动期写入（httpapi assembly / worker 装配），
// 运行期只读；测试可用 DoOutboundWithPolicy 显式注入，不受全局影响。
var outboundPolicy atomic.Pointer[egress.Policy]

// SetOutboundPolicy 装配统一出站策略（PolicyFromConfig 的结果）。
func SetOutboundPolicy(policy egress.Policy) { outboundPolicy.Store(&policy) }

// OutboundPolicy 返回当前出站策略；未装配时回退注册表默认值。
func OutboundPolicy() egress.Policy {
	if stored := outboundPolicy.Load(); stored != nil {
		return *stored
	}
	return egress.PolicyFromConfig(nil)
}

// RequestDescriptor 是 buildSubmitRequest 的规范化结果。
type RequestDescriptor struct {
	URL          string
	Method       string
	Headers      map[string]string
	Body         []byte
	ResponseType string
	// Credentialless 的公开 CDN 请求只允许 GET/HEAD，且不得带插件头/体
	// （relay-06 §5.2；README「credentialless」）。此时不套主机允许清单，
	// 但 SSRF 地址与重定向规则照常生效。
	Credentialless bool

	// rawBody 保留插件返回的 JSON 值，供宿主在发送前替换文件占位符。
	// 它不导出：文件字节只经宿主内存进入编码结果，JS 永远看不到。
	rawBody any
}

// ResponseSnapshot 是传给 parseSubmitResponse 的响应投影。
// headers 只保留首个值（插件只需要读 content-type 之类）。
type ResponseSnapshot struct {
	StatusCode int
	Headers    map[string]string
	Body       string
}

// DecodeRequestDescriptor 把插件返回值转成描述符。
//
// 未知字段不拒绝：插件描述符是数据形状，多余的键不会被执行；但 url 缺失、
// method 非法等会让请求发不出去，必须早失败。
func DecodeRequestDescriptor(value any) (*RequestDescriptor, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tasks: buildSubmitRequest must return an object")
	}
	desc := &RequestDescriptor{
		URL:            toString(obj["url"]),
		Method:         strings.ToUpper(toString(obj["method"])),
		ResponseType:   strings.ToLower(toString(obj["responseType"])),
		Headers:        map[string]string{},
		Credentialless: toBool(obj["credentialless"]),
	}
	if desc.URL == "" {
		return nil, fmt.Errorf("tasks: buildSubmitRequest returned an empty url")
	}
	if desc.Method == "" {
		desc.Method = http.MethodGet
	}
	if headers, ok := obj["headers"].(map[string]any); ok {
		for k, v := range headers {
			desc.Headers[k] = toString(v)
		}
	}
	if body, exists := obj["body"]; exists && body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("tasks: encode submit body: %w", err)
		}
		desc.Body = raw
		desc.rawBody = body
	}
	switch desc.ResponseType {
	case "":
		desc.ResponseType = "json"
	case "json":
	case "sse":
		// SSE 由宿主逐事件读取并调用 parseSubmitEvent / parseSubmitEventDelta；
		// 是否允许由提交路径按 manifest.submitResponseTypes 再判一次。
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedResponseType, desc.ResponseType)
	}
	if desc.Credentialless {
		if desc.Method != http.MethodGet && desc.Method != http.MethodHead {
			return nil, fmt.Errorf("%w: credentialless requests must use GET or HEAD", ErrOutboundPolicy)
		}
		if len(desc.Headers) != 0 || len(desc.Body) != 0 {
			return nil, fmt.Errorf("%w: credentialless requests cannot carry headers or a body", ErrOutboundPolicy)
		}
	}
	return desc, nil
}

// toBool 把插件导出的布尔解释为 bool；其它值按 false（描述符字段是可选开关）。
func toBool(v any) bool {
	value, _ := v.(bool)
	return value
}

// NewOutboundClient 构造禁重定向的出站客户端。
//
// CheckRedirect 返回错误而不是 ErrUseLastResponse：后者把 3xx 交给调用方，
// 一个忘了检查状态码的新调用点就可能把 Location 内容当成功响应解析。直接
// 报错让「重定向」在任何路径上都是失败。
func NewOutboundClient() *http.Client {
	return &http.Client{
		Timeout: OutboundTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrOutboundRedirect
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          50,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// ValidateOutboundURL 校验 scheme、主机白名单与字面量地址。
//
// allowedHosts 由调用方合并两个来源：渠道 base URL 的主机（插件声明的
// baseUrl 可能为空/缺省，渠道才是运行时基址）与插件 meta.allowedHosts。
// 比较前统一小写；入口若带端口（host:port），只与同样带端口的白名单项
// 精确匹配，不带端口的项按主机名匹配（manifest 允许两种写法）。
func ValidateOutboundURL(raw string, allowedHosts []string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("tasks: invalid outbound url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrOutboundScheme
	}
	if u.User != nil || u.Host == "" {
		return nil, ErrOutboundHost
	}
	host := strings.ToLower(u.Host)
	hostname := strings.ToLower(u.Hostname())
	if isBlockedLiteral(hostname) {
		return nil, ErrOutboundBlockedAddr
	}
	for _, allowed := range allowedHosts {
		candidate := strings.ToLower(strings.TrimSpace(allowed))
		if candidate == "" {
			continue
		}
		if candidate == host {
			return u, nil
		}
		// 不带端口的白名单项按主机名匹配；带端口的必须与 host:port 全等。
		if !hostHasPort(candidate) && candidate == hostname {
			return u, nil
		}
	}
	return nil, ErrOutboundHost
}

// hostHasPort 判断白名单项是否显式带端口。IPv6 字面量形如 [::1]:8080，
// 冒号在方括号内不算端口分隔符；方括号外还有冒号才算带端口。
func hostHasPort(host string) bool {
	if strings.HasPrefix(host, "[") {
		end := strings.LastIndex(host, "]")
		return end >= 0 && len(host) > end+1 && host[end+1] == ':'
	}
	return strings.Count(host, ":") == 1
}

// isBlockedLiteral 判断主机名是否是必须拒绝的地址字面量。
//
// 只处理 IP 字面量：域名解析后的地址属于 DNS 级守卫（已登记，T6a 不实现）。
// netip 的 IsPrivate 覆盖 RFC1918 与 fc00::/7；IsLinkLocal* 覆盖 169.254/16
// 与 fe80::/10；IsMulticast 覆盖 224/4 与 ff00::/8；IsUnspecified 覆盖
// 0.0.0.0 / ::。IPv4-mapped IPv6（::ffff:127.0.0.1）先 Unmap 再判断，
// 否则可以借映射形式绕过环回检查。
func isBlockedLiteral(hostname string) bool {
	addr, err := netip.ParseAddr(hostname)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() ||
		addr.IsUnspecified()
}

// AllowedHosts 合并渠道 base URL 主机与插件声明的主机白名单。
//
// baseURL 解析失败返回空白名单（不是「全部放行」）：渠道配置坏掉时
// 出站守卫必须拒绝，而不是退化成任意主机可出站。
func AllowedHosts(baseURL string, declared []string) []string {
	out := make([]string, 0, len(declared)+1)
	if u, err := url.Parse(strings.TrimSpace(baseURL)); err == nil && u.Host != "" {
		out = append(out, u.Host)
	}
	out = append(out, declared...)
	return out
}

// PolicyForHosts 把统一出站策略收窄到本次任务的允许主机。
//
// 任务面的冻结语义（relay-06 §5.2）：允许清单 = 渠道 Base URL 主机 +
// `meta.allowedHosts`，而不是全局 sec.ssrf.domain_list。因此：
//   - 基础策略的地址/端口/协议/私网规则全部保留；
//   - 域名模式改为 allowlist，清单就是本次合并出的主机；
//   - 基础策略显式配置为 denylist 时保留 denylist（管理员要求的是「这些
//     不許」，任务主机仍会先经过 AllowedHosts 的 allowlist 检查）；
//   - credentialless 例外：公开 CDN 不套主机允许清单（DomainAll），但地址
//     与重定向规则仍然生效。
//
// allowPrivate 仅供 httptest 装配：放行环回与任意端口，其它规则不关。
func PolicyForHosts(base egress.Policy, allowedHosts []string, credentialless, allowPrivate bool) egress.Policy {
	policy := base
	if allowPrivate {
		policy.AllowLoopback = true
		policy.AllowAnyPort = true
	}
	switch {
	case credentialless:
		policy.DomainMode = egress.DomainAll
		policy.DomainList = nil
	case policy.DomainMode != egress.DomainDenylist:
		policy.DomainMode = egress.DomainAllowlist
		policy.DomainList = allowedHostPatterns(allowedHosts)
	}
	return policy
}

// allowedHostPatterns 把 `host[:port]` 白名单项归一为 egress 期望的主机名。
//
// egress 的域名匹配对象是 URL.Hostname()（不含端口）；端口约束由策略的
// 端口规则承担。若把 "api.example.com:8443" 原样放进清单，它永远不会
// 匹配（hostname 无端口），测试用的随机端口也会被误拒。
func allowedHostPatterns(hosts []string) []string {
	out := make([]string, 0, len(hosts))
	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			continue
		}
		if strings.HasPrefix(host, "[") {
			if end := strings.Index(host, "]"); end > 0 {
				host = host[1:end]
			}
		} else if strings.Count(host, ":") == 1 {
			host = host[:strings.Index(host, ":")]
		}
		if host != "" {
			out = append(out, host)
		}
	}
	return out
}

// guardedClient 复制调用方客户端并把拨号换成 Guard.DialContext。
//
// 复制而不是替换调用方客户端：httpapi 的测试会注入带 httptest TLS 配置的
// 客户端，信任链必须保留；只替换拨号与重定向，连接池/超时仍是调用方的。
func guardedClient(base *http.Client, guard *egress.Guard) *http.Client {
	if base == nil {
		base = NewOutboundClient()
	}
	clone := *base
	var transport *http.Transport
	if existing, ok := base.Transport.(*http.Transport); ok && existing != nil {
		transport = existing.Clone()
	} else {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	// 代理会让拨号目标变成代理而不是目标主机，策略校验对象与实际连接对象
	// 分离；与 egress.NewClient 同一口径，这里也禁用代理。
	transport.Proxy = nil
	transport.DialContext = guard.DialContext
	clone.Transport = transport
	// 任何 3xx 都是失败而不是交给调用方：忘记检查状态码的调用点会把
	// Location 内容当成功响应解析（既有冻结语义）。
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return ErrOutboundRedirect
	}
	return &clone
}

// resolveOutboundTarget 做形状/允许清单检查，并返回经统一策略校验的 URL 与 Guard。
func resolveOutboundTarget(ctx context.Context, raw string, allowedHosts []string, policy egress.Policy, credentialless, allowPrivate bool) (*url.URL, *egress.Guard, error) {
	var (
		u   *url.URL
		err error
	)
	if credentialless {
		u, err = parseCredentiallessURL(raw, allowPrivate)
	} else if allowPrivate {
		u, err = parseForTest(raw, allowedHosts)
	} else {
		u, err = ValidateOutboundURL(raw, allowedHosts)
	}
	if err != nil {
		return nil, nil, err
	}
	guard := egress.NewGuard(PolicyForHosts(policy, allowedHosts, credentialless, allowPrivate))
	// 尽早给策略错误（解析全部候选地址并逐个检查）。拨号时 Guard 会再次
	// 校验最终 IP，因此这里的解析不构成 rebinding 窗口。
	if _, err := guard.ValidateURL(ctx, u.String()); err != nil {
		return nil, nil, mapEgressError(err)
	}
	return u, guard, nil
}

// mapEgressError 把统一执行器的错误收敛为任务面哨兵。
//
// 绝不回传 egress 的原始文本给 API：其中虽不含 query，但保持一层分类能让
// 调用方稳定映射状态码，也避免未来错误文案变化影响对外契约。
func mapEgressError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, egress.ErrRedirect):
		return fmt.Errorf("%w: %w", ErrOutboundPolicy, ErrOutboundRedirect)
	default:
		return fmt.Errorf("%w: %w", ErrOutboundPolicy, egress.ErrBlocked)
	}
}

// outboundTransportError 收敛拨号期（DialContext）返回的策略错误。
//
// Guard 在拨号时重新解析并校验 IP，所以 rebinding 的失败发生在 client.Do
// 内部，而不是前置 ValidateURL。若不在这里分类，调用方拿到的会是带完整
// URL 的 *url.Error；策略错误必须回到固定哨兵，避免 URL/query 外泄。
func outboundTransportError(err error) error {
	if errors.Is(err, egress.ErrBlocked) || errors.Is(err, egress.ErrUnresolved) ||
		errors.Is(err, egress.ErrRedirect) {
		return mapEgressError(err)
	}
	return fmt.Errorf("tasks: outbound request failed: %w", err)
}

// parseCredentiallessURL 只做 credentialless 的 URL 形状检查：绝对 http(s)、
// 无 userinfo、非私网字面量。主机允许清单不适用（公开 CDN）。
func parseCredentiallessURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, ErrOutboundHost
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrOutboundScheme
	}
	if u.User != nil {
		return nil, ErrOutboundHost
	}
	if !allowPrivate && isBlockedLiteral(strings.ToLower(u.Hostname())) {
		return nil, ErrOutboundBlockedAddr
	}
	return u, nil
}

// DoOutbound 校验描述符并发出请求，返回有界响应快照。
//
// allowPrivate 仅供测试注入 httptest（127.0.0.1）使用；生产装配恒为 false。
// 它不关闭 scheme/白名单/重定向/大小限制，也不关闭 DNS 地址校验（只把
// 「环回/任意端口」加入策略），把测试接缝的影响面压到最小。
//
// 策略来自进程级 SetOutboundPolicy（PolicyFromConfig 的结果）；需要显式
// 注入时用 DoOutboundWithPolicy 或 DoOutboundWithGuard。
func DoOutbound(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, allowPrivate bool) (*ResponseSnapshot, error) {
	return DoOutboundWithPolicy(ctx, client, desc, allowedHosts, OutboundPolicy(), allowPrivate)
}

// DoOutboundWithPolicy 是同 DoOutbound，但使用调用方显式给出的基础策略。
func DoOutboundWithPolicy(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, policy egress.Policy, allowPrivate bool) (*ResponseSnapshot, error) {
	u, guard, err := resolveOutboundTarget(ctx, desc.URL, allowedHosts, policy, desc.Credentialless, allowPrivate)
	if err != nil {
		return nil, err
	}
	return doOutbound(ctx, guardedClient(client, guard), desc, u)
}

// DoOutboundWithGuard 允许测试注入带自定义解析器的 Guard（DNS rebinding 用例）。
// 生产调用方使用 DoOutbound/DoOutboundWithPolicy。
func DoOutboundWithGuard(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, guard *egress.Guard) (*ResponseSnapshot, error) {
	if guard == nil {
		return nil, fmt.Errorf("%w: nil guard", ErrOutboundPolicy)
	}
	u, err := ValidateOutboundURL(desc.URL, allowedHosts)
	if err != nil {
		return nil, err
	}
	if _, err := guard.ValidateURL(ctx, u.String()); err != nil {
		return nil, mapEgressError(err)
	}
	return doOutbound(ctx, guardedClient(client, guard), desc, u)
}

func doOutbound(ctx context.Context, client *http.Client, desc *RequestDescriptor, u *url.URL) (*ResponseSnapshot, error) {
	if desc == nil {
		return nil, fmt.Errorf("tasks: nil request descriptor")
	}
	reqCtx, cancel := context.WithTimeout(ctx, OutboundTimeout)
	defer cancel()

	var body io.Reader
	if len(desc.Body) > 0 {
		body = bytes.NewReader(desc.Body)
	}
	req, err := http.NewRequestWithContext(reqCtx, desc.Method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("tasks: build outbound request: %w", err)
	}
	for k, v := range desc.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 错误文本可能包含 URL；策略类错误收敛为哨兵，其余保留分类。
		return nil, outboundTransportError(err)
	}
	defer resp.Body.Close()

	if resp.ContentLength > MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	limited := io.LimitReader(resp.Body, MaxResponseBytes+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("tasks: read upstream response: %w", err)
	}
	if len(payload) > MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}

	headers := make(map[string]string, len(resp.Header))
	for k, values := range resp.Header {
		if len(values) > 0 {
			headers[k] = values[0]
		}
	}
	return &ResponseSnapshot{StatusCode: resp.StatusCode, Headers: headers, Body: string(payload)}, nil
}

// StreamResponse 是流式出站响应：Body 由调用方负责关闭。
type StreamResponse struct {
	StatusCode int
	Headers    map[string]string
	Body       io.ReadCloser
}

// DoOutboundStream 校验描述符并**流式**返回上游响应（产物内容专用）。
//
// 与 DoOutbound 共用同一套守卫（scheme / 主机白名单 / 字面量私网 / 禁重定向
// / 30s 客户端），只把「读满 4MiB 到内存」换成「按 MaxContentBytes 边读边
// 转发」——产物可以远大于 JSON 控制面，整块读入会直接打爆进程内存。
//
// 超限语义：调用方已经写出响应头后无法改状态码，因此错误在 Read 时返回，
// 由调用方关闭连接（宁可不完整的流也不能把截断内容伪装成完整产物）。
func DoOutboundStream(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, allowPrivate bool) (*StreamResponse, error) {
	return DoOutboundStreamWithPolicy(ctx, client, desc, allowedHosts, OutboundPolicy(), allowPrivate)
}

// DoOutboundStreamWithPolicy 是同 DoOutboundStream，但使用显式策略。
func DoOutboundStreamWithPolicy(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, policy egress.Policy, allowPrivate bool) (*StreamResponse, error) {
	if desc == nil {
		return nil, fmt.Errorf("tasks: nil request descriptor")
	}
	u, guard, err := resolveOutboundTarget(ctx, desc.URL, allowedHosts, policy, desc.Credentialless, allowPrivate)
	if err != nil {
		return nil, err
	}
	streamClient := guardedClient(client, guard)
	// 客户端本身带 30s 总超时（JSON 控制面的冻结值）；内容流改用
	// 空闲+总时长双限，因此清空 Timeout。Transport（含 Guard 拨号）保留。
	streamClient.Timeout = 0

	reqCtx, cancel := context.WithTimeout(ctx, ContentTotalTimeout)
	var body io.Reader
	if len(desc.Body) > 0 {
		body = bytes.NewReader(desc.Body)
	}
	req, err := http.NewRequestWithContext(reqCtx, desc.Method, u.String(), body)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("tasks: build outbound request: %w", err)
	}
	for k, v := range desc.Headers {
		req.Header.Set(k, v)
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		cancel()
		return nil, outboundTransportError(err)
	}
	if resp.ContentLength > MaxContentBytes {
		resp.Body.Close()
		cancel()
		return nil, ErrResponseTooLarge
	}
	headers := make(map[string]string, len(resp.Header))
	for k, values := range resp.Header {
		if len(values) > 0 {
			headers[k] = values[0]
		}
	}
	// cancel 由 body 的 Close 触发：流式响应必须在读取结束后释放 context。
	return &StreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       &boundedReadCloser{rc: newIdleReadCloser(resp.Body, ContentIdleTimeout), cancel: cancel, max: MaxContentBytes},
	}, nil
}

// idleReadCloser 在两次成功读取之间超过 idle 没有数据时关闭上游 body，
// 让阻塞中的 Read 以错误返回。
//
// 为什么用「超时后 Close」而不是给每个 Read 加 context：net/http 的
// Response.Body.Read 不接受 context，关闭连接是唯一能打断阻塞读的手段。
type idleReadCloser struct {
	rc    io.ReadCloser
	idle  time.Duration
	timer *time.Timer
}

func newIdleReadCloser(rc io.ReadCloser, idle time.Duration) *idleReadCloser {
	r := &idleReadCloser{rc: rc, idle: idle}
	r.reset()
	return r
}

func (r *idleReadCloser) reset() {
	if r.timer != nil {
		r.timer.Stop()
	}
	if r.idle > 0 {
		r.timer = time.AfterFunc(r.idle, func() { _ = r.rc.Close() })
	}
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.reset()
	}
	return n, err
}

func (r *idleReadCloser) Close() error {
	if r.timer != nil {
		r.timer.Stop()
	}
	return r.rc.Close()
}

// boundedReadCloser 给上游 body 套上字节上限，并在 Close 时取消请求 context。
type boundedReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	max    int64
	total  int64
}

func (b *boundedReadCloser) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.total += int64(n)
	if b.total > b.max {
		// 只交付上限内的字节；多读的部分丢弃并报错，绝不把超限内容当完整流。
		over := int(b.total - b.max)
		if over >= n {
			return 0, ErrResponseTooLarge
		}
		return n - over, ErrResponseTooLarge
	}
	return n, err
}

func (b *boundedReadCloser) Close() error {
	err := b.rc.Close()
	b.cancel()
	return err
}

// SSEEvent 是宿主解析后的单个上游事件（relay-06 §3.6）。
// 宿主只做分帧：不解析厂商 JSON、不判断完成标记、不汇总用量。
type SSEEvent struct {
	Event string
	ID    string
	Data  string
}

// errStopSSE 由回调返回表示插件已声明 done（事件已完整处理）。
var errStopSSE = errors.New("tasks: submit stream done")

// errSSEIncomplete 表示上游连接在 done 之前结束（EOF/传输错误）。
var errSSEIncomplete = errors.New("tasks: submit stream ended before done")

// DoOutboundSSE 校验描述符并逐事件回调，直到回调返回 done 或出错。
//
// 返回上游 HTTP 状态与响应头：调用方在成功流结束后把它们与累积状态一起
// 交给 parseSubmitResponse。非 2xx 不进入事件读取（设计冻结：非 2xx 在
// parseSubmitResponse 之前失败），错误文本不含 URL。
func DoOutboundSSE(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, policy egress.Policy, allowPrivate bool, onEvent func(SSEEvent) error) (int, map[string]string, error) {
	if desc == nil {
		return 0, nil, fmt.Errorf("tasks: nil request descriptor")
	}
	u, guard, err := resolveOutboundTarget(ctx, desc.URL, allowedHosts, policy, desc.Credentialless, allowPrivate)
	if err != nil {
		return 0, nil, err
	}
	streamClient := guardedClient(client, guard)
	// 事件流可能持续超过 JSON 控制面的 30s 总超时；改用 context + 空闲
	// 读超时（上游连续 30s 无字节才失败）。
	streamClient.Timeout = 0

	reqCtx, cancel := context.WithTimeout(ctx, SSETotalTimeout)
	defer cancel()

	var body io.Reader
	if len(desc.Body) > 0 {
		body = bytes.NewReader(desc.Body)
	}
	req, err := http.NewRequestWithContext(reqCtx, desc.Method, u.String(), body)
	if err != nil {
		return 0, nil, fmt.Errorf("tasks: build outbound request: %w", err)
	}
	for k, v := range desc.Headers {
		req.Header.Set(k, v)
	}
	resp, err := streamClient.Do(req)
	if err != nil {
		return 0, nil, outboundTransportError(err)
	}
	defer resp.Body.Close()

	headers := make(map[string]string, len(resp.Header))
	for k, values := range resp.Header {
		if len(values) > 0 {
			headers[k] = values[0]
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 只读取有界前缀用于快速失败；不把上游 body 交给插件（设计：非 2xx
		// 在 parseSubmitResponse 之前失败），也不回显给客户端。
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxResponseBytes))
		return resp.StatusCode, headers, fmt.Errorf("%w: %d", ErrUpstreamStatus, resp.StatusCode)
	}

	reader := newIdleReadCloser(resp.Body, ContentIdleTimeout)
	defer reader.Close()
	err = readSSE(reqCtx, reader, onEvent)
	if errors.Is(err, errStopSSE) {
		return resp.StatusCode, headers, nil
	}
	if err == nil {
		return resp.StatusCode, headers, errSSEIncomplete
	}
	return resp.StatusCode, headers, err
}

// readSSE 按 SSE 分帧规则读取事件（RFC 风格：CRLF/CR/LF、注释、多行 data）。
//
// 宿主只接受 text/event-stream 语义；每个事件的 data 上限 1 MiB，超限
// 直接失败而不是截断（截断会把不完整 JSON 交给插件）。
func readSSE(ctx context.Context, body io.Reader, onEvent func(SSEEvent) error) error {
	reader := bufio.NewReader(body)
	var (
		eventName   string
		lastEventID string
		dataLines   []string
		dataBytes   int
		sawField    bool
	)
	dispatch := func() error {
		if !sawField {
			return nil
		}
		// SSE 规范：id 字段在后续事件中持续生效，直到被新 id 覆盖。
		event := SSEEvent{Event: eventName, ID: lastEventID, Data: strings.Join(dataLines, "\n")}
		eventName, dataLines, dataBytes, sawField = "", nil, 0, false
		if event.Event == "" {
			event.Event = "message"
		}
		return onEvent(event)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			switch {
			case line == "":
				if err := dispatch(); err != nil {
					return err
				}
			case strings.HasPrefix(line, ":"):
				// 注释/心跳：不派发。
			default:
				field, value, found := strings.Cut(line, ":")
				if !found {
					field, value = line, ""
				}
				// 规范允许冒号后有一个空格，且只有第一个空格被吃掉。
				value = strings.TrimPrefix(value, " ")
				sawField = true
				switch field {
				case "event":
					eventName = value
				case "id":
					lastEventID = value
				case "data":
					dataBytes += len(value)
					if len(dataLines) > 0 {
						dataBytes++ // 换行符也计入事件上限
					}
					if dataBytes > SSEEventMaxBytes {
						return fmt.Errorf("%w: event exceeds %d bytes", ErrPluginHook, SSEEventMaxBytes)
					}
					dataLines = append(dataLines, value)
				default:
					// retry/未知字段：SSE 允许忽略。
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return errSSEIncomplete
			}
			return fmt.Errorf("tasks: read submit stream: %w", readErr)
		}
	}
}

// parseForTest 是 allowPrivate 分支的校验：保留 scheme/白名单/空主机检查，
// 跳过字面量私网拒绝。与 ValidateOutboundURL 的结构保持一致，避免测试
// 路径与生产路径出现两套「白名单」语义。
func parseForTest(raw string, allowedHosts []string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("tasks: invalid outbound url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrOutboundScheme
	}
	if u.User != nil || u.Host == "" {
		return nil, ErrOutboundHost
	}
	host := strings.ToLower(u.Host)
	hostname := strings.ToLower(u.Hostname())
	for _, allowed := range allowedHosts {
		candidate := strings.ToLower(strings.TrimSpace(allowed))
		if candidate == "" {
			continue
		}
		if candidate == host {
			return u, nil
		}
		if !hostHasPort(candidate) && candidate == hostname {
			return u, nil
		}
	}
	return nil, ErrOutboundHost
}

func toString(v any) string {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		// sobek 的数值导出可能是 float64 或 int64；整数去掉小数点，
		// 避免 "5" 变成 "5e+00"。
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	case int64:
		return fmt.Sprintf("%d", typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", typed)
	}
}
