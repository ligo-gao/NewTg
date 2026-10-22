package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/metrics"
)

// 本文件是 W10-N10 的过期 `usage_logs` 分区删除（R-W10-03；billing-06 §6.2、
// platform-01 §12、G7-R47）。
//
// 【为什么默认关闭 · R-W10-03 A】设计原文是「默认开」，但 year 排行榜仍直接
// 聚合 12 个月 `usage_logs`（rankings/aggregate.go），而设计承诺的
// `usage_dashboards`（13 个月聚合）在代码与迁移中都不存在。90 天后 DROP 会让
// year 榜**静默**退化为「最近 90 天」。因此本波冻结为：设置键
// `log.partition_drop_enabled` 默认 false，只有在 `usage_dashboards` 落地或
// year 榜改读 `rankings_timeseries` 之后才允许开启（上线闸门，已登记）。
//
// 【执行主体 · R-W10-01 A】删除必须用独立的 `DB_RETENTION_DSN`
// （newtg_retention），**绝不复用 app DSN**：运行时应用身份没有 DDL/所有权，
// 用它执行 DROP 会把「身份配置错误」变成权限噪音甚至误删。装配层在 DSN
// 缺失时 fail-closed（不启动本循环），本文件的循环本身也拒绝 nil DB。
//
// 【为什么 DROP 子分区而不是 DETACH + DROP】`ALTER TABLE ... DETACH PARTITION`
// 要求**父表**属主；把 `usage_logs` 整表属主交给维护角色等于允许它 DROP 整个
// 日志表，违反最小权限。实测（PG16）子分区属主可直接 `DROP TABLE` 子分区，
// PostgreSQL 会隐式完成 detach。因此保留角色只拥有**子分区**（迁移与预建时
// `ALTER TABLE ... OWNER TO newtg_retention`），删除路径是单条 DROP。
//
// 【只删完全过期的分区】判据是分区**上界** <= cutoff（now - 保留期），即
// 分区内所有行都已超期。当前月/未来月分区上界必然 > cutoff，永不参与删除；
// 无候选时是纯 no-op（幂等，无水位）。
//
// 【失败语义】单轮失败只记录并在下一轮重试；权限/所有权不足返回可识别的
// `PartitionRetentionUnavailableError`，循环记 warn 并跳过（不重试风暴）。
// 删除失败不影响分区预建（两条循环独立，预建仍用 app 身份静默 no-op）。
//
// 【单轮上限】每轮最多删除 `BatchSize` 个分区（默认 2）。DROP 是重 DDL
// （ACCESS EXCLUSIVE + 文件删除），小批把锁与 IO 压力限制在有界窗口内；
// 积压由下一轮继续（按上界最旧优先）。

const (
	// DefaultPartitionRetentionDays 是请求日志保留期（G7-R47：3 个月 = 90 天）。
	//
	// 设计 #36 的 `log.retention_days` 是配置载体，但本波冻结口径只登记
	// `log.partition_*` 两键；保留期先按设计默认固定为 90 天（只影响「删哪个
	// 过期分区」，不影响资金/审计）。后续补登记该键时替换此常量即可。
	DefaultPartitionRetentionDays = 90

	// PartitionDropInterval 是删除周期（billing-06 §6.2：每天夜间小批）。
	// 保留期以天计，更频繁的检查不会改变删除边界。
	PartitionDropInterval = 24 * time.Hour

	// PartitionDropBatchSize 是单轮最多删除的分区数。
	PartitionDropBatchSize = 2

	// partitionDropLockKey 是跨副本租约键。
	partitionDropLockKey = "newtg:log-partition-drop:lock:tick"
	// partitionDropLockTTL 只需覆盖一轮删除（最多 2 个 DROP）；固定 1 小时让
	// 崩溃后的接手延迟有界，而不是被日更间隔卡住一整天。
	partitionDropLockTTL = time.Hour
)

// UsagePartition 是一个 `usage_logs_YYYYMM` 月分区的元数据。
type UsagePartition struct {
	Schema string
	Name   string
	// From/To 是分区右开区间 [From, To)，UTC。
	From time.Time
	To   time.Time
}

// PartitionRetentionUnavailableError 表示当前连接不是过期分区的属主
// （通常意味着误用了 app DSN，或迁移没有把分区所有权转给 newtg_retention）。
//
// 循环把它当**配置/身份问题**记 warn 并跳过本轮：重复尝试 DDL 只会得到
// 同一个 permission denied，重试风暴既无用又污染告警。
type PartitionRetentionUnavailableError struct {
	Schema    string
	Partition string
}

func (e *PartitionRetentionUnavailableError) Error() string {
	return fmt.Sprintf(
		"billing: retention role does not own partition %s.%s; "+
			"run the logs retention migration / ALTER TABLE ... OWNER TO newtg_retention, "+
			"and check that DB_RETENTION_DSN is not the app DSN",
		e.Schema, e.Partition)
}

// ListUsagePartitions 列出 `usage_logs` 的所有月分区（按上界升序）。
//
// 只识别规范命名的分区 `usage_logs_YYYYMM`：保留任务的删除面必须是可预测的
// 月分区；其它名字（人工建的、DEFAULT 分区）一律不进入候选，避免按上界
// 解析失败后误删。父表不存在时返回明确错误（未跑迁移）。
func ListUsagePartitions(ctx context.Context, db *sql.DB) ([]UsagePartition, error) {
	if db == nil {
		return nil, errors.New("billing: ListUsagePartitions requires a logs database")
	}
	if _, err := usageLogParentSchema(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, c.relname
		  FROM pg_inherits i
		  JOIN pg_class c     ON c.oid = i.inhrelid
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE i.inhparent = to_regclass('usage_logs')`)
	if err != nil {
		return nil, fmt.Errorf("billing: list usage partitions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var parts []UsagePartition
	for rows.Next() {
		var (
			partSchema, name string
		)
		if err := rows.Scan(&partSchema, &name); err != nil {
			return nil, fmt.Errorf("billing: scan usage partition: %w", err)
		}
		from, ok := usagePartitionMonth(name)
		if !ok {
			// 非规范命名：不参与保留删除（见函数注释），但保留在结果外。
			continue
		}
		parts = append(parts, UsagePartition{
			Schema: partSchema,
			Name:   name,
			From:   from,
			To:     from.AddDate(0, 1, 0),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: iterate usage partitions: %w", err)
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].To.Equal(parts[j].To) {
			return parts[i].Name < parts[j].Name
		}
		return parts[i].To.Before(parts[j].To)
	})
	return parts, nil
}

// usagePartitionMonth 从规范分区名解析月初（UTC）。
//
// 直接按名字解析而不是解析 `pg_get_expr(relpartbound)`：后者是 PostgreSQL
// 内部表达式文本，格式随版本/时区设置漂移；而分区名是**本仓库自己冻结**的
// 规范（迁移与预建都按 usage_logs_YYYYMM 创建），名字即契约。
func usagePartitionMonth(name string) (time.Time, bool) {
	const prefix = usageLogParentTable + "_"
	if len(name) != len(prefix)+6 || name[:len(prefix)] != prefix {
		return time.Time{}, false
	}
	for _, r := range name[len(prefix):] {
		if r < '0' || r > '9' {
			return time.Time{}, false
		}
	}
	month, err := time.Parse("200601", name[len(prefix):])
	if err != nil || month.Month() < time.January || month.Month() > time.December {
		return time.Time{}, false
	}
	return month.UTC(), true
}

// selectExpiredPartitions 返回上界 <= cutoff 的分区，按上界最旧优先，最多 limit 个。
//
// 抽成纯函数便于单测覆盖边界（当前月、未来月、恰好等于 cutoff）。
func selectExpiredPartitions(parts []UsagePartition, cutoff time.Time, limit int) []UsagePartition {
	if limit <= 0 {
		return nil
	}
	out := make([]UsagePartition, 0, limit)
	for _, p := range parts {
		// 右开区间：To <= cutoff 表示分区内所有时间戳都 < cutoff。
		if p.To.After(cutoff) {
			continue
		}
		out = append(out, p)
		if len(out) == limit {
			break
		}
	}
	return out
}

// DropExpiredUsagePartitions 删除上界早于保留窗口的月分区，返回被删除的分区名。
//
// 调用方必须传入 retention 角色的连接池（R-W10-01）；函数在 DDL 之前逐个
// 校验当前连接是否拥有目标子分区，权限不足返回
// *PartitionRetentionUnavailableError，绝不尝试用父表属主兜底。
//
// 幂等：重复执行时候选已被删除，列表里不再出现；DROP TABLE IF EXISTS 再兜一层。
func DropExpiredUsagePartitions(ctx context.Context, db *sql.DB, now time.Time,
	retention time.Duration, limit int) ([]string, error) {
	if retention <= 0 {
		// 0 在设置层表示关闭；到这里是编程错误而非「立即全删」。
		// 先校验参数再校验连接：调用方同时传错两者时，先拿到更具体的错误。
		return nil, fmt.Errorf("billing: partition retention must be positive, got %v", retention)
	}
	if db == nil {
		return nil, errors.New("billing: DropExpiredUsagePartitions requires a retention database")
	}
	if limit <= 0 {
		limit = PartitionDropBatchSize
	}
	cutoff := now.UTC().Add(-retention)
	parts, err := ListUsagePartitions(ctx, db)
	if err != nil {
		return nil, err
	}
	candidates := selectExpiredPartitions(parts, cutoff, limit)
	if len(candidates) == 0 {
		return nil, nil
	}

	// 先做所有权预检再执行任何 DDL：一次权限错误就应终止整轮（身份错了，
	// 后续每个分区都会同样失败），而不是逐个撞 permission denied。
	for _, p := range candidates {
		owns, err := usagePartitionOwnedByCurrentUser(ctx, db, p.Schema, p.Name)
		if err != nil {
			return nil, fmt.Errorf("billing: check ownership of %s.%s: %w", p.Schema, p.Name, err)
		}
		if !owns {
			return nil, &PartitionRetentionUnavailableError{Schema: p.Schema, Partition: p.Name}
		}
	}

	dropped := make([]string, 0, len(candidates))
	for _, p := range candidates {
		// 单条 DDL、不在显式事务内：DROP 自身原子，且不与其它语句共享事务
		// （大表 DROP 不应把无关操作拖进同一个长事务）。
		stmt := fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s`,
			quoteSQLIdent(p.Schema), quoteUsagePartitionIdent(p.Name))
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return dropped, fmt.Errorf("billing: drop usage partition %s.%s: %w",
				p.Schema, p.Name, err)
		}
		dropped = append(dropped, p.Name)
	}
	return dropped, nil
}

// usagePartitionOwnedByCurrentUser 报告当前连接是否拥有指定子分区。
//
// 用 pg_has_role(..., 'USAGE')（覆盖直接属主与继承属主）；与 DROP TABLE 的
// 权限检查同口径。父表不存在时返回 false（调用方已通过 ListUsagePartitions
// 确认父表存在，这里只处理竞态）。
func usagePartitionOwnedByCurrentUser(ctx context.Context, db *sql.DB, schema, name string) (bool, error) {
	var owns bool
	err := db.QueryRowContext(ctx, `
		SELECT pg_has_role(current_user, c.relowner, 'USAGE')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2`, schema, name).Scan(&owns)
	if errors.Is(err, sql.ErrNoRows) {
		// 列表与检查之间分区被另一副本删掉：视为「无需删除」，不算权限错误。
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return owns, nil
}

// PartitionDropDeps 是过期分区删除循环的依赖与可调参数。
//
// 零值默认：Now → time.Now；Logger → slog.Default()；Interval →
// PartitionDropInterval；BatchSize → PartitionDropBatchSize。
// RetentionDays <= 0 表示关闭（装配层通常根本不启动循环，这里再兜一层）。
type PartitionDropDeps struct {
	// DB 是 **retention 角色**的日志库连接池（必填）。绝不传 app DSN。
	DB       *sql.DB
	Redis    *redis.Client
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration

	RetentionDays int
	BatchSize     int

	// Drop 是测试接缝（nil 时用生产 DROP SQL）。
	Drop func(ctx context.Context, now time.Time, retention time.Duration, limit int) ([]string, error)
}

type partitionDropWorker struct {
	db            *sql.DB
	redis         *redis.Client
	log           *slog.Logger
	now           func() time.Time
	interval      time.Duration
	retentionDays int
	batchSize     int
	dropFn        func(ctx context.Context, now time.Time, retention time.Duration, limit int) ([]string, error)
}

// RunPartitionDropLoop 运行过期分区删除循环，直到 ctx 取消。
//
// 启动即删一轮（重启后不必等一天），之后按 Interval 周期运行。单轮失败
// 只记录并在下一轮重试；Redis 锁抢不到就跳过本轮（滚动升级期间避免两个
// 副本同时 DROP）。ctx.Done() 优雅退出并返回 nil。
func RunPartitionDropLoop(ctx context.Context, deps PartitionDropDeps) error {
	if deps.DB == nil {
		return errors.New("billing: partition drop requires a retention database")
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
		interval = PartitionDropInterval
	}
	batch := deps.BatchSize
	if batch <= 0 {
		batch = PartitionDropBatchSize
	}
	if deps.RetentionDays <= 0 {
		// 0 = 关闭（冻结语义；不是「立即删除全部分区」）。
		log.Info("log partition drop disabled (retention days <= 0)")
		return nil
	}
	w := &partitionDropWorker{
		db:            deps.DB,
		redis:         deps.Redis,
		log:           log,
		now:           now,
		interval:      interval,
		retentionDays: deps.RetentionDays,
		batchSize:     batch,
	}
	w.dropFn = deps.Drop
	if w.dropFn == nil {
		w.dropFn = func(ctx context.Context, now time.Time, retention time.Duration, limit int) ([]string, error) {
			return DropExpiredUsagePartitions(ctx, w.db, now, retention, limit)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
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

// runOnce 执行一轮删除。Redis 锁的语义与 N4/N9 一致：抢不到跳过，运行时
// 错误只记录并跳过（Redis 是节流不是正确性来源；DROP 本身幂等）。
func (w *partitionDropWorker) runOnce(ctx context.Context) {
	if w.redis != nil {
		token, acquired, err := acquireWorkerLock(ctx, w.redis, partitionDropLockKey, partitionDropLockTTL)
		if err != nil {
			w.log.Warn("log partition drop skipped: redis lock unavailable", "error", err)
			return
		}
		if !acquired {
			return
		}
		defer releaseWorkerLock(ctx, w.redis, partitionDropLockKey, token, w.log)
	}

	start := time.Now()
	status := "succeeded"
	defer func() {
		metrics.ObserveWorkerTask("log_partition_drop", status, time.Since(start))
	}()

	retention := time.Duration(w.retentionDays) * 24 * time.Hour
	dropped, err := w.dropFn(ctx, w.now(), retention, w.batchSize)
	if err != nil {
		// 身份/属主错误是配置问题，不是瞬时故障：warn 一次，下一轮仍按日
		// 检查即可，绝不在同一轮内对每个候选分区重复尝试。
		var permErr *PartitionRetentionUnavailableError
		if errors.As(err, &permErr) {
			status = "skipped"
			w.log.Warn("log partition drop skipped: retention role lacks partition ownership",
				"schema", permErr.Schema, "partition", permErr.Partition, "error", err)
			return
		}
		status = "failed"
		w.log.Error("log partition drop failed; retrying next round", "error", err)
		return
	}
	if len(dropped) > 0 {
		// 条数与分区名（保留期边界的可追溯线索）；不记录行内容。
		w.log.Info("dropped expired usage_logs partitions",
			"count", len(dropped), "partitions", dropped,
			"retention_days", w.retentionDays)
	}
}
