package platform

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

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
