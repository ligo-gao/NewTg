package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/egress"
)

// dryRunOutboundTimeout 是 dryrun 最小出站的单次上限。
// 比生产提交的 30s 更短：管理页试运行不应长时间占住请求。
const dryRunOutboundTimeout = 10 * time.Second

// dryRunMaxResponseBytes 是 dryrun 回显响应的上限（与 JSON 控制面同口径）。
const dryRunMaxResponseBytes = 4 << 20

// dryRunEgressConfig 是 dryrun 出站策略的装配点。
//
// 为什么是包级：dryrun 入口在 plugin.Manager 上，而 Manager 的 Options 由
// registry.go 定义；把配置读取面放在 dryrun 自己的文件里，装配层只需在
// 构造 Manager 后调用 ConfigureEgress 一次，不需要改动注册表核心。
var dryRunEgressConfig atomic.Pointer[egress.ConfigReader]

// ConfigureEgress 装配 dryrun 出站策略读取面（生产为 configcenter.Center）。
func ConfigureEgress(cfg egress.ConfigReader) {
	if cfg == nil {
		dryRunEgressConfig.Store(nil)
		return
	}
	dryRunEgressConfig.Store(&cfg)
}

func currentDryRunEgressConfig() egress.ConfigReader {
	if stored := dryRunEgressConfig.Load(); stored != nil {
		return *stored
	}
	return nil
}

// DryRunRequest 是管理端沙箱试运行请求（relay-06 §2.6 `POST /:key/dryrun`）。
//
// 安全语义：默认**零副作用**——不出站、不写库、不扣费。`AllowOutbound=true`
// 时只执行一次**受守卫的最小请求**：目标主机来自 payload.ctx.baseUrl 与
// manifest 的 baseUrl/allowedHosts，仍经 sec.ssrf.* 策略（DNS 级校验、反
// rebinding、禁重定向、私网规则）；返回值只含状态码与响应体，绝不回显
// 请求 URL/查询串或插件上下文里的凭据。
type DryRunRequest struct {
	Key string
	// Version 为空表示使用当前生效层；非空时从 loader 读取该版本源并编译。
	Version string
	// Endpoint 是试运行端点："submit" | "query" | "result" | "submit_response"，
	// 或任意已导出的钩子名（按单参数调用）。
	Endpoint string
	// Payload 是端点参数：submit/query 传 ctx 对象；result 传
	// {ctx, body, status, headers}；submit_response 传 {statusCode, headers, body}。
	Payload any
	// AllowOutbound 为 true 时执行受守卫的最小请求（不再返回
	// dryrun_outbound_unavailable）。
	AllowOutbound bool

	// egressPolicy 仅供同包测试注入带 AllowLoopback 的策略。
	egressPolicy *egress.Policy
}

// DryRunResult 是试运行结果。Result 已做 JSON 往返以保证可序列化
// （插件可能返回函数/循环引用，管理端点不能因此 500）。
type DryRunResult struct {
	Key        string `json:"key"`
	Version    string `json:"version"`
	Endpoint   string `json:"endpoint"`
	Result     any    `json:"result"`
	DurationMS int64  `json:"duration_ms"`
	// Outbound 标记 Result 是受守卫出站的结构化结果（而非钩子原始返回值）。
	Outbound bool `json:"outbound,omitempty"`
}

// DryRun 在指定 key/version 上执行一次钩子。
func (m *Manager) DryRun(ctx context.Context, req DryRunRequest) (*DryRunResult, error) {
	if req.Key == "" {
		return nil, Issues{issuef("", "key", "", "dryrun_required", "dryrun 必须指定 key")}
	}
	plugin, err := m.resolveForDryRun(ctx, req)
	if err != nil {
		return nil, err
	}

	hook, args, err := dryRunCall(req)
	if err != nil {
		return nil, err
	}
	if !plugin.HasHook(hook) {
		return nil, Issues{issuef(req.Key, "hooks."+hook, "", "hook_missing", "插件未导出 %s", hook)}
	}

	start := time.Now()
	value, err := plugin.Call(ctx, hook, args...)
	if err != nil {
		return nil, err
	}
	endpoint := req.Endpoint
	if endpoint == "" {
		endpoint = "submit"
	}
	result := &DryRunResult{
		Key:        plugin.Key,
		Version:    plugin.Version,
		Endpoint:   endpoint,
		DurationMS: time.Since(start).Milliseconds(),
	}

	if !req.AllowOutbound {
		normalized, normErr := jsonNormalize(value)
		if normErr != nil {
			return nil, Issues{issuef(req.Key, "result", "", "dryrun_unserializable",
				"钩子返回值不可 JSON 序列化: %v", normErr)}
		}
		result.Result = normalized
		return result, nil
	}

	outbound, obErr := executeDryRunOutbound(ctx, plugin, req, value)
	if obErr != nil {
		return nil, obErr
	}
	result.Result = outbound
	result.Outbound = true
	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}

// executeDryRunOutbound 把描述符构造成一次受守卫的最小请求。
//
// 只对返回请求描述符的端点有意义；其它端点返回结构化
// `dryrun_outbound_unsupported`（而不是假装执行了出站）。
func executeDryRunOutbound(ctx context.Context, compiled *CompiledPlugin, req DryRunRequest, value any) (any, error) {
	descriptor, ok := value.(map[string]any)
	if !ok {
		return nil, Issues{issuef(req.Key, "result", "", "dryrun_outbound_unsupported",
			"allow_outbound 只支持返回请求描述符的端点")}
	}
	rawURL := strings.TrimSpace(toStringValue(descriptor["url"]))
	if rawURL == "" {
		return nil, Issues{issuef(req.Key, "result.url", "", "dryrun_outbound_unsupported",
			"钩子返回值不是请求描述符（缺 url）")}
	}
	responseType := strings.ToLower(strings.TrimSpace(toStringValue(descriptor["responseType"])))
	if responseType == "sse" {
		return nil, Issues{issuef(req.Key, "result.responseType", "", "dryrun_outbound_unsupported",
			"dryrun 不支持 SSE 响应")}
	}
	method := strings.ToUpper(strings.TrimSpace(toStringValue(descriptor["method"])))
	if method == "" {
		method = http.MethodGet
	}
	credentialless, _ := descriptor["credentialless"].(bool)
	headers := map[string]any{}
	if rawHeaders, ok := descriptor["headers"].(map[string]any); ok {
		headers = rawHeaders
	}
	if credentialless {
		if method != http.MethodGet && method != http.MethodHead {
			return nil, Issues{issuef(req.Key, "result.method", "", "dryrun_outbound_rejected",
				"credentialless 请求只允许 GET/HEAD")}
		}
		if len(headers) != 0 || descriptor["body"] != nil {
			return nil, Issues{issuef(req.Key, "result", "", "dryrun_outbound_rejected",
				"credentialless 请求不得携带头或体")}
		}
	}
	body, bodyErr := dryRunRequestBody(descriptor["body"])
	if bodyErr != nil {
		return nil, bodyErr
	}
	if bytes.Contains(body, []byte(`"__fileRef"`)) {
		return nil, Issues{issuef(req.Key, "result.body", "", "dryrun_outbound_unsupported",
			"dryrun 没有上传文件，不能替换文件占位符")}
	}

	allowedHosts := dryRunAllowedHosts(compiled.Manifest(), req.Payload)
	policy := egress.PolicyFromConfig(currentDryRunEgressConfig())
	if req.egressPolicy != nil {
		policy = *req.egressPolicy
	}
	policy = dryRunPolicyForHosts(policy, allowedHosts, credentialless)
	guard := egress.NewGuard(policy)

	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return nil, Issues{issuef(req.Key, "result.url", "", "dryrun_outbound_rejected",
			"请求 URL 不是合法的绝对地址")}
	}
	if credentialless {
		// credentialless 不套主机允许清单，但仍要求 http(s) 且无 userinfo。
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, Issues{issuef(req.Key, "result.url", "", "dryrun_outbound_rejected",
				"请求 URL 只允许 http/https")}
		}
		if parsed.User != nil {
			return nil, Issues{issuef(req.Key, "result.url", "", "dryrun_outbound_rejected",
				"请求 URL 不得携带 userinfo")}
		}
	} else if _, err := guard.ValidateURLShape(rawURL); err != nil {
		return nil, dryRunEgressIssue(req.Key, err)
	}
	if _, err := guard.ValidateURL(ctx, rawURL); err != nil {
		return nil, dryRunEgressIssue(req.Key, err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, dryRunOutboundTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, Issues{issuef(req.Key, "result.url", "", "dryrun_outbound_rejected",
			"无法构造出站请求")}
	}
	for key, item := range headers {
		request.Header.Set(key, toStringValue(item))
	}
	client := guard.NewClient(egress.ClientOptions{Timeout: dryRunOutboundTimeout})
	resp, err := client.Do(request)
	if err != nil {
		return nil, Issues{issuef(req.Key, "outbound", "", "dryrun_outbound_failed",
			"出站请求失败（目标被策略拒绝或不可达）")}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, dryRunMaxResponseBytes+1))
	if err != nil {
		return nil, Issues{issuef(req.Key, "outbound", "", "dryrun_outbound_failed",
			"读取上游响应失败")}
	}
	if len(payload) > dryRunMaxResponseBytes {
		return nil, Issues{issuef(req.Key, "outbound", "", "dryrun_outbound_failed",
			"上游响应超过 %d 字节", dryRunMaxResponseBytes)}
	}
	// 只回显状态码与响应体；请求 URL/query 与插件上下文凭据永不进入结果。
	out := map[string]any{
		"kind":       "outbound",
		"statusCode": resp.StatusCode,
		"bodyBytes":  len(payload),
	}
	if json.Valid(payload) {
		var parsedBody any
		if err := json.Unmarshal(payload, &parsedBody); err == nil {
			out["body"] = parsedBody
			return out, nil
		}
	}
	out["body"] = string(payload)
	return out, nil
}

func dryRunEgressIssue(key string, err error) error {
	if errors.Is(err, egress.ErrRedirect) {
		return Issues{issuef(key, "outbound", "", "dryrun_outbound_rejected",
			"出站目标返回重定向；dryrun 不跟随重定向")}
	}
	return Issues{issuef(key, "outbound", "", "dryrun_outbound_rejected",
		"出站目标被 SSRF 策略拒绝（地址/域名/端口/协议）")}
}

// dryRunPolicyForHosts 是任务面允许清单在 dryrun 的等价实现。
func dryRunPolicyForHosts(base egress.Policy, allowedHosts []string, credentialless bool) egress.Policy {
	policy := base
	switch {
	case credentialless:
		policy.DomainMode = egress.DomainAll
		policy.DomainList = nil
	case policy.DomainMode != egress.DomainDenylist:
		policy.DomainMode = egress.DomainAllowlist
		policy.DomainList = dryRunHostPatterns(allowedHosts)
	}
	return policy
}

// dryRunHostPatterns 归一 host[:port] 为 egress 匹配用的主机名。
func dryRunHostPatterns(hosts []string) []string {
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

// dryRunAllowedHosts 合并 payload.ctx.baseUrl、manifest.baseUrl 与 allowedHosts。
func dryRunAllowedHosts(manifest *Manifest, payload any) []string {
	out := make([]string, 0, 3)
	add := func(raw string) {
		if parsed, err := url.Parse(strings.TrimSpace(raw)); err == nil && parsed.Host != "" {
			out = append(out, parsed.Host)
		}
	}
	if ctx, ok := payload.(map[string]any); ok {
		add(toStringValue(ctx["baseUrl"]))
	}
	if manifest != nil {
		add(manifest.BaseURL)
		out = append(out, manifest.AllowedHosts...)
	}
	return out
}

func dryRunRequestBody(value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(typed), nil
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return nil, Issues{issuef("", "result.body", "", "dryrun_outbound_rejected",
				"请求体不可 JSON 序列化")}
		}
		return encoded, nil
	}
}

// toStringValue 是 dryrun 的轻量标量转换（描述符字段都是字符串/数字）。
func toStringValue(v any) string {
	switch typed := v.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
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

// resolveForDryRun 选择 dryrun 使用的编译实例。
//
// 指定版本时从 loader 读取源并**重新编译校验**（relay-06 §4.3：dryrun 走
// 同一校验，D 规则违规同样拒绝）；不指定版本时用当前快照的生效层。
func (m *Manager) resolveForDryRun(ctx context.Context, req DryRunRequest) (*CompiledPlugin, error) {
	if req.Version == "" {
		plugin, ok := m.Snapshot().Plugin(req.Key)
		if !ok {
			entry := m.Snapshot().Lookup(req.Key)
			return nil, Issues{issuef(req.Key, "", "", "dryrun_unavailable",
				"插件当前不可执行（status=%s）", entry.Status)}
		}
		return plugin, nil
	}
	if m.loader == nil {
		return nil, Issues{issuef(req.Key, "version", "", "dryrun_unavailable", "未配置版本加载器")}
	}
	version, err := m.loader.LoadVersion(ctx, req.Key, req.Version)
	if err != nil {
		return nil, Issues{issuef(req.Key, "version", "", "dryrun_version_not_found",
			"读取版本 %q 失败: %v", req.Version, err)}
	}
	if version == nil {
		return nil, Issues{issuef(req.Key, "version", "", "dryrun_version_not_found", "版本 %q 不存在", req.Version)}
	}
	plugin, issues := compileOverrideForDryRun(req.Key, *version, m.timeout)
	if issues.HasErrors() {
		return nil, issues
	}
	return plugin, nil
}

func compileOverrideForDryRun(key string, version ActiveVersion, timeout time.Duration) (*CompiledPlugin, Issues) {
	plugin, issues := compileOverride(key, version, timeout)
	if plugin != nil {
		return plugin, nil
	}
	return nil, issues
}

// dryRunCall 把端点名映射到钩子与调用参数。
func dryRunCall(req DryRunRequest) (hook string, args []any, err error) {
	switch req.Endpoint {
	case "", "submit":
		return "buildSubmitRequest", []any{req.Payload}, nil
	case "query":
		return "buildQueryRequest", []any{req.Payload}, nil
	case "submit_response":
		return "parseSubmitResponse", []any{req.Payload}, nil
	case "result":
		// parseTaskResult 的签名是 (ctx, body, {status, headers})；
		// 管理端 payload 用命名对象表达三段，避免调用方依赖位置参数。
		payload, ok := req.Payload.(map[string]any)
		if !ok {
			return "", nil, Issues{issuef(req.Key, "payload", "", "dryrun_payload",
				"result 端点的 payload 必须是 {ctx, body, status, headers} 对象")}
		}
		meta := map[string]any{}
		if v, ok := payload["status"]; ok {
			meta["status"] = v
		}
		if v, ok := payload["headers"]; ok {
			meta["headers"] = v
		}
		return "parseTaskResult", []any{payload["ctx"], payload["body"], meta}, nil
	default:
		// 其它钩子按「单参数」调用，便于沙箱页直接试任意导出钩子。
		return req.Endpoint, []any{req.Payload}, nil
	}
}

// jsonNormalize 通过 JSON 往返把 JS 导出值收敛为纯 JSON 数据。
//
// 意义：Sobek 的 safeExport 可能产出 map/slice 内的宿主对象；管理端点
// （httpapi）必须能直接编码。往返失败明确报错，不静默丢弃字段。
func jsonNormalize(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrDryRunUnavailable 预留给装配层做错误映射（当前用 Issues 表达）。
var ErrDryRunUnavailable = errors.New("plugin: dryrun 不可用")
