//go:build integration

package settings

// notify.webhook 的数据库级验收（D2-10）：密钥掩码/留空不覆盖/null 清除、
// 事件去重、启用时的 section 交叉校验。复用 service_integration_test.go
// 的 openSettingsTestDB / snapshotAndClean / fakeSealer。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestIntegrationWebhookSettingsRoundTrip(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "notify.webhook")
	broadcast := &fakeBroadcaster{}
	svc, err := NewService(ServiceConfig{DB: db, Broadcaster: broadcast, Sealer: fakeSealer{prefix: "sealed:"}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()

	// 默认值：未落库时 secret 为 nil，其余取注册表默认。
	view, err := svc.Get(ctx, "notify.webhook")
	if err != nil {
		t.Fatalf("Get defaults: %v", err)
	}
	for _, item := range view.Items {
		switch item.Key {
		case "notify.webhook.enabled":
			if item.Value != nil || item.Default != false {
				t.Fatalf("enabled 默认 = %v/%v", item.Value, item.Default)
			}
		case "notify.webhook.events":
			def, _ := item.Default.([]any)
			if len(def) != 1 || def[0] != "balance_low" {
				t.Fatalf("events 默认 = %v", item.Default)
			}
		case "notify.webhook.secret":
			if item.Value != nil {
				t.Fatal("未落库的 secret 应为 null")
			}
		}
	}

	first, err := svc.Put(ctx, UpdateInput{
		Section: "notify.webhook",
		Values: map[string]json.RawMessage{
			"notify.webhook.enabled": json.RawMessage(`true`),
			"notify.webhook.url":     json.RawMessage(`"https://hooks.example.com/notify"`),
			"notify.webhook.secret":  json.RawMessage(`"s3cret"`),
			"notify.webhook.events":  json.RawMessage(`["balance_low","announcement","balance_low"]`),
		},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if first.Values["notify.webhook.secret"] != MaskedValue {
		t.Fatalf("响应 secret = %v，期望掩码", first.Values["notify.webhook.secret"])
	}
	var stored string
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE "key" = 'notify.webhook.secret'`).Scan(&stored); err != nil {
		t.Fatalf("read sealed secret: %v", err)
	}
	if stored != `"sealed:s3cret"` {
		t.Fatalf("落库 secret = %s，期望密文", stored)
	}
	var eventsRaw []byte
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE "key" = 'notify.webhook.events'`).Scan(&eventsRaw); err != nil {
		t.Fatalf("read events: %v", err)
	}
	var events []string
	if err := json.Unmarshal(eventsRaw, &events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(events) != 2 || events[0] != "balance_low" || events[1] != "announcement" {
		t.Fatalf("events 去重保序失败: %v", events)
	}

	// 空串与掩码都表示「不更新」：secret 不得被清空/覆盖。
	version := first.UpdatedAt
	second, err := svc.Put(ctx, UpdateInput{
		Section:         "notify.webhook",
		ExpectUpdatedAt: version,
		Values: map[string]json.RawMessage{
			"notify.webhook.url":    json.RawMessage(`"https://hooks.example.com/v2"`),
			"notify.webhook.secret": json.RawMessage(`""`),
		},
	})
	if err != nil {
		t.Fatalf("Put preserve secret: %v", err)
	}
	if len(second.Changed) != 1 || second.Changed[0] != "notify.webhook.url" {
		t.Fatalf("空串不应更新密钥，Changed = %v", second.Changed)
	}
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE "key" = 'notify.webhook.secret'`).Scan(&stored); err != nil {
		t.Fatalf("reread sealed secret: %v", err)
	}
	if stored != `"sealed:s3cret"` {
		t.Fatalf("空串覆盖了密钥: %s", stored)
	}

	// 启用状态下清除 secret 必须被 section 校验拒绝：否则会出现「已启用但
	// 无法签名」的不可投递配置。
	if _, err := svc.Put(ctx, UpdateInput{
		Section:         "notify.webhook",
		ExpectUpdatedAt: second.UpdatedAt,
		Values: map[string]json.RawMessage{
			"notify.webhook.secret": json.RawMessage(`null`),
		},
	}); err == nil {
		t.Fatal("启用时清除 secret 应被拒绝")
	}

	// 关闭开关后 null = 清除：行被删除，读视图回到「未配置」。
	cleared, err := svc.Put(ctx, UpdateInput{
		Section:         "notify.webhook",
		ExpectUpdatedAt: second.UpdatedAt,
		Values: map[string]json.RawMessage{
			"notify.webhook.enabled": json.RawMessage(`false`),
			"notify.webhook.secret":  json.RawMessage(`null`),
		},
	})
	if err != nil {
		t.Fatalf("Put clear secret: %v", err)
	}
	if cleared.Values["notify.webhook.secret"] != nil {
		t.Fatalf("清除后响应值 = %v，期望 null", cleared.Values["notify.webhook.secret"])
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM system_settings WHERE "key" = 'notify.webhook.secret'`).Scan(&count); err != nil {
		t.Fatalf("count secret: %v", err)
	}
	if count != 0 {
		t.Fatalf("清除后仍残留 %d 行", count)
	}
}

func TestIntegrationWebhookSettingsSectionValidation(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "notify.webhook")
	svc, err := NewService(ServiceConfig{DB: db, Sealer: fakeSealer{prefix: "sealed:"}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()

	// 启用 + http URL → 400（同一事务内 section 校验）。
	_, err = svc.Put(ctx, UpdateInput{
		Section: "notify.webhook",
		Values: map[string]json.RawMessage{
			"notify.webhook.enabled": json.RawMessage(`true`),
			"notify.webhook.url":     json.RawMessage(`"http://hooks.example.com/notify"`),
			"notify.webhook.secret":  json.RawMessage(`"s3cret"`),
		},
	})
	if err == nil {
		t.Fatal("启用 + http URL 应被 section 校验拒绝")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Key != "notify.webhook.url" {
		t.Fatalf("错误应为 notify.webhook.url 的校验错误，实际 %v", err)
	}

	// 启用但无 secret（且 DB 无存量）→ 拒绝。
	_, err = svc.Put(ctx, UpdateInput{
		Section: "notify.webhook",
		Values: map[string]json.RawMessage{
			"notify.webhook.enabled": json.RawMessage(`true`),
			"notify.webhook.url":     json.RawMessage(`"https://hooks.example.com/notify"`),
		},
	})
	if err == nil {
		t.Fatal("启用但无 secret 应被拒绝")
	}

	// 非法事件值 → 400。
	_, err = svc.Put(ctx, UpdateInput{
		Section: "notify.webhook",
		Values: map[string]json.RawMessage{
			"notify.webhook.events": json.RawMessage(`["payment_failed"]`),
		},
	})
	if err == nil {
		t.Fatal("未注册事件应被拒绝")
	}
}
