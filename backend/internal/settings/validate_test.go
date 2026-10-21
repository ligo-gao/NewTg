package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestDisableKeywordsIsListType 是 R-M4-11 的回归点：前端 lines 控件保存时
// 发送字符串数组，旧 TypeString 会以 400 拒绝。这里同时锁定注册表类型、
// 默认值形态（7 条数组）与元素/数量校验。
func TestDisableKeywordsIsListType(t *testing.T) {
	setting, ok := Lookup("rl.health.disable_keywords")
	if !ok {
		t.Fatal("rl.health.disable_keywords 未注册")
	}
	if setting.Type != TypeList {
		t.Fatalf("类型 = %q, want %q（R-M4-11）", setting.Type, TypeList)
	}
	defaults, ok := DefaultValue(setting).([]any)
	if !ok || len(defaults) != 7 {
		t.Fatalf("默认值 = %#v, want 7 条字符串数组", DefaultValue(setting))
	}

	// 数组保存：值原样接受（trim/lowercase 由消费方完成，见
	// channelhealth.ParseDisableKeywords）。
	got, err := normalizeRaw(setting, json.RawMessage(`["A quota","Permission denied"]`))
	if err != nil {
		t.Fatalf("数组关键词应被接受：%v", err)
	}
	items := got.([]any)
	if len(items) != 2 || items[0] != "A quota" {
		t.Fatalf("归一化结果 = %#v", got)
	}

	// 旧的 JSON 字符串形态在新类型下必须被拒绝（管理员应通过界面重新保存；
	// 历史行仍可读，见 channelhealth 的兼容用例）。
	if _, err := normalizeRaw(setting, json.RawMessage(`"a\nb"`)); err == nil {
		t.Fatal("字符串形态不得通过 TypeList 写入")
	}

	badElements := []string{
		`["ok", 1]`,     // 非字符串
		`["ok", ""]`,    // 空串
		`["ok", "   "]`, // 纯空白
		fmt.Sprintf(`[%q]`, strings.Repeat("x", 257)), // 超过单项上限
	}
	for _, raw := range badElements {
		if _, err := normalizeRaw(setting, json.RawMessage(raw)); err == nil {
			t.Fatalf("非法元素 %s 应被拒绝", raw)
		}
	}

	// 数量上限：超过 100 条拒绝（匹配成本的硬边界）。
	tooMany, _ := json.Marshal(func() []string {
		out := make([]string, 101)
		for i := range out {
			out[i] = "keyword"
		}
		return out
	}())
	if _, err := normalizeRaw(setting, tooMany); err == nil {
		t.Fatal("超过 100 条关键词应被拒绝")
	}
}

// TestWebhookSectionWarnsWithoutBlocking 覆盖 M1 的保存期告警：
// 设计（modules-08 F-08-17）要求 allowlist 下自填 Webhook 主机「给出明确
// 提示」但**不阻断保存**；settings 没有响应级 warning 通道，因此这里断言
// 校验返回 nil（HTTP 200）且告警接缝被调用一次。
func TestWebhookSectionWarnsWithoutBlocking(t *testing.T) {
	original := webhookAllowlistWarning
	t.Cleanup(func() { webhookAllowlistWarning = original })

	var warned []string
	webhookAllowlistWarning = func(host string) { warned = append(warned, host) }

	values := map[string]any{
		"notify.webhook.enabled": true,
		"notify.webhook.url":     "https://hooks.example.com/notify",
	}
	secrets := map[string]bool{"notify.webhook.secret": true}
	if err := validateWebhookSection(values, secrets); err != nil {
		t.Fatalf("告警不得阻断保存：%v", err)
	}
	if len(warned) != 1 || warned[0] != "hooks.example.com" {
		t.Fatalf("告警主机 = %v，期望 [hooks.example.com]", warned)
	}

	// 未启用时不产生告警（草稿保存不应制造噪声）。
	warned = nil
	values["notify.webhook.enabled"] = false
	if err := validateWebhookSection(values, secrets); err != nil {
		t.Fatalf("disabled 保存失败：%v", err)
	}
	if len(warned) != 0 {
		t.Fatalf("未启用时不应告警：%v", warned)
	}

	// 校验失败时不告警：失败路径本身会阻断，提示没有意义。
	warned = nil
	values["notify.webhook.enabled"] = true
	values["notify.webhook.url"] = "http://hooks.example.com/notify"
	var verr *ValidationError
	if err := validateWebhookSection(values, secrets); !errors.As(err, &verr) {
		t.Fatalf("http URL 应被拒绝，实际 %v", err)
	}
	if len(warned) != 0 {
		t.Fatalf("校验失败时不应告警：%v", warned)
	}
}
