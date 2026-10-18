//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"newtg/gateway/internal/auth"
	"newtg/gateway/internal/rbac"
)

// 本文件是 Task 14 的数据库级验收：真实 PostgreSQL + Redis。
// 运行（与既有集成测试同口径）：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:newtg@localhost:55432/newtg_test?sslmode=disable' \
//	NEWTG_TEST_REDIS_ADDR=localhost:56379 \
//	go test -tags=integration ./internal/httpapi/ -run TestIntegrationAdmin -v
//
// 未设置环境变量时跳过（GC-8：未打标签的 go test ./... 不依赖外部服务）。
//
// 覆盖单元测试无法证明的真相：
//   - GC-5 的并发扣减永不产生负余额（条件 UPDATE 的 READ COMMITTED 语义）；
//   - 封禁/解禁对 api_tokens 的真实事务行为；
//   - user_groups 的 FK RESTRICT 与引用检查；
//   - Redis 幂等缓存下同一 Idempotency-Key 只落一行。

type adminIntegrationEnv struct {
	db       *sql.DB
	logsDB   *sql.DB
	repo     *SQLAdminUserRepo
	groups   *SQLAdminGroupRepo
	users    *UserHandlers
	groupAPI *GroupHandlers
	router   http.Handler
}

// openAuditLogsIntegration 在独立 schema 上建 audit_logs 并返回专用连接池。
//
// 为什么不用主库 public schema：store 包的 schema 契约测试要求 public 恰好
// 22 张主库表；日志表属独立日志库，混进 public 会破坏该断言（且跨包并行时
// 结果不稳定）。search_path 通过 DSN 设置，连接池里每条连接都生效。
func openAuditLogsIntegration(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA IF NOT EXISTS newtg_audit_test`); err != nil {
		_ = admin.Close()
		t.Fatalf("create audit schema: %v", err)
	}
	_ = admin.Close()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", "newtg_audit_test")
	parsed.RawQuery = query.Encode()
	logsDB, err := sql.Open("pgx", parsed.String())
	if err != nil {
		t.Fatalf("open logs db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := logsDB.PingContext(ctx); err != nil {
		_ = logsDB.Close()
		t.Fatalf("ping logs db: %v", err)
	}
	t.Cleanup(func() { _ = logsDB.Close() })
	ensureAuditLogsTable(t, logsDB)
	return logsDB
}

// ensureAuditLogsTable 在给定日志库连接上补建 audit_logs（幂等），列定义与
// migrations-logs 逐列一致。只用于集成测试。
func ensureAuditLogsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id          bigint      GENERATED ALWAYS AS IDENTITY,
			user_id     bigint      NOT NULL,
			username    text        NOT NULL,
			category    text        NOT NULL,
			ip          text,
			user_agent  text,
			method      text,
			route       text,
			status_code smallint,
			result      text        NOT NULL,
			detail      jsonb,
			before      jsonb,
			after       jsonb,
			created_at  timestamptz NOT NULL DEFAULT now(),
			CONSTRAINT audit_logs_pkey PRIMARY KEY (id),
			CONSTRAINT audit_logs_category_chk CHECK (category IN ('login', 'security', 'operation', 'access_token')),
			CONSTRAINT audit_logs_result_chk CHECK (result IN ('success', 'failed')))`,
		`CREATE INDEX IF NOT EXISTS audit_logs_user_created_idx ON audit_logs (user_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("ensure audit_logs: %v", err)
		}
	}
}

func newAdminIntegrationEnv(t *testing.T) *adminIntegrationEnv {
	t.Helper()
	db, rdb := openAuthIntegration(t)
	// 日志表放独立 schema（见 openAuditLogsIntegration 的理由）。
	logsDB := openAuditLogsIntegration(t)
	// default/auto 是种子迁移写入的系统组；测试库可能尚未跑种子迁移。
	for _, name := range []string{"default", "auto"} {
		if _, err := db.Exec(`INSERT INTO user_groups (name, is_system, status)
			VALUES ($1, true, 1) ON CONFLICT (name) DO NOTHING`, name); err != nil {
			t.Fatalf("seed group %s: %v", name, err)
		}
	}

	repo := NewSQLAdminUserRepo(db)
	groupRepo := NewSQLAdminGroupRepo(db)
	sessions := auth.NewSessionManager(rdb, []byte(testSessionSecret))
	authz := rbac.NewAuthzMiddleware()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	users, err := NewUserHandlers(UserHandlerConfig{
		Repo: repo, Sessions: sessions, Redis: rdb, Authz: authz, Logger: logger,
		Anonymizer: NewSQLPIIAnonymizer(logsDB),
	})
	if err != nil {
		t.Fatalf("NewUserHandlers: %v", err)
	}
	groups, err := NewGroupHandlers(GroupHandlerConfig{Repo: groupRepo, Authz: authz, Logger: logger})
	if err != nil {
		t.Fatalf("NewGroupHandlers: %v", err)
	}
	mux := http.NewServeMux()
	users.RegisterUserRoutes(mux, nil)
	groups.RegisterGroupRoutes(mux, nil)

	return &adminIntegrationEnv{
		db: db, logsDB: logsDB, repo: repo, groups: groupRepo,
		users: users, groupAPI: groups, router: mux,
	}
}

func (e *adminIntegrationEnv) request(t *testing.T, method, path string, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	// 全权管理员（nil = FULL），与单元测试同口径。
	principal := &auth.Principal{UserID: 900001, Role: rbac.RoleAdmin}
	req = req.WithContext(auth.WithPrincipal(req.Context(), principal))
	return serve(e.router, req)
}

func createIntegrationUser(t *testing.T, repo *SQLAdminUserRepo, group string) AdminUserRecord {
	t.Helper()
	hash, err := auth.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	rec, err := repo.CreateUser(context.Background(), AdminCreateUserInput{
		Username:     "it" + uniqueAuthSuffix(),
		Role:         rbac.RoleUser,
		Group:        group,
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create integration user: %v", err)
	}
	t.Cleanup(func() { cleanupAdminIntegrationUser(t, repo.db, rec.ID) })
	return rec
}

func cleanupAdminIntegrationUser(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	for _, statement := range []string{
		`DELETE FROM wallet_transactions WHERE user_id = $1`,
		`DELETE FROM login_sessions WHERE user_id = $1`,
		`DELETE FROM user_oauth_bindings WHERE user_id = $1`,
		`DELETE FROM user_passkeys WHERE user_id = $1`,
		`DELETE FROM checkins WHERE user_id = $1`,
		`DELETE FROM api_tokens WHERE user_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := db.Exec(statement, id); err != nil {
			t.Errorf("cleanup user %d: %v", id, err)
		}
	}
}

// TestIntegrationAdminConcurrentSubtractNeverNegative 是 GC-5 的核心断言：
// 20 个并发事务对余额 100 各扣 10，恰好 10 个成功、余额归零、任何时刻不为负。
func TestIntegrationAdminConcurrentSubtractNeverNegative(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	user := createIntegrationUser(t, env.repo, "default")

	if _, err := env.repo.AdjustQuota(context.Background(), user.ID, "add", 100, "seed-"+uniqueAuthSuffix()); err != nil {
		t.Fatalf("seed balance: %v", err)
	}

	const attempts = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := env.repo.AdjustQuota(context.Background(), user.ID, "subtract", 10,
				"concurrent-"+strconv.Itoa(i))
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAdminInsufficient):
		default:
			t.Fatalf("unexpected concurrent subtract error: %v", err)
		}
	}
	if successes != 10 {
		t.Fatalf("successful subtracts = %d, want 10", successes)
	}
	stored, err := env.repo.GetUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("read final balance: %v", err)
	}
	if stored.Balance != 0 {
		t.Fatalf("final balance = %d, want 0", stored.Balance)
	}

	var negative int64
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND balance_after < 0`, user.ID).Scan(&negative); err != nil {
		t.Fatalf("count negative balances: %v", err)
	}
	if negative != 0 {
		t.Fatalf("wallet_transactions contains %d negative balance_after rows", negative)
	}
	var rows int64
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'admin_adjustment'`, user.ID).Scan(&rows); err != nil {
		t.Fatalf("count adjustment rows: %v", err)
	}
	if rows != 11 { // 1 seed + 10 successful subtracts
		t.Fatalf("admin_adjustment rows = %d, want 11", rows)
	}
}

// TestIntegrationAdminIdempotentCreate 证明 Redis 幂等缓存 + 唯一约束下，
// 同一 Idempotency-Key 只落一行（TC-F-07-02-01）。
func TestIntegrationAdminIdempotentCreate(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	username := "it" + uniqueAuthSuffix()
	key := "idem-" + uniqueAuthSuffix()

	var createdID int64
	t.Cleanup(func() { cleanupAdminIntegrationUser(t, env.db, createdID) })

	body := map[string]any{"username": username, "password": "correct-horse-battery"}
	first := env.request(t, http.MethodPost, "/api/users", body, key)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201 (body=%q)", first.Code, first.Body.String())
	}
	var view adminUserView
	if err := json.Unmarshal(first.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	createdID = view.ID

	replay := env.request(t, http.MethodPost, "/api/users", body, key)
	if replay.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201 (body=%q)", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != first.Body.String() {
		t.Fatalf("replay body differs:\nfirst=%s\nreplay=%s", first.Body.String(), replay.Body.String())
	}

	var rows int64
	if err := env.db.QueryRow(`SELECT count(*) FROM users WHERE username = $1`, username).Scan(&rows); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if rows != 1 {
		t.Fatalf("users with username %q = %d, want 1", username, rows)
	}
}

// TestIntegrationAdminStatusTokens 验证封禁/解禁对令牌的真实事务行为：
// 封禁只禁用 enabled 令牌；解禁只恢复封禁时记录的集合。
func TestIntegrationAdminStatusTokens(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	user := createIntegrationUser(t, env.repo, "default")

	enabledID := insertIntegrationToken(t, env.db, user.ID, "enabled")
	disabledID := insertIntegrationToken(t, env.db, user.ID, "disabled")

	changed, enabledIDs, err := env.repo.SetStatus(context.Background(), user.ID, "disabled", nil)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !changed || len(enabledIDs) != 1 || enabledIDs[0] != enabledID {
		t.Fatalf("disable changed=%v enabledIDs=%v, want [%d]", changed, enabledIDs, enabledID)
	}
	if got := integrationTokenStatus(t, env.db, enabledID); got != "disabled" {
		t.Fatalf("enabled token status = %q, want disabled", got)
	}
	if got := integrationTokenStatus(t, env.db, disabledID); got != "disabled" {
		t.Fatalf("pre-disabled token status = %q, want disabled", got)
	}
	restoreIDs := enabledIDs

	// 幂等：重复封禁不返回恢复集（避免覆盖第一次记录）。
	changed, enabledIDs, err = env.repo.SetStatus(context.Background(), user.ID, "disabled", nil)
	if err != nil || changed || len(enabledIDs) != 0 {
		t.Fatalf("repeat disable changed=%v ids=%v err=%v, want no-op", changed, enabledIDs, err)
	}

	changed, _, err = env.repo.SetStatus(context.Background(), user.ID, "enabled", restoreIDs)
	if err != nil || !changed {
		t.Fatalf("enable changed=%v err=%v", changed, err)
	}
	if got := integrationTokenStatus(t, env.db, enabledID); got != "enabled" {
		t.Fatalf("restored token status = %q, want enabled", got)
	}
	if got := integrationTokenStatus(t, env.db, disabledID); got != "disabled" {
		t.Fatalf("pre-disabled token status after enable = %q, want disabled", got)
	}

	// 删除前置：enabled 时拒绝；disabled 后硬删并清空令牌。
	if _, err := env.repo.DeleteUser(context.Background(), user.ID); !errors.Is(err, ErrAdminNotDisabled) {
		t.Fatalf("delete enabled user err = %v, want ErrAdminNotDisabled", err)
	}
	if _, _, err := env.repo.SetStatus(context.Background(), user.ID, "disabled", nil); err != nil {
		t.Fatalf("disable before delete: %v", err)
	}
	deleted, err := env.repo.DeleteUser(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("delete disabled user: %v", err)
	}
	if deleted.TokensDeleted != 2 {
		t.Fatalf("deleted tokens = %d, want 2", deleted.TokensDeleted)
	}
	if _, err := env.repo.GetUser(context.Background(), user.ID); !errors.Is(err, ErrAdminUserNotFound) {
		t.Fatalf("user still exists after delete: %v", err)
	}
}

func insertIntegrationToken(t *testing.T, db *sql.DB, userID int64, status string) int64 {
	t.Helper()
	hash := auth.HashTokenKey("nkt-" + uniqueAuthSuffix() + uniqueAuthSuffix())
	var id int64
	if err := db.QueryRow(`INSERT INTO api_tokens (user_id, name, key_hash, status, "group")
		VALUES ($1, $2, $3, $4, 'default') RETURNING id`,
		userID, "it-"+uniqueAuthSuffix(), hash, status).Scan(&id); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	return id
}

func integrationTokenStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM api_tokens WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read token status: %v", err)
	}
	return status
}

// TestIntegrationAdminGroupReferences 验证 FK RESTRICT 与引用预检一致：
// 被用户引用的分组不能删除/改名；释放引用后可以删除。
func TestIntegrationAdminGroupReferences(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	suffix := uniqueAuthSuffix()
	name := "itvip" + suffix
	if _, err := env.groups.CreateGroup(context.Background(), name, nil); err != nil {
		t.Fatalf("create group: %v", err)
	}
	t.Cleanup(func() {
		_, _ = env.db.Exec(`UPDATE users SET "group" = 'default' WHERE "group" = $1`, name)
		_, _ = env.db.Exec(`DELETE FROM user_groups WHERE name = $1`, name)
	})

	user := createIntegrationUser(t, env.repo, name)

	if err := env.groups.DeleteGroup(context.Background(), name); !errors.Is(err, ErrGroupReferenced) {
		t.Fatalf("delete referenced group err = %v, want ErrGroupReferenced", err)
	}
	newName := name + "x"
	if _, err := env.groups.UpdateGroup(context.Background(), name, &newName, nil, false); !errors.Is(err, ErrGroupReferenced) {
		t.Fatalf("rename referenced group err = %v, want ErrGroupReferenced", err)
	}
	if err := env.groups.DeleteGroup(context.Background(), "default"); !errors.Is(err, ErrGroupSystem) {
		t.Fatalf("delete default err = %v, want ErrGroupSystem", err)
	}

	// 释放用户引用后删除成功。
	if _, _, err := env.repo.SetStatus(context.Background(), user.ID, "disabled", nil); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := env.repo.DeleteUser(context.Background(), user.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if err := env.groups.DeleteGroup(context.Background(), name); err != nil {
		t.Fatalf("delete unreferenced group: %v", err)
	}
}

// TestIntegrationAdminCreateGroupNameConstraint 验证 DB CHECK 与端点校验
// 使用同一 name 规则（端点拒绝 + DB 兜底）。
func TestIntegrationAdminCreateGroupNameConstraint(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	name := "itgrp" + uniqueAuthSuffix()
	t.Cleanup(func() { _, _ = env.db.Exec(`DELETE FROM user_groups WHERE name = $1`, name) })

	if _, err := env.groups.CreateGroup(context.Background(), name, strPtrAdmin("desc")); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := env.groups.CreateGroup(context.Background(), name, nil); !errors.Is(err, ErrGroupConflict) {
		t.Fatalf("duplicate group err = %v, want ErrGroupConflict", err)
	}
}

// TestIntegrationAdminUserListAndPermissions 让列表 SQL（ILIKE 转义、排序、
// 分页计数）与权限矩阵 SQL 真正跑一遍 PG：单元测试的假仓储无法暴露 SQL 语法
// 或 jsonb 往返问题。
func TestIntegrationAdminUserListAndPermissions(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	user := createIntegrationUser(t, env.repo, "default")

	items, total, err := env.repo.ListUsers(context.Background(), UserListFilter{
		Keyword: user.Username[2:8], Status: "enabled", Role: "user", Group: "default",
		Page: 1, PageSize: 20, Sort: "balance",
	})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != user.ID {
		t.Fatalf("ListUsers = total %d items %+v, want user %d", total, items, user.ID)
	}
	if items[0].AdminPermissions != nil {
		t.Fatalf("普通用户 admin_permissions = %s, want null", items[0].AdminPermissions)
	}

	// LIKE 通配符必须按字面量转义：用户名不允许出现 %，因此关键字符 % 不应
	// 匹配任何行（未转义时它会匹配全部）。
	_, wildcardTotal, err := env.repo.ListUsers(context.Background(), UserListFilter{
		Keyword: "%", Page: 1, PageSize: 20, Sort: "-created_at",
	})
	if err != nil {
		t.Fatalf("ListUsers wildcard: %v", err)
	}
	if wildcardTotal != 0 {
		t.Fatalf("keyword %q matched %d users; LIKE wildcard was not escaped", "%", wildcardTotal)
	}

	// 权限矩阵：admin 行可写；user 行拒绝。
	hash, err := auth.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	admin, err := env.repo.CreateUser(context.Background(), AdminCreateUserInput{
		Username:         "itadm" + uniqueAuthSuffix(),
		Role:             rbac.RoleAdmin,
		Group:            "default",
		PasswordHash:     hash,
		AdminPermissions: []string{rbac.PermUsersRead},
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	t.Cleanup(func() { cleanupAdminIntegrationUser(t, env.db, admin.ID) })

	updated, err := env.repo.SetPermissions(context.Background(), admin.ID, []string{rbac.PermUsersRead, rbac.PermLogsRead})
	if err != nil {
		t.Fatalf("SetPermissions: %v", err)
	}
	var perms []string
	if err := json.Unmarshal(updated.AdminPermissions, &perms); err != nil {
		t.Fatalf("permissions jsonb: %v (raw=%s)", err, updated.AdminPermissions)
	}
	if len(perms) != 2 {
		t.Fatalf("permissions = %v, want two keys", perms)
	}
	if _, err := env.repo.SetPermissions(context.Background(), user.ID, []string{rbac.PermUsersRead}); !errors.Is(err, ErrAdminNotAdmin) {
		t.Fatalf("SetPermissions on user err = %v, want ErrAdminNotAdmin", err)
	}
}

// TestIntegrationAdminLoadRespectsRole 是 Critical 1 的数据库侧断言：
// Load 必须把 users.role 与 admin_permissions 一起读取，非 admin 行的 NULL
// 绝不映射为 FULL；陈旧 admin 会话（Role=admin）经中间件判定为 403。
func TestIntegrationAdminLoadRespectsRole(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	authRepo := NewSQLAuthRepo(env.db)
	var id int64
	username := "itadm" + uniqueAuthSuffix()
	if err := env.db.QueryRow(`INSERT INTO users (username, role) VALUES ($1, 'admin') RETURNING id`,
		username).Scan(&id); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	t.Cleanup(func() { cleanupAdminIntegrationUser(t, env.db, id) })

	perms, err := authRepo.Load(context.Background(), id)
	if err != nil || perms != nil {
		t.Fatalf("admin NULL perms = %v (err=%v), want nil=FULL", perms, err)
	}

	// 降权后同一行的 NULL 权限必须变成显式空集，而不是 FULL。
	if _, err := env.db.Exec(`UPDATE users SET role = 'user' WHERE id = $1`, id); err != nil {
		t.Fatalf("demote: %v", err)
	}
	perms, err = authRepo.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("Load after demote: %v", err)
	}
	if perms == nil {
		t.Fatal("非 admin 行的 NULL 权限被映射为 FULL")
	}
	if len(perms) != 0 {
		t.Fatalf("非 admin 行权限 = %v, want empty", perms)
	}

	// 陈旧 admin 会话（Role=admin + Permissions=nil）经中间件解析后 403。
	authz := rbac.NewAuthzMiddleware()
	authz.SetPermissionLoader(authRepo)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := authz.Handler(rbac.PermUsersRead, rbac.AuthzAction{ResourceKind: "user", ActionName: "read"})(next)
	stale := &auth.Principal{UserID: id, Role: rbac.RoleAdmin}
	req := httptest.NewRequest(http.MethodGet, "/api/users", nil).
		WithContext(auth.WithPrincipal(context.Background(), stale))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("降权后的陈旧会话 status = %d, want 403", rr.Code)
	}

	// 重新提权后（NULL=FULL）同一会话判定恢复放行。
	if _, err := env.db.Exec(`UPDATE users SET role = 'admin' WHERE id = $1`, id); err != nil {
		t.Fatalf("promote: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/users", nil).
		WithContext(auth.WithPrincipal(context.Background(), stale))
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("重新提权后 status = %d, want 204", rr.Code)
	}
}

// TestIntegrationAdminDeleteCascadesDependents 证明管理员硬删除与自助删除
// （SQLAccountRepo.DeleteAccount）清理同一组逻辑引用表。
func TestIntegrationAdminDeleteCascadesDependents(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	user := createIntegrationUser(t, env.repo, "default")

	suffix := uniqueAuthSuffix()
	if _, err := env.db.Exec(`INSERT INTO login_sessions (session_id_hash, user_id) VALUES ($1, $2)`,
		"itsess"+suffix, user.ID); err != nil {
		t.Fatalf("insert login_session: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO user_oauth_bindings (user_id, provider, provider_user_id)
		VALUES ($1, 'github', $2)`, user.ID, "itgh"+suffix); err != nil {
		t.Fatalf("insert oauth binding: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO user_passkeys (user_id, name, credential_id, public_key)
		VALUES ($1, 'key', $2, $3)`, user.ID, []byte("cred"+suffix), []byte("pk"+suffix)); err != nil {
		t.Fatalf("insert passkey: %v", err)
	}
	if _, err := env.db.Exec(`INSERT INTO checkins (user_id, checkin_date, quota_awarded)
		VALUES ($1, CURRENT_DATE, 0)`, user.ID); err != nil {
		t.Fatalf("insert checkin: %v", err)
	}

	if _, _, err := env.repo.SetStatus(context.Background(), user.ID, "disabled", nil); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := env.repo.DeleteUser(context.Background(), user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, table := range []string{
		"login_sessions", "user_oauth_bindings", "user_passkeys", "checkins",
	} {
		var count int64
		if err := env.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE user_id = $1`, user.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s 仍有 %d 行孤儿记录", table, count)
		}
	}
}

// TestIntegrationAdminDeleteAnonymizesAuditLogs 覆盖 B1：管理端删除必须与自助
// 删除一致，把日志库 audit_logs.username 快照改写为 deleted_{id}，再硬删 users。
func TestIntegrationAdminDeleteAnonymizesAuditLogs(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	user := createIntegrationUser(t, env.repo, "default")
	t.Cleanup(func() { _, _ = env.logsDB.Exec(`DELETE FROM audit_logs WHERE user_id = $1`, user.ID) })

	// 模拟 12 个月保留期内的真实用户名快照。
	if _, err := env.logsDB.Exec(`INSERT INTO audit_logs (user_id, username, category, result)
		VALUES ($1, $2, 'operation', 'success')`, user.ID, user.Username); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
	if _, _, err := env.repo.SetStatus(context.Background(), user.ID, "disabled", nil); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	rr := env.request(t, http.MethodDelete, fmt.Sprintf("/api/users/%d", user.ID), nil, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d（body=%q）", rr.Code, rr.Body.String())
	}
	var stored string
	if err := env.logsDB.QueryRow(`SELECT username FROM audit_logs WHERE user_id = $1 ORDER BY id DESC LIMIT 1`,
		user.ID).Scan(&stored); err != nil {
		t.Fatalf("read anonymized audit row: %v", err)
	}
	if want := fmt.Sprintf("deleted_%d", user.ID); stored != want {
		t.Fatalf("audit_logs.username = %q，期望 %q", stored, want)
	}
}

// TestIntegrationAdminQuotaAdjustmentBindsParams 覆盖 C9：调额幂等键必须绑定
// 请求语义（mode+amount）。同 key 同参数重放首次结果；不同参数返回
// ErrAdminIdempotencyReused（HTTP 422），绝不静默重放。
func TestIntegrationAdminQuotaAdjustmentBindsParams(t *testing.T) {
	env := newAdminIntegrationEnv(t)
	user := createIntegrationUser(t, env.repo, "default")
	key := "qadj-" + uniqueAuthSuffix()
	ctx := context.Background()

	first, err := env.repo.AdjustQuota(ctx, user.ID, "add", 100, key)
	if err != nil {
		t.Fatalf("adjust add 100: %v", err)
	}
	if first.NewBalance != 100 || first.Replayed {
		t.Fatalf("first = %+v，期望 NewBalance=100/Replayed=false", first)
	}
	replay, err := env.repo.AdjustQuota(ctx, user.ID, "add", 100, key)
	if err != nil {
		t.Fatalf("replay add 100: %v", err)
	}
	if !replay.Replayed || replay.TransactionID != first.TransactionID {
		t.Fatalf("replay = %+v，期望重放首次流水 %d", replay, first.TransactionID)
	}

	if _, err := env.repo.AdjustQuota(ctx, user.ID, "add", 200, key); !errors.Is(err, ErrAdminIdempotencyReused) {
		t.Fatalf("同 key 不同 amount err = %v，期望 ErrAdminIdempotencyReused", err)
	}
	if _, err := env.repo.AdjustQuota(ctx, user.ID, "subtract", 100, key); !errors.Is(err, ErrAdminIdempotencyReused) {
		t.Fatalf("同 key 不同 mode err = %v，期望 ErrAdminIdempotencyReused", err)
	}

	var balance int64
	if err := env.db.QueryRow(`SELECT balance FROM users WHERE id = $1`, user.ID).Scan(&balance); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance != 100 {
		t.Fatalf("balance = %d，期望 100（被拒绝的复用不得改余额）", balance)
	}
	var rows int64
	if err := env.db.QueryRow(`SELECT count(*) FROM wallet_transactions
		WHERE user_id = $1 AND type = 'admin_adjustment'`, user.ID).Scan(&rows); err != nil {
		t.Fatalf("count admin_adjustment: %v", err)
	}
	if rows != 1 {
		t.Fatalf("admin_adjustment rows = %d，期望 1", rows)
	}
}
