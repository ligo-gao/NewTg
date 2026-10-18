// Package usage 实现上游用量事实的三源提取与 OpenAI include_usage 请求注入。
//
// 依据：
//   - relay-03 §2.3（include_usage 语义）、§6.1–§6.2（逐协议用量提取/归一化）、
//     §11（UsageFact / UsageSource 赋值）
//   - billing-00 §7.1–§7.2（配额与用量一律整数 int64；金额链路禁止 float64）
//   - contract §2.3（Usage / UsageSource 冻结字段）
//
// 【R-05 · 三源优先级】原生 usage > include_usage 注入 > `x-model-response-usage` 头。
// 三个来源是**互斥候选**而非可相加的增量：首个非空来源胜出后立即返回，绝不再叠加
// 其它来源的数字——同一份 token 被两个来源各记一次会把费用算成 2～3 倍。
//
// 【R-02 · 职责边界】include_usage 的注入只在本包实现一次：非流式（stream=false）
// 不向上游注入顶层 `usage`（R-P14），流式只写 `stream_options.include_usage`。
//
// 【GC-9】所有配额/用量字段保持 int64；本包不做任何浮点金额运算，负值一律钳 0。
// 兜底估算（relay-03 §7 的 tiktoken / 字符权重状态机）不在这里实现：本包只把
// 「无上游来源」标记为 UsageEstimated，估算由调用方 / M3 计费侧完成。
package usage

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/ir"
)

// headerModelResponseUsage 是上游以响应头附带用量的非标准约定。
// 它不是 OpenAI 协议的一部分，只有部分兼容网关会返回；因此关系布尔必须
// 保守默认 false（见 headerUsage 注释）。
const headerModelResponseUsage = "x-model-response-usage"

// ExtractUsage 从三个来源中提取一份用量事实（三源优先级见包注释 R-05）。
//
// 生产调用方：relay.Route（非流式路径，在 ParseResponse 之后以 response.Raw +
// 上游响应头调用）。流式路径的跨块累计由端点层 bridgeStream 复用同一套
// ir.ParseOpenAIUsage 语义（Task 7/8）；本包因此不是「仅测试使用」的死代码。
//
// 参数：
//   - resp：上游非流式响应；本函数从 resp.Raw 的 `usage` 对象读取原生用量。
//     resp 为 nil 或 Raw 为空/不含有效 usage 时视为「原生来源缺席」，不 panic。
//   - headers：上游响应头；读取 `x-model-response-usage` 的非标准用量。
//   - injected：调用方合成并传入的用量（流式 include_usage 末块 / 跨块累计结果）。
//     仅当非 nil 且归一化后非零时才算一个来源。
//
// 返回的 UsageFact 只填 Model 与 Usage；ImageCount / LatencyMS / FirstTokenMS /
// Stream / Attempts 由调用方（流式/计费接缝）填写，这里保持零值（FirstTokenMS
// 为 nil 表示非流式，契约 §6.1）。
//
// 三个来源都缺席时返回零值 Usage 且 Source=UsageEstimated；按 relay-03 §7 的
// 兜底估算（tiktoken / 字符权重）是调用方 / M3 的职责，本包不实现。
func ExtractUsage(resp *contract.Response, headers http.Header, injected *contract.Usage) *contract.UsageFact {
	fact := &contract.UsageFact{}
	if resp != nil {
		fact.Model = resp.Model
		// 优先级 1：原生 usage（非流式整包 / 适配器从流中归一的末块）。
		if native := nativeUsage(resp); native != nil {
			fact.Usage = *native
			return fact
		}
	}

	// 优先级 2：调用方注入的合成用量（include_usage 路径）。
	// GC-9：即便来源在内部，负值也一律钳 0，避免负乘数进入计费。
	if injected != nil {
		clamped := clampUsage(*injected)
		if !usageIsZero(clamped) {
			fact.Usage = clamped
			return fact
		}
	}

	// 优先级 3：`x-model-response-usage` 头（上游报告的非标准用量）。
	if fromHeader, ok := headerUsage(headers); ok {
		fact.Usage = fromHeader
		return fact
	}

	// 无任何来源：零值 + estimated，提示调用方走 relay-03 §7 兜底估算。
	// 注意不能沿用零值 Usage 的默认 Source（iota 0 = upstream_exact）——
	// 那会把「没有用量」谎报成「上游精确用量」。
	fact.Usage = contract.Usage{Source: contract.UsageEstimated}
	return fact
}

// nativeUsage 从 resp.Raw 解析原生 `usage` 对象，复用 ir.ParseOpenAIUsage
// 以保证与非流式整包、流式末块走同一套字段路径（relay-03 §6.2 row 1）。
//
// 「存在且非空」判定：`usage` 键缺失、为 null、为空对象 {} 或类型非法时，
// ir.ParseOpenAIUsage 返回 UsageEstimated——此时视为无原生来源，让调用方继续
// 尝试注入/头部来源；返回 nil 而不是一份 estimated 的 Usage，正是为了不截断
// 后面的来源（R-05：首个**非空**来源胜出）。
func nativeUsage(resp *contract.Response) *contract.Usage {
	if len(resp.Raw) == 0 {
		return nil
	}
	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(resp.Raw, &payload); err != nil {
		return nil
	}
	raw := bytes.TrimSpace(payload.Usage)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	parsed := ir.ParseOpenAIUsage(raw)
	if parsed.Source == contract.UsageEstimated {
		// usage 对象为空或类型非法：没有任何上游事实，继续下一个来源。
		return nil
	}
	return &parsed
}

// headerUsagePayload 是非标准头的 JSON 形状。
// 同时接受 OpenAI 风格（prompt_tokens/completion_tokens）与 Responses 风格
// （input_tokens/output_tokens）的别名；标准字段优先。
//
// 关系布尔在头部不是协议标准的一部分，只有头部显式声明时才置 true
// （「unless the header says otherwise」）；默认 false 是保守方向：计费侧会
// 把 CacheRead/Reasoning 当独立维度，宁可少判包含关系，也不重复加价
// （contract §2.3：方向搞反会成倍错算费用）。
type headerUsagePayload struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`

	PromptIncludesCache           *bool `json:"prompt_includes_cache"`
	ReasoningIncludedInCompletion *bool `json:"reasoning_included_in_completion"`
}

// headerUsage 解析 `x-model-response-usage` 头。
//
// 用量数字用 *int64 解码：encoding/json 会拒绝小数/字符串，整个头部按非法处理，
// 从而不会把浮点 token 数带进计费（GC-9 / billing-00 §7.1–§7.2）。所有维度钳 ≥0。
// Source 置 UsageUpstreamExact：数字来自上游报告（虽非标准协议，仍不是本地估算）。
func headerUsage(headers http.Header) (contract.Usage, bool) {
	value := lookupHeader(headers, headerModelResponseUsage)
	if strings.TrimSpace(value) == "" {
		return contract.Usage{}, false
	}
	var payload headerUsagePayload
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return contract.Usage{}, false
	}

	prompt, promptOK := firstInt(payload.PromptTokens, payload.InputTokens)
	completion, completionOK := firstInt(payload.CompletionTokens, payload.OutputTokens)
	if !promptOK && !completionOK && payload.TotalTokens == nil {
		// 没有任何可识别的数字字段：不是一份用量来源，继续走兜底。
		return contract.Usage{}, false
	}

	usage := contract.Usage{
		Prompt:                        nonNeg(prompt),
		Completion:                    nonNeg(completion),
		Source:                        contract.UsageUpstreamExact,
		PromptIncludesCache:           boolValue(payload.PromptIncludesCache),
		ReasoningIncludedInCompletion: boolValue(payload.ReasoningIncludedInCompletion),
	}
	if payload.TotalTokens != nil {
		usage.Total = nonNeg(*payload.TotalTokens)
	} else if usage.Prompt+usage.Completion > 0 {
		// total_tokens 缺席时按 Prompt+Completion 派生，与 ir.ParseOpenAIUsage 一致。
		usage.Total = usage.Prompt + usage.Completion
	}
	// 原始头保留在 Raw 供对账与审计（relay-03 §11.3）。该 map 只用于审计，
	// 不是计费数学的输入；计费只读上面的 int64 字段（GC-9）。
	var raw map[string]any
	if json.Unmarshal([]byte(value), &raw) == nil {
		usage.Raw = raw
	}
	return usage, true
}

// lookupHeader 先走 Header.Get（标准 HTTP 头在服务端已被规范化），
// 再按大小写不敏感扫描——兼容测试或中间件直接构造的非规范 map。
func lookupHeader(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if len(values) > 0 && strings.EqualFold(key, name) {
			return values[0]
		}
	}
	return ""
}

// InjectIncludeUsage 返回请求的深拷贝，并按流式语义注入 include_usage。
//
// R-P14 / relay-03 §2.3（「不支持该选项或不流式时清空」）：
//   - stream=false：**不**注入顶层 `usage`（计划草稿的「非流式注入 usage」按
//     响应侧合成理解）。真实 OpenAI 会以未知参数 400 拒绝顶层 usage，因此这里
//     原样返回等价副本，并文档化该 no-op。
//   - stream=true 且 withUsage=true：把 `stream_options.include_usage` 合并为 true，
//     保留 stream_options 的其它键与 Raw 中所有未建模字段。
//   - stream=true 且 withUsage=false：原样返回副本——客户端显式关闭 include_usage
//     的选择不得被擦除（客户端的显式选择优先于默认值）。
//
// 入参永不被修改：messages / tools / raw / 所有指针与 map 都做深拷贝（R-02：
// 注入是单一职责，不得产生调用方可观察的副作用）。
//
// Raw 为空或不是 JSON 对象时无从合并，返回未改动的副本——正常流程中
// ir.ParseOpenAIRequest 已把完整请求体写入 Raw（归一化层的职责）。
func InjectIncludeUsage(req *contract.Request, withUsage bool) *contract.Request {
	if req == nil {
		return nil
	}
	out := cloneRequest(req)
	if !out.Stream || !withUsage {
		return out
	}
	if len(bytes.TrimSpace(out.Raw)) == 0 {
		return out
	}
	merged, ok := mergeIncludeUsage(out.Raw)
	if !ok {
		return out
	}
	out.Raw = merged
	return out
}

// mergeIncludeUsage 在 Raw JSON 对象里合并 stream_options.include_usage=true。
// 只改这一条路径，其余顶层字段与 stream_options 的其它键逐字保留。
func mergeIncludeUsage(raw contract.ProtocolBody) (contract.ProtocolBody, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		// 非 JSON 对象（数组/标量/invalid/null）：不猜测、不覆盖。
		return nil, false
	}
	options, ok := obj["stream_options"].(map[string]any)
	if !ok {
		// 缺失、null 或形状非法：替换为对象，保证 include_usage 能落位。
		options = map[string]any{}
		obj["stream_options"] = options
	}
	options["include_usage"] = true
	encoded, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// clampUsage 把注入来源的所有数值维度钳 ≥0（GC-9）。
// 只做安全前置动作，不做 max(cache_write, cached_creation) 之类的计费归一——
// 后者属于计费引擎（relay-03 §11.4）。
func clampUsage(u contract.Usage) contract.Usage {
	u.Prompt = nonNeg(u.Prompt)
	u.Completion = nonNeg(u.Completion)
	u.Total = nonNeg(u.Total)
	u.CacheRead = nonNeg(u.CacheRead)
	u.CacheWrite = nonNeg(u.CacheWrite)
	u.Reasoning = nonNeg(u.Reasoning)
	u.AudioSeconds = nonNeg(u.AudioSeconds)
	u.Reserved = nonNeg(u.Reserved)
	u.PromptDetail = clampUsageDetail(u.PromptDetail)
	u.CompletionDetail = clampUsageDetail(u.CompletionDetail)
	return u
}

func clampUsageDetail(d contract.UsageDetail) contract.UsageDetail {
	d.Image = nonNeg(d.Image)
	d.Audio = nonNeg(d.Audio)
	d.Text = nonNeg(d.Text)
	d.CacheRead = nonNeg(d.CacheRead)
	d.CacheWrite = nonNeg(d.CacheWrite)
	d.CacheReadImage = nonNeg(d.CacheReadImage)
	return d
}

// usageIsZero 判断一份用量是否没有任何数值事实（Raw 只是审计载荷，不算事实）。
func usageIsZero(u contract.Usage) bool {
	return u.Prompt == 0 && u.Completion == 0 && u.Total == 0 &&
		u.CacheRead == 0 && u.CacheWrite == 0 && u.Reasoning == 0 &&
		u.AudioSeconds == 0 && u.Reserved == 0 &&
		u.PromptDetail == (contract.UsageDetail{}) &&
		u.CompletionDetail == (contract.UsageDetail{})
}

func firstInt(primary, fallback *int64) (int64, bool) {
	if primary != nil {
		return *primary, true
	}
	if fallback != nil {
		return *fallback, true
	}
	return 0, false
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func nonNeg(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// ============================================================================
// 请求深拷贝
//
// ir 包的 cloneRequest 未导出，且本包不应为「拷一份请求」反向依赖更多内部实现
// （R-02：include_usage 注入是单一职责）。此处按 contract.Request 的冻结字段
// 逐层复制：任何可变结构（切片/map/指针/字节）都必须独立，否则调用方修改返回值
// 会污染仍要发往其它渠道的原始请求。
// ============================================================================

func cloneRequest(in *contract.Request) *contract.Request {
	out := *in
	out.Raw = append(contract.ProtocolBody(nil), in.Raw...)
	out.Stop = append([]string(nil), in.Stop...)
	out.Messages = make([]contract.Message, len(in.Messages))
	for i, message := range in.Messages {
		out.Messages[i] = cloneMessage(message)
	}
	out.Tools = make([]contract.Tool, len(in.Tools))
	for i, tool := range in.Tools {
		out.Tools[i] = contract.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      cloneMap(tool.Schema),
		}
	}
	if in.ResponseFormat != nil {
		format := *in.ResponseFormat
		format.Schema = cloneMap(in.ResponseFormat.Schema)
		out.ResponseFormat = &format
	}
	out.MaxTokens = cloneInt(in.MaxTokens)
	out.Temperature = cloneFloat(in.Temperature)
	out.TopP = cloneFloat(in.TopP)
	out.Seed = cloneInt(in.Seed)
	return &out
}

func cloneMessage(in contract.Message) contract.Message {
	out := in
	out.Content = make([]contract.ContentPart, len(in.Content))
	for i, part := range in.Content {
		out.Content[i] = contract.ContentPart{
			Kind:    part.Kind,
			Text:    part.Text,
			Payload: append([]byte(nil), part.Payload...),
			URL:     part.URL,
			Meta:    cloneMap(part.Meta),
		}
	}
	out.Extensions = cloneMap(in.Extensions)
	return out
}

// cloneMap 深拷贝 map[string]any（含嵌套 map/切片）；标量不可变，直接复用。
func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = cloneAny(value)
	}
	return out
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneAny(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	case []byte:
		return append([]byte(nil), typed...)
	default:
		return value
	}
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneFloat(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
