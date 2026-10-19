//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
	first, granted, err := repo.MarkEmailVerified(ctx, userID, 0)
	if err != nil {
		t.Fatalf("MarkEmailVerified #1: %v", err)
	}
	if granted {
		t.Fatal("signupBonus=0 时不得报告已发放赠送")
	}
	second, _, err := repo.MarkEmailVerified(ctx, userID, 0)
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
	if _, _, err := repo.MarkEmailVerified(ctx, missing, 0); !errors.Is(err, ErrSelfNotFound) {
		t.Errorf("MarkEmailVerified(missing) err = %v，期望 ErrSelfNotFound", err)
	}
}

// TestIntegrationMarkEmailVerifiedGrantsOnce 覆盖 B6 的真实事务语义：并发
// 首次验证只有一个事务插入 signup_grant 流水并加余额，其余是 no-op。
func TestIntegrationMarkEmailVerifiedGrantsOnce(t *testing.T) {
	db, _ := openAuthIntegration(t)
	ensureDefaultGroup(t, db)
	repo := NewSQLSelfRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	suffix := uniqueAuthSuffix()
	username := "grant_" + suffix
	var userID int64
	if err := db.QueryRow(`INSERT INTO users (username, email) VALUES ($1, $2) RETURNING id`,
		username, username+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, userID) })

	const bonus = int64(500)
	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	grantedCount := make(chan bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, granted, err := repo.MarkEmailVerified(ctx, userID, bonus)
			errs <- err
			grantedCount <- granted
		}()
	}
	wg.Wait()
	close(errs)
	close(grantedCount)
	for err := range errs {
		if err != nil {
			t.Fatalf("MarkEmailVerified: %v", err)
		}
	}
	grantedTotal := 0
	for granted := range grantedCount {
		if granted {
			grantedTotal++
		}
	}
	if grantedTotal != 1 {
		t.Fatalf("granted 次数 = %d，期望 1（并发唯一索引裁决）", grantedTotal)
	}

	var balance int64
	if err := db.QueryRow(`SELECT balance FROM users WHERE id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != bonus {
		t.Fatalf("balance = %d，期望 %d（并发只加一次）", balance, bonus)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND reference_type = 'signup_grant'`, userID).Scan(&rows); err != nil {
		t.Fatalf("count signup_grant rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("signup_grant 流水 = %d 行，期望 1（唯一索引幂等）", rows)
	}
	var amount, balanceAfter int64
	if err := db.QueryRow(`SELECT amount, balance_after FROM wallet_transactions
		WHERE user_id = $1 AND reference_type = 'signup_grant'`, userID).Scan(&amount, &balanceAfter); err != nil {
		t.Fatalf("read signup_grant row: %v", err)
	}
	if amount != bonus || balanceAfter != bonus {
		t.Fatalf("流水 amount/balance_after = %d/%d，期望 %d/%d", amount, balanceAfter, bonus, bonus)
	}
}

// TestIntegrationSelfDefaultModelSQL 验证 users.default_model 的 SQL 三值语义：
// 设置/保持/清除（空串 → NULL）与「只改该键」。
func TestIntegrationSelfDefaultModelSQL(t *testing.T) {
	db, _ := openAuthIntegration(t)
	ensureDefaultGroup(t, db)
	repo := NewSQLSelfRepo(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	username := "defmodel_" + uniqueAuthSuffix()
	var userID int64
	if err := db.QueryRow(`INSERT INTO users (username) VALUES ($1) RETURNING id`, username).
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { cleanupAuthUser(t, db, userID) })

	rec, err := repo.GetSelf(ctx, userID)
	if err != nil {
		t.Fatalf("GetSelf: %v", err)
	}
	if rec.DefaultModel != nil {
		t.Fatalf("新用户 default_model = %v, want NULL", rec.DefaultModel)
	}

	model := "gpt-5"
	rec, err = repo.UpdateSettings(ctx, userID, SelfPatch{DefaultModel: &model}, rec.UpdatedAt)
	if err != nil {
		t.Fatalf("set default_model: %v", err)
	}
	if rec.DefaultModel == nil || *rec.DefaultModel != model {
		t.Fatalf("default_model = %v, want %s", rec.DefaultModel, model)
	}
	if rec.Language != "en" {
		t.Fatalf("language = %q，设置默认模型不得改写其它键", rec.Language)
	}

	// 只改 language，default_model 必须保持（CASE WHEN 的 ELSE 分支）。
	language := "zh-CN"
	rec, err = repo.UpdateSettings(ctx, userID, SelfPatch{Language: &language}, rec.UpdatedAt)
	if err != nil {
		t.Fatalf("set language: %v", err)
	}
	if rec.DefaultModel == nil || *rec.DefaultModel != model {
		t.Fatalf("default_model = %v，未提交时必须保持", rec.DefaultModel)
	}

	// 空串 = 清除。
	empty := ""
	rec, err = repo.UpdateSettings(ctx, userID, SelfPatch{DefaultModel: &empty}, rec.UpdatedAt)
	if err != nil {
		t.Fatalf("clear default_model: %v", err)
	}
	if rec.DefaultModel != nil {
		t.Fatalf("清除后 default_model = %v, want NULL", rec.DefaultModel)
	}

	// 迁移 CHECK：空串不得入库（应用层归一之外的最后防线）。
	if _, err := db.Exec(`UPDATE users SET default_model = '' WHERE id = $1`, userID); err == nil {
		t.Fatal("空串 default_model 被数据库接受，users_default_model_len_chk 未生效")
	}
}
