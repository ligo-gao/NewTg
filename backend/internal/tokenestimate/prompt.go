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
)

// PromptTokens 从请求 IR 保守估算输入 token。nil 请求返回 0；空请求也至少
// 返回 requestOverheadTokens，保证付费模型不会因「请求没有可计费内容」而被
// 估算成 0。图片按固定 token 计（不解析 URL/二进制），工具定义与响应格式
// 按 JSON 序列化文本计入。
func PromptTokens(req *contract.Request) int64 {
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
				total += textTokens(part.Text)
			default:
				// 音频/工具调用/工具结果等：正文与 URL 都是真实上下文。
				total += textTokens(part.Text) + textTokens(part.URL)
			}
		}
	}
	if len(req.Tools) > 0 {
		total += float64(perToolOverhead * len(req.Tools))
		if b, err := json.Marshal(req.Tools); err == nil {
			total += textTokens(string(b))
		}
	}
	if req.ResponseFormat != nil {
		if b, err := json.Marshal(req.ResponseFormat); err == nil {
			total += textTokens(string(b))
		}
	}
	return int64(math.Ceil(total))
}

// textTokens 按 rune 累计：ASCII 与 CJK 使用不同系数，避免用字节数把
// 中文请求低估到 1/3。
func textTokens(s string) float64 {
	var total float64
	for _, r := range s {
		if r < utf8.RuneSelf {
			total += asciiTokensPerRune
			continue
		}
		total += nonASCIITokensPerRune
	}
	return total
}
