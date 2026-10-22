package httpapi

// Responses 原生协议面的 HTTP 执行段（R-W18-07 / W18-C）。
//
// 与既有 /v1/tasks 入口的分工：
//   - 提交管线（鉴权 → 插件准入 → 渠道选择 → 预扣 → 出站 → 落库 → 结算）
//     仍然只有 handleSubmit 一份实现（P-01：计费只发生一次）；
//   - 本文件只在**响应出口**替换形状：create 返回 Responses 对象或 SSE，
//     不再返回 202 task 形状；终止事件的 usage 是同一份任务事实的投影，
//     不产生第二次结算。
//
// 为什么用请求上下文里的 mode 而不是复制 handleSubmit：复制会让两条提交
// 路径在计费/退款/幂等语义上分叉——这正是 P-01 与「不得双计」要禁止的。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"newtg/gateway/internal/plugin"
	"newtg/gateway/internal/tasks"
)

// responsesSubmitKey 是「本次提交走 Responses 出口」的请求作用域标记。
type responsesSubmitKey struct{}

// responsesSubmitMode 携带 Responses create 的渲染输入。
type responsesSubmitMode struct {
	PluginKey string
	// Request 是客户端原始 Responses 请求体（decodeRequest 的同一个入参）。
	Request map[string]any
	Stream  bool
}

// responsesSubmitModeFrom 返回请求上的 Responses 模式（无则 nil）。
func responsesSubmitModeFrom(r *http.Request) *responsesSubmitMode {
	if r == nil {
		return nil
	}
	mode, _ := r.Context().Value(responsesSubmitKey{}).(*responsesSubmitMode)
	return mode
}

// handleResponsesCreate 是 `POST /v1/responses` 的执行入口。
//
// 触发方式有两种，都收敛到本函数：
//   - protocols.openai_responses.supports 含 create（R-W18-07 的协议派生绑定）；
//   - routes[] 显式声明 POST /v1/responses（历史兼容，routes[] 仍可用）。
//
// 请求体在这里读一次并归档到 mode，然后复原 body 交给 handleSubmit：
// 后者负责全部校验/计费/落库，错误也由它按 /v1 的 OpenAI 信封输出。
func (h *TasksHandler) handleResponsesCreate(w http.ResponseWriter, r *http.Request, match plugin.RouteMatch) {
	requestID := newRelayRequestID()
	key := strings.TrimSpace(match.PluginKey)
	if key == "" {
		writeRelayError(w, http.StatusNotFound, requestID, "invalid_request_error", "unknown task plugin")
		return
	}

	// 有界读取：与 /v1/tasks 同一个上限，拒绝超限体而不是让后续 JSON 解码
	// 把内存打满。超限是参数错误（413），不是 task 形状。
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTaskSubmitBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeRelayError(w, http.StatusRequestEntityTooLarge, requestID, "payload_too_large",
				"request body is too large")
			return
		}
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"failed to read request body")
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		// 参数错误一律 400 OpenAI 信封；不得泄漏解码器原文（可能回显输入片段）。
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"request body must be a JSON object")
		return
	}
	// M2（W18-C 复核）：宿主必须自己校验协议字段类型。此前 `stream` 非 bool
	// 会被静默当成 false，`input`/`tools` 的错误形状则一路交给插件——最终以
	// 502（上游/插件内部错误）出现，把客户端参数问题误报成服务故障。
	stream, ok := responsesStreamFlag(body)
	if !ok {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error",
			"stream must be a boolean")
		return
	}
	if message := validateResponsesRequestShape(body); message != "" {
		writeRelayError(w, http.StatusBadRequest, requestID, "invalid_request_error", message)
		return
	}

	// 复原 body 供 handleSubmit 重新解码（它自己也会做有界读取与 model 校验）。
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.SetPathValue("key", key)
	mode := &responsesSubmitMode{PluginKey: key, Request: body, Stream: stream}
	r = r.WithContext(context.WithValue(r.Context(), responsesSubmitKey{}, mode))
	h.handleSubmit(w, r)
}

// writeResponsesSubmit 是提交成功后的 Responses 出口。
//
// 非流式：200 + Responses 对象；流式：200 text/event-stream + 规范化事件序列。
// 渲染失败发生在任务已落库/已结算之后，因此返回内部错误而不是回滚资金：
// 资金事实已经存在，回滚反而会让账实不符；客户端可用 GET 检索同一任务。
func (h *TasksHandler) writeResponsesSubmit(w http.ResponseWriter, r *http.Request,
	requestID string, task *tasks.Task, compiled *plugin.CompiledPlugin, mode *responsesSubmitMode) {
	in := tasks.ResponsesRenderInput{
		Plugin:     compiled,
		ResponseID: responsesIDForTask(task.PublicTaskID),
		Model:      toStringValue(task.Properties["OriginModelName"]),
		CreatedAt:  responsesCreatedAt(task),
		Status:     responsesStatusForState(task.State),
		State:      task.State,
		Usage:      tasks.ResponsesUsageFromData(task.Data),
		Request:    mode.Request,
		Data:       task.Data,
	}
	if mode.Stream {
		events, err := tasks.RenderResponsesEvents(r.Context(), in)
		if err != nil {
			// 复核 Low：渲染失败必须留痕，但不得把插件错误/上游文本回给客户端。
			slog.Error("tasks: render responses stream failed",
				"request_id", requestID, "plugin", compiled.Key, "error", err)
			writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
				"failed to render the response stream")
			return
		}
		writeResponsesSSE(w, events)
		return
	}
	object, err := tasks.RenderResponsesFinal(r.Context(), in)
	if err != nil {
		slog.Error("tasks: render responses object failed",
			"request_id", requestID, "plugin", compiled.Key, "error", err)
		writeRelayError(w, http.StatusInternalServerError, requestID, "internal_error",
			"failed to render the response")
		return
	}
	writeJSON(w, http.StatusOK, object)
}

// writeResponsesSSE 把规范化事件写成 SSE 帧。
//
// 帧格式 `event: <type>\ndata: <json>\n\n` 与 OpenAI Responses 一致；
// 不追加 `[DONE]`（那是 chat/completions 的约定，Responses 以终止事件收尾）。
func writeResponsesSSE(w http.ResponseWriter, events []map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		eventType, _ := event["type"].(string)
		payload, err := json.Marshal(event)
		if err != nil {
			// 事件由宿主构造/校验，序列化失败属于内部缺陷；跳过该帧比中断
			// 整条流更安全（客户端至少能收到终止事件）。
			continue
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// responsesIDForTask 把 task PublicTaskID 映射为 Responses id（纯前缀替换）。
//
// 与 resolvetaskFromResponseID 同一口径：resp_ 与 task_ 后的随机量一一对应，
// 不做哈希/转义，检索面才能反解。
func responsesIDForTask(publicTaskID string) string {
	return responsesIDPrefix + strings.TrimPrefix(publicTaskID, tasks.PublicTaskIDPrefix)
}

// responsesStreamFlag 解析 stream 字段：缺省/null = false；其它非 bool 值
// 视为参数错误（ok=false），不再静默降级。
func responsesStreamFlag(body map[string]any) (stream bool, ok bool) {
	value, exists := body["stream"]
	if !exists || value == nil {
		return false, true
	}
	typed, isBool := value.(bool)
	if !isBool {
		return false, false
	}
	return typed, true
}

// validateResponsesRequestShape 校验本波支持的核心字段形状（M2）。
//
// 只校验宿主需要用到的结构，不做完整 Responses schema 建模（本波不引入 IR）：
//   - input：字符串或 item 数组；
//   - tools：数组；
//   - instructions：字符串。
//
// 返回固定文案（不拼接用户输入），空串表示通过。
func validateResponsesRequestShape(body map[string]any) string {
	if value, exists := body["input"]; exists && value != nil {
		switch value.(type) {
		case string, []any:
		default:
			return "input must be a string or an array of items"
		}
	}
	if value, exists := body["tools"]; exists && value != nil {
		if _, ok := value.([]any); !ok {
			return "tools must be an array"
		}
	}
	if value, exists := body["instructions"]; exists && value != nil {
		if _, ok := value.(string); !ok {
			return "instructions must be a string"
		}
	}
	return ""
}

// isResponsesCreateRoute 判定命中的路由是否应走 Responses 创建出口。
//
// 协议派生路由带 Kind=responses；routes[] 直接声明 POST /v1/responses 的
// 历史形态（kind=submit）也必须在同一出口渲染，否则同一端点会因声明方式
// 不同而返回 task 202，客户端无法依赖协议形状。
func isResponsesCreateRoute(method, path, kind string) bool {
	if kind == plugin.RouteKindResponses {
		return true
	}
	return strings.EqualFold(method, http.MethodPost) &&
		strings.TrimSuffix(path, "/") == "/v1/responses"
}
