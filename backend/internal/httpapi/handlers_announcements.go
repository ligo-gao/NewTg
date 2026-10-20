// 站点公告端点（D7a；modules-08 F-08-04 / R-D7-05～R-D7-11）。
//
// 端点面：
//
//	GET    /api/announcements                      管理：分页列表（筛选 status/category）
//	POST   /api/announcements                      管理：创建（publish_now 原子发布）
//	GET    /api/announcements/{id}                 管理：详情（含渲染后 HTML）
//	PUT    /api/announcements/{id}                 管理：编辑（N-35 语义）
//	DELETE /api/announcements/{id}                 管理：删除（同事务清已读）
//	POST   /api/announcements/{id}/publish         管理：发布（幂等）
//	POST   /api/announcements/{id}/offline         管理：下线（幂等）
//	GET    /api/announcements/active               用户：生效中公告 + read 标记
//	POST   /api/announcements/{id}/read            用户：标记已读（幂等）
//
// 设计约束：
//   - 管理面全部要求 settings:manage（R-D7-06/R-D7-11，不新增权限键）；
//     用户侧只要求已登录本人（R-D7-05/R-D7-11），数据范围恒为 Principal.UserID。
//   - 写动作登记字段白名单，未知字段由 RBAC 字段层直接 403（绝不静默丢弃）。
//   - 发布/下线支持可选 Idempotency-Key；发布副作用 best-effort（R-D7-09）。
//   - 管理写操作用管理员维度 10/min 固定窗口限流（与设置写端点的兜底口径
//     一致：设置写没有可复用的限流中间件，见 handlers_performance.go 注释）。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/markup"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

const (
	// announcementAnnounceChannel 是发布时广播的 Redis 频道（R-D7-09 冻结）。
	announcementAnnounceChannel = "newtg:announcements.changed"
	// announcementWebhookEvent 是 Webhook 事件名（payload 形状见 R-D7-09）。
	announcementWebhookEvent = "announcement"

	// announcementDefaultPageSize / max 复用全库 offset 分页口径。
	announcementDefaultPageSize = 20
	announcementMaxPageSize     = 100
	announcementMaxPage         = 1_000_000

	// 管理写限流：10/min/admin（与性能管理/设置写端点同口径）。
	announcementWriteRateLimit  = 10
	announcementWriteRateWindow = time.Minute
	announcementRateMark        = "AN"

	// announcementMaxBodyBytes 是请求体上限：正文 16KiB + JSON 转义最坏
	// 情况（每个字节变 6 字符）留足余量，避免合法正文被 MaxBytesReader 拒绝。
	announcementMaxBodyBytes = 256 << 10

	// announcementSideEffectTimeout 是 goroutine 内副作用的硬上限：Webhook
	// 自身 10s 超时 + Redis 往返。请求已返回，这个上限只防止 goroutine 泄漏。
	announcementSideEffectTimeout = 15 * time.Second
)

// RFC 7807 type URI（管理面用可机读的公告域 slug，与渠道/订阅端点同构）。
const (
	problemTypeAnnouncementNotFound      = "https://newtg.dev/problems/announcement_not_found"
	problemTypeAnnouncementStateConflict = "https://newtg.dev/problems/announcement_state_conflict"
)

// 公告域动作（字段策略按动作登记；与权限键不同）。
var (
	announcementListAction     = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "list"}
	announcementReadAction     = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "read"}
	announcementCreateAction   = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "create"}
	announcementUpdateAction   = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "update"}
	announcementDeleteAction   = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "delete"}
	announcementPublishAction  = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "publish"}
	announcementOfflineAction  = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "offline"}
	announcementActiveAction   = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "active"}
	announcementMarkReadAction = rbac.AuthzAction{ResourceKind: "announcement", ActionName: "mark_read"}
)

// AnnouncementWebhookSender 是发布副作用的发送接缝（生产 *notify.WebhookSender）。
//
// 只要求 Send：未配置/未启用/未订阅都通过返回的 notify 哨兵错误表达，
// handler 据此静默跳过，不需要额外的 Configured 探测（WebhookTester 的
// 口径是「测试端点要验证未启用配置」，与业务事件不同）。
type AnnouncementWebhookSender interface {
	Send(ctx context.Context, payload []byte, event, eventID string) (notify.WebhookResult, error)
}

// AnnouncementInAppNotifier 是 A5 站内通知投递接缝（生产 *notify.Service）。
//
// 只暴露公告需要的单个方法：handler 不应知道模板注册表/存储细节。
// 返回的计数用于发布审计的 delivered_users（不含正文）。
type AnnouncementInAppNotifier interface {
	DeliverAnnouncementInApp(ctx context.Context, in notify.AnnouncementDeliveryInput) (notify.AnnouncementDeliveryResult, error)
}

// AnnouncementHandlerConfig 是公告端点的装配输入。
type AnnouncementHandlerConfig struct {
	Repo  store.AnnouncementRepo
	Authz *rbac.AuthzMiddleware
	Audit *audit.Logger
	// Idem 是幂等存储；nil 时退化为进程内实现（单副本）。
	Idem   IdempotencyStore
	Redis  *redis.Client
	Logger *slog.Logger
	// Webhook 为 nil 时发布副作用只做 Redis 广播。
	Webhook AnnouncementWebhookSender
	// Notifier 为 nil 时发布不生成站内通知（纯内存/测试装配）。
	// 生产由 notify.Service 提供（`system.announcement` 模板，A5）。
	Notifier AnnouncementInAppNotifier
	// BaseURL 返回站点对外基址（site.server_address）。缺省/空时 webhook
	// payload 的 url 使用站内相对路径（R-D7-09）。
	BaseURL func() string
	// Now 是时间接缝（测试用）；nil 时用 time.Now。
	Now func() time.Time
	// WaitForSideEffects 仅供测试：为 true 时 handler 等到副作用 goroutine
	// 结束再返回，让「未配置静默/配置时 payload」可确定性断言。生产必须为
	// false（副作用不得阻塞发布响应）。
	WaitForSideEffects bool
}

// AnnouncementHandlers 持有公告端点依赖，可并发使用。
type AnnouncementHandlers struct {
	repo     store.AnnouncementRepo
	authz    *rbac.AuthzMiddleware
	audit    *audit.Logger
	idem     IdempotencyStore
	rdb      *redis.Client
	log      *slog.Logger
	webhook  AnnouncementWebhookSender
	notifier AnnouncementInAppNotifier
	baseURL  func() string
	now      func() time.Time
	waitSefe bool
}

// NewAnnouncementHandlers 构造公告端点。
func NewAnnouncementHandlers(cfg AnnouncementHandlerConfig) (*AnnouncementHandlers, error) {
	if cfg.Repo == nil {
		return nil, errors.New("httpapi: AnnouncementHandlerConfig.Repo is required")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	idem := cfg.Idem
	if idem == nil {
		idem = NewMemoryIdempotencyStore()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	baseURL := cfg.BaseURL
	if baseURL == nil {
		baseURL = func() string { return "" }
	}
	h := &AnnouncementHandlers{
		repo: cfg.Repo, authz: authz, audit: cfg.Audit, idem: idem,
		rdb: cfg.Redis, log: logger, webhook: cfg.Webhook,
		notifier: cfg.Notifier,
		baseURL:  baseURL, now: now, waitSefe: cfg.WaitForSideEffects,
	}
	h.registerPolicies()
	return h, nil
}

// registerPolicies 登记写动作字段白名单。未登记动作对任何写请求都 403
// （security-02 §2.6.1 规则 6）。
func (h *AnnouncementHandlers) registerPolicies() {
	payload := func() *rbac.FieldPolicy {
		return &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{
			"title":       nil,
			"category":    nil,
			"content":     nil,
			"publish_at":  nil,
			"expire_at":   nil,
			"publish_now": nil,
		}}
	}
	h.authz.SetWritePolicy(announcementCreateAction, payload())
	h.authz.SetWritePolicy(announcementUpdateAction, payload())
	// 无请求体的写动作登记空白名单：任何携带字段的 body 都 403，
	// 无 body 的请求照常通过（与订阅/令牌同名动作同构）。
	empty := &rbac.FieldPolicy{Fields: map[string]*rbac.FieldPolicy{}}
	h.authz.SetWritePolicy(announcementDeleteAction, empty)
	h.authz.SetWritePolicy(announcementPublishAction, empty)
	h.authz.SetWritePolicy(announcementOfflineAction, empty)
	h.authz.SetWritePolicy(announcementMarkReadAction, empty)
}

// Register 把公告端点挂到受保护子树。
func (h *AnnouncementHandlers) Register(mux *http.ServeMux) {
	admin := func(action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler(rbac.PermSettingsManage, action)(next)
	}
	self := func(action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler("", action)(next)
	}

	mux.Handle("GET /api/announcements", admin(announcementListAction, h.handleList))
	mux.Handle("POST /api/announcements", admin(announcementCreateAction, h.handleCreate))
	// 用户端点必须先于 {id} 注册（ServeMux 虽按特异性择路，显式顺序让
	// 读者一眼看出 active 是字面量、不是公告 id）。
	mux.Handle("GET /api/announcements/active", self(announcementActiveAction, h.handleActive))
	mux.Handle("GET /api/announcements/{id}", admin(announcementReadAction, h.handleGet))
	mux.Handle("PUT /api/announcements/{id}", admin(announcementUpdateAction, h.handleUpdate))
	mux.Handle("DELETE /api/announcements/{id}", admin(announcementDeleteAction, h.handleDelete))
	mux.Handle("POST /api/announcements/{id}/publish",
		admin(announcementPublishAction, h.idempotent(h.handlePublish)))
	mux.Handle("POST /api/announcements/{id}/offline",
		admin(announcementOfflineAction, h.idempotent(h.handleOffline)))
	mux.Handle("POST /api/announcements/{id}/read",
		self(announcementMarkReadAction, h.handleMarkRead))
}

// ---------------------------------------------------------------------------
// 响应 DTO
// ---------------------------------------------------------------------------

// announcementSummaryView 是列表行投影：**不含正文**——列表页不展示正文，
// 20 行 × 16KiB 会让一次翻页达到数百 KB，且正文只在编辑抽屉里需要。
type announcementSummaryView struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Category      string     `json:"category"`
	Status        string     `json:"status"`
	DisplayStatus string     `json:"display_status"`
	PublishAt     time.Time  `json:"publish_at"`
	ExpireAt      *time.Time `json:"expire_at"`
	PublishedAt   *time.Time `json:"published_at"`
	OfflineAt     *time.Time `json:"offline_at"`
	CreatedBy     int64      `json:"created_by"`
	UpdatedBy     int64      `json:"updated_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// announcementDetailView 额外带正文原文与净化后 HTML（R-D7-04）。
type announcementDetailView struct {
	announcementSummaryView
	ContentMarkdown string `json:"content_markdown"`
	ContentHTML     string `json:"content_html"`
}

// announcementActiveView 是用户侧投影：只暴露展示所需字段，不含 created_by/
// updated_by/offline_at 等管理面事实（用户不需要也无法据此做任何操作）。
type announcementActiveView struct {
	ID              string     `json:"id"`
	Title           string     `json:"title"`
	Category        string     `json:"category"`
	PublishAt       time.Time  `json:"publish_at"`
	ExpireAt        *time.Time `json:"expire_at"`
	ContentMarkdown string     `json:"content_markdown"`
	ContentHTML     string     `json:"content_html"`
	Read            bool       `json:"read"`
}

type announcementListView struct {
	Items    []announcementSummaryView `json:"items"`
	Total    int64                     `json:"total"`
	Page     int                       `json:"page"`
	PageSize int                       `json:"page_size"`
}

type announcementActiveListView struct {
	Items []announcementActiveView `json:"items"`
}

// displayStatus 把 DB 状态映射为 UI 徽章状态：expired 是**虚拟态**
// （R-D7-10），只在读取时派生，绝不写回 DB。
func displayStatus(a store.Announcement, now time.Time) string {
	if a.Status == store.AnnouncementStatusPublished &&
		a.ExpireAt != nil && !a.ExpireAt.After(now) {
		return store.AnnouncementDisplayExpired
	}
	return a.Status
}

func newAnnouncementSummary(a store.Announcement, now time.Time) announcementSummaryView {
	return announcementSummaryView{
		ID: a.ID, Title: a.Title, Category: a.Category, Status: a.Status,
		DisplayStatus: displayStatus(a, now), PublishAt: a.PublishAt,
		ExpireAt: a.ExpireAt, PublishedAt: a.PublishedAt, OfflineAt: a.OfflineAt,
		CreatedBy: a.CreatedBy, UpdatedBy: a.UpdatedBy,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

// renderAnnouncementHTML 渲染正文；失败时降级纯文本（设计验收要求
// 「解析失败降级纯文本展示」）。渲染失败不改变 HTTP 状态码：一条历史
// 脏数据不应让整个列表 500。
func (h *AnnouncementHandlers) renderAnnouncementHTML(content string) string {
	html, err := markup.MarkdownToHTML(content)
	if err != nil {
		h.log.Warn("announcements: markdown render failed; falling back to plain text", "err", err)
		return markup.PlainTextHTML(content)
	}
	return html
}

func (h *AnnouncementHandlers) newDetail(a store.Announcement) announcementDetailView {
	return announcementDetailView{
		announcementSummaryView: newAnnouncementSummary(a, h.now()),
		ContentMarkdown:         a.Content,
		ContentHTML:             h.renderAnnouncementHTML(a.Content),
	}
}

// ---------------------------------------------------------------------------
// 请求解析与校验
// ---------------------------------------------------------------------------

// announcementPayload 用指针区分「未提交」与显式零值：
//   - publish_at 省略 = 创建取 now() / 编辑保留原值；
//   - expire_at 省略或 null = 永久有效；
//   - publish_now 省略 = false（保存草稿）。
type announcementPayload struct {
	Title      string     `json:"title"`
	Category   string     `json:"category"`
	Content    string     `json:"content"`
	PublishAt  *time.Time `json:"publish_at"`
	ExpireAt   *time.Time `json:"expire_at"`
	PublishNow *bool      `json:"publish_now"`
}

// decodeAnnouncementPayload 读取并解析请求体。返回 false 时已写出 400。
func decodeAnnouncementPayload(w http.ResponseWriter, r *http.Request) (announcementPayload, bool) {
	var p announcementPayload
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, announcementMaxBodyBytes))
	if err != nil {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "unable to read request body")
		return p, false
	}
	if err := json.Unmarshal(body, &p); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "invalid JSON body")
		return p, false
	}
	return p, true
}

// validateAnnouncementPayload 做写库前的字段校验，返回字段错误列表。
// 与 store 的 normalize 同口径：应用层给出可读的 422 + fields，DB CHECK
// 只是最后兜底。
func (h *AnnouncementHandlers) validateAnnouncementPayload(p announcementPayload) (store.AnnouncementInput, []string) {
	in := store.AnnouncementInput{
		Title:      strings.TrimSpace(p.Title),
		Category:   strings.TrimSpace(p.Category),
		Content:    p.Content,
		ExpireAt:   p.ExpireAt,
		PublishNow: p.PublishNow != nil && *p.PublishNow,
	}
	if p.PublishAt != nil {
		in.PublishAt = p.PublishAt.UTC()
	}
	var fields []string
	if n := utf8.RuneCountInString(in.Title); n < 1 || n > store.AnnouncementMaxTitleRunes {
		fields = append(fields, "title")
	}
	if in.Category == "" {
		in.Category = store.AnnouncementCategoryNotice
	}
	switch in.Category {
	case store.AnnouncementCategoryNotice, store.AnnouncementCategoryMaintenance,
		store.AnnouncementCategoryRelease:
	default:
		fields = append(fields, "category")
	}
	if len(in.Content) == 0 || len(in.Content) > store.AnnouncementMaxContentBytes {
		fields = append(fields, "content")
	} else if _, err := markup.MarkdownToHTML(in.Content); err != nil {
		// 渲染器唯一会拒绝的输入是非法 UTF-8；写库前就拒绝，避免把
		// 「读接口永远降级纯文本」的半坏行存进库。
		fields = append(fields, "content")
	}
	// 两侧都显式提交时才能在这里判时间窗；publish_at 省略时其值由 store
	// 决定（创建取 now、编辑保留原值），store 的 ErrAnnouncementInvalid
	// 仍是最后兜底。字段名必须回给前端，才能高亮 expire_at 输入框（L11）。
	if p.ExpireAt != nil && p.PublishAt != nil && !p.ExpireAt.After(*p.PublishAt) {
		fields = append(fields, "expire_at")
	}
	return in, fields
}

// writeValidationProblem 输出带 fields 的 422（管理面既有约定）。
func writeValidationProblem(w http.ResponseWriter, fields []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	problem := map[string]any{
		"type":   problemTypeValidationFailed,
		"title":  "Validation failed",
		"status": http.StatusUnprocessableEntity,
		"fields": fields,
	}
	if raw, err := json.Marshal(problem); err == nil {
		_, _ = w.Write(raw)
	}
}

// writeAnnouncementError 把存储/状态机错误映射为 RFC 7807。
func (h *AnnouncementHandlers) writeAnnouncementError(w http.ResponseWriter, err error, route string) {
	switch {
	case errors.Is(err, store.ErrAnnouncementNotFound):
		writeProblemDetail(w, http.StatusNotFound, problemTypeAnnouncementNotFound,
			"Announcement not found", "")
	case errors.Is(err, store.ErrAnnouncementStateConflict):
		writeProblemDetail(w, http.StatusConflict, problemTypeAnnouncementStateConflict,
			"Announcement state conflict", "the requested state transition is not allowed")
	case errors.Is(err, store.ErrAnnouncementInvalid):
		writeProblemDetail(w, http.StatusUnprocessableEntity, problemTypeValidationFailed,
			"Validation failed", "the announcement violates a data constraint")
	default:
		h.log.Error("announcements: request failed", "route", route, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
	}
}

// allowAdminWrite 消耗管理员维度的固定窗口名额（10/min/admin）。
// Redis 不可用时 fail-open：与其它管理面一致，限流不是授权边界。
func (h *AnnouncementHandlers) allowAdminWrite(w http.ResponseWriter, r *http.Request) bool {
	if h.rdb == nil {
		return true
	}
	key := RateLimitKeyPrefix + announcementRateMark + ":admin:" + strconv.FormatInt(actorID(r), 10)
	limited, retry, err := allowRequestRate(r.Context(), h.rdb, key,
		announcementWriteRateLimit, announcementWriteRateWindow)
	if err != nil {
		h.log.Warn("announcements: rate limiter unavailable; allowing operation", "err", err)
		return true
	}
	if !limited {
		return true
	}
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds()+0.999)))
	}
	writeProblemDetail(w, http.StatusTooManyRequests, authProblemTooManyRequests,
		"Too many requests", "announcement writes are limited to 10 requests per minute")
	return false
}

// ---------------------------------------------------------------------------
// 管理面 handlers
// ---------------------------------------------------------------------------

func (h *AnnouncementHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.AnnouncementFilter{
		Status:   strings.TrimSpace(query.Get("status")),
		Category: strings.TrimSpace(query.Get("category")),
		Page:     1,
		PageSize: announcementDefaultPageSize,
	}
	switch filter.Status {
	case "", store.AnnouncementStatusDraft, store.AnnouncementStatusPublished,
		store.AnnouncementStatusOffline, store.AnnouncementDisplayExpired:
	default:
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "unknown status filter")
		return
	}
	// category 与 status 同口径：非法枚举值在 handler 预校验为 400，
	// 不把自由文本透传给仓储（否则会静默返回空列表，用户以为没有数据）。
	switch filter.Category {
	case "", store.AnnouncementCategoryNotice, store.AnnouncementCategoryMaintenance,
		store.AnnouncementCategoryRelease:
	default:
		writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
			"Invalid request", "unknown category filter")
		return
	}
	if raw := strings.TrimSpace(query.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > announcementMaxPage {
			writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
				"Invalid request", "p is out of range")
			return
		}
		filter.Page = value
	}
	if raw := strings.TrimSpace(query.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > announcementMaxPageSize {
			writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
				"Invalid request", "page_size is out of range")
			return
		}
		filter.PageSize = value
	}
	items, total, err := h.repo.List(r.Context(), filter)
	if err != nil {
		h.writeAnnouncementError(w, err, "GET /api/announcements")
		return
	}
	now := h.now()
	views := make([]announcementSummaryView, 0, len(items))
	for _, item := range items {
		views = append(views, newAnnouncementSummary(item, now))
	}
	writeJSON(w, http.StatusOK, announcementListView{
		Items: views, Total: total, Page: filter.Page, PageSize: filter.PageSize,
	})
}

func (h *AnnouncementHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.allowAdminWrite(w, r) {
		return
	}
	payload, ok := decodeAnnouncementPayload(w, r)
	if !ok {
		return
	}
	in, fields := h.validateAnnouncementPayload(payload)
	if len(fields) > 0 {
		writeValidationProblem(w, fields)
		return
	}
	actor := actorID(r)
	mut, err := h.repo.Create(r.Context(), in, actor)
	if err != nil {
		h.writeAnnouncementError(w, err, "POST /api/announcements")
		return
	}
	h.writeAudit(r, audit.ActionAnnouncementCreate, mut.Announcement, nil)
	if mut.Published {
		result := h.deliverAnnouncementInApp(r.Context(), mut.Announcement)
		h.writePublishAudit(r, mut.Announcement, result)
		h.afterPublish(r.Context(), mut.Announcement)
	}
	writeJSON(w, http.StatusCreated, h.newDetail(mut.Announcement))
}

func (h *AnnouncementHandlers) handleGet(w http.ResponseWriter, r *http.Request) {
	a, err := h.repo.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeAnnouncementError(w, err, "GET /api/announcements/{id}")
		return
	}
	writeJSON(w, http.StatusOK, h.newDetail(a))
}

func (h *AnnouncementHandlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.allowAdminWrite(w, r) {
		return
	}
	payload, ok := decodeAnnouncementPayload(w, r)
	if !ok {
		return
	}
	in, fields := h.validateAnnouncementPayload(payload)
	if len(fields) > 0 {
		writeValidationProblem(w, fields)
		return
	}
	mut, err := h.repo.Update(r.Context(), r.PathValue("id"), in, actorID(r))
	if err != nil {
		h.writeAnnouncementError(w, err, "PUT /api/announcements/{id}")
		return
	}
	h.writeAudit(r, audit.ActionAnnouncementUpdate, mut.Announcement, nil)
	if mut.Published {
		result := h.deliverAnnouncementInApp(r.Context(), mut.Announcement)
		h.writePublishAudit(r, mut.Announcement, result)
		h.afterPublish(r.Context(), mut.Announcement)
	}
	writeJSON(w, http.StatusOK, h.newDetail(mut.Announcement))
}

func (h *AnnouncementHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !h.allowAdminWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	// 先读一次只为审计 detail（删除后无法再取 title/status）。读失败直接
	// 返回：删除一个不存在的公告本来就应该 404，不必进入删除事务。
	existing, err := h.repo.Get(r.Context(), id)
	if err != nil {
		h.writeAnnouncementError(w, err, "DELETE /api/announcements/{id}")
		return
	}
	if err := h.repo.Delete(r.Context(), id); err != nil {
		h.writeAnnouncementError(w, err, "DELETE /api/announcements/{id}")
		return
	}
	h.writeAudit(r, audit.ActionAnnouncementDelete, existing, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *AnnouncementHandlers) handlePublish(w http.ResponseWriter, r *http.Request) {
	if !h.allowAdminWrite(w, r) {
		return
	}
	mut, err := h.repo.Publish(r.Context(), r.PathValue("id"), actorID(r))
	if err != nil {
		h.writeAnnouncementError(w, err, "POST /api/announcements/{id}/publish")
		return
	}
	if mut.Published {
		// A5：站内投递先同步完成，否则发布审计拿不到投递计数。
		// 投递本身是本地 DB 批量写，失败只 Warn（不回滚发布）。
		result := h.deliverAnnouncementInApp(r.Context(), mut.Announcement)
		h.writePublishAudit(r, mut.Announcement, result)
		// 幂等重放（Published=false）不重复广播：用户侧会把同一公告再弹一次。
		h.afterPublish(r.Context(), mut.Announcement)
	} else {
		h.writeAudit(r, audit.ActionAnnouncementPublish, mut.Announcement, nil)
	}
	writeJSON(w, http.StatusOK, h.newDetail(mut.Announcement))
}

func (h *AnnouncementHandlers) handleOffline(w http.ResponseWriter, r *http.Request) {
	if !h.allowAdminWrite(w, r) {
		return
	}
	mut, err := h.repo.Offline(r.Context(), r.PathValue("id"), actorID(r))
	if err != nil {
		h.writeAnnouncementError(w, err, "POST /api/announcements/{id}/offline")
		return
	}
	h.writeAudit(r, audit.ActionAnnouncementOffline, mut.Announcement, nil)
	writeJSON(w, http.StatusOK, h.newDetail(mut.Announcement))
}

// ---------------------------------------------------------------------------
// 用户侧 handlers
// ---------------------------------------------------------------------------

func (h *AnnouncementHandlers) handleActive(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal == nil || principal.UserID == 0 {
		writeUnauthorized(w)
		return
	}
	items, err := h.repo.ListActive(r.Context(), principal.UserID, h.now())
	if err != nil {
		h.writeAnnouncementError(w, err, "GET /api/announcements/active")
		return
	}
	views := make([]announcementActiveView, 0, len(items))
	for _, item := range items {
		views = append(views, announcementActiveView{
			ID: item.ID, Title: item.Title, Category: item.Category,
			PublishAt: item.PublishAt, ExpireAt: item.ExpireAt,
			ContentMarkdown: item.Content,
			ContentHTML:     h.renderAnnouncementHTML(item.Content),
			Read:            item.Read,
		})
	}
	writeJSON(w, http.StatusOK, announcementActiveListView{Items: views})
}

func (h *AnnouncementHandlers) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal == nil || principal.UserID == 0 {
		writeUnauthorized(w)
		return
	}
	// read 是幂等 upsert：重复调用返回 204，不区分「首次」与「重复」。
	// 非生效公告返回 404（不透露「存在但不可读」，见 store.MarkRead 注释）。
	if err := h.repo.MarkRead(r.Context(), principal.UserID,
		r.PathValue("id"), h.now()); err != nil {
		h.writeAnnouncementError(w, err, "POST /api/announcements/{id}/read")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// 审计与发布副作用
// ---------------------------------------------------------------------------

// writeAudit 写公告审计。detail 只允许 id/title/status（R-D7-08）：
// 正文可能含未公开内容，审计表不得成为它的第二副本。extra 只用于发布事件
// 的投递计数（A5：delivered_users / delivery_failed），调用方不得借此塞入
// 正文/收件人明细。
func (h *AnnouncementHandlers) writeAudit(r *http.Request, action string, a store.Announcement, extra map[string]any) {
	if h.audit == nil {
		return
	}
	detailMap := map[string]any{
		"id":     a.ID,
		"title":  a.Title,
		"status": a.Status,
	}
	for key, value := range extra {
		detailMap[key] = value
	}
	detail, err := json.Marshal(detailMap)
	if err != nil {
		return
	}
	event := audit.Event{
		Action:     action,
		Actor:      actorID(r),
		Method:     r.Method,
		Route:      r.Pattern,
		StatusCode: auditSuccessStatus(action),
		IP:         clientIP(r),
		UserAgent:  r.UserAgent(),
		Detail:     detail,
	}
	_ = writeAuditEvent(r.Context(), h.audit, event)
}

// writePublishAudit 写发布审计并带站内投递计数（A5）。
//
// delivery_failed 与 delivered_users 成对出现：部分批次的 INSERT 失败（或
// 进程在投递中途崩溃）不会被自动重试，运维需要从审计里一眼看出「有多少
// 启用用户没收到」。计数只是可观测事实，不影响发布响应/事务。
func (h *AnnouncementHandlers) writePublishAudit(
	r *http.Request, a store.Announcement, result notify.AnnouncementDeliveryResult,
) {
	h.writeAudit(r, audit.ActionAnnouncementPublish, a, map[string]any{
		"delivered_users": result.Delivered,
		"delivery_failed": result.Failed,
	})
}

// auditSuccessStatus 在响应写出前无法拿到真实状态码；公告写动作成功时
// 创建是 201、删除是 204、其余 200。审计只需要「成功」这一事实，这里给出
// 与端点契约一致的状态码。
func auditSuccessStatus(action string) int {
	switch action {
	case audit.ActionAnnouncementCreate:
		return http.StatusCreated
	case audit.ActionAnnouncementDelete:
		return http.StatusNoContent
	default:
		return http.StatusOK
	}
}

// deliverAnnouncementInApp 为首次发布生成站内通知（A5），返回投递计数。
//
// 同步执行的理由：发布审计的 detail 必须带 delivered_users/delivery_failed，
// 而审计在响应前写出；投递本身只是本地 DB 批量插入（每批 ≤500）。它仍被
// 定义成 best-effort——任何失败只 Warn 并返回已完成/失败的计数，绝不改变
// 「发布成功」。注意：部分批次失败或进程崩溃后**不会自动恢复**；运维的
// 手工恢复路径是「下线 → 再发布」，重新发布按 dedupe_key 只为缺失用户
// 补齐（已投递的计为 duplicates），见 G-DOM-37 ⑤。
// ctx 用 WithoutCancel：客户端断开不应让半批投递留下不可见的中间态；
// 15s 上限与其它发布副作用一致，防止慢库拖住 handler。
func (h *AnnouncementHandlers) deliverAnnouncementInApp(ctx context.Context, a store.Announcement) notify.AnnouncementDeliveryResult {
	if h.notifier == nil {
		return notify.AnnouncementDeliveryResult{}
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), announcementSideEffectTimeout)
	defer cancel()

	publishedAt := h.now()
	if a.PublishedAt != nil {
		publishedAt = *a.PublishedAt
	}
	result, err := h.notifier.DeliverAnnouncementInApp(runCtx, notify.AnnouncementDeliveryInput{
		AnnouncementID: a.ID,
		Title:          a.Title,
		Summary:        announcementNotificationSummary(a.Content),
		LinkURL:        h.announcementURL(),
		PublishedAt:    publishedAt,
	})
	if err != nil {
		h.log.Warn("announcements: inapp notification delivery failed",
			"announcement_id", a.ID, "err", err)
		return result
	}
	if result.Failed > 0 {
		h.log.Warn("announcements: inapp notification delivery partially failed",
			"announcement_id", a.ID, "delivered", result.Delivered,
			"duplicates", result.Duplicates, "failed", result.Failed)
	}
	return result
}

// announcementNotificationSummary 从公告正文派生站内通知摘要。
//
// 公告表没有 summary 列（modules-08 F-08-04 数据字段表），而
// `system.announcement` 模板要求 {{.summary}}。收敛顺序是有意的：
//  1. 先剥链接/图片语法（`[文字](url)` → 文字、`![alt](src)` → alt），
//     否则后面的行首/标记剥离会把 URL 的括号残片留在摘要里；
//  2. 再剥**行首**标记（标题 #、引用 >、列表 -/*/1.），只认行首；
//  3. 最后去强调/代码标记并压空白，截断 120 个字符。
//
// 连字符 `-` 只在「行首列表标记 + 空格」形态被移除，正文内的日期
// （2026-10-20）与版本号（v1-2）保持原样——旧实现无条件删 `-` 会把
// 日期粘成 20261020。
func announcementNotificationSummary(content string) string {
	text := announcementMarkdownImage.ReplaceAllString(content, "$1")
	text = announcementMarkdownLink.ReplaceAllString(text, "$1")
	text = announcementMarkdownLinePrefix.ReplaceAllString(text, "")
	text = announcementMarkdownInlineMarker.ReplaceAllString(text, "")

	words := strings.Fields(text)
	joined := strings.Join(words, " ")
	runes := []rune(joined)
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return strings.TrimSpace(string(runes))
}

var (
	// 图片必须先于链接处理（图片语法包含链接语法）。
	announcementMarkdownImage      = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	announcementMarkdownLink       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	announcementMarkdownLinePrefix = regexp.MustCompile(`(?m)^[ \t]*(?:#{1,6}[ \t]+|>[ \t]?|[-+*][ \t]+|[0-9]+[.)][ \t]+)`)
	// 剩余的行内标记：强调、行内代码、分隔线字符与未成对的方括号/圆括号。
	announcementMarkdownInlineMarker = regexp.MustCompile("[`*_~\\[\\]()|]")
)

// afterPublish 触发发布副作用（R-D7-09）：best-effort，不阻塞、不失败发布。
//
// 用 goroutine 而不是同步调用：Webhook 单次超时 10s，同步会把一次普通发布
// 请求拖到 10s；而副作用失败（端点宕机/未配置/Redis 抖动）都不应改变
// 「发布成功」这个事实。ctx 用 WithoutCancel：handler 返回后请求上下文会被
// 取消，副作用仍应走完（与审计写入同一理由）。
func (h *AnnouncementHandlers) afterPublish(ctx context.Context, a store.Announcement) {
	base := context.WithoutCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCtx, cancel := context.WithTimeout(base, announcementSideEffectTimeout)
		defer cancel()
		h.publishSideEffects(runCtx, a)
	}()
	if h.waitSefe {
		<-done
	}
}

// publishSideEffects 依次执行 Redis 广播与 Webhook 投递。
func (h *AnnouncementHandlers) publishSideEffects(ctx context.Context, a store.Announcement) {
	if h.rdb != nil {
		if err := h.rdb.Publish(ctx, announcementAnnounceChannel, a.ID).Err(); err != nil {
			// 未配置/连接失败都只记日志：发布本身已成功。
			h.log.Warn("announcements: publish redis notification failed",
				"announcement_id", a.ID, "err", err)
		}
	}
	if h.webhook == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"announcement_id": a.ID,
		"title":           a.Title,
		"category":        a.Category,
		"published_at":    a.PublishedAt,
		"url":             h.announcementURL(),
	})
	if err != nil {
		h.log.Warn("announcements: marshal webhook payload failed",
			"announcement_id", a.ID, "err", err)
		return
	}
	if _, err := h.webhook.Send(ctx, payload, announcementWebhookEvent, a.ID); err != nil {
		// 未配置（ErrWebhookDisabled/ErrWebhookNotConfigured/未订阅）是正常
		// 部署状态，静默跳过；其余失败也只是一行日志。
		if errors.Is(err, notify.ErrWebhookDisabled) ||
			errors.Is(err, notify.ErrWebhookNotConfigured) ||
			errors.Is(err, notify.ErrEventNotSubscribed) {
			return
		}
		h.log.Warn("announcements: webhook delivery failed",
			"announcement_id", a.ID, "err", err)
	}
}

// announcementURL 组装 webhook payload 的 url：当前指向站点根 `/`。
//
// 公告详情页由 D7b 实现，在那之前 `/announcements/{id}` 会 404；按 R-D7-09
// 「若前端无该页则用 `/`」的裁决先回根路径。**待 D7b 落地后恢复**
// `base + "/announcements/" + id`（id 参数随之加回）。基址未配置时使用
// 站内相对路径（R-D7-09 允许的缺省）；base 末尾的 `/` 先剥掉，避免出现
// `https://gw//`。
func (h *AnnouncementHandlers) announcementURL() string {
	base := strings.TrimRight(strings.TrimSpace(h.baseURL()), "/")
	return base + "/"
}

// ---------------------------------------------------------------------------
// 幂等（发布/下线）
// ---------------------------------------------------------------------------

// idempotent 与设置端点同构：Claim → 执行 → Complete/Release。
// 作用域键绑定「用户 + 方法 + 完整 URI + 请求指纹」：同一个 Idempotency-Key
// 被不同公告/不同动作复用时不会被错误重放。
func (h *AnnouncementHandlers) idempotent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if headerKey == "" {
			next(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, announcementMaxBodyBytes))
		if err != nil {
			writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
				"Invalid request", "unable to read request body")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		fingerprint := settingsFingerprint(body)
		key := "announcement:" + strconv.FormatInt(actorID(r), 10) + ":" +
			r.Method + " " + r.URL.RequestURI() + ":" + fingerprint + ":" + headerKey

		claim, err := h.idem.Claim(r.Context(), key, IdempotencyPendingTTL)
		if err != nil {
			// fail-open：幂等存储不可用不阻断管理写入（与渠道/设置端点同口径）。
			next(w, r)
			return
		}
		switch claim.Status {
		case IdempotencyClaimReplay:
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSONBytes(w, claim.Response.Status, claim.Response.Body)
			return
		case IdempotencyClaimInFlight:
			w.Header().Set("Retry-After", "1")
			writeProblemDetail(w, http.StatusConflict, problemTypeIdempotencyInProgress,
				"Idempotency key is in progress",
				"another request with this Idempotency-Key is still being processed; retry shortly")
			return
		}

		recorder := &bufferedResponseWriter{status: http.StatusOK}
		next(recorder, r)

		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), idempotencyFinalizeTimeout)
		defer cancel()
		if recorder.status >= http.StatusOK && recorder.status < http.StatusMultipleChoices {
			if err := h.idem.Complete(ctx, key, claim.Token,
				IdempotentResponse{Status: recorder.status, Body: recorder.body}, IdempotencyTTL); err != nil {
				h.log.Error("httpapi: announcement idempotency complete failed", "err", err)
			}
		} else {
			if err := h.idem.Release(ctx, key, claim.Token); err != nil {
				h.log.Error("httpapi: announcement idempotency release failed", "err", err)
			}
		}
		recorder.flush(w)
	}
}

// bufferedResponseWriter 缓冲下游响应，供幂等 Complete 记录后一次性写出。
type bufferedResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *bufferedResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) { w.status = status }

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	w.body = append(w.body, data...)
	return len(data), nil
}

func (w *bufferedResponseWriter) flush(dst http.ResponseWriter) {
	for key, values := range w.header {
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	contentType := "application/json"
	if w.status >= 400 {
		contentType = "application/problem+json"
	}
	if dst.Header().Get("Content-Type") == "" {
		dst.Header().Set("Content-Type", contentType)
	}
	dst.WriteHeader(w.status)
	_, _ = dst.Write(w.body)
}
