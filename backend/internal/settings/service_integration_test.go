//go:build integration

package settings

// 设置服务的数据库级验收：真实 PostgreSQL + 假 broadcaster / Sealer。
// 覆盖单测无法证明的部分：单事务、乐观锁、密钥密文落库、广播键集合。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	  go test -tags=integration ./internal/settings/ -v

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"newtg/gateway/internal/configcenter"
)

type fakeBroadcaster struct {
	keys []string
	err  error
}

func (f *fakeBroadcaster) Refresh(_ context.Context, keys []string) error {
	f.keys = append(f.keys, keys...)
	return f.err
}

type fakeSealer struct{ prefix string }

func (s fakeSealer) Seal(plain string) (string, error) { return s.prefix + plain, nil }
func (s fakeSealer) Open(sealed string) (string, error) {
	return strings.TrimPrefix(sealed, s.prefix), nil
}

func openSettingsTestDB(t *testing.T) *sql.DB {
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
	return db
}

type savedRow struct {
	key       string
	value     []byte
	updatedBy sql.NullInt64
	updatedAt time.Time
	exists    bool
}

// snapshotAndClean 删除 section 的注册键并登记恢复动作：集成测试共享同一个
// 测试库，绝不能让「断言用临时值」污染其它用例。
func snapshotAndClean(t *testing.T, db *sql.DB, section string) {
	t.Helper()
	rows, _ := Section(section)
	ctx := context.Background()
	for _, setting := range rows {
		var (
			value     []byte
			updatedBy sql.NullInt64
			updatedAt time.Time
		)
		err := db.QueryRowContext(ctx,
			`SELECT value, updated_by, updated_at FROM system_settings WHERE "key" = $1`,
			setting.Key).Scan(&value, &updatedBy, &updatedAt)
		saved := savedRow{key: setting.Key, exists: err == nil}
		if err == nil {
			saved.value = append([]byte(nil), value...)
			saved.updatedBy = updatedBy
			saved.updatedAt = updatedAt
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("snapshot %q: %v", setting.Key, err)
		}
		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if saved.exists {
				_, _ = db.ExecContext(cleanupCtx, `
					INSERT INTO system_settings ("key", value, updated_by, updated_at)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT ("key") DO UPDATE
					SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = EXCLUDED.updated_at`,
					saved.key, saved.value, nullableActor(saved.updatedBy.Int64), saved.updatedAt)
				return
			}
			_, _ = db.ExecContext(cleanupCtx, `DELETE FROM system_settings WHERE "key" = $1`, saved.key)
		})
		if _, err := db.ExecContext(ctx, `DELETE FROM system_settings WHERE "key" = $1`, setting.Key); err != nil {
			t.Fatalf("clean %q: %v", setting.Key, err)
		}
	}
}

func TestIntegrationSettingsSectionRoundTrip(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "notify.smtp")
	broadcast := &fakeBroadcaster{}
	cipher := fakeSealer{prefix: "sealed:"}
	svc, err := NewService(ServiceConfig{DB: db, Broadcaster: broadcast, Sealer: cipher})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()

	view, err := svc.Get(ctx, "notify.smtp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if view.UpdatedAt != nil {
		t.Fatalf("空 section updated_at 应为 null，实际 %v", view.UpdatedAt)
	}
	for _, item := range view.Items {
		if item.Value != nil {
			t.Fatalf("%q 未存值时 value 应为 null，实际 %v", item.Key, item.Value)
		}
		if item.Default == nil {
			t.Fatalf("%q 缺少默认值", item.Key)
		}
	}

	first, err := svc.Put(ctx, UpdateInput{
		Section: "notify.smtp",
		Values: map[string]json.RawMessage{
			"notify.smtp.host":     json.RawMessage(`"smtp.example.com"`),
			"notify.smtp.password": json.RawMessage(`"s3cret"`),
		},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if first.UpdatedAt == nil || first.Values["notify.smtp.password"] != MaskedValue {
		t.Fatalf("首次写入结果异常: %+v", first)
	}
	if len(broadcast.keys) != 2 {
		t.Fatalf("广播键 = %v，期望 2 个", broadcast.keys)
	}

	// 密钥必须密文落库，且 GET 只返回掩码。
	var stored string
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE "key" = 'notify.smtp.password'`).
		Scan(&stored); err != nil {
		t.Fatalf("读取密文: %v", err)
	}
	var sealed string
	if err := json.Unmarshal([]byte(stored), &sealed); err != nil {
		t.Fatalf("密文不是 JSON string: %v", err)
	}
	if sealed != "sealed:s3cret" {
		t.Fatalf("落库值 = %q，期望密文", sealed)
	}
	view, _ = svc.Get(ctx, "notify.smtp")
	for _, item := range view.Items {
		if item.Key == "notify.smtp.password" && item.Value != MaskedValue {
			t.Fatalf("密钥 GET 应掩码，实际 %v", item.Value)
		}
	}

	// 留空不更新：同时改 host（会 bump 版本），密码保持原密文。
	second, err := svc.Put(ctx, UpdateInput{
		Section: "notify.smtp",
		Values: map[string]json.RawMessage{
			"notify.smtp.host":     json.RawMessage(`"smtp2.example.com"`),
			"notify.smtp.password": json.RawMessage(`""`),
		},
		ExpectUpdatedAt: first.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("留空更新: %v", err)
	}
	if len(second.Changed) != 1 || second.Changed[0] != "notify.smtp.host" {
		t.Fatalf("留空密钥不应产生写入: %v", second.Changed)
	}
	if err := db.QueryRow(`SELECT value FROM system_settings WHERE "key" = 'notify.smtp.password'`).
		Scan(&stored); err != nil {
		t.Fatalf("读取密文: %v", err)
	}
	var sealedAgain string
	_ = json.Unmarshal([]byte(stored), &sealedAgain)
	if sealedAgain != "sealed:s3cret" {
		t.Fatalf("留空把密钥覆盖成了 %q", sealedAgain)
	}

	// 陈旧 expect → 409，且不广播。
	broadcast.keys = nil
	if _, err := svc.Put(ctx, UpdateInput{
		Section:         "notify.smtp",
		Values:          map[string]json.RawMessage{"notify.smtp.host": json.RawMessage(`"other"`)},
		ExpectUpdatedAt: first.UpdatedAt,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧 expect 应 409，实际 %v", err)
	}
	if len(broadcast.keys) != 0 {
		t.Fatalf("冲突不得广播: %v", broadcast.keys)
	}

	// null = CLEAR：删除行，密钥回到「未配置」。
	cleared, err := svc.Put(ctx, UpdateInput{
		Section:         "notify.smtp",
		Values:          map[string]json.RawMessage{"notify.smtp.password": json.RawMessage(`null`)},
		ExpectUpdatedAt: second.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("清除密钥: %v", err)
	}
	if cleared.Values["notify.smtp.password"] != nil {
		t.Fatalf("清除后响应值 = %v，期望 null", cleared.Values["notify.smtp.password"])
	}
	var remaining int
	if err := db.QueryRow(`SELECT count(*) FROM system_settings WHERE "key" = 'notify.smtp.password'`).
		Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("null 清除后仍剩余 %d 行", remaining)
	}
	view, _ = svc.Get(ctx, "notify.smtp")
	for _, item := range view.Items {
		if item.Key == "notify.smtp.password" && item.Value != nil {
			t.Fatalf("清除后 GET value 应为 null，实际 %v", item.Value)
		}
	}
}

func TestIntegrationSettingsNullResetsNonSecretToDefault(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "site")
	svc, _ := NewService(ServiceConfig{DB: db})
	ctx := context.Background()

	first, err := svc.Put(ctx, UpdateInput{Section: "site", Values: map[string]json.RawMessage{
		"site.title": json.RawMessage(`"Override"`),
	}})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	cleared, err := svc.Put(ctx, UpdateInput{
		Section:         "site",
		Values:          map[string]json.RawMessage{"site.title": json.RawMessage(`null`)},
		ExpectUpdatedAt: first.UpdatedAt,
	})
	if err != nil {
		t.Fatalf("清除: %v", err)
	}
	if cleared.UpdatedAt != nil {
		t.Fatalf("删掉 section 最后一行后 updated_at 应为 null，实际 %v", cleared.UpdatedAt)
	}
	view, err := svc.Get(ctx, "site")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, item := range view.Items {
		if item.Key == "site.title" {
			if item.Value != nil {
				t.Fatalf("清除后 value 应为 null，实际 %v", item.Value)
			}
			if item.Default != "NewTG" {
				t.Fatalf("default = %v，期望 NewTG", item.Default)
			}
		}
	}
}

func TestIntegrationSettingsValidationAndAtomicity(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "site")
	broadcast := &fakeBroadcaster{}
	svc, _ := NewService(ServiceConfig{DB: db, Broadcaster: broadcast})
	ctx := context.Background()

	// 一个合法值 + 一个未注册键：整段拒绝且零写入。
	_, err := svc.Put(ctx, UpdateInput{Section: "site", Values: map[string]json.RawMessage{
		"site.title":     json.RawMessage(`"ShouldNotPersist"`),
		"site.not.a.key": json.RawMessage(`true`),
	}})
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("未注册键应返回 ValidationError，实际 %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM system_settings WHERE "key" = 'site.title'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatal("校验失败时不得写入任何键")
	}
	if len(broadcast.keys) != 0 {
		t.Fatalf("校验失败不得广播: %v", broadcast.keys)
	}

	// 跨 section 键同样拒绝。
	if _, err := svc.Put(ctx, UpdateInput{Section: "site", Values: map[string]json.RawMessage{
		"nav.home.enabled": json.RawMessage(`false`),
	}}); !errors.As(err, &verr) {
		t.Fatalf("跨 section 键应被拒，实际 %v", err)
	}

	// 锁定键不可改为 false（422，F-08-03 TC-04）。
	var locked *LockedError
	if _, err := svc.Put(ctx, UpdateInput{Section: "nav", Values: map[string]json.RawMessage{
		"nav.console.require_auth": json.RawMessage(`false`),
	}}); !errors.As(err, &locked) {
		t.Fatalf("锁定键置 false 应返回 LockedError，实际 %v", err)
	}
}

func TestIntegrationSettingsDangerousDirection(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "pay")
	snapshotAndClean(t, db, "sec")
	svc, _ := NewService(ServiceConfig{DB: db})
	ctx := context.Background()

	// 关闭型风险：默认 true → false 需要确认；true（安全方向）放行。
	var verr *ValidationError
	if _, err := svc.Put(ctx, UpdateInput{Section: "pay", Values: map[string]json.RawMessage{
		"pay.online_enabled": json.RawMessage(`false`),
	}}); !errors.As(err, &verr) {
		t.Fatalf("关闭支付未确认应被拒，实际 %v", err)
	}
	confirmed, err := svc.Put(ctx, UpdateInput{Section: "pay", Values: map[string]json.RawMessage{
		"pay.online_enabled": json.RawMessage(`false`),
	}, DangerousConfirmed: true})
	if err != nil {
		t.Fatalf("确认后应允许关闭支付: %v", err)
	}
	if _, err := svc.Put(ctx, UpdateInput{
		Section:         "pay",
		Values:          map[string]json.RawMessage{"pay.online_enabled": json.RawMessage(`true`)},
		ExpectUpdatedAt: confirmed.UpdatedAt,
	}); err != nil {
		t.Fatalf("安全方向（重新开启支付）不应要求确认: %v", err)
	}

	// 开启型风险：allow_private_ip false（安全）放行，true 需要确认。
	safe, err := svc.Put(ctx, UpdateInput{Section: "sec", Values: map[string]json.RawMessage{
		"sec.ssrf.allow_private_ip": json.RawMessage(`false`),
	}})
	if err != nil {
		t.Fatalf("安全方向（保持禁止私网）不应要求确认: %v", err)
	}
	if _, err := svc.Put(ctx, UpdateInput{
		Section:         "sec",
		Values:          map[string]json.RawMessage{"sec.ssrf.allow_private_ip": json.RawMessage(`true`)},
		ExpectUpdatedAt: safe.UpdatedAt,
	}); !errors.As(err, &verr) {
		t.Fatalf("允许私网未确认应被拒，实际 %v", err)
	}
	if _, err := svc.Put(ctx, UpdateInput{
		Section:            "sec",
		Values:             map[string]json.RawMessage{"sec.ssrf.allow_private_ip": json.RawMessage(`true`)},
		ExpectUpdatedAt:    safe.UpdatedAt,
		DangerousConfirmed: true,
	}); err != nil {
		t.Fatalf("确认后应允许允许私网: %v", err)
	}

	// GET 必须暴露 danger_direction，供前端只对风险方向弹确认框。
	view, err := svc.Get(ctx, "pay")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	found := false
	for _, item := range view.Items {
		if item.Key == "pay.online_enabled" {
			found = true
			if !item.Dangerous || item.DangerDirection != DangerDirectionOff {
				t.Fatalf("pay.online_enabled danger=%v direction=%q", item.Dangerous, item.DangerDirection)
			}
		}
	}
	if !found {
		t.Fatal("pay.online_enabled 未返回")
	}
}

// 支付密钥属 any 方向：任何替换/清除都要确认，没有「安全方向」。
func TestIntegrationSettingsPaymentSecretRequiresAnyConfirmation(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "pay.stripe")
	svc, _ := NewService(ServiceConfig{DB: db, Sealer: fakeSealer{prefix: "sealed:"}})
	ctx := context.Background()

	var verr *ValidationError
	if _, err := svc.Put(ctx, UpdateInput{Section: "pay.stripe", Values: map[string]json.RawMessage{
		"pay.stripe.secret_key": json.RawMessage(`"sk_test_123"`),
	}}); !errors.As(err, &verr) {
		t.Fatalf("支付密钥未确认应被拒，实际 %v", err)
	}
	if _, err := svc.Put(ctx, UpdateInput{Section: "pay.stripe", Values: map[string]json.RawMessage{
		"pay.stripe.secret_key": json.RawMessage(`"sk_test_123"`),
	}, DangerousConfirmed: true}); err != nil {
		t.Fatalf("确认后应允许保存支付密钥: %v", err)
	}
	view, _ := svc.Get(ctx, "pay.stripe")
	for _, item := range view.Items {
		if item.Key == "pay.stripe.secret_key" {
			if !item.Dangerous || item.DangerDirection != DangerDirectionAny {
				t.Fatalf("secret danger=%v direction=%q", item.Dangerous, item.DangerDirection)
			}
		}
	}
}

// F14：并发同一 expect_updated_at 的两次 Put，恰好一个成功、一个 409。
func TestIntegrationSettingsConcurrentPutConflict(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "site")
	svc, _ := NewService(ServiceConfig{DB: db})
	ctx := context.Background()

	initial, err := svc.Get(ctx, "site")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if initial.UpdatedAt != nil {
		t.Fatalf("清理后 updated_at 应为 null，实际 %v", initial.UpdatedAt)
	}

	results := make(chan error, 2)
	start := make(chan struct{})
	for _, title := range []string{"A", "B"} {
		title := title
		go func() {
			<-start
			_, err := svc.Put(ctx, UpdateInput{
				Section:         "site",
				Values:          map[string]json.RawMessage{"site.title": json.RawMessage(`"` + title + `"`)},
				ExpectUpdatedAt: initial.UpdatedAt,
			})
			results <- err
		}()
	}
	close(start)

	var success, conflict int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			success++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatalf("意外错误: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d，期望各 1", success, conflict)
	}
}

func TestIntegrationSettingsPublicSnapshotReadsStoredValue(t *testing.T) {
	db := openSettingsTestDB(t)
	snapshotAndClean(t, db, "site")
	svc, _ := NewService(ServiceConfig{DB: db})
	ctx := context.Background()
	if _, err := svc.Put(ctx, UpdateInput{Section: "site", Values: map[string]json.RawMessage{
		"site.title": json.RawMessage(`"IntegrationTitle"`),
	}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// configcenter.Center 的读取面在集成装配中由 platform 提供；这里用
	// 直接查询模拟它的 Get 语义（只读已落库键）。
	reader := dbConfigReader{db: db}
	snapshot := PublicSnapshot(reader)
	if snapshot["site.title"] != "IntegrationTitle" {
		t.Fatalf("公开快照 site.title = %v", snapshot["site.title"])
	}
	if snapshot["site.timezone"] != "Asia/Shanghai" {
		t.Fatalf("未存值应回落默认值，实际 %v", snapshot["site.timezone"])
	}
	site := DomainValues(snapshot, "site")
	if site["title"] != "IntegrationTitle" {
		t.Fatalf("投影 site.title = %v", site["title"])
	}
}

type dbConfigReader struct{ db *sql.DB }

func (r dbConfigReader) Get(key string) (json.RawMessage, bool) {
	var raw []byte
	if err := r.db.QueryRow(`SELECT value FROM system_settings WHERE "key" = $1`, key).Scan(&raw); err != nil {
		return nil, false
	}
	return json.RawMessage(raw), true
}

// TestIntegrationSettingsBroadcastUpdatesCenter 证明写成功后配置中心快照被刷新
// （不是只写库）：生产装配传入的就是同一个 *configcenter.Center。
func TestIntegrationSettingsBroadcastUpdatesCenter(t *testing.T) {
	db := openSettingsTestDB(t)
	addr := os.Getenv("NEWTG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("NEWTG_TEST_REDIS_ADDR 未设置，跳过广播集成测试")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis 不可用: %v", err)
	}

	snapshotAndClean(t, db, "site")
	center := configcenter.New(db, rdb)
	if err := center.Load(ctx); err != nil {
		t.Fatalf("center Load: %v", err)
	}
	svc, err := NewService(ServiceConfig{DB: db, Broadcaster: center})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Put(ctx, UpdateInput{Section: "site", Values: map[string]json.RawMessage{
		"site.title": json.RawMessage(`"BroadcastTitle"`),
	}}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	raw, ok := center.Get("site.title")
	if !ok {
		t.Fatal("配置中心快照未刷新")
	}
	var title string
	if err := json.Unmarshal(raw, &title); err != nil || title != "BroadcastTitle" {
		t.Fatalf("center 值 = %s, err=%v", raw, err)
	}
}
