package billing

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

// 本文件是 W10-N4 的兑换码回收循环（R-W10-05；billing-06 §7）。
//
// 【冻结口径 · R-W10-05】只删除「超过保留期且 `unused + expired`」的行：
//   - `expired` 仍是**虚拟态**（status='unused' AND expires_at <= now()），
//     本循环不物化 `expired_at`、不改 `status`、不新增过期列。R11 的
//     「不新增过期批处理任务」被解释为「不做过期状态物化批处理」，与
//     「删除超保留期的死码」不冲突。
//   - 已使用（used）与已禁用（disabled）**永不删除**：前者是已入账的
//     审计事实，后者是管理员的操作内容，回收它们等于丢账。
//   - 保留期从 `expires_at` 起算（不是 created_at）：码在有效期内不占用
//     回收窗口，过期后还要再留 `redemption.retention_days` 天才删。
//
// 【0 = 关闭】`redemption.retention_days <= 0` 表示不回收（冻结语义，
// 与 N9 两个保留键一致）。0 不是「立即删除」：过期码仍会被核销路径实时
// 拒绝，只是不再物理回收。
//
// 【分批 + 上限】保留期外的行在积压时可能很多，一个大 DELETE 会长时间
// 持有行锁并放大 WAL。每批 `LIMIT` 行、单轮最多 `MaxBatches` 批，未清完
// 的积压由下一轮继续（按 expires_at 最旧优先，不会饿死）。
//
// 【幂等】删除谓词是纯时间/状态条件，重复执行只会得到 affected=0；没有
// 水位或游标需要维护，进程崩溃后重跑安全。
//
// 【跨副本】单副本部署由平台保证（R-04），但滚动升级期间可能并存新旧
// 副本。沿用既有 worker 的 Redis `SET NX PX` 租约：抢不到就跳过本轮，
// 锁 TTL 固定 1 小时而不是扫描间隔（24h）——日更任务崩溃后最多等 1 小时
// 即可被其它副本接管，而不是被一行残留锁卡一整天。

const (
	// DefaultRedemptionRetentionDays 是设计默认保留期（billing-06 §7：
	// 过期未使用码保留 90 天）。注册表默认值与此保持一致。
	DefaultRedemptionRetentionDays = 90

	// RedemptionRetentionInterval 是回收周期（billing-06 §7：每日一次）。
	// 冻结为常量而不是环境变量：保留期是天的量级，扫描频率改变不会改变
	// 删除边界，不值得再引入一个配置面。
	RedemptionRetentionInterval = 24 * time.Hour

	// RedemptionRetentionBatchSize 是单条 DELETE 的最大行数。
	RedemptionRetentionBatchSize = 1000

	// RedemptionRetentionMaxBatchesPerPass 限制单轮工作量（默认每轮最多
	// 1 万行）。未清完的积压下一轮继续，避免回收循环长时间占住 goroutine。
	RedemptionRetentionMaxBatchesPerPass = 10

	// redemptionRetentionLockKey 是跨副本租约键。
	redemptionRetentionLockKey = "newtg:redemption-retention:lock:tick"
	// redemptionRetentionLockTTL 只需覆盖一轮回收（最多 1 万行分批删除）；
	// 固定 1 小时让崩溃后的接手延迟有界。
	redemptionRetentionLockTTL = time.Hour
)

// RedemptionRetentionDeps 是回收循环的依赖与可调参数。零值使用默认值：
//   - Now → time.Now；Logger → slog.Default()；
//   - Interval → RedemptionRetentionInterval；BatchSize → RedemptionRetentionBatchSize；
//   - MaxBatches → RedemptionRetentionMaxBatchesPerPass；
//   - Delete → 绑定 DB 的生产删除函数（此时 DB 必填）。
//
// RetentionDays 是**已解析**的生效值（平台在启动时把设置中心的键与设计
// 默认合并成 int）：<=0 表示关闭。
type RedemptionRetentionDeps struct {
	DB       *sql.DB
	Redis    *redis.Client
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration

	RetentionDays int
	BatchSize     int
	MaxBatches    int

	// Delete 是批量删除接缝（测试注入 fake；nil 时用 DB 的生产 SQL）。
	Delete func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

type redemptionRetentionWorker struct {
	redis         *redis.Client
	log           *slog.Logger
	now           func() time.Time
	interval      time.Duration
	retentionDays int
	batchSize     int
	maxBatches    int
	deleteFn      func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// RunRedemptionRetentionLoop 运行兑换码回收循环，直到 ctx 取消。
//
// 启动即回收一轮（重启后不必等一天），之后按 Interval 周期性运行。
// 单轮失败只记录并在下一轮重试——必须区分「删除失败」与「没有可删行」，
// 绝不把数据库故障当成空结果并推进任何水位（本循环没有水位，天然满足）。
func RunRedemptionRetentionLoop(ctx context.Context, deps RedemptionRetentionDeps) error {
	w, err := newRedemptionRetentionWorker(deps)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	if w.retentionDays <= 0 {
		// 冻结语义：0 = 关闭。不做任何 DB/Redis 访问，也不常驻空转。
		w.log.Info("redemption retention disabled (retention_days <= 0)")
		return nil
	}

	w.log.Info("redemption retention worker started",
		"interval", w.interval,
		"retention_days", w.retentionDays,
		"batch_size", w.batchSize,
		"max_batches", w.maxBatches,
	)

	w.runOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// 优雅停机不是错误：与 billing.RunWorkerLoop 返回 nil 一致。
			return nil
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func newRedemptionRetentionWorker(deps RedemptionRetentionDeps) (*redemptionRetentionWorker, error) {
	if deps.Redis == nil {
		return nil, errors.New("billing: redemption retention requires Redis for the cross-replica lock")
	}
	if deps.Delete == nil && deps.DB == nil {
		return nil, errors.New("billing: redemption retention requires DB or a delete function")
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
		interval = RedemptionRetentionInterval
	}
	batchSize := deps.BatchSize
	if batchSize <= 0 {
		batchSize = RedemptionRetentionBatchSize
	}
	maxBatches := deps.MaxBatches
	if maxBatches <= 0 {
		maxBatches = RedemptionRetentionMaxBatchesPerPass
	}
	deleteFn := deps.Delete
	if deleteFn == nil {
		deleteFn = func(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
			return DeleteExpiredRedemptionCodes(ctx, deps.DB, cutoff, limit)
		}
	}
	return &redemptionRetentionWorker{
		redis:         deps.Redis,
		log:           log,
		now:           now,
		interval:      interval,
		retentionDays: deps.RetentionDays,
		batchSize:     batchSize,
		maxBatches:    maxBatches,
		deleteFn:      deleteFn,
	}, nil
}

// runOnce 抢租约后执行一轮回收；抢不到说明另一副本正在执行，跳过不是错误。
func (w *redemptionRetentionWorker) runOnce(ctx context.Context) {
	if ctx.Err() != nil || w.retentionDays <= 0 {
		return
	}
	token, acquired, err := acquireWorkerLock(ctx, w.redis, redemptionRetentionLockKey, redemptionRetentionLockTTL)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// Redis 抖动不应阻断回收（幂等 SQL 本身安全），但也不在无互斥的
		// 情况下强行执行：多副本同时删同一批只会增加锁竞争，收益为零。
		w.log.Error("redemption retention skipped: acquire lock failed", "error", err)
		return
	}
	if !acquired {
		w.log.Debug("redemption retention skipped: lock held by another replica")
		return
	}
	defer releaseWorkerLock(ctx, w.redis, redemptionRetentionLockKey, token, w.log)

	deleted := w.reclaim(ctx)
	if deleted > 0 {
		w.log.Info("expired redemption codes reclaimed",
			"count", deleted, "retention_days", w.retentionDays)
	}
}

// reclaim 按批删除，返回本轮删除总数。
//
// 判据：`status='unused' AND expires_at < now - retention`。先算 cutoff 再
// 下推，避免在 SQL 里把「保留期天数」和「now」拼成表达式造成两次求值。
// 删除边界用严格小于：恰好等于 cutoff 的码再留一轮，与「超过保留期」
// 的字面语义一致（差一轮最多一天，无资金影响）。
func (w *redemptionRetentionWorker) reclaim(ctx context.Context) int64 {
	if w.retentionDays <= 0 {
		return 0
	}
	cutoff := w.now().UTC().Add(-time.Duration(w.retentionDays) * 24 * time.Hour)
	var total int64
	for batch := 0; batch < w.maxBatches; batch++ {
		if ctx.Err() != nil {
			break
		}
		deleted, err := w.deleteFn(ctx, cutoff, w.batchSize)
		if err != nil {
			// 本轮剩余批次不再执行；下一轮从头重试（删除幂等）。
			w.log.Error("redemption retention delete failed; retrying next round", "error", err,
				"deleted", total)
			return total
		}
		total += deleted
		// 不足一批说明保留期外的行已清空；多查一次只会多一个空查询。
		if deleted < int64(w.batchSize) {
			break
		}
	}
	if total >= int64(w.batchSize*w.maxBatches) {
		w.log.Warn("redemption retention hit the per-pass batch budget; backlog continues next pass",
			"deleted", total, "batch_size", w.batchSize, "max_batches", w.maxBatches)
	}
	return total
}

// DeleteExpiredRedemptionCodes 删除 `id` 属于「unused 且 expires_at 早于
// cutoff」的前 limit 行，返回实际删除行数。
//
// 【为什么用子查询 + LIMIT】PostgreSQL 的 `DELETE ... LIMIT` 不是标准语法
// （仅 MySQL 支持），子查询选主键再删是跨方言安全的写法；ORDER BY
// (expires_at, id) 让批次顺序确定，避免同秒行反复落选导致空转。
//
// 【为什么必须同时锁 status】管理员可能在删除与核销之间把码用掉：条件
// UPDATE 与 DELETE 都以 status='unused' 为前置，谁先提交谁生效；DELETE
// 后到达时核销会得到「无效码」，不会出现「已删但已入账」的双花。
func DeleteExpiredRedemptionCodes(ctx context.Context, db *sql.DB, cutoff time.Time, limit int) (int64, error) {
	if db == nil {
		return 0, errors.New("billing: redemption retention database is not configured")
	}
	if limit <= 0 {
		limit = 1
	}
	res, err := db.ExecContext(ctx, `
		DELETE FROM redemption_codes
		WHERE id IN (
			SELECT id FROM redemption_codes
			WHERE status = 'unused'
			  AND expires_at IS NOT NULL
			  AND expires_at < $1
			ORDER BY expires_at ASC, id ASC
			LIMIT $2
		)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("billing: delete expired redemption codes: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("billing: redemption codes rows affected: %w", err)
	}
	return affected, nil
}

// acquireWorkerLock 用 SET NX PX 抢跨副本租约，返回持有者令牌。
// 令牌用于 compare-and-delete：TTL 恰在操作结束前过期、另一副本已抢到
// 锁时，直接 DEL 会误删别人的锁。
func acquireWorkerLock(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration) (string, bool, error) {
	token, err := newWorkerLockToken()
	if err != nil {
		return "", false, fmt.Errorf("billing: generate lock token: %w", err)
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	acquired, err := rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("billing: acquire lock %s: %w", key, err)
	}
	return token, acquired, nil
}

// releaseWorkerLock 用 Lua 做 compare-and-delete；失败只告警，TTL 自动回收。
// 停机时 ctx 可能已取消，因此使用独立短超时，保证锁尽量被主动释放。
func releaseWorkerLock(ctx context.Context, rdb *redis.Client, key, token string, log *slog.Logger) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := releaseWorkerLockScript.Run(releaseCtx, rdb, []string{key}, token).Err(); err != nil {
		log.Warn("billing: release worker lock failed (TTL will reclaim it)", "key", key, "error", err)
	}
}

var releaseWorkerLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

func newWorkerLockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
