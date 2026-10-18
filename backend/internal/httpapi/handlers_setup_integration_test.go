//go:build integration

package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"newtg/gateway/internal/auth"
)

// 本文件是 Task 11 的真实 PG 验收：锚与首个 admin 的同事务原子性、锚唯一
// 约束对并发双提交的裁决、以及重复提交 409 的存储层语义。运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationSetup -v
//
// 未设置环境变量时跳过（GC-8 只约束未打标签的测试；这里沿用既有集成测试
// 的「缺 DSN 即 skip」约定）。

func openSetupTestDB(t *testing.T) *sql.DB {
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

// ensureSetupDefaultGroup 补齐 users.group 的外键目标（与 Task 6 集成测试同口径）。
func ensureSetupDefaultGroup(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO user_groups (name) VALUES ('default')
		ON CONFLICT (name) DO NOTHING`); err != nil {
		t.Fatalf("seed default group: %v", err)
	}
}

func uniqueSetupSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// resetSetupState 清掉锚与站点键。测试顺序执行，不会与其他用例并行。
func resetSetupState(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`DELETE FROM system_settings
		WHERE "key" IN ($1, $2, $3)`, SetupCompletedKey, setupSiteTitleKey, setupServerAddressKey); err != nil {
		t.Fatalf("reset setup settings: %v", err)
	}
}

// cleanupSetupUsers 按精确用户名删除。刻意不用 LIKE：测试前缀里的 "_" 是
// LIKE 通配符，会误删同一测试库中并行用例创建的用户。
func cleanupSetupUsers(t *testing.T, db *sql.DB, usernames ...string) {
	t.Helper()
	for _, username := range usernames {
		if _, err := db.Exec(`DELETE FROM users WHERE username = $1`, username); err != nil {
			t.Logf("cleanup setup user %q: %v", username, err)
		}
	}
}

func setupIntegrationHash(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPassword(setupTestPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

// TestIntegrationSetupCompleteSetup 验证首次初始化在同一事务里写入 admin、
// 锚与站点配置；重复调用返回 ErrSetupAlreadyCompleted 且不产生第二个用户。
func TestIntegrationSetupCompleteSetup(t *testing.T) {
	db := openSetupTestDB(t)
	ensureSetupDefaultGroup(t, db)
	resetSetupState(t, db)

	suffix := uniqueSetupSuffix()
	prefix := "setupit_" + suffix
	t.Cleanup(func() {
		cleanupSetupUsers(t, db, prefix+"_a", prefix+"_b")
		resetSetupState(t, db)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store := NewSQLSetupStore(db)
	if initialized, err := store.IsInitialized(ctx); err != nil {
		t.Fatalf("IsInitialized: %v", err)
	} else if initialized {
		t.Fatal("清理后不应处于已初始化状态")
	}

	username := prefix + "_a"
	email := prefix + "@example.com"
	in := SetupInput{
		SiteTitle:     "IT Gateway",
		ServerAddress: "https://it.example.com",
		Username:      username,
		Email:         email,
		PasswordHash:  setupIntegrationHash(t),
	}
	result, err := store.CompleteSetup(ctx, in)
	if err != nil {
		t.Fatalf("CompleteSetup: %v", err)
	}
	if result.UserID <= 0 || result.Role != "admin" || result.Status != "enabled" {
		t.Fatalf("CompleteSetup result = %+v，期望 admin/enabled", result)
	}

	var (
		role, status, passwordHash string
		emailVerified              bool
	)
	if err := db.QueryRow(`SELECT role, status, email_verified_at IS NOT NULL, COALESCE(password_hash, '')
		FROM users WHERE id = $1`, result.UserID).
		Scan(&role, &status, &emailVerified, &passwordHash); err != nil {
		t.Fatalf("query admin row: %v", err)
	}
	if role != "admin" || status != "enabled" || !emailVerified {
		t.Fatalf("admin row = %s/%s/verified=%v，期望 admin/enabled/true", role, status, emailVerified)
	}
	if !auth.VerifyPassword(passwordHash, setupTestPassword) {
		t.Fatal("落库口令哈希无法用原文校验")
	}
	if passwordHash == setupTestPassword {
		t.Fatal("落库的是明文口令")
	}

	var (
		anchorValue string
		siteTitle   string
		serverAddr  string
	)
	if err := db.QueryRow(`SELECT value #>> '{}' FROM system_settings WHERE "key" = $1`,
		SetupCompletedKey).Scan(&anchorValue); err != nil {
		t.Fatalf("锚未写入或不是 JSON 字符串: %v", err)
	}
	if anchorValue == "" {
		t.Error("锚值不应为空")
	}
	if err := db.QueryRow(`SELECT value #>> '{}' FROM system_settings WHERE "key" = $1`,
		setupSiteTitleKey).Scan(&siteTitle); err != nil {
		t.Fatalf("site.title 未写入: %v", err)
	}
	if siteTitle != "IT Gateway" {
		t.Errorf("site.title = %q，期望 IT Gateway", siteTitle)
	}
	if err := db.QueryRow(`SELECT value #>> '{}' FROM system_settings WHERE "key" = $1`,
		setupServerAddressKey).Scan(&serverAddr); err != nil {
		t.Fatalf("site.server_address 未写入: %v", err)
	}
	if serverAddr != "https://it.example.com" {
		t.Errorf("site.server_address = %q", serverAddr)
	}

	if initialized, err := store.IsInitialized(ctx); err != nil || !initialized {
		t.Fatalf("IsInitialized = %v, %v，期望 true", initialized, err)
	}

	// 重复提交：锚已存在，必须拒绝且不新增用户。
	if _, err := store.CompleteSetup(ctx, SetupInput{
		SiteTitle:     "Another Gateway",
		ServerAddress: "https://another.example.com",
		Username:      prefix + "_b",
		Email:         prefix + "_b@example.com",
		PasswordHash:  in.PasswordHash,
	}); !errors.Is(err, ErrSetupAlreadyCompleted) {
		t.Fatalf("重复 CompleteSetup err = %v，期望 ErrSetupAlreadyCompleted", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE username = $1`, username).Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("用户数 = %d，期望 1", count)
	}
}

// TestIntegrationSetupAtomicRollback 证明「admin 插入失败」会连带回滚锚与站点
// 配置——不允许留下「有锚无 admin」的半初始化状态。
func TestIntegrationSetupAtomicRollback(t *testing.T) {
	db := openSetupTestDB(t)
	ensureSetupDefaultGroup(t, db)
	resetSetupState(t, db)

	suffix := uniqueSetupSuffix()
	prefix := "setupit_r_" + suffix
	username := prefix + "_taken"
	if _, err := db.Exec(`INSERT INTO users (username) VALUES ($1)`, username); err != nil {
		t.Fatalf("seed conflicting user: %v", err)
	}
	t.Cleanup(func() {
		cleanupSetupUsers(t, db, username)
		resetSetupState(t, db)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store := NewSQLSetupStore(db)

	_, err := store.CompleteSetup(ctx, SetupInput{
		SiteTitle:     "Rollback Gateway",
		ServerAddress: "https://rollback.example.com",
		Username:      username,
		Email:         prefix + "@example.com",
		PasswordHash:  setupIntegrationHash(t),
	})
	if !errors.Is(err, ErrDuplicateUser) {
		t.Fatalf("err = %v，期望 ErrDuplicateUser", err)
	}

	var settings int
	if err := db.QueryRow(`SELECT count(*) FROM system_settings WHERE "key" IN ($1, $2, $3)`,
		SetupCompletedKey, setupSiteTitleKey, setupServerAddressKey).Scan(&settings); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if settings != 0 {
		t.Fatalf("事务回滚后仍有 %d 行 setup 设置，期望 0（锚不得先提交）", settings)
	}
	if initialized, err := store.IsInitialized(ctx); err != nil || initialized {
		t.Fatalf("回滚后 IsInitialized = %v, %v，期望 false", initialized, err)
	}
}

// TestIntegrationSetupConcurrentOnlyOneWins 是 TC-F-01-20-02 的真实数据库验收：
// 两个事务都通过未初始化预检查后，锚的主键唯一约束只允许一个提交成功。
func TestIntegrationSetupConcurrentOnlyOneWins(t *testing.T) {
	db := openSetupTestDB(t)
	ensureSetupDefaultGroup(t, db)
	resetSetupState(t, db)

	suffix := uniqueSetupSuffix()
	prefix := "setupit_c_" + suffix
	t.Cleanup(func() {
		cleanupSetupUsers(t, db, prefix+"_a", prefix+"_b")
		resetSetupState(t, db)
	})

	store := NewSQLSetupStore(db)
	passwordHash := setupIntegrationHash(t)

	inputs := []SetupInput{
		{
			SiteTitle:     "Concurrent A",
			ServerAddress: "https://a.example.com",
			Username:      prefix + "_a",
			Email:         prefix + "_a@example.com",
			PasswordHash:  passwordHash,
		},
		{
			SiteTitle:     "Concurrent B",
			ServerAddress: "https://b.example.com",
			Username:      prefix + "_b",
			Email:         prefix + "_b@example.com",
			PasswordHash:  passwordHash,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := make(chan struct{})
	errs := make([]error, len(inputs))
	var wg sync.WaitGroup
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = store.CompleteSetup(ctx, inputs[i])
		}(i)
	}
	close(start)
	wg.Wait()

	var succeeded, conflicts int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrSetupAlreadyCompleted):
			conflicts++
		default:
			t.Fatalf("CompleteSetup[%d] 意外错误: %v", i, err)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Fatalf("并发结果 = %d 成功 / %d 409，期望 1/1", succeeded, conflicts)
	}

	var users, anchors int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE username IN ($1, $2)`,
		inputs[0].Username, inputs[1].Username).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Fatalf("并发后用户数 = %d，期望 1", users)
	}
	if err := db.QueryRow(`SELECT count(*) FROM system_settings WHERE "key" = $1`, SetupCompletedKey).
		Scan(&anchors); err != nil {
		t.Fatalf("count anchor: %v", err)
	}
	if anchors != 1 {
		t.Fatalf("锚行数 = %d，期望 1", anchors)
	}
}
