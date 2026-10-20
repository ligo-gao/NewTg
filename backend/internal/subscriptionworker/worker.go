package subscriptionworker

// 订阅域的 B12 后台任务（billing-06 §9.3、billing-08 §7）。
//
// 本包只负责三件周期性、可重复执行的数据维护：
//  1. 到期：宽限期只适用于「正常周期末」——未取消续费的实例在
//     end_at + grace_days <= now 时置 expired；cancel_at_period_end=true
//     表示用户明确不续费，到期即终止、**不享宽限**（end_at <= now）。
//     两条分支必须在同一条条件 UPDATE 里，避免宽限期把已取消实例留活。
//  2. 额度重置：next_reset_at <= now 且仍在本周期内（end_at > now）的
//     active 实例把 amount_used 归零并推进游标。宽限期内不发放新周期额度
//     （billing-08 §3.4）；amount_total=0（无限额度）也与其他实例一样归零。
//     推进游标是幂等的核心：没有它，同一实例会在每个扫描周期被重复重置
//     （双倍赠额，属于严重资损）。
//  3. 清理：删除 7 天前的 subscription_pre_consume_records，分批 LIMIT，
//     避免长事务拖慢资金路径的预扣/退款。
//
// 【单副本语义】调度框架（worker_task_locks 租约）由部署层保证单副本；
// 本包按现有 rankings worker 的简化模式使用 Redis `SET NX PX` 作为跨副本
// 互斥：锁 TTL = 本轮扫描间隔，进程崩溃后最多一个间隔自动释放。Redis 是
// 缓存不是权威——真正的正确性来自每条 SQL 的条件 UPDATE（重复执行
// affected=0），所以锁获取失败时跳过本轮而不是在没有互斥的情况下执行。
// Redis 运行时不可用同样只跳过本轮并告警，不阻断 worker 启动。
//
// 【时间口径】所有判定使用调用方注入的 now（UTC）；生产传 time.Now，
// 测试注入固定时刻。到期/重置都是边界敏感操作，跨时区进程必须看到
// 同一个「现在」。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/store"
)

const (
	// DefaultSweepInterval 是到期/重置扫描周期（billing-06 §9.3：每分钟）。
	DefaultSweepInterval = 60 * time.Second

	// DefaultCleanupInterval 是幂等记录清理扫描周期（billing-08 §7 / C-3：30 分钟）。
	//
	// 它不是保留期：保留期恒为 7 天（PreConsumeRetention），间隔只决定
	// 「过期数据最多多留多久」，与资金正确性无关。C-3 已澄清两者的区别，
	// 实现按设计取 30 分钟。
	DefaultCleanupInterval = 30 * time.Minute

	// PreConsumeRetention 是预扣幂等记录的保留期（billing-04 §3.4 /
	// billing-08 §7）。不能短于请求可能延迟结算的窗口，否则迟到的
	// Terminate/Refund 找不到幂等记录，会重复扣减或无法退款。
	PreConsumeRetention = 7 * 24 * time.Hour

	// CleanupBatchSize 是单条 DELETE 的最大行数。LIMIT 把锁与 WAL 压力
	// 限制在有界事务内；清理期间仍可正常预扣/退款。
	CleanupBatchSize = 500

	// CleanupMaxBatchesPerPass 限制单轮清理的工作量（默认每轮 1 万行，按 30 分钟间隔即每小时 2 万行）。
	// 有界工作量让清理不会长时间占住 unique goroutine，从而推迟同一 select
	// 里的到期/重置扫描；未清完的积压由下一轮继续（最旧优先）。
	CleanupMaxBatchesPerPass = 20
)

// 锁键。TTL = 对应扫描间隔：崩溃后最多一个间隔即可被其他副本接管。
const (
	SweepLockKey   = "newtg:subscription-worker:lock:sweep"
	CleanupLockKey = "newtg:subscription-worker:lock:cleanup"
)

// Repository 是本 worker 需要的订阅存储接缝。
//
// 刻意不复用 store.SubscriptionRepo 全接口：worker 只依赖这三个方法，
// 窄接口让单测 fake 不必实现套餐 CRUD / 预扣写入等无关能力。
// *store.SQLSubscriptionRepo 是生产实现。
type Repository interface {
	// ExpireDue 条件更新到期实例，返回实际迁移行数。
	ExpireDue(ctx context.Context, now time.Time) (int64, error)
	// DueForReset 返回 next_reset_at 已到期且仍在本周期内的 active 实例
	// （只读扫描；宽限期实例由 SQL 的 end_at > now 排除）。
	DueForReset(ctx context.Context, now time.Time) ([]store.Subscription, error)
	// ResetSubscriptionQuota 把所有实例（含 amount_total=0）的 amount_used
	// 归零，并把 next_reset_at 一次推进到第一个严格晚于 now 的周期边界。
	ResetSubscriptionQuota(ctx context.Context, id int64, now time.Time) (int64, error)
}

// Cleaner 是预扣幂等记录的删除接缝。
type Cleaner interface {
	DeletePreConsumeBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// Deps 是 worker 的运行依赖与可调参数。零值参数使用包级默认值。
type Deps struct {
	// Repo 是订阅存储（必填）。
	Repo Repository
	// Cleaner 是幂等记录清理（必填；生产传同一个 *store.SQLSubscriptionRepo）。
	Cleaner Cleaner
	// Redis 提供跨副本互斥锁（必填）。运行时错误只跳过本轮，不退出循环。
	Redis *redis.Client
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Now 为空时使用 time.Now。
	Now func() time.Time

	// SweepInterval / CleanupInterval 覆盖默认周期（测试注入短周期）。
	SweepInterval   time.Duration
	CleanupInterval time.Duration
	// Retention 覆盖保留期（测试注入边界时刻）。
	Retention time.Duration
	// BatchSize / MaxBatches 覆盖单批与单轮上限。
	BatchSize  int
	MaxBatches int
}

// SweepResult 是一轮到期/重置的实际效果（affected 而非扫描数）。
type SweepResult struct {
	// Expired 是本轮由 active 迁移为 expired 的实例数。
	Expired int64
	// Reset 是本轮真正重置了额度并推进游标的实例数（含无限额度实例）。
	Reset int64
}

type worker struct {
	repo            Repository
	cleaner         Cleaner
	redis           *redis.Client
	log             *slog.Logger
	now             func() time.Time
	sweepInterval   time.Duration
	cleanupInterval time.Duration
	retention       time.Duration
	batchSize       int
	maxBatches      int
}

func newWorker(deps Deps) (*worker, error) {
	if deps.Repo == nil {
		return nil, errors.New("subscriptionworker: Repo is required")
	}
	if deps.Cleaner == nil {
		return nil, errors.New("subscriptionworker: Cleaner is required")
	}
	if deps.Redis == nil {
		return nil, errors.New("subscriptionworker: Redis is required for the cross-replica lock")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	sweepInterval := deps.SweepInterval
	if sweepInterval <= 0 {
		sweepInterval = DefaultSweepInterval
	}
	cleanupInterval := deps.CleanupInterval
	if cleanupInterval <= 0 {
		cleanupInterval = DefaultCleanupInterval
	}
	retention := deps.Retention
	if retention <= 0 {
		retention = PreConsumeRetention
	}
	batchSize := deps.BatchSize
	if batchSize <= 0 {
		batchSize = CleanupBatchSize
	}
	maxBatches := deps.MaxBatches
	if maxBatches <= 0 {
		maxBatches = CleanupMaxBatchesPerPass
	}
	return &worker{
		repo:            deps.Repo,
		cleaner:         deps.Cleaner,
		redis:           deps.Redis,
		log:             log,
		now:             now,
		sweepInterval:   sweepInterval,
		cleanupInterval: cleanupInterval,
		retention:       retention,
		batchSize:       batchSize,
		maxBatches:      maxBatches,
	}, nil
}

// RunLoop 阻塞运行 B12 的到期/重置与清理循环，直到 ctx 取消。
//
// 调用方（platform.runWorker）把它放进独立 goroutine 并用 WaitGroup 等待，
// 保证停机时本循环的 SQL 先于连接池关闭退出。启动即各跑一轮：重启后不必
// 等一个完整周期才回收积压的到期实例与过期幂等记录。
//
// 两个周期共用一个 goroutine 串行执行：单副本下没有并发收益，串行还避免
// 清理的批量 DELETE 与重置扫描争抢连接/锁。单轮失败只记录，下一轮重试。
func RunLoop(ctx context.Context, deps Deps) error {
	w, err := newWorker(deps)
	if err != nil {
		return err
	}
	// 已取消的上下文不再启动任何 DB 负载（停机竞态）。
	if ctx.Err() != nil {
		return nil
	}

	w.log.Info("subscription worker started",
		"sweep_interval", w.sweepInterval,
		"cleanup_interval", w.cleanupInterval,
		"retention", w.retention,
	)

	w.sweepOnce(ctx)
	w.cleanupOnce(ctx)

	sweepTicker := time.NewTicker(w.sweepInterval)
	defer sweepTicker.Stop()
	cleanupTicker := time.NewTicker(w.cleanupInterval)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 优雅停机不是错误：与 billing.RunWorkerLoop 返回 nil 一致。
			return nil
		case <-sweepTicker.C:
			w.sweepOnce(ctx)
		case <-cleanupTicker.C:
			w.cleanupOnce(ctx)
		}
	}
}

// sweepOnce 执行一轮并记录结果。错误只记录：DB/Redis 故障在下一轮重试，
// 不把「扫描失败」伪装成「没有到期订阅」。
func (w *worker) sweepOnce(ctx context.Context) {
	res, ran, err := w.sweep(ctx)
	if err != nil && ctx.Err() == nil {
		w.log.Error("subscription sweep failed", "error", err)
	}
	if ran && (res.Expired > 0 || res.Reset > 0) {
		w.log.Info("subscriptions swept",
			"expired", res.Expired,
			"reset", res.Reset,
		)
	}
}

// sweep 抢锁后执行到期 + 重置，返回是否真正执行（ran）与实际计数。
func (w *worker) sweep(ctx context.Context) (SweepResult, bool, error) {
	var zero SweepResult
	if ctx.Err() != nil {
		return zero, false, nil
	}
	token, acquired, err := w.acquireLock(ctx, SweepLockKey, w.sweepInterval)
	if err != nil {
		if ctx.Err() != nil {
			return zero, false, nil
		}
		return zero, false, err
	}
	if !acquired {
		// 多副本部署下的预期路径：另一副本正在扫描。不是错误。
		w.log.Debug("subscription sweep skipped: lock held by another replica")
		return zero, false, nil
	}
	defer w.releaseLock(SweepLockKey, token)

	now := w.now().UTC()
	var res SweepResult
	var errs []error

	expired, err := w.repo.ExpireDue(ctx, now)
	if err != nil {
		errs = append(errs, err)
	} else {
		res.Expired = expired
	}

	due, err := w.repo.DueForReset(ctx, now)
	if err != nil {
		errs = append(errs, err)
		return res, true, errors.Join(errs...)
	}
	for _, sub := range due {
		if ctx.Err() != nil {
			break
		}
		// 所有实例统一归零 amount_used 并推进游标，amount_total=0（无限
		// 额度，R-D5-07）不例外：amount_used 是**本周期**已用额度，重置
		// 语义对所有实例一致；「累计用量」不属于本列。
		affected, rerr := w.repo.ResetSubscriptionQuota(ctx, sub.ID, now)
		if rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		res.Reset += affected
	}
	return res, true, errors.Join(errs...)
}

// cleanupOnce 执行一轮清理并记录结果。
func (w *worker) cleanupOnce(ctx context.Context) {
	deleted, ran, err := w.cleanup(ctx)
	if err != nil && ctx.Err() == nil {
		w.log.Error("subscription pre-consume cleanup failed", "error", err)
	}
	if ran && deleted > 0 {
		w.log.Info("subscription pre-consume records deleted", "count", deleted)
	}
}

// cleanup 分批删除保留期外的幂等记录，返回删除总数与是否真正执行。
func (w *worker) cleanup(ctx context.Context) (int64, bool, error) {
	if ctx.Err() != nil {
		return 0, false, nil
	}
	token, acquired, err := w.acquireLock(ctx, CleanupLockKey, w.cleanupInterval)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !acquired {
		w.log.Debug("subscription cleanup skipped: lock held by another replica")
		return 0, false, nil
	}
	defer w.releaseLock(CleanupLockKey, token)

	cutoff := w.now().UTC().Add(-w.retention)
	var total int64
	for batch := 0; batch < w.maxBatches; batch++ {
		if ctx.Err() != nil {
			break
		}
		deleted, err := w.cleaner.DeletePreConsumeBefore(ctx, cutoff, w.batchSize)
		if err != nil {
			return total, true, err
		}
		total += deleted
		// 不足一批说明保留期外的行已清空；多删一次只会多一个空查询。
		if deleted < int64(w.batchSize) {
			break
		}
	}
	if total >= int64(w.batchSize*w.maxBatches) {
		w.log.Warn("subscription cleanup hit the per-pass batch budget; backlog continues next pass",
			"deleted", total, "batch_size", w.batchSize, "max_batches", w.maxBatches)
	}
	return total, true, nil
}

// acquireLock 用 SET NX PX 抢本轮租约，返回持有者令牌。
//
// 令牌用于 compare-and-delete：TTL 恰在操作结束前过期、另一个副本已经
// 抢到锁时，直接 DEL 会误删别人的锁。
func (w *worker) acquireLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	token, err := newLockToken()
	if err != nil {
		return "", false, fmt.Errorf("subscriptionworker: generate lock token: %w", err)
	}
	if ttl <= 0 {
		ttl = time.Second
	}
	acquired, err := w.redis.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("subscriptionworker: acquire lock %s: %w", key, err)
	}
	return token, acquired, nil
}

// releaseLock 用 Lua 做 compare-and-delete；失败只告警，TTL 会自动回收。
// 停机时 ctx 可能已取消，因此使用独立短超时，保证锁尽量被主动释放。
func (w *worker) releaseLock(key, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := releaseLockScript.Run(ctx, w.redis, []string{key}, token).Err(); err != nil {
		w.log.Warn("subscription worker: release lock failed (TTL will reclaim it)",
			"key", key, "error", err)
	}
}

var releaseLockScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`)

// newLockToken 生成跨副本唯一的持有者令牌。
func newLockToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
