package rankings

// 历史时间桶的保留清理（B5a）。
//
// 【保留期（冻结值，2026-10-20）】hour 30 天、day 730 天、week 1825 天。
// 取值理由：hour 桶只服务 today 视图（今天最远 24 小时），30 天足够覆盖
// 跨天排障与「今天 vs 上周同日」的对账；day 桶覆盖两年（week/month/year
// 视图的历史下钻），week 桶覆盖五年（year 视图的长期趋势）。超过保留期的
// 桶不会再被任何窗口选到（窗口最长一年），继续留存只增存储。
//
// 【分批删除】每批最多 1000 行，循环到不足一批：单个 DELETE 不长期持有
// 行锁/不制造长事务，避免与公开读的桶扫描互相阻塞。删除按 bucket_start
// 过滤，走 rankings_*_bucket_idx；重复执行（多副本、重启）是幂等的。
//
// 【为什么模型桶和作者桶都清】两表都是跨 run 累积的历史序列，作者桶是
// 模型桶的派生聚合；只清一张会让另一张无限增长。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	// retentionHour / retentionDay / retentionWeek 是冻结的桶保留期。
	retentionHour = 30 * 24 * time.Hour
	retentionDay  = 730 * 24 * time.Hour
	retentionWeek = 1825 * 24 * time.Hour

	// cleanupBatchSize 是单条 DELETE 的上限（避免长事务）。
	cleanupBatchSize = 1000

	// cleanupInterval 是清理节奏。每轮快照都删会让 4 个 period 重复清理，
	// 每小时一次足以保证有界增长（hour 桶每天只新增 24 行/实体）。
	cleanupInterval = time.Hour
)

// timeseriesCleanup 描述一张历史序列表与它的保留粒度。
type timeseriesCleanup struct {
	table       string
	granularity Granularity
	retention   time.Duration
}

// timeseriesCleanupTargets 是全部需要清理的表/粒度组合。
var timeseriesCleanupTargets = []timeseriesCleanup{
	{"rankings_timeseries", GranularityHour, retentionHour},
	{"rankings_timeseries", GranularityDay, retentionDay},
	{"rankings_timeseries", GranularityWeek, retentionWeek},
	{"rankings_author_timeseries", GranularityHour, retentionHour},
	{"rankings_author_timeseries", GranularityDay, retentionDay},
	{"rankings_author_timeseries", GranularityWeek, retentionWeek},
}

// CleanupExpiredTimeseries 删除超过保留期的历史桶，返回删除总行数。
//
// 每次删除最多 cleanupBatchSize 行；同一步骤内循环直到没有可删除行。
// 上下文取消时停止并返回已删除计数（不把取消记为错误，调用方正在停机）。
func CleanupExpiredTimeseries(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("rankings: cleanup timeseries: db is nil")
	}
	var total int64
	for _, target := range timeseriesCleanupTargets {
		cutoff := now.Add(-target.retention)
		for {
			if err := ctx.Err(); err != nil {
				return total, nil
			}
			n, err := deleteExpiredBatch(ctx, db, target.table, string(target.granularity), cutoff)
			if err != nil {
				return total, err
			}
			total += n
			if n < cleanupBatchSize {
				break
			}
		}
	}
	return total, nil
}

// deleteExpiredBatch 删除一批过期桶。
//
// 用 ctid 子查询而不是直接 `DELETE ... WHERE` + LIMIT（PG 的 DELETE 不支持
// LIMIT），一次只锁 1000 行。表名来自包内常量而非外部输入，拼接安全。
func deleteExpiredBatch(ctx context.Context, db *sql.DB, table, granularity string, cutoff time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM %s
		WHERE ctid IN (
		    SELECT ctid FROM %s
		    WHERE granularity = $1
		      AND bucket_start < $2
		    LIMIT %d
		)`, table, table, cleanupBatchSize), granularity, cutoff)
	if err != nil {
		return 0, fmt.Errorf("rankings: cleanup %s/%s: %w", table, granularity, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rankings: cleanup %s/%s affected: %w", table, granularity, err)
	}
	return n, nil
}

// runCleanupLoop 每小时清理一次历史桶，直到 ctx 结束。
func runCleanupLoop(ctx context.Context, deps Deps) {
	// 启动即清一次：重启后无需等满一个间隔。
	cleanupOnce(ctx, deps)
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupOnce(ctx, deps)
		}
	}
}

func cleanupOnce(ctx context.Context, deps Deps) {
	if ctx.Err() != nil {
		return
	}
	deleted, err := CleanupExpiredTimeseries(ctx, deps.MainDB, deps.Now())
	if err != nil {
		if ctx.Err() == nil {
			deps.Logger.Warn("rankings: 历史桶清理失败", "error", err)
		}
		return
	}
	if deleted > 0 {
		deps.Logger.Info("rankings: 已清理过期历史桶", "deleted", deleted)
	}
}
