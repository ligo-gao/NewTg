package notify

// W10-EM2：用户通知偏好（users.notification_preferences）的读取与裁决。
//
// 设计权威：modules-01 F-01-22 字段表（modules-01:536-553）与 modules-08
// §3.9.5「（c）用户通知偏好」。冻结口径（R-W10-06）：
//   - 事务性类别（auth.* / security.* / billing.payment_* /
//     billing.order_expired / billing.redemption_success /
//     billing.violation_fee）**恒发**，偏好不得关闭；
//   - 只有 balance_warning（billing.balance_low / billing.quota_exhausted）
//     与 usage_digest（billing.usage_digest）受偏好闸门约束；
//   - 无偏好映射的类别（管理员侧 ops.*）同样恒发：设计明示「无映射者
//     （事务性 / 管理员侧）恒定发送」。
//
// 默认值来自 modules-01 字段表：balance_warning.enabled=false、
// usage_digest.enabled=false；enabled=true 且 channels 缺省时按 ["email"]
// 处理（与 PUT 合并层的补默认逻辑同一口径）。因此「没有任何偏好行」的
// 用户默认收不到这两类营销/提醒邮件，但密码重置、支付结果等事务性邮件
// 不受影响。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// NotificationPreferences 是本流程消费的偏好子集。未出现的类别保持零值
// （enabled=false），与设计默认值一致。
type NotificationPreferences struct {
	BalanceWarning NotificationPreferenceCategory `json:"balance_warning"`
	UsageDigest    NotificationPreferenceCategory `json:"usage_digest"`
}

// NotificationPreferenceCategory 是一个通知类别的偏好。
type NotificationPreferenceCategory struct {
	Enabled     bool     `json:"enabled"`
	Channels    []string `json:"channels"`
	CustomEmail string   `json:"custom_email"`
}

// ParseNotificationPreferences 解析 users.notification_preferences。
//
// 空/NULL → 零值（全部默认关闭）；非对象或字段类型不符 → 错误。调用方对
// 非事务性邮件必须把错误当作 opt-out（fail-closed），而不是当成「未设置」。
func ParseNotificationPreferences(raw []byte) (NotificationPreferences, error) {
	var prefs NotificationPreferences
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return prefs, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || probe == nil {
		return NotificationPreferences{}, fmt.Errorf("notify: notification preferences are not an object")
	}
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return NotificationPreferences{}, fmt.Errorf("notify: decode notification preferences: %w", err)
	}
	for _, category := range []struct {
		name  string
		value *NotificationPreferenceCategory
	}{
		{"balance_warning", &prefs.BalanceWarning},
		{"usage_digest", &prefs.UsageDigest},
	} {
		for _, channel := range category.value.Channels {
			if channel == "" {
				return NotificationPreferences{}, fmt.Errorf("notify: %s has an empty channel", category.name)
			}
		}
	}
	return prefs, nil
}

// IsTransactionalEmailCategory 报告该类别是否属于 R-W10-06 冻结的恒发集合。
//
// 判定按类别前缀而不是模板 key：注册表的 category 与 key 目前同名，但
// 冻结语义是「类别」维度的（auth.* 是通配），前缀匹配避免新增 key 时漏配。
// system.smtp_test 是管理员测试发送，同样不受用户偏好约束。
func IsTransactionalEmailCategory(category string) bool {
	switch {
	case strings.HasPrefix(category, "auth."),
		strings.HasPrefix(category, "security."),
		strings.HasPrefix(category, "billing.payment_"):
		return true
	}
	switch category {
	case "billing.order_expired", "billing.redemption_success",
		"billing.violation_fee", "system.smtp_test":
		return true
	}
	return false
}

// HasEmailPreference 报告该类别是否存在用户偏好槽位。
//
// false 表示事务性/管理员侧类别：调用方**不得**查询或应用偏好（恒发），
// 发送前复核也应跳过，避免多一次无意义的用户查询。billing.quota_exhausted
// 与 billing.balance_low 共用 balance_warning（modules-08 §3.9.1：避免两个
// 开关语义重叠）。
func HasEmailPreference(category string) bool {
	switch category {
	case "billing.balance_low", "billing.quota_exhausted", "billing.usage_digest":
		return true
	}
	return false
}

// PreferencesAllowEmail 判断偏好是否允许该类别走 email。
//
// 无映射的类别恒 true（恒发）；有映射但 enabled=false → false；enabled=true
// 且 channels 为空时按默认 ["email"] 放行（字段表默认值），channels 不含
// email → false。大小写按冻结枚举逐字匹配，不做宽松归一化。
func PreferencesAllowEmail(prefs NotificationPreferences, category string) bool {
	var slot NotificationPreferenceCategory
	switch category {
	case "billing.balance_low", "billing.quota_exhausted":
		slot = prefs.BalanceWarning
	case "billing.usage_digest":
		slot = prefs.UsageDigest
	default:
		// 事务性/管理员侧：没有可关闭的偏好。
		return true
	}
	if !slot.Enabled {
		return false
	}
	if len(slot.Channels) == 0 {
		return true
	}
	for _, channel := range slot.Channels {
		if channel == "email" {
			return true
		}
	}
	return false
}

// EmailAddressForCategory 返回该类别应投递的地址：balance_warning 的
// custom_email 覆盖账号邮箱（modules-01 F-01-22「地址显示账号邮箱，可改填
// 其他」）；其它类别没有 custom_email 字段，一律用账号邮箱。
func EmailAddressForCategory(prefs NotificationPreferences, category, accountEmail string) string {
	if category == "billing.balance_low" || category == "billing.quota_exhausted" {
		if custom := strings.TrimSpace(prefs.BalanceWarning.CustomEmail); custom != "" {
			return custom
		}
	}
	return strings.TrimSpace(accountEmail)
}
