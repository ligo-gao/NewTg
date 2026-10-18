package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// usageLogParentTable 是 usage_logs 分区父表（platform-01 §12）。
const usageLogParentTable = "usage_logs"

// usagePartitionMonths 是 worker 侧每次滚动预建的分区数：当月 + 下月。
//
// 契约（R-12 / platform-04 §5.2）冻结为 2 个月；能跑 DDL 的执行者（migrate
// 凭据）每天调用即可覆盖跨月。**不建 DEFAULT 分区**（裁决 V11）：落入未建
// 月份必须直接失败并由 /readyz 告警，而不是静默进入一个无法按保留期 drop
// 的默认分区。
//
// 【M7 已登记】platform-01 §12 提到可配 `log.partition_lookahead_months`
// 默认 3，与本函数的 2 个月口径不一致；M3 冻结 R-12=2 个月，配置化留后续。
// 生产兜底不依赖 worker DDL，而是日志迁移预建的 12 个月窗口（R-56）。
const usagePartitionMonths = 2

// PartitionDDLUnavailableError 表示当前数据库连接没有执行分区 DDL 的权限（R-56）。
//
// 为什么需要这个类型：生产 worker 以 newtg_app 运行，而 newtg_app 在日志库
// 只有 DML 权限、且不是 usage_logs 的属主；它**永远**不可能执行
// CREATE TABLE ... PARTITION OF。直接尝试 DDL 只会反复得到难以归类的
// permission denied。这里在 DDL 之前显式探测并把「身份不对」与「真正的
// 故障」区分开：
//   - worker 用 errors.As 识别本类型 → 记 warn + 一条 ops 提示（分区必须由
//     migrate 凭据预建/滚动），**不**当作扫描失败；
//   - 其它错误（连接失败、锁超时、父表缺失）仍按 error 处理并重试。
//
// 绝不因此给 newtg_app 授予 schema CREATE 或表属主：运行期应用不应具备 DDL。
type PartitionDDLUnavailableError struct {
	Schema       string // usage_logs 所在 schema
	SchemaCreate bool   // has_schema_privilege(current_user, schema, 'CREATE')
	OwnsParent   bool   // pg_has_role(current_user, relowner, 'USAGE')
}

// Error 实现 error；消息直接告诉运维「用 migrate 凭据预建分区」。
func (e *PartitionDDLUnavailableError) Error() string {
	return fmt.Sprintf(
		"billing: usage_logs partition DDL unavailable: schema %q CREATE=%t, parent ownership=%t; "+
			"runtime role must not run DDL — partitions are pre-created by the logs migration (12-month lookahead), "+
			"extend them with migrate credentials",
		e.Schema, e.SchemaCreate, e.OwnsParent)
}

// EnsureUsagePartitions 创建当月与下月的 usage_logs 月分区（UTC）。
//
// 由 T5 的 worker 在启动时与每 24h 调用一次；本任务只导出函数与类型。
//
// 【执行身份（R-56）】本函数先探测 DDL 权限（schema CREATE + 父表属主）：
//   - 无权 → 返回 *PartitionDDLUnavailableError，**不尝试任何 DDL**；
//   - 有权 → 才执行 CREATE TABLE IF NOT EXISTS ... PARTITION OF ...。
//
// 因此生产 worker（newtg_app）的调用会稳定得到可识别的 warn 路径，而不是
// 每轮 permission denied 的假故障；真正的分区供给由日志迁移的 12 个月预建
// 承担（见 migrations-logs/20261019000000_usage_logs_app_update.up.sql）。
//
// 其余语义：
//   - 分区名 usage_logs_YYYYMM，区间左闭右开 [月初, 次月初)，一律按 UTC
//     计算（调用方传本地时间也先转 UTC，避免月末在非 UTC 时区落到错误月份）；
//   - CREATE TABLE IF NOT EXISTS ... 幂等；
//   - **不创建 DEFAULT 分区**；
//   - 权限探测之外的失败（连接、锁、父表缺失）返回 error。
//
// 幂等的并发面：PostgreSQL 的 CREATE TABLE IF NOT EXISTS 在并发创建同名表
// 时可能抛 duplicate key 而不是安静返回。这里在错误后复核分区是否已真实
// 存在（to_regclass），存在即视为本轮成功——只有「确认存在」才吞掉错误。
func EnsureUsagePartitions(ctx context.Context, logsDB *sql.DB, now time.Time) error {
	if logsDB == nil {
		return errors.New("billing: EnsureUsagePartitions requires a logs database")
	}

	schema, schemaCreate, ownsParent, err := usagePartitionDDLPrivileges(ctx, logsDB)
	if err != nil {
		return fmt.Errorf("billing: probe usage_logs partition privileges: %w", err)
	}
	if !schemaCreate || !ownsParent {
		return &PartitionDDLUnavailableError{
			Schema:       schema,
			SchemaCreate: schemaCreate,
			OwnsParent:   ownsParent,
		}
	}

	start := usageMonthStart(now)
	// N3：DDL 必须与权限探测使用**同一个 schema**。探测解析出父表实际所在
	// schema（to_regclass 走 search_path 的结果），这里把父表与分区名都显式
	// schema 限定，避免执行期 search_path 变化把 DDL 指向别的 schema。
	parent := quoteSQLIdent(schema) + "." + quoteSQLIdent(usageLogParentTable)
	for i := 0; i < usagePartitionMonths; i++ {
		from := start.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		name := usagePartitionName(from)
		partition := quoteSQLIdent(schema) + "." + quoteUsagePartitionIdent(name)
		stmt := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%s) TO (%s)`,
			partition, parent, quoteTimestamptz(from), quoteTimestamptz(to))

		if _, err := logsDB.ExecContext(ctx, stmt); err != nil {
			exists, checkErr := usagePartitionExists(ctx, logsDB, schema, name)
			if checkErr != nil {
				return fmt.Errorf("billing: create usage partition %s.%s: %w (existence check: %v)",
					schema, name, err, checkErr)
			}
			if exists {
				continue
			}
			return fmt.Errorf("billing: create usage partition %s.%s: %w", schema, name, err)
		}
	}
	return nil
}

// usagePartitionDDLPrivileges 探测当前连接能否创建 usage_logs 的分区。
//
// 判据与 PostgreSQL 的 DDL 权限检查一致：
//   - 在父表所在 schema 上有 CREATE（创建新表/分区）；
//   - 对父表有属主权限（pg_has_role(..., 'USAGE') 覆盖直接属主与继承属主）。
//
// 父表不存在时返回明确错误（schema 未迁移），而不是把它伪装成权限问题。
func usagePartitionDDLPrivileges(ctx context.Context, logsDB *sql.DB) (string, bool, bool, error) {
	var (
		schema       string
		schemaCreate bool
		ownsParent   bool
	)
	err := logsDB.QueryRowContext(ctx, `
		SELECT n.nspname,
		       has_schema_privilege(current_user, n.nspname, 'CREATE'),
		       pg_has_role(current_user, c.relowner, 'USAGE')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = to_regclass('usage_logs')`).
		Scan(&schema, &schemaCreate, &ownsParent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, errors.New(
			"usage_logs parent table not found; run the logs migrations first")
	}
	if err != nil {
		return "", false, false, err
	}
	return schema, schemaCreate, ownsParent, nil
}

// usageMonthStart 返回 now 所在 UTC 月的月初（与 date_trunc('month', ...)
// 的 UTC 口径一致）。
func usageMonthStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// usagePartitionName 返回 usage_logs_YYYYMM。
func usagePartitionName(month time.Time) string {
	return usageLogParentTable + "_" + month.UTC().Format("200601")
}

// quoteUsagePartitionIdent 直接返回分区名。
//
// 名字只由常量前缀 + 6 位数字构成（usagePartitionName），不含引号/特殊字符，
// 因此无需通用标识符引用；这里加一个校验，防止未来改动把外部输入带进 DDL。
func quoteUsagePartitionIdent(name string) string {
	for _, r := range name {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			panic("billing: unsafe usage partition identifier " + name)
		}
	}
	return name
}

// quoteSQLIdent 按 SQL 标准引用任意标识符（双引号内的 " 双写）。
//
// schema 名来自数据库元数据，可能包含大写/空格/引号；DDL 是字符串拼接，
// 必须完整引用。分区名本身已由 quoteUsagePartitionIdent 限定字符集。
func quoteSQLIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteTimestamptz 把时间渲染成带 UTC 偏移的 SQL 字面量。
//
// 使用固定格式而不是 %v/默认 String()：DDL 是字符串拼接（分区边界不能走
// 参数占位符），格式必须与 PostgreSQL 的 timestamptz 输入稳定兼容。
func quoteTimestamptz(t time.Time) string {
	return "'" + t.UTC().Format("2006-01-02 15:04:05+00") + "'"
}

// usagePartitionExists 判断分区表是否已在指定 schema 中存在（并发创建的复核）。
func usagePartitionExists(ctx context.Context, logsDB *sql.DB, schema, name string) (bool, error) {
	var exists bool
	if err := logsDB.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relname = $2)`, schema, name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}
