package notify

// Store 是 notification_templates / notification_deliveries 的 SQL 访问层。
// 设计权威：§3.9.2 与 §3.9.5 的列定义；DDL 在
// backend/migrations/20261019113846_notification_templates.up.sql。
//
// 这里刻意用 database/sql + 显式 SQL，而不是 GORM：两张表的写入面很窄
// （播种 upsert、乐观锁 update、投递 insert/list），显式 SQL 让约束与冲突
// 语义（ON CONFLICT DO NOTHING / DO UPDATE）一眼可见。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrConflict 表示 expect_updated_at 与行的当前版本不一致（HTTP 409）。
	ErrConflict = errors.New("notify: template version conflict")
	// ErrExpectationRequired 表示更新已存在的行却没有带 expect_updated_at
	// （HTTP 400）。乐观锁是 PUT 的必要条件，缺版本号等于「无条件覆盖」，
	// 会让并发编辑静默丢改动。
	ErrExpectationRequired = errors.New("notify: expect_updated_at is required for an existing template")
	// ErrUnknownTemplate 表示 key×channel 未注册（HTTP 404）。
	ErrUnknownTemplate = errors.New("notify: unknown template")
	// ErrTemplateMissing 表示语言回退链（语言 → en）全部缺行（P1 fail-closed）。
	ErrTemplateMissing = errors.New("notify: template row missing for all locales")
	// ErrDeliveryNotFound：站内通知不存在或不属于当前用户（HTTP 404）。
	ErrDeliveryNotFound = errors.New("notify: notification not found")
)

// TemplateRow 是 notification_templates 的一行（与 §3.9.2 列一一对应）。
type TemplateRow struct {
	Key        string
	Category   string
	Channel    Channel
	Locale     Locale
	Subject    string
	Body       string
	CTAText    string
	CTAURL     string
	IsModified bool
	Enabled    bool
	UpdatedBy  *int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Delivery 是 notification_deliveries 的一行（§3.9.5b）。
type Delivery struct {
	ID                int64
	TemplateKey       *string
	Category          string
	Channel           Channel
	Recipient         string
	RecipientUserID   *int64
	Locale            *Locale
	DedupeKey         string
	Status            string
	Attempt           int
	NextRetryAt       *time.Time
	LastError         *string
	ProviderMessageID *string
	// Payload 是投递载荷：站内通知的渲染输入/结果（A5，迁移
	// 20261020200000）与 **webhook 投递的请求体**（N8 worker 原样发送并
	// 对这份字节签名）。email 行没有载荷，为 NULL。注意 PostgreSQL jsonb
	// 会重排/格式化，worker 签名的是读回来的这一份字节。
	Payload []byte
	// AnnouncementID 是读取面从 payload->>'announcement_id' 提取的公告
	// 归因键（D7b）。普通站内通知与非对象 payload 一律为空串：这里只做
	// 文本提取，不解析整个 payload，因此 payload 损坏也不影响本字段与
	// 其它列（jsonb 保证存进去的必是合法 JSON，但可能是标量/数组）。
	AnnouncementID string
	// ReadAt 是站内通知的已读时刻；email/webhook 恒为 NULL。
	ReadAt    *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TemplateFilter 是模板列表筛选（category/channel/locale/仅已修改）。
type TemplateFilter struct {
	Category     string
	Channel      Channel
	Locale       Locale
	ModifiedOnly bool
}

// UpdateInput 是一次模板写入。乐观锁口径（F6 裁决）：
//   - 行已存在 → ExpectUpdatedAt 必填，缺失返回 ErrExpectationRequired（400），
//     与当前值不一致返回 ErrConflict（409）；
//   - 行不存在（首次插入/补种）→ 允许省略 ExpectUpdatedAt；带了却对不上行
//     视为 ErrConflict。
type UpdateInput struct {
	Key     string
	Channel Channel
	Locale  Locale
	Subject string
	Body    string
	CTAText string
	CTAURL  string
	// Enabled 为 nil 表示「保持当前值」：PUT 省略 enabled 不得把已停用的
	// 模板悄悄重新启用（F5 评审裁决）。
	Enabled         *bool
	IsModified      bool
	ExpectUpdatedAt *time.Time
	Actor           int64
}

// DeliveryFilter 是投递记录筛选。Status 使用**API 口径**（见 apiDeliveryStatus），
// 由 Service 转成 DB 口径后传入。
type DeliveryFilter struct {
	Category string
	Channel  Channel
	Status   string
	Page     int
	PageSize int
}

// Store 持有主库连接，可并发使用。
type Store struct {
	db *sql.DB
}

// NewStore 构造存储。db 必填：两张表都在主库（§3.9.2「独立表」）。
func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("notify: store requires a database")
	}
	return &Store{db: db}, nil
}

// Seed 幂等补种内置行（§3.9.2「启动时若发现内置键缺失则补种」）。
//
// ON CONFLICT (key,channel,locale) DO NOTHING 是关键：管理员改过的行绝不能
// 被启动补种覆盖；只有缺失行被插入。内容来自代码 bundle，不来自 SQL。
func (s *Store) Seed(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("notify: begin seed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO notification_templates
		("key", channel, locale, category, subject, body_markdown, cta_text, cta_url, is_modified, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, false, true)
		ON CONFLICT ("key", channel, locale) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("notify: prepare seed: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, spec := range templateSpecs {
		for _, locale := range localeOrder {
			content, ok := spec.DefaultContent(locale)
			if !ok {
				// 注册表自身不完整属于编码错误，启动期暴露比运行期 500 好。
				return fmt.Errorf("notify: no built-in copy for %s/%s/%s", spec.Key, spec.Channel, locale)
			}
			var subject any
			if spec.Channel == ChannelEmail {
				subject = content.Subject
			}
			if _, err := stmt.ExecContext(ctx,
				spec.Key, string(spec.Channel), string(locale), spec.Category,
				subject, content.Body, nullableString(content.CTAText), nullableString(content.CTAURL),
			); err != nil {
				return fmt.Errorf("notify: seed %s/%s/%s: %w", spec.Key, spec.Channel, locale, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("notify: commit seed: %w", err)
	}
	return nil
}

// List 读取命中筛选的行。缺失的内置组合由 Service 用 bundle 补齐，因此这里
// 只返回真实存在的行。
func (s *Store) List(ctx context.Context, filter TemplateFilter) ([]TemplateRow, error) {
	var (
		where []string
		args  []any
	)
	if filter.Category != "" {
		args = append(args, filter.Category)
		where = append(where, fmt.Sprintf("category = $%d", len(args)))
	}
	if filter.Channel != "" {
		args = append(args, string(filter.Channel))
		where = append(where, fmt.Sprintf("channel = $%d", len(args)))
	}
	if filter.Locale != "" {
		args = append(args, string(filter.Locale))
		where = append(where, fmt.Sprintf("locale = $%d", len(args)))
	}
	if filter.ModifiedOnly {
		where = append(where, "is_modified = true")
	}
	query := `SELECT "key", category, channel, locale, subject, body_markdown,
		cta_text, cta_url, is_modified, enabled, updated_by, created_at, updated_at
		FROM notification_templates`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY "key", channel, locale`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("notify: list templates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TemplateRow
	for rows.Next() {
		row, err := scanTemplateRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: list templates: %w", err)
	}
	return out, nil
}

// Get 读取精确 (key,channel,locale) 行；不存在返回 sql.ErrNoRows。
func (s *Store) Get(ctx context.Context, key string, channel Channel, locale Locale) (TemplateRow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT "key", category, channel, locale, subject, body_markdown,
		cta_text, cta_url, is_modified, enabled, updated_by, created_at, updated_at
		FROM notification_templates WHERE "key" = $1 AND channel = $2 AND locale = $3`,
		key, string(channel), string(locale))
	return scanTemplateRow(row)
}

// Update 写入一行并实施乐观锁。
//
// 行不存在时插入（内置行可能尚未补种），此时 ExpectUpdatedAt 可省略；
// 行已存在时 ExpectUpdatedAt **必填**（F6：缺版本号等于无条件覆盖），
// 与当前 updated_at 不一致返回 ErrConflict。
func (s *Store) Update(ctx context.Context, in UpdateInput) (TemplateRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TemplateRow{}, fmt.Errorf("notify: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		current        time.Time
		currentEnabled = true
	)
	err = tx.QueryRowContext(ctx, `SELECT updated_at, enabled FROM notification_templates
		WHERE "key" = $1 AND channel = $2 AND locale = $3 FOR UPDATE`,
		in.Key, string(in.Channel), string(in.Locale)).Scan(&current, &currentEnabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if in.ExpectUpdatedAt != nil {
			// 调用方认为行存在但实际不存在：这是版本冲突而不是「静默新建」。
			return TemplateRow{}, ErrConflict
		}
		currentEnabled = true
	case err != nil:
		return TemplateRow{}, fmt.Errorf("notify: lock template: %w", err)
	default:
		// F6 裁决：已存在的行必须带版本号。缺版本 = 无条件覆盖，会让并发
		// 编辑静默丢改动；首次插入（行不存在）不受此限。
		if in.ExpectUpdatedAt == nil {
			return TemplateRow{}, ErrExpectationRequired
		}
		if !current.Equal(*in.ExpectUpdatedAt) {
			return TemplateRow{}, ErrConflict
		}
	}
	// enabled 省略时保持原值（行不存在时默认 true）。
	enabled := currentEnabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	var subject any
	if in.Channel == ChannelEmail {
		subject = in.Subject
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO notification_templates
		("key", channel, locale, category, subject, body_markdown, cta_text, cta_url,
		 is_modified, enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now())
		ON CONFLICT ("key", channel, locale) DO UPDATE SET
			subject = EXCLUDED.subject,
			body_markdown = EXCLUDED.body_markdown,
			cta_text = EXCLUDED.cta_text,
			cta_url = EXCLUDED.cta_url,
			is_modified = EXCLUDED.is_modified,
			enabled = EXCLUDED.enabled,
			updated_by = EXCLUDED.updated_by,
			updated_at = now()`,
		in.Key, string(in.Channel), string(in.Locale), categoryForKey(in.Key),
		subject, in.Body, nullableString(in.CTAText), nullableString(in.CTAURL),
		in.IsModified, enabled, nullableInt64(in.Actor))

	row := tx.QueryRowContext(ctx, `SELECT "key", category, channel, locale, subject, body_markdown,
		cta_text, cta_url, is_modified, enabled, updated_by, created_at, updated_at
		FROM notification_templates WHERE "key" = $1 AND channel = $2 AND locale = $3`,
		in.Key, string(in.Channel), string(in.Locale))
	updated, err := scanTemplateRow(row)
	if err != nil {
		return TemplateRow{}, fmt.Errorf("notify: read updated template: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TemplateRow{}, fmt.Errorf("notify: commit update: %w", err)
	}
	return updated, nil
}

// InsertDelivery 写入一条投递记录。dedupe_key 冲突时**不重复投递**（§3.9.6b：
// 至少一次 + 幂等键 = 有效一次）。返回 inserted=false 表示命中去重。
func (s *Store) InsertDelivery(ctx context.Context, d Delivery) (Delivery, bool, error) {
	var templateKey any
	if d.TemplateKey != nil {
		templateKey = *d.TemplateKey
	}
	var locale any
	if d.Locale != nil {
		locale = string(*d.Locale)
	}
	var lastError any
	if d.LastError != nil {
		lastError = *d.LastError
	}
	var providerID any
	if d.ProviderMessageID != nil {
		providerID = *d.ProviderMessageID
	}
	row := s.db.QueryRowContext(ctx, `INSERT INTO notification_deliveries
		(template_key, category, channel, recipient, recipient_user_id, locale,
		 dedupe_key, status, attempt, last_error, provider_message_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (dedupe_key) DO NOTHING
		RETURNING id, created_at, updated_at`,
		templateKey, d.Category, string(d.Channel), d.Recipient, nullableInt64Ptr(d.RecipientUserID),
		locale, d.DedupeKey, d.Status, d.Attempt, lastError, providerID, nullableJSON(d.Payload))
	var id int64
	var createdAt, updatedAt time.Time
	err := row.Scan(&id, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, fmt.Errorf("notify: insert delivery: %w", err)
	}
	d.ID = id
	d.CreatedAt = createdAt
	d.UpdatedAt = updatedAt
	return d, true, nil
}

// ListDeliveries 分页读取投递记录，按时间倒序（UI 最新在上）。
func (s *Store) ListDeliveries(ctx context.Context, filter DeliveryFilter) ([]Delivery, int, error) {
	var (
		where []string
		args  []any
	)
	if filter.Category != "" {
		args = append(args, filter.Category)
		where = append(where, fmt.Sprintf("category = $%d", len(args)))
	}
	if filter.Channel != "" {
		args = append(args, string(filter.Channel))
		where = append(where, fmt.Sprintf("channel = $%d", len(args)))
	}
	if filter.Status != "" {
		args = append(args, filter.Status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries`+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("notify: count deliveries: %w", err)
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	pageSize := filter.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	args = append(args, pageSize, (page-1)*pageSize)
	query := fmt.Sprintf(`SELECT id, template_key, category, channel, recipient, recipient_user_id,
		locale, dedupe_key, status, attempt, next_retry_at, last_error, provider_message_id,
		payload,`+inAppAnnouncementIDExpr+`, read_at, created_at, updated_at
		FROM notification_deliveries%s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d OFFSET $%d`, clause, len(args)-1, len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("notify: list deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		item, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("notify: list deliveries: %w", err)
	}
	return out, total, nil
}

// InAppAudience 是站内通知的一个收件人：用户 id + 该用户的语言。
type InAppAudience struct {
	UserID int64
	Locale Locale
}

// ListInAppAudience 按 users.id 升序返回一页启用用户（status='enabled'）。
//
// keyset 分页：afterID 传上一页最后一个 id（首页传 0），limit 由调用方
// 固定为写入批大小（≤500）。旧实现一次 SELECT 全表，用户量大时内存与查询
// 时长不可控；keyset 也避免 OFFSET 在并发写入下的跳页/重复。
// 只取 id/language 两列，避免把 users 行的敏感列带进内存。
func (s *Store) ListInAppAudience(ctx context.Context, afterID int64, limit int) ([]InAppAudience, error) {
	if afterID < 0 {
		afterID = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, language FROM users
		WHERE status = 'enabled' AND id > $1
		ORDER BY id
		LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("notify: list inapp audience: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []InAppAudience
	for rows.Next() {
		var (
			userID   int64
			language string
		)
		if err := rows.Scan(&userID, &language); err != nil {
			return nil, fmt.Errorf("notify: scan inapp audience: %w", err)
		}
		locale := Locale(language)
		if _, ok := ParseLocale(language); !ok {
			// users.language 有 CHECK 约束；若历史数据越过约束，按站点默认
			// en 处理而不是让整次广播失败（读取端是 fail-soft 的展示面）。
			locale = DefaultLocale
		}
		out = append(out, InAppAudience{UserID: userID, Locale: locale})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: iterate inapp audience: %w", err)
	}
	return out, nil
}

// CountInAppAudience 返回启用用户总数。只在模板缺失这类异常路径上使用，
// 用于保留审计的「失败数 = 启用用户数」口径，不参与正常广播循环。
func (s *Store) CountInAppAudience(ctx context.Context) (int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE status = 'enabled'`).Scan(&total); err != nil {
		return 0, fmt.Errorf("notify: count inapp audience: %w", err)
	}
	return total, nil
}

// BalanceLowUser 是低余额扫描命中的一个用户：只取投递所需的两列，
// 不把 users 行的邮箱/手机号等 PII 列带进内存（B4 脱敏要求）。
type BalanceLowUser struct {
	ID      int64
	Balance int64
}

// ListLowBalanceUsers 返回 balance < threshold 且**当天尚未通知**的启用用户
// （按 id 升序）。
//
// 扫描口径是冻结的（tasks B4 / N6）：只认 status='enabled'，禁用用户即使
// 余额为 0 也不产生通知；threshold <= 0 时由调用方直接跳过，不执行查询。
// siteDay 是站点时区的业务日（YYYY-MM-DD），对应 dedupe_key 的日期后缀
// `balance_low:<userID>:<siteDay>`。
//
// 已通知用户在 SQL 里用 NOT EXISTS 排除，而不是留给 INSERT 命中
// UNIQUE(dedupe_key) 去丢弃：持续低余额的用户每天都会出现在命中集里，
// 若不过滤，每轮扫描都要为同一批人做一次注定失败的插入（每 5 分钟一轮、
// 用户量大时是纯噪声）。NOT EXISTS 走 dedupe_key 唯一索引，代价可控。
// 这**不替代** UNIQUE 约束：多副本在同一轮同时查询时仍可能都通过
// NOT EXISTS，最终由唯一约束收敛，Deduped 计数记录这种竞态。
//
// 用户量大时分页由调用方按 id keyset 推进——这里保留纯读接口，扫描器
// 每 5 分钟一次全量命中集，避免把「明天仍低于阈值」的持久状态写进表。
func (s *Store) ListLowBalanceUsers(ctx context.Context, threshold int64, siteDay string) ([]BalanceLowUser, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.balance FROM users u
		WHERE u.status = 'enabled' AND u.balance < $1
		  AND NOT EXISTS (
		      SELECT 1 FROM notification_deliveries d
		      WHERE d.dedupe_key = 'balance_low:' || u.id::text || ':' || $2
		  )
		ORDER BY u.id`, threshold, siteDay)
	if err != nil {
		return nil, fmt.Errorf("notify: list low balance users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []BalanceLowUser
	for rows.Next() {
		var user BalanceLowUser
		if err := rows.Scan(&user.ID, &user.Balance); err != nil {
			return nil, fmt.Errorf("notify: scan low balance user: %w", err)
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: iterate low balance users: %w", err)
	}
	return out, nil
}

// InsertInAppDeliveries 批量写入站内投递记录（A5）。
//
// 单条 SQL 多值插入，每批由调用方限制 ≤500 行（10 列 × 500 = 5000 个参数，
// 远低于 PostgreSQL 65535 上限）。ON CONFLICT (dedupe_key) DO NOTHING 让
// 重复发布/重试天然幂等；返回**实际插入**行数，用于审计的 delivered_users。
// 整批失败时返回错误且该批一条都不落库——调用方的「失败不阻塞发布」语义
// 由它自己决定（Warn + 计数）。
func (s *Store) InsertInAppDeliveries(ctx context.Context, items []Delivery) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	if len(items) > 500 {
		return 0, errors.New("notify: inapp delivery batch must not exceed 500 rows")
	}
	var (
		placeholders []string
		args         []any
	)
	for _, d := range items {
		base := len(args)
		var templateKey any
		if d.TemplateKey != nil {
			templateKey = *d.TemplateKey
		}
		var locale any
		if d.Locale != nil {
			locale = string(*d.Locale)
		}
		args = append(args,
			templateKey, d.Category, string(d.Channel), d.Recipient,
			nullableInt64Ptr(d.RecipientUserID), locale, d.DedupeKey,
			d.Status, d.Attempt, nullableJSON(d.Payload),
		)
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10))
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO notification_deliveries
		(template_key, category, channel, recipient, recipient_user_id, locale,
		 dedupe_key, status, attempt, payload)
		VALUES `+strings.Join(placeholders, ", ")+`
		ON CONFLICT (dedupe_key) DO NOTHING`, args...)
	if err != nil {
		return 0, fmt.Errorf("notify: batch insert inapp deliveries: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("notify: batch insert inapp deliveries rows: %w", err)
	}
	return int(affected), nil
}

// inAppAnnouncementIDExpr 是读取面唯一的 announcement_id 提取口径：
// 只认「对象里字符串类型的 announcement_id」。缺失、JSON null、数字/对象
// 等非法值统一折叠成 SQL NULL，扫描后归一为空串——既不会让整页 500，
// 也不会把非法文本当成归因键下发给前端。
const inAppAnnouncementIDExpr = `
	CASE WHEN jsonb_typeof(payload->'announcement_id') = 'string'
		THEN payload->>'announcement_id' END`

// inAppVisibleAnnouncementClause 让站内信读取面随公告生命周期失效
// （G-DOM-37 ⑧）：公告投递（payload 带 announcement_id）只有在公告仍
// 生效时才返回——status='published' AND publish_at<=now AND
// (expire_at IS NULL OR expire_at>now)。下线（offline）与虚拟过期
// （expired）都由这一组条件派生；公告行被物理删除时子查询自然不含该 id。
//
// 普通站内通知没有 announcement_id，条件短路为真，不受影响。
// 实现选择「文本集合半连接」而不是 a.id = ($x)::uuid：payload 是 JSONB，
// 非法 uuid 文本在读取面会变成 500；这里比较 a.id::text 与 payload 原文，
// 只扫一遍 announcements 的生效集（status/publish 索引可用），每页 ≤100 行
// 的读取开销可忽略。EXISTS + PK 的点查同样可行，但需要 uuid 强转，故不取。
const inAppVisibleAnnouncementClause = `
	AND (
		` + inAppAnnouncementIDExpr + ` IS NULL
		OR ` + inAppAnnouncementIDExpr + ` IN (
			SELECT a.id::text FROM announcements a
			WHERE a.status = 'published'
			  AND a.publish_at <= now()
			  AND (a.expire_at IS NULL OR a.expire_at > now())
		)
	)`

// ListUserInAppDeliveries 分页返回某用户本人的站内通知（A5 读取端点）。
// 数据范围只由 recipient_user_id 决定，绝不接受调用方传入他人 id；
// 公告已下线/已过期/已删除的投递不会出现在列表与 total 中。
func (s *Store) ListUserInAppDeliveries(ctx context.Context, userID int64, page, pageSize int) ([]Delivery, int, error) {
	if userID <= 0 {
		return nil, 0, errors.New("notify: user id is required")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries
		WHERE channel = 'inapp' AND recipient_user_id = $1`+inAppVisibleAnnouncementClause,
		userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("notify: count user inapp deliveries: %w", err)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, template_key, category, channel, recipient,
		recipient_user_id, locale, dedupe_key, status, attempt, next_retry_at, last_error,
		provider_message_id, payload,`+inAppAnnouncementIDExpr+`, read_at, created_at, updated_at
		FROM notification_deliveries
		WHERE channel = 'inapp' AND recipient_user_id = $1`+inAppVisibleAnnouncementClause+`
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`, userID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("notify: list user inapp deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Delivery
	for rows.Next() {
		item, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("notify: iterate user inapp deliveries: %w", err)
	}
	return out, total, nil
}

// CountUserUnreadInAppDeliveries 返回某用户本人「可见且未读」的站内通知数。
//
// 统计口径必须与 ListUserInAppDeliveries 逐字同源：同样的 channel/归属条件，
// 同样的 inAppVisibleAnnouncementClause，再加上 read_at IS NULL。任何一侧单独
// 改动都会让徽章与列表互相矛盾（G-DOM-46），因此两者共用同一常量而不是各自
// 复制一份条件。
func (s *Store) CountUserUnreadInAppDeliveries(ctx context.Context, userID int64) (int, error) {
	if userID <= 0 {
		return 0, errors.New("notify: user id is required")
	}
	var unread int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries
		WHERE channel = 'inapp' AND recipient_user_id = $1
		  AND read_at IS NULL`+inAppVisibleAnnouncementClause,
		userID).Scan(&unread); err != nil {
		return 0, fmt.Errorf("notify: count user unread inapp deliveries: %w", err)
	}
	return unread, nil
}

// MarkAllUserInAppDeliveriesRead 把本人所有「可见且未读」的站内通知一次置为
// 已读，返回本次实际更新的行数。
//
// 幂等语义（G-DOM-47）：
//   - `read_at IS NULL` 谓词保证已读行的 `read_at` 永不被覆盖——第二次调用
//     更新 0 行，首次标记的时刻即最终值；
//   - 只按 recipient_user_id 限定范围，绝不接受调用方传入他人 id；
//   - 复用列表的 inAppVisibleAnnouncementClause：公告已下线/过期/删除的投递
//     不参与更新（它们也不会出现在列表里，更新它们只会制造不可见的「已读」）。
//
// 这是一条集合 UPDATE，天然原子：要么本批可见未读全部置读，要么整条语句失败，
// 不会出现「部分已读」的中间态。
func (s *Store) MarkAllUserInAppDeliveriesRead(ctx context.Context, userID int64, now time.Time) (int, error) {
	if userID <= 0 {
		return 0, errors.New("notify: user id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries SET
		read_at = $2, updated_at = now()
		WHERE channel = 'inapp' AND recipient_user_id = $1
		  AND read_at IS NULL`+inAppVisibleAnnouncementClause,
		userID, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("notify: mark all user inapp deliveries read: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("notify: mark all user inapp deliveries read rows: %w", err)
	}
	return int(affected), nil
}

// MarkInAppDeliveryRead 幂等标记本人一条站内通知已读。
// 他人通知与不存在的 id 同为 ErrDeliveryNotFound（不区分，避免枚举）。
func (s *Store) MarkInAppDeliveryRead(ctx context.Context, userID, id int64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries SET
		read_at = COALESCE(read_at, $3), updated_at = now()
		WHERE id = $1 AND recipient_user_id = $2 AND channel = 'inapp'`,
		id, userID, now.UTC())
	if err != nil {
		return fmt.Errorf("notify: mark inapp delivery read: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("notify: mark inapp delivery read rows: %w", err)
	}
	if affected == 0 {
		return ErrDeliveryNotFound
	}
	return nil
}

// webhookDeliveryColumns 是 N8 worker 读取投递行的列序，与 scanDelivery 一致。
const webhookDeliveryColumns = `id, template_key, category, channel, recipient, recipient_user_id,
	locale, dedupe_key, status, attempt, next_retry_at, last_error, provider_message_id,
	payload,` + inAppAnnouncementIDExpr + `, read_at, created_at, updated_at`

// WebhookDeliveryClaim 是一条被本 worker 占用的 Webhook 投递。
//
// 占用方式是「事务 + SELECT ... FOR UPDATE SKIP LOCKED」：行锁一直持有到
// MarkSent/MarkFailed/MarkSuppressed 提交或 Release 回滚。代价是发送期间
// 占着一个连接（单次投递上限 10s），收益是**同一行绝不会被第二个副本
// 同时发送**——SKIP LOCKED 让并发扫描直接跳过已锁行，即使部署了多个
// worker 也不会重复投递。进程崩溃时事务自动回滚，行回到 queued，
// 下一轮重新领取（至少一次；幂等锚是投递行的 dedupe_key）。
type WebhookDeliveryClaim struct {
	tx       *sql.Tx
	Delivery Delivery
	closed   bool
}

// ClaimWebhookDelivery 领取一条到期的 Webhook 投递。
//
// 返回 (nil,false,nil) 表示当前没有可领取的行；调用方据此结束本轮。
// 领取口径即 N8 的冻结扫描条件：
// channel='webhook' AND status='queued' AND (next_retry_at IS NULL OR <= now)。
func (s *Store) ClaimWebhookDelivery(ctx context.Context, now time.Time) (*WebhookDeliveryClaim, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("notify: begin webhook claim: %w", err)
	}
	row := tx.QueryRowContext(ctx, `SELECT `+webhookDeliveryColumns+`
		FROM notification_deliveries
		WHERE channel = 'webhook' AND status = 'queued'
		  AND (next_retry_at IS NULL OR next_retry_at <= $1)
		ORDER BY next_retry_at NULLS FIRST, id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`, now.UTC())
	delivery, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, false, nil
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("notify: claim webhook delivery: %w", err)
	}
	return &WebhookDeliveryClaim{tx: tx, Delivery: delivery}, true, nil
}

// MarkSent 把投递置为 sent 并提交占用。
func (c *WebhookDeliveryClaim) MarkSent(ctx context.Context, attempt int) error {
	return c.finish(ctx, `UPDATE notification_deliveries
		SET status = 'sent', attempt = $2, next_retry_at = NULL, last_error = NULL, updated_at = now()
		WHERE id = $1`, c.Delivery.ID, attempt)
}

// MarkFailed 记录一次失败。nextRetryAt 非 nil 时保持 queued 并设置重试时刻；
// nil 表示已达最大尝试次数，置为终态 failed。
func (c *WebhookDeliveryClaim) MarkFailed(ctx context.Context, attempt int, nextRetryAt *time.Time, reason string) error {
	status := "failed"
	var next any
	if nextRetryAt != nil {
		status = "queued"
		next = nextRetryAt.UTC()
	}
	return c.finish(ctx, `UPDATE notification_deliveries
		SET status = $2, attempt = $3, next_retry_at = $4, last_error = $5, updated_at = now()
		WHERE id = $1`, c.Delivery.ID, status, attempt, next, reason)
}

// MarkSuppressed 把投递置为 suppressed（例如事件已不在订阅列表内）。
// 这不是端点故障：不消耗重试次数、不计入连续失败。
func (c *WebhookDeliveryClaim) MarkSuppressed(ctx context.Context, reason string) error {
	return c.finish(ctx, `UPDATE notification_deliveries
		SET status = 'suppressed', next_retry_at = NULL, last_error = $2, updated_at = now()
		WHERE id = $1`, c.Delivery.ID, reason)
}

// Release 放弃占用但不改行：投递保持 queued，下一轮重新领取。
// 用于「配置暂时不可用/已暂停」这类不应消耗重试次数的情形。
func (c *WebhookDeliveryClaim) Release() {
	if c == nil || c.closed {
		return
	}
	c.closed = true
	_ = c.tx.Rollback()
}

func (c *WebhookDeliveryClaim) finish(ctx context.Context, query string, args ...any) error {
	if c == nil || c.closed {
		return errors.New("notify: webhook claim is already closed")
	}
	c.closed = true
	if _, err := c.tx.ExecContext(ctx, query, args...); err != nil {
		_ = c.tx.Rollback()
		return fmt.Errorf("notify: update webhook delivery: %w", err)
	}
	if err := c.tx.Commit(); err != nil {
		return fmt.Errorf("notify: commit webhook delivery: %w", err)
	}
	return nil
}

// rowScanner 抽象 *sql.Row / *sql.Rows，便于同一扫描逻辑复用。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTemplateRow(scanner rowScanner) (TemplateRow, error) {
	var (
		row              TemplateRow
		channel, locale  string
		subject, ctaText sql.NullString
		ctaURL           sql.NullString
		updatedBy        sql.NullInt64
	)
	if err := scanner.Scan(&row.Key, &row.Category, &channel, &locale, &subject,
		&row.Body, &ctaText, &ctaURL, &row.IsModified, &row.Enabled, &updatedBy,
		&row.CreatedAt, &row.UpdatedAt); err != nil {
		return TemplateRow{}, err
	}
	row.Channel = Channel(channel)
	row.Locale = Locale(locale)
	row.Subject = subject.String
	row.CTAText = ctaText.String
	row.CTAURL = ctaURL.String
	if updatedBy.Valid {
		value := updatedBy.Int64
		row.UpdatedBy = &value
	}
	return row, nil
}

func scanDelivery(scanner rowScanner) (Delivery, error) {
	var (
		item                  Delivery
		channel               string
		templateKey, locale   sql.NullString
		recipientUserID       sql.NullInt64
		nextRetryAt           sql.NullTime
		lastError, providerID sql.NullString
		payload               []byte
		announcementID        sql.NullString
		readAt                sql.NullTime
	)
	if err := scanner.Scan(&item.ID, &templateKey, &item.Category, &channel, &item.Recipient,
		&recipientUserID, &locale, &item.DedupeKey, &item.Status, &item.Attempt,
		&nextRetryAt, &lastError, &providerID, &payload, &announcementID, &readAt,
		&item.CreatedAt, &item.UpdatedAt); err != nil {
		return Delivery{}, err
	}
	item.Channel = Channel(channel)
	item.Payload = payload
	// payload 为 NULL、非对象或缺少该键时 -> 为 SQL NULL，归一为空串；
	// 视图层靠 omitempty 决定是否下发。
	item.AnnouncementID = announcementID.String
	if readAt.Valid {
		value := readAt.Time
		item.ReadAt = &value
	}
	if templateKey.Valid {
		value := templateKey.String
		item.TemplateKey = &value
	}
	if recipientUserID.Valid {
		value := recipientUserID.Int64
		item.RecipientUserID = &value
	}
	if locale.Valid {
		value := Locale(locale.String)
		item.Locale = &value
	}
	if nextRetryAt.Valid {
		value := nextRetryAt.Time
		item.NextRetryAt = &value
	}
	if lastError.Valid {
		value := lastError.String
		item.LastError = &value
	}
	if providerID.Valid {
		value := providerID.String
		item.ProviderMessageID = &value
	}
	return item, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableInt64Ptr(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// nullableJSON 把空 payload 写成 SQL NULL：email/webhook 行没有载荷，
// 用空对象会让「无载荷」与「空载荷」不可区分。
func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

// categoryForKey 在写入前由注册表裁决；Store 不做第二真源校验，但需要一个
// 稳定映射避免调用方漏传。未知 key 在 Service 层已被 404。
func categoryForKey(key string) string {
	if category, ok := LookupCategory(key); ok {
		return category.Key
	}
	return key
}
