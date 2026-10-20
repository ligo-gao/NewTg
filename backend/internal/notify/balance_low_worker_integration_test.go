//go:build integration

package notify

// B4 / N6 低余额扫描的真实 PostgreSQL 验收。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_b4?sslmode=disable' \
//	  go test -tags=integration ./internal/notify/ -run BalanceLow -v
//
// 覆盖：阈值命中/未命中、禁用/阈值 0/未订阅不扫描、按站点时区按天去重、
// 跨天可再发、余额回升后再跌破、site.timezone 从 cfg 读取与回退、查询阶段
// 排除当天已通知用户、投递行字段与载荷脱敏（无邮箱/手机号）。
//
// 【共享库隔离】本文件可能在存有其它集成用例数据的共享库上执行（例如同一
// 次 `go test ./internal/notify/ ./internal/httpapi/` 的另一个包）。因此：
//   - 所有投递行断言都按**本用例用户**的 `dedupe_key` 前缀过滤，绝不断言
//     `notification_deliveries` 全表为空或全表计数；
//   - 不断言 `Scanned`/`Enqueued` 的精确值（共享库里其它低余额用户也会
//     命中），只断言本用例用户的行为；`Failed` 仍要求为 0；
//   - 清理只删除 `balance_low:` 前缀的投递行与 `b4-` 前缀用户——这仍是对
//     全表的范围谓词，但不触碰其它类别/其它用例的数据。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

const balanceLowTestDay = "2026-10-20"

type balanceLowSubscription struct {
	events map[string]bool
}

func (s balanceLowSubscription) Subscribed(event string) bool { return s.events[event] }

func subscribedBalanceLow() balanceLowSubscription {
	return balanceLowSubscription{events: map[string]bool{"balance_low": true}}
}

// cleanBalanceLowFixtures 删除本文件拥有的用户与低余额投递行。
//
// 投递行没有「测试归属」列，只能用 `dedupe_key` 前缀做全表范围谓词；用户
// 按 `b4-` 前缀限定。删除是全局可见的，因此本文件的断言必须按用户过滤，
// 不能依赖全表计数（见文件头「共享库隔离」）。
func cleanBalanceLowFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM notification_deliveries WHERE dedupe_key LIKE 'balance_low:%'`); err != nil {
		t.Fatalf("clean balance_low deliveries: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE username LIKE 'b4-%'`); err != nil {
		t.Fatalf("clean users: %v", err)
	}
}

func insertBalanceLowUser(t *testing.T, db *sql.DB, username string, balance int64, status string) int64 {
	t.Helper()
	if status == "" {
		status = "enabled"
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO users (username, balance, status)
		VALUES ($1, $2, $3) RETURNING id`, username, balance, status).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

func newBalanceLowConfig(enabled bool, threshold int64, timezone string) mapConfig {
	cfg := mapConfig{
		KeyWebhookEnabled:             enabled,
		KeyWebhookBalanceLowThreshold: threshold,
	}
	if timezone != "" {
		cfg[KeySiteTimezone] = timezone
	}
	return cfg
}

func newBalanceLowWorkerForTest(t *testing.T, db *sql.DB, cfg mapConfig,
	subscription WebhookEventSubscription, now time.Time, loc *time.Location,
) *BalanceLowWorker {
	t.Helper()
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	producer, err := NewWebhookEventProducer(store, subscription, nil)
	if err != nil {
		t.Fatalf("NewWebhookEventProducer: %v", err)
	}
	worker, err := NewBalanceLowWorker(BalanceLowWorkerDeps{
		Store:        store,
		Producer:     producer,
		Config:       cfg,
		Subscription: subscription,
		Now:          func() time.Time { return now },
		Location:     loc,
	})
	if err != nil {
		t.Fatalf("NewBalanceLowWorker: %v", err)
	}
	return worker
}

type balanceLowDelivery struct {
	Category  string
	Channel   string
	Recipient string
	Status    string
	Dedupe    string
	Payload   []byte
}

// listBalanceLowDeliveriesForUser 只返回 dedupe_key 属于该用户的投递行。
// 共享库里其它用户的 balance_low 行不会进入结果，也因此不会造成假失败。
func listBalanceLowDeliveriesForUser(t *testing.T, db *sql.DB, userID int64) []balanceLowDelivery {
	t.Helper()
	rows, err := db.Query(`SELECT category, channel, recipient, status, dedupe_key, payload
		FROM notification_deliveries
		WHERE dedupe_key LIKE $1
		ORDER BY id`, fmt.Sprintf("balance_low:%d:%%", userID))
	if err != nil {
		t.Fatalf("list deliveries for user %d: %v", userID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []balanceLowDelivery
	for rows.Next() {
		var item balanceLowDelivery
		if err := rows.Scan(&item.Category, &item.Channel, &item.Recipient,
			&item.Status, &item.Dedupe, &item.Payload); err != nil {
			t.Fatalf("scan delivery: %v", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate deliveries: %v", err)
	}
	return out
}

func TestBalanceLowWorkerEnqueuesOnlyEnabledUsersBelowThreshold(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanBalanceLowFixtures(t, db)

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	below := insertBalanceLowUser(t, db, "b4-below", 500, "enabled")
	atThreshold := insertBalanceLowUser(t, db, "b4-at-threshold", 1000, "enabled")
	above := insertBalanceLowUser(t, db, "b4-above", 2000, "enabled")
	disabled := insertBalanceLowUser(t, db, "b4-disabled", 0, "disabled")

	worker := newBalanceLowWorkerForTest(t, db,
		newBalanceLowConfig(true, 1000, ""), subscribedBalanceLow(), now, nil)
	result := worker.RunOnce(context.Background())
	// 共享库里可能还有其它低余额用户，Scanned/Enqueued 的精确值不可依赖；
	// 本用例只要求没有入队失败且自己的用户被写入。
	if result.Failed != 0 || result.Enqueued < 1 {
		t.Fatalf("result = %+v，期望 failed=0 enqueued>=1", result)
	}

	items := listBalanceLowDeliveriesForUser(t, db, below)
	if len(items) != 1 {
		t.Fatalf("命中用户投递行数 = %d，期望 1", len(items))
	}
	item := items[0]
	if item.Category != "billing.balance_low" || item.Channel != "webhook" ||
		item.Status != "queued" || item.Recipient != "webhook" {
		t.Fatalf("投递行字段 = %+v", item)
	}
	wantDedupe := fmt.Sprintf("balance_low:%d:%s", below, balanceLowTestDay)
	if item.Dedupe != wantDedupe {
		t.Fatalf("dedupe_key = %q，期望 %q", item.Dedupe, wantDedupe)
	}

	var payload map[string]any
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		t.Fatalf("payload 不是 JSON: %v（%s）", err, item.Payload)
	}
	want := map[string]any{
		"event":       "balance_low",
		"user_id":     float64(below),
		"balance":     float64(500),
		"threshold":   float64(1000),
		"detected_at": "2026-10-20T12:00:00Z",
	}
	if len(payload) != len(want) {
		t.Fatalf("payload 字段数 = %d，期望 %d（%s）", len(payload), len(want), item.Payload)
	}
	for key, value := range want {
		if payload[key] != value {
			t.Fatalf("payload[%s] = %v，期望 %v（%s）", key, payload[key], value, item.Payload)
		}
	}
	// 脱敏：载荷不得携带邮箱、手机号或用户名。
	for _, forbidden := range []string{"email", "phone", "mobile", "username"} {
		if _, ok := payload[forbidden]; ok {
			t.Fatalf("payload 含 PII 字段 %q：%s", forbidden, item.Payload)
		}
	}

	// 阈值上沿、阈值之上与禁用用户都不得产生投递行（按用户断言）。
	for name, id := range map[string]int64{
		"at threshold": atThreshold,
		"above":        above,
		"disabled":     disabled,
	} {
		if got := listBalanceLowDeliveriesForUser(t, db, id); len(got) != 0 {
			t.Fatalf("%s 用户不应有投递行，实得 %d 行", name, len(got))
		}
	}
}

func TestBalanceLowWorkerSkipsWhenDisabledZeroOrUnsubscribed(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanBalanceLowFixtures(t, db)
	skipUser := insertBalanceLowUser(t, db, "b4-skip", 0, "enabled")
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		cfg  mapConfig
		sub  WebhookEventSubscription
	}{
		{"webhook disabled", newBalanceLowConfig(false, 1000, ""), subscribedBalanceLow()},
		{"threshold zero", newBalanceLowConfig(true, 0, ""), subscribedBalanceLow()},
		{"threshold negative treated as invalid", newBalanceLowConfig(true, -1, ""), subscribedBalanceLow()},
		{"event not subscribed", newBalanceLowConfig(true, 1000, ""),
			balanceLowSubscription{events: map[string]bool{"payment_success": true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			worker := newBalanceLowWorkerForTest(t, db, tc.cfg, tc.sub, now, nil)
			result := worker.RunOnce(context.Background())
			if !result.Skipped {
				t.Fatalf("result = %+v，期望 skipped", result)
			}
			if items := listBalanceLowDeliveriesForUser(t, db, skipUser); len(items) != 0 {
				t.Fatalf("不应产生本用例用户的投递行，实得 %d", len(items))
			}
		})
	}
}

func TestBalanceLowWorkerDedupesPerSiteDayAndResendsNextDay(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanBalanceLowFixtures(t, db)
	userID := insertBalanceLowUser(t, db, "b4-daily", 100, "enabled")

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	cfg := newBalanceLowConfig(true, 1000, "")

	// UTC 17:00 = 上海次日 01:00：dedupe 的「天」必须按站点时区取。
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	worker := newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), now, loc)

	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("首轮 result = %+v", result)
	}
	items := listBalanceLowDeliveriesForUser(t, db, userID)
	if len(items) != 1 || items[0].Dedupe != fmt.Sprintf("balance_low:%d:2026-10-21", userID) {
		t.Fatalf("首轮投递行 = %+v，期望上海日期 2026-10-21 且仅 1 行", items)
	}

	// 同一站点日的第二轮：该用户已被查询排除，不会新增行。
	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("同日第二轮 result = %+v", result)
	}
	if items := listBalanceLowDeliveriesForUser(t, db, userID); len(items) != 1 {
		t.Fatalf("同日第二轮后投递行 = %d，期望仍为 1", len(items))
	}

	// 次日（上海 10-22）可再发一条。
	next := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	worker = newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), next, loc)
	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("次日 result = %+v", result)
	}
	items = listBalanceLowDeliveriesForUser(t, db, userID)
	if len(items) != 2 || items[1].Dedupe != fmt.Sprintf("balance_low:%d:2026-10-22", userID) {
		t.Fatalf("跨天投递行 = %+v", items)
	}
}

func TestBalanceLowWorkerReadsSiteTimezoneFromConfig(t *testing.T) {
	db := openNotifyTestDB(t)
	now := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		set      bool
		timezone any
		wantDay  string
	}{
		// UTC 17:00 在上海已是次日 01:00。
		{"configured timezone", true, "Asia/Shanghai", "2026-10-21"},
		// 缺失 / 未知时区 / 非字符串一律回退 UTC：扫描可用且跨副本边界一致。
		{"missing timezone", false, nil, "2026-10-20"},
		{"unknown timezone", true, "Mars/Olympus", "2026-10-20"},
		{"non-string timezone", true, 123, "2026-10-20"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cleanBalanceLowFixtures(t, db)
			userID := insertBalanceLowUser(t, db, fmt.Sprintf("b4-tz-%d", i), 100, "enabled")
			cfg := newBalanceLowConfig(true, 1000, "")
			if tc.set {
				cfg[KeySiteTimezone] = tc.timezone
			}
			// Location 故意留空：这里验证的是从 cfg 读取 site.timezone 的路径。
			worker := newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), now, nil)
			if result := worker.RunOnce(context.Background()); result.Failed != 0 {
				t.Fatalf("result = %+v", result)
			}
			items := listBalanceLowDeliveriesForUser(t, db, userID)
			if len(items) != 1 {
				t.Fatalf("投递行数 = %d，期望 1", len(items))
			}
			wantDedupe := fmt.Sprintf("balance_low:%d:%s", userID, tc.wantDay)
			if items[0].Dedupe != wantDedupe {
				t.Fatalf("dedupe_key = %q，期望 %q", items[0].Dedupe, wantDedupe)
			}
		})
	}
}

func TestListLowBalanceUsersExcludesUsersNotifiedForSiteDay(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanBalanceLowFixtures(t, db)

	notified := insertBalanceLowUser(t, db, "b4-list-notified", 100, "enabled")
	pending := insertBalanceLowUser(t, db, "b4-list-pending", 100, "enabled")
	atThreshold := insertBalanceLowUser(t, db, "b4-list-at-threshold", 1000, "enabled")

	if _, err := db.Exec(`INSERT INTO notification_deliveries
		(category, channel, recipient, dedupe_key, status)
		VALUES ('billing.balance_low', 'webhook', 'webhook', $1, 'queued')`,
		fmt.Sprintf("balance_low:%d:%s", notified, balanceLowTestDay)); err != nil {
		t.Fatalf("seed today's delivery: %v", err)
	}

	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ids := func(users []BalanceLowUser) map[int64]bool {
		out := make(map[int64]bool, len(users))
		for _, u := range users {
			out[u.ID] = true
		}
		return out
	}

	// 共享库里可能有其它低余额用户，只断言本用例用户是否在集合中。
	today, err := store.ListLowBalanceUsers(context.Background(), 1000, balanceLowTestDay)
	if err != nil {
		t.Fatalf("ListLowBalanceUsers(today): %v", err)
	}
	todayIDs := ids(today)
	if todayIDs[notified] {
		t.Fatal("当天已有 dedupe_key 的用户不应再次进入扫描集")
	}
	if !todayIDs[pending] {
		t.Fatal("当天未通知的低余额用户应进入扫描集")
	}
	if todayIDs[atThreshold] {
		t.Fatal("余额等于阈值的用户不应进入扫描集（口径为 balance < threshold）")
	}

	// 换一个业务日后，同一用户可以再次进入扫描集。
	tomorrow, err := store.ListLowBalanceUsers(context.Background(), 1000, "2026-10-21")
	if err != nil {
		t.Fatalf("ListLowBalanceUsers(tomorrow): %v", err)
	}
	if !ids(tomorrow)[notified] {
		t.Fatal("次日应重新命中同一用户")
	}
}

func TestBalanceLowWorkerRecoveryThenFallAgain(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanBalanceLowFixtures(t, db)
	userID := insertBalanceLowUser(t, db, "b4-recover", 100, "enabled")
	cfg := newBalanceLowConfig(true, 1000, "")
	loc := time.UTC
	day1 := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	worker := newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), day1, loc)
	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("首次跌破 result = %+v", result)
	}
	if items := listBalanceLowDeliveriesForUser(t, db, userID); len(items) != 1 {
		t.Fatalf("首次跌破投递行 = %d，期望 1", len(items))
	}

	// 余额回升：本用例用户不再命中，也不会新增投递行。
	if _, err := db.Exec(`UPDATE users SET balance = 5000 WHERE id = $1`, userID); err != nil {
		t.Fatalf("raise balance: %v", err)
	}
	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("回升后 result = %+v", result)
	}
	if items := listBalanceLowDeliveriesForUser(t, db, userID); len(items) != 1 {
		t.Fatalf("回升后投递行 = %d，期望仍为 1", len(items))
	}

	// 再次跌破：当天已有投递行，查询阶段被排除；次日可再发。
	if _, err := db.Exec(`UPDATE users SET balance = 50 WHERE id = $1`, userID); err != nil {
		t.Fatalf("lower balance: %v", err)
	}
	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("同日再跌破 result = %+v", result)
	}
	if items := listBalanceLowDeliveriesForUser(t, db, userID); len(items) != 1 {
		t.Fatalf("同日再跌破后投递行 = %d，期望仍为 1", len(items))
	}

	worker = newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), day2, loc)
	if result := worker.RunOnce(context.Background()); result.Failed != 0 {
		t.Fatalf("次日再跌破 result = %+v", result)
	}
	if items := listBalanceLowDeliveriesForUser(t, db, userID); len(items) != 2 {
		t.Fatalf("次日再跌破后投递行 = %d，期望 2", len(items))
	}
}
