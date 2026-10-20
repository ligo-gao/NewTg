package store

// 排行榜快照的读取仓储（D6a；modules-09 §C1.7、platform-01 §13–§17）。
//
// 【为什么读侧独立于写侧】快照写入是 worker 的事务逻辑（internal/rankings
// 的 snapshot.go），公开接口只需要「按 period 取最新 run + 子表」的确定性
// 读投影。把读侧放在 store 让 D6b 的 handler 不必接触聚合 SQL，也避免
// rankings 包成为 HTTP 层的依赖。
//
// 【为什么 LatestRun 一并返回 timeseries】趋势图（F-09-32）必须与榜单同源
// 同窗口：如果 D6b 先取 run 再单独查趋势，两次读之间可能插入新 run，图表
// 的桶范围就会与 model_stats 的窗口错位。一次读内按 run 的 window_start/
// window_end/granularity 取桶，得到一致快照。
//
// 【为什么带 Slot】颜色跟随实体（§C1.9）要求跨 period 稳定。槽位注册表
// rankings_entities 与 run 无 FK，但公开面总是「榜单实体 + 槽位」一起用；
// LEFT JOIN 把槽位随子表带出，D6b 不需要第二次查询（未注册实体或色板耗尽
// 的已注册实体 slot=NULL）。

import (
	"context"
	"database/sql"
	"fmt"
)

// RankingsSnapshot 是一次「run + 子表」的完整读结果。
//
// 行类型复用 models.go 的 GORM 模型：它们是 DB 的唯一权威投影，
// Slot 字段是从 rankings_entities LEFT JOIN 来的读投影（gorm:"-"）。
type RankingsSnapshot struct {
	Run        RankingsSnapshotRun
	Models     []RankingsModelStat
	Providers  []RankingsProviderStat
	Timeseries []RankingsTimeseries
}

// RankingsRepo 是公开排行榜的只读接缝（单测可注入假实现）。
type RankingsRepo interface {
	// LatestRun 返回 period 最新快照；从未生成过时 found=false。
	LatestRun(ctx context.Context, period string) (RankingsSnapshot, bool, error)
	// RunByID 返回指定 run；ID 不存在时 found=false。
	RunByID(ctx context.Context, id int64) (RankingsSnapshot, bool, error)
	// DeleteOlderRuns 只保留 period 最近 keep 个 run（级联删除子表）。
	DeleteOlderRuns(ctx context.Context, period string, keep int) (int64, error)
}

// SQLRankingsRepo 是 RankingsRepo 的 PostgreSQL 实现。
type SQLRankingsRepo struct {
	db *sql.DB
}

// NewRankingsRepo 构造排行榜读取仓储。db 必须非 nil：没有可用的内存兜底，
// 静默返回空快照会让「冷启动」文案掩盖真实的数据库故障。
func NewRankingsRepo(db *sql.DB) *SQLRankingsRepo {
	return &SQLRankingsRepo{db: db}
}

// RankingsExecer 是 DeleteOlderRunsTx 接受的最小执行面（*sql.DB 与 *sql.Tx
// 都满足）。写侧在快照事务内调用它，保证「插入新 run + 回收旧 run」原子。
type RankingsExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

const rankingsRunColumns = `id, period, window_start, window_end, snapshot_at,
	sample_threshold, status, model_count`

// LatestRun 见接口注释。
func (r *SQLRankingsRepo) LatestRun(ctx context.Context, period string) (RankingsSnapshot, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+rankingsRunColumns+`
		FROM rankings_snapshot_runs
		WHERE period = $1
		ORDER BY snapshot_at DESC, id DESC
		LIMIT 1`, period)
	run, err := scanRankingsSnapshotRun(row)
	if err == sql.ErrNoRows {
		return RankingsSnapshot{}, false, nil
	}
	if err != nil {
		return RankingsSnapshot{}, false, fmt.Errorf("store: latest rankings run: %w", err)
	}
	snap, err := loadRankingsChildren(ctx, r.db, run)
	if err != nil {
		return RankingsSnapshot{}, false, err
	}
	return snap, true, nil
}

// RunByID 见接口注释。
func (r *SQLRankingsRepo) RunByID(ctx context.Context, id int64) (RankingsSnapshot, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+rankingsRunColumns+`
		FROM rankings_snapshot_runs
		WHERE id = $1`, id)
	run, err := scanRankingsSnapshotRun(row)
	if err == sql.ErrNoRows {
		return RankingsSnapshot{}, false, nil
	}
	if err != nil {
		return RankingsSnapshot{}, false, fmt.Errorf("store: rankings run by id: %w", err)
	}
	snap, err := loadRankingsChildren(ctx, r.db, run)
	if err != nil {
		return RankingsSnapshot{}, false, err
	}
	return snap, true, nil
}

// DeleteOlderRuns 见接口注释。
func (r *SQLRankingsRepo) DeleteOlderRuns(ctx context.Context, period string, keep int) (int64, error) {
	return DeleteOlderRunsTx(ctx, r.db, period, keep)
}

// DeleteOlderRunsTx 删除 period 中除最近 keep 个之外的 run。
//
// 子表 rankings_model_stats / rankings_provider_stats 通过 ON DELETE CASCADE
// 一并回收（platform-01 §14/§15）。按 (snapshot_at DESC, id DESC) 排序而不是
// 只看 id：append-only 表的 id 单调，但同一毫秒内多次写入时 snapshot_at 相同，
// id 作为稳定的第二排序键保证「保留哪 10 个」是确定的。
func DeleteOlderRunsTx(ctx context.Context, q RankingsExecer, period string, keep int) (int64, error) {
	if q == nil {
		return 0, fmt.Errorf("store: delete older rankings runs: execer is nil")
	}
	if keep < 0 {
		return 0, fmt.Errorf("store: delete older rankings runs: keep must be >= 0, got %d", keep)
	}
	res, err := q.ExecContext(ctx, `
		DELETE FROM rankings_snapshot_runs
		WHERE period = $1
		  AND id NOT IN (
		    SELECT id FROM rankings_snapshot_runs
		    WHERE period = $1
		    ORDER BY snapshot_at DESC, id DESC
		    LIMIT $2
		  )`, period, keep)
	if err != nil {
		return 0, fmt.Errorf("store: delete older rankings runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete older rankings runs affected: %w", err)
	}
	return n, nil
}

// rankingsScanner 抽象 *sql.Row / *sql.Rows 的 Scan 方法。
type rankingsScanner interface {
	Scan(dest ...any) error
}

func scanRankingsSnapshotRun(s rankingsScanner) (RankingsSnapshotRun, error) {
	var run RankingsSnapshotRun
	err := s.Scan(
		&run.ID, &run.Period, &run.WindowStart, &run.WindowEnd, &run.SnapshotAt,
		&run.SampleThreshold, &run.Status, &run.ModelCount,
	)
	return run, err
}

// loadRankingsChildren 读取 run 的三类子数据（模型、作者、趋势桶）。
//
// 趋势桶的粒度由 period 派生（today→hour、year→week、其余→day），与
// internal/rankings.Period.Granularity 同口径。这里用 SQL CASE 表达，是为了
// 让 store 不反向依赖 rankings 包（保持「store 是最底层」的依赖方向）。
func loadRankingsChildren(ctx context.Context, db *sql.DB, run RankingsSnapshotRun) (RankingsSnapshot, error) {
	snap := RankingsSnapshot{Run: run}

	models, err := loadRankingsModels(ctx, db, run.ID)
	if err != nil {
		return RankingsSnapshot{}, err
	}
	snap.Models = models

	providers, err := loadRankingsProviders(ctx, db, run.ID)
	if err != nil {
		return RankingsSnapshot{}, err
	}
	snap.Providers = providers

	series, err := loadRankingsTimeseries(ctx, db, run)
	if err != nil {
		return RankingsSnapshot{}, err
	}
	snap.Timeseries = series
	return snap, nil
}

func loadRankingsModels(ctx context.Context, db *sql.DB, runID int64) ([]RankingsModelStat, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.run_id, m.model, m.display_name, m.author, m.total_tokens, m.requests,
		       m.rank, m.prev_rank, m.delta_tokens, m.delta_pct::float8, e.slot
		FROM rankings_model_stats m
		LEFT JOIN rankings_entities e
		       ON e.kind = 'model' AND e.entity = m.model
		WHERE m.run_id = $1
		ORDER BY m.rank NULLS LAST, m.model`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: rankings model stats: %w", err)
	}
	defer rows.Close()

	out := make([]RankingsModelStat, 0, 16)
	for rows.Next() {
		var (
			stat            RankingsModelStat
			display, author sql.NullString
			rank, prevRank  sql.NullInt64
			deltaTokens     sql.NullInt64
			deltaPct        sql.NullFloat64
			slot            sql.NullInt16
		)
		if err := rows.Scan(&stat.RunID, &stat.Model, &display, &author,
			&stat.TotalTokens, &stat.Requests, &rank, &prevRank,
			&deltaTokens, &deltaPct, &slot); err != nil {
			return nil, fmt.Errorf("store: scan rankings model stat: %w", err)
		}
		stat.DisplayName = nullStringPtr(display)
		stat.Author = nullStringPtr(author)
		stat.Rank = nullIntPtr(rank)
		stat.PrevRank = nullIntPtr(prevRank)
		stat.DeltaTokens = nullInt64Ptr(deltaTokens)
		stat.DeltaPct = nullFloat64Ptr(deltaPct)
		stat.Slot = nullInt16Ptr(slot)
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rankings model stats rows: %w", err)
	}
	return out, nil
}

func loadRankingsProviders(ctx context.Context, db *sql.DB, runID int64) ([]RankingsProviderStat, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.run_id, p.author, p.total_tokens, p.share_pct::float8, p.rank, e.slot
		FROM rankings_provider_stats p
		LEFT JOIN rankings_entities e
		       ON e.kind = 'author' AND e.entity = p.author
		WHERE p.run_id = $1
		ORDER BY p.rank NULLS LAST, p.author`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: rankings provider stats: %w", err)
	}
	defer rows.Close()

	out := make([]RankingsProviderStat, 0, 8)
	for rows.Next() {
		var (
			stat  RankingsProviderStat
			share sql.NullFloat64
			rank  sql.NullInt64
			slot  sql.NullInt16
		)
		if err := rows.Scan(&stat.RunID, &stat.Author, &stat.TotalTokens, &share, &rank, &slot); err != nil {
			return nil, fmt.Errorf("store: scan rankings provider stat: %w", err)
		}
		stat.SharePct = nullFloat64Ptr(share)
		stat.Rank = nullIntPtr(rank)
		stat.Slot = nullInt16Ptr(slot)
		out = append(out, stat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rankings provider stats rows: %w", err)
	}
	return out, nil
}

func loadRankingsTimeseries(ctx context.Context, db *sql.DB, run RankingsSnapshotRun) ([]RankingsTimeseries, error) {
	// 【下界放宽一个桶】写入侧按站点时区 date_trunc 对齐桶起点；year 的首个
	// 周桶会截断到窗口开始前的周一（例如 1 月 1 日是周四，首桶起点为 12 月
	// 29 日），若仍用 `bucket_start >= window_start` 会丢掉合法首桶。因此按
	// 粒度放宽恰好一个桶：hour −1h、day −1d、week −7d。上界保持 window_end
	// 不放松，避免把下一窗口的桶带进当前快照。
	rows, err := db.QueryContext(ctx, `
		SELECT t.model, t.granularity, t.bucket_start, COALESCE(t.tokens, 0)
		FROM rankings_timeseries t
		WHERE t.granularity = CASE $1::text
		        WHEN 'today' THEN 'hour'
		        WHEN 'year'  THEN 'week'
		        ELSE 'day'
		      END
		  AND t.bucket_start >= $2::timestamptz - CASE $1::text
		        WHEN 'today' THEN interval '1 hour'
		        WHEN 'year'  THEN interval '7 days'
		        ELSE interval '1 day'
		      END
		  AND t.bucket_start < $3
		  AND t.model IN (SELECT model FROM rankings_model_stats WHERE run_id = $4)
		ORDER BY t.bucket_start, t.model`, run.Period, run.WindowStart, run.WindowEnd, run.ID)
	if err != nil {
		return nil, fmt.Errorf("store: rankings timeseries: %w", err)
	}
	defer rows.Close()

	out := make([]RankingsTimeseries, 0, 64)
	for rows.Next() {
		var point RankingsTimeseries
		if err := rows.Scan(&point.Model, &point.Granularity, &point.BucketStart, &point.Tokens); err != nil {
			return nil, fmt.Errorf("store: scan rankings timeseries: %w", err)
		}
		out = append(out, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rankings timeseries rows: %w", err)
	}
	return out, nil
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func nullFloat64Ptr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func nullInt16Ptr(v sql.NullInt16) *int16 {
	if !v.Valid {
		return nil
	}
	n := v.Int16
	return &n
}
