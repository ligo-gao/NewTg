package store

// discounts 仓储：按「用户 / 分组 × 模型」解析并管理折扣规则（spec §4.2/§5.2）。
//
// 【为什么在这里回源 users."group"】Bearer 路径的 RelayContext.UserGroup 是
// api_tokens."group"，可以与用户的归属分组不同（group=auto 时字面值就是
// "auto"）。若用令牌分组匹配，折扣会被挂到令牌或 auto 上；Resolve 的子查询
// 一次读取 users."group"，不依赖 relay 层的任何分组字段。
//
// 【为什么规则不设外键】目标既可能是 users.id（十进制文本）也可能是
// user_groups.name，单列无法同时引用两张表。生命周期由应用层在同一事务内
// 级联：删除用户/分组时调 DeleteDiscountsForTarget，分组改名时调
// RenameGroupDiscountTargets（spec §4.5）。
//
// 【为什么读取侧还要再校验】DB CHECK 已经限定 rate_percent 0–100、target_id
// 非空，但历史数据、手工 SQL 或未来迁移都可能绕过约束。折扣直接决定实收
// 金额：一条越界行若被当成 rate=0 或空目标使用，就是免费请求。Resolve 对
// 坏行记 WARN 后跳过，宁可原价也不误免。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	discountTargetUser  = "user"
	discountTargetGroup = "group"

	// discountDefaultListLimit 是 ListEnabledModels 在调用方未给 limit 时
	// 的默认条数；与其它模型选择器的默认口径一致（默认 200，不设硬上限，
	// 上限由管理面 UI 契约控制）。
	discountDefaultListLimit = 200
)

// 折扣仓储的哨兵错误，供 httpapi 映射 404/409。
var (
	// ErrDiscountNotFound：被寻址的折扣规则不存在。
	ErrDiscountNotFound = errors.New("store: discount not found")
	// ErrDiscountDuplicate：同一 (model_name, target_type, target_id) 已有规则
	// （discounts_rule_uq 唯一索引冲突）。
	ErrDiscountDuplicate = errors.New("store: discount already exists for this model and target")
)

// CustomerDiscount 是一条折扣规则。
//
// RatePercent 是**实收百分比**：90 = 9 折、0 = 全免、100 = 原价。
// ValidFrom/ValidUntil 为 UTC 时刻、左闭右开；nil 表示该侧无限制。
type CustomerDiscount struct {
	ID          int64
	ModelName   string
	TargetType  string
	TargetID    string
	RatePercent int16
	ValidFrom   *time.Time
	ValidUntil  *time.Time
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Rate 把实收百分比换算为 0–1 的实收倍率。
func (d CustomerDiscount) Rate() float64 { return float64(d.RatePercent) / 100 }

// Target 返回计费契约使用的目标编码：user:<id> / group:<name>。
// 未知 target_type 返回空串——ApplyDiscount 只对非空目标生效，不会误打折。
func (d CustomerDiscount) Target() string {
	switch d.TargetType {
	case discountTargetUser:
		return discountTargetUser + ":" + d.TargetID
	case discountTargetGroup:
		return discountTargetGroup + ":" + d.TargetID
	default:
		return ""
	}
}

// CustomerDiscountInput 是创建/更新折扣规则的写入值。
//
// Update 按全字段覆盖（含 ModelName）；改比例走 Update 而不是叠一条新规则，
// 由 (model_name, target_type, target_id) 唯一索引保证同一对象同一模型只有
// 一条规则。
type CustomerDiscountInput struct {
	ModelName   string
	TargetType  string
	TargetID    string
	RatePercent int16
	ValidFrom   *time.Time
	ValidUntil  *time.Time
	Enabled     bool
}

// CustomerDiscountRepo 是 discounts 的 PostgreSQL 实现。
type CustomerDiscountRepo struct {
	db *sql.DB
}

// NewCustomerDiscountRepo 构造仓储。db 必须非 nil：折扣解析没有内存兜底。
func NewCustomerDiscountRepo(db *sql.DB) *CustomerDiscountRepo {
	return &CustomerDiscountRepo{db: db}
}

// customerDiscountColumns 是读取/回写共用的列清单；顺序与 scanCustomerDiscount
// 一一对应。
const customerDiscountColumns = `id, model_name, target_type, target_id, rate_percent,
	valid_from, valid_until, enabled, created_at, updated_at`

// resolveCustomerDiscountsQuery 按候选模型名与用户身份取回**全部**可能命中的
// 规则（spec §5.2 冻结 SQL）。$1 是候选模型名数组，$2 是用户 id 的十进制
// 文本（目标列是 text），$3 是判定有效期的时刻。
//
// 分组不是从令牌或 RelayContext 取：子查询回源 users."group"，一次完成。
// 最终选唯一规则由 PickCustomerDiscount 负责（rate 最低 → user 优先 →
// 候选名靠前）。
const resolveCustomerDiscountsQuery = `
	SELECT ` + customerDiscountColumns + `
	FROM discounts
	WHERE enabled
	  AND model_name = ANY($1::text[])
	  AND ((target_type = 'user' AND target_id = $2)
	    OR (target_type = 'group' AND target_id = (SELECT "group" FROM users WHERE id = $2::bigint)))
	  AND (valid_from IS NULL OR valid_from <= $3)
	  AND (valid_until IS NULL OR $3 < valid_until)`

// scanCustomerDiscount 把一行 database/sql 结果扫描为 CustomerDiscount。
// 可空 timestamp 先落 sql.NullTime 再转 *time.Time，避免驱动对 NULL 的
// 指针语义差异。
func scanCustomerDiscount(scan func(...any) error) (CustomerDiscount, error) {
	var (
		d          CustomerDiscount
		validFrom  sql.NullTime
		validUntil sql.NullTime
	)
	if err := scan(&d.ID, &d.ModelName, &d.TargetType, &d.TargetID, &d.RatePercent,
		&validFrom, &validUntil, &d.Enabled, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return CustomerDiscount{}, err
	}
	if validFrom.Valid {
		t := validFrom.Time
		d.ValidFrom = &t
	}
	if validUntil.Valid {
		t := validUntil.Time
		d.ValidUntil = &t
	}
	return d, nil
}

// Resolve 解析用户对某个请求模型命中的唯一折扣规则。
//
// models 是 pricing.ResolveBillingModelCandidates 展开后的候选名（规范名 →
// 原名 → 基名），空切片直接视为未命中。at 是会话冻结的 requestTime，不能用
// time.Now()：有效期必须与预扣时一致。
//
// 第二个返回值为 false 表示没有命中规则（原价），不是错误。命中时按
// (rate_percent asc, user before group, 候选名靠前) 取唯一一条。
//
// 读取侧对坏数据 fail-closed：rate_percent 越界、target_id 为空或
// target_type 不是 user/group 的行都被跳过（WARN 后按原价）；
// 个人规则还要求目标用户仍然存在——用户被删除而规则因绕过级联残留时，
// 该规则不能给任何请求打折。
func (r *CustomerDiscountRepo) Resolve(ctx context.Context, userID int64, models []string, at time.Time) (CustomerDiscount, bool, error) {
	if r == nil || r.db == nil {
		return CustomerDiscount{}, false, errors.New("store: discounts 仓储未绑定数据库")
	}
	if len(models) == 0 {
		return CustomerDiscount{}, false, nil
	}
	rows, err := r.db.QueryContext(ctx, resolveCustomerDiscountsQuery,
		models, strconv.FormatInt(userID, 10), at)
	if err != nil {
		return CustomerDiscount{}, false, fmt.Errorf("store: resolve discounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hasUserRule := false
	valid := make([]CustomerDiscount, 0, 2)
	for rows.Next() {
		d, err := scanCustomerDiscount(rows.Scan)
		if err != nil {
			return CustomerDiscount{}, false, fmt.Errorf("store: resolve discounts scan: %w", err)
		}
		if d.RatePercent < 0 || d.RatePercent > 100 {
			slog.Warn("discount rule skipped: rate_percent out of range",
				"discount_id", d.ID, "rate_percent", d.RatePercent)
			continue
		}
		if d.TargetType != discountTargetUser && d.TargetType != discountTargetGroup {
			slog.Warn("discount rule skipped: unknown target_type",
				"discount_id", d.ID, "target_type", d.TargetType)
			continue
		}
		if d.TargetID == "" {
			slog.Warn("discount rule skipped: empty target_id",
				"discount_id", d.ID, "target_type", d.TargetType)
			continue
		}
		if d.TargetType == discountTargetUser {
			hasUserRule = true
		}
		valid = append(valid, d)
	}
	if err := rows.Err(); err != nil {
		return CustomerDiscount{}, false, fmt.Errorf("store: resolve discounts iterate: %w", err)
	}
	// 个人规则按 target_id 文本直接匹配，不经过 users 联结：用户被删除而规则
	// 残留（绕过应用层级联）时必须显式确认用户仍存在，否则会给一个已不存在的
	// 用户 id 返回折扣。分组规则的分支用子查询读 users."group"，用户不存在时
	// 天然为 NULL，不需要这一步。
	//
	// 只有实际取回个人规则时才多查一次；普通（无个人规则）请求不多一次往返。
	if hasUserRule {
		var exists bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1::bigint)`, userID).Scan(&exists); err != nil {
			return CustomerDiscount{}, false, fmt.Errorf("store: resolve discount target user(%d): %w", userID, err)
		}
		if !exists {
			return CustomerDiscount{}, false, nil
		}
	}
	picked, ok := PickCustomerDiscount(valid, models)
	return picked, ok, nil
}

// PickCustomerDiscount 从候选规则中选出唯一一条（spec §5.2 的优先级）：
//
//  1. rate_percent 最低者胜（折后金额最小、折扣力度最大）；
//  2. 相同 rate_percent：user 规则胜 group 规则（目标更具体）；
//  3. 仍相同：取 ModelName 在 candidates 里更靠前的规则（规范名优先）。
//
// 三元组完全相同时保留先出现的一行，保证结果与数据库返回顺序无关但确定。
// 空输入返回零值与 false。
func PickCustomerDiscount(rows []CustomerDiscount, candidates []string) (CustomerDiscount, bool) {
	if len(rows) == 0 {
		return CustomerDiscount{}, false
	}
	candidateIndex := func(model string) int {
		for i, c := range candidates {
			if c == model {
				return i
			}
		}
		return len(candidates)
	}
	best := -1
	bestRank, bestIndex := 0, 0
	for i, row := range rows {
		rank := 1
		if row.TargetType == discountTargetUser {
			rank = 0
		}
		index := candidateIndex(row.ModelName)
		if best < 0 {
			best, bestRank, bestIndex = i, rank, index
			continue
		}
		current := rows[best]
		if row.RatePercent < current.RatePercent ||
			(row.RatePercent == current.RatePercent &&
				(rank < bestRank || (rank == bestRank && index < bestIndex))) {
			best, bestRank, bestIndex = i, rank, index
		}
	}
	return rows[best], true
}

// ListForTarget 列出某个目标（user:<id> 或 group:<name>）的全部规则，含停用
// 与过期行——管理面要展示并允许重新启用它们。按模型名排序保证稳定。
func (r *CustomerDiscountRepo) ListForTarget(ctx context.Context, targetType, targetID string) ([]CustomerDiscount, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: discounts 仓储未绑定数据库")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+customerDiscountColumns+`
		 FROM discounts
		 WHERE target_type = $1 AND target_id = $2
		 ORDER BY model_name`, targetType, targetID)
	if err != nil {
		return nil, fmt.Errorf("store: list discounts for %s:%s: %w", targetType, targetID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]CustomerDiscount, 0, 4)
	for rows.Next() {
		d, err := scanCustomerDiscount(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: list discounts scan: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list discounts iterate: %w", err)
	}
	return out, nil
}

// Get 按 id 读取一条规则；不存在返回 ErrDiscountNotFound。
func (r *CustomerDiscountRepo) Get(ctx context.Context, id int64) (CustomerDiscount, bool, error) {
	if r == nil || r.db == nil {
		return CustomerDiscount{}, false, errors.New("store: discounts 仓储未绑定数据库")
	}
	d, err := scanCustomerDiscount(r.db.QueryRowContext(ctx,
		`SELECT `+customerDiscountColumns+` FROM discounts WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomerDiscount{}, false, ErrDiscountNotFound
	}
	if err != nil {
		return CustomerDiscount{}, false, fmt.Errorf("store: read discount(%d): %w", id, err)
	}
	return d, true, nil
}

// Create 新建一条规则并回读完整行。唯一键冲突返回 ErrDiscountDuplicate。
func (r *CustomerDiscountRepo) Create(ctx context.Context, in CustomerDiscountInput) (CustomerDiscount, error) {
	if r == nil || r.db == nil {
		return CustomerDiscount{}, errors.New("store: discounts 仓储未绑定数据库")
	}
	d, err := scanCustomerDiscount(r.db.QueryRowContext(ctx,
		`INSERT INTO discounts
		   (model_name, target_type, target_id, rate_percent, valid_from, valid_until, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+customerDiscountColumns,
		in.ModelName, in.TargetType, in.TargetID, in.RatePercent,
		in.ValidFrom, in.ValidUntil, in.Enabled).Scan)
	if isUniqueViolation(err) {
		return CustomerDiscount{}, ErrDiscountDuplicate
	}
	if err != nil {
		return CustomerDiscount{}, fmt.Errorf("store: create discount: %w", err)
	}
	return d, nil
}

// Update 全字段覆盖一条规则（含 model_name），并刷新 updated_at。
// 行不存在返回 ErrDiscountNotFound，唯一键冲突返回 ErrDiscountDuplicate。
func (r *CustomerDiscountRepo) Update(ctx context.Context, id int64, in CustomerDiscountInput) (CustomerDiscount, error) {
	if r == nil || r.db == nil {
		return CustomerDiscount{}, errors.New("store: discounts 仓储未绑定数据库")
	}
	d, err := scanCustomerDiscount(r.db.QueryRowContext(ctx,
		`UPDATE discounts
		 SET model_name = $2, target_type = $3, target_id = $4, rate_percent = $5,
		     valid_from = $6, valid_until = $7, enabled = $8, updated_at = now()
		 WHERE id = $1
		 RETURNING `+customerDiscountColumns,
		id, in.ModelName, in.TargetType, in.TargetID, in.RatePercent,
		in.ValidFrom, in.ValidUntil, in.Enabled).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomerDiscount{}, ErrDiscountNotFound
	}
	if isUniqueViolation(err) {
		return CustomerDiscount{}, ErrDiscountDuplicate
	}
	if err != nil {
		return CustomerDiscount{}, fmt.Errorf("store: update discount(%d): %w", id, err)
	}
	return d, nil
}

// Delete 删除一条规则；行不存在返回 ErrDiscountNotFound。
func (r *CustomerDiscountRepo) Delete(ctx context.Context, id int64) error {
	if r == nil || r.db == nil {
		return errors.New("store: discounts 仓储未绑定数据库")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM discounts WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete discount(%d): %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete discount(%d) rows affected: %w", id, err)
	}
	if n == 0 {
		return ErrDiscountNotFound
	}
	return nil
}

// ListEnabledModels 返回已启用（model_pricing.status = 1）的模型名，供管理面
// 折扣规则编辑器的模型选择器使用（spec Task 7）。q 先 TrimSpace，非空时做
// 大小写不敏感的子串过滤；limit <= 0 取默认 200。
//
// 关键字经 escapeLike 转义并配 ESCAPE '!'（与渠道/供应商搜索同一口径）：
// 模型名允许 `%`/`_`，不转义时 `a_b` 会连 `axb` 一起命中、裸 `%` 会返回全表。
func (r *CustomerDiscountRepo) ListEnabledModels(ctx context.Context, q string, limit int) ([]string, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: discounts 仓储未绑定数据库")
	}
	if limit <= 0 {
		limit = discountDefaultListLimit
	}
	query := `SELECT model FROM model_pricing WHERE status = 1`
	args := make([]any, 0, 2)
	if keyword := strings.TrimSpace(q); keyword != "" {
		args = append(args, "%"+escapeLike(keyword)+"%")
		query += ` AND model ILIKE $1 ESCAPE '!'`
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY model LIMIT $%d`, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list enabled models: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, limit)
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("store: list enabled models scan: %w", err)
		}
		out = append(out, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list enabled models iterate: %w", err)
	}
	return out, nil
}

// TargetExists 判断 target_type/target_id 是否指向一个真实存在的对象。
//
// user 的 target_id 必须是十进制用户 id；非法文本直接返回 false（不查库，
// 也不报错——调用方用它做写入前的引用校验）。group 查 user_groups.name，
// 包含停用分组：改名/删除的引用保护以「行是否存在」为准。
func (r *CustomerDiscountRepo) TargetExists(ctx context.Context, targetType, targetID string) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("store: discounts 仓储未绑定数据库")
	}
	switch targetType {
	case discountTargetUser:
		id, err := strconv.ParseInt(targetID, 10, 64)
		if err != nil || id <= 0 {
			return false, nil
		}
		var exists bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1::bigint)`, id).Scan(&exists); err != nil {
			return false, fmt.Errorf("store: discount target user %q: %w", targetID, err)
		}
		return exists, nil
	case discountTargetGroup:
		var exists bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM user_groups WHERE name = $1)`, targetID).Scan(&exists); err != nil {
			return false, fmt.Errorf("store: discount target group %q: %w", targetID, err)
		}
		return exists, nil
	default:
		return false, nil
	}
}

// sqlExecer 是 *sql.DB 与 *sql.Tx 的公共写接口。级联辅助函数用它表达
// 「必须与调用方的删除/改名在同一事务内执行」，不额外持有连接。
type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// DeleteDiscountsForTarget 删除某个目标的全部规则，返回删除行数。
//
// 用户/分组删除时由调用方在同一事务内调用：不级联会留下指向不存在对象的
// 幽灵规则（spec §4.5）。
func DeleteDiscountsForTarget(ctx context.Context, ex sqlExecer, targetType, targetID string) (int64, error) {
	if ex == nil {
		return 0, errors.New("store: DeleteDiscountsForTarget 需要非 nil 执行器")
	}
	res, err := ex.ExecContext(ctx,
		`DELETE FROM discounts WHERE target_type = $1 AND target_id = $2`, targetType, targetID)
	if err != nil {
		return 0, fmt.Errorf("store: delete discounts for %s:%s: %w", targetType, targetID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete discounts rows affected: %w", err)
	}
	return n, nil
}

// RenameGroupDiscountTargets 把旧分组名的全部规则改挂到新分组名，返回更新
// 行数。分组改名时由调用方在同一事务内调用。
//
// 目标表没有外键，改名撞上 (model_name, 'group', newName) 的唯一键时返回
// 原始错误（含 SQLSTATE 23505），由调用方决定映射 409。
func RenameGroupDiscountTargets(ctx context.Context, ex sqlExecer, oldName, newName string) (int64, error) {
	if ex == nil {
		return 0, errors.New("store: RenameGroupDiscountTargets 需要非 nil 执行器")
	}
	res, err := ex.ExecContext(ctx,
		`UPDATE discounts SET target_id = $2, updated_at = now()
		 WHERE target_type = 'group' AND target_id = $1`, oldName, newName)
	if err != nil {
		return 0, fmt.Errorf("store: rename discounts %q -> %q: %w", oldName, newName, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rename discounts rows affected: %w", err)
	}
	return n, nil
}
