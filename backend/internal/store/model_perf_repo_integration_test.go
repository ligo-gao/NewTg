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

// G-M6C-13 模型性能存储集成测试。与既有集成测试同一口径：
//
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_logs?sslmode=disable' \
//	go test -tags=integration ./internal/store/ -run TestModelPerf -p 1 -v
//
// 未设置 DSN 时 Skip。测试只使用 node='test-%' 的行并在 Cleanup 删除。

const modelPerfMigrationUp = "../../migrations-logs/20261024110000_model_perf_buckets.up.sql"
const modelPerfMigrationDown = "../../migrations-logs/20261024110000_model_perf_buckets.down.sql"

func openModelPerfTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_LOGS_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过模型性能存储集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	ensureModelPerfTable(t, db)
	// 同包测试串行执行：进入每个用例前清掉上次失败留下的 test-% 行，避免
	// 绝对快照测试读到脏数据。
	if _, err := db.Exec(`DELETE FROM model_perf_buckets WHERE node LIKE 'test-%'`); err != nil {
		_ = db.Close()
		t.Fatalf("clear model_perf_buckets: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM model_perf_buckets WHERE node LIKE 'test-%'`); err != nil {
			t.Logf("cleanup model_perf_buckets: %v", err)
		}
		_ = db.Close()
	})
	return db
}

func ensureModelPerfTable(t *testing.T, db *sql.DB) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass('public.model_perf_buckets') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("check model_perf_buckets: %v", err)
	}
	if exists {
		return
	}
	raw, err := os.ReadFile(modelPerfMigrationUp)
	if err != nil {
		t.Fatalf("read migration up: %v", err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("apply model perf migration: %v", err)
	}
}

func modelPerfTestSnapshot(start time.Time, model, node, epoch string, seq uint64,
	success, failed, refused int64) perfmetrics.Snapshot {

	return perfmetrics.Snapshot{
		BucketStart:    start.UTC(),
		Model:          model,
		Node:           node,
		Epoch:          epoch,
		Seq:            seq,
		Success:        success,
		Error:          failed,
		Refused:        refused,
		LatencyCount:   success + failed + refused,
		LatencySumMS:   100 * float64(success+failed+refused),
		LatencyBuckets: [perfmetrics.HistogramBuckets]int64{0, success + failed + refused},
	}
}

func TestModelPerfRepoUpsertIsAbsoluteSnapshot(t *testing.T) {
	db := openModelPerfTestDB(t)
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
	db := openModelPerfTestDB(t)
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
	db := openModelPerfTestDB(t)
	repo := NewModelPerfRepo(db)
	ctx := context.Background()
	hour := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
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

func TestModelPerfRepoModelFilterAndInvalidGranularity(t *testing.T) {
	db := openModelPerfTestDB(t)
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
}

// TestModelPerfMigrationUpDown 在事务内验证迁移 SQL 可上可下，结束时回滚，
// 不影响其他用例建好的表。
func TestModelPerfMigrationUpDown(t *testing.T) {
	db := openModelPerfTestDB(t)
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
	if _, err := tx.Exec(string(upSQL)); err != nil {
		t.Fatalf("up: %v", err)
	}
	var indexExists bool
	if err := tx.QueryRow(`SELECT to_regclass('public.model_perf_buckets_window_idx') IS NOT NULL`).Scan(&indexExists); err != nil {
		t.Fatalf("index check: %v", err)
	}
	if !indexExists {
		t.Fatalf("model_perf_buckets_window_idx missing after up migration")
	}
}
