package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/crossproto"
	"newtg/gateway/internal/relay/ir"
)

// 本文件由 relay_endpoints.go 按职责拆分（P1-10），仅移动代码，未改动逻辑。

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
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "unsupported target protocol (expected openai, anthropic, or gemini)",
			Structure:    "invalid",
		})
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_protocol",
			"unsupported target protocol (expected openai, anthropic, or gemini)")
		return
	}

	body, err := readRelayBody(r)
	if err != nil {
		if errors.Is(err, errRelayBodyTooLarge) {
			h.recordRefusal(r, billing.Refusal{
				RequestID:    requestID,
				Stage:        billing.RefusalStageRequestStructure,
				ErrorClass:   "param_invalid",
				ErrorMessage: "request body exceeds the 32 MiB limit",
				Structure:    "invalid",
			})
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large",
				"request body exceeds the 32 MiB limit")
			return
		}
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "failed to read request body",
			Structure:    "invalid",
		})
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "failed to read request body")
		return
	}
	// 复原 body：relay.BuildRelayContext 的 peekStreaming 需要读请求体判定 stream
	// （它会把已读前缀拼回）。之后归一化层仍以 IR 的 Stream 为权威值（§7.2）。
	r.Body = io.NopCloser(bytes.NewReader(body))

	parsed, err := ir.ParseOpenAIRequest(body)
	if err != nil {
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "invalid OpenAI request body",
			Structure:    "invalid",
		})
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid OpenAI request body")
		return
	}
	// finding 14（OpenAI stream bridge 丢弃 choice 身份）：stream=true 时
	// 下游 SSE 桥接只编码 index=0 的增量，n>1 会静默丢失其余候选。
	// 与其返回一个语义不完整的流，不如在入口 400 拒绝并明确说明。
	if parsed.Stream && requestChoiceCount(body) > 1 {
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Model:        parsed.Model,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "stream=true with n>1 is not supported: the streaming bridge cannot preserve choice identity",
			Structure:    "invalid",
		})
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request",
			"stream=true with n>1 is not supported: the streaming bridge cannot preserve choice identity")
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		// 只有中间件未挂载才会到这里；fail-closed，绝不把零值身份当匿名用户。
		// 不记录拒绝行：这是网关接线故障（中间件缺失），记成 refused 会把网关
		// 自己的 bug 归到用户头上——正是本功能要防的倒置判定。
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
	// 到这里 IR 已解析成功、入口校验（n>1 流式限制）已通过，结构是真实
	// 「合法」的证据；后续拒绝（限流/模型准入/配额）据此带上 valid。
	rc.RequestStructure = "valid"

	// F-08-18：在 IR 归一化之后、PreConsume/上游之前过滤（见函数注释）。
	// 放在 BuildRelayContext 之后是为了日志能带上 user_id；此时尚未创建
	// 计费会话，block 路径仍然零计费。
	if h.rejectSensitiveContent(parsed, w, requestID, rc.UserID, writeRelayError) {
		return
	}

	// /pg/* 会话路径（relay-01 §1.5）：BuildRelayContext 不查令牌，因此
	// UsingGroup 为空；必须回源 users."group" 再写身份组，否则候选查询会
	// 变成「所有分组」并把定价倍率落到错误分组。
	// 同一分支在计费前执行模型准入：模型不在用户当前分组的可用清单 →
	// 403 且不落任何计费/日志（modules-02 §2.15 入口 6 与 TC-F-02-19-06）。
	if principal.TokenID == nil {
		status, code, message, ok := applyPlaygroundSession(r.Context(), h.userGroups, principal, rc, parsed.Model)
		if !ok {
			writeRelayError(w, status, requestID, code, message)
			return
		}
	}

	// W7/M5-b 准入限流（R-M5-04）：位置固定在这里——BuildRelayContext 与分组
	// 解析之后（/pg 的 UsingGroup 此时才可用），pin/白名单/routeDeps/PreConsume
	// 与上游之前。被拒绝的请求因此零预扣、零 usage_logs、零上游调用；流式与
	// 非流式在分流前只计一次（不按 SSE 帧计）。
	if rejection := checkRelayRateLimit(r.Context(), h.rateLimiter, h.config, rc, principal, clientIP(r)); rejection != nil {
		// Task 3b：限流拒绝在入口直接写响应（不经过 logRelayFailure），因此
		// 在这里补记录。500（限流器故障）是网关故障，明确不记。
		// 演示站两层日额度是站点级容量（全站保险丝），不是调用方自己的令牌
		// 配额；把它记成用户拒绝会误归因，故排除。其余 429 照记。
		if rejection.Status == http.StatusTooManyRequests && rejection.Code != demoRelayQuotaExceededCode {
			h.recordRefusal(r, billing.Refusal{
				RequestID:    rc.RequestID,
				UserID:       int64(rc.UserID),
				TokenID:      refusalTokenID(rc.TokenID),
				Model:        parsed.Model,
				Endpoint:     contract.EndpointLabel(rc.RequestPath),
				Stage:        billing.RefusalStageRateLimit,
				ErrorMessage: "rate limit exceeded",
				Structure:    rc.RequestStructure,
			})
		}
		writeRelayRateLimitError(w, rc.RequestID, rejection)
		return
	}

	// finding 1（relay-01 §3.5/§5.4-A）：pin 必须在 BuildRelayContext **之后**
	// 解析——后者已经完成了「非管理员使用 pin → 403」的统一校验（R-P2a）。
	// 提前解析会让会话路径的 pin 绕过那道校验。
	pinID, hasPin := relay.ParseChannelPin(r)

	// finding 6（relay-01 §3.4）：令牌模型白名单在路由阶段执行；pin 渠道
	// 优先级高于白名单（§3.4 注：pin 命中时跳过白名单）。非管理员 pin
	// 已在 BuildRelayContext 返回 403，因此这里 hasPin 等价于管理员 pin。
	if !hasPin && !tokenModelAllowed(principal.ModelLimits, parsed.Model) {
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			UserID:       int64(rc.UserID),
			TokenID:      refusalTokenID(rc.TokenID),
			Model:        parsed.Model,
			Stage:        billing.RefusalStageModelAdmission,
			ErrorMessage: "this token is not allowed to access the requested model",
			Structure:    rc.RequestStructure,
		})
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
	// Raw 携带本次归一化 IR：计费侧在 PromptTokens 缺席时据此保守估算输入，
	// 调用方不得把 PromptTokens=0 误读成「prompt 免费」。
	relay.WithQuotaEstimate(rc, &contract.QuotaEstimate{
		Model:     parsed.Model,
		Group:     rc.UsingGroup,
		MaxTokens: parsed.MaxTokens,
		Raw:       rc.Request,
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
