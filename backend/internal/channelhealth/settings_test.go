package channelhealth

import (
	"encoding/json"
	"strings"
	"testing"
)

// mapSettings 是 SettingsSource 的内存实现（configcenter 快照的等价物）。
type mapSettings map[string]json.RawMessage

func (m mapSettings) Get(key string) (json.RawMessage, bool) {
	value, ok := m[key]
	return value, ok
}

func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %v: %v", value, err)
	}
	return raw
}

// TestLoadDecisionSettingsDefaults：缺键 / nil source → 开关 false +
// 注册表默认关键词（7 条）。
func TestLoadDecisionSettingsDefaults(t *testing.T) {
	for name, source := range map[string]SettingsSource{
		"nil":     nil,
		"missing": mapSettings{},
	} {
		t.Run(name, func(t *testing.T) {
			settings := LoadDecisionSettings(source)
			if settings.AutoDisableEnabled {
				t.Fatal("缺键时自动禁用开关必须为 false（R-M4-01 C 默认关闭）")
			}
			if len(settings.DisableKeywords) != 7 {
				t.Fatalf("默认关键词数 = %d，want 7: %v",
					len(settings.DisableKeywords), settings.DisableKeywords)
			}
			// 注册表默认文案必须被 lowercase/trim，便于判定时直接 Contains。
			joined := ""
			for _, keyword := range settings.DisableKeywords {
				if keyword != normalizeKeywords([]string{keyword})[0] {
					t.Fatalf("关键词未归一化: %q", keyword)
				}
				joined += keyword + "\n"
			}
			if !strings.Contains(joined, "your credit balance is too low") {
				t.Fatalf("默认关键词缺少冻结文案: %v", settings.DisableKeywords)
			}
		})
	}
}

// TestLoadDecisionSettingsDirtyBoolFailsClosed：非 bool 的脏值一律回落 false，
// 不做字符串 "true" 之类的宽松解析（避免静默开启危险方向）。
func TestLoadDecisionSettingsDirtyBoolFailsClosed(t *testing.T) {
	for _, value := range []any{"true", 1, nil} {
		source := mapSettings{KeyAutoDisableEnabled: rawJSON(t, value)}
		if LoadDecisionSettings(source).AutoDisableEnabled {
			t.Fatalf("脏值 %#v 不得被解释为开启", value)
		}
	}
	if !LoadDecisionSettings(mapSettings{KeyAutoDisableEnabled: rawJSON(t, true)}).AutoDisableEnabled {
		t.Fatal("显式 true 应开启")
	}
}

// TestLoadDecisionSettingsKeywordShapes：兼容旧 TypeString 行与新 TypeList
// 数组（R-M4-11），并按行 trim/去空/lowercase。
func TestLoadDecisionSettingsKeywordShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want []string
	}{
		{
			name: "string-lines",
			raw:  rawJSON(t, "  First Keyword \n\nSECOND\n"),
			want: []string{"first keyword", "second"},
		},
		{
			name: "list",
			raw:  rawJSON(t, []string{" Alpha ", "", "BETA"}),
			want: []string{"alpha", "beta"},
		},
		{
			name: "empty-list",
			raw:  rawJSON(t, []string{}),
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := LoadDecisionSettings(mapSettings{KeyDisableKeywords: tc.raw})
			if len(settings.DisableKeywords) != len(tc.want) {
				t.Fatalf("keywords = %v, want %v", settings.DisableKeywords, tc.want)
			}
			for i := range tc.want {
				if settings.DisableKeywords[i] != tc.want[i] {
					t.Fatalf("keywords = %v, want %v", settings.DisableKeywords, tc.want)
				}
			}
		})
	}
}

// TestLoadDecisionSettingsInvalidKeywordsFallBackToDefaults：形态无法识别时
// 回落注册表默认，而不是变成「零关键词」（那会静默关闭关键词触发面）。
func TestLoadDecisionSettingsInvalidKeywordsFallBackToDefaults(t *testing.T) {
	source := mapSettings{KeyDisableKeywords: rawJSON(t, map[string]any{"not": "a list"})}
	settings := LoadDecisionSettings(source)
	if len(settings.DisableKeywords) != 7 {
		t.Fatalf("非法关键词表应回落默认 7 条，得到 %v", settings.DisableKeywords)
	}
}

// TestParseDisableKeywordsRejectsScalar：数字/布尔/null 等不是合法关键词表。
func TestParseDisableKeywordsRejectsScalar(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`123`),
		json.RawMessage(`true`),
		json.RawMessage(`null`),
		json.RawMessage(`{"a":1}`),
	} {
		if _, ok := ParseDisableKeywords(raw); ok {
			t.Fatalf("%s 不应被接受为关键词表", raw)
		}
	}
}
