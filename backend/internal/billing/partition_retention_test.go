package billing

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// 本文件验收 W10-N10 的纯逻辑面（不依赖 PG）：
//   - 分区名 → 月界的解析（只认规范命名）；
//   - 过期分区选择（上界 <= cutoff、当前/未来月永不入选、单轮上限）；
//   - 删除入口的 fail-closed 参数校验（nil DB / 非正保留期）；
//   - 循环在 retention 天数 <= 0 时不启动（关闭语义）。

func TestUsagePartitionMonthParsesCanonicalNames(t *testing.T) {
	got, ok := usagePartitionMonth("usage_logs_202602")
	if !ok {
		t.Fatal("canonical name rejected")
	}
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("month = %v, want %v", got, want)
	}
}

func TestUsagePartitionMonthRejectsNonCanonicalNames(t *testing.T) {
	// 人工分区、DEFAULT 分区、大小写/位数错误都不能进入删除候选：删除面必须
	// 只包含本仓库自己按规范创建的月分区。
	for _, name := range []string{
		"usage_logs_20260",     // 位数不足
		"usage_logs_2026001",   // 位数过多
		"usage_logs_202613",    // 非法月份
		"usage_logs_2026_02",   // 分隔符
		"USAGE_LOGS_202602",    // 前缀大小写
		"usage_logs_default",   // DEFAULT 分区
		"usage_logs_202602x",   // 尾随字符
		"other_logs_202602",    // 其它父表
		"usage_logs_202602-01", // 带日期
	} {
		if _, ok := usagePartitionMonth(name); ok {
			t.Fatalf("name %q accepted, want rejected", name)
		}
	}
}

func TestSelectExpiredPartitionsBoundary(t *testing.T) {
	month := func(y int, m time.Month) UsagePartition {
		from := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return UsagePartition{
			Schema: "public",
			Name:   usagePartitionName(from),
			From:   from,
			To:     from.AddDate(0, 1, 0),
		}
	}
	cutoff := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	parts := []UsagePartition{
		month(2026, 6),  // To = 2026-07-01 <= cutoff → 过期
		month(2026, 9),  // To = 2026-10-01 <= cutoff → 过期
		month(2026, 10), // To = 2026-11-01 > cutoff → 保留（当月）
		month(2026, 11), // 未来月 → 保留
	}

	got := selectExpiredPartitions(parts, cutoff, 10)
	if len(got) != 2 {
		t.Fatalf("expired = %d (%v), want 2", len(got), got)
	}
	if got[0].Name != "usage_logs_202606" || got[1].Name != "usage_logs_202609" {
		t.Fatalf("expired order = %v, want oldest first", got)
	}

	// 单轮上限只取最旧的 N 个；积压留给下一轮。
	limited := selectExpiredPartitions(parts, cutoff, 1)
	if len(limited) != 1 || limited[0].Name != "usage_logs_202606" {
		t.Fatalf("limited = %v, want only usage_logs_202606", limited)
	}

	// 恰好等于 cutoff 的上界必须删除（右开区间 [from,to)：To==cutoff 表示
	// 分区内没有 >= cutoff 的行）。
	exact := month(2026, 9) // To 2026-10-01
	if got := selectExpiredPartitions([]UsagePartition{exact}, exact.To, 10); len(got) != 1 {
		t.Fatalf("To == cutoff must be expired, got %v", got)
	}
}

func TestSelectExpiredPartitionsLimitZero(t *testing.T) {
	if got := selectExpiredPartitions(nil, time.Now(), 0); got != nil {
		t.Fatalf("limit=0 returned %v, want nil", got)
	}
}

func TestDropExpiredUsagePartitionsRejectsMissingDB(t *testing.T) {
	if _, err := DropExpiredUsagePartitions(context.Background(), nil, time.Now(), 90*24*time.Hour, 2); err == nil {
		t.Fatal("nil db = nil error, want error")
	}
}

func TestDropExpiredUsagePartitionsRejectsNonPositiveRetention(t *testing.T) {
	// 0 = 关闭，不是「立即删除全部分区」。装配层不启动循环，这里也必须拒绝。
	if _, err := DropExpiredUsagePartitions(context.Background(), nil, time.Now(), 0, 2); err == nil ||
		!strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("retention=0 error = %v, want positive-retention error", err)
	}
}

func TestPartitionDropLoopRequiresRetentionDB(t *testing.T) {
	if err := RunPartitionDropLoop(context.Background(), PartitionDropDeps{}); err == nil {
		t.Fatal("nil retention db = nil error, want error")
	}
}

func TestPartitionDropLoopDisabledRetentionIsNoop(t *testing.T) {
	// DB 存在但 retentionDays<=0：直接返回 nil，不做任何 DROP。
	// 零值 *sql.DB 足够：关闭分支必须在触碰连接池之前返回。
	db := &sql.DB{}
	var called bool
	err := RunPartitionDropLoop(context.Background(), PartitionDropDeps{
		DB:            db,
		RetentionDays: 0,
		Drop: func(context.Context, time.Time, time.Duration, int) ([]string, error) {
			called = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("disabled loop = %v, want nil", err)
	}
	if called {
		t.Fatal("disabled loop invoked drop")
	}
}

func TestPartitionRetentionUnavailableErrorMentionsIdentity(t *testing.T) {
	err := &PartitionRetentionUnavailableError{Schema: "public", Partition: "usage_logs_202501"}
	msg := err.Error()
	if !strings.Contains(msg, "DB_RETENTION_DSN") || !strings.Contains(msg, "OWNER TO newtg_retention") {
		t.Fatalf("error message lacks ops guidance: %s", msg)
	}
}
