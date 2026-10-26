//go:build integration

package store

// 批量建用户仓储的运行时集成测试（plan 2026-10-26 Task 9）。
//
// 这些不变量只能连真 PostgreSQL 才能验证：
//   - 全有或全无：第二行撞库内用户名时第一行也必须回滚；
//   - afterInsert（临时口令标记）在提交前执行，失败即整批回滚；
//   - AssignSequential 在 advisory lock 下取 MAX 并分配 NewTG_user%04d；
//   - 并发生成会串行分配序号，不产生重复用户名。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://...' go test -tags=integration ./internal/store/ -run TestBulkCreateUsers -v
//
// DSN 未设置时跳过。测试库与其它 store 集成测试共享，因此每次用纳秒后缀
// 生成唯一用户名，并在 t.Cleanup 中按用户名清理。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

func bulkTestSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// cleanupBulkTestUsers 在用例结束时删除本次创建的用户（按用户名），
// 顺序在 openTestDB 的 db.Close 之前（t.Cleanup 后进先出）。
func cleanupBulkTestUsers(t *testing.T, db *sql.DB, usernames *[]string) {
	t.Helper()
	t.Cleanup(func() {
		if len(*usernames) == 0 {
			return
		}
		if _, err := db.Exec(`DELETE FROM users WHERE username = ANY($1::text[])`, *usernames); err != nil {
			t.Errorf("cleanup bulk users: %v", err)
		}
	})
}

func insertBulkTestUser(t *testing.T, db *sql.DB, username string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO users (username, "group") VALUES ($1, 'default')`, username); err != nil {
		t.Fatalf("insert existing user %s: %v", username, err)
	}
}

func bulkTestRows(count int) []BulkUserRow {
	rows := make([]BulkUserRow, count)
	for i := range rows {
		rows[i] = BulkUserRow{
			PasswordHash: "argon2id$v=19$m=19456,t=2,p=1$bulk$test",
			CustomerType: "general",
			Group:        "default",
		}
	}
	return rows
}

func countBulkTestUsers(t *testing.T, db *sql.DB, username string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM users WHERE username = $1`, username).Scan(&n); err != nil {
		t.Fatalf("count user %s: %v", username, err)
	}
	return n
}

// TestBulkCreateUsersAllOrNothing：第二行撞库内唯一键时整批回滚，第一行也不落库。
func TestBulkCreateUsersAllOrNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := bulkTestSuffix()
	existing := "bulkex" + suffix
	fresh := "bulknew" + suffix

	var created []string
	insertBulkTestUser(t, db, existing)
	created = append(created, existing, fresh)
	cleanupBulkTestUsers(t, db, &created)

	repo := NewBulkUserRepo(db)
	rows := bulkTestRows(2)
	rows[0].Username = fresh
	rows[1].Username = existing

	if _, err := repo.BulkCreateUsers(ctx, rows, BulkCreateOptions{}, nil); !errors.Is(err, ErrBulkDuplicateUsername) {
		t.Fatalf("err = %v，期望 ErrBulkDuplicateUsername", err)
	}
	if got := countBulkTestUsers(t, db, fresh); got != 0 {
		t.Fatalf("回滚后 %s 仍有 %d 行，期望 0（全有或全无）", fresh, got)
	}
}

// TestBulkCreateUsersWritesMarkersBeforeCommit：afterInsert 返回错误时事务回滚，
// 一行都不落库——这是「DB 有 must_change_password 而 Redis 无标记 = 永远 401」
// 的反面保证。
func TestBulkCreateUsersWritesMarkersBeforeCommit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := bulkTestSuffix()
	username := "bulkmark" + suffix

	var created []string
	created = append(created, username)
	cleanupBulkTestUsers(t, db, &created)

	repo := NewBulkUserRepo(db)
	rows := bulkTestRows(1)
	rows[0].Username = username

	var marks []int64
	_, err := repo.BulkCreateUsers(ctx, rows, BulkCreateOptions{MustChangePassword: true},
		func(_ *sql.Tx, ids []int64) error {
			marks = append([]int64(nil), ids...)
			return errors.New("marker write failed")
		})
	if err == nil {
		t.Fatal("afterInsert 失败时 BulkCreateUsers 必须返回错误")
	}
	if len(marks) != 1 {
		t.Fatalf("afterInsert 收到 ids = %v，期望 1 个", marks)
	}
	if got := countBulkTestUsers(t, db, username); got != 0 {
		t.Fatalf("标记写入失败后 %s 仍有 %d 行，期望 0", username, got)
	}
}

// TestBulkCreateUsersPersistsMustChangePassword：成功路径必须把
// opts.MustChangePassword 真正写进 users.must_change_password。只观察
// afterInsert 收到的 ids 无法发现 INSERT 漏写该列或把它硬编码成 false——
// 那会让批量创建的账号要么免改密，要么与 Redis 标记脱节。
func TestBulkCreateUsersPersistsMustChangePassword(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	suffix := bulkTestSuffix()
	username := "bulkmcp" + suffix

	var created []string
	created = append(created, username)
	cleanupBulkTestUsers(t, db, &created)

	repo := NewBulkUserRepo(db)
	rows := bulkTestRows(1)
	rows[0].Username = username

	if _, err := repo.BulkCreateUsers(ctx, rows, BulkCreateOptions{MustChangePassword: true}, nil); err != nil {
		t.Fatalf("BulkCreateUsers: %v", err)
	}

	var mustChange bool
	if err := db.QueryRow(
		`SELECT must_change_password FROM users WHERE username = $1`, username).Scan(&mustChange); err != nil {
		t.Fatalf("read must_change_password: %v", err)
	}
	if !mustChange {
		t.Fatal("users.must_change_password = false，期望 true")
	}
}

// TestBulkCreateUsersSequentialNames：预先占用当前最大序号 +1，新生用户名必须
// 从其后再顺延，证明分配基于库内 MAX 而不是从 1 重新开始。
func TestBulkCreateUsersSequentialNames(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var current int64
	if err := db.QueryRow(`
		SELECT COALESCE(MAX(substring(username FROM 11)::bigint), 0)
		FROM users
		WHERE username ~ '^NewTG_user[0-9]{1,9}$'`).Scan(&current); err != nil {
		t.Fatalf("read current sequence: %v", err)
	}
	if current+3 > bulkUserSequenceMax {
		t.Skipf("序号已接近上限（current=%d），跳过顺序分配测试", current)
	}
	sentinel := fmt.Sprintf("NewTG_user%04d", current+1)

	var created []string
	insertBulkTestUser(t, db, sentinel)
	created = append(created, sentinel)
	cleanupBulkTestUsers(t, db, &created)

	repo := NewBulkUserRepo(db)
	got, err := repo.BulkCreateUsers(ctx, bulkTestRows(2), BulkCreateOptions{AssignSequential: true}, nil)
	if err != nil {
		t.Fatalf("BulkCreateUsers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("返回行数 = %d，期望 2", len(got))
	}
	for i, want := range []int64{current + 2, current + 3} {
		wantName := fmt.Sprintf("NewTG_user%04d", want)
		if got[i].Username != wantName {
			t.Fatalf("第 %d 行 username = %q，期望 %q", i, got[i].Username, wantName)
		}
		created = append(created, got[i].Username)
	}
}

// TestBulkCreateUsersConcurrentGenerate：两个并发的 AssignSequential 请求必须
// 各自拿到 5 个互不重复的连续用户名。
func TestBulkCreateUsersConcurrentGenerate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	repo := NewBulkUserRepo(db)

	const perRequest = 5
	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		created []string
		errs    [2]error
	)
	cleanupBulkTestUsers(t, db, &created)

	for g := range 2 {
		wait.Add(1)
		go func(g int) {
			defer wait.Done()
			rows, err := repo.BulkCreateUsers(ctx, bulkTestRows(perRequest),
				BulkCreateOptions{AssignSequential: true}, nil)
			if err != nil {
				errs[g] = err
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows {
				created = append(created, row.Username)
			}
		}(g)
	}
	wait.Wait()

	for g, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", g, err)
		}
	}
	if len(created) != 2*perRequest {
		t.Fatalf("创建用户数 = %d，期望 %d", len(created), 2*perRequest)
	}
	seen := make(map[string]bool, len(created))
	for _, name := range created {
		if seen[name] {
			t.Fatalf("并发分配出现重复用户名：%s（全部：%v）", name, created)
		}
		seen[name] = true
	}
}
