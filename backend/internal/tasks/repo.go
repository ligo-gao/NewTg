package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 本文件是 tasks 表的 SQL 仓储（database/sql + pgx stdlib，与 store 包同口径）。
// 读路径全部按 (user_id, public_task_id) 定位：能力式读取必须 fail-closed，
// 「任务不存在」与「他人任务」返回同一个 ErrTaskNotFound，不制造归属侧信道。

var (
	// ErrTaskNotFound 表示任务不存在或不属于当前用户。
	ErrTaskNotFound = errors.New("tasks: task not found")
	// ErrChannelNotFound 表示没有启用且绑定该插件、支持该模型的渠道。
	ErrChannelNotFound = errors.New("tasks: no task channel for plugin/model")
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
