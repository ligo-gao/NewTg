package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 本文件是任务插件平台的只读仓储骨架（M6d Task 1）。
// 只提供管理面「列表 / 详情 / 版本列表」三条最小查询；上传、激活、回滚
// 等写路径由后续任务（T5 管理端点）在同一仓储上扩展，本任务不预置写方法，
// 避免在尚无事务/审计/编译校验消费方时先冻结错误的写语义。
//
// "key" 是 PG 保留字，所有 SQL 文本双引号引用；GORM tag 只写裸列名
// （platform-04 §3.7 V8）。

// ErrTaskPluginNotFound 表示 key 不存在。与 ErrTokenNotFound 同口径：
// 对外只在管理端点映射为 404，不泄露额外信息。
var ErrTaskPluginNotFound = errors.New("store: task plugin not found")

// TaskPluginRepo 是 task_plugins / task_plugin_versions 的只读仓储。
type TaskPluginRepo struct{ db *sql.DB }

// NewTaskPluginRepo 构造任务插件只读仓储。
func NewTaskPluginRepo(db *sql.DB) *TaskPluginRepo { return &TaskPluginRepo{db: db} }

const taskPluginColumns = `"key", name, icon, source, builtin, enabled,
	active_version, description, created_at, updated_at`

// scanTaskPlugin 把一行主表投影扫进模型。
func scanTaskPlugin(row interface{ Scan(...any) error }) (*TaskPlugin, error) {
	var p TaskPlugin
	if err := row.Scan(&p.Key, &p.Name, &p.Icon, &p.Source, &p.Builtin, &p.Enabled,
		&p.ActiveVersion, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// List 返回全部插件主表行，按 key 升序。
//
// 不在此层过滤 enabled：管理列表要展示已停用插件；数据面路由是否可用
// 由 enabled/active 共同决定，属于运行时关注点。
func (r *TaskPluginRepo) List(ctx context.Context) ([]TaskPlugin, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+taskPluginColumns+` FROM task_plugins ORDER BY "key"`)
	if err != nil {
		return nil, fmt.Errorf("store: list task plugins: %w", err)
	}
	defer rows.Close()
	var out []TaskPlugin
	for rows.Next() {
		p, err := scanTaskPlugin(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task plugin: %w", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate task plugins: %w", err)
	}
	return out, nil
}

// Get 按 key 返回插件主表行；不存在返回 ErrTaskPluginNotFound。
func (r *TaskPluginRepo) Get(ctx context.Context, key string) (*TaskPlugin, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	p, err := scanTaskPlugin(r.db.QueryRowContext(ctx,
		`SELECT `+taskPluginColumns+` FROM task_plugins WHERE "key" = $1`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskPluginNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task plugin %q: %w", key, err)
	}
	return p, nil
}

// ListVersions 返回某插件全部版本行，按创建时间倒序（最新在前）。
//
// 排序与「删除 active 后提升上一版本」一致（relay-06 §2.4）；不加
// LIMIT：单插件版本数由上传频率决定，管理面一次拉全量，前端再分页展示。
func (r *TaskPluginRepo) ListVersions(ctx context.Context, key string) ([]TaskPluginVersion, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, "key", version, api_version, source_text, source_hash,
		        icon, enabled, active, uploaded_by, created_at
		   FROM task_plugin_versions WHERE "key" = $1
		  ORDER BY created_at DESC`, key)
	if err != nil {
		return nil, fmt.Errorf("store: list task plugin versions: %w", err)
	}
	defer rows.Close()
	var out []TaskPluginVersion
	for rows.Next() {
		var v TaskPluginVersion
		if err := rows.Scan(&v.ID, &v.Key, &v.Version, &v.APIVersion, &v.SourceText,
			&v.SourceHash, &v.Icon, &v.Enabled, &v.Active, &v.UploadedBy, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan task plugin version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate task plugin versions: %w", err)
	}
	return out, nil
}
