// 站内通知读取端点（A5；modules-08 §3.9.1 `system.announcement`）。
//
// 端点面：
//
//	GET  /api/notifications?p=&page_size=   本人站内通知（含 read_at）
//	GET  /api/notifications/unread-count    本人未读数（与列表同口径）
//	POST /api/notifications/{id}/read       标记本人一条通知已读（幂等）
//	POST /api/notifications/read-all        本人全部可见未读一次置读（幂等）
//
// 设计约束：
//   - 数据范围恒为当前登录用户（Principal.UserID），不接受 user_id 参数；
//     他人通知与不存在统一 404，避免枚举。
//   - 读端点只消费 notification_deliveries 中 channel='inapp' 的行；
//     A5 之前 inapp 不落表，因此旧数据不会出现在这里。
//   - 未登录一律 401（与公告用户侧端点同口径）。
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
)

const (
	notificationDefaultPageSize = 20
	notificationMaxPageSize     = 100
	notificationMaxPage         = 1_000_000
)

var (
	notificationListAction = rbac.AuthzAction{ResourceKind: "notification", ActionName: "list"}
	notificationReadAction = rbac.AuthzAction{ResourceKind: "notification", ActionName: "mark_read"}
)

// problemTypeNotificationNotFound 是站内通知域的可机读 404 类型。
const problemTypeNotificationNotFound = "https://newtg.dev/problems/notification_not_found"

// UserNotificationService 是 handler 对 *notify.Service 的最小依赖面。
type UserNotificationService interface {
	ListUserNotifications(ctx context.Context, userID int64, page, pageSize int) ([]notify.UserNotificationView, int, error)
	CountUserUnreadNotifications(ctx context.Context, userID int64) (int, error)
	MarkUserNotificationRead(ctx context.Context, userID, id int64, now time.Time) error
	MarkAllUserNotificationsRead(ctx context.Context, userID int64, now time.Time) (int, error)
}

// NotificationHandlerConfig 是端点装配输入。
type NotificationHandlerConfig struct {
	Service UserNotificationService
	Authz   *rbac.AuthzMiddleware
	Logger  *slog.Logger
	// Now 是已读时刻的时钟接缝；nil 时 time.Now。
	Now func() time.Time
}

// NotificationHandlers 可并发使用。
type NotificationHandlers struct {
	service UserNotificationService
	authz   *rbac.AuthzMiddleware
	log     *slog.Logger
	now     func() time.Time
}

// NewNotificationHandlers 构造站内通知端点。
func NewNotificationHandlers(cfg NotificationHandlerConfig) (*NotificationHandlers, error) {
	if cfg.Service == nil {
		return nil, errors.New("httpapi: NotificationHandlerConfig.Service is required")
	}
	authz := cfg.Authz
	if authz == nil {
		authz = rbac.NewAuthzMiddleware()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	h := &NotificationHandlers{service: cfg.Service, authz: authz, log: logger, now: now}
	// 写端点（单条已读 / 全部已读）都没有请求体：任何携带字段的 body 都
	// 403（与公告的 delete/publish/read 无体动作同构，绝不静默丢弃未知字段）。
	// 两个写端点语义相同，共用同一 FieldPolicy 登记键，避免出现一处登记、
	// 另一处因缺少登记而失败关闭的装配缝隙。
	h.authz.SetWritePolicy(notificationReadAction, &rbac.FieldPolicy{
		Fields: map[string]*rbac.FieldPolicy{},
	})
	return h, nil
}

// Register 把站内通知端点挂到受保护子树。
func (h *NotificationHandlers) Register(mux *http.ServeMux) {
	self := func(action rbac.AuthzAction, next http.HandlerFunc) http.Handler {
		return h.authz.Handler("", action)(next)
	}
	mux.Handle("GET /api/notifications", self(notificationListAction, h.handleList))
	// 精确模式优先于上面的 /api/notifications：两个路径互不重叠，ServeMux
	// 会为更长的字面量路径选择 unread-count。
	mux.Handle("GET /api/notifications/unread-count", self(notificationListAction, h.handleUnreadCount))
	mux.Handle("POST /api/notifications/{id}/read", self(notificationReadAction, h.handleMarkRead))
	mux.Handle("POST /api/notifications/read-all", self(notificationReadAction, h.handleMarkAllRead))
}

type userNotificationListView struct {
	Items    []notify.UserNotificationView `json:"items"`
	Total    int                           `json:"total"`
	Page     int                           `json:"page"`
	PageSize int                           `json:"page_size"`
}

func (h *NotificationHandlers) handleList(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal == nil || principal.UserID == 0 {
		writeUnauthorized(w)
		return
	}
	page := 1
	pageSize := notificationDefaultPageSize
	query := r.URL.Query()
	if raw := strings.TrimSpace(query.Get("p")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > notificationMaxPage {
			writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
				"Invalid request", "p is out of range")
			return
		}
		page = value
	}
	if raw := strings.TrimSpace(query.Get("page_size")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > notificationMaxPageSize {
			writeProblemDetail(w, http.StatusBadRequest, authProblemBadRequest,
				"Invalid request", "page_size is out of range")
			return
		}
		pageSize = value
	}
	items, total, err := h.service.ListUserNotifications(r.Context(), principal.UserID, page, pageSize)
	if err != nil {
		h.log.Error("notifications: list failed", "user_id", principal.UserID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}
	if items == nil {
		items = []notify.UserNotificationView{}
	}
	writeJSON(w, http.StatusOK, userNotificationListView{
		Items: items, Total: total, Page: page, PageSize: pageSize,
	})
}

type userNotificationUnreadCountView struct {
	Unread int `json:"unread"`
}

type userNotificationReadAllView struct {
	Updated int `json:"updated"`
}

// handleUnreadCount 返回本人未读数。计数与列表同口径：channel='inapp'、
// 归属当前用户、公告类投递仅在公告生效时计入（见 notify 的可见性条件）。
func (h *NotificationHandlers) handleUnreadCount(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal == nil || principal.UserID == 0 {
		writeUnauthorized(w)
		return
	}
	unread, err := h.service.CountUserUnreadNotifications(r.Context(), principal.UserID)
	if err != nil {
		h.log.Error("notifications: unread count failed", "user_id", principal.UserID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}
	writeJSON(w, http.StatusOK, userNotificationUnreadCountView{Unread: unread})
}

// handleMarkAllRead 一次性把本人全部可见未读置读。
// 幂等：重复调用 updated=0，已读时刻保持首次值；公告已失效的投递不更新。
func (h *NotificationHandlers) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal == nil || principal.UserID == 0 {
		writeUnauthorized(w)
		return
	}
	updated, err := h.service.MarkAllUserNotificationsRead(r.Context(), principal.UserID, h.now())
	if err != nil {
		h.log.Error("notifications: mark all read failed", "user_id", principal.UserID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}
	writeJSON(w, http.StatusOK, userNotificationReadAllView{Updated: updated})
}

func (h *NotificationHandlers) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok || principal == nil || principal.UserID == 0 {
		writeUnauthorized(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		// 形态非法与不存在同码：不透露 id 空间。
		writeProblemDetail(w, http.StatusNotFound, problemTypeNotificationNotFound,
			"Notification not found", "")
		return
	}
	if err := h.service.MarkUserNotificationRead(r.Context(), principal.UserID, id, h.now()); err != nil {
		if errors.Is(err, notify.ErrDeliveryNotFound) {
			writeProblemDetail(w, http.StatusNotFound, problemTypeNotificationNotFound,
				"Notification not found", "")
			return
		}
		h.log.Error("notifications: mark read failed",
			"user_id", principal.UserID, "delivery_id", id, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError, authProblemInternal,
			"Internal Server Error", "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
