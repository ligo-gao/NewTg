//go:build integration

package store

// D6a 排行榜读取仓储的数据库级验收：LatestRun / RunByID / DeleteOlderRuns。
//
// 覆盖：
//   - 按 (snapshot_at DESC, id DESC) 取最新 run；
//   - 子表按 rank 排序、LEFT JOIN 带出槽位；
//   - timeseries 只取 run 窗口内、period 对应粒度、且属于该 run 入榜模型的桶；
//   - 保留最近 10 个 run，旧 run 的子表经 ON DELETE CASCADE 回收。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d6?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestRankingsRepo -count=1 -v
//
// 只用 period=month 与带唯一前缀的模型名：rankings 包的集成测试用 today，
// 两个包并行时互不干扰。

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func rankingsRepoPrefix() string {
	return fmt.Sprintf("d6a-repo-%d", time.Now().UnixNano())
}

func insertRankingsRun(t *testing.T, db *sql.DB, period string, windowStart, windowEnd, snapshotAt time.Time, modelCount int) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`
		INSERT INTO rankings_snapshot_runs
		    (period, window_start, window_end, snapshot_at, sample_threshold, status, model_count)
		VALUES ($1, $2, $3, $4, 50, $5, $6)
		RETURNING id`,
		period, windowStart, windowEnd, snapshotAt,
		map[bool]string{true: "empty", false: "ok"}[modelCount == 0], modelCount,
	).Scan(&id); err != nil {
		t.Fatalf("insert rankings run: %v", err)
	}
	return id
}

// TestRankingsRepoTimeseriesFirstWeekBucket 覆盖 M1：year 周期的首个周桶
// 会被 date_trunc 截断到 window_start 之前的周一，读取时按 week 粒度把
// 下界放宽 7 天，首桶必须返回；超出 7 天前或窗口后的桶仍必须排除。
func TestRankingsRepoTimeseriesFirstWeekBucket(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewRankingsRepo(db)
	prefix := rankingsRepoPrefix()
	windowStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	model := prefix + "-year"

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = 'year' AND window_start = $1`, windowStart)
		_, _ = db.Exec(`DELETE FROM rankings_timeseries WHERE model = $1`, model)
	})
	if _, err := db.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = 'year' AND window_start = $1`, windowStart); err != nil {
		t.Fatalf("pre-clean year run: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM rankings_timeseries WHERE model = $1`, model); err != nil {
		t.Fatalf("pre-clean timeseries: %v", err)
	}

	runID := insertRankingsRun(t, db, "year", windowStart, windowEnd, windowEnd, 1)
	if _, err := db.Exec(`
		INSERT INTO rankings_model_stats (run_id, model, total_tokens, requests, rank)
		VALUES ($1, $2, 100, 50, 1)`, runID, model); err != nil {
		t.Fatalf("insert model stat: %v", err)
	}
	for _, point := range []struct {
		bucket time.Time
		tokens int64
	}{
		{time.Date(2025, 12, 26, 0, 0, 0, 0, time.UTC), 111}, // window_start - 6d：首桶（放宽 7d 内）
		{time.Date(2025, 12, 24, 0, 0, 0, 0, time.UTC), 999}, // window_start - 8d：过旧
		{time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 222},   // 窗口内
	} {
		if _, err := db.Exec(`INSERT INTO rankings_timeseries (model, granularity, bucket_start, tokens)
			VALUES ($1, 'week', $2, $3)`, model, point.bucket, point.tokens); err != nil {
			t.Fatalf("insert week bucket: %v", err)
		}
	}

	// 用 RunByID 定位本用例刚插入的 run：LatestRun("year") 会被共享隔离库里
	// 其它包并发写入的更晚 snapshot_at 抢先（测试非密闭，复核实测可复现）。
	snap, found, err := repo.RunByID(ctx, runID)
	if err != nil || !found {
		t.Fatalf("RunByID(%d): found=%v err=%v", runID, found, err)
	}
	if len(snap.Timeseries) != 2 ||
		snap.Timeseries[0].Tokens != 111 || !snap.Timeseries[0].BucketStart.Equal(time.Date(2025, 12, 26, 0, 0, 0, 0, time.UTC)) ||
		snap.Timeseries[1].Tokens != 222 {
		t.Fatalf("year timeseries = %+v，want 首桶 111 + 窗口内 222", snap.Timeseries)
	}
}

func TestRankingsRepoReadAndRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewRankingsRepo(db)
	prefix := rankingsRepoPrefix()
	windowStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)

	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = 'month'`)
		_, _ = db.Exec(`DELETE FROM rankings_timeseries WHERE model LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM rankings_author_timeseries WHERE author LIKE $1`, prefix+"%")
		_, _ = db.Exec(`DELETE FROM rankings_entities WHERE entity LIKE $1`, prefix+"%")
	})

	// 第一次删除可能在库中残留其他测试的 month run；先清空本表 period=month。
	if _, err := db.Exec(`DELETE FROM rankings_snapshot_runs WHERE period = 'month'`); err != nil {
		t.Fatalf("pre-clean month runs: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM rankings_timeseries WHERE model LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("pre-clean timeseries: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM rankings_author_timeseries WHERE author LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("pre-clean author timeseries: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM rankings_entities WHERE entity LIKE $1`, prefix+"%"); err != nil {
		t.Fatalf("pre-clean entities: %v", err)
	}

	modelA := prefix + "-a"
	modelB := prefix + "-b"
	author := prefix + "-author"

	runID := insertRankingsRun(t, db, "month", windowStart, windowEnd, windowEnd, 2)
	for i, m := range []struct {
		model  string
		tokens int64
		rank   int
	}{
		{modelA, 300, 1},
		{modelB, 200, 2},
	} {
		if _, err := db.Exec(`
			INSERT INTO rankings_model_stats
			    (run_id, model, display_name, author, total_tokens, requests,
			     rank, prev_rank, delta_tokens, delta_pct)
			VALUES ($1, $2, $3, $4, $5, 50, $6, NULL, $5, NULL)`,
			runID, m.model, m.model+" display", author, m.tokens, m.rank); err != nil {
			t.Fatalf("insert model stat %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO rankings_provider_stats (run_id, author, total_tokens, share_pct, rank)
		VALUES ($1, $2, 100, 100.00, 1)`, runID, author); err != nil {
		t.Fatalf("insert provider stat: %v", err)
	}
	// 槽位注册（跨 run）。
	for i, entity := range []string{modelA, modelB, author} {
		kind := "model"
		if entity == author {
			kind = "author"
		}
		if _, err := db.Exec(`INSERT INTO rankings_entities (entity, kind, slot) VALUES ($1, $2, $3)`,
			entity, kind, i+1); err != nil {
			t.Fatalf("insert entity slot: %v", err)
		}
	}
	// 趋势桶：窗口内一条、窗口前一桶边界一条（M1：day 粒度放宽 1 天，
	// 首桶起点早于 window_start 仍须返回）、以及其他模型一条（不应出现）。
	for _, point := range []struct {
		model  string
		bucket time.Time
		tokens int64
	}{
		{modelA, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), 120},
		{modelA, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), 999}, // window_start - 1d：首桶
		{prefix + "-other", time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), 500},
	} {
		if _, err := db.Exec(`INSERT INTO rankings_timeseries (model, granularity, bucket_start, tokens)
			VALUES ($1, 'day', $2, $3)`, point.model, point.bucket, point.tokens); err != nil {
			t.Fatalf("insert timeseries: %v", err)
		}
	}
	// 作者序列：窗口内、窗口前一桶边界（放宽 1 天）各一条，窗口外一条不得返回；
	// 另一个不在该 run 作者榜的作者的桶也不得返回。
	for _, point := range []struct {
		author string
		bucket time.Time
		tokens int64
	}{
		{author, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), 90},
		{author, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), 11},
		{author, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), 777}, // window_start - 3d：过旧
		{prefix + "-outsider", time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), 555},
	} {
		if _, err := db.Exec(`INSERT INTO rankings_author_timeseries (period, author, granularity, bucket_start, tokens)
			VALUES ('month', $1, 'day', $2, $3)`, point.author, point.bucket, point.tokens); err != nil {
			t.Fatalf("insert author timeseries: %v", err)
		}
	}
	// 同一作者、同一桶但属于 week 周期：run 是 month，读侧必须按 period 过滤，
	// 否则 week 的行会混进 month 的 series（这是 B5a 收口要防的跨周期泄漏）。
	if _, err := db.Exec(`INSERT INTO rankings_author_timeseries (period, author, granularity, bucket_start, tokens)
		VALUES ('week', $1, 'day', $2, $3)`,
		author, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), 4444); err != nil {
		t.Fatalf("insert cross-period author timeseries: %v", err)
	}

	snap, found, err := repo.LatestRun(ctx, "month")
	if err != nil || !found {
		t.Fatalf("LatestRun: found=%v err=%v", found, err)
	}
	if snap.Run.ID != runID || snap.Run.ModelCount != 2 || snap.Run.SampleThreshold != 50 {
		t.Fatalf("run = %+v", snap.Run)
	}
	if len(snap.Models) != 2 || snap.Models[0].Model != modelA || snap.Models[1].Model != modelB {
		t.Fatalf("模型行顺序错误: %+v", snap.Models)
	}
	if snap.Models[0].Slot == nil || *snap.Models[0].Slot != 1 {
		t.Fatalf("模型 A 槽位 = %v，want 1", snap.Models[0].Slot)
	}
	if snap.Models[0].DisplayName == nil || *snap.Models[0].DisplayName != modelA+" display" {
		t.Fatalf("display_name = %v", snap.Models[0].DisplayName)
	}
	if len(snap.Providers) != 1 || snap.Providers[0].Slot == nil || *snap.Providers[0].Slot != 3 {
		t.Fatalf("作者行 = %+v", snap.Providers)
	}
	if snap.Providers[0].SharePct == nil || *snap.Providers[0].SharePct != 100 {
		t.Fatalf("share_pct = %v", snap.Providers[0].SharePct)
	}
	// 只返回窗口内（含 M1 放宽的首桶）且属于入榜模型的日桶。
	if len(snap.Timeseries) != 2 ||
		snap.Timeseries[0].Model != modelA || snap.Timeseries[0].Tokens != 999 ||
		snap.Timeseries[1].Model != modelA || snap.Timeseries[1].Tokens != 120 {
		t.Fatalf("timeseries = %+v", snap.Timeseries)
	}
	for _, point := range snap.Timeseries {
		if point.Granularity != "day" {
			t.Fatalf("粒度 = %s，want day", point.Granularity)
		}
	}

	// 作者序列只返回该 run 作者榜（Top5）成员、窗口内（含放宽首桶）的桶。
	if len(snap.AuthorSeries) != 2 ||
		snap.AuthorSeries[0].Author != author || snap.AuthorSeries[0].Tokens != 11 ||
		snap.AuthorSeries[1].Author != author || snap.AuthorSeries[1].Tokens != 90 {
		t.Fatalf("author series = %+v，want 窗口内 11 + 90", snap.AuthorSeries)
	}
	for _, point := range snap.AuthorSeries {
		if point.Period != "month" {
			t.Fatalf("作者序列 period = %q，want month（week 的同桶行不得泄漏）", point.Period)
		}
		if point.Granularity != "day" {
			t.Fatalf("作者序列粒度 = %s，want day", point.Granularity)
		}
	}

	byID, found, err := repo.RunByID(ctx, runID)
	if err != nil || !found || len(byID.Models) != 2 {
		t.Fatalf("RunByID: found=%v err=%v models=%d", found, err, len(byID.Models))
	}
	if _, found, err := repo.RunByID(ctx, 1<<62); err != nil || found {
		t.Fatalf("不存在的 run: found=%v err=%v", found, err)
	}

	// 保留最近 10 个：在已有 run 之后再插 11 个（共 12），最旧的两个应被删除。
	extraIDs := make([]int64, 0, 11)
	for i := 0; i < 11; i++ {
		at := windowEnd.Add(time.Duration(i+1) * time.Minute)
		id := insertRankingsRun(t, db, "month", windowStart, windowEnd, at, 0)
		extraIDs = append(extraIDs, id)
		if _, err := db.Exec(`INSERT INTO rankings_model_stats (run_id, model, total_tokens, requests, rank)
			VALUES ($1, $2, 1, 50, 1)`, id, prefix+"-child"); err != nil {
			t.Fatalf("insert extra child: %v", err)
		}
	}
	deleted, err := repo.DeleteOlderRuns(ctx, "month", 10)
	if err != nil {
		t.Fatalf("DeleteOlderRuns: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("删除 run 数 = %d，want 2", deleted)
	}
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM rankings_snapshot_runs WHERE period = 'month'`).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 10 {
		t.Fatalf("剩余 run 数 = %d，want 10", remaining)
	}
	// 最早的 run（runID 本身）已被删除，子表必须级联清空。
	var children int
	if err := db.QueryRow(`SELECT count(*) FROM rankings_model_stats WHERE run_id = $1`, runID).Scan(&children); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if children != 0 {
		t.Fatalf("被删 run 的子表未级联删除，剩 %d 行", children)
	}
}
