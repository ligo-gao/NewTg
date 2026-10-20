package settings

import (
	"errors"
	"testing"
)

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
