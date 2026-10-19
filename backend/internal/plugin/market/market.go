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
// 出站守卫（S2）：本包实现**最小**守卫——固定 https、拒绝重定向、拒绝
// 私网/环回 IP 字面量与非限定主机名。DNS 解析后的地址校验（完整
// ValidateURL/DialGuard）属 security-04；在此之前，域名解析到私网仍可能
// 绕过，已在 T5 报告中登记。
package market

import (
	"context"
	"crypto/sha256"
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
// HTTP 可注入以支持测试（httptest TLS），但 CheckRedirect 始终被强制为
// 「拒绝」：注入 transport 是测试接缝，不是放开重定向的开关。
type Client struct {
	HTTP           *http.Client
	MaxIndexBytes  int64
	MaxSourceBytes int64
}

// NewClient 返回默认配置的客户端。
func NewClient() *Client { return &Client{} }

func (c *Client) httpClient() *http.Client {
	base := http.DefaultClient
	if c != nil && c.HTTP != nil {
		base = c.HTTP
	}
	clone := *base
	if clone.Timeout <= 0 {
		clone.Timeout = DefaultTimeout
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("market: build request: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/javascript, */*")
	response, err := c.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("market: request %s: %w", redactedURL(u), err)
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
