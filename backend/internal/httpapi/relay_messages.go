package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/egress"
	"newtg/gateway/internal/relay"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/crossproto"
	"newtg/gateway/internal/relay/ir"
	"newtg/gateway/internal/relay/stream"
	"newtg/gateway/internal/relay/usage"
	"newtg/gateway/internal/tokenestimate"
)

// 本文件由 relay_endpoints.go 按职责拆分（P1-10），仅移动代码，未改动逻辑。

// ---------- POST /v1/messages（Anthropic Messages 原生入口，W7/M1） ----------

// handleMessages 把 Claude 原生客户端（含 Claude Code）的 `/v1/messages`
// 接入中继管线。鉴权复用 Register 里的**同一个** RelayAuth（x-api-key 载体
// 已按路径在 relay_auth.go 归一化），计费复用**同一条** contract.BillingSession
// 接缝（h.billing），不另建计费路径。
//
// 方向矩阵（R-59；W18-B · R-W18-03 放开异构上游）：
//   - 入站 Anthropic → 上游 Anthropic 协议族渠道（type 14 anthropic、
//     type 25 moonshot，后者是 Anthropic 兼容协议）：**同协议透传**。
//     请求经 IR 重新编码（system/messages/max_tokens/tools/stop_sequences 都在
//     IR 建模范围内，Raw 作为未建模字段的透传基底）；响应与 SSE 是上游原生
//     帧，逐字节转发。
//   - 入站 Anthropic → OpenAI / Gemini 原生渠道：**跨协议转码**。请求经 IR
//     编码为上游协议；响应用 ir.BuildAnthropicResponse / AnthropicStreamRenderer
//     回写为 Claude 形状（W18-B 补齐的响应编码器）。
//   - 其余渠道类型（无 openai/anthropic/gemini 适配器，如任务插件）：501。
//
// 为什么之前 fail-closed、现在可以放行：旧的 501 唯一原因是响应方向缺
// IR → Anthropic 编码器，放行会让 Claude 客户端收到 OpenAI 形状的 200。
// R-W18-03 冻结实现编码器后，方向门禁只剩「三类已实现协议之外」的上游，
// 这是真正无法表达的方向，仍返回 501。
//
// usage 归属：契约 contract.Response 没有 Usage 字段（R-W18-03：不扩展冻结
// 结构）。非流式从上游 Raw 按 crossproto.UsageFromNative 解析，与结算侧
// usage.ExtractUsageMultimodal 读同一份字节；流式直接用适配器的 EventUsage
// 累计（与计费同一个 accumulator），保证 usage_logs 与客户端回写一致。
//
// 判定时点：方向由「请求模型 → 候选渠道」决定，必须先读到 model。请求体读入
// 之后、PreConsume 与任何上游调用之前完成判定：不支持的协议方向在构造
// routeDeps 之前返回 501（连计费会话都不创建）。
func (h *RelayHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	// R-W18-04：本端点（含 count_tokens）所有错误出口统一用 Claude 形信封。
	// problem+json 只保留给「端点形态不支持」的 501（writeMessagesMappedError
	// 内部的既有例外），不得用于普通 4xx/5xx。
	writeErr := writeClaudeError

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
			writeErr(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large",
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
		writeErr(w, http.StatusBadRequest, requestID, "invalid_request", "failed to read request body")
		return
	}
	// 复原 body：BuildRelayContext 的 peekStreaming 需要读请求体（stream 标志）。
	r.Body = io.NopCloser(bytes.NewReader(body))

	parsed, err := ir.ParseAnthropicRequest(body)
	if err != nil {
		// GC-6：解析器文本可能回显用户输入片段，对客户端只给固定文案；
		// 服务端日志保留“哪个字段/块类型”这一层，便于定位客户端兼容性问题。
		h.log.Warn("messages: invalid Anthropic request body", "error", err, "request_id", requestID)
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "invalid Anthropic request body",
			Structure:    "invalid",
		})
		writeErr(w, http.StatusBadRequest, requestID, "invalid_request", "invalid Anthropic request body")
		return
	}
	// Anthropic Messages 把 max_tokens 列为**必填**。IR 解析器对缺省保持 nil，
	// 编码器（ir.BuildAnthropicRequestBody）会为跨协议调用补 8192——但那条
	// 默认路径只为「OpenAI 客户端省略 max_tokens」服务，不能让原生入口继承：
	// 缺省时 QuotaEstimate.MaxTokens 为 nil，免费模型的防滥用 hold 会退化为 0
	// （billing/session.go 的 preConsumeFreeModelLocked：MaxTokens 缺失/≤0 →
	// hold 0 → 不写流水），等于让免费模型的高成本请求零预扣（F-08-12）。
	// 因此在这里、PreConsume 与任何上游调用之前显式拒绝缺失、0 与负数。
	// 负数本会被 ir.validateScalars 拦住（同样是 400），这里统一口径。
	if parsed.MaxTokens == nil || *parsed.MaxTokens <= 0 {
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Model:        parsed.Model,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "max_tokens is required and must be a positive integer",
			Structure:    "invalid",
		})
		writeErr(w, http.StatusBadRequest, requestID, "invalid_request",
			"max_tokens is required and must be a positive integer")
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		// 只有中间件未挂载才会到这里；fail-closed，绝不把零值身份当匿名用户。
		// 不记录拒绝行：网关接线故障不得写成用户请求被拒（与 chat 同口径）。
		writeErr(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, code, message := mapRelayContextError(err)
		writeErr(w, status, requestID, code, message)
		return
	}
	// 归一化层是 Request/OriginModel/Streaming 的唯一写入方（relay-01 §7.2）。
	relay.WithRequest(rc, parsed, ir.FormatAnthropicMessages, r.URL.Path)
	// IR 与 max_tokens 校验均已通过（见上方必填校验），结构证据为 valid。
	rc.RequestStructure = "valid"

	// F-08-18：与 chat 同一时点（IR 之后、PreConsume/上游之前）。/v1/messages
	// 的 system 已由解析器放进 Messages[0]（RoleSystem），因此同一个
	// CheckRequest 同时覆盖 prompt 与 system 两个作用面。
	if h.rejectSensitiveContent(parsed, w, requestID, rc.UserID, writeErr) {
		return
	}

	// W7/M5-b 准入限流（R-M5-04）：敏感词过滤之后、pin/白名单/停用检查/预扣
	// 之前。原生入口没有 /pg 分支，因此不存在 TokenID==nil 的会话路径
	// （B11 恒参与）。
	if rejection := checkRelayRateLimit(r.Context(), h.rateLimiter, h.config, rc, principal, clientIP(r)); rejection != nil {
		// Task 3b：与 chat 同口径——入口限流直接写响应，不经过 logRelayFailure。
		// 与 chat 同口径：演示站日额度是站点容量，不计入用户拒绝。
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
		writeRelayRateLimitErrorWith(w, rc.RequestID, rejection, writeErr)
		return
	}

	// pin 必须在 BuildRelayContext 之后解析（R-P2a：非管理员 pin 已在那里
	// 统一拒绝）。/v1/messages 不挂 /pg 会话平面（RegisterPlayground 只暴露
	// 对话族），因此没有 TokenID==nil 的分组回源分支。
	pinID, hasPin := relay.ParseChannelPin(r)
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
		writeErr(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}

	// R-84：停用模型必须在派发与预扣之前 404（与 chat 同口径）。
	if h.modelStatus != nil {
		status, found, statusErr := h.modelStatus.ModelPricingStatus(r.Context(), parsed.Model)
		if statusErr != nil {
			writeErr(w, http.StatusInternalServerError, requestID, "database_error",
				"failed to check model availability")
			return
		}
		if found && status == modelPricingStatusDisabled {
			writeErr(w, http.StatusNotFound, requestID, "model_not_found",
				"the requested model is not available")
			return
		}
	}

	// 与 chat 完全同一份预扣估算（finding 13）。准确口径：MaxTokens 只是
	// QuotaEstimate 的输入之一——付费 token 模型的预扣由 estimateUsage 从
	// Raw 估算输入，并按 max_tokens 预留输出；这里的 MaxTokens 同时决定
	// **免费模型**的防滥用 hold（MaxTokens × multiplier）。上面的入口校验
	// 保证这里永远是非零的合法值。
	relay.WithQuotaEstimate(rc, &contract.QuotaEstimate{
		Model:     parsed.Model,
		Group:     rc.UsingGroup,
		MaxTokens: parsed.MaxTokens,
		Raw:       rc.Request,
	})

	// 方向约束：只允许 openai / anthropic / gemini 三类已实现协议的上游
	// （见上方方向矩阵；W18-B · R-W18-03 放开 OpenAI/Gemini）。
	//
	// 这里在构造 routeDeps（其 Billing 字段会新建计费会话）之前先判定方向：
	// 501 分支因此既不创建计费会话、也不 PreConsume、更不触达上游。
	// 候选为空时不在这里下结论——那属于「没有可用渠道」（503），交给执行层
	// 按既有口径映射。
	//
	// R-68：方向预检必须按**有效分组序列**判定——首组 + 开关开启时的后续组
	// （受 R-63 上限约束）。首组无可用协议、后续组有且开关开启时不得 501，
	// 交给执行层换组；只有所有有候选的组都不含已实现协议才 501。
	if checked, supported := h.messagesDirectionCovered(r.Context(), rc); checked && !supported {
		h.writeMessagesMappedError(w, requestID, crossproto.ErrUpstreamProtocolUnsupported)
		return
	}

	// targetProtocol 传空：/v1/messages 现在接受三类协议中的任意一类，不能再按
	// Anthropic 过滤候选（那会把 OpenAI/Gemini 渠道错误地排除）。具体走
	// 透传还是转码由执行后的 rc.ChannelType 决定（serveNativeJSON/Stream）。
	deps := h.routeDeps(rc, pinID, hasPin, "")
	if parsed.Stream {
		h.serveNativeStream(w, r, rc, deps)
		return
	}
	h.serveNativeJSON(w, r, rc, deps)
}

// ---------- POST /v1/messages/count_tokens（R-W18-06） ----------

// problemTypeCountTokensUnsupported 是 count_tokens 方向不支持的 501 problem
// type（R-W18-04 允许的「端点形态不支持」例外）。
const problemTypeCountTokensUnsupported = "https://newtg.dev/problems/count-tokens-unsupported"

// errCountTokensUnsupported 表示解析到的上游不是 Anthropic 协议族（或该协议族
// 适配器没有实现精确计数）。它不是渠道故障，不重试、不自动禁用。
var errCountTokensUnsupported = errors.New("httpapi: count_tokens requires an Anthropic-protocol channel")

// countTokensAdapter 是 Anthropic 协议族的零计费计数扩展。
//
// 刻意不扩充冻结的 contract.Adapter：count_tokens 只有 Anthropic 一个协议族
// 能精确回答，给所有适配器加方法会让「不支持」与「实现为空」混在一起。
// 窄接口断言让不支持的适配器自然落到 501。
type countTokensAdapter interface {
	BuildCountTokensRequest(rc *contract.RelayContext, body []byte) (*http.Request, error)
	ParseCountTokensResponse(resp *http.Response) (int64, error)
}

// handleMessagesCountTokens 把 Claude 客户端的 count_tokens 透传给 Anthropic
// 协议族上游，响应严格为 `{"input_tokens": N}`。
//
// **零计费**（R-W18-06 冻结裁决）：本函数
//   - 不创建 BillingSession：没有 PreConsume/Terminate/Refund，钱包与
//     wallet_transactions 不可能变化；
//   - 不写 usage_logs：计数不产生模型输出，没有可结算的用量事实；
//   - 不调用 checkRelayRateLimit：不消耗 B10/B11/B12 任何桶。Claude Code 这类
//     客户端会在每次编辑循环里调用 count_tokens，把它计入模型准入桶会让正常
//     使用被 429 挡住，而它本身不消耗上游生成配额。
//
// **不做本地估算**（裁决）：仓库没有 tokenizer（usage 包明确不实现兜底估算），
// 本地近似值冒充精确值会让客户端的上下文预算判断失真。因此只对 Anthropic
// 协议族上游透传精确计数；解析到其它上游时返回 501 problem+json
// （`count_tokens_unsupported`，端点形态不支持的既有例外），不引入 tokenizer。
func (h *RelayHandler) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	requestID := newRelayRequestID()
	writeErr := writeClaudeError

	body, err := readRelayBody(r)
	if err != nil {
		if errors.Is(err, errRelayBodyTooLarge) {
			// 中继前拒绝：count_tokens 与 messages 共用拒绝词表，结构校验
			// 尚未发生（Structure 留空 = 未校验），只记「请求体超限」这一事实。
			h.recordRefusal(r, billing.Refusal{
				RequestID:    requestID,
				Stage:        billing.RefusalStageRequestStructure,
				ErrorClass:   "request_too_large",
				ErrorMessage: "request body exceeds the 32 MiB limit",
			})
			writeErr(w, http.StatusRequestEntityTooLarge, requestID, "request_too_large",
				"request body exceeds the 32 MiB limit")
			return
		}
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "failed to read request body",
		})
		writeErr(w, http.StatusBadRequest, requestID, "invalid_request", "failed to read request body")
		return
	}
	// count_tokens 与 messages 共用同一份 Anthropic IR 解析：敏感词过滤和
	// 模型选路都基于客户端真实内容。解析失败按 messages 的既有 400 处理
	// （fail-closed）——绝不能因为「只是计数」就把无法解析的 body 直接送上游。
	// 其余未建模字段仍保留在 parsed.Raw 里，由 BuildCountTokensRequest 原样透传。
	parsed, err := ir.ParseAnthropicRequest(body)
	if err != nil {
		h.log.Warn("messages count_tokens: invalid Anthropic request body", "error", err, "request_id", requestID)
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "invalid Anthropic request body",
			Structure:    "invalid",
		})
		writeErr(w, http.StatusBadRequest, requestID, "invalid_request", "invalid Anthropic request body")
		return
	}
	if strings.TrimSpace(parsed.Model) == "" {
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			Stage:        billing.RefusalStageRequestStructure,
			ErrorClass:   "param_invalid",
			ErrorMessage: "model is required",
			Structure:    "invalid",
		})
		writeErr(w, http.StatusBadRequest, requestID, "invalid_request", "model is required")
		return
	}

	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, requestID, "", "invalid token")
		return
	}
	rc, err := relay.BuildRelayContext(r.Context(), principal, r, h.tokens)
	if err != nil {
		status, code, message := mapRelayContextError(err)
		writeErr(w, status, requestID, code, message)
		return
	}
	relay.WithRequest(rc, parsed, ir.FormatAnthropicMessages, r.URL.Path)
	// IR 解析与 model 必填校验均已通过；count_tokens 同样带出真实结构证据。
	rc.RequestStructure = "valid"

	// F-08-18：与 /v1/messages 同一时点与同一口径（IR 之后、任何上游调用
	// 之前）。count_tokens 原始 body 会被原样送到上游，若不在这里过滤，敏感词
	// 配置就存在一条绕过路径：messages 400 而 count_tokens 仍把命中文本发给
	// 上游。命中 → Claude 形 400 + 零上游 + 零计费。
	if h.rejectSensitiveContent(parsed, w, requestID, rc.UserID, writeErr) {
		return
	}

	// 演示站额度（P3-2 裁决）：count_tokens 虽零计费，但仍是一次真实上游调用。
	// 演示模式下计入 site.demo.relay_quota.* 两层日额度（超限沿用 429/403），
	// 但**不**调用 checkRelayRateLimit——因此不消耗 B10/B11/B12 任何桶。
	// 演示额度使用的是独立的 demo:relay:* 键，与中继准入桶物理隔离。
	if rejection := checkDemoRelayQuota(r.Context(), h.rateLimiter, h.config, rc.RequestID, clientIP(r)); rejection != nil {
		writeRelayRateLimitErrorWith(w, rc.RequestID, rejection, writeErr)
		return
	}

	// 与 messages 同口径的入口准入：pin 权限、令牌模型白名单、停用模型。
	// 这些检查都不产生计费或上游调用，失败路径仍零副作用。
	pinID, hasPin := relay.ParseChannelPin(r)
	if !hasPin && !tokenModelAllowed(principal.ModelLimits, parsed.Model) {
		h.recordRefusal(r, billing.Refusal{
			RequestID:    requestID,
			UserID:       int64(rc.UserID),
			TokenID:      refusalTokenID(rc.TokenID),
			Model:        parsed.Model,
			Endpoint:     contract.EndpointLabel(rc.RequestPath),
			Stage:        billing.RefusalStageModelAdmission,
			ErrorMessage: "this token is not allowed to access the requested model",
			Structure:    rc.RequestStructure,
		})
		writeErr(w, http.StatusForbidden, requestID, "distributor_token_model_forbidden",
			"this token is not allowed to access the requested model")
		return
	}
	if h.modelStatus != nil {
		status, found, statusErr := h.modelStatus.ModelPricingStatus(r.Context(), parsed.Model)
		if statusErr != nil {
			writeErr(w, http.StatusInternalServerError, requestID, "database_error",
				"failed to check model availability")
			return
		}
		if found && status == modelPricingStatusDisabled {
			writeErr(w, http.StatusNotFound, requestID, "model_not_found",
				"the requested model is not available")
			return
		}
	}

	assignment, err := h.selectCountTokensChannel(r.Context(), rc, pinID, hasPin)
	switch {
	case errors.Is(err, errCountTokensUnsupported):
		if requestID != "" {
			w.Header().Set("X-Request-ID", requestID)
		}
		writeProblemDetail(w, http.StatusNotImplemented, problemTypeCountTokensUnsupported,
			"count_tokens requires an Anthropic-protocol channel",
			"count_tokens_unsupported: only Anthropic/Moonshot channels can return exact token counts; "+
				"this request performs no billing and no upstream call")
		return
	case errors.Is(err, relay.ErrChannelNotFound):
		writeErr(w, http.StatusNotFound, requestID, "channel_not_found",
			"the pinned channel is not available")
		return
	case errors.Is(err, relay.ErrNoAvailableChannel):
		writeErr(w, http.StatusServiceUnavailable, requestID, "model_not_found",
			"no available channel for the requested model")
		return
	case err != nil:
		// 存储/凭据解封失败：固定 500 文案，不回显底层错误（GC-6）。
		writeErr(w, http.StatusInternalServerError, requestID, "database_error",
			"failed to resolve the upstream channel")
		return
	}
	relay.WithChannel(rc, assignment)

	adapter, err := h.dispatch(assignment.ChannelType)
	if err != nil {
		h.writeCountTokensUnsupported(w, requestID)
		return
	}
	counter, ok := adapter.(countTokensAdapter)
	if !ok {
		h.writeCountTokensUnsupported(w, requestID)
		return
	}
	upstreamReq, err := counter.BuildCountTokensRequest(rc, body)
	if err != nil {
		writeCountTokensRequestError(w, requestID, err)
		return
	}
	client := h.client
	if client == nil {
		client = newRelayHTTPClient(egress.NewGuard(egress.ChannelPolicy(h.config)))
	}
	// R-01：与 chat 执行层同口径，count-tokens 也是一次真实出站。
	if assignment.AllowPrivateNetwork {
		upstreamReq = upstreamReq.WithContext(
			egress.WithAllowPrivateNetwork(upstreamReq.Context()))
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		// 传输层失败：不把底层文本（可能含 URL）交给客户端。
		h.logRelayFailure(r, rc, err)
		writeErr(w, http.StatusBadGateway, requestID, "upstream_error", "upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		class := adapter.ClassifyError(resp.StatusCode, nil)
		upstreamErr := &relay.UpstreamStatusError{StatusCode: resp.StatusCode, Class: class}
		h.logRelayFailure(r, rc, upstreamErr)
		status, _, message := relayUpstreamStatus(upstreamErr)
		writeErr(w, status, requestID, "upstream_error", message)
		return
	}
	tokens, err := counter.ParseCountTokensResponse(resp)
	if err != nil {
		h.logRelayFailure(r, rc, err)
		writeErr(w, http.StatusBadGateway, requestID, "upstream_error",
			"upstream returned an invalid count_tokens response")
		return
	}
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		InputTokens int64 `json:"input_tokens"`
	}{InputTokens: tokens})
}

// writeCountTokensUnsupported 写「端点方向不支持」的 501 problem+json。
func (h *RelayHandler) writeCountTokensUnsupported(w http.ResponseWriter, requestID string) {
	if requestID != "" {
		w.Header().Set("X-Request-ID", requestID)
	}
	writeProblemDetail(w, http.StatusNotImplemented, problemTypeCountTokensUnsupported,
		"count_tokens requires an Anthropic-protocol channel",
		"count_tokens_unsupported: only Anthropic/Moonshot channels can return exact token counts; "+
			"this request performs no billing and no upstream call")
}

// writeCountTokensRequestError 映射适配器构造请求失败。
//
// 参数类错误（请求体非法/模型为空）是客户端 400；其余（渠道配置）按 502
// 上游请求失败处理——与 chat 的 mapRelayError 口径一致，且不回显底层文本。
func writeCountTokensRequestError(w http.ResponseWriter, requestID string, err error) {
	if class, ok := adapters.ClassOf(err); ok && class == contract.ErrParamInvalid {
		writeClaudeError(w, http.StatusBadRequest, requestID, "invalid_request", "invalid count_tokens request")
		return
	}
	writeClaudeError(w, http.StatusBadGateway, requestID, "upstream_error", "upstream request failed")
}

// selectCountTokensChannel 按有效分组序列选出一个 Anthropic 协议族渠道。
//
// 语义与 /v1/messages 的方向预检一致（R-68）：
//   - 按 GroupSequence 逐组看候选；任一有效组内有 Anthropic 协议族渠道即选中；
//   - 有候选但全不是 Anthropic 族 → errCountTokensUnsupported（501）；
//   - 完全无候选 → relay.ErrNoAvailableChannel（503）；
//   - pin 指向的渠道不在候选集 → relay.ErrChannelNotFound（404）；
//     pin 命中但非 Anthropic 族 → errCountTokensUnsupported。
//
// 这里不重试：count_tokens 零计费、幂等，单次失败让客户端自行重试即可；
// 引入重试会顺便带入自动禁用上报（针对生成请求设计），与零计费端点不符。
func (h *RelayHandler) selectCountTokensChannel(ctx context.Context,
	rc *contract.RelayContext, pinID int, hasPin bool) (relay.ChannelAssignment, error) {
	sawCandidates := false
	for _, group := range relay.GroupSequence(rc, h.maxAutoGroups) {
		candidates, err := h.channels.Candidates(ctx, rc.OriginModel, group)
		if err != nil {
			return relay.ChannelAssignment{}, err
		}
		if len(candidates) == 0 {
			continue
		}
		sawCandidates = true
		if hasPin {
			inGroup := false
			for _, candidate := range candidates {
				if candidate.ChannelID == pinID {
					inGroup = true
					break
				}
			}
			if !inGroup {
				continue
			}
			ch, err := h.channels.Get(ctx, int64(pinID))
			if err != nil {
				return relay.ChannelAssignment{}, err
			}
			if !crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), crossproto.ProtocolAnthropic) {
				return relay.ChannelAssignment{}, errCountTokensUnsupported
			}
			// 计费虽不发生，仍按该组写入 UsingGroup：渠道解析的模型映射
			// 与后续诊断都以「实际服务该请求的分组」为准。
			rc.UsingGroup = group
			return h.resolveChannel(ctx, rc, pinID)
		}
		for _, candidate := range candidates {
			ch, err := h.channels.Get(ctx, int64(candidate.ChannelID))
			if err != nil {
				// 与方向预检同口径：Get 失败的候选按不可用跳过。
				continue
			}
			if !crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), crossproto.ProtocolAnthropic) {
				continue
			}
			rc.UsingGroup = group
			return h.resolveChannel(ctx, rc, candidate.ChannelID)
		}
	}
	if hasPin {
		return relay.ChannelAssignment{}, relay.ErrChannelNotFound
	}
	if !sawCandidates {
		return relay.ChannelAssignment{}, relay.ErrNoAvailableChannel
	}
	return relay.ChannelAssignment{}, errCountTokensUnsupported
}

// messagesDirectionCovered 按 R-68 的**有效分组序列**做方向预检。
//
// 返回 checked=false 表示有效序列中没有任何组存在候选（无法下方向结论，
// 交给执行层按 ErrNoAvailableChannel/换组处理）；checked=true 且
// supported=false 才表示「端点方向不支持」→ 501。这里与执行层共用
// relay.GroupSequence，避免预检与真实换组范围漂移。
func (h *RelayHandler) messagesDirectionCovered(ctx context.Context, rc *contract.RelayContext) (checked, supported bool) {
	for _, group := range relay.GroupSequence(rc, h.maxAutoGroups) {
		candidates, err := h.channels.Candidates(ctx, rc.OriginModel, group)
		if err != nil || len(candidates) == 0 {
			// 与既有单组口径一致：查询失败/空组不下方向结论，交给执行层。
			continue
		}
		checked = true
		if h.messagesDirectionSupported(ctx, candidates) {
			return true, true
		}
	}
	return checked, false
}

// messagesDirectionSupported 判定候选集里是否存在**已实现协议族**的渠道。
//
// W18-B · R-W18-03：/v1/messages 不再限定 Anthropic 上游——OpenAI/Gemini
// 经 IR 响应编码器回写，因此三类协议都算「方向支持」。仍然拒绝的是没有
// openai/anthropic/gemini 适配器的渠道类型（如任务插件），它们无法被中继
// 表达为任何对话响应。
//
// 渠道类型 → 协议族的映射只来自 adapters.DispatchTable（crossproto 的
// channelType 映射），不在这里另抄类型清单：新增原生渠道时方向判定必须
// 自动跟随分派表。Get 失败的候选按「不可用」跳过（与执行层换渠道同口径）；
// 真正的错误留给执行层映射。
func (h *RelayHandler) messagesDirectionSupported(ctx context.Context, candidates []contract.ChannelCandidate) bool {
	for _, candidate := range candidates {
		ch, err := h.channels.Get(ctx, int64(candidate.ChannelID))
		if err != nil {
			continue
		}
		if crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), crossproto.ProtocolAnthropic) ||
			crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), crossproto.ProtocolOpenAI) ||
			crossproto.ChannelTypeSupportsProtocol(contract.ChannelType(ch.Type), crossproto.ProtocolGemini) {
			return true
		}
	}
	return false
}

// writeMessagesMappedError 是 /v1/messages 的错误出口。
//
// 唯一的差异化映射是「没有已实现协议族的渠道」：这是**端点的方向不支持**
// （决定是否放行的是协议族能力，不是这一次请求的参数），返回 501
// problem+json，与 /v1/responses 的 fail-closed 共用同一个 problem type。
// 资金副作用为 0：常规路径甚至不创建计费会话；pin 触发的 TOCTOU 兜底路径
// 只创建一个会话，PreConsume/Terminate 从不发生，非流式兜底会对未预扣会话
// 调用一次 no-op Refund（真实引擎无 hold 时直接返回 nil，不写流水）。
//
// 其余错误按 R-W18-04 一律用 Claude 形（状态码/Retry-After/requestId 语义
// 与 mapRelayError + writeRelayMappedError 完全一致，只替换信封形状）。
// W18-B · R-W18-03 之后，Anthropic/OpenAI/Gemini 上游都算支持，只有三类
// 之外的渠道类型（如任务插件）才会走到上面的 501。
func (h *RelayHandler) writeMessagesMappedError(w http.ResponseWriter, requestID string, err error) {
	if errors.Is(err, crossproto.ErrUpstreamProtocolUnsupported) {
		if requestID != "" {
			w.Header().Set("X-Request-ID", requestID)
		}
		writeProblemDetail(w, http.StatusNotImplemented, problemTypeEndpointNotSupported,
			"Messages endpoint requires an openai/anthropic/gemini channel",
			"the generic relay can only answer /v1/messages through channels with an implemented "+
				"openai, anthropic or gemini adapter; this request performs no billing and no upstream call")
		return
	}
	writeClaudeMappedError(w, requestID, err)
}

// writeClaudeMappedError 用 Claude 信封写映射后的路由/执行错误。
//
// 409 + ErrSettlementInProgress 的 Retry-After 与 OpenAI 出口保持一致：
// 裁决要求错误信封改造不得改变这一可重试信号。
func writeClaudeMappedError(w http.ResponseWriter, requestID string, err error) {
	status, code, message := mapRelayError(err)
	if status == http.StatusConflict && errors.Is(err, billing.ErrSettlementInProgress) {
		w.Header().Set("Retry-After", "1")
	}
	writeClaudeError(w, status, requestID, code, message)
}

// serveNativeJSON 是非流式 /v1/messages 的执行与回写。
//
// 与 serveJSON 的差异：同协议（Anthropic 上游）时**不做响应转换**，resp.Raw
// 就是 Anthropic 客户端期望的原生 JSON——逐字节透传（与 M2 的 OpenAI→OpenAI
// 透传同口径）。异构上游（OpenAI/Gemini）时按 W18-B · R-W18-03 走
// ir.BuildAnthropicResponse，把 IR 响应编码成 Claude 形状。
// 计费仍由 relay.Route 在自己的 defer 里完成；这里包一层 deferredSettlement
// 只是复用 chat 的「编码/写失败 → 不收费」语义。
func (h *RelayHandler) serveNativeJSON(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	var settlement *deferredSettlement
	if deps.Billing != nil {
		settlement = newDeferredSettlement(deps.Billing)
		deps.Billing = settlement
		defer settlement.abort()
	}
	resp, err := relay.Route(r.Context(), rc, deps)
	if err != nil {
		h.logRelayFailure(r, rc, err)
		h.writeMessagesMappedError(w, rc.RequestID, err)
		return
	}
	protocol, known := crossproto.ProtocolOfChannelType(rc.ChannelType)
	if !known {
		// 分派表之外的协议没有响应编码器；绝不把未知形状的 200 透传给客户端。
		// 信封跟随 /v1/messages 的 Claude 形（R-W18-04）。
		writeClaudeError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"upstream protocol does not match this endpoint")
		return
	}
	if resp == nil || len(resp.Raw) == 0 {
		// 原生响应体缺失：宁可 502 也不回一份形状未知的 JSON。
		writeClaudeError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"upstream returned an empty response")
		return
	}
	body := resp.Raw
	if protocol != crossproto.ProtocolAnthropic {
		// 异构上游：从 Raw 重新解析 usage（与结算侧同一份字节、同一口径），
		// 再编码为 Anthropic 消息。编码失败属于输出不可用，走 abort 退款。
		usage, _ := crossproto.UsageFromNative(protocol, resp.Raw)
		encoded, encodeErr := ir.BuildAnthropicResponse(resp, usage)
		if encodeErr != nil {
			// 编码失败是输出不可用；信封仍是 Claude 形（R-W18-04）。
			writeClaudeError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
				"failed to encode Anthropic response")
			return
		}
		body = encoded
	}
	if settlement != nil {
		// 用户付不起这笔账：不交付服务。hold 留在账本由补偿扫描处理，
		// 这里绝不能 Refund（上游已经执行完了）。
		if commitErr := settlement.commit(); commitErr != nil {
			// Important-2：资金已提交、hold 已关闭，只有日志写失败时钱其实
			// 已经收上来了——拒绝交付只会让客户端重试并二次扣费。
			postCharge := errors.Is(commitErr, billing.ErrPostChargeLogFailed)
			h.reportSettlementFailure(rc, commitErr, postCharge)
			if !postCharge {
				h.writeMessagesMappedError(w, rc.RequestID, commitErr)
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// serveNativeStream 是流式 /v1/messages 的执行与桥接。
//
// 与 serveStream 的差异：客户端协议是 Anthropic，因此**不**走 OpenAI 的
// chat.completion.chunk 渲染，也不写 `data: [DONE]`（Anthropic SSE 以
// `message_stop` 收尾）。
//
// W18-B · R-W18-03：Anthropic 上游继续逐字节透传（pipeNativeAnthropicStream）；
// OpenAI/Gemini 上游经 AnthropicStreamRenderer 渲染为 Anthropic 事件序列
// （bridgeAnthropicCrossProtocolStream）。两条路径的计费接缝完全相同：
// 一个 accumulator 既写 UsageFact 又决定回写的 usage，保证只结算一次且
// usage_logs 的 token 数与客户端看到的 message_delta 一致。
func (h *RelayHandler) serveNativeStream(w http.ResponseWriter, r *http.Request, rc *contract.RelayContext, deps relay.RouteDeps) {
	// 流式 usage：跨协议方向由 ir.BuildOpenAIRequestBody 在重建 OpenAI 请求时
	// 自动补 stream_options.include_usage（Anthropic 入站的 Raw 不是 OpenAI
	// 形状，无法透传该字段）；同协议 Anthropic/Gemini 的流本身恒带用量。
	// 因此这里不注入，也不需要预先知道上游协议。
	start := time.Now()
	result, err := relay.ExecuteAttempts(r.Context(), rc, deps)
	if err != nil {
		h.logRelayFailure(r, rc, err)
		h.writeMessagesMappedError(w, rc.RequestID, err)
		return
	}
	defer func() { _ = result.Response.Body.Close() }()

	streamAdapter, ok := result.Adapter.(contract.StreamAdapter)
	if !ok {
		// 预扣已成功（拿到 2xx），失败出口必须退款（R-85）。
		if deps.Billing != nil {
			_ = deps.Billing.Refund()
		}
		writeClaudeError(w, http.StatusBadGateway, rc.RequestID, "stream_unsupported",
			"upstream adapter does not support streaming")
		return
	}

	protocol, known := crossproto.ProtocolOfChannelType(rc.ChannelType)
	if !known {
		// 分派表之外的协议无法选择透传或转码：退款，不写任何响应帧。
		if deps.Billing != nil {
			_ = deps.Billing.Refund()
		}
		writeClaudeError(w, http.StatusBadGateway, rc.RequestID, "upstream_error",
			"upstream protocol does not match this endpoint")
		return
	}

	stream.SetSSEHeaders(w.Header())
	if rc.RequestID != "" {
		w.Header().Set("X-Request-ID", rc.RequestID)
	}
	sse := newRelaySSEWriter(w)
	w.WriteHeader(http.StatusOK)
	sse.flush()

	if protocol == crossproto.ProtocolAnthropic {
		h.pipeNativeAnthropicStream(r, rc, result.Response.Body, result.Response.Header, streamAdapter, deps.Billing, sse, start)
		return
	}
	h.bridgeAnthropicCrossProtocolStream(r, rc, result.Response.Body, result.Response.Header, streamAdapter, deps.Billing, sse, start)
}

// pipeNativeAnthropicStream 把 Anthropic 上游的 SSE 字节逐字节转给客户端，
// 同时用适配器的 StreamEvents 解析**同一份字节**做用量提取与终结计费。
//
// 为什么不是「直接 io.Copy」：计费需要 usage 事实，而 Chat 口径的桥接会把
// 事件重编码成 OpenAI 形状。这里用 io.TeeReader 把上游字节一分为二
// （客户端写出一份、适配器解析一份），内容零改写；适配器只作为**用量与结束
// 信号**的来源。这是同协议透传，不是「碰运气」：字节本来就是该协议合法的
// 响应流（与 M2 的 OpenAI 原始帧透传同一判据）。
//
// 异常语义与 bridgeStream 一致：客户端断开/上游读取失败/看门狗超时一律
// Refund（R-85），不伪造 `message_stop` 或任何收尾帧（relay-03 §5.2/§10.3）。
//
// 与 bridgeStream 的两处口径差异，均因字节级透传没有协议级分帧：
//   - SendCount 按空行边界计 SSE 帧（见 countFrames），ping 帧也计入；
//   - OutcomePartial 的判据是「已写出任何帧」，无法像语义桥接那样只数内容帧。
//     一个只发 ping 后中断的流会被记为 partial 而非 failed——两者都是异常
//     结束并全额退款，只影响审计字段。
func (h *RelayHandler) pipeNativeAnthropicStream(
	r *http.Request,
	rc *contract.RelayContext,
	upstream io.ReadCloser,
	upstreamHeader http.Header,
	adapter contract.StreamAdapter,
	billing contract.BillingSession,
	sse *relaySSEWriter,
	start time.Time,
) {
	var (
		accumulated  contract.Usage
		sawUsage     bool
		received     int
		sent         int
		softErrors   int
		firstTokenMS *int
		finished     bool
		upstreamFail bool
		clientGone   bool
		timedOut     atomic.Bool
	)

	var closeOnce sync.Once
	closeUpstream := func() { closeOnce.Do(func() { _ = upstream.Close() }) }

	terminated := false
	finalize := func(terminate func() error) error {
		if terminated || billing == nil {
			return nil
		}
		terminated = true
		return terminate()
	}
	terminate := func(fact contract.UsageFact) {
		if err := finalize(func() error { return billing.Terminate(fact) }); err != nil {
			// 流式响应已经写出：无法改 402，只能留下结算失败证据。
			h.reportSettlementFailure(rc, err, true)
		}
	}
	refund := func() {
		_ = finalize(func() error { return billing.Refund() })
	}
	// GC-7 兜底：任何未走显式终结的出口（含 panic）都必须释放预留。
	defer refund()

	// 看门狗与 bridgeStream 同口径：块间静默超过 streamInactivity 即判超时，
	// 关闭上游 body 解除阻塞读。
	inactivity := h.streamInactivity
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	watchdog := time.AfterFunc(inactivity, func() {
		timedOut.Store(true)
		closeUpstream()
	})
	defer watchdog.Stop()
	touch := func() {
		if !timedOut.Load() {
			watchdog.Reset(inactivity)
		}
	}

	defer func() {
		if rec := recover(); rec != nil {
			watchdog.Stop()
			relay.WithStream(rc, &contract.StreamStatus{
				Phase:     contract.PhaseTerminated,
				EndReason: contract.EndPanic,
				Outcome:   relayStreamOutcome(sent),
				RecvCount: received,
				SendCount: sent,
			}, sent, received, firstTokenMS)
			refund()
		}
	}()

	// teeWriter 是 io.TeeReader 的写端：原始字节块直接作为客户端帧写出。
	// 写失败即客户端断开，返回错误会让 StreamEvents 的读取中止。
	//
	// SendCount 的口径是**SSE 帧数**（与 chat 桥接每个 EventRaw/合成帧记一次
	// 同义），不是写次数：tee 拿到的是上游字节块，可能含多帧、也可能在帧中间
	// 被截断。lineEmpty 跨块保留，帧边界落在两次 Read 之间也能正确计数。
	lineEmpty := false
	countFrames := func(p []byte) {
		for _, b := range p {
			switch b {
			case '\n':
				if lineEmpty {
					sent++
				}
				lineEmpty = true
			case '\r':
				// CRLF 的 \r 不改变行状态，由随后的 \n 收行。
			default:
				lineEmpty = false
			}
		}
	}
	teeWriter := writerFunc(func(p []byte) (int, error) {
		if len(p) == 0 {
			return 0, nil
		}
		if !sse.writeFrame(p) {
			clientGone = true
			return 0, io.ErrClosedPipe
		}
		countFrames(p)
		if firstTokenMS == nil {
			ms := int(time.Since(start).Milliseconds())
			firstTokenMS = &ms
		}
		return len(p), nil
	})

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: io.TeeReader(upstream, teeWriter), Writer: io.Discard}

	adapter.StreamEvents(rc, rw)(func(ev contract.StreamEvent) bool {
		if r.Context().Err() != nil {
			clientGone = true
			return false
		}
		if clientGone {
			return false
		}
		if timedOut.Load() {
			upstreamFail = true
			return false
		}
		touch()
		received++

		switch ev.Kind {
		case contract.EventUsage:
			if ev.Usage != nil {
				mergeUsage(&accumulated, *ev.Usage)
				sawUsage = true
			}
		case contract.EventFinish:
			// 结束帧本身已由 tee 写出（Anthropic 的 message_stop）；这里只记录
			// 「上游正常收尾」以便 EndReason 判定。
			finished = true
		case contract.EventError:
			// 软错误（坏 JSON 块/上游 error 事件）原样到达客户端后继续；
			// 硬错误（Raw 为空：读取失败/超限）或软错误超限则终止。
			if len(ev.Raw) > 0 && softErrors < maxSoftStreamErrors {
				softErrors++
				return true
			}
			upstreamFail = true
			return false
		}
		return true
	})

	// 头部兜底（R-05 优先级 3）：事件流没给出任何非零用量时，读上游
	// `x-model-response-usage` 头；仍没有则由估计值兜底（Source=estimated）。
	if !sawUsage || !usageHasFact(accumulated) {
		extracted := usage.ExtractUsageMultimodal(&contract.Response{}, upstreamHeader, nil, rc.ChannelType)
		if extracted.Usage.Source == contract.UsageUpstreamExact {
			accumulated = extracted.Usage
			sawUsage = true
		}
	}

	abnormal := clientGone || upstreamFail || timedOut.Load()
	endReason := contract.EndUpstreamDone
	outcome := contract.OutcomeSuccess
	switch {
	case clientGone:
		endReason = contract.EndClientAbort
		outcome = relayStreamOutcome(sent)
	case timedOut.Load():
		endReason = contract.EndStreamTimeout
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

	if abnormal {
		refund()
		return
	}

	fact := contract.UsageFact{
		Model:        rc.OriginModel,
		Usage:        accumulated,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		FirstTokenMS: firstTokenMS,
		Stream:       true,
		Attempts:     rc.AttemptIndex + 1,
		// 流式路径不保留上游响应 id；归属维度照常冻结（对账按 SUM 聚合，
		// response_id 列对流式行文档化为 NULL）。
		UpstreamModel:     rc.UpstreamModel,
		UpstreamAccountID: rc.UpstreamAccountID,
	}
	if !usageHasFact(accumulated) {
		fact.Usage = estimatedUsageFor(rc.ChannelType)
	}
	terminate(fact)
}

// bridgeAnthropicCrossProtocolStream 把 OpenAI/Gemini 上游的 IR 事件流渲染为
// Anthropic SSE（W18-B · R-W18-03）。
//
// 与 pipeNativeAnthropicStream 的分工：那里是**同协议逐字节透传**（上游字节
// 就是客户端的合法响应）；这里必须重编码——上游是 OpenAI 的
// chat.completion.chunk 或 Gemini 的 candidates 帧，Claude 客户端无法解析。
//
// 计费与输出的一致性：accumulated 是**唯一**的用量来源，既用于
// Terminate(UsageFact)，也传给渲染器的 message_delta，因此 usage_logs 的
// token 数与客户端看到的 usage 不会各说各话（R-W18-03 的验收点）。
//
// 异常语义与 bridgeStream/pipeNativeAnthropicStream 完全一致：
//   - 客户端断开/上游读错误/超时 → Refund，绝不伪造 message_delta/message_stop；
//   - EventRaw（Gemini inlineData 等无法用 Anthropic 块表达的内容）→ 按连接级
//     失败处理（不静默丢弃内容）。
func (h *RelayHandler) bridgeAnthropicCrossProtocolStream(
	r *http.Request,
	rc *contract.RelayContext,
	upstream io.ReadCloser,
	upstreamHeader http.Header,
	adapter contract.StreamAdapter,
	billing contract.BillingSession,
	sse *relaySSEWriter,
	start time.Time,
) {
	var (
		accumulated  contract.Usage
		sawUsage     bool
		received     int
		sent         int
		softErrors   int
		firstTokenMS *int
		finished     bool
		upstreamFail bool
		clientGone   bool
		timedOut     atomic.Bool
	)

	var closeOnce sync.Once
	closeUpstream := func() { closeOnce.Do(func() { _ = upstream.Close() }) }

	terminated := false
	finalize := func(terminate func() error) error {
		if terminated || billing == nil {
			return nil
		}
		terminated = true
		return terminate()
	}
	terminate := func(fact contract.UsageFact) {
		if err := finalize(func() error { return billing.Terminate(fact) }); err != nil {
			// 流式响应已经写出：无法改 402，只能留下结算失败证据。
			h.reportSettlementFailure(rc, err, true)
		}
	}
	refund := func() {
		_ = finalize(func() error { return billing.Refund() })
	}
	// GC-7 兜底：任何未走显式终结的出口（含 panic）都必须释放预留。
	defer refund()

	inactivity := h.streamInactivity
	if inactivity <= 0 {
		inactivity = streamInactivityTimeout
	}
	watchdog := time.AfterFunc(inactivity, func() {
		timedOut.Store(true)
		closeUpstream()
	})
	defer watchdog.Stop()
	touch := func() {
		if !timedOut.Load() {
			watchdog.Reset(inactivity)
		}
	}

	defer func() {
		if rec := recover(); rec != nil {
			watchdog.Stop()
			relay.WithStream(rc, &contract.StreamStatus{
				Phase:     contract.PhaseTerminated,
				EndReason: contract.EndPanic,
				Outcome:   relayStreamOutcome(sent),
				RecvCount: received,
				SendCount: sent,
			}, sent, received, firstTokenMS)
			refund()
		}
	}()

	// message_start 必须最先写出：Claude 客户端在没有它之前不认任何事件。
	// 异构上游的精确用量只在流末尾出现，这里先填本地估算的展示值，避免
	// 整个生成过程里客户端都以为上下文是空的；精确值由 message_delta 覆盖。
	renderer := ir.NewAnthropicStreamRenderer(anthropicMessageID(rc), rc.OriginModel).
		WithInputTokens(int(tokenestimate.DisplayPromptTokens(rc.Request)))
	for _, frame := range renderer.Start() {
		if !sse.anthropicEvent(frame.Event, frame.Data) {
			clientGone = true
			break
		}
		sent++
	}

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: upstream, Writer: io.Discard}

	if !clientGone {
		adapter.StreamEvents(rc, rw)(func(ev contract.StreamEvent) bool {
			if r.Context().Err() != nil {
				clientGone = true
				return false
			}
			if clientGone {
				return false
			}
			if timedOut.Load() {
				upstreamFail = true
				return false
			}
			touch()
			received++

			switch ev.Kind {
			case contract.EventUsage:
				if ev.Usage != nil {
					mergeUsage(&accumulated, *ev.Usage)
					sawUsage = true
				}
			case contract.EventFinish:
				finished = true
			case contract.EventRaw:
				// 原生帧承载无法用 Anthropic 块表达的内容（如 Gemini inlineData）：
				// 丢弃即内容丢失，必须按连接级失败终止（crossproto 的 F2 同一口径）。
				upstreamFail = true
				return false
			case contract.EventError:
				// 软错误（坏 JSON 块/上游 error 事件）没有可渲染内容；计入上限后继续。
				// 硬错误（Raw 为空：读取失败/超限）终止。
				if len(ev.Raw) > 0 && softErrors < maxSoftStreamErrors {
					softErrors++
					return true
				}
				upstreamFail = true
				return false
			}

			for _, frame := range renderer.Push(ev) {
				if !sse.anthropicEvent(frame.Event, frame.Data) {
					clientGone = true
					return false
				}
				sent++
				if firstTokenMS == nil {
					ms := int(time.Since(start).Milliseconds())
					firstTokenMS = &ms
				}
			}
			return true
		})
	}

	// 事件循环以 EOF 结束时不会再有 yield 回调，因此上面的 ctx 检查覆盖不到
	// 「客户端断开 → 上游 body 被级联取消 → 直接 EOF」这条路径。这里补一次
	// 判定，否则会把半截流误当正常收尾，写出伪造的 message_delta/message_stop
	// 并结算（复核 F5）。
	if r.Context().Err() != nil {
		clientGone = true
	}

	// 头部兜底（R-05 优先级 3）：事件流没给出任何非零用量时读上游
	// `x-model-response-usage` 头；仍没有则由估计值兜底（Source=estimated）。
	if !sawUsage || !usageHasFact(accumulated) {
		extracted := usage.ExtractUsageMultimodal(&contract.Response{}, upstreamHeader, nil, rc.ChannelType)
		if extracted.Usage.Source == contract.UsageUpstreamExact {
			accumulated = extracted.Usage
			sawUsage = true
		}
	}

	abnormal := clientGone || upstreamFail || timedOut.Load()
	endReason := contract.EndUpstreamDone
	outcome := contract.OutcomeSuccess
	switch {
	case clientGone:
		endReason = contract.EndClientAbort
		outcome = relayStreamOutcome(sent)
	case timedOut.Load():
		endReason = contract.EndStreamTimeout
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

	if abnormal {
		refund()
		return
	}

	// 结算与回写必须用同一份 finalUsage：先确定事实用量（含 header/估计兜底），
	// 再交给渲染器写 message_delta，保证 usage_logs 与客户端看到的一致。
	finalUsage := accumulated
	if !usageHasFact(finalUsage) {
		finalUsage = estimatedUsageFor(rc.ChannelType)
	}

	// 只在正常收尾时写 content_block_stop/message_delta/message_stop；
	// 异常流不伪造收尾（relay-03 §5.2/§10.3）。
	for _, frame := range renderer.Close(finalUsage) {
		if !sse.anthropicEvent(frame.Event, frame.Data) {
			// 收尾帧写不出去 = 客户端已断开：整笔退款，不结算半截流。
			refund()
			return
		}
		sent++
	}

	fact := contract.UsageFact{
		Model:        rc.OriginModel,
		Usage:        finalUsage,
		LatencyMS:    int(time.Since(start).Milliseconds()),
		FirstTokenMS: firstTokenMS,
		Stream:       true,
		Attempts:     rc.AttemptIndex + 1,
		// 流式路径不保留上游响应 id；归属维度照常冻结。
		UpstreamModel:     rc.UpstreamModel,
		UpstreamAccountID: rc.UpstreamAccountID,
	}
	terminate(fact)
}

// anthropicMessageID 由请求 ID 派生 Anthropic 消息 ID（缺省给稳定占位）。
func anthropicMessageID(rc *contract.RelayContext) string {
	if rc != nil && rc.RequestID != "" {
		return "msg_" + rc.RequestID
	}
	return "msg_newtg"
}

// writerFunc 把函数适配为 io.Writer（io.TeeReader 的写端）。
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
