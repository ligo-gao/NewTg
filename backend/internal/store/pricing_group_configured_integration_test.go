//go:build integration

package store

// W7/M2 的 R-64 存储侧验收：GroupConfigured 必须是严格存在性查询
// （user_groups.name=? AND status=1），且**不缓存**——分组删除/停用后
// 立即返回 false，绝不让陈旧 true 进入结算。

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPricingRepoGroupConfiguredStrict(t *testing.T) {
	db := openTestDB(t)
	repo := NewPricingRepo(db)
	ctx := context.Background()
	group := fmt.Sprintf("m2gc%d", time.Now().UnixNano()%1_000_000_000)

	if _, err := db.Exec(`INSERT INTO user_groups (name, ratio, user_selectable, status)
		VALUES ($1, 100, true, 1)`, group); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM user_groups WHERE name = $1`, group) })

	// 存在且启用 → true。
	ok, err := repo.GroupConfigured(ctx, group)
	if err != nil || !ok {
		t.Fatalf("GroupConfigured(enabled) = (%v,%v)，期望 (true,nil)", ok, err)
	}
	// 停用（status=2）→ 立即 false（无缓存）。
	if _, err := db.Exec(`UPDATE user_groups SET status = 2 WHERE name = $1`, group); err != nil {
		t.Fatalf("disable group: %v", err)
	}
	ok, err = repo.GroupConfigured(ctx, group)
	if err != nil || ok {
		t.Fatalf("GroupConfigured(disabled) = (%v,%v)，期望 (false,nil)", ok, err)
	}
	// 重新启用 → true；物理删除 → 立即 false（证明没有陈旧缓存）。
	if _, err := db.Exec(`UPDATE user_groups SET status = 1 WHERE name = $1`, group); err != nil {
		t.Fatalf("re-enable group: %v", err)
	}
	if ok, err = repo.GroupConfigured(ctx, group); err != nil || !ok {
		t.Fatalf("GroupConfigured(re-enabled) = (%v,%v)，期望 (true,nil)", ok, err)
	}
	if _, err := db.Exec(`DELETE FROM user_groups WHERE name = $1`, group); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	ok, err = repo.GroupConfigured(ctx, group)
	if err != nil || ok {
		t.Fatalf("GroupConfigured(deleted) = (%v,%v)，期望 (false,nil)：不得返回陈旧 true", ok, err)
	}
	// 从未存在的分组 → false。
	if ok, err = repo.GroupConfigured(ctx, group+"_missing"); err != nil || ok {
		t.Fatalf("GroupConfigured(missing) = (%v,%v)，期望 (false,nil)", ok, err)
	}
}
