package platform

// 本文件是上游用量对账的设置适配器（上游用量对账设计 §4.7）。
//
// reconcile.SettingsReader 的生产实现：从配置中心内存快照读取
// reconcile.* 五个键与 billing.quota_per_unit。读取口径与既有保留设置一致
// （readSettingBool / readSettingInt）：键未落库、JSON null、错类型、负值
// 都回落 reconcile.DefaultSettings()，绝不 panic。
//
// Retention 不是注册表键：usage_logs 的保留窗口是 RunnerConfig.Retention，
// 由装配层填 reconcile.DefaultRetention（90 天）。
//
// 【为什么读取侧不做注册表上界钳制】这些参数只影响判定容差与查询窗口，
// 不像分区删除那样有破坏性动作；写入路径已由 settings 注册表的 min/max
// 校验兜底。读取侧的职责只有「脏值不改变默认语义」。

import (
	"context"
	"log/slog"

	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/reconcile"
)

const (
	// 键名与 settings 注册表（internal/settings/registry.go §4.7 #72–#77）
	// 逐字一致。
	settingReconcileEnabled              = "reconcile.enabled"
	settingReconcileLookbackDays         = "reconcile.lookback_days"
	settingReconcileTokenToleranceBP     = "reconcile.token_tolerance_bp"
	settingReconcileMinDeltaTokens       = "reconcile.min_delta_tokens"
	settingReconcileAmountToleranceMinor = "reconcile.amount_tolerance_minor"
	// billing.quota_per_unit 是「1 额度单位 = 多少额度」的换算比例（默认 100），
	// 与计费引擎共用同一个键，避免对账收入与账单口径漂移。
	settingBillingQuotaPerUnit = "billing.quota_per_unit"
)

// 编译期确认适配器满足 runner 的读取缝，避免 HTTP 装配层复用时才发现漂移。
var _ reconcile.SettingsReader = (*ReconcileSettingsReader)(nil)

// ReconcileSettingsReader 是 reconcile.SettingsReader 的生产实现。
//
// 并发安全：配置中心快照本身就是并发安全的只读视图；本类型无可变状态。
type ReconcileSettingsReader struct {
	reader settingReader
	log    *slog.Logger
}

// NewReconcileSettingsReader 构造适配器；center 为 nil 时按注册表默认值
// 运行（装配不完整不等于对账停摆）。log 为 nil 时使用 slog.Default()。
//
// 导出给 HTTP 装配层复用：管理面预览/手动触发必须读到与调度器相同的口径。
func NewReconcileSettingsReader(center *configcenter.Center, log *slog.Logger) *ReconcileSettingsReader {
	return newReconcileSettingsReader(configCenterReader(center), log)
}

// newReconcileSettingsReader 是测试接缝：接受任意 settingReader，避免单测
// 依赖 PG/Redis。
func newReconcileSettingsReader(reader settingReader, log *slog.Logger) *ReconcileSettingsReader {
	if log == nil {
		log = slog.Default()
	}
	return &ReconcileSettingsReader{reader: reader, log: log}
}

// configCenterReader 把 *configcenter.Center 收窄成最小读取接口。
//
// 显式判 nil 再转接口：typed-nil（值为 nil 的 *Center）装进接口后不等于
// nil，readSetting* 的 nil 守卫会失效，随后在 Center.Get 里解引用 nil 锁
// 直接 panic。
func configCenterReader(center *configcenter.Center) settingReader {
	if center == nil {
		return nil
	}
	return center
}

// ReconcileSettings 读取一次对账设置快照。
//
// ctx 只为满足接口：配置中心的内存快照读取不触库，也不阻塞。
func (r *ReconcileSettingsReader) ReconcileSettings(_ context.Context) (reconcile.Settings, error) {
	defaults := reconcile.DefaultSettings()
	return reconcile.Settings{
		Enabled: readSettingBool(r.reader, settingReconcileEnabled, defaults.Enabled, r.log),
		LookbackDays: int(reconcileSettingInt(r.reader, settingReconcileLookbackDays,
			int64(defaults.LookbackDays), r.log)),
		TokenBP: int(reconcileSettingInt(r.reader, settingReconcileTokenToleranceBP,
			int64(defaults.TokenBP), r.log)),
		MinDeltaTokens: reconcileSettingInt(r.reader, settingReconcileMinDeltaTokens,
			defaults.MinDeltaTokens, r.log),
		AmountToleranceMinor: reconcileSettingInt(r.reader, settingReconcileAmountToleranceMinor,
			defaults.AmountToleranceMinor, r.log),
		QuotaPerUnit: reconcileSettingInt(r.reader, settingBillingQuotaPerUnit,
			defaults.QuotaPerUnit, r.log),
	}, nil
}

// Enabled 是调度 pass 与手动触发的运行时谓词。缺省/脏值按开启处理，与
// 注册表默认值一致：关闭必须是显式 false。
func (r *ReconcileSettingsReader) Enabled() bool {
	return readSettingBool(r.reader, settingReconcileEnabled,
		reconcile.DefaultSettings().Enabled, r.log)
}

// reconcileEnabled 是 registry.SetEnabled 的谓词入口（每小时/每次 pass 都会
// 调用，只读内存快照）。
func reconcileEnabled(center *configcenter.Center) bool {
	return NewReconcileSettingsReader(center, nil).Enabled()
}

// reconcileSettingInt 把 readSettingInt 的 (value, ok) 折成「带默认值的值」。
func reconcileSettingInt(reader settingReader, key string, fallback int64, log *slog.Logger) int64 {
	value, ok := readSettingInt(reader, key, log)
	if !ok {
		return fallback
	}
	return value
}
