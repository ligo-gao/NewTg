//go:build integration

package tokenstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 12 的 DB/Redis 集成验收（覆盖 brief a–g 的持久化面）。
// 运行：
//
//	NEWTG_TEST_DB_DSN=postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/tokenstore/ -v

func integrationDSN() string {
	if dsn := os.Getenv("NEWTG_TEST_DB_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable"
}

func integrationRedisAddr() string {
	if addr := os.Getenv("NEWTG_TEST_REDIS_ADDR"); addr != "" {
		return addr
	}
	return "localhost:56379"
}

func openIntegration(t *testing.T) (*gorm.DB, *redis.Client) {
	t.Helper()
	gdb, err := gorm.Open(postgres.Open(integrationDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		t.Fatalf("ping test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	rdb := redis.NewClient(&redis.Options{Addr: integrationRedisAddr(), MaxRetries: -1})
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		t.Fatalf("ping test redis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return gdb, rdb
}

// uniqueSuffix 避免并行/重复运行之间共享 fixture 名。
func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func seedGroup(t *testing.T, db *gorm.DB, name string, selectable bool, status int, ratio int) {
	t.Helper()
	err := db.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, status)
		VALUES (?, ?, ?, ?) ON CONFLICT (name) DO UPDATE
		SET ratio = EXCLUDED.ratio, user_selectable = EXCLUDED.user_selectable, status = EXCLUDED.status`,
		name, ratio, selectable, status).Error
	if err != nil {
		t.Fatalf("seed group %q: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Exec(`DELETE FROM user_groups WHERE name = ?`, name).Error })
}

func seedUser(t *testing.T, db *gorm.DB, group string) int64 {
	t.Helper()
	suffix := uniqueSuffix()
	var id int64
	err := db.Raw(`INSERT INTO users (username, "group") VALUES (?, ?) RETURNING id`,
		"tg_test_"+suffix, group).Scan(&id).Error
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec(`DELETE FROM api_tokens WHERE user_id = ?`, id).Error
		_ = db.Exec(`DELETE FROM users WHERE id = ?`, id).Error
	})
	return id
}

func hasRow(t *testing.T, db *gorm.DB, query string, args ...any) bool {
	t.Helper()
	var exists bool
	if err := db.Raw(query, args...).Scan(&exists).Error; err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return exists
}

// TestIntegrationCreateStoresOnlyHash 覆盖 brief (a)：
// 100 枚全唯一、DB 只存哈希、除创建响应外任何视图都不含明文或哈希。
func TestIntegrationCreateStoresOnlyHash(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	userID := seedUser(t, db, group)
	st := New(db, rdb, nil)

	ctx := context.Background()
	created, err := st.Create(ctx, userID, CreateInput{
		Name: "bulk", Group: group, Quantity: 100, UnlimitedQuota: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(created) != 100 {
		t.Fatalf("created = %d，期望 100", len(created))
	}

	seen := make(map[string]struct{}, 100)
	for _, c := range created {
		if _, dup := seen[c.Key]; dup {
			t.Fatalf("明文重复: %q", c.Key)
		}
		seen[c.Key] = struct{}{}
		var keyHash string
		var remain sql.NullInt64
		if err := db.Raw(`SELECT key_hash, remain_quota FROM api_tokens WHERE id = ?`, c.Token.ID).
			Row().Scan(&keyHash, &remain); err != nil {
			t.Fatalf("read token %d: %v", c.Token.ID, err)
		}
		if keyHash != auth.HashTokenKey(c.Key) {
			t.Fatalf("DB key_hash 不是完整明文哈希（token %d）", c.Token.ID)
		}
		if remain.Valid {
			t.Fatalf("不限额度令牌 remain_quota 应 NULL，实际 %d", remain.Int64)
		}
		// 返回值里的 updated_at 必须与 DB 微秒精度一致，否则紧接着的 PATCH
		// 乐观锁会因亚微秒尾数误报 409。
		var dbUpdated time.Time
		if err := db.Raw(`SELECT updated_at FROM api_tokens WHERE id = ?`, c.Token.ID).Scan(&dbUpdated).Error; err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		if !c.Token.UpdatedAt.Equal(dbUpdated) {
			t.Fatalf("Create 返回 updated_at=%v，DB=%v（精度未对齐）", c.Token.UpdatedAt, dbUpdated)
		}
		// 数据库任何文本列都不得出现明文。
		if hasRow(t, db, `SELECT EXISTS (SELECT 1 FROM api_tokens WHERE id = ? AND key_hash = ?)`, c.Token.ID, c.Key) {
			t.Fatal("DB 中出现了明文 key")
		}
	}

	// 列表/详情序列化后不得含明文或哈希。
	items, total, err := st.List(ctx, userID, ListFilter{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 100 || len(items) != 100 {
		t.Fatalf("List total=%d len=%d，期望 100/100", total, len(items))
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("marshal list: %v", err)
	}
	plain := created[0].Key
	if strings.Contains(string(raw), plain) || strings.Contains(string(raw), auth.HashTokenKey(plain)) {
		t.Fatal("列表视图泄露了明文或 key_hash")
	}

	// 零额度必须真正落 0（GORM default tag 陷阱的回归断言）。
	zeroQuota, err := st.Create(ctx, userID, CreateInput{
		Name: "zero", Group: group, Quantity: 1, UnlimitedQuota: false, RemainQuota: 0,
	})
	if err != nil {
		t.Fatalf("Create zero quota: %v", err)
	}
	var zero int64
	if err := db.Raw(`SELECT remain_quota FROM api_tokens WHERE id = ?`, zeroQuota[0].Token.ID).Scan(&zero).Error; err != nil {
		t.Fatalf("read zero quota: %v", err)
	}
	if zero != 0 {
		t.Fatalf("remain_quota = %d，期望 0（GORM default 不得把它替换成 100000）", zero)
	}
}

// TestIntegrationListSearchesNameOnly 覆盖 brief (d)/R-08：
// 搜索只匹配名称，key 后缀/完整 key 不命中。
func TestIntegrationListSearchesNameOnly(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	userID := seedUser(t, db, group)
	st := New(db, rdb, nil)
	ctx := context.Background()

	created, err := st.Create(ctx, userID, CreateInput{Name: "searchable-prod", Group: group, Quantity: 1, UnlimitedQuota: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	key := created[0].Key

	byName, total, err := st.List(ctx, userID, ListFilter{Keyword: "searchable", Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("List by name: %v", err)
	}
	if total != 1 || len(byName) != 1 {
		t.Fatalf("按名称搜索 total=%d，期望 1", total)
	}

	for _, keyword := range []string{key, strings.TrimPrefix(key, "nkt-"), key[len(key)-8:]} {
		_, total, err := st.List(ctx, userID, ListFilter{Keyword: keyword, Page: 1, PageSize: 20})
		if err != nil {
			t.Fatalf("List by key suffix: %v", err)
		}
		if total != 0 {
			t.Fatalf("关键字 %q 命中 %d 条：R-08 禁止按密钥搜索", keyword, total)
		}
	}
}

// TestIntegrationAutoGroupsWriteTime 覆盖 brief (b)(c) 与 I-20/I-23：
// auto+空列表写入期拒绝；恰好 20 接受、21 拒绝；非 auto 清空两列。
func TestIntegrationAutoGroupsWriteTime(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	base := "tg_" + suffix
	seedGroup(t, db, base, true, 1, 100)
	userID := seedUser(t, db, base)

	whitelist := make([]string, 0, 21)
	for i := 1; i <= 21; i++ {
		name := fmt.Sprintf("%s_g%02d", base, i)
		seedGroup(t, db, name, true, 1, 100)
		whitelist = append(whitelist, name)
	}
	settings := mapSettings{KeyAutoGroups: mustJSON(t, whitelist)}
	st := New(db, rdb, settings)
	ctx := context.Background()

	// (b) auto + 空列表 → 写入期拒绝（I-20，库中不得出现该状态）。
	if _, err := st.Create(ctx, userID, CreateInput{Name: "auto-empty", Group: "auto", Quantity: 1}); err == nil {
		t.Fatal("group=auto 且 auto_groups 为空必须在写入期拒绝")
	} else {
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != "auto_groups" {
			t.Fatalf("err = %v，期望 auto_groups 字段级错误", err)
		}
	}
	if hasRow(t, db, `SELECT EXISTS (SELECT 1 FROM api_tokens WHERE user_id = ? AND name = 'auto-empty')`, userID) {
		t.Fatal("被拒绝的 auto 空列表令牌不得落库")
	}

	// (c) 恰好 20 接受。
	twenty := whitelist[:20]
	created, err := st.Create(ctx, userID, CreateInput{
		Name: "auto-20", Group: "auto", Quantity: 1, AutoGroups: twenty, CrossGroupRetry: true,
	})
	if err != nil {
		t.Fatalf("恰好 20 项应接受: %v", err)
	}
	var storedAuto string
	var storedRetry bool
	if err := db.Raw(`SELECT auto_groups, cross_group_retry FROM api_tokens WHERE id = ?`, created[0].Token.ID).
		Row().Scan(&storedAuto, &storedRetry); err != nil {
		t.Fatalf("read auto row: %v", err)
	}
	var roundTrip []string
	if err := json.Unmarshal([]byte(storedAuto), &roundTrip); err != nil {
		t.Fatalf("auto_groups 不是 JSON 数组: %v (%q)", err, storedAuto)
	}
	if len(roundTrip) != 20 || roundTrip[0] != twenty[0] || roundTrip[19] != twenty[19] {
		t.Fatalf("auto_groups 顺序未保留: %v", roundTrip)
	}
	if !storedRetry {
		t.Fatal("cross_group_retry 应落 true")
	}

	// 21 项拒绝。
	if _, err := st.Create(ctx, userID, CreateInput{
		Name: "auto-21", Group: "auto", Quantity: 1, AutoGroups: whitelist,
	}); err == nil {
		t.Fatal("21 项应在写入期拒绝")
	}

	// I-23：非 auto 分组时强制清空并置假。
	specific, err := st.Create(ctx, userID, CreateInput{
		Name: "specific", Group: base, Quantity: 1, AutoGroups: twenty, CrossGroupRetry: true,
	})
	if err != nil {
		t.Fatalf("非 auto 创建: %v", err)
	}
	if err := db.Raw(`SELECT auto_groups, cross_group_retry FROM api_tokens WHERE id = ?`, specific[0].Token.ID).
		Row().Scan(&storedAuto, &storedRetry); err != nil {
		t.Fatalf("read specific row: %v", err)
	}
	if storedAuto != "" || storedRetry {
		t.Fatalf("非 auto 分组应 auto_groups='' 且 retry=false，实际 %q/%v", storedAuto, storedRetry)
	}

	// auto → 具体分组：服务端清空（I-23）。
	updated, err := st.Update(ctx, userID, created[0].Token.ID, UpdateInput{
		UpdatedAt: &created[0].Token.UpdatedAt,
		Group:     strPtr(base),
	})
	if err != nil {
		t.Fatalf("Update auto→specific: %v", err)
	}
	if len(updated.AutoGroups) != 0 || updated.CrossGroupRetry {
		t.Fatalf("改回具体分组后应清空 auto_groups/retry，实际 %v/%v", updated.AutoGroups, updated.CrossGroupRetry)
	}
}

// TestIntegrationCacheInvalidationPubSub 覆盖 brief (e)/GC-11：
// A 禁用后，B 的 Resolve/Get 缓存经 pub/sub 在 ≤1s 内失效。
func TestIntegrationCacheInvalidationPubSub(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	userID := seedUser(t, db, group)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	a := New(db, rdb, nil)
	b := New(db, rdb, nil)
	if err := a.Subscribe(ctx); err != nil {
		t.Fatalf("A Subscribe: %v", err)
	}
	if err := b.Subscribe(ctx); err != nil {
		t.Fatalf("B Subscribe: %v", err)
	}

	created, err := a.Create(ctx, userID, CreateInput{Name: "cached", Group: group, Quantity: 1, UnlimitedQuota: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok := created[0].Token
	hash := auth.HashTokenKey(created[0].Key)

	// 预热 B 的两级缓存。
	if _, err := b.Resolve(ctx, hash); err != nil {
		t.Fatalf("B 首次 Resolve: %v", err)
	}
	if _, err := b.Get(ctx, userID, tok.ID); err != nil {
		t.Fatalf("B 首次 Get: %v", err)
	}

	// A 禁用并直接校验自身缓存已失效（不依赖 pub/sub 时延）。
	if _, err := a.Disable(ctx, userID, tok.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := a.Resolve(ctx, hash); !errors.Is(err, ErrTokenDisabled) {
		t.Fatalf("A 禁用后 Resolve err = %v，期望 ErrTokenDisabled", err)
	}

	// B 必须在 1s 内经 token:changed 失效（轮询上限 950ms）。
	deadline := time.Now().Add(950 * time.Millisecond)
	for {
		_, err := b.Resolve(ctx, hash)
		if errors.Is(err, ErrTokenDisabled) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("B 在 950ms 内未通过 pub/sub 看到禁用状态（err=%v）", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Get 缓存同理。
	deadline = time.Now().Add(950 * time.Millisecond)
	for {
		got, err := b.Get(ctx, userID, tok.ID)
		if err == nil && got.Status == StatusDisabled {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("B 的 Get 缓存在 950ms 内未失效（status=%v err=%v）", gotStatus(got), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func gotStatus(t *Token) string {
	if t == nil {
		return "<nil>"
	}
	return t.Status
}

// TestIntegrationBatchPartialFailure 覆盖 brief (f)：逐条独立结果 + 失败明细，
// 且他人令牌在失败明细中不得泄露名称。
func TestIntegrationBatchPartialFailure(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	owner := seedUser(t, db, group)
	other := seedUser(t, db, group)
	st := New(db, rdb, nil)
	ctx := context.Background()

	mk := func(userID int64, name string) *Token {
		created, err := st.Create(ctx, userID, CreateInput{Name: name, Group: group, Quantity: 1, UnlimitedQuota: true})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return created[0].Token
	}
	t1 := mk(owner, "one")
	t2 := mk(owner, "two")
	foreign := mk(other, "foreign-secret-name")
	missing := int64(99999999)

	succeeded, failed, err := st.BatchDisable(ctx, owner, []int64{t1.ID, foreign.ID, missing, t2.ID})
	if err != nil {
		t.Fatalf("BatchDisable: %v", err)
	}
	if succeeded != 2 {
		t.Fatalf("succeeded = %d，期望 2", succeeded)
	}
	if len(failed) != 2 {
		t.Fatalf("failed = %d，期望 2（他人令牌 + 不存在）", len(failed))
	}
	for _, f := range failed {
		if f.ID == foreign.ID || f.ID == missing {
			if f.Name != "" {
				t.Fatalf("失败明细泄露了名称: %+v", f)
			}
			if f.Detail == "" {
				t.Fatalf("失败明细缺 detail: %+v", f)
			}
		}
	}
	// 他人令牌必须仍然 enabled。
	got, err := st.Get(ctx, other, foreign.ID)
	if err != nil || got.Status != StatusEnabled {
		t.Fatalf("他人令牌被批量操作改动: status=%v err=%v", gotStatus(got), err)
	}
}

// TestIntegrationOptimisticLock 覆盖 brief (g)：PATCH 乐观锁 409。
func TestIntegrationOptimisticLock(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	owner := seedUser(t, db, group)
	other := seedUser(t, db, group)
	st := New(db, rdb, nil)
	ctx := context.Background()

	created, err := st.Create(ctx, owner, CreateInput{Name: "locked", Group: group, Quantity: 1, UnlimitedQuota: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tok := created[0].Token

	updated, err := st.Update(ctx, owner, tok.ID, UpdateInput{
		UpdatedAt: &tok.UpdatedAt,
		Name:      strPtr("locked-v2"),
	})
	if err != nil {
		t.Fatalf("首次 Update: %v", err)
	}
	if updated.Name != "locked-v2" {
		t.Fatalf("name = %q，期望 locked-v2", updated.Name)
	}

	// 用旧 updated_at 再写 → 409 语义的 ErrConflict。
	if _, err := st.Update(ctx, owner, tok.ID, UpdateInput{
		UpdatedAt: &tok.UpdatedAt,
		Name:      strPtr("stale"),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧版本 Update err = %v，期望 ErrConflict", err)
	}

	// 他人访问 → ErrNotFound（不泄露存在性）。
	if _, err := st.Update(ctx, other, tok.ID, UpdateInput{UpdatedAt: &updated.UpdatedAt, Name: strPtr("nope")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("他人 Update err = %v，期望 ErrNotFound", err)
	}
	// 带陈旧版本的 DELETE 同样 409。
	if err := st.Delete(ctx, owner, tok.ID, &tok.UpdatedAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("陈旧版本 Delete err = %v，期望 ErrConflict", err)
	}
}

// TestIntegrationResetKeyAndDelete 覆盖重置/删除的令牌失效语义。
func TestIntegrationResetKeyAndDelete(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	owner := seedUser(t, db, group)
	st := New(db, rdb, nil)
	ctx := context.Background()

	created, err := st.Create(ctx, owner, CreateInput{Name: "reset", Group: group, Quantity: 1, UnlimitedQuota: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldKey := created[0].Key
	oldHash := auth.HashTokenKey(oldKey)
	if _, err := st.Resolve(ctx, oldHash); err != nil {
		t.Fatalf("Resolve old: %v", err)
	}

	newKey, masked, tok, err := st.ResetKey(ctx, owner, created[0].Token.ID)
	if err != nil {
		t.Fatalf("ResetKey: %v", err)
	}
	if newKey == oldKey {
		t.Fatal("重置后明文未变化")
	}
	if masked != "nkt-"+strings.Repeat("•", 24) {
		t.Fatalf("masked = %q", masked)
	}
	if _, err := st.Resolve(ctx, oldHash); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("旧 key Resolve err = %v，期望 ErrInvalidToken", err)
	}
	meta, err := st.Resolve(ctx, auth.HashTokenKey(newKey))
	if err != nil || meta.ID != tok.ID {
		t.Fatalf("新 key Resolve = %+v, %v", meta, err)
	}

	if err := st.Delete(ctx, owner, tok.ID, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, owner, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get err = %v，期望 ErrNotFound", err)
	}
	if _, err := st.Resolve(ctx, auth.HashTokenKey(newKey)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("删除后 Resolve err = %v，期望 ErrInvalidToken", err)
	}
}

// TestIntegrationResolveStatusSemantics：disabled→ErrTokenDisabled、expired→ErrTokenExpired、
// exhausted 仍返回 meta（自读用量端点要能 200）。
func TestIntegrationResolveStatusSemantics(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	owner := seedUser(t, db, group)
	st := New(db, rdb, nil)
	ctx := context.Background()

	// disabled。
	disabled, err := st.Create(ctx, owner, CreateInput{Name: "disabled", Group: group, Quantity: 1, UnlimitedQuota: true})
	if err != nil {
		t.Fatalf("Create disabled: %v", err)
	}
	if _, err := st.Disable(ctx, owner, disabled[0].Token.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := st.Resolve(ctx, auth.HashTokenKey(disabled[0].Key)); !errors.Is(err, ErrTokenDisabled) {
		t.Fatalf("disabled err = %v，期望 ErrTokenDisabled", err)
	}

	// expired：直接改 DB（created_at 提前，满足 CHECK）。
	expired, err := st.Create(ctx, owner, CreateInput{Name: "expired", Group: group, Quantity: 1, UnlimitedQuota: true})
	if err != nil {
		t.Fatalf("Create expired: %v", err)
	}
	if err := db.Exec(`UPDATE api_tokens SET created_at = now() - interval '2 hours',
		expires_at = now() - interval '1 hour' WHERE id = ?`, expired[0].Token.ID).Error; err != nil {
		t.Fatalf("age token: %v", err)
	}
	fresh := New(db, rdb, nil) // 空缓存，避免读到旧值
	if _, err := fresh.Resolve(ctx, auth.HashTokenKey(expired[0].Key)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired err = %v，期望 ErrTokenExpired", err)
	}

	// exhausted：remain_quota=0 且非不限 → Resolve 返回 meta（由 usage 端点决定 200）。
	exhausted, err := st.Create(ctx, owner, CreateInput{
		Name: "exhausted", Group: group, Quantity: 1, UnlimitedQuota: false, RemainQuota: 0,
	})
	if err != nil {
		t.Fatalf("Create exhausted: %v", err)
	}
	meta, err := fresh.Resolve(ctx, auth.HashTokenKey(exhausted[0].Key))
	if err != nil {
		t.Fatalf("exhausted Resolve err = %v，期望 nil", err)
	}
	if meta.UnlimitedQuota || meta.RemainQuota != 0 {
		t.Fatalf("meta = %+v，期望 RemainQuota=0 且非不限", meta)
	}

	// 完整明文哈希：去掉 nkt- 前缀的哈希不得命中。
	trimmedHash := auth.HashTokenKey(strings.TrimPrefix(exhausted[0].Key, "nkt-"))
	if _, err := fresh.Resolve(ctx, trimmedHash); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("去前缀哈希 err = %v，期望 ErrInvalidToken（必须哈希完整明文）", err)
	}
}

// TestIntegrationAutoGroupsCandidateSet 覆盖 §2.9 候选集：
// 站点白名单 ∩ 用户可选（启用 + user_selectable）∩ 有倍率，按白名单顺序。
func TestIntegrationAutoGroupsCandidateSet(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	selectable := "tg_sel_" + suffix
	hidden := "tg_hid_" + suffix
	disabled := "tg_dis_" + suffix
	seedGroup(t, db, selectable, true, 1, 150)
	seedGroup(t, db, hidden, false, 1, 100)
	seedGroup(t, db, disabled, true, 2, 100)
	userID := seedUser(t, db, selectable)

	settings := mapSettings{
		KeyAutoGroups:         mustJSON(t, []string{hidden, selectable, disabled, "missing_" + suffix}),
		KeyMaxAutoGroups:      json.RawMessage(`7`),
		KeyCrossGroupRetryMax: json.RawMessage(`3`),
	}
	st := New(db, rdb, settings)
	groups, max, err := st.AutoGroups(context.Background(), userID)
	if err != nil {
		t.Fatalf("AutoGroups: %v", err)
	}
	if max != 7 {
		t.Fatalf("max = %d，期望 7", max)
	}
	if len(groups) != 1 || groups[0].Name != selectable {
		t.Fatalf("候选集 = %+v，期望仅 %q", groups, selectable)
	}
	if groups[0].Ratio != 1.5 {
		t.Fatalf("ratio = %v，期望 1.5（150/100）", groups[0].Ratio)
	}
	if st.CrossGroupRetryMaxGroups() != 3 {
		t.Fatalf("cross group retry max = %d，期望 3", st.CrossGroupRetryMaxGroups())
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}

func strPtr(s string) *string { return &s }
