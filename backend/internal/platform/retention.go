package platform

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/tasks"
)

// 本文件装配 W10 的两组保留清理循环（N4 兑换码回收 + N9 任务保留）。
//
// 为什么放在 worker 角色：两者都是后台维护，与请求生命周期无关；gateway
// 不持有清理所需的长期连接与租约。单副本约束与计费兜底/任务轮询一致
// （M3 R-04），跨副本由 Redis `SET NX PX` 租约收敛（见各包的实现）。
//
// 【生效时机】设置键在 worker 启动时解析一次快照：与 W9-B4 的任务轮询
// 上限同口径（平台装配期解析，改动随 worker 重建生效）。保留期以天计，
// 热更新带来的收益远小于「两个副本看到不同删除边界」的风险；需要变更
// 时滚动重启 worker 即可。

const (
	// W10-N4（R-W10-05）：兑换码回收保留期。默认 90 天与设计一致，
	// 0 = 关闭（不物化 expired_at、不改 status）。
	settingRedemptionRetentionDays = "redemption.retention_days"
	// W10-N9（R-W10-10）：插件族终态任务保留期，默认 30 天，0 = 关闭。
	settingTaskRetentionTerminalDays = "task.retention_terminal_days"
	// W10-N9（R-W10-10）：MJ 族终态任务保留期，默认 30 天，0 = 关闭。
	// 与插件族**分开**，两族互不影响。
	settingMJRetentionTerminalDays = "mj.retention_terminal_days"
)

// startRetentionWorkers 解析三组保留设置并启动两个后台循环，返回停止函数。
//
// 停止函数取消上下文并等待两个循环退出；调用方必须在关闭 Redis/DB 之前
// 调用，否则进行中的 DELETE 会拿到「连接已关闭」错误，把正常停机噪声
// 打进告警。两个循环各自独立：任一失败只记录，不影响另一个。
func startRetentionWorkers(parent context.Context, db *sql.DB, rdb *redis.Client,
	center *configcenter.Center, node, instanceID string, log *slog.Logger) func() {

	if log == nil {
		log = slog.Default()
	}
	redemptionDays := resolveRetentionDays(center, settingRedemptionRetentionDays,
		billing.DefaultRedemptionRetentionDays, log)
	pluginDays := resolveRetentionDays(center, settingTaskRetentionTerminalDays,
		tasks.DefaultRetentionTerminalDays, log)
	mjDays := resolveRetentionDays(center, settingMJRetentionTerminalDays,
		tasks.DefaultRetentionTerminalDays, log)
	log.Info("worker: retention settings resolved",
		"redemption_retention_days", redemptionDays,
		"plugin_task_retention_days", pluginDays,
		"mj_task_retention_days", mjDays)

	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := tasks.RunRetentionLoop(ctx, tasks.RetentionDeps{
			DB:                  db,
			Redis:               rdb,
			Logger:              log,
			PluginRetentionDays: pluginDays,
			MJRetentionDays:     mjDays,
			Node:                node,
			InstanceID:          instanceID,
		}); err != nil {
			log.Error("task retention worker exited with error", "error", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := billing.RunRedemptionRetentionLoop(ctx, billing.RedemptionRetentionDeps{
			DB:            db,
			Redis:         rdb,
			Logger:        log,
			RetentionDays: redemptionDays,
			Node:          node,
			InstanceID:    instanceID,
		}); err != nil {
			log.Error("redemption retention worker exited with error", "error", err)
		}
	}()

	return func() {
		cancel()
		wg.Wait()
	}
}

// maxRetentionDays 是保留天数的硬上界（约 106751 天 ≈ 292 年）：
// `time.Duration` 的纳秒刻度是 int64，超过这个天数再乘 24h 会溢出成负数，
// 让 cutoff 变成未来（提前全删）或远古（永不删）。注册表本身限制 3650 天，
// 但 configcenter 的历史行/手工写入不受注册表校验，读取侧仍需防御。
// 106751 * 24h ≈ 9.22e18ns < MaxInt64，恰好是安全的最大整数值。
var maxRetentionDays = int64(math.MaxInt64) / int64(24*time.Hour)

// resolveRetentionDays 把设置中心的键与设计默认合并成生效天数。
//
// 三态冻结（与 taskPollLimits 同口径）：
//   - 键未落库 / JSON null / 非法 / 负值 → 回落设计默认（readSettingInt
//     已对后三种发出 warn）。负值不是「关闭」的另一种写法：设计只冻结了
//     0 = 关闭，静默接受负数会让运营以为清理已关而实际仍在按默认值执行。
//   - 显式 0 → 原样返回 0（关闭）。
//   - 正值 → 透传；超过 maxRetentionDays 的值回落默认并 warn，避免
//     Duration 换算溢出后把「关闭」和「巨大保留期」混为一谈。
func resolveRetentionDays(reader settingReader, key string, fallback int, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}
	days, ok := readSettingInt(reader, key, log)
	if !ok {
		return fallback
	}
	if days > maxRetentionDays {
		log.Warn("worker: retention setting is too large (duration overflow); falling back to the design default",
			"key", key, "days", days, "max_days", maxRetentionDays, "fallback", fallback)
		return fallback
	}
	return int(days)
}
