//go:build integration

package store

// 插件写路径的数据库级验收：真实 PostgreSQL 上验证唯一约束、部分唯一索引、
// FOR UPDATE 串行化与事务回滚。单测 fake 只能模拟结果，证明不了这些性质。
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_m6d_t5?sslmode=disable' \
//	  go test -tags=integration ./internal/store/ -run TestTaskPlugin -v

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func cleanPluginTables(t *testing.T) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	for _, query := range []string{
		`DELETE FROM task_plugin_versions`,
		`DELETE FROM task_plugins`,
		`DELETE FROM plugin_marketplace_sources`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("clean %q: %v", query, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugin_versions`)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM task_plugins`)
		_, _ = db.ExecContext(context.Background(), `DELETE FROM plugin_marketplace_sources`)
	})
}

// uniquePluginKey 把测试名压成 DB 允许的 key（≤30，[a-z0-9-]）。
func uniquePluginKey(t *testing.T) string {
	t.Helper()
	key := "t" + time.Now().UTC().Format("150405.000000")
	key = replaceAllNonKey(stringsLower(key))
	if len(key) > 30 {
		key = key[:30]
	}
	return key
}

func stringsLower(value string) string {
	out := []byte(value)
	for i, b := range out {
		if b >= 'A' && b <= 'Z' {
			out[i] = b + 32
		}
	}
	return string(out)
}

func replaceAllNonKey(value string) string {
	out := make([]byte, 0, len(value))
	for _, b := range []byte(value) {
		switch {
		case b >= 'a' && b <= 'z', b >= '0' && b <= '9', b == '-':
			out = append(out, b)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func uploadFixture(key, version, hash string) TaskPluginUpload {
	return TaskPluginUpload{
		Key: key, Name: "Test " + key, Version: version, APIVersion: "v1",
		SourceText: "source-" + version, SourceHash: hash, UploadedBy: 1,
	}
}

func TestTaskPluginUploadIdempotentAndConflict(t *testing.T) {
	cleanPluginTables(t)
	db := openTestDB(t)
	repo := NewTaskPluginRepo(db)
	ctx := context.Background()
	key := uniquePluginKey(t)
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)

	p, v, created, err := repo.Upload(ctx, uploadFixture(key, "1.0.0", hashA))
	if err != nil || !created {
		t.Fatalf("首次上传 created=%v err=%v", created, err)
	}
	if p.Key != key || v.Version != "1.0.0" || v.Active {
		t.Fatalf("首次上传结果不符：%+v / %+v", p, v)
	}

	_, _, created, err = repo.Upload(ctx, uploadFixture(key, "1.0.0", hashA))
	if err != nil || created {
		t.Fatalf("同 hash 幂等 created=%v err=%v", created, err)
	}

	_, _, _, err = repo.Upload(ctx, uploadFixture(key, "1.0.0", hashB))
	if !errors.Is(err, ErrTaskPluginVersionConflict) {
		t.Fatalf("异 hash err = %v，期望 ErrTaskPluginVersionConflict", err)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM task_plugin_versions WHERE "key" = $1`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("冲突上传不得写入新行，count=%d", count)
	}
}

func TestTaskPluginActivateSerializesConcurrent(t *testing.T) {
	cleanPluginTables(t)
	db := openTestDB(t)
	repo := NewTaskPluginRepo(db)
	ctx := context.Background()
	key := uniquePluginKey(t)
	for i, version := range []string{"1.0.0", "2.0.0"} {
		hash := strings.Repeat(string(rune('a'+i)), 64)
		if _, _, _, err := repo.Upload(ctx, uploadFixture(key, version, hash)); err != nil {
			t.Fatalf("seed %s: %v", version, err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		version := version
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := repo.Activate(ctx, key, version, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发激活出错：%v", err)
		}
	}

	var active int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM task_plugin_versions WHERE "key" = $1 AND active`, key).Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 1 {
		t.Fatalf("并发激活后 active 行数 = %d，期望 1（部分唯一索引 + 行锁）", active)
	}
	p, err := repo.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p.ActiveVersion == nil {
		t.Fatal("active_version 镜像未同步")
	}
}

func TestTaskPluginDeleteActivePromotesPrevious(t *testing.T) {
	cleanPluginTables(t)
	db := openTestDB(t)
	repo := NewTaskPluginRepo(db)
	ctx := context.Background()
	key := uniquePluginKey(t)
	versions := []string{"1.0.0", "2.0.0", "3.0.0"}
	for i, version := range versions {
		if _, _, _, err := repo.Upload(ctx, uploadFixture(key, version, strings.Repeat(string(rune('a'+i)), 64))); err != nil {
			t.Fatalf("seed %s: %v", version, err)
		}
		// created_at 决定提升顺序；同事务内 now() 可能相同，显式错开。
		if _, err := db.ExecContext(ctx,
			`UPDATE task_plugin_versions SET created_at = now() - make_interval(hours => $3)
			 WHERE "key" = $1 AND version = $2`, key, version, 10-i); err != nil {
			t.Fatalf("stagger %s: %v", version, err)
		}
	}
	if _, _, err := repo.Activate(ctx, key, "2.0.0", nil); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	p, promoted, err := repo.DeleteVersion(ctx, key, "2.0.0", nil)
	if err != nil {
		t.Fatalf("DeleteVersion: %v", err)
	}
	// 上一版本 = 剩余行里 created_at 最新的（v3 上传晚于 v2，早于删除时的
	// active 只是展示状态；提升规则只看创建时间）。
	if promoted != "3.0.0" {
		t.Fatalf("promoted = %q，期望 3.0.0（created_at 上一版本）", promoted)
	}
	if p.ActiveVersion == nil || *p.ActiveVersion != "3.0.0" {
		t.Fatalf("镜像 active_version = %v，期望 3.0.0", p.ActiveVersion)
	}

	// 把剩余行都禁用后删除 active：无可提升版本 → 409。
	if _, err := db.ExecContext(ctx,
		`UPDATE task_plugin_versions SET enabled = false WHERE "key" = $1`, key); err != nil {
		t.Fatalf("disable remaining: %v", err)
	}
	if _, _, err := repo.DeleteVersion(ctx, key, "3.0.0", nil); !errors.Is(err, ErrTaskPluginVersionRequired) {
		t.Fatalf("无可用提升版本 err = %v，期望 ErrTaskPluginVersionRequired", err)
	}

	// 唯一版本禁删。
	key2 := key + "-one"
	if len(key2) > 30 {
		key2 = key2[:30]
	}
	if _, _, _, err := repo.Upload(ctx, uploadFixture(key2, "1.0.0", strings.Repeat("c", 64))); err != nil {
		t.Fatalf("seed key2: %v", err)
	}
	if _, _, err := repo.DeleteVersion(ctx, key2, "1.0.0", nil); !errors.Is(err, ErrTaskPluginVersionRequired) {
		t.Fatalf("唯一版本 err = %v，期望 ErrTaskPluginVersionRequired", err)
	}
}

func TestTaskPluginOptimisticLock(t *testing.T) {
	cleanPluginTables(t)
	db := openTestDB(t)
	repo := NewTaskPluginRepo(db)
	ctx := context.Background()
	key := uniquePluginKey(t)
	if _, _, _, err := repo.Upload(ctx, uploadFixture(key, "1.0.0", strings.Repeat("a", 64))); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	p, err := repo.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	stale := p.UpdatedAt.Add(-time.Hour)
	if _, err := repo.SetEnabled(ctx, key, false, &stale); !errors.Is(err, ErrTaskPluginStale) {
		t.Fatalf("陈旧版本 err = %v，期望 ErrTaskPluginStale", err)
	}
	if _, err := repo.SetEnabled(ctx, key, false, &p.UpdatedAt); err != nil {
		t.Fatalf("匹配版本应成功：%v", err)
	}
}

func TestMarketSourcesReplacePreservesCheckState(t *testing.T) {
	cleanPluginTables(t)
	db := openTestDB(t)
	repo := NewTaskPluginRepo(db)
	ctx := context.Background()

	saved, err := repo.ReplaceMarketSources(ctx, []MarketSourceInput{
		{Name: "A", URL: "https://a.example/index.json", Enabled: true},
		{Name: "B", URL: "https://b.example/index.json", Enabled: false},
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved = %+v", saved)
	}
	var idA string
	for _, source := range saved {
		if source.URL == "https://a.example/index.json" {
			idA = source.ID
		}
	}
	if _, err := repo.UpdateMarketSourceCheck(ctx, idA, "ok", "", time.Now()); err != nil {
		t.Fatalf("UpdateCheck: %v", err)
	}

	// 只改名（保留 id/URL）不应清空最近校验结果。
	saved, err = repo.ReplaceMarketSources(ctx, []MarketSourceInput{
		{ID: idA, Name: "A renamed", URL: "https://a.example/index.json", Enabled: true},
	})
	if err != nil {
		t.Fatalf("Replace 2: %v", err)
	}
	if len(saved) != 1 || saved[0].Name != "A renamed" || saved[0].LastCheckStatus != "ok" {
		t.Fatalf("替换应保留校验状态：%+v", saved)
	}

	if _, err := repo.ReplaceMarketSources(ctx, []MarketSourceInput{
		{Name: "dup", URL: "https://x.example/i.json"},
		{Name: "dup2", URL: "https://x.example/i.json"},
	}); !errors.Is(err, ErrMarketSourceURLConflict) {
		t.Fatalf("重复 URL err = %v，期望 ErrMarketSourceURLConflict", err)
	}
}

func TestTaskPluginBuiltinUpsertAndMetadataListing(t *testing.T) {
	cleanPluginTables(t)
	db := openTestDB(t)
	repo := NewTaskPluginRepo(db)
	ctx := context.Background()
	key := uniquePluginKey(t)
	description := "builtin description"

	if err := repo.UpsertBuiltinPlugins(ctx, []BuiltinPlugin{
		{Key: key, Name: "Builtin", Description: &description},
	}); err != nil {
		t.Fatalf("UpsertBuiltinPlugins: %v", err)
	}
	// 幂等：再次装配不应改变 enabled/active_version。
	if err := repo.UpsertBuiltinPlugins(ctx, []BuiltinPlugin{
		{Key: key, Name: "Builtin renamed", Description: &description},
	}); err != nil {
		t.Fatalf("UpsertBuiltinPlugins 2: %v", err)
	}
	p, err := repo.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !p.Builtin || p.Source != "builtin" || p.Name != "Builtin renamed" || p.ActiveVersion != nil {
		t.Fatalf("内置行不符：%+v", p)
	}

	// 覆盖层上传保留内置事实，版本列表不含源码、GetVersion 单独返回正文。
	if _, _, _, err := repo.Upload(ctx, TaskPluginUpload{
		Key: key, Name: "Builtin", Version: "1.0.0", APIVersion: "v1",
		SourceText: "builtin override source", SourceHash: strings.Repeat("d", 64),
		UploadedBy: 1, Builtin: true,
	}); err != nil {
		t.Fatalf("Upload override: %v", err)
	}
	p, err = repo.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after upload: %v", err)
	}
	if !p.Builtin || p.Source != "builtin" {
		t.Fatalf("上传覆盖层不得改写内置来源：%+v", p)
	}
	versions, err := repo.ListVersions(ctx, key)
	if err != nil || len(versions) != 1 {
		t.Fatalf("ListVersions = %+v, %v", versions, err)
	}
	if versions[0].SourceText != "" {
		t.Fatalf("版本列表不得返回源码正文：%q", versions[0].SourceText)
	}
	full, err := repo.GetVersion(ctx, key, "1.0.0")
	if err != nil || full.SourceText != "builtin override source" {
		t.Fatalf("GetVersion = %+v, %v", full, err)
	}
}
