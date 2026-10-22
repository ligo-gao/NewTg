package plugin

import (
	"strings"
	"testing"
)

func requireRule(t *testing.T, issues Issues, rule string) {
	t.Helper()
	if !issues.hasRule(rule) {
		t.Fatalf("期望规则 %s 命中，实际 %v", rule, issues)
	}
}

func TestRuleT1KeyLength(t *testing.T) {
	issues := compileIssues(t, fixture{Key: strings.Repeat("k", 31)}.Source(), CompileOptions{})
	requireRule(t, issues, "T1")
}

func TestRuleT2Description(t *testing.T) {
	t.Run("too_long", func(t *testing.T) {
		issues := compileIssues(t, fixture{EnDesc: strings.Repeat("a", 513)}.Source(), CompileOptions{})
		requireRule(t, issues, "T2")
	})
	t.Run("missing_en", func(t *testing.T) {
		source := strings.Replace(fixture{}.Source(),
			`en: "Sample task plugin", zh: "示例任务插件"`, `zh: "示例任务插件"`, 1)
		issues := compileIssues(t, source, CompileOptions{})
		requireRule(t, issues, "T2")
	})
}

func TestRuleT3AuthShape(t *testing.T) {
	issues := compileIssues(t, fixture{Auth: `"oauth1"`}.Source(), CompileOptions{})
	requireRule(t, issues, "T3")

	issues = compileIssues(t, fixture{Auth: `{ type: "oauth1" }`}.Source(), CompileOptions{})
	requireRule(t, issues, "T3")

	// 合规形态（字符串与对象）都应通过。
	for _, auth := range []string{`"none"`, `"api_key"`, `"vertex_oauth"`, `{ type: "oauth2_jwt" }`} {
		if _, err := Compile(t.Context(), fixture{Auth: auth}.Source(), CompileOptions{}); err != nil {
			t.Fatalf("auth=%s 应通过: %v", auth, err)
		}
	}
}

func TestRuleT4ExamplesLimit(t *testing.T) {
	examples := make([]string, 17)
	for idx := range examples {
		examples[idx] = `{ seconds: 5 }`
	}
	issues := compileIssues(t, fixture{UsageExamples: "[" + strings.Join(examples, ",") + "]"}.Source(), CompileOptions{})
	requireRule(t, issues, "T4")
}

func TestRuleT5EnumLabels(t *testing.T) {
	long := strings.Repeat("x", 257)
	schema := `{
    mode: { type: "enum", enum: ["pro"],
      enumLabels: { pro: { en: "` + long + `" } } }
  }`
	issues := compileIssues(t, fixture{UsageSchema: schema}.Source(), CompileOptions{})
	requireRule(t, issues, "T5")
}

func TestRuleT7NumberDescriptionRequired(t *testing.T) {
	schema := `{ width: { type: "number", unit: "count" } }`
	issues := compileIssues(t, fixture{UsageSchema: schema}.Source(), CompileOptions{})
	requireRule(t, issues, "T7")
}

func TestRuleT8Units(t *testing.T) {
	schema := `{
    seconds: { type: "number", unit: "seconds",
      description: { en: "Video generation unit price", zh: "视频生成单价" } }
  }`
	issues := compileIssues(t, fixture{UsageSchema: schema}.Source(), CompileOptions{})
	requireRule(t, issues, "T8")
}

func TestRuleT9UnitLabelOnlyCount(t *testing.T) {
	schema := `{
    seconds: { type: "number", unit: "second", unitLabel: { en: "Second" },
      description: { en: "Video generation unit price", zh: "视频生成单价" } }
  }`
	issues := compileIssues(t, fixture{UsageSchema: schema}.Source(), CompileOptions{})
	requireRule(t, issues, "T9")

	// enumLabels 的 key 必须精确匹配一个已声明 enum 值。
	badEnum := `{
    mode: { type: "enum", enum: ["pro"], enumLabels: { plus: { en: "Plus" } } }
  }`
	issues = compileIssues(t, fixture{UsageSchema: badEnum}.Source(), CompileOptions{})
	requireRule(t, issues, "T9")
}

func TestRuleT10Profiles(t *testing.T) {
	t.Run("model_not_declared", func(t *testing.T) {
		profiles := `[
      { models: ["other-model"], schema: {},
        examples: [{ seconds: 5 }] }
    ]`
		issues := compileIssues(t, fixture{UsageProfiles: profiles}.Source(), CompileOptions{})
		requireRule(t, issues, "T10")
	})
	t.Run("model_in_two_profiles", func(t *testing.T) {
		profiles := `[
      { models: ["sample-model"], schema: {}, examples: [] },
      { models: ["sample-model"], schema: {}, examples: [] }
    ]`
		issues := compileIssues(t, fixture{UsageProfiles: profiles}.Source(), CompileOptions{})
		requireRule(t, issues, "T10")
	})
	t.Run("schema_null", func(t *testing.T) {
		profiles := `[ { models: ["sample-model"], schema: null, examples: [] } ]`
		issues := compileIssues(t, fixture{UsageProfiles: profiles}.Source(), CompileOptions{})
		requireRule(t, issues, "T10")
	})
	t.Run("token_profile_needs_examples", func(t *testing.T) {
		profiles := `[
      { models: ["sample-model"], schema: {
          prompt_tokens: { type: "number", unit: "token",
            description: { en: "Prompt tokens unit price", zh: "提示词单价" } }
        }, examples: [] }
    ]`
		issues := compileIssues(t, fixture{UsageSchema: "{}", UsageProfiles: profiles}.Source(), CompileOptions{})
		requireRule(t, issues, "T10")
	})
}

func TestDRules(t *testing.T) {
	cases := []struct {
		name string
		en   string
		zh   string
		rule string
	}{
		{name: "currency_symbol", en: "Video generation unit price $", zh: "视频生成单价", rule: "D2"},
		{name: "chinese_currency", en: "Video generation unit price", zh: "视频生成单价元", rule: "D2"},
		{name: "digits", en: "Video 4K generation unit price", zh: "视频生成单价", rule: "D3"},
		{name: "fullwidth_digits", en: "Video generation unit price", zh: "视频生成单价４", rule: "D3"},
		{name: "trailing_punctuation", en: "Video generation unit price.", zh: "视频生成单价", rule: "D4"},
		{name: "en_missing_suffix", en: "Video generation price", zh: "视频生成单价", rule: "D5"},
		{name: "zh_missing_word", en: "Video generation unit price", zh: "视频生成价格", rule: "D6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := compileIssues(t, fixture{EnDesc: "ok", ZhDesc: "示例", UsageSchema: `{
    seconds: { type: "number", unit: "second",
      description: { en: ` + quote(tc.en) + `, zh: ` + quote(tc.zh) + ` } }
  }`}.Source(), CompileOptions{})
			requireRule(t, issues, tc.rule)
		})
	}
}

func TestD1EmptyDescription(t *testing.T) {
	schema := `{
    seconds: { type: "number", unit: "second",
      description: { en: "   ", zh: "视频生成单价" } }
  }`
	issues := compileIssues(t, fixture{UsageSchema: schema}.Source(), CompileOptions{})
	requireRule(t, issues, "D1")
}

func TestNonNumberDescriptionOnlyStructural(t *testing.T) {
	// 布尔/枚举描述不施加 D5/D6 词根约束，只做 D1/D2/D4。
	schema := `{
    generate_audio: { type: "boolean", description: { en: "Generate audio", zh: "生成音频" } },
    mode: { type: "enum", enum: ["pro"],
      description: { en: "Generation mode", zh: "生成模式" },
      enumLabels: { pro: { en: "Pro", zh: "专业" } } }
  }`
	if _, err := Compile(t.Context(), fixture{UsageSchema: schema}.Source(), CompileOptions{}); err != nil {
		t.Fatalf("非数值字段不应触发 D5/D6: %v", err)
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

func TestRuleMissingAPIVersion(t *testing.T) {
	source := strings.Replace(fixture{}.Source(), "  apiVersion: 1,\n", "", 1)
	issues := compileIssues(t, source, CompileOptions{})
	requireRule(t, issues, "api_version")
}

func TestUnknownMetaFieldRejected(t *testing.T) {
	// T6：filePlaceholder 不是 manifest 字段；v1 严格拒绝未知字段。
	issues := compileIssues(t, fixture{ExtraMeta: map[string]string{
		"filePlaceholder": `{ __fileRef: "x" }`,
	}}.Source(), CompileOptions{})
	requireRule(t, issues, "meta_unknown_field")
}

func TestCapabilitiesValidation(t *testing.T) {
	issues := compileIssues(t, fixture{Capabilities: `["json-clone@2"]`}.Source(), CompileOptions{})
	requireRule(t, issues, "capability_unsupported")

	// submit-sse-delta@1 要求 submitResponseTypes 含 sse。
	issues = compileIssues(t, fixture{Capabilities: `["submit-sse-delta@1"]`}.Source(), CompileOptions{})
	requireRule(t, issues, "capability_mismatch")

	ok := fixture{
		Capabilities:        `["submit-sse-delta@1"]`,
		SubmitResponseTypes: `["json", "sse"]`,
		Exports: map[string]string{
			"parseSubmitEventDelta": `function () { return { changes: [], state: null, done: true }; }`,
		},
	}.Source()
	if _, err := Compile(t.Context(), ok, CompileOptions{}); err != nil {
		t.Fatalf("合规能力声明应通过: %v", err)
	}

	// 声明能力但未导出增量钩子 → 加载期拒绝（不能静默回退快照语义）。
	issues = compileIssues(t, fixture{
		Capabilities:        `["submit-sse-delta@1"]`,
		SubmitResponseTypes: `["json", "sse"]`,
	}.Source(), CompileOptions{})
	requireRule(t, issues, "hook_missing")

	// 导出增量钩子但没声明能力 → capability_missing。
	issues = compileIssues(t, fixture{
		SubmitResponseTypes: `["json", "sse"]`,
		Exports: map[string]string{
			"parseSubmitEventDelta": `function () { return { changes: [], state: null, done: true }; }`,
		},
	}.Source(), CompileOptions{})
	requireRule(t, issues, "capability_missing")

	// 快照钩子要求 submitResponseTypes 含 sse。
	issues = compileIssues(t, fixture{
		Exports: map[string]string{
			"parseSubmitEvent": `function () { return { state: null, done: true }; }`,
		},
	}.Source(), CompileOptions{})
	requireRule(t, issues, "capability_mismatch")
}

func TestRouteKindHookRules(t *testing.T) {
	// query 路由声明 decode 必须拒绝。
	issues := compileIssues(t, fixture{
		Routes: routeJS("GET", "/v1/query/:task_id", "query", "buildQueryRequest", "parseTaskResult"),
	}.Source(), CompileOptions{})
	requireRule(t, issues, "route_decode_forbidden")

	// 路由引用的钩子未导出。
	issues = compileIssues(t, fixture{
		Routes: routeJS("POST", "/v1/submit", "submit", "noSuchHook", "parseSubmitResponse"),
	}.Source(), CompileOptions{})
	requireRule(t, issues, "route_hook_missing")
}

func TestProtocolShape(t *testing.T) {
	source := strings.Replace(fixture{}.Source(), "  protocols: {},", "  protocols: { openai_responses: { deliver: \"sse\" } },", 1)
	issues := compileIssues(t, source, CompileOptions{})
	requireRule(t, issues, "protocol_shape")
}
