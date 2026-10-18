//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 本文件是 Task 9 的集成验收：SQLSelfRepo 对真实 PG16。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationSelf -v
//
// 未设置环境变量时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。
// 单测用 fakeSelfRepo 覆盖 HTTP 语义，这里只验证 SQL 接缝本身：jsonb 的
// COALESCE/NULLIF 语义、updated_at 乐观锁的「零行即冲突」、以及列扫描。

func TestIntegrationSelfSQLRepo(t *testing.T) {
	db, _ := openAuthIntegration(t)
	ensureDefaultGroup(t, db)
	repo := NewSQLSelfRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	suffix := uniqueAuthSuffix()
	username := "self_" + suffix
	var userID int64
	// 只填 username：display_name / email / avatar_url / sidebar_modules 保持 NULL，
	// 用来冻结「NULL 投影为零值指针」的语义。
	if err := db.QueryRow(`INSERT INTO users (username) VALUES ($1) RETURNING id`, username).
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, userID) })

	// (1) GetSelf：四个可空列全为 NULL。
	rec, err := repo.GetSelf(ctx, userID)
	if err != nil {
		t.Fatalf("GetSelf: %v", err)
	}
	if rec.Username != username {
		t.Fatalf("username = %q，期望 %q", rec.Username, username)
	}
	if rec.DisplayName != nil || rec.Email != nil || rec.AvatarURL != nil || rec.SidebarModules != nil {
		t.Fatalf("NULL 列投影错误: display_name=%v email=%v avatar_url=%v sidebar=%s",
			rec.DisplayName, rec.Email, rec.AvatarURL, rec.SidebarModules)
	}
	if rec.Language != "en" || !rec.RecordIPLog {
		t.Fatalf("默认值错误: language=%q record_ip_log=%v", rec.Language, rec.RecordIPLog)
	}
	createdAt := rec.UpdatedAt

	// (2) UpdateDisplayName 必须推进 updated_at（同一行版本，供自助设置乐观锁）。
	displayName := "Integration Name"
	rec, err = repo.UpdateDisplayName(ctx, userID, &displayName)
	if err != nil {
		t.Fatalf("UpdateDisplayName: %v", err)
	}
	if rec.DisplayName == nil || *rec.DisplayName != displayName {
		t.Fatalf("display_name = %v，期望 %q", rec.DisplayName, displayName)
	}
	if !rec.UpdatedAt.After(createdAt) {
		t.Fatalf("updated_at 未推进: %v → %v", createdAt, rec.UpdatedAt)
	}

	// (3) UpdateSettings 成功：语言 + 稀疏 sidebar（jsonb 往返）。
	language := "zh-CN"
	sidebar := json.RawMessage(`{"billing":false,"channels":true}`)
	rec, err = repo.UpdateSettings(ctx, userID, SelfPatch{
		Language:       &language,
		SidebarModules: &sidebar,
	}, rec.UpdatedAt)
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if rec.Language != language {
		t.Fatalf("language = %q，期望 %q", rec.Language, language)
	}
	var stored map[string]bool
	if err := json.Unmarshal(rec.SidebarModules, &stored); err != nil {
		t.Fatalf("sidebar_modules 不是 JSON 对象: %v（raw=%s）", err, rec.SidebarModules)
	}
	if len(stored) != 2 || stored["billing"] || !stored["channels"] {
		t.Fatalf("sidebar_modules = %v，期望 {billing:false, channels:true}", stored)
	}
	afterSuccess := rec.UpdatedAt

	// (4) 陈旧 token → ErrSelfConflict，且无任何写入。
	staleLanguage := "fr"
	if _, err := repo.UpdateSettings(ctx, userID, SelfPatch{Language: &staleLanguage},
		afterSuccess.Add(-time.Second)); !errors.Is(err, ErrSelfConflict) {
		t.Fatalf("陈旧 updated_at err = %v，期望 ErrSelfConflict", err)
	}
	fresh, err := repo.GetSelf(ctx, userID)
	if err != nil {
		t.Fatalf("GetSelf after conflict: %v", err)
	}
	if fresh.Language != language {
		t.Fatalf("冲突请求产生了写入: language = %q，期望保持 %q", fresh.Language, language)
	}

	// (5) sidebar JSON null → SQL NULL（不是 jsonb 'null'，也不是空对象）。
	nullSidebar := json.RawMessage(`null`)
	rec, err = repo.UpdateSettings(ctx, userID, SelfPatch{SidebarModules: &nullSidebar}, fresh.UpdatedAt)
	if err != nil {
		t.Fatalf("clear sidebar: %v", err)
	}
	if rec.SidebarModules != nil {
		t.Fatalf("sidebar_modules 清空后应为 nil，实际 %s", rec.SidebarModules)
	}
	var sidebarIsNull bool
	if err := db.QueryRow(`SELECT sidebar_modules IS NULL FROM users WHERE id = $1`, userID).
		Scan(&sidebarIsNull); err != nil {
		t.Fatalf("query sidebar IS NULL: %v", err)
	}
	if !sidebarIsNull {
		t.Fatal("sidebar_modules 必须是 SQL NULL（NULLIF 未生效）")
	}

	// (6) record_ip_log=false 的部分更新不影响其它列。
	recordIP := false
	rec, err = repo.UpdateSettings(ctx, userID, SelfPatch{RecordIPLog: &recordIP}, rec.UpdatedAt)
	if err != nil {
		t.Fatalf("record_ip_log: %v", err)
	}
	if rec.RecordIPLog || rec.Language != language {
		t.Fatalf("部分更新错误: record_ip_log=%v language=%q", rec.RecordIPLog, rec.Language)
	}

	// (7) MarkEmailVerified 幂等：两次返回同一时间戳。
	first, err := repo.MarkEmailVerified(ctx, userID)
	if err != nil {
		t.Fatalf("MarkEmailVerified #1: %v", err)
	}
	second, err := repo.MarkEmailVerified(ctx, userID)
	if err != nil {
		t.Fatalf("MarkEmailVerified #2: %v", err)
	}
	if !second.Equal(first) {
		t.Fatalf("email_verified_at 非幂等: %v → %v", first, second)
	}

	// (8) 用户不存在时四个方法都必须 ErrSelfNotFound（不是裸 sql.ErrNoRows）。
	missing := int64(1 << 62)
	if _, err := repo.GetSelf(ctx, missing); !errors.Is(err, ErrSelfNotFound) {
		t.Errorf("GetSelf(missing) err = %v，期望 ErrSelfNotFound", err)
	}
	if _, err := repo.UpdateDisplayName(ctx, missing, nil); !errors.Is(err, ErrSelfNotFound) {
		t.Errorf("UpdateDisplayName(missing) err = %v，期望 ErrSelfNotFound", err)
	}
	if _, err := repo.UpdateSettings(ctx, missing, SelfPatch{Language: &language},
		time.Now()); !errors.Is(err, ErrSelfNotFound) {
		t.Errorf("UpdateSettings(missing) err = %v，期望 ErrSelfNotFound", err)
	}
	if _, err := repo.MarkEmailVerified(ctx, missing); !errors.Is(err, ErrSelfNotFound) {
		t.Errorf("MarkEmailVerified(missing) err = %v，期望 ErrSelfNotFound", err)
	}
}
