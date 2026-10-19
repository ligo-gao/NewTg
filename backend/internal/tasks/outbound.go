package tasks

import (
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
	"time"
)

// 本文件是任务提交的出站执行器：宿主拥有连接（relay-06 §0.1），插件只
// 返回请求描述符。安全边界（brief 冻结）：
//   - 仅 http/https；主机白名单 = 渠道 base URL 主机 ∪ 插件 meta.allowedHosts；
//   - 禁止重定向；单次 30s；响应上限 4MiB；
//   - 私网 / 环回 / 链路本地 / 组播的**字面量**地址拒绝。
//
// 明确登记（T6a 不做）：DNS 级守卫（解析后的地址再判一次）属后续任务。
// 当前只拒绝字面量，域名解析到内网仍可能出站——这一点在任务报告的风险
// 项里登记，不能把「字面量拒绝」误当成完整 SSRF 防护。

const (
	// OutboundTimeout 是单次提交请求的总时限（brief 冻结 30s）。
	// 用 Client.Timeout + context 双保险：前者约束传输，后者让插件解析钩子
	// 之后的等待也能随请求取消。
	OutboundTimeout = 30 * time.Second

	// MaxResponseBytes 是上游响应体上限（brief 冻结 4MiB）。
	// 超限必须报错而不是截断：截断会让 parseSubmitResponse 看到不完整 JSON
	// 并可能把失败解析成成功。
	MaxResponseBytes = 4 << 20
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
)

// RequestDescriptor 是 buildSubmitRequest 的规范化结果。
type RequestDescriptor struct {
	URL          string
	Method       string
	Headers      map[string]string
	Body         []byte
	ResponseType string
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
		URL:          toString(obj["url"]),
		Method:       strings.ToUpper(toString(obj["method"])),
		ResponseType: strings.ToLower(toString(obj["responseType"])),
		Headers:      map[string]string{},
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
	}
	switch desc.ResponseType {
	case "", "json":
		// 本任务只实现 JSON 快照；SSE 增量（submit-sse-delta@1）归后续任务。
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedResponseType, desc.ResponseType)
	}
	return desc, nil
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

// DoOutbound 校验描述符并发出请求，返回有界响应快照。
//
// allowPrivate 仅供测试注入 httptest（127.0.0.1）使用；生产装配恒为 false。
// 它不关闭 scheme/白名单/重定向/大小限制，只跳过「字面量私网地址」检查，
// 把测试接缝的影响面压到最小。
func DoOutbound(ctx context.Context, client *http.Client, desc *RequestDescriptor, allowedHosts []string, allowPrivate bool) (*ResponseSnapshot, error) {
	if desc == nil {
		return nil, fmt.Errorf("tasks: nil request descriptor")
	}
	var (
		u   *url.URL
		err error
	)
	if allowPrivate {
		u, err = parseForTest(desc.URL, allowedHosts)
	} else {
		u, err = ValidateOutboundURL(desc.URL, allowedHosts)
	}
	if err != nil {
		return nil, err
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
		// 错误文本可能包含 URL；调用方不得把它写进响应/日志（GC-6 同口径）。
		return nil, fmt.Errorf("tasks: outbound request failed: %w", err)
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
		// sobek 的数值导出是 float64；整数去掉小数点，避免 "5" 变成 "5e+00"。
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", typed)
	}
}
