package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"newtg/gateway/internal/contract"
)

// 本文件是 tasks 表的 SQL 仓储（database/sql + pgx stdlib，与 store 包同口径）。
// 读路径全部按 (user_id, public_task_id) 定位：能力式读取必须 fail-closed，
// 「任务不存在」与「他人任务」返回同一个 ErrTaskNotFound，不制造归属侧信道。

var (
	// ErrTaskNotFound 表示任务不存在或不属于当前用户。
	ErrTaskNotFound = errors.New("tasks: task not found")
	// ErrChannelNotFound 表示没有启用且绑定该插件、支持该模型的渠道。
	ErrChannelNotFound = errors.New("tasks: no task channel for plugin/model")
	// ErrBatchNotFound 表示批次不存在或不属于当前用户（与 ErrTaskNotFound
	// 同口径：不区分原因，避免批次 id 成为存在性 oracle）。
	ErrBatchNotFound = errors.New("tasks: batch not found")
)

// Repo 是 tasks 表的 SQL 仓储。db 为 nil 时所有方法 fail-closed（返回错误），
// 绝不退化成内存表——生产忘记接线应当立即暴露。
type Repo struct{ db *sql.DB }

// NewRepo 构造仓储。
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ChannelTarget 是提交管线选出的渠道快照。
//
// Credentials 是 channels.credentials 的**密文**（AES-GCM），解密在 httpapi
// 完成：数据面不持有 Sealer，也就没有把明文凭据写进任务行的路径。
type ChannelTarget struct {
	ID           int64
	BaseURL      string
	Credentials  []byte
	ModelMapping map[string]string
	// Status / Type 是只读路由事实（W8/MJ-a2）。
	//
	// 动作类 MJ 任务必须锁定原任务渠道，并在提交前校验「原渠道仍启用」
	// （relay-05 §4.1）。只查 base_url/credentials 无法区分 status=1 与
	// 人工/自动禁用，会把已禁用的渠道当成可用目标，任务必然失败并需要
	// 退款。Type 则用于 Plus-only 端点（midjourney_plus 才能承接）。
	//
	// 零值（0/""）表示调用方没有读取这两列（例如既有插件路径），不得据此
	// 判定「已禁用」——只有 MJ 提交路径会在意它们，且它总是用 GetChannel
	// 的新查询形状读取。
	Status int
	Type   string
}

const insertTaskSQL = `
	INSERT INTO tasks (
		public_task_id, upstream_task_id, family, action, plugin_key,
		plugin_generation, fetch_mode, locked_channel_id, origin_task_ids,
		state, raw_state, progress_percent, retention_deadline,
		user_id, "group", channel_id, quota, fail_reason,
		submit_time, start_time, finish_time,
		properties, private_data, data
	) VALUES (
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
		$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24
	) RETURNING id, created_at, updated_at`

// Insert 写入任务行。零值的 jsonb 列显式序列化为 {} / []：DB 默认值只在
// 列被省略时生效，而本仓储总是显式赋值，避免「零值 = 空对象 vs NULL」歧义。
func (r *Repo) Insert(ctx context.Context, t *Task) error {
	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	origin, err := json.Marshal(defaultStrings(t.OriginTaskIDs))
	if err != nil {
		return fmt.Errorf("tasks: marshal origin_task_ids: %w", err)
	}
	props, err := json.Marshal(defaultMap(t.Properties))
	if err != nil {
		return fmt.Errorf("tasks: marshal properties: %w", err)
	}
	priv, err := json.Marshal(defaultMap(t.PrivateData))
	if err != nil {
		return fmt.Errorf("tasks: marshal private_data: %w", err)
	}
	data, err := json.Marshal(defaultMap(t.Data))
	if err != nil {
		return fmt.Errorf("tasks: marshal data: %w", err)
	}
	return r.db.QueryRowContext(ctx, insertTaskSQL,
		t.PublicTaskID, t.UpstreamTaskID, t.Family, t.Action, t.PluginKey,
		t.PluginGeneration, t.FetchMode, t.LockedChannelID, origin,
		string(t.State), t.RawState, t.ProgressPercent, t.RetentionDeadline,
		t.UserID, t.Group, t.ChannelID, t.Quota, t.FailReason,
		t.SubmitTime, t.StartTime, t.FinishTime,
		props, priv, data,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
}

// taskColumns 是单行读取的列清单，顺序与 scanTask 一一对应。
const taskColumns = `
	id, public_task_id, upstream_task_id, family, action, plugin_key,
	plugin_generation, fetch_mode, locked_channel_id, origin_task_ids,
	state, raw_state, progress_percent, retention_deadline,
	user_id, "group", channel_id, quota, fail_reason,
	submit_time, start_time, finish_time,
	properties, private_data, data, created_at, updated_at`

// GetByPublicID 按归属 + 插件 key 读取任务。
//
// plugin_key 是路径段的一部分：同一个 public_task_id 属于哪个插件是任务行
// 的事实，调用方不得用别的 key 读取（否则插件隔离形同虚设）。
func (r *Repo) GetByPublicID(ctx context.Context, userID int64, publicID, pluginKey string) (*Task, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE user_id = $1 AND public_task_id = $2 AND plugin_key = $3`,
		userID, publicID, pluginKey)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: get task: %w", err)
	}
	return t, nil
}

// ListByUser 返回某用户的任务台账（self 端点）。不返回 data / private_data。
func (r *Repo) ListByUser(ctx context.Context, userID int64, f ListFilter) ([]Task, int, error) {
	return r.list(ctx, f, userID)
}

// ListAdmin 返回全站台账（admin 端点）。userID > 0 时按用户过滤。
func (r *Repo) ListAdmin(ctx context.Context, f ListFilter) ([]Task, int, error) {
	return r.list(ctx, f, f.UserID)
}

// listColumns 是台账投影：刻意不取 data / private_data（设计 §7.1 Omit）。
const listColumns = `
	id, public_task_id, upstream_task_id, family, action, plugin_key,
	plugin_generation, fetch_mode, locked_channel_id, origin_task_ids,
	state, raw_state, progress_percent, retention_deadline,
	user_id, "group", channel_id, quota, fail_reason,
	submit_time, start_time, finish_time,
	properties, created_at, updated_at`

func (r *Repo) list(ctx context.Context, f ListFilter, userID int64) ([]Task, int, error) {
	if r == nil || r.db == nil {
		return nil, 0, errors.New("tasks: repository is not configured")
	}
	page, pageSize := normalizePage(f.Page, f.PageSize)
	where := make([]string, 0, 2)
	args := make([]any, 0, 4)
	if userID > 0 {
		args = append(args, userID)
		where = append(where, fmt.Sprintf("user_id = $%d", len(args)))
	}
	if len(f.States) > 0 {
		placeholders := make([]string, 0, len(f.States))
		for _, st := range f.States {
			args = append(args, string(st))
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ",")+")")
	}
	// 族过滤（R-MJ-18）：两族共用同一张表，台账默认全量；只有显式传
	// family 时才收窄。handler 已按冻结集合校验，这里不做猜测式降级。
	if f.Family != "" {
		args = append(args, f.Family)
		where = append(where, fmt.Sprintf("family = $%d", len(args)))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM tasks`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("tasks: count tasks: %w", err)
	}

	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+listColumns+` FROM tasks`+clause+
			fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)),
		args...)
	if err != nil {
		return nil, 0, fmt.Errorf("tasks: list tasks: %w", err)
	}
	defer rows.Close()

	items := make([]Task, 0, pageSize)
	for rows.Next() {
		t, err := scanListRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("tasks: scan task: %w", err)
		}
		items = append(items, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("tasks: iterate tasks: %w", err)
	}
	return items, total, nil
}

// selectChannelSQL 按 T7a 的绑定列挑选渠道。
//
// 排序冻结为 priority DESC, weight DESC, id ASC：设计要求「按 priority/weight
// 选一个」；加权随机会让同一输入的测试不可复现，且需要额外的 RNG 接缝。
// 这里的确定性次序等价于「最高优先级的最高权重，再按 id 稳定」，把模型路由
// 的随机性留给 T6b（如需）在候选集上实现。
//
// 单绑（task_plugin_key）与类型 60 的多绑（task_extend_plugin_keys @>）都要
// 命中；@> 走 jsonb_path_ops GIN 索引。group 为空表示调用方没有分组上下文，
// 只按 NULL/通配处理（不追加 group_tags 条件会放宽可见性，故显式分支）。
const selectChannelSQL = `
	SELECT id, base_url, credentials, model_mapping
	FROM channels
	WHERE status = 1
	  AND (task_plugin_key = $1 OR task_extend_plugin_keys @> jsonb_build_array($1::text))
	  AND (supported_models @> jsonb_build_array($2::text) OR jsonb_exists(model_mapping, $2))
	  AND ($3::text = '' OR group_tags IS NULL OR group_tags @> jsonb_build_array($3::text))
	ORDER BY priority DESC, weight DESC, id ASC
	LIMIT 1`

// GetChannel 按渠道 ID 读取任务出站所需的渠道快照。
//
// 与 SelectChannel 不同，这里**不按 status 过滤**：任务一旦提交成功，渠道后来
// 被管理员禁用不能让它永远无法轮询/取产物（那会把已预扣的钱锁死）。凭据仍是
// 密文，解密只在调用方内存中发生。
func (r *Repo) GetChannel(ctx context.Context, id int64) (*ChannelTarget, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	var (
		baseURL sql.NullString
		cred    []byte
		mapping []byte
		status  int
		typ     string
	)
	err := r.db.QueryRowContext(ctx,
		`SELECT base_url, credentials, model_mapping, status, type FROM channels WHERE id = $1`, id).
		Scan(&baseURL, &cred, &mapping, &status, &typ)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: get channel: %w", err)
	}
	out := &ChannelTarget{
		ID: id, BaseURL: strings.TrimSpace(baseURL.String), Credentials: cred,
		Status: status, Type: typ,
	}
	if len(mapping) > 0 {
		if err := json.Unmarshal(mapping, &out.ModelMapping); err != nil {
			return nil, fmt.Errorf("tasks: decode channel model_mapping: %w", err)
		}
	}
	return out, nil
}

// SelectChannel 返回启用、绑定插件 key、支持模型的渠道（最高优先级）。
// BaseURL 为 NULL 时返回空串；调用方必须把空基址判成配置错误。
func (r *Repo) SelectChannel(ctx context.Context, pluginKey, model, group string) (*ChannelTarget, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	var (
		id      int64
		baseURL sql.NullString
		cred    []byte
		mapping []byte
	)
	err := r.db.QueryRowContext(ctx, selectChannelSQL, pluginKey, model, group).
		Scan(&id, &baseURL, &cred, &mapping)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChannelNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: select channel: %w", err)
	}
	out := &ChannelTarget{ID: id, BaseURL: strings.TrimSpace(baseURL.String), Credentials: cred}
	if len(mapping) > 0 {
		if err := json.Unmarshal(mapping, &out.ModelMapping); err != nil {
			// model_mapping 是不可信的渠道配置；解析失败视为「无映射」会让上游
			// 收到未映射模型名，故按配置错误返回，由提交路径 502 并退款。
			return nil, fmt.Errorf("tasks: decode channel model_mapping: %w", err)
		}
	}
	return out, nil
}

// GetByID 按主键读取任务（轮询器/补偿扫描用）。不带 user_id 过滤：后台
// 驱动者不是某个用户，归属过滤只属于客户端读取面。
func (r *Repo) GetByID(ctx context.Context, id int64) (*Task, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	t, err := scanTask(r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: get task by id: %w", err)
	}
	return t, nil
}

// GetByPublicIDAny 是产物能力 URL 的读取面：按 public_task_id 全局读取，
// 不带 user_id（能力式 URL 本身就是授权凭证）。
//
// 返回后调用方必须核对行上的 plugin_key 与 URL 路径段一致——否则同一个
// task_id 可以借另一个插件的路径读取产物。
func (r *Repo) GetByPublicIDAny(ctx context.Context, publicID string) (*Task, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	t, err := scanTask(r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE public_task_id = $1`, publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: get task by public id: %w", err)
	}
	return t, nil
}

// GetByUserUpstreamID 按 (user_id, upstream_task_id) 读取本族的任务行。
//
// 【归属是复用键，不是全局唯一键（R-MJ-05）】上游 code 21「任务已存在」
// 可能让不同用户在各自请求里拿到同一个 MjId；把 upstream_task_id 当全局
// 唯一会迫使第二个用户复用别人的行（跨用户数据泄露）或无法落库。
// 因此查询必须同时带 user_id，并限定 family='mj'——插件任务即便巧合有
// 同名 upstream id 也不得成为 MJ 动作任务的锁定来源。
//
// 同一个用户理论上仍可能有多行（历史上重复提交且上游返回同一 MjId）。
// 取 id 最大的一行：那是最新一次受理，也是客户端最近看到的任务。
func (r *Repo) GetByUserUpstreamID(ctx context.Context, userID int64, upstreamID string) (*Task, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	if userID <= 0 || strings.TrimSpace(upstreamID) == "" {
		// 空 id 绝不查库：空串会匹配所有 inline 上传行（upstream_task_id=''），
		// 把「没有原任务」伪装成「找到了一个上传任务」。
		return nil, ErrTaskNotFound
	}
	t, err := scanTask(r.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE user_id = $1 AND upstream_task_id = $2 AND family = $3
		 ORDER BY id DESC LIMIT 1`,
		userID, upstreamID, FamilyMJ))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: get task by user/upstream id: %w", err)
	}
	return t, nil
}

// ListByUserUpstreamIDs 批量读取同一用户、同一族内的任务行（MJ-b 的
// list-by-condition）。
//
// 【为什么必须带 user_id + family】批量查询只返回本人任务：把 ids 直接
// 当成全局键会跨用户泄露他人任务的存在与产物（R-MJ-05/18）；family 过滤
// 防止插件行即便巧合有同名 upstream id 被 MJ 批量查询命中。
//
// 输入 ids 的去重/顺序由调用方负责：这里返回数据库顺序，调用方按输入
// 顺序重组（重复 id 只落一行，避免同一任务在响应数组里出现两次）。
func (r *Repo) ListByUserUpstreamIDs(ctx context.Context, userID int64, ids []string, limit int) ([]*Task, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	if userID <= 0 || len(ids) == 0 {
		return []*Task{}, nil
	}
	// 上限是成本边界：调用方（R-MJ-16）已限制 ids ≤ 100，这里再钳一次，
	// 防止内部调用方绕过归一化层放大单次查询。
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return r.scanTasks(r.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE user_id = $1 AND family = $2 AND upstream_task_id = ANY($3::text[])
		 ORDER BY id ASC LIMIT $4`,
		userID, FamilyMJ, ids, limit))
}

// ListOwnersByUpstreamID 按 MjId 全局取候选任务行（W8/MJ-c 图片代理）。
//
// 【为什么不能带 user_id】图片代理在验签前只知道 URL 里的 MjId，不知道 owner；
// 签名又恰好绑定 owner_user_id。因此只能先按 MjId 取出候选行，再逐个用候选
// owner 复算 HMAC，任一匹配即通过（R-MJ-05 备选 A）。
//
// 【为什么有上限且固定排序】同一 MjId 理论上可能被大量用户各拿到一次（上游
// code 21）。无界查询 + 逐候选验证会变成一次请求放大成全表扫描的 DoS 面，
// 因此截断到 100（mj.MaxOwnerCandidates 的同一冻结值）。排序固定为
// (user_id, id)，保证截断结果确定、可复现——否则每次请求可能验证不同的
// 候选集合，行为不可诊断。
//
// 【family 过滤】插件行即便巧合有同名 upstream id 也不得成为图片候选：
// 图片代理只服务 family='mj' 的产物。
func (r *Repo) ListOwnersByUpstreamID(ctx context.Context, upstreamID string, limit int) ([]*Task, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	// 空 id 绝不查库：空串会匹配所有 inline 上传行（upstream_task_id=''），
	// 把它们当成「MjId 的候选 owner」。
	if strings.TrimSpace(upstreamID) == "" {
		return []*Task{}, nil
	}
	// 与 mj.MaxOwnerCandidates 同值；tasks 不能反向 import mj（mj 已 import
	// tasks），因此在这里再钳一次，防止内部调用方绕过归一化放大查询。
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return r.scanTasks(r.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE upstream_task_id = $1 AND family = $2
		 ORDER BY user_id ASC, id ASC LIMIT $3`,
		upstreamID, FamilyMJ, limit))
}

// MJChannelCandidate 是 MJ 提交路径的渠道候选投影。
//
// 与 contract.ChannelCandidate（中继路由用，只含 id/priority/weight）分开：
// MJ 的领域层选择函数需要 type/status/supported_models/model_mapping 才能
// 做「Plus-only、类型过滤、模型命中」三件事。本类型是数据面 → 领域层的
// 传输形状，不携带凭据（凭据在选中后再按 id 单独读取）。
type MJChannelCandidate struct {
	ID              int64
	Type            string
	Status          int
	Priority        int
	Weight          uint
	SupportedModels []string
	ModelMapping    map[string]string
}

// mjChannelTypes 是 MJ 渠道类型集合（SQL 参数真源）。
//
// 从 contract 的冻结通道类型派生（M6）：SQL 里的 `type = ANY(...)` 与领域层
// 的 EligibleChannels 必须覆盖同一集合；任一侧改名而另一侧没跟上，会让
// 「渠道明明存在却永远选不中」这种静默故障出现。测试
// TestMJChannelTypesMatchContract 冻结本变量与 contract 常量的一致性。
var mjChannelTypes = []string{
	string(contract.ChannelTypeMidjourney),
	string(contract.ChannelTypeMidjourneyPlus),
}

// MJChannelCandidates 返回可能服务该模型的 MJ 渠道（relay-05 §3.4）。
//
// 只按「MJ 渠道类型 + 模型命中 + 分组可见」取候选，**不在 SQL 里做加权
// 随机/优先级选择**：选择策略在 mj.SelectChannel 纯函数里，测试可穷举
// 优先级/权重/Plus-only 边界。SQL 里过滤掉非 MJ 类型（重要）：OpenAI
// 渠道若被误配了 mj_* 模型，绝不能参与 MJ 提交。
func (r *Repo) MJChannelCandidates(ctx context.Context, model, group string) ([]MJChannelCandidate, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("tasks: mj channel candidates require a model")
	}
	// 类型集合参数化（M6）：SQL 里不再硬编码渠道类型字面量，唯一真源是
	// contract.ChannelType* → mjChannelTypes；测试断言三者不漂移。
	query := `SELECT id, type, status, priority, weight, supported_models, model_mapping
		FROM channels
		WHERE status = 1
		  AND type = ANY($1::text[])
		  AND (supported_models @> jsonb_build_array($2::text)
		       OR jsonb_exists(model_mapping, $2))`
	args := []any{mjChannelTypes, model}
	if group != "" {
		query += ` AND (group_tags IS NULL OR group_tags @> jsonb_build_array($3::text))`
		args = append(args, group)
	}
	query += ` ORDER BY priority DESC, id ASC`

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tasks: mj channel candidates: %w", err)
	}
	defer rows.Close()
	out := make([]MJChannelCandidate, 0, 4)
	for rows.Next() {
		var (
			candidate     MJChannelCandidate
			supported     []byte
			mapping       []byte
			priority, wgt int
		)
		if err := rows.Scan(&candidate.ID, &candidate.Type, &candidate.Status,
			&priority, &wgt, &supported, &mapping); err != nil {
			return nil, fmt.Errorf("tasks: mj channel candidate scan: %w", err)
		}
		candidate.Priority = priority
		if wgt > 0 {
			candidate.Weight = uint(wgt)
		}
		if len(supported) > 0 {
			if err := json.Unmarshal(supported, &candidate.SupportedModels); err != nil {
				return nil, fmt.Errorf("tasks: decode mj channel supported_models: %w", err)
			}
		}
		if len(mapping) > 0 {
			if err := json.Unmarshal(mapping, &candidate.ModelMapping); err != nil {
				return nil, fmt.Errorf("tasks: decode mj channel model_mapping: %w", err)
			}
		}
		out = append(out, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tasks: mj channel candidates iterate: %w", err)
	}
	return out, nil
}

// ListPendingByFamily 返回指定家族未完成的任务（relay-06 §8.1：state NOT IN 终态）。
//
// 【为什么必须带 family（R-MJ-04，强制项）】tasks 是插件族与 MJ 族共用的同一张
// 表（relay-05:238-240、relay-06:64）。插件 poller 若不按 family 过滤，会把
// MJ 行当成插件任务领取：先累计 plugin_unavailable，约 20×15s 后强制
// FAILURE 并退款（poller.go 的失败处理）——而 MJ 上游此时可能仍在产出，
// 退款等于把「结果已生成」白送给用户，是直接的资金损失；同时 MJ 的 Action
// 解释权（family=mj 的动作族）也会被插件钩子错误解释。
//
// 按最近一次被轮询的时间升序（id 兜底）：ApplyPoll / IncrementPollFailures
// 都会把 updated_at 推到 now()，于是「刚轮询过的」自动排到队尾。按 id 升序
// 会让超过单轮 LIMIT 时 id 大的任务永远进不了这一批——视频/动画这类后提交
// 的长任务正好被饿死，用户看到一直「处理中」、hold 也不解（P1-7）。
//
// 代价：现有索引都不支持这个排序——tasks_family_pending_idx 是
// (family, id)（部分谓词 state NOT IN 终态），tasks_pending_idx 只覆盖 state。
// 查询对匹配 family 的全部在飞行行做一次排序后才截断，LIMIT 不限制排序输入；
// 在飞行规模大时应补 (family, updated_at, id) 索引，当前量级可接受。
//
// 未知 family 返回错误而不是空集：静默空结果会把调用方拼错家族名伪装成
// 「本轮没有任务」，让资金侧停摆不可见。
func (r *Repo) ListPendingByFamily(ctx context.Context, family string, limit int) ([]*Task, error) {
	if !ValidFamily(family) {
		return nil, fmt.Errorf("tasks: list pending: unknown family %q", family)
	}
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	if limit <= 0 {
		limit = 1000
	}
	return r.scanTasks(r.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE family = $1 AND state NOT IN ('success', 'failure')
		 ORDER BY updated_at ASC, id ASC LIMIT $2`, family, limit))
}

// ListReconcilable 返回指定家族中可能需要补偿的终态任务（本批最小版本）。
//
// 判定依据是 private_data 上的布尔标记：billing_pending 在终态 CAS 的同一
// 语句里写入，覆盖「状态已终态但结算/退款尚未发生就崩溃」的窗口；settlement_pending
// / refund_pending 是 T6a/T6b 结算失败时留下的标记。
//
// 【family 过滤的作用（R-MJ-04）】补偿 = 按该族的计费语义动钱。插件 poller
// 的补偿扫描只认领 family='plugin'，明确排除 MJ 行：MJ 的结算时机（R-MJ-01：
// 提交 hold → 终态终结/退款）与提交路径要到 MJ-a2/d 才接线，在接线前让插件
// 补偿器替 MJ 行结算等于在未知语义上动资金。MJ 族自己的补偿扫描随 MJ-a2
// （写入 request_id）一起接；settleTerminal 本身对两族是同一套「SUCCESS 按
// 预扣额结清 / FAILURE 全额退款」语义，届时只需传入 FamilyMJ。
//
// 扫描只取最近的终态行没有索引可依（partial pending 索引只覆盖未完成态）；
// LIMIT 让单轮成本有界，索引优化与分页属 T6c。
func (r *Repo) ListReconcilable(ctx context.Context, family string, limit int) ([]*Task, error) {
	if !ValidFamily(family) {
		return nil, fmt.Errorf("tasks: list reconcilable: unknown family %q", family)
	}
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	if limit <= 0 {
		limit = 200
	}
	return r.scanTasks(r.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks
		 WHERE family = $1
		   AND state IN ('success', 'failure')
		   AND (private_data @> '{"billing_pending": true}'::jsonb
		        OR private_data @> '{"settlement_pending": true}'::jsonb
		        OR private_data @> '{"refund_pending": true}'::jsonb)
		 ORDER BY id ASC LIMIT $2`, family, limit))
}

func (r *Repo) scanTasks(rows *sql.Rows, err error) ([]*Task, error) {
	if err != nil {
		return nil, fmt.Errorf("tasks: list tasks: %w", err)
	}
	defer rows.Close()
	out := make([]*Task, 0, 16)
	for rows.Next() {
		t, scanErr := scanTask(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("tasks: scan task: %w", scanErr)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tasks: iterate tasks: %w", err)
	}
	return out, nil
}

// PollUpdate 是一次轮询 CAS 的写入投影。
//
// 指针字段用 COALESCE 写成「为 nil 则保留原值」：轮询只更新它观察到的事实，
// 未提供的列不得被零值覆盖（例如失败的 PollFailures++ 不能顺手把 quota 清零）。
//
// Data / Private 是**增量**（jsonb ||）：上游快照与插件状态必须合并而不是
// 替换，否则一次只返回 progress 的响应会把已有产物冲掉。
type PollUpdate struct {
	State      State
	RawState   string
	Progress   int
	Data       map[string]any
	Private    map[string]any
	FailReason *string
	FinishTime *time.Time
	Quota      *int64
	// RetentionDeadline 在进入终态时写一次（relay-05 §9）。nil = 保留原值，
	// 与其它指针字段同口径；清理 worker 是后续任务，本字段只负责留下期限。
	RetentionDeadline *time.Time
}

// ApplyPoll 以 (id, fromState) 为条件更新任务行，返回是否赢得 CAS。
//
// CAS 是「终态恰好一次结算」的锚点：并发轮询/补偿同时推进同一任务时只有
// 一个 UPDATE 能命中 fromState。fromState 被别处推进后返回 false，调用方
// 必须放弃本轮（不结算、不退款）。
func (r *Repo) ApplyPoll(ctx context.Context, id int64, from State, u PollUpdate) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("tasks: repository is not configured")
	}
	data, err := json.Marshal(defaultMap(u.Data))
	if err != nil {
		return false, fmt.Errorf("tasks: marshal poll data: %w", err)
	}
	priv, err := json.Marshal(defaultMap(u.Private))
	if err != nil {
		return false, fmt.Errorf("tasks: marshal poll private_data: %w", err)
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE tasks SET
			state = $3, raw_state = $4, progress_percent = $5,
			data = data || $6::jsonb,
			private_data = private_data || $7::jsonb,
			fail_reason = COALESCE($8, fail_reason),
			finish_time = COALESCE($9, finish_time),
			quota = COALESCE($10, quota),
			retention_deadline = COALESCE($11, retention_deadline),
			updated_at = now()
		WHERE id = $1 AND state = $2`,
		id, string(from), string(u.State), u.RawState, u.Progress,
		data, priv, u.FailReason, u.FinishTime, u.Quota, u.RetentionDeadline,
	)
	if err != nil {
		return false, fmt.Errorf("tasks: apply poll: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("tasks: apply poll rows: %w", err)
	}
	return affected > 0, nil
}

// IncrementPollFailures 以 (id, from_state) 原子递增连续失败计数。
//
// jsonb_set + COALESCE 让递增发生在 SQL 内：并发轮询各自 +1，不会互相覆盖
// （读-改-写只能留下最后一次写）。CAS 条件仍是 from_state：被别的执行者
// 推进到其它状态后返回 won=false，调用方必须放弃。
func (r *Repo) IncrementPollFailures(ctx context.Context, id int64, from State) (int, bool, error) {
	if r == nil || r.db == nil {
		return 0, false, errors.New("tasks: repository is not configured")
	}
	var failures int
	err := r.db.QueryRowContext(ctx, `
		UPDATE tasks SET
			private_data = jsonb_set(
				private_data, '{poll_failures}',
				to_jsonb(COALESCE((private_data->>'poll_failures')::int, 0) + 1),
				true),
			updated_at = now()
		WHERE id = $1 AND state = $2
		RETURNING COALESCE((private_data->>'poll_failures')::int, 0)`,
		id, string(from)).Scan(&failures)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("tasks: increment poll failures: %w", err)
	}
	return failures, true, nil
}

// UpdateBilling 写结算侧事实（quota 与 private_data 标记），不带 CAS。
//
// 结算发生在终态 CAS 之后；此处失败只意味着「标记没落库」，调用方必须把它
// 当作结算失败上报，绝不能当成功——否则下一次补偿扫描看不到这笔待办。
// UpdateBilling 合并 billing 标记（不覆盖其它 private_data 键）。
//
// 【JSON null = 删除键】复核 Minor-b 需要「封账后清除 settlement_pending_*
// 审计字段」。`public || patch` 会把 null 写成一个永久 null 值，所以这里
// 把补丁里值为 null 的键从结果中减去：调用方用 nil 表达“删除”，无需再开
// 一个专用 SQL 路径。
func (r *Repo) UpdateBilling(ctx context.Context, id int64, quota *int64, private map[string]any) error {
	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	priv, err := json.Marshal(defaultMap(private))
	if err != nil {
		return fmt.Errorf("tasks: marshal billing private_data: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET quota = COALESCE($2, quota),
		        private_data = (private_data || $3::jsonb)
		            - COALESCE((SELECT array_agg(k) FROM jsonb_each($3::jsonb) AS e(k, v)
		                        WHERE v = 'null'::jsonb), '{}'::text[]),
		        updated_at = now()
		 WHERE id = $1`, id, quota, priv); err != nil {
		return fmt.Errorf("tasks: update billing: %w", err)
	}
	return nil
}

// ---------- 批次（T4 / P-04） ----------

const insertBatchSQL = `
	INSERT INTO task_batches (
		id, user_id, token_id, status, total, completed, failed
	) VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING created_at, updated_at`

// InsertBatch 写入父批次行（单条 INSERT，无子任务关联）。
//
// 顺序由 R-16 冻结为「先落父行 → 逐行提交 → 逐行关联」：
//   - 父行在任何计费/上游调用之前落库，失败即为 500 且零副作用；
//   - 逐行提交成功后用 LinkBatchItem 关联，父行已存在，客户端始终拿得到
//     batch id，绝不会出现「已扣费但任务不可见」；
//   - 关联失败的行按 failed 计入聚合（total - 已关联数），不需要第二套状态机。
//
// 因此本方法不接收子任务 ID：子任务在提交之前根本不存在，把关联放进同一个
// 事务只能在提交之前持有长事务（网络 I/O + 预扣），并让「先提交后落库失败」
// 的窗口重新出现。
func (r *Repo) InsertBatch(ctx context.Context, b *Batch) error {
	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	if b == nil || strings.TrimSpace(b.ID) == "" {
		return errors.New("tasks: batch without id")
	}
	if err := r.db.QueryRowContext(ctx, insertBatchSQL,
		b.ID, b.UserID, b.TokenID, b.Status, b.Total, b.Completed, b.Failed,
	).Scan(&b.CreatedAt, &b.UpdatedAt); err != nil {
		return fmt.Errorf("tasks: insert batch: %w", err)
	}
	return nil
}

// LinkBatchItem 把一条已落库的子任务关联到父批次。
//
// 单条语句即是原子的 INSERT ... SELECT：只有「本用户的该 public_task_id」
// 才能建立关联，RowsAffected != 1（任务不存在/不属于本用户）按错误返回。
// 调用方在每条提交成功后立即调用，使「已计费但未关联」的窗口最小；少数
// 关联失败的行由聚合口径按 failed 计入（见 GetBatchByID）。
func (r *Repo) LinkBatchItem(ctx context.Context, batchID string, userID int64, publicID string) error {
	if r == nil || r.db == nil {
		return errors.New("tasks: repository is not configured")
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO task_batches_items (batch_id, task_id)
		SELECT $1, id FROM tasks WHERE public_task_id = $2 AND user_id = $3`,
		batchID, publicID, userID)
	if err != nil {
		return fmt.Errorf("tasks: link batch item: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("tasks: link batch item rows: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("tasks: link batch item %q: task not found for user", publicID)
	}
	return nil
}

// GetBatchByID 读取批次聚合视图（归属过滤：他人批次与不存在同为 ErrBatchNotFound）。
//
// 聚合状态为什么在读取时算而不是落库时算：子任务状态由轮询器异步推进
// （T6b），本任务没有批次 worker；把计数写死在创建期会让 GET 永远看到
// in_progress。读取是唯一观测点，因此由 task_batches_items ⋈ tasks 现算。
//
// 读与写必须是同一条语句（复核 M4）：先 SELECT 再按旧值 UPDATE 的写法在并发
// GET 下可能用更旧的快照覆盖新值。这里用 UPDATE ... FROM (聚合子查询) 让
// 计数/状态的计算与回写共享同一语句快照；子查询只读本批次关联行，成本受
// (batch_id, task_id) 主键与 task_id 索引限制。items[] 列表另起一条查询，
// 只用于展示（状态可能比计数新一轮，不会影响聚合的单调性）。
//
// 提交失败的子请求没有 tasks 行（提交管线失败不落孤儿行，见 handleSubmit），
// 它们计入 failed：failed = 状态为 failure 的子任务 + (total - 已关联数)。
func (r *Repo) GetBatchByID(ctx context.Context, userID int64, id string) (*Batch, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("tasks: repository is not configured")
	}
	b := &Batch{}
	err := r.db.QueryRowContext(ctx, `
		UPDATE task_batches b SET
			completed = agg.completed,
			failed    = agg.failed + GREATEST(b.total - agg.linked, 0),
			status = CASE
				WHEN b.total = 0 THEN 'failed'
				WHEN agg.completed + agg.failed + GREATEST(b.total - agg.linked, 0) >= b.total
				     AND agg.completed = 0 THEN 'failed'
				WHEN agg.completed + agg.failed + GREATEST(b.total - agg.linked, 0) >= b.total
				     THEN 'completed'
				ELSE 'in_progress'
			END,
			updated_at = now()
		FROM (
			SELECT count(*) FILTER (WHERE t.state = 'success') AS completed,
			       count(*) FILTER (WHERE t.state = 'failure') AS failed,
			       count(*) AS linked
			FROM task_batches_items i
			JOIN tasks t ON t.id = i.task_id
			WHERE i.batch_id = $1
		) agg
		WHERE b.id = $1 AND b.user_id = $2
		RETURNING b.id, b.user_id, b.token_id, b.status, b.total, b.completed, b.failed, b.created_at, b.updated_at`,
		id, userID).
		Scan(&b.ID, &b.UserID, &b.TokenID, &b.Status, &b.Total, &b.Completed, &b.Failed, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBatchNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tasks: get batch: %w", err)
	}
	b.ID = FormatBatchPublicID(b.ID)

	rows, err := r.db.QueryContext(ctx, `
		SELECT t.public_task_id, t.state, t.progress_percent
		FROM task_batches_items i
		JOIN tasks t ON t.id = i.task_id
		WHERE i.batch_id = $1
		ORDER BY t.id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("tasks: list batch items: %w", err)
	}
	defer rows.Close()
	items := make([]BatchItem, 0, b.Total)
	for rows.Next() {
		var (
			item  BatchItem
			state string
		)
		if err := rows.Scan(&item.PublicTaskID, &state, &item.ProgressPercent); err != nil {
			return nil, fmt.Errorf("tasks: scan batch item: %w", err)
		}
		item.State = State(state)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tasks: iterate batch items: %w", err)
	}
	b.Items = items
	return b, nil
}

// ---------- 扫描 ----------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*Task, error) {
	var (
		t                                Task
		origin, props, priv, data        []byte
		state                            string
		retention, submit, start, finish sql.NullTime
	)
	if err := row.Scan(
		&t.ID, &t.PublicTaskID, &t.UpstreamTaskID, &t.Family, &t.Action, &t.PluginKey,
		&t.PluginGeneration, &t.FetchMode, &t.LockedChannelID, &origin,
		&state, &t.RawState, &t.ProgressPercent, &retention,
		&t.UserID, &t.Group, &t.ChannelID, &t.Quota, &t.FailReason,
		&submit, &start, &finish,
		&props, &priv, &data, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	t.State = State(state)
	t.RetentionDeadline = nullTime(retention)
	t.SubmitTime = nullTime(submit)
	t.StartTime = nullTime(start)
	t.FinishTime = nullTime(finish)
	t.OriginTaskIDs = decodeStrings(origin)
	t.Properties = decodeMap(props)
	t.PrivateData = decodeMap(priv)
	t.Data = decodeMap(data)
	return &t, nil
}

func scanListRow(row rowScanner) (*Task, error) {
	var (
		t                                Task
		origin, props                    []byte
		state                            string
		retention, submit, start, finish sql.NullTime
	)
	if err := row.Scan(
		&t.ID, &t.PublicTaskID, &t.UpstreamTaskID, &t.Family, &t.Action, &t.PluginKey,
		&t.PluginGeneration, &t.FetchMode, &t.LockedChannelID, &origin,
		&state, &t.RawState, &t.ProgressPercent, &retention,
		&t.UserID, &t.Group, &t.ChannelID, &t.Quota, &t.FailReason,
		&submit, &start, &finish,
		&props, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	t.State = State(state)
	t.RetentionDeadline = nullTime(retention)
	t.SubmitTime = nullTime(submit)
	t.StartTime = nullTime(start)
	t.FinishTime = nullTime(finish)
	t.OriginTaskIDs = decodeStrings(origin)
	t.Properties = decodeMap(props)
	return &t, nil
}

func nullTime(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

func decodeStrings(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func decodeMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func defaultStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func defaultMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

// normalizePage 与 channels 台账同口径：页大小 10/20/50/100 中的 20 默认、
// 上限 100。上限是成本边界（一次最多拉 100 行含 jsonb 快照）。
func normalizePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
