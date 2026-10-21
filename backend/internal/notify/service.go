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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strconv"
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
	// EmailProducer 把管理员侧事件（如渠道自动禁用）同时写入 email 队列
	// （W10-EM1）。nil 时只做站内直投——纯 Repo 单测与未装配 SMTP 的进程
	// 不应因此失败。
	EmailProducer *EmailEventProducer
	Logger        *slog.Logger
	// Now 是测试发送 dedupe_key 的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// Service 可并发使用。
type Service struct {
	store         *Store
	sender        Sender
	emailProducer *EmailEventProducer
	log           *slog.Logger
	now           func() time.Time
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
	return &Service{
		store: cfg.Store, sender: cfg.Sender, emailProducer: cfg.EmailProducer,
		log: log, now: now,
	}, nil
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
	if in.Enabled != nil && !*in.Enabled && spec.IsLocked() {
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

// ---------------------------------------------------------------------------
// A5：公告站内通知投递与用户侧读取
// ---------------------------------------------------------------------------

// AnnouncementDeliveryInput 是一次公告发布需要广播的站内通知内容。
// Summary 由调用方从公告正文派生（公告表本身没有 summary 列），LinkURL 是
// 站内相对路径或绝对 URL；两者都是 `system.announcement` 模板的变量。
type AnnouncementDeliveryInput struct {
	AnnouncementID string
	Title          string
	Summary        string
	LinkURL        string
	PublishedAt    time.Time
}

// AnnouncementDeliveryResult 是一次广播的计数（写审计 detail 用，不含正文）。
type AnnouncementDeliveryResult struct {
	Delivered  int
	Duplicates int
	Failed     int
}

// InAppPayload 是站内通知的落库载荷（notification_deliveries.payload）。
// 读取端直接消费它，不回头查公告表：发布后编辑公告（N-35 原地更新）不应
// 让用户已经收到的「站内信」改变标题/摘要。
type InAppPayload struct {
	// AnnouncementID 是公告投递的归因键（A5 残余补批）。读取端用它关联
	// announcements，过滤「公告已下线/已过期/已删除」的投递；普通站内通知
	// 没有这个字段，读取端按「无关联」原样返回。
	//
	// 为什么复制到 payload 而不是从 dedupe_key 解析：读路径只该依赖一个
	// 稳定、可索引的载荷键；dedupe_key 是幂等锚，格式冻结但语义属于写入侧
	// （见 G-DOM-37 ⑧⑨）。
	AnnouncementID string `json:"announcement_id,omitempty"`
	Title          string `json:"title"`
	Summary        string `json:"summary,omitempty"`
	Body           string `json:"body,omitempty"`
	LinkURL        string `json:"link_url,omitempty"`
	PublishedAt    string `json:"published_at,omitempty"`
}

// DeliverAnnouncementInApp 为每个启用用户生成一条 `system.announcement`
// 站内投递记录（A5 冻结语义）。
//
// 幂等：dedupe_key=`announcement:<id>:<user_id>`，ON CONFLICT DO NOTHING。
// 有效一次：重复发布/重试只会增加 Duplicates，不会产生第二条未读。
// 失败软着陆：任何模板缺失/渲染失败都只记 Warn 并继续；调用方（公告 handler）
// 只把本函数当 best-effort 副作用，绝不因它回滚发布。
//
// 模板渲染只影响 payload.body（展示用富文本）；title/summary/link_url 是
// 原始变量，即使渲染失败也能展示——这比 P1「绝不静默发空」更严格地保留了
// 用户可见信息，同时不会发出空标题的站内信。
func (s *Service) DeliverAnnouncementInApp(ctx context.Context, in AnnouncementDeliveryInput) (AnnouncementDeliveryResult, error) {
	var result AnnouncementDeliveryResult
	if strings.TrimSpace(in.AnnouncementID) == "" {
		return result, errors.New("notify: announcement id is required")
	}
	// 先解析模板再取受众：模板是编译期注册表，缺失属于代码缺陷；此时用
	// count 保留旧口径「Failed = 启用用户数」，同时避免把整表读进内存。
	spec, ok := LookupTemplate("system.announcement", ChannelInApp)
	if !ok {
		// 注册表是编译期常量；走到这里说明代码被改坏，仍不阻塞发布。
		s.log.Error("notify: system.announcement template is not registered")
		failed, countErr := s.store.CountInAppAudience(ctx)
		if countErr != nil {
			return result, countErr
		}
		return AnnouncementDeliveryResult{Failed: failed}, nil
	}

	vars := map[string]any{
		"title":        in.Title,
		"summary":      in.Summary,
		"link_url":     in.LinkURL,
		"published_at": in.PublishedAt.UTC().Format(time.RFC3339),
	}
	bodies := make(map[Locale]string)
	payloads := make(map[Locale][]byte)

	buildPayload := func(locale Locale) []byte {
		payload := InAppPayload{
			AnnouncementID: in.AnnouncementID,
			Title:          in.Title,
			Summary:        in.Summary,
			Body:           bodies[locale],
			LinkURL:        in.LinkURL,
			PublishedAt:    vars["published_at"].(string),
		}
		raw, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			s.log.Warn("notify: marshal inapp payload failed", "err", marshalErr)
			return nil
		}
		return raw
	}

	const batchSize = 500
	batch := make([]Delivery, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		inserted, insertErr := s.store.InsertInAppDeliveries(ctx, batch)
		if insertErr != nil {
			result.Failed += len(batch)
			s.log.Warn("notify: insert inapp announcement deliveries failed",
				"announcement_id", in.AnnouncementID, "rows", len(batch), "err", insertErr)
		} else {
			result.Delivered += inserted
			result.Duplicates += len(batch) - inserted
		}
		batch = batch[:0]
	}

	// 受众按 users.id keyset 分页（每页 500）。旧实现一次 SELECT 全表再分批
	// 写入，用户量大时内存与查询时长都不可控；keyset（id > 上一页末值）比
	// OFFSET 稳定：投递期间新增用户不会被重复计入，也不会因删除而跳页。
	var afterID int64
	for {
		audience, err := s.store.ListInAppAudience(ctx, afterID, batchSize)
		if err != nil {
			// 已 flush 的批次保留，未拉取的批次不再尝试（best-effort，见
			// G-DOM-37 ⑤）。返回 result+err 让审计保留已完成计数。
			return result, err
		}
		if len(audience) == 0 {
			break
		}
		for _, user := range audience {
			locale := user.Locale
			if _, ok := payloads[locale]; !ok {
				body := ""
				if content, resolved, row, resolveErr := s.resolve(ctx, spec, locale, false); resolveErr != nil {
					// 模板行缺失（未播种/被删）不影响投递记录：payload 仍带原始
					// 标题与摘要，读取端照常展示（见函数头注释）。
					s.log.Warn("notify: resolve system.announcement template failed",
						"locale", locale, "err", resolveErr)
				} else if row != nil && !row.Enabled {
					s.log.Warn("notify: system.announcement template is disabled", "locale", locale)
				} else if rendered, renderErr := RenderContent(spec, content, resolved, vars); renderErr != nil {
					s.log.Warn("notify: render system.announcement failed",
						"locale", locale, "err", renderErr)
				} else {
					body = rendered.Body
				}
				bodies[locale] = body
				payloads[locale] = buildPayload(locale)
			}

			userID := user.UserID
			localeCopy := locale
			batch = append(batch, Delivery{
				TemplateKey:     stringPtr(spec.Key),
				Category:        spec.Category,
				Channel:         ChannelInApp,
				Recipient:       "user:" + strconv.FormatInt(userID, 10),
				RecipientUserID: &userID,
				Locale:          &localeCopy,
				// 冻结语义：dedupe 前缀固定为 announcement:<id>:<user>，公告
				// 删除按 `announcement:<id>:%` 前缀同事务清理。
				DedupeKey: "announcement:" + in.AnnouncementID + ":" + strconv.FormatInt(userID, 10),
				Status:    "sent",
				Attempt:   1,
				Payload:   payloads[locale],
			})
			if len(batch) >= batchSize {
				flush()
			}
		}
		flush()
		if len(audience) < batchSize {
			break
		}
		afterID = audience[len(audience)-1].UserID
	}
	return result, nil
}

// UserNotificationView 是 GET /api/notifications 的 item。
// 只暴露用户可见字段；dedupe_key/recipient_user_id 等内部幂等锚不外泄。
type UserNotificationView struct {
	ID          int64  `json:"id"`
	TemplateKey string `json:"template_key,omitempty"`
	Category    string `json:"category"`
	Title       string `json:"title"`
	Summary     string `json:"summary,omitempty"`
	Body        string `json:"body,omitempty"`
	LinkURL     string `json:"link_url,omitempty"`
	// AnnouncementID 是公告投递的归因键（payload.announcement_id）。
	// 只用于前端拼详情深链；普通站内通知没有该值。不下发 dedupe_key 等
	// 内部幂等锚，但 id 本身是公告的公开标识，可以安全暴露。
	AnnouncementID string     `json:"announcement_id,omitempty"`
	Locale         string     `json:"locale,omitempty"`
	ReadAt         *time.Time `json:"read_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// ListUserNotifications 返回本人站内通知（含 read_at）。userID 由认证上下文
// 提供，本方法不提供任何「按 user_id 参数查询他人」的路径。
func (s *Service) ListUserNotifications(ctx context.Context, userID int64, page, pageSize int) ([]UserNotificationView, int, error) {
	rows, total, err := s.store.ListUserInAppDeliveries(ctx, userID, page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	out := make([]UserNotificationView, 0, len(rows))
	for _, row := range rows {
		view := UserNotificationView{
			ID:        row.ID,
			Category:  row.Category,
			ReadAt:    row.ReadAt,
			CreatedAt: row.CreatedAt,
		}
		// 归因键由存储层从 payload->>'announcement_id' 单独提取，
		// 不依赖下面的整包解码：payload 结构损坏时其它字段留空，
		// 但已提取的 id 仍保留。
		view.AnnouncementID = row.AnnouncementID
		if row.TemplateKey != nil {
			view.TemplateKey = *row.TemplateKey
		}
		if row.Locale != nil {
			view.Locale = string(*row.Locale)
		}
		if len(row.Payload) > 0 {
			var payload InAppPayload
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				// 载荷损坏不应让整页 500：保留 id/时间，内容留空。
				s.log.Warn("notify: decode inapp payload failed", "delivery_id", row.ID, "err", err)
			} else {
				view.Title = payload.Title
				view.Summary = payload.Summary
				view.Body = payload.Body
				view.LinkURL = payload.LinkURL
			}
		}
		out = append(out, view)
	}
	return out, total, nil
}

// CountUserUnreadNotifications 返回本人「可见且未读」的站内通知数。
// 与 ListUserNotifications 共用存储层的可见性口径，徽章不会与列表打架。
func (s *Service) CountUserUnreadNotifications(ctx context.Context, userID int64) (int, error) {
	return s.store.CountUserUnreadInAppDeliveries(ctx, userID)
}

// MarkAllUserNotificationsRead 原子地把本人全部可见未读置读，返回更新行数。
// 重复调用返回 0；已读时刻保持首次值（存储层以 read_at IS NULL 限定）。
func (s *Service) MarkAllUserNotificationsRead(ctx context.Context, userID int64, now time.Time) (int, error) {
	return s.store.MarkAllUserInAppDeliveriesRead(ctx, userID, now)
}

// MarkUserNotificationRead 幂等标记本人一条站内通知已读。重复调用返回 nil。
func (s *Service) MarkUserNotificationRead(ctx context.Context, userID, id int64, now time.Time) error {
	return s.store.MarkInAppDeliveryRead(ctx, userID, id, now)
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
