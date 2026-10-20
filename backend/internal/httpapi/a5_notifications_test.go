package httpapi

// A5 验收（单元层）：
//   - 站内通知读取端点的登录门槛、分页校验、本人数据范围、空态、幂等已读
//     与 404 映射（真实 SQL 在 notify 的集成测试）；
//   - 公告发布接入站内投递：首次发布调用一次、幂等重放不再调用、投递失败
//     不影响发布响应。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

type fakeUserNotificationService struct {
	listFn func(context.Context, int64, int, int) ([]notify.UserNotificationView, int, error)
	markFn func(context.Context, int64, int64, time.Time) error
}

func (f *fakeUserNotificationService) ListUserNotifications(ctx context.Context, userID int64, page, pageSize int) ([]notify.UserNotificationView, int, error) {
	if f.listFn != nil {
		return f.listFn(ctx, userID, page, pageSize)
	}
	return nil, 0, nil
}

func (f *fakeUserNotificationService) MarkUserNotificationRead(ctx context.Context, userID, id int64, now time.Time) error {
	if f.markFn != nil {
		return f.markFn(ctx, userID, id, now)
	}
	return nil
}

func newNotificationTestHandlers(t *testing.T, service UserNotificationService) *http.ServeMux {
	t.Helper()
	h, err := NewNotificationHandlers(NotificationHandlerConfig{
		Service: service,
		Authz:   rbac.NewAuthzMiddleware(),
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("NewNotificationHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func TestUserNotificationsRequireLogin(t *testing.T) {
	mux := newNotificationTestHandlers(t, &fakeUserNotificationService{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/notifications"},
		{http.MethodPost, "/api/notifications/1/read"},
	} {
		if got := doAnnouncementRequest(t, mux, nil, tc.method, tc.path, nil); got.Code != http.StatusUnauthorized && got.Code != http.StatusForbidden {
			t.Fatalf("%s %s 未登录 = %d，want 401/403", tc.method, tc.path, got.Code)
		}
	}
}

func TestUserNotificationsListScopesToPrincipal(t *testing.T) {
	readAt := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var (
		mu          sync.Mutex
		gotUserID   int64
		gotPage     int
		gotPageSize int
	)
	mux := newNotificationTestHandlers(t, &fakeUserNotificationService{
		listFn: func(_ context.Context, userID int64, page, pageSize int) ([]notify.UserNotificationView, int, error) {
			mu.Lock()
			defer mu.Unlock()
			gotUserID, gotPage, gotPageSize = userID, page, pageSize
			return []notify.UserNotificationView{{
				ID: 3, TemplateKey: "system.announcement", Category: "system.announcement",
				Title: "hello", Summary: "world", ReadAt: &readAt, CreatedAt: readAt,
			}}, 1, nil
		},
	})
	principal := announcementTestPrincipal(42, rbac.RoleUser)
	got := doAnnouncementRequest(t, mux, principal, http.MethodGet, "/api/notifications?p=2&page_size=5", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("list = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	if gotUserID != 42 || gotPage != 2 || gotPageSize != 5 {
		t.Fatalf("服务调用 = user=%d page=%d size=%d，want 42/2/5", gotUserID, gotPage, gotPageSize)
	}
	var view userNotificationListView
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Total != 1 || len(view.Items) != 1 || view.Items[0].Title != "hello" {
		t.Fatalf("响应 = %+v", view)
	}
	if view.Items[0].ReadAt == nil || !view.Items[0].ReadAt.Equal(readAt) {
		t.Fatalf("read_at 丢失：%+v", view.Items[0])
	}
}

func TestUserNotificationsListEmptyIsArray(t *testing.T) {
	mux := newNotificationTestHandlers(t, &fakeUserNotificationService{
		listFn: func(context.Context, int64, int, int) ([]notify.UserNotificationView, int, error) {
			return nil, 0, nil
		},
	})
	got := doAnnouncementRequest(t, mux, announcementTestPrincipal(7, rbac.RoleUser),
		http.MethodGet, "/api/notifications", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("empty list = %d", got.Code)
	}
	if body := strings.TrimSpace(got.Body.String()); body != `{"items":[],"total":0,"page":1,"page_size":20}` {
		t.Fatalf("空态响应 = %s", body)
	}
}

func TestUserNotificationsRejectsBadPagination(t *testing.T) {
	mux := newNotificationTestHandlers(t, &fakeUserNotificationService{})
	principal := announcementTestPrincipal(7, rbac.RoleUser)
	for _, query := range []string{"?p=0", "?p=x", "?page_size=0", "?page_size=101"} {
		if got := doAnnouncementRequest(t, mux, principal, http.MethodGet, "/api/notifications"+query, nil); got.Code != http.StatusBadRequest {
			t.Fatalf("GET %s = %d，want 400", query, got.Code)
		}
	}
}

func TestUserNotificationMarkReadIsIdempotentAndIsolated(t *testing.T) {
	var calls int
	mux := newNotificationTestHandlers(t, &fakeUserNotificationService{
		markFn: func(_ context.Context, userID, id int64, _ time.Time) error {
			if userID != 9 {
				t.Fatalf("userID = %d，want 9", userID)
			}
			if id == 404 {
				return notify.ErrDeliveryNotFound
			}
			calls++
			return nil
		},
	})
	principal := announcementTestPrincipal(9, rbac.RoleUser)
	if got := doAnnouncementRequest(t, mux, principal, http.MethodPost, "/api/notifications/7/read", nil); got.Code != http.StatusNoContent {
		t.Fatalf("mark read = %d，want 204", got.Code)
	}
	if got := doAnnouncementRequest(t, mux, principal, http.MethodPost, "/api/notifications/7/read", nil); got.Code != http.StatusNoContent {
		t.Fatalf("重复 mark read = %d，want 204", got.Code)
	}
	if calls != 2 {
		t.Fatalf("幂等调用次数 = %d，want 2（服务层负责幂等）", calls)
	}
	if got := doAnnouncementRequest(t, mux, principal, http.MethodPost, "/api/notifications/404/read", nil); got.Code != http.StatusNotFound {
		t.Fatalf("他人/不存在通知 = %d，want 404", got.Code)
	}
	if got := doAnnouncementRequest(t, mux, principal, http.MethodPost, "/api/notifications/abc/read", nil); got.Code != http.StatusNotFound {
		t.Fatalf("非法 id = %d，want 404", got.Code)
	}
}

// fakeAnnouncementNotifier 记录发布时的投递输入。
type fakeAnnouncementNotifier struct {
	mu     sync.Mutex
	inputs []notify.AnnouncementDeliveryInput
	result notify.AnnouncementDeliveryResult
	err    error
}

func (f *fakeAnnouncementNotifier) DeliverAnnouncementInApp(_ context.Context, in notify.AnnouncementDeliveryInput) (notify.AnnouncementDeliveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	return f.result, f.err
}

func TestAnnouncementPublishDeliversInAppOnce(t *testing.T) {
	notifier := &fakeAnnouncementNotifier{result: notify.AnnouncementDeliveryResult{Delivered: 3}}
	repo := &fakeAnnouncementRepo{
		publishFn: func(_ context.Context, id string, _ int64) (store.AnnouncementMutation, error) {
			a := sampleAnnouncement()
			a.ID = id
			return store.AnnouncementMutation{Announcement: a, Published: true}, nil
		},
	}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Notifier: notifier, WaitForSideEffects: true,
	})
	got := doAnnouncementRequest(t, mux, announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage),
		http.MethodPost, "/api/announcements/"+sampleAnnouncement().ID+"/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("publish = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	if len(notifier.inputs) != 1 {
		t.Fatalf("投递调用 %d 次，want 1", len(notifier.inputs))
	}
	in := notifier.inputs[0]
	if in.Title != "Maintenance" || in.Summary != "down" {
		t.Fatalf("投递输入 = %+v（summary 应由正文派生）", in)
	}
	if in.AnnouncementID != sampleAnnouncement().ID {
		t.Fatalf("公告 id = %s", in.AnnouncementID)
	}
}

func TestAnnouncementPublishInAppFailureDoesNotBlock(t *testing.T) {
	notifier := &fakeAnnouncementNotifier{err: errors.New("db down")}
	repo := &fakeAnnouncementRepo{
		publishFn: func(_ context.Context, id string, _ int64) (store.AnnouncementMutation, error) {
			a := sampleAnnouncement()
			a.ID = id
			return store.AnnouncementMutation{Announcement: a, Published: true}, nil
		},
	}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Notifier: notifier, WaitForSideEffects: true,
	})
	got := doAnnouncementRequest(t, mux, announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage),
		http.MethodPost, "/api/announcements/"+sampleAnnouncement().ID+"/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("投递失败后发布 = %d，want 200（best-effort）", got.Code)
	}
}

func TestAnnouncementIdempotentPublishSkipsInApp(t *testing.T) {
	notifier := &fakeAnnouncementNotifier{}
	repo := &fakeAnnouncementRepo{
		publishFn: func(_ context.Context, id string, _ int64) (store.AnnouncementMutation, error) {
			a := sampleAnnouncement()
			a.ID = id
			return store.AnnouncementMutation{Announcement: a, Published: false}, nil
		},
	}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Notifier: notifier, WaitForSideEffects: true,
	})
	if got := doAnnouncementRequest(t, mux, announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage),
		http.MethodPost, "/api/announcements/"+sampleAnnouncement().ID+"/publish", nil); got.Code != http.StatusOK {
		t.Fatalf("publish = %d", got.Code)
	}
	if len(notifier.inputs) != 0 {
		t.Fatalf("幂等重放触发了 %d 次投递", len(notifier.inputs))
	}
}

// TestAnnouncementUpdatePublishNowDelivers：PUT + publish_now 的首次发布路径
// （编辑草稿并原子发布）同样必须触发站内投递——handler 里 update 与 create
// 是两条独立分支，create 已覆盖，这里锁定 update 分支。
func TestAnnouncementUpdatePublishNowDelivers(t *testing.T) {
	notifier := &fakeAnnouncementNotifier{result: notify.AnnouncementDeliveryResult{Delivered: 2}}
	repo := &fakeAnnouncementRepo{
		updateFn: func(_ context.Context, id string, in store.AnnouncementInput, _ int64) (store.AnnouncementMutation, error) {
			if !in.PublishNow {
				t.Fatalf("publish_now 未透传到 store：%+v", in)
			}
			a := sampleAnnouncement()
			a.ID = id
			a.Title = in.Title
			a.Content = in.Content
			return store.AnnouncementMutation{Announcement: a, Published: true}, nil
		},
	}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Notifier: notifier, WaitForSideEffects: true,
	})
	got := doAnnouncementRequest(t, mux, announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage),
		http.MethodPut, "/api/announcements/"+sampleAnnouncement().ID,
		map[string]any{"title": "First publish", "content": "maintenance", "publish_now": true})
	if got.Code != http.StatusOK {
		t.Fatalf("PUT publish_now = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	if len(notifier.inputs) != 1 {
		t.Fatalf("PUT publish_now 投递调用 %d 次，want 1", len(notifier.inputs))
	}
	if notifier.inputs[0].AnnouncementID != sampleAnnouncement().ID {
		t.Fatalf("投递公告 id = %s", notifier.inputs[0].AnnouncementID)
	}
}

// TestAnnouncementPublishAuditRecordsDeliveryFailureCount：发布审计在
// delivered_users 旁带 delivery_failed，部分批失败可被运维看见，但响应仍是
// 200（best-effort，不回滚发布）。
func TestAnnouncementPublishAuditRecordsDeliveryFailureCount(t *testing.T) {
	capture := &captureExec{}
	notifier := &fakeAnnouncementNotifier{
		result: notify.AnnouncementDeliveryResult{Delivered: 2, Duplicates: 1, Failed: 3},
	}
	repo := &fakeAnnouncementRepo{
		publishFn: func(_ context.Context, id string, _ int64) (store.AnnouncementMutation, error) {
			a := sampleAnnouncement()
			a.ID = id
			return store.AnnouncementMutation{Announcement: a, Published: true}, nil
		},
	}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Notifier: notifier, Audit: audit.New(capture), WaitForSideEffects: true,
	})
	got := doAnnouncementRequest(t, mux, announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage),
		http.MethodPost, "/api/announcements/"+sampleAnnouncement().ID+"/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("投递部分失败后发布 = %d，want 200（body=%s）", got.Code, got.Body.String())
	}

	var detail map[string]any
	for _, call := range capture.snapshot() {
		// insertSQL 参数顺序：actor, username, category, ip, ua, method,
		// route, status, result, detail, before, after。
		if len(call) < 10 {
			continue
		}
		raw, ok := call[9].(string)
		if !ok {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			t.Fatalf("审计 detail 不是 JSON：%s", raw)
		}
		if parsed["action"] == audit.ActionAnnouncementPublish {
			detail = parsed
			break
		}
	}
	if detail == nil {
		t.Fatal("未捕获 announcement.publish 审计")
	}
	if detail["delivered_users"] != float64(2) {
		t.Fatalf("delivered_users = %v，want 2（detail=%v）", detail["delivered_users"], detail)
	}
	if detail["delivery_failed"] != float64(3) {
		t.Fatalf("delivery_failed = %v，want 3（detail=%v）", detail["delivery_failed"], detail)
	}
}

// TestAnnouncementNotificationSummary：摘要先剥链接/图片语法、再剥行首标记，
// 正文里的日期连字符必须原样保留。
func TestAnnouncementNotificationSummary(t *testing.T) {
	content := "## 2026-10-20 维护窗口\n" +
		"- 开始时间 2026-10-20 22:00\n" +
		"> 状态请见 [状态页](https://status.example.com/incidents/1)\n" +
		"![维护图示](https://cdn.example.com/a.png)\n" +
		"代码 `v1-2` 与 **粗体** 保持可读"
	got := announcementNotificationSummary(content)
	want := "2026-10-20 维护窗口 开始时间 2026-10-20 22:00 状态请见 状态页 维护图示 代码 v1-2 与 粗体 保持可读"
	if got != want {
		t.Fatalf("summary = %q\nwant     %q", got, want)
	}
	if strings.Contains(got, "https://") {
		t.Fatalf("摘要残留链接 URL：%q", got)
	}
}
