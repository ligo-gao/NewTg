package notify

import "testing"

// 注册表是 §3.9.1 / §3.9.3 的封闭契约；这些断言是「不接受未登记键」的
// 第一道门，任何新增/删除类别都必须同时改测试与设计文档。

func TestRegistryHasSixteenCategoriesAndEighteenGroups(t *testing.T) {
	categories := Categories()
	if len(categories) != 16 {
		t.Fatalf("category 数 = %d，want 16（§3.9.1）", len(categories))
	}
	seen := make(map[string]bool, len(categories))
	for _, category := range categories {
		if seen[category.Key] {
			t.Fatalf("category %q 重复", category.Key)
		}
		seen[category.Key] = true
		if len(category.Keys) == 0 {
			t.Fatalf("category %q 没有模板 key", category.Key)
		}
		if len(category.Channels) == 0 {
			t.Fatalf("category %q 没有渠道", category.Key)
		}
	}

	templates := Templates()
	if len(templates) != 18 {
		t.Fatalf("key×channel 组数 = %d，want 18（§3.9.3）", len(templates))
	}
	groups := make(map[string]bool, len(templates))
	for _, spec := range templates {
		if !seen[spec.Category] {
			t.Fatalf("模板 %s/%s 的 category %q 未注册", spec.Key, spec.Channel, spec.Category)
		}
		group := spec.Key + "\x00" + string(spec.Channel)
		if groups[group] {
			t.Fatalf("重复的 key×channel 组：%s/%s", spec.Key, spec.Channel)
		}
		groups[group] = true
	}
	// billing.balance_low / billing.quota_exhausted 各有 email 与 inapp 两组，
	// 故 18 组而非 16 组。
	if !groups["billing.balance_low\x00email"] || !groups["billing.balance_low\x00inapp"] {
		t.Fatal("billing.balance_low 必须同时有 email 与 inapp 模板")
	}
	if !groups["billing.quota_exhausted\x00email"] || !groups["billing.quota_exhausted\x00inapp"] {
		t.Fatal("billing.quota_exhausted 必须同时有 email 与 inapp 模板")
	}
}

func TestRegistryLockedSetMatchesP4(t *testing.T) {
	want := map[string]bool{
		"auth.verify_code":    true,
		"auth.password_reset": true,
		"auth.welcome":        true,
		"security.alert":      true,
		"system.smtp_test":    true,
		"system.announcement": true,
	}
	for _, category := range Categories() {
		if want[category.Key] != category.Locked {
			t.Fatalf("category %q locked=%v，want %v（P4）", category.Key, category.Locked, want[category.Key])
		}
	}
}

func TestRegistryEveryDefaultCopyIsValid(t *testing.T) {
	for _, spec := range Templates() {
		for _, locale := range Locales() {
			content, ok := spec.DefaultContent(locale)
			if !ok {
				t.Fatalf("%s/%s/%s 没有内置文案", spec.Key, spec.Channel, locale)
			}
			if err := ValidateContent(spec, content.Subject, content.Body, content.CTAText, content.CTAURL); err != nil {
				t.Fatalf("%s/%s/%s 内置文案未通过校验：%v", spec.Key, spec.Channel, locale, err)
			}
		}
	}
}

func TestRegistryClosedSet(t *testing.T) {
	if _, ok := LookupTemplate("auth.unknown", ChannelEmail); ok {
		t.Fatal("未注册 key 不得命中注册表")
	}
	if _, ok := LookupTemplate("billing.balance_low", ChannelWebhook); ok {
		t.Fatal("webhook 没有模板：billing.balance_low/webhook 不得命中")
	}
	if IsRegisteredCategory("billing.balance_low ") {
		t.Fatal("category 判定不得做模糊匹配")
	}
	if err := ValidateKey("not.registered"); err == nil {
		t.Fatal("ValidateKey 应拒绝未注册 key")
	}
}

func TestRegistryLocaleFallbackToEnglish(t *testing.T) {
	spec, ok := LookupTemplate("billing.balance_low", ChannelEmail)
	if !ok {
		t.Fatal("billing.balance_low/email 未注册")
	}
	// 设计只给 zh-CN / en 文案；其余 5 语回退 en（播种也落 en）。
	fr, frOK := spec.DefaultContent(LocaleFR)
	en, enOK := spec.DefaultContent(LocaleEN)
	if !frOK || !enOK {
		t.Fatal("DefaultContent 必须总能回退到 en")
	}
	if fr.Subject != en.Subject || fr.Body != en.Body {
		t.Fatalf("fr 默认文案应回退 en\nfr=%+v\nen=%+v", fr, en)
	}
}

func TestParseLocaleAndChannel(t *testing.T) {
	if locale, ok := ParseLocale(""); !ok || locale != LocaleEN {
		t.Fatalf("空 locale 应默认 en，得到 %q/%v", locale, ok)
	}
	if locale, ok := ParseLocale("zh-TW"); !ok || locale != LocaleZHTW {
		t.Fatalf("zh-TW 解析失败：%q/%v", locale, ok)
	}
	if _, ok := ParseLocale("de"); ok {
		t.Fatal("未注册语言必须返回 false（不得静默替换为 en）")
	}
	if channel, ok := ParseChannel(""); !ok || channel != ChannelEmail {
		t.Fatalf("空 channel 应默认 email，得到 %q/%v", channel, ok)
	}
	if _, ok := ParseChannel("webhook"); ok {
		t.Fatal("模板渠道不允许 webhook")
	}
}

func TestVariablesAreClosedPerGroup(t *testing.T) {
	spec, _ := LookupTemplate("billing.balance_low", ChannelInApp)
	if _, ok := spec.VariableByName("site_title"); ok {
		t.Fatal("inapp 组不应有 site_title（§3.9.3 白名单按 key×channel）")
	}
	if variable, ok := spec.VariableByName("balance"); !ok || !variable.Required {
		t.Fatalf("balance 应是 inapp 的必填变量，得到 %+v/%v", variable, ok)
	}
	email, _ := LookupTemplate("billing.balance_low", ChannelEmail)
	if _, ok := email.VariableByName("site_title"); !ok {
		t.Fatal("email 组应有 site_title")
	}
}
