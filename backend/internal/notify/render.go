package notify

// 渲染契约（§3.9.3 / §3.9.6a）：
//   - 只允许 {{.name}} 字段访问；管道 / 函数 / 条件 / 循环一律拒绝；
//   - 每个 key×channel 的变量集是封闭白名单；
//   - 只做**纯文本替换**，不做格式化、不做 HTML 消毒（格式化由服务端在注入
//     前完成，P2；Markdown 渲染属前端/邮件壳的职责，P3）；
//   - 必填变量缺失 → fail-closed（P1），绝不空串顶替。

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 允许的变量构造：{{.name}}（允许大括号内空白）。
var fieldPattern = regexp.MustCompile(`\{\{\s*\.([a-z][a-z0-9_]*)\s*\}\}`)

// 任意模板构造（用于找出字段访问之外的一切 {{...}}）。
var constructPattern = regexp.MustCompile(`\{\{(?:[^{}]|\{[^{}]*\})*\}\}`)

// 长度上限与表约束一致（§3.9.2）：subject ≤256、body ≤16KB、CTA 文案 ≤32。
const (
	MaxSubjectRunes = 256
	MaxBodyRunes    = 16384
	MaxCTATextRunes = 32
)

// ErrUnknownVariable：模板里出现白名单外变量（422）。
type ErrUnknownVariable struct {
	Variable string
}

func (e *ErrUnknownVariable) Error() string {
	return "notify: unknown variable {{." + e.Variable + "}}"
}

// ErrInvalidConstruct：出现字段访问之外的模板构造（管道/函数/条件/循环等）。
type ErrInvalidConstruct struct {
	Construct string
}

func (e *ErrInvalidConstruct) Error() string {
	return "notify: unsupported template construct " + e.Construct
}

// ErrMissingVariable：渲染期必填变量缺失（P1，fail-closed）。
type ErrMissingVariable struct {
	Variable string
}

func (e *ErrMissingVariable) Error() string {
	return "missing_var:" + e.Variable
}

// ErrInvalidValue：变量值类型不符合声明（例如 int 变量收到对象）。
type ErrInvalidValue struct {
	Variable string
	Detail   string
}

func (e *ErrInvalidValue) Error() string {
	return "notify: invalid value for {{." + e.Variable + "}}: " + e.Detail
}

// ValidateContent 静态校验一份模板内容。返回的错误由 HTTP 层映射 422。
// 校验顺序刻意先查构造、再查白名单：`{{.x | upper}}` 应报「非字段访问构造」，
// 而不是「未知变量 x」——后者会误导管理员删掉变量。
func ValidateContent(t Template, subject, body string, ctaText, ctaURL string) error {
	if t.Channel == ChannelInApp && strings.TrimSpace(subject) != "" {
		return fmt.Errorf("notify: inapp template %s must not have a subject", t.Key)
	}
	if t.Channel == ChannelEmail && strings.TrimSpace(subject) == "" {
		return fmt.Errorf("notify: email template %s requires a subject", t.Key)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("notify: template %s body must not be empty", t.Key)
	}
	if utf8.RuneCountInString(subject) > MaxSubjectRunes {
		return fmt.Errorf("notify: subject exceeds %d characters", MaxSubjectRunes)
	}
	if utf8.RuneCountInString(body) > MaxBodyRunes {
		return fmt.Errorf("notify: body exceeds %d characters", MaxBodyRunes)
	}
	if utf8.RuneCountInString(ctaText) > MaxCTATextRunes {
		return fmt.Errorf("notify: CTA text exceeds %d characters", MaxCTATextRunes)
	}
	if (ctaText == "") != (ctaURL == "") {
		return fmt.Errorf("notify: CTA text and URL must be provided together")
	}
	if ctaURL != "" && !validCTAURL(ctaURL) {
		return fmt.Errorf("notify: CTA URL must start with http:// or https://")
	}
	for _, field := range []string{subject, body, ctaText, ctaURL} {
		if err := validateVariables(t, field); err != nil {
			return err
		}
	}
	return nil
}

func isHTTPURL(raw string) bool {
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

// validCTAURL 接受字面 http(s) 链接，或**整个 URL 恰是一个变量**
// （设计 §3.9.3 的 CTA 列就是 `{{.reset_url}}` 这种形态）。变量的实际值
// 在渲染后由 RenderContent 再校验一次，避免把 javascript: 之类注入按钮。
func validCTAURL(raw string) bool {
	if isHTTPURL(raw) {
		return true
	}
	matches := fieldPattern.FindAllString(raw, -1)
	if len(matches) != 1 {
		return false
	}
	return strings.TrimSpace(fieldPattern.ReplaceAllString(raw, "")) == ""
}

// validateVariables 用一个文本字段做白名单校验。
func validateVariables(t Template, text string) error {
	for _, construct := range constructPattern.FindAllString(text, -1) {
		// 整个 {{...}} 必须正好等于 {{.name}}：`{{{.code}}}` 这类额外花括号
		// 形态若只用 FindStringSubmatch 会被内部子串蒙混过关（F8 评审）。
		if fieldPattern.FindString(construct) != construct {
			return &ErrInvalidConstruct{Construct: construct}
		}
		match := fieldPattern.FindStringSubmatch(construct)
		if match == nil {
			return &ErrInvalidConstruct{Construct: construct}
		}
		if _, ok := t.VariableByName(match[1]); !ok {
			return &ErrUnknownVariable{Variable: match[1]}
		}
	}
	// 剥离合法字段后，任何残留花括号都说明正文含未闭合/嵌套/额外括号构造。
	// 模板只做字段访问，不允许把花括号当普通文本使用。
	stripped := fieldPattern.ReplaceAllString(text, "")
	if strings.ContainsAny(stripped, "{}") {
		return &ErrInvalidConstruct{Construct: strings.TrimSpace(stripped)}
	}
	return nil
}

// Rendered 是一次渲染结果。Locale 是**实际命中的行**（回退后），便于审计与
// 排障区分「请求语言」与「真正发送的语言」。
type Rendered struct {
	Subject string
	Body    string
	CTAText string
	CTAURL  string
	Locale  Locale
}

// RenderContent 按白名单变量做纯文本替换。vars 里未声明的键被忽略；声明为
// 必填但缺失的变量返回 ErrMissingVariable。可选变量缺失按空串渲染（这正是
// 设计对 welcome_bonus 的约定，§3.9.4 末）。
func RenderContent(t Template, content Content, locale Locale, vars map[string]any) (Rendered, error) {
	rendered := Rendered{Locale: locale}
	for _, field := range []struct {
		name string
		text string
		dst  *string
	}{
		{"subject", content.Subject, &rendered.Subject},
		{"body", content.Body, &rendered.Body},
		{"cta_text", content.CTAText, &rendered.CTAText},
		{"cta_url", content.CTAURL, &rendered.CTAURL},
	} {
		value, err := substitute(t, field.text, vars)
		if err != nil {
			return Rendered{}, err
		}
		*field.dst = value
	}
	// CTA 链接的运行期兜底：模板只写了 {{.some_url}} 时，静态校验无法知道
	// 值是不是 http(s)，这里对替换后的结果再验一次（P3 无裸 HTML 面）。
	if t.Channel == ChannelEmail && rendered.CTAURL != "" && !isHTTPURL(rendered.CTAURL) {
		return Rendered{}, fmt.Errorf("notify: CTA URL must start with http:// or https://")
	}
	return rendered, nil
}

// substitute 替换单个文本字段里的全部 {{.name}}。
func substitute(t Template, text string, vars map[string]any) (string, error) {
	var firstErr error
	out := fieldPattern.ReplaceAllStringFunc(text, func(construct string) string {
		if firstErr != nil {
			return construct
		}
		name := fieldPattern.FindStringSubmatch(construct)[1]
		spec, ok := t.VariableByName(name)
		if !ok {
			// 静态校验应当已经拦截；运行期出现说明库里的行被绕过校验写入，
			// 仍按 fail-closed 处理（不渲染未知变量）。
			firstErr = &ErrUnknownVariable{Variable: name}
			return construct
		}
		raw, exists := vars[name]
		if !exists || raw == nil {
			if spec.Required {
				firstErr = &ErrMissingVariable{Variable: name}
				return construct
			}
			return ""
		}
		value, err := formatValue(spec, raw)
		if err != nil {
			firstErr = err
			return construct
		}
		return value
	})
	if firstErr != nil {
		return "", firstErr
	}
	// 替换后仍残留构造 ⇒ 模板正文含未闭合/非字段访问语法；同样不发送。
	if err := validateVariables(t, text); err != nil {
		return "", err
	}
	return out, nil
}

// formatValue 把调用方值转成注入字符串。数值与时间必须由业务方**预格式化**
// （P2），这里只处理 JSON 解码后不可避免的数字类型差异：
// 整数值输出为整数串（不出现 1e+06 或 .0），其余按十进制最短表示。
func formatValue(spec Variable, raw any) (string, error) {
	switch value := raw.(type) {
	case string:
		// int 变量的样例值以字符串存储（便于文档展示），但调用方输入必须
		// 真的是整数：`"abc"` 不能让渲染静默通过（F7 评审）。
		if spec.Type == VarInt {
			if _, err := strconv.ParseInt(value, 10, 64); err != nil {
				return "", &ErrInvalidValue{Variable: spec.Name, Detail: "expected an integer"}
			}
		}
		return value, nil
	case json.Number:
		return formatNumber(spec, value.String())
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", &ErrInvalidValue{Variable: spec.Name, Detail: "non-finite number"}
		}
		if value == math.Trunc(value) && math.Abs(value) < 1e15 {
			return strconv.FormatInt(int64(value), 10), nil
		}
		return strconv.FormatFloat(value, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(value), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case bool:
		return strconv.FormatBool(value), nil
	default:
		return "", &ErrInvalidValue{Variable: spec.Name, Detail: fmt.Sprintf("unsupported type %T", raw)}
	}
}

func formatNumber(spec Variable, raw string) (string, error) {
	if spec.Type == VarInt {
		if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
			return "", &ErrInvalidValue{Variable: spec.Name, Detail: "expected an integer"}
		}
		return raw, nil
	}
	return raw, nil
}

// ValidateVariables 在渲染前校验调用方传入的变量：必填存在且类型合法。
// 与 RenderContent 的差别：它可以对**未在正文使用**的必填变量也报错（例如
// 调用方漏传 code，而管理员把验证码从正文删掉了——仍应视为业务错误）。
func ValidateVariables(t Template, vars map[string]any) error {
	for _, spec := range t.Variables {
		raw, exists := vars[spec.Name]
		if !exists || raw == nil {
			if spec.Required {
				return &ErrMissingVariable{Variable: spec.Name}
			}
			continue
		}
		if _, err := formatValue(spec, raw); err != nil {
			return err
		}
	}
	return nil
}

// MergeVariables 以样例值为底、调用方值为覆盖，生成预览/测试发送的变量集。
// 必填变量若调用方不传则沿用样例值——预览的语义就是「用样例值看效果」
// （§3.9.6a「预览用样例值表」），而真实发送必须走 ValidateVariables。
func MergeVariables(t Template, locale Locale, vars map[string]any) map[string]any {
	out := t.SampleVariables(locale)
	for name, value := range vars {
		out[name] = value
	}
	return out
}
