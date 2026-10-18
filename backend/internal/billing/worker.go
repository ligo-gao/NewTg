package billing

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"newtg/gateway/internal/contract"
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

// AbandonedLister 列出满足「开始超窗口且心跳超 TTL」的 requestID。
// *RequestRegistry 是其生产实现；单测用 fake。
type AbandonedLister interface {
	ListAbandoned(ctx context.Context, now time.Time) ([]string, error)
}

// AbandonedRecoverer 按 requestID 回收一个悬挂请求。
// *Engine（RecoverAbandonedByID）是其生产实现；单测用 fake。
type AbandonedRecoverer interface {
	RecoverAbandonedByID(ctx context.Context, requestID string) error
}

// WorkerDeps 是兜底循环的依赖与可调参数。
//
// 零值字段使用契约冻结值/默认值：
//   - ScanInterval 零值 → AbandonedScanIntervalSeconds（300s，扫描参数）；
//   - PartitionInterval 零值 → 24h；
//   - Now 零值 → time.Now；Logger 零值 → slog.Default()；
//   - EnsurePartitions 零值 → ensureUsagePartitionsFn。
type WorkerDeps struct {
	Abandoned         AbandonedLister
	Recoverer         AbandonedRecoverer
	LogsDB            *sql.DB // 日志库；nil 时跳过分区维护
	Logger            *slog.Logger
	Now               func() time.Time
	ScanInterval      time.Duration
	PartitionInterval time.Duration

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

	// 启动序：先保证日志分区存在，再处理悬挂请求（回收过程可能补写日志）。
	ensure()
	scanOnce(ctx, deps.Abandoned, deps.Recoverer, log, now)

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
		case <-partitionTicker.C:
			ensure()
		}
	}
}

// scanOnce 执行一轮扫描与回收。
//
// 列表失败只记录错误：本轮结束，下一轮重试。单个请求回收失败同样只记录，
// 不中断同轮其它请求——一笔坏数据不应拖住整批兜底退款。
func scanOnce(ctx context.Context, lister AbandonedLister, recoverer AbandonedRecoverer,
	log *slog.Logger, now func() time.Time) {

	ids, err := lister.ListAbandoned(ctx, now())
	if err != nil {
		// 【关键】扫描失败 ≠ 无悬挂：这里绝不更新任何「已处理」水位，
		// 下一轮会从头再扫（Redis 的 ZSET 索引本身就是完整候选集）。
		log.Error("abandoned request scan failed; retrying next round", "error", err)
		return
	}
	if len(ids) == 0 {
		return
	}

	log.Info("recovering abandoned requests", "count", len(ids))
	for _, id := range ids {
		// 停机期间不再发起新的回收：每笔回收都是有界 DB 事务，但 500 条
		// 逐个失败会拖慢优雅退出，且这些请求下一轮仍会被索引扫到。
		if ctx.Err() != nil {
			return
		}
		if err := recoverer.RecoverAbandonedByID(ctx, id); err != nil {
			log.Error("recover abandoned request failed; retrying next round",
				"request_id", id, "error", err)
			continue
		}
		log.Info("recovered abandoned request", "request_id", id)
	}
}
