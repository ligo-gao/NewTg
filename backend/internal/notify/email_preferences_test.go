package notify

// W10-EMAIL 的纯函数/限流单测（不依赖 PostgreSQL）。
//
// 覆盖：10 条/秒令牌桶边界（前 10 立即、第 11 起 100ms 排队）、事务性类别
// 冻结集合、偏好默认值与 opt-out 语义、损坏偏好 fail-closed。

import (
	"testing"
	"time"
)

func TestEmailTokenBucketAllowsTenImmediateThenThrottles(t *testing.T) {
	bucket := newEmailTokenBucket(EmailRateLimitPerSecond, emailRateBurst)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	for i := 0; i < EmailRateLimitPerSecond; i++ {
		if wait := bucket.take(now); wait != 0 {
			t.Fatalf("第 %d 个令牌等待 %v，期望 0（burst=%d）", i+1, wait, emailRateBurst)
		}
	}
	// 第 11 个必须等到下一个 100ms 槽位。
	if wait := bucket.take(now); wait != 100*time.Millisecond {
		t.Fatalf("第 11 个令牌等待 %v，期望 100ms", wait)
	}
	// 第 12 个在未推进时钟时排队 200ms（等待时间可累加）。
	if wait := bucket.take(now); wait != 200*time.Millisecond {
		t.Fatalf("第 12 个令牌等待 %v，期望 200ms", wait)
	}
	// 时钟推进 200ms 后，之前的排队已被覆盖，下一次只等 100ms。
	later := now.Add(200 * time.Millisecond)
	if wait := bucket.take(later); wait != 100*time.Millisecond {
		t.Fatalf("推进时钟后等待 %v，期望 100ms", wait)
	}
}

func TestIsTransactionalEmailCategoryFrozenSet(t *testing.T) {
	transactional := []string{
		"auth.verify_code", "auth.password_reset", "auth.welcome",
		"security.alert",
		"billing.payment_success", "billing.payment_failed",
		"billing.order_expired", "billing.redemption_success",
		"billing.violation_fee", "system.smtp_test",
	}
	for _, category := range transactional {
		if !IsTransactionalEmailCategory(category) {
			t.Fatalf("%s 应属于事务性恒发集合", category)
		}
	}
	nonTransactional := []string{
		"billing.balance_low", "billing.quota_exhausted",
		"billing.usage_digest", "ops.channel_auto_disabled",
		"ops.redemption_expiring",
	}
	for _, category := range nonTransactional {
		if IsTransactionalEmailCategory(category) {
			t.Fatalf("%s 不应属于事务性集合", category)
		}
	}
}

func TestHasEmailPreferenceOnlyForUserFacingWarnings(t *testing.T) {
	for _, category := range []string{
		"billing.balance_low", "billing.quota_exhausted", "billing.usage_digest",
	} {
		if !HasEmailPreference(category) {
			t.Fatalf("%s 应有用户偏好槽位", category)
		}
	}
	for _, category := range []string{
		"auth.password_reset", "billing.payment_failed", "ops.channel_auto_disabled",
	} {
		if HasEmailPreference(category) {
			t.Fatalf("%s 不应有偏好槽位（恒发）", category)
		}
	}
}

func TestPreferencesAllowEmailDefaults(t *testing.T) {
	var zero NotificationPreferences
	if PreferencesAllowEmail(zero, "billing.balance_low") {
		t.Fatal("缺少偏好时 balance_warning 默认关闭，不应放行")
	}
	if !PreferencesAllowEmail(zero, "billing.payment_failed") {
		t.Fatal("事务性类别必须放行")
	}
	enabledNoChannels := NotificationPreferences{
		BalanceWarning: NotificationPreferenceCategory{Enabled: true},
	}
	if !PreferencesAllowEmail(enabledNoChannels, "billing.balance_low") {
		t.Fatal("enabled=true 且 channels 缺省时默认 email")
	}
	enabledNoEmail := NotificationPreferences{
		UsageDigest: NotificationPreferenceCategory{Enabled: true, Channels: []string{"webhook"}},
	}
	if PreferencesAllowEmail(enabledNoEmail, "billing.usage_digest") {
		t.Fatal("channels 不含 email 时不应放行")
	}
}

func TestParseNotificationPreferencesRejectsNonObject(t *testing.T) {
	if _, err := ParseNotificationPreferences([]byte(`"oops"`)); err == nil {
		t.Fatal("非对象偏好必须报错（调用方 fail-closed）")
	}
	if _, err := ParseNotificationPreferences([]byte(`{"balance_warning":{"enabled":"yes"}}`)); err == nil {
		t.Fatal("字段类型不符必须报错")
	}
	prefs, err := ParseNotificationPreferences(nil)
	if err != nil || prefs.BalanceWarning.Enabled {
		t.Fatalf("NULL 偏好应解析为零值：%+v %v", prefs, err)
	}
}

func TestEmailAddressForCategoryUsesCustomEmailOnlyForBalanceWarning(t *testing.T) {
	prefs := NotificationPreferences{
		BalanceWarning: NotificationPreferenceCategory{Enabled: true, CustomEmail: "alt@example.com"},
	}
	if got := EmailAddressForCategory(prefs, "billing.balance_low", "main@example.com"); got != "alt@example.com" {
		t.Fatalf("balance_low 应用 custom_email，得到 %q", got)
	}
	if got := EmailAddressForCategory(prefs, "billing.usage_digest", "main@example.com"); got != "main@example.com" {
		t.Fatalf("usage_digest 不应使用 balance_warning.custom_email，得到 %q", got)
	}
}
