package tasks

// 本文件是 Responses 原生协议面的**渲染层**（R-W18-07 / W18-C）。
//
// 设计边界（冻结）：
//   - 只做渲染与钩子执行，不引入 Responses IR 建模（input_text /
//     function_call_output 等仍是 raw items）。工具调用本波按原样透传，
//     不臆造未建模语义；
//   - 主机拥有 Responses 对象的身份字段（id / object / created_at / model /
//     status），插件只提供 output 与 usage。这样同一任务无论由哪个版本/
//     哪个插件渲染，客户端看到的身份都是稳定的；
//   - 非终态（异步语义保留）时按裁决输出 status=in_progress/queued +
//     output=[]，检索语义由 GET /v1/responses/{id} 承担；
//   - SSE 事件顺序不可乱：response.created 必须第一、终止事件必须最后；
//     插件只负责 delta 等中间事件，终止事件由宿主补写，保证 usage 一定出现在
//     response.completed 上（R-W18-07 的最小事件集）。

import (
	"context"
	"errors"
	"fmt"

	"newtg/gateway/internal/plugin"
)

// ErrResponsesRender 表示插件渲染钩子返回了非法形状。
//
// 与 ErrPluginHook 分开：钩子未导出/执行异常是插件不可用；渲染返回非对象
// 是**协议形状违约**，两者在 HTTP 面都映射为内部错误，但排查方向不同。
var ErrResponsesRender = errors.New("tasks: invalid responses render result")

// Responses 协议的事件与对象字面量（relay-07 §1.3/§1.5）。
//
// 本波只保证最小事件集；其它 31 事件中的未知类型经 raw 透传语义原样发出，
// 不做映射（不引入 IR 事件模型）。
const (
	ResponsesObjectType       = "response"
	ResponsesStatusCompleted  = "completed"
	ResponsesStatusFailed     = "failed"
	ResponsesStatusInProgress = "in_progress"
	ResponsesStatusQueued     = "queued"

	ResponsesEventCreated         = "response.created"
	ResponsesEventInProgress      = "response.in_progress"
	ResponsesEventOutputTextDelta = "response.output_text.delta"
	ResponsesEventCompleted       = "response.completed"
	ResponsesEventFailed          = "response.failed"
)

// ResponsesRenderInput 是一次 Responses 渲染所需的宿主事实。
type ResponsesRenderInput struct {
	Plugin     *plugin.CompiledPlugin
	ResponseID string
	Model      string
	CreatedAt  int64
	// Status 是宿主映射后的 Responses status 字面量（completed/failed/
	// in_progress/queued/incomplete）。插件不能覆盖它：状态来自任务行，
	// 不是插件的自述。
	Status string
	State  State
	// Usage 已由宿主从 task.data 归一化；renderFinal 返回的 usage 优先。
	Usage map[string]any
	// Request 是客户端原始 Responses 请求体（decodeRequest 的同一个入参）。
	Request map[string]any
	// Data 是 parseSubmitResponse 的 taskData（产物的唯一来源）。
	Data map[string]any
}

// Terminal 报告本次渲染是否处于终态。
func (in ResponsesRenderInput) Terminal() bool { return in.State.Terminal() }

// RenderResponsesFinal 执行 renderFinal 并返回合法的 Responses 对象。
//
// 无 renderFinal 钩子的插件走宿主回退路径（必测项「无钩子插件回退路径」）：
// 仍返回完整 Responses 形状（id/object/created_at/status/model/output），只是
// output 为空数组。这保证历史 routes[] 插件在升级后不会拿到 task 202 形状，
// 也不需要为了能用 Responses 端点而补钩子。
func RenderResponsesFinal(ctx context.Context, in ResponsesRenderInput) (map[string]any, error) {
	out := responsesObject(in, in.Status)
	if in.Plugin == nil || !in.Plugin.HasHook("renderFinal") {
		return out, nil
	}
	raw, err := in.Plugin.Call(ctx, "renderFinal", responsesRenderContext(in))
	if err != nil {
		return nil, fmt.Errorf("%w: renderFinal: %v", ErrResponsesRender, err)
	}
	rendered, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: renderFinal must return an object", ErrResponsesRender)
	}
	// 只接受协议字段：id/object/created_at/status/model 由宿主写死，
	// 插件即使返回也不生效；其它未知字段不得静默塞进 Responses 对象
	//（R-W18-08：扩展字段必须显式命名，不能混入标准形状）。
	//
	// output 只接受成功终态：非终态按裁决为 []（异步语义由检索面承担），
	// 失败终态也不把中间产物伪装成成功结果。
	if output, exists := rendered["output"]; exists && in.State == StateSuccess {
		items, ok := output.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: renderFinal output must be an array", ErrResponsesRender)
		}
		out["output"] = items
	}
	if usage, ok := rendered["usage"].(map[string]any); ok {
		if normalized := NormalizeResponsesUsage(usage); normalized != nil {
			out["usage"] = normalized
		}
	}
	return out, nil
}

// RenderResponsesEvents 执行 renderEvents 并规范化事件序列。
//
// 顺序契约（不可乱）：
//  1. response.created 恒为第一帧，承载 status=in_progress、output=[]；
//  2. 插件事件按返回顺序排在中间，response.created/终止事件被宿主接管；
//  3. 终态补写 response.completed（含 usage）或 response.failed；非终态补写
//     response.in_progress，绝不伪造 completed（否则客户端会认为异步任务已完成）。
//
// 未知事件类型原样保留（raw 透传语义）：本条协议的事件全集有 31 个，宿主
// 没有 IR 建模，丢弃未知类型会让插件扩展事件静默消失。
func RenderResponsesEvents(ctx context.Context, in ResponsesRenderInput) ([]map[string]any, error) {
	events := []map[string]any{{
		"type":     ResponsesEventCreated,
		"response": responsesObject(in, ResponsesStatusInProgress),
	}}

	var pluginTerminal map[string]any
	if in.Plugin != nil && in.Plugin.HasHook("renderEvents") {
		raw, err := in.Plugin.Call(ctx, "renderEvents", responsesRenderContext(in))
		if err != nil {
			return nil, fmt.Errorf("%w: renderEvents: %v", ErrResponsesRender, err)
		}
		pluginEvents, err := decodeResponsesEvents(raw)
		if err != nil {
			return nil, err
		}
		for _, event := range pluginEvents {
			eventType, _ := event["type"].(string)
			switch {
			case eventType == ResponsesEventCreated:
				// 宿主已写第一帧；插件重复的 created 不得打乱顺序。
				continue
			case isTerminalResponsesEvent(eventType):
				// 终止事件必须最后，而 usage 必须由宿主按任务事实补写；
				// 先记下插件的 response 供最终对象合并，再统一重写。
				pluginTerminal = event
			default:
				events = append(events, event)
			}
		}
	}

	final, err := RenderResponsesFinal(ctx, in)
	if err != nil {
		return nil, err
	}
	// 只声明 renderEvents 的插件没有 renderFinal：它的终止事件 response 是
	// output/usage 的唯一来源，合并进宿主最终对象（身份字段仍不可覆盖）。
	if pluginTerminal != nil {
		if response, ok := pluginTerminal["response"].(map[string]any); ok {
			if in.State == StateSuccess && len(asArrayValue(final["output"])) == 0 {
				if output, ok := response["output"].([]any); ok {
					final["output"] = output
				}
			}
			if _, exists := final["usage"]; !exists {
				if usage, ok := response["usage"].(map[string]any); ok {
					if normalized := NormalizeResponsesUsage(usage); normalized != nil {
						final["usage"] = normalized
					}
				}
			}
		}
	}

	switch {
	case in.State == StateFailure:
		events = append(events, map[string]any{"type": ResponsesEventFailed, "response": final})
	case in.Terminal():
		events = append(events, map[string]any{"type": ResponsesEventCompleted, "response": final})
	default:
		// 异步语义保留：只到 in_progress 为止，不写终止事件。
		inProgress := responsesObject(in, in.Status)
		events = append(events, map[string]any{"type": ResponsesEventInProgress, "response": inProgress})
	}
	return events, nil
}

// NormalizeResponsesUsage 把插件 usage 映射为 Responses 的 usage 形状。
//
// 映射规则（relay-07 §1.4）：input_tokens ← prompt_tokens、output_tokens ←
// completion_tokens、total_tokens 缺失时由两者相加。只做字段搬运，不做结算：
// 计费仍走任务结算路径，这里显示同一份 usage，**不得**产生第二次扣费。
func NormalizeResponsesUsage(usage map[string]any) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	input, hasInput := firstNumeric(usage, "input_tokens", "prompt_tokens")
	output, hasOutput := firstNumeric(usage, "output_tokens", "completion_tokens")
	total, hasTotal := firstNumeric(usage, "total_tokens")
	if !hasTotal && (hasInput || hasOutput) {
		total, hasTotal = input+output, true
	}
	out := make(map[string]any, 3)
	if hasInput {
		out["input_tokens"] = input
	}
	if hasOutput {
		out["output_tokens"] = output
	}
	if hasTotal {
		out["total_tokens"] = total
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ResponsesUsageFromData 从 task.data 的 usage 投影 Responses usage。
//
// 插件 usage schema 的单位是计费口径（seconds/token/count…），只有含
// input/output token 字段时才能映射；其它用量（例如视频秒数）不冒充 token
// 用量——那会让客户端按错误的含义展示成本。
func ResponsesUsageFromData(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	usage, ok := data["usage"].(map[string]any)
	if !ok {
		return nil
	}
	return NormalizeResponsesUsage(usage)
}

// responsesObject 构造宿主权威的 Responses 对象。
//
// output 恒为数组：非终态按裁决为空；终态由 renderFinal 填充。
func responsesObject(in ResponsesRenderInput, status string) map[string]any {
	if status == "" {
		status = in.Status
	}
	if status == "" {
		status = ResponsesStatusQueued
	}
	output := []any{}
	out := map[string]any{
		"id":         in.ResponseID,
		"object":     ResponsesObjectType,
		"created_at": in.CreatedAt,
		"status":     status,
		"output":     output,
	}
	if in.Model != "" {
		out["model"] = in.Model
	}
	if len(in.Usage) > 0 {
		out["usage"] = in.Usage
	}
	return out
}

// responsesRenderContext 组装传给 renderFinal / renderEvents 的只读上下文。
//
// ctx.body 与 decodeRequest 的入参同源（客户端原始请求），ctx.taskData 是
// parseSubmitResponse 的结果。插件只应读取，不返回身份字段。
func responsesRenderContext(in ResponsesRenderInput) map[string]any {
	ctx := map[string]any{
		"model":      in.Model,
		"responseId": in.ResponseID,
		"status":     in.Status,
		"createdAt":  in.CreatedAt,
		"state":      string(in.State),
		"body":       in.Request,
		"request":    in.Request,
		"taskData":   in.Data,
	}
	if len(in.Usage) > 0 {
		ctx["usage"] = in.Usage
	}
	return ctx
}

// decodeResponsesEvents 接受两种插件返回形态：
//   - 事件数组 [ {type, ...}, ... ]；
//   - 包装对象 { events: [...] }（给未来扩展留位，但仍只读取 events）。
func decodeResponsesEvents(raw any) ([]map[string]any, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		object, isObject := raw.(map[string]any)
		if !isObject {
			return nil, fmt.Errorf("%w: renderEvents must return an array or {events: [...]}", ErrResponsesRender)
		}
		items, ok = object["events"].([]any)
		if !ok {
			return nil, fmt.Errorf("%w: renderEvents.events must be an array", ErrResponsesRender)
		}
	}
	out := make([]map[string]any, 0, len(items))
	for idx, item := range items {
		event, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: renderEvents[%d] must be an object", ErrResponsesRender, idx)
		}
		if eventType, _ := event["type"].(string); eventType == "" {
			return nil, fmt.Errorf("%w: renderEvents[%d].type is required", ErrResponsesRender, idx)
		}
		out = append(out, event)
	}
	return out, nil
}

func isTerminalResponsesEvent(eventType string) bool {
	switch eventType {
	case ResponsesEventCompleted, ResponsesEventFailed,
		"response.done", "response.incomplete", "response.error",
		"response.cancelled", "response.canceled":
		return true
	}
	return false
}

func firstNumeric(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := numericValue(values[key]); ok {
			return value, true
		}
	}
	return 0, false
}

// numericValue 只接受 JSON 解码会产生的数值类型；字符串不隐式转数字
// （"5" 与 5 在协议上不是同一件事，静默转换会把上游错误伪装成合法 usage）。
func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	}
	return 0, false
}

func asArrayValue(value any) []any {
	if array, ok := value.([]any); ok {
		return array
	}
	return nil
}
