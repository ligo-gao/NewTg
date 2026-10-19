package plugin

import (
	"fmt"
	"strings"
)

// Issue 是插件加载期校验的**结构化错误**（relay-06 §4.3）。
//
// 形状冻结为 `{key, fieldPath, locale, rule, message}`：管理端与
// `runtime/status` 直接投影该结构，不做二次拼装。`Rule` 取设计编号
// （T1–T10 / D1–D7）或运行期规则名（syntax_error / hook_missing / ...），
// 使 UI 能按规则聚合，也让测试可以断言「拒绝的原因」而不是错误文案。
type Issue struct {
	Key       string `json:"key"`
	FieldPath string `json:"fieldPath"`
	Locale    string `json:"locale,omitempty"`
	Rule      string `json:"rule"`
	Message   string `json:"message"`
}

// Error 实现 error；单条 Issue 很少单独作为 error 使用，但便于 %w 包装。
func (i Issue) Error() string {
	var b strings.Builder
	b.WriteString(i.Key)
	if i.FieldPath != "" {
		b.WriteString(":")
		b.WriteString(i.FieldPath)
	}
	if i.Locale != "" {
		b.WriteString("[")
		b.WriteString(i.Locale)
		b.WriteString("]")
	}
	b.WriteString(" (")
	b.WriteString(i.Rule)
	b.WriteString(") ")
	b.WriteString(i.Message)
	return b.String()
}

// Issues 是一组结构化错误；作为 error 返回时逐条列出。
type Issues []Issue

// Error 实现 error。仅用于日志/诊断；结构化消费方应直接遍历切片。
func (is Issues) Error() string {
	if len(is) == 0 {
		return "plugin: no issues"
	}
	parts := make([]string, len(is))
	for idx, issue := range is {
		parts[idx] = issue.Error()
	}
	return strings.Join(parts, "; ")
}

// HasErrors 表达「是否拒绝加载」的语义。空切片视为通过。
func (is Issues) HasErrors() bool { return len(is) > 0 }

// hasRule 判断是否包含某条规则（测试与状态分类用）。
func (is Issues) hasRule(rule string) bool {
	for _, issue := range is {
		if issue.Rule == rule {
			return true
		}
	}
	return false
}

// rulesOf 返回出现过的规则集合，便于测试断言「未误报」。
func (is Issues) rulesOf() map[string]struct{} {
	out := make(map[string]struct{}, len(is))
	for _, issue := range is {
		out[issue.Rule] = struct{}{}
	}
	return out
}

// issuef 是构造 Issue 的内部便利函数；locale 为空表示字段级（非 localized）错误。
func issuef(key, fieldPath, locale, rule, format string, args ...any) Issue {
	return Issue{
		Key:       key,
		FieldPath: fieldPath,
		Locale:    locale,
		Rule:      rule,
		Message:   fmt.Sprintf(format, args...),
	}
}
