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
	// ErrUnknownTemplate 表示 key×channel 未注册（HTTP 404）。
	ErrUnknownTemplate = errors.New("notify: unknown template")
	// ErrTemplateMissing 表示语言回退链（语言 → en）全部缺行（P1 fail-closed）。
	ErrTemplateMissing = errors.New("notify: template row missing for all locales")
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
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// TemplateFilter 是模板列表筛选（category/channel/locale/仅已修改）。
type TemplateFilter struct {
	Category     string
	Channel      Channel
	Locale       Locale
	ModifiedOnly bool
}

// UpdateInput 是一次模板写入。ExpectUpdatedAt 为 nil 表示不启用乐观锁
// （首次补种或前端明确不带版本）；非 nil 时必须与 DB 当前值一致（409）。
type UpdateInput struct {
	Key             string
	Channel         Channel
	Locale          Locale
	Subject         string
	Body            string
	CTAText         string
	CTAURL          string
	Enabled         bool
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
// 行不存在时插入（内置行可能尚未补种）；存在时要求 ExpectUpdatedAt 匹配。
// 注意 expect 为 nil 表示「不校验版本」，用于补种/恢复默认等系统写入，
// 普通管理端 PUT 会带上响应里的 updated_at。
func (s *Store) Update(ctx context.Context, in UpdateInput) (TemplateRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TemplateRow{}, fmt.Errorf("notify: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current time.Time
	err = tx.QueryRowContext(ctx, `SELECT updated_at FROM notification_templates
		WHERE "key" = $1 AND channel = $2 AND locale = $3 FOR UPDATE`,
		in.Key, string(in.Channel), string(in.Locale)).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if in.ExpectUpdatedAt != nil {
			// 调用方认为行存在但实际不存在：这是版本冲突而不是「静默新建」。
			return TemplateRow{}, ErrConflict
		}
	case err != nil:
		return TemplateRow{}, fmt.Errorf("notify: lock template: %w", err)
	default:
		if in.ExpectUpdatedAt != nil && !current.Equal(*in.ExpectUpdatedAt) {
			return TemplateRow{}, ErrConflict
		}
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
		in.IsModified, in.Enabled, nullableInt64(in.Actor))

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
		 dedupe_key, status, attempt, last_error, provider_message_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (dedupe_key) DO NOTHING
		RETURNING id, created_at, updated_at`,
		templateKey, d.Category, string(d.Channel), d.Recipient, nullableInt64Ptr(d.RecipientUserID),
		locale, d.DedupeKey, d.Status, d.Attempt, lastError, providerID)
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
		created_at, updated_at
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
	)
	if err := scanner.Scan(&item.ID, &templateKey, &item.Category, &channel, &item.Recipient,
		&recipientUserID, &locale, &item.DedupeKey, &item.Status, &item.Attempt,
		&nextRetryAt, &lastError, &providerID, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return Delivery{}, err
	}
	item.Channel = Channel(channel)
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

// categoryForKey 在写入前由注册表裁决；Store 不做第二真源校验，但需要一个
// 稳定映射避免调用方漏传。未知 key 在 Service 层已被 404。
func categoryForKey(key string) string {
	if category, ok := LookupCategory(key); ok {
		return category.Key
	}
	return key
}
