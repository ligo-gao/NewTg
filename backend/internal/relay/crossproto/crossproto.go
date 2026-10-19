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
	"context"
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

	encoded, err := ir.FromIR(resp)
	if err != nil {
		return nil, fmt.Errorf("crossproto: encode OpenAI response: %w", err)
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
