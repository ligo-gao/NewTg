package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"newtg/gateway/internal/contract"
)

// 本文件是 channels 表的 SQL 仓储（R-P10：database/sql + pgx stdlib，不用 GORM）。
// 列顺序与 platform-01 §6 DDL 一一对应；NULL 语义集中在 scanChannel。
//
// 错误口径：handler 只按 errors.Is 判定业务语义，不解析数据库错误文本
// （避免把表名/约束名/SQL 片段带到 RFC7807 响应里）。
var (
	// ErrChannelNotFound 表示按 id 查询/更新/删除时目标行不存在。
	ErrChannelNotFound = errors.New("store: channel not found")
	// ErrConflict 表示乐观锁失配（R-P8：stored updated_at != expected_version）。
	ErrConflict = errors.New("store: channel version conflict")
	// ErrNameConflict 表示 channels_name_uq 唯一约束冲突（pgcode 23505）。
	ErrNameConflict = errors.New("store: channel name conflict")
	// ErrInvalidChannel 表示写入被 DB CHECK 拒绝（pgcode 23514）。
	// handler 把它映射为 422：应用层校验是第一道闸，但 CHECK 是最终兜底
	// （例如 tag 字符集、test_model ∈ supported_models），不应退化成 500。
	ErrInvalidChannel = errors.New("store: channel violates schema constraints")
)

// ChannelFilter 是列表查询的筛选条件（modules-06 F-06-01）。
// Page 为 1 基；PageSize 上限 100；Sort 支持 priority（默认）/name；
// response_time 由 handler 接受后落回默认 priority 顺序（M2 尚无 24h 聚合数据源）。
//
// Group/Model 的 jsonb 成员判断用**包含**（`@> jsonb_build_array($n)`）而不是
// 存在运算符 `?`：DDL 上的 channels_group_tags_gin / channels_supported_models_gin
// 是 jsonb_path_ops GIN，`?` 谓词无法使用该类索引（M2 final review finding 5，
// platform-04 §3.3.3）。group_tags IS NULL 分支语义上是「全部分组」，无法走索引，
// 由低基数过滤兜底。
type ChannelFilter struct {
	Page     int
	PageSize int
	Keyword  string
	Status   *int16
	Types    []string
	Group    string
	Model    string
	Sort     string
}

// ChannelPatch 是 PATCH 的部分更新载荷。指针/Set 标志区分三种语义：
//
//	字段为 nil / Set=false  → 不修改（PATCH 的缺省）
//	指针非 nil               → 写入该值
//	Set=true 且指针为 nil    → 写入 SQL NULL（可空列的「清空」语义）
//
// Credentials 例外：nil = 保持原密文（modules-06 F-06-03：留空不修改）；
// 非 nil = 已由 handler 重新封装的密文。
type ChannelPatch struct {
	Name            *string
	Type            *string
	BaseURLSet      bool
	BaseURL         *string
	Credentials     []byte
	SupportedModels []byte
	GroupTagsSet    bool
	GroupTags       []byte
	ModelMapping    []byte
	Weight          *int16
	Priority        *int16
	RateLimitSet    bool
	RateLimit       *int32
	Status          *int16
	AutoBan         *bool
	TestModelSet    bool
	TestModel       *string
	RemarkSet       bool
	Remark          *string
	TagSet          bool
	Tag             *string
	FailCount       *int32

	AutoDisabledReasonSet bool
	AutoDisabledReason    *string
	LastTestAtSet         bool
	LastTestAt            *time.Time
	LastTestResultSet     bool
	LastTestResult        *bool
}

// BatchResult 对应 modules-06 F-06-02 的 {succeeded:[id], failed:[{id,reason}]}。
// 字段名即对外 JSON（handler 直接序列化），切片初始化为空切片而非 nil，
// 保证空结果序列化为 [] 而不是 null。
type BatchResult struct {
	Succeeded []int64        `json:"succeeded"`
	Failed    []BatchFailure `json:"failed"`
}

// BatchFailure 是批量操作逐项失败原因；reason 只允许稳定短码，
// 不携带数据库错误文本（GC-6：错误不得泄漏内部细节）。
type BatchFailure struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// ChannelRepo 是 channels 表的 SQL 仓储。
type ChannelRepo struct{ db *sql.DB }

// NewChannelRepo 构造仓储；db 由 platform.OpenDB 提供（pgx stdlib 驱动）。
func NewChannelRepo(db *sql.DB) *ChannelRepo { return &ChannelRepo{db: db} }

// channelColumns 与 platform-01 §6 的 22 列逐列对应（顺序固定，供 scanChannel 使用）。
const channelColumns = `id, name, type, base_url, credentials, supported_models, group_tags,
	model_mapping, weight, priority, rate_limit, status, auto_ban, test_model, remark, tag,
	fail_count, auto_disabled_reason, last_test_at, last_test_result, created_at, updated_at`

// scanRow 抽象 *sql.Row / *sql.Rows 的共同 Scan 方法。
type scanRow interface {
	Scan(dest ...any) error
}

// scanChannel 读取一行。可空列统一走 sql.Null*：jsonb 的 NULL（group_tags）
// 与 JSON null 是两回事，前者必须保留为 nil []byte。
func scanChannel(row scanRow) (*Channel, error) {
	var (
		ch       Channel
		baseURL  sql.NullString
		groupTag []byte
		rateLim  sql.NullInt32
		testMod  sql.NullString
		remark   sql.NullString
		tag      sql.NullString
		reason   sql.NullString
		lastAt   sql.NullTime
		lastRes  sql.NullBool
	)
	err := row.Scan(
		&ch.ID, &ch.Name, &ch.Type, &baseURL, &ch.Credentials, &ch.SupportedModels, &groupTag,
		&ch.ModelMapping, &ch.Weight, &ch.Priority, &rateLim, &ch.Status, &ch.AutoBan,
		&testMod, &remark, &tag, &ch.FailCount, &reason, &lastAt, &lastRes,
		&ch.CreatedAt, &ch.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if baseURL.Valid {
		ch.BaseURL = &baseURL.String
	}
	// group_tags 是 NULL 时 scan 得到 nil，正是「全部分组」的表示。
	ch.GroupTags = groupTag
	if rateLim.Valid {
		v := rateLim.Int32
		ch.RateLimit = &v
	}
	if testMod.Valid {
		ch.TestModel = &testMod.String
	}
	if remark.Valid {
		ch.Remark = &remark.String
	}
	if tag.Valid {
		ch.Tag = &tag.String
	}
	if reason.Valid {
		ch.AutoDisabledReason = &reason.String
	}
	if lastAt.Valid {
		t := lastAt.Time
		ch.LastTestAt = &t
	}
	if lastRes.Valid {
		v := lastRes.Bool
		ch.LastTestResult = &v
	}
	return &ch, nil
}

// List 返回 offset 分页的渠道与总数。
func (r *ChannelRepo) List(ctx context.Context, f ChannelFilter) ([]Channel, int, error) {
	page, pageSize := normalizePage(f.Page, f.PageSize)
	where, args := buildChannelWhere(f)

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM channels`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count channels: %w", err)
	}

	// 默认 priority 降序；name 升序为 F-06-01 的第二种排序。
	// id ASC 作为稳定的最终 tie-breaker，避免同优先级翻页时行序漂移。
	order := `ORDER BY priority DESC, id ASC`
	if f.Sort == "name" {
		order = `ORDER BY name ASC, id ASC`
	}

	query := `SELECT ` + channelColumns + ` FROM channels` + where + ` ` + order +
		fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)

	rows, err := r.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list channels: %w", err)
	}
	defer rows.Close()

	items := make([]Channel, 0, pageSize)
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("store: scan channel: %w", err)
		}
		items = append(items, *ch)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate channels: %w", err)
	}
	return items, total, nil
}

// Get 按 id 读取一行；不存在返回 ErrChannelNotFound。
func (r *ChannelRepo) Get(ctx context.Context, id int64) (*Channel, error) {
	ch, err := scanChannel(r.db.QueryRowContext(ctx,
		`SELECT `+channelColumns+` FROM channels WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get channel %d: %w", id, err)
	}
	return ch, nil
}

// Create 插入一行并返回数据库生成的 id/created_at/updated_at。
// credentials 必须已是密文（GC-8）；本层不做加解密，避免密钥泄漏到 SQL 层。
func (r *ChannelRepo) Create(ctx context.Context, ch *Channel) (*Channel, error) {
	if len(ch.ModelMapping) == 0 {
		// model_mapping NOT NULL DEFAULT '{}'；显式写入以保证 RETURNING 语义一致。
		ch.ModelMapping = []byte(`{}`)
	}
	row := r.db.QueryRowContext(ctx, `INSERT INTO channels
		(name, type, base_url, credentials, supported_models, group_tags, model_mapping,
		 weight, priority, rate_limit, status, auto_ban, test_model, remark, tag, auto_disabled_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING `+channelColumns,
		ch.Name, ch.Type, nullableString(ch.BaseURL), ch.Credentials, ch.SupportedModels,
		nullableBytes(ch.GroupTags), ch.ModelMapping, ch.Weight, ch.Priority,
		nullableInt32(ch.RateLimit), ch.Status, ch.AutoBan, nullableString(ch.TestModel),
		nullableString(ch.Remark), nullableString(ch.Tag), nullableString(ch.AutoDisabledReason))

	out, err := scanChannel(row)
	if isUniqueViolation(err) {
		return nil, ErrNameConflict
	}
	if isCheckViolation(err) {
		return nil, ErrInvalidChannel
	}
	if err != nil {
		return nil, fmt.Errorf("store: create channel: %w", err)
	}
	return out, nil
}

// Update 执行乐观锁部分更新（R-P8）：WHERE id AND updated_at = expected。
//
// 行为：
//   - 命中 → 返回更新后的整行（updated_at 由 now() 刷新）；
//   - 行存在但版本失配 → ErrConflict；
//   - 行不存在 → ErrChannelNotFound；
//   - 无任何字段变更时仍校验版本，避免「空 PATCH 绕过 409」。
func (r *ChannelRepo) Update(ctx context.Context, id int64, patch ChannelPatch, expected time.Time) (*Channel, error) {
	sets := make([]string, 0, 24)
	args := make([]any, 0, 25)
	add := func(expr string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf(expr, len(args)))
	}

	if patch.Name != nil {
		add("name = $%d", *patch.Name)
	}
	if patch.Type != nil {
		add("type = $%d", *patch.Type)
	}
	if patch.BaseURLSet {
		add("base_url = $%d", nullableString(patch.BaseURL))
	}
	if patch.Credentials != nil {
		add("credentials = $%d", patch.Credentials)
	}
	if patch.SupportedModels != nil {
		add("supported_models = $%d", patch.SupportedModels)
	}
	if patch.GroupTagsSet {
		add("group_tags = $%d", nullableBytes(patch.GroupTags))
	}
	if patch.ModelMapping != nil {
		add("model_mapping = $%d", patch.ModelMapping)
	}
	if patch.Weight != nil {
		add("weight = $%d", *patch.Weight)
	}
	if patch.Priority != nil {
		add("priority = $%d", *patch.Priority)
	}
	if patch.RateLimitSet {
		add("rate_limit = $%d", nullableInt32(patch.RateLimit))
	}
	if patch.Status != nil {
		add("status = $%d", *patch.Status)
	}
	if patch.AutoBan != nil {
		add("auto_ban = $%d", *patch.AutoBan)
	}
	if patch.TestModelSet {
		add("test_model = $%d", nullableString(patch.TestModel))
	}
	if patch.RemarkSet {
		add("remark = $%d", nullableString(patch.Remark))
	}
	if patch.TagSet {
		add("tag = $%d", nullableString(patch.Tag))
	}
	if patch.FailCount != nil {
		add("fail_count = $%d", *patch.FailCount)
	}
	if patch.AutoDisabledReasonSet {
		add("auto_disabled_reason = $%d", nullableString(patch.AutoDisabledReason))
	}
	if patch.LastTestAtSet {
		add("last_test_at = $%d", nullableTime(patch.LastTestAt))
	}
	if patch.LastTestResultSet {
		add("last_test_result = $%d", nullableBool(patch.LastTestResult))
	}

	if len(sets) == 0 {
		return r.getWithVersion(ctx, id, expected)
	}

	// updated_at=now()：任何写操作都推进乐观锁版本；操作日志 before-after 由
	// 后续任务消费该行（本任务只保证语义），启停切换同样刷新（modules-06 §1.4）。
	sets = append(sets, "updated_at = now()")
	args = append(args, id, expected)
	query := `UPDATE channels SET ` + strings.Join(sets, ", ") +
		fmt.Sprintf(` WHERE id = $%d AND updated_at = $%d RETURNING `, len(args)-1, len(args)) +
		channelColumns

	out, err := scanChannel(r.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, r.classifyMissingUpdate(ctx, id)
	}
	if isUniqueViolation(err) {
		return nil, ErrNameConflict
	}
	if isCheckViolation(err) {
		return nil, ErrInvalidChannel
	}
	if err != nil {
		return nil, fmt.Errorf("store: update channel %d: %w", id, err)
	}
	return out, nil
}

// getWithVersion 用于空 PATCH：只做版本校验并返回当前行。
func (r *ChannelRepo) getWithVersion(ctx context.Context, id int64, expected time.Time) (*Channel, error) {
	ch, err := scanChannel(r.db.QueryRowContext(ctx,
		`SELECT `+channelColumns+` FROM channels WHERE id = $1 AND updated_at = $2`, id, expected))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, r.classifyMissingUpdate(ctx, id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get channel %d with version: %w", id, err)
	}
	return ch, nil
}

// classifyMissingUpdate 区分「行不存在」与「版本失配」。
//
// 为了不把两种语义混成一个错误，失配后额外做一次存在性查询；
// 并发删除会落到 ErrChannelNotFound，这是正确的（客户端应刷新列表）。
func (r *ChannelRepo) classifyMissingUpdate(ctx context.Context, id int64) error {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1)`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("store: check channel %d existence: %w", id, err)
	}
	if !exists {
		return ErrChannelNotFound
	}
	return ErrConflict
}

// Delete 删除一行；不存在返回 ErrChannelNotFound。
func (r *ChannelRepo) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM channels WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete channel %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete channel %d rows: %w", id, err)
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

// batchStatusLockQuery 锁定批量的目标行。
//
// ORDER BY id 是**死锁防护**（M2 final review finding 8）：FOR UPDATE 按扫描顺序
// 逐行加锁，两个批量请求若以不同顺序锁重叠 id 集合（如 [1,2] 与 [2,1]），
// 会形成 A 等 B、B 等 A 的环。固定升序加锁让所有事务以同一顺序获取行锁。
const batchStatusLockQuery = `SELECT id FROM channels WHERE id = ANY($1::bigint[]) ORDER BY id FOR UPDATE`

// BatchStatus 在单事务内批量切换状态（modules-06 F-06-02）：
//   - 不存在的 id → failed(not_found)；
//   - 已处于目标态 → succeeded，但不产生 UPDATE（TC-F-06-02-02）；
//   - enable(1)：status != 1 的全部迁移，并清零 fail_count / auto_disabled_reason
//     （F-06-08：auto_disabled 手动启用同时清零失败计数）；
//   - disable(2)：仅 enabled(1) → manual_disabled(2)（F-06-02 状态机；
//     该端点不承担按标签禁用把 auto_disabled 升级为 manual 的 F-06-21 语义）。
//
// 先 SELECT ... FOR UPDATE 再 UPDATE：保证「哪些行存在」与「更新结果」一致，
// 且以 ORDER BY id 的固定顺序串行化并发批量操作（避免重叠 id 集合的死锁）。
func (r *ChannelRepo) BatchStatus(ctx context.Context, ids []int64, status int16) (BatchResult, error) {
	ids = dedupeIDs(ids)
	result := BatchResult{Succeeded: []int64{}, Failed: []BatchFailure{}}
	if len(ids) == 0 {
		return result, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("store: batch status begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, batchStatusLockQuery, ids)
	if err != nil {
		return result, fmt.Errorf("store: batch status lock: %w", err)
	}
	existing := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, fmt.Errorf("store: batch status scan: %w", err)
		}
		existing[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("store: batch status iterate: %w", err)
	}
	_ = rows.Close()

	existingIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := existing[id]; ok {
			result.Succeeded = append(result.Succeeded, id)
			existingIDs = append(existingIDs, id)
		} else {
			result.Failed = append(result.Failed, BatchFailure{ID: id, Reason: "not_found"})
		}
	}

	if len(existingIDs) > 0 {
		var update string
		if status == 1 {
			update = `UPDATE channels SET status = 1, fail_count = 0,
				auto_disabled_reason = NULL, updated_at = now()
				WHERE id = ANY($1::bigint[]) AND status <> 1`
		} else {
			update = `UPDATE channels SET status = 2, updated_at = now()
				WHERE id = ANY($1::bigint[]) AND status = 1`
		}
		if _, err := tx.ExecContext(ctx, update, existingIDs); err != nil {
			return result, fmt.Errorf("store: batch status update: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return BatchResult{Succeeded: []int64{}, Failed: []BatchFailure{}},
			fmt.Errorf("store: batch status commit: %w", err)
	}
	return result, nil
}

// BatchDelete 在单事务内删除多行；不存在的 id → failed(not_found)。
func (r *ChannelRepo) BatchDelete(ctx context.Context, ids []int64) (BatchResult, error) {
	ids = dedupeIDs(ids)
	result := BatchResult{Succeeded: []int64{}, Failed: []BatchFailure{}}
	if len(ids) == 0 {
		return result, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("store: batch delete begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `DELETE FROM channels WHERE id = ANY($1::bigint[]) RETURNING id`, ids)
	if err != nil {
		return result, fmt.Errorf("store: batch delete: %w", err)
	}
	deleted := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, fmt.Errorf("store: batch delete scan: %w", err)
		}
		deleted[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("store: batch delete iterate: %w", err)
	}
	_ = rows.Close()

	// 保持入参顺序，前端逐项标记失败行时无需重排。
	for _, id := range ids {
		if _, ok := deleted[id]; ok {
			result.Succeeded = append(result.Succeeded, id)
		} else {
			result.Failed = append(result.Failed, BatchFailure{ID: id, Reason: "not_found"})
		}
	}

	if err := tx.Commit(); err != nil {
		return BatchResult{Succeeded: []int64{}, Failed: []BatchFailure{}},
			fmt.Errorf("store: batch delete commit: %w", err)
	}
	return result, nil
}

// UpdateTestResult 回写连通性测试结果（modules-06 F-06-06）。
//
// updated_at 一并刷新：它是渠道行版本（R-P8），测试结果也是行内容的一部分；
// 正在编辑该渠道的管理员会收到 409 并刷新，而不是用旧快照覆盖测试结果。
func (r *ChannelRepo) UpdateTestResult(ctx context.Context, id int64, at time.Time, success bool) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE channels SET last_test_at = $2, last_test_result = $3, updated_at = now() WHERE id = $1`,
		id, at.UTC(), success)
	if err != nil {
		return fmt.Errorf("store: update test result for channel %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update test result rows: %w", err)
	}
	if n == 0 {
		return ErrChannelNotFound
	}
	return nil
}

// enabledModelsQuery 返回所有启用渠道对外的模型名并集。
//
// 两个来源（modules-06 F-06-10 验收标准「对外 /v1/models 返回映射键」）：
//   - supported_models：渠道原生支持的模型；
//   - model_mapping 的**键**：客户端可见的别名（映射到上游真实模型）。
//
// 只选 status=1：手动/自动禁用渠道不得对外宣告模型。
const enabledModelsQuery = `
	SELECT DISTINCT m
	FROM (
		SELECT jsonb_array_elements_text(supported_models) AS m
		FROM channels WHERE status = 1
		UNION
		SELECT jsonb_object_keys(model_mapping) AS m
		FROM channels WHERE status = 1
	) AS t
	ORDER BY m`

// EnabledModels 返回所有启用渠道支持模型的去重并集（/v1/models 聚合的数据源）。
func (r *ChannelRepo) EnabledModels(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, enabledModelsQuery)
	if err != nil {
		return nil, fmt.Errorf("store: enabled models: %w", err)
	}
	defer rows.Close()

	models := make([]string, 0, 32)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("store: enabled models scan: %w", err)
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: enabled models iterate: %w", err)
	}
	return models, nil
}

// candidatesQuery 构造路由候选查询（relay-01 §5、relay-02 §7.1）：
//   - 仅 status=1（自动/手动禁用均不选，modules-06 §1.2）；
//   - supported_models 含请求模型，**或** model_mapping 有同名键（客户端别名）；
//   - group_tags 为 NULL（全部分组）或含请求分组；
//   - priority 降序；组内加权随机由路由层消费 Weight。
//
// 成员判断用 jsonb 包含（`@> jsonb_build_array($n)`）而不是 `?`：
// channels_*_gin 是 jsonb_path_ops 索引，只支持 `@>`/`@?`/`@@`（finding 5）。
// $1::text 不可省略：pgx v5 stdlib 不推断 jsonb_build_array 参数类型，缺省会
// 报 SQLSTATE 42P18（finding 1 —— Candidates 处于 relay 热路径）。
//
// 别名分支（finding 3）与索引：
//   - jsonb_exists(model_mapping, $1) 是 `?` 的函数形式，语义为「对象含该键」；
//   - M2 DDL 没有 model_mapping 的 GIN 索引，且 jsonb_path_ops **不支持** `?`
//     谓词（它只覆盖 @>/@?/@@）。OR 两侧类型不同，规划器通常无法合并成一次
//     索引扫描，可能退化为顺序扫描；channels 在 M2 规模很小（个位数～百级），
//     这是可接受的正确性优先取舍。若后续量级上来，应新增
//     `USING gin (model_mapping jsonb_ops)`（注意不是 jsonb_path_ops），
//     `jsonb_exists` 才能走索引。
//   - group_tags IS NULL 分支同样不可索引（NULL = 全部分组）。
//
// group != "" 时参数依次为 model、group。
func candidatesQuery(model, group string) (string, []any) {
	query := `SELECT id, priority, weight FROM channels
		WHERE status = 1 AND (supported_models @> jsonb_build_array($1::text)
			OR jsonb_exists(model_mapping, $1))`
	args := []any{model}
	if group != "" {
		query += ` AND (group_tags IS NULL OR group_tags @> jsonb_build_array($2::text))`
		args = append(args, group)
	}
	query += ` ORDER BY priority DESC, id ASC`
	return query, args
}

// Candidates 是路由层的候选来源。ChannelCandidate 的 Group 填本次查询的分组：
// NULL group_tags 的行对所有分组都等价可用，路由快照需要记录「本次以哪个分组命中」。
func (r *ChannelRepo) Candidates(ctx context.Context, model, group string) ([]contract.ChannelCandidate, error) {
	query, args := candidatesQuery(model, group)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: channel candidates: %w", err)
	}
	defer rows.Close()

	candidates := make([]contract.ChannelCandidate, 0, 8)
	for rows.Next() {
		var (
			id       int64
			priority int16
			weight   int16
		)
		if err := rows.Scan(&id, &priority, &weight); err != nil {
			return nil, fmt.Errorf("store: channel candidate scan: %w", err)
		}
		candidates = append(candidates, contract.ChannelCandidate{
			ChannelID: int(id),
			Priority:  int(priority),
			Weight:    uint(weight),
			Group:     group,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: channel candidates iterate: %w", err)
	}
	return candidates, nil
}

// ---------- 内部工具 ----------

func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	// modules-06 F-06-01：页大小 10/20/50/100，默认 20，上限 100。
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}

// buildChannelWhere 生成 WHERE 子句（含前导空格）与参数。
// 所有值都走 $n 占位符；ILIKE 的 %/_ 由 ESCAPE '!' 转义，避免关键字被当作通配符。
func buildChannelWhere(f ChannelFilter) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 6)
	placeholder := func() string { return fmt.Sprintf("$%d", len(args)) }

	if f.Status != nil {
		args = append(args, *f.Status)
		clauses = append(clauses, "status = "+placeholder())
	}
	if len(f.Types) > 0 {
		args = append(args, f.Types)
		// 显式 ::text[]：不依赖驱动对 []string 的隐式类型推断。
		clauses = append(clauses, "type = ANY("+placeholder()+"::text[])")
	}
	if f.Group != "" {
		args = append(args, f.Group)
		// NULL = 全部分组：按任意分组筛选时都必须保留（TC-F-06-01-03）。
		// 包含谓词命中 jsonb_path_ops GIN；IS NULL 分支不可索引（见类型注释）。
		// ::text 原因同 candidatesQuery（pgx stdlib 不推断参数类型 → 42P18）。
		clauses = append(clauses, "(group_tags IS NULL OR group_tags @> jsonb_build_array("+placeholder()+"::text))")
	}
	if f.Model != "" {
		args = append(args, f.Model)
		clauses = append(clauses, "supported_models @> jsonb_build_array("+placeholder()+"::text)")
	}
	if keyword := strings.TrimSpace(f.Keyword); keyword != "" {
		args = append(args, "%"+escapeLike(keyword)+"%")
		ph := placeholder()
		// TC-F-06-01-02：关键字同时匹配 name 与 supported_models，大小写不敏感。
		// 逐元素匹配（而不是 ::text 子串）避免跨数组边界的误命中。
		clauses = append(clauses, `(name ILIKE `+ph+` ESCAPE '!' OR EXISTS (
			SELECT 1 FROM jsonb_array_elements_text(supported_models) AS t(m)
			WHERE m ILIKE `+ph+` ESCAPE '!'))`)
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// escapeLike 转义 LIKE/ILIKE 的通配符；转义符用 '!' 而不是反斜杠，
// 规避 standard_conforming_strings 配置差异。
func escapeLike(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// isUniqueViolation 识别 PostgreSQL 唯一约束冲突（SQLSTATE 23505）。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isCheckViolation 识别 PostgreSQL CHECK 约束冲突（SQLSTATE 23514）。
func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func dedupeIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt32(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return (*p).UTC()
}

// nullableBytes 把 nil 切片显式转成 SQL NULL（jsonb 列语义：
// group_tags NULL = 全部分组，空数组会被 channels_group_tags_chk 拒绝）。
func nullableBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}
