//go:build integration

package store

// W16-1：渠道指标（F-06-11）的日志库集成验收。
//
// 证明四件只有真 PostgreSQL + 真日志库才能证明的事：
//  1. 24h/7d 桶数恰为 24/168，且整点对齐、跨渠道不串；
//  2. 成功率口径 = success / (success + error + refused)，refunded 不入分母；
//  3. 无样本渠道返回零值计数 + null 比率（「—」而不是 0）；
//  4. usage_logs(channel_id, created_at) 索引存在，且迁移 down/up 可往返。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w16a?sslmode=disable' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_w16a_logs?sslmode=disable' \
//	  go test -tags=integration -p 1 ./internal/store -run TestChannelMetricsRepo -v

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func openW16LogsDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过 W16-1 日志库集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping logs db: %v", err)
	}
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass('public.usage_logs') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check usage_logs: %v", err)
	}
	if !exists {
		t.Skip("日志库缺少 usage_logs，请先执行 migrations-logs")
	}
	return db
}

// ensureUsageLogsPartition 保证目标月份分区存在（初始化迁移只预建当前月及
// 未来月份；测试若在月初回看 3 天会落到上个月）。测试专属，生产分区由迁移建。
func ensureUsageLogsPartition(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()
	month := time.Date(at.UTC().Year(), at.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	next := month.AddDate(0, 1, 0)
	name := "usage_logs_" + month.Format("200601")
	if _, err := db.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF usage_logs FOR VALUES FROM ('%s') TO ('%s')`,
		name, month.Format(time.RFC3339), next.Format(time.RFC3339))); err != nil {
		t.Fatalf("ensure partition %s: %v", name, err)
	}
}

// insertW16UsageLog 写一条 usage_logs 测试行。latency/firstToken 为 nil 时写 NULL。
func insertW16UsageLog(
	t *testing.T, db *sql.DB,
	channelID int64, at time.Time, status int16, latencyMS, firstTokenMS *int, cost int64,
) {
	t.Helper()
	ensureUsageLogsPartition(t, db, at)
	if _, err := db.Exec(`
		INSERT INTO usage_logs
			(request_id, user_id, channel_id, model, status, latency_ms, first_token_ms, cost, created_at)
		VALUES ($1, $2, $3, 'w16-test-model', $4, $5, $6, $7, $8)`,
		uuid.NewString(), channelID, channelID, status, latencyMS, firstTokenMS, cost, at.UTC(),
	); err != nil {
		t.Fatalf("insert usage_log: %v", err)
	}
}

func intPtr(v int) *int { return &v }

func cleanupW16UsageLogs(t *testing.T, db *sql.DB, channelIDs ...int64) {
	t.Helper()
	for _, id := range channelIDs {
		if _, err := db.Exec(`DELETE FROM usage_logs WHERE channel_id = $1`, id); err != nil {
			t.Errorf("cleanup channel %d: %v", id, err)
		}
	}
}

func findBucket(t *testing.T, points []ChannelMetricsPoint, at time.Time) ChannelMetricsPoint {
	t.Helper()
	want := at.UTC().Truncate(time.Hour)
	for _, p := range points {
		if p.BucketStart.Equal(want) {
			return p
		}
	}
	t.Fatalf("bucket %s not found in %d points", want, len(points))
	return ChannelMetricsPoint{}
}

func TestChannelMetricsRepoIntegration(t *testing.T) {
	db := openW16LogsDB(t)
	ctx := context.Background()
	repo := NewChannelMetricsRepo(db)
	now := time.Now().UTC()

	// 每个用例用独立渠道 id，避免并发/残留数据串号。
	base := now.UnixNano() % 100_000_000
	chA, chB, chC, chD := base+1_000_000, base+2_000_000, base+3_000_000, base+4_000_000
	t.Cleanup(func() { cleanupW16UsageLogs(t, db, chA, chB, chC, chD) })

	// A：3 条（2 success + 1 error），延迟 100/300/500，首字 40/60/null，cost 10/20/30。
	insertW16UsageLog(t, db, chA, now.Add(-1*time.Hour), 1, intPtr(100), intPtr(40), 10)
	insertW16UsageLog(t, db, chA, now.Add(-2*time.Hour), 1, intPtr(300), intPtr(60), 20)
	insertW16UsageLog(t, db, chA, now.Add(-3*time.Hour), 2, intPtr(500), nil, 30)
	// B：只有 3 天前的一条；24h 看不到，7d 看得到。
	insertW16UsageLog(t, db, chB, now.Add(-3*24*time.Hour), 1, intPtr(200), nil, 7)
	// D：success/error/refused/refunded 各一条，固定成功率口径。
	insertW16UsageLog(t, db, chD, now.Add(-1*time.Hour), 1, intPtr(100), nil, 1)
	insertW16UsageLog(t, db, chD, now.Add(-1*time.Hour), 2, intPtr(100), nil, 1)
	insertW16UsageLog(t, db, chD, now.Add(-1*time.Hour), 0, intPtr(100), nil, 1)
	insertW16UsageLog(t, db, chD, now.Add(-1*time.Hour), 3, intPtr(100), nil, 1)

	t.Run("24h_shape_and_values", func(t *testing.T) {
		m, err := repo.Get(ctx, chA, ChannelMetricsWindow24h, now)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(m.Points) != 24 {
			t.Fatalf("points = %d, want 24", len(m.Points))
		}
		s := m.Summary
		if s.RequestCount != 3 || s.SuccessCount != 2 || s.FailureCount != 1 {
			t.Fatalf("counts = %+v, want requests=3 success=2 failure=1", s)
		}
		if s.SuccessRate == nil || !closeTo(*s.SuccessRate, 2.0/3.0) {
			t.Fatalf("success_rate = %v, want 2/3", s.SuccessRate)
		}
		if s.AvgLatencyMS == nil || !closeTo(*s.AvgLatencyMS, 300) {
			t.Fatalf("avg_latency_ms = %v, want 300", s.AvgLatencyMS)
		}
		if s.AvgFirstTokenMS == nil || !closeTo(*s.AvgFirstTokenMS, 50) {
			t.Fatalf("avg_first_token_ms = %v, want 50", s.AvgFirstTokenMS)
		}
		if s.AvgCost == nil || !closeTo(*s.AvgCost, 20) {
			t.Fatalf("avg_cost = %v, want 20", s.AvgCost)
		}
		if s.CostTotal != 60 || s.UsedCredits != 60 || s.UsedCredits24h != 60 || s.RequestCount24h != 3 {
			t.Fatalf("costs = %+v, want cost_total=60 used=60 used24h=60 req24h=3", s)
		}
		// 整点对齐：now-1h 落在对应小时桶，且桶内计数为 1。
		p := findBucket(t, m.Points, now.Add(-time.Hour))
		if p.RequestCount != 1 || p.SuccessCount != 1 {
			t.Fatalf("bucket for now-1h = %+v, want 1 success", p)
		}
		if p.AvgLatencyMS == nil || !closeTo(*p.AvgLatencyMS, 100) {
			t.Fatalf("bucket avg latency = %v, want 100", p.AvgLatencyMS)
		}
	})

	t.Run("7d_has_168_points_and_partitions_by_channel", func(t *testing.T) {
		m, err := repo.Get(ctx, chA, ChannelMetricsWindow7d, now)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(m.Points) != 168 {
			t.Fatalf("points = %d, want 168", len(m.Points))
		}
		// 另一个渠道 3 天前的数据绝不能串进来。
		if m.Summary.RequestCount != 3 {
			t.Fatalf("channel A request_count = %d, want 3 (channel B must not leak in)", m.Summary.RequestCount)
		}
		b24, err := repo.Get(ctx, chB, ChannelMetricsWindow24h, now)
		if err != nil {
			t.Fatalf("Get B 24h: %v", err)
		}
		if b24.Summary.RequestCount != 0 {
			t.Fatalf("channel B 24h request_count = %d, want 0", b24.Summary.RequestCount)
		}
		b7, err := repo.Get(ctx, chB, ChannelMetricsWindow7d, now)
		if err != nil {
			t.Fatalf("Get B 7d: %v", err)
		}
		if b7.Summary.RequestCount != 1 {
			t.Fatalf("channel B 7d request_count = %d, want 1", b7.Summary.RequestCount)
		}
	})

	t.Run("empty_window_returns_zero_counts_and_null_rates", func(t *testing.T) {
		m, err := repo.Get(ctx, chC, ChannelMetricsWindow24h, now)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(m.Points) != 24 {
			t.Fatalf("points = %d, want 24 even with no samples", len(m.Points))
		}
		for i, p := range m.Points {
			if p.RequestCount != 0 || p.SuccessCount != 0 || p.FailureCount != 0 {
				t.Fatalf("point %d counts = %+v, want all zero", i, p)
			}
			if p.SuccessRate != nil || p.AvgLatencyMS != nil || p.AvgFirstTokenMS != nil || p.AvgCost != nil {
				t.Fatalf("point %d rates = %+v, want all null (— not 0)", i, p)
			}
		}
		s := m.Summary
		if s.RequestCount != 0 || s.CostTotal != 0 || s.UsedCredits != 0 {
			t.Fatalf("summary counts = %+v, want zeros", s)
		}
		if s.SuccessRate != nil || s.AvgLatencyMS != nil || s.AvgFirstTokenMS != nil || s.AvgCost != nil {
			t.Fatalf("summary rates = %+v, want all null", s)
		}
	})

	t.Run("success_rate_denominator_includes_refused_excludes_refunded", func(t *testing.T) {
		m, err := repo.Get(ctx, chD, ChannelMetricsWindow24h, now)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		s := m.Summary
		// 4 行都计入 request_count；成功率分母只有 success+error+refused = 3。
		if s.RequestCount != 4 {
			t.Fatalf("request_count = %d, want 4", s.RequestCount)
		}
		if s.SuccessCount != 1 || s.FailureCount != 2 {
			t.Fatalf("success/failure = %d/%d, want 1/2", s.SuccessCount, s.FailureCount)
		}
		if s.SuccessRate == nil || !closeTo(*s.SuccessRate, 1.0/3.0) {
			t.Fatalf("success_rate = %v, want 1/3（refused 入分母、refunded 不入）", s.SuccessRate)
		}
	})

	t.Run("batch_summaries_single_query", func(t *testing.T) {
		out, err := repo.Summaries(ctx, []int64{chA, chB, chC}, now)
		if err != nil {
			t.Fatalf("Summaries: %v", err)
		}
		a, ok := out[chA]
		if !ok || a.RequestCount != 3 || a.SuccessRate == nil || !closeTo(*a.SuccessRate, 2.0/3.0) {
			t.Fatalf("summary A = %+v (ok=%v), want requests=3 rate=2/3", a, ok)
		}
		if a.AvgLatencyMS == nil || !closeTo(*a.AvgLatencyMS, 300) {
			t.Fatalf("summary A avg latency = %v, want 300", a.AvgLatencyMS)
		}
		if _, ok := out[chB]; ok {
			t.Fatalf("summary B must be absent (3d old is outside 24h)")
		}
		if _, ok := out[chC]; ok {
			t.Fatalf("summary C must be absent (no rows)")
		}
	})

	t.Run("channel_index_exists", func(t *testing.T) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND tablename = 'usage_logs'
			  AND indexname = 'usage_logs_channel_created_idx'`).Scan(&n); err != nil {
			t.Fatalf("query index: %v", err)
		}
		if n != 1 {
			t.Fatalf("usage_logs_channel_created_idx count = %d, want 1", n)
		}
	})
}

// TestUsageLogsChannelIndexMigrationRoundTrip 验证 W16-1 日志迁移的 down/up
// 可往返：down 删索引、up 重建索引。放在事务里执行并回滚，不破坏其他用例
// 依赖的索引（DDL 在 PG 中是事务性的）。
func TestUsageLogsChannelIndexMigrationRoundTrip(t *testing.T) {
	db := openW16LogsDB(t)
	ctx := context.Background()

	upSQL, err := os.ReadFile("../../migrations-logs/20261022100000_usage_logs_channel_idx.up.sql")
	if err != nil {
		t.Fatalf("read up migration: %v", err)
	}
	downSQL, err := os.ReadFile("../../migrations-logs/20261022100000_usage_logs_channel_idx.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	indexCount := func() int {
		t.Helper()
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM pg_indexes
			WHERE schemaname = current_schema()
			  AND tablename = 'usage_logs'
			  AND indexname = 'usage_logs_channel_created_idx'`).Scan(&n); err != nil {
			t.Fatalf("query index: %v", err)
		}
		return n
	}

	if got := indexCount(); got != 1 {
		t.Fatalf("precondition: index count = %d, want 1", got)
	}
	if _, err := tx.Exec(string(downSQL)); err != nil {
		t.Fatalf("apply down migration: %v", err)
	}
	if got := indexCount(); got != 0 {
		t.Fatalf("after down: index count = %d, want 0", got)
	}
	// 再执行一次 down 必须仍成功（IF EXISTS 幂等）。
	if _, err := tx.Exec(string(downSQL)); err != nil {
		t.Fatalf("down migration must be idempotent: %v", err)
	}
	if _, err := tx.Exec(string(upSQL)); err != nil {
		t.Fatalf("apply up migration: %v", err)
	}
	if got := indexCount(); got != 1 {
		t.Fatalf("after up: index count = %d, want 1", got)
	}
}

func closeTo(got, want float64) bool {
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}
