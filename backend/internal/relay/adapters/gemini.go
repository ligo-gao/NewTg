package adapters

// Gemini 原生适配器（AKGeminiNative）。
//
// 净室实现：encoding/json + net/http，不引入外部 SDK。
//
// 协议要点：
//   - generateContent / streamGenerateContent 的模型名在 URL 路径里；
//   - 鉴权头是 `x-goog-api-key`（relay-02 §8.2）；
//   - 流式用量跨块累计（relay-03 §6.2 row 7、契约 §2.4 硬规则 2）。

import (
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
	// geminiDefaultBaseURL 是类型 24 的默认基址（relay-02 §5 行 24）。
	geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"
	// geminiAPIVersion 是 generateContent 的稳定版本前缀。
	geminiAPIVersion = "v1beta"
)

// Gemini 是 Gemini generateContent 适配器。
type Gemini struct {
	channelType contract.ChannelType
}

var (
	_ contract.Adapter       = (*Gemini)(nil)
	_ contract.StreamAdapter = (*Gemini)(nil)
)

// NewGeminiAdapter 构造绑定到给定渠道类型的 Gemini 适配器（ledger 冻结签名）。
func NewGeminiAdapter(channelType contract.ChannelType) contract.Adapter {
	return &Gemini{channelType: channelType}
}

func (g *Gemini) Type() contract.ChannelType { return g.channelType }

// Caps 返回 Gemini 原生能力声明（relay-02 §3/§5 行 24）。
//
// StreamUsage=false：原生协议没有 include_usage 请求项；
// StreamAggregatesUsage=true：usageMetadata 跨块累计（relay-03 §6.2 row 7）。
func (g *Gemini) Caps() contract.Capabilities {
	return contract.Capabilities{
		Endpoints: contract.Set[contract.Endpoint]{
			contract.EndpointChat:            {},
			contract.EndpointGenerateContent: {},
			contract.EndpointModels:          {},
		},
		Stream:                true,
		StreamUsage:           false,
		StreamAggregatesUsage: true,
		ModelList:             true,
		CreditShape:           contract.CreditSingleKey,
		DefaultBaseURL:        geminiDefaultBaseURL,
		Roles:                 []contract.ChannelRole{contract.RoleChat},
		BaseURLMode:           contract.BaseURLDefault,
	}
}

// BuildRequest 构造 generateContent / streamGenerateContent 请求。
//
// 流式通过 URL 后缀 + `?alt=sse` 表达，而不是请求体字段。
// 幂等与 GC-6 约定同 Anthropic/OpenAI 适配器。
func (g *Gemini) BuildRequest(rc *contract.RelayContext, req *contract.Request) (*http.Request, error) {
	if rc == nil || req == nil {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: nil relay context or request")
	}
	channelType := rc.ChannelType
	if channelType == contract.ChannelTypeUnknown {
		channelType = g.channelType
	}
	if channelType != contract.ChannelTypeGemini {
		// 本适配器只服务类型 24；Vertex（41）有自己的凭据与路径规则。
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel type %q is not Gemini native", channelType)
	}
	if rc.Credential == "" {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: channel %d has no credential", rc.ChannelID)
	}

	baseURL := strings.TrimSpace(rc.APIBase)
	if baseURL == "" {
		baseURL = geminiDefaultBaseURL
	}

	upstreamReq := *req
	if rc.UpstreamModel != "" {
		upstreamReq.Model = rc.UpstreamModel
	}
	if strings.TrimSpace(upstreamReq.Model) == "" {
		return nil, classifiedf(contract.ErrParamInvalid, "adapters: model is empty")
	}
	body, err := ir.BuildGeminiRequestBody(&upstreamReq)
	if err != nil {
		return nil, classifiedf(contract.ErrParamInvalid, "%w", err)
	}

	fullURL, err := geminiURL(baseURL, upstreamReq.Model, upstreamReq.Stream)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, classifiedf(contract.ErrChannelConfig, "adapters: construct upstream request failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	accept := "application/json"
	if req.Stream {
		accept = "text/event-stream"
	}
	httpReq.Header.Set("Accept", accept)
	// Gemini 使用 x-goog-api-key，而不是 Bearer（relay-02 §8.2）。
	httpReq.Header.Set("x-goog-api-key", rc.Credential)
	return httpReq, nil
}

// geminiURL 拼接 {base}[/{version}]/models/{model}:{method}[?alt=sse]。
//
// base_url 可能已经带 /v1beta（渠道管理员按官方文档填写）；此时不再重复版本段。
// URL 解析错误不回显底层文本（GC-6）。
func geminiURL(baseURL, model string, stream bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", classifiedf(contract.ErrChannelConfig, "adapters: invalid API base URL")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(basePath, "/v1beta") && !strings.HasSuffix(basePath, "/v1") &&
		!strings.HasSuffix(basePath, "/v1alpha") {
		basePath += "/" + geminiAPIVersion
	}
	// 模型名可能以 "models/" 前缀给出；路径里已固定 /models/，避免重复。
	model = strings.TrimPrefix(model, "models/")
	method := ":generateContent"
	if stream {
		method = ":streamGenerateContent"
	}
	parsed.Path = basePath + "/models/" + url.PathEscape(model) + method
	if stream {
		query := parsed.Query()
		query.Set("alt", "sse")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// ParseResponse 解析非流式 generateContent 响应。
func (g *Gemini) ParseResponse(_ *contract.RelayContext, resp *http.Response) (*contract.Response, contract.Usage, error) {
	if resp == nil {
		return nil, contract.Usage{}, classifiedf(contract.ErrUpstreamServer, "adapters: nil upstream response")
	}
	body, err := readUpstreamBody(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		class := g.ClassifyError(resp.StatusCode, body)
		return nil, contract.Usage{}, classifiedf(class, "adapters: upstream returned status %d", resp.StatusCode)
	}
	response, usage, err := ir.ParseGeminiResponse(body)
	if err != nil {
		return nil, contract.Usage{}, classifiedf(contract.ErrUpstreamServer, "adapters: invalid upstream response")
	}
	return response, usage, nil
}

// ClassifyError 与 M2 OpenAI 适配器同口径。
func (g *Gemini) ClassifyError(status int, _ []byte) contract.ErrorClass {
	return classifyUpstreamStatus(status)
}

// ============================================================================
// 流式：Gemini SSE → StreamEvent
// ============================================================================

// geminiStreamChunk 是 streamGenerateContent 的单个 data JSON。
type geminiStreamChunk struct {
	Candidates []struct {
		Index   int `json:"index"`
		Content *struct {
			Parts []json.RawMessage `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata json.RawMessage `json:"usageMetadata"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// StreamEvents 逐帧解析 Gemini SSE。
//
// 事件映射：
//   - part.text → EventDelta（thought=true → Reasoning 独立通道）；
//   - part.functionCall → EventDelta.ToolDelta（Gemini 一次给完整参数，
//     不是增量切片；Index 递增以区分同一块里的多个调用）；
//   - finishReason → EventFinish（每个候选只发一次）；
//   - usageMetadata → 合并后 EventUsage（跨块累计，非零覆盖）。
//
// 图片（inlineData）等无法用 StreamEvent 表达的载荷以 EventRaw 原样交出，
// 不丢弃。未知 part 同样 EventRaw；未知 SSE 类型不 panic。
func (g *Gemini) StreamEvents(_ *contract.RelayContext, rw io.ReadWriter) iter.Seq[contract.StreamEvent] {
	return geminiStreamEvents(rw, sseMaxEventBytes)
}

func geminiStreamEvents(rw io.ReadWriter, maxEventBytes int) iter.Seq[contract.StreamEvent] {
	return func(yield func(contract.StreamEvent) bool) {
		usageAcc := contract.Usage{
			PromptIncludesCache:           false,
			ReasoningIncludedInCompletion: true,
			Source:                        contract.UsageEstimated,
		}
		finishSent := false
		toolIndex := 0
		model := ""

		for frame := range sseFrameSeq(rw, maxEventBytes) {
			if frame.hard {
				yield(contract.StreamEvent{Kind: contract.EventError})
				return
			}
			payload := strings.TrimSpace(frame.data)
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				if !finishSent {
					finishSent = true
					yield(contract.StreamEvent{Kind: contract.EventFinish, Model: model, Raw: frame.raw})
				}
				return
			}
			var chunk geminiStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				if !yield(contract.StreamEvent{Kind: contract.EventError, Model: model, Raw: frame.raw}) {
					return
				}
				continue
			}
			if chunk.Error != nil {
				// 上游错误块：只交原始帧，不拼接可能含敏感信息的消息串。
				if !yield(contract.StreamEvent{Kind: contract.EventError, Model: model, Raw: frame.raw}) {
					return
				}
				continue
			}
			for _, candidate := range chunk.Candidates {
				if candidate.Content != nil {
					for _, rawPart := range candidate.Content.Parts {
						var part map[string]any
						if err := json.Unmarshal(rawPart, &part); err != nil {
							if !yield(contract.StreamEvent{Kind: contract.EventError, Model: model, Raw: frame.raw}) {
								return
							}
							continue
						}
						if text, ok := part["text"].(string); ok {
							processed := contract.StreamEvent{Model: model, Raw: frame.raw}
							if thought, _ := part["thought"].(bool); thought {
								processed.Kind = contract.EventDelta
								processed.Reasoning = text
							} else {
								processed.Kind = contract.EventDelta
								processed.Delta = text
							}
							if !yield(processed) {
								return
							}
							continue
						}
						if call, ok := part["functionCall"].(map[string]any); ok {
							name, _ := call["name"].(string)
							args, err := json.Marshal(call["args"])
							if err != nil {
								args = []byte("{}")
							}
							delta := contract.ToolCallDelta{
								Index:             toolIndex,
								Name:              name,
								ArgumentsFragment: string(args),
							}
							if id, ok := call["id"].(string); ok {
								delta.ID = id
							}
							toolIndex++
							if !yield(contract.StreamEvent{Kind: contract.EventDelta, ToolDelta: &delta, Model: model, Raw: frame.raw}) {
								return
							}
							continue
						}
						// inlineData / functionResponse / 未知 part：流式事件没有对应
						// 语义位，交原始帧（EventRaw），绝不静默丢弃。
						if !yield(contract.StreamEvent{Kind: contract.EventRaw, Model: model, Raw: frame.raw}) {
							return
						}
					}
				}
				if candidate.FinishReason != "" && !finishSent {
					finishSent = true
					if !yield(contract.StreamEvent{Kind: contract.EventFinish, Model: model, Raw: frame.raw}) {
						return
					}
				}
			}
			if len(chunk.UsageMetadata) > 0 && string(chunk.UsageMetadata) != "null" {
				parsed := ir.ParseGeminiUsage(chunk.UsageMetadata)
				mergeGeminiStreamUsage(&usageAcc, parsed)
				if !yield(contract.StreamEvent{Kind: contract.EventUsage, Usage: usageCopy(usageAcc), Model: model, Raw: frame.raw}) {
					return
				}
			}
		}
		// Gemini 原生流没有 [DONE]，依赖上游关闭（relay-03 §7 Q6）。
		// EOF 是正常终止路径：不伪造 finish 事件，由端点层按 EndUpstreamEOF 处理。
	}
}

// mergeGeminiStreamUsage 跨块累计 usageMetadata（relay-03 §6.1 规则 4）。
//
// Gemini 的 usageMetadata 是**累计值**而非增量，因此按「非零覆盖」合并，
// 不能相加（相加会把同一批 token 计成多倍）。零值不得擦除已有非零值。
func mergeGeminiStreamUsage(dst *contract.Usage, src contract.Usage) {
	if src.Prompt != 0 {
		dst.Prompt = src.Prompt
	}
	if src.Completion != 0 {
		dst.Completion = src.Completion
	}
	if src.Total != 0 {
		dst.Total = src.Total
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.Reasoning != 0 {
		dst.Reasoning = src.Reasoning
	}
	if src.PromptDetail != (contract.UsageDetail{}) {
		dst.PromptDetail = src.PromptDetail
	}
	if src.CompletionDetail != (contract.UsageDetail{}) {
		dst.CompletionDetail = src.CompletionDetail
	}
	if dst.Total == 0 {
		dst.Total = dst.Prompt + dst.Completion
	}
	if src.Source == contract.UsageUpstreamExact {
		dst.Source = contract.UsageUpstreamExact
	}
	if src.Raw != nil {
		if dst.Raw == nil {
			dst.Raw = map[string]any{}
		}
		for key, value := range src.Raw {
			// 原键保留：cachedContentTokenCount 等必须原样可见（M3 usage_semantic）。
			dst.Raw[key] = value
		}
	}
}
