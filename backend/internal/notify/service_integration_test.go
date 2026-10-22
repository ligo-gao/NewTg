//go:build integration

package notify

// 集成验收（真实 PostgreSQL）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/notify/ -count=1
//
// 覆盖 brief 要求的：迁移后的表可用、播种幂等、语言回退链、乐观锁、
// 锁定模板不可关、测试发送的 SMTP 未配置/记录投递。

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type fakeSender struct {
	configured bool
	messageID  string
	err        error
	calls      int
}

func (f *fakeSender) Configured(context.Context) bool { return f.configured }

func (f *fakeSender) Send(context.Context, string, string, string) (string, error) {
	f.calls++
	return f.messageID, f.err
}

func openNotifyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过通知模板集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// resetNotifyTables 清空两张表后重新播种，保证每个用例从同一基线开始。
func resetNotifyTables(t *testing.T, db *sql.DB) *Service {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM notification_deliveries`); err != nil {
		t.Fatalf("clean deliveries: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM notification_templates`); err != nil {
		t.Fatalf("clean templates: %v", err)
	}
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	service, err := NewService(ServiceConfig{Store: store})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := service.EnsureSeeded(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return service
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestSeedIsIdempotentAndListCoversRegistry(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// 19 组 × 7 语 = 133 行（M4-a 增补 ops.channel_auto_disabled/inapp）。
	if got := countRows(t, db, "notification_templates"); got != 133 {
		t.Fatalf("播种行数 = %d，want 133", got)
	}
	// 幂等：再次播种不得覆盖或新增。
	if err := service.EnsureSeeded(ctx); err != nil {
		t.Fatalf("第二次 seed: %v", err)
	}
	if got := countRows(t, db, "notification_templates"); got != 133 {
		t.Fatalf("重复播种后行数 = %d，want 133（ON CONFLICT DO NOTHING）", got)
	}

	all, err := service.List(ctx, TemplateFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 133 {
		t.Fatalf("列表行数 = %d，want 133", len(all))
	}
	balance, err := service.List(ctx, TemplateFilter{Category: "billing.balance_low"})
	if err != nil {
		t.Fatalf("List category: %v", err)
	}
	if len(balance) != 14 {
		t.Fatalf("billing.balance_low 行数 = %d，want 14（email+inapp × 7 语，TC-F-08-24-01）", len(balance))
	}
	inapp, err := service.List(ctx, TemplateFilter{Channel: ChannelInApp})
	if err != nil {
		t.Fatalf("List channel: %v", err)
	}
	// 4 个 inapp 模板：system.announcement、billing.balance_low、
	// billing.quota_exhausted、ops.channel_auto_disabled（W7/M4-a）。
	if len(inapp) != 28 {
		t.Fatalf("inapp 行数 = %d，want 28（4 个 inapp 模板 × 7 语）", len(inapp))
	}
	for _, item := range all {
		if item.Customized {
			t.Fatalf("刚播种的行不应标记为已修改：%+v", item)
		}
	}
}

func TestUpdateOptimisticLockAndLockedTemplates(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	view, err := service.Get(ctx, "billing.balance_low", ChannelEmail, LocaleEN)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.UpdatedAt == nil || view.Customized {
		t.Fatalf("播种行应带 updated_at 且 customized=false：%+v", view)
	}
	updated, err := service.Update(ctx, UpdateInput{
		Key:             "billing.balance_low",
		Channel:         ChannelEmail,
		Locale:          LocaleEN,
		Subject:         "Custom subject",
		Body:            "Custom body {{.balance}}",
		ExpectUpdatedAt: view.UpdatedAt,
		Actor:           7,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated.Customized || updated.Subject != "Custom subject" {
		t.Fatalf("更新结果 = %+v，want customized=true 且新主题", updated)
	}

	// F6：已存在的行缺少 expect_updated_at → 400 语义错误（不允许无条件覆盖）。
	_, err = service.Update(ctx, UpdateInput{
		Key: "billing.balance_low", Channel: ChannelEmail, Locale: LocaleEN,
		Subject: "No precondition", Body: "x",
	})
	if !errors.Is(err, ErrExpectationRequired) {
		t.Fatalf("缺 expect_updated_at 应返回 ErrExpectationRequired，得到 %v", err)
	}

	// 旧版本的 expect 必须 409（乐观锁）。
	_, err = service.Update(ctx, UpdateInput{
		Key:             "billing.balance_low",
		Channel:         ChannelEmail,
		Locale:          LocaleEN,
		Subject:         "Stale",
		Body:            "stale",
		ExpectUpdatedAt: view.UpdatedAt,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("过期 expect_updated_at 应返回 ErrConflict，得到 %v", err)
	}

	// F5：PUT 省略 enabled 时保持当前值，不得把停用模板悄悄重新启用。
	disabled := false
	updated, err = service.Update(ctx, UpdateInput{
		Key: "billing.balance_low", Channel: ChannelEmail, Locale: LocaleEN,
		Subject: "Disabled", Body: "body {{.balance}}", Enabled: &disabled,
		ExpectUpdatedAt: updated.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if updated.Enabled {
		t.Fatal("显式 enabled=false 应停用模板")
	}
	kept, err := service.Update(ctx, UpdateInput{
		Key: "billing.balance_low", Channel: ChannelEmail, Locale: LocaleEN,
		Subject: "Still disabled", Body: "body {{.balance}}",
		ExpectUpdatedAt: updated.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("update without enabled: %v", err)
	}
	if kept.Enabled {
		t.Fatal("省略 enabled 应保持 false，而不是默认重新启用（F5）")
	}

	// P4：锁定模板不可停用。
	falseValue := false
	_, err = service.Update(ctx, UpdateInput{
		Key:     "auth.verify_code",
		Channel: ChannelEmail,
		Locale:  LocaleEN,
		Subject: "code",
		Body:    "{{.code}}",
		Enabled: &falseValue,
	})
	if !errors.Is(err, ErrLockedTemplate) {
		t.Fatalf("锁定模板停用应返回 ErrLockedTemplate，得到 %v", err)
	}

	// 白名单外变量在保存时拒绝（422 的语义来源）。
	_, err = service.Update(ctx, UpdateInput{
		Key:     "billing.balance_low",
		Channel: ChannelEmail,
		Locale:  LocaleEN,
		Subject: "subject",
		Body:    "{{.foo}}",
	})
	var unknown *ErrUnknownVariable
	if !errors.As(err, &unknown) {
		t.Fatalf("未登记变量应返回 ErrUnknownVariable，得到 %v", err)
	}
}

func TestLanguageFallbackChain(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// fr 行删除后，读取应回退到 en 行并返回实际命中的 locale。
	if _, err := db.Exec(`DELETE FROM notification_templates
		WHERE "key" = 'billing.balance_low' AND channel = 'email' AND locale = 'fr'`); err != nil {
		t.Fatalf("delete fr row: %v", err)
	}
	view, err := service.Get(ctx, "billing.balance_low", ChannelEmail, LocaleFR)
	if err != nil {
		t.Fatalf("Get fr: %v", err)
	}
	if view.Locale != LocaleEN {
		t.Fatalf("fr 缺行应回退 en，得到 %s", view.Locale)
	}

	// 发送路径更严格：fr 与 en 都缺行 → fail-closed，且落 failed 记录。
	if _, err := db.Exec(`DELETE FROM notification_templates
		WHERE "key" = 'billing.balance_low' AND channel = 'email' AND locale IN ('fr', 'en')`); err != nil {
		t.Fatalf("delete en/fr rows: %v", err)
	}
	sender := &fakeSender{configured: true}
	strict, err := NewService(ServiceConfig{Store: service.store, Sender: sender})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = strict.TestSend(ctx, TestInput{
		Key: "billing.balance_low", Channel: ChannelEmail, Locale: LocaleFR,
		Recipient: "admin@example.com", ActorID: 1,
	})
	if !errors.Is(err, ErrTemplateMissing) {
		t.Fatalf("语言全链回退失败应返回 ErrTemplateMissing，得到 %v", err)
	}
	if sender.calls != 0 {
		t.Fatalf("模板缺失时不得调用 SMTP，calls=%d", sender.calls)
	}
	var failed int
	if err := db.QueryRow(`SELECT count(*) FROM notification_deliveries
		WHERE category = 'billing.balance_low' AND status = 'failed'`).Scan(&failed); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if failed != 1 {
		t.Fatalf("模板缺失应落 1 条 failed 记录，得到 %d", failed)
	}
}

func TestTestSendRecordsDeliveryAndSMTPRequired(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// 无 SMTP：422 语义错误，且不写记录。
	_, err := service.TestSend(ctx, TestInput{
		Key: "system.smtp_test", Channel: ChannelEmail, Locale: LocaleEN,
		Recipient: "admin@example.com", ActorID: 1,
	})
	if !errors.Is(err, ErrSMTPNotConfigured) {
		t.Fatalf("无 SMTP 应返回 ErrSMTPNotConfigured，得到 %v", err)
	}
	if got := countRows(t, db, "notification_deliveries"); got != 0 {
		t.Fatalf("SMTP 未配置不应写投递记录，得到 %d 行", got)
	}

	sender := &fakeSender{configured: true, messageID: "<abc@example.com>"}
	withSMTP, err := NewService(ServiceConfig{Store: service.store, Sender: sender})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	delivery, err := withSMTP.TestSend(ctx, TestInput{
		Key: "system.smtp_test", Channel: ChannelEmail, Locale: LocaleEN,
		Recipient: "admin@example.com", ActorID: 1,
	})
	if err != nil {
		t.Fatalf("TestSend: %v", err)
	}
	if delivery.Status != "sent" || delivery.ID == 0 {
		t.Fatalf("测试发送应写 sent 记录，得到 %+v", delivery)
	}
	if delivery.ProviderMessageID == nil || *delivery.ProviderMessageID != "<abc@example.com>" {
		t.Fatalf("应记录 Message-ID，得到 %v", delivery.ProviderMessageID)
	}
	if sender.calls != 1 {
		t.Fatalf("SMTP 调用次数 = %d，want 1", sender.calls)
	}

	// 投递列表：pending 是 API 口径，DB queued 也要能按它筛选。
	if _, _, err := service.store.InsertDelivery(ctx, Delivery{
		Category: "billing.payment_success", Channel: ChannelEmail,
		Recipient: "user@example.com", DedupeKey: "test:pending:1", Status: "queued",
	}); err != nil {
		t.Fatalf("insert queued delivery: %v", err)
	}
	items, total, err := withSMTP.ListDeliveries(ctx, DeliveryFilter{Status: "pending"})
	if err != nil {
		t.Fatalf("ListDeliveries pending: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Status != "pending" {
		t.Fatalf("pending 筛选 = total %d items %+v，want 1 条 pending", total, items)
	}
}

func TestDeliveryDedupeKeyIsIdempotent(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	record := Delivery{
		Category: "billing.balance_low", Channel: ChannelEmail,
		Recipient: "user@example.com", DedupeKey: "billing.balance_low:u17:window",
		Status: "queued",
	}
	first, inserted, err := service.store.InsertDelivery(ctx, record)
	if err != nil || !inserted {
		t.Fatalf("首次插入应成功：inserted=%v err=%v", inserted, err)
	}
	_, inserted, err = service.store.InsertDelivery(ctx, record)
	if err != nil {
		t.Fatalf("重复插入: %v", err)
	}
	if inserted {
		t.Fatal("相同 dedupe_key 不得重复插入（§3.9.6b 幂等）")
	}
	if got := countRows(t, db, "notification_deliveries"); got != 1 {
		t.Fatalf("去重后行数 = %d，want 1", got)
	}
	if first.ID == 0 {
		t.Fatal("首次插入应返回 id")
	}
}

func TestTestSendRecordsFailedStatus(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	sender := &fakeSender{configured: true, err: errors.New("connection refused")}
	withSMTP, err := NewService(ServiceConfig{Store: service.store, Sender: sender})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	delivery, err := withSMTP.TestSend(ctx, TestInput{
		Key: "system.smtp_test", Channel: ChannelEmail, Locale: LocaleEN,
		Recipient: "admin@example.com", ActorID: 1,
	})
	if err != nil {
		t.Fatalf("发送失败仍应返回投递记录：%v", err)
	}
	if delivery.Status != "failed" || delivery.LastError == nil {
		t.Fatalf("SMTP 失败应记 failed + last_error，得到 %+v", delivery)
	}
	if !strings.Contains(*delivery.LastError, "connection refused") {
		t.Fatalf("last_error 应保留失败原因，得到 %q", *delivery.LastError)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM notification_deliveries WHERE id = $1`, delivery.ID).Scan(&status); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if status != "failed" {
		t.Fatalf("DB 状态 = %q，want failed", status)
	}
}

func TestRestoreDefaultRestoresBundleAndPreservesEnabled(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	view, err := service.Get(ctx, "billing.balance_low", ChannelEmail, LocaleEN)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	disabled := false
	custom, err := service.Update(ctx, UpdateInput{
		Key: "billing.balance_low", Channel: ChannelEmail, Locale: LocaleEN,
		Subject: "Custom", Body: "Custom {{.balance}}", Enabled: &disabled,
		ExpectUpdatedAt: view.UpdatedAt, Actor: 7,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !custom.Customized || custom.Enabled {
		t.Fatalf("前置状态 = %+v，want customized=true enabled=false", custom)
	}

	restored, err := service.RestoreDefault(ctx, "billing.balance_low", ChannelEmail, LocaleEN, 9)
	if err != nil {
		t.Fatalf("RestoreDefault: %v", err)
	}
	spec, _ := LookupTemplate("billing.balance_low", ChannelEmail)
	want, _ := spec.DefaultContent(LocaleEN)
	if restored.Subject != want.Subject || restored.Body != want.Body {
		t.Fatalf("恢复内容 = %+v，want bundle %+v", restored, want)
	}
	if restored.Customized {
		t.Fatal("恢复后 customized 必须为 false")
	}
	if restored.Enabled {
		t.Fatal("恢复默认只重置内容，不得重新启用管理员停用的模板")
	}

	// 幂等：再恢复一次结果不变（不产生新行、不改变内容）。
	again, err := service.RestoreDefault(ctx, "billing.balance_low", ChannelEmail, LocaleEN, 9)
	if err != nil {
		t.Fatalf("second RestoreDefault: %v", err)
	}
	if again.Subject != restored.Subject || again.Body != restored.Body || again.Customized {
		t.Fatalf("重复恢复结果漂移：%+v vs %+v", again, restored)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM notification_templates
		WHERE "key" = 'billing.balance_low' AND channel = 'email' AND locale = 'en'`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("恢复默认不得产生重复行，得到 %d", rows)
	}
}

func TestRestoreDefaultFallsBackToEnAndInsertsMissingRow(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// 内置 bundle 只有 zh-CN / en；fr 的默认值就是 en 文案（§3.9.6a）。
	if _, err := db.Exec(`DELETE FROM notification_templates
		WHERE "key" = 'billing.balance_low' AND channel = 'email' AND locale = 'fr'`); err != nil {
		t.Fatalf("delete fr: %v", err)
	}
	restored, err := service.RestoreDefault(ctx, "billing.balance_low", ChannelEmail, LocaleFR, 1)
	if err != nil {
		t.Fatalf("RestoreDefault fr: %v", err)
	}
	spec, _ := LookupTemplate("billing.balance_low", ChannelEmail)
	want, _ := spec.DefaultContent(LocaleEN)
	if restored.Locale != LocaleFR || restored.Subject != want.Subject || restored.Body != want.Body {
		t.Fatalf("fr 恢复 = %+v，want locale=fr + en 文案", restored)
	}
	if restored.Customized {
		t.Fatal("插入的恢复行 customized 应为 false")
	}
}

func TestRestoreDefaultsBulkFilterAndModifiedOnly(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// 修改两行：一行 email/en，一行 inapp/en；另留一行未修改作对照。
	view, err := service.Get(ctx, "billing.balance_low", ChannelEmail, LocaleEN)
	if err != nil {
		t.Fatalf("Get email: %v", err)
	}
	if _, err := service.Update(ctx, UpdateInput{
		Key: "billing.balance_low", Channel: ChannelEmail, Locale: LocaleEN,
		Subject: "Custom email", Body: "Custom {{.balance}}",
		ExpectUpdatedAt: view.UpdatedAt,
	}); err != nil {
		t.Fatalf("Update email: %v", err)
	}
	inapp, err := service.Get(ctx, "billing.balance_low", ChannelInApp, LocaleEN)
	if err != nil {
		t.Fatalf("Get inapp: %v", err)
	}
	if _, err := service.Update(ctx, UpdateInput{
		Key: "billing.balance_low", Channel: ChannelInApp, Locale: LocaleEN,
		Body:            "Custom inapp {{.balance}}",
		ExpectUpdatedAt: inapp.UpdatedAt,
	}); err != nil {
		t.Fatalf("Update inapp: %v", err)
	}

	// 只恢复已修改的行：仅 email/en 被改过，因此只应回写 1 行。
	items, err := service.RestoreDefaults(ctx, RestoreFilter{
		Categories: []string{"billing.balance_low"}, Channel: ChannelEmail, ModifiedOnly: true,
	}, 5)
	if err != nil {
		t.Fatalf("RestoreDefaults: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("仅已修改恢复行数 = %d，want 1（只改了 email/en）", len(items))
	}
	for _, item := range items {
		if item.Customized {
			t.Fatalf("恢复后仍有 customized 行：%+v", item)
		}
	}
	still, err := service.Get(ctx, "billing.balance_low", ChannelInApp, LocaleEN)
	if err != nil {
		t.Fatalf("Get inapp after: %v", err)
	}
	if !still.Customized {
		t.Fatal("未命中筛选的 inapp 行不应被恢复")
	}

	// 不带「仅已修改」时，分类×渠道的全集（7 语）都回写默认。
	channelWide, err := service.RestoreDefaults(ctx, RestoreFilter{
		Categories: []string{"billing.balance_low"}, Channel: ChannelEmail,
	}, 5)
	if err != nil {
		t.Fatalf("RestoreDefaults channel: %v", err)
	}
	if len(channelWide) != 7 {
		t.Fatalf("email 渠道恢复行数 = %d，want 7（7 语）", len(channelWide))
	}

	// 全量恢复（无筛选）：133 行全部 is_modified=false。
	all, err := service.RestoreDefaults(ctx, RestoreFilter{}, 5)
	if err != nil {
		t.Fatalf("RestoreDefaults all: %v", err)
	}
	if len(all) != 133 {
		t.Fatalf("全量恢复行数 = %d，want 133", len(all))
	}
	var modified int
	if err := db.QueryRow(`SELECT count(*) FROM notification_templates WHERE is_modified = true`).Scan(&modified); err != nil {
		t.Fatalf("count modified: %v", err)
	}
	if modified != 0 {
		t.Fatalf("全量恢复后仍有 %d 行 is_modified=true", modified)
	}
}

func TestDeliveryFilterTimeRangeAndDetailFields(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	insert := func(dedupe string, createdAt time.Time, attempt int, next *time.Time) Delivery {
		t.Helper()
		row, ok, err := service.store.InsertDelivery(ctx, Delivery{
			Category: "billing.balance_low", Channel: ChannelEmail,
			Recipient: "user@example.com", DedupeKey: dedupe, Status: "queued",
			Attempt: attempt, NextRetryAt: next, ProviderMessageID: stringPtr("<" + dedupe + "@example.com>"),
		})
		if err != nil || !ok {
			t.Fatalf("insert %s: ok=%v err=%v", dedupe, ok, err)
		}
		if _, err := db.Exec(`UPDATE notification_deliveries
			SET created_at = $2, next_retry_at = $3 WHERE id = $1`, row.ID, createdAt, next); err != nil {
			t.Fatalf("set delivery times: %v", err)
		}
		return row
	}
	old := insert("range:old", base.Add(-48*time.Hour), 1, nil)
	fresh := insert("range:fresh", base, 2, &base)
	_ = old

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	items, total, err := service.ListDeliveries(ctx, DeliveryFilter{From: &from, To: &to})
	if err != nil {
		t.Fatalf("ListDeliveries range: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].DedupeKey != "range:fresh" {
		t.Fatalf("时间范围筛选 = total %d items %+v", total, items)
	}
	got := items[0]
	if got.Attempt != 2 || got.NextRetryAt == nil || !got.NextRetryAt.Equal(base) ||
		got.ProviderMessageID != "<range:fresh@example.com>" {
		t.Fatalf("列表 DTO 未带排障字段：%+v", got)
	}

	detail, err := service.GetDelivery(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("GetDelivery: %v", err)
	}
	if detail.DedupeKey != "range:fresh" || detail.Attempt != 2 || detail.ProviderMessageID == "" {
		t.Fatalf("详情 DTO = %+v", detail)
	}
	if _, err := service.GetDelivery(ctx, 999999); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("不存在的投递应返回 ErrDeliveryNotFound，得到 %v", err)
	}

	// 倒置与 >90 天都由 Service 拒绝。
	before := base.Add(time.Hour)
	after := base.Add(-time.Hour)
	if _, _, err := service.ListDeliveries(ctx, DeliveryFilter{From: &before, To: &after}); !errors.Is(err, ErrInvalidTimeRange) {
		t.Fatalf("倒置范围应返回 ErrInvalidTimeRange，得到 %v", err)
	}
	longFrom := base.Add(-91 * 24 * time.Hour)
	if _, _, err := service.ListDeliveries(ctx, DeliveryFilter{From: &longFrom, To: &to}); !errors.Is(err, ErrInvalidTimeRange) {
		t.Fatalf("超 90 天应返回 ErrInvalidTimeRange，得到 %v", err)
	}
}

func TestUpdateInsertsWhenRowMissingWithoutExpectation(t *testing.T) {
	db := openNotifyTestDB(t)
	service := resetNotifyTables(t, db)
	ctx := context.Background()

	// L3：行不存在时（首次补种/被删除后）允许不带 expect_updated_at 插入。
	if _, err := db.Exec(`DELETE FROM notification_templates
		WHERE "key" = 'billing.balance_low' AND channel = 'email' AND locale = 'en'`); err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if _, err := service.store.Get(ctx, "billing.balance_low", ChannelEmail, LocaleEN); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("删除后应无行，得到 %v", err)
	}
	updated, err := service.Update(ctx, UpdateInput{
		Key:     "billing.balance_low",
		Channel: ChannelEmail,
		Locale:  LocaleEN,
		Subject: "Recreated subject",
		Body:    "Recreated body {{.balance}}",
		// ExpectUpdatedAt 省略：新建行豁免（既有行则必须提供）。
	})
	if err != nil {
		t.Fatalf("缺行时无 expect 应允许插入：%v", err)
	}
	if updated.Subject != "Recreated subject" || updated.Locale != LocaleEN {
		t.Fatalf("插入结果 = %+v", updated)
	}
	if !updated.Enabled {
		t.Fatal("新建行应默认 enabled=true")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_templates
		WHERE "key" = 'billing.balance_low' AND channel = 'email' AND locale = 'en'`).Scan(&count); err != nil {
		t.Fatalf("count row: %v", err)
	}
	if count != 1 {
		t.Fatalf("重建行数 = %d，want 1", count)
	}
}
