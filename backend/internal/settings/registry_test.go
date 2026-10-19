package settings

import (
	"encoding/json"
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
