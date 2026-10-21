package notify

// W10-EM1：email 入队契约（`notification_deliveries.channel='email'`）。
//
// 设计权威：modules-08 §3.9.5「（b）队列」与 §3.9.6b（dedupe_key 唯一幂等
// 锚）。N8 的 webhook 生产者（webhook_producer.go）只负责 Webhook 订阅面；
// email 的收件人、语言与偏好来自 users 行，形状完全不同，因此单独一个生产者。
//
// 冻结语义（R-W10-06）：
//   - 入队时解析偏好：无映射类别（事务性/管理员侧）恒发；有映射类别
//     （balance_warning / usage_digest）opt-out → 写一条 suppressed 行，
//     不进入发送队列（保留可观测性，而不是静默丢弃）；
//   - 收件地址：users.email，balance_warning 类别的 custom_email 覆盖账号
//     邮箱；两者皆空 → 跳过（无地址可投递，不是失败）；
//   - dedupe_key 必须带 `email:` 前缀：同一业务事件也会产生 webhook/inapp
//     行，而 dedupe_key 是全局 UNIQUE，不区分渠道就会互相顶掉；
//   - 写入失败是 best-effort：业务调用方只记 Warn，绝不回滚已完成的
//     入账/禁用等权威状态。
//
// payload 是**模板变量**的 JSON 对象（预格式化字符串，P2）。变量集由注册表
// 白名单约束；这里不做渲染（worker 负责 fail-closed 的渲染与终态判定）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const (
	// siteTitleKey / siteServerAddressKey 与 httpapi/routes.go 的公共基址
	// 读取键一致（site.server_address 允许带尾斜杠）。
	siteTitleKey         = "site.title"
	siteServerAddressKey = "site.server_address"
)

// EmailEnqueueStatus 是一次入队尝试的结果分类。
type EmailEnqueueStatus string

const (
	// EmailEnqueueQueued：写入 queued，等待 worker 发送。
	EmailEnqueueQueued EmailEnqueueStatus = "queued"
	// EmailEnqueueSuppressed：用户已退订该类别，写入 terminal suppressed。
	EmailEnqueueSuppressed EmailEnqueueStatus = "suppressed"
	// EmailEnqueueDuplicate：dedupe_key 已存在（同一事件重复入队），不重复写。
	EmailEnqueueDuplicate EmailEnqueueStatus = "duplicate"
	// EmailEnqueueSkipped：没有可用收件地址/公共基址缺失，未写任何行。
	EmailEnqueueSkipped EmailEnqueueStatus = "skipped"
)

// EmailEnqueueResult 是入队结果。Inserted 表示本次真的新增了一行。
type EmailEnqueueResult struct {
	Status   EmailEnqueueStatus
	Inserted bool
}

// EmailEventProducer 把业务事件写成 email 投递行，可并发使用。
type EmailEventProducer struct {
	store *Store
	cfg   ConfigReader
	log   *slog.Logger
}

// NewEmailEventProducer 构造生产者。Store 必填；cfg 可为 nil（此时站点标题与
// 公共基址为空，需要绝对链接的类别会跳过入队并记 Warn，而不是写入必然会
// 渲染失败的投递行）。
func NewEmailEventProducer(store *Store, cfg ConfigReader, logger *slog.Logger) (*EmailEventProducer, error) {
	if store == nil {
		return nil, errors.New("notify: email event producer requires a store")
	}
	log := logger
	if log == nil {
		log = slog.Default()
	}
	return &EmailEventProducer{store: store, cfg: cfg, log: log}, nil
}

// EmailEvent 是通用 email 事件：调用方给出类别、幂等键与模板变量。
//
// TemplateKey 为空时取 Category（注册表当前一一对应）；Variables 里的值必须
// 已由业务方预格式化（P2），不得包含凭据、验证码之外的机密或原始上游错误。
type EmailEvent struct {
	Category    string
	TemplateKey string
	DedupeKey   string
	UserID      int64
	Variables   map[string]any
}

// Enqueue 写入一条 email 投递（或在 opt-out 时写 suppressed 终态）。
//
// 发送前偏好复核由 worker 再做一次（R-W10-06 A）：这里决定「是否入队」，
// worker 决定「入队后用户是否已退订」。两次判定共用同一组纯函数，避免
// 口径漂移。
func (p *EmailEventProducer) Enqueue(ctx context.Context, event EmailEvent) (EmailEnqueueResult, error) {
	if event.UserID <= 0 {
		return EmailEnqueueResult{}, errors.New("notify: email event requires a user id")
	}
	if strings.TrimSpace(event.DedupeKey) == "" {
		return EmailEnqueueResult{}, errors.New("notify: email event requires a dedupe key")
	}
	key := event.TemplateKey
	if key == "" {
		key = event.Category
	}
	spec, ok := LookupTemplate(key, ChannelEmail)
	if !ok {
		return EmailEnqueueResult{}, fmt.Errorf("notify: %s has no email template", key)
	}

	profile, err := p.store.LoadEmailProfile(ctx, event.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		// 用户已删除：没有收件人，也没有偏好可读；不入队。
		p.log.Debug("email event skipped: user not found", "category", event.Category)
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}
	if err != nil {
		return EmailEnqueueResult{}, fmt.Errorf("notify: load email profile: %w", err)
	}

	recipient := EmailAddressForCategory(profile.Preferences, event.Category, profile.Email)
	transactional := IsTransactionalEmailCategory(event.Category)

	if !transactional && HasEmailPreference(event.Category) {
		allowed := !profile.PreferencesCorrupt &&
			PreferencesAllowEmail(profile.Preferences, event.Category)
		if !allowed {
			if recipient == "" {
				// 没有地址就没有可观测的抑制对象；跳过而不是写空收件人
				// （表约束要求 recipient 非空）。
				return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
			}
			_, ok, err := p.store.InsertDelivery(ctx, Delivery{
				TemplateKey:     &key,
				Category:        event.Category,
				Channel:         ChannelEmail,
				Recipient:       recipient,
				RecipientUserID: &profile.UserID,
				Locale:          &profile.Locale,
				DedupeKey:       event.DedupeKey,
				Status:          "suppressed",
				Attempt:         0,
				LastError:       stringPtr("recipient opted out of email"),
			})
			if err != nil {
				return EmailEnqueueResult{}, err
			}
			return EmailEnqueueResult{Status: EmailEnqueueSuppressed, Inserted: ok}, nil
		}
	}

	if recipient == "" {
		p.log.Debug("email event skipped: no recipient address", "category", event.Category)
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}
	if err := validateRecipient(recipient); err != nil {
		// 地址格式非法是数据问题：跳过并留下类别日志，不写永远发不出去的
		// 投递行（否则 worker 每轮领取一次、重试到 failed，纯噪声）。
		p.log.Warn("email event skipped: invalid recipient address", "category", event.Category)
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}

	variables := make(map[string]any, len(event.Variables)+1)
	for name, value := range event.Variables {
		variables[name] = value
	}
	// username 是所有面向用户的 email 模板的可选变量，而业务方（扫描器/
	// 续费 worker）通常只持有 user id。这里统一从画像补齐，避免每个生产
	// 者都多查一次 users。
	if _, declared := spec.VariableByName("username"); declared {
		if _, present := variables["username"]; !present {
			variables["username"] = profile.Username
		}
	}
	payload, err := json.Marshal(variables)
	if err != nil {
		return EmailEnqueueResult{}, fmt.Errorf("notify: marshal email variables: %w", err)
	}

	_, inserted, err := p.store.InsertDelivery(ctx, Delivery{
		TemplateKey:     &key,
		Category:        event.Category,
		Channel:         ChannelEmail,
		Recipient:       recipient,
		RecipientUserID: &profile.UserID,
		Locale:          &profile.Locale,
		DedupeKey:       event.DedupeKey,
		Status:          "queued",
		Attempt:         0,
		Payload:         payload,
	})
	if err != nil {
		return EmailEnqueueResult{}, err
	}
	if !inserted {
		return EmailEnqueueResult{Status: EmailEnqueueDuplicate}, nil
	}
	return EmailEnqueueResult{Status: EmailEnqueueQueued, Inserted: true}, nil
}

// BalanceLowEmailEvent 是 N6 低余额扫描命中一个用户时的 email 输入。
type BalanceLowEmailEvent struct {
	UserID     int64
	Balance    int64
	Threshold  int64
	DetectedAt time.Time
	SiteDay    string
}

// EnqueueBalanceLow 写入一条 balance_low email（受 balance_warning 偏好约束）。
//
// topup_url 必须是绝对链接：email 模板把 CTA 渲染成可点击按钮，RenderContent
// 对非 http(s) 值 fail-closed。公共基址未配置时跳过并 Warn——写一条注定
// 终态 failed 的行只会污染排障视图。
func (p *EmailEventProducer) EnqueueBalanceLow(ctx context.Context, event BalanceLowEmailEvent) (EmailEnqueueResult, error) {
	base := p.baseURL()
	if base == "" {
		p.log.Warn("email balance_low skipped: site.server_address is not configured")
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}
	day := event.SiteDay
	if day == "" {
		detected := event.DetectedAt
		if detected.IsZero() {
			detected = time.Now()
		}
		day = detected.UTC().Format("2006-01-02")
	}
	return p.Enqueue(ctx, EmailEvent{
		Category:  "billing.balance_low",
		DedupeKey: fmt.Sprintf("email:balance_low:%d:%s", event.UserID, day),
		UserID:    event.UserID,
		Variables: map[string]any{
			"balance":    strconv.FormatInt(event.Balance, 10),
			"threshold":  strconv.FormatInt(event.Threshold, 10),
			"topup_url":  base + "/wallet",
			"site_title": p.siteTitle(),
		},
	})
}

// PaymentFailedEmailEvent 是一次支付失败 email 的输入。所有展示值由调用方
// 预格式化；本类型不感知订单/渠道实现。
type PaymentFailedEmailEvent struct {
	UserID      int64
	AmountLabel string
	OrderNo     string
	ReasonLabel string
	FailedAt    time.Time
	// DedupeKey 是业务侧事件键（例如自动续费的 notice.DedupeKey）。
	DedupeKey string
}

// EnqueuePaymentFailed 写入一条 billing.payment_failed email。
//
// 该类别属于 R-W10-06 冻结的事务性集合：只要用户有邮箱就恒发，不查偏好。
func (p *EmailEventProducer) EnqueuePaymentFailed(ctx context.Context, event PaymentFailedEmailEvent) (EmailEnqueueResult, error) {
	base := p.baseURL()
	if base == "" {
		p.log.Warn("email payment_failed skipped: site.server_address is not configured")
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}
	if strings.TrimSpace(event.DedupeKey) == "" {
		return EmailEnqueueResult{}, errors.New("notify: payment_failed email requires a dedupe key")
	}
	failedAt := event.FailedAt
	if failedAt.IsZero() {
		failedAt = time.Now()
	}
	return p.Enqueue(ctx, EmailEvent{
		Category:  "billing.payment_failed",
		DedupeKey: "email:" + event.DedupeKey,
		UserID:    event.UserID,
		Variables: map[string]any{
			"amount":       event.AmountLabel,
			"order_no":     event.OrderNo,
			"reason_label": event.ReasonLabel,
			"failed_at":    failedAt.UTC().Format(time.RFC3339),
			"retry_url":    base + "/wallet",
		},
	})
}

// ChannelAutoDisabledEmailEvent 是一次渠道自动禁用需要通知单个管理员的输入。
type ChannelAutoDisabledEmailEvent struct {
	ChannelID   int64
	ChannelName string
	ReasonCode  string
	DisabledAt  time.Time
	// DedupeKey 是事件前缀（与站内投递共用同一来源），追加管理员 id。
	DedupeKey   string
	AdminUserID int64
}

// EnqueueChannelAutoDisabled 写入一条 ops.channel_auto_disabled email。
//
// 管理员侧类别属于「无映射者恒发」：自动禁用是运维事件，不能因为普通用户
// 的营销偏好而被关闭（设计 §3.9.5c）。reason_label 按管理员语言本地化，
// 未知 code 用固定文案——绝不回显上游错误原文或渠道凭据。
func (p *EmailEventProducer) EnqueueChannelAutoDisabled(ctx context.Context, event ChannelAutoDisabledEmailEvent) (EmailEnqueueResult, error) {
	base := p.baseURL()
	if base == "" {
		p.log.Warn("email channel_auto_disabled skipped: site.server_address is not configured")
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}
	if strings.TrimSpace(event.DedupeKey) == "" {
		return EmailEnqueueResult{}, errors.New("notify: channel_auto_disabled email requires a dedupe key")
	}
	// 先读画像只为拿语言做 reason_label；Enqueue 会再读一次（管理员数量
	// 个位数，N+1 可接受，换来「偏好/收件人只有一处解析」的单一真源）。
	profile, err := p.store.LoadEmailProfile(ctx, event.AdminUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return EmailEnqueueResult{Status: EmailEnqueueSkipped}, nil
	}
	if err != nil {
		return EmailEnqueueResult{}, fmt.Errorf("notify: load admin email profile: %w", err)
	}
	_, reasonLabel := channelAutoDisabledCopy(profile.Locale, event.ReasonCode)
	disabledAt := event.DisabledAt
	if disabledAt.IsZero() {
		disabledAt = time.Now()
	}
	return p.Enqueue(ctx, EmailEvent{
		Category:  "ops.channel_auto_disabled",
		DedupeKey: fmt.Sprintf("email:%s:%d", event.DedupeKey, event.AdminUserID),
		UserID:    event.AdminUserID,
		Variables: map[string]any{
			"channel_name": event.ChannelName,
			"channel_id":   strconv.FormatInt(event.ChannelID, 10),
			"reason_label": reasonLabel,
			"disabled_at":  disabledAt.UTC().Format(time.RFC3339),
			"channels_url": base + "/admin/channels",
		},
	})
}

// baseURL 返回去掉尾斜杠的站点点位基址；未配置时为空串。
func (p *EmailEventProducer) baseURL() string {
	if p.cfg == nil {
		return ""
	}
	raw, ok := p.cfg.Get(siteServerAddressKey)
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func (p *EmailEventProducer) siteTitle() string {
	if p.cfg == nil {
		return ""
	}
	raw, ok := p.cfg.Get(siteTitleKey)
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
