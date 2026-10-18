package tokenstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newMiniredisStore 返回只带 Redis 的 Store（缓存语义测试不需要 DB）。
func newMiniredisStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(nil, rdb, nil), mr
}

// TestExternalChangeBumpsGenerationBeforeInvalidation 覆盖 D1-1：
// 外部写路径（封禁/删用户/删账号）只广播 token:changed，订阅者必须先把
// token:gen 自增再 DEL，否则在途回填（捕获了旧代际）会通过 WATCH 把被删除/
// 禁用的旧行写回缓存并存活 60s。
func TestExternalChangeBumpsGenerationBeforeInvalidation(t *testing.T) {
	st, mr := newMiniredisStore(t)
	ctx := context.Background()

	token := &Token{ID: 7, UserID: 11, Name: "prod", Status: StatusEnabled}
	const keyHash = "hash-old"

	// 读者在失效前捕获代际并预热缓存（idhash 反向索引让失效能找到 meta 键）。
	gen, ok := st.cacheGeneration(ctx)
	if !ok {
		t.Fatal("cacheGeneration 应可用")
	}
	st.storeCachedToken(ctx, gen, token, keyHash)
	if _, cached := st.loadCachedToken(ctx, 7); !cached {
		t.Fatal("预热缓存失败（测试装置失效）")
	}

	// 外部写路径提交后广播；订阅者执行失效。
	st.invalidateOnChange(ctx, 7)

	if got, err := mr.Get(cacheGenerationKey); err != nil || got != "1" {
		t.Fatalf("token:gen = %q (err=%v)，期望 1：外部失效必须先推进代际", got, err)
	}

	// 在途回填携带旧代际：必须被 WATCH 拒绝，不得写回旧行。
	st.storeCachedToken(ctx, gen, token, keyHash)
	if _, cached := st.loadCachedToken(ctx, 7); cached {
		t.Fatal("外部失效后，旧代际的在途回填把令牌写回了 view 缓存")
	}
	if mr.Exists(cacheMetaKey(keyHash)) {
		t.Fatal("外部失效后，旧代际的在途回填把令牌写回了 meta 缓存")
	}
	if mr.Exists(cacheIDHashKey(7)) {
		t.Fatal("外部失效后 idhash 反向索引仍然存在")
	}
}

// TestInvalidationIgnoresCanceledRequestContext 覆盖 D1-8：DB 提交后客户端
// 断开（请求 ctx 已取消）不得让 INCR/DEL/PUBLISH 全部失败。
func TestInvalidationIgnoresCanceledRequestContext(t *testing.T) {
	st, mr := newMiniredisStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 预置三类缓存键与订阅，验证失效三步都真的执行了。
	if err := st.rdb.Set(ctx, cacheViewKey(9), "view", CacheTTL).Err(); err != nil {
		t.Fatalf("seed view: %v", err)
	}
	if err := st.rdb.Set(ctx, cacheIDHashKey(9), "hash-x", CacheTTL).Err(); err != nil {
		t.Fatalf("seed idhash: %v", err)
	}
	if err := st.rdb.Set(ctx, cacheMetaKey("hash-x"), "meta", CacheTTL).Err(); err != nil {
		t.Fatalf("seed meta: %v", err)
	}
	pubsub := st.rdb.Subscribe(context.Background(), "token:changed:9")
	t.Cleanup(func() { _ = pubsub.Close() })
	if _, err := pubsub.Receive(context.Background()); err != nil {
		t.Fatalf("establish subscription: %v", err)
	}

	cancel() // 模拟客户端在 DB 提交后立即断开
	st.invalidateToken(ctx, 9, "hash-x")

	if got, err := mr.Get(cacheGenerationKey); err != nil || got != "1" {
		t.Fatalf("token:gen = %q (err=%v)，期望 1（INCR 不得被请求取消打断）", got, err)
	}
	for _, key := range []string{cacheViewKey(9), cacheIDHashKey(9), cacheMetaKey("hash-x")} {
		if mr.Exists(key) {
			t.Fatalf("缓存键 %q 未删除（DEL 被请求取消打断）", key)
		}
	}
	select {
	case msg := <-pubsub.Channel():
		if msg.Payload != "9" {
			t.Fatalf("token:changed payload = %q，期望 9", msg.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("失效路径未广播 token:changed（PUBLISH 被请求取消打断）")
	}
}

// ---------------------------------------------------------------------------
// Get 的缓存写入顺序（D1-7）——用固定行的假驱动 + miniredis 做无外部依赖单测。
// ---------------------------------------------------------------------------

type oneRowDriver struct{}

func (oneRowDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("oneRowDriver: Open is unused")
}

type oneRowConnector struct{ row tokenRow }

func (c oneRowConnector) Connect(context.Context) (driver.Conn, error) {
	return &oneRowConn{row: c.row}, nil
}

func (c oneRowConnector) Driver() driver.Driver { return oneRowDriver{} }

type oneRowConn struct{ row tokenRow }

func (c *oneRowConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("oneRowConn: Prepare is unused")
}
func (c *oneRowConn) Close() error { return nil }
func (c *oneRowConn) Begin() (driver.Tx, error) {
	return nil, errors.New("oneRowConn: Begin is unused")
}

// QueryContext 忽略 SQL：测试只模拟 loadRow 的 `SELECT * FROM api_tokens`。
func (c *oneRowConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &oneRowRows{row: c.row}, nil
}

type oneRowRows struct {
	row  tokenRow
	done bool
}

func (r *oneRowRows) Columns() []string {
	return []string{
		"id", "user_id", "name", "key_hash", "status", "expires_at",
		"model_limits", "group", "remain_quota", "allow_ips", "last_used_at",
		"created_at", "updated_at", "auto_groups", "cross_group_retry",
	}
}
func (r *oneRowRows) Close() error { return nil }

func (r *oneRowRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	nullableString := func(v *string) driver.Value {
		if v == nil {
			return nil
		}
		return *v
	}
	nullableInt64 := func(v *int64) driver.Value {
		if v == nil {
			return nil
		}
		return *v
	}
	nullableTime := func(v *time.Time) driver.Value {
		if v == nil {
			return nil
		}
		return *v
	}
	dest[0] = r.row.ID
	dest[1] = r.row.UserID
	dest[2] = r.row.Name
	dest[3] = r.row.KeyHash
	dest[4] = r.row.Status
	dest[5] = nullableTime(r.row.ExpiresAt)
	dest[6] = nullableString(r.row.ModelLimits)
	dest[7] = r.row.Group
	dest[8] = nullableInt64(r.row.RemainQuota)
	dest[9] = nullableString(r.row.AllowIPs)
	dest[10] = nullableTime(r.row.LastUsedAt)
	dest[11] = r.row.CreatedAt
	dest[12] = r.row.UpdatedAt
	dest[13] = nullableString(r.row.AutoGroups)
	dest[14] = r.row.CrossGroupRetry
	return nil
}

func newOneRowStore(t *testing.T, row tokenRow) (*Store, *miniredis.Miniredis) {
	t.Helper()
	sqlDB := sql.OpenDB(oneRowConnector{row: row})
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open fake gorm: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return New(db, rdb, nil), mr
}

// TestGetRejectsOtherUsersTokenBeforeCaching 覆盖 D1-7：跨用户 Get 必须在
// 归属校验之后才允许写缓存，否则任何已认证用户都能用他人 token ID 触发
// 跨租户的 view/key_hash 缓存写入。
func TestGetRejectsOtherUsersTokenBeforeCaching(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	st, mr := newOneRowStore(t, tokenRow{
		ID: 5, UserID: 99, Name: "other", KeyHash: "hash-99",
		Status: StatusEnabled, CreatedAt: now, UpdatedAt: now,
	})
	ctx := context.Background()

	if _, err := st.Get(ctx, 7, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨用户 Get err = %v，期望 ErrNotFound", err)
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("跨用户 Get 产生了缓存键: %v", keys)
	}

	// 正向对照：本人 Get 会回填缓存，证明上面的空缓存不是测试装置失效。
	got, err := st.Get(ctx, 99, 5)
	if err != nil {
		t.Fatalf("本人 Get: %v", err)
	}
	if got.ID != 5 {
		t.Fatalf("本人 Get ID = %d，期望 5", got.ID)
	}
	if !mr.Exists(cacheViewKey(5)) {
		t.Fatal("本人 Get 未回填 view 缓存（假驱动路径失效）")
	}
	if hash, err := mr.Get(cacheIDHashKey(5)); err != nil || hash != "hash-99" {
		t.Fatalf("idhash = %q (err=%v)，期望 hash-99", hash, err)
	}
}
