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

// DefaultPartitionLookaheadMonths 是分区预建的默认 lookahead（R-W10-04 A）：
// 当月 + 下 3 个月。与 platform-04 §5.2 / billing-06 §6.2 的
// `log.partition_lookahead_months` 默认 3 对齐。
//
// 为什么从 2 改成 3：迁移预建窗口是 12 个月（R-56），而 worker 滚动预建供
// 月边界之外再留 3 个月的缓冲；2 个月在下游迁移延迟时会逼近断档。M3 冻结的
// R-12=2 已被 R-W10-04 A 取代，这里保留常量作为「设置缺失/非法」时的回退。
//
// `0` 表示只保证当月（关闭滚动预建），由调用方显式传入。
const DefaultPartitionLookaheadMonths = 3

// usagePartitionMonths 是旧 R-12 常量名，保留为默认 lookahead 的别名，
// 避免既有调用点/文档引用失效。
//
// Deprecated: 使用 DefaultPartitionLookaheadMonths。
const usagePartitionMonths = DefaultPartitionLookaheadMonths

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

// EnsureUsagePartitions 创建当月与后续默认 lookahead 个月份的 usage_logs 月分区
// （UTC）。等价于 EnsureUsagePartitionsWithLookahead(..., DefaultPartitionLookaheadMonths)；
// 保留旧签名给不关心配置的调用点（测试、迁移前置检查）。
func EnsureUsagePartitions(ctx context.Context, logsDB *sql.DB, now time.Time) error {
	return EnsureUsagePartitionsWithLookahead(ctx, logsDB, now, DefaultPartitionLookaheadMonths)
}

// EnsureUsagePartitionsWithLookahead 创建 [当月, 当月+lookaheadMonths] 的
// usage_logs 月分区（UTC）。lookaheadMonths=0 表示只保证当月。
//
// 由 worker 在启动时与每 24h 调用一次；本任务只导出函数与类型。
//
// 【执行顺序（R-56 运维友好）】
//  1. 只读解析父表 schema 并检查目标月份是否都已存在；
//  2. 全部存在 → 不执行任何 CREATE（不产生「DDL 权限不可用」告警），但**仍**
//     对账分区属主：属主已是 newtg_retention 时纯 no-op；否则在有能力时转移，
//     无能力时返回 *PartitionOwnershipUnavailableError（I-1/I-2 升级路径）；
//  3. 缺任一分区 → 才探测 DDL 权限（schema CREATE + 父表属主）：
//     - 无权 → *PartitionDDLUnavailableError（worker 记 warn）；
//     - 有权 → 执行 CREATE TABLE IF NOT EXISTS ... PARTITION OF ...，随后同样
//     做属主对账。
//
// 因此生产 worker（newtg_app）在迁移预建窗口内是**静默 no-op**，只有窗口
// 真正耗尽时才得到可识别的 warn；真正的分区供给由日志迁移的 12 个月预建
// 承担（见 migrations-logs/20261019000001_usage_logs_app_update_v2.up.sql）。
//
// 【分区所有权（W10-N10 / R-W10-01 A）】所有目标月分区（无论本轮是否新建）
// 都要对账到 newtg_retention。retention 角色只拥有**子分区**（DROP TABLE
// 子分区即隐式 detach），不拥有父表——最小权限，不能整表 DROP。无法转移时
// 返回可识别的 error（见 PartitionOwnershipUnavailableError），下一轮重试。
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
// 存在（按 schema+name 查 pg_class），存在即视为本轮成功。
func EnsureUsagePartitionsWithLookahead(ctx context.Context, logsDB *sql.DB, now time.Time,
	lookaheadMonths int) error {
	if logsDB == nil {
		return errors.New("billing: EnsureUsagePartitions requires a logs database")
	}
	if lookaheadMonths < 0 {
		lookaheadMonths = 0
	}
	// 目标月份数 = 当月 + lookahead。0 仍是 1 个月（当月必须存在）。
	monthCount := lookaheadMonths + 1

	// 只读解析父表 schema：后续存在性检查与 DDL 都必须落在同一个 schema
	// （N3），避免执行期 search_path 与探测结果不一致。
	schema, err := usageLogParentSchema(ctx, logsDB)
	if err != nil {
		return fmt.Errorf("billing: resolve usage_logs schema: %w", err)
	}

	start := usageMonthStart(now)
	missing := false
	for i := 0; i < monthCount; i++ {
		name := usagePartitionName(start.AddDate(0, i, 0))
		exists, err := usagePartitionExists(ctx, logsDB, schema, name)
		if err != nil {
			return fmt.Errorf("billing: check usage partition %s.%s: %w", schema, name, err)
		}
		if !exists {
			missing = true
		}
	}
	if !missing {
		// 分区窗口已由迁移预建：无需 DDL，但**仍要做属主对账**（I-1）。
		// 「分区已存在」不等于「保留角色能删」——历史卷可能由 migrate/admin
		// 持有分区；只在对账时转移属主，避免 N10 永久 must be owner。
		if err := reconcileUsagePartitionOwnership(ctx, logsDB, schema, start, monthCount); err != nil {
			return err
		}
		return nil
	}

	schemaCreate, ownsParent, err := usagePartitionDDLPrivileges(ctx, logsDB, schema)
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

	// DDL 必须与权限探测使用**同一个 schema**。父表与分区名都显式 schema
	// 限定，避免执行期 search_path 变化把 DDL 指向别的 schema（N3）。
	parent := quoteSQLIdent(schema) + "." + quoteSQLIdent(usageLogParentTable)
	for i := 0; i < monthCount; i++ {
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
	// 创建完成后统一做属主对账（与「本轮是否新建」解耦）：已有分区同样要
	// 确认属主；无法转移时返回可识别的 error（worker 记 error/warn 而非
	// 静默），绝不把「预建成功」当成「保留任务可删」。
	if err := reconcileUsagePartitionOwnership(ctx, logsDB, schema, start, monthCount); err != nil {
		return err
	}
	return nil
}

// retentionRoleName 是保留/维护主体的角色名（R-W10-01 A）。
const retentionRoleName = "newtg_retention"

// PartitionOwnershipUnavailableError 表示目标分区存在，但无法把属主转给
// newtg_retention（当前身份不是属主/不是 retention 成员，或 retention 在
// schema 上没有 CREATE）。
//
// 这是**需要运维动作**的配置错误（I-2 升级路径）：不静默、也不在每轮对每个
// 分区反复撞权限。worker 会按 error 记录到日志/指标，直到有人执行
// deploy/scripts/retention-upgrade.sql（或重跑 initdb 的 20-databases.sql）。
type PartitionOwnershipUnavailableError struct {
	Schema          string
	Partition       string
	Owner           string
	CanSetRole      bool // 当前用户能否 SET ROLE 到 newtg_retention
	TargetCanCreate bool // newtg_retention 是否有 schema CREATE（ALTER OWNER 前置）
}

func (e *PartitionOwnershipUnavailableError) Error() string {
	return fmt.Sprintf(
		"billing: cannot transfer usage partition %s.%s (owner=%s) to %s: "+
			"can_set_role=%t target_schema_create=%t; "+
			"run deploy/scripts/retention-upgrade.sql as the bootstrap superuser "+
			"(grants USAGE/CREATE and re-owns partitions) or re-run postgres-init/20-databases.sql",
		e.Schema, e.Partition, e.Owner, retentionRoleName,
		e.CanSetRole, e.TargetCanCreate)
}

// reconcileUsagePartitionOwnership 把目标月份分区的属主对账为 newtg_retention。
//
// 与「本轮是否新建分区」完全解耦（I-1）：即使所有目标分区都已存在，也检查
// 属主；属主已经是 retention 时是纯 no-op（生产 app 身份因此不产生噪声）。
//
// 角色不存在时直接 no-op：角色由 deploy/postgres-init/10-roles.sql 创建，
// 单机/测试库可能没有。需要转移而当前身份无能力时返回
// *PartitionOwnershipUnavailableError（可告警、可运维），不是静默跳过。
func reconcileUsagePartitionOwnership(ctx context.Context, logsDB *sql.DB, schema string,
	start time.Time, monthCount int) error {
	var (
		roleExists      bool
		canSetRole      bool
		targetCanCreate bool
	)
	err := logsDB.QueryRowContext(ctx, `
		SELECT TRUE,
		       pg_has_role(current_user, r.oid, 'USAGE'),
		       has_schema_privilege(r.rolname, $1, 'CREATE')
		  FROM pg_roles r
		 WHERE r.rolname = $2`, schema, retentionRoleName).
		Scan(&roleExists, &canSetRole, &targetCanCreate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("billing: probe retention role: %w", err)
	}

	for i := 0; i < monthCount; i++ {
		name := usagePartitionName(start.AddDate(0, i, 0))
		owner, err := usagePartitionOwner(ctx, logsDB, schema, name)
		if err != nil {
			return fmt.Errorf("billing: read owner of %s.%s: %w", schema, name, err)
		}
		if owner == retentionRoleName {
			continue
		}
		if !canSetRole || !targetCanCreate {
			return &PartitionOwnershipUnavailableError{
				Schema:          schema,
				Partition:       name,
				Owner:           owner,
				CanSetRole:      canSetRole,
				TargetCanCreate: targetCanCreate,
			}
		}
		stmt := fmt.Sprintf(`ALTER TABLE %s.%s OWNER TO %s`,
			quoteSQLIdent(schema), quoteUsagePartitionIdent(name), quoteSQLIdent(retentionRoleName))
		if _, err := logsDB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("billing: alter %s.%s owner: %w", schema, name, err)
		}
	}
	return nil
}

// usagePartitionOwner 返回分区属主角色名；分区不存在时返回空串（列表与
// 对账之间的并发删除视为「无需处理」）。
func usagePartitionOwner(ctx context.Context, db *sql.DB, schema, name string) (string, error) {
	var owner string
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_userbyid(c.relowner)
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2`, schema, name).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return owner, nil
}

// usageLogParentSchema 只读解析 usage_logs 父表所在 schema。
//
// 走 to_regclass 的 search_path 解析，与未限定表名的调用方视角一致；随后
// 所有存在性检查与 DDL 都用该 schema 显式限定（N3）。父表不存在时返回
// 明确错误（schema 未迁移），而不是伪装成权限问题。
func usageLogParentSchema(ctx context.Context, logsDB *sql.DB) (string, error) {
	var schema string
	err := logsDB.QueryRowContext(ctx, `
		SELECT n.nspname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = to_regclass('usage_logs')`).Scan(&schema)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("usage_logs parent table not found; run the logs migrations first")
	}
	if err != nil {
		return "", err
	}
	return schema, nil
}

// usagePartitionDDLPrivileges 探测当前连接能否在指定 schema 创建分区。
//
// 判据与 PostgreSQL 的 DDL 权限检查一致：
//   - 在父表所在 schema 上有 CREATE（创建新表/分区）；
//   - 对父表有属主权限（pg_has_role(..., 'USAGE') 覆盖直接属主与继承属主）。
func usagePartitionDDLPrivileges(ctx context.Context, logsDB *sql.DB, schema string) (bool, bool, error) {
	var (
		schemaCreate bool
		ownsParent   bool
	)
	err := logsDB.QueryRowContext(ctx, `
		SELECT has_schema_privilege(current_user, $1, 'CREATE'),
		       pg_has_role(current_user, c.relowner, 'USAGE')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = $2`, schema, usageLogParentTable).
		Scan(&schemaCreate, &ownsParent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, errors.New("usage_logs parent table not found; run the logs migrations first")
	}
	if err != nil {
		return false, false, err
	}
	return schemaCreate, ownsParent, nil
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
