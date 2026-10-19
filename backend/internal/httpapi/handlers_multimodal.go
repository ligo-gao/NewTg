package httpapi

// 本文件是 M4 Task 1 的「多模态端点族路由骨架」：只负责路由注册、鉴权后的
// RelayContext 组装与**入口校验**（relay-04 §0.1/§4、§9）。真正的归一化、
// 适配器分派、预扣/结算与二进制直传由 MultimodalDispatcher 的实现方（Task 4）
// 完成；T1 不解析 IR、不构造 AttemptPlan。
//
// 为什么入口校验放在这里，而不是等适配器转换时再做：
//   - billing.md 与 relay-04 §9 要求「凡成为计费乘数的用户可控量，必须在进入
//     配额计算前带上界，越界以 400 拒绝」。n / size / quality / prompt 都会
//     直接影响图像张数与 token，若放到上游才拒绝，既浪费一次上游调用，也让
//     构造超大乘数的请求进入预扣路径；
//   - multipart 与 JSON 两条入口都必须做**同强度**校验（relay-04 §4.2）——
//     multipart 绕过标准 DTO，是基线里最容易漏界的一条通道。
//
// 路由边界（relay-04 §4.4/§4.5、决议 H）：
//   - 不注册 POST /v1/images/variations：回到标准 404，不留 501 半死桩；
//   - 不注册 legacy POST /v1/edits：该路径在基线里路由到图像格式却按文本解析，
//     属于半死路径（错误分派），新系统只提供 /v1/images/edits；
//   - 不注册 GET /v1/models：M2 的 RelayHandler.handleModels 已实现该端点
//     （裁决 R-09），重复注册会让 ServeMux 在装配期 panic。

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
)

// ============================================================================
// 冻结常量（relay-04 §9、契约 §10.1）
//
// 常量集中在本文件的唯一所有者处（relay-04 §9 通则：禁止各处再造）。
// 导出是为了让 Task 4 的分派器与测试复用同一组上界，而不是各自硬编码。
// ============================================================================

const (
	// MaxImageN 是图像张数上界（契约 §10.1 冻结 128）。
	// n 是直接的计费乘数，必须上界；0 也在入口拒绝（合法区间 [1, MaxImageN]）。
	MaxImageN = 128

	// MaxImageSidePixels 是 size 单边像素上界（relay-04 §9 甲类修正：基线无上界）。
	MaxImageSidePixels = 8192

	// MaxImagePixels 是 size 总像素上界（relay-04 §9，= 8192×8192）。
	MaxImagePixels = 67_108_864

	// MaxImagePromptBytes 是图像 prompt 的字节上界。
	// relay-04 §9 只冻结了常量名、未给出数值；取与 MaxTTSTextBytes /
	// MaxAudioMetadataBytes 一致的 64 KiB，作为入口防御上界（非计费乘数）。
	MaxImagePromptBytes = 64 << 10

	// MaxPartialImages 是 partial_images 上界（relay-04 §9 冻结 8）。
	// 它决定流式图像的事件数，属于用户可控的间接乘数。
	MaxPartialImages = 8

	// MaxTTSTextBytes 是 TTS 输入文本上界（relay-04 §9 冻结 64 KiB）。
	MaxTTSTextBytes = 64 << 10

	// MaxAudioFileBytes 是音频上传文件上界（relay-04 §9、security-04 §3.2 冻结 25 MiB）。
	MaxAudioFileBytes = 25 << 20

	// MinTTSSpeed / MaxTTSSpeed 是 TTS speed 的合法闭区间（relay-04 §9 甲类修正）。
	MinTTSSpeed = 0.25
	MaxTTSSpeed = 4.0
)

const (
	// maxMultipartFieldBytes 限制单个 multipart 文本字段的读取量。
	// 与 MaxExtraParamBytes（1 MiB）同量级：parameters 等透传字段可能接近该上界，
	// 其余表单字段远小于它；它是防止畸形 multipart 把单个字段读爆的兜底，
	// 与整体 maxRelayBodyBytes 共同构成内存上界。
	maxMultipartFieldBytes = 1 << 20

	// multimodalRawFormat 标注多模态族的入站形态。这些端点的对外契约均按
	// OpenAI 形态暴露；真正的分派依据是 Handle 的 endpoint 参数，RawFormat
	// 只用于让 Task 4 知道请求体是 OpenAI 风格的原始 JSON / multipart。
	multimodalRawFormat contract.RelayFormat = "openai"
)

// MultimodalDispatcher 由 Task 4 实现并注入（relay-04 §0.2 统一入口的后半段）。
//
// 实现方负责：归一化 → 适配器分派 → 预扣/结算 → 响应（含二进制直传）。
// 参数 endpoint 是路由层已判定的端点键；原始请求体由调用方恢复在 r.Body
// 与 rc.Request.Raw 中，multipart 的 boundary 仍保留在 r.Header 里。
type MultimodalDispatcher interface {
	Handle(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, endpoint contract.Endpoint)
}

// MultimodalHandlerOptions 是 httpapi 内部的装配点。
type MultimodalHandlerOptions struct {
	// Tokens 用于 BuildRelayContext 回源令牌分组（与 M2 /v1/chat/completions 同一接缝）。
	Tokens auth.TokenLookup
	// Dispatcher 为 nil 时端点返回 501（仅 Task 1 测试态；Task 4 必须注入真实实现）。
	Dispatcher MultimodalDispatcher
}

// MultimodalHandler 持有入口校验之外的最小依赖。
type MultimodalHandler struct {
	tokens     auth.TokenLookup
	dispatcher MultimodalDispatcher
}

// NewMultimodalHandler 构造多模态端点 handler。
// tokens 允许为 nil（鉴权中间件已独立校验令牌）；BuildRelayContext 会在
// 需要分组回源时 fail-closed（ErrTokenInvalid → 401）。
func NewMultimodalHandler(opts MultimodalHandlerOptions) *MultimodalHandler {
	return &MultimodalHandler{tokens: opts.Tokens, dispatcher: opts.Dispatcher}
}

// RegisterMultimodalRoutes 把多模态端点族挂到 mux。
//
// auth 必须是 RelayAuth 构造的中间件：多模态端点与 /v1/chat/completions 共用
// 同一套令牌/用户/IP 判定（relay-04 §0.2 统一入口）。nil mux/h 视为装配错误，
// 直接 panic 而不是静默少挂路由（fail-closed）。
func RegisterMultimodalRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler, h *MultimodalHandler) {
	if mux == nil {
		panic("httpapi: RegisterMultimodalRoutes requires a mux")
	}
	if h == nil {
		panic("httpapi: RegisterMultimodalRoutes requires a handler")
	}
	if auth == nil {
		panic("httpapi: RegisterMultimodalRoutes requires the relay auth middleware")
	}
	register := func(method, path string, endpoint contract.Endpoint) {
		mux.Handle(method+" "+path, auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.serve(w, r, endpoint)
		})))
	}
	register(http.MethodPost, "/v1/images/generations", contract.EndpointImageGenerate)
	register(http.MethodPost, "/v1/images/edits", contract.EndpointImageEdit)
	register(http.MethodPost, "/v1/embeddings", contract.EndpointEmbeddings)
	register(http.MethodPost, "/v1/rerank", contract.EndpointRerank)
	register(http.MethodPost, "/v1/audio/speech", contract.EndpointAudioSpeech)
	register(http.MethodPost, "/v1/audio/transcriptions", contract.EndpointAudioTranscribe)
	register(http.MethodPost, "/v1/audio/translations", contract.EndpointAudioTranslate)
	// 实时骨架：WebSocket 握手是 GET（relay-07 G-06 冻结 `GET /v1/realtime`）。
	// M4 计划草图写的 POST 已被该裁决取代；这里只注册 GET，避免留下一个
	// 永远不会收到升级请求的 POST 半死路径。升级与双向桥接由 T4/T5 负责，
	// T1 只保证鉴权与上下文注入与其它端点一致。
	register(http.MethodGet, "/v1/realtime", contract.EndpointRealtime)
}

// serve 是所有多模态端点的公共流程：读体 → 入口校验 → 组装 RelayContext →
// 交给 Dispatcher。
//
// 为什么不在这里做限流/定价：它们的唯一入口是 billing 接缝与路由层（contract
// §6），本层只承担 relay-04 §2 阶段 4–5 的职责。
func (h *MultimodalHandler) serve(w http.ResponseWriter, r *http.Request, endpoint contract.Endpoint) {
	requestID := newRelayRequestID()

	body, err := readMultimodalBody(w, r)
	if err != nil {
		writeMultimodalEntryError(w, requestID, err)
		return
	}
	// 复原 body：入口校验是一次性的，但 Dispatcher（尤其 multipart）需要完整
	// 请求体；与 handleChatCompletions 的复原姿势一致。
	r.Body = io.NopCloser(bytes.NewReader(body))

	model, err := validateMultimodalEntry(endpoint, r.Header.Get("Content-Type"), body)
	if err != nil {
		writeMultimodalEntryError(w, requestID, err)
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		// 只有中间件未挂载才会到这里；fail-closed，绝不把零值身份当匿名用户。
		writeRelayError(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, code, message := mapRelayContextError(err)
		writeRelayError(w, status, requestID, code, message)
		return
	}
	// 归一化层（Task 4）随后仍是 Request 的权威写入方；这里只写入入口阶段
	// 已确定的模型名与原始 body，保证 OriginModel（路由按它选渠道）可用。
	relay.WithRequest(rc, &contract.Request{
		Model: model,
		Raw:   contract.ProtocolBody(body),
	}, multimodalRawFormat, r.URL.Path)

	if h.dispatcher == nil {
		// Task 1 的测试态：路由与校验已完成，但没有真实分派器。
		// Task 4 必须在 platform 装配时注入；最终 DoD 不允许保留 501 桩。
		writeRelayError(w, http.StatusNotImplemented, requestID, "not_implemented",
			"multimodal dispatcher is not configured")
		return
	}
	h.dispatcher.Handle(w, r, rc, endpoint)
}

// readMultimodalBody 用 http.MaxBytesReader 读取请求体。
//
// 与 /v1/chat/completions 的 maxRelayBodyBytes 同语义（32 MiB），但 multipart
// 必须走 MaxBytesReader：它在超限时立刻中止读取并让 handler 能映射 413，
// 而不是先把整个文件读进内存再比较大小（防 OOM）。
func readMultimodalBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := http.MaxBytesReader(w, r.Body, maxRelayBodyBytes)
	body, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, &multimodalEntryError{
				status:  http.StatusRequestEntityTooLarge,
				code:    "request_too_large",
				message: "request body exceeds the 32 MiB limit",
			}
		}
		return nil, invalidMultimodalEntry("failed to read request body")
	}
	return body, nil
}

// ============================================================================
// 入口校验
// ============================================================================

// multimodalEntryError 是入口校验的可直接下发给客户端的错误。
// message 只含固定文案与上界数值，绝不包含上游/内部细节。
type multimodalEntryError struct {
	status  int
	code    string
	message string
}

func (e *multimodalEntryError) Error() string { return e.message }

func invalidMultimodalEntry(message string) error {
	return &multimodalEntryError{status: http.StatusBadRequest, code: "invalid_request", message: message}
}

func writeMultimodalEntryError(w http.ResponseWriter, requestID string, err error) {
	var entryErr *multimodalEntryError
	if errors.As(err, &entryErr) {
		writeRelayError(w, entryErr.status, requestID, entryErr.code, entryErr.message)
		return
	}
	writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid request")
}

// validateMultimodalEntry 按端点分派入口校验，返回路由需要的客户端模型名。
//
// 校验强度按 relay-04 §9 的「用户声明的量 → 400 拒绝」口径：图像 n/size/
// quality/prompt、TTS speed/input、音频上传文件与 model 必填。音频时长等
// 「上游测得的量」不在此拒绝（由适配器饱和并审计）。
func validateMultimodalEntry(endpoint contract.Endpoint, contentType string, body []byte) (string, error) {
	switch endpoint {
	case contract.EndpointImageGenerate:
		return validateImageJSON(body)
	case contract.EndpointImageEdit:
		// 编辑端点同时接受 multipart 与 JSON（relay-04 §4.1）；两条路径必须
		// 做同强度校验，否则 multipart 会成为绕过上界的通道。
		if isMultipartForm(contentType) {
			return validateImageMultipart(contentType, body)
		}
		return validateImageJSON(body)
	case contract.EndpointAudioSpeech:
		return validateSpeechJSON(body)
	case contract.EndpointAudioTranscribe, contract.EndpointAudioTranslate:
		return validateAudioMultipart(contentType, body)
	case contract.EndpointRealtime:
		// WebSocket 升级请求没有可解析的 JSON body；模型/协议由 Task 4/T5 处理。
		return "", nil
	default:
		// embeddings / rerank：T1 只做 JSON 结构校验与模型提取，字段级上界
		// 由对应的归一化实现（Task 2/T4）继续执行。
		return validateLooseJSON(body)
	}
}

// imageJSONFields 只声明入口校验关心的字段；未知字段原样保留在 body 中，
// 供 Task 2/T4 的归一化与透传使用。
type imageJSONFields struct {
	Model         *string            `json:"model"`
	Prompt        *string            `json:"prompt"`
	N             optionalJSONNumber `json:"n"`
	Size          *string            `json:"size"`
	Quality       *string            `json:"quality"`
	PartialImages optionalJSONNumber `json:"partial_images"`
	Parameters    json.RawMessage    `json:"parameters"`
}

func validateImageJSON(body []byte) (string, error) {
	var payload imageJSONFields
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	model, err := validateImageFields(payload.Model, payload.Prompt, payload.N, payload.Size, payload.Quality, payload.PartialImages)
	if err != nil {
		return "", err
	}
	if err := validateImageParameters(payload.Parameters); err != nil {
		return "", err
	}
	return model, nil
}

// validateImageFields 是 JSON 与 multipart 共用的图像字段校验。
func validateImageFields(model, prompt *string, n optionalJSONNumber, size, quality *string, partialImages optionalJSONNumber) (string, error) {
	if model == nil || strings.TrimSpace(*model) == "" {
		return "", invalidMultimodalEntry("model is required")
	}
	if prompt == nil || strings.TrimSpace(*prompt) == "" {
		return "", invalidMultimodalEntry("prompt is required")
	}
	if len(*prompt) > MaxImagePromptBytes {
		return "", invalidMultimodalEntry("prompt exceeds the 64 KiB limit")
	}
	if err := validateImageCount(n); err != nil {
		return "", err
	}
	if size != nil && *size != "" {
		if err := validateImageSize(*size); err != nil {
			return "", err
		}
	}
	if quality != nil && *quality != "" {
		if err := validateImageQuality(*quality); err != nil {
			return "", err
		}
	}
	if partialImages.Set {
		value, ok := jsonNumberInt(partialImages)
		if !ok || value < 0 || value > MaxPartialImages {
			return "", invalidMultimodalEntry("partial_images must be an integer in [0, 8]")
		}
	}
	return *model, nil
}

// validateImageCount 校验顶层 n：缺省视为 1，显式值必须是 [1, MaxImageN] 的整数。
// 用严格数字类型而不是 *int：`2.0`/`2e0` 是合法 JSON 数字且语义等于 2；
// 解码到 int 会失败并静默回落成缺省值，从而绕过上界检查。字符串 "2" 必须拒绝。
func validateImageCount(n optionalJSONNumber) error {
	if !n.Set {
		return nil
	}
	value, ok := jsonNumberInt(n)
	if !ok || value < 1 || value > MaxImageN {
		return invalidMultimodalEntry("n must be an integer in [1, 128]")
	}
	return nil
}

// validateImageParameters 校验 Extra["parameters"] 里的计费乘数
// （relay-04 §4.2/§9：parameters.n 与顶层 n 同强度上界）。
func validateImageParameters(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var params struct {
		N                     optionalJSONNumber `json:"n"`
		UseProviderParameters *bool              `json:"useProviderParameters"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return invalidMultimodalEntry("parameters must be a JSON object")
	}
	if !params.N.Set {
		return nil
	}
	value, ok := jsonNumberInt(params.N)
	if !ok || value < 0 || value > MaxImageN {
		return invalidMultimodalEntry("parameters.n must be an integer in [0, 128]")
	}
	// useProviderParameters 时 n==0 表示「让上游决定张数」；这会绕过本层
	// 对计费乘数的上界约束，因此显式拒绝（relay-04 §4.2/§4.9）。
	if params.UseProviderParameters != nil && *params.UseProviderParameters && value == 0 {
		return invalidMultimodalEntry("parameters.n must be positive when useProviderParameters is set")
	}
	return nil
}

// validateImageSize 校验 size 形如 `{w}x{h}` 或 `auto`。
//
// 只接受 ASCII 小写 x：基线的「禁 ×」规则保留（relay-04 §9），避免同义字符
// 绕过解析后在上游被解释成不同尺寸。
func validateImageSize(size string) error {
	if size == "auto" {
		return nil
	}
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return invalidMultimodalEntry("size must be '{width}x{height}' or 'auto'")
	}
	width, ok := parsePositiveInt(parts[0])
	if !ok {
		return invalidMultimodalEntry("size must be '{width}x{height}' or 'auto'")
	}
	height, ok := parsePositiveInt(parts[1])
	if !ok {
		return invalidMultimodalEntry("size must be '{width}x{height}' or 'auto'")
	}
	if width > MaxImageSidePixels || height > MaxImageSidePixels || width*height > MaxImagePixels {
		return invalidMultimodalEntry("size exceeds the 8192x8192 pixel limit")
	}
	return nil
}

// validateImageQuality 校验 quality 六值枚举（relay-04 §9 甲类修正）。
func validateImageQuality(quality string) error {
	switch quality {
	case "standard", "hd", "auto", "low", "medium", "high":
		return nil
	default:
		return invalidMultimodalEntry("quality must be one of standard, hd, auto, low, medium, high")
	}
}

// validateSpeechJSON 校验 TTS 的入口字段：model 必填、input 字节上界、
// speed 闭区间（relay-04 §3.2/§9）。
func validateSpeechJSON(body []byte) (string, error) {
	var payload struct {
		Model *string            `json:"model"`
		Input *string            `json:"input"`
		Speed optionalJSONNumber `json:"speed"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	if payload.Model == nil || strings.TrimSpace(*payload.Model) == "" {
		return "", invalidMultimodalEntry("model is required")
	}
	if payload.Input != nil && len(*payload.Input) > MaxTTSTextBytes {
		return "", invalidMultimodalEntry("input exceeds the 64 KiB limit")
	}
	if payload.Speed.Set {
		speed, err := strconv.ParseFloat(payload.Speed.Value.String(), 64)
		if err != nil || math.IsNaN(speed) || speed < MinTTSSpeed || speed > MaxTTSSpeed {
			return "", invalidMultimodalEntry("speed must be between 0.25 and 4.0")
		}
	}
	return *payload.Model, nil
}

// validateLooseJSON 用于 embeddings / rerank 等端点：只保证请求体是 JSON 对象
// 并尽力提取 model。字段级校验归各自的归一化实现（T2/T4）。
func validateLooseJSON(body []byte) (string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", invalidMultimodalEntry("request body must be a JSON object")
	}
	var payload struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	if payload.Model != nil {
		return *payload.Model, nil
	}
	return "", nil
}

// ============================================================================
// multipart 校验
// ============================================================================

func isMultipartForm(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "multipart/form-data"
}

// newMultipartReader 从已读取的 body 构造 multipart reader。
// 不能用 r.ParseMultipartForm：它在解析时会把文件写进临时文件/内存，
// 而我们已经在 readMultimodalBody 用 MaxBytesReader 限了总大小；这里只需要
// 逐 part 的顺序读取，不保留文件内容。
func newMultipartReader(contentType string, body []byte) (*multipart.Reader, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, invalidMultimodalEntry("multipart/form-data is required for this endpoint")
	}
	return multipart.NewReader(bytes.NewReader(body), params["boundary"]), nil
}

// readMultipartField 读取一个文本字段并施加上界。
func readMultipartField(part *multipart.Part) (string, error) {
	data, err := io.ReadAll(io.LimitReader(part, maxMultipartFieldBytes+1))
	if err != nil {
		return "", invalidMultimodalEntry("invalid multipart form")
	}
	if len(data) > maxMultipartFieldBytes {
		return "", invalidMultimodalEntry("multipart field exceeds the 1 MiB limit")
	}
	return string(data), nil
}

// drainMultipartFile 丢弃文件 part 的内容，返回其字节数。
// 只复制到 io.Discard：文件内容由 Task 4 从恢复后的 r.Body 重新解析，
// 入口校验只需要大小，不需要在内存里保留第二份。
func drainMultipartFile(part *multipart.Part, limit int64) (int64, error) {
	written, err := io.Copy(io.Discard, io.LimitReader(part, limit+1))
	if err != nil {
		return 0, invalidMultimodalEntry("invalid multipart form")
	}
	return written, nil
}

// validateImageMultipart 对 /v1/images/edits 的 multipart 做与 JSON 同强度的校验。
// 缺省 n=1（relay-04 §4.2/基线怪癖），其余字段缺省不校验。
func validateImageMultipart(contentType string, body []byte) (string, error) {
	reader, err := newMultipartReader(contentType, body)
	if err != nil {
		return "", err
	}
	var (
		model, prompt, size, quality string
		parameters                   string
		nRaw                         string
		hasPrompt, hasN              bool
		hasParameters                bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", invalidMultimodalEntry("invalid multipart form")
		}
		name := part.FormName()
		// parameters 是计费乘数通道（Extra["parameters"].n），即使它以文件
		// part 的形式上传也必须按文本读取并走与 JSON 相同的校验——否则
		// multipart 会绕过 parameters.n 的上界（复审 F1）。
		if name == "parameters" {
			value, fieldErr := readMultipartField(part)
			_ = part.Close()
			if fieldErr != nil {
				return "", fieldErr
			}
			parameters = value
			hasParameters = true
			continue
		}
		// image / mask 等文件 part：只校验整体大小（已由 MaxBytesReader 限幅），
		// 内容留给 Task 4 重新解析。
		if part.FileName() != "" || name == "image" || name == "mask" {
			if _, err := drainMultipartFile(part, maxRelayBodyBytes); err != nil {
				_ = part.Close()
				return "", err
			}
			_ = part.Close()
			continue
		}
		value, err := readMultipartField(part)
		_ = part.Close()
		if err != nil {
			return "", err
		}
		switch name {
		case "model":
			model = value
		case "prompt":
			prompt = value
			hasPrompt = true
		case "size":
			size = value
		case "quality":
			quality = value
		case "n":
			nRaw = value
			hasN = true
		}
	}
	if strings.TrimSpace(model) == "" {
		return "", invalidMultimodalEntry("model is required")
	}
	if !hasPrompt || strings.TrimSpace(prompt) == "" {
		return "", invalidMultimodalEntry("prompt is required")
	}
	if len(prompt) > MaxImagePromptBytes {
		return "", invalidMultimodalEntry("prompt exceeds the 64 KiB limit")
	}
	if hasN {
		value, ok := parsePositiveInt(strings.TrimSpace(nRaw))
		if !ok || value > MaxImageN {
			return "", invalidMultimodalEntry("n must be an integer in [1, 128]")
		}
	}
	if size != "" {
		if err := validateImageSize(size); err != nil {
			return "", err
		}
	}
	if quality != "" {
		if err := validateImageQuality(quality); err != nil {
			return "", err
		}
	}
	if hasParameters {
		// 空字符串不是合法 JSON；与 JSON 入口 `"parameters":""` 同样拒绝。
		if strings.TrimSpace(parameters) == "" {
			return "", invalidMultimodalEntry("parameters must be a JSON object")
		}
		if err := validateImageParameters(json.RawMessage(parameters)); err != nil {
			return "", err
		}
	}
	return model, nil
}

// validateAudioMultipart 校验转写/翻译的 multipart：model 必填、file 必填
// 且不超过 25 MiB（relay-04 §3.2、security-04 §3.2）。
func validateAudioMultipart(contentType string, body []byte) (string, error) {
	reader, err := newMultipartReader(contentType, body)
	if err != nil {
		return "", err
	}
	var (
		model     string
		fileBytes int64
		fileSeen  bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", invalidMultimodalEntry("invalid multipart form")
		}
		name := part.FormName()
		if name == "file" {
			size, drainErr := drainMultipartFile(part, MaxAudioFileBytes)
			_ = part.Close()
			if drainErr != nil {
				return "", drainErr
			}
			if size > MaxAudioFileBytes {
				return "", invalidMultimodalEntry("audio file exceeds the 25 MiB limit")
			}
			fileSeen = true
			fileBytes += size
			continue
		}
		if part.FileName() != "" {
			// 其它文件字段不属于本端点契约；丢弃但有界，避免畸形表单拖垮内存。
			if _, drainErr := drainMultipartFile(part, maxRelayBodyBytes); drainErr != nil {
				_ = part.Close()
				return "", drainErr
			}
			_ = part.Close()
			continue
		}
		value, fieldErr := readMultipartField(part)
		_ = part.Close()
		if fieldErr != nil {
			return "", fieldErr
		}
		if name == "model" {
			model = value
		}
	}
	if strings.TrimSpace(model) == "" {
		return "", invalidMultimodalEntry("model is required")
	}
	// 零字节文件同样视为缺失：空文件无法提供时长，计费与识别都没有意义。
	if !fileSeen || fileBytes == 0 {
		return "", invalidMultimodalEntry("file is required")
	}
	return model, nil
}

// ============================================================================
// 数值 helper
// ============================================================================

// optionalJSONNumber 区分「字段缺席 / null」与「显式 JSON 数字」，并拒绝
// 字符串形式的数字（`"2"`）。encoding/json 的 json.Number 会接受带引号的
// 数字字面量，这会让 `"n":"129"` 之类的载荷绕过按数字类型的上界判定，
// 因此这里用自定义 UnmarshalJSON 只认裸数字 token。
type optionalJSONNumber struct {
	Set   bool
	Value json.Number
}

func (n *optionalJSONNumber) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		return errors.New("json: number field must not be quoted")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	number, ok := value.(json.Number)
	if !ok {
		return errors.New("json: expected a number")
	}
	n.Set = true
	n.Value = number
	return nil
}

// jsonNumberInt 把 JSON 数字严格转成整数。非整数（1.5）与超范围值返回 false。
func jsonNumberInt(raw optionalJSONNumber) (int64, bool) {
	if !raw.Set {
		return 0, true
	}
	value, err := strconv.ParseFloat(raw.Value.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
		return 0, false
	}
	if value < math.MinInt64 || value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

// parsePositiveInt 只接受纯十进制数字（拒绝 +/-、空格与指数形式），
// 避免 `8e3` 这类形态在入口与上游之间产生不同解释。
func parsePositiveInt(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
