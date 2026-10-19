package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/billing/expr"
	"newtg/gateway/internal/billing/pricing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/relay/crossproto"
	"newtg/gateway/internal/relay/ir"
	"newtg/gateway/internal/relay/multimodal"
	"newtg/gateway/internal/relay/stream"
	"newtg/gateway/internal/relay/usage"
	"newtg/gateway/internal/store"
)

// 本文件是 Task 8 phase 8B 的 /v1 中继端点：
//
//	POST /v1/chat/completions   非流式走 relay.Route；流式走 ExecuteAttempts + SSE 桥接
//	GET  /v1/models             启用渠道 supported_models 去重并集
//
// 边界（relay-01 §2 阶段序）：鉴权在中间件（relay_auth.go）已完成；本文件负责
// 请求体上限与 IR 归一化（阶段 4）、RelayContext 组装（阶段 5）、路由/执行
// （阶段 7–13）、响应/错误映射（阶段 13–14）。计费只经由 contract.BillingSession
// 接缝（M2 是 billingseam.Stub，GC-3：不实现定价）。
//
// 安全不变量（GC-6）：所有错误只含固定文案 + requestId；上游 URL、渠道凭据、
// 适配器原始错误文本绝不出现在响应体或日志。上游请求体的 32MiB 上限与
// relay-01 §11.1 的超时值是冻结常量。

const (
	// maxRelayBodyBytes 是 /v1/chat/completions 的请求体上限（32MiB）。
	// 超限返回 413（OpenAI 信封），而不是把超大 body 读进内存。
	maxRelayBodyBytes = 32 << 20

	// relayMaxRetries 是 M2 的重试次数（不含首试）。GC-5 / C-9/D11 冻结默认值 3；
	// relay.ExecuteAttempts 会再钳制到 [1,5]。
	relayMaxRetries = 3

	// relay-01 §11.1 冻结的上游客户端超时。这些值同时存在于 relay 包的未导出
	// 常量里；端点层再写一份是任务书要求（请求级注入客户端时的唯一配置点）。
	// 整体读取（Client.Timeout）保持 0：流式/长响应不能被总时长掐断，
	// 响应头等待由 Transport.ResponseHeaderTimeout 负责。
	relayResponseHeaderTimeout = 1800 * time.Second
	relayIdleConnTimeout       = 90 * time.Second
	relayDialTimeout           = 30 * time.Second
	relayTLSHandshakeTimeout   = 10 * time.Second
	relayMaxIdleConns          = 500
	relayMaxIdleConnsPerHost   = 100
	relayExpectContinueTimeout = 1 * time.Second

	// channelTestTimeout 是渠道连通性测试的单次上限（modules-06 F-06-06 的 30s）。
	channelTestTimeout = 30 * time.Second

	// channelTestMaxErrorBodyBytes 限制连通性测试读取响应体的上限；只为让
	// adapter.ClassifyError 有界读取，绝不回显（GC-6）。
	channelTestMaxErrorBodyBytes = 64 << 10

	// maxSoftStreamErrors 是软流错误的记录上限（relay-03 §4.3/§10.1：上限 20 条）。
	// 第 21 条软错误按硬错误处理：继续吞错会让一个持续损坏的上游把连接
	// 拖成永不结束的「成功」流。
	maxSoftStreamErrors = 20

	// streamInactivityTimeout 是块间不活动看门狗的默认值（relay-03 §5.4/§10.1
	// 冻结的 300s）。上游在等待期或传输期静默超过该值即中断流并按 timeout 结算。
	// 测试通过 RelayHandlerOptions.StreamInactivityTimeout 注入短值。
	streamInactivityTimeout = 300 * time.Second

	// streamPingInterval 是 SSE 保活注释帧 `: PING\n\n` 的间隔（relay-03 §3.3：
	// 契约 §10.1 冻结的唯一值 10s）。等待期与传输期共用同一个 ticker。
	streamPingInterval = 10 * time.Second
)

var (
	// errRelayBodyTooLarge 表示请求体超过 maxRelayBodyBytes（映射 413）。
	errRelayBodyTooLarge = errors.New("httpapi: relay request body too large")
	// errRelayCredentialMissing / errRelayCredentialInvalid 是渠道凭据解封失败的
	// 统一出口：文本不含密文、base_url 或底层错误（GC-6/GC-8）。
	errRelayCredentialMissing = errors.New("httpapi: channel credential is not configured")
	errRelayCredentialInvalid = errors.New("httpapi: channel credential cannot be opened")
)

// RelayChannelStore 是中继端点需要的渠道存储面；生产实现是 *store.ChannelRepo，
// 测试注入内存 fake（与 phase 8A 的 ChannelStore 分离：这里只需要读路径）。
type RelayChannelStore interface {
	Candidates(ctx context.Context, model, group string) ([]contract.ChannelCandidate, error)
	Get(ctx context.Context, id int64) (*store.Channel, error)
	EnabledModels(ctx context.Context) ([]string, error)
}

var _ RelayChannelStore = (*store.ChannelRepo)(nil)

// RelayModelStatusStore 是派发前的模型定价状态只读查询面（R-84 /
// modules-06 TC-F-06-13-01）。生产实现是 *store.SQLPricingRepo。
//
// 返回 (status, found, err)：found=false 表示 model_pricing 无该模型的行，
// 按默认价兜底、视为启用；status=2 表示管理员已停用该模型，用户请求必须
// 在进入上游之前返回 404。
type RelayModelStatusStore interface {
	ModelPricingStatus(ctx context.Context, model string) (status int16, found bool, err error)
}

var _ RelayModelStatusStore = (*store.SQLPricingRepo)(nil)

// modelPricingStatusDisabled 是 model_pricing.status 的停用值（modules-06
// §3.7 状态机：1 启用 / 2 停用；迁移的 CHECK 约束只允许这两个值）。
const modelPricingStatusDisabled int16 = 2

// RelayHandlerOptions 是 platform 装配与测试的注入点。
type RelayHandlerOptions struct {
	// Channels 是候选/渠道/模型列表的数据源（必填）。
	Channels RelayChannelStore
	// Tokens 是令牌解析接缝（auth.TokenLookup；生产为 *store.TokenRepo）。
	Tokens auth.TokenLookup
	// Access 回源 users.role/status（生产为 *store.TokenRepo）。
	Access UserAccessStore
	// Sealer 解开 channels.credentials 的 AES-GCM 密文（GC-8，必填）。
	Sealer *SecretSealer
	// Billing 为每个请求构造计费会话；nil 时使用 billingseam.NewStub（M2 桩，GC-3）。
	// 每请求新建是契约要求（真实 Session 与 billingseam.Stub 都是单会话实例）。
	//
	// 【T9 签名】入参是该请求的 RelayContext：真实引擎的 NewSession 需要它把
	// RequestID/UserID/TokenID/UsingGroup/PriceSnapshot 绑定到会话（契约 §6.1）。
	Billing func(*contract.RelayContext) contract.BillingSession
	// ModelStatus 是派发前的模型停用检查（R-84）；nil 时跳过（测试/降级）。
	// 生产由 platform 注入 store.NewPricingRepo(DB)。
	ModelStatus RelayModelStatusStore
	// Client 是上游 HTTP 客户端；nil 时按 relay-01 §11.1 超时构造。
	// 测试用 httptest.Server.Client() 注入，以复用其 TLS/连接配置。
	Client *http.Client
	// Dispatch 是渠道类型 → 适配器分派；nil 时使用 adapters.Dispatch。
	// 测试注入 fake StreamAdapter 以覆盖 soft/hard 流错误等上游难以复现的路径。
	Dispatch func(contract.ChannelType) (contract.Adapter, error)
	// TrustedProxies 是 TRUSTED_PROXIES 原文（逗号分隔 IP/CIDR）。为空时
	// RelayAuth 只信 RemoteAddr，忽略 X-Forwarded-For（M2 策略见 relay_auth.go）。
	TrustedProxies string
	// StreamInactivityTimeout 覆盖流式看门狗的默认 300s；<=0 使用默认值。
	// 仅测试注入短值，生产不得调小（relay-03 §10.1 冻结值）。
	StreamInactivityTimeout time.Duration
	// Multimodal 是 M4 多模态端点族的分派器（Task 4 注入）。
	// nil 时这些端点返回 501（仅 Task 1 骨架态）；T4 装配真实实现后不得再为 nil。
	Multimodal MultimodalDispatcher
	// Config 是跨协议转发开关的读取面（configcenter.Center 实现 ConfigReader）。
	// nil 时按默认 false 处理（GC-8 / R-03）：功能必须显式启用，绝无隐式开启。
	Config ConfigReader
}

// RelayHandler 持有 /v1 端点依赖。字段不导出，装配面只有 RelayHandlerOptions。
type RelayHandler struct {
	channels         RelayChannelStore
	tokens           auth.TokenLookup
	access           UserAccessStore
	sealer           *SecretSealer
	billing          func(*contract.RelayContext) contract.BillingSession
	modelStatus      RelayModelStatusStore
	client           *http.Client
	dispatch         func(contract.ChannelType) (contract.Adapter, error)
	trustedProxies   []netip.Prefix
	streamInactivity time.Duration
	multimodal       MultimodalDispatcher
	config           ConfigReader
}

// NewRelayHandler 构造中继端点 handler。
//
// 渠道存储与密钥封装是硬依赖：静默降级（空候选/空密钥）会把「配置错误」
// 伪装成「用户请求失败」，因此直接 panic（与 phase 8A NewChannelHandler 同口径）。
func NewRelayHandler(opts RelayHandlerOptions) *RelayHandler {
	if opts.Channels == nil {
		panic("httpapi: NewRelayHandler requires a channel store")
	}
	if opts.Sealer == nil {
		panic("httpapi: NewRelayHandler requires a credential sealer (GC-8)")
	}
	client := opts.Client
	if client == nil {
		client = newRelayHTTPClient()
	}
	billing := opts.Billing
	if billing == nil {
		billing = func(*contract.RelayContext) contract.BillingSession { return billingseam.NewStub() }
	}
	dispatch := opts.Dispatch
	if dispatch == nil {
		dispatch = adapters.Dispatch
	}
	inactivity := opts.StreamInactivityTimeout
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	h := &RelayHandler{
		channels:         opts.Channels,
		tokens:           opts.Tokens,
		access:           opts.Access,
		sealer:           opts.Sealer,
		billing:          billing,
		modelStatus:      opts.ModelStatus,
		client:           client,
		dispatch:         dispatch,
		trustedProxies:   parseTrustedProxies(opts.TrustedProxies),
		streamInactivity: inactivity,
		multimodal:       opts.Multimodal,
		config:           opts.Config,
	}
	// M4 T4：生产装配默认注入真实多模态分派器（T1 复审 F2/F4/F5 与 R-02 的
	// 落点）。显式传入的 Multimodal 优先（测试可替换）；nil 不再表示 501——
	// 多模态端点与 chat 共用同一套候选/渠道/计费/客户端接缝。
	if h.multimodal == nil {
		h.multimodal = multimodal.NewDispatcher(multimodal.Options{
			Candidates: func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
				return h.channels.Candidates(ctx, rc.OriginModel, rc.UsingGroup)
			},
			ResolveChannel: func(ctx context.Context, rc *contract.RelayContext, channelID int) (relay.ChannelAssignment, error) {
				return h.resolveChannel(ctx, rc, channelID)
			},
			Dispatch:   h.dispatch,
			Billing:    h.billing,
			Client:     h.client,
			MaxRetries: relayMaxRetries,
			WriteError: writeRelayMappedError,
		})
	}
	return h
}

// Register 把 /v1 路由挂到 mux；鉴权中间件只包裹这些端点（管理面不经过它）。
//
// 多模态端点族（M4 T1）复用同一个 RelayAuth 中间件：统一入口要求所有 /v1
// 端点先鉴权再归一化（relay-04 §0.2）。GET /v1/models 只在此处注册一次
// （R-09），多模态注册函数不得重复注册它。
func (h *RelayHandler) Register(mux *http.ServeMux) {
	authMiddleware := RelayAuth(h.tokens, h.access, h.trustedProxies)
	mux.Handle("POST /v1/chat/completions", authMiddleware(http.HandlerFunc(h.handleChatCompletions)))
	mux.Handle("GET /v1/models", authMiddleware(http.HandlerFunc(h.handleModels)))

	// M4 T1 的多模态路由骨架：dispatcher 由 Task 4 注入；同时把 TokenLookup
	// 交给多模态 handler 以复用 BuildRelayContext 的分组回源路径。
	RegisterMultimodalRoutes(mux, authMiddleware, NewMultimodalHandler(MultimodalHandlerOptions{
		Tokens:      h.tokens,
		ModelStatus: h.modelStatus,
		Dispatcher:  h.multimodal,
	}))
}

// Handler 返回挂好路由的独立 mux（测试与嵌入式装配用）。
func (h *RelayHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// ---------- POST /v1/chat/completions ----------

func (h *RelayHandler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()

	// 跨协议转发（M4 T5 / GC-8）：协议头只在这里解析一次，随后贯穿选路
	// （targetProtocol）与响应回写。判定顺序是冻结口径：
	//   1. 开关关闭且携带头 → 403（无论头值是否合法）：关闭状态不得静默
	//      降级为 OpenAI（Review Focus 5），也不透露「只是拼错了」；
	//   2. 开关开启但头值非法 → 400；
	//   3. 未携带头 → targetProtocol 为空，后续选路与 M2 完全一致。
	// 鉴权在中间件已完成，因此未认证请求永远先得到 401，不会被本分支抢答。
	targetProtocol, hasProtocolHeader, protocolErr := crossproto.TargetFromHeader(r.Header.Get(crossproto.HeaderProtocol))
	if hasProtocolHeader && !readConfigBool(h.config, crossproto.ConfigKeyEnabled, false) {
		writeRelayError(w, http.StatusForbidden, requestID, "cross_protocol_forward_disabled",
			"cross-protocol forwarding is not enabled")
		return
	}
	if protocolErr != nil {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_protocol",
			"unsupported target protocol (expected openai, anthropic, or gemini)")
		return
	}

	body, err := readRelayBody(r)
	if err != nil {
		if errors.Is(err, errRelayBodyTooLarge) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large",
				"request body exceeds the 32 MiB limit")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "failed to read request body")
		return
	}
	// 复原 body：relay.BuildRelayContext 的 peekStreaming 需要读请求体判定 stream
	// （它会把已读前缀拼回）。之后归一化层仍以 IR 的 Stream 为权威值（§7.2）。
	r.Body = io.NopCloser(bytes.NewReader(body))

	parsed, err := ir.ParseOpenAIRequest(body)
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid OpenAI request body")
		return
	}
	// finding 14（OpenAI stream bridge 丢弃 choice 身份）：stream=true 时
	// 下游 SSE 桥接只编码 index=0 的增量，n>1 会静默丢失其余候选。
	// 与其返回一个语义不完整的流，不如在入口 400 拒绝并明确说明。
	if parsed.Stream && requestChoiceCount(body) > 1 {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request",
			"stream=true with n>1 is not supported: the streaming bridge cannot preserve choice identity")
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
	// 归一化层是 Request/OriginModel/Streaming 的唯一写入方（relay-01 §7.2）。
	// 必须在 BuildRelayContext 之后写：它会先用 peekStreaming 填一个尽力值。
	relay.WithRequest(rc, parsed, ir.FormatOpenAIChat, r.URL.Path)

	// finding 1（relay-01 §3.5/§5.4-A）：pin 必须在 BuildRelayContext **之后**
	// 解析——后者已经完成了「非管理员使用 pin → 403」的统一校验（R-P2a）。
	// 提前解析会让会话路径的 pin 绕过那道校验。
	pinID, hasPin := relay.ParseChannelPin(r)

	// finding 6（relay-01 §3.4）：令牌模型白名单在路由阶段执行；pin 渠道
	// 优先级高于白名单（§3.4 注：pin 命中时跳过白名单）。非管理员 pin
	// 已在 BuildRelayContext 返回 403，因此这里 hasPin 等价于管理员 pin。
	if !hasPin && !tokenModelAllowed(principal.ModelLimits, parsed.Model) {
		writeRelayError(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}

	// R-84 / modules-06 TC-F-06-13-01：管理员停用（status=2）的定价模型不得
	// 被用户请求命中，必须在派发（含 PreConsume）之前 404。无定价行的模型
	// 按默认价兜底，仍视为启用（found=false 放行）。
	//
	// 为什么不交给 pricing.DBSource：它对停用行刻意回退默认价（保留管理员
	// 事后重新启用的能力），不会拒绝请求；「停用即不可调用」是路由层语义。
	if h.modelStatus != nil {
		status, found, statusErr := h.modelStatus.ModelPricingStatus(r.Context(), parsed.Model)
		if statusErr != nil {
			// GC-6：存储错误可能含 DSN/SQL，只回固定文案。
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

	// finding 13：预扣估算必须进入 RelayContext，否则 PreConsume 只能看到
	// 零值（MaxTokens=nil），402 判定与 Terminate 预算释放都成了空转。
	// PromptTokens 暂为 0：本地 token 估算器（relay-03 §7）尚未落地，
	// 调用方不得把 0 误读成「prompt 免费」——M3 估算器接入后在此填充。
	relay.WithQuotaEstimate(rc, &contract.QuotaEstimate{
		Model:     parsed.Model,
		Group:     rc.UsingGroup,
		MaxTokens: parsed.MaxTokens,
	})

	deps := h.routeDeps(rc, pinID, hasPin, targetProtocol)
	if parsed.Stream {
		h.serveStream(w, r, rc, deps)
		return
	}
	// finding 11（relay-03 §2.3）：非流式请求不得向上游发送 stream_options /
	// include_usage——OpenAI 会以未知参数拒绝，且该选项只对流式有意义。
	// 归一化层保留原始 Raw 供透传，故在非流式出口显式剥离。
	if cleaned := stripStreamOptions(rc.Request); cleaned != rc.Request {
		rc.Request = cleaned
	}
	h.serveJSON(w, r, rc, deps)
}

// requestChoiceCount 读取 OpenAI 请求体的 n（缺省/非法为 1）。
// 只用于 finding 14 的「stream + n>1」拒绝，不进入 IR（契约未建模 n）。
//
// 用 json.Number/float64 而不是 *int：`2.0`、`2e0` 是合法 JSON 数字且语义
// 等于 2；解码到 int 会失败并静默回落成 1，从而绕过 stream+n>1 的入口拒绝
// （二轮复审 finding 4）。超出 float64 范围的合法数字（如 1e999）按 +Inf
// 处理——语义上必然 >1，同样拒绝。
func requestChoiceCount(body []byte) float64 {
	var payload struct {
		N *json.Number `json:"n"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.N == nil {
		return 1
	}
	n, err := payload.N.Float64()
	if err != nil {
		return math.Inf(1)
	}
	if n < 1 {
		return 1
	}
	return n
}

// stripStreamOptions 返回剥离顶层 stream_options 的请求副本。
//
// 非流式路径专用（relay-03 §2.3「不支持该选项时不流式时清空」）。
// 入参不被修改：Raw 是透传基底，可能仍被其它路径引用。
func stripStreamOptions(req *contract.Request) *contract.Request {
	if req == nil || len(req.Raw) == 0 {
		return req
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(req.Raw, &obj); err != nil || obj == nil {
		return req
	}
	if _, ok := obj["stream_options"]; !ok {
		return req
	}
	delete(obj, "stream_options")
	raw, err := json.Marshal(obj)
	if err != nil {
		return req
	}
	out := *req
	out.Stream = false
	out.Raw = raw
	return &out
}

// tokenModelAllowed 判定请求模型是否满足令牌模型白名单（relay-01 §3.4）。
// 空白名单 = 不限；大小写精确匹配（模型名是协议标识符，不做归一化）。
func tokenModelAllowed(modelLimits []string, model string) bool {
	if len(modelLimits) == 0 {
		return true
	}
	for _, allowed := range modelLimits {
		if allowed == model {
			return true
		}
	}
	return false
}

// readRelayBody 读取并限幅请求体。多读 1 字节即可区分「恰好 32MiB」与「超限」。
func readRelayBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxRelayBodyBytes {
		return nil, errRelayBodyTooLarge
	}
	return body, nil
}

// routeDeps 组装 relay.RouteDeps（控制器裁决 R-P4：依赖全部显式注入）。
//
// pinID/hasPin 来自 relay.ParseChannelPin（handleChatCompletions 在
// BuildRelayContext 成功后解析，finding 1）。pin 只写进候选约束，不写进
// RelayContext（R-P2a）；若 pin 目标不在候选集内，返回 relay.ErrChannelNotFound，
// 由 ExecuteAttempts 在 PreConsume **之前**快失败（HTTP 404）。
func (h *RelayHandler) routeDeps(rc *contract.RelayContext, pinID int, hasPin bool, targetProtocol crossproto.Protocol) relay.RouteDeps {
	return relay.RouteDeps{
		Candidates: func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			// 候选按**客户端请求模型**与令牌分组过滤（relay-01 §5；store 侧
			// 只返回 status=1 且 supported_models 命中该模型的行）。
			candidates, err := h.channels.Candidates(ctx, rc.OriginModel, rc.UsingGroup)
			if err != nil {
				return nil, err
			}
			if !hasPin {
				return filterCandidatesByProtocol(ctx, h.channels, candidates, targetProtocol)
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
				// pin 不在候选集：被过滤（禁用/分组/模型）或根本不存在。
				// 绝不在这里按 pin 另取渠道，否则会绕过 §5.4 D 步的过滤兜底。
				return nil, fmt.Errorf("%w: channel %d", relay.ErrChannelNotFound, pinID)
			}
			return filterCandidatesByProtocol(ctx, h.channels, candidates, targetProtocol)
		},
		ResolveChannel: func(ctx context.Context, channelID int) (relay.ChannelAssignment, error) {
			return h.resolveChannel(ctx, rc, channelID)
		},
		Dispatch:   h.dispatch,
		Billing:    h.billing(rc),
		Client:     h.client,
		MaxRetries: relayMaxRetries,
	}
}

// filterCandidatesByProtocol 按目标上游协议过滤候选（跨协议转发）。
//
// targetProtocol 为空 = 未携带协议头：原样返回，保证 M2 选路行为逐字节不变
// （只有候选切片本身，未排序、未改写）。
//
// 过滤必须在 PreConsume 之前完成（本函数位于 Candidates 回调内，执行层在
// 预扣前调用）：目标协议没有可用渠道时直接显式失败，不产生任何计费副作用。
//
// 为什么逐个 Get 渠道类型而不是让 store 查询时过滤：Candidates 的存储面
// （RelayChannelStore）是冻结的，且协议族映射的唯一真源在 adapters 分派表；
// 在路由层做这一层过滤可以避免把「渠道类型 → 协议」的知识泄漏进 store。
// 候选规模是单个模型 + 分组，通常个位数，额外点查询有界。
func filterCandidatesByProtocol(
	ctx context.Context,
	channels RelayChannelStore,
	candidates []contract.ChannelCandidate,
	targetProtocol crossproto.Protocol,
) ([]contract.ChannelCandidate, error) {
	if targetProtocol == "" {
		return candidates, nil
	}
	filtered := make([]contract.ChannelCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		ch, err := channels.Get(ctx, int64(candidate.ChannelID))
		if err != nil {
			return nil, err
		}
		if crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), targetProtocol) {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 {
		// 显式失败而不是回退到任意协议：客户端声明了目标协议，静默换协议
		// 会让它按错误的响应语义解析（Review Focus 5 / GC-8）。
		return nil, crossproto.ErrUpstreamProtocolUnsupported
	}
	return filtered, nil
}

// resolveChannel 把 store.Channel 投影为执行层需要的整组渠道字段（relay-01 §7.4）。
//
// GroupRatio 在 M2 固定 1.0：分组倍率属于计费定价（M3），中继只持有与透传
// （GC-3）；填 1.0 而不是 0，避免下游把零值误读成「免费」。
func (h *RelayHandler) resolveChannel(ctx context.Context, rc *contract.RelayContext, channelID int) (relay.ChannelAssignment, error) {
	ch, err := h.channels.Get(ctx, int64(channelID))
	if err != nil {
		return relay.ChannelAssignment{}, err
	}
	creds, err := h.openChannelCredentials(ch)
	if err != nil {
		return relay.ChannelAssignment{}, err
	}
	apiBase := ""
	if ch.BaseURL != nil {
		apiBase = *ch.BaseURL
	}
	return relay.ChannelAssignment{
		ChannelID:     channelID,
		ChannelType:   contract.ChannelType(ch.Type),
		APIBase:       apiBase,
		Credential:    creds.APIKey,
		UpstreamModel: modelMappingTarget(ch.ModelMapping, rc.OriginModel),
		GroupRatio:    1.0,
		CreatedTime:   ch.CreatedAt,
	}, nil
}

// openChannelCredentials 解封渠道凭据。失败统一返回不含细节的哨兵（GC-6/GC-8）。
func (h *RelayHandler) openChannelCredentials(ch *store.Channel) (ChannelCredentials, error) {
	if ch == nil || len(ch.Credentials) == 0 {
		return ChannelCredentials{}, errRelayCredentialMissing
	}
	plaintext, err := h.sealer.Open(ch.Credentials)
	if err != nil {
		return ChannelCredentials{}, errRelayCredentialInvalid
	}
	var creds ChannelCredentials
	if err := json.Unmarshal(plaintext, &creds); err != nil || creds.APIKey == "" {
		return ChannelCredentials{}, errRelayCredentialInvalid
	}
	return creds, nil
}

// modelMappingTarget 返回 model_mapping 中「客户端模型 → 上游模型」的映射值；
// 未配置/不是字符串时返回空串，适配器会回落到客户端模型名（relay-02 §8.1）。
func modelMappingTarget(raw []byte, model string) string {
	if len(raw) == 0 || model == "" {
		return ""
	}
	var mapping map[string]string
	if err := json.Unmarshal(raw, &mapping); err != nil {
		return ""
	}
	return mapping[model]
}

// ---------- 非流式 ----------

func (h *RelayHandler) serveJSON(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	resp, err := relay.Route(r.Context(), rc, deps)
	if err != nil {
		writeRelayMappedError(w, rc.RequestID, err)
		return
	}

	// 响应方向跨协议回写（GC-2）：上游是 Anthropic/Gemini 原生协议时，
	// resp.Raw 是上游原始 JSON，绝不能直接透传给 OpenAI 兼容客户端。
	// 目标协议取**实际命中渠道**的类型（而不是协议头）：重试可能换渠道，
	// 头只约束协议族，最终响应格式由最后一个成功渠道决定；二者在正常路径
	// 上一致（选路阶段已按头过滤），这里以执行结果为权威。
	upstreamProtocol, known := crossproto.ProtocolOfChannelType(rc.ChannelType)
	if !known {
		// 未实现的协议族不会走到这里（分派会先失败）；防御性按 OpenAI 透传。
		upstreamProtocol = crossproto.ProtocolOpenAI
	}
	resp, err = crossproto.Forward(r.Context(), rc, string(upstreamProtocol), resp)
	if err != nil {
		// 计费已在 Route 内结算；这里只是编码失败（异常上游形状），
		// 按上游错误映射，且不回显底层文本（GC-6）。
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"failed to encode the upstream response")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	if resp != nil && len(resp.Raw) > 0 {
		// 上游原始响应体逐字节透传（relay-03 的非流式路径不重编码）。
		_, _ = w.Write(resp.Raw)
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ---------- 流式 ----------

// includeUsageRequested 判定是否向上游索要并向下游合成 include_usage（relay-03 §2.3）：
//   - stream_options.include_usage 缺省 → true（OpenAI Chat 的默认语义）；
//   - 显式 true/false → 尊重客户端选择（客户端显式 false 优先于默认值）。
//
// 只看归一化请求的 Raw：ir.ParseOpenAIRequest 已把它保存为完整原始体。
func includeUsageRequested(req *contract.Request) bool {
	if req == nil || len(req.Raw) == 0 {
		return true
	}
	var payload struct {
		StreamOptions *struct {
			IncludeUsage *bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(req.Raw, &payload); err != nil {
		return true
	}
	if payload.StreamOptions == nil || payload.StreamOptions.IncludeUsage == nil {
		return true
	}
	return *payload.StreamOptions.IncludeUsage
}

// serveStream 执行流式中继（relay-01 §2 阶段 7–14；relay-03 §3–§5）。
//
// 与 Route 的分工（R-P7）：流式路径先 ExecuteAttempts 拿原始 2xx 响应，再交给
// 适配器的 StreamEvents 事件流；ExecuteAttempts 在失败出口已 Terminate（GC-7），
// 因此本函数只在**拿到 2xx 之后**负责终结计费会话，且恰好一次。
func (h *RelayHandler) serveStream(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	// relay-03 §2.3：include_usage 默认 true，但客户端显式 false 优先。
	// 仅当需要时才注入 stream_options.include_usage=true（Raw 的其它字段保留）；
	// 客户端显式 false 时原样透传，不注入、也不合成末块 usage。
	includeUsage := includeUsageRequested(rc.Request)
	injected := usage.InjectIncludeUsage(rc.Request, includeUsage)
	if injected == nil {
		writeRelayError(w, http.StatusBadRequest, rc.RequestID, "invalid_request", "invalid request body")
		return
	}
	// R-P14：流式路径强制 stream=true（即使上游请求体里残留 stream=false）。
	injected.Stream = true
	rc.Request = injected
	rc.Streaming = true

	// start 覆盖连上游在内的完整请求时长：LatencyMS / FirstTokenMS 都相对
	// 客户端请求开始计（与 relay.Route 的 start 口径一致）。
	start := time.Now()
	result, err := relay.ExecuteAttempts(r.Context(), rc, deps)
	if err != nil {
		writeRelayMappedError(w, rc.RequestID, err)
		return
	}
	defer func() { _ = result.Response.Body.Close() }()

	streamAdapter, ok := result.Adapter.(contract.StreamAdapter)
	if !ok {
		// 预扣已成功（ExecuteAttempts 返回 2xx），必须释放预算再拒绝。
		// R-85：这是失败出口，走 Refund（Terminate(零事实) 会让按次模型扣费）。
		if deps.Billing != nil {
			_ = deps.Billing.Refund()
		}
		writeRelayError(w, http.StatusBadGateway, rc.RequestID, "stream_unsupported",
			"upstream adapter does not support streaming")
		return
	}

	// 必须在 WriteHeader 之前设置响应头（relay-03 §3.1）。
	stream.SetSSEHeaders(w.Header())
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	sse := newRelaySSEWriter(w)
	w.WriteHeader(http.StatusOK)
	sse.flush()

	h.bridgeStream(r, rc, result.Response.Body, result.Response.Header, streamAdapter, deps.Billing, sse, start, includeUsage)
}

// bridgeStream 消费适配器的 StreamEvent 序列并写入客户端。
//
// 语义（relay-03 §3–§9、relay-01 §7.6）：
//   - EventRaw：原始 SSE 帧逐字节转发（补足帧结束空行，帧内容不改写）；
//   - EventDelta/ToolDelta/Reasoning：重编码为 OpenAI chat.completion.chunk；
//   - EventUsage：非零合并累计（不擦除已有维度）；
//   - 若整个流都没有 EventUsage/finish usage，回落到上游 `x-model-response-usage`
//     响应头（R-05 优先级 3，二轮复审 finding 5）；
//   - EventFinish：[DONE] 之前写出 include_usage 末块；整个响应对客户端只写一次
//     data: [DONE]；
//   - EventError：软错误记录后继续；硬错误只终止流，绝不伪造 [DONE]（GC-6）。
//
// 健壮性（relay-03 §5.4/§10.1，M2 final review finding 4）：
//   - 看门狗：块间不活动超过 streamInactivity（默认 300s）→ EndReason=timeout，
//     关闭上游 body 以解除阻塞读；
//   - 心跳：每 10s 写 `: PING\n\n` 注释帧（等待期与传输期共用），写失败按客户端
//     断开处理并关闭上游；
//   - panic 恢复：EndReason=EndPanic，进程不崩；
//   - 计费（GC-7/R-85）：2xx 之后会话已开启；正常出口 Terminate(final fact)，
//     任何异常出口（断开/上游错误/超时/panic）一律 Refund；terminated 守卫
//     保证恰好一次。
//
// 异常结束（客户端断开/上游错误/超时/panic）不写收尾 usage 块与 [DONE]：
// relay-03 §5.2/§10.3 要求由连接终止表达异常，伪造正常结束标记会误导客户端。
func (h *RelayHandler) bridgeStream(
	r *http.Request,
	rc *contract.RelayContext,
	upstream io.ReadCloser,
	upstreamHeader http.Header,
	adapter contract.StreamAdapter,
	billing contract.BillingSession,
	sse *relaySSEWriter,
	start time.Time,
	includeUsage bool,
) {
	chunkID := "chatcmpl-newtg"
	if rc.RequestID != "" {
		chunkID = "chatcmpl-" + rc.RequestID
	}
	created := start.Unix()
	model := rc.OriginModel

	var (
		accumulated  contract.Usage
		sawUsage     bool
		received     int
		sent         int
		contentSent  int // 仅内容帧（raw/delta）：OutcomePartial 的判据（契约 §7.2）
		softErrors   int
		firstTokenMS *int
		finished     bool
		upstreamFail bool
		clientGone   bool
	)
	// 看门狗与 ping 协程会并发写这两个标志；事件循环只读。
	var timedOut atomic.Bool
	var pingFailed atomic.Bool

	// 上游 body 可能被看门狗、ping 失败与 serveStream 的 defer 多处关闭：
	// 统一经 sync.Once 收敛，避免并发 Close 的竞争。
	var closeUpstreamOnce sync.Once
	closeUpstream := func() { closeUpstreamOnce.Do(func() { _ = upstream.Close() }) }

	terminated := false
	finalize := func(terminate func() error) {
		if terminated || billing == nil {
			return
		}
		terminated = true
		_ = terminate()
	}
	terminate := func(fact contract.UsageFact) {
		finalize(func() error { return billing.Terminate(fact) })
	}
	// R-85：失败/取消/panic 是整笔不收费语义。真实引擎的 Refund 会全额退回
	// hold 与增量，并把 usage_logs 收敛为 refunded；Terminate(零事实) 会让
	// 按次/常量表达式模型对失败流照常扣费并记 success。
	refund := func() {
		finalize(func() error { return billing.Refund() })
	}
	// GC-7 兜底：任何未走显式终结的出口（含 panic）也必须释放预留。
	// 失败语义优先（R-85）：兜底一律退款，不结算。
	defer refund()

	// —— 看门狗（块间不活动超时）——
	inactivity := h.streamInactivity
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	watchdog := time.AfterFunc(inactivity, func() {
		timedOut.Store(true)
		// 关闭 body 让阻塞中的上游 Read 立即返回错误，事件循环才能退出。
		closeUpstream()
	})
	defer watchdog.Stop()
	touch := func() {
		// timedOut 已置位时不再续期：避免把已判超时的流又「救活」。
		if !timedOut.Load() {
			watchdog.Reset(inactivity)
		}
	}

	// —— SSE 心跳（等待首字节与流式传输期都发送）——
	// 写端与事件循环共享 relaySSEWriter 的互斥锁，帧不会交错。
	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		ticker := time.NewTicker(streamPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if !sse.ping() {
					// 写失败 = 客户端已断开；置位并关闭上游解除阻塞读。
					pingFailed.Store(true)
					closeUpstream()
					return
				}
			}
		}
	}()
	var stopPingOnce sync.Once
	stopPing := func() {
		stopPingOnce.Do(func() {
			close(pingStop)
			<-pingDone
		})
	}
	defer stopPing()

	// —— panic 恢复 ——
	// 注册在兜底 refund 之后（LIFO：本 defer 先执行），显式写入 EndPanic
	// 并退款；兜底 refund 随后因 terminated 守卫成为 no-op。
	defer func() {
		if rec := recover(); rec != nil {
			stopPing()
			watchdog.Stop()
			relay.WithStream(rc, &contract.StreamStatus{
				Phase:     contract.PhaseTerminated,
				EndReason: contract.EndPanic,
				Outcome:   relayStreamOutcome(contentSent),
				RecvCount: received,
				SendCount: sent,
			}, sent, received, firstTokenMS)
			// R-85：panic 是失败出口，整笔退款；不构造/上报用量事实
			// （abnormal 的「部分用量」不再参与计费）。
			refund()
		}
	}()

	writeData := func(payload []byte) bool {
		if !sse.data(payload) {
			clientGone = true
			return false
		}
		sent++
		return true
	}
	recordFirstToken := func() {
		if firstTokenMS != nil {
			return
		}
		ms := int(time.Since(start).Milliseconds())
		firstTokenMS = &ms
	}

	// StreamEvents 接受 io.ReadWriter（契约 §3.1 v1.1 决议 D3：SSE 与 WS 共用）。
	// SSE 是单向流，写端用 io.Discard 占位。
	rw := struct {
		io.Reader
		io.Writer
	}{Reader: upstream, Writer: io.Discard}

	adapter.StreamEvents(rc, rw)(func(ev contract.StreamEvent) bool {
		// 异常优先判定：客户端断开 / ping 写失败 / 看门狗超时。
		// 即使适配器把取消包装成 EventError，也按对应异常处理，而不是上游错误。
		if r.Context().Err() != nil {
			clientGone = true
			return false
		}
		if pingFailed.Load() {
			clientGone = true
			return false
		}
		if timedOut.Load() {
			upstreamFail = true
			return false
		}
		touch()
		received++

		switch ev.Kind {
		case contract.EventRaw:
			if len(ev.Raw) == 0 {
				return true
			}
			if !sse.raw(ev.Raw) {
				clientGone = true
				return false
			}
			sent++
			contentSent++
			return true

		case contract.EventDelta:
			recordFirstToken()
			payload, err := marshalRelayDeltaChunk(chunkID, created, model, ev)
			if err != nil {
				upstreamFail = true
				return false
			}
			if !writeData(payload) {
				return false
			}
			contentSent++
			return true

		case contract.EventUsage:
			if ev.Usage != nil {
				mergeUsage(&accumulated, *ev.Usage)
				sawUsage = true
			}
			return true

		case contract.EventFinish:
			// finish_reason 是客户端可见语义（stop/length/tool_calls），不能只记布尔：
			// 从原始帧解析后写一个 delta 为空、finish_reason 非空的 chunk。
			// ev.Raw 可能是 [DONE] 哨兵帧（适配器在未见 finish_reason 时补发），
			// 因此必须解析而不是盲目转发。
			finished = true
			reason, finishUsage := parseRelayFinishEvent(ev.Raw)
			if finishUsage != nil {
				mergeUsage(&accumulated, *finishUsage)
				sawUsage = true
			}
			if reason != "" {
				payload, err := marshalRelayFinishChunk(chunkID, created, model, reason)
				if err != nil {
					upstreamFail = true
					return false
				}
				if !writeData(payload) {
					return false
				}
			}
			return true

		case contract.EventError:
			// relay-03 §4.3/§10.1：EventError 是**软错误**（坏 JSON 块/上游 error
			// 事件），记录后继续；上限 20 条，第 21 条按硬错误处理。
			// relay-03 §5.2/§10.3：异常结束不得向客户端注入错误对象——因此硬错误
			// （Raw 为空：读取失败/事件超限/空 reader）只终止流，不写错误帧。
			if len(ev.Raw) > 0 && softErrors < maxSoftStreamErrors {
				softErrors++
				return true
			}
			upstreamFail = true
			return false

		case contract.EventPing:
			// 心跳不是客户端可见事件；保活由传输层/SSE 注释帧负责（relay-03 §3.3）。
			return true

		default:
			return true
		}
	})

	// 停止心跳后再写收尾帧，避免 [DONE] 之后仍出现 ping。
	stopPing()
	if pingFailed.Load() {
		clientGone = true
	}

	// 头部兜底（R-05 优先级 3，二轮复审 finding 5）：整个流从未出现
	// EventUsage/finish usage 时，上游可能只在 `x-model-response-usage` 头里
	// 报告用量。先尝试头部；只有头部也没有可用数字时才在下方标记 estimated，
	// 绝不能把「头部给了精确值」的情况降级成本地估算。
	//
	// 判定条件是**非零事实**而非「是否出现过 usage 事件」（re-review fix 1）：
	// 上游发 `usage: {}` 或全零 usage 帧时 sawUsage 已为 true，但累计用量没有
	// 任何数字——此时头部才是唯一的真实来源，不能被事件占位符挡住。反过来，
	// 只要累计出任何非零维度，就绝不拿头部覆盖（三源互斥，R-05）。
	if !sawUsage || !usageHasFact(accumulated) {
		extracted := usage.ExtractUsageMultimodal(&contract.Response{}, upstreamHeader, nil, rc.ChannelType)
		if extracted.Usage.Source == contract.UsageUpstreamExact {
			accumulated = extracted.Usage
			sawUsage = true
		}
	}

	// 收尾帧只在正常/EOF 路径写；异常结束（客户端断开、上游错误、看门狗超时、
	// panic）不写 usage 块与 [DONE]（relay-03 §5.2/§10.3）。
	abnormal := clientGone || upstreamFail || timedOut.Load()
	if !abnormal {
		// 仅当 include_usage 开启时合成末块 usage（客户端显式 false 时不伪造）。
		if includeUsage {
			if payload, err := marshalRelayUsageChunk(chunkID, created, model, accumulated); err == nil {
				if sse.data(payload) {
					sent++
				}
			}
		}
		// relay-03 §3.2：统一结束标记且整个响应只出现一次。
		if sse.done() {
			sent++
		}
	}

	endReason := contract.EndUpstreamDone
	outcome := contract.OutcomeSuccess
	switch {
	case clientGone:
		endReason = contract.EndClientAbort
		outcome = relayStreamOutcome(contentSent)
	case timedOut.Load():
		endReason = contract.EndStreamTimeout
		outcome = relayStreamOutcome(contentSent)
	case upstreamFail:
		endReason = contract.EndUpstreamError
		outcome = relayStreamOutcome(contentSent)
	case !finished:
		endReason = contract.EndUpstreamEOF
		outcome = relayStreamOutcome(contentSent)
	}
	relay.WithStream(rc, &contract.StreamStatus{
		Phase:     contract.PhaseTerminated,
		EndReason: endReason,
		Outcome:   outcome,
		RecvCount: received,
		SendCount: sent,
	}, sent, received, firstTokenMS)

	// R-85：异常结束（客户端断开、上游错误、看门狗超时、panic）整笔退款。
	// 不再为失败流构造/上报用量事实——契约的 Refund() 没有事实入参，
	// 而把零值/部分事实交给 Terminate 会让按次模型照常扣费并记 success。
	if abnormal {
		refund()
		return
	}

	fact := contract.UsageFact{
		Model:        model,
		Usage:        accumulated,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		FirstTokenMS: firstTokenMS,
		Stream:       true,
		Attempts:     rc.AttemptIndex + 1,
	}
	if !usageHasFact(accumulated) {
		// 上游没有给出任何非零用量（包括空 `usage: {}` / 全零帧，且无头部）：
		// 标记 estimated，绝不把零值谎报成 upstream_exact（零值 Source 的
		// iota 是 UsageUpstreamExact）。关系布尔按**渠道协议族**补齐：
		// OpenAI 的 prompt 已含缓存、completion 已含推理；Claude/Gemini 是
		// 独立维度，不能沿用 OpenAI 的 true（否则缓存段被重复加价）。
		fact.Usage = estimatedUsageFor(rc.ChannelType)
	}
	terminate(fact)
}

// estimatedUsageFor 为「无上游用量、需要本地估算」的事实补上**渠道协议族**
// 语义的关系布尔。
//
// 关系布尔是协议属性而非用量数字（relay-03 §6.2 row 1）：OpenAI 的
// prompt_tokens 已含 cached_tokens、completion_tokens 已含 reasoning_tokens；
// Claude/Gemini 的缓存与思考是独立维度。M4 接线后原生渠道也走本辅助函数，
// 不能再无条件按 OpenAI 置 true（那会让计费侧把缓存段重复加价）。
func estimatedUsageFor(kind contract.ChannelType) contract.Usage {
	return usage.ExtractUsageMultimodal(&contract.Response{}, nil, nil, kind).Usage
}

// relaySSEWriter 是 SSE 帧的唯一写出口。
//
// 为什么需要互斥：流式事件循环（主协程）与 10s ping ticker（独立协程）都会
// 写 http.ResponseWriter；net/http 不保证 ResponseWriter 并发写安全，帧必须
// 串行化（relay-03 §3.4 的「单写者」要求）。failed 置位后所有后续写直接失败，
// 避免客户端断开后反复写坏连接。
type relaySSEWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	failed  bool
}

func newRelaySSEWriter(w http.ResponseWriter) *relaySSEWriter {
	flusher, _ := w.(http.Flusher)
	return &relaySSEWriter{w: w, flusher: flusher}
}

func (s *relaySSEWriter) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// writeFrame 写一个完整帧并立即 Flush。
func (s *relaySSEWriter) writeFrame(frame []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// data 写 `data: <payload>\n\n`（relay-03 §3.2 OpenAI 帧形态）。
func (s *relaySSEWriter) data(payload []byte) bool {
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return s.writeFrame(frame)
}

// raw 逐字节转发原始 SSE 事件（R-P2）。适配器的 Raw 是「事件块原文」
// （行间 \n，无尾部空行），因此这里只补足帧结束空行，不改写内容。
func (s *relaySSEWriter) raw(raw []byte) bool {
	frame := append([]byte(nil), raw...)
	switch {
	case bytes.HasSuffix(frame, []byte("\n\n")):
	case bytes.HasSuffix(frame, []byte("\n")):
		frame = append(frame, '\n')
	default:
		frame = append(frame, '\n', '\n')
	}
	return s.writeFrame(frame)
}

// done 写唯一的统一终止标记（relay-03 §3.2）。
func (s *relaySSEWriter) done() bool {
	return s.writeFrame([]byte("data: [DONE]\n\n"))
}

// ping 写 SSE 注释帧 `: PING\n\n`（relay-03 §3.3，10s 间隔）。
func (s *relaySSEWriter) ping() bool {
	return s.writeFrame([]byte(": PING\n\n"))
}

// relayStreamOutcome 按**内容帧**数判定非正常结束的结果（契约 §7.2：
// OutcomePartial = 「有部分内容但未正常终结」）。合成帧（usage/[DONE]）
// 不算内容，否则空流 EOF 会被误报为 partial。
func relayStreamOutcome(contentSent int) contract.ResponseOutcome {
	if contentSent > 0 {
		return contract.OutcomePartial
	}
	return contract.OutcomeFailed
}

// ---------- GET /v1/models ----------

func (h *RelayHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	models, err := h.channels.EnabledModels(r.Context())
	if err != nil {
		// GC-6：存储错误可能含 SQL/DSN，只回固定文案。
		writeRelayError(w, http.StatusInternalServerError, requestID, "database_error",
			"failed to load model list")
		return
	}
	type modelObject struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	payload := struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}{Object: "list", Data: make([]modelObject, 0, len(models))}
	for _, m := range models {
		payload.Data = append(payload.Data, modelObject{ID: m, Object: "model", OwnedBy: "newtg"})
	}
	w.Header().Set("Content-Type", "application/json")
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(payload)
}

// ---------- 错误映射 ----------

// mapRelayContextError 把 BuildRelayContext 的哨兵映射为 /v1 状态码/信封。
func mapRelayContextError(err error) (int, string, string) {
	switch {
	case errors.Is(err, relay.ErrTokenInvalid), errors.Is(err, relay.ErrPrincipalMissing):
		// 401 不带 code：令牌类错误不区分原因（relay-01 §3.6）。
		return http.StatusUnauthorized, "", "invalid token"
	case errors.Is(err, relay.ErrPinForbidden):
		return http.StatusForbidden, "channel_pin_forbidden", "channel pin is not allowed for this user"
	case errors.Is(err, relay.ErrIdentityOutOfRange):
		return http.StatusInternalServerError, "internal_error", "internal error"
	case errors.Is(err, relay.ErrRequestMissing):
		return http.StatusBadRequest, "invalid_request", "invalid request"
	default:
		return http.StatusInternalServerError, "internal_error", "internal error"
	}
}

// writeRelayMappedError 映射错误并写 /v1 响应。
//
// ErrSettlementInProgress 额外带 Retry-After（R-45/R-86）：同一 requestID 的
// 结算被另一执行者持锁是**可短暂重试**的并发冲突，状态码 409 + 退避提示
// 比把它伪装成 502 上游故障更准确。值为 1 秒：结算锁 TTL 30s、锁内只有
// 一个有界 DB 事务（10s），1s 足以跨过一次正常冲突窗口。
func writeRelayMappedError(w http.ResponseWriter, requestID string, err error) {
	status, code, message := mapRelayError(err)
	if status == http.StatusConflict && errors.Is(err, billing.ErrSettlementInProgress) {
		w.Header().Set("Retry-After", "1")
	}
	writeRelayError(w, status, requestID, code, message)
}

// mapRelayError 把路由/执行错误映射为客户端状态码（relay-01 §9.3）。
//
// 顺序即语义：计费类错误（402/400/409）先于 FastFail 判定——预扣/结算错误
// 被 FastFailError/safeError 包装，但契约语义必须保留（R-86：定价/钳制类
// 不得落 502；R-45：结算冲突 409）。FastFail 与 AttemptsError 的区分决定
// 「一次失败」与「重试耗尽」的展示。所有消息只含固定文案/次数/状态码，
// 绝不含上游 URL/凭据。
func mapRelayError(err error) (int, string, string) {
	// 额度不足（钱包或令牌额度）：真实引擎返回 *billing.QuotaError（Unwrap 到
	// billing.ErrInsufficientQuota）；M2 桩返回 *billingseam.InsufficientQuotaError。
	// 两者都在此统一为 402 Payment Required（契约 §4.4；N7 的令牌/钱包文案统一）。
	var quota *billing.QuotaError
	var stubQuota *billingseam.InsufficientQuotaError
	if errors.As(err, &quota) || errors.As(err, &stubQuota) ||
		errors.Is(err, billing.ErrInsufficientQuota) || errors.Is(err, billingseam.ErrInsufficientQuota) {
		return http.StatusPaymentRequired, "insufficient_quota", "insufficient quota"
	}

	// 定价/配置/表达式失败：400 model_price_error（relay-01 §2 阶段 6；N6）。
	if isModelPriceError(err) {
		return http.StatusBadRequest, "model_price_error", "pricing configuration rejected the request"
	}

	// 请求已在别处结案（结算/退款/worker 兜底）：fail-closed 的 409，不重试。
	if errors.Is(err, billing.ErrRequestClosed) {
		return http.StatusConflict, "request_closed", "the request has already been settled"
	}
	// 并发结算冲突：409 + Retry-After（R-45/R-86），不重试。
	if errors.Is(err, billing.ErrSettlementInProgress) {
		return http.StatusConflict, "settlement_in_progress", "settlement for this request is in progress"
	}
	// 重复预扣超出预算（C-10/D12）：请求级 400，不重试。
	if errors.Is(err, billing.ErrBudgetExceeded) {
		return http.StatusBadRequest, "invalid_request", "reservation exceeds the estimated budget"
	}

	var fastFail *relay.FastFailError
	if errors.As(err, &fastFail) {
		switch {
		case errors.Is(err, relay.ErrTokenInvalid):
			return http.StatusUnauthorized, "", "invalid token"
		case errors.Is(err, relay.ErrPinForbidden):
			return http.StatusForbidden, "channel_pin_forbidden", "channel pin is not allowed for this user"
		case errors.Is(err, relay.ErrNoAvailableChannel):
			// relay-01 §2 阶段 3：503 get_channel_failed（SkipRetry）。
			return http.StatusServiceUnavailable, "model_not_found", "no available channel for the requested model"
		case errors.Is(err, relay.ErrChannelNotFound):
			return http.StatusNotFound, "channel_not_found", "the pinned channel is not available"
		case errors.Is(err, crossproto.ErrUpstreamProtocolUnsupported):
			// 跨协议转发：目标协议没有匹配渠道 = 上游能力不支持，非重试类
			// 错误（mapRelayError 走的 FastFail 分支已保证不重试）。
			return http.StatusBadRequest, "endpoint_not_supported",
				"no available channel supports the requested upstream protocol"
		}
		var upstream *relay.UpstreamStatusError
		if errors.As(err, &upstream) {
			return relayUpstreamStatus(upstream)
		}
		if errors.Is(err, adapters.ErrEndpointUnsupported) {
			return http.StatusBadRequest, "endpoint_not_supported", "the channel does not support this endpoint"
		}
		// 适配器分派/构造失败等：不暴露底层文本（可能含 base_url 解析细节）。
		return http.StatusBadGateway, "upstream_error", "upstream request failed"
	}

	var attempts *relay.AttemptsError
	if errors.As(err, &attempts) {
		// 任务书：AttemptsError → 502，并把尝试次数写进 message。
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("upstream request failed after %d attempt(s)", attempts.Attempts)
	}

	return http.StatusBadGateway, "upstream_error", "upstream request failed"
}

// isModelPriceError 判定「定价配置/表达式错误」族（N6：400 model_price_error）。
//
// 覆盖三类来源：
//   - 会话层：billing.ErrPriceError（预扣前已有 QuotaClamp）、billing.ErrInvalidConfig；
//   - 定价数学：pricing 的 spec/price/参数/表达式哨兵与 *pricing.ClampError；
//   - 表达式求值：*expr.EvalError（语法/未知变量/除零/溢出/未知函数）。
//
// 这些错误都表示「本次请求的定价配置不可用」，属于请求侧 400，不是上游
// 故障；落 502 会误导运维去查上游与网络。
func isModelPriceError(err error) bool {
	if errors.Is(err, billing.ErrPriceError) || errors.Is(err, billing.ErrInvalidConfig) {
		return true
	}
	var clamp *pricing.ClampError
	if errors.As(err, &clamp) {
		return true
	}
	var evalErr *expr.EvalError
	if errors.As(err, &evalErr) {
		return true
	}
	for _, sentinel := range []error{
		pricing.ErrInvalidSpec,
		pricing.ErrNilPrice,
		pricing.ErrMissingExpression,
		pricing.ErrUnexpectedExpression,
		pricing.ErrInvalidMode,
		pricing.ErrInvalidPrice,
		pricing.ErrInvalidParams,
		pricing.ErrInvalidGroupRatio,
		pricing.ErrNegativeExpression,
		pricing.ErrNoExpression,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// relayUpstreamStatus 实现 §9.3 的 ErrorClass → 客户端状态码映射。
func relayUpstreamStatus(upstream *relay.UpstreamStatusError) (int, string, string) {
	if upstream == nil {
		return http.StatusBadGateway, "upstream_error", "upstream request failed"
	}
	switch upstream.Class {
	case contract.ErrParamInvalid:
		switch upstream.StatusCode {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
			return upstream.StatusCode, "invalid_request",
				fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
		}
		return http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	case contract.ErrUpstreamAuth:
		return http.StatusUnauthorized, "upstream_auth_failed",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	case contract.ErrUpstreamRateLimit:
		return http.StatusTooManyRequests, "rate_limit_exceeded",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	case contract.ErrContentFilter:
		return http.StatusBadRequest, "content_filter", "request was rejected by the upstream content filter"
	case contract.ErrChannelConfig:
		return http.StatusServiceUnavailable, "channel_config_error", "channel is misconfigured"
	case contract.ErrUpstreamServer:
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	default:
		return http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("upstream returned status %d", upstream.StatusCode)
	}
}

// ---------- OpenAI chunk 编码 ----------

type openAIChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []openAIChunkChoice `json:"choices"`
	Usage   *openAIChunkUsage   `json:"usage,omitempty"`
}

type openAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

type openAIChunkDelta struct {
	Content          string                `json:"content,omitempty"`
	ReasoningContent string                `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIChunkToolCall `json:"tool_calls,omitempty"`
}

type openAIChunkToolCall struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function openAIChunkToolFunction `json:"function"`
}

type openAIChunkToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIChunkUsage struct {
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
	TotalTokens             int64                     `json:"total_tokens"`
	PromptTokensDetails     *openAIPromptTokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *openAICompletionDetails  `json:"completion_tokens_details,omitempty"`
}

type openAIPromptTokenDetails struct {
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type openAICompletionDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// marshalRelayDeltaChunk 把协议无关的 delta 事件编码为 OpenAI chat.completion.chunk。
// 文本走 content，推理走 reasoning_content（DeepSeek 兼容），工具增量走 tool_calls。
func marshalRelayDeltaChunk(id string, created int64, fallbackModel string, ev contract.StreamEvent) ([]byte, error) {
	model := ev.Model
	if model == "" {
		model = fallbackModel
	}
	delta := openAIChunkDelta{Content: ev.Delta, ReasoningContent: ev.Reasoning}
	if ev.ToolDelta != nil {
		delta.ToolCalls = []openAIChunkToolCall{{
			Index: ev.ToolDelta.Index,
			ID:    ev.ToolDelta.ID,
			Type:  "function",
			Function: openAIChunkToolFunction{
				Name:      ev.ToolDelta.Name,
				Arguments: ev.ToolDelta.ArgumentsFragment,
			},
		}}
	}
	return json.Marshal(openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChunkChoice{{Index: 0, Delta: delta, FinishReason: nil}},
	})
}

// marshalRelayUsageChunk 编码 include_usage 末块：choices 为空数组、只带 usage。
func marshalRelayUsageChunk(id string, created int64, model string, u contract.Usage) ([]byte, error) {
	usagePayload := &openAIChunkUsage{
		PromptTokens:     u.Prompt,
		CompletionTokens: u.Completion,
		TotalTokens:      u.Total,
	}
	if u.CacheRead != 0 || u.CacheWrite != 0 {
		usagePayload.PromptTokensDetails = &openAIPromptTokenDetails{
			CachedTokens:     u.CacheRead,
			CacheWriteTokens: u.CacheWrite,
		}
	}
	if u.Reasoning != 0 {
		usagePayload.CompletionTokensDetails = &openAICompletionDetails{ReasoningTokens: u.Reasoning}
	}
	return json.Marshal(openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChunkChoice{},
		Usage:   usagePayload,
	})
}

// marshalRelayFinishChunk 编码带 finish_reason 的终止 chunk（delta 为空）。
// OpenAI 客户端据此判断 stop / length / tool_calls；缺失会让上层误以为流被截断。
func marshalRelayFinishChunk(id string, created int64, model, reason string) ([]byte, error) {
	finish := reason
	return json.Marshal(openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openAIChunkChoice{{
			Index:        0,
			Delta:        openAIChunkDelta{},
			FinishReason: &finish,
		}},
	})
}

// parseRelayFinishEvent 从 EventFinish 的原始 SSE 帧中提取 finish_reason 与 usage。
//
// EventFinish 的 Raw 有两种来源（见 adapters/openai_stream.go）：
//   - 上游 finish_reason 块的原文（可能同一块还带 usage）；
//   - 上游只发 [DONE] 时，适配器补发的 EventFinish（Raw = "data: [DONE]"）。
//
// 后者必须识别并忽略，绝不能当 JSON 解析或原样转发。返回空 reason 表示
// 「没有可用的 finish_reason」；usage 仅在帧内确有 usage 对象时非 nil。
func parseRelayFinishEvent(raw []byte) (string, *contract.Usage) {
	payload := sseDataPayload(raw)
	if payload == "" || payload == "[DONE]" {
		return "", nil
	}
	var chunk struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage         json.RawMessage `json:"usage"`
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return "", nil
	}
	reason := ""
	if len(chunk.Choices) > 0 {
		reason = chunk.Choices[0].FinishReason
	}
	if reason == "" {
		// 原生协议（Anthropic/Gemini）的 EventFinish.Raw 是上游原始事件帧，
		// 结束原因不在 OpenAI 的 choices[].finish_reason 里。不在此处归一，
		// 原生流会「丢 finish_reason」，客户端据空值误判为被截断。
		reason = nativeFinishReason([]byte(payload))
	}
	var usage *contract.Usage
	usageRaw := bytes.TrimSpace(chunk.Usage)
	if len(usageRaw) > 0 && string(usageRaw) != "null" {
		// 原生 Anthropic message_delta 的 usage 形状是 input/output_tokens，
		// 用 OpenAI 解析器会得到零值（或把 total 误映射成 prompt）。按键
		// 形状选择解析器，与 T3 的「形状优先」口径一致。
		var parsed contract.Usage
		if bytes.Contains(usageRaw, []byte("input_tokens")) || bytes.Contains(usageRaw, []byte("output_tokens")) {
			parsed = ir.ParseAnthropicUsage(chunk.Usage)
		} else {
			parsed = ir.ParseOpenAIUsage(chunk.Usage)
		}
		usage = &parsed
	} else if meta := bytes.TrimSpace(chunk.UsageMetadata); len(meta) > 0 && string(meta) != "null" {
		parsed := ir.ParseGeminiUsage(chunk.UsageMetadata)
		usage = &parsed
	}
	return reason, usage
}

// nativeFinishReason 把 Anthropic / Gemini 的原始结束原因映射为 OpenAI 的
// finish_reason 语义（relay-03 的客户端契约是 OpenAI 形态）。
//
// 无法映射的值返回空串而不是臆造：调用方（bridgeStream）在 reason 为空时
// 不写 finish chunk——宁可让客户端按 EOF 处理，也不伪造 stop。
func nativeFinishReason(payload []byte) string {
	var anthropic struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(payload, &anthropic); err == nil && anthropic.Delta.StopReason != "" {
		switch anthropic.Delta.StopReason {
		case "end_turn", "stop_sequence", "pause_turn":
			return "stop"
		case "max_tokens":
			return "length"
		case "tool_use":
			return "tool_calls"
		case "refusal":
			return "content_filter"
		default:
			return ""
		}
	}
	var gemini struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(payload, &gemini); err == nil {
		for _, candidate := range gemini.Candidates {
			switch strings.ToUpper(strings.TrimSpace(candidate.FinishReason)) {
			case "":
				continue
			case "STOP":
				return "stop"
			case "MAX_TOKENS":
				return "length"
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
				return "content_filter"
			case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
				return "tool_calls"
			default:
				return ""
			}
		}
	}
	return ""
}

// sseDataPayload 合并一个 SSE 事件块内的多条 data: 行（\n 连接）。
func sseDataPayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// mergeUsage 把流式分块用量累计到 dst：**非零才覆盖，绝不擦除已有维度**
// （多块/多候选上游可能只在前几块给出非零值；relay-03 §6）。
func mergeUsage(dst *contract.Usage, src contract.Usage) {
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
	if src.Reserved != 0 {
		dst.Reserved = src.Reserved
	}
	dst.PromptIncludesCache = dst.PromptIncludesCache || src.PromptIncludesCache
	dst.ReasoningIncludedInCompletion = dst.ReasoningIncludedInCompletion || src.ReasoningIncludedInCompletion
	dst.PromptDetail = mergeUsageDetail(dst.PromptDetail, src.PromptDetail)
	dst.CompletionDetail = mergeUsageDetail(dst.CompletionDetail, src.CompletionDetail)
	if src.Raw != nil {
		dst.Raw = src.Raw
	}
	if usageHasFact(src) {
		dst.Source = src.Source
	}
}

func mergeUsageDetail(dst, src contract.UsageDetail) contract.UsageDetail {
	if src.Image != 0 {
		dst.Image = src.Image
	}
	if src.Audio != 0 {
		dst.Audio = src.Audio
	}
	if src.Text != 0 {
		dst.Text = src.Text
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheWrite != 0 {
		dst.CacheWrite = src.CacheWrite
	}
	if src.CacheReadImage != 0 {
		dst.CacheReadImage = src.CacheReadImage
	}
	return dst
}

func usageHasFact(u contract.Usage) bool {
	return u.Prompt != 0 || u.Completion != 0 || u.Total != 0 ||
		u.CacheRead != 0 || u.CacheWrite != 0 || u.Reasoning != 0 ||
		u.AudioSeconds != 0 || u.Reserved != 0 ||
		u.PromptDetail != (contract.UsageDetail{}) ||
		u.CompletionDetail != (contract.UsageDetail{})
}

// ---------- 上游 HTTP 客户端 ----------

// newRelayHTTPClient 返回 /v1 中继使用的上游客户端（relay-01 §11.1）。
//
// 在 handler 构造期创建一次并跨请求复用：Transport 持有连接池，
// §11.1 的 500 / 每主机 100 上限只有在长期复用时才有意义（与 relay 包
// defaultRouteClient 同口径）。测试通过 RelayHandlerOptions.Client 注入。
func newRelayHTTPClient() *http.Client {
	return &http.Client{
		Transport: newRelayTransport(),
		// relay-01 §6.3：3xx 视为上游异常，不跟随重定向（避免凭据被转发）。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// Client.Timeout 保持 0：整体读取默认禁用（§11.1）。
	}
}

func newRelayTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   relayDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          relayMaxIdleConns,
		MaxIdleConnsPerHost:   relayMaxIdleConnsPerHost,
		IdleConnTimeout:       relayIdleConnTimeout,
		TLSHandshakeTimeout:   relayTLSHandshakeTimeout,
		ExpectContinueTimeout: relayExpectContinueTimeout,
		ResponseHeaderTimeout: relayResponseHeaderTimeout,
	}
}

// ---------- 渠道连通性测试（真实实现） ----------

// channelTestUpstreamError 是连通性测试的上游非 2xx 摘要。Error() 只含状态码：
// base_url/凭据/上游 body 绝不进入错误文本（GC-6），handler 也整体丢弃该错误。
type channelTestUpstreamError struct {
	StatusCode int
	Class      contract.ErrorClass
}

func (e *channelTestUpstreamError) Error() string {
	// Class 是契约枚举（int），不含任何上游数据；带上是为排障提供归因，
	// 同时避免字段只写不读（handler 仍整体丢弃错误文本，GC-6）。
	return fmt.Sprintf("httpapi: channel test: upstream returned status %d (class %d)",
		e.StatusCode, int(e.Class))
}

// modules-06 F-06-06 冻结的连通性失败类别：只允许这五个字面量。
// 前端按该枚举做文案/引导，任何新词都会落到未定义分支，故不得扩展。
const (
	channelTestClassAuth    = "auth"    // 上游鉴权失败（401/403）
	channelTestClassTimeout = "timeout" // 请求超时（context deadline / net timeout）
	channelTestClassServer  = "server"  // 上游 5xx 或其它非 auth/param 状态；不可分类错误也归此档
	channelTestClassNetwork = "network" // 连接/DNS 失败
	channelTestClassParam   = "param"   // 请求参数非法（含内容审核）
)

// channelTestTransportError 是连通性测试的传输层失败摘要。
// Class 只由 channelTestTransportClass 写入（timeout/network），
// Error() 不含 URL/凭据（GC-6）；handler 最终仍整体丢弃该错误文本。
type channelTestTransportError struct{ Class string }

func (e *channelTestTransportError) Error() string {
	return "httpapi: channel test: upstream request failed (" + e.Class + ")"
}

// channelTestTransportClass 区分超时与一般网络失败：
//   - context deadline 或 net.Error.Timeout() → timeout；
//   - 其余（连接拒绝、DNS、TLS 握手失败、客户端取消）→ network。
func channelTestTransportClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return channelTestClassTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return channelTestClassTimeout
	}
	return channelTestClassNetwork
}

// channelTestErrorClass 把连通性测试错误归一为 modules-06 F-06-06 的
// **五值枚举**：auth | timeout | server | network | param。
//
// 映射口径：
//   - auth：上游 401/403（契约 ErrUpstreamAuth）；
//   - param：请求/内容校验类（ErrParamInvalid、ErrContentFilter）；
//   - timeout / network：由 channelTestTransportClass 在传输层写入；
//   - server：上游 5xx、限流、渠道配置等其余状态。
//
// 【不可分类 → server】选择保守档：server 不会把「上游不可用」误报成客户端
// 需要改参数的 param 或重新配置凭据的 auth，避免把排障方向指向错误一侧。
// 返回值本身不含状态码/URL/凭据，绝不回显底层错误文本（GC-6）。
func channelTestErrorClass(err error) string {
	var transport *channelTestTransportError
	if errors.As(err, &transport) {
		switch transport.Class {
		case channelTestClassTimeout, channelTestClassNetwork:
			return transport.Class
		default:
			return channelTestClassServer
		}
	}
	var upstream *channelTestUpstreamError
	if errors.As(err, &upstream) {
		switch upstream.Class {
		case contract.ErrUpstreamAuth:
			return channelTestClassAuth
		case contract.ErrParamInvalid, contract.ErrContentFilter:
			return channelTestClassParam
		default:
			// 5xx / 限流 / 渠道配置 / 未知状态：server。
			return channelTestClassServer
		}
	}
	return channelTestClassServer
}

// NewChannelConnectivityTester 构造 modules-06 F-06-06 的真实连通性测试：
// 解封凭据 → 适配器构造一个最小非流式请求（单条 user "ping"，max_tokens=1）
// → 30s 内发送 → 非 2xx 按 adapter.ClassifyError 归类并返回非敏感错误。
//
// 返回值是 phase 8A 的 ChannelTester 签名（latencyMS, error）。
func NewChannelConnectivityTester(sealer *SecretSealer, client *http.Client) ChannelTester {
	if client == nil {
		client = &http.Client{
			Timeout:       channelTestTimeout,
			Transport:     newRelayTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return func(ctx context.Context, ch *store.Channel, model string) (int, error) {
		if ch == nil {
			return 0, errors.New("httpapi: channel test: channel is nil")
		}
		if sealer == nil {
			return 0, errors.New("httpapi: channel test: credential sealer is not configured")
		}
		plaintext, err := sealer.Open(ch.Credentials)
		if err != nil {
			return 0, errRelayCredentialInvalid
		}
		var creds ChannelCredentials
		if err := json.Unmarshal(plaintext, &creds); err != nil || creds.APIKey == "" {
			return 0, errRelayCredentialInvalid
		}

		channelType := contract.ChannelType(ch.Type)
		adapter, err := adapters.Dispatch(channelType)
		if err != nil {
			// 适配器分派错误只含渠道类型；仍用固定文案（GC-6 的最后一道出口）。
			return 0, errors.New("httpapi: channel test: no adapter for this channel type")
		}
		apiBase := ""
		if ch.BaseURL != nil {
			apiBase = *ch.BaseURL
		}

		maxTokens := 1
		request := &contract.Request{
			Model:  model,
			Stream: false,
			Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Kind: contract.PartText, Text: "ping"}},
			}},
			MaxTokens: &maxTokens,
			RawFormat: ir.FormatOpenAIChat,
		}
		rc := &contract.RelayContext{
			ChannelType:   channelType,
			APIBase:       apiBase,
			Credential:    creds.APIKey,
			UpstreamModel: modelMappingTarget(ch.ModelMapping, model),
			CreatedTime:   ch.CreatedAt,
		}

		httpReq, err := adapter.BuildRequest(rc, request)
		if err != nil {
			// 构造错误可能回显 URL（GC-6）→ 固定文案。
			return 0, errors.New("httpapi: channel test: build upstream request failed")
		}

		testCtx, cancel := context.WithTimeout(ctx, channelTestTimeout)
		defer cancel()
		start := time.Now()
		resp, err := client.Do(httpReq.WithContext(testCtx))
		if err != nil {
			// *url.Error 的文本含完整 URL → 绝不透出（GC-6）；只保留
			// timeout/network 这一层泛化归因，供 F-06-06 的 error_class 使用。
			return 0, &channelTestTransportError{Class: channelTestTransportClass(err)}
		}
		latency := int(time.Since(start).Milliseconds())
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(io.LimitReader(resp.Body, channelTestMaxErrorBodyBytes))
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return 0, &channelTestUpstreamError{
				StatusCode: resp.StatusCode,
				Class:      adapter.ClassifyError(resp.StatusCode, body),
			}
		}
		return latency, nil
	}
}
