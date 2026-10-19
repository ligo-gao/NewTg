package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 本文件是任务插件平台的仓储：M6d Task 1 提供列表/详情/版本列表三条读查询，
// M6d Task 5 在同一仓储上补齐上传、激活、启停、版本删除与市场源写路径。
// 读路径刻意不过滤 enabled：管理列表要展示已停用插件，数据面是否可用由
// 运行时注册表按 enabled/active 判定。
//
// "key" 是 PG 保留字，所有 SQL 文本双引号引用；GORM tag 只写裸列名
// （platform-04 §3.7 V8）。

// ErrTaskPluginNotFound 表示 key 不存在。与 ErrTokenNotFound 同口径：
// 对外只在管理端点映射为 404，不泄露额外信息。
var ErrTaskPluginNotFound = errors.New("store: task plugin not found")

// ErrTaskPluginVersionNotFound 表示 key 存在但版本行不存在。
var ErrTaskPluginVersionNotFound = errors.New("store: task plugin version not found")

// ErrTaskPluginVersionConflict 表示同 (key, version) 已存在且 source_hash 不同。
// relay-06 §2.4 冻结「同 key/version 异源拒绝」：版本号是内容标识，
// 静默覆盖会让已激活版本与审计记录指向不同源码。
var ErrTaskPluginVersionConflict = errors.New("store: task plugin version conflict")

// ErrTaskPluginVersionRequired 表示删除会留下无 active 版本的 key（S4）：
// 唯一版本不可删；删除 active 但没有任何 enabled 的旧版本可提升时同样拒绝。
var ErrTaskPluginVersionRequired = errors.New("store: task plugin version required")

// ErrTaskPluginStale 表示 expect_updated_at 与当前行不一致（409 乐观锁）。
var ErrTaskPluginStale = errors.New("store: task plugin updated_at conflict")

// ErrMarketSourceNotFound 表示市场源 id 不存在。
var ErrMarketSourceNotFound = errors.New("store: marketplace source not found")

// ErrMarketSourceURLConflict 表示整份替换的输入里出现重复 URL（含与其它行
// 大小写不同的同一 URL）；DB 的 UNIQUE(url) 是兜底，应用层给出可读错误。
var ErrMarketSourceURLConflict = errors.New("store: marketplace source url conflict")

// TaskPluginUpload 是一次上传入库的完整输入。
//
// SourceHash 必须由调用方（服务端）计算，绝不接受客户端提交的 hash（S3）；
// 本层只落库与比对，不做编译/校验（S1：编译失败零写入，编译发生在本层之前）。
type TaskPluginUpload struct {
	Key         string
	Name        string
	Version     string
	APIVersion  string
	SourceText  string
	SourceHash  string
	Icon        *string
	Description *string
	UploadedBy  int64
	// Builtin 表示这个 key 同时是 factory 层内置插件（由装配层依据内置源
	// 清单注入）。它只影响主表行的 source/builtin 标记：覆盖层版本仍来自
	// upload，B-11 的「内置事实」不能因为一次上传而被抹掉。
	Builtin bool
}

// BuiltinPlugin 是内置 factory 插件的主表元数据（装配期幂等 upsert 的输入）。
//
// 内置插件在编译期编入二进制，但 B-11 仍需要行：列表/详情/启停都读主表；
// 没有行会让「已安装」列表永远为空（F-08-28）。
type BuiltinPlugin struct {
	Key         string
	Name        string
	Icon        *string
	Description *string
}

// MarketSourceInput 是整份替换时的一行。ID 为空表示新源（按 URL 匹配已有行，
// 以便保存后保留既有校验状态）；ID 非空但行已不存在时按新源插入。
type MarketSourceInput struct {
	ID      string
	Name    string
	URL     string
	Enabled bool
}

// TaskPluginRepo 是 task_plugins / task_plugin_versions 的只读仓储。
type TaskPluginRepo struct{ db *sql.DB }

// NewTaskPluginRepo 构造任务插件只读仓储。
func NewTaskPluginRepo(db *sql.DB) *TaskPluginRepo { return &TaskPluginRepo{db: db} }

const taskPluginColumns = `"key", name, icon, source, builtin, enabled,
	active_version, description, created_at, updated_at`

// taskPluginVersionColumns 是 B-12 的固定列序；写路径与读路径共用，
// 避免两边列序漂移导致 scan 错位。
const taskPluginVersionColumns = `id, "key", version, api_version, source_text, source_hash,
	icon, enabled, active, uploaded_by, created_at`

// taskPluginVersionMetaColumns 是版本列表投影：**不含 source_text**。
// 列表/详情默认不得下发源码（S5）；源查看器与 include_source=1 通过
// GetVersion 单独读取正文。
const taskPluginVersionMetaColumns = `id, "key", version, api_version, source_hash,
	icon, enabled, active, uploaded_by, created_at`

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
		`SELECT `+taskPluginVersionMetaColumns+`
		   FROM task_plugin_versions WHERE "key" = $1
		  ORDER BY created_at DESC`, key)
	if err != nil {
		return nil, fmt.Errorf("store: list task plugin versions: %w", err)
	}
	defer rows.Close()
	var out []TaskPluginVersion
	for rows.Next() {
		v, err := scanTaskPluginVersionMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan task plugin version: %w", err)
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate task plugin versions: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 写路径（M6d Task 5）
//
// 所有写方法都在事务内完成，并且：
//   - 激活/启停/删除先 `SELECT ... FOR UPDATE` 锁插件主表行。主表行是同一 key
//     的串行化点：并发激活两个版本时，第二个事务在拿到锁后会重新读状态，
//     不会出现「双方都先把别的行置 false」导致零 active 的窗口；
//   - B-11 active_version 与 B-12 active 在同一事务内更新（R-M6D-05：镜像
//     只供展示，唯一真源仍是 active 部分唯一索引）；
//   - 唯一 active 由 DB 部分唯一索引兜底：应用层即使漏掉互斥更新，事务也会
//     因索引冲突回滚，而不是留下两个 active。
// ---------------------------------------------------------------------------

// Upload 入库一个已通过编译校验的版本。
//
// 幂等（brief 语义 1）：同 (key, version) 同 source_hash 直接返回已有行，
// created=false；异 hash 返回 ErrTaskPluginVersionConflict。整个写入在一个
// 事务里：新 key 的主表行与版本行要么同时出现，要么都不出现（编译失败本来
// 就在调用本方法之前拒绝，S1 的「零写入」在这里再次由事务保证）。
//
// 不自动激活（R-M6D-07）：新版本 active=false，启用状态为 true；激活必须是
// 管理员的显式操作，避免一次上传改变数据面行为。
func (r *TaskPluginRepo) Upload(ctx context.Context, in TaskPluginUpload) (*TaskPlugin, *TaskPluginVersion, bool, error) {
	if r == nil || r.db == nil {
		return nil, nil, false, errors.New("store: task plugin repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: begin plugin upload: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 主表行只在 key 首次出现时插入。已存在时保留既有元数据：内置插件被
	// 上传覆盖（override）时，source/builtin 仍表示「它属于 factory 层」，
	// 覆盖关系由 B-12 的 active 行表达，不能在主表上抹掉内置事实。
	source, builtin := "upload", false
	if in.Builtin {
		source, builtin = "builtin", true
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_plugins
		("key", name, icon, source, builtin, enabled, description)
		VALUES ($1, $2, $3, $4, $5, true, $6)
		ON CONFLICT ("key") DO UPDATE SET
			builtin = task_plugins.builtin OR EXCLUDED.builtin,
			source = CASE WHEN EXCLUDED.builtin THEN 'builtin' ELSE task_plugins.source END`,
		in.Key, in.Name, in.Icon, source, builtin, in.Description); err != nil {
		return nil, nil, false, fmt.Errorf("store: insert task plugin %q: %w", in.Key, err)
	}

	plugin, err := scanTaskPlugin(tx.QueryRowContext(ctx,
		`SELECT `+taskPluginColumns+` FROM task_plugins WHERE "key" = $1`, in.Key))
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: read task plugin %q after insert: %w", in.Key, err)
	}

	// ON CONFLICT DO NOTHING + RowsAffected：用唯一约束而不是「先查后插」判定
	// 幂等，两个并发上传同键时只有一个插入成功，另一个走回读比对 hash。
	result, err := tx.ExecContext(ctx, `INSERT INTO task_plugin_versions
		("key", version, api_version, source_text, source_hash, icon, enabled, active, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, true, false, $7)
		ON CONFLICT ("key", version) DO NOTHING`,
		in.Key, in.Version, in.APIVersion, in.SourceText, in.SourceHash, in.Icon, in.UploadedBy)
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: insert task plugin version %s/%s: %w", in.Key, in.Version, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: plugin version rows affected: %w", err)
	}

	version, err := scanTaskPluginVersion(tx.QueryRowContext(ctx, `SELECT `+taskPluginVersionColumns+`
		FROM task_plugin_versions WHERE "key" = $1 AND version = $2`, in.Key, in.Version))
	if err != nil {
		return nil, nil, false, fmt.Errorf("store: read task plugin version %s/%s: %w", in.Key, in.Version, err)
	}
	if version.SourceHash != in.SourceHash {
		return nil, nil, false, fmt.Errorf("%w: %s/%s", ErrTaskPluginVersionConflict, in.Key, in.Version)
	}
	if inserted == 1 {
		// 新版本改变插件行内容（版本列表/更新时间），推进 updated_at；它也是
		// 管理面乐观锁版本。插入前读过 plugin，必须重新读一次才能返回新值。
		if _, err := tx.ExecContext(ctx, `UPDATE task_plugins
			SET updated_at = now() WHERE "key" = $1`, in.Key); err != nil {
			return nil, nil, false, fmt.Errorf("store: touch task plugin %q after upload: %w", in.Key, err)
		}
		plugin, err = scanTaskPlugin(tx.QueryRowContext(ctx,
			`SELECT `+taskPluginColumns+` FROM task_plugins WHERE "key" = $1`, in.Key))
		if err != nil {
			return nil, nil, false, fmt.Errorf("store: reload task plugin %q: %w", in.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, false, fmt.Errorf("store: commit plugin upload: %w", err)
	}
	return plugin, version, inserted == 1, nil
}

// scanTaskPluginVersion 把一行版本投影扫进模型。
func scanTaskPluginVersion(row interface{ Scan(...any) error }) (*TaskPluginVersion, error) {
	var v TaskPluginVersion
	if err := row.Scan(&v.ID, &v.Key, &v.Version, &v.APIVersion, &v.SourceText,
		&v.SourceHash, &v.Icon, &v.Enabled, &v.Active, &v.UploadedBy, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// scanTaskPluginVersionMeta 扫描不含 source_text 的版本投影，正文留空。
func scanTaskPluginVersionMeta(row interface{ Scan(...any) error }) (*TaskPluginVersion, error) {
	var v TaskPluginVersion
	if err := row.Scan(&v.ID, &v.Key, &v.Version, &v.APIVersion,
		&v.SourceHash, &v.Icon, &v.Enabled, &v.Active, &v.UploadedBy, &v.CreatedAt); err != nil {
		return nil, err
	}
	return &v, nil
}

// GetVersion 读取单个版本的完整行（含 source_text）。
//
// 只有源查看器与详情 include_source=1 使用：版本列表刻意不含正文，避免
// 一次列表请求把全部插件源码带进响应（S5）。
func (r *TaskPluginRepo) GetVersion(ctx context.Context, key, version string) (*TaskPluginVersion, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	v, err := scanTaskPluginVersion(r.db.QueryRowContext(ctx, `SELECT `+taskPluginVersionColumns+`
		FROM task_plugin_versions WHERE "key" = $1 AND version = $2`, key, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskPluginVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get task plugin version %s/%s: %w", key, version, err)
	}
	return v, nil
}

// UpsertBuiltinPlugins 把内置 factory 清单幂等写入 B-11（装配期调用）。
//
// 只维护「内置事实」相关列：source='builtin'、builtin=true、name/icon/
// description 以 manifest 为准。**不动** enabled（管理员可能已停用）与
// active_version（内置 key 可能已有上传覆盖层的 active 版本）。新行默认
// enabled=true、active_version=NULL。
func (r *TaskPluginRepo) UpsertBuiltinPlugins(ctx context.Context, plugins []BuiltinPlugin) error {
	if r == nil || r.db == nil {
		return errors.New("store: task plugin repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin builtin plugin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, p := range plugins {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_plugins
			("key", name, icon, source, builtin, enabled, description)
			VALUES ($1, $2, $3, 'builtin', true, true, $4)
			ON CONFLICT ("key") DO UPDATE SET
				name = EXCLUDED.name,
				icon = COALESCE(EXCLUDED.icon, task_plugins.icon),
				description = COALESCE(EXCLUDED.description, task_plugins.description),
				source = 'builtin',
				builtin = true`,
			p.Key, p.Name, p.Icon, p.Description); err != nil {
			return fmt.Errorf("store: upsert builtin plugin %q: %w", p.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit builtin plugin upsert: %w", err)
	}
	return nil
}

// lockTaskPlugin 在事务内锁定插件主表行并返回投影。
//
// 同一 key 的所有写路径都先拿这把行锁：激活、启停、删除因此串行执行，
// 「读 updated_at → 比较 → 写」的乐观锁窗口不会被并发请求穿透。
func lockTaskPlugin(ctx context.Context, tx *sql.Tx, key string) (*TaskPlugin, error) {
	plugin, err := scanTaskPlugin(tx.QueryRowContext(ctx,
		`SELECT `+taskPluginColumns+` FROM task_plugins WHERE "key" = $1 FOR UPDATE`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskPluginNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: lock task plugin %q: %w", key, err)
	}
	return plugin, nil
}

// checkExpectUpdatedAt 实现乐观锁：调用方未提供版本时跳过（兼容无并发语义的
// 脚本调用），提供时任何不相等都返回 ErrTaskPluginStale。用 UTC 比较，
// 避免驱动/时区让同一时刻的两个 time.Time 被判为不同。
func checkExpectUpdatedAt(plugin *TaskPlugin, expect *time.Time) error {
	if expect == nil {
		return nil
	}
	if !plugin.UpdatedAt.UTC().Equal(expect.UTC()) {
		return ErrTaskPluginStale
	}
	return nil
}

// Activate 把指定版本置为唯一 active，并同事务同步 B-11 镜像。
//
// 顺序是先清同 key 的其它 active、再置目标 active：两步都在插件行锁内，
// 外部观察者即使读的是 READ COMMITTED 也只会看到提交后的最终状态。
// 目标版本不存在返回 ErrTaskPluginVersionNotFound；内部不存在的 key 不泄露。
func (r *TaskPluginRepo) Activate(ctx context.Context, key, version string, expect *time.Time) (*TaskPlugin, *TaskPluginVersion, error) {
	if r == nil || r.db == nil {
		return nil, nil, errors.New("store: task plugin repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("store: begin plugin activate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	plugin, err := lockTaskPlugin(ctx, tx, key)
	if err != nil {
		return nil, nil, err
	}
	if err := checkExpectUpdatedAt(plugin, expect); err != nil {
		return nil, nil, err
	}

	// 必须分两步：部分唯一索引是**立即**检查的，单条
	// `SET active = (version = $2)` 会按 PG 的更新顺序逐行写索引——
	// 若先把目标行写成 true 而旧 active 行还没写成 false，就会撞上
	// task_plugin_versions_active_key_uq。先清后置让中间状态始终至多一个
	// active（甚至在语句边界之外也成立）。两步都在插件行锁内，外部不可见。
	if _, err := tx.ExecContext(ctx, `UPDATE task_plugin_versions
		SET active = false WHERE "key" = $1 AND active AND version <> $2`, key, version); err != nil {
		return nil, nil, fmt.Errorf("store: deactivate other versions of %s: %w", key, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_plugin_versions
		SET active = true, enabled = true WHERE "key" = $1 AND version = $2`, key, version); err != nil {
		return nil, nil, fmt.Errorf("store: activate task plugin version %s/%s: %w", key, version, err)
	}
	// 目标行不存在时上面的 UPDATE 不触碰任何行；必须显式回读目标行，
	// 不能只看 RowsAffected（其它行的 deactivate 会贡献行数）。
	target, err := scanTaskPluginVersion(tx.QueryRowContext(ctx, `SELECT `+taskPluginVersionColumns+`
		FROM task_plugin_versions WHERE "key" = $1 AND version = $2`, key, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrTaskPluginVersionNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: read activated version %s/%s: %w", key, version, err)
	}
	if !target.Active {
		// 防御：UPDATE 本应已置 true；此处失败说明 SQL 与预期漂移。
		return nil, nil, fmt.Errorf("store: version %s/%s not active after update", key, version)
	}
	if err := updateActiveVersionMirror(ctx, tx, key, &version); err != nil {
		return nil, nil, err
	}
	plugin, err = scanTaskPlugin(tx.QueryRowContext(ctx, `SELECT `+taskPluginColumns+`
		FROM task_plugins WHERE "key" = $1`, key))
	if err != nil {
		return nil, nil, fmt.Errorf("store: reload task plugin %q: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("store: commit plugin activate: %w", err)
	}
	return plugin, target, nil
}

// updateActiveVersionMirror 同事务写 B-11 镜像并刷新 updated_at。
//
// updated_at 必须由写路径显式推进：它同时是管理面乐观锁版本与运行时
// revision 的输入（R-M6D-05；relay-06 §2.5）。用数据库时钟而不是应用时钟，
// 多副本下才有一致的排序依据。
func updateActiveVersionMirror(ctx context.Context, tx *sql.Tx, key string, version *string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE task_plugins
		SET active_version = $2, updated_at = now() WHERE "key" = $1`, key, version); err != nil {
		return fmt.Errorf("store: mirror active version for %q: %w", key, err)
	}
	return nil
}

// SetEnabled 切换插件主表 enabled，并推进 updated_at（乐观锁版本）。
//
// 禁用一个正被渠道绑定的插件时，受影响渠道数由调用方实时计算（本仓储不落库
// 绑定关系）；版本行状态不变——重新启用后原 active 版本继续生效。
func (r *TaskPluginRepo) SetEnabled(ctx context.Context, key string, enabled bool, expect *time.Time) (*TaskPlugin, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin plugin status update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	plugin, err := lockTaskPlugin(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if err := checkExpectUpdatedAt(plugin, expect); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_plugins
		SET enabled = $2, updated_at = now() WHERE "key" = $1`, key, enabled); err != nil {
		return nil, fmt.Errorf("store: set plugin %q enabled=%t: %w", key, enabled, err)
	}
	plugin, err = scanTaskPlugin(tx.QueryRowContext(ctx, `SELECT `+taskPluginColumns+`
		FROM task_plugins WHERE "key" = $1`, key))
	if err != nil {
		return nil, fmt.Errorf("store: reload task plugin %q: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit plugin status update: %w", err)
	}
	return plugin, nil
}

// DeleteVersion 删除一个版本；若删除的是 active，按
// (created_at DESC, id DESC) 提升上一版本（relay-06 §2.4 / S4）。
//
// 两个 409 分支都表达「删除会破坏 S4」：
//   - 唯一版本禁删（删完就没有任何版本可 active）；
//   - active 版本之外没有 enabled 的旧版本可提升。
//
// 提升目标必须 enabled=true：把已禁用的版本提升成 active 会让运行时仍不可用，
// 却让管理面误以为已有生效版本（fail-closed）。
func (r *TaskPluginRepo) DeleteVersion(ctx context.Context, key, version string, expect *time.Time) (*TaskPlugin, string, error) {
	if r == nil || r.db == nil {
		return nil, "", errors.New("store: task plugin repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("store: begin plugin version delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	plugin, err := lockTaskPlugin(ctx, tx, key)
	if err != nil {
		return nil, "", err
	}
	if err := checkExpectUpdatedAt(plugin, expect); err != nil {
		return nil, "", err
	}

	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM task_plugin_versions WHERE "key" = $1`, key).Scan(&count); err != nil {
		return nil, "", fmt.Errorf("store: count plugin versions %q: %w", key, err)
	}
	target, err := scanTaskPluginVersion(tx.QueryRowContext(ctx, `SELECT `+taskPluginVersionColumns+`
		FROM task_plugin_versions WHERE "key" = $1 AND version = $2`, key, version))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrTaskPluginVersionNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: read version %s/%s: %w", key, version, err)
	}
	if count <= 1 {
		return nil, "", ErrTaskPluginVersionRequired
	}

	promoted := ""
	if target.Active {
		err := tx.QueryRowContext(ctx, `SELECT version FROM task_plugin_versions
			WHERE "key" = $1 AND version <> $2 AND enabled
			ORDER BY created_at DESC, id DESC LIMIT 1`, key, version).Scan(&promoted)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrTaskPluginVersionRequired
		}
		if err != nil {
			return nil, "", fmt.Errorf("store: select promotion for %s/%s: %w", key, version, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM task_plugin_versions WHERE "key" = $1 AND id = $2`, key, target.ID); err != nil {
		return nil, "", fmt.Errorf("store: delete version %s/%s: %w", key, version, err)
	}
	if promoted != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE task_plugin_versions
			SET active = true WHERE "key" = $1 AND version = $2`, key, promoted); err != nil {
			return nil, "", fmt.Errorf("store: promote version %s/%s: %w", key, promoted, err)
		}
		if err := updateActiveVersionMirror(ctx, tx, key, &promoted); err != nil {
			return nil, "", err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE task_plugins
		SET updated_at = now() WHERE "key" = $1`, key); err != nil {
		return nil, "", fmt.Errorf("store: touch plugin %q: %w", key, err)
	}

	plugin, err = scanTaskPlugin(tx.QueryRowContext(ctx, `SELECT `+taskPluginColumns+`
		FROM task_plugins WHERE "key" = $1`, key))
	if err != nil {
		return nil, "", fmt.Errorf("store: reload task plugin %q: %w", key, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("store: commit plugin version delete: %w", err)
	}
	return plugin, promoted, nil
}

// ---------------------------------------------------------------------------
// 市场源（B-13）
// ---------------------------------------------------------------------------

const marketSourceColumns = `id, name, url, enabled, last_check_status, last_check_at, last_error`

func scanMarketSource(row interface{ Scan(...any) error }) (*PluginMarketplaceSource, error) {
	var s PluginMarketplaceSource
	if err := row.Scan(&s.ID, &s.Name, &s.URL, &s.Enabled, &s.LastCheckStatus,
		&s.LastCheckAt, &s.LastError); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListMarketSources 返回全部市场源（按名称、URL 稳定排序）。
func (r *TaskPluginRepo) ListMarketSources(ctx context.Context) ([]PluginMarketplaceSource, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+marketSourceColumns+` FROM plugin_marketplace_sources ORDER BY name, url`)
	if err != nil {
		return nil, fmt.Errorf("store: list marketplace sources: %w", err)
	}
	defer rows.Close()
	var out []PluginMarketplaceSource
	for rows.Next() {
		source, err := scanMarketSource(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan marketplace source: %w", err)
		}
		out = append(out, *source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate marketplace sources: %w", err)
	}
	return out, nil
}

// GetMarketSource 按 id 读取市场源。
func (r *TaskPluginRepo) GetMarketSource(ctx context.Context, id string) (*PluginMarketplaceSource, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	source, err := scanMarketSource(r.db.QueryRowContext(ctx,
		`SELECT `+marketSourceColumns+` FROM plugin_marketplace_sources WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMarketSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get marketplace source %q: %w", id, err)
	}
	return source, nil
}

// ReplaceMarketSources 整份替换市场源（PUT 语义）。
//
// 保留策略：输入行为已有行（按 id，其次按 URL 匹配）时只更新 name/url/enabled，
// 不动 last_check_* —— 管理员保存名称/启用状态不应把「最近校验结果」清空。
// 未出现在输入里的行被删除；URL 唯一性由 DB 兜底，重复输入在应用层先拒绝。
//
// 整个替换单事务：部分成功会让「整份替换」的语义失真（请求失败但源已删一半）。
func (r *TaskPluginRepo) ReplaceMarketSources(ctx context.Context, inputs []MarketSourceInput) ([]PluginMarketplaceSource, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin marketplace replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := listMarketSourcesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]PluginMarketplaceSource, len(existing))
	byURL := make(map[string]PluginMarketplaceSource, len(existing))
	for _, source := range existing {
		byID[source.ID] = source
		byURL[source.URL] = source
	}

	seenURL := make(map[string]struct{}, len(inputs))
	type resolved struct {
		input  MarketSourceInput
		target *PluginMarketplaceSource
	}
	plan := make([]resolved, 0, len(inputs))
	kept := make(map[string]struct{}, len(inputs))
	for _, in := range inputs {
		if _, dup := seenURL[in.URL]; dup {
			return nil, fmt.Errorf("%w: %s", ErrMarketSourceURLConflict, in.URL)
		}
		seenURL[in.URL] = struct{}{}

		var target *PluginMarketplaceSource
		if in.ID != "" {
			if existing, ok := byID[in.ID]; ok {
				copied := existing
				target = &copied
			}
		}
		if target == nil {
			if existing, ok := byURL[in.URL]; ok {
				copied := existing
				target = &copied
			}
		}
		if target != nil {
			kept[target.ID] = struct{}{}
		}
		plan = append(plan, resolved{input: in, target: target})
	}

	// 先删除未保留的行，再更新/插入：否则「把 B 的 URL 改成将被删除的 A 的
	// URL」会在中间状态撞上 UNIQUE(url)。整个操作在单事务内，删除失败会
	// 回滚全部改动，不会出现半份替换。
	for _, source := range existing {
		if _, ok := kept[source.ID]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM plugin_marketplace_sources WHERE id = $1`, source.ID); err != nil {
			return nil, fmt.Errorf("store: delete marketplace source %q: %w", source.ID, err)
		}
	}

	for _, item := range plan {
		if item.target != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE plugin_marketplace_sources
				SET name = $2, url = $3, enabled = $4 WHERE id = $1`,
				item.target.ID, item.input.Name, item.input.URL, item.input.Enabled); err != nil {
				return nil, fmt.Errorf("store: update marketplace source %q: %w", item.target.ID, err)
			}
			continue
		}
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO plugin_marketplace_sources (name, url, enabled)
			VALUES ($1, $2, $3) RETURNING id`, item.input.Name, item.input.URL, item.input.Enabled).Scan(&id); err != nil {
			return nil, fmt.Errorf("store: insert marketplace source %q: %w", item.input.URL, err)
		}
	}

	out, err := listMarketSourcesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit marketplace replace: %w", err)
	}
	return out, nil
}

func listMarketSourcesTx(ctx context.Context, tx *sql.Tx) ([]PluginMarketplaceSource, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+marketSourceColumns+` FROM plugin_marketplace_sources ORDER BY name, url`)
	if err != nil {
		return nil, fmt.Errorf("store: list marketplace sources in tx: %w", err)
	}
	defer rows.Close()
	var out []PluginMarketplaceSource
	for rows.Next() {
		source, err := scanMarketSource(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan marketplace source: %w", err)
		}
		out = append(out, *source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate marketplace sources: %w", err)
	}
	return out, nil
}

// UpdateMarketSourceCheck 写回一次试拉结果。
//
// last_error 截断到 512 字符（DB CHECK 同值），并且调用方只传分类后的短原因，
// 不传远端响应体（B-13 明确「不含响应体」）；本方法再兜底一次截断，
// 防止错误文本里混入意外长内容。
func (r *TaskPluginRepo) UpdateMarketSourceCheck(ctx context.Context, id, status, lastError string, at time.Time) (*PluginMarketplaceSource, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("store: task plugin repo not configured")
	}
	source, err := scanMarketSource(r.db.QueryRowContext(ctx, `UPDATE plugin_marketplace_sources
		SET last_check_status = $2, last_check_at = $3, last_error = $4
		WHERE id = $1 RETURNING `+marketSourceColumns,
		id, status, at, nullableMarketError(lastError)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMarketSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: update marketplace source check %q: %w", id, err)
	}
	return source, nil
}

func nullableMarketError(text string) any {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) > 512 {
		runes = runes[:512]
	}
	return string(runes)
}
