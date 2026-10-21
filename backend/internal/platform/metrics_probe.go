package platform

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/metrics"
)

// metricsProbeInterval 是依赖健康探针周期。
//
// 15s 与 Prometheus 默认 30s 抓取对齐：比抓取更密即可保证每次 scrape 都有
// 新鲜值，又不至于把 PG/Redis 当成压测目标。A18 的判定窗口是 2m，允许偶发
// 单次探测失败被下一次覆盖。
const metricsProbeInterval = 15 * time.Second

// migrationProbeInterval 是迁移版本刷新周期。
//
// 迁移只在启动/运维窗口变化，60s 足够让 dirty 状态在 A19 的判定窗口内可见。
const migrationProbeInterval = 60 * time.Second

// startMetricsProbe 启动依赖/迁移/连接池指标的后台探针，阻塞至 ctx done。
//
// 它不改变任何业务状态：只读 Ping、SELECT version 与 sql.DB.Stats。探测失败
// 只把 gauge 置 0（依赖不可用），不 panic、不重试风暴；连接池/DB 的真实
// 健康仍以 /readyz 为准，指标只是同一事实的可抓取视图。
func startMetricsProbe(ctx context.Context, mainDB, logsDB *sql.DB, rdb *redis.Client,
	log *slog.Logger) {
	metrics.AttachDBPools(mainDB, logsDB)

	probeDeps := func() {
		if mainDB != nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := mainDB.PingContext(pingCtx)
			cancel()
			metrics.SetDependencyUp("postgres", err == nil)
			if err != nil && log != nil {
				log.Debug("metrics: postgres probe failed", "error", err)
			}
		}
		if rdb != nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := rdb.Ping(pingCtx).Err()
			cancel()
			metrics.SetRedisUp(err == nil)
			metrics.SetDependencyUp("redis", err == nil)
			if err != nil && log != nil {
				log.Debug("metrics: redis probe failed", "error", err)
			}
		}
	}

	probeMigration := func() {
		if mainDB == nil {
			return
		}
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var version int64
		var dirty bool
		err := mainDB.QueryRowContext(queryCtx,
			"SELECT version, dirty FROM schema_migrations").Scan(&version, &dirty)
		if err != nil {
			// 表不存在/迁移尚未执行：不覆盖旧值，也不伪造 0 版本。
			if log != nil {
				log.Debug("metrics: migration probe failed", "error", err)
			}
			return
		}
		metrics.SetMigration(version, dirty)
	}

	// 启动即探测：避免 scrape 在首个 tick 前看到「依赖全 0」的假故障窗口。
	probeDeps()
	probeMigration()

	depsTicker := time.NewTicker(metricsProbeInterval)
	defer depsTicker.Stop()
	migrationTicker := time.NewTicker(migrationProbeInterval)
	defer migrationTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-depsTicker.C:
			probeDeps()
		case <-migrationTicker.C:
			probeMigration()
		}
	}
}

// redisMetricsHook 记录 Redis 命令耗时（go-redis v9 Hook 接口）。
//
// 只观察 Process/Cmd 两族命令；Dial 与 Pipeline 的展开不单独计时，避免
// 同一命令被重复计入。命令名经 ObserveRedisCommand 的白名单收敛。
type redisMetricsHook struct{}

func (redisMetricsHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (redisMetricsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		metrics.ObserveRedisCommand(cmd.Name(), time.Since(start))
		return err
	}
}

func (redisMetricsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		// 管道内各命令无法单独计时；记一条批次总耗时（cmd=pipeline），
		// 避免把同一段时长重复累加到每条命令上。
		metrics.ObserveRedisCommand("pipeline", time.Since(start))
		return err
	}
}
