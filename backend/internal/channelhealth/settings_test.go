package channelhealth

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
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

// TestLoadWorkerSettingsDefaults：六键全部缺省时按注册表默认（R-M4-07/13/14）：
// 自动恢复关闭、阈值 5s、间隔 10 分钟、并发 5；自动禁用仍默认关闭。
func TestLoadWorkerSettingsDefaults(t *testing.T) {
	for name, source := range map[string]SettingsSource{
		"nil":     nil,
		"missing": mapSettings{},
	} {
		t.Run(name, func(t *testing.T) {
			settings := LoadWorkerSettings(source)
			if settings.AutoDisableEnabled {
				t.Fatal("rl.health.auto_disable_enabled 默认必须 false（危险方向）")
			}
			if settings.AutoEnableEnabled {
				t.Fatal("rl.health.auto_enable_enabled 默认必须 false（R-M4-09）")
			}
			if settings.ResponseTimeThreshold != 5*time.Second {
				t.Fatalf("threshold = %v, want 5s", settings.ResponseTimeThreshold)
			}
			if settings.Interval != 10*time.Minute {
				t.Fatalf("interval = %v, want 10m", settings.Interval)
			}
			if settings.Concurrency != 5 {
				t.Fatalf("concurrency = %d, want 5", settings.Concurrency)
			}
		})
	}
}

// TestLoadWorkerSettingsOverridesAndClamp：配置值生效；越界钳制到注册表
// 范围（1–1440 / 1–50），脏值回落默认（R-M4-13）。
func TestLoadWorkerSettingsOverridesAndClamp(t *testing.T) {
	source := mapSettings{
		KeyAutoEnableEnabled:            json.RawMessage(`true`),
		KeyResponseTimeThresholdSeconds: json.RawMessage(`2.5`),
		KeyTestIntervalMinutes:          json.RawMessage(`15`),
		KeyTestConcurrency:              json.RawMessage(`7`),
	}
	settings := LoadWorkerSettings(source)
	if !settings.AutoEnableEnabled {
		t.Fatal("auto_enable_enabled=true 未生效")
	}
	if settings.ResponseTimeThreshold != 2500*time.Millisecond {
		t.Fatalf("threshold = %v, want 2.5s", settings.ResponseTimeThreshold)
	}
	if settings.Interval != 15*time.Minute {
		t.Fatalf("interval = %v, want 15m", settings.Interval)
	}
	if settings.Concurrency != 7 {
		t.Fatalf("concurrency = %d, want 7", settings.Concurrency)
	}

	for name, tc := range map[string]struct {
		interval    int
		concurrency int
		wantInt     time.Duration
		wantConc    int
	}{
		"below-min": {interval: 0, concurrency: 0, wantInt: time.Minute, wantConc: 1},
		"above-max": {interval: 99999, concurrency: 999, wantInt: 1440 * time.Minute, wantConc: 50},
		"negative":  {interval: -5, concurrency: -1, wantInt: time.Minute, wantConc: 1},
	} {
		t.Run(name, func(t *testing.T) {
			clamped := LoadWorkerSettings(mapSettings{
				KeyTestIntervalMinutes: json.RawMessage(strconv.Itoa(tc.interval)),
				KeyTestConcurrency:     json.RawMessage(strconv.Itoa(tc.concurrency)),
			})
			if clamped.Interval != tc.wantInt {
				t.Fatalf("interval = %v, want %v", clamped.Interval, tc.wantInt)
			}
			if clamped.Concurrency != tc.wantConc {
				t.Fatalf("concurrency = %d, want %d", clamped.Concurrency, tc.wantConc)
			}
		})
	}

	// 脏值/非法阈值：回落默认而不是把 worker 停摆。
	dirty := LoadWorkerSettings(mapSettings{
		KeyAutoEnableEnabled:            json.RawMessage(`"true"`),
		KeyResponseTimeThresholdSeconds: json.RawMessage(`0`),
		KeyTestIntervalMinutes:          json.RawMessage(`"ten"`),
		KeyTestConcurrency:              json.RawMessage(`null`),
	})
	if dirty.AutoEnableEnabled {
		t.Fatal("字符串 \"true\" 不得被解释为开启")
	}
	if dirty.ResponseTimeThreshold != 5*time.Second {
		t.Fatalf("非法阈值 = %v, want 5s 默认", dirty.ResponseTimeThreshold)
	}
	if dirty.Interval != 10*time.Minute || dirty.Concurrency != 5 {
		t.Fatalf("脏值 interval/concurrency = %v/%d, want 10m/5", dirty.Interval, dirty.Concurrency)
	}
}
