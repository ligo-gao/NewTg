package tasks

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 W10-N9 的任务保留清理（R-W10-10；billing-06 §9、relay-05 §7.4）。
//
// 【只删终态】谓词固定 `state IN ('success','failure')`。未终结状态
// （not_start/submitted/queued/in_progress/unknown）可能仍被轮询器推进、
// 仍持有预扣，删掉它们会把资金线索从 worker 的事件流里移除——这是资损，
// 不是清理。`unknown` 也**不在**终态集合内：设计要求 UNKNOWN 原样返回并
// 继续探测，把它当终态会过早删除可能仍在上游运行的任务。
//
// 【族隔离 · R-W10-10】插件族与 MJ 族使用**两个独立保留键**：
//   - `task.retention_terminal_days`（插件族，默认 30）
//   - `mj.retention_terminal_days`（MJ 族，默认 30）
//
// 都支持 `0 = 关闭`。两族的上游生命周期与产物形态不同，共用一个键会让
// 运营无法单独延长 MJ 任务的保留期（或反过来被插件堆积拖住）；键的解析
// 与默认值合并在平台装配层完成，本包只接收已解析的天数。
//
// 【待补偿行永不删除】终态行仍可能带 `billing_pending` / `settlement_pending`
// / `refund_pending` 标记（T6b 的补偿扫描依据）。删除这类行等于永久丢弃
// 结算/退款线索，因此 SQL 显式排除它们；补偿完成后标记消失，下一次清理
// 自然会收回。该谓词与 `tasks_billing_pending_idx` 的索引谓词逐字一致。
//
// 【保留期基准】优先 `retention_deadline`（设计给每任务的绝对期限），否则
// `finish_time + N 天`（插件/MJ 的族保留期），再否则 `updated_at + N 天`
// （历史行可能没有 finish_time；宁晚勿早，绝不因缺列而永远留垃圾或提前
// 删除）。用 COALESCE 而不是 `OR` 是为了让「取最晚的有效基准」由 SQL 一次
// 求值，避免应用层再算一遍时间。
//
// 【分批 + 上限】每批 `LIMIT` 行、单轮每族最多 `MaxBatches` 批；积压由
// 下一轮继续（按 id 最旧优先）。大 DELETE 会长时间持锁并放大 WAL，分批
// 把回收对在线任务读写的影响限制在有界事务内。

const (
	// DefaultRetentionTerminalDays 是两族的设计默认保留期（billing-06 §9：
	// 终态任务保留 30 天）。注册表的两个默认值与此保持一致。
	DefaultRetentionTerminalDays = 30

	// RetentionInterval 是清理周期（billing-06 §9：每日一次）。冻结为常量：
	// 保留期以天计，检查更频繁不会改变删除边界。
	RetentionInterval = 24 * time.Hour

	// RetentionBatchSize 是单条 DELETE 的最大行数。
	RetentionBatchSize = 1000

	// RetentionMaxBatchesPerPass 是单轮**每族**的批次数上限（默认 1 万行/族）。
	RetentionMaxBatchesPerPass = 10

	// retentionLockKey 是跨副本租约键（插件与 MJ 共用一把：两者在同一
	// goroutine 内串行执行，分开加锁反而制造「一半族被另一副本处理」的
	// 交错，让每族日志难以对账）。
	retentionLockKey = "newtg:tasks-retention:lock:tick"
	// retentionLockTTL 只需覆盖一轮回收；固定 1 小时让崩溃后的接手延迟
	// 有界（日更任务若把 TTL 设成整个间隔，残留锁会卡住一整天）。
	retentionLockTTL = time.Hour
)

// RetentionDeps 是任务保留清理循环的依赖与可调参数。零值使用默认值：
//   - Now → time.Now；Logger → slog.Default()；
//   - Interval → RetentionInterval；BatchSize → RetentionBatchSize；
//   - MaxBatches → RetentionMaxBatchesPerPass；
//   - Delete → 绑定 DB 的生产删除函数（此时 DB 必填）。
//
// PluginRetentionDays / MJRetentionDays 是**已解析**的生效值（平台把设置
// 中心的键与设计默认合并成 int）：<=0 表示该族关闭。两族独立，互不影响。
type RetentionDeps struct {
	DB       *sql.DB
	Redis    *redis.Client
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration

	PluginRetentionDays int
	MJRetentionDays     int
	BatchSize           int
	MaxBatches          int

	// Delete 是批量删除接缝（测试注入 fake；nil 时用 DB 的生产 SQL）。
	Delete func(ctx context.Context, family string, retentionDays int, now time.Time, limit int) (int64, error)
}

type retentionWorker struct {
	redis               *redis.Client
	log                 *slog.Logger
	now                 func() time.Time
	interval            time.Duration
	pluginRetentionDays int
	mjRetentionDays     int
	batchSize           int
	maxBatches          int
	deleteFn            func(ctx context.Context, family string, retentionDays int, now time.Time, limit int) (int64, error)
}

// RunRetentionLoop 运行任务保留清理循环，直到 ctx 取消。
//
// 启动即清理一轮（重启后不必等一天），之后按 Interval 周期性运行。单轮
// 单族失败只记录并在下一轮重试；一族失败不阻断另一族（族隔离）。
func RunRetentionLoop(ctx context.Context, deps RetentionDeps) error {
	w, err := newRetentionWorker(deps)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	if w.pluginRetentionDays <= 0 && w.mjRetentionDays <= 0 {
		// 冻结语义：0 = 关闭。两族都关时不做任何 DB/Redis 访问。
		w.log.Info("task retention disabled for both families (retention_days <= 0)")
		return nil
	}

	w.log.Info("task retention worker started",
		"interval", w.interval,
		"plugin_retention_days", w.pluginRetentionDays,
		"mj_retention_days", w.mjRetentionDays,
		"batch_size", w.batchSize,
		"max_batches", w.maxBatches,
	)

	w.runOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func newRetentionWorker(deps RetentionDeps) (*retentionWorker, error) {
	if deps.Redis == nil {
		return nil, errors.New("tasks: retention requires Redis for the cross-replica lock")
	}
	if deps.Delete == nil && deps.DB == nil {
		return nil, errors.New("tasks: retention requires DB or a delete function")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	interval := deps.Interval
	if interval <= 0 {
		interval = RetentionInterval
	}
	batchSize := deps.BatchSize
	if batchSize <= 0 {
		batchSize = RetentionBatchSize
	}
	maxBatches := deps.MaxBatches
	if maxBatches <= 0 {
		maxBatches = RetentionMaxBatchesPerPass
	}
	deleteFn := deps.Delete
	if deleteFn == nil {
		deleteFn = func(ctx context.Context, family string, retentionDays int, now time.Time, limit int) (int64, error) {
			return DeleteTerminalTasksBefore(ctx, deps.DB, family, retentionDays, now, limit)
		}
	}
	return &retentionWorker{
		redis:               deps.Redis,
		log:                 log,
		now:                 now,
		interval:            interval,
		pluginRetentionDays: deps.PluginRetentionDays,
		mjRetentionDays:     deps.MJRetentionDays,
		batchSize:           batchSize,
		maxBatches:          maxBatches,
		deleteFn:            deleteFn,
	}, nil
}

// runOnce 抢租约后按族清理；抢不到说明另一副本正在执行，跳过不是错误。
func (w *retentionWorker) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	token, acquired, err := acquireRetentionLock(ctx, w.redis, retentionLockKey, retentionLockTTL)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.log.Error("task retention skipped: acquire lock failed", "error", err)
		return
	}
	if !acquired {
		w.log.Debug("task retention skipped: lock held by another replica")
		return
	}
	defer releaseRetentionLock(ctx, w.redis, retentionLockKey, token, w.log)

	w.runFamilies(ctx)
}

// runFamilies 按族执行清理（不含锁），返回每族删除数之和。
//
// 与抢锁分离是为了让单测能在不依赖 Redis 的情况下验证「族隔离 / 0=关闭 /
// 批量上限」；生产路径永远先经过 runOnce 的租约。
func (w *retentionWorker) runFamilies(ctx context.Context) int64 {
	now := w.now().UTC()
	var total int64
	// 族隔离：每族独立判定、独立累计；一族失败不影响另一族。
	for _, family := range []struct {
		name string
		days int
	}{
		{FamilyPlugin, w.pluginRetentionDays},
		{FamilyMJ, w.mjRetentionDays},
	} {
		if family.days <= 0 {
			continue
		}
		deleted := w.reclaimFamily(ctx, family.name, family.days, now)
		total += deleted
		if deleted > 0 {
			w.log.Info("terminal tasks reclaimed",
				"family", family.name, "count", deleted, "retention_days", family.days)
		}
	}
	return total
}

// reclaimFamily 按批删除某族的终态行，返回该族本轮删除总数。
func (w *retentionWorker) reclaimFamily(ctx context.Context, family string, retentionDays int, now time.Time) int64 {
	if retentionDays <= 0 {
		// 0 = 关闭（冻结语义）。调用方已经跳过，这里再兜一层，保证任何
		// 直接调用都不会把「关闭」误执行成「按默认值删除」。
		return 0
	}
	var total int64
	for batch := 0; batch < w.maxBatches; batch++ {
		if ctx.Err() != nil {
			break
		}
		deleted, err := w.deleteFn(ctx, family, retentionDays, now, w.batchSize)
		if err != nil {
			w.log.Error("task retention delete failed; retrying next round",
				"family", family, "error", err, "deleted", total)
			return total
		}
		total += deleted
		// 不足一批说明该族保留期外的终态行已清空。
		if deleted < int64(w.batchSize) {
			break
		}
	}
	if total >= int64(w.batchSize*w.maxBatches) {
		w.log.Warn("task retention hit the per-pass batch budget; backlog continues next pass",
			"family", family, "deleted", total,
			"batch_size", w.batchSize, "max_batches", w.maxBatches)
	}
	return total
}

// DeleteTerminalTasksBefore 删除某族中「终态且超过保留期」的前 limit 行。
//
// `now` 是调用方注入的时刻（测试用固定时间）；保留期天数在 SQL 内用
// `make_interval(days => $2::int)` 换算，避免把「天」在不同时区口径下
// 提前折算成秒。ORDER BY id 让批次顺序确定，不会因同一时间戳反复空转。
//
// 【COALESCE 优先级】retention_deadline → finish_time + 保留期 →
// updated_at + 保留期。三者取第一个非空值：自定义期限优先于族默认；
// 缺 finish_time 的历史行用 updated_at 兜底（宁晚勿早）。
//
// 【NOT pending 谓词】与 `tasks_billing_pending_idx` 的部分索引谓词一致，
// 既保证待补偿行不被删，也让本查询能命中该索引之外的终态保留索引。
func DeleteTerminalTasksBefore(ctx context.Context, db *sql.DB, family string, retentionDays int, now time.Time, limit int) (int64, error) {
	if !ValidFamily(family) {
		// 拼错的族名必须报错而不是静默返回 0：那会把「配置写错」伪装成
		// 「今天没有可清理任务」，让保留期失效而不被察觉。
		return 0, fmt.Errorf("tasks: retention: invalid family %q", family)
	}
	if db == nil {
		return 0, errors.New("tasks: retention database is not configured")
	}
	if retentionDays <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = 1
	}
	res, err := db.ExecContext(ctx, `
		DELETE FROM tasks
		WHERE id IN (
			SELECT id FROM tasks
			WHERE family = $1
			  AND state IN ('success', 'failure')
			  AND COALESCE(
			        retention_deadline,
			        finish_time + make_interval(days => $2::int),
			        updated_at  + make_interval(days => $2::int)
			      ) < $3
			  AND NOT (private_data @> '{"billing_pending": true}'::jsonb
			        OR private_data @> '{"settlement_pending": true}'::jsonb
			        OR private_data @> '{"refund_pending": true}'::jsonb)
			ORDER BY id ASC
			LIMIT $4
		)`, family, retentionDays, now.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("tasks: delete terminal tasks (family=%s): %w", family, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("tasks: terminal tasks rows affected: %w", err)
	}
	return affected, nil
}

// acquireRetentionLock 用 SET NX PX 抢跨副本租约，返回持有者令牌。
// 令牌用于 compare-and-delete，避免 TTL 过期后误删另一副本的锁。
func acquireRetentionLock(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration) (string, bool, error) {
	token, err := newRetentionLockToken()
	if err != nil {
		return "", false, fmt.Errorf("tasks: generate lock token: %w", err)
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	acquired, err := rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("tasks: acquire lock %s: %w", key, err)
	}
	return token, acquired, nil
}

// releaseRetentionLock 用 Lua 做 compare-and-delete；失败只告警，TTL 回收。
// 停机时 ctx 可能已取消，因此使用 Context.WithoutCancel + 独立短超时。
func releaseRetentionLock(ctx context.Context, rdb *redis.Client, key, token string, log *slog.Logger) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := releaseRetentionLockScript.Run(releaseCtx, rdb, []string{key}, token).Err(); err != nil {
		log.Warn("tasks: release retention lock failed (TTL will reclaim it)", "key", key, "error", err)
	}
}

var releaseRetentionLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

func newRetentionLockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
