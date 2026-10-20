package httpapi

// D7a 站点公告端点的单元验收：权限正反、字段校验、状态码映射、分页筛选、
// 用户侧 active/read、发布副作用与限流。真实 SQL 语义（N-35/虚拟过期/
// 删除级联 reads/审计落库）在 handlers_announcements_integration_test.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeAnnouncementRepo 用函数字段覆盖每个方法：每个用例只装配它关心的
// 行为，未装配的方法返回零值——避免一个 400 行的内存实现成为测试的第二
// 真源（真实语义由集成测试对 DB 断言）。
type fakeAnnouncementRepo struct {
	createFn     func(context.Context, store.AnnouncementInput, int64) (store.AnnouncementMutation, error)
	getFn        func(context.Context, string) (store.Announcement, error)
	updateFn     func(context.Context, string, store.AnnouncementInput, int64) (store.AnnouncementMutation, error)
	listFn       func(context.Context, store.AnnouncementFilter) ([]store.Announcement, int64, error)
	publishFn    func(context.Context, string, int64) (store.AnnouncementMutation, error)
	offlineFn    func(context.Context, string, int64) (store.AnnouncementMutation, error)
	deleteFn     func(context.Context, string) error
	listActiveFn func(context.Context, int64, time.Time) ([]store.Announcement, error)
	markReadFn   func(context.Context, int64, string, time.Time) error
}

func (f *fakeAnnouncementRepo) Create(ctx context.Context, in store.AnnouncementInput, actor int64) (store.AnnouncementMutation, error) {
	if f.createFn != nil {
		return f.createFn(ctx, in, actor)
	}
	return store.AnnouncementMutation{}, nil
}

func (f *fakeAnnouncementRepo) Get(ctx context.Context, id string) (store.Announcement, error) {
	if f.getFn != nil {
		return f.getFn(ctx, id)
	}
	return store.Announcement{}, store.ErrAnnouncementNotFound
}

func (f *fakeAnnouncementRepo) Update(ctx context.Context, id string, in store.AnnouncementInput, actor int64) (store.AnnouncementMutation, error) {
	if f.updateFn != nil {
		return f.updateFn(ctx, id, in, actor)
	}
	return store.AnnouncementMutation{}, nil
}

func (f *fakeAnnouncementRepo) List(ctx context.Context, filter store.AnnouncementFilter) ([]store.Announcement, int64, error) {
	if f.listFn != nil {
		return f.listFn(ctx, filter)
	}
	return nil, 0, nil
}

func (f *fakeAnnouncementRepo) Publish(ctx context.Context, id string, actor int64) (store.AnnouncementMutation, error) {
	if f.publishFn != nil {
		return f.publishFn(ctx, id, actor)
	}
	return store.AnnouncementMutation{}, nil
}

func (f *fakeAnnouncementRepo) Offline(ctx context.Context, id string, actor int64) (store.AnnouncementMutation, error) {
	if f.offlineFn != nil {
		return f.offlineFn(ctx, id, actor)
	}
	return store.AnnouncementMutation{}, nil
}

func (f *fakeAnnouncementRepo) Delete(ctx context.Context, id string) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, id)
	}
	return nil
}

func (f *fakeAnnouncementRepo) ListActive(ctx context.Context, userID int64, now time.Time) ([]store.Announcement, error) {
	if f.listActiveFn != nil {
		return f.listActiveFn(ctx, userID, now)
	}
	return nil, nil
}

func (f *fakeAnnouncementRepo) MarkRead(ctx context.Context, userID int64, id string, now time.Time) error {
	if f.markReadFn != nil {
		return f.markReadFn(ctx, userID, id, now)
	}
	return nil
}

// fakeAnnouncementWebhook 记录发布副作用投递。
type fakeAnnouncementWebhook struct {
	mu       sync.Mutex
	payloads [][]byte
	events   []string
	err      error
}

func (f *fakeAnnouncementWebhook) Send(_ context.Context, payload []byte, event, _ string) (notify.WebhookResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads = append(f.payloads, append([]byte(nil), payload...))
	f.events = append(f.events, event)
	if f.err != nil {
		return notify.WebhookResult{}, f.err
	}
	return notify.WebhookResult{StatusCode: 200, OK: true}, nil
}

// ---------------------------------------------------------------------------
// 装配辅助
// ---------------------------------------------------------------------------

func newAnnouncementTestHandlers(t *testing.T, cfg AnnouncementHandlerConfig) (*AnnouncementHandlers, *http.ServeMux) {
	t.Helper()
	if cfg.Authz == nil {
		cfg.Authz = rbac.NewAuthzMiddleware()
	}
	if cfg.Logger == nil {
		cfg.Logger = testLogger()
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	}
	h, err := NewAnnouncementHandlers(cfg)
	if err != nil {
		t.Fatalf("NewAnnouncementHandlers: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return h, mux
}

func announcementTestPrincipal(userID int64, role string, perms ...string) *auth.Principal {
	return &auth.Principal{UserID: userID, Role: role, Permissions: perms}
}

func doAnnouncementRequest(t *testing.T, mux http.Handler, principal *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if principal != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	}
	return serve(mux, req)
}

func sampleAnnouncement() store.Announcement {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	return store.Announcement{
		ID: "11111111-1111-4111-8111-111111111111", Title: "Maintenance",
		Category: store.AnnouncementCategoryMaintenance, Content: "**down**",
		PublishAt: now, Status: store.AnnouncementStatusPublished,
		CreatedBy: 1, UpdatedBy: 1, PublishedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
}

// ---------------------------------------------------------------------------
// 权限
// ---------------------------------------------------------------------------

func TestAnnouncementAdminRequiresSettingsManage(t *testing.T) {
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: &fakeAnnouncementRepo{}})

	user := announcementTestPrincipal(7, rbac.RoleUser)
	if got := doAnnouncementRequest(t, mux, user, http.MethodGet, "/api/announcements", nil); got.Code != http.StatusForbidden {
		t.Fatalf("普通用户 GET /api/announcements = %d，want 403", got.Code)
	}

	limitedAdmin := announcementTestPrincipal(8, rbac.RoleAdmin, rbac.PermUsersRead)
	if got := doAnnouncementRequest(t, mux, limitedAdmin, http.MethodPost, "/api/announcements",
		map[string]any{"title": "x", "content": "y"}); got.Code != http.StatusForbidden {
		t.Fatalf("缺 settings:manage 的 admin = %d，want 403", got.Code)
	}

	fullAdmin := announcementTestPrincipal(9, rbac.RoleAdmin, rbac.PermSettingsManage)
	if got := doAnnouncementRequest(t, mux, fullAdmin, http.MethodGet, "/api/announcements", nil); got.Code != http.StatusOK {
		t.Fatalf("settings:manage admin GET = %d，want 200", got.Code)
	}
}

func TestAnnouncementUserEndpointsRequireLogin(t *testing.T) {
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: &fakeAnnouncementRepo{}})
	for _, path := range []string{"/api/announcements/active"} {
		got := doAnnouncementRequest(t, mux, nil, http.MethodGet, path, nil)
		if got.Code != http.StatusUnauthorized && got.Code != http.StatusForbidden {
			t.Fatalf("未登录 %s = %d，want 401/403", path, got.Code)
		}
	}
}

// TestAnnouncementUnknownFieldRejected 证明 RBAC 字段层生效（422 之外的另一
// 道门：未知字段 403，绝不静默丢弃）。
func TestAnnouncementUnknownFieldRejected(t *testing.T) {
	repo := &fakeAnnouncementRepo{}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	got := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
		map[string]any{"title": "x", "content": "y", "created_by": 99})
	if got.Code != http.StatusForbidden {
		t.Fatalf("未知字段 = %d，want 403", got.Code)
	}
}

// ---------------------------------------------------------------------------
// 校验与状态码
// ---------------------------------------------------------------------------

func TestAnnouncementCreateValidation(t *testing.T) {
	called := false
	repo := &fakeAnnouncementRepo{createFn: func(context.Context, store.AnnouncementInput, int64) (store.AnnouncementMutation, error) {
		called = true
		return store.AnnouncementMutation{}, nil
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"empty title", map[string]any{"title": "", "content": "x"}, "title"},
		{"long title", map[string]any{"title": strings.Repeat("a", 81), "content": "x"}, "title"},
		{"empty content", map[string]any{"title": "t", "content": ""}, "content"},
		{"oversized content", map[string]any{"title": "t", "content": strings.Repeat("a", store.AnnouncementMaxContentBytes+1)}, "content"},
		{"bad category", map[string]any{"title": "t", "content": "x", "category": "bogus"}, "category"},
		// L11：时间窗不合法时必须回 fields:["expire_at"]，前端才能高亮
		// 到期时间输入框；equal 与 before 都非法（要求严格晚于）。
		{"expire equals publish", map[string]any{
			"title": "t", "content": "x",
			"publish_at": time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
			"expire_at":  time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
		}, "expire_at"},
		{"expire before publish", map[string]any{
			"title": "t", "content": "x",
			"publish_at": time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
			"expire_at":  time.Date(2026, 10, 21, 9, 59, 0, 0, time.UTC),
		}, "expire_at"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements", c.body)
			if got.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d，want 422（body=%s）", got.Code, got.Body.String())
			}
			var problem struct {
				Fields []string `json:"fields"`
			}
			if err := json.Unmarshal(got.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			found := false
			for _, f := range problem.Fields {
				if f == c.field {
					found = true
				}
			}
			if !found {
				t.Fatalf("fields = %v，want 含 %q", problem.Fields, c.field)
			}
		})
	}
	if called {
		t.Fatal("校验失败的请求不应触达 repo.Create")
	}
}

func TestAnnouncementErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", store.ErrAnnouncementNotFound, http.StatusNotFound},
		{"state conflict", store.ErrAnnouncementStateConflict, http.StatusConflict},
		{"invalid", store.ErrAnnouncementInvalid, http.StatusUnprocessableEntity},
		{"internal", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			repo := &fakeAnnouncementRepo{getFn: func(context.Context, string) (store.Announcement, error) {
				return store.Announcement{}, c.err
			}}
			_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
			admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
			got := doAnnouncementRequest(t, mux, admin, http.MethodGet,
				"/api/announcements/11111111-1111-4111-8111-111111111111", nil)
			if got.Code != c.want {
				t.Fatalf("status = %d，want %d", got.Code, c.want)
			}
		})
	}
}

func TestAnnouncementOfflineConflict(t *testing.T) {
	repo := &fakeAnnouncementRepo{offlineFn: func(context.Context, string, int64) (store.AnnouncementMutation, error) {
		return store.AnnouncementMutation{}, store.ErrAnnouncementStateConflict
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	got := doAnnouncementRequest(t, mux, admin, http.MethodPost,
		"/api/announcements/11111111-1111-4111-8111-111111111111/offline", nil)
	if got.Code != http.StatusConflict {
		t.Fatalf("draft→offline = %d，want 409", got.Code)
	}
}

// ---------------------------------------------------------------------------
// 列表：分页/筛选/虚拟过期
// ---------------------------------------------------------------------------

func TestAnnouncementListFilterAndPagination(t *testing.T) {
	var gotFilter store.AnnouncementFilter
	expired := sampleAnnouncement()
	past := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	expired.ExpireAt = &past
	repo := &fakeAnnouncementRepo{listFn: func(_ context.Context, f store.AnnouncementFilter) ([]store.Announcement, int64, error) {
		gotFilter = f
		return []store.Announcement{expired}, 1, nil
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	got := doAnnouncementRequest(t, mux, admin, http.MethodGet,
		"/api/announcements?status=expired&category=maintenance&p=2&page_size=50", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d，want 200", got.Code)
	}
	if gotFilter.Status != "expired" || gotFilter.Category != "maintenance" ||
		gotFilter.Page != 2 || gotFilter.PageSize != 50 {
		t.Fatalf("filter = %+v", gotFilter)
	}
	var view announcementListView
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(view.Items) != 1 || view.Items[0].DisplayStatus != store.AnnouncementDisplayExpired {
		t.Fatalf("虚拟过期未派生：%+v", view.Items)
	}
}

func TestAnnouncementListRejectsBadQuery(t *testing.T) {
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: &fakeAnnouncementRepo{}})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	for _, path := range []string{
		"/api/announcements?status=bogus",
		// M1：category 与 status 同口径，非法枚举值在 handler 预校验 400。
		"/api/announcements?category=bogus",
		"/api/announcements?p=0",
		"/api/announcements?page_size=101",
	} {
		if got := doAnnouncementRequest(t, mux, admin, http.MethodGet, path, nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d，want 400", path, got.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 用户侧 active / read
// ---------------------------------------------------------------------------

func TestAnnouncementActiveReturnsReadFlag(t *testing.T) {
	item := sampleAnnouncement()
	item.Read = true
	var gotUser int64
	repo := &fakeAnnouncementRepo{listActiveFn: func(_ context.Context, userID int64, _ time.Time) ([]store.Announcement, error) {
		gotUser = userID
		return []store.Announcement{item}, nil
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	user := announcementTestPrincipal(42, rbac.RoleUser)

	got := doAnnouncementRequest(t, mux, user, http.MethodGet, "/api/announcements/active", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	if gotUser != 42 {
		t.Fatalf("数据范围 = %d，want Principal.UserID=42", gotUser)
	}
	var view struct {
		Items []struct {
			ID   string `json:"id"`
			Read bool   `json:"read"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(view.Items) != 1 || !view.Items[0].Read {
		t.Fatalf("items = %+v，want read=true", view.Items)
	}
}

func TestAnnouncementMarkReadIsIdempotent(t *testing.T) {
	calls := 0
	repo := &fakeAnnouncementRepo{markReadFn: func(context.Context, int64, string, time.Time) error {
		calls++
		return nil
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	user := announcementTestPrincipal(42, rbac.RoleUser)
	path := "/api/announcements/11111111-1111-4111-8111-111111111111/read"
	for i := 0; i < 2; i++ {
		if got := doAnnouncementRequest(t, mux, user, http.MethodPost, path, nil); got.Code != http.StatusNoContent {
			t.Fatalf("第 %d 次 read = %d，want 204", i+1, got.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("repo.MarkRead 调用 %d 次，want 2（幂等由 store 的 ON CONFLICT 保证）", calls)
	}
}

func TestAnnouncementMarkReadUnknownIsNotFound(t *testing.T) {
	repo := &fakeAnnouncementRepo{markReadFn: func(context.Context, int64, string, time.Time) error {
		return store.ErrAnnouncementNotFound
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo})
	user := announcementTestPrincipal(42, rbac.RoleUser)
	got := doAnnouncementRequest(t, mux, user, http.MethodPost,
		"/api/announcements/11111111-1111-4111-8111-111111111111/read", nil)
	if got.Code != http.StatusNotFound {
		t.Fatalf("非生效公告 read = %d，want 404", got.Code)
	}
}

// ---------------------------------------------------------------------------
// 发布副作用
// ---------------------------------------------------------------------------

func TestAnnouncementPublishWebhookPayload(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	repo := &fakeAnnouncementRepo{publishFn: func(context.Context, string, int64) (store.AnnouncementMutation, error) {
		return store.AnnouncementMutation{Announcement: sampleAnnouncement(), Published: true}, nil
	}}
	webhook := &fakeAnnouncementWebhook{}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Webhook: webhook, WaitForSideEffects: true,
		BaseURL: func() string { return "https://gw.example.com/" },
		Now:     func() time.Time { return now },
	})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	got := doAnnouncementRequest(t, mux, admin, http.MethodPost,
		"/api/announcements/11111111-1111-4111-8111-111111111111/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("publish = %d，want 200（body=%s）", got.Code, got.Body.String())
	}

	webhook.mu.Lock()
	defer webhook.mu.Unlock()
	if len(webhook.payloads) != 1 || len(webhook.events) != 1 {
		t.Fatalf("webhook 投递 %d 次，want 1", len(webhook.payloads))
	}
	if webhook.events[0] != "announcement" {
		t.Fatalf("event = %q，want announcement", webhook.events[0])
	}
	var payload map[string]any
	if err := json.Unmarshal(webhook.payloads[0], &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for _, key := range []string{"announcement_id", "title", "category", "published_at", "url"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("payload 缺少 %q：%s", key, webhook.payloads[0])
		}
	}
	// L3：公告详情页由 D7b 实现；那之前 url 回站点根，避免 webhook 指向 404。
	if payload["url"] != "https://gw.example.com/" {
		t.Fatalf("url = %v，want 站点根", payload["url"])
	}
	if payload["announcement_id"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("announcement_id = %v", payload["announcement_id"])
	}
}

func TestAnnouncementPublishWebhookUnconfiguredIsSilent(t *testing.T) {
	repo := &fakeAnnouncementRepo{publishFn: func(context.Context, string, int64) (store.AnnouncementMutation, error) {
		return store.AnnouncementMutation{Announcement: sampleAnnouncement(), Published: true}, nil
	}}
	webhook := &fakeAnnouncementWebhook{err: notify.ErrWebhookDisabled}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Webhook: webhook, WaitForSideEffects: true,
	})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	got := doAnnouncementRequest(t, mux, admin, http.MethodPost,
		"/api/announcements/11111111-1111-4111-8111-111111111111/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("未配置 webhook 时 publish = %d，want 200", got.Code)
	}
}

func TestAnnouncementIdempotentPublishDoesNotResendWebhook(t *testing.T) {
	repo := &fakeAnnouncementRepo{publishFn: func(context.Context, string, int64) (store.AnnouncementMutation, error) {
		// 幂等重放：目标态已达成。
		return store.AnnouncementMutation{Announcement: sampleAnnouncement(), Published: false}, nil
	}}
	webhook := &fakeAnnouncementWebhook{}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Webhook: webhook, WaitForSideEffects: true,
	})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	if got := doAnnouncementRequest(t, mux, admin, http.MethodPost,
		"/api/announcements/11111111-1111-4111-8111-111111111111/publish", nil); got.Code != http.StatusOK {
		t.Fatalf("status = %d", got.Code)
	}
	webhook.mu.Lock()
	defer webhook.mu.Unlock()
	if len(webhook.payloads) != 0 {
		t.Fatalf("幂等重放不应重复投递，实际 %d 次", len(webhook.payloads))
	}
}

// ---------------------------------------------------------------------------
// 发布副作用：Redis 广播（M2）
// ---------------------------------------------------------------------------

// newAnnouncementPubSub 订阅发布广播频道并等待 SUBSCRIBE 确认。先确认再发
// 请求，避免「发布快于订阅」的假阴性。
func newAnnouncementPubSub(t *testing.T) (*redis.Client, *redis.PubSub) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	pubsub := rdb.Subscribe(context.Background(), announcementAnnounceChannel)
	t.Cleanup(func() { _ = pubsub.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := pubsub.Receive(ctx); err != nil {
		t.Fatalf("subscribe %s: %v", announcementAnnounceChannel, err)
	}
	return rdb, pubsub
}

// expectAnnouncementBroadcast 断言频道上收到一条消息且内容为公告 id。
func expectAnnouncementBroadcast(t *testing.T, pubsub *redis.PubSub, wantID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg, err := pubsub.ReceiveMessage(ctx)
	if err != nil {
		t.Fatalf("未收到 %s 广播：%v", announcementAnnounceChannel, err)
	}
	if msg.Channel != announcementAnnounceChannel {
		t.Fatalf("channel = %q，want %q", msg.Channel, announcementAnnounceChannel)
	}
	if msg.Payload != wantID {
		t.Fatalf("payload = %q，want 公告 id %q", msg.Payload, wantID)
	}
}

// expectNoAnnouncementBroadcast 在短窗口内确认频道静默（副作用路径根本没跑）。
func expectNoAnnouncementBroadcast(t *testing.T, pubsub *redis.PubSub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if msg, err := pubsub.ReceiveMessage(ctx); err == nil {
		t.Fatalf("不应广播，实际收到 %q on %s", msg.Payload, msg.Channel)
	}
}

func TestAnnouncementPublishBroadcastsRedisChannel(t *testing.T) {
	rdb, pubsub := newAnnouncementPubSub(t)
	repo := &fakeAnnouncementRepo{publishFn: func(context.Context, string, int64) (store.AnnouncementMutation, error) {
		return store.AnnouncementMutation{Announcement: sampleAnnouncement(), Published: true}, nil
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
		Repo: repo, Redis: rdb, WaitForSideEffects: true,
	})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)
	got := doAnnouncementRequest(t, mux, admin, http.MethodPost,
		"/api/announcements/11111111-1111-4111-8111-111111111111/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("publish = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	expectAnnouncementBroadcast(t, pubsub, sampleAnnouncement().ID)
}

func TestAnnouncementNoRebroadcastWhenStateAlreadyReached(t *testing.T) {
	rdb, pubsub := newAnnouncementPubSub(t)
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	t.Run("idempotent publish", func(t *testing.T) {
		repo := &fakeAnnouncementRepo{publishFn: func(context.Context, string, int64) (store.AnnouncementMutation, error) {
			return store.AnnouncementMutation{Announcement: sampleAnnouncement(), Published: false}, nil
		}}
		_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
			Repo: repo, Redis: rdb, WaitForSideEffects: true,
		})
		if got := doAnnouncementRequest(t, mux, admin, http.MethodPost,
			"/api/announcements/11111111-1111-4111-8111-111111111111/publish", nil); got.Code != http.StatusOK {
			t.Fatalf("status = %d，want 200", got.Code)
		}
		expectNoAnnouncementBroadcast(t, pubsub)
	})

	t.Run("edit published", func(t *testing.T) {
		repo := &fakeAnnouncementRepo{updateFn: func(context.Context, string, store.AnnouncementInput, int64) (store.AnnouncementMutation, error) {
			// N-35：编辑已发布公告是原地更新，不是一次新的发布迁移。
			return store.AnnouncementMutation{Announcement: sampleAnnouncement(), Published: false}, nil
		}}
		_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{
			Repo: repo, Redis: rdb, WaitForSideEffects: true,
		})
		if got := doAnnouncementRequest(t, mux, admin, http.MethodPut,
			"/api/announcements/11111111-1111-4111-8111-111111111111",
			map[string]any{"title": "Maintenance", "content": "**down**"}); got.Code != http.StatusOK {
			t.Fatalf("status = %d，want 200（body=%s）", got.Code, got.Body.String())
		}
		expectNoAnnouncementBroadcast(t, pubsub)
	})
}

// ---------------------------------------------------------------------------
// 限流
// ---------------------------------------------------------------------------

func TestAnnouncementAdminWriteRateLimited(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := &fakeAnnouncementRepo{createFn: func(context.Context, store.AnnouncementInput, int64) (store.AnnouncementMutation, error) {
		return store.AnnouncementMutation{Announcement: sampleAnnouncement()}, nil
	}}
	_, mux := newAnnouncementTestHandlers(t, AnnouncementHandlerConfig{Repo: repo, Redis: rdb})
	admin := announcementTestPrincipal(1, rbac.RoleAdmin, rbac.PermSettingsManage)

	last := 0
	for i := 0; i < announcementWriteRateLimit+1; i++ {
		got := doAnnouncementRequest(t, mux, admin, http.MethodPost, "/api/announcements",
			map[string]any{"title": "t", "content": "x"})
		last = got.Code
		if i < announcementWriteRateLimit && got.Code != http.StatusCreated {
			t.Fatalf("第 %d 次写 = %d，want 201", i+1, got.Code)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("超限后 = %d，want 429", last)
	}
}
