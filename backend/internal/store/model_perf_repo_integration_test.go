//go:build integration

package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/perfmetrics"
)

// G-M6C-13 独立指标库存储/保留集成测试。与既有集成测试同一口径：
//
//	NEWTG_TEST_METRICS_DSN='postgres://newtg:newtg@localhost:55432/newtg_metrics?sslmode=disable' \
//	go test -tags=integration ./internal/store/ -run TestModelPerf -p 1 -v
//
// 未设置 DSN 时 Skip。测试库必须是**独立指标库**（不是 newtg_logs / newtg）；
// 用例进入时 TRUNCATE 两张指标表，互不影响。

const (
	modelPerfMigrationUp   = "../../migrations-metrics/20261024110000_model_perf_buckets.up.sql"
	modelPerfMigrationDown = "../../migrations-metrics/20261024110000_model_perf_buckets.down.sql"
)

func openMetricsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_METRICS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_METRICS_DSN 未设置，跳过模型性能指标库集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open metrics db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping metrics db: %v", err)
	}
	ensureModelPerfTables(t, db)
	// 独立指标库只服务本包用例；串行执行（-p 1）下每次进入清空。
	if _, err := db.Exec(`TRUNCATE model_perf_buckets, model_perf_hourly_buckets`); err != nil {
		_ = db.Close()
		t.Fatalf("truncate model perf tables: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func ensureModelPerfTables(t *testing.T, db *sql.DB) {
	t.Helper()
	raw, err := os.ReadFile(modelPerfMigrationUp)
	if err != nil {
		t.Fatalf("read metrics migration up: %v", err)
	}
	// 迁移 SQL 全部 IF NOT EXISTS，重复执行是幂等 no-op；角色守卫允许在
	// 没有 newtg_app/newtg_backup 的测试库里执行。
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("apply metrics migration: %v", err)
	}
}

func modelPerfTestSnapshot(start time.Time, model, node, epoch string, seq uint64,
	success, failed, refused int64) perfmetrics.Snapshot {

	total := success + failed + refused
	return perfmetrics.Snapshot{
		BucketStart:    start.UTC(),
		Model:          model,
		Node:           node,
		Epoch:          epoch,
		Seq:            seq,
		Success:        success,
		Error:          failed,
		Refused:        refused,
		LatencyCount:   total,
		LatencySumMS:   100 * float64(total),
		LatencyBuckets: [perfmetrics.HistogramBuckets]int64{0, total},
	}
}

func TestModelPerfRepoUpsertIsAbsoluteSnapshot(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	epoch := "11111111-1111-4111-8111-111111111111"
	snap := modelPerfTestSnapshot(start, "gpt-test", "test-abs", epoch, 1, 9, 1, 0)

	if err := repo.UpsertBuckets(ctx, []perfmetrics.Snapshot{snap}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	buckets, updated, err := repo.ModelPerfBuckets(ctx, start, start.Add(time.Minute), "1m", "gpt-test")
	if err != nil {
		t.Fatalf("query after first upsert: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != 9 || buckets[0].Error != 1 {
		t.Fatalf("buckets = %+v, want success=9 error=1", buckets)
	}
	if buckets[0].ObservedSeconds != 60 || buckets[0].UpSeconds != 60 {
		t.Fatalf("availability = (%v,%v), want (60,60)", buckets[0].ObservedSeconds, buckets[0].UpSeconds)
	}
	if updated.IsZero() {
		t.Fatalf("updatedAt must be non-zero")
	}

	// 同一序号重放（内容不同）必须被忽略：绝对快照不能被旧序号回滚。
	replay := modelPerfTestSnapshot(start, "gpt-test", "test-abs", epoch, 1, 1, 0, 0)
	if err := repo.UpsertBuckets(ctx, []perfmetrics.Snapshot{replay}); err != nil {
		t.Fatalf("replay upsert: %v", err)
	}
	buckets, _, err = repo.ModelPerfBuckets(ctx, start, start.Add(time.Minute), "1m", "gpt-test")
	if err != nil {
		t.Fatalf("query after replay: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != 9 || buckets[0].Error != 1 {
		t.Fatalf("replay changed snapshot: %+v", buckets)
	}

	// 更大序号（迟到样本让累计量变大）必须覆盖。
	next := modelPerfTestSnapshot(start, "gpt-test", "test-abs", epoch, 2, 10, 1, 0)
	if err := repo.UpsertBuckets(ctx, []perfmetrics.Snapshot{next}); err != nil {
		t.Fatalf("next upsert: %v", err)
	}
	buckets, _, err = repo.ModelPerfBuckets(ctx, start, start.Add(time.Minute), "1m", "gpt-test")
	if err != nil {
		t.Fatalf("query after next: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Success != 10 {
		t.Fatalf("higher seq did not replace snapshot: %+v", buckets)
	}
}

func TestModelPerfRepoSumsNodesAndEpochs(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	start := time.Date(2026, 10, 22, 11, 0, 0, 0, time.UTC)
	snaps := []perfmetrics.Snapshot{
		modelPerfTestSnapshot(start, "sum-model", "test-node-a", "22222222-2222-4222-8222-222222222222", 1, 3, 1, 0),
		modelPerfTestSnapshot(start, "sum-model", "test-node-b", "33333333-3333-4333-8333-333333333333", 1, 4, 2, 0),
	}
	if err := repo.UpsertBuckets(ctx, snaps); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	buckets, _, err := repo.ModelPerfBuckets(ctx, start, start.Add(time.Minute), "1m", "sum-model")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1 aggregated row", len(buckets))
	}
	got := buckets[0]
	if got.Success != 7 || got.Error != 3 || got.Refused != 0 || got.LatencyCount != 10 {
		t.Fatalf("aggregate = %+v, want success=7 error=3 latency=10", got)
	}
	// 7/10 < 90%：观测 60s，但不可用。
	if got.ObservedSeconds != 60 || got.UpSeconds != 0 {
		t.Fatalf("availability = (%v,%v), want (60,0)", got.ObservedSeconds, got.UpSeconds)
	}
}

func TestModelPerfRepoHourRollupKeepsMinuteJudgement(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	// 用「当前时刻向前 2 小时」构造窗口，确保落在分钟表保留的 7 天内，
	// 1h 查询走分钟行在线 rollup 分支。
	hour := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	epoch := "44444444-4444-4444-8444-444444444444"
	snaps := []perfmetrics.Snapshot{
		// 5 个样本全成功 -> 该分钟可用。
		modelPerfTestSnapshot(hour, "hour-model", "test-hour", epoch, 1, 5, 0, 0),
		// 5 个样本全成功 -> 该分钟可用。
		modelPerfTestSnapshot(hour.Add(time.Minute), "hour-model", "test-hour", epoch, 1, 5, 0, 0),
		// 1 个样本 -> 不计可用，但计入观测。
		modelPerfTestSnapshot(hour.Add(2*time.Minute), "hour-model", "test-hour", epoch, 1, 1, 0, 0),
	}
	if err := repo.UpsertBuckets(ctx, snaps); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	buckets, _, err := repo.ModelPerfBuckets(ctx, hour, hour.Add(time.Hour), "1h", "hour-model")
	if err != nil {
		t.Fatalf("query hour: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("hour buckets = %d, want 1", len(buckets))
	}
	got := buckets[0]
	if !got.Start.Equal(hour) || got.Success != 11 {
		t.Fatalf("hour bucket = %+v, want start=%v success=11", got, hour)
	}
	// 三个分钟各 60s 观测；前两分钟可用，第三分钟样本不足不可用。
	if got.ObservedSeconds != 180 || got.UpSeconds != 120 {
		t.Fatalf("hour availability = (%v,%v), want (180,120)", got.ObservedSeconds, got.UpSeconds)
	}
}

func TestModelPerfRepoReadsMaterializedHourlyTable(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	// 10 天前已在保留期之外（>7d），1h 查询必须读小时表而不是分钟表。
	hour := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_perf_hourly_buckets (
			bucket_start, model, success, error, refused, latency_count, latency_sum_ms,
			lat_b1, ttft_count, ttft_b0, completion_tokens, generation_seconds,
			observed_seconds, up_seconds)
		VALUES ($1, 'hour-old', 5, 0, 0, 5, 500, 5, 5, 5, 100, 2, 60, 60)`, hour); err != nil {
		t.Fatalf("insert hourly row: %v", err)
	}
	buckets, updated, err := repo.ModelPerfBuckets(ctx, hour, hour.Add(time.Hour), "1h", "hour-old")
	if err != nil {
		t.Fatalf("query hourly table: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	got := buckets[0]
	if !got.Start.Equal(hour) || got.Success != 5 || got.LatencyCount != 5 ||
		got.LatencyBuckets[1] != 5 || got.TTFTCount != 5 || got.CompletionTokens != 100 {
		t.Fatalf("hourly bucket = %+v", got)
	}
	if got.ObservedSeconds != 60 || got.UpSeconds != 60 {
		t.Fatalf("hourly availability = (%v,%v), want (60,60)", got.ObservedSeconds, got.UpSeconds)
	}
	if updated.IsZero() {
		t.Fatalf("updatedAt must be non-zero")
	}
}

func TestModelPerfRepoHourQueryMergesOldHoursAndRecentMinutes(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	now := time.Now().UTC()
	oldHour := now.Add(-10 * 24 * time.Hour).Truncate(time.Hour)
	recentHour := now.Add(-time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_perf_hourly_buckets (bucket_start, model, success, observed_seconds, up_seconds)
		VALUES ($1, 'merge-model', 3, 60, 60)`, oldHour); err != nil {
		t.Fatalf("insert old hour: %v", err)
	}
	epoch := "77777777-7777-4777-8777-777777777777"
	if err := repo.UpsertBuckets(ctx, []perfmetrics.Snapshot{
		modelPerfTestSnapshot(recentHour, "merge-model", "test-merge", epoch, 1, 4, 0, 0),
	}); err != nil {
		t.Fatalf("insert recent minute: %v", err)
	}
	buckets, _, err := repo.ModelPerfBuckets(ctx, oldHour, now, "1h", "merge-model")
	if err != nil {
		t.Fatalf("query merged hours: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2 (old hour + recent minute rollup): %+v", len(buckets), buckets)
	}
	if !buckets[0].Start.Equal(oldHour) || buckets[0].Success != 3 {
		t.Fatalf("first bucket = %+v, want old hour success=3", buckets[0])
	}
	if !buckets[1].Start.Equal(recentHour) || buckets[1].Success != 4 {
		t.Fatalf("second bucket = %+v, want recent hour success=4", buckets[1])
	}
}

func TestModelPerfRepoModelFilterAndInvalidGranularity(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	start := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
	epoch := "55555555-5555-4555-8555-555555555555"
	snaps := []perfmetrics.Snapshot{
		modelPerfTestSnapshot(start, "filter-a", "test-filter", epoch, 1, 1, 0, 0),
		modelPerfTestSnapshot(start, "filter-b", "test-filter", epoch, 1, 2, 0, 0),
	}
	if err := repo.UpsertBuckets(ctx, snaps); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	buckets, _, err := repo.ModelPerfBuckets(ctx, start, start.Add(time.Minute), "1m", "filter-b")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Model != "filter-b" || buckets[0].Success != 2 {
		t.Fatalf("filtered buckets = %+v, want only filter-b success=2", buckets)
	}
	if _, _, err := repo.ModelPerfBuckets(ctx, start, start.Add(time.Minute), "5m", ""); err == nil {
		t.Fatalf("invalid granularity must return an error")
	}
}

func TestModelPerfRepoNilDB(t *testing.T) {
	repo := NewModelPerfRepo(nil)
	if err := repo.UpsertBuckets(context.Background(), []perfmetrics.Snapshot{{}}); err == nil {
		t.Fatalf("nil db upsert must fail")
	}
	if _, _, err := repo.ModelPerfBuckets(context.Background(), time.Now(), time.Now(), "1m", ""); err == nil {
		t.Fatalf("nil db query must fail")
	}
	if _, err := repo.RunRetention(context.Background(), perfmetrics.RetentionOptions{}); err == nil {
		t.Fatalf("nil db retention must fail")
	}
}

// TestModelPerfRetentionMaterializesAndCleans 验证：
//   - 分钟→小时物化（先判定分钟可用性再累加）；
//   - 7 天前的分钟行删除（但先物化，不丢 8 天前的数据）；
//   - 超过 PERF_METRICS_RETENTION_DAYS 的小时行删除。
func TestModelPerfRetentionMaterializesAndCleans(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	epoch := "66666666-6666-4666-8666-666666666666"

	oldHour := now.Add(-8 * 24 * time.Hour).Truncate(time.Hour)
	recentHour := now.Add(-time.Hour).Truncate(time.Hour)
	snaps := []perfmetrics.Snapshot{
		modelPerfTestSnapshot(oldHour, "ret-model", "test-ret", epoch, 1, 5, 0, 0),
		modelPerfTestSnapshot(oldHour.Add(time.Minute), "ret-model", "test-ret", epoch, 1, 5, 0, 0),
		modelPerfTestSnapshot(recentHour, "ret-model", "test-ret", epoch, 1, 1, 0, 0),
	}
	if err := repo.UpsertBuckets(ctx, snaps); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 一条超过 400 天的小时行必须被清理。
	expiredHour := now.Add(-401 * 24 * time.Hour).Truncate(time.Hour)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO model_perf_hourly_buckets (bucket_start, model, success, observed_seconds, up_seconds)
		VALUES ($1, 'ret-model', 1, 60, 60)`, expiredHour); err != nil {
		t.Fatalf("insert expired hour: %v", err)
	}

	result, err := repo.RunRetention(ctx, perfmetrics.RetentionOptions{
		Now:             now,
		MinuteRetention: 7 * 24 * time.Hour,
		HourRetention:   400 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("RunRetention: %v", err)
	}
	if !result.Locked {
		t.Fatalf("retention did not acquire the advisory lock")
	}
	if result.MinutesDeleted < 2 {
		t.Fatalf("minutes deleted = %d, want >=2", result.MinutesDeleted)
	}
	if result.HoursDeleted < 1 {
		t.Fatalf("hours deleted = %d, want >=1", result.HoursDeleted)
	}

	// 8 天前的分钟行已删除，但对应小时行已物化（success=10，观测/可用 120s）。
	var minutes int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM model_perf_buckets WHERE bucket_start < $1`, now.Add(-7*24*time.Hour)).
		Scan(&minutes); err != nil {
		t.Fatalf("count old minutes: %v", err)
	}
	if minutes != 0 {
		t.Fatalf("old minutes = %d, want 0", minutes)
	}
	var success int64
	var observed, up float64
	if err := db.QueryRowContext(ctx, `
		SELECT success, observed_seconds, up_seconds FROM model_perf_hourly_buckets
		WHERE bucket_start = $1 AND model = 'ret-model'`, oldHour).
		Scan(&success, &observed, &up); err != nil {
		t.Fatalf("read materialized hour: %v", err)
	}
	if success != 10 || observed != 120 || up != 120 {
		t.Fatalf("materialized hour = (%d,%v,%v), want (10,120,120)", success, observed, up)
	}

	// 7 天内的分钟行保留。
	var recentMinutes int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM model_perf_buckets WHERE bucket_start = $1 AND model = 'ret-model'`, recentHour).
		Scan(&recentMinutes); err != nil {
		t.Fatalf("count recent minutes: %v", err)
	}
	if recentMinutes != 1 {
		t.Fatalf("recent minutes = %d, want 1", recentMinutes)
	}

	// 超过 400 天的小时行已删除。
	var expiredRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM model_perf_hourly_buckets WHERE bucket_start = $1 AND model = 'ret-model'`, expiredHour).
		Scan(&expiredRows); err != nil {
		t.Fatalf("count expired hours: %v", err)
	}
	if expiredRows != 0 {
		t.Fatalf("expired hours = %d, want 0", expiredRows)
	}
}

func TestModelPerfRetentionSkipsWhenLocked(t *testing.T) {
	db := openMetricsTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin lock tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, modelPerfRetentionLockKey); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}

	result, err := repo.RunRetention(ctx, perfmetrics.RetentionOptions{Now: time.Now()})
	if err != nil {
		t.Fatalf("RunRetention while locked: %v", err)
	}
	if result.Locked {
		t.Fatalf("second replica must skip retention when the lock is held")
	}
}

// TestModelPerfMetricsMigrationIdempotent 在事务内验证指标库迁移可上可下，
// 且重复执行 up 是幂等的；结束时回滚，不影响其他用例。
func TestModelPerfMetricsMigrationIdempotent(t *testing.T) {
	db := openMetricsTestDB(t)
	upSQL, err := os.ReadFile(modelPerfMigrationUp)
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	downSQL, err := os.ReadFile(modelPerfMigrationDown)
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(string(downSQL)); err != nil {
		t.Fatalf("down: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := tx.Exec(string(upSQL)); err != nil {
			t.Fatalf("up #%d: %v", i+1, err)
		}
	}
	for _, index := range []string{"model_perf_buckets_window_idx", "model_perf_hourly_buckets_window_idx"} {
		var exists bool
		if err := tx.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, "public."+index).Scan(&exists); err != nil {
			t.Fatalf("index check %s: %v", index, err)
		}
		if !exists {
			t.Fatalf("%s missing after up migration", index)
		}
	}
	for table, want := range map[string]int{
		"model_perf_buckets":        32,
		"model_perf_hourly_buckets": 31,
	} {
		var got int
		if err := tx.QueryRow(`SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1`, table).Scan(&got); err != nil {
			t.Fatalf("column count %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("%s columns = %d, want %d", table, got, want)
		}
	}
}
