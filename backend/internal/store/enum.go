// Package store 承载主库 GORM 模型与「DB CHECK ↔ Go 常量」对照表。
//
// DDL/DML 纪律（platform-04 §3.7 V8）：保留字列 "group"（users/api_tokens/
// group_pricing）与 "key"（system_settings）在一切 SQL 与 GORM 映射中必须
// 双引号引用；GORM 模型填充（含 column tag）随 M1+ 各里程碑，M0 只有空壳。
package store

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
	"orders.provider": {"stripe", "alipay", "creem", "waffo", "waffo_pancake", "epay"},
	"orders.status":   {"pending", "succeeded", "failed", "expired", "cancelled"},
	"orders.failure_code": {
		"provider_unavailable", "provider_rejected", "signature_invalid", "amount_mismatch",
		"currency_mismatch", "order_not_found", "order_state_conflict", "manual_required",
	},

	// wallet_transactions（§11）
	"wallet_transactions.type": {
		"topup", "redemption", "consume_pre", "consume", "release",
		"reversal", "admin_adjustment", "bonus", "affiliate_transfer", "checkin_bonus",
	},
	"wallet_transactions.status": {"hold", "settled", "released", "expired", "confirmed"},
	"wallet_transactions.reference_type": {
		"order", "redemption", "usage_log", "admin_op", "reversal",
		"signup_grant", "promo", "checkin", "affiliate_transfer",
	},

	// rankings_*（§13/§16/§17）
	"rankings_snapshot_runs.period":   {"today", "week", "month", "year"},
	"rankings_snapshot_runs.status":   {"ok", "partial", "empty"},
	"rankings_timeseries.granularity": {"day", "hour"},
	"rankings_entities.kind":          {"model", "author"},

	// request_registry（§18 / platform-04 V9）
	"request_registry.state": {"in_flight", "settled", "refunded", "failed"},
}

// EnumChecksLogs 是日志库（migrations-logs 流）枚举列的「DB CHECK ↔ 期望值」对照表，
// 口径与 EnumChecks 相同：键为 "<table>.<column>"，值为 CHECK IN 白名单成员。
//
// 日志库与主库是物理分离的两个库（G7-R36），CHECK 必须在日志库连接上对账；
// 主库的 EnumChecks 只覆盖主库 16 表，二者互不包含。日志库唯一的枚举型 CHECK
// 均登记于此（反向对账：日志库每个枚举 CHECK 必须在此有且仅有一个键）。
var EnumChecksLogs = map[string][]string{
	// usage_logs（platform-01 §12；smallint：0=refused / 1=success / 2=error / 3=refunded）
	"usage_logs.status": {"0", "1", "2", "3"},

	// audit_logs（§5；R3 冻结的四类事件分类）
	"audit_logs.category": {"login", "security", "operation", "access_token"},
	"audit_logs.result":   {"success", "failed"},
}
