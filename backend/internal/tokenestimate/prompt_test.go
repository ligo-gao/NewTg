package tokenestimate

import (
	"testing"

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
