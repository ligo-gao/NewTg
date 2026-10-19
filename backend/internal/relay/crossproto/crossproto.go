// Package crossproto 实现 M4 Task 5 的跨协议转发适配面。
//
// 【设计缺口 R-08】跨协议转发的机制在权威设计文档中没有独立出处：
// `x-newtg-protocol` 头与 `relay.cross_protocol_forward_enabled` 配置键
// 只由 M4 计划冻结（GC-8 / R-03）。本包按计划实现，并把「目标协议 = 上游
// 渠道协议」这一语义解释登记在任务报告里，等待设计文档回填。
//
// 语义（GC-2：只经 IR 中转）：
//
//	客户端协议（固定 OpenAI 兼容，入口 /v1/chat/completions）
//	  → IR（ir.ParseOpenAIRequest）
//	  → 目标上游协议（Anthropic / Gemini / OpenAI，由适配器的 BuildRequest 编码）
//	  → IR（适配器的 ParseResponse 解析原生响应）
//	  → 客户端协议（本包 Forward 用 ir.FromIR 回写 OpenAI 形状）
//
// 为什么不在这里直接拼协议字段：契约 §2.1 把 IR 定为唯一归一化面，任何在
// handler 或本包里逐字段拼接 Anthropic/Gemini JSON 的做法都会绕过 IR，
// 让计费/日志看到两套口径（GC-2）。请求方向复用 T2 的适配器（IR → 原生），
// 响应方向复用 ir.FromIR（IR → OpenAI），本包只做选择、校验与回写。
package crossproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/relay/adapters"
	"newtg/gateway/internal/relay/ir"
)

const (
	// HeaderProtocol 是客户端声明目标上游协议的请求头（M4 计划冻结，R-08）。
	// 头值大小写不敏感，允许首尾空白；空值等价于未携带。
	HeaderProtocol = "x-newtg-protocol"

	// ConfigKeyEnabled 是跨协议转发总开关（configcenter 键，M4 计划冻结，R-03）。
	// 默认 false：GC-8 要求显式启用，关闭时必须拒绝携带协议头的请求，
	// 绝不能静默按 OpenAI 处理——那会把「客户端以为走了 Anthropic」变成
	// 一次语义不同的成功响应。
	ConfigKeyEnabled = "relay.cross_protocol_forward_enabled"
)

// Protocol 是可作为上游目标的三类协议族。
//
// 只承诺三类键（openai/anthropic/gemini）：它们是当前已落地适配器且
// IR 映射闭合的协议族。其他渠道协议（baidu/zhipu/vertex 等）尚无适配器
// 实现，按「上游不支持 → 显式错误」处理，不做猜测性映射。
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGemini    Protocol = "gemini"
)

var (
	// ErrUnknownProtocol 表示协议头的值不在 openai|anthropic|gemini 内。
	// 端点层映射 400（固定口径，不泄露可用值之外的内部信息）。
	ErrUnknownProtocol = errors.New("crossproto: unknown target protocol")

	// ErrUpstreamProtocolUnsupported 表示候选渠道里没有任何渠道的协议族
	// 与目标协议匹配。端点层按「上游不支持」显式拒绝（400），不得退化为
	// 「随便挑一个渠道按 OpenAI 处理」（Review Focus 5）。
	ErrUpstreamProtocolUnsupported = errors.New("crossproto: no channel supports the requested protocol")

	// ErrNilResponse 表示调用方把 nil 响应交给 Forward。属于程序错误，
	// 但绝不能在 nil 上 panic 后把进程拖垮（端点层映射 502）。
	ErrNilResponse = errors.New("crossproto: nil response")
)

// ParseTargetProtocol 把协议头值解析为 Protocol（纯函数）。
//
// 空值返回 ErrUnknownProtocol：调用方应先用 TargetFromHeader 区分
// 「未携带头」（不启用跨协议分支）与「携带了非法值」（400）。
func ParseTargetProtocol(value string) (Protocol, error) {
	switch Protocol(strings.ToLower(strings.TrimSpace(value))) {
	case ProtocolOpenAI:
		return ProtocolOpenAI, nil
	case ProtocolAnthropic:
		return ProtocolAnthropic, nil
	case ProtocolGemini:
		return ProtocolGemini, nil
	default:
		return "", ErrUnknownProtocol
	}
}

// TargetFromHeader 解析协议头，返回 (协议, 是否携带头, 错误)。
//
// 返回三个值而不是两个，是为了让端点层能锁定 GC-8 的顺序语义：
// 先判「开关关闭 + 头存在 → 403」，再判「头的值非法 → 400」。
// 如果先校验取值，关闭状态下的非法头会得到 400，泄露「该值只是拼错」
// 而非「功能未启用」的差异；反过来固定 403 也让关闭状态下所有携带头的
// 请求走同一条拒绝路径（不得静默降级）。
func TargetFromHeader(value string) (Protocol, bool, error) {
	if strings.TrimSpace(value) == "" {
		return "", false, nil
	}
	protocol, err := ParseTargetProtocol(value)
	return protocol, true, err
}

// protocolByChannelType 是渠道类型 → 目标协议族的静态映射。
//
// 数据源是 adapters.DispatchTable()（契约 §3.3 的唯一分派真源），而不是
// 本包另抄一份渠道类型列表：两处各写一份会在新增原生渠道时静默漂移。
// 只看 AdapterKey，不看 FallbackTo —— Azure 等回退到 AKOpenAI 的类型本就
// 属于 OpenAI 协议族；无实现的键（vertex/baidu/...）不进入映射，因此它们
// 不会被误判为三类协议中的任何一类。
var protocolByChannelType = buildProtocolByChannelType()

func buildProtocolByChannelType() map[contract.ChannelType]Protocol {
	table := adapters.DispatchTable()
	out := make(map[contract.ChannelType]Protocol, len(table))
	for channelType, entry := range table {
		switch adapters.AdapterKey(entry.AdapterKey) {
		case adapters.AKOpenAI:
			out[channelType] = ProtocolOpenAI
		case adapters.AKAnthropicNative:
			out[channelType] = ProtocolAnthropic
		case adapters.AKGeminiNative:
			out[channelType] = ProtocolGemini
		}
	}
	return out
}

// ProtocolOfChannelType 返回渠道类型所属的目标协议族。
//
// ok=false 表示该类型没有已实现的 openai/anthropic/gemini 适配器（例如
// vertex、baidu、task 平台）。调用方不得把 ok=false 猜成 OpenAI：跨协议
// 选择必须显式失败（ErrUpstreamProtocolUnsupported）。
func ProtocolOfChannelType(channelType contract.ChannelType) (Protocol, bool) {
	protocol, ok := protocolByChannelType[channelType]
	return protocol, ok
}

// ChannelTypeSupportsProtocol 判定渠道类型是否属于目标协议族。
func ChannelTypeSupportsProtocol(channelType contract.ChannelType, target Protocol) bool {
	protocol, ok := ProtocolOfChannelType(channelType)
	return ok && protocol == target
}

// Forward 把上游协议返回的 IR 响应回写为客户端协议（OpenAI 兼容）响应。
//
// 【与计划签名的差异（R-06）】计划草稿的签名是
// `Forward(ctx, rc, targetProtocol) (*contract.Response, error)`，把「待转换的
// 响应」隐含在 RelayContext 里；但契约 §5 的 RelayContext 没有响应字段（已冻结），
// 上游响应只存在于 relay.Route 的返回值中。为了不在冻结契约里硬塞字段，
// 这里显式增加 resp 形参——与 R-06「计划接口为草图、实际契约为准」的裁决一致。
//
// targetProtocol 是**上游**目标协议（协议头的值），不是客户端协议：客户端
// 入口固定 OpenAI 兼容（M4 无独立 Anthropic/Gemini 客户端端点）。
//   - openai：上游响应已是客户端形状，原样返回（保持 M2 的逐字节透传）；
//   - anthropic/gemini：经 ir.FromIR 从 IR 重建 OpenAI Chat Completions JSON。
//
// GC-6：只读取 IR 的结构字段；rc 里的凭据/APIBase 绝不进入输出。
func Forward(ctx context.Context, rc *contract.RelayContext, targetProtocol string, resp *contract.Response) (*contract.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	protocol, err := ParseTargetProtocol(targetProtocol)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, ErrNilResponse
	}
	if protocol == ProtocolOpenAI {
		// 客户端与上游同为 OpenAI：不重编码，避免破坏 M2 的透传语义
		// （include_usage 末块之外的原始字节、供应商扩展字段都要保留）。
		return resp, nil
	}

	// F1：先剥离原生扩展，再**清空 Raw** 交给 ir.FromIR。
	// FromIR 会把原始响应里的 created/system_fingerprint/usage/service_tier
	// 原样复制进 OpenAI 负载；对原生上游来说 `usage` 是 input_tokens /
	// output_tokens（Gemini 甚至是 usageMetadata），直接复制会把原生 usage
	// 形状泄漏给 OpenAI 客户端（缺 prompt_tokens/completion_tokens/total_tokens）。
	// 因此不让 FromIR 看到原生 Raw，随后用归一化用量合成 OpenAI 形状的 usage。
	sanitized := sanitizeForOpenAIClient(resp)
	sanitized.Raw = nil
	encoded, err := ir.FromIR(sanitized)
	if err != nil {
		return nil, fmt.Errorf("crossproto: encode OpenAI response: %w", err)
	}
	if usage, ok := openAIUsageFromNative(protocol, resp.Raw); ok {
		encoded.Raw = withOpenAIUsage(encoded.Raw, usage)
	}
	// 转换链是契约 §5 的追加式审计（relay-01 要求协议转换可追溯）。
	// 只记录阶段与协议标识，不记录响应体，避免把上游内容写进审计。
	if rc != nil {
		rc.ConversionChain = append(rc.ConversionChain, contract.ConversionStepEntry{
			Stage: "protocol_convert",
			From:  relayFormatOf(protocol),
			To:    ir.FormatOpenAIChat,
			At:    time.Now().UTC(),
		})
	}
	return encoded, nil
}

// openAIResponseExtensionAllowlist 是允许进入 OpenAI 客户端响应的 message
// Extensions 键（N3）。
//
// 为什么用 allow-list 而不是 deny-list：`unmapped_blocks`（Anthropic 未知块
// 的保真载体）只是当前已知的泄漏点，未来原生解析器可能新增更多协议私有的
// 扩展键。deny-list 会让新键默认泄漏；allow-list 让未知键默认被剥离
// （fail-closed），只有明确属于 OpenAI Chat Completions message 的字段才保留。
// `finish_reason` 不在其中：它由 mergeExtensions 跳过、由 Choice 层级写出。
var openAIResponseExtensionAllowlist = map[string]struct{}{
	"refusal":     {},
	"annotations": {},
	"audio":       {},
}

// sanitizeForOpenAIClient 返回一份剥离了非 OpenAI 协议扩展的响应副本，
// 供 ir.FromIR 编码给 OpenAI 兼容客户端（N3）。
//
// 只复制被修改的消息切片/map，原始 IR 响应不被改动：同一次请求的
// UsageFact/审计仍需要原生保真数据（对账与诊断），剥离只发生在客户端出口。
func sanitizeForOpenAIClient(resp *contract.Response) *contract.Response {
	out := *resp
	out.Choices = make([]contract.Choice, len(resp.Choices))
	for i, choice := range resp.Choices {
		choice.Message = sanitizeMessageExtensions(choice.Message)
		out.Choices[i] = choice
	}
	return &out
}

func sanitizeMessageExtensions(message contract.Message) contract.Message {
	if len(message.Extensions) == 0 {
		return message
	}
	filtered := make(map[string]any, len(message.Extensions))
	for key, value := range message.Extensions {
		if _, ok := openAIResponseExtensionAllowlist[key]; ok {
			filtered[key] = value
		}
	}
	message.Extensions = filtered
	return message
}

// openAIUsageFromNative 从原生上游响应体合成 OpenAI 形状的 usage 对象（F1）。
//
// 为什么重新解析 Raw 而不是从 Response 取：契约 §2.3 的 Response 没有 Usage
// 字段（用量由适配器 ParseResponse 的第二返回值单独给出，Forward 签名收不到），
// 而原生 Raw 里恰好保留了协议原始用量。只在 Raw 确实带用量字段时返回 ok=true：
// 上游没给 usage 时不伪造零值 usage（客户端会把零值当真）。
func openAIUsageFromNative(protocol Protocol, raw []byte) (map[string]any, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false
	}
	var usage contract.Usage
	switch protocol {
	case ProtocolAnthropic:
		var payload struct {
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			return nil, false
		}
		trimmed := bytes.TrimSpace(payload.Usage)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			return nil, false
		}
		usage = ir.ParseAnthropicUsage(payload.Usage)
	case ProtocolGemini:
		var payload struct {
			UsageMetadata json.RawMessage `json:"usageMetadata"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			return nil, false
		}
		trimmed := bytes.TrimSpace(payload.UsageMetadata)
		if len(trimmed) == 0 || string(trimmed) == "null" {
			return nil, false
		}
		usage = ir.ParseGeminiUsage(payload.UsageMetadata)
	default:
		return nil, false
	}
	// M5：解析器对「非空但没有已识别用量键」的对象返回 estimated。这类
	// 对象没有可采信的上游事实，绝不能合成一份全零 usage 让客户端当真。
	if usage.Source != contract.UsageUpstreamExact {
		return nil, false
	}

	// OpenAI 的 prompt_tokens 是**含缓存**的总输入量；Anthropic/Gemini 的
	// Prompt 不含缓存（契约 §2.3 关系布尔）。客户端看到的 prompt_tokens
	// 必须按 OpenAI 语义求和，否则订阅方会把缓存段漏算。
	prompt := usage.Prompt + usage.CacheRead + usage.CacheWrite
	completion := usage.Completion
	object := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
	}
	if usage.CacheRead > 0 {
		object["prompt_tokens_details"] = map[string]any{"cached_tokens": usage.CacheRead}
	}
	if usage.Reasoning > 0 {
		// OpenAI 的 completion_tokens 已含 reasoning_tokens（同 Anthropic 的
		// output_tokens 语义）；只补明细，不再加进 completion_tokens。
		object["completion_tokens_details"] = map[string]any{"reasoning_tokens": usage.Reasoning}
	}
	return object, true
}

// withOpenAIUsage 把合成 usage 写入已编码的 OpenAI 响应 JSON。
//
// 解析失败时保留原响应：编码结果来自 ir.FromIR，正常情况下必为 JSON 对象；
// 这里宁可少一个 usage 字段，也不能让客户端拿不到正文。
func withOpenAIUsage(raw []byte, usage map[string]any) []byte {
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return raw
	}
	object["usage"] = usage
	encoded, err := json.Marshal(object)
	if err != nil {
		return raw
	}
	return encoded
}

// RawEventDisposition 是原生 EventRaw 在跨协议流中的处置方式。
type RawEventDisposition int

const (
	// RawEventDrop：该帧只承载原生元数据（签名/引用），OpenAI 流没有对应
	// 语义通道；丢弃是**有意省略**，正文/工具/用量仍由语义事件完整输出。
	RawEventDrop RawEventDisposition = iota

	// RawEventUnsupported：该帧承载无法用 OpenAI SSE 表达的内容载荷
	// （加密思考块、未知块、Gemini inlineData 等）。调用方必须按连接级
	// 失败终止流（不写 [DONE]、退款），绝不逐字节透传或静默丢内容。
	RawEventUnsupported
)

// ClassifyNativeRawEvent 判定原生适配器交出的 EventRaw 能否在 OpenAI 客户端
// 流中安全省略（F2 / N2）。
//
// 判定依据是原生帧的协议语义，而不是「能否解析」：
//   - Anthropic：signature_delta / citations_delta / ping 是辅助元数据，
//     OpenAI 无对应字段 → Drop；content_block_start（redacted_thinking 或
//     未知块）与未知事件可能携带内容 → Unsupported。
//   - Gemini：适配器只在 inlineData / functionResponse / 未知 part 时发
//     EventRaw，全部是内容载荷 → Unsupported。
//
// fail-closed：无法解析或协议未知一律 Unsupported（宁可显式失败，也不把
// 无法理解的载荷当成「可安全丢弃」）。
func ClassifyNativeRawEvent(protocol Protocol, raw []byte) RawEventDisposition {
	data := sseDataPayload(raw)
	if len(data) == 0 {
		return RawEventUnsupported
	}
	switch protocol {
	case ProtocolAnthropic:
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil {
			return RawEventUnsupported
		}
		if event.Type == "content_block_delta" {
			switch event.Delta.Type {
			case "signature_delta", "citations_delta":
				return RawEventDrop
			}
			return RawEventUnsupported
		}
		if event.Type == "ping" {
			return RawEventDrop
		}
		return RawEventUnsupported
	default:
		return RawEventUnsupported
	}
}

// sseDataPayload 从一个原始 SSE 帧中取出 data 行并合并（多行 data: 以 \n 连接）。
//
// 原生适配器交出的 Raw 是 `sseFrame.raw`（含 event:/data:/id: 行）；若上游
// 直接给了裸 JSON（无 SSE 包装），则整体当作 data。
func sseDataPayload(raw []byte) []byte {
	var dataLines []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(dataLines) == 0 {
		return bytes.TrimSpace(raw)
	}
	return []byte(strings.Join(dataLines, "\n"))
}

// relayFormatOf 把目标协议映射为审计用的 RelayFormat 字面量。
//
// 字面量取自 ir 包的导出常量（协议标识的唯一出处）；未知值回落到
// 空串而不是 panic——Forward 已先经 ParseTargetProtocol 校验，这里只是
// 防御性兜底。
func relayFormatOf(protocol Protocol) contract.RelayFormat {
	switch protocol {
	case ProtocolOpenAI:
		return ir.FormatOpenAIChat
	case ProtocolAnthropic:
		return ir.FormatAnthropicMessages
	case ProtocolGemini:
		return ir.FormatGeminiGenerateContent
	default:
		return ""
	}
}
