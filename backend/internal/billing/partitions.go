package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// usageLogParentTable 是 usage_logs 分区父表（platform-01 §12）。
const usageLogParentTable = "usage_logs"

// usagePartitionMonths 是每次滚动预建的分区数：当月 + 下月。
//
// 契约（R-12 / platform-04 §5.2）冻结为 2 个月；worker 每天与每次启动调用，
// 因此只要进程每天至少存活一次，写入永远不会落到未建月份。**不建 DEFAULT
// 分区**（裁决 V11）：落入未建月份必须直接失败并由 /readyz 告警，而不是
// 静默进入一个无法按保留期 drop 的默认分区。
const usagePartitionMonths = 2

// EnsureUsagePartitions 创建当月与下月的 usage_logs 月分区（UTC）。
//
// 由 T5 的 worker 在启动时与每 24h 调用一次；本任务只导出函数，不接线。
// 语义：
//   - 分区名 usage_logs_YYYYMM，区间左闭右开 [月初, 次月初)，一律按 UTC
//     计算（调用方传本地时间也先转 UTC，避免月末在非 UTC 时区落到错误月份）；
//   - CREATE TABLE IF NOT EXISTS ... PARTITION OF ... 幂等；
//   - **不创建 DEFAULT 分区**；
//   - 权限/锁失败返回 error（绝不把「建分区失败」当成「已存在」）。
//
// 幂等的并发面：PostgreSQL 的 CREATE TABLE IF NOT EXISTS 在并发创建同名表
// 时可能抛 duplicate key（pg_type/pg_class 唯一索引）而不是安静返回。这里在
// 错误后复核分区是否已真实存在（to_regclass），存在即视为本轮成功——只有
// 「确认存在」才吞掉错误，其余错误（权限、锁超时、父表缺失）原样上抛。
func EnsureUsagePartitions(ctx context.Context, logsDB *sql.DB, now time.Time) error {
	if logsDB == nil {
		return errors.New("billing: EnsureUsagePartitions requires a logs database")
	}

	start := usageMonthStart(now)
	for i := 0; i < usagePartitionMonths; i++ {
		from := start.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		name := usagePartitionName(from)
		stmt := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM (%s) TO (%s)`,
			quoteUsagePartitionIdent(name), usageLogParentTable,
			quoteTimestamptz(from), quoteTimestamptz(to))

		if _, err := logsDB.ExecContext(ctx, stmt); err != nil {
			exists, checkErr := usagePartitionExists(ctx, logsDB, name)
			if checkErr != nil {
				return fmt.Errorf("billing: create usage partition %s: %w (existence check: %v)",
					name, err, checkErr)
			}
			if exists {
				continue
			}
			return fmt.Errorf("billing: create usage partition %s: %w", name, err)
		}
	}
	return nil
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

// quoteTimestamptz 把时间渲染成带 UTC 偏移的 SQL 字面量。
//
// 使用固定格式而不是 %v/默认 String()：DDL 是字符串拼接（分区边界不能走
// 参数占位符），格式必须与 PostgreSQL 的 timestamptz 输入稳定兼容。
func quoteTimestamptz(t time.Time) string {
	return "'" + t.UTC().Format("2006-01-02 15:04:05+00") + "'"
}

// usagePartitionExists 判断分区表是否已存在（并发创建的复核）。
func usagePartitionExists(ctx context.Context, logsDB *sql.DB, name string) (bool, error) {
	var exists bool
	if err := logsDB.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}
