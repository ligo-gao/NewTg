// Package tokenestimate 提供不依赖具体 tokenizer 的保守 token 估算。
//
// 估算用于预扣（hold）而非最终结算：结算仍以上游返回的 exact usage 为准，
// 这里的目标是让预扣不会因为「估算为 0」而变成零保护。
package tokenestimate

import (
	"encoding/json"
	"math"
	"unicode/utf8"

	"newtg/gateway/internal/contract"
)

const (
	// 与 tokenizer 无关的保守系数：ASCII 文本约 4 字符/token，取 0.5
	// 留出代码/JSON 的密集标点；非 ASCII（CJK）约 1 字符/token。
	asciiTokensPerRune    = 0.5
	nonASCIITokensPerRune = 1.0
	requestOverheadTokens = 16
	perMessageOverhead    = 4
	perToolOverhead       = 16
	perImagePartTokens    = 1024

	// 展示口径系数（DisplayPromptTokens 专用）。按 2026-10-26 的 DeepSeek
	// 实测标定：ASCII 0.223（英文散文）～0.335（Go 源码）token/字符，
	// 中文 0.55 token/字符。取中偏高的 0.28 / 0.55。
	asciiDisplayTokensPerRune    = 0.28
	nonASCIIDisplayTokensPerRune = 0.55
)

// PromptTokens 从请求 IR 保守估算输入 token。nil 请求返回 0；空请求也至少
// 返回 requestOverheadTokens，保证付费模型不会因「请求没有可计费内容」而被
// 估算成 0。图片按固定 token 计（不解析 URL/二进制），工具定义与响应格式
// 按 JSON 序列化文本计入。
func PromptTokens(req *contract.Request) int64 {
	return estimatePromptTokens(req, asciiTokensPerRune, nonASCIITokensPerRune)
}

// estimatePromptTokens 是两种口径共用的累加器：结构（消息/图片/工具开销）
// 相同，只有 rune 系数不同。分开只此一处，避免两个数字对同一次请求给出
// 互相矛盾的量级。
func estimatePromptTokens(req *contract.Request, ascii, nonASCII float64) int64 {
	if req == nil {
		return 0
	}
	total := float64(requestOverheadTokens)
	for _, msg := range req.Messages {
		total += perMessageOverhead
		for _, part := range msg.Content {
			switch part.Kind {
			case contract.PartImageURL, contract.PartImageB64:
				// 图片的真实成本取决于分辨率，这里只取一个固定下界，
				// 避免把 URL/base64 文本本身当成 prompt 重复计费。
				total += perImagePartTokens
			case contract.PartText:
				total += textTokens(part.Text, ascii, nonASCII)
			default:
				// 音频/工具调用/工具结果等：正文与 URL 都是真实上下文。
				total += textTokens(part.Text, ascii, nonASCII) + textTokens(part.URL, ascii, nonASCII)
			}
		}
	}
	if len(req.Tools) > 0 {
		total += float64(perToolOverhead * len(req.Tools))
		if b, err := json.Marshal(req.Tools); err == nil {
			total += textTokens(string(b), ascii, nonASCII)
		}
	}
	if req.ResponseFormat != nil {
		if b, err := json.Marshal(req.ResponseFormat); err == nil {
			total += textTokens(string(b), ascii, nonASCII)
		}
	}
	return int64(math.Ceil(total))
}

// textTokens 按 rune 累计：ASCII 与 CJK 使用不同系数，避免用字节数把
// 中文请求低估到 1/3。系数由调用方给出，以便预扣与展示共用同一结构。
func textTokens(s string, ascii, nonASCII float64) float64 {
	var total float64
	for _, r := range s {
		if r < utf8.RuneSelf {
			total += ascii
			continue
		}
		total += nonASCII
	}
	return total
}

// DisplayPromptTokens 估算**展示口径**的输入 token（写进 Anthropic 客户端的
// message_start.usage.input_tokens）。
//
// 与 PromptTokens 的区别只在用途：那个是预扣口径的保守上界（宁可多冻不可少
// 冻），系数 0.5 token/ASCII 字符会把英文实测放大 1.6～2.2 倍；本函数给客户端
// 看的数字必须尽量贴近真实 tokenizer，否则上下文百分比会显示成两倍。
//
// 系数按 2026-10-26 对 DeepSeek 的实测标定：10 万字符 Go 源码 33466 token
// （0.335/字符）、10 万字符英文散文 22253 token（0.223/字符）、2 万字符中文
// 10998 token（0.55/字符）。取 ASCII 0.28 / 非 ASCII 0.55，实测误差约
// -16%（代码）～+26%（英文散文）。
//
// 本函数的结果**只用于展示**：结算与 message_delta 回写一律用上游精确用量。
func DisplayPromptTokens(req *contract.Request) int64 {
	return estimatePromptTokens(req, asciiDisplayTokensPerRune, nonASCIIDisplayTokensPerRune)
}
