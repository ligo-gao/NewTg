package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/tasks"
)

// 本文件装配 W10 的两组保留清理循环（N4 兑换码回收 + N9 任务保留），以及
// W10-N10/N11 的日志分区删除与审计保留（R-W10-01/03/04/09）。
//
// 为什么放在 worker 角色：两者都是后台维护，与请求生命周期无关；gateway
// 不持有清理所需的长期连接与租约。单副本约束与计费兜底/任务轮询一致
// （M3 R-04），跨副本由 Redis `SET NX PX` 租约收敛（见各包的实现）。
//
// 【生效时机】设置键在 worker 启动时解析一次快照：与 W9-B4 的任务轮询
// 上限同口径（平台装配期解析，改动随 worker 重建生效）。保留期以天/月计，
// 热更新带来的收益远小于「两个副本看到不同删除边界」的风险；需要变更
// 时滚动重启 worker 即可。
//
// 【两套执行身份】N4/N9 在主库、用 app 身份（条件 UPDATE/DELETE，权限在
// 主库 DML 内）；N10/N11 在日志库、必须用独立的 DB_RETENTION_DSN
// （newtg_retention）。**绝不复用 app DSN**：应用身份没有分区所有权、也没有
// audit_logs 的 DELETE/列级 UPDATE。DSN 缺失时 fail-closed（跳过，不误删）。

const (
	// W10-N4（R-W10-05）：兑换码回收保留期。默认 90 天与设计一致，
	// 0 = 关闭（不物化 expired_at、不改 status）。
	settingRedemptionRetentionDays = "redemption.retention_days"
	// W10-N9（R-W10-10）：插件族终态任务保留期，默认 30 天，0 = 关闭。
	settingTaskRetentionTerminalDays = "task.retention_terminal_days"
	// W10-N9（R-W10-10）：MJ 族终态任务保留期，默认 30 天，0 = 关闭。
	// 与插件族**分开**，两族互不影响。
	settingMJRetentionTerminalDays = "mj.retention_terminal_days"

	// W10-N10（R-W10-03/04）：日志分区预建 lookahead（默认 3，0 = 只保证当月）
	// 与过期分区删除开关（默认 **false**，上线闸门见注册表注释）。
	settingLogPartitionLookaheadMonths = "log.partition_lookahead_months"
	settingLogPartitionDropEnabled     = "log.partition_drop_enabled"
	// W10-N11（R-W10-09 A）：审计行保留月数（默认 12，0 = 不删）与 IP 擦除
	// 天数（默认 90，0 = 不擦除）。
	settingAuditRetentionMonths = "audit.retention_months"
	settingAuditIPEraseDays     = "audit.ip_erase_days"
)

// logRetentionSettings 是 N10/N11 的已解析设置快照。
//
// 分区预建 lookahead 也在这里解析，因为同一份快照同时供
// billing.RunWorkerLoop（预建，app 身份）与分区删除（retention 身份）使用：
// 两者对「目标窗口」必须看到同一个值。
type logRetentionSettings struct {
	PartitionLookaheadMonths int
	PartitionDropEnabled     bool
	AuditRetentionMonths     int
	AuditIPEraseDays         int
}

// resolveLogRetentionSettings 把注册表默认值与配置中心三态合并成生效值。
//
// 与 N4/N9 同口径：未配置/非法/负值 → 设计默认；显式 0 → 0（关闭/不擦除）；
// 正值透传。布尔键缺省 → false（尤其是 partition_drop_enabled 的 fail-safe
// 方向：任何解析问题都不能意外打开破坏性删除）。
func resolveLogRetentionSettings(reader settingReader, log *slog.Logger) logRetentionSettings {
	if log == nil {
		log = slog.Default()
	}
	return logRetentionSettings{
		PartitionLookaheadMonths: clampSetting(resolveRetentionDays(reader,
			settingLogPartitionLookaheadMonths, billing.DefaultPartitionLookaheadMonths, log),
			maxLogPartitionLookaheadMonths, settingLogPartitionLookaheadMonths, log),
		PartitionDropEnabled: readSettingBool(reader, settingLogPartitionDropEnabled, false, log),
		AuditRetentionMonths: clampSetting(resolveRetentionDays(reader,
			settingAuditRetentionMonths, audit.DefaultRetentionMonths, log),
			maxAuditRetentionMonths, settingAuditRetentionMonths, log),
		AuditIPEraseDays: clampSetting(resolveRetentionDays(reader,
			settingAuditIPEraseDays, audit.DefaultIPEraseDays, log),
			maxAuditIPEraseDays, settingAuditIPEraseDays, log),
	}
}

// 注册表上界在读取侧再钳制一次（M2）。
//
// configcenter 的历史行/手工写入不经过注册表校验：一个手滑的
// `log.partition_lookahead_months=100000` 会让预建循环生成海量分区 DDL，
// `audit.retention_months` 的极端值虽不会溢出（有 Duration 护栏），但仍会
// 让「保留期」语义失控。读取侧钳到注册表上界并 warn，方向始终是「更保守」。
const (
	maxLogPartitionLookaheadMonths = 36
	maxAuditRetentionMonths        = 120
	maxAuditIPEraseDays            = 3650
)

func clampSetting(value, max int, key string, log *slog.Logger) int {
	if value <= max {
		return value
	}
	log.Warn("worker: retention setting exceeds the registry maximum; clamping",
		"key", key, "value", value, "max", max)
	return max
}

// readSettingBool 读取布尔设置；缺省/非布尔/JSON null 一律回落 fallback。
//
// 与 readSettingInt 的指针对齐：null 解到 bool 会得到 false 且无错误，若不
// 显式区分，脏行会把「默认关」的键读成关闭、也可能把默认开的键误关；本键
// fallback 恒为 false，所以这里仍显式 warn，便于发现脏数据。
func readSettingBool(reader settingReader, key string, fallback bool, log *slog.Logger) bool {
	if reader == nil {
		return fallback
	}
	raw, ok := reader.Get(key)
	if !ok {
		return fallback
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		log.Warn("worker: setting is not a valid boolean; falling back to the fail-safe default",
			"key", key, "raw", string(raw), "fallback", fallback)
		return fallback
	}
	return *value
}

// startRetentionWorkers 解析保留设置并启动后台循环，返回停止函数。
//
// 循环分两组：
//   - N4/N9：主库 + app 身份（兑换码回收、任务保留）；
//   - N10/N11：日志库 + **retention 身份**（过期分区删除、审计保留/IP 擦除）。
//
// 停止函数取消上下文并等待全部循环退出；调用方必须在关闭 Redis/DB 之前
// 调用，否则进行中的 DELETE/DROP 会拿到「连接已关闭」错误，把正常停机噪声
// 打进告警。各循环独立：任一失败只记录，不影响其它循环。
//
// 【retention DSN fail-closed】cfg.DBRetentionDSN 为空或连接失败时，N10/N11
// **都不启动**，只记 warn/error；绝不回退到 app 日志库 DSN（那会用应用身份
// 执行 DROP/DELETE，既越权又可能误删）。N4/N9 不受影响。
func startRetentionWorkers(parent context.Context, cfg *config.Config, db *sql.DB,
	logsDB *sql.DB, rdb *redis.Client, center *configcenter.Center,
	logRetention logRetentionSettings, node, instanceID string, log *slog.Logger) func() {

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
		"mj_task_retention_days", mjDays,
		"log_partition_drop_enabled", logRetention.PartitionDropEnabled,
		"log_partition_lookahead_months", logRetention.PartitionLookaheadMonths,
		"audit_retention_months", logRetention.AuditRetentionMonths,
		"audit_ip_erase_days", logRetention.AuditIPEraseDays)

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

	// ---- W10-N10/N11：retention 身份（独立 DSN，缺省 fail-closed） ----
	plan := planLogRetention(cfg, logRetention)
	var retentionDB *sql.DB
	if !plan.OpenRetention {
		// N10/N11 的删除面只能由维护角色执行；缺 DSN 时保持「什么都不删」，
		// 而不是借用 app DSN 悄悄降级。这是部署配置问题，需要运维补齐。
		log.Warn("worker: DB_RETENTION_DSN is not set; log partition drop and audit retention are disabled (fail-closed)",
			"partition_drop_enabled", logRetention.PartitionDropEnabled,
			"audit_retention_months", logRetention.AuditRetentionMonths,
			"audit_ip_erase_days", logRetention.AuditIPEraseDays)
	} else {
		var err error
		retentionDB, err = openRetentionDB(parent, cfg)
		if err != nil {
			// 连接失败同样不降级：宁可本轮不清理，也不误用应用身份。
			log.Error("worker: open retention database failed; log partition drop and audit retention are disabled (fail-closed)",
				"error", err)
			retentionDB = nil
		}
	}

	if retentionDB != nil {
		if plan.PartitionDrop {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := billing.RunPartitionDropLoop(ctx, billing.PartitionDropDeps{
					DB:            retentionDB,
					Redis:         rdb,
					Logger:        log,
					RetentionDays: billing.DefaultPartitionRetentionDays,
					BatchSize:     billing.PartitionDropBatchSize,
				}); err != nil {
					log.Error("log partition drop worker exited with error", "error", err)
				}
			}()
		} else {
			// R-W10-03 A 的显式偏离：默认关。year 排行榜仍直接聚合 12 个月
			// usage_logs；在 usage_dashboards 落地或 year 榜改源之前，开启
			// 会让 year 榜静默退化。
			log.Info("worker: log partition drop is disabled (log.partition_drop_enabled=false); " +
				"enable only after usage_dashboards or rankings_timeseries year reads land")
		}

		if plan.AuditRetention {
			// 自审计走 app 的 append-only 写入口（audit_logs INSERT 属 app），
			// retention 角色只需要 SELECT/DELETE/UPDATE(ip)；每轮最多一条。
			auditLogger := audit.New(logsDB)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := audit.RunRetentionLoop(ctx, audit.RetentionDeps{
					DB:              retentionDB,
					Redis:           rdb,
					Logger:          log,
					RetentionMonths: logRetention.AuditRetentionMonths,
					IPEraseDays:     logRetention.AuditIPEraseDays,
					Audit: func(ctx context.Context, detail json.RawMessage) error {
						return auditLogger.Log(ctx, audit.ActionAuditRetention, 0, detail)
					},
				}); err != nil {
					log.Error("audit retention worker exited with error", "error", err)
				}
			}()
		} else {
			log.Info("worker: audit retention is disabled (audit.retention_months=0 and audit.ip_erase_days=0)")
		}
	}

	return func() {
		cancel()
		wg.Wait()
		// 停机序：先等循环退出，再关 retention 连接池；最后才轮到主库/日志库。
		if retentionDB != nil {
			if err := retentionDB.Close(); err != nil {
				log.Warn("worker: close retention database failed", "error", err)
			}
		}
	}
}

// logRetentionPlan 是「哪些保留循环应该启动」的纯决策结果（M5 wiring）。
//
// 抽成纯函数让「缺 retention DSN 时 N10/N11 一个都不启动」可以被单测锁定：
// 即使 operator 把 log.partition_drop_enabled 设为 true、审计保留期非 0，
// 只要 DSN 缺失，三个布尔值都必须是 false（fail-closed），绝不回退 app DSN。
type logRetentionPlan struct {
	OpenRetention  bool // 是否打开独立 retention 连接池
	PartitionDrop  bool // 是否启动 N10 分区删除
	AuditRetention bool // 是否启动 N11 审计保留/IP 擦除
}

func planLogRetention(cfg *config.Config, settings logRetentionSettings) logRetentionPlan {
	if cfg == nil || cfg.DBRetentionDSN == "" {
		return logRetentionPlan{}
	}
	return logRetentionPlan{
		OpenRetention:  true,
		PartitionDrop:  settings.PartitionDropEnabled,
		AuditRetention: settings.AuditRetentionMonths > 0 || settings.AuditIPEraseDays > 0,
	}
}

// openRetentionDB 只按 DB_RETENTION_DSN 打开维护连接池。
//
// 为空返回 (nil, nil)，调用方据此完全跳过 N10/N11——这是 fail-closed 的
// 唯一实现点：**绝不**在缺 DSN 时回退到 DB_LOGS_DSN / DATABASE_URL。
// 抽成函数是为了让「不回退」可以被单元测试直接锁定，而不是只能靠评审。
func openRetentionDB(ctx context.Context, cfg *config.Config) (*sql.DB, error) {
	if cfg == nil || cfg.DBRetentionDSN == "" {
		return nil, nil
	}
	return openDBPool(ctx, cfg.DBRetentionDSN, cfg, "retention db")
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
