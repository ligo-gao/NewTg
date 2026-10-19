package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 本文件是 T3 复核（With-fixes）的回归用例：每条对应一个裁决/问题编号。

// H1：key 字符集与 DB CHECK 同源（^[a-z0-9-]{1,30}$）。
func TestReviewKeyCharsetMatchesDB(t *testing.T) {
	for _, key := range []string{"Sample", "sam.ple", "sam_ple", "sample "} {
		t.Run(key, func(t *testing.T) {
			issues := compileIssues(t, fixture{Key: key}.Source(), CompileOptions{})
			requireRule(t, issues, "T1")
		})
	}
	// 合规边界：30 字符、全小写/数字/连字符。
	valid := "a" + strings.Repeat("b", 29)
	if len(valid) != 30 {
		t.Fatalf("构造错误")
	}
	if _, err := Compile(t.Context(), fixture{Key: valid}.Source(), CompileOptions{}); err != nil {
		t.Fatalf("30 字符合法 key 应通过: %v", err)
	}
}

// M1：D5/D6 的 locale 比较按归一化小写。
func TestReviewLocaleCaseNormalized(t *testing.T) {
	schema := `{
    seconds: { type: "number", unit: "second",
      description: { EN: "Video generation price", zh: "视频生成单价" } }
  }`
	issues := compileIssues(t, fixture{UsageSchema: schema}.Source(), CompileOptions{})
	requireRule(t, issues, "D5")

	// 大写 locale 的合规描述必须通过（不能因为大小写产生假阴/假阳）。
	okSchema := `{
    seconds: { type: "number", unit: "second",
      description: { EN: "Video generation unit price", ZH: "视频生成单价" } }
  }`
	if _, err := Compile(t.Context(), fixture{UsageSchema: okSchema}.Source(), CompileOptions{}); err != nil {
		t.Fatalf("大写 locale 的合规描述应通过: %v", err)
	}
}

// M2：D3 仅对数值字段（strict）生效。
func TestReviewD3OnlyForNumericFields(t *testing.T) {
	schema := `{
    generate_audio: { type: "boolean", description: { en: "Generate 2 variants", zh: "生成音频" } }
  }`
	if _, err := Compile(t.Context(), fixture{UsageSchema: schema}.Source(), CompileOptions{}); err != nil {
		t.Fatalf("布尔描述含数字不应被 D3 拒绝: %v", err)
	}

	// 数值字段仍受 D3 约束。
	badSchema := `{
    seconds: { type: "number", unit: "second",
      description: { en: "Video 2 generation unit price", zh: "视频生成单价" } }
  }`
	issues := compileIssues(t, fixture{UsageSchema: badSchema}.Source(), CompileOptions{})
	requireRule(t, issues, "D3")
}

// M3（R-M6D-15）：同 key 多 active → compile_failed，错误码 multiple_active_versions。
func TestReviewMultipleActiveVersions(t *testing.T) {
	v1 := activeVersion("sample", "1.0.0", fixture{}.Source())
	v2 := activeVersion("sample", "2.0.0", fixture{Version: "2.0.0"}.Source())
	m := newManager(t, newFakeLoader(v1, v2), Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry := m.Snapshot().Lookup("sample")
	if entry.Status != StatusCompileFailed {
		t.Fatalf("多 active 必须 fail-closed: %+v", entry)
	}
	if !entry.Errors.hasRule("multiple_active_versions") {
		t.Fatalf("错误码必须为 multiple_active_versions: %v", entry.Errors)
	}
	if entry.Errors.hasRule("duplicate_active") {
		t.Fatalf("旧错误码不应再出现")
	}
}

// M5：manifest 层长度与 DB CHECK 同值。
func TestReviewManifestLengthLimits(t *testing.T) {
	t.Run("name", func(t *testing.T) {
		issues := compileIssues(t, fixture{Name: strings.Repeat("n", 129)}.Source(), CompileOptions{})
		requireRule(t, issues, "meta_name_len")
	})
	t.Run("version", func(t *testing.T) {
		version := "1.0.0-" + strings.Repeat("a", 27) // 33 字符且仍是 semver
		if len(version) != 33 {
			t.Fatalf("构造错误: %d", len(version))
		}
		issues := compileIssues(t, fixture{Version: version}.Source(), CompileOptions{})
		requireRule(t, issues, "meta_version_len")
	})
	t.Run("icon", func(t *testing.T) {
		issues := compileIssues(t, fixture{ExtraMeta: map[string]string{
			"icon": `"` + strings.Repeat("i", 129) + `"`,
		}}.Source(), CompileOptions{})
		requireRule(t, issues, "meta_icon_len")
	})
	t.Run("description_total", func(t *testing.T) {
		// 每 locale 512 合规，总量 512+512+1=1025 超过 DB 的 1024。
		description := `{ en: "` + strings.Repeat("e", 512) + `", zh: "` +
			strings.Repeat("中", 512) + `", fr: "x" }`
		issues := compileIssues(t, fixture{DescriptionJS: description}.Source(), CompileOptions{})
		requireRule(t, issues, "meta_description_len")
	})
}

// L1：模板字符串的 ${...} 进入代码上下文扫描；纯文本不误伤。
func TestReviewTemplateLiteralInterpolation(t *testing.T) {
	issues := compileIssues(t, fixture{
		Prepend: "var s = `${async () => 1}`;",
	}.Source(), CompileOptions{})
	requireRule(t, issues, "sandbox_forbidden")

	if _, err := Compile(t.Context(), fixture{
		Prepend: "var s = `plain async process text`;",
	}.Source(), CompileOptions{}); err != nil {
		t.Fatalf("模板纯文本不应被误伤: %v", err)
	}
}

// L2：allow_outbound=true 显式拒绝。
func TestReviewDryRunOutboundUnavailable(t *testing.T) {
	m := newManager(t, nil, Options{MasterEnabled: true})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	_, err := m.DryRun(context.Background(), DryRunRequest{Key: "sample", AllowOutbound: true})
	issues, ok := err.(Issues)
	if !ok || !issues.hasRule("dryrun_outbound_unavailable") {
		t.Fatalf("allow_outbound=true 必须显式拒绝: %v", err)
	}
}

// L3：Issue.Locale 保持冻结形状（无 omitempty）。
func TestReviewIssueJSONShape(t *testing.T) {
	raw, err := json.Marshal(Issue{Key: "k", FieldPath: "p", Rule: "r", Message: "m"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["locale"]; !ok {
		t.Fatalf("locale 字段必须恒在响应形状中: %s", raw)
	}
}

// L6：auth 对象的未知字段显式拒绝。
func TestReviewAuthUnknownField(t *testing.T) {
	issues := compileIssues(t, fixture{Auth: `{ type: "api_key", foo: "bar" }`}.Source(), CompileOptions{})
	requireRule(t, issues, "meta_unknown_field")
}

// 复核回归：默认超时仍为 5s。
func TestReviewDefaultTimeout(t *testing.T) {
	if DefaultTimeout != 5*time.Second {
		t.Fatalf("DefaultTimeout=%v", DefaultTimeout)
	}
}
