//go:build integration

package httpapi

// D7a 的数据库级验收：真实 PostgreSQL + 真实 store.SQLAnnouncementRepo。
// 覆盖单元测试无法证明的真相：
//   - R-D7-03/N-35：编辑已发布公告原地更新（publish_at/published_at 不变），
//     offline 编辑保存回 draft 并清 offline_at；
//   - 状态机（draft/offline → published，published → offline，draft → offline 409）；
//   - publish_now 的原子性（校验失败不留草稿行）；
//   - 虚拟过期筛选（DB 只有 published，筛选由 expire_at 派生）；
//   - active 只含生效中公告、read 标记、read 幂等；
//   - 删除公告同事务清已读行；
//   - 审计 detail 不含正文。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d7' \
//	NEWTG_TEST_LOGS_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d7_logs' \
//	  go test -tags=integration ./internal/httpapi/ -run TestAnnouncementIntegration -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/audit"
	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/notify"
	"newtg/gateway/internal/rbac"
	"newtg/gateway/internal/store"
)

// a5UserSeq 保证 A5 集成用例的用户名唯一（users.username 有唯一约束）。
var a5UserSeq atomic.Int64

type announcementIntegrationEnv struct {
	db     *sql.DB
	logs   *sql.DB
	repo   *store.SQLAnnouncementRepo
	router http.Handler
	// adminID 是本次用例专用的管理员 id。为每个用例分配唯一 actor：
	// 审计行写入共享日志库，而其它用例（如 audit-logs 分页）按 user_id
	// 断言行数——复用 actor=1 会让我的公告审计污染它们的计数。
	adminID int64
}

func newAnnouncementIntegrationEnv(t *testing.T) *announcementIntegrationEnv {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var logsDB *sql.DB
	if logsDSN := os.Getenv("NEWTG_TEST_LOGS_DSN"); logsDSN != "" {
		logsDB, err = sql.Open("pgx", logsDSN)
		if err != nil {
			t.Fatalf("open logs db: %v", err)
		}
		t.Cleanup(func() { _ = logsDB.Close() })
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	repo := store.NewAnnouncementRepo(db)
	var auditLogger *audit.Logger
	if logsDB != nil {
		auditLogger = audit.New(logsDB)
	}
	handlers, err := NewAnnouncementHandlers(AnnouncementHandlerConfig{
		Repo: repo, Redis: rdb, Authz: rbac.NewAuthzMiddleware(),
		Audit: auditLogger, Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewAnnouncementHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	// D7b：公开详情在真实 Router 里挂根 mux；集成环境用同一 ServeMux 同时
	// 承载两条路径（精确模式优先，语义与生产一致）。
	handlers.RegisterPublic(mux)
	adminID := time.Now().UnixNano()%1_000_000_000 + 2_000_000_000
	env := &announcementIntegrationEnv{db: db, logs: logsDB, repo: repo, router: mux, adminID: adminID}
	if logsDB != nil {
		t.Cleanup(func() {
			_, _ = logsDB.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, adminID)
		})
	}
	return env
}

// adminPrincipal 返回本用例专用的全权管理员身份。
func (e *announcementIntegrationEnv) adminPrincipal() *auth.Principal {
	return announcementTestPrincipal(e.adminID, rbac.RoleAdmin, rbac.PermSettingsManage)
}

func (e *announcementIntegrationEnv) do(t *testing.T, principal *auth.Principal, method, path string, body any) *httptest.ResponseRecorder {
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
	return serve(e.router, req)
}

func (e *announcementIntegrationEnv) cleanupAnnouncement(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = e.db.Exec(`DELETE FROM announcement_reads WHERE announcement_id = $1`, id)
		_, _ = e.db.Exec(`DELETE FROM announcements WHERE id = $1`, id)
	})
}

func (e *announcementIntegrationEnv) createAnnouncement(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	got := e.do(t, e.adminPrincipal(), http.MethodPost, "/api/announcements", body)
	if got.Code != http.StatusCreated {
		t.Fatalf("create = %d，want 201（body=%s）", got.Code, got.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	e.cleanupAnnouncement(t, view["id"].(string))
	return view
}

func (e *announcementIntegrationEnv) insertUser(t *testing.T) int64 {
	t.Helper()
	username := fmt.Sprintf("d7a_%d", time.Now().UnixNano())
	var id int64
	if err := e.db.QueryRow(`INSERT INTO users (username, role) VALUES ($1, 'user') RETURNING id`,
		username).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { _, _ = e.db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

// TestAnnouncementIntegrationCreateAndValidation 覆盖创建默认值与超长/时间校验。
func TestAnnouncementIntegrationCreateAndValidation(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()

	draft := env.createAnnouncement(t, map[string]any{
		"title": "Draft", "content": "**hello**",
	})
	if draft["status"] != store.AnnouncementStatusDraft || draft["display_status"] != store.AnnouncementStatusDraft {
		t.Fatalf("默认应为 draft：%+v", draft)
	}
	if draft["category"] != store.AnnouncementCategoryNotice {
		t.Fatalf("默认分类 = %v，want notice", draft["category"])
	}
	if draft["content_html"] == "" || !strings.Contains(draft["content_html"].(string), "<strong>hello</strong>") {
		t.Fatalf("content_html 未渲染：%v", draft["content_html"])
	}

	// 内容超限 → 422 且不留行。
	before := env.countAnnouncements(t)
	got := env.do(t, admin, http.MethodPost, "/api/announcements", map[string]any{
		"title": "Too long", "content": strings.Repeat("a", store.AnnouncementMaxContentBytes+1),
	})
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("超长正文 = %d，want 422", got.Code)
	}
	if after := env.countAnnouncements(t); after != before {
		t.Fatalf("校验失败留下 %d 行（before=%d）", after, before)
	}

	// publish_at 晚于 expire_at → 422（不落行）。
	publishAt := time.Now().UTC().Add(time.Hour)
	expireAt := publishAt.Add(-time.Minute)
	got = env.do(t, admin, http.MethodPost, "/api/announcements", map[string]any{
		"title": "Bad window", "content": "x",
		"publish_at": publishAt.Format(time.RFC3339),
		"expire_at":  expireAt.Format(time.RFC3339),
	})
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expire<=publish = %d，want 422（body=%s）", got.Code, got.Body.String())
	}
	if after := env.countAnnouncements(t); after != before {
		t.Fatalf("非法时间窗留下 %d 行（before=%d）", after, before)
	}
}

func (e *announcementIntegrationEnv) countAnnouncements(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM announcements`).Scan(&n); err != nil {
		t.Fatalf("count announcements: %v", err)
	}
	return n
}

// TestAnnouncementIntegrationPublishNowAtomic 覆盖 publish_now 的成功与回滚。
func TestAnnouncementIntegrationPublishNowAtomic(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()

	got := env.do(t, admin, http.MethodPost, "/api/announcements", map[string]any{
		"title": "Publish now", "content": "body", "publish_now": true,
	})
	if got.Code != http.StatusCreated {
		t.Fatalf("publish_now create = %d，want 201（body=%s）", got.Code, got.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	env.cleanupAnnouncement(t, view["id"].(string))
	if view["status"] != store.AnnouncementStatusPublished || view["published_at"] == nil {
		t.Fatalf("publish_now 应落 published + published_at：%+v", view)
	}

	// publish_now + 非法时间窗：整体回滚，不留草稿行。
	publishAt := time.Now().UTC().Add(time.Hour)
	expireAt := publishAt.Add(-time.Minute)
	before := env.countAnnouncements(t)
	got = env.do(t, admin, http.MethodPost, "/api/announcements", map[string]any{
		"title": "Atomic fail", "content": "x", "publish_now": true,
		"publish_at": publishAt.Format(time.RFC3339),
		"expire_at":  expireAt.Format(time.RFC3339),
	})
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("非法 publish_now = %d，want 422", got.Code)
	}
	if after := env.countAnnouncements(t); after != before {
		t.Fatalf("回滚失败：%d -> %d 行", before, after)
	}
}

// TestAnnouncementIntegrationStateMachine 覆盖迁移矩阵与幂等。
func TestAnnouncementIntegrationStateMachine(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()

	draft := env.createAnnouncement(t, map[string]any{"title": "SM", "content": "x"})
	id := draft["id"].(string)
	path := "/api/announcements/" + id

	// draft → offline 非法。
	if got := env.do(t, admin, http.MethodPost, path+"/offline", nil); got.Code != http.StatusConflict {
		t.Fatalf("draft→offline = %d，want 409", got.Code)
	}

	// draft → published。
	got := env.do(t, admin, http.MethodPost, path+"/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("draft→published = %d，want 200", got.Code)
	}
	var published map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &published)
	if published["status"] != store.AnnouncementStatusPublished {
		t.Fatalf("status = %v", published["status"])
	}
	publishedAt := published["published_at"]

	// published → published 幂等：published_at 不变。
	got = env.do(t, admin, http.MethodPost, path+"/publish", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("published→published = %d，want 200", got.Code)
	}
	var repeated map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &repeated)
	if repeated["published_at"] != publishedAt {
		t.Fatalf("幂等重放改写了 published_at：%v -> %v", publishedAt, repeated["published_at"])
	}

	// published → offline。
	if got := env.do(t, admin, http.MethodPost, path+"/offline", nil); got.Code != http.StatusOK {
		t.Fatalf("published→offline = %d，want 200", got.Code)
	}

	// offline → published（允许）。
	if got := env.do(t, admin, http.MethodPost, path+"/publish", nil); got.Code != http.StatusOK {
		t.Fatalf("offline→published = %d，want 200", got.Code)
	}
}

// TestAnnouncementIntegrationN35EditSemantics 覆盖编辑已发布/已下线。
func TestAnnouncementIntegrationN35EditSemantics(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()

	created := env.createAnnouncement(t, map[string]any{
		"title": "Edit me", "content": "v1", "publish_now": true,
	})
	id := created["id"].(string)
	path := "/api/announcements/" + id
	publishAt := created["publish_at"]
	publishedAt := created["published_at"]

	// 编辑已发布：保持 published，publish_at/published_at 不变（N-35）。
	newPublishAt := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	got := env.do(t, admin, http.MethodPut, path, map[string]any{
		"title": "Edited", "content": "v2", "publish_at": newPublishAt,
	})
	if got.Code != http.StatusOK {
		t.Fatalf("edit published = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	var edited map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &edited)
	if edited["status"] != store.AnnouncementStatusPublished {
		t.Fatalf("编辑已发布后 status = %v，want published（N-35 原地更新）", edited["status"])
	}
	if edited["publish_at"] != publishAt || edited["published_at"] != publishedAt {
		t.Fatalf("N-35：publish_at/published_at 不应变化，got %v/%v，want %v/%v",
			edited["publish_at"], edited["published_at"], publishAt, publishedAt)
	}
	if edited["content_markdown"] != "v2" {
		t.Fatalf("正文未更新：%v", edited["content_markdown"])
	}

	// 下线后编辑：回 draft 并清 offline_at。
	if got := env.do(t, admin, http.MethodPost, path+"/offline", nil); got.Code != http.StatusOK {
		t.Fatalf("offline = %d", got.Code)
	}
	got = env.do(t, admin, http.MethodPut, path, map[string]any{
		"title": "Back to draft", "content": "v3",
	})
	if got.Code != http.StatusOK {
		t.Fatalf("edit offline = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	var drafted map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &drafted)
	if drafted["status"] != store.AnnouncementStatusDraft {
		t.Fatalf("offline 编辑后 status = %v，want draft", drafted["status"])
	}
	if drafted["offline_at"] != nil {
		t.Fatalf("offline 编辑后 offline_at 应清空，got %v", drafted["offline_at"])
	}
}

// TestAnnouncementIntegrationVirtualExpired 覆盖虚拟过期筛选。
func TestAnnouncementIntegrationVirtualExpired(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()

	publishAt := time.Now().UTC().Add(-2 * time.Hour)
	expireAt := time.Now().UTC().Add(-time.Hour)
	got := env.do(t, admin, http.MethodPost, "/api/announcements", map[string]any{
		"title": "Expired", "content": "x", "publish_now": true,
		"publish_at": publishAt.Format(time.RFC3339),
		"expire_at":  expireAt.Format(time.RFC3339),
	})
	if got.Code != http.StatusCreated {
		t.Fatalf("create expired = %d（body=%s）", got.Code, got.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &created)
	id := created["id"].(string)
	env.cleanupAnnouncement(t, id)

	if created["status"] != store.AnnouncementStatusPublished {
		t.Fatalf("DB 状态应为 published（虚拟过期不落库）：%v", created["status"])
	}
	if created["display_status"] != store.AnnouncementDisplayExpired {
		t.Fatalf("display_status = %v，want expired", created["display_status"])
	}

	listIDs := func(t *testing.T, status string) []string {
		t.Helper()
		resp := env.do(t, admin, http.MethodGet, "/api/announcements?status="+status+"&page_size=100", nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("list %s = %d", status, resp.Code)
		}
		var view struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]string, 0, len(view.Items))
		for _, item := range view.Items {
			out = append(out, item.ID)
		}
		return out
	}
	contains := func(ids []string, want string) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}
	if !contains(listIDs(t, "expired"), id) {
		t.Fatal("expired 筛选未包含过期公告")
	}
	if contains(listIDs(t, "published"), id) {
		t.Fatal("published 筛选不应包含已过期公告（两个徽章互斥）")
	}
}

// TestAnnouncementIntegrationActiveAndRead 覆盖 active 查询与 read 幂等。
func TestAnnouncementIntegrationActiveAndRead(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	userID := env.insertUser(t)
	user := announcementTestPrincipal(userID, rbac.RoleUser)

	active := env.createAnnouncement(t, map[string]any{
		"title": "Active", "content": "x", "publish_now": true,
	})
	// 未来生效：不应出现在 active。
	future := env.createAnnouncement(t, map[string]any{
		"title": "Future", "content": "x", "publish_now": true,
		"publish_at": time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339),
	})
	// 草稿：不应出现在 active。
	draft := env.createAnnouncement(t, map[string]any{"title": "Draft", "content": "x"})

	activeIDs := func(t *testing.T) map[string]bool {
		t.Helper()
		resp := env.do(t, user, http.MethodGet, "/api/announcements/active", nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("active = %d（body=%s）", resp.Code, resp.Body.String())
		}
		var view struct {
			Items []struct {
				ID   string `json:"id"`
				Read bool   `json:"read"`
			} `json:"items"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &view); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]bool{}
		for _, item := range view.Items {
			out[item.ID] = item.Read
		}
		return out
	}

	items := activeIDs(t)
	if !containsKey(items, active["id"].(string)) {
		t.Fatal("生效中公告未出现在 active")
	}
	if containsKey(items, future["id"].(string)) || containsKey(items, draft["id"].(string)) {
		t.Fatal("active 不应包含未来生效/草稿公告")
	}
	if items[active["id"].(string)] {
		t.Fatal("首次读取 read 应为 false")
	}

	// read 幂等：两次都 204；随后 read=true。
	path := "/api/announcements/" + active["id"].(string) + "/read"
	for i := 0; i < 2; i++ {
		if got := env.do(t, user, http.MethodPost, path, nil); got.Code != http.StatusNoContent {
			t.Fatalf("第 %d 次 read = %d，want 204", i+1, got.Code)
		}
	}
	var readCount int
	if err := env.db.QueryRow(`SELECT count(*) FROM announcement_reads WHERE user_id = $1 AND announcement_id = $2`,
		userID, active["id"]).Scan(&readCount); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	if readCount != 1 {
		t.Fatalf("幂等 read 落了 %d 行，want 1", readCount)
	}
	if !activeIDs(t)[active["id"].(string)] {
		t.Fatal("标记已读后 active 的 read 应为 true")
	}

	// 未来生效公告不可标记已读（只返回 active 的 read 状态）。
	if got := env.do(t, user, http.MethodPost,
		"/api/announcements/"+future["id"].(string)+"/read", nil); got.Code != http.StatusNotFound {
		t.Fatalf("非生效公告 read = %d，want 404", got.Code)
	}
}

func containsKey(m map[string]bool, key string) bool {
	_, ok := m[key]
	return ok
}

// TestAnnouncementIntegrationDeleteCascadesReads 覆盖删除同事务清已读。
func TestAnnouncementIntegrationDeleteCascadesReads(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()
	userID := env.insertUser(t)
	user := announcementTestPrincipal(userID, rbac.RoleUser)

	created := env.createAnnouncement(t, map[string]any{
		"title": "Delete me", "content": "x", "publish_now": true,
	})
	id := created["id"].(string)
	if got := env.do(t, user, http.MethodPost, "/api/announcements/"+id+"/read", nil); got.Code != http.StatusNoContent {
		t.Fatalf("read = %d", got.Code)
	}

	if got := env.do(t, admin, http.MethodDelete, "/api/announcements/"+id, nil); got.Code != http.StatusNoContent {
		t.Fatalf("delete = %d，want 204（body=%s）", got.Code, got.Body.String())
	}
	var reads int
	if err := env.db.QueryRow(`SELECT count(*) FROM announcement_reads WHERE announcement_id = $1`, id).Scan(&reads); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	if reads != 0 {
		t.Fatalf("删除后仍残留 %d 条已读记录", reads)
	}
	if got := env.do(t, admin, http.MethodGet, "/api/announcements/"+id, nil); got.Code != http.StatusNotFound {
		t.Fatalf("删除后 GET = %d，want 404", got.Code)
	}
}

// TestAnnouncementIntegrationAuditHasNoContent 覆盖 R-D7-08：审计 detail 只含
// id/title/status，正文绝不进入审计表。
func TestAnnouncementIntegrationAuditHasNoContent(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	if env.logs == nil {
		t.Skip("NEWTG_TEST_LOGS_DSN 未设置，跳过审计断言")
	}
	admin := env.adminPrincipal()
	secret := fmt.Sprintf("SECRET-BODY-%d", time.Now().UnixNano())

	created := env.createAnnouncement(t, map[string]any{
		"title": "Audit", "content": secret,
	})
	id := created["id"].(string)
	path := "/api/announcements/" + id
	if got := env.do(t, admin, http.MethodPut, path, map[string]any{
		"title": "Audit edited", "content": secret,
	}); got.Code != http.StatusOK {
		t.Fatalf("update = %d", got.Code)
	}
	if got := env.do(t, admin, http.MethodPost, path+"/publish", nil); got.Code != http.StatusOK {
		t.Fatalf("publish = %d", got.Code)
	}
	if got := env.do(t, admin, http.MethodPost, path+"/offline", nil); got.Code != http.StatusOK {
		t.Fatalf("offline = %d", got.Code)
	}
	if got := env.do(t, admin, http.MethodDelete, path, nil); got.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", got.Code)
	}

	rows, err := env.logs.Query(`
		SELECT detail::text FROM audit_logs
		WHERE detail->>'action' LIKE 'announcement.%'
		ORDER BY id DESC LIMIT 10`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatalf("scan detail: %v", err)
		}
		if strings.Contains(detail, secret) {
			t.Fatalf("审计 detail 含公告正文：%s", detail)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(detail), &parsed); err != nil {
			t.Fatalf("detail 不是 JSON：%s", detail)
		}
		if parsed["id"] != id {
			continue
		}
		if _, ok := parsed["action"]; ok {
			seen[parsed["action"].(string)] = true
		}
		// detail 只允许 id/title/status + writer 注入的 action；
		// A5 的发布审计额外带 delivered_users/delivery_failed 计数（不含正文/收件人）。
		for key := range parsed {
			switch key {
			case "id", "title", "status", "action", "delivered_users", "delivery_failed":
			default:
				t.Fatalf("审计 detail 出现未允许字段 %q：%s", key, detail)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for _, action := range []string{
		audit.ActionAnnouncementCreate, audit.ActionAnnouncementUpdate,
		audit.ActionAnnouncementPublish, audit.ActionAnnouncementOffline,
		audit.ActionAnnouncementDelete,
	} {
		if !seen[action] {
			t.Fatalf("缺少审计动作 %q（seen=%v）", action, seen)
		}
	}
}

// newA5AnnouncementRouter 构造带真实 notify.Service 的公告路由（A5）。
//
// 模板播种是发布投递的前置：服务必须先 EnsureSeeded，否则 system.announcement
// 缺行会让 payload.body 降级为空（投递记录仍落库）。把这段装配抽出来，供
// 首次发布（POST publish_now）与编辑发布（PUT publish_now）两条路径复用。
func newA5AnnouncementRouter(t *testing.T, env *announcementIntegrationEnv) (*notify.Service, *http.ServeMux) {
	t.Helper()
	notifyStore, err := notify.NewStore(env.db)
	if err != nil {
		t.Fatalf("notify.NewStore: %v", err)
	}
	notifyService, err := notify.NewService(notify.ServiceConfig{Store: notifyStore, Logger: testLogger()})
	if err != nil {
		t.Fatalf("notify.NewService: %v", err)
	}
	if err := notifyService.EnsureSeeded(context.Background()); err != nil {
		t.Fatalf("seed templates: %v", err)
	}
	var auditLogger *audit.Logger
	if env.logs != nil {
		auditLogger = audit.New(env.logs)
	}
	handlers, err := NewAnnouncementHandlers(AnnouncementHandlerConfig{
		Repo: env.repo, Authz: rbac.NewAuthzMiddleware(), Audit: auditLogger,
		Logger: testLogger(), Notifier: notifyService, WaitForSideEffects: true,
	})
	if err != nil {
		t.Fatalf("NewAnnouncementHandlers: %v", err)
	}
	mux := http.NewServeMux()
	handlers.Register(mux)
	return notifyService, mux
}

// TestAnnouncementIntegrationInAppDeliveryAndAudit 覆盖 A5：
//   - 首次发布为每个启用用户落一条 system.announcement 站内投递；
//   - payload.body 非空且含公告标题（模板已播种 + 渲染成功）；
//   - 发布审计 detail 带 delivered_users/delivery_failed 且仍不含正文；
//   - 公告删除同事务清理其投递记录，且不影响另一条公告的投递。
func TestAnnouncementIntegrationInAppDeliveryAndAudit(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	_, mux := newA5AnnouncementRouter(t, env)

	// 两个启用用户（不同语言）+ 一个停用用户。
	insertUser := func(language, status string) int64 {
		t.Helper()
		username := fmt.Sprintf("a5i_%d_%d", time.Now().UnixNano(), a5UserSeq.Add(1))
		var id int64
		if err := env.db.QueryRow(`INSERT INTO users (username, language, status)
			VALUES ($1, $2, $3) RETURNING id`, username, language, status).Scan(&id); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM users WHERE id = $1`, id) })
		return id
	}
	insertUser("en", "enabled")
	insertUser("zh-CN", "enabled")
	disabledID := insertUser("en", "disabled")

	got := doAnnouncementRequest(t, mux, env.adminPrincipal(), http.MethodPost, "/api/announcements",
		map[string]any{"title": "A5", "content": "**maintenance** tonight", "publish_now": true})
	if got.Code != http.StatusCreated {
		t.Fatalf("publish_now create = %d（body=%s）", got.Code, got.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := view["id"].(string)
	env.cleanupAnnouncement(t, id)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+id+":%")
	})

	// 负向对照：另一条已发布公告的投递必须不受本次删除影响。级联清理按
	// dedupe_key 的 `announcement:<id>:%` 前缀匹配，任何把前缀写宽的改动
	// （例如只按 `announcement:%`）都会在这里变红。
	other := doAnnouncementRequest(t, mux, env.adminPrincipal(), http.MethodPost, "/api/announcements",
		map[string]any{"title": "A5 other", "content": "unrelated", "publish_now": true})
	if other.Code != http.StatusCreated {
		t.Fatalf("create second announcement = %d（body=%s）", other.Code, other.Body.String())
	}
	var otherView map[string]any
	if err := json.Unmarshal(other.Body.Bytes(), &otherView); err != nil {
		t.Fatalf("decode second announcement: %v", err)
	}
	otherID := otherView["id"].(string)
	env.cleanupAnnouncement(t, otherID)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+otherID+":%")
	})
	var otherDelivered int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1`, "announcement:"+otherID+":%").Scan(&otherDelivered); err != nil {
		t.Fatalf("count second announcement deliveries: %v", err)
	}
	if otherDelivered < 2 {
		t.Fatalf("第二条公告投递 %d 条，want ≥ 2", otherDelivered)
	}

	// 并行测试包可能同时增删用户；只断言「至少覆盖本用例的两个启用用户」，
	// 停用用户不投递，审计计数与落库行数一致（同一事实的两个投影）。
	var delivered int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1 AND channel = 'inapp' AND status = 'sent' AND read_at IS NULL`,
		"announcement:"+id+":%").Scan(&delivered); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if delivered < 2 {
		t.Fatalf("投递 %d 条，want ≥ 本用例的 2 个启用用户", delivered)
	}
	var disabledRows int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1 AND recipient_user_id = $2`,
		"announcement:"+id+":%", disabledID).Scan(&disabledRows); err != nil {
		t.Fatalf("count disabled deliveries: %v", err)
	}
	if disabledRows != 0 {
		t.Fatalf("停用用户收到 %d 条投递", disabledRows)
	}
	// 负载带标题/摘要/渲染后的正文，且摘要已从 Markdown 收敛为纯文本。
	var payload []byte
	if err := env.db.QueryRow(`SELECT payload FROM notification_deliveries
		WHERE dedupe_key LIKE $1 LIMIT 1`, "announcement:"+id+":%").Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var decoded notify.InAppPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.Title != "A5" || strings.Contains(decoded.Summary, "*") {
		t.Fatalf("payload = %+v", decoded)
	}
	// 深链必须落在 D7b 公开详情页：基址未配置时是站内相对路径，不能是
	// 旧的 '/'（那会把用户带回首页，S4 前端深链因此失效）。
	if want := "/announcements/" + id; decoded.LinkURL != want {
		t.Fatalf("payload.link_url = %q，want %q", decoded.LinkURL, want)
	}
	// body 来自已播种的 system.announcement 模板；为空说明播种/渲染链路断了，
	// 用户虽收到站内信却看不到正文。模板正文以标题开头，因此同时断言标题。
	if strings.TrimSpace(decoded.Body) == "" {
		t.Fatalf("payload.body 为空（模板未播种或渲染失败）：%+v", decoded)
	}
	if !strings.Contains(decoded.Body, "A5") {
		t.Fatalf("payload.body 未包含公告标题：%q", decoded.Body)
	}

	if env.logs != nil {
		var rawDetail string
		if err := env.logs.QueryRow(`SELECT detail::text FROM audit_logs
			WHERE detail->>'action' = $1 AND detail->>'id' = $2
			ORDER BY id DESC LIMIT 1`, audit.ActionAnnouncementPublish, id).Scan(&rawDetail); err != nil {
			t.Fatalf("query publish audit: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(rawDetail), &detail); err != nil {
			t.Fatalf("decode audit detail: %v", err)
		}
		count, ok := detail["delivered_users"].(float64)
		if !ok || int(count) != delivered {
			t.Fatalf("审计 delivered_users = %v，want 落库行数 %d（detail=%s）", detail["delivered_users"], delivered, rawDetail)
		}
		// 成功路径的失败计数必须存在且为 0：缺失会让运维无法区分「全成功」
		// 与「旧版本没有该字段」。
		failed, ok := detail["delivery_failed"].(float64)
		if !ok || int(failed) != 0 {
			t.Fatalf("审计 delivery_failed = %v，want 0（detail=%s）", detail["delivery_failed"], rawDetail)
		}
		if strings.Contains(rawDetail, "maintenance") {
			t.Fatalf("审计 detail 含正文：%s", rawDetail)
		}
	}

	// 删除公告：同一事务清理投递。
	if got := doAnnouncementRequest(t, mux, env.adminPrincipal(), http.MethodDelete,
		"/api/announcements/"+id, nil); got.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", got.Code)
	}
	var remaining int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1`, "announcement:"+id+":%").Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("删除公告后残留 %d 条投递", remaining)
	}
	// 负向对照：另一条公告的投递必须原样保留（前缀级联清理不得误伤）。
	var otherRemaining int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1`, "announcement:"+otherID+":%").Scan(&otherRemaining); err != nil {
		t.Fatalf("count other remaining: %v", err)
	}
	if otherRemaining != otherDelivered {
		t.Fatalf("另一条公告投递被误删：%d -> %d", otherDelivered, otherRemaining)
	}
}

// TestAnnouncementIntegrationUpdatePublishNowDelivers：PUT + publish_now 的
// 首次发布（编辑草稿并原子发布）同样走站内投递，且审计带投递计数。
func TestAnnouncementIntegrationUpdatePublishNowDelivers(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	_, mux := newA5AnnouncementRouter(t, env)

	// 本用例需要一个启用用户作为收件人；并行测试会增删用户，只断言本行。
	username := fmt.Sprintf("a5u_%d_%d", time.Now().UnixNano(), a5UserSeq.Add(1))
	var userID int64
	if err := env.db.QueryRow(`INSERT INTO users (username, language, status)
		VALUES ($1, 'en', 'enabled') RETURNING id`, username).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM users WHERE id = $1`, userID) })

	draft := env.createAnnouncement(t, map[string]any{"title": "Draft only", "content": "v1"})
	id := draft["id"].(string)
	t.Cleanup(func() {
		_, _ = env.db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+id+":%")
	})
	var before int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1`, "announcement:"+id+":%").Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 0 {
		t.Fatalf("草稿阶段不应有投递，实际 %d 条", before)
	}

	got := doAnnouncementRequest(t, mux, env.adminPrincipal(), http.MethodPut,
		"/api/announcements/"+id, map[string]any{
			"title": "Published by PUT", "content": "**maintenance** now", "publish_now": true,
		})
	if got.Code != http.StatusOK {
		t.Fatalf("PUT publish_now = %d（body=%s）", got.Code, got.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["status"] != store.AnnouncementStatusPublished {
		t.Fatalf("PUT publish_now 后 status = %v，want published", view["status"])
	}

	var delivered int
	if err := env.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key = $1`, "announcement:"+id+":"+fmt.Sprint(userID)).Scan(&delivered); err != nil {
		t.Fatalf("count delivery: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("PUT publish_now 后本用户投递 %d 条，want 1", delivered)
	}
	var payload []byte
	if err := env.db.QueryRow(`SELECT payload FROM notification_deliveries
		WHERE dedupe_key = $1`, "announcement:"+id+":"+fmt.Sprint(userID)).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var decoded notify.InAppPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.Title != "Published by PUT" || strings.TrimSpace(decoded.Body) == "" {
		t.Fatalf("PUT 首次发布 payload = %+v", decoded)
	}

	if env.logs != nil {
		var rawDetail string
		if err := env.logs.QueryRow(`SELECT detail::text FROM audit_logs
			WHERE detail->>'action' = $1 AND detail->>'id' = $2
			ORDER BY id DESC LIMIT 1`, audit.ActionAnnouncementPublish, id).Scan(&rawDetail); err != nil {
			t.Fatalf("query publish audit: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(rawDetail), &detail); err != nil {
			t.Fatalf("decode audit detail: %v", err)
		}
		if _, ok := detail["delivered_users"].(float64); !ok {
			t.Fatalf("PUT 发布审计缺 delivered_users：%s", rawDetail)
		}
		if failed, ok := detail["delivery_failed"].(float64); !ok || failed != 0 {
			t.Fatalf("PUT 发布审计 delivery_failed = %v，want 0：%s", detail["delivery_failed"], rawDetail)
		}
	}
}

// ---------------------------------------------------------------------------
// D7b 公开只读详情
// ---------------------------------------------------------------------------

// TestAnnouncementIntegrationPublicDetailVisibility 钉死公开详情的可见性矩阵：
// 已发布且生效 → 200；draft/未来/过期/offline/已删除/不存在/畸形 id → 同一个
// 404（不泄露存在性）。真实 SQL 谓词由 store.GetPublic 执行。
func TestAnnouncementIntegrationPublicDetailVisibility(t *testing.T) {
	env := newAnnouncementIntegrationEnv(t)
	admin := env.adminPrincipal()

	// 可见：正文含原始 HTML 与 javascript: 链接，公开响应必须已净化。
	visible := env.createAnnouncement(t, map[string]any{
		"title":       "Public detail",
		"content":     "## Heading\n\n[bad](javascript:alert(1))\n\n<script>alert(1)</script>",
		"publish_now": true,
	})
	visibleID := visible["id"].(string)
	// 公开读取不得写审计：先按公告 id 记录现有审计行数（创建/发布已各写
	// 一条），读取后再比对。按 id 计数而非全表，避免并行用例互相干扰。
	var auditBefore int
	if env.logs != nil {
		if err := env.logs.QueryRow(`SELECT count(*) FROM audit_logs WHERE detail->>'id' = $1`,
			visibleID).Scan(&auditBefore); err != nil {
			t.Fatalf("count audit before public read: %v", err)
		}
	}
	got := env.do(t, nil, http.MethodGet, "/api/announcements/public/"+visibleID, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("可见公告 = %d，want 200（body=%s）", got.Code, got.Body.String())
	}
	if cc := got.Header().Get("Cache-Control"); cc != publicAnnouncementCacheControl {
		t.Fatalf("Cache-Control = %q，want %q", cc, publicAnnouncementCacheControl)
	}
	var pub map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode public detail: %v", err)
	}
	for _, key := range []string{"id", "title", "category", "content_html", "published_at", "expire_at"} {
		if _, ok := pub[key]; !ok {
			t.Fatalf("公开响应缺少 %q：%s", key, got.Body.String())
		}
	}
	html := pub["content_html"].(string)
	if !strings.Contains(html, "<h2>Heading</h2>") {
		t.Fatalf("content_html 未渲染标题：%s", html)
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "javascript:") {
		t.Fatalf("content_html 未净化：%s", html)
	}
	// 公开读取不写已读行（匿名没有用户维度）。
	var reads int
	if err := env.db.QueryRow(`SELECT count(*) FROM announcement_reads WHERE announcement_id = $1`,
		visibleID).Scan(&reads); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	if reads != 0 {
		t.Fatalf("公开读取写入了 %d 条已读记录", reads)
	}
	// 匿名只读同样不写审计（除 announcement_reads 外的表不应新增行）。
	if env.logs != nil {
		var auditAfter int
		if err := env.logs.QueryRow(`SELECT count(*) FROM audit_logs WHERE detail->>'id' = $1`,
			visibleID).Scan(&auditAfter); err != nil {
			t.Fatalf("count audit after public read: %v", err)
		}
		if auditAfter != auditBefore {
			t.Fatalf("公开读取新增了审计行：%d -> %d", auditBefore, auditAfter)
		}
	}

	// 草稿不可见。
	draft := env.createAnnouncement(t, map[string]any{"title": "Draft", "content": "x"})

	// 已发布但 publish_at 在未来。
	future := env.createAnnouncement(t, map[string]any{
		"title": "Future", "content": "x", "publish_now": true,
		"publish_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})

	// 已发布但已过期。
	expiredAt := time.Now().UTC().Add(-time.Hour)
	expired := env.createAnnouncement(t, map[string]any{
		"title": "Expired", "content": "x", "publish_now": true,
		"publish_at": expiredAt.Add(-time.Hour).Format(time.RFC3339),
		"expire_at":  expiredAt.Format(time.RFC3339),
	})

	// 已下线。
	offline := env.createAnnouncement(t, map[string]any{
		"title": "Offline", "content": "x", "publish_now": true,
	})
	offlineID := offline["id"].(string)
	if r := env.do(t, admin, http.MethodPost,
		"/api/announcements/"+offlineID+"/offline", nil); r.Code != http.StatusOK {
		t.Fatalf("offline = %d，want 200", r.Code)
	}

	// 已删除。
	deleted := env.createAnnouncement(t, map[string]any{
		"title": "Deleted", "content": "x", "publish_now": true,
	})
	deletedID := deleted["id"].(string)
	if r := env.do(t, admin, http.MethodDelete,
		"/api/announcements/"+deletedID, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete = %d，want 204", r.Code)
	}

	invisible := []struct {
		name string
		id   string
	}{
		{"draft", draft["id"].(string)},
		{"future", future["id"].(string)},
		{"expired", expired["id"].(string)},
		{"offline", offlineID},
		{"deleted", deletedID},
		{"unknown", "22222222-2222-4222-8222-222222222222"},
		{"malformed", "not-a-uuid"},
	}
	var firstBody []byte
	for _, tc := range invisible {
		r := env.do(t, nil, http.MethodGet, "/api/announcements/public/"+tc.id, nil)
		if r.Code != http.StatusNotFound {
			t.Fatalf("%s = %d，want 404（body=%s）", tc.name, r.Code, r.Body.String())
		}
		if ct := r.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("%s Content-Type = %q，want problem+json", tc.name, ct)
		}
		if firstBody == nil {
			firstBody = append([]byte(nil), r.Body.Bytes()...)
		} else if !bytes.Equal(firstBody, r.Body.Bytes()) {
			// 所有不可见分支必须逐字节相同：任何差异都是存在性探测信道。
			t.Fatalf("%s 的 404 与其它分支不一致：\n got=%s\nwant=%s",
				tc.name, r.Body.String(), firstBody)
		}
	}
}
