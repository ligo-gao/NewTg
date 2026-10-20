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

	payload, err := json.Marshal(InAppPayload{Title: "hello", Summary: "world"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
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
