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
	// openAIDefaultBaseURL 是 OpenAI 类型的默认基址（relay-02 §5 行 1）。
	// 其余类型的默认基址见 openAITypeSpecs（逐类型表，禁止共享 OpenAI 默认值）。
	openAIDefaultBaseURL = "https://api.openai.com"
	// openAIChatPath 是 OpenAI 兼容族通用的 Chat Completions 路径
	// （relay-02 §7.2）。逐类型特化路径（paas/v4、api/v3、v2、compatible-mode）
	// 见 openAITypeSpecs。
	openAIChatPath = "/v1/chat/completions"
	// maxResponseBodyBytes 是非流式响应体的读取上限（64 MiB）。上游返回超大
	// 响应时按 ErrUpstreamServer 拒绝，避免无界读入拖垮进程（final-review
	// finding 9；与流式事件上限 sseMaxEventBytes 同口径）。
	maxResponseBodyBytes = 64 << 20
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

// openAITypeSpec 是一个 OpenAI 兼容渠道类型的默认基址与 Chat 路径。
//
// 基址为空的类型表示「没有可静默使用的默认值」：必填类型（§7.1 的 9 个）
// 或语义上默认空、留空即不可用的类型（xinference）——两者都在 BuildRequest
// 返回 ErrChannelConfig，绝不回落到 https://api.openai.com（凭据错向风险；
// final-review finding 3）。
type openAITypeSpec struct {
	baseURL  string
	chatPath string
}

// openAITypeSpecs 是逐 ChannelType 的默认基址/路径表（relay-02 §5/§7.1）。
//
// 只覆盖分派到 AKOpenAI 的 24 个类型；未列出的类型（哨兵/其他协议族）由
// BuildRequest 显式拒绝。路径特化依据：
//   - zhipu_v4 → /api/paas/v4/chat/completions（§5 行 26、relay-02 §8 模型路径特化）；
//   - volcengine → /api/v3/chat/completions（§5 行 45、§8 模型路径特化）；
//   - baidu_v2 → /v2/chat/completions（§5 行 46）；
//   - ali → /compatible-mode/v1/chat/completions（DashScope OpenAI 兼容模式，
//     §8 模型路径特化为 /compatible-mode/v1）；
//   - perplexity → /chat/completions（基线 perplexity 适配器不带 /v1）；
//   - custom → 整 URL（§7.2），不拼路径。
var openAITypeSpecs = map[contract.ChannelType]openAITypeSpec{
	contract.ChannelTypeOpenAI:      {baseURL: openAIDefaultBaseURL, chatPath: openAIChatPath},
	contract.ChannelTypeOpenAIMax:   {baseURL: "https://api.openaimax.com", chatPath: openAIChatPath},
	contract.ChannelTypeOhMyGPT:     {baseURL: "https://api.ohmygpt.com", chatPath: openAIChatPath},
	contract.ChannelTypeCustom:      {baseURL: "", chatPath: ""}, // 必填整 URL（§7.2）
	contract.ChannelTypeAILS:        {baseURL: "https://api.caipacity.com", chatPath: openAIChatPath},
	contract.ChannelTypeAIProxy:     {baseURL: "https://api.aiproxy.io", chatPath: openAIChatPath},
	contract.ChannelTypeAPI2GPT:     {baseURL: "https://api.api2gpt.com", chatPath: openAIChatPath},
	contract.ChannelTypeAIGC2D:      {baseURL: "https://api.aigc2d.com", chatPath: openAIChatPath},
	contract.ChannelTypeAli:         {baseURL: "https://dashscope.aliyuncs.com", chatPath: "/compatible-mode/v1/chat/completions"},
	contract.ChannelAi360:           {baseURL: "https://api.360.cn", chatPath: openAIChatPath},
	contract.ChannelTypeOpenRouter:  {baseURL: "https://openrouter.ai/api", chatPath: openAIChatPath},
	contract.ChannelTypeFastGPT:     {baseURL: "https://fastgpt.run/api/openapi", chatPath: openAIChatPath},
	contract.ChannelTypeZhipuV4:     {baseURL: "https://open.bigmodel.cn", chatPath: "/api/paas/v4/chat/completions"},
	contract.ChannelTypePerplexity:  {baseURL: "https://api.perplexity.ai", chatPath: "/chat/completions"},
	contract.ChannelTypeLingYiWanWu: {baseURL: "https://api.lingyiwanwu.com", chatPath: openAIChatPath},
	contract.ChannelTypeSiliconFlow: {baseURL: "https://api.siliconflow.cn", chatPath: openAIChatPath},
	contract.ChannelTypeMistral:     {baseURL: "https://api.mistral.ai", chatPath: openAIChatPath},
	contract.ChannelTypeDeepSeek:    {baseURL: "https://api.deepseek.com", chatPath: openAIChatPath},
	// volcengine 属 §7.1 必填类型（渠道必须提供 base_url），但路径固定为 /api/v3。
	contract.ChannelTypeVolcEngine: {baseURL: "", chatPath: "/api/v3/chat/completions"},
	contract.ChannelTypeBaiduV2:    {baseURL: "https://qianfan.baidubce.com", chatPath: "/v2/chat/completions"},
	// xinference 默认基址空（§5 行 47）：留空不可用，不得回退 OpenAI。
	contract.ChannelTypeXinference: {baseURL: "", chatPath: openAIChatPath},
	contract.ChannelTypeXai:        {baseURL: "https://api.x.ai", chatPath: openAIChatPath},
	contract.ChannelTypeSubmodel:   {baseURL: "https://llm.submodel.ai", chatPath: openAIChatPath},
	// Azure 必填（形如 https://{resource}.openai.azure.com），路径按部署名特化。
	contract.ChannelTypeAzure: {baseURL: "", chatPath: ""},
}

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
//
// DefaultBaseURL 按实例绑定的 ChannelType 取逐类型默认值（§5/§7.1）：
// openrouter 是 https://openrouter.ai/api、zhipu_v4 是 https://open.bigmodel.cn
// 等，而不是统一回落到 https://api.openai.com。必填/默认空类型返回 ""，
// 由渠道表单要求用户填写（§7.1）。
//
// BaseURLMode 必须与 DefaultBaseURL 一致（契约 context.go 的 BaseURLMode 注释）：
// 没有默认基址的必填类型（azure/custom/volcengine）与 xinference 返回
// BaseURLRequired，否则渠道表单会把「必填」误当成「有可覆盖的默认值」；
// 有默认值的类型保持 BaseURLDefault。
func (o *OpenAI) Caps() contract.Capabilities {
	defaultBase := o.defaultBaseURL()
	mode := contract.BaseURLDefault
	if defaultBase == "" {
		mode = contract.BaseURLRequired
	}
	return contract.Capabilities{
		Endpoints: contract.Set[contract.Endpoint]{
			contract.EndpointChat:   {},
			contract.EndpointModels: {},
		},
		Stream:         true, // SvO：支持 SSE，且显式请求用量
		StreamUsage:    true, // 下发 stream_options.include_usage
		ModelList:      true, // GET /v1/models
		CreditShape:    contract.CreditSingleKey,
		DefaultBaseURL: defaultBase,
		Roles:          []contract.ChannelRole{contract.RoleChat},
		// BaseURLDefault：有默认基址、渠道可覆盖（relay-02 §7.1）。
		BaseURLMode: mode,
	}
}

// defaultBaseURL 返回实例渠道类型的默认基址；未知类型返回空串（不臆造）。
func (o *OpenAI) defaultBaseURL() string {
	spec, ok := openAITypeSpecs[o.channelType]
	if !ok {
		return ""
	}
	return spec.baseURL
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

	// 逐类型默认基址/路径（relay-02 §5/§7.1）。表未命中说明该类型不该由
	// 本适配器处理：显式拒绝，绝不隐式回落到 OpenAI 默认基址
	// （final-review finding 3：凭据错向会把渠道 key 发给错误上游）。
	spec, ok := openAITypeSpecs[channelType]
	if !ok {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q has no OpenAI default base URL", channelType)
	}

	// 凭据由执行层每轮写入（契约 §5 渠道组）。缺失属于渠道配置问题。
	if rc.Credential == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel %d has no credential", rc.ChannelID)
	}

	// relay-02 §7.1 的「必填 base_url」类型（以及默认空的 xinference）不得静默
	// 落到 OpenAI 默认基址：那会把凭据发往错误的上游（凭据错向风险）。
	baseInput := strings.TrimSpace(rc.APIBase)
	if baseInput == "" && (requiresExplicitBaseURL(channelType) || spec.baseURL == "") {
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

	// 逐类型路径（relay-02 §5/§7.2）：不能统一用 /v1/chat/completions——
	// zhipu_v4 / volcengine / baidu_v2 / ali 的 OpenAI 兼容端点各有前缀。
	requestPath := spec.chatPath
	var query url.Values
	if azure {
		// Azure 变体：base_url 已在上方必填校验；路径携带部署名与 api-version。
		deployment := azureDeploymentName(upstreamReq.Model, rc.CreatedTime)
		requestPath = "/openai/deployments/" + url.PathEscape(deployment) + "/chat/completions"
		query = url.Values{}
		query.Set("api-version", azureDefaultAPIVersion)
	}

	baseURL := baseInput
	if baseURL == "" {
		baseURL = spec.baseURL // 上方已保证：无默认值（必填/默认空）时不会走到这里
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
// {model} 的转义必须按占位符所在组件区分（final-review finding 6）：
// url.PathEscape 不转义 `&`/`=`，若占位符位于 query 中，模型名
// `a&admin=1` 会凭空注入一个 admin 参数。因此：
//   - authority 中的占位符**一律拒绝**（security-04 §2.4：替换不得改变主机）：
//     模型名来自客户端请求，若允许落进 authority，客户端就能选择 host/port
//     （例如 `10.0.0.5:8080`），构成 SSRF / 上游凭据外泄原语，详见
//     authorityHasModelPlaceholder；
//   - path/fragment 中的占位符用 PathEscape（转义 ? / # / %，保留 &/=（路径里安全））；
//   - query 中的占位符用 QueryEscape（转义 & / = / ? / #，杜绝参数注入）。
//
// 实现细节：先用原始 base_url 解析出组件位置，再分别在 EscapedPath() 与
// RawQuery 上替换。EscapedPath 会把花括号编码成 %7B/model/%7D，所以两种
// 写法都要匹配（调用方也可能手写已编码的占位符）。替换后的 path 回填
// Path/RawPath：RawPath 保留转义形态，URL.String 才不会把 PathEscape 的
// `%` 二次转义成 `%25`。错误不回显 URL（GC-6）。
func customURL(baseURL, model string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	// security-04 §2.4：{model} 替换不得改变主机。模型名由客户端请求控制
	// （OriginModel/UpstreamModel），若允许它落进 authority，客户端就能选择
	// host/port（例如模型名 `10.0.0.5:8080`），把网关变成访问内网的跳板或把
	// 渠道凭据发往攻击者主机。authority 没有安全的转义机制（net/url 拒绝 host
	// 中的 `{` 与 %XX），因此不做任何替换：模板在 authority 出现占位符就是
	// 渠道配置错误（ErrChannelConfig，不重试），而不是逐请求拒绝模型名。
	if authorityHasModelPlaceholder(baseURL) {
		return "", classifiedf(contract.ErrChannelConfig, "adapters: model placeholder not allowed in custom base URL authority")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", classifiedf(contract.ErrChannelConfig, "adapters: invalid custom base URL")
	}

	if hasModelPlaceholder(parsed.Path) {
		escapedPath := replaceModelPlaceholder(parsed.EscapedPath(), url.PathEscape(model))
		decodedPath, unescapeErr := url.PathUnescape(escapedPath)
		if unescapeErr != nil {
			return "", classifiedf(contract.ErrChannelConfig, "adapters: invalid custom base URL")
		}
		parsed.Path = decodedPath
		parsed.RawPath = escapedPath
	}
	if hasModelPlaceholder(parsed.RawQuery) {
		// QueryEscape 把空格转成 `+`（query 规范形态），& / = / # 全部转义。
		parsed.RawQuery = replaceModelPlaceholder(parsed.RawQuery, url.QueryEscape(model))
	}
	if hasModelPlaceholder(parsed.Fragment) {
		// Fragment 不发送给上游，但仍按路径规则转义，避免残留占位符。
		// 修复 double-escape：url.PathEscape 的输出若直接赋给 Fragment，
		// URL.String 会再按 fragment 规则把 `%` 转义成 `%25`（%20 → %2520）。
		// RawFragment 保存已转义形态且其解码值等于 Fragment，String 便原样使用。
		original := parsed.Fragment
		parsed.Fragment = replaceModelPlaceholder(original, model)
		parsed.RawFragment = replaceModelPlaceholder(original, url.PathEscape(model))
	}

	return parsed.String(), nil
}

// hasModelPlaceholder 判断组件是否含 {model} 占位符。
// url.Parse 会解码 path/fragment，raw query 保持原样，因此两种写法都要匹配
// （调用方也可能手写已编码的占位符）。
func hasModelPlaceholder(component string) bool {
	return strings.Contains(component, "{model}") || strings.Contains(component, "%7Bmodel%7D") ||
		strings.Contains(component, "%7bmodel%7d")
}

// replaceModelPlaceholder 把组件中的 {model} 占位符替换为 replacement。
func replaceModelPlaceholder(component, replacement string) string {
	return strings.NewReplacer(
		"%7Bmodel%7D", replacement,
		"%7bmodel%7d", replacement,
		"{model}", replacement,
	).Replace(component)
}

// authorityHasModelPlaceholder 判断 base_url 的 authority（host[:port]）段是否
// 含 {model} 占位符。
//
// security-04 §2.4 要求 {model} 替换不得改变主机：host 组件没有任何可用的
// 转义机制，模型名只能原样落位；而模型名是客户端可控输入，允许替换就等于
// 允许客户端选择主机与端口（`{model}` → `10.0.0.5:8080`）。与其尝试清洗
// 模型名（黑名单永远追不上 URL 解析的边角），这里直接拒绝整个模板：
// 该渠道配置不可用（ErrChannelConfig，不重试），运行期请求永远到不了这一步。
//
// scheme 缺失时返回 false：没有 authority 可供替换，后续 url.Parse 会按非法
// URL 拒绝。检查在 url.Parse **之前**进行：net/url 对 host 中的 `{` 报
// invalid character，若先解析就无法区分「占位符」与「损坏的 URL」。
func authorityHasModelPlaceholder(baseURL string) bool {
	schemeEnd := strings.Index(baseURL, "://")
	if schemeEnd < 0 {
		return false
	}
	authorityStart := schemeEnd + len("://")
	authorityEnd := len(baseURL)
	if stop := strings.IndexAny(baseURL[authorityStart:], "/?#"); stop >= 0 {
		authorityEnd = authorityStart + stop
	}
	return hasModelPlaceholder(baseURL[authorityStart:authorityEnd])
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
	// 有界读取（final-review finding 9）：64 MiB 上限，超限按上游故障拒绝，
	// 避免恶意/异常上游用无限响应体耗尽进程内存（与流式 64 MiB 事件上限同口径）。
	body, err := readUpstreamBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return nil, contract.Usage{}, err
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

// readUpstreamBody 有界读取非流式响应体。
//
// 多读 1 字节用于区分「恰好等于上限」与「超限」：恰好 64 MiB 是合法响应，
// 65 MiB（及以上）按 ErrUpstreamServer 拒绝。读取本身的 I/O 错误保留
// ErrRetryable（瞬时故障，执行层可换渠道重试）；错误文本不含 URL/响应体（GC-6）。
func readUpstreamBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, classifiedf(contract.ErrRetryable, "adapters: read upstream response failed")
	}
	if int64(len(body)) > limit {
		return nil, classifiedf(contract.ErrUpstreamServer, "adapters: upstream response exceeds %d bytes", limit)
	}
	return body, nil
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
