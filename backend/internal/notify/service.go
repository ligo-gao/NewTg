package notify

// Service 把注册表（registry）、渲染（render）与存储（store）组合成 HTTP 层
// 可直接消费的用例。它是唯一知道「回退链」与「fail-closed」顺序的地方：
//   - 语言：请求语言 → en → 失败（§3.9.6a）；
//   - 变量：必填缺失 → 不发送（P1）；
//   - 模板停用：不发送（锁定类别不允许停用，P4）；
//   - 测试发送：SMTP 未配置 → 422，写投递记录（UI §6.9）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
)

// Sender 是测试发送的出口。生产实现是 SMTPSender；测试注入 fake。
type Sender interface {
	// Configured 报告 SMTP 是否可用（host 与 from_email 均非空）。
	// 不可用时测试发送必须 422，而不是假装入队。
	Configured(ctx context.Context) bool
	// Send 返回 SMTP Message-ID（供退信关联）与错误。
	Send(ctx context.Context, to, subject, body string) (string, error)
}

var (
	// ErrSMTPNotConfigured：SMTP 未配置（HTTP 422，F-08-19 依赖徽章口径）。
	ErrSMTPNotConfigured = errors.New("notify: SMTP is not configured")
	// ErrNotEmailTemplate：inapp 模板不能测试发送（只有 email 有 SMTP 出口）。
	ErrNotEmailTemplate = errors.New("notify: test send is only available for email templates")
	// ErrTemplateDisabled：模板已停用（不可停用的锁定类别在 Update 已拒绝）。
	ErrTemplateDisabled = errors.New("notify: template is disabled")
	// ErrLockedTemplate：P4 锁定类别不接受 enabled=false。
	ErrLockedTemplate = errors.New("notify: this template cannot be disabled")
)

// ServiceConfig 是 Service 的装配输入。
type ServiceConfig struct {
	Store  *Store
	Sender Sender
	Logger *slog.Logger
	// Now 是测试发送 dedupe_key 的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// Service 可并发使用。
type Service struct {
	store  *Store
	sender Sender
	log    *slog.Logger
	now    func() time.Time
}

// NewService 构造服务。Store 必填；Sender 允许为 nil（此时测试发送恒 422，
// 等价「SMTP 未配置」——装配层在无配置时也应得到同一行为）。
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("notify: service requires a store")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{store: cfg.Store, sender: cfg.Sender, log: log, now: now}, nil
}

// EnsureSeeded 幂等补种内置行。装配层在启动期调用；失败必须阻止上线
// （模板行缺失会让所有通知 fail-closed，属于配置性故障）。
func (s *Service) EnsureSeeded(ctx context.Context) error {
	return s.store.Seed(ctx)
}

// TemplateView 是列表/详情/写入的响应项。字段名是前端契约（UI §6.9）；
// customized 对应 DB 的 is_modified。
type TemplateView struct {
	Key        string     `json:"key"`
	Category   string     `json:"category"`
	Channel    Channel    `json:"channel"`
	Locale     Locale     `json:"locale"`
	Subject    string     `json:"subject"`
	Body       string     `json:"body"`
	CTAText    string     `json:"cta_text,omitempty"`
	CTAURL     string     `json:"cta_url,omitempty"`
	Enabled    bool       `json:"enabled"`
	Customized bool       `json:"customized"`
	UpdatedAt  *time.Time `json:"updated_at"`
}

// List 返回注册表全集（含未落库的内置默认行）。不依赖 Seed 已执行：
// 缺失行按 bundle 补视图，customized=false、updated_at=null。
func (s *Service) List(ctx context.Context, filter TemplateFilter) ([]TemplateView, error) {
	if filter.Category != "" && !IsRegisteredCategory(filter.Category) {
		return nil, ErrUnknownCategory
	}
	rows, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]TemplateRow, len(rows))
	for _, row := range rows {
		stored[templateRowKey(row.Key, row.Channel, row.Locale)] = row
	}

	locales := localeOrder
	if filter.Locale != "" {
		locales = []Locale{filter.Locale}
	}
	out := make([]TemplateView, 0, len(templateSpecs)*len(locales))
	for _, spec := range templateSpecs {
		if filter.Category != "" && spec.Category != filter.Category {
			continue
		}
		if filter.Channel != "" && spec.Channel != filter.Channel {
			continue
		}
		for _, locale := range locales {
			row, ok := stored[templateRowKey(spec.Key, spec.Channel, locale)]
			if ok {
				if filter.ModifiedOnly && !row.IsModified {
					continue
				}
				out = append(out, viewFromRow(row))
				continue
			}
			if filter.ModifiedOnly {
				continue
			}
			content, _ := spec.DefaultContent(locale)
			out = append(out, TemplateView{
				Key:        spec.Key,
				Category:   spec.Category,
				Channel:    spec.Channel,
				Locale:     locale,
				Subject:    content.Subject,
				Body:       content.Body,
				CTAText:    content.CTAText,
				CTAURL:     content.CTAURL,
				Enabled:    true,
				Customized: false,
			})
		}
	}
	return out, nil
}

// ErrUnknownCategory：category 不在 16 类注册表（400）。
var ErrUnknownCategory = errors.New("notify: unknown category")

// Get 返回一个 (key,channel,locale) 的有效视图。精确语言缺行时回退 en
// （§3.9.6a）；两者皆无则返回内置 bundle（仍然 fail-closed 的保护在发送路径）。
func (s *Service) Get(ctx context.Context, key string, channel Channel, locale Locale) (TemplateView, error) {
	spec, ok := LookupTemplate(key, channel)
	if !ok {
		return TemplateView{}, ErrUnknownTemplate
	}
	content, resolved, row, err := s.resolve(ctx, spec, locale, true)
	if err != nil {
		return TemplateView{}, err
	}
	view := TemplateView{
		Key:        spec.Key,
		Category:   spec.Category,
		Channel:    spec.Channel,
		Locale:     resolved,
		Subject:    content.Subject,
		Body:       content.Body,
		CTAText:    content.CTAText,
		CTAURL:     content.CTAURL,
		Enabled:    true,
		Customized: false,
	}
	if row != nil {
		view.Enabled = row.Enabled
		view.Customized = row.IsModified
		updated := row.UpdatedAt
		view.UpdatedAt = &updated
	}
	return view, nil
}

// Update 保存一份模板。调用方已完成 key/channel/locale 解析；这里负责：
//   - 内容静态校验（422）；
//   - P4 锁定类别拒绝 enabled=false；
//   - is_modified 由内容与内置默认的差异推导（不接受客户端自报）；
//   - 乐观锁（409）。
func (s *Service) Update(ctx context.Context, in UpdateInput) (TemplateView, error) {
	spec, ok := LookupTemplate(in.Key, in.Channel)
	if !ok {
		return TemplateView{}, ErrUnknownTemplate
	}
	if err := ValidateContent(spec, in.Subject, in.Body, in.CTAText, in.CTAURL); err != nil {
		return TemplateView{}, err
	}
	if !in.Enabled && spec.IsLocked() {
		return TemplateView{}, ErrLockedTemplate
	}
	in.IsModified = s.isModified(spec, in)

	row, err := s.store.Update(ctx, in)
	if err != nil {
		return TemplateView{}, err
	}
	return viewFromRow(row), nil
}

// isModified 以该语言的**内置默认**为基准判断「已修改」。内置默认缺失时
// 以 en 为基准（与 DefaultContent 同一回退链）。
func (s *Service) isModified(spec Template, in UpdateInput) bool {
	defaults, ok := spec.DefaultContent(in.Locale)
	if !ok {
		return true
	}
	return in.Subject != defaults.Subject ||
		in.Body != defaults.Body ||
		in.CTAText != defaults.CTAText ||
		in.CTAURL != defaults.CTAURL
}

// Preview 用样例值渲染保存中的模板（§3.9.6a）。不落投递记录。
// variables 覆盖同名样例；未提供的必填变量沿用样例值。
func (s *Service) Preview(ctx context.Context, key string, channel Channel, locale Locale, variables map[string]any) (Rendered, error) {
	spec, ok := LookupTemplate(key, channel)
	if !ok {
		return Rendered{}, ErrUnknownTemplate
	}
	content, resolved, row, err := s.resolve(ctx, spec, locale, true)
	if err != nil {
		return Rendered{}, err
	}
	if row != nil && !row.Enabled {
		return Rendered{}, ErrTemplateDisabled
	}
	if err := ValidateContent(spec, content.Subject, content.Body, content.CTAText, content.CTAURL); err != nil {
		return Rendered{}, err
	}
	merged := MergeVariables(spec, resolved, variables)
	if err := ValidateVariables(spec, merged); err != nil {
		return Rendered{}, err
	}
	return RenderContent(spec, content, resolved, merged)
}

// TestInput 是测试发送的输入。
type TestInput struct {
	Key       string
	Channel   Channel
	Locale    Locale
	Variables map[string]any
	Recipient string
	ActorID   int64
}

// TestSend 走与真实发送相同的渲染管道，仅投递给指定收件人（§3.9.6d：
// 测试发送豁免去重与业务窗口，UI 限流另计）。
//
// 返回的 Delivery 是落库后的投递记录；SMTP 未配置时返回 ErrSMTPNotConfigured
// 且**不写记录**（没有任何投递尝试，写一条 queued 只会污染排障视图）。
func (s *Service) TestSend(ctx context.Context, in TestInput) (Delivery, error) {
	spec, ok := LookupTemplate(in.Key, in.Channel)
	if !ok {
		return Delivery{}, ErrUnknownTemplate
	}
	if spec.Channel != ChannelEmail {
		return Delivery{}, ErrNotEmailTemplate
	}
	if err := validateRecipient(in.Recipient); err != nil {
		return Delivery{}, err
	}
	if s.sender == nil || !s.sender.Configured(ctx) {
		return Delivery{}, ErrSMTPNotConfigured
	}
	content, resolved, row, err := s.resolve(ctx, spec, in.Locale, false)
	if err != nil {
		// 语言全链回退失败（P1）：记录 failed 后返回错误，绝不静默发空。
		// 该记录没有渲染内容，但保留 locale/category 供排障。
		locale := in.Locale
		record := Delivery{
			TemplateKey: stringPtr(spec.Key),
			Category:    spec.Category,
			Channel:     ChannelEmail,
			Recipient:   strings.TrimSpace(in.Recipient),
			Locale:      &locale,
			DedupeKey:   fmt.Sprintf("%s:u%d:test:%d", spec.Category, in.ActorID, s.now().UnixNano()),
			Status:      "failed",
			Attempt:     0,
			LastError:   stringPtr(sanitizeError(err.Error())),
		}
		if _, _, insertErr := s.store.InsertDelivery(ctx, record); insertErr != nil {
			s.log.Error("notify: record missing-template test send", "key", spec.Key, "err", insertErr)
		}
		return Delivery{}, err
	}
	if row != nil && !row.Enabled {
		return Delivery{}, ErrTemplateDisabled
	}

	merged := MergeVariables(spec, resolved, in.Variables)
	record := Delivery{
		TemplateKey:     stringPtr(spec.Key),
		Category:        spec.Category,
		Channel:         ChannelEmail,
		Recipient:       strings.TrimSpace(in.Recipient),
		Locale:          &resolved,
		DedupeKey:       fmt.Sprintf("%s:u%d:test:%d", spec.Category, in.ActorID, s.now().UnixNano()),
		Attempt:         1,
		RecipientUserID: nil,
	}
	if err := ValidateVariables(spec, merged); err != nil {
		record.Status = "failed"
		record.LastError = stringPtr(sanitizeError(err.Error()))
		if _, _, insertErr := s.store.InsertDelivery(ctx, record); insertErr != nil {
			s.log.Error("notify: record failed test send", "key", spec.Key, "err", insertErr)
		}
		return Delivery{}, err
	}
	rendered, err := RenderContent(spec, content, resolved, merged)
	if err != nil {
		record.Status = "failed"
		record.LastError = stringPtr(sanitizeError(err.Error()))
		if _, _, insertErr := s.store.InsertDelivery(ctx, record); insertErr != nil {
			s.log.Error("notify: record failed test send", "key", spec.Key, "err", insertErr)
		}
		return Delivery{}, err
	}

	messageID, sendErr := s.sender.Send(ctx, record.Recipient, rendered.Subject, rendered.Body)
	record.Status = "sent"
	if sendErr != nil {
		record.Status = "failed"
		record.LastError = stringPtr(sanitizeError(sendErr.Error()))
	} else if messageID != "" {
		record.ProviderMessageID = &messageID
	}
	inserted, ok, err := s.store.InsertDelivery(ctx, record)
	if err != nil {
		// 发送已发生，记录失败不能谎报发送失败；返回记录错误让调用方 500。
		return Delivery{}, err
	}
	if !ok {
		// 理论上测试发送的 dedupe_key 含纳秒时间戳，不会冲突；冲突时仍以
		// 已存在的记录为准，不重复发送（本函数此时还没有第二次发送）。
		return Delivery{}, errors.New("notify: test send dedupe collision")
	}
	return inserted, nil
}

// ListDeliveries 返回 API 视图与总数。status=pending 是 API 口径，
// 对应 DB 的 queued（见 APIDeliveryStatus）。
func (s *Service) ListDeliveries(ctx context.Context, filter DeliveryFilter) ([]DeliveryView, int, error) {
	if filter.Status != "" {
		dbStatus, ok := DatabaseDeliveryStatus(filter.Status)
		if !ok {
			return nil, 0, fmt.Errorf("notify: unknown delivery status %q", filter.Status)
		}
		filter.Status = dbStatus
	}
	if filter.Category != "" && !IsRegisteredCategory(filter.Category) {
		return nil, 0, ErrUnknownCategory
	}
	rows, total, err := s.store.ListDeliveries(ctx, filter)
	if err != nil {
		return nil, 0, err
	}
	out := make([]DeliveryView, 0, len(rows))
	for _, row := range rows {
		out = append(out, viewFromDelivery(row))
	}
	return out, total, nil
}

// DeliveryView 是冻结合同 GET /api/notification-deliveries 的 item。
// 契约只列 failed|sent|pending；DB 另有 bounced/suppressed，按真实值返回
// （它们是可观测的终态，压成 failed 会丢失退信信息）。
type DeliveryView struct {
	ID          int64     `json:"id"`
	TemplateKey string    `json:"template_key,omitempty"`
	Category    string    `json:"category"`
	Channel     Channel   `json:"channel"`
	Recipient   string    `json:"recipient"`
	Locale      string    `json:"locale,omitempty"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// APIDeliveryStatus 把 DB 状态映射为 API 口径：queued → pending（契约词汇）。
func APIDeliveryStatus(status string) string {
	if status == "queued" {
		return "pending"
	}
	return status
}

// DatabaseDeliveryStatus 是 APIDeliveryStatus 的逆映射。
func DatabaseDeliveryStatus(status string) (string, bool) {
	switch status {
	case "pending", "queued":
		return "queued", true
	case "sent", "failed", "bounced", "suppressed":
		return status, true
	}
	return "", false
}

// resolve 实施语言回退链：精确行 → en 行 →（可选）内置 bundle。
//
// allowBundle 只对**读路径**（Get/List/Preview）为 true：管理界面必须能展示
// 「还没播种」的内置默认。发送路径（TestSend/真实管道）必须为 false——设计
// 的口径是「个人语言与站点默认 en 皆缺 → 不发送」，用 bundle 兜底会让
// 被删除的模板行看起来仍然可用，违反 P1 的 fail-closed。
func (s *Service) resolve(ctx context.Context, spec Template, locale Locale, allowBundle bool) (Content, Locale, *TemplateRow, error) {
	row, err := s.store.Get(ctx, spec.Key, spec.Channel, locale)
	if err == nil {
		return contentFromRow(row), locale, &row, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Content{}, "", nil, err
	}
	if locale != DefaultLocale {
		row, err := s.store.Get(ctx, spec.Key, spec.Channel, DefaultLocale)
		if err == nil {
			return contentFromRow(row), DefaultLocale, &row, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Content{}, "", nil, err
		}
	}
	if !allowBundle {
		return Content{}, "", nil, ErrTemplateMissing
	}
	content, ok := spec.DefaultContent(locale)
	if !ok {
		return Content{}, "", nil, ErrTemplateMissing
	}
	resolved := locale
	if _, exact := spec.Defaults[locale]; !exact {
		resolved = DefaultLocale
	}
	return content, resolved, nil, nil
}

func contentFromRow(row TemplateRow) Content {
	return Content{Subject: row.Subject, Body: row.Body, CTAText: row.CTAText, CTAURL: row.CTAURL}
}

func viewFromRow(row TemplateRow) TemplateView {
	updated := row.UpdatedAt
	return TemplateView{
		Key:        row.Key,
		Category:   row.Category,
		Channel:    row.Channel,
		Locale:     row.Locale,
		Subject:    row.Subject,
		Body:       row.Body,
		CTAText:    row.CTAText,
		CTAURL:     row.CTAURL,
		Enabled:    row.Enabled,
		Customized: row.IsModified,
		UpdatedAt:  &updated,
	}
}

func viewFromDelivery(row Delivery) DeliveryView {
	view := DeliveryView{
		ID:        row.ID,
		Category:  row.Category,
		Channel:   row.Channel,
		Recipient: row.Recipient,
		Status:    APIDeliveryStatus(row.Status),
		CreatedAt: row.CreatedAt,
	}
	if row.TemplateKey != nil {
		view.TemplateKey = *row.TemplateKey
	}
	if row.Locale != nil {
		view.Locale = string(*row.Locale)
	}
	if row.LastError != nil {
		view.Error = *row.LastError
	}
	return view
}

// IsLocked 报告该模板属于 P4 不可关集合（auth.* / security.alert /
// system.smtp_test / system.announcement）。
func (t Template) IsLocked() bool {
	category, ok := LookupCategory(t.Category)
	return ok && category.Locked
}

func templateRowKey(key string, channel Channel, locale Locale) string {
	return key + "\x00" + string(channel) + "\x00" + string(locale)
}

// validateRecipient 只做最小校验：net/mail 解析地址并限制 ≤320 字符
// （RFC 5321 的 path 上限 256 + 显示名余量）。不把 email 语法当成安全边界。
func validateRecipient(raw string) error {
	address := strings.TrimSpace(raw)
	if address == "" || len(address) > 320 {
		return errors.New("notify: invalid recipient")
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address == "" {
		return errors.New("notify: invalid recipient")
	}
	return nil
}

// sanitizeError 截断到 512 字符（表约束），并去掉换行以免污染日志视图。
func sanitizeError(message string) string {
	message = strings.ReplaceAll(message, "\n", " ")
	runes := []rune(message)
	if len(runes) > 512 {
		runes = runes[:512]
	}
	return string(runes)
}

func stringPtr(value string) *string { return &value }
