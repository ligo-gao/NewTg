package billing

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"newtg/gateway/internal/contract"
	"newtg/gateway/internal/metrics"
)

// 本文件是 worker 的计费兜底循环（billing-06 §5）：周期扫描在飞租约、
// 回收悬挂请求、维护 usage_logs 月分区。
//
// 单副本约束：M3 的 worker 按单副本部署（R-04；分片留 M7）。因此本循环
// 不实现 worker_task_locks 的每类型 DB 租约——正确性由「recover 幂等 +
// 扫描只读」保证：即使滚动升级期间新旧副本并存，重复扫描也不会重复退款
// （releaseAbandoned 的 status='hold' CAS 与 release 行唯一索引双重兜底）。
const (
	// defaultPartitionInterval 是 usage_logs 分区滚动维护的周期。
	// 分区按「当月 + 下月」预建（R-12），每 24h 检查一次即可覆盖跨月；
	// 启动时也必须立即执行一次，避免新月第一笔日志写入失败。
	defaultPartitionInterval = 24 * time.Hour
)

// ensureUsagePartitionsFn 是 usage_logs 月分区维护的接缝（R-12）。
//
// 默认直连 Task 6 的 EnsureUsagePartitions（同包，分区按「当月 + 下月」
// 预建、幂等）。保留变量而非直接调用，是为了让循环的单测可以注入 fake，
// 不依赖真实日志库与 DDL 权限。
//
// 失败语义：T6 的权限/锁失败返回 error，worker 只记录并下一轮重试；
// 分区维护失败不代表本轮扫描失败，两者互不阻断。
var ensureUsagePartitionsFn = EnsureUsagePartitions

// AbandonedLister 列出满足「开始超窗口且心跳超 TTL」的可回收请求
// （含 R-88 回补令牌额度所需的 user_id/token_id）。
// *RequestRegistry 是其生产实现；单测用 fake。
type AbandonedLister interface {
	ListAbandoned(ctx context.Context, now time.Time) ([]AbandonedRequest, error)
}

// AbandonedRecoverer 按 requestID 回收一个悬挂请求。
// tokenID 来自租约（0 = 旧租约/未绑定，回补令牌额度为 no-op）。
// *Engine（RecoverAbandonedByID）是其生产实现；单测用 fake。
type AbandonedRecoverer interface {
	RecoverAbandonedByID(ctx context.Context, requestID string, tokenID int64) error
}

// StaleSubscriptionRecoverer 扫描并退款长期未结算的订阅预扣（S4b；
// billing-06 §5.2 的「订阅预扣记录」状态面）。*Engine 是生产实现。
//
// 与 AbandonedLister 分开：钱包悬挂请求由 Redis 租约索引驱动，而订阅预扣
// 的判据在主库幂等表上（created_at 窗口 + request_registry 终态），两者
// 的候选集与幂等锚点不同，不能合并成一次扫描。
type StaleSubscriptionRecoverer interface {
	RecoverStaleSubscriptionPreConsumes(ctx context.Context, now time.Time, limit int) (int, error)
}

// WorkerDeps 是兜底循环的依赖与可调参数。
//
// 零值字段使用契约冻结值/默认值：
//   - ScanInterval 零值 → AbandonedScanIntervalSeconds（300s，扫描参数）；
//   - PartitionInterval 零值 → 24h；
//   - Now 零值 → time.Now；Logger 零值 → slog.Default()；
//   - EnsurePartitions 零值 → ensureUsagePartitionsFn。
type WorkerDeps struct {
	Abandoned AbandonedLister
	Recoverer AbandonedRecoverer
	// SubscriptionRecoverer 是可选的订阅预扣兜底（nil 表示未装配订阅，
	// 跳过该扫描；装配后每轮与悬挂请求扫描一起执行）。
	SubscriptionRecoverer StaleSubscriptionRecoverer
	LogsDB                *sql.DB // 日志库；nil 时跳过分区维护
	Logger                *slog.Logger
	Now                   func() time.Time
	ScanInterval          time.Duration
	PartitionInterval     time.Duration

	// EnsurePartitions 覆盖分区维护函数（测试注入）；nil 用包级接缝。
	EnsurePartitions func(ctx context.Context, logsDB *sql.DB, now time.Time) error
}

// RunWorkerLoop 运行 worker 的计费兜底循环，直到 ctx 取消。
//
// 顺序（billing-06 §5）：启动时先维护分区、立即扫一轮，然后按
// ScanInterval 周期扫描、按 PartitionInterval 周期维护分区。ctx.Done()
// 优雅退出并返回 nil（停机不是错误）。
//
// 失败语义（简报冻结）：Redis/DB 故障一律记录日志并在下一轮重试；
// 循环不因单轮失败退出，也**绝不**把「扫描失败」当成「无悬挂」。
// 回收单个请求失败不影响同轮其它请求（fault isolation）。
func RunWorkerLoop(ctx context.Context, deps WorkerDeps) error {
	if deps.Abandoned == nil {
		return errors.New("billing: worker requires an abandoned-request lister")
	}
	if deps.Recoverer == nil {
		return errors.New("billing: worker requires an abandoned-request recoverer")
	}

	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	scanInterval := deps.ScanInterval
	if scanInterval <= 0 {
		scanInterval = time.Duration(contract.AbandonedScanIntervalSeconds) * time.Second
	}
	partitionInterval := deps.PartitionInterval
	if partitionInterval <= 0 {
		partitionInterval = defaultPartitionInterval
	}
	ensurePartitions := deps.EnsurePartitions
	if ensurePartitions == nil {
		ensurePartitions = ensureUsagePartitionsFn
	}

	// 已取消的上下文不再启动任何扫描（停机竞态下不产生新的 DB/Redis 负载）。
	if ctx.Err() != nil {
		return nil
	}

	ensure := func() {
		if deps.LogsDB == nil {
			return
		}
		if err := ensurePartitions(ctx, deps.LogsDB, now()); err != nil {
			// R-56：生产 worker 以 newtg_app 运行，没有分区 DDL 权限；
			// 这是**预期身份**而不是故障——分区由日志迁移预建 12 个月。
			// 用 warn + 明确 ops 信息提示扩窗方式，绝不按 error 反复告警，
			// 也不阻断本轮悬挂扫描。
			var ddlErr *PartitionDDLUnavailableError
			if errors.As(err, &ddlErr) {
				log.Warn("usage_logs partition DDL unavailable (runtime role is not the table owner); "+
					"partitions are pre-created by the logs migration with a 12-month lookahead — "+
					"extend the lookahead with migrate credentials before it runs out",
					"schema", ddlErr.Schema,
					"schema_create", ddlErr.SchemaCreate,
					"owns_parent", ddlErr.OwnsParent)
				return
			}
			log.Error("ensure usage_logs partitions failed; retrying next round", "error", err)
		}
	}

	// 启动序：先保证日志分区存在，再处理钱包悬挂请求（回收过程可能补写日志）。
	ensure()
	scanOnce(ctx, deps.Abandoned, deps.Recoverer, log, now)
	// 订阅兜底**不在启动时立即执行**（R-W9B-02 / M-4 复核）。
	//
	// 任务轮询器与 worker 是并发启动的：任务提交路径 Detach 后到轮询器首次
	// 批量续租之间存在一个很短的「无守护者」窗口。若 worker 启动即扫，可能
	// 在轮询器完成首批续租之前就把刚交接、租约尚未刷新（或上一副本心跳已
	// 接近过期）的任务当成崩溃请求退款。首轮延后一个扫描周期（默认 300s，
	// 远大于该窗口），崩溃请求仍在候选集里，下一轮照常回收；方向安全。
	//
	// 钱包悬挂扫描不受影响：它的证据是 Redis 租约索引 + hold 行，不依赖
	// 任务轮询器续租。

	scanTicker := time.NewTicker(scanInterval)
	defer scanTicker.Stop()
	partitionTicker := time.NewTicker(partitionInterval)
	defer partitionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-scanTicker.C:
			scanOnce(ctx, deps.Abandoned, deps.Recoverer, log, now)
			recoverStaleSubscriptions(ctx, deps.SubscriptionRecoverer, log, now)
		case <-partitionTicker.C:
			ensure()
		}
	}
}

// recoverStaleSubscriptions 执行一轮订阅预扣兜底。
//
// nil 表示未装配订阅（纯钱包部署）：直接跳过，不产生任何日志噪声。
// 扫描失败只记录并下一轮重试——与钱包悬挂扫描同一失败语义，绝不把
// 「数据库不可用」当成「没有可退记录」。
func recoverStaleSubscriptions(ctx context.Context, recoverer StaleSubscriptionRecoverer,
	log *slog.Logger, now func() time.Time) {

	if recoverer == nil {
		return
	}
	// W10-METRICS：订阅兜底与钱包兜底分开计数（source 标签区分资金面）。
	start := time.Now()
	status := "succeeded"
	defer func() {
		metrics.ObserveWorkerTask("subscription_reconcile", status, time.Since(start))
	}()

	recovered, err := recoverer.RecoverStaleSubscriptionPreConsumes(ctx, now(), subscriptionRecoveryScanLimit)
	if err != nil {
		status = "failed"
		log.Error("stale subscription pre-consume recovery failed; retrying next round",
			"recovered", recovered, "error", err)
		return
	}
	if recovered > 0 {
		log.Info("recovered stale subscription pre-consume records", "count", recovered)
		// 扫描窗口是最老记录的保守下界（候选至少超过窗口才会被扫到）。
		metrics.SetWorkerAbandoned("subscription", int64(recovered),
			time.Duration(contract.AbandonedWindowSeconds)*time.Second)
	}
}

// scanOnce 执行一轮扫描与回收。
//
// 列表失败只记录错误：本轮结束，下一轮重试。单个请求回收失败同样只记录，
// 不中断同轮其它请求——一笔坏数据不应拖住整批兜底退款。
func scanOnce(ctx context.Context, lister AbandonedLister, recoverer AbandonedRecoverer,
	log *slog.Logger, now func() time.Time) {

	// W10-METRICS：一轮扫描计一次任务执行；失败只影响 status 标签，不改变
	// 既有「记录后下一轮重试」的失败语义。
	start := time.Now()
	status := "succeeded"
	var recovered int64
	defer func() {
		metrics.ObserveWorkerTask("abandoned_reconcile", status, time.Since(start))
		age := time.Duration(0)
		if recovered > 0 {
			// 候选都超过 AbandonedWindowSeconds；以窗口作为最老年龄的保守
			// 下界，A9 不会因此误报（阈值 900s > 窗口 600s）。
			age = time.Duration(contract.AbandonedWindowSeconds) * time.Second
		}
		metrics.SetWorkerAbandoned("wallet", recovered, age)
	}()

	entries, err := lister.ListAbandoned(ctx, now())
	if err != nil {
		status = "failed"
		// 【关键】扫描失败 ≠ 无悬挂：这里绝不更新任何「已处理」水位，
		// 下一轮会从头再扫（Redis 的 ZSET 索引本身就是完整候选集）。
		log.Error("abandoned request scan failed; retrying next round", "error", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	log.Info("recovering abandoned requests", "count", len(entries))
	for _, entry := range entries {
		// 停机期间不再发起新的回收：每笔回收都是有界 DB 事务，但 500 条
		// 逐个失败会拖慢优雅退出，且这些请求下一轮仍会被索引扫到。
		if ctx.Err() != nil {
			return
		}
		// tokenID 来自租约（R-88）：worker 没有原始 RelayContext，令牌额度
		// 只能靠这条持久化身份回补；0（旧租约/未绑定）表示 no-op。
		if err := recoverer.RecoverAbandonedByID(ctx, entry.RequestID, entry.TokenID); err != nil {
			log.Error("recover abandoned request failed; retrying next round",
				"request_id", entry.RequestID, "token_id", entry.TokenID, "error", err)
			continue
		}
		log.Info("recovered abandoned request",
			"request_id", entry.RequestID, "token_id", entry.TokenID)
		recovered++
	}
}
