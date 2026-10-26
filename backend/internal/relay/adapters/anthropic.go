package adapters

// Anthropic Messages API 原生适配器（AKAnthropicNative）。
//
// 净室实现：只按公开协议规格用 encoding/json + net/http 构造/解析，
// 不引入任何外部 SDK（M4 约束）。
//
// 协议差异全部收敛在本文件与 `relay/ir/anthropic.go`：
//   - relay-02 §5 行 14（anthropic）/ 行 25（moonshot，Anthropic 原生）；
//   - relay-02 §8.2：凭据走 `x-api-key` 头（不是 Bearer）；
//   - relay-03 §6.2 row 4：usage 字段路径与关系布尔。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/ir"
)

const (
	// anthropicDefaultBaseURL 是类型 14 的默认基址（relay-02 §5 行 14）。
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	// moonshotDefaultBaseURL 是类型 25 的默认基址（relay-02 §5 行 25）。
	moonshotDefaultBaseURL = "https://api.moonshot.cn"
	// moonshotAnthropicPrefix 是 Moonshot 的 Anthropic 兼容路径前缀。
	// 基线把 `https://api.moonshot.cn` 与 `/anthropic/v1/messages` 拼接；
	// 若渠道 base_url 已包含 /anthropic，joinAnthropicURL 会去重。
	moonshotAnthropicPrefix = "/anthropic"
	// anthropicMessagesPath 是 Messages API 路径。
	anthropicMessagesPath = "/v1/messages"
	// anthropicCountTokensPath 是 Messages 的零计费计数端点（R-W18-06）。
	// 与 messages 同基址同鉴权，只是路径后缀不同。
	anthropicCountTokensPath = "/v1/messages/count_tokens"
	// anthropicVersion 是 Anthropic 要求的协议版本头。
	// 2023-06-01 是当前 Messages API 的稳定版本；不随请求变化。
	anthropicVersion = "2023-06-01"
)

// anthropicBetaAllowlist 是**允许转发**给上游的 `anthropic-beta` 值集合
// （R-W18-05 冻结裁决）。
//
// 为什么是白名单而不是透传：`anthropic-beta` 是开放集合，客户端可以放置任意
// 字符串。无条件转发等于让调用方决定「网关向上游声明什么能力」——既可能触发
// 网关尚未理解的上游行为（例如改变响应形状），也让上游把网关当成任意 beta
// 的透传代理。白名单只放行网关已核实的、形状固定的稳定 beta 值；未列出的值
// 静默丢弃（请求仍按无 beta 处理，由上游按常规语义响应或 4xx，绝不报错——
// 报错会泄露白名单内容）。
//
// 运营化说明：本集合是代码内固定值（新增/删除需发版）。之所以不挂
// settings 注册表：注册表要求同时回写 docs/system-config-mapping.md 与
// modules-08 §1.4.1（registry.go 的 buildRegistry 注释），本任务不修改设计
// 文档；后续运营化时应新增 `relay.anthropic_beta_allowlist`（TypeList），
// 以「代码默认 ∪ 配置追加」合并，而不是替换默认集合。
//
// 取值均为 Anthropic 公开文档中的 beta 标识；不含 Authorization / Cookie /
// hop-by-hop 头——本表只作用于 `anthropic-beta` 这一个头。
var anthropicBetaAllowlist = map[string]struct{}{
	"messages-2023-12-15":                      {},
	"prompt-caching-2024-07-31":                {},
	"computer-use-2024-10-22":                  {},
	"computer-use-2025-01-24":                  {},
	"token-counting-2024-11-01":                {},
	"max-tokens-3-5-sonnet-2024-07-15":         {},
	"output-128k-2025-02-19":                   {},
	"interleaved-thinking-2025-05-14":          {},
	"extended-cache-ttl-2025-04-11":            {},
	"fine-grained-tool-streaming-2025-05-14":   {},
	"context-1m-2025-08-07":                    {},
	"mcp-client-2025-04-04":                    {},
	"code-execution-2025-05-22":                {},
	"files-api-2025-04-14":                     {},
	"mid-conversation-tool-changes-2026-07-01": {},
	"inline-tools-2026-09-15":                  {},
}

// FilterAnthropicBeta 对客户端 `anthropic-beta` 原始值做白名单过滤。
//
// 输入是 HTTP 边界收集到的原始串（可能来自多个同名头，调用方用逗号拼接；
// 也可能一个头里带逗号分隔的多个值）。输出是保序去重后的逗号分隔串；
// 全部值被丢弃时返回空串，适配器据此**不发送**该头（空头不等于「无 beta」）。
//
// 只做精确匹配：beta 标识是版本化的不透明字面量，前缀/大小写归一化会把
// 未核实的新值悄悄放行。
func FilterAnthropicBeta(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	seen := make(map[string]struct{}, 4)
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		if _, ok := anthropicBetaAllowlist[value]; !ok {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		kept = append(kept, value)
	}
	return strings.Join(kept, ", ")
}

// anthropicTypeSpec 是 Anthropic 原生族逐渠道类型的默认基址与路径前缀。
// 未列出的类型显式拒绝，绝不回落到 Anthropic 官方基址（凭据错向风险）。
type anthropicTypeSpec struct {
	baseURL    string
	pathPrefix string
}

var anthropicTypeSpecs = map[contract.ChannelType]anthropicTypeSpec{
	contract.ChannelTypeAnthropic: {baseURL: anthropicDefaultBaseURL},
	contract.ChannelTypeMoonshot:  {baseURL: moonshotDefaultBaseURL, pathPrefix: moonshotAnthropicPrefix},
}

// Anthropic 是 Anthropic Messages API 适配器。
type Anthropic struct {
	channelType contract.ChannelType
}

var (
	_ contract.Adapter       = (*Anthropic)(nil)
	_ contract.StreamAdapter = (*Anthropic)(nil)
)

// NewAnthropicAdapter 构造绑定到给定渠道类型的 Anthropic 适配器
// （ledger 冻结签名；anthropic 与 moonshot 共用实现）。
func NewAnthropicAdapter(channelType contract.ChannelType) contract.Adapter {
	return &Anthropic{channelType: channelType}
}

func (a *Anthropic) Type() contract.ChannelType { return a.channelType }

// Caps 返回 Anthropic 原生族的能力声明（relay-02 §3/§5 行 14）。
//
// Endpoints 同时声明 chat 与 messages：本系统对外的对话入口是 chat，
// messages 是该渠道的原生协议端点；模型列表走 GET /v1/models。
//
// StreamUsage=false：Anthropic 流式恒带 usage，没有 OpenAI 的
// `stream_options.include_usage` 请求项；若置 true，执行层会向下游注入
// OpenAI 专用字段，导致上游 400。
// StreamAggregatesUsage=true：input_tokens 在 message_start、
// output_tokens 在 message_delta，必须跨块合并（relay-03 §6.2 row 4）。
func (a *Anthropic) Caps() contract.Capabilities {
	spec, ok := anthropicTypeSpecs[a.channelType]
	base := ""
	if ok {
		base = spec.baseURL
	}
	mode := contract.BaseURLDefault
	if base == "" {
		mode = contract.BaseURLRequired
	}
	return contract.Capabilities{
		Endpoints: contract.Set[contract.Endpoint]{
			contract.EndpointChat:     {},
			contract.EndpointMessages: {},
			contract.EndpointModels:   {},
		},
		Stream:                true,
		StreamUsage:           false,
		StreamAggregatesUsage: true,
		ModelList:             true,
		CreditShape:           contract.CreditSingleKey,
		DefaultBaseURL:        base,
		Roles:                 []contract.ChannelRole{contract.RoleChat},
		BaseURLMode:           mode,
	}
}

// BuildRequest 构造发往 Anthropic /v1/messages 的请求。
//
// 幂等：同一 (rc, req) 多次调用产出等价请求；bytes.Reader 让
// http.NewRequest 自动填充 ContentLength/GetBody，满足跨传输重放。
// GC-6：错误信息只含渠道类型/渠道 ID，不含 base_url 或凭据。
func (a *Anthropic) BuildRequest(rc *contract.RelayContext, req *contract.Request) (*http.Request, error) {
	if rc == nil || req == nil {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: nil relay context or request")
	}
	channelType := rc.ChannelType
	if channelType == contract.ChannelTypeUnknown {
		channelType = a.channelType
	}
	spec, ok := anthropicTypeSpecs[channelType]
	if !ok {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q has no Anthropic base URL", channelType)
	}
	if rc.Credential == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel %d has no credential", rc.ChannelID)
	}

	baseURL := strings.TrimSpace(rc.APIBase)
	if baseURL == "" {
		baseURL = spec.baseURL
	}
	if baseURL == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q requires a base URL", channelType)
	}

	// 模型映射后的上游模型名优先；未映射时使用客户端模型名。
	upstreamReq := *req
	if rc.UpstreamModel != "" {
		upstreamReq.Model = rc.UpstreamModel
	}
	if strings.TrimSpace(upstreamReq.Model) == "" {
		return nil, classifiedf(contract.ErrParamInvalid, "adapters: model is empty")
	}
	body, err := ir.BuildAnthropicRequestBody(&upstreamReq)
	if err != nil {
		return nil, classifiedf(contract.ErrParamInvalid, "%w", err)
	}

	fullURL, err := joinAnthropicURL(baseURL, spec.pathPrefix)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		// 底层错误可能回显 URL；按 GC-6 只返回分类。
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: construct upstream request failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	accept := "application/json"
	if req.Stream {
		accept = "text/event-stream"
	}
	httpReq.Header.Set("Accept", accept)
	// Anthropic 的不是 Bearer：凭据必须放 x-api-key（relay-02 §8.2）。
	httpReq.Header.Set("x-api-key", rc.Credential)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	// R-W18-05：客户端 beta 头按白名单转发。值已在 HTTP 边界过滤一次；
	// 这里再过滤一次是防御性冗余（契约字段可能被其它内部调用方直接填写）。
	// 仅在结果非空时设置头：空头与「没有 beta」在上游语义上不同。
	if beta := FilterAnthropicBeta(req.AnthropicBeta); beta != "" {
		httpReq.Header.Set("anthropic-beta", beta)
	}
	return httpReq, nil
}

// BuildCountTokensRequest 构造上游 `/v1/messages/count_tokens` 请求（R-W18-06）。
//
// 本方法不属于冻结的 contract.Adapter 接口：count_tokens 是 Anthropic 协议族
// 专有的零计费端点，用 httpapi 侧的窄接口断言比扩充冻结契约更小。body 是
// 客户端原始请求体（已由 handler 读取），这里只做模型映射改写——其余字段
// 原样透传，因为精确计数必须基于客户端真实的 messages/system/tools 内容。
func (a *Anthropic) BuildCountTokensRequest(rc *contract.RelayContext, body []byte) (*http.Request, error) {
	if rc == nil || len(body) == 0 {
		return nil, classifiedf(contract.ErrParamInvalid, "adapters: empty count_tokens body")
	}
	channelType := rc.ChannelType
	if channelType == contract.ChannelTypeUnknown {
		channelType = a.channelType
	}
	spec, ok := anthropicTypeSpecs[channelType]
	if !ok {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q has no Anthropic base URL", channelType)
	}
	if rc.Credential == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel %d has no credential", rc.ChannelID)
	}
	baseURL := strings.TrimSpace(rc.APIBase)
	if baseURL == "" {
		baseURL = spec.baseURL
	}
	if baseURL == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q requires a base URL", channelType)
	}

	payload := body
	if rc.UpstreamModel != "" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, classifiedf(contract.ErrParamInvalid, "adapters: invalid count_tokens body")
		}
		modelJSON, err := json.Marshal(rc.UpstreamModel)
		if err != nil {
			return nil, classifiedf(contract.ErrParamInvalid, "adapters: invalid upstream model")
		}
		fields["model"] = modelJSON
		payload, err = json.Marshal(fields)
		if err != nil {
			return nil, classifiedf(contract.ErrParamInvalid, "adapters: invalid count_tokens body")
		}
	}

	fullURL, err := joinAnthropicURLPath(baseURL, spec.pathPrefix, anthropicCountTokensPath)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: construct upstream request failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-api-key", rc.Credential)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if beta := FilterAnthropicBeta(rc.AnthropicBeta); beta != "" {
		httpReq.Header.Set("anthropic-beta", beta)
	}
	return httpReq, nil
}

// ParseCountTokensResponse 解析上游 count_tokens 响应（R-W18-06）。
//
// 只接受 Anthropic 的精确形状 `{"input_tokens": N}`：缺字段/负数/非 JSON 一律
// 按上游服务问题（ErrUpstreamServer）拒绝。绝不在这里做本地估算——裁决明确
// 「不引入本地 tokenizer」，估算值冒充精确值会让客户端的上下文预算判断失真。
func (a *Anthropic) ParseCountTokensResponse(resp *http.Response) (int64, error) {
	if resp == nil {
		return 0, classifiedf(contract.ErrUpstreamServer, "adapters: nil count_tokens response")
	}
	body, err := readUpstreamBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		class := a.ClassifyError(resp.StatusCode, body)
		return 0, classifiedf(class, "adapters: upstream returned status %d", resp.StatusCode)
	}
	var parsed struct {
		InputTokens *int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.InputTokens == nil || *parsed.InputTokens < 0 {
		return 0, classifiedf(contract.ErrUpstreamServer, "adapters: invalid count_tokens response")
	}
	return *parsed.InputTokens, nil
}

// joinAnthropicURL 拼接 base_url + [类型前缀] + /v1/messages。
func joinAnthropicURL(baseURL, typePrefix string) (string, error) {
	return joinAnthropicURLPath(baseURL, typePrefix, anthropicMessagesPath)
}

// joinAnthropicURLPath 拼接 base_url + [类型前缀] + apiPath。
//
// 去重规则：
//   - base_url 已以 /v1 结尾时不再重复 /v1（避免 /v1/v1/messages）；
//   - base_url 已含类型前缀（如 moonshot 的 /anthropic）时不再重复。
//
// 解析失败不返回底层文本（可能回显 base_url，GC-6）。
func joinAnthropicURLPath(baseURL, typePrefix, apiPath string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", classifiedf(contract.ErrChannelConfig, "adapters: invalid API base URL")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	prefix := strings.TrimRight(typePrefix, "/")
	// 前缀去重：base_url 以 /anthropic 结尾，或已在路径中包含 /anthropic/…
	// （如 https://api.moonshot.cn/anthropic/v1）时都不再重复。
	if prefix != "" && (strings.HasSuffix(basePath, prefix) || strings.Contains(basePath, prefix+"/")) {
		prefix = ""
	}
	path := prefix + apiPath
	if strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	parsed.Path = basePath + path
	return parsed.String(), nil
}

// ParseResponse 解析非流式 Messages 响应（契约 §3.1）。
func (a *Anthropic) ParseResponse(_ *contract.RelayContext, resp *http.Response) (*contract.Response, contract.Usage, error) {
	if resp == nil {
		return nil, contract.Usage{}, classifiedf(contract.ErrUpstreamServer, "adapters: nil upstream response")
	}
	body, err := readUpstreamBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		class := a.ClassifyError(resp.StatusCode, body)
		return nil, contract.Usage{}, classifiedf(class, "adapters: upstream returned status %d", resp.StatusCode)
	}
	response, usage, err := ir.ParseAnthropicResponse(body)
	if err != nil {
		// 上游返回非法/未知块形状：这是上游服务问题，不是客户端参数错误。
		return nil, contract.Usage{}, classifiedf(contract.ErrUpstreamServer, "adapters: invalid upstream response")
	}
	return response, usage, nil
}

// ClassifyError 与 M2 OpenAI 适配器同口径（errors.go/relay-01 §6.3）。
func (a *Anthropic) ClassifyError(status int, _ []byte) contract.ErrorClass {
	return classifyUpstreamStatus(status)
}

// ============================================================================
// 流式：Anthropic SSE → StreamEvent
// ============================================================================

// anthropicStreamEvent 是 SSE data 的解码形状。
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
		Text string `json:"text"`
		// redacted_thinking 的加密负载随 content_block_start 一次给出。
		Data string `json:"data"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		// signature_delta 携带思考块签名；它不是正文，不能用 Delta 通道。
		Signature  string `json:"signature"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
}

// StreamEvents 逐帧解析 Anthropic SSE 流。
//
// 事件映射（relay-03 §6.2 row 4）：
//   - message_start：usage（input/cache）→ EventUsage；记录模型名；
//   - content_block_delta：text_delta → EventDelta.Delta；
//     input_json_delta → EventDelta.ToolDelta；thinking_delta → Reasoning；
//   - content_block_start(tool_use)：工具的 id/name 首片；
//   - message_delta：usage（output_tokens）→ EventUsage；stop_reason → EventFinish；
//   - message_stop：兜底 EventFinish；
//   - error：EventError；ping：EventPing。
//
// 未知事件类型直接忽略（不 panic，不伪造内容）——Anthropic 会按需新增事件，
// 把它们变成硬错误会让旧网关在新上游面前整体不可用。读取/超限错误以
// 硬 EventError（Raw=nil）交付，错误文本不含 URL/凭据（GC-6）。
func (a *Anthropic) StreamEvents(_ *contract.RelayContext, rw io.ReadWriter) iter.Seq[contract.StreamEvent] {
	return anthropicStreamEvents(rw, sseMaxEventBytes)
}

func anthropicStreamEvents(rw io.ReadWriter, maxEventBytes int) iter.Seq[contract.StreamEvent] {
	return func(yield func(contract.StreamEvent) bool) {
		usageAcc := contract.Usage{
			// 关系布尔是协议属性：即使上游没有给 usage 也必须显式赋值
			// （契约 §2.3：方向搞反会成倍错算费用）。
			PromptIncludesCache:           false,
			ReasoningIncludedInCompletion: true,
			Source:                        contract.UsageEstimated,
		}
		model := ""
		finishSent := false

		emitFinish := func(raw []byte) bool {
			if finishSent {
				return true
			}
			finishSent = true
			return yield(contract.StreamEvent{Kind: contract.EventFinish, Model: model, Raw: raw})
		}

		for frame := range sseFrameSeq(rw, maxEventBytes) {
			if frame.hard {
				// 硬错误形状：Raw 必须为空，端点层据此终止流而不是计软错误。
				yield(contract.StreamEvent{Kind: contract.EventError})
				return
			}
			payload := strings.TrimSpace(frame.data)
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				emitFinish(frame.raw)
				return
			}
			var event anthropicStreamEvent
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				// 单块 JSON 损坏是软错误：交出原始帧，流继续（relay-03 §4.3）。
				if !yield(contract.StreamEvent{Kind: contract.EventError, Model: model, Raw: frame.raw}) {
					return
				}
				continue
			}
			eventType := event.Type
			if eventType == "" {
				eventType = frame.event
			}
			switch eventType {
			case "message_start":
				if event.Message != nil {
					if event.Message.Model != "" {
						model = event.Message.Model
					}
					if len(event.Message.Usage) > 0 && string(event.Message.Usage) != "null" {
						parsed := ir.ParseAnthropicUsage(event.Message.Usage)
						mergeAnthropicStreamUsage(&usageAcc, parsed)
						if !yield(contract.StreamEvent{Kind: contract.EventUsage, Usage: usageCopy(usageAcc), Model: model, Raw: frame.raw}) {
							return
						}
					}
				}
			case "content_block_start":
				if event.ContentBlock != nil {
					if event.ContentBlock.Type == "tool_use" {
						// 工具调用的 id/name 只在首片出现，必须单独交出。
						delta := contract.ToolCallDelta{
							Index: event.Index,
							ID:    event.ContentBlock.ID,
							Name:  event.ContentBlock.Name,
						}
						if !yield(contract.StreamEvent{Kind: contract.EventDelta, ToolDelta: &delta, Model: model, Raw: frame.raw}) {
							return
						}
					} else if event.ContentBlock.Type != "text" && event.ContentBlock.Type != "thinking" {
						// redacted_thinking（加密思考）与未来新增块没有正文增量可
						// 表达；交出原始帧，绝不静默丢弃（复审 P1）。
						if !yield(contract.StreamEvent{Kind: contract.EventRaw, Model: model, Raw: frame.raw}) {
							return
						}
					}
				}
			case "content_block_delta":
				switch event.Delta.Type {
				case "text_delta":
					if event.Delta.Text != "" {
						if !yield(contract.StreamEvent{Kind: contract.EventDelta, Delta: event.Delta.Text, Model: model, Raw: frame.raw}) {
							return
						}
					}
				case "input_json_delta":
					delta := contract.ToolCallDelta{
						Index:             event.Index,
						ArgumentsFragment: event.Delta.PartialJSON,
					}
					if !yield(contract.StreamEvent{Kind: contract.EventDelta, ToolDelta: &delta, Model: model, Raw: frame.raw}) {
						return
					}
				case "thinking_delta":
					if event.Delta.Thinking != "" {
						// 思考增量走独立通道，不混入正文（契约 §2.4）。
						if !yield(contract.StreamEvent{Kind: contract.EventDelta, Reasoning: event.Delta.Thinking, Model: model, Raw: frame.raw}) {
							return
						}
					}
				default:
					// signature_delta / citations_delta / 未来新增的 delta 类型：
					// 没有对应的语义通道，但原始帧必须保留（复审 P1）。
					if !yield(contract.StreamEvent{Kind: contract.EventRaw, Model: model, Raw: frame.raw}) {
						return
					}
				}
			case "message_delta":
				if len(event.Usage) > 0 && string(event.Usage) != "null" {
					parsed := ir.ParseAnthropicUsage(event.Usage)
					mergeAnthropicStreamUsage(&usageAcc, parsed)
					if !yield(contract.StreamEvent{Kind: contract.EventUsage, Usage: usageCopy(usageAcc), Model: model, Raw: frame.raw}) {
						return
					}
				}
				if event.Delta.StopReason != "" && !finishSent {
					if !emitFinish(frame.raw) {
						return
					}
				}
			case "message_stop":
				if !emitFinish(frame.raw) {
					return
				}
				return
			case "ping":
				if !yield(contract.StreamEvent{Kind: contract.EventPing, Model: model, Raw: frame.raw}) {
					return
				}
			case "error":
				// 上游错误事件：只交原始帧，不构造可能泄露敏感信息的消息串。
				if !yield(contract.StreamEvent{Kind: contract.EventError, Model: model, Raw: frame.raw}) {
					return
				}
			default:
				// 未知事件：忽略负载语义，不 panic。
			}
		}
	}
}

// mergeAnthropicStreamUsage 把后到的非零字段合并进累计用量。
//
// message_start 给 input/cache，message_delta 只给 output_tokens；
// 「只覆盖非零字段」保证 output 不会把 input 擦成 0（relay-03 §6.1 规则 4）。
func mergeAnthropicStreamUsage(dst *contract.Usage, src contract.Usage) {
	if src.Prompt != 0 {
		dst.Prompt = src.Prompt
	}
	if src.Completion != 0 {
		dst.Completion = src.Completion
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheWrite != 0 {
		dst.CacheWrite = src.CacheWrite
	}
	dst.Total = dst.Prompt + dst.Completion
	dst.PromptDetail = contract.UsageDetail{
		Text:       dst.Prompt,
		CacheRead:  dst.CacheRead,
		CacheWrite: dst.CacheWrite,
	}
	dst.CompletionDetail = contract.UsageDetail{Text: dst.Completion}
	if src.Source == contract.UsageUpstreamExact {
		dst.Source = contract.UsageUpstreamExact
	}
	// Raw 逐块合并：后到的键覆盖同名字段，保留所有原始键供对账。
	if src.Raw != nil {
		if dst.Raw == nil {
			dst.Raw = map[string]any{}
		}
		for key, value := range src.Raw {
			dst.Raw[key] = value
		}
	}
}

// usageCopy 返回一份可安全交给调用方的用量副本（Raw 也复制）。
func usageCopy(usage contract.Usage) *contract.Usage {
	out := usage
	if usage.Raw != nil {
		out.Raw = make(map[string]any, len(usage.Raw))
		for key, value := range usage.Raw {
			out.Raw[key] = value
		}
	}
	return &out
}

// ============================================================================
// 共用 SSE 帧解析（Anthropic/Gemini）
// ============================================================================

// sseFrame 是一个已聚合的 SSE 事件。
type sseFrame struct {
	event string
	data  string
	raw   []byte
	// hard=true 表示读取/超限等硬错误：调用方必须交付 Raw=nil 的 EventError。
	hard bool
}

// sseFrameSeq 按 SSE 规范切分事件：空行分界、同一事件的多个 data: 行用
// "\n" 合并、event:/id:/retry:/注释行保留在 raw 中。
//
// 与 M2 openai_stream.go 的帧解析同口径（relay-03 §4.2 甲类修正），
// 但参数化以复用：OpenAI 适配器保留自己的实现，本文件服务两个原生适配器。
// 【复审 #9 登记】两份实现的完全合并留待后续任务；本轮不改 OpenAI（不动 M2），
// 但两处对空行/多 data 行/超限的处理必须保持一致。
func sseFrameSeq(rw io.ReadWriter, maxEventBytes int) iter.Seq[sseFrame] {
	return func(yield func(sseFrame) bool) {
		if rw == nil {
			yield(sseFrame{hard: true})
			return
		}
		reader := bufio.NewReaderSize(rw, sseInitialBuffer)
		var dataLines []string
		var rawLines []string
		var eventName string
		eventBytes := 0

		dispatch := func() (keep, stop bool) {
			if len(rawLines) == 0 {
				return true, false
			}
			frame := sseFrame{
				event: eventName,
				data:  strings.Join(dataLines, "\n"),
				raw:   []byte(strings.Join(rawLines, "\n")),
			}
			dataLines = dataLines[:0]
			rawLines = rawLines[:0]
			eventName = ""
			eventBytes = 0
			if !yield(frame) {
				return false, true
			}
			return true, false
		}

		for {
			line, readErr := reader.ReadString('\n')
			if len(line) > 0 {
				line = strings.TrimSuffix(line, "\n")
				line = strings.TrimSuffix(line, "\r")
				if line == "" {
					keep, stop := dispatch()
					if stop || !keep {
						return
					}
				} else {
					eventBytes += len(line) + 1
					if eventBytes > maxEventBytes {
						yield(sseFrame{hard: true})
						return
					}
					rawLines = append(rawLines, line)
					switch {
					case strings.HasPrefix(line, "data:"):
						dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					case strings.HasPrefix(line, "event:"):
						eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
					}
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					// 流未以空行结尾时处理最后一个事件。
					if _, stop := dispatch(); stop {
						return
					}
					return
				}
				yield(sseFrame{hard: true})
				return
			}
		}
	}
}

// classifyUpstreamStatus 是原生适配器共用的状态码分类。
// 规则与 M2 OpenAI 适配器完全一致（brief：按 M2 errors.go 口径），
// 保持为一个函数避免两处实现漂移。
// 【复审 #8 登记】OpenAI 的 ClassifyError 目前仍是独立方法；本轮按裁决
// 不重构 OpenAI，但 TestAnthropicClassifyMatchesOpenAI 会锁住两者的一致性，
// 防止未来只改一侧造成分派差异。
func classifyUpstreamStatus(status int) contract.ErrorClass {
	switch {
	case status < 100 || status > 599:
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
		// relay-01 §6.2 把 408 列入「不重试的固定集合」，不得归为可重试。
		return contract.ErrUnknown
	case status == http.StatusTooManyRequests:
		return contract.ErrUpstreamRateLimit
	case status >= 500:
		return contract.ErrUpstreamServer
	default:
		return contract.ErrUnknown
	}
}
