package plugin

// 本文件把 B-12（task_plugin_versions）适配成运行时 VersionLoader。
//
// 为什么适配层放在内部而不是 store：plugin 包只依赖标准库与 Sobek，而
// store 依赖 GORM 模型与迁移口径。让运行时导入 store 会把「执行引擎」与
// 「数据库形状」绑死；这里只用 database/sql + 固定 SQL，使 plugin 包仍可在
// 没有 store 的单测中运行。
//
// 读路径刻意只取运行时需要的列（不取 icon/uploaded_by 等管理元数据）：
// 运行时快照与 revision 不应随管理字段变化而变化，否则一次改名也会让
// generation 递增（relay-06 §2.3）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SQLVersionLoader 是 VersionLoader 的生产实现。
//
// 查询不筛 enabled/active：Manager 会在 buildSnapshot 中再判一次
// （fail-closed：注入方筛选错误不应让未启用版本被执行）。LoadActive 只取
// active=true 行是 SQL 层的性能优化，语义上等价于全量返回后由 Manager 过滤。
type SQLVersionLoader struct{ db *sql.DB }

// NewSQLVersionLoader 构造 DB 版本加载器。
func NewSQLVersionLoader(db *sql.DB) *SQLVersionLoader { return &SQLVersionLoader{db: db} }

// LoadActive 返回全部 active 版本行。
//
// Revision 使用 created_at 而不是 updated_at：B-12 没有 updated_at 列，
// 版本行是不可变源码（同 (key, version) 异源会在写路径被拒绝），因此
// 创建时间就是该行内容的稳定版本戳。跨节点比较时它随行单调增加。
func (l *SQLVersionLoader) LoadActive(ctx context.Context) ([]ActiveVersion, error) {
	if l == nil || l.db == nil {
		return nil, errors.New("plugin: version loader not configured")
	}
	// 插件主表的 enabled 与版本行 enabled 做逻辑与：/status 关闭的是整个插件
	// （B-11），而运行时的执行判据是 ActiveVersion.Enabled（B-12 投影）。
	// 若只投影版本行，禁用插件不会让数据面失效——这是权限/计费面的静默缺口。
	rows, err := l.db.QueryContext(ctx, `SELECT v."key", v.version, v.source_text, v.source_hash,
		(v.enabled AND p.enabled) AS enabled, v.active, v.created_at
		FROM task_plugin_versions v
		JOIN task_plugins p ON p."key" = v."key"
		WHERE v.active ORDER BY v."key"`)
	if err != nil {
		return nil, fmt.Errorf("plugin: load active versions: %w", err)
	}
	defer rows.Close()

	var out []ActiveVersion
	for rows.Next() {
		var v ActiveVersion
		if err := rows.Scan(&v.Key, &v.Version, &v.SourceText, &v.SourceHash,
			&v.Enabled, &v.Active, &v.UpdatedAt); err != nil {
			return nil, fmt.Errorf("plugin: scan active version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plugin: iterate active versions: %w", err)
	}
	return out, nil
}

// LoadVersion 读取指定 key/version（dryrun 指定版本时使用）。
//
// 不存在返回 (nil, nil)：dryrun 需要区分「版本不存在」这一业务结果并给出
// dryrun_version_not_found，而不是把 sql.ErrNoRows 当作内部故障 500。
func (l *SQLVersionLoader) LoadVersion(ctx context.Context, key, version string) (*ActiveVersion, error) {
	if l == nil || l.db == nil {
		return nil, errors.New("plugin: version loader not configured")
	}
	var v ActiveVersion
	err := l.db.QueryRowContext(ctx, `SELECT v."key", v.version, v.source_text, v.source_hash,
		(v.enabled AND p.enabled) AS enabled, v.active, v.created_at
		FROM task_plugin_versions v
		JOIN task_plugins p ON p."key" = v."key"
		WHERE v."key" = $1 AND v.version = $2`, key, version).
		Scan(&v.Key, &v.Version, &v.SourceText, &v.SourceHash, &v.Enabled, &v.Active, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("plugin: load version %s/%s: %w", key, version, err)
	}
	return &v, nil
}

// HasFactoryKey 报告 key 是否属于内置 factory 层（与当前是否禁用无关）。
//
// 管理端上传一个内置 key 时用它决定主表行的 source/builtin 标记：覆盖层
// 版本来自 upload，但插件的「内置事实」不能因此被改写。
func (m *Manager) HasFactoryKey(key string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.factory[key]
	return ok
}
