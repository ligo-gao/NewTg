// Package store 承载主库 GORM 模型与「DB CHECK ↔ Go 常量」对照表。
//
// DDL/DML 纪律（platform-04 §3.7 V8）：保留字列 "group"（users/api_tokens/
// group_pricing）与 "key"（system_settings）在 SQL 文本中必须双引号引用；
// GORM 的 column tag 只写裸列名（如 gorm:"column:group"），标识符引用由
// dialector 负责——tag 里写引号会变成列名的一部分，扫描时按裸列名取不到。
// GORM 模型字段随里程碑填充（M0 空壳，M1 起逐步落地）。
package store

import (
	"errors"

	"newtg/gateway/internal/contract"
)

// 用户扣费来源偏好的持久化字面量（users.billing_preference，R-D5-05）。
//
// 【禁止按下标 / 类型转换】这四个常量的顺序刻意与 contract.BillingPreference
// 的 iota 顺序不同，也不要假设它与 DB CHECK 的书写顺序相同：
//
//	contract（内部枚举）：subscription_first=0, subscription_only=1,
//	                      wallet_only=2, wallet_first=3
//	users.billing_preference（DB 文本）：subscription_first, wallet_first,
//	                      subscription_only, wallet_only
//
// 例如把 internal 值 1 直接用 strconv/index 转成文本会写成 wallet_first，
// 而它其实是 subscription_only——用户的扣费来源被悄悄改成「只用钱包」，
// 属于资金路径上的静默数据损坏。两个方向只能走下面的显式映射表。
const (
	BillingPreferenceSubscriptionFirst = "subscription_first"
	BillingPreferenceWalletFirst       = "wallet_first"
	BillingPreferenceSubscriptionOnly  = "subscription_only"
	BillingPreferenceWalletOnly        = "wallet_only"
)

// ErrBillingPreferenceInvalid：偏好文本不在四值白名单内。
var ErrBillingPreferenceInvalid = errors.New("store: billing preference is invalid")

// billingPreferencePairs 是文本 ↔ 内部枚举的唯一对照表。双向映射都从这里
// 读取，新增/调整取值只需改一处，避免两个 switch 各自漂移。
var billingPreferencePairs = [...]struct {
	text  string
	value contract.BillingPreference
}{
	{BillingPreferenceSubscriptionFirst, contract.PrefSubscriptionFirst},
	{BillingPreferenceWalletFirst, contract.PrefWalletFirst},
	{BillingPreferenceSubscriptionOnly, contract.PrefSubscriptionOnly},
	{BillingPreferenceWalletOnly, contract.PrefWalletOnly},
}

// BillingPreferenceText 把内部枚举映射为 users.billing_preference 落库文本。
// 越界值返回 ErrBillingPreferenceInvalid，而不是兜底成 subscription_first：
// 静默归一属于迁移层的职责，存储层不能掩盖调用方的枚举错误。
func BillingPreferenceText(p contract.BillingPreference) (string, error) {
	for _, pair := range billingPreferencePairs {
		if pair.value == p {
			return pair.text, nil
		}
	}
	return "", ErrBillingPreferenceInvalid
}

// ParseBillingPreference 把 DB 文本解析为内部枚举（未知文本返回
// ErrBillingPreferenceInvalid，由调用方决定是否按迁移规则归一为
// subscription_first）。
func ParseBillingPreference(text string) (contract.BillingPreference, error) {
	for _, pair := range billingPreferencePairs {
		if pair.text == text {
			return pair.value, nil
		}
	}
	return 0, ErrBillingPreferenceInvalid
}

// EnumChecks 是 Tier-1 枚举列的「DB CHECK ↔ 期望值」对照表（platform-04 §6 V6）：
// 键为 "<table>.<column>"，值为 CHECK IN 白名单成员。CI/集成测试逐条与
// pg_constraint 的 CHECK 文本对账（成员集合相等），漂移即红。
//
// channels.type 为 Tier-2（60 slug 白名单，随渠道增长），**刻意不在此表**——
// 它的唯一真源是 internal/contract 的 ChannelTypes，由应用层 Valid() 强制。
//
// 成员口径：text 枚举为字面量；smallint 枚举为十进制数字文本（如 "1"），
// 与 CHECK 文本 IN (1, 2) 的解析结果同形。usage_logs/audit_logs 属日志库
// （migrations-logs），由 EnumChecksLogs 单独对账，不在本表范围。
var EnumChecks = map[string][]string{
	// users（platform-01 §1）
	"users.role":     {"user", "admin"},
	"users.status":   {"enabled", "disabled"},
	"users.language": {"en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"},

	// api_tokens（§2）
	"api_tokens.status": {"enabled", "disabled"},

	// user_groups（§3，smallint）
	"user_groups.status": {"1", "2"},

	// custom_oauth_providers（M1 / modules-01 §3.13，smallint：
	// 0=auto / 1=params / 2=header）
	"custom_oauth_providers.auth_style": {"0", "1", "2"},

	// channels（§6，smallint；type 除外，见上）
	"channels.status": {"1", "2", "3"},

	// model_pricing（§7，smallint）
	"model_pricing.mode":   {"0", "1"},
	"model_pricing.status": {"1", "2"},

	// group_pricing（§8，smallint）
	"group_pricing.status": {"1", "2"},

	// redemption_codes（§9）
	"redemption_codes.status": {"unused", "disabled", "used"},

	// orders（§10；failure_code 值集取 modules-04 F-04-18「统一失败码」8 值）
	// provider 的 wallet 是 P2b 余额购买订阅（迁移 20261020230000），
	// 不是第三方渠道：只出现在 biz_kind='subscribe' 的订单上。
	"orders.provider": {"stripe", "alipay", "creem", "waffo", "waffo_pancake", "epay", "wallet"},
	// refund_required 由 S2/R-29② 加入（迁移 20261021130000）：钱已收、履约
	// 失败，留给退款流程的终态。refunded/refund_failed 由 S3 加入（迁移
	// 20261022090000）：前者是退款完成终态，后者是渠道退款失败的可重试态。
	"orders.status": {
		"pending", "succeeded", "failed", "expired", "cancelled", "refund_required",
		"refunded", "refund_failed",
	},
	// refund_in_progress 由 S3/R-37 加入（迁移 20261022091000）：渠道退款调用
	// 期间的瞬时标记，不是资金事实；成功清空、失败改写为 provider_*。
	"orders.failure_code": {
		"provider_unavailable", "provider_rejected", "signature_invalid", "amount_mismatch",
		"currency_mismatch", "order_not_found", "order_state_conflict", "manual_required",
		"refund_in_progress",
	},

	// wallet_transactions（§11）
	"wallet_transactions.type": {
		"topup", "redemption", "consume_pre", "consume", "release",
		"reversal", "admin_adjustment", "bonus", "affiliate_transfer", "checkin_bonus",
		// P2b 余额购买订阅（迁移 20261020230000）。
		"subscription_purchase",
	},
	"wallet_transactions.status": {"hold", "settled", "released", "expired", "confirmed"},
	"wallet_transactions.reference_type": {
		"order", "redemption", "usage_log", "admin_op", "reversal",
		"signup_grant", "promo", "checkin", "affiliate_transfer",
	},

	// rankings_*（§13/§16/§17）
	// granularity 的 week 由 D6a 迁移 20261020171000 加入（R-D6-04：year 周期
	// 天桶跨度 > 90 天，必须降采样为周桶）。
	"rankings_snapshot_runs.period":          {"today", "week", "month", "year"},
	"rankings_snapshot_runs.status":          {"ok", "partial", "empty"},
	"rankings_timeseries.granularity":        {"day", "hour", "week"},
	"rankings_author_timeseries.granularity": {"day", "hour", "week"},
	// 作者序列按公开周期隔离（B5a 收口迁移 20261020260000）；period 进自然键后
	// week/month 的 day 桶不再互相覆盖，读侧也按 run.Period 过滤。
	"rankings_author_timeseries.period": {"today", "week", "month", "year"},
	"rankings_entities.kind":            {"model", "author"},

	// request_registry（§18 / platform-04 V9）
	"request_registry.state": {"in_flight", "settled", "refunded", "failed"},

	// notification_templates / notification_deliveries（M6c Task 4，
	// modules-08 §3.9.2 / §3.9.5）。模板载体 {email,inapp}。投递记录原设计
	// 只有 {email,webhook}（inapp 不落表）；A5 冻结语义要求公告为每个启用
	// 用户落一条 inapp 投递记录，故投递枚举扩为 {email,webhook,inapp}
	// （迁移 20261020200000，契约扩展已登记 G-DOM-23/G-DOM-37）。
	// locale 与 users.language 同集（canonical 7 语）。
	"notification_templates.channel":  {"email", "inapp"},
	"notification_templates.locale":   {"en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"},
	"notification_deliveries.channel": {"email", "webhook", "inapp"},
	"notification_deliveries.locale":  {"en", "zh-CN", "zh-TW", "fr", "ru", "ja", "vi"},
	"notification_deliveries.status": {
		"queued", "sent", "failed", "bounced", "suppressed",
	},

	// 任务插件三表（M6d Task 1，modules-08 §3.10.2 B-11/B-12/B-13）。
	// task_plugins.source 是「层」枚举（内置 factory / 上传覆盖层）；
	// task_plugin_versions.api_version 当前只有 v1（relay-06 §2.2，清单
	// 版本化预留多值）；市场源的 last_check_status 三态对应试拉结果。
	"task_plugins.source":                          {"builtin", "upload"},
	"task_plugin_versions.api_version":             {"v1"},
	"plugin_marketplace_sources.last_check_status": {"pending", "ok", "failed"},

	// 任务数据面主表（M6d Task 6a，relay-06 §7 / 契约 §2.5）。
	// family 是族判别键：plugin=任务插件平台（06），mj=绘图任务族（05，
	// W8/MJ-A0 迁移 20261023090000 扩入）；两族共用同一张 tasks 表，
	// 由 family 决定 Action 的解释权与轮询归属（R-MJ-03/R-MJ-04）。
	// fetch_mode 是「本系统如何取回」，与插件清单 meta.fetchMode
	// （per_task/batch）同名不同义；state 是 TaskState 的 7 态持久化字面量。
	"tasks.family":     {"plugin", "mj"},
	"tasks.fetch_mode": {"poll", "callback", "inline"},
	"tasks.state": {
		"not_start", "submitted", "queued", "in_progress",
		"success", "failure", "unknown",
	},
	// W8/MJ-d 提交幂等键（R-MJ-06）：in_flight=已占位执行中（可被过期租约
	// 接管），completed=首次结果已落库（重放命中）。没有第三种状态：失败
	// 直接删除行（Abandon），让同键可以重试。
	"task_submit_idempotency.status": {"in_flight", "completed"},
	// T4 / P-04：批次聚合状态。只登记本实现会产出的三态；validating/
	// finalizing/cancelled 属 OpenAI 状态机，取消端点当前 fail-closed，
	// 因而不进约束（写死未使用取值会掩盖实现进度）。
	"task_batches.status": {"in_progress", "completed", "failed"},

	// 模型目录与供应商（D1，platform-01 §22.7.1/§22.7.2）。
	// vendors.status / model_meta.status：1=启用 / 2=停用；
	// model_meta.name_rule：0=精确 / 1=前缀 / 2=包含 / 3=后缀（D1-02）。
	// 这三列都是 smallint 枚举，成员按十进制文本登记，与 CHECK 解析同形。
	"vendors.status":       {"1", "2"},
	"model_meta.status":    {"1", "2"},
	"model_meta.name_rule": {"0", "1", "2", "3"},

	// 订阅套餐数据层（D5a，billing-08 §2/§3/§6）。
	//   - plans.status：enabled/disabled（R-D5-08，刻意为 text，不用 smallint）；
	//   - subscriptions.status：active/expired/cancelled 三态（§3.1）；
	//   - pre-consume 记录：consumed→refunded 单向（§4.4）；
	//   - orders.biz_kind / users.billing_preference：R-D5-04 / R-D5-05。
	// currency 不在此表：S1 只有单成员 CHECK（'CNY'），PG 会把单元素 IN
	// 规范化为 `currency = 'CNY'::bpchar`，不呈枚举形态；其断言在
	// schema_integration_test.go 的 documentedChecks（R-D5-07）。
	"subscription_plans.status":               {"enabled", "disabled"},
	"subscriptions.status":                    {"active", "expired", "cancelled"},
	"subscription_pre_consume_records.status": {"consumed", "refunded"},
	// S1 订阅事件流的 kind 白名单（R-18 冻结）：S1 只产出 created/renewed/
	// cancelled/expired/reset；upgraded/downgraded/refunded 由 S2/S3 接入，
	// 白名单先冻结，避免各批次对同一含义使用不同拼写。
	"subscription_events.kind": {
		"created", "renewed", "upgraded", "downgraded",
		"refunded", "cancelled", "expired", "reset",
		// S4：自动续费扣款失败（P-16；迁移 20261022092000）。
		"renewal_failed",
	},
	// 'change' 由 S2 加入（迁移 20261021120000）：biz_ref_id=subscriptions.id，
	// amount 是应补差价而不是套餐全价。
	"orders.biz_kind":          {"topup", "subscribe", "renew", "change"},
	"users.billing_preference": {"subscription_first", "wallet_first", "subscription_only", "wallet_only"},

	// S4 支付令牌（P-14）：provider 单成员 CHECK 会被 PG 规范化为等值表达式，
	// 不呈枚举形态，故只登记 status（active/invalid）。
	"payment_methods.status": {"active", "invalid"},

	// 站点公告（D7a，modules-08 F-08-04 / R-D7-01）。
	// status 只有三值：expired 是**虚拟态**（published + expire_at <= now），
	// 刻意不进入 CHECK 白名单——落库会引入一个需要 worker 迁移的状态
	// （R-D7-10），而时间流逝是纯函数，不需要后台任务。
	"announcements.category": {"notice", "maintenance", "release"},
	"announcements.status":   {"draft", "published", "offline"},

	// C2 / Task 5c：GPU 资源租赁计费两表（platform-01 §22.13）。
	// 段状态机：pending_confirm → running → settled/abandoned/failed；
	// deleting_unknown 是删除结果未知的旁支（billing-03 §5.9.3）。
	"deployment_billing_segments.segment_kind": {"create", "extend"},
	"deployment_billing_segments.state":        {"pending_confirm", "running", "deleting_unknown", "settled", "abandoned", "failed"},
	"deployment_cost_facts.fact_type":          {"preconsume", "accrual", "settlement", "reconciliation_adjustment", "released"},
	"deployment_cost_facts.usage_source":       {"provider_exact", "provider_partial", "internal_accounting", "estimated", "reserved_default"},
	"deployment_cost_facts.state":              {"held", "accrued", "confirmed", "released", "adjusted"},

	// W17-2 worker 任务框架（billing-06 §2.4）：三处枚举 CHECK 与迁移逐字对应。
	// last_status 只记录终态（running 不是调度目录的状态，执行态在 run 行）。
	"worker_tasks.last_status": {"succeeded", "failed", "skipped"},
	"worker_task_runs.status":  {"pending", "running", "succeeded", "failed"},
	"worker_task_runs.trigger": {"schedule", "manual", "wakeup"},

	// 上游用量对账（迁移 20261028090000–20261028090400，2026-10-24）：
	// 账号状态 1=active / 2=disabled；快照来源 manual|csv；run 的触发者与
	// 终态（running 在开跑时写入，succeeded/partial/failed 为终态）；
	// 明细的差异等级与运营处置状态；完整性发现的严重度与处置状态。
	// usage_source / usage_semantic 属日志库，登记在 EnumChecksLogs。
	"upstream_accounts.status":                  {"1", "2"},
	"upstream_usage_snapshots.source":           {"manual", "csv"},
	"upstream_reconciliation_runs.triggered_by": {"worker", "manual"},
	"upstream_reconciliation_runs.status":       {"running", "succeeded", "partial", "failed"},
	"upstream_reconciliation_items.severity": {
		"ok", "warn", "alert", "partial_snapshot",
		"missing_snapshot", "missing_local", "local_expired",
	},
	"upstream_reconciliation_items.status": {"open", "acknowledged", "resolved"},
	"usage_integrity_findings.severity":    {"info", "warn", "alert"},
	"usage_integrity_findings.status":      {"open", "acknowledged", "resolved"},
}

// EnumChecksLogs 是日志库（migrations-logs 流）枚举列的「DB CHECK ↔ 期望值」对照表，
// 口径与 EnumChecks 相同：键为 "<table>.<column>"，值为 CHECK IN 白名单成员。
//
// 日志库与主库是物理分离的两个库（G7-R36），CHECK 必须在日志库连接上对账；
// 主库的 EnumChecks 只覆盖主库 wantColumns 表集（M0 16 + M1 6 + M6c 2 + M6d 4
// + D1 2 + D5a 3 + D7a 2 + B5a 1 + C2 2 + T4 2 + S1 1 + S4 1 + MJ-d 1 + W17-2 3
// + 上游对账 5 = 51 表），二者互不包含。日志库唯一的枚举型 CHECK
// 均登记于此（反向对账：日志库每个枚举 CHECK 必须在此有且仅有一个键）。
var EnumChecksLogs = map[string][]string{
	// usage_logs（platform-01 §12；smallint：0=refused / 1=success / 2=error / 3=refunded）
	"usage_logs.status": {"0", "1", "2", "3"},

	// usage_logs 对账维度（migrations-logs 20261028090500，2026-10-24）：
	// usage_semantic 0=真实计量 / 1=估算，列可空（NULL = 迁移前历史行未知，
	// 不得用 0 冒充）。usage_source 的 CHECK 是 BETWEEN 0 AND 3，PG 会规范化
	// 为 >= / <= 比较式而非 IN/ANY，不呈枚举形态，故不登记于此。
	"usage_logs.usage_semantic": {"0", "1"},

	// audit_logs（§5；R3 冻结的四类事件分类）
	"audit_logs.category": {"login", "security", "operation", "access_token"},
	"audit_logs.result":   {"success", "failed"},
}
