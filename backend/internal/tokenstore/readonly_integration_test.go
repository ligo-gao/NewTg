//go:build integration

package tokenstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
)

// 只读鉴权接缝（modules-05 F-05-11 / D2-02）的集成验收：
//   - 过期、额度耗尽仍返回 meta（只读面可查）；
//   - 显式禁用、未知 key 拒绝；
//   - 与 Resolve 共用缓存时口径不串味（缓存失效后立即反映禁用）。
//
// 运行：NEWTG_TEST_DB_DSN=... NEWTG_TEST_REDIS_ADDR=... go test -tags=integration ./internal/tokenstore/
func TestIntegrationResolveReadOnly(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	group := "tg_ro_" + suffix
	seedGroup(t, db, group, true, 1, 100)
	userID := seedUser(t, db, group)
	st := New(db, rdb, nil)
	ctx := context.Background()

	create := func(name string, unlimited bool, remain int64) *Created {
		t.Helper()
		created, err := st.Create(ctx, userID, CreateInput{
			Name: name, Group: group, Quantity: 1,
			UnlimitedQuota: unlimited, RemainQuota: remain,
		})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return &created[0]
	}

	t.Run("enabled", func(t *testing.T) {
		c := create("ro-enabled", true, 0)
		meta, err := st.ResolveReadOnly(ctx, auth.HashTokenKey(c.Key))
		if err != nil {
			t.Fatalf("ResolveReadOnly(enabled): %v", err)
		}
		if meta.ID != c.Token.ID || meta.UserID != userID {
			t.Fatalf("meta = %+v，期望 id=%d user=%d", meta, c.Token.ID, userID)
		}
	})

	t.Run("expired_is_readable", func(t *testing.T) {
		past := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Microsecond)
		c := create("ro-expired", true, 0)
		if err := db.Exec(`UPDATE api_tokens SET created_at = ?, expires_at = ? WHERE id = ?`,
			past.Add(-24*time.Hour), past, c.Token.ID).Error; err != nil {
			t.Fatalf("expire token: %v", err)
		}
		// 严格 Resolve 必须拒绝过期令牌（对照组：证明两条路径口径不同）。
		if _, err := st.Resolve(ctx, auth.HashTokenKey(c.Key)); !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("Resolve(expired) = %v，期望 ErrTokenExpired", err)
		}
		// 只读面必须放行，且第二次走缓存仍然放行（判定在缓存之后）。
		for i := 0; i < 2; i++ {
			meta, err := st.ResolveReadOnly(ctx, auth.HashTokenKey(c.Key))
			if err != nil {
				t.Fatalf("ResolveReadOnly(expired) 第 %d 次: %v", i+1, err)
			}
			if meta.ID != c.Token.ID {
				t.Fatalf("meta.ID = %d，期望 %d", meta.ID, c.Token.ID)
			}
		}
	})

	t.Run("exhausted_is_readable", func(t *testing.T) {
		c := create("ro-exhausted", false, 0)
		meta, err := st.ResolveReadOnly(ctx, auth.HashTokenKey(c.Key))
		if err != nil {
			t.Fatalf("ResolveReadOnly(exhausted): %v", err)
		}
		if meta.RemainQuota != 0 || meta.UnlimitedQuota {
			t.Fatalf("meta remain=%d unlimited=%v，期望 0/false", meta.RemainQuota, meta.UnlimitedQuota)
		}
	})

	t.Run("disabled_rejected_even_after_cache", func(t *testing.T) {
		c := create("ro-disabled", true, 0)
		// 先填充缓存，再禁用：验证 Store.Disable 的失效链对只读路径同样生效。
		if _, err := st.ResolveReadOnly(ctx, auth.HashTokenKey(c.Key)); err != nil {
			t.Fatalf("warm cache: %v", err)
		}
		if _, err := st.Disable(ctx, userID, c.Token.ID); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if _, err := st.ResolveReadOnly(ctx, auth.HashTokenKey(c.Key)); !errors.Is(err, ErrTokenDisabled) {
			t.Fatalf("ResolveReadOnly(disabled) = %v，期望 ErrTokenDisabled", err)
		}
	})

	t.Run("unknown_and_empty", func(t *testing.T) {
		if _, err := st.ResolveReadOnly(ctx, auth.HashTokenKey("nkt-does-not-exist")); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("unknown key = %v，期望 ErrInvalidToken", err)
		}
		if _, err := st.ResolveReadOnly(ctx, ""); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("empty key = %v，期望 ErrInvalidToken", err)
		}
	})
}
