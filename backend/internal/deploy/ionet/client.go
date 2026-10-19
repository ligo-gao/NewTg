// Package ionet 是 io.net 控制面（模型部署）的 provider 客户端骨架。
//
// 设计权威：docs/modules-08-系统管理与设置-子功能设计.md §3.20.7（网络、失败
// 与重试）、§3.20.5（凭据治理）；计划裁决 R-M6D-13（base path 待实测冻结）。
//
// 安全边界（T2 的最小出站守卫，完整 SSRF 守卫待 security-04）：
//   - 主机与路径是代码常量，管理员不可配置 endpoint，因此不存在「用户提供 URL」；
//   - TLS 最低 1.2，证书校验默认开启；
//   - 不跟随任何重定向（比设计要求的「不跟随跨主机重定向」更严格：3xx 直接
//     作为错误返回，避免控制面凭据被带到未预期的位置）；
//   - 错误消息解析优先 {"detail": ...}，对外截断并抹掉 API Key 原文。
//
// T2 只交付连接测试所需：TestConnection / ListHardware / ListLocations。
// 部署列表、创建、续期、终止等由 T4 在此文件上扩展；接口已经以
// DeploymentProvider 固定，T4 只增方法不破坏已有实现。
package ionet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultBaseURL 是唯一允许的控制面主机（modules-08 §3.20.7）。
	// 管理员不可配置；Config.BaseURL 只供测试指向 httptest，生产装配必须留空。
	DefaultBaseURL = "https://api.io.solutions"

	// DefaultTimeout 是普通控制面请求上限（§3.20.7：30s）。
	DefaultTimeout = 30 * time.Second

	// apiBasePath 是 provider 控制面的版本前缀。
	//
	// ★ 待实测冻结（R-M6D-13）：没有真实凭据前无法确认 provider 实际前缀，
	// 因此只允许在这一处常量修改；路径常量全部由它派生，适配器测试锁定
	// method/path/header 契约。未实测前不得把本能力标记为「已完成」。
	apiBasePath = "/v1"

	// 只读 GET 的有界重试（§3.20.7：最多 2 次，指数退避，尊重 Retry-After）。
	maxReadRetries  = 2
	retryBaseDelay  = 200 * time.Millisecond
	maxRetryAfter   = 5 * time.Second
	maxErrorRunes   = 200
	maxResponseSize = 4 << 20 // 4 MiB：控制面响应都是小 JSON，超限视为异常
)

// 控制面路径（全部由 apiBasePath 派生，适配器测试逐条锁定）。
const (
	PathHardwareTypes = apiBasePath + "/hardware-types"
	PathLocations     = apiBasePath + "/locations"
)

// Config 是客户端装配输入。BaseURL / Timeout / HTTPClient 只用于测试；
// 生产装配仅提供 APIKey。
type Config struct {
	APIKey string
	// BaseURL 为空时使用 DefaultBaseURL（固定 host）。
	BaseURL string
	// Timeout 为 0 时使用 DefaultTimeout。
	Timeout time.Duration
	// HTTPClient 非 nil 时完全替代默认客户端（测试注入）；生产不要设置，
	// 否则 TLS/重定向守卫可能被绕过。
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Client 是 io.net 控制面客户端，可并发使用。
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	log     *slog.Logger
}

// NewClient 构造客户端。APIKey 允许为空（设置页尚未录入），此时请求会由
// provider 以 401/403 拒绝，映射为 deploy_secret_invalid。
func NewClient(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				TLSClientConfig: &tls.Config{
					// 最低 TLS 1.2（§3.20.7）；不关闭证书校验。
					MinVersion: tls.VersionTLS12,
				},
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout:     90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				// 返回 ErrUseLastResponse 会「不跟随并把 3xx 交给调用方」；这里
				// 直接返回错误更明确：控制面不接受重定向，避免凭据被转移到
				// 另一个 origin 后再由 transport 重新附带。
				return errors.New("ionet: redirects are not allowed")
			},
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{apiKey: cfg.APIKey, baseURL: baseURL, http: httpClient, log: log}
}

// HTTPClient 返回底层客户端（测试断言超时/TLS/重定向策略）。
func (c *Client) HTTPClient() *http.Client { return c.http }

// BaseURL 返回生效的 base URL（测试断言固定 host）。
func (c *Client) BaseURL() string { return c.baseURL }

// DeploymentProvider 是部署控制面的能力接口。T2 只有连接测试所需的三个
// 只读方法；T4 在此基础上增加部署管理方法。
//
// 所有方法都只读 provider 状态，不写库、不产生副作用。
type DeploymentProvider interface {
	TestConnection(ctx context.Context) (ConnectionInfo, error)
	ListHardware(ctx context.Context) (HardwareTypesResponse, error)
	ListLocations(ctx context.Context) (LocationsResponse, error)
}

var _ DeploymentProvider = (*Client)(nil)

// ConnectionInfo 是连接测试的聚合结果。
type ConnectionInfo struct {
	HardwareCount  int
	TotalAvailable int
}

// HardwareType 是硬件类型目录的一行。字段形状待实测冻结（R-M6D-13）：
// 未知字段被忽略，缺失字段保持零值，调用方不得把零值当「确实为 0」。
type HardwareType struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	AvailableReplicas int    `json:"available_replicas"`
}

// HardwareTypesResponse 是 GET hardware-types 的响应。
type HardwareTypesResponse struct {
	HardwareTypes  []HardwareType `json:"hardware_types"`
	Total          int            `json:"total"`
	TotalAvailable int            `json:"total_available"`
}

// Location 是地域目录的一行（形状待实测冻结）。
type Location struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// LocationsResponse 是 GET locations 的响应。
type LocationsResponse struct {
	Locations []Location `json:"locations"`
	Total     int        `json:"total"`
}

// TestConnection 读取硬件类型目录并汇总数量。它是只读探测：不创建、不修改、
// 不落库（§3.20.5）。汇总口径在 provider 未返回 total 字段时回退到列表长度，
// 因此 fake 与实测响应都能工作。
func (c *Client) TestConnection(ctx context.Context) (ConnectionInfo, error) {
	hardware, err := c.ListHardware(ctx)
	if err != nil {
		return ConnectionInfo{}, err
	}
	count := hardware.Total
	if count == 0 {
		count = len(hardware.HardwareTypes)
	}
	available := hardware.TotalAvailable
	if available == 0 {
		for _, item := range hardware.HardwareTypes {
			available += item.AvailableReplicas
		}
	}
	return ConnectionInfo{HardwareCount: count, TotalAvailable: available}, nil
}

// ListHardware 读取硬件类型目录。
func (c *Client) ListHardware(ctx context.Context) (HardwareTypesResponse, error) {
	var out HardwareTypesResponse
	if err := c.get(ctx, PathHardwareTypes, &out); err != nil {
		return HardwareTypesResponse{}, err
	}
	return out, nil
}

// ListLocations 读取地域目录。
func (c *Client) ListLocations(ctx context.Context) (LocationsResponse, error) {
	var out LocationsResponse
	if err := c.get(ctx, PathLocations, &out); err != nil {
		return LocationsResponse{}, err
	}
	return out, nil
}

// APIError 是 provider 非 2xx 响应。Detail 已截断/脱敏，绝不包含 API Key。
type APIError struct {
	StatusCode int
	Detail     string
	// RetryAfter 是 provider 的 Retry-After 建议（只对 429/503 有意义）。
	// 单独字段而不是夹在 Detail 里，便于重试层与 handler 的 429 映射读取。
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("ionet: provider returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("ionet: provider returned HTTP %d: %s", e.StatusCode, e.Detail)
}

// InvalidResponseError 表示 2xx 但响应体不是预期 JSON（映射 502）。
type InvalidResponseError struct{ Err error }

func (e *InvalidResponseError) Error() string {
	return "ionet: invalid provider response"
}
func (e *InvalidResponseError) Unwrap() error { return e.Err }

// get 执行带重试的只读 GET。重试只覆盖网络错误、429 与 5xx；
// 4xx（含 401/403）立即返回，避免把鉴权失败放大成多次请求。
func (c *Client) get(ctx context.Context, path string, out any) error {
	var lastErr error
	for attempt := 0; attempt <= maxReadRetries; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, retryDelay(lastErr, attempt)); err != nil {
				return err
			}
		}
		resp, err := c.doOnce(ctx, http.MethodGet, path)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			lastErr = err
			c.log.Warn("ionet: request failed", "path", path, "attempt", attempt+1, "err", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if len(body) > maxResponseSize {
			return &InvalidResponseError{Err: errors.New("response too large")}
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return &InvalidResponseError{Err: err}
			}
			return nil
		}
		apiErr := c.newAPIError(resp.StatusCode, body, parseRetryAfter(resp.Header.Get("Retry-After")))
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = apiErr
			continue
		}
		return apiErr
	}
	return lastErr
}

// doOnce 发送单次请求。请求始终带 X-API-KEY 与 application/json，即使 GET
// 没有请求体——provider 的控制面契约要求两者同时出现。
func (c *Client) doOnce(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "newtg-deploy/1")
	return c.http.Do(req)
}

// newAPIError 解析并脱敏错误响应。
func (c *Client) newAPIError(status int, body []byte, retryAfter time.Duration) *APIError {
	detail := providerDetail(body)
	if detail == "" {
		detail = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Detail: c.sanitize(detail), RetryAfter: retryAfter}
}

// providerDetail 优先读取 {"detail": ...}，其次 {"message": ...}；
// detail 可能是字符串、对象或数组，统一序列化后再截断。
func providerDetail(body []byte) string {
	var envelope struct {
		Detail  json.RawMessage `json:"detail"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if len(envelope.Detail) > 0 && string(envelope.Detail) != "null" {
		var text string
		if err := json.Unmarshal(envelope.Detail, &text); err == nil {
			return text
		}
		return string(envelope.Detail)
	}
	return envelope.Message
}

// sanitize 截断到 maxErrorRunes 并抹掉 API Key 原文。
//
// 即便 provider 错误体回显了请求头，也不能把凭据带进响应/日志/审计。
func (c *Client) sanitize(detail string) string {
	if c.apiKey != "" {
		detail = strings.ReplaceAll(detail, c.apiKey, "[REDACTED]")
	}
	detail = strings.TrimSpace(detail)
	if utf8.RuneCountInString(detail) > maxErrorRunes {
		runes := []rune(detail)
		detail = string(runes[:maxErrorRunes]) + "…"
	}
	return detail
}

// retryDelay 计算指数退避；若上一次错误是带 Retry-After 的 429/5xx，则取
// max(退避, Retry-After)，但上限 5s 防止 provider 用超大值拖住管理请求。
func retryDelay(lastErr error, attempt int) time.Duration {
	delay := time.Duration(float64(retryBaseDelay) * math.Pow(2, float64(attempt-1)))
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > delay {
		delay = apiErr.RetryAfter
	}
	if delay > maxRetryAfter {
		delay = maxRetryAfter
	}
	return delay
}

// sleepContext 是可取消的退避等待。
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseRetryAfter 只解析秒数形态；HTTP-date 形态按 0 处理（退避仍生效）。
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// IsTimeout 判断错误是否属于「provider 超时」（handler 映射 504）。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
