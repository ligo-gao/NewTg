package store

// 本文件是 upstream_accounts 表的 SQL 仓储（上游用量对账设计 §4.1）。
//
// 口径：
//   - 一个账号 = 一份可独立对账的账期主体（provider + currency + timezone）；
//   - 渠道通过 channels.upstream_account_id 归属账号，归属关系是历史事实，
//     删除仍被引用的账号必须被数据库拒绝（FK ON DELETE RESTRICT）；
//   - updated_at 同时是管理面 PATCH 的乐观锁版本（镜像 users 端点）。
//
// 与 channel_repo.go 保持同一错误口径：handler 只按 errors.Is 判定业务语义，
// 不解析数据库错误文本。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrUpstreamAccountNotFound 表示按 id 查询/更新/删除时目标行不存在。
	ErrUpstreamAccountNotFound = errors.New("store: upstream account not found")
	// ErrUpstreamAccountConflict 表示乐观锁失配（stored updated_at != expected_updated_at）。
	ErrUpstreamAccountConflict = errors.New("store: upstream account version conflict")
	// ErrUpstreamAccountInUse 表示账号仍被渠道或快照引用（pgcode 23503，
	// FK ON DELETE RESTRICT），handler 把它映射为 409。
	ErrUpstreamAccountInUse = errors.New("store: upstream account still in use")
)

// UpstreamAccount 是 upstream_accounts 的一行。ConsoleURL 可空（nil = 未填写）。
type UpstreamAccount struct {
	ID         int64
	Name       string
	Provider   string
	Currency   string
	Timezone   string
	ConsoleURL *string
	Status     int16
	Note       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// UpstreamAccountPatch 是 PATCH 的部分更新载荷。语义与 ChannelPatch 一致：
//
//	字段为 nil / Set=false → 不修改
//	指针非 nil              → 写入该值
//	Set=true 且指针为 nil   → 写入 SQL NULL（仅 console_url 可空）
//
// ExpectedUpdatedAt 是乐观锁版本；零值表示跳过版本校验（仅供内部调用，
// HTTP 层必须填入客户端回传的 expected_updated_at）。
type UpstreamAccountPatch struct {
	ExpectedUpdatedAt time.Time

	Name          *string
	Provider      *string
	Currency      *string
	Timezone      *string
	ConsoleURLSet bool
	ConsoleURL    *string
	Status        *int16
	Note          *string
}

// UpstreamAccountRepo 是 upstream_accounts 表的 SQL 仓储。
type UpstreamAccountRepo struct{ db *sql.DB }

// NewUpstreamAccountRepo 构造仓储；db 由 platform.OpenDB 提供（pgx stdlib 驱动）。
func NewUpstreamAccountRepo(db *sql.DB) *UpstreamAccountRepo {
	return &UpstreamAccountRepo{db: db}
}

// upstreamAccountColumns 与迁移 20261028090000 的列顺序一一对应，供 scanUpstreamAccount 使用。
const upstreamAccountColumns = `id, name, provider, currency, timezone, console_url,
	status, note, created_at, updated_at`

// scanUpstreamAccount 读取一行；console_url 可空，统一走 sql.NullString。
func scanUpstreamAccount(row scanRow) (*UpstreamAccount, error) {
	var (
		a          UpstreamAccount
		consoleURL sql.NullString
	)
	err := row.Scan(&a.ID, &a.Name, &a.Provider, &a.Currency, &a.Timezone, &consoleURL,
		&a.Status, &a.Note, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.CreatedAt = a.CreatedAt.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	if consoleURL.Valid {
		v := consoleURL.String
		a.ConsoleURL = &v
	}
	return &a, nil
}

// List 返回 offset 分页的账号与总数；status 为 nil 表示不筛选。
// 按 id 升序（稳定顺序，翻页不漂移）。
func (r *UpstreamAccountRepo) List(ctx context.Context, status *int16, page, pageSize int) ([]UpstreamAccount, int64, error) {
	page, pageSize = normalizePage(page, pageSize)

	where := ""
	args := make([]any, 0, 1)
	if status != nil {
		args = append(args, *status)
		where = " WHERE status = $1"
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM upstream_accounts`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count upstream accounts: %w", err)
	}

	query := `SELECT ` + upstreamAccountColumns + ` FROM upstream_accounts` + where +
		fmt.Sprintf(` ORDER BY id ASC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)

	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list upstream accounts: %w", err)
	}
	defer rows.Close()

	items := make([]UpstreamAccount, 0, pageSize)
	for rows.Next() {
		a, err := scanUpstreamAccount(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan upstream account: %w", err)
		}
		items = append(items, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate upstream accounts: %w", err)
	}
	return items, total, nil
}

// ListEnabled 返回全部启用账号（status = 1），供对账 runner 遍历。
// 不排序参数、不分页：账号是小基数配置表，runner 需要全量且稳定的顺序。
func (r *UpstreamAccountRepo) ListEnabled(ctx context.Context) ([]UpstreamAccount, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+upstreamAccountColumns+` FROM upstream_accounts WHERE status = 1 ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled upstream accounts: %w", err)
	}
	defer rows.Close()

	items := make([]UpstreamAccount, 0, 16)
	for rows.Next() {
		a, err := scanUpstreamAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan enabled upstream account: %w", err)
		}
		items = append(items, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate enabled upstream accounts: %w", err)
	}
	return items, nil
}

// Get 按 id 读取一行；不存在返回 ErrUpstreamAccountNotFound。
func (r *UpstreamAccountRepo) Get(ctx context.Context, id int64) (*UpstreamAccount, error) {
	a, err := scanUpstreamAccount(r.db.QueryRowContext(ctx,
		`SELECT `+upstreamAccountColumns+` FROM upstream_accounts WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUpstreamAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get upstream account %d: %w", id, err)
	}
	return a, nil
}

// Create 插入一行并返回数据库生成的 id/created_at/updated_at。
//
// 默认值在应用层补齐而不是依赖 DDL 默认（status 0 会被 CHECK 拒绝）：
// currency = CNY、timezone = Asia/Shanghai、status = 1。
// name 唯一冲突映射为 ErrNameConflict（23505）。
func (r *UpstreamAccountRepo) Create(ctx context.Context, a UpstreamAccount) (*UpstreamAccount, error) {
	if a.Currency == "" {
		a.Currency = "CNY"
	}
	if a.Timezone == "" {
		a.Timezone = "Asia/Shanghai"
	}
	if a.Status == 0 {
		a.Status = 1
	}

	out, err := scanUpstreamAccount(r.db.QueryRowContext(ctx, `INSERT INTO upstream_accounts
		(name, provider, currency, timezone, console_url, status, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+upstreamAccountColumns,
		a.Name, a.Provider, a.Currency, a.Timezone, nullableString(a.ConsoleURL),
		a.Status, a.Note))
	if isUniqueViolation(err) {
		return nil, ErrNameConflict
	}
	if err != nil {
		return nil, fmt.Errorf("store: create upstream account: %w", err)
	}
	return out, nil
}

// Update 执行部分更新；当 ExpectedUpdatedAt 非零时附带乐观锁。
//
// 行为：
//   - 命中 → 返回更新后的整行（updated_at 由 now() 刷新）；
//   - 行存在但版本失配 → ErrUpstreamAccountConflict；
//   - 行不存在 → ErrUpstreamAccountNotFound；
//   - 无字段变更时仍推进 updated_at 并校验版本。
func (r *UpstreamAccountRepo) Update(ctx context.Context, id int64, p UpstreamAccountPatch) (*UpstreamAccount, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 9)
	add := func(expr string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf(expr, len(args)))
	}

	if p.Name != nil {
		add("name = $%d", *p.Name)
	}
	if p.Provider != nil {
		add("provider = $%d", *p.Provider)
	}
	if p.Currency != nil {
		add("currency = $%d", *p.Currency)
	}
	if p.Timezone != nil {
		add("timezone = $%d", *p.Timezone)
	}
	if p.ConsoleURLSet {
		add("console_url = $%d", nullableString(p.ConsoleURL))
	}
	if p.Status != nil {
		add("status = $%d", *p.Status)
	}
	if p.Note != nil {
		add("note = $%d", *p.Note)
	}

	// updated_at 是乐观锁版本：任何写操作都推进它（包括空 PATCH）。
	sets = append(sets, "updated_at = now()")
	args = append(args, id)
	query := `UPDATE upstream_accounts SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(` WHERE id = $%d`, len(args))
	if !p.ExpectedUpdatedAt.IsZero() {
		args = append(args, p.ExpectedUpdatedAt.UTC())
		query += fmt.Sprintf(` AND updated_at = $%d`, len(args))
	}
	query += ` RETURNING ` + upstreamAccountColumns

	out, err := scanUpstreamAccount(r.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, r.classifyMissingUpdate(ctx, id, !p.ExpectedUpdatedAt.IsZero())
	}
	if isUniqueViolation(err) {
		return nil, ErrNameConflict
	}
	if err != nil {
		return nil, fmt.Errorf("store: update upstream account %d: %w", id, err)
	}
	return out, nil
}

// classifyMissingUpdate 区分「行不存在」与「版本失配」。
// 只有带版本校验的调用才需要额外一次存在性查询。
func (r *UpstreamAccountRepo) classifyMissingUpdate(ctx context.Context, id int64, withVersion bool) error {
	if !withVersion {
		return ErrUpstreamAccountNotFound
	}
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM upstream_accounts WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("store: check upstream account %d existence: %w", id, err)
	}
	if !exists {
		return ErrUpstreamAccountNotFound
	}
	return ErrUpstreamAccountConflict
}

// Delete 删除一行。
//
// 仍被渠道或快照引用时数据库以 FK RESTRICT（23503）拒绝，映射为
// ErrUpstreamAccountInUse（历史对账必须以当时的归属为准，不能级联删除）。
func (r *UpstreamAccountRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM upstream_accounts WHERE id = $1`, id)
	if isForeignKeyViolation(err) {
		return ErrUpstreamAccountInUse
	}
	if err != nil {
		return fmt.Errorf("store: delete upstream account %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete upstream account %d rows: %w", id, err)
	}
	if n == 0 {
		return ErrUpstreamAccountNotFound
	}
	return nil
}

// ChannelIDs 返回归属该账号的渠道 id（升序）。
// 账号不存在或未绑定渠道时返回空切片，不返回错误——归属是可选关系，
// runner 只需要一个可传给日志聚合的集合。
func (r *UpstreamAccountRepo) ChannelIDs(ctx context.Context, accountID int64) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM channels WHERE upstream_account_id = $1 ORDER BY id ASC`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list channels for upstream account %d: %w", accountID, err)
	}
	defer rows.Close()

	ids := make([]int64, 0, 8)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan channel id for upstream account %d: %w", accountID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate channels for upstream account %d: %w", accountID, err)
	}
	return ids, nil
}
