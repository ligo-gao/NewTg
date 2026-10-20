package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/deploy/ionet"
)

// WorkerDeps 是 GPU 计费 worker 的装配输入。
type WorkerDeps struct {
	// Ledger 必填（持有 DB 与 provider 接缝）。
	Ledger *Ledger
	// Provider 必填：每轮重新构造 provider（密钥可能轮换）。
	Provider ProviderFunc
	// Redis 可选：提供跨副本租约；nil 时按单副本运行（SQL 幂等仍是正确性兜底）。
	Redis *redis.Client
	// Logger 为空时使用 slog.Default。
	Logger *slog.Logger
	// Now 为空时使用 time.Now。
	Now func() time.Time
	// AccrualInterval 默认每小时；ReconcileInterval 默认 60s。
	AccrualInterval   time.Duration
	ReconcileInterval time.Duration
	// LeaseTTL 默认 300s（N16 的心跳窗口）。
	LeaseTTL time.Duration
	// BatchSize 默认 500。
	BatchSize int
}

// Worker 是 N16 + 周期记账的驱动器。
//
// 设计取舍：记账与对账串行在同一 goroutine（与 subscriptionworker 相同）。
// 两者都写同一批段，并发只会增加锁竞争；单副本语义由 Redis 租约收敛。
type Worker struct {
	ledger            *Ledger
	provider          ProviderFunc
	redis             *redis.Client
	log               *slog.Logger
	now               func() time.Time
	accrualInterval   time.Duration
	reconcileInterval time.Duration
	leaseTTL          time.Duration
	batchSize         int
}

// AccrueStats 是一轮记账的统计。
type AccrueStats struct {
	DeploymentsSeen int
	SegmentsCreated int
	SegmentsAccrued int
	FactsWritten    int
	Errors          int
}

// ReconcileStats 是一轮对账的统计。
type ReconcileStats struct {
	SegmentsScanned  int
	SegmentsSettled  int
	SegmentsReleased int
	Errors           int
}

// NewWorker 校验依赖并应用默认值。
func NewWorker(deps WorkerDeps) (*Worker, error) {
	if deps.Ledger == nil {
		return nil, errors.New("deploy billing: worker ledger is required")
	}
	if deps.Provider == nil {
		return nil, errors.New("deploy billing: worker provider is required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	accrual := deps.AccrualInterval
	if accrual <= 0 {
		accrual = DefaultAccrualInterval
	}
	reconcile := deps.ReconcileInterval
	if reconcile <= 0 {
		reconcile = DefaultReconcileInterval
	}
	lease := deps.LeaseTTL
	if lease <= 0 {
		lease = DeploymentBillingLeaseTTLSeconds
	}
	batch := deps.BatchSize
	if batch <= 0 {
		batch = accrueBatchSize
	}
	deps.Ledger.SetProvider(deps.Provider)
	return &Worker{
		ledger:            deps.Ledger,
		provider:          deps.Provider,
		redis:             deps.Redis,
		log:               log,
		now:               now,
		accrualInterval:   accrual,
		reconcileInterval: reconcile,
		leaseTTL:          lease,
		batchSize:         batch,
	}, nil
}

// RunLoop 阻塞运行记账与对账循环，直到 ctx 取消。
//
// 启动即各跑一轮：重启后的「补算」不必等一个完整小时——已关闭小时的事实
// 按整点边界重算，ON CONFLICT 保证不双计；终态段由对账立刻收敛。
func RunLoop(ctx context.Context, deps WorkerDeps) error {
	w, err := NewWorker(deps)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	w.log.Info("deploy billing worker started",
		"accrual_interval", w.accrualInterval,
		"reconcile_interval", w.reconcileInterval,
		"lease_ttl", w.leaseTTL,
	)

	w.accrueOnce(ctx)
	w.reconcileOnce(ctx)

	accrualTicker := time.NewTicker(w.accrualInterval)
	defer accrualTicker.Stop()
	reconcileTicker := time.NewTicker(w.reconcileInterval)
	defer reconcileTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("deploy billing worker stopped")
			return nil
		case <-accrualTicker.C:
			w.accrueOnce(ctx)
		case <-reconcileTicker.C:
			w.reconcileOnce(ctx)
		}
	}
}

func (w *Worker) accrueOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	stats, err := w.AccrueOnce(ctx)
	if err != nil && ctx.Err() == nil {
		w.log.Error("deploy billing: accrual pass failed", "error", err)
	}
	if stats.FactsWritten > 0 || stats.SegmentsCreated > 0 {
		w.log.Info("deploy billing: accrual pass",
			"deployments_seen", stats.DeploymentsSeen,
			"segments_created", stats.SegmentsCreated,
			"segments_accrued", stats.SegmentsAccrued,
			"facts_written", stats.FactsWritten,
			"errors", stats.Errors,
		)
	}
}

func (w *Worker) reconcileOnce(ctx context.Context) {
	if ctx.Err() == nil {
		stats, err := w.ReconcileOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error("deploy billing: reconcile pass failed", "error", err)
		}
		if stats.SegmentsSettled > 0 || stats.SegmentsReleased > 0 {
			w.log.Info("deploy billing: reconcile pass",
				"scanned", stats.SegmentsScanned,
				"settled", stats.SegmentsSettled,
				"released", stats.SegmentsReleased,
				"errors", stats.Errors,
			)
		}
	}
}

// AccrueOnce 执行一轮「发现运行中部署 → 建段 → 按整点累计」。
//
// 多副本：Redis 租约只是节流（避免同一时刻重复打 provider）；真正的幂等
// 来自 Idempotency-Key 唯一约束与 fact_key 唯一约束。
func (w *Worker) AccrueOnce(ctx context.Context) (AccrueStats, error) {
	var stats AccrueStats
	var zero AccrueStats
	token, acquired, err := w.acquireLock(ctx, "newtg:deploy-billing:lock:accrual", w.accrualInterval)
	if err != nil {
		return zero, err
	}
	if !acquired {
		return zero, nil
	}
	defer w.releaseLock(ctx, "newtg:deploy-billing:lock:accrual", token)

	provider, ok, err := w.provider(ctx)
	if err != nil {
		// 设置/密钥问题不阻断已有段的内部记账：继续走下面的 DB 累计。
		w.log.Warn("deploy billing: provider unavailable", "error", err)
		stats.Errors++
	}
	if ok && provider != nil {
		seen, created, errs := w.syncDeployments(ctx, provider)
		stats.DeploymentsSeen = seen
		stats.SegmentsCreated = created
		stats.Errors += errs
	}

	// keyset 分页处理**全部**运行段：单页 500 的 LIMIT 只限制每页大小，
	// 不能只扫最老的一批（否则段数超过批量时后面的段永远不记账，M4 复核）。
	var cursorStarted time.Time
	var cursorID string
	for {
		if ctx.Err() != nil {
			break
		}
		segments, err := w.ledger.store.listRunningSegmentsAfter(ctx, cursorStarted, cursorID, w.batchSize)
		if err != nil {
			return stats, err
		}
		if len(segments) == 0 {
			break
		}
		for _, seg := range segments {
			if ctx.Err() != nil {
				break
			}
			n, err := w.ledger.Accrue(ctx, seg.SegmentID)
			if err != nil {
				stats.Errors++
				w.log.Warn("deploy billing: accrue segment failed",
					"segment_id", seg.SegmentID, "error", err)
				continue
			}
			if n > 0 {
				stats.SegmentsAccrued++
				stats.FactsWritten += n
			}
		}
		last := segments[len(segments)-1]
		if last.StartedAt == nil {
			// 查询条件已排除，这里只是防御：无法推进游标时必须停止，
			// 否则会空转。
			break
		}
		cursorStarted = *last.StartedAt
		cursorID = last.SegmentID
		if len(segments) < w.batchSize {
			break
		}
	}
	return stats, nil
}

// ReconcileOnce 执行一轮 N16 对账（扫描心跳过期的非终态段）。
func (w *Worker) ReconcileOnce(ctx context.Context) (ReconcileStats, error) {
	var stats ReconcileStats
	var zero ReconcileStats
	token, acquired, err := w.acquireLock(ctx, "newtg:deploy-billing:lock:reconcile", w.reconcileInterval)
	if err != nil {
		return zero, err
	}
	if !acquired {
		return zero, nil
	}
	defer w.releaseLock(ctx, "newtg:deploy-billing:lock:reconcile", token)

	now := w.now().UTC()
	segments, err := w.ledger.store.listStaleSegments(ctx, now, w.leaseTTL, w.batchSize)
	if err != nil {
		return stats, err
	}
	stats.SegmentsScanned = len(segments)
	for _, seg := range segments {
		if ctx.Err() != nil {
			break
		}
		state, err := w.ledger.Reconcile(ctx, seg.SegmentID)
		if err != nil {
			stats.Errors++
			w.log.Warn("deploy billing: reconcile segment failed",
				"segment_id", seg.SegmentID,
				"provider_state", "unknown",
				"error", err)
			continue
		}
		switch state {
		case SegmentSettled:
			stats.SegmentsSettled++
		case SegmentAbandoned:
			stats.SegmentsReleased++
		}
	}
	return stats, nil
}

// syncDeployments 发现 provider 上运行中的部署并确保有幂等段。
//
// 这是本批唯一能创建段的路径（管理面写路径的 Hold 接线见台账残余项）；
// 幂等键由部署 ID + provider 起点推导，重启/多副本都命中同一段。
func (w *Worker) syncDeployments(ctx context.Context, provider ionet.DeploymentProvider) (seen, created, errs int) {
	const pageSize = 100
	const maxPages = 50
	now := w.now().UTC()
	for page := 1; page <= maxPages; page++ {
		// 不在 provider 侧按 status 过滤：真实 provider 的状态词表未实测
		// （R-M6D-13），管理面同样采用「全量扫描 + 本地过滤」；这样也不会
		// 因大小写/同义词差异漏掉运行中的部署。
		result, err := provider.ListDeployments(ctx, ionet.DeploymentListOptions{
			Page: page, PageSize: pageSize,
		})
		if err != nil {
			w.log.Warn("deploy billing: list deployments failed", "page", page, "error", err)
			return seen, created, errs + 1
		}
		for _, item := range result.Items {
			if ctx.Err() != nil {
				return seen, created, errs
			}
			if item.ID == "" || ClassifyStatus(item.Status) != StatusRunning {
				continue
			}
			seen++
			if _, err := w.ledger.ActiveSegmentForDeployment(ctx, item.ID); err == nil {
				continue
			} else if !errors.Is(err, ErrSegmentNotFound) {
				errs++
				w.log.Warn("deploy billing: lookup active segment failed",
					"deployment_id", item.ID, "error", err)
				continue
			}
			detail, err := provider.GetDeployment(ctx, item.ID)
			if err != nil {
				errs++
				w.log.Warn("deploy billing: get deployment failed",
					"deployment_id", item.ID, "error", err)
				continue
			}
			if ClassifyStatus(detail.Status) != StatusRunning {
				continue
			}
			snapshot, err := snapshotFromDetail(ctx, provider, detail)
			if err != nil {
				errs++
				w.log.Warn("deploy billing: deployment not billable",
					"deployment_id", item.ID, "error", err)
				continue
			}
			// detail 契约没有 name；列表行有。保留 provider 的部署名用于对账。
			if item.Name != "" {
				snapshot.Name = item.Name
			}
			estimate, err := estimateFromSnapshot(snapshot, now)
			if err != nil {
				errs++
				w.log.Warn("deploy billing: estimate failed",
					"deployment_id", item.ID, "error", err)
				continue
			}
			seg, isNew, err := w.ledger.Hold(ctx, estimate)
			if err != nil {
				errs++
				w.log.Warn("deploy billing: hold segment failed",
					"deployment_id", item.ID, "error", err)
				continue
			}
			if isNew {
				created++
			}
			if _, err := w.ledger.Accrue(ctx, seg.SegmentID); err != nil {
				errs++
				w.log.Warn("deploy billing: immediate accrue failed",
					"segment_id", seg.SegmentID, "error", err)
			}
		}
		total := result.Total
		if len(result.Items) < pageSize {
			break
		}
		if total > 0 && page*pageSize >= total {
			break
		}
	}
	return seen, created, errs
}

// acquireLock 用 SET NX PX 抢跨副本租约；Redis 不可用时跳过本轮（与其它
// worker 口径一致：宁可少跑一轮，也不在无互斥的情况下重复写）。
func (w *Worker) acquireLock(ctx context.Context, key string, ttl time.Duration) (string, bool, error) {
	if w.redis == nil {
		return "", true, nil
	}
	token, err := lockToken()
	if err != nil {
		return "", false, err
	}
	ok, err := w.redis.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, err
	}
	return token, ok, nil
}

func (w *Worker) releaseLock(ctx context.Context, key, token string) {
	if w.redis == nil || token == "" {
		return
	}
	if err := releaseLockScript.Run(ctx, w.redis, []string{key}, token).Err(); err != nil &&
		!errors.Is(err, redis.Nil) && ctx.Err() == nil {
		w.log.Warn("deploy billing: release lock failed", "key", key, "error", err)
	}
}

var releaseLockScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("del", KEYS[1])
end
return 0
`)

func lockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("deploy billing: lock token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
