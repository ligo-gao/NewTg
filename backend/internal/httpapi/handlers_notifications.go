// 站内通知读取端点（A5；modules-08 §3.9.1 `system.announcement`）。
//
// 端点面：
//
//	GET  /api/notifications?p=&page_size=   本人站内通知（含 read_at）
//	POST /api/notifications/{id}/read       标记本人一条通知已读（幂等）
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
	MarkUserNotificationRead(ctx context.Context, userID, id int64, now time.Time) error
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
	// 两个端点都没有请求体：任何携带字段的 body 都 403（与公告的
	// delete/publish/read 无体动作同构，绝不静默丢弃未知字段）。
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
	mux.Handle("POST /api/notifications/{id}/read", self(notificationReadAction, h.handleMarkRead))
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
