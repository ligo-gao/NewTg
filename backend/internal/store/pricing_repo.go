package store

// 定价管理面的 PostgreSQL 仓储（modules-06 F-06-13/14/16，platform-01 §7/§8）。
//
// 【为什么放在 store 包而不是 httpapi】定价行是计费热路径与公开目录的共同数据源；
// 把读写收敛到 store 后，管理端点只依赖一个接缝，Task 3 的 pricing.DBSource 与
// 管理面读的是同一张表/同一套列语义。httpapi 定义消费侧接口会迫使 store 反向
// 依赖 httpapi（或复制类型），因此类型与接口都放在本包。
//
// 【R9：显式零用 NULL 承载】model_pricing 的四个价列可空：NULL=未设置（按
// billing-02 §3.2 兜底），0=管理员显式免费。任何扫描/写入都必须保留三值语义，
// 不得把 NULL 折叠成 0。
//
// 【R10：model 是自然键】model_pricing 有代理 id，但 group_pricing 以
// ("group", model) 联合唯一；管理端点按 spec 以 model 寻址。因此这里以 model
// 作为更新/删除条件，id 只用于响应。
//
// 【乐观锁】所有更新/删除都带 `updated_at = $expect` 条件；并发下 RowsAffected=0
// 后必须二次查询区分「行不存在（404）」与「版本过期（409）」，不得一律当 404。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 定价仓储的哨兵错误，供 httpapi 映射 404/409/422。
var (
	// ErrPricingNotFound：model_pricing / group_pricing 行不存在。
	ErrPricingNotFound = errors.New("store: pricing row not found")
	// ErrPricingConflict：updated_at 不匹配（乐观锁冲突）。
	ErrPricingConflict = errors.New("store: pricing row was modified concurrently")
	// ErrPricingDuplicate：唯一键冲突（model 或 (group, model)）。
	ErrPricingDuplicate = errors.New("store: pricing row already exists")
	// ErrPricingInvalidGroup：分组不存在或已停用。
	ErrPricingInvalidGroup = errors.New("store: group does not exist or is disabled")
	// ErrPricingInvalidModel：分组覆盖价引用的源模型未定价。
	ErrPricingInvalidModel = errors.New("store: source model is not priced")
	// ErrPricingSourceIsExpression：源模型使用 tiered_expr，表达式自包含全部
	// 计价，覆盖价在该模式下不会被读取——写入它只会制造「看起来生效」的假象。
	ErrPricingSourceIsExpression = errors.New("store: source model uses expression pricing; group overrides have no effect")
	// ErrPricingPerRequestFields：per_request 源模型只读取 prompt_price（每次
	// 请求价），completion/cache 覆盖价无意义。
	ErrPricingPerRequestFields = errors.New("store: per_request source ignores completion/cache override prices")
)

// ModelPricingRecord 是 model_pricing 的完整读取投影（13 列）。
type ModelPricingRecord struct {
	ID              int64
	Model           string
	Mode            int16
	BillingExpr     *string
	PromptPrice     *int64
	CompletionPrice *int64
	CacheReadPrice  *int64
	CacheWritePrice *int64
	Author          string
	Vendor          *string
	Status          int16
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// GroupPricingRecord 是 group_pricing 的读取投影，附带源模型的派生信息
// （列表页需要展示「计价模式随源模型」与「源定价已停用」警告）。
type GroupPricingRecord struct {
	ID              int64
	Group           string
	Model           string
	PromptPrice     int64
	CompletionPrice int64
	CacheReadPrice  int64
	CacheWritePrice int64
	Status          int16
	// SourceMode 是源 model_pricing 的派生模式；源行缺失时为空串。
	SourceMode string
	// SourceStatus 是源 model_pricing 的状态；源行缺失时为 0。
	SourceStatus int16
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PricingModelOption 是模型选择器的轻量投影。
type PricingModelOption struct {
	Model  string
	Mode   string
	Status int16
}

// ModelPricingFilter 是模型定价列表的筛选条件。
type ModelPricingFilter struct {
	Page     int
	PageSize int
	Keyword  string
	// Mode 取 per_token / per_request / tiered_expr；空串表示不筛选。
	Mode   string
	Status *int16
	Sort   string
}

// PricingModelFilter 是模型选择器的筛选条件。
type PricingModelFilter struct {
	Keyword string
	Status  *int16 // nil 表示全部状态
	Limit   int
}

// GroupPricingFilter 是分组覆盖价列表的筛选条件。
type GroupPricingFilter struct {
	Page     int
	PageSize int
	Group    string
	Keyword  string // 模型关键字
	Status   *int16
	Sort     string
}

// ModelPricingInput 是创建模型定价的写入值。
//
// 可空价列直接使用指针：nil 落库为 NULL（未设置），非 nil（含 0）为显式价。
type ModelPricingInput struct {
	Model           string
	Mode            int16
	BillingExpr     *string
	PromptPrice     *int64
	CompletionPrice *int64
	CacheReadPrice  *int64
	CacheWritePrice *int64
	Author          string
	Vendor          *string
	Status          int16
}

// ModelPricingPatch 是模型定价的部分更新。Set 标记区分「未提交」与「显式置空」。
type ModelPricingPatch struct {
	Mode *int16

	BillingExpr    *string
	BillingExprSet bool

	PromptPrice    *int64
	PromptPriceSet bool

	CompletionPrice    *int64
	CompletionPriceSet bool

	CacheReadPrice    *int64
	CacheReadPriceSet bool

	CacheWritePrice    *int64
	CacheWritePriceSet bool

	Author *string
	Vendor *string
	// VendorSet 为 true 且 Vendor 为 nil 时把 vendor 置 NULL。
	VendorSet bool
	Status    *int16
}

// GroupPricingInput 是创建分组覆盖价的写入值。四个价列 NOT NULL（默认 0）。
type GroupPricingInput struct {
	Group           string
	Model           string
	PromptPrice     int64
	CompletionPrice int64
	CacheReadPrice  int64
	CacheWritePrice int64
	Status          int16
}

// GroupPricingPatch 是分组覆盖价的部分更新。
type GroupPricingPatch struct {
	PromptPrice     *int64
	CompletionPrice *int64
	CacheReadPrice  *int64
	CacheWritePrice *int64
	Status          *int16
}

// PricingRepo 是定价管理端点的存储接缝（单测注入内存假实现）。
type PricingRepo interface {
	ListModelPricing(ctx context.Context, filter ModelPricingFilter) ([]ModelPricingRecord, int64, error)
	GetModelPricing(ctx context.Context, model string) (ModelPricingRecord, error)
	CreateModelPricing(ctx context.Context, in ModelPricingInput) (ModelPricingRecord, error)
	UpdateModelPricing(ctx context.Context, model string, patch ModelPricingPatch, expectUpdatedAt time.Time) (ModelPricingRecord, error)
	DeleteModelPricing(ctx context.Context, model string, expectUpdatedAt time.Time) error
	SetModelPricingStatus(ctx context.Context, model string, status int16, expectUpdatedAt time.Time) (ModelPricingRecord, error)
	ListPricingModels(ctx context.Context, filter PricingModelFilter) ([]PricingModelOption, error)

	ListGroupPricing(ctx context.Context, filter GroupPricingFilter) ([]GroupPricingRecord, int64, error)
	CreateGroupPricing(ctx context.Context, in GroupPricingInput) (GroupPricingRecord, error)
	UpdateGroupPricing(ctx context.Context, group, model string, patch GroupPricingPatch, expectUpdatedAt time.Time) (GroupPricingRecord, error)
	DeleteGroupPricing(ctx context.Context, group, model string, expectUpdatedAt time.Time) error
	SetGroupPricingStatus(ctx context.Context, group, model string, status int16, expectUpdatedAt time.Time) (GroupPricingRecord, error)
}

// SQLPricingRepo 是 PricingRepo 的 PostgreSQL 实现（database/sql + pgx stdlib）。
type SQLPricingRepo struct {
	db *sql.DB
}

// NewPricingRepo 构造定价仓储。db 必须非 nil：定价管理没有可用的内存兜底，
// 静默降级会让管理端以为写入成功。
func NewPricingRepo(db *sql.DB) *SQLPricingRepo {
	return &SQLPricingRepo{db: db}
}

const modelPricingColumns = `id, model, mode, billing_expr, prompt_price,
	completion_price, cache_read_price, cache_write_price, author, vendor,
	status, created_at, updated_at`

func scanModelPricing(row interface{ Scan(...any) error }) (ModelPricingRecord, error) {
	var rec ModelPricingRecord
	err := row.Scan(
		&rec.ID, &rec.Model, &rec.Mode, &rec.BillingExpr, &rec.PromptPrice,
		&rec.CompletionPrice, &rec.CacheReadPrice, &rec.CacheWritePrice,
		&rec.Author, &rec.Vendor, &rec.Status, &rec.CreatedAt, &rec.UpdatedAt,
	)
	return rec, err
}

// modelPricingWhere 生成列表 WHERE 子句与参数（$1 起）。
func modelPricingWhere(filter ModelPricingFilter) (string, []any) {
	where := " WHERE 1=1"
	args := make([]any, 0, 4)
	switch filter.Mode {
	case "tiered_expr":
		where += " AND billing_expr IS NOT NULL"
	case "per_token":
		where += " AND billing_expr IS NULL AND mode = 0"
	case "per_request":
		where += " AND billing_expr IS NULL AND mode = 1"
	}
	if keyword := filter.Keyword; keyword != "" {
		args = append(args, keyword)
		// 前缀匹配用 strpos 而不是 LIKE：模型名里可能含 %/_，转义规则容易漏；
		// strpos 只做字面前缀，语义与 UI 的「模型名前缀模糊」一致。
		where += fmt.Sprintf(" AND strpos(lower(model), lower($%d)) = 1", len(args))
	}
	if filter.Status != nil {
		args = append(args, *filter.Status)
		where += fmt.Sprintf(" AND status = $%d", len(args))
	}
	return where, args
}

func modelPricingOrder(sort string) string {
	switch sort {
	case "-model":
		return " ORDER BY model DESC"
	case "prompt_price":
		return " ORDER BY prompt_price ASC NULLS LAST, model ASC"
	case "-prompt_price":
		return " ORDER BY prompt_price DESC NULLS LAST, model ASC"
	case "updated_at":
		return " ORDER BY updated_at ASC, model ASC"
	case "-updated_at":
		return " ORDER BY updated_at DESC, model ASC"
	default:
		return " ORDER BY model ASC"
	}
}

// ListModelPricing 实现 offset 分页列表。total 与 items 使用同一 WHERE，
// 避免筛选项在计数/取页两处漂移。
func (r *SQLPricingRepo) ListModelPricing(ctx context.Context, filter ModelPricingFilter) ([]ModelPricingRecord, int64, error) {
	where, args := modelPricingWhere(filter)

	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM model_pricing`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count model_pricing: %w", err)
	}

	offset := (filter.Page - 1) * filter.PageSize
	query := `SELECT ` + modelPricingColumns + ` FROM model_pricing` + where +
		modelPricingOrder(filter.Sort) + fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, filter.PageSize, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list model_pricing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]ModelPricingRecord, 0, filter.PageSize)
	for rows.Next() {
		rec, scanErr := scanModelPricing(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("store: scan model_pricing: %w", scanErr)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate model_pricing: %w", err)
	}
	return items, total, nil
}

// GetModelPricing 按自然键读取一行。
func (r *SQLPricingRepo) GetModelPricing(ctx context.Context, model string) (ModelPricingRecord, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+modelPricingColumns+` FROM model_pricing WHERE model = $1`, model)
	rec, err := scanModelPricing(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ModelPricingRecord{}, ErrPricingNotFound
	}
	if err != nil {
		return ModelPricingRecord{}, fmt.Errorf("store: get model_pricing(%s): %w", model, err)
	}
	return rec, nil
}

// CreateModelPricing 插入一行并返回数据库生成的 id/时间戳。
func (r *SQLPricingRepo) CreateModelPricing(ctx context.Context, in ModelPricingInput) (ModelPricingRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO model_pricing
			(model, mode, billing_expr, prompt_price, completion_price,
			 cache_read_price, cache_write_price, author, vendor, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+modelPricingColumns,
		in.Model, in.Mode, in.BillingExpr, in.PromptPrice, in.CompletionPrice,
		in.CacheReadPrice, in.CacheWritePrice, in.Author, in.Vendor, in.Status)
	rec, err := scanModelPricing(row)
	if err == nil {
		return rec, nil
	}
	if isUniqueViolation(err) {
		return ModelPricingRecord{}, ErrPricingDuplicate
	}
	return ModelPricingRecord{}, fmt.Errorf("store: create model_pricing(%s): %w", in.Model, err)
}

// UpdateModelPricing 按 model + updated_at 条件更新（乐观锁）。
//
// 动态 SET 的列名来自本函数内部的固定映射，绝不接受调用方字符串拼接。
func (r *SQLPricingRepo) UpdateModelPricing(ctx context.Context, model string, patch ModelPricingPatch, expectUpdatedAt time.Time) (ModelPricingRecord, error) {
	sets := make([]string, 0, 10)
	args := make([]any, 0, 12)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if patch.Mode != nil {
		add("mode", *patch.Mode)
	}
	if patch.BillingExprSet {
		add("billing_expr", patch.BillingExpr)
	}
	if patch.PromptPriceSet {
		add("prompt_price", patch.PromptPrice)
	}
	if patch.CompletionPriceSet {
		add("completion_price", patch.CompletionPrice)
	}
	if patch.CacheReadPriceSet {
		add("cache_read_price", patch.CacheReadPrice)
	}
	if patch.CacheWritePriceSet {
		add("cache_write_price", patch.CacheWritePrice)
	}
	if patch.Author != nil {
		add("author", *patch.Author)
	}
	if patch.VendorSet {
		add("vendor", patch.Vendor)
	}
	if patch.Status != nil {
		add("status", *patch.Status)
	}
	// updated_at 由数据库 now() 推进；调用方传入的 expected 只用于 WHERE。
	sets = append(sets, "updated_at = now()")
	args = append(args, model, expectUpdatedAt.UTC())

	query := `UPDATE model_pricing SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(` WHERE model = $%d AND updated_at = $%d RETURNING `, len(args)-1, len(args)) +
		modelPricingColumns
	rec, err := scanModelPricing(r.db.QueryRowContext(ctx, query, args...))
	if err == nil {
		return rec, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ModelPricingRecord{}, r.missingOrConflict(ctx, model)
	}
	if isUniqueViolation(err) {
		return ModelPricingRecord{}, ErrPricingDuplicate
	}
	return ModelPricingRecord{}, fmt.Errorf("store: update model_pricing(%s): %w", model, err)
}

// missingOrConflict 在 UPDATE/DELETE 未命中后区分 404 与 409。
func (r *SQLPricingRepo) missingOrConflict(ctx context.Context, model string) error {
	var exists bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM model_pricing WHERE model = $1)`, model).Scan(&exists)
	if err != nil {
		return fmt.Errorf("store: check model_pricing(%s): %w", model, err)
	}
	if !exists {
		return ErrPricingNotFound
	}
	return ErrPricingConflict
}

// DeleteModelPricing 删除模型定价，并在同一事务内清理其分组覆盖价。
//
// 【为什么级联删除覆盖价】group_pricing.model 是逻辑引用（无 FK）。若留下孤儿
// 覆盖价，重新创建同名模型会「复活」旧覆盖价，管理员看到的是一份从未新配置过的
// 价格。覆盖价脱离源定价没有意义，故与源行同事务删除，避免半删状态。
func (r *SQLPricingRepo) DeleteModelPricing(ctx context.Context, model string, expectUpdatedAt time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete model_pricing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx,
		`DELETE FROM model_pricing WHERE model = $1 AND updated_at = $2`,
		model, expectUpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: delete model_pricing(%s): %w", model, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete model_pricing(%s) rows: %w", model, err)
	}
	if affected == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM model_pricing WHERE model = $1)`, model).Scan(&exists); err != nil {
			return fmt.Errorf("store: check model_pricing(%s): %w", model, err)
		}
		if !exists {
			return ErrPricingNotFound
		}
		return ErrPricingConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_pricing WHERE model = $1`, model); err != nil {
		return fmt.Errorf("store: delete group_pricing for model %s: %w", model, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete model_pricing(%s): %w", model, err)
	}
	return nil
}

// SetModelPricingStatus 切换启停状态（status 1/2）。
func (r *SQLPricingRepo) SetModelPricingStatus(ctx context.Context, model string, status int16, expectUpdatedAt time.Time) (ModelPricingRecord, error) {
	rec, err := scanModelPricing(r.db.QueryRowContext(ctx, `
		UPDATE model_pricing SET status = $1, updated_at = now()
		WHERE model = $2 AND updated_at = $3
		RETURNING `+modelPricingColumns, status, model, expectUpdatedAt.UTC()))
	if err == nil {
		return rec, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ModelPricingRecord{}, r.missingOrConflict(ctx, model)
	}
	return ModelPricingRecord{}, fmt.Errorf("store: set model_pricing(%s) status: %w", model, err)
}

// ListPricingModels 返回模型选择器候选。status=nil 表示全部状态。
func (r *SQLPricingRepo) ListPricingModels(ctx context.Context, filter PricingModelFilter) ([]PricingModelOption, error) {
	query := `SELECT model, mode, billing_expr, status FROM model_pricing WHERE 1=1`
	args := make([]any, 0, 3)
	if filter.Keyword != "" {
		args = append(args, filter.Keyword)
		query += fmt.Sprintf(" AND strpos(lower(model), lower($%d)) = 1", len(args))
	}
	if filter.Status != nil {
		args = append(args, *filter.Status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	args = append(args, filter.Limit)
	query += fmt.Sprintf(" ORDER BY model ASC LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list pricing models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]PricingModelOption, 0, filter.Limit)
	for rows.Next() {
		var (
			option      PricingModelOption
			mode        int16
			billingExpr *string
		)
		if err := rows.Scan(&option.Model, &mode, &billingExpr, &option.Status); err != nil {
			return nil, fmt.Errorf("store: scan pricing model: %w", err)
		}
		option.Mode = derivePricingMode(mode, billingExpr)
		items = append(items, option)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pricing models: %w", err)
	}
	return items, nil
}

// derivePricingMode 把 DB 的判别列映射为线格式模式名。
//
// R9：billing_expr 非 NULL 即表达式模式（mode 列被忽略）；否则 mode 0/1
// 分别对应 per_token / per_request。该映射只用于展示，不参与结算。
func derivePricingMode(mode int16, billingExpr *string) string {
	if billingExpr != nil {
		return "tiered_expr"
	}
	if mode == 1 {
		return "per_request"
	}
	return "per_token"
}

// ---------------------------------------------------------------------------
// group_pricing
// ---------------------------------------------------------------------------

const groupPricingSelect = `
	SELECT gp.id, gp."group", gp.model, gp.prompt_price, gp.completion_price,
	       gp.cache_read_price, gp.cache_write_price, gp.status,
	       COALESCE(mp.mode, 0), COALESCE(mp.billing_expr, ''),
	       COALESCE(mp.status, 0), gp.created_at, gp.updated_at
	FROM group_pricing gp
	LEFT JOIN model_pricing mp ON mp.model = gp.model`

func scanGroupPricing(row interface{ Scan(...any) error }) (GroupPricingRecord, error) {
	var (
		rec         GroupPricingRecord
		mode        int16
		billingExpr string
	)
	err := row.Scan(
		&rec.ID, &rec.Group, &rec.Model, &rec.PromptPrice, &rec.CompletionPrice,
		&rec.CacheReadPrice, &rec.CacheWritePrice, &rec.Status, &mode,
		&billingExpr, &rec.SourceStatus, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return GroupPricingRecord{}, err
	}
	// COALESCE(mp.billing_expr,'') 让「源行缺失」与「表达式模式」可区分：
	// 源行缺失时 SourceStatus=0，此时模式名留空。
	if rec.SourceStatus == 0 {
		rec.SourceMode = ""
	} else {
		var expr *string
		if billingExpr != "" {
			expr = &billingExpr
		}
		rec.SourceMode = derivePricingMode(mode, expr)
	}
	return rec, nil
}

func groupPricingWhere(filter GroupPricingFilter) (string, []any) {
	where := " WHERE 1=1"
	args := make([]any, 0, 4)
	if filter.Group != "" {
		args = append(args, filter.Group)
		where += fmt.Sprintf(` AND gp."group" = $%d`, len(args))
	}
	if filter.Keyword != "" {
		args = append(args, filter.Keyword)
		where += fmt.Sprintf(" AND strpos(lower(gp.model), lower($%d)) = 1", len(args))
	}
	if filter.Status != nil {
		args = append(args, *filter.Status)
		where += fmt.Sprintf(" AND gp.status = $%d", len(args))
	}
	return where, args
}

func groupPricingOrder(sort string) string {
	switch sort {
	case "model":
		return ` ORDER BY gp.model ASC, gp."group" ASC`
	case "-model":
		return ` ORDER BY gp.model DESC, gp."group" ASC`
	case "-updated_at":
		return ` ORDER BY gp.updated_at DESC, gp."group" ASC, gp.model ASC`
	case "updated_at":
		return ` ORDER BY gp.updated_at ASC, gp."group" ASC, gp.model ASC`
	case "-group":
		return ` ORDER BY gp."group" DESC, gp.model ASC`
	default:
		return ` ORDER BY gp."group" ASC, gp.model ASC`
	}
}

// ListGroupPricing 返回覆盖价列表（含源模型模式与状态）。
func (r *SQLPricingRepo) ListGroupPricing(ctx context.Context, filter GroupPricingFilter) ([]GroupPricingRecord, int64, error) {
	where, args := groupPricingWhere(filter)

	var total int64
	countQuery := `SELECT count(*) FROM group_pricing gp` + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count group_pricing: %w", err)
	}

	offset := (filter.Page - 1) * filter.PageSize
	query := groupPricingSelect + where + groupPricingOrder(filter.Sort) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	rows, err := r.db.QueryContext(ctx, query, append(args, filter.PageSize, offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list group_pricing: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]GroupPricingRecord, 0, filter.PageSize)
	for rows.Next() {
		rec, scanErr := scanGroupPricing(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("store: scan group_pricing: %w", scanErr)
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate group_pricing: %w", err)
	}
	return items, total, nil
}

// GetGroupPricing 读取单行覆盖价（含源模型信息）。
func (r *SQLPricingRepo) GetGroupPricing(ctx context.Context, group, model string) (GroupPricingRecord, error) {
	row := r.db.QueryRowContext(ctx,
		groupPricingSelect+` WHERE gp."group" = $1 AND gp.model = $2`, group, model)
	rec, err := scanGroupPricing(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupPricingRecord{}, ErrPricingNotFound
	}
	if err != nil {
		return GroupPricingRecord{}, fmt.Errorf("store: get group_pricing(%s,%s): %w", group, model, err)
	}
	return rec, nil
}

// CreateGroupPricing 写入覆盖价。分组必须存在且启用，源模型必须已定价；
// 表达式源与 per_request 源的无效覆盖价字段在保存期拒绝（R-69）。
func (r *SQLPricingRepo) CreateGroupPricing(ctx context.Context, in GroupPricingInput) (GroupPricingRecord, error) {
	sourceMode, err := r.validateGroupAndSource(ctx, in.Group, in.Model)
	if err != nil {
		return GroupPricingRecord{}, err
	}
	if sourceMode == 1 && (in.CompletionPrice != 0 || in.CacheReadPrice != 0 || in.CacheWritePrice != 0) {
		return GroupPricingRecord{}, ErrPricingPerRequestFields
	}
	_, err = r.db.ExecContext(ctx, `
		INSERT INTO group_pricing
			("group", model, prompt_price, completion_price,
			 cache_read_price, cache_write_price, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		in.Group, in.Model, in.PromptPrice, in.CompletionPrice,
		in.CacheReadPrice, in.CacheWritePrice, in.Status)
	if err != nil {
		if isUniqueViolation(err) {
			return GroupPricingRecord{}, ErrPricingDuplicate
		}
		if isForeignKeyViolation(err) {
			return GroupPricingRecord{}, ErrPricingInvalidGroup
		}
		return GroupPricingRecord{}, fmt.Errorf("store: create group_pricing(%s,%s): %w", in.Group, in.Model, err)
	}
	return r.GetGroupPricing(ctx, in.Group, in.Model)
}

// UpdateGroupPricing 按 (group, model) + updated_at 更新。
func (r *SQLPricingRepo) UpdateGroupPricing(ctx context.Context, group, model string, patch GroupPricingPatch, expectUpdatedAt time.Time) (GroupPricingRecord, error) {
	// 覆盖价字段的语义取决于源模型模式：表达式源不读覆盖价，per_request 源
	// 只读 prompt_price。保存期拒绝无效写入，避免管理员以为价格已生效。
	if patch.PromptPrice != nil || patch.CompletionPrice != nil ||
		patch.CacheReadPrice != nil || patch.CacheWritePrice != nil {
		sourceMode, err := r.validateGroupAndSource(ctx, group, model)
		if err != nil {
			return GroupPricingRecord{}, err
		}
		if sourceMode == 1 &&
			((patch.CompletionPrice != nil && *patch.CompletionPrice != 0) ||
				(patch.CacheReadPrice != nil && *patch.CacheReadPrice != 0) ||
				(patch.CacheWritePrice != nil && *patch.CacheWritePrice != 0)) {
			return GroupPricingRecord{}, ErrPricingPerRequestFields
		}
	}

	sets := make([]string, 0, 6)
	args := make([]any, 0, 7)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if patch.PromptPrice != nil {
		add("prompt_price", *patch.PromptPrice)
	}
	if patch.CompletionPrice != nil {
		add("completion_price", *patch.CompletionPrice)
	}
	if patch.CacheReadPrice != nil {
		add("cache_read_price", *patch.CacheReadPrice)
	}
	if patch.CacheWritePrice != nil {
		add("cache_write_price", *patch.CacheWritePrice)
	}
	if patch.Status != nil {
		add("status", *patch.Status)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, group, model, expectUpdatedAt.UTC())

	result, err := r.db.ExecContext(ctx,
		`UPDATE group_pricing SET `+strings.Join(sets, ", ")+
			fmt.Sprintf(` WHERE "group" = $%d AND model = $%d AND updated_at = $%d`,
				len(args)-2, len(args)-1, len(args)), args...)
	if err != nil {
		return GroupPricingRecord{}, fmt.Errorf("store: update group_pricing(%s,%s): %w", group, model, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return GroupPricingRecord{}, fmt.Errorf("store: update group_pricing(%s,%s) rows: %w", group, model, err)
	}
	if affected == 0 {
		if _, err := r.GetGroupPricing(ctx, group, model); err != nil {
			return GroupPricingRecord{}, err
		}
		return GroupPricingRecord{}, ErrPricingConflict
	}
	return r.GetGroupPricing(ctx, group, model)
}

// DeleteGroupPricing 按 (group, model) + updated_at 删除。
func (r *SQLPricingRepo) DeleteGroupPricing(ctx context.Context, group, model string, expectUpdatedAt time.Time) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM group_pricing WHERE "group" = $1 AND model = $2 AND updated_at = $3`,
		group, model, expectUpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: delete group_pricing(%s,%s): %w", group, model, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete group_pricing(%s,%s) rows: %w", group, model, err)
	}
	if affected == 0 {
		if _, err := r.GetGroupPricing(ctx, group, model); err != nil {
			return err
		}
		return ErrPricingConflict
	}
	return nil
}

// SetGroupPricingStatus 切换覆盖价启停。
func (r *SQLPricingRepo) SetGroupPricingStatus(ctx context.Context, group, model string, status int16, expectUpdatedAt time.Time) (GroupPricingRecord, error) {
	result, err := r.db.ExecContext(ctx, `
		UPDATE group_pricing SET status = $1, updated_at = now()
		WHERE "group" = $2 AND model = $3 AND updated_at = $4`,
		status, group, model, expectUpdatedAt.UTC())
	if err != nil {
		return GroupPricingRecord{}, fmt.Errorf("store: set group_pricing(%s,%s) status: %w", group, model, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return GroupPricingRecord{}, fmt.Errorf("store: set group_pricing(%s,%s) rows: %w", group, model, err)
	}
	if affected == 0 {
		if _, err := r.GetGroupPricing(ctx, group, model); err != nil {
			return GroupPricingRecord{}, err
		}
		return GroupPricingRecord{}, ErrPricingConflict
	}
	return r.GetGroupPricing(ctx, group, model)
}

// validateGroupAndModel 把 FK 与逻辑引用提前成可读错误。
// validateGroupAndSource 校验分组存在且启用、源模型已定价，并返回源模型模式。
//
// 表达式源直接拒绝：billing-02 §3.1 规定表达式是唯一价格配置，DBSource 在
// tiered_expr 模式下跳过 group_pricing 查询——写入覆盖价不会生效。
func (r *SQLPricingRepo) validateGroupAndSource(ctx context.Context, group, model string) (int16, error) {
	var groupOK bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_groups WHERE name = $1 AND status = 1)`, group).Scan(&groupOK); err != nil {
		return 0, fmt.Errorf("store: check group %s: %w", group, err)
	}
	if !groupOK {
		return 0, ErrPricingInvalidGroup
	}
	var (
		mode        int16
		billingExpr *string
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT mode, billing_expr FROM model_pricing WHERE model = $1`, model).Scan(&mode, &billingExpr)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrPricingInvalidModel
	}
	if err != nil {
		return 0, fmt.Errorf("store: check source model %s: %w", model, err)
	}
	if billingExpr != nil {
		return 0, ErrPricingSourceIsExpression
	}
	return mode, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
