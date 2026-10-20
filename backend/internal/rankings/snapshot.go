package rankings

// 排行榜快照写入（D6a；R-D6-07/R-D6-09、platform-01 §13–§17）。
//
// 【单事务】run + model_stats + provider_stats + timeseries +
// author_timeseries + entities 必须同一次提交：公开接口只读「最新 run」，
// 如果 run 先提交而子表分几次写，读侧会看到 model_count 与子表行数不一致
// 的半成品。失败整体回滚，上一份完整快照继续服务（append-only 的历史 run
// 不受影响）。
//
// 【保留 K 个 run】K 来自 site.rankings_keep_runs（默认 10；缺省由 worker
// 解析后随 snapshotInput 传入）。清理放在同一事务里：新 run 插入后立刻删更早
// 的 run，避免「插入成功但清理失败」留下无界增长的 run 表。子表由
// ON DELETE CASCADE 回收。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"newtg/gateway/internal/store"
)

const (
	// statusOK / statusEmpty 是 R-D6-03 允许写入的两个状态；
	// partial 由服务端刻意不产生（没有可靠的「部分成功」判据）。
	statusOK    = "ok"
	statusEmpty = "empty"
	// entityKindModel / entityKindAuthor 对应 rankings_entities.kind。
	entityKindModel  = "model"
	entityKindAuthor = "author"
	// entitySlotCount 是分类色板槽位数（§C1.9：第 1–8 槽，禁止第 9 色）。
	entitySlotCount = 8
	// rankingsEntityLockKey 是槽位分配的 PG 事务级咨询锁键。
	// 多个 period 的刷新可能并发（today 5m 与 week 30m 同一秒触发），
	// 槽位必须按「首次见」顺序串行分配，否则两个事务都读到相同空闲槽。
	rankingsEntityLockKey int64 = 0x52414E4B533031 // "RANKS01"
)

// snapshotInput 是一次快照事务的完整内容。
type snapshotInput struct {
	Period          Period
	Window          Window
	SnapshotAt      time.Time
	SampleThreshold int64
	// KeepRuns 是本次清理要保留的 run 数（site.rankings_keep_runs）；
	// <=0 时回退 defaultKeepRuns，避免调用方零值导致删掉刚写入的 run。
	KeepRuns     int
	Models       []store.RankingsModelStat
	Providers    []store.RankingsProviderStat
	Timeseries   []store.RankingsTimeseries
	AuthorSeries []store.RankingsAuthorTimeseries
}

// writeSnapshot 在单事务内写入一份完整快照并返回 run ID。
func writeSnapshot(ctx context.Context, db *sql.DB, input snapshotInput) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rankings: begin snapshot tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先拿咨询锁再读写槽位表：事务级锁在 COMMIT/ROLLBACK 自动释放。
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, rankingsEntityLockKey); err != nil {
		return 0, fmt.Errorf("rankings: acquire entity slot lock: %w", err)
	}

	status := statusOK
	if len(input.Models) == 0 {
		status = statusEmpty
	}
	var runID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO rankings_snapshot_runs
		    (period, window_start, window_end, snapshot_at,
		     sample_threshold, status, model_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		string(input.Period), input.Window.Start, input.Window.End,
		input.SnapshotAt, int(input.SampleThreshold), status, len(input.Models),
	).Scan(&runID); err != nil {
		return 0, fmt.Errorf("rankings: insert snapshot run: %w", err)
	}

	if err := writeModelStats(ctx, tx, runID, input.Models); err != nil {
		return 0, err
	}
	if err := writeProviderStats(ctx, tx, runID, input.Providers); err != nil {
		return 0, err
	}
	if err := writeTimeseries(ctx, tx, input.Timeseries); err != nil {
		return 0, err
	}
	if err := writeAuthorTimeseries(ctx, tx, input.AuthorSeries); err != nil {
		return 0, err
	}

	// 与快照同事务回收旧 run（R-D6-07）。此时新 run 已插入，子查询按
	// snapshot_at DESC, id DESC 保留最近 KeepRuns 个，删掉的旧 run 由
	// CASCADE 带走子表。
	keepRuns := input.KeepRuns
	if keepRuns <= 0 {
		keepRuns = defaultKeepRuns
	}
	if _, err := store.DeleteOlderRunsTx(ctx, tx, string(input.Period), keepRuns); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rankings: commit snapshot: %w", err)
	}
	return runID, nil
}

// writeModelStats 分配模型实体槽位并写入模型行。
func writeModelStats(ctx context.Context, tx *sql.Tx, runID int64, models []store.RankingsModelStat) error {
	if len(models) == 0 {
		return nil
	}
	// 只有能获得独立身份色的实体才落 rankings_entities：色板 8 槽，榜单
	// 之外的实体在图表里归入 Other（R-D6-06「超出归 Other（不落 entity）」）。
	// 模型榜/趋势最多展示 Top 6~10、作者榜前 5 行，因此注册各自前 8 名足够；
	// 超过 8 名的排在 model_stats/provider_stats 里仍可由表格视图展示，
	// 只是不再占用实体槽位。
	names := slotEntities(models, func(m store.RankingsModelStat) string { return m.Model })
	if _, err := assignSlotsTx(ctx, tx, entityKindModel, names); err != nil {
		return err
	}

	for _, m := range models {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rankings_model_stats
			    (run_id, model, display_name, author, total_tokens, requests,
			     rank, prev_rank, delta_tokens, delta_pct)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			runID, m.Model, nullableString(m.DisplayName), nullableString(m.Author),
			m.TotalTokens, m.Requests, nullableInt(m.Rank), nullableInt(m.PrevRank),
			nullableInt64(m.DeltaTokens), nullableFloat(m.DeltaPct),
		); err != nil {
			return fmt.Errorf("rankings: insert model stat %q: %w", m.Model, err)
		}
	}
	return nil
}

// writeProviderStats 分配作者实体槽位并写入作者行。
func writeProviderStats(ctx context.Context, tx *sql.Tx, runID int64, providers []store.RankingsProviderStat) error {
	if len(providers) == 0 {
		return nil
	}
	authors := slotEntities(providers, func(p store.RankingsProviderStat) string { return p.Author })
	if _, err := assignSlotsTx(ctx, tx, entityKindAuthor, authors); err != nil {
		return err
	}

	for _, p := range providers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rankings_provider_stats
			    (run_id, author, total_tokens, share_pct, rank)
			VALUES ($1, $2, $3, $4, $5)`,
			runID, p.Author, p.TotalTokens, nullableFloat(p.SharePct), nullableInt(p.Rank),
		); err != nil {
			return fmt.Errorf("rankings: insert provider stat %q: %w", p.Author, err)
		}
	}
	return nil
}

// writeTimeseries 以 (model, granularity, bucket_start) 为幂等键 upsert 趋势桶。
//
// 【为什么是覆盖而不是累加】同一进程/不同副本都可能对同一桶重复执行快照；
// 累加会双计，覆盖则让「重算同一窗口」得到与首次完全相同的值（usage_logs
// 是 append-only，窗口内的行不会变化，覆盖天然幂等）。历史桶不会出现在本次
// 查询结果里，因此「跨天新建桶、不重写旧桶」由 WHERE 窗口保证。
func writeTimeseries(ctx context.Context, tx *sql.Tx, points []store.RankingsTimeseries) error {
	for _, point := range points {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rankings_timeseries (model, granularity, bucket_start, tokens)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (model, granularity, bucket_start)
			DO UPDATE SET tokens = EXCLUDED.tokens`,
			point.Model, point.Granularity, point.BucketStart, point.Tokens,
		); err != nil {
			return fmt.Errorf("rankings: upsert timeseries %q/%s: %w", point.Model, point.Granularity, err)
		}
	}
	return nil
}

// writeAuthorTimeseries 以 (period, author, granularity, bucket_start) 为
// 幂等键 upsert 作者桶（B5a）。
//
// 与 writeTimeseries 同一取舍：usage_logs 是 append-only，窗口内重算得到
// 相同值，覆盖天然幂等；累加会在多副本/重复执行时双计。period 必须进键：
// week 与 month 共用 day 粒度，否则同一作者同一桶会被另一个周期覆盖。
// 只有作者榜前 5 行会进入 points（buildAuthorSeries 已截断），这里不再判断。
func writeAuthorTimeseries(ctx context.Context, tx *sql.Tx, points []store.RankingsAuthorTimeseries) error {
	for _, point := range points {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rankings_author_timeseries
			    (period, author, granularity, bucket_start, tokens)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (period, author, granularity, bucket_start)
			DO UPDATE SET tokens = EXCLUDED.tokens`,
			point.Period, point.Author, point.Granularity, point.BucketStart, point.Tokens,
		); err != nil {
			return fmt.Errorf("rankings: upsert author timeseries %s/%q/%s: %w",
				point.Period, point.Author, point.Granularity, err)
		}
	}
	return nil
}

// slotEntities 取前 entitySlotCount 个实体的名字（按榜单顺序）。
//
// 只给可能获得独立颜色的前 8 名注册槽位：更多实体在图表里归入 Other，
// 不应消耗有限的色板（R-D6-06）。行顺序是 rank 升序（buildProviders /
// buildModelRows 已排好），因此截断是确定性的。
func slotEntities[T any](rows []T, key func(T) string) []string {
	limit := len(rows)
	if limit > entitySlotCount {
		limit = entitySlotCount
	}
	out := make([]string, 0, limit)
	for _, row := range rows[:limit] {
		out = append(out, key(row))
	}
	return out
}

// assignSlotsTx 为首次出现的实体分配色板槽位（R-D6-06 / platform-01 §17）。
//
// 规则：已注册实体保持原槽位（永不回收/重排）；新实体取 1–8 中**最小空闲槽**，
// 使早期实体的颜色与插入/排名顺序解耦。
//
// 【必须读该 kind 的全部已注册行】`used` 只能由全量注册表得出，不能只看
// 本批 entity：两个 period（today/week）或两轮刷新可能在同一进程里先后
// 调用本函数，本批 top8 与上一批完全不同。若只查 `entity = ANY($2)`，第二
// 批看不到第一批占用的槽位，会把 1–8 再分配一遍，产生重复 (kind, slot)。
//
// 【8 槽用尽后的裁决：slot = NULL】色板只有 8 槽且实体跨 period 永久累积，
// 长期运行必然超过 8 个不同模型/作者。2026-10-20 复核裁决：禁止回绕复用
// （回绕会让两个实体永久同色，且违反「颜色跟随实体」的唯一性），也不重排
// 已有槽位；槽位耗尽后的新实体登记为 slot = NULL，公开面不给他们独立颜色
// （图表归入 Other）。已分配槽位的实体永不回收，因此 NULL 实体以后也不会
// 因为旧实体消失而补位——这保证颜色跨 period 稳定。
func assignSlotsTx(ctx context.Context, tx *sql.Tx, kind string, entities []string) (map[string]*int16, error) {
	unique := make([]string, 0, len(entities))
	seen := make(map[string]struct{}, len(entities))
	for _, entity := range entities {
		if _, ok := seen[entity]; ok {
			continue
		}
		seen[entity] = struct{}{}
		unique = append(unique, entity)
	}
	if len(unique) == 0 {
		return map[string]*int16{}, nil
	}

	// 全量读取该 kind 的注册行：`used` 来自全部非 NULL 槽位，本批实体的
	// 既有槽位同时填入返回值。slot 可空，必须用 sql.NullInt16 扫描。
	rows, err := tx.QueryContext(ctx, `
		SELECT entity, slot FROM rankings_entities
		WHERE kind = $1`, kind)
	if err != nil {
		return nil, fmt.Errorf("rankings: load entity slots: %w", err)
	}
	slots := make(map[string]*int16, len(unique))
	used := make(map[int16]struct{}, entitySlotCount)
	for rows.Next() {
		var (
			entity string
			slot   sql.NullInt16
		)
		if err := rows.Scan(&entity, &slot); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("rankings: scan entity slot: %w", err)
		}
		if _, wanted := seen[entity]; wanted {
			if slot.Valid {
				value := slot.Int16
				slots[entity] = &value
			} else {
				// 已登记 NULL 的实体必须记住「已处理」，否则本批会再次尝试
				// 分配；槽位一旦回收（手动清理旧实体）就可能把它补位，破坏
				// 「NULL 永不复用/不重排」的稳定性。
				slots[entity] = nil
			}
		}
		if !slot.Valid {
			continue // 槽位耗尽的实体不占用色板
		}
		used[slot.Int16] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("rankings: entity slots rows: %w", err)
	}
	_ = rows.Close()

	nextFree := func() (int16, bool) {
		for slot := int16(1); slot <= entitySlotCount; slot++ {
			if _, taken := used[slot]; !taken {
				used[slot] = struct{}{}
				return slot, true
			}
		}
		return 0, false
	}

	for _, entity := range unique {
		if _, ok := slots[entity]; ok {
			continue
		}
		// 槽位耗尽时 slot 保持 nil → 写入 NULL（迁移已放开 NOT NULL）。
		var slot *int16
		if free, ok := nextFree(); ok {
			slot = &free
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rankings_entities (entity, kind, slot)
			VALUES ($1, $2, $3)
			ON CONFLICT (entity, kind) DO NOTHING`, entity, kind, slot); err != nil {
			return nil, fmt.Errorf("rankings: register entity %q: %w", entity, err)
		}
		slots[entity] = slot
	}
	return slots, nil
}

func nullableString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
