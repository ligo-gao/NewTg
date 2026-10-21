// Package settings 实现 system_settings 的代码内静态注册表与按 section 的读写服务。
//
// 设计权威：docs/modules-08-系统管理与设置-子功能设计.md §1.3（保存与生效）、
// §1.4/§1.4.1（键命名与公开下发全集）、§3.1–§3.8、§3.16；逐键登记以
// docs/system-config-mapping.md §4.7 为准。
//
// 注册表是「哪些键可以被写」的唯一判据（§1.4：未注册键写入一律拒绝）。
// 默认值只写在这里一处——前端与其它副本都从接口取，禁止第二份默认值
// （§3.16 Q1 / T-1c-10 的教训）。
package settings

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Type 是注册表声明的值类型。JSON 只有 number，因此整数族统一为 TypeInt，
// 由校验层保证不出现小数（数据库 bigint 字段不接受浮点）。
type Type string

const (
	TypeBool   Type = "bool"
	TypeInt    Type = "int"
	TypeNumber Type = "number"
	TypeString Type = "string"
	TypeEnum   Type = "enum"
	TypeList   Type = "list"
	TypeObject Type = "object"
)

// Setting 是注册表的一行。字段与 GET /api/settings 的 item 元数据一一对应。
type Setting struct {
	Key     string
	Section string
	Type    Type
	// Default 是出厂默认值，原生 Go 值（bool/int64/float64/string/[]any/map[string]any）。
	Default any

	// Public 表示进 /api/status 的公开下发全集（§1.4.1）。
	Public bool
	// Secret 表示密钥类：落库前 AES-256-GCM 密封，响应一律掩码。
	Secret bool
	// Dangerous 表示危险项：保存前需二次确认（服务端按 DangerDirection
	// 判定本次提交是否处于风险方向）。
	Dangerous bool
	// DangerDirection 是风险方向："off"（关闭型风险，如关闭注册/支付）或
	// "on"（开启型风险，如允许私网/开启自动禁用）；非布尔危险项的任意
	// 变更都视为风险方向。
	DangerDirection string
	// Locked 表示服务端强制值（如 nav.console.require_auth），只读不可改。
	Locked bool

	// 数值范围（int 用 MinInt/MaxInt，number 用 MinFloat/MaxFloat）。
	MinInt   *int64
	MaxInt   *int64
	MinFloat *float64
	MaxFloat *float64
	// MaxLen 是 string/enum 的最大字符数（rune）。
	MaxLen int
	// SanitizeHTML 表示该字段是 HTML 入站面，写入前按 {a,p,br,strong,em}
	// 白名单消毒（R-04-9）。
	SanitizeHTML bool
	// Allowed 是 enum 的闭集。
	Allowed []string
	// Pattern 是可选的正则约束（字符串整串匹配）。
	Pattern *regexp.Regexp
	// Normalize 在校验前后归一化值（如 server_address 去尾斜杠、档位去重排序）。
	Normalize func(any) (any, error)
	// Validate 是键级补充校验（类型/范围之外）。
	Validate func(any) error
	// Element 是 list 元素的类型/结构校验。
	Element func(any) error
}

// MaxResponseTimeThresholdSeconds 是 rl.health.response_time_threshold_seconds
// 的上界（24 小时）。
//
// 复核 M-9：无上界的「极大有限值」（如 1e18）在读取侧 float64→time.Duration
// 转换时会溢出成负数，使 `latency >= threshold` 恒真/恒假，静默关闭超时
// 判定。写入侧用它拒绝越界值；读取侧（channelhealth.LoadWorkerSettings）
// 对历史脏行回落注册表默认。
const MaxResponseTimeThresholdSeconds = 86400

// Sections 的返回顺序固定，便于前端与测试稳定比对。
var sectionOrder = []string{
	"site", "nav", "auth", "auth.turnstile", "billing", "billing_setting",
	"pay", "pay.stripe", "pay.alipay", "pay.epay", "pay.creem", "pay.waffo", "pay.waffo_pancake",
	"rl", "rl.health", "sec", "notify.smtp", "notify.webhook",
	// W10-N4 / N9：兑换码回收与任务保留清理。两组保留键都只服务 worker，
	// 不进入公开下发全集；独立 section 让运营按域查看而不互相覆盖。
	"redemption", "mj",
	// M6d：任务插件平台与模型部署控制面。两者都是独立页，但设置 Tab /
	// 插件主开关仍走同一注册表（modules-08 §3.10 / §3.20）。
	"plugin", "task", "deploy",
	// W8/MJ-a2：绘图任务族（relay-05 §1）。整族 8 键进注册表，
	// 键名/默认值与 system-config-mapping §4.7 #25–#30、#70–#71 逐字一致。
	"mj",
}

var (
	registryByKey     map[string]Setting
	registryBySection map[string][]Setting
)

func init() {
	registry := buildRegistry()
	registryByKey = make(map[string]Setting, len(registry))
	registryBySection = make(map[string][]Setting, len(sectionOrder))
	for _, s := range registry {
		if _, dup := registryByKey[s.Key]; dup {
			panic(fmt.Sprintf("settings: duplicate key %q in registry", s.Key))
		}
		registryByKey[s.Key] = s
		registryBySection[s.Section] = append(registryBySection[s.Section], s)
	}
	for _, section := range sectionOrder {
		if _, ok := registryBySection[section]; !ok {
			panic(fmt.Sprintf("settings: section %q declared but empty", section))
		}
	}
}

// IsKnownSection 报告 section 是否已登记。
func IsKnownSection(section string) bool {
	_, ok := registryBySection[section]
	return ok
}

// Lookup 返回注册表行。
func Lookup(key string) (Setting, bool) {
	s, ok := registryByKey[key]
	return s, ok
}

// Section 返回某个 section 的全部注册行（按登记顺序）。
func Section(section string) ([]Setting, bool) {
	rows, ok := registryBySection[section]
	return rows, ok
}

// Sections 返回冻结的 section 顺序副本。
func Sections() []string {
	out := make([]string, len(sectionOrder))
	copy(out, sectionOrder)
	return out
}

// All 返回全部注册行副本（测试与文档用）。
func All() []Setting {
	out := make([]Setting, 0, len(registryByKey))
	for _, section := range sectionOrder {
		out = append(out, registryBySection[section]...)
	}
	return out
}

// DefaultValue 返回默认值的深拷贝，避免调用方修改注册表内的共享结构。
func DefaultValue(s Setting) any {
	return cloneValue(s.Default)
}

// MaskedValue 是密钥类字段在响应与审计中的唯一掩码。
const MaskedValue = "******"

// ---------------------------------------------------------------------------
// 注册表构造辅助
// ---------------------------------------------------------------------------

// 风险方向（modules-08 §1.5 / §3 各危险项的数据字段表）。
const (
	DangerDirectionOn  = "on"
	DangerDirectionOff = "off"
	// DangerDirectionAny：任意变更都需要确认（如支付密钥替换）。
	DangerDirectionAny = "any"
)

type option func(*Setting)

func public() option { return func(s *Setting) { s.Public = true } }
func secret() option { return func(s *Setting) { s.Secret = true } }

// dangerousOn / dangerousOff 同时声明危险位与风险方向；非布尔危险项的
// 任何变更都按 on 方向保守处理（改值即风险）。
func dangerousOn() option {
	return func(s *Setting) {
		s.Dangerous = true
		s.DangerDirection = DangerDirectionOn
	}
}

func dangerousOff() option {
	return func(s *Setting) {
		s.Dangerous = true
		s.DangerDirection = DangerDirectionOff
	}
}

// dangerousAny：任意变更即风险（支付密钥替换、危险运营参数）。
func dangerousAny() option {
	return func(s *Setting) {
		s.Dangerous = true
		s.DangerDirection = DangerDirectionAny
	}
}

func locked() option        { return func(s *Setting) { s.Locked = true } }
func maxLen(n int) option   { return func(s *Setting) { s.MaxLen = n } }
func minInt(n int64) option { return func(s *Setting) { s.MinInt = &n } }
func maxInt(n int64) option { return func(s *Setting) { s.MaxInt = &n } }
func minFloat(n float64) option {
	return func(s *Setting) { s.MinFloat = &n }
}

func maxFloat(n float64) option {
	return func(s *Setting) { s.MaxFloat = &n }
}

func enum(values ...string) option {
	return func(s *Setting) {
		s.Type = TypeEnum
		s.Allowed = append([]string(nil), values...)
	}
}

func pattern(expr string) option {
	re := regexp.MustCompile(expr)
	return func(s *Setting) { s.Pattern = re }
}

func sanitizeHTMLOption() option {
	return func(s *Setting) { s.SanitizeHTML = true }
}

func normalize(fn func(any) (any, error)) option {
	return func(s *Setting) { s.Normalize = fn }
}

func validate(fn func(any) error) option {
	return func(s *Setting) { s.Validate = fn }
}

func element(fn func(any) error) option {
	return func(s *Setting) { s.Element = fn }
}

func def(key, section string, typ Type, value any, opts ...option) Setting {
	s := Setting{Key: key, Section: section, Type: typ, Default: value}
	for _, opt := range opts {
		opt(&s)
	}
	// Registry values must be JSON-serializable; fail at startup rather than
	// at first read (a broken default would otherwise only surface in prod).
	if _, err := json.Marshal(s.Default); err != nil {
		panic(fmt.Sprintf("settings: default for %q is not JSON serializable: %v", key, err))
	}
	return s
}

// buildRegistry 是本波 P0 的冻结注册表。新增键必须同时落在 docs/system-config-
// mapping.md §4.7 与 modules-08 §1.4.1（公开键），否则本表不认。
func buildRegistry() []Setting {
	// 出厂预置的 SSRF 域名白名单：仅覆盖 OAuth discovery / 支付外调等已知服务商。
	// 管理员自填的 SMTP / Webhook / 更新源不在其中，需自行加入（F-08-17）。
	ssrfDefaultDomains := []any{
		"api.openai.com", "api.anthropic.com", "generativelanguage.googleapis.com",
		"api.stripe.com", "openapi.alipay.com", "github.com", "www.googleapis.com",
	}

	return []Setting{
		// ==== site（F-08-01/02/03/04，§3.1）====
		def("site.title", "site", TypeString, "NewTG", public(), maxLen(64),
			validate(nonEmptyTrimmed("site.title"))),
		def("site.logo_url", "site", TypeString, "", public(), maxLen(512),
			validate(urlOrRelative)),
		def("site.server_address", "site", TypeString, "http://localhost:3000", public(), maxLen(256),
			normalize(trimTrailingSlash), validate(nonEmptyAbsoluteHTTPURL)),
		def("site.timezone", "site", TypeString, "Asia/Shanghai", public(), maxLen(64),
			validate(ianaTimezone)),
		def("site.footer_html", "site", TypeString, "", public(), sanitizeHTMLOption(), maxLen(4096)),
		def("site.about_html", "site", TypeString, "", public(), sanitizeHTMLOption(), maxLen(8192)),
		def("site.filing_no", "site", TypeString, "", public(), maxLen(64)),
		def("site.docs_url", "site", TypeString, "", public(), maxLen(256),
			validate(urlOrRelative)),
		def("site.status_page_url", "site", TypeString, "", public(), maxLen(256),
			validate(absoluteHTTPURL)),
		def("site.rankings_min_samples", "site", TypeInt, int64(100), public(),
			minInt(50), maxInt(100000)),
		// 每 period 保留的排行榜 run 数（B5a；billing-06 §8 / R-D6-07）。
		// 仅 worker 清理时消费，不需要下发给公开面，故不标 public。
		def("site.rankings_keep_runs", "site", TypeInt, int64(10),
			minInt(1), maxInt(100)),
		def("site.oss_credits", "site", TypeList, []any{}, public(),
			element(validateOSSCredit)),
		def("site.seo.description", "site", TypeString, "", public(), maxLen(160)),
		def("site.seo.og_image_url", "site", TypeString, "", public(), maxLen(512),
			validate(absoluteHTTPURL)),
		def("site.home_custom_enabled", "site", TypeBool, false, public()),
		def("site.home_custom_html", "site", TypeString, "", public(), dangerousOn(), sanitizeHTMLOption(), maxLen(65536)),
		def("site.home_blocks", "site", TypeObject, map[string]any{
			"hero": true, "live_feed": true, "features": true, "stats": true, "onboarding": true,
		}, public(), validate(boolObjectKeys("hero", "live_feed", "features", "stats", "onboarding"))),
		def("site.demo.enabled", "site", TypeBool, false, public()),

		// ==== nav（F-08-03）====
		def("nav.home.enabled", "nav", TypeBool, true, public()),
		def("nav.home.require_auth", "nav", TypeBool, false, public()),
		def("nav.console.enabled", "nav", TypeBool, true, public()),
		// 控制台必须登录：服务端强制 true，管理界面只读（F-08-03）。
		def("nav.console.require_auth", "nav", TypeBool, true, public(), locked()),
		def("nav.pricing.enabled", "nav", TypeBool, true, public()),
		def("nav.pricing.require_auth", "nav", TypeBool, false, public()),
		def("nav.rankings.enabled", "nav", TypeBool, true, public()),
		def("nav.rankings.require_auth", "nav", TypeBool, false, public()),
		def("nav.docs.enabled", "nav", TypeBool, true, public()),
		// R-PUB-09：文档站与关于页的登录守卫。默认 false（公开页可匿名访问）；
		// 与其它 nav.*.require_auth 同语义，由前端路由守卫消费。
		def("nav.docs.require_auth", "nav", TypeBool, false, public()),
		def("nav.about.enabled", "nav", TypeBool, true, public()),
		def("nav.about.require_auth", "nav", TypeBool, false, public()),
		def("nav.modules.console", "nav", TypeObject, map[string]any{
			"dashboard": true, "tokens": true, "usage_logs": true,
			"wallet": true, "profile": true, "security": true,
		}, public(), validate(boolObjectKeys("dashboard", "tokens", "usage_logs", "wallet", "profile", "security"))),
		def("nav.modules.chat.playground", "nav", TypeBool, true, public()),
		// 聊天预设入口的侧栏显隐（F-08-03）。条目与契约已撤销（§3.13），
		// 但开关仍按 §3.1 登记；不进 §1.4.1 公开全集。
		def("nav.modules.chat.presets", "nav", TypeBool, true),

		// ==== auth（F-08-05/06/08/23）====
		def("auth.signup_enabled", "auth", TypeBool, true, public(), dangerousOff()),
		def("auth.password_login_enabled", "auth", TypeBool, true, public(), dangerousOff()),
		def("auth.email_verification_required", "auth", TypeBool, false, public()),
		def("auth.email_domain_restrict_enabled", "auth", TypeBool, false),
		def("auth.email_domain_allowlist", "auth", TypeList, []any{},
			element(validateDomain), validate(maxItems(100, "auth.email_domain_allowlist"))),
		def("auth.mfa.admin_required", "auth", TypeBool, true),
		def("auth.mfa.admin_hardware_required", "auth", TypeBool, true),
		def("auth.passkey.enabled", "auth", TypeBool, false, public()),
		def("auth.passkey.rp_name", "auth", TypeString, "", maxLen(64)),
		def("auth.passkey.rp_id", "auth", TypeString, "", maxLen(253),
			validate(hostnameOnly)),
		def("auth.passkey.origins", "auth", TypeList, []any{},
			element(validateAbsoluteHTTPURL)),

		// ==== auth.turnstile（F-08-07）====
		def("auth.turnstile.enabled", "auth.turnstile", TypeBool, false, public()),
		def("auth.turnstile.site_key", "auth.turnstile", TypeString, "", public(), maxLen(64)),
		// 密钥类：configcenter.secretKeys 同步登记，Save 路径同样密封。
		def("auth.turnstile.secret_key", "auth.turnstile", TypeString, "", secret()),

		// ==== billing（F-08-09…12，§3.3）====
		def("billing.quota_per_unit", "billing", TypeInt, int64(100), public(), dangerousOn(),
			minInt(1), maxInt(1000000)),
		def("billing.display_unit", "billing", TypeEnum, "credits", public(), enum("credits", "cny")),
		def("billing.currency_symbol", "billing", TypeString, "¥", public(), dangerousOn(), maxLen(8),
			validate(nonEmptyTrimmed("billing.currency_symbol"))),
		def("billing.signup_grant_credits", "billing", TypeInt, int64(0), public(),
			minInt(0), maxInt(1000000000000)),
		def("billing.signup_grant_requires_verified_email", "billing", TypeBool, false, public()),
		def("billing.balance_alert_threshold_credits", "billing", TypeInt, int64(1000),
			minInt(0), maxInt(1000000000000)),
		def("billing.balance_alert_channels", "billing", TypeObject, map[string]any{
			"banner": true, "email": false, "webhook": false,
		}, validate(boolObjectKeys("banner", "email", "webhook"))),
		def("billing.free_model_preconsume_enabled", "billing", TypeBool, true),
		def("billing.free_model_preconsume_multiplier", "billing", TypeNumber, 1.0,
			validate(positiveFiniteNumber)),
		def("billing.free_model_preconsume_trust_credits", "billing", TypeInt, int64(0),
			minInt(0), maxInt(1000000000000)),
		def("billing.display_token_stat_enabled", "billing", TypeBool, true, public()),
		def("billing.expose_group_ratios", "billing", TypeBool, false),
		def("billing.group_group_ratio", "billing", TypeObject, map[string]any{}, validate(validateNestedRatioMap)),
		// 订阅在飞租约 TTL（R-W9B-02 / M-2 复核）。管理面必须可写，否则
		// 运维只能直接改配置行。下界 120s 来自 I-1：必须大于单任务最坏
		// 预算（约 55s），否则任务处理中途会失去租约；上界 24h 防止
		// 误写成「事实上的永不过期」。
		def("billing.subscription_inflight_lease_ttl_seconds", "billing", TypeInt, int64(120),
			minInt(120), maxInt(86400)),

		// ==== billing_setting（T5 任务计价表达式，R-06 设置契约扩展）====
		//
		// 仓库原本没有 `billing_setting` 命名空间；R-06 要求「无则新建并在
		// 报告登记为设置契约扩展，不要平铺顶层键」，故新建本 section，键名
		// 与 billing-02 §2.1/§3.5 的命名空间 + 字段名一致。
		//
		// 值形状：map["<pluginKey>::<model>"] = 任务用量表达式（u("字段")）。
		// 非公开：表达式是运营配置，不进入 /api/status 的公开下发全集。
		// 空值/非法键在保存期 422（R-37 fail-closed 的写入侧落点）。
		def("billing_setting.plugin_billing_expr", "billing_setting", TypeObject,
			map[string]any{}, validate(validatePluginBillingExprMap)),

		// ==== pay（F-08-13…15）====
		def("pay.online_enabled", "pay", TypeBool, true, public(), dangerousOff()),
		def("pay.amount_presets_cny", "pay", TypeList, []any{
			int64(10), int64(30), int64(50), int64(100), int64(300), int64(500),
		}, public(), element(positiveIntElement), normalize(normalizeIntList)),
		def("pay.min_topup_cny", "pay", TypeInt, int64(10), minInt(1), maxInt(100000)),
		def("pay.force_complete_max_amount", "pay", TypeInt, int64(100000), dangerousOn(),
			minInt(1)),
		// 充值分组倍率：未配置的分组回退 1.0（modules-04 F-04-01）。
		def("pay.topup_group_ratio", "pay", TypeObject, map[string]any{}, validate(validateRatioMap)),

		// ==== pay.stripe（F-08-13）====
		def("pay.stripe.enabled", "pay.stripe", TypeBool, false, public()),
		def("pay.stripe.secret_key", "pay.stripe", TypeString, "", secret(), dangerousAny()),
		def("pay.stripe.webhook_secret", "pay.stripe", TypeString, "", secret(), dangerousAny()),
		// P-17 / G7-R46 冻结名：钱包页展示用，公开非密钥。
		def("pay.stripe.publishable_key", "pay.stripe", TypeString, "", public(), maxLen(256)),
		def("pay.stripe.min_topup_cny", "pay.stripe", TypeInt, int64(10), minInt(1), maxInt(100000)),

		// ==== pay.alipay（F-08-14）====
		def("pay.alipay.enabled", "pay.alipay", TypeBool, false, public()),
		def("pay.alipay.app_id", "pay.alipay", TypeString, "", maxLen(32)),
		def("pay.alipay.private_key", "pay.alipay", TypeString, "", secret(), dangerousAny(), validate(pemKey("PRIVATE KEY"))),
		def("pay.alipay.public_key", "pay.alipay", TypeString, "", secret(), dangerousAny(), validate(pemKey("PUBLIC KEY"))),
		def("pay.alipay.gateway_url", "pay.alipay", TypeEnum,
			"https://openapi.alipay.com/gateway.do",
			enum("https://openapi.alipay.com/gateway.do",
				"https://openapi-sandbox.dl.alipaydev.com/gateway.do")),
		def("pay.alipay.sandbox", "pay.alipay", TypeBool, false, public()),

		// ==== pay.epay（模块 04 F-04-21；C1 落地）====
		// 收银台 API 根地址必须是 https 绝对 URL：表单会把商户号、金额与
		// MD5 签名 POST 到该地址。pid 是商户号（非密钥但属敏感标识），
		// key 是签名密钥（secret：落库密封、响应掩码）。
		def("pay.epay.enabled", "pay.epay", TypeBool, false, public()),
		def("pay.epay.api_url", "pay.epay", TypeString, "", maxLen(1024),
			validate(httpsAbsoluteURL)),
		def("pay.epay.pid", "pay.epay", TypeString, "", maxLen(128), dangerousOn()),
		def("pay.epay.key", "pay.epay", TypeString, "", secret(), dangerousAny()),
		// 签名拼接口径：append（默认，标准 $signstr.$key）或 amp
		// （部分网关的 $signstr."&".$key）。两种口径不能同时兼容，
		// 默认保持标准算法；下单与回调共用同一取值。
		def("pay.epay.sign_mode", "pay.epay", TypeEnum, "append",
			enum("append", "amp")),
		// 可选回调/返回地址；留空时由 site.server_address 推导默认值。
		def("pay.epay.notify_url", "pay.epay", TypeString, "", maxLen(1024),
			validate(httpsAbsoluteURL)),
		def("pay.epay.return_url", "pay.epay", TypeString, "", maxLen(1024),
			validate(urlOrRelative)),

		// ==== pay.creem / waffo（§4.7 一次性登记）====
		def("pay.creem.enabled", "pay.creem", TypeBool, false, public()),
		def("pay.creem.test_mode", "pay.creem", TypeBool, false),

		def("pay.waffo.enabled", "pay.waffo", TypeBool, false, public()),
		def("pay.waffo.sandbox", "pay.waffo", TypeBool, false),
		def("pay.waffo.merchant_id", "pay.waffo", TypeString, "", maxLen(128), dangerousOn()),
		def("pay.waffo.min_topup_cny", "pay.waffo", TypeInt, int64(10), minInt(1), maxInt(100000)),

		def("pay.waffo_pancake.enabled", "pay.waffo_pancake", TypeBool, false, public()),
		def("pay.waffo_pancake.merchant_id", "pay.waffo_pancake", TypeString, "", maxLen(128), dangerousOn()),
		def("pay.waffo_pancake.min_topup_cny", "pay.waffo_pancake", TypeInt, int64(10), minInt(1), maxInt(100000)),
		def("pay.waffo_pancake.store_id", "pay.waffo_pancake", TypeString, "", maxLen(128)),
		def("pay.waffo_pancake.product_id", "pay.waffo_pancake", TypeString, "", maxLen(128)),

		// ==== rl（F-08-16）====
		def("rl.user_model_rpm", "rl", TypeInt, int64(60), minInt(0), maxInt(100000)),
		def("rl.token_rpm", "rl", TypeInt, int64(0), minInt(0), maxInt(100000)),
		def("rl.user_global_rpm", "rl", TypeInt, int64(0), minInt(0), maxInt(100000)),
		def("rl.group_overrides", "rl", TypeObject, map[string]any{}, validate(validateGroupOverrides)),

		// ==== rl.health（§3.16 F-08-45，P0）====
		def("rl.health.auto_disable_enabled", "rl.health", TypeBool, false, dangerousOn()),
		def("rl.health.response_time_threshold_seconds", "rl.health", TypeNumber, 5.0,
			validate(positiveFiniteNumber), maxFloat(MaxResponseTimeThresholdSeconds)),
		// R-M4-11：关键词表是**列表**而不是多行字符串。前端 lines 控件保存时
		// 发送字符串数组（settings-section-form.tsx），TypeString 会以
		// 「expected string」400 拒绝，因此必须用 TypeList；element 限制单项
		// 长度，maxItems 限制匹配成本（R-M4-12 的有界复杂度）。
		// 历史 TypeString 行由消费方 channelhealth.ParseDisableKeywords 兼容。
		def("rl.health.disable_keywords", "rl.health", TypeList,
			[]any{
				"Your credit balance is too low",
				"This organization has been disabled.",
				"You exceeded your current quota",
				"Permission denied",
				"The security token included in the request is invalid",
				"Operation not allowed",
				"Your account is not authorized",
			},
			dangerousOn(), element(validateDisableKeyword),
			validate(maxItems(100, "rl.health.disable_keywords"))),
		def("rl.health.auto_enable_enabled", "rl.health", TypeBool, false),
		def("rl.health.test_interval_minutes", "rl.health", TypeInt, int64(10), minInt(1), maxInt(1440)),
		def("rl.health.test_concurrency", "rl.health", TypeInt, int64(5), minInt(1), maxInt(50)),

		// ==== sec（F-08-17/18，§3.5）====
		def("sec.ssrf.enabled", "sec", TypeBool, true, dangerousOff()),
		// B2（security-04 §2.4）：统一出站执行器的三态强度。`enabled` 是
		// 旧布尔开关，仅在本键缺失时作为 off/basic 的兼容来源；新装配一律
		// 以 mode 为准（关闭型风险：off 会解除全部出站校验）。
		def("sec.ssrf.mode", "sec", TypeEnum, "basic",
			enum("off", "basic", "strict"), dangerousOff()),
		def("sec.ssrf.allow_private_ip", "sec", TypeBool, false, dangerousOn()),
		def("sec.ssrf.allowed_ports", "sec", TypeList, []any{int64(443)},
			element(portElement)),
		def("sec.ssrf.domain_mode", "sec", TypeEnum, "allowlist",
			enum("all", "allowlist", "denylist")),
		def("sec.ssrf.domain_list", "sec", TypeList, ssrfDefaultDomains,
			element(validateDomainOrWildcard)),
		// 重定向上限与连接超时按 B2 冻结语义补齐；两者都不公开下发。
		def("sec.ssrf.max_redirects", "sec", TypeInt, int64(3), minInt(0), maxInt(10)),
		def("sec.ssrf.connect_timeout_seconds", "sec", TypeInt, int64(10),
			minInt(1), maxInt(120)),
		def("sec.sensitive.enabled", "sec", TypeBool, false),
		def("sec.sensitive.scope", "sec", TypeObject, map[string]any{
			"prompt": true, "system": false,
		}, validate(boolObjectKeys("prompt", "system"))),
		def("sec.sensitive.words", "sec", TypeList, []any{}, dangerousOn(),
			element(validateSensitiveWord), validate(maxItems(5000, "sec.sensitive.words"))),
		def("sec.sensitive.action", "sec", TypeEnum, "block", enum("block", "log")),

		// ==== notify.smtp（F-08-19，§3.6）====
		def("notify.smtp.host", "notify.smtp", TypeString, "", maxLen(256)),
		def("notify.smtp.port", "notify.smtp", TypeInt, int64(465), minInt(1), maxInt(65535)),
		def("notify.smtp.encryption", "notify.smtp", TypeEnum, "ssl", enum("ssl", "starttls", "none")),
		def("notify.smtp.username", "notify.smtp", TypeString, "", maxLen(256)),
		def("notify.smtp.from_name", "notify.smtp", TypeString, "", maxLen(128)),
		def("notify.smtp.password", "notify.smtp", TypeString, "", secret()),
		def("notify.smtp.force_auth_login", "notify.smtp", TypeBool, false),
		def("notify.smtp.insecure_skip_verify", "notify.smtp", TypeBool, false),
		def("notify.smtp.from_email", "notify.smtp", TypeString, "", maxLen(254),
			validate(emailOrEmpty)),

		// ==== notify.webhook（F-08-20，§3.6）====
		//
		// 暂停状态/连续失败计数是运行时状态，不新增设置键（D2-10）：设置表的
		// 每键都要有审计与乐观锁语义，把 worker 的临时状态塞进来会让「保存
		// 设置」与「运行时暂停」互相覆盖。
		def("notify.webhook.enabled", "notify.webhook", TypeBool, false),
		// 键级只校验「非空时必须是绝对 http(s) URL」；https / 必填等随
		// enabled 变化的约束在 section 交叉校验里（D2-11）。
		def("notify.webhook.url", "notify.webhook", TypeString, "", maxLen(1024),
			validate(validateAbsoluteHTTPURL)),
		// 密钥类：configcenter.secretKeys 同步登记，落库前 AES-256-GCM 密封，
		// 读接口只回 "******"（security-03 密钥治理）。
		def("notify.webhook.secret", "notify.webhook", TypeString, "", secret()),
		// 订阅事件闭集；允许空数组（= 不下发任何事件）；写入时去重保序。
		def("notify.webhook.events", "notify.webhook", TypeList, []any{"balance_low"},
			normalize(normalizeWebhookEvents), element(validateWebhookEvent),
			validate(maxItems(4, "notify.webhook.events"))),
		// 低余额扫描阈值（积分）。0 = 关闭扫描（默认）；>0 且 Webhook 启用时，
		// N6 worker 每 5 分钟扫描 balance < threshold 的启用用户并投递
		// balance_low 事件（§F-08-11 / billing-06 §9.6）。非公开：只服务
		// worker，不进入 /api/status 公开下发全集。上限 1e12 与其它积分
		// 阈值键（billing.balance_alert_threshold_credits）同口径。
		def("notify.webhook.balance_low_threshold", "notify.webhook", TypeInt, int64(0),
			minInt(0), maxInt(1000000000000)),

		// ==== redemption（W10-N4 / R-W10-05：兑换码回收）====
		//
		// 只回收「超过保留期且 unused + expired」的码（billing-06 §7）。
		// `expired` 仍是虚拟态：本键只控制物理删除窗口，不触发任何
		// status/expired_at 的物化。默认 90 天与设计一致；**0 = 关闭**
		// （与 N9 两个保留键同一冻结语义），关闭后过期码永久保留。
		// 上限 10 年，防止把「天」误写成极大值后在换算 Duration 时溢出。
		def("redemption.retention_days", "redemption", TypeInt, int64(90),
			minInt(0), maxInt(3650)),

		// ==== mj（W10-N9 / R-W10-10：MJ 族任务保留清理）====
		//
		// 与插件族的 `task.retention_terminal_days` **分开**：两族上游生命
		// 周期不同，共用一个键会让运营无法单独调整（R-W10-10 选 A）。
		// 只删终态（success/failure）行，未终结与待补偿行永不删除。
		// 默认 30 天；0 = 关闭该族清理。
		def("mj.retention_terminal_days", "mj", TypeInt, int64(30),
			minInt(0), maxInt(3650)),

		// ==== plugin（M6d 任务插件平台，relay-06 / modules-08 §3.10）====
		//
		// 全部归 plugin section（R-M6D-10）：管理员一次提交主开关与策略，
		// 插件管理面的写权限同样是 settings:manage，不存在第二个 section。
		// plugin 键一律不进公开下发（§1.4.1 未登记），避免把平台策略暴露给匿名面。
		def("plugin.enabled", "plugin", TypeBool, true),
		// 出厂插件禁用清单：命中 key 的 factory 层不注册。any 方向——增删任意
		// 条目都会改变数据面可用能力，按「任意变更即风险」保守处理。
		def("plugin.disabled_factory_keys", "plugin", TypeList, []any{},
			dangerousAny(), element(validatePluginKey),
			validate(maxItems(100, "plugin.disabled_factory_keys"))),
		def("plugin.marketplace.enabled", "plugin", TypeBool, true),
		// 上传源码上限 2 MiB（R-M6D-07 默认值）。上限再取 64 MiB 作为注册表
		// 硬边界：任何超出 provider/网关内存预算的值在保存期就拒绝。
		def("plugin.upload.max_bytes", "plugin", TypeInt, int64(2097152),
			minInt(1), maxInt(67108864)),

		// ==== task（W9-B4 / G-M6D-27：任务轮询上限）====
		//
		// 设计回写（2026-10-21，R-W9B-04/08）：设计把 TASK_TIMEOUT_MINUTES /
		// TASK_POLL_MAX_FAILURES 写成基线环境变量，但新系统的可运行配置面是
		// system_settings（configcenter 统一承载；轮询器在 worker 启动时取快照，
		// 改动随 worker 重建生效），本波据此新增 `task` section。
		// 键名映射：poll_timeout_minutes ↔ TASK_TIMEOUT_MINUTES（1440 分钟），
		// poll_max_failures ↔ TASK_POLL_MAX_FAILURES（20）。
		//
		// 语义冻结：**未配置 → 设计默认；显式 0 → 禁用该判定；正值 → 按值**。
		// 注册表这里存的是「管理员写入值」，不承担默认判定——未配置时读取方
		// 拿到 ok=false，必须交给轮询器的默认常量（见 platform/task_poller.go
		// 的 taskPollLimits）。因此这里 Default 仅用于管理面展示，取值与
		// tasks.DefaultTaskTimeout/DefaultMaxPollFailures 保持一致。
		// 上限取 1 年/100 万次，避免把 Timeout 换算成 time.Duration 时溢出。
		def("task.poll_timeout_minutes", "task", TypeInt, int64(1440),
			minInt(0), maxInt(525600)),
		def("task.poll_max_failures", "task", TypeInt, int64(20),
			minInt(0), maxInt(1000000)),
		// W10-N9（R-W10-10）：插件族终态任务保留期（天）。只删 success/
		// failure 且不带待补偿标记的行；MJ 族用独立的
		// `mj.retention_terminal_days`。默认 30，0 = 关闭本族清理。
		def("task.retention_terminal_days", "task", TypeInt, int64(30),
			minInt(0), maxInt(3650)),

		// ==== mj（W8/MJ-a2 绘图任务族，relay-05 §1）====
		//
		// 设计冻结为设置键（N-MAP-1 / C-44），不另立环境变量载体。全部非公开：
		// 这些是站点能力/内部策略，不进 /api/status 匿名下发全集。
		//
		// 读取侧必须 fail-closed：键缺失/解析失败一律按 false 处理，尤其
		// mj.enabled 关闭时提交端点直接 403，不解析、不预扣、不出站。
		def("mj.enabled", "mj", TypeBool, false),
		def("mj.notify_enabled", "mj", TypeBool, false),
		def("mj.account_filter_enabled", "mj", TypeBool, false),
		def("mj.mode_clear_enabled", "mj", TypeBool, false),
		def("mj.forward_url_enabled", "mj", TypeBool, false),
		def("mj.action_check_success_enabled", "mj", TypeBool, false),
		// 终态保留天数（#70）：0 = 不自动清理（显式关闭）。上限 10 年，
		// 避免把异常配置换算成 time.Duration 时溢出。
		def("mj.retention_terminal_days", "mj", TypeInt, int64(30),
			minInt(0), maxInt(3650)),
		// 旧 simple-change 兼容开关（#71）：默认关，走正确的 SimpleChange 语义。
		def("mj.legacy_simple_change_as_change", "mj", TypeBool, false),

		// ==== deploy（M6d io.net 控制面，modules-08 §3.20）====
		//
		// 只有两个设置键；部署/容器/日志等 provider 实时资源不落库（§3.20.4）。
		// enabled 是「关闭即能力失效」的关闭型风险；api_key 是长期凭据，
		// 替换/清除都必须二次确认（direction=any）。
		def("deploy.ionet.enabled", "deploy", TypeBool, false, dangerousOff()),
		def("deploy.ionet.api_key", "deploy", TypeString, "", secret(), dangerousAny()),
	}
}

// pluginKeyPattern 与 task_plugins.key 的 DB CHECK 同口径（R-M6D-01：≤30 字符，
// 小写字母/数字开头，仅含小写字母、数字、下划线与连字符）。注册表先拒绝，
// 避免把非法项写进设置后由激活路径才失败。
var pluginKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,29}$`)

// validatePluginKey 校验 plugin.disabled_factory_keys 的单个元素。
func validatePluginKey(v any) error {
	key, ok := v.(string)
	if !ok {
		return fmt.Errorf("expected string")
	}
	if !pluginKeyPattern.MatchString(key) {
		return fmt.Errorf("plugin key must match %s", pluginKeyPattern.String())
	}
	return nil
}

// validatePluginBillingExprMap 校验任务计价表达式表的形状。
//
// 键格式冻结为 "<pluginKey>::<model>"（billing-02 §2.6）：pluginKey 与
// task_plugins.key 同约束（复用 pluginKeyPattern），model 非空。空值在这里
// 就拒绝（R-37：键存在但为空是配置错误，运行期会 fail-closed 保留预扣，
// 但不能让管理员把明显无效的配置保存进去）。
//
// 表达式源码本身不在此编译：设置包只做形状校验，编译/白名单由
// pricing.CompileTaskExpr 在解析与结算时统一执行（避免两套语法判定）。
func validatePluginBillingExprMap(v any) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("expected object")
	}
	if len(obj) > 1000 {
		return fmt.Errorf("too many entries (max 1000)")
	}
	for key, value := range obj {
		pluginKey, model, found := strings.Cut(key, "::")
		if !found {
			return fmt.Errorf("key %q must be \"<pluginKey>::<model>\"", key)
		}
		if !pluginKeyPattern.MatchString(pluginKey) {
			return fmt.Errorf("key %q: plugin key must match %s", key, pluginKeyPattern.String())
		}
		if strings.TrimSpace(model) == "" || len(model) > 200 {
			return fmt.Errorf("key %q: model must be non-empty and <= 200 chars", key)
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("key %q: expression must be a string", key)
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("key %q: expression must not be blank", key)
		}
		if len(text) > 64<<10 {
			return fmt.Errorf("key %q: expression too long (max 64 KiB)", key)
		}
	}
	return nil
}

// cloneValue 深拷贝 JSON 形态的值，并保持整数精度。
//
// 不能用 json.Unmarshal 到 any：那会把 int64 默认值变成 float64，
// validatePaySection 的 int64 断言随即失效（F1）。这里复用写入路径的
// UseNumber + normalizeScalars，保证默认值与落库值同型。
func cloneValue(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	decoded, err := decodeJSON(raw)
	if err != nil {
		return v
	}
	return normalizeScalars(decoded)
}

// sortedKeys 供稳定输出（审计 diff 与测试）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// nowUTC 收敛时间来源，便于测试注入。
func nowUTC() time.Time { return time.Now().UTC() }
