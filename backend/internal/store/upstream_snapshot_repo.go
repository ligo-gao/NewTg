package store

// 本文件是 upstream_usage_snapshots 表的 SQL 仓储（上游用量对账设计 §4.3）。
//
// 口径：
//   - 一行 = 某账号 × 某自然日 × 某上游模型的控制台账面数；
//   - 每个 token/amount 列都可空：NULL = 控制台没有提供该维度，0 = 真实零，
//     两者判定语义不同，仓储层必须原样保留（不能把 nil 归一成 0）；
//   - (account_id, usage_date, upstream_model) 唯一，重复导入按 upsert 覆盖；
//   - 「一天」由调用方按账号时区算好后以 date 传入，仓储层不做 24h 运算。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UpstreamSnapshot 是 upstream_usage_snapshots 的一行。
// 指针字段为 nil 表示控制台未提供该维度。
type UpstreamSnapshot struct {
	ID               int64
	AccountID        int64
	UsageDate        time.Time
	UpstreamModel    string
	Currency         string
	Source           string
	CacheHitTokens   *int64
	CacheMissTokens  *int64
	CacheWriteTokens *int64
	OutputTokens     *int64
	Requests         *int64
	AmountMinor      *int64
	EnteredBy        *int64
	EnteredAt        time.Time
	Note             string
}

// UpstreamSnapshotRepo 是 upstream_usage_snapshots 表的 SQL 仓储。
type UpstreamSnapshotRepo struct{ db *sql.DB }

// NewUpstreamSnapshotRepo 构造仓储；db 由 platform.OpenDB 提供（pgx stdlib 驱动）。
func NewUpstreamSnapshotRepo(db *sql.DB) *UpstreamSnapshotRepo {
	return &UpstreamSnapshotRepo{db: db}
}

// upstreamSnapshotColumns 与迁移 20261028090100 的列顺序一一对应。
const upstreamSnapshotColumns = `id, account_id, usage_date, upstream_model,
	input_cache_hit_tokens, input_cache_miss_tokens, cache_write_tokens, output_tokens,
	requests, amount_minor, currency, source, entered_by, entered_at, note`

// scanUpstreamSnapshot 读取一行。可空列统一走 sql.NullInt64，保留 NULL 语义。
func scanUpstreamSnapshot(row scanRow) (*UpstreamSnapshot, error) {
	var (
		s                     UpstreamSnapshot
		usageDate             time.Time
		hit, miss, write, out sql.NullInt64
		requests, amount      sql.NullInt64
		enteredBy             sql.NullInt64
	)
	err := row.Scan(&s.ID, &s.AccountID, &usageDate, &s.UpstreamModel,
		&hit, &miss, &write, &out, &requests, &amount,
		&s.Currency, &s.Source, &enteredBy, &s.EnteredAt, &s.Note)
	if err != nil {
		return nil, err
	}
	// date 在 pgx 下以 UTC 零点返回；显式归一避免调用方拿到的位置不一致。
	s.UsageDate = usageDate.UTC()
	s.EnteredAt = s.EnteredAt.UTC()
	s.CacheHitTokens = nullInt64Ptr(hit)
	s.CacheMissTokens = nullInt64Ptr(miss)
	s.CacheWriteTokens = nullInt64Ptr(write)
	s.OutputTokens = nullInt64Ptr(out)
	s.Requests = nullInt64Ptr(requests)
	s.AmountMinor = nullInt64Ptr(amount)
	s.EnteredBy = nullInt64Ptr(enteredBy)
	return &s, nil
}

// snapshotKey 是快照的业务唯一键。
type snapshotKey struct {
	accountID int64
	date      string
	model     string
}

// dedupeSnapshots 折叠同一次批量写入中的重复键（后出现者胜出）。
//
// 批量 INSERT ... ON CONFLICT 无法在同一语句里两次命中同一行
// （PostgreSQL 报「ON CONFLICT DO UPDATE command cannot affect row a second time」），
// 所以重复键必须先在应用层折叠。顺序按「最后一次出现」的位置保留，
// 返回值长度即实际写入的行数。
func dedupeSnapshots(items []UpstreamSnapshot) []UpstreamSnapshot {
	out := make([]UpstreamSnapshot, 0, len(items))
	index := make(map[snapshotKey]int, len(items))
	for _, item := range items {
		key := snapshotKey{accountID: item.AccountID, date: formatSQLDate(item.UsageDate), model: item.UpstreamModel}
		if pos, ok := index[key]; ok {
			out[pos] = item
			continue
		}
		index[key] = len(out)
		out = append(out, item)
	}
	return out
}

// Upsert 批量写入快照，冲突键按「后写覆盖」更新，返回实际写入行数。
//
// 所有行在同一事务内写入：一次批量导入要么全部生效，要么全部回滚。
func (r *UpstreamSnapshotRepo) Upsert(ctx context.Context, items []UpstreamSnapshot) (int, error) {
	items = dedupeSnapshots(items)
	if len(items) == 0 {
		return 0, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: upsert upstream snapshots begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const perRow = 14
	valueRows := make([]string, 0, len(items))
	args := make([]any, 0, len(items)*perRow)
	for _, item := range items {
		currency := item.Currency
		if currency == "" {
			currency = "CNY"
		}
		source := item.Source
		if source == "" {
			source = "manual"
		}
		enteredAt := item.EnteredAt
		if enteredAt.IsZero() {
			enteredAt = time.Now()
		}
		base := len(args)
		valueRows = append(valueRows, fmt.Sprintf(
			"($%d,$%d::date,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7,
			base+8, base+9, base+10, base+11, base+12, base+13, base+14))
		args = append(args,
			item.AccountID, formatSQLDate(item.UsageDate), item.UpstreamModel,
			nullableInt64(item.CacheHitTokens), nullableInt64(item.CacheMissTokens),
			nullableInt64(item.CacheWriteTokens), nullableInt64(item.OutputTokens),
			nullableInt64(item.Requests), nullableInt64(item.AmountMinor),
			currency, source, nullableInt64(item.EnteredBy), enteredAt.UTC(), item.Note)
	}

	query := `INSERT INTO upstream_usage_snapshots
		(account_id, usage_date, upstream_model,
		 input_cache_hit_tokens, input_cache_miss_tokens, cache_write_tokens, output_tokens,
		 requests, amount_minor, currency, source, entered_by, entered_at, note)
		VALUES ` + strings.Join(valueRows, ", ") + `
		ON CONFLICT (account_id, usage_date, upstream_model) DO UPDATE SET
			input_cache_hit_tokens = EXCLUDED.input_cache_hit_tokens,
			input_cache_miss_tokens = EXCLUDED.input_cache_miss_tokens,
			cache_write_tokens = EXCLUDED.cache_write_tokens,
			output_tokens = EXCLUDED.output_tokens,
			requests = EXCLUDED.requests,
			amount_minor = EXCLUDED.amount_minor,
			currency = EXCLUDED.currency,
			source = EXCLUDED.source,
			entered_by = EXCLUDED.entered_by,
			entered_at = EXCLUDED.entered_at,
			note = EXCLUDED.note,
			updated_at = now()`

	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		if isForeignKeyViolation(err) {
			return 0, ErrUpstreamAccountNotFound
		}
		return 0, fmt.Errorf("store: upsert upstream snapshots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: upsert upstream snapshots commit: %w", err)
	}
	return len(items), nil
}

// ListRange 返回 [from, to]（含两端，按日期）内的快照，按日期与模型升序。
//
// from/to 是调用方按账号时区换算后的自然日 date；仓储层只做字面日期比较，
// 不做任何时区/24h 运算。
func (r *UpstreamSnapshotRepo) ListRange(ctx context.Context, accountID int64, from, to time.Time) ([]UpstreamSnapshot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+upstreamSnapshotColumns+`
		FROM upstream_usage_snapshots
		WHERE account_id = $1 AND usage_date >= $2::date AND usage_date <= $3::date
		ORDER BY usage_date ASC, upstream_model ASC`, accountID, formatSQLDate(from), formatSQLDate(to))
	if err != nil {
		return nil, fmt.Errorf("store: list upstream snapshots for account %d: %w", accountID, err)
	}
	defer rows.Close()

	items := make([]UpstreamSnapshot, 0, 64)
	for rows.Next() {
		s, err := scanUpstreamSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan upstream snapshot: %w", err)
		}
		items = append(items, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate upstream snapshots: %w", err)
	}
	return items, nil
}

// formatSQLDate 把「调用方时区里的某一天」格式化为 ISO date 字面值。
//
// 不能用 time.Time 直接绑定 date 列：pgx 会把它编码为 timestamptz，随后
// PostgreSQL 按会话时区截断，跨时区时可能落到前一天。Format 保留传入
// time.Time 自身的墙上时间，正是调用方计算的日界。
func formatSQLDate(t time.Time) string {
	return t.Format("2006-01-02")
}

// nullableInt64 把 *int64 转成可写入 SQL 的值，nil → NULL。
func nullableInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
