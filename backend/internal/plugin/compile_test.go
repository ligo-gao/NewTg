package plugin

import (
	"context"
	"strings"
	"testing"
	"time"

	"newtg/gateway/plugins"
)

func mustCompile(t *testing.T, source string, opts CompileOptions) *CompiledPlugin {
	t.Helper()
	plugin, err := Compile(context.Background(), source, opts)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	return plugin
}

func compileIssues(t *testing.T, source string, opts CompileOptions) Issues {
	t.Helper()
	plugin, err := Compile(context.Background(), source, opts)
	if err == nil {
		t.Fatalf("期望编译失败，实际成功: %+v", plugin.Key)
	}
	issues, ok := err.(Issues)
	if !ok {
		t.Fatalf("错误类型应为 Issues，实际 %T: %v", err, err)
	}
	return issues
}

func TestEmbeddedSampleCompiles(t *testing.T) {
	sources, err := plugins.TaskPluginSources()
	if err != nil {
		t.Fatalf("读取内置插件: %v", err)
	}
	source, ok := sources["sample"]
	if !ok {
		t.Fatalf("内置插件缺少 sample，实际 keys=%v", sortedKeys(sources))
	}
	plugin := mustCompile(t, source, CompileOptions{ExpectedKey: "sample", Layer: LayerFactory})

	if plugin.Key != "sample" || plugin.Version != "1.0.0" {
		t.Fatalf("manifest 不符: key=%q version=%q", plugin.Key, plugin.Version)
	}
	if plugin.Manifest().FetchMode != "per_task" {
		t.Fatalf("fetchMode=%q", plugin.Manifest().FetchMode)
	}
	for _, hook := range []string{"buildSubmitRequest", "parseSubmitResponse", "buildQueryRequest", "parseTaskResult"} {
		if !plugin.HasHook(hook) {
			t.Fatalf("缺少钩子 %s，实际 %v", hook, plugin.Hooks())
		}
	}
	if plugin.SourceHash() != sourceHash(source) {
		t.Fatalf("source hash 口径必须是原始字节的 sha256")
	}
	if len(plugin.SourceHash()) != 64 || strings.ToLower(plugin.SourceHash()) != plugin.SourceHash() {
		t.Fatalf("source hash 必须是 64 位小写 hex: %q", plugin.SourceHash())
	}
}

func TestCompileSyntaxErrorHasLine(t *testing.T) {
	source := fixture{Append: "function ("}.Source()
	issues := compileIssues(t, source, CompileOptions{})
	if !issues.hasRule("syntax_error") {
		t.Fatalf("期望 syntax_error，实际 %v", issues)
	}
	if !strings.Contains(issues[0].Message, "plugin.js:") {
		t.Fatalf("语法错误必须带行号: %q", issues[0].Message)
	}
}

func TestCompileMissingExport(t *testing.T) {
	issues := compileIssues(t, "var unused = 1;", CompileOptions{})
	if !issues.hasRule("compile_failed") {
		t.Fatalf("期望 compile_failed，实际 %v", issues)
	}
}

func TestCompileUnknownVariableHasLine(t *testing.T) {
	source := fixture{Prepend: "notDefinedAnywhere();"}.Source()
	issues := compileIssues(t, source, CompileOptions{})
	if !issues.hasRule("runtime_error") {
		t.Fatalf("期望 runtime_error，实际 %v", issues)
	}
	if !strings.Contains(issues[0].Message, "第 1 行") {
		t.Fatalf("运行期异常必须带行号: %q", issues[0].Message)
	}
}

func TestCompileSandboxRejectsForbiddenIdentifiers(t *testing.T) {
	cases := []string{
		`fetch("https://evil.invalid");`,
		`require("fs");`,
		`async function run() {}`,
		`await something;`,
		`process.env.SECRET;`,
	}
	for _, prepend := range cases {
		t.Run(prepend, func(t *testing.T) {
			issues := compileIssues(t, fixture{Prepend: prepend}.Source(), CompileOptions{})
			if !issues.hasRule("sandbox_forbidden") {
				t.Fatalf("期望 sandbox_forbidden，实际 %v", issues)
			}
		})
	}
}

func TestCompileSandboxIgnoresStringsAndComments(t *testing.T) {
	// 描述文案/注释里出现这些词不应误伤：扫描跳过字符串与注释。
	source := fixture{
		Prepend: "// fetch is forbidden by the host\n/* require() too */",
		EnDesc:  "Async video generation unit price",
	}.Source()
	if _, err := Compile(context.Background(), source, CompileOptions{}); err != nil {
		t.Fatalf("字符串/注释中的关键字不应被拒绝: %v", err)
	}
}

func TestCompileTimeout(t *testing.T) {
	source := fixture{Prepend: "while (true) {}"}.Source()
	start := time.Now()
	issues := compileIssues(t, source, CompileOptions{Timeout: 100 * time.Millisecond})
	elapsed := time.Since(start)
	if !issues.hasRule("timeout") {
		t.Fatalf("期望 timeout，实际 %v", issues)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("超时未被及时中断: %v", elapsed)
	}
}

func TestCompileKeyMismatch(t *testing.T) {
	issues := compileIssues(t, fixtureWithKey("other"), CompileOptions{ExpectedKey: "sample"})
	if !issues.hasRule("key_mismatch") {
		t.Fatalf("期望 key_mismatch，实际 %v", issues)
	}
}

func TestCompileMissingRequiredHook(t *testing.T) {
	source := fixture{Exports: map[string]string{"buildQueryRequest": ""}}.Source()
	issues := compileIssues(t, source, CompileOptions{})
	if !issues.hasRule("hook_missing") {
		t.Fatalf("期望 hook_missing，实际 %v", issues)
	}
}

func TestCompileBatchHooksRequired(t *testing.T) {
	source := fixture{FetchMode: "batch"}.Source()
	issues := compileIssues(t, source, CompileOptions{})
	rules := issues.rulesOf()
	if _, ok := rules["hook_missing"]; !ok {
		t.Fatalf("batch 必须要求 buildBatchQueryRequest/parseBatchResult: %v", issues)
	}
	// batch 插件补充批量钩子后应通过（buildQueryRequest 保留也无妨）。
	source = fixture{FetchMode: "batch", Exports: map[string]string{
		"buildBatchQueryRequest": `function (ctx) { return { url: "https://example.invalid/batch", method: "POST" }; }`,
		"parseBatchResult":       `function (ctx, body) { return []; }`,
	}}.Source()
	if _, err := Compile(context.Background(), source, CompileOptions{}); err != nil {
		t.Fatalf("batch 插件应通过: %v", err)
	}
}

func TestCompileArtifactHooksMustPair(t *testing.T) {
	source := fixture{Exports: map[string]string{
		"listArtifacts": `function (ctx) { return []; }`,
	}}.Source()
	issues := compileIssues(t, source, CompileOptions{})
	if !issues.hasRule("hook_pair") {
		t.Fatalf("期望 hook_pair，实际 %v", issues)
	}
}

func TestCompileD7WarningDoesNotBlock(t *testing.T) {
	source := fixture{
		UsageSchema: `{
    seconds: { type: "number", unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价", fr: "Prix de generation video" } }
  }`,
	}.Source()
	plugin := mustCompile(t, source, CompileOptions{})
	if !plugin.Warnings().hasRule("D7") {
		t.Fatalf("非 en/zh locale 应产生 D7 告警: %v", plugin.Warnings())
	}
}
