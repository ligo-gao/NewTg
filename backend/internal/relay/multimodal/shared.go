// Package multimodal 实现 relay-04 的多模态端点族：图像生成/编辑、嵌入、
// 重排、语音合成/转写/翻译与实时骨架。
//
// 职责边界（relay-04 §0.2 / §11）：
//   - 本包只负责「端点契约 → 上游请求 → 可计费量事实」；
//   - 路由/重试/预扣/退款复用 M2 的 relay.ExecuteAttempts（同一套候选、pin、
//     重试预算与失败退款语义），不另造一套执行循环；
//   - 定价与积分换算仍归 M3 计费引擎：本包只产出 contract.UsageFact 与
//     QuotaEstimate（GC-3/GC-4）。
//
// 适配器纪律：contract 包冻结的 ImageAdapter/EmbeddingAdapter/... 签名不改
// （GC-1）。本包用一个 endpointAdapter 实现全部端点方法，由 requestPlan 决定
// 实际路径与响应形态；对执行层而言它就是某个端点的 contract.Adapter。
package multimodal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
)

const (
	// maxResponseBodyBytes 是非流式响应体的读取上限（64 MiB），与 M2 的
	// OpenAI 适配器同口径：图像 b64 响应可能较大，但无界读取会让异常上游
	// 拖垮进程（GC-6/防 OOM）。
	maxResponseBodyBytes = 64 << 20

	// maxErrorBodyBytes 是错误体读取上限，只用于适配器分类，绝不回显（GC-6）。
	maxErrorBodyBytes = 64 << 10

	// multimodalRawFormat 标注入站协议形态：对外契约按 OpenAI 形态暴露。
	multimodalRawFormat contract.RelayFormat = "openai"
)

var (
	// ErrUnsupportedChannel 表示该渠道类型没有本端点族实现。
	// 包装 adapters.ErrEndpointUnsupported：不可重试、不自动禁用（契约 §4）。
	ErrUnsupportedChannel = fmt.Errorf("multimodal: channel type is not supported: %w", adapters.ErrEndpointUnsupported)
)

// classified 构造带契约分类的适配器错误。错误文本只含固定描述（GC-6）。
func classified(class contract.ErrorClass, message string) error {
	return &adapters.Error{Class: class, Err: errors.New(message)}
}

// RequestPlan 是某个端点的一次请求编排：路径、原始体、是否流式与预扣估算。
//
// rawBody 是归一化层（httpapi 入口校验）已经校验过的完整请求体；本包不再
// 放宽任何上界，只把上游模型名映射后转发（relay-04 §9 的「不得放宽」）。
type RequestPlan struct {
	endpoint    contract.Endpoint
	path        string
	contentType string
	rawBody     []byte
	// stream 决定响应形态：图像请求体 stream=true，或 TTS stream_format=sse。
	stream bool
	// multipart 表示 rawBody 是 multipart/form-data，路径不得改写模型字段
	// （改写会破坏 boundary 与字段顺序，模型映射交由上游适配器选择）。
	multipart bool
	// estimate 是 PreConsume 的输入（图片张数、音频时长等占位量）。
	estimate contract.QuotaEstimate
	// responseFormat 是 TTS 的 response_format（pcm/mp3/wav...），决定时长
	// 解析口径；也用于二进制响应的 Content-Type 兜底。
	responseFormat string
	// started 是请求进入 dispatcher 的时刻；JSON 路径的 LatencyMS 以它为
	// 起点（F14：此前固定写 0，审计事实会丢失真实延迟）。
	started time.Time
	// geminiPath 表示请求走 Gemini 语义嵌入路径（:embedContent 等），
	// 上游 URL 直接复用客户端路径，鉴权用 x-goog-api-key。
	geminiPath bool
	// geminiEngines 表示客户端走 `/v1/engines/{model}/embeddings`：该形态
	// 使用 OpenAI 风格 body，需要翻译成 Gemini `:embedContent`（N2）。
	geminiEngines bool
	// geminiModel 是 Gemini 语义路径提取出的模型名（URL 中承载）。
	geminiModel string
}

// endpointAdapter 实现 contract 冻结的全部多模态适配器接口。
//
// 一个实例只服务一次尝试（ExecuteAttempts 每轮调用 Dispatch 重新构造），
// 因而无需考虑并发复用；plan 由 dispatcher 按请求注入。
type endpointAdapter struct {
	channelType contract.ChannelType
	defaultBase string
	base        contract.Adapter // 用于错误分类（复用该渠道的协议判定）
	client      *http.Client
	plan        *RequestPlan
}

// 编译期断言：冻结接口签名不改（GC-1）。同一类型实现全部接口是允许的——
// 契约按端点做能力切分，而不是要求每种端点一个 Go 类型。
var (
	_ contract.ImageAdapter             = (*endpointAdapter)(nil)
	_ contract.ChannelImageUsageAdapter = (*endpointAdapter)(nil)
	_ contract.EmbeddingAdapter         = (*endpointAdapter)(nil)
	_ contract.RerankAdapter            = (*endpointAdapter)(nil)
	_ contract.SpeechAdapter            = (*endpointAdapter)(nil)
	_ contract.TranscriptionAdapter     = (*endpointAdapter)(nil)
	_ contract.TranslationAdapter       = (*endpointAdapter)(nil)
)

func (a *endpointAdapter) Type() contract.ChannelType { return a.channelType }

func (a *endpointAdapter) Caps() contract.Capabilities {
	caps := a.base.Caps()
	// 端点能力是**请求级事实**：M4 的多模态端点由本包按 channel kind 矩阵
	// 提供，不复用基础适配器只声明 chat 的 Endpoints（避免把「支持」误判为
	// 「不支持」）。渠道选择仍由 dispatcher 的 supports 判定把关。
	caps.Endpoints = contract.Set[contract.Endpoint]{a.plan.endpoint: {}}
	if a.plan.stream {
		caps.Stream = true
	}
	return caps
}

// BuildRequest 构造发往上游的端点请求。
//
// 幂等（relay-02 §8.5）：body 来自只读的 plan.rawBody；每次调用都重新建
// bytes.Reader，http.NewRequest 自动填 ContentLength/GetBody。
func (a *endpointAdapter) BuildRequest(rc *contract.RelayContext, req *contract.Request) (*http.Request, error) {
	if rc == nil || req == nil {
		return nil, classified(contract.ErrChannelConfig, "multimodal: nil relay context or request")
	}
	channelType := rc.ChannelType
	if channelType == contract.ChannelTypeUnknown {
		channelType = a.channelType
	}
	base := strings.TrimSpace(rc.APIBase)
	if base == "" {
		base = a.defaultBase
	}
	if base == "" {
		// 必填 base_url 的类型（azure/custom 等）没有可静默使用的默认值，
		// 回落到 OpenAI 默认基址会把凭据发往错误上游（relay-02 §7.1）。
		return nil, classified(contract.ErrChannelConfig, "multimodal: channel requires a base URL")
	}

	body := a.plan.rawBody
	if len(req.Raw) > 0 {
		body = req.Raw
	}
	upstreamModel := rc.UpstreamModel
	if upstreamModel == "" {
		upstreamModel = req.Model
	}
	path := a.plan.path
	if a.plan.multipart {
		// F10：multipart 也必须做模型映射（编辑/转写/翻译）。重新编码表单
		// 时保持原 boundary 与 part 顺序，内容逐字节复制。
		if upstreamModel != "" {
			if rewritten, ok := rewriteMultipartModel(body, a.plan.contentType, upstreamModel); ok {
				body = rewritten
			}
		}
		// F9：适配器本地二次校验（CCR-9），只校验计费乘数。
		if err := validatePayloadForUpstream(a.plan, body); err != nil {
			return nil, err
		}
	} else {
		// 模型映射：上游模型名优先（relay-02 §8.1）。仅 JSON 路径可安全改写。
		// Gemini 语义路径的模型在 URL 中，body 没有 model 字段；写入顶层
		// model 会被 Gemini 当作未知字段拒绝，故跳过。
		if !a.plan.geminiPath {
			body = rewriteJSONModel(body, upstreamModel)
		} else if a.plan.geminiEngines {
			// N2：`/v1/engines/{model}/embeddings` 的入站是 OpenAI 风格
			// `input`，翻译成 Gemini `content.parts`；多输入 → 批量端点。
			if translated, batch, ok := openAIInputToGeminiEmbedding(body, a.plan.geminiModel); ok {
				body = translated
				action := ":embedContent"
				if batch {
					action = ":batchEmbedContents"
				}
				path = "/v1beta/models/" + a.plan.geminiModel + action
			}
		}
		if err := validatePayloadForUpstream(a.plan, body); err != nil {
			return nil, err
		}
	}

	var query url.Values
	if channelType == contract.ChannelTypeAzure {
		// Azure 的图像/音频端点挂在部署名之下；api-version 与 chat 路径同口径
		// （relay-02 §7.4 的冻结默认值由适配器层维护，不在本包重复）。
		deployment := rc.UpstreamModel
		if deployment == "" {
			deployment = req.Model
		}
		path = "/openai/deployments/" + url.PathEscape(deployment) + a.plan.path
		query = url.Values{}
		query.Set("api-version", "2025-04-01-preview")
	}

	fullURL, err := joinBaseURL(base, path, query)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		// 底层错误可能回显 URL（GC-6）：只返回固定分类。
		return nil, classified(contract.ErrChannelConfig, "multimodal: construct upstream request failed")
	}
	contentType := a.plan.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	httpReq.Header.Set("Content-Type", contentType)
	accept := "application/json"
	if a.plan.stream {
		accept = "text/event-stream"
	}
	httpReq.Header.Set("Accept", accept)
	if rc.Credential == "" {
		return nil, classified(contract.ErrChannelConfig, "multimodal: channel has no credential")
	}
	if channelType == contract.ChannelTypeAzure {
		httpReq.Header.Set("api-key", rc.Credential)
	} else if channelType == contract.ChannelTypeGemini || channelType == contract.ChannelTypeVertexAi {
		// Gemini 原生鉴权头（查询参数 key 属兼容载体，优先用头避免进日志）。
		httpReq.Header.Set("x-goog-api-key", rc.Credential)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+rc.Credential)
	}
	return httpReq, nil
}

// validatePayloadForUpstream 按端点选择适配器本地校验（F9/CCR-9）。
func validatePayloadForUpstream(plan *RequestPlan, body []byte) error {
	switch plan.endpoint {
	case contract.EndpointImageGenerate, contract.EndpointImageEdit:
		return validateImagePayloadForUpstream(plan, body)
	case contract.EndpointAudioSpeech:
		return validateSpeechPayloadForUpstream(body)
	default:
		return nil
	}
}

// rewriteMultipartModel 重新编码 multipart，仅把 model 字段替换为
// upstreamModel，其余 part（含文件）按原顺序逐字节复制。
//
// 保持原 boundary：Content-Type 已由调用方透传，换 boundary 会让请求头与
// body 不一致。失败时返回 ok=false，调用方保留原始 body（不静默破坏请求）。
func rewriteMultipartModel(body []byte, contentType, upstreamModel string) ([]byte, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, false
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		return nil, false
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false
		}
		header := textproto.MIMEHeader{}
		for key, values := range part.Header {
			header[key] = append([]string(nil), values...)
		}
		dst, err := writer.CreatePart(header)
		if err != nil {
			_ = part.Close()
			return nil, false
		}
		if part.FormName() == "model" {
			_, err = io.WriteString(dst, upstreamModel)
		} else {
			_, err = io.Copy(dst, part)
		}
		_ = part.Close()
		if err != nil {
			return nil, false
		}
	}
	if err := writer.Close(); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// ParseResponse 读取非流式响应体并保留原文（透传路径不重编码）。
// 真正的用量推导在 dispatcher 中按端点完成；本方法保持最小化以满足
// contract.Adapter 的签名。
func (a *endpointAdapter) ParseResponse(rc *contract.RelayContext, resp *http.Response) (*contract.Response, contract.Usage, error) {
	if resp == nil {
		return nil, contract.Usage{}, classified(contract.ErrUpstreamServer, "multimodal: nil upstream response")
	}
	body, err := readBounded(resp.Body, maxResponseBodyBytes)
	if err != nil {
		return nil, contract.Usage{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, contract.Usage{}, &adapters.Error{
			Class: a.ClassifyError(resp.StatusCode, body),
			Err:   fmt.Errorf("multimodal: upstream returned status %d", resp.StatusCode),
		}
	}
	model := ""
	if rc != nil {
		model = rc.UpstreamModel
	}
	return &contract.Response{Model: model, Raw: body}, contract.Usage{}, nil
}

// ClassifyError 复用渠道基础适配器的分类（协议族决定 4xx/5xx 语义）。
func (a *endpointAdapter) ClassifyError(status int, body []byte) contract.ErrorClass {
	if a.base != nil {
		return a.base.ClassifyError(status, body)
	}
	return contract.ErrUnknown
}

// ---------- 一次性交换（冻结接口的直连实现） ----------
//
// 冻结的 Embeddings/Rerank/Speech/... 方法没有 ctx 与 *http.Response 参数，
// 因而天然只能「自己完成一次上游交换」。生产路径**不**走这些方法，而是由
// dispatcher 复用 relay.ExecuteAttempts（带 ctx/重试/预扣/退款）后调用同一
// 套解析 helper；这些方法保留给契约消费方与适配器级单测，两者共享解析逻辑，
// 不会出现两套用量口径。
func (a *endpointAdapter) exchange(rc *contract.RelayContext, req *contract.Request) (*http.Response, []byte, error) {
	httpReq, err := a.BuildRequest(rc, req)
	if err != nil {
		return nil, nil, err
	}
	// 冻结签名没有 context；这里用 Background，超时由 client 自身控制。
	// 生产路径的取消/看门狗由 ExecuteAttempts 负责。
	httpReq = httpReq.WithContext(context.Background())
	client := a.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, nil, classified(contract.ErrRetryable, "multimodal: upstream request failed")
	}
	body, readErr := readBounded(resp.Body, maxResponseBodyBytes)
	_ = resp.Body.Close()
	if readErr != nil {
		return resp, nil, readErr
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp, body, &adapters.Error{
			Class: a.ClassifyError(resp.StatusCode, body),
			Err:   fmt.Errorf("multimodal: upstream returned status %d", resp.StatusCode),
		}
	}
	return resp, body, nil
}

// ---------- HTTP helper ----------

// readBounded 有界读取；超限按上游服务器错误拒绝（不重试）。
func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, classified(contract.ErrRetryable, "multimodal: read upstream response failed")
	}
	if int64(len(body)) > limit {
		return nil, classified(contract.ErrUpstreamServer, "multimodal: upstream response exceeds the limit")
	}
	return body, nil
}

// rewriteJSONModel 只改写顶层 model 字段，其余字段逐字保留。
// 非 JSON 对象原样返回：模型映射失败不应破坏透传（上游会给出明确 400）。
func rewriteJSONModel(body []byte, model string) []byte {
	if model == "" || len(body) == 0 {
		return body
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return body
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return body
	}
	object["model"] = encoded
	out, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return out
}

// joinBaseURL 拼接 base 与端点路径。与 M2 joinURL 同语义：base 以 /v1 或
// /v1beta 结尾时去掉请求路径中重复的版本段，避免 /v1/v1/... 或
// /v1beta/v1beta/...（N3）。
func joinBaseURL(baseURL, requestPath string, query url.Values) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", classified(contract.ErrChannelConfig, "multimodal: invalid API base URL")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	// 任意版本段对之间去重：base 以 /v1、/v1beta、/v1alpha 等结尾时，
	// 客户端路径若再以任一版本段开头（例如 base=/v1 + /v1beta/...），
	// 去掉客户端路径的版本段，避免 /v1/v1beta/...（N3 残余）。
	if baseSegments := strings.Split(strings.Trim(basePath, "/"), "/"); len(baseSegments) > 0 &&
		isVersionSegment(baseSegments[len(baseSegments)-1]) {
		trimmed := strings.TrimPrefix(requestPath, "/")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) == 2 && isVersionSegment(parts[0]) {
			requestPath = "/" + parts[1]
		}
	}
	parsed.Path = basePath + requestPath
	if query != nil {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// isVersionSegment 判断单个路径段是否是 API 版本段（v1 / v1beta / v1alpha
// / v1beta1 / v2 等）。
func isVersionSegment(segment string) bool {
	if len(segment) < 2 || segment[0] != 'v' {
		return false
	}
	index := 1
	for index < len(segment) && segment[index] >= '0' && segment[index] <= '9' {
		index++
	}
	if index == 1 {
		return false
	}
	// s[1:index] 是主版本号；其后只允许 alpha/beta 或 alphaN/betaN。
	rest := segment[index:]
	switch rest {
	case "", "alpha", "beta":
		return true
	}
	for _, marker := range []string{"alpha", "beta"} {
		if !strings.HasPrefix(rest, marker) {
			continue
		}
		suffix := rest[len(marker):]
		if suffix == "" {
			return true
		}
		for i := 0; i < len(suffix); i++ {
			if suffix[i] < '0' || suffix[i] > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// endpointPath 返回端点族的上游路径（relay-04 §0.1）。
func endpointPath(endpoint contract.Endpoint) string {
	switch endpoint {
	case contract.EndpointImageGenerate:
		return "/v1/images/generations"
	case contract.EndpointImageEdit:
		return "/v1/images/edits"
	case contract.EndpointEmbeddings:
		return "/v1/embeddings"
	case contract.EndpointRerank:
		return "/v1/rerank"
	case contract.EndpointAudioSpeech:
		return "/v1/audio/speech"
	case contract.EndpointAudioTranscribe:
		return "/v1/audio/transcriptions"
	case contract.EndpointAudioTranslate:
		return "/v1/audio/translations"
	default:
		return ""
	}
}

// relayWithRequest 是 dispatcher 写请求组的状态（保持与 httpapi 的
// relay.WithRequest 同一写法）。
func relayWithRequest(rc *contract.RelayContext, plan *RequestPlan, path string) {
	relay.WithRequest(rc, &contract.Request{
		Model:  rc.OriginModel,
		Raw:    plan.rawBody,
		Stream: plan.stream,
	}, multimodalRawFormat, path)
}
