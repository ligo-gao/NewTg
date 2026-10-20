package settings

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// 本文件是注册表与值校验的单元验收（不依赖 PG/Redis）。
// 集成读写见 service_integration_test.go。

func TestRegistryUniqueAndSectionsDeclared(t *testing.T) {
	seen := map[string]bool{}
	for _, setting := range All() {
		if seen[setting.Key] {
			t.Fatalf("重复键 %q", setting.Key)
		}
		seen[setting.Key] = true
		if !IsKnownSection(setting.Section) {
			t.Fatalf("%q 的 section %q 未声明", setting.Key, setting.Section)
		}
		if !strings.HasPrefix(setting.Key, strings.SplitN(setting.Section, ".", 2)[0]+".") {
			t.Fatalf("%q 的键前缀与 section %q 不一致", setting.Key, setting.Section)
		}
		if _, err := json.Marshal(setting.Default); err != nil {
			t.Fatalf("%q 默认值不可序列化: %v", setting.Key, err)
		}
	}
	for _, section := range Sections() {
		if _, ok := Section(section); !ok {
			t.Fatalf("section %q 无注册行", section)
		}
	}
}

// TestP0SectionsRegistered 覆盖任务书点名的 P0 section。
func TestP0SectionsRegistered(t *testing.T) {
	for _, section := range []string{
		"site", "nav", "auth", "auth.turnstile", "billing",
		"pay", "pay.stripe", "pay.alipay", "rl", "rl.health", "sec", "notify.smtp",
	} {
		if !IsKnownSection(section) {
			t.Fatalf("P0 section %q 未登记", section)
		}
	}
}

func TestPublicKeysAllProjectable(t *testing.T) {
	snapshot := PublicSnapshot(nil)
	for _, setting := range All() {
		_, inSnapshot := snapshot[setting.Key]
		if setting.Public != inSnapshot {
			t.Fatalf("%q public=%v，但快照包含=%v", setting.Key, setting.Public, inSnapshot)
		}
	}
	// 每个公开键都应能投影到自己的域；否则前端读不到（§1.4.1 一致性断言）。
	for _, setting := range All() {
		if !setting.Public {
			continue
		}
		domain := strings.SplitN(setting.Key, ".", 2)[0]
		projected := DomainValues(snapshot, domain)
		if len(projected) == 0 {
			t.Fatalf("%q 无法投影到域 %q", setting.Key, domain)
		}
	}
}

func TestDomainValuesNestsPaths(t *testing.T) {
	snapshot := map[string]any{
		"site.title":             "Demo",
		"site.seo.description":   "desc",
		"nav.home.enabled":       true,
		"billing.quota_per_unit": int64(100),
	}
	site := DomainValues(snapshot, "site")
	if site["title"] != "Demo" {
		t.Fatalf("site.title = %v", site["title"])
	}
	seo, ok := site["seo"].(map[string]any)
	if !ok || seo["description"] != "desc" {
		t.Fatalf("site.seo 投影错误: %#v", site["seo"])
	}
	nav := DomainValues(snapshot, "nav")
	home, ok := nav["home"].(map[string]any)
	if !ok || home["enabled"] != true {
		t.Fatalf("nav.home 投影错误: %#v", nav["home"])
	}
}

func TestNormalizeRawRejectsTypeAndRange(t *testing.T) {
	cases := []struct {
		name string
		key  string
		raw  string
		want string
	}{
		{"bool 类型", "nav.home.enabled", `"yes"`, "expected bool"},
		{"int 小数", "billing.quota_per_unit", `1.5`, "expected integer"},
		{"int 下限", "billing.quota_per_unit", `0`, "must be >="},
		{"int 上限", "site.rankings_min_samples", `10`, "must be >="},
		{"enum 闭集", "billing.display_unit", `"usd"`, "must be one of"},
		{"URL 协议", "site.server_address", `"javascript:alert(1)"`, "absolute http(s)"},
		{"时区", "site.timezone", `"Mars/Olympus"`, "invalid IANA timezone"},
		{"null", "site.title", `null`, "must not be null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setting, ok := Lookup(tc.key)
			if !ok {
				t.Fatalf("键 %q 未登记", tc.key)
			}
			_, err := normalizeRaw(setting, json.RawMessage(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v，期望包含 %q", err, tc.want)
			}
		})
	}
}

func TestNormalizeRawNormalizesServerAddressAndPresets(t *testing.T) {
	server, _ := Lookup("site.server_address")
	value, err := normalizeRaw(server, json.RawMessage(`"https://example.com/"`))
	if err != nil {
		t.Fatalf("server_address: %v", err)
	}
	if value != "https://example.com" {
		t.Fatalf("server_address = %v，期望去尾斜杠", value)
	}

	presets, _ := Lookup("pay.amount_presets_cny")
	value, err = normalizeRaw(presets, json.RawMessage(`[50,10,10,30]`))
	if err != nil {
		t.Fatalf("amount_presets: %v", err)
	}
	list := value.([]any)
	if len(list) != 3 || list[0].(int64) != 10 || list[1].(int64) != 30 || list[2].(int64) != 50 {
		t.Fatalf("档位未去重排序: %#v", list)
	}
}

func TestSensitiveWordLimit(t *testing.T) {
	setting, _ := Lookup("sec.sensitive.words")
	tooMany := make([]string, 5001)
	for i := range tooMany {
		tooMany[i] = `"w` + string(rune('a'+i%26)) + `"`
	}
	raw := json.RawMessage("[" + strings.Join(tooMany, ",") + "]")
	if _, err := normalizeRaw(setting, raw); err == nil {
		t.Fatal("5001 个敏感词应被拒绝")
	}
}

// F1：默认值深拷贝必须保持整数类型，否则 section 级 int64 校验会被静默跳过。
func TestDefaultValuePreservesInt64(t *testing.T) {
	for _, key := range []string{
		"billing.quota_per_unit",
		"pay.amount_presets_cny",
		"site.rankings_min_samples",
		"rl.user_model_rpm",
	} {
		setting, ok := Lookup(key)
		if !ok {
			t.Fatalf("%q 未登记", key)
		}
		value := DefaultValue(setting)
		switch typed := value.(type) {
		case int64:
			// 单值默认。
		case []any:
			for _, item := range typed {
				if _, ok := item.(int64); !ok {
					t.Fatalf("%q 列表元素类型 = %T，期望 int64", key, item)
				}
			}
		default:
			t.Fatalf("%q 默认值类型 = %T，期望 int64/[]int64", key, value)
		}
	}
}

func TestDangerousDirectionsDeclared(t *testing.T) {
	for _, setting := range All() {
		if !setting.Dangerous {
			if setting.DangerDirection != "" {
				t.Fatalf("%q 非危险项却有 direction=%q", setting.Key, setting.DangerDirection)
			}
			continue
		}
		switch setting.DangerDirection {
		case DangerDirectionOn, DangerDirectionOff, DangerDirectionAny:
		default:
			t.Fatalf("%q 危险项 direction=%q 非法", setting.Key, setting.DangerDirection)
		}
	}
}

// 公开 / 危险 / 密钥三份金集合：注册表扩张必须先改这里（契约审计点）。
func TestRegistryGoldenSets(t *testing.T) {
	public := map[string]bool{}
	var dangerous, secrets []string
	for _, setting := range All() {
		if setting.Public {
			public[setting.Key] = true
		}
		if setting.Dangerous {
			dangerous = append(dangerous, setting.Key+":"+setting.DangerDirection)
		}
		if setting.Secret {
			secrets = append(secrets, setting.Key)
		}
	}
	wantPublic := []string{
		"site.title", "site.logo_url", "site.server_address", "site.timezone",
		"site.footer_html", "site.about_html", "site.filing_no", "site.docs_url",
		"site.status_page_url", "site.rankings_min_samples", "site.oss_credits",
		"site.seo.description", "site.seo.og_image_url", "site.home_custom_enabled",
		"site.home_custom_html", "site.home_blocks", "site.demo.enabled",
		"nav.home.enabled", "nav.home.require_auth", "nav.console.enabled",
		"nav.console.require_auth", "nav.pricing.enabled", "nav.pricing.require_auth",
		"nav.rankings.enabled", "nav.rankings.require_auth", "nav.docs.enabled",
		"nav.docs.require_auth", "nav.about.enabled", "nav.about.require_auth",
		"nav.modules.console", "nav.modules.chat.playground",
		"auth.signup_enabled", "auth.password_login_enabled",
		"auth.email_verification_required", "auth.passkey.enabled",
		"auth.turnstile.enabled", "auth.turnstile.site_key",
		"billing.quota_per_unit", "billing.display_unit", "billing.currency_symbol",
		"billing.signup_grant_credits", "billing.signup_grant_requires_verified_email",
		"billing.display_token_stat_enabled",
		"pay.online_enabled", "pay.amount_presets_cny",
		"pay.stripe.enabled", "pay.stripe.publishable_key",
		"pay.alipay.enabled", "pay.alipay.sandbox",
		"pay.epay.enabled", "pay.creem.enabled", "pay.waffo.enabled",
		"pay.waffo_pancake.enabled",
	}
	if len(public) != len(wantPublic) {
		t.Fatalf("公开键数量 = %d，期望 %d", len(public), len(wantPublic))
	}
	for _, key := range wantPublic {
		if !public[key] {
			t.Fatalf("公开键 %q 缺失", key)
		}
	}
	sort.Strings(dangerous)
	wantDangerous := []string{
		"auth.password_login_enabled:off", "auth.signup_enabled:off",
		"billing.currency_symbol:on", "billing.quota_per_unit:on",
		"deploy.ionet.api_key:any", "deploy.ionet.enabled:off",
		"pay.alipay.private_key:any", "pay.alipay.public_key:any",
		"pay.epay.merchant_id:on", "pay.force_complete_max_amount:on",
		"pay.online_enabled:off", "pay.stripe.secret_key:any",
		"pay.stripe.webhook_secret:any", "pay.waffo.merchant_id:on",
		"pay.waffo_pancake.merchant_id:on",
		// 插件禁用清单：增删任意条目都会改变数据面能力（any）。
		"plugin.disabled_factory_keys:any",
		"rl.health.auto_disable_enabled:on",
		"rl.health.disable_keywords:on", "sec.sensitive.words:on",
		"sec.ssrf.allow_private_ip:on", "sec.ssrf.enabled:off",
		"site.home_custom_html:on",
	}
	if strings.Join(dangerous, ",") != strings.Join(wantDangerous, ",") {
		t.Fatalf("危险键集合 = %v\n期望 = %v", dangerous, wantDangerous)
	}
	sort.Strings(secrets)
	wantSecrets := []string{
		"auth.turnstile.secret_key", "deploy.ionet.api_key",
		"notify.smtp.password", "notify.webhook.secret",
		"pay.alipay.private_key", "pay.alipay.public_key",
		"pay.stripe.secret_key", "pay.stripe.webhook_secret",
	}
	if strings.Join(secrets, ",") != strings.Join(wantSecrets, ",") {
		t.Fatalf("密钥集合 = %v\n期望 = %v", secrets, wantSecrets)
	}
	if _, ok := Lookup("pay.stripe.publishable_key"); !ok {
		t.Fatal("pay.stripe.publishable_key 由 P-17/G7-R46 冻结，必须在注册表且公开")
	}
}

func TestSectionValidators(t *testing.T) {
	merged := sectionDefaults("pay")
	merged["pay.stripe.min_topup_cny"] = int64(100)
	// 默认档位含 10/30/50，低于 100 必须被拒（F1 回归点）。
	if err := validatePaySection(merged, nil); err == nil {
		t.Fatal("默认档位低于 min_topup 时应被拒")
	}

	merged = sectionDefaults("pay.stripe")
	merged["pay.stripe.enabled"] = true
	if err := validateStripeSection(merged, nil); err == nil {
		t.Fatal("启用 Stripe 但无 secret 应被拒")
	}
	merged = sectionDefaults("pay.alipay")
	merged["pay.alipay.enabled"] = true
	if err := validateAlipaySection(merged, nil); err == nil {
		t.Fatal("启用支付宝但无 app_id/密钥应被拒")
	}

	if _, err := normalizeRaw(mustLookup(t, "pay.topup_group_ratio"),
		json.RawMessage(`{"vip":"1.2"}`)); err == nil {
		t.Fatal("倍率表字符串值应被拒")
	}
	if _, err := normalizeRaw(mustLookup(t, "pay.topup_group_ratio"),
		json.RawMessage(`{"vip":-1}`)); err == nil {
		t.Fatal("倍率表负值应被拒")
	}
	if _, err := normalizeRaw(mustLookup(t, "billing.group_group_ratio"),
		json.RawMessage(`{"a":{"b":0}}`)); err == nil {
		t.Fatal("嵌套倍率 0 应被拒")
	}
	if _, err := normalizeRaw(mustLookup(t, "rl.group_overrides"),
		json.RawMessage(`{"vip":-1}`)); err == nil {
		t.Fatal("分组限速负值应被拒")
	}
	if _, err := normalizeRaw(mustLookup(t, "pay.alipay.private_key"),
		json.RawMessage(`"not-a-pem"`)); err == nil {
		t.Fatal("非法 PEM 应被拒")
	}
	if _, err := normalizeRaw(mustLookup(t, "pay.alipay.private_key"),
		json.RawMessage(`"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----"`)); err != nil {
		t.Fatalf("合法 PEM 被拒: %v", err)
	}
}

func sectionDefaults(section string) map[string]any {
	rows, _ := Section(section)
	out := make(map[string]any, len(rows))
	for _, setting := range rows {
		out[setting.Key] = DefaultValue(setting)
	}
	return out
}

func mustLookup(t *testing.T, key string) Setting {
	t.Helper()
	setting, ok := Lookup(key)
	if !ok {
		t.Fatalf("%q 未登记", key)
	}
	return setting
}

// notify.webhook（F-08-20 / D2-10..11）的注册表与交叉校验验收。
func TestWebhookSectionRegistered(t *testing.T) {
	rows, ok := Section("notify.webhook")
	if !ok || len(rows) != 4 {
		t.Fatalf("notify.webhook section 行数 = %d，期望 4", len(rows))
	}
	enabled := mustLookup(t, "notify.webhook.enabled")
	if enabled.Type != TypeBool || enabled.Default != false {
		t.Fatalf("enabled 类型/默认 = %v/%v", enabled.Type, enabled.Default)
	}
	url := mustLookup(t, "notify.webhook.url")
	if url.Type != TypeString || url.MaxLen != 1024 {
		t.Fatalf("url 类型/上限 = %v/%d", url.Type, url.MaxLen)
	}
	secret := mustLookup(t, "notify.webhook.secret")
	if !secret.Secret {
		t.Fatal("secret 必须登记为密钥类")
	}
	events := mustLookup(t, "notify.webhook.events")
	if events.Type != TypeList {
		t.Fatalf("events 类型 = %v", events.Type)
	}
	got := DefaultValue(events).([]any)
	if len(got) != 1 || got[0] != "balance_low" {
		t.Fatalf("events 默认 = %v，期望 [balance_low]", got)
	}
}

func TestWebhookEventsNormalizeAndValidate(t *testing.T) {
	events := mustLookup(t, "notify.webhook.events")

	// 去重并保持提交顺序。
	value, err := normalizeRaw(events, json.RawMessage(`["payment_success","balance_low","payment_success"]`))
	if err != nil {
		t.Fatalf("合法事件被拒: %v", err)
	}
	list := value.([]any)
	if len(list) != 2 || list[0] != "payment_success" || list[1] != "balance_low" {
		t.Fatalf("去重保序失败: %v", list)
	}

	// 允许空数组（= 不订阅任何事件）。
	if _, err := normalizeRaw(events, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("空事件数组应允许: %v", err)
	}

	// 闭集之外的值、非字符串、超过 4 项都必须拒绝。
	for _, bad := range []string{
		`["payment_failed"]`,
		`["balance_low","unknown"]`,
		`[1]`,
		`["balance_low","payment_success","announcement","user_signup","balance_low2"]`,
	} {
		if _, err := normalizeRaw(events, json.RawMessage(bad)); err == nil {
			t.Fatalf("非法事件列表应被拒: %s", bad)
		}
	}
}

func TestWebhookSectionValidation(t *testing.T) {
	base := sectionDefaults("notify.webhook")
	base["notify.webhook.enabled"] = true

	t.Run("https_and_secret_required", func(t *testing.T) {
		values := cloneSettingsValues(t, base)
		if err := validateWebhookSection(values, map[string]bool{}); err == nil {
			t.Fatal("启用但无 URL/secret 应被拒")
		}
		values["notify.webhook.url"] = "https://hooks.example.com/x"
		if err := validateWebhookSection(values, map[string]bool{}); err == nil {
			t.Fatal("启用但无 secret 应被拒")
		}
		if err := validateWebhookSection(values, map[string]bool{"notify.webhook.secret": true}); err != nil {
			t.Fatalf("https + secret 应通过: %v", err)
		}
	})

	t.Run("http_rejected", func(t *testing.T) {
		values := cloneSettingsValues(t, base)
		values["notify.webhook.url"] = "http://hooks.example.com/x"
		err := validateWebhookSection(values, map[string]bool{"notify.webhook.secret": true})
		if err == nil {
			t.Fatal("http URL 在启用时必须被拒（D2-11 不实现本地调试例外）")
		}
	})

	t.Run("userinfo_and_fragment_rejected", func(t *testing.T) {
		for _, raw := range []string{
			"https://user:pass@hooks.example.com/x",
			"https://hooks.example.com/x#frag",
			"https:///nohost",
		} {
			values := cloneSettingsValues(t, base)
			values["notify.webhook.url"] = raw
			if err := validateWebhookSection(values, map[string]bool{"notify.webhook.secret": true}); err == nil {
				t.Fatalf("%q 应被拒", raw)
			}
		}
	})

	t.Run("disabled_allows_incomplete_draft", func(t *testing.T) {
		values := cloneSettingsValues(t, base)
		values["notify.webhook.enabled"] = false
		values["notify.webhook.url"] = ""
		if err := validateWebhookSection(values, map[string]bool{}); err != nil {
			t.Fatalf("未启用时允许未配置草稿: %v", err)
		}
	})
}

func cloneSettingsValues(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
