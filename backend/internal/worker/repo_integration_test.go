//go:build integration

package worker

// F-3 的滞留 pending 兜底验收（真实 PostgreSQL）。
//
// 运行：
//
//	NEWTG_TEST_DB_DSN='postgres://newtg:...@127.0.0.1:55432/newtg_it_main?sslmode=disable' \
//	go test -tags=integration -p 1 ./internal/worker/ -run TestExpireRunningRunsReapsStalePending -v

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestExpireRunningRunsReapsStalePending(t *testing.T) {
	dsn := os.Getenv("NEWTG_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("NEWTG_TEST_DB_DSN 未设置，跳过集成测试")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// 专用 task_type，避免与其它用例（或其他并发跑的套件）互相影响。
	const taskType = "f3_pending_expiry_probe"
	if _, err := db.ExecContext(ctx, `DELETE FROM worker_task_runs WHERE task_type = $1`, taskType); err != nil {
		t.Fatalf("clear rows: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM worker_task_runs WHERE task_type = $1`, taskType)
	})

	insert := func(ageInterval string) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRowContext(ctx, `
			INSERT INTO worker_task_runs (task_type, status, trigger, created_at, updated_at)
			VALUES ($1, 'pending', 'manual', now() - $2::interval, now() - $2::interval)
			RETURNING id`, taskType, ageInterval).Scan(&id); err != nil {
			t.Fatalf("insert pending (%s): %v", ageInterval, err)
		}
		return id
	}
	staleID := insert("25 hours")
	boundaryID := insert("23 hours")
	freshID := insert("1 minute")

	store, err := NewSQLStore(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	affected, err := store.ExpireRunningRuns(ctx)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	// 库里可能还有其它套件留下的超龄 pending 行，因此只断言计数 ≥1，
	// 行级结论靠下面按 id 的查询。
	if affected < 1 {
		t.Fatalf("收敛计数 = %d，期望 ≥1", affected)
	}

	type row struct {
		status string
		error  sql.NullString
	}
	read := func(id int64) row {
		t.Helper()
		var r row
		if err := db.QueryRowContext(ctx,
			`SELECT status, error FROM worker_task_runs WHERE id = $1`, id).
			Scan(&r.status, &r.error); err != nil {
			t.Fatalf("read row %d: %v", id, err)
		}
		return r
	}

	if got := read(staleID); got.status != RunFailed || got.error.String != ErrClassPendingExpired {
		t.Fatalf("25h pending = %+v，期望 failed/%s", got, ErrClassPendingExpired)
	}
	// 24h 边界：未超龄的 pending 行必须原样保留（兜底不能吃掉正常队列）。
	if got := read(boundaryID); got.status != RunPending {
		t.Fatalf("23h pending = %+v，期望仍为 pending", got)
	}
	if got := read(freshID); got.status != RunPending || got.error.Valid {
		t.Fatalf("新鲜 pending = %+v，期望仍为 pending 且无错误", got)
	}
}
