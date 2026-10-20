//go:build integration

package notify

// A5（公告站内通知投递）的真实 PostgreSQL 验收：
//   - 为每个启用用户生成 channel='inapp' 的投递记录，按 ≤500 分批；
//   - dedupe_key 幂等：重复发布只增加 Duplicates，不产生第二条未读；
//   - 用户侧读取只返回本人记录，已读标记幂等、他人记录 404；
//   - 无通知用户返回空列表（空态）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_a5?sslmode=disable' \
//	  go test -tags=integration ./internal/notify/ -run TestAnnouncementInApp -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// userSeq 保证并发/快速连续测试的用户名唯一（users.username 有唯一约束）。
var userSeq int64

// insertA5User 插入一个专用用户并登记清理。
func insertA5User(t *testing.T, db *sql.DB, language, status string) int64 {
	t.Helper()
	username := fmt.Sprintf("a5_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&userSeq, 1))
	var id int64
	if err := db.QueryRow(`INSERT INTO users (username, language, status)
		VALUES ($1, $2, $3) RETURNING id`, username, language, status).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id = $1`, id) })
	return id
}

func TestAnnouncementInAppDeliveryBatchesAndDedupes(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// 把启用用户补到 >500，验证跨批（≤500/批）拼接正确。
	var enabled int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE status = 'enabled'`).Scan(&enabled); err != nil {
		t.Fatalf("count enabled users: %v", err)
	}
	for i := enabled; i < 501; i++ {
		insertA5User(t, db, "en", "enabled")
	}
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE status = 'enabled'`).Scan(&enabled); err != nil {
		t.Fatalf("recount enabled users: %v", err)
	}
	// 停用用户不得收到投递。
	disabledID := insertA5User(t, db, "zh-CN", "disabled")
	// 中文用户用于断言 locale 选择（追加在计数之后，故同步 +1）。
	zhID := insertA5User(t, db, "zh-CN", "enabled")
	enabled++

	const announcementID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
		"announcement:"+announcementID+":%")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+announcementID+":%")
	})

	input := AnnouncementDeliveryInput{
		AnnouncementID: announcementID,
		Title:          "维护通知",
		Summary:        "今晚维护",
		LinkURL:        "/",
		PublishedAt:    time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC),
	}
	first, err := service.DeliverAnnouncementInApp(ctx, input)
	if err != nil {
		t.Fatalf("DeliverAnnouncementInApp: %v", err)
	}
	if first.Failed != 0 || first.Delivered == 0 {
		t.Fatalf("首次投递 = %+v，want delivered>0 且无失败", first)
	}
	firstRows := first.Delivered
	// 停用用户没有记录。
	var disabledRows int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE recipient_user_id = $1`, disabledID).Scan(&disabledRows); err != nil {
		t.Fatalf("count disabled: %v", err)
	}
	if disabledRows != 0 {
		t.Fatalf("停用用户收到 %d 条投递", disabledRows)
	}
	// 任意一条中文用户的记录：locale=zh-CN，状态 sent，payload 带标题。
	var (
		locale, status, title string
		payload               []byte
		readAt                *time.Time
	)
	if err := db.QueryRow(`SELECT locale, status, payload, read_at
		FROM notification_deliveries WHERE recipient_user_id = $1
		AND dedupe_key = $2`, zhID, "announcement:"+announcementID+":"+fmt.Sprint(zhID)).
		Scan(&locale, &status, &payload, &readAt); err != nil {
		t.Fatalf("read zh delivery: %v", err)
	}
	var decoded InAppPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	title = decoded.Title
	if locale != "zh-CN" || status != "sent" || readAt != nil || title != input.Title {
		t.Fatalf("zh 记录 = locale=%s status=%s read_at=%v title=%s", locale, status, readAt, title)
	}

	// 重复投递：已投递用户全部命中去重。并行测试可能新增用户，故只断言
	// 「本批总数 == 当前启用用户数」且「已投递人数不增加」。
	var enabledNow int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE status = 'enabled'`).Scan(&enabledNow); err != nil {
		t.Fatalf("count enabled users: %v", err)
	}
	second, err := service.DeliverAnnouncementInApp(ctx, input)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if second.Failed != 0 || second.Delivered+second.Duplicates != enabledNow {
		t.Fatalf("重复投递 = %+v，want 本批 %d 全部处理", second, enabledNow)
	}
	if second.Duplicates < firstRows {
		t.Fatalf("重复投递 duplicates=%d，want ≥ 首批 %d", second.Duplicates, firstRows)
	}
	var total int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE dedupe_key LIKE $1`, "announcement:"+announcementID+":%").Scan(&total); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if expected := firstRows + second.Delivered; total != expected {
		t.Fatalf("去重后 %d 行，want %d", total, expected)
	}
}

func TestUserInAppNotificationsIsolationAndIdempotentRead(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	userA := insertA5User(t, db, "en", "enabled")
	userB := insertA5User(t, db, "en", "enabled")
	const announcementID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
		"announcement:"+announcementID+":%")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+announcementID+":%")
	})

	payload, err := json.Marshal(InAppPayload{
		AnnouncementID: announcementID, Title: "hello", Summary: "world",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	// 读取面按 payload->>'announcement_id' 关联 announcements；这里插入一条
	// 生效中的公告，保证该投递在隔离/已读用例里可见。
	insertA5Announcement(t, db, announcementID, "published", time.Now().Add(-time.Hour), nil)
	locale := LocaleEN
	items := []Delivery{
		{TemplateKey: stringPtr("system.announcement"), Category: "system.announcement",
			Channel: ChannelInApp, Recipient: fmt.Sprintf("user:%d", userA),
			RecipientUserID: &userA, Locale: &locale, Status: "sent", Attempt: 1,
			DedupeKey: "announcement:" + announcementID + ":" + fmt.Sprint(userA), Payload: payload},
		{TemplateKey: stringPtr("system.announcement"), Category: "system.announcement",
			Channel: ChannelInApp, Recipient: fmt.Sprintf("user:%d", userB),
			RecipientUserID: &userB, Locale: &locale, Status: "sent", Attempt: 1,
			DedupeKey: "announcement:" + announcementID + ":" + fmt.Sprint(userB), Payload: payload},
	}
	inserted, err := service.store.InsertInAppDeliveries(ctx, items)
	if err != nil || inserted != 2 {
		t.Fatalf("insert deliveries = %d/%v，want 2", inserted, err)
	}

	list, total, err := service.ListUserNotifications(ctx, userA, 1, 20)
	if err != nil {
		t.Fatalf("ListUserNotifications: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].Title != "hello" || list[0].ReadAt != nil {
		t.Fatalf("用户 A 列表 = %+v total=%d", list, total)
	}
	// D7b：读取面把 payload.announcement_id 下发给前端拼详情深链。
	if list[0].AnnouncementID != announcementID {
		t.Fatalf("announcement_id = %q，want %q", list[0].AnnouncementID, announcementID)
	}
	otherID := int64(0)
	if err := db.QueryRow(`SELECT id FROM notification_deliveries WHERE recipient_user_id = $1`,
		userB).Scan(&otherID); err != nil {
		t.Fatalf("read B id: %v", err)
	}
	if err := service.MarkUserNotificationRead(ctx, userA, otherID, time.Now()); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("标记他人通知 = %v，want ErrDeliveryNotFound", err)
	}

	ownID := list[0].ID
	firstRead := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if err := service.MarkUserNotificationRead(ctx, userA, ownID, firstRead); err != nil {
		t.Fatalf("mark own read: %v", err)
	}
	// 幂等：第二次不报错，read_at 保持第一次的值。
	if err := service.MarkUserNotificationRead(ctx, userA, ownID, firstRead.Add(time.Hour)); err != nil {
		t.Fatalf("second mark read: %v", err)
	}
	list, _, err = service.ListUserNotifications(ctx, userA, 1, 20)
	if err != nil {
		t.Fatalf("list after read: %v", err)
	}
	if list[0].ReadAt == nil || !list[0].ReadAt.Equal(firstRead) {
		t.Fatalf("read_at = %v，want %v（幂等）", list[0].ReadAt, firstRead)
	}

	// 无通知用户：空列表（空态），但 total 为 0。
	emptyUser := insertA5User(t, db, "en", "enabled")
	empty, emptyTotal, err := service.ListUserNotifications(ctx, emptyUser, 1, 20)
	if err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if len(empty) != 0 || emptyTotal != 0 {
		t.Fatalf("空态 = %+v total=%d", empty, emptyTotal)
	}
}

// insertA5Announcement 插入一条公告并登记清理；id 必须是合法 uuid。
func insertA5Announcement(t *testing.T, db *sql.DB, id, status string, publishAt time.Time, expireAt *time.Time) {
	t.Helper()
	var expire any
	if expireAt != nil {
		expire = expireAt.UTC()
	}
	if _, err := db.Exec(`INSERT INTO announcements
		(id, title, content, publish_at, expire_at, status, created_by, updated_by)
		VALUES ($1, 'a5 test', 'body', $2, $3, $4, 1, 1)`,
		id, publishAt.UTC(), expire, status); err != nil {
		t.Fatalf("insert announcement %s: %v", id, err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM announcements WHERE id = $1`, id) })
}

// TestUserInAppNotificationsHideInactiveAnnouncements 覆盖 G-DOM-37 ⑧：
// 公告下线、虚拟过期、尚未到 publish_at、已物理删除时，其站内信不再返回；
// 无 announcement_id 的普通投递不受影响，total 与分页口径同步收缩。
func TestUserInAppNotificationsHideInactiveAnnouncements(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	userID := insertA5User(t, db, "en", "enabled")
	now := time.Now().UTC()

	activeID := "11111111-1111-4111-8111-111111111111"
	futureID := "22222222-2222-4222-8222-222222222222"
	expiredID := "33333333-3333-4333-8333-333333333333"
	offlineID := "44444444-4444-4444-8444-444444444444"
	deletedID := "55555555-5555-4555-8555-555555555555"

	insertA5Announcement(t, db, activeID, "published", now.Add(-time.Hour), nil)
	insertA5Announcement(t, db, futureID, "published", now.Add(time.Hour), nil)
	expiredAt := now.Add(-30 * time.Minute)
	insertA5Announcement(t, db, expiredID, "published", now.Add(-2*time.Hour), &expiredAt)
	insertA5Announcement(t, db, offlineID, "offline", now.Add(-time.Hour), nil)
	insertA5Announcement(t, db, deletedID, "published", now.Add(-time.Hour), nil)

	locale := LocaleEN
	makeDelivery := func(announcementID, dedupeSuffix, title string) Delivery {
		raw, err := json.Marshal(InAppPayload{AnnouncementID: announcementID, Title: title})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		return Delivery{
			TemplateKey:     stringPtr("system.announcement"),
			Category:        "system.announcement",
			Channel:         ChannelInApp,
			Recipient:       fmt.Sprintf("user:%d", userID),
			RecipientUserID: &userID,
			Locale:          &locale,
			Status:          "sent",
			Attempt:         1,
			DedupeKey:       "announcement:" + announcementID + ":" + dedupeSuffix,
			Payload:         raw,
		}
	}
	ordinaryPayload, err := json.Marshal(InAppPayload{Title: "ordinary"})
	if err != nil {
		t.Fatalf("marshal ordinary payload: %v", err)
	}
	items := []Delivery{
		makeDelivery(activeID, "1", "active"),
		makeDelivery(futureID, "2", "future"),
		makeDelivery(expiredID, "3", "expired"),
		makeDelivery(offlineID, "4", "offline"),
		makeDelivery(deletedID, "5", "deleted"),
		{
			TemplateKey:     stringPtr("billing.balance_low"),
			Category:        "billing.balance_low",
			Channel:         ChannelInApp,
			Recipient:       fmt.Sprintf("user:%d", userID),
			RecipientUserID: &userID,
			Locale:          &locale,
			Status:          "sent",
			Attempt:         1,
			DedupeKey:       "ordinary:" + fmt.Sprint(userID),
			Payload:         ordinaryPayload,
		},
	}
	if inserted, err := service.store.InsertInAppDeliveries(ctx, items); err != nil || inserted != len(items) {
		t.Fatalf("insert deliveries = %d/%v，want %d", inserted, err, len(items))
	}
	// 物理删除公告行，模拟「投递行仍在、公告已不在」的残余（正常删除路径
	// 会由 announcement_repo.Delete 同事务清理投递，读取面仍需兜底）。
	if _, err := db.Exec(`DELETE FROM announcements WHERE id = $1`, deletedID); err != nil {
		t.Fatalf("delete announcement: %v", err)
	}

	list, total, err := service.ListUserNotifications(ctx, userID, 1, 20)
	if err != nil {
		t.Fatalf("ListUserNotifications: %v", err)
	}
	if total != 2 || len(list) != 2 {
		t.Fatalf("可见通知 total=%d len=%d，want 2（active + ordinary）：%+v", total, len(list), list)
	}
	got := map[string]bool{}
	for _, item := range list {
		got[item.Title] = true
	}
	if !got["active"] || !got["ordinary"] {
		t.Fatalf("可见标题 = %v，want active + ordinary", got)
	}

	// 分页口径：第二页仍只有 1 条（total 已按可见集收缩）。
	second, secondTotal, err := service.ListUserNotifications(ctx, userID, 2, 1)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if secondTotal != 2 || len(second) != 1 {
		t.Fatalf("第二页 = %+v total=%d，want 1/2", second, secondTotal)
	}
}

// TestAnnouncementInAppAudienceKeysetCoversLargeAudience 覆盖 G-DOM-37 ⑩：
// 受众 >1500 时按 users.id keyset 分页（500/页），投递不得漏人、不得重复。
func TestAnnouncementInAppAudienceKeysetCoversLargeAudience(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	const bulkUsers = 1501
	if _, err := db.Exec(`INSERT INTO users (username, language, status)
		SELECT 'a5_bulk_' || g, 'en', 'enabled' FROM generate_series(1, $1) AS g`, bulkUsers); err != nil {
		t.Fatalf("insert bulk users: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM users WHERE username LIKE 'a5_bulk_%'`)
	})

	var enabled int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE status = 'enabled'`).Scan(&enabled); err != nil {
		t.Fatalf("count enabled: %v", err)
	}
	if enabled <= 1500 {
		t.Fatalf("启用用户 = %d，want >1500（本用例需要跨 3 页以上）", enabled)
	}

	// 直接验证 keyset：严格递增、无重复、页数 = ceil(enabled/500)。
	seen := make(map[int64]bool, enabled)
	var (
		afterID int64
		pages   int
		prevID  int64
	)
	for {
		page, err := service.store.ListInAppAudience(ctx, afterID, 500)
		if err != nil {
			t.Fatalf("ListInAppAudience(after=%d): %v", afterID, err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for _, item := range page {
			if item.UserID <= prevID {
				t.Fatalf("keyset 非递增：%d 在 %d 之后", item.UserID, prevID)
			}
			if seen[item.UserID] {
				t.Fatalf("keyset 重复返回用户 %d", item.UserID)
			}
			seen[item.UserID] = true
			prevID = item.UserID
		}
		afterID = page[len(page)-1].UserID
	}
	if len(seen) != enabled {
		t.Fatalf("keyset 覆盖 %d 个用户，want %d", len(seen), enabled)
	}
	if wantPages := (enabled + 499) / 500; pages != wantPages {
		t.Fatalf("分页数 = %d，want %d", pages, wantPages)
	}

	const announcementID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
		"announcement:"+announcementID+":%")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE $1`,
			"announcement:"+announcementID+":%")
	})
	result, err := service.DeliverAnnouncementInApp(ctx, AnnouncementDeliveryInput{
		AnnouncementID: announcementID,
		Title:          "bulk",
		PublishedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("DeliverAnnouncementInApp: %v", err)
	}
	if result.Failed != 0 || result.Delivered != enabled {
		t.Fatalf("投递结果 = %+v，want delivered=%d failed=0（keyset 不得漏批）", result, enabled)
	}
	var covered int
	if err := db.QueryRow(`SELECT count(DISTINCT recipient_user_id)
		FROM notification_deliveries WHERE dedupe_key LIKE $1`,
		"announcement:"+announcementID+":%").Scan(&covered); err != nil {
		t.Fatalf("count covered: %v", err)
	}
	if covered != enabled {
		t.Fatalf("投递覆盖 = %d，want %d", covered, enabled)
	}
}

// TestUserInAppNotificationsAnnouncementIDExtraction 覆盖 D7b 的读取口径：
// announcement_id 由存储层用 payload->>'announcement_id' 单独提取，
// 公告投递下发 UUID，普通投递为空串，非法值（非字符串）折叠为空串且
// 不报错，不影响同页其它行。
func TestUserInAppNotificationsAnnouncementIDExtraction(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	userID := insertA5User(t, db, "en", "enabled")
	const announcementID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	insertA5Announcement(t, db, announcementID, "published", time.Now().Add(-time.Hour), nil)

	announcementPayload, err := json.Marshal(InAppPayload{
		AnnouncementID: announcementID, Title: "announcement",
	})
	if err != nil {
		t.Fatalf("marshal announcement payload: %v", err)
	}
	plainPayload, err := json.Marshal(InAppPayload{Title: "plain"})
	if err != nil {
		t.Fatalf("marshal plain payload: %v", err)
	}
	locale := LocaleEN
	newItem := func(key string, payload []byte) Delivery {
		return Delivery{
			TemplateKey: stringPtr("system.announcement"), Category: "system.announcement",
			Channel: ChannelInApp, Recipient: fmt.Sprintf("user:%d", userID),
			RecipientUserID: &userID, Locale: &locale, Status: "sent", Attempt: 1,
			DedupeKey: key, Payload: payload,
		}
	}
	items := []Delivery{
		newItem(fmt.Sprintf("d7b:announcement:%d", userID), announcementPayload),
		newItem(fmt.Sprintf("d7b:plain:%d", userID), plainPayload),
		// 对象但类型非法：jsonb_typeof != 'string' 时提取为 NULL（空串），
		// 整包解码失败只记 Warn，列表照常返回。
		newItem(fmt.Sprintf("d7b:invalid:%d", userID),
			[]byte(`{"announcement_id":{"nested":true},"title":"invalid"}`)),
	}
	inserted, err := service.store.InsertInAppDeliveries(ctx, items)
	if err != nil || inserted != len(items) {
		t.Fatalf("insert deliveries = %d/%v，want %d", inserted, err, len(items))
	}

	list, total, err := service.ListUserNotifications(ctx, userID, 1, 20)
	if err != nil {
		t.Fatalf("ListUserNotifications: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Fatalf("total=%d len=%d，want 3", total, len(list))
	}
	byTitle := make(map[string]UserNotificationView, len(list))
	emptyKey := 0
	for _, view := range list {
		if view.ID == 0 || view.CreatedAt.IsZero() {
			t.Fatalf("行缺少 id/created_at：%+v", view)
		}
		if view.AnnouncementID == "" {
			emptyKey++
		}
		byTitle[view.Title] = view
	}
	if got := byTitle["announcement"].AnnouncementID; got != announcementID {
		t.Fatalf("公告 announcement_id = %q，want %q", got, announcementID)
	}
	plain, ok := byTitle["plain"]
	if !ok {
		t.Fatalf("普通投递缺失：%+v", list)
	}
	if plain.AnnouncementID != "" {
		t.Fatalf("普通投递 announcement_id = %q，want 空串", plain.AnnouncementID)
	}
	// 普通投递与非法值两行都折叠为空串，且整页仍正常返回（不因解码失败 500）。
	if emptyKey != 2 {
		t.Fatalf("空 announcement_id 行数 = %d，want 2（列表=%+v）", emptyKey, list)
	}
}
