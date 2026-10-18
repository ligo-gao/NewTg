package billing

import (
	"testing"

	"newtg/gateway/internal/contract"
)

// vars 构造 usedVars 字面量。
func vars(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

// TestNormalizeTokens 覆盖 billing-00 §5 的核心规则：
// GPT 分支逐子类减、Claude 分支加回、len 两口径、img_cr 三段式、
// 缓存写归一与 p/c 下限钳制。
func TestNormalizeTokens(t *testing.T) {
	snap := contract.PriceSnapshot{}

	tests := []struct {
		name     string
		usage    contract.Usage
		usedVars map[string]bool
		want     contract.TokenParams
	}{
		{
			name: "GPT cr 被引用时从 p 扣除",
			usage: contract.Usage{
				Prompt: 100, Completion: 10,
				PromptIncludesCache: true, CacheRead: 30,
			},
			usedVars: vars("cr"),
			want: contract.TokenParams{
				P: 70, C: 10, Len: 100, CR: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "GPT cr 未引用时留在 p（基础价计费）",
			usage: contract.Usage{
				Prompt: 100, Completion: 10,
				PromptIncludesCache: true, CacheRead: 30,
			},
			usedVars: nil,
			want: contract.TokenParams{
				P: 100, C: 10, Len: 100, CR: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "Claude 未引用 cr 时加回 p（双向规则的加方向）",
			usage: contract.Usage{
				Prompt: 70, Completion: 5,
				PromptIncludesCache: false, CacheRead: 30,
			},
			usedVars: nil,
			want: contract.TokenParams{
				P: 100, C: 5, Len: 100, CR: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "Claude 引用 cr 时不加回（缓存段独立计价）",
			usage: contract.Usage{
				Prompt: 70, Completion: 5,
				PromptIncludesCache: false, CacheRead: 30,
			},
			usedVars: vars("cr"),
			want: contract.TokenParams{
				P: 70, C: 5, Len: 100, CR: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "Claude 的 5m/1h 缓存写从 Raw 拆分（仅携带，不减 p）",
			usage: contract.Usage{
				Prompt: 170, PromptIncludesCache: false, CacheWrite: 50,
				Raw: map[string]any{
					"cache_creation": map[string]any{
						"ephemeral_5m_input_tokens": 20,
						"ephemeral_1h_input_tokens": 30,
					},
				},
			},
			usedVars: vars("cc", "cc1h"),
			want: contract.TokenParams{
				P: 170, Len: 220, CC5m: 20, CC1h: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "GPT 缓存写取两个原始字段的较大值",
			usage: contract.Usage{
				Prompt: 200, PromptIncludesCache: true, CacheWrite: 10,
				Raw: map[string]any{"cached_creation_tokens": 50},
			},
			usedVars: vars("cc"),
			want: contract.TokenParams{
				P: 150, Len: 200, CC5m: 50, ImageCount: imageCountUnset,
			},
		},
		{
			name: "img_cr 三段式：有效明细时从 cr 与 img 同时移出",
			usage: contract.Usage{
				Prompt: 100, PromptIncludesCache: true, CacheRead: 40,
				PromptDetail: contract.UsageDetail{
					Image: 30, CacheReadImage: 10, Text: 50,
				},
			},
			usedVars: vars("cr", "img", "img_cr"),
			want: contract.TokenParams{
				P: 40, Len: 100, CR: 30, Img: 20, ImgCR: 10, ImageCount: imageCountUnset,
			},
		},
		{
			name: "img_cr 明细异常（缓存读小于图片缓存）时回退原聚合",
			usage: contract.Usage{
				Prompt: 100, PromptIncludesCache: true, CacheRead: 5,
				PromptDetail: contract.UsageDetail{Image: 30, CacheReadImage: 10},
			},
			usedVars: vars("img_cr"),
			want: contract.TokenParams{
				P: 100, Len: 100, CR: 5, Img: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "Claude 语义不拆分 img_cr（缓存总量继续进 cr）",
			usage: contract.Usage{
				Prompt: 100, PromptIncludesCache: false, CacheRead: 40,
				PromptDetail: contract.UsageDetail{Image: 30, CacheReadImage: 10},
			},
			usedVars: vars("img_cr"),
			want: contract.TokenParams{
				P: 140, Len: 140, CR: 40, Img: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "输出子类 img_o/ao 从 c 扣除",
			usage: contract.Usage{
				Prompt: 10, Completion: 100, PromptIncludesCache: true,
				CompletionDetail: contract.UsageDetail{Image: 20, Audio: 10},
			},
			usedVars: vars("img_o", "ao"),
			want: contract.TokenParams{
				P: 10, C: 70, Len: 10, ImgO: 20, AO: 10, ImageCount: imageCountUnset,
			},
		},
		{
			name: "p/c 负余量钳 0",
			usage: contract.Usage{
				Prompt: 10, PromptIncludesCache: true, CacheRead: 30,
			},
			usedVars: vars("cr"),
			want: contract.TokenParams{
				P: 0, Len: 10, CR: 30, ImageCount: imageCountUnset,
			},
		},
		{
			name: "Relation bool: ReasoningIncludedInCompletion 当前不产生扣除",
			usage: contract.Usage{
				Prompt: 10, Completion: 20, Reasoning: 5,
				PromptIncludesCache:           true,
				ReasoningIncludedInCompletion: true,
			},
			usedVars: nil,
			want: contract.TokenParams{
				P: 10, C: 20, Len: 10, ImageCount: imageCountUnset,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeTokens(tc.usage, snap, tc.usedVars)
			if got != tc.want {
				t.Fatalf("NormalizeTokens() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestNormalizeTokensLenUnaffectedByDeduction 验证 len 在扣除前取值：
// 缓存命中会缩小 p，但档位长度必须保持原始上下文长度（billing-00 §5.3）。
func TestNormalizeTokensLenUnaffectedByDeduction(t *testing.T) {
	u := contract.Usage{
		Prompt: 500_000, PromptIncludesCache: true, CacheRead: 400_000,
	}
	got := NormalizeTokens(u, contract.PriceSnapshot{}, vars("cr"))
	if got.Len != 500_000 {
		t.Fatalf("Len = %d, want 500000（len 必须在扣除前取值）", got.Len)
	}
	if got.P != 100_000 {
		t.Fatalf("P = %d, want 100000", got.P)
	}
}

// TestNormalizeTokensClaudeCacheWriteRemainder 验证 5m/1h 拆分的守恒：
// cc5m + cc1h 必须等于归一后的 cc（剩余部分归 5m/通用档），len 不得漏 token。
func TestNormalizeTokensClaudeCacheWriteRemainder(t *testing.T) {
	tests := []struct {
		name     string
		raw      map[string]any
		wantCC5m int64
		wantCC1h int64
	}{
		{
			name:     "只给 5m 明细：全部缓存写归 5m",
			raw:      map[string]any{"cache_creation": map[string]any{"ephemeral_5m_input_tokens": 20}},
			wantCC5m: 50,
			wantCC1h: 0,
		},
		{
			name:     "只给 1h 明细：剩余归 5m",
			raw:      map[string]any{"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 30}},
			wantCC5m: 20,
			wantCC1h: 30,
		},
		{
			name: "两档与总量不一致：以总量为准，剩余归 5m",
			raw: map[string]any{"cache_creation": map[string]any{
				"ephemeral_5m_input_tokens": 20,
				"ephemeral_1h_input_tokens": 10,
			}},
			wantCC5m: 40,
			wantCC1h: 10,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := contract.Usage{
				Prompt: 170, PromptIncludesCache: false, CacheWrite: 50, Raw: tc.raw,
			}
			got := NormalizeTokens(u, contract.PriceSnapshot{}, nil)
			if got.CC5m != tc.wantCC5m || got.CC1h != tc.wantCC1h {
				t.Fatalf("CC5m/CC1h = %d/%d, want %d/%d",
					got.CC5m, got.CC1h, tc.wantCC5m, tc.wantCC1h)
			}
			if got.CC5m+got.CC1h != 50 {
				t.Fatalf("CC5m+CC1h = %d, want 50（拆分必须守恒）", got.CC5m+got.CC1h)
			}
			if got.Len != 220 { // 170 + cr 0 + cc 50
				t.Fatalf("Len = %d, want 220", got.Len)
			}
		})
	}
}

// TestNormalizeTokensClaudeCacheWriteMax 验证缓存写取 max 且负值按 0 参与。
func TestNormalizeTokensClaudeCacheWriteMax(t *testing.T) {
	u := contract.Usage{
		Prompt: 10, PromptIncludesCache: false, CacheWrite: 10,
		Raw: map[string]any{"cached_creation_tokens": -5},
	}
	got := NormalizeTokens(u, contract.PriceSnapshot{}, nil)
	if got.CC5m != 10 {
		t.Fatalf("CC5m = %d, want 10（负值按 0 参与）", got.CC5m)
	}
	if got.Len != 20 { // input 10 + cr 0 + cc 10
		t.Fatalf("Len = %d, want 20", got.Len)
	}
}
