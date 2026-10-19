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
	"path/filepath"
	"strconv"
	"strings"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/multimodal"
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
	MaxImageN = multimodal.MaxImageN

	// MaxImageSidePixels 是 size 单边像素上界（relay-04 §9 甲类修正：基线无上界）。
	MaxImageSidePixels = multimodal.MaxImageSidePixels

	// MaxImagePixels 是 size 总像素上界（relay-04 §9，= 8192×8192）。
	MaxImagePixels = multimodal.MaxImagePixels

	// MaxImagePromptBytes 是图像 prompt 的字节上界。
	// relay-04 §9 只冻结了常量名、未给出数值；取与 MaxTTSTextBytes /
	// MaxAudioMetadataBytes 一致的 64 KiB，作为入口防御上界（非计费乘数）。
	MaxImagePromptBytes = multimodal.MaxImagePromptBytes

	// MaxPartialImages 是 partial_images 上界（relay-04 §9 冻结 8）。
	MaxPartialImages = multimodal.MaxPartialImages

	// MaxImageEditInputBytes 是 images/edits 输入图片的字节上界
	// （security-04 §3.2：25 MiB，与音频上传同量级）。
	MaxImageEditInputBytes = multimodal.MaxImageEditInputBytes

	// MaxTTSTextBytes 是 TTS 输入文本上界（relay-04 §9 冻结 64 KiB）。
	MaxTTSTextBytes = multimodal.MaxTTSTextBytes

	// MaxAudioFileBytes 是音频上传文件上界（relay-04 §9、security-04 §3.2 冻结 25 MiB）。
	MaxAudioFileBytes = multimodal.MaxAudioFileBytes

	// MaxAudioMetadataBytes / MaxSpeechNewTokens 是音频透传载荷的上界
	// （relay-04 §9；常量来源在 multimodal 包，避免两处再造）。
	MaxAudioMetadataBytes = multimodal.MaxAudioMetadataBytes
	MaxSpeechNewTokens    = multimodal.MaxSpeechNewTokens

	// MinTTSSpeed / MaxTTSSpeed 是 TTS speed 的合法闭区间（relay-04 §9 甲类修正）。
	MinTTSSpeed = multimodal.MinTTSSpeed
	MaxTTSSpeed = multimodal.MaxTTSSpeed
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
	// ModelStatus 是派发前的模型停用检查（R-84/T1 复审 F2）：与 chat 路径同口径，
	// status=2 的模型必须在进入上游之前 404。nil 时跳过（测试/降级）。
	ModelStatus RelayModelStatusStore
	// Dispatcher 为 nil 时端点返回 501（仅入口骨架的直接构造场景；生产装配
	// 由 RelayHandler 注入真实实现，不会走到该分支）。
	Dispatcher MultimodalDispatcher
	// Groups 是 /pg/* 会话路径的用户分组查询面；nil 时仅令牌路径可用
	// （会话请求会 fail-closed 500）。
	Groups PlaygroundUserGroups
}

// MultimodalHandler 持有入口校验之外的最小依赖。
type MultimodalHandler struct {
	tokens      auth.TokenLookup
	modelStatus RelayModelStatusStore
	dispatcher  MultimodalDispatcher
	groups      PlaygroundUserGroups
}

// NewMultimodalHandler 构造多模态端点 handler。
// tokens 允许为 nil（鉴权中间件已独立校验令牌）；BuildRelayContext 会在
// 需要分组回源时 fail-closed（ErrTokenInvalid → 401）。
func NewMultimodalHandler(opts MultimodalHandlerOptions) *MultimodalHandler {
	return &MultimodalHandler{
		tokens:      opts.Tokens,
		modelStatus: opts.ModelStatus,
		dispatcher:  opts.Dispatcher,
		groups:      opts.Groups,
	}
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
	// Gemini 专用嵌入路径（R-11）：语义路径由模型名 + 动作后缀承载。
	// 通配只在 :embedContent / :batchEmbedContents 时归入嵌入族；其它
	// 动作在 serve 中回标准 404（不注册 501 桩）。
	register(http.MethodPost, "/v1/engines/{model}/embeddings", contract.EndpointEmbeddings)
	register(http.MethodPost, "/v1/models/{path...}", contract.EndpointEmbeddings)
	register(http.MethodPost, "/v1beta/models/{path...}", contract.EndpointEmbeddings)
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

	// Gemini 通配路径只接受嵌入动作后缀；其它动作回到标准 404（不把
	// generateContent/countTokens 误当成本族）。
	if endpoint == contract.EndpointEmbeddings && isGeminiWildcardPath(r.URL.Path) {
		if _, ok := geminiEmbeddingModel(r.URL.Path); !ok {
			http.NotFound(w, r)
			return
		}
	}

	model, err := validateMultimodalEntry(endpoint, r.URL.Path, r.Header.Get("Content-Type"), body)
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

	// /pg/* 会话路径：补分组 + 计费前模型准入（与 chat 路径同一 helper，
	// 保证两条入口对「模型不可用」得到相同 403 与相同的零计费副作用）。
	if status, code, message, ok := applyPlaygroundSession(r.Context(), h.groups, principal, rc, model); !ok {
		writeRelayError(w, status, requestID, code, message)
		return
	}

	// 鉴权/授权一致性（T1 复审 F2）：多模态端点必须复用 chat 路径的两道
	// 派发前检查，否则它们会成为「绕过令牌模型白名单 / 调用已停用模型」的通道。
	// 顺序与 chat 一致（先 BuildRelayContext 的 pin 403，再白名单/停用检查），
	// 避免同一请求在两处得到不同语义的 403。
	if model != "" {
		_, hasPin := relay.ParseChannelPin(r)
		if !hasPin && !tokenModelAllowed(principal.ModelLimits, model) {
			writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
				"this token is not allowed to access the requested model")
			return
		}
		// R-84：管理员停用（status=2）的模型不得被命中，必须在派发（含
		// PreConsume）之前 404；无定价行按默认价兜底、视为启用。
		if h.modelStatus != nil {
			status, found, statusErr := h.modelStatus.ModelPricingStatus(r.Context(), model)
			if statusErr != nil {
				writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
					"failed to check model availability")
				return
			}
			if found && status == modelPricingStatusDisabled {
				writeRelayError(w, http.StatusNotFound, requestID, "model_not_found",
					"the requested model is not available")
				return
			}
		}
	}

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
func validateMultimodalEntry(endpoint contract.Endpoint, path string, contentType string, body []byte) (string, error) {
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
	case contract.EndpointEmbeddings:
		if model, ok := geminiEmbeddingModel(path); ok {
			if strings.HasPrefix(path, "/v1/engines/") {
				return validateGeminiEnginesJSON(model, body)
			}
			return validateGeminiEmbeddingJSON(model, body)
		}
		return validateEmbeddingsJSON(body)
	case contract.EndpointRerank:
		return validateRerankJSON(body)
	default:
		// 未知端点（防御）：仍按 JSON 对象做最小校验。
		return validateLooseJSON(body)
	}
}

// isGeminiWildcardPath 判断路径是否落在 Gemini 语义通配路由的形态内。
func isGeminiWildcardPath(path string) bool {
	return strings.HasPrefix(path, "/v1/engines/") ||
		strings.HasPrefix(path, "/v1/models/") ||
		strings.HasPrefix(path, "/v1beta/models/")
}

// geminiEmbeddingModel 从 Gemini 语义路径提取模型名。
//
// 支持形态（relay-04 §1.1）：
//   - /v1/engines/{model}/embeddings
//   - /v1/models/{model}:embedContent 或 :batchEmbedContents
//   - /v1beta/models/{model}:embedContent 或 :batchEmbedContents
func geminiEmbeddingModel(path string) (string, bool) {
	// N6：路径形态的唯一实现位于 multimodal 包，避免入口与 dispatcher
	// 两处各写一份后漂移。
	return multimodal.GeminiEmbeddingModel(path)
}

// validateGeminiEnginesJSON 校验 `/v1/engines/{model}/embeddings` 的
// OpenAI 风格 body：input 必填（string 或 string[]），批量/单条字节与
// dimensions 上界与主嵌入入口一致（N3 残余：此路由此前完全绕过字段校验）。
func validateGeminiEnginesJSON(model string, body []byte) (string, error) {
	var payload struct {
		Input      json.RawMessage    `json:"input"`
		Dimensions optionalJSONNumber `json:"dimensions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	input := bytes.TrimSpace(payload.Input)
	if len(input) == 0 || string(input) == "null" {
		return "", invalidMultimodalEntry("input is required")
	}
	switch input[0] {
	case '"':
		var text string
		if err := json.Unmarshal(input, &text); err != nil || text == "" {
			return "", invalidMultimodalEntry("input is required")
		}
		if len(text) > multimodal.MaxEmbeddingInputBytes {
			return "", invalidMultimodalEntry("input exceeds the 1 MiB limit")
		}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(input, &items); err != nil || len(items) == 0 {
			return "", invalidMultimodalEntry("input is required")
		}
		if len(items) > multimodal.MaxEmbeddingBatch {
			return "", invalidMultimodalEntry("input batch exceeds 2048 items")
		}
		found := false
		for _, item := range items {
			item = bytes.TrimSpace(item)
			if len(item) == 0 || item[0] != '"' {
				continue
			}
			var text string
			if json.Unmarshal(item, &text) != nil {
				continue
			}
			found = true
			if len(text) > multimodal.MaxEmbeddingInputBytes {
				return "", invalidMultimodalEntry("input item exceeds the 1 MiB limit")
			}
		}
		if !found {
			return "", invalidMultimodalEntry("input is required")
		}
	default:
		return "", invalidMultimodalEntry("input must be a string or an array of strings")
	}
	if payload.Dimensions.Set {
		value, ok := jsonNumberInt(payload.Dimensions)
		if !ok || value < 1 || value > multimodal.MaxEmbeddingDimensions {
			return "", invalidMultimodalEntry("dimensions must be an integer in [1, 8192]")
		}
	}
	return model, nil
}

// validateGeminiEmbeddingJSON 校验 Gemini 语义嵌入请求。
//
// N5：`requests[]` 批量数 ≤ MaxEmbeddingBatch、每段 text ≤
// MaxEmbeddingInputBytes；单请求形态校验 content.parts 的文本上界。
func validateGeminiEmbeddingJSON(model string, body []byte) (string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", invalidMultimodalEntry("request body must be a JSON object")
	}
	type geminiPart struct {
		Text string `json:"text"`
	}
	type geminiContent struct {
		Parts []geminiPart `json:"parts"`
	}
	var payload struct {
		Content  geminiContent `json:"content"`
		Requests []struct {
			Content geminiContent `json:"content"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	if len(payload.Requests) > multimodal.MaxEmbeddingBatch {
		return "", invalidMultimodalEntry("requests batch exceeds 2048 items")
	}
	checkParts := func(parts []geminiPart) error {
		for _, part := range parts {
			if len(part.Text) > multimodal.MaxEmbeddingInputBytes {
				return invalidMultimodalEntry("embedding text exceeds the 1 MiB limit")
			}
		}
		return nil
	}
	if err := checkParts(payload.Content.Parts); err != nil {
		return "", err
	}
	for _, request := range payload.Requests {
		if err := checkParts(request.Content.Parts); err != nil {
			return "", err
		}
	}
	return model, nil
}

// validateEmbeddingsJSON 实施 relay-04 §1.2/§9 的入口上界：
// input 必填、批量与单条字节上界、dimensions 上界均为**用户声明的乘数**，
// 越界在归一化层 400 拒绝（billing.md 的上界前置原则）。
//
// 非字符串数组元素被忽略（照搬基线）；但必须至少有一个字符串输入，否则
// 归一化层无法产出可计费用量。
func validateEmbeddingsJSON(body []byte) (string, error) {
	var payload struct {
		Model      *string            `json:"model"`
		Input      json.RawMessage    `json:"input"`
		Dimensions optionalJSONNumber `json:"dimensions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	input := bytes.TrimSpace(payload.Input)
	if len(input) == 0 || string(input) == "null" {
		return "", invalidMultimodalEntry("input is required")
	}
	switch input[0] {
	case '"':
		var text string
		if err := json.Unmarshal(input, &text); err != nil || text == "" {
			return "", invalidMultimodalEntry("input is required")
		}
		if len(text) > multimodal.MaxEmbeddingInputBytes {
			return "", invalidMultimodalEntry("input exceeds the 1 MiB limit")
		}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(input, &items); err != nil || len(items) == 0 {
			return "", invalidMultimodalEntry("input is required")
		}
		if len(items) > multimodal.MaxEmbeddingBatch {
			return "", invalidMultimodalEntry("input batch exceeds 2048 items")
		}
		found := false
		for _, item := range items {
			trimmed := bytes.TrimSpace(item)
			if len(trimmed) == 0 || trimmed[0] != '"' {
				continue // 非字符串元素忽略（基线行为）
			}
			var text string
			if err := json.Unmarshal(trimmed, &text); err != nil {
				continue
			}
			found = true
			if len(text) > multimodal.MaxEmbeddingInputBytes {
				return "", invalidMultimodalEntry("input item exceeds the 1 MiB limit")
			}
		}
		if !found {
			return "", invalidMultimodalEntry("input is required")
		}
	default:
		return "", invalidMultimodalEntry("input must be a string or an array of strings")
	}
	if payload.Dimensions.Set {
		value, ok := jsonNumberInt(payload.Dimensions)
		if !ok || value < 1 || value > multimodal.MaxEmbeddingDimensions {
			return "", invalidMultimodalEntry("dimensions must be an integer in [1, 8192]")
		}
	}
	if payload.Model != nil {
		return *payload.Model, nil
	}
	return "", nil
}

// validateRerankJSON 实施 relay-04 §2.2/§9：query/documents 必填，文档批量、
// 单文档字节、top_n/max_chunk_per_doc/overlap_tokens 均带上界（甲类修正）。
func validateRerankJSON(body []byte) (string, error) {
	var payload struct {
		Model          *string            `json:"model"`
		Query          *string            `json:"query"`
		Documents      []json.RawMessage  `json:"documents"`
		TopN           optionalJSONNumber `json:"top_n"`
		MaxChunkPerDoc optionalJSONNumber `json:"max_chunk_per_doc"`
		OverlapTokens  optionalJSONNumber `json:"overlap_tokens"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", invalidMultimodalEntry("invalid JSON request body")
	}
	if payload.Query == nil || strings.TrimSpace(*payload.Query) == "" {
		return "", invalidMultimodalEntry("query is required")
	}
	if len(payload.Documents) == 0 {
		return "", invalidMultimodalEntry("documents is required")
	}
	if len(payload.Documents) > multimodal.MaxRerankDocuments {
		return "", invalidMultimodalEntry("documents exceeds 1000 items")
	}
	for _, document := range payload.Documents {
		trimmed := bytes.TrimSpace(document)
		if len(trimmed) > multimodal.MaxRerankDocBytes {
			return "", invalidMultimodalEntry("a document exceeds the 1 MiB limit")
		}
	}
	for _, item := range []struct {
		name   string
		number optionalJSONNumber
		max    int64
		label  string
	}{
		{"top_n", payload.TopN, multimodal.MaxRerankTopN, "top_n must be an integer in [1, 1000]"},
		{"max_chunk_per_doc", payload.MaxChunkPerDoc, multimodal.MaxRerankChunkPerDoc, "max_chunk_per_doc must be an integer in [1, 512]"},
		{"overlap_tokens", payload.OverlapTokens, multimodal.MaxRerankOverlapTokens, "overlap_tokens must be an integer in [0, 4096]"},
	} {
		if !item.number.Set {
			continue
		}
		value, ok := jsonNumberInt(item.number)
		minimum := int64(1)
		if item.name == "overlap_tokens" {
			minimum = 0
		}
		if !ok || value < minimum || value > item.max {
			return "", invalidMultimodalEntry(item.label)
		}
	}
	if payload.Model != nil {
		return *payload.Model, nil
	}
	return "", nil
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
		Model          *string            `json:"model"`
		Input          *string            `json:"input"`
		Speed          optionalJSONNumber `json:"speed"`
		ResponseFormat *string            `json:"response_format"`
		MaxNewTokens   optionalJSONNumber `json:"max_new_tokens"`
		Metadata       json.RawMessage    `json:"metadata"`
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
	// F6：response_format 是决定 TTS 时长解析口径的枚举；未知值会让响应
	// 落到 body 大小兜底，必须在入口拒绝。
	if payload.ResponseFormat != nil && *payload.ResponseFormat != "" {
		switch strings.ToLower(strings.TrimSpace(*payload.ResponseFormat)) {
		case "mp3", "opus", "aac", "flac", "wav", "pcm":
		default:
			return "", invalidMultimodalEntry("response_format must be one of mp3, opus, aac, flac, wav, pcm")
		}
	}
	// max_new_tokens 是 vllm-omini 透传字段，属用户可控的计费乘数（§9）。
	if payload.MaxNewTokens.Set {
		value, ok := jsonNumberInt(payload.MaxNewTokens)
		if !ok || value < 1 || value > multimodal.MaxSpeechNewTokens {
			return "", invalidMultimodalEntry("max_new_tokens must be an integer in [1, 4096]")
		}
	}
	// metadata 是透传载荷，按字节上界防御（§9：MaxAudioMetadataBytes）。
	if len(bytes.TrimSpace(payload.Metadata)) > multimodal.MaxAudioMetadataBytes {
		return "", invalidMultimodalEntry("metadata exceeds the 64 KiB limit")
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

// prefixCaptureWriter 统计总字节数并只保留前 limit 字节（魔数校验用）。
// 之后的内容直接丢弃：入口校验不保留上传体（T4 会从复原后的 body 重新解析）。
type prefixCaptureWriter struct {
	prefix bytes.Buffer
	limit  int
	total  int64
}

func (w *prefixCaptureWriter) Write(p []byte) (int, error) {
	w.total += int64(len(p))
	if remaining := w.limit - w.prefix.Len(); remaining > 0 {
		take := len(p)
		if take > remaining {
			take = remaining
		}
		_, _ = w.prefix.Write(p[:take])
	}
	return len(p), nil
}

// drainMultipartFileCapture 与 drainMultipartFile 同语义，但保留文件头
// 用于「扩展名 ↔ MIME ↔ 魔数」一致性校验（security-04 §3.2 / ASVS 5.2.2）。
func drainMultipartFileCapture(part *multipart.Part, limit int64) (int64, []byte, error) {
	capture := &prefixCaptureWriter{limit: 512}
	if _, err := io.Copy(capture, io.LimitReader(part, limit+1)); err != nil {
		return 0, nil, invalidMultimodalEntry("invalid multipart form")
	}
	return capture.total, capture.prefix.Bytes(), nil
}

// imageFileFamily 返回图片文件所属格式族（png/jpeg/webp）；未知返回空串。
func imageFileFamily(prefix []byte) string {
	switch {
	case bytes.HasPrefix(prefix, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}):
		return "png"
	case len(prefix) >= 3 && prefix[0] == 0xFF && prefix[1] == 0xD8 && prefix[2] == 0xFF:
		return "jpeg"
	case len(prefix) >= 12 && string(prefix[0:4]) == "RIFF" && string(prefix[8:12]) == "WEBP":
		return "webp"
	default:
		return ""
	}
}

// imageExtensionFamily 把文件扩展名映射到图片格式族。
func imageExtensionFamily(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".png":
		return "png"
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".webp":
		return "webp"
	default:
		return ""
	}
}

// imageMIMEFamily 把声明的 Content-Type 映射到图片格式族。
// 空值 / application/octet-stream 视为「未声明」，由扩展名+魔数负责。
func imageMIMEFamily(contentType string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/webp":
		return "webp"
	case "", "application/octet-stream":
		return ""
	default:
		return "unsupported"
	}
}

// validateImageUpload 校验输入图片的扩展名/MIME/魔数三者一致。
func validateImageUpload(filename, contentType string, prefix []byte) error {
	extFamily := imageExtensionFamily(filename)
	if extFamily == "" {
		return invalidMultimodalEntry("image must be a png, jpeg or webp file")
	}
	if mimeFamily := imageMIMEFamily(contentType); mimeFamily == "unsupported" || (mimeFamily != "" && mimeFamily != extFamily) {
		return invalidMultimodalEntry("image content type does not match its extension")
	}
	if magicFamily := imageFileFamily(prefix); magicFamily == "" || magicFamily != extFamily {
		return invalidMultimodalEntry("image content does not match its declared type")
	}
	return nil
}

// audioFileFamily 由魔数判定音频容器族（relay-04 §3.2 的格式清单）。
func audioFileFamily(prefix []byte) string {
	switch {
	case bytes.HasPrefix(prefix, []byte("ID3")):
		return "mp3"
	case len(prefix) >= 2 && prefix[0] == 0xFF && prefix[1]&0xE0 == 0xE0 &&
		len(prefix) >= 3 && (prefix[1]>>1)&0x03 == 0:
		// ADTS AAC：layer 位必须为 00；MP3 的 layer 位非 0（否则会把
		// 最常见的 0xFB 帧头误判成 AAC）。
		return "aac"
	case len(prefix) >= 2 && prefix[0] == 0xFF && prefix[1]&0xE0 == 0xE0:
		return "mp3"
	case len(prefix) >= 12 && string(prefix[0:4]) == "RIFF" && string(prefix[8:12]) == "WAVE":
		return "wav"
	case bytes.HasPrefix(prefix, []byte("fLaC")):
		return "flac"
	case len(prefix) >= 8 && string(prefix[4:8]) == "ftyp":
		return "m4a"
	case bytes.HasPrefix(prefix, []byte("OggS")):
		return "ogg"
	case len(prefix) >= 12 && string(prefix[0:4]) == "FORM" &&
		(string(prefix[8:12]) == "AIFF" || string(prefix[8:12]) == "AIFC"):
		return "aiff"
	default:
		return ""
	}
}

func audioExtensionFamily(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp3":
		return "mp3"
	case ".wav":
		return "wav"
	case ".flac":
		return "flac"
	case ".m4a", ".mp4":
		return "m4a"
	case ".ogg", ".oga", ".opus":
		return "ogg"
	case ".aiff", ".aif", ".aifc":
		return "aiff"
	case ".aac":
		return "aac"
	default:
		return ""
	}
}

func audioMIMEFamily(contentType string) string {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "audio/flac", "audio/x-flac":
		return "flac"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return "m4a"
	case "audio/ogg", "audio/opus", "application/ogg":
		return "ogg"
	case "audio/aiff", "audio/x-aiff":
		return "aiff"
	case "audio/aac":
		return "aac"
	case "", "application/octet-stream":
		return ""
	default:
		return "unsupported"
	}
}

// validateAudioUpload 校验音频上传的扩展名/MIME/魔数三者一致。
// 三者不一致 → 400（security-04 §3.2：按魔数拒绝伪造类型）。
func validateAudioUpload(filename, contentType string, prefix []byte) error {
	extFamily := audioExtensionFamily(filename)
	if extFamily == "" {
		return invalidMultimodalEntry("audio file type is not supported")
	}
	if mimeFamily := audioMIMEFamily(contentType); mimeFamily == "unsupported" || (mimeFamily != "" && mimeFamily != extFamily) {
		return invalidMultimodalEntry("audio content type does not match its extension")
	}
	if magicFamily := audioFileFamily(prefix); magicFamily == "" || magicFamily != extFamily {
		return invalidMultimodalEntry("audio content does not match its declared type")
	}
	return nil
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
		hasInputImage                bool
		inputImageBytes              int64
	)
	// seen 用于拒绝重复的计费相关标量字段（复审 F9）。
	//
	// 本函数是顺序读取、后者覆盖前者（last-wins），而 Go 标准库的
	// ParseMultipartForm/FormValue 取**第一个**值。两者对重复字段的解释不同，
	// 攻击者可以先给越界值再给合法值绕过入口校验、让下游消费越界值（反之亦然）。
	// 重复本身不是合法表单，统一 400 fail-closed。
	seen := make(map[string]bool, 6)
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
			if seen[name] {
				_ = part.Close()
				return "", invalidMultimodalEntry("duplicate multipart field")
			}
			seen[name] = true
			value, fieldErr := readMultipartField(part)
			_ = part.Close()
			if fieldErr != nil {
				return "", fieldErr
			}
			parameters = value
			hasParameters = true
			continue
		}
		// image / mask 等文件 part：校验大小、扩展名/MIME/魔数（T1 复审 F4/F5）
		// 与输入图片总字节上界。内容本身留给 Task 4 从复原后的 body 重新解析。
		if part.FileName() != "" || name == "image" || name == "mask" {
			size, prefix, err := drainMultipartFileCapture(part, MaxImageEditInputBytes)
			_ = part.Close()
			if err != nil {
				return "", err
			}
			if size > MaxImageEditInputBytes {
				return "", invalidMultimodalEntry("image exceeds the 25 MiB limit")
			}
			if size > 0 {
				if err := validateImageUpload(part.FileName(), part.Header.Get("Content-Type"), prefix); err != nil {
					return "", err
				}
			}
			if name == "image" {
				hasInputImage = true
				inputImageBytes += size
				if inputImageBytes > MaxImageEditInputBytes {
					return "", invalidMultimodalEntry("input images exceed the 25 MiB limit")
				}
			}
			continue
		}
		value, err := readMultipartField(part)
		_ = part.Close()
		if err != nil {
			return "", err
		}
		switch name {
		case "model", "prompt", "size", "quality", "n":
			if seen[name] {
				return "", invalidMultimodalEntry("duplicate multipart field")
			}
			seen[name] = true
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
	// F4：编辑端点必须携带输入图片。缺少 image（或空文件）无法执行编辑，
	// 也意味着计费乘数（n）没有对应的输入——必须在派发前 400。
	if !hasInputImage || inputImageBytes == 0 {
		return "", invalidMultimodalEntry("image is required")
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
		modelSeen bool
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
			size, prefix, drainErr := drainMultipartFileCapture(part, MaxAudioFileBytes)
			filename := part.FileName()
			partType := part.Header.Get("Content-Type")
			_ = part.Close()
			if drainErr != nil {
				return "", drainErr
			}
			if size > MaxAudioFileBytes {
				return "", invalidMultimodalEntry("audio file exceeds the 25 MiB limit")
			}
			if size > 0 {
				// F5：扩展名 ↔ 声明 MIME ↔ 魔数三者一致（security-04 §3.2）。
				if err := validateAudioUpload(filename, partType, prefix); err != nil {
					return "", err
				}
				// F12 / relay-04 §8.2.3：转写/翻译的预扣依赖上传文件头时长。
				// ogg/aiff/aac 等无法在不解码全文件时测得时长，必须显式
				// 失败（可诊断），不得静默按 0 秒计费。
				if !multimodal.SupportsAudioDuration(audioExtensionFamily(filename)) {
					return "", invalidMultimodalEntry("audio format cannot be measured for billing; use mp3, wav, flac or m4a")
				}
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
			// 与图像字段同理：本层 last-wins、标准库 FormValue first-wins，
			// 重复 model 会让校验与会话/定价消费到不同模型名，fail-closed。
			if modelSeen {
				return "", invalidMultimodalEntry("duplicate multipart field")
			}
			modelSeen = true
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
