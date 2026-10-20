//go:build integration

package notify

// B4 / N6 低余额扫描的真实 PostgreSQL 验收（Redis 用 miniredis 验证扫描器
// 不写任何暂停/计数状态）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_dom_b4?sslmode=disable' \
//	  go test -tags=integration ./internal/notify/ -run BalanceLow -v
//
// 覆盖：阈值命中/未命中、禁用/阈值 0/未订阅不扫描、按站点时区按天去重、
// 跨天可再发、余额回升后再跌破、投递行字段与载荷脱敏（无邮箱/手机号）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const balanceLowTestDay = "2026-10-20"

type balanceLowSubscription struct {
	events map[string]bool
}

func (s balanceLowSubscription) Subscribed(event string) bool { return s.events[event] }

func subscribedBalanceLow() balanceLowSubscription {
	return balanceLowSubscription{events: map[string]bool{"balance_low": true}}
}

// cleanBalanceLowFixtures 清空本文件拥有的用户与全部投递行。
// 用户按 b4- 前缀限定，避免误删其它集成用例的数据。
func cleanBalanceLowFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM notification_deliveries`); err != nil {
		t.Fatalf("clean deliveries: %v", err)
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

func listBalanceLowDeliveries(t *testing.T, db *sql.DB) []balanceLowDelivery {
	t.Helper()
	rows, err := db.Query(`SELECT category, channel, recipient, status, dedupe_key, payload
		FROM notification_deliveries ORDER BY id`)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
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
	insertBalanceLowUser(t, db, "b4-at-threshold", 1000, "enabled")
	insertBalanceLowUser(t, db, "b4-above", 2000, "enabled")
	insertBalanceLowUser(t, db, "b4-disabled", 0, "disabled")

	// miniredis 只用于观察：扫描器不读写 N8 的暂停/连续失败状态。
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	worker := newBalanceLowWorkerForTest(t, db,
		newBalanceLowConfig(true, 1000, ""), subscribedBalanceLow(), now, nil)
	result := worker.RunOnce(context.Background())
	if result.Skipped || result.Scanned != 1 || result.Enqueued != 1 || result.Deduped != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v，期望 scanned=1 enqueued=1", result)
	}

	if keys, err := rdb.Keys(context.Background(), "*").Result(); err != nil || len(keys) != 0 {
		t.Fatalf("扫描器不应写 Redis（keys=%v err=%v）", keys, err)
	}

	items := listBalanceLowDeliveries(t, db)
	if len(items) != 1 {
		t.Fatalf("投递行数 = %d，期望 1", len(items))
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
}

func TestBalanceLowWorkerSkipsWhenDisabledZeroOrUnsubscribed(t *testing.T) {
	db := openNotifyTestDB(t)
	cleanBalanceLowFixtures(t, db)
	insertBalanceLowUser(t, db, "b4-skip", 0, "enabled")
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
			if items := listBalanceLowDeliveries(t, db); len(items) != 0 {
				t.Fatalf("不应产生投递行，实得 %d", len(items))
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

	if result := worker.RunOnce(context.Background()); result.Enqueued != 1 {
		t.Fatalf("首轮 result = %+v", result)
	}
	// 同一站点日的第二轮被 dedupe_key 收敛。
	if result := worker.RunOnce(context.Background()); result.Deduped != 1 || result.Enqueued != 0 {
		t.Fatalf("同日第二轮 result = %+v", result)
	}
	items := listBalanceLowDeliveries(t, db)
	if len(items) != 1 || items[0].Dedupe != fmt.Sprintf("balance_low:%d:2026-10-21", userID) {
		t.Fatalf("同日投递行 = %+v，期望上海日期 2026-10-21 且仅 1 行", items)
	}

	// 次日（上海 10-22）可再发一条。
	next := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	worker = newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), next, loc)
	if result := worker.RunOnce(context.Background()); result.Enqueued != 1 {
		t.Fatalf("次日 result = %+v", result)
	}
	items = listBalanceLowDeliveries(t, db)
	if len(items) != 2 || items[1].Dedupe != fmt.Sprintf("balance_low:%d:2026-10-22", userID) {
		t.Fatalf("跨天投递行 = %+v", items)
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
	if result := worker.RunOnce(context.Background()); result.Enqueued != 1 {
		t.Fatalf("首次跌破 result = %+v", result)
	}

	// 余额回升：当天不再命中。
	if _, err := db.Exec(`UPDATE users SET balance = 5000 WHERE id = $1`, userID); err != nil {
		t.Fatalf("raise balance: %v", err)
	}
	if result := worker.RunOnce(context.Background()); result.Scanned != 0 {
		t.Fatalf("回升后 result = %+v，期望不命中", result)
	}

	// 再次跌破：当天已有一条，按天去重；次日可再发。
	if _, err := db.Exec(`UPDATE users SET balance = 50 WHERE id = $1`, userID); err != nil {
		t.Fatalf("lower balance: %v", err)
	}
	if result := worker.RunOnce(context.Background()); result.Deduped != 1 {
		t.Fatalf("同日再跌破 result = %+v，期望 deduped=1", result)
	}
	worker = newBalanceLowWorkerForTest(t, db, cfg, subscribedBalanceLow(), day2, loc)
	if result := worker.RunOnce(context.Background()); result.Enqueued != 1 {
		t.Fatalf("次日再跌破 result = %+v", result)
	}
	if items := listBalanceLowDeliveries(t, db); len(items) != 2 {
		t.Fatalf("投递行数 = %d，期望 2", len(items))
	}
}
