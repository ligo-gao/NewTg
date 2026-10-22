package platform

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/settings"
)

// 本文件验收 W10-N4/N9 保留设置的三态映射（不依赖 PG/Redis）：
// 未配置 → 设计默认；显式 0 → 0（关闭）；正值 → 按值；负值/坏值 → 默认。

// registryDefault 读注册表默认值，确保装配层的 fallback 与 settings 注册表
// 不会各写一份而漂移（例如把 90 改成 30）。
func registryDefault(t *testing.T, key string) int64 {
	t.Helper()
	row, ok := settings.Lookup(key)
	if !ok {
		t.Fatalf("setting %q is not registered", key)
	}
	days, ok := row.Default.(int64)
	if !ok {
		t.Fatalf("setting %q default is %T, want int64", key, row.Default)
	}
	return days
}

func TestResolveRetentionDaysUnsetUsesDesignDefault(t *testing.T) {
	log, _ := newLimitsLog()
	fallback := int(registryDefault(t, settingRedemptionRetentionDays))
	got := resolveRetentionDays(fakeSettingReader{}, settingRedemptionRetentionDays, fallback, log)
	if got != fallback {
		t.Fatalf("unset = %d, want registry default %d", got, fallback)
	}
}

func TestResolveRetentionDaysExplicitZeroDisables(t *testing.T) {
	log, _ := newLimitsLog()
	got := resolveRetentionDays(fakeSettingReader{
		settingTaskRetentionTerminalDays: json.RawMessage(`0`),
	}, settingTaskRetentionTerminalDays, 30, log)
	if got != 0 {
		t.Fatalf("explicit zero = %d, want 0 (disabled)", got)
	}
}

func TestResolveRetentionDaysPositive(t *testing.T) {
	log, _ := newLimitsLog()
	got := resolveRetentionDays(fakeSettingReader{
		settingMJRetentionTerminalDays: json.RawMessage(`45`),
	}, settingMJRetentionTerminalDays, 30, log)
	if got != 45 {
		t.Fatalf("positive = %d, want 45", got)
	}
}

func TestResolveRetentionDaysInvalidFallsBack(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`"abc"`),
		json.RawMessage(`-1`),
	} {
		log, _ := newLimitsLog()
		got := resolveRetentionDays(fakeSettingReader{
			settingRedemptionRetentionDays: raw,
		}, settingRedemptionRetentionDays, 90, log)
		if got != 90 {
			t.Fatalf("invalid %s = %d, want fallback 90", string(raw), got)
		}
	}
}

func TestResolveRetentionDaysDurationOverflowFallsBack(t *testing.T) {
	// 2^31 天已在 int 范围内（64 位），但乘 24h 会溢出 time.Duration；
	// 上界必须按 Duration 的安全刻度而不是 int 宽度来判。
	for _, days := range []int64{
		int64(math.MaxInt32) + 1,
		int64(1) << 40,
		math.MaxInt64,
	} {
		log, _ := newLimitsLog()
		got := resolveRetentionDays(fakeSettingReader{
			settingRedemptionRetentionDays: json.RawMessage(strconv.FormatInt(days, 10)),
		}, settingRedemptionRetentionDays, 90, log)
		if got != 90 {
			t.Fatalf("%d days = %d, want fallback 90 (over %d)", days, got, maxRetentionDays)
		}
	}
}

func TestResolveRetentionDaysBoundaryAccepted(t *testing.T) {
	log, _ := newLimitsLog()
	got := resolveRetentionDays(fakeSettingReader{
		settingRedemptionRetentionDays: json.RawMessage(strconv.FormatInt(maxRetentionDays, 10)),
	}, settingRedemptionRetentionDays, 90, log)
	if int64(got) != maxRetentionDays {
		t.Fatalf("boundary %d days = %d, want accepted", maxRetentionDays, got)
	}
	// 上界 +1 必须回落。
	log, _ = newLimitsLog()
	got = resolveRetentionDays(fakeSettingReader{
		settingRedemptionRetentionDays: json.RawMessage(strconv.FormatInt(maxRetentionDays+1, 10)),
	}, settingRedemptionRetentionDays, 90, log)
	if got != 90 {
		t.Fatalf("boundary+1 = %d, want fallback 90", got)
	}
}

func TestRetentionRegistryDefaults(t *testing.T) {
	// 两族键必须是独立的注册行且默认 30；兑换码默认 90（设计预算）。
	if got := registryDefault(t, settingTaskRetentionTerminalDays); got != 30 {
		t.Fatalf("task.retention_terminal_days default = %d, want 30", got)
	}
	if got := registryDefault(t, settingMJRetentionTerminalDays); got != 30 {
		t.Fatalf("mj.retention_terminal_days default = %d, want 30", got)
	}
	if got := registryDefault(t, settingRedemptionRetentionDays); got != 90 {
		t.Fatalf("redemption.retention_days default = %d, want 90", got)
	}
}

// ---- W10-N10/N11：日志分区与审计保留的设置三态 ----

func TestLogRetentionSettingsDefaultsComeFromRegistry(t *testing.T) {
	log, _ := newLimitsLog()
	got := resolveLogRetentionSettings(fakeSettingReader{}, log)
	if got.PartitionLookaheadMonths != int(registryDefault(t, settingLogPartitionLookaheadMonths)) {
		t.Fatalf("lookahead = %d, want registry default %d",
			got.PartitionLookaheadMonths, registryDefault(t, settingLogPartitionLookaheadMonths))
	}
	if got.AuditRetentionMonths != int(registryDefault(t, settingAuditRetentionMonths)) {
		t.Fatalf("audit months = %d, want registry default %d",
			got.AuditRetentionMonths, registryDefault(t, settingAuditRetentionMonths))
	}
	if got.AuditIPEraseDays != int(registryDefault(t, settingAuditIPEraseDays)) {
		t.Fatalf("ip erase days = %d, want registry default %d",
			got.AuditIPEraseDays, registryDefault(t, settingAuditIPEraseDays))
	}
	// R-W10-03：破坏性开关的 fail-safe 默认必须是关闭。
	if got.PartitionDropEnabled {
		t.Fatal("partition drop must default to false (R-W10-03 A)")
	}
	if row, ok := settings.Lookup(settingLogPartitionDropEnabled); !ok {
		t.Fatalf("setting %q is not registered", settingLogPartitionDropEnabled)
	} else if row.Default != false {
		t.Fatalf("registry default for %q = %v, want false", settingLogPartitionDropEnabled, row.Default)
	}
}

func TestLogRetentionSettingsExplicitValues(t *testing.T) {
	log, _ := newLimitsLog()
	got := resolveLogRetentionSettings(fakeSettingReader{
		settingLogPartitionLookaheadMonths: json.RawMessage(`5`),
		settingLogPartitionDropEnabled:     json.RawMessage(`true`),
		settingAuditRetentionMonths:        json.RawMessage(`18`),
		settingAuditIPEraseDays:            json.RawMessage(`30`),
	}, log)
	if got.PartitionLookaheadMonths != 5 || !got.PartitionDropEnabled ||
		got.AuditRetentionMonths != 18 || got.AuditIPEraseDays != 30 {
		t.Fatalf("resolved = %+v, want {5 true 18 30}", got)
	}
}

func TestLogRetentionSettingsZeroSemantics(t *testing.T) {
	// 0 是冻结的唯一「关闭/不处理」写法，必须原样保留。
	log, _ := newLimitsLog()
	got := resolveLogRetentionSettings(fakeSettingReader{
		settingLogPartitionLookaheadMonths: json.RawMessage(`0`),
		settingAuditRetentionMonths:        json.RawMessage(`0`),
		settingAuditIPEraseDays:            json.RawMessage(`0`),
	}, log)
	if got.PartitionLookaheadMonths != 0 || got.AuditRetentionMonths != 0 || got.AuditIPEraseDays != 0 {
		t.Fatalf("zero semantics lost: %+v", got)
	}
}

func TestLogRetentionSettingsInvalidValuesFallBack(t *testing.T) {
	log, _ := newLimitsLog()
	got := resolveLogRetentionSettings(fakeSettingReader{
		settingLogPartitionLookaheadMonths: json.RawMessage(`-1`),
		// 脏布尔行绝不能被读成 true（否则会意外打开分区删除）。
		settingLogPartitionDropEnabled: json.RawMessage(`"yes"`),
		settingAuditRetentionMonths:    json.RawMessage(`null`),
		settingAuditIPEraseDays:        json.RawMessage(`"90"`),
	}, log)
	if got.PartitionLookaheadMonths != billing.DefaultPartitionLookaheadMonths {
		t.Fatalf("negative lookahead = %d, want default %d",
			got.PartitionLookaheadMonths, billing.DefaultPartitionLookaheadMonths)
	}
	if got.PartitionDropEnabled {
		t.Fatal("invalid bool must fall back to false, not true")
	}
	if got.AuditRetentionMonths != audit.DefaultRetentionMonths {
		t.Fatalf("null months = %d, want default %d", got.AuditRetentionMonths, audit.DefaultRetentionMonths)
	}
	if got.AuditIPEraseDays != audit.DefaultIPEraseDays {
		t.Fatalf("string days = %d, want default %d", got.AuditIPEraseDays, audit.DefaultIPEraseDays)
	}
}

// TestLogRetentionDefaultsAreFrozen 锁定 W10-N10/N11 的四个默认值。
//
// 这不是重复断言：lookahead 2→3（R-W10-04 A）、drop 默认关（R-W10-03 A）、
// 审计 12 个月/90 天（R-W10-09 A）都是控制端裁决；任何一处被改回旧值都必须
// 让测试变红，而不是悄悄改变保留边界。
func TestLogRetentionDefaultsAreFrozen(t *testing.T) {
	cases := []struct {
		key  string
		want int64
	}{
		{settingLogPartitionLookaheadMonths, 3},
		{settingAuditRetentionMonths, 12},
		{settingAuditIPEraseDays, 90},
	}
	for _, tc := range cases {
		if got := registryDefault(t, tc.key); got != tc.want {
			t.Fatalf("%s default = %d, want %d", tc.key, got, tc.want)
		}
	}
	if row, ok := settings.Lookup(settingLogPartitionDropEnabled); !ok {
		t.Fatalf("%s not registered", settingLogPartitionDropEnabled)
	} else if row.Default != false {
		t.Fatalf("%s default = %v, want false", settingLogPartitionDropEnabled, row.Default)
	}
	if billing.DefaultPartitionLookaheadMonths != 3 {
		t.Fatalf("billing lookahead fallback = %d, want 3", billing.DefaultPartitionLookaheadMonths)
	}
}

// TestOpenRetentionDBNeverFallsBackToAppDSN 锁定 fail-closed：
// 没有 DB_RETENTION_DSN 时必须返回 (nil, nil)，即使 app 日志库 DSN 存在。
// 变异（回退到 DB_LOGS_DSN）会让这里尝试连接一个假 DSN 并返回 error/非 nil。
func TestOpenRetentionDBNeverFallsBackToAppDSN(t *testing.T) {
	db, err := openRetentionDB(context.Background(), &config.Config{
		DBLogsDSN: "postgres://should-not-be-used@127.0.0.1:1/none?sslmode=disable",
	})
	if err != nil {
		t.Fatalf("empty retention DSN returned error %v, want (nil, nil)", err)
	}
	if db != nil {
		_ = db.Close()
		t.Fatal("empty retention DSN opened a database (must never fall back to app DSN)")
	}
}
