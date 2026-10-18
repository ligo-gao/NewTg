package adapters

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/ir"
)

const (
	// openAIDefaultBaseURL 是 OpenAI 兼容族的默认基址（relay-02 §5 行 1/§7.1）。
	openAIDefaultBaseURL = "https://api.openai.com"
	// openAIChatPath 是 OpenAI Chat Completions 的相对路径（relay-02 §7.2）。
	openAIChatPath = "/v1/chat/completions"
	// azureDefaultAPIVersion 是 Azure 变体 api_version 的缺省值（relay-02 §7.4）。
	//
	// ⚠️ 渠道 `other.api_version` 的读取在本任务中推迟：冻结的
	// `contract.RelayContext` 没有承载 `other` 的字段（契约 §5 渠道组只有
	// APIBase/Credential/UpstreamModel/...）。执行层扩展上下文后由本适配器读取；
	// 当前一律使用基线缺省值。
	azureDefaultAPIVersion = "2025-04-01-preview"
	// azureNoRemoveDotTime 是 Azure 部署名「点号保留规则」的分界点（relay-02 §7.4）：
	// 此前创建的渠道移除部署名中的 "."，此后创建的保留。判定使用
	// RelayContext.CreatedTime（契约 §5 · 决议 F2）。
	azureNoRemoveDotTime = "2025-05-10"
)

// azureNoRemoveDotTimeValue 是 azureNoRemoveDotTime 的解析值。
// 单独成 var 以免每次构造请求都解析一次日期。
var azureNoRemoveDotTimeValue = func() time.Time {
	value, err := time.Parse("2006-01-02", azureNoRemoveDotTime)
	if err != nil {
		// 常量写错属编程错误；零值会让点号保留（保守方向）。
		return time.Time{}
	}
	return value
}()

// OpenAI 是 OpenAI Chat Completions 适配器（AKOpenAI）。
//
// 它同时服务 openai 与 azure_openai 两个渠道类型（relay-02 §2.5），
// 协议差异（api-key 头 / 部署名路径 / api-version）在 BuildRequest 内按
// RelayContext.ChannelType 分流。
type OpenAI struct {
	// channelType 是实例绑定的渠道类型：契约 §3.1 要求 Azure 变体返回各自类型。
	channelType contract.ChannelType
}

var (
	_ contract.Adapter       = (*OpenAI)(nil)
	_ contract.StreamAdapter = (*OpenAI)(nil)
)

// NewOpenAI 构造绑定到 openai 类型的 OpenAI 适配器。
func NewOpenAI() *OpenAI {
	return &OpenAI{channelType: contract.ChannelTypeOpenAI}
}

// Type 返回实例绑定的渠道类型。
func (o *OpenAI) Type() contract.ChannelType { return o.channelType }

// Caps 返回 OpenAI 兼容族的基础能力声明（relay-02 §5 行 1，brief 冻结值）。
func (o *OpenAI) Caps() contract.Capabilities {
	return contract.Capabilities{
		Endpoints: contract.Set[contract.Endpoint]{
			contract.EndpointChat:   {},
			contract.EndpointModels: {},
		},
		Stream:         true, // SvO：支持 SSE，且显式请求用量
		StreamUsage:    true, // 下发 stream_options.include_usage
		ModelList:      true, // GET /v1/models
		CreditShape:    contract.CreditSingleKey,
		DefaultBaseURL: openAIDefaultBaseURL,
		Roles:          []contract.ChannelRole{contract.RoleChat},
		// BaseURLDefault：有默认基址、渠道可覆盖（relay-02 §7.1）。
		BaseURLMode: contract.BaseURLDefault,
	}
}

// BuildRequest 构造发往上游的 OpenAI Chat Completions 请求（契约 §3.1）。
//
// 幂等：同一 (rc, req) 多次调用产出一致的 URL/头/请求体；http.NewRequest
// 对 bytes.Reader 自动填充 ContentLength 与 GetBody，满足跨传输重试的重放要求
// （relay-02 §8.5）。
//
// GC-6：所有错误信息只含渠道类型/状态码等非敏感信息，绝不包含 base_url 或凭据；
// 底层 URL 解析错误（其文本通常回显 URL）一律不向外透出。
func (o *OpenAI) BuildRequest(rc *contract.RelayContext, req *contract.Request) (*http.Request, error) {
	if rc == nil || req == nil {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: nil relay context or request")
	}
	channelType := rc.ChannelType
	if channelType == contract.ChannelTypeUnknown {
		// 执行层未写渠道类型时退回实例绑定类型（Type() 的语义）。
		channelType = o.channelType
	}
	azure := channelType == contract.ChannelTypeAzure

	// 凭据由执行层每轮写入（契约 §5 渠道组）。缺失属于渠道配置问题。
	if rc.Credential == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel %d has no credential", rc.ChannelID)
	}

	// relay-02 §7.1 的「必填 base_url」类型不得静默落到 OpenAI 默认基址：
	// 那会把凭据发往错误的上游（凭据错向风险）。
	if strings.TrimSpace(rc.APIBase) == "" && requiresExplicitBaseURL(channelType) {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q requires a base URL", channelType)
	}

	// 模型映射后的上游模型名优先；未映射时使用客户端模型名。
	upstreamReq := *req
	if rc.UpstreamModel != "" {
		upstreamReq.Model = rc.UpstreamModel
	}
	body, err := ir.BuildOpenAIRequestBody(&upstreamReq)
	if err != nil {
		// IR 编码错误是请求参数问题（不重试），统一带上契约分类供执行层判定。
		return nil, classifiedf(contract.ErrParamInvalid, "%w", err)
	}

	requestPath := openAIChatPath
	var query url.Values
	if azure {
		// Azure 变体：base_url 已在上方必填校验；路径携带部署名与 api-version。
		deployment := azureDeploymentName(upstreamReq.Model, rc.CreatedTime)
		requestPath = "/openai/deployments/" + url.PathEscape(deployment) + "/chat/completions"
		query = url.Values{}
		query.Set("api-version", azureDefaultAPIVersion)
	}

	baseURL := rc.APIBase
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	var fullURL string
	if channelType == contract.ChannelTypeCustom {
		// 类型 8（custom）：base_url 是**完整 URL**（relay-02 §5 行 8、§7.2），
		// 支持 {model} 占位；不得再拼接 /v1/chat/completions。
		fullURL, err = customURL(baseURL, upstreamReq.Model)
	} else {
		fullURL, err = joinURL(baseURL, requestPath, query)
	}
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		// 底层错误可能回显 URL；按 GC-6 只返回分类，不透出细节。
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: construct upstream request failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	accept := "application/json"
	if req.Stream {
		accept = "text/event-stream"
	}
	httpReq.Header.Set("Accept", accept)
	if azure {
		// Azure 用 api-key 头（relay-02 §8.2）。
		httpReq.Header.Set("api-key", rc.Credential)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+rc.Credential)
	}
	return httpReq, nil
}

// requiredBaseURLTypes 是 relay-02 §7.1 的「必填 base_url」类型集合：
// 3 azure_openai、8 custom、36 sunoapi、45 volcengine、54 doubao_video、
// 60 newapi、61 task_plugin、62 vllm、63 sglang。缺失时校验期即拒绝
// （ErrChannelConfig），不得回落到默认基址。
var requiredBaseURLTypes = map[contract.ChannelType]bool{
	contract.ChannelTypeAzure:       true,
	contract.ChannelTypeCustom:      true,
	contract.ChannelTypeSunoAPI:     true,
	contract.ChannelTypeVolcEngine:  true,
	contract.ChannelTypeDoubaoVideo: true,
	contract.ChannelTypeNewAPI:      true,
	contract.ChannelTypeTaskPlugin:  true,
	contract.ChannelTypeVLLM:        true,
	contract.ChannelTypeSGLang:      true,
}

func requiresExplicitBaseURL(channelType contract.ChannelType) bool {
	return requiredBaseURLTypes[channelType]
}

// customURL 处理类型 8（custom）的整 URL 语义（relay-02 §5 行 8、§7.2）：
// base_url 本身就是完整请求 URL，支持 {model} 占位；结果不再拼接任何路径。
//
// 模型名做 PathEscape 后再替换：模型名是用户可控量，直接拼进 URL 会形成
// 路径注入面（可注入 ? / # / ../）。替换后重新解析校验，错误不回显 URL（GC-6）。
func customURL(baseURL, model string) (string, error) {
	substituted := strings.ReplaceAll(baseURL, "{model}", url.PathEscape(model))
	parsed, err := url.Parse(substituted)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", classifiedf(contract.ErrChannelConfig, "adapters: invalid custom base URL")
	}
	return parsed.String(), nil
}

// azureDeploymentName 实现 Azure 部署名的点号保留规则（relay-02 §7.4）。
//
// CreatedTime 为零值（测试或旧上下文未写入）时保守地保留原样：删除点号会改变
// 部署名，宁可保持上游模型名也不猜测渠道年龄。
func azureDeploymentName(model string, created time.Time) string {
	if created.IsZero() || !created.Before(azureNoRemoveDotTimeValue) {
		return model
	}
	return strings.ReplaceAll(model, ".", "")
}

// joinURL 拼接 base_url 与请求路径（relay-02 §7.2）。
//
// 特殊处理：base_url 已以 /v1 结尾时，去掉请求路径开头的 /v1，避免
// /v1/v1/chat/completions（照搬基线的去重复拼接语义）。
// 解析失败不返回底层错误文本（可能回显 base_url，GC-6）。
func joinURL(baseURL, requestPath string, query url.Values) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", classifiedf(contract.ErrChannelConfig, "adapters: invalid API base URL")
	}
	// 去掉 base 路径尾部斜杠后再判断 /v1 后缀：`https://x/v1/` 与 `https://x/v1`
	// 都必须避免拼出 /v1/v1/chat/completions（relay-02 §7.2）。
	basePath := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(requestPath, "/v1/") {
		requestPath = strings.TrimPrefix(requestPath, "/v1")
	}
	parsed.Path = basePath + requestPath
	if query != nil {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// ParseResponse 解析非流式上游响应（契约 §3.1）。
//
// 非 2xx 不解析响应体：错误只携带状态码与契约分类（GC-6：响应体可能回显
// 凭据/base_url，绝不进入错误串）。流式响应不经过本方法（relay-03 §4.5）。
func (o *OpenAI) ParseResponse(_ *contract.RelayContext, resp *http.Response) (*contract.Response, contract.Usage, error) {
	if resp == nil {
		return nil, contract.Usage{}, classifiedf(contract.ErrUpstreamServer, "adapters: nil upstream response")
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// 读取失败属瞬时故障；不包装底层错误（网络错误可能含 URL）。
		return nil, contract.Usage{}, classifiedf(contract.ErrRetryable, "adapters: read upstream response failed")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		class := o.ClassifyError(resp.StatusCode, body)
		return nil, contract.Usage{}, classifiedf(class, "adapters: upstream returned status %d", resp.StatusCode)
	}
	response, usage, err := ir.ParseOpenAIResponse(body)
	if err != nil {
		// 上游返回了非法 JSON：这是上游服务问题，不应当作客户端参数错误。
		return nil, contract.Usage{}, classifiedf(contract.ErrUpstreamServer, "adapters: invalid upstream response")
	}
	return response, usage, nil
}

// ClassifyError 按 brief/契约 §4 的 OpenAI 映射归类 HTTP 状态。
//
// 边界依据：
//   - 100–199 / 300–399 → 可重试（relay-01 §6.3 状态区间语义；重定向已禁用，
//     3xx 通常意味着上游异常）；
//   - 400/404/422 → 参数非法（404 快失败，brief 冻结）；
//   - 401/403 → 上游鉴权；
//   - 429 → 上游限流；
//   - 5xx → 上游服务器（含 504/524）；
//   - 其余（含 2xx / 未列出的 4xx）→ ErrUnknown，重试决策仍由执行层区间判定。
//
// body 不参与分类：响应体内容不得进入错误路径（GC-6），且分类只依赖状态码。
func (o *OpenAI) ClassifyError(status int, _ []byte) contract.ErrorClass {
	switch {
	case status < 100 || status > 599:
		// 状态码 ∉ [100,599] 视为上游连接异常（relay-01 §6.3 第 3 步：可重试）。
		return contract.ErrRetryable
	case status < 200:
		return contract.ErrRetryable
	case status < 300:
		return contract.ErrUnknown // 2xx 不应进入错误分类
	case status < 400:
		return contract.ErrRetryable
	case status == http.StatusBadRequest, status == http.StatusNotFound, status == http.StatusUnprocessableEntity:
		return contract.ErrParamInvalid
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return contract.ErrUpstreamAuth
	case status == http.StatusRequestTimeout:
		// relay-01 §6.2 把 408 列入「不重试的固定集合」；不得分类为可重试，
		// 否则与执行层的固定集合语义冲突。归为未分类（由执行层固定集合兜底）。
		return contract.ErrUnknown
	case status == http.StatusTooManyRequests:
		return contract.ErrUpstreamRateLimit
	case status >= 500:
		return contract.ErrUpstreamServer
	default:
		return contract.ErrUnknown
	}
}
