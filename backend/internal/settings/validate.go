package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ValidationError 是写入期的字段/整段校验失败（API 映射 400）。
type ValidationError struct {
	Key     string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Key == "" {
		return "settings: " + e.Message
	}
	return fmt.Sprintf("settings: %s: %s", e.Key, e.Message)
}

func invalid(key, format string, args ...any) error {
	return &ValidationError{Key: key, Message: fmt.Sprintf(format, args...)}
}

// normalizeScalars 把 json.Number 递归转换成 int64（整数）或 float64（小数）。
// 整数优先：bigint 配置（赠送额度上限 10^12）不能经过 float64 丢精度。
func normalizeScalars(value any) any {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer
		}
		if number, err := typed.Float64(); err == nil {
			return number
		}
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = normalizeScalars(item)
		}
		return typed
	case map[string]any:
		for key, item := range typed {
			typed[key] = normalizeScalars(item)
		}
		return typed
	default:
		return value
	}
}

// decodeJSON 用 UseNumber 保留整数精度：默认 float64 会把 10^12 量级的
// bigint 配置静默舍入（modules-03 的赠送额度上限就是 10^12）。
func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	// 拒绝尾随内容："1 2" 不是合法值。
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return value, nil
}

// normalizeRaw 按注册表声明的类型校验并归一化一个写入值。
func normalizeRaw(s Setting, raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, invalid(s.Key, "value must not be null")
	}
	value, err := decodeJSON(trimmed)
	if err != nil {
		return nil, invalid(s.Key, "invalid JSON value")
	}
	// json.Number → int64/float64 统一在入口完成：列表元素与对象内数字都由
	// 各校验器按原生类型判断，避免每个校验器各写一份 json.Number 分支。
	value = normalizeScalars(value)

	switch s.Type {
	case TypeBool:
		if _, ok := value.(bool); !ok {
			return nil, invalid(s.Key, "expected bool")
		}
	case TypeInt:
		if _, ok := value.(int64); !ok {
			return nil, invalid(s.Key, "expected integer")
		}
	case TypeNumber:
		switch number := value.(type) {
		case int64:
			value = float64(number)
		case float64:
		default:
			return nil, invalid(s.Key, "expected number")
		}
	case TypeString, TypeEnum:
		if _, ok := value.(string); !ok {
			return nil, invalid(s.Key, "expected string")
		}
	case TypeList:
		if _, ok := value.([]any); !ok {
			return nil, invalid(s.Key, "expected array")
		}
	case TypeObject:
		if _, ok := value.(map[string]any); !ok {
			return nil, invalid(s.Key, "expected object")
		}
	default:
		return nil, invalid(s.Key, "unsupported registry type %q", s.Type)
	}

	if s.Normalize != nil {
		value, err = s.Normalize(value)
		if err != nil {
			return nil, invalid(s.Key, "%v", err)
		}
	}
	if err := validateValue(s, value); err != nil {
		return nil, err
	}
	return value, nil
}

func validateValue(s Setting, value any) error {
	switch s.Type {
	case TypeInt:
		n := value.(int64)
		if s.MinInt != nil && n < *s.MinInt {
			return invalid(s.Key, "must be >= %d", *s.MinInt)
		}
		if s.MaxInt != nil && n > *s.MaxInt {
			return invalid(s.Key, "must be <= %d", *s.MaxInt)
		}
	case TypeNumber:
		f := value.(float64)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return invalid(s.Key, "must be a finite number")
		}
		if s.MinFloat != nil && f < *s.MinFloat {
			return invalid(s.Key, "must be >= %v", *s.MinFloat)
		}
		if s.MaxFloat != nil && f > *s.MaxFloat {
			return invalid(s.Key, "must be <= %v", *s.MaxFloat)
		}
	case TypeString, TypeEnum:
		str := value.(string)
		if s.MaxLen > 0 && utf8.RuneCountInString(str) > s.MaxLen {
			return invalid(s.Key, "must be at most %d characters", s.MaxLen)
		}
		if s.Type == TypeEnum && !contains(s.Allowed, str) {
			return invalid(s.Key, "must be one of %s", strings.Join(s.Allowed, ", "))
		}
		if s.Pattern != nil && !s.Pattern.MatchString(str) {
			return invalid(s.Key, "invalid format")
		}
	case TypeList:
		items := value.([]any)
		if s.Element != nil {
			for i, item := range items {
				if err := s.Element(item); err != nil {
					return invalid(s.Key, "element %d: %v", i, err)
				}
			}
		}
	}
	if s.Validate != nil {
		if err := s.Validate(value); err != nil {
			return invalid(s.Key, "%v", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 通用校验器
// ---------------------------------------------------------------------------

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func nonEmptyTrimmed(key string) func(any) error {
	return func(v any) error {
		if strings.TrimSpace(v.(string)) == "" {
			return fmt.Errorf("%s must not be empty", key)
		}
		return nil
	}
}

// trimTrailingSlash 统一服务地址口径（F-08-01：末尾去斜杠）。
func trimTrailingSlash(v any) (any, error) {
	return strings.TrimRight(strings.TrimSpace(v.(string)), "/"), nil
}

func absoluteHTTPURL(v any) error {
	return validateAbsoluteHTTPURL(v)
}

// nonEmptyAbsoluteHTTPURL 用于必填的服务地址：空串也不合法。
func nonEmptyAbsoluteHTTPURL(v any) error {
	if strings.TrimSpace(v.(string)) == "" {
		return errors.New("must not be empty")
	}
	return validateAbsoluteHTTPURL(v)
}

// validateAbsoluteHTTPURL 只接受 http(s) 绝对 URL；javascript:/data: 一律拒绝
// （§3.1 的验收：非 http(s) 或含 javascript: 前后端均拒绝）。
func validateAbsoluteHTTPURL(v any) error {
	raw, ok := v.(string)
	if !ok {
		return errors.New("expected string")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil // 可空字段由调用方决定；空值本身不是 URL 语法错误
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return errors.New("must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// urlOrRelative 接受 http(s) 绝对 URL 或站内 / 开头的相对路径。
func urlOrRelative(v any) error {
	raw := strings.TrimSpace(v.(string))
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "/") {
		if strings.HasPrefix(raw, "//") {
			return errors.New("protocol-relative URLs are not allowed")
		}
		return nil
	}
	return validateAbsoluteHTTPURL(raw)
}

func ianaTimezone(v any) error {
	name := strings.TrimSpace(v.(string))
	if name == "" {
		return errors.New("timezone must not be empty")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return errors.New("invalid IANA timezone")
	}
	return nil
}

func hostnameOnly(v any) error {
	raw := strings.TrimSpace(v.(string))
	if raw == "" {
		return nil // 开启 passkey 时由 section 校验要求必填
	}
	if strings.ContainsAny(raw, "/:#@ \t") {
		return errors.New("must be a bare hostname")
	}
	return nil
}

var domainPattern = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

func validateDomain(v any) error {
	raw, ok := v.(string)
	if !ok {
		return errors.New("expected string")
	}
	if !domainPattern.MatchString(strings.ToLower(strings.TrimSpace(raw))) {
		return errors.New("invalid domain")
	}
	return nil
}

func validateDomainOrWildcard(v any) error { return validateDomain(v) }

func validateSensitiveWord(v any) error {
	word, ok := v.(string)
	if !ok {
		return errors.New("expected string")
	}
	if strings.TrimSpace(word) == "" {
		return errors.New("word must not be empty")
	}
	if utf8.RuneCountInString(word) > 64 {
		return errors.New("word must be at most 64 characters")
	}
	return nil
}

func portElement(v any) error {
	n, ok := v.(int64)
	if !ok {
		return errors.New("expected integer port")
	}
	if n < 1 || n > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func positiveIntElement(v any) error {
	n, ok := v.(int64)
	if !ok {
		return errors.New("expected integer")
	}
	if n < 1 || n > 100000 {
		return errors.New("must be between 1 and 100000")
	}
	return nil
}

func emailOrEmpty(v any) error {
	raw := strings.TrimSpace(v.(string))
	if raw == "" {
		return nil
	}
	if _, err := mail.ParseAddress(raw); err != nil {
		return errors.New("invalid email address")
	}
	return nil
}

func positiveFiniteNumber(v any) error {
	f, ok := v.(float64)
	if !ok {
		return errors.New("expected number")
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return errors.New("must be a finite number > 0")
	}
	return nil
}

func validateOSSCredit(v any) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return errors.New("expected object")
	}
	allowed := map[string]struct{}{"name": {}, "url": {}, "license": {}}
	for key := range obj {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unexpected field %q", key)
		}
	}
	name, _ := obj["name"].(string)
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 64 {
		return errors.New("name is required and must be at most 64 characters")
	}
	if rawURL, ok := obj["url"].(string); !ok || validateAbsoluteHTTPURL(rawURL) != nil {
		return errors.New("url must be an absolute http(s) URL")
	}
	license, _ := obj["license"].(string)
	if utf8.RuneCountInString(license) > 32 {
		return errors.New("license must be at most 32 characters")
	}
	return nil
}

func boolObjectKeys(keys ...string) func(any) error {
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	return func(v any) error {
		obj, ok := v.(map[string]any)
		if !ok {
			return errors.New("expected object")
		}
		for key, value := range obj {
			if _, ok := allowed[key]; !ok {
				return fmt.Errorf("unexpected field %q", key)
			}
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("field %q must be bool", key)
			}
		}
		return nil
	}
}

func maxItems(n int, key string) func(any) error {
	return func(v any) error {
		if len(v.([]any)) > n {
			return fmt.Errorf("%s must contain at most %d items", key, n)
		}
		return nil
	}
}

// normalizeIntList 对充值档位去重并升序（F-08-15：去重排序）。
func normalizeIntList(v any) (any, error) {
	items := v.([]any)
	seen := make(map[int64]struct{}, len(items))
	out := make([]any, 0, len(items))
	for _, item := range items {
		n, ok := item.(int64)
		if !ok {
			return nil, errors.New("expected integer array")
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].(int64) < out[j].(int64) })
	return out, nil
}

// ---------------------------------------------------------------------------
// section 级交叉校验
// ---------------------------------------------------------------------------

// sectionValidator 接收「默认值 + 已存值 + 本次新值」合并后的完整视图与
// 已配置密钥集合，实现单键校验无法表达的约束（§1.3「section 整体校验」）。
type sectionValidator func(values map[string]any, secretConfigured map[string]bool) error

var sectionValidators = map[string]sectionValidator{
	"auth":           validateAuthSection,
	"auth.turnstile": validateTurnstileSection,
	"pay":            validatePaySection,
	"notify.smtp":    validateSMTPSection,
	"rl.health":      validateRLHealthSection,
	"sec":            validateSecSection,
}

func validateAuthSection(values map[string]any, _ map[string]bool) error {
	// 通行密钥开启时 RP ID 必填（F-08-23）。
	if values["auth.passkey.enabled"] == true {
		if strings.TrimSpace(asString(values["auth.passkey.rp_id"])) == "" {
			return invalid("auth.passkey.rp_id", "required when passkey login is enabled")
		}
	}
	return nil
}

func validateTurnstileSection(values map[string]any, secretConfigured map[string]bool) error {
	if values["auth.turnstile.enabled"] != true {
		return nil
	}
	if strings.TrimSpace(asString(values["auth.turnstile.site_key"])) == "" {
		return invalid("auth.turnstile.site_key", "required when Turnstile is enabled")
	}
	if !secretConfigured["auth.turnstile.secret_key"] {
		return invalid("auth.turnstile.secret_key", "required when Turnstile is enabled")
	}
	return nil
}

func validatePaySection(values map[string]any, _ map[string]bool) error {
	// 档位不得低于任一已启用支付方式的最低充值（F-08-15）。
	minTopup := int64(0)
	for _, key := range []string{
		"pay.min_topup_cny", "pay.stripe.min_topup_cny",
		"pay.waffo.min_topup_cny", "pay.waffo_pancake.min_topup_cny",
	} {
		if n, ok := values[key].(int64); ok && n > minTopup {
			minTopup = n
		}
	}
	if presets, ok := values["pay.amount_presets_cny"].([]any); ok && minTopup > 0 {
		for _, item := range presets {
			if n, ok := item.(int64); ok && n < minTopup {
				return invalid("pay.amount_presets_cny",
					"preset %d is below the minimum top-up %d", n, minTopup)
			}
		}
	}
	return nil
}

func validateSMTPSection(values map[string]any, _ map[string]bool) error {
	// from_email 留空回退 username（G7-R41）；两者皆空且已开启邮箱验证时仅警告，
	// 不阻断保存（§3.6 F-08-19）。
	return nil
}

func validateRLHealthSection(values map[string]any, _ map[string]bool) error {
	// 响应时间阈值 >0 已由键级校验覆盖；此处保留 section 钩子以便未来
	// 「关键词表整行为空 → 忽略该行」一类约束集中落点。
	return nil
}

func validateSecSection(values map[string]any, _ map[string]bool) error {
	// allowlist 模式且清单为空 → fail-closed（F-08-17 TC-05）。允许保存：
	// 这是管理员显式选择的安全状态，拒绝保存反而会让关闭防护无从下手。
	return nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
