// Package market 拉取任务插件市场索引与插件包体（R-M6D-08 / modules-08 F-08-33/34）。
//
// 索引格式（2026-10-20 冻结，实现期回写设计）：
//
//	{"schema_version": 1,
//	 "plugins": [{"key","name","description","icon_url",
//	              "versions": [{"version","api_version","source_url","source_hash"}]}]}
//
// 包体 = 单文件 JS 原始字节；安装前必须校验 source_hash（S3），任何不符
// 零写入。
//
// 出站守卫（S2 / B2）：本包沿用 https 强制与「3xx 不跟随」语义；生产
// 路径（未注入 HTTP）的协议/端口/userinfo/域名/解析地址校验与反 rebinding
// 拨号统一由 internal/egress 完成（sec.ssrf.* 策略）。注入 HTTP 仅作为
// 测试接缝，不改变重定向拒绝。
package market

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"newtg/gateway/internal/egress"
)

// 默认上限：索引 1 MiB、包体 2 MiB（与 `plugin.upload.max_bytes` 默认值同口径）。
// 上限的意义是内存与耗时上界：索引/包体由远端控制，不设上限等于让一个
// 恶意市场源把网关内存与磁盘吞吐作为放大器。
const (
	DefaultMaxIndexBytes  = 1 << 20
	DefaultMaxSourceBytes = 2 << 20
	DefaultTimeout        = 15 * time.Second
)

var (
	// ErrNonHTTPS 表示 URL 不是 https（R-M6D-03：MVP 仅 https）。
	ErrNonHTTPS = errors.New("market: url must use https")
	// ErrUnsafeURL 表示最小 SSRF 守卫拒绝该 URL（userinfo/私网字面量/本地名）。
	ErrUnsafeURL = errors.New("market: url rejected by SSRF guard")
	// ErrRedirect 表示远端返回 3xx；重定向可以把已校验的 https URL 引到任意目标。
	ErrRedirect = errors.New("market: redirects are not allowed")
	// ErrInvalidIndex 表示索引结构/字段不合法。
	ErrInvalidIndex = errors.New("market: invalid marketplace index")
	// ErrHashMismatch 表示下载包体的 sha256 与索引声明不符（S3）。
	ErrHashMismatch = errors.New("market: source hash mismatch")
	// ErrTooLarge 表示响应超过上限。
	ErrTooLarge = errors.New("market: response too large")
)

// StatusError 表示远端返回非 2xx。
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("market: unexpected HTTP status %d", e.Code) }

// Index 是 schema_version=1 的市场索引。
type Index struct {
	SchemaVersion int      `json:"schema_version"`
	Plugins       []Plugin `json:"plugins"`
}

// Plugin 是索引中的一个插件条目。
type Plugin struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IconURL     string    `json:"icon_url"`
	Versions    []Version `json:"versions"`
}

// Version 是索引中的一个版本条目。
type Version struct {
	Version    string `json:"version"`
	APIVersion string `json:"api_version"`
	SourceURL  string `json:"source_url"`
	SourceHash string `json:"source_hash"`
}

// Client 拉取市场资源。零值可用（默认上限/超时/不跟随重定向）。
//
// 出站注入点（http/guard/resolve/tlsConfig）一律未导出：它们是测试接缝，
// 生产装配只能经 NewClientWithConfig 从 sec.ssrf.* 构造受守卫的客户端，
// 不存在「顺手把 HTTP 字段换掉」的静默旁路。需要注入的用例必须显式调用
// NewClientWithHTTP（见 LOW-2）。
//
// 生产路径（http == nil）的出站统一走 internal/egress：解析全部地址并
// 校验后才拨号（反 rebinding），3xx 仍不跟随。
type Client struct {
	MaxIndexBytes  int64
	MaxSourceBytes int64

	// Config 是 sec.ssrf.* 的读取面；nil 时使用注册表默认策略。
	Config egress.ConfigReader

	// 以下为仅测试接缝，故意未导出。
	http          *http.Client
	resolve       func(ctx context.Context, host string) ([]net.IP, error)
	guardOverride *egress.Guard
	tlsConfig     *tls.Config
}

// NewClient 返回默认配置的客户端。
func NewClient() *Client { return &Client{} }

// NewClientWithConfig 返回从 sec.ssrf.* 读取策略的客户端（生产装配用）。
func NewClientWithConfig(cfg egress.ConfigReader) *Client {
	return &Client{Config: cfg}
}

// NewClientWithHTTP 是显式注入接缝（httptest TLS / 固定地址），只用于测试。
// 字段未导出后，生产代码无法静默替换 transport；调用本构造函数即明确声明
// 「这次出站由调用方控制拨号」。注入客户端仍保留 https 与「3xx 不跟随」。
func NewClientWithHTTP(base *http.Client) *Client { return &Client{http: base} }

func (c *Client) guard() *egress.Guard {
	if c != nil && c.guardOverride != nil {
		return c.guardOverride
	}
	var cfg egress.ConfigReader
	if c != nil {
		cfg = c.Config
	}
	guard := egress.NewGuard(egress.PolicyFromConfig(cfg))
	if c != nil && c.resolve != nil {
		guard = guard.WithResolver(c.resolve)
	}
	return guard
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.http != nil {
		// 测试接缝：沿用注入的 transport（TLS 配置/拨号由用例控制）；
		// 重定向仍被强制拒绝，保证 TestFetchIndexRejectsRedirects 有效。
		clone := *c.http
		if clone.Timeout <= 0 {
			clone.Timeout = DefaultTimeout
		}
		clone.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		return &clone
	}
	var (
		timeout   = DefaultTimeout
		tlsConfig *tls.Config
	)
	if c != nil {
		tlsConfig = c.tlsConfig
	}
	return c.guard().NewClient(egress.ClientOptions{
		Timeout:         timeout,
		FollowRedirects: false,
		TLSConfig:       tlsConfig,
	})
}

func (c *Client) maxIndex() int64 {
	if c != nil && c.MaxIndexBytes > 0 {
		return c.MaxIndexBytes
	}
	return DefaultMaxIndexBytes
}

func (c *Client) maxSource() int64 {
	if c != nil && c.MaxSourceBytes > 0 {
		return c.MaxSourceBytes
	}
	return DefaultMaxSourceBytes
}

// ValidateURL 实施最小出站守卫并返回解析后的 URL。
//
// 只接受绝对 https URL；拒绝 userinfo（`https://user:pass@host/` 会让
// 凭据进入日志/错误文本）、拒绝非 443 以外的显式端口？——不拒绝自定义端口，
// 但拒绝环回/私网/链路本地 IP 字面量与 localhost/.local/.internal 一类
// 本地名。完整 DNS 级守卫待 security-04。
func ValidateURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty url", ErrNonHTTPS)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNonHTTPS, err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, fmt.Errorf("%w: %s", ErrNonHTTPS, u.Scheme)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: missing host", ErrUnsafeURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo is not allowed", ErrUnsafeURL)
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		if unsafeIP(ip) {
			return nil, fmt.Errorf("%w: private or loopback address %s", ErrUnsafeURL, host)
		}
	} else {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
			strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
			return nil, fmt.Errorf("%w: local host name %s", ErrUnsafeURL, host)
		}
	}
	return u, nil
}

func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

// FetchIndex 下载并校验索引。
//
// 先校验 URL 再发请求；响应 3xx 直接拒绝（CheckRedirect 返回
// ErrUseLastResponse，这里把非 2xx 映射为可分类错误）。索引内容做结构与
// 长度校验，避免把畸形索引带到管理面再 500。
func (c *Client) FetchIndex(ctx context.Context, rawURL string) (*Index, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, u, c.maxIndex())
	if err != nil {
		return nil, err
	}
	var index Index
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&index); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidIndex, err)
	}
	if err := ValidateIndex(&index); err != nil {
		return nil, err
	}
	return &index, nil
}

// FetchSource 下载插件包体并校验 source_hash。
//
// 校验在返回任何字节之前完成：hash 不符返回 ErrHashMismatch，调用方因此
// 不可能在失败时拿到「可入库」的内容（S3 零写入）。
func (c *Client) FetchSource(ctx context.Context, rawURL, expectedHash string) ([]byte, error) {
	u, err := ValidateURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !isSHA256Hex(expectedHash) {
		return nil, fmt.Errorf("%w: source_hash must be 64 lowercase hex characters", ErrInvalidIndex)
	}
	body, err := c.get(ctx, u, c.maxSource())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	actual := hex.EncodeToString(sum[:])
	if actual != strings.ToLower(expectedHash) {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrHashMismatch, strings.ToLower(expectedHash), actual)
	}
	return body, nil
}

func (c *Client) get(ctx context.Context, u *url.URL, limit int64) ([]byte, error) {
	// 注入 HTTP 的用例由用例自己控制拨号（TLS/随机端口），这里只保留
	// 静态 https 校验；生产路径（HTTP == nil）一律先过统一执行器。
	if c == nil || c.http == nil || c.guardOverride != nil {
		if _, err := c.guard().ValidateURL(ctx, u.String()); err != nil {
			return nil, mapEgressError(err, u)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("market: build request")
	}
	request.Header.Set("Accept", "application/json, text/javascript, */*")
	client := c.httpClient()
	if c == nil || c.http == nil {
		// 生产路径每次新建 Transport（与原实现一致）；关闭空闲连接，
		// 避免为每个市场源留下 30s 的连接池。
		defer client.CloseIdleConnections()
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, mapRequestError(err, u)
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, ErrRedirect
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &StatusError{Code: response.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("market: read response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w: limit %d bytes", ErrTooLarge, limit)
	}
	return body, nil
}

// redactedURL 只保留 scheme+host：错误文本会进入 last_error/日志，
// 不能把 query 里的潜在令牌或路径细节带出去。
func redactedURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// mapEgressError 把统一执行器的校验失败映射回本包的稳定分类。
//
// URL 语义拒绝（协议/端口/userinfo/域名/地址）沿用 ErrUnsafeURL（422）；
// 解析失败是「源不可达」而不是「URL 非法」，保持 502 语义。
func mapEgressError(err error, u *url.URL) error {
	switch {
	case errors.Is(err, egress.ErrBlocked):
		return fmt.Errorf("%w: %s", ErrUnsafeURL, redactedURL(u))
	case errors.Is(err, egress.ErrRedirect):
		return ErrRedirect
	default:
		return fmt.Errorf("market: source %s is unreachable", redactedURL(u))
	}
}

// mapRequestError 收敛传输错误。底层的 *url.Error 会拼上完整 URL（含
// query），因此**不包装**原始错误，只保留可判别的类别。
func mapRequestError(err error, u *url.URL) error {
	switch {
	case errors.Is(err, egress.ErrBlocked):
		return fmt.Errorf("%w: %s", ErrUnsafeURL, redactedURL(u))
	case errors.Is(err, egress.ErrRedirect):
		return ErrRedirect
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("market: request %s: %w", redactedURL(u), context.DeadlineExceeded)
	default:
		return fmt.Errorf("market: request %s failed", redactedURL(u))
	}
}

// 索引内容的长度/形态上限。数值与 DB 列宽/运行时 manifest 校验同口径，
// 并在解析阶段就拒绝：索引不落库，但它会进入管理面响应与安装路径。
const (
	maxIndexPlugins  = 1000
	maxIndexVersions = 200
	maxKeyLen        = 30
	maxNameLen       = 128
	maxDescription   = 1024
	maxURLLen        = 1024
	maxVersionLen    = 32
)

var pluginKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateIndex 校验索引结构与字段（导出以便 handler/测试复用）。
func ValidateIndex(index *Index) error {
	if index == nil {
		return fmt.Errorf("%w: empty index", ErrInvalidIndex)
	}
	if index.SchemaVersion != 1 {
		return fmt.Errorf("%w: schema_version must be 1", ErrInvalidIndex)
	}
	if len(index.Plugins) > maxIndexPlugins {
		return fmt.Errorf("%w: too many plugins (%d)", ErrInvalidIndex, len(index.Plugins))
	}
	for i := range index.Plugins {
		p := &index.Plugins[i]
		if !pluginKeyPattern.MatchString(p.Key) {
			return fmt.Errorf("%w: plugins[%d].key %q is invalid", ErrInvalidIndex, i, p.Key)
		}
		if p.Name == "" || len([]rune(p.Name)) > maxNameLen {
			return fmt.Errorf("%w: plugins[%d].name must be 1..%d characters", ErrInvalidIndex, i, maxNameLen)
		}
		if len([]rune(p.Description)) > maxDescription {
			return fmt.Errorf("%w: plugins[%d].description is too long", ErrInvalidIndex, i)
		}
		if p.IconURL != "" {
			if _, err := ValidateURL(p.IconURL); err != nil {
				return fmt.Errorf("%w: plugins[%d].icon_url: %v", ErrInvalidIndex, i, err)
			}
		}
		if len(p.Versions) == 0 || len(p.Versions) > maxIndexVersions {
			return fmt.Errorf("%w: plugins[%d].versions must have 1..%d entries", ErrInvalidIndex, i, maxIndexVersions)
		}
		for j := range p.Versions {
			v := &p.Versions[j]
			if v.Version == "" || len([]rune(v.Version)) > maxVersionLen {
				return fmt.Errorf("%w: plugins[%d].versions[%d].version must be 1..%d characters", ErrInvalidIndex, i, j, maxVersionLen)
			}
			if v.APIVersion == "" {
				v.APIVersion = "v1"
			}
			if v.APIVersion != "v1" {
				return fmt.Errorf("%w: plugins[%d].versions[%d].api_version must be v1", ErrInvalidIndex, i, j)
			}
			if len(v.SourceURL) > maxURLLen {
				return fmt.Errorf("%w: plugins[%d].versions[%d].source_url is too long", ErrInvalidIndex, i, j)
			}
			if _, err := ValidateURL(v.SourceURL); err != nil {
				return fmt.Errorf("%w: plugins[%d].versions[%d].source_url: %v", ErrInvalidIndex, i, j, err)
			}
			if !isSHA256Hex(v.SourceHash) {
				return fmt.Errorf("%w: plugins[%d].versions[%d].source_hash must be 64 lowercase hex", ErrInvalidIndex, i, j)
			}
		}
	}
	return nil
}

func isSHA256Hex(value string) bool { return sha256HexPattern.MatchString(value) }

// FindVersion 在索引中定位 key/version；不存在返回 nil。
func (index *Index) FindVersion(key, version string) (*Plugin, *Version) {
	if index == nil {
		return nil, nil
	}
	for i := range index.Plugins {
		p := &index.Plugins[i]
		if p.Key != key {
			continue
		}
		for j := range p.Versions {
			if p.Versions[j].Version == version {
				return p, &p.Versions[j]
			}
		}
		return p, nil
	}
	return nil, nil
}
