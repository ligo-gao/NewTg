package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/metrics"
)

// 本文件是 W10-N11 的审计保留与 IP 字段级擦除（R-W10-09 A；platform-01 §5、
// billing-06 §6.2、security-09 §2.2、G7-R47）。
//
// 【保留口径】
//   - 审计**行**保留 12 个月（`audit.retention_months`，0 = 不删除）。审计是
//     合规追溯面，出问题往往数月后才回查，因此比请求日志（90 天）长得多。
//   - `audit_logs.ip` 只保留 90 天（`audit.ip_erase_days`，0 = 不擦除）：到期
//     `UPDATE ... SET ip = NULL`，**行本身不提前删**（满足 security-09 的
//     ≥180 天下限）。`username` 是写入时的快照，**保留**（管理员仍可看到
//     「谁做的」，即使该用户已被删除）。
//
// 【执行主体 · R-W10-01 A】本任务必须用独立的 `DB_RETENTION_DSN`
// （newtg_retention）执行：日志库 app 角色没有 audit_logs 的 DELETE 与
// 列级 UPDATE(ip)（平台 §12.6 / security-09 §2.11）。装配层在 DSN 缺失时
// fail-closed，绝不回退到 app DSN。
//
// 【用户删除 · R-W10-09 A】用户删除**不做**即时 IP 擦除，也不匿名化
// username 快照：历史审计行按本任务的 90 天/12 个月时间窗处理，隐私政策与
// 数据主体流程负责说明这一保留口径。这样审计保持 append-only 语义（删除
// 用户不触发对审计表的跨库改写），也避免「删除即擦除」造成取证链断裂。
//
// 【分批 + 上限】`DELETE`/`UPDATE` 都按 `created_at` 索引分批（每批 LIMIT），
// 单轮各最多 MaxBatches 批。大 `UPDATE` 会产生大量 WAL/死元组，分批把锁与
// 膨胀限制在有界事务内；积压由下一轮继续（最旧优先）。
//
// 【自审计有界】一轮里无论删/擦了多少批，最多写**一条**审计行（只含计数与
// 时间窗参数），且无变化时不写。这样保留任务本身可追溯，又不会让审计表
// 因「自记录」而无界增长；自记录行没有 IP、created_at=now，不会被本轮规则
// 立即删除或擦除（自引用有界）。审计写入走 app 的 append-only 入口（由
// 装配层注入），retention 角色只需要 SELECT + DELETE + UPDATE(ip)。
//
// 【单副本】沿用既有 worker 的 Redis `SET NX PX` 租约（见 N4/N9）：抢不到
// 跳过本轮；锁 TTL 固定 1 小时，崩溃后接手延迟有界。SQL 条件本身幂等
// （重复执行 affected=0），锁只是节流。

const (
	// DefaultRetentionMonths 是审计行保留期（platform-01 §5：12 个月）。
	DefaultRetentionMonths = 12

	// DefaultIPEraseDays 是审计 IP 的字段级保留期（platform-01 §5：90 天）。
	DefaultIPEraseDays = 90

	// RetentionInterval 是保留任务周期（billing-06 §6.2：每日夜间小批）。
	RetentionInterval = 24 * time.Hour

	// RetentionBatchSize 是单条 DELETE/UPDATE 的最大行数。
	RetentionBatchSize = 1000

	// RetentionMaxBatchesPerPass 是单轮删除与单轮擦除各自的批次数上限
	// （默认每轮各 1 万行）。
	RetentionMaxBatchesPerPass = 10

	// retentionLockKey 是跨副本租约键。
	retentionLockKey = "newtg:audit-retention:lock:tick"
	// retentionLockTTL 只需覆盖一轮（最多各 1 万行）；固定 1 小时让崩溃后
	// 的接手延迟有界。
	retentionLockTTL = time.Hour
)

// ActionAuditRetention 是保留任务的自审计动作名，归 operation 类。
//
// 只记录计数与生效窗口（deleted_rows / erased_ips / retention_months /
// ip_erase_days），绝不记录被删除或擦除的行内容。
const ActionAuditRetention = "audit.retention"

// DeleteAuditBefore 删除 `created_at < cutoff` 的审计行，单批最多 limit 行。
//
// 必须由 retention 角色连接执行；应用角色的 DELETE 被数据库拒绝（这是
// 设计要的 fail-closed）。返回本批实际删除行数（affected）。
func DeleteAuditBefore(ctx context.Context, db *sql.DB, cutoff time.Time, limit int) (int64, error) {
	if db == nil {
		return 0, errors.New("audit: DeleteAuditBefore requires a retention database")
	}
	if limit <= 0 {
		limit = RetentionBatchSize
	}
	// 子查询按 created_at 索引取最旧的一批，避免 `DELETE ... WHERE created_at`
	// 在一次语句里持锁扫全表；LIMIT 在有界事务内完成。
	res, err := db.ExecContext(ctx, `
		DELETE FROM audit_logs
		 WHERE id IN (
			SELECT id FROM audit_logs
			 WHERE created_at < $1
			 ORDER BY created_at, id
			 LIMIT $2)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("audit: delete expired audit rows: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit: delete expired audit rows: rows affected: %w", err)
	}
	return n, nil
}

// EraseAuditIPBefore 把 `created_at < cutoff` 且 ip 非空的审计行 ip 置 NULL，
// 单批最多 limit 行。行本身保留，其它列不变（append-only 的字段级例外）。
func EraseAuditIPBefore(ctx context.Context, db *sql.DB, cutoff time.Time, limit int) (int64, error) {
	if db == nil {
		return 0, errors.New("audit: EraseAuditIPBefore requires a retention database")
	}
	if limit <= 0 {
		limit = RetentionBatchSize
	}
	// 只 UPDATE ip 一列：retention 角色只有列级 UPDATE(ip) 授权，任何整行
	// UPDATE 都会触发权限错误（这是最小权限的回归保护）。
	res, err := db.ExecContext(ctx, `
		UPDATE audit_logs
		   SET ip = NULL
		 WHERE id IN (
			SELECT id FROM audit_logs
			 WHERE created_at < $1 AND ip IS NOT NULL
			 ORDER BY created_at, id
			 LIMIT $2)`, cutoff.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("audit: erase expired audit ip: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit: erase expired audit ip: rows affected: %w", err)
	}
	return n, nil
}

// RetentionDeps 是保留循环的依赖与可调参数。零值默认：
// Now → time.Now；Logger → slog.Default()；Interval → RetentionInterval；
// BatchSize → RetentionBatchSize；MaxBatches → RetentionMaxBatchesPerPass。
//
// RetentionMonths / IPEraseDays <= 0 表示对应分支关闭（冻结的 0 语义）。
type RetentionDeps struct {
	// DB 是 **retention 角色**的日志库连接池（必填）。
	DB       *sql.DB
	Redis    *redis.Client
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration

	RetentionMonths int
	IPEraseDays     int
	BatchSize       int
	MaxBatches      int

	// Audit 是自审计写入接缝（可选；装配层注入 audit.Logger 的 append-only
	// 入口）。为 nil 时不写自审计（测试/无审计装配）。
	Audit func(ctx context.Context, detail json.RawMessage) error

	// Delete / EraseIP 是测试接缝（nil 时用生产 SQL）。
	Delete  func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	EraseIP func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

type retentionWorker struct {
	db              *sql.DB
	redis           *redis.Client
	log             *slog.Logger
	now             func() time.Time
	interval        time.Duration
	retentionMonths int
	ipEraseDays     int
	batchSize       int
	maxBatches      int
	audit           func(ctx context.Context, detail json.RawMessage) error
	deleteFn        func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
	eraseIPFn       func(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

// RunRetentionLoop 运行审计保留循环，直到 ctx 取消。
//
// 启动即跑一轮（重启后不必等一天），之后按 Interval 周期运行。两个分支
// 独立：删除行失败不阻止 IP 擦除，反之亦然；单轮失败只记录并在下一轮重试。
func RunRetentionLoop(ctx context.Context, deps RetentionDeps) error {
	if deps.DB == nil {
		return errors.New("audit: retention loop requires a retention database")
	}
	if deps.RetentionMonths <= 0 && deps.IPEraseDays <= 0 {
		// 两个分支都关闭：不启动空转循环（显式 0 的冻结语义）。
		return nil
	}
	w := &retentionWorker{
		db:              deps.DB,
		redis:           deps.Redis,
		log:             deps.Logger,
		now:             deps.Now,
		interval:        deps.Interval,
		retentionMonths: deps.RetentionMonths,
		ipEraseDays:     deps.IPEraseDays,
		batchSize:       deps.BatchSize,
		maxBatches:      deps.MaxBatches,
		audit:           deps.Audit,
		deleteFn:        deps.Delete,
		eraseIPFn:       deps.EraseIP,
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.interval <= 0 {
		w.interval = RetentionInterval
	}
	if w.batchSize <= 0 {
		w.batchSize = RetentionBatchSize
	}
	if w.maxBatches <= 0 {
		w.maxBatches = RetentionMaxBatchesPerPass
	}
	if w.deleteFn == nil {
		w.deleteFn = func(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
			return DeleteAuditBefore(ctx, w.db, cutoff, limit)
		}
	}
	if w.eraseIPFn == nil {
		w.eraseIPFn = func(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
			return EraseAuditIPBefore(ctx, w.db, cutoff, limit)
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

// runOnce 执行一轮删除 + 擦除，并至多写一条自审计。
func (w *retentionWorker) runOnce(ctx context.Context) {
	if w.redis != nil {
		token, acquired, err := acquireRetentionLock(ctx, w.redis, retentionLockKey, retentionLockTTL)
		if err != nil {
			w.log.Warn("audit retention skipped: redis lock unavailable", "error", err)
			return
		}
		if !acquired {
			return
		}
		defer releaseRetentionLock(ctx, w.redis, retentionLockKey, token, w.log)
	}

	start := time.Now()
	status := "succeeded"
	var deleted, erased int64
	defer func() {
		metrics.ObserveWorkerTask("retention", status, time.Since(start))
	}()

	now := w.now().UTC()
	// 两个分支**互相独立**：删除行失败不应阻止 IP 擦除（90 天时效更短、
	// 隐私优先级更高），反之亦然。错误分别记录，status 只要有一个失败即 failed；
	// 已经完成的部分计数仍写入自审计（不掩盖成功的那一半）。
	var deleteErr, eraseErr error
	if w.retentionMonths > 0 {
		// 用日历月（AddDate）而不是 30 天倍数：设计口径是「12 个月」，
		// 2 月/闰年不应让边界漂移。
		cutoff := now.AddDate(0, -w.retentionMonths, 0)
		n, err := w.runBatches(ctx, w.deleteFn, cutoff)
		if err != nil {
			deleteErr = err
			w.log.Error("audit row retention failed; retrying next round",
				"cutoff", cutoff, "error", err)
		}
		deleted = n
	}
	if w.ipEraseDays > 0 {
		cutoff := now.Add(-time.Duration(w.ipEraseDays) * 24 * time.Hour)
		n, err := w.runBatches(ctx, w.eraseIPFn, cutoff)
		if err != nil {
			eraseErr = err
			w.log.Error("audit ip erasure failed; retrying next round",
				"cutoff", cutoff, "error", err)
		}
		erased = n
	}
	if deleteErr != nil || eraseErr != nil {
		status = "failed"
	}

	if deleted > 0 || erased > 0 {
		w.log.Info("audit retention pass completed",
			"deleted_rows", deleted, "erased_ips", erased,
			"retention_months", w.retentionMonths, "ip_erase_days", w.ipEraseDays)
		w.recordSelfAudit(ctx, deleted, erased)
	}
}

// runBatches 反复调用单批操作直到不足一批或达到单轮上限。
func (w *retentionWorker) runBatches(ctx context.Context,
	fn func(ctx context.Context, cutoff time.Time, limit int) (int64, error),
	cutoff time.Time) (int64, error) {
	var total int64
	for i := 0; i < w.maxBatches; i++ {
		n, err := fn(ctx, cutoff, w.batchSize)
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(w.batchSize) {
			break
		}
	}
	return total, nil
}

// recordSelfAudit 写一条有界的保留自审计（每轮最多一条）。
//
// 失败只 warn：审计保留不能因为审计写入失败而停摆，下一轮仍会尝试；
// 计数已进结构化日志，运维仍可对账。
func (w *retentionWorker) recordSelfAudit(ctx context.Context, deleted, erased int64) {
	if w.audit == nil {
		return
	}
	detail, err := json.Marshal(map[string]any{
		"event":            "audit_retention",
		"deleted_rows":     deleted,
		"erased_ips":       erased,
		"retention_months": w.retentionMonths,
		"ip_erase_days":    w.ipEraseDays,
	})
	if err != nil {
		w.log.Warn("audit retention: marshal self-audit detail failed", "error", err)
		return
	}
	if err := w.audit(ctx, detail); err != nil {
		w.log.Warn("audit retention: write self-audit failed", "error", err)
	}
}

// acquireRetentionLock / releaseRetentionLock 与 N4/N9 的 Redis 租约同范式
// （SET NX PX + compare-and-delete Lua）。audit 包不能导入 billing 的未导出
// 助手，故在此保留一份等价实现；语义必须保持一致：抢不到跳过，释放只删
// 自己持有的令牌。
func acquireRetentionLock(ctx context.Context, rdb *redis.Client, key string, ttl time.Duration) (string, bool, error) {
	token, err := newRetentionLockToken()
	if err != nil {
		return "", false, err
	}
	ok, err := rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return "", false, err
	}
	return token, ok, nil
}

func releaseRetentionLock(ctx context.Context, rdb *redis.Client, key, token string, log *slog.Logger) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := releaseRetentionLockScript.Run(releaseCtx, rdb, []string{key}, token).Err(); err != nil {
		log.Warn("audit retention: release redis lock failed; ttl will reclaim it", "error", err)
	}
}

var releaseRetentionLockScript = redis.NewScript(`
if redis.call('get', KEYS[1]) == ARGV[1] then
  return redis.call('del', KEYS[1])
end
return 0`)

func newRetentionLockToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
