// Package notify 实现通知模板注册表、模板存储、渲染回退与投递记录。
//
// 设计权威：docs/modules-08-系统管理与设置-子功能设计.md §3.9：
//   - §3.9.1 16 个 category 的封闭注册表（key 与 category 一一对应）；
//   - §3.9.2 模板数据模型（key×channel×locale，7 语 canonical）；
//   - §3.9.3 变量契约（每 key×channel 一组封闭白名单，只允许 {{.name}} 字段访问）；
//   - §3.9.4 内置文案（本包 defaults.go 是**唯一**代码真源）；
//   - §3.9.6 渲染/回退/fail-closed 与投递记录。
//
// 本包不依赖 HTTP 层；httpapi 只做参数/权限/错误映射。
package notify

import (
	"fmt"
	"sort"
	"strings"
)

// Channel 是模板载体。两处枚举差异见 migration 注释与 §3.9.2 末：
// 模板表 {email,inapp}，投递表 {email,webhook}。
type Channel string

const (
	ChannelEmail Channel = "email"
	ChannelInApp Channel = "inapp"
	// ChannelWebhook 只出现在投递记录里（payload 由 owner 定义，不走模板）。
	ChannelWebhook Channel = "webhook"
)

// Locale 是 canonical 7 语（与 users.language 同集，默认 en；G7-R14）。
type Locale string

const (
	LocaleEN   Locale = "en"
	LocaleZHCN Locale = "zh-CN"
	LocaleZHTW Locale = "zh-TW"
	LocaleFR   Locale = "fr"
	LocaleRU   Locale = "ru"
	LocaleJA   Locale = "ja"
	LocaleVI   Locale = "vi"
	// DefaultLocale 是站点默认与回退链终点（§3.9.6a：个人语言 → en）。
	DefaultLocale = LocaleEN
)

var localeOrder = []Locale{LocaleEN, LocaleZHCN, LocaleZHTW, LocaleFR, LocaleRU, LocaleJA, LocaleVI}

// Locales 返回 7 语的冻结顺序副本（前端下拉与列表排序共用同一顺序）。
func Locales() []Locale {
	out := make([]Locale, len(localeOrder))
	copy(out, localeOrder)
	return out
}

// ParseLocale 归一化请求语言。空值按站点默认 en 处理；未注册值返回 false，
// 由调用方映射 400——绝不静默替换成 en（那会让管理员的筛选看起来生效了）。
func ParseLocale(raw string) (Locale, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return DefaultLocale, true
	}
	for _, locale := range localeOrder {
		if string(locale) == value {
			return locale, true
		}
	}
	return "", false
}

// ParseChannel 归一化渠道。空值默认 email（列表/详情最常用的一侧）。
func ParseChannel(raw string) (Channel, bool) {
	switch strings.TrimSpace(raw) {
	case "":
		return ChannelEmail, true
	case string(ChannelEmail):
		return ChannelEmail, true
	case string(ChannelInApp):
		return ChannelInApp, true
	}
	return "", false
}

// VariableType 是变量声明的类型。渲染只做字符串注入，类型用于校验调用方
// 传值是否合法（int 必须是 JSON number），而不是做模板内格式化（P2）。
type VariableType string

const (
	VarString VariableType = "string"
	VarInt    VariableType = "int"
)

// Variable 是一个白名单变量的声明。Sample 是预览/测试发送的样例值
// （§3.9.3「样例值」列），必填变量在预览时缺省即 fail-closed。
type Variable struct {
	Name     string
	Type     VariableType
	Required bool
	Sample   string
}

// Content 是某个模板在某种语言下的默认文案。CTA 允许为空；inapp 的
// Subject 恒为空。
type Content struct {
	Subject string
	Body    string
	CTAText string
	CTAURL  string
}

// Template 是注册表的一行：一个 (key, channel) 组合（§3.9.3 的 18 组）。
// Category 与 Key 同名（16 个 category 各对应一个 key），但仍显式存储：
// 投递记录的 category 是独立字段，未来若出现同 category 多 key 不必改模型。
type Template struct {
	Key       string
	Channel   Channel
	Category  string
	Variables []Variable
	Defaults  map[Locale]Content
}

// Category 是 §3.9.1 的 16 个 category 之一。Locked 对应 P4：
// auth.* / security.alert / system.smtp_test / system.announcement 不可关。
type Category struct {
	Key      string
	Locked   bool
	Channels []Channel
	Keys     []string
}

// 16 个 category 的冻结清单（§3.9.1）。顺序按 auth → security → billing →
// system → ops，与文档表格一致。
var categoryOrder = []Category{
	{Key: "auth.verify_code", Locked: true, Channels: []Channel{ChannelEmail}, Keys: []string{"auth.verify_code"}},
	{Key: "auth.password_reset", Locked: true, Channels: []Channel{ChannelEmail}, Keys: []string{"auth.password_reset"}},
	// auth.welcome 在 §3.9.1 标为「事务性」而非 P4，但欢迎信无用户偏好开关；
	// 它不在 P4 锁定集（§3.9 表格明确只列 auth.* / security.alert /
	// system.smtp_test / system.announcement）。P4 的 auth.* 是通配，故锁定。
	{Key: "auth.welcome", Locked: true, Channels: []Channel{ChannelEmail}, Keys: []string{"auth.welcome"}},
	{Key: "security.alert", Locked: true, Channels: []Channel{ChannelEmail}, Keys: []string{"security.alert"}},
	{Key: "billing.balance_low", Locked: false, Channels: []Channel{ChannelEmail, ChannelInApp}, Keys: []string{"billing.balance_low"}},
	{Key: "billing.quota_exhausted", Locked: false, Channels: []Channel{ChannelEmail, ChannelInApp}, Keys: []string{"billing.quota_exhausted"}},
	{Key: "billing.usage_digest", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"billing.usage_digest"}},
	{Key: "billing.payment_success", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"billing.payment_success"}},
	{Key: "billing.payment_failed", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"billing.payment_failed"}},
	{Key: "billing.order_expired", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"billing.order_expired"}},
	{Key: "billing.redemption_success", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"billing.redemption_success"}},
	{Key: "billing.violation_fee", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"billing.violation_fee"}},
	{Key: "system.announcement", Locked: true, Channels: []Channel{ChannelInApp}, Keys: []string{"system.announcement"}},
	{Key: "system.smtp_test", Locked: true, Channels: []Channel{ChannelEmail}, Keys: []string{"system.smtp_test"}},
	// W7/M4-a（R-M4-09）：渠道自动禁用同时提供站内模板。M4 只做 inapp
	// 直投（无 SMTP 依赖）；email 模板保留但不接线（R-M4-18）。
	{Key: "ops.channel_auto_disabled", Locked: false, Channels: []Channel{ChannelEmail, ChannelInApp}, Keys: []string{"ops.channel_auto_disabled"}},
	{Key: "ops.redemption_expiring", Locked: false, Channels: []Channel{ChannelEmail}, Keys: []string{"ops.redemption_expiring"}},
}

// Categories 返回冻结注册表的副本（16 行）。
func Categories() []Category {
	out := make([]Category, len(categoryOrder))
	copy(out, categoryOrder)
	return out
}

// LookupCategory 按 category key 查注册表。
func LookupCategory(key string) (Category, bool) {
	for _, category := range categoryOrder {
		if category.Key == key {
			return category, true
		}
	}
	return Category{}, false
}

// ChannelsForKey 返回该 key 注册的渠道（注册表声明顺序）。单渠道 key 在
// 请求省略 channel 时可安全推断；多渠道 key（billing.balance_low /
// billing.quota_exhausted）必须由调用方显式指定，否则同一个 key 会指向
// 两行不同模板。
func ChannelsForKey(key string) []Channel {
	category, ok := LookupCategory(key)
	if !ok {
		return nil
	}
	out := make([]Channel, len(category.Channels))
	copy(out, category.Channels)
	return out
}

// IsRegisteredCategory 是「封闭集合」判据：未注册 category 的列表筛选/写入
// 一律 400，不做模糊匹配。
func IsRegisteredCategory(key string) bool {
	_, ok := LookupCategory(key)
	return ok
}

// templateSpecs 是 18 个 (key, channel) 组。Defaults 在 defaults.go 中以
// 初始化函数填入，避免与本文件的大段文案掺杂。
var templateSpecs = []Template{
	{Key: "auth.verify_code", Channel: ChannelEmail, Category: "auth.verify_code", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "code", Type: VarString, Required: true, Sample: "428173"},
		{Name: "expires_minutes", Type: VarInt, Required: true, Sample: "10"},
		{Name: "purpose_label", Type: VarString, Required: true, Sample: "注册验证"},
	}},
	{Key: "auth.password_reset", Channel: ChannelEmail, Category: "auth.password_reset", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "reset_url", Type: VarString, Required: true, Sample: "https://gw.example.com/reset-password?token=sample"},
		{Name: "expires_minutes", Type: VarInt, Required: true, Sample: "30"},
		{Name: "ip", Type: VarString, Sample: "203.0.113.7"},
	}},
	{Key: "auth.welcome", Channel: ChannelEmail, Category: "auth.welcome", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "site_title", Type: VarString, Required: true, Sample: "Example Gateway"},
		{Name: "welcome_bonus", Type: VarString, Sample: "我们已向你的账户赠送 1,000 积分。"},
		{Name: "dashboard_url", Type: VarString, Required: true, Sample: "https://gw.example.com/"},
	}},
	{Key: "security.alert", Channel: ChannelEmail, Category: "security.alert", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "event_title", Type: VarString, Required: true, Sample: "密码已修改"},
		{Name: "event_detail", Type: VarString, Required: true, Sample: "你的账户密码刚刚被修改。"},
		{Name: "event_time", Type: VarString, Required: true, Sample: "2026-10-14T09:12:33Z"},
		{Name: "ip", Type: VarString, Sample: "203.0.113.7"},
		{Name: "device", Type: VarString, Sample: "Chrome 141 / macOS"},
		{Name: "action_hint", Type: VarString, Required: true, Sample: "若不是你本人操作，请立即重置密码。"},
		{Name: "security_url", Type: VarString, Required: true, Sample: "https://gw.example.com/security"},
	}},
	{Key: "billing.balance_low", Channel: ChannelEmail, Category: "billing.balance_low", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "balance", Type: VarString, Required: true, Sample: "812"},
		{Name: "threshold", Type: VarString, Required: true, Sample: "1,000"},
		{Name: "topup_url", Type: VarString, Required: true, Sample: "https://gw.example.com/wallet"},
		{Name: "site_title", Type: VarString, Required: true, Sample: "Example Gateway"},
	}},
	{Key: "billing.balance_low", Channel: ChannelInApp, Category: "billing.balance_low", Variables: []Variable{
		{Name: "balance", Type: VarString, Required: true, Sample: "812"},
		{Name: "threshold", Type: VarString, Required: true, Sample: "1,000"},
		{Name: "topup_url", Type: VarString, Required: true, Sample: "/wallet"},
	}},
	{Key: "billing.quota_exhausted", Channel: ChannelEmail, Category: "billing.quota_exhausted", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "balance", Type: VarString, Required: true, Sample: "0"},
		{Name: "topup_url", Type: VarString, Required: true, Sample: "https://gw.example.com/wallet"},
		{Name: "site_title", Type: VarString, Required: true, Sample: "Example Gateway"},
	}},
	{Key: "billing.quota_exhausted", Channel: ChannelInApp, Category: "billing.quota_exhausted", Variables: []Variable{
		{Name: "balance", Type: VarString, Required: true, Sample: "0"},
		{Name: "topup_url", Type: VarString, Required: true, Sample: "/wallet"},
	}},
	{Key: "billing.usage_digest", Channel: ChannelEmail, Category: "billing.usage_digest", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "period_label", Type: VarString, Required: true, Sample: "2026-10-06 ～ 2026-10-12"},
		{Name: "total_credits", Type: VarString, Required: true, Sample: "12,480"},
		{Name: "request_count", Type: VarString, Required: true, Sample: "1,204"},
		{Name: "top_models", Type: VarString, Required: true, Sample: "gpt-6-astra / claude-5.5 / gemini-4"},
		{Name: "dashboard_url", Type: VarString, Required: true, Sample: "https://gw.example.com/logs"},
	}},
	{Key: "billing.payment_success", Channel: ChannelEmail, Category: "billing.payment_success", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "amount", Type: VarString, Required: true, Sample: "≈ ¥50.00"},
		{Name: "credits", Type: VarString, Required: true, Sample: "5,000"},
		{Name: "order_no", Type: VarString, Required: true, Sample: "20261014-8F3A21"},
		{Name: "paid_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "wallet_url", Type: VarString, Required: true, Sample: "https://gw.example.com/wallet"},
	}},
	{Key: "billing.payment_failed", Channel: ChannelEmail, Category: "billing.payment_failed", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "amount", Type: VarString, Required: true, Sample: "≈ ¥50.00"},
		{Name: "order_no", Type: VarString, Required: true, Sample: "20261014-8F3A21"},
		{Name: "reason_label", Type: VarString, Required: true, Sample: "银行卡被拒"},
		{Name: "failed_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "retry_url", Type: VarString, Required: true, Sample: "https://gw.example.com/wallet"},
	}},
	{Key: "billing.order_expired", Channel: ChannelEmail, Category: "billing.order_expired", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "order_no", Type: VarString, Required: true, Sample: "20261014-8F3A21"},
		{Name: "amount", Type: VarString, Required: true, Sample: "≈ ¥50.00"},
		{Name: "expired_at", Type: VarString, Required: true, Sample: "2026-10-14T19:33:11Z"},
		{Name: "topup_url", Type: VarString, Required: true, Sample: "https://gw.example.com/wallet"},
	}},
	{Key: "billing.redemption_success", Channel: ChannelEmail, Category: "billing.redemption_success", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "credits", Type: VarString, Required: true, Sample: "5,000"},
		{Name: "balance", Type: VarString, Required: true, Sample: "5,812"},
		{Name: "code_hint", Type: VarString, Sample: "NEW-****-3A21"},
		{Name: "redeemed_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "wallet_url", Type: VarString, Required: true, Sample: "https://gw.example.com/wallet"},
	}},
	{Key: "billing.violation_fee", Channel: ChannelEmail, Category: "billing.violation_fee", Variables: []Variable{
		{Name: "username", Type: VarString, Sample: "alice"},
		{Name: "fee", Type: VarString, Required: true, Sample: "200"},
		{Name: "reason_label", Type: VarString, Required: true, Sample: "上游内容合规拦截"},
		{Name: "occurred_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "balance", Type: VarString, Required: true, Sample: "4,800"},
		{Name: "policy_url", Type: VarString, Required: true, Sample: "https://gw.example.com/docs/acceptable-use"},
	}},
	{Key: "system.announcement", Channel: ChannelInApp, Category: "system.announcement", Variables: []Variable{
		{Name: "title", Type: VarString, Required: true, Sample: "计划维护通知"},
		{Name: "summary", Type: VarString, Required: true, Sample: "系统将于 10 月 20 日 02:00 维护。"},
		{Name: "published_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "link_url", Type: VarString, Sample: "/announcements/42"},
	}},
	{Key: "system.smtp_test", Channel: ChannelEmail, Category: "system.smtp_test", Variables: []Variable{
		{Name: "site_title", Type: VarString, Required: true, Sample: "Example Gateway"},
		{Name: "sent_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "admin_username", Type: VarString, Required: true, Sample: "root"},
	}},
	{Key: "ops.channel_auto_disabled", Channel: ChannelEmail, Category: "ops.channel_auto_disabled", Variables: []Variable{
		{Name: "admin_username", Type: VarString, Sample: "root"},
		{Name: "channel_name", Type: VarString, Required: true, Sample: "openai-main"},
		{Name: "channel_id", Type: VarString, Required: true, Sample: "17"},
		{Name: "reason_label", Type: VarString, Required: true, Sample: "上游返回鉴权错误"},
		{Name: "disabled_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "channels_url", Type: VarString, Required: true, Sample: "https://gw.example.com/admin/channels/17"},
	}},
	// W7/M4-a：站内版本。inapp 没有 subject（DDL CHECK），标题由 `title`
	// 变量承载（生产者按管理员语言本地化）；正文只引用稳定事实，不含
	// 上游错误原文、凭据或 base_url（GC-6）。
	{Key: "ops.channel_auto_disabled", Channel: ChannelInApp, Category: "ops.channel_auto_disabled", Variables: []Variable{
		{Name: "title", Type: VarString, Required: true, Sample: "渠道已自动禁用"},
		{Name: "channel_name", Type: VarString, Required: true, Sample: "openai-main"},
		{Name: "channel_id", Type: VarString, Required: true, Sample: "17"},
		{Name: "reason_label", Type: VarString, Required: true, Sample: "上游返回鉴权错误"},
		{Name: "disabled_at", Type: VarString, Required: true, Sample: "2026-10-14T19:03:11Z"},
		{Name: "channels_url", Type: VarString, Required: true, Sample: "/admin/channels"},
	}},
	{Key: "ops.redemption_expiring", Channel: ChannelEmail, Category: "ops.redemption_expiring", Variables: []Variable{
		{Name: "admin_username", Type: VarString, Sample: "root"},
		{Name: "batch_name", Type: VarString, Required: true, Sample: "双十一活动"},
		{Name: "remaining_count", Type: VarString, Required: true, Sample: "48"},
		{Name: "expires_at", Type: VarString, Required: true, Sample: "2026-11-01T00:00:00Z"},
		{Name: "codes_url", Type: VarString, Required: true, Sample: "https://gw.example.com/admin/redemption-codes"},
	}},
}

// Templates 返回注册表副本。内部切片字段不做深拷贝会泄露可变状态，故复制
// Variables；Defaults 由只读 map 提供，调用方不得修改。
func Templates() []Template {
	out := make([]Template, len(templateSpecs))
	for i, spec := range templateSpecs {
		spec.Variables = append([]Variable(nil), spec.Variables...)
		out[i] = spec
	}
	return out
}

// LookupTemplate 按 (key, channel) 查注册表。未注册组合 ok=false——这是
// 「16 category 封闭集合」在渠道维度的延伸：billing.balance_low 没有 webhook
// 模板，任何 webhook 写入都必须 400，而不是隐式创建。
func LookupTemplate(key string, channel Channel) (Template, bool) {
	for _, spec := range templateSpecs {
		if spec.Key == key && spec.Channel == channel {
			return spec, true
		}
	}
	return Template{}, false
}

// VariableByName 在封闭白名单中查变量声明。
func (t Template) VariableByName(name string) (Variable, bool) {
	for _, variable := range t.Variables {
		if variable.Name == name {
			return variable, true
		}
	}
	return Variable{}, false
}

// DefaultContent 返回内置文案：精确语言缺失时回退 en（§3.9.6a 的语言回退
// 同样适用于「内置 bundle 没有该语言文案」的情形）。
//
// 设计只给出 zh-CN / en 两列基线（§3.9.4 明示），其余 5 语在 defaults.go
// 中不另造译文；播种时以 en 文案落行。这样「每语言一行」的模型成立，
// 而译文补齐是内容任务，不改变契约。
func (t Template) DefaultContent(locale Locale) (Content, bool) {
	if content, ok := t.Defaults[locale]; ok {
		return content, true
	}
	content, ok := t.Defaults[DefaultLocale]
	return content, ok
}

// SampleVariables 返回必填/可选变量的样例值（§3.9.3「样例值」列）。
// 预览与测试发送都以它为底，调用方传入的值覆盖同名样例。
func (t Template) SampleVariables(locale Locale) map[string]any {
	out := make(map[string]any, len(t.Variables))
	// purpose_label 等代码内置供给值本应由业务方按语言拼装；预览期用
	// 注册表样例即可，不能因此把样例写进模板正文。int 变量以字符串注入
	// （P2：格式化在服务端完成后注入，模板只做文本替换）。
	for _, variable := range t.Variables {
		out[variable.Name] = localizedSample(locale, t.Key, variable)
	}
	return out
}

// SortedKeys 返回注册表全部 (key, channel) 组合的稳定顺序，便于列表回填与
// 测试逐行断言。顺序：categoryOrder 声明顺序 → channel(email 在前)。
func SortedKeys() []string {
	out := make([]string, 0, len(templateSpecs))
	for _, spec := range templateSpecs {
		out = append(out, spec.Key+"\x00"+string(spec.Channel))
	}
	sort.Strings(out)
	return out
}

// RegisteredKeyNames 返回 16 个 category key 的排序副本。
func RegisteredKeyNames() []string {
	out := make([]string, 0, len(categoryOrder))
	for _, category := range categoryOrder {
		out = append(out, category.Key)
	}
	sort.Strings(out)
	return out
}

// ValidateKey 报告 key 是否在 16 类注册表内。任何未注册 key 的读/写都必须
// 404/400（§3.9.1 封闭集合），不允许自动建模板。
func ValidateKey(key string) error {
	if _, ok := LookupCategory(key); !ok {
		return fmt.Errorf("notify: unknown template key %q", key)
	}
	return nil
}
