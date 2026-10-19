package multimodal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/usage"
)

// ============================================================================
// 多模态端点分派器（M4 Task 4）
//
// 执行模型与 /v1/chat/completions 完全一致（relay-04 §0.2/§11）：
//
//	入口校验（httpapi）→ PreConsume → ExecuteAttempts（候选/pin/重试）
//	→ 解析响应/推导用量 → Terminate（成功）或 Refund（失败/中断）
//
// 计费生命周期的唯一性由 settlement 守卫保证：一次请求至多一次 Terminate
// 或一次 Refund，绝不两者都发生（R-02 的「不得双扣」与 R-85 的「失败不收费」
// 是同一条不变量在成功/失败两侧的表述）。
// ============================================================================

// Options 是 Dispatcher 的显式依赖（与 relay.RouteDeps 同风格：不读全局单例）。
type Options struct {
	// Candidates 返回候选渠道；所需字段与 httpapi 的候选来源一致。
	Candidates func(context.Context, *contract.RelayContext) ([]contract.ChannelCandidate, error)
	// ResolveChannel 解析一轮候选的完整渠道组（凭据已在 httpapi 解封）。
	ResolveChannel func(context.Context, *contract.RelayContext, int) (relay.ChannelAssignment, error)
	// Dispatch 是渠道类型 → 基础适配器（生产为 adapters.Dispatch）。
	Dispatch func(contract.ChannelType) (contract.Adapter, error)
	// Billing 为每个请求构造计费会话。
	Billing func(*contract.RelayContext) contract.BillingSession
	// Client 是上游 HTTP 客户端；nil 时使用 http.DefaultClient（测试可注入）。
	Client *http.Client
	// MaxRetries 与 relay.RouteDeps 同语义（0 = 默认 3）。
	MaxRetries int
	// Supports 覆盖「渠道是否支持本端点族」的判定；nil 时使用 relay-04
	// 的端点×渠道类型矩阵（F2）。测试可注入以覆盖矩阵。
	Supports func(contract.ChannelType) bool
	// StreamInactivityTimeout 覆盖流式/二进制块间不活动看门狗的默认 300s。
	// 仅测试注入短值；生产使用冻结默认值（relay-03 §10.1）。
	StreamInactivityTimeout time.Duration
	// WriteError 把执行错误映射为客户端响应（生产注入 httpapi 的统一映射，
	// 保持与 chat 端点完全相同的状态码/信封）。nil 时使用固定 502。
	WriteError func(http.ResponseWriter, string, error)
}

// Dispatcher 实现 httpapi.MultimodalDispatcher。
type Dispatcher struct {
	opts Options
}

// NewDispatcher 构造分派器。依赖缺失是装配错误，直接 panic（与 M2 同口径）。
func NewDispatcher(opts Options) *Dispatcher {
	if opts.Candidates == nil || opts.ResolveChannel == nil || opts.Dispatch == nil || opts.Billing == nil {
		panic("multimodal: NewDispatcher requires Candidates/ResolveChannel/Dispatch/Billing")
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.WriteError == nil {
		opts.WriteError = writeFallbackError
	}
	return &Dispatcher{opts: opts}
}

// Handle 实现 MultimodalDispatcher。
func (d *Dispatcher) Handle(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, endpoint contract.Endpoint) {
	if rc == nil || r == nil {
		d.opts.WriteError(w, "", errors.New("multimodal: missing relay context"))
		return
	}
	plan, err := buildPlan(endpoint, r, rc)
	if err != nil {
		d.writeEntryError(w, rc.RequestID, err)
		return
	}
	// 归一化层仍是请求组的权威写入方：把端点归一化后的 Stream 与原始体写回。
	relayWithRequest(rc, plan, r.URL.Path)
	relay.WithQuotaEstimate(rc, &plan.estimate)

	if endpoint == contract.EndpointRealtime {
		d.serveRealtime(w, r, rc, plan)
		return
	}

	billing := d.opts.Billing(rc)
	settlement := &settlement{billing: billing}
	result, err := relay.ExecuteAttempts(r.Context(), rc, d.routeDeps(rc, r, plan, billing))
	if err != nil {
		// ExecuteAttempts 在 PreConsume 之后的所有失败出口已 Refund（GC-7/R-85）；
		// 本层只需要映射错误，不再触碰计费会话。
		d.opts.WriteError(w, rc.RequestID, err)
		return
	}
	defer func() { _ = result.Response.Body.Close() }()
	// 兜底：任何未显式终结的出口（含 panic）都必须释放预留。失败语义优先，
	// 与 relay.Route/bridgeStream 的 defer refund 同口径（R-85）。
	defer settlement.refundIfPending()

	switch endpoint {
	case contract.EndpointImageGenerate, contract.EndpointImageEdit:
		d.serveImage(w, r, rc, plan, result, settlement)
	case contract.EndpointAudioSpeech:
		d.serveAudioSpeech(w, r, rc, plan, result, settlement)
	default:
		d.serveJSON(w, rc, plan, result, settlement)
	}
}

// settlement 把一次请求的计费终结收敛为「Terminate XOR Refund，恰好一次」。
type settlement struct {
	billing contract.BillingSession
	mu      sync.Mutex
	done    bool
}

func (s *settlement) terminate(fact contract.UsageFact) {
	if s == nil || s.billing == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	_ = s.billing.Terminate(fact)
}

func (s *settlement) refund() {
	if s == nil || s.billing == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	_ = s.billing.Refund()
}

func (s *settlement) refundIfPending() {
	if s == nil {
		return
	}
	s.mu.Lock()
	pending := !s.done
	s.mu.Unlock()
	if pending {
		s.refund()
	}
}

// routeDeps 组装 relay.RouteDeps。Dispatch 每轮返回绑定本请求计划的端点适配器：
// ExecuteAttempts 负责候选/pin/重试，适配器只负责该端点的 URL/body/分类。
func (d *Dispatcher) routeDeps(rc *contract.RelayContext, r *http.Request, plan *RequestPlan, billing contract.BillingSession) relay.RouteDeps {
	pinID, hasPin := relay.ParseChannelPin(r)
	return relay.RouteDeps{
		Candidates: func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			candidates, err := d.opts.Candidates(ctx, rc)
			if err != nil {
				return nil, err
			}
			if !hasPin {
				return candidates, nil
			}
			pinned := false
			for i := range candidates {
				candidates[i].Constraints.PinChannelID = pinID
				candidates[i].Constraints.RetryMode = contract.PinRetrySingleAttempt
				if candidates[i].ChannelID == pinID {
					pinned = true
				}
			}
			if !pinned {
				return nil, fmt.Errorf("%w: channel %d", relay.ErrChannelNotFound, pinID)
			}
			return candidates, nil
		},
		ResolveChannel: func(ctx context.Context, channelID int) (relay.ChannelAssignment, error) {
			return d.opts.ResolveChannel(ctx, rc, channelID)
		},
		Dispatch: func(channelType contract.ChannelType) (contract.Adapter, error) {
			return d.adapterFor(channelType, plan)
		},
		Billing:    billing,
		Client:     d.opts.Client,
		MaxRetries: d.opts.MaxRetries,
	}
}

// adapterFor 把基础适配器包装成本端点适配器；不支持的类型显式拒绝
// （ErrEndpointUnsupported：不可重试、不自动禁用，契约 §4）。
func (d *Dispatcher) adapterFor(channelType contract.ChannelType, plan *RequestPlan) (contract.Adapter, error) {
	base, err := d.opts.Dispatch(channelType)
	if err != nil {
		return nil, err
	}
	if !d.supports(channelType, base, plan) {
		return nil, ErrUnsupportedChannel
	}
	return &endpointAdapter{
		channelType: channelType,
		defaultBase: base.Caps().DefaultBaseURL,
		base:        base,
		client:      d.opts.Client,
		plan:        plan,
	}, nil
}

func (d *Dispatcher) supports(channelType contract.ChannelType, base contract.Adapter, plan *RequestPlan) bool {
	if d.opts.Supports != nil {
		return d.opts.Supports(channelType)
	}
	// Gemini 语义嵌入路径（:embedContent / :batchEmbedContents）只由 Gemini
	// 原生渠道承载；OpenAI 兼容渠道无法解释该路径。
	if plan.geminiPath {
		return channelType == contract.ChannelTypeGemini
	}
	// F2：按 relay-04 §1.4/§2.4/§3.4/§4.6 的端点×渠道类型表判定。
	// 仅「表内声明支持」且「本包有 OpenAI 兼容实现」的渠道通过。
	allowed := endpointChannelSupport[plan.endpoint]
	if _, ok := allowed[channelType]; !ok {
		return false
	}
	// M4 的多模态端点以 OpenAI 兼容形态实现；其它协议族没有本族实现，
	// 必须显式拒绝而不是发出空体。Azure 的部署名/api-version 路径规则
	// 尚未对齐，显式排除（F11）。
	if channelType == contract.ChannelTypeAzure {
		return false
	}
	_, ok := base.(*adapters.OpenAI)
	return ok
}

// endpointChannelSupport 是 relay-04 端点×渠道类型支持矩阵的**显式白名单**。
//
// 只列「表内为是」且本包能正确编码的类型；relay-04 中标注为「否」的类型
// （deepseek/mistral/openrouter…）即使共用 OpenAI 适配器也不得放行——
// 能力声明缺省即不支持（契约 §4 · ErrEndpointUnsupported）。
var endpointChannelSupport = map[contract.Endpoint]map[contract.ChannelType]struct{}{
	contract.EndpointEmbeddings: {
		contract.ChannelTypeOpenAI:      {},
		contract.ChannelTypeAli:         {},
		contract.ChannelTypeSiliconFlow: {},
		contract.ChannelTypeVolcEngine:  {},
		contract.ChannelTypeZhipuV4:     {},
		contract.ChannelTypeNewAPI:      {},
	},
	contract.EndpointRerank: {
		contract.ChannelTypeOpenAI:      {},
		contract.ChannelTypeAli:         {},
		contract.ChannelTypeSiliconFlow: {},
		// N2：relay-04 §2.3 明确 Xinference 的重排响应形态；
		// 其 OpenAI 兼容适配器可以承载本实现。
		contract.ChannelTypeXinference: {},
	},
	contract.EndpointAudioSpeech: {
		contract.ChannelTypeOpenAI:      {},
		contract.ChannelTypeSiliconFlow: {},
		contract.ChannelTypeVolcEngine:  {},
	},
	contract.EndpointAudioTranscribe: {
		contract.ChannelTypeOpenAI:      {},
		contract.ChannelTypeSiliconFlow: {},
	},
	contract.EndpointAudioTranslate: {
		contract.ChannelTypeOpenAI:      {},
		contract.ChannelTypeSiliconFlow: {},
	},
	contract.EndpointImageGenerate: {
		contract.ChannelTypeOpenAI: {},
		contract.ChannelTypeAli:    {},
		contract.ChannelTypeXai:    {},
	},
	contract.EndpointImageEdit: {
		contract.ChannelTypeOpenAI: {},
		contract.ChannelTypeAli:    {},
		contract.ChannelTypeXai:    {},
	},
	// N1：realtime P0 骨架仍由 OpenAI 兼容渠道的 DialUpstream 承载；
	// 矩阵缺项会让 Handle 在握手前就 400，F6 的路径不可达。
	contract.EndpointRealtime: {
		contract.ChannelTypeOpenAI: {},
	},
}

// GeminiEmbeddingModel 从 Gemini 语义嵌入路径提取模型名（N6：httpapi 入口
// 与 dispatcher 共用这一份实现，避免两处形态漂移）。
func GeminiEmbeddingModel(path string) (string, bool) {
	if strings.HasPrefix(path, "/v1/engines/") {
		rest := strings.TrimPrefix(path, "/v1/engines/")
		if !strings.HasSuffix(rest, "/embeddings") {
			return "", false
		}
		model := strings.TrimSuffix(rest, "/embeddings")
		if model == "" || strings.Contains(model, "/") {
			return "", false
		}
		return model, true
	}
	for _, prefix := range []string{"/v1/models/", "/v1beta/models/"} {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		for _, action := range []string{":embedContent", ":batchEmbedContents"} {
			if strings.HasSuffix(rest, action) {
				model := strings.TrimSuffix(rest, action)
				if model == "" || strings.Contains(model, "/") {
					return "", false
				}
				return model, true
			}
		}
		return "", false
	}
	return "", false
}

// ============================================================================
// 请求计划
// ============================================================================

// entryError 是 dispatcher 自己的入口错误（正常入口校验已在 httpapi 完成；
// 这里只覆盖 multipart/JSON 二次解析失败的防御路径）。
type entryError struct {
	status  int
	code    string
	message string
}

func (e *entryError) Error() string { return e.message }

func buildPlan(endpoint contract.Endpoint, r *http.Request, rc *contract.RelayContext) (*RequestPlan, error) {
	body := []byte(nil)
	if rc.Request != nil {
		body = append([]byte(nil), rc.Request.Raw...)
	}
	if len(body) == 0 && r.Body != nil {
		read, err := io.ReadAll(io.LimitReader(r.Body, int64(32<<20)))
		if err != nil {
			return nil, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "failed to read request body"}
		}
		body = read
	}
	contentType := r.Header.Get("Content-Type")
	plan := &RequestPlan{
		endpoint:    endpoint,
		path:        endpointPath(endpoint),
		contentType: contentType,
		rawBody:     body,
		multipart:   strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data"),
		estimate:    contract.QuotaEstimate{Model: rc.OriginModel, Group: rc.UsingGroup},
		started:     time.Now(),
	}

	switch endpoint {
	case contract.EndpointImageGenerate, contract.EndpointImageEdit:
		fields, err := decodeImageRequest(plan)
		if err != nil {
			return nil, err
		}
		plan.stream = fields.stream
		plan.estimate.ImageCount = fields.n
		// AttemptImageCount 是两段预扣的第二段：出站 body 与请求 IR 的 n
		// 在本实现中一致（适配器只做模型映射），但显式写入以便计费侧按
		// 尝试域处理（契约 §6.1 · CCR-7）。
		plan.estimate.AttemptImageCount = fields.n
		plan.responseFormat = fields.responseFormat
		if plan.multipart {
			plan.contentType = contentType
		} else {
			plan.contentType = "application/json"
		}
	case contract.EndpointEmbeddings:
		plan.contentType = "application/json"
		plan.estimate.Model = rc.OriginModel
		// R-11 Gemini 专用嵌入路径：直接复用客户端路径；模型取自路径。
		if model, ok := GeminiEmbeddingModel(rc.RequestPath); ok {
			plan.geminiPath = true
			plan.geminiModel = model
			plan.estimate.Model = model
			if strings.HasPrefix(rc.RequestPath, "/v1/engines/") {
				// N2：engines 形态的 OpenAI 风格 body 需要翻译，上游路径
				// 固定为单体 :embedContent。
				plan.geminiEngines = true
				plan.path = "/v1beta/models/" + model + ":embedContent"
			} else {
				plan.path = rc.RequestPath
			}
		}
	case contract.EndpointRerank:
		plan.contentType = "application/json"
		plan.estimate.Model = rc.OriginModel
	case contract.EndpointAudioSpeech:
		plan.contentType = "application/json"
		plan.responseFormat = ResponseFormatFromRequest(body)
		var payload struct {
			StreamFormat string `json:"stream_format"`
		}
		_ = json.Unmarshal(body, &payload)
		plan.stream = strings.EqualFold(strings.TrimSpace(payload.StreamFormat), "sse")
		plan.estimate.Model = rc.OriginModel
	case contract.EndpointAudioTranscribe, contract.EndpointAudioTranslate:
		plan.contentType = contentType
		file, err := multipartFileByName(body, contentType, "file")
		if err != nil {
			return nil, err
		}
		// 预扣阶段用上传文件头时长（relay-04 §8.2.1）：这是用户可控量，
		// 经 AudioDuration 饱和换算；取不到时为 0（结算再以上游为准）。
		format := formatFromFilename(file.filename, file.contentType)
		plan.estimate.AudioSeconds = audioSecondsForEstimate(rc, file.data, format)
		plan.estimate.Model = rc.OriginModel
	case contract.EndpointRealtime:
		plan.path = realtimePath
	default:
		return nil, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "unsupported multimodal endpoint"}
	}
	return plan, nil
}

type imageFields struct {
	n              int
	stream         bool
	responseFormat string
}

// decodeImageRequest 解析 JSON 或 multipart 的图像计划字段。
//
// 入口上界校验由 httpapi 完成（relay-04 §9）；这里只读取规划所需字段，
// 并对「入口漏校验」做防御性钳制（不绕过、只拒绝）。
func decodeImageRequest(plan *RequestPlan) (imageFields, error) {
	fields := imageFields{n: 1}
	if plan.multipart {
		parsed, err := parseMultipart(plan.rawBody, plan.contentType, false)
		if err != nil {
			return fields, err
		}
		if n, ok := parseImageN(parsed.fields["n"]); ok {
			fields.n = n
		} else if _, present := parsed.fields["n"]; present {
			return fields, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "n must be an integer in [1, 128]"}
		}
		if raw, ok := parsed.fields["parameters"]; ok {
			if n, _, ok := parseImageParameters(raw); ok && n > 0 {
				fields.n = n
			} else if !ok {
				return fields, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "parameters must be a JSON object"}
			}
		}
		fields.stream = strings.EqualFold(parsed.fields["stream"], "true")
		fields.responseFormat = parsed.fields["response_format"]
		return fields, nil
	}

	var payload struct {
		N              json.RawMessage `json:"n"`
		Stream         bool            `json:"stream"`
		ResponseFormat string          `json:"response_format"`
		Parameters     json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(plan.rawBody, &payload); err != nil {
		return fields, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "invalid JSON request body"}
	}
	if n, ok := parseImageN(string(payload.N)); ok {
		fields.n = n
	} else if len(bytes.TrimSpace(payload.N)) > 0 && string(bytes.TrimSpace(payload.N)) != "null" {
		return fields, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "n must be an integer in [1, 128]"}
	}
	if n, _, ok := parseImageParameters(string(payload.Parameters)); ok && n > 0 {
		fields.n = n
	}
	fields.stream = payload.Stream
	fields.responseFormat = strings.ToLower(payload.ResponseFormat)
	return fields, nil
}

// parseImageParameters 解析 Extra["parameters"].n / useProviderParameters。
// 返回 (n, useProvider, valid)。n==0 且非 useProvider 表示「沿用请求 n」。
func parseImageParameters(raw string) (int, bool, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return 0, false, true
	}
	var payload struct {
		N                     json.RawMessage `json:"n"`
		UseProviderParameters bool            `json:"useProviderParameters"`
	}
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return 0, false, false
	}
	n, ok := parseImageN(string(payload.N))
	if !ok {
		if len(bytes.TrimSpace(payload.N)) == 0 || string(bytes.TrimSpace(payload.N)) == "null" {
			return 0, payload.UseProviderParameters, true
		}
		return 0, false, false
	}
	return n, payload.UseProviderParameters, true
}

// parseImageN 解析入口已校验的 n；返回 [1, MaxImageN] 的整数。
//
// F5：JSON 的 `2.0`/`2e0` 是合法整数语义（入口用 optionalJSONNumber 接受），
// 这里必须同样按数值是否整判定，不能只认十进制字面量（否则入口 200、
// 分派阶段 400）。
func parseImageN(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return 0, false
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
		return 0, false
	}
	if value < 1 || value > MaxImageN {
		return 0, false
	}
	return int(value), true
}

// ============================================================================
// 响应路径
// ============================================================================

// serveJSON 用于嵌入/重排/转写/翻译：上游 JSON 原样回写，用量三源提取。
func (d *Dispatcher) serveJSON(w http.ResponseWriter, rc *contract.RelayContext, plan *RequestPlan, result *relay.AttemptResult, settlement *settlement) {
	body, err := readBounded(result.Response.Body, maxResponseBodyBytes)
	if err != nil {
		settlement.refund()
		d.opts.WriteError(w, rc.RequestID, err)
		return
	}
	fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: body, Model: rc.UpstreamModel}, result.Response.Header, nil, rc.ChannelType)
	// 转写/翻译的上游 `duration` 是**测得量**：超过 MaxAudioSeconds 时 T3 会
	// 静默饱和到 3600s（usage 包内 maxAudioSeconds），但没有审计标记。
	// 这里读取**原始** duration 并在发生截断时补写 QuotaClamp（契约 §12
	// 不变量 9：任何饱和都必须可审计）；已饱和后的 usage 无需再改。
	if plan.endpoint == contract.EndpointAudioTranscribe || plan.endpoint == contract.EndpointAudioTranslate {
		if original, ok := rawAudioDuration(body); ok && original > MaxAudioSeconds {
			relay.WithQuotaClamp(rc, &contract.QuotaClamp{
				Op:       "TranscriptionDuration",
				Kind:     contract.ClampOverflow,
				Original: original,
				Clamped:  MaxAudioSeconds,
			})
		}
	}
	// F2/N1：音频兜底只看**音频维度是否缺席**（PromptDetail.Audio 与
	// AudioSeconds 都为 0），而不是看整份 usage 是否为空。上游给出纯文本
	// usage（如 {"prompt_tokens":5}）时仍必须用上传文件头测得的时长补上
	// 音频 token，并与文本 usage 合并。
	if (plan.endpoint == contract.EndpointAudioTranscribe || plan.endpoint == contract.EndpointAudioTranslate) &&
		plan.estimate.AudioSeconds > 0 &&
		fact.Usage.AudioSeconds == 0 && fact.Usage.PromptDetail.Audio == 0 && fact.Usage.CompletionDetail.Audio == 0 {
		fact.Usage.AudioSeconds = plan.estimate.AudioSeconds
		fact.Usage.PromptDetail.Audio = pricing.AudioTokensFromSeconds(float64(plan.estimate.AudioSeconds))
		// 文本来自上游、音频来自本地测量：整份事实标记为 estimated（混合来源
		// 不能谎报 upstream_exact）。
		fact.Usage.Source = contract.UsageEstimated
	}
	// F7：嵌入/重排上游省略 usage 时做本地估算（relay-04 §1.3/§2.3/§8.3/§8.4），
	// 不再静默按 0 结算。估算口径为确定性字节数/4（中继侧没有分词器，M2
	// chat 路径同样只报 estimated、由计费侧决定兜底）。
	if !usageHasFact(fact.Usage) {
		switch plan.endpoint {
		case contract.EndpointEmbeddings:
			// engines 路由的请求体是 OpenAI 形状（input），即使它走 Gemini
			// 语义上游；只有原生 :embedContent/:batchEmbedContents 才是
			// Gemini parts 形状。
			if plan.geminiPath && !plan.geminiEngines {
				fact.Usage = estimatedTextUsage(estimateGeminiEmbeddingText(plan.rawBody))
			} else {
				fact.Usage = estimatedTextUsage(estimateEmbeddingText(plan.rawBody))
			}
		case contract.EndpointRerank:
			fact.Usage = estimatedTextUsage(estimateRerankText(plan.rawBody))
		}
	}
	if rc.OriginModel != "" {
		fact.Model = rc.OriginModel
	}
	fact.LatencyMS = int(time.Since(plan.started).Milliseconds())
	fact.Stream = false
	fact.Attempts = rc.AttemptIndex + 1
	settlement.terminate(*fact)

	writeUpstream(w, rc.RequestID, result.Response, body)
}

// estimatedTextUsage 把本地估算的字节数转成文本 token 事实（F7）。
//
// 口径：ceil(bytes/4)，至少 1 token（与 relay-04「缺失 usage → 本地估算」
// 的兜底方向一致）。中继没有分词器，计费侧仍按 estimated 处理。
func estimatedTextUsage(textBytes int) contract.Usage {
	if textBytes <= 0 {
		return contract.Usage{Source: contract.UsageEstimated}
	}
	tokens := int64((textBytes + 3) / 4)
	return contract.Usage{
		Prompt:       tokens,
		Total:        tokens,
		PromptDetail: contract.UsageDetail{Text: tokens},
		Source:       contract.UsageEstimated,
	}
}

// estimateEmbeddingText 汇总嵌入请求中所有字符串输入的字节数
// （非字符串元素按基线忽略）。
func estimateEmbeddingText(body []byte) int {
	var payload struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	return sumJSONStrings(payload.Input)
}

// estimateRerankText 汇总 query 与 documents 的文本字节数（文档对象按
// JSON 序列化后的字节数计入，保持确定性）。
func estimateRerankText(body []byte) int {
	var payload struct {
		Query     string            `json:"query"`
		Documents []json.RawMessage `json:"documents"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	total := len(payload.Query)
	for _, document := range payload.Documents {
		trimmed := bytes.TrimSpace(document)
		if len(trimmed) >= 2 && trimmed[0] == '"' {
			var text string
			if json.Unmarshal(trimmed, &text) == nil {
				total += len(text)
				continue
			}
		}
		total += len(trimmed)
	}
	return total
}

// estimateGeminiEmbeddingText 汇总 Gemini 嵌入请求的文本字节数：
//   - embedContent: content.parts[].text
//   - batchEmbedContents: requests[].content.parts[].text
func estimateGeminiEmbeddingText(body []byte) int {
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	var payload struct {
		Content  content `json:"content"`
		Requests []struct {
			Content content `json:"content"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0
	}
	total := 0
	for _, part := range payload.Content.Parts {
		total += len(part.Text)
	}
	for _, request := range payload.Requests {
		for _, part := range request.Content.Parts {
			total += len(part.Text)
		}
	}
	return total
}

// sumJSONStrings 递归统计 JSON 字符串的字节数（字符串或字符串数组）。
func sumJSONStrings(raw json.RawMessage) int {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0
	}
	if trimmed[0] == '"' {
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			return len(text)
		}
		return 0
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil {
			return 0
		}
		total := 0
		for _, item := range items {
			total += sumJSONStrings(item)
		}
		return total
	}
	return 0
}

// applyImageTokenFloor 实施 relay-04 §4.7 的 token 维度兜底：
// usage.TotalTokens / PromptTokens 为 0 时置 1（仅影响 token 维度，
// 不改变张数）。上游 image usage 常只给张数，零 token 会让计费日志
// 看起来像「免费」。
func applyImageTokenFloor(u *contract.Usage) {
	if u.Total == 0 {
		u.Total = 1
	}
	if u.Prompt == 0 {
		u.Prompt = 1
	}
}

// streamInactivityTimeout 是流式/二进制块间不活动看门狗（relay-03 §10.1
// 冻结 300s；测试可经 Options 覆盖）。
const streamInactivityTimeout = 300 * time.Second

func (d *Dispatcher) streamTimeout() time.Duration {
	if d.opts.StreamInactivityTimeout > 0 {
		return d.opts.StreamInactivityTimeout
	}
	return streamInactivityTimeout
}

// watchStream 返回一个在每块数据后重置的看门狗 reader：超过 timeout 没有
// 新字节时关闭上游 body，让阻塞的 Read 立即返回错误，避免无限挂起（F7）。
// 调用方必须 defer stop。
func (d *Dispatcher) watchStream(body io.ReadCloser) (io.Reader, *atomic.Bool, func()) {
	timeout := d.streamTimeout()
	timedOut := &atomic.Bool{}
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		_ = body.Close()
	})
	reader := &watchdogReader{reader: body, timer: timer, timeout: timeout}
	return reader, timedOut, func() { timer.Stop() }
}

type watchdogReader struct {
	reader  io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r *watchdogReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

// serveImage 处理图像生成/编辑：非流式原样回写（201 视为成功），流式转 SSE。
func (d *Dispatcher) serveImage(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, plan *RequestPlan, result *relay.AttemptResult, settlement *settlement) {
	start := time.Now()
	upstreamSSE := strings.Contains(strings.ToLower(result.Response.Header.Get("Content-Type")), "text/event-stream")
	if plan.stream || upstreamSSE {
		d.serveImageStream(w, r, rc, plan, result, settlement, upstreamSSE)
		return
	}

	adapter := result.Adapter
	// 信任顺序 ①：渠道专用有界上游计数（ImageUsage 读后复原 body）。
	count, ok := 0, false
	if imageUsage, supported := adapter.(contract.ChannelImageUsageAdapter); supported {
		count, ok = imageUsage.ImageUsage(rc, result.Response)
	}
	body, err := readBounded(result.Response.Body, maxResponseBodyBytes)
	if err != nil {
		settlement.refund()
		d.opts.WriteError(w, rc.RequestID, err)
		return
	}
	// 信任顺序 ②：payload 四形态推导。
	if !ok {
		if imageAdapter, supported := adapter.(contract.ImageAdapter); supported {
			count, ok = imageAdapter.CountImages(body)
		}
	}
	fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: body, Model: rc.UpstreamModel}, result.Response.Header, nil, rc.ChannelType)
	applyImageCount(fact, count, ok)
	applyImageTokenFloor(&fact.Usage)
	if rc.OriginModel != "" {
		fact.Model = rc.OriginModel
	}
	fact.LatencyMS = int(time.Since(start).Milliseconds())
	fact.Stream = false
	fact.Attempts = rc.AttemptIndex + 1
	settlement.terminate(*fact)

	writeUpstream(w, rc.RequestID, result.Response, body)
}

// applyImageCount 把三步信任顺序的结果写进用量事实。
//
// ①② 都不成立 → Source=reserved_default：计费会话据此保持请求预留量
// （billing/session.go adoptImageCount），绝不凭空造 0 张（§8.1.2）。
func applyImageCount(fact *contract.UsageFact, count int, ok bool) {
	if !ok {
		fact.Usage.Source = contract.UsageReservedDefault
		return
	}
	fact.ImageCount = count
	if fact.Usage.Source == contract.UsageReservedDefault {
		// 计数本身可用：清掉 T3 因 payload 异常留下的「保持预留量」标记。
		fact.Usage.Source = contract.UsageEstimated
	}
}

// serveImageStream 逐事件转发真实 SSE；上游是 JSON 时按 relay-04 §8.1.4
// 拆成 image_generation.completed 事件并补 [DONE]。
func (d *Dispatcher) serveImageStream(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, plan *RequestPlan, result *relay.AttemptResult, settlement *settlement, upstreamSSE bool) {
	start := time.Now()
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	if rc.RequestID != "" {
		header.Set("X-Request-ID", rc.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	writer := newSSEWriter(w)
	writer.flush()

	var (
		accumulated contract.Usage
		sawUsage    bool
		completed   int
		wroteDone   bool
		firstToken  *int
		clientGone  bool
	)
	recordFirst := func() {
		if firstToken == nil {
			ms := int(time.Since(start).Milliseconds())
			firstToken = &ms
		}
	}
	writeFrame := func(frame []byte) bool {
		if !writer.raw(frame) {
			clientGone = true
			return false
		}
		recordFirst()
		return true
	}

	// R-15（控制端裁决）：图像流的中断语义以 relay-04 §8.1.3 为准，
	// 不走通用的「失败整笔退款」：
	//   - 正常结束：信任 completed 计数（允许低于请求 n）；
	//   - 客户端中断：费用不得低于请求的 n（取 max(completed, n)）；
	//   - completed 超过请求 n：照实上调（actual higher count）。
	requested := plan.estimate.ImageCount
	if requested <= 0 {
		requested = plan.estimate.AttemptImageCount
	}
	if requested <= 0 {
		requested = 1
	}
	finalize := func(aborted bool, jsonBody []byte) {
		count := completed
		if aborted && count < requested {
			count = requested
		}
		var fact *contract.UsageFact
		if len(jsonBody) > 0 {
			fact = usage.ExtractUsageMultimodal(&contract.Response{Raw: jsonBody, Model: rc.UpstreamModel}, result.Response.Header, nil, rc.ChannelType)
		} else {
			fact = &contract.UsageFact{Model: rc.OriginModel, Usage: accumulated}
			if !sawUsage || !usageHasFact(accumulated) {
				fact.Usage = contract.Usage{Source: contract.UsageEstimated}
			}
		}
		// 直接写 ImageCount（数值来自 completed 事件/请求 n），不再经过
		// payload 解析的 reserved_default 回退——流式路径天然没有完整 payload。
		if fact.Usage.Source == contract.UsageReservedDefault {
			fact.Usage.Source = contract.UsageEstimated
		}
		applyImageTokenFloor(&fact.Usage)
		if rc.OriginModel != "" {
			fact.Model = rc.OriginModel
		}
		fact.ImageCount = count
		fact.LatencyMS = int(time.Since(start).Milliseconds())
		fact.FirstTokenMS = firstToken
		fact.Stream = true
		fact.Attempts = rc.AttemptIndex + 1
		settlement.terminate(*fact)
	}

	finalizeReserved := func() {
		// §8.1.2：payload 解析异常时保持请求预留量（reserved_default），
		// 不按 0 张静默收费。
		settlement.terminate(contract.UsageFact{
			Model:        rc.OriginModel,
			Usage:        contract.Usage{Source: contract.UsageReservedDefault},
			LatencyMS:    int(time.Since(start).Milliseconds()),
			FirstTokenMS: firstToken,
			Stream:       true,
			Attempts:     rc.AttemptIndex + 1,
		})
	}

	if !upstreamSSE {
		body, err := readBounded(result.Response.Body, maxResponseBodyBytes)
		if err != nil {
			settlement.refund()
			return
		}
		items, valid, malformed := imagePayloadItems(body)
		if !valid {
			settlement.refund()
			return
		}
		if malformed {
			// 类型非法的 payload（如 url 为数字）：不可信，保持预留量。
			finalizeReserved()
			return
		}
		// F1：按 relay-04 §8.1.4 把每张图**展平**成
		// `image_generation.completed` 事件（created_at/url/b64_json/
		// revised_prompt 与 usage 平铺在顶层），而不是塞进 `data` 子对象——
		// OpenAI 客户端按顶层字段提取图片。
		var meta struct {
			Created int64           `json:"created"`
			Usage   json.RawMessage `json:"usage"`
		}
		_ = json.Unmarshal(body, &meta)
		createdAt := meta.Created
		if createdAt == 0 {
			createdAt = time.Now().Unix()
		}
		for _, item := range items {
			event, err := imageCompletedEventPayload(item, createdAt, meta.Usage)
			if err != nil {
				clientGone = true
				break
			}
			frame := append([]byte("data: "), event...)
			frame = append(frame, '\n', '\n')
			if !writeFrame(frame) {
				break
			}
			completed++
		}
		if clientGone {
			// 客户端在事件转发途中断开：按 §8.1.3 结算 max(completed, n)。
			finalize(true, body)
			return
		}
		if !writer.done() {
			finalize(true, body)
			return
		}
		finalize(false, body)
		return
	}

	watched, inactivityTimedOut, stopWatch := d.watchStream(result.Response.Body)
	defer stopWatch()
	reader := bufio.NewReader(watched)
	readErrored := false
	for {
		if r.Context().Err() != nil {
			clientGone = true
			break
		}
		frame, readErr := readSSEFrame(reader)
		if len(frame) > 0 {
			if payload := sseDataPayload(frame); payload != "" {
				if payload == "[DONE]" {
					wroteDone = true
				} else {
					if imageCompletedEvent([]byte(payload)) {
						completed++
					}
					if hasUsagePayload([]byte(payload)) {
						fact := usage.ExtractUsageMultimodal(&contract.Response{Raw: []byte(payload)}, nil, nil, rc.ChannelType)
						mergeUsageLocal(&accumulated, fact.Usage)
						sawUsage = true
					}
				}
			}
			if !writeFrame(frame) {
				break
			}
		}
		if readErr != nil {
			// F3：只有 io.EOF 是「上游正常结束」；连接被重置/截断或看门狗
			// 超时都必须按异常处理——不能伪造 [DONE]，费用按中断口径
			// max(completed, n) 结算。
			if !errors.Is(readErr, io.EOF) || inactivityTimedOut.Load() {
				readErrored = true
			}
			break
		}
	}
	if clientGone || readErrored {
		// 客户端断开/写失败/上游读取错误/看门狗超时：图像流按 R-15/§8.1.3
		// 结算 max(completed, n)，费用不低于请求 n；不写 [DONE]。经 T3 的
		// 一次性关闭守卫收敛（同一 ctx 只执行一次）。
		_ = usage.OnStreamClose(r.Context(), func() error {
			finalize(true, nil)
			return nil
		})
		return
	}
	if !wroteDone && !writer.done() {
		finalize(true, nil)
		return
	}

	// 正常结束：信任 completed 计数（relay-04 §8.1.3）。
	finalize(false, nil)
}

// serveAudioSpeech 以 io.Copy 直传二进制音频（GC-6：不整段缓冲）。
func (d *Dispatcher) serveAudioSpeech(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, plan *RequestPlan, result *relay.AttemptResult, settlement *settlement) {
	start := time.Now()
	contentType := binaryContentType(result.Response.Header.Get("Content-Type"), plan.responseFormat)
	w.Header().Set("Content-Type", contentType)
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	w.WriteHeader(result.Response.StatusCode)
	writer := &flushWriter{w: w}

	watched, _, stopWatch := d.watchStream(result.Response.Body)
	defer stopWatch()
	captured := newCaptureReader(watched, audioDurationPrefixBytes)
	written, copyErr := io.Copy(writer, captured)
	if copyErr != nil || r.Context().Err() != nil {
		// 客户端断开/写失败/看门狗超时：不得漏释放预算（R-85）。终结器
		// 经 T3 的一次性关闭守卫执行（同一请求只退款一次），本包不再
		// 另建一条并行路径（F11：usage.OnStreamClose 的生产调用方）。
		_ = usage.OnStreamClose(r.Context(), func() error {
			settlement.refund()
			return nil
		})
		return
	}
	writer.flush()

	format := plan.responseFormat
	if format == "" {
		format = formatFromContentType(contentType)
	}
	var fact *contract.UsageFact
	seconds, durationOK := 0.0, false
	if format == "pcm" {
		// pcm 没有可解析的文件头：按固定参数 + 实际字节数计算（§8.2.3）。
		if written > 0 {
			seconds = float64(written) / float64(pcmSampleRate*pcmBytesPerSample)
			durationOK = true
		}
	} else {
		seconds, durationOK = AudioDurationWithSize(captured.prefix.Bytes(), written, format)
	}
	if durationOK {
		seconds = clampAudioSeconds(rc, seconds)
		injected := contract.Usage{
			AudioSeconds: pricing.AudioSecondsFromDuration(seconds),
			Source:       contract.UsageEstimated,
		}
		fact = usage.ExtractUsageMultimodal(&contract.Response{Binary: []byte{0}}, nil, &injected, rc.ChannelType)
	} else {
		// 取不到时长：每 KB ≈ 1 token 兜底（乙类怪癖，§8.2.3）。
		u := fallbackAudioUsage(int(written))
		fact = &contract.UsageFact{Usage: u}
	}
	if rc.OriginModel != "" {
		fact.Model = rc.OriginModel
	}
	fact.LatencyMS = int(time.Since(start).Milliseconds())
	fact.Stream = plan.stream
	fact.Attempts = rc.AttemptIndex + 1
	settlement.terminate(*fact)
}

// serveRealtime 是实时端点的 P0 骨架：只完成鉴权后的渠道解析与上游握手；
// 双向桥接后置（relay-07 §3）。成功建连后明确 501，绝不假装已转发。
func (d *Dispatcher) serveRealtime(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, plan *RequestPlan) {
	connection := strings.ToLower(r.Header.Get("Connection"))
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	if !strings.Contains(connection, "upgrade") || upgrade != "websocket" {
		d.writeEntryError(w, rc.RequestID, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "websocket upgrade required"})
		return
	}
	candidates, err := d.opts.Candidates(r.Context(), rc)
	if err != nil || len(candidates) == 0 {
		d.opts.WriteError(w, rc.RequestID, fmt.Errorf("%w", relay.ErrNoAvailableChannel))
		return
	}
	chosen := candidates[0]
	// F6：realtime 也要遵守管理员 pin；pin 不在候选集内按 404 语义快失败。
	if pinID, hasPin := relay.ParseChannelPin(r); hasPin {
		found := false
		for _, candidate := range candidates {
			if candidate.ChannelID == pinID {
				chosen = candidate
				found = true
				break
			}
		}
		if !found {
			d.opts.WriteError(w, rc.RequestID, fmt.Errorf("%w: channel %d", relay.ErrChannelNotFound, pinID))
			return
		}
	}
	assignment, err := d.opts.ResolveChannel(r.Context(), rc, chosen.ChannelID)
	if err != nil {
		d.opts.WriteError(w, rc.RequestID, fmt.Errorf("%w: %v", relay.ErrNoAvailableChannel, err))
		return
	}
	assignment.ChannelID = chosen.ChannelID
	relay.WithChannel(rc, assignment)

	adapter, err := d.adapterFor(assignment.ChannelType, plan)
	if err != nil {
		d.opts.WriteError(w, rc.RequestID, err)
		return
	}
	realtime, ok := adapter.(contract.RealtimeAdapter)
	if !ok {
		d.writeEntryError(w, rc.RequestID, &entryError{status: http.StatusBadRequest, code: "endpoint_not_supported", message: "the channel does not support this endpoint"})
		return
	}
	conn, err := realtime.DialUpstream(rc)
	if err != nil {
		d.opts.WriteError(w, rc.RequestID, err)
		return
	}
	_ = conn.Close()
	// P0 骨架：握手成功后仍不桥接（进阶后置）。明确 501 而不是挂起连接。
	d.writeEntryError(w, rc.RequestID, &entryError{
		status:  http.StatusNotImplemented,
		code:    "not_implemented",
		message: "realtime bridging is not implemented in this milestone",
	})
}

// ============================================================================
// 输出 helper
// ============================================================================

func writeUpstream(w http.ResponseWriter, requestID string, resp *http.Response, body []byte) {
	contentType := resp.Header.Get("Content-Type")
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	status := resp.StatusCode
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		status = http.StatusOK
	}
	// 上游 201 Created 视为成功并原样回写（relay-04 §4.3）。
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeFallbackError 是 Options.WriteError 的缺省实现（生产会注入 httpapi 的
// 统一映射；测试/嵌入方也能拿到稳定的 OpenAI 信封）。
func writeFallbackError(w http.ResponseWriter, requestID string, err error) {
	status := http.StatusBadGateway
	code := "upstream_error"
	message := "upstream request failed"
	if errors.Is(err, adapters.ErrEndpointUnsupported) {
		status = http.StatusBadRequest
		code = "endpoint_not_supported"
		message = "the channel does not support this endpoint"
	}
	writeOpenAIError(w, status, requestID, code, message)
}

func (d *Dispatcher) writeEntryError(w http.ResponseWriter, requestID string, err error) {
	var entry *entryError
	if errors.As(err, &entry) {
		writeOpenAIError(w, entry.status, requestID, entry.code, entry.message)
		return
	}
	writeOpenAIError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid request")
}

// writeOpenAIError 输出与 httpapi.writeRelayError 同形的错误信封（避免本包
// 反向依赖 httpapi 造成 import 环）。
func writeOpenAIError(w http.ResponseWriter, status int, requestID, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
		message = message + " (requestId: " + requestID + ")"
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
}

// sseWriter 是流式图像的唯一写出口（单协程写，无需互斥）。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	failed  bool
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *sseWriter) raw(frame []byte) bool {
	if s.failed {
		return false
	}
	if _, err := s.w.Write(frame); err != nil {
		s.failed = true
		return false
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return true
}

func (s *sseWriter) done() bool {
	return s.raw([]byte("data: [DONE]\n\n"))
}

// flushWriter 在每次 Write 后 Flush，保证二进制流边写边发（GC-6）。
type flushWriter struct {
	w http.ResponseWriter
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func (f *flushWriter) flush() {
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// readSSEFrame 读取一个以空行结尾的 SSE 事件块（保留原始字节）。
func readSSEFrame(reader *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			frame.WriteString(line)
		}
		if err != nil {
			return frame.Bytes(), err
		}
		if line == "\n" || line == "\r\n" {
			return frame.Bytes(), nil
		}
	}
}

// sseDataPayload 合并事件块内的 data 行（与 httpapi.sseDataPayload 同语义）。
func sseDataPayload(frame []byte) string {
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func hasUsagePayload(payload []byte) bool {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	_, ok := envelope["usage"]
	return ok
}

// mergeUsageLocal 与 httpapi.mergeUsage 同语义：非零才覆盖，绝不擦除已有维度。
func mergeUsageLocal(dst *contract.Usage, src contract.Usage) {
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
	if src.CacheWrite != 0 {
		dst.CacheWrite = src.CacheWrite
	}
	if src.Reasoning != 0 {
		dst.Reasoning = src.Reasoning
	}
	if src.AudioSeconds != 0 {
		dst.AudioSeconds = src.AudioSeconds
	}
	if src.PromptDetail != (contract.UsageDetail{}) {
		dst.PromptDetail = src.PromptDetail
	}
	if src.CompletionDetail != (contract.UsageDetail{}) {
		dst.CompletionDetail = src.CompletionDetail
	}
	if src.Raw != nil {
		dst.Raw = src.Raw
	}
	if usageHasFact(src) {
		dst.Source = src.Source
	}
}

func usageHasFact(u contract.Usage) bool {
	return u.Prompt != 0 || u.Completion != 0 || u.Total != 0 ||
		u.CacheRead != 0 || u.CacheWrite != 0 || u.Reasoning != 0 ||
		u.AudioSeconds != 0 || u.Reserved != 0 ||
		u.PromptDetail != (contract.UsageDetail{}) ||
		u.CompletionDetail != (contract.UsageDetail{})
}

// ============================================================================
// multipart 读取
// ============================================================================

type multipartFile struct {
	filename    string
	contentType string
	data        []byte
}

// maxMultipartTextFieldBytes 与 httpapi 入口的单字段上界一致（1 MiB，
// MaxExtraParamBytes 同量级）。N4：此前 dispatcher 只读 64 KiB 且不检查
// 截断，会让入口已接受的合法 parameters 在二次解析时被悄悄砍掉。
const maxMultipartTextFieldBytes = 1 << 20

type multipartParsed struct {
	fields map[string]string
	files  map[string]multipartFile
}

// parseMultipart 顺序读取 multipart 表单。captureFiles=false 时只记录文件名
// 与大小（图像规划不需要文件内容，避免二次复制大文件）。
func parseMultipart(body []byte, contentType string, captureFiles bool) (multipartParsed, error) {
	parsed := multipartParsed{fields: map[string]string{}, files: map[string]multipartFile{}}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return parsed, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "multipart/form-data is required for this endpoint"}
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return parsed, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "invalid multipart form"}
		}
		name := part.FormName()
		if part.FileName() != "" || name == "file" || name == "image" || name == "mask" {
			var data []byte
			if captureFiles {
				data, err = io.ReadAll(io.LimitReader(part, maxResponseBodyBytes))
				if err != nil {
					_ = part.Close()
					return parsed, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "invalid multipart form"}
				}
			} else {
				_, _ = io.Copy(io.Discard, part)
			}
			parsed.files[name] = multipartFile{
				filename:    part.FileName(),
				contentType: part.Header.Get("Content-Type"),
				data:        data,
			}
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxMultipartTextFieldBytes+1))
		_ = part.Close()
		if err != nil {
			return parsed, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "invalid multipart form"}
		}
		if len(data) > maxMultipartTextFieldBytes {
			// N4：与 httpapi 入口的 1 MiB 上界对齐；超限必须显式报错，
			// 不能静默截断后把半截 JSON 交给下游。
			return parsed, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "multipart field exceeds the 1 MiB limit"}
		}
		parsed.fields[name] = string(data)
	}
	return parsed, nil
}

// multipartFileByName 读取指定文件 part（用于转写/翻译的时长估算）。
func multipartFileByName(body []byte, contentType, name string) (multipartFile, error) {
	parsed, err := parseMultipart(body, contentType, true)
	if err != nil {
		return multipartFile{}, err
	}
	file, ok := parsed.files[name]
	if !ok || len(file.data) == 0 {
		return multipartFile{}, &entryError{status: http.StatusBadRequest, code: "invalid_request", message: "file is required"}
	}
	return file, nil
}
