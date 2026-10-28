package notify

// W10-EMAIL 的纯函数/限流单测（不依赖 PostgreSQL）。
//
// 覆盖：10 条/秒令牌桶边界（前 10 立即、第 11 起 100ms 排队）、事务性类别
// 冻结集合、偏好默认值与 opt-out 语义、损坏偏好 fail-closed。

import (
	"testing"
	"time"
)

func TestEmailRetryDelaysMatchWebhookSchedule(t *testing.T) {
	// R-W10-06 冻结「与 webhook 同范式」。两个数组刻意独立声明，这里把
	// 「当前一致」钉成断言：只改一侧时必须显式改测试并确认是产品决策。
	if len(emailRetryDelays) != len(webhookRetryDelays) {
		t.Fatalf("email/webhook 重试次数不同：%d vs %d", len(emailRetryDelays), len(webhookRetryDelays))
	}
	for i := range webhookRetryDelays {
		if emailRetryDelays[i] != webhookRetryDelays[i] {
			t.Fatalf("第 %d 个重试间隔不同：%v vs %v", i+1, emailRetryDelays[i], webhookRetryDelays[i])
		}
	}
	if emailMaxAttempts != len(emailRetryDelays)+1 {
		t.Fatalf("emailMaxAttempts = %d，期望初投 + %d 次重试", emailMaxAttempts, len(emailRetryDelays))
	}
}

func TestDecodeEmailVariablesRejectsNonObjectPayload(t *testing.T) {
	if _, problem := decodeEmailVariables([]byte(`"not-an-object"`)); problem != "email payload is invalid" {
		t.Fatalf("非对象 payload = %q，期望 email payload is invalid", problem)
	}
	if _, problem := decodeEmailVariables([]byte(`[1,2]`)); problem != "email payload is invalid" {
		t.Fatalf("数组 payload = %q", problem)
	}
	vars, problem := decodeEmailVariables([]byte(`{"order_no":"X"}`))
	if problem != "" || vars["order_no"] != "X" {
		t.Fatalf("合法 payload 解码失败：%v %q", vars, problem)
	}
	if vars, problem := decodeEmailVariables(nil); problem != "" || len(vars) != 0 {
		t.Fatalf("空 payload = %v %q", vars, problem)
	}
}

func TestEmailDedupeKeyForcesChannelPrefix(t *testing.T) {
	cases := map[string]string{
		"":                         "email:",
		"plain:1":                  "email:plain:1",
		"email:already":            "email:already",
		"  email:spaced  ":         "email:spaced",
		"balance_low:7:2026-10-22": "email:balance_low:7:2026-10-22",
	}
	for input, want := range cases {
		if got := emailDedupeKey(input); got != want {
			t.Fatalf("emailDedupeKey(%q) = %q，期望 %q", input, got, want)
		}
	}
}

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

// TestEmailTokenBucketIdleDoesNotBankCredits 冻结 P2-3 的修复：空闲期不得把
// 令牌攒成「信贷」。
//
// 原实现的 next 只在 take 时推进，空闲 10 分钟后 next 仍停在很久以前，
// 之后的每一次 take 都满足 scheduled.Before(now)，于是可以连续放行任意多条
// 邮件——burst 形同虚设，发信域名可能被上游判成滥用。修复后空闲最多攒
// burst 个令牌：前 burst 次立即通过，第 burst+1 次必须等一个间隔。
func TestEmailTokenBucketIdleDoesNotBankCredits(t *testing.T) {
	bucket := newEmailTokenBucket(EmailRateLimitPerSecond, emailRateBurst)
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

	// 先用掉初始 burst，让 next 处于稳态。
	for i := 0; i < emailRateBurst; i++ {
		bucket.take(now)
	}

	idle := now.Add(10 * time.Minute)
	for i := 0; i < emailRateBurst; i++ {
		if wait := bucket.take(idle); wait != 0 {
			t.Fatalf("空闲后第 %d 个令牌等待 %v，期望 0（burst=%d）", i+1, wait, emailRateBurst)
		}
	}
	if wait := bucket.take(idle); wait != 100*time.Millisecond {
		t.Fatalf("空闲后第 %d 个令牌等待 %v，期望 100ms（空闲不得累积超过 burst 个令牌）",
			emailRateBurst+1, wait)
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
