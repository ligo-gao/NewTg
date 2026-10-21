//go:build integration

package tokenstore

// W7/M2 的 tokenstore 侧集成验收（R-67）：resolveMeta 的投影/缓存/clone 必须
// 与生产接缝 store.TokenRepo.Resolve 同口径——auto_groups / cross_group_retry
// 以及既有白名单列都要能从 DB 与 Redis 缓存原样还原，令牌更新后缓存失效。

import (
	"context"
	"encoding/json"
	"testing"

	"newtg/gateway/internal/auth"
)

// m2Settings 是最小配置源（token.auto_groups 白名单）。
type m2Settings map[string]json.RawMessage

func (s m2Settings) Get(key string) (json.RawMessage, bool) {
	value, ok := s[key]
	return value, ok
}

func TestIntegrationResolveMetaProjectsAutoFields(t *testing.T) {
	db, rdb := openIntegration(t)
	suffix := uniqueSuffix()
	g1, g2 := "tg_m2a_"+suffix, "tg_m2b_"+suffix
	seedGroup(t, db, g1, true, 1, 100)
	seedGroup(t, db, g2, true, 1, 300)
	userID := seedUser(t, db, g1)

	settings := m2Settings{
		KeyAutoGroups: json.RawMessage(`["` + g1 + `","` + g2 + `"]`),
	}
	store := New(db, rdb, settings)
	ctx := context.Background()
	created, err := store.Create(ctx, userID, CreateInput{
		Name:            "m2-auto",
		Group:           GroupAuto,
		UnlimitedQuota:  true,
		ModelLimits:     []string{"allowed-model"},
		AllowIPs:        []string{"10.0.0.0/8"},
		AutoGroups:      []string{g1, g2},
		CrossGroupRetry: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token := created[0].Token
	keyHash := auth.HashTokenKey(created[0].Key)

	meta, err := store.Resolve(ctx, keyHash)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertAutoProjection(t, meta, g1, g2, true)

	// 第二次 Resolve 走 Redis 缓存（同一 key_hash）：缓存往返不得丢字段。
	meta, err = store.Resolve(ctx, keyHash)
	if err != nil {
		t.Fatalf("Resolve (cached): %v", err)
	}
	assertAutoProjection(t, meta, g1, g2, true)

	// 令牌更新（关闭跨组重试）必须使缓存失效：下一次 Resolve 反映新值，
	// 而 AutoGroups 保持不变（部分更新语义）。
	updatedAt := token.UpdatedAt
	if _, err := store.Update(ctx, userID, token.ID, UpdateInput{
		UpdatedAt:       &updatedAt,
		CrossGroupRetry: boolPtr(false),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	meta, err = store.Resolve(ctx, keyHash)
	if err != nil {
		t.Fatalf("Resolve (after update): %v", err)
	}
	if meta.CrossGroupRetry {
		t.Fatal("令牌更新后缓存未失效：CrossGroupRetry 仍为 true")
	}
	if len(meta.AutoGroups) != 2 || meta.AutoGroups[0] != g1 || meta.AutoGroups[1] != g2 {
		t.Fatalf("AutoGroups = %v，期望 [%s %s]（部分更新不得清空）", meta.AutoGroups, g1, g2)
	}
}

func assertAutoProjection(t *testing.T, meta *auth.TokenMeta, g1, g2 string, retry bool) {
	t.Helper()
	if meta == nil {
		t.Fatal("meta = nil")
	}
	if meta.Group != GroupAuto || meta.CrossGroupRetry != retry {
		t.Fatalf("group/retry = %q/%v，期望 auto/%v", meta.Group, meta.CrossGroupRetry, retry)
	}
	if len(meta.AutoGroups) != 2 || meta.AutoGroups[0] != g1 || meta.AutoGroups[1] != g2 {
		t.Fatalf("AutoGroups = %v，期望 [%s %s]（顺序即路由优先级）", meta.AutoGroups, g1, g2)
	}
	if len(meta.ModelLimits) != 1 || meta.ModelLimits[0] != "allowed-model" {
		t.Fatalf("ModelLimits = %v", meta.ModelLimits)
	}
	if len(meta.AllowIPs) != 1 || meta.AllowIPs[0] != "10.0.0.0/8" {
		t.Fatalf("AllowIPs = %v", meta.AllowIPs)
	}
}

func boolPtr(v bool) *bool { return &v }
