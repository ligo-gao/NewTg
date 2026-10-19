// 通知模板后端端点（modules-08 F-08-24～27 / §3.9；UI §6.9）。
//
// 冻结路由（Task 4 brief）：
//
//	GET  /api/notification-templates?category=&channel=&locale=&modified=   admin
//	GET  /api/notification-templates/{key}?locale=&channel=                admin
//	PUT  /api/notification-templates/{key}                                 settings:manage
//	POST /api/notification-templates/{key}:preview                         admin
//	POST /api/notification-templates/{key}:test                            settings:manage
//	GET  /api/notification-deliveries?p=&page_size=&status=                admin
//
// 路由实现细节：Go 1.22 ServeMux 的通配符必须是完整路径段，`{key}:preview`
// 会被拒绝（bad wildcard segment）。因此 POST 只注册 `{key}`，在处理器内
// 按后缀分派到 preview / test；未知后缀一律 404，不猜测意图。
//
// 权限口径来自 brief：读 admin（与 settings 读一致），写 settings:manage。
// 字段级 BOPLA 由 rbac.AuthzMiddleware 的写策略兜底，未登记的 body 字段 403。
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

// 模板正文上限 16KB（§3.9.2）；请求体额外允许 JSON 结构与变量，取 64KB。
// 超过即 400，而不是让 MaxBytesReader 在深层解析时才截断。
const notificationTemplateMaxBody = 64 << 10

var (
	notificationTemplateUpdateAction = rbac.AuthzAction{ResourceKind: "notification_template", ActionName: "update"}
	notificationTemplateTestAction   = rbac.AuthzAction{ResourceKind: "notification_template", ActionName: "test"}
)

// NotificationTemplateService 是 httpapi 对 notify.Service 的最小依赖面，
// 单测可注入 fake（不触真库）。
type NotificationTemplateService interface {
	List(ctx context.Context, filter notify.TemplateFilter) ([]notify.TemplateView, error)
	Get(ctx context.Context, key string, channel notify.Channel, locale notify.Locale) (notify.TemplateView, error)
	Update(ctx context.Context, in notify.UpdateInput) (notify.TemplateView, error)
	Preview(ctx context.Context, key string, channel notify.Channel, locale notify.Locale, variables map[string]any) (notify.Rendered, error)
	TestSend(ctx context.Context, in notify.TestInput) (notify.Delivery, error)
	ListDeliveries(ctx context.Context, filter notify.DeliveryFilter) ([]notify.DeliveryView, int, error)
}

// NotificationTemplateHandlerConfig 是端点装配输入。
type NotificationTemplateHandlerConfig struct {
	Service NotificationTemplateService
	Authz   *rbac.AuthzMiddleware
	Audit   *audit.Logger
	Logger  *slog.Logger
	// Now 是测试发送限流窗口的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// NotificationTemplateHandlers 可并发使用。
type NotificationTemplateHandlers struct {
	service NotificationTemplateService
	authz   *rbac.AuthzMiddleware
	audit   *audit.Logger
	log     *slog.Logger
	limiter *testSendLimiter
	// preview / test 是预先包好鉴权链的处理器：POST 只有一个模式，
	// 分派时必须应用各自的权限（preview=admin，test=settings:manage）。
	preview http.Handler
	test    http.Handler
}

// NewNotificationTemplateHandlers 构造端点。
func NewNotificationTemplateHandlers(cfg NotificationTemplateHandlerConfig) (*NotificationTemplateHandlers, error) {
	if cfg.Service == nil {
		return nil, errors.New("httpapi: NotificationTemplateHandlerConfig.Service is required")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	// 写侧字段白名单：模板编辑允许的字段。未登记字段由 RBAC 层 403
	// （security-02 §2.6.1 规则 6），绝不静默丢弃。
	authz.SetWritePolicy(notificationTemplateUpdateAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"locale":            nil,
		"channel":           nil,
		"subject":           nil,
		"body":              nil,
		"cta_text":          nil,
		"cta_url":           nil,
		"enabled":           nil,
		"expect_updated_at": nil,
	}})
	// preview 是读语义，不登记写策略（不经 RBAC 写链）。
	authz.SetWritePolicy(notificationTemplateTestAction, &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
		"locale":    nil,
		"channel":   nil,
		"variables": nil,
		"recipient": nil,
	}})

	h := &NotificationTemplateHandlers{
		service: cfg.Service,
		authz:   authz,
		audit:   cfg.Audit,
		log:     log,
		limiter: newTestSendLimiter(now),
	}
	// preview 是读语义（brief：admin）；test 是写语义（settings:manage）。
	h.preview = requireAdminRole(http.HandlerFunc(h.handlePreview))
	h.test = authz.Handler(rbac.PermSettingsManage, notificationTemplateTestAction)(http.HandlerFunc(h.handleTestSend))
	return h, nil
}

// Register 挂载全部端点。调用方保证 mux 已在 SessionAuth/CSRF 之后。
func (h *NotificationTemplateHandlers) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/notification-templates", requireAdminRole(http.HandlerFunc(h.handleList)))
	mux.Handle("GET /api/notification-templates/{key}", requireAdminRole(http.HandlerFunc(h.handleGet)))
	mux.Handle("PUT /api/notification-templates/{key}",
		h.authz.Handler(rbac.PermSettingsManage, notificationTemplateUpdateAction)(http.HandlerFunc(h.handlePut)))
	// ServeMux 不接受 `{key}:preview` 作为模式（通配符必须占满路径段），
	// 故 POST 统一入口后按后缀分派。
	mux.Handle("POST /api/notification-templates/{key}", http.HandlerFunc(h.handlePost))
	mux.Handle("GET /api/notification-deliveries", requireAdminRole(http.HandlerFunc(h.handleDeliveries)))
}

func (h *NotificationTemplateHandlers) handlePost(w http.ResponseWriter, r *http.Request) {
	rawKey := r.PathValue("key")
	switch {
	case strings.HasSuffix(rawKey, ":preview"):
		h.preview.ServeHTTP(w, r)
	case strings.HasSuffix(rawKey, ":test"):
		h.test.ServeHTTP(w, r)
	default:
		writeNotificationProblem(w, http.StatusNotFound, "not_found",
			"Not found", "unknown notification-template action")
	}
}

func (h *NotificationTemplateHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	category := strings.TrimSpace(query.Get("category"))
	if category != "" && !notify.IsRegisteredCategory(category) {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown category")
		return
	}
	channel, ok := notify.ParseChannel(query.Get("channel"))
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown channel")
		return
	}
	locale, ok := notify.ParseLocale(query.Get("locale"))
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown locale")
		return
	}
	filter := notify.TemplateFilter{
		Category:     category,
		ModifiedOnly: query.Get("modified") == "true" || query.Get("modified") == "1",
	}
	// 空 channel/locale 表示不过滤；ParseChannel/ParseLocale 会把空值归一到
	// email/en，因此这里按原始值判断是否真的指定了筛选。
	if strings.TrimSpace(query.Get("channel")) != "" {
		filter.Channel = channel
	}
	if strings.TrimSpace(query.Get("locale")) != "" {
		filter.Locale = locale
	}
	items, err := h.service.List(r.Context(), filter)
	if err != nil {
		h.writeServiceError(w, "list notification templates", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *NotificationTemplateHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	channel, locale, ok := parseTemplateTarget(w, r, key)
	if !ok {
		return
	}
	view, err := h.service.Get(r.Context(), key, channel, locale)
	if err != nil {
		h.writeServiceError(w, "get notification template", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type notificationTemplatePutRequest struct {
	Locale          string     `json:"locale"`
	Channel         string     `json:"channel"`
	Subject         string     `json:"subject"`
	Body            string     `json:"body"`
	CTAText         string     `json:"cta_text"`
	CTAURL          string     `json:"cta_url"`
	Enabled         *bool      `json:"enabled"`
	ExpectUpdatedAt *time.Time `json:"expect_updated_at"`
}

func (h *NotificationTemplateHandlers) handlePut(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req notificationTemplatePutRequest
	if !decodeNotificationBody(w, r, &req) {
		return
	}
	channel, ok := notify.ParseChannel(req.Channel)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown channel")
		return
	}
	locale, ok := notify.ParseLocale(req.Locale)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown locale")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	// 审计 before 摘要：模板正文可能很长，审计只留可检索的身份与开关状态；
	// 正文版本由 updated_at 唯一标识（§3.9.2 的 updated_at 即版本）。
	var before *notify.TemplateView
	if previous, err := h.service.Get(r.Context(), key, channel, locale); err == nil {
		before = &previous
	}
	view, err := h.service.Update(r.Context(), notify.UpdateInput{
		Key:             key,
		Channel:         channel,
		Locale:          locale,
		Subject:         req.Subject,
		Body:            req.Body,
		CTAText:         req.CTAText,
		CTAURL:          req.CTAURL,
		Enabled:         enabled,
		ExpectUpdatedAt: req.ExpectUpdatedAt,
		Actor:           actorID(r),
	})
	if err != nil {
		h.writeServiceError(w, "update notification template", err)
		return
	}
	h.auditUpdate(r, before, view)
	writeJSON(w, http.StatusOK, view)
}

type notificationTemplatePreviewRequest struct {
	Locale    string         `json:"locale"`
	Channel   string         `json:"channel"`
	Variables map[string]any `json:"variables"`
}

func (h *NotificationTemplateHandlers) handlePreview(w http.ResponseWriter, r *http.Request) {
	key, ok := stripActionSuffix(r.PathValue("key"), ":preview")
	if !ok {
		writeNotificationProblem(w, http.StatusNotFound, "not_found", "Not found", "unknown template action")
		return
	}
	var req notificationTemplatePreviewRequest
	if !decodeNotificationBody(w, r, &req) {
		return
	}
	channel, ok := notify.ParseChannel(req.Channel)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown channel")
		return
	}
	locale, ok := notify.ParseLocale(req.Locale)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown locale")
		return
	}
	rendered, err := h.service.Preview(r.Context(), key, channel, locale, req.Variables)
	if err != nil {
		h.writeServiceError(w, "preview notification template", err)
		return
	}
	// 契约冻结 {subject, body}；纯文本替换，不做格式化（P2）。
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": rendered.Subject,
		"body":    rendered.Body,
	})
}

type notificationTemplateTestRequest struct {
	Locale    string         `json:"locale"`
	Channel   string         `json:"channel"`
	Variables map[string]any `json:"variables"`
	Recipient string         `json:"recipient"`
}

func (h *NotificationTemplateHandlers) handleTestSend(w http.ResponseWriter, r *http.Request) {
	key, ok := stripActionSuffix(r.PathValue("key"), ":test")
	if !ok {
		writeNotificationProblem(w, http.StatusNotFound, "not_found", "Not found", "unknown template action")
		return
	}
	var req notificationTemplateTestRequest
	if !decodeNotificationBody(w, r, &req) {
		return
	}
	channel, ok := notify.ParseChannel(req.Channel)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown channel")
		return
	}
	locale, ok := notify.ParseLocale(req.Locale)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown locale")
		return
	}
	actor := actorID(r)
	// 测试发送限流 3 次/分钟（§3.9.6d 对齐 modules-01:532），按管理员计。
	// 这是独立的进程内桶：只保护 SMTP 出口，不与登录/兑换桶共用键。
	if !h.limiter.allow(actor) {
		writeNotificationProblem(w, http.StatusTooManyRequests, "rate_limited",
			"Too many requests", "test send is limited to 3 times per minute")
		return
	}
	delivery, err := h.service.TestSend(r.Context(), notify.TestInput{
		Key:       key,
		Channel:   channel,
		Locale:    locale,
		Variables: req.Variables,
		Recipient: req.Recipient,
		ActorID:   actor,
	})
	if err != nil {
		h.writeServiceError(w, "test send notification template", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":     delivery.ID,
		"status": notify.APIDeliveryStatus(delivery.Status),
		"error":  nullableErrorMessage(delivery.LastError),
	})
}

func (h *NotificationTemplateHandlers) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	page, ok := parseNotifyPositiveInt(query.Get("p"), 1, 1_000_000)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "p must be a positive integer")
		return
	}
	pageSize, ok := parseNotifyPositiveInt(query.Get("page_size"), 20, 100)
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "page_size must be between 1 and 100")
		return
	}
	filter := notify.DeliveryFilter{
		Category: strings.TrimSpace(query.Get("category")),
		Status:   strings.TrimSpace(query.Get("status")),
		Page:     page,
		PageSize: pageSize,
	}
	if rawChannel := strings.TrimSpace(query.Get("channel")); rawChannel != "" {
		channel, valid := notify.ParseChannel(rawChannel)
		if !valid {
			writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
				"Invalid request", "unknown channel")
			return
		}
		filter.Channel = channel
	}
	items, total, err := h.service.ListDeliveries(r.Context(), filter)
	if err != nil {
		h.writeServiceError(w, "list notification deliveries", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// parseTemplateTarget 解析 GET /{key} 的 channel/locale。key 必须在注册表内
// 由 service 裁决（404）；这里只处理参数形态，避免把 400/404 混淆。
func parseTemplateTarget(w http.ResponseWriter, r *http.Request, key string) (notify.Channel, notify.Locale, bool) {
	query := r.URL.Query()
	channel, ok := notify.ParseChannel(query.Get("channel"))
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown channel")
		return "", "", false
	}
	locale, ok := notify.ParseLocale(query.Get("locale"))
	if !ok {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown locale")
		return "", "", false
	}
	if strings.TrimSpace(key) == "" {
		writeNotificationProblem(w, http.StatusNotFound, "not_found", "Not found", "template key is required")
		return "", "", false
	}
	return channel, locale, true
}

func stripActionSuffix(raw, suffix string) (string, bool) {
	if !strings.HasSuffix(raw, suffix) {
		return "", false
	}
	key := strings.TrimSuffix(raw, suffix)
	if key == "" {
		return "", false
	}
	return key, true
}

// decodeNotificationBody 读取并解析 JSON 请求体；失败已写响应。
// 使用 UseNumber 保留整数形态（模板变量 `expires_minutes` 不能被 float64
// 格式化成 10.0）。
func decodeNotificationBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, notificationTemplateMaxBody))
	if err != nil {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unable to read request body")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "invalid JSON body")
		return false
	}
	return true
}

func parseNotifyPositiveInt(raw string, fallback, max int) (int, bool) {
	if strings.TrimSpace(raw) == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > max {
		return 0, false
	}
	return value, true
}

func actorID(r *http.Request) int64 {
	if principal, ok := auth.PrincipalFrom(r.Context()); ok && principal != nil {
		return principal.UserID
	}
	return 0
}

func nullableErrorMessage(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

// auditUpdate 写模板编辑审计（mutation 类必须留痕）。before/after 只含摘要：
// key/channel/locale/subject/enabled/is_modified；正文不入审计（体量大且可由
// updated_at 版本定位），避免审计表成为模板内容的第二副本。
func (h *NotificationTemplateHandlers) auditUpdate(r *http.Request, before *notify.TemplateView, after notify.TemplateView) {
	if h.audit == nil {
		return
	}
	detail, _ := json.Marshal(map[string]any{
		"key":      after.Key,
		"channel":  after.Channel,
		"locale":   after.Locale,
		"category": after.Category,
	})
	summary := func(view *notify.TemplateView) json.RawMessage {
		if view == nil {
			return nil
		}
		raw, _ := json.Marshal(map[string]any{
			"subject":    view.Subject,
			"enabled":    view.Enabled,
			"customized": view.Customized,
		})
		return raw
	}
	if err := writeAuditEvent(r.Context(), h.audit, audit.Event{
		Action: audit.ActionNotificationTemplateUpdate,
		Actor:  actorID(r),
		Detail: detail,
		Before: summary(before),
		After:  summary(&after),
	}); err != nil {
		// 审计失败不回滚业务写入，但必须留下错误日志（与 settings 写路径一致）。
		h.log.Error("notify: write template audit failed", "key", after.Key, "error", err)
	}
}

// writeServiceError 把 notify 包的语义错误映射为 RFC 7807。
// 映射顺序与设计一致：未知 key 404；变量/锁定/停用/SMTP 422；冲突 409。
func (h *NotificationTemplateHandlers) writeServiceError(w http.ResponseWriter, operation string, err error) {
	var (
		unknownVariable  *notify.ErrUnknownVariable
		missingVariable  *notify.ErrMissingVariable
		invalidConstruct *notify.ErrInvalidConstruct
		invalidValue     *notify.ErrInvalidValue
	)
	switch {
	case errors.Is(err, notify.ErrUnknownTemplate):
		writeNotificationProblem(w, http.StatusNotFound, "not_found",
			"Not found", "unknown notification template")
	case errors.Is(err, notify.ErrUnknownCategory):
		writeNotificationProblem(w, http.StatusBadRequest, "invalid_request",
			"Invalid request", "unknown category")
	case errors.Is(err, notify.ErrConflict):
		writeNotificationProblem(w, http.StatusConflict, "conflict",
			"Conflict", "template was updated by someone else")
	case errors.As(err, &unknownVariable):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "invalid_template",
			"Invalid template", err.Error())
	case errors.As(err, &missingVariable):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "missing_variable",
			"Missing variable", err.Error())
	case errors.As(err, &invalidConstruct):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "invalid_template",
			"Invalid template", err.Error())
	case errors.As(err, &invalidValue):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "invalid_variable",
			"Invalid variable", err.Error())
	case errors.Is(err, notify.ErrLockedTemplate):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "locked_template",
			"Template is locked", "this template cannot be disabled")
	case errors.Is(err, notify.ErrTemplateDisabled):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "template_disabled",
			"Template is disabled", "enable the template before sending")
	case errors.Is(err, notify.ErrSMTPNotConfigured):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "smtp_not_configured",
			"SMTP is not configured", "configure notify.smtp before sending a test message")
	case errors.Is(err, notify.ErrNotEmailTemplate):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "not_email_template",
			"Unsupported channel", "test send is only available for email templates")
	case errors.Is(err, notify.ErrTemplateMissing):
		writeNotificationProblem(w, http.StatusUnprocessableEntity, "template_missing",
			"Template missing", "no template is available for the requested locale")
	default:
		// 其余错误来自校验（静态文案/收件人）或数据库；前者是管理员输入问题，
		// 统一 422 而不是 500。数据库错误仍按 500 并记录日志。
		if isValidationError(err) {
			writeNotificationProblem(w, http.StatusUnprocessableEntity, "invalid_request",
				"Invalid request", err.Error())
			return
		}
		h.log.Error("notify: "+operation+" failed", "error", err)
		writeNotificationProblem(w, http.StatusInternalServerError, "internal_error",
			"Internal error", "operation failed")
	}
}

// isValidationError 识别渲染层/收件人校验返回的纯文本错误。
// 这些错误没有哨兵类型，但都来自 notify 包的前置校验；用前缀判断比把所有
// 文本错误都当 500 更准确，也不会泄露数据库细节（数据库错误带 "notify: xxx: %w"）。
func isValidationError(err error) bool {
	message := err.Error()
	for _, prefix := range []string{
		"notify: inapp template",
		"notify: email template",
		"notify: template",
		"notify: subject exceeds",
		"notify: body exceeds",
		"notify: CTA",
		"notify: invalid recipient",
		"notify: unknown delivery status",
	} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
}

func writeNotificationProblem(w http.ResponseWriter, status int, problemType, title, detail string) {
	writeProblemDetail(w, status, "https://newtg.dev/problems/"+problemType, title, detail)
}

// testSendLimiter 是测试发送的独立固定窗口桶（3 次/分钟/管理员）。
// 进程内实现：测试邮件是低频管理操作，多副本下最坏允许 N×3 次/分钟，
// 不构成业务风险；若将来需要严格跨副本限流，替换为 Redis 桶即可。
type testSendLimiter struct {
	mu     sync.Mutex
	hits   map[int64][]time.Time
	now    func() time.Time
	limit  int
	window time.Duration
}

func newTestSendLimiter(now func() time.Time) *testSendLimiter {
	return &testSendLimiter{
		hits:   make(map[int64][]time.Time),
		now:    now,
		limit:  3,
		window: time.Minute,
	}
}

func (l *testSendLimiter) allow(userID int64) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	existing := l.hits[userID]
	kept := existing[:0]
	for _, hit := range existing {
		if hit.After(cutoff) {
			kept = append(kept, hit)
		}
	}
	if len(kept) >= l.limit {
		l.hits[userID] = kept
		return false
	}
	l.hits[userID] = append(kept, now)
	return true
}
