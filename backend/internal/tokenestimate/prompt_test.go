package tokenestimate

import (
	"strings"
	"testing"
	"unicode/utf8"

	"newtg/gateway/internal/contract"
)

func TestPromptTokens(t *testing.T) {
	tests := []struct {
		name string
		req  *contract.Request
		want int64
	}{
		{"nil request", nil, 0},
		{"empty request", &contract.Request{}, 16},
		{
			"ascii text",
			&contract.Request{Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Kind: contract.PartText, Text: "hello world"}},
			}}},
			26, // ceil(11 × 0.5) + 4 + 16
		},
		{
			"cjk text",
			&contract.Request{Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Kind: contract.PartText, Text: "你好"}},
			}}},
			22, // ceil(2 × 1.0) + 4 + 16
		},
		{
			"message without content",
			&contract.Request{Messages: []contract.Message{{Role: contract.RoleUser}}},
			20, // 4 + 16
		},
		{
			"image part",
			&contract.Request{Messages: []contract.Message{{
				Role:    contract.RoleUser,
				Content: []contract.ContentPart{{Kind: contract.PartImageURL, URL: "https://example.com/a.png"}},
			}}},
			1044, // 1024 + 4 + 16
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PromptTokens(tt.req); got != tt.want {
				t.Fatalf("PromptTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPromptTokensIncludesToolSchema 保证只有 tools 的请求不会回到 0：
// 工具定义是真实 prompt 成本，必须进入预扣。
func TestPromptTokensIncludesToolSchema(t *testing.T) {
	without := PromptTokens(&contract.Request{})
	with := PromptTokens(&contract.Request{Tools: []contract.Tool{{
		Name:        "lookup",
		Description: "look up a record",
		Schema:      map[string]any{"type": "object"},
	}}})
	if with <= without {
		t.Fatalf("with tools = %d, without = %d; want tools to increase the estimate", with, without)
	}
}

// TestDisplayPromptTokensWithinMeasuredRange：展示口径的估算（写进
// message_start.usage.input_tokens）必须落在真实 tokenizer 的量级内。
//
// 参考实测（2026-10-26，DeepSeek）：10 万字符 Go 源码 ≈ 33466 token
// （0.335/字符）、10 万字符英文散文 ≈ 22253 token（0.223/字符）、
// 2 万字符中文 ≈ 10998 token（0.55/字符）。展示值允许近似，但翻倍或归零
// 都会让客户端的上下文统计失真。
func TestDisplayPromptTokensWithinMeasuredRange(t *testing.T) {
	ascii := strings.Repeat("for i := 0; i < len(items); i++ { total += items[i].Value } ", 60)
	zh := strings.Repeat("这是一段用于校准中文分词系数的说明文字", 60)

	asciiRate := float64(DisplayPromptTokens(textRequest(ascii))) / float64(len(ascii))
	if asciiRate < 0.20 || asciiRate > 0.40 {
		t.Fatalf("ascii 估算 %.3f token/字符，超出实测区间 [0.20, 0.40]", asciiRate)
	}
	zhRate := float64(DisplayPromptTokens(textRequest(zh))) / float64(utf8.RuneCountInString(zh))
	if zhRate < 0.45 || zhRate > 0.65 {
		t.Fatalf("中文估算 %.3f token/字符，超出实测区间 [0.45, 0.65]", zhRate)
	}
	if got := DisplayPromptTokens(nil); got != 0 {
		t.Fatalf("nil request = %d, want 0", got)
	}
	// 展示值不得高于预扣用的保守上界，否则同一次请求的两个数字自相矛盾。
	req := textRequest(zh)
	if display, hold := DisplayPromptTokens(req), PromptTokens(req); display > hold {
		t.Fatalf("display = %d > hold = %d", display, hold)
	}
}

func textRequest(text string) *contract.Request {
	return &contract.Request{Messages: []contract.Message{{
		Role:    contract.RoleUser,
		Content: []contract.ContentPart{{Kind: contract.PartText, Text: text}},
	}}}
}
