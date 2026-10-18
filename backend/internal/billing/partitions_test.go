package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPartitionBounds 验证分区名与区间按 UTC 的 date_trunc('month') 口径计算。
func TestPartitionBounds(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		wantName string
		wantFrom string
	}{
		{
			name:     "月中",
			now:      time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC),
			wantName: "usage_logs_202607",
			wantFrom: "2026-07-01 00:00:00+00",
		},
		{
			name: "非 UTC 时区的月初归属 UTC 上个月",
			// 2026-07-01 00:30 +08:00 == 2026-06-30 16:30 UTC。
			now:      time.Date(2026, 7, 1, 0, 30, 0, 0, time.FixedZone("CST", 8*3600)),
			wantName: "usage_logs_202606",
			wantFrom: "2026-06-01 00:00:00+00",
		},
		{
			name:     "年末跨年",
			now:      time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			wantName: "usage_logs_202612",
			wantFrom: "2026-12-01 00:00:00+00",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start := usageMonthStart(tc.now)
			if got := usagePartitionName(start); got != tc.wantName {
				t.Fatalf("partition name = %q, want %q", got, tc.wantName)
			}
			if got := quoteTimestamptz(start); got != "'"+tc.wantFrom+"'" {
				t.Fatalf("from bound = %s, want '%s'", got, tc.wantFrom)
			}
			next := start.AddDate(0, 1, 0)
			if got := usagePartitionName(next); got == tc.wantName {
				t.Fatalf("next month name = %s, must differ", got)
			}
		})
	}
}

// TestPartitionEnsureRequiresDB 验证未装配日志库时返回 error（不 panic）。
func TestPartitionEnsureRequiresDB(t *testing.T) {
	if err := EnsureUsagePartitions(context.Background(), nil, time.Now()); err == nil {
		t.Fatal("EnsureUsagePartitions(nil) = nil, want error")
	}
}

// TestPartitionDDLUnavailableError 验证 R-56 的 typed error 可被 errors.As 识别，
// 且消息包含「用 migrate 凭据扩窗」的运维指引（worker 依赖该语义分流 warn）。
func TestPartitionDDLUnavailableError(t *testing.T) {
	err := &PartitionDDLUnavailableError{Schema: "public", SchemaCreate: false, OwnsParent: false}
	var target *PartitionDDLUnavailableError
	if !errors.As(err, &target) {
		t.Fatal("errors.As(*PartitionDDLUnavailableError) = false")
	}
	msg := err.Error()
	if !strings.Contains(msg, "migrate credentials") {
		t.Fatalf("错误信息缺少运维指引: %s", msg)
	}
	if !strings.Contains(msg, "CREATE=false") || !strings.Contains(msg, "ownership=false") {
		t.Fatalf("错误信息缺少探测结果: %s", msg)
	}
}

// TestPartitionIdentifierGuard 验证分区名只允许安全字符（DDL 是字符串拼接），
// 且 schema 标识符会被完整引用（N3：DDL 必须与探测 schema 一致）。
func TestPartitionIdentifierGuard(t *testing.T) {
	if got := quoteUsagePartitionIdent("usage_logs_202607"); got != "usage_logs_202607" {
		t.Fatalf("ident = %q", got)
	}
	if got := quoteSQLIdent("public"); got != `"public"` {
		t.Fatalf("quoteSQLIdent(public) = %s", got)
	}
	if got := quoteSQLIdent(`weird"schema`); got != `"weird""schema"` {
		t.Fatalf("quoteSQLIdent 未双写引号: %s", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("unsafe identifier must panic")
		}
	}()
	_ = quoteUsagePartitionIdent("usage_logs; DROP TABLE users")
}
