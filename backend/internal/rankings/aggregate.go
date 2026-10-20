package rankings

// 排行榜聚合（D6a；modules-09 §C1.7/C1.8、billing-06 §8）。
//
// 【唯一数据源】usage_logs，且只统计 status=1（success）。refused/error/
// refunded 的调用不代表平台真实使用热度；退款请求已经不计费，也不应计入
// token 排名（R-D6-01）。口径固定为 prompt_tokens + completion_tokens，
// 不把 cache_read/write 重复相加——prompt_tokens 已按各上游的语义归一
// （billing/normalize.go 的约定）。
//
// 【阈值】窗口内 requests < N 的模型不进入任何榜单（模型榜、升降榜、趋势、
// 份额一致，R-D6-02 / §C1.8）。默认 N=100，隐私地板 50：单用户的大量调用
// 不能把某个模型推上榜并由此反推出该用户行为。
//
// 【读写分离】聚合 SQL 跑在日志库（usage_logs 所在库），作者/展示名从主库
// 的 model_pricing/model_meta 补齐（模块 06 的公开元数据）。日志库连接不能
// 用于主库事务（platform-03 §3.4 的两库分治）。

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"newtg/gateway/internal/store"
)

// usageLogStatusSuccess 是 usage_logs.status 的成功值（migrations-logs 的
// CHECK 0..3；billing.UsageLogStatusSuccess 同值）。这里用字面量而不是 import
// billing：排行榜只读日志表，不该为四个状态值把计费包拖进依赖图。
const usageLogStatusSuccess = 1

// moverLimit 是升降榜每方向最多条数（F-09-31：各 Top 5）。
const moverLimit = 5

// modelTotal 是 usage_logs 按模型聚合后的原始行（阈值过滤前）。
type modelTotal struct {
	Model    string
	Tokens   int64
	Requests int64
}

// queryModelTotals 在 [window.Start, window.End) 内按模型聚合 token 与请求数。
//
// HAVING COUNT(*) >= minSamples 在 SQL 层先裁剪：year 窗口可能扫到数十万行，
// 把低于隐私地板的模型先丢掉再上传，既减少内存也避免「先取全量再过滤」把
// 低样本模型短暂暴露在进程内存里。
func queryModelTotals(ctx context.Context, db *sql.DB, window Window, minSamples int64) ([]modelTotal, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT model,
		       COALESCE(SUM(COALESCE(prompt_tokens, 0)::bigint
		                  + COALESCE(completion_tokens, 0)::bigint), 0)::bigint,
		       COUNT(*)::bigint
		FROM usage_logs
		WHERE status = $1
		  AND created_at >= $2
		  AND created_at < $3
		GROUP BY model
		HAVING COUNT(*) >= $4`, usageLogStatusSuccess, window.Start, window.End, minSamples)
	if err != nil {
		return nil, fmt.Errorf("rankings: aggregate model totals: %w", err)
	}
	defer rows.Close()

	out := make([]modelTotal, 0, 32)
	for rows.Next() {
		var row modelTotal
		if err := rows.Scan(&row.Model, &row.Tokens, &row.Requests); err != nil {
			return nil, fmt.Errorf("rankings: scan model total: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rankings: model totals rows: %w", err)
	}
	return out, nil
}

// sortModelTotals 按 token 降序、模型名字典序升序排序。
//
// 【为什么用 model 名做 tie-break】排行必须稳定：相同 token 的并列项如果按
// SQL 返回顺序排，数据库的并行计划会让两次快照的名次漂移。§F-09-30 的
// 「排序稳定用 model 名次字母序 tie-break」即此口径。
func sortModelTotals(rows []modelTotal) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Tokens != rows[j].Tokens {
			return rows[i].Tokens > rows[j].Tokens
		}
		return rows[i].Model < rows[j].Model
	})
}

// buildModelRows 把当前/上一窗口的原始聚合转换为带名次与环比的快照行。
//
// 名次规则（F-09-30 异常流程）：token 相同则名次并列，**下一名跳号**
// （例如 1,2,2,4）。这是竞赛排名（competition ranking），不是 1,2,2,3。
//
// 上一窗口同样先过阈值：prev_rank 只对「上一窗口也达到样本门槛」的模型
// 有意义。prev_rank 为 NULL 表示新增或上窗口样本不足，delta_pct 为 NULL
// 表示上窗口 token 为 0（前端显示「新增」）。delta_tokens 在无上一窗口
// 记录时等于当前值（把 prev 视为 0），保留「与本窗口自身等价的绝对量」，
// 便于表格视图展示；环比是否成立只看 delta_pct 是否为 NULL。
func buildModelRows(current, previous []modelTotal, minSamples int64) []store.RankingsModelStat {
	cur := make([]modelTotal, 0, len(current))
	for _, row := range current {
		if row.Requests >= minSamples {
			cur = append(cur, row)
		}
	}
	prev := make([]modelTotal, 0, len(previous))
	for _, row := range previous {
		if row.Requests >= minSamples {
			prev = append(prev, row)
		}
	}
	sortModelTotals(cur)
	sortModelTotals(prev)

	type prevEntry struct {
		rank   int
		tokens int64
	}
	prevByModel := make(map[string]prevEntry, len(prev))
	prevRank := 0
	for i, row := range prev {
		// 竞赛排名：与上一行 token 相同则沿用同一名次，否则名次 = i+1（跳号）。
		if i == 0 || row.Tokens != prev[i-1].Tokens {
			prevRank = i + 1
		}
		prevByModel[row.Model] = prevEntry{rank: prevRank, tokens: row.Tokens}
	}

	out := make([]store.RankingsModelStat, 0, len(cur))
	curRank := 0
	for i, row := range cur {
		if i == 0 || row.Tokens != cur[i-1].Tokens {
			curRank = i + 1
		}
		rank := curRank
		stat := store.RankingsModelStat{
			Model:       row.Model,
			TotalTokens: row.Tokens,
			Requests:    row.Requests,
			Rank:        &rank,
		}
		if entry, ok := prevByModel[row.Model]; ok {
			prevRankValue := entry.rank
			stat.PrevRank = &prevRankValue
			delta := row.Tokens - entry.tokens
			stat.DeltaTokens = &delta
			if entry.tokens > 0 {
				pct := clampDeltaPct(round2(float64(delta) / float64(entry.tokens) * 100))
				stat.DeltaPct = &pct
			}
		} else {
			delta := row.Tokens
			stat.DeltaTokens = &delta
		}
		out = append(out, stat)
	}
	return out
}

// round2 四舍五入到 2 位小数（delta_pct numeric(7,2) / share_pct numeric(5,2)）。
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// maxDeltaPct 是 numeric(7,2) 能表示的最大绝对值（99999.99）。
const maxDeltaPct = 99999.99

// clampDeltaPct 把环比钳制到列宽可表示的区间。
//
// 阈值只要求 requests ≥ N，不要求上一窗口 token 足够大：上一窗口 50 次调用
// 共 50 token、当前窗口涨到 5000 万 token 时，原始环比是 1e8%，numeric(7,2)
// 会直接报 overflow 并让**整个快照事务回滚**——一个模型的异常增长会拖垮
// 其他模型与四个区块的展示。钳制到列上限保留「极度上升」的语义，牺牲的是
// 超出部分的小数精度（已登记为设计缺口）。
func clampDeltaPct(v float64) float64 {
	if v > maxDeltaPct {
		return maxDeltaPct
	}
	if v < -maxDeltaPct {
		return -maxDeltaPct
	}
	return v
}

// authorOf 把可空作者归一为公开面文案：缺失回退 Unknown（F-09-30 分支流程）。
func authorOf(author *string) string {
	if author == nil {
		return "Unknown"
	}
	if *author == "" {
		return "Unknown"
	}
	return *author
}

// buildProviders 从入榜模型聚合作者份额（F-09-33 / platform-01 §15）。
//
// 【份额合计必须恰为 100.00】percent 是四舍五入后的展示值，逐项 round 后
// 合计会出现 ±0.01~±0.0n 的误差。服务端在**最后一个（token 最少）作者**上
// 吸收差额，保证前端不做二次修正（F-09-33 异常流程明示「末位作者吸收误差」）。
//
// 排名按 total_tokens 降序、作者名字典序 tie-break；并列作者共享名次并跳号，
// 与模型榜同一口径。
func buildProviders(models []store.RankingsModelStat) []store.RankingsProviderStat {
	if len(models) == 0 {
		return nil
	}
	type authorTotal struct {
		author string
		tokens int64
	}
	byAuthor := make(map[string]int64, 8)
	var total int64
	for _, m := range models {
		name := authorOf(m.Author)
		byAuthor[name] += m.TotalTokens
		total += m.TotalTokens
	}
	if total <= 0 {
		return nil
	}

	rows := make([]authorTotal, 0, len(byAuthor))
	for author, tokens := range byAuthor {
		rows = append(rows, authorTotal{author: author, tokens: tokens})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].tokens != rows[j].tokens {
			return rows[i].tokens > rows[j].tokens
		}
		return rows[i].author < rows[j].author
	})

	// 以「万分之一」为单位做整数运算，避免 float 累计误差；最后一项吸收残差。
	bps := make([]int64, len(rows))
	var sum int64
	for i, row := range rows {
		bps[i] = int64(math.Round(float64(row.tokens) / float64(total) * 10000))
		sum += bps[i]
	}
	bps[len(bps)-1] += 10000 - sum

	out := make([]store.RankingsProviderStat, 0, len(rows))
	rank := 0
	for i, row := range rows {
		if i == 0 || row.tokens != rows[i-1].tokens {
			rank = i + 1
		}
		rankValue := rank
		share := float64(bps[i]) / 100
		out = append(out, store.RankingsProviderStat{
			Author:      row.author,
			TotalTokens: row.tokens,
			SharePct:    &share,
			Rank:        &rankValue,
		})
	}
	return out
}

// Mover 是升降榜的一项（F-09-31 响应结构的数据侧）。
type Mover struct {
	Model      string
	Author     string
	DeltaPct   float64
	CurTokens  int64
	PrevTokens int64
}

// Movers 是升降榜的双向结果。
type Movers struct {
	Up   []Mover
	Down []Mover
}

// ComputeMovers 从快照行派生升降榜。
//
// 【双窗口门槛】只有 PrevRank 与 DeltaPct 都非 NULL 的模型才进入候选：
// 这等价于「当前窗口与上一窗口都 ≥ N」（R-D6-02 / §C1.8-4）。prev_rank 为
// NULL 说明上一窗口样本不足或模型是新增的——把「从 0 到 N」的突刺放进
// 榜单会暴露个别用户/单次批量调用，所以直接排除，而不是当作 +∞ 上升。
//
// up 按 delta_pct 降序、down 按升序（最负在前），同值按模型名稳定排序。
func ComputeMovers(models []store.RankingsModelStat) Movers {
	up := make([]Mover, 0, moverLimit)
	down := make([]Mover, 0, moverLimit)
	for _, m := range models {
		if m.PrevRank == nil || m.DeltaPct == nil || m.DeltaTokens == nil {
			continue
		}
		if *m.DeltaPct == 0 {
			continue
		}
		mover := Mover{
			Model:      m.Model,
			Author:     authorOf(m.Author),
			DeltaPct:   *m.DeltaPct,
			CurTokens:  m.TotalTokens,
			PrevTokens: m.TotalTokens - *m.DeltaTokens,
		}
		if *m.DeltaPct > 0 {
			up = append(up, mover)
		} else {
			down = append(down, mover)
		}
	}
	sort.Slice(up, func(i, j int) bool {
		if up[i].DeltaPct != up[j].DeltaPct {
			return up[i].DeltaPct > up[j].DeltaPct
		}
		return up[i].Model < up[j].Model
	})
	sort.Slice(down, func(i, j int) bool {
		if down[i].DeltaPct != down[j].DeltaPct {
			return down[i].DeltaPct < down[j].DeltaPct
		}
		return down[i].Model < down[j].Model
	})
	if len(up) > moverLimit {
		up = up[:moverLimit]
	}
	if len(down) > moverLimit {
		down = down[:moverLimit]
	}
	return Movers{Up: up, Down: down}
}

// queryTimeseries 按趋势粒度聚合本次窗口内的桶。
//
// 只对已入榜模型取桶（models 参数）：趋势图只展示 Top 模型 + Other，
// 未达阈值的模型既不公开也不应无限累积到跨 run 的 timeseries 表里。
//
// 桶起点按**站点时区**对齐（date_trunc 在 created_at AT TIME ZONE tz 的墙上
// 时间上执行），再在 Go 里用同一 location 转回 UTC 落库（platform-01 §16）。
// 跨天/跨周只写当前窗口覆盖的桶；历史桶不重写（窗口外的行被 WHERE 排除）。
func queryTimeseries(
	ctx context.Context,
	db *sql.DB,
	window Window,
	loc *time.Location,
	granularity Granularity,
	models []string,
) ([]store.RankingsTimeseries, error) {
	if len(models) == 0 || !window.Valid() {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT model,
		       to_char(date_trunc($1::text, created_at AT TIME ZONE $2),
		               'YYYY-MM-DD HH24:MI:SS') AS bucket,
		       COALESCE(SUM(COALESCE(prompt_tokens, 0)::bigint
		                  + COALESCE(completion_tokens, 0)::bigint), 0)::bigint
		FROM usage_logs
		WHERE status = $3
		  AND created_at >= $4
		  AND created_at < $5
		  AND model = ANY($6)
		GROUP BY model, bucket
		ORDER BY bucket, model`,
		string(granularity), loc.String(), usageLogStatusSuccess,
		window.Start, window.End, models)
	if err != nil {
		return nil, fmt.Errorf("rankings: aggregate timeseries: %w", err)
	}
	defer rows.Close()

	const bucketLayout = "2006-01-02 15:04:05"
	out := make([]store.RankingsTimeseries, 0, 64)
	for rows.Next() {
		var (
			point  store.RankingsTimeseries
			bucket string
		)
		if err := rows.Scan(&point.Model, &bucket, &point.Tokens); err != nil {
			return nil, fmt.Errorf("rankings: scan timeseries: %w", err)
		}
		// ParseInLocation 把「站点时区的墙上时间」还原为 UTC 时刻；
		// date_trunc 已经对齐到自然桶起点，这里只做时区转换。
		start, err := time.ParseInLocation(bucketLayout, bucket, loc)
		if err != nil {
			return nil, fmt.Errorf("rankings: parse timeseries bucket %q: %w", bucket, err)
		}
		point.Granularity = string(granularity)
		point.BucketStart = start.UTC()
		out = append(out, point)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rankings: timeseries rows: %w", err)
	}
	return out, nil
}

// enrichModelMetadata 从主库补齐展示名与作者。
//
// 【映射口径】author 取 model_pricing.author（模块 06 的公开模型作者，
// NOT NULL DEFAULT 'Unknown'）；display_name 取 model_meta.display_name
// （可空，缺失时保持 NULL，由 DTO 回退为 model）。model_pricing 里没有的
// 模型（只看过调用但从未定价）仍可上榜：author 回退 Unknown——排行榜衡量
// 的是真实调用量，不因目录缺档把模型剔除（F-09-30「author 缺失仍参与排名」）。
func enrichModelMetadata(ctx context.Context, db *sql.DB, models []store.RankingsModelStat) error {
	if len(models) == 0 {
		return nil
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Model)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT p.model,
		       NULLIF(btrim(p.author), ''),
		       m.display_name
		FROM model_pricing p
		LEFT JOIN model_meta m ON m.model_name = p.model
		WHERE p.model = ANY($1)`, names)
	if err != nil {
		return fmt.Errorf("rankings: load model metadata: %w", err)
	}
	defer rows.Close()

	type meta struct {
		author      *string
		displayName *string
	}
	found := make(map[string]meta, len(models))
	for rows.Next() {
		var (
			model       string
			author      sql.NullString
			displayName sql.NullString
		)
		if err := rows.Scan(&model, &author, &displayName); err != nil {
			return fmt.Errorf("rankings: scan model metadata: %w", err)
		}
		var entry meta
		if author.Valid {
			value := author.String
			entry.author = &value
		}
		if displayName.Valid && displayName.String != "" {
			value := displayName.String
			entry.displayName = &value
		}
		found[model] = entry
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rankings: model metadata rows: %w", err)
	}

	for i := range models {
		entry, ok := found[models[i].Model]
		if !ok {
			unknown := "Unknown"
			models[i].Author = &unknown
			continue
		}
		if entry.author != nil {
			models[i].Author = entry.author
		} else {
			unknown := "Unknown"
			models[i].Author = &unknown
		}
		models[i].DisplayName = entry.displayName
	}
	return nil
}

// aggregate 完成一个周期的全部只读聚合（不含写库）。
type aggregation struct {
	Window     Window
	Models     []store.RankingsModelStat
	Providers  []store.RankingsProviderStat
	Timeseries []store.RankingsTimeseries
	Threshold  int64
}

// aggregatePeriod 读取当前/上一窗口并构造快照内容。
func aggregatePeriod(ctx context.Context, mainDB, logsDB *sql.DB, period Period, now time.Time, loc *time.Location, minSamples int64) (aggregation, error) {
	window := CurrentWindow(period, now, loc)
	previous := PreviousWindow(period, now, loc)

	currentTotals, err := queryModelTotals(ctx, logsDB, window, minSamples)
	if err != nil {
		return aggregation{}, err
	}
	var previousTotals []modelTotal
	if previous.Valid() {
		previousTotals, err = queryModelTotals(ctx, logsDB, previous, minSamples)
		if err != nil {
			return aggregation{}, err
		}
	}

	models := buildModelRows(currentTotals, previousTotals, minSamples)
	if err := enrichModelMetadata(ctx, mainDB, models); err != nil {
		return aggregation{}, err
	}

	modelNames := make([]string, 0, len(models))
	for _, m := range models {
		modelNames = append(modelNames, m.Model)
	}
	series, err := queryTimeseries(ctx, logsDB, window, loc, period.Granularity(), modelNames)
	if err != nil {
		return aggregation{}, err
	}

	return aggregation{
		Window:     window,
		Models:     models,
		Providers:  buildProviders(models),
		Timeseries: series,
		Threshold:  minSamples,
	}, nil
}
