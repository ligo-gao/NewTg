//go:build integration

package store

// D7a 公告仓储的数据库级验收：状态机条件 UPDATE、虚拟过期筛选、active
// 查询与 read 幂等、删除同事务清已读。handler 层的行为断言在
// internal/httpapi/handlers_announcements_integration_test.go；这里直接对
// SQL 语义断言，避免一个 handler 装配错误掩盖仓储缺陷。
//
// 运行：
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_d7' \
//	  go test -tags=integration ./internal/store/ -run TestAnnouncement -v

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newAnnouncementRepoEnv(t *testing.T) *SQLAnnouncementRepo {
	t.Helper()
	db := openTestDB(t)
	return NewAnnouncementRepo(db)
}

func createRepoAnnouncement(t *testing.T, repo *SQLAnnouncementRepo, in AnnouncementInput) Announcement {
	t.Helper()
	mut, err := repo.Create(context.Background(), in, 1)
	if err != nil {
		t.Fatalf("Create(%+v): %v", in, err)
	}
	t.Cleanup(func() { _ = repo.Delete(context.Background(), mut.Announcement.ID) })
	return mut.Announcement
}

func TestAnnouncementRepoCreateDefaultsAndValidation(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)

	a := createRepoAnnouncement(t, repo, AnnouncementInput{Title: "  hello  ", Content: "x"})
	if a.Title != "hello" {
		t.Fatalf("title 未 trim：%q", a.Title)
	}
	if a.Status != AnnouncementStatusDraft {
		t.Fatalf("status = %q，want draft", a.Status)
	}
	if a.Category != AnnouncementCategoryNotice {
		t.Fatalf("category = %q，want notice", a.Category)
	}

	for _, in := range []AnnouncementInput{
		{Title: "", Content: "x"},
		{Title: "t", Content: "x", Category: "bogus"},
		{Title: "t", Content: "x", PublishAt: time.Now().Add(time.Hour),
			ExpireAt: ptrTime(time.Now().Add(-time.Hour))},
	} {
		if _, err := repo.Create(context.Background(), in, 1); !errors.Is(err, ErrAnnouncementInvalid) {
			t.Fatalf("Create(%+v) err = %v，want ErrAnnouncementInvalid", in, err)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestAnnouncementRepoStateMachine 覆盖条件 UPDATE 的迁移矩阵与幂等标记。
func TestAnnouncementRepoStateMachine(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)
	ctx := context.Background()

	a := createRepoAnnouncement(t, repo, AnnouncementInput{Title: "SM", Content: "x"})

	if _, err := repo.Offline(ctx, a.ID, 1); !errors.Is(err, ErrAnnouncementStateConflict) {
		t.Fatalf("draft→offline err = %v，want StateConflict", err)
	}

	mut, err := repo.Publish(ctx, a.ID, 1)
	if err != nil || !mut.Published || mut.Announcement.Status != AnnouncementStatusPublished {
		t.Fatalf("publish = %+v/%v", mut, err)
	}
	publishedAt := mut.Announcement.PublishedAt

	// 幂等：published → published 返回 200 语义且不重写 published_at。
	mut, err = repo.Publish(ctx, a.ID, 1)
	if err != nil || mut.Published {
		t.Fatalf("repeat publish = %+v/%v，want Published=false", mut, err)
	}
	if mut.Announcement.PublishedAt == nil || publishedAt == nil ||
		!mut.Announcement.PublishedAt.Equal(*publishedAt) {
		t.Fatalf("幂等 publish 改写了 published_at")
	}

	if _, err := repo.Offline(ctx, a.ID, 1); err != nil {
		t.Fatalf("published→offline: %v", err)
	}
	// offline → offline 幂等。
	if mut, err := repo.Offline(ctx, a.ID, 1); err != nil || mut.Announcement.Status != AnnouncementStatusOffline {
		t.Fatalf("repeat offline = %+v/%v", mut, err)
	}
}

// TestAnnouncementRepoUpdateN35 覆盖 N-35 的两条编辑语义。
func TestAnnouncementRepoUpdateN35(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, AnnouncementInput{
		Title: "N35", Content: "v1", PublishNow: true,
	}, 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	a := created.Announcement
	t.Cleanup(func() { _ = repo.Delete(ctx, a.ID) })

	future := time.Now().UTC().Add(48 * time.Hour)
	mut, err := repo.Update(ctx, a.ID, AnnouncementInput{
		Title: "N35 edited", Content: "v2", PublishAt: future, PublishNow: true,
	}, 2)
	if err != nil {
		t.Fatalf("Update published: %v", err)
	}
	got := mut.Announcement
	if got.Status != AnnouncementStatusPublished || mut.Published {
		t.Fatalf("已发布编辑应原地保持 published 且不触发副作用：%+v", mut)
	}
	if !got.PublishAt.Equal(a.PublishAt) {
		t.Fatalf("N-35：publish_at 被改写 %v -> %v", a.PublishAt, got.PublishAt)
	}
	if got.PublishedAt == nil || a.PublishedAt == nil || !got.PublishedAt.Equal(*a.PublishedAt) {
		t.Fatalf("N-35：published_at 被改写")
	}
	if got.UpdatedBy != 2 {
		t.Fatalf("updated_by = %d，want 2", got.UpdatedBy)
	}

	if _, err := repo.Offline(ctx, a.ID, 1); err != nil {
		t.Fatalf("Offline: %v", err)
	}
	mut, err = repo.Update(ctx, a.ID, AnnouncementInput{Title: "N35 draft", Content: "v3"}, 3)
	if err != nil {
		t.Fatalf("Update offline: %v", err)
	}
	if mut.Announcement.Status != AnnouncementStatusDraft {
		t.Fatalf("offline 编辑后 status = %q，want draft", mut.Announcement.Status)
	}
	if mut.Announcement.OfflineAt != nil {
		t.Fatalf("offline 编辑后 offline_at 应清空：%v", mut.Announcement.OfflineAt)
	}
}

// TestAnnouncementRepoListVirtualExpired 覆盖虚拟过期筛选与 published 互斥。
func TestAnnouncementRepoListVirtualExpired(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-2 * time.Hour)
	expiredAt := time.Now().UTC().Add(-time.Hour)
	expired := createRepoAnnouncement(t, repo, AnnouncementInput{
		Title: "expired", Content: "x", PublishAt: past,
		ExpireAt: &expiredAt, PublishNow: true,
	})
	live := createRepoAnnouncement(t, repo, AnnouncementInput{
		Title: "live", Content: "x", PublishNow: true,
	})

	ids := func(status string) map[string]bool {
		items, _, err := repo.List(ctx, AnnouncementFilter{Status: status, PageSize: 100})
		if err != nil {
			t.Fatalf("List(%s): %v", status, err)
		}
		out := map[string]bool{}
		for _, item := range items {
			out[item.ID] = true
		}
		return out
	}
	if !ids(AnnouncementDisplayExpired)[expired.ID] {
		t.Fatal("expired 筛选未包含过期公告")
	}
	if ids(AnnouncementStatusPublished)[expired.ID] {
		t.Fatal("published 筛选不应包含已过期公告")
	}
	if !ids(AnnouncementStatusPublished)[live.ID] {
		t.Fatal("published 筛选未包含生效公告")
	}

	// 排序：publish_at DESC（live 的 publish_at 更晚，排在前）。
	items, total, err := repo.List(ctx, AnnouncementFilter{PageSize: 100})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if total < 2 || len(items) < 2 {
		t.Fatalf("total=%d items=%d", total, len(items))
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].PublishAt.Before(items[i].PublishAt) {
			t.Fatal("列表未按 publish_at DESC 排序")
		}
	}

	if _, _, err := repo.List(ctx, AnnouncementFilter{Status: "bogus"}); !errors.Is(err, ErrAnnouncementInvalid) {
		t.Fatalf("未知 status err = %v，want ErrAnnouncementInvalid", err)
	}
}

// TestAnnouncementRepoActiveAndRead 覆盖 active 谓词、read 标记与幂等。
func TestAnnouncementRepoActiveAndRead(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)
	ctx := context.Background()

	active := createRepoAnnouncement(t, repo, AnnouncementInput{
		Title: "active", Content: "x", PublishNow: true,
	})
	future := createRepoAnnouncement(t, repo, AnnouncementInput{
		Title: "future", Content: "x", PublishNow: true,
		PublishAt: time.Now().UTC().Add(24 * time.Hour),
	})
	draft := createRepoAnnouncement(t, repo, AnnouncementInput{Title: "draft", Content: "x"})
	// 已过期（虚拟态）：active 必须排除。
	expiredAt := time.Now().UTC().Add(-time.Minute)
	expired := createRepoAnnouncement(t, repo, AnnouncementInput{
		Title: "expired", Content: "x", PublishNow: true,
		PublishAt: time.Now().UTC().Add(-time.Hour), ExpireAt: &expiredAt,
	})

	const userID int64 = 987654321 // 无 FK，任意 bigint 即可
	t.Cleanup(func() {
		_, _ = repo.db.Exec(`DELETE FROM announcement_reads WHERE user_id = $1`, userID)
	})

	items, err := repo.ListActive(ctx, userID, time.Now().UTC())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	seen := map[string]Announcement{}
	for _, item := range items {
		seen[item.ID] = item
	}
	if _, ok := seen[active.ID]; !ok {
		t.Fatal("active 缺生效公告")
	}
	if _, ok := seen[future.ID]; ok {
		t.Fatal("active 不应包含未来生效公告")
	}
	if _, ok := seen[draft.ID]; ok {
		t.Fatal("active 不应包含草稿")
	}
	if _, ok := seen[expired.ID]; ok {
		t.Fatal("active 不应包含已过期公告")
	}
	if seen[active.ID].Read {
		t.Fatal("未读公告 read 应为 false")
	}

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if err := repo.MarkRead(ctx, userID, active.ID, now); err != nil {
			t.Fatalf("MarkRead 第 %d 次: %v", i+1, err)
		}
	}
	var reads int
	if err := repo.db.QueryRow(`SELECT count(*) FROM announcement_reads
		WHERE user_id = $1 AND announcement_id = $2`, userID, active.ID).Scan(&reads); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	if reads != 1 {
		t.Fatalf("幂等 read 落了 %d 行，want 1", reads)
	}
	items, err = repo.ListActive(ctx, userID, now)
	if err != nil {
		t.Fatalf("ListActive after read: %v", err)
	}
	for _, item := range items {
		if item.ID == active.ID && !item.Read {
			t.Fatal("已读公告 read 应为 true")
		}
	}

	if err := repo.MarkRead(ctx, userID, future.ID, now); !errors.Is(err, ErrAnnouncementNotFound) {
		t.Fatalf("未来公告 MarkRead err = %v，want NotFound", err)
	}
	if err := repo.MarkRead(ctx, userID, future.ID, future.PublishAt.Add(time.Hour)); err != nil {
		t.Fatalf("生效后 MarkRead: %v", err)
	}
	_, _ = repo.db.Exec(`DELETE FROM announcement_reads WHERE user_id = $1`, userID)
}

// TestAnnouncementRepoDeleteCascadesReads 覆盖删除同事务清已读与幂等形态。
func TestAnnouncementRepoDeleteCascadesReads(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)
	ctx := context.Background()

	a := createRepoAnnouncement(t, repo, AnnouncementInput{Title: "del", Content: "x", PublishNow: true})
	const userID int64 = 987654322
	t.Cleanup(func() {
		_, _ = repo.db.Exec(`DELETE FROM announcement_reads WHERE user_id = $1`, userID)
	})
	if err := repo.MarkRead(ctx, userID, a.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	// A5：公告的站内投递记录也必须在同一事务内清理（dedupe 前缀匹配）。
	t.Cleanup(func() {
		_, _ = repo.db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+a.ID+":%")
	})
	if _, err := repo.db.Exec(`INSERT INTO notification_deliveries
		(template_key, category, channel, recipient, recipient_user_id, dedupe_key, status, payload)
		VALUES ('system.announcement', 'system.announcement', 'inapp', $1, $2, $3, 'sent', '{"title":"x"}')`,
		"user:987654322", userID, "announcement:"+a.ID+":987654322"); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var deliveries int
	if err := repo.db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1`, "announcement:"+a.ID+":%").Scan(&deliveries); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if deliveries != 0 {
		t.Fatalf("删除后残留 %d 条站内投递", deliveries)
	}
	var reads int
	if err := repo.db.QueryRow(`SELECT count(*) FROM announcement_reads WHERE announcement_id = $1`,
		a.ID).Scan(&reads); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	if reads != 0 {
		t.Fatalf("删除后残留 %d 条已读", reads)
	}
	if _, err := repo.Get(ctx, a.ID); !errors.Is(err, ErrAnnouncementNotFound) {
		t.Fatalf("删除后 Get err = %v，want NotFound", err)
	}
	if err := repo.Delete(ctx, a.ID); !errors.Is(err, ErrAnnouncementNotFound) {
		t.Fatalf("重复 Delete err = %v，want NotFound", err)
	}
}

// TestAnnouncementRepoInvalidID 钉死畸形 uuid 不会冒泡成 22P02。
func TestAnnouncementRepoInvalidID(t *testing.T) {
	repo := newAnnouncementRepoEnv(t)
	ctx := context.Background()

	if _, err := repo.Get(ctx, "not-a-uuid"); !errors.Is(err, ErrAnnouncementNotFound) {
		t.Fatalf("Get(not-a-uuid) err = %v，want NotFound", err)
	}
	if err := repo.Delete(ctx, "not-a-uuid"); !errors.Is(err, ErrAnnouncementNotFound) {
		t.Fatalf("Delete(not-a-uuid) err = %v，want NotFound", err)
	}
	if _, err := repo.Publish(ctx, "not-a-uuid", 1); !errors.Is(err, ErrAnnouncementNotFound) {
		t.Fatalf("Publish(not-a-uuid) err = %v，want NotFound", err)
	}
}

// TestAnnouncementRepoDBConstraints 证明绕过应用层直接写库时 CHECK 仍生效。
func TestAnnouncementRepoDBConstraints(t *testing.T) {
	db := openTestDB(t)
	// 空标题被 title_len_chk 拒绝。
	if _, err := db.Exec(`INSERT INTO announcements (title, content, created_by, updated_by)
		VALUES ('   ', 'x', 1, 1)`); err == nil {
		t.Fatal("空标题应被 CHECK 拒绝")
	}
	// 过期早于生效被 expire_after_publish_chk 拒绝。
	if _, err := db.Exec(`INSERT INTO announcements
		(title, content, publish_at, expire_at, created_by, updated_by)
		VALUES ('t', 'x', now(), now() - interval '1 hour', 1, 1)`); err == nil {
		t.Fatal("expire_at <= publish_at 应被 CHECK 拒绝")
	}
	// 未知状态被 status_chk 拒绝。
	if _, err := db.Exec(`INSERT INTO announcements (title, content, status, created_by, updated_by)
		VALUES ('t', 'x', 'expired', 1, 1)`); err == nil {
		t.Fatal("expired 是虚拟态，不得作为 DB 状态写入")
	}
}
