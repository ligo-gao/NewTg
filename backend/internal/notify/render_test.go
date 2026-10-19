package notify

import (
	"errors"
	"strings"
	"testing"
)

func mustTemplate(t *testing.T, key string, channel Channel) Template {
	t.Helper()
	spec, ok := LookupTemplate(key, channel)
	if !ok {
		t.Fatalf("模板 %s/%s 未注册", key, channel)
	}
	return spec
}

func TestValidateContentRejectsUnregisteredVariable(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	err := ValidateContent(spec, "{{.purpose_label}}验证码", "{{.code}} {{.foo}}", "", "")
	var unknown *ErrUnknownVariable
	if !errors.As(err, &unknown) || unknown.Variable != "foo" {
		t.Fatalf("未知变量应返回 ErrUnknownVariable，得到 %v", err)
	}
}

func TestValidateContentRejectsNonFieldConstructs(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	cases := []string{
		"{{.code | upper}}",
		"{{if .code}}x{{end}}",
		"{{range .items}}{{.}}{{end}}",
		"{{printf \"%s\" .code}}",
		"{{ .code",
	}
	for _, body := range cases {
		err := ValidateContent(spec, "code", body, "", "")
		if err == nil {
			t.Fatalf("非字段访问构造 %q 应被拒绝", body)
		}
		var construct *ErrInvalidConstruct
		if !errors.As(err, &construct) {
			t.Fatalf("%q 应返回 ErrInvalidConstruct，得到 %v", body, err)
		}
	}
}

func TestValidateContentFieldRules(t *testing.T) {
	inapp := mustTemplate(t, "billing.balance_low", ChannelInApp)
	if err := ValidateContent(inapp, "不该有标题", "余额不足", "", ""); err == nil {
		t.Fatal("inapp 模板不允许 subject")
	}
	email := mustTemplate(t, "billing.balance_low", ChannelEmail)
	if err := ValidateContent(email, "", "正文", "", ""); err == nil {
		t.Fatal("email 模板必须提供 subject")
	}
	if err := ValidateContent(email, "主题", "正文", "按钮", ""); err == nil {
		t.Fatal("CTA 文案与链接必须成对")
	}
	if err := ValidateContent(email, "主题", "正文", "按钮", "javascript:alert(1)"); err == nil {
		t.Fatal("CTA 链接只允许 http(s)")
	}
	if err := ValidateContent(email, strings.Repeat("a", MaxSubjectRunes+1), "正文", "", ""); err == nil {
		t.Fatal("subject 超过 256 字符应被拒绝")
	}
	if err := ValidateContent(email, "主题", strings.Repeat("a", MaxBodyRunes+1), "", ""); err == nil {
		t.Fatal("body 超过 16KB 应被拒绝")
	}
}

func TestRenderContentPlainSubstitution(t *testing.T) {
	spec := mustTemplate(t, "billing.balance_low", ChannelEmail)
	content, _ := spec.DefaultContent(LocaleZHCN)
	rendered, err := RenderContent(spec, content, LocaleZHCN, map[string]any{
		"username":   "alice",
		"balance":    "812",
		"threshold":  "1,000",
		"topup_url":  "https://gw.example.com/wallet",
		"site_title": "Example Gateway",
	})
	if err != nil {
		t.Fatalf("RenderContent: %v", err)
	}
	if !strings.Contains(rendered.Subject, "Example Gateway") ||
		!strings.Contains(rendered.Body, "812") ||
		!strings.Contains(rendered.Body, "1,000") {
		t.Fatalf("替换结果不完整：subject=%q body=%q", rendered.Subject, rendered.Body)
	}
	if strings.Contains(rendered.Body, "{{") {
		t.Fatalf("渲染后仍残留模板构造：%q", rendered.Body)
	}
	// 纯文本替换：值里的 HTML 不做转义/解释（P2/P3 由 Markdown 渲染器负责
	// 消毒，不在模板层做）。
	rendered, err = RenderContent(spec, content, LocaleZHCN, map[string]any{
		"username":   "<b>alice</b>",
		"balance":    "1",
		"threshold":  "2",
		"topup_url":  "https://gw.example.com/wallet",
		"site_title": "Site",
	})
	if err != nil {
		t.Fatalf("RenderContent: %v", err)
	}
	if !strings.Contains(rendered.Body, "<b>alice</b>") {
		t.Fatalf("纯文本替换不得转义值：%q", rendered.Body)
	}
}

func TestRenderContentFailsOnMissingRequired(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	content, _ := spec.DefaultContent(LocaleEN)
	_, err := RenderContent(spec, content, LocaleEN, map[string]any{
		"username":        "alice",
		"expires_minutes": "10",
		"purpose_label":   "Sign-up verification",
		// code 缺失
	})
	var missing *ErrMissingVariable
	if !errors.As(err, &missing) || missing.Variable != "code" {
		t.Fatalf("缺失必填变量应返回 ErrMissingVariable(code)，得到 %v", err)
	}
}

func TestRenderContentOptionalVariableRendersEmpty(t *testing.T) {
	spec := mustTemplate(t, "auth.welcome", ChannelEmail)
	content, _ := spec.DefaultContent(LocaleEN)
	rendered, err := RenderContent(spec, content, LocaleEN, map[string]any{
		"username":      "alice",
		"site_title":    "Example Gateway",
		"dashboard_url": "https://gw.example.com/",
		// welcome_bonus 是可选的；未开启赠送时渲染为空行（§3.9.4 末）
	})
	if err != nil {
		t.Fatalf("可选变量缺失不应失败：%v", err)
	}
	if strings.Contains(rendered.Body, "{{.welcome_bonus}}") {
		t.Fatalf("可选变量应渲染为空串：%q", rendered.Body)
	}
}

func TestValidateVariablesChecksTypes(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	err := ValidateVariables(spec, map[string]any{
		"code":            "428173",
		"expires_minutes": map[string]any{"bad": true},
		"purpose_label":   "注册验证",
	})
	var invalid *ErrInvalidValue
	if !errors.As(err, &invalid) || invalid.Variable != "expires_minutes" {
		t.Fatalf("int 变量收到对象应返回 ErrInvalidValue，得到 %v", err)
	}
}

func TestMergeVariablesUsesSamples(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	merged := MergeVariables(spec, LocaleEN, map[string]any{"username": "bob"})
	if merged["username"] != "bob" {
		t.Fatalf("调用方值应覆盖样例，得到 %v", merged["username"])
	}
	if merged["purpose_label"] != "Sign-up verification" {
		t.Fatalf("en 预览应使用英文样例，得到 %v", merged["purpose_label"])
	}
	if err := ValidateVariables(spec, merged); err != nil {
		t.Fatalf("样例值应满足必填约束：%v", err)
	}
}

func TestValidateContentRejectsExtraBraces(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	for _, body := range []string{
		"{{{.code}}}",
		"{{.code}}}",
		"{{{{.code}}}}",
	} {
		err := ValidateContent(spec, "code", body, "", "")
		if err == nil {
			t.Fatalf("额外花括号构造 %q 应被拒绝（F8）", body)
		}
		var construct *ErrInvalidConstruct
		if !errors.As(err, &construct) {
			t.Fatalf("%q 应返回 ErrInvalidConstruct，得到 %v", body, err)
		}
	}
}

func TestIntVariablesMustBeIntegers(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	// 样例值以字符串保存，但输入必须真的是整数（F7）。
	err := ValidateVariables(spec, map[string]any{
		"code":            "428173",
		"expires_minutes": "abc",
		"purpose_label":   "注册验证",
	})
	var invalid *ErrInvalidValue
	if !errors.As(err, &invalid) || invalid.Variable != "expires_minutes" {
		t.Fatalf("非整数输入应返回 ErrInvalidValue，得到 %v", err)
	}
	// 合法整数串通过。
	if err := ValidateVariables(spec, map[string]any{
		"code":            "428173",
		"expires_minutes": "10",
		"purpose_label":   "注册验证",
	}); err != nil {
		t.Fatalf("合法整数串不应失败：%v", err)
	}
}

func TestIntVariablesRejectNonIntegralFloats(t *testing.T) {
	spec := mustTemplate(t, "auth.verify_code", ChannelEmail)
	// HTTP 层用 UseNumber，但进程内调用方可能直接传 float64（L4 纵深防御）。
	err := ValidateVariables(spec, map[string]any{
		"code":            "428173",
		"expires_minutes": 1.5,
		"purpose_label":   "注册验证",
	})
	var invalid *ErrInvalidValue
	if !errors.As(err, &invalid) || invalid.Variable != "expires_minutes" {
		t.Fatalf("非整数 float64 应返回 ErrInvalidValue，得到 %v", err)
	}
	// 整数值的 float64 合法，渲染为整数串而不是 "10.0"。
	if err := ValidateVariables(spec, map[string]any{
		"code":            "428173",
		"expires_minutes": 10.0,
		"purpose_label":   "注册验证",
	}); err != nil {
		t.Fatalf("整数值 float64 不应失败：%v", err)
	}
	content, _ := spec.DefaultContent(LocaleEN)
	rendered, err := RenderContent(spec, content, LocaleEN, map[string]any{
		"code":            "428173",
		"expires_minutes": 10.0,
		"purpose_label":   "Sign-up verification",
	})
	if err != nil {
		t.Fatalf("RenderContent: %v", err)
	}
	if !strings.Contains(rendered.Body, "10 minutes") || strings.Contains(rendered.Body, "10.0") {
		t.Fatalf("float64 整数应渲染为 \"10\"：%q", rendered.Body)
	}
}
