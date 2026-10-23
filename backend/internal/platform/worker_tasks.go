package platform

// W17-2 worker 任务框架的进程装配（billing-06 §2 / §13.5）。
//
// 【接入方式】既有后台循环**不重写**：它们的 ticker、Redis 锁与失败语义
// 保持原样，注册表只登记元数据（Scheduled=false, Run=nil）。调度器只执行
// 两类执行体：
//   1. Scheduled=true 的任务（当前只有 worker_history_cleanup）；
//   2. 白名单维护任务的手动触发（复用既有 run-once 函数，不复制业务逻辑）。
//
// 【为什么手动触发要绑定 Run】§13.5 要求「写 pending 行 + 唤醒，由调度 pass
// 按正常租约认领」。如果白名单任务没有可执行的 Run，触发只会留下一条永远
// pending 的行——这比 404 更具误导性。因此 5 个白名单任务都绑定一个既有的
// 一轮执行函数；它们自身的周期执行仍归既有循环。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/billing"
	"newtg/gateway/internal/config"
	"newtg/gateway/internal/configcenter"
	"newtg/gateway/internal/holiday"
	"newtg/gateway/internal/httpapi"
	"newtg/gateway/internal/pricingstrategy"
	"newtg/gateway/internal/rankings"
	"newtg/gateway/internal/worker"
)

// DefaultRunRetentionDays 是 worker_task_runs 的保留期（billing-06 §9.9
// 与运行历史默认 30 天同口径）。
const DefaultRunRetentionDays = 30

// workerTaskDeps 是装配输入。
type workerTaskDeps struct {
	Cfg      *config.Config
	DB       *sql.DB
	LogsDB   *sql.DB
	Redis    *redis.Client
	Center   *configcenter.Center
	Settings logRetentionSettings
	Logger   *slog.Logger
}

// startWorkerTaskFramework 装配并启动调度器，返回停止函数。
//
// 失败语义：注册表/仓储构造失败是编程错误，返回 error 由调用方记录；
// EnsureTaskRows 失败（迁移缺失）同样不阻止既有循环——框架只是可观测面，
// 不能因为管理面没准备好就让计费/任务轮询停摆。
func startWorkerTaskFramework(parent context.Context, deps workerTaskDeps) (func(), error) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	registry, err := worker.DefaultRegistry()
	if err != nil {
		return nil, fmt.Errorf("worker task registry: %w", err)
	}
	store, err := worker.NewSQLStore(deps.DB)
	if err != nil {
		return nil, fmt.Errorf("worker task store: %w", err)
	}
	if err := store.EnsureTaskRows(parent, registry.Tasks()); err != nil {
		log.Error("worker: ensure worker task rows failed; task framework disabled", "error", err)
		return nil, nil
	}
	if err := bindWorkerTaskRuns(registry, deps); err != nil {
		return nil, err
	}

	nodeID := httpapi.ResolveProcessInstanceID()
	scheduler, err := worker.NewScheduler(worker.SchedulerConfig{
		Registry: registry,
		Store:    store,
		Lease:    worker.NewRedisLease(deps.Redis),
		NodeID:   nodeID,
		Logger:   log,
		// 设计数值（billing-06 §2.2/§2.3.1）：15s pass / 60s TTL / 20s 心跳 /
		// 30s 过期清理。显式写出以便后续调参不依赖包内默认值。
		PassInterval:      worker.DefaultPassInterval,
		LeaseTTL:          worker.DefaultLeaseTTL,
		HeartbeatInterval: worker.DefaultHeartbeatInterval,
		CleanupInterval:   worker.DefaultCleanupInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("worker task scheduler: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	// 唤醒订阅：网关进程触发后 publish，worker 立即 pass；消息丢失由 15s
	// 周期兜底。订阅失败只记录，不影响调度。
	go func() {
		sub := deps.Redis.Subscribe(ctx, worker.WakeupChannel)
		defer func() { _ = sub.Close() }()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				scheduler.Wake()
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if runErr := scheduler.Run(ctx); runErr != nil && ctx.Err() == nil {
			log.Error("worker task scheduler exited with error", "error", runErr)
		}
	}()
	log.Info("worker: task framework started", "node_id", nodeID)
	return func() {
		cancel()
		<-done
	}, nil
}

// bindWorkerTaskRuns 给白名单任务绑定「一轮执行」函数。
func bindWorkerTaskRuns(registry *worker.Registry, deps workerTaskDeps) error {
	bindings := map[string]func(ctx context.Context) (worker.Result, error){
		"worker_history_cleanup": func(ctx context.Context) (worker.Result, error) {
			store, err := worker.NewSQLStore(deps.DB)
			if err != nil {
				return nil, err
			}
			cutoff := time.Now().UTC().Add(-DefaultRunRetentionDays * 24 * time.Hour)
			deleted, err := store.DeleteRunsBefore(ctx, cutoff)
			if err != nil {
				return nil, err
			}
			return worker.Result{"deleted_runs": deleted}, nil
		},
		"redemption_expiry": func(ctx context.Context) (worker.Result, error) {
			days := resolveRetentionDays(deps.Center, settingRedemptionRetentionDays,
				billing.DefaultRedemptionRetentionDays, deps.Logger)
			if days <= 0 {
				return worker.Result{"skipped": "redemption retention is disabled"}, nil
			}
			cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
			var total int64
			for i := 0; i < billing.RedemptionRetentionMaxBatchesPerPass; i++ {
				if ctx.Err() != nil {
					return worker.Result{"deleted": total}, ctx.Err()
				}
				deleted, err := billing.DeleteExpiredRedemptionCodes(ctx, deps.DB, cutoff,
					billing.RedemptionRetentionBatchSize)
				if err != nil {
					return worker.Result{"deleted": total}, err
				}
				total += deleted
				if deleted < int64(billing.RedemptionRetentionBatchSize) {
					break
				}
			}
			return worker.Result{"deleted": total}, nil
		},
		"log_partition_precreate": func(ctx context.Context) (worker.Result, error) {
			if err := billing.EnsureUsagePartitionsWithLookahead(ctx, deps.LogsDB, time.Now().UTC(),
				deps.Settings.PartitionLookaheadMonths); err != nil {
				return nil, err
			}
			return worker.Result{"lookahead_months": deps.Settings.PartitionLookaheadMonths}, nil
		},
		"log_cleanup": func(ctx context.Context) (worker.Result, error) {
			// 冻结裁决：手动清理必须尊重 log.partition_drop_enabled。开关关闭
			// 时零 DROP（触发层已返回 409；这里是「触发后开关被关」的竞态兜底）。
			if !deps.Settings.PartitionDropEnabled {
				return nil, &worker.TaskBlockedError{Code: "log_partition_drop_disabled"}
			}
			// 维护角色连接池按需打开：只有管理员手动触发时才需要 DROP 权限。
			// 缺 DB_RETENTION_DSN 时 fail-closed（不借用应用身份去删分区）。
			retentionDB, err := openRetentionDB(ctx, deps.Cfg)
			if err != nil || retentionDB == nil {
				if err != nil {
					deps.Logger.Warn("worker: log cleanup skipped; retention database unavailable", "error", err)
				}
				return worker.Result{"skipped": "retention database is not configured"}, nil
			}
			defer func() {
				if closeErr := retentionDB.Close(); closeErr != nil {
					deps.Logger.Warn("worker: close retention database failed", "error", closeErr)
				}
			}()
			dropped, err := billing.DropExpiredUsagePartitions(ctx, retentionDB, time.Now().UTC(),
				billing.DefaultPartitionRetentionDays*24*time.Hour, billing.PartitionDropBatchSize)
			if err != nil {
				return nil, err
			}
			return worker.Result{"dropped_partitions": len(dropped)}, nil
		},
		"rankings_snapshot": func(ctx context.Context) (worker.Result, error) {
			if err := rankings.RefreshOnce(ctx, rankings.Deps{
				MainDB: deps.DB,
				LogsDB: deps.LogsDB,
				Redis:  deps.Redis,
				Config: deps.Center,
				Logger: deps.Logger,
			}); err != nil {
				return nil, err
			}
			return worker.Result{"periods": 4}, nil
		},
		"holiday_sync": func(ctx context.Context) (worker.Result, error) {
			return runHolidaySync(ctx, deps)
		},
	}
	for name, run := range bindings {
		if err := registry.BindRun(name, run); err != nil {
			return fmt.Errorf("worker task bind %q: %w", name, err)
		}
	}
	// 手动 log_cleanup 不能绕过 log.partition_drop_enabled（控制端裁决）：
	// 谓词为假时触发 409 log_partition_drop_disabled，且 Scheduler 的 pass
	// 也会跳过（不会留下 pending 行）。
	if err := registry.SetEnabled("log_cleanup",
		func() bool { return deps.Settings.PartitionDropEnabled },
		"log_partition_drop_disabled"); err != nil {
		return fmt.Errorf("worker task enable predicate: %w", err)
	}
	// holiday_sync 的出站开关（HOLIDAY_SYNC_ENABLED）：关闭时调度 pass 跳过、
	// 手动触发 409 holiday_sync_disabled。已同步的 holiday_calendar 行仍参与
	// 计价——关掉的是拉取，不是使用。deps.Cfg 为 nil 表示装配不完整，按关闭
	// 处理（与配置默认 true 相反，但装配缺配置时不该发请求）。
	if err := registry.SetEnabled("holiday_sync",
		func() bool { return deps.Cfg != nil && deps.Cfg.HolidaySyncEnabled },
		"holiday_sync_disabled"); err != nil {
		return fmt.Errorf("worker task enable predicate: %w", err)
	}
	return nil
}

// ---- holiday_sync（spec §5.2.1） ------------------------------------------

const (
	// holidaySyncClientTimeout 是单次拉取的 HTTP 超时（spec §5.2.1「HTTP 超时 10s」）。
	holidaySyncClientTimeout = 10 * time.Second
)

// holidaySyncLocation 是中国日期的「今天/今年」口径。中国不实行夏令时，
// 固定 +08:00 与 Asia/Shanghai 在当前与历史年份都等价，且不会因容器缺
// tzdata 而 init 失败。
var holidaySyncLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// runHolidaySync 拉取并替换当前年度（12 月追加下一年）的中国节假日日历。
//
// 每年独立「拉取 → 校验 → 整年替换」：某一年失败只保留该年的旧行，不影响
// 另一次写入（spec §5.2.1「失败保留上一次的好数据」）。运行结果只包含年、
// 假日数与调休数（运行历史保留 30 天，不得写入上游响应体）。
func runHolidaySync(ctx context.Context, deps workerTaskDeps) (worker.Result, error) {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	if deps.Cfg == nil {
		return nil, errors.New("worker: holiday sync requires config")
	}
	if deps.DB == nil {
		return nil, errors.New("worker: holiday sync requires the main database")
	}

	store := holiday.NewStore(deps.DB)
	client := &http.Client{Timeout: holidaySyncClientTimeout}
	template := deps.Cfg.HolidaySyncURLTemplate
	years := holidaySyncYears(time.Now())

	var (
		failures []error
		offDays  int
		makeups  int
		perYear  []map[string]any
	)
	for _, year := range years {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		days, err := holiday.FetchYear(ctx, client, template, year)
		if err != nil {
			log.Warn("worker: holiday sync fetch failed; keeping existing rows",
				"year", year, "source", holiday.SourceName, "error", err)
			failures = append(failures, fmt.Errorf("year %d: %w", year, err))
			continue
		}
		off, makeup := countHolidayDays(days)
		if err := store.ReplaceYear(ctx, pricingstrategy.CountryCN, year, days, holiday.SourceName); err != nil {
			log.Warn("worker: holiday sync store failed; existing rows kept",
				"year", year, "source", holiday.SourceName, "error", err)
			failures = append(failures, fmt.Errorf("year %d: %w", year, err))
			continue
		}
		log.Info("worker: holiday sync stored year",
			"year", year, "off_days", off, "makeup_days", makeup, "source", holiday.SourceName)
		offDays += off
		makeups += makeup
		perYear = append(perYear, map[string]any{
			"year": year, "off_days": off, "makeup_days": makeup,
		})
	}

	result := worker.Result{
		"years":       perYear,
		"off_days":    offDays,
		"makeup_days": makeups,
	}
	if len(failures) > 0 {
		// 部分年份失败仍返回已成功的计数：运行历史里能看到「成功的年份
		// 已写库、失败的年份保留旧数据」。errors.Join 保留 context 取消
		// 的 errors.Is 语义。
		return result, errors.Join(failures...)
	}
	return result, nil
}

// holidaySyncYears 返回本次要同步的年份：始终含当年；12 月追加下一年，
// 让元旦假期在数据源发布后尽快进入日历（spec §5.2.1）。
func holidaySyncYears(now time.Time) []int {
	local := now.In(holidaySyncLocation)
	years := []int{local.Year()}
	if local.Month() == time.December {
		years = append(years, local.Year()+1)
	}
	return years
}

// countHolidayDays 统计法定假日与调休上班日，用于运行结果与日志。
func countHolidayDays(days []holiday.Day) (off, makeup int) {
	for _, day := range days {
		if day.OffDay {
			off++
		} else {
			makeup++
		}
	}
	return off, makeup
}
