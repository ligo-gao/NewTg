package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/billingseam"
	"newtg/gateway/internal/relay/ir"
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
	// 每请求新建是契约要求（billingseam.Stub 是单会话实例）。
	Billing func() contract.BillingSession
	// Client 是上游 HTTP 客户端；nil 时按 relay-01 §11.1 超时构造。
	// 测试用 httptest.Server.Client() 注入，以复用其 TLS/连接配置。
	Client *http.Client
}

// RelayHandler 持有 /v1 端点依赖。字段不导出，装配面只有 RelayHandlerOptions。
type RelayHandler struct {
	channels RelayChannelStore
	tokens   auth.TokenLookup
	access   UserAccessStore
	sealer   *SecretSealer
	billing  func() contract.BillingSession
	client   *http.Client
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
		billing = func() contract.BillingSession { return billingseam.NewStub() }
	}
	return &RelayHandler{
		channels: opts.Channels,
		tokens:   opts.Tokens,
		access:   opts.Access,
		sealer:   opts.Sealer,
		billing:  billing,
		client:   client,
	}
}

// Register 把 /v1 路由挂到 mux；鉴权中间件只包裹这两个端点（管理面不经过它）。
func (h *RelayHandler) Register(mux *http.ServeMux) {
	authMiddleware := RelayAuth(h.tokens, h.access)
	mux.Handle("POST /v1/chat/completions", authMiddleware(http.HandlerFunc(h.handleChatCompletions)))
	mux.Handle("GET /v1/models", authMiddleware(http.HandlerFunc(h.handleModels)))
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

	deps := h.routeDeps(rc)
	if parsed.Stream {
		h.serveStream(w, r, rc, deps)
		return
	}
	h.serveJSON(w, r, rc, deps)
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
func (h *RelayHandler) routeDeps(rc *contract.RelayContext) relay.RouteDeps {
	return relay.RouteDeps{
		Candidates: func(ctx context.Context, rc *contract.RelayContext) ([]contract.ChannelCandidate, error) {
			// 候选按**客户端请求模型**与令牌分组过滤（relay-01 §5；store 侧
			// 只返回 status=1 且 supported_models 命中该模型的行）。
			return h.channels.Candidates(ctx, rc.OriginModel, rc.UsingGroup)
		},
		ResolveChannel: func(ctx context.Context, channelID int) (relay.ChannelAssignment, error) {
			return h.resolveChannel(ctx, rc, channelID)
		},
		Dispatch:   adapters.Dispatch,
		Billing:    h.billing(),
		Client:     h.client,
		MaxRetries: relayMaxRetries,
	}
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
		status, code, message := mapRelayError(err)
		writeRelayError(w, status, rc.RequestID, code, message)
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

// serveStream 执行流式中继（relay-01 §2 阶段 7–14；relay-03 §3–§5）。
//
// 与 Route 的分工（R-P7）：流式路径先 ExecuteAttempts 拿原始 2xx 响应，再交给
// 适配器的 StreamEvents 事件流；ExecuteAttempts 在失败出口已 Terminate（GC-7），
// 因此本函数只在**拿到 2xx 之后**负责终结计费会话，且恰好一次。
func (h *RelayHandler) serveStream(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	// R-P14 / R-02：流式路径强制 stream=true，并把 stream_options.include_usage=true
	// 合并进 Raw（保留其它未建模字段）。非流式不注入顶层 usage。
	injected := usage.InjectIncludeUsage(rc.Request, true)
	if injected == nil {
		writeRelayError(w, http.StatusBadRequest, rc.RequestID, "invalid_request", "invalid request body")
		return
	}
	injected.Stream = true
	rc.Request = injected
	rc.Streaming = true

	// start 覆盖连上游在内的完整请求时长：LatencyMS / FirstTokenMS 都相对
	// 客户端请求开始计（与 relay.Route 的 start 口径一致）。
	start := time.Now()
	result, err := relay.ExecuteAttempts(r.Context(), rc, deps)
	if err != nil {
		status, code, message := mapRelayError(err)
		writeRelayError(w, status, rc.RequestID, code, message)
		return
	}
	defer func() { _ = result.Response.Body.Close() }()

	streamAdapter, ok := result.Adapter.(contract.StreamAdapter)
	if !ok {
		// 预扣已成功（ExecuteAttempts 返回 2xx），必须释放预算再拒绝（GC-7）。
		if deps.Billing != nil {
			_ = deps.Billing.Terminate(contract.UsageFact{
				Model:    rc.OriginModel,
				Stream:   true,
				Attempts: rc.AttemptIndex + 1,
			})
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
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	h.bridgeStream(w, r, rc, result.Response.Body, streamAdapter, deps.Billing, flusher, start)
}

// bridgeStream 消费适配器的 StreamEvent 序列并写入客户端。
//
// 语义（relay-03 §3–§9、relay-01 §7.6）：
//   - EventRaw：原始 SSE 帧逐字节转发（补足帧结束空行，帧内容不改写）；
//   - EventDelta/ToolDelta/Reasoning：重编码为 OpenAI chat.completion.chunk；
//   - EventUsage：非零合并累计（不擦除已有维度）；
//   - EventFinish：[DONE] 之前写出 include_usage 末块；整个响应对客户端只写一次
//     data: [DONE]；
//   - EventError：写错误帧 + [DONE]（错误文本固定，不含上游原始 body，GC-6）。
//
// 计费（GC-7）：2xx 之后会话已开启。正常完成用累计 UsageFact 终结；客户端断开
// 用 Source=UsageUpstreamPartial 的 partial fact 终结；两者都只调用一次。
func (h *RelayHandler) bridgeStream(
	w http.ResponseWriter,
	r *http.Request,
	rc *contract.RelayContext,
	upstream io.ReadCloser,
	adapter contract.StreamAdapter,
	billing contract.BillingSession,
	flusher http.Flusher,
	start time.Time,
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
		firstTokenMS *int
		finished     bool
		upstreamFail bool
		clientGone   bool
	)

	terminated := false
	terminate := func(fact contract.UsageFact) {
		if terminated || billing == nil {
			return
		}
		terminated = true
		_ = billing.Terminate(fact)
	}
	// GC-7 兜底：任何未走显式终结的出口（含 panic）也必须释放预留。
	defer func() {
		if terminated {
			return
		}
		terminate(contract.UsageFact{
			Model:    model,
			Stream:   true,
			Attempts: rc.AttemptIndex + 1,
		})
	}()

	writeData := func(payload []byte) bool {
		if !writeRelaySSEData(w, flusher, payload) {
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
		// 客户端断开优先判定：即使适配器把取消包装成 EventError，也按
		// EndClientAbort/partial 处理，而不是上游错误。
		if r.Context().Err() != nil {
			clientGone = true
			return false
		}
		received++

		switch ev.Kind {
		case contract.EventRaw:
			if len(ev.Raw) == 0 {
				return true
			}
			if !writeRelaySSERaw(w, flusher, ev.Raw) {
				clientGone = true
				return false
			}
			sent++
			return true

		case contract.EventDelta:
			recordFirstToken()
			payload, err := marshalRelayDeltaChunk(chunkID, created, model, ev)
			if err != nil {
				upstreamFail = true
				return false
			}
			return writeData(payload)

		case contract.EventUsage:
			if ev.Usage != nil {
				mergeUsage(&accumulated, *ev.Usage)
				sawUsage = true
			}
			return true

		case contract.EventFinish:
			// 只记状态：同一上游块可能在 finish 之后仍带 usage（include_usage
			// 末块），立即写 [DONE] 会把它丢掉。统一在迭代结束后收尾。
			finished = true
			return true

		case contract.EventError:
			upstreamFail = true
			// GC-6：不转发上游错误帧（可能含内部主机名/凭据提示），只给固定信封。
			payload, _ := json.Marshal(relayErrorEnvelope{Error: relayErrorBody{
				Message: "upstream stream error",
				Type:    relayErrorType,
				Code:    "upstream_stream_error",
			}})
			_ = writeData(payload)
			return false

		case contract.EventPing:
			// 心跳不是客户端可见事件；保活由传输层/SSE 注释帧负责（relay-03 §3.3）。
			return true

		default:
			return true
		}
	})

	// 收尾帧（客户端已断开时不写：连接已不可用）。
	// include_usage 是我们强制注入的（R-P14），因此正常/EOF 路径都发末块 usage。
	if !clientGone {
		if !upstreamFail {
			if payload, err := marshalRelayUsageChunk(chunkID, created, model, accumulated); err == nil {
				if writeRelaySSEData(w, flusher, payload) {
					sent++
				}
			}
		}
		// relay-03 §3.2：统一结束标记且整个响应只出现一次。
		if writeRelaySSEDone(w, flusher) {
			sent++
		}
	}

	endReason := contract.EndUpstreamDone
	outcome := contract.OutcomeSuccess
	switch {
	case clientGone:
		endReason = contract.EndClientAbort
		outcome = relayStreamOutcome(sent)
	case upstreamFail:
		endReason = contract.EndUpstreamError
		outcome = relayStreamOutcome(sent)
	case !finished:
		endReason = contract.EndUpstreamEOF
		outcome = relayStreamOutcome(sent)
	}
	relay.WithStream(rc, &contract.StreamStatus{
		Phase:     contract.PhaseTerminated,
		EndReason: endReason,
		Outcome:   outcome,
		RecvCount: received,
		SendCount: sent,
	}, sent, received, firstTokenMS)

	fact := contract.UsageFact{
		Model:        model,
		Usage:        accumulated,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		FirstTokenMS: firstTokenMS,
		Stream:       true,
		Attempts:     rc.AttemptIndex + 1,
	}
	switch {
	case clientGone, upstreamFail:
		// 客户端断开/上游中断：已收到的用量只是部分事实（relay-03 §11）。
		fact.Usage.Source = contract.UsageUpstreamPartial
	case !sawUsage:
		// 上游没有给出 usage：标记 estimated，绝不把零值谎报成 upstream_exact
		// （零值 Source 的 iota 是 UsageUpstreamExact）。
		fact.Usage.Source = contract.UsageEstimated
	}
	terminate(fact)
}

// relayStreamOutcome 按已发送帧数判定非正常结束的结果：有内容 = partial。
func relayStreamOutcome(sent int) contract.ResponseOutcome {
	if sent > 0 {
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

// mapRelayError 把路由/执行错误映射为客户端状态码（relay-01 §9.3）。
//
// 顺序即语义：402 先于 FastFail（预扣错误被 FastFail 包装，但契约 §4.4 的
// Payment Required 必须保留）；FastFail 与 AttemptsError 的区分决定「一次失败」
// 与「重试耗尽」的展示。所有消息只含固定文案/次数/状态码，绝不含上游 URL/凭据。
func mapRelayError(err error) (int, string, string) {
	var insufficient *billingseam.InsufficientQuotaError
	if errors.As(err, &insufficient) || errors.Is(err, billingseam.ErrInsufficientQuota) {
		return http.StatusPaymentRequired, "insufficient_quota", "insufficient quota"
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

// ---------- SSE 写出 ----------

// writeRelaySSEData 写一个 `data: <payload>\n\n` 帧并立即 Flush（relay-03 §3）。
func writeRelaySSEData(w http.ResponseWriter, flusher http.Flusher, payload []byte) bool {
	frame := make([]byte, 0, len(payload)+8)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	if _, err := w.Write(frame); err != nil {
		return false
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

// writeRelaySSERaw 逐字节转发原始 SSE 事件（R-P2）。适配器的 Raw 是「事件块原文」
// （行间 \n，无尾部空行），因此这里只补足帧结束空行，不改写内容。
func writeRelaySSERaw(w http.ResponseWriter, flusher http.Flusher, raw []byte) bool {
	frame := append([]byte(nil), raw...)
	switch {
	case bytes.HasSuffix(frame, []byte("\n\n")):
	case bytes.HasSuffix(frame, []byte("\n")):
		frame = append(frame, '\n')
	default:
		frame = append(frame, '\n', '\n')
	}
	if _, err := w.Write(frame); err != nil {
		return false
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

// writeRelaySSEDone 写唯一的统一终止标记（relay-03 §3.2）。
func writeRelaySSEDone(w http.ResponseWriter, flusher http.Flusher) bool {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return false
	}
	if flusher != nil {
		flusher.Flush()
	}
	return true
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
			// *url.Error 的文本含完整 URL → 绝不透出（GC-6）。
			return 0, errors.New("httpapi: channel test: upstream request failed")
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
