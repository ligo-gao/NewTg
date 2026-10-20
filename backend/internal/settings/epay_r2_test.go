package settings

// Epay 设置段 R2 收口：空查询标记与 sign_mode 闭集。

import (
	"encoding/json"
	"testing"
)

func epayR2ValidSection() map[string]any {
	merged := sectionDefaults("pay.epay")
	merged["pay.epay.enabled"] = true
	merged["pay.epay.api_url"] = "https://pay.example.com"
	merged["pay.epay.pid"] = "1001"
	return merged
}

// TestEpaySectionRejectsEmptyQuery 验证 `https://host/?` 这类「有分隔符、
// 无查询内容」的地址被拒：url.Parse 把它表示为 ForceQuery=true 而
// RawQuery=""，只看 RawQuery/Fragment 会漏掉。
func TestEpaySectionRejectsEmptyQuery(t *testing.T) {
	secrets := map[string]bool{"pay.epay.key": true}
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"api_url", func(m map[string]any) {
			m["pay.epay.api_url"] = "https://pay.example.com/?"
		}},
		{"notify_url", func(m map[string]any) {
			m["pay.epay.notify_url"] = "https://tg.example.com/cb?"
		}},
		{"return_url", func(m map[string]any) {
			m["pay.epay.return_url"] = "/wallet?"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged := epayR2ValidSection()
			tc.mutate(merged)
			if err := validateEpaySection(merged, secrets); err == nil {
				t.Fatalf("%s 的空 query 必须被拒绝", tc.name)
			}
		})
	}

	// 真实 query 仍合法：返回页可以带固定参数（`/wallet?pay=epay`）。
	merged := epayR2ValidSection()
	merged["pay.epay.return_url"] = "/wallet?pay=epay"
	if err := validateEpaySection(merged, secrets); err != nil {
		t.Fatalf("带实际参数的 return_url 应通过: %v", err)
	}
}

// TestEpaySignModeRegistry：默认 append（标准算法，不能因新增键改变默认
// 行为），只接受 append/amp 闭集。
func TestEpaySignModeRegistry(t *testing.T) {
	def := mustLookup(t, "pay.epay.sign_mode")
	if def.Default != "append" {
		t.Fatalf("默认 sign_mode = %v，期望 append", def.Default)
	}
	if _, err := normalizeRaw(def, json.RawMessage(`"amp"`)); err != nil {
		t.Fatalf("amp 应可保存: %v", err)
	}
	if _, err := normalizeRaw(def, json.RawMessage(`"raw"`)); err == nil {
		t.Fatal("未知 sign_mode 必须被拒绝")
	}
}
