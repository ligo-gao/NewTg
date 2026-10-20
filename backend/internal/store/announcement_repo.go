package store

// 站点公告数据层（D7a；modules-08 F-08-04 / R-D7-01～R-D7-10）。
//
// 【为什么收敛在 store】公告有三类**数据层不变量**，不能让每个 handler 各自
// 记得执行：
//  1. 状态迁移必须条件化（draft|offline → published，published → offline），
//     并发下用条件 UPDATE + RowsAffected 判定而不是「先读后写」；
//  2. N-35：编辑已发布公告 **原地更新**，不重写 publish_at/published_at，
//     也不回到 draft；offline 编辑保存回 draft 并清 offline_at；
//  3. 删除公告必须在**同一事务**内删除它的已读行（R-D7-02）——否则已删公告
//     的 read 行会永久滞留，且同名 uuid 复用时会出现「新公告凭空已读」。
//
// 【虚拟过期】status 只有三值；expired 由 SQL 条件派生（R-D7-10）。列表筛选
// 与用户侧 active 查询共用同一组时间谓词，避免两处边界口径漂移。
//
// 【时间口径】所有时间列 timestamptz；调用方传入的 now 统一转 UTC，边界
// 判定（publish_at <= now / expire_at > now）在跨时区进程间必须一致。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 公告状态/分类的持久化字面量。与迁移的 CHECK 白名单逐字一致；不要按下标
// 或类型转换，新增取值必须同时改迁移与 EnumChecks。
const (
	AnnouncementStatusDraft     = "draft"
	AnnouncementStatusPublished = "published"
	AnnouncementStatusOffline   = "offline"

	// AnnouncementDisplayExpired 是**虚拟**展示态，不是 DB 取值（R-D7-10）。
	// 列表筛选用它，落库永远不出现。
	AnnouncementDisplayExpired = "expired"

	AnnouncementCategoryNotice      = "notice"
	AnnouncementCategoryMaintenance = "maintenance"
	AnnouncementCategoryRelease     = "release"
)

// 公告域哨兵错误。handler 据此映射 400/404/409/422。
var (
	// ErrAnnouncementNotFound：公告行不存在（或 id 形态非法，避免 22P02）。
	ErrAnnouncementNotFound = errors.New("store: announcement not found")
	// ErrAnnouncementInvalid：字段不合法（长度/分类/到期时间/超长）→ 422。
	ErrAnnouncementInvalid = errors.New("store: announcement input is invalid")
	// ErrAnnouncementStateConflict：状态迁移非法（如 draft → offline）→ 409。
	ErrAnnouncementStateConflict = errors.New("store: announcement state transition is not allowed")
)

// AnnouncementMaxContentBytes 是应用层内容上限（16KB）。DB CHECK 放宽到
// 64KiB 只做兜底，理由见迁移文件头。
const AnnouncementMaxContentBytes = 16 * 1024

// AnnouncementMaxTitleRunes 是标题上限（modules-08 F-08-04 的数据字段表）。
const AnnouncementMaxTitleRunes = 80

// announcementIDPattern 是 PG uuid 文本形态。先做形态校验再进 SQL：非法
// 文本会触发 22P02，把它统一映射成 404/400 而不是暴露驱动错误。
var announcementIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// AnnouncementInput 是创建/编辑公告的写入值。
//
// PublishAt 零值表示「未指定」：创建时取 now()，编辑时保留原值（N-35 下
// 已发布行还会被强制保留）。ExpireAt 为 nil 表示永久有效。
type AnnouncementInput struct {
	Title      string
	Category   string
	Content    string
	PublishAt  time.Time
	ExpireAt   *time.Time
	PublishNow bool
}

// AnnouncementFilter 是管理侧列表筛选（R-D7-06）。Status 取
// draft/published/offline/expired 或空（全部）；Page 1 基，PageSize 默认 20。
type AnnouncementFilter struct {
	Status   string
	Category string
	Page     int
	PageSize int
}

// AnnouncementMutation 是写操作的结果。
//
// Published 表示**本次调用**把公告迁移到了 published：发布副作用（Redis
// PUBLISH + Webhook）只在首次发布时触发，幂等重放/编辑已发布行不得重复
// 广播（否则用户侧会收到同一公告的重复事件）。
type AnnouncementMutation struct {
	Announcement Announcement
	Published    bool
}

// AnnouncementRepo 是公告域的存储接缝（单测可注入假实现）。
type AnnouncementRepo interface {
	Create(ctx context.Context, in AnnouncementInput, actor int64) (AnnouncementMutation, error)
	Get(ctx context.Context, id string) (Announcement, error)
	Update(ctx context.Context, id string, in AnnouncementInput, actor int64) (AnnouncementMutation, error)
	List(ctx context.Context, f AnnouncementFilter) ([]Announcement, int64, error)
	Publish(ctx context.Context, id string, actor int64) (AnnouncementMutation, error)
	Offline(ctx context.Context, id string, actor int64) (AnnouncementMutation, error)
	Delete(ctx context.Context, id string) error
	// ListActive 返回生效中的公告（status=published 且 publish_at<=now 且
	// 未过期），并按 userID 填充 Read 标记（R-D7-05）。
	ListActive(ctx context.Context, userID int64, now time.Time) ([]Announcement, error)
	// MarkRead 幂等标记已读；非生效公告返回 ErrAnnouncementNotFound。
	MarkRead(ctx context.Context, userID int64, id string, now time.Time) error
}

// SQLAnnouncementRepo 是 AnnouncementRepo 的 PostgreSQL 实现。
type SQLAnnouncementRepo struct {
	db *sql.DB
}

// NewAnnouncementRepo 构造公告仓储。db 必须非 nil：公告没有内存兜底，
// 静默返回空列表会把「数据库故障」伪装成「暂无公告」。
func NewAnnouncementRepo(db *sql.DB) *SQLAnnouncementRepo {
	return &SQLAnnouncementRepo{db: db}
}

const announcementColumns = `id, title, category, content, publish_at, expire_at,
	status, created_by, updated_by, published_at, offline_at, created_at, updated_at`

func scanAnnouncement(row interface{ Scan(...any) error }) (Announcement, error) {
	var a Announcement
	err := row.Scan(&a.ID, &a.Title, &a.Category, &a.Content, &a.PublishAt,
		&a.ExpireAt, &a.Status, &a.CreatedBy, &a.UpdatedBy, &a.PublishedAt,
		&a.OfflineAt, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// scanAnnouncementWithRead 扫描「公告列 + read 标记」（用户侧 active 查询）。
func scanAnnouncementWithRead(row interface{ Scan(...any) error }) (Announcement, error) {
	var a Announcement
	err := row.Scan(&a.ID, &a.Title, &a.Category, &a.Content, &a.PublishAt,
		&a.ExpireAt, &a.Status, &a.CreatedBy, &a.UpdatedBy, &a.PublishedAt,
		&a.OfflineAt, &a.CreatedAt, &a.UpdatedAt, &a.Read)
	return a, err
}

// validAnnouncementID 只做 UUID 形态校验；不存在由调用方映射成 404。
func validAnnouncementID(id string) bool {
	return announcementIDPattern.MatchString(strings.TrimSpace(id))
}

// normalizeAnnouncementInput 在写库前做与 DB CHECK 同口径的校验，让 handler
// 得到可读的 422 而不是 23514 的原始错误。字符长度按 rune 计数，对齐 PG 的
// char_length；内容按字节（应用层 16KB，与编辑器计数一致）。
func normalizeAnnouncementInput(in *AnnouncementInput) error {
	in.Title = strings.TrimSpace(in.Title)
	if n := utf8.RuneCountInString(in.Title); n < 1 || n > AnnouncementMaxTitleRunes {
		return ErrAnnouncementInvalid
	}
	if in.Category == "" {
		in.Category = AnnouncementCategoryNotice
	}
	switch in.Category {
	case AnnouncementCategoryNotice, AnnouncementCategoryMaintenance, AnnouncementCategoryRelease:
	default:
		return ErrAnnouncementInvalid
	}
	if len(in.Content) > AnnouncementMaxContentBytes {
		return ErrAnnouncementInvalid
	}
	// 渲染层也会拒绝非法 UTF-8（MarkdownToHTML 返回错误）；这里提前拒绝，
	// 存储层保证进入 DB 的正文永远是合法 UTF-8（title 的 rune 计数已隐含
	// 这一点，content 是唯一可能携带非法字节的字段）。
	if !utf8.ValidString(in.Content) {
		return ErrAnnouncementInvalid
	}
	if in.ExpireAt != nil {
		expire := in.ExpireAt.UTC()
		in.ExpireAt = &expire
	}
	return nil
}

// Create 插入公告。publish_now=true 时直接以 published 落库（R-D7-06 的
// 「同一事务内写+发布」）：写入与发布在该行上是同一个 INSERT，单语句事务
// 天然原子——校验失败时不会留下草稿行，也不会出现「行已写入但状态仍是
// draft」的中间态。失败整体回滚 422 由 handler 映射。
func (r *SQLAnnouncementRepo) Create(ctx context.Context, in AnnouncementInput, actor int64) (AnnouncementMutation, error) {
	if err := normalizeAnnouncementInput(&in); err != nil {
		return AnnouncementMutation{}, err
	}
	now := time.Now().UTC()
	publishAt := in.PublishAt.UTC()
	if in.PublishAt.IsZero() {
		publishAt = now
	}
	if in.ExpireAt != nil && !in.ExpireAt.After(publishAt) {
		return AnnouncementMutation{}, ErrAnnouncementInvalid
	}
	status := AnnouncementStatusDraft
	var publishedAt *time.Time
	if in.PublishNow {
		status = AnnouncementStatusPublished
		publishedAt = &now
	}
	a, err := scanAnnouncement(r.db.QueryRowContext(ctx, `
		INSERT INTO announcements
			(title, category, content, publish_at, expire_at, status,
			 created_by, updated_by, published_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8)
		RETURNING `+announcementColumns,
		in.Title, in.Category, in.Content, publishAt, in.ExpireAt, status,
		actor, publishedAt))
	if err != nil {
		switch {
		case isCheckViolation(err), isInvalidTextRepresentation(err),
			isStringDataRightTruncation(err):
			return AnnouncementMutation{}, ErrAnnouncementInvalid
		}
		return AnnouncementMutation{}, fmt.Errorf("store: create announcement: %w", err)
	}
	return AnnouncementMutation{Announcement: a, Published: in.PublishNow}, nil
}

// Get 按 id 读取公告。
func (r *SQLAnnouncementRepo) Get(ctx context.Context, id string) (Announcement, error) {
	if !validAnnouncementID(id) {
		return Announcement{}, ErrAnnouncementNotFound
	}
	a, err := scanAnnouncement(r.db.QueryRowContext(ctx,
		`SELECT `+announcementColumns+` FROM announcements WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Announcement{}, ErrAnnouncementNotFound
	}
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return Announcement{}, ErrAnnouncementNotFound
		}
		return Announcement{}, fmt.Errorf("store: get announcement(%s): %w", id, err)
	}
	return a, nil
}

// Update 全量编辑公告，并实现 N-35 的编辑语义：
//
//   - 当前 published：保持 published **原地更新**；publish_at 与
//     published_at 一律保留（即使请求带了新的 publish_at）——已发布公告的
//     「生效时间」是用户已经看到过的历史事实，重写它会让列表排序与审计
//     时间线无法解释；
//   - 当前 offline：保存回 draft 并清 offline_at（offline → draft）；
//   - 当前 draft：保持 draft；
//   - publish_now=true 时，draft/offline 在**同一条 UPDATE** 内迁移到
//     published 并写 published_at（R-D7-06 的原子组合路径）；published 行
//     本来就是目标态，不触发发布副作用。
//
// 事务内先 SELECT ... FOR UPDATE：并发编辑/发布同一行时，后到者看到的是
// 先到者提交后的状态，状态机判定不会基于过期快照。
func (r *SQLAnnouncementRepo) Update(ctx context.Context, id string, in AnnouncementInput, actor int64) (AnnouncementMutation, error) {
	if !validAnnouncementID(id) {
		return AnnouncementMutation{}, ErrAnnouncementNotFound
	}
	if err := normalizeAnnouncementInput(&in); err != nil {
		return AnnouncementMutation{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return AnnouncementMutation{}, fmt.Errorf("store: begin update announcement(%s): %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := scanAnnouncement(tx.QueryRowContext(ctx,
		`SELECT `+announcementColumns+` FROM announcements WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AnnouncementMutation{}, ErrAnnouncementNotFound
	}
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return AnnouncementMutation{}, ErrAnnouncementNotFound
		}
		return AnnouncementMutation{}, fmt.Errorf("store: lock announcement(%s): %w", id, err)
	}

	now := time.Now().UTC()
	var (
		status      string
		publishAt   = current.PublishAt
		publishedAt = current.PublishedAt
		offlineAt   = current.OfflineAt
		published   bool
	)
	switch current.Status {
	case AnnouncementStatusPublished:
		// N-35：原地更新。publish_now 在已发布行上没有额外语义。
		status = AnnouncementStatusPublished
	case AnnouncementStatusDraft, AnnouncementStatusOffline:
		status = AnnouncementStatusDraft
		offlineAt = nil // offline → draft 时清下线时刻
		if !in.PublishAt.IsZero() {
			publishAt = in.PublishAt.UTC()
		}
		if in.PublishNow {
			status = AnnouncementStatusPublished
			publishedAt = &now
			published = true
		}
	default:
		// DB CHECK 保证不可达；防御性失败关闭，避免未知状态被当成草稿写回。
		return AnnouncementMutation{}, fmt.Errorf("store: announcement(%s) has unknown status %q",
			id, current.Status)
	}
	if in.ExpireAt != nil && !in.ExpireAt.After(publishAt) {
		return AnnouncementMutation{}, ErrAnnouncementInvalid
	}

	a, err := scanAnnouncement(tx.QueryRowContext(ctx, `
		UPDATE announcements SET
			title = $2, category = $3, content = $4, publish_at = $5,
			expire_at = $6, status = $7, published_at = $8, offline_at = $9,
			updated_by = $10, updated_at = now()
		WHERE id = $1
		RETURNING `+announcementColumns,
		id, in.Title, in.Category, in.Content, publishAt, in.ExpireAt, status,
		publishedAt, offlineAt, actor))
	if err != nil {
		switch {
		case isCheckViolation(err), isInvalidTextRepresentation(err),
			isStringDataRightTruncation(err):
			return AnnouncementMutation{}, ErrAnnouncementInvalid
		}
		return AnnouncementMutation{}, fmt.Errorf("store: update announcement(%s): %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return AnnouncementMutation{}, fmt.Errorf("store: commit update announcement(%s): %w", id, err)
	}
	return AnnouncementMutation{Announcement: a, Published: published}, nil
}

// List 返回管理侧分页列表与总数。排序冻结为 publish_at DESC, id DESC
// （R-D7-06）。status=expired 是虚拟态筛选；status=published 只返回**未过期**
// 的已发布行——「已发布」与「已过期」在 UI 上是两个互斥徽章，若 published
// 同时包含过期行，同一公告会在两个筛选项里重复出现。
func (r *SQLAnnouncementRepo) List(ctx context.Context, f AnnouncementFilter) ([]Announcement, int64, error) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	now := time.Now().UTC()
	where := make([]string, 0, 2)
	args := make([]any, 0, 4)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, strings.ReplaceAll(clause, "?", fmt.Sprintf("$%d", len(args))))
	}
	switch f.Status {
	case "":
	case AnnouncementStatusDraft, AnnouncementStatusOffline:
		add("status = ?", f.Status)
	case AnnouncementStatusPublished:
		add("status = ?", AnnouncementStatusPublished)
		add("(expire_at IS NULL OR expire_at > ?)", now)
	case AnnouncementDisplayExpired:
		add("status = ?", AnnouncementStatusPublished)
		add("expire_at IS NOT NULL AND expire_at <= ?", now)
	default:
		return nil, 0, ErrAnnouncementInvalid
	}
	if f.Category != "" {
		switch f.Category {
		case AnnouncementCategoryNotice, AnnouncementCategoryMaintenance, AnnouncementCategoryRelease:
			add("category = ?", f.Category)
		default:
			return nil, 0, ErrAnnouncementInvalid
		}
	}
	condition := ""
	if len(where) > 0 {
		condition = " WHERE " + strings.Join(where, " AND ")
	}

	var total int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM announcements`+condition, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count announcements: %w", err)
	}

	offsetArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT `+announcementColumns+`
		FROM announcements`+condition+fmt.Sprintf(
		` ORDER BY publish_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		len(offsetArgs)-1, len(offsetArgs)), offsetArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: list announcements: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]Announcement, 0, pageSize)
	for rows.Next() {
		a, scanErr := scanAnnouncement(rows)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("store: scan announcement: %w", scanErr)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: iterate announcements: %w", err)
	}
	return items, total, nil
}

// Publish 把公告置为 published。允许的来源状态：draft、offline；
// published 是**幂等成功**（R-D7-07：published → published 幂等 200），
// 返回原行且 Published=false，不重写 published_at、不重复触发副作用。
//
// publish_at 取 LEAST(publish_at, now)：排期在未来的草稿在管理员点击
// 「发布」（确认框文案是「发布后用户立即可见」）时应立刻生效，而不是继续
// 等待原定时间。已有历史的行（publish_at <= now）保持原值，不篡改生效
// 时间线。LEAST 只会把时间往前移，因此不会破坏 expire_at > publish_at。
func (r *SQLAnnouncementRepo) Publish(ctx context.Context, id string, actor int64) (AnnouncementMutation, error) {
	if !validAnnouncementID(id) {
		return AnnouncementMutation{}, ErrAnnouncementNotFound
	}
	a, err := scanAnnouncement(r.db.QueryRowContext(ctx, `
		UPDATE announcements SET
			status = $2, published_at = COALESCE(published_at, now()),
			publish_at = LEAST(publish_at, now()), offline_at = NULL,
			updated_by = $3, updated_at = now()
		WHERE id = $1 AND status IN ($4, $5)
		RETURNING `+announcementColumns,
		id, AnnouncementStatusPublished, actor,
		AnnouncementStatusDraft, AnnouncementStatusOffline))
	if err == nil {
		return AnnouncementMutation{Announcement: a, Published: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if isInvalidTextRepresentation(err) {
			return AnnouncementMutation{}, ErrAnnouncementNotFound
		}
		return AnnouncementMutation{}, fmt.Errorf("store: publish announcement(%s): %w", id, err)
	}
	// 0 行：区分「不存在」与「已发布（幂等）」。
	existing, getErr := r.Get(ctx, id)
	if getErr != nil {
		return AnnouncementMutation{}, getErr
	}
	if existing.Status == AnnouncementStatusPublished {
		return AnnouncementMutation{Announcement: existing, Published: false}, nil
	}
	// DB CHECK 只有三值；走到这里说明出现了未知状态，失败关闭。
	return AnnouncementMutation{}, ErrAnnouncementStateConflict
}

// Offline 把已发布公告置为 offline。仅 published → offline 合法（R-D7-07）；
// draft → offline 返回 ErrAnnouncementStateConflict（409）。
// offline → offline 幂等成功（目标态已达成），不重写下线时刻。
func (r *SQLAnnouncementRepo) Offline(ctx context.Context, id string, actor int64) (AnnouncementMutation, error) {
	if !validAnnouncementID(id) {
		return AnnouncementMutation{}, ErrAnnouncementNotFound
	}
	a, err := scanAnnouncement(r.db.QueryRowContext(ctx, `
		UPDATE announcements SET
			status = $2, offline_at = now(), updated_by = $3, updated_at = now()
		WHERE id = $1 AND status = $4
		RETURNING `+announcementColumns,
		id, AnnouncementStatusOffline, actor, AnnouncementStatusPublished))
	if err == nil {
		return AnnouncementMutation{Announcement: a}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if isInvalidTextRepresentation(err) {
			return AnnouncementMutation{}, ErrAnnouncementNotFound
		}
		return AnnouncementMutation{}, fmt.Errorf("store: offline announcement(%s): %w", id, err)
	}
	existing, getErr := r.Get(ctx, id)
	if getErr != nil {
		return AnnouncementMutation{}, getErr
	}
	switch existing.Status {
	case AnnouncementStatusOffline:
		return AnnouncementMutation{Announcement: existing}, nil
	default:
		return AnnouncementMutation{}, ErrAnnouncementStateConflict
	}
}

// Delete 删除公告并在**同一事务**内删除其已读行（R-D7-02）。
//
// 为什么不能只依赖读端过滤：已读行的主键是 (user_id, announcement_id)，
// 公告删除后这些行永远不再被引用；若未来复用 uuid（或恢复备份）会让新公告
// 凭空处于「已读」状态。同事务删除同时保证「公告已不可见」与「已读记录已
// 清理」对外是一个原子事实。
func (r *SQLAnnouncementRepo) Delete(ctx context.Context, id string) error {
	if !validAnnouncementID(id) {
		return ErrAnnouncementNotFound
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete announcement(%s): %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM announcement_reads WHERE announcement_id = $1`, id); err != nil {
		if isInvalidTextRepresentation(err) {
			return ErrAnnouncementNotFound
		}
		return fmt.Errorf("store: delete announcement(%s) reads: %w", id, err)
	}
	// A5：公告删除时同事务清理该公告的站内投递记录。dedupe_key 前缀
	// `announcement:<id>:` 是冻结格式（公告 id 是 uuid，前缀无碰撞）；与
	// announcement_reads 同理，不能让投递行在公告删除后永久滞留。
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
		"announcement:"+id+":%"); err != nil {
		return fmt.Errorf("store: delete announcement(%s) deliveries: %w", id, err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM announcements WHERE id = $1`, id)
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return ErrAnnouncementNotFound
		}
		return fmt.Errorf("store: delete announcement(%s): %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete announcement(%s) rows: %w", id, err)
	}
	if affected == 0 {
		return ErrAnnouncementNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delete announcement(%s): %w", id, err)
	}
	return nil
}

// ListActive 返回生效中的公告，并带上当前用户的 read 标记（R-D7-05）。
//
// 生效判定冻结为 status='published' AND publish_at<=now AND
// (expire_at IS NULL OR expire_at>now)，与 List 的虚拟过期筛选共用同一组
// 谓词（两处漂移会让「列表显示已过期」与「用户仍收到弹层」同时成立）。
func (r *SQLAnnouncementRepo) ListActive(ctx context.Context, userID int64, now time.Time) ([]Announcement, error) {
	if userID <= 0 {
		// 未登录用户不可访问（R-D7-05）；handler 已拦截，repo 层同样失败关闭。
		return nil, ErrAnnouncementNotFound
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+prefixedAnnouncementColumns("a")+`,
		       (r.user_id IS NOT NULL) AS read
		FROM announcements a
		LEFT JOIN announcement_reads r
		  ON r.announcement_id = a.id AND r.user_id = $1
		WHERE a.status = $2
		  AND a.publish_at <= $3
		  AND (a.expire_at IS NULL OR a.expire_at > $3)
		ORDER BY a.publish_at DESC, a.id DESC`, userID, AnnouncementStatusPublished, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("store: list active announcements: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]Announcement, 0, 8)
	for rows.Next() {
		a, scanErr := scanAnnouncementWithRead(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("store: scan active announcement: %w", scanErr)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate active announcements: %w", err)
	}
	return items, nil
}

// prefixedAnnouncementColumns 给列清单加上表别名，避免 JOIN 后与
// announcement_reads 的同名列（无冲突列也存在歧义风险）混淆。
func prefixedAnnouncementColumns(alias string) string {
	parts := strings.Split(announcementColumns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// MarkRead 幂等标记已读（R-D7-05）。
//
// 只允许**生效中**的公告：先确认 active，再 ON CONFLICT DO NOTHING upsert。
// 非 active（草稿/下线/过期/不存在）一律 ErrAnnouncementNotFound——返回
// 409/403 会泄露「该 id 存在但当前不可读」的状态，而 read 端点没有这种
// 区分需求。
func (r *SQLAnnouncementRepo) MarkRead(ctx context.Context, userID int64, id string, now time.Time) error {
	if userID <= 0 || !validAnnouncementID(id) {
		return ErrAnnouncementNotFound
	}
	now = now.UTC()
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM announcements
			WHERE id = $1 AND status = $2 AND publish_at <= $3
			  AND (expire_at IS NULL OR expire_at > $3)
		)`, id, AnnouncementStatusPublished, now).Scan(&exists)
	if err != nil {
		if isInvalidTextRepresentation(err) {
			return ErrAnnouncementNotFound
		}
		return fmt.Errorf("store: check announcement(%s) for read: %w", id, err)
	}
	if !exists {
		return ErrAnnouncementNotFound
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO announcement_reads (user_id, announcement_id, read_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, announcement_id) DO NOTHING`, userID, id, now); err != nil {
		if isInvalidTextRepresentation(err) {
			return ErrAnnouncementNotFound
		}
		return fmt.Errorf("store: mark announcement(%s) read: %w", id, err)
	}
	return nil
}
