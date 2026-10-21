package sensitive

import (
	"encoding/json"
	"strings"
	"testing"

	"newtg/gateway/internal/contract"
)

// mapReader 是 Reader 的内存实现（值按 configcenter 的 JSON 快照语义存放）。
type mapReader map[string]json.RawMessage

func (m mapReader) Get(key string) (json.RawMessage, bool) {
	raw, ok := m[key]
	return raw, ok
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func config(t *testing.T, scope map[string]bool, words []string, action string) mapReader {
	t.Helper()
	return mapReader{
		keyEnabled: raw(t, true),
		keyScope:   raw(t, scope),
		keyWords:   raw(t, words),
		keyAction:  raw(t, action),
	}
}

func textRequest(messages ...contract.Message) *contract.Request {
	return &contract.Request{Messages: messages}
}

func textMessage(role contract.Role, text string) contract.Message {
	return contract.Message{Role: role, Content: []contract.ContentPart{{Kind: contract.PartText, Text: text}}}
}

// ---------- 启用 / 关闭 / 坏配置 ----------

func TestDisabledIsNoOp(t *testing.T) {
	for name, reader := range map[string]Reader{
		"nil":      nil,
		"missing":  mapReader{},
		"disabled": mapReader{keyEnabled: raw(t, false), keyWords: raw(t, []string{"bad"})},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := (&Cache{}).Load(reader)
			if cfg.Active() {
				t.Fatal("关闭/缺省态不得 Active")
			}
			if got := cfg.CheckText(SurfacePrompt, "bad"); got {
				t.Fatal("关闭态不得命中")
			}
			if got := cfg.CheckRequest(textRequest(textMessage(contract.RoleUser, "bad"))); got.Matched {
				t.Fatal("关闭态不得命中 IR 请求")
			}
		})
	}
}

func TestEmptyWordsNeverBlocks(t *testing.T) {
	cfg := (&Cache{}).Load(config(t, map[string]bool{"prompt": true}, nil, "block"))
	if cfg.Active() {
		t.Fatal("空词表即使开启也不应 Active（设计：不会拦截任何内容）")
	}
	if cfg.CheckText(SurfacePrompt, "anything") {
		t.Fatal("空词表不得命中")
	}
}

func TestBadConfigFallsBackToDisabled(t *testing.T) {
	cases := map[string]mapReader{
		"enabled not bool": {
			keyEnabled: raw(t, "yes"),
			keyWords:   raw(t, []string{"bad"}),
		},
		"scope not object": {
			keyEnabled: raw(t, true),
			keyScope:   raw(t, "prompt"),
			keyWords:   raw(t, []string{"bad"}),
		},
		"scope unknown field": {
			keyEnabled: raw(t, true),
			keyScope:   raw(t, map[string]bool{"prompt": true, "tools": true}),
			keyWords:   raw(t, []string{"bad"}),
		},
		"scope value not bool": {
			keyEnabled: raw(t, true),
			keyScope:   raw(t, map[string]json.RawMessage{"prompt": raw(t, "true")}),
			keyWords:   raw(t, []string{"bad"}),
		},
		"words wrong element": {
			keyEnabled: raw(t, true),
			keyWords:   raw(t, []any{"bad", 7}),
		},
		"words wrong shape": {
			keyEnabled: raw(t, true),
			keyWords:   raw(t, map[string]bool{"bad": true}),
		},
		"action unknown": {
			keyEnabled: raw(t, true),
			keyWords:   raw(t, []string{"bad"}),
			keyAction:  raw(t, "replace"),
		},
	}
	for name, reader := range cases {
		t.Run(name, func(t *testing.T) {
			if cfg := (&Cache{}).Load(reader); cfg.Active() {
				t.Fatalf("坏配置必须回落为不启用，got %+v", cfg)
			}
		})
	}
}

func TestLegacyStringWords(t *testing.T) {
	reader := mapReader{
		keyEnabled: raw(t, true),
		keyScope:   raw(t, map[string]bool{"prompt": true}),
		keyWords:   raw(t, "first\nsecond\r\nthird"),
		keyAction:  raw(t, "block"),
	}
	cfg := (&Cache{}).Load(reader)
	for _, word := range []string{"first", "second", "third"} {
		if !cfg.CheckText(SurfacePrompt, "x "+word+" y") {
			t.Fatalf("旧字符串词表未命中 %q", word)
		}
	}
}

func TestDefaultsWhenKeysAbsent(t *testing.T) {
	reader := mapReader{keyEnabled: raw(t, true), keyWords: raw(t, []string{"bad"})}
	cfg := (&Cache{}).Load(reader)
	if !cfg.Scope.Prompt || cfg.Scope.System {
		t.Fatalf("scope 缺省应为 {prompt:true, system:false}，got %+v", cfg.Scope)
	}
	if cfg.Action != ActionBlock {
		t.Fatalf("action 缺省应为 block，got %q", cfg.Action)
	}
}

// ---------- 作用面 ----------

func TestScopeControlsSurfaces(t *testing.T) {
	reader := config(t, map[string]bool{"prompt": true, "system": false}, []string{"bad"}, "block")
	cfg := (&Cache{}).Load(reader)

	if !cfg.CheckText(SurfacePrompt, "bad") {
		t.Fatal("prompt 面已勾选，必须命中")
	}
	if cfg.CheckText(SurfaceSystem, "bad") {
		t.Fatal("system 面未勾选，不得命中")
	}
	if res := cfg.CheckRequest(textRequest(textMessage(contract.RoleSystem, "bad"))); res.Matched {
		t.Fatal("system 消息不得在 prompt-only 作用面命中")
	}
	if res := cfg.CheckRequest(textRequest(textMessage(contract.RoleUser, "bad"))); !res.Matched || res.Surface != SurfacePrompt {
		t.Fatalf("user 消息应命中 prompt 面，got %+v", res)
	}

	reader = config(t, map[string]bool{"prompt": false, "system": true}, []string{"bad"}, "block")
	cfg = (&Cache{}).Load(reader)
	if cfg.CheckText(SurfacePrompt, "bad") {
		t.Fatal("prompt 面未勾选，不得命中")
	}
	if res := cfg.CheckRequest(textRequest(textMessage(contract.RoleSystem, "bad"))); !res.Matched || res.Surface != SurfaceSystem {
		t.Fatalf("system 消息应命中 system 面，got %+v", res)
	}
	// developer 是系统指令角色，并入 system 面。
	if res := cfg.CheckRequest(textRequest(textMessage(contract.RoleDeveloper, "bad"))); !res.Matched || res.Surface != SurfaceSystem {
		t.Fatalf("developer 消息应命中 system 面，got %+v", res)
	}
}

func TestOnlyUserControlledTextIsScanned(t *testing.T) {
	cfg := NewConfig(true, Scope{Prompt: true, System: true}, []string{"bad"}, ActionBlock)

	// 图片 URL / 工具结果 / assistant 内容都不属于用户提示词文本。
	req := &contract.Request{Messages: []contract.Message{
		{Role: contract.RoleUser, Content: []contract.ContentPart{
			{Kind: contract.PartImageURL, URL: "https://example.test/bad.png"},
		}},
		{Role: contract.RoleAssistant, Content: []contract.ContentPart{
			{Kind: contract.PartText, Text: "bad"},
		}},
		{Role: contract.RoleTool, Content: []contract.ContentPart{
			{Kind: contract.PartText, Text: "bad"},
		}},
	}}
	if res := cfg.CheckRequest(req); res.Matched {
		t.Fatalf("非用户提示词文本被误扫：%+v", res)
	}
}

func TestReasoningPartsAreSkipped(t *testing.T) {
	cfg := NewConfig(true, Scope{Prompt: true, System: true}, []string{"bad"}, ActionBlock)
	req := textRequest(contract.Message{Role: contract.RoleUser, Content: []contract.ContentPart{
		{Kind: contract.PartText, Text: "bad", Meta: map[string]any{"thought": true}},
		{Kind: contract.PartText, Text: "bad", Meta: map[string]any{"anthropic_thinking": "sig"}},
	}})
	if res := cfg.CheckRequest(req); res.Matched {
		t.Fatalf("推理载体不得参与匹配：%+v", res)
	}
}

// ---------- 匹配规则：大小写 / Unicode / 全半角 / 通配 / 多词 ----------

func TestMatchingRules(t *testing.T) {
	cfg := NewConfig(true, Scope{Prompt: true}, []string{
		"Secret", "机密", "暴*词", "full width",
	}, ActionBlock)

	cases := []struct {
		name string
		text string
		want bool
	}{
		{"exact", "a Secret b", true},
		{"lower case input", "a secret b", true},
		{"upper case input", "a SECRET b", true},
		{"unicode", "这是机密内容", true},
		{"wildcard", "暴风词", true},
		{"wildcard long", "暴什么什么词", true},
		{"wildcard missing tail", "暴风", false},
		{"full width word vs half width input", "full width", true},
		// 词条含半角空格，输入用全角空格（U+3000）也应命中。
		{"full width space", "full　width", true},
		// 全角字母输入应折叠到半角词条。
		{"full width letters", "ＳＥＣＲＥＴ", true},
		{"multi word later entry", "x 机密 y", true},
		{"miss", "nothing here", false},
	}
	// “class is not ass” 只有在词表含 ass 时才命中；这里单独验证 Contains 语义。
	cfgContains := NewConfig(true, Scope{Prompt: true}, []string{"ass"}, ActionBlock)
	if !cfgContains.CheckText(SurfacePrompt, "class") {
		t.Fatal("设计未定义词边界，Contains 语义应命中 class 中的 ass")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.CheckText(SurfacePrompt, tc.text)
			if got != tc.want {
				t.Fatalf("CheckText(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestWordsAreTrimmedAndDeduped(t *testing.T) {
	cfg := NewConfig(true, Scope{Prompt: true}, []string{" bad ", "BAD", "bad", ""}, ActionBlock)
	if len(cfg.words) != 1 || cfg.words[0] != "bad" {
		t.Fatalf("词表应 trim + 去重，got %v", cfg.words)
	}
}

// ---------- 缓存 ----------

func TestCacheInvalidatesOnChange(t *testing.T) {
	reader := config(t, map[string]bool{"prompt": true}, []string{"first"}, "block")
	cache := &Cache{}
	cfg := cache.Load(reader)
	if !cfg.CheckText(SurfacePrompt, "first") || cfg.CheckText(SurfacePrompt, "second") {
		t.Fatal("初始快照错误")
	}

	reader[keyWords] = raw(t, []string{"second"})
	cfg = cache.Load(reader)
	if cfg.CheckText(SurfacePrompt, "first") {
		t.Fatal("词表变更后仍命中旧词：缓存未失效")
	}
	if !cfg.CheckText(SurfacePrompt, "second") {
		t.Fatal("词表变更后未命中新词")
	}
}

func TestCacheDisabledFastPathDoesNotReadWords(t *testing.T) {
	reader := &countingReader{values: mapReader{keyEnabled: raw(t, false)}}
	cfg := (&Cache{}).Load(reader)
	if cfg.Active() {
		t.Fatal("关闭态不得 Active")
	}
	if reader.calls != 1 {
		t.Fatalf("关闭态只应读取 enabled 一个键，got %d 次 Get", reader.calls)
	}
}

type countingReader struct {
	values mapReader
	calls  int
}

func (r *countingReader) Get(key string) (json.RawMessage, bool) {
	r.calls++
	raw, ok := r.values[key]
	return raw, ok
}

func TestNormalizeFoldsWidthAndCase(t *testing.T) {
	if got := normalize("ＡＢＣ　ＤＥＦ"); got != "abc def" {
		t.Fatalf("normalize = %q, want %q", got, "abc def")
	}
	if got := normalize("MiXeD 机密"); got != "mixed 机密" {
		t.Fatalf("normalize 不应破坏 Unicode：%q", got)
	}
	if !strings.Contains(normalize("＊"), "*") {
		t.Fatal("全角星号应折叠为半角通配符")
	}
}
