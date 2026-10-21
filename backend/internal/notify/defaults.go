package notify

// 本文件是内置文案的**唯一代码真源**（§3.9.4）。迁移 SQL 刻意不重复文案：
// 若把 7 语言文案同时写进 SQL，改一句文案要动两处，必然漂移。
//
// 语言维度：设计 §3.9.4 只给出 zh-CN / en 两列基线（明示「实际按每种语言
// 一行存储，不在此枚举七列」）。本 bundle 因此只持有这两语文案；播种时
// 其余 5 语（zh-TW / fr / ru / ja / vi）以 en 文案落行，保证 UNIQUE(key,
// channel,locale) 的「每语言一行」模型成立。补齐译文是内容任务，不改变
// 变量契约与渲染路径。
//
// 文案中的 {{.name}} 必须全部命中该 (key,channel) 的封闭白名单；registry_test
// 会逐条静态校验，白名单外变量的文案在该测试里直接失败。

func init() {
	for i := range templateSpecs {
		spec := &templateSpecs[i]
		spec.Defaults = map[Locale]Content{}
		zh, zhOK := zhCNDefaults[spec.Key+"\x00"+string(spec.Channel)]
		en, enOK := enDefaults[spec.Key+"\x00"+string(spec.Channel)]
		if !zhOK || !enOK {
			panic("notify: missing built-in copy for " + spec.Key + "/" + string(spec.Channel))
		}
		spec.Defaults[LocaleZHCN] = zh
		spec.Defaults[LocaleEN] = en
	}
}

// 中文基线（zh-CN）。正文中的换行是真实换行，不是文档表格里的 <br> 标记。
var zhCNDefaults = map[string]Content{
	"auth.verify_code\x00email": {
		Subject: "{{.purpose_label}}验证码：{{.code}}",
		Body: "你好{{.username}}：\n" +
			"你正在进行{{.purpose_label}}，验证码为：\n" +
			"**{{.code}}**\n" +
			"验证码 {{.expires_minutes}} 分钟内有效，请勿转发给他人。若非你本人操作，请忽略本邮件。",
	},
	"auth.password_reset\x00email": {
		Subject: "重置你的密码",
		Body: "你好{{.username}}：\n" +
			"我们收到了重置密码的请求。请点击下方按钮设置新密码（{{.expires_minutes}} 分钟内有效）。\n" +
			"若你没有发起此请求，请忽略本邮件，你的密码不会改变。\n" +
			"请求来源 IP：{{.ip}}",
		CTAText: "重置密码",
		CTAURL:  "{{.reset_url}}",
	},
	"auth.welcome\x00email": {
		Subject: "欢迎加入 {{.site_title}}",
		Body: "你好{{.username}}：\n" +
			"欢迎加入 {{.site_title}}，你的账号已创建成功。\n" +
			"{{.welcome_bonus}}\n" +
			"前往控制台创建 API 令牌，即可开始调用模型。",
		CTAText: "进入控制台",
		CTAURL:  "{{.dashboard_url}}",
	},
	"security.alert\x00email": {
		Subject: "安全提醒：{{.event_title}}",
		Body: "你好{{.username}}：\n" +
			"{{.event_detail}}\n" +
			"时间：{{.event_time}}\n" +
			"位置：{{.ip}} {{.device}}\n" +
			"{{.action_hint}}",
		CTAText: "查看安全设置",
		CTAURL:  "{{.security_url}}",
	},
	"billing.balance_low\x00email": {
		Subject: "【{{.site_title}}】余额不足提醒",
		Body: "你好{{.username}}：\n" +
			"你的账户余额为 {{.balance}} 积分，已低于提醒阈值 {{.threshold}} 积分。为避免请求被拒绝，建议及时充值。",
		CTAText: "立即充值",
		CTAURL:  "{{.topup_url}}",
	},
	"billing.balance_low\x00inapp": {
		Body: "余额不足：当前 {{.balance}} 积分（阈值 {{.threshold}}）。[立即充值]({{.topup_url}})",
	},
	"billing.quota_exhausted\x00email": {
		Subject: "【{{.site_title}}】余额已耗尽，API 请求已暂停",
		Body: "你好{{.username}}：\n" +
			"你的账户余额已耗尽（当前 {{.balance}} 积分），API 请求已被暂停。充值后会自动恢复。",
		CTAText: "立即充值",
		CTAURL:  "{{.topup_url}}",
	},
	"billing.quota_exhausted\x00inapp": {
		Body: "余额已耗尽，API 请求已暂停。[立即充值]({{.topup_url}})",
	},
	"billing.usage_digest\x00email": {
		Subject: "你的用量周报（{{.period_label}}）",
		Body: "你好{{.username}}：\n" +
			"以下是你在 {{.period_label}} 的用量概览：\n" +
			"- 消耗积分：{{.total_credits}}\n" +
			"- 请求次数：{{.request_count}}\n" +
			"- 常用模型：{{.top_models}}",
		CTAText: "查看完整用量",
		CTAURL:  "{{.dashboard_url}}",
	},
	"billing.payment_success\x00email": {
		Subject: "【充值成功】+{{.credits}} 积分已到账",
		Body: "你好{{.username}}：\n" +
			"你的充值已到账：\n" +
			"- 实付金额：{{.amount}}\n" +
			"- 到账积分：{{.credits}}\n" +
			"- 订单号：{{.order_no}}\n" +
			"- 支付时间：{{.paid_at}}",
		CTAText: "查看钱包",
		CTAURL:  "{{.wallet_url}}",
	},
	"billing.payment_failed\x00email": {
		Subject: "【充值未完成】订单 {{.order_no}}",
		Body: "你好{{.username}}：\n" +
			"你的充值订单未能完成：\n" +
			"- 订单金额：{{.amount}}\n" +
			"- 订单号：{{.order_no}}\n" +
			"- 原因：{{.reason_label}}\n" +
			"- 时间：{{.failed_at}}\n" +
			"本次未产生扣款，你可稍后重试。",
		CTAText: "重新支付",
		CTAURL:  "{{.retry_url}}",
	},
	"billing.order_expired\x00email": {
		Subject: "【订单已过期】{{.order_no}}",
		Body: "你好{{.username}}：\n" +
			"你的充值订单因超时未支付已被关闭：\n" +
			"- 订单金额：{{.amount}}\n" +
			"- 订单号：{{.order_no}}\n" +
			"- 关闭时间：{{.expired_at}}\n" +
			"如需充值，请重新下单。",
		CTAText: "重新充值",
		CTAURL:  "{{.topup_url}}",
	},
	"billing.redemption_success\x00email": {
		Subject: "【兑换成功】+{{.credits}} 积分已到账",
		Body: "你好{{.username}}：\n" +
			"兑换码 {{.code_hint}} 已成功兑换：\n" +
			"- 到账积分：{{.credits}}\n" +
			"- 兑换后余额：{{.balance}}\n" +
			"- 兑换时间：{{.redeemed_at}}",
		CTAText: "查看钱包",
		CTAURL:  "{{.wallet_url}}",
	},
	"billing.violation_fee\x00email": {
		Subject: "违规费用通知",
		Body: "你好{{.username}}：\n" +
			"你的账号因以下原因产生了一笔违规费用：{{.reason_label}}。\n" +
			"- 扣费积分：{{.fee}}\n" +
			"- 时间：{{.occurred_at}}\n" +
			"- 扣费后余额：{{.balance}}\n" +
			"请查阅可接受使用政策。",
		CTAText: "查看政策",
		CTAURL:  "{{.policy_url}}",
	},
	"system.announcement\x00inapp": {
		Body: "**{{.title}}** {{.summary}} [查看详情]({{.link_url}})",
	},
	"system.smtp_test\x00email": {
		Subject: "【{{.site_title}}】SMTP 测试邮件",
		Body: "这是一封来自 {{.site_title}} 的测试邮件。\n" +
			"发信时间：{{.sent_at}}\n" +
			"发信人：{{.admin_username}}\n" +
			"收到本邮件说明 SMTP 配置有效。",
	},
	"ops.channel_auto_disabled\x00email": {
		Subject: "【渠道自动禁用】{{.channel_name}}",
		Body: "管理员你好{{.admin_username}}：\n" +
			"渠道 {{.channel_name}}（ID {{.channel_id}}）已被系统自动禁用。\n" +
			"原因：{{.reason_label}}\n" +
			"时间：{{.disabled_at}}\n" +
			"请检查后手动启用。",
		CTAText: "前往渠道",
		CTAURL:  "{{.channels_url}}",
	},
	"ops.channel_auto_disabled\x00inapp": {
		Body: "{{.title}}：{{.channel_name}}（ID {{.channel_id}}）。" +
			"原因：{{.reason_label}}。时间：{{.disabled_at}}。[前往渠道]({{.channels_url}})",
	},
	"ops.redemption_expiring\x00email": {
		Subject: "兑换码批次即将过期：{{.batch_name}}",
		Body: "管理员你好{{.admin_username}}：\n" +
			"批次「{{.batch_name}}」有 {{.remaining_count}} 个兑换码将于 {{.expires_at}} 过期。\n" +
			"请及时处理。",
		CTAText: "查看批次",
		CTAURL:  "{{.codes_url}}",
	},
}

// localizedSample 给出预览用的语言敏感样例。代码内置供给值（purpose_label /
// event_* / action_hint / reason_label / welcome_bonus）在真实发送时由业务层
// 按语言拼装（§3.9.3 供给表）；预览没有业务上下文，只能给样例。英文预览若
// 用中文样例会让人误判模板，故在此补一套 en 样例。
func localizedSample(locale Locale, key string, variable Variable) string {
	if locale != LocaleEN {
		return variable.Sample
	}
	switch variable.Name {
	case "purpose_label":
		return "Sign-up verification"
	case "event_title":
		return "Password changed"
	case "event_detail":
		return "Your account password was just changed."
	case "action_hint":
		return "If this wasn't you, reset your password now."
	case "welcome_bonus":
		return "We've added 1,000 credits to your account."
	case "title":
		if key == "ops.channel_auto_disabled" {
			return "Channel auto-disabled"
		}
	case "reason_label":
		switch key {
		case "billing.payment_failed":
			return "Card declined"
		case "billing.violation_fee":
			return "Upstream policy violation"
		case "ops.channel_auto_disabled":
			return "Upstream auth error"
		}
	}
	return variable.Sample
}

// 英文基线（en），同时作为其余 5 语播种时的回退文案。
var enDefaults = map[string]Content{
	"auth.verify_code\x00email": {
		Subject: "Your {{.purpose_label}} code: {{.code}}",
		Body: "Hi {{.username}},\n" +
			"Your {{.purpose_label}} code is:\n" +
			"**{{.code}}**\n" +
			"This code expires in {{.expires_minutes}} minutes. Never share it with anyone. If you didn't request it, you can ignore this email.",
	},
	"auth.password_reset\x00email": {
		Subject: "Reset your password",
		Body: "Hi {{.username}},\n" +
			"We received a request to reset your password. Use the button below to set a new one (valid for {{.expires_minutes}} minutes).\n" +
			"If you didn't request this, ignore this email — your password will not change.\n" +
			"Requested from IP: {{.ip}}",
		CTAText: "Reset password",
		CTAURL:  "{{.reset_url}}",
	},
	"auth.welcome\x00email": {
		Subject: "Welcome to {{.site_title}}",
		Body: "Hi {{.username}},\n" +
			"Welcome to {{.site_title}}. Your account is ready.\n" +
			"{{.welcome_bonus}}\n" +
			"Create an API token in the console to start calling models.",
		CTAText: "Open console",
		CTAURL:  "{{.dashboard_url}}",
	},
	"security.alert\x00email": {
		Subject: "Security alert: {{.event_title}}",
		Body: "Hi {{.username}},\n" +
			"{{.event_detail}}\n" +
			"Time: {{.event_time}}\n" +
			"From: {{.ip}} {{.device}}\n" +
			"{{.action_hint}}",
		CTAText: "Review security",
		CTAURL:  "{{.security_url}}",
	},
	"billing.balance_low\x00email": {
		Subject: "[{{.site_title}}] Low balance warning",
		Body: "Hi {{.username}},\n" +
			"Your balance is {{.balance}} credits, below your alert threshold of {{.threshold}}. Top up to avoid request failures.",
		CTAText: "Top up",
		CTAURL:  "{{.topup_url}}",
	},
	"billing.balance_low\x00inapp": {
		Body: "Low balance: {{.balance}} credits left (threshold {{.threshold}}). [Top up]({{.topup_url}})",
	},
	"billing.quota_exhausted\x00email": {
		Subject: "[{{.site_title}}] Balance depleted — API requests paused",
		Body: "Hi {{.username}},\n" +
			"Your balance is depleted ({{.balance}} credits) and API requests are paused. They resume automatically after a top-up.",
		CTAText: "Top up",
		CTAURL:  "{{.topup_url}}",
	},
	"billing.quota_exhausted\x00inapp": {
		Body: "Balance depleted. API requests are paused. [Top up]({{.topup_url}})",
	},
	"billing.usage_digest\x00email": {
		Subject: "Your usage digest ({{.period_label}})",
		Body: "Hi {{.username}},\n" +
			"Here is your usage for {{.period_label}}:\n" +
			"- Credits consumed: {{.total_credits}}\n" +
			"- Requests: {{.request_count}}\n" +
			"- Top models: {{.top_models}}",
		CTAText: "View usage",
		CTAURL:  "{{.dashboard_url}}",
	},
	"billing.payment_success\x00email": {
		Subject: "[Top-up complete] +{{.credits}} credits added",
		Body: "Hi {{.username}},\n" +
			"Your top-up is complete:\n" +
			"- Amount paid: {{.amount}}\n" +
			"- Credits added: {{.credits}}\n" +
			"- Order: {{.order_no}}\n" +
			"- Paid at: {{.paid_at}}",
		CTAText: "View wallet",
		CTAURL:  "{{.wallet_url}}",
	},
	"billing.payment_failed\x00email": {
		Subject: "[Top-up failed] Order {{.order_no}}",
		Body: "Hi {{.username}},\n" +
			"Your top-up order could not be completed:\n" +
			"- Order amount: {{.amount}}\n" +
			"- Order: {{.order_no}}\n" +
			"- Reason: {{.reason_label}}\n" +
			"- Time: {{.failed_at}}\n" +
			"No charge was made. You can try again later.",
		CTAText: "Try again",
		CTAURL:  "{{.retry_url}}",
	},
	"billing.order_expired\x00email": {
		Subject: "[Order expired] {{.order_no}}",
		Body: "Hi {{.username}},\n" +
			"Your top-up order was closed because it was not paid in time:\n" +
			"- Order amount: {{.amount}}\n" +
			"- Order: {{.order_no}}\n" +
			"- Closed at: {{.expired_at}}\n" +
			"To top up, please place a new order.",
		CTAText: "New order",
		CTAURL:  "{{.topup_url}}",
	},
	"billing.redemption_success\x00email": {
		Subject: "[Redeemed] +{{.credits}} credits added",
		Body: "Hi {{.username}},\n" +
			"Redemption code {{.code_hint}} was redeemed successfully:\n" +
			"- Credits added: {{.credits}}\n" +
			"- Balance after: {{.balance}}\n" +
			"- Redeemed at: {{.redeemed_at}}",
		CTAText: "View wallet",
		CTAURL:  "{{.wallet_url}}",
	},
	"billing.violation_fee\x00email": {
		Subject: "Violation fee notice",
		Body: "Hi {{.username}},\n" +
			"A violation fee was charged to your account for: {{.reason_label}}.\n" +
			"- Fee: {{.fee}} credits\n" +
			"- Time: {{.occurred_at}}\n" +
			"- Balance after: {{.balance}}\n" +
			"Please review our acceptable use policy.",
		CTAText: "View policy",
		CTAURL:  "{{.policy_url}}",
	},
	"system.announcement\x00inapp": {
		Body: "**{{.title}}** {{.summary}} [View details]({{.link_url}})",
	},
	"system.smtp_test\x00email": {
		Subject: "[{{.site_title}}] SMTP test email",
		Body: "This is a test email from {{.site_title}}.\n" +
			"Sent at: {{.sent_at}}\n" +
			"Requested by: {{.admin_username}}\n" +
			"Receiving this message means your SMTP configuration works.",
	},
	"ops.channel_auto_disabled\x00email": {
		Subject: "[Channel auto-disabled] {{.channel_name}}",
		Body: "Hi {{.admin_username}},\n" +
			"Channel {{.channel_name}} (ID {{.channel_id}}) was auto-disabled by the system.\n" +
			"Reason: {{.reason_label}}\n" +
			"Time: {{.disabled_at}}\n" +
			"Please review and re-enable it manually.",
		CTAText: "Open channel",
		CTAURL:  "{{.channels_url}}",
	},
	"ops.channel_auto_disabled\x00inapp": {
		Body: "{{.title}}: {{.channel_name}} (ID {{.channel_id}}). " +
			"Reason: {{.reason_label}}. Time: {{.disabled_at}}. [Open channels]({{.channels_url}})",
	},
	"ops.redemption_expiring\x00email": {
		Subject: "Redemption code batch expiring soon: {{.batch_name}}",
		Body: "Hi {{.admin_username}},\n" +
			"Batch \"{{.batch_name}}\" has {{.remaining_count}} unused code(s) expiring at {{.expires_at}}.\n" +
			"Please review in time.",
		CTAText: "View codes",
		CTAURL:  "{{.codes_url}}",
	},
}
